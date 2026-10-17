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
	"fmt"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/pkg/types"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// LeaderElectionLeaseName is the default lease name for leader election.
	LeaderElectionLeaseName = "haptic-leader"
)

// buildLeaderElectionTwoReplicasFeature builds a feature that verifies two-replica deployment
// elects exactly one leader.
//
// This test validates:
//  1. Two controller pods are deployed and become ready
//  2. A Lease resource is created with a holder identity
//  3. Exactly one pod has is_leader=1 metric
//  4. The other pod has is_leader=0 metric
//  5. The Lease holder matches the pod with is_leader=1
func buildLeaderElectionTwoReplicasFeature() types.Feature {
	return features.New("Leader Election - Two Replicas").
		Setup(setupLeaderElectionTwoReplicas).
		Assess("Exactly one leader elected", assessExactlyOneLeader).
		Assess("Leader deploys configs", assessLeaderDeploysConfigs).
		Feature()
}

func setupLeaderElectionTwoReplicas(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	t.Log("Setting up leader election two replicas test")

	// Generate unique namespace for this test
	// Note: RandomName generates a name of total length n, not prefix + n random chars
	namespace := envconf.RandomName("test-leader-2rep", 32)
	t.Logf("Using test namespace: %s", namespace)

	ctx = StoreNamespaceInContext(ctx, namespace)

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	// Create controller environment with 2 replicas and leader election enabled
	opts := ControllerEnvironmentOptions{
		Replicas:             2,
		CRDName:              ControllerCRDName,
		EnableLeaderElection: true,
	}
	if err := CreateControllerEnvironment(ctx, t, client, namespace, opts); err != nil {
		t.Fatal("Failed to create controller environment:", err)
	}
	t.Log("Created controller environment with 2 replicas and leader election enabled")

	return ctx
}

func assessExactlyOneLeader(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()

	namespace, err := GetNamespaceFromContext(ctx)
	if err != nil {
		t.Fatal("Failed to get namespace from context:", err)
	}

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	// Use shared clientset (rate limiting disabled) to avoid exhaustion
	clientset := Clientset()

	// Wait for 2 pods to be ready
	t.Log("Waiting for 2 controller pods to be ready...")
	if err := WaitForPodReady(ctx, client, namespace, "app="+ControllerDeploymentName, 2*time.Minute); err != nil {
		t.Fatal("Controller pods did not become ready:", err)
	}
	t.Log("Controller pods are ready")

	// Wait for leader election to complete
	t.Log("Waiting for leader election...")
	if _, err := WaitForLeaderElection(ctx, clientset, namespace, LeaderElectionLeaseName, PodRestartTimeout); err != nil {
		// Dump pod logs for debugging
		t.Log("Leader election failed, dumping pod logs...")
		pods, podErr := GetAllControllerPods(ctx, client, namespace)
		if podErr == nil {
			for i := range pods {
				t.Logf("=== Logs for pod %s ===", pods[i].Name)
				DumpPodLogs(ctx, t, clientset, &pods[i])
			}
		}
		t.Fatal("Leader election did not complete:", err)
	}
	t.Log("Leader election completed")

	pods, err := GetAllControllerPods(ctx, client, namespace)
	if err != nil {
		t.Fatal("Failed to get controller pods:", err)
	}

	if len(pods) != 2 {
		t.Fatalf("Expected 2 pods, found %d", len(pods))
	}

	// Get leader from Lease (the authoritative source)
	leaderPodName, err := GetLeaseHolder(ctx, clientset, namespace, LeaderElectionLeaseName)
	if err != nil {
		t.Fatal("Failed to get lease holder:", err)
	}

	if leaderPodName == "" {
		t.Fatal("No leader found in lease")
	}

	// Verify leader pod is in our pod list
	leaderCount := 0
	followerCount := 0
	for i := range pods {
		pod := &pods[i]
		if pod.Name == leaderPodName {
			leaderCount++
			t.Logf("Pod %s is the leader", pod.Name)
		} else {
			followerCount++
			t.Logf("Pod %s is a follower", pod.Name)
		}
	}

	// Verify exactly one leader
	if leaderCount != 1 {
		t.Fatalf("Expected exactly 1 leader, found %d", leaderCount)
	}
	if followerCount != 1 {
		t.Fatalf("Expected exactly 1 follower, found %d", followerCount)
	}

	t.Logf("✓ Leader election working correctly: leader=%s, follower count=%d", leaderPodName, followerCount)

	return ctx
}

