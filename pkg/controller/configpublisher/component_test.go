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

package configpublisher

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	crdclientfake "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// testEnv is a config-publisher Component wired over fake clients. The SSA
// list-map merge reactor makes per-pod status patches accumulate the way a real
// apiserver applies them. Subscribe to bus before calling start.
type testEnv struct {
	ctx       context.Context
	crdClient *crdclientfake.Clientset
	bus       *busevents.EventBus
	publisher *configpublisher.Publisher
	start     func()
}

type mapPublicationFailureOnce struct {
	mu           sync.Mutex
	contents     []string
	firstFailure chan struct{}
}

func newMapPublicationFailureOnce() *mapPublicationFailureOnce {
	return &mapPublicationFailureOnce{firstFailure: make(chan struct{})}
}

func (f *mapPublicationFailureOnce) react(action k8stesting.Action) (bool, runtime.Object, error) {
	mapFile := action.(k8stesting.CreateAction).GetObject().(*v1alpha1.HAProxyMapFile)
	f.mu.Lock()
	f.contents = append(f.contents, mapFile.Spec.Entries)
	attempt := len(f.contents)
	f.mu.Unlock()
	if attempt == 1 {
		close(f.firstFailure)
		return true, nil, apierrors.NewServiceUnavailable("map storage unavailable")
	}
	return false, nil, nil
}

func (f *mapPublicationFailureOnce) attemptedContents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.contents...)
}

type publicationRetryGate struct {
	waiting chan struct{}
	release chan struct{}
	once    sync.Once
}

func newPublicationRetryGate() *publicationRetryGate {
	return &publicationRetryGate{
		waiting: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (g *publicationRetryGate) wait(ctx context.Context, _ time.Duration, superseded <-chan struct{}) bool {
	g.once.Do(func() { close(g.waiting) })
	select {
	case <-g.release:
		return true
	case <-superseded:
		return false
	case <-ctx.Done():
		return false
	}
}

func waitForTestSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(failure)
	}
}

func countConfigPublishedEvents(eventChan <-chan busevents.Event) int {
	count := 0
	for {
		select {
		case event := <-eventChan:
			if _, ok := event.(*events.ConfigPublishedEvent); ok {
				count++
			}
		default:
			return count
		}
	}
}

func waitForConfigPublished(t *testing.T, ctx context.Context, eventChan <-chan busevents.Event) {
	t.Helper()
	for {
		select {
		case event := <-eventChan:
			if _, ok := event.(*events.ConfigPublishedEvent); ok {
				return
			}
		case <-ctx.Done():
			t.Fatal("complete retry did not emit ConfigPublishedEvent")
		}
	}
}

func newTestComponent(t *testing.T) testEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	crdClient := crdclientfake.NewSimpleClientset()
	installSSAListMapMergeReactor(crdClient)
	bus := busevents.NewEventBus(100)
	publisher := configpublisher.NewWithListers(k8sfake.NewClientset(), crdClient, nil, testutil.NewTestLogger())
	component := New(publisher, bus, testutil.NewTestLogger())

	return testEnv{ctx, crdClient, bus, publisher, func() {
		go component.Start(ctx)
		waitForTestSignal(t, ctx, component.SubscriptionReady(), "config publisher did not subscribe")
		bus.Start()
	}}
}

func TestComponent_ConfigPublishedEvent(t *testing.T) {
	env := newTestComponent(t)

	// Subscribe to capture ConfigPublishedEvent
	eventChan := env.bus.Subscribe("test-sub", 50)

	env.start()

	// Create template config for ConfigValidatedEvent
	templateConfig := &v1alpha1.HAProxyTemplateConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
			UID:       "test-uid-456",
		},
	}

	// Step 1: Publish ConfigValidatedEvent to cache template config
	env.bus.Publish(events.NewConfigValidatedEvent(
		nil, // Config (not used by component)
		templateConfig,
		"v1",
		"secret-v1",
	))

	// Step 2: Publish TemplateRenderedEvent, which queues the publish
	correlationID := t.Name()
	testHAProxyConfig := "global\n  daemon\n\ndefaults\n  mode http\n"
	env.bus.Publish(newConfigPublisherRenderedEvent(
		t, testHAProxyConfig, nil, 100,
		events.WithCorrelation(correlationID, ""),
	))

	// Wait for ConfigPublishedEvent
	var receivedEvent *events.ConfigPublishedEvent
	timeout := time.After(2 * time.Second)

