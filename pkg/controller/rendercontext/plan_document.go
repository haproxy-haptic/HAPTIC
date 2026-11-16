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

package rendercontext

import (
	"errors"
	"fmt"
	"slices"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/rendercontent"
)

// DocumentPlanTransition is one authenticated document-and-plan publication candidate.
type DocumentPlanTransition struct {
	Document      rendercontent.Document
	DocumentDelta *rendercontent.DocumentDelta
	Plan          *renderplan.Snapshot
	PlanDelta     *renderplan.Delta
}

// PlanDocument builds exact plan metadata without materializing the assembled config.
func (r *PlanRegistry) PlanDocument(
	document rendercontent.Document,
	aux *dataplane.AuxiliaryFiles,
	authority *renderplan.Authority,
	previous *renderoutput.Snapshot,
) (*DocumentPlanTransition, error) {
	if r == nil {
		return nil, errors.New("planRegistry: document plan registry is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateTokenAuthority(); err != nil {
		return nil, err
	}
	if err := r.validateDocumentAssembly(document); err != nil {
		return nil, err
	}
	if r.prepared != nil {
		if err := r.prepared.ValidateAuthentication(); err != nil {
			return nil, err
		}
	}

	nextDocument := document
	var previousPlan *renderplan.Snapshot
	var documentDelta *rendercontent.DocumentDelta
	if previous != nil {
		var err error
		nextDocument, previousPlan, documentDelta, err = r.transitionFromPreviousLocked(authority, previous)
		if err != nil {
			return nil, err
		}
	}

	plan, backends, err := r.documentPlanMetadata(nextDocument, aux)
	if err != nil {
		return nil, err
	}
	snapshot, planDelta, err := renderplan.ReconcileSnapshotWithBackendSource(
		authority, previousPlan, plan, backends, nextDocument,
	)
	if err != nil {
		return nil, err
	}
	if documentDelta != nil {
		if err := validateDocumentPlanDeltaAlignment(documentDelta, planDelta); err != nil {
			return nil, err
		}
	}
	return &DocumentPlanTransition{
		Document: nextDocument, DocumentDelta: documentDelta,
		Plan: snapshot, PlanDelta: planDelta,
	}, nil
}

func (r *PlanRegistry) transitionFromPreviousLocked(
	authority *renderplan.Authority,
	previous *renderoutput.Snapshot,
) (rendercontent.Document, *renderplan.Snapshot, *rendercontent.DocumentDelta, error) {
	if err := previous.ValidateAuthentication(); err != nil {
		return rendercontent.Document{}, nil, nil, err
	}
	baseDocument, err := previous.ConfigDocument()
	if err != nil {
		return rendercontent.Document{}, nil, nil, err
	}
	previousPlan, err := previous.PlanSnapshot()
	if err != nil {
		return rendercontent.Document{}, nil, nil, err
	}
	if err := authority.ValidateSnapshot(previousPlan); err != nil {
		return rendercontent.Document{}, nil, nil, err
	}
	baseSections, err := previousPlan.SectionsCopy()
	if err != nil {
		return rendercontent.Document{}, nil, nil, err
	}
	nextDocument, documentDelta, err := transitionPlanDocument(
		baseDocument, baseSections, r.assembled,
	)
	if err != nil {
		return rendercontent.Document{}, nil, nil, err
	}
	return nextDocument, previousPlan, documentDelta, nil
}

// documentPlanMetadata builds the plan without its backends, which the
// returned source serves by name so the transition compares only what moved.
func (r *PlanRegistry) documentPlanMetadata(
	document rendercontent.Document,
	aux *dataplane.AuxiliaryFiles,
) (*renderplan.Plan, renderplan.BackendSource, error) {
	files, mapContents, err := r.planFiles("", aux)
	if err != nil {
		return nil, nil, err
	}
	bytes, err := document.Bytes()
	if err != nil {
		return nil, nil, err
	}
	files[0] = renderplan.File{
		Path: renderplan.ConfigFilePath, Kind: renderplan.FileKindConfig,
		ReloadOnChange: true, Size: int64(bytes),
	}
	backends, err := r.planBackendSource()
	if err != nil {
		return nil, nil, err
	}
	return &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		Sections:      slices.Clone(r.assembled),
		Profiles:      r.profiles(),
		Maps:          r.mapFiles(mapContents),
		CRTLists:      crtLists(files),
		Files:         sortedFiles(files),
	}, backends, nil
}

