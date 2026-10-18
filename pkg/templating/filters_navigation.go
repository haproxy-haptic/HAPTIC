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
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/haproxy-haptic/scriggo/native"
)

// renderContextKey is the context key for storing the render context (globals).
type renderContextKey struct{}

type incrementalRenderContextValuesKey struct{}

type incrementalRenderContextValues interface {
	incrementalRenderContextValue(string) (any, bool)
}

// RenderContextContextKey is exported for use in engine_scriggo.go.
var RenderContextContextKey = renderContextKey{}

// getRenderContextValue retrieves a *T stored under key in the render-context
// map. Returns nil if the env, the render-context map, or the typed entry is
// absent. getSharedContext / getStatusPatchCollector are thin wrappers over it.
func getRenderContextValue[T any](env native.Env, key string) *T {
	ctx := env.Context()
	if ctx == nil {
		return nil
	}
	value, ok := lookupRenderContextValue(ctx, key)
	if !ok {
		return nil
	}
	v, _ := value.(*T)
	return v
}

func lookupRenderContextValue(ctx context.Context, name string) (any, bool) {
	if ctx == nil {
		return nil, false
	}
	if values, ok := ctx.Value(incrementalRenderContextValuesKey{}).(incrementalRenderContextValues); ok {
		if value, exists := values.incrementalRenderContextValue(name); exists {
			return value, true
		}
	}
	renderCtx, ok := ctx.Value(RenderContextContextKey).(map[string]any)
	if !ok {
		return nil, false
	}
	value, exists := renderCtx[name]
	return value, exists
}

// getSharedContext retrieves the SharedContext from the template context.
// Returns nil if not found or not properly configured.
func getSharedContext(env native.Env) *SharedContext {
	return getRenderContextValue[SharedContext](env, declShared)
}

// scriggoFail stops template execution with an error message using
// Scriggo's native.Env.Stop() mechanism to properly halt template rendering.
//
// The function returns a string (which is never used because env.Stop() halts
// execution) so it can be used in expression context {{ }}.
//
// Usage in Scriggo templates:
//
//	{{ fail("Service not found") }}
//	{% if !service %}{{ fail("Service is required") }}{% end %}
func scriggoFail(env native.Env, msg string) string {
	env.Stop(&FailError{Message: msg})
	return "" // Never reached - env.Stop() halts execution
}

// scriggoDigString fuses dig + fallback + tostring into a single filter
// for the chart's most common polymorphic-value pattern: extracting a
// string field from a typegen-typed-or-untyped value with a default.
// The original 3-stage chain — dig(...) | fallback(default) | tostring()
// — appears 370+ times across the chart libraries (108 in
// haproxy-ingress.yaml, 84 in nginx-ingress.yaml, 73 in haproxytech.yaml,
// dozens more elsewhere); collapsing it removes most of that boilerplate.
//
// Semantics match the chain it replaces: dig(obj, keys...) navigates the
// nested structure (typed struct via JSON tag, or untyped map), and a nil
// result triggers defaultStr. A non-nil result is coerced via
// scriggoToString — which transparently handles the typegen tristate
// `*int64` / `*bool` pointers so chart code calling dig_string on an
// optional scalar field keeps producing "5" rather than a pointer string.
//
// Usage in Scriggo templates:
//
//	{{ ingress | dig_string("", "metadata", "annotations", "haproxy.org/foo") }}
//	{{ route | dig_string("default", "metadata", "name") }}
func scriggoDigString(obj any, defaultStr string, keys ...string) string {
	v := scriggoDig(obj, keys...)
	if v == nil {
		return defaultStr
	}
	return scriggoToString(v)
}

// scriggoDig navigates nested maps using a sequence of keys.
// Returns nil if any key along the path is missing or the value is nil.
// This is a Ruby-style dig function for cleaner nested access.
//
// Usage in Scriggo templates:
//
//	{{ ingress | dig("metadata", "namespace") | fallback("") }}
//	{{ path | dig("backend", "service", "name") | fallback("unknown") }}
//	{%- var port = path | dig("backend", "service", "port", "number") | fallback(80) %}
func scriggoDig(obj any, keys ...string) any {
	if obj == nil || len(keys) == 0 {
		return obj
	}

	// Fast path: direct map[string]any (99% of cases in K8s templates)
	// Avoids reflection overhead from isNilValue() on every iteration
	if m, ok := obj.(map[string]any); ok {
		return digMapFast(m, keys)
	}

	// Slow path with reflection for typed nil pointers and other types
	return digReflect(obj, keys)
}

