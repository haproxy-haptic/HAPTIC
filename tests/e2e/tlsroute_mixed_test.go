// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

const mixedTLSPort = 9743

type mixedTLSFixture struct {
	client             klient.Client
	namespace          string
	terminate          string
	passthrough        string
	plain              BackendRef
	encrypted          BackendRef
	port               int
	listenerPort       int
	certificate        []byte
	backendCertificate []byte
	validation         map[string]any
	catchall           bool
}

func TestTLSRouteMixedTerminationLifecycle(t *testing.T) {
	feature := features.New("TLSRoute mixed termination lifecycle").
		Assess("mode transitions and certificate rotation retain each listener's transport", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			f := newMixedTLSFixture(ctx, t, cfg)
			f.applyGateway(ctx, t, false)
			f.applyRoute(ctx, t, "terminated", f.terminate, "terminate", f.plain)
			f.waitListeners(ctx, t, false)
			f.port = ForwardGateway(ctx, t, f.namespace, "mixed", mixedTLSPort).Ports[mixedTLSPort]
			f.assertTerminated(ctx, t)
			f.assertALPN(ctx, t)

			f.applyGateway(ctx, t, true)
			f.applyRoute(ctx, t, "encrypted", "*."+f.namespace+".localdev.me", "passthrough", f.encrypted)
			f.waitListeners(ctx, t, true)
			f.assertClientAddress(ctx, t)
			f.assertTerminated(ctx, t)
			f.assertALPN(ctx, t)
			f.assertPassthrough(ctx, t)
			f.assertUppercaseSNI(ctx, t)
			f.assertRejected(ctx, t, "outside.localdev.me")
			f.assertRejected(ctx, t, "")

			f.rotateCertificate(ctx, t)
			f.removeAndRestoreCertificate(ctx, t)
			f.assertTerminated(ctx, t)
			f.assertPassthrough(ctx, t)
			f.deleteTerminatingRoute(ctx, t)
			f.waitTerminatingRejection(ctx, t)
			f.assertPassthrough(ctx, t)
			f.applyRoute(ctx, t, "terminated", f.terminate, "terminate", f.plain)
			f.assertTerminated(ctx, t)
			f.assertPassthroughOnlyTransitions(ctx, t)

			f.applyGateway(ctx, t, false)
			f.waitListeners(ctx, t, false)
			f.assertTerminated(ctx, t)
			f.assertALPN(ctx, t)
			f.assertUppercaseSNI(ctx, t)
			f.assertRejected(ctx, t, f.passthrough)
			f.applyGateway(ctx, t, true)
			f.waitListeners(ctx, t, true)
			f.assertTerminated(ctx, t)
			f.assertPassthrough(ctx, t)
			f.catchall = true
			f.applyGateway(ctx, t, true)
			f.waitListeners(ctx, t, true)
			f.assertTerminated(ctx, t)
			f.assertPassthrough(ctx, t)
			f.assertRejected(ctx, t, "")
			f.assertRejected(ctx, t, "outside.localdev.me")
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}

func newMixedTLSFixture(ctx context.Context, t *testing.T, cfg *envconf.Config) *mixedTLSFixture {
	t.Helper()
	client, err := cfg.NewClient()
	require.NoError(t, err)
	namespace := NamespaceForTest(ctx, t, client)
	DumpLogsOnFailure(t, namespace)
	f := &mixedTLSFixture{client: client, namespace: namespace, listenerPort: mixedTLSPort,
		terminate: "terminate." + namespace + ".localdev.me", passthrough: "encrypted." + namespace + ".localdev.me"}
	f.plain = NewEchoServerBackend(ctx, t, client, namespace)
	f.encrypted = NewTLSResponderBackend(ctx, t, client, namespace, "encrypted", f.passthrough)
	NewTLSSecret(ctx, t, client, namespace, "terminate-cert", []string{f.terminate})
	f.certificate = f.secretCertificate(ctx, t, "terminate-cert", "tls.crt")
	f.backendCertificate = f.secretCertificate(ctx, t, "encrypted-tls", "tls.pem")
	return f
}

func (f *mixedTLSFixture) secretCertificate(ctx context.Context, t *testing.T, name, key string) []byte {
	t.Helper()
	secret := &corev1.Secret{}
	require.NoError(t, f.client.Resources(f.namespace).Get(ctx, name, f.namespace, secret))
	block, _ := pem.Decode(secret.Data[key])
	require.NotNil(t, block)
	require.Equal(t, "CERTIFICATE", block.Type)
	return block.Bytes
}

func (f *mixedTLSFixture) listeners(mixed bool) []any {
	listeners := []any{map[string]any{
		"name": "terminate", "protocol": "TLS", "port": f.listenerPort, "hostname": f.terminate,
		"tls": map[string]any{"mode": "Terminate", "certificateRefs": []any{map[string]any{"name": "terminate-cert"}}},
	}}
	if mixed {
		listeners = append(listeners, map[string]any{
			"name": "passthrough", "protocol": "TLS", "port": f.listenerPort,
			"hostname": "*." + f.namespace + ".localdev.me", "tls": map[string]any{"mode": "Passthrough"},
		})
	}
	if f.catchall {
		delete(listeners[0].(map[string]any), "hostname")
		if mixed {
			listeners[1].(map[string]any)["hostname"] = f.passthrough
		}
	}
	return listeners
}

func (f *mixedTLSFixture) applyGateway(ctx context.Context, t *testing.T, mixed bool) {
	t.Helper()
	f.applyListeners(ctx, t, f.listeners(mixed))
}

func (f *mixedTLSFixture) applyListeners(ctx context.Context, t *testing.T, listeners []any) {
	t.Helper()
	spec := map[string]any{"gatewayClassName": gatewayClassName, "listeners": listeners}
	if f.validation != nil {
		spec["tls"] = map[string]any{"frontend": map[string]any{"default": map[string]any{"validation": f.validation}}}
	}
	f.apply(ctx, t, "Gateway", "mixed", spec)
}

func (f *mixedTLSFixture) apply(ctx context.Context, t *testing.T, kind, name string, spec map[string]any) {
	t.Helper()
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": f.namespace}, "spec": spec,
	})
	require.NoError(t, err)
	require.NoError(t, kubectlApplyStdin(ctx, manifest))
}

