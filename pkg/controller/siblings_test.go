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
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const siblingTestNamespace = "haptic"

// controllerSelector is what the chart passes in CONTROLLER_POD_SELECTOR.
const controllerSelector = "app.kubernetes.io/component=controller,app.kubernetes.io/instance=haptic"

var controllerPodLabels = map[string]string{
	"app.kubernetes.io/instance":  "haptic",
	"app.kubernetes.io/component": "controller",
}

// releaseLabels are the per-release labels older charts put on a pod.
func releaseLabels(version string) map[string]string {
	return map[string]string{
		"helm.sh/chart":             "haptic-" + version,
		"app.kubernetes.io/version": version,
	}
}

type healthzServe func(status int, body string) int32

// healthzServer answers /healthz with the given status and body.
func healthzServer(t *testing.T, status int, body string) int32 {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	parsed, err := strconv.Atoi(port)
	require.NoError(t, err)
	return int32(parsed)
}

func controllerPod(name, hash string, ready bool, port int32) *corev1.Pod {
	podLabels := releaseLabels("0.2.1")
	podLabels["pod-template-hash"] = hash
	for k, v := range controllerPodLabels {
		podLabels[k] = v
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: siblingTestNamespace, Labels: podLabels},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "controller",
			Ports: []corev1.ContainerPort{{Name: siblingHealthPortName, ContainerPort: port}},
		}}},
		Status: corev1.PodStatus{
			PodIP:      "127.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func newTestSiblingProbes(t *testing.T, pods ...runtime.Object) *siblingProbes {
	t.Helper()
	self := controllerPod("self", "a", true, 1)
	probes, err := newSiblingProbes(context.Background(), fake.NewClientset(append(pods, self)...),
		siblingTestNamespace, "self", controllerSelector, slog.Default())
	require.NoError(t, err)
	return probes
}

func newTestSiblings(t *testing.T, pods ...runtime.Object) *siblingProbe {
	t.Helper()
	return newTestSiblingProbes(t, pods...).converged
}

const (
	convergedBody = `{"status":"ok","components":{"initialized":{"healthy":true}}}`
	graceBody     = `{"status":"ok","components":{"initialized":{"healthy":true,"error":"reinitializing (grace period): controller still initializing"}}}`
	failingBody   = `{"status":"degraded","components":{"initialized":{"healthy":false,"error":"controller still initializing"}}}`
)

func TestSiblingConvergence_Check(t *testing.T) {
	tests := []struct {
		name     string
		pods     func(serve healthzServe) []runtime.Object
		wantNone bool
		wantErr  bool
	}{
		{
			name:     "no sibling",
			pods:     func(healthzServe) []runtime.Object { return nil },
			wantNone: true,
		},
		{
			name: "converged sibling from another ReplicaSet",
			pods: func(serve healthzServe) []runtime.Object {
				return []runtime.Object{controllerPod("new", "b", true, serve(http.StatusOK, convergedBody))}
			},
			wantNone: false,
		},
		{
			name: "sibling within its grace window",
			pods: func(serve healthzServe) []runtime.Object {
				return []runtime.Object{controllerPod("peer", "a", true, serve(http.StatusOK, graceBody))}
			},
			wantNone: true,
		},
		{
			name: "sibling failing its own reload",
			pods: func(serve healthzServe) []runtime.Object {
				return []runtime.Object{controllerPod("peer", "a", true, serve(http.StatusServiceUnavailable, failingBody))}
			},
			wantNone: true,
		},
		{
			name: "not-ready sibling is not asked",
			pods: func(serve healthzServe) []runtime.Object {
				return []runtime.Object{controllerPod("peer", "a", false, serve(http.StatusOK, convergedBody))}
			},
			wantNone: true,
		},
		{
			name: "pods of another component are not siblings",
			pods: func(serve healthzServe) []runtime.Object {
				haproxy := controllerPod("haproxy", "c", true, serve(http.StatusOK, convergedBody))
				haproxy.Labels["app.kubernetes.io/component"] = "loadbalancer"
				return []runtime.Object{haproxy}
			},
			wantNone: true,
		},
		{
			name: "unreachable ready sibling cannot be ruled out",
			pods: func(healthzServe) []runtime.Object {
				server := httptest.NewServer(http.NotFoundHandler())
				_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
				server.Close()
				parsed, _ := strconv.Atoi(port)
				return []runtime.Object{controllerPod("peer", "a", true, int32(parsed))}
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			siblings := newTestSiblings(t, tt.pods(func(status int, body string) int32 {
				return healthzServer(t, status, body)
			})...)
			none, err := siblings.check(context.Background())
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantNone, none)
		})
	}
}

