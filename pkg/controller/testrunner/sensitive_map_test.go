// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package testrunner

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
)

const sensitiveMapSecret = "hunter2-hmac-key"

func sensitiveMapConfig(assertions ...config.ValidationAssertion) *config.Config {
	return &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: `global
  daemon
{%- var path, _ = fileRegistry.Register("map", "keys.map", "route-a ` + sensitiveMapSecret + `\n") %}
{%- var _ = planRegistry.SensitiveMap("keys.map") %}
{%- var plain, _ = fileRegistry.Register("map", "plain.map", "route-a public\n") %}
# {{ path }} {{ plain }}
`},
		Maps: map[string]config.MapFile{
			"declared.map": {Template: `{{- planRegistry.SensitiveMap("declared.map") -}}route-b ` + sensitiveMapSecret + "\n"},
		},
		ValidationTests: map[string]config.ValidationTest{"sensitive": {Assertions: assertions}},
	}
}

func TestSensitiveMapTargetsResolveOnlySensitiveMaps(t *testing.T) {
	results := runSharedAnalysisGate(t, sensitiveMapConfig(
		config.ValidationAssertion{Type: "contains", Target: "sensitive-map:keys.map", Pattern: "hunter2-hmac-.ey"},
		config.ValidationAssertion{Type: "contains", Target: "sensitive-map:declared.map", Pattern: "route-b"},
		config.ValidationAssertion{Type: "contains", Target: "map:keys.map", Pattern: "route-a hunter2-hmac"},
		config.ValidationAssertion{Type: "contains", Target: "map:plain.map", Pattern: "public"},
	), nil)
	result := results.TestResults[0]
	assert.True(t, result.Passed, "%s %+v", result.RenderError, result.Assertions)

	assert.Contains(t, result.RenderedMaps, "plain.map")
	assert.NotContains(t, result.RenderedMaps, "keys.map", "--dump-rendered and the snapshot CLI read RenderedMaps")
	assert.NotContains(t, result.RenderedMaps, "declared.map")
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), sensitiveMapSecret)
}

func TestSensitiveMapTargetRejectsAPlainOrMissingMap(t *testing.T) {
	results := runSharedAnalysisGate(t, sensitiveMapConfig(
		config.ValidationAssertion{Type: "contains", Target: "sensitive-map:plain.map", Pattern: "public"},
		config.ValidationAssertion{Type: "contains", Target: "sensitive-map:missing.map", Pattern: "x"},
	), nil)
	assertions := results.TestResults[0].Assertions
	require.GreaterOrEqual(t, len(assertions), 2)
	assert.False(t, assertions[0].Passed)
	assert.Contains(t, assertions[0].Error, "not sensitive")
	assert.False(t, assertions[1].Passed)
	assert.Contains(t, assertions[1].Error, "was not produced by this render")
}

func TestFailedSensitiveMapAssertionShowsNoContent(t *testing.T) {
	results := runSharedAnalysisGate(t, sensitiveMapConfig(
		config.ValidationAssertion{Type: "equals", Target: "sensitive-map:keys.map", Expected: "nope"},
		config.ValidationAssertion{Type: "contains", Target: "map:keys.map", Pattern: "absent"},
	), nil)
	encoded, err := json.Marshal(results)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), sensitiveMapSecret)
	output, err := FormatResults(results, OutputOptions{Format: OutputFormatSummary, Verbose: true})
	require.NoError(t, err)
	assert.NotContains(t, output, sensitiveMapSecret)
	assert.False(t, results.TestResults[0].Passed)
}

func TestDeterminismCheckNeverPrintsSensitiveContent(t *testing.T) {
	first := &dataplane.AuxiliaryFiles{SensitiveMapFiles: []auxiliaryfiles.SensitiveMapFile{{Path: "k.map", Content: "a " + sensitiveMapSecret, Seal: "1"}}}
	second := &dataplane.AuxiliaryFiles{SensitiveMapFiles: []auxiliaryfiles.SensitiveMapFile{{Path: "k.map", Content: "b " + sensitiveMapSecret, Seal: "2"}}}
	diff := compareAuxiliaryFiles(first, second)
	assert.NotEmpty(t, diff)
	assert.NotContains(t, diff, sensitiveMapSecret)
}
