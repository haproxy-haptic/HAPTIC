// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package kindutil

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

func TestCalicoManifest(t *testing.T) {
	images, err := cniImages()
	require.NoError(t, err)
	header := regexp.MustCompile(`^# Calico (v[0-9.]+) `).FindSubmatch(cniManifest)
	require.NotNil(t, header, "manifest header must name the upstream Calico version")
	for _, image := range images {
		require.Regexp(t, `^quay\.io/calico/[a-z-]+:`+regexp.QuoteMeta(string(header[1]))+`@sha256:[a-f0-9]{64}$`, image)
	}

	var daemonSet appsv1.DaemonSet
	for document := range strings.SplitSeq(string(cniManifest), "\n---\n") {
		var candidate appsv1.DaemonSet
		require.NoError(t, yaml.Unmarshal([]byte(document), &candidate))
		if candidate.Kind == "DaemonSet" {
			daemonSet = candidate
		}
	}
	require.Equal(t, "calico-node", daemonSet.Name)
	require.Contains(t, daemonSet.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "CALICO_IPV4POOL_CIDR", Value: PodSubnet})
}

type recordedCommand struct {
	args  []string
	input string
}

// cniRunner fails the worker's first pull of failingImage pullFailures times.
func cniRunner(t *testing.T, failingImage string, pullFailures int, controlPlane string) (runner clusterRunner, recorded func() []recordedCommand) {
	t.Helper()
	var mu sync.Mutex
	var commands []recordedCommand
	runner = clusterRunner{run: func(_ context.Context, cmd *process.Command) (process.Result, error) {
		input := ""
		if cmd.Stdin != nil {
			data, err := io.ReadAll(cmd.Stdin)
			if err != nil {
				return process.Result{}, err
			}
			input = string(data)
		}
		mu.Lock()
		defer mu.Unlock()
		commands = append(commands, recordedCommand{args: slices.Clone(cmd.Args), input: input})
		switch {
		case cmd.Name != "docker":
			return process.Result{}, errors.New("unexpected command " + cmd.Name)
		case cmd.Args[0] == "ps" && slices.Contains(cmd.Args, "label=io.x-k8s.kind.role=control-plane"):
			return process.Result{Stdout: controlPlane + "\n"}, nil
		case cmd.Args[0] == "ps":
			return process.Result{Stdout: "control-id\nworker-id\n"}, nil
		case slices.Equal(cmd.Args, []string{"exec", "worker-id", "crictl", "pull", failingImage}) && pullFailures > 0:
			pullFailures--
			return process.Result{Stderr: "registry unavailable"}, errors.New("exit status 1")
		}
		return process.Result{}, nil
	}}
	recorded = func() []recordedCommand {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(commands)
	}
	return runner, recorded
}

func TestInstallCNIPreloadsEveryImageOnEveryNodeAndAppliesOnControlPlane(t *testing.T) {
	images, err := cniImages()
	require.NoError(t, err)
	for _, readyTimeout := range []time.Duration{0, 2 * time.Minute} {
		t.Run(readyTimeout.String(), func(t *testing.T) {
			runner, commands := cniRunner(t, images[0], 1, "control-id")
			cluster := &Cluster{Name: "private", ReadyTimeout: readyTimeout, runner: runner}
			require.NoError(t, cluster.installCNI(t.Context(), []string{"control-id", "worker-id"}))

			var pulls []string
			var kubectl []recordedCommand
			for _, command := range commands() {
				switch {
				case slices.Contains(command.args, "crictl"):
					pulls = append(pulls, strings.Join(command.args, " "))
				case slices.Contains(command.args, "kubectl"):
					require.Equal(t, []string{"exec", "-i", "control-id", "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf"}, command.args[:5])
					kubectl = append(kubectl, command)
				}
			}
			want := make([]string, 0, 2*len(images)+1)
			for _, node := range []string{"control-id", "worker-id"} {
				for _, image := range images {
					want = append(want, "exec "+node+" crictl pull "+image)
				}
			}
			want = append(want, "exec worker-id crictl pull "+images[0])
			require.ElementsMatch(t, want, pulls)
			require.Equal(t, []string{"apply", "--server-side", "-f", "-"}, kubectl[0].args[5:])
			require.Equal(t, string(cniManifest), kubectl[0].input)
			require.Len(t, kubectl, 4)
			wantTimeout := map[time.Duration]string{0: "3m0s", 2 * time.Minute: "2m0s"}[readyTimeout]
			require.Contains(t, strings.Join(kubectl[1].args, " "), "rollout status daemonset/calico-node --timeout "+wantTimeout)
			require.Contains(t, strings.Join(kubectl[2].args, " "), "rollout status deployment/calico-kube-controllers --timeout ")
			require.Contains(t, strings.Join(kubectl[3].args, " "), "wait --for condition=Ready nodes --all --timeout ")
		})
	}
}

func TestInstallCNIFailsLoudly(t *testing.T) {
	images, err := cniImages()
	require.NoError(t, err)
	runner, commands := cniRunner(t, images[len(images)-1], cniPullAttempts, "control-id")
	require.ErrorContains(t, InstallCNICluster(t.Context(), runner, "private", nil), "registry unavailable")
	for _, command := range commands() {
		require.NotContains(t, command.args, "kubectl")
	}

	runner, commands = cniRunner(t, "", 0, "foreign-id")
	require.ErrorContains(t, InstallCNICluster(t.Context(), runner, "private", nil), "no single control-plane")
	for _, command := range commands() {
		require.NotContains(t, command.args, "kubectl")
	}
}

func TestOwnedClusterInstallsCNIInsteadOfKindDefault(t *testing.T) {
	data, err := ownedKindConfig(nil, "/private/owner", "")
	require.NoError(t, err)
	require.Contains(t, string(data), "disableDefaultCNI: true")
	require.Contains(t, string(data), "podSubnet: "+PodSubnet)

	_, err = ownedKindConfig([]byte(BaseKindConfig+"networking:\n  podSubnet: 192.168.0.0/16\n"), "/private/owner", "")
	require.ErrorContains(t, err, "must equal the CNI's IP pool")

	dir := t.TempDir()
	var cluster *Cluster
	var applied atomic.Bool
	base := successfulClusterRunner(t, "", &cluster)
	runner := clusterRunner{run: func(ctx context.Context, cmd *process.Command) (process.Result, error) {
		if cmd.Name == "kind" {
			require.NotContains(t, cmd.Args, "--wait")
		}
		if cmd.Name == "docker" && slices.Contains(cmd.Args, "apply") {
			applied.Store(true)
		}
		return base.Run(ctx, cmd)
	}}
	cluster, err = NewCluster(runner, &ClusterOptions{Name: "matrix", Kubeconfig: filepath.Join(dir, "kubeconfig"), Artifacts: dir, ReadyTimeout: time.Minute})
	require.NoError(t, err)
	require.NoError(t, cluster.Create(t.Context(), ""))
	require.True(t, applied.Load())
}
