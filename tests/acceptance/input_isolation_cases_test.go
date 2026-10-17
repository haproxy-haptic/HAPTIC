// Copyright 2026 Philipp Hossner
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
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/pkg/types"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/inputisolation"
)

func TestAnnotationInputIsolation(t *testing.T) {
	testEnv.Test(t, buildIngressInputIsolationFeature("annotation", "haproxy.org/auth-type", "invalid-auth-type"))
}

func TestSnippetInputIsolation(t *testing.T) {
	testEnv.Test(t, buildIngressInputIsolationFeature("snippet", "haproxy-haptic.org/config-backend", "invalid-haproxy-directive"))
}

func buildIngressInputIsolationFeature(name, annotation, invalid string) types.Feature {
	return features.New("Invalid Ingress "+name+" isolation").
		Setup(setupControllerEnv("test-isolate-"+name, func(o *ControllerEnvironmentOptions) { o.SkipCRDAndDeployment = true })).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			// Existing invalid inputs must be isolated when the controller and webhook first start.
			return setupIsolationInputs(ctx, t, cfg, func(ingress *networkingv1.Ingress) {
				ingress.Annotations[annotation] = invalid
			}, false)
		}).
		Assess("rejected Ingress permits independent changes and recovers after repair", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			namespace, client, clientset := readyControllerEnv(ctx, t, cfg)
			logIsolationFailure(ctx, t, client, clientset, namespace)
			debug, err := SetupDebugClient(ctx, client, clientset, namespace, 30*time.Second)
			require.NoError(t, err)
			ingress, err := clientset.NetworkingV1().Ingresses(namespace).Get(ctx, "protected", metav1.GetOptions{})
			require.NoError(t, err)
			waitRejectedIngress(ctx, t, debug, ingress, true)
			waitIsolationWarning(ctx, t, clientset, debug, ingress, "Ingress", time.Time{})
			advanceIsolationEndpoints(ctx, t, clientset, namespace, "192.0.2.20")
			checkIsolationAdmission(ctx, t, client, namespace, "unrelated")
			waitIsolationConfig(ctx, t, debug, "192.0.2.20:8080", "unrelated.example.test")
			rendered, err := debug.GetRenderedConfig(ctx)
			require.NoError(t, err)
			require.NotContains(t, rendered, invalid)
			require.NotContains(t, rendered, "backend route_protected")
			ingress.Annotations[annotation] = invalid + "-still-invalid"
			_, err = clientset.NetworkingV1().Ingresses(namespace).Update(ctx, ingress, metav1.UpdateOptions{})
			require.ErrorContains(t, err, "denied")
			delete(ingress.Annotations, annotation)
			_, err = clientset.NetworkingV1().Ingresses(namespace).Update(ctx, ingress, metav1.UpdateOptions{})
			require.NoError(t, err)
			waitRejectedIngress(ctx, t, debug, ingress, false)
			waitIsolationConfig(ctx, t, debug, "backend route_protected", "192.0.2.20:8080")
			return ctx
		}).Teardown(cleanupIsolationEnvironment).Feature()
}

func cleanupIsolationEnvironment(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	client, err := cfg.NewClient()
	require.NoError(t, err)
	namespace, err := GetNamespaceFromContext(ctx)
	require.NoError(t, err)
	require.NoError(t, Clientset().AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(ctx, namespace, metav1.DeleteOptions{}))
	return CleanupControllerEnvironment(ctx, t, client)
}

func advanceIsolationEndpoints(ctx context.Context, t *testing.T, clientset kubernetes.Interface, namespace, address string) {
	t.Helper()
	slice, err := clientset.DiscoveryV1().EndpointSlices(namespace).Get(ctx, "healthy", metav1.GetOptions{})
	require.NoError(t, err)
	slice.Endpoints[0].Addresses = []string{address}
	_, err = clientset.DiscoveryV1().EndpointSlices(namespace).Update(ctx, slice, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func restartIsolationController(ctx context.Context, t *testing.T, client klient.Client, clientset kubernetes.Interface, namespace string, debug *DebugClient) *corev1.Pod {
	t.Helper()
	before, err := GetControllerPod(ctx, client, namespace)
	require.NoError(t, err)
	require.NoError(t, clientset.CoreV1().Pods(namespace).Delete(ctx, before.Name, metav1.DeleteOptions{}))
	var replacement *corev1.Pod
	require.NoError(t, debug.waitFor(ctx, DefaultPodReadyTimeout, "replacement controller pod", func(ctx context.Context) (bool, error) {
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + ControllerDeploymentName})
		if err != nil {
			return false, err
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.UID == before.UID {
				return false, nil
			}
		}
		for i := range pods.Items {
			for _, condition := range pods.Items[i].Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					replacement = &pods.Items[i]
					return true, nil
				}
			}
		}
		return false, nil
	}))
	waitIsolationWebhook(ctx, t, clientset, namespace, debug)
	return replacement
}

