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
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
)

const (
	firstVerdictWaitLog = "Waiting for the render gate's first verdict"
	gateFailedHoldLog   = "Render gate did not pass the last render"
)

func TestScheduler_WaitingForFirstGateVerdictIsNotAWarning(t *testing.T) {
	logger, logs := testutil.NewRecordingLogger()
	scheduler, scheduled, ctx := gateLatchSchedulerWithLogger(t, logger)
	scheduler.retained = &retainedRecovery{}
	scheduler.retainedRecovering = true

	scheduler.handleEvent(ctx, renderEvent("cold-first"))
	scheduler.handleEvent(ctx, renderEvent("cold-second"))

	requireNothingScheduled(t, scheduled)
	assert.Equal(t, []slog.Level{slog.LevelInfo, slog.LevelInfo}, logs.Levels(firstVerdictWaitLog))
	assert.Empty(t, logs.Levels(gateFailedHoldLog))
	assert.Empty(t, logs.Levels("Render gate is holding renders"))
}

func TestScheduler_HoldingAfterAFailedGateVerdictWarns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		plan    string
		refused bool
		cold    bool
	}{
		{name: "refused by HAProxy", plan: "refused", refused: true},
		{name: "check could not run", plan: "unchecked"},
		{name: "cold term refused", plan: "cold-refused", refused: true, cold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := testutil.NewRecordingLogger()
			scheduler, scheduled, ctx := gateLatchSchedulerWithLogger(t, logger)
			if tc.cold {
				scheduler.retained = &retainedRecovery{}
				scheduler.retainedRecovering = true
			}

			scheduler.handleEvent(ctx, renderEvent("bad-"+tc.plan))
			if !tc.cold {
				requireScheduled(t, scheduled, "bad-"+tc.plan)
			}
			scheduler.handleEvent(ctx, gateEvent("bad-"+tc.plan, false, tc.refused, true, "boom"))
			scheduler.handleEvent(ctx, renderEvent("held-"+tc.plan))

			requireNothingScheduled(t, scheduled)
			assert.Equal(t, []slog.Level{slog.LevelWarn}, logs.Levels(gateFailedHoldLog))
		})
	}
}
