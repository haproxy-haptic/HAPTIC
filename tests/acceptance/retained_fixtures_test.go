// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	v1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	hapticclient "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/podclient"
)

const retainedReadyBound = 2 * time.Minute

const retainedFleetTemplate = `global
  default-path origin {{ pathResolver.GetBaseDir() }}
  stats socket /etc/haproxy/haproxy-worker.sock mode 600 level admin
  nbthread 2
  hard-stop-after 10s

defaults
  mode http
  timeout connect 5s
  timeout client 10s
  timeout server 10s

frontend status
  bind :8404
  http-request use-service prometheus-exporter if { path /metrics }
  http-request return status 200 content-type text/plain string "OK" if { path /healthz } || { path /ready }

frontend http
  bind :8080
  acl blocked src -f {{ pathResolver.GetPath("blocked-ips.acl", "file") }}
  http-request deny if blocked
  http-request return status 200 content-type text/plain string "host-x" if { hdr(host) -i retained.example }
  http-request return status 404
`

type retainedFleet struct {
	abruptFailover       bool
	namespace            string
	client               klient.Client
	haptic               hapticclient.Interface
	config               *v1.HAProxyTemplateConfig
	deployment           *appsv1.Deployment
	selector             string
	publishedFiles       map[string]*v1.HAProxyGeneralFile
	publishedCheckpoints map[string]*v1.HAProxyCfg
}

func setupRetainedFleet(ctx context.Context, t *testing.T, cfg *envconf.Config, watchedObjects int) (context.Context, *retainedFleet) {
	t.Helper()
	namespace := envconf.RandomName("retained", 32)
	ctx = StoreNamespaceInContext(ctx, namespace)
	client, err := cfg.NewClient()
	require.NoError(t, err)
	opts := DefaultControllerEnvironmentOptions()
	opts.SkipCRDAndDeployment = true
	require.NoError(t, CreateControllerEnvironment(ctx, t, client, namespace, opts))
	require.NoError(t, SetupBlocklistServer(ctx, t, client, namespace, ValidBlocklistContent))
	deployment, bootstrap := retainedChartFleet(ctx, t, namespace)
	config := NewHTTPStoreHAProxyTemplateConfig(namespace, ControllerCRDName, ControllerSecretName, true)
	config.Spec.HAProxyConfig.Template = retainedFleetTemplate
	config.Spec.PodSelector.MatchLabels = deployment.Spec.Selector.DeepCopy().MatchLabels
	config.Spec.Dataplane.MapsDir = "/etc/haproxy/maps"
	config.Spec.Dataplane.SSLCertsDir = "/etc/haproxy/certs"
	config.Spec.Dataplane.GeneralStorageDir = "/etc/haproxy/general"
	config.Spec.Dataplane.ConfigFile = "/etc/haproxy/haproxy.cfg"
	config.Spec.Controller.ConfigPublishing.CompressionThreshold = 1 << 20
	if watchedObjects > 0 {
		addRetainedWatchedInputs(ctx, t, config, watchedObjects)
	}
	require.NoError(t, client.Resources().Create(ctx, config))
	require.NoError(t, client.Resources().Create(ctx, bootstrap))
	require.NoError(t, client.Resources().Create(ctx, deployment))
	require.NoError(t, client.Resources().Create(ctx, NewControllerDeployment(namespace, ControllerCRDName, ControllerSecretName, ControllerServiceAccountName, DebugPort, 2)))
	haptic, err := hapticclient.NewForConfig(RESTConfig())
	require.NoError(t, err)
	f := &retainedFleet{namespace: namespace, client: client, haptic: haptic, config: config, deployment: deployment, selector: labels.SelectorFromSet(deployment.Spec.Selector.MatchLabels).String()}
	defer f.dumpOnFailure(ctx, t)
	f.waitReady(ctx, t, 2)
	if watchedObjects > 0 {
		for _, pod := range f.pods(ctx, t) {
			content, err := f.exec(ctx, pod.Name, "haproxy", "cat", "/etc/haproxy/haproxy.cfg")
			require.NoError(t, err)
			require.Contains(t, content, fmt.Sprintf("# watched inputs: %d", watchedObjects))
		}
	}
	f.waitCheckpoint(ctx, t)
	return ctx, f
}

