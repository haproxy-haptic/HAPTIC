package testrunner

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func TestRunnerNamedSelectionPreservesEveryResult(t *testing.T) {
	cfg := sharedAnalysisConfig("{{ render \"route-lines\" }}\n", 4)
	failing := cfg.ValidationTests["route-1"]
	failing.Assertions[0].Pattern = "missing output"
	cfg.ValidationTests["route-1"] = failing
	skipped := cfg.ValidationTests["route-2"]
	skipped.MinHAProxyVersion = "99.0"
	cfg.ValidationTests["route-2"] = skipped
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, nil, helpers.EngineOptions{})
	require.NoError(t, err)
	version, err := dataplane.ParseVersionString("3.4")
	require.NoError(t, err)
	paths := &dataplane.ValidationPaths{ConfigFile: filepath.Join(t.TempDir(), "haproxy.cfg")}
	runner := New(cfg, engine, paths, &Options{Workers: 4, HAProxyVersion: version})
	results, err := runner.RunNamedTests(t.Context(), []string{"route-0", "route-1", "route-2", "route-0"})
	require.NoError(t, err)
	assert.Equal(t, 2, results.TotalTests)
	assert.Equal(t, 1, results.PassedTests)
	assert.Equal(t, 1, results.FailedTests)
	assert.Equal(t, 1, results.SkippedTests)
	names := make([]string, 0, len(results.TestResults))
	for _, result := range results.TestResults {
		names = append(names, result.TestName)
	}
	assert.ElementsMatch(t, []string{"route-0", "route-1", "route-2"}, names)
}

func TestRunnerNamedSelectionRejectsMissingNamesBeforeRendering(t *testing.T) {
	var renders atomic.Int64
	cfg := sharedAnalysisConfig("{{ observed() }}\n{{ render \"route-lines\" }}\n", 1)
	functions := map[string]templating.GlobalFunc{
		"observed": func(...any) (any, error) { renders.Add(1); return "", nil },
	}
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, functions, nil, nil, helpers.EngineOptions{})
	require.NoError(t, err)
	runner := New(cfg, engine, &dataplane.ValidationPaths{}, nil)
	for _, missing := range []string{"missing", "", "route-0,missing"} {
		t.Run(missing, func(t *testing.T) {
			results, err := runner.RunNamedTests(t.Context(), []string{"route-0", missing})
			require.ErrorContains(t, err, "not found")
			assert.Nil(t, results)
			assert.Zero(t, renders.Load())
		})
	}
}

func TestRunnerRemovesEachTestDirectoryWhenTheTestFinishes(t *testing.T) {
	cfg := sharedAnalysisConfig("{{ render \"route-lines\" }}\n", 4)
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, nil, helpers.EngineOptions{})
	require.NoError(t, err)
	version, err := dataplane.ParseVersionString("3.4")
	require.NoError(t, err)
	scratch := t.TempDir()
	runner := New(cfg, engine, &dataplane.ValidationPaths{ConfigFile: filepath.Join(scratch, "haproxy.cfg")}, &Options{Workers: 2, HAProxyVersion: version})

	results, err := runner.RunTests(t.Context(), "")
	require.NoError(t, err)
	require.Equal(t, 4, results.PassedTests)

	leftovers, err := filepath.Glob(filepath.Join(scratch, "worker-*", "test-*"))
	require.NoError(t, err)
	assert.Empty(t, leftovers)
	_, err = os.Stat(scratch)
	require.NoError(t, err, "the base directory stays for the caller to remove")
}
