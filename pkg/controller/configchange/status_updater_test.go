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

package configchange

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	crdclientfake "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
)

const (
	testNamespace = "default"
	testName      = "test-htc"
	// testGeneration is the metadata.generation carried by the fixture config, so
	// status.observedGeneration and the Validated condition's observedGeneration
	// are assertable.
	testGeneration int64 = 3
)

func newHTC() *v1alpha1.HAProxyTemplateConfig {
	return &v1alpha1.HAProxyTemplateConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  testNamespace,
			Name:       testName,
			Generation: testGeneration,
		},
	}
}

func newStatusUpdaterFixture(t *testing.T, existing ...runtime.Object) (*StatusUpdater, *crdclientfake.Clientset) {
	t.Helper()
	crdClient := crdclientfake.NewSimpleClientset(existing...)
	bus, logger := testutil.NewTestBusAndLogger()
	return NewStatusUpdater(crdClient, k8sfake.NewSimpleClientset(), bus, logger), crdClient
}

// testVerdicts numbers the verdicts tests hand the updater in the order they
// are built, as the config-change handler numbers its own.
var testVerdicts atomic.Uint64

func validatedVerdict(templateConfig any, version string) *events.ConfigValidatedEvent {
	event := events.NewConfigValidatedEvent(nil, templateConfig, version, "")
	event.VerdictSequence = testVerdicts.Add(1)
	return event
}

func invalidVerdict(version string, templateConfig any, validationErrors map[string][]string) *events.ConfigInvalidEvent {
	event := events.NewConfigInvalidEvent(version, templateConfig, validationErrors)
	event.VerdictSequence = testVerdicts.Add(1)
	return event
}

func inconclusiveVerdict(version string, templateConfig any, reasons map[string][]string) *events.ConfigValidationInconclusiveEvent {
	event := events.NewConfigValidationInconclusiveEvent(version, templateConfig, reasons, time.Second)
	event.VerdictSequence = testVerdicts.Add(1)
	return event
}

func TestStatusUpdater_EmitsEvents(t *testing.T) {
	u, _ := newStatusUpdaterFixture(t)
	rec := record.NewFakeRecorder(10)
	u.recorder = rec
	htc := &v1alpha1.HAProxyTemplateConfig{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName},
	}

	// Invalid -> a Warning Event carrying the first error (+ "more" hint).
	htc.Status = v1alpha1.HAProxyTemplateConfigStatus{
		ValidationStatus: statusInvalid,
		ValidationErrors: []string{"boom", "kaboom"},
	}
	u.emitStatusEvent(htc)
	assertNextEvent(t, rec, "Warning", eventReasonValidationFailed, "boom (+1 more)")

	// Invalid -> Valid is a recovery: a single Normal Event.
	htc.Status = v1alpha1.HAProxyTemplateConfigStatus{ValidationStatus: statusValid}
	u.emitStatusEvent(htc)
	assertNextEvent(t, rec, "Normal", eventReasonValidated, "valid again")

	// A subsequent routine Valid emits NOTHING (no spam on every success).
	u.emitStatusEvent(htc)
	select {
	case e := <-rec.Events:
		t.Fatalf("expected no event on routine success, got %q", e)
	default:
	}
}

// The render gate's verdict must reach `kubectl get events`, and only on a
// transition: it answers once per render, so repeating the Warning would bury
// the one that mattered under a steady stream of identical lines.
func TestStatusUpdater_EmitsRenderGateEvents(t *testing.T) {
	u, _ := newStatusUpdaterFixture(t)
	rec := record.NewFakeRecorder(10)
	u.recorder = rec
	u.adoptVerdict(1, true, []events.ConfigSourceRef{{Namespace: testNamespace, Name: testName}})

	u.handleRenderGateCompleted(events.NewRenderGateCompletedEvent(
		"plan-1", false, true, true, "[ALERT] unknown keyword 'bogus'", false, 12))
	assertNextEvent(t, rec, "Warning", eventReasonRenderRefused, "unknown keyword")

	// The same verdict again is not news.
	u.handleRenderGateCompleted(events.NewRenderGateCompletedEvent(
		"plan-2", false, true, true, "[ALERT] unknown keyword 'bogus'", false, 12))
	select {
	case e := <-rec.Events:
		t.Fatalf("expected no event on an unchanged verdict, got %q", e)
	default:
	}

	// Escalating to pinned is: nothing reaches the pods any more.
	u.handleRenderGateCompleted(events.NewRenderGateCompletedEvent(
		"plan-3", false, true, true, "[ALERT] unknown keyword 'bogus'", true, 12))
	assertNextEvent(t, rec, "Warning", eventReasonRenderRefused, "No new render reaches the pods")

	// Recovery is.
	u.handleRenderGateCompleted(events.NewRenderGateCompletedEvent("plan-4", true, false, true, "", false, 12))
	assertNextEvent(t, rec, "Normal", eventReasonRenderAccepted, "accepted the rendered configuration again")

	// A verdict for a plan the fleet has moved past says nothing about the
	// current render, so it emits nothing.
	u.handleRenderGateCompleted(events.NewRenderGateCompletedEvent(
		"plan-2", false, true, false, "[ALERT] unknown keyword 'bogus'", false, 12))
	select {
	case e := <-rec.Events:
		t.Fatalf("expected no event for a superseded plan's verdict, got %q", e)
	default:
	}
}

