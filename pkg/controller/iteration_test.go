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

package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/configchange"
	controllerevents "github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	leaderelectionctrl "github.com/haproxy-haptic/HAPTIC/pkg/controller/leaderelection"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	k8sleaderelection "github.com/haproxy-haptic/HAPTIC/pkg/k8s/leaderelection"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestLoadIterationBundleUsesValidatedSnapshotWithoutLiveRefetch(t *testing.T) {
	raw := &coreconfig.Config{WatchedResources: map[string]coreconfig.WatchedResource{
		"routes": {APIVersions: []string{"example.io/v2", "example.io/v1"}},
	}}
	effective := &coreconfig.Config{WatchedResources: map[string]coreconfig.WatchedResource{
		"routes": {APIVersion: "example.io/v1"},
	}}
	credentials := &coreconfig.Credentials{DataplaneUsername: "snapshot"}
	crd := &v1alpha1.HAProxyTemplateConfig{}
	sources := []controllerevents.ConfigSourceRef{{Name: "config", Generation: 7}}
	request := &configchange.ReloadRequest{Snapshot: &configchange.ValidatedSnapshot{
		RawConfig:          raw,
		Config:             effective,
		Resolution:         &coreconfig.Resolution{ResolvedVersions: map[string]string{"routes": "example.io/v1"}},
		TemplateConfig:     crd,
		ConfigVersion:      "validated-b",
		Credentials:        credentials,
		CredentialsVersion: "secret-b",
		Sources:            sources,
	}}

	// A nil client would panic on a live fetch. The accepted handoff must make
	// that path unreachable even if the apiserver has already advanced to C.
	bundle, err := loadIterationBundle(t.Context(), nil, "live-c", "secret-c", nil, request, nil)
	require.NoError(t, err)
	assert.Same(t, raw, bundle.Config)
	assert.Same(t, crd, bundle.CRD)
	assert.Same(t, credentials, bundle.Credentials)
	assert.Equal(t, "validated-b", bundle.ConfigVersion)
	assert.Equal(t, "secret-b", bundle.CredentialsVersion)
	assert.Equal(t, sources, bundle.Sources)

	selected, resolution, alreadyValidated, err := effectiveConfigForIteration(t.Context(), request, raw, nil, nil)
	require.NoError(t, err)
	assert.True(t, alreadyValidated)
	assert.Same(t, effective, selected)
	assert.Same(t, request.Snapshot.Resolution, resolution)
}

func TestBuildAndRegisterPluggableValidatorManagerRejectsMalformedGlob(t *testing.T) {
	setup := &componentSetup{}
	cfg := &coreconfig.Config{
		Validators: []coreconfig.ValidatorConfig{{
			Name:       "spoa-hub",
			SocketPath: "/var/run/haptic-validators/spoa-hub.sock",
			Files:      []string{"general/[broken"},
		}},
	}

	mgr, cleanup, err := buildAndRegisterPluggableValidatorManager(setup, cfg, nil)
	require.Error(t, err)
	assert.Nil(t, mgr)
	assert.Nil(t, cleanup)
	assert.ErrorContains(t, err, "invalid file glob")
}

