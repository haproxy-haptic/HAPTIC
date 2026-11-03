// Copyright 2026 Philipp Hossner
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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/dataplanetest"
)

func TestBundledChartPreflightJWKS(t *testing.T) {
	var body atomic.Value
	var status atomic.Int64
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(int(status.Load()))
		_, _ = fmt.Fprint(w, body.Load().(string))
	}))
	t.Cleanup(server.Close)
	transport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = transport })
	previous := preflightResourcePaths
	t.Cleanup(func() { preflightResourcePaths = previous })
	directory := t.TempDir()
	values := fmt.Sprintf(`controller:
  config:
    templatingSettings:
      extraContext:
        jwt:
          allowedJWKSURLs: {issuer: %q}
`, server.URL)
	manifests, err := renderChartManifests(repoPath(t, "charts", "haptic"),
		[]string{writeValues(t, directory, "values.yaml", values)}, "", offlineCaps())
	require.NoError(t, err)
	documents, err := collectConfigDocuments(manifests)
	require.NoError(t, err)
	configFile := writeValues(t, directory, "config.yaml", documents)
	spec, err := loadConfigFromFiles([]string{configFile})
	require.NoError(t, err)
	cfg, err := conversion.ConvertSpec(spec)
	require.NoError(t, err)
	resources := filepath.Join(directory, "resources")
	require.NoError(t, os.MkdirAll(resources, 0o750))
	certificate, err := yaml.Marshal(cfg.ValidationTests["_global"].Fixtures["secrets"][0])
	require.NoError(t, err)
	writeValues(t, resources, "certificate.yaml", string(certificate))
	writeValues(t, resources, "ingress.yaml", fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  namespace: default
  name: jwks
  annotations:
    haproxy-haptic.org/jwt-jwks-url: %q
spec:
  ingressClassName: haptic
  rules:
    - host: jwks.example.com
      http:
        paths:
          - {path: /, pathType: Prefix, backend: {service: {name: echo, port: {number: 80}}}}
---
apiVersion: v1
kind: Service
metadata: {namespace: default, name: echo}
spec: {ports: [{port: 80}]}
`, server.URL))
	preflightResourcePaths = []string{resources}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	schemas, err := newDirSchemaSource(repoPath(t, "tests", "schemas"), logger)
	require.NoError(t, err)
	for _, tc := range []struct {
		name       string
		status     int64
		body       string
		mapPattern string
		wantError  string
	}{
		{name: "successful fetch", status: 200, body: cfg.ValidationTests["test-haptic-jwks-overlap"].HTTPFixtures[0].Content, mapPattern: " jwks "},
		{name: "non-critical outage renders an unavailable route", status: 503, body: "unavailable", mapPattern: " missing "},
		{name: "invalid content fails preflight", status: 200, body: "not JSON", wantError: "JWKS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status.Store(tc.status)
			body.Store(tc.body)
			requests.Store(0)
			checked := false
			t.Cleanup(dataplanetest.InstallFakeHAProxy(dataplanetest.WithCheck(func(workDir string, _ []string) ([]byte, error) {
				content, readErr := os.ReadFile(filepath.Join(workDir, "maps", "haptic-jwt-routes.map"))
				if readErr != nil {
					return nil, readErr
				}
				assert.Contains(t, string(content), tc.mapPattern)
				checked = true
				return nil, nil
			})))
			checkErr := checkPreflightResources(t.Context(), configFile, schemas, logger)
			if tc.wantError != "" {
				require.ErrorContains(t, checkErr, tc.wantError)
				assert.False(t, checked)
			} else {
				require.NoError(t, checkErr)
				assert.True(t, checked)
			}
			assert.Positive(t, requests.Load())
		})
	}
}
