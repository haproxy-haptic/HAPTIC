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

package dryrunvalidator

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/proposalvalidator"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validation"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const governanceSnippet = "features-960-governance"

// #412: the denial led with internal wrapping and a batch query key full of
// control characters instead of the governance rule's message.
func TestValidateDirect_GovernanceDenialLeadsWithRuleMessage(t *testing.T) {
	logger := slog.Default()
	cfg := &config.Config{
		Dataplane: config.DataplaneConfig{
			MapsDir:           "/etc/haproxy/maps",
			SSLCertsDir:       "/etc/haproxy/ssl",
			GeneralStorageDir: "/etc/haproxy/general",
		},
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"governance": map[string]any{
				"enabled":          true,
				"exemptNamespaces": []any{},
				"rules": map[string]any{
					"no-raw-config": map[string]any{
						"enabled":  true,
						"resource": "ingresses",
						"path":     "metadata.annotations['haproxy-haptic.org/config-frontend']",
						"pattern":  "^$",
						"message":  "raw HAProxy configuration from Ingresses is not allowed",
					},
				},
			},
		}},
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {
				APIVersion: "networking.k8s.io/v1",
				Resources:  "ingresses",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{governanceSnippet: loadGovernanceSnippet(t)},
		HAProxyConfig: config.HAProxyConfig{
			Template: testutil.ValidHAProxyConfigTemplate + `{{ render "features-960-governance" }}`,
		},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types:  map[string]reflect.Type{},
		Kinds:  map[string]string{},
		Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)

	proposalValidator := proposalvalidator.NewService(&proposalvalidator.ServiceConfig{
		Pipeline: pipeline.New(&pipeline.PipelineConfig{
			Renderer: renderer.NewRenderService(&renderer.RenderServiceConfig{Engine: engine, Config: cfg, Logger: logger}),
			Validator: validation.NewValidationService(&validation.ValidationServiceConfig{
				Logger:            logger,
				SkipDNSValidation: true,
			}),
			Logger: logger,
		}),
		BaseStoreProvider: stores.NewRealStoreProvider(map[string]stores.Store{"ingresses": k8sstore.NewMemoryStore(2)}),
		Logger:            logger,
	})
	component, err := New(&ComponentConfig{
		RESTMapper:        newTestRESTMapper(),
		ProposalValidator: proposalValidator,
		WatchedResources:  cfg.WatchedResources,
		Logger:            logger,
	})
	require.NoError(t, err)

	ingress := createTestIngress("x7-raw", "x7-raw")
	ingress.SetAnnotations(map[string]string{"haproxy-haptic.org/config-frontend": "http-request deny"})
	allowed, reason, _ := component.ValidateDirect(
		t.Context(), "networking.k8s.io/v1.Ingress", "x7-raw", "x7-raw", ingress, nil, operationCreate,
	)
	assert.False(t, allowed)
	assert.False(t, strings.ContainsFunc(reason, unicode.IsControl), "%q", reason)
	assert.NotContains(t, reason, "query")
	assert.Equal(t,
		"ingresses x7-raw/x7-raw violates governance rule 'no-raw-config': raw HAProxy configuration from Ingresses is not allowed",
		reason)
}

func loadGovernanceSnippet(t *testing.T) config.TemplateSnippet {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	content, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile),
		"..", "..", "..", "charts", "haptic", "charts", "governance", "library.yaml"))
	require.NoError(t, err)
	var library struct {
		TemplateSnippets map[string]struct {
			Template    string `yaml:"template"`
			Incremental *struct {
				BindingsTemplate string                     `yaml:"bindingsTemplate"`
				Effects          []config.IncrementalEffect `yaml:"effects"`
			} `yaml:"incremental"`
		} `yaml:"templateSnippets"`
	}
	require.NoError(t, yaml.Unmarshal(content, &library))
	snippet, exists := library.TemplateSnippets[governanceSnippet]
	require.True(t, exists)
	require.NotNil(t, snippet.Incremental)
	return config.TemplateSnippet{
		Name:     governanceSnippet,
		Template: snippet.Template,
		Incremental: &config.IncrementalTemplate{
			BindingsTemplate: snippet.Incremental.BindingsTemplate,
			Effects:          snippet.Incremental.Effects,
		},
	}
}