func addRetainedWatchedInputs(ctx context.Context, t *testing.T, config *v1.HAProxyTemplateConfig, count int) {
	t.Helper()
	roleName := NewClusterRole(ControllerClusterRoleName, config.Namespace).Name
	role, err := Clientset().RbacV1().ClusterRoles().Get(ctx, roleName, metav1.GetOptions{})
	require.NoError(t, err)
	role.Rules = append(role.Rules, rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "watch"},
	})
	_, err = Clientset().RbacV1().ClusterRoles().Update(ctx, role, metav1.UpdateOptions{})
	require.NoError(t, err)
	config.Spec.WatchedResources["scaleinputs"] = v1.WatchedResource{
		APIVersion: "v1", Resources: "configmaps",
		IndexBy:       []string{"metadata.namespace", "metadata.name"},
		LabelSelector: "retained-fixture=" + config.Namespace,
	}
	config.Spec.HAProxyConfig.Template += "\n# watched inputs: {{ len(resources.scaleinputs.List()) }}\n"
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(16)
	for i := range count {
		group.Go(func() error {
			_, err := Clientset().CoreV1().ConfigMaps(config.Namespace).Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:   fmt.Sprintf("watched-%05d", i),
					Labels: map[string]string{"retained-fixture": config.Namespace},
				},
				Data: map[string]string{"value": "healthy"},
			}, metav1.CreateOptions{})
			return err
		})
	}
	require.NoError(t, group.Wait())
}

func retainedChartFleet(ctx context.Context, t *testing.T, namespace string) (*appsv1.Deployment, *corev1.ConfigMap) {
	t.Helper()
	output, err := exec.CommandContext(ctx, "helm", "template", "retained", "../../charts/haptic", "--namespace", namespace,
		"--set", "haproxy.agent.tls.enabled=false", "--set", "spoaHub.enabled=false", "--set", "vector.enabled=false",
		"--set", "haproxy.ports.http=8080", "--set", "haproxy.ports.https=8443",
		"--set", "haproxy.replicaCount=2", "--set", "credentials.existingSecret="+ControllerSecretName,
		"--show-only", "templates/haproxy-deployment.yaml", "--show-only", "templates/haproxy-configmap.yaml").CombinedOutput()
	require.NoError(t, err, "%s", output)
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var deployment *appsv1.Deployment
	var bootstrap *corev1.ConfigMap
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		var meta metav1.TypeMeta
		require.NoError(t, json.Unmarshal(raw, &meta))
		switch meta.Kind {
		case "Deployment":
			deployment = &appsv1.Deployment{}
			require.NoError(t, json.Unmarshal(raw, deployment))
		case "ConfigMap":
			bootstrap = &corev1.ConfigMap{}
			require.NoError(t, json.Unmarshal(raw, bootstrap))
		}
	}
	require.NotNil(t, deployment)
	require.NotNil(t, bootstrap)
	for _, containers := range [][]corev1.Container{deployment.Spec.Template.Spec.Containers, deployment.Spec.Template.Spec.InitContainers} {
		for i := range containers {
			containers[i].Image = ControllerImageName
			containers[i].ImagePullPolicy = corev1.PullNever
		}
	}
	return deployment, bootstrap
}

func (f *retainedFleet) scale(ctx context.Context, t *testing.T, name string, replicas int32) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := Clientset().AppsV1().Deployments(f.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		deployment.Spec.Replicas = &replicas
		_, err = Clientset().AppsV1().Deployments(f.namespace).Update(ctx, deployment, metav1.UpdateOptions{})
		return err
	}))
}

func (f *retainedFleet) pods(ctx context.Context, t *testing.T) []*corev1.Pod {
	t.Helper()
	pods, err := Clientset().CoreV1().Pods(f.namespace).List(ctx, metav1.ListOptions{LabelSelector: f.selector})
	require.NoError(t, err)
	result := make([]*corev1.Pod, len(pods.Items))
	for i := range pods.Items {
		result[i] = &pods.Items[i]
	}
	return result
}

func (f *retainedFleet) waitReady(ctx context.Context, t *testing.T, count int) {
	t.Helper()
	var ready []*corev1.Pod
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, retainedReadyBound, true, func(ctx context.Context) (bool, error) {
		ready = nil
		for _, pod := range f.pods(ctx, t) {
			if pod.DeletionTimestamp != nil {
				continue
			}
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					ready = append(ready, pod)
				}
			}
		}
		return len(ready) == count, nil
	}), "HAProxy replacements must become Ready within %s", retainedReadyBound)
	for _, pod := range ready {
		body, err := f.exec(ctx, pod.Name, "haproxy", "curl", "-fsS", "--max-time", "5", "-H", "Host: retained.example", "http://127.0.0.1:8080/")
		require.NoError(t, err)
		require.Equal(t, "host-x", body)
	}
}

