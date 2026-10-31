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

package dataplane

import (
	"cmp"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
)

// BuildAuxiliaryFileSnapshot seals a complete legacy auxiliary-file set.
func BuildAuxiliaryFileSnapshot(
	authority *renderartifact.Authority,
	previous *renderartifact.Snapshot,
	files *AuxiliaryFiles,
) (*renderartifact.Snapshot, error) {
	return BuildAuxiliaryFileSnapshotWithRuntimePaths(authority, previous, files, nil)
}

// BuildAuxiliaryFileSnapshotWithRuntimePaths seals files with resolved plan paths.
func BuildAuxiliaryFileSnapshotWithRuntimePaths(
	authority *renderartifact.Authority,
	previous *renderartifact.Snapshot,
	files *AuxiliaryFiles,
	resolve func(renderartifact.Family, string) (string, error),
) (*renderartifact.Snapshot, error) {
	builder, err := renderartifact.NewBuilder(authority, previous)
	if err != nil {
		return nil, err
	}
	if files == nil {
		return builder.Build()
	}
	if err := addResolvedAuxiliaryArtifacts(builder, renderartifact.Map, files.MapFiles, resolve); err != nil {
		return nil, err
	}
	if err := addGeneralAuxiliaryArtifacts(builder, files.GeneralFiles); err != nil {
		return nil, err
	}
	if err := addResolvedAuxiliaryArtifacts(
		builder, renderartifact.Certificate, files.SSLCertificates, resolve,
	); err != nil {
		return nil, err
	}
	if err := addCAAuxiliaryArtifacts(builder, files.SSLCaFiles); err != nil {
		return nil, err
	}
	if err := addResolvedAuxiliaryArtifacts(
		builder, renderartifact.CRTList, files.CRTListFiles, resolve,
	); err != nil {
		return nil, err
	}
	for _, file := range files.SecretFiles {
		if err := addAuxiliaryArtifact(builder, renderartifact.Descriptor{
			Family: renderartifact.Secret, Path: file.Path, RuntimePath: file.Path, Seal: file.Seal,
		}, file.Content); err != nil {
			return nil, err
		}
	}
	return builder.Build()
}

func addResolvedAuxiliaryArtifacts[T auxiliaryfiles.FileItem](
	builder *renderartifact.Builder,
	family renderartifact.Family,
	files []T,
	resolve func(renderartifact.Family, string) (string, error),
) error {
	for _, file := range files {
		filePath := file.GetIdentifier()
		runtimePath, err := resolveAuxiliaryRuntimePath(resolve, family, filePath)
		if err != nil {
			return err
		}
		if err := addAuxiliaryArtifact(builder, renderartifact.Descriptor{
			Family: family, Path: filePath, RuntimePath: runtimePath,
		}, file.GetContent()); err != nil {
			return err
		}
	}
	return nil
}

func addGeneralAuxiliaryArtifacts(
	builder *renderartifact.Builder,
	files []auxiliaryfiles.GeneralFile,
) error {
	for _, file := range files {
		family := renderartifact.General
		if file.IsCaFile {
			family = renderartifact.GeneralCA
		}
		if err := addAuxiliaryArtifact(builder, renderartifact.Descriptor{
			Family: family, Name: file.Filename, Path: file.Path, RuntimePath: file.Path,
			ReloadOnChange: file.ReloadsOnPush(),
		}, file.Content); err != nil {
			return err
		}
	}
	return nil
}

func addCAAuxiliaryArtifacts(
	builder *renderartifact.Builder,
	files []auxiliaryfiles.SSLCaFile,
) error {
	for _, file := range files {
		if err := addAuxiliaryArtifact(builder, renderartifact.Descriptor{
			Family: renderartifact.CA, Path: file.Path, RuntimePath: file.Path,
		}, file.Content); err != nil {
			return err
		}
	}
	return nil
}

func resolveAuxiliaryRuntimePath(
	resolve func(renderartifact.Family, string) (string, error),
	family renderartifact.Family,
	filePath string,
) (string, error) {
	if resolve == nil {
		return filePath, nil
	}
	runtimePath, err := resolve(family, filePath)
	if err != nil {
		return "", fmt.Errorf("resolving auxiliary file family %d path %q: %w", family, filePath, err)
	}
	return runtimePath, nil
}

func addAuxiliaryArtifact(
	builder *renderartifact.Builder,
	descriptor renderartifact.Descriptor,
	content string,
) error {
	if err := builder.Add(descriptor, renderartifact.NewLiteralContent(content)); err != nil {
		return fmt.Errorf("sealing auxiliary file family %d name %q path %q: %w",
			descriptor.Family, descriptor.Name, descriptor.Path, err)
	}
	return nil
}

