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

package pluggablevalidator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// DefaultTimeout matches the hub-side default per-request timeout so
// operators see consistent latency budgets across both ends of the
// wire.
const DefaultTimeout = 5 * time.Second

// DefaultMaxConnections is the per-validator pool ceiling when
// `spec.validators[i].maxConnections` is omitted. Sized to match
// typical reconciliation-burst concurrency (a handful of in-flight
// validations) without holding many idle file descriptors per
// validator.
const DefaultMaxConnections = 4

// idleClose is the period after which a checked-in connection is
// closed and not returned to the pool. Mirrors HTTP keep-alive
// timeouts and stays well under the validator's typical idle close
// (60s) so the controller proactively reaps before the validator
// closes underneath it.
const idleClose = 30 * time.Second

var errClientClosed = errors.New("validator client is closed")

// Client speaks the HAPTIC validator wire protocol over a unix
// socket. One Client maps to one configured validator: one socket
// path, one timeout budget, one connection pool. Clients are safe
// for concurrent use; the pool serialises within-connection traffic
// and parallelises across connections.
//
// Pool semantics (adaptive):
//   - Start small (zero connections; lazy open on first use).
//   - Grow on contention up to MaxConnections (acquires that find
//     no free connection and have headroom open a fresh one).
//   - Shrink on idleness (connections checked back into the pool
//     past `idleClose` are closed instead of returned, so the pool
//     deflates when traffic dies down).
//   - Connections that error on read/write are discarded (poisoned)
//     and replaced lazily on next acquire.
type Client struct {
	// Name is the operator-facing validator name from
	// spec.validators[i].name. Surfaced in synthetic protocol-level
	// diagnostics so users can identify which validator failed.
	Name string

	// SocketPath is the absolute filesystem path to the validator's
	// unix domain socket.
	SocketPath string

	// Timeout is the per-call deadline for one request-response
	// cycle (acquire + write + read). Zero falls back to
	// DefaultTimeout.
	Timeout time.Duration

	// MaxConnections caps the pool size. Zero falls back to
	// DefaultMaxConnections; values < 1 are clamped up to 1.
	MaxConnections int

	// dialer is overridable in tests. nil means the default unix
	// dialer.
	dialer func(ctx context.Context, path string) (net.Conn, error)

	mu       sync.Mutex
	cond     *sync.Cond    // broadcast on every release/discard so waiters wake without polling
	idle     []*pooledConn // free connections
	inFlight int           // checked-out + in-progress dial count
	closed   bool
}

type pooledConn struct {
	conn   net.Conn
	parked time.Time // when it was last checked back in (for idle close)
}

// NewClient builds a Client for the given validator socket.
// `timeout <= 0` falls back to DefaultTimeout; `maxConnections <= 0`
// falls back to DefaultMaxConnections.
func NewClient(name, socketPath string, timeout time.Duration, maxConnections int) *Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if maxConnections <= 0 {
		maxConnections = DefaultMaxConnections
	}
	c := &Client{
		Name:           name,
		SocketPath:     socketPath,
		Timeout:        timeout,
		MaxConnections: maxConnections,
	}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Validate sends one request frame and reads one response frame on a
