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

package endpointrouting

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	namespace = "haptic"
	ownUID    = types.UID("own-pod")
	ownNode   = "node-a"
)

// kubeProxies fakes every node's healthz. A node's answer is computed per call
// from the node's clock offset and the age of its last sync.
type kubeProxies struct {
	mu    sync.Mutex
	nodes map[string]*fakeKubeProxy
}

type fakeKubeProxy struct {
	skew     time.Duration
	syncedAt time.Time
	families []discoveryv1.AddressType
	err      error
}

func (k *kubeProxies) set(address string, proxy *fakeKubeProxy) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.nodes == nil {
		k.nodes = map[string]*fakeKubeProxy{}
	}
	k.nodes[address] = proxy
}

func (k *kubeProxies) read(_ context.Context, address string) (Healthz, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	proxy, ok := k.nodes[address]
	if !ok {
		return Healthz{}, fmt.Errorf("%w: connection refused", ErrNoKubeProxy)
	}
	if proxy.err != nil {
		return Healthz{}, proxy.err
	}
	health := Healthz{CurrentTime: time.Now().Add(proxy.skew), Synced: map[discoveryv1.AddressType]time.Time{}}
	for _, family := range proxy.families {
		health.Synced[family] = proxy.syncedAt.Add(proxy.skew)
	}
	return health, nil
}

func node(name, address string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}}},
	}
}

func service(name string, mutate ...func(*corev1.Service)) *corev1.Service {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort}}
	for _, m := range mutate {
		m(svc)
	}
	return svc
}

type endpoint struct {
	uid        types.UID
	node       string
	ready      bool
	notServing bool
}

func slice(name, svc string, endpoints ...endpoint) *discoveryv1.EndpointSlice {
	s := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{discoveryv1.LabelServiceName: svc},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	for _, e := range endpoints {
		ready, terminating := e.ready, !e.ready
		s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{
			Addresses:  []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready, Serving: new(!e.notServing), Terminating: &terminating},
			NodeName:   new(e.node),
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", UID: e.uid},
		})
	}
	return s
}

type fixture struct {
	observer *Observer
	client   *fake.Clientset
	proxies  *kubeProxies
}

func start(t *testing.T, objects ...runtime.Object) *fixture {
	t.Helper()
	client := fake.NewClientset(objects...)
	proxies := &kubeProxies{}
	observer, err := New(&Config{Client: client, Namespace: namespace, PodUID: ownUID, NodeName: ownNode, Healthz: proxies.read})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = observer.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	require.Eventually(t, func() bool {
		for _, synced := range observer.synced {
			if !synced() {
				return false
			}
		}
		return true
	}, 5*time.Second, 5*time.Millisecond)
	return &fixture{observer: observer, client: client, proxies: proxies}
}

func (f *fixture) changedAt() time.Time {
	f.observer.mu.Lock()
	defer f.observer.mu.Unlock()
	return f.observer.changed["haproxy"]
}

func newPoll() *nodePoll {
	return &nodePoll{confirmed: map[string]time.Time{}, answered: map[string]bool{}}
}

func (f *fixture) routing(t *testing.T, poll *nodePoll) Routing {
	t.Helper()
	return f.observer.routing(t.Context(), poll)
}

var ipv4 = []discoveryv1.AddressType{discoveryv1.AddressTypeIPv4}

func terminatingWithAReadyPeer() []runtime.Object {
	return []runtime.Object{
		node(ownNode, "192.0.2.1"), node("node-b", "192.0.2.2"),
		service("haproxy"),
		slice("haproxy-1", "haproxy", endpoint{uid: ownUID, node: ownNode}, endpoint{uid: "peer", node: "node-b", ready: true}),
	}
}

func TestAReadyEndpointIsStillRouted(t *testing.T) {
	f := start(t, node(ownNode, "192.0.2.1"), service("haproxy"),
		slice("haproxy-1", "haproxy", endpoint{uid: ownUID, node: ownNode, ready: true}, endpoint{uid: "peer", node: "node-b", ready: true}))
	routing := f.routing(t, newPoll())
	assert.True(t, routing.Routed)
	assert.Contains(t, routing.Detail, "Service haproxy still lists this pod as ready")
}

func TestTheRoutingIsWithdrawnOnceEveryKubeProxySyncedAfterTheChange(t *testing.T) {
	f := start(t, terminatingWithAReadyPeer()...)
	changed := f.changedAt()
	require.False(t, changed.IsZero())
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: changed.Add(10 * time.Millisecond), families: ipv4})
	f.proxies.set("192.0.2.2", &fakeKubeProxy{syncedAt: changed.Add(-time.Second), families: ipv4})
	poll := newPoll()

	routing := f.routing(t, poll)
	assert.True(t, routing.Routed, "node-b's kube-proxy last synced before the change")
	assert.Contains(t, routing.Detail, "node-b")
	assert.NotContains(t, routing.Detail, ownNode)

	f.proxies.set("192.0.2.2", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	routing = f.routing(t, poll)
	assert.Equal(t, Routing{Detail: "kube-proxy synced on all 2 nodes"}, routing)
}

