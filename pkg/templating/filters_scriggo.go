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
	"crypto/rand"
	"reflect"

	"github.com/haproxy-haptic/scriggo/builtin"
	"github.com/haproxy-haptic/scriggo/native"
)

// scriggoRandBytes returns n cryptographically-secure random bytes as a string
// (raw bytes; pipe through base64/hex to encode). Backed by crypto/rand.
//
// NON-DETERMINISTIC by design: every call returns fresh bytes, so a template
// that calls it produces a different render each time. Callers MUST guard usage
// so steady-state renders stay byte-identical — e.g. the TLS session-ticket-key
// template only calls this when a rotation is actually due (the current file's
// embedded date marker is older than today), and otherwise re-emits the current
// file unchanged. Unguarded use breaks HAPTIC's diff-based reload decisions.
func scriggoRandBytes(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read does not fail on supported platforms; return empty
		// so template validation (e.g. haproxy_valid on a wrong-length key)
		// surfaces the problem rather than emitting weak key material.
		return ""
	}
	return string(buf)
}

// buildScriggoGlobals creates the global declarations for Scriggo templates.
// In Scriggo, filters become regular functions that templates can call.
//
// Example usage in Scriggo template:
//
//	{% for item := range sort_by(items, []string{"$.name"}) %}
//	{{ strip(item.value) }}
//	{% end %}
//
// The additionalDeclarations parameter allows callers to inject domain-specific type
// declarations without the templating package needing to know about those types.
// This maintains clean architecture by keeping templating independent of domain packages.
//
// Example:
//
//	additionalDecls := map[string]any{
//	    "currentConfig": (*renderplan.CurrentConfig)(nil),
//	}
//	globals := buildScriggoGlobals(filters, funcs, additionalDecls)
func buildScriggoGlobals(customFilters map[string]FilterFunc, customFunctions map[string]GlobalFunc, additionalDeclarations map[string]any) native.Declarations {
	decl := native.Declarations{}

	// Register runtime context variables, custom functions, and builtins
	registerScriggoRuntimeVars(decl)
	registerScriggoCustomFunctions(decl)
	registerScriggoBuiltins(decl)
	certifySynchronousDeclarations(decl)

	// Register any custom filters passed in (wrapped for Scriggo)
	for name, filter := range customFilters {
		decl[name] = wrapFilterForScriggo(filter)
	}

	// Register any custom functions (except fail, which we handle specially)
	for name, fn := range customFunctions {
		if name == FuncFail {
			// Skip - we use scriggoFail instead of wrapped FailFunction
			continue
		}
		decl[name] = wrapFunctionForScriggo(fn)
	}

	// Register additional declarations from caller (domain-specific types)
	// These are typically nil pointers for Scriggo type registration
	for name, value := range additionalDeclarations {
		decl[name] = value
	}

	return decl
}

