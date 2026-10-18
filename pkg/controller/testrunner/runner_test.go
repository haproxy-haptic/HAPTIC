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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/dataplanetest"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// Helper function to create RawExtension from map.
func mustMarshalRawExtension(obj map[string]any) runtime.RawExtension {
	data, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return runtime.RawExtension{Raw: data}
}

// servicesWatch is the single watched resource these tests declare. It returns a
// fresh map because the runner takes the spec by pointer.
func servicesWatch() map[string]v1alpha1.WatchedResource {
	return map[string]v1alpha1.WatchedResource{
		"services": {APIVersion: "v1", Resources: "services", IndexBy: []string{"metadata.namespace", "metadata.name"}},
	}
}

func TestRunner_RunTests(t *testing.T) {
	tests := []struct {
		name            string
		config          *v1alpha1.HAProxyTemplateConfigSpec
		testName        string
		wantErr         bool
		wantTotalTests  int
		wantPassedTests int
		wantFailedTests int
		skipValidation  bool // Skip HAProxy validation for tests without HAProxy binary
	}{
		{
			name: "simple rendering test with contains assertion",
			config: &v1alpha1.HAProxyTemplateConfigSpec{
				HAProxyConfig:    v1alpha1.HAProxyConfig{Template: "global\n  maxconn 1000\n"},
				WatchedResources: servicesWatch(),
				ValidationTests: map[string]v1alpha1.ValidationTest{
					"basic-rendering": {
						Description: "Test basic HAProxy rendering",
						Fixtures: map[string][]runtime.RawExtension{
							"services": {},
						},
						Assertions: []v1alpha1.ValidationAssertion{
							{
								Type:        "contains",
								Target:      "haproxy.cfg",
								Pattern:     "maxconn 1000",
								Description: "HAProxy config should contain maxconn 1000",
							},
						},
					},
				},
			},
			wantErr:         false,
			wantTotalTests:  1,
			wantPassedTests: 1,
			wantFailedTests: 0,
			skipValidation:  true,
		},
		{
			name: "test with failing assertion",
			config: &v1alpha1.HAProxyTemplateConfigSpec{
				HAProxyConfig:    v1alpha1.HAProxyConfig{Template: "global\n  maxconn 1000\n"},
				WatchedResources: servicesWatch(),
				ValidationTests: map[string]v1alpha1.ValidationTest{
					"failing-test": {
						Description: "Test with failing assertion",
						Fixtures: map[string][]runtime.RawExtension{
							"services": {},
						},
						Assertions: []v1alpha1.ValidationAssertion{
							{
								Type:        "contains",
								Target:      "haproxy.cfg",
								Pattern:     "this-does-not-exist",
								Description: "Should not find this pattern",
							},
						},
					},
				},
			},
			wantErr:         false,
			wantTotalTests:  1,
			wantPassedTests: 0,
			wantFailedTests: 1,
			skipValidation:  true,
		},
		{
			name: "multiple tests with mixed results",
			config: &v1alpha1.HAProxyTemplateConfigSpec{
				HAProxyConfig:    v1alpha1.HAProxyConfig{Template: "global\n  maxconn 1000\n"},
				WatchedResources: servicesWatch(),
				ValidationTests: map[string]v1alpha1.ValidationTest{
					"passing-test": {
						Description: "This test should pass",
						Fixtures: map[string][]runtime.RawExtension{
							"services": {},
						},
						Assertions: []v1alpha1.ValidationAssertion{
							{
								Type:    "contains",
								Target:  "haproxy.cfg",
								Pattern: "maxconn",
							},
						},
					},
					"failing-test": {
						Description: "This test should fail",
						Fixtures: map[string][]runtime.RawExtension{
							"services": {},
						},
						Assertions: []v1alpha1.ValidationAssertion{
							{
								Type:    "contains",
								Target:  "haproxy.cfg",
								Pattern: "invalid-pattern",
							},
						},
					},
				},
			},
			wantErr:         false,
			wantTotalTests:  2,
			wantPassedTests: 1,
			wantFailedTests: 1,
			skipValidation:  true,
		},
		{
			name: "filter specific test by name",
			config: &v1alpha1.HAProxyTemplateConfigSpec{
				HAProxyConfig:    v1alpha1.HAProxyConfig{Template: "global\n  maxconn 1000\n"},
				WatchedResources: servicesWatch(),
				ValidationTests: map[string]v1alpha1.ValidationTest{
					"test-1": {
						Description: "First test",
						Fixtures: map[string][]runtime.RawExtension{
							"services": {},
						},
						Assertions: []v1alpha1.ValidationAssertion{
							{
								Type:    "contains",
								Target:  "haproxy.cfg",
								Pattern: "maxconn",
							},
						},
					},
					"test-2": {
						Description: "Second test",
						Fixtures: map[string][]runtime.RawExtension{
							"services": {},
						},
						Assertions: []v1alpha1.ValidationAssertion{
							{
								Type:    "contains",
								Target:  "haproxy.cfg",
								Pattern: "maxconn",
							},
						},
					},
				},
			},
			testName:        "test-1",
			wantErr:         false,
			wantTotalTests:  1,
			wantPassedTests: 1,
			wantFailedTests: 0,
			skipValidation:  true,
		},
		{
			name: "non-existent test name",
			config: &v1alpha1.HAProxyTemplateConfigSpec{
				HAProxyConfig:    v1alpha1.HAProxyConfig{Template: "global\n  maxconn 1000\n"},
				WatchedResources: servicesWatch(),
				ValidationTests: map[string]v1alpha1.ValidationTest{
					"test-1": {
						Fixtures: map[string][]runtime.RawExtension{
							"services": {},
						},
						Assertions: []v1alpha1.ValidationAssertion{
							{
								Type:    "contains",
								Target:  "haproxy.cfg",
								Pattern: "maxconn",
							},
						},
					},
				},
			},
			testName: "non-existent",
			wantErr:  true,
		},
		{
			name: "_global fixtures entry is excluded from execution",
			config: &v1alpha1.HAProxyTemplateConfigSpec{
				HAProxyConfig:    v1alpha1.HAProxyConfig{Template: "global\n  maxconn 1000\n"},
				WatchedResources: servicesWatch(),
				ValidationTests: map[string]v1alpha1.ValidationTest{
					"_global": {
						Description: "shared fixtures only; must never run as a standalone test",
						Fixtures: map[string][]runtime.RawExtension{
							"services": {},
						},
						Assertions: []v1alpha1.ValidationAssertion{
							{
								// Would fail if _global were ever executed.
								Type:    "contains",
								Target:  "haproxy.cfg",
								Pattern: "this-pattern-would-fail-if-global-executed",
							},
						},
					},
					"real-test": {
						Description: "the only runnable test",
						Fixtures: map[string][]runtime.RawExtension{
							"services": {},
						},
						Assertions: []v1alpha1.ValidationAssertion{
							{
								Type:    "contains",
								Target:  "haproxy.cfg",
								Pattern: "maxconn",
							},
						},
					},
				},
			},
			wantErr:         false,
			wantTotalTests:  1,
			wantPassedTests: 1,
			wantFailedTests: 0,
			skipValidation:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
			// Create template engine
			templates := map[string]string{
				"haproxy.cfg": tt.config.HAProxyConfig.Template,
			}
			engine, err := templating.New(templates, nil)
			require.NoError(t, err)

			// Convert CRD spec to internal config format
			cfg, err := conversion.ConvertSpec(tt.config)
			require.NoError(t, err)

			// Create test runner
			runner := New(
				cfg,
				engine,
				&dataplane.ValidationPaths{}, // Empty paths for unit tests
				&Options{
					TestName: tt.testName,
					Logger:   logger,
				},
			)

			// Run tests
			ctx := context.Background()
			results, err := runner.RunTests(ctx, tt.testName)

			// Check error expectation
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)

			// Verify results
			assert.Equal(t, tt.wantTotalTests, results.TotalTests, "total tests mismatch")
			assert.Equal(t, tt.wantPassedTests, results.PassedTests, "passed tests mismatch")
			assert.Equal(t, tt.wantFailedTests, results.FailedTests, "failed tests mismatch")
			assert.Len(t, results.TestResults, tt.wantTotalTests, "test results length mismatch")
			if tt.wantTotalTests > 0 {
				assert.Contains(t, logs.String(), `level=INFO msg="Test run completed"`)
				assert.Contains(t, logs.String(), fmt.Sprintf("total=%d passed=%d failed=%d skipped=%d",
					results.TotalTests, results.PassedTests, results.FailedTests, results.SkippedTests))
			}

			// Verify AllPassed() method
			if tt.wantFailedTests == 0 && tt.wantTotalTests > 0 {
				assert.True(t, results.AllPassed(), "AllPassed() should return true")
			} else {
				assert.False(t, results.AllPassed(), "AllPassed() should return false")
			}
		})
	}
}

