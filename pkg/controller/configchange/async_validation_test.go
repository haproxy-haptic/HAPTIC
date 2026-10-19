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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
)

// TestConfigChangeHandler_BecameLeaderReplay_NotBlockedByInFlightValidation
// pins the issue #55 publish-stall mechanism (CI jobs 15179436894,
// 15180658393, 15180832368): after a CRD-triggered reinitialization, the new
// iteration's config validation runs 13-15s+ (the validationtests validator
// executes the config's entire embedded suite). When leadership was acquired
// during that window, the BecameLeaderEvent sat queued behind the event loop
// — which was blocked inside the validation scatter-gather — so the state
// replay that hands the last validated config to the freshly-subscribed
// leader-only config-publisher never ran. The publisher dropped every
// HAProxyCfg publish with "Cannot publish configuration, missing cached
// state" until validation completed, blowing the e2e convergence budget
// (waitForControllerDeployed: marker never observed in spec.content).
//
// The pin: with a validation deliberately held in flight (a required
// validator that never responds), a BecameLeaderEvent must still be replayed
// promptly. Before the async-validation fix this timed out (the replay
// would only happen after the 45s scatter-gather timeout).
func TestConfigChangeHandler_BecameLeaderReplay_NotBlockedByInFlightValidation(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	// One expected validator that never responds: the scatter-gather stays in
	// flight for the whole test (stand-in for a long validationtests run).
	handler := NewConfigChangeHandler(bus, logger, configCh, []string{"slow"}, testDebounceInterval)

	eventChan := bus.Subscribe("test-sub", 50)
	validatorChan := bus.Subscribe("test-validator", 50)
	bus.Start()

	go handler.Start(t.Context())
	time.Sleep(testutil.StartupDelay)

	// Cache a validated config for leadership replay (production equivalent:
	// the synthetic bootstrap ConfigValidatedEvent every iteration publishes
	// at startup). Consume its delivery on eventChan so the replay below is
	// unambiguously a SECOND publish of the same version.
	cached := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(cached, nil, "initial", ""))
	first := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, eventChan, testutil.LongTimeout)
	assert.Equal(t, "initial", first.Version)

	// Start a validation that will not complete within the test.
	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "v2", "sv2"))
	testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)

	// The validation is now in flight. The loop must still process side
	// events: leadership acquisition must replay the cached config promptly,
	// not after the scatter-gather finishes.
	bus.Publish(events.NewBecameLeaderEvent("test-identity"))

	replayed := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, eventChan, testutil.LongTimeout)
	assert.Equal(t, "initial", replayed.Version,
		"BecameLeaderEvent must replay the cached validated config while a validation is in flight")
}

