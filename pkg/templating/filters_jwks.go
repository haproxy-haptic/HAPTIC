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

package templating

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
)

const maxJWKSKeys = 64

type jsonWebKey struct {
	KeyType string   `json:"kty"`
	ID      string   `json:"kid"`
	Use     string   `json:"use"`
	Ops     []string `json:"key_ops"`
	Alg     string   `json:"alg"`
	N       string   `json:"n"`
	E       string   `json:"e"`
	Curve   string   `json:"crv"`
	X       string   `json:"x"`
	Y       string   `json:"y"`
	D       string   `json:"d"`
}

func scriggoJWKSPublicKeys(encoded, algorithm string) (map[string]string, error) {
	keyType, curve, err := jwksAlgorithm(algorithm)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 1<<20 {
		return nil, errors.New("JWKS exceeds 1 MiB")
	}
	var set struct {
		Keys []*jsonWebKey `json:"keys"`
	}
	if err := json.Unmarshal([]byte(encoded), &set); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	if set.Keys == nil || len(set.Keys) > maxJWKSKeys {
		return nil, fmt.Errorf("JWKS must contain a keys array with at most %d keys", maxJWKSKeys)
	}
	keys := make(map[string]string)
	for _, key := range set.Keys {
		if key == nil {
			return nil, errors.New("JWKS entries must be key objects")
		}
		if !key.verifies(algorithm, keyType, curve) {
			continue
		}
		if key.ID == "" || len(key.ID) > 256 {
			return nil, errors.New("JWKS signing key kid must contain 1 to 256 bytes")
		}
		if _, exists := keys[key.ID]; exists {
			return nil, fmt.Errorf("duplicate JWKS signing key kid %q", key.ID)
		}
		encoded, err := key.encodedPublicKey()
		if err != nil {
			return nil, fmt.Errorf("JWKS key %q: %w", key.ID, err)
		}
		keys[key.ID] = encoded
	}
	return keys, nil
}

func jwksAlgorithm(algorithm string) (keyType, curve string, err error) {
	switch algorithm {
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512":
		return publicKeyAlgorithmRSA, "", nil
	case "ES256":
		return "EC", "P-256", nil
	case "ES384":
		return "EC", "P-384", nil
	case "ES512":
		return "EC", "P-521", nil
	default:
		return "", "", fmt.Errorf("unsupported JWKS verification algorithm %q", algorithm)
	}
}

func (key *jsonWebKey) verifies(algorithm, keyType, curve string) bool {
	return key.KeyType == keyType && (curve == "" || key.Curve == curve) &&
		(key.Alg == "" || key.Alg == algorithm) && (key.Use == "" || key.Use == "sig") &&
		(key.Ops == nil || slices.Contains(key.Ops, "verify"))
}

func (key *jsonWebKey) publicKey() (any, error) {
	if key.KeyType == publicKeyAlgorithmRSA {
		return key.rsaPublicKey()
	}
	return key.ecPublicKey()
}

func (key *jsonWebKey) rsaPublicKey() (*rsa.PublicKey, error) {
	n, err := jwksInteger(key.N)
	if err != nil {
		return nil, fmt.Errorf("invalid RSA modulus: %w", err)
	}
	e, err := jwksInteger(key.E)
	if err != nil {
		return nil, fmt.Errorf("invalid RSA exponent: %w", err)
	}
	if n.BitLen() < 2048 || n.BitLen() > 8192 || n.Bit(0) == 0 {
		return nil, errors.New("RSA modulus must be odd and contain 2048 to 8192 bits")
	}
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() >= 1<<31 || e.Bit(0) == 0 {
		return nil, errors.New("RSA exponent must be an odd integer from 3 to 2147483647")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

func (key *jsonWebKey) ecPublicKey() (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch key.Curve {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, errors.New("unsupported EC curve")
	}
	x, err := jwksCoordinate(key.X, curve)
	if err != nil {
		return nil, fmt.Errorf("invalid EC x coordinate: %w", err)
	}
	y, err := jwksCoordinate(key.Y, curve)
	if err != nil {
		return nil, fmt.Errorf("invalid EC y coordinate: %w", err)
	}
	encoded := append([]byte{4}, x...)
	encoded = append(encoded, y...)
	return ecdsa.ParseUncompressedPublicKey(curve, encoded)
}

func jwksInteger(encoded string) (*big.Int, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) == 0 || len(decoded) > 1024 || decoded[0] == 0 {
		return nil, errors.New("expected an unsigned base64url integer without leading zeros")
	}
	return new(big.Int).SetBytes(decoded), nil
}

func jwksCoordinate(encoded string, curve elliptic.Curve) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) != (curve.Params().BitSize+7)/8 {
		return nil, errors.New("expected a base64url coordinate of the curve's byte width")
	}
	return decoded, nil
}

func (key *jsonWebKey) encodedPublicKey() (string, error) {
	if key.D != "" {
		return "", errors.New("JWKS must contain public keys only")
	}
	public, err := key.publicKey()
	if err != nil {
		return "", err
	}
	return encodePublicKey(public)
}