// TestRunner_GlobalExtraContextIsolation pins the _global validationTest's
// extraContext as a baseline every test renders with, overridden by a per-test
// extraContext, while the deployment's extraContext never reaches an assertion.
func TestRunner_GlobalExtraContextIsolation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	spec := &v1alpha1.HAProxyTemplateConfigSpec{
		TemplatingSettings: v1alpha1.TemplatingSettings{
			Engine: "scriggo",
			// The deployment's real extraContext — the value a synthetic test
			// must NOT see when it doesn't override it itself.
			ExtraContext: mustMarshalRawExtension(map[string]any{"marker": "production-real"}),
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: "global\n  # marker=" + `{{ extraContext | dig("marker") | fallback("none") | tostring() }}` + "\n",
		},
		ValidationTests: map[string]v1alpha1.ValidationTest{
			"_global": {
				Description:  "shared isolated baseline (never runs standalone)",
				ExtraContext: mustMarshalRawExtension(map[string]any{"marker": "global-baseline"}),
			},
			"test-inherits-global": {
				Description: "no per-test extraContext resolves the _global baseline, not production",
				Assertions: []v1alpha1.ValidationAssertion{
					{Type: "contains", Target: "haproxy.cfg", Pattern: "marker=global-baseline"},
					{Type: "not_contains", Target: "haproxy.cfg", Pattern: "marker=production-real"},
				},
			},
			"test-overrides-global": {
				Description:  "per-test extraContext wins over the _global baseline",
				ExtraContext: mustMarshalRawExtension(map[string]any{"marker": "per-test"}),
				Assertions: []v1alpha1.ValidationAssertion{
					{Type: "contains", Target: "haproxy.cfg", Pattern: "marker=per-test"},
				},
			},
		},
	}

	engine, err := templating.New(map[string]string{"haproxy.cfg": spec.HAProxyConfig.Template}, nil)
	require.NoError(t, err)

	cfg, err := conversion.ConvertSpec(spec)
	require.NoError(t, err)

	runner := New(cfg, engine, &dataplane.ValidationPaths{}, &Options{Logger: logger})

	results, err := runner.RunTests(context.Background(), "")
	require.NoError(t, err)

	assert.Equal(t, 2, results.TotalTests, "_global is excluded; only the two real tests run")
	assert.Equal(t, 2, results.PassedTests, "both isolation tests pass")
	assert.Equal(t, 0, results.FailedTests)
	assert.True(t, results.AllPassed())
}

