package testrunner

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
)

type scopeFixtureObject struct {
	Metadata struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"metadata"`
	Count int `json:"count"`
}

func scopedFixture(namespace, name string, count any) map[string]any {
	if number, ok := count.(int); ok {
		count = int64(number)
	}
	return map[string]any{"apiVersion": "example.io/v1", "kind": "Widget", "metadata": map[string]any{"namespace": namespace, "name": name}, "count": count}
}

func scopeTestConfig(template string) *config.Config {
	return &config.Config{
		WatchedResources: map[string]config.WatchedResource{
			"widgets":  {APIVersion: "example.io/v1", Resources: "widgets", IndexBy: []string{"metadata.namespace", "metadata.name"}, Namespaces: []string{"team-a", "team-b"}},
			"routes":   {APIVersion: "example.io/v1", Resources: "routes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"settings": {APIVersion: "example.io/v1", Resources: "settings", IndexBy: []string{"metadata.name"}},
		},
		HAProxyConfig: config.HAProxyConfig{Template: template},
		ValidationTests: map[string]config.ValidationTest{
			"_global": {Fixtures: map[string][]any{
				"widgets":  {scopedFixture("team-a", "inside", 7), scopedFixture("team-b", "second", 8), scopedFixture("foreign", "outside", 9)},
				"routes":   {scopedFixture("foreign", "route", 0)},
				"settings": {scopedFixture("", "cluster", 1)},
			}},
			"scope": {UseDeploymentWatchScopes: true},
		},
	}
}

func runScopeTests(t *testing.T, cfg *config.Config, types map[string]reflect.Type) *TestResults {
	t.Helper()
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{Types: types, Kinds: map[string]string{}, Errors: map[string]error{}})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	runner := New(cfg, engine, &dataplane.ValidationPaths{}, &Options{Workers: 2, TypedResourceTypes: types})
	results, err := runner.RunTests(t.Context(), "")
	require.NoError(t, err)
	return results
}

func TestRunnerDeploymentWatchScopesRejectLookups(t *testing.T) {
	for _, typed := range []bool{false, true} {
		for _, mode := range []string{"direct", "incremental"} {
			for _, method := range []string{"Fetch", "GetSingle"} {
				t.Run(mode+"/"+method+"/typed="+map[bool]string{false: "false", true: "true"}[typed], func(t *testing.T) {
					call := `{% _ = resources.widgets.` + method + `("foreign", "outside") %}`
					cfg := scopeTestConfig(call)
					if mode == "incremental" {
						cfg.HAProxyConfig.Template = `{{ render "lookup" }}`
						cfg.TemplateSnippets = map[string]config.TemplateSnippet{"lookup": {Template: call, Requires: []string{"widgets", "routes"}, Incremental: &config.IncrementalTemplate{Source: "routes"}}}
					}
					test := cfg.ValidationTests["scope"]
					test.Assertions = []config.ValidationAssertion{{Type: "contains", Target: "rendering_error", Pattern: `widgets foreign/outside cannot be read: watchedResources.widgets.namespaces`}}
					cfg.ValidationTests["scope"] = test
					var types map[string]reflect.Type
					if typed {
						types = map[string]reflect.Type{"widgets": reflect.TypeFor[scopeFixtureObject]()}
					}
					results := runScopeTests(t, cfg, types)
					require.Len(t, results.TestResults, 1)
					assert.True(t, results.AllPassed(), "%+v", results.TestResults)
					assert.Contains(t, results.TestResults[0].RenderError, "team-a, team-b")
				})
			}
		}
	}
}

func TestRunnerDeploymentWatchScopesFilterFixtures(t *testing.T) {
	for _, template := range []string{
		`{{ len(resources.widgets.List()) }}/{{ len(resources.widgets.Fetch("team-b")) }}/{{ dig(resources.widgets.GetSingle("team-a", "inside"), "count") }}/{{ len(resources.settings.List()) }}`,
		`{{ len(resources.widgets.List()) }}/{{ len(resources.widgets.Fetch("team-b")) }}/{{ resources.widgets.GetSingle("team-a", "inside").Count }}/{{ len(resources.settings.List()) }}`,
	} {
		t.Run(template, func(t *testing.T) {
			cfg := scopeTestConfig(template)
			test := cfg.ValidationTests["scope"]
			test.Assertions = []config.ValidationAssertion{{Type: "equals", Target: "haproxy.cfg", Expected: "2/1/7/1\n"}}
			cfg.ValidationTests["scope"] = test
			var types map[string]reflect.Type
			if strings.Contains(template, ".Count") {
				types = map[string]reflect.Type{"widgets": reflect.TypeFor[scopeFixtureObject]()}
			}
			results := runScopeTests(t, cfg, types)
			assert.True(t, results.AllPassed(), "%+v", results.TestResults)
		})
	}
}

func TestRunnerDeploymentWatchScopesFilterIncrementalSources(t *testing.T) {
	cfg := scopeTestConfig(`{{ render "row" }}`)
	cfg.TemplateSnippets = map[string]config.TemplateSnippet{"row": {Template: `{{ dig(item, "metadata", "name") }} `, Incremental: &config.IncrementalTemplate{Source: "widgets"}}}
	test := cfg.ValidationTests["scope"]
	test.Assertions = []config.ValidationAssertion{{Type: "contains", Pattern: "inside"}, {Type: "contains", Pattern: "second"}, {Type: "not_contains", Pattern: "outside"}}
	cfg.ValidationTests["scope"] = test
	results := runScopeTests(t, cfg, nil)
	assert.True(t, results.AllPassed(), "%+v", results.TestResults)
}

func TestRunnerDeploymentWatchScopesRejectResourceProjection(t *testing.T) {
	cfg := scopeTestConfig(`{{ render "selected" }}{{ len(incremental_values("selected", "one")) }}`)
	cfg.TemplateSnippets = map[string]config.TemplateSnippet{"selected": {
		Template:    `{{ fail("projection must not execute its template") }}`,
		Incremental: &config.IncrementalTemplate{Mode: config.IncrementalModeResourceProjection, Group: "selected", Effects: []config.IncrementalEffect{config.IncrementalEffectPublishValue}, BindingsTemplate: `{{ toJSON(map[string]any{"widgets": map[string]any{"cell":"one","key":"one","keys":[]any{"foreign","outside"}}}) }}`},
	}}
	test := cfg.ValidationTests["scope"]
	test.Assertions = []config.ValidationAssertion{{Type: "contains", Target: "rendering_error", Pattern: "widgets foreign/outside cannot be read"}}
	cfg.ValidationTests["scope"] = test
	results := runScopeTests(t, cfg, nil)
	assert.True(t, results.AllPassed(), "%+v", results.TestResults)
}

func TestRunnerDeploymentWatchScopesApplyToDeploymentContext(t *testing.T) {
	cfg := scopeTestConfig(`{{ len(resources.widgets.Fetch(extraContext["namespace"])) }}`)
	cfg.TemplatingSettings.TestExtraContext = map[string]any{"namespace": "team-a"}
	cfg.TemplatingSettings.ExtraContext = map[string]any{"namespace": "foreign"}
	test := cfg.ValidationTests["scope"]
	test.Assertions = []config.ValidationAssertion{{Type: "equals", Expected: "1\n"}}
	cfg.ValidationTests["scope"] = test
	results := runScopeTests(t, cfg, nil)
	require.Len(t, results.TestResults, 1)
	assert.False(t, results.AllPassed())
	assert.Equal(t, "1", strings.TrimSpace(results.TestResults[0].RenderedConfig))
	var deployment *AssertionResult
	for i := range results.TestResults[0].Assertions {
		assertion := &results.TestResults[0].Assertions[i]
		if assertion.Type == deploymentAssertionType {
			deployment = assertion
		}
	}
	require.NotNil(t, deployment)
	assert.False(t, deployment.Passed)
	assert.Contains(t, deployment.Error, "widgets in namespace foreign cannot be read")
}

func TestRunnerDeploymentWatchScopesCannotHideMalformedFixtures(t *testing.T) {
	cfg := scopeTestConfig(`{% _ = resources.widgets.GetSingle("foreign", "outside") %}{{ len(resources.routes.List()) }}`)
	test := cfg.ValidationTests["scope"]
	test.Fixtures = map[string][]any{"routes": {scopedFixture("foreign", "route", "not-a-number")}}
	test.Assertions = []config.ValidationAssertion{{Type: "contains", Target: "rendering_error", Pattern: "cannot be read"}}
	cfg.ValidationTests["scope"] = test
	results := runScopeTests(t, cfg, map[string]reflect.Type{"routes": reflect.TypeFor[scopeFixtureObject]()})
	require.Len(t, results.TestResults, 1)
	assert.False(t, results.AllPassed())
	assert.Contains(t, results.TestResults[0].RenderError, "cannot be read")
	assert.Contains(t, results.TestResults[0].RenderError, "could not materialize its typed object")
	require.Len(t, results.TestResults[0].Assertions, 1)
	assert.Equal(t, "rendering", results.TestResults[0].Assertions[0].Type)
}

func TestRunnerDeploymentWatchScopesRemainPerTest(t *testing.T) {
	cfg := scopeTestConfig(`{{ len(resources.widgets.List()) }}`)
	cfg.ValidationTests["scope"] = config.ValidationTest{UseDeploymentWatchScopes: true, Assertions: []config.ValidationAssertion{{Type: "equals", Expected: "2\n"}}}
	cfg.ValidationTests["isolated"] = config.ValidationTest{Assertions: []config.ValidationAssertion{{Type: "equals", Expected: "3\n"}}}
	results := runScopeTests(t, cfg, nil)
	assert.True(t, results.AllPassed(), "%+v", results.TestResults)
	assert.Len(t, cfg.ValidationTests["_global"].Fixtures["widgets"], 3)
}

func TestRunnerDeploymentWatchScopesAllowUnrestrictedWatches(t *testing.T) {
	cfg := scopeTestConfig(`{{ len(resources.widgets.List()) }}`)
	watch := cfg.WatchedResources["widgets"]
	watch.Namespaces = nil
	cfg.WatchedResources["widgets"] = watch
	cfg.ValidationTests["scope"] = config.ValidationTest{UseDeploymentWatchScopes: true, Assertions: []config.ValidationAssertion{{Type: "equals", Expected: "3\n"}}}
	results := runScopeTests(t, cfg, nil)
	assert.True(t, results.AllPassed(), "%+v", results.TestResults)
}

func TestRunnerDeploymentWatchScopesRejectUnexpectedLookup(t *testing.T) {
	cfg := scopeTestConfig(`{% _ = resources.widgets.GetSingle("foreign", "outside") %}otherwise valid`)
	cfg.ValidationTests["scope"] = config.ValidationTest{UseDeploymentWatchScopes: true, Assertions: []config.ValidationAssertion{{Type: "contains", Pattern: "otherwise valid"}}}
	results := runScopeTests(t, cfg, nil)
	require.Len(t, results.TestResults, 1)
	assert.False(t, results.AllPassed())
	require.NotEmpty(t, results.TestResults[0].Assertions)
	assert.True(t, results.TestResults[0].Assertions[0].ConfigRejected)
}
