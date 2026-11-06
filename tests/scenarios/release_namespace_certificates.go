// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentapi "github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/tests/admission"
	"github.com/haproxy-haptic/HAPTIC/tests/fixtures"
	"github.com/haproxy-haptic/HAPTIC/tests/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/traffic"
)

const additionalCertificatesSetting = "controller.config.templatingSettings.extraContext.tls.additionalCertificates"

type additionalCertificate struct {
	name       string
	host       string
	dnsNames   []string
	annotation bool
}

func (r *releaseNamespaceSecrets) requireAdditionalCertificates(ctx context.Context) error {
	fallback, err := r.defaultCertificate(ctx)
	if err != nil {
		return err
	}
	if err := r.probeAdditionalFallback(ctx, fallback); err != nil {
		return err
	}
	certificates := []additionalCertificate{
		{name: "central-explicit", host: "tenant.new.scoped.test", dnsNames: []string{"*.new.scoped.test"}},
		{name: "central-annotated", host: "tenant.old.scoped.test", dnsNames: []string{"*.old.scoped.test", "old.scoped.test"}, annotation: true},
	}
	if err := r.additionalRoutes(ctx, certificates); err != nil {
		return err
	}
	for i, certificate := range certificates {
		issued, err := r.issueAdditionalCertificate(ctx, certificate, int64(640+i))
		if err != nil {
			return err
		}
		if err := r.probeAdditionalCertificate(ctx, certificate.host, issued.Parsed, true); err != nil {
			return err
		}
	}
	before, err := r.additionalCertificateWorkers(ctx)
	if err != nil {
		return err
	}
	renewed, err := r.issueAdditionalCertificate(ctx, certificates[0], 1640)
	if err != nil {
		return err
	}
	if err := r.probeAdditionalCertificate(ctx, certificates[0].host, renewed.Parsed, true); err != nil {
		return err
	}
	after, err := r.additionalCertificateWorkers(ctx)
	if err != nil {
		return err
	}
	if len(before) != len(after) {
		return errors.New("HAProxy fleet changed during certificate renewal")
	}
	for uid, worker := range before {
		if !worker.SameWorker(after[uid]) {
			return fmt.Errorf("HAProxy pod %s reloaded during certificate renewal", uid)
		}
	}
	r.Infof("Additional certificate renewal preserved all %d serving workers", len(before))
	return r.probeAdditionalFallback(ctx, fallback)
}

func (r *releaseNamespaceSecrets) defaultCertificate(ctx context.Context) (*x509.Certificate, error) {
	secret, err := readJSON[corev1.Secret](ctx, r.Session, "get", "secret", "default-ssl-cert")
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(secret.Data[corev1.TLSCertKey])
	if block == nil {
		return nil, errors.New("default TLS Secret contains no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func (r *releaseNamespaceSecrets) additionalRoutes(ctx context.Context, certificates []additionalCertificate) error {
	setup := namespaceManifest(foreignNamespace) + scopedBackendManifest(foreignNamespace)
	if _, err := r.KubeUnscoped(ctx, strings.NewReader(setup), "apply", "-f", "-"); err != nil {
		return err
	}
	unscoped := r.Client
	unscoped.Namespace = ""
	for _, certificate := range certificates {
		manifest := ingressManifest(foreignNamespace, certificate.name, certificate.host, "", nil)
		if err := admission.Wait(ctx, unscoped, []byte(manifest), 180*time.Second); err != nil {
			return err
		}
		if _, err := r.KubeUnscoped(ctx, strings.NewReader(manifest), "apply", "-f", "-"); err != nil {
			return err
		}
	}
	_, err := r.KubeUnscoped(ctx, nil, "rollout", "status", "deployment/"+scopedBackend, "--namespace="+foreignNamespace, "--timeout=180s")
	return err
}

func (r *releaseNamespaceSecrets) issueAdditionalCertificate(ctx context.Context, fixture additionalCertificate, serial int64) (*fixtures.Certificate, error) {
	certificate, err := fixtures.NewCertificate(&x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: fixture.host}, DNSNames: fixture.dnsNames,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true,
	}, fixtures.ECDSAP256, nil)
	if err != nil {
		return nil, err
	}
	secret := corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: fixture.name, Namespace: r.Namespace}, Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: certificate.PEM, corev1.TLSPrivateKeyKey: certificate.KeyPEM},
	}
	if fixture.annotation {
		secret.Annotations = map[string]string{"cert-manager.io/alt-names": strings.Join(fixture.dnsNames, ",")}
	}
	manifest, err := json.Marshal(&secret)
	if err != nil {
		return nil, err
	}
	if _, err := r.KubeUnscoped(ctx, bytes.NewReader(manifest), "apply", "-f", "-"); err != nil {
		return nil, err
	}
	return certificate, nil
}

