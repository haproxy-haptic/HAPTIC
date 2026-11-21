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

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

// blobTimeout bounds one blob transfer; the agent allows the same.
const blobTimeout = 10 * time.Minute

// ErrNoBlobEndpoint is PutBlob's answer from an agent without blobs: one that
// predates them or runs without --blob-dir.
var ErrNoBlobEndpoint = errors.New("agent client: the agent does not accept blobs")

// ErrBlobNotHeld is GetBlob's answer when the agent does not hold the blob.
var ErrBlobNotHeld = errors.New("agent client: the agent does not hold the blob")

// ErrBlobDigestMismatch is what a GetBlob reader returns at the end of content
// that does not hash to the digest it was asked for.
var ErrBlobDigestMismatch = errors.New("agent client: blob content does not match its SHA-256")

// PutBlob streams size bytes from r to the agent as the blob named digest. The
// agent verifies the digest and keeps nothing on a mismatch. r is read once; a
// refused connection is retried only while nothing of it was read.
func (c *Client) PutBlob(ctx context.Context, digest string, size int64, r io.Reader) error {
	if !blob.ValidDigest(digest) {
		return fmt.Errorf("agent client: blob name %q is not a lowercase hex SHA-256", digest)
	}
	if size < 0 || r == nil {
		return errors.New("agent client: a blob needs a reader and a non-negative size")
	}
	ctx, cancel := context.WithTimeout(ctx, blobTimeout)
	defer cancel()

	body := &onceReader{reader: r}
	build := func(ctx context.Context) (*http.Request, error) {
		var content io.Reader = body
		if size == 0 {
			content = http.NoBody
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+api.BlobPath(digest), content)
		if err != nil {
			return nil, err
		}
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
		c.authorize(req)
		return req, nil
	}
	_, err := c.roundTrip(ctx, build, func() bool { return !body.read })
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && (httpErr.Status == http.StatusNotFound || httpErr.Status == http.StatusMethodNotAllowed) {
		return ErrNoBlobEndpoint
	}
	return err
}

// onceReader records whether the request consumed any of the caller's reader.
type onceReader struct {
	reader io.Reader
	read   bool
}

func (o *onceReader) Read(p []byte) (int, error) {
	n, err := o.reader.Read(p)
	if n > 0 {
		o.read = true
	}
	return n, err
}

// GetBlob opens the blob named digest on the agent and returns its content and
// size. The reader returns ErrBlobDigestMismatch instead of io.EOF when the
// content does not hash to digest; the caller closes it.
func (c *Client) GetBlob(ctx context.Context, digest string) (io.ReadCloser, int64, error) {
	if !blob.ValidDigest(digest) {
		return nil, 0, fmt.Errorf("agent client: blob name %q is not a lowercase hex SHA-256", digest)
	}
	ctx, cancel := context.WithTimeout(ctx, blobTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+api.BlobPath(digest), http.NoBody)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		_, err := readResponse(resp)
		_ = resp.Body.Close()
		cancel()
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
			return nil, 0, ErrBlobNotHeld
		}
		return nil, 0, err
	}
	return &verifiedBlob{body: resp.Body, cancel: cancel, want: digest, summer: sha256.New()}, resp.ContentLength, nil
}

// verifiedBlob hashes the content as the caller reads it.
type verifiedBlob struct {
	body   io.ReadCloser
	cancel context.CancelFunc
	want   string
	summer hash.Hash
}

func (v *verifiedBlob) Read(p []byte) (int, error) {
	n, err := v.body.Read(p)
	_, _ = v.summer.Write(p[:n])
	if errors.Is(err, io.EOF) {
		if got := hex.EncodeToString(v.summer.Sum(nil)); got != v.want {
			return n, fmt.Errorf("%w: content hashes to %s", ErrBlobDigestMismatch, got)
		}
	}
	return n, err
}

func (v *verifiedBlob) Close() error {
	defer v.cancel()
	return v.body.Close()
}
