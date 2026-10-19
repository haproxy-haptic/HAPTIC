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
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	leaderelectionctrl "github.com/haproxy-haptic/HAPTIC/pkg/controller/leaderelection"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	k8sleaderelection "github.com/haproxy-haptic/HAPTIC/pkg/k8s/leaderelection"
	"github.com/haproxy-haptic/HAPTIC/pkg/lifecycle"
)

// leaderOnlyComponents holds components that only the leader should run.
type leaderOnlyComponents struct {
	cancel context.CancelFunc
	run    *lifecycle.ComponentRun
	drain  func(context.Context) error
	// drained runs the drain once: shutdown and the successor's retire both drain the term.
	drained sync.Once
}

// leadershipDrainTimeout bounds how long a graceful stop waits for the term's
// in-flight deployment and checkpoint before it releases the Lease.
const leadershipDrainTimeout = 15 * time.Second

// startReconciliationComponents starts all-replica reconciliation components using the lifecycle registry.
// Leader-only components (Deployer, DeploymentScheduler, ConfigPublisher) are NOT started here.
func startReconciliationComponents(
	iterCtx context.Context,
	registry *lifecycle.Registry,
	logger *slog.Logger,
	cancelCause context.CancelCauseFunc,
	errGroup *errgroup.Group,
) {
	// Start all-replica components using the registry (tracked by errgroup for graceful shutdown)
	// The registry handles concurrent startup and error propagation
	startInErrGroup(errGroup, iterCtx, logger, cancelCause, "reconciliation component", func(ctx context.Context) error {
		return registry.StartAll(ctx, false)
	})

	logger.Info("Reconciliation components started via lifecycle registry (all replicas)",
		"component_count", registry.Count())
}

// startLeaderOnlyComponents starts components that only the leader should run using the lifecycle registry.
// Returns a leaderOnlyComponents struct with cancellation control.
//
// IMPORTANT: This function blocks until all leader-only components have completed their event
// subscription. This ensures the EventBus Pause/Start pattern works correctly - the leaderelection
// callback can safely call eventBus.Start() after this function returns, knowing all leader-only
// components are ready to receive replayed events.
func startLeaderOnlyComponents(
	parentCtx context.Context,
	registry *lifecycle.Registry,
	logger *slog.Logger,
	parentCancel context.CancelCauseFunc,
	errGroup *errgroup.Group,
) (*leaderOnlyComponents, error) {
	// Create separate context for leader-only components
	leaderCtx, leaderCancel := context.WithCancel(parentCtx)

	// Wait for every subscription before the election adapter replays buffered events.
	run, startErr := registry.StartLeaderOnly(leaderCtx)
	components := &leaderOnlyComponents{cancel: leaderCancel, run: run, drain: func(ctx context.Context) error {
		if err := leaderCtx.Err(); err != nil {
			return err
		}
		drainCtx, cancel := context.WithCancel(leaderCtx)
		defer cancel()
		defer context.AfterFunc(ctx, cancel)()
		return registry.DrainLeaderOnly(drainCtx)
	}}
	startupFailed := startErr != nil && parentCtx.Err() == nil

	errGroup.Go(func() error {
		err := run.Wait()
		if err != nil && (startupFailed || leaderCtx.Err() == nil) {
			logger.Error("Leader-only component failed", "error", err)
			parentCancel(err)
			return err
		}
		return nil
	})

	logger.Debug("Leader-only components started via lifecycle registry",
		"components", "Coordinator, DriftMonitor, Deployer, DeploymentScheduler, ConfigPublisher, StatusUpdater")

	return components, startErr
}

// stopLeaderOnlyComponents stops leader-only components gracefully.
func stopLeaderOnlyComponents(components *leaderOnlyComponents, logger *slog.Logger) {
	if components == nil {
		return
	}

	logger.Info("Stopping leader-only components")
	if components.cancel != nil {
		components.cancel()
	}
	if components.run != nil {
		_ = components.run.Wait()
	}
	logger.Info("Leader-only components stopped")
}

