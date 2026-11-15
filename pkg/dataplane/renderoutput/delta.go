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
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/rendercontent"
)

var (
	errInvalidOutputDelta       = errors.New("render output delta is invalid")
	errInvalidOutputTransaction = errors.New("render output transaction is invalid")
	errOutputDeltaStructural    = errors.New("render output delta requires full validation")
)

type outputDeltaAuthentication struct {
	owner     *Delta
	authority *Authority
	base      *Snapshot
	next      *Snapshot
	document  *rendercontent.DocumentDelta
	plan      *renderplan.Delta
	artifacts *renderartifact.Delta
}

// Delta authenticates one atomic publication across config, plan, and artifacts.
type Delta struct {
	authority *Authority
	base      *Snapshot
	next      *Snapshot
	document  *rendercontent.DocumentDelta
	plan      *renderplan.Delta
	artifacts *renderartifact.Delta
	seal      *Delta
	auth      outputDeltaAuthentication
}

type outputTransactionAuthentication struct {
	owner         *Transaction
	authority     *Authority
	base          *Snapshot
	document      *rendercontent.DocumentDelta
	plan          *renderplan.Delta
	artifacts     *renderartifact.Delta
	nextDocument  rendercontent.Document
	nextPlan      *renderplan.Snapshot
	nextArtifacts *renderartifact.Snapshot
}

// Transaction validates and publishes three exact child transitions atomically.
type Transaction struct {
	mu            sync.Mutex
	authority     *Authority
	base          *Snapshot
	document      *rendercontent.DocumentDelta
	plan          *renderplan.Delta
	artifacts     *renderartifact.Delta
	nextDocument  rendercontent.Document
	nextPlan      *renderplan.Snapshot
	nextArtifacts *renderartifact.Snapshot
	built         *Snapshot
	delta         *Delta
	err           error
	sealed        bool
	seal          *Transaction
	auth          outputTransactionAuthentication
}

// BeginTransaction binds exact child deltas to one output base.
func BeginTransaction(
	authority *Authority,
	base *Snapshot,
	document *rendercontent.DocumentDelta,
	plan *renderplan.Delta,
	artifacts *renderartifact.Delta,
) (*Transaction, error) {
	if err := authority.ValidateSnapshot(base); err != nil {
		return nil, err
	}
	if document == nil || plan == nil || artifacts == nil {
		return nil, errInvalidOutputTransaction
	}
	nextDocument, err := document.Apply(base.root.config.document)
	if err != nil {
		return nil, errors.Join(errInvalidOutputTransaction, err)
	}
	nextPlan, err := plan.Apply(base.root.plan)
	if err != nil {
		return nil, errors.Join(errInvalidOutputTransaction, err)
	}
	nextArtifacts, err := artifacts.Apply(base.root.artifacts)
	if err != nil {
		return nil, errors.Join(errInvalidOutputTransaction, err)
	}
	transaction := &Transaction{
		authority: authority, base: base, document: document, plan: plan, artifacts: artifacts,
		nextDocument: nextDocument, nextPlan: nextPlan, nextArtifacts: nextArtifacts,
	}
	transaction.seal = transaction
	transaction.auth = outputTransactionAuthentication{
		owner: transaction, authority: authority, base: base,
		document: document, plan: plan, artifacts: artifacts,
		nextDocument: nextDocument, nextPlan: nextPlan, nextArtifacts: nextArtifacts,
	}
	return transaction, nil
}

