package conversion

import (
	"cmp"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
)

func TestConvertSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    v1alpha1.HAProxyTemplateConfigSpec
		want    *config.Config
		wantErr bool
	}{
		{
			name: "minimal valid config",
			spec: v1alpha1.HAProxyTemplateConfigSpec{
				CredentialsSecretRef: v1alpha1.SecretReference{
					Name: "haproxy-creds",
				},
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{
						"app": "haproxy",
					},
				},
				HAProxyConfig: v1alpha1.HAProxyConfig{
					Template: "global\n  daemon",
				},
			},
			want: &config.Config{
				PodSelector: config.PodSelector{
					MatchLabels: map[string]string{
						"app": "haproxy",
					},
				},
				Controller: config.ControllerConfig{
					LeaderElection: config.LeaderElectionConfig{
						Enabled: true, // Default when not specified
					},
					ConfigPublishing: config.ConfigPublishingConfig{
						CompressionThreshold: config.DefaultCompressionThreshold, // Default when not set
					},
				},
				HAProxyConfig: config.HAProxyConfig{
					Template: "global\n  daemon",
				},
				WatchedResources: map[string]config.WatchedResource{},
				TemplateSnippets: map[string]config.TemplateSnippet{},
				Maps:             map[string]config.MapFile{},
				Files:            map[string]config.GeneralFile{},
				SSLCertificates:  map[string]config.SSLCertificate{},
			},
			wantErr: false,
		},
		{
			name: "complete config with all sections",
			spec: v1alpha1.HAProxyTemplateConfigSpec{
				CredentialsSecretRef: v1alpha1.SecretReference{
					Name:      "haproxy-creds",
					Namespace: "default",
				},
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{
						"app":       "haproxy",
						"component": "loadbalancer",
					},
				},
				Controller: v1alpha1.ControllerConfig{
					LeaderElection: v1alpha1.LeaderElectionConfig{
						Enabled:       new(true),
						LeaseName:     "haproxy-leader",
						LeaseDuration: "15s",
						RenewDeadline: "10s",
						RetryPeriod:   "2s",
					},
				},
				Logging: v1alpha1.LoggingConfig{
					Level: "INFO",
				},
				Dataplane: v1alpha1.DataplaneConfig{
					Port:                      5555,
					MinDeploymentInterval:     "2s",
					DriftPreventionInterval:   "5m",
					ConfigPublishInterval:     "45s",
					ReloadVerificationTimeout: "20s",
					SyncTimeout:               "3m",
				},
				WatchedResources: map[string]v1alpha1.WatchedResource{
					"ingresses": {
						APIVersion:    "networking.k8s.io/v1",
						Resources:     "ingresses",
						LabelSelector: "app=nginx,env=prod",
						IndexBy:       []string{"metadata.namespace", "metadata.name"},
					},
				},
				TemplateSnippets: map[string]v1alpha1.TemplateSnippet{
					"common_defaults": {
						Template: "timeout connect 5s",
					},
				},
				Maps: map[string]v1alpha1.MapFile{
					"backend_map": {
						Template: "{{ range .ingresses }}{{ .metadata.name }} backend_{{ .metadata.name }}\n{{ end }}",
					},
				},
				Files: map[string]v1alpha1.GeneralFile{
					"error_page": {
						Template: "<html><body>Error</body></html>",
					},
				},
				SSLCertificates: map[string]v1alpha1.SSLCertificate{
					"default_cert": {
						Template: "{{ .secret.data.cert }}",
					},
				},
				HAProxyConfig: v1alpha1.HAProxyConfig{
					Template: "global\n  daemon\n\ndefaults\n  {% include 'common_defaults' %}",
				},
			},
			want: &config.Config{
				PodSelector: config.PodSelector{
					MatchLabels: map[string]string{
						"app":       "haproxy",
						"component": "loadbalancer",
					},
				},
				Controller: config.ControllerConfig{
					LeaderElection: config.LeaderElectionConfig{
						Enabled:       true,
						LeaseName:     "haproxy-leader",
						LeaseDuration: "15s",
						RenewDeadline: "10s",
						RetryPeriod:   "2s",
					},
					ConfigPublishing: config.ConfigPublishingConfig{
						CompressionThreshold: config.DefaultCompressionThreshold, // Default when not set
					},
				},
				Logging: config.LoggingConfig{
					Level: "INFO",
				},
				Dataplane: config.DataplaneConfig{
					Port:                      5555,
					MinDeploymentInterval:     "2s",
					DriftPreventionInterval:   "5m",
					ConfigPublishInterval:     "45s",
					ReloadVerificationTimeout: "20s",
					SyncTimeout:               "3m",
				},
				WatchedResources: map[string]config.WatchedResource{
					"ingresses": {
						APIVersion: "networking.k8s.io/v1",
						Resources:  "ingresses",
						LabelSelector: map[string]string{
							"app": "nginx",
							"env": "prod",
						},
						IndexBy: []string{"metadata.namespace", "metadata.name"},
					},
				},
				TemplateSnippets: map[string]config.TemplateSnippet{
					"common_defaults": {
						Name:     "common_defaults",
						Template: "timeout connect 5s",
					},
				},
				Maps: map[string]config.MapFile{
					"backend_map": {
						Template: "{{ range .ingresses }}{{ .metadata.name }} backend_{{ .metadata.name }}\n{{ end }}",
					},
				},
				Files: map[string]config.GeneralFile{
					"error_page": {
						Template: "<html><body>Error</body></html>",
					},
				},
				SSLCertificates: map[string]config.SSLCertificate{
					"default_cert": {
						Template: "{{ .secret.data.cert }}",
					},
				},
				HAProxyConfig: config.HAProxyConfig{
					Template: "global\n  daemon\n\ndefaults\n  {% include 'common_defaults' %}",
				},
			},
			wantErr: false,
		},
		{
			name: "leader election disabled",
			spec: v1alpha1.HAProxyTemplateConfigSpec{
				CredentialsSecretRef: v1alpha1.SecretReference{
					Name: "haproxy-creds",
				},
				PodSelector: v1alpha1.PodSelector{
					MatchLabels: map[string]string{"app": "haproxy"},
				},
				Controller: v1alpha1.ControllerConfig{
					LeaderElection: v1alpha1.LeaderElectionConfig{
						Enabled: new(false),
					},
				},
				HAProxyConfig: v1alpha1.HAProxyConfig{
					Template: "global\n  daemon",
				},
			},
			want: &config.Config{
				PodSelector: config.PodSelector{
					MatchLabels: map[string]string{"app": "haproxy"},
				},
				Controller: config.ControllerConfig{
					LeaderElection: config.LeaderElectionConfig{
						Enabled: false,
					},
					ConfigPublishing: config.ConfigPublishingConfig{
						CompressionThreshold: config.DefaultCompressionThreshold, // Default when not set
					},
				},
				HAProxyConfig: config.HAProxyConfig{
					Template: "global\n  daemon",
				},
				WatchedResources: map[string]config.WatchedResource{},
				TemplateSnippets: map[string]config.TemplateSnippet{},
				Maps:             map[string]config.MapFile{},
				Files:            map[string]config.GeneralFile{},
				SSLCertificates:  map[string]config.SSLCertificate{},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConvertSpec(&tt.spec)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want.PodSelector, got.PodSelector)
			assert.Equal(t, tt.want.Controller, got.Controller)
			assert.Equal(t, tt.want.Logging, got.Logging)
			wantDataplane := tt.want.Dataplane
			wantDataplane.SecretsDir = cmp.Or(wantDataplane.SecretsDir, config.DefaultDataplaneSecretsDir)
			assert.Equal(t, wantDataplane, got.Dataplane)
			assert.Equal(t, tt.want.WatchedResources, got.WatchedResources)
			assert.Equal(t, tt.want.TemplateSnippets, got.TemplateSnippets)
			assert.Equal(t, tt.want.Maps, got.Maps)
			assert.Equal(t, tt.want.Files, got.Files)
			assert.Equal(t, tt.want.SSLCertificates, got.SSLCertificates)
			assert.Equal(t, tt.want.HAProxyConfig, got.HAProxyConfig)
		})
	}
}

