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

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	ctrlconfigpublisher "github.com/haproxy-haptic/HAPTIC/pkg/controller/configpublisher"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/deployer"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	leaderelectionctrl "github.com/haproxy-haptic/HAPTIC/pkg/controller/leaderelection"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercycle"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/agenttest"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	crdfake "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
	"github.com/haproxy-haptic/HAPTIC/pkg/lifecycle"
)

const (
	drainLeaseName      = "haptic-leader"
	drainNamespace      = "haptic"
	drainLeader         = "leader-a"
	drainSuccessor      = "leader-b"
	drainPublishWindow  = 10 * time.Second
	drainEventTimeout   = 10 * time.Second
	drainPollInterval   = 10 * time.Millisecond
	drainWaitForPublish = 5 * time.Second
)

type acceptingChecker struct{}

func (acceptingChecker) CheckOutput(context.Context, *renderoutput.Snapshot, string) error {
	return nil
}

// leaderTermHarness runs one leadership term with the production deployer,
// config publisher, pure publisher and Lease fencing against fake API servers
// and a fake HAProxy agent.
type leaderTermHarness struct {
	kube      *k8sfake.Clientset
	crd       *crdfake.Clientset
	publisher *configpublisher.Publisher
	template  *v1alpha1.HAProxyTemplateConfig
	bus       *busevents.EventBus
	completed <-chan busevents.Event
	agent     *agenttest.Agent
	fixture   *testutil.RenderCycleFixture
	state     *leaderCallbackState
	deployed  []string
}

func newLeaderTermHarness(t *testing.T) *leaderTermHarness {
	t.Helper()
	logger := testutil.NewTestLogger()
	kube := k8sfake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: drainLeaseName, Namespace: drainNamespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: ptr.To(drainLeader)},
	})
	epoch := leaderelectionctrl.NewLeaseEpoch(kube, drainNamespace, drainLeaseName, drainLeader, logger)
	require.NoError(t, epoch.Bump(t.Context()))

	crd := crdfake.NewSimpleClientset()
	var uids atomic.Uint64
	crd.PrependReactor("create", "haproxycfgs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		object := action.(k8stesting.CreateAction).GetObject().(*v1alpha1.HAProxyCfg)
		if object.UID == "" {
			object.UID = types.UID(fmt.Sprintf("runtime-uid-%d", uids.Add(1)))
		}
		return false, nil, nil
	})
	publisher := configpublisher.NewWithListers(kube, crd, nil, logger)
	template := &v1alpha1.HAProxyTemplateConfig{ObjectMeta: metav1.ObjectMeta{
		Name: "haptic", Namespace: drainNamespace, UID: "template-uid",
	}}

	bus := testutil.NewTestBus()
	completed := bus.SubscribeTypes("drain-test", 100, events.EventTypeDeploymentCompleted)
	bus.Start()

	domainMetrics := metrics.NewMetrics(prometheus.NewRegistry())
	stack := deployer.NewDeployStack(bus, &coreconfig.Config{}, logger, domainMetrics, nil,
		leaderelectionctrl.NewTerm(epoch, func(string) {}), nil)
	stack.EnableRetainedRecovery(publisher, acceptingChecker{}, template, domainMetrics, drainLeaseName)
	publisherComponent := ctrlconfigpublisher.New(publisher, bus, logger,
		ctrlconfigpublisher.WithPublishInterval(drainPublishWindow),
		ctrlconfigpublisher.WithTemplateConfig(template))

	registry := lifecycle.NewRegistry().WithLogger(logger)
	registerLifecycleComponents(registry, nil, []lifecycle.Component{stack.Deployer, publisherComponent})
	ctx, cancel := context.WithCancel(context.Background())
	group, _ := errgroup.WithContext(ctx)
	components, err := startLeaderOnlyComponents(ctx, registry, logger, func(error) {}, group)
	require.NoError(t, err)
	state := &leaderCallbackState{components: components}
	state.leading.Store(true)
	t.Cleanup(func() {
		state.stop(logger)
		cancel()
		require.NoError(t, group.Wait())
	})

	return &leaderTermHarness{
		kube: kube, crd: crd, publisher: publisher, template: template, bus: bus,
		completed: completed, agent: agenttest.New(t), fixture: testutil.NewRenderCycleFixture(t),
		state: state,
	}
}

func (h *leaderTermHarness) deploy(t *testing.T, config string) {
	t.Helper()
	snapshot := h.fixture.Snapshot(t, config, nil, nil)
	occurrence, err := rendercycle.NewOccurrence(snapshot)
	require.NoError(t, err)
	checksum, err := snapshot.ContentChecksum()
	require.NoError(t, err)
	endpoint := dataplane.Endpoint{
		URL: h.agent.URL(), Username: h.agent.Username(), Password: h.agent.Password(),
		PodName: "haproxy-0", PodNamespace: drainNamespace, PodUID: "haproxy-0-uid", PodRuntimeID: "runtime-0",
	}
	scheduled, err := events.NewDeploymentScheduledEventWithCycle(occurrence, []dataplane.Endpoint{endpoint},
		configpublisher.GenerateRuntimeConfigName(h.template.Name), drainNamespace, "config_validation", false)
	require.NoError(t, err)
	h.bus.Publish(scheduled)
	result := testutil.WaitForEvent[*events.DeploymentCompletedEvent](t, h.completed, drainEventTimeout)
	require.Equal(t, 1, result.Succeeded)
	h.deployed = append(h.deployed, checksum)
}

