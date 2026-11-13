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

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

// TestHapticSharedRateLimit exercises the Phase-4 shared rate-limit path:
// native haproxy-haptic.org/rate-limit-* annotations, SPOE dispatch to the
// bundled rate-limit plugin, and a chart-deployed Valkey store. The core proof
// hits two HAProxy pod IPs directly from one in-cluster curl pod. A per-pod
// limiter with the same limit would allow all direct requests; the shared
// Valkey-backed budget returns 429 once the fleet-wide limit is exhausted. It
// also proves source-IP rejection runs before Coraza, consumer keys are applied
// after native authentication, Sentinel failover leaves limiting usable, and a
// total Valkey outage falls back to bounded per-sidecar budgets.
func TestHapticSharedRateLimit(t *testing.T) {
	RequireRateLimitProfile(t)
	// This test mutates the shard-wide Valkey StatefulSet and must stay serialized.

	runID := time.Now().UnixNano()
	host := fmt.Sprintf("rl-%d.localdev.me", runID)
	leaseHost := fmt.Sprintf("rl-lease-%d.localdev.me", runID)
	allowHost := fmt.Sprintf("rl-allow-%d.localdev.me", runID)
	failoverHost := fmt.Sprintf("rl-failover-%d.localdev.me", runID)
	consumerHost := fmt.Sprintf("rl-consumer-%d.localdev.me", runID)
	readinessHost := fmt.Sprintf("rl-ready-%d.localdev.me", runID)
	warmupHost := fmt.Sprintf("rl-warmup-%d.localdev.me", runID)
	outageLeaseHost := fmt.Sprintf("rl-outage-lease-%d.localdev.me", runID)
	outageExactHost := fmt.Sprintf("rl-outage-exact-%d.localdev.me", runID)
	consumerSecretName := fmt.Sprintf("rl-consumers-%d", runID)

	const (
		limit         = 5
		leaseLimit    = 2
		failoverLimit = 3
		warmupLimit   = 1000
		burstTotal    = 8
		leaseBurst    = 12
		failoverBurst = 5
	)
	scenario := &sharedRateLimitScenario{
		host:               host,
		leaseHost:          leaseHost,
		allowHost:          allowHost,
		failoverHost:       failoverHost,
		consumerHost:       consumerHost,
		readinessHost:      readinessHost,
		warmupHost:         warmupHost,
		outageLeaseHost:    outageLeaseHost,
		outageExactHost:    outageExactHost,
		consumerSecretName: consumerSecretName,
		limit:              limit,
		leaseLimit:         leaseLimit,
		failoverLimit:      failoverLimit,
		warmupLimit:        warmupLimit,
		burstTotal:         burstTotal,
		leaseBurst:         leaseBurst,
		failoverBurst:      failoverBurst,
	}

	feature := features.New("Ingress: HAPTIC shared rate-limit annotations").
		Setup(scenario.setup).
		Assess("exact and lease budgets are shared across HAProxy pods", scenario.sharedBudgets).
		Assess("total Valkey outage is bounded and isolated from HAProxy", scenario.outage).
		Feature()

	testEnv.Test(t, feature)
}

type sharedRateLimitScenario struct {
	host               string
	leaseHost          string
	allowHost          string
	failoverHost       string
	consumerHost       string
	readinessHost      string
	warmupHost         string
	outageLeaseHost    string
	outageExactHost    string
	consumerSecretName string
	limit              int
	leaseLimit         int
	failoverLimit      int
	warmupLimit        int
	burstTotal         int
	leaseBurst         int
	failoverBurst      int
}

func createSharedRateLimitIngresses(
	ctx context.Context,
	t *testing.T,
	client klient.Client,
	namespace string,
	specs ...IngressSpec,
) []*networkingv1.Ingress {
	t.Helper()
	ingresses := make([]*networkingv1.Ingress, 0, len(specs))
	for _, spec := range specs {
		ing := buildIngress(namespace, &spec)
		if err := client.Resources(namespace).Create(ctx, ing); err != nil {
			t.Fatalf("create Ingress %s/%s: %v", namespace, spec.Name, err)
		}
		ingresses = append(ingresses, ing)
	}
	return ingresses
}

func cleanupSharedRateLimitIngresses(t *testing.T, client klient.Client, namespace string, ingresses []*networkingv1.Ingress) {
	t.Helper()
	t.Cleanup(func() {
		bg := context.Background()
		for i := len(ingresses) - 1; i >= 0; i-- {
			ing := ingresses[i]
			if err := client.Resources(namespace).Delete(bg, ing); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("delete Ingress %s/%s: %v (best-effort)", namespace, ing.Name, err)
			}
		}
		waitForControllerForgetNamespace(bg, t, client, namespace)
	})
}

func listReadyHAProxyPodIPs(ctx context.Context, client klient.Client) ([]string, error) {
	var pods corev1.PodList
	if err := client.Resources(ControllerNamespace).List(ctx, &pods, resources.WithLabelSelector(LabelSelectorHAProxy)); err != nil {
		return nil, err
	}
	ips := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		pod := pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" || !podReady(&pod) {
			continue
		}
		ips = append(ips, pod.Status.PodIP)
	}
	sort.Strings(ips)
	return ips, nil
}

func routableHAProxyPodIPs(ctx context.Context, t *testing.T, client klient.Client, namespace, host string, minPods int) []string {
	t.Helper()
	waitCfg := testutil.WaitConfig{
		InitialInterval: 1 * time.Second,
		MaxInterval:     5 * time.Second,
		Timeout:         2 * time.Minute,
		Multiplier:      1.5,
	}
	var ips []string
	err := testutil.WaitForConditionWithDescription(ctx, waitCfg, "at least two HAProxy pod IPs serving readiness route",
		func(ctx context.Context) (bool, error) {
			candidates, err := listReadyHAProxyPodIPs(ctx, client)
			if err != nil {
				return false, err
			}
			if len(candidates) < minPods {
				return false, fmt.Errorf("need %d Ready HAProxy pod IPs, have %d", minPods, len(candidates))
			}
			routable, err := probeHAProxyPodRoute(ctx, namespace, host, candidates)
			if err != nil {
				return false, err
			}
			ips = routable
			if len(ips) >= minPods {
				return true, nil
			}
			return false, fmt.Errorf("need %d HAProxy pod IPs serving host %q, have %d from candidates %v",
				minPods, host, len(ips), candidates)
		})
	if err != nil {
		t.Fatalf("wait for routable HAProxy pod IPs: %v", err)
	}
	return ips
}

func deleteManagedRateLimitPrimary(ctx context.Context, t *testing.T, client klient.Client) {
	t.Helper()
	waitForManagedRateLimitStoreReady(ctx, t, client)
	primaryPod := currentManagedRateLimitPrimaryPod(ctx, t, client)
	primary := primaryPod.Name
	primaryUID := string(primaryPod.UID)
	t.Logf("deleting managed Valkey primary pod %s (uid %s) to exercise Sentinel failover", primary, primaryUID)

	blockManagedRateLimitStoreReplacementScheduling(ctx, t)
	t.Cleanup(func() {
		restoreManagedRateLimitStoreScheduling(context.Background(), t)
	})
	deleteCmd := exec.CommandContext(ctx, "kubectl",
		kubeconfigFlag, kubeconfigPath,
		"-n", ControllerNamespace,
		"delete", "pod", primary,
		"--grace-period=0",
		"--force",
		"--wait=false")
	var deleteErr bytes.Buffer
	deleteCmd.Stderr = &deleteErr
	if err := deleteCmd.Run(); err != nil {
		t.Fatalf("delete managed Valkey primary pod %s: %v\nstderr: %s", primary, err, deleteErr.String())
	}
	waitForManagedRateLimitPrimaryDeletionObserved(ctx, t, client, primary, primaryUID)
	waitForManagedRateLimitFailover(ctx, t, client, primary)
	restoreManagedRateLimitStoreScheduling(ctx, t)
	waitForManagedRateLimitStoreReady(ctx, t, client)
	newPrimary := currentManagedRateLimitPrimary(ctx, t, client)
	if newPrimary == primary {
		t.Fatalf("managed Valkey primary is still %s after forced deletion; Sentinel failover did not move the primary", primary)
	}
	t.Logf("managed Valkey primary after failover: %s", newPrimary)
}

func blockManagedRateLimitStoreReplacementScheduling(ctx context.Context, t *testing.T) {
	t.Helper()
	patch := `{"spec":{"updateStrategy":{"type":"OnDelete","rollingUpdate":null},"template":{"spec":{"nodeSelector":{"haproxy-haptic.org/e2e-unschedulable":"true"}}}}}`
	cmd := exec.CommandContext(ctx, "kubectl",
		kubeconfigFlag, kubeconfigPath,
		"-n", ControllerNamespace,
		"patch", "statefulset", rateLimitStoreName,
		"--type=merge",
		"-p", patch)
	var patchErr bytes.Buffer
	cmd.Stderr = &patchErr
	if err := cmd.Run(); err != nil {
		t.Fatalf("patch managed Valkey StatefulSet to block replacement scheduling without rolling existing pods: %v\nstderr: %s", err, patchErr.String())
	}
}

func restoreManagedRateLimitStoreScheduling(ctx context.Context, t *testing.T) {
	t.Helper()
	patch := `{"spec":{"updateStrategy":{"type":"RollingUpdate","rollingUpdate":{}},"template":{"spec":{"nodeSelector":null}}}}`
	cmd := exec.CommandContext(ctx, "kubectl",
		kubeconfigFlag, kubeconfigPath,
		"-n", ControllerNamespace,
		"patch", "statefulset", rateLimitStoreName,
		"--type=merge",
		"-p", patch)
	var patchErr bytes.Buffer
	cmd.Stderr = &patchErr
	if err := cmd.Run(); err != nil {
		t.Fatalf("restore managed Valkey StatefulSet scheduling: %v\nstderr: %s", err, patchErr.String())
	}
}

