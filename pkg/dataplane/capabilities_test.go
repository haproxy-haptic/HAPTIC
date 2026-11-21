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

package dataplane

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCapabilitiesFromVersion(t *testing.T) {
	tests := []struct {
		name    string
		version *Version
		want    Capabilities
	}{
		{
			name:    "nil version returns all false (fail-secure)",
			version: nil,
			want:    Capabilities{}, // All false
		},
		{
			name:    "HAProxy 3.0 - base v3 capabilities",
			version: &Version{Major: 3, Minor: 0, Full: "3.0.0"},
			want: Capabilities{
				SupportsCrtList:        false,
				SupportsMapStorage:     true, // All v3.x have /storage/maps
				SupportsGeneralStorage: true,
				SupportsHTTP2:          true,
				SupportsQUIC:           true, // All v3.x have QUIC options
				SupportsRuntimeMaps:    true,
				SupportsRuntimeServers: true,
			},
		},
		{
			name:    "HAProxy 3.1 - same as 3.0 (no new API capabilities)",
			version: &Version{Major: 3, Minor: 1, Full: "3.1.0"},
			want: Capabilities{
				SupportsCrtList:        false,
				SupportsMapStorage:     true,
				SupportsGeneralStorage: true,
				SupportsHTTP2:          true,
				SupportsQUIC:           true,
				SupportsRuntimeMaps:    true,
				SupportsRuntimeServers: true,
			},
		},
		{
			name:    "HAProxy 3.2 - adds CRT-list storage",
			version: &Version{Major: 3, Minor: 2, Full: "3.2.0"},
			want: Capabilities{
				SupportsCrtList:        true, // Only v3.2+ has /storage/ssl_crt_lists
				SupportsMapStorage:     true,
				SupportsGeneralStorage: true,
				SupportsHTTP2:          true,
				SupportsQUIC:           true,
				SupportsRuntimeMaps:    true,
				SupportsRuntimeServers: true,
			},
		},
		{
			name:    "HAProxy 3.3 (future) - should have all 3.2 capabilities",
			version: &Version{Major: 3, Minor: 3, Full: "3.3.0"},
			want: Capabilities{
				SupportsCrtList:        true,
				SupportsMapStorage:     true,
				SupportsGeneralStorage: true,
				SupportsHTTP2:          true,
				SupportsQUIC:           true,
				SupportsRuntimeMaps:    true,
				SupportsRuntimeServers: true,
			},
		},
		{
			name:    "HAProxy 4.0 (future major version) - should have all capabilities",
			version: &Version{Major: 4, Minor: 0, Full: "4.0.0"},
			want: Capabilities{
				SupportsCrtList:        true,
				SupportsMapStorage:     true,
				SupportsGeneralStorage: true,
				SupportsHTTP2:          true,
				SupportsQUIC:           true,
				SupportsRuntimeMaps:    true,
				SupportsRuntimeServers: true,
			},
		},
		{
			name:    "HAProxy 2.x (unsupported) - should have no capabilities",
			version: &Version{Major: 2, Minor: 9, Full: "2.9.0"},
			want: Capabilities{
				SupportsCrtList:        false,
				SupportsMapStorage:     false,
				SupportsGeneralStorage: false,
				SupportsHTTP2:          false,
				SupportsQUIC:           false,
				SupportsRuntimeMaps:    false,
				SupportsRuntimeServers: false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CapabilitiesFromVersion(tt.version)

			assert.Equal(t, tt.want.SupportsCrtList, got.SupportsCrtList, "SupportsCrtList mismatch")
			assert.Equal(t, tt.want.SupportsMapStorage, got.SupportsMapStorage, "SupportsMapStorage mismatch")
			assert.Equal(t, tt.want.SupportsGeneralStorage, got.SupportsGeneralStorage, "SupportsGeneralStorage mismatch")
			assert.Equal(t, tt.want.SupportsHTTP2, got.SupportsHTTP2, "SupportsHTTP2 mismatch")
			assert.Equal(t, tt.want.SupportsQUIC, got.SupportsQUIC, "SupportsQUIC mismatch")
			assert.Equal(t, tt.want.SupportsRuntimeMaps, got.SupportsRuntimeMaps, "SupportsRuntimeMaps mismatch")
			assert.Equal(t, tt.want.SupportsRuntimeServers, got.SupportsRuntimeServers, "SupportsRuntimeServers mismatch")
		})
	}
}

func TestCapabilitiesFromVersion_CrtListThreshold(t *testing.T) {
	// Detailed tests for the CRT-list threshold (v3.2+)
	tests := []struct {
		major    int
		minor    int
		expected bool
	}{
		{2, 9, false},
		{3, 0, false},
		{3, 1, false},
		{3, 2, true}, // Threshold
		{3, 3, true},
		{4, 0, true},
	}

	for _, tt := range tests {
		v := &Version{Major: tt.major, Minor: tt.minor}
		caps := CapabilitiesFromVersion(v)
		assert.Equal(t, tt.expected, caps.SupportsCrtList,
			"SupportsCrtList for v%d.%d", tt.major, tt.minor)
	}
}

func TestCapabilitiesFromVersion_MapStorageThreshold(t *testing.T) {
	// Detailed tests for the map storage threshold (v3.0+)
	tests := []struct {
		major    int
		minor    int
		expected bool
	}{
		{2, 9, false},
		{3, 0, true}, // Threshold - all v3.x have /storage/maps
		{3, 1, true},
		{3, 2, true},
		{4, 0, true},
	}

	for _, tt := range tests {
		v := &Version{Major: tt.major, Minor: tt.minor}
		caps := CapabilitiesFromVersion(v)
		assert.Equal(t, tt.expected, caps.SupportsMapStorage,
			"SupportsMapStorage for v%d.%d", tt.major, tt.minor)
	}
}

func TestCapabilitiesFromVersion_GeneralStorageThreshold(t *testing.T) {
	// Detailed tests for the general storage threshold (v3.0+)
	tests := []struct {
		major    int
		minor    int
		expected bool
	}{
		{2, 9, false},
		{3, 0, true}, // Threshold
		{3, 1, true},
		{3, 2, true},
		{4, 0, true},
	}

	for _, tt := range tests {
		v := &Version{Major: tt.major, Minor: tt.minor}
		caps := CapabilitiesFromVersion(v)
		assert.Equal(t, tt.expected, caps.SupportsGeneralStorage,
			"SupportsGeneralStorage for v%d.%d", tt.major, tt.minor)
	}
}

// Blob support is a fact about the agents, not HAProxy: this release's agents
// store blobs, so the seed and the offline answer say so, while a version alone
// says nothing about them.
func TestLocalCapabilities_AgentsOfThisReleaseStoreBlobs(t *testing.T) {
	version := &Version{Major: 3, Minor: 2, Full: "3.2.0"}

	local := LocalCapabilities(version)

	assert.True(t, local.SupportsBlobs)
	assert.False(t, CapabilitiesFromVersion(version).SupportsBlobs)
	local.SupportsBlobs = false
	assert.Equal(t, CapabilitiesFromVersion(version), local, "the HAProxy flags come from the version alone")
}