func TestConvertSpecIncrementalBindingsTemplate(t *testing.T) {
	spec := &v1alpha1.HAProxyTemplateConfigSpec{
		TemplateSnippets: map[string]v1alpha1.TemplateSnippet{
			"dynamic": {
				Template: "{{ source }}",
				Incremental: &v1alpha1.IncrementalTemplate{
					BindingsTemplate:  `{{ toJSON(extraContext) }}`,
					WhenAnyPathExists: []string{`metadata.annotations['example.test/enabled']`},
					Root:              "features-root",
					Group:             "features",
					Consumes:          []string{"policies"},
					OptionalConsumes:  []string{"optional-policies"},
					Effects: []v1alpha1.IncrementalEffect{
						v1alpha1.IncrementalEffectRecordEvent,
						v1alpha1.IncrementalEffectPublishValue,
						v1alpha1.IncrementalEffectStatusPatch,
					},
				},
			},
		},
	}

	converted, err := ConvertSpec(spec)
	require.NoError(t, err)
	require.NotNil(t, converted.TemplateSnippets["dynamic"].Incremental)
	assert.Equal(t, &config.IncrementalTemplate{
		BindingsTemplate:  `{{ toJSON(extraContext) }}`,
		WhenAnyPathExists: []string{`metadata.annotations['example.test/enabled']`},
		Root:              "features-root",
		Group:             "features",
		Consumes:          []string{"policies"},
		OptionalConsumes:  []string{"optional-policies"},
		Effects: []config.IncrementalEffect{
			config.IncrementalEffectRecordEvent,
			config.IncrementalEffectPublishValue,
			config.IncrementalEffectStatusPatch,
		},
	}, converted.TemplateSnippets["dynamic"].Incremental)

	spec.TemplateSnippets["dynamic"].Incremental.Effects[0] = v1alpha1.IncrementalEffectRecordEvent
	spec.TemplateSnippets["dynamic"].Incremental.Consumes[0] = "mutated"
	spec.TemplateSnippets["dynamic"].Incremental.OptionalConsumes[0] = "mutated"
	spec.TemplateSnippets["dynamic"].Incremental.WhenAnyPathExists[0] = "mutated"
	assert.Equal(t, config.IncrementalEffectRecordEvent, converted.TemplateSnippets["dynamic"].Incremental.Effects[0])
	assert.Equal(t, []string{"policies"}, converted.TemplateSnippets["dynamic"].Incremental.Consumes)
	assert.Equal(t, []string{"optional-policies"}, converted.TemplateSnippets["dynamic"].Incremental.OptionalConsumes)
	assert.Equal(t, []string{`metadata.annotations['example.test/enabled']`},
		converted.TemplateSnippets["dynamic"].Incremental.WhenAnyPathExists)
}

