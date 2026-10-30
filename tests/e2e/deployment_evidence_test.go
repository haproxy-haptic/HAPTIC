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

//go:build e2e

package e2e

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
)

func TestControllerIdentityPodsRejectIncompleteEvidence(t *testing.T) {
	expected := controllerIdentity{sourceHash: "source", rolloutID: "rollout", binarySHA256: "binary"}
	base := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "controller-a",
			Annotations: map[string]string{
				"haproxy-haptic.org/source-hash":              "source",
				"haproxy-haptic.org/e2e-rollout-id":           "rollout",
				"haproxy-haptic.org/controller-binary-sha256": "binary",
			},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	for _, tc := range []struct {
		name      string
		mutate    func(*corev1.Pod)
		wantError string
	}{
		{name: "complete"},
		{name: "pending", mutate: func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }, wantError: "not Ready"},
		{name: "missing readiness", mutate: func(p *corev1.Pod) { p.Status.Conditions = nil }, wantError: "not Ready"},
		{name: "not ready", mutate: func(p *corev1.Pod) { p.Status.Conditions[0].Status = corev1.ConditionFalse }, wantError: "not Ready"},
		{name: "terminating", mutate: func(p *corev1.Pod) {
			stamp := metav1.NewTime(time.Unix(1, 0))
			p.DeletionTimestamp = &stamp
		}, wantError: "terminating"},
		{name: "source mismatch", mutate: func(p *corev1.Pod) { p.Annotations["haproxy-haptic.org/source-hash"] = "old" }, wantError: "source-hash"},
		{name: "rollout mismatch", mutate: func(p *corev1.Pod) { p.Annotations["haproxy-haptic.org/e2e-rollout-id"] = "old" }, wantError: "rollout-id"},
		{name: "binary mismatch", mutate: func(p *corev1.Pod) { p.Annotations["haproxy-haptic.org/controller-binary-sha256"] = "old" }, wantError: "binary-sha256"},
		{name: "missing annotations", mutate: func(p *corev1.Pod) { p.Annotations = nil }, wantError: "annotation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := base.DeepCopy()
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			got, err := verifyControllerIdentityPods([]corev1.Pod{*pod}, expected, 1)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"controller-a"}, got)
		})
	}
	_, err := verifyControllerIdentityPods(nil, expected, 1)
	require.ErrorContains(t, err, "0 controller pods found, expected 1")
	_, err = verifyControllerIdentityPods([]corev1.Pod{base}, expected, 2)
	require.ErrorContains(t, err, "1 controller pods found, expected 2")
}

func TestControllerStatsRequireEverySelectedPodAndMetric(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func([]kubeletPodStats)
		wantError string
	}{
		{name: "complete"},
		{name: "missing working set", mutate: func(p []kubeletPodStats) { p[1].Containers[0].Memory.WorkingSetBytes = nil }, wantError: "working-set"},
		{name: "missing RSS", mutate: func(p []kubeletPodStats) { p[1].Containers[0].Memory.RSSBytes = nil }, wantError: "RSS"},
		{name: "missing CPU", mutate: func(p []kubeletPodStats) { p[1].Containers[0].CPU.UsageCoreNanoSeconds = nil }, wantError: "CPU"},
		{name: "wrong namespace", mutate: func(p []kubeletPodStats) { p[1].PodRef.Namespace = "elsewhere" }, wantError: "1 of 2"},
		{name: "wrong container", mutate: func(p []kubeletPodStats) { p[1].Containers[0].Name = "sidecar" }, wantError: "1 of 2"},
		{name: "unknown pod", mutate: func(p []kubeletPodStats) { p[1].PodRef.Name = "unknown" }, wantError: "1 of 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pods []kubeletPodStats
			require.NoError(t, json.Unmarshal([]byte(`[
{"podRef":{"namespace":"haptic","name":"a"},"containers":[{"name":"controller","cpu":{"usageCoreNanoSeconds":2000000000},"memory":{"workingSetBytes":30,"rssBytes":10}}]},
{"podRef":{"namespace":"haptic","name":"b"},"containers":[{"name":"controller","cpu":{"usageCoreNanoSeconds":4000000000},"memory":{"workingSetBytes":20,"rssBytes":15}}]}
]`), &pods))
			if tc.mutate != nil {
				tc.mutate(pods)
			}
			stats := controllerStatsSnapshot{
				cpuSeconds:  map[string]float64{},
				workingSets: map[string]uint64{},
				rssValues:   map[string]uint64{},
			}
			stats.addPods(pods, map[string]bool{"a": true, "b": true})
			err := stats.requireComplete(2)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 30, stats.maxWorkingSet)
			require.EqualValues(t, 15, stats.maxRSS)
			require.Equal(t, map[string]float64{"a": 2, "b": 4}, stats.cpuSeconds)
		})
	}
}

