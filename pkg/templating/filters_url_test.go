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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseURLPreservesAuthorityAndEscaping(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		fields      map[string]any
	}{
		{"escaped path", "https://example.com:8443/a%2Fb?x=y#section", map[string]any{"host": "example.com:8443", "hostname": "example.com", "port": "8443", "path": "/a/b", "escapedPath": "/a%2Fb", "rawQuery": "x=y", "fragment": "section"}},
		{"empty query", "https://example.com?", map[string]any{"forceQuery": true, "rawQuery": ""}},
		{"empty user", "https://@example.com/", map[string]any{"hasUserInfo": true, "hostname": "example.com"}},
		{"ipv6", "https://[::1]:443/", map[string]any{"hostname": "::1", "port": "443"}},
		{"relative", "/resource", map[string]any{"scheme": "", "host": "", "path": "/resource", "hasUserInfo": false}},
		{"opaque", "urn:example:resource", map[string]any{"scheme": "urn", "opaque": "example:resource"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields, err := scriggoParseURL(tc.value)
			require.NoError(t, err)
			for name, want := range tc.fields {
				require.Equal(t, want, fields[name], name)
			}
		})
	}
	for _, value := range []string{"https://example.com/%zz", "https://example.com:invalid/", "https://example.com/\nheader"} {
		fields, err := scriggoParseURL(value)
		require.Error(t, err)
		require.Nil(t, fields)
	}
}

func TestParseURLAndJSONIncrementalEntryPoint(t *testing.T) {
	source := `{% var value, err = parse_json(item["document"].(string)) %}{% if err != nil %}invalid{% else %}{% var parsed, parseErr = parse_url(value.(map[string]any)["url"].(string)) %}{% if parseErr != nil %}invalid{% else %}{{ parsed["escapedPath"] }}{% end %}{% end %}`
	engine, err := New(map[string]string{"test": source}, &Options{EntryPoints: []string{"test"}, IncrementalEntryPoints: []string{"test"}})
	require.NoError(t, err)
	require.True(t, engine.compiledTemplates["test"].BatchSafe())
	require.NoError(t, engine.compiledTemplates["test"].DeterministicSafe())
	for _, tc := range []struct{ input, want string }{
		{`{"url":"https://example.com/a%2Fb"}`, "/a%2Fb"}, {`{`, "invalid"}, {`{"url":"https://example.com/%zz"}`, "invalid"},
	} {
		output, renderErr := engine.RenderIncrementalComponent(t.Context(), "test", incrementalComponentContext(map[string]any{"item": map[string]any{"document": tc.input}}))
		require.NoError(t, renderErr)
		require.Equal(t, tc.want, output)
	}
}

func TestParseURLExactCycleReplay(t *testing.T) {
	engine, err := New(map[string]string{"test": `{% var parsed, err = parse_url("https://example.com/%zz") %}{% if err != nil %}{{ err.Error() }}{% else %}{{ parsed["path"] }}{% end %}`}, &Options{EntryPoints: []string{"test"}})
	require.NoError(t, err)
	_, err = engine.PrepareExactCycleReplay([]string{"test"})
	require.NoError(t, err)
}
