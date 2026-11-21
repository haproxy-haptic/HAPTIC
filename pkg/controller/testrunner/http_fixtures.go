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

package testrunner

import (
	"fmt"
	"log/slog"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
	ctrlhttpstore "github.com/haproxy-haptic/HAPTIC/pkg/controller/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
)

// FixtureHTTPStoreWrapper wraps an HTTPStore pre-populated with fixtures for test execution.
//
// Unlike the production HTTPStoreWrapper, this wrapper:
//   - Returns content only for URLs that have fixtures loaded
//   - Returns an error for URLs without fixtures (ensuring all HTTP dependencies are mocked)
//   - Does NOT make actual HTTP requests
//
// Template usage:
//
//	{{ http.Fetch("http://blocklist.example.com/list.txt") }}
//
// If the URL is not in fixtures, the template fails with a clear error message.
type FixtureHTTPStoreWrapper struct {
	store   *httpstore.HTTPStore
	pending map[string]bool
	logger  *slog.Logger
}

// NewFixtureHTTPStoreWrapper creates a new fixture-only HTTP wrapper.
//
// Parameters:
//   - store: HTTPStore pre-populated with fixtures via LoadFixture()
//   - logger: Logger for debug messages
func NewFixtureHTTPStoreWrapper(store *httpstore.HTTPStore, logger *slog.Logger) *FixtureHTTPStoreWrapper {
	return &FixtureHTTPStoreWrapper{
		store:  store,
		logger: logger.With("component", "fixture-http-wrapper"),
	}
}

// WithPending marks the fixtures declared pending: Fetch returns empty
// content for them and Pending reports true.
func (w *FixtureHTTPStoreWrapper) WithPending(fixtures []config.HTTPResourceFixture) *FixtureHTTPStoreWrapper {
	for _, fixture := range fixtures {
		if fixture.Pending {
			if w.pending == nil {
				w.pending = make(map[string]bool)
			}
			w.pending[fixture.URL] = true
		}
	}
	return w
}

// Pending reports a fixture declared pending.
func (w *FixtureHTTPStoreWrapper) Pending(url string) bool { return w.pending[url] }

// Fetch returns fixture content for a URL.
//
// Options and authentication are validated exactly as the production
// wrapper validates them, so an invalid declaration fails the test, but
// only the URL selects the fixture; nothing is sent anywhere.
func (w *FixtureHTTPStoreWrapper) Fetch(args ...any) (any, error) {
	url, opts, auth, err := ctrlhttpstore.ParseFetchArgs(args)
	if err != nil {
		return nil, err
	}
	if _, err := httpstore.DescribeSource(opts, auth); err != nil {
		return nil, fmt.Errorf("http.Fetch: %w", err)
	}
	if w.pending[url] {
		return "", nil
	}

	content, ok := w.store.Get(url)
	if !ok {
		return nil, fmt.Errorf("http.Fetch: no fixture defined for URL: %s (add an httpResources fixture for this URL)", httpstore.RedactURL(url))
	}

	w.logger.Debug("Returning fixture content",
		"url", httpstore.RedactURL(url),
		"size", len(content))

	if opts.Blob.Declared() {
		return fixtureBlob(content, opts.Blob)
	}
	return content, nil
}

// fixtureBlob treats the fixture as the stored blob, after any unpacking.
func fixtureBlob(content string, key blob.Key) (string, error) {
	spec, err := key.Spec()
	if err != nil {
		return "", err
	}
	ref := blob.Describe([]byte(content))
	if spec.SHA256 != "" && ref.Digest != spec.SHA256 {
		return "", nil
	}
	return ref.Encode(), nil
}

// CreateHTTPStoreFromFixtures creates an HTTPStore pre-populated with fixture content.
//
// Parameters:
//   - fixtures: HTTP fixtures from test definition
//   - logger: Logger for debug messages
//
// Returns:
//   - HTTPStore with fixtures loaded as accepted content
func CreateHTTPStoreFromFixtures(fixtures []config.HTTPResourceFixture, logger *slog.Logger) *httpstore.HTTPStore {
	store := httpstore.New(logger, 0) // 0 = no eviction for test fixtures

	for _, fixture := range fixtures {
		if fixture.Pending {
			continue
		}
		store.LoadFixture(fixture.URL, fixture.Content)
		logger.Debug("Loaded HTTP fixture",
			"url", httpstore.RedactURL(fixture.URL),
			"size", len(fixture.Content))
	}

	return store
}

// MergeHTTPFixtures merges global and test-specific HTTP fixtures.
//
// Test-specific fixtures override global fixtures for the same URL.
//
// Parameters:
//   - globalFixtures: HTTP fixtures from _global test
//   - testFixtures: HTTP fixtures from specific test
//
// Returns:
//   - Merged HTTP fixtures list
func MergeHTTPFixtures(globalFixtures, testFixtures []config.HTTPResourceFixture) []config.HTTPResourceFixture {
	// Use map to deduplicate by URL (test fixtures override global)
	fixtureMap := make(map[string]config.HTTPResourceFixture)

	// Add global fixtures first
	for _, f := range globalFixtures {
		fixtureMap[f.URL] = f
	}

	// Add test fixtures (override global for same URL)
	for _, f := range testFixtures {
		fixtureMap[f.URL] = f
	}

	// Convert back to slice
	result := make([]config.HTTPResourceFixture, 0, len(fixtureMap))
	for _, f := range fixtureMap {
		result = append(result, f)
	}

	return result
}