// The first pass of a term is not a recovery: nothing was refused before it.
func TestStatusUpdater_FirstRenderGatePassIsNotARecovery(t *testing.T) {
	u, _ := newStatusUpdaterFixture(t)
	rec := record.NewFakeRecorder(10)
	u.recorder = rec
	u.adoptVerdict(1, true, []events.ConfigSourceRef{{Namespace: testNamespace, Name: testName}})

	u.handleRenderGateCompleted(events.NewRenderGateCompletedEvent("plan-1", true, false, true, "", false, 12))

	select {
	case e := <-rec.Events:
		assert.Contains(t, e, "HAProxy accepted the rendered configuration")
		assert.NotContains(t, e, "again")
	default:
		t.Fatal("expected an Event for the term's first verdict")
	}
}

// A gate that could not run is not HAProxy refusing the configuration, and the
// Event has to say so or an operator debugs their own templates instead.
func TestStatusUpdater_RenderGateEventSeparatesAnUnavailableGate(t *testing.T) {
	u, _ := newStatusUpdaterFixture(t)
	rec := record.NewFakeRecorder(10)
	u.recorder = rec
	u.adoptVerdict(1, true, []events.ConfigSourceRef{{Namespace: testNamespace, Name: testName}})

	u.handleRenderGateCompleted(events.NewRenderGateCompletedEvent(
		"plan-1", false, false, true, "creating temp directory: read-only file system", true, 12))

	select {
	case e := <-rec.Events:
		assert.Contains(t, e, "could not run")
		assert.NotContains(t, e, "HAProxy refused")
	default:
		t.Fatal("expected an Event for a gate that could not run")
	}
}

// Without HAProxy's verdict no pod is reverted, so the Event must not send the
// operator looking for a rollback that never happened.
func TestStatusUpdater_UnavailableGateDoesNotClaimARevert(t *testing.T) {
	u, _ := newStatusUpdaterFixture(t)
	rec := record.NewFakeRecorder(10)
	u.recorder = rec
	u.adoptVerdict(1, true, []events.ConfigSourceRef{{Namespace: testNamespace, Name: testName}})

	u.handleRenderGateCompleted(events.NewRenderGateCompletedEvent(
		"plan-1", false, false, true, "creating temp directory: read-only file system", false, 12))

	select {
	case e := <-rec.Events:
		assert.Contains(t, e, "No pod was touched")
		assert.NotContains(t, e, "reverted")
	default:
		t.Fatal("expected an Event for a gate that could not run")
	}
}

func assertNextEvent(t *testing.T, rec *record.FakeRecorder, wantType, wantReason, wantMsgSubstr string) {
	t.Helper()
	select {
	case e := <-rec.Events:
		assert.Contains(t, e, wantType)
		assert.Contains(t, e, wantReason)
		if wantMsgSubstr != "" {
			assert.Contains(t, e, wantMsgSubstr)
		}
	default:
		t.Fatalf("expected an Event (%s/%s) but none was recorded", wantType, wantReason)
	}
}

func getStatus(t *testing.T, crdClient *crdclientfake.Clientset) v1alpha1.HAProxyTemplateConfigStatus {
	t.Helper()
	got, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyTemplateConfigs(testNamespace).
		Get(context.Background(), testName, metav1.GetOptions{})
	require.NoError(t, err)
	return got.Status
}

