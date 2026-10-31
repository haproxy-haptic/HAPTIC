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
	"fmt"
	"slices"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
)

func desiredResourceNames(names []string) map[string]struct{} {
	desired := make(map[string]struct{}, len(names))
	for _, name := range names {
		desired[name] = struct{}{}
	}
	return desired
}

func referencesRuntimeConfig(obj metav1.Object, runtimeConfig *haproxyv1alpha1.HAProxyCfg) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == apiVersionV1Alpha1 && ref.Kind == runtimeConfigKind &&
			ref.Name == runtimeConfig.Name && ref.UID == runtimeConfig.UID {
			return true
		}
	}
	return false
}

func managedByRuntimeConfig(obj metav1.Object, runtimeConfigName string) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion != apiVersionV1Alpha1 || ref.Kind != runtimeConfigKind {
			continue
		}
		if ref.Name == runtimeConfigName {
			return true
		}
		return false
	}
	return len(obj.GetOwnerReferences()) == 0 &&
		obj.GetLabels()[runtimeConfigLabelKey] == runtimeConfigLabelValue(runtimeConfigName)
}

func ownedByRuntimeConfig(obj metav1.Object, runtimeConfig *haproxyv1alpha1.HAProxyCfg) bool {
	return obj.GetLabels()[runtimeConfigLabelKey] == runtimeConfigLabelValue(runtimeConfig.Name) &&
		referencesRuntimeConfig(obj, runtimeConfig)
}

func (p *Publisher) pruneOwnedResources(
	ctx context.Context,
	runtimeConfig *haproxyv1alpha1.HAProxyCfg,
	kind, description string,
	desiredNames []string,
	children []listedChild,
	publicationCurrent func(context.Context) error,
) error {
	desired := desiredResourceNames(desiredNames)
	for _, child := range children {
		obj := child.obj
		if _, keep := desired[obj.GetName()]; keep || !ownedByRuntimeConfig(obj, runtimeConfig) {
			continue
		}
		remove := func(ctx context.Context, name string, options metav1.DeleteOptions) error {
			err := child.remove(ctx, name, options)
			if kind != kindSecret {
				p.forgetAuxFileStampOnDelete(err, kind, runtimeConfig.Namespace, name)
			}
			return err
		}
		if err := deleteOwnedResource(ctx, runtimeConfig, obj, publicationCurrent, child.get, remove); err != nil {
			return cleanupError(runtimeConfig, kind, obj.GetName(), fmt.Errorf("deleting stale %s: %w", description, err))
		}
	}
	return nil
}

func deleteOwnedResource(
	ctx context.Context,
	runtimeConfig *haproxyv1alpha1.HAProxyCfg,
	listed metav1.Object,
	publicationCurrent func(context.Context) error,
	getResource func(context.Context, string) (metav1.Object, error),
	deleteResource func(context.Context, string, metav1.DeleteOptions) error,
) error {
	current := listed
	refresh := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if refresh {
			var err error
			current, err = getResource(ctx, listed.GetName())
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("refreshing deletion candidate: %w", err)
			}
		}
		refresh = true
		// Status writes can race cleanup; a retry must not adopt a replacement or a new owner.
		if current.GetUID() != listed.GetUID() || !ownedByRuntimeConfig(current, runtimeConfig) ||
			current.GetAnnotations()[auxiliaryClaimAnnotationKey] != listed.GetAnnotations()[auxiliaryClaimAnnotationKey] {
			return nil
		}
		if err := publicationCurrent(ctx); err != nil {
			return err
		}
		err := deleteResource(ctx, current.GetName(), deletionOptions(current))
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	})
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return err
}

func deletionOptions(obj metav1.Object) metav1.DeleteOptions {
	preconditions := &metav1.Preconditions{}
	hasPrecondition := false
	if uid := obj.GetUID(); uid != "" {
		preconditions.UID = &uid
		hasPrecondition = true
	}
	if resourceVersion := obj.GetResourceVersion(); resourceVersion != "" {
		preconditions.ResourceVersion = &resourceVersion
		hasPrecondition = true
	}
	if !hasPrecondition {
		return metav1.DeleteOptions{}
	}
	return metav1.DeleteOptions{Preconditions: preconditions}
}

