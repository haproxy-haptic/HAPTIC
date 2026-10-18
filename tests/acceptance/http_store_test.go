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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/pkg/types"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

// setupHTTPStoreEnv returns a Setup step that deploys the blocklist server and
// a controller whose HTTP Store fetches from it, in a fresh namespace named
// after prefix.
func setupHTTPStoreEnv(prefix string, leaderElection bool, replicas int32) types.StepFunc {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		t.Helper()
		namespace := envconf.RandomName(prefix, 32)
		ctx = StoreNamespaceInContext(ctx, namespace)
		t.Logf("Test namespace: %s", namespace)

		client, err := cfg.NewClient()
		require.NoError(t, err)

		// Create base environment (namespace, RBAC, secrets) but skip CRD and deployment
		opts := DefaultControllerEnvironmentOptions()
		opts.SkipCRDAndDeployment = true
		if err := CreateControllerEnvironment(ctx, t, client, namespace, opts); err != nil {
			t.Fatal("Failed to create base controller environment:", err)
		}

		// Create blocklist server resources with valid content
		err = SetupBlocklistServer(ctx, t, client, namespace, ValidBlocklistContent)
		require.NoError(t, err)

		crd := NewHTTPStoreHAProxyTemplateConfig(namespace, ControllerCRDName, ControllerSecretName, leaderElection)
		require.NoError(t, client.Resources().Create(ctx, crd))

		deployment := NewControllerDeployment(namespace, ControllerCRDName, ControllerSecretName, ControllerServiceAccountName, DebugPort, replicas)
		require.NoError(t, client.Resources().Create(ctx, deployment))

		// Create debug and metrics services
		debugSvc := NewDebugService(namespace, ControllerDeploymentName, DebugPort)
		require.NoError(t, client.Resources().Create(ctx, debugSvc))

		metricsSvc := NewMetricsService(namespace, ControllerDeploymentName, MetricsPort)
		require.NoError(t, client.Resources().Create(ctx, metricsSvc))

		return ctx
	}
}

// buildHTTPStoreValidUpdateFeature builds a feature that tests valid HTTP content
// updates are applied correctly.
// Steps:
// 1. Deploy blocklist server with valid IP content
// 2. Deploy controller with HTTP Store template
// 3. Verify initial blocklist content is fetched and used
// 4. Update blocklist content with new valid IPs
// 5. Wait for refresh interval
// 6. Verify HAProxy config updated with new IPs.
func buildHTTPStoreValidUpdateFeature() types.Feature {
	return features.New("HTTP Store - Valid Update").
		Setup(setupHTTPStoreEnv("test-http-valid", false, 1)).
		Assess("Valid HTTP update is applied", assessHTTPStoreValidUpdate).
		Teardown(teardownControllerEnv).
		Feature()
}

