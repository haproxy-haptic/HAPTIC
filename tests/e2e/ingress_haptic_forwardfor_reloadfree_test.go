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
	"net"
	"net/http"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestHapticForwardForRouteAddRemoveIsReloadFree proves a forwardfor route is
// dynamic: the X-Forwarded-For rule reads a per-route map, so the backend
// stays plain and a second route is added and removed at runtime. Serial like
// the other reload-free suites: a reload count is attributable only on a
// quiet fleet.
func TestHapticForwardForRouteAddRemoveIsReloadFree(t *testing.T) {
	const (
		anchorHost = "ingress-haptic-xff-rf-anchor.localdev.me"
		anchorName = "echo-haptic-xff-rf-anchor"
		cycleHost  = "ingress-haptic-xff-rf-cycle.localdev.me"
		cycleName  = "echo-haptic-xff-rf-cycle"
		forged     = "203.0.113.9"
	)
	var (
		client klient.Client
		cs     kubernetes.Interface
		dyn    dynamic.Interface
		ns     string
		echo   BackendRef
	)
	route := func(name, host string) *IngressSpec {
		return &IngressSpec{
			Name:           name,
			Host:           host,
			Path:           "/",
			BackendService: echo.Service,
			BackendPort:    echo.Port,
			Annotations:    map[string]string{"haproxy-haptic.org/forwardfor": "update"},
		}
	}
	feature := features.New("Ingress: HAPTIC-native forwardfor route add/remove is reload-free on 3.4").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			var err error
			if client, err = cfg.NewClient(); err != nil {
				t.Fatalf("new client: %v", err)
			}
			if cs, err = newClientsetForE2E(client.RESTConfig()); err != nil {
				t.Fatalf("build clientset: %v", err)
			}
			if dyn, err = newDynamicForE2E(client.RESTConfig()); err != nil {
				t.Fatalf("build dynamic client: %v", err)
			}
			ns = NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			echo = NewEchoServerBackend(ctx, t, client, ns)
			NewIngress(ctx, t, client, ns, route(anchorName, anchorHost))
			if ok, err := xffOverwritten(ctx, httpclient.New(t), anchorHost, forged); !ok {
				t.Fatalf("anchor route: %v", err)
			}
			return ctx
		}).
		Assess("a second forwardfor route is added and removed at runtime and, on 3.4, never reloads",
			func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
				t.Helper()
				hc := httpclient.New(t)
				waitFleetQuiescent(ctx, t, client, cs)
				before := captureReloadFingerprint(ctx, t, cs)

				cycle := buildIngress(ns, route(cycleName, cycleHost))
				if err := client.Resources(ns).Create(ctx, cycle); err != nil {
					t.Fatalf("create Ingress %s/%s: %v", ns, cycleName, err)
				}
				reloadFreeReaction(ctx, t, cs, fmt.Sprintf("%s/ overwrites a forged X-Forwarded-For", cycleHost),
					func(ctx context.Context) (bool, error) { return xffOverwritten(ctx, hc, cycleHost, forged) })
				deleteRouteByName(ctx, t, dyn, ingressGVR, ns, cycleName)
				waitForRouteGone(ctx, t, cs, hc, cycleHost, "/")

				after := captureReloadFingerprint(ctx, t, cs)
				if dynamicBackendsSupported() {
					assertReloadFree(t, before, after, "forwardfor route create+delete")
				} else {
					t.Logf("forwardfor route cycle: %.0f reloads on %s (dynamic backends need 3.4)",
						after.reloads-before.reloads, ChartHAProxyVersion)
				}
				return ctx
			}).
		Feature()
	testEnv.Test(t, feature)
}

// xffOverwritten reports whether host answers 200 with the forged
// X-Forwarded-For replaced upstream: forwardfor=update overwrites a
// client-supplied value with the real peer address.
func xffOverwritten(ctx context.Context, hc *httpclient.Client, host, forged string) (bool, error) {
	resp, err := hc.GET(host, "/").WithHeader("X-Forwarded-For", forged).Do(ctx)
	if err != nil {
		return false, err
	}
	if resp.Status != http.StatusOK {
		return false, fmt.Errorf("status %d", resp.Status)
	}
	if resp.Echo == nil {
		return false, fmt.Errorf("no echo JSON in %d bytes", len(resp.Body))
	}
	if got := resp.Echo.Headers["x-forwarded-for"]; net.ParseIP(got) == nil || got == forged {
		return false, fmt.Errorf("upstream saw X-Forwarded-For %q, want only the client address", got)
	}
	return true, nil
}
