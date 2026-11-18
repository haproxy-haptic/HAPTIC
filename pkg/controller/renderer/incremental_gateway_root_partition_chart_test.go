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

package renderer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
)

type gatewayRootServedResources map[string]bool

func (s gatewayRootServedResources) IsServed(_, resources string) bool {
	return s[resources]
}

func TestGatewayHTTPRouteIncrementalRootPartition(t *testing.T) {
	type incremental struct {
		Root string `yaml:"root"`
	}
	type snippet struct {
		Incremental *incremental `yaml:"incremental"`
	}
	type library struct {
		TemplateSnippets map[string]snippet `yaml:"templateSnippets"`
	}

	files, err := filepath.Glob(filepath.Join(chartLibrariesRoot(t), "gateway", "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	actual := map[string]string{}
	for _, path := range files {
		content, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		var parsed library
		require.NoError(t, yaml.Unmarshal(content, &parsed), path)
		for name, candidate := range parsed.TemplateSnippets {
			if candidate.Incremental != nil && candidate.Incremental.Root != "" {
				actual[name] = candidate.Incremental.Root
			}
		}
	}

	assert.Equal(t, map[string]string{
		"backenditems-500-gateway-http":                "gateway-http-route-pre-analysis",
		"gateway-backendtlspolicy-route-ancestors-100": "gateway-http-route-pre-analysis",
		"gateway-route-attachments-100-http":           "gateway-http-route-pre-analysis",
		"gateway-route-candidates-100-http":            "gateway-http-route-pre-analysis",
		"gateway-route-filter-maps-100-http":           "gateway-http-route-post-analysis",
		"gateway-route-frontend-100-http":              "gateway-http-route-post-analysis",
		"gateway-route-paths-100-http":                 "gateway-http-route-post-analysis",
		"gateway-ssl-passthrough-100-http":             "gateway-http-route-pre-analysis",
		"map-backend-service-200-gateway-http":         "gateway-http-route-pre-analysis",
		"map-host-510-gateway-http":                    "gateway-http-route-pre-analysis",
		"map-weighted-backend-510-gateway-http":        "gateway-http-route-pre-analysis",
		"status-patches-201-gateway-httproute":         "gateway-http-route-pre-analysis",
	}, actual)
	assert.Empty(t, actual["gateway-route-analysis-100-http"])
}

func TestGatewaySchemaStrippedRootKeepsOnlyServedSourceComponents(t *testing.T) {
	snippets := loadChartSnippets(t, chartLibrariesRoot(t), map[string][]string{
		"gateway/22-ssl-passthrough.yaml": {
			"util-publish-gateway-http-ssl-passthrough",
			"util-publish-gateway-tls-ssl-passthrough",
			"gateway-ssl-passthrough-100-http",
			"gateway-ssl-passthrough-200-tls",
		},
		"gateway/30-backends.yaml": {
			"backenditems-501-gateway-ssl-passthrough-http",
			"backenditems-501-gateway-ssl-passthrough-tls",
			"backends-501-gateway-ssl-passthrough",
			"backendtlsvalues-490-gateway",
		},
	})
	root := snippets["backends-501-gateway-ssl-passthrough"]
	httpBackend := snippets["backenditems-501-gateway-ssl-passthrough-http"]
	require.NotNil(t, httpBackend.Incremental)
	assert.Equal(t, []string{"spec.rules[*].filters"}, httpBackend.Incremental.WhenAnyPathExists)
	assert.NotContains(t, root.Requires, "httproutes")
	assert.NotContains(t, root.Requires, "tlsroutes")
	assert.Contains(t, root.Template, `render_glob "gateway-ssl-passthrough-*"`)
	assert.Contains(t, root.Template, `render_glob "backenditems-501-gateway-ssl-passthrough-*"`)
	cfg := &config.Config{
		WatchedResources: map[string]config.WatchedResource{
			"backendtlspolicies": {APIVersion: "gateway.networking.k8s.io/v1", Resources: "backendtlspolicies"},
			"configmaps":         {APIVersion: "v1", Resources: "configmaps"},
			"endpoints":          {APIVersion: "v1", Resources: "endpointslices"},
			"gateways":           {APIVersion: "v1", Resources: "gateways"},
			"httproutes":         {APIVersion: "v1", Resources: "httproutes"},
			"namespaces":         {APIVersion: "v1", Resources: "namespaces"},
			"referencegrants":    {APIVersion: "v1", Resources: "referencegrants"},
			"secrets":            {APIVersion: "v1", Resources: "secrets"},
			"services":           {APIVersion: "v1", Resources: "services"},
			"tlsroutes":          {APIVersion: "v1", Resources: "tlsroutes", Optional: true},
		},
		TemplateSnippets: snippets,
	}
	require.NoError(t, config.ValidateTemplateStructure(cfg))

	effective, resolution, err := config.ResolveEffective(cfg, gatewayRootServedResources{
		"backendtlspolicies": true,
		"configmaps":         true,
		"endpointslices":     true,
		"gateways":           true,
		"httproutes":         true,
		"namespaces":         true,
		"referencegrants":    true,
		"secrets":            true,
		"services":           true,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"backenditems-501-gateway-ssl-passthrough-tls",
		"gateway-ssl-passthrough-200-tls",
		"util-publish-gateway-tls-ssl-passthrough",
	}, resolution.StrippedSnippets)
	assert.Contains(t, effective.TemplateSnippets, "gateway-ssl-passthrough-100-http")
	assert.Contains(t, effective.TemplateSnippets, "backenditems-501-gateway-ssl-passthrough-http")
	assert.Equal(t, []string{"spec.rules[*].filters"},
		effective.TemplateSnippets["backenditems-501-gateway-ssl-passthrough-http"].Incremental.WhenAnyPathExists)
	assert.Contains(t, effective.TemplateSnippets, "backends-501-gateway-ssl-passthrough")
	assert.NotContains(t, effective.TemplateSnippets, "gateway-ssl-passthrough-200-tls")
	assert.NotContains(t, effective.TemplateSnippets, "backenditems-501-gateway-ssl-passthrough-tls")
	assert.Empty(t, effective.AbsentIncrementalGroups)
	require.NoError(t, config.ValidateTemplateStructure(effective))
}
