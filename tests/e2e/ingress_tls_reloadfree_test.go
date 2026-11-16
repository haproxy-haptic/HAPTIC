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
	"fmt"
	"strings"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestIngressTLSHostRemovalIsReloadFree pins that withdrawing a TLS host takes
// its line out of the running crt-list over the runtime API (#678): an Ingress
// teardown elsewhere in the cluster must not reload every HAProxy pod.
func TestIngressTLSHostRemovalIsReloadFree(t *testing.T) {
	const (
		anchorHost = "tls-rf-anchor.localdev.me"
		cycleHost  = "tls-rf-cycle.localdev.me"
		cycleName  = "tls-rf-cycle"
	)
	var (
		client    klient.Client
		cs        kubernetes.Interface
		namespace string
		backend   BackendRef
	)

	feature := features.New("Ingress TLS host removal is reload-free on 3.4").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			var err error
			if client, err = cfg.NewClient(); err != nil {
				t.Fatalf("new client: %v", err)
			}
			if cs, err = newClientsetForE2E(client.RESTConfig()); err != nil {
				t.Fatalf("build clientset: %v", err)
			}
			namespace = NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, namespace)
			backend = NewNamedEchoServerBackend(ctx, t, client, namespace, "echo-tls-rf")
			// Same shape as the cycled Ingress, so its removal moves only a
			// dynamic backend, map entries, and its crt-list line.
			NewIngress(ctx, t, client, namespace, &IngressSpec{
				Name: "tls-rf-anchor", Host: anchorHost, BackendService: backend.Service, BackendPort: backend.Port,
			})
			waitForHTTPSServing(ctx, t, cs, anchorHost)
			return ctx
		}).
		Assess("removing a TLS host withdraws its crt-list line without a reload", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			http := httpclient.New(t)
			for cycle := 1; cycle <= reloadFreeCycles; cycle++ {
				secret := NewTLSSecret(ctx, t, client, namespace, fmt.Sprintf("%s-%d", cycleName, cycle), []string{cycleHost})
				NewIngress(ctx, t, client, namespace, &IngressSpec{
					Name: cycleName, Host: cycleHost, BackendService: backend.Service, BackendPort: backend.Port,
					TLSSecretName: secret,
				})
				waitForHTTPSServing(ctx, t, cs, cycleHost)
				certFile := namespace + "_" + secret + ".pem"
				if list := showMainCRTList(ctx, t, cs); !strings.Contains(list, certFile) {
					t.Fatalf("cycle %d: running crt-list lacks %s:\n%s", cycle, certFile, list)
				}

				// The add may land mid-list and reload; only the removal is measured.
				waitFleetQuiescent(ctx, t, client, cs)
				before := captureReloadFingerprint(ctx, t, cs)
				deleteRouteByName(ctx, t, mustDynamic(t, client), ingressGVR, namespace, cycleName)
				waitForRouteGone(ctx, t, cs, http, cycleHost, "/")
				reloadFreeReaction(ctx, t, cs, certFile+" leaves the running crt-list",
					func(ctx context.Context) (bool, error) {
						return !strings.Contains(showMainCRTList(ctx, t, cs), certFile), nil
					})
				if dynamicBackendsSupported() {
					assertReloadFree(t, before, captureReloadFingerprint(ctx, t, cs),
						fmt.Sprintf("TLS host removal, cycle %d", cycle))
				}
			}
			return ctx
		}).
		Feature()

	testEnv.Test(t, feature)
}

func waitForHTTPSServing(ctx context.Context, t *testing.T, cs kubernetes.Interface, host string) {
	t.Helper()
	client := httpclient.New(t)
	reloadFreeReaction(ctx, t, cs, host+" answers 200 over HTTPS", func(ctx context.Context) (bool, error) {
		resp, err := client.HTTPS(host, "/").Do(ctx)
		if err != nil {
			return false, err
		}
		if resp.Status != 200 {
			return false, fmt.Errorf("status %d", resp.Status)
		}
		return true, nil
	})
}

// showMainCRTList prints the entries of the running crt-list the default HTTPS
// bind serves, read from one pod's worker socket.
func showMainCRTList(ctx context.Context, t *testing.T, cs kubernetes.Interface) string {
	t.Helper()
	pod := firstHAProxyPod(ctx, t, cs)
	run := func(command string) string {
		script := "printf '" + command + "\\n' | socat -t5 stdio unix-connect:" + haproxyWorkerSocketPath
		out, err := execInHAProxyPod(ctx, pod, "haproxy", "sh", "-c", script)
		if err != nil {
			t.Fatalf("%s on %s: %v\n%s", command, pod, err, out)
		}
		return out
	}
	for _, line := range strings.Split(run("show ssl crt-list"), "\n") {
		if name := strings.TrimSpace(line); strings.HasSuffix(name, "certificate-list.txt") {
			return run("show ssl crt-list " + name)
		}
	}
	t.Fatalf("no certificate-list.txt among the running crt-lists on %s", pod)
	return ""
}

func mustDynamic(t *testing.T, client klient.Client) dynamic.Interface {
	t.Helper()
	dyn, err := newDynamicForE2E(client.RESTConfig())
	if err != nil {
		t.Fatalf("build dynamic client: %v", err)
	}
	return dyn
}