// registerScriggoRuntimeVars registers runtime context variables.
// These are declared with nil pointers so Scriggo knows the TYPE at compile time,
// but the VALUE is provided at runtime via template.Run(vars).
func registerScriggoRuntimeVars(decl native.Declarations) {
	decl[declPathResolver] = native.Synchronous((*PathResolver)(nil), "GetBaseDir", "GetPath", "GetPath[1].Error")
	// `resources` is deliberately NOT declared here. Every engine
	// consumer in this codebase goes through
	// helpers.BuildAdditionalDeclarations + typebootstrap, which
	// declares the typed `*<struct>` shape. There is no untyped
	// fallback — the previous dual-shape (`(*map[string]ResourceStore)(nil)`
	// as a default) created drift between the engine's static
	// declaration and the runtime value (rendercontext built one
	// shape, the engine declared another, Scriggo's variable
	// binding tripped). Callers that bypass the typed-engine path
	// (unit tests) must supply their own `resources` declaration
	// via additionalDeclarations OR avoid templates that touch
	// `resources` at all.
	decl[declController] = (*map[string]ResourceStore)(nil)
	decl["templateSnippets"] = (*[]string)(nil)
	decl[declFileRegistry] = (*FileRegistrar)(nil)
	decl[declPlanRegistry] = (*PlanRegistrar)(nil)
	decl["dataplane"] = (*map[string]any)(nil)
	decl["capabilities"] = (*map[string]any)(nil)
	decl[declShared] = (*SharedContext)(nil)
	decl["extraContext"] = (*map[string]any)(nil)
	decl[declHTTP] = (*HTTPFetcher)(nil)                    // HTTP store for fetching remote content
	decl["runtimeEnvironment"] = (*RuntimeEnvironment)(nil) // Runtime environment info (GOMAXPROCS, etc.)
	decl[declRenderMode] = (*string)(nil)                   // "reconcile" (warn) | "admission" (fail); see rendercontext.RenderMode
	decl["admissionSubject"] = (*map[string]any)(nil)       // Store aliases and identity of the resource under admission review; empty map otherwise
	decl[ResourceDeriverContextName] = (*ResourceDeriver)(nil)
	decl[declItem] = (*map[string]any)(nil)
	// Note: Domain-specific types like currentConfig are registered via additionalDeclarations
	// parameter in buildScriggoGlobals() to maintain clean architecture boundaries
}

// registerScriggoCustomFunctions registers all custom functions for Scriggo templates.
func registerScriggoCustomFunctions(decl native.Declarations) {
	// Custom filters as functions
	// Non-debug default; the engine constructor replaces it with a variant
	// that reads the filter-debug flag. Both are AdaptiveFuncs so the call
	// shapes and the static return type are identical either way.
	decl[FilterSortBy] = sortByAdaptive(func() bool { return false })
	decl[FilterGlobMatch] = scriggoGlobMatch
	decl[FilterStrip] = scriggoStrip
	decl[FilterB64Decode] = scriggoB64Decode
	decl[FilterB64Encode] = scriggoB64Encode
	decl[FilterDebug] = scriggoDebug

	// Scriggo-specific fail function (uses native.Env)
	decl[FuncFail] = scriggoFail

	// Dict utility functions
	decl[FuncMerge] = scriggoMerge
	decl[FuncKeys] = scriggoKeys

	// Sorting functions
	decl[FuncSortStrings] = scriggoSortStrings
	decl[FuncSortInts] = scriggoSortInts

	// Deduplication and filtering functions
	decl[FuncFirstSeen] = scriggoFirstSeen
	decl[FuncSelectAttr] = scriggoSelectAttr
	decl[FuncJoinKey] = scriggoJoinKey

	// String manipulation functions (custom implementations)
	decl[FuncStringsContains] = scriggoStringsContains
	decl[FuncStringsSplit] = scriggoStringsSplit
	decl[FuncStringsTrim] = scriggoStringsTrim
	decl[FuncStringsLower] = scriggoStringsLower
	decl[FuncStringsSplitN] = scriggoStringsSplitN
	decl[FilterIndent] = scriggoIndent

	// Type conversion functions
	decl[FuncToString] = scriggoToString
	decl[FuncToInt] = scriggoToInt
	decl[FuncToFloat] = scriggoToFloat

	// Utility functions
	decl[FuncCeil] = scriggoCeil
	decl[FuncSeq] = scriggoSeq
	decl[FuncRegexSearch] = newScriggoRegexSearch()
	decl[FuncIsDigit] = scriggoIsDigit
	decl[FuncSanitizeRegex] = scriggoSanitizeRegex
	decl[FuncTitle] = scriggoTitle
	decl[funcOnOwnLinesStart] = scriggoOnOwnLinesStart
	decl[funcOnOwnLinesJoin] = scriggoOnOwnLinesJoin
	decl[FuncDig] = scriggoDig
	decl[FuncDigString] = scriggoDigString
	decl[FuncIsNil] = scriggoIsNil
	decl[FuncToStringSlice] = scriggoToStringSlice
	decl[FuncCidrPartition] = scriggoCidrPartition
	decl[FuncJoin] = scriggoJoin
	decl[FuncReplace] = scriggoReplace

	// Namespace function for mutable state patterns
	decl[FuncNamespace] = scriggoNamespace
	decl[FuncCoalesce] = scriggoCoalesce
	decl[FuncFallback] = scriggoCoalesce // Jinja2-style alias

	// Slice manipulation functions
	decl[FuncToSlice] = scriggoToSlice
	decl[FuncToStrMap] = scriggoToStrMap
	// append is declared as an AdaptiveFunc so a typed `[]T` slice
	// round-trips through `append(slice, item)` as `[]T` rather than
	// being widened to `[]any`. Lets chart code build typed slices
	// from typed-access loops (e.g., `hosts []string` from
	// `ingress.Spec.Rules[i].Host`) and pass them to typed-param
	// macros without an intermediate conversion.
	// shard_slice is declared as an AdaptiveFunc so the call's static
	// return type preserves the input slice's element type — enabling
	// typed loop variables (and typed field access) on the resulting
	// shard, instead of degrading every consumer to []any-with-dig().
	decl[FuncShardSlice] = scriggoShardSliceAdaptive

	registerScriggoPipelineFunctions(decl)

	// Path utility functions
	decl[FuncBasename] = scriggoBasename

	// Status-patch and Kubernetes-Event functions
	registerStatusAndEventFunctions(decl)

	// Version comparison functions
	decl[FuncSemverGte] = scriggoSemverGte

	// GUID functions
	decl[FuncMakeGUID] = scriggoMakeGUID
}

