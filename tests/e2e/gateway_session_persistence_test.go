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
	"fmt"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestGatewaySessionPersistence checks GEP-1619 persistence against a
// three-pod backend: the token the first response issues keeps every later
// request on that response's pod, for the Cookie and the Header type. The
// sessionPersistence field is experimental-channel only; the
// test-e2e-api-gateway job runs this test on that channel.
func TestGatewaySessionPersistence(t *testing.T) {
	t.Parallel()
	const (
		cookieHost = "session-cookie.localdev.me"
		headerHost = "session-header.localdev.me"
		cookieName = "STICKY"
		headerName = "X-Session-Id"
		followUps  = 20
	)
	var fwd ServiceForward

	feature := features.New("Gateway API: Cookie and Header session persistence").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			requireExperimentalGatewayAPI(ctx, t, client)
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackendWithReplicas(ctx, t, client, ns, "sticky-echo", 3)
			NewGateway(ctx, t, ns, "test-gateway")

			route := `apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  parentRefs: [{name: test-gateway}]
  hostnames: [%[3]s]
  rules:
    - sessionPersistence: {type: %[4]s, %[8]s: {name: %[5]s}}
      backendRefs: [{name: %[6]s, port: %[7]d}]
---
`
			manifest := fmt.Sprintf(route, "cookie-route", ns, cookieHost, "Cookie", cookieName, backend.Service, backend.Port, "cookie") +
				fmt.Sprintf(route, "header-route", ns, headerHost, "Header", headerName, backend.Service, backend.Port, "header")
			if err := kubectlApplyStdin(ctx, []byte(manifest)); err != nil {
				t.Fatalf("apply HTTPRoutes: %v", err)
			}
			waitForRouteDeployed(ctx, t, client, httpRouteGVR, ns, "cookie-route")
			waitForRouteDeployed(ctx, t, client, httpRouteGVR, ns, "header-route")
			fwd = ForwardGateway(ctx, t, ns, "test-gateway", 80)
			return ctx
		}).
		Assess("without a token, requests reach several pods", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client := httpclient.ForForwarded(t, fwd.HTTPPort, 0)
			pods := map[string]bool{}
			client.GET(headerHost, "/").ExpectMatching(t, "a second pod answers", func(resp *httpclient.Response) bool {
				if resp.Status == http.StatusOK && resp.Echo != nil {
					pods[resp.Echo.PodHostname] = true
				}
				return len(pods) > 1
			})
			return ctx
		}).
		Assess("the Header token keeps requests on one pod", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client := httpclient.ForForwarded(t, fwd.HTTPPort, 0)
			token, pod := headerSession(t, client.GET(headerHost, "/").ExpectOK(t), headerName)
			assertSamePod(ctx, t, client.GET(headerHost, "/").WithHeader(headerName, token), pod, followUps)
			return ctx
		}).
		Assess("the Cookie token keeps requests on one pod", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client := httpclient.ForForwarded(t, fwd.HTTPPort, 0)
			cookie, pod := cookieSession(t, client.GET(cookieHost, "/").ExpectOK(t), cookieName)
			assertSamePod(ctx, t, client.GET(cookieHost, "/").WithHeader("Cookie", cookie), pod, followUps)
			return ctx
		}).
		Feature()
	testEnv.Test(t, feature)
}

// headerSession returns the token and pod of the response that opened a
// Header session, failing if the persistence cookie it rides on outlives it.
func headerSession(t *testing.T, first *httpclient.Response, headerName string) (token, pod string) {
	t.Helper()
	token = first.Header.Get(headerName)
	if token == "" || first.Echo == nil {
		t.Fatalf("first response carries no %s header (headers: %v)", headerName, first.Header)
	}
	for _, cookie := range first.Header.Values("Set-Cookie") {
		if strings.HasPrefix(cookie, headerName+"=") && !strings.Contains(cookie, "Max-Age=0") {
			t.Fatalf("Header persistence left a lasting cookie: %q", cookie)
		}
	}
	return token, first.Echo.PodHostname
}

// cookieSession returns the Cookie request header and pod of the response
// that opened a Cookie session.
func cookieSession(t *testing.T, first *httpclient.Response, cookieName string) (cookie, pod string) {
	t.Helper()
	for _, value := range first.Header.Values("Set-Cookie") {
		if strings.HasPrefix(value, cookieName+"=") {
			cookie = strings.SplitN(value, ";", 2)[0]
		}
	}
	if cookie == "" || first.Echo == nil {
		t.Fatalf("first response sets no %s cookie (headers: %v)", cookieName, first.Header)
	}
	return cookie, first.Echo.PodHostname
}

func assertSamePod(ctx context.Context, t *testing.T, request *httpclient.Request, want string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		resp, err := request.Do(ctx)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if resp.Status != http.StatusOK || resp.Echo == nil {
			t.Fatalf("request %d: status %d", i, resp.Status)
		}
		if resp.Echo.PodHostname != want {
			t.Fatalf("request %d reached pod %s, the session belongs to %s", i, resp.Echo.PodHostname, want)
		}
	}
}

// requireExperimentalGatewayAPI skips a test that needs experimental-channel
// fields when the cluster serves the standard channel, which prunes them.
func requireExperimentalGatewayAPI(ctx context.Context, t *testing.T, client klient.Client) {
	t.Helper()
	dyn, err := dynamic.NewForConfig(client.RESTConfig())
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	crd, err := dyn.Resource(crdGVR).Get(ctx, "httproutes.gateway.networking.k8s.io", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the HTTPRoute CRD: %v", err)
	}
	if crd.GetAnnotations()["gateway.networking.k8s.io/channel"] != "experimental" {
		t.Skip("needs the experimental Gateway API channel (HAPTIC_E2E_GWAPI_CHANNEL=experimental)")
	}
}
