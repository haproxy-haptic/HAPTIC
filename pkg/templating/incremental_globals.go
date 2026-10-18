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

import "github.com/haproxy-haptic/scriggo/native"

var incrementalDeclarationNames = [...]string{
	declItem,
	declShared,
	declHTTP,
	declController,
	declPlanRegistry,
	FilterSortBy,
	FilterGlobMatch,
	FilterStrip,
	builtinTrim,
	FilterB64Decode,
	FilterB64Encode,
	FilterIndent,
	FuncFail,
	FuncMerge,
	FuncKeys,
	FuncSortStrings,
	FuncSortInts,
	FuncStringsContains,
	FuncStringsSplit,
	FuncStringsTrim,
	FuncStringsLower,
	FuncStringsSplitN,
	FuncToString,
	FuncToInt,
	FuncToFloat,
	FuncCeil,
	FuncSeq,
	FuncRegexSearch,
	FuncIsDigit,
	FuncSanitizeRegex,
	FuncTitle,
	funcOnOwnLinesStart,
	funcOnOwnLinesJoin,
	FuncIsNil,
	FuncDig,
	FuncDigString,
	FuncToStringSlice,
	FuncJoin,
	FuncReplace,
	FuncCoalesce,
	FuncFallback,
	FuncNamespace,
	FuncToSlice,
	FuncToStrMap,
	FuncSelectAttr,
	FuncJoinKey,
	FuncShardSlice,
	FuncBasename,
	FuncCondition,
	FuncTransitionTime,
	FilterToJSON,
	FuncSemverGte,
	FuncMakeGUID,
	FuncJSONPathGet,
	FuncDeriveResource,
	FuncRecordEvent,
	FuncStatusPatch,
	FuncMap,
	FuncFilter,
	FuncReject,
	FuncFlatMap,
	FuncUnique,
	FuncUniqueBy,
	FuncGroupBy,
	FuncUntarGz,
	builtinHmacSHA1,
	builtinHmacSHA256,
	builtinSHA1,
	builtinSHA256,
	builtinPublicKeyInfo,
	builtinParseYAML,
	builtinBase64,
	builtinHex,
	builtinMarshalJSON,
	builtinMarshalJSONIndent,
	builtinMarshalYAML,
	builtinMD5,
	builtinHTMLEscape,
	builtinAbs,
	builtinMax,
	builtinMin,
	builtinPow,
	builtinQueryEscape,
	builtinCountBy,
	builtinIndexBy,
	builtinMapExtract,
	builtinFormatFloat,
	builtinFormatInt,
	builtinParseFloat,
	builtinParseInt,
	builtinDate,
	builtinParseDuration,
	builtinParseTime,
	builtinUnixTime,
	builtinTrimSpace,
	builtinAbbreviate,
	builtinCapitalize,
	builtinCapitalizeAll,
	builtinHasPrefix,
	builtinHasSuffix,
	builtinIndex,
	builtinIndexAny,
	builtinLastIndex,
	builtinReplaceAll,
	builtinRuneCount,
	builtinSplit,
	builtinSplitAfter,
	builtinSplitAfterN,
	builtinSplitN,
	builtinToKebab,
	builtinToLower,
	builtinToUpper,
	builtinTrimLeft,
	builtinTrimPrefix,
	builtinTrimRight,
	builtinTrimSuffix,
}

func buildScriggoIncrementalGlobals(
	additionalDeclarations map[string]any,
	_ func() bool,
) native.Declarations {
	standard := buildScriggoGlobals(nil, nil, nil)
	decl := make(native.Declarations, len(incrementalDeclarationNames)+4)
	for _, name := range &incrementalDeclarationNames {
		value, ok := standard[name]
		if !ok {
			panic("templating: unknown incremental declaration " + name)
		}
		decl[name] = value
	}
	decl[declShared] = native.Synchronous(
		(*SharedContributionContext)(nil),
		"Unique", "Publish", "PublishRanked", "Select", "SelectValues", "Count",
	)
	decl[declHTTP] = native.Synchronous((*HTTPFetcher)(nil), memberFetch, memberPending)
	decl[declController] = native.Synchronous(
		(*map[string]ResourceStore)(nil),
		"*.List", "*.Fetch", "*.GetSingle",
	)
	decl[declSource] = (*string)(nil)
	decl[declProps] = (*map[string]any)(nil)
	decl[declRenderSubject] = (*map[string]any)(nil)
	decl[declRenderMode] = (*string)(nil)
	decl[declPlanRegistry] = native.Synchronous(
		(*IncrementalBackendPlanRegistrar)(nil),
		"Profile", "Profile[1].Error",
		memberBackend, "Backend[1].Error",
		"BackendWhenAny", "BackendWhenAny[1].Error",
	)
	registerIncrementalDeterministicDeclarations(decl)
	certifySynchronousDeclarations(decl)
	if resources, ok := additionalDeclarations[declResources]; ok {
		incrementalResources, leaseBound := incrementalResourcesDeclaration(resources)
		decl[declResources] = incrementalResources
		if leaseBound {
			decl[declResources] = native.Synchronous(
				incrementalResources,
				"*.List", "*.Fetch", "*.GetSingle", "*.APIVersion",
			)
		}
	}
	return decl
}

