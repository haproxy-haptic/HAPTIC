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

package reconciler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	controllerhttpstore "github.com/haproxy-haptic/HAPTIC/pkg/controller/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercycle"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	purehttpstore "github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

// acceptanceHarness runs the leader's coordinator over a real render pipeline
// and HTTP store, playing the resource applier and the render gate itself.
type acceptanceHarness struct {
	t           *testing.T
	bus         *busevents.EventBus
	component   *controllerhttpstore.Component
	service     *renderer.RenderService
	coordinator *Coordinator
	routes      *k8sstore.MemoryStore
	rendered    chan renderedOutput
	metrics     *metrics.Metrics
	stop        func()
}

type renderedOutput struct {
	reason     string
	config     string
	occurrence *rendercycle.Occurrence
}

func newAcceptanceHarness(t *testing.T, pageURL string, critical bool, subscribe ...func(*busevents.EventBus)) *acceptanceHarness {
	t.Helper()
	return newAcceptanceHarnessWithTemplate(t, pageURL, routeTemplate(critical), subscribe...)
}

func newAcceptanceHarnessWithTemplate(t *testing.T, pageURL, template string, subscribe ...func(*busevents.EventBus)) *acceptanceHarness {
	t.Helper()
	cfg := acceptanceConfig()
	cfg.TemplateSnippets = map[string]config.TemplateSnippet{
		"routes": {
			Name:        "routes",
			Requires:    []string{"routes"},
			Incremental: &config.IncrementalTemplate{Source: "routes"},
			Template:    template,
		},
	}
	cfg.HAProxyConfig = config.HAProxyConfig{Template: `{{ render "routes" }}`}
	return newAcceptanceHarnessWithConfig(t, pageURL, cfg, subscribe...)
}

func acceptanceConfig() *config.Config {
	return &config.Config{
		Dataplane: config.DataplaneConfig{
			MapsDir: "/etc/haproxy/maps", SSLCertsDir: "/etc/haproxy/ssl", GeneralStorageDir: "/etc/haproxy/files",
		},
		WatchedResources: map[string]config.WatchedResource{
			"routes": {APIVersion: "example.test/v1", Resources: "routes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
	}
}

func newAcceptanceHarnessWithConfig(t *testing.T, pageURL string, cfg *config.Config, subscribe ...func(*busevents.EventBus)) *acceptanceHarness {
	t.Helper()
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types: map[string]reflect.Type{}, Kinds: map[string]string{}, Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	bus, logger := testutil.NewTestBusAndLogger()
	component := controllerhttpstore.New(bus, logger, 0)
	service := renderer.NewRenderService(&renderer.RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: logger, HTTPStoreComponent: component,
		ColdRendersRunToCompletion: true,
	})
	t.Cleanup(func() { _ = service.RetireIncrementalCache() })
	routes := k8sstore.NewMemoryStore(2)
	harness := &acceptanceHarness{
		t: t, bus: bus, component: component, service: service, routes: routes,
		rendered: make(chan renderedOutput, 16),
		metrics:  metrics.NewMetrics(prometheus.NewRegistry()),
	}
	require.NoError(t, routes.Add(harness.route(pageURL, ""), []string{"default", "a"}))
	harness.coordinator = NewCoordinator(&CoordinatorConfig{
		EventBus:       bus,
		Pipeline:       pipeline.New(&pipeline.PipelineConfig{Renderer: service, Logger: logger}),
		StoreProvider:  stores.NewRealStoreProvider(map[string]stores.Store{"routes": routes}),
		HTTPAcceptance: component,
		GraphWarm:      service.IncrementalGraphWarm,
		Metrics:        harness.metrics,
		Logger:         logger,
	})
	observed := bus.Subscribe("acceptance-harness", 64)
	for _, observe := range subscribe {
		observe(bus)
	}
	bus.Start()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go harness.applyResources(ctx, observed)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = harness.coordinator.Start(ctx)
	}()
	componentStopped := make(chan struct{})
	go func() {
		defer close(componentStopped)
		_ = component.Start(ctx)
	}()
	harness.stop = func() {
		cancel()
		for _, done := range []chan struct{}{stopped, componentStopped} {
			select {
			case <-done:
			case <-time.After(testutil.EventTimeout):
				t.Fatal("the coordinator or the HTTP store did not stop")
			}
		}
	}
	t.Cleanup(harness.stop)
	select {
	case <-harness.coordinator.SubscriptionReady():
	case <-time.After(testutil.EventTimeout):
		t.Fatal("coordinator did not subscribe")
	}
	return harness
}

