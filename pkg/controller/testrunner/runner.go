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

// Package testrunner implements validation test execution for HAProxyTemplateConfig.
//
// This package provides a test runner that executes embedded validation tests
// defined in HAProxyTemplateConfig CRDs. It can be used both by the CLI
// (controller validate command) and by the admission webhook for validation.
//
// The test runner:
//   - Creates resource stores from test fixtures
//   - Renders templates with fixture context
//   - Runs assertions against rendered output
//   - Returns structured test results
//
// This is a pure component with no EventBus dependency - it's called directly
// by the CLI and by the DryRunValidator component.
package testrunner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/logging"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// New creates a new test runner.
//
// Parameters:
//   - cfg: The internal config containing templates and validation tests
//   - engine: Pre-compiled template engine
//   - validationPaths: Filesystem paths for HAProxy validation
//   - options: Runner options (pointer to avoid hugeParam copy — the
//     embedded Capabilities + new typed-resource-types map push the
//     struct past 80 bytes). Pass nil to use defaults.
//
// Returns:
//   - A new Runner instance ready to execute tests
func New(
	cfg *config.Config,
	engine templating.Engine,
	validationPaths *dataplane.ValidationPaths,
	options *Options,
) *Runner {
	if options == nil {
		options = &Options{}
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}

	workers := options.Workers
	if workers <= 0 {
		workers = automaticWorkers(runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1))
	}

	// Capture tracing state from template engine
	traceTemplates := engine.IsTracingEnabled()

	return &Runner{
		engineTemplate:     engine,
		validationPaths:    validationPaths,
		config:             cfg,
		logger:             logger.With("component", "test-runner"),
		workers:            workers,
		debugFilters:       options.DebugFilters,
		traceTemplates:     traceTemplates,
		profileIncludes:    options.ProfileIncludes,
		capabilities:       options.Capabilities,
		haproxyVersion:     options.HAProxyVersion,
		typedResourceTypes: options.TypedResourceTypes,
		checkWithoutBinary: options.CheckWithoutBinary,
		coldAnalysis: sync.OnceValue(func() *renderer.ColdIncrementalRenderAnalysis {
			return renderer.AnalyzeColdIncrementalRenders(cfg, engine)
		}),
	}
}

func automaticWorkers(parallelism int, memoryLimit int64) int {
	// HAProxy subprocess memory is outside GOMEMLIMIT; see README.md#concurrency-model.
	const memoryPerWorker = 128 << 20
	return max(1, int(min(2*int64(parallelism), memoryLimit/memoryPerWorker)))
}

// RunTests runs one named test, or all tests when testName is empty.
func (r *Runner) RunTests(ctx context.Context, testName string) (*TestResults, error) {
	var names []string
	if testName != "" {
		names = []string{testName}
	}
	return r.RunNamedTests(ctx, names)
}

