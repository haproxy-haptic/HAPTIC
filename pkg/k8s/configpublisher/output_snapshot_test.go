// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package configpublisher

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestCanonicalizePublishRequestOutputSnapshot(t *testing.T) {
	fixture := newConfigPublisherOutputFixture(t)
	snapshot, artifacts, files := fixture.snapshot, fixture.artifacts, fixture.files
	config, checksum := fixture.config, fixture.checksum
	poisonedArtifacts := configPublisherPoisonArtifacts(t)
	families := make(map[renderartifact.Family]bool, 6)
	require.NoError(t, artifacts.Walk(func(artifact *renderartifact.Artifact) error {
		descriptor, err := artifact.Descriptor()
		if err != nil {
			return err
		}
		families[descriptor.Family] = true
		return nil
	}))
	for _, family := range []renderartifact.Family{
		renderartifact.Map,
		renderartifact.General,
		renderartifact.Certificate,
		renderartifact.CA,
		renderartifact.CRTList,
		renderartifact.GeneralCA,
	} {
		assert.True(t, families[family])
	}
	req := basePublishRequest()
	req.OutputSnapshot = snapshot
	req.Checksum = "mutable-shadow"
	req.AuxiliaryFileSnapshot = poisonedArtifacts
	req.AuxiliaryFiles = &AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{{
		Path: "maps/poison.map", Content: "poison\n",
	}}}

	canonical, err := canonicalizePublishRequest(&req)
	require.NoError(t, err)
	assert.Equal(t, config, canonical.Config)
	assert.Equal(t, checksum, canonical.Checksum)
	assert.Nil(t, canonical.OutputSnapshot)
	assert.Nil(t, canonical.AuxiliaryFileSnapshot)
	require.NotNil(t, canonical.AuxiliaryFiles)
	assert.Equal(t, files.MapFiles, canonical.AuxiliaryFiles.MapFiles)
	assert.Equal(t, files.SSLCertificates, canonical.AuxiliaryFiles.SSLCertificates)
	assert.Equal(t, files.SSLCaFiles, canonical.AuxiliaryFiles.SSLCaFiles)
	assert.Equal(t, files.GeneralFiles, canonical.AuxiliaryFiles.GeneralFiles)
	assert.Equal(t, files.CRTListFiles, canonical.AuxiliaryFiles.CRTListFiles)
	require.Len(t, canonical.AuxiliaryFiles.GeneralFiles, 2)
	generalFamilies := make(map[string]bool, len(canonical.AuxiliaryFiles.GeneralFiles))
	for _, file := range canonical.AuxiliaryFiles.GeneralFiles {
		generalFamilies[file.Filename] = file.IsCaFile
	}
	assert.False(t, generalFamilies["errors.http"])
	assert.True(t, generalFamilies["dynamic-ca.pem"])

	transitional := basePublishRequest()
	transitional.AuxiliaryFileSnapshot = artifacts
	transitional.AuxiliaryFiles = nil
	transitional.Config = config
	transitional.Checksum = checksum
	canonical, err = canonicalizePublishRequest(&transitional)
	require.NoError(t, err)
	assert.Equal(t, files.GeneralFiles, canonical.AuxiliaryFiles.GeneralFiles)
}

func configPublisherPoisonArtifacts(tb testing.TB) *renderartifact.Snapshot {
	tb.Helper()
	builder, err := renderartifact.NewBuilder(renderartifact.NewAuthority(), nil)
	require.NoError(tb, err)
	require.NoError(tb, builder.Add(
		renderartifact.Descriptor{Family: renderartifact.Map, Path: "maps/poison.map"},
		renderartifact.NewLiteralContent("poison\n"),
	))
	artifacts, err := builder.Build()
	require.NoError(tb, err)
	return artifacts
}

func TestCanonicalizePublishRequestRejectsDualLegacyCarriers(t *testing.T) {
	artifacts := newConfigPublisherOutputFixture(t).artifacts
	_, err := canonicalizePublishRequest(&PublishRequest{
		AuxiliaryFileSnapshot: artifacts,
		AuxiliaryFiles:        &AuxiliaryFiles{},
	})
	require.Error(t, err)
	_, err = canonicalizePublishRequest(nil)
	require.Error(t, err)
	_, err = canonicalizePublishRequest(&PublishRequest{OutputSnapshot: &renderoutput.Snapshot{}})
	require.ErrorContains(t, err, "authenticating output snapshot")
}