func assessHTTPStoreValidUpdate(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	namespace, client, clientset := readyControllerEnv(ctx, t, cfg)

	// Setup debug client via NodePort
	debugClient, err := EnsureDebugClientReady(ctx, t, client, clientset, namespace, 30*time.Second)
	require.NoError(t, err)

	// Wait for first reconciliation to complete (ensures config is loaded AND auxiliary files rendered)
	metricsClient, err := SetupMetricsAccess(ctx, client, clientset, namespace, 30*time.Second)
	require.NoError(t, err)
	_, err = WaitForControllerReadyWithMetrics(ctx, client, namespace, metricsClient, DefaultPodReadyTimeout)
	if err != nil {
		t.Logf("Controller not ready: %v", err)
		dumpControllerPodLogs(ctx, t, client, clientset, namespace)
		require.NoError(t, err, "Controller should complete first reconciliation")
	}

	// Wait for initial config to be loaded and auxiliary files to contain the initial blocklist
	t.Log("Waiting for initial blocklist content...")
	err = debugClient.WaitForAuxFileContains(ctx, "blocked-ips.acl", "192.168.1.0/24", 60*time.Second)
	if err != nil {
		t.Logf("Blocklist not found: %v", err)
		dumpControllerPodLogs(ctx, t, client, clientset, namespace)
	}
	require.NoError(t, err)
	t.Log("Initial blocklist content verified")

	// Verify the content also contains the other IP
	content, err := debugClient.GetGeneralFileContent(ctx, "blocked-ips.acl")
	require.NoError(t, err)
	if !strings.Contains(content, "10.0.0.0/8") {
		t.Logf("FAILURE: Initial blocklist content missing expected IP\nActual content:\n%s", content)
	}
	assert.Contains(t, content, "10.0.0.0/8", "Initial content should contain both IPs")

	// Update the blocklist content with new valid IPs
	t.Log("Updating blocklist content with new valid IPs...")
	err = UpdateBlocklistAndRestart(ctx, t, client, clientset, namespace, UpdatedBlocklistContent)
	require.NoError(t, err)

	requireNginxServing(ctx, t, clientset, namespace, "192.168.0.0/16")
	t.Log("Nginx content verified - now testing controller behavior")

	// Wait for the updated content to appear in auxiliary files
	t.Log("Waiting for updated blocklist content in controller...")
	err = debugClient.WaitForAuxFileContains(ctx, "blocked-ips.acl", "172.16.0.0/12", 60*time.Second)
	if err != nil {
		t.Logf("Updated content wait failed: %v, dumping controller logs...", err)
		dumpControllerPodLogs(ctx, t, client, clientset, namespace)
	}
	require.NoError(t, err)
	t.Log("Updated blocklist content verified")

	// Verify the new content contains both new IPs
	content, err = debugClient.GetGeneralFileContent(ctx, "blocked-ips.acl")
	require.NoError(t, err)
	if !strings.Contains(content, "192.168.0.0/16") {
		t.Logf("FAILURE: Updated blocklist content missing expected IP\nActual content:\n%s", content)
	}
	assert.Contains(t, content, "192.168.0.0/16", "Updated content should contain both new IPs")

	// Verify old content is gone
	assert.NotContains(t, content, "192.168.1.0/24", "Old content should be replaced")
	assert.NotContains(t, content, "10.0.0.0/8", "Old content should be replaced")

	return ctx
}

// TestHTTPStoreValidUpdate runs the valid HTTP Store update test.
func TestHTTPStoreValidUpdate(t *testing.T) {
	testEnv.Test(t, buildHTTPStoreValidUpdateFeature())
}

// buildHTTPStoreInvalidUpdateFeature builds a feature that tests invalid HTTP content
// is rejected and old content is preserved.
// Steps:
// 1. Deploy blocklist server with valid IP content
// 2. Deploy controller with HTTP Store template
// 3. Verify initial blocklist content is fetched and used
// 4. Update blocklist content with invalid content (non-IP values)
// 5. Wait for refresh interval and validation attempt
// 6. Verify HAProxy config still contains old valid content
// 7. Verify invalid content was rejected.
func buildHTTPStoreInvalidUpdateFeature() types.Feature {
	return features.New("HTTP Store - Invalid Update Cache Preservation").
		Setup(setupHTTPStoreEnv("test-http-invalid", false, 1)).
		Assess("Invalid HTTP update is rejected and old content preserved", assessHTTPStoreInvalidUpdate).
		Teardown(teardownControllerEnv).
		Feature()
}

