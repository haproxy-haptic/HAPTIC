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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

const (
	certificatePEMType   = "CERTIFICATE"
	rsaPrivateKeyPEMType = "RSA PRIVATE KEY"
)

// NamespaceForTest creates a namespace with a unique name derived from the
// test name and registers a t.Cleanup that deletes the namespace (cascading
// cleanup of all resources inside).
//
// The naming convention is "e2e-<short-test-name>-<rand>" — short enough to
// stay under the 63-char DNS label limit, unique enough that parallel tests
// don't collide.
func NamespaceForTest(ctx context.Context, t *testing.T, client klient.Client) string {
	t.Helper()
	name := uniqueNamespaceName(t.Name())

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := client.Resources().Create(ctx, ns); err != nil {
		t.Fatalf("create namespace %q: %v", name, err)
	}

	t.Cleanup(func() {
		if shouldKeepNamespace() {
			t.Logf("KEEP_NAMESPACE=true: keeping %s", name)
			return
		}
		bg := context.Background()
		if err := client.Resources().Delete(bg, ns); err != nil {
			t.Logf("delete namespace %q: %v (best-effort)", name, err)
		}
	})

	return name
}

// uniqueNamespaceName builds a DNS-label-safe namespace name from a test
// name and a 4-byte random suffix.
func uniqueNamespaceName(testName string) string {
	short := dnsLabelify(testName)
	if len(short) > 40 {
		short = short[:40]
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; fall back to fixed suffix.
		return "e2e-" + short + "-x"
	}
	return "e2e-" + short + "-" + hex.EncodeToString(b[:])
}

// dnsLabelify converts a Go test name to a DNS-1123-label-friendly string.
// Replaces underscores and slashes with dashes and lowercases.
func dnsLabelify(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		default:
			// Collapse runs of non-alphanumerics to a single dash.
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	// Trim leading/trailing dashes.
	for len(out) > 0 && out[0] == '-' {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// shouldKeepNamespace mirrors tests/acceptance ShouldKeepNamespace.
func shouldKeepNamespace() bool {
	// Lazily imported via os to avoid cyclic deps with env.go.
	v, _ := lookupEnv("KEEP_NAMESPACE")
	return v == "true"
}

// BackendRef identifies a per-test backend (Deployment + Service) the
// tests deploy as their HTTP upstream. Each test owns its own backend so
// HAProxy reaches a real Endpoint when the controller renders it — the
// chart's routing logic only emits server lines for Services with at
// least one ready Endpoint, so ExternalName aliases don't work here.
//
// Backends are stateless and small (echo-server image), and they tear
// down with the test namespace.
type BackendRef struct {
	// Service is the per-test Service name.
	Service string
	// Port is the Service's HTTP port.
	Port int32
}

// EchoServerBackend is the conventional name + port for the echo-server
// backend. Tests that don't care about the specifics use this directly.
var EchoServerBackend = BackendRef{Service: "echo-server", Port: 80}

// echoServerImage is the upstream image used for routing tests. echo-server
// returns the incoming request as JSON, which lets assertions check headers
// and rewritten paths without setting up custom backends.
const echoServerImage = "ealen/echo-server@sha256:ec8a6e95890df937a1eb5fafca033a32172d4f43c1fea1f302931d5f230a137f"

// NewEchoServerBackend deploys an echo-server Deployment + Service into the
// test namespace and waits for at least one endpoint to be Ready. Returns
// the BackendRef the caller passes to NewIngress / similar fixtures.
//
// Cleaned up automatically when the test namespace is deleted in t.Cleanup.
func NewEchoServerBackend(ctx context.Context, t *testing.T, client klient.Client, namespace string) BackendRef {
	t.Helper()
	return NewEchoServerBackendWithResources(ctx, t, client, namespace, corev1.ResourceRequirements{})
}

// NewEchoServerBackendWithResources applies container resources to the per-test backend.
func NewEchoServerBackendWithResources(ctx context.Context, t *testing.T, client klient.Client, namespace string, requirements corev1.ResourceRequirements) BackendRef {
	t.Helper()
	mutate := func(deployment *appsv1.Deployment) {
		deployment.Spec.Template.Spec.Containers[0].Resources = requirements
	}
	if err := applyNamedEchoServerBackend(ctx, client, namespace, EchoServerBackend, 1, mutate); err != nil {
		t.Fatalf("%v", err)
	}
	if err := waitForServiceEndpointReady(ctx, client, namespace, EchoServerBackend.Service); err != nil {
		t.Fatalf("echo-server endpoint not ready: %v", err)
	}
	return EchoServerBackend
}

// PodReadinessGate is the condition HAPTIC sets once every HAProxy pod routes
// to a pod (controller.templateLibraries.kubernetesBackends.podReadinessGate).
const PodReadinessGate = "haproxy-haptic.org/load-balancer-ready"

// NewGatedEchoServerBackend is NewEchoServerBackend with the HAPTIC readiness
// gate on its pod template. The wait for a ready endpoint therefore also waits
// for HAPTIC to set the gate.
func NewGatedEchoServerBackend(ctx context.Context, t *testing.T, client klient.Client, namespace string) BackendRef {
	t.Helper()
	gate := func(deployment *appsv1.Deployment) {
		deployment.Spec.Template.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: PodReadinessGate}}
	}
	if err := applyNamedEchoServerBackend(ctx, client, namespace, EchoServerBackend, 1, gate); err != nil {
		t.Fatalf("%v", err)
	}
	if err := waitForServiceEndpointReady(ctx, client, namespace, EchoServerBackend.Service); err != nil {
		t.Fatalf("gated echo-server endpoint not ready: %v", err)
	}
	return EchoServerBackend
}

