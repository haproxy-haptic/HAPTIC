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

package renderplan

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotOwnsSourceAndEveryLegacyCopy(t *testing.T) {
	source := snapshotPlanFixture(3)
	want := source.Clone()
	snapshot := mustPlanSnapshot(t, NewAuthority(), source, nil)

	poisonPlanFixture(source)
	first, err := snapshot.LegacyCopy()
	require.NoError(t, err)
	assert.Equal(t, want, first)
	assert.True(t, ExactlyEqual(want, first))
	firstSection := &first.Sections[0]
	firstWeight := first.Backends["backend-000000"].Servers[0].Weight

	poisonPlanFixture(first)
	second, err := snapshot.LegacyCopy()
	require.NoError(t, err)
	assert.Equal(t, want, second)
	assert.True(t, ExactlyEqual(want, second))
	assert.NotSame(t, firstSection, &second.Sections[0])
	assert.NotSame(t, firstWeight, second.Backends["backend-000000"].Servers[0].Weight)
}

// TestSnapshotSharedPlanIsBuiltOnceAndAliasesTheSealedRecords pins the
// read-only view: one object per snapshot, equal to the owned copy, and its
// slices are the sealed entries' own, so no copy is made.
func TestSnapshotSharedPlanIsBuiltOnceAndAliasesTheSealedRecords(t *testing.T) {
	source := snapshotPlanFixture(3)
	want := source.Clone()
	snapshot := mustPlanSnapshot(t, NewAuthority(), source, nil)
	poisonPlanFixture(source)

	shared, err := snapshot.SharedPlan()
	require.NoError(t, err)
	assert.Equal(t, want, shared)
	assert.True(t, ExactlyEqual(want, shared))
	assert.Equal(t, want.ID, shared.ID)
	again, err := snapshot.SharedPlan()
	require.NoError(t, err)
	assert.Same(t, shared, again)

	entry, err := findSnapshotEntry(
		snapshot.authority, backendSnapshotCollection, snapshot.root.backends,
		snapshotKey{index: -1, name: "backend-000000"},
	)
	require.NoError(t, err)
	assert.Same(t, &entry.value.value.Servers[0], &shared.Backends["backend-000000"].Servers[0])
	owned, err := snapshot.LegacyCopy()
	require.NoError(t, err)
	assert.NotSame(t, &entry.value.value.Servers[0], &owned.Backends["backend-000000"].Servers[0])
}

func TestSnapshotReusesExactRootEntriesAndSubtrees(t *testing.T) {
	authority := NewAuthority()
	basePlan := snapshotPlanFixture(7)
	base := mustPlanSnapshot(t, authority, basePlan, nil)
	detached, err := base.LegacyCopy()
	require.NoError(t, err)
	exact := mustPlanSnapshot(t, authority, detached, base)
	assert.Same(t, base, exact)
	same, err := base.SameRoot(exact)
	require.NoError(t, err)
	assert.True(t, same)

	changedPlan, err := base.LegacyCopy()
	require.NoError(t, err)
	backend := changedPlan.Backends["backend-000000"]
	backend.Servers[0].Address = "192.0.2.10"
	changedPlan.Backends[backend.Name] = backend
	changedPlan.ComputeID()
	changed := mustPlanSnapshot(t, authority, changedPlan, base)
	assert.NotSame(t, base, changed)
	assert.Same(t, base.root.sections, changed.root.sections)
	assert.Same(t, base.root.profiles, changed.root.profiles)
	assert.Same(t, base.root.maps, changed.root.maps)
	assert.Same(t, base.root.crtLists, changed.root.crtLists)
	assert.Same(t, base.root.files, changed.root.files)
	assert.NotSame(t, base.root.backends, changed.root.backends)
	assert.Same(t, base.root.backends.root.right, changed.root.backends.root.right)

	baseChangedEntry := mustSnapshotEntry(
		t, base.root.backends, snapshotKey{index: -1, name: "backend-000000"},
	)
	changedEntry := mustSnapshotEntry(
		t, changed.root.backends, snapshotKey{index: -1, name: "backend-000000"},
	)
	assert.NotSame(t, baseChangedEntry, changedEntry)
	for index := 1; index < 7; index++ {
		key := snapshotKey{index: -1, name: fmt.Sprintf("backend-%06d", index)}
		assert.Same(t, mustSnapshotEntry(t, base.root.backends, key),
			mustSnapshotEntry(t, changed.root.backends, key))
	}
	equal, err := base.ExactEqual(changed)
	require.NoError(t, err)
	assert.False(t, equal)
}

