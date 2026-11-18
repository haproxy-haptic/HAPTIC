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

package httpclient

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

type connGenKey struct{}

// Each connection retains the route generation it first saw, as a draining HAProxy worker does.
func TestPollRetriesOnFreshConnection(t *testing.T) {
	var gen atomic.Int64
	var conns atomic.Int64

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer gen.CompareAndSwap(0, 1)
		if g, _ := r.Context().Value(connGenKey{}).(int64); g < 1 {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		conns.Add(1)
		return context.WithValue(ctx, connGenKey{}, gen.Load())
	}
	srv.Start()
	t.Cleanup(srv.Close)
	port, err := strconv.Atoi(srv.URL[len("http://127.0.0.1:"):])
	if err != nil {
		t.Fatal(err)
	}

	c := New(&Config{Host: "127.0.0.1", HTTPPort: port})
	c.waitCfg = testutil.WaitConfig{
		Timeout:         5 * time.Second,
		InitialInterval: 20 * time.Millisecond,
		MaxInterval:     50 * time.Millisecond,
		Multiplier:      1,
	}

	c.GET("pinned.example.test", "/").ExpectOK(t)

	if got := conns.Load(); got < 2 {
		t.Fatalf("poll succeeded over %d connection(s); convergence requires a re-dial (old connection can only 404)", got)
	}
}
