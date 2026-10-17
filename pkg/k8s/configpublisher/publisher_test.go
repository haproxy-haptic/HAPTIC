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
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// testLogger creates a slog logger for tests that discards output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestPublisher builds a Publisher over fake clients. The SSA list-map merge
// reactor is what makes per-pod status patches accumulate the way a real
// apiserver applies them.
func newTestPublisher(t *testing.T) (ctx context.Context, k8sClient *k8sfake.Clientset, crdClient *fake.Clientset, publisher *Publisher) {
	t.Helper()
	k8sClient = k8sfake.NewClientset()
	crdClient = fake.NewSimpleClientset()
	installSSAListMapMergeReactor(crdClient)
	return context.Background(), k8sClient, crdClient, NewWithListers(k8sClient, crdClient, nil, testLogger())
}

// basePublishRequest is the minimal valid request the tests vary from.
func basePublishRequest() PublishRequest {
	return PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       types.UID("test-uid-123"),
		Config:                  "global\n  daemon\n",
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                "abc123",
	}
}

func TestPublishConfig_CreateNew(t *testing.T) {
	ctx, k8sClient, crdClient, publisher := newTestPublisher(t)

	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{
		MapFiles: []auxiliaryfiles.MapFile{
			{
				Path:    "/etc/haproxy/maps/host.map",
				Content: "example.com backend1\n",
			},
		},
		SSLCertificates: []auxiliaryfiles.SSLCertificate{
			{
				Path:    "/etc/haproxy/ssl/cert.pem",
				Content: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n",
			},
		},
	}

	result, err := publisher.PublishConfig(ctx, &req)

	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "test-config-haproxycfg", result.RuntimeConfigName)
	assert.Equal(t, "default", result.RuntimeConfigNamespace)
	assert.Len(t, result.MapFileNames, 1)
	assert.Len(t, result.SecretNames, 1)

	// Verify HAProxyCfg was created
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	assert.Equal(t, "/etc/haproxy/haproxy.cfg", runtimeConfig.Spec.Path)
	assert.Equal(t, "global\n  daemon\n", runtimeConfig.Spec.Content)
	assert.Equal(t, "abc123", runtimeConfig.Spec.Checksum)

	// Verify owner reference
	require.Len(t, runtimeConfig.OwnerReferences, 1)
	assert.Equal(t, "HAProxyTemplateConfig", runtimeConfig.OwnerReferences[0].Kind)
	assert.Equal(t, "test-config", runtimeConfig.OwnerReferences[0].Name)
	assert.Equal(t, types.UID("test-uid-123"), runtimeConfig.OwnerReferences[0].UID)

	// Verify map file was created
	mapFiles, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyMapFiles("default").
		List(ctx, metav1.ListOptions{})

	require.NoError(t, err)
	require.Len(t, mapFiles.Items, 1)
	assert.Equal(t, "/etc/haproxy/maps/host.map", mapFiles.Items[0].Spec.Path)
	assert.Equal(t, "example.com backend1\n", mapFiles.Items[0].Spec.Entries)

	// Verify SSL secret was created
	secrets, err := k8sClient.CoreV1().
		Secrets("default").
		List(ctx, metav1.ListOptions{})

	require.NoError(t, err)
	require.Len(t, secrets.Items, 1)
	assert.Contains(t, secrets.Items[0].Data, "certificate")
	assert.Contains(t, secrets.Items[0].Data, "path")
	assert.Equal(t, []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"),
		secrets.Items[0].Data["certificate"])
	assert.Equal(t, []byte("/etc/haproxy/ssl/cert.pem"),
		secrets.Items[0].Data["path"])
}

func TestPublishConfig_RejectsConflictingAuxiliaryIdentitiesBeforeMutation(t *testing.T) {
	ctx, k8sClient, crdClient, publisher := newTestPublisher(t)
	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{
		{Path: "routes.map", Content: "one"},
		{Path: "routes.map", Content: "two"},
	}}

	result, err := publisher.PublishConfig(ctx, &req)

	require.Error(t, err)
	assert.Nil(t, result)
	assert.False(t, IsRetryablePublicationError(err))
	configs, listErr := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, configs.Items)
	mapFiles, listErr := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, mapFiles.Items)
	secrets, listErr := k8sClient.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, secrets.Items)
}

func TestPublishConfig_DeduplicatesIdenticalAuxiliaryIdentities(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{
		{Path: "routes.map", Content: "one"},
		{Path: "routes.map", Content: "one"},
	}}

	result, err := publisher.PublishConfig(ctx, &req)

	require.NoError(t, err)
	require.Len(t, result.MapFileNames, 1)
	configs, listErr := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	require.Len(t, configs.Items, 1)
	require.NotNil(t, configs.Items[0].Status.AuxiliaryFiles)
	assert.Len(t, configs.Items[0].Status.AuxiliaryFiles.MapFiles, 1)
}

func TestPublishConfig_StaleCleanupCannotDeleteNewPublication(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	initial := basePublishRequest()
	initial.Checksum = "initial"
	initial.AuxiliaryFiles = &AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{
		{Path: "keep.map", Content: "keep"},
		{Path: "restored.map", Content: "restored"},
	}}
	initialResult, err := publisher.PublishConfig(ctx, &initial)
	require.NoError(t, err)

	staleRequest := initial
	staleRequest.Checksum = "stale-exclusion"
	staleRequest.AuxiliaryFiles = &AuxiliaryFiles{MapFiles: initial.AuxiliaryFiles.MapFiles[:1]}
	canonicalStale, err := canonicalizePublishRequest(&staleRequest)
	require.NoError(t, err)
	staleRuntimeConfig, err := publisher.createOrUpdateRuntimeConfig(ctx, canonicalStale)
	require.NoError(t, err)
	staleResult := &PublishResult{
		RuntimeConfigName:      staleRuntimeConfig.Name,
		RuntimeConfigNamespace: staleRuntimeConfig.Namespace,
		MapFileNames:           initialResult.MapFileNames[:1],
	}

	newRequest := initial
	newRequest.Checksum = "new-inclusion"
	_, err = publisher.PublishConfig(ctx, &newRequest)
	require.NoError(t, err)

	err = publisher.pruneAuxiliaryFiles(ctx, staleRuntimeConfig, staleResult, newChildListing("default", runtimeConfigSelector(staleRuntimeConfig.Name)))
	require.ErrorContains(t, err, "publication was superseded; publish the current output")
	_, err = crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Get(ctx, initialResult.MapFileNames[1], metav1.GetOptions{})
	require.NoError(t, err)
}

