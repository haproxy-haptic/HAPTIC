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
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/pkg/types"

	v1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
)

func TestRetainedConfigurationRecovery(t *testing.T) {
	testEnv.Test(t, buildRetainedConfigurationFeature("Recovery", 0, (*retainedFleet).assessRecovery))
}

func TestRetainedConfigurationRecoveryAtScale(t *testing.T) {
	testEnv.Test(t, buildRetainedConfigurationFeature("Recovery with 3200 watched inputs", 3200, (*retainedFleet).assessRecovery))
}

func TestRetainedConfigurationSuddenFailover(t *testing.T) {
	testEnv.Test(t, buildRetainedConfigurationFeature("Sudden leader loss", 0, (*retainedFleet).assessSuddenRecovery))
}

func (f *retainedFleet) assessSuddenRecovery(ctx context.Context, t *testing.T) {
	t.Helper()
	f.abruptFailover = true
	f.assessRecovery(ctx, t)
}

func TestRetainedConfigurationAfterRecentDeployment(t *testing.T) {
	testEnv.Test(t, buildRetainedConfigurationFeature("Leader change right after a deployment", 1, (*retainedFleet).assessRecentDeployment))
}

// assessRecentDeployment deletes the leader right after a deployment whose
// receipt the publish throttle still holds (issue #358).
func (f *retainedFleet) assessRecentDeployment(ctx context.Context, t *testing.T) {
	t.Helper()
	f.blockSource(ctx, t)
	leader := f.refreshStandby(ctx, t)
	f.waitCheckpoint(ctx, t)
	f.deployWatchedInputs(ctx, t, 2)
	plan := f.deployWatchedInputs(ctx, t, 3)
	require.NoError(t, Clientset().CoreV1().Pods(f.namespace).Delete(ctx, leader, metav1.DeleteOptions{}))
	require.NoError(t, WaitForPodTerminated(ctx, f.client, f.namespace, leader, retainedReadyBound))
	// Captured after the stop: a graceful stop may still observe a paced reload.
	old := f.captureFleet(ctx, t)
	leader = f.leader(ctx, t)
	f.waitRenderFailure(ctx, t, leader)
	f.scale(ctx, t, f.deployment.Name, 3)
	f.waitReady(ctx, t, 3)
	f.assertUnchanged(ctx, t, old)
	for _, pod := range f.pods(ctx, t) {
		require.Equal(t, plan, f.state(ctx, t, pod.Name).AppliedPlanID, "pod %s must run the last deployment", pod.Name)
	}
	f.waitGauge(ctx, t, leader, 1)
	f.repair(ctx, t, leader)
}

// deployWatchedInputs grows the watched set to count and returns the plan
// once every HAProxy pod applied it.
func (f *retainedFleet) deployWatchedInputs(ctx context.Context, t *testing.T, count int) string {
	t.Helper()
	_, err := Clientset().CoreV1().ConfigMaps(f.namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   fmt.Sprintf("watched-%05d", count-1),
			Labels: map[string]string{"retained-fixture": f.namespace},
		},
		Data: map[string]string{"value": "healthy"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	var plan string
	require.NoError(t, wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, retainedReadyBound, true, func(ctx context.Context) (bool, error) {
		plan = ""
		for _, pod := range f.pods(ctx, t) {
			if !f.servesWatchedInputs(ctx, pod.Name, count) {
				return false, nil
			}
			applied := f.state(ctx, t, pod.Name).AppliedPlanID
			if plan != "" && applied != plan {
				return false, nil
			}
			plan = applied
		}
		return plan != "", nil
	}))
	return plan
}

func TestRetainedConfigurationTampering(t *testing.T) {
	testEnv.Test(t, buildRetainedConfigurationFeature("Tampering", 0, (*retainedFleet).assessTampering))
}

func TestRetainedConfigurationFreshness(t *testing.T) {
	testEnv.Test(t, buildRetainedConfigurationFeature("Freshness", 0, (*retainedFleet).assessFreshness))
}