func assessHTTPStoreInvalidUpdate(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	namespace, client, clientset := readyControllerEnv(ctx, t, cfg)

	// Setup debug client via NodePort
	debugClient, err := EnsureDebugClientReady(ctx, t, client, clientset, namespace, 30*time.Second)
	require.NoError(t, err)

	// Wait for first reconciliation to complete (ensures config is loaded AND auxiliary files rendered)
	metricsClient, err := SetupMetricsAccess(ctx, client, clientset, namespace, 30*time.Second)
	require.NoError(t, err)
	_, err = WaitForControllerReadyWithMetrics(ctx, client, namespace, metricsClient, DefaultPodReadyTimeout)
	require.NoError(t, err, "Controller should complete first reconciliation")

	// Wait for initial config to be loaded
	t.Log("Waiting for initial blocklist content...")
	err = debugClient.WaitForAuxFileContains(ctx, "blocked-ips.acl", "192.168.1.0/24", 60*time.Second)
	require.NoError(t, err)
	t.Log("Initial blocklist content verified")

	// Update the blocklist content with invalid content
	t.Log("Updating blocklist content with invalid content...")
	err = UpdateBlocklistAndRestart(ctx, t, client, clientset, namespace, InvalidBlocklistContent)
	require.NoError(t, err)
	t.Log("Blocklist server restarted with invalid content")

	// Wait for the refresh to happen and verify old valid content is preserved
	// The invalid content should be rejected by HAProxy validation
	t.Log("Waiting for validation to reject invalid content...")
	waitCfg := testutil.SlowWaitConfig()
	waitCfg.Timeout = 30 * time.Second
	err = testutil.WaitForConditionWithDescription(ctx, waitCfg, "invalid content rejected",
		func(ctx context.Context) (bool, error) {
			content, err := debugClient.GetGeneralFileContent(ctx, "blocked-ips.acl")
			if err != nil {
				return false, err
			}
			// Old content should still be present (invalid update was rejected)
			hasOldContent := strings.Contains(content, "192.168.1.0/24") &&
				strings.Contains(content, "10.0.0.0/8")
			// Invalid content should NOT be present
			hasInvalidContent := strings.Contains(content, "not-an-ip-address") ||
				strings.Contains(content, "invalid-cidr")
			return hasOldContent && !hasInvalidContent, nil
		})
	require.NoError(t, err, "Invalid content should be rejected")

	// Dump controller logs to understand what happened
	t.Log("Dumping controller logs after wait...")
	pod, podErr := GetControllerPod(ctx, client, namespace)
	if podErr == nil {
		DumpPodLogs(ctx, t, clientset, pod)
	}

	// Verify the old valid content is still present (not replaced with invalid content)
	content, err := debugClient.GetGeneralFileContent(ctx, "blocked-ips.acl")
	require.NoError(t, err)

	// Old content should still be present
	if !strings.Contains(content, "192.168.1.0/24") || !strings.Contains(content, "10.0.0.0/8") {
		t.Logf("FAILURE: Old valid content not preserved after invalid update rejection\nActual content:\n%s", content)
	}
	assert.Contains(t, content, "192.168.1.0/24", "Old valid content should be preserved")
	assert.Contains(t, content, "10.0.0.0/8", "Old valid content should be preserved")

	// Invalid content should NOT be present
	assert.NotContains(t, content, "not-an-ip-address", "Invalid content should not be applied")
	assert.NotContains(t, content, "invalid-cidr", "Invalid content should not be applied")

	// Verify content remains stable for a period (confirming rejection was permanent)
	t.Log("Verifying content remains stable...")
	err = debugClient.WaitForAuxFileContentStable(ctx, "blocked-ips.acl", "192.168.1.0/24", 10*time.Second)
	require.NoError(t, err)
	t.Log("Content stability verified - invalid update was rejected")

	// Check controller logs for validation failure message
	if pod != nil {
		logs, logsErr := GetPodLogs(ctx, clientset, pod, 100)
		if logsErr == nil {
			if strings.Contains(logs, "validation failed") || strings.Contains(logs, "invalid") {
				t.Log("Controller logs show validation failure (expected)")
			}
		}
	}

	return ctx
}

// TestHTTPStoreInvalidUpdate runs the invalid HTTP Store update test.
func TestHTTPStoreInvalidUpdate(t *testing.T) {
	testEnv.Test(t, buildHTTPStoreInvalidUpdateFeature())
}

// buildHTTPStoreFailoverFeature builds a feature that tests HTTP Store continues
// working after controller failover.
// Steps:
// 1. Deploy blocklist server with valid IP content
// 2. Deploy controller with 2 replicas and leader election
// 3. Wait for leader election
// 4. Verify initial blocklist content is fetched
// 5. Delete the leader pod
// 6. Wait for new leader election
// 7. Update blocklist content with new valid IPs
// 8. Verify new leader processes the update correctly.
func buildHTTPStoreFailoverFeature() types.Feature {
	return features.New("HTTP Store - Controller Failover").
		Setup(setupHTTPStoreEnv("test-http-failover", true, 2)).
		Assess("HTTP Store works after controller failover", assessHTTPStoreFailover).
		Teardown(teardownControllerEnv).
		Feature()
}

