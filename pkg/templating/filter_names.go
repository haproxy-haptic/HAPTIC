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

// Filter name constants for template engines.
const (
	// FilterSortBy sorts items by JSONPath criteria.
	FilterSortBy = "sort_by"

	// FilterGlobMatch filters items by glob pattern.
	FilterGlobMatch = "glob_match"

	// FilterStrip removes leading/trailing whitespace.
	FilterStrip = "strip"

	// FilterB64Decode decodes base64-encoded strings.
	FilterB64Decode = "b64decode"

	// FilterB64Encode encodes strings as standard base64.
	FilterB64Encode = "b64encode"

	// FilterDebug outputs debug information for items.
	FilterDebug = "debug"

	// FilterIndent indents each line of a string by a specified amount.
	FilterIndent = "indent"
)

// Function name constants for template engines.
const (
	// FuncFail stops template execution with an error message.
	FuncFail = "fail"

	// FuncMerge merges two maps, returning a new map.
	// Values from the second map override values from the first.
	FuncMerge = "merge"

	// FuncKeys returns sorted keys from a map.
	FuncKeys = "keys"

	// FuncSortStrings sorts a []any slice as strings.
	// Useful when append() returns []any but you need sorted strings.
	FuncSortStrings = "sort_strings"

	// FuncSortInts sorts a []any slice of integer values numerically.
	// Useful for sorting port numbers, IDs, or any other integer keys
	// where lexicographic sort_strings ordering would be wrong
	// (e.g. "10" before "2"). Non-integer entries are coerced via
	// toint() and sort to the front (toint of a non-numeric value
	// returns 0).
	FuncSortInts = "sort_ints"

	// String manipulation functions (Scriggo).

	// FuncStringsContains checks if a string contains a substring.
	FuncStringsContains = "strings_contains"

	// FuncStringsSplit splits a string by a separator.
	FuncStringsSplit = "strings_split"

	// FuncStringsTrim trims whitespace from a string.
	FuncStringsTrim = "strings_trim"

	// FuncStringsLower converts a string to lowercase.
	FuncStringsLower = "strings_lower"

	// FuncStringsSplitN splits a string by a separator with a maximum number of parts.
	FuncStringsSplitN = "strings_splitn"

	// Type conversion functions (Scriggo only).

	// FuncToString converts a value to string.
	FuncToString = "tostring"

	// FuncToInt converts a value to int.
	FuncToInt = "toint"

	// FuncToFloat converts a value to float64.
	FuncToFloat = "tofloat"

	// Utility functions (Scriggo only).

	// FuncCeil returns the ceiling of a float.
	FuncCeil = "ceil"

	// FuncSeq generates a sequence of integers from 0 to n-1.
	// Syntax: seq(n) returns []int{0, 1, 2, ..., n-1}.
	FuncSeq = "seq"

	// FuncRegexSearch checks if a string matches a regex pattern.
	FuncRegexSearch = "regex_search"

	// FuncIsDigit checks if a string contains only digits.
	FuncIsDigit = "isdigit"

	// FuncSanitizeRegex escapes regex special characters.
	FuncSanitizeRegex = "sanitize_regex"

	// FuncTitle converts a string to title case.
	FuncTitle = "title"

	// FuncOnOwnLines starts each snippet a render_glob matches on a new line.
	// distributeOnOwnLines rewrites it into the two declared functions below.
	FuncOnOwnLines      = "on_own_lines"
	funcOnOwnLinesStart = "on_own_lines_start"
	funcOnOwnLinesJoin  = "on_own_lines_join"

	// FuncIsNil checks if a value is nil, including typed nil pointers.
	// In Go, a typed nil pointer stored in an any is not equal to nil.
	// This function uses reflection to properly check for nil pointers.
	// Syntax: isNil(value) returns bool.
	FuncIsNil = "isNil"

	// FuncDig navigates nested maps/structures using a sequence of keys.
	// Returns nil if any key along the path is missing.
	// Ruby-style dig: obj | dig("metadata", "namespace") | fallback("")
	// Available in: Scriggo only.
	FuncDig = "dig"

	// FuncDigString fuses the very common dig + fallback + tostring chain
	// used at the chart's polymorphic-value boundaries (annotation lookups,
	// metadata extraction from any-typed values, etc.) into one filter.
	// Syntax: obj | dig_string(defaultStr, keys...) returns string.
	// Equivalent to: obj | dig(keys...) | fallback(defaultStr) | tostring().
	// Available in: Scriggo only.
	FuncDigString = "dig_string"

	// FuncToStringSlice converts []any to []string.
	// Available in: Scriggo only.
	FuncToStringSlice = "toStringSlice"

	// FuncCidrPartition splits a CIDR set into the disjoint blocks each prefix is a union of.
	FuncCidrPartition = "cidr_partition"

	// FuncJoin joins a string slice with a separator.
	// Available in: Scriggo only.
	FuncJoin = "join"

	// FuncReplace replaces all occurrences of old with new.
	FuncReplace = "replace"

	// FuncCoalesce returns the first non-nil value, or the default if nil.
	// Workaround for Scriggo's default operator limitation with field access.
	FuncCoalesce = "coalesce"

	// FuncFallback returns the first non-nil value, or the fallback if nil.
	// Jinja2-style alias for coalesce, works well with pipe syntax (e.g., {{ value | fallback("default") }}).
	FuncFallback = "fallback"

	// FuncNamespace creates a mutable map for storing state in loops.
	FuncNamespace = "namespace"

	// Slice manipulation functions.

	// FuncToSlice converts any value to []any for safe ranging.
	// Returns empty slice if input is nil, otherwise converts to []any.
	// Syntax: toSlice(value) - returns []any.
	FuncToSlice = "toSlice"

	// FuncToStrMap normalises any string-keyed map (map[string]string
	// from typegen-produced fields like metadata.labels / matchLabels,
	// or map[string]any from the untyped store path) into a uniform
	// map[string]string for template iteration. Returns nil for nil
	// input. Non-string values from a map[string]any input are
	// coerced via tostring().
	//
	// Motivation: chart code that did `value.(map[string]any)` on a
	// label / matchLabels field panicked once the typed-watched-
	// resources path landed, because typegen builds those fields as
	// `map[string]string` (matching the K8s OpenAPI schema). A
	// shape-agnostic conversion at the call site keeps chart code
	// from having to branch per shape.
	//
	// Syntax: to_str_map(value) - returns map[string]string.
	FuncToStrMap = "to_str_map"

	// Deduplication and filtering functions.

	// FuncFirstSeen checks if a composite key is being seen for the first time.
	// Returns true on first occurrence, false on subsequent calls with same key.
	// Thread-safe for parallel template rendering.
	// Syntax: first_seen("prefix", key1, key2, ...) returns bool.
	FuncFirstSeen = "first_seen"

	// FuncSelectAttr filters items by attribute existence or value.
	// Jinja2-compatible filter for selecting items from a sequence.
	// Syntax:
	//   selectattr(items, "attr")           - items where attr is defined/truthy
	//   selectattr(items, "attr", "eq", v)  - items where attr equals v
	//   selectattr(items, "attr", "ne", v)  - items where attr not equals v
	//   selectattr(items, "attr", "in", list) - items where attr is in list
	FuncSelectAttr = "selectattr"

	// FuncJoinKey joins multiple values into a composite key string with a separator.
	// Automatically converts all values to strings.
	// Syntax: join_key("_", val1, val2, ...) returns string.
	FuncJoinKey = "join_key"

	// FuncShardSlice divides a slice into N shards and returns the portion for a given shard index.
	// Used for parallel template rendering where work is split across goroutines.
	// Syntax: shard_slice(items, shardIndex, totalShards) returns []any.
	FuncShardSlice = "shard_slice"

	// Path utility functions.

	// FuncBasename extracts the filename from a path (like Unix basename command).
	// Useful for extracting sanitized filenames from paths returned by fileRegistry.Register().
	// Syntax: basename(path) returns string.
	FuncBasename = "basename"

	// Status patch functions.

	// FuncStatusPatch registers a status patch for a Kubernetes resource during rendering.
	// Syntax: statusPatch(resource, variants).
	FuncStatusPatch = "statusPatch"

	// FuncRecordEvent registers a Kubernetes Warning Event to emit against a
	// resource during rendering (resource-agnostic — namespace/name/apiVersion/
	// kind are read off the passed resource object via dig, so it works for any
	// watched resource or CRD).
	// Syntax: recordEvent(resource, reason, message).
	FuncRecordEvent = "recordEvent"

	// FuncCondition builds a metav1.Condition-compatible map.
	// Syntax: condition(type, status, reason, message, observedGeneration, lastTransitionTime).
	FuncCondition = "condition"

	// FuncTransitionTime determines the correct lastTransitionTime for a
	// metav1.Condition-shaped entry. Given the *existing* list of conditions
	// for the target field (e.g. .status.conditions, or
	// .status.listeners[i].conditions, or .status.parents[i].conditions —
	// the helper is resource-agnostic, the caller navigates to the list),
	// the existing lastTransitionTime is preserved if a condition with the
	// same type AND status is already present; otherwise the current time
	// is returned. Pass nil when no prior conditions exist (e.g. the
	// resource was just created) — the helper will treat every condition
	// as new.
	//
	// Syntax: transitionTime(existingConditions, conditionType, newStatus).
	FuncTransitionTime = "transitionTime"

	// FuncCycleTimeBucket returns the current UTC time bucket in a caller-supplied layout.
	FuncCycleTimeBucket = "cycleTimeBucket"

	// FuncCycleRandomBytes returns random bytes owned by the current render attempt.
	FuncCycleRandomBytes = "cycleRandomBytes"

	// FilterToJSON serializes any value to a JSON string.
	// Syntax: toJSON(value) or value | toJSON().
	FilterToJSON = "toJSON"

	// Version comparison functions.

	// FuncSemverGte checks if a semver version is >= a minimum version.
	// Compares major.minor (patch ignored). Returns false for unparseable versions.
	// Syntax: semver_gte(version, minVersion) returns bool.
	FuncSemverGte = "semver_gte"

	// GUID functions.

	// FuncMakeGUID builds a HAProxy GUID from parts joined by ":".
	// Auto-truncates with a hash suffix if the result exceeds 127 characters.
	// Syntax: make_guid(parts...) returns string.
	FuncMakeGUID = "make_guid"

	// Generic governance helpers (resource-agnostic; used by the chart's
	// governance layer to inject/validate any watched resource by JSONPath).

	// FuncResource returns the per-render items of a watched resource named
	// dynamically, sharing the same objects as resources.<name>.List(), so a
	// jsonpathSet write is observed downstream. Syntax: resource(name) []any.
	FuncResource = "resource"

	// FuncJSONPathGet reads a concrete JSONPath out of a resource item.
	// Syntax: jsonpathGet(item, path) any.
	FuncJSONPathGet = "jsonpathGet"

	// FuncJSONPathSet writes a concrete JSONPath into a resource item in place.
	// Syntax: jsonpathSet(item, path, value) bool.
	FuncJSONPathSet = "jsonpathSet"

	// FuncDeriveResource publishes an immutable transformed resource value.
	// Syntax: deriveResource(resource, item, path, value) any.
	FuncDeriveResource = "deriveResource"

	// FuncIncrementalRender renders a keyed component snippet.
	FuncIncrementalRender = "incremental_render"

	// FuncIncrementalValues returns winning immutable values for a component group cell.
	FuncIncrementalValues = "incremental_values"

	// FuncIncrementalRankedFragments joins ranked string publications in rank order.
	FuncIncrementalRankedFragments = "incremental_ranked_fragments"

	// FuncIncrementalRankedFragmentsJoin joins ranked string publications with a delimiter.
	FuncIncrementalRankedFragmentsJoin = "incremental_ranked_fragments_join"

	// FuncIncrementalRankedTextFragment retains ranked publications in direct output.
	FuncIncrementalRankedTextFragment = "incremental_ranked_text_fragment"

	// FuncIncrementalRankedTextFragmentJoin retains delimiter-joined publications in direct output.
	FuncIncrementalRankedTextFragmentJoin = "incremental_ranked_text_fragment_join"

	// FuncIncrementalRankedFragmentBytes reports a ranked cell's joined length
	// without materialising it, so a template can test emptiness cheaply.
	FuncIncrementalRankedFragmentBytes = "incremental_ranked_fragment_bytes"

	// FuncIncrementalValueCount reports how many winning values a cell holds
	// without decoding them, so a presence test costs nothing per value.
	FuncIncrementalValueCount = "incremental_value_count"

	// Collection pipeline helpers (ADR-0018). Each preserves its input's
	// element type, so a chain over a typed watched resource keeps typed
	// field access at every stage. Predicates are closures rather than
	// JSONPath strings so field names are checked at engine compile time.
	//
	// FuncMap applies a function to every element, preserving length.
	// Syntax: items | map(func(e T) U { ... }) returns []U.
	// The name is available because the fork's parser resolves `map` as an
	// identifier when it is not followed by `[`.
	FuncMap = "map"

	// FuncFilter keeps the elements a predicate accepts.
	// Syntax: items | filter(func(e T) bool { ... }) returns []T.
	FuncFilter = "filter"

	// FuncReject drops the elements a predicate accepts, so a call site
	// reads as a positive statement instead of a negated one.
	// Syntax: items | reject(func(e T) bool { ... }) returns []T.
	FuncReject = "reject"

	// FuncFlatMap maps each element to a slice and concatenates the
	// results, flattening exactly one level (not recursively).
	// Syntax: items | flat_map(func(e T) []U { ... }) returns []U.
	FuncFlatMap = "flat_map"

	// FuncUnique keeps the first occurrence of each distinct element,
	// preserving input order.
	// Syntax: items | unique() returns []T.
	FuncUnique = "unique"

	// FuncUniqueBy keeps the first element per key, preserving input order.
	// Replaces the hand-rolled map[string]bool{} dedup sets.
	// Syntax: items | unique_by(func(e T) K { ... }) returns []T.
	FuncUniqueBy = "unique_by"

	// FuncGroupBy buckets elements by a string key, preserving input order
	// within each bucket. Iterate the result via keys() — Go map order is
	// not stable, and a reordered render reads as a change to the
	// controller.
	// Syntax: items | group_by(func(e T) string { ... }) returns map[string][]T.
	FuncGroupBy = "group_by"

	// FuncUntarGz expands a gzip-compressed tar archive into entry path →
	// entry content. Syntax: untar_gz(archive) (map[string]string, error).
	// Reports failure through the error return rather than panicking, so a
	// bad archive costs the caller a fallback and not the whole render.
	FuncUntarGz = "untar_gz"
)