// registerStatusAndEventFunctions registers the resource-agnostic status-patch
// helpers (statusPatch/condition/transitionTime/toJSON) and the recordEvent
// Kubernetes-Event function. Split out of registerScriggoCustomFunctions to
// keep that function within the statement-count limit.
// registerScriggoPipelineFunctions registers the collection pipeline helpers.
// All are AdaptiveFuncs, so a chain over a typed watched resource keeps its
// element type at every stage instead of degrading to []any (ADR-0018).
//
// group_by and unique_by deliberately supersede scriggo/builtin's versions of
// the same names: these accept a key closure as well as an attribute path, and
// they preserve the input's element type. registerScriggoBuiltins must not
// re-register them — it runs last and would shadow these.
//
// `map` is declarable at all because the fork's parser resolves it as an
// identifier when the next token is not `[`. A map type always requires one,
// so that position was previously an unconditional syntax error.
func registerScriggoPipelineFunctions(decl native.Declarations) {
	decl[FuncMap] = scriggoMapAdaptive
	decl[FuncFilter] = scriggoFilterAdaptive
	decl[FuncReject] = scriggoRejectAdaptive
	decl[FuncFlatMap] = scriggoFlatMapAdaptive
	decl[FuncUnique] = scriggoUniqueAdaptive
	decl[FuncUniqueBy] = scriggoUniqueByAdaptive
	decl[FuncGroupBy] = scriggoGroupByAdaptive
}

func registerStatusAndEventFunctions(decl native.Declarations) {
	decl[FuncStatusPatch] = scriggoStatusPatch
	decl[FuncCondition] = scriggoCondition
	decl[FuncTransitionTime] = scriggoTransitionTime
	decl[FuncCycleTimeBucket] = scriggoCycleTimeBucket
	decl[FuncCycleRandomBytes] = scriggoCycleRandomBytes
	decl[FilterToJSON] = scriggoToJSON
	decl[FuncRecordEvent] = scriggoRecordEvent
	decl[FuncResource] = scriggoResource
	decl[FuncJSONPathGet] = scriggoJSONPathGet
	decl[FuncJSONPathSet] = scriggoJSONPathSet
	decl[FuncDeriveResource] = scriggoDeriveResource
	decl[FuncIncrementalRender] = native.Synchronous(scriggoIncrementalRender)
	decl[FuncIncrementalValues] = native.Synchronous(scriggoIncrementalValues)
	decl[FuncIncrementalRankedFragments] = native.Synchronous(scriggoIncrementalRankedFragments)
	decl[FuncIncrementalRankedFragmentsJoin] = native.Synchronous(scriggoIncrementalRankedFragmentsJoin)
	decl[FuncIncrementalRankedTextFragment] = native.Synchronous(scriggoIncrementalRankedTextFragment)
	decl[FuncIncrementalRankedTextFragmentJoin] = native.Synchronous(scriggoIncrementalRankedTextFragmentJoin)
	decl[FuncIncrementalRankedFragmentBytes] = native.Synchronous(scriggoIncrementalRankedFragmentBytes)
	decl[FuncIncrementalValueCount] = native.Synchronous(scriggoIncrementalValueCount)
	decl[FuncUntarGz] = scriggoUntarGz
}