func TestNewStatusUpdater(t *testing.T) {
	u, _ := newStatusUpdaterFixture(t)
	require.NotNil(t, u)
	assert.NotNil(t, u.Base)
	assert.Equal(t, StatusUpdaterComponentName, u.Name())
}

func TestStatusUpdater_HandleConfigValidated_Success(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)

	u.handleConfigValidated(context.Background(), validatedVerdict(htc, "v1"))

	status := getStatus(t, crd)
	assert.Equal(t, "Valid", status.ValidationStatus)
	assert.Equal(t, "Configuration validated successfully", status.ValidationMessage)
	assert.Nil(t, status.ValidationErrors)
	assert.NotNil(t, status.LastValidated)

	// observedGeneration records the generation the controller processed, and the
	// Validated condition reports it True with the same generation.
	assert.Equal(t, testGeneration, status.ObservedGeneration)
	cond := meta.FindStatusCondition(status.Conditions, conditionValidated)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, reasonValidationSucceeded, cond.Reason)
	assert.Equal(t, testGeneration, cond.ObservedGeneration)

	// Cached config references should be populated for subsequent HAProxy validation events.
	u.mu.RLock()
	require.Len(t, u.configRefs, 1)
	assert.Equal(t, testNamespace, u.configRefs[0].Namespace)
	assert.Equal(t, testName, u.configRefs[0].Name)
	assert.Equal(t, testGeneration, u.configRefs[0].Generation)
	u.mu.RUnlock()
}

// TestReportConfigLoadFailure covers the synchronous startup path: when the
// load gate rejects the config, the iteration writes an Invalid status
// (observedGeneration + Validated=False/LoadGateFailed + the failing tests)
// before it fails and retries, so operators can see WHY via kubectl.
func TestReportConfigLoadFailure(t *testing.T) {
	htc := newHTC()
	crdClient := crdclientfake.NewSimpleClientset(htc)
	_, logger := testutil.NewTestBusAndLogger()

	failures := []string{"test-ssl-x failed: boom", "test-ssl-y failed: kaboom"}
	ReportConfigLoadFailure(context.Background(), crdClient, events.ConfigSourceRef{
		Namespace: htc.Namespace, Name: htc.Name, Generation: htc.Generation,
	}, failures, logger)

	status := getStatus(t, crdClient)
	assert.Equal(t, "Invalid", status.ValidationStatus)
	assert.Equal(t, testGeneration, status.ObservedGeneration)
	assert.ElementsMatch(t, failures, status.ValidationErrors)
	assert.Contains(t, status.ValidationMessage, "startup load gate")

	cond := meta.FindStatusCondition(status.Conditions, conditionValidated)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonLoadGateFailed, cond.Reason)
	assert.Equal(t, testGeneration, cond.ObservedGeneration)
	assert.Contains(t, cond.Message, "boom") // first failure surfaces in the condition
}

func TestReportConfigLoadFailure_GetError(t *testing.T) {
	// CRD not seeded — Get() returns NotFound; must not panic, just log + return.
	crdClient := crdclientfake.NewSimpleClientset()
	_, logger := testutil.NewTestBusAndLogger()
	htc := newHTC()
	ReportConfigLoadFailure(context.Background(), crdClient, events.ConfigSourceRef{
		Namespace: htc.Namespace, Name: htc.Name, Generation: htc.Generation,
	}, []string{"boom"}, logger)
}

func TestStatusUpdater_HandleConfigInvalid(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)

	u.handleConfigInvalid(context.Background(), invalidVerdict("v1", htc, map[string][]string{
		"template": {"boom", "kaboom"},
	}))

	status := getStatus(t, crd)
	assert.Equal(t, "Invalid", status.ValidationStatus)
	assert.Equal(t, testGeneration, status.ObservedGeneration)
	assert.ElementsMatch(t, []string{"boom", "kaboom"}, status.ValidationErrors)

	cond := meta.FindStatusCondition(status.Conditions, conditionValidated)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, reasonConfigInvalid, cond.Reason)
	assert.Equal(t, testGeneration, cond.ObservedGeneration)
	// The condition message surfaces the first error so `kubectl describe` is useful.
	assert.Contains(t, cond.Message, "boom")
}

