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

package renderer

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	controllerhttpstore "github.com/haproxy-haptic/HAPTIC/pkg/controller/httpstore"
	controllernames "github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// mockStoreProvider implements stores.StoreProvider for testing.
type mockStoreProvider struct {
	storeMap map[string]stores.Store
}

func (m *mockStoreProvider) GetStore(name string) stores.Store {
	return m.storeMap[name]
}

func TestHTTPSourceModeForRender(t *testing.T) {
	tests := []struct {
		name string
		mode rendercontext.RenderMode
		want controllerhttpstore.SourceMode
	}{
		{name: "live reconcile", mode: rendercontext.RenderModeReconcile, want: controllerhttpstore.SourceModeAuthoritative},
		{name: "admission", mode: rendercontext.RenderModeAdmission, want: controllerhttpstore.SourceModeReadOnly},
		{name: "unknown fails safe", mode: rendercontext.RenderMode("unknown"), want: controllerhttpstore.SourceModeReadOnly},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, httpSourceModeForRender(test.mode))
		})
	}
}

func (m *mockStoreProvider) StoreNames() []string {
	names := make([]string, 0, len(m.storeMap))
	for name := range m.storeMap {
		names = append(names, name)
	}
	return names
}

// mockTypedStore implements both stores.Store and types.Store for testing.
type mockTypedStore struct {
	items   []any
	listErr error
	getErr  error
}

func (m *mockTypedStore) Add(resource any, keys []string) error {
	m.items = append(m.items, resource)
	return nil
}

func (m *mockTypedStore) Update(resource any, keys []string) error {
	return nil
}

func (m *mockTypedStore) Delete(_, _ string, _ []string) error {
	return nil
}

func (m *mockTypedStore) List() ([]any, error) {
	return m.items, m.listErr
}

func (m *mockTypedStore) Get(keys ...string) ([]any, error) {
	return nil, m.getErr
}

func (m *mockTypedStore) Clear() error {
	m.items = nil
	return nil
}

// Verify mockTypedStore implements both interfaces.
var _ stores.Store = (*mockTypedStore)(nil)
var _ types.Store = (*mockTypedStore)(nil)

// testDataplaneConfig returns a Dataplane config with proper directory paths
// for testing. The RenderService extracts directory names from these paths
// using path.Base().
func testDataplaneConfig() config.DataplaneConfig {
	return config.DataplaneConfig{
		MapsDir:           "/etc/haproxy/maps",
		SSLCertsDir:       "/etc/haproxy/ssl",
		GeneralStorageDir: "/etc/haproxy/files",
	}
}

func TestNewRenderService(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{
			Template: "global\n    daemon\n",
		},
		Dataplane: testDataplaneConfig(),
	}

	engine, err := templating.New(map[string]string{"haproxy.cfg": cfg.HAProxyConfig.Template}, nil)
	require.NoError(t, err)

	logger := slog.Default()

	svc := NewRenderService(&RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       logger,
		Capabilities: defaultCapabilities(),
	})

	require.NotNil(t, svc)
	assert.NotNil(t, svc.engine)
	assert.NotNil(t, svc.config)
	assert.NotNil(t, svc.logger)
	assert.NotNil(t, svc.pathResolver)
	assert.Equal(t, "maps", svc.pathResolver.MapsDir)
	assert.Equal(t, "ssl", svc.pathResolver.SSLDir)
	assert.Equal(t, "files", svc.pathResolver.GeneralDir)
}