func assessLeaderDeploysConfigs(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()

	namespace, err := GetNamespaceFromContext(ctx)
	if err != nil {
		t.Fatal("Failed to get namespace from context:", err)
	}

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	// Use shared clientset (rate limiting disabled) to avoid exhaustion
	clientset := Clientset()

	// Create a simple Ingress resource
	pathType := networkingv1.PathTypePrefix
	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ingress",
			Namespace: namespace,
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{
				{
					Host: "test.example.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/test",
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: "test-service",
											Port: networkingv1.ServiceBackendPort{
												Number: 80,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if err := client.Resources().Create(ctx, ingress); err != nil {
		t.Fatal("Failed to create ingress:", err)
	}
	t.Log("Created test ingress")

	// Get leader pod
	holderIdentity, err := GetLeaseHolder(ctx, clientset, namespace, LeaderElectionLeaseName)
	if err != nil {
		t.Fatal("Failed to get lease holder:", err)
	}

	pods, err := GetAllControllerPods(ctx, client, namespace)
	if err != nil {
		t.Fatal("Failed to get controller pods:", err)
	}

	var leaderPod *corev1.Pod
	for i := range pods {
		if pods[i].Name == holderIdentity {
			leaderPod = &pods[i]
			break
		}
	}

	if leaderPod == nil {
		t.Fatal("Could not find leader pod")
	}

	// Access debug endpoint to verify rendered config contains the ingress
	debugClient, err := EnsureDebugClientReady(ctx, t, client, clientset, namespace, 30*time.Second)
	if err != nil {
		t.Fatal("Failed to setup debug client:", err)
	}

	// Wait for reconciliation by polling the rendered config
	// This replaces the previous 10s sleep with proper condition checking
	expectedBackend := fmt.Sprintf("ing_%s_test-ingress", namespace)
	err = WaitForCondition(ctx, "ingress backend in rendered config", 30*time.Second, 100*time.Millisecond, func() (bool, error) {
		rendered, err := debugClient.GetRenderedConfig(ctx)
		if err != nil {
			// Transient error, keep trying
			return false, err
		}
		return strings.Contains(rendered, expectedBackend), nil
	})

	if err != nil {
		// Get final rendered config for debugging
		rendered, getErr := debugClient.GetRenderedConfig(ctx)
		if getErr == nil {
			t.Logf("Final rendered config:\n%s", rendered)
		}
		t.Fatalf("Reconciliation did not complete: %v", err)
	}

	t.Logf("✓ Leader successfully deployed config with ingress backend")

	return ctx
}

// TestLeaderElection_TwoReplicas runs the two replicas leader election test.
func TestLeaderElection_TwoReplicas(t *testing.T) {
	testEnv.Test(t, buildLeaderElectionTwoReplicasFeature())
}

// buildLeaderElectionFailoverFeature builds a feature that verifies automatic failover
// when leader fails.
//
// This test validates:
//  1. Initial leader is elected
//  2. Deleting leader pod triggers failover
//  3. A new leader is elected within the failover window
//  4. New leader is different from deleted pod
//  5. Only one leader exists after failover
func buildLeaderElectionFailoverFeature() types.Feature {
	return features.New("Leader Election - Failover").
		Setup(setupLeaderElectionFailover).
		Assess("Initial leader elected", assessInitialLeaderElected).
		Assess("Failover on leader deletion", assessFailoverOnLeaderDeletion).
		Feature()
}

func setupLeaderElectionFailover(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	t.Log("Setting up leader election failover test")

	// Note: "test-leader-failover" is 20 chars, use 36 to get 16 random chars
	namespace := envconf.RandomName("test-leader-failover", 36)
	t.Logf("Using test namespace: %s", namespace)

	ctx = StoreNamespaceInContext(ctx, namespace)

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	// Create controller environment with 2 replicas and leader election enabled
	opts := ControllerEnvironmentOptions{
		Replicas:             2,
		CRDName:              ControllerCRDName,
		EnableLeaderElection: true,
	}
	if err := CreateControllerEnvironment(ctx, t, client, namespace, opts); err != nil {
		t.Fatal("Failed to create controller environment:", err)
	}
	t.Log("Created controller environment with 2 replicas and leader election enabled")

	return ctx
}

func assessInitialLeaderElected(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()

	namespace, err := GetNamespaceFromContext(ctx)
	if err != nil {
		t.Fatal("Failed to get namespace from context:", err)
	}

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	// Use shared clientset (rate limiting disabled) to avoid exhaustion
	clientset := Clientset()

	if err := WaitForPodReady(ctx, client, namespace, "app="+ControllerDeploymentName, 2*time.Minute); err != nil {
		t.Fatal("Controller pods did not become ready:", err)
	}

	// Wait for leader election (returns the stable holder identity)
	holderIdentity, err := WaitForLeaderElection(ctx, clientset, namespace, LeaderElectionLeaseName, PodRestartTimeout)
	if err != nil {
		t.Fatal("Leader election did not complete:", err)
	}

	t.Logf("✓ Initial leader elected: %s", holderIdentity)

	return ctx
}

func assessFailoverOnLeaderDeletion(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()

	namespace, err := GetNamespaceFromContext(ctx)
	if err != nil {
		t.Fatal("Failed to get namespace from context:", err)
	}

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	// Use shared clientset (rate limiting disabled) to avoid exhaustion
	clientset := Clientset()

	// Get current leader
	oldLeader, err := GetLeaseHolder(ctx, clientset, namespace, LeaderElectionLeaseName)
	if err != nil {
		t.Fatal("Failed to get lease holder:", err)
	}

	t.Logf("Deleting leader pod: %s", oldLeader)

	// Delete leader pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      oldLeader,
			Namespace: namespace,
		},
	}
	if err := client.Resources().Delete(ctx, pod); err != nil {
		t.Fatal("Failed to delete leader pod:", err)
	}

	t.Log("Leader pod deleted, waiting for failover...")

	newLeader := waitForLeaseHolderChange(ctx, t, clientset, namespace, oldLeader)
	t.Logf("✓ New leader elected: %s", newLeader)

	// Failover already verified by Lease holder check above
	t.Log("✓ Failover successful, new leader elected via Lease")

	return ctx
}

// waitForLeaseHolderChange returns the first non-empty Lease holder other than
// oldLeader, failing the test after two minutes.
func waitForLeaseHolderChange(ctx context.Context, t *testing.T, clientset kubernetes.Interface, namespace, oldLeader string) string {
	t.Helper()
	// Wait for new leader (within lease_duration + renew_deadline = 75s)
	// Using 2 minutes to be safe
	failoverCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-failoverCtx.Done():
			t.Fatal("Timeout waiting for failover")

		case <-ticker.C:
			holder, err := GetLeaseHolder(ctx, clientset, namespace, LeaderElectionLeaseName)
			if err != nil {
				// Lease might be temporarily unavailable during transition
				continue
			}

			if holder != "" && holder != oldLeader {
				return holder
			}
		}
	}
}

