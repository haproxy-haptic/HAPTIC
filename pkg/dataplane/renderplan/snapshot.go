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
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cespare/xxhash/v2"
)

var (
	errInvalidSnapshotAuthority = errors.New("render plan snapshot authority is invalid")
	errInvalidSnapshot          = errors.New("render plan snapshot is invalid")
	errForeignSnapshot          = errors.New("render plan snapshot has a foreign authority")
	errNilSnapshotPlan          = errors.New("render plan snapshot source is nil")
	errInexactSnapshotPlan      = errors.New("render plan snapshot source has no exact local content")
	errSnapshotTreeTooDeep      = errors.New("render plan snapshot tree is too deep")
)

// Authority owns one lineage of render-plan snapshots. digestFallbacks is a
// pointer so a shallow copy of an authority stays a copyable value, which
// authentication still rejects.
type Authority struct {
	seal            *Authority
	digestFallbacks *atomic.Uint64
}

// NewAuthority creates an isolated snapshot lineage.
func NewAuthority() *Authority {
	authority := &Authority{digestFallbacks: &atomic.Uint64{}}
	authority.seal = authority
	return authority
}

// DigestFallbacks counts the plan digests this lineage computed by rebuilding
// the whole plan because the streaming canonical writer could not prove its
// order. It stays zero while the incremental digest engages.
func (a *Authority) DigestFallbacks() uint64 {
	if a == nil || a.digestFallbacks == nil {
		return 0
	}
	return a.digestFallbacks.Load()
}

// ValidateAuthentication verifies the authority's exact identity.
func (a *Authority) ValidateAuthentication() error {
	if a == nil || a.seal != a || a.digestFallbacks == nil {
		return errInvalidSnapshotAuthority
	}
	return nil
}

// ValidateSnapshot proves that snapshot belongs to this authority.
func (a *Authority) ValidateSnapshot(snapshot *Snapshot) error {
	if err := a.ValidateAuthentication(); err != nil {
		return err
	}
	if err := snapshot.ValidateAuthentication(); err != nil {
		return err
	}
	if snapshot.authority != a {
		return errForeignSnapshot
	}
	return nil
}

type snapshotCollectionKind uint8

const (
	sectionSnapshotCollection snapshotCollectionKind = iota + 1
	backendSnapshotCollection
	profileSnapshotCollection
	mapSnapshotCollection
	crtListSnapshotCollection
	fileSnapshotCollection
)

type snapshotKey struct {
	index int
	name  string
}

func compareSnapshotKeys(left, right snapshotKey) int {
	if left.index < right.index {
		return -1
	}
	if left.index > right.index {
		return 1
	}
	return strings.Compare(left.name, right.name)
}

type snapshotEntryAuthentication[T any] struct {
	owner        *snapshotEntry[T]
	authority    *Authority
	kind         snapshotCollectionKind
	key          snapshotKey
	value        *snapshotValue[T]
	deferredFile *snapshotDeferredFile
	canonical    *canonicalFragment
}

type snapshotValue[T any] struct {
	value T
	seal  *snapshotValue[T]
}

type snapshotEntry[T any] struct {
	authority    *Authority
	kind         snapshotCollectionKind
	key          snapshotKey
	value        *snapshotValue[T]
	deferredFile *snapshotDeferredFile
	canonical    *canonicalFragment
	// predecessor is the entry this one replaced under the same key, kept
	// until the canonical fragment is derived from it and then dropped.
	predecessor *snapshotEntry[T]
	seal        *snapshotEntry[T]
	auth        snapshotEntryAuthentication[T]
}

func sealSnapshotEntry[T any](
	authority *Authority,
	kind snapshotCollectionKind,
	key snapshotKey,
	value T,
) *snapshotEntry[T] {
	owned := &snapshotValue[T]{value: value}
	owned.seal = owned
	canonical := &canonicalFragment{}
	entry := &snapshotEntry[T]{
		authority: authority, kind: kind, key: key, value: owned, canonical: canonical,
	}
	entry.seal = entry
	entry.auth = snapshotEntryAuthentication[T]{
		owner: entry, authority: authority, kind: kind, key: key, value: owned,
		canonical: canonical,
	}
	return entry
}

func (e *snapshotEntry[T]) validate(authority *Authority, kind snapshotCollectionKind) error {
	if e == nil || e.seal != e || e.auth.owner != e || e.authority != authority ||
		e.auth.authority != e.authority || e.kind != kind || e.auth.kind != e.kind ||
		e.auth.key != e.key || e.value == nil || e.value.seal != e.value ||
		e.auth.value != e.value || e.auth.deferredFile != e.deferredFile ||
		e.canonical == nil || e.auth.canonical != e.canonical {
		return errInvalidSnapshot
	}
	if e.deferredFile != nil {
		file, ok := any(e.value.value).(File)
		if e.kind != fileSnapshotCollection || !ok || file.ContentKnown ||
			file != snapshotDeferredFileStub(e.deferredFile) {
			return errInvalidSnapshot
		}
		return e.deferredFile.validate(authority)
	}
	return nil
}

type snapshotNodeAuthentication[T any] struct {
	owner     *snapshotNode[T]
	authority *Authority
	kind      snapshotCollectionKind
	entry     *snapshotEntry[T]
	left      *snapshotNode[T]
	right     *snapshotNode[T]
	height    int
	entries   int
}

