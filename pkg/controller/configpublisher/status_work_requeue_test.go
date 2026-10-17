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

package configpublisher

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
	configpublisher "github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
)

// holdAuxiliaryReferences refuses a publication's status.auxiliaryFiles patch
// until release is called, leaving the HAProxyCfg written without them.
func holdAuxiliaryReferences(c *fake.Clientset) (release func()) {
	var held atomic.Bool
	held.Store(true)
	c.PrependReactor("patch", "haproxycfgs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch := action.(k8stesting.PatchAction)
		if !held.Load() || patch.GetPatchType() != types.JSONPatchType ||
			!bytes.Contains(patch.GetPatch(), []byte(`"/status/auxiliaryFiles"`)) {
			return false, nil, nil
		}
		return true, nil, apierrors.NewServiceUnavailable("auxiliary references held")
	})
	return func() { held.Store(false) }
}

func newStatusWorkComponent(t *testing.T) (*fake.Clientset, *Component) {
	t.Helper()
	crdClient := fake.NewSimpleClientset()
	installSSAListMapMergeReactor(crdClient)
	publisher := configpublisher.NewWithListers(k8sfake.NewClientset(), crdClient, nil, testutil.NewTestLogger())
	c := New(publisher, testutil.NewTestBus(), testutil.NewTestLogger(), WithPublishInterval(0))
	t.Cleanup(c.publishThrottle.Stop)
	t.Cleanup(c.statusThrottle.Stop)
	return crdClient, c
}

func podStatusEvent(pod, uid, checksum string) *events.ConfigAppliedToPodEvent {
	return &events.ConfigAppliedToPodEvent{
		RuntimeConfigName: "test-config-haproxycfg", RuntimeConfigNamespace: "default",
		PodName: pod, PodNamespace: "default", PodUID: uid, Checksum: checksum,
	}
}

func mapFilePublishRequest(content, checksum string) *configpublisher.PublishRequest {
	return &configpublisher.PublishRequest{
		TemplateConfigName: "test-config", TemplateConfigNamespace: "default", TemplateConfigUID: "test-uid",
		Config: "global\n", ConfigPath: "/etc/haproxy/haproxy.cfg", Checksum: checksum,
		AuxiliaryFiles: &configpublisher.AuxiliaryFiles{
			MapFiles: []auxiliaryfiles.MapFile{{Path: "/etc/haproxy/maps/host.map", Content: content}},
		},
	}
}

func publishedMapFilePods(t *testing.T, ctx context.Context, c *fake.Clientset) (name string, pods []string) {
	t.Helper()
	cfg, err := c.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, cfg.Status.AuxiliaryFiles.MapFiles, 1)
	name = cfg.Status.AuxiliaryFiles.MapFiles[0].Name
	mapFile, err := c.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	for i := range mapFile.Status.DeployedToPods {
		pods = append(pods, mapFile.Status.DeployedToPods[i].PodName)
	}
	return name, pods
}

func (c *Component) statusWorkFor(key string) (pending, parked *statusWorkItem) {
	c.statusWorkMu.Lock()
	defer c.statusWorkMu.Unlock()
	return c.statusWorkPending[key], c.statusWorkParked[key]
}

func (c *Component) statusWorkerSignalled() bool {
	select {
	case <-c.statusWorkTrigger:
		return true
	default:
		return false
	}
}

func alwaysCurrent() bool { return true }

// TestStatusUpdateBeforeItsPublicationStampsTheNewSet is issue #384: a pod's
// update that runs before its config's publication starts stamps no file of
// the previous set, and stamps the new set when that publication commits
// instead of at the next drift round.
func TestStatusUpdateBeforeItsPublicationStampsTheNewSet(t *testing.T) {
	ctx := t.Context()
	crdClient, c := newStatusWorkComponent(t)
	_, err := c.attemptPublication(ctx, mapFilePublishRequest("example.com be_a\n", "previous"), alwaysCurrent)
	require.NoError(t, err)
	previousMap, _ := publishedMapFilePods(t, ctx, crdClient)

	c.processStatusWork(ctx, &statusWorkItem{event: podStatusEvent("haproxy-0", "uid-0", "next")})
	_, previousPods := publishedMapFilePods(t, ctx, crdClient)
	assert.NotContains(t, previousPods, "haproxy-0", "haproxy-0 never ran the previous map file")

	_, err = c.attemptPublication(ctx, mapFilePublishRequest("example.com be_b\n", "next"), alwaysCurrent)
	require.NoError(t, err)
	assert.True(t, c.statusWorkerSignalled(), "the publication must wake the status worker")
	c.processAllPendingStatusWork(ctx)

	nextMap, nextPods := publishedMapFilePods(t, ctx, crdClient)
	require.NotEqual(t, previousMap, nextMap)
	assert.Contains(t, nextPods, "haproxy-0", "haproxy-0 must reach the new map file without a drift round")
}

