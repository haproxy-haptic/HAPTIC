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

package blobstore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestStore(t *testing.T, maxSize int64) (*Store, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	store, err := New(t.TempDir(), maxSize, slog.New(slog.NewTextHandler(io.Discard, nil)), WithClock(clock.Now))
	require.NoError(t, err)
	return store, clock
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, err := writer.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func tarGz(t *testing.T, files map[string][]byte, order ...string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, name := range order {
		data := files[name]
		require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}))
		_, err := writer.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return gzipped(t, archive.Bytes())
}

func readBlob(t *testing.T, store *Store, digest string) []byte {
	t.Helper()
	file, err := store.Open(digest)
	require.NoError(t, err)
	defer file.Close()
	data, err := io.ReadAll(file)
	require.NoError(t, err)
	return data
}

func assertNoTemporaryFiles(t *testing.T, store *Store) {
	t.Helper()
	entries, err := os.ReadDir(store.dir)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), ingestPrefix), "leftover %s", entry.Name())
	}
}

func TestIngestStoresTheBodyUnderItsDigest(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	data := []byte("geo database")

	ref, err := store.Ingest(t.Context(), bytes.NewReader(data), blob.Spec{Unpack: blob.UnpackNone})

	require.NoError(t, err)
	assert.Equal(t, blob.Describe(data), ref)
	assert.Equal(t, data, readBlob(t, store, ref.Digest))
	assert.True(t, store.Has(ref.Digest))
	info, err := os.Stat(filepath.Join(store.dir, ref.Digest))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(blobFileMode), info.Mode().Perm(), "a stored blob is immutable")
	assertNoTemporaryFiles(t, store)

	again, err := store.Ingest(t.Context(), bytes.NewReader(data), blob.Spec{Unpack: blob.UnpackNone})
	require.NoError(t, err)
	assert.Equal(t, ref, again, "the same bytes land on the same file")
}

func TestIngestUnpacks(t *testing.T) {
	data := []byte("the member's bytes")
	tests := []struct {
		name string
		body []byte
		spec blob.Spec
	}{
		{name: "gzip", body: gzipped(t, data), spec: blob.Spec{Unpack: blob.UnpackGzip}},
		{
			name: "tar.gz member",
			body: tarGz(t, map[string][]byte{"db/README.txt": []byte("readme"), "./db/City.mmdb": data, "db/LICENSE": []byte("license")},
				"db/README.txt", "./db/City.mmdb", "db/LICENSE"),
			spec: blob.Spec{Unpack: blob.UnpackTarGz, Member: "db/City.mmdb"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := newTestStore(t, 1<<20)
			ref, err := store.Ingest(t.Context(), bytes.NewReader(tt.body), tt.spec)
			require.NoError(t, err)
			assert.Equal(t, blob.Describe(data), ref, "the digest covers the unpacked bytes")
			assert.Equal(t, data, readBlob(t, store, ref.Digest))
		})
	}
}

func TestIngestRefusesContentARetryCannotFix(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 64)
	tests := []struct {
		name    string
		maxSize int64
		body    []byte
		spec    blob.Spec
		wantErr string
	}{
		{name: "declared size", maxSize: 1 << 20, body: data, spec: blob.Spec{Unpack: blob.UnpackNone, MaxSize: 63}, wantErr: "size limit of 63 bytes"},
		{name: "store cap beats the declaration", maxSize: 32, body: data, spec: blob.Spec{Unpack: blob.UnpackNone, MaxSize: 1 << 20}, wantErr: "size limit of 32 bytes"},
		{name: "cap applies to unpacked bytes", maxSize: 63, body: gzipped(t, data), spec: blob.Spec{Unpack: blob.UnpackGzip}, wantErr: "size limit of 63 bytes"},
		{
			name: "cap applies to the archive up to the member's end", maxSize: 550,
			body: tarGz(t, map[string][]byte{"m": data}, "m"), spec: blob.Spec{Unpack: blob.UnpackTarGz, Member: "m"},
			wantErr: "size limit of 550 bytes",
		},
		{name: "pinned digest", maxSize: 1 << 20, body: data, spec: blob.Spec{Unpack: blob.UnpackNone, SHA256: blob.Describe([]byte("other")).Digest}, wantErr: "the declaration pins"},
		{name: "missing member", maxSize: 1 << 20, body: tarGz(t, map[string][]byte{"a": data}, "a"), spec: blob.Spec{Unpack: blob.UnpackTarGz, Member: "b"}, wantErr: `no member "b"`},
		{name: "not gzip", maxSize: 1 << 20, body: data, spec: blob.Spec{Unpack: blob.UnpackGzip}, wantErr: "not gzip"},
		{name: "invalid spec", maxSize: 1 << 20, body: data, spec: blob.Spec{Unpack: "zip"}, wantErr: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, _ := newTestStore(t, tt.maxSize)
			_, err := store.Ingest(t.Context(), bytes.NewReader(tt.body), tt.spec)
			require.ErrorIs(t, err, blob.ErrRefused)
			assert.ErrorContains(t, err, tt.wantErr)
			entries, readErr := os.ReadDir(store.dir)
			require.NoError(t, readErr)
			assert.Empty(t, entries, "refused content leaves nothing behind")
		})
	}
}