func TestPublishConfig_StaleCleanupCannotDeleteNewNameForSameAuxiliarySet(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{{Path: "/maps/host.map", Content: "ours"}}}
	staleResult, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	configs := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default")
	staleRuntimeConfig, err := configs.Get(ctx, staleResult.RuntimeConfigName, metav1.GetOptions{})
	require.NoError(t, err)
	maps := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default")
	replacement, err := maps.Get(ctx, staleResult.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	replacement.Name = "replacement-" + replacement.Name
	replacement.ResourceVersion = ""
	_, err = maps.Create(ctx, replacement, metav1.CreateOptions{})
	require.NoError(t, err)
	current := staleRuntimeConfig.DeepCopy()
	current.Status.AuxiliaryFiles.MapFiles[0].Name = replacement.Name
	_, err = configs.UpdateStatus(ctx, current, metav1.UpdateOptions{})
	require.NoError(t, err)

	err = publisher.pruneAuxiliaryFiles(ctx, staleRuntimeConfig, staleResult, newChildListing("default", runtimeConfigSelector(staleRuntimeConfig.Name)))
	require.ErrorContains(t, err, "no longer owns the committed auxiliary references")
	_, err = maps.Get(ctx, replacement.Name, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestPublishConfig_ReportsIncompleteAuxiliaryPublication(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	crdClient.PrependReactor("create", "haproxymapfiles", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("map storage unavailable")
	})

	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{{
		Path: "/etc/haproxy/maps/host.map", Content: "example.com backend1\n",
	}}}
	result, err := publisher.PublishConfig(ctx, &req)

	require.Error(t, err)
	require.NotNil(t, result)
	var publicationErr *IncompletePublicationError
	require.ErrorAs(t, err, &publicationErr)
	assert.Equal(t, PublicationStageAuxiliary, publicationErr.Stage)
	assert.Equal(t, "HAProxyMapFile", publicationErr.ResourceKind)
	assert.True(t, strings.HasPrefix(publicationErr.ResourceName, "haproxy-map-host-"))
	assert.Empty(t, result.MapFileNames)

	runtimeConfig, getErr := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Nil(t, runtimeConfig.Status.AuxiliaryFiles)
}

func TestPublishConfig_RetriesIncompleteAuxiliaryReferences(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	statusFailures := 1
	crdClient.PrependReactor("patch", "haproxycfgs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != statusSubresource || statusFailures == 0 {
			return false, nil, nil
		}
		statusFailures--
		return true, nil, apierrors.NewServiceUnavailable("status storage unavailable")
	})

	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{{
		Path: "/etc/haproxy/maps/host.map", Content: "example.com backend1\n",
	}}}
	result, err := publisher.PublishConfig(ctx, &req)
	require.Error(t, err)
	require.Len(t, result.MapFileNames, 1)
	var publicationErr *IncompletePublicationError
	require.ErrorAs(t, err, &publicationErr)
	assert.Equal(t, PublicationStageReferences, publicationErr.Stage)

	result, err = publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, runtimeConfig.Status.AuxiliaryFiles)
	require.Len(t, runtimeConfig.Status.AuxiliaryFiles.MapFiles, 1)
	assert.Equal(t, result.MapFileNames[0], runtimeConfig.Status.AuxiliaryFiles.MapFiles[0].Name)
}

func TestPublishConfig_RetriesValidationStatusWithUnchangedChecksum(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	statusFailures := 1
	crdClient.PrependReactor("patch", "haproxycfgs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != statusSubresource || statusFailures == 0 {
			return false, nil, nil
		}
		statusFailures--
		return true, nil, apierrors.NewServiceUnavailable("status storage unavailable")
	})

	req := basePublishRequest()
	req.NameSuffix = "-invalid"
	req.ValidationError = "maxconn must be numeric"
	_, err := publisher.PublishConfig(ctx, &req)
	require.Error(t, err)
	var publicationErr *IncompletePublicationError
	require.True(t, errors.As(err, &publicationErr))
	assert.Equal(t, PublicationStageRuntimeConfig, publicationErr.Stage)

	_, err = publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg-invalid", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, req.ValidationError, runtimeConfig.Status.ValidationError)
}

func TestPublishConfig_IsolatesInvalidAuxiliaryResources(t *testing.T) {
	ctx, k8sClient, crdClient, publisher := newTestPublisher(t)
	valid := basePublishRequest()
	valid.AuxiliaryFiles = &AuxiliaryFiles{
		MapFiles:        []auxiliaryfiles.MapFile{{Path: "/maps/host.map", Content: "valid map"}},
		SSLCertificates: []auxiliaryfiles.SSLCertificate{{Path: "/certs/site.pem", Content: "valid cert"}},
		GeneralFiles:    []auxiliaryfiles.GeneralFile{{Filename: "error.http", Path: "/files/error.http", Content: "valid file"}},
		CRTListFiles:    []auxiliaryfiles.CRTListFile{{Path: "/lists/site.list", Content: "valid list"}},
	}
	validResult, err := publisher.PublishConfig(ctx, &valid)
	require.NoError(t, err)

	invalid := valid
	invalid.Config = "invalid config"
	invalid.Checksum = "invalid-checksum"
	invalid.NameSuffix = "-invalid"
	invalid.ValidationError = "configuration rejected"
	invalid.AuxiliaryFiles = &AuxiliaryFiles{
		MapFiles:        []auxiliaryfiles.MapFile{{Path: "/maps/host.map", Content: "invalid map"}},
		SSLCertificates: []auxiliaryfiles.SSLCertificate{{Path: "/certs/site.pem", Content: "invalid cert"}},
		GeneralFiles:    []auxiliaryfiles.GeneralFile{{Filename: "error.http", Path: "/files/error.http", Content: "invalid file"}},
		CRTListFiles:    []auxiliaryfiles.CRTListFile{{Path: "/lists/site.list", Content: "invalid list"}},
	}
	invalidResult, err := publisher.PublishConfig(ctx, &invalid)
	require.NoError(t, err)

	for _, names := range [][]string{
		validResult.MapFileNames,
		validResult.SecretNames,
		validResult.GeneralFileNames,
		validResult.CRTListFileNames,
	} {
		require.Len(t, names, 1)
		assert.False(t, strings.HasSuffix(names[0], "-invalid"))
	}
	for _, names := range [][]string{
		invalidResult.MapFileNames,
		invalidResult.SecretNames,
		invalidResult.GeneralFileNames,
		invalidResult.CRTListFileNames,
	} {
		require.Len(t, names, 1)
		assert.True(t, strings.HasSuffix(names[0], "-invalid"))
	}

	validMap, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Get(ctx, validResult.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	invalidMap, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Get(ctx, invalidResult.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "valid map", validMap.Spec.Entries)
	assert.Equal(t, "invalid map", invalidMap.Spec.Entries)
	require.Len(t, validMap.OwnerReferences, 1)
	require.Len(t, invalidMap.OwnerReferences, 1)
	assert.Equal(t, validResult.RuntimeConfigName, validMap.OwnerReferences[0].Name)
	assert.Equal(t, invalidResult.RuntimeConfigName, invalidMap.OwnerReferences[0].Name)

	validSecret, err := k8sClient.CoreV1().Secrets("default").
		Get(ctx, validResult.SecretNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	invalidSecret, err := k8sClient.CoreV1().Secrets("default").
		Get(ctx, invalidResult.SecretNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("valid cert"), validSecret.Data["certificate"])
	assert.Equal(t, []byte("invalid cert"), invalidSecret.Data["certificate"])
}

func TestPublishConfig_DisambiguatesCollidingAuxiliaryResourceNames(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{GeneralFiles: []auxiliaryfiles.GeneralFile{
		{Filename: "error.http", Path: "/files/error.http", Content: "HTTP error page"},
		{Filename: "error.lua", Path: "/files/error.lua", Content: "Lua error handler"},
	}}

	result, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.Len(t, result.GeneralFileNames, 2)
	assert.NotEqual(t, result.GeneralFileNames[0], result.GeneralFileNames[1])

	published := make(map[string]string, 2)
	for _, name := range result.GeneralFileNames {
		file, getErr := crdClient.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles("default").
			Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, getErr)
		published[file.Spec.FileName] = file.Spec.Content
	}
	assert.Equal(t, map[string]string{
		"error.http": "HTTP error page",
		"error.lua":  "Lua error handler",
	}, published)

	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, result.RuntimeConfigName, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, runtimeConfig.Status.AuxiliaryFiles)
	assert.Len(t, runtimeConfig.Status.AuxiliaryFiles.GeneralFiles, 2)
}

