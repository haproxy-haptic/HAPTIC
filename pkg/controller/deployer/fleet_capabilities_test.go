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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
)

func fleetOf(versions ...string) []dataplane.Endpoint {
	endpoints := make([]dataplane.Endpoint, 0, len(versions))
	for i, version := range versions {
		endpoints = append(endpoints, dataplane.Endpoint{
			URL:                 "http://10.0.0." + string(rune('1'+i)) + ":5555",
			PodName:             "haproxy-" + string(rune('0'+i)),
			DetectedFullVersion: version,
		})
	}
	return endpoints
}

// A render must never use a feature the oldest pod would reject, so the fleet's
// lowest reported version is what templates see — and the value only reaches
// them through a render, which is why the change triggers one.
func TestFleetCapabilities_LowestVersionWins(t *testing.T) {
	bus := testutil.NewTestBus()
	triggers := bus.SubscribeTypes("capability-watch", 4, events.EventTypeReconciliationTriggered)
	bus.Start()
	scheduler := newDeploymentScheduler(bus, testutil.NewTestLogger(), 0, 30*time.Second)
	sink := &recordingPlanSink{}
	scheduler.capabilities = sink

	scheduler.publishFleetCapabilities(fleetOf("3.4.3", "3.0.9", "3.2.1"))

	require.Len(t, sink.capabilities, 1)
	assert.False(t, sink.capabilities[0].SupportsCrtList,
		"3.0 has no crt-list storage, and one pod on 3.0 is the whole fleet's answer")
	assert.True(t, sink.capabilities[0].SupportsMapStorage)
	trigger := testutil.WaitForEvent[*events.ReconciliationTriggeredEvent](t, triggers, testutil.EventTimeout)
	assert.Equal(t, "fleet_capabilities_changed", trigger.Reason)
}

// An unchanged fleet must not re-render: the capability set is the same, and a
// reconcile per discovery pass would be a render per drift tick.
func TestFleetCapabilities_UnchangedFleetSaysNothing(t *testing.T) {
	bus := testutil.NewTestBus()
	bus.Start()
	scheduler := newDeploymentScheduler(bus, testutil.NewTestLogger(), 0, 30*time.Second)
	sink := &recordingPlanSink{}
	scheduler.capabilities = sink

	scheduler.publishFleetCapabilities(fleetOf("3.4.3"))
	scheduler.publishFleetCapabilities(fleetOf("3.4.3"))

	assert.Len(t, sink.capabilities, 1)
}

// A fleet that reports no readable version leaves the bootstrap value alone:
// the controller image's own HAProxy is a better answer than all-false.
func TestFleetCapabilities_UnreadableVersionsKeepTheBootstrap(t *testing.T) {
	bus := testutil.NewTestBus()
	bus.Start()
	scheduler := newDeploymentScheduler(bus, testutil.NewTestLogger(), 0, 30*time.Second)
	sink := &recordingPlanSink{}
	scheduler.capabilities = sink

	scheduler.publishFleetCapabilities(fleetOf("", ""))

	assert.Empty(t, sink.capabilities)
}

// Blobs are offered to templates only while every pod's agent stores them, so
// a render never references a file one pod could not receive (ADR-0041). The
// answer changing on its own is a capability change.
func TestFleetCapabilities_BlobsOnlyWhenEveryAgentStoresThem(t *testing.T) {
	bus := testutil.NewTestBus()
	bus.Start()
	scheduler := newDeploymentScheduler(bus, testutil.NewTestLogger(), 0, 30*time.Second)
	sink := &recordingPlanSink{}
	scheduler.capabilities = sink
	fleet := fleetOf("3.4.3", "3.4.3")
	fleet[0].AcceptsBlobs = true

	scheduler.publishFleetCapabilities(fleet)
	fleet[1].AcceptsBlobs = true
	scheduler.publishFleetCapabilities(fleet)

	require.Len(t, sink.capabilities, 2)
	assert.False(t, sink.capabilities[0].SupportsBlobs, "one older agent is the whole fleet's answer")
	assert.True(t, sink.capabilities[1].SupportsBlobs)
}
