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
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/webhook"
)

// generateTestCertPEM creates a valid self-signed cert + key pair for tests
// that construct webhook.Server (NewServer eagerly parses the PEM, so the
// previous placeholder []byte("test-cert") no longer works for tests that
// actually start a server).
func generateTestCertPEM(t *testing.T) (cert, key []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"unit-test"}},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	require.NoError(t, err)
	cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	key = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	return
}

func TestComponent_New(t *testing.T) {
	t.Run("applies default port", func(t *testing.T) {
		config := &Config{
			CertPEM: []byte("test-cert"),
			KeyPEM:  []byte("test-key"),
		}

		component := New(testutil.NewTestLogger(), config, nil, nil)

		assert.Equal(t, DefaultWebhookPort, component.config.Port)
	})

	t.Run("applies default path", func(t *testing.T) {
		config := &Config{
			CertPEM: []byte("test-cert"),
			KeyPEM:  []byte("test-key"),
		}

		component := New(testutil.NewTestLogger(), config, nil, nil)

		assert.Equal(t, DefaultWebhookPath, component.config.Path)
	})

	t.Run("preserves custom port", func(t *testing.T) {
		config := &Config{
			Port:    8443,
			CertPEM: []byte("test-cert"),
			KeyPEM:  []byte("test-key"),
		}

		component := New(testutil.NewTestLogger(), config, nil, nil)

		assert.Equal(t, 8443, component.config.Port)
	})

	t.Run("preserves custom path", func(t *testing.T) {
		config := &Config{
			Path:    "/custom-validate",
			CertPEM: []byte("test-cert"),
			KeyPEM:  []byte("test-key"),
		}

		component := New(testutil.NewTestLogger(), config, nil, nil)

		assert.Equal(t, "/custom-validate", component.config.Path)
	})

	t.Run("applies split admission timeout defaults", func(t *testing.T) {
		component := New(testutil.NewTestLogger(), &Config{}, nil, nil)

		assert.Equal(t, DefaultResourceAdmissionTimeout, component.config.ResourceAdmissionTimeout)
	})

	t.Run("preserves custom admission timeouts", func(t *testing.T) {
		component := New(testutil.NewTestLogger(), &Config{
			ResourceAdmissionTimeout: 33 * time.Second,
		}, nil, nil)

		assert.Equal(t, 33*time.Second, component.config.ResourceAdmissionTimeout)
		assert.Equal(t, 35*time.Second, component.serverWriteTimeout())
	})
}

