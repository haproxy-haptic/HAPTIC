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
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/klient"
)

// maxStoreRollWriteGap is below Sentinel's 5s down-after: a primary that exits
// without handing its role over cannot stay under it (#773).
const maxStoreRollWriteGap = 5 * time.Second

// storeRollWriteProbe is what the rate-limit plugin does: ask Sentinel for the
// primary and write to it.
const storeRollWriteProbe = `m="$(valkey-cli -p 26379 sentinel get-master-addr-by-name haptic-rate-limit | head -n 1)" && ` +
	`valkey-cli -h "$m" -p 6379 incr e2e-store-roll-probe`

// rollManagedRateLimitStore rolls every store pod, as a template or image
// change does, and fails when the store had no writable primary for
// maxStoreRollWriteGap or longer.
func rollManagedRateLimitStore(ctx context.Context, t *testing.T, client klient.Client) {
	t.Helper()
	waitForManagedRateLimitStoreReady(ctx, t, client)
	t.Logf("rolling the managed Valkey store; primary before the roll: %s", currentManagedRateLimitPrimary(ctx, t, client))

	probeCtx, stopProbe := context.WithCancel(ctx)
	gaps := make(chan storeRollWriteGaps, 1)
	go func() { gaps <- probeStoreWritesDuringRoll(probeCtx, client) }()

	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"haproxy-haptic.org/e2e-roll":"%d"}}}}}`,
		time.Now().UnixNano())
	if out, err := exec.CommandContext(ctx, "kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
		"patch", "statefulset", rateLimitStoreName, "--type=merge", "-p", patch).CombinedOutput(); err != nil {
		stopProbe()
		t.Fatalf("patch managed Valkey StatefulSet to roll it: %v\noutput: %s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
		"rollout", "status", "statefulset/"+rateLimitStoreName, "--timeout=5m").CombinedOutput(); err != nil {
		stopProbe()
		t.Fatalf("wait for managed Valkey StatefulSet rollout: %v\noutput: %s", err, out)
	}
	waitForManagedRateLimitStoreReady(ctx, t, client)
	stopProbe()
	result := <-gaps
	t.Logf("managed Valkey store roll: %d successful writes, longest stretch without one %s; primary after the roll: %s",
		result.successes, result.longest, currentManagedRateLimitPrimary(ctx, t, client))
	if result.successes == 0 || result.longest >= maxStoreRollWriteGap {
		t.Fatalf("managed Valkey store had no writable primary for %s during a rolling update (limit %s); last errors: %v",
			result.longest, maxStoreRollWriteGap, result.lastErrors)
	}
}

type storeRollWriteGaps struct {
	successes  int
	longest    time.Duration
	lastErrors []string
}

func probeStoreWritesDuringRoll(ctx context.Context, client klient.Client) storeRollWriteGaps {
	var result storeRollWriteGaps
	lastSuccess := time.Now()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			result.longest = max(result.longest, time.Since(lastSuccess))
			return result
		case <-ticker.C:
		}
		if err := writeThroughSentinel(ctx, client); err != nil {
			if ctx.Err() == nil {
				result.lastErrors = append(result.lastErrors, err.Error())
				if len(result.lastErrors) > 5 {
					result.lastErrors = result.lastErrors[1:]
				}
			}
			continue
		}
		result.longest = max(result.longest, time.Since(lastSuccess))
		lastSuccess = time.Now()
		result.successes++
	}
}

// writeThroughSentinel runs the probe in a Ready store pod that is not shutting
// down, the only pods the store's NetworkPolicy lets reach Valkey besides HAProxy.
func writeThroughSentinel(ctx context.Context, client klient.Client) error {
	pods, err := listManagedRateLimitStorePods(ctx, client)
	if err != nil {
		return err
	}
	for i := range pods {
		if pods[i].DeletionTimestamp != nil || !podReady(&pods[i]) {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		cmd := exec.CommandContext(callCtx, "kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pods[i].Name, "-c", "sentinel", "--", "sh", "-c", storeRollWriteProbe)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		cancel()
		reply := strings.TrimSpace(out.String())
		if _, parseErr := strconv.Atoi(reply); err == nil && parseErr == nil {
			return nil
		}
		return fmt.Errorf("%s: %v %q", pods[i].Name, err, reply)
	}
	return fmt.Errorf("no Ready managed Valkey pod to probe from")
}
