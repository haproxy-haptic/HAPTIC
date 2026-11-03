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

package templating

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

const (
	publicKeyPEMType      = "PUBLIC KEY"
	publicKeyAlgorithmRSA = "RSA"
)

func scriggoPublicKeyInfo(encoded string) (map[string]any, error) {
	data := bytes.TrimSpace([]byte(encoded))
	if !bytes.HasPrefix(data, []byte("-----BEGIN ")) || bytes.Count(data, []byte("-----BEGIN ")) != 1 {
		return nil, errors.New("expected one PEM public key")
	}
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 || len(block.Headers) != 0 {
		return nil, errors.New("expected one unencrypted PEM public key without extra data")
	}
	var key any
	var err error
	switch block.Type {
	case publicKeyPEMType:
		key, err = x509.ParsePKIXPublicKey(block.Bytes)
	case "RSA PUBLIC KEY":
		key, err = x509.ParsePKCS1PublicKey(block.Bytes)
	default:
		return nil, errors.New("expected PUBLIC KEY or RSA PUBLIC KEY PEM block")
	}
	if err != nil {
		return nil, fmt.Errorf("invalid public key: %w", err)
	}
	var algorithm, curve string
	var bits int
	switch public := key.(type) {
	case *rsa.PublicKey:
		algorithm, bits = publicKeyAlgorithmRSA, public.N.BitLen()
	case *ecdsa.PublicKey:
		algorithm, bits, curve = "ECDSA", public.Curve.Params().BitSize, public.Curve.Params().Name
	case ed25519.PublicKey:
		algorithm, bits, curve = "Ed25519", len(public)*8, "Ed25519"
	default:
		return nil, errors.New("public key algorithm must be RSA, ECDSA, or Ed25519")
	}
	encodedPEM, err := encodePublicKey(key)
	if err != nil {
		return nil, err
	}
	return map[string]any{"algorithm": algorithm, "bits": bits, "curve": curve, "pem": encodedPEM}, nil
}

func encodePublicKey(key any) (string, error) {
	canonical, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", fmt.Errorf("encode public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: publicKeyPEMType, Bytes: canonical})), nil
}