func TestConvertSpecIncrementalResourceProjection(t *testing.T) {
	spec := &v1alpha1.HAProxyTemplateConfigSpec{
		TemplateSnippets: map[string]v1alpha1.TemplateSnippet{
			"projection": {
				Template: `{{- "" -}}`,
				Incremental: &v1alpha1.IncrementalTemplate{
					Mode:             v1alpha1.IncrementalModeResourceProjection,
					BindingsTemplate: `{{ toJSON(extraContext) }}`,
					Group:            "selected-resources",
					Effects:          []v1alpha1.IncrementalEffect{v1alpha1.IncrementalEffectPublishValue},
				},
			},
		},
	}

	converted, err := ConvertSpec(spec)
	require.NoError(t, err)
	assert.Equal(t, &config.IncrementalTemplate{
		Mode:             config.IncrementalModeResourceProjection,
		BindingsTemplate: `{{ toJSON(extraContext) }}`,
		Group:            "selected-resources",
		Effects:          []config.IncrementalEffect{config.IncrementalEffectPublishValue},
	}, converted.TemplateSnippets["projection"].Incremental)
}

func TestParseLabelSelector(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		want     map[string]string
		wantErr  bool
	}{
		{
			// An empty selector never reaches here — ConvertSpec treats it as
			// "unfiltered" and skips the parse. Reaching it means a caller lost
			// that guard, which must not silently produce an empty selector.
			name:     "empty string is a caller error",
			selector: "",
			wantErr:  true,
		},
		{
			name:     "single label",
			selector: "app=nginx",
			want: map[string]string{
				"app": "nginx",
			},
		},
		{
			name:     "multiple labels",
			selector: "app=nginx,env=prod,version=v1",
			want: map[string]string{
				"app":     "nginx",
				"env":     "prod",
				"version": "v1",
			},
		},
		{
			name:     "labels with spaces",
			selector: "app = nginx , env = prod",
			want: map[string]string{
				"app": "nginx",
				"env": "prod",
			},
		},
		{
			name:     "label with empty value",
			selector: "app=nginx,env=",
			want: map[string]string{
				"app": "nginx",
				"env": "",
			},
		},
		{
			name:     "kubernetes style labels",
			selector: "app.kubernetes.io/name=haproxy,app.kubernetes.io/component=loadbalancer",
			want: map[string]string{
				"app.kubernetes.io/name":      "haproxy",
				"app.kubernetes.io/component": "loadbalancer",
			},
		},
		{
			// Double-equals is valid equality syntax. The old split-on-"="
			// parser turned this into {"app": "=nginx"}.
			name:     "double equals",
			selector: "app==nginx",
			want: map[string]string{
				"app": "nginx",
			},
		},
		{
			name:     "trailing comma",
			selector: "app=nginx,env=prod,",
			wantErr:  true,
		},
		{
			// A bare key is an Exists requirement, not an equality pair. The
			// old parser dropped it and watched with the remainder.
			name:     "malformed - no equals",
			selector: "app,env=prod",
			wantErr:  true,
		},
		{
			// The strongest regression guard: the old parser produced the
			// key "app!", which then failed conversion at list time and left
			// the watch unfiltered across the whole cluster.
			name:     "not-equals is rejected",
			selector: "app!=nginx",
			wantErr:  true,
		},
		{
			name:     "set-based in",
			selector: "tier in (frontend,api)",
			wantErr:  true,
		},
		{
			name:     "set-based notin",
			selector: "tier notin (backend)",
			wantErr:  true,
		},
		{
			name:     "set-based exists negation",
			selector: "!disabled",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLabelSelector(tt.selector)
			if tt.wantErr {
				require.Error(t, err, "selector %q must be rejected, not silently narrowed", tt.selector)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// An absent selector means "unfiltered" and must convert cleanly to nil.
func TestConvertSpec_EmptyLabelSelectorStaysNil(t *testing.T) {
	spec := &v1alpha1.HAProxyTemplateConfigSpec{
		WatchedResources: map[string]v1alpha1.WatchedResource{
			"ingresses": {
				APIVersion: "networking.k8s.io/v1",
				Resources:  "ingresses",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
	}

	cfg, err := ConvertSpec(spec)

	require.NoError(t, err)
	assert.Nil(t, cfg.WatchedResources["ingresses"].LabelSelector)
}

// A rejected selector must fail the whole conversion, naming the watched
// resource so the operator knows which entry to fix.
func TestConvertSpec_RejectsSetBasedLabelSelector(t *testing.T) {
	spec := &v1alpha1.HAProxyTemplateConfigSpec{
		WatchedResources: map[string]v1alpha1.WatchedResource{
			"ingresses": {
				APIVersion:    "networking.k8s.io/v1",
				Resources:     "ingresses",
				IndexBy:       []string{"metadata.namespace", "metadata.name"},
				LabelSelector: "tier in (frontend,api)",
			},
		},
	}

	_, err := ConvertSpec(spec)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "watchedResources[ingresses].labelSelector")
	assert.Contains(t, err.Error(), "tier in (frontend,api)")
}

func TestParseCRD(t *testing.T) {
	tests := []struct {
		name    string
		crd     *unstructured.Unstructured
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid HAProxyTemplateConfig CRD",
			crd: &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "haproxy-haptic.org/v1alpha1",
					"kind":       "HAProxyTemplateConfig",
					"metadata": map[string]any{
						"name":      "test-config",
						"namespace": "default",
					},
					"spec": map[string]any{
						"credentialsSecretRef": map[string]any{
							"name": "haproxy-creds",
						},
						"podSelector": map[string]any{
							"matchLabels": map[string]any{
								"app": "haproxy",
							},
						},
						"haproxyConfig": map[string]any{
							"template": "global\n  daemon",
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid kind",
			crd: &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "haproxy-haptic.org/v1alpha1",
					"kind":       "Deployment",
					"metadata": map[string]any{
						"name":      "test-config",
						"namespace": "default",
					},
				},
			},
			wantErr: true,
			errMsg:  "expected HAProxyTemplateConfig, got Deployment",
		},
		{
			name: "invalid apiVersion",
			crd: &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "apps/v1",
					"kind":       "HAProxyTemplateConfig",
					"metadata": map[string]any{
						"name":      "test-config",
						"namespace": "default",
					},
				},
			},
			wantErr: true,
			errMsg:  "expected apiVersion haproxy-haptic.org/v1alpha1, got apps/v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, crd, err := ParseCRD(tt.crd)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				assert.Nil(t, cfg)
				assert.Nil(t, crd)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, cfg)
			require.NotNil(t, crd)
			assert.Equal(t, "test-config", crd.Name)
		})
	}
}

