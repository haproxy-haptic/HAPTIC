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

package testrunner

import (
	"fmt"
	"path"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
)

// targetPrefixes are the target forms that name one artefact of the render. A
// target carrying one of them and resolving to nothing is an error, never a
// fallback: an absence assertion against a map that stopped being registered
// would otherwise be re-evaluated against haproxy.cfg, which never contained
// the string either, and pass green with the property it guards gone.
var targetPrefixes = []string{"map:", "file:", "cert:", "crt-list:", "secret:", "k8s:", "status:", "backend:"}

// errTargetNotFound reports a prefixed target the render did not produce.
type errTargetNotFound struct{ target string }

func (e *errTargetNotFound) Error() string {
	return fmt.Sprintf("target %s was not produced by this render — check the name, or that the template still registers it", e.target)
}

// errBackendProfileMissing reports a backend whose `from` chain names a profile
// section the render did not emit — a profile typo, or a template regression
// that stopped emitting it. Erroring (rather than returning a truncated chain)
// keeps the helper from hiding that regression behind an assertion that only
// checked the directives it happened to find.
type errBackendProfileMissing struct{ target, profile string }

func (e *errBackendProfileMissing) Error() string {
	return fmt.Sprintf("target %s inherits from profile %q, which this render did not produce — a profile typo, or the template stopped emitting it", e.target, e.profile)
}

// resolveTarget resolves the target content based on the target specification.
//
// Target format: "haproxy.cfg", "map:<name>", "file:<name>", "cert:<name>",
// "crt-list:<name>", "secret:<name>", "k8s:<template-name>", "status:<ns>/<name>:<phase>",
// "backend:<name>", "events", or "rendering_error".
func (r *Runner) resolveTarget(target, haproxyConfig string, auxiliaryFiles *dataplane.AuxiliaryFiles, k8sResources, statusPatches map[string]string, renderedEvents, renderError string) (string, error) {
	if target == "rendering_error" {
		return renderError, nil
	}

	// Kubernetes Events the templates recorded via recordEvent(), one per line
	// (`<Type> <Reason> <apiVersion> <Kind> <ns>/<name>: <message>`). Assert on
	// them with the standard contains / not_contains / match_count machinery.
	if target == "events" {
		return renderedEvents, nil
	}

	if target == names.MainTemplateName || target == "" {
		return haproxyConfig, nil
	}

	// k8sResources lookup by template name. Returns the rendered YAML
	// (potentially multi-doc with `---` separators) for `k8s:<name>`.
	if after, ok := strings.CutPrefix(target, "k8s:"); ok {
		if content, found := k8sResources[after]; found {
			return content, nil
		}
		return "", &errTargetNotFound{target}
	}

	// Status-patch lookup by `<namespace>/<name>:<phase>`. Returns the
	// JSON-marshalled status payload that the corresponding
	// statusPatch() template call emitted. Phase is one of
	// rendered / deployed / renderFailed / deployFailed.
	if after, ok := strings.CutPrefix(target, "status:"); ok {
		if content, found := statusPatches[after]; found {
			return content, nil
		}
		return "", &errTargetNotFound{target}
	}

	// Backend lookup by name: the backend's own section text plus every profile
	// section in its `from` chain, so a test asserts on a backend — its own
	// directives and the ones it inherits — without a (?ms) regex over the whole
	// config or knowing the generated `haptic-be-<hash>` profile name.
	if after, ok := strings.CutPrefix(target, "backend:"); ok {
		return backendTarget(after, target, haproxyConfig)
	}

	if content, found := r.resolveAuxiliaryFile(target, auxiliaryFiles); found {
		return content, nil
	}

	for _, prefix := range targetPrefixes {
		if strings.HasPrefix(target, prefix) {
			return "", &errTargetNotFound{target}
		}
	}

	// Default to haproxy.cfg if target format is unknown
	return haproxyConfig, nil
}

// configSection is one top-level section of a rendered HAProxy config: the
// column-0 header line and the indented body under it.
type configSection struct {
	keyword string // first header token: "backend", "defaults", "frontend", …
	name    string // section name (second token), "" for an unnamed section
	from    string // the `from <parent>` token on the header, "" if none
	text    string // the full section text, header line and body
}

// backendTarget resolves `backend:<name>` to the backend's own section text
// followed by every profile section in its `from` chain. It lets a test assert
// on a backend by name — the directives it declares and the ones it inherits
// from `defaults haptic-be-<hash> from haptic-base` — without a whole-config
// (?ms) regex or the generated profile hash. A missing backend, or a `from`
// parent the render did not emit, is an error, never a partial result: a
// truncated chain would let an assertion pass against a regression it should
// have caught. target is the caller's prefixed form, for the error message.
func backendTarget(name, target, haproxyConfig string) (string, error) {
	byKey := make(map[string]configSection)
	for _, section := range splitConfigSections(haproxyConfig) {
		// Key on keyword AND name so `frontend foo` and `backend foo` cannot
		// collide; a future caller must not assume a section name alone is unique.
		byKey[section.keyword+"\x00"+section.name] = section
	}

	backend, ok := byKey["backend\x00"+name]
	if !ok {
		return "", &errTargetNotFound{target}
	}

	var chain []string
	seen := make(map[string]bool)
	parent := backend.from
	for parent != "" && !seen[parent] {
		seen[parent] = true
		profile, ok := byKey["defaults\x00"+parent]
		if !ok {
			return "", &errBackendProfileMissing{target: target, profile: parent}
		}
		chain = append(chain, profile.text)
		parent = profile.from
	}

	return strings.Join(append(chain, backend.text), "\n"), nil
}

