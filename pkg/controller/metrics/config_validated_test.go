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

package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
)

const (
	validatedTrue         = "True/ValidationSucceeded"
	validatedInvalid      = "False/ConfigInvalid"
	validatedHAProxy      = "False/HAProxyValidationFailed"
	validatedLoadGate     = "False/LoadGateFailed"
	validatedInconclusive = "Unknown/ValidationInconclusive"
)

type validatedFixture struct {
	t         *testing.T
	registry  *prometheus.Registry
	metrics   *Metrics
	component *Component
}

func newValidatedFixture(t *testing.T) *validatedFixture {
	t.Helper()
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	return &validatedFixture{
		t:         t,
		registry:  registry,
		metrics:   metrics,
		component: New(metrics, busevents.NewEventBus(16)),
	}
}

func (f *validatedFixture) handle(evts ...busevents.Event) {
	for _, event := range evts {
		f.component.handleEvent(event)
	}
}

// requireState asserts every known state is exported and only want is 1; an
// empty want means no state is 1.
func (f *validatedFixture) requireState(want string) {
	f.t.Helper()
	expected := map[string]float64{
		validatedTrue: 0, validatedInvalid: 0, validatedHAProxy: 0, validatedLoadGate: 0, validatedInconclusive: 0,
	}
	if want != "" {
		expected[want] = 1
	}
	require.Equal(f.t, expected, gatherSeries(f.t, f.registry, "haptic_config_validated", "status", "reason"))
}

func (f *validatedFixture) requireOutcomes(valid, invalid, inconclusive float64) {
	f.t.Helper()
	require.Equal(f.t,
		map[string]float64{"valid": valid, "invalid": invalid, "inconclusive": inconclusive},
		gatherSeries(f.t, f.registry, "haptic_config_validation_total", "result"))
}

// gatherSeries returns the family's samples keyed by its label values, joined
// with "/" in the order given.
func gatherSeries(t *testing.T, gatherer prometheus.Gatherer, name string, labels ...string) map[string]float64 {
	t.Helper()
	families, err := gatherer.Gather()
	require.NoError(t, err)
	series := make(map[string]float64)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			values := make(map[string]string)
			for _, pair := range metric.GetLabel() {
				values[pair.GetName()] = pair.GetValue()
			}
			key := ""
			for i, label := range labels {
				if i > 0 {
					key += "/"
				}
				key += values[label]
			}
			value := metric.GetGauge().GetValue()
			if metric.GetCounter() != nil {
				value = metric.GetCounter().GetValue()
			}
			series[key] = value
		}
	}
	return series
}

func parsed(version string) *events.ConfigParsedEvent {
	return events.NewConfigParsedEvent(nil, nil, version, "")
}

// announced is the running configuration re-announced without a verdict: the
// startup announcement, or a leadership replay.
func announced(version string) *events.ConfigValidatedEvent {
	return events.NewConfigValidatedEvent(nil, nil, version, "")
}

func validated(version string, candidateGeneration uint64) *events.ConfigValidatedEvent {
	event := events.NewConfigValidatedEvent(nil, nil, version, "")
	event.CandidateGeneration = candidateGeneration
	return event
}

func invalid(version string) *events.ConfigInvalidEvent {
	return events.NewConfigInvalidEvent(version, nil, map[string][]string{"template": {"unexpected EOF"}})
}

func inconclusive(version string) *events.ConfigValidationInconclusiveEvent {
	return events.NewConfigValidationInconclusiveEvent(version, nil,
		map[string][]string{"template": {"fetching schemas: context deadline exceeded"}}, time.Second)
}

func TestConfigValidationSeriesStartAtZero(t *testing.T) {
	f := newValidatedFixture(t)

	f.requireState("")
	f.requireOutcomes(0, 0, 0)
}

func TestRenderSeriesStartAtZero(t *testing.T) {
	f := newValidatedFixture(t)

	require.Equal(t, map[string]float64{"cold": 0, "warm": 0},
		gatherSeries(t, f.registry, "haptic_render_errors_total", "cache_state"))
	require.Equal(t, map[string]float64{"cold": 0, "warm": 0, "replay": 0},
		gatherSeries(t, f.registry, "haptic_render_total", "cache_state"))
}

func TestConfigRejectedSeriesStartAtZero(t *testing.T) {
	f := newValidatedFixture(t)

	require.Equal(t, map[string]float64{"basic": 0, "template": 0, "jsonpath": 0, "validationtests": 0, "coordinator": 0},
		gatherSeries(t, f.registry, "haptic_config_rejected_total", "validator"))
}

