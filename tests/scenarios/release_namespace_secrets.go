// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/haproxy-haptic/HAPTIC/tests/admission"
	"github.com/haproxy-haptic/HAPTIC/tests/fixtures"
	"github.com/haproxy-haptic/HAPTIC/tests/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
	"github.com/haproxy-haptic/HAPTIC/tests/traffic"
)

const (
	secretModeSetting  = "controller.rbac.releaseNamespaceSecretsOnly"
	foreignNamespace   = "scoped-elsewhere"
	scopedHost         = "tls.scoped.test"
	scopedPath         = "/scoped-check"
	scopedBackend      = "scoped-backend"
	scopedIngressClass = "haptic"
)

type releaseNamespaceSecrets struct {
	*Session
	roots *x509.CertPool
}

// ReleaseNamespaceSecrets installs the chart with releaseNamespaceSecretsOnly
// and proves the controller can read Secrets only in its release namespace,
// still serves TLS from there, and rejects references elsewhere at admission
// and, past admission, at reconciliation.
func (s *Session) ReleaseNamespaceSecrets(ctx context.Context) error {
	r := &releaseNamespaceSecrets{Session: s}
	for _, phase := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"install", s.installReleaseNamespaceSecrets},
		{"service account permissions", s.requireReleaseNamespaceSecretRBAC},
		{"TLS from the release namespace", r.requireReleaseNamespaceTLS},
		{"references outside the release namespace", s.requireForeignSecretsRejected},
		{"a reference that bypassed admission", r.requireForeignSecretWithheld},
	} {
		s.Infof("Release-namespace Secrets: %s", phase.name)
		if err := phase.run(ctx); err != nil {
			return fmt.Errorf("%s: %w", phase.name, err)
		}
	}
	if err := s.RequireNoControllerRestarts(ctx); err != nil {
		return err
	}
	if err := s.CheckControllerOutput(ctx); err != nil {
		return err
	}
	s.Infof("PASS: the controller reads Secrets only in %s, serves TLS from there, and rejects Secrets elsewhere", s.Namespace)
	return nil
}

func (s *Session) installReleaseNamespaceSecrets(ctx context.Context) error {
	if _, err := s.Kube(ctx, nil, "wait", "--for=condition=Ready", "node", "--all", "--timeout=180s"); err != nil {
		return err
	}
	if err := s.LoadControllerImage(ctx); err != nil {
		return err
	}
	if err := s.LoadSPOAImage(ctx); err != nil {
		return err
	}
	args := []string{helmInstall, s.Release, filepath.Join(s.Root, "charts", chartName), helmCreateNamespace, "--timeout=10m",
		"--set", secretModeSetting + "=true"}
	if _, err := s.Helm(ctx, append(args, s.ImageValues()...)...); err != nil {
		return err
	}
	if err := s.WaitHAProxyReady(ctx, 420*time.Second); err != nil {
		return err
	}
	return s.WaitValidated(ctx, 180*time.Second)
}

func (s *Session) requireReleaseNamespaceSecretRBAC(ctx context.Context) error {
	deployment, err := s.Deployment(ctx, "-controller")
	if err != nil {
		return err
	}
	identity := "system:serviceaccount:" + s.Namespace + ":" + deployment.Spec.Template.Spec.ServiceAccountName
	everywhere, secrets := "--all-namespaces", "secrets"
	for _, check := range []struct {
		resource, scope string
		allowed         bool
	}{
		{secrets, everywhere, false},
		{secrets, "--namespace=" + foreignNamespace, false},
		{secrets, "--namespace=" + s.Namespace, true},
		{"ingresses.networking.k8s.io", everywhere, true},
	} {
		for _, verb := range []string{"list", "watch", "get"} {
			allowed, err := s.canI(ctx, identity, verb, check.resource, check.scope)
			if err != nil {
				return err
			}
			if allowed != check.allowed {
				return fmt.Errorf("kubectl auth can-i %s %s %s --as=%s: got %t, want %t", verb, check.resource, check.scope, identity, allowed, check.allowed)
			}
		}
	}
	return nil
}

// canI reads kubectl's answer; "no" exits 1, which is an answer, not a failure.
func (s *Session) canI(ctx context.Context, identity, verb, resource, scope string) (bool, error) {
	result, err := s.KubeUnscoped(ctx, nil, "auth", "can-i", verb, resource, scope, "--as="+identity)
	switch answer := strings.TrimSpace(result.Stdout); {
	case answer == "yes" && err == nil:
		return true, nil
	case answer == "no" && result.ExitCode == 1:
		return false, nil
	default:
		return false, fmt.Errorf("kubectl auth can-i %s %s %s: %q: %w", verb, resource, scope, answer, err)
	}
}

