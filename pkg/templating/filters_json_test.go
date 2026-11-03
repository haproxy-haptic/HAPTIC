// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package templating

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseJSON(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        any
		invalid     bool
	}{
		{"object", `{"name":"first"}`, map[string]any{"name": "first"}, false},
		{"array", `["a",1,true,null]`, []any{"a", float64(1), true, nil}, false},
		{"null", `null`, nil, false},
		{"string", `"value"`, "value", false},
		{"empty", ``, nil, true},
		{"malformed", `{`, nil, true},
		{"extra document", `{} {}`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := scriggoParseJSON(tc.input)
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, value)
		})
	}
}

func TestParseJSONExactCycleReplay(t *testing.T) {
	engine, err := New(map[string]string{"test": `{% var value, err = parse_json("{") %}{% if err != nil %}{{ err.Error() }}{% else %}{{ value }}{% end %}`}, &Options{EntryPoints: []string{"test"}})
	require.NoError(t, err)
	_, err = engine.PrepareExactCycleReplay([]string{"test"})
	require.NoError(t, err)
}
