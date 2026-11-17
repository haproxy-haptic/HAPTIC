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

package configpublisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	v1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/compression"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
)

// The key is low-entropy on purpose: it is what an unkeyed digest would leak.
const testSensitiveMapContent = "route-a hunter2\n"

var testSensitiveMap = auxiliaryfiles.SensitiveMapFile{
	Path: "maps/keys.map", Content: testSensitiveMapContent,
	Seal: auxiliaryfiles.SealContent([]byte("installation key"), testSensitiveMapContent),
}

func sensitiveMapPublishRequest(t *testing.T, sensitive auxiliaryfiles.SensitiveMapFile) PublishRequest {
	t.Helper()
	const config = "global\n"
	const routes = "example.test backend\n"
	files := &dataplane.AuxiliaryFiles{
		MapFiles:          []auxiliaryfiles.MapFile{{Path: "maps/routes.map", Content: routes}},
		SensitiveMapFiles: []auxiliaryfiles.SensitiveMapFile{sensitive},
	}
	artifactAuthority := renderartifact.NewAuthority()
	artifacts, err := dataplane.BuildAuxiliaryFileSnapshot(artifactAuthority, nil, files)
	require.NoError(t, err)
	sealed := exactPublisherPlanFile(sensitive.Path, renderplan.FileKindMap, false, sensitive.Content)
	sealed.Seal = sensitive.Seal
	plan := &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		Sections: []renderplan.Section{{
			Kind: renderplan.SectionKindCore, Name: "core#0", Text: config,
			TextKnown: true, TextDigest: renderplan.DigestString(config), Length: len(config),
		}},
		Maps: map[string]renderplan.Map{"maps/routes.map": {
			Path: "maps/routes.map", Ordered: true, Entries: renderplan.ParseMapEntries(routes),
		}},
		Files: []renderplan.File{
			exactPublisherPlanFile(renderplan.ConfigFilePath, renderplan.FileKindConfig, true, config),
			exactPublisherPlanFile("maps/routes.map", renderplan.FileKindMap, false, routes),
			sealed,
		},
	}
	plan.ComputeID()
	outputAuthority, err := renderoutput.NewAuthority(renderplan.NewAuthority(), artifactAuthority)
	require.NoError(t, err)
	snapshot, err := renderoutput.NewSnapshot(outputAuthority, config, plan, artifacts, nil)
	require.NoError(t, err)
	checksum, err := snapshot.ContentChecksum()
	require.NoError(t, err)
	req := basePublishRequest()
	req.OutputSnapshot = snapshot
	req.ConfirmedPod = &v1.PodDeploymentStatus{
		PodName: "haproxy-0", PodUID: "pod-uid", PodRuntimeID: "container-id", Checksum: checksum,
		AppliedPlanID: plan.ID, RunningPlanID: plan.ID, WorkerOpsPlanID: plan.ID, Mode: "reload",
	}
	return req
}

func sensitiveMapSecrets(ctx context.Context, t *testing.T, kube *k8sfake.Clientset) []corev1.Secret {
	t.Helper()
	secrets, err := kube.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	var found []corev1.Secret
	for i := range secrets.Items {
		if secrets.Items[i].Labels["haproxy-haptic.org/type"] == sensitiveMapType {
			found = append(found, secrets.Items[i])
		}
	}
	return found
}

func mapFilePaths(ctx context.Context, t *testing.T, crd *fake.Clientset) []string {
	t.Helper()
	mapFiles, err := crd.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	paths := make([]string, 0, len(mapFiles.Items))
	for i := range mapFiles.Items {
		paths = append(paths, mapFiles.Items[i].Spec.Path)
	}
	return paths
}

func TestSensitiveMapIsPublishedOnlyAsSecret(t *testing.T) {
	ctx, kube, crd, publisher := newTestPublisher(t)
	req := sensitiveMapPublishRequest(t, testSensitiveMap)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))

	for _, path := range mapFilePaths(ctx, t, crd) {
		assert.Equal(t, "maps/routes.map", path, "a sensitive map is never an HAProxyMapFile")
	}
	secrets := sensitiveMapSecrets(ctx, t, kube)
	require.NotEmpty(t, secrets)

	sha := sha256.Sum256([]byte(testSensitiveMapContent))
	forbidden := []string{"hunter2", hex.EncodeToString(sha[:]), renderplan.DigestString(testSensitiveMapContent)}
	for i := range secrets {
		assert.Equal(t, []byte(testSensitiveMapContent), secrets[i].Data[secretFileDataKey])
		metadata, err := json.Marshal(secrets[i].ObjectMeta)
		require.NoError(t, err)
		for _, value := range forbidden {
			assert.NotContains(t, string(metadata), value, "Secret %s", secrets[i].Name)
		}
	}

	cfgs, err := crd.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, cfgs.Items, 2, "the current configuration and its retained checkpoint")
	for i := range cfgs.Items {
		cfg := &cfgs.Items[i]
		readable, err := json.Marshal(cfg)
		require.NoError(t, err)
		texts := []string{string(readable)}
		if cfg.Spec.RetainedPlan != "" {
			payload, err := compression.Decompress(cfg.Spec.RetainedPlan)
			require.NoError(t, err)
			var decoded struct{ Plan string }
			require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
			checkpoint, err := compression.Decompress(decoded.Plan)
			require.NoError(t, err)
			assert.NotContains(t, checkpoint, `"maps/keys.map":`, "the checkpoint carries no sensitive entries")
			texts = append(texts, payload, checkpoint)
		}
		for _, text := range texts {
			for _, value := range forbidden {
				assert.NotContains(t, text, value, "HAProxyCfg %s", cfg.Name)
			}
		}
	}
}