func (r *releaseNamespaceSecrets) requireReleaseNamespaceTLS(ctx context.Context) error {
	s := r.Session
	certificate, err := fixtures.NewCertificate(&x509.Certificate{
		SerialNumber:          big.NewInt(409),
		Subject:               pkix.Name{CommonName: scopedHost},
		DNSNames:              []string{scopedHost},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}, fixtures.ECDSAP256, nil)
	if err != nil {
		return err
	}
	manifest := scopedBackendManifest(s.Namespace) + tlsSecretManifest(s.Namespace, "scoped-tls", certificate) +
		ingressManifest(s.Namespace, "scoped-https", scopedHost, "scoped-tls", nil)
	if _, err := s.KubeUnscoped(ctx, strings.NewReader(manifest), "apply", "-f", "-"); err != nil {
		return err
	}
	if _, err := s.Kube(ctx, nil, "rollout", "status", "deployment/"+scopedBackend, "--timeout=180s"); err != nil {
		return err
	}
	r.roots = x509.NewCertPool()
	r.roots.AddCert(certificate.Parsed)
	return r.probeScopedTLS(ctx)
}

func (r *releaseNamespaceSecrets) probeScopedTLS(ctx context.Context) error {
	s := r.Session
	pods, err := s.Pods(ctx, "loadbalancer")
	if err != nil {
		return err
	}
	pods = livePods(pods)
	if len(pods) == 0 {
		return errors.New("no running HAProxy pods")
	}
	ports, err := podRoutePorts(&pods[0])
	if err != nil {
		return err
	}
	roots := r.roots
	return traffic.Wait(ctx, &traffic.PodProbe{Client: s.Client, Target: "pod/" + pods[0].Name, Ports: ports, StartupTimeout: 30 * time.Second, RouteTimeout: 120 * time.Second, Interval: time.Second, Check: func(ctx context.Context, locals []int) error {
		client := httpclient.New(&httpclient.Config{Host: loopbackHost, HTTPPort: locals[0], HTTPSPort: locals[1], TLS: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}})
		defer client.CloseIdleConnections()
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		response, err := client.HTTPS(scopedHost, scopedPath).Do(attempt)
		if err != nil {
			return err
		}
		if response.Status != 200 || response.Echo == nil || !strings.HasPrefix(response.Echo.PodHostname, scopedBackend+"-") {
			return fmt.Errorf("https://%s%s did not reach %s with the release-namespace certificate: HTTP %d: %s", scopedHost, scopedPath, scopedBackend, response.Status, response.Body)
		}
		return nil
	}})
}

