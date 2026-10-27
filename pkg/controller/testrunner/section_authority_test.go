// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package testrunner

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
)

func TestRunnerPreservesSectionMarkerAuthorityAcrossRenders(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: "global\n  daemon\n{{ render \"backends-900-d4s-ssh\" }}"},
		Maps: map[string]config.MapFile{
			"backends-900-d4s-ssh": {Template: `
{%- var body = "backend d4s-ssh-backend\n  mode tcp\n" -%}
{%- for _, endpoint := range toSlice(dig(extraContext, "endpoints")) -%}
  {%- if dig(endpoint, "enabled") == true -%}
    {%- body += "  server " + dig_string(endpoint, "", "name") + " " + dig_string(endpoint, "", "address") + "\n" -%}
  {%- end -%}
{%- end -%}
{{- planRegistry.Section("backend", "d4s-ssh-backend", body) -}}
`},
		},
		ValidationTests: map[string]config.ValidationTest{},
	}
	enabled := map[string]any{"name": "enabled", "address": "127.0.0.1:22", "enabled": true}
	disabled := map[string]any{"name": "disabled", "address": "127.0.0.2:22", "enabled": false}
	for name, endpoints := range map[string][]any{
		"enabled-only":         {enabled},
		"enabled-and-disabled": {enabled, disabled},
	} {
		cfg.ValidationTests[name] = config.ValidationTest{
			ExtraContext: map[string]any{"endpoints": endpoints},
			Assertions: []config.ValidationAssertion{
				{Type: "contains", Target: "haproxy.cfg", Pattern: "server enabled 127.0.0.1:22"},
				{Type: "not_contains", Target: "haproxy.cfg", Pattern: "disabled|@haptic:"},
				{Type: "contains", Target: "map:backends-900-d4s-ssh", Pattern: "@haptic:[0-9a-f]{16}:section:backend:d4s-ssh-backend@"},
			},
		}
	}
	var firstMarker string
	for _, result := range runSharedAnalysisGate(t, cfg, nil).TestResults {
		assert.True(t, result.Passed, "%s: %s %+v", result.TestName, result.RenderError, result.Assertions)
		marker := result.RenderedMaps["backends-900-d4s-ssh"]
		if firstMarker == "" {
			firstMarker = marker
		}
		assert.Equal(t, firstMarker, marker)
	}
	for _, result := range runSharedAnalysisGate(t, cfg, nil).TestResults {
		assert.True(t, result.Passed, "%s: %s %+v", result.TestName, result.RenderError, result.Assertions)
		assert.NotEqual(t, firstMarker, result.RenderedMaps["backends-900-d4s-ssh"])
	}
}