func TestNewRenderService_CrtListDir(t *testing.T) {
	// CRT-list files are ALWAYS stored in general file storage, regardless of HAProxy version.
	// This is because the native CRT-list API (POST ssl_crt_lists) triggers a reload without
	// supporting skip_reload, while general file storage returns 201 without triggering reloads.
	// See: pkg/dataplane/auxiliaryfiles/crtlist.go
	tests := []struct {
		name         string
		capabilities dataplane.Capabilities
		wantCrtList  string
	}{
		{
			name:         "supports crt-list",
			capabilities: defaultCapabilities(),
			wantCrtList:  "files", // Always uses general storage
		},
		{
			name:         "no crt-list support",
			capabilities: dataplane.Capabilities{SupportsCrtList: false},
			wantCrtList:  "files", // Always uses general storage
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				HAProxyConfig: config.HAProxyConfig{
					Template: "global\n",
				},
				Dataplane: testDataplaneConfig(),
			}

			engine, err := templating.New(map[string]string{"haproxy.cfg": cfg.HAProxyConfig.Template}, nil)
			require.NoError(t, err)

			svc := NewRenderService(&RenderServiceConfig{
				Engine:       engine,
				Config:       cfg,
				Logger:       slog.Default(),
				Capabilities: tt.capabilities,
			})

			assert.Equal(t, tt.wantCrtList, svc.pathResolver.CRTListDir)
		})
	}
}

func TestRenderService_Render_SimpleConfig(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{
			Template: "global\n    daemon\n\ndefaults\n    mode http\n",
		},
		Dataplane: testDataplaneConfig(),
	}

	engine, err := templating.New(map[string]string{"haproxy.cfg": cfg.HAProxyConfig.Template}, nil)
	require.NoError(t, err)

	svc := NewRenderService(&RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})

	provider := &mockStoreProvider{
		storeMap: map[string]stores.Store{},
	}

	result, err := svc.Render(context.Background(), provider, rendercontext.RenderModeReconcile)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Contains(t, result.HAProxyConfig, "global")
	assert.Contains(t, result.HAProxyConfig, "daemon")
	assert.Contains(t, result.HAProxyConfig, "defaults")
	assert.NotNil(t, result.AuxiliaryFileSnapshot)
	assert.NotNil(t, requireAuxiliaryFiles(t, result))
	assert.GreaterOrEqual(t, result.DurationMs, int64(0))
}

func TestRenderService_RenderReusesOnlyExactCommittedOutput(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: "global\n    daemon\n"},
		Dataplane:     testDataplaneConfig(),
	}
	engine, err := templating.New(map[string]string{"haproxy.cfg": cfg.HAProxyConfig.Template}, nil)
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
	})
	provider := &mockStoreProvider{storeMap: map[string]stores.Store{}}

	first, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	service.planMu.Lock()
	firstStoredPlan := service.lastPlan
	firstStoredOutput := service.lastOutputSnapshot
	service.planMu.Unlock()
	assert.Nil(t, firstStoredPlan)
	require.Same(t, first.OutputSnapshot, firstStoredOutput)
	unchanged, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	assert.Same(t, first.OutputSnapshot, unchanged.OutputSnapshot)

	require.NoError(t, unchanged.InputTransaction.Commit(t.Context()))
	service.planMu.Lock()
	unchangedStoredPlan := service.lastPlan
	unchangedStoredOutput := service.lastOutputSnapshot
	service.planMu.Unlock()
	assert.Nil(t, unchangedStoredPlan)
	assert.Same(t, firstStoredOutput, unchangedStoredOutput)
}

func TestRenderService_RenderNeverReusesUncommittedOutput(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: "global\n    daemon\n"},
		Dataplane:     testDataplaneConfig(),
	}
	engine, err := templating.New(map[string]string{"haproxy.cfg": cfg.HAProxyConfig.Template}, nil)
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
	})
	provider := &mockStoreProvider{storeMap: map[string]stores.Store{}}

	aborted, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	aborted.InputTransaction.Abort()

	retried, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	assert.NotSame(t, aborted.OutputSnapshot, retried.OutputSnapshot)
	require.NoError(t, retried.InputTransaction.Commit(t.Context()))

	committed, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	assert.Same(t, retried.OutputSnapshot, committed.OutputSnapshot)
	committed.InputTransaction.Abort()
}

