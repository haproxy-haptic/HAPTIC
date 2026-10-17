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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	crdclientfake "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type successfulPublicationGate struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newSuccessfulPublicationGate() *successfulPublicationGate {
	return &successfulPublicationGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *successfulPublicationGate) react(action k8stesting.Action) (bool, runtime.Object, error) {
	if action.GetSubresource() != "status" || action.(k8stesting.PatchAction).GetPatchType() != types.JSONPatchType {
		return false, nil, nil
	}
	blocked := false
	g.once.Do(func() {
		blocked = true
		close(g.entered)
	})
	if !blocked {
		return false, nil, nil
	}
	<-g.release
	return false, nil, nil
}

func newPublicationAuthorityComponent(t *testing.T) (*Component, *crdclientfake.Clientset, <-chan busevents.Event) {
	t.Helper()
	crdClient := crdclientfake.NewSimpleClientset()
	installRuntimeUIDReactor(crdClient)
	bus := busevents.NewEventBus(20)
	publisher := configpublisher.NewWithListers(
		k8sfake.NewClientset(), crdClient, nil, testutil.NewTestLogger())
	component := New(publisher, bus, testutil.NewTestLogger())
	component.mu.Lock()
	component.publicationTerm = 1
	component.mu.Unlock()
	events := bus.Subscribe("publication-authority", 20)
	bus.Start()
	return component, crdClient, events
}

func publicationAuthorityWork(component *Component, correlationID, checksum string) *publishWorkItem {
	templateConfig := &v1alpha1.HAProxyTemplateConfig{ObjectMeta: metav1.ObjectMeta{
		Name: "test-config", Namespace: "default", UID: types.UID("template-uid"),
	}}
	entry := &renderedConfigEntry{config: "global\n  daemon\n# " + checksum, contentChecksum: checksum}
	component.mu.Lock()
	component.renderedConfigs[correlationID] = entry
	component.mu.Unlock()
	return component.makePublishWorkItem(correlationID, templateConfig, entry, false)
}

func TestSuccessfulPublishDoesNotCommitAfterGenerationSuperseded(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	component, crdClient, publishedEvents := newPublicationAuthorityComponent(t)
	gate := newSuccessfulPublicationGate()
	crdClient.PrependReactor("patch", "haproxycfgs", gate.react)

	workA := publicationAuthorityWork(component, "generation-a", "checksum-a")
	done := make(chan struct{})
	go func() {
		component.executePublish(ctx, workA)
		close(done)
	}()
	waitForTestSignal(t, ctx, gate.entered, "publication did not reach its final status write")

	workB := publicationAuthorityWork(component, "generation-b", "checksum-b")
	close(gate.release)
	waitForTestSignal(t, ctx, done, "superseded publication did not return")

	component.mu.RLock()
	assert.Empty(t, component.lastPublishedChecksum)
	component.mu.RUnlock()
	assert.Zero(t, countConfigPublishedEvents(publishedEvents))

	component.executePublish(ctx, workB)
	waitForConfigPublished(t, ctx, publishedEvents)
	component.mu.RLock()
	assert.Equal(t, "checksum-b", component.lastPublishedChecksum)
	component.mu.RUnlock()
}

func TestSuccessfulPublishDoesNotCommitAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	component, crdClient, publishedEvents := newPublicationAuthorityComponent(t)
	gate := newSuccessfulPublicationGate()
	crdClient.PrependReactor("patch", "haproxycfgs", gate.react)

	work := publicationAuthorityWork(component, "canceled-generation", "checksum-a")
	done := make(chan struct{})
	go func() {
		component.executePublish(ctx, work)
		close(done)
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not reach its final status write")
	}

	cancel()
	close(gate.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled publication did not return")
	}

	component.mu.RLock()
	assert.Empty(t, component.lastPublishedChecksum)
	component.mu.RUnlock()
	assert.Zero(t, countConfigPublishedEvents(publishedEvents))
}

func TestPermanentPublicationFailureDoesNotStarveDeployedQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	crdClient := crdclientfake.NewSimpleClientset()
	installRuntimeUIDReactor(crdClient)
	crdClient.PrependReactor("create", "haproxycfgs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		cfg := action.(k8stesting.CreateAction).GetObject().(*v1alpha1.HAProxyCfg)
		if cfg.Spec.Checksum != "terminal" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "haproxy-haptic.org", Resource: "haproxycfgs"},
			cfg.Name,
			errors.New("publication denied"),
		)
	})
	bus := busevents.NewEventBus(20)
	publisher := configpublisher.NewWithListers(
		k8sfake.NewClientset(), crdClient, nil, testutil.NewTestLogger())
	component := New(publisher, bus, testutil.NewTestLogger())
	component.mu.Lock()
	component.publicationTerm = 1
	component.mu.Unlock()
	var retryWaits atomic.Int32
	component.publicationRetryWait = func(context.Context, time.Duration, <-chan struct{}) bool {
		retryWaits.Add(1)
		return true
	}
	publishedEvents := bus.Subscribe("permanent-publication-error", 20)
	bus.Start()

	templateConfig := &v1alpha1.HAProxyTemplateConfig{ObjectMeta: metav1.ObjectMeta{
		Name: "test-config", Namespace: "default", UID: types.UID("template-uid"),
	}}
	makeDeployedWork := func(checksum string) *publishWorkItem {
		return component.makePublishWorkItem(
			"deployed:"+checksum,
			templateConfig,
			&renderedConfigEntry{config: "global\n# " + checksum, contentChecksum: checksum},
			true,
		)
	}
	component.enqueueDeployed(makeDeployedWork("terminal"))
	component.enqueueDeployed(makeDeployedWork("complete"))

	workerDone := make(chan struct{})
	go func() {
		component.publishWorker(ctx)
		close(workerDone)
	}()
	waitForConfigPublished(t, ctx, publishedEvents)

	component.mu.RLock()
	assert.Equal(t, "complete", component.lastPublishedChecksum)
	component.mu.RUnlock()
	assert.Zero(t, retryWaits.Load())
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "complete", runtimeConfig.Spec.Checksum)

	cancel()
	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("publish worker did not stop")
	}
}