func TestIngestPinnedDigestAccepted(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	data := []byte("pinned")
	ref, err := store.Ingest(t.Context(), bytes.NewReader(data), blob.Spec{Unpack: blob.UnpackNone, SHA256: blob.Describe(data).Digest})
	require.NoError(t, err)
	assert.Equal(t, blob.Describe(data), ref)
}

func TestIngestStopsWhenTheContextEnds(t *testing.T) {
	store, _ := newTestStore(t, 1<<20)
	cancelled, stop := context.WithCancel(t.Context())
	stop()
	_, err := store.Ingest(cancelled, bytes.NewReader([]byte("data")), blob.Spec{Unpack: blob.UnpackNone})
	require.Error(t, err)
	assert.NotErrorIs(t, err, blob.ErrRefused, "a cancelled download may succeed on retry")
	assertNoTemporaryFiles(t, store)
}

func TestRetainKeepsHeldDigestsAndRecentOrphans(t *testing.T) {
	store, clock := newTestStore(t, 1<<20)
	ingest := func(data string) string {
		ref, err := store.Ingest(t.Context(), strings.NewReader(data), blob.Spec{Unpack: blob.UnpackNone})
		require.NoError(t, err)
		return ref.Digest
	}
	held, inUse, orphan := ingest("held"), ingest("in use"), ingest("orphan")
	remove := store.AddHolder(func() []string { return []string{held} })
	leftover := filepath.Join(store.dir, ingestPrefix+"crashed")
	require.NoError(t, os.WriteFile(leftover, []byte("partial"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(store.dir, "unrelated"), []byte("x"), 0o600))
	require.NoError(t, os.Chtimes(leftover, clock.Now(), clock.Now()))

	removed, err := store.Retain([]string{inUse})
	require.NoError(t, err)
	assert.Empty(t, removed, "an orphan inside its grace may still be accepted by a render")

	clock.Advance(OrphanGrace)
	removed, err = store.Retain([]string{inUse})
	require.NoError(t, err)
	assert.Equal(t, []string{orphan}, removed)
	assert.True(t, store.Has(held))
	assert.True(t, store.Has(inUse))
	assert.False(t, store.Has(orphan))
	assert.NoFileExists(t, leftover)
	assert.FileExists(t, filepath.Join(store.dir, "unrelated"), "the store removes only its own files")

	remove()
	removed, err = store.Retain(nil)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{held, inUse}, removed)
}

func TestRetainGivesBlobsFromAnEarlierProcessTheGraceFromStart(t *testing.T) {
	dir := t.TempDir()
	data := []byte("survivor")
	digest := blob.Describe(data).Digest
	require.NoError(t, os.WriteFile(filepath.Join(dir, digest), data, blobFileMode))
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	store, err := New(dir, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)), WithClock(clock.Now))
	require.NoError(t, err)

	removed, err := store.Retain(nil)
	require.NoError(t, err)
	assert.Empty(t, removed)

	clock.Advance(OrphanGrace)
	removed, err = store.Retain(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{digest}, removed)
}

func TestNewValidatesItsArguments(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := New("", 1, logger)
	require.Error(t, err)
	_, err = New(t.TempDir(), 0, logger)
	require.Error(t, err)
	store, err := New(filepath.Join(t.TempDir(), "nested", "blobs"), 1, logger)
	require.NoError(t, err)
	assert.Equal(t, int64(1), store.MaxSize())
	_, err = store.Open("not-a-digest")
	require.Error(t, err)
	assert.False(t, store.Has("not-a-digest"))
}
