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

package renderer

import (
	"encoding/binary"
	"testing"

	iradix "github.com/hashicorp/go-immutable-radix/v2"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

func TestDecodeOpaqueMatchesCanonicalLengthFraming(t *testing.T) {
	for length := range 1025 {
		value := make([]byte, length)
		for index := range value {
			value[index] = byte(index*131 + length*17)
		}
		var decoded [1]string
		require.True(t, decodeOpaque(testOpaqueEncoding("kind", string(value)), "kind", decoded[:]))
		require.Equal(t, string(value), decoded[0])
	}

	var empty [2]string
	require.True(t, decodeOpaque(testOpaqueEncoding("kind", "", ""), "kind", empty[:]))
	require.Equal(t, [2]string{}, empty)

	for _, value := range []string{
		"",
		testOpaqueEncoding("other", "value"),
		string([]byte{0x84, 0}) + "kind" + testOpaqueEncoding("value"),
		string([]byte{4}) + "kin",
		testOpaqueEncoding("kind", "value") + "\x00",
		"\x80",
		string([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 2}),
	} {
		var decoded [1]string
		require.False(t, decodeOpaque(value, "kind", decoded[:]), "%q", value)
	}
}

func TestOpaquePrefixesSelectOnlyExactFramedParts(t *testing.T) {
	component := incrementalComponent{group: "group\x00one", name: "component"}
	txn := iradix.New[struct{}]().Txn()
	txn.Insert(incrementalActiveGroupInstanceKey(&component, "routes", "default", "one"), struct{}{})
	txn.Insert(incrementalActiveGroupInstanceKey(
		&incrementalComponent{group: "group\x00two", name: "component"},
		"routes", "default", "two",
	), struct{}{})
	root := txn.Commit().Root()
	require.True(t, incrementalActiveGroupExists(root, component.group))
	require.False(t, incrementalActiveGroupExists(root, "group\x00"))

	members := iradix.New[struct{}]().Txn()
	members.Insert(memberKey("route", "default", "one"), struct{}{})
	members.Insert(memberKey("routes", "default", "two"), struct{}{})
	count := 0
	members.Commit().Root().WalkPrefix(memberPrefix("route"), func(_ []byte, _ struct{}) bool {
		count++
		return false
	})
	require.Equal(t, 1, count)

	resources := iradix.New[struct{}]().Txn()
	resources.Insert([]byte(resourceInputKey(&resourceInputSpec{
		resourceType: "route", scope: resourceInputIdentity, namespace: "default", name: "one",
	}).Opaque()), struct{}{})
	resources.Insert([]byte(resourceInputKey(&resourceInputSpec{
		resourceType: "routes", scope: resourceInputIdentity, namespace: "default", name: "two",
	}).Opaque()), struct{}{})
	count = 0
	resources.Commit().Root().WalkPrefix(resourceInputPrefix("route"), func(_ []byte, _ struct{}) bool {
		count++
		return false
	})
	require.Equal(t, 1, count)
}

func testOpaqueEncoding(values ...string) string {
	encoded := make([]byte, 0)
	for _, value := range values {
		encoded = binary.AppendUvarint(encoded, uint64(len(value)))
		encoded = append(encoded, value...)
	}
	return string(encoded)
}

func TestExactBytesRevisionSeparatesKindAndValue(t *testing.T) {
	inputs := []struct {
		kind  string
		value []byte
	}{
		{kind: "a", value: []byte("bc")},
		{kind: "ab", value: []byte("c")},
		{kind: "a\x00", value: []byte("bc")},
		{kind: "a", value: []byte("\x00bc")},
		{kind: "", value: []byte("abc")},
	}
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		revision := exactBytesRevision(input.kind, input.value).Opaque()
		_, duplicate := seen[revision]
		require.False(t, duplicate)
		seen[revision] = struct{}{}
		require.Equal(t, revision, exactBytesRevision(input.kind, input.value).Opaque())
	}
}

func TestSealedResourceInputKeyFallsBackAfterTampering(t *testing.T) {
	original := sealResourceInputSpec(&resourceInputSpec{
		resourceType: "routes", scope: resourceInputGet, keys: []string{"default", "route"},
	})
	expected := buildResourceInputKey(&original)
	require.Equal(t, expected, resourceInputKey(&original))

	tests := map[string]func(*resourceInputSpec){
		"cache key": func(spec *resourceInputSpec) {
			spec.keyCache.key = incremental.NewInputKey("resource.poison")
		},
		"proof key": func(spec *resourceInputSpec) {
			spec.keyCache.proof.key = incremental.NewInputKey("resource.poison")
		},
		"cache keys": func(spec *resourceInputSpec) {
			spec.keyCache.keys[0] = "poison"
		},
		"proof keys": func(spec *resourceInputSpec) {
			spec.keyCache.proof.keys[0] = "poison"
		},
		"seal": func(spec *resourceInputSpec) {
			spec.keyCache.seal = nil
		},
		"foreign": func(spec *resourceInputSpec) {
			foreign := sealResourceInputSpec(&resourceInputSpec{
				resourceType: "services", scope: resourceInputGet, keys: []string{"default", "route"},
			})
			spec.keyCache = foreign.keyCache
		},
	}
	for name, poison := range tests {
		t.Run(name, func(t *testing.T) {
			spec := sealResourceInputSpec(&resourceInputSpec{
				resourceType: "routes", scope: resourceInputGet, keys: []string{"default", "route"},
			})
			poison(&spec)
			require.Equal(t, expected, resourceInputKey(&spec))
		})
	}

	changed := original
	changed.keys = []string{"default", "other"}
	require.Equal(t, buildResourceInputKey(&changed), resourceInputKey(&changed))
	require.NotEqual(t, expected, resourceInputKey(&changed))
}