func waitForManagedRateLimitPrimaryDeletionObserved(ctx context.Context, t *testing.T, client klient.Client, name, uid string) {
	t.Helper()
	waitCfg := testutil.WaitConfig{
		InitialInterval: 500 * time.Millisecond,
		MaxInterval:     2 * time.Second,
		Timeout:         30 * time.Second,
		Multiplier:      1.5,
	}
	err := testutil.WaitForConditionWithDescription(ctx, waitCfg, "managed rate-limit primary pod deletion observed",
		func(ctx context.Context) (bool, error) {
			pods, err := listManagedRateLimitStorePods(ctx, client)
			if err != nil {
				return false, err
			}
			for i := range pods {
				pod := pods[i]
				if pod.Name != name || string(pod.UID) != uid {
					continue
				}
				if pod.DeletionTimestamp != nil || !podReady(&pod) {
					return true, nil
				}
				return false, fmt.Errorf("pod %s uid %s still Ready and not deleting", name, uid)
			}
			return true, nil
		})
	if err != nil {
		t.Fatalf("wait for managed Valkey primary pod deletion: %v", err)
	}
}

func waitForManagedRateLimitFailover(ctx context.Context, t *testing.T, client klient.Client, oldPrimary string) {
	t.Helper()
	waitCfg := testutil.WaitConfig{
		InitialInterval: 1 * time.Second,
		MaxInterval:     5 * time.Second,
		Timeout:         2 * time.Minute,
		Multiplier:      1.5,
	}
	err := testutil.WaitForConditionWithDescription(ctx, waitCfg, "managed rate-limit Valkey Sentinel failover",
		func(ctx context.Context) (bool, error) {
			pods, err := listManagedRateLimitStorePods(ctx, client)
			if err != nil {
				return false, err
			}
			ready := 0
			for i := range pods {
				if podReady(&pods[i]) {
					ready++
				}
			}
			if ready < 2 {
				return false, fmt.Errorf("need at least 2 Ready managed Valkey pods during failover, have %d", ready)
			}
			primary, err := findManagedRateLimitPrimary(ctx, pods)
			if err != nil {
				return false, err
			}
			if primary == oldPrimary {
				return false, fmt.Errorf("primary is still %s", oldPrimary)
			}
			return true, nil
		})
	if err != nil {
		t.Fatalf("wait for managed Valkey Sentinel failover away from %s: %v", oldPrimary, err)
	}
}

func waitForManagedRateLimitStoreReady(ctx context.Context, t *testing.T, client klient.Client) {
	t.Helper()
	if err := managedRateLimitStoreReady(ctx, client); err != nil {
		t.Fatalf("wait for managed rate-limit Valkey store: %v", err)
	}
}

func managedRateLimitStoreReady(ctx context.Context, client klient.Client) error {
	waitCfg := testutil.WaitConfig{
		InitialInterval: 1 * time.Second,
		MaxInterval:     5 * time.Second,
		Timeout:         2 * time.Minute,
		Multiplier:      1.5,
	}
	return testutil.WaitForConditionWithDescription(ctx, waitCfg, "managed rate-limit Valkey store ready",
		func(ctx context.Context) (bool, error) {
			pods, err := listManagedRateLimitStorePods(ctx, client)
			if err != nil {
				return false, err
			}
			if len(pods) < 3 {
				return false, fmt.Errorf("need 3 managed Valkey pods, have %d", len(pods))
			}
			ready := 0
			for i := range pods {
				if podReady(&pods[i]) {
					ready++
				}
			}
			if ready < 3 {
				return false, fmt.Errorf("need 3 Ready managed Valkey pods, have %d", ready)
			}
			if _, err := findManagedRateLimitPrimary(ctx, pods); err != nil {
				return false, err
			}
			return true, nil
		})
}

func managedRateLimitStoreReplicas(ctx context.Context, client klient.Client) (int32, error) {
	statefulSet := &appsv1.StatefulSet{}
	if err := client.Resources(ControllerNamespace).Get(ctx, rateLimitStoreName, ControllerNamespace, statefulSet); err != nil {
		return 0, fmt.Errorf("get StatefulSet %s/%s: %w", ControllerNamespace, rateLimitStoreName, err)
	}
	if statefulSet.Spec.Replicas == nil || *statefulSet.Spec.Replicas < 1 {
		return 0, fmt.Errorf("StatefulSet %s/%s has no positive replica count", ControllerNamespace, rateLimitStoreName)
	}
	return *statefulSet.Spec.Replicas, nil
}

func scaleManagedRateLimitStore(ctx context.Context, replicas int32) error {
	cmd := exec.CommandContext(ctx, "kubectl",
		kubeconfigFlag, kubeconfigPath,
		"-n", ControllerNamespace,
		"scale", "statefulset/"+rateLimitStoreName,
		fmt.Sprintf("--replicas=%d", replicas))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scale StatefulSet %s/%s to %d replicas: %w; output: %s",
			ControllerNamespace, rateLimitStoreName, replicas, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func waitForManagedRateLimitStoreScaledDown(ctx context.Context, client klient.Client) error {
	waitCfg := testutil.WaitConfig{
		InitialInterval: 500 * time.Millisecond,
		MaxInterval:     2 * time.Second,
		Timeout:         2 * time.Minute,
		Multiplier:      1.5,
	}
	return testutil.WaitForConditionWithDescription(ctx, waitCfg, "managed rate-limit Valkey store scaled down",
		func(ctx context.Context) (bool, error) {
			pods, err := listManagedRateLimitStorePods(ctx, client)
			if err != nil {
				return false, err
			}
			if len(pods) != 0 {
				return false, fmt.Errorf("%d managed Valkey pods remain", len(pods))
			}
			return true, nil
		})
}

func currentManagedRateLimitPrimary(ctx context.Context, t *testing.T, client klient.Client) string {
	t.Helper()
	return currentManagedRateLimitPrimaryPod(ctx, t, client).Name
}

func currentManagedRateLimitPrimaryPod(ctx context.Context, t *testing.T, client klient.Client) corev1.Pod {
	t.Helper()
	pods, err := listManagedRateLimitStorePods(ctx, client)
	if err != nil {
		t.Fatalf("list managed Valkey pods: %v", err)
	}
	primary, err := findManagedRateLimitPrimary(ctx, pods)
	if err != nil {
		t.Fatalf("find managed Valkey primary: %v", err)
	}
	for i := range pods {
		if pods[i].Name == primary {
			return pods[i]
		}
	}
	t.Fatalf("managed Valkey primary %s disappeared from listed pods", primary)
	return corev1.Pod{}
}

func listManagedRateLimitStorePods(ctx context.Context, client klient.Client) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := client.Resources(ControllerNamespace).List(ctx, &pods, resources.WithLabelSelector(labelSelectorRateLimitStore)); err != nil {
		return nil, err
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].Name < pods.Items[j].Name
	})
	return pods.Items, nil
}

func findManagedRateLimitPrimary(ctx context.Context, pods []corev1.Pod) (string, error) {
	var checked []string
	for i := range pods {
		pod := pods[i]
		if pod.DeletionTimestamp != nil || !podReady(&pod) {
			continue
		}
		checked = append(checked, pod.Name)
		roleCmd := exec.CommandContext(ctx, "kubectl",
			kubeconfigFlag, kubeconfigPath,
			"-n", ControllerNamespace,
			"exec", pod.Name,
			"-c", "valkey",
			"--", "valkey-cli", "-p", "6379", "role")
		var out, roleErr bytes.Buffer
		roleCmd.Stdout = &out
		roleCmd.Stderr = &roleErr
		if err := roleCmd.Run(); err != nil {
			return "", fmt.Errorf("read Valkey role from pod %s: %w; stderr: %s", pod.Name, err, roleErr.String())
		}
		if strings.HasPrefix(strings.TrimSpace(out.String()), "master") {
			return pod.Name, nil
		}
	}
	return "", fmt.Errorf("no Ready managed Valkey primary among pods %v", checked)
}

func waitForSharedRateLimitReadyOnPods(ctx context.Context, t *testing.T, namespace, host string, podIPs []string) {
	t.Helper()
	waitForSharedRateLimitVerdicts(ctx, t, "rate-limit plugin active on HAProxy pods", func(ctx context.Context) (bool, error) {
		return sharedRateLimitHeadersReady(ctx, namespace, host, podIPs)
	})
}

// waitForStoreBackedSharedRateLimitOnPods also rejects fallback verdicts, which
// carry the same headers: only a store-backed verdict proves a shared budget.
func waitForStoreBackedSharedRateLimitOnPods(ctx context.Context, t *testing.T, client klient.Client, namespace, host string, podIPs []string) {
	t.Helper()
	waitForSharedRateLimitVerdicts(ctx, t, "store-backed rate-limit verdicts on HAProxy pods", func(ctx context.Context) (bool, error) {
		before, err := rateLimitOutageSignalsByPod(ctx, client, podIPs)
		if err != nil {
			return false, err
		}
		if ready, err := sharedRateLimitHeadersReady(ctx, namespace, host, podIPs); !ready {
			return false, err
		}
		after, err := rateLimitOutageSignalsByPod(ctx, client, podIPs)
		if err != nil {
			return false, err
		}
		return storeBackedOnEveryPod(before, after, podIPs)
	})
}

func waitForSharedRateLimitVerdicts(ctx context.Context, t *testing.T, description string, ready func(context.Context) (bool, error)) {
	t.Helper()
	waitCfg := testutil.WaitConfig{
		InitialInterval: 1 * time.Second,
		MaxInterval:     5 * time.Second,
		Timeout:         2 * time.Minute,
		Multiplier:      1.5,
	}
	if err := testutil.WaitForConditionWithDescription(ctx, waitCfg, description, ready); err != nil {
		t.Fatalf("wait for %s: %v", description, err)
	}
}