func (f *mixedTLSFixture) applyRoute(ctx context.Context, t *testing.T, name, host, listener string, backend BackendRef) {
	t.Helper()
	spec := map[string]any{
		"parentRefs": []any{map[string]any{"name": "mixed", "sectionName": listener}}, "hostnames": []any{host},
		"rules": []any{map[string]any{"backendRefs": []any{map[string]any{"name": backend.Service, "port": backend.Port}}}},
	}
	f.apply(ctx, t, "TLSRoute", name, spec)
	waitForRouteDeployed(ctx, t, f.client, tlsRouteGVR, f.namespace, name)
}

func (f *mixedTLSFixture) waitListeners(ctx context.Context, t *testing.T, mixed bool) {
	t.Helper()
	count := 1
	if mixed {
		count = 2
	}
	waitForResourceDeployed(ctx, t, f.client, gatewayGVR, f.namespace, "mixed", func(gateway *unstructured.Unstructured) (bool, string) {
		listeners, _, err := unstructured.NestedSlice(gateway.Object, "status", "listeners")
		if err != nil || len(listeners) != count {
			return false, "waiting for TLS listeners"
		}
		for _, raw := range listeners {
			listener, ok := raw.(map[string]any)
			if !ok || !gatewayMTLSListenerConditions(listener, gateway.GetGeneration(), true) {
				return false, "TLS listener conditions have not converged"
			}
		}
		return true, ""
	})
}

func (f *mixedTLSFixture) probe(ctx context.Context, host string) (body, certificate []byte, err error) {
	return f.probeClient(ctx, host, nil)
}

func (f *mixedTLSFixture) probeClient(ctx context.Context, host string, clientCertificate *tls.Certificate) (body, certificate []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dialer := tls.Dialer{Config: &tls.Config{
		MinVersion: tls.VersionTLS12, ServerName: host,
		InsecureSkipVerify: true, // #nosec G402 -- Every successful probe pins the exact fixture certificate.
	}}
	if clientCertificate != nil {
		dialer.Config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return clientCertificate, nil }
	}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port)))
	if err != nil {
		return nil, nil, err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, nil, err
	}
	certificate = connection.(*tls.Conn).ConnectionState().PeerCertificates[0].Raw
	if _, err := fmt.Fprintf(connection, "GET /mixed-tls HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host); err != nil {
		return nil, certificate, err
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		return nil, certificate, err
	}
	defer response.Body.Close()
	body, err = io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err == nil && response.StatusCode != http.StatusOK {
		err = fmt.Errorf("HTTP status %d", response.StatusCode)
	}
	return body, certificate, err
}

