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

// Package client is the controller's end of the HAPTIC agent wire contract
// (pkg/dataplane/agent/api): one pooled keep-alive HTTP client per pod, a
// state read and a streaming multipart apply.
//
// Pure: no controller, templating or Kubernetes imports. Every limit the
// contract states is asserted here as well as in the agent, so a malformed
// apply never reaches the wire.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/transportsecurity"
)

const (
	defaultTimeout      = 10 * time.Second
	defaultApplyTimeout = 2 * time.Minute
	dialTimeout         = 5 * time.Second
	idleConnTimeout     = 90 * time.Second
)

// Config configures one agent endpoint. Zero fields take the contract's
// defaults; BaseURL is the only required one.
type Config struct {
	BaseURL  string
	Username string
	Password string
	TLS      *transportsecurity.Source
	// UnixSocket uses local filesystem access instead of network authentication.
	UnixSocket string
	// Timeout bounds a State call, PerPodApplyTimeout an Apply call.
	Timeout            time.Duration
	PerPodApplyTimeout time.Duration
	// ConnectRetries and ConnectRetryBackoff cover the master's re-exec
	// window; nothing but a refused or reset connection is ever retried.
	ConnectRetries      int
	ConnectRetryBackoff time.Duration
}

// Client talks to one agent.
type Client struct {
	baseURL      string
	username     string
	password     string
	timeout      time.Duration
	applyTimeout time.Duration
	retries      int
	backoff      time.Duration
	http         *http.Client
}

// New validates cfg and builds a client with its own connection pool. cfg is
// copied; the caller may reuse it for the next pod.
func New(cfg *Config) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("agent client: Config is required")
	}
	base, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	transport, err := configuredTransport(cfg, base)
	if err != nil {
		return nil, err
	}
	c := &Client{
		baseURL:      base,
		username:     cfg.Username,
		password:     cfg.Password,
		timeout:      cfg.Timeout,
		applyTimeout: cfg.PerPodApplyTimeout,
		retries:      cfg.ConnectRetries,
		backoff:      cfg.ConnectRetryBackoff,
		http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
	if cfg.TLS != nil || cfg.UnixSocket != "" {
		c.username, c.password = "", ""
	}
	if c.timeout <= 0 {
		c.timeout = defaultTimeout
	}
	if c.applyTimeout <= 0 {
		c.applyTimeout = defaultApplyTimeout
	}
	if c.retries <= 0 {
		c.retries = api.ConnectRetries
	}
	if c.backoff <= 0 {
		c.backoff = api.ConnectRetryBackoffMs * time.Millisecond
	}
	return c, nil
}

func configuredTransport(cfg *Config, base string) (http.RoundTripper, error) {
	transport := newTransport()
	if cfg.UnixSocket != "" {
		if cfg.TLS != nil || base != "http://localhost" {
			return nil, errors.New("agent client: UnixSocket requires BaseURL http://localhost without TLS")
		}
		socket := cfg.UnixSocket
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "unix", socket)
		}
		return transport, nil
	}
	if cfg.TLS == nil {
		return transport, nil
	}
	if !strings.HasPrefix(base, "https://") {
		return nil, errors.New("agent client: TLS requires an HTTPS BaseURL")
	}
	return transportsecurity.NewTransport(cfg.TLS, transport), nil
}

func normalizeBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("agent client: BaseURL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("agent client: BaseURL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("agent client: BaseURL %q must be http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("agent client: BaseURL %q has no host", raw)
	}
	return strings.TrimSuffix(raw, "/"), nil
}

func newTransport() *http.Transport {
	// No Proxy: the controller dials pod IPs directly, and an inherited
	// HTTPS_PROXY would divert every apply to a proxy that cannot reach them.
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     false,
	}
}

// Close releases the pooled connections.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

// Health checks process liveness independently of the HAProxy worker.
func (c *Client) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	_, err := c.roundTrip(ctx, func(ctx context.Context) (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+api.PathHealthz, http.NoBody)
	}, replayableAlways)
	return err
}