// pooled connection. Returns the decoded Response or, on transport /
// protocol failure, a synthetic ProtocolError Response.
//
// Validate never returns a non-nil error alongside a non-nil
// Response: the caller treats every transport failure as a
// protocol-level error diagnostic. The error return is reserved for
// caller-supplied invariant violations (nil request, etc.) where the
// failure is "you misused this API" rather than "the validator is
// unreachable".
func (c *Client) Validate(ctx context.Context, req *Request) (*Response, error) {
	if req == nil {
		return nil, errors.New("validator client: nil request")
	}
	if cause := context.Cause(ctx); cause != nil {
		return canceledResponse(c.Name, cause), nil
	}

	deadline := time.Now().Add(c.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// Two attempts max: the first on a connection from acquire (which may be
	// reused or freshly dialed), the second always on a forced-fresh dial. A
	// reused connection that fails on encode/decode may simply have been
	// idle-closed by the validator between our last use and now, so it earns
	// one retry; a fresh connection that fails is the real failure mode and is
	// surfaced directly. Connect/set-deadline failures never retry.
	resp, retry := c.attempt(callCtx, req, deadline, 0)
	if retry {
		resp, _ = c.attempt(callCtx, req, deadline, 1)
	}
	return resp, nil
}

// attempt performs one acquire/send/receive round-trip. On success it returns
// the validator's Response and retry=false. On transport failure it returns a
// ProtocolError Response to surface, and retry=true only when a *reused*
// connection (attempt 0, !fresh) failed on encode/decode — the idle-close
// signal that earns a single fresh-dial retry. Connect/set-deadline failures
// never retry.
func (c *Client) attempt(callCtx context.Context, req *Request, deadline time.Time, n int) (result *Response, retry bool) {
	if cause := context.Cause(callCtx); cause != nil {
		return canceledResponse(c.Name, cause), false
	}

	var (
		conn  net.Conn
		fresh bool
		err   error
	)
	if n == 0 {
		conn, fresh, err = c.acquire(callCtx)
	} else {
		conn, fresh, err = c.dialFresh(callCtx)
	}
	if err != nil {
		return unavailableResponse(fmt.Sprintf(
			"validator %q: connect %s%s: %v", c.Name, c.SocketPath, retrySuffix(n), err,
		)), false
	}
	if cause := context.Cause(callCtx); cause != nil {
		c.discard(conn)
		return canceledResponse(c.Name, cause), false
	}

	if err := conn.SetDeadline(deadline); err != nil {
		c.discard(conn)
		return unavailableResponse(fmt.Sprintf(
			"validator %q: set deadline%s: %v", c.Name, retrySuffix(n), err,
		)), false
	}
	stopCancellation := armConnectionCancellation(callCtx, conn)
	if cause := context.Cause(callCtx); cause != nil {
		stopCancellation()
		c.discard(conn)
		return canceledResponse(c.Name, cause), false
	}

	// A reused connection (first attempt, not freshly dialed) may have been
	// idle-closed; its encode/decode failure earns one retry.
	canRetry := n == 0 && !fresh

	if _, err := EncodeRequest(conn, req); err != nil {
		stopCancellation()
		c.discard(conn)
		if cause := context.Cause(callCtx); cause != nil {
			return canceledResponse(c.Name, cause), false
		}
		message := fmt.Sprintf("validator %q: encode request%s: %v", c.Name, retrySuffix(n), err)
		if _, ok := errors.AsType[*net.OpError](err); ok {
			return unavailableResponse(message), canRetry
		}
		return ProtocolError(message), false
	}

	resp, err := DecodeResponse(conn)
	if err != nil {
		stopCancellation()
		c.discard(conn)
		if cause := context.Cause(callCtx); cause != nil {
			return canceledResponse(c.Name, cause), false
		}
		return unavailableResponse(fmt.Sprintf(
			"validator %q: decode response%s: %v", c.Name, retrySuffix(n), err,
		)), canRetry
	}

	stopCancellation()
	if cause := context.Cause(callCtx); cause != nil {
		c.discard(conn)
		return canceledResponse(c.Name, cause), false
	}
	c.release(conn)
	return resp, false
}

func armConnectionCancellation(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
		close(done)
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			if !stop() {
				<-done
			}
		})
	}
}

func unavailableResponse(message string) *Response {
	resp := ProtocolError(message)
	resp.transportFailure = true
	return resp
}

func canceledResponse(validatorName string, cause error) *Response {
	return unavailableResponse(fmt.Sprintf(
		"validator %q: validation did not finish: %v; retry the request",
		validatorName,
		cause,
	))
}