func TestConfigChangeHandler_SupersededValidationCannotArmRestart(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	debounce := 40 * time.Millisecond
	handler := NewConfigChangeHandler(bus, logger, configCh, []string{"gated"}, debounce)

	initialConfig := &coreconfig.Config{}
	credentials := &coreconfig.Credentials{DataplaneUsername: "current"}
	handler.SetInitialSnapshot(&ValidatedSnapshot{
		RawConfig:          initialConfig,
		Config:             initialConfig,
		TemplateConfig:     &v1alpha1.HAProxyTemplateConfig{},
		ConfigVersion:      "initial-version",
		Credentials:        credentials,
		CredentialsVersion: "secret-1",
	})

	rawA := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "raw-a"}}
	rawB := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "raw-b"}}
	effectiveA := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "effective-a"}}
	effectiveB := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "effective-b"}}
	resolutionA := &coreconfig.Resolution{ResolvedVersions: map[string]string{"routes": "example.io/v1"}}
	resolutionB := &coreconfig.Resolution{ResolvedVersions: map[string]string{"routes": "example.io/v2"}}
	handler.SetEffectiveResolver(func(cfg *coreconfig.Config) (*ResolvedConfig, error) {
		switch cfg {
		case rawA:
			return &ResolvedConfig{Config: effectiveA, Resolution: resolutionA}, nil
		case rawB:
			return &ResolvedConfig{Config: effectiveB, Resolution: resolutionB}, nil
		default:
			return nil, assert.AnError
		}
	})

	validatorChan := bus.Subscribe("gated-validator", 20)
	resultChan := bus.Subscribe("result-observer", 20)
	bus.Start()
	go handler.Start(t.Context())
	handler.EnableReinitialization()

	templateB := &v1alpha1.HAProxyTemplateConfig{}
	eventA := events.NewConfigParsedEvent(rawA, &v1alpha1.HAProxyTemplateConfig{}, "a", "")
	eventB := events.NewConfigParsedEvent(rawB, templateB, "b", "")
	eventB.Sources = []events.ConfigSourceRef{{Name: "config", Generation: 2}}

	bus.Publish(eventA)
	requestA := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	bus.Publish(eventB)
	bus.Publish(events.NewConfigValidationResponse(requestA.RequestID(), "gated", true, nil))

	requestB := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	time.Sleep(debounce * 2)
	select {
	case reload := <-configCh:
		t.Fatalf("superseded A armed a restart with %q", reload.Snapshot.ConfigVersion)
	default:
	}

	bus.Publish(events.NewConfigValidationResponse(requestB.RequestID(), "gated", false, []string{"rejected"}))
	invalidB := testutil.WaitForEvent[*events.ConfigInvalidEvent](t, resultChan, testutil.LongTimeout)
	require.Equal(t, "b", invalidB.Version)
	time.Sleep(debounce * 2)
	select {
	case reload := <-configCh:
		t.Fatalf("invalid B armed a restart with %q", reload.Snapshot.ConfigVersion)
	default:
	}

	bus.Publish(eventB)
	validRequestB := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	bus.Publish(events.NewConfigValidationResponse(validRequestB.RequestID(), "gated", true, nil))

	select {
	case reload := <-configCh:
		require.NotNil(t, reload.Snapshot)
		assert.Same(t, rawB, reload.Snapshot.RawConfig)
		assert.Same(t, effectiveB, reload.Snapshot.Config)
		assert.Same(t, resolutionB, reload.Snapshot.Resolution)
		assert.Same(t, templateB, reload.Snapshot.TemplateConfig)
		assert.Same(t, credentials, reload.Snapshot.Credentials)
		assert.Equal(t, "secret-1", reload.Snapshot.CredentialsVersion)
		assert.Equal(t, eventB.Sources, reload.Snapshot.Sources)
	case <-time.After(testutil.LongTimeout):
		t.Fatal("timeout waiting for latest valid B reload")
	}
}

func TestConfigChangeHandler_NewerParsedConfigRestoresActiveStateAndRetiresRestart(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	debounce := 80 * time.Millisecond
	handler := NewConfigChangeHandler(bus, logger, configCh, []string{"gated"}, debounce)
	activeA := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{Port: 5555}}
	activeTemplate := &v1alpha1.HAProxyTemplateConfig{}
	handler.SetInitialSnapshot(&ValidatedSnapshot{
		RawConfig:          activeA,
		Config:             activeA,
		TemplateConfig:     activeTemplate,
		ConfigVersion:      "active-a",
		Credentials:        &coreconfig.Credentials{},
		CredentialsVersion: "secret-1",
	})

	validatorChan := bus.Subscribe("gated-validator", 20)
	resultChan := bus.Subscribe("result-observer", 20)
	bus.Start()
	go handler.Start(t.Context())
	handler.EnableReinitialization()

	candidateB := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{Port: 6666}}
	bus.Publish(events.NewConfigParsedEvent(
		candidateB, &v1alpha1.HAProxyTemplateConfig{}, "candidate-b", ""))
	requestB := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	bus.Publish(events.NewConfigValidationResponse(requestB.RequestID(), "gated", true, nil))
	validatedB := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, resultChan, testutil.LongTimeout)
	require.Equal(t, "candidate-b", validatedB.Version)
	require.NotZero(t, validatedB.CandidateGeneration)

	invalidC := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{Port: 7777}}
	bus.Publish(events.NewConfigParsedEvent(
		invalidC, &v1alpha1.HAProxyTemplateConfig{}, "invalid-c", ""))
	requestC := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	restoredA := testutil.WaitForEventWithPredicate(t, resultChan, testutil.LongTimeout,
		func(event *events.ConfigValidatedEvent) bool { return event.ActiveSnapshotRestore })
	assert.Same(t, activeA, restoredA.Config)
	assert.Same(t, activeTemplate, restoredA.TemplateConfig)
	assert.Equal(t, "active-a", restoredA.Version)
	assert.Zero(t, restoredA.CandidateGeneration)

	time.Sleep(debounce * 2)
	select {
	case reload := <-configCh:
		t.Fatalf("invalid C did not retire B's armed restart: got %q", reload.Snapshot.ConfigVersion)
	default:
	}

	bus.Publish(events.NewConfigValidationResponse(requestC.RequestID(), "gated", false, []string{"rejected"}))
	invalidEventC := testutil.WaitForEvent[*events.ConfigInvalidEvent](t, resultChan, testutil.LongTimeout)
	require.Equal(t, "invalid-c", invalidEventC.Version)
}

