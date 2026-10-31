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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	v1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

func retainedPublishRequest(t *testing.T) PublishRequest {
	t.Helper()
	return retainedPublishRequestWithConfig(t, "global\n")
}

func retainedPublishRequestWithConfig(t *testing.T, config string) PublishRequest {
	t.Helper()
	return retainedPublishRequestWithMaps(t, config, 0)
}

func retainedPublishRequestWithMaps(t *testing.T, config string, extraMaps int) PublishRequest {
	t.Helper()
	fixture := newConfigPublisherOutputFixtureWithMaps(t, config, extraMaps)
	req := basePublishRequest()
	req.OutputSnapshot = fixture.snapshot
	planID, err := fixture.snapshot.PlanID()
	require.NoError(t, err)
	req.ConfirmedPod = &v1.PodDeploymentStatus{PodName: "haproxy-0", PodUID: "pod-uid", PodRuntimeID: "container-id", Checksum: fixture.checksum, AppliedPlanID: planID, RunningPlanID: planID, WorkerOpsPlanID: planID, Mode: "reload"}
	return req
}

func alreadyDeletedReaction(tracker k8stesting.ObjectTracker, name string) k8stesting.ReactionFunc {
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		deletion := action.(k8stesting.DeleteAction)
		if deletion.GetName() != name {
			return false, nil, nil
		}
		if err := tracker.Delete(action.GetResource(), action.GetNamespace(), name); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), name)
	}
}

func TestRetainedRepublishesPrunedCheckpoint(t *testing.T) {
	for _, alreadyDeleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("already-deleted=%t", alreadyDeleted), func(t *testing.T) {
			ctx, _, crd, publisher := newTestPublisher(t)
			publisher.SetRepublishInterval(time.Hour)
			first := retainedPublishRequest(t)
			_, err := publisher.PublishConfig(ctx, &first)
			require.NoError(t, err)
			require.NoError(t, publisher.PublishRetained(ctx, &first))
			original, err := publisher.LoadRetained(ctx, first.TemplateConfigNamespace, first.TemplateConfigName, first.TemplateConfigUID)
			require.NoError(t, err)
			require.Len(t, original, 1)
			name := original[0].Reference.Name
			if alreadyDeleted {
				crd.PrependReactor("delete", "haproxycfgs", alreadyDeletedReaction(crd.Tracker(), name))
			}
			for _, maxConnections := range []int{1000, 2000} {
				request := retainedPublishRequestWithConfig(t, fmt.Sprintf("global\n  maxconn %d\n", maxConnections))
				_, err := publisher.PublishConfig(ctx, &request)
				require.NoError(t, err)
				require.NoError(t, publisher.PublishRetained(ctx, &request))
			}
			client := crd.HaproxyTemplateICV1alpha1().HAProxyCfgs(first.TemplateConfigNamespace)
			_, err = client.Get(ctx, name, metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err))
			_, err = publisher.PublishConfig(ctx, &first)
			require.NoError(t, err)
			require.NoError(t, publisher.PublishRetained(ctx, &first))
			restored, err := publisher.LoadRetained(ctx, first.TemplateConfigNamespace, first.TemplateConfigName, first.TemplateConfigUID)
			require.NoError(t, err)
			require.Len(t, restored, 2)
			assert.Equal(t, original[0].Reference.Checksum, restored[0].Reference.Checksum)
			assert.Equal(t, name, restored[0].Reference.Name)
			assert.Equal(t, original[0].ConfirmedPod, restored[0].ConfirmedPod)
			require.NoError(t, restored[0].Authority.ValidateSnapshot(restored[0].Output))
		})
	}
}

func TestRetainedReferencesPreserveOlderRunningVersions(t *testing.T) {
	newest := v1.RetainedConfigReference{Name: "newest", Checksum: "newest"}
	previous := v1.RetainedConfigReference{Name: "previous", Checksum: "previous"}
	unused := v1.RetainedConfigReference{Name: "unused", Checksum: "unused"}
	running := v1.RetainedConfigReference{Name: "running", Checksum: "running"}
	status := v1.HAProxyCfgStatus{
		RetainedConfigs: []v1.RetainedConfigReference{previous, unused, running},
		DeployedToPods:  []v1.PodDeploymentStatus{{PodName: "lagging", Checksum: running.Checksum}},
	}
	assert.Equal(t, []v1.RetainedConfigReference{newest, previous, running}, retainedReferences(newest, &status))
	status.DeployedToPods[0].Checksum = newest.Checksum
	assert.Equal(t, []v1.RetainedConfigReference{newest, previous}, retainedReferences(newest, &status))
}

