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
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Request builds a single HTTP exchange for Do or a retrying Expect assertion.
type Request struct {
	client *Client

	scheme     string
	host       string
	hostHeader string // overrides the Host header; host still sets the TLS SNI
	path       string

	method  string
	headers http.Header
	body    []byte
	chunked bool

	basicAuth *basicAuth
	mtls      *mtlsConfig
}

type basicAuth struct {
	user, password string
}

type mtlsConfig struct {
	cert tls.Certificate
	ca   []byte
}

// Response captures one HTTP exchange.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	Echo   *EchoBody // nil for unrecognized echo-server responses
}

// GET builds an HTTP request with the given Host header.
func (c *Client) GET(host, path string) *Request {
	return c.newRequest("http", host, path)
}

// HTTPS builds a GET using host for both TLS SNI and the Host header.
func (c *Client) HTTPS(host, path string) *Request {
	return c.newRequest("https", host, path)
}

func (c *Client) newRequest(scheme, host, path string) *Request {
	return &Request{
		client:  c,
		scheme:  scheme,
		host:    host,
		path:    normalizePath(path),
		method:  http.MethodGet,
		headers: http.Header{},
	}
}

// WithHostHeader sends host as the Host header while the TLS SNI keeps the
// request's original host.
func (r *Request) WithHostHeader(host string) *Request {
	r.hostHeader = host
	return r
}

// WithMethod overrides the HTTP method (default GET).
func (r *Request) WithMethod(method string) *Request {
	r.method = method
	return r
}

// WithHeader adds a header. Subsequent calls with the same name append.
func (r *Request) WithHeader(name, value string) *Request {
	r.headers.Add(name, value)
	return r
}

// WithBody sets a fixed-length body and leaves the method and Content-Type unchanged.
func (r *Request) WithBody(body string) *Request {
	r.body = []byte(body)
	r.chunked = false
	return r
}

// WithChunkedBody sends an HTTP/1.1 chunked body of unknown length.
func (r *Request) WithChunkedBody(body string) *Request {
	r.body = []byte(body)
	r.chunked = true
	return r
}

// WithBasicAuth attaches HTTP Basic credentials.
func (r *Request) WithBasicAuth(user, password string) *Request {
	r.basicAuth = &basicAuth{user, password}
	return r
}

// WithClientCert supplies a client identity and CA roots for this request.
func (r *Request) WithClientCert(certPEM, keyPEM, caPEM []byte) *Request {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		r.mtls = &mtlsConfig{ca: caPEM}
		return r
	}
	r.mtls = &mtlsConfig{cert: cert, ca: caPEM}
	return r
}

// Do executes one request.
func (r *Request) Do(ctx context.Context) (*Response, error) {
	url := r.url()
	var requestBody io.Reader
	if r.body != nil {
		if r.chunked {
			requestBody = io.LimitReader(bytes.NewReader(r.body), int64(len(r.body)))
		} else {
			requestBody = bytes.NewReader(r.body)
		}
	}
	req, err := http.NewRequestWithContext(ctx, r.method, url, requestBody)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Host = r.host
	if r.hostHeader != "" {
		req.Host = r.hostHeader
	}
	if r.chunked {
		req.ContentLength = -1
		req.TransferEncoding = []string{"chunked"}
	}
	for k, vs := range r.headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if r.basicAuth != nil {
		req.SetBasicAuth(r.basicAuth.user, r.basicAuth.password)
	}

	transport := r.client.transport
	if r.mtls != nil {
		t, err := transportForClientCert(r.client.endpointHost, r.client.httpsPort, &r.mtls.cert, r.mtls.ca)
		if err != nil {
			return nil, fmt.Errorf("build mTLS transport: %w", err)
		}
		defer t.CloseIdleConnections()
		transport = t
	}

	httpClient := &http.Client{
		Transport: transport,
		// Don't follow redirects automatically — tests assert on Location.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 10 * time.Second,
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return &Response{
		Status: resp.StatusCode,
		Header: resp.Header.Clone(),
		Body:   body,
		Echo:   parseEchoBody(body),
	}, nil
}

func (r *Request) url() string {
	switch r.scheme {
	case "https":
		return "https://" + r.host + r.path
	default:
		return "http://" + net.JoinHostPort(r.client.endpointHost, strconv.Itoa(r.client.httpPort)) + r.path
	}
}

func normalizePath(path string) string {
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}
