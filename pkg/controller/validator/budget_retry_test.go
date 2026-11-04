// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package validator_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/configchange"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validator"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
)

func TestValidationBudgetRetriesUnchangedCandidateBeforeReload(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	bus := busevents.NewEventBus(100)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reloads := make(chan *configchange.ReloadRequest, 1)
	handler := configchange.NewConfigChangeHandler(bus, logger, reloads, validator.AllValidatorNames(), time.Millisecond)
	previous := validationRetryConfig("old")
	candidate := validationRetryConfig("candidate")
	handler.SetInitialSnapshot(&configchange.ValidatedSnapshot{
		RawConfig: previous, Config: previous, ConfigVersion: "previous", CredentialsVersion: "credentials",
	})
	handler.EnableReinitialization()
	bootstrap := func(context.Context, *coreconfig.Config) (*typebootstrap.Result, error) {
		return &typebootstrap.Result{}, nil
	}
	var attempts atomic.Int32
	retryAllowed := make(chan struct{})
	tests := validator.NewBudgetedValidationTestsValidator(bus, logger, func(count int) time.Duration {
		if attempts.Add(1) == 1 {
			return time.Nanosecond
		}
		select {
		case <-retryAllowed:
		case <-ctx.Done():
		}
		return validator.SuiteRunBudget(count)
	})
	basic := validator.NewBasicValidator(bus, logger)
	templates := validator.NewTemplateValidator(bus, logger, bootstrap)
	jsonpath := validator.NewJSONPathValidator(bus, logger)
	observed := bus.SubscribeTypes("budget-retry-observer", 30,
		events.EventTypeConfigValidationInconclusive, events.EventTypeConfigInvalid, events.EventTypeConfigValidated)
	var running sync.WaitGroup
	for _, start := range []func(context.Context) error{handler.Start, tests.Start, basic.Start, templates.Start, jsonpath.Start} {
		running.Go(func() {
			if err := start(ctx); err != nil {
				t.Errorf("component stopped: %v", err)
			}
		})
	}
	t.Cleanup(func() {
		cancel()
		running.Wait()
	})
	require.True(t, bus.StartContext(ctx))
	bus.Publish(events.NewConfigParsedEvent(candidate, nil, "candidate", "credentials"))
	incomplete, ok := awaitConfigValidationOutcome(t, ctx, observed).(*events.ConfigValidationInconclusiveEvent)
	require.True(t, ok, "the first incomplete run must be inconclusive")
	require.Equal(t, "candidate", incomplete.Version)
	require.Contains(t, incomplete.Reasons, validator.ValidatorNameValidationTests)
	select {
	case <-reloads:
		t.Fatal("an unfinished suite reached the reload channel")
	default:
	}
	close(retryAllowed)

	validated, ok := awaitConfigValidationOutcome(t, ctx, observed).(*events.ConfigValidatedEvent)
	require.True(t, ok, "the same candidate must pass on retry")
	require.Equal(t, "candidate", validated.Version)
	require.EqualValues(t, 2, attempts.Load())
	select {
	case reload := <-reloads:
		require.Equal(t, "candidate", reload.Snapshot.ConfigVersion)
		require.Equal(t, candidate, reload.Snapshot.Config)
		require.True(t, reload.Reasons.Has(configchange.ReloadReasonConfig))
	case <-ctx.Done():
		t.Fatal("the completed suite did not release the candidate")
	}
}

func awaitConfigValidationOutcome(t *testing.T, ctx context.Context, observed <-chan busevents.Event) busevents.Event {
	t.Helper()
	select {
	case event := <-observed:
		if invalid, ok := event.(*events.ConfigInvalidEvent); ok {
			t.Fatalf("an unfinished suite permanently rejected the candidate: %v", invalid.ValidationErrors)
		}
		return event
	case <-ctx.Done():
		t.Fatal("the same candidate was not retried after its suite timed out")
		return nil
	}
}

func validationRetryConfig(name string) *coreconfig.Config {
	cfg := &coreconfig.Config{
		PodSelector: coreconfig.PodSelector{MatchLabels: map[string]string{"app": "haproxy"}},
		Logging:     coreconfig.LoggingConfig{Level: "INFO"},
		WatchedResources: map[string]coreconfig.WatchedResource{
			"records": {APIVersion: "example.com/v1", Resources: "records", IndexBy: []string{"metadata.name"}},
		},
		HAProxyConfig: coreconfig.HAProxyConfig{
			Template: "frontend " + name + "\n  bind *:80\n",
		},
		ValidationTests: map[string]coreconfig.ValidationTest{
			"frontend-present": {
				Assertions: []coreconfig.ValidationAssertion{{Type: "contains", Target: "haproxy.cfg", Pattern: "frontend " + name}},
			},
		},
	}
	coreconfig.SetDefaults(cfg)
	return cfg
}
