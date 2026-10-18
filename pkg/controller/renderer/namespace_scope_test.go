package renderer

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	controllernames "github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func TestRenderService_Render_LookupOutsideWatchedNamespacesFails(t *testing.T) {
	const scopeError = `secrets team-b/tls cannot be read: watchedResources.secrets.namespaces limits the secrets watch to team-a, haptic. Reference secrets in team-a, haptic, or change watchedResources.secrets.namespaces`
	tests := []struct {
		name       string
		namespaces []string
		template   string
		want       string
		wantErr    string
	}{
		{
			name:       "lookup inside the namespaces",
			namespaces: []string{"team-a", "haptic"},
			template:   `{% if resources.secrets.GetSingle("team-a", "tls") != nil %}found{% end %}`,
			want:       "found",
		},
		{
			name:       "missing object inside the namespaces is not an error",
			namespaces: []string{"team-a", "haptic"},
			template:   `{% if resources.secrets.GetSingle("team-a", "absent") == nil %}absent{% end %}`,
			want:       "absent",
		},
		{
			name:       "GetSingle outside the namespaces",
			namespaces: []string{"team-a", "haptic"},
			template:   `{% if resources.secrets.GetSingle("team-b", "tls") == nil %}absent{% end %}`,
			wantErr:    scopeError,
		},
		{
			name:       "Fetch outside the namespaces",
			namespaces: []string{"team-a", "haptic"},
			template:   `{{ len(resources.secrets.Fetch("team-b")) }}`,
			wantErr:    "secrets in namespace team-b cannot be read",
		},
		{
			name:     "unscoped watch reads every namespace",
			template: `{% if resources.secrets.GetSingle("team-b", "tls") != nil %}found{% end %}`,
			want:     "found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			indexBy := []string{"metadata.namespace", "metadata.name"}
			cfg := &config.Config{
				HAProxyConfig: config.HAProxyConfig{Template: tt.template},
				WatchedResources: map[string]config.WatchedResource{
					"secrets": {APIVersion: "v1", Resources: "secrets", IndexBy: indexBy, Namespaces: tt.namespaces},
				},
				Dataplane: testDataplaneConfig(),
			}
			engine, err := templating.New(
				map[string]string{controllernames.MainTemplateName: tt.template},
				&templating.Options{
					EntryPoints:  []string{controllernames.MainTemplateName},
					Declarations: typebootstrap.BuildEngineDeclarations(&typebootstrap.Result{}, "secrets"),
				},
			)
			require.NoError(t, err)
			secrets := store.NewMemoryStore(len(indexBy))
			for _, namespace := range []string{"team-a", "team-b"} {
				require.NoError(t, secrets.Add(map[string]any{
					"apiVersion": "v1", "kind": "Secret",
					"metadata": map[string]any{"namespace": namespace, "name": "tls"},
				}, []string{namespace, "tls"}))
			}
			svc := NewRenderService(&RenderServiceConfig{
				Engine:       engine,
				Config:       cfg,
				Logger:       slog.Default(),
				Capabilities: defaultCapabilities(),
			})

			result, err := svc.Render(t.Context(), &mockStoreProvider{storeMap: map[string]stores.Store{"secrets": secrets}}, rendercontext.RenderModeReconcile)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, result.HAProxyConfig, tt.want)
		})
	}
}

func TestRenderService_Render_IncrementalLookupOutsideWatchedNamespacesFails(t *testing.T) {
	indexBy := []string{"metadata.namespace", "metadata.name"}
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"routes":  {APIVersion: "example.io/v1", Resources: "routes", IndexBy: indexBy},
			"secrets": {APIVersion: "v1", Resources: "secrets", IndexBy: indexBy, Namespaces: []string{"team-a"}},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"route-lines": {
				Template:    `route={% if resources.secrets.GetSingle(dig(item, "metadata", "namespace"), "tls") != nil %}tls{% end %}`,
				Requires:    []string{"routes", "secrets"},
				Incremental: &config.IncrementalTemplate{Source: "routes"},
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: `{{ render "route-lines" }}`},
	}
	require.NoError(t, config.ValidateTemplateStructure(cfg))
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types: map[string]reflect.Type{}, Kinds: map[string]string{}, Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{Engine: engine, Config: cfg, Logger: slog.Default()})
	object := func(kind, namespace, name string) map[string]any {
		return map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"namespace": namespace, "name": name}}
	}
	routes := store.NewMemoryStore(len(indexBy))
	secrets := store.NewMemoryStore(len(indexBy))
	require.NoError(t, routes.Add(object("Route", "team-a", "app"), []string{"team-a", "app"}))
	require.NoError(t, secrets.Add(object("Secret", "team-a", "tls"), []string{"team-a", "tls"}))
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"routes": routes, "secrets": secrets})

	result, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	assert.Contains(t, result.HAProxyConfig, "route=tls")
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, service)

	require.NoError(t, routes.Add(object("Route", "team-b", "app"), []string{"team-b", "app"}))
	_, err = service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secrets team-b/tls cannot be read: watchedResources.secrets.namespaces limits the secrets watch to team-a")
}

func TestRenderService_Render_ResourceProjectionOutsideWatchedNamespacesFails(t *testing.T) {
	for _, tt := range []struct {
		namespaces []string
		wantErr    string
	}{
		{namespaces: []string{"default"}},
		{namespaces: []string{"team-a"}, wantErr: "objects default/selected cannot be read: watchedResources.objects.namespaces limits the objects watch to team-a"},
	} {
		t.Run(tt.namespaces[0], func(t *testing.T) {
			fixture := newResourceProjectionFixture(t, "selected")
			watched := fixture.config.WatchedResources["objects"]
			watched.Namespaces = tt.namespaces
			fixture.config.WatchedResources["objects"] = watched
			fixture.service = NewRenderService(&RenderServiceConfig{Engine: fixture.engine, Config: fixture.config, Logger: slog.Default()})
			fixture.add(t, "selected", "one")

			result, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)

			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Contains(t, result.HAProxyConfig, "selected=one")
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