func sharedRateLimitHeadersReady(ctx context.Context, namespace, host string, podIPs []string) (bool, error) {
	result, err := probeSharedRateLimitHeadersOnPods(ctx, namespace, host, podIPs)
	if err != nil {
		return false, err
	}
	if !result.ready {
		return false, fmt.Errorf("rate-limit headers not ready: %s", result.String())
	}
	return true, nil
}

func storeBackedOnEveryPod(before, after map[string]rateLimitOutageSignals, podIPs []string) (bool, error) {
	for _, ip := range podIPs {
		for _, outcome := range rateLimitFallbackOutcomes {
			if delta := after[ip].outcomes[outcome] - before[ip].outcomes[outcome]; delta > 0 {
				return false, fmt.Errorf("HAProxy pod %s answered from local fallback (%s +%v)", ip, outcome, delta)
			}
		}
		if delta := after[ip].outcomes["exact_allowed"] - before[ip].outcomes["exact_allowed"]; delta < 1 {
			return false, fmt.Errorf("HAProxy pod %s has store-backed exact_allowed delta %v", ip, delta)
		}
	}
	return true, nil
}

type sharedRateLimitReadinessProbeResult struct {
	ready bool
	lines []string
}

func (r sharedRateLimitReadinessProbeResult) String() string {
	if len(r.lines) == 0 {
		return "<no probe output>"
	}
	return strings.Join(r.lines, "; ")
}

func probeSharedRateLimitHeadersOnPods(ctx context.Context, namespace, host string, podIPs []string) (sharedRateLimitReadinessProbeResult, error) {
	var script strings.Builder
	script.WriteString("set -u; ")
	for _, ip := range podIPs {
		fmt.Fprintf(&script, `
headers=$(mktemp);
code=$(curl -s --connect-timeout 2 --max-time 5 -o /dev/null -D "$headers" -H "Host: %s" -w "%%{http_code}" http://%s:80/ || true);
limit=$(awk 'BEGIN{v=0} tolower($1)=="x-ratelimit-limit:"{v=1} END{print v}' "$headers");
remaining=$(awk 'BEGIN{v=0} tolower($1)=="x-ratelimit-remaining:"{v=1} END{print v}' "$headers");
reset=$(awk 'BEGIN{v=0} tolower($1)=="x-ratelimit-reset:"{v=1} END{print v}' "$headers");
echo "%s $code $limit $remaining $reset";
`, host, ip, ip)
	}

	podName := fmt.Sprintf("shared-ratelimit-ready-%d", time.Now().UnixNano())
	kubectlArgs := func(extra ...string) []string {
		return append([]string{kubeconfigFlag, kubeconfigPath, "-n", namespace}, extra...)
	}

	runCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(
		"run", podName,
		"--restart=Never",
		"--image=alpine/curl:latest",
		"--quiet",
		"--command", "--",
		"sh", "-c", script.String(),
	)...)
	var runErr bytes.Buffer
	runCmd.Stderr = &runErr
	if err := runCmd.Run(); err != nil {
		return sharedRateLimitReadinessProbeResult{}, fmt.Errorf("create rate-limit readiness probe pod: %w; stderr: %s", err, runErr.String())
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, "kubectl", kubectlArgs("delete", "pod", podName, "--now", "--ignore-not-found")...).Run()
	}()

	waitCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(
		"wait", "--for=jsonpath={.status.phase}=Succeeded",
		"pod/"+podName, "--timeout=30s")...)
	var waitErr bytes.Buffer
	waitCmd.Stderr = &waitErr
	if err := waitCmd.Run(); err != nil {
		return sharedRateLimitReadinessProbeResult{}, fmt.Errorf("wait for rate-limit readiness probe pod: %w; stderr: %s", err, waitErr.String())
	}

	var out, logsErr bytes.Buffer
	logsCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs("logs", podName)...)
	logsCmd.Stdout = &out
	logsCmd.Stderr = &logsErr
	if err := logsCmd.Run(); err != nil {
		return sharedRateLimitReadinessProbeResult{}, fmt.Errorf("read rate-limit readiness probe logs: %w; stderr: %s", err, logsErr.String())
	}

	result := sharedRateLimitReadinessProbeResult{ready: true}
	seen := 0
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		result.lines = append(result.lines, line)
		seen++
		fields := strings.Fields(line)
		if len(fields) != 5 || fields[1] != "200" || fields[2] != "1" || fields[3] != "1" || fields[4] != "1" {
			result.ready = false
		}
	}
	if seen != len(podIPs) {
		result.ready = false
		result.lines = append(result.lines, fmt.Sprintf("expected %d pod probe lines, got %d", len(podIPs), seen))
	}
	return result, nil
}

func probeHAProxyPodRoute(ctx context.Context, namespace, host string, podIPs []string) ([]string, error) {
	var script strings.Builder
	script.WriteString("set -u; ")
	for _, ip := range podIPs {
		fmt.Fprintf(&script,
			`code=$(curl -s --connect-timeout 2 --max-time 5 -o /dev/null -H "Host: %s" -w "%%{http_code}" http://%s:80/ || true); echo "%s $code"; `,
			host, ip, ip)
	}

	podName := fmt.Sprintf("shared-ratelimit-route-probe-%d", time.Now().UnixNano())
	kubectlArgs := func(extra ...string) []string {
		return append([]string{kubeconfigFlag, kubeconfigPath, "-n", namespace}, extra...)
	}

	runCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(
		"run", podName,
		"--restart=Never",
		"--image=alpine/curl:latest",
		"--quiet",
		"--command", "--",
		"sh", "-c", script.String(),
	)...)
	var runErr bytes.Buffer
	runCmd.Stderr = &runErr
	if err := runCmd.Run(); err != nil {
		return nil, fmt.Errorf("create route probe pod: %w; stderr: %s", err, runErr.String())
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, "kubectl", kubectlArgs("delete", "pod", podName, "--now", "--ignore-not-found")...).Run()
	}()

	waitCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(
		"wait", "--for=jsonpath={.status.phase}=Succeeded",
		"pod/"+podName, "--timeout=30s")...)
	var waitErr bytes.Buffer
	waitCmd.Stderr = &waitErr
	if err := waitCmd.Run(); err != nil {
		return nil, fmt.Errorf("wait for route probe pod: %w; stderr: %s", err, waitErr.String())
	}

	var out, logsErr bytes.Buffer
	logsCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs("logs", podName)...)
	logsCmd.Stdout = &out
	logsCmd.Stderr = &logsErr
	if err := logsCmd.Run(); err != nil {
		return nil, fmt.Errorf("read route probe logs: %w; stderr: %s", err, logsErr.String())
	}

	codes := map[string]string{}
	routable := make([]string, 0, len(podIPs))
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		codes[fields[0]] = fields[1]
		if fields[1] == "200" {
			routable = append(routable, fields[0])
		}
	}
	sort.Strings(routable)
	if len(routable) == 0 {
		return nil, fmt.Errorf("no HAProxy pod IP served host %q; candidates=%v codes=%v", host, podIPs, codes)
	}
	return routable, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

const (
	pluginRateLimitRequestsMetric = "plugin_rate_limit_ratelimit_requests_total"
	rateLimitDegradedMetric       = "haptic_degraded_rate_limit_total"
	rateLimitHubMetricsURL        = "http://127.0.0.1:9095/metrics"
	spoaHubRuntimeServer          = "spoa-hub/hub"
	haproxyForcedMaintenance      = 1
)

var rateLimitFallbackOutcomes = [...]string{
	"borrow_fallback_allowed",
	"borrow_fallback_limited",
	"exact_fallback_allowed",
	"exact_fallback_limited",
}

var trackedRateLimitOutcomes = [...]string{
	"borrow_fallback_allowed",
	"borrow_fallback_limited",
	"exact_fallback_allowed",
	"exact_fallback_limited",
	"exact_allowed",
	"exact_limited",
}

type rateLimitOutageSignals struct {
	outcomes map[string]float64
	degraded float64
}

func haproxyPodNameForIP(ctx context.Context, client klient.Client, podIP string) (string, error) {
	var pods corev1.PodList
	if err := client.Resources(ControllerNamespace).List(ctx, &pods, resources.WithLabelSelector(LabelSelectorHAProxy)); err != nil {
		return "", fmt.Errorf("list HAProxy pods: %w", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Status.PodIP == podIP {
			return pods.Items[i].Name, nil
		}
	}
	return "", fmt.Errorf("no HAProxy pod has IP %s", podIP)
}

func setSPOAHubRuntimeServerDisabled(ctx context.Context, pod string, disabled bool) error {
	action := "enable"
	if disabled {
		action = "disable"
	}
	commands := fmt.Sprintf("@1 %s server %s\\n", action, spoaHubRuntimeServer)
	if disabled {
		commands += fmt.Sprintf("@1 shutdown sessions server %s\\n", spoaHubRuntimeServer)
	}
	script := fmt.Sprintf("printf '%s' | socat - UNIX-CONNECT:/etc/haproxy/haproxy-master.sock", commands)
	out, err := execInHAProxyPod(ctx, pod, "haproxy", "sh", "-c", script)
	if err != nil {
		return err
	}
	if response := strings.TrimSpace(out); response != "" {
		return fmt.Errorf("HAProxy runtime returned %q", response)
	}
	return nil
}

func waitForSPOAHubRuntimeServerState(ctx context.Context, pod string, disabled bool) error {
	want := "enabled"
	if disabled {
		want = "disabled"
	}
	return testutil.WaitForConditionWithDescription(ctx, testutil.FastWaitConfig(),
		fmt.Sprintf("SPOA hub runtime server %s on pod %s", want, pod),
		func(ctx context.Context) (bool, error) {
			adminState, err := spoaHubRuntimeServerAdminState(ctx, pod)
			if err != nil {
				return false, err
			}
			isDisabled := adminState&haproxyForcedMaintenance != 0
			if isDisabled != disabled {
				return false, fmt.Errorf("server %s admin state is %d", spoaHubRuntimeServer, adminState)
			}
			return true, nil
		})
}

func spoaHubRuntimeServerAdminState(ctx context.Context, pod string) (int, error) {
	out, err := execInHAProxyPod(ctx, pod, "haproxy", "sh", "-c",
		"printf '@1 show servers state spoa-hub\\n' | socat - UNIX-CONNECT:/etc/haproxy/haproxy-master.sock")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || fields[0] == "#" || fields[1] != "spoa-hub" || fields[3] != "hub" {
			continue
		}
		adminState, err := strconv.Atoi(fields[6])
		if err != nil {
			return 0, fmt.Errorf("parse server %s admin state from %q: %w", spoaHubRuntimeServer, line, err)
		}
		return adminState, nil
	}
	return 0, fmt.Errorf("server %s is absent from HAProxy runtime state: %q", spoaHubRuntimeServer, strings.TrimSpace(out))
}

