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
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/rendercontent"
)

func TestCanonicalStreamEqualsFullEncodingByteForByte(t *testing.T) {
	for _, test := range canonicalPlanCases() {
		t.Run(test.name, func(t *testing.T) {
			authority := NewAuthority()
			snapshot := mustPlanSnapshot(t, authority, test.plan, nil)

			var stream bytes.Buffer
			require.NoError(t, writeCanonicalPlan(snapshot.root, &stream))
			require.Equal(t, string(test.plan.Canonical()), stream.String())

			deferred := deferredSnapshotOf(authority, snapshot)
			id, err := deferred.ID()
			require.NoError(t, err)
			assert.Equal(t, Digest(test.plan.Canonical()), id)
			assert.Zero(t, authority.DigestFallbacks())
		})
	}
}

func TestCanonicalStreamEqualsFullEncodingForDocumentBackedFiles(t *testing.T) {
	for _, position := range []string{"first", "middle", "last"} {
		t.Run(position, func(t *testing.T) {
			shape := canonicalShapeFixture()
			shape.configPosition = position
			plan, document := canonicalDocumentFixture(t, &shape)
			authority := NewAuthority()
			snapshot, _, err := ReconcileSnapshotWithConfigDocument(authority, nil, plan, document)
			require.NoError(t, err)

			oracle, err := snapshot.canonicalCopyWithoutID()
			require.NoError(t, err)
			var stream bytes.Buffer
			require.NoError(t, writeCanonicalPlan(snapshot.root, &stream))
			require.Equal(t, string(oracle.Canonical()), stream.String())

			id, err := snapshot.ID()
			require.NoError(t, err)
			assert.Equal(t, Digest(oracle.Canonical()), id)
			assert.Zero(t, authority.DigestFallbacks())
		})
	}
}

// TestCanonicalStreamSurvivesEveryTransition is the stale-memo guard: after
// every add, update, delete and reorder the streamed digest must still equal
// the digest of the plan materialized from that same snapshot.
func TestCanonicalStreamSurvivesEveryTransition(t *testing.T) {
	authority := NewAuthority()
	shape := canonicalShapeFixture()
	plan, document := canonicalDocumentFixture(t, &shape)
	snapshot, _, err := ReconcileSnapshotWithConfigDocument(authority, nil, plan, document)
	require.NoError(t, err)
	assertCanonicalStreamAgrees(t, authority, snapshot, &shape)

	for _, step := range canonicalTransitionSteps() {
		t.Run(step.name, func(t *testing.T) {
			step.mutate(&shape)
			plan, document := canonicalDocumentFixture(t, &shape)
			next, _, err := ReconcileSnapshotWithConfigDocument(authority, snapshot, plan, document)
			require.NoError(t, err)
			snapshot = next
			assertCanonicalStreamAgrees(t, authority, snapshot, &shape)
		})
	}
	assert.Zero(t, authority.DigestFallbacks(),
		"a normal transition chain must never leave the incremental digest")
}

// assertCanonicalStreamAgrees pins the incremental digest against both the plan
// this snapshot materializes and a snapshot the same shape reaches cold, which
// is the state a restarted controller rebuilds into.
func assertCanonicalStreamAgrees(
	t *testing.T,
	authority *Authority,
	snapshot *Snapshot,
	shape *canonicalShape,
) {
	t.Helper()
	oracle, err := snapshot.canonicalCopyWithoutID()
	require.NoError(t, err)
	var stream bytes.Buffer
	require.NoError(t, writeCanonicalPlan(snapshot.root, &stream))
	require.Equal(t, string(oracle.Canonical()), stream.String())

	id, err := snapshot.ID()
	require.NoError(t, err)
	require.Equal(t, Digest(oracle.Canonical()), id)

	coldPlan, coldDocument := canonicalDocumentFixture(t, shape)
	cold, _, err := ReconcileSnapshotWithConfigDocument(NewAuthority(), nil, coldPlan, coldDocument)
	require.NoError(t, err)
	coldID, err := cold.ID()
	require.NoError(t, err)
	assert.Equal(t, coldID, id, "cold rebuild and incremental chain must agree on the plan ID")
	assert.Zero(t, authority.DigestFallbacks())
}

