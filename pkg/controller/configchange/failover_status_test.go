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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	crdclientfake "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
	"github.com/haproxy-haptic/HAPTIC/pkg/lifecycle"
)

type verdictKind int

const (
	verdictValid verdictKind = iota
	verdictInvalid
	verdictInconclusive
)

// failoverRunningGeneration is the generation the replica's iteration runs.
const failoverRunningGeneration int64 = 3

func failoverVersion(generation int64) string { return fmt.Sprintf("rv-%d", generation) }

func failoverHTC(generation int64) *v1alpha1.HAProxyTemplateConfig {
	htc := newHTC()
	htc.Generation = generation
	htc.ResourceVersion = failoverVersion(generation)
	return htc
}

func failoverSources(generation int64) []events.ConfigSourceRef {
	return []events.ConfigSourceRef{{Namespace: testNamespace, Name: testName, Generation: generation}}
}

// failoverReplica is one controller replica: its config-change handler and
// its (leader-only) status updater on one bus, as an iteration wires them.
type failoverReplica struct {
	bus      *busevents.EventBus
	handler  *ConfigChangeHandler
	updater  *StatusUpdater
	crd      *crdclientfake.Clientset
	verdicts <-chan busevents.Event

	mu        sync.Mutex
	verdictOf map[string]verdictKind
}

