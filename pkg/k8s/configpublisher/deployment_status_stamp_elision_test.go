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
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

// ssaStatusPatchCounter counts Server-Side Apply patches on the /status
// subresource, keyed by resource plural (e.g. "haproxymapfiles"). This is the
// exact write issue #163 elides: each per-pod aux-file re-stamp is one throttled
// SSA status PATCH.
type ssaStatusPatchCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

const (
	resHAProxyCfgs         = "haproxycfgs"
	resHAProxyMapFiles     = "haproxymapfiles"
	resHAProxyGeneralFiles = "haproxygeneralfiles"
	resHAProxyCRTListFiles = "haproxycrtlistfiles"
)

// installSSAStatusPatchCounter wires a counting reactor that sits in front of
// the merge reactor. It only observes SSA status patches and always falls
// through (returns false), so it never changes behaviour.
func installSSAStatusPatchCounter(c *fake.Clientset) *ssaStatusPatchCounter {
	counter := &ssaStatusPatchCounter{counts: make(map[string]int)}
	c.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(k8stesting.PatchAction)
		if !ok || pa.GetPatchType() != types.ApplyPatchType || pa.GetSubresource() != statusSubresource {
			return false, nil, nil
		}
		counter.mu.Lock()
		counter.counts[pa.GetResource().Resource]++
		counter.mu.Unlock()
		return false, nil, nil
	})
	return counter
}

func (c *ssaStatusPatchCounter) runtimeConfigs() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[resHAProxyCfgs]
}

func (c *ssaStatusPatchCounter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.counts)
}

// aux returns the total SSA status patches across all three auxiliary-file kinds.
func (c *ssaStatusPatchCounter) aux() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[resHAProxyMapFiles] + c.counts[resHAProxyGeneralFiles] + c.counts[resHAProxyCRTListFiles]
}

// auxPublishRequest publishes one map, one general, and one crt-list file so a
// pod-status update stamps three auxiliary-file CRs.
func auxPublishRequest() PublishRequest {
	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{
		MapFiles: []auxiliaryfiles.MapFile{
			{Path: "/etc/haproxy/maps/host.map", Content: "example.com be_a\n"},
		},
		GeneralFiles: []auxiliaryfiles.GeneralFile{
			{Filename: "503.http", Path: "/etc/haproxy/general/503.http", Content: "HTTP/1.0 503\r\n\r\n"},
		},
		CRTListFiles: []auxiliaryfiles.CRTListFile{
			{Path: "/etc/haproxy/ssl/crt-list.txt", Content: "/etc/haproxy/ssl/x.pem example.com\n"},
		},
	}
	return req
}

// newAuxPublisherWithCounter publishes the aux-file set, then installs the
// counter so only the pod-status stamps under test are counted (not the
// PublishConfig setup writes).
func newAuxPublisherWithCounter(t *testing.T) (context.Context, *Publisher, *ssaStatusPatchCounter) {
	t.Helper()
	ctx, _, crdClient, publisher := newTestPublisher(t)
	req := auxPublishRequest()
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	return ctx, publisher, installSSAStatusPatchCounter(crdClient)
}

func statusUpdate(podName, podUID, podRuntimeID, checksum string) *DeploymentStatusUpdate {
	return &DeploymentStatusUpdate{
		RuntimeConfigName:      "test-config-haproxycfg",
		RuntimeConfigNamespace: "default",
		PodName:                podName,
		PodUID:                 podUID,
		PodRuntimeID:           podRuntimeID,
		Checksum:               checksum,
	}
}

func driftStatusUpdate(podName, podUID, podRuntimeID, checksum string) *DeploymentStatusUpdate {
	u := statusUpdate(podName, podUID, podRuntimeID, checksum)
	u.IsDriftCheck = true
	return u
}