func TestConfigMarkerEvidenceRejectsPartialContent(t *testing.T) {
	require.NoError(t, requireConfigMarkers("backend a\nbackend b\n", []string{"backend a", "backend b"}))
	require.ErrorContains(t, requireConfigMarkers("backend a\n", []string{"backend a", "backend b", "backend c"}), "2/3 markers not yet in rendered config (first missing: backend b)")
}

func TestConfigMarkerRemovalRequiresFleetReload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*haproxyv1alpha1.HAProxyCfg)
		replicas int
		want     bool
	}{
		{name: "fully running", replicas: 2, want: true},
		{name: "no replicas", replicas: 0},
		{name: "missing replica", replicas: 3},
		{name: "route still published", replicas: 2, mutate: func(c *haproxyv1alpha1.HAProxyCfg) { c.Spec.Content = "use_backend removed-route" }},
		{name: "missing checksum", replicas: 2, mutate: func(c *haproxyv1alpha1.HAProxyCfg) { c.Spec.Checksum = "" }},
		{name: "old checksum", replicas: 2, mutate: func(c *haproxyv1alpha1.HAProxyCfg) { c.Status.DeployedToPods[1].Checksum = "old" }},
		{name: "missing plan", replicas: 2, mutate: func(c *haproxyv1alpha1.HAProxyCfg) {
			c.Status.DeployedToPods[1].AppliedPlanID = ""
			c.Status.DeployedToPods[1].RunningPlanID = ""
		}},
		{name: "pending reload", replicas: 2, mutate: func(c *haproxyv1alpha1.HAProxyCfg) { c.Status.DeployedToPods[1].RunningPlanID = "old" }},
		{name: "runtime operations without reload", replicas: 2, mutate: func(c *haproxyv1alpha1.HAProxyCfg) {
			c.Status.DeployedToPods[1].RunningPlanID = "old"
			c.Status.DeployedToPods[1].WorkerOpsPlanID = "new"
		}},
		{name: "failed sync", replicas: 2, mutate: func(c *haproxyv1alpha1.HAProxyCfg) { c.Status.DeployedToPods[1].LastError = "failed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &haproxyv1alpha1.HAProxyCfg{
				Spec: haproxyv1alpha1.HAProxyCfgSpec{Content: "tcp-request content reject", Checksum: "current"},
				Status: haproxyv1alpha1.HAProxyCfgStatus{DeployedToPods: []haproxyv1alpha1.PodDeploymentStatus{
					{PodName: "a", Checksum: "current", AppliedPlanID: "new", RunningPlanID: "new"},
					{PodName: "b", Checksum: "current", AppliedPlanID: "new", RunningPlanID: "new"},
				}},
			}
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			got, err := configMarkerRemovalReloaded(cfg, "removed-route", tc.replicas)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestRateLimitBurstPreservesUnjudgeableMeasurements(t *testing.T) {
	for _, tc := range []struct {
		name        string
		output      string
		wantElapsed time.Duration
		wantCodes   map[string]int
	}{
		{"complete", "200\n429\nBURST_WINDOW 2 2.5\n", 500 * time.Millisecond, map[string]int{"200": 1, "429": 1}},
		{"missing timing", "200\n000\n", -1, map[string]int{"200": 1, "000": 1}},
		{"invalid timing", "BURST_WINDOW bad value\n", -1, map[string]int{}},
		{"backwards timing", "BURST_WINDOW 3 2\n", -1, map[string]int{}},
		{"empty logs", "", -1, map[string]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := parseRateLimitBurst(tc.output, 2, time.Second)
			require.Equal(t, 2, result.requested)
			require.Equal(t, time.Second, result.duration)
			require.Equal(t, tc.wantElapsed, result.podElapsed)
			require.Equal(t, tc.wantCodes, result.byCode)
		})
	}
}

func TestExhaustedRateLimitHeadersRequireEveryField(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*sharedRateLimitBurstResult)
	}{
		{"status", func(r *sharedRateLimitBurstResult) { r.headerProbeCode = "200" }},
		{"retry after", func(r *sharedRateLimitBurstResult) { r.headerRetryAfter = false }},
		{"limit", func(r *sharedRateLimitBurstResult) { r.headerLimit = false }},
		{"remaining", func(r *sharedRateLimitBurstResult) { r.headerRemaining = false }},
		{"reset", func(r *sharedRateLimitBurstResult) { r.headerReset = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := sharedRateLimitBurstResult{
				headerProbeCode:  "429",
				headerRetryAfter: true,
				headerLimit:      true,
				headerRemaining:  true,
				headerReset:      true,
			}
			require.True(t, result.hasExhaustedHeaders())
			tc.mutate(&result)
			require.False(t, result.hasExhaustedHeaders())
		})
	}
}

