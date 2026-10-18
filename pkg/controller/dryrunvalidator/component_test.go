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

package dryrunvalidator

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/proposalvalidator"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validation"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores/storetest"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// newTestRESTMapper builds a RESTMapper knowing the kinds the dryrunvalidator
// tests exercise, plus a CRD ("Mesh") whose real plural a naive English
// pluralizer gets wrong ("meshs" instead of "meshes"). The resolver must take
// the mapper's answer, never a guessed plural.
func newTestRESTMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	add := func(group, version, kind, plural, singular string) {
		m.AddSpecific(
			schema.GroupVersionKind{Group: group, Version: version, Kind: kind},
			schema.GroupVersionResource{Group: group, Version: version, Resource: plural},
			schema.GroupVersionResource{Group: group, Version: version, Resource: singular},
			meta.RESTScopeNamespace,
		)
	}
	add("networking.k8s.io", "v1", "Ingress", "ingresses", "ingress")
	add("", "v1", "Service", "services", "service")
	add("", "v1", "ConfigMap", "configmaps", "configmap")
	add("", "v1", "Secret", "secrets", "secret")
	add("", "v1", "Endpoints", "endpoints", "endpoints")
	add("discovery.k8s.io", "v1", "EndpointSlice", "endpointslices", "endpointslice")
	add("", "v1", "Pod", "pods", "pod")
	add("custom.example.io", "v1beta1", "MyResource", "myresources", "myresource")
	add("example.com", "v1", "Mesh", "meshes", "mesh") // naive pluralizer → "meshs"
	return m
}

func testWatchedResources() map[string]config.WatchedResource {
	return map[string]config.WatchedResource{
		"ingresses":      {APIVersion: "networking.k8s.io/v1", Resources: "ingresses"},
		"services":       {APIVersion: "v1", Resources: "services"},
		"configmaps":     {APIVersion: "v1", Resources: "configmaps"},
		"secrets":        {APIVersion: "v1", Resources: "secrets"},
		"endpoints":      {APIVersion: "v1", Resources: "endpoints"},
		"endpointslices": {APIVersion: "discovery.k8s.io/v1", Resources: "endpointslices"},
		"pods":           {APIVersion: "v1", Resources: "pods"},
		"custom":         {APIVersion: "custom.example.io/v1beta1", Resources: "myresources"},
		"mesh":           {APIVersion: "example.com/v1", Resources: "meshes"},
	}
}

// resettableFakeMapper simulates a deferred discovery mapper whose cache
// predates a late-registered CRD: RESTMapping returns NoMatch until Reset()
// refreshes discovery, after which it delegates to an inner mapper.
type resettableFakeMapper struct {
	meta.RESTMapper
	reset bool
}

func (m *resettableFakeMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if !m.reset {
		return nil, &meta.NoKindMatchError{GroupKind: gk}
	}
	return m.RESTMapper.RESTMapping(gk, versions...)
}

func (m *resettableFakeMapper) Reset() { m.reset = true }

// A late-registered CRD whose kind isn't in the mapper's initial discovery
// cache must resolve after the validator refreshes discovery via Reset(),
// rather than denying admission for the iteration's lifetime.
func TestMapGVKToResourceAliases_ResetsOnNoMatchThenRetries(t *testing.T) {
	rm := &resettableFakeMapper{RESTMapper: newTestRESTMapper()}
	aliases, err := buildResourceAliases(testWatchedResources())
	require.NoError(t, err)
	c := &Component{logger: slog.Default(), restMapper: rm, aliasesByGVR: aliases}

	resourceAliases, err := c.mapGVKToResourceAliases("networking.k8s.io/v1.Ingress")

	require.NoError(t, err)
	assert.True(t, rm.reset, "validator should Reset() the mapper on a NoMatch error")
	require.Len(t, resourceAliases, 1)
	assert.Equal(t, "ingresses", resourceAliases[0].name)
}