func buildRetainedConfigurationFeature(name string, watchedObjects int, assess func(*retainedFleet, context.Context, *testing.T)) types.Feature {
	var fleet *retainedFleet
	return features.New("Retained configuration: "+name).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			ctx, fleet = setupRetainedFleet(ctx, t, cfg, watchedObjects)
			return ctx
		}).
		Assess("a blocked fresh leader safely handles replacement pods", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			t.Helper()
			defer fleet.dumpOnFailure(ctx, t)
			assess(fleet, ctx, t)
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Logf("cleanup client: %v", err)
				return ctx
			}
			return CleanupControllerEnvironment(ctx, t, client)
		}).Feature()
}

func (f *retainedFleet) assessRecovery(ctx context.Context, t *testing.T) {
	t.Helper()
	f.blockSource(ctx, t)
	leader := f.refreshStandby(ctx, t)
	old := f.captureFleet(ctx, t)
	publication := f.waitCheckpoint(ctx, t)
	options := metav1.DeleteOptions{}
	if f.abruptFailover {
		options.GracePeriodSeconds = ptr.To[int64](0)
	}
	require.NoError(t, Clientset().CoreV1().Pods(f.namespace).Delete(ctx, leader, options))
	require.NoError(t, WaitForPodTerminated(ctx, f.client, f.namespace, leader, retainedReadyBound))
	leader = f.leader(ctx, t)
	f.waitRenderFailure(ctx, t, leader)
	f.scale(ctx, t, f.deployment.Name, 3)
	f.waitReady(ctx, t, 3)
	f.assertUnchanged(ctx, t, old)
	f.waitWarning(ctx, t, "RetainedConfigActive", publication.Status.RetainedConfigs[0].Checksum)
	f.waitGauge(ctx, t, leader, 1)
	f.assertPublicationUnchanged(ctx, t, publication)
	f.waitRenderFailure(ctx, t, leader)
	if f.abruptFailover {
		f.repair(ctx, t, leader)
		return
	}

	f.stopControllers(ctx, t)
	f.scale(ctx, t, ControllerDeploymentName, 2)
	leader = f.leader(ctx, t)
	f.waitRenderFailure(ctx, t, leader)
	f.waitReady(ctx, t, 3)
	old = f.captureFleet(ctx, t)
	checkpoint := f.publishedCheckpoints[publication.Status.RetainedConfigs[0].Name]
	require.NotEmpty(t, checkpoint.Status.DeployedToPods)
	witness := checkpoint.Status.DeployedToPods[0].PodName
	require.Contains(t, old, witness)
	delete(old, witness)
	require.NoError(t, Clientset().CoreV1().Pods(f.namespace).Delete(ctx, witness, metav1.DeleteOptions{}))
	require.NoError(t, WaitForPodTerminated(ctx, f.client, f.namespace, witness, retainedReadyBound))
	f.waitReady(ctx, t, 3)
	f.assertUnchanged(ctx, t, old)
	f.waitGauge(ctx, t, leader, 1)
	f.assertPublicationUnchanged(ctx, t, publication)
	f.repair(ctx, t, leader)
}

func (f *retainedFleet) refreshStandby(ctx context.Context, t *testing.T) string {
	t.Helper()
	leader := f.leader(ctx, t)
	controllers, err := GetAllControllerPods(ctx, f.client, f.namespace)
	require.NoError(t, err)
	require.Len(t, controllers, 2)
	for i := range controllers {
		pod := &controllers[i]
		if pod.Name == leader {
			continue
		}
		// A fresh standby has no accepted HTTP cache when the source is unreachable.
		require.NoError(t, Clientset().CoreV1().Pods(f.namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
		require.NoError(t, WaitForPodTerminated(ctx, f.client, f.namespace, pod.Name, retainedReadyBound))
		require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, retainedReadyBound, true, func(ctx context.Context) (bool, error) {
			current, err := GetAllControllerPods(ctx, f.client, f.namespace)
			if err != nil {
				return false, err
			}
			return replacementPodReady(current, leader, pod.Name), nil
		}))
	}
	return leader
}

