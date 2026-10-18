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

//go:build acceptance

package acceptance

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/pkg/types"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/compression"
)

// haproxyCfgKey carries the HAProxyCfg found by one assessment to the next.
const haproxyCfgKey ctxKey = "compression.haproxyCfg"

// haproxyCfgGVR returns the GroupVersionResource for HAProxyCfg.
func haproxyCfgGVR() schema.GroupVersionResource {
	return haproxyv1alpha1.SchemeGroupVersion.WithResource("haproxycfgs")
}

// buildCompressionFeature builds a feature that verifies the controller correctly compresses
// HAProxyCfg content when the compression threshold is set.
//
// This test validates:
//  1. Controller reads compressionThreshold from HAProxyTemplateConfig
//  2. HAProxyCfg is published with compressed content when threshold is exceeded
//  3. The compressed field is set to true in HAProxyCfg spec
//  4. Compressed content can be decompressed to valid HAProxy config
//
// Test flow:
//  1. Deploy controller with a low compression threshold (1 byte to force compression)
//  2. Wait for controller to complete reconciliation
//  3. Read the HAProxyCfg CRD
//  4. Verify spec.compressed is true
//  5. Decompress content and verify it's valid HAProxy config
func buildCompressionFeature() types.Feature {
	return features.New("Compression").
		Setup(setupCompression).
		Assess("HAProxyCfg is compressed", assessHAProxyCfgCompressed).
		Assess("Compressed content can be decompressed", assessCompressedContentDecompresses).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			t.Log("Cleaning up compression test resources")

			client, err := cfg.NewClient()
			if err != nil {
				t.Log("Failed to create client:", err)
				return ctx
			}

			return CleanupControllerEnvironment(ctx, t, client)
		}).
		Feature()
}

func setupCompression(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	t.Log("Setting up compression test")

	// Generate unique namespace for this test
	namespace := envconf.RandomName("test-compress", 16)
	t.Logf("Using test namespace: %s", namespace)

	ctx = StoreNamespaceInContext(ctx, namespace)

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	// Create base environment with SkipCRDAndDeployment so we can create custom config
	opts := ControllerEnvironmentOptions{
		Replicas:             1,
		CRDName:              ControllerCRDName,
		EnableLeaderElection: false,
		SkipCRDAndDeployment: true, // We'll create our own CRD with compression threshold
	}
	if err := CreateControllerEnvironment(ctx, t, client, namespace, opts); err != nil {
		t.Fatal("Failed to create base environment:", err)
	}

	// Create HAProxyTemplateConfig with a low compression threshold to force compression
	// Setting threshold to 1 means any content larger than 1 byte will be compressed
	htplConfig := NewHAProxyTemplateConfigBuilder(namespace, ControllerCRDName, ControllerSecretName).
		WithCompressionThreshold(1). // Force compression for any content
		Build()
	if err := client.Resources().Create(ctx, htplConfig); err != nil {
		t.Fatal("Failed to create HAProxyTemplateConfig:", err)
	}
	t.Log("Created HAProxyTemplateConfig with compression threshold 1")

	// Create deployment and services
	deployment := NewControllerDeployment(namespace, ControllerCRDName, ControllerSecretName, ControllerServiceAccountName, DebugPort, 1)
	if err := client.Resources().Create(ctx, deployment); err != nil {
		t.Fatal("Failed to create deployment:", err)
	}
	if err := createControllerServices(ctx, client, namespace); err != nil {
		t.Fatal("Failed to create services:", err)
	}
	t.Log("Created controller deployment and services")

	// Wait for controller pod to be ready AND reconciliation to complete
	t.Log("Waiting for controller to complete startup reconciliation...")
	metricsClient, err := SetupMetricsAccess(ctx, client, Clientset(), namespace, DefaultClientSetupTimeout)
	if err != nil {
		t.Fatal("Failed to setup metrics access:", err)
	}

	pod, err := WaitForControllerReadyWithMetrics(ctx, client, namespace, metricsClient, DefaultPodReadyTimeout)
	if err != nil {
		t.Fatal("Controller did not become ready:", err)
	}
	t.Logf("Controller pod %s is ready with reconciliation completed", pod.Name)

	return ctx
}

