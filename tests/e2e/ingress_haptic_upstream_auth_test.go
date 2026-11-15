// Copyright 2026 Philipp Hossner
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

//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestHapticUpstreamAuthorization verifies upstream-authorization-secret
// (66-upstream-auth.yaml): after API-key authentication the upstream receives
// the Secret's Authorization value instead of the caller's, a rotated Secret
// reaches the upstream, and the value never appears in HAPTIC's output CRs.
func TestHapticUpstreamAuthorization(t *testing.T) {
	t.Parallel()

	const (
		host     = "ingress-haptic-upstream-auth.localdev.me"
		first    = "Basic Z2l0bGFiK2RlcGxveS10b2tlbi00MjpmaXJzdA=="
		rotated  = "Bearer rotated-upstream-token"
		apiKey   = "apikey-abc123"
		caller   = "Bearer caller-token"
		credName = "git-deploy"
	)
	var namespace string

	feature := features.New("Ingress: HAPTIC upstream credential from a Secret").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			namespace = NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, namespace)
			backend := NewEchoServerBackend(ctx, t, client, namespace)
			mustCreateSecret(ctx, t, client, namespace, "api-keys", map[string][]byte{"keys": []byte(apiKey + ":alice\n")})
			mustCreateSecret(ctx, t, client, namespace, credName, map[string][]byte{"authorization": []byte(first)})
			NewIngress(ctx, t, client, namespace, &IngressSpec{
				Name:           "echo",
				Host:           host,
				BackendService: backend.Service,
				BackendPort:    backend.Port,
				Annotations: map[string]string{
					"haproxy-haptic.org/api-key-secret":                "api-keys",
					"haproxy-haptic.org/upstream-authorization-secret": credName,
				},
			})
			return ctx
		}).
		Assess("caller's Authorization is replaced after authentication", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			t.Helper()
			httpclient.New(t).GET(host, "/").WithHeader("X-API-Key", apiKey).WithHeader("Authorization", caller).
				ExpectEchoHeader(t, "Authorization", first)
			httpclient.New(t).GET(host, "/").WithHeader("Authorization", caller).
				ExpectStatus(t, http.StatusUnauthorized)
			return ctx
		}).
		Assess("TRACE is refused", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			t.Helper()
			httpclient.New(t).GET(host, "/").WithMethod(http.MethodTrace).WithHeader("X-API-Key", apiKey).
				ExpectStatus(t, http.StatusMethodNotAllowed)
			return ctx
		}).
		Assess("a rotated Secret reaches the upstream", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			var secret corev1.Secret
			if err := client.Resources(namespace).Get(ctx, credName, namespace, &secret); err != nil {
				t.Fatalf("get secret: %v", err)
			}
			secret.Data["authorization"] = []byte(rotated)
			if err := client.Resources(namespace).Update(ctx, &secret); err != nil {
				t.Fatalf("rotate secret: %v", err)
			}
			httpclient.New(t).GET(host, "/").WithHeader("X-API-Key", apiKey).
				ExpectEchoHeader(t, "Authorization", rotated)
			return ctx
		}).
		Assess("the credential stays out of HAProxyCfg and HAProxyMapFile", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			t.Helper()
			for _, kind := range []string{"haproxycfgs", "haproxymapfiles"} {
				out, err := kubectlWithBackoff(ctx, kubeconfigFlag, kubeconfigPath, "get", kind, "-A", "-o", "yaml")
				if err != nil {
					t.Fatalf("list %s: %v: %s", kind, err, out)
				}
				for _, value := range []string{first, rotated, "rotated-upstream-token", apiKey} {
					if strings.Contains(string(out), value) {
						t.Errorf("%s contains %q", kind, value)
					}
				}
			}
			return ctx
		})

	testEnv.Test(t, feature.Feature())
}