func replacementPodReady(pods []corev1.Pod, excluded ...string) bool {
	for i := range pods {
		pod := &pods[i]
		if slices.Contains(excluded, pod.Name) || pod.DeletionTimestamp != nil {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

func (f *retainedFleet) blockSource(ctx context.Context, t *testing.T) {
	t.Helper()
	f.scale(ctx, t, BlocklistServerName, 0)
	require.NoError(t, WaitForPodCount(ctx, f.client, f.namespace, "app="+BlocklistServerName, 0, retainedReadyBound))
}

func (f *retainedFleet) assessFreshness(ctx context.Context, t *testing.T) {
	t.Helper()
	older := f.waitCheckpoint(ctx, t).Status.RetainedConfigs[0]
	f.scale(ctx, t, BlocklistServerName, 0)
	require.NoError(t, WaitForPodCount(ctx, f.client, f.namespace, "app="+BlocklistServerName, 0, retainedReadyBound))
	f.scale(ctx, t, f.deployment.Name, 3)
	f.waitReady(ctx, t, 3)
	f.repair(ctx, t, f.leader(ctx, t))
	var publication *v1.HAProxyCfg
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, retainedReadyBound, true, func(ctx context.Context) (bool, error) {
		var err error
		publication, err = f.haptic.HaproxyTemplateICV1alpha1().HAProxyCfgs(f.namespace).Get(ctx, ControllerCRDName+"-haproxycfg", metav1.GetOptions{})
		return err == nil && len(publication.Status.RetainedConfigs) > 1 && publication.Status.RetainedConfigs[0].Checksum != older.Checksum, nil
	}))
	newest := publication.Status.RetainedConfigs[0]
	f.blockSource(ctx, t)
	f.stopControllers(ctx, t)
	// Reproduce a crash after apply but before the newer checkpoint pointer was committed.
	client := f.haptic.HaproxyTemplateICV1alpha1().HAProxyCfgs(f.namespace)
	publication, err := client.Get(ctx, publication.Name, metav1.GetOptions{})
	require.NoError(t, err)
	publication.Status.RetainedConfigs = []v1.RetainedConfigReference{older}
	_, err = client.UpdateStatus(ctx, publication, metav1.UpdateOptions{})
	require.NoError(t, err)
	before := f.captureFleet(ctx, t)
	f.scale(ctx, t, ControllerDeploymentName, 2)
	leader := f.leader(ctx, t)
	f.waitRenderFailure(ctx, t, leader)
	f.scale(ctx, t, f.deployment.Name, 4)
	require.NoError(t, WaitForPodCount(ctx, f.client, f.namespace, f.selector, 4, retainedReadyBound))
	f.waitWarning(ctx, t, "RetainedConfigUnavailable", "newer deployment intent")
	f.assertBootstrapOnly(ctx, t, before)
	f.assertUnchanged(ctx, t, before)
	f.waitGauge(ctx, t, leader, 0)

	// A complete newer checkpoint restores capacity without touching the old pods.
	publication, err = client.Get(ctx, publication.Name, metav1.GetOptions{})
	require.NoError(t, err)
	publication.Status.RetainedConfigs = []v1.RetainedConfigReference{older, newest}
	_, err = client.UpdateStatus(ctx, publication, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.waitReady(ctx, t, 4)
	f.waitWarning(ctx, t, "RetainedConfigActive", newest.Checksum)
	f.waitGauge(ctx, t, leader, 1)
	f.assertUnchanged(ctx, t, before)
	expected := before[nextObservedPod(before)].plan
	require.NotEmpty(t, expected)
	for _, pod := range f.pods(ctx, t) {
		state := f.state(ctx, t, pod.Name)
		require.Equal(t, expected, state.AppliedPlanID, "pod %s state: %+v", pod.Name, state)
	}
}

func nextObservedPod(before map[string]retainedObservation) string {
	for name := range before {
		return name
	}
	return ""
}

func (f *retainedFleet) assertBootstrapOnly(ctx context.Context, t *testing.T, old map[string]retainedObservation) {
	t.Helper()
	fresh := 0
	for _, pod := range f.pods(ctx, t) {
		if _, exists := old[pod.Name]; exists {
			continue
		}
		fresh++
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady {
				require.Equal(t, corev1.ConditionFalse, condition.Status)
			}
		}
		state := f.state(ctx, t, pod.Name)
		require.Empty(t, state.AppliedPlanID)
		require.Empty(t, state.RunningPlanID)
	}
	require.Equal(t, 1, fresh)
}

func (f *retainedFleet) stopControllers(ctx context.Context, t *testing.T) {
	t.Helper()
	f.scale(ctx, t, ControllerDeploymentName, 0)
	require.NoError(t, WaitForPodCount(ctx, f.client, f.namespace, "app="+ControllerDeploymentName, 0, retainedReadyBound))
}

func (f *retainedFleet) repair(ctx context.Context, t *testing.T, leader string) {
	t.Helper()
	cm, err := Clientset().CoreV1().ConfigMaps(f.namespace).Get(ctx, BlocklistContentConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	cm.Data["blocklist.txt"] = "192.0.2.123\n"
	_, err = Clientset().CoreV1().ConfigMaps(f.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.scale(ctx, t, BlocklistServerName, 1)
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, retainedReadyBound, true, func(ctx context.Context) (bool, error) {
		for _, pod := range f.pods(ctx, t) {
			body, err := f.exec(ctx, pod.Name, "haproxy", "cat", "/etc/haproxy/general/blocked-ips.acl")
			if ready := err == nil && body == "192.0.2.123\n"; !ready {
				return false, nil
			}
		}
		return true, nil
	}))
	f.waitReady(ctx, t, 3)
	f.waitGauge(ctx, t, leader, 0)
	f.waitHealthRenderError(ctx, t, leader, false)
	var plan string
	for _, pod := range f.pods(ctx, t) {
		state := f.state(ctx, t, pod.Name)
		require.NotEmpty(t, state.AppliedPlanID)
		if plan == "" {
			plan = state.AppliedPlanID
		}
		require.Equal(t, plan, state.AppliedPlanID)
		require.Equal(t, plan, state.WorkerOpsPlanID)
		require.Empty(t, state.ReloadPendingAt)
	}
}

func (f *retainedFleet) assessTampering(ctx context.Context, t *testing.T) {
	t.Helper()
	f.blockSource(ctx, t)
	f.stopControllers(ctx, t)
	publication := f.waitCheckpoint(ctx, t)
	reference := publication.Status.RetainedConfigs[0]
	checkpoint, err := f.haptic.HaproxyTemplateICV1alpha1().HAProxyCfgs(f.namespace).Get(ctx, reference.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.False(t, checkpoint.Spec.Compressed)
	checkpoint.Spec.Content += "\n# modified outside the publisher\n"
	_, err = f.haptic.HaproxyTemplateICV1alpha1().HAProxyCfgs(f.namespace).Update(ctx, checkpoint, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.capturePublication(ctx, t, publication)
	old := f.captureFleet(ctx, t)
	f.scale(ctx, t, ControllerDeploymentName, 2)
	leader := f.leader(ctx, t)
	f.waitRenderFailure(ctx, t, leader)
	f.scale(ctx, t, f.deployment.Name, 3)
	require.NoError(t, WaitForPodCount(ctx, f.client, f.namespace, f.selector, 3, retainedReadyBound))
	f.waitWarning(ctx, t, "RetainedConfigUnavailable", "checkpoint")
	var fresh *corev1.Pod
	for _, pod := range f.pods(ctx, t) {
		if _, exists := old[pod.Name]; !exists {
			fresh = pod
		}
	}
	require.NotNil(t, fresh)
	require.NotEmpty(t, fresh.Name)
	for _, condition := range fresh.Status.Conditions {
		if condition.Type == corev1.PodReady {
			require.Equal(t, corev1.ConditionFalse, condition.Status)
		}
	}
	state := f.state(ctx, t, fresh.Name)
	require.Empty(t, state.AppliedPlanID)
	require.Empty(t, state.RunningPlanID)
	f.assertUnchanged(ctx, t, old)
	f.waitGauge(ctx, t, leader, 0)
	f.assertPublicationUnchanged(ctx, t, publication)
}

type retainedObservation struct {
	applies, worker float64
	plan            string
}

func retainedMetric(body, name string) (float64, bool) {
	var sum float64
	found := false
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+" ") && !strings.HasPrefix(line, name+"{") {
			continue
		}
		fields := strings.Fields(line)
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			return 0, false
		}
		sum += value
		found = true
	}
	return sum, found
}

