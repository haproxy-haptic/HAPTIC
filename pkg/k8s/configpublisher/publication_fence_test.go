// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package configpublisher

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
	listersv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/generated/listers/haproxytemplate/v1alpha1"
)

// installTrackerResourceVersions serves resourceVersion reads from the fake's
// tracker, which records no action, the way a metadata-only GET costs no
// content transfer.
func installTrackerResourceVersions(publisher *Publisher, client *fake.Clientset) {
	resource := haproxyv1alpha1.SchemeGroupVersion.WithResource("haproxycfgs")
	// The fake tracker leaves resourceVersion alone; stamp a fresh one on
	// every write the way the apiserver does.
	version := 0
	write := k8stesting.ObjectReaction(client.Tracker())
	client.PrependReactor("*", "haproxycfgs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if patch, ok := action.(k8stesting.PatchAction); ok && patch.GetPatchType() == types.ApplyPatchType {
			return false, nil, nil
		}
		switch action.GetVerb() {
		case "update", "patch":
		default:
			return false, nil, nil
		}
		if update, ok := action.(k8stesting.UpdateAction); ok && update.GetSubresource() == "" {
			requested := update.GetObject().(*haproxyv1alpha1.HAProxyCfg)
			stored, err := client.Tracker().Get(resource, requested.Namespace, requested.Name)
			if err == nil && requested.ResourceVersion != stored.(*haproxyv1alpha1.HAProxyCfg).ResourceVersion {
				return true, nil, apierrors.NewConflict(resource.GroupResource(), requested.Name, errors.New("stale resourceVersion"))
			}
		}
		handled, obj, err := write(action)
		if !handled || err != nil {
			return handled, obj, err
		}
		cfg := obj.(*haproxyv1alpha1.HAProxyCfg)
		version++
		cfg.ResourceVersion = strconv.Itoa(version)
		return true, cfg, client.Tracker().Update(resource, cfg, cfg.Namespace)
	})
	publisher.resourceVersionOf = func(_ context.Context, namespace, name string) (string, error) {
		obj, err := client.Tracker().Get(resource, namespace, name)
		if err != nil {
			return "", err
		}
		return obj.(*haproxyv1alpha1.HAProxyCfg).ResourceVersion, nil
	}
}

func mapFilesRequest(content string, count int) PublishRequest {
	req := basePublishRequest()
	req.Checksum = content
	req.AuxiliaryFiles = &AuxiliaryFiles{}
	for i := range count {
		req.AuxiliaryFiles.MapFiles = append(req.AuxiliaryFiles.MapFiles,
			auxiliaryfiles.MapFile{Path: fmt.Sprintf("/maps/m%d.map", i), Content: content})
	}
	return req
}

func countRuntimeConfigGets(client *fake.Clientset) int {
	gets := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "haproxycfgs" {
			gets++
		}
	}
	return gets
}

func TestPublishConfig_ChangeReadsRuntimeConfigOnce(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	installTrackerResourceVersions(publisher, crdClient)
	initial := mapFilesRequest("v1", 8)
	_, err := publisher.PublishConfig(ctx, &initial)
	require.NoError(t, err)
	crdClient.ClearActions()

	next := mapFilesRequest("v2", 8)
	_, err = publisher.PublishConfig(ctx, &next)
	require.NoError(t, err)

	// One read to update the spec and one for the first stale-child check;
	// the 7 further deletions re-check the unchanged resourceVersion only.
	assert.Equal(t, 2, countRuntimeConfigGets(crdClient))
	mapFiles, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, mapFiles.Items, 8)
}

func TestPublishConfig_StaleCleanupStopsWhenSupersededMidPrune(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	installTrackerResourceVersions(publisher, crdClient)
	initial := mapFilesRequest("v1", 3)
	initialResult, err := publisher.PublishConfig(ctx, &initial)
	require.NoError(t, err)

	resource := haproxyv1alpha1.SchemeGroupVersion.WithResource("haproxycfgs")
	superseded := false
	crdClient.PrependReactor("delete", "haproxymapfiles", func(k8stesting.Action) (bool, runtime.Object, error) {
		if superseded {
			return false, nil, nil
		}
		superseded = true
		obj, err := crdClient.Tracker().Get(resource, "default", "test-config-haproxycfg")
		require.NoError(t, err)
		cfg := obj.(*haproxyv1alpha1.HAProxyCfg)
		cfg.Annotations[AuxiliarySetIDAnnotationKey] = "sha256:newer-publication"
		cfg.ResourceVersion = "superseded"
		require.NoError(t, crdClient.Tracker().Update(resource, cfg, cfg.Namespace))
		return false, nil, nil
	})

	next := mapFilesRequest("v2", 3)
	_, err = publisher.PublishConfig(ctx, &next)
	require.ErrorContains(t, err, "superseded")

	remaining := 0
	for _, name := range initialResult.MapFileNames {
		if _, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, name, metav1.GetOptions{}); err == nil {
			remaining++
		}
	}
	assert.Equal(t, 2, remaining, "only the deletion that raced the newer publication may land")
}

func TestPublishConfig_ParallelChildrenKeepInputOrder(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	req := mapFilesRequest("v1", 3*auxiliaryPublishConcurrency)
	result, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	canonical, err := canonicalizePublishRequest(&req)
	require.NoError(t, err)
	require.Len(t, result.MapFileNames, len(canonical.AuxiliaryFiles.MapFiles))
	for i, name := range result.MapFileNames {
		file, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, canonical.AuxiliaryFiles.MapFiles[i].Path, file.Spec.Path)
	}
}

func TestPublishConfig_ChangeUpdatesFromInformerCache(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%v", stale), func(t *testing.T) {
			ctx, _, crdClient, publisher := newTestPublisher(t)
			installTrackerResourceVersions(publisher, crdClient)
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			publisher.listers = &Listers{HAProxyCfgs: listersv1alpha1.NewHAProxyCfgLister(indexer)}
			initial := mapFilesRequest("v1", 1)
			_, err := publisher.PublishConfig(ctx, &initial)
			require.NoError(t, err)

			resource := haproxyv1alpha1.SchemeGroupVersion.WithResource("haproxycfgs")
			stored, err := crdClient.Tracker().Get(resource, "default", "test-config-haproxycfg")
			require.NoError(t, err)
			cached := stored.(*haproxyv1alpha1.HAProxyCfg).DeepCopy()
			if stale {
				cached.ResourceVersion = "stale"
			}
			require.NoError(t, indexer.Add(cached))
			crdClient.ClearActions()

			next := mapFilesRequest("v2", 1)
			next.Config = "global\n  maxconn 42\n"
			_, err = publisher.PublishConfig(ctx, &next)
			require.NoError(t, err)

			current, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
				Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, next.Config, current.Spec.Content)
			// Two reads: the prune fence's and the one above. The spec patch
			// carries no resourceVersion, so a stale cache costs no live read.
			assert.Equal(t, 2, countRuntimeConfigGets(crdClient))
		})
	}
}