func TestConfigChangeHandler_InvalidConfigDoesNotCancelCredentialRotation(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	handler := NewConfigChangeHandler(bus, logger, configCh, []string{"gated"}, 200*time.Millisecond)
	activeConfig := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "active"}}
	handler.SetInitialSnapshot(&ValidatedSnapshot{
		RawConfig:          activeConfig,
		Config:             activeConfig,
		TemplateConfig:     &v1alpha1.HAProxyTemplateConfig{},
		ConfigVersion:      "active",
		Credentials:        &coreconfig.Credentials{DataplaneUsername: "old"},
		CredentialsVersion: "secret-old",
	})

	validatorChan := bus.Subscribe("gated-validator", 20)
	resultChan := bus.Subscribe("result-observer", 20)
	bus.Start()
	go handler.Start(t.Context())
	handler.EnableReinitialization()

	rotated := &coreconfig.Credentials{DataplaneUsername: "rotated"}
	bus.Publish(events.NewCredentialsUpdatedEvent(rotated, "secret-new"))
	require.Eventually(t, func() bool {
		return handler.credentialsVersion() == "secret-new"
	}, testutil.LongTimeout, time.Millisecond)
	require.Eventually(t, func() bool {
		return len(configCh) == 1
	}, testutil.LongTimeout, time.Millisecond)

	invalidConfig := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "invalid"}}
	bus.Publish(events.NewConfigParsedEvent(
		invalidConfig, &v1alpha1.HAProxyTemplateConfig{}, "invalid", ""))
	request := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	bus.Publish(events.NewConfigValidationResponse(request.RequestID(), "gated", false, []string{"rejected"}))
	testutil.WaitForEvent[*events.ConfigInvalidEvent](t, resultChan, testutil.LongTimeout)

	select {
	case reload := <-configCh:
		assert.Same(t, activeConfig, reload.Snapshot.Config)
		assert.Same(t, rotated, reload.Snapshot.Credentials)
		assert.Equal(t, "secret-new", reload.Snapshot.CredentialsVersion)
		assert.Equal(t, ReloadReasonCredentials, reload.Reasons)
	case <-time.After(testutil.LongTimeout):
		t.Fatal("invalid config candidate canceled the independent credentials reload")
	}
}

