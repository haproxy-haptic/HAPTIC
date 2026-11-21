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
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/agenttest"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/haproxytest"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/server"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// startRealAgent runs the production agent against the HAProxy model and
// returns the pod endpoint and the tree it writes.
func startRealAgent(t *testing.T, podName string) (endpoint dataplane.Endpoint, baseDir string) {
	t.Helper()
	return startRealAgentWith(t, podName, "")
}

// startRealAgentWith also stores blobs in blobDir; empty leaves blobs off.
func startRealAgentWith(t *testing.T, podName, blobDir string) (endpoint dataplane.Endpoint, baseDir string) {
	t.Helper()
	model := haproxytest.Start(t)
	baseDir = t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	agent, err := server.New(ctx, &server.Config{
		BlobDir:      blobDir,
		BaseDir:      baseDir,
		ConfigFile:   "haproxy.cfg",
		MasterSocket: model.MasterSocket(),
		WorkerSocket: model.WorkerSocket(),
		StateFile:    ".haptic-agent.json",
		Listen:       "127.0.0.1:0",
		Username:     agenttest.DefaultUsername,
		Password:     agenttest.DefaultPassword,
		AgentVersion: "drift",
		Logger:       slog.New(slog.DiscardHandler),
		Registry:     prometheus.NewRegistry(),
	})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- agent.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the agent did not shut down")
		}
	})
	require.Eventually(t, agent.Ready, 10*time.Second, 10*time.Millisecond)
	return dataplane.Endpoint{
		URL:          "http://" + agent.Addr(),
		Username:     agenttest.DefaultUsername,
		Password:     agenttest.DefaultPassword,
		PodName:      podName,
		PodNamespace: "haptic",
		PodUID:       podName + "-uid",
	}, baseDir
}

// driftFixture is a pod on the validated render, ready for drift rounds.
type driftFixture struct {
	component *Component
	bus       *deployerBus
	endpoint  dataplane.Endpoint
	baseDir   string
	plan      *renderplan.Plan
	config    string
	aux       *dataplane.AuxiliaryFiles
}

func newDriftFixture(t *testing.T) *driftFixture {
	t.Helper()
	endpoint, baseDir := startRealAgent(t, "haproxy-0")
	f := &driftFixture{bus: newTestBus(t), endpoint: endpoint, baseDir: baseDir}
	f.plan, f.config, f.aux = renderFor("plan-1", "10.0.0.1", mapEntry)
	f.component = createTestDeployer(f.bus.EventBus)
	initial := f.deploy(t, "config_validation")
	require.Equal(t, 1, initial.Succeeded)
	occurrence, err := initial.RenderOccurrence()
	require.NoError(t, err)
	f.component.SetValidatedOccurrence(occurrence)
	return f
}

func (f *driftFixture) deploy(t *testing.T, reason string) *events.DeploymentCompletedEvent {
	t.Helper()
	return deployTo(t, f.component, f.bus, f.plan, f.config, f.aux, reason, f.endpoint)
}

func (f *driftFixture) applies(mode string) float64 {
	return promtestutil.ToFloat64(f.component.metrics.AgentApplyTotal.WithLabelValues(f.endpoint.PodName, mode))
}

// An idle pod's drift rounds find nothing to correct: no file is sent and every
// apply is a noop.
func TestDriftRoundsOnAnIdlePodAreNoops(t *testing.T) {
	f := newDriftFixture(t)

	const rounds = 5
	for range rounds {
		completed := f.deploy(t, events.TriggerReasonDriftPrevention)
		require.Equal(t, 1, completed.Succeeded)
		require.NotNil(t, completed.Phases)
		assert.Zero(t, completed.Phases.UploadBytes, "an idle drift round sends no file")
	}
	assert.Equal(t, float64(rounds), f.applies(api.ResultNoop))
	assert.Zero(t, f.applies(api.ResultFileOnly))
}

// A drift round still repairs the pod: a file changed behind the agent is sent
// again, and only that file.
func TestDriftRoundRestoresAFileChangedBehindTheAgent(t *testing.T) {
	changes := []struct {
		name   string
		change func(path string) error
	}{
		{"rewritten", func(path string) error { return os.WriteFile(path, []byte("example.com be_tampered\n"), 0o600) }},
		{"removed", os.Remove},
		{"made unreadable to HAProxy", func(path string) error { return os.Chmod(path, 0o600) }},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			f := newDriftFixture(t)
			path := filepath.Join(f.baseDir, "maps", "host.map")
			require.NoError(t, tc.change(path))

			completed := f.deploy(t, events.TriggerReasonDriftPrevention)

			require.Equal(t, 1, completed.Succeeded)
			require.NotNil(t, completed.Phases)
			assert.Equal(t, int64(len(mapEntry)), completed.Phases.UploadBytes, "the changed file and nothing else")
			assert.Equal(t, 1.0, f.applies(api.ResultFileOnly))
			restored, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, mapEntry, string(restored))
			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, fs.FileMode(0o644), info.Mode())

			idle := f.deploy(t, events.TriggerReasonDriftPrevention)
			require.Equal(t, 1, idle.Succeeded)
			assert.Zero(t, idle.Phases.UploadBytes, "the repaired pod is idle again")
		})
	}
}
