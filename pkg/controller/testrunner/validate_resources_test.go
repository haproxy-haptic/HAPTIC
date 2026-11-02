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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