func TestRetainedSensitiveMapRoundTrips(t *testing.T) {
	ctx, _, _, publisher := newTestPublisher(t)
	req := sensitiveMapPublishRequest(t, testSensitiveMap)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))

	loaded, err := publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	artifacts, err := loaded[0].Output.ArtifactSnapshot()
	require.NoError(t, err)
	files, err := dataplane.MaterializeAuxiliaryFileSnapshot(artifacts)
	require.NoError(t, err)
	assert.Equal(t, []auxiliaryfiles.SensitiveMapFile{testSensitiveMap}, files.SensitiveMapFiles)
	assert.Empty(t, files.CurrentFiles()["keys.map"])
	planSnapshot, err := loaded[0].Output.PlanSnapshot()
	require.NoError(t, err)
	plan, err := planSnapshot.SharedPlan()
	require.NoError(t, err)
	assert.NotContains(t, plan.Maps, "maps/keys.map")
}

func TestRetainedRefusesTamperedSensitiveMap(t *testing.T) {
	for name, tamper := range map[string]func(*corev1.Secret){
		"content": func(secret *corev1.Secret) { secret.Data[secretFileDataKey] = []byte("route-a other\n") },
		"seal": func(secret *corev1.Secret) {
			secret.Annotations[AuxiliaryChecksumAnnotationKey] = sealChecksumPrefix + "forged"
		},
		"type": func(secret *corev1.Secret) { secret.Labels["haproxy-haptic.org/type"] = secretFileType },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, kube, _, publisher := newTestPublisher(t)
			req := sensitiveMapPublishRequest(t, testSensitiveMap)
			_, err := publisher.PublishConfig(ctx, &req)
			require.NoError(t, err)
			require.NoError(t, publisher.PublishRetained(ctx, &req))
			for _, secret := range sensitiveMapSecrets(ctx, t, kube) {
				if !strings.HasSuffix(secret.Name, retainedChildSuffix) {
					continue
				}
				tamper(&secret)
				_, err := kube.CoreV1().Secrets("default").Update(ctx, &secret, metav1.UpdateOptions{})
				require.NoError(t, err)
			}
			_, err = publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
			require.Error(t, err)
		})
	}
}

func TestUnkeyedSensitiveMapIsNotPublished(t *testing.T) {
	ctx, kube, _, publisher := newTestPublisher(t)
	req := basePublishRequest()
	req.Force = true
	unkeyed := testSensitiveMap
	unkeyed.Seal = auxiliaryfiles.SealSensitive(nil, unkeyed.Content)
	req.AuxiliaryFiles = &AuxiliaryFiles{SensitiveMapFiles: []auxiliaryfiles.SensitiveMapFile{unkeyed}}
	_, err := publisher.PublishConfig(ctx, &req)
	require.ErrorContains(t, err, "has no keyed seal")
	assert.Empty(t, sensitiveMapSecrets(ctx, t, kube))
}

// A map that turns sensitive leaves its HAProxyMapFile behind as a stale child.
func TestMapTurningSensitivePrunesItsMapFile(t *testing.T) {
	ctx, kube, crd, publisher := newTestPublisher(t)
	publish := func(files *AuxiliaryFiles) {
		t.Helper()
		req := basePublishRequest()
		req.Force = true
		req.AuxiliaryFiles = files
		_, err := publisher.PublishConfig(ctx, &req)
		require.NoError(t, err)
	}
	publish(&AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{{Path: testSensitiveMap.Path, Content: testSensitiveMap.Content}}})
	require.Equal(t, []string{testSensitiveMap.Path}, mapFilePaths(ctx, t, crd))

	publish(&AuxiliaryFiles{SensitiveMapFiles: []auxiliaryfiles.SensitiveMapFile{testSensitiveMap}})
	assert.Empty(t, mapFilePaths(ctx, t, crd))
	assert.Len(t, sensitiveMapSecrets(ctx, t, kube), 1)

	publish(&AuxiliaryFiles{})
	assert.Empty(t, sensitiveMapSecrets(ctx, t, kube), "a dropped sensitive map is pruned")
}
