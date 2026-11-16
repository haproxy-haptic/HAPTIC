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

package renderplan

import (
	"slices"
	"strings"
)

// crtListKeywordArity is the argument count of every sslbindconf keyword a
// crt-list line accepts (https://docs.haproxy.org/3.2/configuration.html#5.1-crt-list).
var crtListKeywordArity = map[string]int{
	"allow-0rtt": 0, "no-alpn": 0, "no-ca-names": 0,
	"alpn": 1, "ca-file": 1, "ca-verify-file": 1, "ciphers": 1, "ciphersuites": 1,
	"client-sigalgs": 1, "crl-file": 1, "curves": 1, "ecdhe": 1, "npn": 1,
	"sigalgs": 1, "ssl-min-ver": 1, "ssl-max-ver": 1, "verify": 1,
	"crt": 1, "key": 1, "ocsp": 1, "issuer": 1, "sctl": 1, "ocsp-update": 1,
}

// ParseCRTListEntries reads rendered crt-list content as
// `<cert> [[<sslbindconf> ...]] [[!]<snifilter> ...]` lines. It reports false,
// and no entries, for anything it cannot represent exactly: an unknown
// keyword, a stray bracket, quoting or escaping, or a certificate listed twice,
// since `del ssl crt-list` names an entry by its certificate alone.
func ParseCRTListEntries(content string) ([]CRTListEntry, bool) {
	var entries []CRTListEntry
	seen := make(map[string]struct{})
	for line := range strings.SplitSeq(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		entry, ok := parseCRTListLine(fields)
		if !ok {
			return nil, false
		}
		if _, duplicate := seen[entry.Cert]; duplicate {
			return nil, false
		}
		seen[entry.Cert] = struct{}{}
		entries = append(entries, entry)
	}
	return entries, true
}

func parseCRTListLine(fields []string) (CRTListEntry, bool) {
	if slices.ContainsFunc(fields, func(field string) bool { return strings.ContainsAny(field, `"'\#`) }) {
		return CRTListEntry{}, false
	}
	entry := CRTListEntry{Cert: fields[0]}
	rest := fields[1:]
	if len(rest) > 0 && strings.HasPrefix(rest[0], "[") {
		end := slices.IndexFunc(rest, func(field string) bool { return strings.HasSuffix(field, "]") })
		if end < 0 {
			return CRTListEntry{}, false
		}
		tokens := slices.Clone(rest[:end+1])
		tokens[0] = strings.TrimPrefix(tokens[0], "[")
		tokens[end] = strings.TrimSuffix(tokens[end], "]")
		options, ok := parseCRTListOptions(tokens)
		if !ok {
			return CRTListEntry{}, false
		}
		entry.Options = options
		rest = rest[end+1:]
	}
	if strings.ContainsAny(entry.Cert, "[]") || slices.ContainsFunc(rest, hasBracket) {
		return CRTListEntry{}, false
	}
	if len(rest) > 0 {
		entry.SNIFilters = slices.Clone(rest)
	}
	return entry, true
}

func parseCRTListOptions(tokens []string) ([]KeywordArg, bool) {
	tokens = slices.DeleteFunc(tokens, func(token string) bool { return token == "" })
	var options []KeywordArg
	for i := 0; i < len(tokens); {
		arity, known := crtListKeywordArity[tokens[i]]
		if !known || i+arity >= len(tokens) || slices.ContainsFunc(tokens[i:i+1+arity], hasBracket) {
			return nil, false
		}
		option := KeywordArg{Name: tokens[i]}
		if arity > 0 {
			option.Args = slices.Clone(tokens[i+1 : i+1+arity])
		}
		options = append(options, option)
		i += 1 + arity
	}
	return options, true
}

func hasBracket(token string) bool {
	return strings.ContainsAny(token, "[]")
}
