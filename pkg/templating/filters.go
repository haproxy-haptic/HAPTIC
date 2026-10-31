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

package templating

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// sanitizeStorageNameRegex matches all characters that are NOT alphanumeric, underscore, or hyphen.
// This regex mirrors the HAProxy client-native library's misc.SanitizeFilename behavior.
// See: github.com/haproxytech/client-native/v6/misc/stringutil.go (lines 220-245).
var sanitizeStorageNameRegex = regexp.MustCompile(`[^a-zA-Z0-9_\-]+`)

// sanitizeStorageName reduces a filename to HAPTIC's own certificate and
// crt-list naming convention.
// The client-native library replaces ALL non-alphanumeric characters (except underscore and hyphen)
// with underscores in the basename, preserving the extension.
//
// Examples:
//   - "api.example.com.pem" becomes "api_example_com.pem"
//   - "my-service.pem" becomes "my-service.pem" (hyphen preserved)
//   - "namespace_name.pem" becomes "namespace_name.pem" (underscore preserved)
//   - "file with spaces.pem" becomes "file_with_spaces.pem"
//
// This replicates the logic from github.com/haproxytech/client-native/v6/misc/stringutil.go
// to avoid introducing a dependency on the dataplane package (pkg/templating is a pure library).
func sanitizeStorageName(name string) string {
	ext := filepath.Ext(name)

	// Get the base name without extension
	base := name
	if ext != "" {
		base = strings.TrimSuffix(name, ext)
	}

	// Replace all non-alphanumeric characters (except _ and -) with underscores
	sanitizedBase := sanitizeStorageNameRegex.ReplaceAllString(base, "_")

	return sanitizedBase + ext
}

// PathResolver resolves auxiliary file names to paths based on file type.
// This is used via the GetPath method in templates to construct paths
// for HAProxy auxiliary files (maps, SSL certificates, crt-list files, general files).
//
// The paths are relative (maps/, ssl/, files/) and rely on HAProxy's
// "default-path origin <BaseDir>" directive to resolve to absolute locations.
// This enables the same rendered config to work for both local validation
// and deployment onto the pod.
type PathResolver struct {
	// BaseDir is the absolute base path for HAProxy auxiliary files.
	// This is used with "default-path origin" in HAProxy's global section
	// to resolve relative paths regardless of where the config file is located.
	// Example: "/etc/haproxy"
	BaseDir string

	// MapsDir is the relative path to the HAProxy maps directory.
	// Example: "maps"
	MapsDir string

	// SSLDir is the relative path to the HAProxy SSL certificates directory.
	// Example: "ssl"
	SSLDir string

	// CRTListDir is the relative path to the HAProxy crt-list files directory.
	// Example: "ssl"
	CRTListDir string

	// GeneralDir is the relative path to the HAProxy general files directory.
	// Example: "files"
	GeneralDir string

	// SecretDir is the relative path to the directory of files HAProxy never
	// reads, for sidecars. Example: "secrets"
	SecretDir string
}

// GetBaseDir returns the BaseDir field for use in templates.
// This method exists because Scriggo runtime variables (declared with nil pointers)
// support method calls but may not support direct field access.
func (pr *PathResolver) GetBaseDir() string {
	return pr.BaseDir
}