func registerIncrementalDeterministicDeclarations(decl native.Declarations) {
	registerIncrementalStringDeclarations(decl)
	registerIncrementalCollectionDeclarations(decl)
	registerIncrementalEncodingDeclarations(decl)
}

func registerIncrementalStringDeclarations(decl native.Declarations) {
	decl[FilterGlobMatch] = incrementalGlobMatch
	decl[FilterStrip] = incrementalStrip
	decl[FilterB64Decode] = incrementalB64Decode
	decl[FilterB64Encode] = incrementalB64Encode
	decl[FilterIndent] = incrementalIndent
	decl[FuncStringsContains] = incrementalStringsContains
	decl[FuncStringsSplit] = incrementalStringsSplit
	decl[FuncStringsTrim] = incrementalStringsTrim
	decl[FuncStringsLower] = incrementalStringsLower
	decl[FuncStringsSplitN] = incrementalStringsSplitN
	decl[FuncToString] = incrementalToString
	decl[FuncToInt] = incrementalToInt
	decl[FuncToFloat] = incrementalToFloat
	decl[FuncCeil] = incrementalCeil
	decl[FuncRegexSearch] = newIncrementalRegexSearch()
	decl[FuncIsDigit] = incrementalIsDigit
	decl[FuncSanitizeRegex] = incrementalSanitizeRegex
	decl[FuncTitle] = incrementalTitle
	decl[FuncDig] = incrementalDig
	decl[FuncDigString] = incrementalDigString
	decl[FuncJoin] = incrementalJoin
	decl[FuncReplace] = incrementalReplace
	decl[FuncJoinKey] = incrementalJoinKey
	decl[FuncSemverGte] = incrementalSemverGte
	decl[FuncMakeGUID] = incrementalMakeGUID
	decl[FuncJSONPathGet] = incrementalJSONPathGet
	decl[builtinTrimSpace] = incrementalStringsTrim
}

func registerIncrementalCollectionDeclarations(decl native.Declarations) {
	decl[FilterSortBy] = incrementalSortByAdaptive()
	decl[FuncKeys] = incrementalKeys
	decl[FuncSortStrings] = incrementalSortStrings
	decl[FuncSortInts] = incrementalSortInts
	decl[FuncToStringSlice] = incrementalToStringSlice
	decl[FuncToStrMap] = incrementalToStrMap
	decl[FuncSelectAttr] = incrementalSelectAttr
	decl[FuncUnique] = incrementalUniqueAdaptive
	decl[FuncUniqueBy] = incrementalUniqueByAdaptive
	decl[FuncGroupBy] = incrementalGroupByAdaptive
	decl[builtinCountBy] = incrementalCountBy
	decl[builtinIndexBy] = incrementalIndexBy
	decl[builtinMapExtract] = incrementalMapExtract
}

func registerIncrementalEncodingDeclarations(decl native.Declarations) {
	decl[FuncCondition] = incrementalCondition
	decl[FuncTransitionTime] = incrementalTransitionTime
	decl[FilterToJSON] = incrementalToJSON
	decl[builtinMarshalJSON] = incrementalMarshalJSON
	decl[builtinMarshalJSONIndent] = incrementalMarshalJSONIndent
	decl[builtinMarshalYAML] = incrementalMarshalYAML
	decl[builtinFormatFloat] = incrementalFormatFloat
	decl[builtinPow] = incrementalPow
	decl[builtinDate] = incrementalDate
	decl[builtinParseTime] = incrementalParseTime
	decl[builtinUnixTime] = incrementalUnixTime
	decl[FuncUntarGz] = scriggoUntarGz
	decl[FuncDeriveResource] = incrementalDeriveResource
	decl[FuncRecordEvent] = incrementalRecordEvent
	decl[FuncStatusPatch] = incrementalStatusPatch
}
