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

// Package configtest runs a HAProxyTemplateConfig's embedded validationTests
// against an already-built engine, bounded by a timeout. It backs
// pkg/controller/validator.RunValidationTestsSync, which both the startup load
// gate and the live config-change gate run.
package configtest

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// maxReportedFailures bounds how many failed-test names are folded into a
// result so a config with a broken shared snippet (which can fail hundreds of
// tests at once) produces an actionable, not overwhelming, message.
const maxReportedFailures = 10

// Result is the outcome of running a config's validationTests.
type Result struct {
	// Passed is true when every test passed AND the run completed. A config with
	// no validationTests passes trivially.
	Passed bool

	// Failures holds bounded, human-readable summaries of the failed tests.
	// Non-empty only when Passed is false and Incomplete is false.
	Failures []string

	// Incomplete is true when the run was cut short by the timeout before every
	// test finished. The caller owns the fail-closed (reject) vs fail-open
	// (admit-with-warning) decision: the daemon load gate fails closed; the
	// admission webhook fails open so a slow suite never blocks an apply.
	Incomplete bool
}

// RunValidationTests executes cfg's embedded validationTests against the given
// (already-compiled) engine, bounded by timeout. typedResourceTypes carries the
// per-resource typed reflect.Types so the test render context matches the typed
// globals the engine compiled against. It builds and tears down a temporary
// HAProxy validation tree for `haproxy_valid` assertions.
//
// Returns Passed=true immediately when the config declares no validationTests.
func RunValidationTests(
	ctx context.Context,
	cfg *config.Config,
	engine templating.Engine,
	typedResourceTypes map[string]reflect.Type,
	timeout time.Duration,
	logger *slog.Logger,
) (Result, error) {
	if len(cfg.ValidationTests) == 0 {
		return Result{Passed: true}, nil
	}

	paths, capabilities, haproxyVersion, cleanup, err := buildValidationPaths(ctx, cfg)
	if err != nil {
		return Result{}, fmt.Errorf("preparing HAProxy validation environment: %w", err)
	}
	defer cleanup()

	runner := testrunner.New(cfg, engine, paths, &testrunner.Options{
		Logger:             logger,
		Capabilities:       capabilities,
		HAProxyVersion:     haproxyVersion,
		TypedResourceTypes: typedResourceTypes,
	})

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	results, err := runner.RunTests(runCtx, "")
	if err != nil {
		return Result{}, fmt.Errorf("executing validationTests: %w", err)
	}

	// RunTests does NOT report context cancellation as an error: on timeout its
	// workers stop early and the un-run tests are silently absent from the
	// counts (counted in neither PassedTests nor FailedTests). So a timed-out
	// run with no observed failures looks like "0 failures" — which would be
	// fail-OPEN if trusted. Flag that case as Incomplete and let the caller
	// decide (the load gate fails closed; the webhook admits-with-warning).
	//
	// BUT a cut-short run that ALREADY observed real failures must surface them
	// as a denial, not collapse to Incomplete: observed failures are
	// authoritative regardless of whether the rest of the suite finished.
	// Without this, a genuinely-failing config whose suite happens to exceed the
	// budget would slip through the webhook's admit-with-warning path.
	if results.FailedTests > 0 {
		return Result{Failures: summarizeTestFailures(results)}, nil
	}
	if runCtx.Err() != nil {
		return Result{Incomplete: true}, nil
	}
	return Result{Passed: true}, nil
}