eventLoop:
	for {
		select {
		case event := <-eventChan:
			if published, ok := event.(*events.ConfigPublishedEvent); ok {
				receivedEvent = published
				break eventLoop
			}
		case <-timeout:
			t.Fatal("timeout waiting for ConfigPublishedEvent")
		}
	}

	// Verify ConfigPublishedEvent metadata
	require.NotNil(t, receivedEvent)
	assert.Equal(t, "test-config-haproxycfg", receivedEvent.RuntimeConfigName)
	assert.Equal(t, "default", receivedEvent.RuntimeConfigNamespace)
	assert.Equal(t, 0, receivedEvent.MapFileCount)
	assert.Equal(t, 0, receivedEvent.SecretCount)

	// Verify runtime config was created in Kubernetes
	runtimeConfig, err := env.crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(env.ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	assert.Equal(t, "test-config-haproxycfg", runtimeConfig.Name)
	assert.Contains(t, runtimeConfig.Spec.Content, "global")
}

func TestComponent_RetriesIncompletePublicationWithoutCompletingEarly(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	crdClient := crdclientfake.NewSimpleClientset()
	installSSAListMapMergeReactor(crdClient)
	failure := newMapPublicationFailureOnce()
	crdClient.PrependReactor("create", "haproxymapfiles", failure.react)

	bus := busevents.NewEventBus(100)
	publisher := configpublisher.NewWithListers(
		k8sfake.NewClientset(), crdClient, nil, testutil.NewTestLogger())
	retryGate := newPublicationRetryGate()
	component := New(publisher, bus, testutil.NewTestLogger(), withPublicationRetryWait(retryGate.wait))
	eventChan := bus.Subscribe("publication-retry-test", 50)
	startDone := make(chan error, 1)
	go func() { startDone <- component.Start(ctx) }()
	waitForTestSignal(t, ctx, component.SubscriptionReady(), "config publisher did not subscribe")
	bus.Start()

	templateConfig := &v1alpha1.HAProxyTemplateConfig{ObjectMeta: metav1.ObjectMeta{
		Name: "test-config", Namespace: "default", UID: "test-uid",
	}}
	auxFiles := &dataplane.AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{{
		Path: "/etc/haproxy/maps/host.map", Content: "example.com backend1\n",
	}}}
	correlationID := t.Name()
	bus.Publish(events.NewConfigValidatedEvent(nil, templateConfig, "v1", "secret-v1"))
	rendered := newConfigPublisherRenderedEvent(
		t, "global\n  daemon\n", auxFiles, 1,
		events.WithCorrelation(correlationID, ""),
	)
	bus.Publish(rendered)
	waitForTestSignal(t, ctx, failure.firstFailure, "map-file publication did not fail")
	waitForTestSignal(t, ctx, retryGate.waiting, "publication retry was not scheduled")

	component.mu.RLock()
	assert.Empty(t, component.lastPublishedChecksum)
	component.mu.RUnlock()

	assert.Zero(t, countConfigPublishedEvents(eventChan), "incomplete publication emitted ConfigPublishedEvent")

	auxFiles.MapFiles[0].Content = "mutated after enqueue\n"
	close(retryGate.release)
	waitForConfigPublished(t, ctx, eventChan)
	assert.Zero(t, countConfigPublishedEvents(eventChan), "publication emitted duplicate completion")
	assert.Equal(t, []string{"example.com backend1\n", "example.com backend1\n"}, failure.attemptedContents())

	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, runtimeConfig.Status.AuxiliaryFiles)
	require.Len(t, runtimeConfig.Status.AuxiliaryFiles.MapFiles, 1)
	mapFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Get(ctx, runtimeConfig.Status.AuxiliaryFiles.MapFiles[0].Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "example.com backend1\n", mapFile.Spec.Entries)
	assert.Equal(t, mapFile.Name, runtimeConfig.Status.AuxiliaryFiles.MapFiles[0].Name)
	assert.Equal(t, rendered.ContentChecksum, runtimeConfig.Spec.Checksum)

	cancel()
	select {
	case err := <-startDone:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("config publisher did not stop")
	}
}