func TestPublishConfig_DoesNotTakeOverAnotherRuntimeConfigsAuxiliaryResource(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	first := basePublishRequest()
	first.TemplateConfigName = "first-config"
	first.TemplateConfigUID = types.UID("first-uid")
	first.AuxiliaryFiles = &AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{{
		Path: "/maps/host.map", Content: "first map",
	}}}
	firstResult, err := publisher.PublishConfig(ctx, &first)
	require.NoError(t, err)

	second := basePublishRequest()
	second.TemplateConfigName = "second-config"
	second.TemplateConfigUID = types.UID("second-uid")
	second.AuxiliaryFiles = &AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{{
		Path: "/maps/host.map", Content: "second map",
	}}}
	secondResult, err := publisher.PublishConfig(ctx, &second)
	require.NoError(t, err)
	require.Len(t, firstResult.MapFileNames, 1)
	require.Len(t, secondResult.MapFileNames, 1)
	assert.NotEqual(t, firstResult.MapFileNames[0], secondResult.MapFileNames[0])

	firstMap, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Get(ctx, firstResult.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	secondMap, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Get(ctx, secondResult.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "first map", firstMap.Spec.Entries)
	assert.Equal(t, firstResult.RuntimeConfigName, firstMap.OwnerReferences[0].Name)
	assert.Equal(t, "second map", secondMap.Spec.Entries)
	assert.Equal(t, secondResult.RuntimeConfigName, secondMap.OwnerReferences[0].Name)

	_, err = publisher.PublishConfig(ctx, &first)
	require.NoError(t, err)
	_, err = crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Get(ctx, secondResult.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
}

func TestPublishConfig_PrunesObsoleteAuxiliaryResources(t *testing.T) {
	ctx, k8sClient, crdClient, publisher := newTestPublisher(t)
	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{
		MapFiles:        []auxiliaryfiles.MapFile{{Path: "/maps/host.map", Content: "map"}},
		SSLCertificates: []auxiliaryfiles.SSLCertificate{{Path: "/certs/site.pem", Content: "cert"}},
		GeneralFiles: []auxiliaryfiles.GeneralFile{
			{Filename: "error.http", Path: "/files/error.http", Content: "HTTP"},
			{Filename: "error.lua", Path: "/files/error.lua", Content: "Lua"},
		},
		CRTListFiles: []auxiliaryfiles.CRTListFile{{Path: "/lists/site.list", Content: "list"}},
	}
	initial, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.Len(t, initial.GeneralFileNames, 2)

	_, err = k8sClient.CoreV1().Secrets("default").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	req.Checksum = "next"
	req.AuxiliaryFiles = &AuxiliaryFiles{GeneralFiles: []auxiliaryfiles.GeneralFile{
		{Filename: "error.http", Path: "/files/error.http", Content: "HTTP"},
	}}
	result, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.Len(t, result.GeneralFileNames, 1)

	mapFiles, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, mapFiles.Items)
	crtLists, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCRTListFiles("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, crtLists.Items)
	secrets, err := k8sClient.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, secrets.Items, 1)
	assert.Equal(t, "unrelated", secrets.Items[0].Name)
	generalFiles, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, generalFiles.Items, 1)
	assert.Equal(t, result.GeneralFileNames[0], generalFiles.Items[0].Name)

	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, result.RuntimeConfigName, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, runtimeConfig.Status.AuxiliaryFiles)
	assert.Empty(t, runtimeConfig.Status.AuxiliaryFiles.MapFiles)
	assert.Empty(t, runtimeConfig.Status.AuxiliaryFiles.SSLCertificates)
	assert.Empty(t, runtimeConfig.Status.AuxiliaryFiles.CRTListFiles)
	require.Len(t, runtimeConfig.Status.AuxiliaryFiles.GeneralFiles, 1)
	assert.Equal(t, result.GeneralFileNames[0], runtimeConfig.Status.AuxiliaryFiles.GeneralFiles[0].Name)
}

func TestPublishConfig_RetriesIncompleteAuxiliaryCleanup(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{{
		Path: "/maps/host.map", Content: "map",
	}}}
	initial, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.Len(t, initial.MapFileNames, 1)

	deleteFailures := 1
	crdClient.PrependReactor("delete", "haproxymapfiles", func(k8stesting.Action) (bool, runtime.Object, error) {
		if deleteFailures == 0 {
			return false, nil, nil
		}
		deleteFailures--
		return true, nil, apierrors.NewServiceUnavailable("map storage unavailable")
	})
	req.Checksum = "without-map"
	req.AuxiliaryFiles = &AuxiliaryFiles{}
	_, err = publisher.PublishConfig(ctx, &req)
	require.Error(t, err)
	var publicationErr *IncompletePublicationError
	require.ErrorAs(t, err, &publicationErr)
	assert.Equal(t, PublicationStageCleanup, publicationErr.Stage)
	assert.Equal(t, "HAProxyMapFile", publicationErr.ResourceKind)
	assert.Equal(t, initial.MapFileNames[0], publicationErr.ResourceName)
	assert.True(t, IsRetryablePublicationError(err))

	_, err = publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	_, err = crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Get(ctx, initial.MapFileNames[0], metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestPublishConfig_RepairsDesiredStateWithUnchangedChecksums(t *testing.T) {
	ctx, k8sClient, crdClient, publisher := newTestPublisher(t)
	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{
		MapFiles:        []auxiliaryfiles.MapFile{{Path: "/maps/host.map", Content: "map content"}},
		SSLCertificates: []auxiliaryfiles.SSLCertificate{{Path: "/certs/site.pem", Content: "cert content"}},
	}
	result, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, result.RuntimeConfigName, metav1.GetOptions{})
	require.NoError(t, err)
	runtimeConfig.Spec.Path = "/stale/haproxy.cfg"
	runtimeConfig.Labels = map[string]string{"stale": "true"}
	runtimeConfig.OwnerReferences[0].UID = types.UID("stale-template-uid")
	_, err = crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Update(ctx, runtimeConfig, metav1.UpdateOptions{})
	require.NoError(t, err)

	mapFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Get(ctx, result.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	mapFile.Spec.Path = "/stale/host.map"
	mapFile.Labels = map[string]string{"stale": "true"}
	mapFile.OwnerReferences[0].UID = types.UID("stale-runtime-uid")
	_, err = crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Update(ctx, mapFile, metav1.UpdateOptions{})
	require.NoError(t, err)

	secret, err := k8sClient.CoreV1().Secrets("default").
		Get(ctx, result.SecretNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	secret.Data["path"] = []byte("/stale/site.pem")
	secret.Labels = map[string]string{"stale": "true"}
	secret.OwnerReferences[0].UID = types.UID("stale-runtime-uid")
	_, err = k8sClient.CoreV1().Secrets("default").Update(ctx, secret, metav1.UpdateOptions{})
	require.NoError(t, err)

	_, err = publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	runtimeConfig, err = crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, result.RuntimeConfigName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, req.ConfigPath, runtimeConfig.Spec.Path)
	assert.Equal(t, req.TemplateConfigUID, runtimeConfig.OwnerReferences[0].UID)
	assert.Equal(t, req.TemplateConfigName, runtimeConfig.Labels["haproxy-haptic.org/template-config"])

	mapFile, err = crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		Get(ctx, result.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, req.AuxiliaryFiles.MapFiles[0].Path, mapFile.Spec.Path)
	assert.Equal(t, result.RuntimeConfigName, mapFile.Labels[runtimeConfigLabelKey])
	assert.NotEqual(t, types.UID("stale-runtime-uid"), mapFile.OwnerReferences[0].UID)

	secret, err = k8sClient.CoreV1().Secrets("default").
		Get(ctx, result.SecretNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte(req.AuxiliaryFiles.SSLCertificates[0].Path), secret.Data["path"])
	assert.Equal(t, result.RuntimeConfigName, secret.Labels[runtimeConfigLabelKey])
	assert.NotEqual(t, types.UID("stale-runtime-uid"), secret.OwnerReferences[0].UID)
}

func TestPublishConfig_Update(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create initial runtime config
	initialReq := basePublishRequest()

	_, err := publisher.PublishConfig(ctx, &initialReq)
	require.NoError(t, err)

	// Update with new config
	updatedReq := PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       types.UID("test-uid-123"),
		Config:                  "global\n  daemon\n  maxconn 1000\n",
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                "def456",
	}

	result, err := publisher.PublishConfig(ctx, &updatedReq)

	require.NoError(t, err)
	assert.NotNil(t, result)

	// Verify config was updated
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	assert.Equal(t, "global\n  daemon\n  maxconn 1000\n", runtimeConfig.Spec.Content)
	assert.Equal(t, "def456", runtimeConfig.Spec.Checksum)
}

