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
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
)

// retainedChildSuffix marks children shared by every checkpoint of one
// template. Checkpoints published before sharing own their children and name
// them with their own retainedSuffix instead.
const retainedChildSuffix = "-retained"

func retainedChildOwnership(req *PublishRequest) auxiliaryChildOwnership {
	return auxiliaryChildOwnership{
		labels:          map[string]string{retainedLabel: string(req.TemplateConfigUID)},
		annotations:     map[string]string{},
		ownerReferences: templateConfigOwnerRefs(req),
		manages: func(existing metav1.Object) bool {
			return ownedByRetainedTemplate(existing, req.TemplateConfigName, req.TemplateConfigUID)
		},
	}
}

func ownedByRetainedTemplate(obj metav1.Object, name string, uid types.UID) bool {
	return obj.GetLabels()[retainedLabel] == string(uid) && ownedByTemplate(obj, name, uid)
}

func retainedSelector(uid types.UID) string {
	return retainedLabel + "=" + string(uid)
}

// pruneRetainedFiles deletes shared children that no checkpoint references.
// Children listed before the references were read are safe to judge against
// them; ones created later are judged by the next prune.
func (p *Publisher) pruneRetainedFiles(ctx context.Context, req *PublishRequest, listing *childListing) error {
	referenced, committed, err := p.retainedFileReferences(ctx, req, retainedSelector(req.TemplateConfigUID))
	if err != nil || !committed {
		return err
	}
	if err := p.listInto(ctx, listing, auxiliaryChildKinds); err != nil {
		return err
	}
	for _, kind := range auxiliaryChildKinds {
		for _, child := range listing.byKind[kind] {
			if referenced[kind+"/"+child.obj.GetName()] ||
				!ownedByRetainedTemplate(child.obj, req.TemplateConfigName, req.TemplateConfigUID) {
				continue
			}
			err := child.remove(ctx, child.obj.GetName(), deletionOptions(child.obj))
			if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return err
			}
		}
	}
	return nil
}

// retainedFileReferences returns every child the template's checkpoints
// reference; committed is false while a checkpoint's references are unknown.
func (p *Publisher) retainedFileReferences(ctx context.Context, req *PublishRequest, selector string) (referenced map[string]bool, committed bool, err error) {
	checkpoints, err := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs(req.TemplateConfigNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, false, err
	}
	referenced = make(map[string]bool)
	for i := range checkpoints.Items {
		item := &checkpoints.Items[i]
		if !ownedByTemplate(item, req.TemplateConfigName, req.TemplateConfigUID) || item.DeletionTimestamp != nil {
			continue
		}
		refs, ok := committedReferences(item)
		if !ok {
			return nil, false, nil
		}
		for _, group := range [][]v1.ResourceReference{refs.MapFiles, refs.GeneralFiles, refs.CRTListFiles, refs.SSLCertificates, refs.SSLCaFiles, refs.SecretFiles} {
			for _, ref := range group {
				referenced[ref.Kind+"/"+ref.Name] = true
			}
		}
	}
	return referenced, true, nil
}

type retainedFileKey struct {
	family renderartifact.Family
	path   string
}

type retainedFile struct {
	meta    metav1.Object
	key     retainedFileKey
	name    string
	content string
	// checksum is the identity checksum names carry; contentChecksum, when
	// set, is the content's own one, which a secret file keeps out of sight.
	checksum        string
	contentChecksum string
	seal            string
	compressed      bool
	empty           bool
}

type retainedFileGroup struct {
	kind   string
	family renderartifact.Family
	refs   []v1.ResourceReference
}

func (p *Publisher) readRetainedFiles(ctx context.Context, owner *v1.HAProxyCfg, listing *childListing) (map[retainedFileKey]retainedFile, error) {
	refs := owner.Status.AuxiliaryFiles
	if refs == nil || !UsesContentIdentity(refs.SetID) {
		return nil, errors.New("retained configuration has no committed content-addressed auxiliary set")
	}
	groups := []retainedFileGroup{
		{kindMapFile, renderartifact.Map, refs.MapFiles},
		{kindGeneralFile, renderartifact.General, refs.GeneralFiles},
		{kindCRTListFile, renderartifact.CRTList, refs.CRTListFiles},
		{kindSecret, renderartifact.Certificate, refs.SSLCertificates},
		{kindSecret, renderartifact.CA, refs.SSLCaFiles},
		{kindSecret, renderartifact.Secret, refs.SecretFiles},
	}
	listing, err := p.listSharedRetainedFiles(ctx, owner, groups, listing)
	if err != nil {
		return nil, err
	}
	files := make(map[retainedFileKey]retainedFile)
	seen := make(map[string]bool)
	for _, group := range groups {
		for _, ref := range group.refs {
			file, err := p.readRetainedReference(ctx, owner, ref, group.kind, group.family, seen, listing)
			if err != nil {
				return nil, err
			}
			if _, duplicate := files[file.key]; duplicate {
				return nil, errors.New("retained auxiliary set repeats a file path")
			}
			files[file.key] = file
		}
	}
	return files, nil
}