func TestMapGVKToResourceAliases(t *testing.T) {
	// Create a minimal component for testing
	aliases, err := buildResourceAliases(testWatchedResources())
	require.NoError(t, err)
	c := &Component{
		logger:       slog.Default(),
		restMapper:   newTestRESTMapper(),
		aliasesByGVR: aliases,
	}

	tests := []struct {
		name        string
		gvk         string
		expected    string
		expectError bool
	}{
		{
			name:        "Ingress - networking.k8s.io",
			gvk:         "networking.k8s.io/v1.Ingress",
			expected:    "ingresses",
			expectError: false,
		},
		{
			// A naive pluralizer produces "meshs"; the mapper knows "meshes".
			name:        "irregular plural comes from the mapper",
			gvk:         "example.com/v1.Mesh",
			expected:    "mesh",
			expectError: false,
		},
		{
			name:        "Service - core v1",
			gvk:         "v1.Service",
			expected:    "services",
			expectError: false,
		},
		{
			name:        "ConfigMap - core v1",
			gvk:         "v1.ConfigMap",
			expected:    "configmaps",
			expectError: false,
		},
		{
			name:        "Secret - core v1",
			gvk:         "v1.Secret",
			expected:    "secrets",
			expectError: false,
		},
		{
			name:        "Endpoints - core v1",
			gvk:         "v1.Endpoints",
			expected:    "endpoints",
			expectError: false,
		},
		{
			name:        "EndpointSlice - discovery.k8s.io",
			gvk:         "discovery.k8s.io/v1.EndpointSlice",
			expected:    "endpointslices",
			expectError: false,
		},
		{
			name:        "Pod - core v1",
			gvk:         "v1.Pod",
			expected:    "pods",
			expectError: false,
		},
		{
			name:        "Custom resource with group",
			gvk:         "custom.example.io/v1beta1.MyResource",
			expected:    "custom",
			expectError: false,
		},
		{
			name:        "Invalid GVK - no dot",
			gvk:         "invalid",
			expected:    "",
			expectError: true,
		},
		{
			name:        "Invalid GVK - only version",
			gvk:         "v1",
			expected:    "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := c.mapGVKToResourceAliases(tt.gvk)

			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "invalid GVK")
			} else {
				require.NoError(t, err)
				require.Len(t, result, 1)
				assert.Equal(t, tt.expected, result[0].name)
			}
		})
	}
}

func TestCreateAliasOverlay_SelectorTransitions(t *testing.T) {
	aliases, err := buildResourceAliases(map[string]config.WatchedResource{
		"selected": {
			APIVersion:    "networking.k8s.io/v1",
			Resources:     "ingresses",
			FieldSelector: "spec.ingressClassName=haptic",
		},
	})
	require.NoError(t, err)
	alias := aliases[schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}][0]
	matching := createTestIngressWithClass("test-ingress", "haptic")
	nonMatching := createTestIngressWithClass("test-ingress", "other")

	tests := []struct {
		name                string
		operation           string
		newResource         *unstructured.Unstructured
		oldResource         *unstructured.Unstructured
		expectAdditions     int
		expectModifications int
		expectDeletions     int
	}{
		{name: "create matching", operation: operationCreate, newResource: matching, expectAdditions: 1},
		{name: "create non-matching", operation: operationCreate, newResource: nonMatching},
		{name: "update remains matching", operation: operationUpdate, newResource: matching, oldResource: matching, expectModifications: 1},
		{name: "update enters selector", operation: operationUpdate, newResource: matching, oldResource: nonMatching, expectAdditions: 1},
		{name: "update leaves selector", operation: operationUpdate, newResource: nonMatching, oldResource: matching, expectDeletions: 1},
		{name: "update remains excluded", operation: operationUpdate, newResource: nonMatching, oldResource: nonMatching},
		{name: "delete matching", operation: operationDelete, oldResource: matching, expectDeletions: 1},
		{name: "delete non-matching", operation: operationDelete, oldResource: nonMatching},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overlay, err := createAliasOverlay(alias, "default", "test-ingress", tt.newResource, tt.oldResource, tt.operation)
			require.NoError(t, err)
			assert.Len(t, overlay.Additions, tt.expectAdditions)
			assert.Len(t, overlay.Modifications, tt.expectModifications)
			assert.Len(t, overlay.Deletions, tt.expectDeletions)
		})
	}
}