func TestUpdateDeploymentStatus_AddPod(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create runtime config first
	req := basePublishRequest()

	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	update := DeploymentStatusUpdate{
		RuntimeConfigName:      "test-config-haproxycfg",
		RuntimeConfigNamespace: "default",
		PodName:                "haproxy-0",
		Checksum:               "abc123",
	}

	err = publisher.UpdateDeploymentStatus(ctx, &update)

	require.NoError(t, err)

	// Verify deployment status was updated
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	require.Len(t, runtimeConfig.Status.DeployedToPods, 1)
	assert.Equal(t, "haproxy-0", runtimeConfig.Status.DeployedToPods[0].PodName)
	assert.Equal(t, "abc123", runtimeConfig.Status.DeployedToPods[0].Checksum)
}

// TestUpdateDeploymentStatus_NotPublishedYetReturnsSentinel is the regression
// test for the startup race where the first deployment's per-pod status SSA
// lands before the initial HAProxyCfg publish. The old code swallowed the
// NotFound as success, permanently losing the pod's deployedToPods entry
// (observed as an e2e initial-sync timeout with 1/2 pods reported). The
// caller needs the sentinel to requeue the update.
func TestUpdateDeploymentStatus_NotPublishedYetReturnsSentinel(t *testing.T) {
	ctx := context.Background()
	k8sClient := k8sfake.NewClientset()
	crdClient := fake.NewSimpleClientset()

	publisher := NewWithListers(k8sClient, crdClient, nil, testLogger())

	// No PublishConfig first — the HAProxyCfg does not exist.
	update := DeploymentStatusUpdate{
		RuntimeConfigName:      "test-config-haproxycfg",
		RuntimeConfigNamespace: "default",
		PodName:                "haproxy-0",
		Checksum:               "abc123",
	}

	err := publisher.UpdateDeploymentStatus(ctx, &update)

	require.ErrorIs(t, err, ErrRuntimeConfigNotPublished)
}

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

// TestUpdateDeploymentStatus_UncommittedReferencesReturnSentinel: a publication
// writes the HAProxyCfg before its auxiliary references. A status update in
// between must be retried, not stamp no files or the previous set's.
func TestUpdateDeploymentStatus_UncommittedReferencesReturnSentinel(t *testing.T) {
	for name, republish := range map[string]bool{"first publication": false, "changed auxiliary set": true} {
		t.Run(name, func(t *testing.T) {
			ctx, _, crdClient, publisher := newTestPublisher(t)
			if republish {
				previous := auxPublishRequest()
				_, err := publisher.PublishConfig(ctx, &previous)
				require.NoError(t, err)
			}
			req := auxPublishRequest()
			req.AuxiliaryFiles.MapFiles[0].Content = "example.com be_b\n"
			release := holdAuxiliaryReferences(crdClient)
			_, err := publisher.PublishConfig(ctx, &req)
			require.Error(t, err)

			update := statusUpdate("haproxy-0", "uid-0", "runtime-0", req.Checksum)
			require.ErrorIs(t, publisher.UpdateDeploymentStatus(ctx, update), ErrRuntimeConfigNotPublished)

			release()
			_, err = publisher.PublishConfig(ctx, &req)
			require.NoError(t, err)
			require.NoError(t, publisher.UpdateDeploymentStatus(ctx, update))
			objects := publishedAuxObjects(t, ctx, crdClient)
			for _, resource := range []string{resHAProxyMapFiles, resHAProxyGeneralFiles, resHAProxyCRTListFiles} {
				assertEntryRestored(t, ctx, crdClient, resource, objects)
			}
		})
	}
}

func podsOn(t *testing.T, ctx context.Context, c *fake.Clientset, resource string, objects auxObjectNames) []string {
	t.Helper()
	pods, ok := podStatusesOf(statusObject(t, ctx, c, resource, objects))
	require.True(t, ok)
	names := make([]string, 0, len(pods))
	for i := range pods {
		names = append(names, pods[i].PodName)
	}
	return names
}

// TestUpdateDeploymentStatus_StampsOnlyThePublicationThePodRuns is issue #384:
// an auxiliary file's entry says the pod runs that file, so a pod is stamped
// only on the files of the publication at its own checksum.
func TestUpdateDeploymentStatus_StampsOnlyThePublicationThePodRuns(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	previous := auxPublishRequest()
	_, err := publisher.PublishConfig(ctx, &previous)
	require.NoError(t, err)
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, statusUpdate("haproxy-0", "uid-0", "runtime-0", previous.Checksum)))
	previousFiles := publishedAuxObjects(t, ctx, crdClient)

	next := auxPublishRequest()
	next.Checksum = "def456"
	next.AuxiliaryFiles.MapFiles[0].Content = "example.com be_b\n"

	// haproxy-1 joins on next before next's publication starts.
	ahead := statusUpdate("haproxy-1", "uid-1", "runtime-1", next.Checksum)
	assert.ErrorIs(t, publisher.UpdateDeploymentStatus(ctx, ahead), ErrRuntimeConfigNotPublished,
		"the update must be retried once next is published")
	assert.NotContains(t, podsOn(t, ctx, crdClient, resHAProxyMapFiles, previousFiles), "haproxy-1",
		"haproxy-1 never ran the previous map file")
	cfg, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, cfg.Status.DeployedToPods, haproxyv1alpha1.PodDeploymentStatus{
		PodName: "haproxy-1", PodUID: "uid-1", PodRuntimeID: "runtime-1", Checksum: next.Checksum,
	}, "the HAProxyCfg entry, which carries its own checksum, is not delayed")

	_, err = publisher.PublishConfig(ctx, &next)
	require.NoError(t, err)
	nextFiles := publishedAuxObjects(t, ctx, crdClient)
	require.NotEqual(t, previousFiles.mapFile, nextFiles.mapFile)
	require.NoError(t, publisher.UpdateDeploymentStatus(ctx, ahead))
	for _, resource := range []string{resHAProxyMapFiles, resHAProxyGeneralFiles, resHAProxyCRTListFiles} {
		assert.Contains(t, podsOn(t, ctx, crdClient, resource, nextFiles), "haproxy-1", "%s of next", resource)
	}

	// haproxy-0 still runs previous: next's map file must not name it, while
	// the files both sets share keep its entry.
	behind := driftStatusUpdate("haproxy-0", "uid-0", "runtime-0", previous.Checksum)
	assert.ErrorIs(t, publisher.UpdateDeploymentStatus(ctx, behind), ErrRuntimeConfigNotPublished)
	assert.NotContains(t, podsOn(t, ctx, crdClient, resHAProxyMapFiles, nextFiles), "haproxy-0",
		"haproxy-0 never ran next's map file")
	assert.Contains(t, podsOn(t, ctx, crdClient, resHAProxyGeneralFiles, nextFiles), "haproxy-0")
}