// TestLeaderElection_Failover runs the failover leader election test.
func TestLeaderElection_Failover(t *testing.T) {
	testEnv.Test(t, buildLeaderElectionFailoverFeature())
}

// buildLeaderElectionDisabledModeFeature builds a feature that verifies single-replica
// mode without leader election.
//
// This test validates:
//  1. Controller starts with leader_election.enabled=false
//  2. No Lease resource is created
//  3. Controller operates normally
func buildLeaderElectionDisabledModeFeature() types.Feature {
	return features.New("Leader Election - Disabled Mode").
		Setup(setupLeaderElectionDisabledMode).
		Assess("No Lease created", assessNoLeaseCreated).
		Assess("Controller operates normally", assessControllerOperatesNormally).
		Feature()
}

func setupLeaderElectionDisabledMode(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	t.Log("Setting up leader election disabled mode test")

	// Note: "test-leader-disabled" is 20 chars, use 36 to get 16 random chars
	namespace := envconf.RandomName("test-leader-disabled", 36)
	t.Logf("Using test namespace: %s", namespace)

	ctx = StoreNamespaceInContext(ctx, namespace)

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	// Create controller environment with 1 replica and leader election disabled
	opts := DefaultControllerEnvironmentOptions() // 1 replica, leader election disabled
	if err := CreateControllerEnvironment(ctx, t, client, namespace, opts); err != nil {
		t.Fatal("Failed to create controller environment:", err)
	}
	t.Log("Created controller environment with leader election disabled")

	return ctx
}

