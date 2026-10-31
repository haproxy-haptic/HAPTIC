// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServedCustomResourcesKeepsServedHAPTICKindsOnly(t *testing.T) {
	kinds := servedKinds("haproxycfgs   haproxy-haptic.org/v1alpha1   true   HAProxyCfg\n" +
		"haproxytemplateconfigs   htplcfg   haproxy-haptic.org/v1alpha1   true   HAProxyTemplateConfig\n")
	manifest := []byte(`apiVersion: v1
kind: ConfigMap
metadata: {name: c}
---
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata: {name: config}
spec: {dataplane: {secretsDir: /etc/haproxy/secrets}}
---
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateLibrary
metadata: {name: library}
`)

	list, err := servedCustomResources(manifest, kinds)
	require.NoError(t, err)
	var decoded struct {
		Kind  string
		Items []struct{ Kind string }
	}
	require.NoError(t, json.Unmarshal(list, &decoded))
	assert.Equal(t, "List", decoded.Kind)
	require.Len(t, decoded.Items, 1)
	assert.Equal(t, "HAProxyTemplateConfig", decoded.Items[0].Kind)

	_, err = servedCustomResources([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\n"), kinds)
	require.Error(t, err)
}

func TestNewestStableIgnoresTheRequiredPrerelease(t *testing.T) {
	newest, err := newestStable([]string{"0.8.1", "0.10.0", "0.9.0", requiredBaseline})
	require.NoError(t, err)
	assert.Equal(t, "0.10.0", newest)
}
