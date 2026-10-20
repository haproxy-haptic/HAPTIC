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

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"helm.sh/helm/v4/pkg/chart/common"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
)

func TestCollectConfigDocuments(t *testing.T) {
	tests := []struct {
		name      string
		manifests map[string]string
		want      []string
		wantErr   string
	}{
		{
			name: "keeps only HAProxyTemplateConfig documents",
			manifests: map[string]string{
				"haptic/templates/deployment.yaml":            "kind: Deployment\nmetadata:\n  name: c\n",
				"haptic/templates/haproxytemplateconfig.yaml": "kind: HAProxyTemplateConfig\nmetadata:\n  name: cfg\n",
			},
			want: []string{"name: cfg"},
		},
		{
			// Dropping these silently produced a config whose libraryRefs
			// pointed at documents that were not in the stream, so preflight
			// failed to load every chart-rendered configuration.
			name: "keeps the HAProxyTemplateLibrary objects the config references",
			manifests: map[string]string{
				"haptic/templates/deployment.yaml": "kind: Deployment\nmetadata:\n  name: c\n",
				"haptic/templates/haproxytemplateconfig.yaml": "kind: HAProxyTemplateConfig\nmetadata:\n  name: cfg\n" +
					"\n---\nkind: HAProxyTemplateLibrary\nmetadata:\n  name: lib-base\n",
			},
			want: []string{"name: cfg", "name: lib-base"},
		},
		{
			name: "splits multi-document files",
			manifests: map[string]string{
				"haptic/templates/all.yaml": "kind: Service\nmetadata:\n  name: s\n" +
					"\n---\nkind: HAProxyTemplateConfig\nmetadata:\n  name: a\n" +
					"\n---\nkind: HAProxyTemplateConfig\nmetadata:\n  name: b\n",
			},
			want: []string{"name: a", "name: b"},
		},
		{
			name: "orders by manifest path so a failure reproduces",
			manifests: map[string]string{
				"haptic/templates/z.yaml": "kind: HAProxyTemplateConfig\nmetadata:\n  name: zzz\n",
				"haptic/templates/a.yaml": "kind: HAProxyTemplateConfig\nmetadata:\n  name: aaa\n",
			},
			want: []string{"name: aaa", "name: zzz"},
		},
		{
			name:      "no config is an error, not an empty pass",
			manifests: map[string]string{"haptic/templates/deployment.yaml": "kind: Deployment\n"},
			wantErr:   "no HAProxyTemplateConfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := collectConfigDocuments(tt.manifests)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)

			var names []string
			for _, doc := range strings.Split(got, "\n---\n") {
				for _, line := range strings.Split(doc, "\n") {
					if strings.HasPrefix(strings.TrimSpace(line), "name:") {
						names = append(names, strings.TrimSpace(line))
					}
				}
			}
			assert.Equal(t, tt.want, names)
		})
	}
}

// The zero schemaSource is `validate` without --schema-dir: no schemas, both
// served checkers nil (ResolveEffectiveSpec then keeps every candidate), and an
// empty type-bootstrap Result rather than an error.
func TestZeroSchemaSourceIsTheOfflineFallThrough(t *testing.T) {
	var zero schemaSource
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec := &v1alpha1.HAProxyTemplateConfigSpec{}

	resolution, err := zero.resolveEffectiveSpec(context.Background(), spec, logger)
	require.NoError(t, err)
	assert.NotNil(t, resolution)

	typed, err := zero.typeBootstrap(context.Background(), spec, logger)
	require.NoError(t, err)
	require.NotNil(t, typed)
	assert.Empty(t, typed.Types)
}

func TestNewDirSchemaSource(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// An empty path is "no schemas", not an error.
	empty, err := newDirSchemaSource("", logger)
	require.NoError(t, err)
	assert.Equal(t, schemaSource{}, empty)

	loaded, err := newDirSchemaSource("../../tests/schemas", logger)
	require.NoError(t, err)
	require.NotNil(t, loaded.dir)
	assert.Positive(t, loaded.dir.Len())
	assert.Nil(t, loaded.live)

	// NewDirFetcher tolerates a missing directory by design, so the loader
	// itself does not error — TestPreflightSchemas covers the check that keeps
	// a typo from silently weakening preflight.
	absent, err := newDirSchemaSource(filepath.Join(t.TempDir(), "absent"), logger)
	require.NoError(t, err)
	assert.Zero(t, absent.dir.Len())
}