func (f *mixedTLSFixture) assertTerminated(ctx context.Context, t *testing.T) {
	t.Helper()
	f.assertTerminatedHost(ctx, t, f.terminate)
}

func (f *mixedTLSFixture) assertTerminatedHost(ctx context.Context, t *testing.T, host string) {
	t.Helper()
	body, certificate, err := f.probe(ctx, host)
	require.NoError(t, err)
	require.Equal(t, f.certificate, certificate, "terminating listener certificate")
	require.Contains(t, string(body), "mixed-tls")
	require.NotEqual(t, "encrypted", strings.TrimSpace(string(body)))
}

func (f *mixedTLSFixture) assertPassthrough(ctx context.Context, t *testing.T) {
	t.Helper()
	f.assertEncryptedHost(ctx, t, f.passthrough)
}

func (f *mixedTLSFixture) assertEncryptedHost(ctx context.Context, t *testing.T, host string) {
	t.Helper()
	body, certificate, err := f.probe(ctx, host)
	require.NoError(t, err)
	require.Equal(t, f.backendCertificate, certificate, "passthrough must preserve the backend certificate")
	require.Equal(t, "encrypted", strings.TrimSpace(string(body)))
}

func (f *mixedTLSFixture) assertRejected(ctx context.Context, t *testing.T, host string) {
	t.Helper()
	body, _, err := f.probe(ctx, host)
	require.Error(t, err, "unclaimed SNI %q returned %q", host, body)
}

func (f *mixedTLSFixture) rotateCertificate(ctx context.Context, t *testing.T) {
	t.Helper()
	certificate, key, err := generateSelfSignedCert([]string{f.terminate})
	require.NoError(t, err)
	secret := &corev1.Secret{}
	require.NoError(t, f.client.Resources(f.namespace).Get(ctx, "terminate-cert", f.namespace, secret))
	secret.Data = map[string][]byte{"tls.crt": certificate, "tls.key": key}
	require.NoError(t, f.client.Resources(f.namespace).Update(ctx, secret))
	block, _ := pem.Decode(certificate)
	require.NotNil(t, block)
	f.certificate = block.Bytes
	require.NoError(t, testutil.WaitForConditionWithDescription(ctx, testutil.SlowWaitConfig(), "mixed TLS certificate rotation", func(ctx context.Context) (bool, error) {
		_, served, err := f.probe(ctx, f.terminate)
		return err == nil && bytes.Equal(served, f.certificate), nil
	}))
}

func (f *mixedTLSFixture) deleteTerminatingRoute(ctx context.Context, t *testing.T) {
	t.Helper()
	client, err := dynamic.NewForConfig(f.client.RESTConfig())
	require.NoError(t, err)
	require.NoError(t, client.Resource(tlsRouteGVR).Namespace(f.namespace).Delete(ctx, "terminated", metav1.DeleteOptions{}))
	waitForResourceDeployed(ctx, t, f.client, gatewayGVR, f.namespace, "mixed", func(gateway *unstructured.Unstructured) (bool, string) {
		listeners, _, err := unstructured.NestedSlice(gateway.Object, "status", "listeners")
		if err != nil {
			return false, err.Error()
		}
		for _, raw := range listeners {
			listener, ok := raw.(map[string]any)
			if !ok || listener["name"] != "terminate" {
				continue
			}
			attached, _, err := unstructured.NestedInt64(listener, "attachedRoutes")
			return err == nil && attached == 0 && gatewayMTLSListenerConditions(listener, gateway.GetGeneration(), true), "waiting for route detachment"
		}
		return false, "terminating listener missing"
	})
}

