// Package config provides configuration loading and validation.
package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
)

// LoadCredentials parses Secret data into a Credentials struct.
// This is a pure function that extracts credentials from Secret data.
// It does not load from Kubernetes or perform validation.
//
// Expected Secret keys: dataplane_username, dataplane_password.
func LoadCredentials(secretData map[string][]byte) (*Credentials, error) {
	if secretData == nil {
		return nil, errors.New("secret data is nil")
	}

	// Extract required fields
	dataplaneUsername, ok := secretData["dataplane_username"]
	if !ok || len(dataplaneUsername) == 0 {
		return nil, errors.New("missing required secret key: dataplane_username")
	}

	dataplanePassword, ok := secretData["dataplane_password"]
	if !ok || len(dataplanePassword) == 0 {
		return nil, errors.New("missing required secret key: dataplane_password")
	}

	return &Credentials{
		DataplaneUsername: string(dataplaneUsername),
		DataplanePassword: string(dataplanePassword),
	}, nil
}

// SecretFileSealKey derives the key of secret-file seals from the dataplane
// password: the one per-installation secret every replica already shares.
func (c *Credentials) SecretFileSealKey() []byte {
	mac := hmac.New(sha256.New, []byte(c.DataplanePassword))
	_, _ = mac.Write([]byte("haptic secret file seal"))
	return mac.Sum(nil)
}