// TestRunner_DeploymentValues pins the split: an assertion on a template default
// passes whatever the deployment sets, while the deployment's values still have
// to render the same fixtures and pass haproxy -c.
func TestRunner_DeploymentValues(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restore := dataplanetest.InstallFakeHAProxy(dataplanetest.WithCheck(func(workDir string, _ []string) ([]byte, error) {
		cfg, err := os.ReadFile(filepath.Join(workDir, "haproxy.cfg"))
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(cfg), "port=rejected") {
			return []byte("[ALERT]    (1) : config : invalid port\n"), errors.New("exit status 1")
		}
		if strings.Contains(string(cfg), "port=unavailable") {
			return nil, errors.New("cannot execute HAProxy: permission denied")
		}
		return nil, nil
	}))
	t.Cleanup(restore)

	template := "global\n  # port={{ extraContext | dig(\"port\") | fallback(\"default\") | tostring() }}\n" +
		"{%- if (extraContext | dig(\"breaks\") | fallback(false)) == true %}{{ fail(\"breaks is set\") }}{% end %}\n" +
		"{%- if (extraContext | dig(\"strict\") | fallback(false)) == true and (extraContext | dig(\"weak\") | fallback(false)) == true %}" +
		"{{ fail(\"weak input refused\") }}{% end %}\n"
	defaultAssertion := v1alpha1.ValidationTest{
		Assertions: []v1alpha1.ValidationAssertion{
			{Type: "contains", Target: "haproxy.cfg", Pattern: "port=default"},
			{Type: "haproxy_valid"},
		},
	}
	tests := []struct {
		name         string
		deployment   map[string]any
		test         v1alpha1.ValidationTest
		wantPassed   bool
		wantError    string
		wantRejected bool
	}{
		{
			name:       "an overridden default does not fail the assertion",
			deployment: map[string]any{"port": "9443"},
			test:       defaultAssertion,
			wantPassed: true,
		},
		{
			name:         "a value that fails the render fails the test",
			deployment:   map[string]any{"breaks": true},
			test:         defaultAssertion,
			wantError:    "breaks is set",
			wantRejected: true,
		},
		{
			name:       "a value that refuses only these fixtures fails the test",
			deployment: map[string]any{"strict": true},
			test: v1alpha1.ValidationTest{
				ExtraContext: mustMarshalRawExtension(map[string]any{"weak": true}),
				Assertions: []v1alpha1.ValidationAssertion{
					{Type: "contains", Target: "haproxy.cfg", Pattern: "port=default"},
				},
			},
			wantError:    "weak input refused",
			wantRejected: true,
		},
		{
			name:         "a value HAProxy rejects fails the test",
			deployment:   map[string]any{"port": "rejected"},
			test:         defaultAssertion,
			wantError:    "invalid port",
			wantRejected: true,
		},
		{
			name:       "an unavailable deployment checker is not an input rejection",
			deployment: map[string]any{"port": "unavailable"},
			test:       defaultAssertion,
			wantError:  "permission denied",
		},
		{
			name:       "haproxy -c runs only where the test asserts haproxy_valid",
			deployment: map[string]any{"port": "rejected"},
			test: v1alpha1.ValidationTest{Assertions: []v1alpha1.ValidationAssertion{
				{Type: "contains", Target: "haproxy.cfg", Pattern: "port=default"},
			}},
			wantPassed: true,
		},
		{
			name:       "a test expecting a render error skips the check",
			deployment: map[string]any{"breaks": true},
			test: v1alpha1.ValidationTest{
				ExtraContext: mustMarshalRawExtension(map[string]any{"breaks": true}),
				Assertions: []v1alpha1.ValidationAssertion{
					{Type: "contains", Target: "rendering_error", Pattern: "breaks is set"},
				},
			},
			wantPassed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &v1alpha1.HAProxyTemplateConfigSpec{
				TemplatingSettings: v1alpha1.TemplatingSettings{
					Engine:       "scriggo",
					ExtraContext: mustMarshalRawExtension(tt.deployment),
				},
				HAProxyConfig:   v1alpha1.HAProxyConfig{Template: template},
				ValidationTests: map[string]v1alpha1.ValidationTest{"test-default": tt.test},
			}
			engine, err := templating.New(map[string]string{"haproxy.cfg": template}, nil)
			require.NoError(t, err)
			cfg, err := conversion.ConvertSpec(spec)
			require.NoError(t, err)
			tempDir := t.TempDir()
			paths := &dataplane.ValidationPaths{
				MapsDir:           filepath.Join(tempDir, "maps"),
				SSLCertsDir:       filepath.Join(tempDir, "ssl"),
				GeneralStorageDir: filepath.Join(tempDir, "general"),
				ConfigFile:        filepath.Join(tempDir, "haproxy.cfg"),
			}

			results, err := New(cfg, engine, paths, &Options{Logger: logger, Workers: 1}).RunTests(t.Context(), "")
			require.NoError(t, err)

			require.Len(t, results.TestResults, 1)
			result := results.TestResults[0]
			assert.Equal(t, tt.wantPassed, result.Passed)
			if tt.wantError != "" {
				last := result.Assertions[len(result.Assertions)-1]
				assert.Equal(t, deploymentAssertionType, last.Type)
				assert.Contains(t, last.Error, tt.wantError)
				assert.Equal(t, tt.wantRejected, last.ConfigRejected)
			}
		})
	}
}

