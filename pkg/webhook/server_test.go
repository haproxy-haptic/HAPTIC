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

package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// generateTestCertificates generates a self-signed certificate for testing.
func generateTestCertificates() (certPEM, keyPEM []byte, err error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generating private key: %w", err)
	}

	// Create certificate template
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
	}

	// Create self-signed certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("creating certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})

	return certPEM, keyPEM, nil
}

// newTestServer builds a Server with freshly generated TLS material; cfg carries
// whatever else the test varies.
func newTestServer(t *testing.T, cfg *ServerConfig) *Server {
	t.Helper()
	certPEM, keyPEM, err := generateTestCertificates()
	require.NoError(t, err)
	cfg.CertPEM, cfg.KeyPEM = certPEM, keyPEM
	server, err := NewServer(cfg)
	require.NoError(t, err)
	return server
}

// configMapAdmissionRequest posts a Create AdmissionReview for a v1.ConfigMap
// at /validate — the shape most handler tests exercise.
func configMapAdmissionRequest(t *testing.T) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	reviewBytes, err := json.Marshal(&admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test","namespace":"default"}}`),
			},
		},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(reviewBytes))
	req.Header.Set("Content-Type", "application/json")
	return httptest.NewRecorder(), req
}

func TestNewServer_Defaults(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	require.NotNil(t, server)
	assert.Equal(t, 9443, server.config.Port)
	assert.Equal(t, "0.0.0.0", server.config.BindAddress)
	assert.Equal(t, "/validate", server.config.Path)
	assert.Equal(t, 10*time.Second, server.config.ReadTimeout)
	assert.Equal(t, 10*time.Second, server.config.WriteTimeout)
	assert.Equal(t, 120*time.Second, server.config.IdleTimeout)
	assert.NotNil(t, server.validators)
}

// A zero IdleTimeout silently inherits ReadTimeout, which closed pooled API
// server connections after 10s and surfaced as `failed calling webhook: EOF` (#133).
func TestNewServer_IdleTimeoutOutlivesTheAPIServersIdleConnections(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	// 90s is the client-go transport default; anything at or below it lets the
	// gate close first.
	assert.Greater(t, server.config.IdleTimeout, 90*time.Second,
		"idle timeout must exceed the API server's idle-connection timeout")
	assert.Greater(t, server.config.IdleTimeout, server.config.ReadTimeout,
		"a zero IdleTimeout silently inherits ReadTimeout — that is the bug")
}

func TestNewServer_CustomConfig(t *testing.T) {
	certPEM, keyPEM, err := generateTestCertificates()
	require.NoError(t, err)

	server, err := NewServer(&ServerConfig{
		Port:         8443,
		BindAddress:  "127.0.0.1",
		Path:         "/webhook",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 15 * time.Second,
		CertPEM:      certPEM,
		KeyPEM:       keyPEM,
	})
	require.NoError(t, err)

	require.NotNil(t, server)
	assert.Equal(t, 8443, server.config.Port)
	assert.Equal(t, "127.0.0.1", server.config.BindAddress)
	assert.Equal(t, "/webhook", server.config.Path)
	assert.Equal(t, 5*time.Second, server.config.ReadTimeout)
	assert.Equal(t, 15*time.Second, server.config.WriteTimeout)
}

func TestServer_RegisterValidator(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	server.RegisterValidator("networking.k8s.io/v1.Ingress", func(_ *ValidationContext) (bool, string, []string, error) {
		return true, "", nil, nil
	})

	// Verify validator is registered
	server.mu.RLock()
	_, exists := server.validators["networking.k8s.io/v1.Ingress"]
	server.mu.RUnlock()

	assert.True(t, exists)

	// Replace validator
	server.RegisterValidator("networking.k8s.io/v1.Ingress", func(_ *ValidationContext) (bool, string, []string, error) {
		return false, "replaced", nil, nil
	})

	// Verify it was replaced
	server.mu.RLock()
	newValidator := server.validators["networking.k8s.io/v1.Ingress"]
	server.mu.RUnlock()

	allowed, reason, _, err := newValidator(nil)
	assert.NoError(t, err)
	assert.False(t, allowed)
	assert.Equal(t, "replaced", reason)
}

