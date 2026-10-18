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

var synchronousDeclarationNames = [...]string{
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

var synchronousDeclarationMembers = map[string][]string{
	builtinParseDuration: {"[0].Milliseconds"},
	builtinParseTime:     {"UnixNano"},
	builtinParseYAML:     {"[1].Error"},
}

func certifySynchronousDeclarations(decl native.Declarations) {
	for _, name := range &synchronousDeclarationNames {
		value, ok := decl[name]
		if !ok {
			panic("templating: unknown synchronous declaration " + name)
		}
		if _, certified := value.(native.SynchronousDeclaration); certified {
			continue
		}
		decl[name] = native.Synchronous(value, synchronousDeclarationMembers[name]...)
	}
}