func assessHAProxyCfgCompressed(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	t.Log("Verifying HAProxyCfg is compressed")

	namespace, err := GetNamespaceFromContext(ctx)
	if err != nil {
		t.Fatal("Failed to get namespace from context:", err)
	}

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	dynamicClient, err := getDynamicClient(client.RESTConfig())
	if err != nil {
		t.Fatal("Failed to create dynamic client:", err)
	}

	haproxyCfg := waitForHAProxyCfg(ctx, t, dynamicClient, namespace)
	if haproxyCfg == nil {
		DumpControllerLogsOnError(ctx, t, client, namespace)
		t.Fatal("HAProxyCfg was not created within timeout")
	}

	// Check if compressed field is true
	compressed, found, err := unstructured.NestedBool(haproxyCfg.Object, "spec", "compressed")
	if err != nil {
		t.Fatalf("Failed to read spec.compressed: %v", err)
	}
	if !found {
		// If field is not present, it defaults to false
		DumpControllerLogsOnError(ctx, t, client, namespace)
		t.Fatal("spec.compressed field not found in HAProxyCfg - compression may not be working")
	}

	if !compressed {
		// Check the content to understand what's happening
		content, _, _ := unstructured.NestedString(haproxyCfg.Object, "spec", "content")
		t.Logf("Content length: %d bytes", len(content))
		DumpControllerLogsOnError(ctx, t, client, namespace)
		t.Fatal("HAProxyCfg spec.compressed is false, expected true with compression threshold 1")
	}

	t.Log("✓ HAProxyCfg spec.compressed is true")

	return context.WithValue(ctx, haproxyCfgKey, haproxyCfg)
}

// waitForHAProxyCfg returns the first HAProxyCfg the controller publishes in
// namespace, or nil when none appears within a minute.
func waitForHAProxyCfg(ctx context.Context, t *testing.T, dynamicClient dynamic.Interface, namespace string) *unstructured.Unstructured {
	t.Helper()
	gvr := haproxyCfgGVR()
	interval := 2 * time.Second
	deadline := time.Now().Add(60 * time.Second)

	for time.Now().Before(deadline) {
		list, err := dynamicClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Logf("Failed to list HAProxyCfg resources: %v", err)
			time.Sleep(interval)
			continue
		}

		if len(list.Items) > 0 {
			haproxyCfg := &list.Items[0]
			t.Logf("Found HAProxyCfg: %s", haproxyCfg.GetName())
			return haproxyCfg
		}

		t.Log("Waiting for HAProxyCfg to be created...")
		time.Sleep(interval)
	}
	return nil
}

func assessCompressedContentDecompresses(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
	t.Helper()
	t.Log("Verifying compressed content can be decompressed")

	haproxyCfg, ok := ctx.Value(haproxyCfgKey).(*unstructured.Unstructured)
	if !ok {
		t.Fatal("HAProxyCfg not found in context")
	}

	// Get compressed content
	content, found, err := unstructured.NestedString(haproxyCfg.Object, "spec", "content")
	if err != nil || !found {
		t.Fatalf("Failed to read spec.content: %v", err)
	}

	t.Logf("Compressed content length: %d bytes", len(content))

	decompressed, err := compression.Decompress(content)
	if err != nil {
		t.Fatalf("Failed to decompress content: %v", err)
	}

	t.Logf("Decompressed content length: %d bytes", len(decompressed))
	t.Logf("Compression ratio: %.2f%%", float64(len(content))*100/float64(len(decompressed)))

	// Verify decompressed content is valid HAProxy config
	if decompressed == "" {
		t.Fatal("Decompressed content is empty")
	}

	// Check for expected HAProxy config sections
	expectedSections := []string{"global", "defaults", "frontend", "backend"}
	for _, section := range expectedSections {
		if !strings.Contains(decompressed, section) {
			t.Errorf("Decompressed content missing expected section: %s", section)
		} else {
			t.Logf("✓ Found expected section: %s", section)
		}
	}

	return ctx
}

// TestCompression runs the compression test.
func TestCompression(t *testing.T) {
	testEnv.Test(t, buildCompressionFeature())
}
