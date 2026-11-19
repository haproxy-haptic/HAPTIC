//go:build integration

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

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rekby/fixenv"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/client"
	"github.com/haproxy-haptic/HAPTIC/tests/kindutil"
)

// SharedCluster provides a package-scoped Kind cluster shared across all tests
// This fixture runs only once per test package and is kept by default for faster test iterations
// The cluster is automatically reused if it already exists
// Set KEEP_CLUSTER=false to force cleanup after tests.
func SharedCluster(env fixenv.Env) *KindCluster {
	return fixenv.CacheResult(env, func() (*fixenv.GenericResult[*KindCluster], error) {
		cluster, err := SetupKindCluster(&KindClusterConfig{
			Name: "haproxy-test",
		})
		if err != nil {
			return nil, fmt.Errorf("failed to setup kind cluster: %w", err)
		}

		return fixenv.NewGenericResultWithCleanup(cluster, func() {
			keepCluster := ShouldKeepCluster()
			if keepCluster == "true" {
				fmt.Printf("\n🔒 Keeping Kind cluster '%s' (KEEP_CLUSTER=true)\n", cluster.Name)
				fmt.Printf("🧹 To manually clean up: kind delete cluster --name=%s\n", cluster.Name)
				return
			}
			_ = cluster.Teardown()
		}), nil
	}, fixenv.CacheOptions{Scope: fixenv.ScopePackage})
}

// AgentImage builds the pod image once per run and loads it into the cluster.
// Package-scoped: every test's pod runs the same binary, and the build is the
// slowest step of the suite.
func AgentImage(env fixenv.Env) string {
	cluster := SharedCluster(env)

	return fixenv.CacheResult(env, func() (*fixenv.GenericResult[string], error) {
		tag, err := buildAndLoadAgentImage(cluster)
		if err != nil {
			return nil, err
		}
		fmt.Printf("✓ Agent image '%s' loaded into Kind cluster\n", tag)
		return fixenv.NewGenericResultWithCleanup(tag, func() {
			kindutil.RemoveImage(tag)
		}), nil
	}, fixenv.CacheOptions{Scope: fixenv.ScopePackage})
}

func generateSafeNamespaceName(testName string) string {
	const prefix = "test-"
	const suffixLength = 8
	const maxBaseLength = validation.DNS1123LabelMaxLength - len(prefix) - len("-") - suffixLength

	base := strings.ToLower(strings.ReplaceAll(testName, "/", "-"))
	// Hash the full name before truncation so tests with a common prefix can share a cluster.
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", base, time.Now().UnixNano())))
	suffix := hex.EncodeToString(hash[:])[:suffixLength]
	if len(base) > maxBaseLength {
		base = base[:maxBaseLength]
	}
	name := prefix + base + "-" + suffix
	if len(name) > validation.DNS1123LabelMaxLength {
		panic(fmt.Sprintf("BUG: generated namespace name '%s' exceeds %d characters (length: %d)",
			name, validation.DNS1123LabelMaxLength, len(name)))
	}
	return name
}

// TestNamespace keeps each test's namespace for inspection by default.
func TestNamespace(env fixenv.Env) *Namespace {
	cluster := SharedCluster(env)

	return fixenv.CacheResult(env, func() (*fixenv.GenericResult[*Namespace], error) {
		ns, err := cluster.CreateNamespace(generateSafeNamespaceName(env.T().Name()))
		if err != nil {
			return nil, fmt.Errorf("failed to create namespace: %w", err)
		}

		return fixenv.NewGenericResultWithCleanup(ns, func() {
			keepCluster := ShouldKeepCluster()
			if keepCluster == "true" {
				fmt.Printf("🔒 Keeping namespace '%s' (KEEP_CLUSTER=true)\n", ns.Name)
				return
			}
			_ = ns.Delete()
		}), nil
	})
}

// TestHAProxy provides a test-scoped HAProxy pod: the HAProxy container plus
// its agent, the topology the chart deploys.
// Automatically depends on TestNamespace fixture (which depends on SharedCluster)
// HAProxy instances are kept by default for faster test iterations
// Set KEEP_CLUSTER=false to force cleanup after tests.
func TestHAProxy(env fixenv.Env) *HAProxyInstance {
	// Automatic dependency chain: TestNamespace -> SharedCluster
	ns := TestNamespace(env)
	image := AgentImage(env)

	return fixenv.CacheResult(env, func() (*fixenv.GenericResult[*HAProxyInstance], error) {
		haproxy, err := DeployHAProxy(ns, DefaultHAProxyConfig(image))
		if err != nil {
			return nil, fmt.Errorf("failed to deploy haproxy: %w", err)
		}

		// Register cleanup to dump container logs on test failure
		// This runs after test completion, so logs include all activity
		// Type assert to *testing.T to access Failed() method
		if t, ok := env.T().(*testing.T); ok {
			t.Cleanup(func() {
				haproxy.DumpLogsOnFailure(t)
			})
		}

		return fixenv.NewGenericResultWithCleanup(haproxy, func() {
			keepCluster := ShouldKeepCluster()
			if keepCluster == "true" {
				fmt.Printf("🔒 Keeping HAProxy instance '%s' in namespace '%s' (KEEP_CLUSTER=true)\n", haproxy.Name, haproxy.Namespace)
				return
			}
			_ = haproxy.Delete()
		}), nil
	})
}

// TestAgentClient provides the controller's end of the wire contract, pointed
// at the test pod's agent through the forwarded port.
// Automatically depends on TestHAProxy fixture.
func TestAgentClient(env fixenv.Env) *client.Client {
	// Automatic dependency chain: TestHAProxy -> TestNamespace -> SharedCluster
	haproxy := TestHAProxy(env)

	return fixenv.CacheResult(env, func() (*fixenv.GenericResult[*client.Client], error) {
		agentClient, err := client.New(&client.Config{
			BaseURL:            haproxy.AgentURL(),
			Username:           haproxy.AgentUser,
			Password:           haproxy.AgentPass,
			Timeout:            30 * time.Second,
			PerPodApplyTimeout: 2 * time.Minute,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create agent client: %w", err)
		}
		return fixenv.NewGenericResultWithCleanup(agentClient, agentClient.Close), nil
	})
}

// skipBelowHAProxy skips a test whose subject needs a later HAProxy release
// than the one under test. The version comes from HAPROXY_VERSION, which also
// selects the image, so the gate and the pod can never disagree.
func skipBelowHAProxy(t *testing.T, bound string) {
	t.Helper()
	if !haproxyAtLeast(bound) {
		t.Skipf("HAProxy %s is under test; this needs %s or later", HAProxyVersion(), bound)
	}
}

// haproxyAtLeast compares the version under test with a "major.minor" bound.
func haproxyAtLeast(bound string) bool {
	haveMajor, haveMinor := majorMinor(HAProxyVersion())
	wantMajor, wantMinor := majorMinor(bound)
	if haveMajor != wantMajor {
		return haveMajor > wantMajor
	}
	return haveMinor >= wantMinor
}

func majorMinor(version string) (major, minor int) {
	fields := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	major, _ = strconv.Atoi(leadingDigits(fields[0]))
	if len(fields) > 1 {
		minor, _ = strconv.Atoi(leadingDigits(fields[1]))
	}
	return major, minor
}

func leadingDigits(s string) string {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return s[:i]
		}
	}
	return s
}