func TestSnapshotNeverUsesDigestsToAuthorizeReuse(t *testing.T) {
	authority := NewAuthority()
	basePlan := snapshotPlanFixture(2)
	base := mustPlanSnapshot(t, authority, basePlan, nil)

	changedText := basePlan.Clone()
	changedText.Sections[0].Text = "X" + changedText.Sections[0].Text[1:]
	changedText.ComputeID()
	require.Equal(t, basePlan.ID, changedText.ID,
		"section exact bytes are deliberately outside the canonical digest")
	changed := mustPlanSnapshot(t, authority, changedText, base)
	assert.NotSame(t, base, changed)
	equal, err := base.ExactEqual(changed)
	require.NoError(t, err)
	assert.False(t, equal)

	changedFile := basePlan.Clone()
	changedFile.Files[0].Content = "X" + changedFile.Files[0].Content[1:]
	changedFile.ComputeID()
	require.Equal(t, basePlan.ID, changedFile.ID)
	changed = mustPlanSnapshot(t, authority, changedFile, base)
	assert.NotSame(t, base, changed)
	equal, err = base.ExactEqual(changed)
	require.NoError(t, err)
	assert.False(t, equal)
}

func TestSnapshotExactEqualAcrossAuthorities(t *testing.T) {
	plan := snapshotPlanFixture(4)
	left := mustPlanSnapshot(t, NewAuthority(), plan, nil)
	right := mustPlanSnapshot(t, NewAuthority(), plan.Clone(), nil)
	same, err := left.SameRoot(right)
	require.NoError(t, err)
	assert.False(t, same)
	equal, err := left.ExactEqual(right)
	require.NoError(t, err)
	assert.True(t, equal)

	differentID := plan.Clone()
	differentID.ID = "untrusted-id"
	idSnapshot := mustPlanSnapshot(t, NewAuthority(), differentID, nil)
	equal, err = left.ExactEqual(idSnapshot)
	require.NoError(t, err)
	assert.True(t, equal, "the derived ID is never an equality proof")
	id, err := idSnapshot.ID()
	require.NoError(t, err)
	assert.Equal(t, "untrusted-id", id)

	differentSchema := plan.Clone()
	differentSchema.SchemaVersion++
	differentSchema.ComputeID()
	schemaSnapshot := mustPlanSnapshot(t, NewAuthority(), differentSchema, nil)
	equal, err = left.ExactEqual(schemaSnapshot)
	require.NoError(t, err)
	assert.False(t, equal)
}

// TestSnapshotExactEqualSeesServerTemplate pins that a server's slot count is
// part of exact equality, so a snapshot never treats a resized template as reusable.
func TestSnapshotExactEqualSeesServerTemplate(t *testing.T) {
	plan := snapshotPlanFixture(1)
	resized := plan.Clone()
	backend := resized.Backends["backend-000000"]
	servers := append([]Server(nil), backend.Servers...)
	servers[0].Template = 6
	backend.Servers = servers
	resized.Backends["backend-000000"] = backend

	equal, err := mustPlanSnapshot(t, NewAuthority(), plan, nil).ExactEqual(mustPlanSnapshot(t, NewAuthority(), resized, nil))
	require.NoError(t, err)
	assert.False(t, equal)
}

func TestSnapshotPreservesNilAndEmptyCollections(t *testing.T) {
	nilPlan := exactEmptyPlan()
	emptyPlan := exactEmptyPlan()
	emptyPlan.Sections = []Section{}
	emptyPlan.Backends = map[string]Backend{}
	emptyPlan.Profiles = map[string]Profile{}
	emptyPlan.Maps = map[string]Map{}
	emptyPlan.CRTLists = map[string]CRTList{}
	emptyPlan.Files = []File{}
	emptyPlan.ComputeID()

	nilSnapshot := mustPlanSnapshot(t, NewAuthority(), nilPlan, nil)
	emptySnapshot := mustPlanSnapshot(t, NewAuthority(), emptyPlan, nil)
	equal, err := nilSnapshot.ExactEqual(emptySnapshot)
	require.NoError(t, err)
	assert.False(t, equal)

	nilCopy, err := nilSnapshot.LegacyCopy()
	require.NoError(t, err)
	assert.Nil(t, nilCopy.Sections)
	assert.Nil(t, nilCopy.Backends)
	assert.Nil(t, nilCopy.Profiles)
	assert.Nil(t, nilCopy.Maps)
	assert.Nil(t, nilCopy.CRTLists)
	assert.Nil(t, nilCopy.Files)

	emptyCopy, err := emptySnapshot.LegacyCopy()
	require.NoError(t, err)
	assert.NotNil(t, emptyCopy.Sections)
	assert.NotNil(t, emptyCopy.Backends)
	assert.NotNil(t, emptyCopy.Profiles)
	assert.NotNil(t, emptyCopy.Maps)
	assert.NotNil(t, emptyCopy.CRTLists)
	assert.NotNil(t, emptyCopy.Files)
}

