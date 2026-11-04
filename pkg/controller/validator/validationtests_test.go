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

package validator

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
)

// configWithValidationTest builds a minimal valid config carrying a single
// `contains` validationTest asserting that the rendered haproxy.cfg matches
// pattern. A pattern the render produces ⇒ the test passes; one it doesn't ⇒
// the test (and therefore the gate) fails.
func configWithValidationTest(pattern string) *coreconfig.Config {
	cfg := &coreconfig.Config{
		PodSelector: coreconfig.PodSelector{MatchLabels: map[string]string{"app": "haproxy"}},
		Logging:     coreconfig.LoggingConfig{Level: "INFO"},
		HAProxyConfig: coreconfig.HAProxyConfig{
			Template: "frontend http\n  bind *:80\n",
		},
		ValidationTests: map[string]coreconfig.ValidationTest{
			"test-frontend-present": {
				Description: "the rendered config contains the http frontend",
				Assertions: []coreconfig.ValidationAssertion{
					{
						Type:        "contains",
						Target:      "haproxy.cfg",
						Pattern:     pattern,
						Description: "frontend present",
					},
				},
			},
		},
	}
	coreconfig.SetDefaults(cfg)
	return cfg
}

// runValidationTestsValidator drives one HandleRequest through the validator and
// returns the published ConfigValidationResponse.
func runValidationTestsValidator(t *testing.T, cfg *coreconfig.Config) *events.ConfigValidationResponse {
	t.Helper()
	return runValidationTestsValidatorWithTimeout(t, cfg, 0)
}

// runValidationTestsValidatorWithTimeout is like runValidationTestsValidator but
// overrides the test-execution timeout (0 = leave the production default).
func runValidationTestsValidatorWithTimeout(t *testing.T, cfg *coreconfig.Config, runTimeout time.Duration) *events.ConfigValidationResponse {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	bus := busevents.NewEventBus(16)
	responses := bus.Subscribe("test-collector", 4)
	v := NewValidationTestsValidator(bus, logger, stubTypeBootstrapper)
	bus.Start()
	if runTimeout > 0 {
		v.budgetFor = func(int) time.Duration { return runTimeout }
	}
	v.HandleRequest(events.NewConfigValidationRequest(cfg, "v-test"))

	select {
	case ev := <-responses:
		resp, ok := ev.(*events.ConfigValidationResponse)
		if !ok {
			t.Fatalf("expected *ConfigValidationResponse, got %T", ev)
		}
		if resp.ValidatorName != ValidatorNameValidationTests {
			t.Fatalf("expected responder %q, got %q", ValidatorNameValidationTests, resp.ValidatorName)
		}
		return resp
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for ConfigValidationResponse")
		return nil
	}
}

func TestValidationTestsValidator_PassingTestsAccepted(t *testing.T) {
	resp := runValidationTestsValidator(t, configWithValidationTest("frontend http"))
	if !resp.Valid {
		t.Fatalf("expected config with passing validationTests to be accepted, errors: %v", resp.Errors)
	}
	if len(resp.Errors) != 0 {
		t.Fatalf("expected no errors on accept, got: %v", resp.Errors)
	}
}

func TestValidationTestsValidator_FailingTestsRejected(t *testing.T) {
	// The render never emits this string, so the contains assertion fails and
	// the gate must reject the config.
	resp := runValidationTestsValidator(t, configWithValidationTest("this-string-is-never-rendered"))
	if resp.Valid {
		t.Fatal("expected config with a failing validationTest to be rejected")
	}
	if resp.Inconclusive {
		t.Fatal("an assertion failure must remain a definitive rejection")
	}
	if len(resp.Errors) == 0 {
		t.Fatal("expected at least one error describing the failing test")
	}
	// The failing test name must surface so operators know what broke.
	if !strings.Contains(strings.Join(resp.Errors, "\n"), "test-frontend-present") {
		t.Fatalf("expected the failing test name in the errors, got: %v", resp.Errors)
	}
}

func TestValidationTestsValidator_TimeoutFailsClosed(t *testing.T) {
	resp := runValidationTestsValidatorWithTimeout(t, configWithValidationTest("frontend http"), time.Nanosecond)
	if resp.Valid {
		t.Fatal("an incomplete validationTests run must not pass")
	}
	if !resp.Inconclusive {
		t.Fatal("an incomplete suite without assertion failures must be retryable")
	}
	if len(resp.Errors) == 0 || !strings.Contains(strings.Join(resp.Errors, "\n"), "did not complete") {
		t.Fatalf("expected a 'did not complete' rejection reason, got: %v", resp.Errors)
	}
}

