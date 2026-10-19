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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
)

// testDebounceInterval is a short debounce interval for tests.
// Using a short interval (50ms) keeps tests fast while still exercising debounce logic.
const testDebounceInterval = 50 * time.Millisecond

func TestNewConfigChangeHandler(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	validators := []string{"basic", "template"}

	handler := NewConfigChangeHandler(bus, logger, configCh, validators, testDebounceInterval)

	require.NotNil(t, handler)
	assert.Equal(t, bus, handler.eventBus)
	assert.NotNil(t, handler.eventChan) // Event channel subscribed in constructor
	assert.NotNil(t, handler.logger)    // Logger is enhanced with component name
	// Can't directly compare bidirectional channel to send-only channel, just verify it's set
	assert.NotNil(t, handler.configChangeCh)
	assert.Equal(t, validators, handler.validators)
	assert.Equal(t, testDebounceInterval, handler.debounceInterval)
}

func TestConfigChangeHandler_StartWithContextCancel(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)
	bus.Start()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		handler.Start(ctx)
		close(done)
	}()

	time.Sleep(testutil.StartupDelay)
	cancel()

	select {
	case <-done:
		// Success
	case <-time.After(testutil.LongTimeout):
		t.Fatal("handler did not stop in time after context cancel")
	}
}

func TestConfigChangeHandler_HandleConfigParsed_NoValidators(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	// No validators configured
	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)

	eventChan := bus.Subscribe("test-sub", 50)
	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// Publish ConfigParsedEvent
	testConfig := &coreconfig.Config{}
	bus.Publish(events.NewConfigParsedEvent(testConfig, nil, "v1", "sv1"))

	// Should immediately publish ConfigValidatedEvent (no validation needed)
	validated := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, eventChan, testutil.LongTimeout)
	assert.Equal(t, "v1", validated.Version)
	assert.Equal(t, testConfig, validated.Config)
}

func TestConfigChangeHandler_CoalescesSupersededConfigParsed(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	// No validators: handleConfigParsed coalesces, then short-circuits to
	// publishValidated — so the published ConfigValidatedEvent identifies which
	// config actually got validated.
	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)

	eventChan := bus.Subscribe("test-sub", 50)

	cfg1 := &coreconfig.Config{}
	cfg2 := &coreconfig.Config{}
	// Publish both BEFORE bus.Start() so they're flushed into the handler's
	// subscription buffer together; when the handler picks up v1, v2 is already
	// queued and must be coalesced ahead of.
	bus.Publish(events.NewConfigParsedEvent(cfg1, nil, "v1", "sv1"))
	bus.Publish(events.NewConfigParsedEvent(cfg2, nil, "v2", "sv2"))
	bus.Start()

	go handler.Start(t.Context())

	// Only the latest config (v2) should be validated.
	validated := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, eventChan, testutil.LongTimeout)
	assert.Equal(t, "v2", validated.Version, "handler should validate the latest config, not the superseded one")
	assert.Same(t, cfg2, validated.Config)

	// The superseded config (v1) must NOT also be validated.
	deadline := time.After(testutil.NoEventTimeout)
	for {
		select {
		case ev := <-eventChan:
			if cv, ok := ev.(*events.ConfigValidatedEvent); ok {
				t.Fatalf("superseded config produced a second ConfigValidatedEvent (version %s)", cv.Version)
			}
		case <-deadline:
			return
		}
	}
}

func TestConfigChangeHandler_HandleConfigValidated_SignalController(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)

	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// Enable reinitialization (simulating startup complete)
	handler.EnableReinitialization()

	// Publish ConfigValidatedEvent (actual config change)
	testConfig := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(testConfig, nil, "v2", "sv2"))

	// Wait for debounce
	time.Sleep(testDebounceInterval + 50*time.Millisecond)

	// Should signal controller reinitialization
	select {
	case reload := <-configCh:
		assert.Same(t, testConfig, reload.Snapshot.Config)
	case <-time.After(testutil.LongTimeout):
		t.Fatal("timeout waiting for config signal")
	}
}

