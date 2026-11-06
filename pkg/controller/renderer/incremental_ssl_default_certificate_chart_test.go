// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package renderer

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

const (
	sslDefaultCertificateComponent      = "ssl-default-certificates-100-secret"
	sslDefaultECDSACertificateComponent = "ssl-default-ecdsa-certificates-100-secret"
	sslDefaultCertificateRoot           = `{{- render "ssl-default-certificates-100-secret" -}}
{%- for _, certificate := range incremental_values("ssl-default-certificates", "default") %}
# default={{ b64decode(tostring(dig(certificate, "data", "tls.crt"))) }}
{%- end %}
{{- render "ssl-default-ecdsa-certificates-100-secret" -}}
{%- var ecdsaCertificates = incremental_values("ssl-default-ecdsa-certificates", "ecdsa") -%}
{%- if len(ecdsaCertificates) == 0 -%}
{{ fail("Default ECDSA TLS Secret not found") }}
{%- end -%}
{%- for _, certificate := range ecdsaCertificates %}
# ecdsa={{ b64decode(tostring(dig(certificate, "data", "tls.crt"))) }}
{%- end %}`
	sslDefaultCertificateGatewayWinnerRoot = `{{- render "ssl-test-empty-mtls-host-policies" -}}
{{- render "features-050-ssl-initialization" -}}
{%- var gf = shared.Get("globalFeatures").(map[string]any) -%}
{%- gf["tlsCertificates"] = []any{map[string]any{
  "is_gateway_default": true,
  "secret_namespace": "default",
  "secret_name": "gateway",
  "sanitized_filename": "gateway.pem",
}} -%}
{{- render "features-150-ssl-crtlist" -}}`
)

type sslDefaultCertificateLibrary struct {
	TemplateSnippets map[string]sslDefaultCertificateSnippet `yaml:"templateSnippets"`
	SSLCertificates  map[string]config.SSLCertificate        `yaml:"sslCertificates"`
}

type sslDefaultCertificateSnippet struct {
	Incremental *sslDefaultCertificateIncremental `yaml:"incremental"`
}

type sslDefaultCertificateIncremental struct {
	Mode config.IncrementalMode `yaml:"mode"`
}

type sslDefaultCertificateFixture struct {
	service  *RenderService
	secrets  *k8sstore.MemoryStore
	provider stores.StoreProvider
}

func TestSSLDefaultCertificatePublicationsTrackSeparateCellsAndABA(t *testing.T) {
	fixture := newSSLDefaultCertificateFixture(t, "rsa", "ecdsa")
	fixture.addSecret(t, sslDefaultCertificateSecret("rsa", "RSA-CERT-A", "RSA-KEY-A", nil))
	fixture.addSecret(t, sslDefaultCertificateSecret("ecdsa", "ECDSA-CERT-A", "ECDSA-KEY-A", nil))

	cold := fixture.renderAndCommit(t)
	assert.Contains(t, cold.HAProxyConfig, "# default=RSA-CERT-A")
	assert.Contains(t, cold.HAProxyConfig, "# ecdsa=ECDSA-CERT-A")
	assert.Equal(t, "RSA-CERT-ARSA-KEY-A\n", sslDefaultCertificateContent(t, cold))
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 1)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 1)

	warm := fixture.renderAndCommit(t)
	assertRenderResultObservablesEqual(t, cold, warm)
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 1)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 1)

	fixture.addSecret(t, sslDefaultCertificateSecret("unrelated", "OTHER-CERT", "OTHER-KEY", nil))
	unrelated := fixture.renderAndCommit(t)
	assert.Equal(t, sslDefaultCertificateSnapshot(t, cold), sslDefaultCertificateSnapshot(t, unrelated))
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 1)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 1)

	fixture.updateSecret(t, sslDefaultCertificateSecret(
		"rsa", "RSA-CERT-A", "RSA-KEY-A", map[string]any{"revision": "metadata-only"},
	))
	metadataOnly := fixture.renderAndCommit(t)
	assert.Equal(t, sslDefaultCertificateSnapshot(t, cold), sslDefaultCertificateSnapshot(t, metadataOnly))
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 2)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 1)

	fixture.updateSecret(t, sslDefaultCertificateSecret("rsa", "RSA-CERT-B", "RSA-KEY-B", nil))
	rsaChanged := fixture.renderAndCommit(t)
	assert.Contains(t, rsaChanged.HAProxyConfig, "# default=RSA-CERT-B")
	assert.Equal(t, "RSA-CERT-BRSA-KEY-B\n", sslDefaultCertificateContent(t, rsaChanged))
	assert.Contains(t, rsaChanged.HAProxyConfig, "# ecdsa=ECDSA-CERT-A")
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 3)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 1)

	fixture.updateSecret(t, sslDefaultCertificateSecret("ecdsa", "ECDSA-CERT-B", "ECDSA-KEY-B", nil))
	ecdsaChanged := fixture.renderAndCommit(t)
	assert.Contains(t, ecdsaChanged.HAProxyConfig, "# ecdsa=ECDSA-CERT-B")
	assert.Equal(t, "RSA-CERT-BRSA-KEY-B\n", sslDefaultCertificateContent(t, ecdsaChanged))
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 3)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 2)

	assertSSLDefaultCertificateSecretRecreationBackdates(t, fixture, ecdsaChanged)
}