func (r *releaseNamespaceSecrets) probeAdditionalFallback(ctx context.Context, certificate *x509.Certificate) error {
	for _, host := range []string{"", "unmatched.scoped.test"} {
		if err := r.probeAdditionalCertificate(ctx, host, certificate, false); err != nil {
			return err
		}
	}
	return nil
}

func (r *releaseNamespaceSecrets) probeAdditionalCertificate(ctx context.Context, host string, expected *x509.Certificate, route bool) error {
	pods, err := r.Pods(ctx, "loadbalancer")
	if err != nil {
		return err
	}
	pods = livePods(pods)
	if len(pods) == 0 {
		return errors.New("no HAProxy pods to check certificate selection")
	}
	for i := range pods {
		ports, err := podRoutePorts(&pods[i])
		if err != nil {
			return err
		}
		probe := &traffic.PodProbe{
			Client: r.Client, Target: "pod/" + pods[i].Name, Ports: ports,
			StartupTimeout: 30 * time.Second, RouteTimeout: 120 * time.Second, Interval: time.Second,
			Check: func(ctx context.Context, locals []int) error {
				return additionalCertificateCheck(ctx, locals, host, expected, route)
			},
		}
		if err := traffic.Wait(ctx, probe); err != nil {
			return fmt.Errorf("certificate for %q on %s: %w", host, pods[i].Name, err)
		}
	}
	return nil
}

func additionalCertificateCheck(ctx context.Context, ports []int, host string, expected *x509.Certificate, route bool) error {
	roots := x509.NewCertPool()
	roots.AddCert(expected)
	client := httpclient.New(&httpclient.Config{Host: loopbackHost, HTTPPort: ports[0], HTTPSPort: ports[1], TLS: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}})
	defer client.CloseIdleConnections()
	actual, err := client.PeerCertificate(ctx, host)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual.Raw, expected.Raw) {
		return fmt.Errorf("served certificate serial %s, want %s", actual.SerialNumber, expected.SerialNumber)
	}
	if !route {
		return nil
	}
	response, err := client.HTTPS(host, scopedPath).Do(ctx)
	if err != nil {
		return err
	}
	if response.Status != 200 || response.Echo == nil || !strings.HasPrefix(response.Echo.PodHostname, scopedBackend+"-") {
		return fmt.Errorf("additional TLS host %s did not reach %s: HTTP %d", host, scopedBackend, response.Status)
	}
	return nil
}

func (r *releaseNamespaceSecrets) additionalCertificateWorkers(ctx context.Context) (map[string]agentapi.HAProxyInfo, error) {
	pods, err := r.Pods(ctx, "loadbalancer")
	if err != nil {
		return nil, err
	}
	workers := map[string]agentapi.HAProxyInfo{}
	pods = livePods(pods)
	for i := range pods {
		pod := &pods[i]
		result, err := r.Kube(ctx, nil, "exec", pod.Name, "-c", "agent", "--", chartName, "agent", "state", "--output", "json", "--verify")
		if err != nil {
			return nil, err
		}
		var state agentapi.State
		if err := json.Unmarshal([]byte(result.Stdout), &state); err != nil {
			return nil, fmt.Errorf("decode agent state on %s: %w", pod.Name, err)
		}
		if !state.HAProxy.HasWorkerIdentity() {
			return nil, fmt.Errorf("HAProxy pod %s has no serving worker identity", pod.Name)
		}
		workers[string(pod.UID)] = state.HAProxy
	}
	if len(workers) == 0 {
		return nil, errors.New("no HAProxy workers to check certificate renewal")
	}
	return workers, nil
}