// TestUpdateDeploymentStatus_ElidesUnchangedAuxStamps is the headline win for
// issue #163: the first update stamps every aux file once, and byte-identical
// repeats issue ZERO aux-file SSA patches — while the HAProxyCfg is still
// patched every time (its checksum/plan genuinely changes per deploy).
func TestUpdateDeploymentStatus_ElidesUnchangedAuxStamps(t *testing.T) {
	ctx, publisher, counter := newAuxPublisherWithCounter(t)

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	assert.Equal(t, 3, counter.aux(), "first update stamps all three aux files")
	assert.Equal(t, 1, counter.runtimeConfigs(), "HAProxyCfg patched on first update")

	const repeats = 5
	for range repeats {
		require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	}

	assert.Equal(t, 3, counter.aux(),
		"identical repeats must issue no further aux-file patches (before this fix: %d)", 3+3*repeats)
	assert.Equal(t, 1+repeats, counter.runtimeConfigs(),
		"HAProxyCfg is patched on every update, unchanged by the elision")
}

// TestUpdateDeploymentStatus_RestampsOnChangedPodIdentity verifies that a
// changed podUID or podRuntimeID (and a new pod) re-stamps, while a changed
// deploy checksum alone does not — the aux entry records the aux file's OWN
// immutable content checksum, not the main-config deploy checksum, so a new
// deploy over an unchanged aux set is exactly the redundant write #163 elides.
func TestUpdateDeploymentStatus_RestampsOnChangedPodIdentity(t *testing.T) {
	ctx, publisher, counter := newAuxPublisherWithCounter(t)

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	require.Equal(t, 3, counter.aux())

	// A new deploy checksum over the same aux set leaves aux stamps unchanged.
	req := auxPublishRequest()
	req.Config = "global\n  maxconn 10\n"
	req.Checksum = "def456"
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "def456")))
	assert.Equal(t, 3, counter.aux(), "a new deploy checksum alone does not re-stamp aux files")

	// Changed podUID re-stamps.
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-1", "rt-0", "def456")))
	assert.Equal(t, 6, counter.aux(), "changed podUID re-stamps all aux files")

	// Changed podRuntimeID re-stamps.
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-1", "rt-1", "def456")))
	assert.Equal(t, 9, counter.aux(), "changed podRuntimeID re-stamps all aux files")

	// A different pod re-stamps (its own keys).
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-1", "uid-9", "rt-9", "def456")))
	assert.Equal(t, 12, counter.aux(), "a new pod stamps its own aux entries")
}

// TestUpdateDeploymentStatus_RestampsOnlyChangedAuxiliaryFiles verifies that a new
// content-hashed aux-file set (new set-id → new CR names) re-stamps: the changed
// files are new keys, so nothing is wrongly skipped.
func TestUpdateDeploymentStatus_RestampsOnlyChangedAuxiliaryFiles(t *testing.T) {
	ctx, publisher, counter := newAuxPublisherWithCounter(t)

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	require.Equal(t, 3, counter.aux())
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	require.Equal(t, 3, counter.aux(), "unchanged repeat elided")

	req := auxPublishRequest()
	req.AuxiliaryFiles.MapFiles[0].Content = "example.com be_b\n"
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	assert.Equal(t, 4, counter.aux(), "only the changed map needs a new pod stamp")
}

// TestCleanupPodReferences_EvictsAuxStamps proves the pod-departure invalidation:
// after a pod is cleaned up, a same-named pod re-stamps rather than being elided
// against the departed pod's cached entry.
func TestCleanupPodReferences_EvictsAuxStamps(t *testing.T) {
	ctx, publisher, counter := newAuxPublisherWithCounter(t)

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	require.Equal(t, 3, counter.aux())

	require.NoError(t, publisher.CleanupPodReferences(ctx, &PodCleanupRequest{PodName: "haproxy-0", Namespace: "default"}))

	// Same identity returns: without eviction the value would match the cache
	// and be wrongly skipped. It must re-stamp.
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	assert.Equal(t, 6, counter.aux(), "a departed-then-returned pod re-stamps all aux files")
}

