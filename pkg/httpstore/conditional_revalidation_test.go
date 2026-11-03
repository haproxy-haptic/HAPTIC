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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRejectedRefreshIsFetchedAgainBeforeAcceptance(t *testing.T) {
	for _, validator := range []string{"ETag", "Last-Modified"} {
		for _, verdict := range []struct {
			name     string
			finalize func(*HTTPStore, string, string, uint64) bool
		}{
			{"reject", (*HTTPStore).RejectPendingVersion},
			{"discard", (*HTTPStore).DiscardPendingVersion},
		} {
			t.Run(validator+"/"+verdict.name, func(t *testing.T) {
				var version atomic.Int32
				version.Store(1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					current := version.Load()
					tag := fmt.Sprintf(`"v%d"`, current)
					requestHeader := "If-None-Match"
					if validator == "Last-Modified" {
						tag = time.Unix(int64(current), 0).UTC().Format(http.TimeFormat)
						requestHeader = "If-Modified-Since"
					}
					w.Header().Set(validator, tag)
					if r.Header.Get(requestHeader) == tag {
						w.WriteHeader(http.StatusNotModified)
						return
					}
					fmt.Fprintf(w, "v%d", current)
				}))
				defer server.Close()
				store := New(nil, 0)
				content, err := store.Fetch(t.Context(), server.URL, FetchOptions{Delay: time.Minute}, nil)
				require.NoError(t, err)
				require.Equal(t, "v1", content)
				version.Store(2)
				first, err := store.RefreshURLVersion(t.Context(), server.URL)
				require.NoError(t, err)
				require.NotNil(t, first)
				require.NotEmpty(t, first.Checksum)
				require.True(t, verdict.finalize(store, server.URL, first.Checksum, first.Revision))
				second, err := store.RefreshURLVersion(t.Context(), server.URL)
				require.NoError(t, err)
				require.NotNil(t, second, "a discarded body must pass validation again")
				require.NotEmpty(t, second.Checksum)
				require.Greater(t, second.Revision, first.Revision)
				accepted, ok := store.Get(server.URL)
				require.True(t, ok)
				require.Equal(t, "v1", accepted)
				require.True(t, store.PromotePendingVersion(server.URL, second.Checksum, second.Revision))
				unchanged, err := store.RefreshURLVersion(t.Context(), server.URL)
				require.NoError(t, err)
				require.Nil(t, unchanged)
			})
		}
	}
}