func TestSnapshotRejectsPoisonedAuthenticationAndInexactSources(t *testing.T) {
	authority := NewAuthority()
	require.NoError(t, authority.ValidateAuthentication())
	copyAuthority := *authority
	require.ErrorIs(t, copyAuthority.ValidateAuthentication(), errInvalidSnapshotAuthority)
	var zeroAuthority Authority
	require.ErrorIs(t, zeroAuthority.ValidateAuthentication(), errInvalidSnapshotAuthority)

	plan := snapshotPlanFixture(7)
	snapshot := mustPlanSnapshot(t, authority, plan, nil)
	require.NoError(t, snapshot.ValidateAuthentication())
	require.NoError(t, authority.ValidateSnapshot(snapshot))
	require.ErrorIs(t, NewAuthority().ValidateSnapshot(snapshot), errForeignSnapshot)
	shallow := *snapshot
	require.ErrorIs(t, shallow.ValidateAuthentication(), errInvalidSnapshot)
	require.ErrorIs(t, authority.ValidateSnapshot(&shallow), errInvalidSnapshot)
	poisoned := *snapshot
	poisoned.root = nil
	require.ErrorIs(t, poisoned.ValidateAuthentication(), errInvalidSnapshot)
	poisoned = *snapshot
	poisoned.entries++
	require.ErrorIs(t, poisoned.ValidateAuthentication(), errInvalidSnapshot)
	poisoned = *snapshot
	poisoned.authority = NewAuthority()
	require.ErrorIs(t, poisoned.ValidateAuthentication(), errInvalidSnapshot)
	var zeroSnapshot Snapshot
	require.ErrorIs(t, zeroSnapshot.ValidateAuthentication(), errInvalidSnapshot)
	_, err := (*Snapshot)(nil).Len()
	require.ErrorIs(t, err, errInvalidSnapshot)
	_, err = snapshot.SameRoot(nil)
	require.ErrorIs(t, err, errInvalidSnapshot)

	foreign := mustPlanSnapshot(t, NewAuthority(), plan.Clone(), nil)
	_, err = NewSnapshot(authority, plan.Clone(), foreign)
	require.ErrorIs(t, err, errForeignSnapshot)
	_, err = NewSnapshot(nil, plan.Clone(), nil)
	require.ErrorIs(t, err, errInvalidSnapshotAuthority)
	require.ErrorIs(t, (*Authority)(nil).ValidateSnapshot(snapshot), errInvalidSnapshotAuthority)
	require.ErrorIs(t, authority.ValidateSnapshot(nil), errInvalidSnapshot)
	_, err = NewSnapshot(authority, nil, nil)
	require.ErrorIs(t, err, errNilSnapshotPlan)
	assertInexactPlansRejected(t, authority, plan)
}

func assertInexactPlansRejected(t *testing.T, authority *Authority, plan *Plan) {
	t.Helper()
	inexact := plan.Clone()
	inexact.Sections[0].TextKnown = false
	_, err := NewSnapshot(authority, inexact, nil)
	require.ErrorIs(t, err, errInexactSnapshotPlan)
	inexact = plan.Clone()
	backend := inexact.Backends["backend-000000"]
	backend.ContentKnown = false
	inexact.Backends[backend.Name] = backend
	_, err = NewSnapshot(authority, inexact, nil)
	require.ErrorIs(t, err, errInexactSnapshotPlan)
	inexact = plan.Clone()
	inexact.Files[0].ContentKnown = false
	_, err = NewSnapshot(authority, inexact, nil)
	require.ErrorIs(t, err, errInexactSnapshotPlan)
}

