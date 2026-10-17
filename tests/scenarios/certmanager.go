// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

// kubelet owns the webhook's default port 10250 on the host network.
const certManagerWebhookPort = 10260

const (
	securePortFlag        = "--secure-port="
	controlPlaneLabel     = "node-role.kubernetes.io/control-plane"
	kindValidatingWebhook = "ValidatingWebhookConfiguration"
	kindMutatingWebhook   = "MutatingWebhookConfiguration"
)

var certManagerWebhookURL = "https://localhost:" + strconv.Itoa(certManagerWebhookPort)

func certManagerManifestURL(version string) string {
	return "https://github.com/cert-manager/cert-manager/releases/download/" + version + "/cert-manager.yaml"
}

func (s *Session) applyCertManager(ctx context.Context, version string) error {
	content, err := fetchManifest(ctx, certManagerManifestURL(version))
	if err != nil {
		return err
	}
	if content, err = loopbackCertManagerWebhook(content); err != nil {
		return err
	}
	_, err = s.KubeUnscoped(ctx, bytes.NewReader(content), "apply", "-f", "-")
	return err
}

// loopbackCertManagerWebhook moves the webhook to the control plane's localhost, the only address kube-apiserver
// calls over HTTP/2; over HTTP/1.1 both ends close idle connections at 90 s and race admission calls (#300).
func loopbackCertManagerWebhook(manifest []byte) ([]byte, error) {
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	var output bytes.Buffer
	patched := map[string]bool{}
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode cert-manager manifest: %w", err)
		}
		if len(object.Object) == 0 {
			continue
		}
		if object.GetName() == certManagerWebhook {
			if err := patchCertManagerWebhook(&object); err != nil {
				return nil, fmt.Errorf("%s %s: %w", object.GetKind(), certManagerWebhook, err)
			}
			patched[object.GetKind()] = true
		}
		content, err := json.Marshal(object.Object)
		if err != nil {
			return nil, err
		}
		output.WriteString("---\n")
		output.Write(content)
		output.WriteByte('\n')
	}
	for _, kind := range []string{kindDeployment, kindValidatingWebhook, kindMutatingWebhook} {
		if !patched[kind] {
			return nil, fmt.Errorf("cert-manager manifest has no %s %s", kind, certManagerWebhook)
		}
	}
	return output.Bytes(), nil
}

func patchCertManagerWebhook(object *unstructured.Unstructured) error {
	var err error
	switch object.GetKind() {
	case kindDeployment:
		object.Object, err = hostNetworkWebhook(object.Object)
	case kindValidatingWebhook, kindMutatingWebhook:
		err = loopbackClientConfigs(object.Object)
	}
	return err
}

func hostNetworkWebhook(object map[string]any) (map[string]any, error) {
	var deployment appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object, &deployment); err != nil {
		return nil, err
	}
	pod := &deployment.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		return nil, fmt.Errorf("want one container, found %d", len(pod.Containers))
	}
	container := &pod.Containers[0]
	flag := slices.IndexFunc(container.Args, func(arg string) bool { return strings.HasPrefix(arg, securePortFlag) })
	port := slices.IndexFunc(container.Ports, func(port corev1.ContainerPort) bool { return port.Name == "https" })
	if flag < 0 || port < 0 {
		return nil, errors.New("webhook container has no " + securePortFlag + " flag or https port")
	}
	container.Args[flag] = securePortFlag + strconv.Itoa(certManagerWebhookPort)
	container.Args = append(container.Args, "--dynamic-serving-dns-names=localhost")
	container.Ports[port].ContainerPort = certManagerWebhookPort
	// A surge pod cannot bind the host ports its predecessor holds.
	deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	pod.HostNetwork = true
	pod.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	if pod.NodeSelector == nil {
		pod.NodeSelector = map[string]string{}
	}
	pod.NodeSelector[controlPlaneLabel] = ""
	pod.Tolerations = append(pod.Tolerations, corev1.Toleration{Key: controlPlaneLabel, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
	return runtime.DefaultUnstructuredConverter.ToUnstructured(&deployment)
}

func loopbackClientConfigs(object map[string]any) error {
	webhooks, _, err := unstructured.NestedSlice(object, "webhooks")
	if err != nil {
		return err
	}
	if len(webhooks) == 0 {
		return errors.New("no webhooks")
	}
	for _, value := range webhooks {
		webhook, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("webhook has invalid type %T", value)
		}
		path, found, err := unstructured.NestedString(webhook, "clientConfig", "service", "path")
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("webhook %v has no service path", webhook[fieldName])
		}
		unstructured.RemoveNestedField(webhook, "clientConfig", "service")
		if err := unstructured.SetNestedField(webhook, certManagerWebhookURL+path, "clientConfig", "url"); err != nil {
			return err
		}
	}
	return unstructured.SetNestedSlice(object, webhooks, "webhooks")
}

// requireCertManagerHTTP2 fails loudly if a cert-manager release stops serving HTTP/2, which would restore #300's race.
func (s *Session) requireCertManagerHTTP2(ctx context.Context) error {
	result, err := s.Run(ctx, "docker", "exec", s.Cluster.Name+"-control-plane", "curl", "--silent", "--insecure", "--http2",
		"--output", "/dev/null", "--write-out", "%{http_version}", certManagerWebhookURL+"/validate")
	if err != nil {
		return err
	}
	if version := strings.TrimSpace(result.Stdout); version != "2" {
		return fmt.Errorf("cert-manager's webhook at %s negotiated HTTP/%s, not HTTP/2, so admission calls race its 90 s idle close (#300)", certManagerWebhookURL, version)
	}
	return nil
}
