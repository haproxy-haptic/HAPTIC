package configchange

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
)

const schemaTimeoutReason = "schema acquisition failed for one or more watched resources " +
	"(typed template access cannot be validated without real schemas): " +
	"type bootstrap: context deadline exceeded"

// startScriptedValidators answers every validation request with respond's
// responses and reports each request's attempt number on attempts.
func startScriptedValidators(
	bus *busevents.EventBus,
	respond func(attempt int, requestID string) []*events.ConfigValidationResponse,
) <-chan int {
	requests := bus.SubscribeTypes("test-validator", 50, events.EventTypeConfigValidationRequest)
	attempts := make(chan int, 50)
	go func() {
		attempt := 0
		for event := range requests {
			req, ok := event.(*events.ConfigValidationRequest)
			if !ok {
				continue
			}
			attempt++
			for _, response := range respond(attempt, req.RequestID()) {
				bus.Publish(response)
			}
			attempts <- attempt
		}
	}()
	return attempts
}

func TestConfigChangeHandler_TransientSchemaAcquisitionFailureIsNotFinal(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	handler := NewConfigChangeHandler(bus, logger, make(chan *ReloadRequest, 1), []string{"template"}, testDebounceInterval)
	eventChan := bus.Subscribe("test-sub", 50)
	startScriptedValidators(bus, func(attempt int, requestID string) []*events.ConfigValidationResponse {
		if attempt == 1 {
			return []*events.ConfigValidationResponse{
				events.NewConfigValidationInconclusiveResponse(requestID, "template", []string{schemaTimeoutReason}),
			}
		}
		return []*events.ConfigValidationResponse{events.NewConfigValidationResponse(requestID, "template", true, nil)}
	})
	bus.Start()
	go handler.Start(t.Context())

	time.Sleep(testutil.StartupDelay)
	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "v1", "sv1"))

	sawInconclusive := false
	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev := <-eventChan:
			switch e := ev.(type) {
			case *events.ConfigInvalidEvent:
				t.Fatalf("an inconclusive validation was reported as invalid: %v", e.ValidationErrors)
			case *events.ConfigValidationInconclusiveEvent:
				assert.Equal(t, "v1", e.Version)
				assert.Equal(t, map[string][]string{"template": {schemaTimeoutReason}}, e.Reasons)
				assert.Equal(t, validationRetryInitialBackoff, e.RetryIn)
				sawInconclusive = true
			case *events.ConfigValidatedEvent:
				if e.Version == "v1" {
					assert.True(t, sawInconclusive, "the first validation must have been inconclusive")
					return
				}
			}
		case <-deadline:
			t.Fatal("v1 was never validated after one transient schema acquisition failure")
		}
	}
}

func TestConfigChangeHandler_RealValidationErrorStaysFinal(t *testing.T) {
	tests := []struct {
		name       string
		validators []string
		respond    func(requestID string) []*events.ConfigValidationResponse
		wantErrors map[string][]string
	}{
		{
			name:       "rejection",
			validators: []string{"template"},
			respond: func(requestID string) []*events.ConfigValidationResponse {
				return []*events.ConfigValidationResponse{
					events.NewConfigValidationResponse(requestID, "template", false, []string{schemaTimeoutReason}),
				}
			},
			wantErrors: map[string][]string{"template": {schemaTimeoutReason}},
		},
		{
			name:       "rejection beside an inconclusive validator",
			validators: []string{"template", "basic"},
			respond: func(requestID string) []*events.ConfigValidationResponse {
				return []*events.ConfigValidationResponse{
					events.NewConfigValidationInconclusiveResponse(requestID, "template", []string{schemaTimeoutReason}),
					events.NewConfigValidationResponse(requestID, "basic", false, []string{"template is empty"}),
				}
			},
			wantErrors: map[string][]string{
				"template": {schemaTimeoutReason},
				"basic":    {"template is empty"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus, logger := testutil.NewTestBusAndLogger()
			handler := NewConfigChangeHandler(bus, logger, make(chan *ReloadRequest, 1), tt.validators, testDebounceInterval)
			eventChan := bus.Subscribe("test-sub", 50)
			attempts := startScriptedValidators(bus, func(_ int, requestID string) []*events.ConfigValidationResponse {
				return tt.respond(requestID)
			})
			bus.Start()
			go handler.Start(t.Context())

			time.Sleep(testutil.StartupDelay)
			bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "v1", "sv1"))

			invalid := testutil.WaitForEvent[*events.ConfigInvalidEvent](t, eventChan, testutil.LongTimeout)
			assert.Equal(t, "v1", invalid.Version)
			assert.Equal(t, tt.wantErrors, invalid.ValidationErrors)
			require.Equal(t, 1, <-attempts)

			select {
			case attempt := <-attempts:
				t.Fatalf("a rejected config was validated again (attempt %d)", attempt)
			case <-time.After(2 * validationRetryInitialBackoff):
			}
		})
	}
}

