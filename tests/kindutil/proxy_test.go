// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package kindutil

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

func TestFreshClusterReservesProxyCPUAfterCNIAndWaitsForRollout(t *testing.T) {
	for _, timeout := range []time.Duration{0, time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			checkProxyCreation(t, timeout)
		})
	}
}

func checkProxyCreation(t *testing.T, timeout time.Duration) {
	t.Helper()
	dir := t.TempDir()
	var cluster *Cluster
	base := successfulClusterRunner(t, "", &cluster)
	nodesReady, patched, rolledOut := false, false, false
	runner := clusterRunner{run: func(ctx context.Context, cmd *process.Command) (process.Result, error) {
		if cmd.Name == "docker" && slices.Contains(cmd.Args, "condition=Ready") {
			nodesReady = true
		}
		if cmd.Name == "kubectl" && slices.Contains(cmd.Args, "daemonset/kube-proxy") {
			require.True(t, nodesReady)
			require.Equal(t, []string{"--kubeconfig", cluster.Kubeconfig, "--context", "kind-matrix", "--namespace", "kube-system"}, cmd.Args[:6])
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			expectedTimeout := timeout
			if expectedTimeout == 0 {
				expectedTimeout = 3 * time.Minute
			}
			require.WithinDuration(t, time.Now().Add(expectedTimeout), deadline, time.Second)
			switch cmd.Args[6] {
			case "patch":
				require.False(t, rolledOut)
				require.Equal(t, []string{"patch", "daemonset/kube-proxy", "--type=strategic", "--patch"}, cmd.Args[6:10])
				require.JSONEq(t, `{"spec":{"template":{"spec":{"containers":[{"name":"kube-proxy","resources":{"requests":{"cpu":"100m"}}}]}}}}`, cmd.Args[10])
				patched = true
			case "rollout":
				require.True(t, patched)
				require.Equal(t, []string{"rollout", "status", "daemonset/kube-proxy", "--timeout", expectedTimeout.String()}, cmd.Args[6:])
				rolledOut = true
			default:
				t.Fatalf("unexpected proxy command: %v", cmd.Args)
			}
		}
		return base.Run(ctx, cmd)
	}}
	var err error
	cluster, err = NewCluster(runner, &ClusterOptions{Name: "matrix", Kubeconfig: filepath.Join(dir, "kubeconfig"), Artifacts: dir, ReadyTimeout: timeout})
	require.NoError(t, err)
	require.NoError(t, cluster.Create(t.Context(), ""))
	require.True(t, patched)
	require.True(t, rolledOut)
}

type proxyFailureRunner struct {
	t         *testing.T
	base      clusterRunner
	phase     string
	failure   error
	cancel    context.CancelFunc
	patched   bool
	rolledOut bool
	removed   bool
}

func (r *proxyFailureRunner) run(ctx context.Context, cmd *process.Command) (process.Result, error) {
	if cmd.Name == "docker" && cmd.Args[0] == "inspect" && r.phase == "ownership" {
		return process.Result{Stdout: `[{"Id":"owned-id","Mounts":[]}]`}, nil
	}
	if cmd.Name == "docker" && cmd.Args[0] == "rm" {
		require.Equal(r.t, []string{"rm", "--force", "--volumes", "owned-id"}, cmd.Args)
		r.removed = true
		return process.Result{}, nil
	}
	if cmd.Name != "kubectl" || !slices.Contains(cmd.Args, "daemonset/kube-proxy") {
		return r.base.Run(ctx, cmd)
	}
	if !slices.Contains(cmd.Args, "patch") {
		r.rolledOut = true
		return process.Result{Stderr: "proxy unavailable"}, r.failure
	}
	r.patched = true
	switch r.phase {
	case "patch":
		return process.Result{Stderr: "patch refused"}, r.failure
	case "canceled":
		r.cancel()
		return process.Result{}, ctx.Err()
	default:
		return r.base.Run(ctx, cmd)
	}
}

func TestProxyReservationFailuresAbortCreationAndKeepOwnedCleanup(t *testing.T) {
	for _, phase := range []string{"ownership", "patch", "rollout", "canceled"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			var cluster *Cluster
			failure := errors.New("fixture unavailable")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			probe := &proxyFailureRunner{t: t, base: successfulClusterRunner(t, "", &cluster), phase: phase, failure: failure, cancel: cancel}
			var err error
			cluster, err = NewCluster(clusterRunner{run: probe.run}, &ClusterOptions{Name: "matrix", Kubeconfig: filepath.Join(dir, "kubeconfig"), Artifacts: dir})
			require.NoError(t, err)
			err = cluster.Create(ctx, "")
			switch phase {
			case "ownership":
				require.ErrorContains(t, err, "foreign node")
				require.False(t, probe.patched)
				require.Error(t, cluster.Close(t.Context()))
				require.False(t, probe.removed)
			case "canceled":
				require.ErrorIs(t, err, context.Canceled)
			default:
				require.ErrorIs(t, err, failure)
			}
			require.Equal(t, phase == "rollout", probe.rolledOut)
			if phase != "ownership" {
				require.True(t, probe.patched)
				require.NoError(t, cluster.Close(t.Context()))
				require.True(t, probe.removed)
			}
		})
	}
}
