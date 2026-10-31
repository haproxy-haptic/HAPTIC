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

import "path/filepath"

// PathConfig contains the base directory configuration for HAProxy auxiliary files.
// These are the raw filesystem paths before capability-based resolution.
type PathConfig struct {
	// MapsDir is the base path for HAProxy map files (e.g., /etc/haproxy/maps).
	MapsDir string

	// SSLDir is the base path for HAProxy SSL certificates (e.g., /etc/haproxy/ssl).
	SSLDir string

	// GeneralDir is the base path for HAProxy general files (e.g., /etc/haproxy/general).
	GeneralDir string

	// SecretDir is the base path for secret files (e.g., /etc/haproxy/secrets).
	SecretDir string

	// ConfigFile is the path to the HAProxy configuration file (e.g., /tmp/haproxy.cfg).
	// Only used in validation contexts; can be empty for production paths.
	ConfigFile string
}

// ResolvedPaths contains capability-aware resolved paths for HAProxy auxiliary files.
// This is the result of applying capability-based resolution to a PathConfig.
//
// The key difference from PathConfig is that CRTListDir is always set to GeneralDir
// because CRT-list files are stored as general files to avoid reload on create.
type ResolvedPaths struct {
	// MapsDir is the resolved path for HAProxy map files.
	MapsDir string

	// SSLDir is the resolved path for HAProxy SSL certificates.
	SSLDir string

	// CRTListDir is the resolved path for CRT-list files.
	// Always equals GeneralDir because CRT-list files are stored as general files
	// to avoid reload on create (native CRT-list API doesn't support skip_reload).
	CRTListDir string

	// GeneralDir is the resolved path for HAProxy general files.
	GeneralDir string

	// SecretDir is the resolved path for secret files.
	SecretDir string

	// ConfigFile is the path to the HAProxy configuration file.
	ConfigFile string
}

// ResolvePaths applies capability-based path resolution to base paths.
// This is the SINGLE SOURCE OF TRUTH for all capability-dependent path logic.
//
// Currently handles:
//   - CRT-list storage: Always uses general directory because the native CRT-list
//     API triggers reload on create without supporting skip_reload parameter.
//     General file storage returns 201 without triggering reload.
func ResolvePaths(base *PathConfig) *ResolvedPaths {
	return &ResolvedPaths{
		MapsDir:    base.MapsDir,
		SSLDir:     base.SSLDir,
		GeneralDir: base.GeneralDir,
		SecretDir:  base.SecretDir,
		ConfigFile: base.ConfigFile,
		// Always use GeneralDir for CRT-lists to avoid reload on create.
		// Native CRT-list API triggers reload without skip_reload support.
		CRTListDir: base.GeneralDir,
	}
}

// ToValidationPaths converts ResolvedPaths to ValidationPaths.
// Use this when you need ValidationPaths for HAProxy configuration validation.
// TempDir is derived from ConfigFile's parent directory (e.g., /tmp/validate-xxx/haproxy.cfg -> /tmp/validate-xxx).
func (r *ResolvedPaths) ToValidationPaths() *ValidationPaths {
	return &ValidationPaths{
		TempDir:           filepath.Dir(r.ConfigFile),
		MapsDir:           r.MapsDir,
		SSLCertsDir:       r.SSLDir,
		CRTListDir:        r.CRTListDir,
		GeneralStorageDir: r.GeneralDir,
		SecretDir:         r.SecretDir,
		ConfigFile:        r.ConfigFile,
	}
}
