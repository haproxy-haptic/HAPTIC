// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package kindutil

import (
	"context"
	"fmt"
	"time"
)

func (c *Cluster) reserveProxyCPU(ctx context.Context) error {
	timeout := c.ReadyTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := c.Client("kube-system")
	// Kubeadm leaves kube-proxy BestEffort; concurrent validation can starve routing updates (#483).
	patch := `{"spec":{"template":{"spec":{"containers":[{"name":"kube-proxy","resources":{"requests":{"cpu":"100m"}}}]}}}}`
	if result, err := client.Run(ctx, nil, "patch", "daemonset/kube-proxy", "--type=strategic", "--patch", patch); err != nil {
		return fmt.Errorf("reserve kube-proxy CPU: %w: %s", err, result.Stderr)
	}
	if result, err := client.Run(ctx, nil, "rollout", "status", "daemonset/kube-proxy", "--timeout", timeout.String()); err != nil {
		return fmt.Errorf("wait for kube-proxy CPU reservation: %w: %s", err, result.Stderr)
	}
	return nil
}