func TestServer_GetGVK(t *testing.T) {
	tests := []struct {
		name     string
		request  *admissionv1.AdmissionRequest
		expected string
	}{
		{
			name: "core type",
			request: &admissionv1.AdmissionRequest{
				Kind: metav1.GroupVersionKind{
					Group:   "",
					Version: "v1",
					Kind:    "Pod",
				},
			},
			expected: "v1.Pod",
		},
		{
			name: "namespaced group",
			request: &admissionv1.AdmissionRequest{
				Kind: metav1.GroupVersionKind{
					Group:   "networking.k8s.io",
					Version: "v1",
					Kind:    "Ingress",
				},
			},
			expected: "networking.k8s.io/v1.Ingress",
		},
		{
			name: "apps group",
			request: &admissionv1.AdmissionRequest{
				Kind: metav1.GroupVersionKind{
					Group:   "apps",
					Version: "v1",
					Kind:    "Deployment",
				},
			},
			expected: "apps/v1.Deployment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getGVK(tt.request)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestServer_HandleHealthz(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	// Create test request
	req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	w := httptest.NewRecorder()

	// Call handler
	server.handleHealthz(w, req)

	// Verify response
	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body := w.Body.String()
	assert.Equal(t, "ok", body)
}

func TestServer_HandleValidation_MethodNotAllowed(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	// Test GET request (should be rejected)
	req := httptest.NewRequest(http.MethodGet, "/validate", http.NoBody)
	w := httptest.NewRecorder()

	server.handleValidation(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestServer_HandleValidation_InvalidBody(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	// Test with invalid JSON body
	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader([]byte("invalid json")))
	w := httptest.NewRecorder()

	server.handleValidation(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestServer_HandleValidation_NoValidatorDenies(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	// Create a valid AdmissionReview request for a type with no validator
	w, req := configMapAdmissionRequest(t)

	server.handleValidation(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var responseReview admissionv1.AdmissionReview
	err := json.NewDecoder(resp.Body).Decode(&responseReview)
	require.NoError(t, err)

	assert.False(t, responseReview.Response.Allowed)
	assert.Equal(t, int32(http.StatusServiceUnavailable), responseReview.Response.Result.Code)
	assert.Contains(t, responseReview.Response.Result.Message, "no validator registered")
}

func TestServer_HandleValidation_ValidatorAllows(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	// Register validator that allows
	server.RegisterValidator("v1.ConfigMap", func(ctx *ValidationContext) (bool, string, []string, error) {
		assert.Equal(t, "test", ctx.Name)
		assert.Equal(t, "default", ctx.Namespace)
		assert.Equal(t, "CREATE", ctx.Operation)
		return true, "", nil, nil
	})

	// Create a valid AdmissionReview request
	w, req := configMapAdmissionRequest(t)

	server.handleValidation(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var responseReview admissionv1.AdmissionReview
	err := json.NewDecoder(resp.Body).Decode(&responseReview)
	require.NoError(t, err)

	assert.True(t, responseReview.Response.Allowed)
	assert.Equal(t, "test-uid", string(responseReview.Response.UID))
}

func TestServer_HandleValidation_ValidatorDenies(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	// Register validator that denies
	server.RegisterValidator("v1.ConfigMap", func(_ *ValidationContext) (bool, string, []string, error) {
		return false, "validation failed: missing required field", nil, nil
	})

	// Create a valid AdmissionReview request
	w, req := configMapAdmissionRequest(t)

	server.handleValidation(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var responseReview admissionv1.AdmissionReview
	err := json.NewDecoder(resp.Body).Decode(&responseReview)
	require.NoError(t, err)

	assert.False(t, responseReview.Response.Allowed)
	assert.Contains(t, responseReview.Response.Result.Message, "validation failed: missing required field")
	assert.Equal(t, int32(http.StatusForbidden), responseReview.Response.Result.Code)
}

func TestServer_HandleValidation_ValidatorUnavailable(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})
	server.RegisterValidator("v1.ConfigMap", func(_ *ValidationContext) (bool, string, []string, error) {
		return false, "", []string{"warning"}, apierrors.NewServiceUnavailable("validation unavailable")
	})
	w, req := configMapAdmissionRequest(t)
	server.handleValidation(w, req)
	var review admissionv1.AdmissionReview
	require.NoError(t, json.NewDecoder(w.Body).Decode(&review))
	require.False(t, review.Response.Allowed)
	assert.Equal(t, int32(http.StatusServiceUnavailable), review.Response.Result.Code)
	assert.Equal(t, metav1.StatusReasonServiceUnavailable, review.Response.Result.Reason)
	assert.Equal(t, []string{"warning"}, review.Response.Warnings)
}

func TestServer_HandleValidation_ValidatorError(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	// Register validator that returns an error
	server.RegisterValidator("v1.ConfigMap", func(_ *ValidationContext) (bool, string, []string, error) {
		return false, "", nil, fmt.Errorf("internal validation error")
	})

	// Create a valid AdmissionReview request
	w, req := configMapAdmissionRequest(t)

	server.handleValidation(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var responseReview admissionv1.AdmissionReview
	err := json.NewDecoder(resp.Body).Decode(&responseReview)
	require.NoError(t, err)

	assert.False(t, responseReview.Response.Allowed)
	assert.Contains(t, responseReview.Response.Result.Message, "internal validation error")
	assert.Equal(t, int32(http.StatusInternalServerError), responseReview.Response.Result.Code)
}

func TestServer_HandleValidation_InvalidObject(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	server.RegisterValidator("v1.ConfigMap", func(_ *ValidationContext) (bool, string, []string, error) {
		return true, "", nil, nil
	})

	// Create a request with invalid object JSON directly
	// We must build the JSON manually because json.Marshal can't handle invalid Raw
	reviewJSON := `{
		"apiVersion": "admission.k8s.io/v1",
		"kind": "AdmissionReview",
		"request": {
			"uid": "test-uid",
			"kind": {
				"group": "",
				"version": "v1",
				"kind": "ConfigMap"
			},
			"operation": "CREATE",
			"object": "not-valid-object-structure"
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader([]byte(reviewJSON)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleValidation(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var responseReview admissionv1.AdmissionReview
	err := json.NewDecoder(resp.Body).Decode(&responseReview)
	require.NoError(t, err)

	assert.False(t, responseReview.Response.Allowed)
	assert.Contains(t, responseReview.Response.Result.Message, "parsing object")
}

func TestServer_HandleValidation_UpdateWithOldObject(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	// Register validator that checks old object
	server.RegisterValidator("v1.ConfigMap", func(ctx *ValidationContext) (bool, string, []string, error) {
		assert.Equal(t, "UPDATE", ctx.Operation)
		assert.NotNil(t, ctx.Object)
		assert.NotNil(t, ctx.OldObject)

		// Verify old object metadata
		oldName := ctx.OldObject.GetName()
		assert.Equal(t, "test", oldName)

		return true, "", nil, nil
	})

	// Create an UPDATE request with old object
	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID: "test-uid",
			Kind: metav1.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "ConfigMap",
			},
			Operation: admissionv1.Update,
			Object: runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test","namespace":"default"},"data":{"new":"value"}}`),
			},
			OldObject: runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test","namespace":"default"},"data":{"old":"value"}}`),
			},
		},
	}

	reviewBytes, err := json.Marshal(review)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(reviewBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleValidation(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var responseReview admissionv1.AdmissionReview
	err = json.NewDecoder(resp.Body).Decode(&responseReview)
	require.NoError(t, err)

	assert.True(t, responseReview.Response.Allowed)
}

func TestServer_ValidateDeleteWithOnlyOldObject(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})
	server.RegisterValidator("v1.ConfigMap", func(ctx *ValidationContext) (bool, string, []string, error) {
		assert.Nil(t, ctx.Object)
		require.NotNil(t, ctx.OldObject)
		assert.Equal(t, "default", ctx.Namespace)
		assert.Equal(t, "test", ctx.Name)
		return true, "", nil, nil
	})

	response := server.validateRequest(t.Context(), &admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		Operation: admissionv1.Delete,
		OldObject: runtime.RawExtension{
			Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test","namespace":"default"}}`),
		},
	}, false)

	assert.True(t, response.Allowed)
}

func TestServer_HandleValidation_InvalidOldObject(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	server.RegisterValidator("v1.ConfigMap", func(_ *ValidationContext) (bool, string, []string, error) {
		return true, "", nil, nil
	})

	// Construct JSON manually to test invalid OldObject handling
	// (json.Marshal fails on RawExtension with unrecognized content)
	reviewJSON := `{
		"apiVersion": "admission.k8s.io/v1",
		"kind": "AdmissionReview",
		"request": {
			"uid": "test-uid",
			"kind": {"group": "", "version": "v1", "kind": "ConfigMap"},
			"operation": "UPDATE",
			"object": {"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test","namespace":"default"}},
			"oldObject": "not-valid-object-structure"
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader([]byte(reviewJSON)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleValidation(w, req)

	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var responseReview admissionv1.AdmissionReview
	err := json.NewDecoder(resp.Body).Decode(&responseReview)
	require.NoError(t, err)

	assert.False(t, responseReview.Response.Allowed)
	assert.Contains(t, responseReview.Response.Result.Message, "parsing old object")
}

func TestServer_NewServer_InvalidCertificate(t *testing.T) {
	server, err := NewServer(&ServerConfig{
		CertPEM: []byte("invalid cert"),
		KeyPEM:  []byte("invalid key"),
		Port:    0, // ephemeral: a fixed port collides with whatever already holds it
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "loading initial TLS certificate")
	assert.Nil(t, server)
}

func TestServer_Start_ContextCancellation(t *testing.T) {
	certPEM, keyPEM, err := generateTestCertificates()
	require.NoError(t, err)

	server, err := NewServer(&ServerConfig{
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
		Port:    0, // ephemeral: see above
		// Loopback, not the 0.0.0.0 default: binding all interfaces makes
		// Windows Firewall prompt to whitelist every freshly built test
		// binary. Tests must only ever listen on loopback.
		BindAddress: "127.0.0.1",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- server.Start(ctx)
	}()

	// Wait until the server is actually listening, rather than a fixed sleep.
	// Select on done too: if Start fails to bind, nothing ever closes
	// Listening(), and waiting on it alone hangs until the test binary's
	// timeout with the real error sitting unread in done.
	select {
	case <-server.Listening():
	case err := <-done:
		t.Fatalf("server exited before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server neither started listening nor exited")
	}

	// Cancel context to trigger shutdown
	cancel()

	// Wait for server to stop
	select {
	case err := <-done:
		// Should return nil on graceful shutdown
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Server did not shut down within timeout")
	}
}

func TestServer_Start_Integration(t *testing.T) {
	certPEM, keyPEM, err := generateTestCertificates()
	require.NoError(t, err)

	server, err := NewServer(&ServerConfig{
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
		Port:        0, // ephemeral: see above
		BindAddress: "127.0.0.1",
	})
	require.NoError(t, err)

	// Register a test validator
	server.RegisterValidator("v1.ConfigMap", func(_ *ValidationContext) (bool, string, []string, error) {
		return true, "", nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- server.Start(ctx)
	}()

	// Wait until the server is actually listening, rather than a fixed sleep.
	// Select on done too: if Start fails to bind, nothing ever closes
	// Listening(), and waiting on it alone hangs until the test binary's
	// timeout with the real error sitting unread in done.
	select {
	case <-server.Listening():
	case err := <-done:
		t.Fatalf("server exited before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server neither started listening nor exited")
	}

	// Create TLS client that trusts our self-signed cert
	certPool := x509.NewCertPool()
	certPool.AppendCertsFromPEM(certPEM)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: certPool,
			},
		},
	}

	// Test healthz endpoint
	healthResp, err := client.Get("https://" + server.Addr() + "/healthz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, healthResp.StatusCode)
	healthResp.Body.Close()

	// Test validation endpoint
	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID: "test-uid",
			Kind: metav1.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "ConfigMap",
			},
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test","namespace":"default"}}`),
			},
		},
	}

	reviewBytes, err := json.Marshal(review)
	require.NoError(t, err)

	validateResp, err := client.Post("https://"+server.Addr()+"/validate", "application/json", bytes.NewReader(reviewBytes))
	require.NoError(t, err)
	defer validateResp.Body.Close()

	assert.Equal(t, http.StatusOK, validateResp.StatusCode)

	var responseReview admissionv1.AdmissionReview
	err = json.NewDecoder(validateResp.Body).Decode(&responseReview)
	require.NoError(t, err)

	assert.True(t, responseReview.Response.Allowed)

	// Shut down server
	cancel()

	select {
	case <-done:
		// Server shut down successfully
	// Generous because this waits on a real HTTP server's graceful shutdown,
	// and the coverage build runs every statement through instrumentation: 5s
	// was enough for the plain build and not for that one, which failed the
	// coverage job on a server that had shut down perfectly well.
	case <-time.After(30 * time.Second):
		t.Fatal("Server did not shut down within timeout")
	}
}

