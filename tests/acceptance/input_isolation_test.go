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

//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/pkg/types"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/inputisolation"
)

const isolationAuthHash = "$2y$05$mN1WVk5Qnbg4QwdAdXbfz.8b3ceH6Q5KOVCKxR2IkNAfJgLi5pIKW"

const isolationAcceptanceTemplate = `global
  maxconn 2000

defaults
  mode http
  timeout connect 5s
  timeout client 50s
  timeout server 50s

{{ render "global-top-500-haproxytech-ingress-auth" }}
{{ render "features-951-raw-haproxy-config-permission" }}

frontend http_front
  bind :8080
  default_backend healthy
{% for _, ingress := range resources.ingresses.List() %}
  use_backend route_{{ ingress.Metadata.Name }} if { hdr(host) {{ ingress.Spec.Rules[0].Host }} }
{% end %}

{% for _, ingress := range resources.ingresses.List() %}
backend route_{{ ingress.Metadata.Name }}
{{ render "backend-directives-900-haptic-config-backend" }}
  server app 192.0.2.10:8080
{% end %}

backend healthy
{% for _, slice := range resources.endpointslices.List() %}
{% for i, endpoint := range slice.Endpoints %}
  server ep{{ i }} {{ endpoint.Addresses[0] }}:8080
{% end %}
{% end %}
`

func TestBasicAuthInputIsolation(t *testing.T) {
	testEnv.Test(t, buildBasicAuthInputIsolationFeature())
}

func buildBasicAuthInputIsolationFeature() types.Feature {
	return features.New("Malformed basic-auth Secret isolation").
		Setup(setupControllerEnv("test-input-isolation", func(o *ControllerEnvironmentOptions) { o.SkipCRDAndDeployment = true })).
		Setup(setupBasicAuthIsolation).
		Assess("bad hash is held while endpoints and admission advance", assessBasicAuthIsolation).
		Teardown(cleanupIsolationEnvironment).Feature()
}

func setupBasicAuthIsolation(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	return setupIsolationInputs(ctx, t, cfg, nil, false, false)
}

func setupIsolationInputs(ctx context.Context, t *testing.T, cfg *envconf.Config, mutate func(*networkingv1.Ingress), fetchedList, allowRawHAProxyConfig bool) context.Context {
	t.Helper()
	namespace, err := GetNamespaceFromContext(ctx)
	require.NoError(t, err)
	client, err := cfg.NewClient()
	require.NoError(t, err)
	labels := map[string]string{"input-isolation-test": namespace}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "basic-auth", Namespace: namespace, Labels: labels}, Data: map[string][]byte{"admin": []byte(isolationAuthHash)}}
	require.NoError(t, client.Resources().Create(ctx, secret))
	ingress := NewValidIngress(namespace, "protected")
	ingress.Labels = labels
	ingress.Annotations["haproxy.org/auth-secret"] = secret.Name
	if mutate != nil {
		mutate(ingress)
	}
	require.NoError(t, client.Resources().Create(ctx, ingress))
	endpoints := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "healthy", Namespace: namespace, Labels: labels},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Port: ptr.To(int32(8080))}},
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"192.0.2.10"}}},
	}
	require.NoError(t, client.Resources().Create(ctx, endpoints))
	config := NewHAProxyTemplateConfigBuilder(namespace, ControllerCRDName, ControllerSecretName).WithTemplate(isolationAcceptanceTemplate).Build()
	config.Spec.TemplateSnippets = isolationChartSnippets(ctx, t, namespace)
	if allowRawHAProxyConfig {
		config.Spec.TemplatingSettings.ExtraContext = runtime.RawExtension{Raw: []byte(`{"ingressPermissions":{"allowRawHAProxyConfig":true}}`)}
	}
	// The shipped backend snippet consumes its caller's ingress variable.
	snippetName := "backend-directives-900-haptic-config-backend"
	config.Spec.HAProxyConfig.Template = strings.Replace(config.Spec.HAProxyConfig.Template, `{{ render "backend-directives-900-haptic-config-backend" }}`, config.Spec.TemplateSnippets[snippetName].Template, 1)
	delete(config.Spec.TemplateSnippets, snippetName)
	config.Spec.WatchedResources = map[string]haproxyv1alpha1.WatchedResource{
		"ingresses":      {APIVersion: "networking.k8s.io/v1", Resources: "ingresses", EnableValidationWebhook: true},
		"secrets":        {APIVersion: "v1", Resources: "secrets", Store: "on-demand"},
		"endpointslices": {APIVersion: "discovery.k8s.io/v1", Resources: "endpointslices"},
	}
	for name := range config.Spec.WatchedResources {
		watch := config.Spec.WatchedResources[name]
		watch.IndexBy = []string{"metadata.namespace", "metadata.name"}
		watch.LabelSelector = "input-isolation-test=" + namespace
		config.Spec.WatchedResources[name] = watch
	}
	if fetchedList {
		require.NoError(t, SetupBlocklistServer(ctx, t, client, namespace, ValidBlocklistContent))
		config.Spec.HAProxyConfig.Template = strings.Replace(config.Spec.HAProxyConfig.Template, "  bind :8080", "  bind :8080\n  acl blocked_ips src -f {{ pathResolver.GetPath(\"blocked-ips.acl\", \"file\") }}\n  http-request deny if blocked_ips", 1)
		config.Spec.Files = NewHTTPStoreHAProxyTemplateConfig(namespace, ControllerCRDName, ControllerSecretName, false).Spec.Files
	}
	require.NoError(t, client.Resources().Create(ctx, config))
	cert, key, err := genSelfSignedServerCert(0x1234, webhookServiceDNS(namespace))
	require.NoError(t, err)
	rotateTLSSecret(ctx, t, client, namespace, WebhookCertSecretName, cert, key)
	require.NoError(t, client.Resources().Create(ctx, webhookEnabledControllerDeployment(namespace)))
	require.NoError(t, client.Resources().Create(ctx, NewWebhookService(namespace, webhookServiceName)))
	require.NoError(t, createControllerServices(ctx, client, namespace))
	require.NoError(t, WaitForPodReady(ctx, client, namespace, "app="+ControllerDeploymentName, DefaultTimeout))
	failurePolicy, sideEffects := admissionv1.Fail, admissionv1.SideEffectClassNone
	webhook := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name: "input-isolation.haproxy-haptic.org", AdmissionReviewVersions: []string{"v1"}, FailurePolicy: &failurePolicy, SideEffects: &sideEffects, TimeoutSeconds: ptr.To(int32(10)),
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}},
			ClientConfig:      admissionv1.WebhookClientConfig{CABundle: cert, Service: &admissionv1.ServiceReference{Namespace: namespace, Name: webhookServiceName, Path: ptr.To("/validate")}},
			Rules:             []admissionv1.RuleWithOperations{{Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update}, Rule: admissionv1.Rule{APIGroups: []string{"networking.k8s.io"}, APIVersions: []string{"v1"}, Resources: []string{"ingresses"}}}},
		}},
	}
	require.NoError(t, client.Resources().Create(ctx, webhook))
	debug, err := SetupDebugClient(ctx, client, Clientset(), namespace, 30*time.Second)
	require.NoError(t, err)
	waitIsolationWebhook(ctx, t, Clientset(), namespace, debug)
	return ctx
}