// registerScriggoBuiltins registers all Scriggo builtin functions.
func registerScriggoBuiltins(decl native.Declarations) {
	registerScriggoBuiltinCore(decl)
	registerScriggoBuiltinStrings(decl)
}

// registerScriggoBuiltinCore registers core Scriggo builtins (crypto, encoding, math, etc.).
func registerScriggoBuiltinCore(decl native.Declarations) {
	// crypto
	decl[builtinHmacSHA1] = builtin.HmacSHA1
	decl[builtinHmacSHA256] = builtin.HmacSHA256
	decl[builtinSHA1] = builtin.Sha1
	decl[builtinSHA256] = builtin.Sha256
	decl["randBytes"] = scriggoRandBytes
	decl[builtinPublicKeyInfo] = scriggoPublicKeyInfo
	decl[builtinParseYAML] = scriggoParseYAML

	// encoding
	decl[builtinBase64] = builtin.Base64
	decl[builtinHex] = builtin.Hex
	decl[builtinMarshalJSON] = builtin.MarshalJSON
	decl[builtinMarshalJSONIndent] = builtin.MarshalJSONIndent
	decl[builtinMarshalYAML] = builtin.MarshalYAML
	decl[builtinMD5] = builtin.Md5
	decl["unmarshalJSON"] = scriggoUnmarshalJSON
	decl["unmarshalYAML"] = scriggoUnmarshalYAML

	// html
	decl[builtinHTMLEscape] = builtin.HtmlEscape

	// math
	decl[builtinAbs] = builtin.Abs
	decl[builtinMax] = builtin.Max
	decl[builtinMin] = builtin.Min
	decl[builtinPow] = builtin.Pow

	// net
	decl[builtinQueryEscape] = builtin.QueryEscape

	// regexp
	decl[declRegexp] = builtin.RegExp

	// sort
	decl["reverse"] = scriggoReverse

	// slice operations (batch processing for performance)
	// group_by and unique_by are deliberately absent here: the pipeline
	// helpers supersede builtin.GroupBy/UniqueBy with overloads that accept
	// either an attribute path or a key closure, and that preserve the
	// input's element type instead of widening to []any. Registering the
	// builtins here would shadow them — this block runs last.
	decl[builtinCountBy] = builtin.CountBy
	decl[builtinIndexBy] = builtin.IndexBy
	decl[builtinMapExtract] = builtin.MapExtract

	// strconv
	decl[builtinFormatFloat] = builtin.FormatFloat
	decl[builtinFormatInt] = builtin.FormatInt
	decl[builtinParseFloat] = builtin.ParseFloat
	decl[builtinParseInt] = builtin.ParseInt

	// time
	decl[builtinDate] = builtin.Date
	decl["now"] = builtin.Now
	decl[builtinParseDuration] = builtin.ParseDuration
	decl[builtinParseTime] = builtin.ParseTime
	decl[builtinUnixTime] = builtin.UnixTime
}