func TestThrottleFlushKeepsDeployedFIFOAheadOfValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	crdClient := crdclientfake.NewSimpleClientset()
	installRuntimeUIDReactor(crdClient)
	bus := busevents.NewEventBus(20)
	publisher := configpublisher.NewWithListers(
		k8sfake.NewClientset(), crdClient, nil, testutil.NewTestLogger())
	component := New(publisher, bus, testutil.NewTestLogger(), WithPublishInterval(time.Hour))
	t.Cleanup(component.publishThrottle.Stop)
	t.Cleanup(component.statusThrottle.Stop)
	component.mu.Lock()
	component.publicationTerm = 1
	component.mu.Unlock()
	bus.Start()

	templateConfig := &v1alpha1.HAProxyTemplateConfig{ObjectMeta: metav1.ObjectMeta{
		Name: "test-config", Namespace: "default", UID: types.UID("template-uid"),
	}}
	makeWork := func(correlationID, checksum string, deployed bool) *publishWorkItem {
		return component.makePublishWorkItem(
			correlationID,
			templateConfig,
			&renderedConfigEntry{config: "global\n# " + checksum, contentChecksum: checksum},
			deployed,
		)
	}
	component.enqueueDeployed(makeWork("deployed:first", "first", true))
	component.enqueueDeployed(makeWork("deployed:second", "second", true))
	validation := makeWork("validation:latest", "validation", false)
	component.pendingMu.Lock()
	component.pendingPublish = validation
	component.pendingMu.Unlock()

	assertPublishedChecksum := func(want string) {
		component.flushPendingPublish(ctx)
		runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
			Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, want, runtimeConfig.Spec.Checksum)
	}

	assertPublishedChecksum("first")
	assert.Equal(t, 1, component.deployedQueueDepth())
	component.pendingMu.Lock()
	assert.Same(t, validation, component.pendingPublish)
	component.pendingMu.Unlock()

	assertPublishedChecksum("second")
	assert.Zero(t, component.deployedQueueDepth())
	component.pendingMu.Lock()
	assert.Same(t, validation, component.pendingPublish)
	component.pendingMu.Unlock()

	assertPublishedChecksum("validation")
	component.pendingMu.Lock()
	assert.Nil(t, component.pendingPublish)
	component.pendingMu.Unlock()
}

func TestValidationPublishRepairsSameChecksumDesiredState(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	component, crdClient, publishedEvents := newPublicationAuthorityComponent(t)
	templateConfig := &v1alpha1.HAProxyTemplateConfig{ObjectMeta: metav1.ObjectMeta{
		Name: "test-config", Namespace: "default", UID: types.UID("template-uid"),
	}}
	entry := &renderedConfigEntry{
		config:          "global\n  daemon\n",
		contentChecksum: "stable-checksum",
		auxFiles: &dataplane.AuxiliaryFiles{
			MapFiles: []auxiliaryfiles.MapFile{{
				Path: "/etc/haproxy/maps/host.map", Content: "example.com backend\n",
			}},
		},
	}
	makeWork := func(correlationID string) *publishWorkItem {
		component.mu.Lock()
		component.renderedConfigs[correlationID] = entry
		component.mu.Unlock()
		return component.makePublishWorkItem(correlationID, templateConfig, entry, false)
	}

	component.executePublish(ctx, makeWork("initial"))
	waitForConfigPublished(t, ctx, publishedEvents)
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, runtimeConfig.Status.AuxiliaryFiles)
	require.Len(t, runtimeConfig.Status.AuxiliaryFiles.MapFiles, 1)
	mapFileName := runtimeConfig.Status.AuxiliaryFiles.MapFiles[0].Name
	require.NoError(t, crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Delete(ctx, mapFileName, metav1.DeleteOptions{}))

	component.processPublishWork(ctx, makeWork("repair"))
	mapFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Get(ctx, mapFileName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "example.com backend\n", mapFile.Spec.Entries)
	assert.Zero(t, countConfigPublishedEvents(publishedEvents))
}
