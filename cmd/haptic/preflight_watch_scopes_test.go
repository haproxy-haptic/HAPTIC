package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/dataplanetest"
)

func TestBundledChartPreflightDeploymentWatchScopes(t *testing.T) {
	t.Cleanup(dataplanetest.InstallFakeHAProxy())
	previousNamespace, previousRelease := preflightNamespace, preflightRelease
	preflightNamespace, preflightRelease = "haptic", "haptic"
	t.Cleanup(func() { preflightNamespace, preflightRelease = previousNamespace, previousRelease })
	values := `controller:
  rbac:
    releaseNamespaceSecretsOnly: true
  config:
    validationTests:
      rejects-foreign-secret:
        useDeploymentWatchScopes: true
        fixtures:
          ingresses:
            - apiVersion: networking.k8s.io/v1
              kind: Ingress
              metadata: {namespace: default, name: foreign-tls}
              spec:
                ingressClassName: haptic
                tls: [{hosts: [app.example.com], secretName: foreign-tls}]
                rules:
                  - host: app.example.com
                    http:
                      paths:
                        - path: /
                          pathType: Prefix
                          backend: {service: {name: app, port: {number: 80}}}
        assertions:
          - type: contains
            target: rendering_error
            pattern: 'secrets default/foreign-tls cannot be read: controller.rbac.releaseNamespaceSecretsOnly'
`
	dir := t.TempDir()
	manifests, err := renderChartManifests(repoPath(t, "charts", "haptic"), []string{writeValues(t, dir, "values.yaml", values)}, "", offlineCaps())
	require.NoError(t, err)
	docs, err := collectConfigDocuments(manifests)
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	schemas, err := newDirSchemaSource(repoPath(t, "tests", "schemas"), logger)
	require.NoError(t, err)
	setup, err := setupValidation(t.Context(), []string{writeValues(t, dir, "config.yaml", docs)}, schemas, nil, logger)
	require.NoError(t, err)
	t.Cleanup(setup.Cleanup)
	cfg, err := conversion.ConvertSpec(setup.ConfigSpec)
	require.NoError(t, err)
	require.Equal(t, []string{"haptic"}, cfg.WatchedResources["secrets"].Namespaces)
	cfg.TemplatingSettings.RenderTimeout = "5m"
	test := cfg.ValidationTests["rejects-foreign-secret"]
	require.True(t, test.UseDeploymentWatchScopes)
	global := cfg.ValidationTests["_global"].Fixtures["secrets"]
	require.NotEmpty(t, global)
	secret := runtime.DeepCopyJSON(global[0].(map[string]any))
	secret["metadata"] = map[string]any{"namespace": "default", "name": "foreign-tls"}
	test.Fixtures["secrets"] = []any{secret}
	cfg.ValidationTests["rejects-foreign-secret"] = test
	runner := testrunner.New(cfg, setup.Engine, setup.ValidationPaths, &testrunner.Options{Logger: logger, Workers: 1, Capabilities: setup.Capabilities, HAProxyVersion: setup.HAProxyVersion, TypedResourceTypes: setup.TypedResourceTypes})
	results, err := runner.RunTests(t.Context(), "rejects-foreign-secret")
	require.NoError(t, err)
	assert.True(t, results.AllPassed(), "%+v", results.TestResults)
	require.Len(t, results.TestResults, 1)
	assert.Contains(t, results.TestResults[0].RenderError, "secrets default/foreign-tls cannot be read")
}