func TestSnapshotTraversalFailsClosedOnPrivateDeepPoison(t *testing.T) {
	plan := snapshotPlanFixture(15)
	left := mustPlanSnapshot(t, NewAuthority(), plan, nil)
	right := mustPlanSnapshot(t, NewAuthority(), plan.Clone(), nil)
	deep := left.root.backends.root.left.left
	require.NotNil(t, deep)
	originalSeal := deep.seal
	deep.seal = nil
	require.NoError(t, left.ValidateAuthentication())
	_, err := left.LegacyCopy()
	require.ErrorIs(t, err, errInvalidSnapshot)
	_, err = left.ExactEqual(right)
	require.ErrorIs(t, err, errInvalidSnapshot)
	deep.seal = originalSeal
	require.NoError(t, left.ValidateAuthentication())
	equal, err := left.ExactEqual(right)
	require.NoError(t, err)
	assert.True(t, equal)
}

func TestSnapshotConcurrentReadsRemainDetached(t *testing.T) {
	plan := snapshotPlanFixture(64)
	snapshot := mustPlanSnapshot(t, NewAuthority(), plan, nil)
	foreign := mustPlanSnapshot(t, NewAuthority(), plan.Clone(), nil)
	const readers = 32
	const iterations = 20
	start := make(chan struct{})
	errorsChannel := make(chan error, readers)
	var wait sync.WaitGroup
	for reader := 0; reader < readers; reader++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if err := runSnapshotDetachedReads(snapshot, foreign, iterations); err != nil {
				errorsChannel <- err
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		require.NoError(t, err)
	}
	after, err := snapshot.LegacyCopy()
	require.NoError(t, err)
	assert.Equal(t, plan, after)
}

func runSnapshotDetachedReads(snapshot, foreign *Snapshot, iterations int) error {
	for range iterations {
		if err := snapshot.ValidateAuthentication(); err != nil {
			return err
		}
		equal, err := snapshot.ExactEqual(foreign)
		if err != nil {
			return err
		}
		if !equal {
			return fmt.Errorf("exact snapshots compared unequal")
		}
		copyPlan, err := snapshot.LegacyCopy()
		if err != nil {
			return err
		}
		poisonPlanFixture(copyPlan)
	}
	return nil
}

func snapshotPlanFixture(count int) *Plan {
	plan := &Plan{
		SchemaVersion: SchemaVersion,
		Sections:      make([]Section, count),
		Backends:      make(map[string]Backend, count),
		Profiles: map[string]Profile{
			"profile": {Name: "profile", BodyDigest: DigestString("mode http\n"), HasRules: true},
		},
		Maps:     make(map[string]Map, count),
		CRTLists: make(map[string]CRTList, count),
		Files:    make([]File, count+1),
	}
	config := "global\n"
	plan.Files[0] = File{
		Path: "haproxy.cfg", Kind: FileKindConfig, ReloadOnChange: true,
		Digest: DigestString(config), Size: int64(len(config)), Content: config, ContentKnown: true,
	}
	for index := range count {
		name := fmt.Sprintf("backend-%06d", index)
		text := fmt.Sprintf("backend %s\n    server server-%06d 10.0.%d.%d:8080\n",
			name, index, index/255, index%255)
		plan.Sections[index] = Section{
			Kind: SectionKindBackend, Name: name, TextDigest: DigestString(text),
			Length: len(text), Text: text, TextKnown: true,
		}
		weight := index + 1
		body := []string{fmt.Sprintf("server server-%06d 10.0.%d.%d:8080", index, index/255, index%255)}
		comments := []string{fmt.Sprintf("route %06d", index)}
		plan.Backends[name] = Backend{
			Name: name, Profile: "profile", Mode: "http", GUID: fmt.Sprintf("guid-%06d", index),
			Balance: "roundrobin", HashType: "consistent", Shape: ShapeDynamic,
			Servers: []Server{{
				Name: fmt.Sprintf("server-%06d", index), Address: fmt.Sprintf("10.0.%d.%d", index/255, index%255),
				Port: 8080, Weight: &weight, GUID: fmt.Sprintf("server-guid-%06d", index), Comment: "ready",
				Extra:    []KeywordArg{{Name: "check"}, {Name: "inter", Args: []string{"1s"}}},
				Template: 4,
			}},
			DefaultServer:  []KeywordArg{{Name: "init-addr", Args: []string{"last", "libc", "none"}}},
			BodyDigest:     DigestString(body[0]),
			CommentsDigest: DigestString(comments[0]),
			RecordDigest:   DigestString(fmt.Sprintf("record-%06d", index)),
			TextDigest:     DigestString(text), Body: body, Comments: comments, ContentKnown: true,
		}
		mapPath := fmt.Sprintf("maps/route-%06d.map", index)
		mapContent := fmt.Sprintf("host-%06d.example %s\n", index, name)
		plan.Maps[mapPath] = Map{
			Path: mapPath, Ordered: index%2 == 0,
			Entries: []Entry{{Key: fmt.Sprintf("host-%06d.example", index), Value: name}},
		}
		crtPath := fmt.Sprintf("crt-lists/frontend-%06d.list", index)
		plan.CRTLists[crtPath] = CRTList{
			Path: crtPath,
			Entries: []CRTListEntry{{
				Cert:       fmt.Sprintf("cert-%06d.pem", index),
				Options:    []KeywordArg{{Name: "alpn", Args: []string{"h2", "http/1.1"}}},
				SNIFilters: []string{fmt.Sprintf("host-%06d.example", index)},
			}},
		}
		plan.Files[index+1] = File{
			Path: mapPath, Kind: FileKindMap, Digest: DigestString(mapContent),
			Size: int64(len(mapContent)), Content: mapContent, ContentKnown: true,
		}
	}
	plan.ComputeID()
	return plan
}