func TestNodeClockSkewDoesNotConfirmAStaleSync(t *testing.T) {
	f := start(t, terminatingWithAReadyPeer()...)
	changed := f.changedAt()
	f.proxies.set("192.0.2.1", &fakeKubeProxy{skew: time.Hour, syncedAt: changed.Add(-50 * time.Millisecond), families: ipv4})
	f.proxies.set("192.0.2.2", &fakeKubeProxy{skew: -time.Hour, syncedAt: time.Now(), families: ipv4})
	routing := f.routing(t, newPoll())
	assert.True(t, routing.Routed)
	assert.Contains(t, routing.Detail, ownNode)
	assert.NotContains(t, routing.Detail, "node-b")
}

func TestALaterChangeToThePodsEndpointsNeedsANewSync(t *testing.T) {
	f := start(t, terminatingWithAReadyPeer()...)
	first := f.changedAt()
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	f.proxies.set("192.0.2.2", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	poll := newPoll()
	require.False(t, f.routing(t, poll).Routed)

	updated := slice("haproxy-1", "haproxy", endpoint{uid: ownUID, node: ownNode}, endpoint{uid: "peer", node: "node-b", ready: true}, endpoint{uid: "surge", node: ownNode, ready: true})
	_, err := f.client.DiscoveryV1().EndpointSlices(namespace).Update(t.Context(), updated, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return f.changedAt().After(first) }, 5*time.Second, 5*time.Millisecond)

	routing := f.routing(t, poll)
	assert.True(t, routing.Routed, "both nodes were confirmed only for the earlier change")
	assert.Contains(t, routing.Detail, ownNode)
	assert.Contains(t, routing.Detail, "node-b")
}

func TestASliceVersionTheHandlerHasNotTimedHoldsTheVerdict(t *testing.T) {
	f := start(t, terminatingWithAReadyPeer()...)
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	f.proxies.set("192.0.2.2", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	f.observer.mu.Lock()
	for uid := range f.observer.handled {
		f.observer.handled[uid] = "older"
	}
	f.observer.mu.Unlock()
	routing := f.routing(t, newPoll())
	assert.True(t, routing.Routed, "the lister is ahead of the change time, so no sync can be judged against it")
	assert.Contains(t, routing.Detail, "still being processed")
}

func TestKubeProxyKeepsRoutingToTheLastServingEndpoint(t *testing.T) {
	f := start(t, node(ownNode, "192.0.2.1"), service("haproxy"),
		slice("haproxy-1", "haproxy", endpoint{uid: ownUID, node: ownNode}, endpoint{uid: "peer", node: "node-b"}))
	routing := f.routing(t, newPoll())
	assert.True(t, routing.Routed)
	assert.Contains(t, routing.Detail, "no other ready IPv4 endpoint")
}

func TestKubeProxyDoesNotFallBackToAPodThatStoppedServing(t *testing.T) {
	f := start(t, node(ownNode, "192.0.2.1"), service("haproxy"),
		slice("haproxy-1", "haproxy", endpoint{uid: ownUID, node: ownNode, notServing: true}, endpoint{uid: "peer", node: "node-b"}))
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	assert.False(t, f.routing(t, newPoll()).Routed)
}

func TestALocalTrafficPolicyNeedsAReadyPeerOnTheSameNode(t *testing.T) {
	local := func(svc *corev1.Service) { svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal }
	f := start(t, node(ownNode, "192.0.2.1"), service("haproxy", local),
		slice("haproxy-1", "haproxy", endpoint{uid: ownUID, node: ownNode}, endpoint{uid: "peer", node: "node-b", ready: true}))
	routing := f.routing(t, newPoll())
	assert.True(t, routing.Routed)
	assert.Contains(t, routing.Detail, "Local traffic policy")

	f = start(t, node(ownNode, "192.0.2.1"), service("haproxy", local),
		slice("haproxy-1", "haproxy", endpoint{uid: ownUID, node: ownNode}, endpoint{uid: "peer", node: ownNode, ready: true}))
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	assert.False(t, f.routing(t, newPoll()).Routed)
}

func TestAnInternalLocalTrafficPolicyNeedsAReadyPeerOnTheSameNode(t *testing.T) {
	f := start(t, node(ownNode, "192.0.2.1"),
		service("haproxy", func(svc *corev1.Service) {
			svc.Spec.Type = corev1.ServiceTypeClusterIP
			svc.Spec.InternalTrafficPolicy = new(corev1.ServiceInternalTrafficPolicyLocal)
		}),
		slice("haproxy-1", "haproxy", endpoint{uid: ownUID, node: ownNode}, endpoint{uid: "peer", node: "node-b", ready: true}))
	routing := f.routing(t, newPoll())
	assert.True(t, routing.Routed)
	assert.Contains(t, routing.Detail, "Local traffic policy")
}

func TestAServiceWhoseTrafficPolicyCannotBeReadIsUnverified(t *testing.T) {
	f := start(t, node(ownNode, "192.0.2.1"),
		slice("haproxy-1", "haproxy", endpoint{uid: ownUID, node: ownNode}, endpoint{uid: "peer", node: "node-b", ready: true}))
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	routing := f.routing(t, newPoll())
	assert.False(t, routing.Routed)
	assert.Contains(t, routing.Unverified, `traffic policy of Service "haproxy"`)
}

func TestServicesKubeProxyIgnoresDoNotHoldTheDrain(t *testing.T) {
	headless := slice("haproxy-headless-1", "haproxy-headless", endpoint{uid: ownUID, node: ownNode, ready: true})
	headless.Labels[corev1.IsHeadlessService] = ""
	f := start(t, append(terminatingWithAReadyPeer(), service("haproxy-headless"), headless)...)
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	f.proxies.set("192.0.2.2", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	assert.Equal(t, Routing{Detail: "kube-proxy synced on all 2 nodes"}, f.routing(t, newPoll()))
}

func TestEveryServiceThatListsThePodMustWithdrawIt(t *testing.T) {
	f := start(t, node(ownNode, "192.0.2.1"), service("haproxy"), service("gateway-a"),
		slice("haproxy-1", "haproxy", endpoint{uid: ownUID, node: ownNode}, endpoint{uid: "peer", node: "node-b", ready: true}),
		slice("gateway-a-1", "gateway-a", endpoint{uid: ownUID, node: ownNode, ready: true}))
	routing := f.routing(t, newPoll())
	assert.True(t, routing.Routed)
	assert.Contains(t, routing.Detail, "Service gateway-a")
}

func TestAKubeProxyThatNeverAnsweredIsUnverifiedAndOneThatStopsAnsweringIsPending(t *testing.T) {
	f := start(t, terminatingWithAReadyPeer()...)
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: f.changedAt().Add(-time.Second), families: ipv4})
	poll := newPoll()

	routing := f.routing(t, poll)
	assert.True(t, routing.Routed, "node-a answered and has not synced")

	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	routing = f.routing(t, poll)
	assert.False(t, routing.Routed)
	assert.Contains(t, routing.Unverified, "node-b (no kube-proxy healthz: connection refused)")

	f = start(t, terminatingWithAReadyPeer()...)
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: f.changedAt().Add(-time.Second), families: ipv4})
	f.proxies.set("192.0.2.2", &fakeKubeProxy{syncedAt: f.changedAt().Add(-time.Second), families: ipv4})
	poll = newPoll()
	require.True(t, f.routing(t, poll).Routed)
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	f.proxies.set("192.0.2.2", &fakeKubeProxy{err: errors.New("i/o timeout")})
	routing = f.routing(t, poll)
	assert.True(t, routing.Routed)
	assert.Contains(t, routing.Detail, "node-b (i/o timeout)")
}