func TestConfigChangeHandler_HandleConfigValidated_InitialVersion_SkipsSignal(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)

	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// Publish ConfigValidatedEvent with version="initial"
	testConfig := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(testConfig, nil, "initial", ""))

	// Should NOT signal controller (initial version is skipped)
	select {
	case <-configCh:
		t.Fatal("unexpected config signal for initial version")
	case <-time.After(testutil.NoEventTimeout):
		// Expected - no signal
	}
}

func TestConfigChangeHandler_HandleConfigValidated_InvalidConfigType(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)

	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// Publish ConfigValidatedEvent with invalid config type
	bus.Publish(events.NewConfigValidatedEvent("not-a-config", nil, "v2", ""))

	// Should NOT signal controller (invalid type)
	select {
	case <-configCh:
		t.Fatal("unexpected config signal for invalid config type")
	case <-time.After(testutil.NoEventTimeout):
		// Expected - no signal
	}
}

func TestConfigChangeHandler_ActiveSnapshotRestoreDoesNotReload(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)
	handler.EnableReinitialization()
	bus.Start()
	go handler.Start(t.Context())

	restore := events.NewConfigValidatedEvent(&coreconfig.Config{}, nil, "active", "secret")
	restore.ActiveSnapshotRestore = true
	bus.Publish(restore)

	time.Sleep(testDebounceInterval * 2)
	select {
	case reload := <-configCh:
		t.Fatalf("active snapshot restore triggered reload with version %q", reload.Snapshot.ConfigVersion)
	default:
	}
}

func TestConfigChangeHandler_HandleConfigValidated_ChannelFull(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	// Channel with no buffer
	configCh := make(chan *ReloadRequest)

	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)

	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// Publish ConfigValidatedEvent - should not block even if channel is full
	testConfig := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(testConfig, nil, "v2", ""))

	// Give it time to process - should not hang
	time.Sleep(testutil.NoEventTimeout)
}

func TestConfigChangeHandler_HandleBecameLeader_NoValidatedConfig(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)

	eventChan := bus.Subscribe("test-sub", 50)
	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// Publish BecameLeaderEvent without any prior config
	bus.Publish(events.NewBecameLeaderEvent("test-identity"))

	// Should NOT publish any ConfigValidatedEvent (no config cached)
	testutil.AssertNoEvent[*events.ConfigValidatedEvent](t, eventChan, testutil.NoEventTimeout)
}

func TestConfigChangeHandler_HandleBecameLeader_WithValidatedConfig(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)

	eventChan := bus.Subscribe("test-sub", 50)
	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// First, publish a ConfigValidatedEvent to cache it
	testConfig := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(testConfig, nil, "initial", ""))
	time.Sleep(testutil.DebounceWait)

	testutil.DrainChannel(eventChan)

	bus.Publish(events.NewBecameLeaderEvent("test-identity"))

	// Should re-publish the cached ConfigValidatedEvent
	validated := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, eventChan, testutil.LongTimeout)
	assert.Equal(t, "initial", validated.Version)
}

func TestConfigChangeHandler_StateCaching(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)

	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// Initially no cached config
	assert.False(t, handler.configReplayer.HasState())

	// Publish ConfigParsedEvent (with no validators, will be immediately validated)
	testConfig := &coreconfig.Config{}
	bus.Publish(events.NewConfigParsedEvent(testConfig, nil, "v1", "sv1"))
	time.Sleep(testutil.DebounceWait)

	// Should now have cached config
	assert.True(t, handler.configReplayer.HasState())
	cached, ok := handler.configReplayer.Get()
	require.True(t, ok)
	assert.Equal(t, "v1", cached.Version)
}

func TestConfigChangeHandler_IgnoresOtherEvents(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)
	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// Publish unrelated event - should not cause any issues
	bus.Publish(events.NewCredentialsUpdatedEvent(nil, "v1"))

	// Handler should continue running
	time.Sleep(testutil.DebounceWait)
}

