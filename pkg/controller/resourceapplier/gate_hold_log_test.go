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

package resourceapplier

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

const gateFailedHoldLog = "Render gate did not pass the last render"

func TestHandleReconciliationCompleted_GateHoldLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failedFirst bool
		want        []slog.Level
	}{
		{name: "no verdict yet applies without a hold"},
		{name: "failed verdict holds and warns", failedFirst: true, want: []slog.Level{slog.LevelWarn}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := testutil.NewRecordingLogger()
			client, counter := newClientWithPatchCounter()
			comp := New(&Config{
				EventBus: testutil.NewTestBus(), DynamicClient: client, GVRResolver: newResolver(),
				Logger: logger, OwnNamespace: "haptic",
			})
			setLeader(comp)
			fixture := newResourceCycleFixture(t)
			first := fixture.completed(t, "config-a", nil, nil, nil)
			if tc.failedFirst {
				comp.handleRenderGateCompleted(context.Background(),
					renderGateCompletedEvent(t, first, false, true, true))
			}

			next := fixture.completed(
				t, "config-b", []templating.RenderedResource{sampleResource("haptic", "svc-a", 80)},
				nil, eventCycleSnapshot(t, first),
			)
			comp.handleReconciliationCompleted(context.Background(), next)

			require.Equal(t, !tc.failedFirst, counter.Load() == 1)
			assert.Equal(t, tc.want, logs.Levels(gateFailedHoldLog))
		})
	}
}
