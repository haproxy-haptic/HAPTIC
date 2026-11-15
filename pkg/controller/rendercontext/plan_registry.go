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

package rendercontext

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/rendercontent"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// sectionNamePattern is the character set a section name may use. It excludes
// whitespace and '@' so a name can never close or forge a token.
var sectionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// validSectionName is sectionNamePattern without the regexp: it runs once
// per token per render, 6,000 times at 3,000 routes.
func validSectionName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// PlanRegistry collects the structure templates declare about the config they
// emit and assembles the final config from the placeholder tokens they got
// back. One per render; every method is safe for the sharded render's
// goroutines.
//
// Templates reach it as the `planRegistry` global (templating.PlanRegistrar).
type PlanRegistry struct {
	mu sync.Mutex

	authority *PlanTokenAuthority
	nonce     string
	// fragmentMarker is the fragment token's fixed prefix. cutFragmentToken
	// runs once per line of the assembled config, so it is built once here.
	fragmentMarker string

	// paths turns a map/cert/crt-list name into the base-relative path the
	// config references it by; nil (tests) keeps names as they are.
	paths *templating.PathResolver

	sections            map[sectionKey]string
	fragments           map[string]rendercontent.TextFragment
	backends            map[string]renderplan.Backend
	mapsMeta            map[string]bool
	sensitiveMaps       map[string]struct{}
	memo                *PlanMemo
	assembled           []renderplan.Section
	prepared            *PreparedPlanSnapshot
	assembly            *renderAssemblyGeneration
	documentAssembly    *planDocumentAssembly
	declarationRevision uint64
}

type sectionKey struct {
	Kind string
	Name string
}

var _ templating.PlanRegistrar = (*PlanRegistry)(nil)

// PlanTokenAuthority makes placeholders unguessable within one immutable renderer.
type PlanTokenAuthority struct {
	nonce string
	seal  *PlanTokenAuthority
}

// NewPlanTokenAuthority creates an authenticated placeholder namespace.
func NewPlanTokenAuthority() *PlanTokenAuthority {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	authority := &PlanTokenAuthority{nonce: hex.EncodeToString(raw[:])}
	authority.seal = authority
	return authority
}

func (a *PlanTokenAuthority) validate() error {
	if a == nil || a.seal != a || a.nonce == "" {
		return errors.New("plan token authority has an invalid authentication seal")
	}
	return nil
}

// NewPlanRegistry creates a registry with a fresh random nonce. paths may be
// nil, in which case file names are used unresolved.
func NewPlanRegistry(paths *templating.PathResolver) *PlanRegistry {
	registry, err := NewPlanRegistryWithAuthority(paths, NewPlanTokenAuthority())
	if err != nil {
		panic(err)
	}
	return registry
}

// NewPlanRegistryWithAuthority reuses one sealed placeholder namespace.
func NewPlanRegistryWithAuthority(
	paths *templating.PathResolver,
	authority *PlanTokenAuthority,
) (*PlanRegistry, error) {
	if err := authority.validate(); err != nil {
		return nil, err
	}
	return &PlanRegistry{
		authority:      authority,
		nonce:          authority.nonce,
		fragmentMarker: "# @haptic:" + authority.nonce + ":fragment:",
		paths:          paths,
		sections:       make(map[sectionKey]string),
		fragments:      make(map[string]rendercontent.TextFragment),
		backends:       make(map[string]renderplan.Backend),
		mapsMeta:       make(map[string]bool),
		sensitiveMaps:  make(map[string]struct{}),
	}, nil
}

func (r *PlanRegistry) validateTokenAuthority() error {
	if r == nil || r.authority == nil {
		return errors.New("planRegistry: plan token authority is missing")
	}
	if err := r.authority.validate(); err != nil {
		return fmt.Errorf("planRegistry: %w", err)
	}
	if r.nonce != r.authority.nonce {
		return errors.New("planRegistry: plan token authority does not match its namespace")
	}
	return nil
}