func TestFinishIterationStartupRejectsCanceledIteration(t *testing.T) {
	iterCtx, cancelCause := context.WithCancelCause(t.Context())
	failure := errors.New("required component failed")
	cancelCause(failure)
	state := &configState{}
	infra := &persistentInfra{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	authority := newIterationReloadAuthority()
	err := finishIterationStartup(&componentSetup{IterCtx: iterCtx}, state, infra, authority, nil, nil, logger)
	require.ErrorIs(t, err, failure)
	assert.False(t, state.IsInitialized())
	assert.False(t, authority.Serving())
	infra.graceMu.Lock()
	assert.False(t, infra.iterationInitialized)
	infra.graceMu.Unlock()
}

func TestWaitForIterationExitReturnsCancellationCause(t *testing.T) {
	iterCtx, cancelCause := context.WithCancelCause(t.Context())
	failure := errors.New("required component failed")
	cancelCause(failure)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	_, err := waitForIterationExit(
		&componentSetup{IterCtx: iterCtx}, newIterationReloadAuthority(), logger)
	require.ErrorIs(t, err, failure)
}

func TestCRDDisappearanceReloadInterruptsStartupWatcherSync(t *testing.T) {
	parent, cancelCause := context.WithCancelCause(t.Context())
	group, iterCtx := errgroup.WithContext(parent)
	setup := &componentSetup{
		IterCtx:        iterCtx,
		Cancel:         func() { cancelCause(nil) },
		ConfigChangeCh: make(chan *configchange.ReloadRequest, 1),
		ErrGroup:       group,
	}
	authority := newIterationReloadAuthority()
	startIterationReloadObserver(setup, authority)

	startupExited := make(chan struct{})
	go func() {
		<-setup.IterCtx.Done()
		close(startupExited)
	}()
	request := &configchange.ReloadRequest{
		Snapshot: &configchange.ValidatedSnapshot{
			RawConfig:     &coreconfig.Config{},
			Config:        &coreconfig.Config{},
			ConfigVersion: "active",
		},
		Reasons: configchange.ReloadReasonEffectiveConfig,
	}
	setup.ConfigChangeCh <- request

	select {
	case <-startupExited:
	case <-time.After(time.Second):
		t.Fatal("CRD reload did not interrupt startup watcher synchronization")
	}
	assert.Same(t, request, authority.Latest())
	require.NoError(t, group.Wait())
}

func TestCompleteIterationPreservesCancellationCause(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deliveryErr := &criticalEventDeliveryError{drop: busevents.DropInfo{
		SubscriberName: "blocked",
		EventType:      "test.delivery",
	}}
	tests := []struct {
		name      string
		cause     error
		stageErr  error
		want      error
		wantTyped bool
	}{
		{name: "typed cause replaces generic cancellation", cause: deliveryErr, stageErr: context.Canceled, want: context.Canceled, wantTyped: true},
		{name: "ordinary cancellation remains ordinary", cause: context.Canceled, stageErr: context.Canceled, want: context.Canceled},
		{name: "unrelated stage error remains", stageErr: errors.New("stage failed")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent, cancelCause := context.WithCancelCause(t.Context())
			group, groupCtx := errgroup.WithContext(parent)
			setup := &componentSetup{
				IterCtx:     groupCtx,
				Cancel:      func() { cancelCause(nil) },
				CancelCause: cancelCause,
				ErrGroup:    group,
			}
			if test.cause != nil {
				cancelCause(test.cause)
			}

			stoppingCause := make(chan error, 1)
			group.Go(func() error {
				<-groupCtx.Done()
				stoppingCause <- context.Cause(groupCtx)
				return nil
			})
			err := completeIteration(setup, test.stageErr, logger)
			wantCause := test.cause
			if wantCause == nil {
				wantCause = test.stageErr
			}
			require.ErrorIs(t, <-stoppingCause, wantCause)
			if test.want != nil {
				require.ErrorIs(t, err, test.want)
			}
			if test.stageErr != nil {
				require.ErrorIs(t, err, test.stageErr)
			}
			var typed *criticalEventDeliveryError
			assert.Equal(t, test.wantTyped, errors.As(err, &typed))
		})
	}
}

