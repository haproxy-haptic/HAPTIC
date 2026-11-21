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

// Capabilities is what the fleet's HAProxy supports. The controller derives it
// from the lowest version its pods report, so a render never uses a feature the
// oldest pod would reject. Templates read the fields as snake_case keys
// (`capabilities.supports_crt_list`), which makes every field name a contract.
type Capabilities struct {
	// Storage capabilities
	SupportsCrtList        bool // crt-list files (3.2+)
	SupportsMapStorage     bool // map files (3.0+)
	SupportsGeneralStorage bool // general files (3.0+)
	SupportsSslCaFiles     bool // CA files (3.2+)
	SupportsSslCrlFiles    bool // CRL files (3.2+)

	// Configuration capabilities
	SupportsHTTP2            bool // HTTP/2 (3.0+)
	SupportsQUIC             bool // QUIC/HTTP3 (3.0+)
	SupportsQUICInitialRules bool // quic-initial rules (3.1+)

	// Observability capabilities
	SupportsLogProfiles bool // log-profile sections (3.1+)
	SupportsTraces      bool // traces section (3.1+)

	// Certificate automation capabilities
	SupportsAcmeProviders bool // acme sections (3.2+)

	// Runtime capabilities
	SupportsRuntimeMaps    bool // runtime map operations (3.0+)
	SupportsRuntimeServers bool // runtime server operations (3.0+)

	// SupportsBlobs reports that every pod's agent stores blobs (ADR-0041),
	// whatever its HAProxy version. blob.Fetch returns no handle without it.
	SupportsBlobs bool
}

// LocalCapabilities is what a fleet of this release's agents running HAProxy v
// supports: the seed from the controller's own binary until the pods have
// reported, and the answer for renders that have no fleet.
func LocalCapabilities(v *Version) Capabilities {
	capabilities := CapabilitiesFromVersion(v)
	capabilities.SupportsBlobs = v != nil
	return capabilities
}

// CapabilitiesFromVersion computes what HAProxy v supports. The deployer
// derives the fleet's capabilities from its lowest reported version.
func CapabilitiesFromVersion(v *Version) Capabilities {
	if v == nil {
		return Capabilities{} // All false - safest default
	}

	isV31OrLater := v.Major > 3 || (v.Major == 3 && v.Minor >= 1)
	isV32OrLater := v.Major > 3 || (v.Major == 3 && v.Minor >= 2)

	return Capabilities{
		// Storage capabilities
		SupportsCrtList:        isV32OrLater,
		SupportsMapStorage:     v.Major >= 3,
		SupportsGeneralStorage: v.Major >= 3,
		SupportsSslCaFiles:     isV32OrLater,
		SupportsSslCrlFiles:    isV32OrLater,

		// Configuration capabilities
		SupportsHTTP2:            v.Major >= 3,
		SupportsQUIC:             v.Major >= 3,
		SupportsQUICInitialRules: isV31OrLater,

		// Observability capabilities
		SupportsLogProfiles: isV31OrLater,
		SupportsTraces:      isV31OrLater,

		// Certificate automation capabilities
		SupportsAcmeProviders: isV32OrLater,

		// Runtime capabilities
		SupportsRuntimeMaps:    v.Major >= 3,
		SupportsRuntimeServers: v.Major >= 3,
	}
}