func TestCanonicalStreamFallsBackWhenOrderIsUnproven(t *testing.T) {
	authority := NewAuthority()
	source := snapshotPlanFixture(2)
	base := mustPlanSnapshot(t, authority, source, nil)

	unsorted, err := buildSnapshotCollection(
		authority, backendSnapshotCollection, true,
		[]*snapshotEntry[Backend]{
			sealSnapshotEntry(authority, backendSnapshotCollection,
				snapshotKey{index: -1, name: "backend-000001"}, source.Backends["backend-000001"]),
			sealSnapshotEntry(authority, backendSnapshotCollection,
				snapshotKey{index: -1, name: "backend-000000"}, source.Backends["backend-000000"]),
		}, nil,
	)
	require.NoError(t, err)
	snapshot := sealSnapshot(authority, sealDeferredPlanRoot(
		authority, base.root.schema, base.root.sections, unsorted, base.root.profiles,
		base.root.maps, base.root.crtLists, base.root.files,
	))

	var stream bytes.Buffer
	require.ErrorIs(t, writeCanonicalPlan(snapshot.root, &stream), errCanonicalOrderUnproven)
	id, err := snapshot.ID()
	require.NoError(t, err)
	assert.Equal(t, Digest(source.Canonical()), id, "the fallback must still be exact")
	assert.Equal(t, uint64(1), authority.DigestFallbacks())
	requireSnapshotJSONMatchesLegacy(t, snapshot)
}

func TestCanonicalStreamRefusesCorruptKeys(t *testing.T) {
	authority := NewAuthority()
	source := snapshotPlanFixture(1)
	base := mustPlanSnapshot(t, authority, source, nil)

	corrupt, err := buildSnapshotCollection(
		authority, backendSnapshotCollection, true,
		[]*snapshotEntry[Backend]{
			sealSnapshotEntry(authority, backendSnapshotCollection,
				snapshotKey{index: 0, name: "backend-000000"}, source.Backends["backend-000000"]),
		}, nil,
	)
	require.NoError(t, err)
	snapshot := sealSnapshot(authority, sealDeferredPlanRoot(
		authority, base.root.schema, base.root.sections, corrupt, base.root.profiles,
		base.root.maps, base.root.crtLists, base.root.files,
	))

	_, err = snapshot.ID()
	require.ErrorIs(t, err, errInvalidSnapshot,
		"a corrupt snapshot must fail the digest, not be hashed the slow way")
}

func TestCanonicalStreamMemoIsAuthenticated(t *testing.T) {
	authority := NewAuthority()
	snapshot := mustPlanSnapshot(t, authority, snapshotPlanFixture(1), nil)
	entry := mustSnapshotEntry(
		t, snapshot.root.backends, snapshotKey{index: -1, name: "backend-000000"},
	)
	require.NotNil(t, entry.canonical)
	entry.canonical = &canonicalFragment{}
	require.ErrorIs(t, snapshot.ValidateAuthentication(), errInvalidSnapshot)
}

// TestPlanCanonicalGoldenID pins the encoding itself. T1-T3 only prove the two
// implementations agree; this catches an encoding/json change that would move
// both together and reload every pod in the fleet.
func TestPlanCanonicalGoldenID(t *testing.T) {
	const goldenID = "b81823d8197e8e2e"
	plan := canonicalGoldenPlan()
	require.Equal(t, goldenID, Digest(plan.Canonical()))

	authority := NewAuthority()
	snapshot := mustPlanSnapshot(t, authority, plan, nil)
	id, err := deferredSnapshotOf(authority, snapshot).ID()
	require.NoError(t, err)
	assert.Equal(t, goldenID, id)
}