func newTestLiveIteration(t *testing.T) *liveIteration {
	t.Helper()
	parent, cancelCause := context.WithCancelCause(t.Context())
	group, iterCtx := errgroup.WithContext(parent)
	setup := &componentSetup{
		IterCtx:  iterCtx,
		Cancel:   func() { cancelCause(nil) },
		ErrGroup: group,
	}
	authority := newIterationReloadAuthority()
	authority.MarkServing()
	return &liveIteration{
		setup:     setup,
		authority: authority,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func testReload(version string) *configchange.ReloadRequest {
	return &configchange.ReloadRequest{Snapshot: &configchange.ValidatedSnapshot{ConfigVersion: version}}
}

// The serving iteration keeps serving until its successor is up; a successor
// that fails to start leaves it in place, and one that started replaces it.
func TestIterationSequenceStepHandsOverOnlyToAStartedSuccessor(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	first := newTestLiveIteration(t)
	seq := &iterationSequence{logger: logger, current: first, startup: testReload("b")}

	startFailure := errors.New("startup failed")
	err := seq.step(func(startup *configchange.ReloadRequest, previous *liveIteration) (*liveIteration, error) {
		assert.Same(t, previous, first)
		assert.Equal(t, "b", startup.Snapshot.ConfigVersion)
		return nil, startFailure
	})
	require.ErrorIs(t, err, startFailure)
	assert.Same(t, first, seq.current, "a failed start must leave the serving iteration in place")
	require.NoError(t, first.setup.IterCtx.Err(), "the serving iteration must not be torn down")
	assert.Nil(t, seq.startup, "the retry must load live state, not the consumed hand-off")

	second := newTestLiveIteration(t)
	second.authority.Record(testReload("c"))
	err = seq.step(func(startup *configchange.ReloadRequest, previous *liveIteration) (*liveIteration, error) {
		assert.Nil(t, startup)
		assert.Same(t, previous, first)
		return second, nil
	})
	require.NoError(t, err)
	require.Error(t, first.setup.IterCtx.Err(), "the predecessor is torn down once the successor serves")
	assert.Same(t, second, seq.current)
	require.NoError(t, second.setup.IterCtx.Err(), "a reload leaves the iteration serving for the next hand-over")
	assert.Equal(t, "c", seq.startup.Snapshot.ConfigVersion)
}

// A successor whose start fails after it already took leadership over cannot
// leave the predecessor serving: neither leads, so both go and the retry
// starts cold.
func TestIterationSequenceStepTearsDownARetiredPredecessorOnFailure(t *testing.T) {
	first := newTestLiveIteration(t)
	first.retired = true
	seq := &iterationSequence{logger: first.logger, current: first}

	startFailure := errors.New("startup failed")
	err := seq.step(func(*configchange.ReloadRequest, *liveIteration) (*liveIteration, error) {
		return nil, startFailure
	})
	require.ErrorIs(t, err, startFailure)
	assert.Nil(t, seq.current)
	require.Error(t, first.setup.IterCtx.Err())
}

// The Lease a retired predecessor kept for a successor that then failed to
// start is released, so a standby replica need not wait for it to expire;
// otherwise a shutdown during the hand-over leaves the fleet leaderless (#249).
func TestIterationSequenceStepReleasesTheLeaseKeptForAFailedSuccessor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		startErr error
	}{
		{name: "failure", startErr: context.Canceled},
		// The restart begins cold with no predecessor, so keeping the Lease
		// would leave every replica leaderless for its whole startup.
		{name: "interrupting reload", startErr: &startupInterruptedError{reload: testReload("d")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkKeptLeaseReleasedOnFailedStart(t, tc.startErr)
		})
	}
}

func checkKeptLeaseReleasedOnFailedStart(t *testing.T, startErr error) {
	t.Helper()
	const identity = "controller-0"
	holder := identity
	clientset := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "haptic-leader", Namespace: "haptic"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	})
	elector, err := leaderelectionctrl.New(&k8sleaderelection.Config{
		Enabled: true, Identity: identity, LeaseName: "haptic-leader", LeaseNamespace: "haptic",
		LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
		ReleaseOnCancel: true,
	}, clientset, busevents.NewEventBus(1), k8sleaderelection.Callbacks{}, nil, nil)
	require.NoError(t, err)

	first := newTestLiveIteration(t)
	first.retired = true
	first.setup.Election = &electionRun{elector: elector}
	seq := &iterationSequence{logger: first.logger, current: first}

	err = seq.step(func(*configchange.ReloadRequest, *liveIteration) (*liveIteration, error) {
		return nil, startErr
	})
	var interrupted *startupInterruptedError
	if errors.As(startErr, &interrupted) {
		require.NoError(t, err)
	} else {
		require.ErrorIs(t, err, startErr)
	}
	assert.Nil(t, seq.current)
	lease, err := clientset.CoordinationV1().Leases("haptic").Get(t.Context(), "haptic-leader", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, lease.Spec.HolderIdentity)
}

// A reload that interrupts a startup restarts at once from that reload, with
// the predecessor still serving.
func TestIterationSequenceStepRestartsFromAnInterruptingReload(t *testing.T) {
	first := newTestLiveIteration(t)
	seq := &iterationSequence{logger: first.logger, current: first}

	err := seq.step(func(*configchange.ReloadRequest, *liveIteration) (*liveIteration, error) {
		return nil, &startupInterruptedError{reload: testReload("d")}
	})
	require.NoError(t, err)
	assert.Same(t, first, seq.current)
	require.NoError(t, first.setup.IterCtx.Err())
	assert.Equal(t, "d", seq.startup.Snapshot.ConfigVersion)
}