func TestRunner_RunTests_WithFixtures(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Test with fixtures that are used in template (Scriggo syntax with direct method calls)
	config := &v1alpha1.HAProxyTemplateConfigSpec{
		TemplatingSettings: v1alpha1.TemplatingSettings{
			Engine: "scriggo",
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: `global
  maxconn 1000

{% for _, svc := range resources.services.List() -%}
{% var svcMeta = svc.(map[string]any)["metadata"].(map[string]any) -%}
{% var svcSpec = svc.(map[string]any)["spec"].(map[string]any) -%}
backend {{ svcMeta["namespace"] }}-{{ svcMeta["name"] }}
  server {{ svcMeta["name"] }} {{ svcSpec["clusterIP"] }}:80
{% end %}
`,
		},
		WatchedResources: servicesWatch(),
		ValidationTests: map[string]v1alpha1.ValidationTest{
			"with-service-fixture": {
				Description: "Test with service fixture",
				Fixtures: map[string][]runtime.RawExtension{
					"services": {
						mustMarshalRawExtension(map[string]any{
							"metadata": map[string]any{
								"name":      "test-service",
								"namespace": "default",
							},
							"spec": map[string]any{
								"clusterIP": "10.0.0.1",
							},
						}),
					},
				},
				Assertions: []v1alpha1.ValidationAssertion{
					{
						Type:        "contains",
						Target:      "haproxy.cfg",
						Pattern:     "backend default-test-service",
						Description: "Should contain backend for test-service",
					},
					{
						Type:        "contains",
						Target:      "haproxy.cfg",
						Pattern:     "server test-service 10.0.0.1:80",
						Description: "Should contain server entry",
					},
				},
			},
		},
	}

	templates := map[string]string{
		"haproxy.cfg": config.HAProxyConfig.Template,
	}
	// Declare `resources` to match the typed-struct shape that
	// testrunner.buildRenderingContext produces via
	// rendercontext.BuildResourcesValue. The engine no longer ships an
	// untyped `resources` default — every consumer must supply the
	// typed declaration (production uses helpers.BuildAdditionalDeclarations).
	decls := typebootstrap.BuildEngineDeclarations(&typebootstrap.Result{}, "services")
	engine, err := templating.New(templates, &templating.Options{EntryPoints: []string{"haproxy.cfg"}, Declarations: decls})
	require.NoError(t, err)

	// Convert CRD spec to internal config format
	cfg, err := conversion.ConvertSpec(config)
	require.NoError(t, err)

	runner := New(
		cfg,
		engine,
		&dataplane.ValidationPaths{},
		&Options{Logger: logger},
	)

	ctx := context.Background()
	results, err := runner.RunTests(ctx, "")
	require.NoError(t, err)

	// Verify test passed
	assert.Equal(t, 1, results.TotalTests)
	assert.Equal(t, 1, results.PassedTests)
	assert.Equal(t, 0, results.FailedTests)
	assert.True(t, results.AllPassed())

	// Verify all assertions passed
	require.Len(t, results.TestResults, 1)
	testResult := results.TestResults[0]
	assert.True(t, testResult.Passed)
	assert.Len(t, declaredAssertions(t, &testResult), 2)
	for _, assertion := range testResult.Assertions {
		assert.True(t, assertion.Passed, "assertion %s should pass: %s", assertion.Type, assertion.Error)
	}
}