func TestConfigChangeHandler_CredentialRotationUsesActiveSnapshotAfterSupersededCandidate(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	handler := NewConfigChangeHandler(bus, logger, configCh, []string{"gated"}, 200*time.Millisecond)
	activeConfig := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "active"}}
	handler.SetInitialSnapshot(&ValidatedSnapshot{
		RawConfig:          activeConfig,
		Config:             activeConfig,
		TemplateConfig:     &v1alpha1.HAProxyTemplateConfig{},
		ConfigVersion:      "active",
		Credentials:        &coreconfig.Credentials{DataplaneUsername: "old"},
		CredentialsVersion: "secret-old",
	})

	validatorChan := bus.Subscribe("gated-validator", 20)
	resultChan := bus.Subscribe("result-observer", 20)
	bus.Start()
	go handler.Start(t.Context())
	handler.EnableReinitialization()

	accepted := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "accepted-but-not-active"}}
	bus.Publish(events.NewConfigParsedEvent(
		accepted, &v1alpha1.HAProxyTemplateConfig{}, "accepted", ""))
	acceptedRequest := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	bus.Publish(events.NewConfigValidationResponse(acceptedRequest.RequestID(), "gated", true, nil))
	testutil.WaitForEvent[*events.ConfigValidatedEvent](t, resultChan, testutil.LongTimeout)
	require.Eventually(t, func() bool {
		return len(configCh) == 1
	}, testutil.LongTimeout, time.Millisecond)

	newerInvalid := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "newer-invalid"}}
	bus.Publish(events.NewConfigParsedEvent(
		newerInvalid, &v1alpha1.HAProxyTemplateConfig{}, "newer-invalid", ""))
	invalidRequest := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	assert.Empty(t, configCh, "the newer parsed candidate must retract the queued accepted candidate")
	bus.Publish(events.NewConfigValidationResponse(invalidRequest.RequestID(), "gated", false, []string{"rejected"}))
	testutil.WaitForEvent[*events.ConfigInvalidEvent](t, resultChan, testutil.LongTimeout)

	rotated := &coreconfig.Credentials{DataplaneUsername: "rotated"}
	bus.Publish(events.NewCredentialsUpdatedEvent(rotated, "secret-new"))
	select {
	case reload := <-configCh:
		assert.Same(t, activeConfig, reload.Snapshot.Config,
			"an independent reload must not resurrect a superseded candidate")
		assert.Same(t, rotated, reload.Snapshot.Credentials)
		assert.Equal(t, ReloadReasonCredentials, reload.Reasons)
	case <-time.After(testutil.LongTimeout):
		t.Fatal("timeout waiting for credentials reload")
	}
}

func TestConfigChangeHandler_SupersededCandidateCannotSurviveEffectiveReload(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	handler := NewConfigChangeHandler(bus, logger, configCh, []string{"gated"}, time.Hour)
	activeConfig := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "active"}}
	handler.SetInitialSnapshot(&ValidatedSnapshot{
		RawConfig:          activeConfig,
		Config:             activeConfig,
		ConfigVersion:      "active",
		Credentials:        &coreconfig.Credentials{DataplaneUsername: "active"},
		CredentialsVersion: "secret-active",
	})

	validatorChan := bus.Subscribe("gated-validator", 20)
	resultChan := bus.Subscribe("result-observer", 20)
	bus.Start()
	go handler.Start(t.Context())
	handler.EnableReinitialization()

	accepted := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "accepted"}}
	bus.Publish(events.NewConfigParsedEvent(
		accepted, &v1alpha1.HAProxyTemplateConfig{}, "accepted", ""))
	acceptedRequest := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	bus.Publish(events.NewConfigValidationResponse(acceptedRequest.RequestID(), "gated", true, nil))
	testutil.WaitForEvent[*events.ConfigValidatedEvent](t, resultChan, testutil.LongTimeout)
	handler.RequestEffectiveReload()
	require.Eventually(t, func() bool {
		return len(configCh) == 1
	}, testutil.LongTimeout, time.Millisecond)

	newerInvalid := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "newer-invalid"}}
	bus.Publish(events.NewConfigParsedEvent(
		newerInvalid, &v1alpha1.HAProxyTemplateConfig{}, "newer-invalid", ""))
	invalidRequest := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	bus.Publish(events.NewConfigValidationResponse(invalidRequest.RequestID(), "gated", false, []string{"rejected"}))
	testutil.WaitForEvent[*events.ConfigInvalidEvent](t, resultChan, testutil.LongTimeout)

	select {
	case reload := <-configCh:
		assert.Same(t, activeConfig, reload.Snapshot.Config)
		assert.Equal(t, ReloadReasonEffectiveConfig, reload.Reasons)
	case <-time.After(testutil.LongTimeout):
		t.Fatal("superseding the config candidate discarded the effective-config reload")
	}
}