// routeTemplate renders "name=page<suffix>". A critical fetch is not handled
// by the template, so its failure fails the render.
func routeTemplate(critical bool) string {
	fetch := `http.Fetch(item | dig_string("", "spec", "url"), map[string]any{"retries": 1, "timeout": "1s", "critical": ` +
		strconv.FormatBool(critical) + `})`
	line := `{{ item | dig_string("", "metadata", "name") }}={{ page }}{{ item | dig_string("", "spec", "suffix") }}` + "\n"
	if critical {
		return strings.Replace(line, "{{ page }}", "{{ "+fetch+" }}", 1)
	}
	return `{%- var page, _ = ` + fetch + ` -%}` + "\n" + line
}

func (h *acceptanceHarness) route(url, suffix string) map[string]any {
	return map[string]any{
		"apiVersion": "example.test/v1",
		"kind":       "Route",
		"metadata":   map[string]any{"namespace": "default", "name": "a"},
		"spec":       map[string]any{"url": url, "suffix": suffix},
	}
}

// applyResources answers each deploying render as the resource applier does
// and hands it to the test.
func (h *acceptanceHarness) applyResources(ctx context.Context, observed <-chan busevents.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-observed:
			rendered, ok := event.(*events.TemplateRenderedEvent)
			if !ok {
				continue
			}
			occurrence, err := rendered.RenderOccurrence()
			if err != nil {
				continue
			}
			processed, err := events.NewResourcesProcessedEvent(occurrence)
			if err != nil {
				continue
			}
			h.bus.Publish(processed)
			h.rendered <- renderedOutput{reason: rendered.TriggerReason, config: rendered.HAProxyConfig, occurrence: occurrence}
		}
	}
}

func (h *acceptanceHarness) reconcile(reason string) {
	h.bus.Publish(events.NewReconciliationTriggeredEvent(reason, false))
}

func (h *acceptanceHarness) deployedFor(reason string) renderedOutput {
	h.t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case output := <-h.rendered:
			if output.reason == reason {
				return output
			}
		case <-timeout.C:
			h.t.Fatalf("no render was deployed for %q", reason)
			return renderedOutput{}
		}
	}
}

func (h *acceptanceHarness) verdict(output renderedOutput, ok bool) {
	h.t.Helper()
	event, err := events.NewRenderGateCompletedEventWithCycle(output.occurrence, ok, !ok, true, "", false, 1)
	require.NoError(h.t, err)
	h.bus.Publish(event)
}

func (h *acceptanceHarness) waitForAttempts(after uint64) {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		h.coordinator.ledger.mu.Lock()
		idle := !h.coordinator.ledger.attempting
		h.coordinator.ledger.mu.Unlock()
		return idle && h.coordinator.ledger.attempted.Load() > after
	}, 5*time.Second, 5*time.Millisecond)
}

func TestAcceptedHTTPContentRetainsRenderCorrelation(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("page"))
	}))
	t.Cleanup(page.Close)
	var observed <-chan busevents.Event
	h := newAcceptanceHarness(t, page.URL, false, func(bus *busevents.EventBus) {
		observed = bus.Subscribe("acceptance-correlation", 64)
	})
	h.reconcile("start")
	h.deployedFor("http_content_accepted")
	h.waitForAttempts(0)

	triggers := make(map[string]*events.ReconciliationTriggeredEvent)
	observeTrigger := func(event busevents.Event) {
		if trigger, ok := event.(*events.ReconciliationTriggeredEvent); ok && trigger.Reason == "http_content_accepted" {
			assert.NotEmpty(t, trigger.CorrelationID())
			assert.Empty(t, trigger.CausationID())
			triggers[trigger.EventID()] = trigger
		}
	}
	var rendered *events.TemplateRenderedEvent
	testutil.WaitForEventWithPredicate(t, observed, testutil.EventTimeout, func(event busevents.Event) bool {
		observeTrigger(event)
		if output, ok := event.(*events.TemplateRenderedEvent); ok && output.TriggerReason == "http_content_accepted" {
			rendered = output
		}
		return rendered != nil
	})
	for range len(observed) {
		observeTrigger(<-observed)
	}
	trigger, ok := triggers[rendered.CausationID()]
	require.True(t, ok, "the render must name an observed trigger, including when triggers coalesce")
	assert.Equal(t, "a=page\n", rendered.HAProxyConfig)
	assert.Equal(t, trigger.CorrelationID(), rendered.CorrelationID())
}