func TestServer_ExtractMetadata(t *testing.T) {
	tests := []struct {
		name              string
		objectJSON        string
		expectedNamespace string
		expectedName      string
	}{
		{
			name:              "namespaced resource",
			objectJSON:        `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"my-config","namespace":"production"}}`,
			expectedNamespace: "production",
			expectedName:      "my-config",
		},
		{
			name:              "cluster-scoped resource",
			objectJSON:        `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"my-namespace"}}`,
			expectedNamespace: "",
			expectedName:      "my-namespace",
		},
		{
			name:              "empty metadata",
			objectJSON:        `{"apiVersion":"v1","kind":"ConfigMap"}`,
			expectedNamespace: "",
			expectedName:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse object as unstructured (same as server does)
			var obj map[string]any
			err := json.Unmarshal([]byte(tt.objectJSON), &obj)
			require.NoError(t, err)

			// Create unstructured object
			unstructuredObj := &unstructured.Unstructured{Object: obj}

			namespace, name := extractMetadata(unstructuredObj)
			assert.Equal(t, tt.expectedNamespace, namespace)
			assert.Equal(t, tt.expectedName, name)
		})
	}
}

func TestServer_ExtractMetadata_NilObject(t *testing.T) {
	namespace, name := extractMetadata(nil)
	assert.Empty(t, namespace)
	assert.Empty(t, name)
}

