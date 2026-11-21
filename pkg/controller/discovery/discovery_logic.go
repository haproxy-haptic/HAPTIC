// Copyright 2025 Philipp Hossner
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
	"context"
	"sync"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	agentclient "github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/client"
)

const (
	// probeTimeout bounds one pod's /v1/state answer during admission. The
	// agent serves it from memory once its startup init is done, so a pod that
	// needs longer is not ready to be applied to either.
	probeTimeout = 5 * time.Second

	// maxConcurrentProbes bounds a discovery pass's fan-out, matching the
	// deployer's per-deployment cap.
	maxConcurrentProbes = 16
)

// triggerDiscovery discovers the candidate pods, admits the reachable ones and
// publishes the result.
//
// Admission is one rule: the pod has an IP, its agent container is running, and
// GET /v1/state answers. Pod Ready is deliberately not part of it — HAProxy's
// readiness probe only turns 200 after the first apply lands, so requiring it
// would never admit a fresh pod.
func (c *Component) triggerDiscovery(source string) {
	c.discoveryMu.Lock()
	defer c.discoveryMu.Unlock()

	c.mu.RLock()
	ctx := c.lifecycleCtx
	discovery := c.discovery
	podStore := c.podStore
	hasCredentials := c.hasCredentials
	hasDataplanePort := c.hasDataplanePort
	credentialsValue := c.credentials
	c.mu.RUnlock()

	if ctx == nil || ctx.Err() != nil || discovery == nil || podStore == nil || !hasCredentials || !hasDataplanePort || credentialsValue == nil {
		return
	}
	credentials := *credentialsValue
	c.Logger().Debug("Triggering HAProxy pod discovery", "source", source)

	candidates, err := discovery.DiscoverEndpointsWithLogger(podStore, credentials, c.Logger())
	if err != nil {
		c.Logger().Error("Discovery failed", "error", err)
		return
	}

	c.Logger().Debug("Discovered candidate pods", "count", len(candidates))

	current := make(map[endpointIdentity]struct{}, len(candidates))
	for i := range candidates {
		current[endpointIdentityOf(&candidates[i].Endpoint)] = struct{}{}
	}
	c.cleanupRemovedPods(current)

	admitted, rejections := c.admitReachable(ctx, candidates)
	if ctx.Err() != nil {
		return
	}
	c.publishDiscoveryResult(source, len(candidates), admitted, rejections)
}

type terminatedEndpoint struct {
	podName      string
	podNamespace string
	podUID       string
}

func (c *Component) publishDiscoveryResult(source string, candidateCount int, admittedEndpoints []*dataplane.Endpoint, rejections []rejection) {
	for _, r := range rejections {
		c.EventBus().Publish(events.NewHAProxyPodRejectedEvent(r.podName, r.reason))
	}

	currentEndpoints := make(map[podIdentity]endpointAuthority, len(admittedEndpoints))
	for _, ep := range admittedEndpoints {
		currentEndpoints[podIdentity{podNamespace: ep.PodNamespace, podName: ep.PodName}] = endpointAuthorityOf(ep)
	}

	terminated := make([]terminatedEndpoint, 0)
	c.mu.Lock()
	previousCount := len(c.lastEndpoints)
	for identity := range c.lastEndpoints {
		previous := c.lastEndpoints[identity]
		current, exists := currentEndpoints[identity]
		if !exists || current != previous {
			terminated = append(terminated, terminatedEndpoint{
				podName:      previous.identity.podName,
				podNamespace: previous.identity.podNamespace,
				podUID:       previous.identity.podUID,
			})
		}
	}
	c.lastEndpoints = currentEndpoints
	c.mu.Unlock()

	// Info only when the result is worth surfacing: an empty fleet, or a count
	// that moved. A steady fleet re-reports the same numbers every drift-
	// prevention tick, and at Info those drown every other line the leader logs.
	log := c.Logger().Debug
	if len(admittedEndpoints) == 0 || len(admittedEndpoints) != previousCount {
		log = c.Logger().Info
	}
	log("Discovered HAProxy pods",
		"source", source,
		"candidates", candidateCount,
		"admitted", len(admittedEndpoints))

	for _, endpoint := range terminated {
		c.Logger().Info("Detected pod termination",
			"pod_name", endpoint.podName,
			"pod_namespace", endpoint.podNamespace)
		c.EventBus().Publish(events.NewHAProxyPodTerminatedEvent(endpoint.podName, endpoint.podNamespace, endpoint.podUID))
	}

	endpointValues := make([]dataplane.Endpoint, len(admittedEndpoints))
	for i, ep := range admittedEndpoints {
		endpointValues[i] = *ep
	}

	event := events.NewHAProxyPodsDiscoveredEvent(endpointValues, len(admittedEndpoints))
	c.discoveredReplayer.Cache(event)
	c.EventBus().Publish(event)
}