func TestConvertSpec_WithValidationTests(t *testing.T) {
	// Create fixture JSON
	ingressJSON, err := json.Marshal(map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata": map[string]any{
			"name":      "test-ingress",
			"namespace": "default",
		},
	})
	require.NoError(t, err)

	spec := v1alpha1.HAProxyTemplateConfigSpec{
		CredentialsSecretRef: v1alpha1.SecretReference{
			Name: "haproxy-creds",
		},
		PodSelector: v1alpha1.PodSelector{
			MatchLabels: map[string]string{"app": "haproxy"},
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: "global\n  daemon",
		},
		ValidationTests: map[string]v1alpha1.ValidationTest{
			"test-basic": {
				Description: "Basic test",
				Fixtures: map[string][]runtime.RawExtension{
					"ingresses": {
						{Raw: ingressJSON},
					},
				},
				HTTPResources: []v1alpha1.HTTPResourceFixture{
					{URL: "http://example.com/data", Content: `{"key": "value"}`},
				},
				Assertions: []v1alpha1.ValidationAssertion{
					{
						Type:        "contains",
						Description: "Check backend exists",
						Target:      "haproxy_config",
						Pattern:     "backend test-ingress",
					},
					{
						Type:        "jsonpath",
						Description: "Check JSON value",
						Target:      "http://example.com/data",
						JSONPath:    "$.key",
						Expected:    "value",
					},
					{
						Type:        "not_contains",
						Description: "Check no errors",
						Target:      "haproxy_config",
						Patterns:    []string{"error", "warning"},
					},
				},
			},
		},
	}

	cfg, err := ConvertSpec(&spec)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Check validation tests were converted
	require.Contains(t, cfg.ValidationTests, "test-basic")
	test := cfg.ValidationTests["test-basic"]

	assert.Equal(t, "Basic test", test.Description)

	// Check fixtures were converted
	require.Contains(t, test.Fixtures, "ingresses")
	require.Len(t, test.Fixtures["ingresses"], 1)

	// Check HTTP fixtures were converted
	require.Len(t, test.HTTPFixtures, 1)
	assert.Equal(t, "http://example.com/data", test.HTTPFixtures[0].URL)
	assert.Equal(t, `{"key": "value"}`, test.HTTPFixtures[0].Content)

	// Check assertions were converted
	require.Len(t, test.Assertions, 3)
	assert.Equal(t, "contains", test.Assertions[0].Type)
	assert.Equal(t, "Check backend exists", test.Assertions[0].Description)
	assert.Equal(t, "haproxy_config", test.Assertions[0].Target)
	assert.Equal(t, "backend test-ingress", test.Assertions[0].Pattern)

	assert.Equal(t, "jsonpath", test.Assertions[1].Type)
	assert.Equal(t, "$.key", test.Assertions[1].JSONPath)
	assert.Equal(t, "value", test.Assertions[1].Expected)

	assert.Equal(t, "not_contains", test.Assertions[2].Type)
	assert.Equal(t, []string{"error", "warning"}, test.Assertions[2].Patterns)
}

