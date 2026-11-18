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

// Package httpclient provides HTTP, HTTPS, and mTLS assertions for explicit test endpoints.
package httpclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

// Client is a fluent HTTP client targeting an explicit test endpoint.
// One Client may serve many Requests; concurrent use is safe.
type Client struct {
	endpointHost string

	httpPort  int
	httpsPort int

	// waitCfg is the retry/backoff policy applied to Expect* sinks.
	waitCfg testutil.WaitConfig

	// transport is shared across all non-mTLS requests for connection pooling.
	transport *http.Transport

	// Capture failure evidence before the test removes its fixtures.
	onPollTimeout PollTimeoutSnapshot
}

// PollTimeoutSnapshot captures evidence on timeout and must let the caller report the failure.
// lastResp is nil if no request succeeded. lastErr is nil if the last request returned a response.
type PollTimeoutSnapshot func(t *testing.T, description string, lastResp *Response, lastErr error)

type Config struct {
	Host          string
	HTTPPort      int
	HTTPSPort     int
	TLS           *tls.Config
	OnPollTimeout PollTimeoutSnapshot
}

// New dials the explicit endpoint while preserving the requested host and TLS SNI.
func New(config *Config) *Client {
	transport := newSharedTransport(config.Host, config.HTTPSPort)
	if config.TLS != nil {
		transport.TLSClientConfig = config.TLS.Clone()
	}
	return &Client{
		endpointHost: config.Host, httpPort: config.HTTPPort, httpsPort: config.HTTPSPort,
		waitCfg:   testutil.WaitConfig{InitialInterval: 100 * time.Millisecond, MaxInterval: 2 * time.Second, Timeout: 15 * time.Second, Multiplier: 2},
		transport: transport, onPollTimeout: config.OnPollTimeout,
	}
}

// CloseIdleConnections lets the next request reach a new HAProxy worker after a reload.
func (c *Client) CloseIdleConnections() { c.transport.CloseIdleConnections() }

// Dial the explicit endpoint while keeping the requested host for TLS SNI.
func newSharedTransport(endpointHost string, httpsPort int) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	target := net.JoinHostPort(endpointHost, strconv.Itoa(httpsPort))
	return &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if strings.HasSuffix(address, ":443") {
				return dialer.DialContext(ctx, network, target)
			}
			return dialer.DialContext(ctx, network, address)
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          16,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
}

// Each mTLS request gets a transport with its own client identity and CA roots.
func transportForClientCert(endpointHost string, httpsPort int, clientCert *tls.Certificate, ca []byte) (*http.Transport, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("failed to parse CA PEM")
	}
	t := newSharedTransport(endpointHost, httpsPort)
	t.TLSClientConfig = &tls.Config{
		Certificates: []tls.Certificate{*clientCert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}
	return t, nil
}
