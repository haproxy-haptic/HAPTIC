//go:build gateway_conformance

// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package conformance

import (
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	conformancetests "sigs.k8s.io/gateway-api/conformance/tests"
	"sigs.k8s.io/gateway-api/pkg/features"
)

func TestMixedTLSConformanceFeatures(t *testing.T) {
	for _, channel := range []string{"standard", "experimental"} {
		t.Run(channel, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, apiextensionsv1.AddToScheme(scheme))
			crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{
				Name:        "httproutes.gateway.networking.k8s.io",
				Annotations: map[string]string{"gateway.networking.k8s.io/channel": channel},
			}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(crd).Build()
			supported := supportedConformanceFeatures(t, c)
			prerequisites := conformancetests.TLSRouteMixedTerminationSameNamespace.Features
			require.Contains(t, prerequisites, features.SupportTLSRouteModeMixed)
			require.Subset(t, supported.UnsortedList(), prerequisites)
		})
	}
}

func TestRetryCodesFeature(t *testing.T) {
	for _, name := range []features.FeatureName{"HTTPRouteRetry", "HTTPRouteRetryCodes"} {
		t.Run(string(name), func(t *testing.T) {
			available := []features.Feature{{Name: "HTTPRouteRetryConnectionError"}, {Name: name}}
			selected, err := retryCodesFeature(available)
			require.NoError(t, err)
			require.Equal(t, name, selected)
		})
	}
	t.Run("unknown name fails", func(t *testing.T) {
		_, err := retryCodesFeature([]features.Feature{{Name: "HTTPRouteRetryConnectionError"}})
		require.ErrorContains(t, err, "no recognized retry-codes feature")
	})
	t.Run("installed upstream registry", func(t *testing.T) {
		_, err := retryCodesFeature(features.AllFeatures.UnsortedList())
		require.NoError(t, err)
	})
}