func assessHTTPStoreFailover(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	namespace, err := GetNamespaceFromContext(ctx)
	require.NoError(t, err)

	client, err := cfg.NewClient()
	require.NoError(t, err)

	// Use shared clientset (rate limiting disabled) to avoid exhaustion
	clientset := Clientset()

	// Wait for at least one controller pod to be ready
	err = WaitForPodReady(ctx, client, namespace, "app="+ControllerDeploymentName, DefaultTimeout)
	require.NoError(t, err)
	t.Log("At least one controller pod ready")

	// Wait for leader election to stabilise (returns a holder seen
	// across consecutive samples — avoids the empty-string window
	// that occurs when a leader briefly releases the lease during
	// startup churn).
	leaseName := "haptic-leader"
	leaderIdentity, err := WaitForLeaderElection(ctx, clientset, namespace, leaseName, 60*time.Second)
	require.NoError(t, err)
	t.Logf("Leader election complete: %s", leaderIdentity)

	// Wait for leader to complete first reconciliation (renderer is leader-only,
	// so aux files won't be available until reconciliation completes)
	metricsClient, err := SetupMetricsAccess(ctx, client, clientset, namespace, 30*time.Second)
	require.NoError(t, err)
	_, err = WaitForControllerReadyWithMetrics(ctx, client, namespace, metricsClient, DefaultPodReadyTimeout)
	require.NoError(t, err)
	t.Log("Leader completed first reconciliation")

	// Get leader pod to identify it for deletion
	allPods, err := GetAllControllerPods(ctx, client, namespace)
	require.NoError(t, err)

	leaderPod := leaderPodOf(allPods, leaderIdentity)
	require.NotNil(t, leaderPod, "Leader pod should be found")

	// Setup debug client via NodePort - this routes to the leader pod
	debugClient, err := EnsureDebugClientReady(ctx, t, client, clientset, namespace, 30*time.Second)
	require.NoError(t, err)

	// Verify initial content
	t.Log("Waiting for initial blocklist content on leader...")
	err = debugClient.WaitForAuxFileContains(ctx, "blocked-ips.acl", "192.168.1.0/24", 60*time.Second)
	require.NoError(t, err)
	t.Log("Initial blocklist content verified on leader")

	failOverLeader(ctx, t, client, namespace, leaseName, leaderIdentity, leaderPod.Name, metricsClient)

	// Verify new leader has the blocklist content via NodePort
	// (NodePort service routes to available pods, which now includes only the new leader)
	t.Log("Waiting for blocklist content on new leader...")
	err = debugClient.WaitForAuxFileContains(ctx, "blocked-ips.acl", "192.168.1.0/24", 60*time.Second)
	require.NoError(t, err)
	t.Log("Blocklist content verified on new leader")

	// Update blocklist content with new valid IPs
	t.Log("Updating blocklist content after failover...")
	err = UpdateBlocklistAndRestart(ctx, t, client, clientset, namespace, UpdatedBlocklistContent)
	require.NoError(t, err)

	requireNginxServing(ctx, t, clientset, namespace, "192.168.0.0/16")
	t.Log("Nginx content verified - now testing new leader behavior")

	// Verify new leader processes the update correctly
	t.Log("Waiting for updated blocklist content on new leader after failover...")
	err = debugClient.WaitForAuxFileContains(ctx, "blocked-ips.acl", "172.16.0.0/12", 60*time.Second)
	if err != nil {
		t.Logf("Updated content wait failed: %v, dumping all controller logs...", err)
		dumpFailoverLogs(ctx, t, client, clientset, namespace)
	}
	require.NoError(t, err)
	t.Log("HTTP Store update successful after failover (first IP verified)")

	// Verify the second IP is also present.
	// Note: We use WaitForAuxFileContains instead of GetGeneralFileContent because
	// with 2 replicas, the ClusterIP service can route to either leader or follower.
	// Only the leader has updated content, so a single GET might hit the follower
	// and return stale data. WaitForAuxFileContains retries until it hits the leader.
	err = debugClient.WaitForAuxFileContains(ctx, "blocked-ips.acl", "192.168.0.0/16", 60*time.Second)
	require.NoError(t, err, "Updated content should be fully applied after failover")

	return ctx
}