// The probe never waits for the API server or a sibling: it reads the last
// result and schedules the next check, and anything but a fresh, successful
// result reads as "a sibling may be converged".
func TestSiblingConvergence_NoneConvergedIsCachedAndFailsSafe(t *testing.T) {
	siblings := newTestSiblings(t, controllerPod("peer", "a", true, healthzServer(t, http.StatusOK, graceBody)))
	now := time.Unix(1_000_000, 0)
	siblings.now = func() time.Time { return now }

	assert.False(t, siblings.NoneMatch(), "no result yet")
	require.Eventually(t, siblings.NoneMatch, 5*time.Second, 10*time.Millisecond)

	now = now.Add(2 * siblingRefreshInterval)
	siblings.mu.Lock()
	siblings.refreshing = true
	siblings.mu.Unlock()
	assert.False(t, siblings.NoneMatch(), "an outdated result cannot rule out a converged sibling")
}

// After a chart upgrade the old leader must still see the new ReplicaSet,
// whose per-release labels differ from its own.
func TestSiblingConvergence_FindsUpgradedReplicas(t *testing.T) {
	upgraded := controllerPod("new", "b", true, healthzServer(t, http.StatusOK, convergedBody))
	for k, v := range releaseLabels("0.3.0") {
		upgraded.Labels[k] = v
	}
	none, err := newTestSiblings(t, upgraded).check(context.Background())
	require.NoError(t, err)
	assert.False(t, none, "a converged pod of the upgraded release is a sibling")
}

func TestNewSiblingConvergence_RejectsUnusableSelectors(t *testing.T) {
	for _, selector := range []string{"", "app.kubernetes.io/instance in (", "!!"} {
		_, err := newSiblingProbes(context.Background(), fake.NewClientset(), siblingTestNamespace, "self", selector, slog.Default())
		assert.Error(t, err, "selector %q", selector)
	}
}

func TestSiblingRenderGraphProbe(t *testing.T) {
	const (
		publishedBody = `{"status":"ok","components":{"admission":{"healthy":true},"render-graph":{"healthy":true}}}`
		aloneBody     = `{"status":"ok","components":{"render-graph":{"healthy":true,"error":"first full render still running"}}}`
		olderBody     = `{"status":"ok","components":{"admission":{"healthy":true}}}`
		waitingBody   = `{"status":"degraded","components":{"render-graph":{"healthy":false,"error":"first full render still running"}}}`
	)
	tests := []struct {
		name     string
		status   int
		body     string
		wantNone bool
	}{
		{name: "sibling with a published graph", status: http.StatusOK, body: publishedBody, wantNone: false},
		{name: "sibling answering alone while its first render runs", status: http.StatusOK, body: aloneBody, wantNone: true},
		{name: "sibling of a release without the entry", status: http.StatusOK, body: olderBody, wantNone: false},
		{name: "sibling waiting for its first render", status: http.StatusServiceUnavailable, body: waitingBody, wantNone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probes := newTestSiblingProbes(t, controllerPod("peer", "a", true, healthzServer(t, tt.status, tt.body)))
			none, err := probes.renderGraph.check(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.wantNone, none)
		})
	}
}