func TestTLSRouteMixedClientValidation(t *testing.T) {
	feature := features.New("TLSRoute mixed termination client validation").
		Assess("client validation applies only to the terminating TLS listener", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			f := newMixedTLSFixture(ctx, t, cfg)
			f.applyGateway(ctx, t, true)
			f.applyRoute(ctx, t, "terminated", f.terminate, "terminate", f.plain)
			f.applyRoute(ctx, t, "encrypted", "*."+f.namespace+".localdev.me", "passthrough", f.encrypted)
			f.waitListeners(ctx, t, true)
			f.port = ForwardGateway(ctx, t, f.namespace, "mixed", mixedTLSPort).Ports[mixedTLSPort]
			f.assertTerminated(ctx, t)
			f.assertPassthrough(ctx, t)
			bundle, err := generateMTLSBundle(f.terminate)
			require.NoError(t, err)
			ca := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "client-ca", Namespace: f.namespace}, Data: map[string]string{"ca.crt": string(bundle.CACertPEM)}}
			require.NoError(t, f.client.Resources(f.namespace).Create(ctx, ca))
			f.validation = gatewayMTLSValidation("client-ca")
			f.applyGateway(ctx, t, true)
			f.waitListeners(ctx, t, true)
			f.assertRejected(ctx, t, f.terminate)
			f.assertPassthrough(ctx, t)
			valid, err := tls.X509KeyPair(bundle.ClientCertPEM, bundle.ClientKeyPEM)
			require.NoError(t, err)
			body, certificate, err := f.probeClient(ctx, f.terminate, &valid)
			require.NoError(t, err)
			require.Equal(t, f.certificate, certificate)
			require.Contains(t, string(body), "mixed-tls")
			wrong, err := tls.X509KeyPair(bundle.WrongCertPEM, bundle.WrongKeyPEM)
			require.NoError(t, err)
			_, _, err = f.probeClient(ctx, f.terminate, &wrong)
			require.Error(t, err)
			f.applyGateway(ctx, t, false)
			f.waitListeners(ctx, t, false)
			f.assertRejected(ctx, t, f.terminate)
			body, certificate, err = f.probeClient(ctx, f.terminate, &valid)
			require.NoError(t, err)
			require.Equal(t, f.certificate, certificate)
			require.Contains(t, string(body), "mixed-tls")
			_, _, err = f.probeClient(ctx, f.terminate, &wrong)
			require.Error(t, err)
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}

func gatewayMTLSValidation(name string) map[string]any {
	return map[string]any{"mode": "AllowValidOnly", "caCertificateRefs": []any{map[string]any{"group": "", "kind": "ConfigMap", "name": name}}}
}

func (f *mixedTLSFixture) assertUppercaseSNI(ctx context.Context, t *testing.T) {
	t.Helper()
	body, certificate, err := f.probe(ctx, strings.ToUpper(f.terminate))
	require.NoError(t, err)
	require.Equal(t, f.certificate, certificate)
	require.Contains(t, string(body), "mixed-tls")
}

func (f *mixedTLSFixture) assertALPN(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, protocol := range []string{"h2", "http/1.1"} {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		dialer := tls.Dialer{Config: &tls.Config{
			MinVersion: tls.VersionTLS12, ServerName: f.terminate, NextProtos: []string{protocol},
			InsecureSkipVerify: true, // #nosec G402 -- The fixture's exact certificate is pinned below.
		}}
		connection, err := dialer.DialContext(probeCtx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port)))
		cancel()
		require.NoError(t, err)
		state := connection.(*tls.Conn).ConnectionState()
		require.NoError(t, connection.Close())
		require.Equal(t, f.certificate, state.PeerCertificates[0].Raw)
		require.Equal(t, protocol, state.NegotiatedProtocol)
	}
}

func (f *mixedTLSFixture) assertClientAddress(ctx context.Context, t *testing.T) {
	t.Helper()
	since := time.Now()
	f.assertTerminated(ctx, t)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(f.namespace+"/mixed/terminate")))
	inner := findAccessLogRecordWhere(ctx, t, since, "private terminating TCP frontend", func(record map[string]any) bool {
		name, _ := record["frontend"].(string)
		return strings.HasPrefix(name, "gateway-tls-") && strings.HasSuffix(name, "-"+digest)
	})
	outer := findAccessLogRecordWhere(ctx, t, since, "public TLS dispatcher", func(record map[string]any) bool {
		return record["backend"] == strings.Replace(recordString(t, inner, "frontend"), "gateway-tls-", "gateway-tls-bridge-", 1)
	})
	clientAddress := recordString(t, outer, "client_ip")
	require.NotNil(t, net.ParseIP(clientAddress))
	require.Equal(t, clientAddress, recordString(t, inner, "client_ip"), "PROXY v2 must preserve the client address")
}