// MaterializeAuxiliaryFileSnapshot returns a detached legacy value.
func MaterializeAuxiliaryFileSnapshot(snapshot *renderartifact.Snapshot) (*AuxiliaryFiles, error) {
	if err := snapshot.ValidateAuthentication(); err != nil {
		return nil, err
	}
	files := &AuxiliaryFiles{}
	if err := snapshot.Walk(func(artifact *renderartifact.Artifact) error {
		return appendMaterializedArtifact(files, artifact)
	}); err != nil {
		return nil, err
	}
	files.Sort()
	return files, nil
}

func appendMaterializedArtifact(files *AuxiliaryFiles, artifact *renderartifact.Artifact) error {
	descriptor, err := artifact.Descriptor()
	if err != nil {
		return err
	}
	sealedContent, err := artifact.Content()
	if err != nil {
		return err
	}
	content, err := sealedContent.String()
	if err != nil {
		return err
	}
	content = strings.Clone(content)
	switch descriptor.Family {
	case renderartifact.Map:
		files.MapFiles = append(files.MapFiles, auxiliaryfiles.MapFile{
			Path: strings.Clone(descriptor.Path), Content: content,
		})
	case renderartifact.General:
		reload := descriptor.ReloadOnChange
		files.GeneralFiles = append(files.GeneralFiles, auxiliaryfiles.GeneralFile{
			Filename: strings.Clone(descriptor.Name), Path: strings.Clone(descriptor.Path), Content: content,
			ReloadOnPush: &reload,
		})
	case renderartifact.GeneralCA:
		files.GeneralFiles = append(files.GeneralFiles, auxiliaryfiles.GeneralFile{
			Filename: strings.Clone(descriptor.Name), Path: strings.Clone(descriptor.Path), Content: content, IsCaFile: true,
		})
	case renderartifact.Certificate:
		files.SSLCertificates = append(files.SSLCertificates, auxiliaryfiles.SSLCertificate{
			Path: strings.Clone(descriptor.Path), Content: content,
		})
	case renderartifact.CA:
		files.SSLCaFiles = append(files.SSLCaFiles, auxiliaryfiles.SSLCaFile{
			Path: strings.Clone(descriptor.Path), Content: content,
		})
	case renderartifact.CRTList:
		files.CRTListFiles = append(files.CRTListFiles, auxiliaryfiles.CRTListFile{
			Path: strings.Clone(descriptor.Path), Content: content,
		})
	case renderartifact.Secret:
		files.SecretFiles = append(files.SecretFiles, auxiliaryfiles.SecretFile{
			Path: strings.Clone(descriptor.Path), Content: content, Seal: descriptor.Seal,
		})
	default:
		return fmt.Errorf("materializing auxiliary file: invalid family %d", descriptor.Family)
	}
	return nil
}

// SnapshotContentEqual compares exact main-config bytes and authenticated artifacts.
func SnapshotContentEqual(
	leftConfig string,
	left *renderartifact.Snapshot,
	rightConfig string,
	right *renderartifact.Snapshot,
) (bool, error) {
	if err := left.ValidateAuthentication(); err != nil {
		return false, err
	}
	if err := right.ValidateAuthentication(); err != nil {
		return false, err
	}
	if leftConfig != rightConfig {
		return false, nil
	}
	return left.ExactEqual(right)
}

type snapshotChecksumItem struct {
	familyOrder int
	identifier  string
	family      renderartifact.Family
	path        string
	content     *renderartifact.Content
	seal        string
}

type snapshotChecksumIdentity struct {
	familyOrder int
	identifier  string
}

