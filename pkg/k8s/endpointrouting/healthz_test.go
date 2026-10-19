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

package endpointrouting

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	discoveryv1 "k8s.io/api/discovery/v1"
)

// Recorded from kube-proxy v1.37 in kind: the top-level lastUpdated is the
// current time, the per-family status carries the sync times.
const kubeProxyHealthz = `{"lastUpdated":"2026-10-19T07:52:57.699829784Z","currentTime":"2026-10-19T07:52:57.698078615Z","nodeEligible":true,"healthy":true,"status":{"IPv4":{"lastUpdated":"2026-10-19T07:12:26.873971549Z","healthy":true},"IPv6":{"lastUpdated":"2026-10-19T06:56:37.306856669Z","healthy":true}}}`

func serveHealthz(t *testing.T, status int, body string) (host string, port int) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/healthz", r.URL.Path)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err = strconv.Atoi(portText)
	require.NoError(t, err)
	return host, port
}

func TestHTTPHealthzReadsThePerFamilySyncTimes(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		host, port := serveHealthz(t, status, kubeProxyHealthz)
		health, err := HTTPHealthz(port)(t.Context(), host)
		require.NoError(t, err)
		assert.Equal(t, time.Date(2026, 10, 19, 7, 52, 57, 698078615, time.UTC), health.CurrentTime.UTC())
		assert.Equal(t, time.Date(2026, 10, 19, 7, 12, 26, 873971549, time.UTC), health.Synced[discoveryv1.AddressTypeIPv4].UTC())
		assert.Equal(t, time.Date(2026, 10, 19, 6, 56, 37, 306856669, time.UTC), health.Synced[discoveryv1.AddressTypeIPv6].UTC())
	}
}

func TestHTTPHealthzIgnoresTheTopLevelLastUpdated(t *testing.T) {
	host, port := serveHealthz(t, http.StatusOK, `{"lastUpdated":"2026-10-19T07:52:57Z","currentTime":"2026-10-19T07:52:57Z","healthy":true}`)
	health, err := HTTPHealthz(port)(t.Context(), host)
	require.NoError(t, err)
	assert.Empty(t, health.Synced)
}

func TestHTTPHealthzRejectsAnswersWithoutAClock(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"not found":      {http.StatusNotFound, kubeProxyHealthz},
		"no currentTime": {http.StatusOK, `{"status":{"IPv4":{"lastUpdated":"2026-10-19T07:12:26Z"}}}`},
		"not json":       {http.StatusOK, `ok`},
	} {
		t.Run(name, func(t *testing.T) {
			host, port := serveHealthz(t, tc.status, tc.body)
			_, err := HTTPHealthz(port)(t.Context(), host)
			require.ErrorIs(t, err, ErrNoKubeProxy)
		})
	}
}

func TestHTTPHealthzTellsAMissingKubeProxyFromAStuckOne(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	_, err = HTTPHealthz(port)(t.Context(), "127.0.0.1")
	require.ErrorIs(t, err, ErrNoKubeProxy, "nothing listens")

	stuck, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = stuck.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = HTTPHealthz(stuck.Addr().(*net.TCPAddr).Port)(ctx, "127.0.0.1")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoKubeProxy, "the kernel accepted the connection; the process does not answer")
}
