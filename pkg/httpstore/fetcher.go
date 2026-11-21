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

package httpstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// errNotModified is a sentinel returned by doFetch (and propagated by
// fetchWithRetry) for an HTTP 304 Not Modified response. It is a control
// signal, not a failure: callers use errors.Is to distinguish "the server
// confirmed our cached copy is current" from a genuine fetch error or from a
// 200 OK that legitimately carries empty content. Modelling 304 this way
// (rather than as "empty content") avoids conflating it with a 200 whose body
// is empty and whose response carries no ETag.
var errNotModified = errors.New("http 304 not modified")

// fetchWithRetry performs an HTTP GET with retry logic and conditional request headers.
//
// Parameters:
//   - ctx: Context for cancellation
//   - url: The URL to fetch
//   - opts: Fetch options (timeout, retries)
//   - auth: Optional authentication config
//   - etag: Previous ETag for conditional request (empty to skip)
//   - lastModified: Previous Last-Modified for conditional request (empty to skip)
//
// Returns:
//   - content: The response body
//   - newEtag: The ETag header from response
//   - newLastModified: The Last-Modified header from response
//   - err: errNotModified for a 304 (use errors.Is); otherwise an error if all
//     retries failed, or nil on a 200 OK

func (s *HTTPStore) fetchWithRetry(
	ctx context.Context,
	url string,
	opts FetchOptions,
	auth *AuthConfig,
	etag string,
	lastModified string,
) (content, newEtag, newLastModified string, err error) {
	opts = opts.WithDefaults()

	var lastErr error
	for attempt := 0; attempt <= opts.Retries; attempt++ {
		if attempt > 0 {
			// Wait before retry with exponential backoff
			// Cap the exponent to prevent overflow (max ~32x multiplier)
			exp := min(attempt-1, 5)
			delay := opts.RetryDelay * time.Duration(1<<exp)
			s.logger.Log(context.Background(), levelTrace, "retrying HTTP fetch",
				"url", RedactURL(url),
				"attempt", attempt+1,
				"delay", delay.String())

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return "", "", "", ctx.Err()
			}
		}

		content, newEtag, newLastModified, err = s.doFetch(ctx, url, opts, auth, etag, lastModified)
		if err == nil {
			return content, newEtag, newLastModified, nil
		}

		// 304 Not Modified is a terminal, expected outcome — not a transient
		// failure to retry. Propagate the sentinel so the caller can branch on it.
		if errors.Is(err, errNotModified) {
			return content, newEtag, newLastModified, err
		}

		lastErr = err
		if permanentFetchError(err) {
			s.reportBlobFetchFailure(url, &opts, err)
			return "", "", "", err
		}
		s.logger.Log(context.Background(), levelTrace, "HTTP fetch attempt failed",
			"url", RedactURL(url),
			"attempt", attempt+1,
			"error", err)
	}

	err = fmt.Errorf("all %d retry attempts failed: %w", opts.Retries+1, lastErr)
	if ctx.Err() == nil {
		s.reportBlobFetchFailure(url, &opts, err)
	}
	return "", "", "", err
}

// doFetch performs a single HTTP GET request and returns the response content
// along with cache headers (ETag, Last-Modified) for conditional requests.
func (s *HTTPStore) doFetch(
	ctx context.Context,
	url string,
	opts FetchOptions,
	auth *AuthConfig,
	etag string,
	lastModified string,
) (content, etagResult, lastModifiedResult string, err error) {
	if err := s.requireBlobSink(&opts); err != nil {
		return "", "", "", err
	}

	// Create request with timeout
	reqCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", "", "", fmt.Errorf("creating request: %w", redactURLError(err, url))
	}

	// Add conditional request headers
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}

	// Add authentication headers
	if auth != nil {
		if err := addAuthHeaders(req, auth); err != nil {
			return "", "", "", err
		}
	}

	// Add user agent
	req.Header.Set("User-Agent", "github.com/haproxy-haptic/HAPTIC/1.0")

	// Perform request
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("request failed: %w", redactURLError(err, url))
	}
	defer resp.Body.Close()

	// Extract cache headers
	etagResult = resp.Header.Get("ETag")
	lastModifiedResult = resp.Header.Get("Last-Modified")

	if resp.StatusCode == http.StatusOK || opts.AcceptStatus.Contains(resp.StatusCode) {
		content, err = s.readBody(reqCtx, resp.Body, &opts)
		if err != nil {
			return "", "", "", err
		}
		return content, etagResult, lastModifiedResult, nil
	}

	switch resp.StatusCode {
	case http.StatusNotModified:
		// Content unchanged — preserve the original etag and signal via the
		// errNotModified sentinel (distinct from a 200 OK with an empty body).
		return "", etag, lastModifiedResult, errNotModified

	case http.StatusUnauthorized:
		return "", "", "", errors.New("authentication failed (401 Unauthorized)")

	case http.StatusForbidden:
		return "", "", "", errors.New("access denied (403 Forbidden)")

	case http.StatusNotFound:
		return "", "", "", errors.New("resource not found (404 Not Found)")

	default:
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return "", "", "", fmt.Errorf("client error: %s", resp.Status)
		}
		if resp.StatusCode >= 500 {
			return "", "", "", fmt.Errorf("server error: %s", resp.Status)
		}
		return "", "", "", fmt.Errorf("unexpected status: %s", resp.Status)
	}
}

func readContent(body io.Reader) (string, error) {
	content, err := io.ReadAll(io.LimitReader(body, MaxContentSize+1))
	if err != nil {
		return "", fmt.Errorf("reading response body: %w", err)
	}
	if len(content) > MaxContentSize {
		return "", fmt.Errorf("response body exceeds maximum size of %d bytes", MaxContentSize)
	}
	return string(content), nil
}

// addAuthHeaders adds authentication headers to the request.
func addAuthHeaders(req *http.Request, auth *AuthConfig) error {
	switch auth.Type {
	case AuthTypeBasic:
		if auth.Username != "" || auth.Password != "" {
			credentials := base64.StdEncoding.EncodeToString(
				[]byte(auth.Username + ":" + auth.Password))
			req.Header.Set("Authorization", "Basic "+credentials)
		}

	case AuthTypeBearer:
		if auth.Token != "" {
			req.Header.Set("Authorization", "Bearer "+auth.Token)
		}

	case AuthTypeHeader, "":
		for key, value := range auth.Headers {
			req.Header.Set(key, value)
		}
	default:
		return fmt.Errorf("unknown HTTP authentication type %q; fetch refused; use basic, bearer, or header", auth.Type)
	}
	return nil
}