// TestStatusUpdater_ObservedGenerationTracksValidatedGeneration pins the drift
// semantics: observedGeneration reflects the generation that was actually
// validated, not the (possibly newer) live spec — so a reader can tell the
// controller is behind without any controller-version field on the spec.
func TestStatusUpdater_ObservedGenerationTracksValidatedGeneration(t *testing.T) {
	stored := newHTC()
	stored.Generation = 5 // live spec has moved on
	u, crd := newStatusUpdaterFixture(t, stored)

	validated := newHTC()
	validated.Generation = 4 // but we validated the previous generation
	u.handleConfigValidated(context.Background(), validatedVerdict(validated, "v1"))

	status := getStatus(t, crd)
	assert.Equal(t, int64(4), status.ObservedGeneration)
	cond := meta.FindStatusCondition(status.Conditions, conditionValidated)
	require.NotNil(t, cond)
	assert.Equal(t, int64(4), cond.ObservedGeneration)
}

func TestStatusUpdater_HandleConfigValidated_SkipsInitialVersion(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)

	u.handleConfigValidated(context.Background(), events.NewConfigValidatedEvent(nil, htc, "initial", ""))

	// Nothing should have changed — ValidationStatus stays empty.
	assert.Empty(t, getStatus(t, crd).ValidationStatus)
}

func TestStatusUpdater_HandleConfigValidated_SkipsActiveSnapshotRestore(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)

	u.handleConfigValidated(context.Background(), newActiveSnapshotRestore(validatedVerdict(htc, "active")))

	assert.Empty(t, getStatus(t, crd).ValidationStatus)
}

func TestStatusUpdater_HandleConfigValidated_WrongTemplateType(t *testing.T) {
	u, crd := newStatusUpdaterFixture(t, newHTC())

	// TemplateConfig is not an *HAProxyTemplateConfig — handler should skip silently.
	u.handleConfigValidated(context.Background(), validatedVerdict("not-a-crd", "v1"))

	assert.Empty(t, getStatus(t, crd).ValidationStatus)
}

func TestStatusUpdater_HandleConfigValidated_GetError(t *testing.T) {
	// CRD not seeded — Get() will return NotFound.
	u, crd := newStatusUpdaterFixture(t)
	htc := newHTC()

	// Must not panic or leave partial state.
	u.handleConfigValidated(context.Background(), validatedVerdict(htc, "v1"))

	_, err := crd.HaproxyTemplateICV1alpha1().
		HAProxyTemplateConfigs(testNamespace).
		Get(context.Background(), testName, metav1.GetOptions{})
	require.Error(t, err)

	// Cache was still populated before the Get() call — that's by design.
	u.mu.RLock()
	require.Len(t, u.configRefs, 1)
	assert.Equal(t, testNamespace, u.configRefs[0].Namespace)
	u.mu.RUnlock()
}

func TestStatusUpdater_HandleConfigValidated_UpdateError(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)

	// Make UpdateStatus fail to exercise the warning branch without affecting state.
	crd.PrependReactor("update", "haproxytemplateconfigs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if subAction, ok := a.(clienttesting.UpdateAction); ok && subAction.GetSubresource() == "status" {
			return true, nil, errors.New("forced update failure")
		}
		return false, nil, nil
	})

	// Must not panic.
	u.handleConfigValidated(context.Background(), validatedVerdict(htc, "v1"))
}

func TestStatusUpdater_HandleConfigInvalid_Success(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)

	errs := map[string][]string{
		"basic":    {"template is empty"},
		"template": {"unknown identifier", "syntax error at line 3"},
	}

	u.handleConfigInvalid(context.Background(), invalidVerdict("v2", htc, errs))

	status := getStatus(t, crd)
	assert.Equal(t, "Invalid", status.ValidationStatus)
	assert.Len(t, status.ValidationErrors, 3)
	assert.Contains(t, status.ValidationMessage, "3 validation error(s)")
	assert.NotNil(t, status.LastValidated)

	u.mu.RLock()
	require.Len(t, u.configRefs, 1)
	assert.Equal(t, testName, u.configRefs[0].Name)
	u.mu.RUnlock()
}

func TestStatusUpdater_HandleConfigInvalid_WrongTemplateType(t *testing.T) {
	u, crd := newStatusUpdaterFixture(t, newHTC())

	u.handleConfigInvalid(context.Background(), invalidVerdict("v2", "not-a-crd", nil))
	assert.Empty(t, getStatus(t, crd).ValidationStatus)
}