func rateLimitOutageSignalsByPod(ctx context.Context, client klient.Client, podIPs []string) (map[string]rateLimitOutageSignals, error) {
	targets := make(map[string]struct{}, len(podIPs))
	for _, ip := range podIPs {
		targets[ip] = struct{}{}
	}

	var pods corev1.PodList
	if err := client.Resources(ControllerNamespace).List(ctx, &pods, resources.WithLabelSelector(LabelSelectorHAProxy)); err != nil {
		return nil, fmt.Errorf("list HAProxy pods: %w", err)
	}

	signals := make(map[string]rateLimitOutageSignals, len(podIPs))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if _, ok := targets[pod.Status.PodIP]; !ok {
			continue
		}
		hubMetrics, err := execInHAProxyPod(ctx, pod.Name, "haproxy", "curl", "-fsS", rateLimitHubMetricsURL)
		if err != nil {
			return nil, fmt.Errorf("scrape SPOA hub metrics from pod %s: %w", pod.Name, err)
		}
		outcomes := make(map[string]float64, len(trackedRateLimitOutcomes))
		for _, outcome := range trackedRateLimitOutcomes {
			value, _, err := rateLimitOutcomeMetricValue(hubMetrics, outcome)
			if err != nil {
				return nil, fmt.Errorf("parse SPOA hub %s metrics from pod %s: %w", outcome, pod.Name, err)
			}
			outcomes[outcome] = value
		}
		vectorMetrics, err := execInHAProxyPod(ctx, pod.Name, "haproxy", "curl", "-fsS",
			fmt.Sprintf("http://127.0.0.1:%d/metrics", VectorMetricsPort))
		if err != nil {
			return nil, fmt.Errorf("scrape Vector metrics from pod %s: %w", pod.Name, err)
		}
		degraded, _, err := prometheusMetricValue(vectorMetrics, rateLimitDegradedMetric, "")
		if err != nil {
			return nil, fmt.Errorf("parse Vector metrics from pod %s: %w", pod.Name, err)
		}
		signals[pod.Status.PodIP] = rateLimitOutageSignals{
			outcomes: outcomes,
			degraded: degraded,
		}
		delete(targets, pod.Status.PodIP)
	}
	if len(targets) != 0 {
		missing := make([]string, 0, len(targets))
		for ip := range targets {
			missing = append(missing, ip)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("HAProxy pods disappeared before their rate-limit outage metrics were scraped: %v", missing)
	}
	return signals, nil
}

func rateLimitOutcomeMetricValue(body, outcome string) (value float64, seen bool, err error) {
	return prometheusMetricValue(body, pluginRateLimitRequestsMetric, fmt.Sprintf("outcome=%q", outcome))
}

func prometheusMetricValue(body, metric, requiredLabel string) (value float64, found bool, err error) {
	var total float64
	var seen bool
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || (fields[0] != metric && !strings.HasPrefix(fields[0], metric+"{")) ||
			(requiredLabel != "" && !strings.Contains(fields[0], requiredLabel)) {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return 0, false, fmt.Errorf("parse %s sample %q: %w", metric, line, err)
		}
		total += value
		seen = true
	}
	return total, seen, nil
}

type valkeyOutageProbe struct {
	target    string
	iteration int
	route     string
	code      string
	duration  time.Duration
	retry     bool
	limit     string
	remaining string
	reset     string
}

type valkeyOutageRoute struct {
	name                    string
	host                    string
	expectedCodes           []string
	requireRetryOnLimit     bool
	requireRateLimitHeaders bool
	forbidRateLimitHeaders  bool
	expectedLimit           string
	expectedRemaining       string
}

type valkeyOutageProbeResult struct {
	targets []string
	probes  []valkeyOutageProbe
	lines   []string
	routes  []valkeyOutageRoute
}

func (r valkeyOutageProbeResult) String() string {
	counts := map[string]map[string]int{}
	maximums := map[string]time.Duration{}
	for _, probe := range r.probes {
		key := probe.target + "/" + probe.route
		if counts[key] == nil {
			counts[key] = map[string]int{}
		}
		counts[key][probe.code]++
		if probe.duration > maximums[key] {
			maximums[key] = probe.duration
		}
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v max=%s", key, counts[key], maximums[key]))
	}
	return fmt.Sprintf("%s; raw=%q", strings.Join(parts, "; "), r.lines)
}

func (r *valkeyOutageProbeResult) validate(maxDuration time.Duration) error {
	expectedRoutes := make(map[string]valkeyOutageRoute, len(r.routes))
	for _, route := range r.routes {
		expectedRoutes[route.name] = route
	}
	seen := map[string]int{}
	for _, probe := range r.probes {
		route, known := expectedRoutes[probe.route]
		if !known {
			return fmt.Errorf("target %s returned unexpected route %q in iteration %d", probe.target, probe.route, probe.iteration)
		}
		if err := probe.validateRoute(&route, maxDuration); err != nil {
			return err
		}
		key := probe.target + "/" + probe.route
		seen[key]++
	}
	for _, target := range r.targets {
		for _, route := range r.routes {
			key := target + "/" + route.name
			if seen[key] != len(route.expectedCodes) {
				return fmt.Errorf("target %s route %s produced %d probes, want %d",
					target, route.name, seen[key], len(route.expectedCodes))
			}
		}
	}
	return nil
}

func (probe *valkeyOutageProbe) validateRoute(route *valkeyOutageRoute, maxDuration time.Duration) error {
	if probe.iteration < 1 || probe.iteration > len(route.expectedCodes) {
		return fmt.Errorf("target %s route %s returned unexpected iteration %d", probe.target, probe.route, probe.iteration)
	}
	expectedCode := route.expectedCodes[probe.iteration-1]
	if probe.code != expectedCode {
		return fmt.Errorf("target %s iteration %d route %s returned %s, want %s",
			probe.target, probe.iteration, probe.route, probe.code, expectedCode)
	}
	if probe.duration > maxDuration {
		return fmt.Errorf("target %s iteration %d route %s took %s, want at most %s",
			probe.target, probe.iteration, probe.route, probe.duration, maxDuration)
	}
	if route.requireRetryOnLimit && expectedCode == "429" && !probe.retry {
		return fmt.Errorf("target %s iteration %d route %s returned 429 without Retry-After", probe.target, probe.iteration, probe.route)
	}
	if route.requireRateLimitHeaders {
		if probe.limit != route.expectedLimit || probe.remaining != route.expectedRemaining {
			return fmt.Errorf("target %s iteration %d route %s returned X-RateLimit-Limit=%q and X-RateLimit-Remaining=%q, want %q and %q",
				probe.target, probe.iteration, probe.route, probe.limit, probe.remaining, route.expectedLimit, route.expectedRemaining)
		}
		reset, err := strconv.ParseUint(probe.reset, 10, 32)
		if err != nil || reset == 0 {
			return fmt.Errorf("target %s iteration %d route %s returned invalid X-RateLimit-Reset %q", probe.target, probe.iteration, probe.route, probe.reset)
		}
	}
	if route.forbidRateLimitHeaders && probe.hasRateLimitHeaders() {
		return fmt.Errorf("target %s iteration %d route %s unexpectedly returned X-RateLimit headers: limit=%q remaining=%q reset=%q",
			probe.target, probe.iteration, probe.route, probe.limit, probe.remaining, probe.reset)
	}
	return nil
}

func createValkeyOutageProbePod(ctx context.Context, t *testing.T, namespace string) string {
	t.Helper()
	podName := fmt.Sprintf("shared-ratelimit-outage-%d", time.Now().UnixNano())
	kubectlArgs := func(extra ...string) []string {
		return append([]string{kubeconfigFlag, kubeconfigPath, "-n", namespace}, extra...)
	}
	runCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(
		"run", podName,
		"--restart=Never",
		"--image=alpine/curl:latest",
		"--quiet",
		"--command", "--",
		"sleep", "3600",
	)...)
	var runErr bytes.Buffer
	runCmd.Stderr = &runErr
	if err := runCmd.Run(); err != nil {
		t.Fatalf("create managed Valkey outage probe pod: %v\nstderr: %s", err, runErr.String())
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, "kubectl", kubectlArgs("delete", "pod", podName, "--now", "--ignore-not-found")...).Run()
	})

	waitCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(
		"wait", "--for=condition=Ready",
		"pod/"+podName, "--timeout=30s")...)
	var waitErr bytes.Buffer
	waitCmd.Stderr = &waitErr
	if err := waitCmd.Run(); err != nil {
		t.Fatalf("wait for managed Valkey outage probe pod: %v\nstderr: %s", err, waitErr.String())
	}
	return podName
}