func TestConfigChangeHandler_NewerCredentialEventRetiresStartupReplay(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)
	activeConfig := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "active"}}
	handler.SetInitialSnapshot(&ValidatedSnapshot{
		RawConfig:          activeConfig,
		Config:             activeConfig,
		ConfigVersion:      "active",
		Credentials:        &coreconfig.Credentials{DataplaneUsername: "old"},
		CredentialsVersion: "secret-old",
	})

	startupCredentials := &coreconfig.Credentials{DataplaneUsername: "startup"}
	handler.handleSecretRotation(events.NewCredentialsUpdatedEvent(startupCredentials, "secret-startup"))
	handler.EnableReinitialization()

	newerCredentials := &coreconfig.Credentials{DataplaneUsername: "newer"}
	handler.handleSecretRotation(events.NewCredentialsUpdatedEvent(newerCredentials, "secret-newer"))
	go handler.Start(t.Context())

	select {
	case reload := <-configCh:
		assert.Same(t, activeConfig, reload.Snapshot.Config)
		assert.Same(t, newerCredentials, reload.Snapshot.Credentials)
		assert.Equal(t, "secret-newer", reload.Snapshot.CredentialsVersion)
	case <-time.After(testutil.LongTimeout):
		t.Fatal("timeout waiting for the latest credentials reload")
	}
}

// TestConfigChangeHandler_InFlightValidation_SupersededParsedCoalesced pins
// the single-flight + latest-wins semantics of asynchronous validation.
func TestConfigChangeHandler_InFlightValidation_SupersededParsedCoalesced(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	handler := NewConfigChangeHandler(bus, logger, configCh, []string{"gated"}, testDebounceInterval)

	eventChan := bus.Subscribe("test-sub", 50)
	validatorChan := bus.Subscribe("test-validator", 50)
	bus.Start()

	go handler.Start(t.Context())

	// Mock validator: holds the FIRST request until the gate opens, then
	// responds valid; every later request is answered immediately.
	gate := make(chan struct{})
	firstReceived := make(chan struct{})
	go func() {
		first := true
		for event := range validatorChan {
			req, ok := event.(*events.ConfigValidationRequest)
			if !ok {
				continue
			}
			if first {
				first = false
				close(firstReceived)
				<-gate
			}
			bus.Publish(events.NewConfigValidationResponse(req.RequestID(), "gated", true, nil))
		}
	}()

	time.Sleep(testutil.StartupDelay)

	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "v1", "sv1"))
	select {
	case <-firstReceived:
	case <-time.After(testutil.LongTimeout):
		t.Fatal("timeout waiting for the first validation request to reach the validator")
	}

	// v1's validation is now in flight and gated. Two newer configs arrive;
	// v2 is superseded by v3 before any validation slot frees up.
	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "v2", "sv2"))
	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "v3", "sv3"))
	time.Sleep(testutil.StartupDelay) // let the loop park them (latest wins)

	close(gate)

	got := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, eventChan, testutil.VeryLongTimeout)
	assert.Equal(t, "v3", got.Version, "only the latest parked config may be accepted")

	// Neither superseded candidate may produce a ConfigValidatedEvent.
	testutil.AssertNoEvent[*events.ConfigValidatedEvent](t, eventChan, testutil.NoEventTimeout)
}

// The watchers of a fresh iteration deliver the configuration that iteration
// was built from, and the loader parses it again. Validating it costs the
// whole validationTests suite per replica, twice, after every reload
// (measured: 2 x 32 s at four CPUs), for a verdict the handler then discards
// as "matches the initial version". The version check belongs before the
// suite, not after it.
func TestConfigChangeHandler_DoesNotRevalidateTheVersionTheIterationStartedFrom(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	handler := NewConfigChangeHandler(bus, logger, configCh, []string{"validationtests"}, testDebounceInterval)
	validatorChan := bus.Subscribe("test-validator", 50)
	bus.Start()
	go handler.Start(t.Context())
	time.Sleep(testutil.StartupDelay)

	running := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "running"}}
	handler.SetInitialSnapshot(&ValidatedSnapshot{Config: running, ConfigVersion: "v-initial"})
	handler.EnableReinitialization()
	resultChan := bus.Subscribe("result-observer", 20)

	template := &v1alpha1.HAProxyTemplateConfig{}
	parsed := events.NewConfigParsedEvent(&coreconfig.Config{}, template, "v-initial", "sv1")
	parsed.Sources = []events.ConfigSourceRef{{Name: "haptic-config", Generation: 3}}
	bus.Publish(parsed)
	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, template, "v-initial", "sv1"))
	testutil.AssertNoEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.NoEventTimeout)

	// The status updater stamps Validated from this event, so the running
	// version is still announced, once, with the sources the parse carried.
	announced := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, resultChan, testutil.LongTimeout)
	assert.Equal(t, "v-initial", announced.Version)
	assert.Same(t, running, announced.Config, "the effective config, not the raw parse")
	assert.Same(t, template, announced.TemplateConfig)
	assert.Equal(t, parsed.Sources, announced.Sources)
	testutil.AssertNoEvent[*events.ConfigValidatedEvent](t, resultChan, testutil.NoEventTimeout)
	assert.Empty(t, configCh, "the running version must not restart the iteration")

	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "v-next", "sv1"))
	request := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	assert.Equal(t, "v-next", request.Version)
}