func TestPreflightSchemas(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Cleanup(func() { preflightSchemaDir, preflightKubeconfig = "", "" })

	t.Run("a directory with no schemas is refused, not silently accepted", func(t *testing.T) {
		preflightSchemaDir = filepath.Join(t.TempDir(), "typo")
		preflightKubeconfig = ""

		_, err := preflightSchemas(logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "holds no schemas")
	})

	t.Run("a populated directory keeps the run offline", func(t *testing.T) {
		preflightSchemaDir = "../../tests/schemas"
		preflightKubeconfig = ""

		schemas, err := preflightSchemas(logger)
		require.NoError(t, err)
		assert.Nil(t, schemas.live, "--schema-dir must not reach for the cluster")
		assert.Positive(t, schemas.dir.Len())
	})

	t.Run("without a directory it goes to the cluster and says so on failure", func(t *testing.T) {
		preflightSchemaDir = ""
		preflightKubeconfig = filepath.Join(t.TempDir(), "no-such-kubeconfig")

		_, err := preflightSchemas(logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reads the API schemas from the cluster")
		assert.Contains(t, err.Error(), "--schema-dir")
	})
}

// Cluster round-trips get a ceiling; reading a local directory cannot hang on a
// network, so an offline source must not inherit one.
func TestWithLiveDeadline(t *testing.T) {
	offlineCtx, cancel := schemaSource{}.withLiveDeadline(context.Background())
	defer cancel()
	_, hasDeadline := offlineCtx.Deadline()
	assert.False(t, hasDeadline)

	liveCtx, cancel2 := schemaSource{live: &liveCluster{}}.withLiveDeadline(context.Background())
	defer cancel2()
	deadline, hasDeadline := liveCtx.Deadline()
	require.True(t, hasDeadline, "a stale kubeconfig would otherwise hang the run")
	assert.WithinDuration(t, time.Now().Add(liveSchemaTimeout), deadline, 5*time.Second)
}

func TestVCLBackendHosts(t *testing.T) {
	vcl := `backend default {
    .host = "haptic-cache-origin.haptic.svc.cluster.local";
    .port = "8090";
}
backend other {
    .host  =  "second.example";
}
backend dup {
    .host = "haptic-cache-origin.haptic.svc.cluster.local";
}`
	// Sorted and de-duplicated: each host becomes one --add-host argument.
	assert.Equal(t, []string{"haptic-cache-origin.haptic.svc.cluster.local", "second.example"},
		vclBackendHosts(vcl))
	assert.Empty(t, vclBackendHosts("vcl 4.1;\n"))
}

func TestContainerRuntimeRejectsMissingOverride(t *testing.T) {
	t.Setenv("HAPTIC_CONTAINER_RUNTIME", "definitely-not-a-container-runtime")

	_, err := containerRuntime()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not executable")
}

// Nothing to compile means nothing to check — but only because the render
// produced neither, not because the runtime was missing.
func TestCheckRenderedSidecarConfigsSkipsWhenNothingRendered(t *testing.T) {
	t.Setenv("HAPTIC_CONTAINER_RUNTIME", "definitely-not-a-container-runtime")

	results := &testrunner.TestResults{TestResults: []testrunner.TestResult{{
		TestName:             "t",
		RenderedFiles:        map[string]string{"haproxy.cfg": "global"},
		RenderedK8sResources: map[string]string{"cm": "kind: ConfigMap\ndata:\n  a.txt: b\n"},
	}}}

	require.NoError(t, checkRenderedSidecarConfigs(context.Background(), nil, results))
}

func sidecarResults() *testrunner.TestResults {
	return &testrunner.TestResults{TestResults: []testrunner.TestResult{{
		TestName:      "t",
		RenderedFiles: map[string]string{"vector.yaml": "sources: {}\n"},
		RenderedK8sResources: map[string]string{
			"cache": "kind: Service\n---\nkind: ConfigMap\nmetadata:\n  name: cache-vcl\n" +
				"data:\n  default.vcl: |\n    backend b { .host = \"svc.ns.svc\"; }\n---\n" +
				varnishWorkload("StatefulSet", "varnish:chart-test", "cache-vcl"),
		},
	}}}
}

func TestCheckRenderedSidecarConfigsInvokesBothCompilers(t *testing.T) {
	checkSidecarCompilers(t)
}

func checkSidecarCompilers(t *testing.T) {
	t.Helper()
	t.Setenv("HAPTIC_VECTOR_IMAGE", "")
	t.Setenv("HAPTIC_VARNISH_IMAGE", "")
	configs, err := collectSidecarConfigs(vectorManifests("vector:chart-test"), sidecarResults())
	require.NoError(t, err)
	var argv []string
	run := func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		assert.Equal(t, "test-runtime", bin)
		_, deadline := ctx.Deadline()
		assert.True(t, deadline)
		argv = append(argv, args...)
		mount := strings.TrimSuffix(args[3], ":/w:ro")
		name := filepath.Base(args[len(args)-1])
		data, err := os.ReadFile(filepath.Join(mount, name))
		require.NoError(t, err)
		assert.NotEmpty(t, data)
		mountInfo, err := os.Stat(mount)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), mountInfo.Mode().Perm())
		parentInfo, err := os.Stat(filepath.Dir(mount))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), parentInfo.Mode().Perm())
		fileInfo, err := os.Stat(filepath.Join(mount, name))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), fileInfo.Mode().Perm())
		return nil, nil
	}
	require.NoError(t, runSidecarChecks(context.Background(), configs, "test-runtime", run))
	got := strings.Join(argv, "\n")

	assert.Contains(t, got, "validate", "vector config was not validated")
	assert.Contains(t, got, "/w/vector.yaml")
	assert.Contains(t, got, "varnishd", "VCL was not compiled")
	assert.Contains(t, got, "--cap-drop=ALL")
	assert.Contains(t, got, "--cap-add=IPC_LOCK")
	assert.Contains(t, got, "--security-opt=no-new-privileges")
	assert.Contains(t, got, "/w/default.vcl")
	// Backend hostnames are faked so varnishd's compile-time resolution can't
	// depend on cluster DNS that doesn't exist on the pipeline host.
	assert.Contains(t, got, "--add-host")
	assert.Contains(t, got, "svc.ns.svc:127.0.0.1")
	assert.Contains(t, got, "vector:chart-test")
	assert.Contains(t, got, "varnish:chart-test")
}

