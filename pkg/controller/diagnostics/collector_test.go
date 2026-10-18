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

package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	agentclient "github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/client"
)

type fakePodAccess struct {
	state    api.State
	err      error
	pipeline string
}

func (f *fakePodAccess) GetFromPod(_ context.Context, _, path string, _ int64) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if path == "/debug/vars/pipeline" {
		return []byte(f.pipeline), nil
	}
	return []byte(`{"events":[{"type":"render.failed","timestamp":"2026-09-19T10:00:00Z","correlation_id":"attempt-1","error":"private-event"}]}`), nil
}
func (f *fakePodAccess) Exec(context.Context, string, string, []string, int64) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return json.Marshal(f.state)
}

func diagnosticFixture(t *testing.T) (*Collector, *fakePodAccess, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	input := diagnosticObject("haproxy-haptic.org/v1alpha1", "HAProxyTemplateConfig", "haptic-config")
	input.SetGeneration(1)
	input.Object["spec"] = map[string]any{"watchedResources": map[string]any{}}
	input.Object["status"] = map[string]any{"validationStatus": "Valid", "observedGeneration": int64(1)}
	output := diagnosticObject("haproxy-haptic.org/v1alpha1", "HAProxyCfg", "haptic-config-haproxycfg")
	output.Object["spec"] = map[string]any{"path": "/etc/haproxy/haproxy.cfg", "checksum": "aggregate-checksum", "content": "private-config"}
	output.Object["status"] = map[string]any{"deployedToPods": []any{map[string]any{
		"podName": "agent", "podUID": "agent-uid", "checksum": "aggregate-checksum", "appliedPlanID": "plan-1", "runningPlanID": "plan-1",
	}}}
	controllers := diagnosticPod("controller", "controller")
	controllers.Spec.Containers = []corev1.Container{{Name: "controller", Ports: []corev1.ContainerPort{{Name: "healthz", ContainerPort: 8080}}}}
	agents := diagnosticPod("agent", "loadbalancer")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "example.test", Version: "v2", Resource: "widgets"}: "WidgetList",
		{Group: "example.test", Version: "v1", Resource: "widgets"}: "WidgetList",
	}, input, output)
	pods := fake.NewClientset(controllers, agents)
	access := &fakePodAccess{pipeline: `{"rendering":{"status":"succeeded"},"validation":{"status":"succeeded","plan_id":"plan-1"},"deployment":{"status":"succeeded"}}`, state: api.State{
		APIVersion: api.Version, AgentOps: agentclient.ComposableOps(), AppliedPlanID: "plan-1", RunningPlanID: "plan-1",
		HAProxy: api.HAProxyInfo{WorkerPID: 10, WorkerStartTimeUnixMicros: 20},
		Files:   map[string]api.FileAt{"haproxy.cfg": {Digest: "individual-file-digest"}}, AppliedPlan: []byte("private-plan"),
	}}
	collector, err := New(&Options{Namespace: "haptic", Release: "haptic", ConfigName: "haptic-config", MaxResources: 100, MaxResponseBytes: 1024}, pods, dyn, func(int) PodAccess { return access })
	require.NoError(t, err)
	return collector, access, dyn
}

func diagnosticObject(version, kind, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{"name": name, "namespace": "haptic"}}}
}

func diagnosticPod(name, role string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "haptic", UID: types.UID(name + "-uid"), Labels: map[string]string{
		"app.kubernetes.io/instance": "haptic", "app.kubernetes.io/component": role}}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
}

func TestCollectHealthyFleetWithoutPayloads(t *testing.T) {
	collector, _, _ := diagnosticFixture(t)
	report := collector.Collect(t.Context())
	require.True(t, report.Complete, "%+v", report.Findings)
	require.True(t, report.Healthy, "%+v", report.Findings)
	require.Equal(t, "individual-file-digest", report.Agents[0].ConfigFileDigest)
	require.Equal(t, "render.failed", report.Controllers[0].LastFailure.Type)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-")
}

func TestCollectReportsUnavailableAndMismatchedFleet(t *testing.T) {
	tests := []struct {
		name, code string
		change     func(*Collector, *fakePodAccess)
	}{
		{name: "denied", code: "agent-unavailable", change: func(_ *Collector, a *fakePodAccess) { a.err = errors.New("private-denial") }},
		{name: "plan mismatch", code: "deployment-proof-mismatch", change: func(_ *Collector, a *fakePodAccess) { a.state.AppliedPlanID = "different" }},
		{name: "stopped worker", code: "agent-unhealthy", change: func(_ *Collector, a *fakePodAccess) { a.state.HAProxy.WorkerPID = 0 }},
		{name: "pipeline failed", code: "pipeline-failed", change: func(_ *Collector, a *fakePodAccess) {
			a.pipeline = `{"rendering":{"status":"failed","error":"private-render"}}`
		}},
		{name: "malformed pipeline", code: "pipeline-unavailable", change: func(_ *Collector, a *fakePodAccess) { a.pipeline = `[` }},
		{name: "replacement pod", code: "deployment-proof-missing", change: func(c *Collector, _ *fakePodAccess) {
			pod, err := c.client.CoreV1().Pods("haptic").Get(t.Context(), "agent", metav1.GetOptions{})
			require.NoError(t, err)
			pod.UID = "replacement"
			_, err = c.client.CoreV1().Pods("haptic").Update(t.Context(), pod, metav1.UpdateOptions{})
			require.NoError(t, err)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, a, _ := diagnosticFixture(t)
			tt.change(c, a)
			report := c.Collect(t.Context())
			require.False(t, report.Healthy)
			requireFinding(t, report, tt.code)
			encoded, err := json.Marshal(report)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "private-")
		})
	}
}