// TestReconcileDeployedToPods_EvictsDepartedAuxStamps proves the reconcile-path
// invalidation: a pod dropped from the running set (e.g. a transient discovery
// blip) re-stamps if it returns with the same identity.
func TestReconcileDeployedToPods_EvictsDepartedAuxStamps(t *testing.T) {
	ctx, publisher, counter := newAuxPublisherWithCounter(t)

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	require.Equal(t, 3, counter.aux())

	// Reconcile with haproxy-0 absent from the running fleet — it is evicted.
	require.NoError(t, publisher.ReconcileDeployedToPods(ctx, "default", []PodIdentity{{PodName: "haproxy-9", PodUID: "uid-9", PodRuntimeID: "rt-9"}}))

	// The same pod returns with the same identity and must re-stamp.
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	assert.Equal(t, 6, counter.aux(), "a pod dropped by reconcile re-stamps on return")
}

// TestReconcileDeployedToPods_KeepsRunningPodStamps verifies a pod still in the
// running set keeps its cache entry — an unchanged repeat after reconcile is
// still elided.
func TestReconcileDeployedToPods_KeepsRunningPodStamps(t *testing.T) {
	ctx, publisher, counter := newAuxPublisherWithCounter(t)

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	require.Equal(t, 3, counter.aux())

	require.NoError(t, publisher.ReconcileDeployedToPods(ctx, "default", []PodIdentity{{PodName: "haproxy-0", PodUID: "uid-0", PodRuntimeID: "rt-0"}}))

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	assert.Equal(t, 3, counter.aux(), "a still-running pod's unchanged repeat stays elided")
}

// TestResetStatusStamps_ForcesRestamp proves the leadership-transition
// invalidation: after a reset a new leader re-stamps once per (pod, file).
func TestResetStatusStamps_ForcesRestamp(t *testing.T) {
	ctx, publisher, counter := newAuxPublisherWithCounter(t)

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	require.Equal(t, 3, counter.aux())
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	require.Equal(t, 3, counter.aux(), "unchanged repeat elided before reset")

	publisher.ResetStatusStamps()

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	assert.Equal(t, 6, counter.aux(), "reset forces one re-stamp per aux file")
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	assert.Equal(t, 6, counter.aux(), "and elision resumes after the re-stamp")
}

// TestStatusStampCache_CommitSkippedWhenInvalidationRacesPatch is the direct unit
// exercise of the check-then-act race guard: an invalidation between beginStamp
// (compare) and commitStamp (record) must drop the record, so the value the CR
// may no longer carry is not cached and the next update re-stamps.
func TestStatusStampCache_CommitSkippedWhenInvalidationRacesPatch(t *testing.T) {
	var c statusStampCache
	key := stampKey{kind: kindMapFile, namespace: "default", name: "haproxy-map-x", podName: "haproxy-0"}
	value := stampedEntry{payload: "p", footprint: "f"}

	_, ok, gen := c.beginStamp(key)
	require.False(t, ok, "nothing cached yet")

	// A pod-departure invalidation races between the (unlocked) Patch and record.
	// It is a no-op on the entries (none yet) but must still bump the generation.
	c.forgetPod("haproxy-0")

	c.commitStamp(key, value, gen)
	_, ok, gen = c.beginStamp(key)
	assert.False(t, ok, "a stamp whose invalidation raced its Patch must not be cached")

	c.commitStamp(key, value, gen)
	last, ok, _ := c.beginStamp(key)
	require.True(t, ok, "an uncontested stamp is cached")
	assert.Equal(t, value, last)
}

// driftReadCounter counts the live reads a drift check takes: GETs of the
// HAProxyCfg and LISTs of auxiliary files.
type driftReadCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func installDriftReadCounter(c *fake.Clientset) *driftReadCounter {
	counter := &driftReadCounter{counts: make(map[string]int)}
	c.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		verb, resource := action.GetVerb(), action.GetResource().Resource
		if (verb == "get" && resource == resHAProxyCfgs) || (verb == "list" && resource != resHAProxyCfgs) {
			counter.mu.Lock()
			counter.counts[verb+" "+resource]++
			counter.mu.Unlock()
		}
		return false, nil, nil
	})
	return counter
}

func (c *driftReadCounter) get(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[key]
}

var testPods = []struct{ name, uid, runtimeID string }{
	{"haproxy-0", "uid-0", "rt-0"},
	{"haproxy-1", "uid-1", "rt-1"},
}