func TestUpdateDeploymentStatus_UpdateExistingPod(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create runtime config
	req := basePublishRequest()

	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	// Add pod first time
	firstUpdate := DeploymentStatusUpdate{
		RuntimeConfigName:      "test-config-haproxycfg",
		RuntimeConfigNamespace: "default",
		PodName:                "haproxy-0",
		Checksum:               "abc123",
	}

	err = publisher.UpdateDeploymentStatus(ctx, &firstUpdate)
	require.NoError(t, err)

	// Update the same pod with a FAILED sync carrying a *different* checksum.
	// The pod did not actually receive "def456", so the recorded checksum must
	// NOT advance to it — otherwise deployedToPods[].checksum would equal
	// spec.checksum and falsely read as converged. The last successfully-deployed
	// checksum ("abc123") is preserved; only LastError is updated.
	time.Sleep(10 * time.Millisecond) // Ensure different timestamp
	secondUpdate := DeploymentStatusUpdate{
		RuntimeConfigName:      "test-config-haproxycfg",
		RuntimeConfigNamespace: "default",
		PodName:                "haproxy-0",
		Checksum:               "def456",
		Error:                  "sync failed",
	}

	err = publisher.UpdateDeploymentStatus(ctx, &secondUpdate)
	require.NoError(t, err)

	// Verify only one pod entry exists, with the LAST-SUCCESSFUL checksum
	// preserved and the error recorded.
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	require.Len(t, runtimeConfig.Status.DeployedToPods, 1)
	assert.Equal(t, "haproxy-0", runtimeConfig.Status.DeployedToPods[0].PodName)
	assert.Equal(t, "abc123", runtimeConfig.Status.DeployedToPods[0].Checksum,
		"a failed sync must preserve the last successfully-deployed checksum, not advance to the failed one")
	assert.Equal(t, "sync failed", runtimeConfig.Status.DeployedToPods[0].LastError)
}

// TestUpdateDeploymentStatus_FirstDeployFailureRecordsNoChecksum pins that when
// a pod's very first deploy fails (no prior success to preserve), no checksum is
// recorded at all — so the pod correctly reads as never-converged rather than
// being stamped with the checksum it failed to receive.
func TestUpdateDeploymentStatus_FirstDeployFailureRecordsNoChecksum(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	_, err := publisher.PublishConfig(ctx, &PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       types.UID("test-uid-123"),
		Config:                  "global\n  daemon\n",
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                "v1",
	})
	require.NoError(t, err)

	// Pod's first-ever deploy fails — it never received "v1".
	err = publisher.UpdateDeploymentStatus(ctx, &DeploymentStatusUpdate{
		RuntimeConfigName:      "test-config-haproxycfg",
		RuntimeConfigNamespace: "default",
		PodName:                "haproxy-0",
		Checksum:               "v1",
		Error:                  "connection refused",
	})
	require.NoError(t, err)

	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, runtimeConfig.Status.DeployedToPods, 1)
	assert.Equal(t, "haproxy-0", runtimeConfig.Status.DeployedToPods[0].PodName)
	assert.Empty(t, runtimeConfig.Status.DeployedToPods[0].Checksum,
		"a first-deploy failure must record no checksum (the pod never converged), not the failed one")
	assert.Equal(t, "connection refused", runtimeConfig.Status.DeployedToPods[0].LastError)
}

func TestUpdateDeploymentStatus_ReplacementFailureClearsPredecessorProof(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	req := basePublishRequest()
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	err = publisher.UpdateDeploymentStatus(ctx, &DeploymentStatusUpdate{
		RuntimeConfigName: "test-config-haproxycfg", RuntimeConfigNamespace: "default",
		PodName: "haproxy-0", PodUID: "uid-old", Checksum: "abc123",
	})
	require.NoError(t, err)
	err = publisher.UpdateDeploymentStatus(ctx, &DeploymentStatusUpdate{
		RuntimeConfigName: "test-config-haproxycfg", RuntimeConfigNamespace: "default",
		PodName: "haproxy-0", PodUID: "uid-new", Checksum: "abc123", Error: "connection refused",
	})
	require.NoError(t, err)

	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, runtimeConfig.Status.DeployedToPods, 1)
	status := runtimeConfig.Status.DeployedToPods[0]
	assert.Equal(t, "uid-new", status.PodUID)
	assert.Empty(t, status.Checksum)
	assert.Equal(t, "connection refused", status.LastError)
}

func TestUpdateDeploymentStatus_RuntimeReplacementFailureClearsPredecessorProof(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	req := basePublishRequest()
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	err = publisher.UpdateDeploymentStatus(ctx, &DeploymentStatusUpdate{
		RuntimeConfigName: "test-config-haproxycfg", RuntimeConfigNamespace: "default",
		PodName: "haproxy-0", PodUID: "uid-same", PodRuntimeID: "runtime-old", Checksum: "abc123",
	})
	require.NoError(t, err)
	err = publisher.UpdateDeploymentStatus(ctx, &DeploymentStatusUpdate{
		RuntimeConfigName: "test-config-haproxycfg", RuntimeConfigNamespace: "default",
		PodName: "haproxy-0", PodUID: "uid-same", PodRuntimeID: "runtime-new",
		Checksum: "abc123", Error: "connection refused",
	})
	require.NoError(t, err)

	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, runtimeConfig.Status.DeployedToPods, 1)
	status := runtimeConfig.Status.DeployedToPods[0]
	assert.Equal(t, "uid-same", status.PodUID)
	assert.Equal(t, "runtime-new", status.PodRuntimeID)
	assert.Empty(t, status.Checksum)
	assert.Equal(t, "connection refused", status.LastError)
}

func TestUpdateDeploymentStatus_MultiplePods(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create runtime config
	req := basePublishRequest()

	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	// Add multiple pods
	pods := []string{"haproxy-0", "haproxy-1", "haproxy-2"}
	for _, podName := range pods {
		update := DeploymentStatusUpdate{
			RuntimeConfigName:      "test-config-haproxycfg",
			RuntimeConfigNamespace: "default",
			PodName:                podName,
			Checksum:               "abc123",
		}

		err = publisher.UpdateDeploymentStatus(ctx, &update)
		require.NoError(t, err)
	}

	// Verify all pods were added
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	require.Len(t, runtimeConfig.Status.DeployedToPods, 3)

	podNames := make([]string, 3)
	for i, pod := range runtimeConfig.Status.DeployedToPods {
		podNames[i] = pod.PodName
	}

	assert.Contains(t, podNames, "haproxy-0")
	assert.Contains(t, podNames, "haproxy-1")
	assert.Contains(t, podNames, "haproxy-2")
}

func TestCleanupPodReferences_RemovePod(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create runtime config
	req := basePublishRequest()

	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	// Add two pods
	for _, podName := range []string{"haproxy-0", "haproxy-1"} {
		update := DeploymentStatusUpdate{
			RuntimeConfigName:      "test-config-haproxycfg",
			RuntimeConfigNamespace: "default",
			PodName:                podName,
			Checksum:               "abc123",
		}

		err = publisher.UpdateDeploymentStatus(ctx, &update)
		require.NoError(t, err)
	}

	// Remove one pod (using namespace-scoped cleanup)
	cleanup := PodCleanupRequest{
		PodName:   "haproxy-0",
		Namespace: "default",
	}

	err = publisher.CleanupPodReferences(ctx, &cleanup)
	require.NoError(t, err)

	// Verify only one pod remains
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	require.Len(t, runtimeConfig.Status.DeployedToPods, 1)
	assert.Equal(t, "haproxy-1", runtimeConfig.Status.DeployedToPods[0].PodName)
}