// TestProcessStatusWork_ParksUntilRuntimeConfigPublished: the first deployment
// can complete before the HAProxyCfg exists, and a publication writes the
// HAProxyCfg before its auxiliary references. Either way the update waits for
// the next publication attempt instead of being lost until a drift check.
func TestProcessStatusWork_ParksUntilRuntimeConfigPublished(t *testing.T) {
	ctx := t.Context()
	crdClient, c := newStatusWorkComponent(t)
	event := podStatusEvent("haproxy-1", "", "abc123")
	key := statusWorkKey(event)

	c.processStatusWork(ctx, &statusWorkItem{event: event})
	pending, parked := c.statusWorkFor(key)
	assert.Nil(t, pending)
	require.NotNil(t, parked, "the update must wait while the HAProxyCfg is missing")
	assert.False(t, c.statusWorkerSignalled(), "nothing retries it before a publication")

	release := holdAuxiliaryReferences(crdClient)
	request := mapFilePublishRequest("example.com be_a\n", "abc123")
	_, err := c.attemptPublication(ctx, request, alwaysCurrent)
	require.Error(t, err)
	require.True(t, c.statusWorkerSignalled(), "every publication attempt retries parked updates")
	c.processAllPendingStatusWork(ctx)
	_, parked = c.statusWorkFor(key)
	require.NotNil(t, parked, "the update must wait while the HAProxyCfg has no auxiliary references")

	release()
	_, err = c.attemptPublication(ctx, request, alwaysCurrent)
	require.NoError(t, err)
	require.True(t, c.statusWorkerSignalled())
	c.processAllPendingStatusWork(ctx)

	cfg, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, cfg.Status.DeployedToPods, 1)
	assert.Equal(t, "haproxy-1", cfg.Status.DeployedToPods[0].PodName)
	assert.Equal(t, "abc123", cfg.Status.DeployedToPods[0].Checksum)
	_, mapPods := publishedMapFilePods(t, ctx, crdClient)
	assert.Equal(t, []string{"haproxy-1"}, mapPods)
	pending, parked = c.statusWorkFor(key)
	assert.Nil(t, pending)
	assert.Nil(t, parked)
}

// TestParkStatusWork_PublicationDuringTheAttemptRetriesAtOnce: a publication
// attempted after the update read the HAProxyCfg finds nothing parked, so the
// update must not wait for the one after it.
func TestParkStatusWork_PublicationDuringTheAttemptRetriesAtOnce(t *testing.T) {
	_, c := newStatusWorkComponent(t)
	work := &statusWorkItem{event: podStatusEvent("haproxy-1", "", "")}

	c.wakeParkedStatusWork()
	c.parkStatusWork(work, 0)

	pending, parked := c.statusWorkFor(statusWorkKey(work.event))
	assert.Same(t, work, pending)
	assert.Nil(t, parked)
	assert.True(t, c.statusWorkerSignalled())
}

// TestBlockedPublicationLeavesStatusWorkParked: an attempt cancelled before it
// reaches the API cannot publish the checksum a parked update waits for.
func TestBlockedPublicationLeavesStatusWorkParked(t *testing.T) {
	_, c := newStatusWorkComponent(t)
	work := &statusWorkItem{event: podStatusEvent("haproxy-1", "", "b")}
	c.parkStatusWork(work, 0)
	c.publicationBlocked = true

	_, _ = c.attemptPublication(context.Background(), mapFilePublishRequest("x", "b"), alwaysCurrent)

	pending, parked := c.statusWorkFor(statusWorkKey(work.event))
	assert.Nil(t, pending)
	assert.Same(t, work, parked)
	assert.False(t, c.statusWorkerSignalled())
}

// TestParkedStatusWork_NewerUpdateSupersedes: a publication must never apply a
// parked update over a newer one for the same pod.
func TestParkedStatusWork_NewerUpdateSupersedes(t *testing.T) {
	_, c := newStatusWorkComponent(t)
	older := &statusWorkItem{event: podStatusEvent("haproxy-1", "", "older")}
	key := statusWorkKey(older.event)

	c.parkStatusWork(older, 0)
	c.handleConfigAppliedToPod(podStatusEvent("haproxy-1", "", "newer"))
	c.wakeParkedStatusWork()
	pending, parked := c.statusWorkFor(key)
	require.NotNil(t, pending)
	assert.Equal(t, "newer", pending.event.Checksum)
	assert.Nil(t, parked)

	c.parkStatusWork(older, 1)
	pending, parked = c.statusWorkFor(key)
	assert.Equal(t, "newer", pending.event.Checksum, "an update finishing after a newer one arrived is dropped")
	assert.Nil(t, parked)
}

// TestPodTerminationDropsItsParkedStatusWork: a parked update must not outlive
// its pod incarnation, or the next publication re-applies what cleanup removed.
func TestPodTerminationDropsItsParkedStatusWork(t *testing.T) {
	_, c := newStatusWorkComponent(t)
	park := func(pod, uid string) string {
		work := &statusWorkItem{event: podStatusEvent(pod, uid, "")}
		c.parkStatusWork(work, 0)
		return statusWorkKey(work.event)
	}
	terminated, replacement, other := park("haproxy-0", "uid-old"), park("haproxy-1", "uid-new"), park("haproxy-2", "uid-2")

	c.handlePodTerminated(t.Context(), &events.HAProxyPodTerminatedEvent{PodName: "haproxy-0", PodNamespace: "default", PodUID: "uid-old"})
	c.handlePodTerminated(t.Context(), &events.HAProxyPodTerminatedEvent{PodName: "haproxy-1", PodNamespace: "default", PodUID: "uid-old"})

	_, parked := c.statusWorkFor(terminated)
	assert.Nil(t, parked, "the terminated incarnation's update is dropped")
	_, parked = c.statusWorkFor(replacement)
	assert.NotNil(t, parked, "another incarnation's update is kept")
	_, parked = c.statusWorkFor(other)
	assert.NotNil(t, parked)
}