// digMapFast is the optimized path for map[string]any traversal.
// Handles nested maps without reflection overhead. When an
// intermediate value is something other than map[string]any /
// map[string]string (e.g. a typegen-produced typed struct that the
// chart stored as a value in a map[string]any wrapper), traversal
// falls back to digReflect for the remaining keys — losing the
// fast-path speed for this dig call but keeping the typed-struct
// chart pattern working end-to-end.
func digMapFast(m map[string]any, keys []string) any {
	for i, key := range keys {
		val, ok := m[key]
		if !ok || val == nil {
			return nil
		}

		// If this is the last key, return the value directly
		if i == len(keys)-1 {
			return val
		}

		// Try to continue traversal with nested map[string]any.
		next, ok := val.(map[string]any)
		if !ok {
			// map[string]string is a common K8s shape (labels,
			// annotations, matchLabels). Handled here for any
			// position in the chain, not just the last key, so
			// dig() keeps working when chart code wraps a typed
			// resource's labels into a multi-level path.
			if strMap, isStrMap := val.(map[string]string); isStrMap {
				if strVal, found := strMap[keys[i+1]]; found {
					if i+1 == len(keys)-1 {
						return strVal
					}
					// Beyond a map[string]string value the chain
					// can't continue — strings have no further
					// navigable fields. Drop to reflection so the
					// shared traversal returns nil at the right
					// boundary instead of pretending the path exists.
					return digReflect(strVal, keys[i+2:])
				}
				return nil
			}
			// Typed structs, slices, primitives, or any other
			// shape — fall back to reflection for the remaining
			// keys. The chart's typegen-produced typed structs land
			// here when the chart embeds them inside a
			// map[string]any (e.g. `map[string]any{"backend":
			// ingress.Spec.DefaultBackend}` then chart code does
			// `dig(path, "backend", "service", "name")`).
			return digReflect(val, keys[i+1:])
		}
		m = next
	}
	return m
}

// digReflect handles typed nil pointers and other edge cases with reflection.
//
// It also navigates *typed structs* — the shape pkg/k8s/typegen produces
// and the chart's render-time wiring (rendercontext.BuildResourcesValue)
// hands to templates via the typed `resources` global. Struct field
// lookup is by JSON tag so chart code stays identical between the
// untyped map shape (`dig(gw, "metadata", "name")`) and the typed
// struct shape (same call, same key strings). This is the property
// that lets the chart adopt typed access incrementally without
// rewriting every dig() call site.
func digReflect(obj any, keys []string) any {
	// Handle typed nil values (e.g., *map[string]any with nil pointer)
	if isNilValue(obj) {
		return nil
	}

	current := obj
	for _, key := range keys {
		if current == nil {
			return nil
		}

		switch v := current.(type) {
		case map[string]any:
			val, ok := v[key]
			if !ok {
				return nil
			}
			current = val
		case map[string]string:
			val, ok := v[key]
			if !ok {
				return nil
			}
			current = val
		default:
			// Generic string-keyed-map fallback. Reached when the
			// value is a `map[string]<T>` for some concrete T that
			// isn't already handled by the explicit cases above
			// (e.g. `map[string][]any` from group_by, or
			// `map[string]int` from a custom counter). Without this,
			// dig() falls through to digStructField on a map value
			// and silently returns nil — which produced the chart's
			// path-map-empty bug where `pathGroups | dig(pathKey)`
			// returned nil for every key.
			if next, ok := digGenericMap(current, key); ok {
				current = next
				continue
			}
			next, ok := digStructField(current, key)
			if !ok {
				return nil
			}
			current = next
		}
	}

	return current
}

