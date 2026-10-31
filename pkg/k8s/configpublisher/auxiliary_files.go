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
	"path"
	"strconv"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

const (
	runtimeConfigLabelKey = "haproxy-haptic.org/runtime-config"
	// AuxiliarySetIDAnnotationKey identifies one immutable auxiliary publication.
	AuxiliarySetIDAnnotationKey = "haproxy-haptic.org/auxiliary-set-id"
	// AuxiliaryChecksumAnnotationKey identifies the content stored in an auxiliary Secret.
	AuxiliaryChecksumAnnotationKey = "haproxy-haptic.org/checksum"
)

// runtimeConfigOwnerRefs builds the OwnerReferences slice that ties an
// auxiliary-file CRD (or Secret) to its parent HAProxyCfg. All four
// createOrUpdate* helpers in this file use the same shape.
func runtimeConfigOwnerRefs(owner *haproxyv1alpha1.HAProxyCfg) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		{
			APIVersion:         apiVersionV1Alpha1,
			Kind:               runtimeConfigKind,
			Name:               owner.Name,
			UID:                owner.UID,
			Controller:         new(true),
			BlockOwnerDeletion: new(true),
		},
	}
}

// auxiliaryChildOwnership is the metadata one publication stamps on a child.
type auxiliaryChildOwnership struct {
	labels          map[string]string
	annotations     map[string]string
	ownerReferences []metav1.OwnerReference
	manages         func(metav1.Object) bool
}

func childOwnership(req *PublishRequest, owner *haproxyv1alpha1.HAProxyCfg) auxiliaryChildOwnership {
	if req.RetainedPlan != "" {
		return retainedChildOwnership(req)
	}
	return auxiliaryChildOwnership{
		labels:          runtimeConfigLabels(owner),
		annotations:     auxiliaryClaimAnnotations(owner, req.auxiliaryClaim),
		ownerReferences: runtimeConfigOwnerRefs(owner),
		manages:         func(existing metav1.Object) bool { return managedByRuntimeConfig(existing, owner.Name) },
	}
}

// runtimeConfigLabels returns the standard label map that pins an auxiliary
// resource to its parent HAProxyCfg via the runtime-config label.
func runtimeConfigLabels(owner *haproxyv1alpha1.HAProxyCfg) map[string]string {
	return map[string]string{
		runtimeConfigLabelKey: runtimeConfigLabelValue(owner.Name),
	}
}

// RuntimeConfigLabelSelector selects children owned by one HAProxyCfg.
func RuntimeConfigLabelSelector(runtimeConfigName string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{
		runtimeConfigLabelKey: runtimeConfigLabelValue(runtimeConfigName),
	}}
}

func runtimeConfigAnnotations(owner *haproxyv1alpha1.HAProxyCfg) map[string]string {
	return map[string]string{
		AuxiliarySetIDAnnotationKey: owner.Annotations[AuxiliarySetIDAnnotationKey],
	}
}

func auxiliaryMetadataEqual(
	existing metav1.Object,
	labels, annotations map[string]string,
	ownerReferences []metav1.OwnerReference,
) bool {
	return apiequality.Semantic.DeepEqual(existing.GetLabels(), labels) &&
		apiequality.Semantic.DeepEqual(existing.GetAnnotations(), annotations) &&
		apiequality.Semantic.DeepEqual(existing.GetOwnerReferences(), ownerReferences)
}

// retriableWrite reports whether a create/update error is a transient write
// conflict worth retrying within retry.OnError:
//
//   - a resourceVersion Conflict (409, the case retry.RetryOnConflict covers), or
//   - an AlreadyExists (409 with a different reason): a racing writer created the
//     object between our Get-returns-NotFound and our Create. retry.RetryOnConflict
//     does NOT cover this (it only matches IsConflict), so the "retry to update"
//     create paths below use this predicate to actually re-Get and take the update
//     branch instead of surfacing the AlreadyExists to the caller.
func retriableWrite(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err)
}

