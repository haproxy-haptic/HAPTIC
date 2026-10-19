// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package kindutil

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

func TestKindnetManifestKeepsKindDefaultsAndPolicyEnforcement(t *testing.T) {
	image, err := kindnetImage()
	require.NoError(t, err)
	require.Regexp(t, `^registry\.k8s\.io/networking/kindnet:v[0-9.]+@sha256:[a-f0-9]{64}$`, image)

	var daemonSet appsv1.DaemonSet
	for document := range strings.SplitSeq(string(kindnetManifest), "\n---\n") {
		var candidate appsv1.DaemonSet
		require.NoError(t, yaml.Unmarshal([]byte(document), &candidate))
		if candidate.Kind == "DaemonSet" {
			daemonSet = candidate
		}
	}
	require.Equal(t, "kindnet", daemonSet.Name)
	command := daemonSet.Spec.Template.Spec.Containers[0].Command
	for _, flag := range []string{"--network-policy=true", "--masquerading=true", "--dns-caching=false", "--fastpath-threshold=0", "--nat64=false", "--disable-cni=false", "--v=0"} {
		require.Contains(t, command, flag)
	}
	disablesEvents := slices.ContainsFunc(daemonSet.Spec.Template.Spec.InitContainers, func(c corev1.Container) bool {
		return slices.Contains(c.Command, "echo 0 > /proc/sys/net/netfilter/nf_conntrack_events") &&
			c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged
	})
	require.True(t, disablesEvents, "kindnet must disable conntrack events before kindnetd subscribes to them")
}

type recordedCommand struct {
	args  []string
	input string
}

func kindnetRunner(t *testing.T, pullFailures int, controlPlane string) (clusterRunner, *[]recordedCommand) {
	t.Helper()
	var commands []recordedCommand
	return clusterRunner{run: func(_ context.Context, cmd *process.Command) (process.Result, error) {
		require.Equal(t, "docker", cmd.Name)
		input := ""
		if cmd.Stdin != nil {
			data, err := io.ReadAll(cmd.Stdin)
			require.NoError(t, err)
			input = string(data)
		}
		commands = append(commands, recordedCommand{args: slices.Clone(cmd.Args), input: input})
		switch {
		case cmd.Args[0] == "ps" && slices.Contains(cmd.Args, "label=io.x-k8s.kind.role=control-plane"):
			return process.Result{Stdout: controlPlane + "\n"}, nil
		case cmd.Args[0] == "ps":
			return process.Result{Stdout: "control-id\nworker-id\n"}, nil
		case slices.Contains(cmd.Args, "crictl") && pullFailures > 0:
			pullFailures--
			return process.Result{Stderr: "registry unavailable"}, errors.New("exit status 1")
		}
		return process.Result{}, nil
	}}, &commands
}

func TestInstallKindnetPreloadsEveryNodeAndAppliesOnControlPlane(t *testing.T) {
	for _, readyTimeout := range []time.Duration{0, 2 * time.Minute} {
		t.Run(readyTimeout.String(), func(t *testing.T) {
			runner, commands := kindnetRunner(t, 1, "control-id")
			require.NoError(t, InstallKindnetCluster(t.Context(), runner, "private", nil, readyTimeout))
			image, err := kindnetImage()
			require.NoError(t, err)

			var pulls, kubectl []recordedCommand
			for _, command := range *commands {
				switch {
				case slices.Contains(command.args, "crictl"):
					pulls = append(pulls, command)
				case slices.Contains(command.args, "kubectl"):
					require.Equal(t, []string{"exec", "-i", "control-id", "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf"}, command.args[:5])
					kubectl = append(kubectl, command)
				}
			}
			require.Equal(t, [][]string{
				{"exec", "control-id", "crictl", "pull", image},
				{"exec", "control-id", "crictl", "pull", image},
				{"exec", "worker-id", "crictl", "pull", image},
			}, [][]string{pulls[0].args, pulls[1].args, pulls[2].args})
			require.Equal(t, []string{"apply", "-f", "-"}, kubectl[0].args[5:])
			require.Equal(t, string(kindnetManifest), kubectl[0].input)
			if readyTimeout == 0 {
				require.Len(t, kubectl, 1)
				return
			}
			require.Len(t, kubectl, 3)
			require.Contains(t, strings.Join(kubectl[1].args, " "), "rollout status daemonset/kindnet --timeout 2m0s")
			require.Contains(t, strings.Join(kubectl[2].args, " "), "wait --for condition=Ready nodes --all --timeout ")
		})
	}
}

func TestInstallKindnetFailsLoudly(t *testing.T) {
	runner, _ := kindnetRunner(t, kindnetPullAttempts, "control-id")
	require.ErrorContains(t, InstallKindnetCluster(t.Context(), runner, "private", nil, 0), "registry unavailable")

	runner, commands := kindnetRunner(t, 0, "foreign-id")
	require.ErrorContains(t, InstallKindnetCluster(t.Context(), runner, "private", nil, 0), "no single control-plane")
	for _, command := range *commands {
		require.NotContains(t, command.args, "kubectl")
	}
}

func TestOwnedClusterInstallsKindnetInsteadOfKindDefaultCNI(t *testing.T) {
	data, err := ownedKindConfig(nil, "/private/owner", "")
	require.NoError(t, err)
	require.Contains(t, string(data), "disableDefaultCNI: true")

	dir := t.TempDir()
	var cluster *Cluster
	var applied bool
	base := successfulClusterRunner(t, "", &cluster)
	runner := clusterRunner{run: func(ctx context.Context, cmd *process.Command) (process.Result, error) {
		if cmd.Name == "kind" {
			require.NotContains(t, cmd.Args, "--wait")
		}
		if cmd.Name == "docker" && slices.Contains(cmd.Args, "apply") {
			applied = true
		}
		return base.Run(ctx, cmd)
	}}
	cluster, err = NewCluster(runner, &ClusterOptions{Name: "matrix", Kubeconfig: filepath.Join(dir, "kubeconfig"), Artifacts: dir, ReadyTimeout: time.Minute})
	require.NoError(t, err)
	require.NoError(t, cluster.Create(t.Context(), ""))
	require.True(t, applied)
}