// NewNamedEchoServerBackend is NewEchoServerBackend for a caller-chosen Service
// name (port 80), so one test namespace can host more than one echo backend —
// e.g. a primary route target plus a separate request-mirror target. Deploys
// the Deployment + Service, waits for a ready endpoint, and returns the
// BackendRef.
func NewNamedEchoServerBackend(ctx context.Context, t *testing.T, client klient.Client, namespace, name string) BackendRef {
	t.Helper()
	ref := BackendRef{Service: name, Port: 80}
	if err := applyNamedEchoServerBackend(ctx, client, namespace, ref, 1); err != nil {
		t.Fatalf("%v", err)
	}
	if err := waitForServiceEndpointReady(ctx, client, namespace, ref.Service); err != nil {
		t.Fatalf("echo-server %q endpoint not ready: %v", name, err)
	}
	return ref
}

// NewEchoServerBackendWithReplicas deploys an echo-server backend with a
// caller-chosen replica count and waits until that many endpoints are ready.
// Several ready endpoints let a test observe HAPTIC's pod-named backend
// servers scaling — one server per pod, no reserved slot pool.
func NewEchoServerBackendWithReplicas(ctx context.Context, t *testing.T, client klient.Client, namespace, name string, replicas int32) BackendRef {
	t.Helper()
	ref := BackendRef{Service: name, Port: 80}
	if err := applyNamedEchoServerBackend(ctx, client, namespace, ref, replicas); err != nil {
		t.Fatalf("%v", err)
	}
	if err := waitForServiceEndpointsReady(ctx, client, namespace, ref.Service, int(replicas)); err != nil {
		t.Fatalf("echo-server %q endpoints not ready: %v", name, err)
	}
	return ref
}

// applyEchoServerBackend creates the echo-server Deployment + Service in the
// given namespace WITHOUT waiting for endpoint readiness and WITHOUT failing
// the test directly. Split out of NewEchoServerBackend so bulk seeders (the
// scale tier deploys one backend per namespace across dozens of namespaces)
// can fan the creates out first and wait for readiness across all namespaces
// in a single condition instead of paying a sequential per-namespace wait.
func applyEchoServerBackend(ctx context.Context, client klient.Client, namespace string) error {
	return applyNamedEchoServerBackend(ctx, client, namespace, EchoServerBackend, 1)
}

