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
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

func TestGatewayRoutePolicyWAF(t *testing.T) {
	feature := features.New("Gateway immutable WAF catalog rotation and recovery").
		Assess("validated references rotate immutable catalogs", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			const host = "gateway-policy-waf.localdev.me"
			catalog := gatewayWAFCatalog(ns, "waf-v1", "local-policy:\n  requestBody: {mode: none}\n  enforcement: deny\n")
			require.NoError(t, client.Resources().Create(ctx, catalog))
			local := gatewayRoutePolicy(ns, "local", map[string]any{"waf": map[string]any{
				"policy": "local-policy", "catalogRef": map[string]any{"name": catalog.Name},
			}})
			body := gatewayRoutePolicy(ns, "body", map[string]any{"waf": map[string]any{"policy": "form-body-inspection"}})
			require.NoError(t, client.Resources().Create(ctx, local))
			require.NoError(t, client.Resources().Create(ctx, body))
			backend := NewEchoServerBackend(ctx, t, client, ns)
			NewGateway(ctx, t, ns, "policy")
			route := gatewayPolicyRoute("HTTPRoute", ns, host, []any{
				gatewayPolicyHTTPRule(backend, "/", "local"),
				gatewayPolicyHTTPRule(backend, "/body", "body"),
				gatewayPolicyHTTPRule(backend, "/public", ""),
			})
			require.NoError(t, client.Resources().Create(ctx, route))
			waitForRouteDeployed(ctx, t, client, httpRouteGVR, ns, route.GetName())
			forward := ForwardGateway(ctx, t, ns, "policy", 80)
			requests := httpclient.ForForwarded(t, forward.HTTPPort, 0)
			requests.GET(host, "/").ExpectOK(t)
			requests.GET(host, "/").WithHeader("User-Agent", "haptic-waf-block-probe").ExpectStatus(t, http.StatusForbidden)
			requests.GET(host, "/public").WithHeader("User-Agent", "haptic-waf-block-probe").ExpectOK(t)
			requests.GET(host, "/body").WithMethod(http.MethodPost).
				WithHeader("Content-Type", "application/x-www-form-urlencoded").WithBody("field=haptic-waf-body-probe").ExpectStatus(t, http.StatusForbidden)
			checkGatewayWAFCatalogRotation(ctx, t, client, local, catalog, requests, host)
			checkGatewayTrustedWAFCatalogRecovery(ctx, t, client, requests, host)
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}

func gatewayWAFCatalog(namespace, name, policies string) *corev1.ConfigMap {
	immutable := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Immutable:  &immutable,
		Data:       map[string]string{"policies.yaml": policies},
	}
}

func checkGatewayTrustedWAFCatalogRecovery(ctx context.Context, t *testing.T, client klient.Client, requests *httpclient.Client, host string) {
	t.Helper()
	original := &corev1.ConfigMap{}
	require.NoError(t, client.Resources().Get(ctx, "haptic-waf-policies", "security", original))
	replacement := gatewayWAFCatalog(original.Namespace, original.Name, original.Data["policies.yaml"])
	t.Cleanup(func() {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		current := &corev1.ConfigMap{}
		if err := client.Resources().Get(recoveryCtx, original.Name, original.Namespace, current); err == nil {
			if current.Data["policies.yaml"] == original.Data["policies.yaml"] {
				return
			}
			require.NoError(t, client.Resources().Delete(recoveryCtx, current))
		}
		require.NoError(t, client.Resources().Create(recoveryCtx, replacement))
	})
	require.NoError(t, client.Resources().Delete(ctx, original))
	requests.GET(host, "/body").ExpectStatus(t, http.StatusServiceUnavailable)
	requests.GET(host, "/public").ExpectOK(t)
	malformed := gatewayWAFCatalog(original.Namespace, original.Name, "form-body-inspection: [")
	require.NoError(t, client.Resources().Create(ctx, malformed))
	requests.GET(host, "/body").ExpectStatus(t, http.StatusServiceUnavailable)
	requests.GET(host, "/public").ExpectOK(t)
	require.NoError(t, client.Resources().Delete(ctx, malformed))
	require.NoError(t, client.Resources().Create(ctx, replacement))
	requests.GET(host, "/body").WithHeader("User-Agent", "haptic-waf-block-probe").ExpectStatus(t, http.StatusForbidden)
}

