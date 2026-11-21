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
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// Keys the Backend record and its nested records accept. A key outside these
// sets is an error: a typo that silently dropped a value would leave the
// record under-describing the section text it was emitted from.
var (
	backendRecordKeys = []string{
		"name", "profile", "mode", "guid", "balance", "hashType",
		"shape", "shapeReason", "servers", "defaultServer", "body", "comments",
	}
	// profileRecordKeys are the shape values a profile is content-addressed
	// over. `profile` is the directive lines that go inside the named defaults.
	profileRecordKeys = []string{"mode", "balance", "hashType", "defaultServer", "profile"}
	serverRecordKeys  = []string{"name", "address", "port", "weight", "disabled", "guid", "comment", "extra", "template"}
	keywordRecordKeys = []string{"name", "args"}
)

var (
	// spop is the SPOE backend mode HAProxy 3.1 added; the bundled chart emits it.
	backendModes  = []string{"http", "tcp", "spop"}
	backendShapes = []string{renderplan.ShapeDynamic, renderplan.ShapeStructural}
)

func validateBackend(backend *renderplan.Backend) error {
	if backend.Name == "" {
		return fmt.Errorf("planRegistry.Backend: %q is required", "name")
	}
	if !sectionNamePattern.MatchString(backend.Name) {
		return fmt.Errorf("planRegistry.Backend: name %q must match %s", backend.Name, sectionNamePattern)
	}
	if backend.Mode != "" && !slices.Contains(backendModes, backend.Mode) {
		return fmt.Errorf("planRegistry.Backend: backend %q has mode %q, want one of %s",
			backend.Name, backend.Mode, strings.Join(backendModes, ", "))
	}
	if !slices.Contains(backendShapes, backend.Shape) {
		return fmt.Errorf("planRegistry.Backend: backend %q has shape %q, want one of %s",
			backend.Name, backend.Shape, strings.Join(backendShapes, ", "))
	}
	for _, server := range backend.Servers {
		if server.Name == "" {
			return fmt.Errorf("planRegistry.Backend: backend %q has a server without a name", backend.Name)
		}
	}
	return nil
}

// recordDigest is the digest of the declared record. Only the digests derived
// from it are excluded, so the body and comments still take part through their
// own digests.
func recordDigest(backend *renderplan.Backend) string {
	record := *backend
	record.RecordDigest = ""
	record.TextDigest = ""
	encoded, err := json.Marshal(&record)
	if err != nil {
		// Only a new field that is not JSON-safe can get here, and digesting a
		// partial encoding would make two different backends compare equal.
		panic(fmt.Sprintf("planRegistry: encoding the backend record failed: %v", err))
	}
	return renderplan.Digest(encoded)
}

// recordDecoder reads a template-supplied record strictly: unknown keys and
// wrong value types are errors, and the first one is kept so the caller can
// report it after decoding the whole record.
type recordDecoder struct {
	macro  string
	record map[string]any
	err    error
}

func newRecordDecoder(macro string, record map[string]any, allowed []string) *recordDecoder {
	dec := &recordDecoder{macro: macro, record: record}
	for _, key := range slices.Sorted(maps.Keys(record)) {
		if slices.Contains(allowed, key) {
			continue
		}
		if suggestion := nearestKey(key, allowed); suggestion != "" {
			dec.failf("unknown key %q (did you mean %q?)", key, suggestion)
			break
		}
		dec.failf("unknown key %q, valid keys are %s", key, strings.Join(allowed, ", "))
		break
	}
	return dec
}

func (d *recordDecoder) failf(format string, args ...any) {
	d.adopt(fmt.Errorf("%s: %s", d.macro, fmt.Sprintf(format, args...)))
}

// adopt keeps the first error; later ones are consequences of it.
func (d *recordDecoder) adopt(err error) {
	if d.err == nil {
		d.err = err
	}
}

func (d *recordDecoder) str(key string) string {
	return d.strOr(key, "")
}

func (d *recordDecoder) strOr(key, fallback string) string {
	value, ok := d.record[key]
	if !ok || value == nil {
		return fallback
	}
	text, ok := value.(string)
	if !ok {
		d.failf("%q must be a string, got %T", key, value)
		return fallback
	}
	return text
}