func TestValidationTestsValidator_NoTestsIsNoOpPass(t *testing.T) {
	cfg := configWithValidationTest("frontend http")
	cfg.ValidationTests = nil
	resp := runValidationTestsValidator(t, cfg)
	if !resp.Valid {
		t.Fatalf("expected config with no validationTests to pass trivially, errors: %v", resp.Errors)
	}
}

// RunValidationTestsSync backs both the live validator and the controller's
// startup load gate; these pin its behaviour directly, independent of the
// event-driven validator.

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestRunValidationTestsSync_Passes(t *testing.T) {
	result, err := RunValidationTestsSync(context.Background(), configWithValidationTest("frontend http"), stubTypeBootstrapper, 0, testLogger())
	if err != nil {
		t.Fatalf("unexpected setup error: %v", err)
	}
	if !result.Passed || result.Incomplete {
		t.Fatalf("expected a passing complete run, got %+v", result)
	}
}

func TestRunValidationTestsSync_Fails(t *testing.T) {
	result, err := RunValidationTestsSync(context.Background(), configWithValidationTest("this-string-is-never-rendered"), stubTypeBootstrapper, 0, testLogger())
	if err != nil {
		t.Fatalf("unexpected setup error: %v", err)
	}
	if result.Passed {
		t.Fatal("expected a failing validationTest to NOT pass")
	}
	if !strings.Contains(strings.Join(result.Failures, "\n"), "test-frontend-present") {
		t.Fatalf("expected the failing test name in the failures, got: %v", result.Failures)
	}
}

func TestRunValidationTestsSync_NoTestsZeroCostPass(t *testing.T) {
	cfg := configWithValidationTest("frontend http")
	cfg.ValidationTests = nil
	// A nil bootstrap proves the no-tests path short-circuits before any setup.
	result, err := RunValidationTestsSync(context.Background(), cfg, nil, 0, testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Fatalf("expected zero-cost pass with no validationTests, got %+v", result)
	}
}

func TestRunValidationTestsSync_BootstrapErrorSurfaces(t *testing.T) {
	boom := func(context.Context, *coreconfig.Config) (*typebootstrap.Result, error) {
		return nil, errors.New("schema server unreachable")
	}
	_, err := RunValidationTestsSync(context.Background(), configWithValidationTest("frontend http"), boom, 0, testLogger())
	if err == nil {
		t.Fatal("expected a setup error when the bootstrap fails")
	}
	if !strings.Contains(err.Error(), "schema acquisition failed") {
		t.Fatalf("expected a wrapped schema-acquisition error, got: %v", err)
	}
}

// TestSuiteRunBudget pins the suite-size scaling (#77): the 25s floor is ADDED
// to per-test time, so every suite gets the small-suite headroom plus time
// proportional to its own work. Taking max(floor, scaled) instead pinned every
// suite under 250 tests to exactly 25s and reintroduced #77 at the crossover —
// the chart's ~249-test effective suite needed 27.3s against a 24.9s scaled
// value that clamped to the floor. The envelope must stay strictly larger than
// the run budget for any size, or the coordinator would declare the
// validationtests validator a missing responder instead of receiving its
// self-reported verdict.
func TestSuiteRunBudget(t *testing.T) {
	if got := SuiteRunBudget(0); got != 25*time.Second {
		t.Fatalf("zero-suite budget must be the 25s floor, got %s", got)
	}
	if got := SuiteRunBudget(100); got != 42500*time.Millisecond {
		t.Fatalf("100 tests must be floor + 17.5s = 42.5s, got %s", got)
	}
	if got := SuiteRunBudget(362); got != 88350*time.Millisecond {
		t.Fatalf("the incident's 362-test suite must get floor + 63.35s = 88.35s, got %s", got)
	}
	// The crossover that clamping broke: a ~249-test suite observed at 27.3s
	// must fit, where max(floor, scaled) gave it exactly 25s.
	if got := SuiteRunBudget(249); got <= 28*time.Second {
		t.Fatalf("a 249-test suite observed at 27.3s must fit its budget, got %s", got)
	}
	// Strictly increasing in suite size — no plateau where extra tests get no
	// extra time, which is exactly the shape that produced the false rejection.
	for _, n := range []int{0, 1, 100, 249, 250, 362, 1000} {
		if SuiteValidationEnvelope(n) <= SuiteRunBudget(n) {
			t.Fatalf("envelope must be strictly larger than the run budget for %d tests", n)
		}
		if n > 0 && SuiteRunBudget(n) <= SuiteRunBudget(n-1) {
			t.Fatalf("budget must strictly increase with suite size at %d tests", n)
		}
	}
}