func TestRefusedContentIsRevokedAndTheFleetKeepsProgressing(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("page"))
	}))
	t.Cleanup(page.Close)
	h := newAcceptanceHarness(t, page.URL, false)

	h.reconcile("start")
	assert.Equal(t, "a=\n", h.deployedFor("start").config, "a deploy does not wait for new content")
	assert.Equal(t, "a=page\n", h.deployedFor("http_content_accepted").config, "the content is accepted beside the reconcile loop")
	h.waitForAttempts(0)

	require.NoError(t, h.routes.Update(h.route(page.URL, "!"), []string{"default", "a"}))
	h.reconcile("cluster changes to S′")
	withContent := h.deployedFor("cluster changes to S′")
	require.Equal(t, "a=page!\n", withContent.config)
	attempts := h.coordinator.ledger.attempted.Load()
	h.verdict(withContent, false)

	assert.Equal(t, "a=!\n", h.deployedFor("http_content_revoked").config, "the render without the revoked content still deploys")
	h.waitForAttempts(attempts)

	require.NoError(t, h.routes.Update(h.route(page.URL, "?"), []string{"default", "a"}))
	h.reconcile("cluster moves on")
	assert.Equal(t, "a=?\n", h.deployedFor("cluster moves on").config)
	assert.Equal(t, "a=page?\n", h.deployedFor("http_content_accepted").config, "new inputs let the same content be accepted again")
}

func TestAPendingFetchDoesNotDelayADeploy(t *testing.T) {
	release := make(chan struct{})
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write([]byte("page"))
	}))
	t.Cleanup(page.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h := newAcceptanceHarness(t, page.URL, false)

	h.reconcile("start")
	assert.Equal(t, "a=\n", h.deployedFor("start").config)
	require.NoError(t, h.routes.Update(h.route(page.URL, "!"), []string{"default", "a"}))
	h.reconcile("next change while the fetch is in flight")
	assert.Equal(t, "a=!\n", h.deployedFor("next change while the fetch is in flight").config)

	close(release)
	assert.Equal(t, "a=page!\n", h.deployedFor("http_content_accepted").config)
}

func TestForegroundRendersDoNotInvalidatePendingAcceptance(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = w.Write([]byte("page"))
	}))
	t.Cleanup(page.Close)
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	h := newAcceptanceHarness(t, page.URL, false)
	h.reconcile("start")
	require.Equal(t, "a=\n", h.deployedFor("start").config)
	select {
	case <-entered:
	case <-time.After(testutil.EventTimeout):
		t.Fatal("acceptance fetch did not start")
	}
	for change := range 4 {
		suffix := strconv.Itoa(change)
		require.NoError(t, h.routes.Update(h.route(page.URL, suffix), []string{"default", "a"}))
		reason := "foreground change " + suffix
		h.reconcile(reason)
		require.Equal(t, "a="+suffix+"\n", h.deployedFor(reason).config)
	}
	unblock()
	require.Equal(t, "a=page3\n", h.deployedFor("http_content_accepted").config)
	h.waitForAttempts(0)
	assert.Equal(t, int32(1), requests.Load(), "unchanged HTTP declarations must preserve the fetched candidate")
}