// PreparedPlanTokenAuthority returns the authenticated namespace used by prepared plan output.
func (r *PlanRegistry) PreparedPlanTokenAuthority() (*PlanTokenAuthority, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateTokenAuthority(); err != nil {
		return nil, err
	}
	return r.authority, nil
}

// Section registers section text and returns the token line that stands in for
// it until assembly. Re-registering identical text is a no-op — sharded
// rendering legitimately emits the same profile from several goroutines —
// while different text under the same name is an error.
func (r *PlanRegistry) Section(kind, name, text string) (string, error) {
	if kind != renderplan.SectionKindProfile && kind != renderplan.SectionKindBackend {
		return "", fmt.Errorf("planRegistry.Section: kind must be %q or %q, got %q",
			renderplan.SectionKindProfile, renderplan.SectionKindBackend, kind)
	}
	if !sectionNamePattern.MatchString(name) {
		return "", fmt.Errorf("planRegistry.Section: %s name %q must match %s", kind, name, sectionNamePattern)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registerSection(kind, name, text)
}

// registerSection stores the text under (kind, name). Caller holds the lock.
func (r *PlanRegistry) registerSection(kind, name, text string) (string, error) {
	key := sectionKey{Kind: kind, Name: name}
	if existing, ok := r.section(kind, name); ok {
		if existing != text {
			return "", fmt.Errorf("planRegistry: %s %q registered twice with different text (%d and %d bytes)",
				kind, name, len(existing), len(text))
		}
		return r.sectionToken(kind, name), nil
	}
	r.sections[key] = text
	r.declarationRevision++
	return r.sectionToken(kind, name), nil
}

// Backend records a backend as data and registers the section text emitted
// from it, so a consumer never has to read the text back to know what changed.
func (r *PlanRegistry) Backend(record map[string]any, text string) (string, error) {
	prepared, err := PreparePlanBackend(record, text)
	if err != nil {
		return "", err
	}
	return r.registerPreparedBackend(&prepared)
}

// RegisterPreparedBackend replays a validated backend declaration.
func (r *PlanRegistry) RegisterPreparedBackend(prepared *PreparedPlanBackend) (string, error) {
	if err := prepared.Validate(); err != nil {
		return "", fmt.Errorf("planRegistry.Backend: %w", err)
	}
	return r.registerPreparedBackend(prepared)
}

func (r *PlanRegistry) registerPreparedBackend(prepared *PreparedPlanBackend) (string, error) {
	detached := prepared.Clone()
	backend := detached.Backend
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok, err := r.backend(backend.Name); err != nil {
		return "", err
	} else if ok && !sameBackendRecordExact(&existing, &backend) {
		return "", fmt.Errorf("planRegistry.Backend: backend %q declared twice with different values", backend.Name)
	}
	token, err := r.registerSection(renderplan.SectionKindBackend, backend.Name, detached.Text)
	if err != nil {
		return "", err
	}
	if _, exists := r.backends[backend.Name]; !exists {
		r.declarationRevision++
	}
	r.backends[backend.Name] = backend
	return token, nil
}

// AttachPreparedPlan makes an authenticated immutable declaration set visible to this render.
func (r *PlanRegistry) AttachPreparedPlan(snapshot *PreparedPlanSnapshot) error {
	if err := snapshot.ValidateAuthentication(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prepared != nil {
		if r.prepared == snapshot {
			return nil
		}
		return errors.New("planRegistry: a prepared plan is already attached")
	}
	for key, text := range r.sections {
		if existing, exists := snapshot.section(key.Kind, key.Name); exists && existing != text {
			return fmt.Errorf("planRegistry: %s %q registered twice with different text (%d and %d bytes)",
				key.Kind, key.Name, len(text), len(existing))
		}
	}
	for name := range r.backends {
		backend := r.backends[name]
		existing, exists, err := snapshot.backend(name)
		if err != nil {
			return err
		}
		if exists && !sameBackendRecordExact(&existing, &backend) {
			return fmt.Errorf("planRegistry.Backend: backend %q declared twice with different values", name)
		}
	}
	r.prepared = snapshot
	r.declarationRevision++
	return nil
}

// PreparedBackendToken returns this render's token for an attached backend declaration.
func (r *PlanRegistry) PreparedBackendToken(name string) (string, error) {
	r.mu.Lock()
	prepared := r.prepared
	r.mu.Unlock()
	if prepared == nil {
		return "", errors.New("planRegistry: no prepared plan is attached")
	}
	return r.PreparedBackendTokenIn(prepared, name)
}

// PreparedBackendTokenIn returns this render's token for a backend that
// snapshot declares. A memo built for an earlier generation of the attached
// plan materializes against that generation's snapshot.
func (r *PlanRegistry) PreparedBackendTokenIn(snapshot *PreparedPlanSnapshot, name string) (string, error) {
	if !sectionNamePattern.MatchString(name) {
		return "", fmt.Errorf("planRegistry: backend name %q must match %s", name, sectionNamePattern)
	}
	if err := snapshot.ValidateAuthentication(); err != nil {
		return "", err
	}
	if _, exists := snapshot.section(renderplan.SectionKindBackend, name); !exists {
		return "", fmt.Errorf("planRegistry: prepared backend %q is unavailable", name)
	}
	if _, exists := snapshot.backends.Root().Get([]byte(name)); !exists {
		return "", fmt.Errorf("planRegistry: prepared backend %q has no record", name)
	}
	return r.sectionToken(renderplan.SectionKindBackend, name), nil
}

// ProfileGroup returns the token line the assembler replaces with every
// registered profile section, sorted by name.
func (r *PlanRegistry) ProfileGroup() string {
	return "# @haptic:" + r.nonce + ":group:profiles@\n"
}

// MapMeta declares whether entry order matters for a map file. Maps are
// ordered unless a template says otherwise, so a third-party map file gets the
// safe treatment by default.
func (r *PlanRegistry) MapMeta(path string, ordered bool) error {
	if path == "" {
		return fmt.Errorf("planRegistry.MapMeta: path must not be empty")
	}

	resolved, err := r.MapPath(path)
	if err != nil {
		return fmt.Errorf("planRegistry.MapMeta: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.mapsMeta[resolved]; ok && existing != ordered {
		return fmt.Errorf("planRegistry.MapMeta: map %q declared both ordered and unordered", resolved)
	}
	if _, exists := r.mapsMeta[resolved]; !exists {
		r.declarationRevision++
	}
	r.mapsMeta[resolved] = ordered
	return nil
}

// SensitiveMap declares that a map file holds credentials, for the whole render:
// it is published as a Secret, kept out of the plan's map entries and replaced
// as a whole at runtime. Declaring it again is a no-op.
func (r *PlanRegistry) SensitiveMap(path string) error {
	if path == "" {
		return fmt.Errorf("planRegistry.SensitiveMap: path must not be empty")
	}
	resolved, err := r.MapPath(path)
	if err != nil {
		return fmt.Errorf("planRegistry.SensitiveMap: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// No declarationRevision bump: sensitivity never changes the assembled
	// sections, and a map root may declare it after assembly.
	r.sensitiveMaps[resolved] = struct{}{}
	return nil
}

// SealSensitiveMaps moves every map file declared sensitive from MapFiles into
// SensitiveMapFiles, sealed under key. files is not modified.
func (r *PlanRegistry) SealSensitiveMaps(files *dataplane.AuxiliaryFiles, key []byte) (*dataplane.AuxiliaryFiles, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if files == nil || len(r.sensitiveMaps) == 0 {
		return files, nil
	}
	sealed := *files
	sealed.MapFiles = make([]auxiliaryfiles.MapFile, 0, len(files.MapFiles))
	sealed.SensitiveMapFiles = slices.Clone(files.SensitiveMapFiles)
	for _, file := range files.MapFiles {
		resolved, err := r.MapPath(file.Path)
		if err != nil {
			return nil, err
		}
		if _, sensitive := r.sensitiveMaps[resolved]; !sensitive {
			sealed.MapFiles = append(sealed.MapFiles, file)
			continue
		}
		sealed.SensitiveMapFiles = append(sealed.SensitiveMapFiles, auxiliaryfiles.SensitiveMapFile{
			Path: file.Path, Content: file.Content, Seal: auxiliaryfiles.SealSensitiveMap(key, file.Content),
		})
	}
	return dataplane.CanonicalizeAuxiliaryFiles(&sealed)
}

// MapPath is the base-relative path a map file is referenced by in the config
// (`maps/host.map`), which is also its runtime name.
func (r *PlanRegistry) MapPath(name string) (string, error) {
	return r.filePath(name, "map")
}

// filePath resolves a static name or an already-resolved registry path to the
// same base-relative string GetPath hands templates (cert names sanitised).
// It is idempotent: resolving a resolved path returns it unchanged.
func (r *PlanRegistry) filePath(name, kind string) (string, error) {
	if r.paths == nil {
		return name, nil
	}
	dir, err := r.paths.GetPath("", kind)
	if err != nil {
		return "", fmt.Errorf("resolve %s %q: %w", kind, name, err)
	}
	resolved, err := r.paths.GetPath(strings.TrimPrefix(name, dir.(string)+"/"), kind)
	if err != nil {
		return "", fmt.Errorf("resolve %s %q: %w", kind, name, err)
	}
	return resolved.(string), nil
}

// Plan bundles the assembled sections with the recorded backends, the profiles,
// the entries of every map file and the file set, and computes the plan ID.
// Call it after Assemble; without it the plan carries no sections. Every path
// in the plan is the base-relative string the config references the file by.
func (r *PlanRegistry) Plan(config string, aux *dataplane.AuxiliaryFiles) (*renderplan.Plan, error) {
	plan, _, err := r.plan(config, aux, nil)
	return plan, err
}

// PlanWithCache reuses a plan only for exact authenticated render inputs.
func (r *PlanRegistry) PlanWithCache(
	config string,
	aux *dataplane.AuxiliaryFiles,
	session *RenderCacheSession,
) (*renderplan.Plan, error) {
	plan, _, err := r.plan(config, aux, session)
	return plan, err
}

// PlanWithCacheIdentity returns the exact cache generation that produced plan.
func (r *PlanRegistry) PlanWithCacheIdentity(
	config string,
	aux *dataplane.AuxiliaryFiles,
	session *RenderCacheSession,
) (*renderplan.Plan, *RenderPlanIdentity, error) {
	return r.plan(config, aux, session)
}

func (r *PlanRegistry) plan(
	config string,
	aux *dataplane.AuxiliaryFiles,
	session *RenderCacheSession,
) (*renderplan.Plan, *RenderPlanIdentity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session != nil {
		cached, identity, hit, err := session.loadPlan(r, config, aux)
		if err != nil {
			return nil, nil, err
		}
		if hit {
			return cached, identity, nil
		}
	}
	files, mapContents, err := r.planFiles(config, aux)
	if err != nil {
		return nil, nil, err
	}

	backends, err := r.planBackends()
	if err != nil {
		return nil, nil, err
	}
	plan := &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		Sections:      slices.Clone(r.assembled),
		Backends:      backends,
		Profiles:      r.profiles(),
		Maps:          r.mapFiles(mapContents),
		Files:         sortedFiles(files),
	}
	plan.ComputeID()
	var identity *RenderPlanIdentity
	if session != nil && r.assembly != nil {
		identity, err = session.storePlan(r, config, aux, plan)
		if err != nil {
			return nil, nil, err
		}
	}
	return plan, identity, nil
}

// planBackends shares the sealed prepared snapshot's slices with the plan:
// every consumer copies before it mutates (renderplan owns its entries, the
// deploy side clones), and cloning here priced each render by the fleet size.
func (r *PlanRegistry) planBackends() (map[string]renderplan.Backend, error) {
	var backends map[string]renderplan.Backend
	if r.prepared != nil {
		if err := r.prepared.ValidateAuthentication(); err != nil {
			return nil, err
		}
		backends = maps.Clone(r.memo.preparedBackends(r.prepared))
		for name := range r.backends {
			backend := r.backends[name]
			if existing, exists := backends[name]; exists && !sameBackendRecordExact(&existing, &backend) {
				return nil, fmt.Errorf("planRegistry.Backend: backend %q declared twice with different values", name)
			}
			backends[name] = backend
		}
	} else {
		backends = maps.Clone(r.backends)
	}
	textDigests := make(map[string]string)
	for _, section := range r.assembled {
		if section.Kind == renderplan.SectionKindBackend {
			textDigests[section.Name] = section.TextDigest
		}
	}
	for name := range backends {
		backend := backends[name]
		backend.TextDigest = textDigests[name]
		backends[name] = backend
	}
	return backends, nil
}

func sameBackendRecordExact(left, right *renderplan.Backend) bool {
	return left.EqualRecord(right) && left.EqualContent(right)
}

// profiles derives the profile records from the exact assembled section bytes.
// Caller holds the lock.
func (r *PlanRegistry) profiles() map[string]renderplan.Profile {
	profiles := make(map[string]renderplan.Profile)
	for _, section := range r.assembled {
		if section.Kind != renderplan.SectionKindProfile {
			continue
		}
		_, body, _ := strings.Cut(section.Text, "\n")
		profiles[section.Name] = renderplan.Profile{Name: section.Name, BodyDigest: renderplan.DigestString(body)}
	}
	return profiles
}

func (r *PlanRegistry) section(kind, name string) (string, bool) {
	if text, exists := r.sections[sectionKey{Kind: kind, Name: name}]; exists {
		return text, true
	}
	if r.prepared == nil {
		return "", false
	}
	return r.prepared.section(kind, name)
}

func (r *PlanRegistry) backend(name string) (renderplan.Backend, bool, error) {
	if backend, exists := r.backends[name]; exists {
		return backend, true, nil
	}
	if r.prepared == nil {
		return renderplan.Backend{}, false, nil
	}
	return r.prepared.backend(name)
}

// mapFiles derives the entry list of every map file from its rendered content.
// Caller holds the lock.
func (r *PlanRegistry) mapFiles(contents map[string]string) map[string]renderplan.Map {
	files := make(map[string]renderplan.Map, len(contents))
	for path, content := range contents {
		ordered, declared := r.mapsMeta[path]
		files[path] = renderplan.Map{
			Path:    path,
			Ordered: !declared || ordered,
			Entries: r.memo.parseMapEntries(path, content),
		}
	}
	return files
}

// PlanMemo carries plan derivations across a render service's renders: each
// map file's parsed entries while its text is unchanged, and the prepared
// snapshot's backends as plan records while the snapshot is the same. Both
// are shared, never mutated: renderplan copies them into its snapshot and the
// deploy side clones before editing.
type PlanMemo struct {
	mu           sync.Mutex
	files        map[string]memoizedMapEntries
	backendsRoot *iradix.Node[PreparedPlanBackend]
	backends     map[string]renderplan.Backend
}

type memoizedMapEntries struct {
	parsed renderplan.ParsedMapEntries
}

// NewPlanMemo creates an empty memo shared by one render service.
func NewPlanMemo() *PlanMemo {
	return &PlanMemo{files: make(map[string]memoizedMapEntries)}
}

func (m *PlanMemo) parseMapEntries(path, content string) []renderplan.Entry {
	if m == nil {
		return renderplan.ParseMapEntries(content)
	}
	m.mu.Lock()
	known, exists := m.files[path]
	m.mu.Unlock()
	var parsed renderplan.ParsedMapEntries
	if exists {
		parsed = known.parsed.Reparse(content)
	} else {
		parsed = renderplan.ParseMapEntriesIndexed(content)
	}
	m.mu.Lock()
	m.files[path] = memoizedMapEntries{parsed: parsed}
	m.mu.Unlock()
	return parsed.Entries
}

// preparedBackends returns the prepared snapshot's backends as plan records
// without their text digests, which change per render. The map is shared:
// callers clone it before adding their own entries.
func (m *PlanMemo) preparedBackends(prepared *PreparedPlanSnapshot) map[string]renderplan.Backend {
	root := prepared.backends.Root()
	if m != nil {
		m.mu.Lock()
		if m.backendsRoot == root && m.backends != nil {
			backends := m.backends
			m.mu.Unlock()
			return backends
		}
		m.mu.Unlock()
	}
	backends := make(map[string]renderplan.Backend, prepared.backends.Len())
	root.Walk(func(name []byte, entry PreparedPlanBackend) bool {
		backends[string(name)] = preparedBackendRecord(&entry)
		return false
	})
	if m != nil {
		m.mu.Lock()
		m.backendsRoot, m.backends = root, backends
		m.mu.Unlock()
	}
	return backends
}

func sharedStrings(source []string) []string {
	if len(source) == 0 {
		return nil
	}
	return source
}

func sortedFiles(files []renderplan.File) []renderplan.File {
	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b renderplan.File) int { return strings.Compare(a.Path, b.Path) })
	return sorted
}

func (r *PlanRegistry) sectionToken(kind, name string) string {
	return "# @haptic:" + r.nonce + ":section:" + kind + ":" + name + "@\n"
}

func (r *PlanRegistry) fragmentToken(name string) string {
	return r.fragmentMarker + name + "@\n"
}

// cutFragmentToken accepts a token that follows text on its own line: the
// fragment's leading newline is what ends that line in an inline render, so
// dropping the token's newline here reproduces it exactly.
func (r *PlanRegistry) cutFragmentToken(line string) (prefix, name string, ok bool) {
	start := strings.Index(line, r.fragmentMarker)
	if start < 0 {
		return "", "", false
	}
	rest := line[start+len(r.fragmentMarker):]
	end := strings.Index(rest, "@")
	if end < 0 {
		return "", "", false
	}
	if trailing := strings.TrimSuffix(rest[end+1:], "\n"); trailing != "" {
		return "", "", false
	}
	name = rest[:end]
	if !validSectionName(name) {
		return "", "", false
	}
	return strings.Clone(line[:start]), strings.Clone(name), true
}

// Fragment implements templating.PlanRegistrar. Bypassing the template writer
// is the point: at chart scale the per-route rules are megabytes the root
// template would otherwise re-emit every render, already memoised.
func (r *PlanRegistry) Fragment(name string, value templating.TextFragment) (string, error) {
	if !sectionNamePattern.MatchString(name) {
		return "", fmt.Errorf("planRegistry.Fragment: name %q must match %s", name, sectionNamePattern)
	}
	fragment, ok := value.(rendercontent.TextFragment)
	if !ok {
		return "", fmt.Errorf("planRegistry.Fragment %q: unsupported fragment type %T", name, value)
	}
	if err := fragment.ValidateAuthentication(); err != nil {
		return "", fmt.Errorf("planRegistry.Fragment %q: %w", name, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.fragments[name]; ok {
		same, err := existing.SameRoot(fragment)
		if err != nil {
			return "", err
		}
		if !same {
			return "", fmt.Errorf("planRegistry: fragment %q registered twice with different text", name)
		}
		return r.fragmentToken(name), nil
	}
	r.fragments[name] = fragment
	r.declarationRevision++
	return r.fragmentToken(name), nil
}

// fragment returns registered fragment text. Caller holds the lock.
func (r *PlanRegistry) fragment(name string) (rendercontent.TextFragment, bool) {
	value, ok := r.fragments[name]
	return value, ok
}

// marker is the nonce-carrying prefix every token of this render shares.
func (r *PlanRegistry) marker() string {
	return "@haptic:" + r.nonce
}