func TestServer_ConcurrentValidation(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})

	// Register validator
	validationCount := 0
	var mu sync.Mutex
	server.RegisterValidator("v1.ConfigMap", func(_ *ValidationContext) (bool, string, []string, error) {
		mu.Lock()
		validationCount++
		mu.Unlock()
		return true, "", nil, nil
	})

	// Create test request body once
	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID: "test-uid",
			Kind: metav1.GroupVersionKind{
				Group:   "",
				Version: "v1",
				Kind:    "ConfigMap",
			},
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test","namespace":"default"}}`),
			},
		},
	}

	reviewBytes, err := json.Marshal(review)
	require.NoError(t, err)

	// Run concurrent validations
	const numRequests = 10
	var wg sync.WaitGroup
	wg.Add(numRequests)

	for range numRequests {
		go func() {
			defer wg.Done()

			req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(reviewBytes))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			server.handleValidation(w, req)

			resp := w.Result()
			assert.Equal(t, http.StatusOK, resp.StatusCode)
		}()
	}

	wg.Wait()

	// Verify all validations completed
	mu.Lock()
	assert.Equal(t, numRequests, validationCount)
	mu.Unlock()
}

// TestServer_HandleValidation_UnregisteredGVKIsReported verifies that a routed
// object without a validator is denied and reported.
func TestServer_HandleValidation_UnregisteredGVKIsReported(t *testing.T) {
	certPEM, keyPEM, err := generateTestCertificates()
	require.NoError(t, err)

	var reported []string
	server, err := NewServer(&ServerConfig{
		CertPEM:           certPEM,
		KeyPEM:            keyPEM,
		OnUnregisteredGVK: func(gvk string) { reported = append(reported, gvk) },
	})
	require.NoError(t, err)

	server.RegisterValidator("networking.k8s.io/v1.Ingress",
		func(*ValidationContext) (bool, string, []string, error) { return true, "", nil, nil })

	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test","namespace":"default"}}`),
			},
		},
	}
	reviewBytes, err := json.Marshal(review)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(reviewBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleValidation(w, req)

	var responseReview admissionv1.AdmissionReview
	require.NoError(t, json.NewDecoder(w.Result().Body).Decode(&responseReview))

	assert.False(t, responseReview.Response.Allowed)
	assert.Equal(t, int32(http.StatusServiceUnavailable), responseReview.Response.Result.Code)
	assert.Equal(t, []string{"v1.ConfigMap"}, reported,
		"the denied admission must be reported with the GVK that had no validator")
}

