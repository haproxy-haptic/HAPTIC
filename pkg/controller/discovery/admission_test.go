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

package discovery

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/agenttest"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
)

// admissionFixture wires a component against a pod store holding one pod whose
// agent endpoint is the fake's address.
func admissionFixture(t *testing.T, agent *agenttest.Agent) (
	component *Component,
	rejections <-chan busevents.Event,
	podStore types.Store,
	port int,
) {
	t.Helper()
	address, err := net.ResolveTCPAddr("tcp", agent.URL()[len("http://"):])
	require.NoError(t, err)

	bus, _ := testutil.NewTestBusAndLogger()
	component = createTestComponent(t, bus)
	podStore = store.NewMemoryStore(2)
	component.SetPodStore(podStore)
	component.mu.Lock()
	component.dataplanePort = address.Port
	component.hasDataplanePort = true
	component.credentials = &coreconfig.Credentials{
		DataplaneUsername: agent.Username(),
		DataplanePassword: agent.Password(),
	}
	component.hasCredentials = true
	component.initialDiscoveryDone = true
	component.discovery = &Discovery{dataplanePort: address.Port}
	component.mu.Unlock()

	rejections = bus.SubscribeTypes("admission-test", 20, events.EventTypeHAProxyPodRejected)
	bus.Start()
	return component, rejections, podStore, address.Port
}

// A pod with an IP whose agent container is running and whose /v1/state answers
// is admitted, and the HAProxy version it reported travels with the endpoint —
// that is what the fleet's template capabilities are derived from.
func TestAdmission_ReachableAgentIsAdmitted(t *testing.T) {
	agent := agenttest.New(t)
	component, _, podStore, port := admissionFixture(t, agent)
	addPodToStoreWithPort(t, podStore, "haproxy-0", "default", "127.0.0.1", int64(port))

	component.triggerDiscovery("test")

	component.mu.RLock()
	defer component.mu.RUnlock()
	require.Len(t, component.lastEndpoints, 1)
	for _, authority := range component.lastEndpoints {
		assert.Equal(t, "3.4.3", authority.detectedFullVersion)
		assert.Equal(t, 3, authority.detectedMajorVersion)
		assert.Equal(t, 4, authority.detectedMinorVersion)
	}
}

// Whether the pod's agent stores blobs travels with the endpoint too: the
// fleet's template capabilities offer blobs only when every agent does.
func TestAdmission_BlobSupportTravelsWithTheEndpoint(t *testing.T) {
	for name, test := range map[string]struct {
		options []agenttest.Option
		want    bool
	}{
		"agent with blobs":    {options: []agenttest.Option{agenttest.WithBlobFeature()}, want: true},
		"agent without blobs": {want: false},
	} {
		t.Run(name, func(t *testing.T) {
			agent := agenttest.New(t, test.options...)
			component, _, podStore, port := admissionFixture(t, agent)
			addPodToStoreWithPort(t, podStore, "haproxy-0", "default", "127.0.0.1", int64(port))

			component.triggerDiscovery("first")
			component.triggerDiscovery("cached")

			component.mu.RLock()
			defer component.mu.RUnlock()
			require.Len(t, component.lastEndpoints, 1)
			for _, authority := range component.lastEndpoints {
				assert.Equal(t, test.want, authority.acceptsBlobs)
			}
		})
	}
}

// An admitted identity is not probed again: the identity carries the pod's
// container fingerprint, so only a restart or a new address costs a round trip.
func TestAdmission_AdmittedIdentityIsNotProbedAgain(t *testing.T) {
	agent := agenttest.New(t)
	component, _, podStore, port := admissionFixture(t, agent)
	addPodToStoreWithPort(t, podStore, "haproxy-0", "default", "127.0.0.1", int64(port))

	component.triggerDiscovery("first")
	component.triggerDiscovery("second")
	component.triggerDiscovery("third")

	assert.Equal(t, 1, agent.StateReads(),
		"discovery re-runs on every drift tick; probing an unchanged pod each time is a round trip per pod per minute for nothing")
}

// A restarted container is a different identity, so it is probed again: its
// tree and its HAProxy may both be new.
func TestAdmission_RestartedContainerIsProbedAgain(t *testing.T) {
	agent := agenttest.New(t)
	component, _, podStore, port := admissionFixture(t, agent)
	addPodToStoreWithPort(t, podStore, "haproxy-0", "default", "127.0.0.1", int64(port))
	component.triggerDiscovery("first")

	pods, err := podStore.List()
	require.NoError(t, err)
	pod := pods[0].(*unstructured.Unstructured)
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{
		map[string]any{
			"name": agentContainerName, "state": map[string]any{"running": map[string]any{}},
			"imageID": "sha256:agent", "containerID": "containerd://agent-2",
		},
	}, "status", "containerStatuses"))
	require.NoError(t, podStore.Update(pod, []string{"default", "haproxy-0"}))

	component.triggerDiscovery("second")

	assert.Equal(t, 2, agent.StateReads())
}

// A pod whose agent does not answer is not admitted, and the rejection is
// reported so an operator can alert on a fleet the controller cannot reach.
func TestAdmission_UnreachableAgentIsRejected(t *testing.T) {
	agent := agenttest.New(t)
	component, rejections, podStore, port := admissionFixture(t, agent)
	// Port 1 refuses connections; the pod's own port is not the agent's.
	addPodToStoreWithPort(t, podStore, "haproxy-0", "default", "127.0.0.1", int64(port))
	component.mu.Lock()
	component.discovery = &Discovery{dataplanePort: 1}
	component.mu.Unlock()

	component.triggerDiscovery("test")

	rejected := waitForRejection(t, rejections)
	assert.Equal(t, RejectionAgentUnreachable, rejected.Reason)
	assert.Equal(t, "haproxy-0", rejected.PodName)

	component.mu.RLock()
	defer component.mu.RUnlock()
	assert.Empty(t, component.lastEndpoints, "an unreachable pod is not an endpoint to deploy to")
}

// A pod whose agent container has not started is reported under its own reason:
// it is a pod problem, not a network problem.
func TestAdmission_AgentContainerNotRunningIsRejected(t *testing.T) {
	agent := agenttest.New(t)
	component, rejections, podStore, port := admissionFixture(t, agent)
	addPodToStoreWithPort(t, podStore, "haproxy-0", "default", "127.0.0.1", int64(port))

	pods, err := podStore.List()
	require.NoError(t, err)
	pod := pods[0].(*unstructured.Unstructured)
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{
		map[string]any{"name": agentContainerName, "state": map[string]any{"waiting": map[string]any{}}},
	}, "status", "containerStatuses"))
	require.NoError(t, podStore.Update(pod, []string{"default", "haproxy-0"}))

	component.triggerDiscovery("test")

	assert.Equal(t, RejectionAgentNotRunning, waitForRejection(t, rejections).Reason)
	assert.Zero(t, agent.StateReads(), "a container that is not running is not worth a round trip")
}

func waitForRejection(t *testing.T, rejections <-chan busevents.Event) *events.HAProxyPodRejectedEvent {
	t.Helper()
	return testutil.WaitForEvent[*events.HAProxyPodRejectedEvent](t, rejections, testutil.EventTimeout)
}
