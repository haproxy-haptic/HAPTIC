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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/native"
	"github.com/stretchr/testify/require"
)

func TestJWKSPublicKeysRotation(t *testing.T) {
	first, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	second, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	old := testRSAJWK(t, "old", first)
	fresh := testRSAJWK(t, "new", second)
	for _, tc := range []struct {
		name string
		keys []jsonWebKey
		ids  []string
	}{
		{"initial", []jsonWebKey{old}, []string{"old"}},
		{"overlap", []jsonWebKey{old, fresh}, []string{"old", "new"}},
		{"retired", []jsonWebKey{fresh}, []string{"new"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, parseErr := scriggoJWKSPublicKeys(testJWKS(t, tc.keys), "RS256")
			require.NoError(t, parseErr)
			require.Len(t, keys, len(tc.ids))
			for _, id := range tc.ids {
				block, rest := pem.Decode([]byte(keys[id]))
				require.NotNil(t, block)
				require.Empty(t, rest)
				public, decodeErr := x509.ParsePKIXPublicKey(block.Bytes)
				require.NoError(t, decodeErr)
				expected := &first.PublicKey
				if id == "new" {
					expected = &second.PublicKey
				}
				require.Equal(t, expected, public)
			}
		})
	}
}

func TestJWKSPublicKeysValidation(t *testing.T) {
	empty, emptyErr := scriggoJWKSPublicKeys(`{"keys":[]}`, "RS256")
	require.NoError(t, emptyErr)
	require.Empty(t, empty)
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	valid := testRSAJWK(t, "key", private)
	for _, tc := range []struct {
		name, algorithm      string
		change               func(*jsonWebKey)
		wantError, wantEmpty bool
	}{
		{name: "RS256", algorithm: "RS256"},
		{name: "PS512", algorithm: "PS512"},
		{name: "symmetric", algorithm: "HS256", wantError: true},
		{name: "unsigned", algorithm: "none", wantError: true},
		{name: "missing kid", change: func(k *jsonWebKey) { k.ID = "" }, wantError: true},
		{name: "long kid", change: func(k *jsonWebKey) { k.ID = strings.Repeat("x", 257) }, wantError: true},
		{name: "private", change: func(k *jsonWebKey) { k.D = "AQ" }, wantError: true},
		{name: "encryption", change: func(k *jsonWebKey) { k.Use = "enc" }, wantEmpty: true},
		{name: "sign only", change: func(k *jsonWebKey) { k.Ops = []string{"sign"} }, wantEmpty: true},
		{name: "no operations", change: func(k *jsonWebKey) { k.Ops = []string{} }, wantEmpty: true},
		{name: "verify operation", change: func(k *jsonWebKey) { k.Ops = []string{"verify"} }},
		{name: "other algorithm", change: func(k *jsonWebKey) { k.Alg = "RS512" }, wantEmpty: true},
		{name: "unknown type", change: func(k *jsonWebKey) { k.KeyType = "unknown" }, wantEmpty: true},
		{name: "bad modulus", change: func(k *jsonWebKey) { k.N = "invalid!" }, wantError: true},
		{name: "small modulus", change: func(k *jsonWebKey) { k.N = "Aw" }, wantError: true},
		{name: "leading zero", change: func(k *jsonWebKey) { k.N = "AAM" }, wantError: true},
		{name: "bad exponent", change: func(k *jsonWebKey) { k.E = "!" }, wantError: true},
		{name: "even exponent", change: func(k *jsonWebKey) { k.E = "BA" }, wantError: true},
		{name: "large exponent", change: func(k *jsonWebKey) { k.E = "gAAAAQ" }, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := valid
			if tc.change != nil {
				tc.change(&key)
			}
			algorithm := tc.algorithm
			if algorithm == "" {
				algorithm = "RS256"
			}
			keys, parseErr := scriggoJWKSPublicKeys(testJWKS(t, []jsonWebKey{key}), algorithm)
			if tc.wantError {
				require.Error(t, parseErr)
				require.Nil(t, keys)
				return
			}
			require.NoError(t, parseErr)
			if tc.wantEmpty {
				require.Empty(t, keys)
			} else {
				require.Contains(t, keys, key.ID)
			}
		})
	}
	for _, input := range []string{"", "null", "{}", `{"keys":{}}`, `{"keys":[null]}`, testJWKS(t, []jsonWebKey{valid, valid}), strings.Repeat(" ", (1<<20)+1), testJWKS(t, make([]jsonWebKey, maxJWKSKeys+1))} {
		_, parseErr := scriggoJWKSPublicKeys(input, "RS256")
		require.Error(t, parseErr)
	}
}

