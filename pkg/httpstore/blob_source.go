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

package httpstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
)

// A blob source (FetchOptions.Blob, ADR-0041) streams its body into a
// BlobSink. Its content is the encoded blob.Ref, so the reference moves
// through the same pending, accepted, and revocation states as text content.

// BlobSink stores a blob source's body outside the cache.
type BlobSink interface {
	Ingest(ctx context.Context, body io.Reader, spec blob.Spec) (blob.Ref, error)
}

// ErrBlobsDisabled refuses a blob source in a store without a sink.
var ErrBlobsDisabled = errors.New("binary blobs are disabled on this controller; set --blob-dir")

// blobTarget is read by fetches, which run outside the store's mutex.
type blobTarget struct {
	sink   BlobSink
	failed func(url string, err error)
}

// SetBlobSink enables blob sources. failed, if set, learns each blob source
// whose fetch failed after its retries.
func (s *HTTPStore) SetBlobSink(sink BlobSink, failed func(url string, err error)) {
	s.blobs.Store(&blobTarget{sink: sink, failed: failed})
}

func (s *HTTPStore) blobSink() BlobSink {
	if target := s.blobs.Load(); target != nil {
		return target.sink
	}
	return nil
}

// requireBlobSink refuses a blob source before its request is sent.
func (s *HTTPStore) requireBlobSink(opts *FetchOptions) error {
	if opts.Blob.Declared() && s.blobSink() == nil {
		return ErrBlobsDisabled
	}
	return nil
}

// readBody returns a text source's body, or a blob source's encoded reference.
func (s *HTTPStore) readBody(ctx context.Context, body io.Reader, opts *FetchOptions) (string, error) {
	if !opts.Blob.Declared() {
		return readContent(body)
	}
	spec, err := opts.Blob.Spec()
	if err != nil {
		return "", fmt.Errorf("%w: %w", blob.ErrRefused, err)
	}
	ref, err := s.blobSink().Ingest(ctx, body, spec)
	if err != nil {
		return "", err
	}
	return ref.Encode(), nil
}

func (s *HTTPStore) reportBlobFetchFailure(url string, opts *FetchOptions, err error) {
	if !opts.Blob.Declared() {
		return
	}
	if target := s.blobs.Load(); target != nil && target.failed != nil {
		target.failed(url, err)
	}
}

// permanentFetchError reports a failure a retry repeats.
func permanentFetchError(err error) bool {
	return errors.Is(err, blob.ErrRefused) || errors.Is(err, ErrBlobsDisabled)
}

// BlobSource lists the digests one blob source holds.
type BlobSource struct {
	URL      string
	Accepted string
	Pending  string
	// Replaced is the accepted digest an unconfirmed acceptance replaced; a
	// revocation restores it.
	Replaced string
}

// Digests returns the source's distinct non-empty digests.
func (b BlobSource) Digests() []string {
	var digests []string
	for _, digest := range []string{b.Accepted, b.Pending, b.Replaced} {
		if digest != "" && !slices.Contains(digests, digest) {
			digests = append(digests, digest)
		}
	}
	return digests
}

// BlobSources returns every blob source the store holds content for.
func (s *HTTPStore) BlobSources() []BlobSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sources []BlobSource
	for url, entry := range s.cache {
		if !entry.Options.Blob.Declared() {
			continue
		}
		source := BlobSource{URL: url, Accepted: blobDigest(entry.AcceptedContent)}
		if entry.HasPending {
			source.Pending = blobDigest(entry.PendingContent)
		}
		if record := s.acceptances[url]; record != nil && record.previous != nil {
			source.Replaced = blobDigest(record.previous.content)
		}
		sources = append(sources, source)
	}
	slices.SortFunc(sources, func(a, b BlobSource) int { return strings.Compare(a.URL, b.URL) })
	return sources
}

func blobDigest(content string) string {
	ref, ok := blob.DecodeRef(content)
	if !ok {
		return ""
	}
	return ref.Digest
}