func (f *retainedFleet) observe(ctx context.Context, t *testing.T, pod string) retainedObservation {
	t.Helper()
	agent, err := f.metrics(ctx, pod, "5557")
	require.NoError(t, err)
	stats, err := f.metrics(ctx, pod, "8404")
	require.NoError(t, err)
	applies, present := retainedMetric(agent, "haptic_agent_apply_total")
	require.True(t, present, "agent apply counter is required")
	worker, present := retainedMetric(stats, "haproxy_process_start_time_seconds")
	require.True(t, present, "HAProxy worker start metric is required")
	return retainedObservation{applies: applies, worker: worker, plan: f.state(ctx, t, pod).AppliedPlanID}
}

func (f *retainedFleet) captureFleet(ctx context.Context, t *testing.T) map[string]retainedObservation {
	t.Helper()
	result := map[string]retainedObservation{}
	for _, pod := range f.pods(ctx, t) {
		result[pod.Name] = f.observe(ctx, t, pod.Name)
	}
	return result
}

func (f *retainedFleet) assertUnchanged(ctx context.Context, t *testing.T, before map[string]retainedObservation) {
	t.Helper()
	for pod, expected := range before {
		require.Equal(t, expected, f.observe(ctx, t, pod), "existing pod %s must not reload, apply, or change plans", pod)
	}
}