// servesWatchedInputs reports whether pod's haproxy.cfg was rendered from count
// watched inputs. A failed read counts as not yet: the pod may be restarting.
func (f *retainedFleet) servesWatchedInputs(ctx context.Context, pod string, count int) bool {
	content, err := f.exec(ctx, pod, "haproxy", "cat", "/etc/haproxy/haproxy.cfg")
	return err == nil && strings.Contains(content, fmt.Sprintf("# watched inputs: %d", count))
}

func (f *retainedFleet) exec(ctx context.Context, pod, container string, command ...string) (string, error) {
	client := podclient.New(RESTConfig(), Clientset(), f.namespace, "", 0)
	body, err := client.Exec(ctx, pod, container, command, 1<<20)
	return string(body), err
}

func (f *retainedFleet) state(ctx context.Context, t *testing.T, pod string) api.State {
	t.Helper()
	body, err := f.exec(ctx, pod, "agent", "curl", "-fsS", "--max-time", "5", "-u", "admin:password", "http://127.0.0.1:5555/v1/state?verify=1&plan=0")
	require.NoError(t, err)
	var state api.State
	require.NoError(t, json.Unmarshal([]byte(body), &state))
	return state
}

func (f *retainedFleet) metrics(ctx context.Context, pod, port string) (string, error) {
	body, err := Clientset().CoreV1().Pods(f.namespace).ProxyGet("http", pod, port, "metrics", nil).DoRaw(ctx)
	return string(body), err
}

func (f *retainedFleet) waitCheckpoint(ctx context.Context, t *testing.T) *v1.HAProxyCfg {
	t.Helper()
	var current *v1.HAProxyCfg
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, retainedReadyBound, true, func(ctx context.Context) (bool, error) {
		var err error
		current, err = f.haptic.HaproxyTemplateICV1alpha1().HAProxyCfgs(f.namespace).Get(ctx, ControllerCRDName+"-haproxycfg", metav1.GetOptions{})
		return err == nil && len(current.Status.RetainedConfigs) > 0, nil
	}))
	f.capturePublication(ctx, t, current)
	return current
}

func (f *retainedFleet) leader(ctx context.Context, t *testing.T) string {
	t.Helper()
	identity, err := WaitForNewLeader(ctx, f.client, Clientset(), f.namespace, "haptic-leader", "", retainedReadyBound)
	require.NoError(t, err)
	pods, err := GetAllControllerPods(ctx, f.client, f.namespace)
	require.NoError(t, err)
	for i := range pods {
		pod := &pods[i]
		if strings.HasPrefix(identity, pod.Name) {
			return pod.Name
		}
	}
	t.Fatalf("leader %q has no live pod", identity)
	return ""
}

func (f *retainedFleet) waitRenderFailure(ctx context.Context, t *testing.T, pod string) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, retainedReadyBound, true, func(ctx context.Context) (bool, error) {
		body, err := f.metrics(ctx, pod, "9090")
		failures, present := retainedMetric(body, "haptic_reconciliation_errors_total")
		if !present || failures == 0 {
			return false, err
		}
		if err != nil {
			return false, err
		}
		logs, err := Clientset().CoreV1().Pods(f.namespace).GetLogs(pod, &corev1.PodLogOptions{Container: "controller", TailLines: ptr.To[int64](200)}).DoRaw(ctx)
		return err == nil && strings.Contains(string(logs), "failed to fetch blocklist"), err
	}), fmt.Sprintf("%s must report the blocked render", pod))
	f.waitHealthRenderError(ctx, t, pod, true)
}

