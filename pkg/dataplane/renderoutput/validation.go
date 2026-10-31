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

package renderoutput

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

var errOutputContentMismatch = errors.New("render output content differs")

func validateExactPlan(plan *renderplan.Plan) (map[string]*renderplan.File, Counts, error) {
	if err := validatePlanHeader(plan); err != nil {
		return nil, Counts{}, err
	}
	backendSections, profileSections, err := validateSections(plan.Sections)
	if err != nil {
		return nil, Counts{}, err
	}
	if backendErr := validateBackends(plan.Backends, backendSections); backendErr != nil {
		return nil, Counts{}, backendErr
	}
	if profileErr := validateProfiles(plan.Profiles, profileSections); profileErr != nil {
		return nil, Counts{}, profileErr
	}
	files, err := validateFiles(plan.Files)
	if err != nil {
		return nil, Counts{}, err
	}
	if mapErr := validateMaps(plan.Maps, files); mapErr != nil {
		return nil, Counts{}, mapErr
	}
	if crtListErr := validateCRTLists(plan.CRTLists); crtListErr != nil {
		return nil, Counts{}, crtListErr
	}
	return files, Counts{
		Sections: len(plan.Sections), Backends: len(plan.Backends),
		Profiles: len(plan.Profiles), Maps: len(plan.Maps),
		CRTLists: len(plan.CRTLists), Files: len(plan.Files),
	}, nil
}

func validatePlanHeader(plan *renderplan.Plan) error {
	if plan == nil {
		return errors.New("render output plan is nil")
	}
	if plan.SchemaVersion != renderplan.SchemaVersion {
		return fmt.Errorf("render output plan schema %d is unsupported", plan.SchemaVersion)
	}
	if plan.ID == "" || plan.ID != renderplan.Digest(plan.Canonical()) {
		return errors.New("render output plan ID does not match its declarations")
	}
	return nil
}

func validateSections(
	sections []renderplan.Section,
) (backendSections, profileSections map[string]renderplan.Section, err error) {
	backendSections = make(map[string]renderplan.Section)
	profileSections = make(map[string]renderplan.Section)
	for index := range sections {
		section := sections[index]
		if !section.TextKnown || section.Length < 0 || section.Length != len(section.Text) ||
			section.TextDigest != renderplan.DigestString(section.Text) {
			return nil, nil, fmt.Errorf("render output plan section %d has inexact content", index)
		}
		switch section.Kind {
		case renderplan.SectionKindCore:
		case renderplan.SectionKindBackend:
			if _, exists := backendSections[section.Name]; exists {
				return nil, nil, fmt.Errorf("render output plan backend section %q is duplicated", section.Name)
			}
			backendSections[section.Name] = section
		case renderplan.SectionKindProfile:
			if _, exists := profileSections[section.Name]; exists {
				return nil, nil, fmt.Errorf("render output plan profile section %q is duplicated", section.Name)
			}
			profileSections[section.Name] = section
		default:
			return nil, nil, fmt.Errorf("render output plan section %q has unknown kind %q", section.Name, section.Kind)
		}
	}
	return backendSections, profileSections, nil
}

func validateFiles(source []renderplan.File) (map[string]*renderplan.File, error) {
	files := make(map[string]*renderplan.File, len(source))
	for index := range source {
		file := &source[index]
		if !file.ContentKnown || file.Size < 0 || file.Size != int64(len(file.Content)) ||
			file.Digest != renderplan.DigestString(file.Content) {
			return nil, fmt.Errorf("render output plan file %d has inexact content", index)
		}
		if !validFileKind(file.Kind) {
			return nil, fmt.Errorf("render output plan file %q has unknown kind %q", file.Path, file.Kind)
		}
		if _, exists := files[file.Path]; exists {
			return nil, fmt.Errorf("render output plan file path %q is duplicated", file.Path)
		}
		files[file.Path] = file
	}
	return files, nil
}

func validateCRTLists(crtLists map[string]renderplan.CRTList) error {
	for key, crtList := range crtLists {
		if key != crtList.Path {
			return fmt.Errorf("render output plan CRT-list key %q differs from path %q", key, crtList.Path)
		}
	}
	return nil
}