type snapshotNode[T any] struct {
	authority *Authority
	kind      snapshotCollectionKind
	entry     *snapshotEntry[T]
	left      *snapshotNode[T]
	right     *snapshotNode[T]
	height    int
	entries   int
	seal      *snapshotNode[T]
	auth      snapshotNodeAuthentication[T]
}

func newSnapshotNode[T any](
	authority *Authority,
	kind snapshotCollectionKind,
	entry *snapshotEntry[T],
	left, right *snapshotNode[T],
) *snapshotNode[T] {
	node := &snapshotNode[T]{
		authority: authority,
		kind:      kind,
		entry:     entry,
		left:      left,
		right:     right,
		height:    max(snapshotNodeHeight(left), snapshotNodeHeight(right)) + 1,
		entries:   snapshotNodeCount(left) + snapshotNodeCount(right) + 1,
	}
	node.seal = node
	node.auth = snapshotNodeAuthentication[T]{
		owner: node, authority: authority, kind: kind, entry: entry,
		left: left, right: right, height: node.height, entries: node.entries,
	}
	return node
}

func (n *snapshotNode[T]) validate(authority *Authority, kind snapshotCollectionKind) error {
	if n == nil || n.seal != n || n.auth.owner != n || n.authority != authority ||
		n.auth.authority != n.authority || n.kind != kind || n.auth.kind != n.kind ||
		n.entry == nil || n.auth.entry != n.entry || n.auth.left != n.left ||
		n.auth.right != n.right || n.auth.height != n.height ||
		n.auth.entries != n.entries || n.height < 1 || n.entries < 1 {
		return errInvalidSnapshot
	}
	return n.entry.validate(authority, kind)
}

func snapshotNodeHeight[T any](node *snapshotNode[T]) int {
	if node == nil {
		return 0
	}
	return node.height
}

func snapshotNodeCount[T any](node *snapshotNode[T]) int {
	if node == nil {
		return 0
	}
	return node.entries
}

type snapshotCollectionAuthentication[T any] struct {
	owner     *snapshotCollection[T]
	authority *Authority
	kind      snapshotCollectionKind
	present   bool
	root      *snapshotNode[T]
	entries   int
}

type snapshotCollection[T any] struct {
	authority *Authority
	kind      snapshotCollectionKind
	present   bool
	root      *snapshotNode[T]
	entries   int
	seal      *snapshotCollection[T]
	auth      snapshotCollectionAuthentication[T]
}

func sealSnapshotCollection[T any](
	authority *Authority,
	kind snapshotCollectionKind,
	present bool,
	root *snapshotNode[T],
) *snapshotCollection[T] {
	collection := &snapshotCollection[T]{
		authority: authority,
		kind:      kind,
		present:   present,
		root:      root,
		entries:   snapshotNodeCount(root),
	}
	collection.seal = collection
	collection.auth = snapshotCollectionAuthentication[T]{
		owner: collection, authority: authority, kind: kind, present: present,
		root: root, entries: collection.entries,
	}
	return collection
}

func (c *snapshotCollection[T]) validate(authority *Authority, kind snapshotCollectionKind) error {
	if c == nil || c.seal != c || c.auth.owner != c || c.authority != authority ||
		c.auth.authority != c.authority || c.kind != kind || c.auth.kind != c.kind ||
		c.auth.present != c.present || c.auth.root != c.root ||
		c.auth.entries != c.entries || c.entries < 0 {
		return errInvalidSnapshot
	}
	if c.root == nil {
		if c.entries != 0 {
			return errInvalidSnapshot
		}
		return nil
	}
	if err := c.root.validate(authority, kind); err != nil {
		return err
	}
	if c.root.entries != c.entries {
		return errInvalidSnapshot
	}
	return nil
}

type planRootAuthentication struct {
	owner      *planRoot
	authority  *Authority
	schema     int
	id         string
	deferredID bool
	idMemo     *planIDMemo
	sections   *snapshotCollection[Section]
	backends   *snapshotCollection[Backend]
	profiles   *snapshotCollection[Profile]
	maps       *snapshotCollection[Map]
	crtLists   *snapshotCollection[CRTList]
	files      *snapshotCollection[File]
	entries    int
}

type planRoot struct {
	authority  *Authority
	schema     int
	id         string
	deferredID bool
	idMemo     *planIDMemo
	sections   *snapshotCollection[Section]
	backends   *snapshotCollection[Backend]
	profiles   *snapshotCollection[Profile]
	maps       *snapshotCollection[Map]
	crtLists   *snapshotCollection[CRTList]
	files      *snapshotCollection[File]
	entries    int
	seal       *planRoot
	auth       planRootAuthentication
}

type planIDMemo struct {
	once sync.Once
	id   string
	err  error
}

func sealPlanRoot(
	authority *Authority,
	schema int,
	id string,
	sections *snapshotCollection[Section],
	backends *snapshotCollection[Backend],
	profiles *snapshotCollection[Profile],
	mapsCollection *snapshotCollection[Map],
	crtLists *snapshotCollection[CRTList],
	files *snapshotCollection[File],
) *planRoot {
	return sealPlanRootState(
		authority, schema, id, false, nil, sections, backends, profiles,
		mapsCollection, crtLists, files,
	)
}

