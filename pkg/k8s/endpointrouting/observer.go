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

// Package endpointrouting reports whether Kubernetes still routes new
// connections to one pod: the pod's EndpointSlice conditions, the Services'
// traffic policies, and the sync time of kube-proxy on every node.
// docs/adr/0037-drain-observes-routing.md explains what it can and cannot see.
package endpointrouting

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	defaultPollInterval = 200 * time.Millisecond
	healthzTimeout      = time.Second
	healthzParallelism  = 16
)

// Config identifies the pod and how to reach kube-proxy.
type Config struct {
	Client    kubernetes.Interface
	Namespace string
	PodUID    types.UID
	NodeName  string
	// Healthz reads kube-proxy's health on one node address.
	Healthz      HealthzReader
	PollInterval time.Duration
}

// Routing is the observer's verdict; see server.Routing for the fields.
type Routing struct {
	Routed     bool
	Unverified string
	Detail     string
}

// Observer watches the EndpointSlices and Services of the pod's namespace for
// the pod's whole life, so it knows when the last change that concerned the pod
// arrived when a drain starts.
type Observer struct {
	cfg      Config
	slices   discoverylisters.EndpointSliceLister
	services corelisters.ServiceLister
	synced   []cache.InformerSynced
	factory  informers.SharedInformerFactory

	mu sync.Mutex
	// handled is the resourceVersion of each EndpointSlice the event handler
	// has seen; changed is when the last event of each Service's slices
	// arrived. The lister can be ahead of both.
	handled  map[types.UID]string
	changed  map[string]time.Time
	watchErr error
}

// New builds the observer; Start runs its watches.
func New(cfg *Config) (*Observer, error) {
	if cfg.Client == nil || cfg.Namespace == "" || cfg.PodUID == "" || cfg.NodeName == "" || cfg.Healthz == nil {
		return nil, errors.New("endpoint routing needs a client, the pod's namespace, UID and node name, and a healthz reader")
	}
	o := &Observer{cfg: *cfg, handled: map[types.UID]string{}, changed: map[string]time.Time{}}
	if o.cfg.PollInterval <= 0 {
		o.cfg.PollInterval = defaultPollInterval
	}
	o.factory = informers.NewSharedInformerFactoryWithOptions(cfg.Client, 0, informers.WithNamespace(cfg.Namespace))
	sliceInformer := o.factory.Discovery().V1().EndpointSlices()
	serviceInformer := o.factory.Core().V1().Services()
	o.slices, o.services = sliceInformer.Lister(), serviceInformer.Lister()
	for _, informer := range []cache.SharedIndexInformer{sliceInformer.Informer(), serviceInformer.Informer()} {
		if err := informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) { o.setWatchErr(err) }); err != nil {
			return nil, err
		}
		o.synced = append(o.synced, informer.HasSynced)
	}
	if _, err := sliceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { o.sliceChanged(obj, false) },
		UpdateFunc: func(_, obj any) { o.sliceChanged(obj, false) },
		DeleteFunc: func(obj any) { o.sliceChanged(obj, true) },
	}); err != nil {
		return nil, err
	}
	return o, nil
}

// Start runs the watches until ctx ends.
func (o *Observer) Start(ctx context.Context) error {
	o.factory.Start(ctx.Done())
	<-ctx.Done()
	o.factory.Shutdown()
	return nil
}

func (o *Observer) setWatchErr(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.watchErr = err
}

// sliceChanged records when a change to a Service's slices reached this
// process. kube-proxy must have synced after it for the change to count.
func (o *Observer) sliceChanged(obj any, deleted bool) {
	now := time.Now()
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if deleted {
		delete(o.handled, slice.UID)
	} else {
		o.handled[slice.UID] = slice.ResourceVersion
	}
	o.changed[slice.Labels[discoveryv1.LabelServiceName]] = now
}

func (o *Observer) ownEndpoint(slice *discoveryv1.EndpointSlice) *discoveryv1.Endpoint {
	for i := range slice.Endpoints {
		if ref := slice.Endpoints[i].TargetRef; ref != nil && ref.UID == o.cfg.PodUID {
			return &slice.Endpoints[i]
		}
	}
	return nil
}