// rejection captures a pod refused admission, accumulated under the state
// mutex and published as HAProxyPodRejectedEvent after the lock is released
// (avoids fanning out events while holding the lock).
type rejection struct {
	podName string
	reason  string
}

// admitReachable admits the candidates whose agent answers, reusing the answer
// for an identity it already admitted. The identity carries the pod's runtime
// fingerprint, so a container restart or a new address re-probes by itself —
// which keeps this to one round trip per pod per identity, not per pass.
//
// The probes run concurrently: one pod whose agent hangs would otherwise delay
// every pod behind it, and a discovery pass is what retires a dead endpoint.
func (c *Component) admitReachable(ctx context.Context, candidates []Candidate) ([]*dataplane.Endpoint, []rejection) {
	reasons := make([]string, len(candidates))
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxConcurrentProbes)
	for i := range candidates {
		if candidates[i].Reason != "" {
			reasons[i] = candidates[i].Reason
			continue
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			reasons[index] = c.admitOne(ctx, &candidates[index].Endpoint)
		}(i)
	}
	wg.Wait()

	admitted := make([]*dataplane.Endpoint, 0, len(candidates))
	var rejections []rejection
	for i := range candidates {
		if reasons[i] == "" {
			admitted = append(admitted, &candidates[i].Endpoint)
			continue
		}
		rejections = append(rejections, rejection{podName: candidates[i].Endpoint.PodName, reason: reasons[i]})
	}
	return admitted, rejections
}

// admitOne decides one pod and stamps the HAProxy version it reported onto its
// endpoint. It returns the rejection reason, or "" when the pod is admitted.
func (c *Component) admitOne(ctx context.Context, endpoint *dataplane.Endpoint) string {
	if ctx.Err() != nil {
		return RejectionAgentUnreachable
	}
	identity := endpointIdentityOf(endpoint)
	if report, known := c.admittedReport(&identity); known {
		applyAgentReport(endpoint, report)
		return ""
	}

	state, err := c.probeAgent(ctx, endpoint)
	if err != nil {
		c.Logger().Warn("Agent did not answer, not admitting the pod",
			"pod", endpoint.PodName, "endpoint", endpoint.URL, "error", err)
		return RejectionAgentUnreachable
	}

	report := reportOf(state)
	c.recordAdmission(&identity, report)
	applyAgentReport(endpoint, report)
	c.Logger().Info("Pod admitted",
		"pod", endpoint.PodName,
		"haproxy_version", state.HAProxy.Version,
		"agent_version", state.AgentVersion)
	return ""
}

// probeAgent is the reachability half of the admission rule.
func (c *Component) probeAgent(ctx context.Context, endpoint *dataplane.Endpoint) (*api.State, error) {
	client, err := agentclient.New(&agentclient.Config{
		BaseURL:  endpoint.URL,
		Username: endpoint.Username,
		Password: endpoint.Password,
		Timeout:  probeTimeout,
		TLS:      c.tls,
	})
	if err != nil {
		return nil, err
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return client.State(ctx, api.StateRead{})
}

func (c *Component) admittedReport(identity *endpointIdentity) (agentReport, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	report, known := c.admitted[*identity]
	return report, known
}

func (c *Component) recordAdmission(identity *endpointIdentity, report agentReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.admitted[*identity] = report
}

// cleanupRemovedPods drops admissions for identities that are no longer
// candidates, which is also how a restarted container gets re-probed.
func (c *Component) cleanupRemovedPods(currentCandidates map[endpointIdentity]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for identity := range c.admitted {
		if _, exists := currentCandidates[identity]; !exists {
			c.Logger().Debug("Cleaning up state for removed pod", "pod", identity.podName)
			delete(c.admitted, identity)
		}
	}
}