func waitRejectedIngress(ctx context.Context, t *testing.T, debug *DebugClient, ingress *networkingv1.Ingress, rejected bool) {
	t.Helper()
	require.NoError(t, debug.waitFor(ctx, 30*time.Second, "Ingress rejection state", func(ctx context.Context) (bool, error) {
		body, err := debug.proxyGet(ctx, "/debug/vars/inputRejections")
		if err != nil {
			return false, err
		}
		var state struct {
			Ready     bool                       `json:"ready"`
			Resources []inputisolation.Rejection `json:"resources"`
		}
		if err := json.Unmarshal(body, &state); err != nil || !state.Ready {
			return false, err
		}
		for i := range state.Resources {
			resource := &state.Resources[i]
			if resource.Store == "ingresses" && resource.Namespace == ingress.Namespace && resource.Name == ingress.Name {
				return rejected, nil
			}
		}
		return !rejected, nil
	}))
}

func waitIsolationWarning(ctx context.Context, t *testing.T, clientset kubernetes.Interface, debug *DebugClient, object metav1.Object, kind string, since time.Time) {
	t.Helper()
	require.NoError(t, debug.waitFor(ctx, 30*time.Second, "Warning event on rejected resource", func(ctx context.Context) (bool, error) {
		list, err := clientset.CoreV1().Events(object.GetNamespace()).List(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("involvedObject.uid", string(object.GetUID())).String()})
		if err != nil {
			return false, err
		}
		for i := range list.Items {
			event := &list.Items[i]
			if event.LastTimestamp.Time.Before(since) && event.EventTime.Time.Before(since) {
				continue
			}
			if event.Type == corev1.EventTypeWarning && event.Reason == "InputRejected" && event.InvolvedObject.ResourceVersion == object.GetResourceVersion() {
				require.Equal(t, kind, event.InvolvedObject.Kind)
				require.Contains(t, event.Message, "Correct this resource")
				return true, nil
			}
		}
		return false, nil
	}))
}

func TestFetchedListInputIsolation(t *testing.T) {
	testEnv.Test(t, buildFetchedListInputIsolationFeature())
}

func buildFetchedListInputIsolationFeature() types.Feature {
	return features.New("Invalid fetched list isolation").
		Setup(setupControllerEnv("test-isolate-http", func(o *ControllerEnvironmentOptions) { o.SkipCRDAndDeployment = true })).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			return setupIsolationInputs(ctx, t, cfg, nil, true)
		}).
		Assess("invalid list retains accepted content while independent updates advance", assessFetchedListIsolation).
		Teardown(cleanupIsolationEnvironment).Feature()
}

func assessFetchedListIsolation(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	namespace, client, clientset := readyControllerEnv(ctx, t, cfg)
	logIsolationFailure(ctx, t, client, clientset, namespace)
	debug, err := SetupDebugClient(ctx, client, clientset, namespace, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, debug.WaitForAuxFileContains(ctx, "blocked-ips.acl", "192.168.1.0/24", 30*time.Second))
	require.NoError(t, UpdateBlocklistAndRestart(ctx, t, client, clientset, namespace, InvalidBlocklistContent))
	waitFetchedListRejection(ctx, t, client, clientset, namespace, debug)
	advanceIsolationEndpoints(ctx, t, clientset, namespace, "192.0.2.20")
	checkIsolationAdmission(ctx, t, client, namespace, "unrelated")
	waitIsolationConfig(ctx, t, debug, "192.0.2.20:8080", "unrelated.example.test")
	content, err := debug.GetGeneralFileContent(ctx, "blocked-ips.acl")
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(ValidBlocklistContent), strings.TrimSpace(content))
	require.NoError(t, UpdateBlocklistAndRestart(ctx, t, client, clientset, namespace, UpdatedBlocklistContent))
	require.NoError(t, debug.WaitForAuxFileContains(ctx, "blocked-ips.acl", "172.16.0.0/12", 30*time.Second))
	advanceIsolationEndpoints(ctx, t, clientset, namespace, "192.0.2.30")
	waitIsolationConfig(ctx, t, debug, "192.0.2.30:8080", "unrelated.example.test")
	return ctx
}