func assertSSLDefaultCertificateSecretRecreationBackdates(
	t *testing.T,
	fixture *sslDefaultCertificateFixture,
	ecdsaChanged *RenderResult,
) {
	t.Helper()
	require.NoError(t, fixture.secrets.Delete("haptic", "rsa", []string{"haptic", "rsa"}))
	failed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "TLS Secret not found: haptic/rsa")
	assert.Nil(t, failed)
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 3)

	fixture.addSecret(t, sslDefaultCertificateSecret("rsa", "RSA-CERT-B", "RSA-KEY-B", nil))
	recreated := fixture.renderAndCommit(t)
	assert.Equal(t, sslDefaultCertificateSnapshot(t, ecdsaChanged), sslDefaultCertificateSnapshot(t, recreated))
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 3)
	fixture.assertBackdates(t, sslDefaultCertificateComponent, 1)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 2)

	require.NoError(t, fixture.secrets.Delete("haptic", "ecdsa", []string{"haptic", "ecdsa"}))
	failed, err = fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "Default ECDSA TLS Secret not found")
	assert.Nil(t, failed)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 2)

	fixture.addSecret(t, sslDefaultCertificateSecret("ecdsa", "ECDSA-CERT-B", "ECDSA-KEY-B", nil))
	recreated = fixture.renderAndCommit(t)
	assert.Equal(t, sslDefaultCertificateSnapshot(t, ecdsaChanged), sslDefaultCertificateSnapshot(t, recreated))
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 2)
	fixture.assertBackdates(t, sslDefaultECDSACertificateComponent, 1)
}

func TestSSLDefaultCertificateOneSecretCanPublishBothCells(t *testing.T) {
	fixture := newSSLDefaultCertificateFixture(t, "combined", "combined")
	fixture.addSecret(t, sslDefaultCertificateSecret("combined", "COMBINED-CERT", "COMBINED-KEY", nil))

	result := fixture.renderAndCommit(t)
	assert.Contains(t, result.HAProxyConfig, "# default=COMBINED-CERT")
	assert.Contains(t, result.HAProxyConfig, "# ecdsa=COMBINED-CERT")
	assert.Equal(t, "COMBINED-CERTCOMBINED-KEY\n", sslDefaultCertificateContent(t, result))
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 1)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 1)

	fixture.updateSecret(t, sslDefaultCertificateSecret("combined", "COMBINED-CERT-B", "COMBINED-KEY-B", nil))
	updated := fixture.renderAndCommit(t)
	assert.Contains(t, updated.HAProxyConfig, "# default=COMBINED-CERT-B")
	assert.Contains(t, updated.HAProxyConfig, "# ecdsa=COMBINED-CERT-B")
	assert.Equal(t, "COMBINED-CERT-BCOMBINED-KEY-B\n", sslDefaultCertificateContent(t, updated))
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 2)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 2)
}

