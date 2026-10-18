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

package renderer

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func newIncrementalStatusPatchPlanFromIndexes(
	indexes map[string]*incrementalGroupIndex,
) (*templating.StatusPatchProjectionPlan, error) {
	groups := make([]string, 0, len(indexes))
	for group := range indexes {
		groups = append(groups, group)
	}
	slices.Sort(groups)
	entryCount := 0
	for _, group := range groups {
		index := indexes[group]
		if index == nil {
			return nil, fmt.Errorf("incremental group %q has no assembly index", group)
		}
		if err := index.validateAuthentication(); err != nil {
			return nil, err
		}
		entryCount += index.status.Len()
	}
	entries := make([]templating.StatusPatchProjectionPlanEntry, 0, entryCount)
	for _, group := range groups {
		index := indexes[group]
		var visitErr error
		index.status.Root().Walk(func(location []byte, indexed incrementalIndexedStatusPatchCall) bool {
			if !stringBytesEqual(indexed.location, location) {
				visitErr = errors.New("incremental statusPatch projection has a mismatched location")
				return true
			}
			if err := validateIncrementalPreparedStatusPatchCall(
				indexed.prepared,
				index.memo.authority,
				indexed.location,
			); err != nil {
				visitErr = err
				return true
			}
			entries = append(entries, templating.StatusPatchProjectionPlanEntry{
				Group: group, Entry: indexed.location, Projection: indexed.prepared.projection,
			})
			return false
		})
		if visitErr != nil {
			return nil, visitErr
		}
	}
	return templating.NewStatusPatchProjectionPlanFromEntries(entries)
}

func (r *incrementalRenderSession) finalizeStatusPatchPlanBootstrap() error {
	if !r.statusPlanBootstrapPending {
		return nil
	}
	plan, err := newIncrementalStatusPatchPlanFromIndexes(r.groupIndexes)
	if err != nil {
		return err
	}
	r.statusPlan = plan
	r.statusPlanBootstrapPending = false
	return nil
}

func replaceIncrementalStatusPatchPlanInstance(
	plan *templating.StatusPatchProjectionPlan,
	group string,
	previous, next *incrementalGroupIndex,
	id incrementalGroupInstanceID,
) (*templating.StatusPatchProjectionPlan, error) {
	if plan == nil {
		plan = templating.NewStatusPatchProjectionPlan()
	}
	if err := plan.ValidateAuthentication(); err != nil {
		return nil, err
	}
	if previous == nil || next == nil {
		return nil, errors.New("incremental statusPatch plan replacement has no group index")
	}
	if err := previous.validateAuthentication(); err != nil {
		return nil, err
	}
	if err := next.validateAuthentication(); err != nil {
		return nil, err
	}
	prefix := incrementalGroupInstanceKey(id)
	before, err := statusPatchProjectionsForPrefix(previous, prefix)
	if err != nil {
		return nil, err
	}
	after, err := statusPatchProjectionsForPrefix(next, prefix)
	if err != nil {
		return nil, err
	}
	locations := make([]string, 0, len(before)+len(after))
	for location := range before {
		locations = append(locations, location)
	}
	for location := range after {
		if _, exists := before[location]; !exists {
			locations = append(locations, location)
		}
	}
	slices.Sort(locations)
	updated := plan
	for _, location := range locations {
		current := before[location]
		replacement := after[location]
		if current == replacement {
			continue
		}
		updated, err = updated.ReplaceEntry(group, location, replacement)
		if err != nil {
			return nil, err
		}
	}
	return updated, nil
}

func statusPatchProjectionsForPrefix(
	index *incrementalGroupIndex,
	prefix []byte,
) (map[string]*templating.StatusPatchProjection, error) {
	result := make(map[string]*templating.StatusPatchProjection)
	var visitErr error
	index.status.Root().WalkPrefix(prefix, func(location []byte, indexed incrementalIndexedStatusPatchCall) bool {
		if !stringBytesEqual(indexed.location, location) {
			visitErr = errors.New("incremental statusPatch projection has a mismatched location")
			return true
		}
		if err := validateIncrementalPreparedStatusPatchCall(
			indexed.prepared,
			index.memo.authority,
			indexed.location,
		); err != nil {
			visitErr = err
			return true
		}
		result[indexed.location] = indexed.prepared.projection
		return false
	})
	return result, visitErr
}

type incrementalStatusPatchProjectionMemoKey struct {
	root *iradix.Node[incrementalIndexedStatusPatchCall]
}

type incrementalStatusPatchPlanAuthentication struct {
	snapshot *incrementalStateSnapshot
	plan     *templating.StatusPatchProjectionPlan
	seal     *incrementalStatusPatchPlanAuthentication
}

func authenticateIncrementalStatusPatchPlan(snapshot *incrementalStateSnapshot) {
	if snapshot == nil {
		return
	}
	auth := &incrementalStatusPatchPlanAuthentication{snapshot: snapshot, plan: snapshot.statusPlan}
	auth.seal = auth
	snapshot.statusAuth = auth
}