func (h *leaderTermHarness) retainedChecksums(t *testing.T) []string {
	t.Helper()
	current, err := h.crd.HaproxyTemplateICV1alpha1().HAProxyCfgs(drainNamespace).Get(
		t.Context(), configpublisher.GenerateRuntimeConfigName(h.template.Name), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	checksums := make([]string, 0, len(current.Status.RetainedConfigs))
	for _, ref := range current.Status.RetainedConfigs {
		checksums = append(checksums, ref.Checksum)
	}
	return checksums
}

func (h *leaderTermHarness) waitRetained(t *testing.T, checksum string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, retained := range h.retainedChecksums(t) {
			if retained == checksum {
				return true
			}
		}
		return false
	}, drainWaitForPublish, drainPollInterval)
}

// takeOver moves the Lease to another replica, which claims a newer epoch and
// the deployment authority the way a new leader's term does.
func (h *leaderTermHarness) takeOver(t *testing.T) *configpublisher.DeploymentAuthority {
	t.Helper()
	leases := h.kube.CoordinationV1().Leases(drainNamespace)
	lease, err := leases.Get(t.Context(), drainLeaseName, metav1.GetOptions{})
	require.NoError(t, err)
	lease.Spec.HolderIdentity = ptr.To(drainSuccessor)
	_, err = leases.Update(t.Context(), lease, metav1.UpdateOptions{})
	require.NoError(t, err)
	epoch := leaderelectionctrl.NewLeaseEpoch(h.kube, drainNamespace, drainLeaseName, drainSuccessor, testutil.NewTestLogger())
	require.NoError(t, epoch.Bump(t.Context()))
	authority := &configpublisher.DeploymentAuthority{
		Namespace: drainNamespace, Name: h.template.Name, UID: h.template.UID,
		Epoch: epoch.LeaderEpoch(), Claim: "successor", LeaseName: drainLeaseName, Identity: drainSuccessor,
	}
	require.NoError(t, h.publisher.ClaimDeploymentAuthority(t.Context(), authority))
	return authority
}

// recoverable applies the recovery's selection rule: a retained checkpoint is
// usable only when it matches the latest deployment intent.
func (h *leaderTermHarness) recoverable(t *testing.T, authority *configpublisher.DeploymentAuthority) (string, error) {
	t.Helper()
	configs, err := h.publisher.LoadRetained(t.Context(), drainNamespace, h.template.Name, h.template.UID)
	if err != nil {
		return "", err
	}
	refusal := errors.New("no retained checkpoint")
	for i := range configs {
		planID, err := configs[i].Output.PlanID()
		require.NoError(t, err)
		if refusal = h.publisher.CheckDeploymentIntent(t.Context(), authority, planID, configs[i].Reference.Checksum); refusal == nil {
			return configs[i].Reference.Checksum, nil
		}
	}
	return "", refusal
}

func TestGracefulLeaderStopLeavesTheNextLeaderAUsableCheckpoint(t *testing.T) {
	h := newLeaderTermHarness(t)
	h.deploy(t, "global\n  maxconn 100\n")
	h.waitRetained(t, h.deployed[0])
	h.deploy(t, "global\n  maxconn 200\n")

	h.state.drain(testutil.NewTestLogger())
	h.state.stop(testutil.NewTestLogger())

	checksum, err := h.recoverable(t, h.takeOver(t))
	require.NoError(t, err, "the next leader must find the checkpoint the deployment intent names")
	require.Equal(t, h.deployed[1], checksum)
}

func TestDrainPublishesNothingAfterAnotherReplicaTookTheLease(t *testing.T) {
	h := newLeaderTermHarness(t)
	h.deploy(t, "global\n  maxconn 100\n")
	h.waitRetained(t, h.deployed[0])
	h.deploy(t, "global\n  maxconn 200\n")
	h.takeOver(t)
	writes := 0
	h.crd.PrependReactor("*", "haproxycfgs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetVerb() != "get" && action.GetVerb() != "list" && action.GetVerb() != "watch" {
			writes++
		}
		return false, nil, nil
	})

	h.state.drain(testutil.NewTestLogger())
	h.state.stop(testutil.NewTestLogger())

	require.Zero(t, writes, "a replica that lost the Lease must not publish")
	require.Equal(t, []string{h.deployed[0]}, h.retainedChecksums(t))
}