func TestRunner_RenderError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Test with template that causes rendering error (calls fail())
	config := &v1alpha1.HAProxyTemplateConfigSpec{
		TemplatingSettings: v1alpha1.TemplatingSettings{
			Engine: "scriggo",
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			// Use fail() to cause rendering error
			Template: `{{ fail("intentional error") }}`,
		},
		WatchedResources: servicesWatch(),
		ValidationTests: map[string]v1alpha1.ValidationTest{
			"rendering-error-test": {
				Description: "Test with rendering error",
				Fixtures: map[string][]runtime.RawExtension{
					"services": {},
				},
				Assertions: []v1alpha1.ValidationAssertion{
					{
						Type:    "contains",
						Target:  "haproxy.cfg",
						Pattern: "anything",
					},
				},
			},
		},
	}

	templates := map[string]string{
		"haproxy.cfg": config.HAProxyConfig.Template,
	}
	engine, err := templating.New(templates, nil)
	require.NoError(t, err)

	// Convert CRD spec to internal config format
	cfg, err := conversion.ConvertSpec(config)
	require.NoError(t, err)

	runner := New(
		cfg,
		engine,
		&dataplane.ValidationPaths{},
		&Options{Logger: logger},
	)

	ctx := context.Background()
	results, err := runner.RunTests(ctx, "")
	require.NoError(t, err)

	// Verify test failed due to rendering error
	assert.Equal(t, 1, results.TotalTests)
	assert.Equal(t, 0, results.PassedTests)
	assert.Equal(t, 1, results.FailedTests)
	assert.False(t, results.AllPassed())

	// Verify rendering error is captured
	require.Len(t, results.TestResults, 1)
	testResult := results.TestResults[0]
	assert.False(t, testResult.Passed)
	assert.NotEmpty(t, testResult.RenderError, "render error should be populated")

	// Verify rendering failure is added as assertion, plus the original assertion also failed
	assert.Len(t, declaredAssertions(t, &testResult), 2)
	assert.Equal(t, "rendering", testResult.Assertions[0].Type)
	assert.False(t, testResult.Assertions[0].Passed)
	assert.Equal(t, "contains", testResult.Assertions[1].Type)
	assert.False(t, testResult.Assertions[1].Passed)
}