func newFailoverReplica(t *testing.T, seed *v1alpha1.HAProxyTemplateConfigStatus) *failoverReplica {
	t.Helper()
	stored := failoverHTC(failoverRunningGeneration)
	if seed != nil {
		stored.Status = *seed.DeepCopy()
	}
	crd := crdclientfake.NewSimpleClientset(stored)
	bus, logger := testutil.NewTestBusAndLogger()
	r := &failoverReplica{
		bus:       bus,
		crd:       crd,
		handler:   NewConfigChangeHandler(bus, logger, make(chan *ReloadRequest, 1), []string{"template"}, testDebounceInterval),
		updater:   NewStatusUpdater(crd, k8sfake.NewSimpleClientset(), bus, logger),
		verdictOf: map[string]verdictKind{},
	}
	r.verdicts = bus.SubscribeTypes("test-verdicts", 100, events.EventTypeConfigVerdictReplayed,
		events.EventTypeConfigValidated, events.EventTypeConfigInvalid, events.EventTypeConfigValidationInconclusive)
	r.answerValidation(t)

	running := failoverHTC(failoverRunningGeneration)
	r.handler.SetInitialSnapshot(&ValidatedSnapshot{
		RawConfig:      &coreconfig.Config{},
		Config:         &coreconfig.Config{},
		TemplateConfig: running,
		ConfigVersion:  running.ResourceVersion,
		Sources:        failoverSources(failoverRunningGeneration),
	})
	r.handler.EnableReinitialization()
	bus.Start()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = r.handler.Start(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return r
}

// answerValidation plays the validators, answering per config version.
func (r *failoverReplica) answerValidation(t *testing.T) {
	t.Helper()
	requests := r.bus.SubscribeTypes("test-validator", 50, events.EventTypeConfigValidationRequest)
	done := make(chan struct{})
	t.Cleanup(func() { <-done })
	go func() {
		defer close(done)
		defer r.bus.UnsubscribeTyped(requests)
		for {
			var event busevents.Event
			select {
			case <-t.Context().Done():
				return
			case event = <-requests:
			}
			req, ok := event.(*events.ConfigValidationRequest)
			if !ok {
				continue
			}
			r.mu.Lock()
			kind := r.verdictOf[req.Version]
			r.mu.Unlock()
			switch kind {
			case verdictValid:
				r.bus.Publish(events.NewConfigValidationResponse(req.RequestID(), "template", true, nil))
			case verdictInvalid:
				r.bus.Publish(events.NewConfigValidationResponse(req.RequestID(), "template", false,
					[]string{"undefined: cidr_partition"}))
			case verdictInconclusive:
				r.bus.Publish(events.NewConfigValidationInconclusiveResponse(req.RequestID(), "template",
					[]string{schemaTimeoutReason}))
			}
		}
	}()
}

// observe delivers a generation the way the config watcher does and waits for
// this replica's verdict on it.
func (r *failoverReplica) observe(t *testing.T, generation int64, kind verdictKind) {
	t.Helper()
	version := failoverVersion(generation)
	r.mu.Lock()
	r.verdictOf[version] = kind
	r.mu.Unlock()
	parsed := events.NewConfigParsedEvent(&coreconfig.Config{}, failoverHTC(generation), version, "")
	parsed.Sources = failoverSources(generation)
	r.bus.Publish(parsed)

	deadline := time.After(10 * time.Second)
	for {
		select {
		case event := <-r.verdicts:
			if verdictVersion(event) == version {
				return
			}
		case <-deadline:
			t.Fatalf("no verdict on generation %d", generation)
		}
	}
}

func verdictVersion(event busevents.Event) string {
	switch e := event.(type) {
	case *events.ConfigValidatedEvent:
		return e.Version
	case *events.ConfigInvalidEvent:
		return e.Version
	case *events.ConfigValidationInconclusiveEvent:
		return e.Version
	}
	return ""
}

// becomeLeader follows the leader-election adapter: pause the bus, publish
// BecameLeaderEvent, start the leader-only components, resume the bus.
func (r *failoverReplica) becomeLeader(t *testing.T) {
	t.Helper()
	registry := lifecycle.NewRegistry()
	registry.Register(r.updater, true)
	ctx, cancel := context.WithCancel(context.Background())
	r.bus.Pause()
	r.bus.Publish(events.NewBecameLeaderEvent("new-leader"))
	run, err := registry.StartLeaderOnly(ctx)
	require.NoError(t, err)
	r.bus.Start()
	t.Cleanup(func() {
		cancel()
		_ = run.Wait()
	})
}

// statusWrites lists every status written through crd, in order.
func statusWrites(crd *crdclientfake.Clientset) []v1alpha1.HAProxyTemplateConfigStatus {
	var writes []v1alpha1.HAProxyTemplateConfigStatus
	for _, action := range crd.Actions() {
		update, ok := action.(clienttesting.UpdateAction)
		if !ok || update.GetSubresource() != "status" {
			continue
		}
		if htc, ok := update.GetObject().(*v1alpha1.HAProxyTemplateConfig); ok {
			writes = append(writes, *htc.Status.DeepCopy())
		}
	}
	return writes
}

type wantVerdict struct {
	status     metav1.ConditionStatus
	reason     string
	generation int64
}

func verdictStatus(want wantVerdict) v1alpha1.HAProxyTemplateConfigStatus {
	status := v1alpha1.HAProxyTemplateConfigStatus{ObservedGeneration: want.generation}
	setValidatedCondition(&status, want.status, want.reason, "written by the previous leader", want.generation)
	return status
}

func requireVerdict(t *testing.T, status *v1alpha1.HAProxyTemplateConfigStatus, want wantVerdict) {
	t.Helper()
	cond := meta.FindStatusCondition(status.Conditions, conditionValidated)
	require.NotNil(t, cond)
	assert.Equal(t, want.status, cond.Status)
	assert.Equal(t, want.reason, cond.Reason)
	assert.Equal(t, want.generation, cond.ObservedGeneration)
	assert.Equal(t, want.generation, status.ObservedGeneration)
}

func TestStatusUpdater_NewLeaderWritesItsNewestVerdict(t *testing.T) {
	tests := []struct {
		name string
		// later are this replica's verdicts on the generations after the
		// running one, reached while it follows.
		later  []verdictKind
		newest wantVerdict
	}{
		{
			name:   "valid then invalid",
			later:  []verdictKind{verdictInvalid},
			newest: wantVerdict{metav1.ConditionFalse, reasonConfigInvalid, failoverRunningGeneration + 1},
		},
		{
			name:   "valid then inconclusive",
			later:  []verdictKind{verdictInconclusive},
			newest: wantVerdict{metav1.ConditionUnknown, reasonValidationInconclusive, failoverRunningGeneration + 1},
		},
		{
			name:   "invalid then valid",
			later:  []verdictKind{verdictInvalid, verdictValid},
			newest: wantVerdict{metav1.ConditionTrue, reasonValidationSucceeded, failoverRunningGeneration + 2},
		},
	}
	for _, tt := range tests {
		for _, previousLeaderWroteNewest := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/previous leader wrote newest=%t", tt.name, previousLeaderWroteNewest), func(t *testing.T) {
				verifyNewestVerdictTakeover(t, tt.later, tt.newest, previousLeaderWroteNewest)
			})
		}
	}
}