func (f *retainedFleet) waitWarning(ctx context.Context, t *testing.T, reason, detail string) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, retainedReadyBound, true, func(ctx context.Context) (bool, error) {
		events, err := Clientset().CoreV1().Events(f.namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.uid=" + string(f.config.UID)})
		if err != nil {
			return false, err
		}
		for i := range events.Items {
			event := &events.Items[i]
			if event.Type == corev1.EventTypeWarning && event.Reason == reason && strings.Contains(event.Message, detail) {
				return true, nil
			}
		}
		return false, nil
	}), "Warning %s must name %s", reason, detail)
}

func (f *retainedFleet) waitGauge(ctx context.Context, t *testing.T, pod string, value float64) {
	t.Helper()
	require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, retainedReadyBound, true, func(ctx context.Context) (bool, error) {
		body, err := f.metrics(ctx, pod, "9090")
		got, present := retainedMetric(body, "haptic_retained_config_active")
		return err == nil && present && got == value, nil
	}))
}

func (f *retainedFleet) assertPublicationUnchanged(ctx context.Context, t *testing.T, before *v1.HAProxyCfg) {
	t.Helper()
	after, err := f.haptic.HaproxyTemplateICV1alpha1().HAProxyCfgs(f.namespace).Get(ctx, before.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, before.Spec, after.Spec)
	require.Equal(t, before.Status.AuxiliaryFiles, after.Status.AuxiliaryFiles)
	require.Equal(t, before.Status.RetainedConfigs, after.Status.RetainedConfigs)
	client := f.haptic.HaproxyTemplateICV1alpha1()
	for name, beforeFile := range f.publishedFiles {
		file, err := client.HAProxyGeneralFiles(f.namespace).Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, beforeFile.UID, file.UID)
		require.Equal(t, beforeFile.Spec, file.Spec)
	}
	for name, beforeCheckpoint := range f.publishedCheckpoints {
		checkpoint, err := client.HAProxyCfgs(f.namespace).Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, beforeCheckpoint.UID, checkpoint.UID)
		require.Equal(t, beforeCheckpoint.Spec, checkpoint.Spec)
		require.Equal(t, beforeCheckpoint.Status, checkpoint.Status)
	}
}