func TestJWKSPublicKeysEC(t *testing.T) {
	for _, tc := range []struct {
		algorithm string
		curve     elliptic.Curve
	}{
		{"ES256", elliptic.P256()}, {"ES384", elliptic.P384()}, {"ES512", elliptic.P521()},
	} {
		t.Run(tc.algorithm, func(t *testing.T) {
			private, err := ecdsa.GenerateKey(tc.curve, rand.Reader)
			require.NoError(t, err)
			width := (tc.curve.Params().BitSize + 7) / 8
			point, err := private.PublicKey.Bytes()
			require.NoError(t, err)
			key := jsonWebKey{ID: "ec", KeyType: "EC", Curve: tc.curve.Params().Name,
				X: base64.RawURLEncoding.EncodeToString(point[1 : 1+width]),
				Y: base64.RawURLEncoding.EncodeToString(point[1+width:])}
			keys, err := scriggoJWKSPublicKeys(testJWKS(t, []jsonWebKey{key}), tc.algorithm)
			require.NoError(t, err)
			block, _ := pem.Decode([]byte(keys["ec"]))
			require.NotNil(t, block)
			public, err := x509.ParsePKIXPublicKey(block.Bytes)
			require.NoError(t, err)
			require.True(t, private.PublicKey.Equal(public))
			key.Y = base64.RawURLEncoding.EncodeToString(make([]byte, width))
			_, err = scriggoJWKSPublicKeys(testJWKS(t, []jsonWebKey{key}), tc.algorithm)
			require.Error(t, err)
			key.X = "AQ"
			_, err = scriggoJWKSPublicKeys(testJWKS(t, []jsonWebKey{key}), tc.algorithm)
			require.Error(t, err)
		})
	}
}

func TestJWKSPublicKeysIncremental(t *testing.T) {
	template := `{% var keys, err = jwks_public_keys(item["content"].(string), "RS256") %}{% if err != nil %}invalid{% else %}{{ len(keys) }}{% end %}`
	engine, err := New(map[string]string{"test": template}, &Options{EntryPoints: []string{"test"}, IncrementalEntryPoints: []string{"test"}})
	require.NoError(t, err)
	require.True(t, engine.compiledTemplates["test"].BatchSafe())
	require.NoError(t, engine.compiledTemplates["test"].DeterministicSafe())
	output, err := engine.RenderIncrementalComponent(t.Context(), "test", incrementalComponentContext(map[string]any{"item": map[string]any{"content": "bad"}}))
	require.NoError(t, err)
	require.Equal(t, "invalid", output)
	replay, err := New(map[string]string{"test": `{% var keys, err = jwks_public_keys("bad", "RS256") %}{% if err != nil %}invalid{% end %}`}, &Options{EntryPoints: []string{"test"}})
	require.NoError(t, err)
	_, err = replay.PrepareExactCycleReplay([]string{"test"})
	require.NoError(t, err)
}

func TestJWKSPublicKeysDirectFrame(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	source := `{% var keys, err = jwks_public_keys(document, "RS256") %}{% if err != nil %}invalid{% else %}{{ len(keys) }}{% end %}`
	template, err := scriggo.BuildTemplate(scriggo.Files{"test.txt": []byte(source)}, "test.txt", &scriggo.BuildOptions{
		Globals: native.Declarations{builtinJWKSPublicKeys: native.Synchronous(scriggoJWKSPublicKeys), "document": (*string)(nil)},
	})
	require.NoError(t, err)
	require.NoError(t, certifyIncrementalNativeFunctionFrames([]*scriggo.Template{template}, nil))
	for _, tc := range []struct{ document, want string }{
		{testJWKS(t, []jsonWebKey{testRSAJWK(t, "key", key)}), "1"},
		{`{"keys":[]}`, "0"},
		{"bad", "invalid"},
	} {
		var output strings.Builder
		err = template.Run(&output, map[string]any{"document": tc.document}, &scriggo.RunOptions{
			Context: t.Context(), NativeFunctionTrampolines: incrementalNativeFunctionFrameTrampolines,
		})
		require.NoError(t, err)
		require.Equal(t, tc.want, output.String())
	}
}

func testRSAJWK(t *testing.T, id string, private *rsa.PrivateKey) jsonWebKey {
	t.Helper()
	return jsonWebKey{KeyType: "RSA", ID: id, N: base64.RawURLEncoding.EncodeToString(private.N.Bytes()), E: "AQAB"}
}

func testJWKS(t *testing.T, keys []jsonWebKey) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"keys": keys})
	require.NoError(t, err)
	return string(data)
}