func TestCreateAliasOverlay_NamespaceScope(t *testing.T) {
	aliases, err := buildResourceAliases(map[string]config.WatchedResource{
		"scoped": {
			APIVersion: "networking.k8s.io/v1",
			Resources:  "ingresses",
			Namespaces: []string{"team-a"},
		},
	})
	require.NoError(t, err)
	alias := aliases[schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}][0]
	inside := createTestIngress("app", "team-a")
	outside := createTestIngress("app", "team-b")

	tests := []struct {
		name          string
		operation     string
		namespace     string
		newResource   *unstructured.Unstructured
		oldResource   *unstructured.Unstructured
		wantSubjected bool
	}{
		{name: "create inside", operation: operationCreate, namespace: "team-a", newResource: inside, wantSubjected: true},
		{name: "create outside", operation: operationCreate, namespace: "team-b", newResource: outside},
		{name: "update inside", operation: operationUpdate, namespace: "team-a", newResource: inside, oldResource: inside, wantSubjected: true},
		{name: "update outside", operation: operationUpdate, namespace: "team-b", newResource: outside, oldResource: outside},
		{name: "delete inside without old object", operation: operationDelete, namespace: "team-a", wantSubjected: true},
		{name: "delete outside without old object", operation: operationDelete, namespace: "team-b"},
		{name: "delete outside", operation: operationDelete, namespace: "team-b", oldResource: outside},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overlay, err := createAliasOverlay(alias, tt.namespace, "app", tt.newResource, tt.oldResource, tt.operation)
			require.NoError(t, err)
			assert.Equal(t, tt.wantSubjected, !overlay.IsEmpty())
		})
	}
}

func TestCreateAliasOverlay_MissingOldObjectFailsClosedForFilteredStore(t *testing.T) {
	aliases, err := buildResourceAliases(map[string]config.WatchedResource{
		"selected": {
			APIVersion:    "networking.k8s.io/v1",
			Resources:     "ingresses",
			FieldSelector: "spec.ingressClassName=haptic",
		},
	})
	require.NoError(t, err)
	alias := aliases[schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}][0]

	_, err = createAliasOverlay(alias, "default", "app", createTestIngressWithClass("app", "haptic"), nil, operationUpdate)
	require.Error(t, err)
	_, err = createAliasOverlay(alias, "default", "app", nil, nil, operationDelete)
	require.Error(t, err)
	_, err = createAliasOverlay(alias, "default", "app", createTestIngressWithClass("app", "haptic"), nil, operationDelete)
	require.Error(t, err)
}

func TestCreateAliasOverlay_RejectsUnsupportedOperation(t *testing.T) {
	_, err := createAliasOverlay(
		resourceAlias{name: "ingresses"},
		"default",
		"app",
		createTestIngress("app", "default"),
		nil,
		"CONNECT",
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported admission operation")
}

func TestCreateOverlays_MultipleAliasesForOneGVR(t *testing.T) {
	resources := map[string]config.WatchedResource{
		"internal-routes": {
			APIVersion:    "networking.k8s.io/v1",
			Resources:     "ingresses",
			FieldSelector: "spec.ingressClassName=internal",
		},
		"public-routes": {
			APIVersion:    "networking.k8s.io/v1",
			Resources:     "ingresses",
			FieldSelector: "spec.ingressClassName=public",
		},
	}
	aliases, err := buildResourceAliases(resources)
	require.NoError(t, err)
	c := &Component{aliasesByGVR: aliases, restMapper: newTestRESTMapper()}

	mapped, err := c.mapGVKToResourceAliases("networking.k8s.io/v1.Ingress")
	require.NoError(t, err)
	overlays, subjects, err := c.createOverlays(
		mapped,
		"default",
		"app",
		createTestIngressWithClass("app", "internal"),
		nil,
		operationCreate,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"internal-routes"}, subjects)
	assert.Len(t, overlays["internal-routes"].Additions, 1)
	assert.True(t, overlays["public-routes"].IsEmpty())
}