func TestDrainIsANoOpOnceTheTermContextEnded(t *testing.T) {
	registry := lifecycle.NewRegistry()
	drainer := &recordingDrainer{ready: make(chan struct{})}
	registry.Register(drainer, true)
	ctx, cancel := context.WithCancel(context.Background())
	group, _ := errgroup.WithContext(ctx)
	components, err := startLeaderOnlyComponents(ctx, registry, testutil.NewTestLogger(), func(error) {}, group)
	require.NoError(t, err)
	state := &leaderCallbackState{components: components}
	state.leading.Store(true)

	cancel()
	state.drain(testutil.NewTestLogger())
	require.NoError(t, group.Wait())
	require.Zero(t, drainer.drains.Load(), "a term whose context ended must not drain")
}

type recordingDrainer struct {
	ready  chan struct{}
	drains atomic.Int32
}

func (*recordingDrainer) Name() string { return "recording-drainer" }

func (d *recordingDrainer) SubscriptionReady() <-chan struct{} { return d.ready }

func (d *recordingDrainer) Start(ctx context.Context) error {
	close(d.ready)
	<-ctx.Done()
	return nil
}

func (d *recordingDrainer) Drain(context.Context) error {
	d.drains.Add(1)
	return nil
}

const (
	drainLostMessage     = "Leadership term ends without its latest checkpoint"
	drainedMessage       = "Leadership term drained"
	shutdownDrainOutcome = "the shutdown drain must report the term drained"
)

// TestRetireAfterShutdownDrainedTheTermStaysSilent is the #453 sequence:
// SIGTERM drains the term and cancels the process, and only then does the
// successor's hand-over retire the same term.
func TestRetireAfterShutdownDrainedTheTermStaysSilent(t *testing.T) {
	h := newLeaderTermHarness(t)
	h.deploy(t, "global\n  maxconn 100\n")
	h.waitRetained(t, h.deployed[0])
	h.deploy(t, "global\n  maxconn 200\n")
	logger, logs := testutil.NewRecordingLogger()

	h.state.drain(logger)
	h.state.components.cancel()
	h.state.drain(logger)

	require.Equal(t, []slog.Level{slog.LevelInfo}, logs.Levels(drainedMessage), shutdownDrainOutcome)
	require.Empty(t, logs.Levels(drainLostMessage), "a drained term must not be reported as losing its checkpoint")
	h.state.stop(logger)
	checksum, err := h.recoverable(t, h.takeOver(t))
	require.NoError(t, err)
	require.Equal(t, h.deployed[1], checksum)
}

func TestDrainRacingAnInFlightDrainObservesItsResult(t *testing.T) {
	drainer := &gatedDrainer{ready: make(chan struct{}), entered: make(chan struct{}), release: make(chan struct{})}
	state, cancelTerm, wait := startDrainerTerm(t, drainer)
	logger, logs := testutil.NewRecordingLogger()

	shutdown := make(chan struct{})
	go func() {
		defer close(shutdown)
		state.drain(logger)
	}()
	<-drainer.entered
	retire := make(chan struct{})
	go func() {
		defer close(retire)
		state.drain(logger)
	}()
	close(drainer.release)
	<-shutdown
	cancelTerm()
	<-retire
	wait()

	require.Equal(t, []slog.Level{slog.LevelInfo}, logs.Levels(drainedMessage), shutdownDrainOutcome)
	require.Empty(t, logs.Levels(drainLostMessage))
	require.Equal(t, int32(1), drainer.drains.Load())
}

func TestFailedDrainWarnsOnce(t *testing.T) {
	drainer := &gatedDrainer{ready: make(chan struct{}), err: errors.New("checkpoint publish refused")}
	state, cancelTerm, wait := startDrainerTerm(t, drainer)
	logger, logs := testutil.NewRecordingLogger()

	state.drain(logger)
	state.drain(logger)
	cancelTerm()
	wait()

	require.Equal(t, []slog.Level{slog.LevelWarn}, logs.Levels(drainLostMessage))
	require.Empty(t, logs.Levels(drainedMessage))
}

func startDrainerTerm(t *testing.T, drainer lifecycle.Component) (*leaderCallbackState, context.CancelFunc, func()) {
	t.Helper()
	registry := lifecycle.NewRegistry()
	registry.Register(drainer, true)
	ctx, cancel := context.WithCancel(context.Background())
	group, _ := errgroup.WithContext(ctx)
	components, err := startLeaderOnlyComponents(ctx, registry, testutil.NewTestLogger(), func(error) {}, group)
	require.NoError(t, err)
	state := &leaderCallbackState{components: components}
	state.leading.Store(true)
	return state, cancel, func() { require.NoError(t, group.Wait()) }
}

// gatedDrainer holds its first drain until release and fails every later one
// once the term ends, the way a second checkpoint flush cannot complete.
type gatedDrainer struct {
	ready   chan struct{}
	entered chan struct{}
	release chan struct{}
	err     error
	drains  atomic.Int32
}

func (*gatedDrainer) Name() string { return "gated-drainer" }

func (d *gatedDrainer) SubscriptionReady() <-chan struct{} { return d.ready }

func (d *gatedDrainer) Start(ctx context.Context) error {
	close(d.ready)
	<-ctx.Done()
	return nil
}

func (d *gatedDrainer) Drain(ctx context.Context) error {
	if d.drains.Add(1) > 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	if d.entered != nil {
		close(d.entered)
		<-d.release
	}
	return d.err
}
