// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

func TestUpgradeRejectsControllerRejectionMetrics(t *testing.T) {
	for _, test := range []struct {
		name    string
		metrics string
		valid   bool
	}{
		{"no rejections yet", "# TYPE process_cpu_seconds_total counter\nprocess_cpu_seconds_total 1\n", true},
		{"zero", "# TYPE haptic_config_rejected_total counter\nhaptic_config_rejected_total{validator=\"template\"} 0\n", true},
		{"one validator rejected", "# TYPE haptic_config_rejected_total counter\nhaptic_config_rejected_total{validator=\"template\"} 0\nhaptic_config_rejected_total{validator=\"validationtests\"} 1\n", false},
		{"empty response", "", false},
		{"wrong metric type", "# TYPE haptic_config_rejected_total gauge\nhaptic_config_rejected_total 1\n", false},
		{"invalid response", "not metrics", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requireZeroRejections(test.metrics)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestUpgradeChecksRetiredControllerValidationLogs(t *testing.T) {
	require.NoError(t, requireNoValidationRejections("Configuration validation succeeded\ncontroller stopped"))
	for _, message := range []string{"Template validation failed", "ValidationTests could not run", "Config validation failed", "Configuration validation failed"} {
		require.Error(t, requireNoValidationRejections("2026-10-18 level=ERROR msg=\""+message+"\" undefined: on_own_lines"))
	}
}

func TestTrafficProbeExitReadsContainerStateBeforePodPhase(t *testing.T) {
	terminated := func(code int32) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "probe", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}},
		}}}
	}
	for _, test := range []struct {
		name   string
		status corev1.PodStatus
		want   testutil.PollResult
		fails  bool
	}{
		{"exit 0 while kubelet still reports Running", terminated(0), testutil.PollSucceeded, false},
		{"non-zero exit", terminated(1), testutil.PollFailed, true},
		{"still running", corev1.PodStatus{Phase: corev1.PodRunning}, testutil.PollPending, false},
		{"pod failed without container status", corev1.PodStatus{Phase: corev1.PodFailed}, testutil.PollFailed, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := probeExit(&corev1.Pod{Status: test.status})
			require.Equal(t, test.want, got)
			if test.fails {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
