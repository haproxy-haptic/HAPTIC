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

package deployer

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/agenttest"
)

type reloadLogLine struct {
	Level   string   `json:"level"`
	Msg     string   `json:"msg"`
	Pod     string   `json:"pod"`
	Reasons []string `json:"reasons"`
}

func reloadLines(t *testing.T, logs *testutil.LogRecorder) []reloadLogLine {
	t.Helper()
	records := logs.Records("Reload required")
	lines := make([]reloadLogLine, 0, len(records))
	for _, raw := range records {
		var line reloadLogLine
		require.NoError(t, json.Unmarshal(raw, &line))
		lines = append(lines, line)
	}
	return lines
}

func reloadLoggingDeployer(bus *deployerBus) (*Component, *testutil.LogRecorder) {
	logger, logs := testutil.NewRecordingLogger()
	return New(bus.EventBus, logger, 0, metrics.NewMetrics(prometheus.NewRegistry())), logs
}

func TestApply_ReloadDecisionIsLoggedAtInfoOncePerPod(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component, logs := reloadLoggingDeployer(bus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	lines := reloadLines(t, logs)
	require.Len(t, lines, 1)
	assert.Equal(t, "INFO", lines[0].Level)
	assert.Equal(t, "haproxy-0", lines[0].Pod)
	assert.NotEmpty(t, lines[0].Reasons)

	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)
	deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)
	assert.Len(t, reloadLines(t, logs), 1, "runtime and no-op deploys must not log a reload")
}

func TestApply_RuntimeBatchFallbackIsLoggedAsAReload(t *testing.T) {
	agent := agenttest.New(t, agenttest.WithoutRuntimeBatches())
	bus := newTestBus(t)
	component, logs := reloadLoggingDeployer(bus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderWithServers("plan-1", 10)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	plan2, config2, aux2 := renderWithServers("plan-2", 20)
	deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	lines := reloadLines(t, logs)
	require.Len(t, lines, 2)
	assert.Equal(t, []string{"agent does not support runtime batches in one transaction"}, lines[1].Reasons)
}

func TestApply_ChangeJoiningAPendingReloadIsNotLoggedAsAnotherReload(t *testing.T) {
	agent := agenttest.New(t, agenttest.WithoutRuntimeBatches())
	bus := newTestBus(t)
	component, logs := reloadLoggingDeployer(bus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderWithServers("plan-1", 10)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	agent.SetReloadPending(true)
	plan2, config2, aux2 := renderWithServers("plan-2", 20)
	deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	assert.Len(t, reloadLines(t, logs), 1, "the pending reload was already logged when it was scheduled")
	assert.Equal(t, []slog.Level{slog.LevelDebug}, logs.Levels("Change joins the pending reload"))
}
