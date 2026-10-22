// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	hapticclient "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned"
	"github.com/haproxy-haptic/HAPTIC/tests/e2e/e2ecluster"
	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

func TestForwardedHeadersOperatorSnippets(t *testing.T) {
	// This test changes and restores the cluster policy before parallel tests start.
	hosts := map[string]string{}
	var namespace string
	modes := []string{"default", "add", "update", "ifmissing", "ignore"}
	feature := features.New("operator forwarded headers survive client sanitization").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			ns := NamespaceForTest(ctx, t, client)
			namespace = ns
			DumpLogsOnFailure(t, ns)
			echo := NewEchoServerBackend(ctx, t, client, ns)
			for _, mode := range modes {
				host := "xfwd-operator." + mode + "." + ns + ".localdev.me"
				hosts[mode] = host
				spec := &IngressSpec{Name: mode, Host: host, Path: "/", BackendService: echo.Service, BackendPort: echo.Port}
				if mode == "default" {
					spec.TLSSecretName = NewTLSSecret(ctx, t, client, ns, "operator-tls", []string{host})
				} else {
					spec.Annotations = map[string]string{"haproxy-haptic.org/forwardfor": mode}
				}
				NewIngress(ctx, t, client, ns, spec)
			}
			for _, mode := range modes {
				waitForIngressDeployed(ctx, t, client, ns, mode)
			}
			hosts["control"] = "xfwd-trusted-control." + ns + ".localdev.me"
			NewIngress(ctx, t, client, ns, &IngressSpec{Name: "control", Host: hosts["control"], Path: "/",
				BackendService: echo.Service, BackendPort: echo.Port})
			waitForIngressDeployed(ctx, t, client, ns, "control")
			return ctx
		}).
		Assess("HTTP, h2c and HTTPS retain operator authority and scheme with or without forged client headers",
			func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
				t.Helper()
				host := hosts["default"]
				for _, protocol := range []string{"http", "h2c", "https"} {
					for _, forged := range []bool{false, true} {
						headers := operatorForwardedHeaders(ctx, t, host, protocol, forged)
						expectHeader(t, headers, "x-forwarded-host", host)
						expectHeader(t, headers, "x-forwarded-port", "9443")
						expectHeader(t, headers, "x-forwarded-proto", "operator")
						expectAlternativeForwardedHeaders(t, headers, "operator")
						requireClientAddress(t, headers["x-forwarded-for"])
						require.NotEqual(t, "198.51.100.9", headers["x-forwarded-for"])
					}
				}
				return ctx
			}).
		Assess("route policy still governs an operator's early X-Forwarded-For",
			func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
				t.Helper()
				for _, mode := range modes[1:] {
					value := operatorForwardedHeaders(ctx, t, hosts[mode], "http", true)["x-forwarded-for"]
					switch mode {
					case "add":
						first, client, found := strings.Cut(value, ", ")
						require.True(t, found)
						require.Equal(t, "198.51.100.9", first)
						requireClientAddress(t, client)
					case "update":
						requireClientAddress(t, value)
						require.NotEqual(t, "198.51.100.9", value)
					default:
						require.Equal(t, "198.51.100.9", value)
					}
				}
				return ctx
			}).
		Assess("trusted client headers survive while operators can replace the scheme",
			func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
				t.Helper()
				assessTrustedOperatorForwardedHeaders(ctx, t, cfg, hosts, namespace)
				return ctx
			}).Feature()
	testEnv.Test(t, feature)
}

func assessTrustedOperatorForwardedHeaders(ctx context.Context, t *testing.T, cfg *envconf.Config, hosts map[string]string, namespace string) {
	t.Helper()
	trustForwardedHeadersDuringTest(ctx, t, cfg, hosts["default"], namespace, []string{"default", "add", "update", "ifmissing", "ignore", "control"}, "198.51.100.9")
	for _, protocol := range []string{"http", "h2c", "https"} {
		for _, forged := range []bool{false, true} {
			headers := operatorForwardedHeaders(ctx, t, hosts["default"], protocol, forged)
			wantHost, wantPort := hosts["default"], "9443"
			if forged {
				wantHost, wantPort = "evil.example", "4443"
			}
			expectHeader(t, headers, "x-forwarded-host", wantHost)
			expectHeader(t, headers, "x-forwarded-port", wantPort)
			expectHeader(t, headers, "x-forwarded-proto", "operator")
			first, address, found := strings.Cut(headers["x-forwarded-for"], ", ")
			require.True(t, found)
			require.Equal(t, "198.51.100.9", first)
			requireClientAddress(t, address)
		}
	}
	for _, protocol := range []string{"http", "h2c"} {
		control := operatorForwardedHeaders(ctx, t, hosts["control"], protocol, true)
		for header, value := range forgedAlternativeForwardedHeaders() {
			expectHeader(t, control, strings.ToLower(header), value)
		}
	}
	control := operatorForwardedHeaders(ctx, t, hosts["control"], "http", true)
	expectHeader(t, control, "x-forwarded-host", "evil.example")
	expectHeader(t, control, "x-forwarded-port", "4443")
	expectHeader(t, control, "x-forwarded-proto", "forged")
	first, address, found := strings.Cut(control["x-forwarded-for"], ", ")
	require.True(t, found)
	require.Equal(t, forgedForwardedFor, first)
	requireClientAddress(t, address)
	headers := forwardedHeaders(t, httpclient.New(t).GET(hosts["default"], "/").
		WithHeader("X-Forwarded-Host", "trusted.example").WithHeader("X-Operator-Source", "198.51.100.8"))
	expectHeader(t, headers, "x-forwarded-host", "trusted.example")
	expectHeader(t, headers, "x-forwarded-for", "198.51.100.8")
}