func TestRenderService_RenderStagesCurrentConfigUntilCommit(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: `{% _ = len(currentConfig.ServerIndex) %}{% var token, backendErr = planRegistry.Backend(map[string]any{"name": "be_app", "servers": []any{map[string]any{"name": "srv1", "address": "10.0.0.2", "port": 8080}}}, "backend be_app\n    server srv1 10.0.0.2:8080\n") %}{% if backendErr != nil %}{{ fail(tostring(backendErr)) }}{% end %}global
{{ token }}`},
		Dataplane: testDataplaneConfig(),
	}
	engine, err := templating.New(
		map[string]string{"haproxy.cfg": cfg.HAProxyConfig.Template},
		&templating.Options{Declarations: map[string]any{
			"currentConfig": (*renderplan.CurrentConfig)(nil),
		}},
	)
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
	})
	service.rememberPlan(rendercontext.RenderModeReconcile, planWithServer("old", "10.0.0.1"))
	provider := &mockStoreProvider{storeMap: map[string]stores.Store{}}

	aborted, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", serverAddress(t, service.currentConfig()))
	aborted.InputTransaction.Abort()
	assert.Equal(t, "10.0.0.1", serverAddress(t, service.currentConfig()))

	committed, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, committed.InputTransaction.Commit(t.Context()))
	assert.Equal(t, "10.0.0.2", serverAddress(t, service.currentConfig()))
}

func TestRenderService_Render_CallCurrentFilesOverridesDefault(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: `{{ currentFiles["ticket.keys"] }}`},
		Dataplane:     testDataplaneConfig(),
	}
	engine, err := templating.New(
		map[string]string{"haproxy.cfg": cfg.HAProxyConfig.Template},
		&templating.Options{Declarations: map[string]any{"currentFiles": (*map[string]string)(nil)}},
	)
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
		CurrentAuxFilesProvider: func() map[string]string {
			return map[string]string{"ticket.keys": "provider"}
		},
	})

	result, err := service.Render(
		context.Background(),
		&mockStoreProvider{storeMap: map[string]stores.Store{}},
		rendercontext.RenderModeReconcile,
		rendercontext.WithCurrentAuxFiles(map[string]string{"ticket.keys": "term"}),
	)

	require.NoError(t, err)
	assert.Equal(t, "term\n", result.HAProxyConfig)
}

// A sensitive file is read back through currentSensitiveFiles only, and a
// changed one renders again rather than replaying the previous output.
func TestRenderService_Render_CurrentSensitiveFiles(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: `{{ currentSensitiveFiles["k"] }}|{{ len(currentFiles) }}`},
		Dataplane:     testDataplaneConfig(),
	}
	engine, err := templating.New(
		map[string]string{"haproxy.cfg": cfg.HAProxyConfig.Template},
		&templating.Options{Declarations: map[string]any{
			"currentFiles":          (*map[string]string)(nil),
			"currentSensitiveFiles": (*map[string]string)(nil),
		}},
	)
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
	})
	provider := &mockStoreProvider{storeMap: map[string]stores.Store{}}

	for _, content := range []string{"a", "b"} {
		result, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile,
			rendercontext.WithCurrentAuxFiles(map[string]string{dataplane.SensitiveFileKeyPrefix + "k": content}))
		require.NoError(t, err)
		assert.Equal(t, content+"|0\n", result.HAProxyConfig)
		require.NoError(t, result.InputTransaction.Commit(t.Context()))
	}
}