func TestRetainedRoundTripPreservesEveryArtifactFamily(t *testing.T) {
	ctx, _, _, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))
	loaded, err := publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	want, err := req.OutputSnapshot.PlanSnapshot()
	require.NoError(t, err)
	wantPlan, err := want.SharedPlan()
	require.NoError(t, err)
	got, err := loaded[0].Output.PlanSnapshot()
	require.NoError(t, err)
	gotPlan, err := got.SharedPlan()
	require.NoError(t, err)
	assert.True(t, renderplan.ExactlyEqual(wantPlan, gotPlan))
	assert.Equal(t, *req.ConfirmedPod, loaded[0].ConfirmedPod)
	assert.NoError(t, loaded[0].Authority.ValidateSnapshot(loaded[0].Output))
}

func TestRetainedPublicationSurvivesNewUnacknowledgedRender(t *testing.T) {
	ctx, _, crd, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))
	before, err := publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.NoError(t, err)
	unconfirmed := basePublishRequest()
	unconfirmed.Config = "not valid HAProxy configuration"
	unconfirmed.Checksum = "unconfirmed"
	_, err = publisher.PublishConfig(ctx, &unconfirmed)
	require.NoError(t, err)
	loaded, err := publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, before[0].Reference, loaded[0].Reference)
	crd.ClearActions()
	_, err = publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.NoError(t, err)
	for _, action := range crd.Actions() {
		assert.Contains(t, []string{"get", "list"}, action.GetVerb(), "recovery must not publish or clean up")
	}
}