func sealDeferredPlanRoot(
	authority *Authority,
	schema int,
	sections *snapshotCollection[Section],
	backends *snapshotCollection[Backend],
	profiles *snapshotCollection[Profile],
	mapsCollection *snapshotCollection[Map],
	crtLists *snapshotCollection[CRTList],
	files *snapshotCollection[File],
) *planRoot {
	return sealPlanRootState(
		authority, schema, "", true, &planIDMemo{}, sections, backends, profiles,
		mapsCollection, crtLists, files,
	)
}

func sealPlanRootState(
	authority *Authority,
	schema int,
	id string,
	deferredID bool,
	idMemo *planIDMemo,
	sections *snapshotCollection[Section],
	backends *snapshotCollection[Backend],
	profiles *snapshotCollection[Profile],
	mapsCollection *snapshotCollection[Map],
	crtLists *snapshotCollection[CRTList],
	files *snapshotCollection[File],
) *planRoot {
	root := &planRoot{
		authority:  authority,
		schema:     schema,
		id:         id,
		deferredID: deferredID,
		idMemo:     idMemo,
		sections:   sections,
		backends:   backends,
		profiles:   profiles,
		maps:       mapsCollection,
		crtLists:   crtLists,
		files:      files,
	}
	root.entries = sections.entries + backends.entries + profiles.entries +
		mapsCollection.entries + crtLists.entries + files.entries
	root.seal = root
	root.auth = planRootAuthentication{
		owner: root, authority: authority, schema: root.schema, id: root.id,
		deferredID: root.deferredID, idMemo: root.idMemo,
		sections: sections, backends: backends, profiles: profiles,
		maps: mapsCollection, crtLists: crtLists, files: files, entries: root.entries,
	}
	return root
}

func (r *planRoot) validate(authority *Authority) error {
	if r == nil || r.seal != r || r.authority != authority || r.entries < 0 {
		return errInvalidSnapshot
	}
	if !r.collectionsPresent() {
		return errInvalidSnapshot
	}
	expected := planRootAuthentication{
		owner: r, authority: r.authority, schema: r.schema, id: r.id,
		deferredID: r.deferredID, idMemo: r.idMemo,
		sections: r.sections, backends: r.backends, profiles: r.profiles,
		maps: r.maps, crtLists: r.crtLists, files: r.files, entries: r.entries,
	}
	if r.auth != expected {
		return errInvalidSnapshot
	}
	if r.deferredID {
		if r.id != "" || r.idMemo == nil {
			return errInvalidSnapshot
		}
	} else if r.idMemo != nil {
		return errInvalidSnapshot
	}
	if err := r.sections.validate(authority, sectionSnapshotCollection); err != nil {
		return err
	}
	if err := r.backends.validate(authority, backendSnapshotCollection); err != nil {
		return err
	}
	if err := r.profiles.validate(authority, profileSnapshotCollection); err != nil {
		return err
	}
	if err := r.maps.validate(authority, mapSnapshotCollection); err != nil {
		return err
	}
	if err := r.crtLists.validate(authority, crtListSnapshotCollection); err != nil {
		return err
	}
	if err := r.files.validate(authority, fileSnapshotCollection); err != nil {
		return err
	}
	entries := r.sections.entries + r.backends.entries + r.profiles.entries +
		r.maps.entries + r.crtLists.entries + r.files.entries
	if entries != r.entries {
		return errInvalidSnapshot
	}
	return nil
}

func (r *planRoot) collectionsPresent() bool {
	return r.sections != nil && r.backends != nil && r.profiles != nil &&
		r.maps != nil && r.crtLists != nil && r.files != nil
}

type snapshotAuthentication struct {
	owner     *Snapshot
	authority *Authority
	root      *planRoot
	entries   int
	source    any
	shared    *sharedPlanMemo
}

// sharedPlanMemo holds the read-only Plan a snapshot hands out, built once.
type sharedPlanMemo struct {
	once sync.Once
	plan *Plan
	err  error
}

// Snapshot is an authenticated immutable final render plan.
type Snapshot struct {
	authority *Authority
	root      *planRoot
	entries   int
	// source is the BackendSource token the backends were reconciled from,
	// nil when they came from a map.
	source any
	shared *sharedPlanMemo
	seal   *Snapshot
	auth   snapshotAuthentication
}

func sealSnapshot(authority *Authority, root *planRoot) *Snapshot {
	return sealSnapshotFromSource(authority, root, nil)
}

func sealSnapshotFromSource(authority *Authority, root *planRoot, source any) *Snapshot {
	snapshot := &Snapshot{
		authority: authority, root: root, entries: root.entries, source: source, shared: &sharedPlanMemo{},
	}
	snapshot.seal = snapshot
	snapshot.auth = snapshotAuthentication{
		owner: snapshot, authority: authority, root: root, entries: snapshot.entries,
		source: source, shared: snapshot.shared,
	}
	return snapshot
}

// NewSnapshot deeply owns plan and reuses exact roots from previous.
func NewSnapshot(authority *Authority, plan *Plan, previous *Snapshot) (*Snapshot, error) {
	if err := authority.ValidateAuthentication(); err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, errNilSnapshotPlan
	}
	if !hasExactIdentity(plan) {
		return nil, errInexactSnapshotPlan
	}
	if previous != nil {
		if err := previous.ValidateAuthentication(); err != nil {
			return nil, err
		}
		if previous.authority != authority {
			return nil, errForeignSnapshot
		}
	}
	var prior *planRoot
	if previous != nil {
		prior = previous.root
	}
	root, err := buildPlanRoot(authority, plan, prior)
	if err != nil {
		return nil, err
	}
	if prior != nil && exactPlanRootPointers(prior, root) {
		return previous, nil
	}
	return sealSnapshot(authority, root), nil
}