func TestRateLimitFallbackRequiresEveryPodAndOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(map[string]rateLimitOutageSignals)
		wantError string
	}{
		{name: "complete"},
		{name: "missing pod", mutate: func(m map[string]rateLimitOutageSignals) { delete(m, "b") }, wantError: "HAProxy pod b"},
		{name: "missing outcome", mutate: func(m map[string]rateLimitOutageSignals) { delete(m["b"].outcomes, rateLimitFallbackOutcomes[0]) }, wantError: "want at least 1"},
		{name: "unchanged outcome", mutate: func(m map[string]rateLimitOutageSignals) { m["b"].outcomes[rateLimitFallbackOutcomes[0]] = 7 }, wantError: "delta 0"},
		{name: "missing degradation", mutate: func(m map[string]rateLimitOutageSignals) {
			signal := m["b"]
			signal.degraded = 7
			m["b"] = signal
		}, wantError: "degraded transaction delta 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := rateLimitSignalFixture(7, 7)
			after := rateLimitSignalFixture(8, 7+float64(len(rateLimitFallbackOutcomes)))
			if tc.mutate != nil {
				tc.mutate(after)
			}
			err := validateLocalRateLimitFallback([]string{"a", "b"}, before, after)
			if tc.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantError)
			}
		})
	}
}

func rateLimitSignalFixture(outcomeValue, degraded float64) map[string]rateLimitOutageSignals {
	signals := map[string]rateLimitOutageSignals{}
	for _, pod := range []string{"a", "b"} {
		outcomes := map[string]float64{}
		for _, outcome := range rateLimitFallbackOutcomes {
			outcomes[outcome] = outcomeValue
		}
		signals[pod] = rateLimitOutageSignals{outcomes: outcomes, degraded: degraded}
	}
	return signals
}

func TestRouteParentsRequireEveryCurrentGeneration(t *testing.T) {
	parent := func(generation int64) map[string]any {
		return map[string]any{"conditions": []any{map[string]any{
			"type": "Accepted", "status": "False", "observedGeneration": generation,
		}}}
	}
	for _, tc := range []struct {
		name       string
		parents    []any
		want       bool
		wantReason string
	}{
		{name: "current negative condition", parents: []any{parent(2)}, want: true},
		{name: "both current", parents: []any{parent(2), parent(2)}, want: true},
		{name: "missing parents", wantReason: "status.parents is empty"},
		{name: "malformed parent", parents: []any{"invalid"}, wantReason: "malformed status.parents"},
		{name: "missing conditions", parents: []any{map[string]any{}}, wantReason: "no conditions"},
		{name: "malformed condition", parents: []any{map[string]any{"conditions": []any{"invalid"}}}, wantReason: "malformed condition"},
		{name: "stale second parent", parents: []any{parent(2), parent(1)}, wantReason: "observedGeneration 1"},
		{name: "future generation", parents: []any{parent(3)}, wantReason: "observedGeneration 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]any{
				"metadata": map[string]any{"generation": int64(2)},
				"status":   map[string]any{"parents": tc.parents},
			}}
			got, reason := routeParentsAtCurrentGeneration(obj)
			require.Equal(t, tc.want, got)
			if tc.wantReason == "" {
				require.Empty(t, reason)
			} else {
				require.Contains(t, reason, tc.wantReason)
			}
		})
	}
}
