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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/haproxy-haptic/HAPTIC/pkg/introspection"
	"github.com/haproxy-haptic/HAPTIC/pkg/webhook"
)

const (
	siblingRefreshInterval = 5 * time.Second
	siblingRequestTimeout  = 2 * time.Second
	siblingHealthPortName  = "healthz"
	maxSiblingHealthBody   = 1 << 20
	// controllerPodSelectorEnv carries the controller Deployment's
	// spec.selector, which stays the same across chart upgrades.
	controllerPodSelectorEnv = "CONTROLLER_POD_SELECTOR"
)

// siblingProbe answers whether another Ready controller replica's health
// endpoint matches a condition, from a result refreshed in the background so a
// probe never waits on the API server or the network. A result that is
// missing, failed or outdated reads as "a sibling may match".
type siblingProbe struct {
	ctx       context.Context
	clientset kubernetes.Interface
	namespace string
	podName   string
	selector  labels.Selector
	client    *http.Client
	now       func() time.Time
	logger    *slog.Logger
	path      string
	matches   siblingHealthMatcher

	mu         sync.Mutex
	refreshing bool
	checkedAt  time.Time
	noneMatch  bool
}

// siblingHealthMatcher judges one sibling's response to the probe's path.
type siblingHealthMatcher func(status int, components map[string]introspection.ComponentHealth) bool

// siblingProbes are the questions the probes ask about the other replicas.
type siblingProbes struct {
	admissionSlots chan struct{}
	// converged: /healthz is 200 without a grace entry (ADR-0028).
	converged *siblingProbe
	// renderGraph: /readyz reports a published render graph.
	renderGraph *siblingProbe
}

func newSiblingProbes(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace, podName, selector string,
	logger *slog.Logger,
) (*siblingProbes, error) {
	parsed, err := labels.Parse(selector)
	if err != nil {
		return nil, fmt.Errorf("parsing %s %q: %w", controllerPodSelectorEnv, selector, err)
	}
	if parsed.Empty() {
		return nil, fmt.Errorf("%s is empty and would match every pod in the namespace", controllerPodSelectorEnv)
	}
	probe := func(path string, matches siblingHealthMatcher) *siblingProbe {
		return &siblingProbe{
			ctx:       ctx,
			clientset: clientset,
			namespace: namespace,
			podName:   podName,
			selector:  parsed,
			client:    &http.Client{Timeout: siblingRequestTimeout},
			now:       time.Now,
			logger:    logger,
			path:      path,
			matches:   matches,
		}
	}
	return &siblingProbes{
		admissionSlots: make(chan struct{}, 4),
		converged:      probe("/healthz", siblingConverged),
		renderGraph:    probe(ReadinessPath, siblingRenderGraphPublished),
	}, nil
}

func siblingConverged(status int, components map[string]introspection.ComponentHealth) bool {
	if status != http.StatusOK {
		return false
	}
	for _, component := range components {
		if strings.HasPrefix(component.Error, reinitGracePrefix) {
			return false
		}
	}
	return true
}

// siblingRenderGraphPublished counts a Ready sibling without the entry, a
// release that predates it, as able to validate.
func siblingRenderGraphPublished(status int, components map[string]introspection.ComponentHealth) bool {
	if status != http.StatusOK {
		return false
	}
	entry, ok := components[healthKeyRenderGraph]
	return !ok || (entry.Healthy && entry.Error == "")
}

// NoneMatch reports whether the last check found no matching sibling, and
// starts a new check when that result is due.
func (s *siblingProbe) NoneMatch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	age := s.now().Sub(s.checkedAt)
	if !s.refreshing && age >= siblingRefreshInterval {
		s.refreshing = true
		go s.refresh()
	}
	return s.noneMatch && age < 2*siblingRefreshInterval
}

func (s *siblingProbe) refresh() {
	none, err := s.check(s.ctx)
	if err != nil {
		s.logger.Debug("Could not rule out a matching controller replica", "path", s.path, "error", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshing = false
	s.checkedAt = s.now()
	s.noneMatch = err == nil && none
}

func (s *siblingProbe) check(ctx context.Context) (bool, error) {
	pods, err := s.clientset.CoreV1().Pods(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: s.selector.String()})
	if err != nil {
		return false, fmt.Errorf("listing controller pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Name == s.podName || pod.Status.PodIP == "" || !podReady(pod) {
			continue
		}
		matched, err := s.matchesPod(ctx, pod)
		if err != nil {
			return false, fmt.Errorf("pod %s: %w", pod.Name, err)
		}
		if matched {
			return false, nil
		}
	}
	return true, nil
}

func (s *siblingProbe) matchesPod(ctx context.Context, pod *corev1.Pod) (bool, error) {
	port, ok := namedContainerPort(pod, siblingHealthPortName)
	if !ok {
		return false, fmt.Errorf("no %q container port", siblingHealthPortName)
	}
	url := "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port))) + s.path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return false, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return s.matches(resp.StatusCode, nil), nil
	}
	var body struct {
		Components map[string]introspection.ComponentHealth `json:"components"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSiblingHealthBody)).Decode(&body); err != nil {
		return false, fmt.Errorf("decoding %s: %w", s.path, err)
	}
	return s.matches(resp.StatusCode, body.Components), nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func namedContainerPort(pod *corev1.Pod, name string) (int32, bool) {
	for i := range pod.Spec.Containers {
		for _, port := range pod.Spec.Containers[i].Ports {
			if port.Name == name {
				return port.ContainerPort, true
			}
		}
	}
	return 0, false
}

// admissionPeer uses the release selector and existing pod-list permission.
// Discovery runs only on failover, under the outgoing concurrency bound.
func (s *siblingProbe) admissionPeer(ctx context.Context) (string, error) {
	pods, err := s.clientset.CoreV1().Pods(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: s.selector.String()})
	if err != nil {
		return "", err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Name == s.podName || pod.DeletionTimestamp != nil || net.ParseIP(pod.Status.PodIP) == nil || !podReady(pod) {
			continue
		}
		if port, ok := namedContainerPort(pod, "webhook"); ok {
			return net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port))), nil
		}
	}
	return "", fmt.Errorf("no ready admission peer in namespace %s", s.namespace)
}

func (s *siblingProbes) admissionForwarder(server *webhook.Server, service string) func(context.Context, *admissionv1.AdmissionRequest) (*admissionv1.AdmissionResponse, error) {
	if s == nil || s.converged == nil || service == "" {
		return nil
	}
	discovery := s.converged
	dns := service + "." + discovery.namespace + ".svc"
	return func(ctx context.Context, req *admissionv1.AdmissionRequest) (*admissionv1.AdmissionResponse, error) {
		select {
		case s.admissionSlots <- struct{}{}:
			defer func() { <-s.admissionSlots }()
		default:
			return nil, errors.New("admission peer capacity exhausted")
		}
		address, err := discovery.admissionPeer(ctx)
		if err != nil {
			return nil, err
		}
		return server.ForwardAdmission(ctx, address, dns, req)
	}
}