func TestComponent_buildGVK(t *testing.T) {
	component := &Component{}

	tests := []struct {
		name     string
		apiGroup string
		version  string
		kind     string
		expected string
	}{
		{
			name:     "core API group",
			apiGroup: "",
			version:  "v1",
			kind:     "ConfigMap",
			expected: "v1.ConfigMap",
		},
		{
			name:     "networking API group",
			apiGroup: "networking.k8s.io",
			version:  "v1",
			kind:     "Ingress",
			expected: "networking.k8s.io/v1.Ingress",
		},
		{
			name:     "apps API group",
			apiGroup: "apps",
			version:  "v1",
			kind:     "Deployment",
			expected: "apps/v1.Deployment",
		},
		{
			name:     "gateway API group",
			apiGroup: "gateway.networking.k8s.io",
			version:  "v1",
			kind:     "HTTPRoute",
			expected: "gateway.networking.k8s.io/v1.HTTPRoute",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := component.buildGVK(tt.apiGroup, tt.version, tt.kind)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestComponent_New_WithMetrics(t *testing.T) {
	metrics := &mockMetricsRecorder{}
	config := &Config{
		CertPEM: []byte("test-cert"),
		KeyPEM:  []byte("test-key"),
	}

	component := New(testutil.NewTestLogger(), config, nil, metrics)

	require.NotNil(t, component.metrics)
}

// mockMetricsRecorder is a mock implementation of MetricsRecorder.
type mockMetricsRecorder struct {
	requestsRecorded    int
	validationsRecorded int
	validationLabels    [][2]string
}

func (m *mockMetricsRecorder) RecordWebhookRequest(gvk, result string, durationSeconds float64) {
	m.requestsRecorded++
}

func (m *mockMetricsRecorder) RecordWebhookValidation(gvk, result string) {
	m.validationsRecorded++
	m.validationLabels = append(m.validationLabels, [2]string{gvk, result})
}

// The GVK is read off the wire and must not reach a Prometheus label because
// an arbitrary caller could create unbounded metric cardinality.
func TestComponent_reportUnregisteredGVK(t *testing.T) {
	tests := []struct {
		name string
		gvk  string
	}{
		{name: "well-formed kind", gvk: "networking.k8s.io/v1.Ingress"},
		{name: "attacker-controlled high-cardinality kind", gvk: "evil.example.com/v1.Kind-8f3a1c9d"},
		{name: "empty kind", gvk: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics := &mockMetricsRecorder{}
			component := New(testutil.NewTestLogger(), &Config{
				CertPEM: []byte("test-cert"),
				KeyPEM:  []byte("test-key"),
			}, nil, metrics)

			component.reportUnregisteredGVK(tt.gvk)

			require.Len(t, metrics.validationLabels, 1)
			assert.Equal(t, [2]string{unregisteredGVKLabel, "unregistered"}, metrics.validationLabels[0],
				"the wire-supplied gvk must not become a metric label value")
			assert.NotEqual(t, tt.gvk, metrics.validationLabels[0][0])
		})
	}
}

// A nil recorder must not panic — metrics are optional throughout this package.
func TestComponent_reportUnregisteredGVK_NilMetrics(t *testing.T) {
	component := New(testutil.NewTestLogger(), &Config{
		CertPEM: []byte("test-cert"),
		KeyPEM:  []byte("test-key"),
	}, nil, nil)

	assert.NotPanics(t, func() { component.reportUnregisteredGVK("v1.ConfigMap") })
}

func TestComponent_Start_MissingCertificate(t *testing.T) {
	config := &Config{
		// CertPEM is empty
		KeyPEM: []byte("test-key"),
	}

	component := New(testutil.NewTestLogger(), config, nil, nil)

	ctx := t.Context()
	err := component.Start(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no webhook TLS certificate configured")
}

func TestComponent_Start_MissingKey(t *testing.T) {
	config := &Config{
		CertPEM: []byte("test-cert"),
		// KeyPEM is empty
	}

	component := New(testutil.NewTestLogger(), config, nil, nil)

	ctx := t.Context()
	err := component.Start(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "tls private key is empty")
}

func TestComponent_resolveKind_Success(t *testing.T) {
	config := &Config{
		CertPEM: []byte("test-cert"),
		KeyPEM:  []byte("test-key"),
	}

	mapper := &mockRESTMapper{
		kindForResults: map[string]string{
			"networking.k8s.io/v1/ingresses": "Ingress",
			"/v1/configmaps":                 "ConfigMap",
		},
	}

	component := New(testutil.NewTestLogger(), config, mapper, nil)

	t.Run("ingress resource", func(t *testing.T) {
		kind, err := component.resolveKind("networking.k8s.io", "v1", "ingresses")
		require.NoError(t, err)
		assert.Equal(t, "Ingress", kind)
	})

	t.Run("core configmap resource", func(t *testing.T) {
		kind, err := component.resolveKind("", "v1", "configmaps")
		require.NoError(t, err)
		assert.Equal(t, "ConfigMap", kind)
	})
}

func TestComponent_resolveKind_Error(t *testing.T) {
	config := &Config{
		CertPEM: []byte("test-cert"),
		KeyPEM:  []byte("test-key"),
	}

	mapper := &mockRESTMapper{
		kindForResults: map[string]string{}, // Empty - no mappings
	}

	component := New(testutil.NewTestLogger(), config, mapper, nil)

	_, err := component.resolveKind("unknown", "v1", "unknowns")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolving kind")
}

func TestConstants(t *testing.T) {
	assert.Equal(t, 9443, DefaultWebhookPort)
	assert.Equal(t, "/validate", DefaultWebhookPath)
}

// mockRESTMapper is a minimal mock for testing resolveKind.
type mockRESTMapper struct {
	kindForResults map[string]string // key: "group/version/resource", value: kind
}

func (m *mockRESTMapper) KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	key := resource.Group + "/" + resource.Version + "/" + resource.Resource
	kind, ok := m.kindForResults[key]
	if !ok {
		return schema.GroupVersionKind{}, fmt.Errorf("no kind mapping for %v", resource)
	}
	return schema.GroupVersionKind{
		Group:   resource.Group,
		Version: resource.Version,
		Kind:    kind,
	}, nil
}

// Implement remaining RESTMapper interface methods as no-ops.
func (m *mockRESTMapper) KindsFor(schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockRESTMapper) ResourceFor(schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, fmt.Errorf("not implemented")
}
func (m *mockRESTMapper) ResourcesFor(schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockRESTMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockRESTMapper) RESTMappings(schema.GroupKind, ...string) ([]*meta.RESTMapping, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockRESTMapper) ResourceSingularizer(string) (string, error) {
	return "", fmt.Errorf("not implemented")
}

func TestComponent_registerValidators(t *testing.T) {
	certPEM, keyPEM := generateTestCertPEM(t)
	t.Run("registers validators for all rules", func(t *testing.T) {
		mapper := &mockRESTMapper{
			kindForResults: map[string]string{
				"networking.k8s.io/v1/ingresses": "Ingress",
				"/v1/configmaps":                 "ConfigMap",
			},
		}

		config := &Config{
			CertPEM:         certPEM,
			KeyPEM:          keyPEM,
			DryRunValidator: &mockDryRunValidator{allowed: true},
			Rules: []WebhookRule{
				{
					APIGroup:   "networking.k8s.io",
					APIVersion: "v1",
					Resource:   "ingresses",
				},
				{
					APIGroup:   "",
					APIVersion: "v1",
					Resource:   "configmaps",
				},
			},
		}

		component := New(testutil.NewTestLogger(), config, mapper, nil)

		// Create server so validators can be registered
		component.server, _ = webhook.NewServer(&webhook.ServerConfig{
			Port:    9443,
			Path:    "/validate",
			CertPEM: config.CertPEM,
			KeyPEM:  config.KeyPEM,
		})

		require.NoError(t, component.registerValidators())
	})

	t.Run("rejects a table with RESTMapper errors", func(t *testing.T) {
		// Empty mapper that will return errors for all lookups
		mapper := &mockRESTMapper{
			kindForResults: map[string]string{},
		}

		config := &Config{
			CertPEM:         certPEM,
			KeyPEM:          keyPEM,
			DryRunValidator: &mockDryRunValidator{allowed: true},
			Rules: []WebhookRule{
				{
					APIGroup:   "unknown.group",
					APIVersion: "v1",
					Resource:   "unknowns",
				},
			},
		}

		component := New(testutil.NewTestLogger(), config, mapper, nil)

		// Create server
		component.server, _ = webhook.NewServer(&webhook.ServerConfig{
			Port:    9443,
			Path:    "/validate",
			CertPEM: config.CertPEM,
			KeyPEM:  config.KeyPEM,
		})

		err := component.registerValidators()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown.group/v1/unknowns")
	})

	t.Run("handles empty rules", func(t *testing.T) {
		mapper := &mockRESTMapper{
			kindForResults: map[string]string{},
		}

		config := &Config{
			CertPEM: certPEM,
			KeyPEM:  keyPEM,
			Rules:   []WebhookRule{}, // Empty rules
		}

		component := New(testutil.NewTestLogger(), config, mapper, nil)

		// Create server
		component.server, _ = webhook.NewServer(&webhook.ServerConfig{
			Port:    9443,
			Path:    "/validate",
			CertPEM: config.CertPEM,
			KeyPEM:  config.KeyPEM,
		})

		require.NoError(t, component.registerValidators())
	})
}

func TestComponent_registerValidators_RequiresDryRunValidator(t *testing.T) {
	certPEM, keyPEM := generateTestCertPEM(t)
	component := New(testutil.NewTestLogger(), &Config{
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
		Rules: []WebhookRule{{
			APIVersion: "v1",
			Resource:   "configmaps",
		}},
	}, &mockRESTMapper{kindForResults: map[string]string{"/v1/configmaps": "ConfigMap"}}, nil)
	component.server, _ = webhook.NewServer(&webhook.ServerConfig{
		CertPEM: certPEM,
		KeyPEM:  keyPEM,
	})

	err := component.registerValidators()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "without a dry-run validator")
}

func TestComponent_createResourceValidator_MissingDryRunValidatorDenies(t *testing.T) {
	component := New(testutil.NewTestLogger(), &Config{}, nil, nil)
	validator := component.createResourceValidator("v1.ConfigMap")
	obj := &unstructured.Unstructured{}
	obj.SetName("test")

	allowed, reason, _, err := validator(&webhook.ValidationContext{Object: obj})

	assert.False(t, allowed)
	assert.Contains(t, reason, "validation is unavailable")
	assert.NoError(t, err)
}

// mockDryRunValidator is a mock implementation of DryRunValidator.
type mockDryRunValidator struct {
	allowed   bool
	reason    string
	warnings  []string
	object    any
	oldObject any
}

func (m *mockDryRunValidator) ValidateAdmission(_ context.Context, _, _, _ string, object, oldObject any, _ string) (allowed bool, reason string, warnings []string, failure error) {
	m.object = object
	m.oldObject = oldObject
	return m.allowed, m.reason, m.warnings, nil
}

type contextBlockingDryRunValidator struct{}

func (contextBlockingDryRunValidator) ValidateAdmission(ctx context.Context, _, _, _ string, _, _ any, _ string) (allowed bool, reason string, warnings []string, failure error) {
	<-ctx.Done()
	return false, ctx.Err().Error(), nil, nil
}

func TestComponent_createResourceValidator_ReturnsFunction(t *testing.T) {
	config := &Config{
		CertPEM: []byte("test-cert"),
		KeyPEM:  []byte("test-key"),
	}
	component := New(testutil.NewTestLogger(), config, nil, nil)

	validator := component.createResourceValidator("v1.ConfigMap")

	require.NotNil(t, validator)
}

func TestComponent_createResourceValidator_BasicValidationFails(t *testing.T) {
	config := &Config{
		CertPEM: []byte("test-cert"),
		KeyPEM:  []byte("test-key"),
	}
	component := New(testutil.NewTestLogger(), config, nil, nil)
	validator := component.createResourceValidator("v1.ConfigMap")

	// Create an invalid object (no name or generateName)
	obj := &unstructured.Unstructured{}
	// Don't set name or generateName

	valCtx := &webhook.ValidationContext{
		Operation: "CREATE",
		Namespace: "default",
		Name:      "test",
		Object:    obj,
	}

	allowed, reason, _, err := validator(valCtx)

	// Should deny due to basic validation failure
	assert.False(t, allowed)
	assert.Contains(t, reason, "metadata.name or metadata.generateName is required")
	assert.NoError(t, err)
}

func TestComponent_createResourceValidator_DryRunValidatorAllows(t *testing.T) {
	dryRunValidator := &mockDryRunValidator{
		allowed: true,
		reason:  "",
	}
	config := &Config{
		CertPEM:         []byte("test-cert"),
		KeyPEM:          []byte("test-key"),
		DryRunValidator: dryRunValidator,
	}
	component := New(testutil.NewTestLogger(), config, nil, nil)
	validator := component.createResourceValidator("v1.ConfigMap")

	// Create a valid unstructured object
	obj := &unstructured.Unstructured{}
	obj.SetName("test-config")

	valCtx := &webhook.ValidationContext{
		Operation: "CREATE",
		Namespace: "default",
		Name:      "test",
		Object:    obj,
	}

	allowed, reason, _, err := validator(valCtx)

	assert.True(t, allowed)
	assert.Empty(t, reason)
	assert.NoError(t, err)
}

func TestComponent_createResourceValidator_DeleteUsesOldObject(t *testing.T) {
	dryRunValidator := &mockDryRunValidator{allowed: true}
	component := New(testutil.NewTestLogger(), &Config{DryRunValidator: dryRunValidator}, nil, nil)
	validator := component.createResourceValidator("v1.ConfigMap")
	oldObject := &unstructured.Unstructured{}
	oldObject.SetName("test-config")

	allowed, _, _, err := validator(&webhook.ValidationContext{
		Operation: "DELETE",
		Namespace: "default",
		Name:      "test-config",
		OldObject: oldObject,
	})

	require.NoError(t, err)
	assert.True(t, allowed)
	assert.Nil(t, dryRunValidator.object)
	assert.Same(t, oldObject, dryRunValidator.oldObject)
}

func TestComponent_createResourceValidator_DryRunValidatorDenies(t *testing.T) {
	dryRunValidator := &mockDryRunValidator{
		allowed: false,
		reason:  "invalid configuration: HAProxy check failed",
	}
	config := &Config{
		CertPEM:         []byte("test-cert"),
		KeyPEM:          []byte("test-key"),
		DryRunValidator: dryRunValidator,
	}
	component := New(testutil.NewTestLogger(), config, nil, nil)
	validator := component.createResourceValidator("v1.ConfigMap")

	// Create a valid unstructured object
	obj := &unstructured.Unstructured{}
	obj.SetName("test-config")

	valCtx := &webhook.ValidationContext{
		Operation: "UPDATE",
		Namespace: "test-ns",
		Name:      "my-config",
		Object:    obj,
	}

	allowed, reason, _, err := validator(valCtx)

	assert.False(t, allowed)
	assert.Contains(t, reason, "invalid configuration")
	assert.NoError(t, err)
}

func TestComponent_createResourceValidator_TimeoutRemainsFailClosed(t *testing.T) {
	component := New(testutil.NewTestLogger(), &Config{
		DryRunValidator:          contextBlockingDryRunValidator{},
		ResourceAdmissionTimeout: 20 * time.Millisecond,
	}, nil, nil)
	validator := component.createResourceValidator("v1.ConfigMap")
	obj := &unstructured.Unstructured{}
	obj.SetName("test-config")

	allowed, reason, warnings, err := validator(&webhook.ValidationContext{
		Operation: "CREATE",
		Namespace: "default",
		Name:      "test-config",
		Object:    obj,
	})

	assert.False(t, allowed, "watched-resource admission must remain fail closed on its internal deadline")
	assert.Contains(t, reason, context.DeadlineExceeded.Error())
	assert.Empty(t, warnings)
	assert.NoError(t, err)
}

func TestComponent_createResourceValidator_MetricsOnSuccess(t *testing.T) {
	dryRunValidator := &mockDryRunValidator{
		allowed: true,
		reason:  "",
	}
	metrics := &mockMetricsRecorder{}
	config := &Config{
		CertPEM:         []byte("test-cert"),
		KeyPEM:          []byte("test-key"),
		DryRunValidator: dryRunValidator,
	}
	component := New(testutil.NewTestLogger(), config, nil, metrics)
	validator := component.createResourceValidator("v1.ConfigMap")

	// Create a valid unstructured object
	obj := &unstructured.Unstructured{}
	obj.SetName("test-config")

	valCtx := &webhook.ValidationContext{
		Operation: "CREATE",
		Namespace: "default",
		Name:      "test",
		Object:    obj,
	}

	allowed, reason, _, err := validator(valCtx)

	assert.True(t, allowed)
	assert.Empty(t, reason)
	assert.NoError(t, err)
	assert.Greater(t, metrics.requestsRecorded, 0)
	assert.Greater(t, metrics.validationsRecorded, 0)
}

func TestComponent_createResourceValidator_MetricsOnDenial(t *testing.T) {
	dryRunValidator := &mockDryRunValidator{
		allowed: false,
		reason:  "invalid configuration",
	}
	metrics := &mockMetricsRecorder{}
	config := &Config{
		CertPEM:         []byte("test-cert"),
		KeyPEM:          []byte("test-key"),
		DryRunValidator: dryRunValidator,
	}
	component := New(testutil.NewTestLogger(), config, nil, metrics)
	validator := component.createResourceValidator("v1.ConfigMap")

	// Create a valid unstructured object
	obj := &unstructured.Unstructured{}
	obj.SetName("test-config")

	valCtx := &webhook.ValidationContext{
		Operation: "UPDATE",
		Namespace: "test-ns",
		Name:      "my-config",
		Object:    obj,
	}

	allowed, reason, _, err := validator(valCtx)

	assert.False(t, allowed)
	assert.Contains(t, reason, "invalid configuration")
	assert.NoError(t, err)
	assert.Greater(t, metrics.requestsRecorded, 0)
	assert.Greater(t, metrics.validationsRecorded, 0)
}

func TestComponent_validateBasicStructure(t *testing.T) {
	component := &Component{}

	t.Run("valid object with name", func(t *testing.T) {
		obj := &unstructured.Unstructured{}
		obj.SetName("test-resource")

		err := component.validateBasicStructure(obj)
		assert.NoError(t, err)
	})

	t.Run("valid object with generateName", func(t *testing.T) {
		obj := &unstructured.Unstructured{}
		obj.SetGenerateName("test-resource-")

		err := component.validateBasicStructure(obj)
		assert.NoError(t, err)
	})

	t.Run("invalid object - no name or generateName", func(t *testing.T) {
		obj := &unstructured.Unstructured{}

		err := component.validateBasicStructure(obj)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "metadata.name or metadata.generateName is required")
	})

	t.Run("invalid object type", func(t *testing.T) {
		err := component.validateBasicStructure("not an unstructured object")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid object type")
	})
}