func (d *recordDecoder) strSlice(key string) []string {
	value, ok := d.record[key]
	if !ok || value == nil {
		return nil
	}
	switch typed := value.(type) {
	case []string:
		return normalizeStrings(typed)
	case []any:
		lines := make([]string, 0, len(typed))
		for _, element := range typed {
			text, ok := element.(string)
			if !ok {
				d.failf("%q must contain strings, got %T", key, element)
				return nil
			}
			lines = append(lines, text)
		}
		return lines
	default:
		d.failf("%q must be a list of strings, got %T", key, value)
		return nil
	}
}

func (d *recordDecoder) intVal(key string) int {
	value, ok := d.record[key]
	if !ok || value == nil {
		return 0
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		if typed != float64(int(typed)) {
			d.failf("%q must be a whole number, got %v", key, typed)
			return 0
		}
		return int(typed)
	default:
		d.failf("%q must be a number, got %T", key, value)
		return 0
	}
}

// optionalIntVal distinguishes an absent key (nil) from an explicit zero.
func (d *recordDecoder) optionalIntVal(key string) *int {
	if value, ok := d.record[key]; !ok || value == nil {
		return nil
	}
	v := d.intVal(key)
	return &v
}

func (d *recordDecoder) boolVal(key string) bool {
	value, ok := d.record[key]
	if !ok || value == nil {
		return false
	}
	flag, ok := value.(bool)
	if !ok {
		d.failf("%q must be a bool, got %T", key, value)
		return false
	}
	return flag
}

// records reads a list of nested records under key.
func (d *recordDecoder) records(key string) []map[string]any {
	value, ok := d.record[key]
	if !ok || value == nil {
		return nil
	}
	switch typed := value.(type) {
	case []map[string]any:
		return typed
	case []any:
		nested := make([]map[string]any, 0, len(typed))
		for _, element := range typed {
			entry, ok := element.(map[string]any)
			if !ok {
				d.failf("%q must contain maps, got %T", key, element)
				return nil
			}
			nested = append(nested, entry)
		}
		return nested
	default:
		d.failf("%q must be a list of maps, got %T", key, value)
		return nil
	}
}

func (d *recordDecoder) servers(key string) []renderplan.Server {
	entries := d.records(key)
	if len(entries) == 0 {
		return nil
	}
	servers := make([]renderplan.Server, 0, len(entries))
	for _, entry := range entries {
		nested := newRecordDecoder(d.macro+" "+key, entry, serverRecordKeys)
		servers = append(servers, renderplan.Server{
			Name:     nested.str("name"),
			Address:  nested.str("address"),
			Port:     nested.intVal("port"),
			Weight:   nested.optionalIntVal("weight"),
			Disabled: nested.boolVal("disabled"),
			GUID:     nested.str("guid"),
			Comment:  nested.str("comment"),
			Extra:    nested.keywords("extra"),
			Template: nested.intVal("template"),
		})
		if nested.err != nil {
			d.adopt(nested.err)
			return nil
		}
	}
	return servers
}

func (d *recordDecoder) keywords(key string) []renderplan.KeywordArg {
	entries := d.records(key)
	if len(entries) == 0 {
		return nil
	}
	keywords := make([]renderplan.KeywordArg, 0, len(entries))
	for _, entry := range entries {
		nested := newRecordDecoder(d.macro+" "+key, entry, keywordRecordKeys)
		keyword := renderplan.KeywordArg{Name: nested.str("name"), Args: nested.strSlice("args")}
		if nested.err != nil {
			d.adopt(nested.err)
			return nil
		}
		if keyword.Name == "" {
			d.failf("%q needs a keyword name", key)
			return nil
		}
		keywords = append(keywords, keyword)
	}
	return keywords
}

// nearestKey returns the valid key closest to an unknown one, or "" when none
// is close enough to be a plausible typo.
func nearestKey(unknown string, valid []string) string {
	best, bestDistance := "", len(unknown)/2+2
	for _, candidate := range valid {
		distance := editDistance(strings.ToLower(unknown), strings.ToLower(candidate))
		if distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
