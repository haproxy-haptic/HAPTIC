// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package configpublisher

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
)

func TestConfigAppliedStatusRejectsRetiredPodUID(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	c := New(nil, bus, logger)
	c.handlePodsDiscovered(t.Context(), events.NewHAProxyPodsDiscoveredEvent([]dataplane.Endpoint{{
		PodName: "haproxy-0", PodNamespace: "haptic", PodUID: "uid-new", PodRuntimeID: "runtime-new",
	}}, 1))

	c.handleConfigAppliedToPod(events.NewConfigAppliedToPodEvent(
		"cfg", "haptic", "haproxy-0", "haptic", "uid-old", "runtime-old", "checksum", false, nil,
	))
	c.statusWorkMu.Lock()
	assert.Empty(t, c.statusWorkPending)
	c.statusWorkMu.Unlock()

	c.handleConfigAppliedToPod(events.NewConfigAppliedToPodEvent(
		"cfg", "haptic", "haproxy-0", "haptic", "uid-new", "runtime-old", "checksum", false, nil,
	))
	c.statusWorkMu.Lock()
	assert.Empty(t, c.statusWorkPending)
	c.statusWorkMu.Unlock()

	c.handleConfigAppliedToPod(events.NewConfigAppliedToPodEvent(
		"cfg", "haptic", "haproxy-0", "haptic", "uid-new", "runtime-new", "checksum", false, nil,
	))
	c.statusWorkMu.Lock()
	defer c.statusWorkMu.Unlock()
	require.Len(t, c.statusWorkPending, 1)
	for _, work := range c.statusWorkPending {
		assert.Equal(t, "uid-new", work.event.PodUID)
		assert.Equal(t, "runtime-new", work.event.PodRuntimeID)
	}
}

func TestQueuedConfigAppliedStatusRejectsRetiredPodRuntime(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	c := New(nil, bus, logger)
	oldEndpoint := dataplane.Endpoint{
		PodName: "haproxy-0", PodNamespace: "haptic", PodUID: "uid-same", PodRuntimeID: "runtime-old",
	}
	c.handlePodsDiscovered(t.Context(), events.NewHAProxyPodsDiscoveredEvent([]dataplane.Endpoint{oldEndpoint}, 1))
	stale := &statusWorkItem{event: events.NewConfigAppliedToPodEvent(
		"cfg", "haptic", "haproxy-0", "haptic", "uid-same", "runtime-old", "checksum", false, nil,
	)}

	replacement := oldEndpoint
	replacement.PodRuntimeID = "runtime-new"
	c.handlePodsDiscovered(t.Context(), events.NewHAProxyPodsDiscoveredEvent([]dataplane.Endpoint{replacement}, 1))

	assert.False(t, c.isCurrentPodAuthority("haptic", "haproxy-0", "uid-same", "runtime-old"))
	require.NotPanics(t, func() { c.processStatusWork(t.Context(), stale) })
}