func TestCleanupPodReferences_NonexistentPod(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create runtime config
	req := basePublishRequest()

	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	// Try to cleanup pod that was never added (using namespace-scoped cleanup)
	cleanup := PodCleanupRequest{
		PodName:   "nonexistent-pod",
		Namespace: "default",
	}

	err = publisher.CleanupPodReferences(ctx, &cleanup)

	require.NoError(t, err)

	// Verify runtime config status unchanged
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	assert.Len(t, runtimeConfig.Status.DeployedToPods, 0)
}

func TestPublishConfig_GeneralFiles(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{
		GeneralFiles: []auxiliaryfiles.GeneralFile{
			{
				Filename: "503.http",
				Path:     "/etc/haproxy/general/503.http",
				Content:  "HTTP/1.0 503 Service Unavailable\r\nContent-Type: text/plain\r\n\r\nService Unavailable",
			},
		},
	}

	result, err := publisher.PublishConfig(ctx, &req)

	require.NoError(t, err)
	assert.NotNil(t, result)
	require.Len(t, result.GeneralFileNames, 1)

	// Verify HAProxyGeneralFile was created
	generalFiles, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyGeneralFiles("default").
		List(ctx, metav1.ListOptions{})

	require.NoError(t, err)
	require.Len(t, generalFiles.Items, 1)
	assert.Equal(t, "503.http", generalFiles.Items[0].Spec.FileName)
	assert.Equal(t, "/etc/haproxy/general/503.http", generalFiles.Items[0].Spec.Path)
	assert.Equal(t, "HTTP/1.0 503 Service Unavailable\r\nContent-Type: text/plain\r\n\r\nService Unavailable",
		generalFiles.Items[0].Spec.Content)
}

func TestPublishConfig_CRTListFiles(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{
		CRTListFiles: []auxiliaryfiles.CRTListFile{
			{
				Path: "/etc/haproxy/ssl/crt-list.txt",
				Content: `/etc/haproxy/ssl/example.pem [verify none alpn h2,http/1.1] example.com
/etc/haproxy/ssl/wildcard.pem *.example.com`,
			},
		},
	}

	result, err := publisher.PublishConfig(ctx, &req)

	require.NoError(t, err)
	assert.NotNil(t, result)
	require.Len(t, result.CRTListFileNames, 1)

	// Verify HAProxyCRTListFile was created
	crtListFiles, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCRTListFiles("default").
		List(ctx, metav1.ListOptions{})

	require.NoError(t, err)
	require.Len(t, crtListFiles.Items, 1)
	assert.Equal(t, "/etc/haproxy/ssl/crt-list.txt", crtListFiles.Items[0].Spec.Path)
	assert.Contains(t, crtListFiles.Items[0].Spec.Entries, "example.com")
	assert.Contains(t, crtListFiles.Items[0].Spec.Entries, "wildcard.pem")
}

func TestPublishConfig_WithCompression(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create large content that will benefit from compression
	// Repeating patterns compress well
	var largeContent strings.Builder
	for i := range 1000 {
		largeContent.WriteString("backend app_backend_" + string(rune('a'+i%26)) + "\n")
		largeContent.WriteString("  server server1 10.0.0.1:8080 check\n")
		largeContent.WriteString("  server server2 10.0.0.2:8080 check\n")
		largeContent.WriteString("\n")
	}

	req := PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       types.UID("test-uid-123"),
		Config:                  largeContent.String(),
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                "abc123",
		CompressionThreshold:    1024, // 1KB threshold
	}

	result, err := publisher.PublishConfig(ctx, &req)

	require.NoError(t, err)
	assert.NotNil(t, result)

	// Verify HAProxyCfg was created with compression flag
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	// Content should be compressed (shorter than original)
	assert.True(t, runtimeConfig.Spec.Compressed, "Large config should be compressed")
	assert.Less(t, len(runtimeConfig.Spec.Content), len(largeContent.String()),
		"Compressed content should be smaller than original")
}

func TestPublishConfig_CompressionDisabled(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create large content
	var largeContent strings.Builder
	for i := range 1000 {
		largeContent.WriteString("backend app_backend_" + string(rune('a'+i%26)) + "\n")
		largeContent.WriteString("  server server1 10.0.0.1:8080 check\n")
	}

	req := PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       types.UID("test-uid-123"),
		Config:                  largeContent.String(),
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                "abc123",
		CompressionThreshold:    0, // Disabled
	}

	result, err := publisher.PublishConfig(ctx, &req)

	require.NoError(t, err)
	assert.NotNil(t, result)

	// Verify HAProxyCfg was created without compression
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	assert.False(t, runtimeConfig.Spec.Compressed, "Compression should be disabled")
	assert.Equal(t, largeContent.String(), runtimeConfig.Spec.Content, "Content should be unchanged")
}

func TestPublishConfig_SSLSecretCompressionAnnotation(t *testing.T) {
	ctx, k8sClient, _, publisher := newTestPublisher(t)

	req := PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       types.UID("test-uid-123"),
		Config:                  "global\n  daemon\n",
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                "abc123",
		CompressionThreshold:    0, // Disabled - small content won't be compressed anyway
		AuxiliaryFiles: &AuxiliaryFiles{
			SSLCertificates: []auxiliaryfiles.SSLCertificate{
				{
					Path:    "/etc/haproxy/ssl/cert.pem",
					Content: "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n",
				},
			},
		},
	}

	result, err := publisher.PublishConfig(ctx, &req)

	require.NoError(t, err)
	assert.NotNil(t, result)
	require.Len(t, result.SecretNames, 1)

	// Verify SSL secret uses annotation for compression flag
	secrets, err := k8sClient.CoreV1().
		Secrets("default").
		List(ctx, metav1.ListOptions{})

	require.NoError(t, err)
	require.Len(t, secrets.Items, 1)

	// Check annotation exists
	compressedAnnotation, ok := secrets.Items[0].Annotations["haproxy-haptic.org/compressed"]
	assert.True(t, ok, "compressed annotation should exist")
	assert.Equal(t, "false", compressedAnnotation, "compressed should be false for small content")

	// Verify 'compressed' is NOT in Data
	_, hasCompressedData := secrets.Items[0].Data["compressed"]
	assert.False(t, hasCompressedData, "compressed should NOT be in Secret data")
}

func TestReconcileDeployedToPods_RemovesStalePods(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create runtime config
	req := basePublishRequest()

	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	// Add three pods
	for _, podName := range []string{"haproxy-0", "haproxy-1", "haproxy-2"} {
		update := DeploymentStatusUpdate{
			RuntimeConfigName:      "test-config-haproxycfg",
			RuntimeConfigNamespace: "default",
			PodName:                podName,
			Checksum:               "abc123",
		}

		err = publisher.UpdateDeploymentStatus(ctx, &update)
		require.NoError(t, err)
	}

	// Reconcile with only haproxy-1 as running (namespace-scoped)
	runningPods := []PodIdentity{{PodName: "haproxy-1"}}
	err = publisher.ReconcileDeployedToPods(ctx, "default", runningPods)
	require.NoError(t, err)

	// Verify only haproxy-1 remains
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	require.Len(t, runtimeConfig.Status.DeployedToPods, 1)
	assert.Equal(t, "haproxy-1", runtimeConfig.Status.DeployedToPods[0].PodName)
}

