// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"testing"

	"github.com/stretchr/testify/require"
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