// leaderCallbackDeps holds dependencies for leader election callbacks.
// Extracting these to a struct makes the dependencies explicit rather than
// relying on closure scope, improving code clarity and testability.
type leaderCallbackDeps struct {
	registry    *lifecycle.Registry
	logger      *slog.Logger
	cancelCause context.CancelCauseFunc
	podName     string
	errGroup    *errgroup.Group
}

// leaderCallbackState holds mutable state shared across leader callbacks.
// This state is protected by a mutex since callbacks may be invoked concurrently.
type leaderCallbackState struct {
	mu         sync.Mutex
	components *leaderOnlyComponents
	stopped    bool
	retiring   bool
	// leading is readable without mu, which OnStartedLeading holds for as
	// long as the leader-only components take to start.
	leading atomic.Bool
}

// retire marks the coming loss of the Lease as the planned hand-over to the
// successor iteration, so it is not reported as a lost election.
func (s *leaderCallbackState) retire() {
	s.leading.Store(false)
	s.mu.Lock()
	s.retiring = true
	s.mu.Unlock()
}

// isLeading reports whether this iteration's leadership term is running.
func (s *leaderCallbackState) isLeading() bool {
	return s != nil && s.leading.Load()
}

func (s *leaderCallbackState) isRetiring() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retiring
}

func (s *leaderCallbackState) take() *leaderOnlyComponents {
	s.leading.Store(false)
	s.mu.Lock()
	defer s.mu.Unlock()
	components := s.components
	s.components = nil
	return components
}

// drain settles the running term's deployment and checkpoint before a
// graceful stop releases the Lease. It runs under the term's context, which
// client-go cancels when renewal fails, so a replica that lost the Lease
// publishes nothing.
func (s *leaderCallbackState) drain(logger *slog.Logger) {
	if !s.isLeading() {
		return
	}
	s.mu.Lock()
	components := s.components
	s.mu.Unlock()
	if components == nil || components.drain == nil {
		return
	}
	components.drained.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), leadershipDrainTimeout)
		defer cancel()
		if err := components.drain(ctx); err != nil {
			logger.Warn("Leadership term ends without its latest checkpoint; replacement pods cannot use retained configuration until the next deployment is acknowledged",
				"error", err)
			return
		}
		logger.Info("Leadership term drained")
	})
}

// stop ends one leadership term. It does not latch: the same replica may
// re-acquire the lease and start the components again.
func (s *leaderCallbackState) stop(logger *slog.Logger) {
	stopLeaderOnlyComponents(s.take(), logger)
}

// cancel is the iteration teardown: no term starts after it.
func (s *leaderCallbackState) cancel() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	components := s.take()
	if components != nil && components.cancel != nil {
		components.cancel()
	}
}

// makeLeaderCallbacks creates leader election callbacks with explicit dependencies.
// The returned state struct allows the caller to access leader component state.
func makeLeaderCallbacks(ctx context.Context, deps leaderCallbackDeps) (k8sleaderelection.Callbacks, *leaderCallbackState) {
	state := &leaderCallbackState{}

	callbacks := k8sleaderelection.Callbacks{
		OnStartedLeading: func(ctx context.Context) {
			deps.logger.Debug("Became leader, starting deployment components")
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.stopped || ctx.Err() != nil {
				return
			}
			components, err := startLeaderOnlyComponents(
				ctx,
				deps.registry,
				deps.logger,
				deps.cancelCause,
				deps.errGroup,
			)
			state.components = components
			if err != nil && ctx.Err() == nil {
				deps.logger.Error("Failed to start leader-only components", "error", err)
				deps.cancelCause(err)
			}
			state.leading.Store(err == nil)
		},
		OnStoppedLeading: func() {
			if state.isRetiring() {
				deps.logger.Info("Leadership term ended for the successor iteration, stopping deployment components")
			} else if context.Cause(ctx) == context.Canceled {
				deps.logger.Info("Controller stopping, ending leadership term")
			} else {
				deps.logger.Warn("Lost leadership, stopping deployment components")
			}
			state.stop(deps.logger)
		},
		OnNewLeader: func(identity string) {
			deps.logger.Debug("New leader observed",
				"leader_identity", identity,
				"is_self", identity == deps.podName,
			)
		},
	}

	return callbacks, state
}