func TestConvertSpec_WithPostProcessors(t *testing.T) {
	spec := v1alpha1.HAProxyTemplateConfigSpec{
		CredentialsSecretRef: v1alpha1.SecretReference{
			Name: "haproxy-creds",
		},
		PodSelector: v1alpha1.PodSelector{
			MatchLabels: map[string]string{"app": "haproxy"},
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: "global\n  daemon",
			PostProcessing: []v1alpha1.PostProcessorConfig{
				{Type: "sort_acls", Params: map[string]string{"order": "asc"}},
				{Type: "deduplicate"},
			},
		},
		Maps: map[string]v1alpha1.MapFile{
			"hosts": {
				Template: "{{ range .ingresses }}{{ .spec.host }} backend\n{{ end }}",
				PostProcessing: []v1alpha1.PostProcessorConfig{
					{Type: "sort_lines"},
				},
			},
		},
		Files: map[string]v1alpha1.GeneralFile{
			"error": {
				Template: "<html>Error</html>",
				PostProcessing: []v1alpha1.PostProcessorConfig{
					{Type: "minify_html"},
				},
			},
		},
		SSLCertificates: map[string]v1alpha1.SSLCertificate{
			"default": {
				Template: "{{ .cert }}{{ .key }}",
				PostProcessing: []v1alpha1.PostProcessorConfig{
					{Type: "pem_format"},
				},
			},
		},
	}

	cfg, err := ConvertSpec(&spec)
	require.NoError(t, err)

	// Check HAProxy config post processors
	require.Len(t, cfg.HAProxyConfig.PostProcessing, 2)
	assert.Equal(t, "sort_acls", cfg.HAProxyConfig.PostProcessing[0].Type)
	assert.Equal(t, map[string]string{"order": "asc"}, cfg.HAProxyConfig.PostProcessing[0].Params)
	assert.Equal(t, "deduplicate", cfg.HAProxyConfig.PostProcessing[1].Type)

	// Check Maps post processors
	require.Contains(t, cfg.Maps, "hosts")
	require.Len(t, cfg.Maps["hosts"].PostProcessing, 1)
	assert.Equal(t, "sort_lines", cfg.Maps["hosts"].PostProcessing[0].Type)

	// Check Files post processors
	require.Contains(t, cfg.Files, "error")
	require.Len(t, cfg.Files["error"].PostProcessing, 1)
	assert.Equal(t, "minify_html", cfg.Files["error"].PostProcessing[0].Type)

	// Check SSL certificates post processors
	require.Contains(t, cfg.SSLCertificates, "default")
	require.Len(t, cfg.SSLCertificates["default"].PostProcessing, 1)
	assert.Equal(t, "pem_format", cfg.SSLCertificates["default"].PostProcessing[0].Type)
}