func TestStatusUpdater_HandleConfigInvalid_GetError(t *testing.T) {
	u, _ := newStatusUpdaterFixture(t)
	u.handleConfigInvalid(context.Background(), invalidVerdict("v2", newHTC(), nil))
}

func TestStatusUpdater_HandleHAProxyValidationFailed_WithoutCachedRef(t *testing.T) {
	u, crd := newStatusUpdaterFixture(t, newHTC())

	// No prior handleConfigValidated/Invalid call — cache is empty, handler should no-op.
	u.handleHAProxyValidationFailed(context.Background(), events.NewValidationFailedEvent([]string{"err"}, 42, "test"))

	assert.Empty(t, getStatus(t, crd).ValidationStatus)
}

func TestStatusUpdater_HandleHAProxyValidationFailed_Success(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)

	// Prime cache via a previous ConfigValidated handler call.
	u.handleConfigValidated(context.Background(), validatedVerdict(htc, "v1"))

	errs := []string{"haproxy: line 42: unknown keyword"}
	u.handleHAProxyValidationFailed(context.Background(), events.NewValidationFailedEvent(errs, 100, "reconcile"))

	status := getStatus(t, crd)
	assert.Equal(t, "Invalid", status.ValidationStatus)
	assert.Equal(t, "HAProxy configuration validation failed", status.ValidationMessage)
	assert.Equal(t, errs, status.ValidationErrors)
}

// A failed HAProxy check leaves the condition False only until HAProxy
// accepts a render again; a load is not needed to clear it.
func TestStatusUpdater_PassingGateRestoresValidatedAfterHAProxyFailure(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)
	u.handleConfigValidated(context.Background(), validatedVerdict(htc, "v1"))
	u.handleHAProxyValidationFailed(context.Background(), events.NewValidationFailedEvent([]string{"refused"}, 1, "reconcile"))
	require.Equal(t, "Invalid", getStatus(t, crd).ValidationStatus)

	// A verdict for a superseded render, or a failing one, changes nothing.
	u.HandleEvent(events.NewRenderGateCompletedEvent("plan-old", true, false, false, "", false, 1))
	u.HandleEvent(events.NewRenderGateCompletedEvent("plan-1", false, true, true, "[ALERT] bogus", false, 1))
	assert.Equal(t, "Invalid", getStatus(t, crd).ValidationStatus)

	u.HandleEvent(events.NewRenderGateCompletedEvent("plan-2", true, false, true, "", false, 1))
	status := getStatus(t, crd)
	assert.Equal(t, "Valid", status.ValidationStatus)
	assert.Empty(t, status.ValidationErrors)
	require.NotEmpty(t, status.Conditions)
	assert.Equal(t, metav1.ConditionTrue, status.Conditions[0].Status)
	assert.Equal(t, "HAProxy accepted the rendered configuration again", status.Conditions[0].Message)

	// A pass with nothing to restore leaves the status alone.
	u.HandleEvent(events.NewRenderGateCompletedEvent("plan-3", true, false, true, "", false, 1))
	assert.Equal(t, "Valid", getStatus(t, crd).ValidationStatus)

	// A load that fails after a refused check owns the condition from then on:
	// a verdict accepting the previous configuration's render must not flip
	// the new, invalid configuration to Valid.
	u.handleHAProxyValidationFailed(context.Background(), events.NewValidationFailedEvent([]string{"refused"}, 1, "reconcile"))
	u.handleConfigInvalid(context.Background(), invalidVerdict("v2", htc, map[string][]string{"basic": {"bad"}}))
	u.HandleEvent(events.NewRenderGateCompletedEvent("plan-4", true, false, true, "", false, 1))
	status = getStatus(t, crd)
	assert.Equal(t, "Invalid", status.ValidationStatus)
	assert.Equal(t, metav1.ConditionFalse, status.Conditions[0].Status)
}