func TestConfigChangeHandler_HandleConfigParsed_WithValidators_AllValid(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	// Configure validators
	validators := []string{"basic", "template"}
	handler := NewConfigChangeHandler(bus, logger, configCh, validators, testDebounceInterval)

	// Subscribe to output events BEFORE bus.Start()
	eventChan := bus.Subscribe("test-sub", 50)

	// Subscribe mock validators BEFORE bus.Start()
	validatorChan := bus.Subscribe("test-sub", 50)

	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)

	// Start mock validators that respond to validation requests
	go func() {
		for event := range validatorChan {
			if req, ok := event.(*events.ConfigValidationRequest); ok {
				// Respond as "basic" validator
				bus.Publish(events.NewConfigValidationResponse(
					req.RequestID(),
					"basic",
					true,
					nil,
				))
				// Respond as "template" validator
				bus.Publish(events.NewConfigValidationResponse(
					req.RequestID(),
					"template",
					true,
					nil,
				))
				return
			}
		}
	}()

	time.Sleep(testutil.StartupDelay)

	// Publish ConfigParsedEvent
	testConfig := &coreconfig.Config{}
	bus.Publish(events.NewConfigParsedEvent(testConfig, nil, "v1", "sv1"))

	// Should publish ConfigValidatedEvent
	validated := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, eventChan, testutil.VeryLongTimeout)
	assert.Equal(t, "v1", validated.Version)
}

func TestConfigChangeHandler_HandleConfigParsed_WithValidators_ValidationFailed(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	// Configure validators
	validators := []string{"basic", "template"}
	handler := NewConfigChangeHandler(bus, logger, configCh, validators, testDebounceInterval)

	// Subscribe to output events BEFORE bus.Start()
	eventChan := bus.Subscribe("test-sub", 50)

	// Subscribe mock validators BEFORE bus.Start()
	validatorChan := bus.Subscribe("test-sub", 50)

	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)

	// Start mock validators - one fails
	go func() {
		for event := range validatorChan {
			if req, ok := event.(*events.ConfigValidationRequest); ok {
				// basic validator passes
				bus.Publish(events.NewConfigValidationResponse(
					req.RequestID(),
					"basic",
					true,
					nil,
				))
				// template validator fails
				bus.Publish(events.NewConfigValidationResponse(
					req.RequestID(),
					"template",
					false,
					[]string{"template syntax error"},
				))
				return
			}
		}
	}()

	time.Sleep(testutil.StartupDelay)

	// Publish ConfigParsedEvent
	testConfig := &coreconfig.Config{}
	bus.Publish(events.NewConfigParsedEvent(testConfig, nil, "v1", "sv1"))

	// Should publish ConfigInvalidEvent
	invalid := testutil.WaitForEvent[*events.ConfigInvalidEvent](t, eventChan, testutil.VeryLongTimeout)
	assert.Equal(t, "v1", invalid.Version)
	assert.Contains(t, invalid.ValidationErrors, "template")
}

func TestConfigChangeHandler_HandleConfigParsed_WithValidators_Timeout(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	// Configure validators that will never respond
	validators := []string{"nonexistent"}
	handler := NewConfigChangeHandler(bus, logger, configCh, validators, testDebounceInterval)

	bus.Start()

	// Use short timeout context
	ctx, cancel := context.WithTimeout(context.Background(), testutil.EventTimeout)
	defer cancel()

	testConfig := &coreconfig.Config{}
	outcome := handler.validateCandidate(ctx, &validationCandidate{
		generation: 1,
		event:      events.NewConfigParsedEvent(testConfig, nil, "v1", "sv1"),
	})
	assert.False(t, outcome.valid)
	assert.Contains(t, outcome.validationErrors, "coordinator")
}