func stampAllPods(t *testing.T, ctx context.Context, publisher *Publisher, update func(name, uid, runtimeID, checksum string) *DeploymentStatusUpdate) {
	t.Helper()
	for _, pod := range testPods {
		require.NoError(t, publisher.UpdateDeploymentStatus(ctx, update(pod.name, pod.uid, pod.runtimeID, "abc123")))
	}
}

// TestUpdateDeploymentStatus_IdleDriftChecksWriteNothing is issue #376: after a
// deployment stamped two pods, drift checks that find every entry as the pods
// left it read each object live and write nothing.
func TestUpdateDeploymentStatus_IdleDriftChecksWriteNothing(t *testing.T) {
	ctx, publisher, counter := newAuxPublisherWithCounter(t)
	crdClient := publisher.crdClient.(*fake.Clientset)
	stampAllPods(t, ctx, publisher, statusUpdate)
	require.Equal(t, 6, counter.aux())
	require.Equal(t, 2, counter.runtimeConfigs())
	reads := installDriftReadCounter(crdClient)

	const rounds = 5
	for range rounds {
		stampAllPods(t, ctx, publisher, driftStatusUpdate)
	}

	assert.Equal(t, 6, counter.aux(), "idle drift checks must not re-apply aux entries (before #376: %d)", 6+6*rounds)
	assert.Equal(t, 2, counter.runtimeConfigs(), "idle drift checks must not re-apply HAProxyCfg entries (before #376: %d)", 2+2*rounds)
	checks := rounds * len(testPods)
	assert.Equal(t, checks, reads.get("get "+resHAProxyCfgs), "every drift check reads the HAProxyCfg live")
	for _, resource := range []string{resHAProxyMapFiles, resHAProxyGeneralFiles, resHAProxyCRTListFiles} {
		assert.Equal(t, checks, reads.get("list "+resource), "every drift check lists %s once", resource)
	}
}