func TestCheckRenderedSidecarConfigsReportsTheConsequence(t *testing.T) {
	for _, kind := range []string{"vector", "varnish"} {
		t.Run(kind, func(t *testing.T) {
			run := func(context.Context, string, ...string) ([]byte, error) {
				return []byte("compiler output"), errors.New("exit status 1")
			}
			configs := []sidecarConfig{{testName: "failing-case", kind: kind, name: "config", image: "image:chart-test"}}
			err := runSidecarChecks(context.Background(), configs, "test-runtime", run)
			require.ErrorContains(t, err, "failing-case/config fails validation with image:chart-test")
			assert.ErrorContains(t, err, "fix the rendered config")
			assert.ErrorContains(t, err, "compiler output")
		})
	}
}

// writeProbeChart writes a minimal chart that echoes the values and release
// options it was rendered with. Rendering the real chart here would cost ~20s
// per case and would mostly test the chart; this tests renderChartManifests.
func writeProbeChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Chart.yaml"),
		[]byte("apiVersion: v2\nname: probe\nversion: "+probeChartVersion+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "values.yaml"),
		[]byte("a: from-defaults\nb: from-defaults\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "templates"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "templates", "cfg.yaml"), []byte(
		`{{- if eq .Values.a "boom" }}{{ fail "the values are not acceptable" }}{{- end }}
kind: HAProxyTemplateConfig
metadata:
  name: {{ .Release.Name }}-{{ .Release.Namespace }}
data:
  a: {{ .Values.a }}
  b: {{ .Values.b }}
  gatewayServed: {{ .Capabilities.APIVersions.Has "gateway.networking.k8s.io/v1/GatewayClass" }}
`), 0o600))
	return dir
}

