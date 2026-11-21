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

// Package blobs is the agent's content-addressed blob directory (ADR-0041):
// opaque files named by the lowercase hex SHA-256 of their content, outside
// the file tree and its journal. A blob is immutable, so a name either holds
// exactly its content or does not exist.
package blobs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

// tempPrefix marks a put in progress; List and GC never see such a file.
const tempPrefix = ".put-"

// The spoa-hub container reads the blobs under its own uid.
const (
	dirPerm  fs.FileMode = 0o755
	filePerm fs.FileMode = 0o644
)

var (
	// ErrInvalidDigest is a name that is not 64 lowercase hex digits.
	ErrInvalidDigest = errors.New("blob name is not a lowercase hex SHA-256")
	// ErrDigestMismatch is content that does not hash to the name it was put under.
	ErrDigestMismatch = errors.New("blob content does not match its SHA-256")
	// ErrTooLarge is content over the store's size cap.
	ErrTooLarge = errors.New("blob exceeds the size limit")
)

// Store is one blob directory.
type Store struct {
	dir     string
	maxSize int64
	// mu orders a put's rename against GC, so GC never removes a blob that a
	// put landed after GC looked at it.
	mu sync.Mutex
}

// New opens dir, creating it, and removes puts a previous process left
// unfinished. maxSize bounds one blob.
func New(dir string, maxSize int64) (*Store, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("blob directory %q must be an absolute path", dir)
	}
	if maxSize <= 0 {
		return nil, fmt.Errorf("blob size limit %d must be positive", maxSize)
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("blob directory: %w", err)
	}
	s := &Store{dir: filepath.Clean(dir), maxSize: maxSize}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("blob directory: %w", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), tempPrefix) {
			_ = os.Remove(filepath.Join(s.dir, entry.Name()))
		}
	}
	return s, nil
}

// MaxSize is the largest blob the store accepts.
func (s *Store) MaxSize() int64 {
	return s.maxSize
}

// Put streams r into a temporary file, verifies size and digest, syncs it and
// renames it onto its name. On any error nothing is kept. Putting a blob the
// store already holds replaces it with identical content, so concurrent puts
// of one digest are safe.
func (s *Store) Put(digest string, r io.Reader) (int64, error) {
	target, err := s.path(digest)
	if err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(s.dir, tempPrefix)
	if err != nil {
		return 0, fmt.Errorf("put blob %s: %w", digest, err)
	}
	size, err := s.fill(tmp, digest, r)
	if err == nil {
		err = s.land(tmp.Name(), target)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	return size, nil
}

func (s *Store) fill(tmp *os.File, digest string, r io.Reader) (int64, error) {
	summer := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(tmp, summer), io.LimitReader(r, s.maxSize+1))
	if copyErr == nil && size > s.maxSize {
		copyErr = fmt.Errorf("%w of %d bytes", ErrTooLarge, s.maxSize)
	}
	if copyErr == nil {
		if got := hex.EncodeToString(summer.Sum(nil)); got != digest {
			copyErr = fmt.Errorf("%w: content hashes to %s", ErrDigestMismatch, got)
		}
	}
	if copyErr == nil {
		copyErr = errors.Join(tmp.Chmod(filePerm), tmp.Sync())
	}
	if err := errors.Join(copyErr, tmp.Close()); err != nil {
		return 0, fmt.Errorf("put blob %s: %w", digest, err)
	}
	return size, nil
}

func (s *Store) land(tmp, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("put blob %s: %w", filepath.Base(target), err)
	}
	return syncDir(s.dir)
}

// Open returns the blob and its size; fs.ErrNotExist when it is not held.
func (s *Store) Open(digest string) (*os.File, int64, error) {
	target, err := s.path(digest)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(filepath.Clean(target))
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// List reports every blob held, sorted by digest (os.ReadDir sorts by name).
func (s *Store) List() ([]api.Blob, error) {
	entries, err := s.entries()
	if err != nil {
		return nil, err
	}
	out := make([]api.Blob, 0, len(entries))
	for _, e := range entries {
		out = append(out, api.Blob{SHA256: e.digest, Size: e.size})
	}
	return out, nil
}

// Missing returns the digests of want the store does not hold, in want's order.
func (s *Store) Missing(want []string) []string {
	var missing []string
	for _, digest := range want {
		target, err := s.path(digest)
		if err != nil {
			missing = append(missing, digest)
			continue
		}
		info, err := os.Lstat(target)
		if err != nil || !info.Mode().IsRegular() {
			missing = append(missing, digest)
		}
	}
	return missing
}

// GC removes every blob that keep does not name and that was last written
// before cutoff; a blob put after cutoff survives until the next GC, because
// the apply it was put for may still be on its way.
func (s *Store) GC(keep []string, cutoff time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.entries()
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, e := range entries {
		if slices.Contains(keep, e.digest) || !e.modified.Before(cutoff) {
			continue
		}
		target, err := s.path(e.digest)
		if err == nil {
			err = os.Remove(target)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, e.digest)
	}
	return removed, errors.Join(errs...)
}

type entry struct {
	digest   string
	size     int64
	modified time.Time
}

// entries lists the blobs; anything else in the directory is not the store's.
func (s *Store) entries() ([]entry, error) {
	dirEntries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("list blobs: %w", err)
	}
	out := make([]entry, 0, len(dirEntries))
	for _, d := range dirEntries {
		if !d.Type().IsRegular() || !blob.DigestPattern.MatchString(d.Name()) {
			continue
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list blobs: %w", err)
		}
		out = append(out, entry{digest: d.Name(), size: info.Size(), modified: info.ModTime()})
	}
	return out, nil
}

// path is the only way to a blob's file: a name that is not a digest never
// reaches the filesystem.
func (s *Store) path(digest string) (string, error) {
	if !blob.DigestPattern.MatchString(digest) {
		return "", ErrInvalidDigest
	}
	return filepath.Join(s.dir, digest), nil
}

// syncDir makes a rename durable.
func syncDir(dir string) error {
	d, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