func validateBackends(
	backends map[string]renderplan.Backend,
	sections map[string]renderplan.Section,
) error {
	if len(backends) != len(sections) {
		return errors.New("render output plan backend declarations do not match its sections")
	}
	for name := range backends {
		backend := backends[name]
		section, exists := sections[name]
		if !exists || backend.Name != name || !backend.ContentKnown ||
			backend.TextDigest != section.TextDigest ||
			backend.BodyDigest != renderplan.DigestString(strings.Join(backend.Body, "\n")) ||
			backend.CommentsDigest != renderplan.DigestString(strings.Join(backend.Comments, "\n")) ||
			backend.RecordDigest != backendRecordDigest(&backend) {
			return fmt.Errorf("render output plan backend %q has inexact content", name)
		}
	}
	return nil
}

func backendRecordDigest(backend *renderplan.Backend) string {
	record := *backend
	record.RecordDigest = ""
	record.TextDigest = ""
	encoded, err := json.Marshal(&record)
	if err != nil {
		panic(fmt.Sprintf("renderoutput: encoding a backend failed: %v", err))
	}
	return renderplan.Digest(encoded)
}

func validateProfiles(
	profiles map[string]renderplan.Profile,
	sections map[string]renderplan.Section,
) error {
	if len(profiles) != len(sections) {
		return errors.New("render output plan profile declarations do not match its sections")
	}
	for name, profile := range profiles {
		section, exists := sections[name]
		if !exists || profile.Name != name {
			return fmt.Errorf("render output plan profile %q has inexact content", name)
		}
		_, body, _ := strings.Cut(section.Text, "\n")
		if profile.BodyDigest != renderplan.DigestString(body) {
			return fmt.Errorf("render output plan profile %q has inexact content", name)
		}
	}
	return nil
}

func validFileKind(kind string) bool {
	switch kind {
	case renderplan.FileKindConfig, renderplan.FileKindMap, renderplan.FileKindCert,
		renderplan.FileKindCA, renderplan.FileKindCRTList, renderplan.FileKindGeneral:
		return true
	default:
		return false
	}
}

func validateMaps(mapsByPath map[string]renderplan.Map, files map[string]*renderplan.File) error {
	mapFiles := 0
	for _, file := range files {
		if file.Kind == renderplan.FileKindMap {
			mapFiles++
		}
	}
	if len(mapsByPath) != mapFiles {
		return errors.New("render output plan map declarations do not match its files")
	}
	for path, declared := range mapsByPath {
		file, exists := files[path]
		if !exists || file.Kind != renderplan.FileKindMap || declared.Path != path {
			return fmt.Errorf("render output plan map %q has inexact content", path)
		}
		parsed := renderplan.ParseMapEntries(file.Content)
		if (declared.Entries == nil) != (parsed == nil) || !slices.Equal(declared.Entries, parsed) {
			return fmt.Errorf("render output plan map %q has inexact content", path)
		}
	}
	return nil
}

func validateAuxiliaryBindingsWith(
	auxiliaryCount int,
	files map[string]*renderplan.File,
	artifacts *renderartifact.Snapshot,
	validated func(renderartifact.Descriptor, *renderartifact.Content) error,
) (int, error) {
	matched := make(map[string]struct{}, auxiliaryCount)
	artifactCount := 0
	err := artifacts.Walk(func(artifact *renderartifact.Artifact) error {
		artifactCount++
		descriptor, content, err := validateArtifactBinding(artifact, files, matched)
		if err != nil || validated == nil {
			return err
		}
		return validated(descriptor, content)
	})
	if err != nil {
		return 0, err
	}
	if len(matched) != auxiliaryCount {
		return 0, fmt.Errorf(
			"render output has %d auxiliary plan files and %d artifacts",
			auxiliaryCount, len(matched),
		)
	}
	return artifactCount, nil
}