// The drain path sees the same parses when they arrive while the loop is on
// another select case, which is exactly the window right after a rebuild.
// Dropping the running version there without announcing it would leave the
// sources unstamped whenever the sole delivery lands in that window.
func TestConfigChangeHandler_DrainAnnouncesTheRunningVersion(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	handler := NewConfigChangeHandler(bus, logger, make(chan *ReloadRequest, 1), []string{"validationtests"}, testDebounceInterval)
	running := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "running"}}
	handler.SetInitialSnapshot(&ValidatedSnapshot{Config: running, ConfigVersion: "v-initial"})
	handler.EnableReinitialization()
	resultChan := bus.Subscribe("result-observer", 20)
	bus.Start()

	// The loop is not running: the parse waits in the subscription channel
	// the way it does while the loop is busy on another case.
	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, &v1alpha1.HAProxyTemplateConfig{}, "v-initial", "sv1"))
	require.Eventually(t, func() bool { return len(handler.eventChan) == 1 }, testutil.LongTimeout, time.Millisecond)
	handler.drainQueuedEvents()

	announced := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, resultChan, testutil.LongTimeout)
	assert.Equal(t, "v-initial", announced.Version)
	assert.Same(t, running, announced.Config)
	assert.Nil(t, handler.queuedParsed, "the running version must not be parked for validation")
}

// A served-CRD change used to restart the iteration at once and let the new
// one run the load gate with nothing serving (34 s measured). The outgoing
// iteration validates the freshly resolved config first and the restart
// adopts it as validated.
func TestConfigChangeHandler_EffectiveReloadValidatesOnTheOutgoingIteration(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	handler := NewConfigChangeHandler(bus, logger, configCh, []string{"gated"}, testDebounceInterval)
	raw := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "raw"}}
	active := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "active"}}
	handler.SetInitialSnapshot(&ValidatedSnapshot{
		RawConfig: raw, Config: active, ConfigVersion: "active",
		Resolution: &coreconfig.Resolution{ResolvedVersions: map[string]string{"routes": "example.io/v1"}},
	})
	fresh := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "fresh"}}
	freshResolution := &coreconfig.Resolution{ResolvedVersions: map[string]string{"routes": "example.io/v2"}}
	handler.SetEffectiveResolver(func(cfg *coreconfig.Config) (*ResolvedConfig, error) {
		require.Same(t, raw, cfg, "the raw config is what gets re-resolved")
		return &ResolvedConfig{Config: fresh, Resolution: freshResolution}, nil
	})
	validatorChan := bus.Subscribe("gated-validator", 20)
	bus.Start()
	go handler.Start(t.Context())
	handler.EnableReinitialization()

	handler.RequestEffectiveReload()
	request := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	assert.Same(t, fresh, request.Config, "validators judge the re-resolved config")
	assert.Empty(t, configCh, "no restart before the verdict")
	bus.Publish(events.NewConfigValidationResponse(request.RequestID(), "gated", true, nil))

	select {
	case reload := <-configCh:
		assert.Equal(t, ReloadReasonConfig, reload.Reasons, "validated: the new iteration must not run the load gate")
		assert.Same(t, fresh, reload.Snapshot.Config)
		assert.Same(t, freshResolution, reload.Snapshot.Resolution)
		assert.Same(t, raw, reload.Snapshot.RawConfig)
	case <-time.After(testDebounceInterval + testutil.LongTimeout):
		t.Fatal("a validated effective reload must restart the iteration")
	}
}