func TestRetainedReceiptSurvivesPodRetirement(t *testing.T) {
	for _, discovered := range []bool{false, true} {
		t.Run(fmt.Sprintf("discovery=%t", discovered), func(t *testing.T) {
			ctx, _, crd, publisher := newTestPublisher(t)
			req := retainedPublishRequest(t)
			_, err := publisher.PublishConfig(ctx, &req)
			require.NoError(t, err)
			require.NoError(t, publisher.PublishRetained(ctx, &req))
			client := crd.HaproxyTemplateICV1alpha1().HAProxyCfgs(req.TemplateConfigNamespace)
			current, err := client.Get(ctx, GenerateRuntimeConfigName(req.TemplateConfigName), metav1.GetOptions{})
			require.NoError(t, err)
			current.Status.DeployedToPods = []v1.PodDeploymentStatus{*req.ConfirmedPod}
			_, err = client.UpdateStatus(ctx, current, metav1.UpdateOptions{})
			require.NoError(t, err)
			checkpoint, err := client.Get(ctx, current.Status.RetainedConfigs[0].Name, metav1.GetOptions{})
			require.NoError(t, err)
			stamp := *req.ConfirmedPod
			require.NoError(t, publisher.mutateAuxiliaryFilePods(ctx, checkpoint.Status.AuxiliaryFiles,
				func([]v1.PodDeploymentStatus) ([]v1.PodDeploymentStatus, bool) {
					return []v1.PodDeploymentStatus{stamp}, true
				}))
			filesBefore, err := publisher.readRetainedFiles(ctx, checkpoint, nil)
			require.NoError(t, err)
			if discovered {
				err = publisher.ReconcileDeployedToPods(ctx, req.TemplateConfigNamespace, nil)
			} else {
				err = publisher.CleanupPodReferences(ctx, &PodCleanupRequest{
					Namespace: req.TemplateConfigNamespace, PodName: stamp.PodName, PodUID: stamp.PodUID,
				})
			}
			require.NoError(t, err)
			after, err := client.Get(ctx, checkpoint.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, checkpoint, after)
			filesAfter, err := publisher.readRetainedFiles(ctx, after, nil)
			require.NoError(t, err)
			assert.Equal(t, filesBefore, filesAfter)
			current, err = client.Get(ctx, current.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Empty(t, current.Status.DeployedToPods, "live fleet status still drops departed pods")
			loaded, err := publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
			require.NoError(t, err)
			require.Len(t, loaded, 1)
			assert.Equal(t, stamp, loaded[0].ConfirmedPod)
		})
	}
}

func TestRetainedRefusesTamperedConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*v1.HAProxyCfg)
		message string
	}{
		{"content", func(c *v1.HAProxyCfg) { c.Spec.Content += "corrupt" }, "checkpoint section"},
		{"checksum", func(c *v1.HAProxyCfg) { c.Spec.Checksum = "forged" }, "content address differs"},
		{"plan", func(c *v1.HAProxyCfg) { c.Spec.RetainedPlan += "corrupt" }, "content address differs"},
		{"owner", func(c *v1.HAProxyCfg) { c.OwnerReferences[0].UID = "foreign" }, "owner or UID differs"},
		{"receipt", func(c *v1.HAProxyCfg) { c.Status.DeployedToPods = nil }, "no confirmed worker receipt"},
		{"pending reload", func(c *v1.HAProxyCfg) { c.Status.DeployedToPods[0].Mode = "scheduled" }, "no confirmed worker receipt"},
		{"auxiliary set", func(c *v1.HAProxyCfg) { c.Status.AuxiliaryFiles.SetID = "content-sha256:forged" }, "set differs"},
		{"missing reference", func(c *v1.HAProxyCfg) { c.Status.AuxiliaryFiles.MapFiles = nil }, "missing or differs"},
		{"foreign namespace", func(c *v1.HAProxyCfg) { c.Status.AuxiliaryFiles.MapFiles[0].Namespace = "foreign" }, "foreign kind or namespace"},
		{"repeated reference", func(c *v1.HAProxyCfg) {
			c.Status.AuxiliaryFiles.MapFiles = append(c.Status.AuxiliaryFiles.MapFiles, c.Status.AuxiliaryFiles.MapFiles[0])
		}, "reference is repeated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, crd, publisher := newTestPublisher(t)
			req := retainedPublishRequest(t)
			_, err := publisher.PublishConfig(ctx, &req)
			require.NoError(t, err)
			require.NoError(t, publisher.PublishRetained(ctx, &req))
			client := crd.HaproxyTemplateICV1alpha1().HAProxyCfgs(req.TemplateConfigNamespace)
			main, err := client.Get(ctx, GenerateRuntimeConfigName(req.TemplateConfigName), metav1.GetOptions{})
			require.NoError(t, err)
			checkpoint, err := client.Get(ctx, main.Status.RetainedConfigs[0].Name, metav1.GetOptions{})
			require.NoError(t, err)
			tc.mutate(checkpoint)
			_, err = client.Update(ctx, checkpoint, metav1.UpdateOptions{})
			require.NoError(t, err)
			_, err = publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
			require.ErrorContains(t, err, tc.message)
		})
	}
}

func TestRetainedChecksSecretBytesRatherThanAnnotations(t *testing.T) {
	ctx, kube, crd, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))
	client := crd.HaproxyTemplateICV1alpha1().HAProxyCfgs(req.TemplateConfigNamespace)
	main, err := client.Get(ctx, GenerateRuntimeConfigName(req.TemplateConfigName), metav1.GetOptions{})
	require.NoError(t, err)
	checkpoint, err := client.Get(ctx, main.Status.RetainedConfigs[0].Name, metav1.GetOptions{})
	require.NoError(t, err)
	secretName := checkpoint.Status.AuxiliaryFiles.SSLCertificates[0].Name
	secret, err := kube.CoreV1().Secrets(req.TemplateConfigNamespace).Get(ctx, secretName, metav1.GetOptions{})
	require.NoError(t, err)
	secret.Data["certificate"] = []byte("tampered key material")
	_, err = kube.CoreV1().Secrets(req.TemplateConfigNamespace).Update(ctx, secret, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.ErrorContains(t, err, "auxiliary checksum differs")
}

func TestRetainedIncompletePublicationNeverCommitsReference(t *testing.T) {
	ctx, _, crd, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	crd.PrependReactor("create", "haproxymapfiles", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("publication interrupted")
	})
	require.ErrorContains(t, publisher.PublishRetained(ctx, &req), "publication interrupted")
	_, err = publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.ErrorContains(t, err, "no acknowledged retained configuration")
}

