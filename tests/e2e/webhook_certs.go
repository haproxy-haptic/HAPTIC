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
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/fixtures"
)

// webhookSecretName is the Secret the chart mounts into the controller pod.
// Naming follows the chart convention: <release>-webhook-tls. NOT -webhook-cert:
// that name belongs to chart 0.1.0, which provisioned it through cert-manager,
// and reusing it made every upgrade from that release fail on Helm ownership.
const webhookSecretName = HelmReleaseName + "-webhook-tls"

// webhookServiceName is the in-cluster Service the webhook listens on.
// Used to populate the server certificate's DNS SANs.
const webhookServiceName = HelmReleaseName + "-webhook"

// setupWebhookCerts returns the base64-encoded CA bundle the helm install
// step passes via `--set controller.webhook.caBundle=...`. Tries to reuse the
// existing webhook-cert Secret first; if found and still valid (cert
// parses + chains + is not within `webhookCertReuseWindow` of expiry),
// returns its CA bundle without touching the Secret. Otherwise generates
// a fresh self-signed CA + server cert and writes the Secret.
//
// Why reuse: cert rotation now actually propagates to the running
// controller (see commit 07876141 — the missing watcher + SingleWatcher
// generation-filter fix), but propagation takes a few seconds. The e2e
// harness used to regenerate the CA on every test run unconditionally;
// against a kept cluster (KEEP_CLUSTER=true, the default), that
// guaranteed every iteration's first test failed with
// `x509: certificate signed by unknown authority` because the test
// created its Ingress before the controller had rotated to the new cert.
// Reusing the existing Secret across runs keeps the same CA bundle on
// both ends, so admission works immediately.
//
// Mirrors what scripts/dev-env-assets/generate-webhook-certs.sh does for
// the dev environment, but in Go (crypto/x509) so the e2e suite stays
// self-contained without shelling out to openssl.
func setupWebhookCerts(ctx context.Context) (caBundleB64 string, err error) {
	if caCertPEM, ok := readReusableWebhookCABundle(ctx); ok {
		return base64.StdEncoding.EncodeToString(caCertPEM), nil
	}

	caKey, caCertDER, err := generateCA()
	if err != nil {
		return "", fmt.Errorf("generate CA: %w", err)
	}
	serverKey, serverCertDER, err := generateServerCert(caCertDER, caKey, []string{
		webhookServiceName,
		webhookServiceName + "." + ControllerNamespace,
		webhookServiceName + "." + ControllerNamespace + ".svc",
		webhookServiceName + "." + ControllerNamespace + ".svc.cluster.local",
	})
	if err != nil {
		return "", fmt.Errorf("generate server cert: %w", err)
	}

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: certificatePEMType, Bytes: caCertDER})
	serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: certificatePEMType, Bytes: serverCertDER})
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: rsaPrivateKeyPEMType, Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})

	if err := applyWebhookSecret(ctx, serverCertPEM, serverKeyPEM, caCertPEM); err != nil {
		return "", fmt.Errorf("apply webhook secret: %w", err)
	}
	return base64.StdEncoding.EncodeToString(caCertPEM), nil
}

// webhookCertReuseWindow is the minimum remaining validity an existing
// webhook cert must have for setupWebhookCerts to reuse it. Cert is
// regenerated when remaining validity drops below this; otherwise we
// reuse to avoid the rotation propagation window biting the first test
// in every kept-cluster iteration.
const webhookCertReuseWindow = 10 * time.Minute