func TestResourceAliasMatchesLabelSelector(t *testing.T) {
	alias := resourceAlias{labelSelector: map[string]string{"tenant": "blue", "managed": "true"}}
	resource := createTestIngress("app", "default")
	resource.SetLabels(map[string]string{"tenant": "blue", "managed": "true", "extra": "kept"})
	matches, err := alias.matches(resource)
	require.NoError(t, err)
	assert.True(t, matches)

	resource.SetLabels(map[string]string{"tenant": "blue"})
	matches, err = alias.matches(resource)
	require.NoError(t, err)
	assert.False(t, matches)
}

func TestMapGVKToResourceAliases_UsesConfiguredAliasNotPlural(t *testing.T) {
	aliases, err := buildResourceAliases(map[string]config.WatchedResource{
		"application-routes": {APIVersion: "networking.k8s.io/v1", Resources: "ingresses"},
	})
	require.NoError(t, err)
	c := &Component{aliasesByGVR: aliases, restMapper: newTestRESTMapper()}

	mapped, err := c.mapGVKToResourceAliases("networking.k8s.io/v1.Ingress")
	require.NoError(t, err)
	require.Len(t, mapped, 1)
	assert.Equal(t, "application-routes", mapped[0].name)
}

func TestSimplifyError(t *testing.T) {
	c := &Component{}

	tests := []struct {
		name     string
		phase    string
		err      error
		expected string
	}{
		{
			name:     "nil error",
			phase:    "render",
			err:      nil,
			expected: "",
		},
		{
			name:     "render phase",
			phase:    "render",
			err:      errors.New("template error"),
			expected: "template error",
		},
		{
			name:     "syntax phase",
			phase:    "syntax",
			err:      errors.New("syntax error"),
			expected: "syntax error",
		},
		{
			name:     "unknown phase",
			phase:    "unknown",
			err:      errors.New("some error"),
			expected: "some error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := c.simplifyError(tt.phase, tt.err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNew(t *testing.T) {
	logger := slog.Default()

	proposalValidator := createMockProposalValidator(logger)

	component, err := New(&ComponentConfig{
		RESTMapper:        newTestRESTMapper(),
		ProposalValidator: proposalValidator,
		WatchedResources:  testWatchedResources(),
		Logger:            logger,
	})

	require.NoError(t, err)
	require.NotNil(t, component)
	assert.NotNil(t, component.logger)
}

func TestNewRejectsInvalidFieldSelector(t *testing.T) {
	component, err := New(&ComponentConfig{
		RESTMapper: newTestRESTMapper(),
		WatchedResources: map[string]config.WatchedResource{
			"routes": {APIVersion: "networking.k8s.io/v1", Resources: "ingresses", FieldSelector: "missing-equals"},
		},
	})

	require.Error(t, err)
	assert.Nil(t, component)
}

// TestValidateDirect_UpdateSuccess tests the full flow for an UPDATE operation.
func TestValidateDirect_UpdateSuccess(t *testing.T) {
	logger := slog.Default()

	proposalValidator := createMockProposalValidator(logger)

	component, err := New(&ComponentConfig{
		RESTMapper:        newTestRESTMapper(),
		ProposalValidator: proposalValidator,
		WatchedResources:  testWatchedResources(),
		Logger:            logger,
	})
	require.NoError(t, err)

	allowed, reason, _ := component.ValidateDirect(
		context.Background(),
		"networking.k8s.io/v1.Ingress",
		"staging",
		"updated-ingress",
		createTestIngress("updated-ingress", "staging"),
		createTestIngress("updated-ingress", "staging"),
		"UPDATE",
	)

	assert.True(t, allowed)
	assert.Empty(t, reason)
}

// TestValidateDirect_DeleteSuccess tests the full flow for a DELETE operation.
func TestValidateDirect_DeleteSuccess(t *testing.T) {
	logger := slog.Default()

	proposalValidator := createMockProposalValidator(logger)

	component, err := New(&ComponentConfig{
		RESTMapper:        newTestRESTMapper(),
		ProposalValidator: proposalValidator,
		WatchedResources:  testWatchedResources(),
		Logger:            logger,
	})
	require.NoError(t, err)

	allowed, reason, _ := component.ValidateDirect(
		context.Background(),
		"networking.k8s.io/v1.Ingress",
		"default",
		"test-ingress",
		nil,
		createTestIngress("test-ingress", "default"),
		"DELETE",
	)

	assert.True(t, allowed)
	assert.Empty(t, reason)
}

// TestValidateDirect_OverlayReferencesInvalidStore tests that overlays
// referencing non-existent stores produce a denial.
func TestValidateDirect_OverlayReferencesInvalidStore(t *testing.T) {
	logger := slog.Default()

	// Create proposal validator with store provider that has NO stores
	engine, err := templating.New(map[string]string{"haproxy.cfg": testutil.ValidHAProxyConfigTemplate}, nil)
	require.NoError(t, err)

	renderService := renderer.NewRenderService(&renderer.RenderServiceConfig{
		Engine: engine,
		Config: &config.Config{},
		Logger: logger,
	})

	validationService := validation.NewValidationService(&validation.ValidationServiceConfig{
		Logger:            logger,
		SkipDNSValidation: true,
	})

	pipelineInstance := pipeline.New(&pipeline.PipelineConfig{
		Renderer:  renderService,
		Validator: validationService,
		Logger:    logger,
	})

	// Empty store provider — overlay for "ingresses" will fail validation
	emptyStoreProvider := stores.NewRealStoreProvider(map[string]stores.Store{})

	noStoreProposalValidator := proposalvalidator.NewService(&proposalvalidator.ServiceConfig{
		Pipeline:          pipelineInstance,
		BaseStoreProvider: emptyStoreProvider,
		Logger:            logger,
	})

	component, err := New(&ComponentConfig{
		RESTMapper:        newTestRESTMapper(),
		ProposalValidator: noStoreProposalValidator,
		WatchedResources:  testWatchedResources(),
		Logger:            logger,
	})
	require.NoError(t, err)

	allowed, reason, _ := component.ValidateDirect(
		context.Background(),
		"networking.k8s.io/v1.Ingress",
		"default",
		"test-ingress",
		createTestIngress("test-ingress", "default"),
		nil,
		"CREATE",
	)

	assert.False(t, allowed)
	assert.Contains(t, reason, "non-existent store")
}

// TestValidateDirect_Success tests the synchronous validation path.
func TestValidateDirect_Success(t *testing.T) {
	logger := slog.Default()

	proposalValidator := createMockProposalValidator(logger)

	component, err := New(&ComponentConfig{
		RESTMapper:        newTestRESTMapper(),
		ProposalValidator: proposalValidator,
		WatchedResources:  testWatchedResources(),
		Logger:            logger,
	})
	require.NoError(t, err)

	allowed, reason, _ := component.ValidateDirect(
		context.Background(),
		"networking.k8s.io/v1.Ingress",
		"default",
		"test-ingress",
		createTestIngress("test-ingress", "default"),
		nil,
		"CREATE",
	)

	assert.True(t, allowed)
	assert.Empty(t, reason)
}

func TestValidateDirect_ConfiguredAliasDiffersFromPlural(t *testing.T) {
	logger := slog.Default()
	proposalValidator := createMockProposalValidatorWithStores(logger, "application-routes")
	component, err := New(&ComponentConfig{
		RESTMapper:        newTestRESTMapper(),
		ProposalValidator: proposalValidator,
		WatchedResources: map[string]config.WatchedResource{
			"application-routes": {APIVersion: "networking.k8s.io/v1", Resources: "ingresses"},
		},
		Logger: logger,
	})
	require.NoError(t, err)

	allowed, reason, _ := component.ValidateDirect(
		context.Background(),
		"networking.k8s.io/v1.Ingress",
		"default",
		"app",
		createTestIngress("app", "default"),
		nil,
		operationCreate,
	)

	assert.True(t, allowed)
	assert.Empty(t, reason)
}

// TestValidateDirect_InvalidGVK tests that ValidateDirect rejects invalid GVKs.
func TestValidateDirect_InvalidGVK(t *testing.T) {
	logger := slog.Default()

	proposalValidator := createMockProposalValidator(logger)

	component, err := New(&ComponentConfig{
		RESTMapper:        newTestRESTMapper(),
		ProposalValidator: proposalValidator,
		WatchedResources:  testWatchedResources(),
		Logger:            logger,
	})
	require.NoError(t, err)

	allowed, reason, _ := component.ValidateDirect(
		context.Background(),
		"invalid",
		"default",
		"test",
		nil,
		nil,
		"CREATE",
	)

	assert.False(t, allowed)
	assert.Contains(t, reason, "unsupported resource type")
}

func TestValidateDirect_AlwaysFailingTemplate_Denies(t *testing.T) {
	logger := slog.Default()

	failingEngine, err := templating.New(map[string]string{"haproxy.cfg": `{{ fail("invalid config") }}`}, nil)
	require.NoError(t, err)

	renderService := renderer.NewRenderService(&renderer.RenderServiceConfig{
		Engine: failingEngine,
		Config: &config.Config{},
		Logger: logger,
	})

	validationService := validation.NewValidationService(&validation.ValidationServiceConfig{
		Logger:            logger,
		SkipDNSValidation: true,
	})

	pipelineInstance := pipeline.New(&pipeline.PipelineConfig{
		Renderer:  renderService,
		Validator: validationService,
		Logger:    logger,
	})

	baseStoreProvider := stores.NewRealStoreProvider(map[string]stores.Store{
		"ingresses": &storetest.MockStore{},
	})

	failingProposalValidator := proposalvalidator.NewService(&proposalvalidator.ServiceConfig{
		Pipeline:          pipelineInstance,
		BaseStoreProvider: baseStoreProvider,
		Logger:            logger,
	})

	component, err := New(&ComponentConfig{
		RESTMapper:        newTestRESTMapper(),
		ProposalValidator: failingProposalValidator,
		WatchedResources:  testWatchedResources(),
		Logger:            logger,
	})
	require.NoError(t, err)

	allowed, reason, _ := component.ValidateDirect(
		context.Background(),
		"networking.k8s.io/v1.Ingress",
		"default",
		"test-ingress",
		createTestIngress("test-ingress", "default"),
		nil,
		"CREATE",
	)

	assert.False(t, allowed)
	assert.Contains(t, reason, "invalid config")
}

// createMockProposalValidator creates a minimal ProposalValidator for testing.
func createMockProposalValidator(logger *slog.Logger) *proposalvalidator.Service {
	return createMockProposalValidatorWithStores(logger, "ingresses", "services")
}

func createMockProposalValidatorWithStores(logger *slog.Logger, names ...string) *proposalvalidator.Service {
	// Create minimal render service
	engine, _ := templating.New(map[string]string{"haproxy.cfg": testutil.ValidHAProxyConfigTemplate}, nil)

	renderService := renderer.NewRenderService(&renderer.RenderServiceConfig{
		Engine: engine,
		Config: &config.Config{},
		Logger: logger,
	})

	// Create minimal validation service
	validationService := validation.NewValidationService(&validation.ValidationServiceConfig{
		Logger:            logger,
		SkipDNSValidation: true,
	})

	// Create pipeline
	pipelineInstance := pipeline.New(&pipeline.PipelineConfig{
		Renderer:  renderService,
		Validator: validationService,
		Logger:    logger,
	})

	// Create base store provider
	storeMap := make(map[string]stores.Store, len(names))
	for _, name := range names {
		storeMap[name] = &storetest.MockStore{}
	}
	baseStoreProvider := stores.NewRealStoreProvider(storeMap)

	return proposalvalidator.NewService(&proposalvalidator.ServiceConfig{
		Pipeline:          pipelineInstance,
		BaseStoreProvider: baseStoreProvider,
		Logger:            logger,
	})
}

// createTestIngress creates a test unstructured ingress object.
func createTestIngress(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "Ingress",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"rules": []any{},
			},
		},
	}
}

func createTestIngressWithClass(name, class string) *unstructured.Unstructured {
	resource := createTestIngress(name, "default")
	resource.Object["spec"].(map[string]any)["ingressClassName"] = class
	return resource
}