// reloadOnPush decides whether a file push reloads HAProxy. Losing it in the
// CRD → internal-config hop would silently reinstate the reload for every
// sidecar-owned file, so pin both the explicit false and the absent default.
func TestConvertSpec_FileReloadOnPush(t *testing.T) {
	noReload := false
	spec := v1alpha1.HAProxyTemplateConfigSpec{
		CredentialsSecretRef: v1alpha1.SecretReference{Name: "haproxy-creds"},
		PodSelector:          v1alpha1.PodSelector{MatchLabels: map[string]string{"app": "haproxy"}},
		HAProxyConfig:        v1alpha1.HAProxyConfig{Template: "global"},
		Files: map[string]v1alpha1.GeneralFile{
			"vector.yaml": {Template: "sources: {}", ReloadOnPush: &noReload},
			"503.http":    {Template: "HTTP/1.0 503"},
		},
	}

	cfg, err := ConvertSpec(&spec)
	require.NoError(t, err)
	require.NotNil(t, cfg.Files["vector.yaml"].ReloadOnPush)
	assert.False(t, *cfg.Files["vector.yaml"].ReloadOnPush)
	assert.Nil(t, cfg.Files["503.http"].ReloadOnPush, "absent means the default (reload), not an explicit false")
}

func TestConvertSpec_ExtraContextError(t *testing.T) {
	spec := v1alpha1.HAProxyTemplateConfigSpec{
		CredentialsSecretRef: v1alpha1.SecretReference{
			Name: "haproxy-creds",
		},
		PodSelector: v1alpha1.PodSelector{
			MatchLabels: map[string]string{"app": "haproxy"},
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: "global",
		},
		TemplatingSettings: v1alpha1.TemplatingSettings{
			ExtraContext: runtime.RawExtension{
				Raw: []byte("invalid json{"),
			},
		},
	}

	cfg, err := ConvertSpec(&spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshalling templating_settings.extra_context")
	assert.Nil(t, cfg)
}

func TestConvertSpec_WithExtraContext(t *testing.T) {
	extraContextJSON, err := json.Marshal(map[string]any{
		"environment": "production",
		"maxConns":    1000,
		"servers": []string{
			"server1.example.com",
			"server2.example.com",
		},
	})
	require.NoError(t, err)

	spec := v1alpha1.HAProxyTemplateConfigSpec{
		CredentialsSecretRef: v1alpha1.SecretReference{
			Name: "haproxy-creds",
		},
		PodSelector: v1alpha1.PodSelector{
			MatchLabels: map[string]string{"app": "haproxy"},
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: "global",
		},
		TemplatingSettings: v1alpha1.TemplatingSettings{
			ExtraContext: runtime.RawExtension{
				Raw: extraContextJSON,
			},
		},
	}

	cfg, err := ConvertSpec(&spec)
	require.NoError(t, err)

	assert.Equal(t, "production", cfg.TemplatingSettings.ExtraContext["environment"])
	assert.Equal(t, float64(1000), cfg.TemplatingSettings.ExtraContext["maxConns"]) // JSON numbers are float64
	assert.Len(t, cfg.TemplatingSettings.ExtraContext["servers"], 2)
}

func TestConvertFixtures(t *testing.T) {
	t.Run("empty fixtures", func(t *testing.T) {
		fixtures := convertFixtures(nil)
		assert.Empty(t, fixtures)
	})

	t.Run("valid fixtures", func(t *testing.T) {
		ingressJSON, err := json.Marshal(map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "Ingress",
			"metadata": map[string]any{
				"name": "my-ingress",
			},
		})
		require.NoError(t, err)

		serviceJSON, err := json.Marshal(map[string]any{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata": map[string]any{
				"name": "my-service",
			},
		})
		require.NoError(t, err)

		crdFixtures := map[string][]runtime.RawExtension{
			"ingresses": {{Raw: ingressJSON}},
			"services":  {{Raw: serviceJSON}},
		}

		fixtures := convertFixtures(crdFixtures)
		require.Len(t, fixtures, 2)
		require.Len(t, fixtures["ingresses"], 1)
		require.Len(t, fixtures["services"], 1)

		// Check ingress was parsed correctly
		ingress := fixtures["ingresses"][0].(map[string]any)
		assert.Equal(t, "Ingress", ingress["kind"])
	})

	t.Run("invalid JSON falls back to empty object", func(t *testing.T) {
		crdFixtures := map[string][]runtime.RawExtension{
			"invalid": {{Raw: []byte("not valid json")}},
		}

		fixtures := convertFixtures(crdFixtures)
		require.Len(t, fixtures, 1)
		require.Len(t, fixtures["invalid"], 1)

		// Should be empty map due to parse error
		obj := fixtures["invalid"][0].(map[string]any)
		assert.Empty(t, obj)
	})
}