func validateArtifactBinding(
	artifact *renderartifact.Artifact,
	files map[string]*renderplan.File,
	matched map[string]struct{},
) (renderartifact.Descriptor, *renderartifact.Content, error) {
	descriptor, err := artifact.Descriptor()
	if err != nil {
		return renderartifact.Descriptor{}, nil, err
	}
	file, exists := files[descriptor.RuntimePath]
	if !exists {
		return renderartifact.Descriptor{}, nil,
			fmt.Errorf("render artifact %q has no plan file", descriptor.RuntimePath)
	}
	if _, exists := matched[descriptor.RuntimePath]; exists {
		return renderartifact.Descriptor{}, nil,
			fmt.Errorf("render artifact runtime path %q is duplicated", descriptor.RuntimePath)
	}
	kind, reload := artifactPlanMetadata(descriptor)
	if file.Kind != kind || file.ReloadOnChange != reload || file.Seal != descriptor.Seal {
		return renderartifact.Descriptor{}, nil,
			fmt.Errorf("render artifact %q metadata differs from its plan file", descriptor.RuntimePath)
	}
	content, err := artifact.Content()
	if err != nil {
		return renderartifact.Descriptor{}, nil, err
	}
	if err := validateArtifactContent(content, descriptor.RuntimePath, file); err != nil {
		return renderartifact.Descriptor{}, nil, err
	}
	matched[descriptor.RuntimePath] = struct{}{}
	return descriptor, content, nil
}

func validateArtifactContent(
	content *renderartifact.Content,
	path string,
	file *renderplan.File,
) error {
	bytes, err := content.Bytes()
	if err != nil {
		return err
	}
	if int64(bytes) != file.Size {
		return fmt.Errorf("render artifact %q size differs from its plan file", path)
	}
	writer := &exactStringWriter{expected: file.Content}
	written, err := content.WriteTo(writer)
	if errors.Is(err, errOutputContentMismatch) {
		return artifactContentMismatchEvidence(content, path, file, written, writer.offset)
	}
	if err != nil {
		return err
	}
	if written != file.Size || writer.offset != len(file.Content) {
		return artifactContentMismatchEvidence(content, path, file, written, writer.offset)
	}
	return nil
}

// ArtifactContentMismatchError carries comparison evidence without file contents.
// IncrementalCommit is set only when the pair was rejected by an incremental
// commit; the four provenance fields are meaningful only then. Inherited means
// the delta carried no After for the path, so the base binding's value was
// re-validated; changed means the After differs from the base. An inherited,
// unchanged pair that mismatches was already inconsistent in the base.
type ArtifactContentMismatchError struct {
	Path              string
	PlanBytes         int
	ArtifactBytes     int
	Written           int64
	Matched           int
	ReadOK            bool
	ExactRead         bool
	PlanDigestValid   bool
	IncrementalCommit bool
	PlanFileInherited bool
	PlanFileChanged   bool
	ArtifactInherited bool
	ArtifactChanged   bool
}

func (e *ArtifactContentMismatchError) Error() string {
	return fmt.Sprintf("render artifact %q content differs from its plan file", e.Path)
}

func artifactContentMismatchEvidence(content *renderartifact.Content, path string, file *renderplan.File, written int64, offset int) error {
	text, readErr := content.String()
	return &ArtifactContentMismatchError{
		Path: path, PlanBytes: len(file.Content), ArtifactBytes: len(text), Written: written, Matched: offset,
		ReadOK: readErr == nil, ExactRead: readErr == nil && text == file.Content,
		PlanDigestValid: renderplan.DigestString(file.Content) == file.Digest,
	}
}

func artifactPlanMetadata(descriptor renderartifact.Descriptor) (string, bool) {
	switch descriptor.Family {
	case renderartifact.Map:
		return renderplan.FileKindMap, false
	case renderartifact.General:
		return renderplan.FileKindGeneral, descriptor.ReloadOnChange
	case renderartifact.Secret:
		return renderplan.FileKindGeneral, false
	case renderartifact.Certificate:
		return renderplan.FileKindCert, false
	case renderartifact.CA, renderartifact.GeneralCA:
		return renderplan.FileKindCA, false
	case renderartifact.CRTList:
		return renderplan.FileKindCRTList, false
	default:
		return "", false
	}
}

type exactStringWriter struct {
	expected string
	offset   int
}

func (w *exactStringWriter) Write(value []byte) (int, error) {
	if len(value) > len(w.expected)-w.offset {
		return 0, errOutputContentMismatch
	}
	for index := range value {
		if value[index] != w.expected[w.offset+index] {
			return 0, errOutputContentMismatch
		}
	}
	w.offset += len(value)
	return len(value), nil
}

func (w *exactStringWriter) WriteString(value string) (int, error) {
	if len(value) > len(w.expected)-w.offset ||
		value != w.expected[w.offset:w.offset+len(value)] {
		return 0, errOutputContentMismatch
	}
	w.offset += len(value)
	return len(value), nil
}

var _ io.StringWriter = (*exactStringWriter)(nil)