// Use the shipped templates so the August regression cannot drift from the chart's hash validation.
func isolationChartSnippets(ctx context.Context, t *testing.T, namespace string) map[string]haproxyv1alpha1.TemplateSnippet {
	t.Helper()
	output, err := exec.CommandContext(ctx, "helm", "template", "isolation", "../../charts/haptic", "--namespace", namespace, "--set", "controller.templateLibraries.haproxytech.enabled=true").Output()
	require.NoError(t, err)
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	wanted := map[string]bool{
		"global-top-500-haproxytech-ingress-auth": true, "haproxytech-auth-userlist-publications": true, "util-config-injection-kind": true,
		"backend-directives-900-haptic-config-backend": true, "features-951-raw-haproxy-config-permission": true,
		"util-waf-governance": true, "util-policy-waf-governance": true,
	}
	result := map[string]haproxyv1alpha1.TemplateSnippet{}
	for {
		var document struct {
			Spec struct {
				TemplateSnippets map[string]haproxyv1alpha1.TemplateSnippet `json:"templateSnippets"`
			} `json:"spec"`
		}
		err := decoder.Decode(&document)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		for name, snippet := range document.Spec.TemplateSnippets {
			if wanted[name] {
				result[name] = snippet
			}
		}
	}
	require.Len(t, result, len(wanted), "all selected chart snippets must be present")
	return result
}