func (f *retainedFleet) capturePublication(ctx context.Context, t *testing.T, current *v1.HAProxyCfg) {
	t.Helper()
	client := f.haptic.HaproxyTemplateICV1alpha1()
	f.publishedFiles = map[string]*v1.HAProxyGeneralFile{}
	f.publishedCheckpoints = map[string]*v1.HAProxyCfg{}
	outputs := make([]*v1.HAProxyCfg, 1, 1+len(current.Status.RetainedConfigs))
	outputs[0] = current
	for _, ref := range current.Status.RetainedConfigs {
		checkpoint, err := client.HAProxyCfgs(f.namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		require.NoError(t, err)
		f.publishedCheckpoints[checkpoint.Name] = checkpoint
		outputs = append(outputs, checkpoint)
	}
	for _, output := range outputs {
		require.NotNil(t, output.Status.AuxiliaryFiles)
		for _, ref := range output.Status.AuxiliaryFiles.GeneralFiles {
			file, err := client.HAProxyGeneralFiles(f.namespace).Get(ctx, ref.Name, metav1.GetOptions{})
			require.NoError(t, err)
			f.publishedFiles[file.Name] = file
		}
	}
}

func (f *retainedFleet) waitHealthRenderError(ctx context.Context, t *testing.T, pod string, failed bool) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, retainedReadyBound, true, func(ctx context.Context) (bool, error) {
		body, err := f.exec(ctx, pod, "controller", "curl", "-fsS", "--max-time", "5", "http://127.0.0.1:6060/healthz")
		if err != nil {
			return false, err
		}
		var response struct {
			Components map[string]struct {
				Healthy bool
				Error   string
			}
		}
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			return false, err
		}
		component, present := response.Components["reconciliation-coordinator"]
		return present && component.Healthy && strings.Contains(component.Error, "Last render failed:") == failed, nil
	}), "health response must preserve component availability and expose the last render result")
}

// dumpOnFailure logs what decides HAProxy readiness, including after a failed setup.
func (f *retainedFleet) dumpOnFailure(ctx context.Context, t *testing.T) {
	t.Helper()
	if !t.Failed() {
		return
	}
	ctx = context.WithoutCancel(ctx)
	controllers, err := GetAllControllerPods(ctx, f.client, f.namespace)
	if err != nil {
		t.Logf("list controller pods: %v", err)
	}
	for i := range controllers {
		f.dumpLogs(ctx, t, &controllers[i], "controller", 200)
	}
	pods, err := Clientset().CoreV1().Pods(f.namespace).List(ctx, metav1.ListOptions{LabelSelector: f.selector})
	if err != nil {
		t.Logf("list HAProxy pods: %v", err)
		pods = &corev1.PodList{}
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		t.Logf("=== HAProxy pod %s (uid %s, deleting %t) conditions: %+v", pod.Name, pod.UID, pod.DeletionTimestamp != nil, pod.Status.Conditions)
		statuses := slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses)
		for j := range statuses {
			status := &statuses[j]
			t.Logf("    container %s ready=%t restarts=%d state=%+v", status.Name, status.Ready, status.RestartCount, status.State)
		}
		state, err := f.exec(ctx, pod.Name, "agent", "curl", "-sS", "--max-time", "5", "-u", "admin:password", "http://127.0.0.1:5555/v1/state?verify=1&plan=0")
		t.Logf("    agent state (err %v): %s", err, state)
		f.dumpLogs(ctx, t, pod, "agent", 50)
	}
	for _, name := range []string{"haptic-leader", ControllerCRDName + "-retained-state"} {
		lease, err := Clientset().CoordinationV1().Leases(f.namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			t.Logf("=== Lease %s holder %v, annotations %v", name, ptr.Deref(lease.Spec.HolderIdentity, ""), lease.Annotations)
		}
	}
	events, err := Clientset().CoreV1().Events(f.namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
	if err == nil {
		for i := range events.Items {
			event := &events.Items[i]
			t.Logf("=== Event %s %s/%s x%d: %s", event.Reason, event.InvolvedObject.Kind, event.InvolvedObject.Name, event.Count, event.Message)
		}
	}
}

// dumpLogs logs every line above DEBUG and the last tail lines.
func (f *retainedFleet) dumpLogs(ctx context.Context, t *testing.T, pod *corev1.Pod, container string, tail int) {
	t.Helper()
	f.dumpInstanceLogs(ctx, t, pod, container, tail, false)
	if containerRestarts(pod, container) > 0 {
		f.dumpInstanceLogs(ctx, t, pod, container, tail, true)
	}
}

func (f *retainedFleet) dumpInstanceLogs(ctx context.Context, t *testing.T, pod *corev1.Pod, container string, tail int, previous bool) {
	t.Helper()
	logs, err := Clientset().CoreV1().Pods(f.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container, Previous: previous}).DoRaw(ctx)
	lines := strings.Split(strings.TrimRight(string(logs), "\n"), "\n")
	recent := max(len(lines)-tail, 0)
	var kept []string
	for i, line := range lines {
		if i >= recent || !strings.Contains(line, "level=DEBUG") {
			kept = append(kept, line)
		}
	}
	instance := "current"
	if previous {
		instance = "previous"
	}
	t.Logf("=== %s %s logs of %s (err %v) ===\n%s", instance, container, pod.Name, err, strings.Join(kept, "\n"))
}
