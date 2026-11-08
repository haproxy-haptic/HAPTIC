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
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const backendServersExpected = `[{"address":"10.0.0.1","comment":"Pod: pod-a","disabled":true,"guid":"srv:be-main:svc-pod-a","name":"svc-pod-a","port":8080,"weight":25},{"address":"10.0.0.2","comment":"Pod: pod-b","guid":"srv:be-main:svc-pod-b","name":"svc-pod-b","port":8080,"weight":25}]`
const backendServersExternalNameExpected = `[{"address":"files.s3.example.net","comment":"ExternalName: files.s3.example.net","extra":[{"args":["dns"],"name":"resolvers"},{"args":["last,libc,none"],"name":"init-addr"}],"guid":"srv:be-main:svc-echo","name":"svc-echo","port":443,"weight":25}]`

const backendServersTestTemplate = `{%- import "util-backend-servers" for BackendServers -%}
{%- import "util-backend-servers-result" for BackendServersResult -%}
{%- var serviceName = "echo" -%}
{%- var namespace = "default" -%}
{%- var port = toint(extraContext | dig("port")) -%}
{%- var portName = extraContext | dig("portName") -%}
{%- var serverOpts = map[string]any{"namePrefix": "svc-", "weight": 25} -%}
{%- var marker = "degradedBackendRef:" + namespace + "/" + serviceName + "/http" -%}
{%- var result = BackendServersResult(serviceName, 0, port, serverOpts, portName, "be-main", namespace) -%}
pure={{ toJSON(result["servers"]) }}
degraded={{ result["degradedByName"] }}
port-name={{ result["portName"] }}
marker-before={{ shared.Get(marker) != nil }}
legacy={{ toJSON(BackendServers(serviceName, 0, port, serverOpts, portName, "be-main", namespace)) }}
marker-after={{ shared.Get(marker) != nil }}`

type backendServersChartLibrary struct {
	TemplateSnippets map[string]backendServersChartSnippet `yaml:"templateSnippets"`
}

type backendServersChartSnippet struct {
	Template string `yaml:"template"`
}

type backendServersService struct {
	Metadata backendServersMetadata    `json:"metadata"`
	Spec     backendServersServiceSpec `json:"spec"`
}

type backendServersServiceSpec struct {
	Type         string                      `json:"type"`
	ExternalName string                      `json:"externalName"`
	Ports        []backendServersServicePort `json:"ports"`
}

type backendServersServicePort struct {
	Name string `json:"name"`
	Port int64  `json:"port"`
}

type backendServersEndpointSlice struct {
	Metadata  backendServersMetadata       `json:"metadata"`
	Ports     []backendServersEndpointPort `json:"ports"`
	Endpoints []backendServersEndpoint     `json:"endpoints"`
}

type backendServersMetadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type backendServersEndpointPort struct {
	Name string `json:"name"`
	Port int64  `json:"port"`
}

type backendServersEndpoint struct {
	Addresses  []string                         `json:"addresses"`
	Conditions backendServersEndpointConditions `json:"conditions"`
	TargetRef  backendServersTargetRef          `json:"targetRef"`
}

type backendServersEndpointConditions struct {
	Ready       *bool `json:"ready"`
	Serving     *bool `json:"serving"`
	Terminating *bool `json:"terminating"`
}

type backendServersTargetRef struct {
	Name string `json:"name"`
}

type backendServersObservation struct {
	pure         string
	legacy       string
	degraded     bool
	portName     string
	markerBefore bool
	markerAfter  bool
}

func TestBackendServersResultPreservesLegacyBytesWithoutSharedMutation(t *testing.T) {
	tests := map[string]struct {
		port             int
		portName         any
		serviceResource  map[string]any
		withEndpoint     bool
		wantServers      string
		wantPortName     string
		wantDegraded     bool
		wantLegacyMarker bool
	}{
		"numeric port": {
			port: 80, serviceResource: backendServersServiceResource(), withEndpoint: true,
			wantServers: backendServersExpected, wantPortName: "http",
		},
		"named port": {
			portName: "http", serviceResource: backendServersServiceResource(), withEndpoint: true,
			wantServers: backendServersExpected, wantPortName: "http",
		},
		"named port resolves without Service": {
			portName: "http", withEndpoint: true,
			wantServers: backendServersExpected, wantPortName: "http",
		},
		"missing Service and endpoints is degraded": {
			portName: "http", wantServers: "[]", wantPortName: "http", wantDegraded: true, wantLegacyMarker: true,
		},
		"ExternalName Service with numeric port": {
			port: 443, serviceResource: backendServersExternalNameServiceResource("files.s3.example.net", true),
			wantServers: backendServersExternalNameExpected, wantPortName: "http",
		},
		"ExternalName Service with named port": {
			portName: "http", serviceResource: backendServersExternalNameServiceResource("files.s3.example.net", true),
			wantServers: backendServersExternalNameExpected, wantPortName: "http",
		},
		"ExternalName Service without spec.ports uses numeric port": {
			port: 443, serviceResource: backendServersExternalNameServiceResource("files.s3.example.net", false),
			wantServers: backendServersExternalNameExpected,
		},
		"ExternalName Service with invalid hostname is skipped": {
			port: 443, serviceResource: backendServersExternalNameServiceResource("bad host\ninjected", true),
			wantServers: "[]", wantPortName: "http",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			observation := renderBackendServersChart(t, test.port, test.portName, test.serviceResource, test.withEndpoint)

			assert.Equal(t, test.wantServers, observation.pure)
			assert.Equal(t, observation.pure, observation.legacy)
			assert.Equal(t, test.wantDegraded, observation.degraded)
			assert.Equal(t, test.wantPortName, observation.portName)
			assert.False(t, observation.markerBefore)
			assert.Equal(t, test.wantLegacyMarker, observation.markerAfter)
		})
	}
}