// State reads the agent's baseline. verify makes the agent re-hash its tree
// so the reported digests are observations rather than its last-known set.
func (c *Client) State(ctx context.Context, read api.StateRead) (*api.State, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	target := c.baseURL + api.PathState + read.Query()
	build := func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
		if err != nil {
			return nil, err
		}
		c.authorize(req)
		return req, nil
	}

	body, err := c.roundTrip(ctx, build, replayableAlways)
	if err != nil {
		return nil, err
	}
	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, fmt.Errorf("agent client: decode state: %w", err)
	}
	return &state, nil
}

// Apply streams one apply: the manifest part first, the optional zstd plan
// blob, then one part per file whose content the agent lacks, named by its
// manifest path.
//
// A NACK is a successful call with ApplyResult.OK false — the agent's verdict,
// not a transport failure. A baseline mismatch returns *ConflictError, missing
// file parts *MissingError, missing required blobs *MissingBlobsError.
func (c *Client) Apply(ctx context.Context, m *api.Manifest, parts map[string]io.Reader, plan io.Reader) (*api.ApplyResult, error) {
	if m == nil {
		return nil, errors.New("agent client: manifest is required")
	}
	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("agent client: encode manifest: %w", err)
	}
	if err := validateApply(m, parts, len(manifestJSON)); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.applyTimeout)
	defer cancel()

	src := newApplySource(m, manifestJSON, parts, plan)
	build := func(ctx context.Context) (*http.Request, error) {
		body, contentType := src.open()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+api.PathApply, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		c.authorize(req)
		return req, nil
	}

	body, err := c.roundTrip(ctx, build, src.replayable)
	if err != nil {
		return nil, err
	}
	var result api.ApplyResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("agent client: decode apply result: %w", err)
	}
	return &result, nil
}

// PutPlan stores the blob of the plan the pod applied under (planID, proof),
// after the apply that landed it. ErrPlanMoved means the pod has applied
// another plan since and dropped the blob; ErrNoPlanEndpoint means the agent
// predates the call, so the blob has to ride the next apply.
func (c *Client) PutPlan(ctx context.Context, planID, proof string, blob []byte) error {
	if planID == "" || proof == "" {
		return errors.New("agent client: plan id and proof are required")
	}
	if len(blob) == 0 || len(blob) > api.MaxPlanBlobBytes {
		return fmt.Errorf("agent client: plan blob of %d bytes is outside 1..%d", len(blob), api.MaxPlanBlobBytes)
	}
	ctx, cancel := context.WithTimeout(ctx, c.applyTimeout)
	defer cancel()

	query := url.Values{"plan_id": {planID}, "proof": {proof}}
	build := func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+api.PathPlan+"?"+query.Encode(), bytes.NewReader(blob))
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(len(blob))
		req.Header.Set("Content-Type", "application/octet-stream")
		c.authorize(req)
		return req, nil
	}
	_, err := c.roundTrip(ctx, build, replayableAlways)
	var conflict *ConflictError
	if errors.As(err, &conflict) {
		return ErrPlanMoved
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.Status {
		case http.StatusConflict:
			return ErrPlanMoved
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			return ErrNoPlanEndpoint
		}
	}
	return err
}

func (c *Client) authorize(req *http.Request) {
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
}

func replayableAlways() bool { return true }

// roundTrip sends the request, retrying only a refused or reset connection
// and only while the request body can still be produced again.
func (c *Client) roundTrip(ctx context.Context, build func(context.Context) (*http.Request, error), replayable func() bool) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			if err := wait(ctx, c.backoff); err != nil {
				return nil, err
			}
		}
		req, err := build(ctx)
		if err != nil {
			return nil, err
		}
		body, err := c.attempt(req)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !isConnectError(err) || !replayable() {
			return nil, err
		}
	}
	return nil, fmt.Errorf("agent client: %d connect attempts failed: %w", c.retries+1, lastErr)
}

func (c *Client) attempt(req *http.Request) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return readResponse(resp)
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func readResponse(resp *http.Response) ([]byte, error) {
	// /v1/state carries the opaque plan blob, so the apply ceiling is also the
	// largest legitimate response on this wire.
	body, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxApplyBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("agent client: read response: %w", err)
	}
	if len(body) > api.MaxApplyBodyBytes {
		return nil, fmt.Errorf("agent client: response exceeds %d bytes", api.MaxApplyBodyBytes)
	}
	if resp.StatusCode == http.StatusOK {
		return body, nil
	}
	return nil, statusError(resp.StatusCode, body)
}