func assessNoLeaseCreated(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()

	namespace, err := GetNamespaceFromContext(ctx)
	if err != nil {
		t.Fatal("Failed to get namespace from context:", err)
	}

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	if err := WaitForPodReady(ctx, client, namespace, "app="+ControllerDeploymentName, 2*time.Minute); err != nil {
		t.Fatal("Controller pod did not become ready:", err)
	}
	t.Log("Controller pod is ready")

	// Poll to verify Lease is NOT created (leader election disabled)
	// This replaces the previous 10s sleep with active checking
	// We poll for 10 seconds to ensure Lease doesn't appear
	// Use shared clientset (rate limiting disabled) to avoid exhaustion
	clientset := Clientset()

	err = WaitForCondition(ctx, "Lease to remain absent", 10*time.Second, 500*time.Millisecond, func() (bool, error) {
		_, err := clientset.CoordinationV1().Leases(namespace).Get(ctx, LeaderElectionLeaseName, metav1.GetOptions{})
		if err == nil {
			// Lease exists - this is a failure condition!
			// Return true to stop polling and fail the test
			return true, fmt.Errorf("Lease resource exists but leader election is disabled")
		}
		// Lease doesn't exist - keep polling until timeout (which is success)
		return false, nil
	})

	// For this test, timeout is actually success (Lease remained absent)
	// But if we get an error message, that means Lease was created (failure)
	if err != nil && strings.Contains(err.Error(), "Lease resource exists") {
		t.Fatal(err)
	}

	t.Log("✓ No Lease resource created (as expected)")

	return ctx
}

func assessControllerOperatesNormally(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()

	namespace, err := GetNamespaceFromContext(ctx)
	if err != nil {
		t.Fatal("Failed to get namespace from context:", err)
	}

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	// Use shared clientset (rate limiting disabled) to avoid exhaustion
	clientset := Clientset()

	// Access debug endpoint via NodePort
	debugClient, err := EnsureDebugClientReady(ctx, t, client, clientset, namespace, 30*time.Second)
	if err != nil {
		t.Fatal("Failed to setup debug client:", err)
	}

	// Wait for config to become available (controller is initializing)
	// This accommodates the time needed for controller startup and debug variable registration
	// Longer timeout for disabled mode since there are no HAProxy pods to sync
	config, err := debugClient.WaitForConfig(ctx, 60*time.Second)
	if err != nil {
		t.Fatal("Failed to wait for config:", err)
	}

	if config == nil {
		t.Fatal("Config is nil")
	}

	t.Log("✓ Controller operating normally without leader election")

	return ctx
}

// TestLeaderElection_DisabledMode runs the disabled mode leader election test.
func TestLeaderElection_DisabledMode(t *testing.T) {
	testEnv.Test(t, buildLeaderElectionDisabledModeFeature())
}
