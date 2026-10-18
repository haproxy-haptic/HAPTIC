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

package statuspatchprojection

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/persistenttree"
)

type planPhaseClaim struct {
	key      string
	metadata Metadata
	phase    string
}

type planLineageClaim struct {
	key      string
	metadata Metadata
	first    planIndexedPatch
	others   []planIndexedPatch
}

type planIndexedPatch struct {
	view     PatchView
	position int
}

type planGroup struct {
	key      string
	name     string
	root     *Root
	owner    any
	phases   []planPhaseClaim
	lineages []planLineageClaim
	seal     *planGroup
}

type planPhaseOwner struct {
	name       string
	groups     *iradix.Tree[*planGroup]
	groupsRoot *iradix.Node[*planGroup]
	seal       *planPhaseOwner
}

type planLineage struct {
	uid             string
	resourceVersion string
	listOwnership   string
	groups          *iradix.Tree[*planGroup]
	groupsRoot      *iradix.Node[*planGroup]
	seal            *planLineage
}

// PlanGroup identifies one exact projection root in a plan.
type PlanGroup struct {
	Name  string
	Root  *Root
	Owner any
}

// PlanEntry identifies one exact projection root at an ordered plan location.
type PlanEntry struct {
	Group string
	Entry string
	Root  *Root
	Owner any
}

// PlanRoot is an immutable exact group, phase-owner, and lineage index.
type PlanRoot struct {
	owner       any
	groups      *iradix.Tree[*planGroup]
	phaseOwners *iradix.Tree[*planPhaseOwner]
	lineages    *persistenttree.Tree[*planLineage]
	groupsRoot  *iradix.Node[*planGroup]
	phaseRoot   *iradix.Node[*planPhaseOwner]
	lineageRoot *persistenttree.Node[*planLineage]
	seal        *PlanRoot
}

// NewPlan returns an empty plan bound to owner.
func NewPlan(owner any) (*PlanRoot, error) {
	if owner == nil {
		return nil, errors.New("plan owner is nil")
	}
	return sealPlan(owner, iradix.New[*planGroup](), iradix.New[*planPhaseOwner](), persistenttree.New[*planLineage]()), nil
}

// NewPlanFromEntries returns a plan built atomically from exact ordered entries.
func NewPlanFromEntries(owner any, entries []PlanEntry) (*PlanRoot, error) {
	if owner == nil {
		return nil, errors.New("plan owner is nil")
	}
	groups := make([]*planGroup, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for index := range entries {
		entry := &entries[index]
		if entry.Group == "" {
			return nil, fmt.Errorf("plan entry %d has an empty group", index)
		}
		if entry.Root == nil {
			return nil, fmt.Errorf("plan entry %d has no projection root", index)
		}
		if err := entry.Root.Validate(entry.Owner); err != nil {
			return nil, fmt.Errorf("plan entry %d group %q: %w", index, entry.Group, err)
		}
		key := string(planOrderTuple(entry.Group, entry.Entry))
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("plan repeats an entry in group %q", entry.Group)
		}
		seen[key] = struct{}{}
		group, err := newPlanGroup(key, entry.Group, entry.Root, entry.Owner)
		if err != nil {
			return nil, fmt.Errorf("plan entry %d group %q: %w", index, entry.Group, err)
		}
		groups[index] = group
	}
	slices.SortFunc(groups, func(left, right *planGroup) int {
		return compareStrings(left.key, right.key)
	})
	return buildPlanFromGroups(owner, groups)
}

type planPhaseBuild struct {
	name   string
	groups []*planGroup
}

type planLineageBuild struct {
	uid             string
	resourceVersion string
	listOwnership   string
	groups          []*planGroup
}

