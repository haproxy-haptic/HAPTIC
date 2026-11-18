// Copyright 2025 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package httpclient

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

// ExpectOK waits for an HTTP 200 response.
func (r *Request) ExpectOK(t *testing.T) *Response {
	t.Helper()
	return r.ExpectStatus(t, http.StatusOK)
}

// ExpectStatus waits for an exact status code.
func (r *Request) ExpectStatus(t *testing.T, code int) *Response {
	t.Helper()
	resp, err := r.poll(t, fmt.Sprintf("%s %s -> %d", r.method, r.url(), code), func(resp *Response) bool {
		return resp.Status == code
	})
	if err != nil {
		t.Fatalf("ExpectStatus(%d): %v", code, err)
	}
	return resp
}

// ExpectHeader waits for a response header containing want.
func (r *Request) ExpectHeader(t *testing.T, name, want string) *Response {
	t.Helper()
	resp, err := r.poll(t, fmt.Sprintf("%s %s header %s contains %q", r.method, r.url(), name, want), func(resp *Response) bool {
		return strings.Contains(resp.Header.Get(name), want)
	})
	if err != nil {
		t.Fatalf("ExpectHeader(%s, %q): %v", name, want, err)
	}
	return resp
}

// ExpectEchoHeader waits for the backend to receive a header containing want.
// A 200 can arrive before the forwarding rules are live.
func (r *Request) ExpectEchoHeader(t *testing.T, name, want string) *Response {
	t.Helper()
	lower := strings.ToLower(name)
	resp, err := r.poll(t, fmt.Sprintf("%s %s echo'd header %s contains %q", r.method, r.url(), name, want),
		func(resp *Response) bool {
			if resp.Status != http.StatusOK || resp.Echo == nil {
				return false
			}
			return strings.Contains(resp.Echo.Headers[lower], want)
		})
	if err != nil {
		t.Fatalf("ExpectEchoHeader(%s, %q): %v", name, want, err)
	}
	return resp
}

// ExpectEchoEnvironment waits for a 200 from the selected backend.
// The default echo backend reports an empty environment.
func (r *Request) ExpectEchoEnvironment(t *testing.T, want string) *Response {
	t.Helper()
	resp, err := r.poll(t, fmt.Sprintf("%s %s echo'd Environment=%q", r.method, r.url(), want),
		func(resp *Response) bool {
			if resp.Status != http.StatusOK || resp.Echo == nil {
				return false
			}
			return resp.Echo.Environment == want
		})
	if err != nil {
		t.Fatalf("ExpectEchoEnvironment(%q): %v", want, err)
	}
	return resp
}

// ExpectEchoPath waits for the backend to receive the rewritten path.
func (r *Request) ExpectEchoPath(t *testing.T, want string) *Response {
	t.Helper()
	resp, err := r.poll(t, fmt.Sprintf("%s %s echo'd Path=%q", r.method, r.url(), want),
		func(resp *Response) bool {
			if resp.Status != http.StatusOK || resp.Echo == nil {
				return false
			}
			return resp.Echo.Path == want
		})
	if err != nil {
		t.Fatalf("ExpectEchoPath(%q): %v", want, err)
	}
	return resp
}

// ExpectMatching waits for one response satisfying the entire predicate.
// The timeout report includes description.
func (r *Request) ExpectMatching(t *testing.T, description string, predicate func(*Response) bool) *Response {
	t.Helper()
	resp, err := r.poll(t, fmt.Sprintf("%s %s: %s", r.method, r.url(), description), predicate)
	if err != nil {
		t.Fatalf("ExpectMatching(%s): %v", description, err)
	}
	return resp
}

// ExpectRedirect waits for a 3xx response with want in the Location header.
func (r *Request) ExpectRedirect(t *testing.T, want string) *Response {
	t.Helper()
	resp, err := r.poll(t, fmt.Sprintf("%s %s redirects to %q", r.method, r.url(), want), func(resp *Response) bool {
		if resp.Status < 300 || resp.Status >= 400 {
			return false
		}
		return strings.Contains(resp.Header.Get("Location"), want)
	})
	if err != nil {
		t.Fatalf("ExpectRedirect(%q): %v", want, err)
	}
	return resp
}

func (r *Request) poll(t *testing.T, description string, predicate func(*Response) bool) (*Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), r.client.waitCfg.Timeout)
	defer cancel()

	var lastResp *Response
	var lastErr error

	err := testutil.WaitForConditionWithDescription(ctx, r.client.waitCfg, description,
		func(ctx context.Context) (bool, error) {
			resp, err := r.Do(ctx)
			if err != nil {
				lastErr = err
				r.client.transport.CloseIdleConnections()
				return false, err
			}
			lastResp = resp
			lastErr = nil
			if predicate(resp) {
				return true, nil
			}
			// An idle connection can still reach the pre-reload worker. Redial for the current routes.
			r.client.transport.CloseIdleConnections()
			return false, fmt.Errorf("status=%d, body=%s", resp.Status, truncate(resp.Body, 200))
		})

	if err == nil {
		return lastResp, nil
	}

	// Capture the failing configuration before test cleanup removes the fixtures.
	if r.client.onPollTimeout != nil {
		r.client.onPollTimeout(t, description, lastResp, lastErr)
	}

	if lastResp != nil {
		return lastResp, fmt.Errorf("%s: last status=%d, last body=%s, err=%w",
			description, lastResp.Status, truncate(lastResp.Body, 500), err)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("%s: never got a response (last err: %v): %w", description, lastErr, err)
	}
	return nil, fmt.Errorf("%s: %w", description, err)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