// retrySuffix returns " (retry)" for retry attempts (attempt > 0) and the
// empty string for the first attempt, so transport-error diagnostics carry
// the same "(retry)" marker the two-phase send/receive used before being
// collapsed into one loop.
func retrySuffix(attempt int) string {
	if attempt > 0 {
		return " (retry)"
	}
	return ""
}

// acquire returns a pooled connection (reusing a free one if any,
// dialing a fresh one if there's pool headroom, or blocking on
// cond.Wait until one is released). The bool indicates whether the
// connection is fresh (just dialed) — Validate uses this to decide
// whether to retry on first-use failure (reused connections may have
// been idle-closed by the validator and deserve one retry; fresh
// connections that fail are the real failure mode).
//
// Context cancellation broadcasts on the cond so waiters can return.
func (c *Client) acquire(ctx context.Context) (net.Conn, bool, error) {
	// Wire ctx cancellation into the cond: when ctx fires, broadcast
	// so any goroutine in cond.Wait below wakes up and re-checks.
	stop := c.armCancelWatcher(ctx)
	defer stop()

	c.mu.Lock()
	defer c.mu.Unlock()

	for {
		if c.closed {
			return nil, false, errClientClosed
		}
		if cause := context.Cause(ctx); cause != nil {
			return nil, false, fmt.Errorf("acquire: %w", cause)
		}
		// Reuse any free, non-stale connection first.
		if pc := c.popIdleLocked(); pc != nil {
			c.inFlight++
			return pc.conn, false, nil
		}
		// Open a fresh connection if there's headroom.
		if c.inFlight < c.MaxConnections {
			c.inFlight++
			c.mu.Unlock()
			conn, dialErr := c.dial(ctx)
			c.mu.Lock()
			conn, err := c.finishDialLocked(ctx, conn, dialErr, "acquire")
			return conn, true, err
		}
		// Wait for release/discard or cancellation broadcast.
		c.cond.Wait()
	}
}

func (c *Client) finishDialLocked(
	ctx context.Context,
	conn net.Conn,
	dialErr error,
	operation string,
) (net.Conn, error) {
	if dialErr != nil {
		c.inFlight--
		c.cond.Signal()
		if conn != nil {
			_ = conn.Close()
		}
		if err := clientContextError(ctx, operation); err != nil {
			return nil, err
		}
		return nil, dialErr
	}
	if err := clientContextError(ctx, operation); err != nil {
		c.inFlight--
		c.cond.Broadcast()
		_ = conn.Close()
		return nil, err
	}
	if c.closed {
		c.inFlight--
		c.cond.Broadcast()
		_ = conn.Close()
		return nil, errClientClosed
	}
	return conn, nil
}

func clientContextError(ctx context.Context, operation string) error {
	if cause := context.Cause(ctx); cause != nil {
		return fmt.Errorf("%s: %w", operation, cause)
	}
	return nil
}

// popIdleLocked removes and returns the most recently parked
// non-stale idle connection, or nil when the pool is empty (or every
// remaining entry is stale and got closed). Caller MUST hold c.mu.
func (c *Client) popIdleLocked() *pooledConn {
	for len(c.idle) > 0 {
		idx := len(c.idle) - 1
		pc := c.idle[idx]
		c.idle = c.idle[:idx]
		if time.Since(pc.parked) > idleClose {
			// Stale — close and try the next one. Stale entries
			// don't count against inFlight; they were released.
			_ = pc.conn.Close()
			continue
		}
		return pc
	}
	return nil
}

