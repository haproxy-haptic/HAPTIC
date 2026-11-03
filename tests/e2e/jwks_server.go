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
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"time"
)

func setupJWKSServerCertificate(ctx context.Context) error {
	if data, err := kubectlGetSecretData(ctx, ControllerNamespace, "jwks-server-tls"); err == nil {
		certPEM, _ := base64.StdEncoding.DecodeString(data["tls.crt"])
		keyPEM, _ := base64.StdEncoding.DecodeString(data["tls.key"])
		caPEM, _ := base64.StdEncoding.DecodeString(data["ca.crt"])
		if validJWKSServerCertificate(certPEM, keyPEM, caPEM) {
			return nil
		}
	}
	caKey, caDER, err := generateCA()
	if err != nil {
		return err
	}
	key, certDER, err := generateServerCert(caDER, caKey, []string{"jwks-server.haptic.svc"})
	if err != nil {
		return err
	}
	return applyJWKSServerCertificate(ctx, key, certDER, caDER)
}

func applyJWKSServerCertificate(ctx context.Context, key *rsa.PrivateKey, cert, ca []byte) error {
	encode := func(kind string, der []byte) string {
		return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
	}
	return kubectlApplyStdin(ctx, []byte(fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: jwks-server-tls
  namespace: %s
type: Opaque
data:
  tls.crt: %s
  tls.key: %s
  ca.crt: %s
`, ControllerNamespace, encode(certificatePEMType, cert), encode(rsaPrivateKeyPEMType, x509.MarshalPKCS1PrivateKey(key)), encode(certificatePEMType, ca))))
}

func validJWKSServerCertificate(certPEM, keyPEM, caPEM []byte) bool {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || time.Until(cert.NotAfter) < webhookCertReuseWindow {
		return false
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return false
	}
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "jwks-server.haptic.svc"})
	return err == nil
}