func TestConfigChangeHandler_HandleConfigParsed_WithValidators_MissingResponder(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	// Configure validators - "missing" won't respond
	validators := []string{"basic", "missing"}
	handler := NewConfigChangeHandler(bus, logger, configCh, validators, testDebounceInterval)

	// Subscribe mock validators BEFORE bus.Start()
	validatorChan := bus.Subscribe("test-sub", 50)

	bus.Start()

	// Use short timeout context
	ctx, cancel := context.WithTimeout(context.Background(), testutil.EventTimeout)
	defer cancel()

	// Start mock validator - only "basic" responds
	go func() {
		for event := range validatorChan {
			if req, ok := event.(*events.ConfigValidationRequest); ok {
				// Only basic validator responds
				bus.Publish(events.NewConfigValidationResponse(
					req.RequestID(),
					"basic",
					true,
					nil,
				))
				return
			}
		}
	}()

	time.Sleep(testutil.StartupDelay)

	testConfig := &coreconfig.Config{}
	outcome := handler.validateCandidate(ctx, &validationCandidate{
		generation: 1,
		event:      events.NewConfigParsedEvent(testConfig, nil, "v1", "sv1"),
	})
	assert.False(t, outcome.valid)
	assert.Contains(t, outcome.validationErrors, "coordinator")
}

func TestConfigChangeHandler_RapidConfigChangesDebounced(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 10)

	// Use longer debounce interval for reliable testing
	debounceInterval := 100 * time.Millisecond
	handler := NewConfigChangeHandler(bus, logger, configCh, nil, debounceInterval)
	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// Enable reinitialization (simulating startup complete)
	handler.EnableReinitialization()

	// Publish 5 rapid config changes, each faster than the debounce interval
	for i := 1; i <= 5; i++ {
		cfg := &coreconfig.Config{}
		version := fmt.Sprintf("v%d", i)
		bus.Publish(events.NewConfigValidatedEvent(cfg, nil, version, ""))
		time.Sleep(20 * time.Millisecond) // Much less than debounce interval
	}

	// Wait for debounce to complete (debounce interval + buffer)
	time.Sleep(debounceInterval + 50*time.Millisecond)

	// Should receive exactly ONE signal (the last config)
	select {
	case <-configCh:
		// First signal received - expected
	default:
		t.Fatal("expected at least one signal after debounce")
	}

	// Verify no additional signals were sent
	select {
	case <-configCh:
		t.Fatal("expected only one signal due to debouncing, but got more")
	case <-time.After(50 * time.Millisecond):
		// Expected - no additional signals
	}
}

func TestConfigChangeHandler_DebounceTimerResetOnEachChange(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 10)

	debounceInterval := 80 * time.Millisecond
	handler := NewConfigChangeHandler(bus, logger, configCh, nil, debounceInterval)
	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// Enable reinitialization (simulating startup complete)
	handler.EnableReinitialization()

	// Publish first config change
	cfg1 := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(cfg1, nil, "v1", ""))

	// Wait 50ms (less than debounce interval)
	time.Sleep(50 * time.Millisecond)

	// No signal should be sent yet
	select {
	case <-configCh:
		t.Fatal("signal sent too early - debounce not working")
	default:
		// Expected - still debouncing
	}

	// Publish second config change - this should reset the timer
	cfg2 := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(cfg2, nil, "v2", ""))

	// Wait another 50ms (total 100ms since first, but only 50ms since second)
	time.Sleep(50 * time.Millisecond)

	// Still no signal - timer was reset
	select {
	case <-configCh:
		t.Fatal("signal sent too early - debounce timer not reset properly")
	default:
		// Expected - still debouncing from second event
	}

	// Wait for the full debounce interval from the second event
	time.Sleep(debounceInterval)

	// Now we should have the signal
	select {
	case reload := <-configCh:
		assert.Same(t, cfg2, reload.Snapshot.Config, "should receive the last config")
	case <-time.After(50 * time.Millisecond):
		t.Fatal("expected signal after debounce completed")
	}
}