// electionRun is one iteration's election loop.
type electionRun struct {
	elector *leaderelectionctrl.Component
	cancel  context.CancelFunc
	done    chan struct{}
}

// retireLeadership ends this iteration's leadership for a successor on the
// same replica: the leader-only components stop, the election loop stops
// with the Lease kept, and no term starts here again. The successor's
// election then resumes the Lease on its first acquire.
func retireLeadership(setup *componentSetup, logger *slog.Logger) {
	if setup.LeaderState != nil {
		setup.LeaderState.drain(logger)
		setup.LeaderState.retire()
	}
	if setup.Election != nil {
		setup.Election.elector.KeepLeaseOnStop()
		setup.Election.cancel()
		<-setup.Election.done
	}
	if setup.LeaderState != nil {
		// The loop's end stopped the components through OnStoppedLeading;
		// with election disabled nothing else does, so stop them here, then
		// latch so no term starts again.
		setup.LeaderState.stop(logger)
		setup.LeaderState.cancel()
	}
	logger.Info("Leadership retired for the successor iteration")
}

// reacquireDelay paces re-entry into leader election after a lost lease.
// client-go's own acquire loop paces itself with RetryPeriod; this only keeps
// a pathological instant return from spinning.
const reacquireDelay = time.Second

// superviseElection keeps the leader-election loop alive for the whole
// iteration.
//
// client-go's LeaderElector.Run returns permanently once an acquired lease is
// lost (a renewal missed its deadline — e.g. the apiserver or this pod was
// starved for longer than renewDeadline, or another replica took the Lease).
// It does NOT re-enter the acquire loop; without supervision the replica
// would keep running its all-replica components with a dead elector — a
// permanent standby zombie (fatal on single-replica deployments, issue #57).
//
// A lost lease re-enters election here, in place. The iteration — its config,
// stores and webhook validators — survives: OnStoppedLeading already stopped
// the leader-only components, the lifecycle registry restarts the same
// instances from Stopped on re-acquisition, and a new term claims a fresh
// fencing epoch. Tearing the iteration down instead would retire the
// admission validators for the whole resync, denying admission for no reason.
//
// A nil return from start with the context still alive is exactly the
// lost-lease case: for a replica that never acquired the lease, Run blocks in
// the acquire loop until the context is cancelled. A non-nil error is a hard
// failure (the elector could not even be constructed) and stays
// iteration-fatal.
//
// restart cancels the current attempt only: the Lease is released
// (ReleaseOnCancel), OnStoppedLeading stops the leader-only components, and
// the loop re-enters election. It is how the deployer's fencing stand-down
// hands leadership to the newer leader it discovered. Nil disables it.
func superviseElection(ctx context.Context, start func(context.Context) error, restart <-chan struct{}, logger *slog.Logger) error {
	for {
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		go func() {
			select {
			case <-restart:
				logger.Warn("Standing down; releasing the Lease and re-entering election")
				cancelAttempt()
			case <-attemptCtx.Done():
			}
		}()
		err := start(attemptCtx)
		stoodDown := attemptCtx.Err() != nil
		cancelAttempt()
		select {
		case <-ctx.Done():
			// Normal teardown (shutdown or config-change reinitialization);
			// any error from the elector here is a cancellation artifact.
			return nil
		default:
		}
		if err != nil && !stoodDown {
			logger.Error("Leader election failed, triggering reinitialization to restart election", "error", err)
			return err
		}
		if !stoodDown {
			logger.Warn("Lease lost; re-entering leader election")
		}
		select {
		case <-time.After(reacquireDelay):
		case <-ctx.Done():
			return nil
		}
	}
}