// An iteration that ends without a reload is torn down and its cause surfaces.
func TestIterationSequenceStepReportsAnIterationThatEnds(t *testing.T) {
	next := newTestLiveIteration(t)
	seq := &iterationSequence{logger: next.logger}
	failure := errors.New("required component failed")
	next.setup.ErrGroup.Go(func() error { return failure })

	err := seq.step(func(*configchange.ReloadRequest, *liveIteration) (*liveIteration, error) {
		return next, nil
	})
	require.ErrorIs(t, err, failure)
	assert.Nil(t, seq.current)
}

// Once the iteration serves, a reload is recorded for the hand-over and the
// iteration keeps running; only a reload during startup cancels it.
func TestReloadObserverKeepsAServingIterationRunning(t *testing.T) {
	parent, cancelCause := context.WithCancelCause(t.Context())
	group, iterCtx := errgroup.WithContext(parent)
	setup := &componentSetup{
		IterCtx:        iterCtx,
		Cancel:         func() { cancelCause(nil) },
		ConfigChangeCh: make(chan *configchange.ReloadRequest, 1),
		ErrGroup:       group,
	}
	authority := newIterationReloadAuthority()
	authority.MarkServing()
	startIterationReloadObserver(setup, authority)

	setup.ConfigChangeCh <- testReload("f")
	select {
	case <-authority.Ready():
	case <-time.After(time.Second):
		t.Fatal("the reload was not recorded")
	}
	assert.Equal(t, "f", authority.Latest().Snapshot.ConfigVersion)
	require.NoError(t, setup.IterCtx.Err(), "a serving iteration must not be cancelled by a reload")
	cancelCause(nil)
	require.NoError(t, group.Wait())
}

// A follower leaves the election as soon as its successor is being prepared;
// a leader keeps campaigning so the fleet sees no vacancy.
func TestAwaitReloadLeavesElectionOnlyAsFollower(t *testing.T) {
	for _, leading := range []bool{false, true} {
		t.Run("leading="+strconv.FormatBool(leading), func(t *testing.T) {
			it := newTestLiveIteration(t)
			electionCtx, cancelElection := context.WithCancel(it.setup.IterCtx)
			run := &electionRun{cancel: cancelElection, done: make(chan struct{})}
			go func() {
				<-electionCtx.Done()
				close(run.done)
			}()
			it.setup.Election = run
			it.setup.LeaderState = &leaderCallbackState{}
			it.setup.LeaderState.leading.Store(leading)

			it.authority.Record(testReload("b"))
			reload, err := it.awaitReload()
			require.NoError(t, err)
			require.Equal(t, "b", reload.Snapshot.ConfigVersion)
			assert.Equal(t, !leading, electionCtx.Err() != nil)
			cancelElection()
		})
	}
}

// A successor that fails to start leaves the follower that stood down for it
// campaigning again; one interrupted by a newer reload keeps it out.
func TestIterationSequenceStepResumesElectionAfterFailedStart(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run("interrupted="+strconv.FormatBool(interrupted), func(t *testing.T) {
			first := newTestLiveIteration(t)
			var launches int
			run := &electionRun{launch: func() (context.CancelFunc, chan struct{}) {
				launches++
				done := make(chan struct{})
				return func() {
					select {
					case <-done:
					default:
						close(done)
					}
				}, done
			}}
			run.cancel, run.done = run.launch()
			first.setup.Election = run
			first.setup.LeaderState = &leaderCallbackState{}
			first.authority.Record(testReload("b"))
			_, err := first.awaitReload()
			require.NoError(t, err)
			require.True(t, first.setup.LeaderState.superseded)

			startErr := errors.New("start failed")
			if interrupted {
				startErr = &startupInterruptedError{reload: testReload("c")}
			}
			seq := &iterationSequence{logger: first.logger, current: first}
			_ = seq.step(func(*configchange.ReloadRequest, *liveIteration) (*liveIteration, error) {
				return nil, startErr
			})
			assert.Equal(t, interrupted, first.setup.LeaderState.superseded)
			assert.Equal(t, map[bool]int{false: 2, true: 1}[interrupted], launches)
			run.stop()
		})
	}
}
