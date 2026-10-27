// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	sharedhttpclient "github.com/haproxy-haptic/HAPTIC/tests/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

func assertClientCertificateResumption(ctx context.Context, t *testing.T, bundle *mTLSBundle, serverCA []byte, host, customCNHeader string) {
	t.Helper()
	certificate, err := tls.X509KeyPair(bundle.ClientCertPEM, bundle.ClientKeyPEM)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(serverCA))
	headers := map[string]string{
		"X-SSL-Client-CN":   leaf.Subject.CommonName,
		"X-SSL-Client-Cert": base64.StdEncoding.EncodeToString(certificate.Certificate[0]),
	}
	if customCNHeader != "" {
		headers[customCNHeader] = leaf.Subject.CommonName
	}
	forward := ForwardService(t, HAProxyDeploymentName, 443)
	for _, version := range []struct {
		name  string
		value uint16
	}{{"TLS1.2", tls.VersionTLS12}, {"TLS1.3", tls.VersionTLS13}} {
		t.Run(version.name, func(t *testing.T) {
			var resumed atomic.Bool
			client := sharedhttpclient.New(&sharedhttpclient.Config{
				Host: "127.0.0.1", HTTPSPort: forward.HTTPSPort,
				TLS: &tls.Config{
					Certificates: []tls.Certificate{certificate}, RootCAs: roots,
					MinVersion: version.value, MaxVersion: version.value,
					ClientSessionCache: tls.NewLRUClientSessionCache(1),
					VerifyConnection: func(state tls.ConnectionState) error {
						resumed.Store(state.DidResume)
						return nil
					},
				},
			})
			t.Cleanup(client.CloseIdleConnections)
			request := func(ctx context.Context) bool {
				client.CloseIdleConnections()
				resumed.Store(false)
				probe := client.HTTPS(host, "/").WithHeader("X-SSL-Client-DN", "forged-client")
				for name := range headers {
					probe.WithHeader(name, "forged-client")
				}
				response, err := probe.Do(ctx)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.Status)
				require.NotNil(t, response.Echo)
				for name, value := range headers {
					require.Equal(t, value, response.Echo.Headers[strings.ToLower(name)], name)
				}
				require.Contains(t, response.Echo.Headers["x-ssl-client-dn"], "CN="+leaf.Subject.CommonName)
				return resumed.Load()
			}
			require.False(t, request(ctx), "the first connection must establish a fresh session")
			require.NoError(t, testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(ctx context.Context) (bool, error) {
				return request(ctx), nil
			}))
			t.Log("client-certificate headers verified on a resumed TLS session")
		})
	}
}
