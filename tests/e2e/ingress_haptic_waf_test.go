// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestHapticWAFPolicies covers the low-maintenance policy path an application
// owner normally uses: one `waf-policy` annotation and no route-local SecLang.
// The streaming-search fixture keeps URI/query/header inspection enabled while
// excluding only its free-form `q` search parameter and leaving request bodies
// untouched, so large uploads remain streaming-compatible. A second centrally
// defined policy proves bounded, complete body inspection explicitly. A
// self-service policy extending streaming-search proves inherited and added
// exclusions both apply.
func TestHapticWAFPolicies(t *testing.T) {
	t.Parallel()

	const (
		streamingHost = "ingress-haptic-waf-streaming.localdev.me"
		webHost       = "ingress-haptic-waf-web.localdev.me"
		extendsHost   = "ingress-haptic-waf-extends.localdev.me"
	)

	feature := features.New("Ingress: reusable Coraza WAF policies are narrow and body-safe").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackend(ctx, t, client, ns)

			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name:           "streaming",
				Host:           streamingHost,
				BackendService: backend.Service,
				BackendPort:    backend.Port,
				Annotations: map[string]string{
					"haproxy-haptic.org/waf-policy": "streaming-search",
				},
			})
			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name:           "web",
				Host:           webHost,
				BackendService: backend.Service,
				BackendPort:    backend.Port,
				Annotations: map[string]string{
					"haproxy-haptic.org/waf-policy": "form-body-inspection",
				},
			})

			if err := client.Resources().Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "waf-policies"},
				Data: map[string]string{"policies.yaml": "search-extended:\n" +
					"  extends: streaming-search\n" +
					"  ruleExclusions:\n" +
					"    - tags: [attack-sqli]\n" +
					"      excludeTarget: \"ARGS:other\"\n"},
			}); err != nil {
				t.Fatalf("create self-service WAF catalog: %v", err)
			}
			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name:           "extends",
				Host:           extendsHost,
				BackendService: backend.Service,
				BackendPort:    backend.Port,
				Annotations: map[string]string{
					"haproxy-haptic.org/waf-policy": "search-extended",
				},
			})

			waitForHaproxyIngressWafMap(ctx, t, []string{
				ns + "/streaming deny",
				ns + "/web deny",
				ns + "/extends deny",
			})
			waitForSnippetOnPod(ctx, t, []string{
				`[plugins.params.applications."policy:streaming-search"]`,
				`[plugins.params.applications."policy:form-body-inspection"]`,
				`[plugins.params.applications."policy:` + ns + `/search-extended"]`,
				`SecRuleUpdateTargetByTag "attack-sqli" "!ARGS:q"`,
				`SecRuleUpdateTargetByTag "attack-sqli" "!ARGS:other"`,
			})
			return ctx
		}).
		Assess("metadata-inspecting policy still blocks hostile headers", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			httpclient.New(t).GET(streamingHost, "/").
				WithHeader("User-Agent", "haptic-waf-block-probe").
				ExpectStatus(t, http.StatusForbidden)
			return ctx
		}).
		Assess("search q exclusion is narrow", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			httpclient.New(t).GET(streamingHost, "/?q=haptic-waf-query-probe").ExpectOK(t)
			httpclient.New(t).GET(streamingHost, "/?other=haptic-waf-query-probe").ExpectStatus(t, http.StatusForbidden)
			return ctx
		}).
		Assess("extended policy keeps inherited checks and adds its own exclusion", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			httpclient.New(t).GET(extendsHost, "/?q=haptic-waf-query-probe").ExpectOK(t)
			httpclient.New(t).GET(extendsHost, "/?other=haptic-waf-query-probe").ExpectOK(t)
			httpclient.New(t).GET(extendsHost, "/?third=haptic-waf-query-probe").ExpectStatus(t, http.StatusForbidden)
			httpclient.New(t).GET(extendsHost, "/").
				WithHeader("User-Agent", "haptic-waf-block-probe").
				ExpectStatus(t, http.StatusForbidden)
			return ctx
		}).
		Assess("streaming uploads are not buffered or body-inspected", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			httpclient.New(t).GET(streamingHost, "/upload").
				WithMethod(http.MethodPost).
				WithHeader("Content-Type", "application/octet-stream").
				WithChunkedBody("haptic-waf-body-probe").
				ExpectOK(t)
			return ctx
		}).
		Assess("web policy inspects complete bounded bodies", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			httpclient.New(t).GET(webHost, "/submit").
				WithMethod(http.MethodPost).
				WithHeader("Content-Type", "application/x-www-form-urlencoded").
				WithBody("payload=haptic-waf-body-probe").
				ExpectStatus(t, http.StatusForbidden)
			httpclient.New(t).GET(webHost, "/submit").
				WithMethod(http.MethodPost).
				WithHeader("Content-Type", "application/x-www-form-urlencoded").
				WithChunkedBody("payload=benign").
				ExpectStatus(t, http.StatusLengthRequired)
			return ctx
		})

	testEnv.Test(t, feature.Feature())
}