// TestPlanCanonicalFieldInventory fails at the point a field is added, because
// writeCanonicalPlan hand-writes the Plan skeleton: a field the skeleton does
// not know would split the cold and warm IDs for the same plan.
func TestPlanCanonicalFieldInventory(t *testing.T) {
	inventory := map[string][]string{
		"Plan": {
			"schemaVersion", "id", "sections", "backends", "profiles",
			"maps", "crtLists,omitempty", "files",
		},
		"Section":      {"kind", "name", "textDigest", "length", "-", "-"},
		"Backend":      {"name", "profile,omitempty", "mode,omitempty", "guid,omitempty", "balance,omitempty", "hashType,omitempty", "shape", "shapeReason,omitempty", "servers,omitempty", "defaultServer,omitempty", "bodyDigest", "commentsDigest", "recordDigest", "textDigest", "-", "-", "-"},
		"Server":       {"name", "address", "port", "weight,omitempty", "disabled,omitempty", "guid,omitempty", "comment,omitempty", "extra,omitempty"},
		"KeywordArg":   {"name", "args,omitempty"},
		"Profile":      {"name", "bodyDigest", "hasRules,omitempty"},
		"Map":          {"path", "ordered", "entries,omitempty"},
		"Entry":        {"key", "value"},
		"CRTList":      {"path", "entries,omitempty"},
		"CRTListEntry": {"cert", "options,omitempty", "sniFilters,omitempty"},
		"File":         {"path", "kind", "reloadOnChange", "digest", "size", "seal,omitempty", "-", "-"},
	}
	types := []reflect.Type{
		reflect.TypeOf(Plan{}), reflect.TypeOf(Section{}), reflect.TypeOf(Backend{}),
		reflect.TypeOf(Server{}), reflect.TypeOf(KeywordArg{}), reflect.TypeOf(Profile{}),
		reflect.TypeOf(Map{}), reflect.TypeOf(Entry{}), reflect.TypeOf(CRTList{}),
		reflect.TypeOf(CRTListEntry{}), reflect.TypeOf(File{}),
	}
	for _, structType := range types {
		tags := make([]string, structType.NumField())
		for index := range tags {
			tags[index] = structType.Field(index).Tag.Get("json")
		}
		assert.Equal(t, inventory[structType.Name()], tags, structType.Name())
	}
}

type canonicalPlanCase struct {
	name string
	plan *Plan
}

func canonicalPlanCases() []canonicalPlanCase {
	cases := make([]canonicalPlanCase, 0, 16)
	cases = append(cases,
		canonicalPlanCase{name: "nil collections", plan: &Plan{SchemaVersion: SchemaVersion}},
		canonicalPlanCase{name: "empty collections", plan: &Plan{
			SchemaVersion: SchemaVersion, Sections: []Section{}, Backends: map[string]Backend{},
			Profiles: map[string]Profile{}, Maps: map[string]Map{},
			CRTLists: map[string]CRTList{}, Files: []File{},
		}},
	)
	for _, count := range []int{0, 1, 2, 3, 7} {
		cases = append(cases, canonicalPlanCase{
			name: fmt.Sprintf("populated %d", count), plan: snapshotPlanFixture(count),
		})
	}
	for _, variant := range []struct {
		name  string
		value map[string]CRTList
	}{
		{name: "nil", value: nil},
		{name: "empty", value: map[string]CRTList{}},
		{name: "entries nil", value: map[string]CRTList{"a": {Path: "a"}}},
		{name: "entries empty", value: map[string]CRTList{"a": {Path: "a", Entries: []CRTListEntry{}}}},
	} {
		plan := snapshotPlanFixture(2)
		plan.CRTLists = variant.value
		cases = append(cases, canonicalPlanCase{name: "crtLists " + variant.name, plan: plan})
	}
	zeroWeight, positiveWeight := 0, 3
	for _, variant := range []struct {
		name  string
		value *int
	}{
		{name: "nil"}, {name: "zero", value: &zeroWeight}, {name: "positive", value: &positiveWeight},
	} {
		plan := snapshotPlanFixture(1)
		backend := plan.Backends["backend-000000"]
		backend.Servers[0].Weight = variant.value
		plan.Backends[backend.Name] = backend
		cases = append(cases, canonicalPlanCase{name: "weight " + variant.name, plan: plan})
	}
	cases = append(cases,
		canonicalPlanCase{name: "all optional fields unset", plan: canonicalSparsePlan()},
		canonicalPlanCase{name: "hostile bytes", plan: canonicalHostilePlan()},
		canonicalPlanCase{name: "byte ordered keys", plan: canonicalKeyOrderPlan()},
		canonicalPlanCase{name: "sealed file", plan: canonicalSealedPlan("hunter2")},
	)
	return cases
}

func canonicalSealedPlan(content string) *Plan {
	plan := snapshotPlanFixture(1)
	plan.Files = append(plan.Files, File{
		Path: "secrets/x", Kind: FileKindGeneral, Digest: DigestString(content), Size: int64(len(content)),
		Seal: "seal", Content: content, ContentKnown: true,
	})
	return plan
}