func trustForwardedHeadersDuringTest(ctx context.Context, t *testing.T, cfg *envconf.Config, host, namespace string, routeNames []string, forwardedPrefix string) {
	t.Helper()
	client, err := cfg.NewClient()
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(client.RESTConfig())
	require.NoError(t, err)
	hc, err := hapticclient.NewForConfig(client.RESTConfig())
	require.NoError(t, err)
	replicas, err := discoverHAProxyReplicaCount(ctx, client)
	require.NoError(t, err)
	publication := forwardedPolicyPublication{client: hc, replicas: replicas, namespace: namespace, routeNames: routeNames}
	publication.baselineChecksum, err = publication.wait(ctx, false)
	require.NoError(t, err)
	configs := dyn.Resource(configGVR).Namespace(ControllerNamespace)
	original, err := configs.Get(ctx, HAProxyConfigName, metav1.GetOptions{})
	require.NoError(t, err)
	previous, existed, err := unstructured.NestedFieldCopy(original.Object, "spec", "templatingSettings", "extraContext", "forwardedHeaders")
	require.NoError(t, err)
	patch := func(ctx context.Context, expected *unstructured.Unstructured, operation string, value any) (*unstructured.Unstructured, error) {
		body, err := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": expected.GetUID()},
			{"op": "test", "path": "/metadata/generation", "value": expected.GetGeneration()},
			{"op": operation, "path": "/spec/templatingSettings/extraContext/forwardedHeaders", "value": value},
		})
		if err != nil {
			return nil, err
		}
		return configs.Patch(ctx, HAProxyConfigName, types.JSONPatchType, body, metav1.PatchOptions{})
	}
	var patched *unstructured.Unstructured
	t.Cleanup(func() {
		if patched == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), testutil.SlowWaitConfig().Timeout)
		defer cancel()
		operation := "remove"
		if existed {
			operation = "add"
		}
		restored, err := patch(cleanupCtx, patched, operation, previous)
		require.NoError(t, err)
		require.NoError(t, waitForForwardedConfigValidation(cleanupCtx, configs, restored))
		_, err = publication.wait(cleanupCtx, false)
		require.NoError(t, err)
		waitForOperatorForwardedPolicy(cleanupCtx, t, host, false, forwardedPrefix)
	})
	address := operatorForwardedHeaders(ctx, t, host, "http", false)["x-forwarded-for"]
	require.NotNil(t, net.ParseIP(address))
	prefix := address + "/32"
	if net.ParseIP(address).To4() == nil {
		prefix = address + "/128"
	}
	patched, err = patch(ctx, original, "add", map[string]any{"trustedProxies": []string{prefix}})
	require.NoError(t, err)
	require.NoError(t, waitForForwardedConfigValidation(ctx, configs, patched))
	_, err = publication.wait(ctx, true)
	require.NoError(t, err)
	waitForOperatorForwardedPolicy(ctx, t, host, true, forwardedPrefix)
}

type forwardedPolicyPublication struct {
	client           hapticclient.Interface
	replicas         int
	namespace        string
	routeNames       []string
	baselineChecksum string
}