func TestIncrementalKeyParsersRejectNoncanonicalAliases(t *testing.T) {
	component := componentQueryKey(&incrementalComponent{name: "a"}, "a", "a", "a").Opaque()
	activation := activationQueryKey("a", "a", "a").Opaque()
	derived := derivedProjectionQueryKey("a", "a", "a").Opaque()
	resource := resourceInputKey(&resourceInputSpec{
		resourceType: "a", scope: resourceInputIdentity, namespace: "a", name: "a",
	}).Opaque()
	http := httpInputKey(1).Opaque()

	tests := []struct {
		name  string
		value string
		parse func(string) bool
	}{
		{
			name: "component overlong length", value: overlongLastOpaqueFrame(component),
			parse: func(value string) bool {
				_, _, _, _, ok := parseComponentQueryKey(incremental.NewQueryKey(value))
				return ok
			},
		},
		{
			name: "activation extra frame", value: activation + "\x00",
			parse: func(value string) bool {
				_, _, _, ok := parseActivationQueryKey(incremental.NewQueryKey(value))
				return ok
			},
		},
		{
			name: "derived truncated", value: derived[:len(derived)-1],
			parse: func(value string) bool {
				_, _, _, ok := parseDerivedProjectionQueryKey(incremental.NewQueryKey(value))
				return ok
			},
		},
		{
			name: "resource overlong length", value: overlongLastOpaqueFrame(resource),
			parse: func(value string) bool {
				_, ok := parseResourceInputKey(incremental.NewInputKey(value))
				return ok
			},
		},
		{
			name: "resource extra frame", value: resource + "\x00",
			parse: func(value string) bool {
				_, ok := parseResourceInputKey(incremental.NewInputKey(value))
				return ok
			},
		},
		{
			name: "http overlong length", value: overlongLastOpaqueFrame(http),
			parse: func(value string) bool {
				_, ok := parseHTTPInputKey(incremental.NewInputKey(value))
				return ok
			},
		},
		{
			name: "http leading zero", value: incremental.EncodeTuple("http", "01"),
			parse: func(value string) bool {
				_, ok := parseHTTPInputKey(incremental.NewInputKey(value))
				return ok
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.False(t, test.parse(test.value))
		})
	}

	componentName, componentSource, componentNamespace, componentObject, componentOK :=
		parseComponentQueryKey(incremental.NewQueryKey(component))
	activationSource, activationNamespace, activationName, activationOK :=
		parseActivationQueryKey(incremental.NewQueryKey(activation))
	derivedSource, derivedNamespace, derivedName, derivedOK :=
		parseDerivedProjectionQueryKey(incremental.NewQueryKey(derived))
	_, resourceOK := parseResourceInputKey(incremental.NewInputKey(resource))
	_, httpOK := parseHTTPInputKey(incremental.NewInputKey(http))
	require.True(t, componentOK)
	require.Equal(t,
		[]string{"a", "a", "a", "a"},
		[]string{componentName, componentSource, componentNamespace, componentObject},
	)
	require.True(t, activationOK)
	require.Equal(t, []string{"a", "a", "a"}, []string{activationSource, activationNamespace, activationName})
	require.True(t, derivedOK)
	require.Equal(t, []string{"a", "a", "a"}, []string{derivedSource, derivedNamespace, derivedName})
	require.True(t, resourceOK)
	require.True(t, httpOK)
}

func overlongLastOpaqueFrame(value string) string {
	position := 0
	last := -1
	lastPrefixLength := 0
	for position < len(value) {
		last = position
		length, prefixLength := binary.Uvarint([]byte(value[position:]))
		if prefixLength <= 0 || length > uint64(len(value)-position-prefixLength) {
			return value
		}
		lastPrefixLength = prefixLength
		position += prefixLength + int(length)
	}
	if last < 0 || lastPrefixLength <= 0 {
		return value
	}
	prefix := append([]byte(nil), value[last:last+lastPrefixLength]...)
	prefix[len(prefix)-1] |= 0x80
	result := make([]byte, 0, len(value)+1)
	result = append(result, value[:last]...)
	result = append(result, prefix...)
	result = append(result, 0)
	result = append(result, value[last+lastPrefixLength:]...)
	return string(result)
}
