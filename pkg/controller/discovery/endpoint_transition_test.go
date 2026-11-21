// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package discovery

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/component"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/leadership"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
)

func TestBecameLeaderReplaySerializesWithDiscoveryPublication(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	discoveredCh := bus.SubscribeTypes("ordered-replay", 10, events.EventTypeHAProxyPodsDiscovered)
	c := &Component{
		lastEndpoints:      make(map[podIdentity]endpointAuthority),
		discoveredReplayer: leadership.NewStateReplayer[*events.HAProxyPodsDiscoveredEvent](bus),
	}
	c.Base = component.New(&component.Config{
		EventBus: bus, Logger: logger, Name: ComponentName, BufferSize: 10, Handler: c,
	})
	oldEvent := events.NewHAProxyPodsDiscoveredEvent([]dataplane.Endpoint{{PodUID: "uid-old"}}, 1)
	newEvent := events.NewHAProxyPodsDiscoveredEvent([]dataplane.Endpoint{{PodUID: "uid-new"}}, 1)
	c.discoveredReplayer.Cache(oldEvent)
	bus.Start()

	c.discoveryMu.Lock()
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(entered)
		c.handleBecameLeader(events.NewBecameLeaderEvent("leader"))
		close(done)
	}()
	<-entered
	select {
	case <-done:
		c.discoveryMu.Unlock()
		t.Fatal("leader replay bypassed serialized discovery publication")
	case <-time.After(testutil.StartupDelay):
	}
	c.discoveredReplayer.Cache(newEvent)
	c.discoveryMu.Unlock()

	select {
	case <-done:
	case <-time.After(testutil.EventTimeout):
		t.Fatal("leader replay did not finish")
	}
	replayed := testutil.WaitForEvent[*events.HAProxyPodsDiscoveredEvent](t, discoveredCh, testutil.EventTimeout)
	require.Len(t, replayed.Endpoints, 1)
	assert.Equal(t, "uid-new", replayed.Endpoints[0].PodUID)
}

func TestPublishDiscoveryResult_PodReplacementTerminatesPredecessorUID(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	terminatedCh := bus.SubscribeTypes("terminated", 10, events.EventTypeHAProxyPodTerminated)
	c := &Component{
		lastEndpoints:      make(map[podIdentity]endpointAuthority),
		discoveredReplayer: leadership.NewStateReplayer[*events.HAProxyPodsDiscoveredEvent](bus),
	}
	c.Base = component.New(&component.Config{
		EventBus: bus, Logger: logger, Name: ComponentName, BufferSize: 10, Handler: c,
	})
	bus.Start()

	oldEndpoint := &dataplane.Endpoint{
		URL: "http://10.0.0.1:5555/v3", Username: "admin", Password: "secret",
		PodName: "haproxy-0", PodNamespace: "haptic", PodUID: "uid-old",
		DetectedMajorVersion: 3, DetectedMinorVersion: 2, DetectedFullVersion: "3.2.1",
	}
	c.publishDiscoveryResult("initial", 1, []*dataplane.Endpoint{oldEndpoint}, nil)
	replacement := *oldEndpoint
	replacement.PodUID = "uid-new"
	c.publishDiscoveryResult("replacement", 1, []*dataplane.Endpoint{&replacement}, nil)

	event := testutil.WaitForEvent[*events.HAProxyPodTerminatedEvent](t, terminatedCh, testutil.EventTimeout)
	require.NotNil(t, event)
	assert.Equal(t, "haproxy-0", event.PodName)
	assert.Equal(t, "haptic", event.PodNamespace)
	assert.Equal(t, "uid-old", event.PodUID)
}

func TestEndpointAuthorityIncludesConnectionAndPodIdentity(t *testing.T) {
	base := dataplane.Endpoint{
		URL: "http://10.0.0.1:5555/v3", Username: "admin", Password: "secret",
		PodName: "haproxy-0", PodNamespace: "haptic", PodUID: "uid-1",
		DetectedMajorVersion: 3, DetectedMinorVersion: 2, DetectedFullVersion: "3.2.1",
	}
	tests := []struct {
		name   string
		mutate func(*dataplane.Endpoint)
	}{
		{name: "URL", mutate: func(endpoint *dataplane.Endpoint) { endpoint.URL = "http://10.0.0.2:5555/v3" }},
		{name: "username", mutate: func(endpoint *dataplane.Endpoint) { endpoint.Username = "other" }},
		{name: "password", mutate: func(endpoint *dataplane.Endpoint) { endpoint.Password = "rotated" }},
		{name: "pod name", mutate: func(endpoint *dataplane.Endpoint) { endpoint.PodName = "haproxy-1" }},
		{name: "pod namespace", mutate: func(endpoint *dataplane.Endpoint) { endpoint.PodNamespace = "other" }},
		{name: "pod UID", mutate: func(endpoint *dataplane.Endpoint) { endpoint.PodUID = "uid-2" }},
		{name: "pod runtime", mutate: func(endpoint *dataplane.Endpoint) { endpoint.PodRuntimeID = "runtime-2" }},
		{name: "major version", mutate: func(endpoint *dataplane.Endpoint) { endpoint.DetectedMajorVersion = 4 }},
		{name: "minor version", mutate: func(endpoint *dataplane.Endpoint) { endpoint.DetectedMinorVersion = 3 }},
		{name: "full version", mutate: func(endpoint *dataplane.Endpoint) { endpoint.DetectedFullVersion = "3.2.2" }},
		{name: "blob support", mutate: func(endpoint *dataplane.Endpoint) { endpoint.AcceptsBlobs = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			test.mutate(&changed)
			assert.NotEqual(t, endpointAuthorityOf(&base), endpointAuthorityOf(&changed))
		})
	}
}
