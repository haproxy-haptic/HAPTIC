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

// Package blobstore keeps binary blobs on the controller's disk, named by
// their SHA-256 digest (ADR-0041). It is a pure component: the HTTP store
// streams response bodies into it, and the controller's event adapter decides
// which digests to retain.
package blobstore

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
)

// OrphanGrace protects a blob that no holder references yet: a render-local
// candidate is ingested before the render that accepts it publishes it.
const OrphanGrace = time.Hour

const (
	ingestPattern = ".ingest-*"
	ingestPrefix  = ".ingest-"
	blobFileMode  = 0o444
)

// Store holds immutable blobs in one directory.
type Store struct {
	dir     string
	maxSize int64
	logger  *slog.Logger
	now     func() time.Time
	started time.Time

	mu         sync.Mutex
	ingested   map[string]time.Time
	holders    map[uint64]func() []string
	nextHolder uint64
}

// Option configures a Store.
type Option func(*Store)

// WithClock replaces the wall clock that ages orphans.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New opens a store in dir. maxSize caps every blob, whatever its declaration asks.
func New(dir string, maxSize int64, logger *slog.Logger, options ...Option) (*Store, error) {
	if dir == "" {
		return nil, errors.New("blob directory is empty")
	}
	if maxSize <= 0 {
		return nil, fmt.Errorf("blob max size %d must be positive", maxSize)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating blob directory: %w", err)
	}
	store := &Store{
		dir:      dir,
		maxSize:  maxSize,
		logger:   logger.With("component", "blob-store"),
		now:      time.Now,
		ingested: make(map[string]time.Time),
		holders:  make(map[uint64]func() []string),
	}
	for _, option := range options {
		option(store)
	}
	store.started = store.now()
	return store, nil
}

// MaxSize is the cap no declaration can raise.
func (s *Store) MaxSize() int64 {
	return s.maxSize
}

// Ingest streams body into the store as spec declares and returns the blob's
// reference. Refused content (size cap, digest pin, missing member) wraps
// blob.ErrRefused; nothing of it is kept.
func (s *Store) Ingest(ctx context.Context, body io.Reader, spec blob.Spec) (blob.Ref, error) {
	if err := spec.Validate(); err != nil {
		return blob.Ref{}, fmt.Errorf("%w: %w", blob.ErrRefused, err)
	}
	limit := s.limit(spec)
	temporary, err := os.CreateTemp(s.dir, ingestPattern)
	if err != nil {
		return blob.Ref{}, fmt.Errorf("creating blob file: %w", err)
	}
	kept := false
	defer func() {
		if !kept {
			_ = temporary.Close()
			_ = os.Remove(temporary.Name())
		}
	}()
	digest := sha256.New()
	size, err := unpack(ctx, io.MultiWriter(temporary, digest), body, spec, limit)
	if err != nil {
		return blob.Ref{}, err
	}
	ref := blob.Ref{Digest: hex.EncodeToString(digest.Sum(nil)), Size: size}
	if spec.SHA256 != "" && ref.Digest != spec.SHA256 {
		return blob.Ref{}, fmt.Errorf("%w: sha256 is %s, the declaration pins %s", blob.ErrRefused, ref.Digest, spec.SHA256)
	}
	if err := temporary.Sync(); err != nil {
		return blob.Ref{}, fmt.Errorf("writing blob file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return blob.Ref{}, fmt.Errorf("writing blob file: %w", err)
	}
	if err := os.Chmod(temporary.Name(), blobFileMode); err != nil {
		return blob.Ref{}, fmt.Errorf("sealing blob file: %w", err)
	}
	// Retain decides and removes under s.mu, so it never deletes this file
	// between its rename and its ingest time.
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(temporary.Name(), s.path(ref.Digest)); err != nil {
		return blob.Ref{}, fmt.Errorf("storing blob file: %w", err)
	}
	kept = true
	s.ingested[ref.Digest] = s.now()
	return ref, nil
}

func (s *Store) limit(spec blob.Spec) int64 {
	limit := spec.MaxSize
	if limit == 0 {
		limit = blob.DefaultMaxSize
	}
	return min(limit, s.maxSize)
}

// unpack copies at most limit unpacked bytes of body to w. For tar.gz the
// limit covers the archive up to the end of the member.
func unpack(ctx context.Context, w io.Writer, body io.Reader, spec blob.Spec, limit int64) (int64, error) {
	if spec.Unpack == blob.UnpackNone {
		return copyLimited(ctx, w, body, limit)
	}
	decompressed, err := gzip.NewReader(body)
	if err != nil {
		return 0, fmt.Errorf("%w: body is not gzip: %w", blob.ErrRefused, err)
	}
	defer decompressed.Close()
	if spec.Unpack == blob.UnpackGzip {
		return copyLimited(ctx, w, decompressed, limit)
	}
	archive := &countingReader{reader: decompressed, remaining: limit, limit: limit}
	member := blob.CleanMember(spec.Member)
	entries := tar.NewReader(archive)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("%w: archive has no member %q", blob.ErrRefused, spec.Member)
		}
		if err != nil {
			return 0, archive.refusal(fmt.Errorf("reading archive: %w", err))
		}
		if blob.CleanMember(header.Name) != member {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return 0, fmt.Errorf("%w: archive member %q is not a regular file", blob.ErrRefused, spec.Member)
		}
		size, err := copyLimited(ctx, w, entries, archive.remaining)
		return size, archive.refusal(err)
	}
}