// TestServer_HandleValidation_RegisteredGVKNotReported is the negative control:
// a kind that IS validated must never be reported as unregistered.
func TestServer_HandleValidation_RegisteredGVKNotReported(t *testing.T) {
	certPEM, keyPEM, err := generateTestCertificates()
	require.NoError(t, err)

	var reported []string
	server, err := NewServer(&ServerConfig{
		CertPEM:           certPEM,
		KeyPEM:            keyPEM,
		OnUnregisteredGVK: func(gvk string) { reported = append(reported, gvk) },
	})
	require.NoError(t, err)

	server.RegisterValidator("v1.ConfigMap",
		func(*ValidationContext) (bool, string, []string, error) { return true, "", nil, nil })

	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test","namespace":"default"}}`),
			},
		},
	}
	reviewBytes, err := json.Marshal(review)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(reviewBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.handleValidation(w, req)

	assert.Empty(t, reported, "a validated kind is not unregistered")
}

// TestServer_SetValidators_ReplacesTable pins the property RegisterValidator
// cannot provide: a kind dropped from the new table stops being validated.
// A caller that rebuilds its wiring (a controller re-reading its config) would
// otherwise keep serving the old closure, which holds state it has discarded.
func TestServer_SetValidators_ReplacesTable(t *testing.T) {
	certPEM, keyPEM, err := generateTestCertificates()
	require.NoError(t, err)

	var unregistered []string
	server, err := NewServer(&ServerConfig{
		CertPEM:           certPEM,
		KeyPEM:            keyPEM,
		OnUnregisteredGVK: func(gvk string) { unregistered = append(unregistered, gvk) },
	})
	require.NoError(t, err)

	var called []string
	record := func(name string) ValidationFunc {
		return func(*ValidationContext) (bool, string, []string, error) {
			called = append(called, name)
			return true, "", nil, nil
		}
	}

	// First wiring validates ConfigMaps.
	server.SetValidators(map[string]ValidationFunc{"v1.ConfigMap": record("first")})
	sendConfigMapReview(t, server)
	assert.Equal(t, []string{"first"}, called)
	assert.Empty(t, unregistered)

	// Rewiring to a table WITHOUT ConfigMap must drop it, not merge.
	server.SetValidators(map[string]ValidationFunc{"networking.k8s.io/v1.Ingress": record("second")})
	sendConfigMapReview(t, server)
	assert.Equal(t, []string{"first"}, called,
		"the dropped kind's old closure must not be called after the swap")
	assert.Equal(t, []string{"v1.ConfigMap"}, unregistered,
		"the dropped kind is denied and reported")
}