func TestConfigChangeHandler_NewerConfigSupersedesValidationRetry(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	handler := NewConfigChangeHandler(bus, logger, make(chan *ReloadRequest, 1), []string{"template"}, testDebounceInterval)
	eventChan := bus.Subscribe("test-sub", 50)
	versions := make(chan string, 10)
	requests := bus.SubscribeTypes("test-validator", 50, events.EventTypeConfigValidationRequest)
	go func() {
		for event := range requests {
			req := event.(*events.ConfigValidationRequest)
			versions <- req.Version
			if req.Version == "v1" {
				bus.Publish(events.NewConfigValidationInconclusiveResponse(req.RequestID(), "template", []string{schemaTimeoutReason}))
				continue
			}
			bus.Publish(events.NewConfigValidationResponse(req.RequestID(), "template", true, nil))
		}
	}()
	bus.Start()
	go handler.Start(t.Context())

	time.Sleep(testutil.StartupDelay)
	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "v1", "sv1"))
	testutil.WaitForEvent[*events.ConfigValidationInconclusiveEvent](t, eventChan, testutil.LongTimeout)
	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "v2", "sv1"))

	validated := testutil.WaitForEvent[*events.ConfigValidatedEvent](t, eventChan, testutil.LongTimeout)
	assert.Equal(t, "v2", validated.Version)
	assert.Equal(t, "v1", <-versions)
	assert.Equal(t, "v2", <-versions)
	select {
	case version := <-versions:
		t.Fatalf("superseded candidate was validated again (%s)", version)
	case <-time.After(2 * validationRetryInitialBackoff):
	}
}

func TestStatusUpdater_HandleConfigValidationInconclusive(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)

	u.handleConfigValidationInconclusive(context.Background(), inconclusiveVerdict(
		"v2", htc, map[string][]string{"template": {schemaTimeoutReason}}))

	status := getStatus(t, crd)
	assert.Equal(t, statusUnknown, status.ValidationStatus)
	assert.Equal(t, testGeneration, status.ObservedGeneration)
	assert.Equal(t, []string{schemaTimeoutReason}, status.ValidationErrors)

	cond := meta.FindStatusCondition(status.Conditions, conditionValidated)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status)
	assert.Equal(t, reasonValidationInconclusive, cond.Reason)
	assert.Contains(t, cond.Message, "context deadline exceeded")
}

// transientResolveError is what the effective-config resolver returns when the
// apiserver couldn't answer discovery or schema requests.
type transientResolveError struct{ transient bool }

func (e transientResolveError) Error() string {
	return "discovery for gateway.networking.k8s.io/v1: context deadline exceeded"
}
func (e transientResolveError) Transient() bool { return e.transient }

func TestConfigChangeHandler_TransientEffectiveConfigFailureIsRetried(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	handler := NewConfigChangeHandler(bus, logger, make(chan *ReloadRequest, 1), []string{"template"}, testDebounceInterval)
	resolves := 0
	handler.SetEffectiveResolver(func(c *coreconfig.Config) (*ResolvedConfig, error) {
		resolves++
		if resolves == 1 {
			return nil, transientResolveError{transient: true}
		}
		return &ResolvedConfig{Config: c}, nil
	})
	eventChan := bus.Subscribe("test-sub", 50)
	startScriptedValidators(bus, func(_ int, requestID string) []*events.ConfigValidationResponse {
		return []*events.ConfigValidationResponse{events.NewConfigValidationResponse(requestID, "template", true, nil)}
	})
	bus.Start()
	go handler.Start(t.Context())

	time.Sleep(testutil.StartupDelay)
	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "v1", "sv1"))

	sawInconclusive := false
	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev := <-eventChan:
			switch e := ev.(type) {
			case *events.ConfigInvalidEvent:
				t.Fatalf("a transient effective-config failure was reported as invalid: %v", e.ValidationErrors)
			case *events.ConfigValidationInconclusiveEvent:
				assert.Contains(t, e.Reasons, "effective-config")
				sawInconclusive = true
			case *events.ConfigValidatedEvent:
				if e.Version == "v1" {
					assert.True(t, sawInconclusive, "the first resolution must have been inconclusive")
					return
				}
			}
		case <-deadline:
			t.Fatal("v1 was never validated after one transient effective-config failure")
		}
	}
}

func TestConfigChangeHandler_EffectiveConfigRejectionStaysFinal(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	handler := NewConfigChangeHandler(bus, logger, make(chan *ReloadRequest, 1), []string{"template"}, testDebounceInterval)
	handler.SetEffectiveResolver(func(*coreconfig.Config) (*ResolvedConfig, error) {
		return nil, transientResolveError{transient: false}
	})
	eventChan := bus.Subscribe("test-sub", 50)
	bus.Start()
	go handler.Start(t.Context())

	time.Sleep(testutil.StartupDelay)
	bus.Publish(events.NewConfigParsedEvent(&coreconfig.Config{}, nil, "v1", "sv1"))

	invalid := testutil.WaitForEvent[*events.ConfigInvalidEvent](t, eventChan, testutil.LongTimeout)
	assert.Equal(t, "v1", invalid.Version)
	assert.Contains(t, invalid.ValidationErrors, "effective-config")
}