// Commit validates changed records and seals one indivisible output root.
func (t *Transaction) Commit() (*Snapshot, *Delta, error) {
	if t == nil {
		return nil, nil, errInvalidOutputTransaction
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.validateAuthentication(); err != nil {
		return nil, nil, err
	}
	if t.sealed {
		if t.err != nil {
			return nil, nil, t.err
		}
		if err := t.delta.ValidateAuthentication(); err != nil {
			return nil, nil, err
		}
		return t.built, t.delta, nil
	}
	t.sealed = true
	counts, bindings, sectionAligned, err := validateOutputChanges(
		t.base, t.nextDocument, t.nextPlan, t.nextArtifacts,
		t.document, t.plan, t.artifacts,
	)
	if err != nil {
		return nil, nil, t.recordError(err)
	}

	documentSame, err := t.nextDocument.SameRoot(t.base.root.config.document)
	if err != nil {
		return nil, nil, t.recordError(err)
	}
	if documentSame && t.nextPlan == t.base.root.plan && t.nextArtifacts == t.base.root.artifacts {
		t.built = t.base
	} else {
		config := t.base.root.config
		if !documentSame {
			bytes, err := t.nextDocument.Bytes()
			if err != nil {
				return nil, nil, t.recordError(err)
			}
			config = sealDeferredConfig(t.nextDocument, bytes, sectionAligned)
		}
		t.built = sealSnapshot(t.authority, sealDeferredRoot(
			t.authority, config, t.nextPlan, t.nextArtifacts, bindings, counts,
		))
	}
	t.delta = sealOutputDelta(
		t.authority, t.base, t.built, t.document, t.plan, t.artifacts,
	)
	return t.built, t.delta, nil
}

func (t *Transaction) validateAuthentication() error {
	if t == nil || t.seal != t {
		return errInvalidOutputTransaction
	}
	if t.authority == nil || t.base == nil {
		return errInvalidOutputTransaction
	}
	if t.document == nil || t.plan == nil || t.artifacts == nil ||
		t.nextPlan == nil || t.nextArtifacts == nil {
		return errInvalidOutputTransaction
	}
	expected := outputTransactionAuthentication{
		owner: t, authority: t.authority, base: t.base,
		document: t.document, plan: t.plan, artifacts: t.artifacts,
		nextDocument: t.nextDocument, nextPlan: t.nextPlan, nextArtifacts: t.nextArtifacts,
	}
	if t.auth != expected {
		return errInvalidOutputTransaction
	}
	if err := t.authority.ValidateSnapshot(t.base); err != nil {
		return errors.Join(errInvalidOutputTransaction, err)
	}
	return validateOutputTransactionChildren(t)
}

func validateOutputTransactionChildren(t *Transaction) error {
	nextDocument, err := t.document.Apply(t.base.root.config.document)
	if err != nil || nextDocument != t.nextDocument {
		return errors.Join(errInvalidOutputTransaction, err)
	}
	nextPlan, err := t.plan.Apply(t.base.root.plan)
	if err != nil || nextPlan != t.nextPlan {
		return errors.Join(errInvalidOutputTransaction, err)
	}
	nextArtifacts, err := t.artifacts.Apply(t.base.root.artifacts)
	if err != nil || nextArtifacts != t.nextArtifacts {
		return errors.Join(errInvalidOutputTransaction, err)
	}
	return nil
}

func (t *Transaction) recordError(err error) error {
	if t.err == nil {
		t.err = err
	}
	return err
}

// Apply returns this atomic publication only for its exact output base.
func (d *Delta) Apply(base *Snapshot) (*Snapshot, error) {
	if err := d.ValidateAuthentication(); err != nil {
		return nil, err
	}
	if base != d.base {
		return nil, errInvalidOutputDelta
	}
	return d.next, nil
}

// SameRoot reports whether none of the three child roots changed.
func (d *Delta) SameRoot() (bool, error) {
	if err := d.ValidateAuthentication(); err != nil {
		return false, err
	}
	return d.base == d.next, nil
}

// ValidateAuthentication verifies the exact atomic base-to-next publication.
func (d *Delta) ValidateAuthentication() error {
	if d == nil || d.seal != d {
		return errInvalidOutputDelta
	}
	if d.authority == nil || d.base == nil || d.next == nil || d.document == nil ||
		d.plan == nil || d.artifacts == nil {
		return errInvalidOutputDelta
	}
	expected := outputDeltaAuthentication{
		owner: d, authority: d.authority, base: d.base, next: d.next,
		document: d.document, plan: d.plan, artifacts: d.artifacts,
	}
	if d.auth != expected {
		return errInvalidOutputDelta
	}
	if err := d.authority.ValidateSnapshot(d.base); err != nil {
		return errors.Join(errInvalidOutputDelta, err)
	}
	if err := d.authority.ValidateSnapshot(d.next); err != nil {
		return errors.Join(errInvalidOutputDelta, err)
	}
	return validateOutputDeltaChildren(d)
}

func validateOutputDeltaChildren(d *Delta) error {
	document, err := d.document.Apply(d.base.root.config.document)
	if err != nil || document != d.next.root.config.document {
		return errors.Join(errInvalidOutputDelta, err)
	}
	plan, err := d.plan.Apply(d.base.root.plan)
	if err != nil || plan != d.next.root.plan {
		return errors.Join(errInvalidOutputDelta, err)
	}
	artifacts, err := d.artifacts.Apply(d.base.root.artifacts)
	if err != nil || artifacts != d.next.root.artifacts {
		return errors.Join(errInvalidOutputDelta, err)
	}
	return nil
}

func sealOutputDelta(
	authority *Authority,
	base, next *Snapshot,
	document *rendercontent.DocumentDelta,
	plan *renderplan.Delta,
	artifacts *renderartifact.Delta,
) *Delta {
	delta := &Delta{
		authority: authority, base: base, next: next,
		document: document, plan: plan, artifacts: artifacts,
	}
	delta.seal = delta
	delta.auth = outputDeltaAuthentication{
		owner: delta, authority: authority, base: base, next: next,
		document: document, plan: plan, artifacts: artifacts,
	}
	return delta
}

type changedSectionKey struct {
	kind string
	name string
}

func validateOutputChanges(
	base *Snapshot,
	nextDocument rendercontent.Document,
	nextPlan *renderplan.Snapshot,
	nextArtifacts *renderartifact.Snapshot,
	documentDelta *rendercontent.DocumentDelta,
	planDelta *renderplan.Delta,
	artifactDelta *renderartifact.Delta,
) (Counts, *outputBindingTree, bool, error) {
	documentChanges, err := documentDelta.Changes()
	if err != nil {
		return Counts{}, nil, false, err
	}
	planChanges, err := planDelta.Changes()
	if err != nil {
		return Counts{}, nil, false, err
	}
	artifactChanges, err := artifactDelta.Changes()
	if err != nil {
		return Counts{}, nil, false, err
	}
	counts, err := changedOutputCounts(base.root.counts, &planChanges, artifactChanges)
	if err != nil {
		return Counts{}, nil, false, err
	}
	if err := validateChangedOutputCounts(counts, nextPlan, nextArtifacts); err != nil {
		return Counts{}, nil, false, err
	}
	sections, sectionAligned, documentChanged, err := validateChangedSections(
		base, nextDocument, nextPlan, documentChanges, planChanges.Sections, counts.Sections,
	)
	if err != nil {
		return Counts{}, nil, false, err
	}
	if err := validateChangedBackends(base.root.plan, nextPlan, planChanges.Backends, sections); err != nil {
		return Counts{}, nil, false, err
	}
	if err := validateChangedProfiles(base.root.plan, nextPlan, planChanges.Profiles, sections); err != nil {
		return Counts{}, nil, false, err
	}
	if err := validateChangedCRTLists(planChanges.CRTLists); err != nil {
		return Counts{}, nil, false, err
	}
	bindings, configChanged, err := validateChangedBindings(
		base, nextDocument, nextPlan, planChanges.Files, planChanges.Maps,
		artifactChanges, counts,
	)
	if err != nil {
		return Counts{}, nil, false, err
	}
	if documentChanged && !configChanged {
		return Counts{}, nil, false, errors.New("render output config changed without its plan file")
	}
	return counts, bindings, sectionAligned, nil
}

func changedOutputCounts(
	base Counts,
	plan *renderplan.Changes,
	artifacts []renderartifact.SnapshotChange,
) (Counts, error) {
	counts := base
	var err error
	if counts.Sections, err = changedSequenceCount(counts.Sections, plan.Sections); err != nil {
		return Counts{}, err
	}
	if counts.Backends, err = changedCount(counts.Backends, plan.Backends); err != nil {
		return Counts{}, err
	}
	if counts.Profiles, err = changedCount(counts.Profiles, plan.Profiles); err != nil {
		return Counts{}, err
	}
	if counts.Maps, err = changedCount(counts.Maps, plan.Maps); err != nil {
		return Counts{}, err
	}
	if counts.CRTLists, err = changedCount(counts.CRTLists, plan.CRTLists); err != nil {
		return Counts{}, err
	}
	if counts.Files, err = changedFileCount(counts.Files, plan.Files); err != nil {
		return Counts{}, err
	}
	if counts.Artifacts, err = changedArtifactCount(counts.Artifacts, artifacts); err != nil {
		return Counts{}, err
	}
	if !counts.valid() {
		return Counts{}, errInvalidOutputDelta
	}
	return counts, nil
}

func changedCount[T any](base int, changes []renderplan.NamedChange[T]) (int, error) {
	count := base
	for _, change := range changes {
		if change.Before == nil && change.After == nil {
			return 0, errInvalidOutputDelta
		}
		count += presence(change.After) - presence(change.Before)
	}
	return count, nil
}

func changedSequenceCount[T any](base int, changes []renderplan.SequenceChange[T]) (int, error) {
	count := base
	for _, change := range changes {
		if change.Before == nil && change.After == nil {
			return 0, errInvalidOutputDelta
		}
		count += presence(change.After) - presence(change.Before)
	}
	return count, nil
}

func changedFileCount(base int, changes []renderplan.FileChange) (int, error) {
	count := base
	for _, change := range changes {
		if change.Before == nil && change.After == nil {
			return 0, errInvalidOutputDelta
		}
		count += presence(change.After) - presence(change.Before)
	}
	return count, nil
}

func changedArtifactCount(base int, changes []renderartifact.SnapshotChange) (int, error) {
	count := base
	for _, change := range changes {
		if change.Before == nil && change.After == nil {
			return 0, errInvalidOutputDelta
		}
		count += presence(change.After) - presence(change.Before)
	}
	return count, nil
}

func presence[T any](value *T) int {
	if value == nil {
		return 0
	}
	return 1
}

func validateChangedOutputCounts(
	counts Counts,
	plan *renderplan.Snapshot,
	artifacts *renderartifact.Snapshot,
) error {
	planEntries, err := plan.Len()
	if err != nil {
		return err
	}
	if planEntries != counts.planEntries() {
		return errInvalidOutputDelta
	}
	artifactCount, err := artifacts.Len()
	if err != nil {
		return err
	}
	if artifactCount != counts.Artifacts {
		return errInvalidOutputDelta
	}
	return nil
}

type changedSectionState struct {
	final           map[changedSectionKey]renderplan.Section
	occurrenceDelta map[changedSectionKey]int
}

type changedSectionValidator struct {
	basePlan       *renderplan.Snapshot
	nextPlan       *renderplan.Snapshot
	state          *changedSectionState
	offset         int
	previousIndex  int
	sectionAligned bool
}

func validateChangedSections(
	base *Snapshot,
	nextDocument rendercontent.Document,
	nextPlan *renderplan.Snapshot,
	documentChanges []rendercontent.DocumentLeafChange,
	sectionChanges []renderplan.SequenceChange[renderplan.Section],
	sectionCount int,
) (state changedSectionState, sectionAligned, documentChanged bool, err error) {
	if len(documentChanges) != len(sectionChanges) {
		return state, false, false,
			errors.New("render output document changes do not match its plan sections")
	}
	if len(sectionChanges) != 0 && !base.root.config.sectionAligned {
		return state, false, false, errOutputDeltaStructural
	}
	validator := changedSectionValidator{
		basePlan: base.root.plan, nextPlan: nextPlan, state: &state,
		previousIndex: -1, sectionAligned: base.root.config.sectionAligned,
	}
	for index := range sectionChanges {
		if err := validator.validate(&sectionChanges[index], &documentChanges[index]); err != nil {
			return state, false, false, err
		}
	}
	leaves, err := nextDocument.Leaves()
	if err != nil {
		return state, false, false, err
	}
	if len(sectionChanges) != 0 && (leaves != sectionCount || !validator.sectionAligned) {
		return state, false, false,
			errors.New("render output document leaves do not match its plan sections")
	}
	return state, validator.sectionAligned, len(documentChanges) != 0, nil
}

func (v *changedSectionValidator) validate(
	change *renderplan.SequenceChange[renderplan.Section],
	documentChange *rendercontent.DocumentLeafChange,
) error {
	if change.Index <= v.previousIndex || documentChange.Index != change.Index {
		return errInvalidOutputDelta
	}
	v.previousIndex = change.Index
	if presence(change.Before) != documentPresence(documentChange.Before) ||
		presence(change.After) != documentPresence(documentChange.After) {
		return errors.New("render output document changes do not match its plan sections")
	}
	if err := v.validateBefore(change, documentChange); err != nil {
		return err
	}
	nextIndex := change.Index + v.offset
	v.offset += presence(change.After) - presence(change.Before)
	return v.validateAfter(change, documentChange, nextIndex)
}

func (v *changedSectionValidator) validateBefore(
	change *renderplan.SequenceChange[renderplan.Section],
	documentChange *rendercontent.DocumentLeafChange,
) error {
	if change.Before == nil {
		return nil
	}
	if err := validateChangedSection(change.Index, change.Before); err != nil {
		return err
	}
	before, err := v.basePlan.SectionAt(change.Index)
	if err != nil || before != *change.Before {
		return errors.Join(errInvalidOutputDelta, err)
	}
	text, err := documentChange.Before.String()
	if err != nil {
		return err
	}
	if text != change.Before.Text {
		return errors.New("render output document leaf differs from its plan section")
	}
	v.state.remove(change.Before)
	return nil
}

func (v *changedSectionValidator) validateAfter(
	change *renderplan.SequenceChange[renderplan.Section],
	documentChange *rendercontent.DocumentLeafChange,
	nextIndex int,
) error {
	if change.After == nil {
		return nil
	}
	if err := validateChangedSection(change.Index, change.After); err != nil {
		return err
	}
	afterLeaves, err := documentChange.After.Leaves()
	if err != nil {
		return err
	}
	if change.After.Length == 0 || afterLeaves != 1 {
		v.sectionAligned = false
	}
	after, err := v.nextPlan.SectionAt(nextIndex)
	if err != nil || after != *change.After {
		return errors.Join(errInvalidOutputDelta, err)
	}
	text, err := documentChange.After.String()
	if err != nil {
		return err
	}
	if text != change.After.Text {
		return errors.New("render output document leaf differs from its plan section")
	}
	v.state.add(change.After)
	return nil
}

func (s *changedSectionState) remove(section *renderplan.Section) {
	if section.Kind == renderplan.SectionKindCore {
		return
	}
	if s.occurrenceDelta == nil {
		s.occurrenceDelta = make(map[changedSectionKey]int)
	}
	s.occurrenceDelta[sectionKey(section)]--
}

func (s *changedSectionState) add(section *renderplan.Section) {
	if section.Kind == renderplan.SectionKindCore {
		return
	}
	if s.occurrenceDelta == nil {
		s.occurrenceDelta = make(map[changedSectionKey]int)
	}
	if s.final == nil {
		s.final = make(map[changedSectionKey]renderplan.Section)
	}
	key := sectionKey(section)
	s.occurrenceDelta[key]++
	s.final[key] = *section
}

func documentPresence(document rendercontent.Document) int {
	if document.ValidateAuthentication() == nil {
		return 1
	}
	return 0
}

func sectionKey(section *renderplan.Section) changedSectionKey {
	return changedSectionKey{kind: section.Kind, name: section.Name}
}

func validateChangedSection(index int, section *renderplan.Section) error {
	if !section.TextKnown || section.Length < 0 || section.Length != len(section.Text) ||
		section.TextDigest != renderplan.DigestString(section.Text) {
		return fmt.Errorf("render output plan section %d has inexact content", index)
	}
	switch section.Kind {
	case renderplan.SectionKindCore, renderplan.SectionKindBackend, renderplan.SectionKindProfile:
		return nil
	default:
		return fmt.Errorf("render output plan section %q has unknown kind %q", section.Name, section.Kind)
	}
}

func validateChangedBackends(
	base, next *renderplan.Snapshot,
	changes []renderplan.NamedChange[renderplan.Backend],
	sections changedSectionState,
) error {
	if len(changes) == 0 && len(sections.occurrenceDelta) == 0 {
		return nil
	}
	affected, err := changedBackendNames(changes, sections)
	if err != nil {
		return err
	}
	for name := range affected {
		if err := validateChangedBackendBinding(base, next, sections, name); err != nil {
			return err
		}
	}
	return nil
}

func changedBackendNames(
	changes []renderplan.NamedChange[renderplan.Backend],
	sections changedSectionState,
) (map[string]struct{}, error) {
	affected := make(map[string]struct{}, len(changes)+len(sections.occurrenceDelta))
	for index := range changes {
		if err := collectChangedBackendName(&changes[index], sections, affected); err != nil {
			return nil, err
		}
	}
	for key := range sections.occurrenceDelta {
		if key.kind == renderplan.SectionKindBackend {
			affected[key.name] = struct{}{}
		}
	}
	return affected, nil
}

func collectChangedBackendName(
	change *renderplan.NamedChange[renderplan.Backend],
	sections changedSectionState,
	affected map[string]struct{},
) error {
	if change.Before == nil && change.After == nil ||
		change.Before != nil && change.Name != change.Before.Name ||
		change.After != nil && change.Name != change.After.Name {
		return errInvalidOutputDelta
	}
	affected[change.Name] = struct{}{}
	if change.After == nil {
		return nil
	}
	if err := validateChangedBackend(change.After, nil); err != nil {
		return err
	}
	if change.Before == nil || change.Before.TextDigest == change.After.TextDigest {
		return nil
	}
	key := changedSectionKey{kind: renderplan.SectionKindBackend, name: change.Name}
	if _, found := sections.final[key]; !found {
		return fmt.Errorf("render output backend %q changed without its section", change.Name)
	}
	return nil
}

func validateChangedBackendBinding(
	base, next *renderplan.Snapshot,
	sections changedSectionState,
	name string,
) error {
	baseBackend, baseFound, err := base.BackendNamed(name)
	if err != nil {
		return err
	}
	backend, found, err := next.BackendNamed(name)
	if err != nil {
		return err
	}
	key := changedSectionKey{kind: renderplan.SectionKindBackend, name: name}
	occurrences := presenceValue(baseFound) + sections.occurrenceDelta[key]
	if occurrences < 0 || occurrences > 1 || occurrences != presenceValue(found) {
		return fmt.Errorf("render output backend section %q does not match its declaration", name)
	}
	if !found {
		return nil
	}
	section, sectionChanged := sections.final[key]
	if sectionChanged {
		return validateChangedBackend(&backend, &section)
	}
	if baseFound && backend.TextDigest != baseBackend.TextDigest {
		return fmt.Errorf("render output backend %q changed without its section", name)
	}
	return nil
}

func validateChangedBackend(
	backend *renderplan.Backend,
	section *renderplan.Section,
) error {
	if !backend.ContentKnown ||
		backend.BodyDigest != renderplan.DigestString(strings.Join(backend.Body, "\n")) ||
		backend.CommentsDigest != renderplan.DigestString(strings.Join(backend.Comments, "\n")) ||
		backend.RecordDigest != backendRecordDigest(backend) ||
		section != nil && backend.TextDigest != section.TextDigest {
		return fmt.Errorf("render output plan backend %q has inexact content", backend.Name)
	}
	return nil
}

func validateChangedProfiles(
	base, next *renderplan.Snapshot,
	changes []renderplan.NamedChange[renderplan.Profile],
	sections changedSectionState,
) error {
	if len(changes) == 0 && len(sections.occurrenceDelta) == 0 {
		return nil
	}
	affected, err := changedProfileNames(changes, sections)
	if err != nil {
		return err
	}
	for name := range affected {
		if err := validateChangedProfileBinding(base, next, sections, name); err != nil {
			return err
		}
	}
	return nil
}

func changedProfileNames(
	changes []renderplan.NamedChange[renderplan.Profile],
	sections changedSectionState,
) (map[string]struct{}, error) {
	affected := make(map[string]struct{}, len(changes)+len(sections.occurrenceDelta))
	for index := range changes {
		change := &changes[index]
		if change.Before == nil && change.After == nil ||
			change.Before != nil && change.Name != change.Before.Name ||
			change.After != nil && change.Name != change.After.Name {
			return nil, errInvalidOutputDelta
		}
		affected[change.Name] = struct{}{}
		if change.Before != nil && change.After != nil &&
			change.Before.BodyDigest != change.After.BodyDigest {
			if _, found := sections.final[changedSectionKey{
				kind: renderplan.SectionKindProfile, name: change.Name,
			}]; !found {
				return nil, fmt.Errorf(
					"render output profile %q changed without its section", change.Name,
				)
			}
		}
	}
	for key := range sections.occurrenceDelta {
		if key.kind == renderplan.SectionKindProfile {
			affected[key.name] = struct{}{}
		}
	}
	return affected, nil
}

func validateChangedProfileBinding(
	base, next *renderplan.Snapshot,
	sections changedSectionState,
	name string,
) error {
	baseProfile, baseFound, err := base.ProfileNamed(name)
	if err != nil {
		return err
	}
	profile, found, err := next.ProfileNamed(name)
	if err != nil {
		return err
	}
	key := changedSectionKey{kind: renderplan.SectionKindProfile, name: name}
	occurrences := presenceValue(baseFound) + sections.occurrenceDelta[key]
	if occurrences < 0 || occurrences > 1 || occurrences != presenceValue(found) {
		return fmt.Errorf("render output profile section %q does not match its declaration", name)
	}
	if !found {
		return nil
	}
	section, sectionChanged := sections.final[key]
	if sectionChanged && !profileMatchesSection(profile, &section) {
		return fmt.Errorf("render output plan profile %q has inexact content", name)
	}
	if !sectionChanged && baseFound && profile.BodyDigest != baseProfile.BodyDigest {
		return fmt.Errorf("render output profile %q changed without its section", name)
	}
	return nil
}

func presenceValue(present bool) int {
	if present {
		return 1
	}
	return 0
}

func profileMatchesSection(profile renderplan.Profile, section *renderplan.Section) bool {
	_, body, _ := strings.Cut(section.Text, "\n")
	return profile.Name == section.Name && profile.BodyDigest == renderplan.DigestString(body)
}

type outputBindingChange struct {
	fileBefore     int
	fileAfter      int
	file           *outputFileBinding
	artifactBefore int
	artifactAfter  int
	artifact       *renderartifact.Artifact
}

type collectedOutputBindingChanges struct {
	paths         map[string]*outputBindingChange
	mapPaths      map[string]struct{}
	configChanged bool
}

func validateChangedBindings(
	base *Snapshot,
	nextDocument rendercontent.Document,
	nextPlan *renderplan.Snapshot,
	fileChanges []renderplan.FileChange,
	mapChanges []renderplan.NamedChange[renderplan.Map],
	artifactChanges []renderartifact.SnapshotChange,
	counts Counts,
) (*outputBindingTree, bool, error) {
	collected, err := collectOutputBindingChanges(
		base, fileChanges, mapChanges, artifactChanges,
	)
	if err != nil {
		return nil, false, err
	}
	bindings := base.root.bindings
	paths := make([]string, 0, len(collected.paths))
	for path := range collected.paths {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		bindings, err = applyChangedOutputBinding(
			bindings, path, collected.paths[path], nextDocument,
		)
		if err != nil {
			return nil, false, err
		}
	}
	if bindings.files != counts.Files || bindings.artifacts != counts.Artifacts {
		return nil, false, errInvalidOutputDelta
	}
	for path := range collected.mapPaths {
		if err := validateChangedMapBinding(nextPlan, bindings, path); err != nil {
			return nil, false, err
		}
	}
	return bindings, collected.configChanged, nil
}

func collectOutputBindingChanges(
	base *Snapshot,
	fileChanges []renderplan.FileChange,
	mapChanges []renderplan.NamedChange[renderplan.Map],
	artifactChanges []renderartifact.SnapshotChange,
) (collectedOutputBindingChanges, error) {
	collected := collectedOutputBindingChanges{
		paths: make(map[string]*outputBindingChange, len(fileChanges)+len(artifactChanges)),
	}
	if err := collectChangedOutputFiles(base.root.bindings, fileChanges, &collected); err != nil {
		return collectedOutputBindingChanges{}, err
	}
	if err := collectChangedOutputArtifacts(
		base.root.bindings, artifactChanges, &collected,
	); err != nil {
		return collectedOutputBindingChanges{}, err
	}
	if err := collectChangedOutputMaps(mapChanges, &collected); err != nil {
		return collectedOutputBindingChanges{}, err
	}
	return collected, nil
}

func collectChangedOutputFiles(
	base *outputBindingTree,
	changes []renderplan.FileChange,
	collected *collectedOutputBindingChanges,
) error {
	for index := range changes {
		change := &changes[index]
		if change.Before == nil && change.After == nil {
			return errInvalidOutputDelta
		}
		if err := collectChangedOutputFileRecord(base, change.Before, false, collected); err != nil {
			return err
		}
		if err := collectChangedOutputFileRecord(base, change.After, true, collected); err != nil {
			return err
		}
		if fileChangeTouchesConfig(*change) {
			collected.configChanged = true
		}
	}
	return nil
}

func collectChangedOutputFileRecord(
	base *outputBindingTree,
	record *renderplan.FileRecord,
	after bool,
	collected *collectedOutputBindingChanges,
) error {
	if record == nil {
		return nil
	}
	binding, err := outputFileBindingFromRecord(record)
	if err != nil {
		return err
	}
	path := binding.descriptor.Path
	change := outputBindingChangeFor(collected.paths, path)
	if after {
		change.fileAfter++
		change.file = binding
	} else {
		change.fileBefore++
		if err := validateBaseFileBinding(base, path, binding); err != nil {
			return err
		}
	}
	if binding.descriptor.Kind == renderplan.FileKindMap {
		if collected.mapPaths == nil {
			collected.mapPaths = make(map[string]struct{})
		}
		collected.mapPaths[path] = struct{}{}
	}
	return nil
}

func collectChangedOutputArtifacts(
	base *outputBindingTree,
	changes []renderartifact.SnapshotChange,
	collected *collectedOutputBindingChanges,
) error {
	for index := range changes {
		change := &changes[index]
		if change.Before == nil && change.After == nil {
			return errInvalidOutputDelta
		}
		if err := collectChangedOutputArtifact(base, change.Before, false, collected); err != nil {
			return err
		}
		if err := collectChangedOutputArtifact(base, change.After, true, collected); err != nil {
			return err
		}
	}
	return nil
}

func collectChangedOutputArtifact(
	base *outputBindingTree,
	artifact *renderartifact.Artifact,
	after bool,
	collected *collectedOutputBindingChanges,
) error {
	if artifact == nil {
		return nil
	}
	descriptor, err := artifact.Descriptor()
	if err != nil {
		return err
	}
	change := outputBindingChangeFor(collected.paths, descriptor.RuntimePath)
	if after {
		change.artifactAfter++
		change.artifact = artifact
		return nil
	}
	change.artifactBefore++
	return validateBaseArtifactBinding(base, descriptor.RuntimePath, artifact)
}

func collectChangedOutputMaps(
	changes []renderplan.NamedChange[renderplan.Map],
	collected *collectedOutputBindingChanges,
) error {
	if len(changes) != 0 && collected.mapPaths == nil {
		collected.mapPaths = make(map[string]struct{}, len(changes))
	}
	for _, change := range changes {
		if change.Before == nil && change.After == nil ||
			change.Before != nil && change.Before.Path != change.Name ||
			change.After != nil && change.After.Path != change.Name {
			return errInvalidOutputDelta
		}
		collected.mapPaths[change.Name] = struct{}{}
	}
	return nil
}

func applyChangedOutputBinding(
	bindings *outputBindingTree,
	path string,
	change *outputBindingChange,
	nextDocument rendercontent.Document,
) (*outputBindingTree, error) {
	baseBinding, baseFound, err := bindings.lookup(path)
	if err != nil {
		return nil, err
	}
	baseArtifactFound := baseFound && baseBinding.artifact != nil
	fileOccurrences, _, err := validateOutputBindingOccurrences(
		path, change, baseFound, baseArtifactFound,
	)
	if err != nil {
		return nil, err
	}
	if fileOccurrences == 0 {
		return bindings.delete(path)
	}
	file, artifact, err := changedOutputBindingValues(
		baseBinding, baseFound, baseArtifactFound, change,
	)
	if err != nil {
		return nil, err
	}
	fileChanged, err := outputFileChanged(baseBinding, baseFound, change)
	if err != nil {
		return nil, err
	}
	artifactChanged := change.artifactAfter != 0 &&
		(!baseArtifactFound || change.artifact != baseBinding.artifact)
	if err := validateOutputBindingCompanionChange(
		path, fileChanged, artifactChanged,
	); err != nil {
		return nil, err
	}
	if err := validateOutputBindingPair(path, file, artifact, nextDocument); err != nil {
		var mismatch *ArtifactContentMismatchError
		if errors.As(err, &mismatch) {
			mismatch.IncrementalCommit = true
			mismatch.PlanFileInherited = change.fileAfter == 0
			mismatch.PlanFileChanged = fileChanged
			mismatch.ArtifactInherited = change.artifactAfter == 0
			mismatch.ArtifactChanged = artifactChanged
		}
		return nil, err
	}
	if baseFound && !fileChanged && !artifactChanged {
		return bindings, nil
	}
	return bindings.put(path, sealOutputBinding(file, artifact))
}

func validateOutputBindingOccurrences(
	path string,
	change *outputBindingChange,
	baseFound, baseArtifactFound bool,
) (files, artifacts int, err error) {
	files = presenceValue(baseFound) + change.fileAfter - change.fileBefore
	artifacts = presenceValue(baseArtifactFound) +
		change.artifactAfter - change.artifactBefore
	if files < 0 || files > 1 || artifacts < 0 || artifacts > 1 ||
		change.fileAfter > 1 || change.artifactAfter > 1 {
		return 0, 0, fmt.Errorf("render output path %q is duplicated", path)
	}
	if path == renderplan.ConfigFilePath {
		if files != 1 || artifacts != 0 {
			return 0, 0, errors.New("render output must retain exactly one config file")
		}
		return files, artifacts, nil
	}
	if files != artifacts {
		return 0, 0, fmt.Errorf(
			"render output file and artifact presence differs at %q", path,
		)
	}
	return files, artifacts, nil
}

func changedOutputBindingValues(
	base *outputBinding,
	baseFound, baseArtifactFound bool,
	change *outputBindingChange,
) (*outputFileBinding, *renderartifact.Artifact, error) {
	file := change.file
	if change.fileAfter == 0 {
		if !baseFound {
			return nil, nil, errInvalidOutputDelta
		}
		file = base.file
	}
	artifact := change.artifact
	if change.artifactAfter == 0 && baseArtifactFound {
		artifact = base.artifact
	}
	return file, artifact, nil
}

func validateOutputBindingCompanionChange(
	path string,
	fileChanged, artifactChanged bool,
) error {
	if path == renderplan.ConfigFilePath {
		return nil
	}
	switch {
	case fileChanged && !artifactChanged:
		return fmt.Errorf("render output file %q changed without its artifact", path)
	case artifactChanged && !fileChanged:
		return fmt.Errorf("render artifact %q changed without its plan file", path)
	default:
		return nil
	}
}

func outputFileChanged(
	base *outputBinding,
	baseFound bool,
	change *outputBindingChange,
) (bool, error) {
	if change.fileAfter == 0 {
		return false, nil
	}
	if !baseFound {
		return true, nil
	}
	equal, err := exactOutputFileBinding(base.file, change.file)
	return !equal, err
}

func outputBindingChangeFor(
	changes map[string]*outputBindingChange,
	path string,
) *outputBindingChange {
	change := changes[path]
	if change == nil {
		change = &outputBindingChange{}
		changes[path] = change
	}
	return change
}

func validateBaseFileBinding(
	bindings *outputBindingTree,
	path string,
	file *outputFileBinding,
) error {
	binding, found, err := bindings.lookup(path)
	if err != nil {
		return err
	}
	if !found {
		return errInvalidOutputDelta
	}
	equal, err := exactOutputFileBinding(binding.file, file)
	if err != nil {
		return err
	}
	if !equal {
		return errInvalidOutputDelta
	}
	return nil
}

func validateBaseArtifactBinding(
	bindings *outputBindingTree,
	path string,
	artifact *renderartifact.Artifact,
) error {
	binding, found, err := bindings.lookup(path)
	if err != nil {
		return err
	}
	if !found || binding.artifact != artifact {
		return errInvalidOutputDelta
	}
	return nil
}

func fileChangeTouchesConfig(change renderplan.FileChange) bool {
	for _, record := range []*renderplan.FileRecord{change.Before, change.After} {
		if record == nil {
			continue
		}
		descriptor, err := record.Descriptor()
		if err == nil && (descriptor.Path == renderplan.ConfigFilePath ||
			descriptor.Kind == renderplan.FileKindConfig) {
			return true
		}
	}
	return false
}

func validateOutputBindingPair(
	path string,
	file *outputFileBinding,
	artifact *renderartifact.Artifact,
	document rendercontent.Document,
) error {
	if err := file.validate(); err != nil {
		return err
	}
	if file.descriptor.Path != path {
		return errInvalidOutputDelta
	}
	if path == renderplan.ConfigFilePath {
		if artifact != nil || file.descriptor.Kind != renderplan.FileKindConfig ||
			!file.descriptor.ReloadOnChange {
			return fmt.Errorf("render output config file %q does not match the rendered config", path)
		}
		if file.documentBacked {
			same, err := file.document.SameRoot(document)
			if err != nil {
				return err
			}
			if !same {
				return fmt.Errorf(
					"render output config file %q does not match the rendered config", path,
				)
			}
			return nil
		}
		return validateChangedConfigFile(document, &file.legacy)
	}
	if file.descriptor.Kind == renderplan.FileKindConfig || artifact == nil ||
		file.documentBacked {
		return errInvalidOutputDelta
	}
	return validateChangedArtifact(artifact, &file.legacy)
}

func validateChangedMapBinding(
	plan *renderplan.Snapshot,
	bindings *outputBindingTree,
	path string,
) error {
	declared, mapFound, err := plan.MapNamed(path)
	if err != nil {
		return err
	}
	binding, fileFound, err := bindings.lookup(path)
	if err != nil {
		return err
	}
	mapFileFound := fileFound && binding.file.descriptor.Kind == renderplan.FileKindMap &&
		binding.file.legacy.Seal == ""
	if mapFound != mapFileFound {
		return fmt.Errorf("render output plan map %q does not match its file", path)
	}
	if mapFound && (binding.file.documentBacked ||
		!mapMatchesFile(declared, &binding.file.legacy)) {
		return fmt.Errorf("render output plan map %q has inexact content", path)
	}
	return nil
}

func validateChangedCRTLists(changes []renderplan.NamedChange[renderplan.CRTList]) error {
	for _, change := range changes {
		if change.Before == nil && change.After == nil ||
			change.Before != nil && change.Name != change.Before.Path ||
			change.After != nil && change.Name != change.After.Path {
			return errInvalidOutputDelta
		}
	}
	return nil
}

func validateChangedFile(index int, file *renderplan.File) error {
	if !file.ContentKnown || file.Size < 0 || file.Size != int64(len(file.Content)) ||
		file.Digest != renderplan.DigestString(file.Content) {
		return fmt.Errorf("render output plan file %d has inexact content", index)
	}
	if !validFileKind(file.Kind) {
		return fmt.Errorf("render output plan file %q has unknown kind %q", file.Path, file.Kind)
	}
	return nil
}

func validateChangedConfigFile(document rendercontent.Document, file *renderplan.File) error {
	if file.Path != renderplan.ConfigFilePath || file.Kind != renderplan.FileKindConfig ||
		!file.ReloadOnChange {
		return fmt.Errorf("render output config file %q does not match the rendered config", file.Path)
	}
	writer := &exactStringWriter{expected: file.Content}
	written, err := document.WriteTo(writer)
	if errors.Is(err, errOutputContentMismatch) {
		return fmt.Errorf("render output config file %q does not match the rendered config", file.Path)
	}
	if err != nil {
		return err
	}
	if written != file.Size || writer.offset != len(file.Content) {
		return fmt.Errorf("render output config file %q does not match the rendered config", file.Path)
	}
	return nil
}

func mapMatchesFile(declared renderplan.Map, file *renderplan.File) bool {
	return declared.Path == file.Path && file.Kind == renderplan.FileKindMap &&
		renderplan.MapEntriesMatch(file.Content, declared.Entries)
}

func validateChangedArtifact(artifact *renderartifact.Artifact, file *renderplan.File) error {
	descriptor, err := artifact.Descriptor()
	if err != nil {
		return err
	}
	kind, reload := artifactPlanMetadata(descriptor)
	if descriptor.RuntimePath != file.Path || kind != file.Kind || reload != file.ReloadOnChange {
		return fmt.Errorf("render artifact %q metadata differs from its plan file", descriptor.RuntimePath)
	}
	content, err := artifact.Content()
	if err != nil {
		return err
	}
	return validateArtifactContent(content, descriptor.RuntimePath, file)
}
