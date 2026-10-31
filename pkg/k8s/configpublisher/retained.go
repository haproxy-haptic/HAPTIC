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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	v1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/compression"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/planblob"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

const retainedLabel = "haproxy-haptic.org/retained-for"

type retainedPayload struct {
	Plan      string                      `json:"plan"`
	Artifacts []renderartifact.Descriptor `json:"artifacts"`
}

// RetainedConfig is a complete, content-verified checkpoint. Its worker evidence
// and compatibility with the current HAProxy binary still require validation.
type RetainedConfig struct {
	Reference    v1.RetainedConfigReference
	Output       *renderoutput.Snapshot
	Authority    *renderoutput.Authority
	ConfirmedPod v1.PodDeploymentStatus
}

func retainedSuffix(checksum, payload string) string {
	return fmt.Sprintf("-retained-%x", sha256.Sum256([]byte(checksum+"\x00"+payload)))
}

func encodeRetainedPayload(output *renderoutput.Snapshot) (string, error) {
	planSnapshot, err := output.PlanSnapshot()
	if err != nil {
		return "", err
	}
	plan, err := planSnapshot.SharedPlan()
	if err != nil {
		return "", err
	}
	encoded, err := planblob.EncodeCheckpoint(plan)
	if err != nil {
		return "", err
	}
	artifacts, err := output.ArtifactSnapshot()
	if err != nil {
		return "", err
	}
	payload := retainedPayload{Plan: encoded}
	err = artifacts.Walk(func(artifact *renderartifact.Artifact) error {
		descriptor, err := artifact.Descriptor()
		if err == nil {
			payload.Artifacts = append(payload.Artifacts, descriptor)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return compression.Compress(string(data)), nil
}

// PublishRetained advances the checkpoint only after the complete snapshot exists.
func (p *Publisher) PublishRetained(ctx context.Context, req *PublishRequest) error {
	if req.ConfirmedPod == nil || req.OutputSnapshot == nil {
		return nil
	}
	planID, err := req.OutputSnapshot.PlanID()
	if err != nil {
		return err
	}
	checksum, err := req.OutputSnapshot.ContentChecksum()
	if err != nil {
		return err
	}
	if !confirmsOutput(req.ConfirmedPod, checksum, planID) {
		return errors.New("retained configuration has no worker acknowledgement")
	}
	canonical := *req
	canonical.RetainedPlan, err = encodeRetainedPayload(req.OutputSnapshot)
	if err != nil {
		return err
	}
	canonical.ConfirmedPod = nil
	canonical.NameSuffix = retainedSuffix(checksum, canonical.RetainedPlan)
	result, err := p.PublishConfig(ctx, &canonical)
	if err != nil {
		return err
	}
	client := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs(req.TemplateConfigNamespace)
	checkpoint, err := client.Get(ctx, result.RuntimeConfigName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := p.patchRuntimeConfigStatusField(ctx, checkpoint, checkpoint, "deployedToPods", []v1.PodDeploymentStatus{*req.ConfirmedPod}); err != nil {
		return err
	}
	ref := v1.RetainedConfigReference{Name: checkpoint.Name, UID: string(checkpoint.UID), Checksum: checksum}
	listing := newChildListing(req.TemplateConfigNamespace, retainedSelector(req.TemplateConfigUID))
	if _, err := p.loadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID, ref, listing); err != nil {
		return fmt.Errorf("verifying retained publication: %w", err)
	}
	var committed []v1.RetainedConfigReference
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := client.Get(ctx, GenerateRuntimeConfigName(req.TemplateConfigName), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !ownedByTemplate(current, req.TemplateConfigName, req.TemplateConfigUID) {
			return errors.New("runtime configuration is not owned by this HAProxyTemplateConfig")
		}
		committed = retainedReferences(ref, &current.Status)
		if slices.Equal(current.Status.RetainedConfigs, committed) {
			return nil
		}
		current.Status.RetainedConfigs = committed
		_, err = client.UpdateStatus(ctx, current, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return err
	}
	return p.pruneRetained(ctx, req, committed, listing)
}

func retainedReferences(newest v1.RetainedConfigReference, status *v1.HAProxyCfgStatus) []v1.RetainedConfigReference {
	keep := []v1.RetainedConfigReference{newest}
	for _, previous := range status.RetainedConfigs {
		if previous.Name == newest.Name {
			continue
		}
		inUse := slices.ContainsFunc(status.DeployedToPods, func(pod v1.PodDeploymentStatus) bool {
			return pod.Checksum == previous.Checksum
		})
		if len(keep) < 2 || inUse {
			keep = append(keep, previous)
		}
	}
	return keep
}

func confirmsOutput(pod *v1.PodDeploymentStatus, checksum, planID string) bool {
	return pod.PodName != "" && pod.PodUID != "" && pod.PodRuntimeID != "" &&
		pod.Checksum == checksum && pod.AppliedPlanID == planID && pod.RunningPlanID != "" &&
		(pod.RunningPlanID == planID || pod.WorkerOpsPlanID == planID) && pod.LastError == "" &&
		(pod.Mode == "reload" || pod.Mode == "runtime" || pod.Mode == "file_only" || pod.Mode == "noop")
}

func ownedByTemplate(obj metav1.Object, name string, uid types.UID) bool {
	owner := metav1.GetControllerOf(obj)
	return uid != "" && owner != nil && owner.APIVersion == apiVersionV1Alpha1 &&
		owner.Kind == kindTemplateConfig && owner.Name == name && owner.UID == uid
}

func (p *Publisher) pruneRetained(ctx context.Context, req *PublishRequest, keep []v1.RetainedConfigReference, listing *childListing) error {
	client := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs(req.TemplateConfigNamespace)
	items, err := client.List(ctx, metav1.ListOptions{LabelSelector: retainedSelector(req.TemplateConfigUID)})
	if err != nil {
		return err
	}
	deleted := false
	for i := range items.Items {
		item := &items.Items[i]
		if !ownedByTemplate(item, req.TemplateConfigName, req.TemplateConfigUID) ||
			slices.ContainsFunc(keep, func(ref v1.RetainedConfigReference) bool { return ref.Name == item.Name }) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		err := client.Delete(ctx, item.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &item.UID, ResourceVersion: &item.ResourceVersion}})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		p.forgetPublishedResource(req.TemplateConfigNamespace, item.Name)
		deleted = true
	}
	// Only a checkpoint deletion can leave a shared child unreferenced.
	if !deleted {
		return nil
	}
	return p.pruneRetainedFiles(ctx, req, listing)
}

// LoadRetained reads only checkpoints explicitly committed by this template owner.
func (p *Publisher) LoadRetained(ctx context.Context, namespace, name string, uid types.UID) ([]RetainedConfig, error) {
	client := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs(namespace)
	current, err := client.Get(ctx, GenerateRuntimeConfigName(name), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if !ownedByTemplate(current, name, uid) {
		return nil, errors.New("runtime configuration has a foreign owner")
	}
	if len(current.Status.RetainedConfigs) == 0 {
		return nil, errors.New("no acknowledged retained configuration has been published")
	}
	result := make([]RetainedConfig, 0, len(current.Status.RetainedConfigs))
	for _, ref := range current.Status.RetainedConfigs {
		config, err := p.loadRetained(ctx, namespace, name, uid, ref, nil)
		if err != nil {
			return nil, fmt.Errorf("HAProxyCfg %s/%s: %w", namespace, ref.Name, err)
		}
		result = append(result, *config)
	}
	latest, err := client.Get(ctx, current.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if latest.UID != current.UID || !ownedByTemplate(latest, name, uid) || !slices.Equal(latest.Status.RetainedConfigs, current.Status.RetainedConfigs) {
		return nil, errors.New("retained checkpoint changed during recovery")
	}
	return result, nil
}

func (p *Publisher) loadRetained(ctx context.Context, namespace, name string, uid types.UID, ref v1.RetainedConfigReference, listing *childListing) (*RetainedConfig, error) {
	item, err := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if !ownedByTemplate(item, name, uid) || ref.UID == "" || string(item.UID) != ref.UID || item.DeletionTimestamp != nil {
		return nil, errors.New("retained configuration owner or UID differs")
	}
	if item.Spec.RetainedPlan == "" || item.Spec.Checksum != ref.Checksum ||
		item.Name != runtimeConfigResourceName(name, retainedSuffix(item.Spec.Checksum, item.Spec.RetainedPlan)) {
		return nil, errors.New("retained configuration content address differs")
	}
	config, err := decodeRetainedContent(item.Spec.Content, item.Spec.Compressed)
	if err != nil {
		return nil, err
	}
	output, authority, err := p.readRetainedOutput(ctx, item, config, listing)
	if err != nil {
		return nil, err
	}
	checksum, err := output.ContentChecksum()
	if err != nil {
		return nil, err
	}
	if checksum != item.Spec.Checksum {
		return nil, errors.New("retained configuration checksum differs from its content")
	}
	planID, err := output.PlanID()
	if err != nil {
		return nil, err
	}
	for i := range item.Status.DeployedToPods {
		pod := &item.Status.DeployedToPods[i]
		if confirmsOutput(pod, checksum, planID) {
			return &RetainedConfig{Reference: ref, Output: output, Authority: authority, ConfirmedPod: *pod}, nil
		}
	}
	return nil, errors.New("retained configuration has no confirmed worker receipt")
}

func decodeRetainedContent(content string, compressed bool) (string, error) {
	if compressed {
		return compression.Decompress(content)
	}
	return content, nil
}

func sealRetainedOutput(config string, payload *retainedPayload, files map[retainedFileKey]retainedFile) (*renderoutput.Snapshot, *renderoutput.Authority, error) {
	authority := renderartifact.NewAuthority()
	builder, err := renderartifact.NewBuilder(authority, nil)
	if err != nil {
		return nil, nil, err
	}
	contents := map[string]string{renderplan.ConfigFilePath: config}
	for _, descriptor := range payload.Artifacts {
		key := retainedFileKey{family: descriptor.Family, path: descriptor.Path}
		file, found := files[key]
		if !found || (descriptor.Family == renderartifact.General || descriptor.Family == renderartifact.GeneralCA) && descriptor.Name != file.name ||
			descriptor.Seal != file.seal {
			return nil, nil, fmt.Errorf("retained artifact %q is missing or differs", descriptor.Path)
		}
		if _, duplicate := contents[descriptor.RuntimePath]; duplicate {
			return nil, nil, errors.New("retained artifact repeats a runtime path")
		}
		contents[descriptor.RuntimePath] = file.content
		if err := builder.Add(descriptor, renderartifact.NewLiteralContent(file.content)); err != nil {
			return nil, nil, err
		}
		delete(files, key)
	}
	if len(files) != 0 {
		return nil, nil, errors.New("retained auxiliary references contain undeclared files")
	}
	artifacts, err := builder.Build()
	if err != nil {
		return nil, nil, err
	}
	plan, err := planblob.DecodeCheckpoint(payload.Plan, config, contents)
	if err != nil {
		return nil, nil, err
	}
	outputAuthority, err := renderoutput.NewAuthority(renderplan.NewAuthority(), authority)
	if err != nil {
		return nil, nil, err
	}
	output, err := renderoutput.NewSnapshot(outputAuthority, config, plan, artifacts, nil)
	return output, outputAuthority, err
}

func (p *Publisher) readRetainedOutput(ctx context.Context, item *v1.HAProxyCfg, config string, listing *childListing) (*renderoutput.Snapshot, *renderoutput.Authority, error) {
	data, err := compression.Decompress(item.Spec.RetainedPlan)
	if err != nil {
		return nil, nil, err
	}
	var payload retainedPayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return nil, nil, err
	}
	files, err := p.readRetainedFiles(ctx, item, listing)
	if err != nil {
		return nil, nil, err
	}
	output, authority, err := sealRetainedOutput(config, &payload, files)
	if err != nil {
		return nil, nil, err
	}
	canonical, err := canonicalizePublishRequest(&PublishRequest{OutputSnapshot: output})
	if err != nil {
		return nil, nil, err
	}
	if canonical.auxiliarySetID != item.Status.AuxiliaryFiles.SetID || canonical.auxiliarySetID != item.Annotations[AuxiliarySetIDAnnotationKey] {
		return nil, nil, errors.New("retained auxiliary set differs from its content address")
	}
	if dataplane.ComputeContentChecksum(canonical.Config, &dataplane.AuxiliaryFiles{
		MapFiles: canonical.AuxiliaryFiles.MapFiles, SSLCertificates: canonical.AuxiliaryFiles.SSLCertificates,
		SSLCaFiles: canonical.AuxiliaryFiles.SSLCaFiles, GeneralFiles: canonical.AuxiliaryFiles.GeneralFiles, CRTListFiles: canonical.AuxiliaryFiles.CRTListFiles,
		SecretFiles: canonical.AuxiliaryFiles.SecretFiles,
	}) != item.Spec.Checksum {
		return nil, nil, errors.New("retained configuration checksum differs from its content")
	}
	return output, authority, nil
}