func TestPublishConfigOutputSnapshotPublishesEveryArtifactFamily(t *testing.T) {
	ctx, k8sClient, crdClient, publisher := newTestPublisher(t)
	fixture := newConfigPublisherOutputFixture(t)
	snapshot, files := fixture.snapshot, fixture.files
	config, checksum := fixture.config, fixture.checksum
	req := basePublishRequest()
	req.Config = ""
	req.OutputSnapshot = snapshot
	req.Checksum = "mutable-shadow"

	result, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.Len(t, result.MapFileNames, 1)
	require.Len(t, result.SecretNames, 1)
	require.Len(t, result.SSLCaFileNames, 1)
	assert.Regexp(t, `^haproxy-cert-trust-`, result.SecretNames[0])
	assert.Regexp(t, `^haproxy-ca-trust-`, result.SSLCaFileNames[0])
	assert.NotEqual(t, result.SecretNames[0], result.SSLCaFileNames[0])
	require.Len(t, result.GeneralFileNames, 2)
	require.Len(t, result.CRTListFileNames, 1)

	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, result.RuntimeConfigName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, config, runtimeConfig.Spec.Content)
	assert.Equal(t, checksum, runtimeConfig.Spec.Checksum)
	require.NotNil(t, runtimeConfig.Status.AuxiliaryFiles)
	assert.Len(t, runtimeConfig.Status.AuxiliaryFiles.MapFiles, 1)
	assert.Len(t, runtimeConfig.Status.AuxiliaryFiles.SSLCertificates, 1)
	assert.Len(t, runtimeConfig.Status.AuxiliaryFiles.SSLCaFiles, 1)
	assert.Len(t, runtimeConfig.Status.AuxiliaryFiles.GeneralFiles, 2)
	assert.Len(t, runtimeConfig.Status.AuxiliaryFiles.CRTListFiles, 1)
	assert.Len(t, runtimeConfig.Status.AuxiliaryFiles.SecretFiles, 1)
	assert.Equal(t, result.SecretFileNames[0], runtimeConfig.Status.AuxiliaryFiles.SecretFiles[0].Name)
	assert.Equal(t, result.MapFileNames[0], runtimeConfig.Status.AuxiliaryFiles.MapFiles[0].Name)
	assert.Equal(t, result.GeneralFileNames[0], runtimeConfig.Status.AuxiliaryFiles.GeneralFiles[0].Name)
	assert.Equal(t, result.GeneralFileNames[1], runtimeConfig.Status.AuxiliaryFiles.GeneralFiles[1].Name)
	assert.Equal(t, result.CRTListFileNames[0], runtimeConfig.Status.AuxiliaryFiles.CRTListFiles[0].Name)
	assert.Equal(t, result.SecretNames[0], runtimeConfig.Status.AuxiliaryFiles.SSLCertificates[0].Name)
	assert.Equal(t, result.SSLCaFileNames[0], runtimeConfig.Status.AuxiliaryFiles.SSLCaFiles[0].Name)

	assertPublishedAuxiliaryObjects(ctx, t, k8sClient, crdClient, files)
}

func assertPublishedAuxiliaryObjects(
	ctx context.Context,
	tb testing.TB,
	k8sClient *k8sfake.Clientset,
	crdClient *fake.Clientset,
	files *dataplane.AuxiliaryFiles,
) {
	tb.Helper()
	mapFiles, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles("default").
		List(ctx, metav1.ListOptions{})
	require.NoError(tb, err)
	require.Len(tb, mapFiles.Items, 1)
	assert.Equal(tb, files.MapFiles[0].Path, mapFiles.Items[0].Spec.Path)
	assert.Equal(tb, files.MapFiles[0].Content, mapFiles.Items[0].Spec.Entries)

	generalFiles, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles("default").
		List(ctx, metav1.ListOptions{})
	require.NoError(tb, err)
	require.Len(tb, generalFiles.Items, 2)
	generalByName := make(map[string]haproxyv1alpha1.HAProxyGeneralFile, len(generalFiles.Items))
	for i := range generalFiles.Items {
		generalByName[generalFiles.Items[i].Spec.FileName] = generalFiles.Items[i]
	}
	for _, file := range files.GeneralFiles {
		published := generalByName[file.Filename]
		assert.Equal(tb, file.Path, published.Spec.Path)
		assert.Equal(tb, file.Content, published.Spec.Content)
		assert.Equal(tb, file.IsCaFile, published.Spec.CAFile)
	}

	secrets, err := k8sClient.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{})
	require.NoError(tb, err)
	require.Len(tb, secrets.Items, 3)
	secretByType := make(map[string]map[string][]byte, len(secrets.Items))
	for i := range secrets.Items {
		secretByType[secrets.Items[i].Labels["haproxy-haptic.org/type"]] = secrets.Items[i].Data
	}
	assert.Equal(tb, []byte(files.SSLCertificates[0].Content), secretByType["ssl-certificate"]["certificate"])
	assert.Equal(tb, []byte(files.SSLCaFiles[0].Content), secretByType["ssl-ca"]["ca"])
	assert.Equal(tb, []byte(files.SSLCertificates[0].Path), secretByType["ssl-certificate"]["path"])
	assert.Equal(tb, []byte(files.SSLCaFiles[0].Path), secretByType["ssl-ca"]["path"])
	assert.Equal(tb, []byte(files.SecretFiles[0].Content), secretByType[secretFileType][secretFileDataKey])
	assert.Equal(tb, []byte(files.SecretFiles[0].Path), secretByType[secretFileType]["path"])

	crtLists, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCRTListFiles("default").
		List(ctx, metav1.ListOptions{})
	require.NoError(tb, err)
	require.Len(tb, crtLists.Items, 1)
	assert.Equal(tb, files.CRTListFiles[0].Path, crtLists.Items[0].Spec.Path)
	assert.Equal(tb, files.CRTListFiles[0].Content, crtLists.Items[0].Spec.Entries)
}