func updateGatewayWAFCatalogReference(ctx context.Context, client klient.Client, policy *unstructured.Unstructured, name string, options ...resources.UpdateOption) error {
	if err := client.Resources().Get(ctx, policy.GetName(), policy.GetNamespace(), policy); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(policy.Object, name, "spec", "waf", "catalogRef", "name"); err != nil {
		return err
	}
	missing := "WAF ConfigMap " + policy.GetNamespace() + "/" + name + " is missing."
	var updateErr error
	// A successful ConfigMap create can precede its arrival in the admission cache.
	waitErr := testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(ctx context.Context) (bool, error) {
		updateErr = client.Resources().Update(ctx, policy, options...)
		return updateErr == nil || !strings.Contains(updateErr.Error(), missing), updateErr
	})
	if waitErr != nil {
		return waitErr
	}
	return updateErr
}

func checkGatewayWAFCatalogRotation(ctx context.Context, t *testing.T, client klient.Client, policy *unstructured.Unstructured, catalog *corev1.ConfigMap, requests *httpclient.Client, host string) {
	t.Helper()
	require.NoError(t, client.Resources().Get(ctx, catalog.Name, catalog.Namespace, catalog))
	invalid := catalog.DeepCopy()
	invalid.Data["policies.yaml"] = "local-policy: ["
	require.ErrorContains(t, client.Resources().Update(ctx, invalid), "immutable")
	malformed := gatewayWAFCatalog(catalog.Namespace, "waf-bad", "local-policy: [")
	require.NoError(t, client.Resources().Create(ctx, malformed))
	require.ErrorContains(t, updateGatewayWAFCatalogReference(ctx, client, policy, malformed.Name), "invalid YAML")
	mutable := gatewayWAFCatalog(catalog.Namespace, "waf-mutable", catalog.Data["policies.yaml"])
	mutable.Immutable = nil
	require.NoError(t, client.Resources().Create(ctx, mutable))
	require.ErrorContains(t, updateGatewayWAFCatalogReference(ctx, client, policy, mutable.Name), "is mutable")
	requests.GET(host, "/").WithHeader("User-Agent", "haptic-waf-block-probe").ExpectStatus(t, http.StatusForbidden)
	next := gatewayWAFCatalog(catalog.Namespace, "waf-v2", "local-policy:\n  requestBody: {mode: none}\n  enforcement: detect\n")
	require.NoError(t, client.Resources().Create(ctx, next))
	require.NoError(t, updateGatewayWAFCatalogReference(ctx, client, policy, next.Name))
	requests.GET(host, "/").WithHeader("User-Agent", "haptic-waf-block-probe").ExpectOK(t)
	requests.GET(host, "/body").WithHeader("User-Agent", "haptic-waf-block-probe").ExpectStatus(t, http.StatusForbidden)
	checkGatewayWAFCatalogRecovery(ctx, t, client, policy, next, requests, host)
}