// readReusableWebhookCABundle reads the existing webhook-cert Secret and
// returns its `ca.crt` if every required key is present, the cert
// parses, and it stays valid for at least webhookCertReuseWindow. Any
// failure (Secret missing, key missing, cert unparseable, expired/near-
// expiry) returns ok=false so the caller regenerates from scratch.
//
// Errors are intentionally swallowed: this is a fast-path optimisation
// for KEEP_CLUSTER=true iteration, not a correctness check. The
// regeneration path that follows handles every "no usable cert" case
// identically.
func readReusableWebhookCABundle(ctx context.Context) ([]byte, bool) {
	output, err := kubectlGetSecretData(ctx, ControllerNamespace, webhookSecretName)
	if err != nil {
		return nil, false
	}
	caB64 := output["ca.crt"]
	tlsB64 := output["tls.crt"]
	keyB64 := output["tls.key"]
	if caB64 == "" || tlsB64 == "" || keyB64 == "" {
		return nil, false
	}
	caCertPEM, err := base64.StdEncoding.DecodeString(caB64)
	if err != nil {
		return nil, false
	}
	tlsPEM, err := base64.StdEncoding.DecodeString(tlsB64)
	if err != nil {
		return nil, false
	}
	block, _ := pem.Decode(tlsPEM)
	if block == nil {
		return nil, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, false
	}
	if time.Until(cert.NotAfter) < webhookCertReuseWindow {
		return nil, false
	}
	return caCertPEM, true
}

// applyWebhookSecret creates the haptic namespace (if needed) and the
// webhook cert Secret. Uses kubectl apply -f - so a re-run replaces the
// Secret atomically (KEEP_CLUSTER mode).
func applyWebhookSecret(ctx context.Context, tlsCrt, tlsKey, caCrt []byte) error {
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
data:
  tls.crt: %s
  tls.key: %s
  ca.crt: %s
`,
		ControllerNamespace,
		webhookSecretName, ControllerNamespace,
		base64.StdEncoding.EncodeToString(tlsCrt),
		base64.StdEncoding.EncodeToString(tlsKey),
		base64.StdEncoding.EncodeToString(caCrt),
	)
	return kubectlApplyStdin(ctx, []byte(manifest))
}

// setupDefaultSSLCert creates a kubernetes.io/tls Secret named
// "default-ssl-cert" in the controller namespace, populated with a fresh
// self-signed certificate for *.example.com. The chart's template
// references this Secret unconditionally (it's the HAProxy default-pem),
// so the suite must create it before the controller starts rendering.
//
// Idempotent under apply — re-running rotates the cert.
func setupDefaultSSLCert(ctx context.Context) error {
	template := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "*.example.com"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"*.example.com", "example.com", defaultCertificateMTLSHost},
	}
	cert, err := fixtures.NewCertificate(template, fixtures.RSA2048, nil)
	if err != nil {
		return err
	}
	certPEM, keyPEM := cert.PEM, cert.KeyPEM

	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: default-ssl-cert
  namespace: %s
type: kubernetes.io/tls
data:
  tls.crt: %s
  tls.key: %s
`,
		ControllerNamespace,
		base64.StdEncoding.EncodeToString(certPEM),
		base64.StdEncoding.EncodeToString(keyPEM),
	)
	return kubectlApplyStdin(ctx, []byte(manifest))
}

// generateCA returns a 2048-bit RSA key + a self-signed CA certificate
// valid for 1 year. Subject CN matches what the dev shell script uses.
func generateCA() (*rsa.PrivateKey, []byte, error) {
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Webhook CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	cert, err := fixtures.NewCertificate(template, fixtures.RSA2048, nil)
	if err != nil {
		return nil, nil, err
	}
	return cert.Key.(*rsa.PrivateKey), cert.DER, nil
}

// mTLSBundle holds everything a client-mTLS test needs: a CA, a server
// cert signed by it (for the HAProxy frontend), a trusted client cert,
// and an untrusted client cert (signed by a *different* CA — used to
// verify HAProxy rejects mismatching CAs at the TLS handshake).
type mTLSBundle struct {
	CACertPEM     []byte // ca.crt — bundle the chart loads via auth-tls-secret
	ServerCertPEM []byte // tls.crt for the server-side TLS Secret
	ServerKeyPEM  []byte // tls.key for the server-side TLS Secret
	ClientCertPEM []byte // trusted client cert (signed by CACertPEM)
	ClientKeyPEM  []byte // matching key
	WrongCertPEM  []byte // client cert signed by an UNTRUSTED CA
	WrongKeyPEM   []byte // matching key for the wrong cert
}