func TestConfigChangeHandler_CleanupWithPendingDebounce(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 10)

	// Use longer debounce to ensure we can stop before it fires
	debounceInterval := 500 * time.Millisecond
	handler := NewConfigChangeHandler(bus, logger, configCh, nil, debounceInterval)
	bus.Start()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		handler.Start(ctx)
		close(done)
	}()
	time.Sleep(testutil.StartupDelay)

	// Enable reinitialization (simulating startup complete)
	handler.EnableReinitialization()

	// Publish config change to start debounce timer
	cfg := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(cfg, nil, "v1", ""))

	// Wait a bit for the event to be processed
	time.Sleep(50 * time.Millisecond)

	// Cancel context while debounce is pending
	cancel()

	// Wait for handler to stop
	select {
	case <-done:
		// Handler stopped
	case <-time.After(testutil.LongTimeout):
		t.Fatal("handler did not stop in time")
	}

	// Verify no signal was sent (debounce was cancelled)
	select {
	case <-configCh:
		t.Fatal("signal should not be sent after shutdown")
	default:
		// Expected - no signal because handler stopped
	}

	// Wait longer than the original debounce interval
	time.Sleep(debounceInterval + 100*time.Millisecond)

	// Still no signal - timer was properly stopped
	select {
	case <-configCh:
		t.Fatal("signal should not be sent after shutdown - timer not stopped properly")
	default:
		// Expected - timer was cleaned up
	}
}

func TestConfigChangeHandler_DefaultDebounceInterval(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	// Pass 0 to use default
	handler := NewConfigChangeHandler(bus, logger, configCh, nil, 0)

	assert.Equal(t, DefaultReinitDebounceInterval, handler.debounceInterval,
		"zero debounce interval should use default")
}

func TestConfigChangeHandler_NegativeDebounceInterval(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	// Pass negative to use default
	handler := NewConfigChangeHandler(bus, logger, configCh, nil, -100*time.Millisecond)

	assert.Equal(t, DefaultReinitDebounceInterval, handler.debounceInterval,
		"negative debounce interval should use default")
}

func TestConfigChangeHandler_QueuesLatestChangeDuringBootstrap(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)
	handler.SetInitialConfigVersion("v1")

	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	initialConfig := &coreconfig.Config{}
	queuedConfig := &coreconfig.Config{}
	latestConfig := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(initialConfig, nil, "v1", ""))
	bus.Publish(events.NewConfigValidatedEvent(queuedConfig, nil, "v2", ""))
	bus.Publish(events.NewConfigValidatedEvent(latestConfig, nil, "v3", ""))
	time.Sleep(testDebounceInterval + 50*time.Millisecond)

	select {
	case <-configCh:
		t.Fatal("startup change signaled before reinitialization was enabled")
	case <-time.After(testutil.NoEventTimeout):
	}

	handler.EnableReinitialization()

	select {
	case reload := <-configCh:
		assert.Same(t, latestConfig, reload.Snapshot.Config)
	case <-time.After(testutil.LongTimeout):
		t.Fatal("timeout waiting for queued startup change")
	}
}

func TestConfigChangeHandler_BootstrapEventOrderingSyntheticThenReal(t *testing.T) {
	// Tests the expected bootstrap sequence:
	// 1. Synthetic event (version="initial") - skipped by version check
	// 2. Watcher event (version=actual) - skipped (reinitialization disabled)
	// 3. EnableReinitialization() called - marks startup complete
	// 4. Real change event - NOT skipped
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)

	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)
	handler.SetInitialConfigVersion("4026")

	bus.Start()

	ctx := t.Context()

	go handler.Start(ctx)
	time.Sleep(testutil.StartupDelay)

	// Step 1: Synthetic bootstrap event (version="initial")
	testConfig1 := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(testConfig1, nil, "initial", ""))
	time.Sleep(testDebounceInterval + 50*time.Millisecond)

	select {
	case <-configCh:
		t.Fatal("unexpected signal for synthetic bootstrap event")
	case <-time.After(testutil.NoEventTimeout):
		// Expected - synthetic event skipped
	}

	// Step 2: Watcher bootstrap event matches the fetched version.
	testConfig2 := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(testConfig2, nil, "4026", "sv1"))
	time.Sleep(testDebounceInterval + 50*time.Millisecond)

	select {
	case <-configCh:
		t.Fatal("unexpected signal for watcher bootstrap event")
	case <-time.After(testutil.NoEventTimeout):
		// Expected - event skipped (reinitialization disabled during startup)
	}

	// Step 3: Enable reinitialization (marks startup complete)
	handler.EnableReinitialization()

	// Step 4: Real config change (should NOT be skipped)
	testConfig3 := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(testConfig3, nil, "4027", "sv2"))
	time.Sleep(testDebounceInterval + 50*time.Millisecond)

	select {
	case reload := <-configCh:
		assert.Same(t, testConfig3, reload.Snapshot.Config)
	case <-time.After(testutil.LongTimeout):
		t.Fatal("timeout waiting for config signal on real change")
	}
}