func TestConfirmedContentSurvivesALaterRefusal(t *testing.T) {
	for _, critical := range []bool{false, true} {
		t.Run(strconv.FormatBool(critical), func(t *testing.T) {
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("page"))
			}))
			t.Cleanup(page.Close)
			h := newAcceptanceHarness(t, page.URL, critical)
			h.reconcile("start")
			if !critical {
				h.deployedFor("start")
			}
			accepted := h.deployedFor("http_content_accepted")
			h.waitForAttempts(0)
			require.Equal(t, "a=page\n", accepted.config)
			window, recorded := h.coordinator.ledger.window(accepted.occurrence)
			require.True(t, recorded)
			require.NotEmpty(t, window.observations, "the first deployed content needs exact confirmation evidence")
			h.verdict(accepted, true)

			require.NoError(t, h.routes.Update(h.route(page.URL, "!"), []string{"default", "a"}))
			h.reconcile("broken change")
			refused := h.deployedFor("broken change")
			require.Equal(t, "a=page!\n", refused.config)
			h.verdict(refused, false)

			h.reconcile("next change")
			assert.Equal(t, "a=page!\n", h.deployedFor("next change").config)
		})
	}
}

func TestCriticalContentHoldsTheDeployInsteadOfGoingWithoutIt(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("page"))
	}))
	t.Cleanup(page.Close)
	h := newAcceptanceHarness(t, page.URL, true)

	h.reconcile("start")
	assert.Equal(t, "a=page\n", h.deployedFor("http_content_accepted").config, "the first deploy already carries the critical content")
	h.waitForAttempts(0)

	require.NoError(t, h.routes.Update(h.route(page.URL, "!"), []string{"default", "a"}))
	h.reconcile("cluster changes to S′")
	withContent := h.deployedFor("cluster changes to S′")
	require.Equal(t, "a=page!\n", withContent.config)
	attempts := h.coordinator.ledger.attempted.Load()
	h.verdict(withContent, false)
	h.waitForAttempts(attempts)

	require.NoError(t, h.routes.Update(h.route(page.URL, "?"), []string{"default", "a"}))
	h.reconcile("cluster moves on")
	assert.Equal(t, "a=page?\n", h.deployedFor("http_content_accepted").config)
}

func TestCriticalFetchFailureReportsBlockedReconciliation(t *testing.T) {
	var repaired atomic.Bool
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !repaired.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("page"))
	}))
	t.Cleanup(page.Close)
	var failures <-chan busevents.Event
	template := strings.Replace(routeTemplate(true), `"retries": 1`, `"retries": 1, "interval": "1s"`, 1)
	h := newAcceptanceHarnessWithTemplate(t, page.URL, template, func(bus *busevents.EventBus) {
		failures = bus.SubscribeTypes("critical-fetch-failures", 16,
			events.EventTypeTemplateRenderFailed, events.EventTypeReconciliationFailed)
	})
	h.reconcile("critical source unavailable")
	failure := testutil.WaitForEvent[*events.TemplateRenderFailedEvent](t, failures, 5*time.Second)
	require.Contains(t, failure.Error, "503")
	testutil.WaitForEvent[*events.ReconciliationFailedEvent](t, failures, testutil.EventTimeout)
	repaired.Store(true)
	require.Equal(t, "a=page\n", h.deployedFor("http_content_accepted").config)
	require.NoError(t, h.coordinator.ledger.criticalContentFailure())
}