func writeValues(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

const probeChartVersion = "0.1.0"

// offlineCaps mirrors the offline default: Gateway API assumed present.
func offlineCaps() *common.Capabilities {
	caps := common.DefaultCapabilities.Copy()
	caps.APIVersions = append(caps.APIVersions, gatewayAPIVersion)
	return caps
}

func TestRenderChartManifests(t *testing.T) {
	chart := writeProbeChart(t)
	vdir := t.TempDir()

	preflightNamespace, preflightRelease = "probe-ns", "probe-rel"
	t.Cleanup(func() { preflightNamespace, preflightRelease = "haptic", "haptic" })

	t.Run("later values file wins, others merge", func(t *testing.T) {
		base := writeValues(t, vdir, "base.yaml", "a: from-base\nb: from-base\n")
		over := writeValues(t, vdir, "over.yaml", "a: from-over\n")

		manifests, err := renderChartManifests(chart, []string{base, over}, "", offlineCaps())
		require.NoError(t, err)
		docs, err := collectConfigDocuments(manifests)
		require.NoError(t, err)

		assert.Contains(t, docs, "a: from-over", "the second values file did not win")
		assert.Contains(t, docs, "b: from-base", "the first values file was discarded instead of merged")
		// The namespace an operator deploys to is the one that gets validated —
		// which is what catches an assertion pinned to the CI namespace.
		assert.Contains(t, docs, "name: probe-rel-probe-ns")
	})

	t.Run("gateway API capability is always declared", func(t *testing.T) {
		vals := writeValues(t, vdir, "plain.yaml", "a: x\n")

		manifests, err := renderChartManifests(chart, []string{vals}, "", offlineCaps())
		require.NoError(t, err)
		docs, err := collectConfigDocuments(manifests)
		require.NoError(t, err)

		// Without it the chart silently drops its Gateway library and the check
		// would validate a config the cluster never receives.
		assert.Contains(t, docs, "gatewayServed: true")
	})

	t.Run("a chart fail() is reported as a values rejection", func(t *testing.T) {
		vals := writeValues(t, vdir, "bad.yaml", "a: boom\n")

		_, err := renderChartManifests(chart, []string{vals}, "", offlineCaps())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the chart rejects these values")
		assert.Contains(t, err.Error(), "the values are not acceptable")
	})

	t.Run("an unreadable values file names itself", func(t *testing.T) {
		_, err := renderChartManifests(chart, []string{filepath.Join(vdir, "absent.yaml")}, "", offlineCaps())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "absent.yaml")
	})
}

// The version guard is what makes the embedded-chart default sound in the
// pre-upgrade hook: a drifted image would otherwise validate the WRONG chart
// and pass on the wrong input.
func TestRenderChartManifests_ChartVersionGuard(t *testing.T) {
	chart := writeProbeChart(t)
	vdir := t.TempDir()
	vals := writeValues(t, vdir, "plain.yaml", "a: x\n")

	t.Run("matching version renders", func(t *testing.T) {
		_, err := renderChartManifests(chart, []string{vals}, probeChartVersion, offlineCaps())
		require.NoError(t, err)
	})

	t.Run("mismatched version hard-fails, never warns through", func(t *testing.T) {
		_, err := renderChartManifests(chart, []string{vals}, "9.9.9", offlineCaps())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "chart version mismatch")
		assert.Contains(t, err.Error(), probeChartVersion)
		assert.Contains(t, err.Error(), "9.9.9")
	})

	t.Run("empty expectation skips the guard", func(t *testing.T) {
		_, err := renderChartManifests(chart, []string{vals}, "", offlineCaps())
		require.NoError(t, err)
	})
}