// Watch reports the routing to the pod on every poll until ctx ends.
func (o *Observer) Watch(ctx context.Context, report func(Routing)) {
	poll := &nodePoll{confirmed: map[string]time.Time{}, answered: map[string]bool{}}
	ticker := time.NewTicker(o.cfg.PollInterval)
	defer ticker.Stop()
	for {
		report(o.routing(ctx, poll))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// nodePoll is the kube-proxy state of one drain: the change each node was
// confirmed for, and which nodes ever answered.
type nodePoll struct {
	nodes     []corev1.Node
	confirmed map[string]time.Time
	answered  map[string]bool
}

func (o *Observer) routing(ctx context.Context, poll *nodePoll) Routing {
	for _, synced := range o.synced {
		if !synced() {
			o.mu.Lock()
			err := o.watchErr
			o.mu.Unlock()
			return Routing{Unverified: fmt.Sprintf("the EndpointSlice and Service watch in %s has not synced (last error: %v)", o.cfg.Namespace, err)}
		}
	}
	families, changed, routing, ok := o.sliceRouting()
	if !ok {
		return routing
	}
	if poll.nodes == nil {
		nodes, err := o.cfg.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return Routing{Unverified: fmt.Sprintf("listing the nodes whose kube-proxy may route here failed: %v", err)}
		}
		poll.nodes = nodes.Items
	}
	return o.kubeProxyRouting(ctx, poll, families, changed)
}

// sliceRouting applies kube-proxy's endpoint selection (KEP-1669) to every
// Service that lists the pod, and returns when the last change to those
// Services' slices arrived. ok is false when the slices alone decide.
func (o *Observer) sliceRouting() (families []discoveryv1.AddressType, changed time.Time, routing Routing, ok bool) {
	all, err := o.slices.List(labels.Everything())
	if err != nil {
		return nil, changed, Routing{Unverified: fmt.Sprintf("reading EndpointSlices: %v", err)}, false
	}
	// listed maps each Service and family that lists the pod to whether the
	// pod still serves there; only a serving pod is kube-proxy's fallback.
	listed := map[serviceFamily]bool{}
	for _, slice := range all {
		if slice.AddressType != discoveryv1.AddressTypeIPv4 && slice.AddressType != discoveryv1.AddressTypeIPv6 {
			continue
		}
		if own := o.ownEndpoint(slice); own != nil {
			key := serviceFamily{slice.Labels[discoveryv1.LabelServiceName], slice.AddressType}
			listed[key] = listed[key] || isServing(own)
			if isReady(own) {
				return nil, changed, Routing{Routed: true, Detail: fmt.Sprintf("Service %s still lists this pod as ready", key.service)}, false
			}
		}
	}
	if len(listed) == 0 {
		return nil, changed, Routing{Unverified: fmt.Sprintf("no EndpointSlice in %s lists this pod", o.cfg.Namespace)}, false
	}
	for key, serving := range listed {
		if detail := o.fallbackRouting(all, key.service, key.family); serving && detail != "" {
			return nil, changed, Routing{Routed: true, Detail: detail}, false
		}
		if !slices.Contains(families, key.family) {
			families = append(families, key.family)
		}
	}
	changed, current := o.lastChange(all, listed)
	if !current {
		return nil, changed, Routing{Routed: true, Detail: "an EndpointSlice change is still being processed"}, false
	}
	return families, changed, Routing{}, true
}

// lastChange returns when the last change to the listed Services' slices
// arrived; current is false while the lister holds a version the event
// handler has not timed yet.
func (o *Observer) lastChange(all []*discoveryv1.EndpointSlice, listed map[serviceFamily]bool) (changed time.Time, current bool) {
	services := map[string]bool{}
	for key := range listed {
		services[key.service] = true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, slice := range all {
		if services[slice.Labels[discoveryv1.LabelServiceName]] && o.handled[slice.UID] != slice.ResourceVersion {
			return changed, false
		}
	}
	for service := range services {
		if o.changed[service].After(changed) {
			changed = o.changed[service]
		}
	}
	return changed, true
}

// fallbackRouting says why kube-proxy still routes to the terminating pod: it
// does so for as long as no other endpoint is ready, per node for Local
// traffic policies.
func (o *Observer) fallbackRouting(all []*discoveryv1.EndpointSlice, service string, family discoveryv1.AddressType) string {
	anyReady, localReady := o.readyPeers(all, service, family)
	if !anyReady {
		return fmt.Sprintf("Service %s has no other ready %s endpoint, so kube-proxy keeps routing to this terminating pod", service, family)
	}
	svc, err := o.services.Services(o.cfg.Namespace).Get(service)
	if err != nil || localReady {
		return ""
	}
	externalLocal := svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal &&
		(svc.Spec.Type == corev1.ServiceTypeNodePort || svc.Spec.Type == corev1.ServiceTypeLoadBalancer)
	internalLocal := svc.Spec.InternalTrafficPolicy != nil && *svc.Spec.InternalTrafficPolicy == corev1.ServiceInternalTrafficPolicyLocal
	if externalLocal || internalLocal {
		return fmt.Sprintf("Service %s has a Local traffic policy and no other ready endpoint on node %s, so kube-proxy there keeps routing to this terminating pod", service, o.cfg.NodeName)
	}
	return ""
}

// readyPeers reports whether another endpoint of the Service and family is
// ready anywhere, and on this pod's node.
func (o *Observer) readyPeers(all []*discoveryv1.EndpointSlice, service string, family discoveryv1.AddressType) (anyReady, localReady bool) {
	for _, slice := range all {
		if slice.Labels[discoveryv1.LabelServiceName] != service || slice.AddressType != family {
			continue
		}
		for i := range slice.Endpoints {
			endpoint := &slice.Endpoints[i]
			if endpoint.TargetRef != nil && endpoint.TargetRef.UID == o.cfg.PodUID || !isReady(endpoint) {
				continue
			}
			anyReady = true
			localReady = localReady || endpoint.NodeName != nil && *endpoint.NodeName == o.cfg.NodeName
		}
	}
	return anyReady, localReady
}

type serviceFamily struct {
	service string
	family  discoveryv1.AddressType
}

func isReady(endpoint *discoveryv1.Endpoint) bool {
	return endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready
}

// isServing reads a missing serving condition as ready, as kube-proxy does.
func isServing(endpoint *discoveryv1.Endpoint) bool {
	if endpoint.Conditions.Serving == nil {
		return isReady(endpoint)
	}
	return *endpoint.Conditions.Serving
}

type nodeVerdict int

const (
	nodeSynced nodeVerdict = iota
	nodePending
	nodeUnverified
)

// kubeProxyRouting asks every node's kube-proxy whether it completed a sync
// after the pod's last change reached this process.
func (o *Observer) kubeProxyRouting(ctx context.Context, poll *nodePoll, families []discoveryv1.AddressType, changed time.Time) Routing {
	type result struct {
		node     string
		answered bool
		verdict  nodeVerdict
		why      string
	}
	results := make([]result, len(poll.nodes))
	var wg sync.WaitGroup
	limit := make(chan struct{}, healthzParallelism)
	for i := range poll.nodes {
		node := &poll.nodes[i]
		if confirmedFor, ok := poll.confirmed[node.Name]; ok && !confirmedFor.Before(changed) {
			continue
		}
		answeredBefore := poll.answered[node.Name]
		wg.Add(1)
		limit <- struct{}{}
		go func() {
			defer func() { <-limit; wg.Done() }()
			answered, verdict, why := o.nodeRouting(ctx, node, answeredBefore, families, changed)
			results[i] = result{node: node.Name, answered: answered, verdict: verdict, why: why}
		}()
	}
	wg.Wait()
	var pending, unverified []string
	for _, r := range results {
		if r.node == "" {
			continue
		}
		poll.answered[r.node] = poll.answered[r.node] || r.answered
		switch r.verdict {
		case nodeSynced:
			poll.confirmed[r.node] = changed
		case nodePending:
			pending = append(pending, r.node+" ("+r.why+")")
		case nodeUnverified:
			unverified = append(unverified, r.node+" ("+r.why+")")
		}
	}
	switch {
	case len(pending) > 0:
		return Routing{Routed: true, Detail: "kube-proxy has not synced since the last change to this pod's endpoints on " + strings.Join(pending, ", ")}
	case len(unverified) > 0:
		return Routing{Unverified: "kube-proxy sync state unknown on " + strings.Join(unverified, ", ")}
	default:
		return Routing{Detail: fmt.Sprintf("kube-proxy synced on all %d nodes", len(poll.nodes))}
	}
}

// nodeRouting reads one node's kube-proxy. A node without a kube-proxy healthz
// is unverified; one that is there but does not answer is pending.
func (o *Observer) nodeRouting(ctx context.Context, node *corev1.Node, answeredBefore bool, families []discoveryv1.AddressType, changed time.Time) (answered bool, verdict nodeVerdict, why string) {
	address := internalIP(node)
	if address == "" {
		return false, nodeUnverified, "no InternalIP"
	}
	callCtx, cancel := context.WithTimeout(ctx, healthzTimeout)
	defer cancel()
	sent := time.Now()
	health, err := o.cfg.Healthz(callCtx, address)
	received := time.Now()
	if err != nil {
		if errors.Is(err, ErrNoKubeProxy) && !answeredBefore {
			return false, nodeUnverified, err.Error()
		}
		return false, nodePending, err.Error()
	}
	// Compare ages, not instants: each age is measured on one clock, so node
	// clock skew cancels out (to within half the round trip).
	changeAge := sent.Add(received.Sub(sent) / 2).Sub(changed)
	for _, family := range families {
		synced, ok := health.Synced[family]
		if !ok {
			return true, nodeUnverified, "healthz reports no " + string(family) + " sync time; kube-proxy before v1.33 or a replacement"
		}
		if syncAge := health.CurrentTime.Sub(synced); syncAge >= changeAge {
			return true, nodePending, "last " + string(family) + " sync " + syncAge.Round(time.Millisecond).String() + " ago"
		}
	}
	return true, nodeSynced, ""
}

func internalIP(node *corev1.Node) string {
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			return address.Address
		}
	}
	return ""
}
