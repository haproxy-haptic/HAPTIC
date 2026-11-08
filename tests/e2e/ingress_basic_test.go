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

//go:build e2e

package e2e

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestIngressBasic is the foundational smoke test: an Ingress with no
// annotations, plain HTTP on the chart's NodePort, hitting echo-server.
// If this passes across the HAProxy version matrix, the e2e suite's
// fixture wiring is sound and richer behavioural tests can layer on
// top.
func TestIngressBasic(t *testing.T) {
	t.Parallel()
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "Ingress: basic routing",
		Host:        "ingress-basic.localdev.me",
		Assess: []SimpleIngressAssertion{{
			Name: "host returns 200 from echo-server",
			Check: func(t *testing.T, host string) {
				t.Helper()
				resp := httpclient.New(t).GET(host, "/").ExpectOK(t)
				if resp.Echo == nil {
					t.Fatalf("expected echo-server JSON body, got %d bytes: %s", len(resp.Body), string(resp.Body))
				}
			},
		}},
	})
}

func TestIngressExternalNameBackend(t *testing.T) {
	t.Parallel()
	host := "ingress-externalname.localdev.me"

	feature := features.New("Ingress: ExternalName Service backend").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			echoBackend := NewEchoServerBackend(ctx, t, client, ns)

			externalSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "external-echo",
					Namespace: ns,
				},
				Spec: corev1.ServiceSpec{
					Type:         corev1.ServiceTypeExternalName,
					ExternalName: echoBackend.Service + "." + ns + ".svc.cluster.local",
					Ports: []corev1.ServicePort{{
						Name: "http",
						Port: echoBackend.Port,
					}},
				},
			}
			if err := client.Resources().Create(ctx, externalSvc); err != nil {
				t.Fatalf("create ExternalName Service: %v", err)
			}

			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name:           "external-echo",
				Host:           host,
				Path:           "/",
				BackendService: externalSvc.Name,
				BackendPort:    echoBackend.Port,
			})
			return ctx
		}).
		Assess("ExternalName Service resolves and routes to upstream", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			resp := httpclient.New(t).GET(host, "/").ExpectOK(t)
			if resp.Echo == nil {
				t.Fatalf("expected echo-server JSON body, got %d bytes: %s", len(resp.Body), string(resp.Body))
			}
			return ctx
		}).
		Feature()
	testEnv.Test(t, feature)
}
