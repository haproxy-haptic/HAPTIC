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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
)

func TestWriteSnapshot(t *testing.T) {
	const tempRoot = "/tmp/haproxy-validate-123"
	results := &testrunner.TestResults{TestResults: []testrunner.TestResult{
		{
			TestName:       "test-a",
			RenderedConfig: "backend b\n  server s1 " + tempRoot + "/worker-3/test-7/x\n",
			RenderedMaps:   map[string]string{"routes/host.map": "example.com be\n"},
			RenderedSecretNames: []string{
				"cert:" + tempRoot + "/worker-3/test-7/ssl/tls.pem",
				"secret:ssl/tls-ticket-keys",
				"sensitive-map:haptic-apikeys.map",
			},
			// Never rendered, never snapshotted — its absence is the signal.
			RenderedK8sResources: map[string]string{"varnish-cache": "kind: ConfigMap"},
		},
		{TestName: "test-skipped", Skipped: true},
	}}

	dir := t.TempDir()
	require.NoError(t, writeSnapshot(dir, tempRoot, results))

	cfg, err := os.ReadFile(filepath.Join(dir, "test-a", "haproxy.cfg"))
	require.NoError(t, err)
	assert.Equal(t, "backend b\n  server s1 "+snapshotRenderRoot+"/x\n", string(cfg),
		"the per-run temp path must be normalised away, or two runs never compare equal")

	mapFile, err := os.ReadFile(filepath.Join(dir, "test-a", "maps", "routes_host.map"))
	require.NoError(t, err)
	assert.Equal(t, "example.com be\n", string(mapFile))

	secretNames, err := os.ReadFile(filepath.Join(dir, "test-a", snapshotSecretNamesFile))
	require.NoError(t, err)
	assert.Equal(t, "cert:"+snapshotRenderRoot+"/ssl/tls.pem\nsecret:ssl/tls-ticket-keys\nsensitive-map:haptic-apikeys.map\n", string(secretNames),
		"certificates, secret files and sensitive maps are listed by name only")
	assert.NoDirExists(t, filepath.Join(dir, "test-a", "certs"))

	assert.NoDirExists(t, filepath.Join(dir, "test-a", "k8s"),
		"Kubernetes-side output carries wall-clock timestamps and must stay out of the snapshot")
	assert.NoDirExists(t, filepath.Join(dir, "test-skipped"))
}

func TestSnapshotName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain name is kept", "haproxy.cfg", "haproxy.cfg"},
		{"separators flatten", "routes/host.map", "routes_host.map"},
		{"traversal cannot escape the snapshot", "../../etc/passwd", "_.._etc_passwd"},
		{"an empty name still gets a file", "", "unnamed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, snapshotName(tt.input))
		})
	}
}