// RunNamedTests runs every selected test once; an empty selection runs all tests.
func (r *Runner) RunNamedTests(ctx context.Context, names []string) (*TestResults, error) {
	startTime := time.Now()

	results := &TestResults{
		TestResults: make([]TestResult, 0),
	}

	testsToRun, err := r.filterTests(names)
	if err != nil {
		return nil, err
	}

	// Separate tests into runnable and skipped based on HAProxy version requirements
	runnableTests := make(map[string]config.ValidationTest, len(testsToRun))
	for name := range testsToRun {
		test := testsToRun[name]
		// "_global" carries shared fixtures AND a shared extraContext baseline
		// merged into every test (see the _global lookup in runSingleTest); it
		// is never executed as a standalone test. The benchmark path excludes it
		// the same way (cmd/haptic/benchmark.go).
		if name == "_global" {
			continue
		}
		if reason := r.shouldSkipTest(&test); reason != "" {
			results.SkippedTests++
			results.TestResults = append(results.TestResults, TestResult{
				TestName:    name,
				Description: test.Description,
				Skipped:     true,
				SkipReason:  reason,
			})
		} else {
			runnableTests[name] = test
		}
	}

	results.TotalTests = len(runnableTests)

	if len(runnableTests) == 0 {
		if results.SkippedTests > 0 {
			r.logger.Debug("All tests skipped", "skipped", results.SkippedTests)
		} else {
			r.logger.Debug("No tests to run")
		}
		results.Duration = time.Since(startTime)
		return results, nil
	}

	// Determine number of workers (use 1 worker if only 1 test)
	numWorkers := min(len(runnableTests), r.workers)

	// Subprocesses share the workers' memory budget and the pod's CPU allocation.
	gateSlots := numWorkers
	if p := runtime.GOMAXPROCS(0); p > 0 && p < gateSlots {
		gateSlots = p
	}
	r.checkGate = dataplane.NewCheckGateN(gateSlots, 0)

	r.logger.Log(context.Background(), logging.LevelTrace, "Starting test execution",
		"total_tests", len(runnableTests),
		"skipped_tests", results.SkippedTests,
		"workers", numWorkers)

	// Create channels for work distribution
	testChan := make(chan testEntry, len(runnableTests))
	resultChan := make(chan TestResult, len(runnableTests))

	// Start worker pool
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go r.testWorker(ctx, i, testChan, resultChan, &wg)
	}

	// Send tests to workers
	for name := range runnableTests {
		testChan <- testEntry{name: name, test: runnableTests[name]}
	}
	close(testChan)

	// Wait for all workers to finish in background
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect results
	for result := range resultChan {
		results.TestResults = append(results.TestResults, result)
		if result.Passed {
			results.PassedTests++
		} else {
			results.FailedTests++
		}
	}

	results.Duration = time.Since(startTime)

	r.logger.Info("Test run completed",
		"total", results.TotalTests,
		"passed", results.PassedTests,
		"failed", results.FailedTests,
		"skipped", results.SkippedTests,
		"duration", results.Duration)

	return results, nil
}

// testWorker is a worker goroutine that processes tests from the test channel.
// Each test gets isolated paths and shares the compiled template engine.
func (r *Runner) testWorker(ctx context.Context, workerID int, tests <-chan testEntry, results chan<- TestResult, wg *sync.WaitGroup) {
	defer wg.Done()

	r.logger.Log(context.Background(), logging.LevelTrace, "Worker started", "worker_id", workerID)

	testNum := 0
	for entry := range tests {
		select {
		case <-ctx.Done():
			// Context cancelled, stop processing
			return
		default:
			testStartTime := time.Now()
			r.logger.Log(context.Background(), logging.LevelTrace, "Worker processing test",
				"worker_id", workerID,
				"test_num", testNum,
				"test", entry.name)

			// Create unique temp directory for this specific test
			dirCreateStart := time.Now()
			testPaths, err := r.createTestPaths(workerID, testNum)
			dirCreateDuration := time.Since(dirCreateStart)

			if err != nil {
				r.logger.Error("Failed to create test paths",
					"worker_id", workerID,
					"test_num", testNum,
					"test", entry.name,
					"error", err,
					"duration_ms", dirCreateDuration.Milliseconds())
				results <- TestResult{
					TestName:    entry.name,
					Description: entry.test.Description,
					Passed:      false,
					RenderError: fmt.Sprintf("creating test temp directory: %v", err),
				}
				testNum++
				continue
			}

			r.logger.Log(context.Background(), logging.LevelTrace, "Created test paths",
				"worker_id", workerID,
				"test_num", testNum,
				"test", entry.name,
				"config_file", testPaths.ConfigFile,
				"duration_ms", dirCreateDuration.Milliseconds())

			// Reuse pre-compiled template engine
			// The engine is thread-safe for concurrent renders and has no per-test mutable state.
			// Filter state is per-render (stored in context), not per-engine.
			testEngine := r.engineTemplate

			// Run test with isolated paths and engine
			result, incomplete := r.runSingleTest(ctx, entry.name, &entry.test, testEngine, testPaths)
			r.removeTestPaths(testPaths)

			testDuration := time.Since(testStartTime)
			r.logger.Log(context.Background(), logging.LevelTrace, "Test completed",
				"worker_id", workerID,
				"test_num", testNum,
				"test", entry.name,
				"passed", result.Passed,
				"total_duration_ms", testDuration.Milliseconds())

			// Append traces from worker engine to main engine (for --trace-templates output)
			if r.traceTemplates {
				r.engineTemplate.AppendTraces(testEngine)
			}

			if !incomplete || !result.Passed {
				results <- result
			}
			if incomplete {
				return
			}

			testNum++
		}
	}
}