func TestRunner_ResourceInputErrorCannotSatisfyRenderingErrorAssertion(t *testing.T) {
	elemType := reflect.StructOf([]reflect.StructField{
		{Name: "Count", Type: reflect.TypeOf(0), Tag: `json:"count"`},
	})
	typedResult := &typebootstrap.Result{Types: map[string]reflect.Type{"widgets": elemType}}
	spec := &v1alpha1.HAProxyTemplateConfigSpec{
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: `# count: {{ len(resources.widgets.List()) }}`,
		},
		WatchedResources: map[string]v1alpha1.WatchedResource{
			"widgets": {
				APIVersion: "example.io/v1",
				Resources:  "widgets",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		ValidationTests: map[string]v1alpha1.ValidationTest{
			"bad-resource-input": {
				Fixtures: map[string][]runtime.RawExtension{
					"widgets": {mustMarshalRawExtension(map[string]any{
						"metadata": map[string]any{"namespace": "default", "name": "bad"},
						"count":    "not-a-number",
					})},
				},
				Assertions: []v1alpha1.ValidationAssertion{{
					Type:    "contains",
					Target:  "rendering_error",
					Pattern: "reading template resources",
				}},
			},
		},
	}
	engine, err := templating.New(
		map[string]string{"haproxy.cfg": spec.HAProxyConfig.Template},
		&templating.Options{
			EntryPoints:  []string{"haproxy.cfg"},
			Declarations: typebootstrap.BuildEngineDeclarations(typedResult),
		},
	)
	require.NoError(t, err)
	cfg, err := conversion.ConvertSpec(spec)
	require.NoError(t, err)
	runner := New(cfg, engine, &dataplane.ValidationPaths{}, &Options{
		Workers:            1,
		TypedResourceTypes: typedResult.Types,
	})

	results, err := runner.RunTests(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, results.TestResults, 1)
	assert.Equal(t, 1, results.FailedTests)
	assert.False(t, results.TestResults[0].Passed)
	assert.Contains(t, results.TestResults[0].RenderError, "reading template resources")
	assert.Len(t, results.TestResults[0].Assertions, 1)
	assert.Equal(t, "rendering", results.TestResults[0].Assertions[0].Type)
}

func TestTestResults_AllPassed(t *testing.T) {
	tests := []struct {
		name   string
		result *TestResults
		want   bool
	}{
		{
			name: "all tests passed",
			result: &TestResults{
				TotalTests:  2,
				PassedTests: 2,
				FailedTests: 0,
			},
			want: true,
		},
		{
			name: "some tests failed",
			result: &TestResults{
				TotalTests:  2,
				PassedTests: 1,
				FailedTests: 1,
			},
			want: false,
		},
		{
			name: "all tests failed",
			result: &TestResults{
				TotalTests:  2,
				PassedTests: 0,
				FailedTests: 2,
			},
			want: false,
		},
		{
			name: "no tests run",
			result: &TestResults{
				TotalTests:  0,
				PassedTests: 0,
				FailedTests: 0,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.result.AllPassed()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRunner_RunTests_WithHTTPFixtures(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Test with HTTP fixtures used in template
	config := &v1alpha1.HAProxyTemplateConfigSpec{
		TemplatingSettings: v1alpha1.TemplatingSettings{
			Engine: "scriggo",
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: `global
  maxconn 1000
`,
		},
		Maps: map[string]v1alpha1.MapFile{
			"blocklist.map": {
				Template: `{%- var blocklist, err = http.Fetch("http://blocklist.example.com/list.txt") -%}
{%- if err != nil %}{{ err }}{% end -%}
{%- var blocklistStr = blocklist.(string) -%}
{%- for _, line := range split(blocklistStr, "\n") -%}
{%- var value = trim(line, " \t\n\r") -%}
{%- if value != "" -%}
{{ value }} 1
{% end -%}
{%- end %}`,
			},
		},
		WatchedResources: servicesWatch(),
		ValidationTests: map[string]v1alpha1.ValidationTest{
			"http-fixture-test": {
				Description: "Test with HTTP fixture",
				Fixtures: map[string][]runtime.RawExtension{
					"services": {},
				},
				HTTPResources: []v1alpha1.HTTPResourceFixture{
					{
						URL:     "http://blocklist.example.com/list.txt",
						Content: "blocked-value-1\nblocked-value-2\nblocked-value-3",
					},
				},
				Assertions: []v1alpha1.ValidationAssertion{
					{
						Type:        "contains",
						Target:      "map:blocklist.map",
						Pattern:     "blocked-value-1",
						Description: "Blocklist map should contain blocked-value-1",
					},
					{
						Type:        "contains",
						Target:      "map:blocklist.map",
						Pattern:     "blocked-value-2",
						Description: "Blocklist map should contain blocked-value-2",
					},
					{
						Type:        "contains",
						Target:      "map:blocklist.map",
						Pattern:     "blocked-value-3",
						Description: "Blocklist map should contain blocked-value-3",
					},
				},
			},
		},
	}

	// Create template engine
	templates := map[string]string{
		"haproxy.cfg":   config.HAProxyConfig.Template,
		"blocklist.map": config.Maps["blocklist.map"].Template,
	}
	engine, err := templating.New(templates, nil)
	require.NoError(t, err)

	// Convert CRD spec to internal config format
	cfg, err := conversion.ConvertSpec(config)
	require.NoError(t, err)

	// Create test runner
	runner := New(
		cfg,
		engine,
		&dataplane.ValidationPaths{}, // Empty paths for unit tests
		&Options{
			Logger: logger,
		},
	)

	// Run tests
	ctx := context.Background()
	results, err := runner.RunTests(ctx, "")
	require.NoError(t, err)

	// Verify results
	assert.Equal(t, 1, results.TotalTests, "should have 1 test")
	assert.Equal(t, 1, results.PassedTests, "test should pass")
	assert.Equal(t, 0, results.FailedTests, "no tests should fail")

	// Verify rendered map contains all expected values
	require.Len(t, results.TestResults, 1)
	testResult := results.TestResults[0]
	assert.True(t, testResult.Passed, "test should pass")
	assert.Len(t, declaredAssertions(t, &testResult), 3, "should have 3 assertions")
	for _, assertion := range testResult.Assertions {
		assert.True(t, assertion.Passed, "assertion should pass: %s", assertion.Description)
	}
}

func TestRunner_RunTests_HTTPFixtureMissing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Test that missing HTTP fixture causes test failure
	config := &v1alpha1.HAProxyTemplateConfigSpec{
		TemplatingSettings: v1alpha1.TemplatingSettings{
			Engine: "scriggo",
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: `{%- var content, err = http.Fetch("http://missing.example.com/data.txt") -%}
{%- if err != nil %}{{ fail(err.(error).Error()) }}{% end -%}
global
  maxconn 1000
`,
		},
		WatchedResources: servicesWatch(),
		ValidationTests: map[string]v1alpha1.ValidationTest{
			"missing-http-fixture": {
				Description: "Test with missing HTTP fixture",
				Fixtures: map[string][]runtime.RawExtension{
					"services": {},
				},
				// No HTTPResources defined - should fail when http.Fetch is called
				Assertions: []v1alpha1.ValidationAssertion{
					{
						Type:        "contains",
						Target:      "haproxy.cfg",
						Pattern:     "maxconn",
						Description: "Config should contain maxconn",
					},
				},
			},
		},
	}

	// Create template engine
	templates := map[string]string{
		"haproxy.cfg": config.HAProxyConfig.Template,
	}
	engine, err := templating.New(templates, nil)
	require.NoError(t, err)

	// Convert CRD spec to internal config format
	cfg, err := conversion.ConvertSpec(config)
	require.NoError(t, err)

	// Create test runner
	runner := New(
		cfg,
		engine,
		&dataplane.ValidationPaths{}, // Empty paths for unit tests
		&Options{
			Logger: logger,
		},
	)

	// Run tests
	ctx := context.Background()
	results, err := runner.RunTests(ctx, "")
	require.NoError(t, err)

	// Verify test failed due to missing HTTP fixture
	assert.Equal(t, 1, results.TotalTests, "should have 1 test")
	assert.Equal(t, 0, results.PassedTests, "test should fail")
	assert.Equal(t, 1, results.FailedTests, "1 test should fail")

	require.Len(t, results.TestResults, 1)
	testResult := results.TestResults[0]
	assert.False(t, testResult.Passed, "test should fail")
	assert.Contains(t, testResult.RenderError, "no fixture defined for URL")
}

// is parsed and made available to templates for slot-aware server assignment.
func TestRunner_RunTests_WithCurrentConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Test template that accesses currentConfig to read previous backend state
	config := &v1alpha1.HAProxyTemplateConfigSpec{
		TemplatingSettings: v1alpha1.TemplatingSettings{
			Engine: "scriggo",
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			// Template checks len(currentConfig.ServerIndex) > 0 to detect if config is available
			// Note: Scriggo converts (*T)(nil) declaration to a zero-value struct, so isNil() doesn't work
			Template: `global
  maxconn 1000

# Previous backends from currentConfig:
{%- if len(currentConfig.ServerIndex) > 0 %}
{%- if currentConfig.ServerIndex["my-backend-1"] != nil %}
# Previous backend: my-backend-1
{%- end %}
{%- if currentConfig.ServerIndex["my-backend-2"] != nil %}
# Previous backend: my-backend-2
{%- end %}
{%- else %}
# No currentConfig available (first deployment)
{%- end %}
`,
		},
		WatchedResources: servicesWatch(),
		ValidationTests: map[string]v1alpha1.ValidationTest{
			"test-currentconfig": {
				Description: "Test that currentConfig is available in templates",
				Fixtures: map[string][]runtime.RawExtension{
					"services": {},
				},
				// Declare the servers of a previous deployment for the template to read
				CurrentServers: map[string]map[string]v1alpha1.ServerAddr{
					"my-backend-1": {"srv1": {Address: "10.0.0.1", Port: ptr.To(int64(8080))}},
					"my-backend-2": {
						"srv2": {Address: "10.0.0.2", Port: ptr.To(int64(8080))},
						"srv3": {Address: "10.0.0.3", Port: ptr.To(int64(8080))},
					},
				},
				Assertions: []v1alpha1.ValidationAssertion{
					{
						Type:        "contains",
						Target:      "haproxy.cfg",
						Pattern:     "Previous backend: my-backend-1",
						Description: "Template should access first backend from currentConfig",
					},
					{
						Type:        "contains",
						Target:      "haproxy.cfg",
						Pattern:     "Previous backend: my-backend-2",
						Description: "Template should access second backend from currentConfig",
					},
				},
			},
		},
	}

	// Create template engine with currentConfig type declaration
	templates := map[string]string{
		"haproxy.cfg": config.HAProxyConfig.Template,
	}

	// Need to provide the currentConfig type declaration for Scriggo
	additionalDeclarations := map[string]any{
		"currentConfig": (*renderplan.CurrentConfig)(nil),
	}
	engine, err := templating.New(templates, &templating.Options{EntryPoints: []string{"haproxy.cfg"}, Declarations: additionalDeclarations})
	require.NoError(t, err)

	// Convert CRD spec to internal config format
	cfg, err := conversion.ConvertSpec(config)
	require.NoError(t, err)

	// Create test runner
	runner := New(
		cfg,
		engine,
		&dataplane.ValidationPaths{}, // Empty paths for unit tests
		&Options{
			Logger: logger,
		},
	)

	// Run tests
	ctx := context.Background()
	results, err := runner.RunTests(ctx, "")
	require.NoError(t, err)

	// Verify test passed
	assert.Equal(t, 1, results.TotalTests, "should have 1 test")
	assert.Equal(t, 1, results.PassedTests, "test should pass")
	assert.Equal(t, 0, results.FailedTests, "no tests should fail")

	require.Len(t, results.TestResults, 1)
	testResult := results.TestResults[0]
	assert.True(t, testResult.Passed, "test should pass: %s", testResult.RenderError)

	// Verify the rendered config contains the expected content
	assert.Contains(t, testResult.RenderedConfig, "Previous backend: my-backend-1")
	assert.Contains(t, testResult.RenderedConfig, "Previous backend: my-backend-2")
}

// currentServers is how a test declares the previous deployment: the server
// index reaches templates with no config parse in between.
func TestRunner_RunTests_WithCurrentServers(t *testing.T) {
	port := int64(8080)
	tests := []struct {
		name           string
		currentServers map[string]map[string]v1alpha1.ServerAddr
		wantError      string
	}{
		{
			name: "the fixture reaches templates as currentConfig.ServerIndex",
			currentServers: map[string]map[string]v1alpha1.ServerAddr{
				"my-backend-1": {"srv1": {Address: "10.0.0.1", Port: &port}},
				"my-backend-2": {"srv2": {Address: "10.0.0.2", Port: &port}},
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &v1alpha1.HAProxyTemplateConfigSpec{
				TemplatingSettings: v1alpha1.TemplatingSettings{Engine: "scriggo"},
				HAProxyConfig: v1alpha1.HAProxyConfig{Template: `global
  maxconn 1000
{%- if currentConfig.ServerIndex["my-backend-1"] != nil %}
# srv1 at {{ currentConfig.ServerIndex["my-backend-1"]["srv1"].Address }}
{%- end %}
`},
				WatchedResources: servicesWatch(),
				ValidationTests: map[string]v1alpha1.ValidationTest{
					"test-currentservers": {
						Fixtures:       map[string][]runtime.RawExtension{"services": {}},
						CurrentServers: tt.currentServers,
						Assertions: []v1alpha1.ValidationAssertion{
							{Type: "contains", Target: "haproxy.cfg", Pattern: "srv1 at 10\\.0\\.0\\.1"},
						},
					},
				},
			}

			engine, err := templating.New(
				map[string]string{"haproxy.cfg": spec.HAProxyConfig.Template},
				&templating.Options{
					EntryPoints:  []string{"haproxy.cfg"},
					Declarations: map[string]any{"currentConfig": (*renderplan.CurrentConfig)(nil)},
				})
			require.NoError(t, err)

			cfg, err := conversion.ConvertSpec(spec)
			require.NoError(t, err)

			results, err := New(cfg, engine, &dataplane.ValidationPaths{}, &Options{Logger: logger}).
				RunTests(context.Background(), "")
			require.NoError(t, err)
			require.Len(t, results.TestResults, 1)

			result := results.TestResults[0]
			if tt.wantError != "" {
				assert.False(t, result.Passed)
				assert.Contains(t, result.RenderError, tt.wantError)
				return
			}
			assert.True(t, result.Passed, "test should pass: %s", result.RenderError)
			assert.Contains(t, result.RenderedConfig, "srv1 at 10.0.0.1")
		})
	}
}

// the case when currentConfig is nil (first deployment).
func TestRunner_RunTests_WithoutCurrentConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Test template that handles nil currentConfig gracefully
	config := &v1alpha1.HAProxyTemplateConfigSpec{
		TemplatingSettings: v1alpha1.TemplatingSettings{
			Engine: "scriggo",
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			// Template checks len(currentConfig.ServerIndex) > 0 to detect if config is available
			// Note: Scriggo converts (*T)(nil) declaration to a zero-value struct, so isNil() doesn't work
			Template: `global
  maxconn 1000

{%- if len(currentConfig.ServerIndex) > 0 %}
# Has previous config
{%- else %}
# First deployment - no previous config
{%- end %}
`,
		},
		WatchedResources: servicesWatch(),
		ValidationTests: map[string]v1alpha1.ValidationTest{
			"test-no-currentconfig": {
				Description: "Test that template handles empty currentConfig (first deployment)",
				Fixtures: map[string][]runtime.RawExtension{
					"services": {},
				},
				// No CurrentConfig provided - simulates first deployment
				Assertions: []v1alpha1.ValidationAssertion{
					{
						Type:        "contains",
						Target:      "haproxy.cfg",
						Pattern:     "First deployment - no previous config",
						Description: "Template should detect missing currentConfig",
					},
					{
						Type:        "not_contains",
						Target:      "haproxy.cfg",
						Pattern:     "Has previous config",
						Description: "Template should not show previous config message",
					},
				},
			},
		},
	}

	// Create template engine with currentConfig type declaration
	templates := map[string]string{
		"haproxy.cfg": config.HAProxyConfig.Template,
	}

	additionalDeclarations := map[string]any{
		"currentConfig": (*renderplan.CurrentConfig)(nil),
	}
	engine, err := templating.New(templates, &templating.Options{EntryPoints: []string{"haproxy.cfg"}, Declarations: additionalDeclarations})
	require.NoError(t, err)

	// Convert CRD spec to internal config format
	cfg, err := conversion.ConvertSpec(config)
	require.NoError(t, err)

	// Create test runner
	runner := New(
		cfg,
		engine,
		&dataplane.ValidationPaths{},
		&Options{
			Logger: logger,
		},
	)

	// Run tests
	ctx := context.Background()
	results, err := runner.RunTests(ctx, "")
	require.NoError(t, err)

	// Verify test passed
	assert.Equal(t, 1, results.TotalTests, "should have 1 test")
	assert.Equal(t, 1, results.PassedTests, "test should pass")
	assert.Equal(t, 0, results.FailedTests, "no tests should fail")

	require.Len(t, results.TestResults, 1)
	testResult := results.TestResults[0]
	assert.True(t, testResult.Passed, "test should pass: %s", testResult.RenderError)
}

// declaredAssertions drops the determinism check the runner appends to every
// rendering test, so a count in a test means "the assertions this test
// declared" and does not move when the runner adds an automatic one. A test
// whose render failed has no such entry — there is nothing to render twice.
func declaredAssertions(t *testing.T, result *TestResult) []AssertionResult {
	t.Helper()
	var declared []AssertionResult
	for _, a := range result.Assertions {
		if a.Type == "deterministic" || a.Type == "no_fused_directive" {
			assert.True(t, a.Passed, "automatic %s check must pass: %s", a.Type, a.Error)
			continue
		}
		declared = append(declared, a)
	}
	return declared
}