// TestUpdateDeploymentStatus_DriftCheckRestoresChangedEntries proves the drift
// check keeps its guarantee: whatever changed a pod's footprint since its last
// apply is re-applied in the next drift check, and nothing else is.
func TestUpdateDeploymentStatus_DriftCheckRestoresChangedEntries(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		// reapplied counts the pods whose entry the tamper changed.
		reapplied int
		tamper    func(t *testing.T, ctx context.Context, c *fake.Clientset, objects auxObjectNames)
	}{
		{
			name:      "map file entry removed",
			reapplied: 1,
			resource:  resHAProxyMapFiles,
			tamper: func(t *testing.T, ctx context.Context, c *fake.Clientset, objects auxObjectNames) {
				t.Helper()
				client := c.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default")
				obj, err := client.Get(ctx, objects.mapFile, metav1.GetOptions{})
				require.NoError(t, err)
				obj.Status.DeployedToPods = withoutPod(obj.Status.DeployedToPods, "haproxy-0")
				_, err = client.UpdateStatus(ctx, obj, metav1.UpdateOptions{})
				require.NoError(t, err)
			},
		},
		{
			name:      "general file entry edited",
			reapplied: 1,
			resource:  resHAProxyGeneralFiles,
			tamper: func(t *testing.T, ctx context.Context, c *fake.Clientset, objects auxObjectNames) {
				t.Helper()
				client := c.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles("default")
				obj, err := client.Get(ctx, objects.generalFile, metav1.GetOptions{})
				require.NoError(t, err)
				for i := range obj.Status.DeployedToPods {
					if obj.Status.DeployedToPods[i].PodName == "haproxy-0" {
						obj.Status.DeployedToPods[i].Checksum = "forged"
					}
				}
				_, err = client.UpdateStatus(ctx, obj, metav1.UpdateOptions{})
				require.NoError(t, err)
			},
		},
		{
			name:      "crt-list entry ownership lost",
			reapplied: 1,
			resource:  resHAProxyCRTListFiles,
			tamper: func(t *testing.T, ctx context.Context, c *fake.Clientset, objects auxObjectNames) {
				t.Helper()
				client := c.HaproxyTemplateICV1alpha1().HAProxyCRTListFiles("default")
				obj, err := client.Get(ctx, objects.crtListFile, metav1.GetOptions{})
				require.NoError(t, err)
				manager := podStatusFieldManager("haproxy-0")
				obj.ManagedFields = slices.DeleteFunc(obj.ManagedFields, func(entry metav1.ManagedFieldsEntry) bool { return entry.Manager == manager })
				_, err = client.Update(ctx, obj, metav1.UpdateOptions{})
				require.NoError(t, err)
			},
		},
		{
			name:      "HAProxyCfg entry removed",
			reapplied: 1,
			resource:  resHAProxyCfgs,
			tamper: func(t *testing.T, ctx context.Context, c *fake.Clientset, _ auxObjectNames) {
				t.Helper()
				client := c.HaproxyTemplateICV1alpha1().HAProxyCfgs("default")
				obj, err := client.Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
				require.NoError(t, err)
				obj.Status.DeployedToPods = withoutPod(obj.Status.DeployedToPods, "haproxy-0")
				_, err = client.UpdateStatus(ctx, obj, metav1.UpdateOptions{})
				require.NoError(t, err)
			},
		},
		{
			name:      "map file recreated empty",
			reapplied: 2,
			resource:  resHAProxyMapFiles,
			tamper: func(t *testing.T, ctx context.Context, c *fake.Clientset, objects auxObjectNames) {
				t.Helper()
				client := c.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default")
				obj, err := client.Get(ctx, objects.mapFile, metav1.GetOptions{})
				require.NoError(t, err)
				require.NoError(t, client.Delete(ctx, obj.Name, metav1.DeleteOptions{}))
				obj.ResourceVersion = ""
				obj.UID = "recreated"
				obj.ManagedFields = nil
				obj.Status = haproxyv1alpha1.HAProxyMapFileStatus{}
				_, err = client.Create(ctx, obj, metav1.CreateOptions{})
				require.NoError(t, err)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, publisher, counter := newAuxPublisherWithCounter(t)
			crdClient := publisher.crdClient.(*fake.Clientset)
			objects := publishedAuxObjects(t, ctx, crdClient)
			stampAllPods(t, ctx, publisher, statusUpdate)
			stampAllPods(t, ctx, publisher, driftStatusUpdate)
			before := counter.snapshot()

			tt.tamper(t, ctx, crdClient, objects)
			stampAllPods(t, ctx, publisher, driftStatusUpdate)

			after := counter.snapshot()
			assert.Equal(t, before[tt.resource]+tt.reapplied, after[tt.resource], "only the changed entries are re-applied")
			for resource, count := range after {
				if resource != tt.resource {
					assert.Equal(t, before[resource], count, "%s is unchanged and must not be re-applied", resource)
				}
			}
			assertEntryRestored(t, ctx, crdClient, tt.resource, objects)

			stampAllPods(t, ctx, publisher, driftStatusUpdate)
			assert.Equal(t, after, counter.snapshot(), "the restored footprint makes the next drift check idle again")
		})
	}
}

// TestUpdateDeploymentStatus_DriftCheckWithoutMemoryAppliesOnce covers a new
// leader: a publisher that never applied anything must not skip, then idles.
func TestUpdateDeploymentStatus_DriftCheckWithoutMemoryAppliesOnce(t *testing.T) {
	ctx, publisher, counter := newAuxPublisherWithCounter(t)
	stampAllPods(t, ctx, publisher, statusUpdate)
	before := counter.snapshot()

	successor := NewWithListers(publisher.k8sClient, publisher.crdClient, nil, testLogger())
	stampAllPods(t, ctx, successor, driftStatusUpdate)
	assert.Equal(t, before[resHAProxyCfgs]+2, counter.runtimeConfigs(), "a new leader re-asserts each pod's HAProxyCfg entry")
	assert.Equal(t, 6+6, counter.aux(), "a new leader re-asserts each pod's aux entries")

	stampAllPods(t, ctx, successor, driftStatusUpdate)
	assert.Equal(t, before[resHAProxyCfgs]+2, counter.runtimeConfigs())
	assert.Equal(t, 12, counter.aux(), "and its next drift check writes nothing")
}