// leaderIdentity resolves who this replica is to leader election, and in which
// namespace its Lease lives. The deployer's fencing epoch is claimed on that
// same Lease, so both sites must derive it identically.
func leaderIdentity(k8sClient *client.Client, logger *slog.Logger) (podName, podNamespace string) {
	podName = os.Getenv("POD_NAME")
	podNamespace = os.Getenv("POD_NAMESPACE")

	if podName == "" {
		logger.Warn("POD_NAME environment variable not set, using hostname as identity")
		hostname, err := os.Hostname()
		if err != nil {
			logger.Error("Failed to get hostname for leader election identity", "error", err)
		}
		podName = hostname
	}
	if podNamespace == "" {
		podNamespace = k8sClient.Namespace()
		logger.Debug("POD_NAMESPACE not set, using client namespace", "namespace", podNamespace)
	}
	return podName, podNamespace
}

// setupLeaderElection initializes leader election or starts leader-only components immediately.
//
// Returns leader callback state for lifecycle management. The state contains a mutex-protected
// pointer to the leader-only components, which is nil until leadership is acquired.
func setupLeaderElection(
	setup *componentSetup,
	cfg *coreconfig.Config,
	k8sClient *client.Client,
	logger *slog.Logger,
) (*leaderCallbackState, error) {
	if cfg.Controller.LeaderElection.Enabled {
		podName, podNamespace := leaderIdentity(k8sClient, logger)

		// Create pure leader election config
		leConfig := &k8sleaderelection.Config{
			Enabled:         true,
			Identity:        podName,
			LeaseName:       cfg.Controller.LeaderElection.LeaseName,
			LeaseNamespace:  podNamespace,
			LeaseDuration:   cfg.Controller.LeaderElection.GetLeaseDuration(),
			RenewDeadline:   cfg.Controller.LeaderElection.GetRenewDeadline(),
			RetryPeriod:     cfg.Controller.LeaderElection.GetRetryPeriod(),
			ReleaseOnCancel: true,
		}

		// Create callbacks with explicit dependencies
		callbacks, state := makeLeaderCallbacks(setup.IterCtx, leaderCallbackDeps{
			registry:    setup.Registry,
			logger:      logger,
			cancelCause: setup.CancelCause,
			podName:     podName,
			errGroup:    setup.ErrGroup,
		})

		// Create leader election component (event adapter). It claims the
		// fencing epoch the deployer stamps on every apply.
		elector, err := leaderelectionctrl.New(leConfig, k8sClient.Clientset(), setup.Bus, callbacks, setup.LeaderEpoch, logger)
		if err != nil {
			return nil, fmt.Errorf("creating leader elector: %w", err)
		}

		// The loop runs on its own child context so a hand-over can end this
		// term without cancelling the iteration; the errgroup still owns it.
		electionCtx, cancelElection := context.WithCancel(setup.IterCtx)
		run := &electionRun{elector: elector, cancel: cancelElection, done: make(chan struct{})}
		setup.Election = run
		setup.ErrGroup.Go(func() error {
			defer close(run.done)
			return superviseElection(electionCtx, elector.Start, setup.ElectionRestart, logger)
		})

		logger.Info("Leader election initialized", "identity", podName, "lease_name", leConfig.LeaseName, "lease_namespace", leConfig.LeaseNamespace)
		return state, nil
	}

	// Leader election disabled - start leader-only components immediately.
	// Use the same Pause/Start pattern as leaderelection/component.go to ensure
	// leader-only components subscribe before any buffered events are replayed.
	logger.Info("Leader election disabled, starting all components")
	setup.Bus.Pause()
	setup.Bus.Publish(events.NewBecameLeaderEvent("standalone"))
	state := &leaderCallbackState{}
	components, err := startLeaderOnlyComponents(setup.IterCtx, setup.Registry, logger, setup.CancelCause, setup.ErrGroup)
	state.components = components
	if err != nil {
		return state, fmt.Errorf("starting leader-only components: %w", err)
	}
	state.leading.Store(true)
	if err := startEventBus(setup); err != nil {
		return state, err
	}
	return state, nil
}