// TestSealedFileDigestStaysOutOfTheIdentity pins that the plan ID, which
// readers without Secret access see, never depends on a sealed file's digest,
// while the JSON the agent receives still carries it.
func TestSealedFileDigestStaysOutOfTheIdentity(t *testing.T) {
	plan := canonicalSealedPlan("hunter2")
	digest := DigestString("hunter2")
	assert.NotContains(t, string(plan.Canonical()), digest)
	other := canonicalSealedPlan("hunter3")
	assert.Equal(t, Digest(plan.Canonical()), Digest(other.Canonical()), "one seal and size, one identity")
	other.Files[len(other.Files)-1].Seal = "other seal"
	assert.NotEqual(t, Digest(plan.Canonical()), Digest(other.Canonical()))

	plan.ComputeID()
	snapshot := mustPlanSnapshot(t, NewAuthority(), plan, nil)
	encoded, err := snapshot.MarshalJSON()
	require.NoError(t, err)
	assert.Contains(t, string(encoded), digest)
	assert.Equal(t, digest, plan.Files[len(plan.Files)-1].Digest, "Canonical leaves the plan untouched")
}

// canonicalSparsePlan leaves every omitempty leaf field at its zero value and
// every optional slice nil, so the omitted-field boundary is exercised.
func canonicalSparsePlan() *Plan {
	return &Plan{
		SchemaVersion: SchemaVersion,
		Sections:      []Section{{Kind: SectionKindCore, Name: "", TextKnown: true}},
		Backends:      map[string]Backend{"": {Name: "", Shape: ShapeDynamic, ContentKnown: true}},
		Profiles:      map[string]Profile{"": {Name: ""}},
		Maps:          map[string]Map{"": {Path: ""}},
		CRTLists:      map[string]CRTList{"": {Path: ""}},
		Files:         []File{{Path: "", Kind: FileKindGeneral, ContentKnown: true}},
	}
}

func canonicalHostilePlan() *Plan {
	hostile := "\"\\<>&\n\t\x00\x7f\u2028\u2029\U0001F600\xff"
	plan := &Plan{
		SchemaVersion: SchemaVersion,
		Sections: []Section{{
			Kind: SectionKindCore, Name: hostile, TextDigest: hostile, TextKnown: true,
		}},
		Backends: map[string]Backend{hostile: {
			Name: hostile, Profile: hostile, Mode: hostile, GUID: hostile,
			Balance: hostile, HashType: hostile, Shape: hostile, ShapeReason: hostile,
			Servers: []Server{{
				Name: hostile, Address: hostile, Port: 1, Disabled: true, GUID: hostile,
				Comment: hostile, Extra: []KeywordArg{{Name: hostile, Args: []string{hostile}}},
			}},
			DefaultServer:  []KeywordArg{{Name: hostile, Args: []string{hostile}}},
			BodyDigest:     hostile,
			CommentsDigest: hostile,
			RecordDigest:   hostile,
			TextDigest:     hostile,
			ContentKnown:   true,
		}},
		Profiles: map[string]Profile{hostile: {Name: hostile, BodyDigest: hostile, HasRules: true}},
		Maps: map[string]Map{hostile: {
			Path: hostile, Ordered: true, Entries: []Entry{{Key: hostile, Value: hostile}},
		}},
		CRTLists: map[string]CRTList{hostile: {
			Path: hostile,
			Entries: []CRTListEntry{{
				Cert:       hostile,
				Options:    []KeywordArg{{Name: hostile, Args: []string{hostile}}},
				SNIFilters: []string{hostile},
			}},
		}},
		Files: []File{{
			Path: hostile, Kind: FileKindGeneral, ReloadOnChange: true,
			Digest: hostile, ContentKnown: true,
		}},
	}
	return plan
}

// canonicalKeyOrderPlan uses key pairs whose byte order differs from any
// rune-, case- or locale-aware order, which is the order encoding/json sorts.
func canonicalKeyOrderPlan() *Plan {
	keys := []string{"Z", "a", "a\x00", "ab", "a\xff", "", "\u00e9", "\U0001F600"}
	plan := &Plan{
		SchemaVersion: SchemaVersion,
		Sections:      []Section{},
		Backends:      map[string]Backend{},
		Profiles:      map[string]Profile{},
		Maps:          map[string]Map{},
		CRTLists:      map[string]CRTList{},
		Files:         []File{},
	}
	for _, key := range keys {
		plan.Backends[key] = Backend{Name: key, Shape: ShapeDynamic, ContentKnown: true}
		plan.Profiles[key] = Profile{Name: key}
		plan.Maps[key] = Map{Path: key}
		plan.CRTLists[key] = CRTList{Path: key}
	}
	return plan
}