// TestUpdateDeploymentStatus_DriftCheckStampsNewPod: a pod first seen by a
// drift check has no footprint anywhere and gets every entry.
func TestUpdateDeploymentStatus_DriftCheckStampsNewPod(t *testing.T) {
	ctx, publisher, counter := newAuxPublisherWithCounter(t)
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))
	require.Equal(t, 3, counter.aux())

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, driftStatusUpdate("haproxy-2", "uid-2", "rt-2", "abc123")))
	assert.Equal(t, 6, counter.aux(), "the new pod gets its aux entries")
	assert.Equal(t, 2, counter.runtimeConfigs(), "and its HAProxyCfg entry")
}

// TestUpdateDeploymentStatus_DriftCheckAppliesWhatItCouldNotRead: a kind whose
// live list failed is re-applied unverified; listed kinds still idle.
func TestUpdateDeploymentStatus_DriftCheckAppliesWhatItCouldNotRead(t *testing.T) {
	ctx, publisher, counter := newAuxPublisherWithCounter(t)
	crdClient := publisher.crdClient.(*fake.Clientset)
	stampAllPods(t, ctx, publisher, statusUpdate)
	before := counter.snapshot()
	crdClient.PrependReactor("list", resHAProxyMapFiles, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("list unavailable")
	})

	stampAllPods(t, ctx, publisher, driftStatusUpdate)

	after := counter.snapshot()
	assert.Equal(t, before[resHAProxyMapFiles]+2, after[resHAProxyMapFiles], "unread map files are re-applied for each pod")
	assert.Equal(t, before[resHAProxyGeneralFiles], after[resHAProxyGeneralFiles])
	assert.Equal(t, before[resHAProxyCRTListFiles], after[resHAProxyCRTListFiles])
	assert.Equal(t, before[resHAProxyCfgs], after[resHAProxyCfgs])
}

type auxObjectNames struct {
	mapFile, generalFile, crtListFile string
}

func publishedAuxObjects(t *testing.T, ctx context.Context, c *fake.Clientset) auxObjectNames {
	t.Helper()
	cfg, err := c.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	refs := cfg.Status.AuxiliaryFiles
	require.Len(t, refs.MapFiles, 1)
	require.Len(t, refs.GeneralFiles, 1)
	require.Len(t, refs.CRTListFiles, 1)
	return auxObjectNames{mapFile: refs.MapFiles[0].Name, generalFile: refs.GeneralFiles[0].Name, crtListFile: refs.CRTListFiles[0].Name}
}

func withoutPod(pods []haproxyv1alpha1.PodDeploymentStatus, podName string) []haproxyv1alpha1.PodDeploymentStatus {
	kept := make([]haproxyv1alpha1.PodDeploymentStatus, 0, len(pods))
	for i := range pods {
		if pods[i].PodName != podName {
			kept = append(kept, pods[i])
		}
	}
	return kept
}

func statusObject(t *testing.T, ctx context.Context, c *fake.Clientset, resource string, objects auxObjectNames) metav1.Object {
	t.Helper()
	client := c.HaproxyTemplateICV1alpha1()
	var (
		obj metav1.Object
		err error
	)
	switch resource {
	case resHAProxyCfgs:
		obj, err = client.HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	case resHAProxyMapFiles:
		obj, err = client.HAProxyMapFiles("default").Get(ctx, objects.mapFile, metav1.GetOptions{})
	case resHAProxyGeneralFiles:
		obj, err = client.HAProxyGeneralFiles("default").Get(ctx, objects.generalFile, metav1.GetOptions{})
	case resHAProxyCRTListFiles:
		obj, err = client.HAProxyCRTListFiles("default").Get(ctx, objects.crtListFile, metav1.GetOptions{})
	}
	require.NoError(t, err)
	return obj
}