func TestConvertPostProcessors(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		result := convertPostProcessors(nil)
		assert.Nil(t, result)
	})

	t.Run("empty slice", func(t *testing.T) {
		result := convertPostProcessors([]v1alpha1.PostProcessorConfig{})
		assert.Nil(t, result)
	})

	t.Run("with processors", func(t *testing.T) {
		input := []v1alpha1.PostProcessorConfig{
			{Type: "sort_acls", Params: map[string]string{"order": "desc"}},
			{Type: "deduplicate"},
		}

		result := convertPostProcessors(input)
		require.Len(t, result, 2)
		assert.Equal(t, "sort_acls", result[0].Type)
		assert.Equal(t, map[string]string{"order": "desc"}, result[0].Params)
		assert.Equal(t, "deduplicate", result[1].Type)
	})
}

func TestConvertAssertions(t *testing.T) {
	t.Run("empty assertions", func(t *testing.T) {
		result := convertAssertions(nil)
		assert.Empty(t, result)
	})

	t.Run("various assertion types", func(t *testing.T) {
		input := []v1alpha1.ValidationAssertion{
			{
				Type:        "contains",
				Description: "Check content",
				Target:      "haproxy_config",
				Pattern:     "backend",
			},
			{
				Type:        "jsonpath",
				Description: "Check JSON",
				Target:      "response",
				JSONPath:    "$.status",
				Expected:    "ok",
			},
			{
				Type:        "not_contains",
				Description: "No errors",
				Target:      "output",
				Patterns:    []string{"error", "fail"},
			},
		}

		result := convertAssertions(input)
		require.Len(t, result, 3)

		assert.Equal(t, "contains", result[0].Type)
		assert.Equal(t, "Check content", result[0].Description)
		assert.Equal(t, "haproxy_config", result[0].Target)
		assert.Equal(t, "backend", result[0].Pattern)

		assert.Equal(t, "jsonpath", result[1].Type)
		assert.Equal(t, "$.status", result[1].JSONPath)
		assert.Equal(t, "ok", result[1].Expected)

		assert.Equal(t, "not_contains", result[2].Type)
		assert.Equal(t, []string{"error", "fail"}, result[2].Patterns)
	})
}

func TestConvertHTTPFixtures(t *testing.T) {
	t.Run("empty fixtures", func(t *testing.T) {
		result := convertHTTPFixtures(nil)
		assert.Nil(t, result)
	})

	t.Run("empty slice", func(t *testing.T) {
		result := convertHTTPFixtures([]v1alpha1.HTTPResourceFixture{})
		assert.Nil(t, result)
	})

	t.Run("with fixtures", func(t *testing.T) {
		input := []v1alpha1.HTTPResourceFixture{
			{URL: "http://api.example.com/users", Content: `[{"id": 1}]`},
			{URL: "http://api.example.com/config", Content: `{"enabled": true}`},
		}

		result := convertHTTPFixtures(input)
		require.Len(t, result, 2)

		assert.Equal(t, "http://api.example.com/users", result[0].URL)
		assert.Equal(t, `[{"id": 1}]`, result[0].Content)

		assert.Equal(t, "http://api.example.com/config", result[1].URL)
		assert.Equal(t, `{"enabled": true}`, result[1].Content)
	})
}

func TestConvertSpec_WithWatchedResourcesIgnoreFields(t *testing.T) {
	spec := v1alpha1.HAProxyTemplateConfigSpec{
		CredentialsSecretRef: v1alpha1.SecretReference{
			Name: "haproxy-creds",
		},
		PodSelector: v1alpha1.PodSelector{
			MatchLabels: map[string]string{"app": "haproxy"},
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: "global",
		},
		WatchedResourcesIgnoreFields: []string{
			"metadata.managedFields",
			"metadata.annotations['kubectl.kubernetes.io/last-applied-configuration']",
		},
	}

	cfg, err := ConvertSpec(&spec)
	require.NoError(t, err)

	require.Len(t, cfg.WatchedResourcesIgnoreFields, 2)
	assert.Contains(t, cfg.WatchedResourcesIgnoreFields, "metadata.managedFields")
}