func verifyNewestVerdictTakeover(t *testing.T, later []verdictKind, newest wantVerdict, previousLeaderWroteNewest bool) {
	t.Helper()
	runningValid := wantVerdict{metav1.ConditionTrue, reasonValidationSucceeded, failoverRunningGeneration}
	seed := verdictStatus(runningValid)
	if previousLeaderWroteNewest {
		seed = verdictStatus(newest)
	}
	replica := newFailoverReplica(t, &seed)

	replica.observe(t, failoverRunningGeneration, verdictValid)
	generation := failoverRunningGeneration
	for _, kind := range later {
		generation++
		replica.observe(t, generation, kind)
	}
	replica.becomeLeader(t)

	// A later verdict marks the end of the takeover: the
	// updater handles events in order.
	next := generation + 1
	replica.observe(t, next, verdictValid)
	final := wantVerdict{metav1.ConditionTrue, reasonValidationSucceeded, next}
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		writes := statusWrites(replica.crd)
		if assert.NotEmpty(c, writes) {
			assert.Equal(c, next, writes[len(writes)-1].ObservedGeneration)
		}
	}, 10*time.Second, 20*time.Millisecond)

	writes := statusWrites(replica.crd)
	beforeNext := seed
	if len(writes) > 1 {
		beforeNext = writes[len(writes)-2]
	}
	requireVerdict(t, &beforeNext, newest)
	requireVerdict(t, &writes[len(writes)-1], final)

	observed := seed.ObservedGeneration
	for i, write := range writes {
		assert.GreaterOrEqual(t, write.ObservedGeneration, observed,
			"write %d moved observedGeneration back", i)
		observed = write.ObservedGeneration
	}
}

func TestConfigChangeHandler_BecameLeaderReplaysTheNewestVerdictFirst(t *testing.T) {
	replica := newFailoverReplica(t, nil)
	replica.observe(t, failoverRunningGeneration, verdictValid)
	replica.observe(t, failoverRunningGeneration+1, verdictInvalid)

	replica.bus.Publish(events.NewBecameLeaderEvent("new-leader"))

	replay := testutil.WaitForEvent[busevents.Event](t, replica.verdicts, testutil.LongTimeout)
	verdict, ok := replay.(*events.ConfigVerdictReplayedEvent)
	require.True(t, ok, "the newest verdict must be replayed first, got %T", replay)
	invalid, ok := verdict.Verdict.(*events.ConfigInvalidEvent)
	require.True(t, ok, "the newest verdict is the invalid generation's, got %T", verdict.Verdict)
	assert.Equal(t, failoverVersion(failoverRunningGeneration+1), invalid.Version)

	state := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, replica.verdicts, testutil.LongTimeout)
	assert.Equal(t, failoverVersion(failoverRunningGeneration), state.Version)
	assert.Less(t, state.VerdictSequence, invalid.VerdictSequence)
}

