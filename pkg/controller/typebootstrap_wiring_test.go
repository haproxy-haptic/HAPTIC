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

package controller

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/kube-openapi/pkg/handler3"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
)

// servedKind is one resource the fake apiserver serves, as a CRD or as a
// built-in published only through OpenAPI v3.
type servedKind struct {
	gvk    schema.GroupVersionKind
	plural string
	crd    bool
}

// bundledChartKinds mirrors the kinds the bundled chart watches on a
// cluster with the Gateway API installed.
var bundledChartKinds = []servedKind{
	{gvk: schema.GroupVersionKind{Version: "v1", Kind: "Service"}, plural: "services"},
	{gvk: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, plural: "configmaps"},
	{gvk: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, plural: "secrets"},
	{gvk: schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, plural: "namespaces"},
	{gvk: schema.GroupVersionKind{Group: "discovery.k8s.io", Version: "v1", Kind: "EndpointSlice"}, plural: "endpointslices"},
	{gvk: schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"}, plural: "ingresses"},
	{gvk: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"}, plural: "gateways", crd: true},
	{gvk: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "GatewayClass"}, plural: "gatewayclasses", crd: true},
	{gvk: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}, plural: "httproutes", crd: true},
	{gvk: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "GRPCRoute"}, plural: "grpcroutes", crd: true},
	{gvk: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "TCPRoute"}, plural: "tcproutes", crd: true},
	{gvk: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "TLSRoute"}, plural: "tlsroutes", crd: true},
	{gvk: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "ReferenceGrant"}, plural: "referencegrants", crd: true},
	{gvk: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "BackendTLSPolicy"}, plural: "backendtlspolicies", crd: true},
	{gvk: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "ListenerSet"}, plural: "listenersets", crd: true},
	{gvk: schema.GroupVersionKind{Group: "haproxy-haptic.org", Version: "v1alpha1", Kind: "HAProxyRoutePolicy"}, plural: "haproxyroutepolicies", crd: true},
}

// bundledChartWatchedResources watches every kind once, plus the two
// entries the chart watches under a second name.
func bundledChartWatchedResources() *config.Config {
	watched := map[string]config.WatchedResource{
		"controller_services":      {APIVersion: "v1", Resources: "services"},
		"waf_selfservice_catalogs": {APIVersion: "v1", Resources: "configmaps"},
	}
	for _, kind := range bundledChartKinds {
		watched[kind.plural] = config.WatchedResource{APIVersion: kind.gvk.GroupVersion().String(), Resources: kind.plural}
	}
	return &config.Config{WatchedResources: watched}
}

// slowAPIServer answers discovery, CRD and OpenAPI v3 requests for its
// kinds, each after a fixed round trip, and counts the requests per path.
type slowAPIServer struct {
	*httptest.Server
	t         *testing.T
	roundTrip time.Duration
	kinds     []servedKind

	mu       sync.Mutex
	requests map[string]int
}

func newSlowAPIServer(t *testing.T, roundTrip time.Duration, kinds []servedKind) *slowAPIServer {
	t.Helper()
	s := &slowAPIServer{t: t, roundTrip: roundTrip, kinds: kinds, requests: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *slowAPIServer) requestCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.requests)
}

func (s *slowAPIServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests[r.URL.Path]++
	s.mu.Unlock()
	select {
	case <-time.After(s.roundTrip):
	case <-r.Context().Done():
		return
	}

	const crdPrefix = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"
	switch path := r.URL.Path; {
	case path == "/api":
		s.writeDiscovery(w, func(gvk schema.GroupVersionKind) bool { return gvk.Group == "" })
	case path == "/apis":
		s.writeDiscovery(w, func(gvk schema.GroupVersionKind) bool { return gvk.Group != "" })
	case strings.HasPrefix(path, crdPrefix):
		s.writeCRD(w, strings.TrimPrefix(path, crdPrefix))
	case path == "/openapi/v3":
		s.writeOpenAPIIndex(w)
	case strings.HasPrefix(path, "/openapi/v3/"):
		s.writeOpenAPIDocument(w, strings.TrimPrefix(path, "/openapi/v3/"))
	default:
		s.writeResourceList(w, strings.TrimPrefix(path, "/"))
	}
}

