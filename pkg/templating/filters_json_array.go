// Copyright 2026 Philipp Hossner
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
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"
)

func scriggoJSONStringArrayPattern(value string) string {
	const whitespace = `[\x20\t\r\n]*`
	const element = `"([^"\\\x00-\x1f]|\\(["\\/bfnrt]|u[0-9a-fA-F]{4}))*"`
	var target strings.Builder
	target.WriteByte('"')
	for _, character := range value {
		target.WriteString(jsonCharacterPattern(character))
	}
	target.WriteByte('"')
	return `^` + whitespace + `\[` + whitespace + `(` + element + whitespace + `,` + whitespace + `)*` +
		target.String() + `(` + whitespace + `,` + whitespace + element + `)*` + whitespace + `\]` + whitespace + `$`
}

func jsonCharacterPattern(character rune) string {
	var escaped strings.Builder
	for _, code := range utf16.Encode([]rune{character}) {
		escaped.WriteString(`\\u`)
		for _, digit := range fmt.Sprintf("%04x", code) {
			if digit >= 'a' && digit <= 'f' {
				escaped.WriteString("[" + string(digit) + strings.ToUpper(string(digit)) + "]")
			} else {
				escaped.WriteRune(digit)
			}
		}
	}
	literal := jsonCharacterLiteral(character)
	if literal == "" {
		return escaped.String()
	}
	return `(` + literal + `|` + escaped.String() + `)`
}

func jsonCharacterLiteral(character rune) string {
	switch character {
	case ' ':
		return `\x20`
	case '/':
		return `/|\\/`
	case '"', '\\':
		return regexp.QuoteMeta(`\` + string(character))
	}
	if index := strings.IndexRune("\b\f\n\r\t", character); index >= 0 {
		return regexp.QuoteMeta(`\` + string("bfnrt"[index]))
	}
	if character < 0x20 {
		return ""
	}
	return regexp.QuoteMeta(string(character))
}