// applyNamedEchoServerBackend is applyEchoServerBackend parameterised by the
// BackendRef and replica count, so callers can deploy additional echo backends
// under distinct Service names in the same namespace, optionally multi-replica.
func applyNamedEchoServerBackend(ctx context.Context, client klient.Client, namespace string, ref BackendRef, replicas int32, mutators ...func(*appsv1.Deployment)) error {
	labels := map[string]string{"app": ref.Service}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ref.Service,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "server",
						Image:           echoServerImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{{
							Name:          HTTPPortName,
							ContainerPort: 80,
							Protocol:      corev1.ProtocolTCP,
						}},
						// Readiness probe so K8s only marks the pod
						// Ready when port 80 actually responds, not the
						// instant the container process starts.
						//
						// Without it, kubelet marks the pod Running
						// (and therefore Ready, since there's no other
						// gating probe) the moment the Node process
						// starts. EndpointSlice gets the pod with
						// `conditions.ready=true`. HAPTIC includes it
						// in the rendered backend. HAProxy dispatches.
						// Meanwhile Node is still loading modules and
						// hasn't called `app.listen(80)` yet — the
						// kernel RSTs the SYN. Visible on the wire as
						// immediate RST in the ~200-500 ms after
						// container start (captured in tcpdump during
						// MR !1019 debugging).
						//
						// With this probe, K8s waits to mark Ready
						// until /:80 answers, by which point Node has
						// bound the listener. Matches how every
						// K8s production deployment configures non-
						// trivial app backends — the contract is
						// "Ready means responding to traffic", and a
						// fixture that simulates a real production
						// backend has to honour it.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/",
									Port: intstr.FromString(HTTPPortName),
								},
							},
							// A 1 s period with success threshold 1
							// keeps startup → Ready tight. Failures
							// need 3 probes of up to 5 s, so a starved
							// CI node does not flap every endpoint (#304).
							PeriodSeconds:    1,
							SuccessThreshold: 1,
							FailureThreshold: 3,
							TimeoutSeconds:   5,
						},
					}},
				},
			},
		},
	}
	for _, mutate := range mutators {
		mutate(deployment)
	}
	if err := client.Resources(namespace).Create(ctx, deployment); err != nil {
		return fmt.Errorf("create echo-server Deployment %s/%s: %w", namespace, ref.Service, err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ref.Service,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:       HTTPPortName,
				Port:       ref.Port,
				TargetPort: intstr.FromString(HTTPPortName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	if err := client.Resources(namespace).Create(ctx, svc); err != nil {
		return fmt.Errorf("create echo-server Service %s/%s: %w", namespace, ref.Service, err)
	}
	return nil
}

// waitForServiceEndpointReady blocks until the named Service has at least
// one ready endpoint in its EndpointSlice. The chart's template logic emits
// no backend servers until this is true.
func waitForServiceEndpointReady(ctx context.Context, client klient.Client, namespace, serviceName string) error {
	cfg := testutil.FastWaitConfig()
	cfg.Timeout = DefaultPerTestSetupTimeout

	return testutil.WaitForConditionWithDescription(ctx, cfg, "service "+namespace+"/"+serviceName+" has ready endpoint",
		func(ctx context.Context) (bool, error) {
			if err := serviceHasReadyEndpoint(ctx, client, namespace, serviceName); err != nil {
				return false, err
			}
			return true, nil
		})
}

// serviceHasReadyEndpoint is the one-shot readiness predicate behind
// waitForServiceEndpointReady: nil when the Service has at least one ready
// endpoint, an explanatory error otherwise. Exposed separately so bulk
// waiters (the scale tier checks dozens of namespaces in one condition) can
// evaluate it without nesting wait loops.
func serviceHasReadyEndpoint(ctx context.Context, client klient.Client, namespace, serviceName string) error {
	var slices discoveryv1.EndpointSliceList
	if err := client.Resources(namespace).List(ctx, &slices,
		resources.WithLabelSelector("kubernetes.io/service-name="+serviceName)); err != nil {
		return err
	}
	for index := range slices.Items {
		sl := &slices.Items[index]
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				return nil
			}
		}
	}
	return fmt.Errorf("no ready endpoints in %d slices", len(slices.Items))
}

// waitForServiceEndpointsReady blocks until the Service has at least minReady
// ready endpoints across its EndpointSlices — the property a multi-replica
// backend needs before HAPTIC renders one pod-named server per pod.
func waitForServiceEndpointsReady(ctx context.Context, client klient.Client, namespace, serviceName string, minReady int) error {
	cfg := testutil.FastWaitConfig()
	cfg.Timeout = DefaultPerTestSetupTimeout

	return testutil.WaitForConditionWithDescription(ctx, cfg,
		fmt.Sprintf("service %s/%s has %d ready endpoints", namespace, serviceName, minReady),
		func(ctx context.Context) (bool, error) {
			ready, err := countReadyEndpoints(ctx, client, namespace, serviceName)
			if err != nil {
				return false, err
			}
			return ready >= minReady, nil
		})
}

