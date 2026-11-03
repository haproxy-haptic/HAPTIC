// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package templating

// Runtime template globals shared by the standard, incremental, and replay
// engine surfaces.
const (
	declItem          = "item"
	declShared        = "shared"
	declHTTP          = "http"
	declController    = "controller"
	declPlanRegistry  = "planRegistry"
	declResources     = "resources"
	declSource        = "source"
	declProps         = "props"
	declRenderSubject = "renderSubject"
	declRenderMode    = "renderMode"
	declCurrentConfig = "currentConfig"
	declCurrentFiles  = "currentFiles"
	declFileRegistry  = "fileRegistry"
	declPathResolver  = "pathResolver"
	declRegexp        = "regexp"
	declInput         = "input"
)

// Render-mode values carried by the renderMode global; see
// rendercontext.RenderMode.
const (
	renderModeReconcile = "reconcile"
	renderModeAdmission = "admission"
)

// Member names proved on runtime globals.
const (
	memberList       = "List"
	memberFetch      = "Fetch"
	memberPending    = "Pending"
	memberGetSingle  = "GetSingle"
	memberAPIVersion = "APIVersion"
	memberBackend    = "Backend"
	memberReplaceAll = "ReplaceAll"
)

// Scriggo's synthetic root package for template declarations.
const scriggoMainPackage = "main"

// Scriggo builtin declaration names re-registered by the incremental engine.
const (
	builtinAbbreviate        = "abbreviate"
	builtinAbs               = "abs"
	builtinBase64            = "base64"
	builtinCapitalize        = "capitalize"
	builtinCapitalizeAll     = "capitalizeAll"
	builtinCountBy           = "count_by"
	builtinDate              = "date"
	builtinFormatFloat       = "formatFloat"
	builtinFormatInt         = "formatInt"
	builtinHasPrefix         = "hasPrefix"
	builtinHasSuffix         = "hasSuffix"
	builtinHex               = "hex"
	builtinHmacSHA1          = "hmacSHA1"
	builtinHmacSHA256        = "hmacSHA256"
	builtinHTMLEscape        = "htmlEscape"
	builtinIndex             = "index"
	builtinIndexAny          = "indexAny"
	builtinIndexBy           = "index_by"
	builtinLastIndex         = "lastIndex"
	builtinMapExtract        = "map_extract"
	builtinMarshalJSON       = "marshalJSON"
	builtinMarshalJSONIndent = "marshalJSONIndent"
	builtinMarshalYAML       = "marshalYAML"
	builtinMax               = "max"
	builtinMD5               = "md5"
	builtinMin               = "min"
	builtinParseDuration     = "parseDuration"
	builtinParseFloat        = "parseFloat"
	builtinParseInt          = "parseInt"
	builtinParseTime         = "parseTime"
	builtinParseYAML         = "parse_yaml"
	builtinPow               = "pow"
	builtinPublicKeyInfo     = "public_key_info"
	builtinJSONArrayPattern  = "json_string_array_pattern"
	builtinQueryEscape       = "queryEscape"
	builtinReplaceAll        = "replaceAll"
	builtinRuneCount         = "runeCount"
	builtinSHA1              = "sha1"
	builtinSHA256            = "sha256"
	builtinSplit             = "split"
	builtinSplitAfter        = "splitAfter"
	builtinSplitAfterN       = "splitAfterN"
	builtinSplitN            = "splitN"
	builtinToKebab           = "toKebab"
	builtinToLower           = "toLower"
	builtinToUpper           = "toUpper"
	builtinTrim              = "trim"
	builtinTrimLeft          = "trimLeft"
	builtinTrimPrefix        = "trimPrefix"
	builtinTrimRight         = "trimRight"
	builtinTrimSpace         = "trimSpace"
	builtinTrimSuffix        = "trimSuffix"
	builtinUnixTime          = "unixTime"
)
