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
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
)

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) errorLines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var lines []string
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if strings.Contains(line, "level=ERROR") {
			lines = append(lines, line)
		}
	}
	return lines
}

// hangingAgent accepts every request and answers none until the caller gives up.
func hangingAgent(t *testing.T) (endpoint dataplane.Endpoint, requested <-chan struct{}) {
	t.Helper()
	hit := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return dataplane.Endpoint{URL: server.URL, PodName: "haproxy-0", PodNamespace: "haptic", PodUID: "haproxy-0-uid"}, hit
}

func TestInFlightDeploymentCancellationReporting(t *testing.T) {
	tests := []struct {
		name     string
		cancel   func(c *Component, deploymentID string, stop context.CancelFunc)
		reported bool
	}{
		{
			name: "superseded by an endpoint authority change is not a failure",
			cancel: func(c *Component, deploymentID string, _ context.CancelFunc) {
				c.handleDeploymentCancelRequest(events.NewDeploymentCancelRequestEvent(
					deploymentID, events.DeploymentCancelReasonEndpointAuthorityChanged))
			},
		},
		{
			name: "a timed-out deployment is a failure",
			cancel: func(c *Component, deploymentID string, _ context.CancelFunc) {
				c.handleDeploymentCancelRequest(events.NewDeploymentCancelRequestEvent(
					deploymentID, events.DeploymentCancelReasonTimeout))
			},
			reported: true,
		},
		{
			name:     "a deployment cut off by shutdown is a failure",
			cancel:   func(_ *Component, _ string, stop context.CancelFunc) { stop() },
			reported: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &lockedLog{}
			bus := newTestBus(t)
			c := New(bus.EventBus, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
				0, metrics.NewMetrics(prometheus.NewRegistry()))
			endpoint, requested := hangingAgent(t)

			plan, config, aux := renderFor("plan-1", "10.0.0.1", mapEntry)
			event, err := events.NewDeploymentScheduledEventWithCycle(
				mustOccurrenceFor(plan, config, aux, nil), []dataplane.Endpoint{endpoint},
				"rt-cfg-1", "haptic", "pod_discovery", true,
			)
			require.NoError(t, err)

			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			go c.performDeployment(ctx, event)
			<-requested
			tt.cancel(c, event.EventID(), stop)

			var failures int
			var completed *events.DeploymentCompletedEvent
			for completed == nil {
				select {
				case e := <-bus.Events:
					switch e := e.(type) {
					case *events.InstanceDeploymentFailedEvent:
						failures++
					case *events.DeploymentCompletedEvent:
						completed = e
					}
				case <-t.Context().Done():
					t.Fatal("deployment never completed")
				}
			}

			assert.Equal(t, 1, completed.Failed, "a cancelled pod did not take the render and must be retried")
			if tt.reported {
				assert.Equal(t, 1, failures)
				assert.NotEmpty(t, logs.errorLines())
				return
			}
			assert.Zero(t, failures, "a superseded apply is not an instance failure")
			assert.Empty(t, logs.errorLines())
		})
	}
}
