// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package discovery

import (
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/agenttest"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
)

type blockingFirstListStore struct {
	types.Store
	firstStarted chan struct{}
	releaseFirst chan struct{}
	calls        atomic.Int32
}

func (s *blockingFirstListStore) List() ([]any, error) {
	resources, err := s.Store.List()
	if err != nil {
		return nil, err
	}
	if s.calls.Add(1) == 1 {
		close(s.firstStarted)
		<-s.releaseFirst
	}
	return resources, nil
}

// An admitted pod keeps its admission across a credential rotation — the
// identity is the pod, not the secret — but every endpoint published after the
// rotation must carry the NEW credentials. Publishing the cached endpoint
// verbatim would send the deployer to authenticate with the retired pair.
func TestCredentialsUpdatedPublishesFreshEndpointCredentials(t *testing.T) {
	bus, _ := testutil.NewTestBusAndLogger()
	component := createTestComponent(t, bus)
	podStore := createTestPodStore(t, []string{"127.0.0.1"})
	component.SetPodStore(podStore)

	identity := endpointIdentity{
		podNamespace: "default",
		podName:      "haproxy-0",
		podUID:       "haproxy-0-uid",
		url:          "http://127.0.0.1:5555",
	}
	component.mu.Lock()
	component.dataplanePort = 5555
	component.hasDataplanePort = true
	component.initialDiscoveryDone = true
	component.discovery = &Discovery{dataplanePort: 5555}
	component.admitted[identity] = agentReport{version: "3.4.3"}
	component.mu.Unlock()

	eventChannel := bus.Subscribe("credential-authority-test", 10)
	bus.Start()
	component.handleCredentialsUpdated(events.NewCredentialsUpdatedEvent(&coreconfig.Credentials{
		DataplaneUsername: "rotated-user",
		DataplanePassword: "rotated-password",
	}, "secret-v2"))

	discovered := testutil.WaitForEvent[*events.HAProxyPodsDiscoveredEvent](t, eventChannel, testutil.EventTimeout)
	require.Len(t, discovered.Endpoints, 1)
	assert.Equal(t, dataplane.Endpoint{
		URL:                  identity.url,
		Username:             "rotated-user",
		Password:             "rotated-password",
		PodName:              identity.podName,
		PodNamespace:         identity.podNamespace,
		PodUID:               identity.podUID,
		DetectedMajorVersion: 3,
		DetectedMinorVersion: 4,
		DetectedFullVersion:  "3.4.3",
	}, discovered.Endpoints[0])
}