// requireNginxServing fails the test unless the blocklist server serves want
// within 30 seconds. Checking this before the controller separates an
// infrastructure fault (nginx serving wrong content) from a controller bug.
func requireNginxServing(ctx context.Context, t *testing.T, clientset kubernetes.Interface, namespace, want string) {
	t.Helper()
	t.Log("Verifying nginx is serving updated content...")
	err := WaitForNginxContent(ctx, clientset, namespace, want, 30*time.Second)
	if err != nil {
		t.Logf("INFRASTRUCTURE ISSUE: nginx not serving expected content after restart: %v", err)
		// Dump ConfigMap content for debugging
		blocklistCM, cmErr := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, BlocklistContentConfigMapName, metav1.GetOptions{})
		if cmErr == nil {
			t.Logf("ConfigMap content: %s", blocklistCM.Data["blocklist.txt"])
		}
	}
	require.NoError(t, err, "nginx should serve new content after restart - if this fails, it's an infrastructure issue, not a controller bug")
}

// leaderPodOf returns the pod whose name the Lease holder identity contains, or nil.
func leaderPodOf(pods []corev1.Pod, identity string) *corev1.Pod {
	for i := range pods {
		if strings.Contains(identity, pods[i].Name) {
			return &pods[i]
		}
	}
	return nil
}

// failOverLeader deletes the leader pod and waits until another replica leads
// and has completed its first reconciliation.
func failOverLeader(ctx context.Context, t *testing.T, client klient.Client, namespace, leaseName, leaderIdentity, leaderPodName string, metricsClient *MetricsClient) {
	t.Helper()
	clientset := Clientset()

	// Delete the leader pod to trigger failover
	t.Logf("Deleting leader pod %s to trigger failover...", leaderPodName)
	err := clientset.CoreV1().Pods(namespace).Delete(ctx, leaderPodName, metav1.DeleteOptions{})
	require.NoError(t, err)

	// Wait for new leader election (must be different from old leader)
	t.Log("Waiting for new leader election...")
	newLeaderIdentity, err := WaitForNewLeader(ctx, client, clientset,
		namespace, leaseName, leaderIdentity, PodRestartTimeout)
	require.NoError(t, err)
	t.Logf("New leader: %s", newLeaderIdentity)

	// Wait for new leader to complete first reconciliation (renderer is leader-only)
	t.Log("Waiting for new leader to complete reconciliation...")
	_, err = WaitForControllerReadyWithMetrics(ctx, client, namespace, metricsClient, FailoverReadyTimeout)
	require.NoError(t, err)
	t.Log("New leader completed reconciliation")
}

// dumpFailoverLogs dumps every controller pod, leader and follower, and the
// blocklist server.
func dumpFailoverLogs(ctx context.Context, t *testing.T, client klient.Client, clientset kubernetes.Interface, namespace string) {
	t.Helper()
	allPods, _ := GetAllControllerPods(ctx, client, namespace)
	for i := range allPods {
		DumpPodLogs(ctx, t, clientset, &allPods[i])
	}
	blocklistPods, _ := GetPodsByLabel(ctx, client, namespace, "app=blocklist-server")
	for i := range blocklistPods {
		DumpPodLogs(ctx, t, clientset, &blocklistPods[i])
	}
}

// TestHTTPStoreFailover runs the HTTP Store failover test.
func TestHTTPStoreFailover(t *testing.T) {
	testEnv.Test(t, buildHTTPStoreFailoverFeature())
}