// auxResourceOps abstracts one auxiliary resource kind (map file, SSL secret,
// general file, crt-list file) for createOrUpdateAuxResource. The closures
// capture the typed client, the precomputed name/spec/labels, and the
// kind-specific checksum location.
type auxResourceOps[T metav1.Object] struct {
	// kind names the resource in error messages (e.g. "map file").
	kind string
	// get fetches the current object (called inside the retry loop so every
	// attempt sees a fresh resourceVersion).
	get func(ctx context.Context) (T, error)
	// create builds and creates the new object.
	create func(ctx context.Context) (T, error)
	// upToDate reports whether the existing object already carries the
	// desired content and ownership metadata.
	upToDate func(existing T) bool
	// managedByOwner reports whether an existing name belongs to this
	// HAProxyCfg and may be updated.
	managedByOwner func(existing T) bool
	// update mutates the freshly fetched object with the desired state and
	// writes it back.
	update func(ctx context.Context, existing T) (T, error)
	// onCreate, if set, runs with the created object's name after a fresh
	// Create succeeds. Aux-file kinds use it to evict the stamp cache: a fresh
	// CR has an empty status.deployedToPods, so any cached stamp for that name
	// is stale and would elide the re-stamp. This covers every delete cause —
	// the prune path, an apiserver owner-GC cascade (kubectl delete haproxycfg /
	// the parent HAProxyTemplateConfig), or a foreign delete — since all of them
	// end in a fresh Create here.
	onCreate func(name string)
}

type auxiliaryResourceOwnershipError struct {
	kind string
	name string
}

func (e *auxiliaryResourceOwnershipError) Error() string {
	return fmt.Sprintf("%s %q is managed by another HAProxyCfg", e.kind, e.name)
}

// createAbsentAuxResource creates the resource when the get returned NotFound,
// running ops.onCreate on success. A returned AlreadyExists is left unwrapped so
// retriableWrite re-drives the loop into the update branch.
func createAbsentAuxResource[T metav1.Object](ctx context.Context, ops auxResourceOps[T]) (string, error) {
	created, err := ops.create(ctx)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return "", err
		}
		return "", fmt.Errorf("creating %s: %w", ops.kind, err)
	}
	name := created.GetName()
	if ops.onCreate != nil {
		ops.onCreate(name)
	}
	return name, nil
}

// createOrUpdateAuxResource runs the shared create-or-update workflow for an
// auxiliary resource: get → create when absent (retrying a lost create race
// into the update branch) → skip when the desired state is unchanged → update.
// Returns the name of the resource that ends up holding the desired state.
func createOrUpdateAuxResource[T metav1.Object](ctx context.Context, ops auxResourceOps[T]) (string, error) {
	var resultName string
	err := retry.OnError(retry.DefaultRetry, retriableWrite, func() error {
		existing, err := ops.get(ctx)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("getting existing %s: %w", ops.kind, err)
			}
			name, createErr := createAbsentAuxResource(ctx, ops)
			resultName = name
			return createErr
		}
		if existing.GetDeletionTimestamp() != nil {
			return fmt.Errorf("%s %q is terminating; retry publication after deletion", ops.kind, existing.GetName())
		}
		if !ops.managedByOwner(existing) {
			return &auxiliaryResourceOwnershipError{kind: ops.kind, name: existing.GetName()}
		}

		// Skip update if the desired state hasn't changed.
		if ops.upToDate(existing) {
			resultName = existing.GetName()
			return nil
		}

		updated, updateErr := ops.update(ctx, existing)
		if updateErr != nil {
			return fmt.Errorf("updating %s: %w", ops.kind, updateErr)
		}

		resultName = updated.GetName()
		return nil
	})
	if err != nil {
		return "", err
	}
	if resultName == "" {
		// See createOrUpdateRuntimeConfig: retry.OnError turns an interrupting
		// context cancellation into a nil error when no retriable error preceded it.
		return "", fmt.Errorf("creating or updating %s: %w", ops.kind, interruptedErr(ctx))
	}
	return resultName, nil
}

