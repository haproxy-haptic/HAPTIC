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

package renderplan_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

func TestParseCRTListEntries(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []renderplan.CRTListEntry
		wantOK  bool
	}{
		{name: "empty", content: "", wantOK: true},
		{name: "comments and blank lines", content: "# header\n\n   \n  # indented\n", wantOK: true},
		{
			name:    "plain certificate",
			content: "default.pem\n",
			want:    []renderplan.CRTListEntry{{Cert: "default.pem"}},
			wantOK:  true,
		},
		{
			name:    "options in brackets",
			content: "a.pem [ocsp-update on ca-file ssl/ca.pem verify required alpn h2,http/1.1 no-ca-names]\n",
			want: []renderplan.CRTListEntry{{Cert: "a.pem", Options: []renderplan.KeywordArg{
				{Name: "ocsp-update", Args: []string{"on"}},
				{Name: "ca-file", Args: []string{"ssl/ca.pem"}},
				{Name: "verify", Args: []string{"required"}},
				{Name: "alpn", Args: []string{"h2,http/1.1"}},
				{Name: "no-ca-names"},
			}}},
			wantOK: true,
		},
		{
			name:    "SNI filters, negated and wildcard",
			content: "default.pem [ocsp-update on] !*\nb.pem *.example.com a.example.com !c.example.com\n",
			want: []renderplan.CRTListEntry{
				{
					Cert: "default.pem", Options: []renderplan.KeywordArg{{Name: "ocsp-update", Args: []string{"on"}}},
					SNIFilters: []string{"!*"},
				},
				{Cert: "b.pem", SNIFilters: []string{"*.example.com", "a.example.com", "!c.example.com"}},
			},
			wantOK: true,
		},
		{
			name:    "spaced brackets, tabs and CRLF",
			content: "a.pem\t[ alpn h2 ]  x.example.com\r\n",
			want: []renderplan.CRTListEntry{{
				Cert: "a.pem", Options: []renderplan.KeywordArg{{Name: "alpn", Args: []string{"h2"}}},
				SNIFilters: []string{"x.example.com"},
			}},
			wantOK: true,
		},
		{name: "empty brackets", content: "a.pem [] x.example.com\n",
			want: []renderplan.CRTListEntry{{Cert: "a.pem", SNIFilters: []string{"x.example.com"}}}, wantOK: true},
		{name: "unknown keyword", content: "a.pem [frobnicate on]\n"},
		{name: "keyword missing its argument", content: "a.pem [alpn]\n"},
		{name: "unclosed bracket", content: "a.pem [alpn h2 x.example.com\n"},
		{name: "bracket after SNI filters", content: "a.pem x.example.com [alpn h2]\n"},
		{name: "nested bracket", content: "a.pem [[alpn h2]]\n"},
		{name: "bracket in certificate", content: "a[1].pem\n"},
		{name: "quoted token", content: "a.pem \"x.example.com\"\n"},
		{name: "escaped token", content: "a\\ b.pem\n"},
		{name: "trailing comment", content: "a.pem x.example.com # note\n"},
		{name: "certificate listed twice", content: "a.pem x.example.com\na.pem y.example.com\n"},
		{name: "one bad line poisons the file", content: "a.pem\nb.pem [frobnicate]\nc.pem\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := renderplan.ParseCRTListEntries(tt.content)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