func canonicalGoldenPlan() *Plan {
	weight := 5
	return &Plan{
		SchemaVersion: SchemaVersion,
		Sections: []Section{
			{Kind: SectionKindCore, Name: "global", TextDigest: "aaaa000000000001", Length: 7, TextKnown: true},
			{Kind: SectionKindBackend, Name: "api", TextDigest: "aaaa000000000002", Length: 21, TextKnown: true},
		},
		Backends: map[string]Backend{"api": {
			Name: "api", Mode: "http", Shape: ShapeDynamic, ContentKnown: true,
			Servers: []Server{{
				Name: "s1", Address: "10.0.0.1", Port: 8080, Weight: &weight,
				Extra: []KeywordArg{{Name: "check"}},
			}},
			BodyDigest: "bbbb000000000001", CommentsDigest: "bbbb000000000002",
			RecordDigest: "bbbb000000000003", TextDigest: "aaaa000000000002",
		}},
		Profiles: map[string]Profile{"defaults": {Name: "defaults", BodyDigest: "cccc000000000001"}},
		Maps: map[string]Map{"maps/host.map": {
			Path: "maps/host.map", Entries: []Entry{{Key: "example.com", Value: "api"}},
		}},
		Files: []File{{
			Path: ConfigFilePath, Kind: FileKindConfig, ReloadOnChange: true,
			Digest: "dddd000000000001", Size: 28, ContentKnown: true,
		}},
	}
}

func deferredSnapshotOf(authority *Authority, snapshot *Snapshot) *Snapshot {
	root := sealDeferredPlanRoot(
		authority, snapshot.root.schema, snapshot.root.sections, snapshot.root.backends,
		snapshot.root.profiles, snapshot.root.maps, snapshot.root.crtLists, snapshot.root.files,
	)
	return sealSnapshot(authority, root)
}

type canonicalShape struct {
	sections       []string
	backends       map[string][]string
	profiles       []string
	mapEntries     map[string][]Entry
	crtLists       map[string][]string
	configPosition string
}

func canonicalShapeFixture() canonicalShape {
	return canonicalShape{
		sections: []string{"global\n", "backend api\n", "backend web\n"},
		backends: map[string][]string{"api": {"s1", "s2"}, "web": {"s1"}},
		profiles: []string{"defaults"},
		mapEntries: map[string][]Entry{
			"maps/host.map": {{Key: "a.example", Value: "api"}, {Key: "b.example", Value: "web"}},
			"maps/path.map": {{Key: "/x", Value: "api"}},
		},
		crtLists:       map[string][]string{"crt/front.list": {"a.example"}},
		configPosition: "first",
	}
}

func canonicalTransitionSteps() []struct {
	name   string
	mutate func(*canonicalShape)
} {
	return []struct {
		name   string
		mutate func(*canonicalShape)
	}{
		{name: "replace section text", mutate: func(s *canonicalShape) {
			s.sections[1] = "backend API\n"
		}},
		{name: "insert section", mutate: func(s *canonicalShape) {
			s.sections = append(s.sections, "backend new\n")
		}},
		{name: "delete section", mutate: func(s *canonicalShape) {
			s.sections = s.sections[:len(s.sections)-1]
		}},
		{name: "add server", mutate: func(s *canonicalShape) {
			s.backends["web"] = append(s.backends["web"], "s2")
		}},
		{name: "remove server", mutate: func(s *canonicalShape) {
			s.backends["api"] = s.backends["api"][:1]
		}},
		{name: "add backend", mutate: func(s *canonicalShape) {
			s.backends["aaa"] = []string{"s1"}
		}},
		{name: "delete backend", mutate: func(s *canonicalShape) {
			delete(s.backends, "web")
		}},
		{name: "swap map values", mutate: func(s *canonicalShape) {
			host, path := s.mapEntries["maps/host.map"], s.mapEntries["maps/path.map"]
			s.mapEntries["maps/host.map"], s.mapEntries["maps/path.map"] = path, host
		}},
		{name: "rename map key", mutate: func(s *canonicalShape) {
			s.mapEntries["maps/renamed.map"] = s.mapEntries["maps/host.map"]
			delete(s.mapEntries, "maps/host.map")
		}},
		{name: "same length map value", mutate: func(s *canonicalShape) {
			s.mapEntries["maps/path.map"] = []Entry{{Key: "/x", Value: "AAA"}}
		}},
		{name: "empty crt lists", mutate: func(s *canonicalShape) {
			s.crtLists = map[string][]string{}
		}},
		{name: "restore crt lists", mutate: func(s *canonicalShape) {
			s.crtLists = map[string][]string{"crt/front.list": {"z.example"}}
		}},
		{name: "add profile", mutate: func(s *canonicalShape) {
			s.profiles = append(s.profiles, "strict")
		}},
		{name: "delete profile", mutate: func(s *canonicalShape) {
			s.profiles = s.profiles[:1]
		}},
	}
}

