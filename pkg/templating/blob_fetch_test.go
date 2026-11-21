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
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
)

// recordingFetcher answers http.Fetch calls with a fixed result and keeps the arguments.
type recordingFetcher struct {
	content string
	err     error
	calls   [][]any
}

func (f *recordingFetcher) Fetch(args ...any) (any, error) {
	f.calls = append(f.calls, args)
	return f.content, f.err
}

func (f *recordingFetcher) Pending(string) bool { return false }

const blobTemplate = `{%- var db, err = blob.Fetch("https://example.com/City.tar.gz", map[string]any{
	"interval": "24h", "unpack": "tar.gz", "member": "City.mmdb", "maxSize": 1024,
}) -%}
{%- if err != nil %}{{ fail(tostring(err)) }}{% end -%}
digest={{ db.digest }} size={{ db.size }} path={{ db.path }}`

func renderBlobTemplate(t *testing.T, template string, fetcher HTTPFetcher) (string, error) {
	t.Helper()
	engine, err := New(map[string]string{"main": template}, nil)
	require.NoError(t, err)
	context := map[string]any{}
	if fetcher != nil {
		context["http"] = fetcher
	}
	output, err := engine.Render(t.Context(), "main", context)
	return strings.TrimSpace(output), err
}

func TestBlobFetchReturnsTheAcceptedHandle(t *testing.T) {
	ref := blob.Describe([]byte("database"))
	fetcher := &recordingFetcher{content: ref.Encode()}

	output, err := renderBlobTemplate(t, blobTemplate, fetcher)

	require.NoError(t, err)
	assert.Equal(t, "digest="+ref.Digest+" size=8 path="+blob.PathPrefix+ref.Digest, output)
	require.Len(t, fetcher.calls, 1)
	assert.Equal(t, "https://example.com/City.tar.gz", fetcher.calls[0][0])
	assert.Equal(t, map[string]any{
		"interval":     "24h",
		"timeout":      defaultBlobTimeout,
		HTTPOptionBlob: blob.Spec{Unpack: blob.UnpackTarGz, Member: "City.mmdb", MaxSize: 1024},
	}, fetcher.calls[0][1], "blob.Fetch declares a non-critical http.Fetch source")
}

func TestBlobFetchReturnsAnEmptyHandleWithoutAcceptedContent(t *testing.T) {
	for name, fetcher := range map[string]HTTPFetcher{
		"pending or failed fetch": &recordingFetcher{content: ""},
		"no http fetcher":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			output, err := renderBlobTemplate(t, blobTemplate, fetcher)
			require.NoError(t, err, "a blob that is not available never fails the render")
			assert.Equal(t, "digest= size=0 path=", output)
		})
	}
}

// A fleet with an agent that cannot store blobs gets no blob references, and
// nothing is fetched, so no version is accepted that a pod could not receive.
func TestBlobFetchDeclaresNothingWhileTheFleetCannotStoreBlobs(t *testing.T) {
	ref := blob.Describe([]byte("database"))
	for name, capabilities := range map[string]any{
		"map":         map[string]any{"supports_blobs": false},
		"map pointer": &map[string]any{"supports_blobs": false},
	} {
		t.Run(name, func(t *testing.T) {
			fetcher := &recordingFetcher{content: ref.Encode()}
			engine, err := New(map[string]string{"main": blobTemplate}, nil)
			require.NoError(t, err)

			output, err := engine.Render(t.Context(), "main", map[string]any{"http": fetcher, "capabilities": capabilities})

			require.NoError(t, err)
			assert.Equal(t, "digest= size=0 path=", strings.TrimSpace(output))
			assert.Empty(t, fetcher.calls)
		})
	}
}

func TestBlobFetchDeclaresTheSourceWhenTheFleetStoresBlobs(t *testing.T) {
	ref := blob.Describe([]byte("database"))
	fetcher := &recordingFetcher{content: ref.Encode()}
	engine, err := New(map[string]string{"main": blobTemplate}, nil)
	require.NoError(t, err)

	output, err := engine.Render(t.Context(), "main", map[string]any{
		"http": fetcher, "capabilities": map[string]any{"supports_blobs": true},
	})

	require.NoError(t, err)
	assert.Contains(t, output, "path="+ref.Path())
	assert.Len(t, fetcher.calls, 1)
}

func TestBlobFetchPassesFetcherErrorsThrough(t *testing.T) {
	fetcher := &recordingFetcher{err: errors.New("render must restart")}
	_, err := renderBlobTemplate(t, blobTemplate, fetcher)
	require.ErrorContains(t, err, "render must restart")
}

func TestBlobFetchRefusesInvalidOptions(t *testing.T) {
	tests := map[string]string{
		"unknown option":       `"critical": true`,
		"member without tar":   `"member": "x"`,
		"tar without member":   `"unpack": "tar.gz"`,
		"unknown unpack":       `"unpack": "zip"`,
		"invalid digest":       `"sha256": "ABC"`,
		"negative size":        `"maxSize": -1`,
		"fractional size":      `"maxSize": 1.5`,
		"non-string interval":  `"interval": 5`,
		"non-numeric retries":  `"retries": "3"`,
		"non-string unpacking": `"unpack": 1`,
	}
	for name, option := range tests {
		t.Run(name, func(t *testing.T) {
			fetcher := &recordingFetcher{}
			template := `{%- var db, err = blob.Fetch("https://example.com/db", map[string]any{` + option + `}) -%}
{%- if err != nil %}{{ fail(tostring(err)) }}{% end -%}{{ db.path }}`
			_, err := renderBlobTemplate(t, template, fetcher)
			require.ErrorContains(t, err, "blob.Fetch")
			assert.Empty(t, fetcher.calls, "an invalid declaration fetches nothing")
		})
	}
}

func TestBlobFetchDefaults(t *testing.T) {
	fetcher := &recordingFetcher{}
	_, err := renderBlobTemplate(t, `{%- var db, err = blob.Fetch("https://example.com/db") -%}{{ db.path }}`, fetcher)
	require.NoError(t, err)
	require.Len(t, fetcher.calls, 1)
	assert.Equal(t, map[string]any{
		"timeout":      defaultBlobTimeout,
		HTTPOptionBlob: blob.Spec{Unpack: blob.UnpackNone},
	}, fetcher.calls[0][1])
}

func TestHTTPFetchReservesTheBlobOption(t *testing.T) {
	_, err := CanonicalIncrementalHTTPArgs("https://example.com", map[string]any{HTTPOptionBlob: "x"})
	require.ErrorContains(t, err, "reserved for blob.Fetch")

	spec := blob.Spec{Unpack: blob.UnpackGzip}
	canonical, err := CanonicalIncrementalHTTPArgs("https://example.com", map[string]any{HTTPOptionBlob: spec, "timeout": "5m"})
	require.NoError(t, err)
	assert.Equal(t, spec, canonical[1].(map[string]any)[HTTPOptionBlob])
}