func exactEmptyPlan() *Plan {
	plan := &Plan{SchemaVersion: SchemaVersion}
	plan.ComputeID()
	return plan
}

func poisonPlanFixture(plan *Plan) {
	plan.SchemaVersion++
	plan.ID = "poison"
	if len(plan.Sections) > 0 {
		plan.Sections[0].Name = "poison"
		plan.Sections[0].Text = "poison"
	}
	poisonPlanBackends(plan)
	poisonPlanProfiles(plan)
	poisonPlanMaps(plan)
	poisonPlanCRTLists(plan)
	if len(plan.Files) > 0 {
		plan.Files[0].Path = "poison"
		plan.Files[0].Content = "poison"
	}
}

func poisonPlanBackends(plan *Plan) {
	for name := range plan.Backends {
		backend := plan.Backends[name]
		backend.Name = "poison"
		if len(backend.Servers) > 0 {
			backend.Servers[0].Name = "poison"
			if backend.Servers[0].Weight != nil {
				*backend.Servers[0].Weight = -1
			}
			if len(backend.Servers[0].Extra) > 1 && len(backend.Servers[0].Extra[1].Args) > 0 {
				backend.Servers[0].Extra[1].Args[0] = "poison"
			}
		}
		if len(backend.DefaultServer) > 0 && len(backend.DefaultServer[0].Args) > 0 {
			backend.DefaultServer[0].Args[0] = "poison"
		}
		if len(backend.Body) > 0 {
			backend.Body[0] = "poison"
		}
		if len(backend.Comments) > 0 {
			backend.Comments[0] = "poison"
		}
		delete(plan.Backends, name)
		plan.Backends["poison"] = backend
		break
	}
}

func poisonPlanProfiles(plan *Plan) {
	for name := range plan.Profiles {
		delete(plan.Profiles, name)
		plan.Profiles["poison"] = Profile{Name: "poison"}
		break
	}
}

func poisonPlanMaps(plan *Plan) {
	for name := range plan.Maps {
		value := plan.Maps[name]
		if len(value.Entries) > 0 {
			value.Entries[0].Value = "poison"
		}
		delete(plan.Maps, name)
		plan.Maps["poison"] = value
		break
	}
}

func poisonPlanCRTLists(plan *Plan) {
	for name := range plan.CRTLists {
		value := plan.CRTLists[name]
		if len(value.Entries) > 0 {
			if len(value.Entries[0].Options) > 0 && len(value.Entries[0].Options[0].Args) > 0 {
				value.Entries[0].Options[0].Args[0] = "poison"
			}
			if len(value.Entries[0].SNIFilters) > 0 {
				value.Entries[0].SNIFilters[0] = "poison"
			}
		}
		delete(plan.CRTLists, name)
		plan.CRTLists["poison"] = value
		break
	}
}

func mustPlanSnapshot(tb testing.TB, authority *Authority, plan *Plan, previous *Snapshot) *Snapshot {
	tb.Helper()
	snapshot, err := NewSnapshot(authority, plan, previous)
	require.NoError(tb, err)
	return snapshot
}

func mustSnapshotEntry[T any](
	tb testing.TB,
	collection *snapshotCollection[T],
	key snapshotKey,
) *snapshotEntry[T] {
	tb.Helper()
	entry, err := findSnapshotEntry(collection.authority, collection.kind, collection, key)
	require.NoError(tb, err)
	return entry
}
