// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

const certManagerWebhookDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: cert-manager-webhook
  namespace: cert-manager
spec:
  strategy:
    rollingUpdate:
      maxSurge: 1
  template:
    spec:
      containers:
        - name: cert-manager-webhook
          args:
            - --v=2
            - --secure-port=10250
            - --dynamic-serving-dns-names=cert-manager-webhook.$(POD_NAMESPACE).svc
          ports:
            - name: https
              containerPort: 10250
            - name: healthcheck
              containerPort: 6080
      nodeSelector:
        kubernetes.io/os: linux
`

const certManagerWebhookService = `apiVersion: v1
kind: Service
metadata:
  name: cert-manager-webhook
  namespace: cert-manager
spec:
  ports:
    - name: https
      port: 443
      targetPort: https
`

func certManagerWebhookConfiguration(kind, path string) string {
	return `apiVersion: admissionregistration.k8s.io/v1
kind: ` + kind + `
metadata:
  name: cert-manager-webhook
webhooks:
  - name: webhook.cert-manager.io
    clientConfig:
      service:
        name: cert-manager-webhook
        namespace: cert-manager
        path: ` + path + `
`
}

func certManagerManifest(documents ...string) []byte {
	return []byte(strings.Join(documents, "---\n"))
}

func decodeCertManagerManifest(t *testing.T, manifest []byte) map[string]map[string]any {
	t.Helper()
	objects := map[string]map[string]any{}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object.Object)
		if errors.Is(err, io.EOF) {
			return objects
		}
		require.NoError(t, err)
		objects[object.GetKind()] = object.Object
	}
}

func TestLoopbackCertManagerWebhookServesHTTP2ToTheAPIServer(t *testing.T) {
	manifest := certManagerManifest(certManagerWebhookService, certManagerWebhookDeployment,
		certManagerWebhookConfiguration(kindValidatingWebhook, "/validate"),
		certManagerWebhookConfiguration(kindMutatingWebhook, "/mutate"))
	patched, err := loopbackCertManagerWebhook(manifest)
	require.NoError(t, err)
	objects := decodeCertManagerManifest(t, patched)

	var deployment appsv1.Deployment
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(objects[kindDeployment], &deployment))
	require.Equal(t, appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}, deployment.Spec.Strategy)
	pod := deployment.Spec.Template.Spec
	require.True(t, pod.HostNetwork)
	require.Equal(t, corev1.DNSClusterFirstWithHostNet, pod.DNSPolicy)
	require.Equal(t, map[string]string{"kubernetes.io/os": "linux", controlPlaneLabel: ""}, pod.NodeSelector)
	require.Contains(t, pod.Tolerations, corev1.Toleration{Key: controlPlaneLabel, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
	require.Equal(t, []string{"--v=2", "--secure-port=10260", "--dynamic-serving-dns-names=cert-manager-webhook.$(POD_NAMESPACE).svc", "--dynamic-serving-dns-names=localhost"}, pod.Containers[0].Args)
	require.Equal(t, []corev1.ContainerPort{{Name: "https", ContainerPort: 10260}, {Name: "healthcheck", ContainerPort: 6080}}, pod.Containers[0].Ports)

	var validating admissionregistrationv1.ValidatingWebhookConfiguration
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(objects[kindValidatingWebhook], &validating))
	require.Equal(t, admissionregistrationv1.WebhookClientConfig{URL: new("https://localhost:10260/validate")}, validating.Webhooks[0].ClientConfig)
	var mutating admissionregistrationv1.MutatingWebhookConfiguration
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(objects[kindMutatingWebhook], &mutating))
	require.Equal(t, admissionregistrationv1.WebhookClientConfig{URL: new("https://localhost:10260/mutate")}, mutating.Webhooks[0].ClientConfig)

	require.Equal(t, decodeCertManagerManifest(t, []byte(certManagerWebhookService))["Service"], objects["Service"])
}

func TestLoopbackCertManagerWebhookRejectsAnUnexpectedManifest(t *testing.T) {
	validating := certManagerWebhookConfiguration(kindValidatingWebhook, "/validate")
	mutating := certManagerWebhookConfiguration(kindMutatingWebhook, "/mutate")
	tests := []struct {
		name     string
		manifest []byte
		want     string
	}{
		{name: "missing mutating webhook", manifest: certManagerManifest(certManagerWebhookDeployment, validating), want: "no MutatingWebhookConfiguration"},
		{name: "missing deployment", manifest: certManagerManifest(validating, mutating), want: "no Deployment"},
		{name: "renamed port flag", manifest: certManagerManifest(strings.ReplaceAll(certManagerWebhookDeployment, "--secure-port=", "--port="), validating, mutating), want: "no --secure-port= flag"},
		{name: "URL client config", manifest: certManagerManifest(certManagerWebhookDeployment, validating, strings.ReplaceAll(mutating, "service:", "urls:")), want: "no service path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loopbackCertManagerWebhook(tt.manifest)
			require.ErrorContains(t, err, tt.want)
		})
	}
}
