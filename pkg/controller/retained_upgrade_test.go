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

package controller

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
)

// releasedIntentAnnotation is where 0.5 and 0.6 kept the deployment intent: on
// the leader Lease.
const releasedIntentAnnotation = "haproxy-haptic.org/deployment-intent"

// moveIntentToLeaderLease leaves the deployment intent where 0.5 and 0.6 kept it.
func (h *leaderTermHarness) moveIntentToLeaderLease(t *testing.T) {
	t.Helper()
	leases := h.kube.CoordinationV1().Leases(drainNamespace)
	retainedState := h.template.Name + "-retained-state"
	retained, err := leases.Get(t.Context(), retainedState, metav1.GetOptions{})
	require.NoError(t, err)
	var intent map[string]any
	require.NoError(t, json.Unmarshal([]byte(retained.Annotations[releasedIntentAnnotation]), &intent))
	delete(intent, "holder")
	released, err := json.Marshal(intent)
	require.NoError(t, err)
	require.NoError(t, leases.Delete(t.Context(), retainedState, metav1.DeleteOptions{}))

	leader, err := leases.Get(t.Context(), drainLeaseName, metav1.GetOptions{})
	require.NoError(t, err)
	leader.Annotations[releasedIntentAnnotation] = string(released)
	_, err = leases.Update(t.Context(), leader, metav1.UpdateOptions{})
	require.NoError(t, err)
}

// The first leader after an upgrade from 0.6 must keep the intent the previous
// leader recorded on the leader Lease: recovery still picks the newest
// checkpoint, and the leader Lease is left to the elector alone.
func TestUpgradedLeaderKeepsTheDeploymentIntentOfAnEarlierRelease(t *testing.T) {
	h := newLeaderTermHarness(t)
	h.deploy(t, "global\n  maxconn 100\n")
	h.waitRetained(t, h.deployed[0])
	h.deploy(t, "global\n  maxconn 200\n")
	h.state.drain(testutil.NewTestLogger())
	h.state.stop(testutil.NewTestLogger())
	h.moveIntentToLeaderLease(t)

	checksum, err := h.recoverable(t, h.takeOver(t))

	require.NoError(t, err, "the upgraded leader must find the checkpoint the released intent names")
	require.Equal(t, h.deployed[1], checksum)
	leader, err := h.kube.CoordinationV1().Leases(drainNamespace).Get(t.Context(), drainLeaseName, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotContains(t, leader.Annotations, releasedIntentAnnotation)
}
