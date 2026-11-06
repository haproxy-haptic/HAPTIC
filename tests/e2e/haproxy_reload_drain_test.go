//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/e2ecluster"
	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

func TestHAProxyReloadDrainsHTTPConnections(t *testing.T) {
	const host = "reload-drain.localdev.me"
	feature := features.New("HAProxy reload drains existing HTTP connections").
		Assess("idle connections finish their next request and long requests complete", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			cs, err := newClientsetForE2E(client.RESTConfig())
			require.NoError(t, err)
			namespace := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, namespace)
			backend := NewEchoServerBackend(ctx, t, client, namespace)
			secretName := NewTLSSecret(ctx, t, client, namespace, "tls", []string{host})
			secret, err := cs.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
			require.NoError(t, err)
			roots := x509.NewCertPool()
			require.True(t, roots.AppendCertsFromPEM(secret.Data["tls.crt"]))
			NewIngress(ctx, t, client, namespace, &IngressSpec{
				Name: "echo", Host: host, BackendService: backend.Service, BackendPort: backend.Port,
				TLSSecretName: secretName,
			})
			httpclient.New(t).GET(host, "/").ExpectOK(t)
			baseline := waitForQuietFleet(ctx, t, cs)
			idle := openIdleHTTPConnections(ctx, t, host, roots)
			long := dialRetainedHTTP(ctx, t, host, roots, false)
			long.send(t, "/?echo_time=30000")
			backendPrefix := namespace + "_echo_svc_"
			require.NoError(t, testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(ctx context.Context) (bool, error) {
				return backendHasActiveRequest(ctx, baseline, backendPrefix)
			}))
			started := time.Now()
			body := []byte(`{"metadata":{"annotations":{"haproxy-haptic.org/response-set-header":"X-Reload-Drain completed"}}}`)
			_, err = cs.NetworkingV1().Ingresses(namespace).Patch(ctx, "echo", types.MergePatchType, body, metav1.PatchOptions{})
			require.NoError(t, err)
			require.NoError(t, testutil.WaitForCondition(ctx, testutil.WaitConfig{
				InitialInterval: 100 * time.Millisecond, MaxInterval: time.Second, Timeout: 10 * time.Second, Multiplier: 1.5,
			}, func(ctx context.Context) (bool, error) {
				current, err := haproxyWorkerStartTimesE(ctx, cs)
				if err != nil {
					return false, err
				}
				for pod, previous := range baseline {
					if current[pod] <= previous {
						return false, nil
					}
				}
				return true, nil
			}))
			t.Logf("all workers reloaded after %s", time.Since(started))
			t.Run("idle HTTP and HTTPS connections", func(t *testing.T) {
				for _, connection := range idle {
					connection.send(t, "/")
					closes := connection.read(t)
					require.True(t, closes, "retiring worker must announce connection closure")
				}
				t.Logf("%d idle connections reused without retries", len(idle))
			})
			t.Run("active 30-second request", func(t *testing.T) {
				long.read(t)
				t.Log("30-second request completed across a structural reload")
			})
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}

type retainedHTTP struct {
	conn   net.Conn
	reader *bufio.Reader
	host   string
}

func dialRetainedHTTP(ctx context.Context, t *testing.T, host string, roots *x509.CertPool, encrypted bool) *retainedHTTP {
	t.Helper()
	endpoint, err := e2ecluster.ResolveTrafficEndpoint()
	require.NoError(t, err)
	var conn net.Conn
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	if encrypted {
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{
			MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: host, NextProtos: []string{"http/1.1"},
		}}
		conn, err = tlsDialer.DialContext(ctx, "tcp", net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.HTTPSPort)))
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.HTTPPort)))
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(45*time.Second)))
	return &retainedHTTP{conn: conn, reader: bufio.NewReader(conn), host: host}
}

func (c *retainedHTTP) send(t *testing.T, path string) {
	t.Helper()
	_, err := fmt.Fprintf(c.conn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, c.host)
	require.NoError(t, err)
}

func (c *retainedHTTP) read(t *testing.T) bool {
	t.Helper()
	response, err := http.ReadResponse(c.reader, nil)
	require.NoError(t, err)
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	return response.Close
}

func openIdleHTTPConnections(ctx context.Context, t *testing.T, host string, roots *x509.CertPool) []*retainedHTTP {
	t.Helper()
	connections := make([]*retainedHTTP, 0, 16)
	for _, encrypted := range []bool{false, true} {
		for range 8 {
			conn := dialRetainedHTTP(ctx, t, host, roots, encrypted)
			conn.send(t, "/")
			require.False(t, conn.read(t), "baseline must allow connection reuse")
			connections = append(connections, conn)
		}
	}
	return connections
}

func backendHasActiveRequest(ctx context.Context, pods map[string]float64, backendPrefix string) (bool, error) {
	for pod := range pods {
		body, err := apiProxyGet(ctx, pod, HAProxyStatsPort, "metrics")
		if err != nil {
			return false, err
		}
		for line := range strings.SplitSeq(body, "\n") {
			if !strings.HasPrefix(line, "haproxy_backend_current_sessions{") || !strings.Contains(line, `proxy="`+backendPrefix) {
				continue
			}
			fields := strings.Fields(line)
			value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				return false, err
			}
			if value > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}