func TestPodCleanupAndReconcileUsePodUID(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	req := basePublishRequest()
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	err = publisher.UpdateDeploymentStatus(ctx, &DeploymentStatusUpdate{
		RuntimeConfigName: "test-config-haproxycfg", RuntimeConfigNamespace: "default",
		PodName: "haproxy-0", PodUID: "uid-old", Checksum: "abc123",
	})
	require.NoError(t, err)
	err = publisher.ReconcileDeployedToPods(ctx, "default", []PodIdentity{{PodName: "haproxy-0", PodUID: "uid-new"}})
	require.NoError(t, err)
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, runtimeConfig.Status.DeployedToPods)

	err = publisher.UpdateDeploymentStatus(ctx, &DeploymentStatusUpdate{
		RuntimeConfigName: "test-config-haproxycfg", RuntimeConfigNamespace: "default",
		PodName: "haproxy-0", PodUID: "uid-new", Checksum: "abc123",
	})
	require.NoError(t, err)
	err = publisher.CleanupPodReferences(ctx, &PodCleanupRequest{PodName: "haproxy-0", PodUID: "uid-old", Namespace: "default"})
	require.NoError(t, err)
	runtimeConfig, err = crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, runtimeConfig.Status.DeployedToPods, 1)
	assert.Equal(t, "uid-new", runtimeConfig.Status.DeployedToPods[0].PodUID)
}

func TestReconcileDeployedToPods_NoRunningPods(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create runtime config
	req := basePublishRequest()

	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	// Add two pods
	for _, podName := range []string{"haproxy-0", "haproxy-1"} {
		update := DeploymentStatusUpdate{
			RuntimeConfigName:      "test-config-haproxycfg",
			RuntimeConfigNamespace: "default",
			PodName:                podName,
			Checksum:               "abc123",
		}

		err = publisher.UpdateDeploymentStatus(ctx, &update)
		require.NoError(t, err)
	}

	// Reconcile with no running pods (namespace-scoped)
	runningPods := []PodIdentity{}
	err = publisher.ReconcileDeployedToPods(ctx, "default", runningPods)
	require.NoError(t, err)

	// Verify all pods are removed
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	assert.Empty(t, runtimeConfig.Status.DeployedToPods)
}

func TestReconcileDeployedToPods_NoStalePods(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create runtime config
	req := basePublishRequest()

	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	// Add two pods
	for _, podName := range []string{"haproxy-0", "haproxy-1"} {
		update := DeploymentStatusUpdate{
			RuntimeConfigName:      "test-config-haproxycfg",
			RuntimeConfigNamespace: "default",
			PodName:                podName,
			Checksum:               "abc123",
		}

		err = publisher.UpdateDeploymentStatus(ctx, &update)
		require.NoError(t, err)
	}

	// Reconcile with all pods running (namespace-scoped)
	runningPods := []PodIdentity{{PodName: "haproxy-0"}, {PodName: "haproxy-1"}}
	err = publisher.ReconcileDeployedToPods(ctx, "default", runningPods)
	require.NoError(t, err)

	// Verify both pods remain
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	require.Len(t, runtimeConfig.Status.DeployedToPods, 2)
}

func TestReconcileDeployedToPods_EmptyStatus(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	// Create runtime config without adding any pods
	req := basePublishRequest()

	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	// Reconcile with some running pods (namespace-scoped)
	runningPods := []PodIdentity{{PodName: "haproxy-0"}, {PodName: "haproxy-1"}}
	err = publisher.ReconcileDeployedToPods(ctx, "default", runningPods)
	require.NoError(t, err)

	// Should not error - no-op
	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})

	require.NoError(t, err)
	assert.Empty(t, runtimeConfig.Status.DeployedToPods)
}

// TestUpdateDeploymentStatus_AuxiliaryFilesUseOwnChecksum verifies that auxiliary files
// (MapFiles, GeneralFiles, CRTListFiles) use their own spec.checksum instead of the main
// config checksum. This prevents unnecessary status updates when only the main config
// changes but auxiliary file content remains the same.
func TestUpdateDeploymentStatus_AuxiliaryFilesUseOwnChecksum(t *testing.T) {
	ctx := context.Background()
	k8sClient := k8sfake.NewClientset()
	crdClient := fake.NewSimpleClientset()
	installSSAListMapMergeReactor(crdClient)
	publisher := NewWithListers(k8sClient, crdClient, nil, testLogger())

	// Create runtime config with auxiliary files
	request := PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       types.UID("test-uid-123"),
		Config:                  "global\n  daemon\n",
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                "main-config-checksum-v1",
		AuxiliaryFiles: &AuxiliaryFiles{
			MapFiles:     []auxiliaryfiles.MapFile{{Path: "/etc/haproxy/maps/host.map", Content: "example.com backend1\n"}},
			GeneralFiles: []auxiliaryfiles.GeneralFile{{Path: "/etc/haproxy/lua/script.lua", Content: "-- lua script\n"}},
			CRTListFiles: []auxiliaryfiles.CRTListFile{{Path: "/etc/haproxy/ssl/crt-list.txt", Content: "default.pem\n"}},
		},
	}
	result, err := publisher.PublishConfig(ctx, &request)
	require.NoError(t, err)
	require.Len(t, result.MapFileNames, 1)
	require.Len(t, result.GeneralFileNames, 1)
	require.Len(t, result.CRTListFileNames, 1)

	// Get the checksums stored in the auxiliary file specs
	mapFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, result.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	mapFileChecksum := mapFile.Spec.Checksum

	generalFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles("default").Get(ctx, result.GeneralFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	generalFileChecksum := generalFile.Spec.Checksum

	crtListFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCRTListFiles("default").Get(ctx, result.CRTListFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	crtListFileChecksum := crtListFile.Spec.Checksum

	// Verify the checksums are different from the main config checksum
	assert.NotEqual(t, "main-config-checksum-v1", mapFileChecksum)
	assert.NotEqual(t, "main-config-checksum-v1", generalFileChecksum)
	assert.NotEqual(t, "main-config-checksum-v1", crtListFileChecksum)

	t.Run("initial deployment uses file-specific checksums", func(t *testing.T) {
		err := publisher.UpdateDeploymentStatus(ctx, &DeploymentStatusUpdate{
			RuntimeConfigName: "test-config-haproxycfg", RuntimeConfigNamespace: "default",
			PodName: "haproxy-0", Checksum: "main-config-checksum-v1",
		})
		require.NoError(t, err)

		// Verify main config status uses the main config checksum
		runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
		require.NoError(t, err)
		require.Len(t, runtimeConfig.Status.DeployedToPods, 1)
		assert.Equal(t, "main-config-checksum-v1", runtimeConfig.Status.DeployedToPods[0].Checksum)

		// Verify auxiliary files use their own checksums
		mapFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, result.MapFileNames[0], metav1.GetOptions{})
		require.NoError(t, err)
		require.Len(t, mapFile.Status.DeployedToPods, 1)
		assert.Equal(t, mapFileChecksum, mapFile.Status.DeployedToPods[0].Checksum, "map file should use its own checksum")

		generalFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles("default").Get(ctx, result.GeneralFileNames[0], metav1.GetOptions{})
		require.NoError(t, err)
		require.Len(t, generalFile.Status.DeployedToPods, 1)
		assert.Equal(t, generalFileChecksum, generalFile.Status.DeployedToPods[0].Checksum, "general file should use its own checksum")

		crtListFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCRTListFiles("default").Get(ctx, result.CRTListFileNames[0], metav1.GetOptions{})
		require.NoError(t, err)
		require.Len(t, crtListFile.Status.DeployedToPods, 1)
		assert.Equal(t, crtListFileChecksum, crtListFile.Status.DeployedToPods[0].Checksum, "crt-list file should use its own checksum")
	})

	t.Run("main config change updates main config checksum but not auxiliary file checksums", func(t *testing.T) {
		// Simulate main config change (new checksum). Checksum is a state field
		// so it triggers an UpdateStatus on the main config.
		changed := request
		changed.Config = "global\n  daemon\n  maxconn 10\n"
		changed.Checksum = "main-config-checksum-v2"
		_, err := publisher.PublishConfig(ctx, &changed)
		require.NoError(t, err)
		err = publisher.UpdateDeploymentStatus(ctx, &DeploymentStatusUpdate{
			RuntimeConfigName: "test-config-haproxycfg", RuntimeConfigNamespace: "default",
			PodName: "haproxy-0", Checksum: "main-config-checksum-v2",
		})
		require.NoError(t, err)

		// Main config status checksum IS updated (checksum is a state field)
		runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, "main-config-checksum-v2", runtimeConfig.Status.DeployedToPods[0].Checksum)

		// Auxiliary files should still have their original checksums
		mapFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, result.MapFileNames[0], metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, mapFileChecksum, mapFile.Status.DeployedToPods[0].Checksum, "map file checksum should remain unchanged")

		generalFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles("default").Get(ctx, result.GeneralFileNames[0], metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, generalFileChecksum, generalFile.Status.DeployedToPods[0].Checksum, "general file checksum should remain unchanged")

		crtListFile, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCRTListFiles("default").Get(ctx, result.CRTListFileNames[0], metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, crtListFileChecksum, crtListFile.Status.DeployedToPods[0].Checksum, "crt-list file checksum should remain unchanged")
	})
}