func credentialsTestSnapshot(config *coreconfig.Config, credentials *coreconfig.Credentials) *ValidatedSnapshot {
	return &ValidatedSnapshot{
		RawConfig:          config,
		Config:             config,
		ConfigVersion:      "v1",
		Credentials:        credentials,
		CredentialsVersion: "secret-v1",
	}
}

// Rotation is decided by the credentials, not the Secret's resourceVersion:
// every chart upgrade relabels the Secret (#456).
func TestConfigChangeHandler_CredentialsRotationComparesContent(t *testing.T) {
	activeCredentials := coreconfig.Credentials{DataplaneUsername: "admin", DataplanePassword: "secret"}
	newPassword := coreconfig.Credentials{DataplaneUsername: "admin", DataplanePassword: "rotated"}
	newUsername := coreconfig.Credentials{DataplaneUsername: "operator", DataplanePassword: "secret"}

	type update struct {
		credentials coreconfig.Credentials
		version     string
	}
	tests := []struct {
		name    string
		updates []update
		restart *coreconfig.Credentials
	}{
		{
			name:    "bootstrap event for the active Secret",
			updates: []update{{activeCredentials, "secret-v1"}},
		},
		{
			name:    "label-only update",
			updates: []update{{activeCredentials, "secret-v2"}},
		},
		{
			name:    "password change",
			updates: []update{{newPassword, "secret-v2"}},
			restart: &newPassword,
		},
		{
			name:    "username change",
			updates: []update{{newUsername, "secret-v2"}},
			restart: &newUsername,
		},
		{
			name:    "deleted and recreated with the same data",
			updates: []update{{activeCredentials, "secret-v1"}, {activeCredentials, "secret-v2"}},
		},
		{
			name:    "deleted and recreated with a new password",
			updates: []update{{activeCredentials, "secret-v1"}, {newPassword, "secret-v2"}},
			restart: &newPassword,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus, logger := testutil.NewTestBusAndLogger()
			configCh := make(chan *ReloadRequest, 1)
			handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)
			activeConfig := &coreconfig.Config{}
			active := activeCredentials
			handler.SetInitialSnapshot(credentialsTestSnapshot(activeConfig, &active))
			handler.EnableReinitialization()
			for _, u := range tt.updates {
				credentials := u.credentials
				handler.handleSecretRotation(events.NewCredentialsUpdatedEvent(&credentials, u.version))
			}
			go handler.Start(t.Context())

			if tt.restart == nil {
				select {
				case reload := <-configCh:
					t.Fatalf("unchanged credentials restarted the iteration: %v", reload.Reasons)
				case <-time.After(testDebounceInterval + testutil.NoEventTimeout):
				}
				return
			}
			select {
			case reload := <-configCh:
				assert.Same(t, activeConfig, reload.Snapshot.Config)
				assert.Equal(t, *tt.restart, *reload.Snapshot.Credentials)
				assert.Equal(t, ReloadReasonCredentials, reload.Reasons)
			case <-time.After(testutil.LongTimeout):
				t.Fatal("credentials rotation did not restart the iteration")
			}
		})
	}
}