func probeValkeyOutageRoutes(
	ctx context.Context,
	t *testing.T,
	namespace, probePod string,
	routes []valkeyOutageRoute,
	podIPs []string,
) valkeyOutageProbeResult {
	t.Helper()
	if len(podIPs) < 2 {
		t.Fatalf("need at least two HAProxy pod IPs, got %v", podIPs)
	}

	if len(routes) == 0 {
		t.Fatal("need at least one managed Valkey outage route")
	}
	var script strings.Builder
	script.WriteString(`set -u; headers=$(mktemp); trap 'rm -f "$headers"' EXIT; `)
	targets := make([]string, 0, len(podIPs))
	for targetIndex, ip := range podIPs {
		target := fmt.Sprintf("P%d", targetIndex+1)
		targets = append(targets, target)
		for _, route := range routes {
			for iteration := range route.expectedCodes {
				fmt.Fprintf(&script, `: > "$headers"; result=$(curl -s --connect-timeout 1 --max-time 2 -o /dev/null -D "$headers" -H "Host: %s" -w "%%{http_code} %%{time_total}" http://%s:80/ || true); retry=$(awk 'BEGIN{v=0} tolower($1)=="retry-after:"{v=1} END{print v}' "$headers"); limit=$(awk 'BEGIN{v="-"} tolower($1)=="x-ratelimit-limit:"{gsub("\r","",$2);v=$2} END{print v}' "$headers"); remaining=$(awk 'BEGIN{v="-"} tolower($1)=="x-ratelimit-remaining:"{gsub("\r","",$2);v=$2} END{print v}' "$headers"); reset=$(awk 'BEGIN{v="-"} tolower($1)=="x-ratelimit-reset:"{gsub("\r","",$2);v=$2} END{print v}' "$headers"); echo "%s %d %s $result $retry $limit $remaining $reset"; `,
					route.host, ip, target, iteration+1, route.name)
			}
		}
	}
	out, err := execValkeyOutageProbeScript(ctx, namespace, probePod, script.String())
	if err != nil {
		t.Fatalf("run managed Valkey outage probes: %v", err)
	}

	result := valkeyOutageProbeResult{targets: targets, routes: routes}
	for _, raw := range strings.Split(strings.TrimSpace(out), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		result.lines = append(result.lines, line)
		probe, err := parseValkeyOutageProbe(line)
		if err != nil {
			t.Fatalf("parse managed Valkey outage probe: %v", err)
		}
		result.probes = append(result.probes, probe)
	}
	return result
}

func probeValkeyOutageRouteOnce(
	ctx context.Context,
	namespace, probePod, target, host, podIP string,
) (valkeyOutageProbe, error) {
	script := fmt.Sprintf(`headers=$(mktemp); trap 'rm -f "$headers"' EXIT; result=$(curl -s --connect-timeout 1 --max-time 2 -o /dev/null -D "$headers" -H "Host: %s" -w "%%{http_code} %%{time_total}" http://%s:80/ || true); retry=$(awk 'BEGIN{v=0} tolower($1)=="retry-after:"{v=1} END{print v}' "$headers"); limit=$(awk 'BEGIN{v="-"} tolower($1)=="x-ratelimit-limit:"{gsub("\r","",$2);v=$2} END{print v}' "$headers"); remaining=$(awk 'BEGIN{v="-"} tolower($1)=="x-ratelimit-remaining:"{gsub("\r","",$2);v=$2} END{print v}' "$headers"); reset=$(awk 'BEGIN{v="-"} tolower($1)=="x-ratelimit-reset:"{gsub("\r","",$2);v=$2} END{print v}' "$headers"); echo "%s 1 exact $result $retry $limit $remaining $reset"`,
		host, podIP, target)
	out, err := execValkeyOutageProbeScript(ctx, namespace, probePod, script)
	if err != nil {
		return valkeyOutageProbe{}, err
	}
	return parseValkeyOutageProbe(strings.TrimSpace(out))
}

func execValkeyOutageProbeScript(ctx context.Context, namespace, probePod, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl",
		kubeconfigFlag, kubeconfigPath,
		"-n", namespace,
		"exec", probePod,
		"--", "sh", "-c", script)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("exec in probe pod %s: %w; stderr: %s", probePod, err, stderr.String())
	}
	return out.String(), nil
}

func parseValkeyOutageProbe(line string) (valkeyOutageProbe, error) {
	fields := strings.Fields(line)
	if len(fields) != 9 {
		return valkeyOutageProbe{}, fmt.Errorf("line %q has %d fields, want 9", line, len(fields))
	}
	iteration, err := strconv.Atoi(fields[1])
	if err != nil {
		return valkeyOutageProbe{}, fmt.Errorf("parse iteration in %q: %w", line, err)
	}
	seconds, err := strconv.ParseFloat(fields[4], 64)
	if err != nil {
		return valkeyOutageProbe{}, fmt.Errorf("parse duration in %q: %w", line, err)
	}
	return valkeyOutageProbe{
		target:    fields[0],
		iteration: iteration,
		route:     fields[2],
		code:      fields[3],
		duration:  time.Duration(seconds * float64(time.Second)),
		retry:     fields[5] == "1",
		limit:     fields[6],
		remaining: fields[7],
		reset:     fields[8],
	}, nil
}

func (p *valkeyOutageProbe) hasRateLimitHeaders() bool {
	return p.limit != "-" || p.remaining != "-" || p.reset != "-"
}

func waitForAuthoritativeExactRateLimitRecovery(
	ctx context.Context,
	t *testing.T,
	client klient.Client,
	namespace, probePod, host string,
	podIPs []string,
	before map[string]rateLimitOutageSignals,
) {
	t.Helper()
	if len(podIPs) < 2 {
		t.Fatalf("need at least two HAProxy pod IPs, got %v", podIPs)
	}
	allowedProbe := waitForAuthoritativeExactAllow(ctx, t, client, namespace, probePod, host, podIPs, before)
	limitedProbe := waitForAuthoritativeExactLimit(ctx, t, client, namespace, probePod, host, podIPs, before)
	t.Logf("authoritative exact recovery probes: allow=%+v limit=%+v", allowedProbe, limitedProbe)
}

func waitForAuthoritativeExactAllow(
	ctx context.Context,
	t *testing.T,
	client klient.Client,
	namespace, probePod, host string,
	podIPs []string,
	before map[string]rateLimitOutageSignals,
) valkeyOutageProbe {
	t.Helper()
	var allowedProbe valkeyOutageProbe
	err := testutil.WaitForConditionWithDescription(ctx, testutil.FastWaitConfig(),
		"an authoritative exact allow after Valkey recovery",
		func(ctx context.Context) (bool, error) {
			probe, err := probeValkeyOutageRouteOnce(ctx, namespace, probePod, "P1", host, podIPs[0])
			if err != nil {
				return false, err
			}
			if probe.duration > time.Second {
				return false, fmt.Errorf("exact recovery allow probe took %s, want at most 1s", probe.duration)
			}
			if probe.code != "200" {
				return false, fmt.Errorf("exact recovery allow probe returned %s, waiting for Valkey", probe.code)
			}
			after, err := rateLimitOutageSignalsByPod(ctx, client, podIPs)
			if err != nil {
				return false, err
			}
			if delta := after[podIPs[0]].outcomes["exact_allowed"] - before[podIPs[0]].outcomes["exact_allowed"]; delta < 1 {
				return false, fmt.Errorf("HAProxy pod %s has authoritative exact_allowed delta %v", podIPs[0], delta)
			}
			allowedProbe = probe
			return true, nil
		})
	if err != nil {
		t.Fatalf("wait for authoritative exact allow after Valkey recovery: %v", err)
	}
	return allowedProbe
}

func waitForAuthoritativeExactLimit(
	ctx context.Context,
	t *testing.T,
	client klient.Client,
	namespace, probePod, host string,
	podIPs []string,
	before map[string]rateLimitOutageSignals,
) valkeyOutageProbe {
	t.Helper()
	var limitedProbe valkeyOutageProbe
	err := testutil.WaitForConditionWithDescription(ctx, testutil.FastWaitConfig(),
		"an authoritative fleet-wide exact limit after Valkey recovery",
		func(ctx context.Context) (bool, error) {
			probe, err := probeValkeyOutageRouteOnce(ctx, namespace, probePod, "P2", host, podIPs[1])
			if err != nil {
				return false, err
			}
			if probe.duration > time.Second {
				return false, fmt.Errorf("exact recovery limit probe took %s, want at most 1s", probe.duration)
			}
			if probe.code != "429" || !probe.retry {
				return false, fmt.Errorf("exact recovery limit probe returned code=%s Retry-After=%t, want 429 and Retry-After",
					probe.code, probe.retry)
			}
			limitedProbe = probe
			after, err := rateLimitOutageSignalsByPod(ctx, client, podIPs)
			if err != nil {
				return false, err
			}
			if delta := after[podIPs[1]].outcomes["exact_limited"] - before[podIPs[1]].outcomes["exact_limited"]; delta < 1 {
				return false, fmt.Errorf("HAProxy pod %s has authoritative exact_limited delta %v", podIPs[1], delta)
			}
			return true, nil
		})
	if err != nil {
		t.Fatalf("wait for authoritative exact limit after Valkey recovery: %v", err)
	}
	return limitedProbe
}

type sharedRateLimitBurstResult struct {
	requested int
	byTarget  map[string]int
	byCode    map[string]int
	lines     []string

	headerProbeCode  string
	headerRetryAfter bool
	headerLimit      bool
	headerRemaining  bool
	headerReset      bool
}

