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
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONStringArrayPattern(t *testing.T) {
	for _, value := range []string{"my-api", "a/b", `a"b`, `a\b`, "a,b", "a[b].*", "mý-api", "😀", "", "a b", "<>&", "a\nb\tc", "\x00\b\f\r", "a\u2028b"} {
		t.Run(value, func(t *testing.T) {
			pattern := regexp.MustCompile(scriggoJSONStringArrayPattern(value))
			for _, entries := range [][]string{{value}, {"other", value}, {value, "other"}, {"other", value, "last"}} {
				encoded, err := json.Marshal(entries)
				require.NoError(t, err)
				require.True(t, pattern.Match(encoded), string(encoded))
			}
			for _, entries := range [][]string{{"prefix" + value}, {value + "suffix"}, {"other"}} {
				encoded, err := json.Marshal(entries)
				require.NoError(t, err)
				require.False(t, pattern.Match(encoded), string(encoded))
			}
		})
	}
	pattern := regexp.MustCompile(scriggoJSONStringArrayPattern("my-api"))
	for _, input := range []string{`["\u006dy-api"]`, `["my\u002Dapi"]`, "[ \n \"other\", \"my-api\"\t ]"} {
		require.True(t, pattern.MatchString(input), input)
	}
	for _, input := range []string{`"my-api"`, `[]`, `["other"]`, `["my-api",false]`, `[false,"my-api"]`, `[["my-api"]]`, `["my-api",]`, `["other\",\"my-api"]`, `["my-api","bad\q"]`} {
		require.False(t, pattern.MatchString(input), input)
	}
	require.True(t, regexp.MustCompile(scriggoJSONStringArrayPattern("😀")).MatchString(`["\uD83D\uDE00"]`))
	require.True(t, regexp.MustCompile(scriggoJSONStringArrayPattern("a/b")).MatchString(`["a\/b"]`))
	require.True(t, regexp.MustCompile(scriggoJSONStringArrayPattern("<>&")).MatchString(`["<>&"]`))
	require.NotContains(t, scriggoJSONStringArrayPattern("a b"), " ", "map regular expressions cannot contain literal whitespace")
	require.False(t, pattern.MatchString(`["`+strings.Repeat("x", 10000)+`"]`))
}

func TestJSONStringArrayPatternIncremental(t *testing.T) {
	source := `{{ json_string_array_pattern(item["value"].(string)) }}`
	engine, err := New(map[string]string{"test": source}, &Options{EntryPoints: []string{"test"}, IncrementalEntryPoints: []string{"test"}})
	require.NoError(t, err)
	require.True(t, engine.compiledTemplates["test"].BatchSafe())
	require.NoError(t, engine.compiledTemplates["test"].DeterministicSafe())
	output, err := engine.RenderIncrementalComponent(t.Context(), "test", incrementalComponentContext(map[string]any{"item": map[string]any{"value": "api"}}))
	require.NoError(t, err)
	require.Equal(t, scriggoJSONStringArrayPattern("api"), output)
	replay, err := New(map[string]string{"test": `{{ json_string_array_pattern("api") }}`}, &Options{EntryPoints: []string{"test"}})
	require.NoError(t, err)
	_, err = replay.PrepareExactCycleReplay([]string{"test"})
	require.NoError(t, err)
}
