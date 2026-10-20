// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package configpublisher

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
)

// beforeEachSpecWrite runs mutate on the stored HAProxyCfg right before every
// spec write reaches it, and counts those writes.
func beforeEachSpecWrite(t *testing.T, client *fake.Clientset, mutate func(*haproxyv1alpha1.HAProxyCfg)) *int {
	t.Helper()
	resource := haproxyv1alpha1.SchemeGroupVersion.WithResource("haproxycfgs")
	writes := 0
	client.PrependReactor("*", "haproxycfgs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "" || (action.GetVerb() != "update" && action.GetVerb() != "patch") {
			return false, nil, nil
		}
		writes++
		obj, err := client.Tracker().Get(resource, "default", "test-config-haproxycfg")
		require.NoError(t, err)
		cfg := obj.(*haproxyv1alpha1.HAProxyCfg)
		mutate(cfg)
		require.NoError(t, client.Tracker().Update(resource, cfg, cfg.Namespace))
		return false, nil, nil
	})
	return &writes
}

func TestPublishConfig_StatusWriteDuringSpecWriteDoesNotConflict(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	installTrackerResourceVersions(publisher, crdClient)
	initial := basePublishRequest()
	_, err := publisher.PublishConfig(ctx, &initial)
	require.NoError(t, err)

	statusWrites := 0
	specWrites := beforeEachSpecWrite(t, crdClient, func(cfg *haproxyv1alpha1.HAProxyCfg) {
		statusWrites++
		cfg.Status.DeployedToPods = []haproxyv1alpha1.PodDeploymentStatus{{PodName: "haproxy-0", Checksum: "abc123"}}
		cfg.ResourceVersion = "pod-status-" + strconv.Itoa(statusWrites)
	})

	next := basePublishRequest()
	next.Config = "global\n  maxconn 42\n"
	next.Checksum = "def456"
	_, err = publisher.PublishConfig(ctx, &next)
	require.NoError(t, err)

	assert.Equal(t, 1, *specWrites, "a status write must not send the spec write into a conflict retry")
	current, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, next.Config, current.Spec.Content)
	assert.Equal(t, "def456", current.Spec.Checksum)
	assert.Len(t, current.Status.DeployedToPods, 1, "the spec write leaves concurrent status alone")
}

func TestCreateOrUpdateRuntimeConfig_RetainedRefusesOwnerChangedBeforeWrite(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	installTrackerResourceVersions(publisher, crdClient)
	req := basePublishRequest()
	req.RetainedPlan = "plan-v1"
	_, err := publisher.createOrUpdateRuntimeConfig(ctx, &req)
	require.NoError(t, err)

	foreign := []metav1.OwnerReference{{APIVersion: apiVersionV1Alpha1, Kind: kindTemplateConfig, Name: "other", UID: "other-uid"}}
	beforeEachSpecWrite(t, crdClient, func(cfg *haproxyv1alpha1.HAProxyCfg) {
		cfg.OwnerReferences = foreign
	})

	changed := req
	changed.Config = "global\n  maxconn 42\n"
	_, err = publisher.createOrUpdateRuntimeConfig(ctx, &changed)
	require.Error(t, err)

	current, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, foreign, current.OwnerReferences)
	assert.Equal(t, req.Config, current.Spec.Content, "a checkpoint that changed owner is not taken over")
}
