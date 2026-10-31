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

package rendercontext

import (
	"fmt"
	"path"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// FileRegistry allows templates to dynamically register auxiliary files
// (certs, maps, general files) during rendering. This is used for cases
// where file content comes from dynamic sources (e.g., certificates from secrets)
// rather than pre-declared templates.
//
// Usage in templates:
//
//	{% set ca_content = secret.data["ca.crt"] | b64decode %}
//	{% set ca_path = file_registry.Register("cert", "my-backend-ca.pem", ca_content) %}
//	server backend:443 ssl ca-file {{ ca_path }} verify required
//
// The Registry method is called on the FileRegistry object in the template
// rendering context, not as a standalone filter.
type FileRegistry struct {
	mu           sync.Mutex
	registered   map[string]registeredFile
	pathResolver *templating.PathResolver
	sealKey      []byte
}

// registeredFile tracks a dynamically-registered file.
type registeredFile struct {
	Type         string // "cert", "map", "file", "crt-list", "ca-file", "secret"
	Filename     string // Base filename
	Content      string // File content
	Path         string // Predicted full path
	ReloadOnPush *bool  // "file" only; nil means true
}

// reloadsOnPush resolves the optional flag: nil means true.
func reloadsOnPush(flag *bool) bool {
	return flag == nil || *flag
}

// NewFileRegistry creates a new FileRegistry with the given path resolver.
// The path resolver is used to compute full paths for registered files,
// ensuring they match the paths used by pathResolver.GetPath() method.
// sealKey keys the seal of every "secret" file (auxiliaryfiles.SealContent).
func NewFileRegistry(pathResolver *templating.PathResolver, sealKey []byte) *FileRegistry {
	return &FileRegistry{
		registered:   make(map[string]registeredFile),
		pathResolver: pathResolver,
		sealKey:      sealKey,
	}
}

// Register registers a new auxiliary file to be created and returns its predicted path.
// This method is called from templates as file_registry.Register(type, filename, content)
// or file_registry.Register("file", filename, content, reloadOnPush).
//
// Parameters:
//   - fileType: "cert", "map", "file", "crt-list", "ca-file", or "secret"
//   - filename: Base filename (e.g., "ca.pem", "domains.map", "certificate-list.txt")
//   - content: File content as a string
//   - reloadOnPush: optional, "file" only. Defaults to true. Pass false for a
//     file a sidecar owns and watches itself (the spoa-hub TOML), so a content
//     change deploys without reloading HAProxy — see the CRD's
//     files[].reloadOnPush.
//
// Returns:
//   - Predicted absolute path where the file will be located
//   - Error if validation fails or content conflict detected
//
// Conflict Detection:
//   - If the same filename is registered multiple times with different content
//     or a different reloadOnPush, returns error
//   - If the same filename is registered identically, no error (idempotent)
func (r *FileRegistry) Register(args ...any) (string, error) {
	// Validate argument count
	if len(args) != 3 && len(args) != 4 {
		return "", fmt.Errorf("file_registry.Register requires 3 arguments (type, filename, content) or 4 (…, reloadOnPush), got %d", len(args))
	}

	// Extract and validate file type
	fileType, ok := args[0].(string)
	if !ok {
		return "", fmt.Errorf("file_registry.Register: type must be a string, got %T", args[0])
	}

	// Extract and validate filename
	filename, ok := args[1].(string)
	if !ok {
		return "", fmt.Errorf("file_registry.Register: filename must be a string, got %T", args[1])
	}

	// Extract and validate content
	content, ok := args[2].(string)
	if !ok {
		return "", fmt.Errorf("file_registry.Register: content must be a string, got %T", args[2])
	}

	// Validate file type
	switch fileType {
	case "cert", "map", "file", "crt-list", "ca-file", "secret":
		// Valid types
	default:
		return "", fmt.Errorf("file_registry.Register: invalid file type %q, must be \"cert\", \"map\", \"file\", \"crt-list\", \"ca-file\", or \"secret\"", fileType)
	}

	// Extract the optional reloadOnPush flag. Only "file" honours it: a cert,
	// map, crt-list or ca-file already has its own reload rules and silently
	// accepting the flag there would promise something the deployer ignores.
	var reloadOnPush *bool
	if len(args) == 4 {
		flag, ok := args[3].(bool)
		if !ok {
			return "", fmt.Errorf("file_registry.Register: reloadOnPush must be a bool, got %T", args[3])
		}
		if fileType != "file" {
			return "", fmt.Errorf("file_registry.Register: reloadOnPush applies to type \"file\" only, got %q", fileType)
		}
		reloadOnPush = &flag
	}

	// A "ca-file" (mTLS trust bundle, referenced as `ca-file <path>`) lives in the
	// general storage dir and is delivered as a general file — the only
	// difference is it is flagged so a content-only rotation can apply via the
	// runtime API without a reload (see GetFiles). Resolve its path with the
	// "file" routing so the config reference stays a GeneralDir path.
	pathType := fileType
	if fileType == "ca-file" {
		pathType = "file"
	}

	// Compute predicted path using path resolver (same logic as pathResolver.GetPath() method)
	pathInterface, err := r.pathResolver.GetPath(filename, pathType)
	if err != nil {
		return "", fmt.Errorf("file_registry.Register: computing path: %w", err)
	}

	resolvedPath, ok := pathInterface.(string)
	if !ok {
		return "", fmt.Errorf("file_registry.Register: path resolver returned unexpected type %T", pathInterface)
	}

	// Thread-safe registration
	r.mu.Lock()
	defer r.mu.Unlock()

	// Create lookup key (type:filename)
	key := fileType + ":" + filename

	// Check for conflicts
	if existing, exists := r.registered[key]; exists {
		if existing.Content != content {
			return "", fmt.Errorf(
				"file_registry.Register: content conflict for %s %q - already registered with different content (existing size: %d, new size: %d)",
				fileType, filename, len(existing.Content), len(content),
			)
		}
		if reloadsOnPush(existing.ReloadOnPush) != reloadsOnPush(reloadOnPush) {
			return "", fmt.Errorf(
				"file_registry.Register: reloadOnPush conflict for %s %q - already registered with reloadOnPush=%t",
				fileType, filename, reloadsOnPush(existing.ReloadOnPush),
			)
		}

		// Same content - idempotent, return existing path
		return existing.Path, nil
	}

	// Register new file
	r.registered[key] = registeredFile{
		Type:         fileType,
		Filename:     filename,
		Content:      content,
		Path:         resolvedPath,
		ReloadOnPush: reloadOnPush,
	}

	return resolvedPath, nil
}

// GetFiles converts all registered files to dataplane AuxiliaryFiles structure.
// This is called by the renderer after template rendering completes to merge
// dynamic files with pre-declared auxiliary files.
func (r *FileRegistry) GetFiles() *dataplane.AuxiliaryFiles {
	r.mu.Lock()
	defer r.mu.Unlock()

	files := &dataplane.AuxiliaryFiles{}

	for _, reg := range r.registered {
		switch reg.Type {
		case "cert":
			files.SSLCertificates = append(files.SSLCertificates, auxiliaryfiles.SSLCertificate{
				Path:    reg.Path,
				Content: reg.Content,
			})

		case "map":
			files.MapFiles = append(files.MapFiles, auxiliaryfiles.MapFile{
				Path:    reg.Filename,
				Content: reg.Content,
			})

		case "crt-list":
			files.CRTListFiles = append(files.CRTListFiles, auxiliaryfiles.CRTListFile{
				Path:    reg.Path,
				Content: reg.Content,
			})

		case "file":
			files.GeneralFiles = append(files.GeneralFiles, auxiliaryfiles.GeneralFile{
				Filename:     path.Base(reg.Path),
				Path:         reg.Path,
				Content:      reg.Content,
				ReloadOnPush: reg.ReloadOnPush,
			})

		case "secret":
			files.SecretFiles = append(files.SecretFiles, auxiliaryfiles.SecretFile{
				Path:    reg.Path,
				Content: reg.Content,
				Seal:    auxiliaryfiles.SealContent(r.sealKey, reg.Content),
			})

		case "ca-file":
			// Delivered as a general file (disk-durable, referenced as
			// `ca-file <path>`), flagged so a content-only rotation applies via
			// the runtime API (`add ssl ca-file`) without a reload on v3.2+.
			files.GeneralFiles = append(files.GeneralFiles, auxiliaryfiles.GeneralFile{
				Filename: path.Base(reg.Path),
				Path:     reg.Path,
				Content:  reg.Content,
				IsCaFile: true,
			})
		}
	}

	return files
}

// MergeAuxiliaryFiles combines static and dynamically registered files without
// mutating either input. Conflicting definitions of one storage object fail.
func MergeAuxiliaryFiles(static, dynamic *dataplane.AuxiliaryFiles) (*dataplane.AuxiliaryFiles, error) {
	result := &dataplane.AuxiliaryFiles{}
	for _, files := range []*dataplane.AuxiliaryFiles{static, dynamic} {
		if files == nil {
			continue
		}
		result.MapFiles = append(result.MapFiles, files.MapFiles...)
		result.GeneralFiles = append(result.GeneralFiles, files.GeneralFiles...)
		result.SSLCertificates = append(result.SSLCertificates, files.SSLCertificates...)
		result.SSLCaFiles = append(result.SSLCaFiles, files.SSLCaFiles...)
		result.CRTListFiles = append(result.CRTListFiles, files.CRTListFiles...)
		result.SecretFiles = append(result.SecretFiles, files.SecretFiles...)
	}
	return dataplane.CanonicalizeAuxiliaryFiles(result)
}