func buildPlanRoot(authority *Authority, plan *Plan, prior *planRoot) (*planRoot, error) {
	var priorSections *snapshotCollection[Section]
	var priorBackends *snapshotCollection[Backend]
	var priorProfiles *snapshotCollection[Profile]
	var priorMaps *snapshotCollection[Map]
	var priorCRTLists *snapshotCollection[CRTList]
	var priorFiles *snapshotCollection[File]
	if prior != nil {
		priorSections = prior.sections
		priorBackends = prior.backends
		priorProfiles = prior.profiles
		priorMaps = prior.maps
		priorCRTLists = prior.crtLists
		priorFiles = prior.files
	}
	sections, err := buildSnapshotSequence(
		authority, sectionSnapshotCollection, plan.Sections, priorSections, ownSection, exactSection,
	)
	if err != nil {
		return nil, err
	}
	backends, err := buildSnapshotMap(
		authority, backendSnapshotCollection, plan.Backends, priorBackends, ownBackend, exactBackend,
	)
	if err != nil {
		return nil, err
	}
	profiles, err := buildSnapshotMap(
		authority, profileSnapshotCollection, plan.Profiles, priorProfiles, ownProfile, exactProfile,
	)
	if err != nil {
		return nil, err
	}
	mapsCollection, err := buildSnapshotMap(
		authority, mapSnapshotCollection, plan.Maps, priorMaps, ownMap, exactMap,
	)
	if err != nil {
		return nil, err
	}
	crtLists, err := buildSnapshotMap(
		authority, crtListSnapshotCollection, plan.CRTLists, priorCRTLists, ownCRTList, exactCRTList,
	)
	if err != nil {
		return nil, err
	}
	files, err := buildSnapshotFileSequence(authority, plan.Files, priorFiles)
	if err != nil {
		return nil, err
	}
	if prior != nil && prior.schema == plan.SchemaVersion && prior.id == plan.ID &&
		prior.sections == sections && prior.backends == backends &&
		prior.profiles == profiles && prior.maps == mapsCollection &&
		prior.crtLists == crtLists && prior.files == files {
		return prior, nil
	}
	return sealPlanRoot(
		authority, plan.SchemaVersion, plan.ID, sections, backends, profiles,
		mapsCollection, crtLists, files,
	), nil
}

func exactPlanRootPointers(left, right *planRoot) bool {
	return left != nil && right != nil && left.schema == right.schema && left.id == right.id &&
		left.deferredID == right.deferredID && left.idMemo == right.idMemo &&
		left.sections == right.sections && left.backends == right.backends &&
		left.profiles == right.profiles && left.maps == right.maps &&
		left.crtLists == right.crtLists && left.files == right.files
}

// ValidateAuthentication verifies the private root in constant time.
func (s *Snapshot) ValidateAuthentication() error {
	if s == nil || s.seal != s || s.auth.owner != s || s.authority == nil ||
		s.auth.authority != s.authority || s.root == nil || s.auth.root != s.root ||
		s.auth.entries != s.entries || s.entries < 0 || s.auth.source != s.source ||
		s.shared == nil || s.auth.shared != s.shared {
		return errInvalidSnapshot
	}
	if err := s.authority.ValidateAuthentication(); err != nil {
		return errors.Join(errInvalidSnapshot, err)
	}
	if err := s.root.validate(s.authority); err != nil {
		return err
	}
	if s.root.entries != s.entries {
		return errInvalidSnapshot
	}
	return nil
}

// Len returns the number of top-level collection entries.
func (s *Snapshot) Len() (int, error) {
	if err := s.ValidateAuthentication(); err != nil {
		return 0, err
	}
	return s.entries, nil
}

// ID returns the plan's immutable render identifier.
func (s *Snapshot) ID() (string, error) {
	if err := s.ValidateAuthentication(); err != nil {
		return "", err
	}
	if !s.root.deferredID {
		return s.root.id, nil
	}
	s.root.idMemo.once.Do(func() {
		s.root.idMemo.id, s.root.idMemo.err = s.deferredID()
	})
	return s.root.idMemo.id, s.root.idMemo.err
}

// deferredID streams the canonical encoding out of the entry fragments the
// snapshot already carries, and rebuilds the whole plan only when that stream
// cannot prove its own order. The fallback is counted on the authority so a
// permanently disengaged fast path is visible rather than silent.
func (s *Snapshot) deferredID() (string, error) {
	hasher := xxhash.New()
	err := writeCanonicalPlan(s.root, hasher)
	if err == nil {
		return FormatDigest(hasher.Sum64()), nil
	}
	if !errors.Is(err, errCanonicalOrderUnproven) {
		return "", err
	}
	s.authority.digestFallbacks.Add(1)
	plan, err := s.canonicalCopyWithoutID()
	if err != nil {
		return "", err
	}
	plan.ComputeID()
	return plan.ID, nil
}

// MarshalJSON preserves the canonical fragments and falls back when their order is unproven.
func (s *Snapshot) MarshalJSON() ([]byte, error) {
	id, err := s.ID()
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	err = writePlanJSON(s.root, id, fileJSONFragment, &buffer)
	if err == nil {
		return buffer.Bytes(), nil
	}
	if !errors.Is(err, errCanonicalOrderUnproven) {
		return nil, err
	}
	plan, err := s.LegacyCopy()
	if err != nil {
		return nil, err
	}
	return json.Marshal(plan)
}

