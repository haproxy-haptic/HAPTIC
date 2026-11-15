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

package renderer

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
)

var mapConverterPrefixes = [...]string{
	"(", "_str(", "_beg(", "_dir(", "_dom(", "_end(",
	"_int(", "_ip(", "_reg(", "_sub(",
}

// auxiliaryFilesConsistencyError describes a render output where the rendered
// HAProxy config references one or more map files the renderer did not
// register in its desired auxiliary file set. Without this check the manifest
// would not list the file, the agent would delete it as one of its own that
// the desired set no longer names, and the next reload would fail with a
// cryptic `failed to open pattern file <maps/X.map>` error and never recover.
type auxiliaryFilesConsistencyError struct {
	missingMaps []string
}

func (e *auxiliaryFilesConsistencyError) Error() string {
	return fmt.Sprintf(
		"rendered config references map files missing from the desired auxiliary set: %s — "+
			"this indicates a chart bug where a snippet emits a map_str(...) reference without a corresponding fileRegistry.Register(\"map\", ...). "+
			"Failing the render to prevent the agent from deleting the file and breaking subsequent HAProxy reloads",
		strings.Join(e.missingMaps, ", "))
}

// validateAuxiliaryFilesConsistency walks the rendered HAProxy config for map
// references and verifies every referenced map filename is present in the
// desired aux files. Catches a class of chart-side bugs early: under high
// resource churn, snippets that should register a map and emit its rule in
// lockstep have been observed to drift, leaving the rule referencing a map
// the renderer never registered. The manifest would then omit that file, the
// agent would delete it, and every subsequent reload would fail.
//
// Only map files are checked because they are the only auxiliary file type
// surfaced through this exact failure mode in production. SSL certificate
// references are validated by HAProxy at parse time (semantic validation
// covers them), and general files (errorfiles, etc.) live under
// `general/` and are referenced via `pathResolver.GetPath` consistently.
func validateAuxiliaryFilesConsistency(haproxyConfig string, auxFiles *dataplane.AuxiliaryFiles) error {
	referenced := extractMapReferences(haproxyConfig)
	if len(referenced) == 0 {
		return nil
	}

	registered := make(map[string]struct{}, len(auxFiles.MapFiles))
	for _, m := range auxFiles.MapFiles {
		registered[path.Base(m.Path)] = struct{}{}
	}
	for _, m := range auxFiles.SensitiveMapFiles {
		registered[path.Base(m.Path)] = struct{}{}
	}

	var missing []string
	for name := range referenced {
		if _, ok := registered[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &auxiliaryFilesConsistencyError{missingMaps: missing}
}

// extractMapReferences returns the set of map filenames referenced by
// map_*(...) converters in the rendered config. Only the basename is
// returned (e.g., `ssl-redirect-301.map`) so it can be compared against
// the auxiliary file set, which keys map files by basename.
func extractMapReferences(haproxyConfig string) map[string]struct{} {
	var out map[string]struct{}
	for offset := 0; offset < len(haproxyConfig); {
		relative := strings.Index(haproxyConfig[offset:], "map")
		if relative < 0 {
			break
		}
		start := offset + relative
		offset = start + len("map")
		if start > 0 && isRegexpWordByte(haproxyConfig[start-1]) {
			continue
		}
		argumentStart, found := mapConverterArgumentStart(haproxyConfig, offset)
		if !found {
			continue
		}
		relativeEnd := strings.IndexByte(haproxyConfig[argumentStart:], ')')
		if relativeEnd < 0 {
			break
		}
		offset = argumentStart + relativeEnd + 1
		if relativeEnd == 0 {
			continue
		}
		arg := strings.TrimSpace(haproxyConfig[argumentStart : argumentStart+relativeEnd])
		// The argument may be a path (`maps/foo.map`), an absolute path
		// (`/etc/haproxy/maps/foo.map`), or a comma-separated list when the
		// converter accepts a default value (`maps/foo.map,default`). Take
		// only the part before any comma, then the basename.
		if i := strings.Index(arg, ","); i >= 0 {
			arg = strings.TrimSpace(arg[:i])
		}
		// Skip references that are clearly not map filenames (HAProxy
		// `map_*` is also occasionally used with stick-table converters
		// like `map_str(`var(txn.x)`)` — but those are followed by a
		// `,delim` second arg or a non-`.map` filename).
		base := path.Base(arg)
		if !strings.HasSuffix(base, ".map") {
			continue
		}
		if out == nil {
			out = make(map[string]struct{})
		}
		out[base] = struct{}{}
	}
	return out
}

func mapConverterArgumentStart(config string, afterMap int) (int, bool) {
	for _, prefix := range mapConverterPrefixes {
		if strings.HasPrefix(config[afterMap:], prefix) {
			return afterMap + len(prefix), true
		}
	}
	return 0, false
}

func isRegexpWordByte(value byte) bool {
	return value == '_' || value >= '0' && value <= '9' ||
		value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}