func assertEntryRestored(t *testing.T, ctx context.Context, c *fake.Clientset, resource string, objects auxObjectNames) {
	t.Helper()
	obj := statusObject(t, ctx, c, resource, objects)
	pods, ok := podStatusesOf(obj)
	require.True(t, ok)
	index := slices.IndexFunc(pods, func(pod haproxyv1alpha1.PodDeploymentStatus) bool { return pod.PodName == "haproxy-0" })
	require.GreaterOrEqual(t, index, 0, "%s lost haproxy-0's entry: %+v", resource, pods)
	assert.Equal(t, "uid-0", pods[index].PodUID)
	assert.NotEqual(t, "forged", pods[index].Checksum)
	manager := podStatusFieldManager("haproxy-0")
	assert.True(t, slices.ContainsFunc(obj.GetManagedFields(), func(entry metav1.ManagedFieldsEntry) bool { return entry.Manager == manager }),
		"%s entry must be owned by %s again", resource, manager)
}

// TestPublishConfig_AuxFileDeleteRecreateReStamps reproduces the content
// oscillation A→B→A: the aux-file CR is deleted (during the B publish) and
// recreated fresh under the SAME content-hashed name (during the revert to A).
// The delete evicts the cache, so the recreated CR is re-stamped and a reader
// observes the pod on it — rather than the pre-delete cache eliding the re-stamp
// and leaving the recreated CR permanently empty.
func TestPublishConfig_AuxFileDeleteRecreateReStamps(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	reqA := auxPublishRequest() // content A
	reqB := auxPublishRequest() // content B (rotates the whole set-id)
	reqB.AuxiliaryFiles.MapFiles[0].Content = "example.com be_b\n"

	// Publish A and stamp pod P onto A's map file.
	resA, err := publisher.PublishConfig(ctx, &reqA)
	require.NoError(t, err)
	mapNameA := resA.MapFileNames[0]
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))

	// Publish B: A's map CR is pruned (deleted) — this must evict the stamp.
	_, err = publisher.PublishConfig(ctx, &reqB)
	require.NoError(t, err)

	// Revert to A: the map CR is recreated fresh (same content-hashed name,
	// empty status.deployedToPods).
	resA2, err := publisher.PublishConfig(ctx, &reqA)
	require.NoError(t, err)
	require.Equal(t, mapNameA, resA2.MapFileNames[0], "content A must reproduce the same content-hashed name")

	// The recreated CR really is empty before the re-stamp.
	recreated, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, mapNameA, metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, recreated.Status.DeployedToPods, "recreated CR starts with empty status")

	// The next status update must re-stamp the recreated CR.
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))

	got, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, mapNameA, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, got.Status.DeployedToPods, 1, "recreated CR must be re-stamped, not elided against the pre-delete cache")
	assert.Equal(t, "haproxy-0", got.Status.DeployedToPods[0].PodName)
}

// TestPublishConfig_AuxFileGCCascadeRecreateReStamps proves the create-path
// eviction: an aux-file CR deleted OUT OF BAND — an apiserver owner-GC cascade
// from `kubectl delete haproxycfg` (or a delete+recreate of the parent
// HAProxyTemplateConfig), which never runs pruneAuxiliaryFiles — and recreated
// fresh on the next publish is re-stamped. The delete here bypasses the prune
// path entirely, so only the create-branch eviction can save it.
func TestPublishConfig_AuxFileGCCascadeRecreateReStamps(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	req := auxPublishRequest()
	res, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	mapName := res.MapFileNames[0]

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))

	// Simulate the owner-GC cascade: delete the CR directly, bypassing
	// pruneAuxiliaryFiles (so the delete-path eviction never runs).
	require.NoError(t, crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Delete(ctx, mapName, metav1.DeleteOptions{}))

	// Republish the identical config: the map CR is recreated fresh (same
	// content-hashed name, empty status). The create-path eviction must drop the
	// stale stamp.
	res2, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.Equal(t, mapName, res2.MapFileNames[0])

	recreated, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, mapName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, recreated.Status.DeployedToPods, "GC-recreated CR starts empty")

	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "rt-0", "abc123")))

	got, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, mapName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, got.Status.DeployedToPods, 1, "GC-recreated CR must be re-stamped via the create-path eviction")
	assert.Equal(t, "haproxy-0", got.Status.DeployedToPods[0].PodName)
}