func requireFinding(t *testing.T, report *Report, code string) {
	t.Helper()
	for _, finding := range report.Findings {
		if finding.Code == code {
			return
		}
	}
	t.Fatalf("missing %s in %+v", code, report.Findings)
}

func TestCollectCustomWatchVersionsSelectorsAndLimit(t *testing.T) {
	c, _, dyn := diagnosticFixture(t)
	watchGVR := schema.GroupVersionResource{Group: "example.test", Version: "v1", Resource: "widgets"}
	input, err := dyn.Resource(inputGVR).Namespace("haptic").Get(t.Context(), "haptic-config", metav1.GetOptions{})
	require.NoError(t, err)
	input.Object["spec"] = map[string]any{"watchedResources": map[string]any{"custom": map[string]any{
		"apiVersions": []any{"example.test/v2", "example.test/v1"}, "resources": "widgets", "labelSelector": "app=chosen", "fieldSelector": "metadata.namespace=selected",
	}}}
	_, err = dyn.Resource(inputGVR).Namespace("haptic").Update(t.Context(), input, metav1.UpdateOptions{})
	require.NoError(t, err)
	calls := 0
	dyn.PrependReactor("list", "widgets", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if action.GetResource().Version == "v2" {
			return true, nil, apierrors.NewNotFound(watchGVR.GroupResource(), "")
		}
		require.Equal(t, "app=chosen", action.(ktesting.ListAction).GetListRestrictions().Labels.String())
		selected := diagnosticObject("example.test/v1", "Widget", "selected")
		selected.SetNamespace("selected")
		selected.SetLabels(map[string]string{"app": "chosen"})
		selected.Object["status"] = map[string]any{"stages": []any{map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "Pending", "message": "private-condition"}}}}}
		skipped := selected.DeepCopy()
		skipped.SetNamespace("elsewhere")
		return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*selected, *skipped}}, nil
	})
	report := c.Collect(t.Context())
	require.True(t, report.Complete, "%+v", report.Findings)
	require.True(t, report.Healthy)
	require.Equal(t, 2, calls)
	require.Len(t, report.Resources, 1)
	require.Equal(t, "Widget", report.Resources[0].Kind)
	require.Equal(t, "False", report.Resources[0].Conditions[0].Status)
	c.options.MaxResources = 1
	report = c.Collect(t.Context())
	require.False(t, report.Complete)
	requireFinding(t, report, "resource-limit-reached")
}

func TestCollectScopedWatchListsOnlyItsNamespaces(t *testing.T) {
	c, _, dyn := diagnosticFixture(t)
	input, err := dyn.Resource(inputGVR).Namespace("haptic").Get(t.Context(), "haptic-config", metav1.GetOptions{})
	require.NoError(t, err)
	input.Object["spec"] = map[string]any{"watchedResources": map[string]any{"custom": map[string]any{
		"apiVersion": "example.test/v1", "resources": "widgets", "namespaces": []any{"team-a", "team-b"},
	}}}
	_, err = dyn.Resource(inputGVR).Namespace("haptic").Update(t.Context(), input, metav1.UpdateOptions{})
	require.NoError(t, err)
	var listed []string
	dyn.PrependReactor("list", "widgets", func(action ktesting.Action) (bool, runtime.Object, error) {
		listed = append(listed, action.GetNamespace())
		return true, &unstructured.UnstructuredList{}, nil
	})
	report := c.Collect(t.Context())
	require.True(t, report.Complete, "%+v", report.Findings)
	require.Equal(t, []string{"team-a", "team-b"}, listed)
}

func TestCollectRejectsStaleLibraryRevision(t *testing.T) {
	c, _, dyn := diagnosticFixture(t)
	input, err := dyn.Resource(inputGVR).Namespace("haptic").Get(t.Context(), "haptic-config", metav1.GetOptions{})
	require.NoError(t, err)
	input.Object["spec"] = map[string]any{"libraryRefs": []any{map[string]any{"name": "base", "revision": "expected"}}}
	_, err = dyn.Resource(inputGVR).Namespace("haptic").Update(t.Context(), input, metav1.UpdateOptions{})
	require.NoError(t, err)
	library := diagnosticObject("haproxy-haptic.org/v1alpha1", "HAProxyTemplateLibrary", "base")
	library.Object["spec"] = map[string]any{}
	_, err = dyn.Resource(libraryGVR).Namespace("haptic").Create(t.Context(), library, metav1.CreateOptions{})
	require.NoError(t, err)
	report := c.Collect(t.Context())
	require.False(t, report.Complete)
	requireFinding(t, report, "library-revision-mismatch")
}

func TestCollectRejectsPublishedValidationErrorDespiteMatchingDeployment(t *testing.T) {
	collector, _, client := diagnosticFixture(t)
	config, err := client.Resource(outputGVR).Namespace("haptic").Get(t.Context(), "haptic-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(config.Object, "private-validation-failure", "status", "validationError"))
	_, err = client.Resource(outputGVR).Namespace("haptic").UpdateStatus(t.Context(), config, metav1.UpdateOptions{})
	require.NoError(t, err)

	report := collector.Collect(t.Context())
	require.True(t, report.Complete)
	require.False(t, report.Healthy)
	requireFinding(t, report, "configuration-validation-failed")
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-validation-failure")
}
