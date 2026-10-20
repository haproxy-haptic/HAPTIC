package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/dataplanetest"
)

func TestBundledChartPreflightUsesConfiguredVarnishImage(t *testing.T) {
	t.Cleanup(dataplanetest.InstallFakeHAProxy())
	t.Setenv("HAPTIC_VECTOR_IMAGE", "")
	t.Setenv("HAPTIC_VARNISH_IMAGE", "")
	version, err := os.ReadFile(repoPath(t, "VERSION"))
	require.NoError(t, err)

	tests := []struct {
		name      string
		values    string
		wantImage string
	}{
		{
			name:      "enabled bundled image",
			values:    "cache: {varnish: {enabled: true}}\n",
			wantImage: "ghcr.io/haproxy-haptic/haptic/varnish:" + strings.TrimSpace(string(version)),
		},
		{
			name:      "enabled operator image",
			values:    "cache: {varnish: {enabled: true, image: 'example.invalid/cache@sha256:abc'}}\n",
			wantImage: "example.invalid/cache@sha256:abc",
		},
		{
			name:      "disabled fixture image",
			values:    "cache: {varnish: {enabled: false}}\n",
			wantImage: "ghcr.io/haproxy-haptic/haptic/varnish:0.6.0",
		},
		{
			name:      "disabled staged operator image",
			values:    "cache: {varnish: {enabled: false, image: 'example.invalid/cache@sha256:def'}}\n",
			wantImage: "example.invalid/cache@sha256:def",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configs := bundledChartSidecarConfigs(t, tt.values)
			var varnish []sidecarConfig
			for _, config := range configs {
				if config.kind == "varnish" {
					varnish = append(varnish, config)
					assert.Equal(t, tt.wantImage, config.image)
				}
			}
			require.NotEmpty(t, varnish, "the cache fixture must reach the native VCL compiler")
			calls := 0
			err := runSidecarChecks(t.Context(), varnish, "container-runtime",
				func(_ context.Context, _ string, args ...string) ([]byte, error) {
					calls++
					assert.Contains(t, args, tt.wantImage)
					assert.Contains(t, args, "varnishd")
					assert.Contains(t, args, "-C")
					return nil, nil
				})
			require.NoError(t, err)
			assert.Len(t, varnish, calls)
		})
	}
}

func bundledChartSidecarConfigs(t *testing.T, values string) []sidecarConfig {
	t.Helper()
	dir := t.TempDir()
	valuesFile := writeValues(t, dir, "values", values)
	manifests, err := renderChartManifests(repoPath(t, "charts", "haptic"), []string{valuesFile}, "", offlineCaps())
	require.NoError(t, err)
	docs, err := collectConfigDocuments(manifests)
	require.NoError(t, err)
	configFile := writeValues(t, dir, "config", docs)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	schemas, err := newDirSchemaSource(repoPath(t, "tests", "schemas"), logger)
	require.NoError(t, err)
	setup, err := setupValidation(t.Context(), []string{configFile}, schemas, nil, logger)
	require.NoError(t, err)
	t.Cleanup(setup.Cleanup)
	cfg, err := conversion.ConvertSpec(setup.ConfigSpec)
	require.NoError(t, err)
	cfg.TemplatingSettings.RenderTimeout = "5m"
	runner := testrunner.New(cfg, setup.Engine, setup.ValidationPaths, &testrunner.Options{
		Logger:             logger,
		Workers:            1,
		Capabilities:       setup.Capabilities,
		HAProxyVersion:     setup.HAProxyVersion,
		TypedResourceTypes: setup.TypedResourceTypes,
	})
	results, err := runner.RunTests(t.Context(), "test-haptic-cache-workdir-statefulset")
	require.NoError(t, err)
	require.True(t, results.AllPassed(), "%+v", results.TestResults)
	configs, err := collectSidecarConfigs(manifests, results)
	require.NoError(t, err)
	return configs
}