// The running config's state replay follows the newest verdict onto a new
// leader's updater, whatever generation each one judged.
func TestStatusUpdater_SkipsAVerdictOlderThanTheTermsNewest(t *testing.T) {
	running := failoverHTC(failoverRunningGeneration)
	newer := failoverHTC(failoverRunningGeneration + 1)
	invalidErrors := map[string][]string{"template": {"undefined: cidr_partition"}}
	inconclusiveReasons := map[string][]string{"template": {schemaTimeoutReason}}
	tests := []struct {
		name string
		// arrival builds the verdicts in the order the handler reached them
		// and returns them in the order the updater receives them.
		arrival func() []busevents.Event
		want    wantVerdict
	}{
		{
			name: "running config replayed after a newer generation's verdict",
			arrival: func() []busevents.Event {
				state := validatedVerdict(running, running.ResourceVersion)
				newest := invalidVerdict(newer.ResourceVersion, newer, invalidErrors)
				return []busevents.Event{events.NewConfigVerdictReplayedEvent(newest), state}
			},
			want: wantVerdict{metav1.ConditionFalse, reasonConfigInvalid, failoverRunningGeneration + 1},
		},
		{
			name: "running config replayed after a newer verdict on its own generation",
			arrival: func() []busevents.Event {
				state := validatedVerdict(running, running.ResourceVersion)
				newest := inconclusiveVerdict(running.ResourceVersion, running, inconclusiveReasons)
				return []busevents.Event{events.NewConfigVerdictReplayedEvent(newest), state}
			},
			want: wantVerdict{metav1.ConditionUnknown, reasonValidationInconclusive, failoverRunningGeneration},
		},
		{
			name: "an earlier verdict on the same generation arrives late",
			arrival: func() []busevents.Event {
				earlier := inconclusiveVerdict(newer.ResourceVersion, newer, inconclusiveReasons)
				newest := invalidVerdict(newer.ResourceVersion, newer, invalidErrors)
				return []busevents.Event{newest, earlier}
			},
			want: wantVerdict{metav1.ConditionFalse, reasonConfigInvalid, failoverRunningGeneration + 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, crd := newStatusUpdaterFixture(t, failoverHTC(failoverRunningGeneration+1))
			u.ctx = t.Context()
			for _, event := range tt.arrival() {
				u.HandleEvent(event)
			}
			status := getStatus(t, crd)
			requireVerdict(t, &status, tt.want)
		})
	}
}

func TestStatusUpdater_WritesAStaleVerdictToNoSource(t *testing.T) {
	library, config := shard("haptic-config-base", 5), shard("haptic-config", 2)
	newest := wantVerdict{metav1.ConditionFalse, reasonConfigInvalid, 5}
	library.Status = verdictStatus(newest)
	config.Status = verdictStatus(wantVerdict{metav1.ConditionFalse, reasonConfigInvalid, 2})
	u, crd := newStatusUpdaterFixture(t, library, config)

	stale := validatedVerdict(config, "v9")
	stale.Sources = []events.ConfigSourceRef{
		{Namespace: testNamespace, Name: library.Name, Generation: 4},
		{Namespace: testNamespace, Name: config.Name, Generation: config.Generation},
	}
	u.handleConfigValidated(t.Context(), stale)

	assert.Empty(t, statusWrites(crd))
	libraryStatus := shardStatus(t, crd, library.Name)
	requireVerdict(t, &libraryStatus, newest)
}

func TestStatusUpdater_UnreadableSourcePreventsPartialVerdict(t *testing.T) {
	library, config := shard("haptic-config-base", 5), shard("haptic-config", 2)
	u, crd := newStatusUpdaterFixture(t, library, config)
	crd.PrependReactor("get", "haproxytemplateconfigs", func(action clienttesting.Action) (bool, k8sruntime.Object, error) {
		get, ok := action.(clienttesting.GetAction)
		if ok && get.GetName() == library.Name {
			return true, nil, errors.New("API unavailable")
		}
		return false, nil, nil
	})
	verdict := validatedVerdict(config, "v9")
	verdict.Sources = []events.ConfigSourceRef{
		{Namespace: testNamespace, Name: config.Name, Generation: config.Generation},
		{Namespace: testNamespace, Name: library.Name, Generation: library.Generation},
	}

	u.handleConfigValidated(t.Context(), verdict)

	assert.Empty(t, statusWrites(crd))
}

