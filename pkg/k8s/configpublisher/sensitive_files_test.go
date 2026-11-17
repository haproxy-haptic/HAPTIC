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
	"encoding/json"
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
)

const testSensitiveFileContent = "c2Vzc2lvbi10aWNrZXQta2V5\n"

var testSensitiveFile = auxiliaryfiles.SecretFile{
	Path: "ssl/tls-ticket-keys", Content: testSensitiveFileContent, HAProxyReads: true,
	Seal: auxiliaryfiles.SealContent([]byte("installation key"), testSensitiveFileContent),
}

func sensitiveFilePublishRequest(t *testing.T, sensitive auxiliaryfiles.SecretFile) PublishRequest {
	t.Helper()
	const config = "global\n"
	files := &dataplane.AuxiliaryFiles{SecretFiles: []auxiliaryfiles.SecretFile{sensitive}}
	artifactAuthority := renderartifact.NewAuthority()
	artifacts, err := dataplane.BuildAuxiliaryFileSnapshot(artifactAuthority, nil, files)
	require.NoError(t, err)
	sealed := exactPublisherPlanFile(sensitive.Path, renderplan.FileKindGeneral, true, sensitive.Content)
	sealed.Seal = sensitive.Seal
	plan := &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		Sections: []renderplan.Section{{
			Kind: renderplan.SectionKindCore, Name: "core#0", Text: config,
			TextKnown: true, TextDigest: renderplan.DigestString(config), Length: len(config),
		}},
		Files: []renderplan.File{
			exactPublisherPlanFile(renderplan.ConfigFilePath, renderplan.FileKindConfig, true, config),
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

func sensitiveFileSecrets(ctx context.Context, t *testing.T, kube *k8sfake.Clientset) []corev1.Secret {
	t.Helper()
	secrets, err := kube.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	var found []corev1.Secret
	for i := range secrets.Items {
		if IsSensitiveFileSecret(secrets.Items[i].Labels) {
			found = append(found, secrets.Items[i])
		}
	}
	return found
}

func TestSensitiveFileIsPublishedOnlyAsSecret(t *testing.T) {
	ctx, kube, crd, publisher := newTestPublisher(t)
	req := sensitiveFilePublishRequest(t, testSensitiveFile)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))

	generalFiles, err := crd.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, generalFiles.Items, "a sensitive file is never an HAProxyGeneralFile")

	secrets := sensitiveFileSecrets(ctx, t, kube)
	require.NotEmpty(t, secrets)
	for i := range secrets {
		content, err := SensitiveFileContent(&secrets[i])
		require.NoError(t, err)
		assert.Equal(t, testSensitiveFileContent, content)
		metadata, err := json.Marshal(secrets[i].ObjectMeta)
		require.NoError(t, err)
		assert.NotContains(t, string(metadata), "c2Vzc2lvbi10aWNrZXQta2V5")
	}

	cfgs, err := crd.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	for i := range cfgs.Items {
		readable, err := json.Marshal(&cfgs.Items[i])
		require.NoError(t, err)
		texts := []string{string(readable)}
		if retained := cfgs.Items[i].Spec.RetainedPlan; retained != "" {
			payload, err := compression.Decompress(retained)
			require.NoError(t, err)
			texts = append(texts, payload)
		}
		for _, text := range texts {
			assert.NotContains(t, text, "c2Vzc2lvbi10aWNrZXQta2V5", "HAProxyCfg %s", cfgs.Items[i].Name)
		}
	}
}

func TestRetainedSensitiveFileRoundTrips(t *testing.T) {
	ctx, _, _, publisher := newTestPublisher(t)
	req := sensitiveFilePublishRequest(t, testSensitiveFile)
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
	assert.Equal(t, []auxiliaryfiles.SecretFile{testSensitiveFile}, files.SecretFiles)
}

func TestUnkeyedSensitiveFileIsNotPublished(t *testing.T) {
	ctx, kube, _, publisher := newTestPublisher(t)
	req := basePublishRequest()
	req.Force = true
	unkeyed := testSensitiveFile
	unkeyed.Seal = auxiliaryfiles.SealSensitive(nil, unkeyed.Content)
	req.AuxiliaryFiles = &AuxiliaryFiles{SecretFiles: []auxiliaryfiles.SecretFile{unkeyed}}
	_, err := publisher.PublishConfig(ctx, &req)
	require.ErrorContains(t, err, "has no keyed seal")
	assert.Empty(t, sensitiveFileSecrets(ctx, t, kube))
}

func TestSensitiveFileContentRejectsTamperedSecret(t *testing.T) {
	ctx, kube, _, publisher := newTestPublisher(t)
	req := sensitiveFilePublishRequest(t, testSensitiveFile)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	secrets := sensitiveFileSecrets(ctx, t, kube)
	require.NotEmpty(t, secrets)
	secrets[0].Data[secretFileDataKey] = []byte("other\n")
	_, err = SensitiveFileContent(&secrets[0])
	require.Error(t, err)
}