func (r *Runner) filterTests(names []string) (map[string]config.ValidationTest, error) {
	if len(names) == 0 {
		return r.config.ValidationTests, nil
	}
	filtered := make(map[string]config.ValidationTest, len(names))
	for _, name := range names {
		test, exists := r.config.ValidationTests[name]
		if !exists {
			return nil, fmt.Errorf("test %q not found", name)
		}
		filtered[name] = test
	}
	return filtered, nil
}

// shouldSkipTest checks whether a test should be skipped based on version requirements.
// Returns the skip reason if the test should be skipped, or empty string if it should run.
func (r *Runner) shouldSkipTest(test *config.ValidationTest) string {
	if test.MinHAProxyVersion == "" {
		return ""
	}

	if r.haproxyVersion == nil {
		return fmt.Sprintf("requires HAProxy >= %s but version is unknown", test.MinHAProxyVersion)
	}

	minVersion, err := dataplane.ParseVersionString(test.MinHAProxyVersion)
	if err != nil {
		r.logger.Warn("Invalid min_haproxy_version, running test anyway",
			"min_haproxy_version", test.MinHAProxyVersion,
			"error", err)
		return ""
	}

	if r.haproxyVersion.Compare(minVersion) < 0 {
		return fmt.Sprintf("requires HAProxy >= %s (detected %s)", test.MinHAProxyVersion, r.haproxyVersion.Full)
	}

	return ""
}