func (r sharedRateLimitBurstResult) String() string {
	type pair struct {
		k string
		v int
	}
	pairs := make([]pair, 0, len(r.byCode))
	for k, v := range r.byCode {
		pairs = append(pairs, pair{k: k, v: v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf("%d×%s", p.v, p.k))
	}
	return fmt.Sprintf("%d requests: %s; targets A=%d B=%d; headerProbe=%s retryAfter=%t limit=%t remaining=%t reset=%t; raw=%q",
		r.requested, strings.Join(parts, " / "), r.byTarget["A"], r.byTarget["B"],
		r.headerProbeCode, r.headerRetryAfter, r.headerLimit, r.headerRemaining, r.headerReset, r.lines)
}

func sharedRateLimitBurstAcrossPods(
	ctx context.Context,
	t *testing.T,
	namespace, host string,
	podIPs []string,
	total int,
	wafBlockProbe bool,
) sharedRateLimitBurstResult {
	t.Helper()
	if len(podIPs) < 2 {
		t.Fatalf("need at least two HAProxy pod IPs, got %v", podIPs)
	}

	var script strings.Builder
	script.WriteString("set -u; ")
	for i := 0; i < total; i++ {
		label := "A"
		ip := podIPs[0]
		if i%2 == 1 {
			label = "B"
			ip = podIPs[1]
		}
		fmt.Fprintf(&script,
			`code=$(curl -s --max-time 5 -o /dev/null -H "Host: %s" -w "%%{http_code}" http://%s:80/); echo "%s $code"; `,
			host, ip, label)
	}
	probeHeader := ""
	if wafBlockProbe {
		probeHeader = ` -H "User-Agent: haptic-waf-block-probe"`
	}
	fmt.Fprintf(&script, `
headers=$(mktemp);
code=$(curl -s --max-time 5 -o /dev/null -D "$headers" -H "Host: %s"%s -w "%%{http_code}" http://%s:80/);
retry_after=$(awk 'BEGIN{v=0} tolower($1)=="retry-after:"{v=1} END{print v}' "$headers");
limit=$(awk 'BEGIN{v=0} tolower($1)=="x-ratelimit-limit:"{v=1} END{print v}' "$headers");
remaining=$(awk 'BEGIN{v=0} tolower($1)=="x-ratelimit-remaining:"{v=1} END{print v}' "$headers");
reset=$(awk 'BEGIN{v=0} tolower($1)=="x-ratelimit-reset:"{v=1} END{print v}' "$headers");
echo "H $code $retry_after $limit $remaining $reset";
`,
		host, probeHeader, podIPs[0])

	podName := fmt.Sprintf("shared-ratelimit-burst-%d", time.Now().UnixNano())
	kubectlArgs := func(extra ...string) []string {
		return append([]string{kubeconfigFlag, kubeconfigPath, "-n", namespace}, extra...)
	}

	runCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(
		"run", podName,
		"--restart=Never",
		"--image=alpine/curl:latest",
		"--quiet",
		"--command", "--",
		"sh", "-c", script.String(),
	)...)
	var runErr bytes.Buffer
	runCmd.Stderr = &runErr
	if err := runCmd.Run(); err != nil {
		t.Fatalf("create shared rate-limit burst pod: %v\nstderr: %s", err, runErr.String())
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, "kubectl", kubectlArgs("delete", "pod", podName, "--now", "--ignore-not-found")...).Run()
	}()

	waitCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(
		"wait", "--for=jsonpath={.status.phase}=Succeeded",
		"pod/"+podName, "--timeout=45s")...)
	var waitErr bytes.Buffer
	waitCmd.Stderr = &waitErr
	if err := waitCmd.Run(); err != nil {
		t.Fatalf("wait for shared rate-limit burst pod: %v\nstderr: %s", err, waitErr.String())
	}

	var out, logsErr bytes.Buffer
	logsCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs("logs", podName)...)
	logsCmd.Stdout = &out
	logsCmd.Stderr = &logsErr
	if err := logsCmd.Run(); err != nil {
		t.Fatalf("read shared rate-limit burst logs: %v\nstderr: %s", err, logsErr.String())
	}

	return parseSharedRateLimitBurst(t, out.String(), total)
}

func parseSharedRateLimitBurst(t *testing.T, output string, total int) sharedRateLimitBurstResult {
	t.Helper()
	result := sharedRateLimitBurstResult{
		requested: total,
		byTarget:  map[string]int{},
		byCode:    map[string]int{},
	}
	for _, raw := range strings.Split(strings.TrimSpace(output), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		result.lines = append(result.lines, line)
		fields := strings.Fields(line)
		if len(fields) == 6 && fields[0] == "H" {
			result.headerProbeCode = fields[1]
			result.headerRetryAfter = fields[2] == "1"
			result.headerLimit = fields[3] == "1"
			result.headerRemaining = fields[4] == "1"
			result.headerReset = fields[5] == "1"
			continue
		}
		if len(fields) != 2 {
			result.byCode["unparsed"]++
			continue
		}
		result.byTarget[fields[0]]++
		result.byCode[fields[1]]++
	}
	if len(result.lines) != total+1 {
		t.Fatalf("expected %d burst status lines plus one header probe from shared rate-limit burst pod, got %d: %s",
			total, len(result.lines), result.String())
	}
	return result
}

// probeSharedConsumerRateLimits proves that consumer-keyed quotas dispatch
// after native API-key authentication. Both requests originate from one pod;
// an accidental source-IP fallback would make bob's first request a 429.
func probeSharedConsumerRateLimits(
	ctx context.Context,
	t *testing.T,
	namespace, host string,
	podIPs []string,
) map[string]string {
	t.Helper()
	if len(podIPs) < 2 {
		t.Fatalf("need at least two HAProxy pod IPs, got %v", podIPs)
	}

	var script strings.Builder
	script.WriteString("set -eu; ")
	probes := []struct {
		label, key, ip string
	}{
		{label: "alice-1", key: "key-alice", ip: podIPs[0]},
		{label: "alice-2", key: "key-alice", ip: podIPs[1]},
		{label: "bob-1", key: "key-bob", ip: podIPs[1]},
		{label: "bob-2", key: "key-bob", ip: podIPs[0]},
	}
	for _, probe := range probes {
		fmt.Fprintf(&script,
			`code=$(curl -s --max-time 5 -o /dev/null -H "Host: %s" -H "X-API-Key: %s" -w "%%{http_code}" http://%s:80/); echo "%s $code"; `,
			host, probe.key, probe.ip, probe.label)
	}

	podName := fmt.Sprintf("shared-ratelimit-consumers-%d", time.Now().UnixNano())
	kubectlArgs := func(extra ...string) []string {
		return append([]string{kubeconfigFlag, kubeconfigPath, "-n", namespace}, extra...)
	}
	runCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(
		"run", podName,
		"--restart=Never",
		"--image=alpine/curl:latest",
		"--quiet",
		"--command", "--",
		"sh", "-c", script.String(),
	)...)
	var runErr bytes.Buffer
	runCmd.Stderr = &runErr
	if err := runCmd.Run(); err != nil {
		t.Fatalf("create shared consumer rate-limit probe pod: %v\nstderr: %s", err, runErr.String())
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, "kubectl", kubectlArgs("delete", "pod", podName, "--now", "--ignore-not-found")...).Run()
	}()

	waitCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(
		"wait", "--for=jsonpath={.status.phase}=Succeeded",
		"pod/"+podName, "--timeout=45s")...)
	var waitErr bytes.Buffer
	waitCmd.Stderr = &waitErr
	if err := waitCmd.Run(); err != nil {
		t.Fatalf("wait for shared consumer rate-limit probe pod: %v\nstderr: %s", err, waitErr.String())
	}

	var out, logsErr bytes.Buffer
	logsCmd := exec.CommandContext(ctx, "kubectl", kubectlArgs("logs", podName)...)
	logsCmd.Stdout = &out
	logsCmd.Stderr = &logsErr
	if err := logsCmd.Run(); err != nil {
		t.Fatalf("read shared consumer rate-limit probe logs: %v\nstderr: %s", err, logsErr.String())
	}

	codes := map[string]string{}
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		fields := strings.Fields(raw)
		if len(fields) != 2 {
			t.Fatalf("unexpected shared consumer rate-limit probe output %q", raw)
		}
		codes[fields[0]] = fields[1]
	}
	return codes
}

