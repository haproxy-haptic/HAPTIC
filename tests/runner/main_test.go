// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpgradeWorkflowCoverageChecksTheCIWorkflow(t *testing.T) {
	root := t.TempDir()
	repository, err := os.OpenRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	require.ErrorIs(t, checkUpgradeWorkflows(repository, []string{"0.1.0"}), os.ErrNotExist)
	path := filepath.Join(root, ".github", "workflows", "ci-speed-proof.yml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("jobs: {}"), 0o600))
	require.ErrorContains(t, checkUpgradeWorkflows(repository, []string{"0.1.0"}), ".github/workflows/ci-speed-proof.yml")
	github := "jobs:\n  verified:\n    needs: [current, minimum]\n"
	for _, variant := range []string{"current", "minimum"} {
		github += fmt.Sprintf(`  %s:
    name: upgrade-0.1.0-%s
    env:
      PILOT_SUITE: upgrade
      BASELINE_CHART_VERSION: 0.1.0
      KIND_NODE_IMAGE: kindest/node:%s
      CI_SUITE: '{"name":"upgrade-0.1.0-%s","suite":"upgrade","baseline":"0.1.0","node":"kindest/node:%s"}'
`, variant, variant, variant, variant, variant)
	}
	require.NoError(t, os.WriteFile(path, []byte(github), 0o600))
	require.NoError(t, checkUpgradeWorkflows(repository, []string{"0.1.0"}))
}

func TestScenarioOptionsRetainEnvironmentAndFlags(t *testing.T) {
	t.Setenv("PLAIN_CLUSTER_NAME", "owned-plain")
	t.Setenv("KIND_NODE_IMAGE", "kindest/node:fixed")
	t.Setenv("KEEP_CLUSTER", "true")
	options, err := parseOptions([]string{"install-without-gateway-api"})
	require.NoError(t, err)
	require.Equal(t, "owned-plain", options.cluster)
	require.Equal(t, "kindest/node:fixed", options.nodeImage)
	require.True(t, options.keep)
	options, err = parseOptions([]string{"install-without-gateway-api", "--keep=false", "--cluster", "another", "--image", "registry:5000/haptic:test", "--artifacts", "/private/evidence"})
	require.NoError(t, err)
	require.Equal(t, "another", options.cluster)
	require.False(t, options.keep)
	require.Equal(t, "/private/evidence", options.artifacts)
}

func TestReleaseNamespaceSecretsOptionsOwnTheirCluster(t *testing.T) {
	t.Setenv("SECRETS_CLUSTER_NAME", "owned-secrets")
	options, err := parseOptions([]string{secretsScenario, "--image", "haptic:secrets"})
	require.NoError(t, err)
	require.Equal(t, "owned-secrets", options.cluster)
	require.Equal(t, "haptic:secrets", options.image)
	require.Equal(t, defaultRelease, options.namespace)
}

func TestImagePartsPreserveRegistryPortAndRequireTag(t *testing.T) {
	repository, tag, err := imageParts("registry:5000/project/haptic:test")
	require.NoError(t, err)
	require.Equal(t, "registry:5000/project/haptic", repository)
	require.Equal(t, "test", tag)
	for _, invalid := range []string{"haptic", "registry:5000/haptic", "haptic:", "haptic@sha256:abcd", ":test"} {
		_, _, err = imageParts(invalid)
		require.Error(t, err, invalid)
	}
}

func TestDefaultsOptionsKeepChartDefaultsAndVersionedImage(t *testing.T) {
	t.Setenv("TIMEOUT", "240")
	t.Setenv("CLUSTER_NAME", "owned-defaults")
	options, err := parseOptions([]string{defaultsScenario, "--keep-cluster", "--namespace", "custom"})
	require.NoError(t, err)
	require.Empty(t, options.image)
	require.True(t, options.keep)
	require.Equal(t, "custom", options.namespace)
	require.Equal(t, "owned-defaults", options.cluster)
	options.image = "registry:5000/haptic:test-haproxy3.3"
	repository, tag, series, err := controllerImage(&options)
	require.NoError(t, err)
	require.Equal(t, "registry:5000/haptic", repository)
	require.Equal(t, "test", tag)
	require.Equal(t, "3.3", series)
}

func TestArtifactsRemainFreshAndUpgradeEvidenceIsCollectedByCI(t *testing.T) {
	root := t.TempDir()
	path, err := scenarioArtifacts(root, &options{name: upgradeScenario, baseline: "0.4.0"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "debug-logs", "upgrade", "0.4.0"), filepath.Dir(path))
	requested := filepath.Join(root, "gitops")
	path, err = artifactDirectory(root, requested, gitopsScenario)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(path, "evidence"), []byte("first run"), 0o600))
	_, err = artifactDirectory(root, requested, gitopsScenario)
	require.ErrorIs(t, err, os.ErrExist)
}

func TestDefaultsRejectNonpositiveBudgetsAndRetainImageEnvironment(t *testing.T) {
	t.Setenv("IMAGE", "haptic:local-haproxy3.4")
	for _, seconds := range []string{"0", "-1"} {
		t.Setenv("TIMEOUT", seconds)
		_, err := parseOptions([]string{defaultsScenario})
		require.Error(t, err)
	}
	t.Setenv("TIMEOUT", "300")
	options, err := parseOptions([]string{defaultsScenario})
	require.NoError(t, err)
	require.Equal(t, "haptic:local-haproxy3.4", options.image)
}