// runSingleTest executes a single validation test using worker-specific engine and validation paths.
func (r *Runner) runSingleTest(ctx context.Context, testName string, test *config.ValidationTest, engine templating.Engine, validationPaths *dataplane.ValidationPaths) (TestResult, bool) {
	r = r.withDeploymentWatchScopes(test.UseDeploymentWatchScopes)
	startTime := time.Now()
	ctx = templating.WithCycleTime(ctx, startTime)

	result := TestResult{
		TestName:    testName,
		Description: test.Description,
		Passed:      true,
		Assertions:  make([]AssertionResult, 0),
	}

	// 1.-4. Fixture stores, HTTP fixtures and the previously-deployed servers
	inputs, inputErr := r.renderInputs(testName, test)
	if inputErr != "" {
		result.Passed = false
		result.RenderError = inputErr
		result.Duration = time.Since(startTime)
		return result, false
	}
	fixtureStores, httpStore := inputs.Stores, inputs.HTTPStore
	currentConfig, effectiveExtraContext := inputs.CurrentConfig, inputs.ExtraContext

	// 5. Render HAProxy configuration and auxiliary files (using worker-specific engine)
	rendered, err := r.renderWithStores(ctx, engine, fixtureStores, validationPaths, httpStore, currentConfig, test.CurrentFiles, effectiveExtraContext)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			result.Duration = time.Since(startTime)
			return result, true
		}
		recordRenderFailure(&result, err, hasRenderingErrorAssertions(test.Assertions))
		if isResourceInputError(err) && !rendercontext.IsNamespaceScopeViolation(err) {
			result.Passed = false
			result.Duration = time.Since(startTime)
			return result, false
		}
		// Don't return early - continue to run assertions
		// Some tests expect rendering to fail (negative tests with rendering_error assertions)
	} else {
		// Store rendered content for --dump-rendered flag
		result.RenderedConfig = rendered.HAProxyConfig
		r.storeAuxiliaryFiles(&result, rendered.AuxiliaryFiles)
		if len(rendered.K8sResources) > 0 {
			result.RenderedK8sResources = rendered.K8sResources
		}
		if len(rendered.StatusPatches) > 0 {
			result.RenderedStatusPatches = rendered.StatusPatches
		}
		// Store include stats for --profile-includes flag
		result.IncludeStats = rendered.IncludeStats
	}

	// 6. Build template context for JSONPath assertions
	bctx, err := r.buildRenderingContext(ctx, fixtureStores, validationPaths, httpStore, currentConfig, test.CurrentFiles, effectiveExtraContext)
	if err != nil {
		recordRenderFailure(&result, err, false)
		result.Passed = false
		result.Duration = time.Since(startTime)
		return result, false
	}
	templateContext := bctx.Context

	// 7. Create render dependencies for deterministic assertion (if needed)
	renderDeps := &RenderDependencies{
		Engine:          engine,
		Stores:          fixtureStores,
		ValidationPaths: validationPaths,
		HTTPStore:       httpStore,
		CurrentConfig:   currentConfig,
		CurrentFiles:    test.CurrentFiles,
		ExtraContext:    effectiveExtraContext,
	}

	// 8. Run all assertions (whether rendering succeeded or failed)
	incomplete := r.executeAssertions(ctx, &result, test, rendered.HAProxyConfig, rendered.AuxiliaryFiles, rendered.K8sResources, rendered.StatusPatches, rendered.Events, templateContext, validationPaths, renderDeps)

	// 9. The same fixtures with the deployment's values, where they differ
	deploymentExtraContext := DeploymentExtraContext(r.config, test)
	if !incomplete && result.RenderError == "" && !hasRenderingErrorAssertions(test.Assertions) &&
		!reflect.DeepEqual(deploymentExtraContext, effectiveExtraContext) {
		deployment := r.assertDeploymentRenders(ctx, test, &inputs, &rendered, deploymentExtraContext, engine, validationPaths)
		incomplete = deployment.incomplete
		if !incomplete {
			result.Assertions = append(result.Assertions, deployment)
			result.Passed = result.Passed && deployment.Passed
		}
	}

	// Test passes if either:
	// - Rendering succeeded AND all assertions passed
	// - Rendering failed BUT test has rendering_error assertions that passed
	if result.RenderError != "" && !hasRenderingErrorAssertions(test.Assertions) {
		result.Passed = false
	}

	result.Duration = time.Since(startTime)
	return result, incomplete
}

func (r *Runner) withDeploymentWatchScopes(enabled bool) *Runner {
	if !enabled {
		return r
	}
	scoped := *r
	scoped.namespaceScopes = rendercontext.NamespaceScopes(r.config)
	return &scoped
}

func recordRenderFailure(result *TestResult, err error, expected bool) {
	result.RenderError = templating.RenderErrorMessage(err)
	result.Assertions = append(result.Assertions, AssertionResult{
		Type:           "rendering",
		Description:    "Template rendering failed",
		Passed:         false,
		ConfigRejected: !expected && isTemplateRejection(err),
		Error:          result.RenderError,
	})
}

func isTemplateRejection(err error) bool {
	_, rejected := errors.AsType[*templating.FailError](err)
	return rejected || rendercontext.IsNamespaceScopeViolation(err)
}

func isResourceInputError(err error) bool {
	var resourceInputErr *rendercontext.ResourceInputError
	return errors.As(err, &resourceInputErr)
}

// renderInput bundles everything a test's render reads besides the templates.
type renderInput struct {
	Stores        map[string]stores.Store
	HTTPStore     *FixtureHTTPStoreWrapper
	CurrentConfig *renderplan.CurrentConfig
	ExtraContext  map[string]any
}