// The leader without a critical source's content reports the failed
// reconciliation once and neither renders nor fetches again per trigger.
func TestUnavailableCriticalSourceDoesNotReRenderPerReconcile(t *testing.T) {
	var requests atomic.Int64
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(page.Close)
	var observed <-chan busevents.Event
	template := strings.Replace(routeTemplate(true), `"retries": 1`, `"retries": 1, "interval": "1h"`, 1)
	h := newAcceptanceHarnessWithTemplate(t, page.URL, template, func(bus *busevents.EventBus) {
		observed = bus.SubscribeTypes("critical-outage", 64,
			events.EventTypeReconciliationStarted, events.EventTypeReconciliationFailed)
	})
	started := func(reason string) {
		t.Helper()
		testutil.WaitForEventWithPredicate(t, observed, 5*time.Second, func(event *events.ReconciliationStartedEvent) bool {
			return event.Trigger == reason
		})
	}
	h.reconcile("critical source unavailable")
	failure := testutil.WaitForEvent[*events.ReconciliationFailedEvent](t, observed, 5*time.Second)
	require.Contains(t, failure.Error, "503")
	renders := promtestutil.ToFloat64(h.metrics.RenderTotal.WithLabelValues("cold"))
	failed := promtestutil.ToFloat64(h.metrics.RenderErrors.WithLabelValues("cold"))
	require.Positive(t, failed, "failed renders are counted")
	require.Equal(t, renders, failed, "no render succeeded")
	fetched := requests.Load()

	for i := range 10 {
		require.NoError(t, h.routes.Update(h.route(page.URL, strconv.Itoa(i)), []string{"default", "a"}))
		h.reconcile("churn " + strconv.Itoa(i))
		started("churn " + strconv.Itoa(i))
	}
	h.reconcile("settled")
	started("settled")
	testutil.AssertNoEvent[*events.ReconciliationFailedEvent](t, observed, testutil.NoEventTimeout)
	assert.Equal(t, fetched, requests.Load(), "held reconciles must not fetch")
	assert.Equal(t, renders, promtestutil.ToFloat64(h.metrics.RenderTotal.WithLabelValues("cold")), "held reconciles must not render")
}

func TestCriticalFetchRetryStopsOnShutdown(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(page.Close)
	var failures <-chan busevents.Event
	template := strings.Replace(routeTemplate(true), `"retries": 1`, `"retries": 1, "interval": "24h"`, 1)
	h := newAcceptanceHarnessWithTemplate(t, page.URL, template, func(bus *busevents.EventBus) {
		failures = bus.SubscribeTypes("critical-fetch-failures", 16, events.EventTypeReconciliationFailed)
	})
	h.reconcile("critical source unavailable")
	testutil.WaitForEvent[*events.ReconciliationFailedEvent](t, failures, 5*time.Second)
	h.stop()
}

func TestCriticalFetchFailureClearsWhenRecoveredContentIsInvalid(t *testing.T) {
	var repaired atomic.Bool
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !repaired.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("page"))
	}))
	t.Cleanup(page.Close)
	template := `{% var _, fetchErr = http.Fetch(item | dig_string("", "spec", "url"), map[string]any{"critical": true, "retries": 1, "timeout": "1s", "interval": "200ms"}) %}
{% if fetchErr == nil %}{{ fail("recovered content is invalid") }}{% end %}`
	h := newAcceptanceHarnessWithTemplate(t, page.URL, template)
	h.reconcile("critical source unavailable")
	h.waitForAttempts(0)
	require.True(t, pipeline.CriticalContentUnavailable(h.coordinator.ledger.criticalContentFailure()))
	repaired.Store(true)
	h.reconcile("critical source repaired")
	require.Eventually(t, func() bool {
		h.coordinator.ledger.mu.Lock()
		defer h.coordinator.ledger.mu.Unlock()
		return strings.Contains(h.coordinator.ledger.lastRefusal, "recovered content is invalid")
	}, 5*time.Second, 5*time.Millisecond, "the source's refetch leads to an acceptance attempt")
	require.NoError(t, h.coordinator.ledger.criticalContentFailure())
	_, accepted := h.component.GetStore().Get(page.URL)
	require.False(t, accepted, "invalid recovered content must remain unaccepted")
}

func TestGateVerdictsRetainEachOccurrenceOfTheSameOutput(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("page"))
	}))
	t.Cleanup(page.Close)
	h := newAcceptanceHarness(t, page.URL, false)
	h.reconcile("start")
	h.deployedFor("start")
	first := h.deployedFor("http_content_accepted")
	h.waitForAttempts(0)
	store := h.component.GetStore()
	source, err := store.ReconcileSource(page.URL+"/unused", purehttpstore.FetchOptions{}, nil)
	require.NoError(t, err)
	_, candidate, err := store.PrepareInitial(t.Context(), page.URL+"/unused", source.State)
	require.NoError(t, err)
	require.NoError(t, store.CommitInitialCandidates(t.Context(), []*purehttpstore.InitialCandidate{candidate}))
	h.reconcile("same output from a later execution")
	second := h.deployedFor("same output from a later execution")
	require.Equal(t, first.config, second.config)
	firstWindow, exists := h.coordinator.ledger.window(first.occurrence)
	require.True(t, exists)
	secondWindow, exists := h.coordinator.ledger.window(second.occurrence)
	require.True(t, exists)
	require.NotSame(t, first.occurrence, second.occurrence)
	assert.Less(t, firstWindow.reached, secondWindow.reached, "a later identical output must not overwrite the earlier occurrence")

	h.verdict(second, false)
	assert.Equal(t, "a=\n", h.deployedFor("http_content_revoked").config)
	_, accepted := store.Get(page.URL)
	assert.False(t, accepted)
}