func TestStatusUpdater_RenderVerdictLeavesANewerConfigVerdict(t *testing.T) {
	newer := failoverHTC(failoverRunningGeneration + 1)
	tests := []struct {
		name   string
		newest func() busevents.Event
		want   wantVerdict
	}{
		{
			name: "invalid",
			newest: func() busevents.Event {
				return invalidVerdict(newer.ResourceVersion, newer, map[string][]string{"template": {"boom"}})
			},
			want: wantVerdict{metav1.ConditionFalse, reasonConfigInvalid, failoverRunningGeneration + 1},
		},
		{
			name: "inconclusive",
			newest: func() busevents.Event {
				return inconclusiveVerdict(newer.ResourceVersion, newer,
					map[string][]string{"template": {schemaTimeoutReason}})
			},
			want: wantVerdict{metav1.ConditionUnknown, reasonValidationInconclusive, failoverRunningGeneration + 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, crd := newStatusUpdaterFixture(t, newer)
			u.ctx = t.Context()
			running := failoverHTC(failoverRunningGeneration)
			u.HandleEvent(validatedVerdict(running, running.ResourceVersion))
			u.HandleEvent(tt.newest())

			u.HandleEvent(events.NewValidationFailedEvent([]string{"[ALERT] unknown keyword 'bogus'"}, 1, "reconcile"))
			status := getStatus(t, crd)
			requireVerdict(t, &status, tt.want)

			u.HandleEvent(events.NewRenderGateCompletedEvent("plan-2", true, false, true, "", false, 1))
			status = getStatus(t, crd)
			requireVerdict(t, &status, tt.want)
		})
	}
}

func TestStatusUpdater_QueuesNothingOutsideATerm(t *testing.T) {
	htc := failoverHTC(failoverRunningGeneration)
	u, crd := newStatusUpdaterFixture(t, htc)
	bus := u.EventBus()
	bus.Start()
	var newest *events.ConfigInvalidEvent
	for i := range 2 * StatusUpdaterEventBufferSize {
		newest = invalidVerdict(htc.ResourceVersion, htc, map[string][]string{"template": {fmt.Sprintf("error %d", i)}})
		bus.Publish(newest)
	}
	assert.Zero(t, bus.DroppedEventsCritical())

	startStatusUpdater(t, u)
	bus.Publish(events.NewConfigVerdictReplayedEvent(newest))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, newest.ValidationErrors["template"], getStatus(t, crd).ValidationErrors)
	}, 10*time.Second, 20*time.Millisecond)
	assert.Len(t, statusWrites(crd), 1)
}

// Events a term received but did not handle before it ended are as stale as a
// follower's: the next term starts from its verdict replay.
func TestStatusUpdater_NewTermDiscardsThePreviousTermsQueue(t *testing.T) {
	htc := failoverHTC(failoverRunningGeneration)
	u, crd := newStatusUpdaterFixture(t, htc)
	bus := u.EventBus()
	bus.Start()
	u.leading.Store(true)
	bus.Publish(invalidVerdict(htc.ResourceVersion, htc, map[string][]string{"template": {"boom"}}))
	u.leading.Store(false)

	startStatusUpdater(t, u)
	bus.Publish(events.NewConfigVerdictReplayedEvent(validatedVerdict(htc, htc.ResourceVersion)))
	require.Eventually(t, func() bool {
		return getStatus(t, crd).ValidationStatus == statusValid
	}, 10*time.Second, 20*time.Millisecond)
	assert.Len(t, statusWrites(crd), 1, "the previous term's verdict was written")
}