func (p *Publisher) ensurePublicationCurrent(
	ctx context.Context,
	runtimeConfig *haproxyv1alpha1.HAProxyCfg,
	expectedReferences *haproxyv1alpha1.AuxiliaryFileReferences,
) (*haproxyv1alpha1.HAProxyCfg, error) {
	current, err := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs(runtimeConfig.Namespace).
		Get(ctx, runtimeConfig.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("checking publication authority: %w", err)
	}
	if err := validateRuntimePublication(runtimeConfig, current); err != nil {
		return nil, err
	}
	want := runtimeConfig.Annotations[AuxiliarySetIDAnnotationKey]
	if current.Annotations[AuxiliarySetIDAnnotationKey] != want {
		return nil, fmt.Errorf("publication %q was superseded; skip stale cleanup", want)
	}
	if !auxiliaryRefsEqual(current.Status.AuxiliaryFiles, expectedReferences) {
		return nil, fmt.Errorf("publication %q no longer owns the committed auxiliary references; skip stale cleanup", want)
	}
	return current, nil
}

// publicationFence runs ensurePublicationCurrent before every stale-child
// deletion. A HAProxyCfg whose resourceVersion has not moved since it last
// passed is the object that passed, so the re-check then reads only metadata
// instead of the whole rendered config.
type publicationFence struct {
	publisher       *Publisher
	runtimeConfig   *haproxyv1alpha1.HAProxyCfg
	expected        *haproxyv1alpha1.AuxiliaryFileReferences
	verifiedVersion string
}

func (f *publicationFence) check(ctx context.Context) error {
	if f.verifiedVersion != "" && f.publisher.resourceVersionOf != nil {
		version, err := f.publisher.resourceVersionOf(ctx, f.runtimeConfig.Namespace, f.runtimeConfig.Name)
		if err == nil && version == f.verifiedVersion {
			return nil
		}
	}
	current, err := f.publisher.ensurePublicationCurrent(ctx, f.runtimeConfig, f.expected)
	if err != nil {
		f.verifiedVersion = ""
		return err
	}
	f.verifiedVersion = current.ResourceVersion
	return nil
}

func runtimeConfigSelector(runtimeConfigName string) string {
	return labels.Set{runtimeConfigLabelKey: runtimeConfigLabelValue(runtimeConfigName)}.AsSelector().String()
}

// pruneAuxiliaryFiles deletes owned children that the committed references
// left out. Children are judged from the listing taken before publication;
// one created after it is judged by the next publication.
func (p *Publisher) pruneAuxiliaryFiles(ctx context.Context, runtimeConfig *haproxyv1alpha1.HAProxyCfg, result *PublishResult, listing *childListing) error {
	expectedReferences := buildAuxiliaryFileReferences(
		runtimeConfig.Namespace,
		result,
		runtimeConfig.Annotations[AuxiliarySetIDAnnotationKey],
	)
	fence := &publicationFence{publisher: p, runtimeConfig: runtimeConfig, expected: expectedReferences}
	if err := fence.check(ctx); err != nil {
		return cleanupError(runtimeConfig, runtimeConfigKind, runtimeConfig.Name, err)
	}
	if err := p.listInto(ctx, listing, auxiliaryChildKinds); err != nil {
		return cleanupError(runtimeConfig, runtimeConfigKind, runtimeConfig.Name, err)
	}
	stale := []struct {
		kind, description string
		desired           []string
	}{
		{kindMapFile, mapFileDescription, result.MapFileNames},
		{kindSecret, "file Secret", slices.Concat(result.SecretNames, result.SSLCaFileNames, result.SecretFileNames)},
		{kindGeneralFile, generalFileDescription, result.GeneralFileNames},
		{kindCRTListFile, crtListFileDescription, result.CRTListFileNames},
	}
	for _, group := range stale {
		if err := p.pruneOwnedResources(ctx, runtimeConfig, group.kind, group.description, group.desired,
			listing.byKind[group.kind], fence.check); err != nil {
			return err
		}
	}
	return nil
}

func cleanupError(runtimeConfig *haproxyv1alpha1.HAProxyCfg, kind, name string, err error) error {
	return incompletePublicationError(
		PublicationStageCleanup,
		runtimeConfig.Namespace,
		runtimeConfig.Name,
		kind,
		name,
		err,
	)
}
