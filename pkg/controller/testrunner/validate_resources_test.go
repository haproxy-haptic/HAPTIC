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

package testrunner

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/dataplanetest"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func TestRunner_ValidateResources(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restore := dataplanetest.InstallFakeHAProxy(dataplanetest.WithCheck(func(workDir string, _ []string) ([]byte, error) {
		cfg, err := os.ReadFile(filepath.Join(workDir, "haproxy.cfg"))
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(cfg), "groups=7") {
			return []byte("[ALERT]    (1) : config : too many words, truncating after word 64\n"), errors.New("exit status 1")
		}
		return nil, nil
	}))
	t.Cleanup(restore)

	template := "global\n" +
		"  # mode={{ extraContext | dig(\"mode\") | fallback(\"unset\") | tostring() }}\n" +
		"{%- for _, ing := range resources.ingresses.List() %}\n" +
		"  # {{ ing | dig(\"metadata\", \"name\") }} groups={{ ing | dig(\"metadata\", \"annotations\", \"groups\") }}\n" +
		"  {%- if (ing | dig(\"metadata\", \"annotations\", \"breaks\")) == \"yes\" %}{{ fail(\"broken ingress\") }}{% end %}\n" +
		"{%- end %}\n"
	ingress := func(name, groups, breaks string) any {
		return map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "Ingress",
			"metadata": map[string]any{
				"namespace":   "default",
				"name":        name,
				"annotations": map[string]any{"groups": groups, "breaks": breaks},
			},
		}
	}
	spec := &v1alpha1.HAProxyTemplateConfigSpec{
		TemplatingSettings: v1alpha1.TemplatingSettings{
			Engine:       "scriggo",
			ExtraContext: mustMarshalRawExtension(map[string]any{"mode": "deployment"}),
		},
		WatchedResources: map[string]v1alpha1.WatchedResource{
			"ingresses": {APIVersion: "networking.k8s.io/v1", Resources: "ingresses", IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
		HAProxyConfig: v1alpha1.HAProxyConfig{Template: template},
		ValidationTests: map[string]v1alpha1.ValidationTest{
			"_global": {ExtraContext: mustMarshalRawExtension(map[string]any{"mode": "global"})},
		},
	}
	decls := typebootstrap.BuildEngineDeclarations(&typebootstrap.Result{}, "ingresses")
	engine, err := templating.New(map[string]string{"haproxy.cfg": template},
		&templating.Options{EntryPoints: []string{"haproxy.cfg"}, Declarations: decls})
	require.NoError(t, err)
	cfg, err := conversion.ConvertSpec(spec)
	require.NoError(t, err)
	tempDir := t.TempDir()
	paths := &dataplane.ValidationPaths{
		MapsDir:           filepath.Join(tempDir, "maps"),
		SSLCertsDir:       filepath.Join(tempDir, "ssl"),
		GeneralStorageDir: filepath.Join(tempDir, "general"),
		ConfigFile:        filepath.Join(tempDir, "haproxy.cfg"),
	}
	runner := New(cfg, engine, paths, &Options{Logger: logger, Workers: 1})

	t.Run("renders the resources with the deployment's extraContext and no _global", func(t *testing.T) {
		rendered, valid, err := runner.ValidateResources(t.Context(), map[string][]any{"ingresses": {ingress("app", "2", "")}})
		require.NoError(t, err)
		assert.True(t, valid.Passed, valid.Error)
		assert.Contains(t, rendered.HAProxyConfig, "# mode=deployment")
		assert.Contains(t, rendered.HAProxyConfig, "# app groups=2")
	})

	t.Run("a config HAProxy rejects fails the check", func(t *testing.T) {
		_, valid, err := runner.ValidateResources(t.Context(), map[string][]any{"ingresses": {ingress("app", "7", "")}})
		require.NoError(t, err)
		assert.False(t, valid.Passed)
		assert.True(t, valid.ConfigRejected)
		assert.Contains(t, valid.Error, "too many words")
	})

	t.Run("a template failure is returned", func(t *testing.T) {
		_, _, err := runner.ValidateResources(t.Context(), map[string][]any{"ingresses": {ingress("app", "2", "yes")}})
		require.Error(t, err)
		assert.Contains(t, templating.RenderErrorMessage(err), "broken ingress")
	})
}

type resourceHTTPTestCase struct {
	name        string
	status      int
	critical    bool
	ignoreError bool
	body        string
	wantError   bool
	wantValid   bool
}

func TestRunnerValidateResourcesHTTP(t *testing.T) {
	for _, tc := range []resourceHTTPTestCase{
		{name: "accepted content", status: 200, body: "live-content", wantValid: true},
		{name: "non-critical outage", status: 503, body: "unavailable", wantValid: true},
		{name: "critical outage", status: 503, critical: true, body: "unavailable", wantError: true},
		{name: "ignored critical outage", status: 503, critical: true, ignoreError: true, body: "unavailable", wantError: true},
		{name: "invalid fetched configuration", status: 200, body: "reject-this-config"},
	} {
		t.Run(tc.name, func(t *testing.T) { validateResourcesHTTPCase(t, tc) })
	}
}

func validateResourcesHTTPCase(t *testing.T, tc resourceHTTPTestCase) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		assert.Equal(t, "Bearer preflight-test", req.Header.Get("Authorization"))
		w.WriteHeader(tc.status)
		_, _ = fmt.Fprint(w, tc.body)
	}))
	t.Cleanup(server.Close)
	template := `{%% var content, fetchErr = http.Fetch(%q, map[string]any{"critical": %t, "timeout": "1s"}, map[string]any{"type": "bearer", "token": "preflight-test"}) %%}
{%% if fetchErr != nil && %t %%}{{ fail(tostring(fetchErr)) }}{%% end %%}
global
  # fetched={{ content }}
`
	template = fmt.Sprintf(template, server.URL, tc.critical, !tc.ignoreError)
	fixtureAssertion := v1alpha1.ValidationAssertion{Type: "contains", Target: "rendering_error", Pattern: "no fixture defined for URL"}
	if tc.ignoreError {
		fixtureAssertion = v1alpha1.ValidationAssertion{Type: "contains", Target: "haproxy.cfg", Pattern: "global"}
	}
	spec := &v1alpha1.HAProxyTemplateConfigSpec{
		TemplatingSettings: v1alpha1.TemplatingSettings{Engine: "scriggo"},
		HAProxyConfig:      v1alpha1.HAProxyConfig{Template: template},
		ValidationTests: map[string]v1alpha1.ValidationTest{
			"requires-fixture": {Assertions: []v1alpha1.ValidationAssertion{fixtureAssertion}},
		},
	}
	cfg, err := conversion.ConvertSpec(spec)
	require.NoError(t, err)
	engine, err := templating.New(map[string]string{"haproxy.cfg": template}, &templating.Options{
		EntryPoints: []string{"haproxy.cfg"}, Declarations: typebootstrap.BuildEngineDeclarations(&typebootstrap.Result{}),
	})
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	directory := t.TempDir()
	paths := &dataplane.ValidationPaths{
		MapsDir: filepath.Join(directory, "maps"), SSLCertsDir: filepath.Join(directory, "ssl"),
		GeneralStorageDir: filepath.Join(directory, "general"), ConfigFile: filepath.Join(directory, "haproxy.cfg"),
	}
	t.Cleanup(dataplanetest.InstallFakeHAProxy(dataplanetest.WithCheck(func(workDir string, _ []string) ([]byte, error) {
		content, readErr := os.ReadFile(filepath.Join(workDir, "haproxy.cfg"))
		if readErr != nil {
			return nil, readErr
		}
		if strings.Contains(string(content), "reject-this-config") {
			return []byte("[ALERT] invalid fetched configuration\n"), errors.New("exit status 1")
		}
		return nil, nil
	})))
	runner := New(cfg, engine, paths, &Options{Logger: logger, Workers: 1})
	results, err := runner.RunTests(t.Context(), "requires-fixture")
	require.NoError(t, err)
	require.Zero(t, results.FailedTests, "%+v", results)
	require.Zero(t, requests.Load(), "validation tests must not fetch over the network")
	for range 2 {
		rendered, valid, renderErr := runner.ValidateResources(t.Context(), nil)
		if tc.wantError {
			require.Error(t, renderErr)
			assert.NotContains(t, renderErr.Error(), "no fixture defined")
			continue
		}
		require.NoError(t, renderErr)
		assert.Equal(t, tc.wantValid, valid.Passed, valid.Error)
		if tc.status == 200 {
			assert.Contains(t, rendered.HAProxyConfig, tc.body)
		} else {
			assert.NotContains(t, rendered.HAProxyConfig, tc.body)
		}
	}
	assert.Positive(t, requests.Load(), "resource validation must fetch real content")
	if tc.status == http.StatusOK {
		assert.Equal(t, int64(2), requests.Load(), "each resource render must fetch fresh content")
	}
}