func countReadyEndpoints(ctx context.Context, client klient.Client, namespace, serviceName string) (int, error) {
	var slices discoveryv1.EndpointSliceList
	if err := client.Resources(namespace).List(ctx, &slices,
		resources.WithLabelSelector("kubernetes.io/service-name="+serviceName)); err != nil {
		return 0, err
	}
	ready := 0
	for index := range slices.Items {
		sl := &slices.Items[index]
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				ready++
			}
		}
	}
	return ready, nil
}

// IngressSpec captures the minimum a routing test needs to declare.
type IngressSpec struct {
	// Name is the Ingress resource name within the test namespace.
	Name string
	// Host is the request Host: header to match.
	Host string
	// Path is the request path prefix to match (default "/").
	Path string
	// PathType is the Ingress pathType ("Prefix" (default), "Exact", or
	// "ImplementationSpecific"). The latter is required by the
	// haproxy-ingress library's regex-path support, which keys on the
	// pathType + the haproxy-ingress.github.io/path-type annotation.
	PathType string
	// Backend identifies the upstream Service in the test namespace,
	// typically created by NewEchoServerBackend.
	BackendService string
	BackendPort    int32
	// Annotations are passed through verbatim. Useful for nginx.ingress.*
	// annotations the chart's nginx-ingress library understands.
	Annotations map[string]string
	// TLSSecretName, if non-empty, populates spec.tls[] with the named
	// Secret covering Host. The Secret must already exist in the test
	// namespace (use NewTLSSecret to create one).
	TLSSecretName string
}

// buildIngress materialises an `IngressSpec` into a typed Ingress object
// without touching the apiserver. Shared by `NewIngress` (apply, expect
// success) and `NewIngressExpectDenied` (apply, expect admission webhook
// rejection).
func buildIngress(namespace string, spec *IngressSpec) *networkingv1.Ingress {
	path := spec.Path
	if path == "" {
		path = "/"
	}
	pathType := networkingv1.PathTypePrefix
	switch spec.PathType {
	case "Exact":
		pathType = networkingv1.PathTypeExact
	case "ImplementationSpecific":
		pathType = networkingv1.PathTypeImplementationSpecific
	}
	ingressClassName := "haptic"

	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.Name,
			Namespace:   namespace,
			Annotations: spec.Annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClassName,
			Rules: []networkingv1.IngressRule{
				{
					Host: spec.Host,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     path,
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: spec.BackendService,
											Port: networkingv1.ServiceBackendPort{
												Number: spec.BackendPort,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	if spec.TLSSecretName != "" {
		ing.Spec.TLS = []networkingv1.IngressTLS{{
			Hosts:      []string{spec.Host},
			SecretName: spec.TLSSecretName,
		}}
	}
	return ing
}

// NewIngress applies the Ingress described by spec. The IngressClass name
// is "haptic" (matches the helm chart's default).
// isTransientAdmissionDenial reports whether err is one of the admission
// denials the webhook itself declares transient (see NewIngress). Both share
// the short budget: a reinitialization (a config push — lost leadership no
// longer reinitializes) re-registers validators within seconds, and a
// genuinely missing reference should fail fast.
func isTransientAdmissionDenial(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "was not found") ||
		strings.Contains(err.Error(), "retry after controller initialization")
}

func NewIngress(ctx context.Context, t *testing.T, client klient.Client, namespace string, spec *IngressSpec) *networkingv1.Ingress {
	t.Helper()

	ing := buildIngress(namespace, spec)
	// Retry the two admission denials the webhook itself declares transient.
	//
	// "was not found": the webhook renders the proposed Ingress against the
	// CONTROLLER's stores, and those are eventually consistent — a fixture that
	// just created a ConfigMap or Secret the Ingress references can win the race
	// to the API server and still lose it to the controller's watch.
	//
	// "retry after controller initialization": the replica that answered is
	// reinitializing (a config push; lost leadership no longer reinitializes)
	// and its validators are not registered yet; the denial says to retry.
	//
	// Scoped deliberately: only these messages are retried, and only for a
	// bounded window, so a genuine denial still fails with the same error
	// instead of being masked.
	createErr := client.Resources(namespace).Create(ctx, ing)
	if isTransientAdmissionDenial(createErr) {
		_ = testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(c context.Context) (bool, error) {
			ing = buildIngress(namespace, spec)
			createErr = client.Resources(namespace).Create(c, ing)
			return createErr == nil || !isTransientAdmissionDenial(createErr), nil
		})
	}
	if createErr != nil {
		t.Fatalf("create Ingress %s/%s: %v", namespace, spec.Name, createErr)
	}

	// Wait for the controller to report THIS Ingress deployed. The address is
	// written under the "deployed" status variant, which statusapplier applies
	// only when every HAProxy replica took the config — so a non-empty address
	// is exactly the property the caller needs. Without it the caller races
	// multi-pod reload: NodePort distributes fresh handshakes round-robin
	// between converged and still-reloading pods.
	waitForIngressDeployed(ctx, t, client, namespace, spec.Name)

	// Delete the Ingress explicitly before the namespace teardown
	// cascades, so the controller observes the Ingress disappear before
	// any Secrets/ConfigMaps it referenced. Without this, a parallel test's
	// webhook validation can fire while this Ingress is still in the
	// controller's resource store but its referenced Secret has already
	// been removed by the cascade — the dry-run render then fails because
	// of the orphaned reference, denying admission for the unrelated
	// resource.
	//
	// The namespace-level wait is registered first because t.Cleanup runs
	// in LIFO order: all Ingresses are deleted before one convergence wait,
	// and the namespace teardown registered by NamespaceForTest runs last.
	registerControllerForgetNamespaceCleanup(t, client, namespace)
	t.Cleanup(func() {
		bg := context.Background()
		if err := client.Resources(namespace).Delete(bg, ing); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("delete Ingress %s/%s: %v (best-effort)", namespace, spec.Name, err)
		}
	})
	return ing
}