func buildPlanFromGroups(owner any, groups []*planGroup) (*PlanRoot, error) {
	phaseBuilds := make(map[string]*planPhaseBuild)
	lineageBuilds := make(map[string]*planLineageBuild)
	for _, group := range groups {
		if err := validatePlanGroup(group); err != nil {
			return nil, err
		}
		for index := range group.phases {
			claim := &group.phases[index]
			build := phaseBuilds[claim.key]
			if build == nil {
				build = &planPhaseBuild{name: group.name}
				phaseBuilds[claim.key] = build
			} else if build.name != group.name {
				return nil, fmt.Errorf(
					"target %s/%s %s %s phase %q has conflicting groups %q and %q",
					claim.metadata.Namespace, claim.metadata.Name, claim.metadata.APIVersion,
					claim.metadata.Kind, claim.phase, build.name, group.name,
				)
			}
			build.groups = append(build.groups, group)
		}
		for index := range group.lineages {
			claim := &group.lineages[index]
			build := lineageBuilds[claim.key]
			if build == nil {
				build = &planLineageBuild{
					uid: claim.metadata.UID, resourceVersion: claim.metadata.ResourceVersion, listOwnership: claim.metadata.ListOwnership,
				}
				lineageBuilds[claim.key] = build
			} else if build.uid != claim.metadata.UID || build.resourceVersion != claim.metadata.ResourceVersion || build.listOwnership != claim.metadata.ListOwnership {
				return nil, fmt.Errorf("%s/%s has conflicting source lineage", claim.metadata.Namespace, claim.metadata.Name)
			}
			build.groups = append(build.groups, group)
		}
	}

	groupTxn := iradix.New[*planGroup]().Txn()
	for _, group := range groups {
		groupTxn.Insert([]byte(group.key), group)
	}
	phaseTxn := iradix.New[*planPhaseOwner]().Txn()
	for _, key := range sortedPlanBuildKeys(phaseBuilds) {
		build := phaseBuilds[key]
		owners := buildPlanGroupOwners(build.groups)
		phaseOwner := &planPhaseOwner{name: build.name, groups: owners, groupsRoot: owners.Root()}
		phaseOwner.seal = phaseOwner
		phaseTxn.Insert([]byte(key), phaseOwner)
	}
	lineageEntries := make([]persistenttree.Entry[*planLineage], 0, len(lineageBuilds))
	for _, key := range sortedPlanBuildKeys(lineageBuilds) {
		build := lineageBuilds[key]
		owners := buildPlanGroupOwners(build.groups)
		lineage := &planLineage{
			uid: build.uid, resourceVersion: build.resourceVersion, listOwnership: build.listOwnership, groups: owners, groupsRoot: owners.Root(),
		}
		lineage.seal = lineage
		lineageEntries = append(lineageEntries, persistenttree.Entry[*planLineage]{Key: key, Value: lineage})
	}
	lineages, err := persistenttree.NewFromSorted(lineageEntries)
	if err != nil {
		return nil, err
	}
	return sealPlan(owner, groupTxn.Commit(), phaseTxn.Commit(), lineages), nil
}

func buildPlanGroupOwners(groups []*planGroup) *iradix.Tree[*planGroup] {
	owners := iradix.New[*planGroup]().Txn()
	for _, group := range groups {
		owners.Insert([]byte(group.key), group)
	}
	return owners.Commit()
}

func sortedPlanBuildKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// Validate verifies exact plan ownership and persistent-root identity.
func (p *PlanRoot) Validate(owner any) error {
	if p == nil || p.seal != p || owner == nil || !sameOwner(p.owner, owner) ||
		p.groups == nil || p.phaseOwners == nil || p.lineages == nil ||
		p.groupsRoot != p.groups.Root() || p.phaseRoot != p.phaseOwners.Root() ||
		p.lineageRoot != p.lineages.Root() {
		return errors.New("plan has invalid provenance")
	}
	return nil
}

// ExactGroup reports whether group already points at root with the exact owner.
func (p *PlanRoot) ExactGroup(owner any, group string, root *Root, rootOwner any) (bool, error) {
	return p.ExactEntry(owner, group, "", root, rootOwner)
}