func logIsolationFailure(ctx context.Context, t *testing.T, client klient.Client, clientset kubernetes.Interface, namespace string) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			pod, err := GetControllerPod(ctx, client, namespace)
			if err == nil {
				DumpPodLogs(ctx, t, clientset, pod)
			}
		}
	})
}

func waitIsolationWebhook(ctx context.Context, t *testing.T, clientset kubernetes.Interface, namespace string, debug *DebugClient) {
	t.Helper()
	probe := NewValidIngress(namespace, "webhook-readiness")
	probe.Labels = map[string]string{"input-isolation-test": namespace}
	probe.Annotations["haproxy.org/auth-type"] = "invalid-auth-type"
	require.NoError(t, debug.waitFor(ctx, 30*time.Second, "webhook rejects an invalid dry-run probe", func(ctx context.Context) (bool, error) {
		_, err := clientset.NetworkingV1().Ingresses(namespace).Create(ctx, probe, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if err != nil && strings.Contains(err.Error(), "denied") {
			return true, nil
		}
		return false, err
	}))
}

func waitRestartedHashRejection(ctx context.Context, t *testing.T, clientset kubernetes.Interface, debug *DebugClient, namespace string, since time.Time) {
	t.Helper()
	var rejected corev1.ObjectReference
	require.NoError(t, debug.waitFor(ctx, 30*time.Second, "cold-start rejection for the malformed hash", func(ctx context.Context) (bool, error) {
		body, err := debug.proxyGet(ctx, "/debug/vars/inputRejections")
		if err != nil {
			return false, err
		}
		var state struct {
			Ready     bool                       `json:"ready"`
			Resources []inputisolation.Rejection `json:"resources"`
		}
		if err := json.Unmarshal(body, &state); err != nil || !state.Ready {
			return false, err
		}
		for i := range state.Resources {
			resource := &state.Resources[i]
			// On a cold start, the on-demand Secret may first be read while admitting its Ingress.
			affected := (resource.Store == "secrets" && resource.Name == "basic-auth") || (resource.Store == "ingresses" && resource.Name == "protected")
			if affected && resource.Namespace == namespace && strings.Contains(resource.Reason, "password hash") {
				rejected = resource.Object
				return true, nil
			}
		}
		return false, nil
	}))
	require.NotEmpty(t, rejected.UID)
	require.NotEmpty(t, rejected.ResourceVersion)
	object := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name: rejected.Name, Namespace: rejected.Namespace, UID: rejected.UID, ResourceVersion: rejected.ResourceVersion,
	}}
	waitIsolationWarning(ctx, t, clientset, debug, object, rejected.Kind, since)
}

func waitFetchedListRejection(ctx context.Context, t *testing.T, client klient.Client, clientset kubernetes.Interface, namespace string, debug *DebugClient) {
	t.Helper()
	config := &v1alpha1.HAProxyTemplateConfig{}
	require.NoError(t, client.Resources().Get(ctx, ControllerCRDName, namespace, config))
	require.NotEmpty(t, config.UID)
	require.NoError(t, debug.waitFor(ctx, 30*time.Second, "HTTPContentRejected Warning on the config", func(ctx context.Context) (bool, error) {
		list, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("involvedObject.uid", string(config.UID)).String(),
		})
		if err != nil {
			return false, err
		}
		for i := range list.Items {
			event := &list.Items[i]
			if event.Type != corev1.EventTypeWarning || event.Reason != "HTTPContentRejected" {
				continue
			}
			require.Equal(t, "HAProxyTemplateConfig", event.InvolvedObject.Kind)
			require.Equal(t, config.Name, event.InvolvedObject.Name)
			require.NotEmpty(t, event.InvolvedObject.ResourceVersion)
			require.Contains(t, event.Message, "previously accepted content remains active")
			require.Contains(t, event.Message, "Fix the fetched content or its template")
			return true, nil
		}
		return false, nil
	}))
	metrics, err := SetupMetricsAccess(ctx, client, clientset, namespace, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, debug.waitFor(ctx, 30*time.Second, "HTTP content rejection counter", func(ctx context.Context) (bool, error) {
		values, err := metrics.GetMetricValues(ctx, []string{"haptic_http_content_rejected_total", "haptic_http_content_revoked_total"})
		if err != nil {
			return false, err
		}
		require.Zero(t, values["haptic_http_content_revoked_total"], "invalid content must be rejected before acceptance")
		return values["haptic_http_content_rejected_total"] > 0, nil
	}))
}