func TestSSLDefaultCertificateECDSAProjectionIsLazyBehindGatewayDefault(t *testing.T) {
	fixture := newSSLDefaultCertificateFixtureWithRoot(
		t, "rsa", "ecdsa", sslDefaultCertificateGatewayWinnerRoot,
	)
	fixture.addSecret(t, sslDefaultCertificateSecret("rsa", "RSA-CERT", "RSA-KEY", nil))
	fixture.addSecret(t, sslDefaultCertificateSecret("ecdsa", "ECDSA-CERT-A", "ECDSA-KEY-A", nil))

	cold := fixture.renderAndCommit(t)
	assert.Equal(t, "RSA-CERTRSA-KEY\n", sslDefaultCertificateContent(t, cold))
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 1)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 0)

	fixture.updateSecret(t, sslDefaultCertificateSecret("ecdsa", "ECDSA-CERT-B", "ECDSA-KEY-B", nil))
	warm := fixture.renderAndCommit(t)
	assertRenderResultObservablesEqual(t, cold, warm)
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 1)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 0)

	require.NoError(t, fixture.secrets.Delete("haptic", "ecdsa", []string{"haptic", "ecdsa"}))
	deleted := fixture.renderAndCommit(t)
	assertRenderResultObservablesEqual(t, cold, deleted)
	fixture.assertExecutions(t, sslDefaultCertificateComponent, 1)
	fixture.assertExecutions(t, sslDefaultECDSACertificateComponent, 0)
}

func TestMigratedChartRootsDoNotReadWatchedResourcesDirectly(t *testing.T) {
	snippets := loadGatewayHostMapSnippets(t, gatewayHostMapChartRoot(t), map[string][]string{
		"gateway/10-features.yaml": {
			"util-aggregate-gateway-cert-metadata",
			"features-100-gateway-tls",
			"util-gateway-listener-mtls-bindings",
			"gateway-listener-mtls-100-gateway",
			"features-110-gateway-frontend-mtls",
			"features-140-gateway-service-extra-ports",
			"features-141-listenerset-service-extra-ports",
			"features-150-gateway-bind",
		},
		"gateway/18-bind-per-gateway.yaml": {
			"https-bind-extra-100-per-gateway",
			"http-bind-extra-100-per-gateway",
		},
		"gateway/30-backends.yaml": {"backends-501-gateway-ssl-passthrough"},
		"ssl/library.yaml":         {"features-150-ssl-crtlist"},
	})
	for name, snippet := range snippets {
		assert.NotContains(t, snippet.Template, "resources.", name)
	}

	sslLibraryPath := filepath.Join(gatewayHostMapChartRoot(t), "ssl", "library.yaml")
	content, err := os.ReadFile(sslLibraryPath)
	require.NoError(t, err)
	var library sslDefaultCertificateLibrary
	require.NoError(t, yaml.Unmarshal(content, &library))
	for _, name := range []string{
		sslDefaultCertificateComponent,
		sslDefaultECDSACertificateComponent,
	} {
		chartSnippet, found := library.TemplateSnippets[name]
		require.True(t, found)
		require.NotNil(t, chartSnippet.Incremental)
		require.Equal(t, config.IncrementalModeResourceProjection, chartSnippet.Incremental.Mode)
	}
	defaultCertificate, found := library.SSLCertificates["default.pem"]
	require.True(t, found)
	assert.NotContains(t, defaultCertificate.Template, "resources.")
}

func newSSLDefaultCertificateFixture(
	t *testing.T,
	defaultName, ecdsaName string,
) *sslDefaultCertificateFixture {
	t.Helper()
	return newSSLDefaultCertificateFixtureWithRoot(
		t, defaultName, ecdsaName, sslDefaultCertificateRoot,
	)
}

