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
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"sigs.k8s.io/e2e-framework/pkg/types"
)

// TestAllAcceptanceParallel runs all acceptance tests in parallel with controlled concurrency.
// Each test runs in its own isolated namespace, sharing the same Kind cluster for efficiency.
//
// Usage:
//
//	go test -tags=acceptance -v -timeout 30m -run TestAllAcceptanceParallel ./tests/acceptance/...
//
// The PARALLEL environment variable controls the maximum number of concurrent tests:
//
//	PARALLEL=4 go test -tags=acceptance -v -timeout 30m -run TestAllAcceptanceParallel ./tests/acceptance/...
//
// By default, parallelism is set to GOMAXPROCS/2 to prevent resource exhaustion.
//
// Benefits:
// - Faster overall test execution (tests run concurrently)
// - Shared Kind cluster reduces setup time
// - Each test has namespace isolation (no cross-test interference)
// - Controlled parallelism prevents flakiness from resource contention
//
// Notes:
// - Each test creates and cleans up its own namespace
// - Cluster-scoped resources (ClusterRole, ClusterRoleBinding) use namespace-prefixed names
// - Tests are independent and should not rely on any shared state
func TestAllAcceptanceParallel(t *testing.T) {
	// Skip when running in CI sharding mode - individual tests run via matrix
	if os.Getenv("SKIP_PARALLEL_RUNNER") == "true" {
		t.Skip("Skipping parallel runner in CI sharding mode - tests run individually via matrix")
	}

	// Collect all feature builders
	features := []types.Feature{
		buildRetainedConfigurationFeature("Recovery", 0, (*retainedFleet).assessRecovery),
		buildRetainedConfigurationFeature("Recovery with 3200 watched inputs", 3200, (*retainedFleet).assessRecovery),
		buildRetainedConfigurationFeature("Sudden leader loss", 0, (*retainedFleet).assessSuddenRecovery),
		buildRetainedConfigurationFeature("Leader change right after a deployment", 1, (*retainedFleet).assessRecentDeployment),
		buildRetainedConfigurationFeature("Tampering", 0, (*retainedFleet).assessTampering),
		buildRetainedConfigurationFeature("Freshness", 0, (*retainedFleet).assessFreshness),
		buildWebhookCertHotRotationFeature(),
		// Error scenarios tests
		buildInvalidHAProxyConfigFeature(),
		buildBasicAuthInputIsolationFeature(),
		buildIngressInputIsolationFeature("annotation", "haproxy.org/auth-type", "invalid-auth-type"),
		buildIngressInputIsolationFeature("snippet", "haproxy-haptic.org/config-backend", "invalid-haproxy-directive"),
		buildFetchedListInputIsolationFeature(),
		buildCredentialsMissingFeature(),
		buildControllerCrashRecoveryFeature(),
		buildRapidConfigUpdatesFeature(),
		buildGracefulShutdownFeature(),
		buildDataplaneUnreachableFeature(),
		buildLeadershipDuringReconciliationFeature(),
		buildWatchReconnectionFeature(),
		buildTransactionConflictFeature(),
		buildPartialDeploymentFailureFeature(),
		buildValidationTestsGateFeature(),

		// Leader election tests
		buildLeaderElectionTwoReplicasFeature(),
		buildLeaderElectionFailoverFeature(),
		buildLeaderElectionDisabledModeFeature(),

		// HTTP store tests
		buildHTTPStoreValidUpdateFeature(),
		buildHTTPStoreInvalidUpdateFeature(),
		buildHTTPStoreFailoverFeature(),

		// Metrics tests
		buildMetricsFeature(),

		// Compression tests
		buildCompressionFeature(),

		// Debug endpoint tests
		buildHeapDumpFeature(),
	}

	// Calculate parallelism limit: GOMAXPROCS / 2 (minimum 1)
	// This prevents resource exhaustion on multi-core CI runners
	parallel := runtime.GOMAXPROCS(0) / 2
	if parallel < 1 {
		parallel = 1
	}

	// Allow override via PARALLEL env var
	if p := os.Getenv("PARALLEL"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			parallel = n
		}
	}

	t.Logf("Running %d features with parallelism limit: %d (GOMAXPROCS=%d)",
		len(features), parallel, runtime.GOMAXPROCS(0))

	// Run features with limited parallelism using a semaphore
	runFeaturesWithLimit(t, features, parallel)
}

// runFeaturesWithLimit executes features in parallel with a concurrency limit.
// It uses a semaphore (buffered channel) to control the number of concurrent goroutines.
func runFeaturesWithLimit(t *testing.T, features []types.Feature, limit int) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for _, f := range features {
		wg.Add(1)
		sem <- struct{}{} // Acquire semaphore slot

		go func(feature types.Feature) {
			defer wg.Done()
			defer func() { <-sem }() // Release semaphore slot

			testEnv.Test(t, feature)
		}(f)
	}

	wg.Wait()
}