func (s *slowAPIServer) writeResourceList(w http.ResponseWriter, path string) {
	var list *metav1.APIResourceList
	for _, kind := range s.kinds {
		if gv := kind.gvk.GroupVersion(); groupVersionPath(gv) == path {
			if list == nil {
				list = &metav1.APIResourceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"}, GroupVersion: gv.String()}
			}
			list.APIResources = append(list.APIResources, metav1.APIResource{Name: kind.plural, Kind: kind.gvk.Kind, Namespaced: true})
		}
	}
	if list == nil {
		s.writeJSON(w, http.StatusNotFound, runtime.ContentTypeJSON, notFoundStatus())
		return
	}
	s.writeJSON(w, http.StatusOK, runtime.ContentTypeJSON, list)
}

func (s *slowAPIServer) writeDiscovery(w http.ResponseWriter, include func(schema.GroupVersionKind) bool) {
	groups := map[string]*apidiscoveryv2.APIGroupDiscovery{}
	for _, kind := range s.kinds {
		if !include(kind.gvk) {
			continue
		}
		group, ok := groups[kind.gvk.Group]
		if !ok {
			group = &apidiscoveryv2.APIGroupDiscovery{
				ObjectMeta: metav1.ObjectMeta{Name: kind.gvk.Group},
				Versions:   []apidiscoveryv2.APIVersionDiscovery{{Version: kind.gvk.Version, Freshness: apidiscoveryv2.DiscoveryFreshnessCurrent}},
			}
			groups[kind.gvk.Group] = group
		}
		group.Versions[0].Resources = append(group.Versions[0].Resources, apidiscoveryv2.APIResourceDiscovery{
			Resource:     kind.plural,
			ResponseKind: &metav1.GroupVersionKind{Group: kind.gvk.Group, Version: kind.gvk.Version, Kind: kind.gvk.Kind},
			Scope:        apidiscoveryv2.ScopeNamespace,
			Verbs:        []string{"get", "list", "watch"},
		})
	}
	list := apidiscoveryv2.APIGroupDiscoveryList{
		TypeMeta: metav1.TypeMeta{APIVersion: "apidiscovery.k8s.io/v2", Kind: "APIGroupDiscoveryList"},
	}
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		list.Items = append(list.Items, *groups[name])
	}
	s.writeJSON(w, http.StatusOK, "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList", list)
}

func (s *slowAPIServer) writeCRD(w http.ResponseWriter, name string) {
	for _, kind := range s.kinds {
		if !kind.crd || kind.plural+"."+kind.gvk.Group != name {
			continue
		}
		s.writeJSON(w, http.StatusOK, runtime.ContentTypeJSON, apiextensionsv1.CustomResourceDefinition{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition"},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: kind.gvk.Group,
				Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind.gvk.Kind, Plural: kind.plural},
				Scope: apiextensionsv1.NamespaceScoped,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
					Name: kind.gvk.Version, Served: true, Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:       "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{"spec": {Type: "object"}},
					}},
				}},
			},
		})
		return
	}
	s.writeJSON(w, http.StatusNotFound, runtime.ContentTypeJSON, notFoundStatus())
}

func groupVersionPath(gv schema.GroupVersion) string {
	if gv.Group == "" {
		return "api/" + gv.Version
	}
	return "apis/" + gv.Group + "/" + gv.Version
}

func (s *slowAPIServer) writeOpenAPIIndex(w http.ResponseWriter) {
	index := handler3.OpenAPIV3Discovery{Paths: map[string]handler3.OpenAPIV3DiscoveryGroupVersion{}}
	for _, kind := range s.kinds {
		path := groupVersionPath(kind.gvk.GroupVersion())
		index.Paths[path] = handler3.OpenAPIV3DiscoveryGroupVersion{ServerRelativeURL: "/openapi/v3/" + path + "?hash=fixture"}
	}
	s.writeJSON(w, http.StatusOK, runtime.ContentTypeJSON, index)
}