// buildValidationPaths creates a temporary HAProxy validation tree mirroring the
// production layout (maps/ssl/general dirs + config-file path) so the
// testrunner's `haproxy_valid` assertions can run `haproxy -c`. The returned
// cleanup removes the temp tree.
func buildValidationPaths(ctx context.Context, cfg *config.Config) (
	paths *dataplane.ValidationPaths,
	capabilities dataplane.Capabilities,
	haproxyVersion *dataplane.Version,
	cleanup func(),
	err error,
) {
	localVersion, err := dataplane.DetectLocalVersionContext(ctx)
	if err != nil {
		return nil, dataplane.Capabilities{}, nil, nil, fmt.Errorf("detecting local HAProxy version: %w", err)
	}
	capabilities = dataplane.CapabilitiesFromVersion(localVersion)

	tempDir, err := os.MkdirTemp("", "haproxy-cfgtest-*")
	if err != nil {
		return nil, dataplane.Capabilities{}, nil, nil, fmt.Errorf("creating temp dir: %w", err)
	}

	// path.Base (not filepath.Base) for the inner extraction: the configured
	// dirs are slash-separated HAProxy target paths regardless of host OS.
	basePaths := dataplane.PathConfig{
		MapsDir:    filepath.Join(tempDir, path.Base(cfg.Dataplane.MapsDir)),
		SSLDir:     filepath.Join(tempDir, path.Base(cfg.Dataplane.SSLCertsDir)),
		GeneralDir: filepath.Join(tempDir, path.Base(cfg.Dataplane.GeneralStorageDir)),
		SecretDir:  filepath.Join(tempDir, path.Base(cfg.Dataplane.SecretsDir)),
		ConfigFile: filepath.Join(tempDir, names.MainTemplateName),
	}
	resolvedPaths := dataplane.ResolvePaths(&basePaths)

	dirsToCreate := []string{resolvedPaths.MapsDir, resolvedPaths.SSLDir, resolvedPaths.GeneralDir}
	if resolvedPaths.CRTListDir != resolvedPaths.SSLDir && resolvedPaths.CRTListDir != resolvedPaths.GeneralDir {
		dirsToCreate = append(dirsToCreate, resolvedPaths.CRTListDir)
	}
	for _, dir := range dirsToCreate {
		if mkErr := os.MkdirAll(dir, 0o750); mkErr != nil {
			_ = os.RemoveAll(tempDir)
			return nil, dataplane.Capabilities{}, nil, nil, fmt.Errorf("creating directory: %w", mkErr)
		}
	}

	cleanup = func() { _ = os.RemoveAll(tempDir) }
	return resolvedPaths.ToValidationPaths(), capabilities, localVersion, cleanup, nil
}

// summarizeTestFailures turns the failed per-test results into a bounded,
// human-readable error list.
func summarizeTestFailures(results *testrunner.TestResults) []string {
	out := make([]string, 0, maxReportedFailures+1)
	reported := 0
	for i := range results.TestResults {
		tr := &results.TestResults[i]
		if tr.Passed || tr.Skipped {
			continue
		}
		if reported >= maxReportedFailures {
			out = append(out, fmt.Sprintf("... and %d more failing test(s)", results.FailedTests-reported))
			break
		}
		out = append(out, fmt.Sprintf("validationTest %q failed: %s", tr.TestName, firstFailureReason(tr)))
		reported++
	}
	if len(out) == 0 {
		out = append(out, fmt.Sprintf("%d validationTest(s) failed", results.FailedTests))
	}
	return out
}

// firstFailureReason extracts a short reason from a failed test result.
func firstFailureReason(tr *testrunner.TestResult) string {
	reasons := assertionFailureMessages(tr)
	if len(reasons) == 0 {
		return "no assertion detail reported"
	}
	return strings.Join(reasons, "; ")
}

// assertionFailureMessages collects the render error (if any) and each failed
// assertion's message from a failed test.
func assertionFailureMessages(tr *testrunner.TestResult) []string {
	var msgs []string
	if tr.RenderError != "" {
		msgs = append(msgs, "render error: "+tr.RenderError)
	}
	for i := range tr.Assertions {
		a := &tr.Assertions[i]
		if a.Passed {
			continue
		}
		msg := a.Error
		if msg == "" {
			msg = a.Type + " assertion failed"
		}
		msgs = append(msgs, msg)
	}
	return msgs
}
