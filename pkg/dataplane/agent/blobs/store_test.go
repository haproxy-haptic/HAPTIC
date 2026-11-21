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

package blobs_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/blobs"
)

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func newStore(t *testing.T, maxSize int64) (store *blobs.Store, dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "blobs")
	store, err := blobs.New(dir, maxSize)
	require.NoError(t, err)
	return store, dir
}

// names lists the directory, temporary files included, so a test sees
// whatever a failed put left behind.
func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestPutStoresTheBlobUnderItsDigest(t *testing.T) {
	store, dir := newStore(t, 1<<20)
	digest := digestOf("database v1")

	size, err := store.Put(digest, strings.NewReader("database v1"))
	require.NoError(t, err)
	assert.Equal(t, int64(len("database v1")), size)
	assert.Equal(t, []string{digest}, names(t, dir))

	f, held, err := store.Open(digest)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	content, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, "database v1", string(content))
	assert.Equal(t, size, held)

	info, err := os.Stat(filepath.Join(dir, digest))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o644), info.Mode().Perm(), "the spoa-hub reads blobs under its own uid")
}

func TestPutRefusesWhatItCannotVerifyAndKeepsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		digest  string
		content io.Reader
		want    error
	}{
		"digest mismatch": {digest: digestOf("expected"), content: strings.NewReader("tampered"), want: blobs.ErrDigestMismatch},
		"over the cap":    {digest: digestOf(strings.Repeat("x", 17)), content: strings.NewReader(strings.Repeat("x", 17)), want: blobs.ErrTooLarge},
		"invalid name":    {digest: strings.ToUpper(digestOf("x")), content: strings.NewReader("x"), want: blobs.ErrInvalidDigest},
		"truncated body":  {digest: digestOf("whole"), content: iotest.ErrReader(io.ErrUnexpectedEOF), want: io.ErrUnexpectedEOF},
	} {
		t.Run(name, func(t *testing.T) {
			store, dir := newStore(t, 16)
			_, err := store.Put(tc.digest, tc.content)
			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, names(t, dir))
		})
	}
}

func TestPutAcceptsABlobOfExactlyTheCap(t *testing.T) {
	store, _ := newStore(t, 16)
	content := strings.Repeat("y", 16)
	_, err := store.Put(digestOf(content), strings.NewReader(content))
	require.NoError(t, err)
}

func TestConcurrentPutsOfOneDigestLeaveOneCompleteBlob(t *testing.T) {
	store, dir := newStore(t, 1<<20)
	content := strings.Repeat("mmdb", 64<<10)
	digest := digestOf(content)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := store.Put(digest, iotest.HalfReader(strings.NewReader(content)))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, []string{digest}, names(t, dir))
	held, err := store.List()
	require.NoError(t, err)
	assert.Equal(t, []api.Blob{{SHA256: digest, Size: int64(len(content))}}, held)
}

func TestListAndMissingIgnoreForeignFiles(t *testing.T) {
	store, dir := newStore(t, 1<<20)
	a, b := digestOf("a"), digestOf("b")
	for content, digest := range map[string]string{"a": a, "b": b} {
		_, err := store.Put(digest, strings.NewReader(content))
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README"), []byte("not a blob"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, digestOf("dir")), 0o750))

	held, err := store.List()
	require.NoError(t, err)
	want := []api.Blob{{SHA256: a, Size: 1}, {SHA256: b, Size: 1}}
	if a > b {
		want[0], want[1] = want[1], want[0]
	}
	assert.Equal(t, want, held)
	assert.Equal(t, []string{digestOf("c"), digestOf("dir")}, store.Missing([]string{a, digestOf("c"), b, digestOf("dir")}))
}

func TestOpenOfAnUnheldBlobIsNotExist(t *testing.T) {
	store, _ := newStore(t, 1<<20)
	_, _, err := store.Open(digestOf("absent"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, _, err = store.Open("../escape")
	require.ErrorIs(t, err, blobs.ErrInvalidDigest)
}

func TestGCRemovesOnlyUnkeptBlobsWrittenBeforeTheCutoff(t *testing.T) {
	store, dir := newStore(t, 1<<20)
	kept, stale, fresh := digestOf("kept"), digestOf("stale"), digestOf("fresh")
	for content, digest := range map[string]string{"kept": kept, "stale": stale} {
		_, err := store.Put(digest, strings.NewReader(content))
		require.NoError(t, err)
	}
	past := time.Now().Add(-time.Hour)
	for _, digest := range []string{kept, stale} {
		require.NoError(t, os.Chtimes(filepath.Join(dir, digest), past, past))
	}
	cutoff := time.Now().Add(-time.Minute)
	_, err := store.Put(fresh, strings.NewReader("fresh"))
	require.NoError(t, err)

	removed, err := store.GC([]string{kept}, cutoff)
	require.NoError(t, err)
	assert.Equal(t, []string{stale}, removed)
	assert.ElementsMatch(t, []string{kept, fresh}, names(t, dir), "a blob put after the cutoff waits for the next GC")

	removed, err = store.GC(nil, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{kept, fresh}, removed)
	assert.Empty(t, names(t, dir))
}

func TestNewSweepsUnfinishedPutsAndRefusesBadSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blobs")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".put-123"), []byte("half"), 0o600))

	_, err := blobs.New(dir, 1)
	require.NoError(t, err)
	assert.Empty(t, names(t, dir))

	_, err = blobs.New("relative/blobs", 1)
	require.Error(t, err)
	_, err = blobs.New(dir, 0)
	require.Error(t, err)
}

func TestPutReportsAFailedDirectory(t *testing.T) {
	store, dir := newStore(t, 1<<20)
	require.NoError(t, os.Remove(dir))
	_, err := store.Put(digestOf("x"), strings.NewReader("x"))
	require.Error(t, err)
	assert.False(t, errors.Is(err, blobs.ErrDigestMismatch))
}