// splitConfigSections splits a rendered HAProxy config into its top-level
// sections. A section starts at a column-0, non-comment line and runs until the
// next one; indented and blank lines are its body. Column-0 comment lines
// separate sections and belong to none.
func splitConfigSections(config string) []configSection {
	var sections []configSection
	var current *configSection
	var body []string

	flush := func() {
		if current != nil {
			current.text = strings.Join(body, "\n")
			sections = append(sections, *current)
		}
	}

	for _, line := range strings.Split(config, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			if current != nil {
				body = append(body, line)
			}
			continue
		}
		flush()
		if strings.HasPrefix(line, "#") {
			current, body = nil, nil
			continue
		}
		fields := strings.Fields(line)
		section := configSection{keyword: fields[0]}
		if len(fields) > 1 {
			section.name = fields[1]
		}
		for i := 1; i+1 < len(fields); i++ {
			if fields[i] == "from" {
				section.from = fields[i+1]
				break
			}
		}
		current, body = &section, []string{line}
	}
	flush()
	return sections
}

// resolveAuxiliaryFile resolves auxiliary file content based on target prefix.
// The bool reports whether the render produced the named file at all, which is
// not the same as it having content — an empty registered map is found.
func (r *Runner) resolveAuxiliaryFile(target string, auxiliaryFiles *dataplane.AuxiliaryFiles) (string, bool) {
	// Handle nil auxiliaryFiles (can happen when rendering fails)
	if auxiliaryFiles == nil {
		return "", false
	}

	if after, ok := strings.CutPrefix(target, "map:"); ok {
		return r.findMapFile(after, auxiliaryFiles)
	}

	if after, ok := strings.CutPrefix(target, "file:"); ok {
		return r.findGeneralFile(after, auxiliaryFiles)
	}

	if after, ok := strings.CutPrefix(target, "cert:"); ok {
		return r.findCertificate(after, auxiliaryFiles)
	}

	if after, ok := strings.CutPrefix(target, "crt-list:"); ok {
		return r.findCRTListFile(after, auxiliaryFiles)
	}

	if after, ok := strings.CutPrefix(target, "secret:"); ok {
		for _, file := range auxiliaryFiles.SecretFiles {
			if path.Base(file.Path) == after {
				return file.Content, true
			}
		}
	}

	return "", false
}

// findMapFile searches for a map file by name.
// The mapName parameter can be just the filename (e.g., "host.map") or a path.
// This method first tries exact match, then falls back to basename matching
// for dynamically registered maps that have full paths.
func (r *Runner) findMapFile(mapName string, auxiliaryFiles *dataplane.AuxiliaryFiles) (string, bool) {
	if auxiliaryFiles == nil {
		return "", false
	}
	for _, mapFile := range auxiliaryFiles.MapFiles {
		if mapFile.Path == mapName || path.Base(mapFile.Path) == mapName {
			return mapFile.Content, true
		}
	}
	return "", false
}

// findGeneralFile searches for a general file by filename.
func (r *Runner) findGeneralFile(fileName string, auxiliaryFiles *dataplane.AuxiliaryFiles) (string, bool) {
	if auxiliaryFiles == nil {
		return "", false
	}
	for _, generalFile := range auxiliaryFiles.GeneralFiles {
		if generalFile.Filename == fileName {
			return generalFile.Content, true
		}
	}
	return "", false
}

// findCertificate searches for a certificate by path.
// The certName parameter should be just the filename (e.g., "certs.crt-list"),
// and this method will match it against the basename of the certificate's Path.
func (r *Runner) findCertificate(certName string, auxiliaryFiles *dataplane.AuxiliaryFiles) (string, bool) {
	if auxiliaryFiles == nil {
		return "", false
	}
	for _, sslCert := range auxiliaryFiles.SSLCertificates {
		// Extract basename from the absolute path for comparison
		// sslCert.Path is like "/tmp/.../ssl/certs.crt-list"
		// certName is like "certs.crt-list"
		if path.Base(sslCert.Path) == certName {
			return sslCert.Content, true
		}
	}
	return "", false
}

// findCRTListFile searches for a crt-list file by name.
// The crtListName parameter should be just the filename (e.g., "certificate-list.txt"),
// and this method will match it against the basename of the crt-list file's Path.
func (r *Runner) findCRTListFile(crtListName string, auxiliaryFiles *dataplane.AuxiliaryFiles) (string, bool) {
	if auxiliaryFiles == nil {
		return "", false
	}
	for _, crtList := range auxiliaryFiles.CRTListFiles {
		// Extract basename from the absolute path for comparison
		// crtList.Path is like "/tmp/.../ssl/certificate-list.txt"
		// crtListName is like "certificate-list.txt"
		if path.Base(crtList.Path) == crtListName {
			return crtList.Content, true
		}
	}
	return "", false
}

// populateTargetMetadata populates the target metadata fields for an assertion result.
// This should be called for ALL assertions (passed or failed) to provide visibility.
func (r *Runner) populateTargetMetadata(result *AssertionResult, target, targetName string, hasFailed bool) {
	result.Target = targetName
	result.TargetSize = len(target)

	// Only add preview for failed assertions to keep output size manageable
	if hasFailed && target != "" {
		result.TargetPreview = truncateString(target, 200)
	}
}

// truncateString truncates a string to maxLen characters.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