func (s *Session) requireForeignSecretsRejected(ctx context.Context) error {
	setup := namespaceManifest(foreignNamespace) + scopedBackendManifest(foreignNamespace)
	if _, err := s.KubeUnscoped(ctx, strings.NewReader(setup), "apply", "-f", "-"); err != nil {
		return err
	}
	// An Ingress without Secret references must still be admitted there.
	unscoped := s.Client
	unscoped.Namespace = ""
	admitted := ingressManifest(foreignNamespace, "foreign-plain", "plain.scoped.test", "", nil)
	if err := admission.Wait(ctx, unscoped, []byte(admitted), 180*time.Second); err != nil {
		return err
	}
	secrets := tlsSecretManifest(foreignNamespace, "foreign-tls", nil) + opaqueSecretManifest(foreignNamespace, "foreign-users", "auth", "user:$apr1$abc$def")
	if _, err := s.KubeUnscoped(ctx, strings.NewReader(secrets), "apply", "-f", "-"); err != nil {
		return err
	}
	for _, rejected := range []struct{ name, manifest, secret string }{
		{"Ingress TLS", ingressManifest(foreignNamespace, "foreign-https", "https.scoped.test", "foreign-tls", nil), foreignNamespace + "/foreign-tls"},
		{"basic-auth annotation", ingressManifest(foreignNamespace, "foreign-auth", "auth.scoped.test", "", map[string]string{
			"haproxy-haptic.org/auth-type":   "basic",
			"haproxy-haptic.org/auth-secret": "foreign-users",
		}), foreignNamespace + "/foreign-users"},
	} {
		result, err := s.KubeUnscoped(ctx, strings.NewReader(rejected.manifest), "create", "-f", "-")
		if err == nil {
			return fmt.Errorf("%s referencing Secret %s was admitted", rejected.name, rejected.secret)
		}
		for _, want := range []string{"denied the request", "secrets " + rejected.secret + " cannot be read", secretModeSetting} {
			if !strings.Contains(result.Stderr, want) {
				return fmt.Errorf("%s rejection does not name %q: %s", rejected.name, want, strings.TrimSpace(result.Stderr))
			}
		}
		s.Infof("Rejected %s: %s", rejected.name, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// requireForeignSecretWithheld creates a reference with the webhook removed:
// reconciliation must withhold that Ingress and keep serving the others.
func (r *releaseNamespaceSecrets) requireForeignSecretWithheld(ctx context.Context) error {
	s := r.Session
	if _, err := s.KubeUnscoped(ctx, nil, "delete", "validatingwebhookconfiguration", "-l", "app.kubernetes.io/instance="+s.Release, "--wait"); err != nil {
		return err
	}
	manifest := ingressManifest(foreignNamespace, "foreign-https", "https.scoped.test", "foreign-tls", nil)
	if _, err := s.KubeUnscoped(ctx, strings.NewReader(manifest), "create", "-f", "-"); err != nil {
		return err
	}
	want := "secrets " + foreignNamespace + "/foreign-tls cannot be read: " + secretModeSetting
	if err := poll(ctx, 120*time.Second, 2*time.Second, "InputRejected event on Ingress foreign-https", func(ctx context.Context) (testutil.PollResult, error) {
		list, err := readJSON[corev1.EventList](ctx, s, "get", "events", "--namespace="+foreignNamespace,
			"--field-selector=involvedObject.name=foreign-https,reason=InputRejected")
		if err != nil {
			return testutil.PollPending, err
		}
		for i := range list.Items {
			if strings.Contains(list.Items[i].Message, want) {
				return testutil.PollSucceeded, nil
			}
		}
		return testutil.PollPending, fmt.Errorf("no InputRejected event naming %q among %d events", want, len(list.Items))
	}); err != nil {
		return err
	}
	return r.probeScopedTLS(ctx)
}

func namespaceManifest(name string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\n", name)
}

func scopedBackendManifest(namespace string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: %[2]s, namespace: %[1]s}
spec:
  replicas: 1
  selector: {matchLabels: {app: %[2]s}}
  template:
    metadata: {labels: {app: %[2]s}}
    spec:
      containers:
        - name: echo
          image: ealen/echo-server@sha256:ec8a6e95890df937a1eb5fafca033a32172d4f43c1fea1f302931d5f230a137f
          ports: [{containerPort: 80}]
          readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 1}
          resources: {requests: {cpu: 10m, memory: 32Mi}, limits: {memory: 128Mi}}
---
apiVersion: v1
kind: Service
metadata: {name: %[2]s, namespace: %[1]s}
spec:
  selector: {app: %[2]s}
  ports: [{name: http, port: 80, targetPort: 80}]
---
`, namespace, scopedBackend)
}

// tlsSecretManifest stores certificate, or a placeholder pair when nil: a
// rejected reference never reads it.
func tlsSecretManifest(namespace, name string, certificate *fixtures.Certificate) string {
	crt, key := []byte("unused"), []byte("unused")
	if certificate != nil {
		crt, key = certificate.PEM, certificate.KeyPEM
	}
	return fmt.Sprintf("apiVersion: v1\nkind: Secret\ntype: kubernetes.io/tls\nmetadata: {name: %s, namespace: %s}\ndata:\n  tls.crt: %s\n  tls.key: %s\n---\n",
		name, namespace, base64.StdEncoding.EncodeToString(crt), base64.StdEncoding.EncodeToString(key))
}

func opaqueSecretManifest(namespace, name, key, value string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata: {name: %s, namespace: %s}\ndata:\n  %s: %s\n---\n",
		name, namespace, key, base64.StdEncoding.EncodeToString([]byte(value)))
}

func ingressManifest(namespace, name, host, tlsSecret string, annotations map[string]string) string {
	var meta strings.Builder
	fmt.Fprintf(&meta, "metadata:\n  name: %s\n  namespace: %s\n", name, namespace)
	if len(annotations) > 0 {
		meta.WriteString("  annotations:\n")
		for key, value := range annotations {
			fmt.Fprintf(&meta, "    %s: %q\n", key, value)
		}
	}
	tlsBlock := ""
	if tlsSecret != "" {
		tlsBlock = fmt.Sprintf("  tls:\n    - hosts: [%s]\n      secretName: %s\n", host, tlsSecret)
	}
	return fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: Ingress
%sspec:
  ingressClassName: %s
%s  rules:
    - host: %s
      http:
        paths:
          - path: %s
            pathType: Prefix
            backend: {service: {name: %s, port: {number: 80}}}
---
`, meta.String(), scopedIngressClass, tlsBlock, host, scopedPath, scopedBackend)
}