func TestAKubeProxyThatAcceptsButDoesNotAnswerIsPending(t *testing.T) {
	f := start(t, terminatingWithAReadyPeer()...)
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	f.proxies.set("192.0.2.2", &fakeKubeProxy{err: context.DeadlineExceeded})
	routing := f.routing(t, newPoll())
	assert.True(t, routing.Routed, "a stopped kube-proxy still holds the old rules")
	assert.Contains(t, routing.Detail, "node-b (context deadline exceeded)")
}

func TestAKubeProxyWithoutPerFamilySyncTimesIsUnverified(t *testing.T) {
	f := start(t, terminatingWithAReadyPeer()...)
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	f.proxies.set("192.0.2.2", &fakeKubeProxy{syncedAt: time.Now()})
	routing := f.routing(t, newPoll())
	assert.False(t, routing.Routed)
	assert.Contains(t, routing.Unverified, "node-b (healthz reports no IPv4 sync time")
}

func TestAPodNoSliceListsIsUnverified(t *testing.T) {
	f := start(t, node(ownNode, "192.0.2.1"), service("haproxy"),
		slice("haproxy-1", "haproxy", endpoint{uid: "peer", node: "node-b", ready: true}))
	routing := f.routing(t, newPoll())
	assert.False(t, routing.Routed)
	assert.Contains(t, routing.Unverified, "no EndpointSlice in haptic lists this pod")
}

func TestWatchReportsUntilTheDrainStops(t *testing.T) {
	f := start(t, terminatingWithAReadyPeer()...)
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: f.changedAt().Add(-time.Second), families: ipv4})
	f.proxies.set("192.0.2.2", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	f.observer.cfg.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	reports := make(chan Routing, 100)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.observer.Watch(ctx, func(r Routing) {
			select {
			case reports <- r:
			default:
			}
		})
	}()
	assert.True(t, (<-reports).Routed)
	f.proxies.set("192.0.2.1", &fakeKubeProxy{syncedAt: time.Now(), families: ipv4})
	require.Eventually(t, func() bool {
		select {
		case r := <-reports:
			return !r.Routed
		default:
			return false
		}
	}, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done
}

func TestNewRejectsAnIncompleteIdentity(t *testing.T) {
	_, err := New(&Config{Client: fake.NewClientset(), Namespace: namespace, NodeName: ownNode, Healthz: (&kubeProxies{}).read})
	require.Error(t, err)
}