func newSSLDefaultCertificateFixtureWithRoot(
	t *testing.T,
	defaultName, ecdsaName, root string,
) *sslDefaultCertificateFixture {
	t.Helper()
	snippets := loadGatewayHostMapSnippets(t, gatewayHostMapChartRoot(t), map[string][]string{
		"ssl/library.yaml": {
			"util-ssl-default-certificate-bindings",
			"util-ssl-default-ecdsa-certificate-bindings",
			"util-ssl-certificate-hosts",
			"util-ssl-additional-certificate-bindings",
			"ssl-additional-certificate-publications",
			sslDefaultCertificateComponent,
			sslDefaultECDSACertificateComponent,
			"features-050-ssl-initialization",
			"util-crtlist-line",
			"features-150-ssl-crtlist",
		},
	})
	if root == sslDefaultCertificateGatewayWinnerRoot {
		snippets["ssl-test-empty-mtls-host-policies"] = config.TemplateSnippet{
			Template: `{{- "" -}}`,
			Incremental: &config.IncrementalTemplate{
				Mode:             config.IncrementalModeResourceProjection,
				BindingsTemplate: "{}",
				Group:            "mtls-host-policies",
				Effects:          []config.IncrementalEffect{config.IncrementalEffectPublishValue},
			},
		}
	}
	sslLibraryPath := filepath.Join(gatewayHostMapChartRoot(t), "ssl", "library.yaml")
	content, err := os.ReadFile(sslLibraryPath)
	require.NoError(t, err)
	var library sslDefaultCertificateLibrary
	require.NoError(t, yaml.Unmarshal(content, &library))
	for _, name := range []string{
		sslDefaultCertificateComponent,
		sslDefaultECDSACertificateComponent,
	} {
		snippet := snippets[name]
		require.NotNil(t, snippet.Incremental)
		require.Equal(t, config.IncrementalModeResourceProjection, snippet.Incremental.Mode)
	}
	defaultCertificate, found := library.SSLCertificates["default.pem"]
	require.True(t, found)

	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"secrets": {
				APIVersion: "v1", Resources: "secrets",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: snippets,
		HAProxyConfig:    config.HAProxyConfig{Template: root},
		SSLCertificates: map[string]config.SSLCertificate{
			"default.pem": defaultCertificate,
		},
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"tls": map[string]any{"defaultCertificate": map[string]any{
				"namespace": "haptic", "name": defaultName, "ecdsaName": ecdsaName,
			}},
		}},
	}
	require.NoError(t, config.ValidateTemplateStructure(cfg))
	allTypes := gatewayRouteAnalysisSchemaTypes(t)
	types := &typebootstrap.Result{
		Types:  map[string]reflect.Type{"secrets": allTypes.Types["secrets"]},
		Kinds:  map[string]string{"secrets": allTypes.Kinds["secrets"]},
		Errors: map[string]error{},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	assertSSLDefaultCertificateBindings(
		t,
		engine,
		service.incremental,
		cfg.TemplatingSettings.ExtraContext,
		defaultName,
		ecdsaName,
	)
	secrets := k8sstore.NewMemoryStore(2)
	return &sslDefaultCertificateFixture{
		service: service,
		secrets: secrets,
		provider: stores.NewRealStoreProvider(map[string]stores.Store{
			"secrets": secrets,
		}),
	}
}

func assertSSLDefaultCertificateBindings(
	t *testing.T,
	engine templating.Engine,
	state *incrementalRenderState,
	extraContext map[string]any,
	defaultName, ecdsaName string,
) {
	t.Helper()
	planner, ok := engine.(templating.IncrementalBindingPlannerExecutor)
	require.True(t, ok)
	snapshotPlanner, ok := engine.(templating.IncrementalBindingSnapshotPlanner)
	require.True(t, ok)
	bindingContext := map[string]any{"extraContext": extraContext}
	entryPoints := []string{
		helpers.IncrementalBindingsEntryPointName(sslDefaultCertificateComponent),
		helpers.IncrementalBindingsEntryPointName(sslDefaultECDSACertificateComponent),
	}
	snapshot, err := snapshotPlanner.SnapshotIncrementalBindingInputs(entryPoints, bindingContext)
	require.NoError(t, err)
	plan, _, exact, err := state.prepareBindingPlan(t.Context(), bindingContext)
	require.NoError(t, err)
	require.True(t, exact)
	tests := []struct {
		component string
		cell      string
		name      string
	}{
		{component: sslDefaultCertificateComponent, cell: "default", name: defaultName},
		{component: sslDefaultECDSACertificateComponent, cell: "ecdsa", name: ecdsaName},
	}
	for _, test := range tests {
		entryPoint := helpers.IncrementalBindingsEntryPointName(test.component)
		encoded, err := planner.RenderIncrementalBindings(
			t.Context(),
			entryPoint,
			bindingContext,
		)
		require.NoError(t, err)
		snapshotEncoded, err := snapshotPlanner.RenderIncrementalBindingsSnapshot(
			t.Context(), entryPoint, snapshot,
		)
		require.NoError(t, err)
		var expected []byte
		var expectedProps []byte
		if test.name == "" {
			expected = []byte("{}")
		} else {
			props := map[string]any{
				"cell": test.cell,
				"key":  "haptic/" + test.name,
				"keys": []string{"haptic", test.name},
			}
			expected, err = json.Marshal(map[string]any{"secrets": props})
			require.NoError(t, err)
			expectedProps, err = json.Marshal(props)
			require.NoError(t, err)
		}
		assert.Equal(t, expected, encoded, test.component)
		assert.Equal(t, expected, snapshotEncoded, test.component)
		bindings := plan.byComponent[test.component]
		if test.name == "" {
			assert.Empty(t, bindings, test.component)
			continue
		}
		require.Len(t, bindings, 1, test.component)
		assert.Equal(t, "secrets", bindings[0].source, test.component)
		assert.Equal(t, expectedProps, bindings[0].props, test.component)
		projection, projectionErr := incrementalResourceProjectionForBinding(bindings[0])
		require.NoError(t, projectionErr)
		assert.Equal(t, []string{"haptic", test.name}, projection.Keys, test.component)
	}
}