func TestRetainedRequiresRunningReceipt(t *testing.T) {
	ctx, _, _, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	req.ConfirmedPod.Mode = "scheduled"
	require.ErrorContains(t, publisher.PublishRetained(ctx, &req), "no worker acknowledgement")
	req.ConfirmedPod.Mode = "reload"
	req.ConfirmedPod.PodUID = ""
	require.ErrorContains(t, publisher.PublishRetained(ctx, &req), "no worker acknowledgement")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	req = retainedPublishRequest(t)
	require.Error(t, publisher.PublishRetained(cancelled, &req))
}

func TestRetainedRoundTripPreservesEmptyArtifactFamilies(t *testing.T) {
	for _, withList := range []bool{false, true} {
		t.Run(fmt.Sprint(withList), func(t *testing.T) {
			ctx, _, _, publisher := newTestPublisher(t)
			config := "global\n"
			plan := &renderplan.Plan{SchemaVersion: renderplan.SchemaVersion,
				Sections: []renderplan.Section{{Kind: renderplan.SectionKindCore, Name: "core#0", Text: config, TextKnown: true, TextDigest: renderplan.DigestString(config), Length: len(config)}},
				Files:    []renderplan.File{exactPublisherPlanFile(renderplan.ConfigFilePath, renderplan.FileKindConfig, true, config)}}
			files := &dataplane.AuxiliaryFiles{}
			if withList {
				files.GeneralFiles = []auxiliaryfiles.GeneralFile{{Filename: "blocked.acl", Path: "general/blocked.acl", Content: "192.0.2.1\n"}}
				plan.Files = append(plan.Files, exactPublisherPlanFile("general/blocked.acl", renderplan.FileKindGeneral, true, "192.0.2.1\n"))
			}
			plan.ComputeID()
			artifactAuthority := renderartifact.NewAuthority()
			artifacts, err := dataplane.BuildAuxiliaryFileSnapshot(artifactAuthority, nil, files)
			require.NoError(t, err)
			authority, err := renderoutput.NewAuthority(renderplan.NewAuthority(), artifactAuthority)
			require.NoError(t, err)
			output, err := renderoutput.NewSnapshot(authority, config, plan, artifacts, nil)
			require.NoError(t, err)
			checksum, err := output.ContentChecksum()
			require.NoError(t, err)
			req := basePublishRequest()
			req.OutputSnapshot = output
			req.ConfirmedPod = &v1.PodDeploymentStatus{PodName: "worker", PodUID: "uid", PodRuntimeID: "runtime", Checksum: checksum, AppliedPlanID: plan.ID, RunningPlanID: plan.ID, Mode: "reload"}
			_, err = publisher.PublishConfig(ctx, &req)
			require.NoError(t, err)
			require.NoError(t, publisher.PublishRetained(ctx, &req))
			loaded, err := publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
			require.NoError(t, err)
			require.Len(t, loaded, 1)
			require.Equal(t, checksum, loaded[0].Reference.Checksum)
		})
	}
}