func (f *mixedTLSFixture) removeAndRestoreCertificate(ctx context.Context, t *testing.T) {
	t.Helper()
	secret := &corev1.Secret{}
	require.NoError(t, f.client.Resources(f.namespace).Get(ctx, "terminate-cert", f.namespace, secret))
	require.NoError(t, f.client.Resources(f.namespace).Delete(ctx, secret))
	waitForResourceDeployed(ctx, t, f.client, gatewayGVR, f.namespace, "mixed", func(gateway *unstructured.Unstructured) (bool, string) {
		listeners, _, err := unstructured.NestedSlice(gateway.Object, "status", "listeners")
		if err != nil {
			return false, err.Error()
		}
		for _, raw := range listeners {
			listener, ok := raw.(map[string]any)
			if !ok || listener["name"] != "terminate" {
				continue
			}
			return gatewayMTLSConditions(listener, gateway.GetGeneration(), false, []string{"ResolvedRefs", "Programmed"}), "waiting for invalid certificate status"
		}
		return false, "terminating listener missing"
	})
	f.waitTerminatingRejection(ctx, t)
	f.assertPassthrough(ctx, t)
	secret.ObjectMeta = metav1.ObjectMeta{Name: "terminate-cert", Namespace: f.namespace}
	require.NoError(t, f.client.Resources(f.namespace).Create(ctx, secret))
	f.waitListeners(ctx, t, true)
}

func (f *mixedTLSFixture) waitTerminatingRejection(ctx context.Context, t *testing.T) {
	t.Helper()
	require.NoError(t, testutil.WaitForConditionWithDescription(ctx, testutil.SlowWaitConfig(), "terminating TLS rejection with healthy passthrough", func(ctx context.Context) (bool, error) {
		_, _, rejected := f.probe(ctx, f.terminate)
		body, certificate, err := f.probe(ctx, f.passthrough)
		return rejected != nil && err == nil && bytes.Equal(certificate, f.backendCertificate) && strings.TrimSpace(string(body)) == "encrypted", nil
	}))
}

func (f *mixedTLSFixture) assertPassthroughOnlyTransitions(ctx context.Context, t *testing.T) {
	t.Helper()
	f.deleteTerminatingRoute(ctx, t)
	f.waitTerminatingRejection(ctx, t)
	listeners := f.listeners(true)
	listeners[0].(map[string]any)["tls"] = map[string]any{"mode": "Passthrough"}
	f.applyListeners(ctx, t, listeners)
	f.waitListeners(ctx, t, true)
	f.assertRejected(ctx, t, f.terminate)
	f.assertEncryptedHost(ctx, t, strings.ToUpper(f.passthrough))
	f.applyRoute(ctx, t, "terminated", f.terminate, "terminate", BackendRef{Service: "missing-backend", Port: f.encrypted.Port})
	f.assertRejected(ctx, t, f.terminate)
	f.assertPassthrough(ctx, t)
	f.applyListeners(ctx, t, listeners[1:])
	f.waitListeners(ctx, t, false)
	f.assertEncryptedHost(ctx, t, f.terminate)
	f.assertEncryptedHost(ctx, t, strings.ToUpper(f.passthrough))
	f.assertRejected(ctx, t, "")
	f.applyGateway(ctx, t, true)
	f.applyRoute(ctx, t, "terminated", f.terminate, "terminate", f.plain)
	f.waitListeners(ctx, t, true)
	f.assertTerminated(ctx, t)
	f.assertPassthrough(ctx, t)
}

func TestTLSRouteMultipleTerminationCertificates(t *testing.T) {
	feature := features.New("TLSRoute terminating listener certificate isolation").
		Assess("multiple terminating listeners retain their own certificates", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			f := newMixedTLSFixture(ctx, t, cfg)
			NewTLSSecret(ctx, t, f.client, f.namespace, "second-cert", []string{f.passthrough})
			secondCertificate := f.secretCertificate(ctx, t, "second-cert", "tls.crt")
			listeners := f.listeners(true)
			listeners[1].(map[string]any)["tls"] = map[string]any{"mode": "Terminate", "certificateRefs": []any{map[string]any{"name": "second-cert"}}}
			f.applyListeners(ctx, t, listeners)
			f.applyRoute(ctx, t, "terminated", f.terminate, "terminate", f.plain)
			f.applyRoute(ctx, t, "second", f.passthrough, "passthrough", f.plain)
			f.waitListeners(ctx, t, true)
			f.port = ForwardGateway(ctx, t, f.namespace, "mixed", mixedTLSPort).Ports[mixedTLSPort]
			f.assertTerminated(ctx, t)
			body, certificate, err := f.probe(ctx, f.passthrough)
			require.NoError(t, err)
			require.Equal(t, secondCertificate, certificate)
			require.Contains(t, string(body), "mixed-tls")
			f.applyListeners(ctx, t, listeners[1:])
			f.waitListeners(ctx, t, false)
			body, certificate, err = f.probe(ctx, f.passthrough)
			require.NoError(t, err)
			require.Equal(t, secondCertificate, certificate)
			require.Contains(t, string(body), "mixed-tls")
			f.assertRejected(ctx, t, f.terminate)
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}