// registerScriggoBuiltinStrings registers string-related Scriggo builtins.
func registerScriggoBuiltinStrings(decl native.Declarations) {
	decl[builtinTrimSpace] = scriggoTrimSpace
	decl["strings_contains"] = scriggoStringsContains
	decl[builtinAbbreviate] = builtin.Abbreviate
	decl[builtinCapitalize] = builtin.Capitalize
	decl[builtinCapitalizeAll] = builtin.CapitalizeAll
	decl[builtinHasPrefix] = builtin.HasPrefix
	decl[builtinHasSuffix] = builtin.HasSuffix
	decl[builtinIndex] = builtin.Index
	decl[builtinIndexAny] = builtin.IndexAny
	decl["join"] = scriggoJoin // Override builtin to support []any from append()
	decl[builtinLastIndex] = builtin.LastIndex
	decl["replace"] = scriggoReplace // Override builtin to support 3-arg syntax (replaces all)
	decl[builtinReplaceAll] = builtin.ReplaceAll
	decl[builtinRuneCount] = builtin.RuneCount
	decl[builtinSplit] = builtin.Split
	decl[builtinSplitAfter] = builtin.SplitAfter
	decl[builtinSplitAfterN] = builtin.SplitAfterN
	decl[builtinSplitN] = builtin.SplitN
	decl["sprint"] = builtin.Sprint
	decl["sprintf"] = builtin.Sprintf
	decl[builtinToKebab] = builtin.ToKebab
	decl[builtinToLower] = builtin.ToLower
	decl[builtinToUpper] = builtin.ToUpper
	decl[builtinTrim] = builtin.Trim
	decl[builtinTrimLeft] = builtin.TrimLeft
	decl[builtinTrimPrefix] = builtin.TrimPrefix
	decl[builtinTrimRight] = builtin.TrimRight
	decl[builtinTrimSuffix] = builtin.TrimSuffix
}

// wrapFilterForScriggo wraps a FilterFunc to be callable from Scriggo templates.
// FilterFunc signature: func(in any, args ...any) (any, error)
// Scriggo needs a concrete function signature, so we wrap it.
func wrapFilterForScriggo(filter FilterFunc) func(native.Env, any, ...any) (any, error) {
	return func(env native.Env, in any, args ...any) (any, error) {
		values := make([]any, 1, len(args)+1)
		values[0] = in
		values = append(values, args...)
		if err := immutableNativeInputError(env, values...); err != nil {
			env.Stop(err)
			return nil, err
		}
		return filter(in, args...)
	}
}

// wrapFunctionForScriggo wraps a GlobalFunc to be callable from Scriggo templates.
func wrapFunctionForScriggo(fn GlobalFunc) func(native.Env, ...any) (any, error) {
	return func(env native.Env, args ...any) (any, error) {
		if err := immutableNativeInputError(env, args...); err != nil {
			env.Stop(err)
			return nil, err
		}
		return fn(args...)
	}
}

func scriggoReverse(env native.Env, slice any) {
	rv := reflect.ValueOf(slice)
	if rv.IsValid() && rv.Kind() == reflect.Slice && rv.Len() > 1 {
		if err := immutableNativeMutationError(env, slice); err != nil {
			env.Stop(err)
			return
		}
	}
	builtin.Reverse(slice)
}

func scriggoUnmarshalJSON(env native.Env, data string, target any) error {
	return guardedNativeUnmarshal(env, data, target, builtin.UnmarshalJSON)
}

func scriggoUnmarshalYAML(env native.Env, data string, target any) error {
	return guardedNativeUnmarshal(env, data, target, builtin.UnmarshalYAML)
}

func guardedNativeUnmarshal(
	env native.Env,
	data string,
	target any,
	unmarshal func(string, any) error,
) error {
	mutationErr := immutableNativeMutationError(env, target)
	if mutationErr == nil {
		return unmarshal(data, target)
	}
	rv := reflect.ValueOf(target)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() {
		return unmarshal(data, target)
	}
	if err := unmarshal(data, reflect.New(rv.Type().Elem()).Interface()); err != nil {
		return err
	}
	env.Stop(mutationErr)
	return mutationErr
}