// armCancelWatcher wakes pool waiters on cancellation and returns a join function.
func (c *Client) armCancelWatcher(ctx context.Context) func() {
	if ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	var finishOnce sync.Once
	finish := func() {
		finishOnce.Do(func() { close(done) })
	}
	stop := context.AfterFunc(ctx, func() {
		defer finish()
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	return func() {
		if stop() {
			finish()
		}
		<-done
	}
}

// dialFresh always opens a new connection, bypassing the pool's
// reuse logic. Used by Validate's retry attempt. Counts toward
// inFlight so the pool's MaxConnections cap is honored even during
// retries. Blocks on cond.Wait when at cap, just like acquire — no
// busy polling.
func (c *Client) dialFresh(ctx context.Context) (net.Conn, bool, error) {
	stop := c.armCancelWatcher(ctx)
	defer stop()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, false, errClientClosed
	}
	for c.inFlight >= c.MaxConnections {
		if c.closed {
			c.mu.Unlock()
			return nil, false, errClientClosed
		}
		if cause := context.Cause(ctx); cause != nil {
			c.mu.Unlock()
			return nil, false, fmt.Errorf("dialFresh: %w", cause)
		}
		c.cond.Wait()
	}
	if cause := context.Cause(ctx); cause != nil {
		c.mu.Unlock()
		return nil, false, fmt.Errorf("dialFresh: %w", cause)
	}
	c.inFlight++
	c.mu.Unlock()

	conn, err := c.dial(ctx)
	c.mu.Lock()
	conn, err = c.finishDialLocked(ctx, conn, err, "dialFresh")
	c.mu.Unlock()
	return conn, true, err
}

// release returns a healthy connection to the pool. The connection
// gets a fresh `parked` timestamp; subsequent acquires will reap it
// if it sits idle past idleClose. Signals the cond so any acquirer
// blocked at the pool cap wakes up.
func (c *Client) release(conn net.Conn) {
	// Drop the deadline before returning to the pool — the next
	// caller will set its own.
	_ = conn.SetDeadline(time.Time{})
	c.mu.Lock()
	c.inFlight--
	if c.closed {
		c.cond.Broadcast()
		c.mu.Unlock()
		_ = conn.Close()
		return
	}
	c.idle = append(c.idle, &pooledConn{conn: conn, parked: time.Now()})
	c.cond.Signal()
	c.mu.Unlock()
}

// discard closes a poisoned connection without returning it to the
// pool. inFlight counter is decremented so a subsequent acquire can
// open a replacement. Signals the cond so any acquirer blocked at
// the pool cap wakes up.
func (c *Client) discard(conn net.Conn) {
	_ = conn.Close()
	c.mu.Lock()
	c.inFlight--
	c.cond.Signal()
	c.mu.Unlock()
}

// dial returns a unix-socket connection. Test code may override
// Client.dialer to redirect through an in-process listener.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	if c.dialer != nil {
		return c.dialer(ctx, c.SocketPath)
	}
	d := net.Dialer{}
	return d.DialContext(ctx, "unix", c.SocketPath)
}

// Close drains the pool and shuts every idle connection. Safe to
// call repeatedly; the pool is unusable after the first close.
// Used during iteration teardown.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	conns := c.idle
	c.idle = nil
	c.cond.Broadcast()
	c.mu.Unlock()
	for _, pc := range conns {
		_ = pc.conn.Close()
	}
}

// HealthCheck verifies a validator socket is reachable and accepting
// connections. Returns nil on success or a wrapped error describing
// the failure.
//
// The check is intentionally lightweight (sub-ms in the happy path)
// so it can run on every Kubernetes liveness/readiness probe
// interval (default 10s). It does NOT exercise the protocol — a
// malformed validator that accepts connections but produces garbage
// will pass HealthCheck. Catching that needs a deeper round-trip
// probe, which is out of scope for /healthz.
//
// On Linux, `os.OpenFile` against a stream unix socket fails with
// ENXIO regardless of the socket's state, so we use a short
// non-blocking dial as the readiness check instead. Stat + mode
// check still rules out the regular-file case before paying the
// dial cost.
func HealthCheck(socketPath string) error {
	info, err := os.Stat(socketPath)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("path is not a unix socket (mode=%s)", info.Mode())
	}
	d := net.Dialer{Timeout: 100 * time.Millisecond}
	conn, err := d.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	_ = conn.Close()
	return nil
}