// WriteJSON writes the complete encoding only after authentication and canonicalization succeed.
func (s *Snapshot) WriteJSON(target io.Writer) error {
	encoded, err := s.MarshalJSON()
	if err != nil {
		return err
	}
	_, err = target.Write(encoded)
	return err
}

// SameRoot reports exact authenticated root identity.
func (s *Snapshot) SameRoot(other *Snapshot) (bool, error) {
	if err := s.ValidateAuthentication(); err != nil {
		return false, err
	}
	if err := other.ValidateAuthentication(); err != nil {
		return false, err
	}
	return s.authority == other.authority && s.root == other.root, nil
}

// ExactEqual compares complete local plan content; IDs never authorize equality.
func (s *Snapshot) ExactEqual(other *Snapshot) (bool, error) {
	same, err := s.SameRoot(other)
	if err != nil || same {
		return same, err
	}
	if s.root.schema != other.root.schema {
		return false, nil
	}
	equal, err := exactSnapshotCollections(s.root.sections, other.root.sections, exactSection)
	if err != nil || !equal {
		return equal, err
	}
	equal, err = exactSnapshotCollections(s.root.backends, other.root.backends, exactBackend)
	if err != nil || !equal {
		return equal, err
	}
	equal, err = exactSnapshotCollections(s.root.profiles, other.root.profiles, exactProfile)
	if err != nil || !equal {
		return equal, err
	}
	equal, err = exactSnapshotCollections(s.root.maps, other.root.maps, exactMap)
	if err != nil || !equal {
		return equal, err
	}
	equal, err = exactSnapshotCollections(s.root.crtLists, other.root.crtLists, exactCRTList)
	if err != nil || !equal {
		return equal, err
	}
	return exactSnapshotFileCollections(s.root.files, other.root.files)
}

// LegacyCopy materializes a fully detached Plan compatibility value.
func (s *Snapshot) LegacyCopy() (*Plan, error) {
	if err := s.ValidateAuthentication(); err != nil {
		return nil, err
	}
	plan, err := s.legacyCopyWithoutID()
	if err != nil {
		return nil, err
	}
	plan.ID, err = s.ID()
	if err != nil {
		return nil, err
	}
	return plan, nil
}

// SharedPlan is the snapshot's Plan as a read-only view, built once per
// snapshot: its records alias the sealed entries, which every reader of a
// plan must leave alone. A caller that changes a plan takes LegacyCopy.
// Materialising a detached copy per deployment deep-copied every backend
// before any pod was touched.
func (s *Snapshot) SharedPlan() (*Plan, error) {
	if err := s.ValidateAuthentication(); err != nil {
		return nil, err
	}
	s.shared.once.Do(func() {
		plan, err := s.copyWithoutID(false, sharedRecords)
		if err == nil {
			plan.ID, err = s.ID()
		}
		s.shared.plan, s.shared.err = plan, err
	})
	return s.shared.plan, s.shared.err
}

func (s *Snapshot) legacyCopyWithoutID() (*Plan, error) {
	return s.copyWithoutID(false, ownedRecords)
}

func (s *Snapshot) canonicalCopyWithoutID() (*Plan, error) {
	return s.copyWithoutID(true, ownedRecords)
}

// recordDetachment is how a materialised plan takes the snapshot's records:
// owned copies, or the sealed values themselves.
type recordDetachment struct {
	section    func(Section) Section
	backend    func(Backend) Backend
	profile    func(Profile) Profile
	mapFile    func(Map) Map
	crtList    func(CRTList) CRTList
	fileShared bool
}

var (
	ownedRecords = recordDetachment{
		section: ownSection, backend: ownBackend, profile: ownProfile, mapFile: ownMap, crtList: ownCRTList,
	}
	sharedRecords = recordDetachment{
		section: sharedRecord[Section], backend: sharedRecord[Backend], profile: sharedRecord[Profile],
		mapFile: sharedRecord[Map], crtList: sharedRecord[CRTList], fileShared: true,
	}
)

func sharedRecord[T any](value T) T { return value }

func (s *Snapshot) copyWithoutID(canonical bool, detach recordDetachment) (*Plan, error) {
	sections, err := materializeSnapshotSequence(s.root.sections, detach.section)
	if err != nil {
		return nil, err
	}
	backends, err := materializeSnapshotMap(s.root.backends, detach.backend)
	if err != nil {
		return nil, err
	}
	profiles, err := materializeSnapshotMap(s.root.profiles, detach.profile)
	if err != nil {
		return nil, err
	}
	mapsCopy, err := materializeSnapshotMap(s.root.maps, detach.mapFile)
	if err != nil {
		return nil, err
	}
	crtLists, err := materializeSnapshotMap(s.root.crtLists, detach.crtList)
	if err != nil {
		return nil, err
	}
	files, err := materializeSnapshotFiles(s.root.files, canonical, detach.fileShared)
	if err != nil {
		return nil, err
	}
	return &Plan{
		SchemaVersion: s.root.schema,
		Sections:      sections,
		Backends:      backends,
		Profiles:      profiles,
		Maps:          mapsCopy,
		CRTLists:      crtLists,
		Files:         files,
	}, nil
}