// A discovery pass that started before a credential rotation must not publish
// its result after it: the endpoints it carries authenticate with the retired
// pair, and the deployer would keep using them until the next pass.
func TestCredentialsUpdateCannotBeOverwrittenByAnOlderDiscoveryPass(t *testing.T) {
	bus, _ := testutil.NewTestBusAndLogger()
	component := createTestComponent(t, bus)
	podStore := &blockingFirstListStore{
		Store:        createTestPodStore(t, []string{"127.0.0.1"}),
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	component.SetPodStore(podStore)

	identity := endpointIdentity{
		podNamespace: "default",
		podName:      "haproxy-0",
		podUID:       "haproxy-0-uid",
		url:          "http://127.0.0.1:5555",
	}
	component.mu.Lock()
	component.dataplanePort = 5555
	component.hasDataplanePort = true
	component.initialDiscoveryDone = true
	component.credentials = &coreconfig.Credentials{
		DataplaneUsername: "old-user",
		DataplanePassword: "old-password",
	}
	component.hasCredentials = true
	component.discovery = &Discovery{dataplanePort: 5555}
	component.admitted[identity] = agentReport{version: "3.4.3"}
	component.mu.Unlock()

	eventChannel := bus.Subscribe("ordered-credential-authority-test", 10)
	bus.Start()
	oldDone := make(chan struct{})
	go func() {
		component.triggerDiscovery("drift_prevention")
		close(oldDone)
	}()
	select {
	case <-podStore.firstStarted:
	case <-time.After(testutil.LongTimeout):
		t.Fatal("older discovery did not start")
	}

	newDone := make(chan struct{})
	go func() {
		component.handleCredentialsUpdated(events.NewCredentialsUpdatedEvent(&coreconfig.Credentials{
			DataplaneUsername: "new-user",
			DataplanePassword: "new-password",
		}, "secret-v2"))
		close(newDone)
	}()
	close(podStore.releaseFirst)

	for _, done := range []<-chan struct{}{oldDone, newDone} {
		select {
		case <-done:
		case <-time.After(testutil.LongTimeout):
			t.Fatal("discovery did not complete")
		}
	}

	first := testutil.WaitForEvent[*events.HAProxyPodsDiscoveredEvent](t, eventChannel, testutil.EventTimeout)
	second := testutil.WaitForEvent[*events.HAProxyPodsDiscoveredEvent](t, eventChannel, testutil.EventTimeout)
	require.Len(t, first.Endpoints, 1)
	require.Len(t, second.Endpoints, 1)
	assert.Equal(t, "old-user", first.Endpoints[0].Username)
	assert.Equal(t, "new-user", second.Endpoints[0].Username)
	assert.Equal(t, "new-password", second.Endpoints[0].Password)
}

// The same ordering rule for the agent port: a pass that started against the
// old port must not overwrite the result computed for the new one.
func TestDataplanePortUpdateCannotBeOverwrittenByAnOlderDiscoveryPass(t *testing.T) {
	agent := agenttest.New(t, agenttest.WithCredentials("admin", "password"))
	newPort := portOf(t, agent.URL())

	bus, _ := testutil.NewTestBusAndLogger()
	component := createTestComponent(t, bus)
	podStore := &blockingFirstListStore{
		Store:        createTestPodStore(t, []string{"127.0.0.1"}),
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	component.SetPodStore(podStore)

	oldIdentity := endpointIdentity{
		podNamespace: "default",
		podName:      "haproxy-0",
		podUID:       "haproxy-0-uid",
		url:          "http://127.0.0.1:5555",
	}
	component.mu.Lock()
	component.dataplanePort = 5555
	component.hasDataplanePort = true
	component.initialDiscoveryDone = true
	component.credentials = &coreconfig.Credentials{
		DataplaneUsername: "admin",
		DataplanePassword: "password",
	}
	component.hasCredentials = true
	component.discovery = &Discovery{dataplanePort: 5555}
	component.admitted[oldIdentity] = agentReport{version: "3.4.3"}
	component.mu.Unlock()

	eventChannel := bus.Subscribe("ordered-port-authority-test", 10)
	bus.Start()
	oldDone := make(chan struct{})
	go func() {
		component.triggerDiscovery("drift_prevention")
		close(oldDone)
	}()
	select {
	case <-podStore.firstStarted:
	case <-time.After(testutil.LongTimeout):
		t.Fatal("older discovery did not start")
	}

	newDone := make(chan struct{})
	go func() {
		component.handleConfigValidated(events.NewConfigValidatedEvent(&coreconfig.Config{
			Dataplane: coreconfig.DataplaneConfig{Port: newPort},
		}, nil, "v2", "secret-v1"))
		close(newDone)
	}()
	close(podStore.releaseFirst)

	for _, done := range []<-chan struct{}{oldDone, newDone} {
		select {
		case <-done:
		case <-time.After(testutil.LongTimeout):
			t.Fatal("discovery did not complete")
		}
	}

	first := testutil.WaitForEvent[*events.HAProxyPodsDiscoveredEvent](t, eventChannel, testutil.EventTimeout)
	second := testutil.WaitForEvent[*events.HAProxyPodsDiscoveredEvent](t, eventChannel, testutil.EventTimeout)
	require.Len(t, first.Endpoints, 1)
	require.Len(t, second.Endpoints, 1)
	assert.Equal(t, oldIdentity.url, first.Endpoints[0].URL)
	assert.Equal(t, fmt.Sprintf("http://127.0.0.1:%d", newPort), second.Endpoints[0].URL)
}

func portOf(t *testing.T, url string) int {
	t.Helper()
	address, err := net.ResolveTCPAddr("tcp", url[len("http://"):])
	require.NoError(t, err)
	return address.Port
}