func TestRetainedCheckpointsShareUnchangedAuxiliaryFiles(t *testing.T) {
	ctx, kube, crd, publisher := newTestPublisher(t)
	first := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &first)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &first))
	children := func() int {
		maps, err := crd.HaproxyTemplateICV1alpha1().HAProxyMapFiles(first.TemplateConfigNamespace).List(ctx, metav1.ListOptions{LabelSelector: retainedLabel})
		require.NoError(t, err)
		secrets, err := kube.CoreV1().Secrets(first.TemplateConfigNamespace).List(ctx, metav1.ListOptions{LabelSelector: retainedLabel})
		require.NoError(t, err)
		return len(maps.Items) + len(secrets.Items)
	}
	shared := children()
	require.Equal(t, 4, shared, "one map, one certificate, one CA, and one secret-file Secret")

	for _, maxConnections := range []int{1000, 2000, 3000} {
		crd.ClearActions()
		kube.ClearActions()
		request := retainedPublishRequestWithConfig(t, fmt.Sprintf("global\n  maxconn %d\n", maxConnections))
		_, err := publisher.PublishConfig(ctx, &request)
		require.NoError(t, err)
		require.NoError(t, publisher.PublishRetained(ctx, &request))
		for _, action := range append(crd.Actions(), kube.Actions()...) {
			if action.GetResource().Resource == "haproxycfgs" {
				continue
			}
			assert.Contains(t, []string{"get", "list"}, action.GetVerb(), "unchanged auxiliary files are reused: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
		assert.Equal(t, shared, children())
	}
	loaded, err := publisher.LoadRetained(ctx, first.TemplateConfigNamespace, first.TemplateConfigName, first.TemplateConfigUID)
	require.NoError(t, err)
	require.Len(t, loaded, 2)
	for i := range loaded {
		require.NoError(t, loaded[i].Authority.ValidateSnapshot(loaded[i].Output))
	}
}

func TestRetainedPruningDeletesOnlyUnreferencedSharedFiles(t *testing.T) {
	ctx, _, crd, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))
	client := crd.HaproxyTemplateICV1alpha1().HAProxyMapFiles(req.TemplateConfigNamespace)
	labels := map[string]string{retainedLabel: string(req.TemplateConfigUID)}
	orphan := &v1.HAProxyMapFile{ObjectMeta: metav1.ObjectMeta{Name: "orphan" + retainedChildSuffix, Namespace: req.TemplateConfigNamespace, Labels: labels, OwnerReferences: templateConfigOwnerRefs(&req)}}
	foreign := &v1.HAProxyMapFile{ObjectMeta: metav1.ObjectMeta{Name: "foreign" + retainedChildSuffix, Namespace: req.TemplateConfigNamespace, Labels: labels}}
	for _, item := range []*v1.HAProxyMapFile{orphan, foreign} {
		_, err = client.Create(ctx, item, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	for _, maxConnections := range []int{1000, 2000} {
		request := retainedPublishRequestWithConfig(t, fmt.Sprintf("global\n  maxconn %d\n", maxConnections))
		_, err := publisher.PublishConfig(ctx, &request)
		require.NoError(t, err)
		require.NoError(t, publisher.PublishRetained(ctx, &request))
	}
	_, err = client.Get(ctx, orphan.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "unreferenced shared file is pruned with its last checkpoint")
	_, err = client.Get(ctx, foreign.Name, metav1.GetOptions{})
	assert.NoError(t, err, "an object the template does not own is never deleted")
	loaded, err := publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.NoError(t, err)
	require.Len(t, loaded, 2)
}

func TestRetainedRefusesSharedFileWithForeignOwner(t *testing.T) {
	ctx, _, crd, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))
	client := crd.HaproxyTemplateICV1alpha1().HAProxyMapFiles(req.TemplateConfigNamespace)
	maps, err := client.List(ctx, metav1.ListOptions{LabelSelector: retainedLabel})
	require.NoError(t, err)
	require.Len(t, maps.Items, 1)
	shared := &maps.Items[0]
	shared.OwnerReferences[0].UID = "foreign"
	_, err = client.Update(ctx, shared, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.ErrorContains(t, err, "foreign owner")
}

func TestRetainedReadsCheckpointOwnedFiles(t *testing.T) {
	ctx, _, crd, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))
	cfgs := crd.HaproxyTemplateICV1alpha1().HAProxyCfgs(req.TemplateConfigNamespace)
	main, err := cfgs.Get(ctx, GenerateRuntimeConfigName(req.TemplateConfigName), metav1.GetOptions{})
	require.NoError(t, err)
	checkpoint, err := cfgs.Get(ctx, main.Status.RetainedConfigs[0].Name, metav1.GetOptions{})
	require.NoError(t, err)
	maps := crd.HaproxyTemplateICV1alpha1().HAProxyMapFiles(req.TemplateConfigNamespace)
	ref := &checkpoint.Status.AuxiliaryFiles.MapFiles[0]
	shared, err := maps.Get(ctx, ref.Name, metav1.GetOptions{})
	require.NoError(t, err)
	legacy := shared.DeepCopy()
	legacy.ResourceVersion, legacy.UID = "", ""
	legacy.Name = strings.TrimSuffix(shared.Name, retainedChildSuffix) + retainedSuffix(checkpoint.Spec.Checksum, checkpoint.Spec.RetainedPlan)
	legacy.Labels = runtimeConfigLabels(checkpoint)
	legacy.OwnerReferences = runtimeConfigOwnerRefs(checkpoint)
	_, err = maps.Create(ctx, legacy, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, maps.Delete(ctx, shared.Name, metav1.DeleteOptions{}))
	ref.Name = legacy.Name
	_, err = cfgs.UpdateStatus(ctx, checkpoint, metav1.UpdateOptions{})
	require.NoError(t, err)

	loaded, err := publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.NoError(t, err)
	require.Len(t, loaded, 1)

	legacy, err = maps.Get(ctx, legacy.Name, metav1.GetOptions{})
	require.NoError(t, err)
	legacy.OwnerReferences = templateConfigOwnerRefs(&req)
	legacy.Labels = map[string]string{retainedLabel: string(req.TemplateConfigUID)}
	_, err = maps.Update(ctx, legacy, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.ErrorContains(t, err, "content address differs", "a shared owner needs a shared name")
}