func sslDefaultCertificateSecret(
	name, certificate, key string,
	labels map[string]any,
) map[string]any {
	metadata := map[string]any{"namespace": "haptic", "name": name}
	if labels != nil {
		metadata["labels"] = labels
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret", "metadata": metadata,
		"data": map[string]any{
			"tls.crt": base64.StdEncoding.EncodeToString([]byte(certificate)),
			"tls.key": base64.StdEncoding.EncodeToString([]byte(key)),
		},
	}
}

func (f *sslDefaultCertificateFixture) addSecret(t *testing.T, resource map[string]any) {
	t.Helper()
	metadata := resource["metadata"].(map[string]any)
	name := metadata["name"].(string)
	require.NoError(t, f.secrets.Add(resource, []string{"haptic", name}))
}

func (f *sslDefaultCertificateFixture) updateSecret(t *testing.T, resource map[string]any) {
	t.Helper()
	metadata := resource["metadata"].(map[string]any)
	name := metadata["name"].(string)
	require.NoError(t, f.secrets.Update(resource, []string{"haptic", name}))
}

func (f *sslDefaultCertificateFixture) renderAndCommit(t *testing.T) *RenderResult {
	t.Helper()
	result, err := f.service.Render(t.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}

func (f *sslDefaultCertificateFixture) assertExecutions(
	t *testing.T,
	componentName string,
	want uint64,
) {
	t.Helper()
	assert.Equal(t, want, f.counters(t, componentName).Executions, componentName)
}

func (f *sslDefaultCertificateFixture) assertBackdates(
	t *testing.T,
	componentName string,
	want uint64,
) {
	t.Helper()
	assert.Equal(t, want, f.counters(t, componentName).Backdates, componentName)
}

func (f *sslDefaultCertificateFixture) counters(
	t *testing.T,
	componentName string,
) incremental.NodeCounters {
	t.Helper()
	f.service.incremental.mu.Lock()
	component, exists := f.service.incremental.components[componentName]
	require.True(t, exists)
	props, found := f.service.incremental.snapshot.bindings.Get(bindingKey(componentName, "secrets"))
	f.service.incremental.mu.Unlock()
	require.True(t, found)
	projection, err := decodeIncrementalResourceProjection([]byte(props))
	require.NoError(t, err)
	namespace, name, ok := incrementalResourceProjectionIdentity(projection)
	require.True(t, ok)
	query := componentQueryKey(&component, "secrets", namespace, name)
	return f.service.incremental.graph.Counters(query)
}

func sslDefaultCertificateContent(t *testing.T, result *RenderResult) string {
	t.Helper()
	const name = "default.pem"
	for _, certificate := range requireAuxiliaryFiles(t, result).SSLCertificates {
		if certificate.GetIdentifier() == name || strings.HasSuffix(certificate.GetIdentifier(), "/"+name) {
			return certificate.GetContent()
		}
	}
	require.FailNow(t, "SSL certificate not found", name)
	return ""
}

func sslDefaultCertificateSnapshot(t *testing.T, result *RenderResult) string {
	t.Helper()
	return result.HAProxyConfig + "\x00" + sslDefaultCertificateContent(t, result)
}