// TestAuxiliaryFileSpec_NoUpdateWhenChecksumUnchanged verifies that auxiliary file
// specs are not updated when the content checksum hasn't changed.
func TestAuxiliaryFileSpec_NoUpdateWhenChecksumUnchanged(t *testing.T) {
	ctx := context.Background()
	k8sClient := k8sfake.NewClientset()
	crdClient := fake.NewSimpleClientset()
	installSSAListMapMergeReactor(crdClient)
	publisher := NewWithListers(k8sClient, crdClient, nil, testLogger())

	// Create runtime config with auxiliary files
	result, err := publisher.PublishConfig(ctx, &PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       types.UID("test-uid-123"),
		Config:                  "global\n  daemon\n",
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                "main-config-checksum-v1",
		AuxiliaryFiles: &AuxiliaryFiles{
			MapFiles:     []auxiliaryfiles.MapFile{{Path: "/etc/haproxy/maps/host.map", Content: "example.com backend1\n"}},
			GeneralFiles: []auxiliaryfiles.GeneralFile{{Path: "/etc/haproxy/lua/script.lua", Filename: "script.lua", Content: "-- lua script\n"}},
			CRTListFiles: []auxiliaryfiles.CRTListFile{{Path: "/etc/haproxy/ssl/crt-list.txt", Content: "default.pem\n"}},
		},
	})
	require.NoError(t, err)
	require.Len(t, result.MapFileNames, 1)
	require.Len(t, result.GeneralFileNames, 1)
	require.Len(t, result.CRTListFileNames, 1)

	// Get initial resource versions
	mapFile1, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, result.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	mapFileRV1 := mapFile1.ResourceVersion

	generalFile1, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles("default").Get(ctx, result.GeneralFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	generalFileRV1 := generalFile1.ResourceVersion

	crtListFile1, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCRTListFiles("default").Get(ctx, result.CRTListFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	crtListFileRV1 := crtListFile1.ResourceVersion

	// Publish SAME config again (same auxiliary file content)
	result2, err := publisher.PublishConfig(ctx, &PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       types.UID("test-uid-123"),
		Config:                  "global\n  daemon\n",
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                "main-config-checksum-v1",
		AuxiliaryFiles: &AuxiliaryFiles{
			MapFiles:     []auxiliaryfiles.MapFile{{Path: "/etc/haproxy/maps/host.map", Content: "example.com backend1\n"}},
			GeneralFiles: []auxiliaryfiles.GeneralFile{{Path: "/etc/haproxy/lua/script.lua", Filename: "script.lua", Content: "-- lua script\n"}},
			CRTListFiles: []auxiliaryfiles.CRTListFile{{Path: "/etc/haproxy/ssl/crt-list.txt", Content: "default.pem\n"}},
		},
	})
	require.NoError(t, err)

	// Verify resource versions didn't change (no update occurred)
	mapFile2, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, result2.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, mapFileRV1, mapFile2.ResourceVersion,
		"map file resource version should not change when checksum is unchanged")

	generalFile2, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles("default").Get(ctx, result2.GeneralFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, generalFileRV1, generalFile2.ResourceVersion,
		"general file resource version should not change when checksum is unchanged")

	crtListFile2, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCRTListFiles("default").Get(ctx, result2.CRTListFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, crtListFileRV1, crtListFile2.ResourceVersion,
		"crt-list file resource version should not change when checksum is unchanged")
}

// TestAuxiliaryFileSpec_UpdateWhenChecksumChanges verifies changed content is
// published under a new immutable auxiliary-set name.
func TestAuxiliaryFileSpec_UpdateWhenChecksumChanges(t *testing.T) {
	ctx := context.Background()
	k8sClient := k8sfake.NewClientset()
	crdClient := fake.NewSimpleClientset()
	installSSAListMapMergeReactor(crdClient)
	publisher := NewWithListers(k8sClient, crdClient, nil, testLogger())

	// Create runtime config with auxiliary files
	result, err := publisher.PublishConfig(ctx, &PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       types.UID("test-uid-123"),
		Config:                  "global\n  daemon\n",
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                "main-config-checksum-v1",
		AuxiliaryFiles: &AuxiliaryFiles{
			MapFiles: []auxiliaryfiles.MapFile{{Path: "/etc/haproxy/maps/host.map", Content: "example.com backend1\n"}},
		},
	})
	require.NoError(t, err)
	require.Len(t, result.MapFileNames, 1)
	initialName := result.MapFileNames[0]

	// Get initial checksum
	mapFile1, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, initialName, metav1.GetOptions{})
	require.NoError(t, err)
	initialChecksum := mapFile1.Spec.Checksum

	// Publish with DIFFERENT auxiliary file content
	result2, err := publisher.PublishConfig(ctx, &PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       types.UID("test-uid-123"),
		Config:                  "global\n  daemon\n",
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                "main-config-checksum-v1",
		AuxiliaryFiles: &AuxiliaryFiles{
			MapFiles: []auxiliaryfiles.MapFile{{Path: "/etc/haproxy/maps/host.map", Content: "example.com backend2\n"}}, // Different content
		},
	})
	require.NoError(t, err)
	require.Len(t, result2.MapFileNames, 1)
	assert.NotEqual(t, initialName, result2.MapFileNames[0])

	// Verify the committed resource carries the changed content.
	mapFile2, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").Get(ctx, result2.MapFileNames[0], metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotEqual(t, initialChecksum, mapFile2.Spec.Checksum,
		"map file checksum should change when content changes")
	assert.Equal(t, "example.com backend2\n", mapFile2.Spec.Entries,
		"map file entries should be updated with new content")
}