// digGenericMap looks key up in a generic string-keyed map of any
// concrete value type — `map[string]<T>` where T is anything the
// digReflect explicit cases don't already cover (e.g.
// `map[string][]any` produced by group_by). The explicit cases for
// `map[string]any` / `map[string]string` are fast paths; this is
// the reflection-based fallback for the long tail.
//
// Returns (value, true) when the value is a map whose key type is
// string and the key is present; (nil, true) when the key is
// absent (string-keyed map, just missing this key — distinguished
// from "wrong shape" so the caller knows to stop walking, not to
// fall through to struct-field lookup); (nil, false) when the
// value isn't a string-keyed map at all (caller should try the
// next dispatch).
func digGenericMap(obj any, key string) (any, bool) {
	v := reflect.ValueOf(obj)
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Map {
		return nil, false
	}
	if v.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	mv := v.MapIndex(reflect.ValueOf(key))
	if !mv.IsValid() {
		return nil, true
	}
	return mv.Interface(), true
}

// digStructField navigates ONE level into a typed struct (or
// pointer/interface wrapping one). Returns ok=false when the value
// isn't a struct, the field doesn't exist, or any pointer along the
// way is nil. The found field's .Interface() is returned as `any` so
// subsequent dig iterations can treat it uniformly with the map path.
//
// JSON-tag matching is the primary lookup — chart authors write
// `dig(gw, "metadata", "name")` with JSON-style keys against both
// untyped maps and typed structs. We populate the json:"name" tags
// from typegen.goFieldName at type-generation time precisely so
// this lookup works without a separate path through the chart.
//
// Reflection cost: ~50ns/level under a sync.Map cache (see
// structFieldLookup). For a typical 3-level dig path that's ~150ns,
// well below the map-fast-path's ~25ns but irrelevant against the
// ~100µs render budget per template.
func digStructField(obj any, key string) (any, bool) {
	v := reflect.ValueOf(obj)
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil, false
	}
	t := v.Type()
	idx, ok := structFieldIndex(t, key)
	if !ok {
		return nil, false
	}
	field := v.Field(idx)
	if !field.IsValid() {
		return nil, false
	}
	// Tristate transparency for pointer-wrapped scalars (issue #52
	// fix): typegen emits *int64 / *bool / *float64 for optional
	// numeric / bool fields so json.Unmarshal preserves the
	// distinction between "absent from source" (nil pointer) and
	// "explicitly zero" (non-nil pointer to zero value). Chart code
	// reads back plain int64 / bool — dereference here and let the
	// nil case fall through to the omitempty branch below so
	// `dig | fallback` fires for absent, while explicit zero passes
	// through as 0 / false.
	if field.Kind() == reflect.Pointer && needsTristatePointerKind(field.Type().Elem().Kind()) {
		if field.IsNil() {
			return nil, false
		}
		return field.Elem().Interface(), true
	}
	// Optional + zero-value normalisation. The chart's universal
	// `dig(obj, "field") | fallback(default)` pattern assumes
	// "absent → nil → fallback fires", which holds for untyped
	// maps (missing keys are nil) but NOT for typegen-produced
	// typed structs, where an unpopulated optional string field
	// is the zero value `""`, not nil — fallback then doesn't
	// fire and downstream key-building (e.g. namespace/name
	// composition) silently produces malformed keys.
	//
	// Strings, structs, slices and maps fall through here — they
	// can't use the pointer-wrapped tristate path because the chart
	// also reads them for "is this empty?" semantics that conflict
	// with the explicit-zero case (a string field's "" already
	// means "no value", an empty slice means "no items", etc.).
	if isStructFieldOmitempty(t, idx) && field.IsZero() {
		return nil, false
	}
	return field.Interface(), true
}