func canonicalDocumentFixture(
	tb testing.TB,
	shape *canonicalShape,
) (*Plan, rendercontent.Document) {
	tb.Helper()
	plan := &Plan{
		SchemaVersion: SchemaVersion,
		Sections:      make([]Section, 0, len(shape.sections)),
		Backends:      map[string]Backend{},
		Profiles:      map[string]Profile{},
		Maps:          map[string]Map{},
		CRTLists:      map[string]CRTList{},
	}
	var builder rendercontent.DocumentBuilder
	total := 0
	for _, text := range shape.sections {
		plan.Sections = append(plan.Sections, Section{
			Kind: SectionKindCore, Name: "core", Text: text, TextKnown: true,
			TextDigest: DigestString(text), Length: len(text),
		})
		var part rendercontent.DocumentBuilder
		_, err := part.WriteString(text)
		require.NoError(tb, err)
		fragment, err := part.Build(nil)
		require.NoError(tb, err)
		require.NoError(tb, builder.AppendDocument(fragment))
		total += len(text)
	}
	document, err := builder.Build(nil)
	require.NoError(tb, err)

	for name, servers := range shape.backends {
		plan.Backends[name] = canonicalBackendFixture(name, servers)
	}
	for _, name := range shape.profiles {
		plan.Profiles[name] = Profile{Name: name, BodyDigest: DigestString(name), HasRules: true}
	}
	for path, filters := range shape.crtLists {
		plan.CRTLists[path] = CRTList{
			Path: path,
			Entries: []CRTListEntry{{
				Cert: path + ".pem", Options: []KeywordArg{{Name: "alpn", Args: []string{"h2"}}},
				SNIFilters: slices.Clone(filters),
			}},
		}
	}
	plan.Files = canonicalFileFixtures(plan, shape, total)
	return plan, document
}

func canonicalBackendFixture(name string, servers []string) Backend {
	backend := Backend{
		Name: name, Profile: "defaults", Mode: "http", Shape: ShapeDynamic,
		BodyDigest: DigestString(name), CommentsDigest: DigestString(name),
		RecordDigest: DigestString(name), TextDigest: DigestString(name),
		ContentKnown: true,
	}
	for index, server := range servers {
		weight := index
		backend.Servers = append(backend.Servers, Server{
			Name: server, Address: fmt.Sprintf("10.0.0.%d", index+1), Port: 8080,
			Weight: &weight, Extra: []KeywordArg{{Name: "check"}},
		})
	}
	return backend
}

func canonicalFileFixtures(plan *Plan, shape *canonicalShape, configSize int) []File {
	config := File{
		Path: ConfigFilePath, Kind: FileKindConfig,
		ReloadOnChange: true, Size: int64(configSize),
	}
	auxiliary := make([]File, 0, len(shape.mapEntries))
	paths := make([]string, 0, len(shape.mapEntries))
	for path := range shape.mapEntries {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		entries := shape.mapEntries[path]
		var content strings.Builder
		for _, entry := range entries {
			content.WriteString(entry.Key + " " + entry.Value + "\n")
		}
		plan.Maps[path] = Map{Path: path, Ordered: true, Entries: slices.Clone(entries)}
		auxiliary = append(auxiliary, File{
			Path: path, Kind: FileKindMap, Digest: DigestString(content.String()),
			Size: int64(content.Len()), Content: content.String(), ContentKnown: true,
		})
	}
	switch shape.configPosition {
	case "first":
		return append([]File{config}, auxiliary...)
	case "last":
		return append(auxiliary, config)
	default:
		middle := len(auxiliary) / 2
		files := make([]File, 0, len(auxiliary)+1)
		files = append(files, auxiliary[:middle]...)
		files = append(files, config)
		return append(files, auxiliary[middle:]...)
	}
}