// ComputeSnapshotContentChecksum preserves ComputeContentChecksum's canonical legacy order.
func ComputeSnapshotContentChecksum(
	haproxyConfig string,
	snapshot *renderartifact.Snapshot,
) (string, error) {
	length, err := snapshot.Len()
	if err != nil {
		return "", err
	}
	items := make([]snapshotChecksumItem, 0, length)
	if err := snapshot.Walk(func(artifact *renderartifact.Artifact) error {
		descriptor, descriptorErr := artifact.Descriptor()
		if descriptorErr != nil {
			return descriptorErr
		}
		content, contentErr := artifact.Content()
		if contentErr != nil {
			return contentErr
		}
		identity, orderErr := legacyChecksumIdentity(descriptor)
		if orderErr != nil {
			return orderErr
		}
		items = append(items, snapshotChecksumItem{
			familyOrder: identity.familyOrder,
			identifier:  identity.identifier,
			family:      descriptor.Family,
			path:        descriptor.Path,
			content:     content,
			seal:        descriptor.Seal,
		})
		return nil
	}); err != nil {
		return "", err
	}
	slices.SortFunc(items, compareSnapshotChecksumItems)
	h := newContentChecksum(haproxyConfig)
	for _, item := range items {
		_, _ = h.Write([]byte(item.identifier))
		if item.family == renderartifact.Secret {
			_, _ = h.Write([]byte(item.seal))
			continue
		}
		if _, err := item.content.WriteTo(h); err != nil {
			return "", err
		}
	}
	return finishContentChecksum(h), nil
}

func legacyChecksumIdentity(descriptor renderartifact.Descriptor) (snapshotChecksumIdentity, error) {
	switch descriptor.Family {
	case renderartifact.General, renderartifact.GeneralCA:
		return snapshotChecksumIdentity{identifier: descriptor.Name}, nil
	case renderartifact.Map:
		return snapshotChecksumIdentity{familyOrder: 1, identifier: descriptor.Path}, nil
	case renderartifact.Certificate:
		return snapshotChecksumIdentity{familyOrder: 2, identifier: descriptor.Path}, nil
	case renderartifact.CA:
		return snapshotChecksumIdentity{familyOrder: 3, identifier: descriptor.Path}, nil
	case renderartifact.CRTList:
		return snapshotChecksumIdentity{familyOrder: 4, identifier: descriptor.Path}, nil
	case renderartifact.Secret:
		return snapshotChecksumIdentity{familyOrder: 5, identifier: descriptor.Path}, nil
	default:
		return snapshotChecksumIdentity{},
			fmt.Errorf("checksumming auxiliary file: invalid family %d", descriptor.Family)
	}
}

func compareSnapshotChecksumItems(left, right snapshotChecksumItem) int {
	if order := cmp.Compare(left.familyOrder, right.familyOrder); order != 0 {
		return order
	}
	if order := strings.Compare(left.identifier, right.identifier); order != 0 {
		return order
	}
	if order := cmp.Compare(left.family, right.family); order != 0 {
		return order
	}
	return strings.Compare(left.path, right.path)
}

type snapshotCurrentFile struct {
	order   int
	sortKey string
	path    string
	content string
}

// SnapshotCurrentFiles is the mutable currentFiles compatibility boundary.
func SnapshotCurrentFiles(snapshot *renderartifact.Snapshot) (map[string]string, error) {
	length, err := snapshot.Len()
	if err != nil {
		return nil, err
	}
	files := make([]snapshotCurrentFile, 0, length)
	if err := snapshot.Walk(func(artifact *renderartifact.Artifact) error {
		file, include, projectionErr := snapshotCurrentFileFor(artifact)
		if projectionErr != nil {
			return projectionErr
		}
		if include {
			files = append(files, file)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	slices.SortFunc(files, func(left, right snapshotCurrentFile) int {
		if order := cmp.Compare(left.order, right.order); order != 0 {
			return order
		}
		return strings.Compare(left.sortKey, right.sortKey)
	})
	current := make(map[string]string, len(files))
	for _, file := range files {
		current[path.Base(file.path)] = file.content
	}
	return current, nil
}

func snapshotCurrentFileFor(artifact *renderartifact.Artifact) (snapshotCurrentFile, bool, error) {
	descriptor, err := artifact.Descriptor()
	if err != nil {
		return snapshotCurrentFile{}, false, err
	}
	var order int
	var sortKey string
	switch descriptor.Family {
	case renderartifact.Map:
		sortKey = descriptor.Path
	case renderartifact.General:
		order = 1
		sortKey = descriptor.Name
	case renderartifact.CRTList:
		order = 2
		sortKey = descriptor.Path
	case renderartifact.Certificate, renderartifact.CA, renderartifact.GeneralCA, renderartifact.Secret:
		return snapshotCurrentFile{}, false, nil
	default:
		return snapshotCurrentFile{}, false,
			fmt.Errorf("projecting current files: invalid family %d", descriptor.Family)
	}
	content, err := artifact.Content()
	if err != nil {
		return snapshotCurrentFile{}, false, err
	}
	value, err := content.String()
	if err != nil {
		return snapshotCurrentFile{}, false, err
	}
	return snapshotCurrentFile{order: order, sortKey: sortKey, path: descriptor.Path, content: value}, true, nil
}