// renderInputs assembles a test's render inputs: the _global-merged fixture
// stores and HTTP fixtures, the previously-deployed servers, and the
// extraContext its assertions render with (AssertionExtraContext). Returns a
// non-empty message when an input is unusable — the message lands in
// TestResult.RenderError.
func (r *Runner) renderInputs(testName string, test *config.ValidationTest) (inputs renderInput, failure string) {
	fixtures := test.Fixtures
	httpFixtures := test.HTTPFixtures
	if globalTest, hasGlobal := r.config.ValidationTests["_global"]; hasGlobal {
		fixtures = MergeFixtures(globalTest.Fixtures, test.Fixtures)
		httpFixtures = MergeHTTPFixtures(globalTest.HTTPFixtures, test.HTTPFixtures)
	}

	fixtureStores, err := r.CreateStoresFromFixtures(fixtures)
	if err != nil {
		return renderInput{}, fmt.Sprintf("creating fixture stores: %v", err)
	}

	// The wrapper is always created so http.Fetch() fails gracefully when a
	// fixture is missing.
	httpStore := NewFixtureHTTPStoreWrapper(CreateHTTPStoreFromFixtures(httpFixtures, r.logger), r.logger).
		WithPending(httpFixtures)

	r.logger.Log(context.Background(), logging.LevelTrace, "Assembled render inputs",
		"test", testName,
		"fixture_types", len(fixtures),
		"http_fixtures", len(httpFixtures))

	return renderInput{
		Stores:        fixtureStores,
		HTTPStore:     httpStore,
		CurrentConfig: r.currentServers(test),
		ExtraContext:  AssertionExtraContext(r.config, test),
	}, ""
}

// assertDeploymentRenders renders the test's fixtures with the deployment's
// extraContext (DeploymentExtraContext) and holds that render to what the
// test's own assertions demand of validity: it renders, and passes `haproxy -c`
// when the test asserts haproxy_valid. Content assertions describe the
// templates' defaults, which a deployment may override, so they don't apply.
// A template fail() fails it too: in a reconcile render it stops the whole
// config, so a value that refuses these fixtures breaks every cluster holding
// such a resource.
func (r *Runner) assertDeploymentRenders(ctx context.Context, test *config.ValidationTest, inputs *renderInput, asserted *RenderOutput, extraContext map[string]any, engine templating.Engine, validationPaths *dataplane.ValidationPaths) AssertionResult {
	result := AssertionResult{
		Type:        deploymentAssertionType,
		Description: "Fixtures render with the deployment's extraContext",
		Passed:      true,
	}
	rendered, err := r.renderWithStores(ctx, engine, inputs.Stores, validationPaths, inputs.HTTPStore,
		inputs.CurrentConfig, test.CurrentFiles, extraContext)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			result.incomplete = true
			return result
		}
		result.Passed = false
		result.ConfigRejected = isTemplateRejection(err)
		result.Error = fmt.Sprintf("This test's fixtures fail to render with the deployment's extraContext, "+
			"so a cluster holding such resources stops reconciling: %s. Fix the value the error names.",
			templating.RenderErrorMessage(err))
		return result
	}
	// The same bytes get the same verdict, which the test's own haproxy_valid gave.
	unchanged := rendered.HAProxyConfig == asserted.HAProxyConfig &&
		compareAuxiliaryFiles(asserted.AuxiliaryFiles, rendered.AuxiliaryFiles) == ""
	if unchanged || !hasAssertionType(test.Assertions, "haproxy_valid") {
		return result
	}
	valid := r.assertHAProxyValid(ctx, rendered.HAProxyConfig, rendered.AuxiliaryFiles, &config.ValidationAssertion{}, validationPaths)
	result.incomplete = valid.incomplete
	result.ConfigRejected = valid.ConfigRejected
	if !valid.Passed {
		result.Passed = false
		result.Error = fmt.Sprintf("With the deployment's extraContext, this test's fixtures render a config HAProxy rejects: %s. "+
			"Fix the value the error names.", valid.Error)
		result.Target, result.TargetSize, result.TargetPreview = valid.Target, valid.TargetSize, valid.TargetPreview
	}
	return result
}

// deploymentAssertionType labels the result of assertDeploymentRenders.
const deploymentAssertionType = "deployment_values"

func hasAssertionType(assertions []config.ValidationAssertion, assertionType string) bool {
	for i := range assertions {
		if assertions[i].Type == assertionType {
			return true
		}
	}
	return false
}