func (s *sharedRateLimitScenario) setup(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	client, err := cfg.NewClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ns := NamespaceForTest(ctx, t, client)
	DumpLogsOnFailure(t, ns)
	backend := NewEchoServerBackend(ctx, t, client, ns)
	consumerSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: s.consumerSecretName, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"keys": "key-alice:alice\nkey-bob:bob\n",
		},
	}
	if err := client.Resources(ns).Create(ctx, consumerSecret); err != nil {
		t.Fatalf("create consumer API-key Secret: %v", err)
	}
	ingresses := createSharedRateLimitIngresses(ctx, t, client, ns,
		IngressSpec{
			Name:           fmt.Sprintf("echo-shared-ratelimit-%d", time.Now().UnixNano()),
			Host:           s.host,
			Path:           "/",
			BackendService: backend.Service,
			BackendPort:    backend.Port,
			Annotations: map[string]string{
				"haproxy-haptic.org/rate-limit-requests":  fmt.Sprintf("%d", s.limit),
				"haproxy-haptic.org/rate-limit-period":    "60s",
				"haproxy-haptic.org/rate-limit-burst":     fmt.Sprintf("%d", s.limit),
				"haproxy-haptic.org/rate-limit-algorithm": "gcra",
			},
		},
		IngressSpec{
			Name:           fmt.Sprintf("echo-shared-ratelimit-allow-%d", time.Now().UnixNano()),
			Host:           s.allowHost,
			Path:           "/",
			BackendService: backend.Service,
			BackendPort:    backend.Port,
			Annotations: map[string]string{
				"haproxy-haptic.org/rate-limit-requests":  fmt.Sprintf("%d", s.limit),
				"haproxy-haptic.org/rate-limit-period":    "60s",
				"haproxy-haptic.org/rate-limit-burst":     fmt.Sprintf("%d", s.limit),
				"haproxy-haptic.org/rate-limit-algorithm": "gcra",
				"haproxy-haptic.org/rate-limit-allowlist": "0.0.0.0/0,::/0",
			},
		},
		IngressSpec{
			Name:           fmt.Sprintf("echo-shared-ratelimit-lease-%d", time.Now().UnixNano()),
			Host:           s.leaseHost,
			Path:           "/",
			BackendService: backend.Service,
			BackendPort:    backend.Port,
			Annotations: map[string]string{
				"haproxy-haptic.org/rate-limit-requests": fmt.Sprintf("%d", s.leaseLimit),
				"haproxy-haptic.org/rate-limit-period":   "60s",
				"haproxy-haptic.org/rate-limit-burst":    fmt.Sprintf("%d", s.leaseLimit),
				"haproxy-haptic.org/waf-policy":          "streaming-search",
			},
		},
		IngressSpec{
			Name:           fmt.Sprintf("echo-shared-ratelimit-failover-%d", time.Now().UnixNano()),
			Host:           s.failoverHost,
			Path:           "/",
			BackendService: backend.Service,
			BackendPort:    backend.Port,
			Annotations: map[string]string{
				"haproxy-haptic.org/rate-limit-requests":  fmt.Sprintf("%d", s.failoverLimit),
				"haproxy-haptic.org/rate-limit-period":    "60s",
				"haproxy-haptic.org/rate-limit-burst":     fmt.Sprintf("%d", s.failoverLimit),
				"haproxy-haptic.org/rate-limit-algorithm": "gcra",
			},
		},
		IngressSpec{
			Name:           fmt.Sprintf("echo-shared-ratelimit-consumer-%d", time.Now().UnixNano()),
			Host:           s.consumerHost,
			Path:           "/",
			BackendService: backend.Service,
			BackendPort:    backend.Port,
			Annotations: map[string]string{
				"haproxy-haptic.org/api-key-secret":       s.consumerSecretName,
				"haproxy-haptic.org/rate-limit-requests":  "1",
				"haproxy-haptic.org/rate-limit-period":    "60s",
				"haproxy-haptic.org/rate-limit-burst":     "1",
				"haproxy-haptic.org/rate-limit-key":       "consumer",
				"haproxy-haptic.org/rate-limit-algorithm": "gcra",
			},
		},
		IngressSpec{
			Name:           fmt.Sprintf("echo-shared-ratelimit-ready-%d", time.Now().UnixNano()),
			Host:           s.readinessHost,
			Path:           "/",
			BackendService: backend.Service,
			BackendPort:    backend.Port,
		},
		IngressSpec{
			Name:           fmt.Sprintf("echo-shared-ratelimit-warmup-%d", time.Now().UnixNano()),
			Host:           s.warmupHost,
			Path:           "/",
			BackendService: backend.Service,
			BackendPort:    backend.Port,
			Annotations: map[string]string{
				"haproxy-haptic.org/rate-limit-requests":  fmt.Sprintf("%d", s.warmupLimit),
				"haproxy-haptic.org/rate-limit-period":    "60s",
				"haproxy-haptic.org/rate-limit-burst":     fmt.Sprintf("%d", s.warmupLimit),
				"haproxy-haptic.org/rate-limit-algorithm": "gcra",
			},
		},
		IngressSpec{
			Name:           fmt.Sprintf("echo-shared-ratelimit-outage-lease-%d", time.Now().UnixNano()),
			Host:           s.outageLeaseHost,
			Path:           "/",
			BackendService: backend.Service,
			BackendPort:    backend.Port,
			Annotations: map[string]string{
				"haproxy-haptic.org/rate-limit-requests": "1",
				"haproxy-haptic.org/rate-limit-period":   "60s",
				"haproxy-haptic.org/rate-limit-burst":    "1",
			},
		},
		IngressSpec{
			Name:           fmt.Sprintf("echo-shared-ratelimit-outage-exact-%d", time.Now().UnixNano()),
			Host:           s.outageExactHost,
			Path:           "/",
			BackendService: backend.Service,
			BackendPort:    backend.Port,
			Annotations: map[string]string{
				"haproxy-haptic.org/rate-limit-requests":  "1",
				"haproxy-haptic.org/rate-limit-period":    "60s",
				"haproxy-haptic.org/rate-limit-burst":     "1",
				"haproxy-haptic.org/rate-limit-algorithm": "gcra",
			},
		},
	)
	cleanupSharedRateLimitIngresses(t, client, ns, ingresses)
	// Wait until the rate-limited route is deployed without spending
	// request budget. HTTP polling is wrong here: the poll itself can
	// exhaust the shared limiter before the actual assertion runs.
	for _, ing := range ingresses {
		waitForIngressDeployed(ctx, t, client, ns, ing.Name)
	}
	return StoreNamespaceInContext(ctx, ns)
}

func (s *sharedRateLimitScenario) sharedBudgets(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	ns, err := GetNamespaceFromContext(ctx)
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	client, err := cfg.NewClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := waitForDeploymentRolloutComplete(ctx, client, ControllerNamespace, HAProxyDeploymentName, 2*time.Minute); err != nil {
		t.Fatalf("wait for HAProxy rollout after rate-limit profile helm upgrade: %v", err)
	}
	podIPs := routableHAProxyPodIPs(ctx, t, client, ns, s.readinessHost, 2)
	waitForStoreBackedSharedRateLimitOnPods(ctx, t, client, ns, s.warmupHost, podIPs[:2])
	result := sharedRateLimitBurstAcrossPods(ctx, t, ns, s.host, podIPs[:2], s.burstTotal, false)
	t.Logf("shared rate-limit direct-pod burst: %s", result.String())
	if result.byTarget["A"] == 0 || result.byTarget["B"] == 0 {
		t.Fatalf("expected burst to hit both HAProxy pods; got %s", result.String())
	}
	if result.byCode["429"] == 0 {
		t.Fatalf("expected at least one 429 from %d requests split across two HAProxy pods with one shared %d-request budget; got %s",
			s.burstTotal, s.limit, result.String())
	}
	if !result.hasExhaustedHeaders() {
		t.Fatalf("expected exhausted same-source probe to return 429 with rate-limit headers; got %s", result.String())
	}
	leaseResult := sharedRateLimitBurstAcrossPods(ctx, t, ns, s.leaseHost, podIPs[:2], s.leaseBurst, true)
	t.Logf("shared rate-limit lease-mode direct-pod burst: %s", leaseResult.String())
	if leaseResult.byTarget["A"] == 0 || leaseResult.byTarget["B"] == 0 {
		t.Fatalf("expected lease-mode burst to hit both HAProxy pods; got %s", leaseResult.String())
	}
	if leaseResult.byCode["429"] == 0 {
		t.Fatalf("expected at least one lease-mode 429 from %d requests split across two HAProxy pods with one shared %d-request budget; got %s",
			s.leaseBurst, s.leaseLimit, leaseResult.String())
	}
	if !leaseResult.hasExhaustedHeaders() {
		t.Fatalf("expected exhausted malicious lease-mode probe to return 429 with rate-limit headers before Coraza could return 403; got %s", leaseResult.String())
	}

	consumerCodes := probeSharedConsumerRateLimits(ctx, t, ns, s.consumerHost, podIPs[:2])
	t.Logf("shared rate-limit authenticated-consumer probes: %v", consumerCodes)
	if consumerCodes["alice-1"] != "200" || consumerCodes["alice-2"] != "429" ||
		consumerCodes["bob-1"] != "200" || consumerCodes["bob-2"] != "429" {
		t.Fatalf("expected independent one-request budgets for authenticated consumers alice and bob; got %v", consumerCodes)
	}

	s.assertAllowlistedRouteUnlimited(ctx, t, ns, podIPs[:2])

	deleteManagedRateLimitPrimary(ctx, t, client)
	waitForStoreBackedSharedRateLimitOnPods(ctx, t, client, ns, s.warmupHost, podIPs[:2])
	failoverResult := sharedRateLimitBurstAcrossPods(ctx, t, ns, s.failoverHost, podIPs[:2], s.failoverBurst, false)
	t.Logf("shared rate-limit after Valkey primary failover: %s", failoverResult.String())
	if failoverResult.byCode["200"] == 0 {
		t.Fatalf("expected at least one 200 after deleting the managed Valkey primary; got %s", failoverResult.String())
	}
	if failoverResult.byCode["429"] == 0 {
		t.Fatalf("expected at least one 429 after deleting the managed Valkey primary; got %s", failoverResult.String())
	}
	if !failoverResult.hasExhaustedHeaders() {
		t.Fatalf("expected exhausted post-failover probe to return 429 with rate-limit headers; got %s", failoverResult.String())
	}
	return ctx
}

// assertAllowlistedRouteUnlimited covers #232: the route's allowlist matches
// every client, so the same shared budget that produced 429s on its siblings
// must never fire here.
func (s *sharedRateLimitScenario) assertAllowlistedRouteUnlimited(ctx context.Context, t *testing.T, ns string, podIPs []string) {
	t.Helper()
	result := sharedRateLimitBurstAcrossPods(ctx, t, ns, s.allowHost, podIPs, s.burstTotal, false)
	t.Logf("shared rate-limit allowlisted-client burst: %s", result.String())
	if result.byCode["429"] != 0 || result.byCode["200"] == 0 {
		t.Fatalf("expected no 429 from %d requests on the allowlisted shared-limit route (rate-limit-allowlist covers the client); got %s",
			s.burstTotal, result.String())
	}
}

func (r *sharedRateLimitBurstResult) hasExhaustedHeaders() bool {
	return r.headerProbeCode == "429" && r.headerRetryAfter && r.headerLimit && r.headerRemaining && r.headerReset
}