// TestHapticWAFSelfServiceOneApply applies an Ingress and the waf-policies
// ConfigMap defining its policy in one manifest, Ingress first (#703).
func TestHapticWAFSelfServiceOneApply(t *testing.T) {
	t.Parallel()

	const (
		host       = "ingress-haptic-waf-selfservice.localdev.me"
		updateHost = "ingress-haptic-waf-selfservice-update.localdev.me"
	)

	feature := features.New("Ingress: a self-service WAF policy applies together with its catalog").
		Assess("one apply admits both and enforces the policy", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackend(ctx, t, client, ns)
			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name: "update", Host: updateHost, BackendService: backend.Service, BackendPort: backend.Port,
			})
			probe := httpclient.New(t)
			probe.GET(updateHost, "/").ExpectOK(t)

			ingress := func(name, hostname string) string {
				return fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  namespace: %s
  name: %s
  annotations:
    haproxy-haptic.org/waf-policy: team-block
spec:
  ingressClassName: haptic
  rules:
  - host: %s
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: %s
            port:
              number: %d
`, ns, name, hostname, backend.Service, backend.Port)
			}
			manifest := []byte(ingress("app", host) + fmt.Sprintf(`---
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: %s
  name: waf-policies
data:
  policies.yaml: |
    team-block:
      requestBody:
        mode: none
      enforcement: deny
`, ns))
			dryRun := func(manifest []byte, name string) {
				t.Helper()
				out, err := kubectlApplyOutput(ctx, manifest, "--server-side", "--dry-run=server")
				require.NoError(t, err, "server-side dry run")
				require.Contains(t, out, "Unknown WAF policy 'team-block' on Ingress "+ns+"/"+name)
			}
			update := []byte(ingress("update", updateHost))
			dryRun(update, "update")
			if _, err := kubectlApplyOutput(ctx, update, "--server-side"); err != nil {
				t.Fatalf("server-side apply to existing Ingress: %v", err)
			}
			probe.GET(updateHost, "/").ExpectStatus(t, http.StatusServiceUnavailable)
			dryRun(manifest, "app")

			registerControllerForgetNamespaceCleanup(t, client, ns)
			t.Cleanup(func() {
				ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"}}
				if err := client.Resources(ns).Delete(context.Background(), ing); err != nil && !apierrors.IsNotFound(err) {
					t.Logf("delete Ingress %s/app: %v (best-effort)", ns, err)
				}
			})
			if err := kubectlApplyStdin(ctx, manifest); err != nil {
				t.Fatalf("apply Ingress and catalog: %v", err)
			}
			waitForIngressDeployed(ctx, t, client, ns, "app")
			waitForHaproxyIngressWafMap(ctx, t, []string{ns + "/app deny", ns + "/update deny"})

			for _, hostname := range []string{host, updateHost} {
				probe.GET(hostname, "/").ExpectOK(t)
				probe.GET(hostname, "/").
					WithHeader("User-Agent", "haptic-waf-block-probe").
					ExpectStatus(t, http.StatusForbidden)
			}
			return ctx
		})

	testEnv.Test(t, feature.Feature())
}