func validateIncrementalStatusPatchPlanAuthentication(snapshot *incrementalStateSnapshot) error {
	if snapshot == nil || snapshot.statusPlan == nil || snapshot.statusAuth == nil ||
		snapshot.statusAuth.seal != snapshot.statusAuth || snapshot.statusAuth.snapshot != snapshot ||
		snapshot.statusAuth.plan != snapshot.statusPlan {
		return errors.New("incremental statusPatch plan has invalid provenance")
	}
	if err := snapshot.statusPlan.ValidateAuthentication(); err != nil {
		return fmt.Errorf("incremental statusPatch plan failed authentication: %w", err)
	}
	return nil
}

type incrementalStatusPatchProjectionMemo struct {
	authority  *incrementalGroupMemoAuthority
	key        incrementalStatusPatchProjectionMemoKey
	projection *templating.StatusPatchProjection
	seal       *incrementalStatusPatchProjectionMemo
}

type incrementalPreparedStatusPatchCall struct {
	authority       *incrementalGroupMemoAuthority
	location        string
	namespace       string
	name            string
	apiVersion      string
	kind            string
	uid             string
	resourceVersion string
	listOwnership   string
	variants        string
	sourceTemplate  string
	sourceLine      int
	projection      *templating.StatusPatchProjection
	seal            *incrementalPreparedStatusPatchCall
}

func newIncrementalPreparedStatusPatchCall(
	authority *incrementalGroupMemoAuthority,
	location string,
	call *incrementalStatusPatchCall,
) (*incrementalPreparedStatusPatchCall, error) {
	if authority == nil || authority.seal != authority || location == "" {
		return nil, errors.New("incremental prepared statusPatch call has invalid provenance")
	}
	patch, err := decodeIncrementalStatusPatchProjectionCall(call)
	if err != nil {
		return nil, fmt.Errorf("preparing incremental statusPatch call %s: %w", describeIncrementalGroupLocation(location), err)
	}
	projection, err := templating.NewStatusPatchProjection([]templating.StatusPatch{patch})
	if err != nil {
		return nil, fmt.Errorf("preparing incremental statusPatch call %s: %w", describeIncrementalGroupLocation(location), err)
	}
	prepared := &incrementalPreparedStatusPatchCall{
		authority: authority, location: location,
		namespace: call.Namespace, name: call.Name, apiVersion: call.APIVersion, kind: call.Kind,
		uid: call.UID, resourceVersion: call.ResourceVersion, listOwnership: call.ListOwnership,
		variants: string(call.Variants), sourceTemplate: call.SourceTemplate, sourceLine: call.SourceLine,
		projection: projection,
	}
	prepared.seal = prepared
	return prepared, nil
}

func validateIncrementalPreparedStatusPatchCall(
	prepared *incrementalPreparedStatusPatchCall,
	authority *incrementalGroupMemoAuthority,
	location string,
) error {
	if prepared == nil || prepared.seal != prepared || prepared.authority != authority ||
		authority == nil || authority.seal != authority || prepared.location != location || prepared.projection == nil {
		return errors.New("incremental prepared statusPatch call has invalid provenance")
	}
	if err := prepared.projection.ValidateAuthentication(); err != nil {
		return fmt.Errorf("incremental prepared statusPatch call failed authentication: %w", err)
	}
	return nil
}

func incrementalPreparedStatusPatchCallMatches(
	prepared *incrementalPreparedStatusPatchCall,
	call *incrementalStatusPatchCall,
) bool {
	return prepared != nil && call != nil && prepared.namespace == call.Namespace && prepared.name == call.Name &&
		prepared.apiVersion == call.APIVersion && prepared.kind == call.Kind &&
		prepared.uid == call.UID && prepared.resourceVersion == call.ResourceVersion && prepared.listOwnership == call.ListOwnership &&
		stringBytesEqual(prepared.variants, call.Variants) && prepared.sourceTemplate == call.SourceTemplate &&
		prepared.sourceLine == call.SourceLine
}

func (p *incrementalPreparedStatusPatchCall) call() incrementalStatusPatchCall {
	return incrementalStatusPatchCall{
		Namespace: p.namespace, Name: p.name, APIVersion: p.apiVersion, Kind: p.kind,
		UID: p.uid, ResourceVersion: p.resourceVersion, ListOwnership: p.listOwnership,
		Variants: []byte(p.variants), SourceTemplate: p.sourceTemplate, SourceLine: p.sourceLine,
	}
}