// NewIngressExpectDenied applies the Ingress and asserts that the
// admission webhook rejects it. Returns the error the apiserver
// returned — assertions on the error message belong to the caller
// (e.g. a webhook denial includes the controller's diagnostic, an
// unrelated apiserver failure does not).
//
// On a successful Create the helper calls `t.Fatalf`: a passing
// Create means the validator stack didn't catch the operator's typo,
// which is itself the bug the test exists to detect. We do NOT
// register a delete cleanup — the resource was either rejected
// (nothing to clean up) or the test already failed.
func NewIngressExpectDenied(ctx context.Context, t *testing.T, client klient.Client, namespace string, spec *IngressSpec) error {
	t.Helper()

	ing := buildIngress(namespace, spec)
	err := client.Resources(namespace).Create(ctx, ing)
	if err == nil {
		// Best-effort cleanup so the unexpected resource doesn't leak
		// into a downstream test's namespace teardown wait. We don't
		// gate the t.Fatalf below on this — the test outcome is
		// already decided.
		_ = client.Resources(namespace).Delete(context.Background(), ing)
		t.Fatalf(
			"expected admission webhook to deny Ingress %s/%s, but Create succeeded",
			namespace, spec.Name,
		)
	}
	return err
}

// NewTLSSecret generates a self-signed certificate for the given hosts and
// writes it as a kubernetes.io/tls Secret in the test namespace. Returns
// the Secret name so the caller can reference it from IngressSpec.TLSSecretName.
//
// The cert is short-lived (1 year) and self-signed; tests that need to
// verify the chain should use httpclient.WithClientCert with a CA bundle
// instead. This helper is for tests that just want HTTPS termination to
// work and don't care about chain validation (httpclient defaults to
// insecure-skip-verify against dev-env certs).
func NewTLSSecret(ctx context.Context, t *testing.T, client klient.Client, namespace, name string, hosts []string) string {
	t.Helper()
	certPEM, keyPEM, err := generateSelfSignedCert(hosts)
	if err != nil {
		t.Fatalf("generate self-signed cert for %v: %v", hosts, err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}
	if err := client.Resources(namespace).Create(ctx, secret); err != nil {
		t.Fatalf("create TLS Secret %s/%s: %v", namespace, name, err)
	}
	return name
}

// generateSelfSignedCert returns a PEM-encoded certificate and key for a
// fresh self-signed server cert covering the given DNS hosts. Used by
// NewTLSSecret. Mirrors the webhook-cert generator's RSA-2048 / 1-year
// shape so cert-related quirks behave consistently.
func generateSelfSignedCert(hosts []string) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	cn := "test-server"
	if len(hosts) > 0 {
		cn = hosts[0]
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     hosts,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: certificatePEMType, Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: rsaPrivateKeyPEMType, Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}