func (s *sharedRateLimitScenario) outage(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	ns, err := GetNamespaceFromContext(ctx)
	if err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	client, err := cfg.NewClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	podIPs := routableHAProxyPodIPs(ctx, t, client, ns, s.readinessHost, 2)
	waitForManagedRateLimitStoreReady(ctx, t, client)
	originalReplicas, err := managedRateLimitStoreReplicas(ctx, client)
	if err != nil {
		t.Fatalf("read managed Valkey replica count: %v", err)
	}
	outageSignalsBefore := waitForRateLimitOutageSignals(ctx, t, client, podIPs)
	probePod := createValkeyOutageProbePod(ctx, t, ns)

	restored := false
	t.Cleanup(func() {
		if restored {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := scaleManagedRateLimitStore(cleanupCtx, originalReplicas); err != nil {
			t.Errorf("restore managed Valkey replicas: %v", err)
			return
		}
		if err := managedRateLimitStoreReady(cleanupCtx, client); err != nil {
			t.Errorf("wait for restored managed Valkey store: %v", err)
		}
	})

	if err := scaleManagedRateLimitStore(ctx, 0); err != nil {
		t.Fatalf("scale managed Valkey store to zero: %v", err)
	}
	if err := waitForManagedRateLimitStoreScaledDown(ctx, client); err != nil {
		t.Fatalf("wait for total managed Valkey outage: %v", err)
	}

	outageResult := probeValkeyOutageRoutes(ctx, t, ns, probePod, []valkeyOutageRoute{
		{name: "plain", host: s.readinessHost, expectedCodes: []string{"200"}, forbidRateLimitHeaders: true},
		{
			name:                    "lease",
			host:                    s.outageLeaseHost,
			expectedCodes:           []string{"200", "429"},
			requireRetryOnLimit:     true,
			requireRateLimitHeaders: true,
			expectedLimit:           "1",
			expectedRemaining:       "0",
		},
		{
			name:                    "exact",
			host:                    s.outageExactHost,
			expectedCodes:           []string{"200", "429"},
			requireRetryOnLimit:     true,
			requireRateLimitHeaders: true,
			expectedLimit:           "1",
			expectedRemaining:       "0",
		},
	}, podIPs)
	t.Logf("managed Valkey outage probes: %s", outageResult.String())
	if err := outageResult.validate(1 * time.Second); err != nil {
		t.Fatalf("managed Valkey local-fallback behavior: %v", err)
	}
	outageSignalsAfter := waitForLocalRateLimitFallback(ctx, t, client, podIPs, outageSignalsBefore)
	if routable := routableHAProxyPodIPs(ctx, t, client, ns, s.readinessHost, 2); len(routable) < 2 {
		t.Fatalf("expected two routable HAProxy pods during Valkey outage, got %v", routable)
	}

	s.checkHubOutage(ctx, t, client, ns, probePod, podIPs[0])
	if err := scaleManagedRateLimitStore(ctx, originalReplicas); err != nil {
		t.Fatalf("restore managed Valkey replicas: %v", err)
	}
	if err := managedRateLimitStoreReady(ctx, client); err != nil {
		t.Fatalf("wait for managed Valkey recovery: %v", err)
	}
	restored = true
	waitForAuthoritativeExactRateLimitRecovery(ctx, t, client, ns, probePod, s.outageExactHost, podIPs, outageSignalsAfter)
	return ctx
}

func (s *sharedRateLimitScenario) checkHubOutage(ctx context.Context, t *testing.T, client klient.Client, ns, probePod, probeIP string) {
	t.Helper()
	probeHAProxyPod, err := haproxyPodNameForIP(ctx, client, probeIP)
	if err != nil {
		t.Fatalf("find HAProxy pod for SPOA hub outage probe: %v", err)
	}
	unavailableSignalsBefore, err := rateLimitOutageSignalsByPod(ctx, client, []string{probeIP})
	if err != nil {
		t.Fatalf("snapshot rate-limit signals before SPOA hub outage: %v", err)
	}
	spoaHubDisabled := false
	t.Cleanup(func() {
		if !spoaHubDisabled {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := setSPOAHubRuntimeServerDisabled(cleanupCtx, probeHAProxyPod, false); err != nil {
			t.Errorf("restore SPOA hub runtime server on %s: %v", probeHAProxyPod, err)
			return
		}
		if err := waitForSPOAHubRuntimeServerState(cleanupCtx, probeHAProxyPod, false); err != nil {
			t.Errorf("verify restored SPOA hub runtime server on %s: %v", probeHAProxyPod, err)
		}
	})
	spoaHubDisabled = true
	if err := setSPOAHubRuntimeServerDisabled(ctx, probeHAProxyPod, true); err != nil {
		t.Fatalf("disable SPOA hub runtime server on %s: %v", probeHAProxyPod, err)
	}
	if err := waitForSPOAHubRuntimeServerState(ctx, probeHAProxyPod, true); err != nil {
		t.Fatalf("verify disabled SPOA hub runtime server on %s: %v", probeHAProxyPod, err)
	}
	unavailableProbe, err := probeValkeyOutageRouteOnce(ctx, ns, probePod, "P1", s.outageExactHost, probeIP)
	if err != nil {
		t.Fatalf("probe rate limit with unavailable SPOA hub: %v", err)
	}
	if unavailableProbe.code != "200" || unavailableProbe.duration > time.Second {
		t.Fatalf("rate limit with unavailable SPOA hub returned code=%s in %s, want 200 in at most 1s",
			unavailableProbe.code, unavailableProbe.duration)
	}
	if unavailableProbe.hasRateLimitHeaders() {
		t.Fatalf("unaccounted SPOA hub fail-open returned X-RateLimit headers: %+v", unavailableProbe)
	}
	waitForHubDegradation(ctx, t, client, probeIP, unavailableSignalsBefore[probeIP].degraded)
	if err := setSPOAHubRuntimeServerDisabled(ctx, probeHAProxyPod, false); err != nil {
		t.Fatalf("restore SPOA hub runtime server on %s: %v", probeHAProxyPod, err)
	}
	if err := waitForSPOAHubRuntimeServerState(ctx, probeHAProxyPod, false); err != nil {
		t.Fatalf("verify restored SPOA hub runtime server on %s: %v", probeHAProxyPod, err)
	}
	waitForSharedRateLimitReadyOnPods(ctx, t, ns, s.warmupHost, []string{probeIP})
	spoaHubDisabled = false
}

func waitForRateLimitOutageSignals(ctx context.Context, t *testing.T, client klient.Client, podIPs []string) map[string]rateLimitOutageSignals {
	t.Helper()
	var signals map[string]rateLimitOutageSignals
	err := testutil.WaitForConditionWithDescription(ctx, testutil.FastWaitConfig(),
		"rate-limit metrics exporters on every HAProxy pod",
		func(ctx context.Context) (bool, error) {
			var err error
			signals, err = rateLimitOutageSignalsByPod(ctx, client, podIPs)
			return err == nil, err
		})
	if err != nil {
		t.Fatalf("snapshot rate-limit outage metrics before Valkey outage: %v", err)
	}
	return signals
}

func waitForLocalRateLimitFallback(ctx context.Context, t *testing.T, client klient.Client, podIPs []string, before map[string]rateLimitOutageSignals) map[string]rateLimitOutageSignals {
	t.Helper()
	var after map[string]rateLimitOutageSignals
	err := testutil.WaitForConditionWithDescription(ctx, testutil.FastWaitConfig(),
		"local-fallback rate-limit signals on every HAProxy pod",
		func(ctx context.Context) (bool, error) {
			var err error
			after, err = rateLimitOutageSignalsByPod(ctx, client, podIPs)
			if err != nil {
				return false, err
			}
			err = validateLocalRateLimitFallback(podIPs, before, after)
			return err == nil, err
		})
	if err != nil {
		t.Fatalf("observe local fallback on every HAProxy pod: %v", err)
	}
	for _, podIP := range podIPs {
		deltas := make(map[string]float64, len(rateLimitFallbackOutcomes))
		for _, outcome := range rateLimitFallbackOutcomes {
			deltas[outcome] = after[podIP].outcomes[outcome] - before[podIP].outcomes[outcome]
		}
		t.Logf("HAProxy pod %s outage signals: outcomes=%v degraded=%v", podIP, deltas, after[podIP].degraded-before[podIP].degraded)
	}
	return after
}

func validateLocalRateLimitFallback(podIPs []string, before, after map[string]rateLimitOutageSignals) error {
	for _, podIP := range podIPs {
		for _, outcome := range rateLimitFallbackOutcomes {
			if delta := after[podIP].outcomes[outcome] - before[podIP].outcomes[outcome]; delta < 1 {
				return fmt.Errorf("HAProxy pod %s has %s delta %v, want at least 1", podIP, outcome, delta)
			}
		}
		if delta := after[podIP].degraded - before[podIP].degraded; delta < float64(len(rateLimitFallbackOutcomes)) {
			return fmt.Errorf("HAProxy pod %s has degraded transaction delta %v, want at least %d", podIP, delta, len(rateLimitFallbackOutcomes))
		}
	}
	return nil
}

func waitForHubDegradation(ctx context.Context, t *testing.T, client klient.Client, probeIP string, before float64) {
	t.Helper()
	err := testutil.WaitForConditionWithDescription(ctx, testutil.FastWaitConfig(),
		"a degraded rate-limit signal after the SPOA hub becomes unavailable",
		func(ctx context.Context) (bool, error) {
			after, err := rateLimitOutageSignalsByPod(ctx, client, []string{probeIP})
			if err != nil {
				return false, err
			}
			delta := after[probeIP].degraded - before
			if delta < 1 {
				return false, fmt.Errorf("HAProxy pod %s has degraded transaction delta %v, want at least 1", probeIP, delta)
			}
			return true, nil
		})
	if err != nil {
		t.Fatalf("observe SPOA hub fail-open degradation: %v", err)
	}
}