func TestRenderService_Render_WithStores(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{
			// Use the dot-notation typed-struct access pattern — the engine no longer
			// declares an untyped `resources` map; production wiring produces a typed
			// `*resources struct{Ingresses *innerStore; …}` (see
			// rendercontext.BuildResourcesValue / typebootstrap.BuildEngineDeclarations).
			Template: `global
{% for _, ing := range resources.ingresses.List() %}
# ingress: {{ ing }}
{% end %}
`,
		},
		WatchedResources: map[string]config.WatchedResource{
			// Declared so RenderService.buildRenderingContext emits an
			// `Ingresses` field on the runtime resources struct. The
			// test only exercises List(); IndexBy isn't needed here.
			"ingresses": {
				APIVersion: "networking.k8s.io/v1",
				Resources:  "ingresses",
			},
		},
		Dataplane: testDataplaneConfig(),
	}

	// Declare `resources` to match the typed-struct shape that
	// RenderService.buildRenderingContext produces via
	// rendercontext.BuildResourcesValue. typebootstrap.BuildEngineDeclarations
	// with an empty Result + extras emits the same per-resource store struct
	// shape (List/Fetch/GetSingle returning []any / any) that the runtime value
	// fills closures for.
	decls := typebootstrap.BuildEngineDeclarations(&typebootstrap.Result{}, "ingresses")
	engine, err := templating.New(map[string]string{"haproxy.cfg": cfg.HAProxyConfig.Template}, &templating.Options{EntryPoints: []string{"haproxy.cfg"}, Declarations: decls})
	require.NoError(t, err)

	svc := NewRenderService(&RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})

	ingressStore := &mockTypedStore{
		items: []any{"ingress1", "ingress2"},
	}

	provider := &mockStoreProvider{
		storeMap: map[string]stores.Store{
			"ingresses": ingressStore,
		},
	}

	result, err := svc.Render(context.Background(), provider, rendercontext.RenderModeReconcile)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Contains(t, result.HAProxyConfig, "global")
	assert.Contains(t, result.HAProxyConfig, "# ingress: ingress1")
	assert.Contains(t, result.HAProxyConfig, "# ingress: ingress2")
}

func TestRenderService_Render_FailsClosedOnResourceReadError(t *testing.T) {
	readErr := errors.New("apiserver unavailable")
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{
			Template: `{% for _, item := range resources.widgets.List() %}{{ item }}{% end %}`,
		},
		WatchedResources: map[string]config.WatchedResource{
			"widgets": {
				APIVersion: "example.io/v1",
				Resources:  "widgets",
			},
		},
		Dataplane: testDataplaneConfig(),
	}
	decls := typebootstrap.BuildEngineDeclarations(&typebootstrap.Result{}, "widgets")
	engine, err := templating.New(
		map[string]string{controllernames.MainTemplateName: cfg.HAProxyConfig.Template},
		&templating.Options{EntryPoints: []string{controllernames.MainTemplateName}, Declarations: decls},
	)
	require.NoError(t, err)

	svc := NewRenderService(&RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})
	provider := &mockStoreProvider{storeMap: map[string]stores.Store{
		"widgets": &mockTypedStore{listErr: readErr},
	}}

	result, err := svc.Render(t.Context(), provider, rendercontext.RenderModeReconcile)

	assert.Nil(t, result)
	require.ErrorIs(t, err, readErr)
	assert.Contains(t, err.Error(), "reading template resources")
}

func TestRenderService_Render_WithMapFiles(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{
			Template: "global\n    daemon\n",
		},
		Maps: map[string]config.MapFile{
			"domains.map": {
				Template: "example.com backend1\n",
			},
		},
		Dataplane: testDataplaneConfig(),
	}

	engine, err := templating.New(map[string]string{
		"haproxy.cfg": cfg.HAProxyConfig.Template,
		"domains.map": cfg.Maps["domains.map"].Template,
	}, nil)
	require.NoError(t, err)

	svc := NewRenderService(&RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})

	provider := &mockStoreProvider{
		storeMap: map[string]stores.Store{},
	}

	result, err := svc.Render(context.Background(), provider, rendercontext.RenderModeReconcile)

	require.NoError(t, err)
	require.NotNil(t, result)
	files := requireAuxiliaryFiles(t, result)
	assert.Len(t, files.MapFiles, 1)
	assert.Equal(t, "domains.map", files.MapFiles[0].Path)
	assert.Contains(t, files.MapFiles[0].Content, "example.com backend1")
	assert.Equal(t, 1, result.AuxFileCount)
}