// ExactEntry reports whether an ordered group entry already points at root with the exact owner.
func (p *PlanRoot) ExactEntry(
	owner any,
	group, entry string,
	root *Root,
	rootOwner any,
) (bool, error) {
	if err := p.Validate(owner); err != nil {
		return false, err
	}
	cached, found := p.groups.Root().Get(planOrderTuple(group, entry))
	if !found {
		return root == nil, nil
	}
	if err := validatePlanGroup(cached); err != nil {
		return false, err
	}
	return cached.root == root && sameOwner(cached.owner, rootOwner), nil
}

// Replace returns a fresh plan root after atomically replacing one group.
func (p *PlanRoot) Replace(
	owner, nextOwner any,
	group string,
	root *Root,
	rootOwner any,
) (*PlanRoot, error) {
	return p.ReplaceEntry(owner, nextOwner, group, "", root, rootOwner)
}

// ReplaceEntry returns a fresh plan root after atomically replacing one ordered group entry.
func (p *PlanRoot) ReplaceEntry(
	owner, nextOwner any,
	group, entry string,
	root *Root,
	rootOwner any,
) (*PlanRoot, error) {
	if err := p.Validate(owner); err != nil {
		return nil, err
	}
	if nextOwner == nil || group == "" {
		return nil, errors.New("plan replacement has invalid owner or group")
	}
	if root != nil {
		if err := root.Validate(rootOwner); err != nil {
			return nil, fmt.Errorf("plan group %q: %w", group, err)
		}
	}
	groups := p.groups.Txn()
	phases := p.phaseOwners.Txn()
	lineages := p.lineages.Txn()
	entryKey := planOrderTuple(group, entry)
	if current, found := p.groups.Root().Get(entryKey); found {
		if err := removePlanGroup(current, groups, phases, lineages); err != nil {
			return nil, err
		}
	}
	if root != nil {
		candidate, err := newPlanGroup(string(entryKey), group, root, rootOwner)
		if err != nil {
			return nil, err
		}
		if err := addPlanGroup(candidate, groups, phases, lineages); err != nil {
			return nil, err
		}
	}
	return sealPlan(nextOwner, groups.Commit(), phases.Commit(), lineages.Commit()), nil
}

// Groups returns the exact group roots in lexical group order.
func (p *PlanRoot) Groups(owner any) ([]PlanGroup, error) {
	if err := p.Validate(owner); err != nil {
		return nil, err
	}
	result := make([]PlanGroup, 0, p.groups.Len())
	var visitErr error
	p.groups.Root().Walk(func(_ []byte, group *planGroup) bool {
		if err := validatePlanGroup(group); err != nil {
			visitErr = err
			return true
		}
		result = append(result, PlanGroup{Name: group.name, Root: group.root, Owner: group.owner})
		return false
	})
	return result, visitErr
}

// VisitGroups visits exact group roots in lexical group order.
func (p *PlanRoot) VisitGroups(owner any, visit func(PlanGroup) error) error {
	if err := p.Validate(owner); err != nil {
		return err
	}
	if visit == nil {
		return errors.New("plan group visitor is nil")
	}
	var visitErr error
	p.groups.Root().Walk(func(_ []byte, group *planGroup) bool {
		if err := validatePlanGroup(group); err != nil {
			visitErr = err
			return true
		}
		visitErr = visit(PlanGroup{Name: group.name, Root: group.root, Owner: group.owner})
		return visitErr != nil
	})
	return visitErr
}

// TargetCount returns the number of unique target resources.
func (p *PlanRoot) TargetCount(owner any) (int, error) {
	if err := p.Validate(owner); err != nil {
		return 0, err
	}
	return p.lineages.Len(), nil
}

// ContainsTarget reports whether a target identity is present in the plan.
func (p *PlanRoot) ContainsTarget(owner any, namespace, name, apiVersion, kind string) (bool, error) {
	if err := p.Validate(owner); err != nil {
		return false, err
	}
	lineage, found := p.lineages.Root().Get(planTuple(namespace, name, apiVersion, kind))
	if !found {
		return false, nil
	}
	if err := validatePlanLineage(lineage); err != nil {
		return false, err
	}
	return true, nil
}