// createOrUpdateMapFile creates or updates a HAProxyMapFile resource.
func (p *Publisher) createOrUpdateMapFile(ctx context.Context, req *PublishRequest, owner *haproxyv1alpha1.HAProxyCfg, mapFile auxiliaryfiles.MapFile, name string) (string, error) {
	checksum := calculateChecksum(mapFile.Content) // Checksum of original content

	// Compress if content exceeds threshold
	result := p.compressIfNeeded(mapFile.Content, req.CompressionThreshold, "HAProxyMapFile/"+name)

	spec := haproxyv1alpha1.HAProxyMapFileSpec{
		Empty:      mapFile.Content == "",
		MapName:    path.Base(mapFile.Path),
		Path:       mapFile.Path,
		Entries:    result.content,
		Checksum:   checksum,
		Compressed: result.compressed,
	}
	ownership := childOwnership(req, owner)
	labels, annotations, ownerReferences := ownership.labels, ownership.annotations, ownership.ownerReferences
	client := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles(req.TemplateConfigNamespace)

	read := listedFirst(req.listedChildren, kindMapFile, name, client.Get)
	return createOrUpdateAuxResource(ctx, auxResourceOps[*haproxyv1alpha1.HAProxyMapFile]{
		kind: mapFileDescription,
		get: func(ctx context.Context) (*haproxyv1alpha1.HAProxyMapFile, error) {
			existing, err := read(ctx)
			if err == nil {
				retainReferencedClaim(existing, annotations, owner, kindMapFile)
			}
			return existing, err
		},
		create: func(ctx context.Context) (*haproxyv1alpha1.HAProxyMapFile, error) {
			return client.Create(ctx, &haproxyv1alpha1.HAProxyMapFile{
				ObjectMeta: metav1.ObjectMeta{
					Name:            name,
					Namespace:       req.TemplateConfigNamespace,
					Labels:          labels,
					Annotations:     annotations,
					OwnerReferences: ownerReferences,
				},
				Spec: spec,
			}, metav1.CreateOptions{})
		},
		upToDate: func(existing *haproxyv1alpha1.HAProxyMapFile) bool {
			return apiequality.Semantic.DeepEqual(existing.Spec, spec) &&
				auxiliaryMetadataEqual(existing, labels, annotations, ownerReferences)
		},
		managedByOwner: func(existing *haproxyv1alpha1.HAProxyMapFile) bool { return ownership.manages(existing) },
		update: func(ctx context.Context, existing *haproxyv1alpha1.HAProxyMapFile) (*haproxyv1alpha1.HAProxyMapFile, error) {
			existing.Spec = spec
			existing.Labels = labels
			existing.Annotations = annotations
			existing.OwnerReferences = ownerReferences
			return client.Update(ctx, existing, metav1.UpdateOptions{})
		},
		onCreate: func(createdName string) {
			p.statusStamps.forgetAuxFile(kindMapFile, req.TemplateConfigNamespace, createdName)
		},
	})
}

// createOrUpdateSSLSecret creates or updates a Secret for SSL certificates.
func (p *Publisher) createOrUpdateSSLSecret(ctx context.Context, req *PublishRequest, owner *haproxyv1alpha1.HAProxyCfg, cert auxiliaryfiles.SSLCertificate, name string) (string, error) {
	return p.createOrUpdateSSLFileSecret(ctx, req, owner, name, cert.Path, cert.Content, "certificate", "ssl-certificate")
}

func (p *Publisher) createOrUpdateSSLCASecret(ctx context.Context, req *PublishRequest, owner *haproxyv1alpha1.HAProxyCfg, ca auxiliaryfiles.SSLCaFile, name string) (string, error) {
	return p.createOrUpdateSSLFileSecret(ctx, req, owner, name, ca.Path, ca.Content, "ca", "ssl-ca")
}

func (p *Publisher) createOrUpdateSSLFileSecret(
	ctx context.Context,
	req *PublishRequest,
	owner *haproxyv1alpha1.HAProxyCfg,
	name, filePath, content, dataKey, fileType string,
) (string, error) {
	result := p.compressIfNeeded(content, req.CompressionThreshold, "Secret/"+name)
	return p.createOrUpdateFileSecret(ctx, req, owner, name, filePath, fileType, calculateChecksum(content), result.compressed,
		map[string][]byte{dataKey: []byte(result.content), "path": []byte(filePath)})
}

// Secret-file Secrets keep the content checksum in data: as an annotation it
// would be readable without Secret access.
const (
	secretFileType            = "secret-file"
	secretFileDataKey         = "file"
	secretFileChecksumDataKey = "checksum"
)

func (p *Publisher) createOrUpdateSecretFileSecret(ctx context.Context, req *PublishRequest, owner *haproxyv1alpha1.HAProxyCfg, file auxiliaryfiles.SecretFile, name string) (string, error) {
	result := p.compressIfNeeded(file.Content, req.CompressionThreshold, "Secret/"+name)
	return p.createOrUpdateFileSecret(ctx, req, owner, name, file.Path, secretFileType, identityChecksum(file), result.compressed,
		map[string][]byte{
			secretFileDataKey:         []byte(result.content),
			"path":                    []byte(file.Path),
			secretFileChecksumDataKey: []byte(calculateChecksum(file.Content)),
		})
}