func TestRenderService_Render_WithGeneralFiles(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{
			Template: "global\n    daemon\n",
		},
		Files: map[string]config.GeneralFile{
			"errors/503.http": {
				Template: "HTTP/1.1 503 Service Unavailable\r\n\r\n",
			},
		},
		Dataplane: testDataplaneConfig(),
	}

	engine, err := templating.New(map[string]string{
		"haproxy.cfg":     cfg.HAProxyConfig.Template,
		"errors/503.http": cfg.Files["errors/503.http"].Template,
	}, nil)
	require.NoError(t, err)

	svc := NewRenderService(&RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})

	provider := &mockStoreProvider{
		storeMap: map[string]stores.Store{},
	}

	result, err := svc.Render(context.Background(), provider, rendercontext.RenderModeReconcile)

	require.NoError(t, err)
	require.NotNil(t, result)
	files := requireAuxiliaryFiles(t, result)
	assert.Len(t, files.GeneralFiles, 1)
	assert.Equal(t, "errors/503.http", files.GeneralFiles[0].Filename)
	assert.Equal(t, "files/errors/503.http", files.GeneralFiles[0].Path)
	assert.Contains(t, files.GeneralFiles[0].Content, "503 Service Unavailable")
	assert.True(t, files.GeneralFiles[0].ReloadsOnPush(), "an entry that omits reloadOnPush must keep reloading")
}

// A `files:` entry carrying reloadOnPush: false has to reach the deployer as
// such — dropping it on the render hop would silently reinstate the reload.
func TestRenderService_Render_GeneralFileReloadOnPushFalse(t *testing.T) {
	noReload := false
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: "global\n    daemon\n"},
		Files: map[string]config.GeneralFile{
			"vector.yaml": {
				Template:     "sources: {}\n",
				ReloadOnPush: &noReload,
			},
		},
		Dataplane: testDataplaneConfig(),
	}

	engine, err := templating.New(map[string]string{
		"haproxy.cfg": cfg.HAProxyConfig.Template,
		"vector.yaml": cfg.Files["vector.yaml"].Template,
	}, nil)
	require.NoError(t, err)

	svc := NewRenderService(&RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})

	result, err := svc.Render(context.Background(), &mockStoreProvider{storeMap: map[string]stores.Store{}}, rendercontext.RenderModeReconcile)

	require.NoError(t, err)
	files := requireAuxiliaryFiles(t, result)
	require.Len(t, files.GeneralFiles, 1)
	assert.False(t, files.GeneralFiles[0].ReloadsOnPush())
}

func TestRenderService_Render_Error(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{
			Template: "global\n    daemon\n",
		},
		Dataplane: testDataplaneConfig(),
	}

	// Create engine without haproxy.cfg template to trigger error
	engine, err := templating.New(map[string]string{"other.cfg": "content"}, nil)
	require.NoError(t, err)

	svc := NewRenderService(&RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})

	provider := &mockStoreProvider{
		storeMap: map[string]stores.Store{},
	}

	result, err := svc.Render(context.Background(), provider, rendercontext.RenderModeReconcile)

	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "rendering haproxy.cfg")
}

func TestRenderService_Render_PathResolverAvailable(t *testing.T) {
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{
			Template: `global
# map path: {{ pathResolver.GetPath("hosts.map", "map") }}
`,
		},
		Dataplane: testDataplaneConfig(),
	}

	engine, err := templating.New(map[string]string{"haproxy.cfg": cfg.HAProxyConfig.Template}, nil)
	require.NoError(t, err)

	svc := NewRenderService(&RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})

	provider := &mockStoreProvider{
		storeMap: map[string]stores.Store{},
	}

	result, err := svc.Render(context.Background(), provider, rendercontext.RenderModeReconcile)

	require.NoError(t, err)
	require.NotNil(t, result)
	// PathResolver should resolve to relative path
	assert.Contains(t, result.HAProxyConfig, "# map path: maps/hosts.map")
}

