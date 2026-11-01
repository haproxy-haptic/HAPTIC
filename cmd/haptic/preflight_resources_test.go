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
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/dataplanetest"
)

func TestWatchedSelectionAdmits(t *testing.T) {
	object := func(namespace, class string, objectLabels map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"namespace": namespace, "name": "x", "labels": objectLabels},
			"spec":     map[string]any{"ingressClassName": class},
		}}
	}
	selection := &watchedSelection{
		namespaces: map[string]bool{"team-a": true},
		labels:     labels.SelectorFromSet(map[string]string{"tier": "web"}),
		fields:     fields.ParseSelectorOrDie("spec.ingressClassName=haptic"),
	}
	assert.True(t, selection.admits(object("team-a", "haptic", map[string]any{"tier": "web"})))
	assert.False(t, selection.admits(object("team-b", "haptic", map[string]any{"tier": "web"})), "namespace outside the watch")
	assert.False(t, selection.admits(object("team-a", "nginx", map[string]any{"tier": "web"})), "field selector")
	assert.False(t, selection.admits(object("team-a", "haptic", nil)), "label selector")
}

func TestReadManifestsExpandsListsAndDirectories(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o750))
	writeValues(t, dir, "a.yaml", "apiVersion: v1\nkind: Service\nmetadata: {name: one}\n---\napiVersion: v1\nkind: List\nitems:\n  - {apiVersion: v1, kind: Service, metadata: {name: two}}\n")
	writeValues(t, filepath.Join(dir, "sub"), "b.yml", "apiVersion: v1\nkind: Service\nmetadata: {name: three}\n")
	writeValues(t, dir, "notes.txt", "not a manifest")

	objects, err := readManifests([]string{dir})
	require.NoError(t, err)
	names := make([]string, 0, len(objects))
	for _, object := range objects {
		names = append(names, object.GetName())
	}
	assert.ElementsMatch(t, []string{"one", "two", "three"}, names)
}

// TestBundledChartPreflightResources renders the bundled chart against
// manifests and checks the watch's class filter and the haproxy -c verdict.
func TestBundledChartPreflightResources(t *testing.T) {
	previous := preflightResourcePaths
	t.Cleanup(func() { preflightResourcePaths = previous })

	dir := t.TempDir()
	manifests, err := renderChartManifests(repoPath(t, "charts", "haptic"),
		[]string{writeValues(t, dir, "values.yaml", "{}\n")}, "", offlineCaps())
	require.NoError(t, err)
	docs, err := collectConfigDocuments(manifests)
	require.NoError(t, err)
	configFile := writeValues(t, dir, "config.yaml", docs)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	schemas, err := newDirSchemaSource(repoPath(t, "tests", "schemas"), logger)
	require.NoError(t, err)

	resources := filepath.Join(dir, "resources")
	require.NoError(t, os.MkdirAll(resources, 0o750))
	ingress := func(name, class string) string {
		return "apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata: {namespace: default, name: " + name + "}\n" +
			"spec:\n  ingressClassName: " + class + "\n  rules:\n    - host: " + name + ".example.com\n" +
			"      http:\n        paths:\n          - {path: /, pathType: Prefix, backend: {service: {name: echo, port: {number: 80}}}}\n"
	}
	writeValues(t, resources, "ingresses.yaml", ingress("app", "haptic")+"---\n"+ingress("foreign", "nginx"))
	spec, err := loadConfigFromFiles([]string{configFile})
	require.NoError(t, err)
	cfg, err := conversion.ConvertSpec(spec)
	require.NoError(t, err)
	defaultCertificate, err := yaml.Marshal(cfg.ValidationTests["_global"].Fixtures["secrets"][0])
	require.NoError(t, err)
	writeValues(t, resources, "default-certificate.yaml", string(defaultCertificate))
	preflightResourcePaths = []string{resources}

	rejectHost := func(host string) {
		t.Cleanup(dataplanetest.InstallFakeHAProxy(dataplanetest.WithCheck(func(workDir string, _ []string) ([]byte, error) {
			cfg, readErr := os.ReadFile(filepath.Join(workDir, "haproxy.cfg"))
			if readErr != nil {
				return nil, readErr
			}
			maps, _ := os.ReadFile(filepath.Join(workDir, "maps", "host.map"))
			if strings.Contains(string(cfg)+string(maps), host) {
				return []byte("[ALERT]    (1) : config : refused " + host + "\n"), errors.New("exit status 1")
			}
			return nil, nil
		})))
	}

	t.Run("an Ingress of another class isn't rendered", func(t *testing.T) {
		rejectHost("foreign.example.com")
		require.NoError(t, checkPreflightResources(t.Context(), configFile, schemas, logger))
	})

	t.Run("a config HAProxy rejects fails preflight", func(t *testing.T) {
		rejectHost("app.example.com")
		err := checkPreflightResources(t.Context(), configFile, schemas, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refused app.example.com")
	})
}

func TestKindOfFromDiscovery(t *testing.T) {
	clientset := kubefake.NewClientset()
	discovery := clientset.Discovery().(*discoveryfake.FakeDiscovery)
	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: "networking.k8s.io/v1",
		APIResources: []metav1.APIResource{{Name: "ingresses", Kind: "Ingress"}},
	}}
	schemas := schemaSource{live: &liveCluster{clientset: clientset}}

	kind, err := kindOf(schemas, "networking.k8s.io/v1", "ingresses")
	require.NoError(t, err)
	assert.Equal(t, "Ingress", kind)

	_, err = kindOf(schemas, "example.org/v1", "widgets")
	require.ErrorContains(t, err, "no schema names the kind of example.org/v1 widgets")

	discovery.PrependReactor("get", "resource", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	_, err = kindOf(schemas, "networking.k8s.io/v1", "ingresses")
	require.ErrorContains(t, err, "discovering the kind of networking.k8s.io/v1 ingresses: forbidden")
}