func checkGatewayWAFCatalogRecovery(ctx context.Context, t *testing.T, client klient.Client, policy *unstructured.Unstructured, catalog *corev1.ConfigMap, requests *httpclient.Client, host string) {
	t.Helper()
	clientset, err := newClientsetForE2E(client.RESTConfig())
	require.NoError(t, err)
	restore := stopControllersForAdmissionTest(ctx, t, clientset)
	require.NoError(t, client.Resources().Get(ctx, catalog.Name, catalog.Namespace, catalog))
	invalid := catalog.DeepCopy()
	invalid.Data["policies.yaml"] = "local-policy: ["
	require.ErrorContains(t, client.Resources().Update(ctx, invalid), "immutable")
	next := gatewayWAFCatalog(catalog.Namespace, "waf-v3", "local-policy:\n  requestBody: {mode: none}\n  enforcement: deny\n")
	require.NoError(t, client.Resources().Create(ctx, next))
	operational := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: catalog.Namespace, Name: "recovery-config"}}
	require.NoError(t, client.Resources().Create(ctx, operational))
	require.ErrorContains(t, updateGatewayWAFCatalogReference(ctx, client, policy, next.Name), "failed calling webhook")
	requests.GET(host, "/").WithHeader("User-Agent", "haptic-waf-block-probe").ExpectOK(t)
	restore()
	require.NoError(t, testutil.WaitForConditionWithDescription(ctx, testutil.DefaultWaitConfig(), "policy admission reachable after controller recovery", func(ctx context.Context) (bool, error) {
		err := updateGatewayWAFCatalogReference(ctx, client, policy, next.Name, func(options *metav1.UpdateOptions) {
			options.DryRun = []string{metav1.DryRunAll}
		})
		return err == nil, err
	}))
	require.NoError(t, updateGatewayWAFCatalogReference(ctx, client, policy, next.Name))
	requests.GET(host, "/").WithHeader("User-Agent", "haptic-waf-block-probe").ExpectStatus(t, http.StatusForbidden)
	require.NoError(t, client.Resources().Delete(ctx, next))
	requests.GET(host, "/").ExpectStatus(t, http.StatusServiceUnavailable)
	replacement := gatewayWAFCatalog(catalog.Namespace, next.Name, "local-policy: [")
	require.NoError(t, client.Resources().Create(ctx, replacement))
	requests.GET(host, "/").ExpectStatus(t, http.StatusServiceUnavailable)
	requests.GET(host, "/public").WithHeader("User-Agent", "haptic-waf-block-probe").ExpectOK(t)
}

func stopControllersForAdmissionTest(ctx context.Context, t *testing.T, client kubernetes.Interface) func() {
	t.Helper()
	deployments := client.AppsV1().Deployments(ControllerNamespace)
	name := ControllerDeploymentName
	scale, err := deployments.GetScale(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	replicas := scale.Spec.Replicas
	require.Positive(t, replicas)
	restored := false
	restore := func() {
		t.Helper()
		if restored {
			return
		}
		wait := testutil.DefaultWaitConfig()
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait.Timeout)
		defer cancel()
		current, getErr := deployments.GetScale(recoveryCtx, name, metav1.GetOptions{})
		require.NoError(t, getErr)
		current.Spec.Replicas = replicas
		_, updateErr := deployments.UpdateScale(recoveryCtx, name, current, metav1.UpdateOptions{})
		require.NoError(t, updateErr)
		require.NoError(t, testutil.WaitForConditionWithDescription(recoveryCtx, wait, "controllers ready after recovery", func(ctx context.Context) (bool, error) {
			deployment, getErr := deployments.Get(ctx, name, metav1.GetOptions{})
			if getErr != nil {
				return false, getErr
			}
			status := deployment.Status
			if status.ObservedGeneration >= deployment.Generation && status.UpdatedReplicas == replicas &&
				status.AvailableReplicas == replicas && status.UnavailableReplicas == 0 {
				return true, nil
			}
			return false, fmt.Errorf("generation %d/%d, replicas updated %d/%d, available %d/%d, unavailable %d",
				status.ObservedGeneration, deployment.Generation, status.UpdatedReplicas, replicas,
				status.AvailableReplicas, replicas, status.UnavailableReplicas)
		}))
		restored = true
	}
	t.Cleanup(restore)
	scale.Spec.Replicas = 0
	_, err = deployments.UpdateScale(ctx, name, scale, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, testutil.WaitForConditionWithDescription(ctx, testutil.DefaultWaitConfig(), "all controllers stopped", func(ctx context.Context) (bool, error) {
		pods, listErr := client.CoreV1().Pods(ControllerNamespace).List(ctx, metav1.ListOptions{LabelSelector: LabelSelectorController})
		return listErr == nil && len(pods.Items) == 0, listErr
	}))
	return restore
}
