// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/kube-openapi/pkg/validation/spec"

	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/schemafetcher"
)

// fakeDiscoveryWithErr wraps the fake discovery to inject per-groupVersion errors.
type fakeDiscoveryWithErr struct {
	discovery.DiscoveryInterface
	errs map[string]error
}

func (f *fakeDiscoveryWithErr) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	if err, ok := f.errs[gv]; ok {
		return nil, err
	}
	return f.DiscoveryInterface.ServerResourcesForGroupVersion(gv)
}

// TestDiscoveryServedChecker_TransientVsNotFound pins the error
// discrimination (runtime-version-detection review finding): only an
// authoritative NotFound counts as unserved; any other discovery error is
// recorded as transient so the caller fails the resolution instead of
// silently stripping optional features on an apiserver blip.
func TestDiscoveryServedChecker_TransientVsNotFound(t *testing.T) {
	cs := kubefake.NewSimpleClientset()
	fd := cs.Discovery().(*fakediscovery.FakeDiscovery)
	fd.Resources = []*metav1.APIResourceList{{
		GroupVersion: "example.io/v1",
		APIResources: []metav1.APIResource{{Name: "widgets"}},
	}}

	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "missing.io"}, "v1")
	d := &fakeDiscoveryWithErr{DiscoveryInterface: fd, errs: map[string]error{
		"missing.io/v1": notFound,
		"flaky.io/v1":   assert.AnError,
	}}

	checker := NewDiscoveryServedChecker(context.Background(), d, schemafetcher.NewMapFetcher(nil), slog.Default())

	assert.True(t, checker.IsServed("example.io/v1", "widgets"))
	assert.False(t, checker.IsServed("example.io/v1", "gadgets"))
	require.NoError(t, checker.TransientErr(), "served/unlisted answers are authoritative")

	assert.False(t, checker.IsServed("missing.io/v1", "widgets"))
	require.NoError(t, checker.TransientErr(), "NotFound is authoritative unserved, not transient")

	assert.False(t, checker.IsServed("flaky.io/v1", "widgets"))
	require.Error(t, checker.TransientErr(), "non-NotFound discovery errors must be recorded as transient")
}

// mustSchema parses a JSON OpenAPI v3 schema literal for the field-probe tests.
func mustSchema(t *testing.T, raw string) *spec.Schema {
	t.Helper()
	var s spec.Schema
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	return &s
}

// TestDiscoveryServedChecker_FieldServed pins the runtime SchemaFieldChecker:
// the plural resolves to its Kind via the same discovery answer IsServed
// memoizes, the schema comes from the fetcher at the RESOLVED group/version,
// and every fetch failure — including NotFound for a resource discovery says
// is served — errors out so the resolution fails instead of silently
// stripping (issue #59's fail-closed contract).
func TestDiscoveryServedChecker_FieldServed(t *testing.T) {
	cs := kubefake.NewSimpleClientset()
	fd := cs.Discovery().(*fakediscovery.FakeDiscovery)
	fd.Resources = []*metav1.APIResourceList{{
		GroupVersion: "gateway.example.io/v1",
		APIResources: []metav1.APIResource{{Name: "httproutes", Kind: "HTTPRoute"}},
	}}

	routeSchema := mustSchema(t, `{
		"type": "object",
		"properties": {
			"spec": {
				"type": "object",
				"properties": {
					"rules": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"filters": {
									"type": "array",
									"items": {
										"type": "object",
										"properties": {"requestMirror": {"type": "object"}}
									}
								}
							}
						}
					}
				}
			}
		}
	}`)
	fetcher := schemafetcher.NewMapFetcher(map[schema.GroupVersionKind]*spec.Schema{
		{Group: "gateway.example.io", Version: "v1", Kind: "HTTPRoute"}: routeSchema,
	})
	checker := NewDiscoveryServedChecker(context.Background(), fd, fetcher, slog.Default())

	served, err := checker.FieldServed("gateway.example.io/v1", "httproutes", "spec.rules.filters.requestMirror")
	require.NoError(t, err)
	assert.True(t, served, "array levels must be descended transparently")

	served, err = checker.FieldServed("gateway.example.io/v1", "httproutes", "spec.rules.filters.cors")
	require.NoError(t, err)
	assert.False(t, served, "field absent from this schema generation")

	_, err = checker.FieldServed("gateway.example.io/v1", "widgets", "spec.rules")
	require.Error(t, err, "unknown plural must fail, not strip")

	// Served per discovery but schema missing from the fetcher: fail closed.
	fd.Resources[0].APIResources = append(fd.Resources[0].APIResources,
		metav1.APIResource{Name: "gadgets", Kind: "Gadget"})
	fresh := NewDiscoveryServedChecker(context.Background(), fd, fetcher, slog.Default())
	_, err = fresh.FieldServed("gateway.example.io/v1", "gadgets", "spec.rules")
	require.Error(t, err, "schema-fetch failure must fail the resolution, not silently strip")
}

func TestDiscoveryServedChecker_IsNamespaced(t *testing.T) {
	fd := kubefake.NewSimpleClientset().Discovery().(*fakediscovery.FakeDiscovery)
	fd.Resources = []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{Name: "secrets", Kind: "Secret", Namespaced: true},
			{Name: "namespaces", Kind: "Namespace"},
		},
	}}
	checker := NewDiscoveryServedChecker(context.Background(), fd, schemafetcher.NewMapFetcher(nil), slog.Default())

	assert.True(t, checker.IsNamespaced("v1", "secrets"))
	assert.False(t, checker.IsNamespaced("v1", "namespaces"))
	assert.False(t, checker.IsNamespaced("v1", "unserved"))
	_, implemented := any(checker).(coreconfig.NamespaceScopeChecker)
	assert.True(t, implemented, "ResolveEffective must see the scope of live resources")
}