// TestComponent_DeployedConfigPublishRequest verifies the CR self-consistency
// fix: a DeployedConfigPublishRequest (the bytes the deployer just applied)
// publishes those exact bytes as the HAProxyCfg spec. Because the publisher
// derives spec.Checksum from spec.Content with the same ComputeContentChecksum
// the deployer used for status.deployedToPods[].Checksum, publishing the
// deployed content guarantees the deployed checksum becomes an observable
// published spec.Checksum — even though no validation-driven publish ran for it
// in this test (mirroring a render coalesced away under churn).
func TestComponent_DeployedConfigPublishRequest(t *testing.T) {
	env := newTestComponent(t)

	eventChan := env.bus.Subscribe("test-sub", 50)
	env.start()

	// Cache the template config — handleDeployedConfigPublishRequest drops the
	// request if no template config is cached yet.
	templateConfig := &v1alpha1.HAProxyTemplateConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "test-config", Namespace: "default", UID: "uid-deployed"},
	}
	env.bus.Publish(events.NewConfigValidatedEvent(nil, templateConfig, "v1", "secret-v1"))
	time.Sleep(50 * time.Millisecond)

	// A render the deployer applied but whose validation-driven publish never ran.
	deployedConfig := "global\n  daemon\n\nbackend marker-be\n  server s1 1.2.3.4:80\n"
	rendered := newConfigPublisherRenderedEvent(t, deployedConfig, nil, 1)
	occurrence, err := rendered.RenderOccurrence()
	require.NoError(t, err)
	publishRequest, err := events.NewDeployedConfigPublishRequestWithCycle(
		"test-config-haproxycfg", "default", occurrence,
	)
	require.NoError(t, err)
	env.bus.Publish(publishRequest)

	var published *events.ConfigPublishedEvent
	timeout := time.After(2 * time.Second)
eventLoop:
	for {
		select {
		case ev := <-eventChan:
			if p, ok := ev.(*events.ConfigPublishedEvent); ok {
				published = p
				break eventLoop
			}
		case <-timeout:
			t.Fatal("timeout waiting for ConfigPublishedEvent from a DeployedConfigPublishRequest")
		}
	}
	require.NotNil(t, published)
	assert.Equal(t, "test-config-haproxycfg", published.RuntimeConfigName)

	cr, err := env.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(env.ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, deployedConfig, cr.Spec.Content,
		"the deployer's exact bytes must be published as spec.Content")
	assert.NotEmpty(t, cr.Spec.Checksum,
		"spec.Checksum must be set so the deployed checksum is observable")
}

func TestComponent_Name(t *testing.T) {
	// Setup
	k8sClient := k8sfake.NewClientset()
	crdClient := crdclientfake.NewSimpleClientset()
	installSSAListMapMergeReactor(crdClient)
	eventBus := busevents.NewEventBus(100)

	publisher := configpublisher.NewWithListers(k8sClient, crdClient, nil, testutil.NewTestLogger())
	component := New(publisher, eventBus, testutil.NewTestLogger())

	// Verify name
	assert.Equal(t, ComponentName, component.Name())
	assert.Equal(t, "config-publisher", component.Name())
}

func TestComponent_LostLeadership(t *testing.T) {
	env := newTestComponent(t)

	// Subscribe to capture events
	eventChan := env.bus.Subscribe("test-sub", 50)

	env.start()

	// Create template config for ConfigValidatedEvent
	templateConfig := &v1alpha1.HAProxyTemplateConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
			UID:       "test-uid-123",
		},
	}

	// Step 1: Publish ConfigValidatedEvent to cache template config
	env.bus.Publish(events.NewConfigValidatedEvent(
		nil, // Config (not used by component)
		templateConfig,
		"v1",
		"secret-v1",
	))

	// Step 2: Publish LostLeadershipEvent to clear cached state
	env.bus.Publish(events.NewLostLeadershipEvent("lost-leader-id", "test_reason"))

	// Give component time to process event
	time.Sleep(200 * time.Millisecond)

	// Step 3: Now publish a render - should NOT publish config because the
	// cached template config was cleared with leadership.
	correlationID := t.Name()
	testHAProxyConfig := "global\n  daemon\n"
	env.bus.Publish(newConfigPublisherRenderedEvent(
		t, testHAProxyConfig, nil, 100,
		events.WithCorrelation(correlationID, ""),
	))

	// Give time for any potential ConfigPublishedEvent (which shouldn't happen)
	timeout := time.After(500 * time.Millisecond)
	var receivedConfigPublished bool

drainLoop:
	for {
		select {
		case event := <-eventChan:
			if _, ok := event.(*events.ConfigPublishedEvent); ok {
				receivedConfigPublished = true
			}
		case <-timeout:
			break drainLoop
		}
	}

	// Verify no ConfigPublishedEvent was received (state should have been cleared)
	assert.False(t, receivedConfigPublished, "ConfigPublishedEvent should not be published after leadership loss")
}