// GetPath resolves a filename to a full path based on the file type.
//
// This method is called from templates via the pathResolver context variable:
//
//	{{ pathResolver.GetPath("host.map", "map") }}              → maps/host.map (relative) or /etc/haproxy/maps/host.map (absolute)
//	{{ pathResolver.GetPath("504.http", "file") }}             → files/504.http (relative) or /etc/haproxy/general/504.http (absolute)
//	{{ pathResolver.GetPath("cert.pem", "cert") }}             → ssl/cert.pem (relative) or /etc/haproxy/ssl/cert.pem (absolute)
//	{{ pathResolver.GetPath("certificate-list.txt", "crt-list") }} → ssl/certificate-list.txt (relative)
//	{{ pathResolver.GetPath("sso.client-secret", "secret") }} → secrets/sso.client-secret (relative)
//	{{ pathResolver.GetPath("", "cert") }}                     → ssl (directory only)
//
// Parameters:
//   - args[0]: filename (string) - The base filename (without directory path), or empty string for directory only
//   - args[1]: fileType (string) - File type: "map", "file", "cert", "crt-list", or "secret"
//
// Returns:
//   - Path to the file (relative or absolute depending on PathResolver configuration)
//   - Error if argument count is wrong, arguments are not strings, file type is invalid, or path construction fails
//
// Note: The pathResolver must be added to the rendering context for templates to access this method.
// Relative paths work with HAProxy's working directory resolution during validation.
func (pr *PathResolver) GetPath(args ...any) (any, error) {
	// Validate argument count
	if len(args) != 2 {
		return nil, fmt.Errorf("GetPath requires 2 arguments (filename, fileType), got %d", len(args))
	}

	// Validate filename is a string
	filenameStr, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("GetPath: filename must be a string, got %T", args[0])
	}

	// Validate and extract file type
	fileTypeStr, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("GetPath: file type must be a string, got %T", args[1])
	}

	// Resolve path based on file type
	var basePath string
	switch fileTypeStr {
	case "map":
		basePath = pr.MapsDir
	case "file":
		basePath = pr.GeneralDir
	case "cert":
		basePath = pr.SSLDir
	case "crt-list":
		basePath = pr.CRTListDir
	case "secret":
		if pr.SecretDir == "" {
			return nil, errors.New("GetPath: no secret file directory is configured; set dataplane.secretsDir")
		}
		basePath = pr.SecretDir
	default:
		return nil, fmt.Errorf("GetPath: invalid file type %q, must be \"map\", \"file\", \"cert\", \"crt-list\", or \"secret\"", fileTypeStr)
	}

	// If filename is empty, return just the base directory
	if filenameStr == "" {
		return basePath, nil
	}

	// Reject a name that would steer the write outside basePath before it is
	// joined. Applied to every type: cert/crt-list sanitisation already strips
	// traversal, but map/file are used verbatim and this is their only guard.
	if err := rejectUncontainedPath("GetPath: filename", filenameStr); err != nil {
		return nil, err
	}

	// Sanitize filename for SSL certificates and crt-list files only.
	// The HAProxy client-native library sanitizes filenames when storing SSL certificates
	// to avoid issues with domain names containing dots (e.g., "api.example.com.pem").
	// Map files and general files do NOT need sanitization.
	// See: github.com/haproxytech/client-native/v6/storage/storage.go (lines 198, 270)
	if fileTypeStr == "cert" || fileTypeStr == "crt-list" {
		filenameStr = sanitizeStorageName(filenameStr)
	}

	// Construct full path by joining base directory with filename.
	// path.Join (not filepath.Join): these are HAProxy target paths that must
	// always use forward slashes, regardless of the OS the controller runs on.
	fullPath := path.Join(basePath, filenameStr)

	return fullPath, nil
}

// rejectUncontainedPath returns an error when name would escape a base
// directory it is about to be joined under: an absolute path, a Windows
// drive/UNC path, or one that climbs out via "..". label names the offending
// value so the operator can fix it. Empty names are the caller's concern.
//
// This is the auxiliary-file counterpart of the untar_gz archive-entry guard;
// both stop a resource-derived name from steering a write outside its directory.
func rejectUncontainedPath(label, name string) error {
	if path.IsAbs(name) || strings.HasPrefix(name, `\`) || strings.Contains(name, `:\`) {
		return fmt.Errorf("%s %q is an absolute path", label, name)
	}
	if cleaned := path.Clean(name); cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("%s %q escapes the base directory", label, name)
	}
	return nil
}

func strip(s string) string {
	return strings.TrimSpace(s)
}

// debug formats a value as JSON-formatted HAProxy comments.
//
// This is the shared core implementation used by the Scriggo engine.
// Useful for debugging template data during development.
//
// Usage in templates:
//
//	{{ routes | debug }}           → "# DEBUG:\n# [...]"
//	{{ routes | debug("label") }}  → "# DEBUG label:\n# [...]"
//
// Parameters:
//   - value: Any value to debug (will be JSON serialized)
//   - label: Optional label to identify the debug output
//
// Returns:
//   - Formatted string with JSON data as HAProxy comments
func debug(value any, label string) string {
	// Marshal to JSON with indentation
	data, err := json.MarshalIndent(value, "# ", "  ")
	if err != nil {
		// Fallback to simple string representation
		data = fmt.Appendf(nil, "%v", value)
	}

	// Format as HAProxy comments
	if label != "" {
		return fmt.Sprintf("# DEBUG %s:\n# %s\n", label, string(data))
	}
	return fmt.Sprintf("# DEBUG:\n# %s\n", string(data))
}