func buildSnapshotSequence[T any](
	authority *Authority,
	kind snapshotCollectionKind,
	source []T,
	previous *snapshotCollection[T],
	own func(T) T,
	equal func(T, T) bool,
) (*snapshotCollection[T], error) {
	present := source != nil
	exact, err := exactSequenceSource(authority, kind, source, previous, equal)
	if err != nil {
		return nil, err
	}
	if exact {
		return previous, nil
	}
	previousEntries, err := snapshotEntries(previous, authority, kind)
	if err != nil {
		return nil, err
	}
	entries := make([]*snapshotEntry[T], len(source))
	for index := range source {
		key := snapshotKey{index: index}
		var entry *snapshotEntry[T]
		if index < len(previousEntries) {
			entry = previousEntries[index]
		}
		if entry == nil || !equal(entry.value.value, source[index]) {
			entry = sealSnapshotEntry(authority, kind, key, own(source[index]))
		}
		entries[index] = entry
	}
	return buildSnapshotCollection(authority, kind, present, entries, previous)
}

func snapshotEntries[T any](
	collection *snapshotCollection[T],
	authority *Authority,
	kind snapshotCollectionKind,
) ([]*snapshotEntry[T], error) {
	if collection == nil {
		return nil, nil
	}
	if err := collection.validate(authority, kind); err != nil {
		return nil, err
	}
	entries := make([]*snapshotEntry[T], 0, collection.entries)
	cursor := newSnapshotCursor(collection)
	for {
		entry, found, err := cursor.next()
		if err != nil {
			return nil, err
		}
		if !found {
			return entries, nil
		}
		entries = append(entries, entry)
	}
}

func exactSequenceSource[T any](
	authority *Authority,
	kind snapshotCollectionKind,
	source []T,
	previous *snapshotCollection[T],
	equal func(T, T) bool,
) (bool, error) {
	if previous == nil || previous.present != (source != nil) || previous.entries != len(source) {
		return false, nil
	}
	if err := previous.validate(authority, kind); err != nil {
		return false, err
	}
	cursor := newSnapshotCursor(previous)
	for index := range source {
		entry, found, err := cursor.next()
		if err != nil {
			return false, err
		}
		if !found || !equal(entry.value.value, source[index]) {
			return false, nil
		}
	}
	_, found, err := cursor.next()
	return !found, err
}

func buildSnapshotMap[T any](
	authority *Authority,
	kind snapshotCollectionKind,
	source map[string]T,
	previous *snapshotCollection[T],
	own func(T) T,
	equal func(T, T) bool,
) (*snapshotCollection[T], error) {
	present := source != nil
	exact, err := exactMapSource(authority, kind, source, previous, equal)
	if err != nil {
		return nil, err
	}
	if exact {
		return previous, nil
	}
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	entries := make([]*snapshotEntry[T], len(keys))
	for index, keyName := range keys {
		key := snapshotKey{index: -1, name: keyName}
		value := source[keyName]
		entry, findErr := findSnapshotEntry(authority, kind, previous, key)
		if findErr != nil && !errors.Is(findErr, errSnapshotEntryNotFound) {
			return nil, findErr
		}
		if entry == nil || !equal(entry.value.value, value) {
			ownedKey := snapshotKey{index: -1, name: keyName}
			entry = sealSnapshotEntry(authority, kind, ownedKey, own(value))
		}
		entries[index] = entry
	}
	return buildSnapshotCollection(authority, kind, present, entries, previous)
}

func exactMapSource[T any](
	authority *Authority,
	kind snapshotCollectionKind,
	source map[string]T,
	previous *snapshotCollection[T],
	equal func(T, T) bool,
) (bool, error) {
	if previous == nil || previous.present != (source != nil) || previous.entries != len(source) {
		return false, nil
	}
	if err := previous.validate(authority, kind); err != nil {
		return false, err
	}
	cursor := newSnapshotCursor(previous)
	for {
		entry, found, err := cursor.next()
		if err != nil {
			return false, err
		}
		if !found {
			return true, nil
		}
		value, exists := source[entry.key.name]
		if !exists || !equal(entry.value.value, value) {
			return false, nil
		}
	}
}

func buildSnapshotCollection[T any](
	authority *Authority,
	kind snapshotCollectionKind,
	present bool,
	entries []*snapshotEntry[T],
	previous *snapshotCollection[T],
) (*snapshotCollection[T], error) {
	var previousRoot *snapshotNode[T]
	if previous != nil {
		previousRoot = previous.root
	}
	var root *snapshotNode[T]
	if len(entries) != 0 {
		var err error
		root, err = buildSnapshotTree(authority, kind, entries, previousRoot)
		if err != nil {
			return nil, err
		}
	}
	if previous != nil && previous.present == present && previous.root == root {
		return previous, nil
	}
	return sealSnapshotCollection(authority, kind, present, root), nil
}