// TestRenderService_buildRenderingContext_PropagatesIndexBy is a regression test
// pinning that the production renderer constructs StoreWrappers WITH the
// `IndexBy` slice from the corresponding `spec.watchedResources` entry. The
// wrapper's `IndexBy` is what enables Fetch/GetSingle to read from the
// per-render snapshot instead of bypassing to the live (potentially mutating)
// store — without it, parallel resource creation during the conformance
// suite produced inconsistent reads and intermittent render output. Issue #45
// tracks the original symptom (a flaky `HTTPRouteRequestMultipleMirrors`
// timeout and `TLSRouteHostnameIntersection` EOF), both traced back to this
// gap.
func TestRenderService_buildRenderingContext_PropagatesIndexBy(t *testing.T) {
	indexBy := []string{"metadata.namespace", "metadata.name"}
	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: "global\n  daemon\n"},
		Dataplane:     testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {
				APIVersion: "networking.k8s.io/v1",
				Resources:  "ingresses",
				IndexBy:    indexBy,
			},
			// "tlsroutes" deliberately omitted to verify the lookup
			// falls through cleanly when WatchedResources has no entry
			// for a present store.
		},
	}
	engine, err := templating.New(map[string]string{"haproxy.cfg": cfg.HAProxyConfig.Template}, nil)
	require.NoError(t, err)

	svc := NewRenderService(&RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})
	provider := &mockStoreProvider{
		storeMap: map[string]stores.Store{
			"ingresses": &mockTypedStore{},
			"tlsroutes": &mockTypedStore{},
		},
	}

	bctx, err := svc.buildRenderingContext(context.Background(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	renderCtx := bctx.Context
	require.NotNil(t, bctx.FileRegistry, "fileRegistry collector must be wired so templates can register dynamic aux files")
	require.NotNil(t, bctx.StatusPatchCollector, "statusPatchCollector must be wired so filters_status.go can capture mutations")
	require.NotNil(t, bctx.RenderedResourceCollector, "renderedResourceCollector must be wired so k8sResources templates can emit owned resources")

	// BuildResourcesValue produces the typed `resources` struct
	// with one field per cfg.WatchedResources entry — and ONLY per
	// WatchedResources entry. Stores outside WatchedResources (the
	// auto-injected haproxy_pods store, leftover provider entries,
	// etc.) are deliberately ignored; production lives in
	// controller["haproxy_pods"] for haproxy-pods and any drift here
	// adds a phantom field that mismatches what
	// typebootstrap.BuildEngineDeclarations declared, tripping
	// Scriggo's "must have type assignable to struct {...}" panic at
	// the first render.
	rv := reflect.ValueOf(renderCtx["resources"])
	require.Equal(t, reflect.Ptr, rv.Kind(),
		"renderCtx[\"resources\"] must be a *struct (typed-resources path); the map fallback was removed alongside the dead untyped engine path")
	resourcesStruct := rv.Elem()
	require.Equal(t, reflect.Struct, resourcesStruct.Kind())
	gotFields := make(map[string]bool, resourcesStruct.NumField())
	for i := 0; i < resourcesStruct.NumField(); i++ {
		gotFields[resourcesStruct.Type().Field(i).Name] = true
	}
	assert.True(t, gotFields["Ingresses"], "Ingresses field must be present (watched in cfg)")
	assert.False(t, gotFields["Tlsroutes"],
		"Tlsroutes must NOT be present — the fixture's store entry isn't in cfg.WatchedResources, "+
			"so leaking it would mismatch the engine declaration and panic Scriggo at render time "+
			"(this is exactly the helm-defaults CI failure mode that motivated the watchedNames-only contract)")
}
