// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package rendercontext

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
)

// CapabilitiesToMap converts the typed Capabilities struct into the
// snake_case-keyed map that templates use to gate version-specific
// features. The tests below pin every contract:
//   - nil input yields an empty (non-nil) map (templates iterate
//     `capabilities[...]` so nil would NPE)
//   - every documented snake_case key is present
//   - each Capability bool flows through verbatim (no inversion, no
//     additional logic)

func TestCapabilitiesToMap_NilInput(t *testing.T) {
	got := CapabilitiesToMap(nil)
	assert.NotNil(t, got, "nil input must produce empty map (templates iterate 'capabilities')")
	assert.Empty(t, got)
}

func TestCapabilitiesToMap_AllZeroHasEveryKey(t *testing.T) {
	got := CapabilitiesToMap(&dataplane.Capabilities{})

	// Every documented key must be present; missing keys would surface
	// as nil in templates and fail truthiness checks.
	expectedKeys := []string{
		"supports_crt_list", "supports_map_storage", "supports_general_storage",
		"supports_ssl_ca_files", "supports_ssl_crl_files",
		"supports_http2", "supports_quic", "supports_quic_initial_rules",
		"supports_log_profiles", "supports_traces", "supports_acme_providers",
		"supports_runtime_maps", "supports_runtime_servers", "supports_blobs",
	}
	for _, key := range expectedKeys {
		val, ok := got[key]
		if assert.True(t, ok, "key %q must be present in CapabilitiesToMap output", key) {
			assert.Equal(t, false, val, "all-zero Capabilities must produce false for %q", key)
		}
	}
	assert.Len(t, got, len(expectedKeys), "the map exposes exactly the documented keys")
}

func TestCapabilitiesToMap_PerFieldFlowsThroughVerbatim(t *testing.T) {
	// Set ONE field at a time and confirm only the matching key flips.
	// This catches any accidental cross-wiring (e.g. supports_traces
	// pointing at SupportsLogProfiles).
	tests := []struct {
		name    string
		set     func(*dataplane.Capabilities)
		wantKey string
	}{
		{name: "SupportsCrtList -> supports_crt_list", set: func(c *dataplane.Capabilities) { c.SupportsCrtList = true }, wantKey: "supports_crt_list"},
		{name: "SupportsMapStorage -> supports_map_storage", set: func(c *dataplane.Capabilities) { c.SupportsMapStorage = true }, wantKey: "supports_map_storage"},
		{name: "SupportsGeneralStorage -> supports_general_storage", set: func(c *dataplane.Capabilities) { c.SupportsGeneralStorage = true }, wantKey: "supports_general_storage"},
		{name: "SupportsSslCaFiles -> supports_ssl_ca_files", set: func(c *dataplane.Capabilities) { c.SupportsSslCaFiles = true }, wantKey: "supports_ssl_ca_files"},
		{name: "SupportsSslCrlFiles -> supports_ssl_crl_files", set: func(c *dataplane.Capabilities) { c.SupportsSslCrlFiles = true }, wantKey: "supports_ssl_crl_files"},
		{name: "SupportsHTTP2 -> supports_http2", set: func(c *dataplane.Capabilities) { c.SupportsHTTP2 = true }, wantKey: "supports_http2"},
		{name: "SupportsQUIC -> supports_quic", set: func(c *dataplane.Capabilities) { c.SupportsQUIC = true }, wantKey: "supports_quic"},
		{name: "SupportsQUICInitialRules -> supports_quic_initial_rules", set: func(c *dataplane.Capabilities) { c.SupportsQUICInitialRules = true }, wantKey: "supports_quic_initial_rules"},
		{name: "SupportsLogProfiles -> supports_log_profiles", set: func(c *dataplane.Capabilities) { c.SupportsLogProfiles = true }, wantKey: "supports_log_profiles"},
		{name: "SupportsTraces -> supports_traces", set: func(c *dataplane.Capabilities) { c.SupportsTraces = true }, wantKey: "supports_traces"},
		{name: "SupportsAcmeProviders -> supports_acme_providers", set: func(c *dataplane.Capabilities) { c.SupportsAcmeProviders = true }, wantKey: "supports_acme_providers"},
		{name: "SupportsRuntimeMaps -> supports_runtime_maps", set: func(c *dataplane.Capabilities) { c.SupportsRuntimeMaps = true }, wantKey: "supports_runtime_maps"},
		{name: "SupportsRuntimeServers -> supports_runtime_servers", set: func(c *dataplane.Capabilities) { c.SupportsRuntimeServers = true }, wantKey: "supports_runtime_servers"},
		{name: "SupportsBlobs -> supports_blobs", set: func(c *dataplane.Capabilities) { c.SupportsBlobs = true }, wantKey: "supports_blobs"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps := &dataplane.Capabilities{}
			tt.set(caps)
			got := CapabilitiesToMap(caps)
			assert.Equal(t, true, got[tt.wantKey], "%s must light up after setting the matching field", tt.wantKey)
		})
	}
}