func buildSnapshotTree[T any](
	authority *Authority,
	kind snapshotCollectionKind,
	entries []*snapshotEntry[T],
	previous *snapshotNode[T],
) (*snapshotNode[T], error) {
	middle := len(entries) / 2
	entry := entries[middle]
	var previousLeft, previousRight *snapshotNode[T]
	if previous != nil {
		if err := previous.validate(authority, kind); err != nil {
			return nil, err
		}
		if previous.entry.key == entry.key {
			previousLeft = previous.left
			previousRight = previous.right
		} else {
			previous = nil
		}
	}
	var left, right *snapshotNode[T]
	if middle > 0 {
		var err error
		left, err = buildSnapshotTree(authority, kind, entries[:middle], previousLeft)
		if err != nil {
			return nil, err
		}
	}
	if middle+1 < len(entries) {
		var err error
		right, err = buildSnapshotTree(authority, kind, entries[middle+1:], previousRight)
		if err != nil {
			return nil, err
		}
	}
	if previous != nil && previous.entry == entry && previous.left == left && previous.right == right {
		return previous, nil
	}
	return newSnapshotNode(authority, kind, entry, left, right), nil
}

var errSnapshotEntryNotFound = errors.New("render plan snapshot entry is absent")

func findSnapshotEntry[T any](
	authority *Authority,
	kind snapshotCollectionKind,
	collection *snapshotCollection[T],
	key snapshotKey,
) (*snapshotEntry[T], error) {
	if collection == nil {
		return nil, errSnapshotEntryNotFound
	}
	if err := collection.validate(authority, kind); err != nil {
		return nil, err
	}
	for node := collection.root; node != nil; {
		if err := node.validate(authority, kind); err != nil {
			return nil, err
		}
		switch comparison := compareSnapshotKeys(key, node.entry.key); {
		case comparison < 0:
			node = node.left
		case comparison > 0:
			node = node.right
		default:
			return node.entry, nil
		}
	}
	return nil, errSnapshotEntryNotFound
}

const maximumSnapshotTreeDepth = 64

type snapshotCursor[T any] struct {
	authority *Authority
	kind      snapshotCollectionKind
	stack     [maximumSnapshotTreeDepth]*snapshotNode[T]
	depth     int
	err       error
}

func newSnapshotCursor[T any](collection *snapshotCollection[T]) *snapshotCursor[T] {
	cursor := &snapshotCursor[T]{authority: collection.authority, kind: collection.kind}
	cursor.pushLeft(collection.root)
	return cursor
}

func (c *snapshotCursor[T]) pushLeft(node *snapshotNode[T]) {
	for node != nil && c.err == nil {
		if c.depth == len(c.stack) {
			c.err = errSnapshotTreeTooDeep
			return
		}
		c.stack[c.depth] = node
		c.depth++
		node = node.left
	}
}

func (c *snapshotCursor[T]) next() (*snapshotEntry[T], bool, error) {
	if c.err != nil {
		return nil, false, c.err
	}
	if c.depth == 0 {
		return nil, false, nil
	}
	c.depth--
	node := c.stack[c.depth]
	c.stack[c.depth] = nil
	if err := node.validate(c.authority, c.kind); err != nil {
		return nil, false, err
	}
	c.pushLeft(node.right)
	if c.err != nil {
		return nil, false, c.err
	}
	return node.entry, true, nil
}

func exactSnapshotCollections[T any](
	left, right *snapshotCollection[T],
	equal func(T, T) bool,
) (bool, error) {
	if err := left.validate(left.authority, left.kind); err != nil {
		return false, err
	}
	if err := right.validate(right.authority, right.kind); err != nil {
		return false, err
	}
	if left == right {
		return true, nil
	}
	if left.kind != right.kind || left.present != right.present || left.entries != right.entries {
		return false, nil
	}
	leftCursor := newSnapshotCursor(left)
	rightCursor := newSnapshotCursor(right)
	for {
		leftEntry, leftFound, err := leftCursor.next()
		if err != nil {
			return false, err
		}
		rightEntry, rightFound, err := rightCursor.next()
		if err != nil {
			return false, err
		}
		if leftFound != rightFound {
			return false, errInvalidSnapshot
		}
		if !leftFound {
			return true, nil
		}
		if leftEntry == rightEntry {
			continue
		}
		if collectionUsesKeys(left.kind) && leftEntry.key != rightEntry.key ||
			!equal(leftEntry.value.value, rightEntry.value.value) {
			return false, nil
		}
	}
}

func collectionUsesKeys(kind snapshotCollectionKind) bool {
	return kind != sectionSnapshotCollection && kind != fileSnapshotCollection
}

func materializeSnapshotSequence[T any](
	collection *snapshotCollection[T],
	detach func(T) T,
) ([]T, error) {
	if err := collection.validate(collection.authority, collection.kind); err != nil {
		return nil, err
	}
	if !collection.present {
		return nil, nil
	}
	result := make([]T, collection.entries)
	cursor := newSnapshotCursor(collection)
	for index := range result {
		entry, found, err := cursor.next()
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errInvalidSnapshot
		}
		result[index] = detach(entry.value.value)
	}
	_, found, err := cursor.next()
	if err != nil {
		return nil, err
	}
	if found {
		return nil, errInvalidSnapshot
	}
	return result, nil
}

func materializeSnapshotMap[T any](
	collection *snapshotCollection[T],
	detach func(T) T,
) (result map[string]T, err error) {
	if err := collection.validate(collection.authority, collection.kind); err != nil {
		return nil, err
	}
	if !collection.present {
		return result, nil
	}
	result = make(map[string]T, collection.entries)
	cursor := newSnapshotCursor(collection)
	for {
		entry, found, err := cursor.next()
		if err != nil {
			return nil, err
		}
		if !found {
			return result, nil
		}
		if entry.key.index != -1 {
			return nil, errInvalidSnapshot
		}
		result[entry.key.name] = detach(entry.value.value)
	}
}