func TestConfigChangeHandler_QueuesCredentialsRotationDuringBootstrap(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)
	cachedConfig := &coreconfig.Config{}
	handler.SetInitialSnapshot(credentialsTestSnapshot(cachedConfig, &coreconfig.Credentials{DataplanePassword: "old"}))

	bus.Start()
	go handler.Start(t.Context())
	time.Sleep(testutil.StartupDelay)

	rotatedCredentials := &coreconfig.Credentials{DataplanePassword: "rotated"}
	bus.Publish(events.NewCredentialsUpdatedEvent(rotatedCredentials, "credentials-rotated"))
	time.Sleep(testDebounceInterval + 50*time.Millisecond)

	select {
	case <-configCh:
		t.Fatal("credentials rotation signaled before startup completed")
	case <-time.After(testutil.NoEventTimeout):
	}

	handler.EnableReinitialization()

	select {
	case reload := <-configCh:
		assert.Same(t, cachedConfig, reload.Snapshot.Config)
		assert.Same(t, rotatedCredentials, reload.Snapshot.Credentials)
	case <-time.After(testutil.LongTimeout):
		t.Fatal("timeout waiting for queued credentials rotation")
	}
}

// TestConfigChangeHandler_HandleSecretRotation_SkipsSyntheticInitialEvent
// pins the regression that caused issue #46: webhook.go publishes a
// synthetic CredentialsUpdatedEvent("initial") during iteration startup
// to kick subscribers before the real watcher onAdd. Without the dedicated "initial"-skip, the handler treated it as a
// rotation and scheduled an iteration restart ~1s after every startup,
// which raced UpdateBlocklistAndRestart in the HTTP-store
// invalid-update acceptance test and caused the new iteration's empty
// HTTPStore to cache invalid blocklist content as accepted.
//
// The fix is parallel to the version="initial" skip
// handleConfigValidated already had at the top of its handler.
func TestConfigChangeHandler_HandleSecretRotation_SkipsSyntheticInitialEvent(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	configCh := make(chan *ReloadRequest, 1)
	handler := NewConfigChangeHandler(bus, logger, configCh, nil, testDebounceInterval)
	handler.SetInitialConfigVersion("v1")

	bus.Start()
	go handler.Start(t.Context())
	time.Sleep(testutil.StartupDelay)

	// Cache a validated config so the path-under-test would otherwise
	// be able to signal reinit. Without this we can't distinguish "fix
	// worked" from "no cached config, would have warned and bailed
	// anyway".
	cachedConfig := &coreconfig.Config{}
	bus.Publish(events.NewConfigValidatedEvent(cachedConfig, nil, "v1", "sv1"))
	time.Sleep(testDebounceInterval + 50*time.Millisecond)
	select {
	case <-configCh:
	default:
	}

	handler.EnableReinitialization()

	// The cached snapshot carries no credentials, so only the synthetic
	// skip keeps this event from signaling reinit.
	bus.Publish(events.NewCredentialsUpdatedEvent(&coreconfig.Credentials{}, "initial"))
	time.Sleep(testDebounceInterval + 50*time.Millisecond)
	select {
	case <-configCh:
		t.Fatal("synthetic CredentialsUpdatedEvent(version=\"initial\") must not trigger iteration restart (issue #46)")
	case <-time.After(testutil.NoEventTimeout):
		// expected
	}

	// A real rotation still triggers reinit.
	rotatedCredentials := &coreconfig.Credentials{}
	bus.Publish(events.NewCredentialsUpdatedEvent(rotatedCredentials, "9999"))
	time.Sleep(testDebounceInterval + 50*time.Millisecond)
	select {
	case reload := <-configCh:
		assert.Same(t, cachedConfig, reload.Snapshot.Config)
		assert.Same(t, rotatedCredentials, reload.Snapshot.Credentials)
	case <-time.After(testutil.LongTimeout):
		t.Fatal("real credentials rotation must still signal reinit after the synthetic-skip fix")
	}
}