// When the re-resolved config fails validation the restart still happens the
// old way: the new iteration re-resolves and runs the load gate, which is
// what reports a required resource that lost its CRD.
func TestConfigChangeHandler_EffectiveReloadFallsBackToTheGateWhenInvalid(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	handler := NewConfigChangeHandler(bus, logger, configCh, []string{"gated"}, testDebounceInterval)
	raw := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "raw"}}
	active := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "active"}}
	handler.SetInitialSnapshot(&ValidatedSnapshot{RawConfig: raw, Config: active, ConfigVersion: "active"})
	handler.SetEffectiveResolver(func(*coreconfig.Config) (*ResolvedConfig, error) {
		return &ResolvedConfig{Config: active, Resolution: &coreconfig.Resolution{}}, nil
	})
	validatorChan := bus.Subscribe("gated-validator", 20)
	bus.Start()
	go handler.Start(t.Context())
	handler.EnableReinitialization()

	handler.RequestEffectiveReload()
	request := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
	bus.Publish(events.NewConfigValidationResponse(request.RequestID(), "gated", false, []string{"rejected"}))

	select {
	case reload := <-configCh:
		assert.Equal(t, ReloadReasonEffectiveConfig, reload.Reasons)
		assert.Same(t, active, reload.Snapshot.Config)
	case <-time.After(testutil.LongTimeout):
		t.Fatal("a rejected effective reload must still restart through the load gate")
	}
}

// A configuration change that arrives while the effective validation is
// pending supersedes it. Its own validation resolves afresh, so a valid
// verdict restarts with that config and resolution, validated; an invalid one
// must not lose the owed restart: the active config restarts through the
// gate, as a served-resource change always did.
func TestConfigChangeHandler_ConfigChangeDuringEffectiveValidationStillRestarts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		valid   bool
		reasons ReloadReason
	}{
		{name: "valid newer config restarts validated", valid: true, reasons: ReloadReasonConfig},
		{name: "invalid newer config restarts the active config through the gate", valid: false, reasons: ReloadReasonEffectiveConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus, logger := testutil.NewTestBusAndLogger()
			configCh := make(chan *ReloadRequest, 1)
			handler := NewConfigChangeHandler(bus, logger, configCh, []string{"gated"}, testDebounceInterval)
			raw := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "raw"}}
			active := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "active"}}
			newer := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "newer"}}
			newerEffective := &coreconfig.Config{Dataplane: coreconfig.DataplaneConfig{MapsDir: "newer-effective"}}
			newerResolution := &coreconfig.Resolution{ResolvedVersions: map[string]string{"routes": "example.io/v2"}}
			handler.SetInitialSnapshot(&ValidatedSnapshot{RawConfig: raw, Config: active, ConfigVersion: "active"})
			handler.SetEffectiveResolver(func(cfg *coreconfig.Config) (*ResolvedConfig, error) {
				if cfg == newer {
					return &ResolvedConfig{Config: newerEffective, Resolution: newerResolution}, nil
				}
				return &ResolvedConfig{Config: active, Resolution: &coreconfig.Resolution{}}, nil
			})
			validatorChan := bus.Subscribe("gated-validator", 20)
			bus.Start()
			go handler.Start(t.Context())
			handler.EnableReinitialization()

			handler.RequestEffectiveReload()
			first := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
			assert.Equal(t, "active", first.Version)
			bus.Publish(events.NewConfigParsedEvent(newer, &v1alpha1.HAProxyTemplateConfig{}, "newer", ""))
			bus.Publish(events.NewConfigValidationResponse(first.RequestID(), "gated", true, nil))
			second := testutil.WaitForEvent[*events.ConfigValidationRequest](t, validatorChan, testutil.LongTimeout)
			assert.Equal(t, "newer", second.Version, "the newer config supersedes the effective candidate")
			assert.Empty(t, configCh, "the superseded effective verdict must not restart")
			bus.Publish(events.NewConfigValidationResponse(second.RequestID(), "gated", tc.valid, []string{"verdict"}))

			select {
			case reload := <-configCh:
				assert.Equal(t, tc.reasons, reload.Reasons)
				if tc.valid {
					assert.Same(t, newerEffective, reload.Snapshot.Config)
					assert.Same(t, newerResolution, reload.Snapshot.Resolution)
				} else {
					assert.Same(t, active, reload.Snapshot.Config)
				}
			case <-time.After(testDebounceInterval + testutil.LongTimeout):
				t.Fatal("the owed restart was lost")
			}
		})
	}
}