// needsTristatePointerKind mirrors typegen.needsTristatePointer's
// scalar-kind rule for the digStructField dereference path. Kept
// here (rather than importing typegen) so pkg/templating stays a
// pure library — typegen lives one layer up. The two functions must
// agree on which Kinds get the pointer treatment; a regression test
// in pkg/k8s/typegen pins the typegen side and TestDigStructField_
// PointerOptional pins the navigation side.
func needsTristatePointerKind(k reflect.Kind) bool {
	switch k {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// isStructFieldOmitempty reports whether the struct field at the
// given index has `,omitempty` in its json tag. Used by digStructField
// to decide whether a zero value means "field absent" (return nil) or
// "field explicitly zero" (return the zero value).
func isStructFieldOmitempty(t reflect.Type, idx int) bool {
	tag := t.Field(idx).Tag.Get("json")
	if tag == "" {
		return false
	}
	return slices.Contains(strings.Split(tag, ",")[1:], "omitempty")
}

// jsonNameCache caches per-type JSON-name → field-index maps. Without
// this, every dig step on a typed struct re-scans every field via
// reflect — quadratic in field count for nested dig walks. The
// generated types come from typegen's Convert(), so they have stable
// identities across renders within one iteration; caching by
// reflect.Type is safe.
var jsonNameCache sync.Map // map[reflect.Type]map[string]int

// structFieldIndex returns the field index for the given JSON name
// on the supplied struct type, or -1 if no such field exists. Falls
// back to the Go field name when no field carries a matching json
// tag — covers types declared outside typegen that the chart might
// reach via dig (e.g. test fixtures), though typegen-produced types
// always have json tags.
func structFieldIndex(t reflect.Type, jsonName string) (int, bool) {
	if cached, ok := jsonNameCache.Load(t); ok {
		idx, ok := cached.(map[string]int)[jsonName]
		return idx, ok
	}
	m := buildJSONNameIndex(t)
	jsonNameCache.Store(t, m)
	idx, ok := m[jsonName]
	return idx, ok
}

// buildJSONNameIndex scans every field on t and builds the
// JSON-name → field-index map. Each field is indexed under TWO
// names — its JSON tag (the canonical chart-template key) and its
// Go field name (the fallback). Conflicts between the two are
// effectively impossible because typegen capitalises the JSON name
// into the Go name, so the JSON tag is always lower-case and the
// Go name is always upper-case-first.
func buildJSONNameIndex(t reflect.Type) map[string]int {
	out := make(map[string]int, t.NumField()*2)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		out[f.Name] = i
		tag := f.Tag.Get("json")
		if tag == "" {
			continue
		}
		// Strip ",omitempty" and the rest. encoding/json's own
		// convention treats everything after the first comma as
		// directives.
		name := tag
		if comma := strings.IndexByte(tag, ','); comma >= 0 {
			name = tag[:comma]
		}
		if name != "" && name != "-" {
			out[name] = i
		}
	}
	return out
}

// scriggoSelectAttr filters items by attribute existence or value.
// This is a Jinja2-compatible filter for selecting items from a sequence.
//
// Usage in Scriggo templates:
//
//	{%- var rules_with_http = selectattr(rules, "http") %}
//	{%- var exact_paths = selectattr(paths, "pathType", "eq", "Exact") %}
//	{%- var matching = selectattr(paths, "pathType", "in", []string{"Exact", "Prefix"}) %}
//
// Supported tests:
//   - selectattr(items, "attr") - items where attr is defined and truthy
//   - selectattr(items, "attr", "eq", value) - items where attr equals value
//   - selectattr(items, "attr", "ne", value) - items where attr does not equal value
//   - selectattr(items, "attr", "in", list) - items where attr value is in list
func scriggoSelectAttr(items any, attr string, args ...any) []any {
	result := []any{}

	// Handle nil input
	if items == nil {
		return result
	}

	// Convert items to slice
	itemsSlice, ok := toSlice(items)
	if !ok {
		return result
	}

	// Parse optional test and value arguments
	var test string
	var testValue any
	if len(args) >= 2 {
		test, _ = args[0].(string)
		testValue = args[1]
	}

	for _, item := range itemsSlice {
		if item == nil {
			continue
		}

		// Get attribute value using dig
		attrValue := scriggoDig(item, attr)

		// Apply test based on arguments
		switch test {
		case "eq":
			// Equal test
			if scriggoToString(attrValue) == scriggoToString(testValue) {
				result = append(result, item)
			}
		case "ne":
			// Not equal test
			if scriggoToString(attrValue) != scriggoToString(testValue) {
				result = append(result, item)
			}
		case "in":
			// Membership test
			if isValueInList(attrValue, testValue) {
				result = append(result, item)
			}
		default:
			// Default: check if attribute is defined (not nil).
			// An empty map or slice still counts as "defined" - only nil means absent.
			if attrValue != nil {
				result = append(result, item)
			}
		}
	}

	return result
}

// isValueInList checks if a value is in a list (for selectattr "in" test).
func isValueInList(value, list any) bool {
	if list == nil {
		return false
	}

	valueStr := scriggoToString(value)

	// Handle []string
	if strList, ok := list.([]string); ok {
		return slices.Contains(strList, valueStr)
	}

	// Handle []any
	if anyList, ok := list.([]any); ok {
		for _, item := range anyList {
			if scriggoToString(item) == valueStr {
				return true
			}
		}
		return false
	}

	// Handle []any (same as []any)
	listSlice, ok := toSlice(list)
	if !ok {
		return false
	}

	for _, item := range listSlice {
		if scriggoToString(item) == valueStr {
			return true
		}
	}
	return false
}

func scriggoJoinKey(sep string, parts ...any) string {
	if len(parts) == 0 {
		return ""
	}

	strs := make([]string, len(parts))
	for i, part := range parts {
		strs[i] = scriggoToString(part)
	}
	return strings.Join(strs, sep)
}

// scriggoMerge returns a new map with all key-value pairs from both maps.
// Values from the updates map override values from the original map.
//
// Usage in Scriggo templates:
//
//	{% var config = map[string]any{"a": 1, "b": 2} %}
//	{% config = merge(config, map[string]any{"b": 3, "c": 4}) %}
//	{# Result: {"a": 1, "b": 3, "c": 4} #}
func scriggoMerge(dict, updates map[string]any) map[string]any {
	result := make(map[string]any, len(dict)+len(updates))
	maps.Copy(result, dict)
	maps.Copy(result, updates)
	return result
}

// scriggoKeys returns a sorted slice of keys from a map.
// Keys are sorted alphabetically for deterministic iteration order.
// Works with any map type that has string keys (map[string]any, map[string][]any, etc.).
//
// Usage in Scriggo templates:
//
//	{% var config = map[string]any{"b": 2, "a": 1, "c": 3} %}
//	{% for _, key := range keys(config) %}
//	{{ key }}: {{ config[key] }}
//	{% end %}
//	{# Output: a: 1, b: 2, c: 3 (sorted) #}
func scriggoKeys(dict any) []string {
	if dict == nil {
		return []string{}
	}

	rv := reflect.ValueOf(dict)
	if rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
		return []string{}
	}

	keys := make([]string, 0, rv.Len())
	for _, k := range rv.MapKeys() {
		keys = append(keys, k.String())
	}
	slices.Sort(keys)
	return keys
}