func TestPublishConfigRejectsDualLegacyCarriersBeforeMutation(t *testing.T) {
	ctx, k8sClient, crdClient, publisher := newTestPublisher(t)
	artifacts := newConfigPublisherOutputFixture(t).artifacts
	req := basePublishRequest()
	req.AuxiliaryFileSnapshot = artifacts
	req.AuxiliaryFiles = &AuxiliaryFiles{}

	_, err := publisher.PublishConfig(ctx, &req)
	require.Error(t, err)

	runtimeConfigs, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, runtimeConfigs.Items)
	secrets, err := k8sClient.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, secrets.Items)
}

func TestPublishConfigPrunesCASecretAndReferenceIndependently(t *testing.T) {
	ctx, k8sClient, crdClient, publisher := newTestPublisher(t)
	fixture := newConfigPublisherOutputFixture(t)
	snapshot, files, config := fixture.snapshot, fixture.files, fixture.config
	initialRequest := basePublishRequest()
	initialRequest.OutputSnapshot = snapshot
	initial, err := publisher.PublishConfig(ctx, &initialRequest)
	require.NoError(t, err)
	require.Len(t, initial.SecretNames, 1)
	require.Len(t, initial.SSLCaFileNames, 1)

	remaining := &dataplane.AuxiliaryFiles{SSLCertificates: files.SSLCertificates}
	nextRequest := basePublishRequest()
	nextRequest.Config = config
	nextRequest.Checksum = dataplane.ComputeContentChecksum(config, remaining)
	nextRequest.AuxiliaryFiles = publisherAuxiliaryFiles(remaining)
	next, err := publisher.PublishConfig(ctx, &nextRequest)
	require.NoError(t, err)
	require.Len(t, next.SecretNames, 1)
	assert.Empty(t, next.SSLCaFileNames)

	secrets, err := k8sClient.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, secrets.Items, 1)
	assert.Equal(t, "ssl-certificate", secrets.Items[0].Labels["haproxy-haptic.org/type"])

	runtimeConfig, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, next.RuntimeConfigName, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotNil(t, runtimeConfig.Status.AuxiliaryFiles)
	require.Len(t, runtimeConfig.Status.AuxiliaryFiles.SSLCertificates, 1)
	assert.Equal(t, next.SecretNames[0], runtimeConfig.Status.AuxiliaryFiles.SSLCertificates[0].Name)
	assert.Empty(t, runtimeConfig.Status.AuxiliaryFiles.SSLCaFiles)
}

type configPublisherOutputFixture struct {
	snapshot  *renderoutput.Snapshot
	artifacts *renderartifact.Snapshot
	files     *dataplane.AuxiliaryFiles
	config    string
	checksum  string
}

func newConfigPublisherOutputFixture(tb testing.TB) configPublisherOutputFixture {
	tb.Helper()
	return newConfigPublisherOutputFixtureWithConfig(tb, "global\n")
}

func newConfigPublisherOutputFixtureWithConfig(tb testing.TB, config string) configPublisherOutputFixture {
	tb.Helper()
	return newConfigPublisherOutputFixtureWithMaps(tb, config, 0)
}