func renderBackendServersChart(
	t *testing.T,
	port int,
	portName any,
	serviceResource map[string]any,
	withEndpoint bool,
) backendServersObservation {
	t.Helper()
	servicesStore := k8sstore.NewMemoryStore(2)
	endpointsStore := k8sstore.NewMemoryStore(2)
	if serviceResource != nil {
		require.NoError(t, servicesStore.Add(serviceResource, []string{"default", "echo"}))
	}
	if withEndpoint {
		require.NoError(t, endpointsStore.Add(backendServersEndpointResource(), []string{"default", "echo"}))
	}
	storesBefore := marshalBackendServersStores(t, servicesStore, endpointsStore)

	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"port":     port,
			"portName": portName,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"services": {
				APIVersion: "v1",
				Resources:  "services",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
			"endpoints": {
				APIVersion: "discovery.k8s.io/v1",
				Resources:  "endpointslices",
				IndexBy:    []string{"metadata.namespace", "metadata.labels.kubernetes\\.io/service-name"},
			},
		},
		TemplateSnippets: loadBackendServersChartSnippets(t),
		HAProxyConfig:    config.HAProxyConfig{Template: backendServersTestTemplate},
	}
	types := &typebootstrap.Result{
		Types: map[string]reflect.Type{
			"services":  reflect.TypeOf(backendServersService{}),
			"endpoints": reflect.TypeOf(backendServersEndpointSlice{}),
		},
		Kinds:  map[string]string{"services": "Service", "endpoints": "EndpointSlice"},
		Errors: map[string]error{},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	engine, err := helpers.NewEngineFromConfigWithOptions(
		cfg, nil, nil, declarations, helpers.EngineOptions{},
	)
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	provider := stores.NewRealStoreProvider(map[string]stores.Store{
		"services": servicesStore, "endpoints": endpointsStore,
	})

	result, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	assert.Equal(t, storesBefore, marshalBackendServersStores(t, servicesStore, endpointsStore))

	return parseBackendServersObservation(t, result.HAProxyConfig)
}

func loadBackendServersChartSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
	result := make(map[string]config.TemplateSnippet, 4)
	for _, relativePath := range []string{"base/library.yaml", "kubernetes-backends/library.yaml"} {
		content, err := os.ReadFile(filepath.Join(chartRoot, relativePath))
		require.NoError(t, err)
		var library backendServersChartLibrary
		require.NoError(t, yaml.Unmarshal(content, &library))
		for _, name := range []string{
			"util-backend-servers-helpers",
			"util-backend-servers-result",
			"util-gated-pod-routable",
			"util-backend-servers",
		} {
			snippet, found := library.TemplateSnippets[name]
			if found {
				result[name] = config.TemplateSnippet{Name: name, Template: snippet.Template}
			}
		}
	}
	require.Len(t, result, 4)
	return result
}

func backendServersServiceResource() map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"namespace": "default", "name": "echo"},
		"spec":     map[string]any{"ports": []any{map[string]any{"name": "http", "port": int64(80)}}},
	}
}

func backendServersExternalNameServiceResource(externalName string, withPorts bool) map[string]any {
	spec := map[string]any{
		"type":         "ExternalName",
		"externalName": externalName,
	}
	if withPorts {
		spec["ports"] = []any{map[string]any{"name": "http", "port": int64(443)}}
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"namespace": "default", "name": "echo"},
		"spec":     spec,
	}
}

func backendServersEndpointResource() map[string]any {
	return map[string]any{
		"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
		"metadata": map[string]any{
			"namespace": "default", "name": "echo-abc",
			"labels": map[string]any{"kubernetes.io/service-name": "echo"},
		},
		"ports": []any{map[string]any{"name": "http", "port": int64(8080)}},
		"endpoints": []any{
			map[string]any{
				"addresses": []any{"10.0.0.2"},
				"targetRef": map[string]any{"name": "pod-b"},
			},
			map[string]any{
				"addresses":  []any{"10.0.0.1"},
				"conditions": map[string]any{"ready": false},
				"targetRef":  map[string]any{"name": "pod-a"},
			},
		},
	}
}

func marshalBackendServersStores(t *testing.T, storesToMarshal ...stores.Store) string {
	t.Helper()
	all := make([]any, 0, len(storesToMarshal))
	for _, store := range storesToMarshal {
		items, err := store.List()
		require.NoError(t, err)
		all = append(all, items)
	}
	encoded, err := json.Marshal(all)
	require.NoError(t, err)
	return string(encoded)
}

func parseBackendServersObservation(t *testing.T, output string) backendServersObservation {
	t.Helper()
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, found := strings.Cut(line, "=")
		require.True(t, found, line)
		values[key] = value
	}
	require.Equal(t, []string{"degraded", "legacy", "marker-after", "marker-before", "port-name", "pure"},
		slices.Sorted(maps.Keys(values)))
	return backendServersObservation{
		pure:         values["pure"],
		legacy:       values["legacy"],
		degraded:     values["degraded"] == "true",
		portName:     values["port-name"],
		markerBefore: values["marker-before"] == "true",
		markerAfter:  values["marker-after"] == "true",
	}
}