// scriggoNamespace creates a mutable map for storing state across loop iterations,
// enabling mutable state patterns in templates.
//
// The returned top-level map never aliases init.
//
// Usage in Scriggo templates:
//
//	{# Create namespace with initial values #}
//	{%- var ns = namespace(map[string]any{"seen": []any{}, "count": 0}) -%}
//
//	{# Modify values (maps are mutable) #}
//	{%- for _, item := range items -%}
//	  {%- ns["count"] = ns["count"].(int) + 1 -%}
//	  {%- ns["seen"] = append(ns["seen"].([]any), item) -%}
//	{%- end -%}
//
//	{# Empty namespace #}
//	{%- var ns = namespace(nil) -%}
func scriggoNamespace(init map[string]any) map[string]any {
	if init == nil {
		return make(map[string]any)
	}
	return maps.Clone(init)
}

// scriggoCoalesce returns the first non-nil value, or the default if all are nil.
// This is needed because Scriggo's `default` operator only works when the left
// expression is a predeclared identifier (variable, constant, function), a render
// expression, or a macro call. It does NOT work with field access expressions
// like `obj.field` or map index expressions like `map["key"]`.
//
// See: https://scriggo.com/templates/specification#default-expression
//
// Usage in Scriggo templates:
//
//	{%- var items = coalesce(obj.field, []any{}) -%}
//	{%- var name = coalesce(user.name, "anonymous") -%}
func scriggoCoalesce(value, defaultVal any) any {
	if value == nil {
		return defaultVal
	}
	return value
}

// scriggoJoin joins a string slice with a separator.
// Accepts any to handle both []string and []any from templates.
//
// Usage in Scriggo templates:
//
//	{%- var list = join(hosts, ", ") -%}
//	{{ items | join(" ") }}
func scriggoJoin(items any, sep string) string {
	switch v := items.(type) {
	case []string:
		return strings.Join(v, sep)
	case []any:
		strs := make([]string, len(v))
		for i, item := range v {
			strs[i] = scriggoToString(item)
		}
		return strings.Join(strs, sep)
	default:
		return ""
	}
}