func (p *Publisher) createOrUpdateFileSecret(
	ctx context.Context,
	req *PublishRequest,
	owner *haproxyv1alpha1.HAProxyCfg,
	name, filePath, fileType, checksum string,
	compressed bool,
	data map[string][]byte,
) (string, error) {
	ownership := childOwnership(req, owner)
	labels, annotations, ownerReferences := ownership.labels, ownership.annotations, ownership.ownerReferences
	labels["haproxy-haptic.org/type"] = fileType
	annotations["haproxy-haptic.org/compressed"] = strconv.FormatBool(compressed)
	annotations[AuxiliaryChecksumAnnotationKey] = checksum
	annotations[AuxiliaryPathAnnotationKey] = filePath
	client := p.k8sClient.CoreV1().Secrets(req.TemplateConfigNamespace)

	read := listedFirst(req.listedChildren, kindSecret, name, client.Get)
	return createOrUpdateAuxResource(ctx, auxResourceOps[*corev1.Secret]{
		kind: "secret",
		get: func(ctx context.Context) (*corev1.Secret, error) {
			existing, err := read(ctx)
			if err == nil {
				retainReferencedClaim(existing, annotations, owner, "Secret")
			}
			return existing, err
		},
		create: func(ctx context.Context) (*corev1.Secret, error) {
			return client.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:            name,
					Namespace:       req.TemplateConfigNamespace,
					Labels:          labels,
					Annotations:     annotations,
					OwnerReferences: ownerReferences,
				},
				Type: corev1.SecretTypeOpaque,
				Data: data,
			}, metav1.CreateOptions{})
		},
		upToDate: func(existing *corev1.Secret) bool {
			return existing.Type == corev1.SecretTypeOpaque &&
				apiequality.Semantic.DeepEqual(existing.Data, data) &&
				auxiliaryMetadataEqual(existing, labels, annotations, ownerReferences)
		},
		managedByOwner: func(existing *corev1.Secret) bool { return ownership.manages(existing) },
		update: func(ctx context.Context, existing *corev1.Secret) (*corev1.Secret, error) {
			existing.Data = data
			existing.Labels = labels
			existing.Annotations = annotations
			existing.OwnerReferences = ownerReferences
			existing.Type = corev1.SecretTypeOpaque
			return client.Update(ctx, existing, metav1.UpdateOptions{})
		},
	})
}

// createOrUpdateGeneralFile creates or updates a HAProxyGeneralFile resource.
func (p *Publisher) createOrUpdateGeneralFile(ctx context.Context, req *PublishRequest, owner *haproxyv1alpha1.HAProxyCfg, generalFile auxiliaryfiles.GeneralFile, name string) (string, error) {
	checksum := calculateChecksum(generalFile.Content) // Checksum of original content

	// Compress if content exceeds threshold
	result := p.compressIfNeeded(generalFile.Content, req.CompressionThreshold, "HAProxyGeneralFile/"+name)

	spec := haproxyv1alpha1.HAProxyGeneralFileSpec{
		Empty:      generalFile.Content == "",
		FileName:   generalFile.Filename,
		Path:       generalFile.Path,
		Content:    result.content,
		Checksum:   checksum,
		Compressed: result.compressed,
		CAFile:     generalFile.IsCaFile,
	}
	ownership := childOwnership(req, owner)
	labels, annotations, ownerReferences := ownership.labels, ownership.annotations, ownership.ownerReferences
	client := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles(req.TemplateConfigNamespace)

	read := listedFirst(req.listedChildren, kindGeneralFile, name, client.Get)
	return createOrUpdateAuxResource(ctx, auxResourceOps[*haproxyv1alpha1.HAProxyGeneralFile]{
		kind: generalFileDescription,
		get: func(ctx context.Context) (*haproxyv1alpha1.HAProxyGeneralFile, error) {
			existing, err := read(ctx)
			if err == nil {
				retainReferencedClaim(existing, annotations, owner, kindGeneralFile)
			}
			return existing, err
		},
		create: func(ctx context.Context) (*haproxyv1alpha1.HAProxyGeneralFile, error) {
			return client.Create(ctx, &haproxyv1alpha1.HAProxyGeneralFile{
				ObjectMeta: metav1.ObjectMeta{
					Name:            name,
					Namespace:       req.TemplateConfigNamespace,
					Labels:          labels,
					Annotations:     annotations,
					OwnerReferences: ownerReferences,
				},
				Spec: spec,
			}, metav1.CreateOptions{})
		},
		upToDate: func(existing *haproxyv1alpha1.HAProxyGeneralFile) bool {
			return apiequality.Semantic.DeepEqual(existing.Spec, spec) &&
				auxiliaryMetadataEqual(existing, labels, annotations, ownerReferences)
		},
		managedByOwner: func(existing *haproxyv1alpha1.HAProxyGeneralFile) bool { return ownership.manages(existing) },
		update: func(ctx context.Context, existing *haproxyv1alpha1.HAProxyGeneralFile) (*haproxyv1alpha1.HAProxyGeneralFile, error) {
			existing.Spec = spec
			existing.Labels = labels
			existing.Annotations = annotations
			existing.OwnerReferences = ownerReferences
			return client.Update(ctx, existing, metav1.UpdateOptions{})
		},
		onCreate: func(createdName string) {
			p.statusStamps.forgetAuxFile(kindGeneralFile, req.TemplateConfigNamespace, createdName)
		},
	})
}