// generateMTLSBundle is the all-in-one cert-fixture builder for tests
// that exercise client-mTLS. Returns PEM bytes ready to drop into
// kubernetes.io/tls or generic Opaque secrets.
//
// All certs are short-lived (1 year), RSA-2048. Server CN matches the
// supplied DNS host so SNI works without InsecureSkipVerify.
func generateMTLSBundle(serverHost string) (*mTLSBundle, error) {
	caKey, caCertDER, err := generateCA()
	if err != nil {
		return nil, fmt.Errorf("trusted CA: %w", err)
	}
	serverKey, serverCertDER, err := generateServerCert(caCertDER, caKey, []string{serverHost})
	if err != nil {
		return nil, fmt.Errorf("server cert: %w", err)
	}
	clientKey, clientCertDER, err := generateClientCert(caCertDER, caKey, "test-client")
	if err != nil {
		return nil, fmt.Errorf("trusted client cert: %w", err)
	}

	// Untrusted CA + client cert signed by it.
	wrongCAKey, wrongCACertDER, err := generateCA()
	if err != nil {
		return nil, fmt.Errorf("untrusted CA: %w", err)
	}
	wrongKey, wrongCertDER, err := generateClientCert(wrongCACertDER, wrongCAKey, "wrong-client")
	if err != nil {
		return nil, fmt.Errorf("untrusted client cert: %w", err)
	}

	return &mTLSBundle{
		CACertPEM:     pem.EncodeToMemory(&pem.Block{Type: certificatePEMType, Bytes: caCertDER}),
		ServerCertPEM: pem.EncodeToMemory(&pem.Block{Type: certificatePEMType, Bytes: serverCertDER}),
		ServerKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: rsaPrivateKeyPEMType, Bytes: x509.MarshalPKCS1PrivateKey(serverKey)}),
		ClientCertPEM: pem.EncodeToMemory(&pem.Block{Type: certificatePEMType, Bytes: clientCertDER}),
		ClientKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: rsaPrivateKeyPEMType, Bytes: x509.MarshalPKCS1PrivateKey(clientKey)}),
		WrongCertPEM:  pem.EncodeToMemory(&pem.Block{Type: certificatePEMType, Bytes: wrongCertDER}),
		WrongKeyPEM:   pem.EncodeToMemory(&pem.Block{Type: rsaPrivateKeyPEMType, Bytes: x509.MarshalPKCS1PrivateKey(wrongKey)}),
	}, nil
}

// generateClientCert returns a client cert (clientAuth EKU) signed by
// the supplied CA. CN drives the issued certificate's identity.
func generateClientCert(caCertDER []byte, caKey *rsa.PrivateKey, cn string) (*rsa.PrivateKey, []byte, error) {
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	cert, err := fixtures.NewCertificate(template, fixtures.RSA2048, &fixtures.Certificate{Parsed: caCert, Key: caKey})
	if err != nil {
		return nil, nil, err
	}
	return cert.Key.(*rsa.PrivateKey), cert.DER, nil
}

// generateServerCert returns a 2048-bit RSA key + a server certificate
// signed by caKey, with the supplied DNS SANs. The CN matches the first
// SAN.
func generateServerCert(caCertDER []byte, caKey *rsa.PrivateKey, dnsNames []string) (*rsa.PrivateKey, []byte, error) {
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}

	cn := webhookServiceName + "." + ControllerNamespace + ".svc"
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
	}
	cert, err := fixtures.NewCertificate(template, fixtures.RSA2048, &fixtures.Certificate{Parsed: caCert, Key: caKey})
	if err != nil {
		return nil, nil, err
	}
	return cert.Key.(*rsa.PrivateKey), cert.DER, nil
}