func ownSection(source Section) Section {
	return source
}

func exactSection(left, right Section) bool {
	return left == right
}

func ownBackend(source Backend) Backend {
	return Backend{
		Name: source.Name, Profile: source.Profile,
		Mode: source.Mode, GUID: source.GUID,
		Balance: source.Balance, HashType: source.HashType,
		Shape: source.Shape, ShapeReason: source.ShapeReason,
		Servers: ownServers(source.Servers), DefaultServer: ownKeywordArgs(source.DefaultServer),
		BodyDigest: source.BodyDigest, CommentsDigest: source.CommentsDigest,
		RecordDigest: source.RecordDigest, TextDigest: source.TextDigest,
		Body: ownStrings(source.Body), Comments: ownStrings(source.Comments),
		ContentKnown: source.ContentKnown,
	}
}

func exactBackend(left, right Backend) bool {
	return left.Name == right.Name && left.Profile == right.Profile && left.Mode == right.Mode &&
		left.GUID == right.GUID && left.Balance == right.Balance && left.HashType == right.HashType &&
		left.Shape == right.Shape && left.ShapeReason == right.ShapeReason &&
		exactServers(left.Servers, right.Servers) &&
		exactKeywordArgs(left.DefaultServer, right.DefaultServer) &&
		left.BodyDigest == right.BodyDigest && left.CommentsDigest == right.CommentsDigest &&
		left.RecordDigest == right.RecordDigest && left.TextDigest == right.TextDigest &&
		exactStrings(left.Body, right.Body) && exactStrings(left.Comments, right.Comments) &&
		left.ContentKnown == right.ContentKnown
}

func ownServers(source []Server) []Server {
	if source == nil {
		return nil
	}
	result := make([]Server, len(source))
	for index := range source {
		server := source[index]
		result[index] = Server{
			Name: server.Name, Address: server.Address,
			Port: server.Port, Disabled: server.Disabled, GUID: server.GUID,
			Comment: server.Comment, Extra: ownKeywordArgs(server.Extra),
		}
		if server.Weight != nil {
			weight := *server.Weight
			result[index].Weight = &weight
		}
	}
	return result
}

func exactServers(left, right []Server) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	if sameSlice(left, right) {
		return true
	}
	for index := range left {
		leftServer := left[index]
		rightServer := right[index]
		if leftServer.Name != rightServer.Name || leftServer.Address != rightServer.Address ||
			leftServer.Port != rightServer.Port || leftServer.Disabled != rightServer.Disabled ||
			leftServer.GUID != rightServer.GUID || leftServer.Comment != rightServer.Comment ||
			!exactIntPointers(leftServer.Weight, rightServer.Weight) ||
			!exactKeywordArgs(leftServer.Extra, rightServer.Extra) {
			return false
		}
	}
	return true
}

func exactIntPointers(left, right *int) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func ownKeywordArgs(source []KeywordArg) []KeywordArg {
	if source == nil {
		return nil
	}
	result := make([]KeywordArg, len(source))
	for index := range source {
		result[index] = KeywordArg{
			Name: source[index].Name, Args: ownStrings(source[index].Args),
		}
	}
	return result
}

func exactKeywordArgs(left, right []KeywordArg) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	if sameSlice(left, right) {
		return true
	}
	for index := range left {
		if left[index].Name != right[index].Name || !exactStrings(left[index].Args, right[index].Args) {
			return false
		}
	}
	return true
}

func ownStrings(source []string) []string {
	return slices.Clone(source)
}

func exactStrings(left, right []string) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	return sameSlice(left, right) || slices.Equal(left, right)
}

func ownProfile(source Profile) Profile {
	return source
}

func exactProfile(left, right Profile) bool {
	return left == right
}

func ownMap(source Map) Map {
	entries := make([]Entry, len(source.Entries))
	if source.Entries == nil {
		entries = nil
	}
	copy(entries, source.Entries)
	return Map{Path: source.Path, Ordered: source.Ordered, Entries: entries}
}

func exactMap(left, right Map) bool {
	return left.Path == right.Path && left.Ordered == right.Ordered &&
		(left.Entries == nil) == (right.Entries == nil) && slices.Equal(left.Entries, right.Entries)
}

func ownCRTList(source CRTList) CRTList {
	entries := make([]CRTListEntry, len(source.Entries))
	if source.Entries == nil {
		entries = nil
	}
	for index := range source.Entries {
		entries[index] = CRTListEntry{
			Cert:       source.Entries[index].Cert,
			Options:    ownKeywordArgs(source.Entries[index].Options),
			SNIFilters: ownStrings(source.Entries[index].SNIFilters),
		}
	}
	return CRTList{Path: source.Path, Entries: entries}
}

func exactCRTList(left, right CRTList) bool {
	if left.Path != right.Path || (left.Entries == nil) != (right.Entries == nil) ||
		len(left.Entries) != len(right.Entries) {
		return false
	}
	for index := range left.Entries {
		leftEntry := left.Entries[index]
		rightEntry := right.Entries[index]
		if leftEntry.Cert != rightEntry.Cert ||
			!exactKeywordArgs(leftEntry.Options, rightEntry.Options) ||
			!exactStrings(leftEntry.SNIFilters, rightEntry.SNIFilters) {
			return false
		}
	}
	return true
}

func ownFile(source File) File {
	return source
}

func exactFile(left, right File) bool {
	return left == right
}
