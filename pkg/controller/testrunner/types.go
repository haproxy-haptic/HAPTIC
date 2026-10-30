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

package testrunner

import (
	"log/slog"
	"reflect"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// Runner executes validation tests for HAProxyTemplateConfig.
//
// It's a pure component with no EventBus dependency, designed to be called
// directly from the CLI or from the DryRunValidator.
type Runner struct {
	// engineTemplate is shared; paths and filter state belong to each render.
	engineTemplate     templating.Engine
	planTokenAuthority *rendercontext.PlanTokenAuthority
	validationPaths    *dataplane.ValidationPaths // Base paths (used to create worker-specific paths)
	config             *config.Config
	logger             *slog.Logger
	workers            int
	// checkGate bounds concurrent `haproxy -c` runs across workers. Without a
	// dedicated gate the workers serialize behind dataplane's single-slot default
	// gate, defeating the pool; sized to the worker count in RunTests.
	checkGate       *dataplane.CheckGate
	debugFilters    bool                   // Enable detailed filter operation logging
	traceTemplates  bool                   // Enable template execution tracing
	profileIncludes bool                   // Enable include timing profiling
	capabilities    dataplane.Capabilities // what the HAProxy version supports
	haproxyVersion  *dataplane.Version     // Local HAProxy version for test skipping

	// typedResourceTypes carries the typed reflect.Types produced by
	// typebootstrap for the offline validate path. Mirrors what
	// renderer.RenderService receives in production. When non-empty,
	// buildRenderingContext forwards it to rendercontext.NewBuilder
	// via WithTypedResources so chart templates that use the typed
	// `gateways` (etc.) globals compile against the same shape they
	// would in production.
	typedResourceTypes map[string]reflect.Type

	// checkWithoutBinary replaces `haproxy -c` where there is neither a
	// filesystem nor the binary. See Options.CheckWithoutBinary.
	checkWithoutBinary func(haproxyConfig string) error

	// coldAnalysis is built once and shared by every render of engineTemplate.
	coldAnalysis    func() *renderer.ColdIncrementalRenderAnalysis
	namespaceScopes func(string) *rendercontext.NamespaceScope
}

// testEntry is a tuple of test name and test definition for worker processing.
type testEntry struct {
	name string
	test config.ValidationTest
}

// Options configures the test runner.
type Options struct {
	// TestName filters tests to run. If empty, all tests run.
	TestName string

	// Logger for structured logging. If nil, uses default logger.
	Logger *slog.Logger

	// Workers is the number of parallel workers for test execution.
	// Default (0): twice GOMAXPROCS, capped at one worker per 128 MiB of GOMEMLIMIT.
	// Set to 1 for sequential execution.
	Workers int

	// DebugFilters enables detailed filter operation logging.
	// When enabled, each sort comparison is logged with values and results.
	DebugFilters bool

	// ProfileIncludes enables include timing profiling.
	// When enabled, shows which included templates take the most time.
	ProfileIncludes bool

	// Capabilities defines which features are available for the local HAProxy version.
	// Used to determine path resolution (e.g., CRT-list paths fallback when not supported).
	Capabilities dataplane.Capabilities

	// HAProxyVersion is the detected local HAProxy version.
	// When set, tests with MinHAProxyVersion above this version are skipped.
	HAProxyVersion *dataplane.Version

	// CheckWithoutBinary answers `haproxy_valid` where there is neither a
	// writable filesystem nor the haproxy binary — the browser (WASM)
	// playground. Non-nil also means per-test temp directories are not created.
	// Whatever it checks is weaker than `haproxy -c`, so callers must label the
	// results as what their check actually covers.
	CheckWithoutBinary func(haproxyConfig string) error

	// TypedResourceTypes is the per-resource generated Go type the
	// engine was constructed with via typebootstrap. Same shape as
	// renderer.RenderServiceConfig.TypedResourceTypes — see
	// pkg/controller/renderer/typed_resources.go for the production
	// counterpart. Passed straight through to
	// rendercontext.WithTypedResources so the offline render context
	// includes the typed top-level globals the engine compiled
	// against. When nil/empty, the test runner behaves as before
	// (no typed globals injected — chart code keeps using
	// resources.<name>.List() through dig()).
	TypedResourceTypes map[string]reflect.Type
}

// TestResults contains the results of running validation tests.
type TestResults struct {
	// TotalTests is the total number of tests executed (excluding skipped).
	TotalTests int

	// PassedTests is the number of tests that passed all assertions.
	PassedTests int

	// FailedTests is the number of tests with at least one failed assertion.
	FailedTests int

	// SkippedTests is the number of tests skipped (e.g., due to HAProxy version requirements).
	SkippedTests int

	// TestResults contains detailed results for each test.
	TestResults []TestResult

	// Duration is the total time taken to run all tests.
	Duration time.Duration
}

// AllPassed returns true if all tests passed.
func (r *TestResults) AllPassed() bool {
	return r.FailedTests == 0 && r.TotalTests > 0
}

// TestResult contains the result of running a single validation test.
type TestResult struct {
	// TestName is the name of the test.
	TestName string

	// Description is the test description.
	Description string

	// Passed is true if all assertions passed.
	Passed bool

	// Skipped is true if the test was skipped (e.g., HAProxy version too low).
	Skipped bool

	// SkipReason explains why the test was skipped.
	SkipReason string

	// Duration is the time taken to run this test.
	Duration time.Duration

	// Assertions contains results for each assertion.
	Assertions []AssertionResult

	// RenderError is set if template rendering failed.
	RenderError string

	// RenderedConfig contains the rendered HAProxy configuration (for --dump-rendered).
	RenderedConfig string `json:"renderedConfig,omitempty" yaml:"renderedConfig,omitempty"`

	// RenderedMaps contains rendered map files (for --dump-rendered).
	RenderedMaps map[string]string `json:"renderedMaps,omitempty" yaml:"renderedMaps,omitempty"`

	// RenderedFiles contains rendered general files (for --dump-rendered).
	RenderedFiles map[string]string `json:"renderedFiles,omitempty" yaml:"renderedFiles,omitempty"`

	// RenderedCerts contains rendered SSL certificates (for --dump-rendered).
	RenderedCerts map[string]string `json:"renderedCerts,omitempty" yaml:"renderedCerts,omitempty"`

	// RenderedK8sResources contains rendered output of every
	// `spec.k8sResources` template (template name → rendered YAML).
	// Asserted via `target: k8s:<template-name>`.
	RenderedK8sResources map[string]string `json:"renderedK8sResources,omitempty" yaml:"renderedK8sResources,omitempty"`

	// RenderedStatusPatches contains the JSON-marshalled status payload
	// emitted by every `statusPatch()` call during the haproxy.cfg
	// render, keyed by `<namespace>/<name>:<phase>` (cluster-scoped
	// resources use `:<phase>` with empty namespace). Asserted via
	// `target: status:<namespace>/<name>:<phase>`.
	RenderedStatusPatches map[string]string `json:"renderedStatusPatches,omitempty" yaml:"renderedStatusPatches,omitempty"`

	// IncludeStats contains timing statistics for included templates (for --profile-includes).
	IncludeStats []templating.IncludeStats `json:"includeStats,omitempty" yaml:"includeStats,omitempty"`
}

// AssertionResult contains the result of running a single assertion.
type AssertionResult struct {
	// ConfigRejected distinguishes a checked invalid input from an unavailable checker.
	ConfigRejected bool `json:"configRejected,omitempty" yaml:"configRejected,omitempty"`
	// Type is the assertion type (haproxy_valid, contains, etc).
	Type string

	// Description is the assertion description.
	Description string

	// Passed is true if the assertion passed.
	Passed bool

	// Error contains the failure message if assertion failed.
	Error string

	// Target is the assertion target (e.g., "haproxy.cfg", "map:path-prefix.map").
	Target string `json:"target,omitempty" yaml:"target,omitempty"`

	// TargetSize is the size of the target content in bytes.
	TargetSize int `json:"targetSize,omitempty" yaml:"targetSize,omitempty"`

	// TargetPreview is a preview of the target content (first 200 chars, only for failed assertions).
	TargetPreview string `json:"targetPreview,omitempty" yaml:"targetPreview,omitempty"`

	incomplete bool
}