// heldPage answers "page" once release is closed; entered signals a request.
func heldPage(t *testing.T) (page *httptest.Server, entered <-chan struct{}, release chan struct{}) {
	t.Helper()
	requested := make(chan struct{}, 1)
	release = make(chan struct{})
	page = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		select {
		case requested <- struct{}{}:
		default:
		}
		select {
		case <-release:
			_, _ = w.Write([]byte("page"))
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(page.Close)
	return page, requested, release
}

func TestFollowerFetchDoesNotBlockADeployAfterLeadershipChanges(t *testing.T) {
	page, entered, release := heldPage(t)
	h := newAcceptanceHarness(t, page.URL, false)
	followerDone := make(chan error, 1)
	followerStopped := make(chan struct{})
	followerCtx, cancelFollower := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancelFollower()
		<-followerStopped
	})
	go func() {
		defer close(followerStopped)
		_, err := h.coordinator.pipeline.Execute(followerCtx,
			h.coordinator.storeProvider, rendercontext.RenderModeReconcile)
		followerDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(testutil.EventTimeout):
		t.Fatal("follower did not begin its fetch")
	}

	h.reconcile("follower becomes leader while fetching")
	assert.Equal(t, "a=\n", h.deployedFor("follower becomes leader while fetching").config)
	close(release)
	select {
	case err := <-followerDone:
		require.NoError(t, err)
	case <-time.After(testutil.EventTimeout):
		t.Fatal("follower render did not settle")
	}
	h.reconcile("deploy after the follower accepted content")
	assert.Equal(t, "a=page\n", h.deployedFor("deploy after the follower accepted content").config)
	assert.True(t, h.service.FirstGraphPublished())
}

// The follower's render accepts the content between the leader's withholding
// render and its acceptance attempt, which then has nothing to accept (#363).
func TestContentAcceptedBeforeTheAttemptStartsStillDeploys(t *testing.T) {
	page, entered, release := heldPage(t)
	var requested <-chan busevents.Event
	cfg := acceptanceConfig()
	cfg.HAProxyConfig = config.HAProxyConfig{Template: `{{ http.Fetch("` + page.URL + `", map[string]any{"critical": true, "retries": 1, "timeout": "1s"}) }}`}
	h := newAcceptanceHarnessWithConfig(t, page.URL, cfg, func(bus *busevents.EventBus) {
		requested = bus.SubscribeTypes("acceptance-requests", 4, events.EventTypeHTTPContentAcceptanceRequested)
	})
	followerDone := make(chan error, 1)
	go func() {
		_, err := h.coordinator.pipeline.Execute(t.Context(), h.coordinator.storeProvider, rendercontext.RenderModeReconcile)
		followerDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(testutil.EventTimeout):
		t.Fatal("follower did not begin its fetch")
	}

	// Holding the ledger keeps the coordinator between the withholding render
	// and the attempt, as a busy leader is while the follower's commit lands.
	h.coordinator.ledger.mu.Lock()
	h.reconcile("leader withholds the unaccepted source")
	testutil.WaitForEvent[*events.HTTPContentAcceptanceRequestedEvent](t, requested, testutil.EventTimeout)
	close(release)
	select {
	case err := <-followerDone:
		require.NoError(t, err)
	case <-time.After(testutil.EventTimeout):
		t.Fatal("follower render did not settle")
	}
	h.coordinator.ledger.mu.Unlock()

	assert.Equal(t, "page\n", h.deployedFor("http_content_accepted").config)
}