func newConfigPublisherOutputFixtureWithMaps(tb testing.TB, config string, extraMaps int) configPublisherOutputFixture {
	tb.Helper()
	reload := true
	files := &dataplane.AuxiliaryFiles{
		MapFiles: []auxiliaryfiles.MapFile{{
			Path: "maps/routes.map", Content: "example.test backend\n",
		}},
		GeneralFiles: []auxiliaryfiles.GeneralFile{
			{
				Filename: "errors.http", Path: "files/errors.http",
				Content: "HTTP/1.1 503 Unavailable\n", ReloadOnPush: &reload,
			},
			{
				Filename: "dynamic-ca.pem", Path: "files/dynamic-ca.pem",
				Content: "dynamic ca\n", IsCaFile: true,
			},
		},
		SSLCertificates: []auxiliaryfiles.SSLCertificate{{
			Path: "ssl/trust.pem", Content: "certificate\n",
		}},
		SSLCaFiles: []auxiliaryfiles.SSLCaFile{{
			Path: "ssl/ca/trust.pem", Content: "ca\n",
		}},
		CRTListFiles: []auxiliaryfiles.CRTListFile{{
			Path: "ssl/frontends.list", Content: "site.pem example.test\n",
		}},
		SecretFiles: []auxiliaryfiles.SecretFile{testSecretFile},
	}
	for i := range extraMaps {
		files.MapFiles = append(files.MapFiles, auxiliaryfiles.MapFile{
			Path: fmt.Sprintf("maps/extra-%d.map", i), Content: fmt.Sprintf("extra-%d.test backend\n", i),
		})
	}
	artifactAuthority := renderartifact.NewAuthority()
	artifacts, err := dataplane.BuildAuxiliaryFileSnapshot(artifactAuthority, nil, files)
	require.NoError(tb, err)

	plan := &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		Sections: []renderplan.Section{{
			Kind: renderplan.SectionKindCore, Name: "core#0", Text: config,
			TextKnown: true, TextDigest: renderplan.DigestString(config), Length: len(config),
		}},
		Maps: map[string]renderplan.Map{
			"maps/routes.map": {
				Path: "maps/routes.map", Ordered: true,
				Entries: renderplan.ParseMapEntries("example.test backend\n"),
			},
		},
		Files: []renderplan.File{
			exactPublisherPlanFile(renderplan.ConfigFilePath, renderplan.FileKindConfig, true, config),
			exactPublisherPlanFile("maps/routes.map", renderplan.FileKindMap, false, "example.test backend\n"),
			exactPublisherPlanFile("files/errors.http", renderplan.FileKindGeneral, true, "HTTP/1.1 503 Unavailable\n"),
			exactPublisherPlanFile("ssl/trust.pem", renderplan.FileKindCert, false, "certificate\n"),
			exactPublisherPlanFile("ssl/ca/trust.pem", renderplan.FileKindCA, false, "ca\n"),
			exactPublisherPlanFile("ssl/frontends.list", renderplan.FileKindCRTList, false, "site.pem example.test\n"),
			exactPublisherPlanFile("files/dynamic-ca.pem", renderplan.FileKindCA, false, "dynamic ca\n"),
			sealedPublisherPlanFile(testSecretFile),
		},
	}
	for _, file := range files.MapFiles[1:] {
		plan.Maps[file.Path] = renderplan.Map{Path: file.Path, Ordered: true, Entries: renderplan.ParseMapEntries(file.Content)}
		plan.Files = append(plan.Files, exactPublisherPlanFile(file.Path, renderplan.FileKindMap, false, file.Content))
	}
	plan.ComputeID()
	outputAuthority, err := renderoutput.NewAuthority(renderplan.NewAuthority(), artifactAuthority)
	require.NoError(tb, err)
	snapshot, err := renderoutput.NewSnapshot(outputAuthority, config, plan, artifacts, nil)
	require.NoError(tb, err)
	checksum, err := snapshot.ContentChecksum()
	require.NoError(tb, err)
	files.Sort()
	return configPublisherOutputFixture{
		snapshot: snapshot, artifacts: artifacts, files: files, config: config, checksum: checksum,
	}
}

// testSecretFileContent is low-entropy on purpose: it is what an unkeyed digest would leak.
const testSecretFileContent = "hunter2"

var testSecretFile = auxiliaryfiles.SecretFile{
	Path: "secrets/sso.client-secret", Content: testSecretFileContent,
	Seal: auxiliaryfiles.SealContent([]byte("installation key"), testSecretFileContent),
}

func sealedPublisherPlanFile(file auxiliaryfiles.SecretFile) renderplan.File {
	sealed := exactPublisherPlanFile(file.Path, renderplan.FileKindGeneral, false, file.Content)
	sealed.Seal = file.Seal
	return sealed
}

func exactPublisherPlanFile(path, kind string, reload bool, content string) renderplan.File {
	return renderplan.File{
		Path: path, Kind: kind, ReloadOnChange: reload,
		Digest: renderplan.DigestString(content), Size: int64(len(content)),
		Content: content, ContentKnown: true,
	}
}