// createOrUpdateCRTListFile creates or updates a HAProxyCRTListFile resource.
func (p *Publisher) createOrUpdateCRTListFile(ctx context.Context, req *PublishRequest, owner *haproxyv1alpha1.HAProxyCfg, crtListFile auxiliaryfiles.CRTListFile, name string) (string, error) {
	checksum := calculateChecksum(crtListFile.Content) // Checksum of original content

	// Compress if content exceeds threshold
	result := p.compressIfNeeded(crtListFile.Content, req.CompressionThreshold, "HAProxyCRTListFile/"+name)

	spec := haproxyv1alpha1.HAProxyCRTListFileSpec{
		Empty:      crtListFile.Content == "",
		ListName:   path.Base(crtListFile.Path),
		Path:       crtListFile.Path,
		Entries:    result.content,
		Checksum:   checksum,
		Compressed: result.compressed,
	}
	ownership := childOwnership(req, owner)
	labels, annotations, ownerReferences := ownership.labels, ownership.annotations, ownership.ownerReferences
	client := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCRTListFiles(req.TemplateConfigNamespace)

	read := listedFirst(req.listedChildren, kindCRTListFile, name, client.Get)
	return createOrUpdateAuxResource(ctx, auxResourceOps[*haproxyv1alpha1.HAProxyCRTListFile]{
		kind: crtListFileDescription,
		get: func(ctx context.Context) (*haproxyv1alpha1.HAProxyCRTListFile, error) {
			existing, err := read(ctx)
			if err == nil {
				retainReferencedClaim(existing, annotations, owner, kindCRTListFile)
			}
			return existing, err
		},
		create: func(ctx context.Context) (*haproxyv1alpha1.HAProxyCRTListFile, error) {
			return client.Create(ctx, &haproxyv1alpha1.HAProxyCRTListFile{
				ObjectMeta: metav1.ObjectMeta{
					Name:            name,
					Namespace:       req.TemplateConfigNamespace,
					Labels:          labels,
					Annotations:     annotations,
					OwnerReferences: ownerReferences,
				},
				Spec: spec,
			}, metav1.CreateOptions{})
		},
		upToDate: func(existing *haproxyv1alpha1.HAProxyCRTListFile) bool {
			return apiequality.Semantic.DeepEqual(existing.Spec, spec) &&
				auxiliaryMetadataEqual(existing, labels, annotations, ownerReferences)
		},
		managedByOwner: func(existing *haproxyv1alpha1.HAProxyCRTListFile) bool { return ownership.manages(existing) },
		update: func(ctx context.Context, existing *haproxyv1alpha1.HAProxyCRTListFile) (*haproxyv1alpha1.HAProxyCRTListFile, error) {
			existing.Spec = spec
			existing.Labels = labels
			existing.Annotations = annotations
			existing.OwnerReferences = ownerReferences
			return client.Update(ctx, existing, metav1.UpdateOptions{})
		},
		onCreate: func(createdName string) {
			p.statusStamps.forgetAuxFile(kindCRTListFile, req.TemplateConfigNamespace, createdName)
		},
	})
}
