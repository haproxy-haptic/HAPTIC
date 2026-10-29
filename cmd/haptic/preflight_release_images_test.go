package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestReleaseChartImageSelectionAndPreflightParity(t *testing.T) {
	versionBytes, err := os.ReadFile(repoPath(t, "VERSION"))
	require.NoError(t, err)
	version := strings.TrimSpace(string(versionBytes))
	digests := map[string]string{
		"3.0": "sha256:" + strings.Repeat("a", 64),
		"3.2": "sha256:" + strings.Repeat("b", 64),
		"3.3": "sha256:" + strings.Repeat("c", 64),
		"3.4": "sha256:" + strings.Repeat("d", 64),
	}
	metadata, err := json.Marshal(map[string]any{"version": version, "digests": digests})
	require.NoError(t, err)
	metadata = append(metadata, '\n')

	for series, digest := range digests {
		t.Run(series, func(t *testing.T) {
			verifyReleaseChartParity(t, version, series, string(metadata), "", "ghcr.io/haproxy-haptic/haptic@"+digest)
		})
	}
	for _, tt := range []struct {
		name, values, image string
	}{
		{"mirror", "controller:\n  image:\n    repository: mirror.example/haptic\n", "mirror.example/haptic@" + digests["3.4"]},
		{"tag", "controller:\n  image:\n    tag: custom\n", "ghcr.io/haproxy-haptic/haptic:custom-haproxy3.4"},
		{"digest", "controller:\n  image:\n    tag: ignored\n    digest: sha256:" + strings.Repeat("e", 64) + "\n",
			"ghcr.io/haproxy-haptic/haptic@sha256:" + strings.Repeat("e", 64)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			verifyReleaseChartParity(t, version, "3.4", string(metadata), tt.values, tt.image)
		})
	}
}

func verifyReleaseChartParity(t *testing.T, version, series, metadata, overrides, image string) {
	t.Helper()
	distributed := copyReleaseTestChart(t)
	require.NoError(t, os.WriteFile(filepath.Join(distributed, "release-images.json"), []byte(metadata), 0o600))
	// Separate files preserve overrides while manual webhook TLS makes both renders deterministic.
	base := writeValues(t, t.TempDir(), "base", "controller:\n  webhook:\n    caBundle: dGVzdA==\ncredentials:\n  dataplane:\n    password: release-parity-fixture\ndefaultSSLCertificate:\n  certManager:\n    enabled: false\n  create: true\n  cert: fixture-cert\n  key: fixture-key\n")
	operator := writeValues(t, t.TempDir(), "operator", "haproxyVersion: '"+series+"'\n"+overrides)
	manifests, err := renderChartManifests(distributed, []string{base, operator}, version, offlineCaps())
	require.NoError(t, err)
	for _, template := range []string{"deployment.yaml", "haproxy-deployment.yaml", "crd-upgrade-hook.yaml", "pre-rollout-validation-hook.yaml"} {
		manifest := manifests["haptic/templates/"+template]
		require.True(t, strings.Contains(manifest, "image: "+`"`+image+`"`) ||
			strings.Contains(manifest, "image: "+image+"\n"), "%s must use %s", template, image)
	}
	hook := manifests["haptic/templates/pre-rollout-validation-hook.yaml"]
	require.Contains(t, hook, "mountPath: /usr/share/haptic/chart/release-images.json")
	require.Contains(t, hook, "subPath: release-images.json")
	secret := releaseHookSecret(t, hook)
	require.Equal(t, metadata, secret.StringData["release-images.json"])
	require.NotContains(t, secret.StringData["values.yaml"], "release-images.json")

	embedded := copyReleaseTestChart(t)
	require.NoError(t, os.WriteFile(filepath.Join(embedded, "release-images.json"), []byte(secret.StringData["release-images.json"]), 0o600))
	hookValues := writeValues(t, t.TempDir(), "hook", secret.StringData["values.yaml"])
	replayed, err := renderChartManifests(embedded, []string{hookValues}, version, offlineCaps())
	require.NoError(t, err)
	require.Len(t, replayed, len(manifests))
	for name, manifest := range manifests {
		require.Equal(t, manifest, replayed[name], "preflight render differs for %s", name)
	}
}

func releaseHookSecret(t *testing.T, hook string) corev1.Secret {
	t.Helper()
	decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(hook), 4096)
	for {
		var secret corev1.Secret
		err := decoder.Decode(&secret)
		if err == io.EOF {
			t.Fatal("pre-rollout values Secret missing")
		}
		require.NoError(t, err)
		if secret.Kind == "Secret" {
			return secret
		}
	}
}

func copyReleaseTestChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS(repoPath(t, "charts", "haptic"))))
	return dir
}

func TestReleaseChartRejectsInvalidMetadata(t *testing.T) {
	version, err := os.ReadFile(repoPath(t, "VERSION"))
	require.NoError(t, err)
	for _, metadata := range []string{
		"", "{", "null", "[]", `{"version":"wrong","digests":{}}`,
		`{"version":"` + strings.TrimSpace(string(version)) + `","digests":{}}`,
		`{"version":"` + strings.TrimSpace(string(version)) + `","digests":{"3.0":"invalid","3.2":"invalid","3.3":"invalid","3.4":"invalid"}}`,
	} {
		t.Run(metadata, func(t *testing.T) {
			chart := copyReleaseTestChart(t)
			require.NoError(t, os.WriteFile(filepath.Join(chart, "release-images.json"), []byte(metadata), 0o600))
			_, err := renderChartManifests(chart, nil, "", offlineCaps())
			require.Error(t, err)
		})
	}
}
