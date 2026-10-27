// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/transportsecurity/tlstest"
)

func TestExplicitEndpointPreservesSNIAndVerifiesCertificate(t *testing.T) {
	const hostname = "route.example.test"
	ca := tlstest.NewAuthority(t, "route CA")
	identity := tlstest.NewIdentity(t, ca, hostname, x509.ExtKeyUsageServerAuth)
	certificate, err := tls.X509KeyPair(identity.Certificate, identity.Key)
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s %s %s", r.TLS.ServerName, r.Host, r.URL.Path)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	for _, mode := range []string{"trusted", "wrong hostname", "untrusted"} {
		t.Run(mode, func(t *testing.T) {
			roots := x509.NewCertPool()
			if mode != "untrusted" {
				require.True(t, roots.AppendCertsFromPEM(ca.PEM))
			}
			client := New(&Config{Host: "127.0.0.1", HTTPSPort: port, TLS: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}})
			t.Cleanup(client.CloseIdleConnections)
			host := hostname
			if mode == "wrong hostname" {
				host = "other.example.test"
			}
			response, err := client.HTTPS(host, "/upgrade-check").Do(t.Context())
			if mode == "trusted" {
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.Status)
				require.Equal(t, hostname+" "+hostname+" /upgrade-check", string(response.Body))
				response, err = client.HTTPS(host, "/upgrade-check").WithHostHeader("other.example.test").Do(t.Context())
				require.NoError(t, err)
				require.Equal(t, hostname+" other.example.test /upgrade-check", string(response.Body))
			} else {
				require.Error(t, err)
			}
		})
	}
}