func (s *slowAPIServer) writeOpenAPIDocument(w http.ResponseWriter, path string) {
	components := &spec3.Components{Schemas: map[string]*spec.Schema{}}
	for _, kind := range s.kinds {
		if groupVersionPath(kind.gvk.GroupVersion()) != path {
			continue
		}
		components.Schemas[kind.gvk.String()] = &spec.Schema{
			VendorExtensible: spec.VendorExtensible{Extensions: spec.Extensions{
				"x-kubernetes-group-version-kind": []any{map[string]any{
					"group": kind.gvk.Group, "version": kind.gvk.Version, "kind": kind.gvk.Kind,
				}},
			}},
			SchemaProps: spec.SchemaProps{
				Type:       spec.StringOrArray{"object"},
				Properties: map[string]spec.Schema{"spec": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"object"}}}},
			},
		}
	}
	s.writeJSON(w, http.StatusOK, runtime.ContentTypeJSON, spec3.OpenAPI{Version: "3.0.0", Components: components})
}

func (s *slowAPIServer) writeJSON(w http.ResponseWriter, status int, contentType string, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		s.t.Errorf("encoding fake apiserver response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func notFoundStatus() metav1.Status {
	return metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound,
	}
}

func newClientForServer(t *testing.T, serverURL string) *client.Client {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: fake, cluster: {server: "`+serverURL+`"}}]
contexts: [{name: fake, context: {cluster: fake, user: fake}}]
users: [{name: fake, user: {}}]
current-context: fake
`), 0o600))
	k8sClient, err := client.New(client.Config{Kubeconfig: kubeconfig, Namespace: "haptic"})
	require.NoError(t, err)
	return k8sClient
}

// The 80 ms round trip stands in for the reported 25 ms one plus the pod's
// CPU share: resolved one after another, the bundled chart's 32 requests
// then cost 2.6 s against the 2 s deadline (#410).
const slowAPIServerRoundTrip = 80 * time.Millisecond

func TestTypeBootstrapFitsDeadlineOnSlowAPIServer(t *testing.T) {
	server := newSlowAPIServer(t, slowAPIServerRoundTrip, bundledChartKinds)
	bootstrap := newIterationTypeBootstrapper(newClientForServer(t, server.URL),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := bundledChartWatchedResources()

	result, err := bootstrap(t.Context(), cfg)
	require.NoError(t, err, "requests: %v", server.requestCounts())
	assert.Len(t, result.Types, len(cfg.WatchedResources))

	for path, n := range server.requestCounts() {
		assert.Equal(t, 1, n, "%s is fetched once per bootstrap", path)
	}
}

func TestBuildBootstrapResourcesFailsClosedOnDiscoveryErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{WatchedResources: map[string]config.WatchedResource{
		"services": {APIVersion: "v1", Resources: "services"},
		"widgets":  {APIVersion: "example.com/v1", Resources: "widgets"},
	}}
	served := &clienttesting.Fake{Resources: []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{{Name: "services", Kind: "Service"}},
	}}}

	resources, err := buildBootstrapResources(t.Context(), cfg, &fakediscovery.FakeDiscovery{Fake: served}, logger)
	require.NoError(t, err)
	require.Len(t, resources, 1, "a resource the cluster doesn't serve is skipped")
	assert.Equal(t, "Service", resources[0].GVK.Kind)

	unavailable := &clienttesting.Fake{}
	unavailable.AddReactor("get", "group", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	_, err = buildBootstrapResources(t.Context(), cfg, &fakediscovery.FakeDiscovery{Fake: unavailable}, logger)
	require.ErrorContains(t, err, "apiserver unavailable",
		"a failed discovery must not degrade every resource to untyped access")
}