// Render renders one validation test and returns everything the render
// produced — including the plan the templates declared — without executing the
// test's assertions. It renders with the deployment's extraContext, so a diff
// shows what a config change does to the test's scenario. It renders into the
// runner's base validation paths, which nothing writes to while no assertion
// runs.
func (r *Runner) Render(ctx context.Context, testName string) (RenderOutput, error) {
	test, found := r.config.ValidationTests[testName]
	if !found {
		return RenderOutput{}, fmt.Errorf("test %q not found", testName)
	}
	inputs, inputErr := r.renderInputs(testName, &test)
	if inputErr != "" {
		return RenderOutput{}, errors.New(inputErr)
	}
	return r.renderWithStores(ctx, r.engineTemplate, inputs.Stores, r.validationPaths,
		inputs.HTTPStore, inputs.CurrentConfig, test.CurrentFiles, DeploymentExtraContext(r.config, &test))
}

// RenderWithoutFixtures renders the configuration against empty stores: no
// watched resource exists, no HTTP fixture answers, nothing was deployed
// before. It is the configuration's own skeleton — what `haptic diff` compares
// when the operator names no validationTest, so the answer is about the
// configuration change rather than about one test's fixtures.
func (r *Runner) RenderWithoutFixtures(ctx context.Context) (RenderOutput, error) {
	httpStore := NewFixtureHTTPStoreWrapper(CreateHTTPStoreFromFixtures(nil, r.logger), r.logger)
	return r.renderWithStores(ctx, r.engineTemplate, r.createEmptyStores(), r.validationPaths,
		httpStore, nil, nil, r.config.TemplatingSettings.ExtraContext)
}

// currentServers resolves what a test declares about the previous deployment
// into the shape templates read as `currentConfig`. Returns nil when the test
// declares nothing.
func (r *Runner) currentServers(test *config.ValidationTest) *renderplan.CurrentConfig {
	if len(test.CurrentServers) == 0 {
		return nil
	}
	return currentConfigFromFixture(test.CurrentServers)
}

// currentConfigFromFixture projects the `currentServers` fixture into the
// template-facing shape.
func currentConfigFromFixture(fixture map[string]map[string]config.ServerAddr) *renderplan.CurrentConfig {
	index := make(map[string]map[string]renderplan.ServerAddr, len(fixture))
	for backend, servers := range fixture {
		addresses := make(map[string]renderplan.ServerAddr, len(servers))
		for name := range servers {
			addresses[name] = renderplan.ServerAddr{Address: servers[name].Address, Port: servers[name].Port}
		}
		index[backend] = addresses
	}
	return &renderplan.CurrentConfig{ServerIndex: index}
}

// storeAuxiliaryFiles stores rendered auxiliary files in the test result for --dump-rendered flag.
func (r *Runner) storeAuxiliaryFiles(result *TestResult, auxiliaryFiles *dataplane.AuxiliaryFiles) {
	if auxiliaryFiles == nil {
		return
	}

	result.RenderedMaps = collectByKey(auxiliaryFiles.MapFiles,
		func(m auxiliaryfiles.MapFile) string { return m.Path },
		func(m auxiliaryfiles.MapFile) string { return m.Content })
	result.RenderedFiles = collectByKey(auxiliaryFiles.GeneralFiles,
		func(f auxiliaryfiles.GeneralFile) string { return f.Filename },
		func(f auxiliaryfiles.GeneralFile) string { return f.Content })
	result.RenderedCerts = collectByKey(auxiliaryFiles.SSLCertificates,
		func(c auxiliaryfiles.SSLCertificate) string { return c.Path },
		func(c auxiliaryfiles.SSLCertificate) string { return c.Content })
}

// collectByKey returns a fresh map keyed by key(item) with values content(item),
// or nil for empty input so callers can leave their pointer fields unset rather
// than holding empty maps. The three RenderedMaps/RenderedFiles/RenderedCerts
// fields all share this "iterate, key by one field, value by another" shape.
func collectByKey[T any](items []T, key, content func(T) string) map[string]string {
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		out[key(item)] = content(item)
	}
	return out
}