func (p *forwardedPolicyPublication) wait(ctx context.Context, trusted bool) (string, error) {
	var checksum string
	err := testutil.WaitForConditionWithDescription(ctx, testutil.SlowWaitConfig(), "forwarding policy is published and deployed to every replica",
		func(ctx context.Context) (bool, error) {
			obj, err := p.client.HaproxyTemplateICV1alpha1().HAProxyCfgs(ControllerNamespace).Get(ctx, HAProxyConfigName+"-haproxycfg", metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			content, err := haproxyCfgContent(obj)
			if err != nil {
				return false, err
			}
			if err := p.matchesPolicy(content, trusted); err != nil {
				return false, err
			}
			checksum = obj.Spec.Checksum
			if checksum == "" || p.baselineChecksum != "" && (checksum == p.baselineChecksum) == trusted {
				return false, fmt.Errorf("published checksum %q does not match the expected forwarding transition", checksum)
			}
			if len(obj.Status.DeployedToPods) < p.replicas {
				return false, fmt.Errorf("only %d/%d replicas reported deployed", len(obj.Status.DeployedToPods), p.replicas)
			}
			for index := range obj.Status.DeployedToPods {
				pod := &obj.Status.DeployedToPods[index]
				if pod.Checksum != checksum {
					return false, fmt.Errorf("pod %s has checksum %q, expected %q", pod.PodName, pod.Checksum, checksum)
				}
			}
			return true, nil
		})
	return checksum, err
}

func (p *forwardedPolicyPublication) matchesPolicy(content string, trusted bool) error {
	for _, mode := range p.routeNames {
		if !strings.Contains(content, "backend "+p.namespace+"_"+mode+"_svc_") {
			return fmt.Errorf("published config is missing the %s fixture backend", mode)
		}
	}
	if strings.Contains(content, "acl haptic_trusted_proxy ") != trusted {
		return fmt.Errorf("published forwarding policy does not match trusted=%t", trusted)
	}
	return nil
}

func waitForForwardedConfigValidation(ctx context.Context, configs dynamic.ResourceInterface, expected *unstructured.Unstructured) error {
	return testutil.WaitForConditionWithDescription(ctx, testutil.SlowWaitConfig(), "patched forwarding policy is validated",
		func(ctx context.Context) (bool, error) {
			current, err := configs.Get(ctx, expected.GetName(), metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if current.GetUID() != expected.GetUID() || current.GetGeneration() != expected.GetGeneration() {
				return false, fmt.Errorf("config identity changed: UID=%s generation=%d", current.GetUID(), current.GetGeneration())
			}
			if reason, valid := validatedAtOwnGeneration(current); !valid {
				return false, fmt.Errorf("config %s: %s", current.GetName(), reason)
			}
			return true, nil
		})
}

func waitForOperatorForwardedPolicy(ctx context.Context, t *testing.T, host string, trusted bool, forwardedPrefix string) {
	t.Helper()
	client := httpclient.New(t)
	waitConfig := testutil.WaitConfig{InitialInterval: 100 * time.Millisecond, MaxInterval: 2 * time.Second, Timeout: 15 * time.Second, Multiplier: 2}
	require.NoError(t, testutil.WaitForConditionWithDescription(ctx, waitConfig, "forwarded-header trust policy is deployed", func(ctx context.Context) (bool, error) {
		defer client.CloseIdleConnections()
		resp, err := client.GET(host, "/").WithHeader("X-Forwarded-For", forgedForwardedFor).Do(ctx)
		if err != nil {
			return false, err
		}
		if resp.Status != http.StatusOK || resp.Echo == nil {
			return false, fmt.Errorf("forwarding probe returned status=%d without expected echo", resp.Status)
		}
		value := resp.Echo.Headers["x-forwarded-for"]
		if trusted && strings.HasPrefix(value, forwardedPrefix+", ") || !trusted && net.ParseIP(value) != nil && value != forwardedPrefix {
			return true, nil
		}
		return false, fmt.Errorf("X-Forwarded-For=%q while trusted=%t", value, trusted)
	}))
}

func operatorForwardedHeaders(ctx context.Context, t *testing.T, host, protocol string, forged bool) map[string]string {
	t.Helper()
	if protocol == "h2c" {
		return operatorForwardedH2CHeaders(ctx, t, host, forged)
	}
	client := httpclient.New(t)
	req := client.GET(host, "/")
	if protocol == "https" {
		req = client.HTTPS(host, "/")
	}
	if forged {
		req = req.WithHeader("X-Forwarded-Host", "evil.example").
			WithHeader("X-Forwarded-Port", "4443").WithHeader("X-Forwarded-Proto", "forged").
			WithHeader("X-Forwarded-For", forgedForwardedFor)
		for header, value := range forgedAlternativeForwardedHeaders() {
			req = req.WithHeader(header, value)
		}
	}
	return forwardedHeaders(t, req)
}

func operatorForwardedH2CHeaders(ctx context.Context, t *testing.T, host string, forged bool) map[string]string {
	t.Helper()
	endpoint, err := e2ecluster.ResolveTrafficEndpoint()
	require.NoError(t, err)
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{Protocols: protocols}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.HTTPPort))+"/", http.NoBody)
	require.NoError(t, err)
	req.Host = host
	if forged {
		req.Header.Set("X-Forwarded-Host", "evil.example")
		req.Header.Set("X-Forwarded-Port", "4443")
		req.Header.Set("X-Forwarded-Proto", "forged")
		req.Header.Set("X-Forwarded-For", forgedForwardedFor)
		for header, value := range forgedAlternativeForwardedHeaders() {
			req.Header.Set(header, value)
		}
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, 2, resp.ProtoMajor)
	var echo struct {
		Request struct {
			Headers map[string]string `json:"headers"`
		} `json:"request"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&echo))
	return echo.Request.Headers
}