// TestServer_SetValidators_CopiesInput pins that the server does not alias the
// caller's map — a caller that keeps building into its own map after the swap
// must not mutate what the server is serving.
func TestServer_SetValidators_CopiesInput(t *testing.T) {
	certPEM, keyPEM, err := generateTestCertificates()
	require.NoError(t, err)

	var unregistered []string
	server, err := NewServer(&ServerConfig{
		CertPEM:           certPEM,
		KeyPEM:            keyPEM,
		OnUnregisteredGVK: func(gvk string) { unregistered = append(unregistered, gvk) },
	})
	require.NoError(t, err)

	caller := map[string]ValidationFunc{
		"v1.ConfigMap": func(*ValidationContext) (bool, string, []string, error) { return true, "", nil, nil },
	}
	server.SetValidators(caller)

	delete(caller, "v1.ConfigMap")
	caller["v1.Secret"] = func(*ValidationContext) (bool, string, []string, error) { return true, "", nil, nil }

	sendConfigMapReview(t, server)
	assert.Empty(t, unregistered, "the server's table must be unaffected by later caller mutations")
}

func TestServer_ReplaceValidatorGenerationRetiresAfterInflightRequest(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})
	oldStarted := make(chan struct{})
	oldRelease := make(chan struct{})
	oldDone := make(chan struct{})
	oldRetired := make(chan struct{})

	require.NoError(t, server.ReplaceValidatorGeneration(map[string]ValidationFunc{
		"v1.ConfigMap": func(*ValidationContext) (bool, string, []string, error) {
			close(oldStarted)
			<-oldRelease
			return false, "old", nil, nil
		},
	}, nil, func() { close(oldRetired) }))
	server.mu.RLock()
	oldGeneration := server.generation
	server.mu.RUnlock()

	request := &admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		Operation: admissionv1.Create,
		Object: runtime.RawExtension{
			Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test"}}`),
		},
	}
	go func() {
		defer close(oldDone)
		server.validateRequest(t.Context(), request, false)
	}()
	<-oldStarted

	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- server.ReplaceValidatorGeneration(map[string]ValidationFunc{
			"v1.ConfigMap": func(*ValidationContext) (bool, string, []string, error) {
				return false, "new", nil, nil
			},
		}, nil, nil)
	}()
	require.Eventually(t, func() bool {
		server.mu.RLock()
		defer server.mu.RUnlock()
		return server.generation != oldGeneration
	}, time.Second, time.Millisecond)

	response := server.validateRequest(t.Context(), request, false)
	require.False(t, response.Allowed)
	assert.Contains(t, response.Result.Message, "new")
	select {
	case <-oldRetired:
		t.Fatal("old generation retired while its validator was still running")
	default:
	}
	select {
	case err := <-replaceDone:
		t.Fatalf("replacement returned before the old generation drained: %v", err)
	default:
	}

	close(oldRelease)
	<-oldDone
	<-oldRetired
	require.NoError(t, <-replaceDone)
}