func copyLimited(ctx context.Context, w io.Writer, r io.Reader, limit int64) (int64, error) {
	n, err := io.Copy(w, &contextReader{ctx: ctx, reader: io.LimitReader(r, limit+1)})
	if err != nil {
		return 0, fmt.Errorf("reading blob: %w", err)
	}
	if n > limit {
		return 0, tooLarge(limit)
	}
	return n, nil
}

func tooLarge(limit int64) error {
	return fmt.Errorf("%w: blob exceeds its size limit of %d bytes", blob.ErrRefused, limit)
}

// countingReader stops an archive at the remaining byte budget.
type countingReader struct {
	reader    io.Reader
	remaining int64
	limit     int64
	exceeded  bool
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		r.exceeded = true
		return 0, tooLarge(r.limit)
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func (r *countingReader) refusal(err error) error {
	if err != nil && r.exceeded {
		return tooLarge(r.limit)
	}
	return err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// Open opens a held blob for streaming.
func (s *Store) Open(digest string) (*os.File, error) {
	if !blob.ValidDigest(digest) {
		return nil, fmt.Errorf("blob digest %q is invalid", digest)
	}
	return os.Open(s.path(digest))
}

// Has reports whether the store holds digest.
func (s *Store) Has(digest string) bool {
	if !blob.ValidDigest(digest) {
		return false
	}
	_, err := os.Stat(s.path(digest))
	return err == nil
}

func (s *Store) path(digest string) string {
	return filepath.Join(s.dir, digest)
}

// AddHolder registers digests that must stay; the returned function removes it.
func (s *Store) AddHolder(digests func() []string) (remove func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextHolder
	s.nextHolder++
	s.holders[id] = digests
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.holders, id)
	}
}

// Retain deletes every blob that neither a holder nor inUse references, once
// it is older than OrphanGrace, and returns the deleted digests.
func (s *Store) Retain(inUse []string) ([]string, error) {
	keep := s.kept(inUse)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("listing blobs: %w", err)
	}
	var removed []string
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() {
			continue
		}
		switch {
		case blob.ValidDigest(name):
			if _, kept := keep[name]; kept {
				continue
			}
			deleted, err := s.removeExpiredOrphan(name)
			if err != nil {
				errs = append(errs, err)
			} else if deleted {
				removed = append(removed, name)
			}
		case strings.HasPrefix(name, ingestPrefix) && s.leftoverExpired(entry):
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	if len(removed) > 0 {
		s.logger.Info("Removed unreferenced blobs", "count", len(removed))
	}
	return removed, errors.Join(errs...)
}

func (s *Store) kept(inUse []string) map[string]struct{} {
	s.mu.Lock()
	holders := make([]func() []string, 0, len(s.holders))
	for _, holder := range s.holders {
		holders = append(holders, holder)
	}
	s.mu.Unlock()
	keep := make(map[string]struct{}, len(inUse))
	for _, digest := range inUse {
		keep[digest] = struct{}{}
	}
	for _, holder := range holders {
		for _, digest := range holder() {
			keep[digest] = struct{}{}
		}
	}
	return keep
}

// removeExpiredOrphan deletes an unheld blob once its grace has passed. A blob
// found on disk at start gets the grace from the start, since the process that
// ingested it is gone.
func (s *Store) removeExpiredOrphan(digest string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	since, ingested := s.ingested[digest]
	if !ingested {
		since = s.started
	}
	if s.now().Sub(since) < OrphanGrace {
		return false, nil
	}
	if err := os.Remove(s.path(digest)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	delete(s.ingested, digest)
	return true, nil
}

func (s *Store) leftoverExpired(entry fs.DirEntry) bool {
	info, err := entry.Info()
	return err == nil && s.now().Sub(info.ModTime()) >= OrphanGrace
}