// Each checkpoint publication, and each republication of a committed one,
// reads the shared children once per kind for publication and once per kind
// for verification, independent of how many children there are.
func TestRetainedPublicationReadsSharedChildrenPerKind(t *testing.T) {
	const checkpoints, republications, extraMaps, kinds = 2, 3, 24, 4
	ctx, kube, crd, publisher := newTestPublisher(t)
	childResources := map[string]bool{"haproxymapfiles": true, "haproxygeneralfiles": true, "haproxycrtlistfiles": true, "secrets": true}
	reads := map[string]int{}
	publishRetained := func(request *PublishRequest) {
		_, err := publisher.PublishConfig(ctx, request)
		require.NoError(t, err)
		crd.ClearActions()
		kube.ClearActions()
		require.NoError(t, publisher.PublishRetained(ctx, request))
		for _, action := range append(crd.Actions(), kube.Actions()...) {
			if childResources[action.GetResource().Resource] && (action.GetVerb() == "get" || action.GetVerb() == "list") {
				reads[action.GetVerb()]++
			}
		}
	}
	var last PublishRequest
	for i := range checkpoints {
		last = retainedPublishRequestWithMaps(t, fmt.Sprintf("global\n  maxconn %d\n", 1000+i), extraMaps)
		publishRetained(&last)
	}
	for range republications {
		publishRetained(&last)
	}

	publications := checkpoints + republications
	assert.Equal(t, map[string]int{"list": 2 * kinds * publications}, reads)
	loaded, err := publisher.LoadRetained(ctx, last.TemplateConfigNamespace, last.TemplateConfigName, last.TemplateConfigUID)
	require.NoError(t, err)
	require.Len(t, loaded, checkpoints)
	for i := range loaded {
		require.NoError(t, loaded[i].Authority.ValidateSnapshot(loaded[i].Output))
	}
}

func TestRetainedRepublicationRepairsSharedChildren(t *testing.T) {
	ctx, kube, crd, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))
	namespace := req.TemplateConfigNamespace
	maps := crd.HaproxyTemplateICV1alpha1().HAProxyMapFiles(namespace)
	listed, err := maps.List(ctx, metav1.ListOptions{LabelSelector: retainedLabel})
	require.NoError(t, err)
	require.Len(t, listed.Items, 1)
	original := listed.Items[0].Spec
	tampered := listed.Items[0].DeepCopy()
	tampered.Spec.Entries = "tampered backend\n"
	_, err = maps.Update(ctx, tampered, metav1.UpdateOptions{})
	require.NoError(t, err)
	secrets, err := kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{LabelSelector: retainedLabel})
	require.NoError(t, err)
	require.NotEmpty(t, secrets.Items)
	deleted := secrets.Items[0].Name
	require.NoError(t, kube.CoreV1().Secrets(namespace).Delete(ctx, deleted, metav1.DeleteOptions{}))

	require.NoError(t, publisher.PublishRetained(ctx, &req))
	repaired, err := maps.Get(ctx, tampered.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, original, repaired.Spec)
	_, err = kube.CoreV1().Secrets(namespace).Get(ctx, deleted, metav1.GetOptions{})
	require.NoError(t, err)
	_, err = publisher.LoadRetained(ctx, namespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.NoError(t, err)
}

func TestRetainedPublicationReadsLiveWhenListingMissesChild(t *testing.T) {
	ctx, _, crd, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))
	crd.PrependReactor("list", "haproxymapfiles", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &v1.HAProxyMapFileList{}, nil
	})
	crd.ClearActions()
	require.NoError(t, publisher.PublishRetained(ctx, &req))
	var mapReads []string
	for _, action := range crd.Actions() {
		if action.GetResource().Resource == "haproxymapfiles" {
			mapReads = append(mapReads, action.GetVerb())
		}
	}
	assert.Equal(t, []string{"list", "create", "get", "list", "get"}, mapReads,
		"a child the listing misses is created, refused as existing, and then read live")
}