// sendConfigMapReview drives one ConfigMap AdmissionReview through the handler.
func sendConfigMapReview(t *testing.T, server *Server) {
	t.Helper()
	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test","namespace":"default"}}`),
			},
		},
	}
	body, err := json.Marshal(review)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	server.handleValidation(httptest.NewRecorder(), req)
}

// An iteration retires the table it installed; once its successor installed
// the next one, the old iteration's stop must leave that one serving.
func TestServer_RetireValidatorGenerationIfCurrentLeavesASuccessorServing(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})
	validator := func(*ValidationContext) (bool, string, []string, error) { return true, "", nil, nil }
	oldRetired := make(chan struct{})
	old, err := server.InstallValidatorGeneration(
		map[string]ValidationFunc{"v1.ConfigMap": validator}, nil, func() { close(oldRetired) })
	require.NoError(t, err)

	newRetired := make(chan struct{})
	next, err := server.InstallValidatorGeneration(
		map[string]ValidationFunc{"v1.ConfigMap": validator, "v1.Secret": validator}, nil, func() { close(newRetired) })
	require.NoError(t, err)
	<-oldRetired

	require.NoError(t, server.RetireValidatorGenerationIfCurrent(old))
	server.mu.RLock()
	installed := server.generation
	server.mu.RUnlock()
	assert.Same(t, next, installed, "the successor's table must survive the predecessor's stop")
	select {
	case <-newRetired:
		t.Fatal("the successor's table was retired")
	default:
	}

	require.NoError(t, server.RetireValidatorGenerationIfCurrent(next))
	<-newRetired
	server.mu.RLock()
	assert.Empty(t, server.validators)
	server.mu.RUnlock()
}

// Controller readiness routes admission only to a server whose table an
// iteration installed; the placeholder before and after one denies everything.
func TestServer_ValidatingTracksTheInstalledGeneration(t *testing.T) {
	server := newTestServer(t, &ServerConfig{})
	assert.False(t, server.Validating(), "a new server has no installed table")

	generation, err := server.InstallValidatorGeneration(map[string]ValidationFunc{}, nil, nil)
	require.NoError(t, err)
	assert.True(t, server.Validating(), "an installed table counts even when it registers no kind")

	require.NoError(t, server.RetireValidatorGenerationIfCurrent(generation))
	assert.False(t, server.Validating(), "retiring the installed table falls back to the placeholder")

	_, err = server.InstallValidatorGeneration(map[string]ValidationFunc{}, nil, nil)
	require.NoError(t, err)
	server.retireValidatorGeneration()
	assert.False(t, server.Validating(), "a closed server validates nothing")
}