func transitionPlanDocument(
	base rendercontent.Document,
	baseSections []renderplan.Section,
	nextSections []renderplan.Section,
) (rendercontent.Document, *rendercontent.DocumentDelta, error) {
	if err := validateSectionDocumentShape(base, baseSections); err != nil {
		return rendercontent.Document{}, nil, err
	}
	prefix, suffix := exactSectionEdges(baseSections, nextSections)
	baseMiddle := len(baseSections) - prefix - suffix
	nextMiddle := len(nextSections) - prefix - suffix
	if difference := baseMiddle - nextMiddle; difference < -1 || difference > 1 {
		return rendercontent.Document{}, nil, renderplan.ErrDocumentTransitionRequiresRebuild
	}
	transaction, err := base.BeginTransaction()
	if err != nil {
		return rendercontent.Document{}, nil, err
	}
	paired := min(baseMiddle, nextMiddle)
	if err := replaceChangedSectionLeaves(
		transaction, base, baseSections, nextSections, prefix, paired,
	); err != nil {
		return rendercontent.Document{}, nil, err
	}
	switch {
	case nextMiddle > baseMiddle:
		if err := insertSectionLeaf(transaction, base, nextSections, prefix+paired); err != nil {
			return rendercontent.Document{}, nil, err
		}
	case baseMiddle > nextMiddle:
		if err := deleteSectionLeaf(transaction, base, prefix+paired); err != nil {
			return rendercontent.Document{}, nil, err
		}
	}
	next, delta, err := transaction.Commit()
	if err != nil {
		return rendercontent.Document{}, nil, err
	}
	if err := validateSectionDocumentShape(next, nextSections); err != nil {
		return rendercontent.Document{}, nil, err
	}
	return next, delta, nil
}

func replaceChangedSectionLeaves(
	transaction *rendercontent.DocumentTransaction,
	base rendercontent.Document,
	baseSections []renderplan.Section,
	nextSections []renderplan.Section,
	prefix, paired int,
) error {
	for offset := range paired {
		index := prefix + offset
		if baseSections[index] == nextSections[index] {
			continue
		}
		handle, err := base.LeafHandle(index)
		if err != nil {
			return err
		}
		part, err := sectionDocument(&nextSections[index])
		if err != nil {
			return err
		}
		if err := transaction.ReplaceDocument(handle, part); err != nil {
			return err
		}
	}
	return nil
}

func insertSectionLeaf(
	transaction *rendercontent.DocumentTransaction,
	base rendercontent.Document,
	nextSections []renderplan.Section,
	index int,
) error {
	gap, err := base.GapHandle(index)
	if err != nil {
		return err
	}
	part, err := sectionDocument(&nextSections[index])
	if err != nil {
		return err
	}
	return transaction.InsertDocument(gap, part)
}

func deleteSectionLeaf(
	transaction *rendercontent.DocumentTransaction,
	base rendercontent.Document,
	index int,
) error {
	handle, err := base.LeafHandle(index)
	if err != nil {
		return err
	}
	return transaction.Delete(handle)
}

func validateSectionDocumentShape(
	document rendercontent.Document,
	sections []renderplan.Section,
) error {
	leaves, err := document.Leaves()
	if err != nil {
		return err
	}
	if leaves != len(sections) {
		return errors.New("planRegistry: plan sections do not align with document leaves")
	}
	total := 0
	// Digests are checked once, when renderplan seals the snapshot; this
	// checks the shape against the document.
	for index := range sections {
		section := sections[index]
		if !section.TextKnown || section.Length != len(section.Text) {
			return fmt.Errorf("planRegistry: section %d has invalid exact content", index)
		}
		leafBytes, err := document.LeafBytes(index)
		if err != nil {
			return err
		}
		if leafBytes != section.Length {
			return fmt.Errorf("planRegistry: section %d does not match its document leaf", index)
		}
		total += section.Length
	}
	documentBytes, err := document.Bytes()
	if err != nil {
		return err
	}
	if total != documentBytes {
		return errors.New("planRegistry: plan sections do not partition the document")
	}
	return nil
}

func exactSectionEdges(
	base []renderplan.Section,
	next []renderplan.Section,
) (prefix, suffix int) {
	for prefix < len(base) && prefix < len(next) && base[prefix] == next[prefix] {
		prefix++
	}
	for suffix < len(base)-prefix && suffix < len(next)-prefix &&
		base[len(base)-1-suffix] == next[len(next)-1-suffix] {
		suffix++
	}
	return prefix, suffix
}

func sectionDocument(section *renderplan.Section) (rendercontent.Document, error) {
	var builder rendercontent.DocumentBuilder
	if _, err := builder.WriteString(section.Text); err != nil {
		return rendercontent.Document{}, err
	}
	return builder.Build(nil)
}

func validateDocumentPlanDeltaAlignment(
	documentDelta *rendercontent.DocumentDelta,
	planDelta *renderplan.Delta,
) error {
	if err := documentDelta.ValidateAuthentication(); err != nil {
		return err
	}
	if err := planDelta.ValidateAuthentication(); err != nil {
		return err
	}
	documentChanges, err := documentDelta.Changes()
	if err != nil {
		return err
	}
	planChanges, err := planDelta.SectionChanges()
	if err != nil {
		return err
	}
	if len(documentChanges) != len(planChanges) {
		return errors.New("planRegistry: document and plan section deltas are not aligned")
	}
	for index := range documentChanges {
		documentChange := documentChanges[index]
		planChange := planChanges[index]
		beforePresent := documentChange.Before.ValidateAuthentication() == nil
		afterPresent := documentChange.After.ValidateAuthentication() == nil
		if documentChange.Index != planChange.Index ||
			beforePresent != (planChange.Before != nil) ||
			afterPresent != (planChange.After != nil) {
			return errors.New("planRegistry: document and plan section deltas are not aligned")
		}
	}
	return nil
}