func TestComponent_ValidationFailed(t *testing.T) {
	env := newTestComponent(t)

	env.start()

	// Create template config for ConfigValidatedEvent
	templateConfig := &v1alpha1.HAProxyTemplateConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
			UID:       "test-uid-789",
		},
	}

	// Step 1: Publish ConfigValidatedEvent to cache template config
	env.bus.Publish(events.NewConfigValidatedEvent(
		nil,
		templateConfig,
		"v1",
		"secret-v1",
	))

	// Step 2: Publish TemplateRenderedEvent to cache rendered config
	// Generate a correlation ID to link TemplateRenderedEvent and ValidationFailedEvent
	correlationID := t.Name()
	testHAProxyConfig := "global\n  daemon\n  maxconn invalid\n"
	env.bus.Publish(newConfigPublisherRenderedEvent(
		t, testHAProxyConfig, nil, 100,
		events.WithCorrelation(correlationID, ""),
	))

	// Give component time to process events
	time.Sleep(200 * time.Millisecond)

	// Step 3: Publish ValidationFailedEvent (with matching correlation ID)
	env.bus.Publish(events.NewValidationFailedEvent(
		[]string{"maxconn must be numeric", "invalid configuration directive"},
		100,
		"",
		events.WithCorrelation(correlationID, ""),
	))

	// Give component time to process event
	time.Sleep(500 * time.Millisecond)

	// Verify invalid config was published (for observability) - note the -invalid suffix
	runtimeConfig, err := env.crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(env.ctx, "test-config-haproxycfg-invalid", metav1.GetOptions{})

	require.NoError(t, err)
	assert.Equal(t, "test-config-haproxycfg-invalid", runtimeConfig.Name)
	assert.Contains(t, runtimeConfig.Spec.Content, "maxconn invalid")
	// Check validation error is recorded in status
	assert.Contains(t, runtimeConfig.Status.ValidationError, "maxconn must be numeric")
}

func TestComponent_ValidationFailed_NoCachedState(t *testing.T) {
	env := newTestComponent(t)

	env.start()

	// Publish ValidationFailedEvent without any prior cached state
	// Even with a correlation ID, there's no matching TemplateRenderedEvent
	correlationID := t.Name()
	env.bus.Publish(events.NewValidationFailedEvent(
		[]string{"some error"},
		100,
		"",
		events.WithCorrelation(correlationID, ""),
	))

	// Give component time to process event
	time.Sleep(300 * time.Millisecond)

	// Verify no config was created (should have been skipped due to missing cached state)
	_, err := env.crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(env.ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.Error(t, err, "Should get error because no config should be created")
}

func newConfigPublisherRenderedEvent(
	tb testing.TB,
	config string,
	auxFiles *dataplane.AuxiliaryFiles,
	durationMs int64,
	opts ...events.CorrelationOption,
) *events.TemplateRenderedEvent {
	tb.Helper()
	fixture := testutil.NewRenderCycleFixture(tb)
	artifacts := fixture.Artifacts(tb, auxFiles, nil)
	plan := &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		Sections: []renderplan.Section{{
			Kind: renderplan.SectionKindCore, Name: "core#0", Text: config,
			TextKnown: true, TextDigest: renderplan.DigestString(config), Length: len(config),
		}},
		Maps: make(map[string]renderplan.Map),
		Files: []renderplan.File{{
			Path: renderplan.ConfigFilePath, Kind: renderplan.FileKindConfig,
			ReloadOnChange: true, Content: config, ContentKnown: true,
			Digest: renderplan.DigestString(config), Size: int64(len(config)),
		}},
	}
	if auxFiles != nil {
		for index := range auxFiles.MapFiles {
			file := auxFiles.MapFiles[index]
			plan.Maps[file.Path] = renderplan.Map{
				Path: file.Path, Ordered: true, Entries: renderplan.ParseMapEntries(file.Content),
			}
			plan.Files = append(plan.Files, renderplan.File{
				Path: file.Path, Kind: renderplan.FileKindMap,
				Content: file.Content, ContentKnown: true,
				Digest: renderplan.DigestString(file.Content), Size: int64(len(file.Content)),
			})
		}
	}
	plan.ComputeID()
	cycle := fixture.SnapshotWithEffects(tb, config, plan, artifacts, nil, nil, nil, nil)
	event, err := events.NewTemplateRenderedEventWithCycle(
		cycle, durationMs, "", true, opts...,
	)
	require.NoError(tb, err)
	return event
}