func assessBasicAuthIsolation(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	namespace, client, clientset := readyControllerEnv(ctx, t, cfg)
	t.Cleanup(func() {
		if t.Failed() {
			pod, err := GetControllerPod(ctx, client, namespace)
			if err == nil {
				DumpPodLogs(ctx, t, clientset, pod)
			}
		}
	})
	debug, err := SetupDebugClient(ctx, client, clientset, namespace, 30*time.Second)
	require.NoError(t, err)
	waitIsolationConfig(ctx, t, debug, isolationAuthHash, "192.0.2.10:8080")
	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "basic-auth", metav1.GetOptions{})
	require.NoError(t, err)
	secret.Data["admin"] = []byte("not-a-password-hash")
	secret, err = clientset.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
	require.NoError(t, err, "Secrets are watched without admission")
	waitIsolationRejections(ctx, t, debug, secret, true)
	require.NoError(t, debug.waitFor(ctx, 30*time.Second, "Warning event on the rejected Secret", func(ctx context.Context) (bool, error) {
		list, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("involvedObject.uid", string(secret.UID)).String()})
		if err != nil {
			return false, err
		}
		for i := range list.Items {
			event := &list.Items[i]
			if event.Type == corev1.EventTypeWarning && event.Reason == "InputRejected" && event.InvolvedObject.ResourceVersion == secret.ResourceVersion {
				require.Equal(t, "Secret", event.InvolvedObject.Kind)
				require.Contains(t, event.Message, "password hash")
				require.Contains(t, event.Message, "Correct this resource")
				return true, nil
			}
		}
		return false, nil
	}))
	endpoints, err := clientset.DiscoveryV1().EndpointSlices(namespace).Get(ctx, "healthy", metav1.GetOptions{})
	require.NoError(t, err)
	endpoints.Endpoints[0].Addresses = []string{"192.0.2.20"}
	_, err = clientset.DiscoveryV1().EndpointSlices(namespace).Update(ctx, endpoints, metav1.UpdateOptions{})
	require.NoError(t, err)
	waitIsolationConfig(ctx, t, debug, isolationAuthHash, "192.0.2.20:8080")
	checkIsolationAdmission(ctx, t, client, namespace, "unrelated")
	waitIsolationConfig(ctx, t, debug, isolationAuthHash, "unrelated.example.test")
	replacement := restartIsolationController(ctx, t, client, clientset, namespace, debug)
	waitRestartedHashRejection(ctx, t, clientset, debug, namespace, replacement.CreationTimestamp.Time)
	advanceIsolationEndpoints(ctx, t, clientset, namespace, "192.0.2.30")
	waitIsolationConfig(ctx, t, debug, "192.0.2.30:8080")
	checkIsolationAdmission(ctx, t, client, namespace, "after-restart")
	waitIsolationConfig(ctx, t, debug, "after-restart.example.test", "192.0.2.30:8080")
	secret.Data = map[string][]byte{"repaired": []byte(isolationAuthHash)}
	secret, err = clientset.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
	require.NoError(t, err)
	waitIsolationRejections(ctx, t, debug, secret, false)
	waitIsolationConfig(ctx, t, debug, "user repaired password", "192.0.2.30:8080")
	return ctx
}

func checkIsolationAdmission(ctx context.Context, t *testing.T, client klient.Client, namespace, name string) {
	t.Helper()
	unrelated := NewValidIngress(namespace, name)
	unrelated.Labels = map[string]string{"input-isolation-test": namespace}
	unrelated.Annotations = nil
	unrelated.Spec.Rules[0].Host = name + ".example.test"
	require.NoError(t, client.Resources().Create(ctx, unrelated), "unrelated admission must succeed")
	unrelated.Annotations = map[string]string{"haproxy.org/auth-type": "invalid-auth-type"}
	err := client.Resources().Update(ctx, unrelated)
	require.ErrorContains(t, err, "denied", "admission must still reject invalid proposals")
}

func waitIsolationConfig(ctx context.Context, t *testing.T, debug *DebugClient, expected ...string) {
	t.Helper()
	require.NoError(t, debug.waitFor(ctx, 60*time.Second, "accepted input configuration", func(ctx context.Context) (bool, error) {
		rendered, err := debug.GetRenderedConfig(ctx)
		if err != nil {
			return false, err
		}
		for _, value := range expected {
			if !strings.Contains(rendered, value) {
				return false, nil
			}
		}
		if strings.Contains(rendered, "not-a-password-hash") {
			return false, fmt.Errorf("rejected hash reached rendered configuration")
		}
		return true, nil
	}))
}

func waitIsolationRejections(ctx context.Context, t *testing.T, debug *DebugClient, secret *corev1.Secret, rejected bool) {
	t.Helper()
	require.NoError(t, debug.waitFor(ctx, 60*time.Second, "input rejection state", func(ctx context.Context) (bool, error) {
		body, err := debug.proxyGet(ctx, "/debug/vars/inputRejections")
		if err != nil {
			return false, err
		}
		var state struct {
			Ready     bool                       `json:"ready"`
			Resources []inputisolation.Rejection `json:"resources"`
		}
		if err := json.Unmarshal(body, &state); err != nil {
			return false, err
		}
		if !state.Ready {
			return false, nil
		}
		if !rejected {
			return len(state.Resources) == 0, nil
		}
		for i := range state.Resources {
			resource := &state.Resources[i]
			if resource.Store == "secrets" && resource.Namespace == secret.Namespace && resource.Name == secret.Name && strings.Contains(resource.Reason, "password hash") {
				return true, nil
			}
		}
		return false, nil
	}))
}