func TestConfigValidated_TrueUnknownTrue(t *testing.T) {
	f := newValidatedFixture(t)

	f.handle(events.NewConfigValidatedEvent(nil, nil, "initial", "initial"), parsed("v1"), announced("v1"))
	f.requireState(validatedTrue)
	f.requireOutcomes(0, 0, 0)

	f.handle(parsed("v2"), inconclusive("v2"))
	f.requireState(validatedInconclusive)
	f.handle(inconclusive("v2"))
	f.requireState(validatedInconclusive)
	f.requireOutcomes(0, 0, 2)

	f.handle(validated("v2", 1))
	f.requireState(validatedTrue)
	f.requireOutcomes(1, 0, 2)
}

func TestConfigValidated_TrueFalse(t *testing.T) {
	f := newValidatedFixture(t)

	f.handle(parsed("v1"), announced("v1"), parsed("v2"), invalid("v2"))

	f.requireState(validatedInvalid)
	f.requireOutcomes(0, 1, 0)
}

func TestConfigValidated_ReannouncementsDoNotOverrideAVerdict(t *testing.T) {
	f := newValidatedFixture(t)

	f.handle(parsed("v1"), announced("v1"), parsed("v2"), inconclusive("v2"))
	f.handle(events.NewBecameLeaderEvent("replica"), announced("v1"))
	f.requireState(validatedInconclusive)

	f.handle(invalid("v2"), announced("v2"))
	f.requireState(validatedInvalid)

	f.handle(parsed("v3"), validated("v3", 2), validated("v3", 2))
	f.requireState(validatedTrue)
	f.requireOutcomes(1, 1, 1)
}

func TestConfigValidated_NoVerdictWithoutTheNewestConfig(t *testing.T) {
	f := newValidatedFixture(t)
	restored := announced("v2")
	restored.ActiveSnapshotRestore = true

	f.handle(parsed("v2"), announced("v1"), restored)

	f.requireState("")
	f.requireOutcomes(0, 0, 0)
}

func TestConfigValidated_HAProxyRefusalOnTheLeader(t *testing.T) {
	f := newValidatedFixture(t)

	f.handle(events.NewValidationFailedEvent([]string{"unknown keyword"}, 1, "test"))
	f.requireState("")

	f.handle(parsed("v1"), announced("v1"), events.NewBecameLeaderEvent("replica"))
	f.handle(events.NewValidationFailedEvent([]string{"unknown keyword"}, 1, "test"))
	f.requireState(validatedHAProxy)

	f.handle(events.NewRenderGateCompletedEvent("plan-1", true, false, false, "", false, 1))
	f.requireState(validatedHAProxy)
	f.handle(events.NewRenderGateCompletedEvent("plan-2", true, false, true, "", false, 1))
	f.requireState(validatedTrue)

	f.handle(events.NewValidationFailedEvent([]string{"unknown keyword"}, 1, "test"))
	f.handle(events.NewLostLeadershipEvent("replica", events.LeadershipLostReasonLeaseLost))
	f.requireState(validatedTrue)

	f.handle(events.NewBecameLeaderEvent("replica"))
	f.handle(events.NewValidationFailedEvent([]string{"unknown keyword"}, 1, "test"))
	f.handle(parsed("v2"), inconclusive("v2"))
	f.requireState(validatedInconclusive)
}

func TestConfigValidated_LoadGate(t *testing.T) {
	f := newValidatedFixture(t)

	f.metrics.RecordLoadGate(false)
	f.requireState(validatedLoadGate)
	f.requireOutcomes(0, 1, 0)

	f.metrics.RecordLoadGate(true)
	f.requireState(validatedLoadGate)
	f.requireOutcomes(1, 1, 0)

	f.handle(parsed("v1"), announced("v1"))
	f.requireState(validatedTrue)
}

func TestConfigValidated_CarriedIntoTheNextIterationsRegistry(t *testing.T) {
	previous := newValidatedFixture(t)
	previous.handle(parsed("v1"), announced("v1"), parsed("v2"), invalid("v2"), parsed("v3"), inconclusive("v3"))

	next := newValidatedFixture(t)
	require.NoError(t, next.metrics.CarryConfigValidation(previous.metrics))
	next.requireState(validatedInconclusive)
	next.requireOutcomes(0, 1, 1)

	next.handle(parsed("v3"), inconclusive("v3"))
	next.requireState(validatedInconclusive)
	next.requireOutcomes(0, 1, 2)

	first := newValidatedFixture(t)
	require.NoError(t, first.metrics.CarryConfigValidation(nil))
	first.requireState("")
}

func TestComponent_SubscribesToConfigValidationEvents(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	bus := busevents.NewEventBus(16)
	component := New(metrics, bus)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bus.Start()
	go func() { _ = component.Start(ctx) }()

	bus.Publish(parsed("v1"))
	bus.Publish(announced("v1"))
	bus.Publish(inconclusive("v1"))

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.ConfigValidated.WithLabelValues("Unknown", "ValidationInconclusive")) == 1 &&
			testutil.ToFloat64(metrics.ConfigValidated.WithLabelValues("True", "ValidationSucceeded")) == 0
	}, 5*time.Second, 10*time.Millisecond)
}
