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

package blob

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpecValidate(t *testing.T) {
	digest := strings.Repeat("a", 64)
	tests := []struct {
		name    string
		spec    Spec
		wantErr string
	}{
		{name: "none", spec: Spec{Unpack: UnpackNone}},
		{name: "gzip with pin and size", spec: Spec{Unpack: UnpackGzip, SHA256: digest, MaxSize: 1}},
		{name: "tar.gz member", spec: Spec{Unpack: UnpackTarGz, Member: "./db/City.mmdb"}},
		{name: "unknown unpack", spec: Spec{Unpack: "zip"}, wantErr: `unpack "zip" is unknown`},
		{name: "member without tar.gz", spec: Spec{Unpack: UnpackGzip, Member: "x"}, wantErr: "needs unpack"},
		{name: "tar.gz without member", spec: Spec{Unpack: UnpackTarGz, Member: "./"}, wantErr: "needs a member"},
		{name: "uppercase pin", spec: Spec{Unpack: UnpackNone, SHA256: strings.ToUpper(digest)}, wantErr: "64 lowercase hex"},
		{name: "negative size", spec: Spec{Unpack: UnpackNone, MaxSize: -1}, wantErr: "negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
	assert.False(t, Spec{}.Declared(), "the zero value declares no blob")
}

func TestKeyRoundTrips(t *testing.T) {
	spec := Spec{Unpack: UnpackTarGz, Member: "db/City.mmdb", SHA256: strings.Repeat("b", 64), MaxSize: 7}
	key := spec.Key()
	require.True(t, key.Declared())
	decoded, err := key.Spec()
	require.NoError(t, err)
	assert.Equal(t, spec, decoded)
	assert.Equal(t, Key(""), Spec{}.Key(), "the zero spec has the empty key")

	_, err = Key("{").Spec()
	require.Error(t, err)
	_, err = Spec{Unpack: "zip"}.Key().Spec()
	require.ErrorContains(t, err, "unknown")
}

func TestRefEncodingRoundTrips(t *testing.T) {
	ref := Describe([]byte("database"))
	decoded, ok := DecodeRef(ref.Encode())
	require.True(t, ok)
	assert.Equal(t, ref, decoded)
	assert.Equal(t, int64(len("database")), decoded.Size)
	assert.Equal(t, PathPrefix+ref.Digest, decoded.Path())

	for _, content := range []string{"", "database", ref.Digest, ref.Digest + " -1", ref.Digest + " x", "ZZ 1"} {
		_, ok := DecodeRef(content)
		assert.False(t, ok, content)
	}
}

func TestReferencedDigestsFindsDataPlanePaths(t *testing.T) {
	first := Describe([]byte("first")).Digest
	second := Describe([]byte("second")).Digest
	config := "path = \"" + PathPrefix + second + "\"\n" +
		"other = \"" + PathPrefix + first + "\"\n" +
		"again = \"" + PathPrefix + second + "\"\n" +
		"longer = \"" + PathPrefix + first + "ab\"\n" +
		"short = \"" + PathPrefix + "abc\"\n"

	assert.Equal(t, slices.Sorted(slices.Values([]string{first, second})), ReferencedDigests(config, PathPrefix+first))
	assert.Empty(t, ReferencedDigests("no blobs here"))
}

func TestCleanMember(t *testing.T) {
	assert.Equal(t, "db/City.mmdb", CleanMember("././/db/City.mmdb"))
	assert.Equal(t, "City.mmdb", CleanMember("City.mmdb"))
}