// ValidateLineage verifies that a direct patch can be overlaid by the plan.
func (p *PlanRoot) ValidateLineage(
	owner any,
	namespace, name, apiVersion, kind, uid, resourceVersion string,
	listOwnership ...string,
) error {
	if err := p.Validate(owner); err != nil {
		return err
	}
	key := planTuple(namespace, name, apiVersion, kind)
	lineage, found := p.lineages.Root().Get(key)
	if !found {
		return nil
	}
	if err := validatePlanLineage(lineage); err != nil {
		return err
	}
	ownership := ""
	if len(listOwnership) > 0 {
		ownership = listOwnership[0]
	}
	if lineage.uid != uid || lineage.resourceVersion != resourceVersion || lineage.listOwnership != ownership {
		return fmt.Errorf("%s/%s has conflicting source lineage", namespace, name)
	}
	return nil
}

func newPlanGroup(key, name string, root *Root, owner any) (*planGroup, error) {
	group := &planGroup{key: key, name: name, root: root, owner: owner}
	phaseKeys := make(map[string]struct{})
	lineages := make(map[string]int)
	position := 0
	err := root.Visit(owner, func(projected PatchView) error {
		metadata, err := projected.Metadata()
		if err != nil {
			return err
		}
		if err := appendPlanGroupLineage(group, lineages, &metadata, planIndexedPatch{view: projected, position: position}); err != nil {
			return err
		}
		position++
		return projected.VisitPhases(func(phase PhaseView) error {
			phaseName, err := phase.Name()
			if err != nil {
				return err
			}
			claimKey := string(planTuple(
				metadata.Namespace, metadata.Name, metadata.APIVersion, metadata.Kind, phaseName,
			))
			if _, exists := phaseKeys[claimKey]; exists {
				return nil
			}
			phaseKeys[claimKey] = struct{}{}
			group.phases = append(group.phases, planPhaseClaim{
				key: claimKey, metadata: metadata, phase: phaseName,
			})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(group.phases, func(left, right planPhaseClaim) int {
		return compareStrings(left.key, right.key)
	})
	slices.SortFunc(group.lineages, func(left, right planLineageClaim) int {
		return compareStrings(left.key, right.key)
	})
	group.seal = group
	return group, nil
}

func appendPlanGroupLineage(group *planGroup, lineages map[string]int, metadata *Metadata, indexed planIndexedPatch) error {
	key := string(planTuple(metadata.Namespace, metadata.Name, metadata.APIVersion, metadata.Kind))
	if index, found := lineages[key]; found {
		previous := &group.lineages[index]
		if previous.metadata.UID != metadata.UID || previous.metadata.ResourceVersion != metadata.ResourceVersion || previous.metadata.ListOwnership != metadata.ListOwnership {
			return fmt.Errorf("plan group %q: %s/%s has conflicting source lineage", group.name, metadata.Namespace, metadata.Name)
		}
		previous.others = append(previous.others, indexed)
		return nil
	}
	lineages[key] = len(group.lineages)
	group.lineages = append(group.lineages, planLineageClaim{key: key, metadata: *metadata, first: indexed})
	return nil
}

func addPlanGroup(
	group *planGroup,
	groups *iradix.Txn[*planGroup],
	phases *iradix.Txn[*planPhaseOwner],
	lineages *persistenttree.Txn[*planLineage],
) error {
	if err := validatePlanGroup(group); err != nil {
		return err
	}
	if err := validatePlanGroupPhaseClaims(group, phases); err != nil {
		return err
	}
	if err := validatePlanGroupLineageClaims(group, lineages); err != nil {
		return err
	}
	groups.Insert([]byte(group.key), group)
	insertPlanGroupPhases(group, phases)
	insertPlanGroupLineages(group, lineages)
	return nil
}

func validatePlanGroupPhaseClaims(group *planGroup, phases *iradix.Txn[*planPhaseOwner]) error {
	for index := range group.phases {
		claim := &group.phases[index]
		if existing, found := phases.Root().Get([]byte(claim.key)); found {
			if err := validatePlanPhaseOwner(existing); err != nil {
				return err
			}
			if existing.name != group.name {
				return fmt.Errorf(
					"target %s/%s %s %s phase %q has conflicting groups %q and %q",
					claim.metadata.Namespace, claim.metadata.Name, claim.metadata.APIVersion,
					claim.metadata.Kind, claim.phase, existing.name, group.name,
				)
			}
		}
	}
	return nil
}

func validatePlanGroupLineageClaims(group *planGroup, lineages *persistenttree.Txn[*planLineage]) error {
	for index := range group.lineages {
		claim := &group.lineages[index]
		if existing, found := lineages.Get([]byte(claim.key)); found {
			if err := validatePlanLineage(existing); err != nil {
				return err
			}
			if existing.uid != claim.metadata.UID || existing.resourceVersion != claim.metadata.ResourceVersion || existing.listOwnership != claim.metadata.ListOwnership {
				return fmt.Errorf("%s/%s has conflicting source lineage", claim.metadata.Namespace, claim.metadata.Name)
			}
		}
	}
	return nil
}

func insertPlanGroupPhases(group *planGroup, phases *iradix.Txn[*planPhaseOwner]) {
	for index := range group.phases {
		claim := &group.phases[index]
		existing, found := phases.Root().Get([]byte(claim.key))
		owners := iradix.New[*planGroup]()
		if found {
			owners = existing.groups
		}
		ownerTxn := owners.Txn()
		ownerTxn.Insert([]byte(group.key), group)
		owners = ownerTxn.Commit()
		owner := &planPhaseOwner{name: group.name, groups: owners, groupsRoot: owners.Root()}
		owner.seal = owner
		phases.Insert([]byte(claim.key), owner)
	}
}

func insertPlanGroupLineages(group *planGroup, lineages *persistenttree.Txn[*planLineage]) {
	for index := range group.lineages {
		claim := &group.lineages[index]
		existing, found := lineages.Get([]byte(claim.key))
		var owners *iradix.Tree[*planGroup]
		if found {
			owners = existing.groups
		} else {
			owners = iradix.New[*planGroup]()
		}
		ownerTxn := owners.Txn()
		ownerTxn.Insert([]byte(group.key), group)
		lineage := &planLineage{
			uid: claim.metadata.UID, resourceVersion: claim.metadata.ResourceVersion, listOwnership: claim.metadata.ListOwnership,
			groups: ownerTxn.Commit(),
		}
		lineage.groupsRoot = lineage.groups.Root()
		lineage.seal = lineage
		lineages.Insert([]byte(claim.key), lineage)
	}
}

func removePlanGroup(
	group *planGroup,
	groups *iradix.Txn[*planGroup],
	phases *iradix.Txn[*planPhaseOwner],
	lineages *persistenttree.Txn[*planLineage],
) error {
	if err := validatePlanGroup(group); err != nil {
		return err
	}
	if err := removePlanGroupPhases(group, phases); err != nil {
		return err
	}
	if err := removePlanGroupLineages(group, lineages); err != nil {
		return err
	}
	stored, found := groups.Root().Get([]byte(group.key))
	if !found || stored != group {
		return errors.New("plan group index has invalid provenance")
	}
	groups.Delete([]byte(group.key))
	return nil
}

func removePlanGroupPhases(group *planGroup, phases *iradix.Txn[*planPhaseOwner]) error {
	for index := range group.phases {
		claim := &group.phases[index]
		owner, found := phases.Root().Get([]byte(claim.key))
		if !found || validatePlanPhaseOwner(owner) != nil || owner.name != group.name {
			return errors.New("plan phase owner has invalid provenance")
		}
		stored, owned := owner.groups.Root().Get([]byte(group.key))
		if !owned || stored != group {
			return errors.New("plan phase owner has invalid provenance")
		}
		ownerTxn := owner.groups.Txn()
		ownerTxn.Delete([]byte(group.key))
		owners := ownerTxn.Commit()
		if owners.Len() == 0 {
			phases.Delete([]byte(claim.key))
			continue
		}
		replacement := &planPhaseOwner{name: owner.name, groups: owners, groupsRoot: owners.Root()}
		replacement.seal = replacement
		phases.Insert([]byte(claim.key), replacement)
	}
	return nil
}

func removePlanGroupLineages(group *planGroup, lineages *persistenttree.Txn[*planLineage]) error {
	for index := range group.lineages {
		claim := &group.lineages[index]
		lineage, found := lineages.Get([]byte(claim.key))
		if !found || validatePlanLineage(lineage) != nil {
			return errors.New("plan lineage has invalid provenance")
		}
		stored, owned := lineage.groups.Root().Get([]byte(group.key))
		if !owned || stored != group {
			return errors.New("plan lineage owner has invalid provenance")
		}
		ownerTxn := lineage.groups.Txn()
		ownerTxn.Delete([]byte(group.key))
		owners := ownerTxn.Commit()
		if owners.Len() == 0 {
			lineages.Delete([]byte(claim.key))
			continue
		}
		replacement := &planLineage{
			uid: lineage.uid, resourceVersion: lineage.resourceVersion, listOwnership: lineage.listOwnership, groups: owners, groupsRoot: owners.Root(),
		}
		replacement.seal = replacement
		lineages.Insert([]byte(claim.key), replacement)
	}
	return nil
}

func validatePlanGroup(group *planGroup) error {
	if group == nil || group.seal != group || group.key == "" || group.name == "" || group.root == nil {
		return errors.New("plan group has invalid provenance")
	}
	if err := group.root.Validate(group.owner); err != nil {
		return fmt.Errorf("plan group has invalid provenance: %w", err)
	}
	return nil
}

func validatePlanPhaseOwner(owner *planPhaseOwner) error {
	if owner == nil || owner.seal != owner || owner.name == "" || owner.groups == nil ||
		owner.groupsRoot != owner.groups.Root() || owner.groups.Len() == 0 {
		return errors.New("plan phase owner has invalid provenance")
	}
	return nil
}

func validatePlanLineage(lineage *planLineage) error {
	if lineage == nil || lineage.seal != lineage || lineage.groups == nil ||
		lineage.groupsRoot != lineage.groups.Root() || lineage.groups.Len() == 0 {
		return errors.New("plan lineage has invalid provenance")
	}
	return nil
}

func sealPlan(
	owner any,
	groups *iradix.Tree[*planGroup],
	phases *iradix.Tree[*planPhaseOwner],
	lineages *persistenttree.Tree[*planLineage],
) *PlanRoot {
	plan := &PlanRoot{
		owner: owner, groups: groups, phaseOwners: phases, lineages: lineages,
		groupsRoot: groups.Root(), phaseRoot: phases.Root(), lineageRoot: lineages.Root(),
	}
	plan.seal = plan
	return plan
}

func planTuple(parts ...string) []byte {
	length := 0
	for _, part := range parts {
		length += binary.MaxVarintLen64 + len(part)
	}
	result := make([]byte, 0, length)
	var scratch [binary.MaxVarintLen64]byte
	for _, part := range parts {
		count := binary.PutUvarint(scratch[:], uint64(len(part)))
		result = append(result, scratch[:count]...)
		result = append(result, part...)
	}
	return result
}

func planOrderTuple(parts ...string) []byte {
	length := 0
	for _, part := range parts {
		length += len(part) + 2
	}
	result := make([]byte, 0, length)
	for _, part := range parts {
		for index := range len(part) {
			if part[index] == 0 {
				result = append(result, 0, 0xff)
				continue
			}
			result = append(result, part[index])
		}
		result = append(result, 0, 0)
	}
	return result
}
