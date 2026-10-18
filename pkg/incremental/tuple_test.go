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

package incremental

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeTupleMatchesCanonicalLengthFraming(t *testing.T) {
	for length := range 1025 {
		value := make([]byte, length)
		for index := range value {
			value[index] = byte(index*131 + length*17)
		}
		element := string(value)
		require.Equal(t, testTupleEncoding("kind", element), EncodeTuple("kind", element))

		decoder := NewTupleDecoder(EncodeTuple("kind", element))
		head, ok := decoder.Next()
		require.True(t, ok)
		require.Equal(t, "kind", head)
		decoded, ok := decoder.Next()
		require.True(t, ok)
		require.Equal(t, element, decoded)
		require.True(t, decoder.Done())
	}

	require.Equal(t, testTupleEncoding("kind", "", ""), EncodeTuple("kind", "", ""))
}

func TestTupleDecoderRejectsNonCanonicalElements(t *testing.T) {
	for _, value := range []string{
		"",
		string([]byte{0x84, 0}) + "kind",
		string([]byte{4}) + "kin",
		"\x80",
		string([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 2}),
	} {
		decoder := NewTupleDecoder(value)
		_, ok := decoder.Next()
		require.False(t, ok, "%q", value)
	}
}

func TestDescribeTuple(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{
			name:  "component key",
			value: EncodeTuple("component", "features-960-governance", "ingresses", "x7-raw", "x7-raw"),
			want:  "component(features-960-governance, ingresses, x7-raw, x7-raw)",
		},
		{name: "head only", value: EncodeTuple("derive-owner"), want: "derive-owner()"},
		{
			name:  "elements that need quotes",
			value: EncodeTuple("resource", "", "a b", "x,y", "tab\there", string([]byte{0xff})),
			want:  `resource("", "a b", "x,y", "tab\there", "\xff")`,
		},
		{name: "long element", value: EncodeTuple("http", strings.Repeat("a", 200)), want: "http(" + strings.Repeat("a", 200) + ")"},
		{name: "not a tuple", value: "plain", want: `"plain"`},
		{name: "truncated element", value: EncodeTuple("kind", "value") + "\x05ab", want: fmt.Sprintf("%q", EncodeTuple("kind", "value")+"\x05ab")},
		{name: "head needs quotes", value: EncodeTuple("a b", "c"), want: fmt.Sprintf("%q", EncodeTuple("a b", "c"))},
		{name: "empty", value: "", want: `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DescribeTuple(tt.value)
			assert.Equal(t, tt.want, got)
			assert.False(t, strings.ContainsFunc(got, unicode.IsControl), "description %q carries a control character", got)
		})
	}
}

func TestKeysFormatAsTupleDescriptions(t *testing.T) {
	encoded := EncodeTuple("component", "policy", "routes", "default", "route")
	assert.Equal(t, "query component(policy, routes, default, route) failed",
		fmt.Errorf("query %s failed", NewQueryKey(encoded)).Error())
	assert.Equal(t, "component(policy, routes, default, route)", NewInputKey(encoded).String())
	assert.Equal(t, `""`, QueryKey{}.String())
}

func testTupleEncoding(elements ...string) string {
	encoded := make([]byte, 0)
	for _, element := range elements {
		encoded = binary.AppendUvarint(encoded, uint64(len(element)))
		encoded = append(encoded, element...)
	}
	return string(encoded)
}