func TestStatusUpdater_HandleHAProxyValidationFailed_GetError(t *testing.T) {
	// Seed an HTC so cache can be populated, then delete it so the subsequent Get fails.
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)
	u.handleConfigValidated(context.Background(), validatedVerdict(htc, "v1"))
	require.NoError(t, crd.HaproxyTemplateICV1alpha1().HAProxyTemplateConfigs(testNamespace).Delete(context.Background(), testName, metav1.DeleteOptions{}))

	u.handleHAProxyValidationFailed(context.Background(), events.NewValidationFailedEvent([]string{"x"}, 1, ""))
}

// TestStatusUpdater_Integration exercises the Start/Stop loop end-to-end by publishing
// one event of each handled type through the real event bus and asserting the CRD
// reflects the final state.
func TestStatusUpdater_Integration(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)
	u.EventBus().Start()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = u.Start(ctx)
		close(done)
	}()
	<-u.SubscriptionReady()

	u.EventBus().Publish(validatedVerdict(htc, "v1"))

	require.Eventually(t, func() bool {
		return getStatus(t, crd).ValidationStatus == "Valid"
	}, 2*time.Second, 10*time.Millisecond)

	u.EventBus().Publish(events.NewValidationFailedEvent([]string{"haproxy: bad"}, 5, ""))

	require.Eventually(t, func() bool {
		s := getStatus(t, crd)
		return s.ValidationMessage == "HAProxy configuration validation failed"
	}, 2*time.Second, 10*time.Millisecond)

	u.EventBus().Publish(invalidVerdict("v2", htc, map[string][]string{"basic": {"err"}}))

	require.Eventually(t, func() bool {
		s := getStatus(t, crd)
		return s.ValidationStatus == "Invalid" && len(s.ValidationErrors) == 1 && s.ValidationErrors[0] == "err"
	}, 2*time.Second, 10*time.Millisecond)

	u.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StatusUpdater did not stop in time")
	}
}

// TestStatusUpdater_DoubleStop verifies Stop is idempotent (sync.Once via
// component.Base): a second call must not panic on an already-closed channel.
func TestStatusUpdater_DoubleStop(t *testing.T) {
	u, _ := newStatusUpdaterFixture(t, newHTC())
	u.Stop()
	assert.NotPanics(t, u.Stop)
}

// TestStatusUpdater_StopViaContext verifies context cancellation shuts down Start().
func TestStatusUpdater_StopViaContext(t *testing.T) {
	u, _ := newStatusUpdaterFixture(t, newHTC())
	u.EventBus().Start()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = u.Start(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancel")
	}
}

// Sanity check: HTC error status text is correctly formatted as N validation error(s).
func TestStatusUpdater_ErrorCountMessage(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)
	errs := map[string][]string{"a": {"1", "2"}, "b": {"3"}}
	u.handleConfigInvalid(context.Background(), invalidVerdict("v3", htc, errs))
	assert.Equal(t, fmt.Sprintf("%d validation error(s)", 3), getStatus(t, crd).ValidationMessage)
}

// shard builds one member of a merged set.
func shard(name string, generation int64) *v1alpha1.HAProxyTemplateConfig {
	return &v1alpha1.HAProxyTemplateConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  testNamespace,
			Name:       name,
			Generation: generation,
		},
	}
}

func shardStatus(t *testing.T, crdClient *crdclientfake.Clientset, name string) v1alpha1.HAProxyTemplateConfigStatus {
	t.Helper()
	got, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyTemplateConfigs(testNamespace).
		Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return got.Status
}

// The verdict is a property of the merged set, so every source is stamped —
// with ITS OWN observedGeneration, because that field is only meaningful
// against the same object's metadata.generation. A designated primary could
// not represent a shard edit at all (ADR-0016).
func TestStatusUpdater_StampsEverySourceOfTheMergedSet(t *testing.T) {
	a, b, c := shard("haptic-config-base", 4), shard("haptic-config-ssl", 7), shard("haptic-config", 2)
	u, crdClient := newStatusUpdaterFixture(t, a, b, c)

	event := validatedVerdict(c, "v9")
	event.Sources = []events.ConfigSourceRef{
		{Namespace: testNamespace, Name: a.Name, Generation: a.Generation},
		{Namespace: testNamespace, Name: b.Name, Generation: b.Generation},
		{Namespace: testNamespace, Name: c.Name, Generation: c.Generation},
	}
	u.handleConfigValidated(context.Background(), event)

	for _, tc := range []struct {
		name       string
		generation int64
	}{
		{a.Name, a.Generation}, {b.Name, b.Generation}, {c.Name, c.Generation},
	} {
		status := shardStatus(t, crdClient, tc.name)
		assert.Equal(t, "Valid", status.ValidationStatus, tc.name)
		assert.Equal(t, tc.generation, status.ObservedGeneration,
			"%s must carry ITS OWN generation, not the merged identity's", tc.name)
		cond := meta.FindStatusCondition(status.Conditions, conditionValidated)
		require.NotNil(t, cond, tc.name)
		assert.Equal(t, tc.generation, cond.ObservedGeneration, tc.name)
	}
}

