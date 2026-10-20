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

package configpublisher

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
)

func gateVerdictPublisher(t *testing.T, existing ...*haproxyv1alpha1.HAProxyCfg) (*Publisher, *fake.Clientset) {
	t.Helper()
	objects := make([]runtime.Object, 0, len(existing))
	for _, cfg := range existing {
		objects = append(objects, cfg)
	}
	crdClient := fake.NewSimpleClientset(objects...)
	return NewWithListers(k8sfake.NewClientset(), crdClient, nil, testLogger()), crdClient
}

func runtimeConfigFixture() *haproxyv1alpha1.HAProxyCfg {
	return &haproxyv1alpha1.HAProxyCfg{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config-haproxycfg", Namespace: "haptic"},
	}
}

func TestApplyGateVerdict_WritesTheConditions(t *testing.T) {
	publisher, crdClient := gateVerdictPublisher(t, runtimeConfigFixture())

	require.NoError(t, publisher.ApplyGateVerdict(t.Context(), &GateVerdict{
		Namespace: "haptic", Name: "test-config-haproxycfg", PlanID: "plan-2",
		Refused: true, Pinned: true, Message: "[ALERT] unknown keyword 'bogus'",
	}))

	updated, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("haptic").Get(t.Context(), "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)

	validated := meta.FindStatusCondition(updated.Status.Conditions, ConditionConfigValidated)
	require.NotNil(t, validated)
	assert.Equal(t, metav1.ConditionFalse, validated.Status)
	assert.Equal(t, reasonHAProxyRefused, validated.Reason)
	assert.Contains(t, validated.Message, "unknown keyword",
		"the operator's only pointer at what to fix is HAProxy's own message")

	pinned := meta.FindStatusCondition(updated.Status.Conditions, ConditionConfigPinned)
	require.NotNil(t, pinned)
	assert.Equal(t, metav1.ConditionTrue, pinned.Status)
	assert.Equal(t, reasonGateHolding, pinned.Reason)
}

// A gate that could not run is not HAProxy refusing the config, and the reason
// must say so or an operator debugs the wrong thing.
func TestApplyGateVerdict_SeparatesAnUnavailableGateFromARefusal(t *testing.T) {
	publisher, crdClient := gateVerdictPublisher(t, runtimeConfigFixture())

	require.NoError(t, publisher.ApplyGateVerdict(t.Context(), &GateVerdict{
		Namespace: "haptic", Name: "test-config-haproxycfg", PlanID: "plan-2",
		Message: "creating temp directory: read-only file system",
	}))

	updated, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("haptic").Get(t.Context(), "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	validated := meta.FindStatusCondition(updated.Status.Conditions, ConditionConfigValidated)
	require.NotNil(t, validated)
	assert.Equal(t, reasonGateUnavailable, validated.Reason)
}

// A pass clears both conditions, so a recovered fleet stops alerting.
func TestApplyGateVerdict_PassClearsThePin(t *testing.T) {
	publisher, crdClient := gateVerdictPublisher(t, runtimeConfigFixture())

	require.NoError(t, publisher.ApplyGateVerdict(t.Context(), &GateVerdict{
		Namespace: "haptic", Name: "test-config-haproxycfg", PlanID: "plan-2",
		Refused: true, Pinned: true, Message: "boom",
	}))
	require.NoError(t, publisher.ApplyGateVerdict(t.Context(), &GateVerdict{
		Namespace: "haptic", Name: "test-config-haproxycfg", PlanID: "plan-3", Accepted: true,
	}))

	updated, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("haptic").Get(t.Context(), "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, meta.IsStatusConditionTrue(updated.Status.Conditions, ConditionConfigValidated))
	assert.True(t, meta.IsStatusConditionFalse(updated.Status.Conditions, ConditionConfigPinned))
}

// The first publish creates the object; a verdict that arrives before it must
// not turn into an error the operator sees.
func TestApplyGateVerdict_MissingRuntimeConfigIsNotAnError(t *testing.T) {
	publisher, _ := gateVerdictPublisher(t)

	assert.NoError(t, publisher.ApplyGateVerdict(t.Context(), &GateVerdict{
		Namespace: "haptic", Name: "test-config-haproxycfg", PlanID: "plan-1", Accepted: true,
	}))
}

// An unchanged verdict must not rewrite the status on every render: each write
// is an etcd round-trip and a watch event for every reader.
func TestApplyGateVerdict_UnchangedVerdictSkipsTheWrite(t *testing.T) {
	publisher, crdClient := gateVerdictPublisher(t, runtimeConfigFixture())
	verdict := &GateVerdict{
		Namespace: "haptic", Name: "test-config-haproxycfg", PlanID: "plan-1", Accepted: true,
	}

	require.NoError(t, publisher.ApplyGateVerdict(t.Context(), verdict))
	crdClient.ClearActions()
	require.NoError(t, publisher.ApplyGateVerdict(t.Context(), verdict))

	for _, action := range crdClient.Actions() {
		assert.NotEqual(t, "update", action.GetVerb(),
			"an unchanged verdict must not write the status again")
	}
}

func TestApplyGateVerdict_NewPlanPreservesUnchangedStatus(t *testing.T) {
	for _, interval := range []time.Duration{0, time.Hour} {
		t.Run(interval.String(), func(t *testing.T) {
			cfg := runtimeConfigFixture()
			cfg.Generation = 7
			cfg.Status.DeployedToPods = []haproxyv1alpha1.PodDeploymentStatus{
				{PodName: "haproxy-1", AppliedPlanID: "deployed-plan"},
			}
			meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
				Type: "Deployed", Status: metav1.ConditionTrue, Reason: "Applied",
				Message: "Deployment succeeded", ObservedGeneration: cfg.Generation,
			})
			publisher, crdClient := gateVerdictPublisher(t, cfg)
			publisher.SetRepublishInterval(interval)
			verdict := acceptedVerdict()
			require.NoError(t, publisher.ApplyGateVerdict(t.Context(), verdict))
			client := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs(cfg.Namespace)
			before, err := client.Get(t.Context(), cfg.Name, metav1.GetOptions{})
			require.NoError(t, err)
			crdClient.ClearActions()

			for _, planID := range []string{"plan-2", "plan-3", "plan-4"} {
				verdict.PlanID = planID
				require.NoError(t, publisher.ApplyGateVerdict(t.Context(), verdict))
			}
			for _, action := range crdClient.Actions() {
				assert.Equal(t, "get", action.GetVerb(), "a new plan alone must not write status")
			}
			after, err := client.Get(t.Context(), cfg.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, before.Status, after.Status)
		})
	}
}

func TestApplyGateVerdict_PersistsConditionTransitions(t *testing.T) {
	publisher, crdClient := gateVerdictPublisher(t, runtimeConfigFixture())
	publisher.SetRepublishInterval(time.Hour)

	steps := []struct {
		name     string
		accepted bool
		refused  bool
		pinned   bool
		message  string
		reason   string
	}{
		{name: "accepted", accepted: true, reason: reasonHAProxyAccepted},
		{name: "refused", refused: true, message: "unknown keyword", reason: reasonHAProxyRefused},
		{name: "pinned", refused: true, pinned: true, message: "unknown keyword", reason: reasonHAProxyRefused},
		{name: "unavailable", pinned: true, message: "read-only file system", reason: reasonGateUnavailable},
		{name: "changed failure", pinned: true, message: "no space left on device", reason: reasonGateUnavailable},
		{name: "recovered", accepted: true, reason: reasonHAProxyAccepted},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			crdClient.ClearActions()
			require.NoError(t, publisher.ApplyGateVerdict(t.Context(), &GateVerdict{
				Namespace: "haptic", Name: "test-config-haproxycfg", PlanID: step.name,
				Accepted: step.accepted, Refused: step.refused, Pinned: step.pinned, Message: step.message,
			}))
			require.Len(t, crdClient.Actions(), 2)
			assert.Equal(t, "get", crdClient.Actions()[0].GetVerb())
			assert.Equal(t, "update", crdClient.Actions()[1].GetVerb())
			assert.Equal(t, "status", crdClient.Actions()[1].GetSubresource())

			cfg, err := crdClient.HaproxyTemplateICV1alpha1().
				HAProxyCfgs("haptic").Get(t.Context(), "test-config-haproxycfg", metav1.GetOptions{})
			require.NoError(t, err)
			validated := meta.FindStatusCondition(cfg.Status.Conditions, ConditionConfigValidated)
			require.NotNil(t, validated)
			assert.Equal(t, step.reason, validated.Reason)
			assert.Equal(t, step.accepted, meta.IsStatusConditionTrue(cfg.Status.Conditions, ConditionConfigValidated))
			assert.Equal(t, step.pinned, meta.IsStatusConditionTrue(cfg.Status.Conditions, ConditionConfigPinned))
			if !step.accepted {
				assert.Equal(t, step.message, validated.Message)
			}
		})
	}
}
