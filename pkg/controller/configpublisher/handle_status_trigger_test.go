// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package configpublisher

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/throttle"
)

// handleStatusTrigger is the throttle-decision point for pod status
// updates (the parallel of processPublishWork's throttle, but for
// status-subresource writes instead of CRD spec writes). Three
// branches; only the immediate-process happy path was indirectly
// covered by integration tests, leaving the throttle-window
// decision unverified:
//
//  1. publishInterval <= 0 → process immediately. This is the
//     no-throttle configuration (default for tests/dev). A regression
//     that always entered the refractory branch would defer status
//     updates indefinitely (no timer would ever start because the
//     refractory window is always "in").
//
//  2. publishInterval > 0 + outside refractory window → process
//     immediately (leading-edge throttle). The first status update
//     after idle MUST fire without delay; without this branch the
//     status pipeline would always wait one full refractory period
//     before publishing.
//
//  3. publishInterval > 0 + inside refractory window → defer to
//     timer. This is the load-bearing throttle behavior: pending
//     status updates accumulate in statusWorkPending (already
//     coalesced per pod) until the timer fires. A regression that
//     processed immediately while inside the refractory would
//     defeat the throttle and let status updates hammer the K8s
//     status subresource at reconciliation rate.

// statusTriggerComponent constructs a Component populated only
// with the fields handleStatusTrigger touches. publisher is nil —
// the tested branches reach processAllPendingStatusWork at most,
// which has its own empty-map fast path that exits before the
// publisher is needed.
// statusTriggerComponent constructs a Component plus a throttle whose gate
// state matches the desired branch. Callers pass either:
//   - interval=0 (throttle disabled — gate always open)
//   - markFired=false (fresh throttle — gate open, "outside refractory")
//   - markFired=true  (throttle fired just now — gate closed, "inside refractory")
func statusTriggerComponent(t *testing.T, interval time.Duration, markFired bool) *Component {
	t.Helper()
	gate := throttle.New(interval)
	t.Cleanup(gate.Stop)
	if markFired {
		gate.MarkFired()
	}
	return &Component{
		logger:            testutil.NewTestLogger(),
		statusWorkPending: make(map[string]*statusWorkItem),
		publishInterval:   interval,
		statusThrottle:    gate,
	}
}

func TestHandleStatusTrigger_NoThrottleProcessesImmediately(t *testing.T) {
	// publishInterval=0 disables throttling entirely. The function
	// MUST call processAllPendingStatusWork directly. With an empty
	// pending map, processAllPendingStatusWork has its own fast
	// path that exits cleanly — no panic.
	c := statusTriggerComponent(t, 0, false)

	require.NotPanics(t, func() { c.handleStatusTrigger(t.Context()) },
		"publishInterval=0 must take the immediate-process branch — "+
			"a regression that always entered the refractory branch would "+
			"defer status updates indefinitely (no timer would ever start "+
			"because the refractory window is always 'in')")
}

func TestHandleStatusTrigger_OutsideRefractoryProcessesImmediately(t *testing.T) {
	// publishInterval=10s, fresh throttle (never fired) → fully
	// outside the refractory window. Leading-edge throttle MUST
	// fire immediately. Empty pending map again so
	// processAllPendingStatusWork's fast path keeps the test
	// self-contained.
	c := statusTriggerComponent(t, 10*time.Second, false)

	require.NotPanics(t, func() { c.handleStatusTrigger(t.Context()) },
		"outside refractory MUST process immediately (leading-edge throttle) "+
			"— without this branch every first status write after idle would "+
			"wait one full refractory period before publishing, defeating "+
			"the leading-edge guarantee")
}

func TestHandleStatusTrigger_InsideRefractoryDefersToTimer(t *testing.T) {
	// publishInterval=10s, throttle marked fired just now → inside the
	// refractory. Pre-seed pending work with a sentinel: if a
	// regression caused processAllPendingStatusWork to be called,
	// the sentinel would be drained from the map (and the function
	// would crash trying to call processStatusWork on a nil
	// publisher). The defer-to-timer contract requires the sentinel
	// to remain in the map.
	c := statusTriggerComponent(t, 10*time.Second, true)
	const podKey = "haptic/rt-cfg/haproxy-pod-1"
	sentinel := &statusWorkItem{
		event: events.NewConfigAppliedToPodEvent(
			"rt-cfg", "haptic", "haproxy-pod-1", "haptic",
			"", "", "checksum-abc", false, nil,
		),
	}
	c.statusWorkMu.Lock()
	c.statusWorkPending[podKey] = sentinel
	c.statusWorkMu.Unlock()

	require.NotPanics(t, func() { c.handleStatusTrigger(t.Context()) },
		"inside-refractory MUST take the timer-schedule branch — a "+
			"regression that called processAllPendingStatusWork directly "+
			"would crash on the nil publisher reachable from processStatusWork")

	c.statusWorkMu.Lock()
	defer c.statusWorkMu.Unlock()
	got, stillPresent := c.statusWorkPending[podKey]
	require.True(t, stillPresent,
		"the sentinel MUST remain in statusWorkPending — a regression that "+
			"processed inside the refractory window would defeat the throttle "+
			"and let status updates hammer the K8s status subresource at "+
			"reconciliation rate (the very pressure the throttle is supposed "+
			"to prevent)")
	assert.Same(t, sentinel, got,
		"the original work item pointer MUST be preserved (no rewrap) — "+
			"the throttle is purely about *when* to publish, not what to publish")
}