func (m *incrementalGroupMemo) statusPatchProjection(
	key incrementalStatusPatchProjectionMemoKey,
) (*incrementalStatusPatchProjectionMemo, bool, error) {
	if !m.valid() || key.root == nil {
		return nil, false, errors.New("incremental statusPatch projection memo is unavailable")
	}
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	entry := m.state.status
	if entry == nil || entry.key != key {
		return nil, false, nil
	}
	if err := validateIncrementalStatusPatchProjectionMemo(entry, m.authority, key); err != nil {
		return nil, false, err
	}
	return entry, true, nil
}

func (m *incrementalGroupMemo) storeStatusPatchProjection(
	entry *incrementalStatusPatchProjectionMemo,
) (*incrementalStatusPatchProjectionMemo, error) {
	key := incrementalStatusPatchProjectionMemoKey{}
	if entry != nil {
		key = entry.key
	}
	if !m.valid() {
		return nil, errors.New("incremental statusPatch projection memo is unavailable")
	}
	if err := validateIncrementalStatusPatchProjectionMemo(entry, m.authority, key); err != nil {
		return nil, err
	}
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if existing := m.state.status; existing != nil && existing.key == key {
		if err := validateIncrementalStatusPatchProjectionMemo(existing, m.authority, key); err != nil {
			return nil, err
		}
		return existing, nil
	}
	m.state.status = entry
	return entry, nil
}

func validateIncrementalStatusPatchProjectionMemo(
	entry *incrementalStatusPatchProjectionMemo,
	authority *incrementalGroupMemoAuthority,
	key incrementalStatusPatchProjectionMemoKey,
) error {
	if entry == nil || entry.seal != entry || entry.authority != authority || entry.key != key ||
		entry.projection == nil {
		return errors.New("incremental statusPatch projection memo has invalid provenance")
	}
	if err := entry.projection.ValidateAuthentication(); err != nil {
		return fmt.Errorf("incremental statusPatch projection memo failed authentication: %w", err)
	}
	return nil
}

func (i *incrementalGroupIndex) compiledStatusPatchProjection() (*templating.StatusPatchProjection, error) {
	if err := i.validateAuthentication(); err != nil {
		return nil, err
	}
	root := i.status.Root()
	key := incrementalStatusPatchProjectionMemoKey{root: root}
	if cached, found, err := i.memo.statusPatchProjection(key); err != nil {
		return nil, err
	} else if found {
		return cached.projection, nil
	}
	parts := make([]*templating.StatusPatchProjection, 0, i.status.Len())
	var projectionErr error
	root.Walk(func(location []byte, indexed incrementalIndexedStatusPatchCall) bool {
		if !stringBytesEqual(indexed.location, location) {
			projectionErr = errors.New("incremental statusPatch projection has a mismatched location")
			return true
		}
		if err := validateIncrementalPreparedStatusPatchCall(indexed.prepared, i.memo.authority, indexed.location); err != nil {
			projectionErr = fmt.Errorf(
				"reading incremental statusPatch projection for call %s: %w", describeIncrementalGroupLocation(indexed.location), err,
			)
			return true
		}
		parts = append(parts, indexed.prepared.projection)
		return false
	})
	if projectionErr != nil {
		return nil, projectionErr
	}
	projection, err := templating.NewStatusPatchProjectionGroup(parts)
	if err != nil {
		return nil, fmt.Errorf("compiling incremental statusPatch projection: %w", err)
	}
	entry := &incrementalStatusPatchProjectionMemo{
		authority: i.memo.authority, key: key, projection: projection,
	}
	entry.seal = entry
	stored, err := i.memo.storeStatusPatchProjection(entry)
	if err != nil {
		return nil, err
	}
	return stored.projection, nil
}

func decodeIncrementalStatusPatchProjectionCall(call *incrementalStatusPatchCall) (templating.StatusPatch, error) {
	if call == nil {
		return templating.StatusPatch{}, errors.New("call is nil")
	}
	decoded, err := decodeResourceValue(call.Variants)
	if err != nil {
		return templating.StatusPatch{}, err
	}
	values, ok := decoded.(map[string]any)
	if !ok {
		return templating.StatusPatch{}, fmt.Errorf("variants must be an object, got %T", decoded)
	}
	variants := make(map[string]map[string]any, len(values))
	for phase, value := range values {
		status, valid := value.(map[string]any)
		if !valid {
			return templating.StatusPatch{}, fmt.Errorf("variant %q must be an object, got %T", phase, value)
		}
		variants[phase] = status
	}
	canonical, err := encodeIncrementalStatusPatchVariants(variants)
	if err != nil || !bytes.Equal(canonical, call.Variants) {
		return templating.StatusPatch{}, errors.New("variants are not canonical JSON")
	}
	return templating.StatusPatch{
		Namespace: call.Namespace, Name: call.Name, APIVersion: call.APIVersion, Kind: call.Kind,
		UID: call.UID, ResourceVersion: call.ResourceVersion, ListOwnership: call.ListOwnership,
		Variants: variants, SourceTemplate: call.SourceTemplate, SourceLine: call.SourceLine,
	}, nil
}