// listSharedRetainedFiles lists the kinds owner references by shared name
// into listing, which a nil or differently selected listing replaces.
func (p *Publisher) listSharedRetainedFiles(ctx context.Context, owner *v1.HAProxyCfg, groups []retainedFileGroup, listing *childListing) (*childListing, error) {
	var shared []string
	for _, group := range groups {
		if !slices.Contains(shared, group.kind) && slices.ContainsFunc(group.refs, func(ref v1.ResourceReference) bool {
			return strings.HasSuffix(ref.Name, retainedChildSuffix)
		}) {
			shared = append(shared, group.kind)
		}
	}
	parent := metav1.GetControllerOf(owner)
	if parent == nil || len(shared) == 0 {
		return newChildListing(owner.Namespace, ""), nil
	}
	selector := retainedSelector(parent.UID)
	if listing == nil || listing.namespace != owner.Namespace || listing.selector != selector {
		listing = newChildListing(owner.Namespace, selector)
	}
	if err := p.listInto(ctx, listing, shared); err != nil {
		return nil, fmt.Errorf("listing shared retained auxiliary files: %w", err)
	}
	return listing, nil
}

func (p *Publisher) readRetainedReference(ctx context.Context, owner *v1.HAProxyCfg, ref v1.ResourceReference, kind string, family renderartifact.Family, seen map[string]bool, listing *childListing) (retainedFile, error) {
	if ref.Kind != kind || ref.Name == "" || ref.Namespace != owner.Namespace {
		return retainedFile{}, errors.New("retained auxiliary reference has a foreign kind or namespace")
	}
	if seen[ref.Kind+"/"+ref.Name] {
		return retainedFile{}, errors.New("retained auxiliary reference is repeated")
	}
	seen[ref.Kind+"/"+ref.Name] = true
	file, err := p.readRetainedFile(ctx, ref, family, listing)
	if err != nil {
		return retainedFile{}, fmt.Errorf("reading %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	if err := verifyRetainedFile(&file, owner, ref.Kind); err != nil {
		return retainedFile{}, fmt.Errorf("%s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	}
	return file, nil
}

func (p *Publisher) readRetainedFile(ctx context.Context, ref v1.ResourceReference, family renderartifact.Family, listing *childListing) (retainedFile, error) {
	client := p.crdClient.HaproxyTemplateICV1alpha1()
	switch family {
	case renderartifact.Map:
		item, err := listedOrGet(ctx, listing, kindMapFile, ref.Name, client.HAProxyMapFiles(ref.Namespace).Get)
		if err != nil {
			return retainedFile{}, err
		}
		return retainedFile{meta: item, key: retainedFileKey{family, item.Spec.Path}, content: item.Spec.Entries, checksum: item.Spec.Checksum, compressed: item.Spec.Compressed, empty: item.Spec.Empty}, nil
	case renderartifact.General:
		item, err := listedOrGet(ctx, listing, kindGeneralFile, ref.Name, client.HAProxyGeneralFiles(ref.Namespace).Get)
		if err != nil {
			return retainedFile{}, err
		}
		if item.Spec.CAFile {
			family = renderartifact.GeneralCA
		}
		return retainedFile{meta: item, key: retainedFileKey{family, item.Spec.Path}, name: item.Spec.FileName, content: item.Spec.Content, checksum: item.Spec.Checksum, compressed: item.Spec.Compressed, empty: item.Spec.Empty}, nil
	case renderartifact.CRTList:
		item, err := listedOrGet(ctx, listing, kindCRTListFile, ref.Name, client.HAProxyCRTListFiles(ref.Namespace).Get)
		if err != nil {
			return retainedFile{}, err
		}
		return retainedFile{meta: item, key: retainedFileKey{family, item.Spec.Path}, content: item.Spec.Entries, checksum: item.Spec.Checksum, compressed: item.Spec.Compressed, empty: item.Spec.Empty}, nil
	case renderartifact.Certificate, renderartifact.CA:
		item, err := listedOrGet(ctx, listing, kindSecret, ref.Name, p.k8sClient.CoreV1().Secrets(ref.Namespace).Get)
		if err != nil {
			return retainedFile{}, err
		}
		return retainedSecretFile(item, family)
	case renderartifact.Secret:
		item, err := listedOrGet(ctx, listing, kindSecret, ref.Name, p.k8sClient.CoreV1().Secrets(ref.Namespace).Get)
		if err != nil {
			return retainedFile{}, err
		}
		return retainedSecretFileSecret(item)
	default:
		return retainedFile{}, errors.New("unsupported retained auxiliary family")
	}
}

func retainedSecretFile(item *corev1.Secret, family renderartifact.Family) (retainedFile, error) {
	key, role := "certificate", "ssl-certificate"
	if family == renderartifact.CA {
		key, role = "ca", "ssl-ca"
	}
	filePath := item.Annotations[AuxiliaryPathAnnotationKey]
	content, present := item.Data[key]
	compressed, declared := compressedAnnotation(item)
	if item.Type != corev1.SecretTypeOpaque || item.Labels["haproxy-haptic.org/type"] != role ||
		!present || filePath == "" || string(item.Data["path"]) != filePath || len(item.Data) != 2 || !declared {
		return retainedFile{}, errors.New("retained certificate metadata differs from its payload")
	}
	return retainedFile{meta: item, key: retainedFileKey{family, filePath}, content: string(content), checksum: item.Annotations[AuxiliaryChecksumAnnotationKey], compressed: compressed, empty: len(content) == 0}, nil
}

func retainedSecretFileSecret(item *corev1.Secret) (retainedFile, error) {
	filePath := item.Annotations[AuxiliaryPathAnnotationKey]
	checksum := item.Annotations[AuxiliaryChecksumAnnotationKey]
	content, present := item.Data[secretFileDataKey]
	compressed, declared := compressedAnnotation(item)
	seal, sealed := strings.CutPrefix(checksum, sealChecksumPrefix)
	if item.Type != corev1.SecretTypeOpaque || item.Labels["haproxy-haptic.org/type"] != secretFileType ||
		!present || !sealed || seal == "" || filePath == "" || string(item.Data["path"]) != filePath || len(item.Data) != 3 || !declared {
		return retainedFile{}, errors.New("retained secret file metadata differs from its payload")
	}
	return retainedFile{
		meta: item, key: retainedFileKey{renderartifact.Secret, filePath}, content: string(content),
		checksum: checksum, contentChecksum: string(item.Data[secretFileChecksumDataKey]), seal: seal,
		compressed: compressed, empty: len(content) == 0,
	}, nil
}

// compressedAnnotation reads the compression flag; declared is false for any
// value the publisher never writes.
func compressedAnnotation(item metav1.Object) (compressed, declared bool) {
	switch item.GetAnnotations()["haproxy-haptic.org/compressed"] {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

func verifyRetainedFile(file *retainedFile, owner *v1.HAProxyCfg, kind string) error {
	parent := metav1.GetControllerOf(owner)
	shared := parent != nil && ownedByRetainedTemplate(file.meta, parent.Name, parent.UID)
	if !shared && !ownedByRuntimeConfig(file.meta, owner) || file.meta.GetDeletionTimestamp() != nil {
		return errors.New("retained auxiliary object has a foreign owner or is being deleted")
	}
	if file.empty != (file.content == "") || (file.empty && file.compressed) {
		return errors.New("retained auxiliary content declaration is invalid")
	}
	content, err := decodeRetainedContent(file.content, file.compressed)
	if err != nil {
		return err
	}
	contentChecksum := file.contentChecksum
	if contentChecksum == "" {
		contentChecksum = file.checksum
	}
	if file.key.path == "" || contentChecksum != calculateChecksum(content) {
		return errors.New("retained auxiliary checksum differs from its content")
	}
	ca := file.key.family == renderartifact.CA || file.key.family == renderartifact.GeneralCA
	address := retainedSuffix(owner.Spec.Checksum, owner.Spec.RetainedPlan)
	if shared {
		address = retainedChildSuffix
	}
	suffix := AuxiliaryContentSuffix(kind, file.key.path, file.checksum, ca) + address
	if !strings.HasSuffix(file.meta.GetName(), suffix) {
		return errors.New("retained auxiliary content address differs")
	}
	file.content = content
	return nil
}