func TestConvertSpec_WithWatchedResourceStore(t *testing.T) {
	spec := v1alpha1.HAProxyTemplateConfigSpec{
		CredentialsSecretRef: v1alpha1.SecretReference{
			Name: "haproxy-creds",
		},
		PodSelector: v1alpha1.PodSelector{
			MatchLabels: map[string]string{"app": "haproxy"},
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: "global",
		},
		WatchedResources: map[string]v1alpha1.WatchedResource{
			"secrets": {
				APIVersion:              "v1",
				Resources:               "secrets",
				EnableValidationWebhook: true,
				// "on-demand" is the only non-default valid Store value (CRD enum is
				// "full" / "on-demand"); see pkg/apis/.../types_config.go for the
				// kubebuilder validation tag and pkg/controller/resourcewatcher.determineStoreType
				// for the runtime mapping.
				Store: "on-demand",
			},
		},
	}

	cfg, err := ConvertSpec(&spec)
	require.NoError(t, err)

	require.Contains(t, cfg.WatchedResources, "secrets")
	assert.Equal(t, "on-demand", cfg.WatchedResources["secrets"].Store)
	assert.True(t, cfg.WatchedResources["secrets"].EnableValidationWebhook)
}

func TestConvertSpec_WithDataplaneDirectories(t *testing.T) {
	spec := v1alpha1.HAProxyTemplateConfigSpec{
		CredentialsSecretRef: v1alpha1.SecretReference{
			Name: "haproxy-creds",
		},
		PodSelector: v1alpha1.PodSelector{
			MatchLabels: map[string]string{"app": "haproxy"},
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{
			Template: "global",
		},
		Dataplane: v1alpha1.DataplaneConfig{
			Port:              5555,
			MapsDir:           "/etc/haproxy/maps",
			SSLCertsDir:       "/etc/haproxy/certs",
			GeneralStorageDir: "/etc/haproxy/storage",
			ConfigFile:        "/etc/haproxy/haproxy.cfg",
		},
	}

	cfg, err := ConvertSpec(&spec)
	require.NoError(t, err)

	assert.Equal(t, "/etc/haproxy/maps", cfg.Dataplane.MapsDir)
	assert.Equal(t, "/etc/haproxy/certs", cfg.Dataplane.SSLCertsDir)
	assert.Equal(t, "/etc/haproxy/storage", cfg.Dataplane.GeneralStorageDir)
	assert.Equal(t, "/etc/haproxy/haproxy.cfg", cfg.Dataplane.ConfigFile)
	// Charts before the secret file class write no secretsDir; a controller
	// rolled forward under such a config must still start.
	assert.Equal(t, config.DefaultDataplaneSecretsDir, cfg.Dataplane.SecretsDir)

	spec.Dataplane.SecretsDir = "/srv/secrets"
	cfg, err = ConvertSpec(&spec)
	require.NoError(t, err)
	assert.Equal(t, "/srv/secrets", cfg.Dataplane.SecretsDir)
}

// TestConvertSpec_WithWatchedResourceDebounceInterval pins that the per-
// resource DebounceInterval string round-trips through ConvertSpec verbatim.
// The converter does NOT parse the value (parsing happens later in
// WatchedResource.GetDebounceInterval, exercised by the GetDebounceInterval
// test in pkg/core/config/getters_test.go), so this test only needs to prove
// the string makes it across.
//
// Without this regression test, a refactor that drops the field from the
// struct-literal copy in converter.go would silently revert per-resource
// debounce overrides to "always use the 5s default" without breaking any
// existing test.
func TestConvertSpec_WithWatchedResourceDebounceInterval(t *testing.T) {
	spec := &v1alpha1.HAProxyTemplateConfigSpec{
		PodSelector: v1alpha1.PodSelector{
			MatchLabels: map[string]string{"app": "haproxy"},
		},
		WatchedResources: map[string]v1alpha1.WatchedResource{
			"ingresses": {
				APIVersion:       "networking.k8s.io/v1",
				Resources:        "ingresses",
				DebounceInterval: "250ms",
			},
			"endpointslices": {
				APIVersion:       "discovery.k8s.io/v1",
				Resources:        "endpointslices",
				DebounceInterval: "30s",
			},
			// Default empty — converter should pass through unchanged.
			"services": {
				APIVersion: "v1",
				Resources:  "services",
			},
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{Template: "global\n"},
	}

	cfg, err := ConvertSpec(spec)
	require.NoError(t, err)

	require.Contains(t, cfg.WatchedResources, "ingresses")
	require.Contains(t, cfg.WatchedResources, "endpointslices")
	require.Contains(t, cfg.WatchedResources, "services")

	assert.Equal(t, "250ms", cfg.WatchedResources["ingresses"].DebounceInterval,
		"per-resource override must round-trip verbatim — converter must not parse or normalize it")
	assert.Equal(t, "30s", cfg.WatchedResources["endpointslices"].DebounceInterval,
		"each resource's DebounceInterval must be independent")
	assert.Empty(t, cfg.WatchedResources["services"].DebounceInterval,
		"empty DebounceInterval must round-trip as empty so the watcher's SetDefaults takes over")
}
