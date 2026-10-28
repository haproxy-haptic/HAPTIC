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
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

type mixedTLSStaticFixture struct {
	*mixedTLSFixture
	static                 mixedTLSFixture
	ingressCertificate     []byte
	ingressTLSCertificate  []byte
	ingressHTTPSHost       string
	ingressPassthroughHost string
}

func TestTLSRouteMixedChartHTTPSOwnership(t *testing.T) {
	feature := features.New("TLSRoute shared HTTPS address ownership").
		Assess("allocated TLS listeners and static Ingress claims remain isolated", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			f := newMixedTLSStaticFixture(ctx, t, cfg)
			f.assertTerminated(ctx, t)
			f.assertPassthrough(ctx, t)
			f.assertIngressControls(ctx, t)
			f.assertIngressPassthrough(ctx, t, f.terminate)
			f.assertIngressPassthrough(ctx, t, f.passthrough)
			f.assertStaticContraction(ctx, t)
			f.assertStaticRouteRemoval(ctx, t)
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}

func newMixedTLSStaticFixture(ctx context.Context, t *testing.T, cfg *envconf.Config) *mixedTLSStaticFixture {
	t.Helper()
	f := &mixedTLSStaticFixture{mixedTLSFixture: newMixedTLSFixture(ctx, t, cfg)}
	f.listenerPort = 8443
	f.ingressHTTPSHost = "web." + f.namespace + ".other.localdev.me"
	f.ingressPassthroughHost = "ingress." + f.namespace + ".other.localdev.me"
	NewTLSSecret(ctx, t, f.client, f.namespace, "ingress-cert", []string{f.ingressHTTPSHost})
	f.ingressCertificate = f.secretCertificate(ctx, t, "ingress-cert", "tls.crt")
	NewIngress(ctx, t, f.client, f.namespace, &IngressSpec{
		Name: "ingress-https", Host: f.ingressHTTPSHost, TLSSecretName: "ingress-cert",
		BackendService: f.plain.Service, BackendPort: f.plain.Port,
	})
	listeners := f.listeners(true)
	listeners[0].(map[string]any)["tls"] = map[string]any{"mode": "Passthrough"}
	f.applyListeners(ctx, t, listeners)
	f.waitListeners(ctx, t, true)
	f.static = *f.mixedTLSFixture
	f.static.address = serviceTCPAddress(ctx, t, f.client, HAProxyDeploymentName, 443)
	f.static.assertRejected(ctx, t, f.terminate)
	f.assertIngressHTTPS(ctx, t)
	backend := NewTLSResponderBackend(ctx, t, f.client, f.namespace, "ingress", f.ingressPassthroughHost)
	f.ingressTLSCertificate = f.secretCertificate(ctx, t, "ingress-tls", "tls.pem")
	for i, host := range []string{f.ingressPassthroughHost, f.terminate, f.passthrough} {
		NewIngress(ctx, t, f.client, f.namespace, &IngressSpec{
			Name: []string{"ingress-passthrough", "ingress-same-terminate", "ingress-same-passthrough"}[i],
			Host: host, BackendService: backend.Service, BackendPort: backend.Port,
			Annotations: map[string]string{"haproxy-haptic.org/ssl-passthrough": "true"},
		})
	}
	f.applyGateway(ctx, t, true)
	f.applyRoute(ctx, t, "terminated", f.terminate, "terminate", f.plain)
	f.applyRoute(ctx, t, "encrypted", "*."+f.namespace+".localdev.me", "passthrough", f.encrypted)
	f.waitListeners(ctx, t, true)
	f.address = f.serviceAddress(ctx, t)
	return f
}

func (f *mixedTLSStaticFixture) assertIngressControls(ctx context.Context, t *testing.T) {
	t.Helper()
	f.assertIngressHTTPS(ctx, t)
	f.assertIngressPassthrough(ctx, t, f.ingressPassthroughHost)
}

func (f *mixedTLSStaticFixture) assertIngressHTTPS(ctx context.Context, t *testing.T) {
	t.Helper()
	body, certificate, err := f.static.probe(ctx, f.ingressHTTPSHost)
	require.NoError(t, err)
	require.Equal(t, f.ingressCertificate, certificate)
	require.Contains(t, string(body), "mixed-tls")
}

func (f *mixedTLSStaticFixture) assertIngressPassthrough(ctx context.Context, t *testing.T, host string) {
	t.Helper()
	body, certificate, err := f.static.probe(ctx, host)
	require.NoError(t, err)
	require.Equal(t, f.ingressTLSCertificate, certificate)
	require.Equal(t, "ingress", strings.TrimSpace(string(body)))
}

func (f *mixedTLSStaticFixture) assertStaticContraction(ctx context.Context, t *testing.T) {
	t.Helper()
	f.deleteTerminatingRoute(ctx, t)
	f.waitTerminatingRejection(ctx, t)
	listeners := f.listeners(true)
	listeners[0].(map[string]any)["tls"] = map[string]any{"mode": "Passthrough"}
	f.applyListeners(ctx, t, listeners)
	f.waitListeners(ctx, t, true)
	f.assertRejected(ctx, t, f.terminate)
	f.static.assertRejected(ctx, t, f.terminate)
	f.static.assertEncryptedHost(ctx, t, strings.ToUpper(f.passthrough))
	f.assertIngressControls(ctx, t)
	f.applyRoute(ctx, t, "terminated", f.terminate, "terminate", BackendRef{Service: "missing-backend", Port: f.encrypted.Port})
	f.assertRejected(ctx, t, f.terminate)
	f.static.assertRejected(ctx, t, f.terminate)
	f.static.assertPassthrough(ctx, t)
	f.applyListeners(ctx, t, listeners[1:])
	f.waitListeners(ctx, t, false)
	f.assertEncryptedHost(ctx, t, f.terminate)
	f.static.assertEncryptedHost(ctx, t, strings.ToUpper(f.terminate))
	f.static.assertRejected(ctx, t, "")
	f.assertIngressControls(ctx, t)
}

func (f *mixedTLSStaticFixture) assertStaticRouteRemoval(ctx context.Context, t *testing.T) {
	t.Helper()
	client, err := dynamic.NewForConfig(f.client.RESTConfig())
	require.NoError(t, err)
	require.NoError(t, client.Resource(tlsRouteGVR).Namespace(f.namespace).Delete(ctx, "encrypted", metav1.DeleteOptions{}))
	require.NoError(t, testutil.WaitForConditionWithDescription(ctx, testutil.SlowWaitConfig(), "unrouted static listener rejects with healthy Ingress", func(ctx context.Context) (bool, error) {
		_, _, rejected := f.static.probe(ctx, f.terminate)
		body, certificate, err := f.static.probe(ctx, f.ingressPassthroughHost)
		return rejected != nil && err == nil && bytes.Equal(certificate, f.ingressTLSCertificate) && strings.TrimSpace(string(body)) == "ingress", nil
	}))
	f.assertRejected(ctx, t, f.terminate)
	f.static.assertRejected(ctx, t, f.passthrough)
	f.assertIngressControls(ctx, t)
	require.NoError(t, client.Resource(gatewayGVR).Namespace(f.namespace).Delete(ctx, "mixed", metav1.DeleteOptions{}))
	require.NoError(t, testutil.WaitForConditionWithDescription(ctx, testutil.SlowWaitConfig(), "removed Gateway releases static Ingress claim", func(ctx context.Context) (bool, error) {
		body, certificate, err := f.static.probe(ctx, f.terminate)
		return err == nil && bytes.Equal(certificate, f.ingressTLSCertificate) && strings.TrimSpace(string(body)) == "ingress", nil
	}))
	f.assertIngressControls(ctx, t)
}