// A repeat verdict must not write: with N sources per set, N-1 statuses are
// usually unchanged, and rewriting them would churn resourceVersions for a
// timestamp refresh nobody reads.
func TestStatusUpdater_SkipsNoOpStatusWrites(t *testing.T) {
	htc := newHTC()
	u, crdClient := newStatusUpdaterFixture(t, htc)

	event := validatedVerdict(htc, "v1")
	u.handleConfigValidated(context.Background(), event)

	var writes int
	crdClient.PrependReactor("update", "haproxytemplateconfigs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if subAction, ok := a.(clienttesting.UpdateAction); ok && subAction.GetSubresource() == "status" {
			writes++
		}
		return false, nil, nil
	})

	// Same verdict again — everything but LastValidated would be identical.
	u.handleConfigValidated(context.Background(), validatedVerdict(htc, "v2"))
	assert.Zero(t, writes, "an unchanged status must not be rewritten")

	// A real change still writes.
	u.handleConfigInvalid(context.Background(), invalidVerdict("v3", htc, map[string][]string{
		"basic": {"boom"},
	}))
	assert.Equal(t, 1, writes, "a changed status must still be written")
}

func TestStatusUpdaterEmitsHTTPContentRevocationEffects(t *testing.T) {
	tests := map[string]struct {
		restored bool
		critical bool
		message  string
	}{
		"optional": {message: "Templates render without it"},
		"critical": {critical: true, message: "nothing new deploys"},
		"restored": {restored: true, critical: true, message: "previously accepted content is served"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			updater, _ := newStatusUpdaterFixture(t)
			recorder := record.NewFakeRecorder(1)
			updater.recorder = recorder
			updater.adoptVerdict(1, true, []events.ConfigSourceRef{{Namespace: testNamespace, Name: testName}})

			updater.HandleEvent(events.NewHTTPContentRevokedEvent(
				"https://example.test/page", "checksum", test.restored, test.critical))

			assertNextEvent(t, recorder, "Warning", eventReasonContentRevoked, test.message)
		})
	}
}

func TestStatusUpdaterHTTPRejectionIdentifiesConfig(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	broadcaster := record.NewBroadcaster()
	defer broadcaster.Shutdown()
	recorded := make(chan *corev1.Event, 1)
	watcher := broadcaster.StartEventWatcher(func(event *corev1.Event) { recorded <- event })
	defer watcher.Stop()
	updater, _ := newStatusUpdaterFixture(t)
	updater.recorder = broadcaster.NewRecorder(scheme, corev1.EventSource{Component: "haptic"})
	htc := newHTC()
	htc.UID = "config-uid"
	htc.ResourceVersion = "42"
	updater.adoptVerdict(1, true, sourceRefsOrFallback(nil, htc))

	updater.HandleEvent(events.NewHTTPContentRejectedEvent("https://example.test/list"))
	select {
	case event := <-recorded:
		assert.Equal(t, corev1.EventTypeWarning, event.Type)
		assert.Equal(t, eventReasonContentRejected, event.Reason)
		assert.Equal(t, htc.UID, event.InvolvedObject.UID)
		assert.Equal(t, htc.ResourceVersion, event.InvolvedObject.ResourceVersion)
		assert.Equal(t, htc.Name, event.InvolvedObject.Name)
		assert.Equal(t, htc.Namespace, event.InvolvedObject.Namespace)
		assert.Equal(t, "HAProxyTemplateConfig", event.InvolvedObject.Kind)
		assert.Contains(t, event.Message, "https://example.test/list")
		assert.Contains(t, event.Message, "previously accepted content remains active")
		assert.Contains(t, event.Message, "Fix the fetched content or its template")
	case <-time.After(time.Second):
		t.Fatal("no HTTPContentRejected Warning recorded")
	}
}
