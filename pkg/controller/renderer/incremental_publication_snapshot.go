// Copyright 2026 Philipp Hossner
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

package renderer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type incrementalPublicationSnapshotAuthority struct {
	seal       *incrementalPublicationSnapshotAuthority
	generation *incrementalPublicationSnapshotGeneration
}

type incrementalPublicationSnapshotGeneration struct {
	seal      *incrementalPublicationSnapshotGeneration
	authority *incrementalPublicationSnapshotAuthority

	// liveSources counts captured source snapshots. A generation with none —
	// every query outside an actively publishing render — resolves nothing,
	// so readers skip the per-winner source walk entirely.
	liveSources atomic.Int64

	mu     sync.RWMutex
	active bool
	shards *incrementalPublicationSnapshotShards
}

func (g *incrementalPublicationSnapshotGeneration) hasLiveSources() bool {
	return g != nil && g.liveSources.Load() > 0
}

const incrementalPublicationSnapshotShardCount = 64

var incrementalPublicationSnapshotShardSeed = maphash.MakeSeed()

type incrementalPublicationSnapshotShards struct {
	seal    *incrementalPublicationSnapshotShards
	sources [incrementalPublicationSnapshotShardCount]incrementalPublicationSourceShard
	derived [incrementalPublicationSnapshotShardCount]incrementalPublicationDerivedShard
}

type incrementalPublicationSourceShard struct {
	mu     sync.RWMutex
	values map[string]*incrementalPublicationSnapshot
}

type incrementalPublicationDerivedShard struct {
	mu     sync.RWMutex
	values map[incrementalPublicationSnapshotBinding]*incrementalPublicationSnapshot
}

type incrementalPublicationSnapshotBinding struct {
	key      incremental.InputKey
	revision incremental.Revision
	found    bool
	encoded  string
}

type incrementalPublicationSnapshot struct {
	seal        *incrementalPublicationSnapshot
	generation  *incrementalPublicationSnapshotGeneration
	binding     incrementalPublicationSnapshotBinding
	value       any
	certificate *templating.IncrementalImmutableCertificate
	proof       *incrementalPublicationSnapshotProof
}

type incrementalPublicationSnapshotProof struct {
	seal        *incrementalPublicationSnapshotProof
	generation  *incrementalPublicationSnapshotGeneration
	binding     incrementalPublicationSnapshotBinding
	value       any
	certificate *templating.IncrementalImmutableCertificate
}

func newIncrementalPublicationSnapshotGeneration() (
	*incrementalPublicationSnapshotGeneration,
	*incrementalPublicationSnapshotAuthority,
) {
	authority := &incrementalPublicationSnapshotAuthority{}
	authority.seal = authority
	shards := &incrementalPublicationSnapshotShards{}
	shards.seal = shards
	for index := range incrementalPublicationSnapshotShardCount {
		shards.sources[index].values = map[string]*incrementalPublicationSnapshot{}
		shards.derived[index].values = map[incrementalPublicationSnapshotBinding]*incrementalPublicationSnapshot{}
	}
	generation := &incrementalPublicationSnapshotGeneration{
		authority: authority,
		active:    true,
		shards:    shards,
	}
	generation.seal = generation
	authority.generation = generation
	return generation, authority
}

func (g *incrementalPublicationSnapshotGeneration) validLocked() bool {
	return g != nil && g.seal == g && g.authority != nil &&
		g.authority.seal == g.authority && g.authority.generation == g && g.active &&
		g.shards != nil && g.shards.seal == g.shards
}

func (g *incrementalPublicationSnapshotGeneration) validFor(
	authority *incrementalPublicationSnapshotAuthority,
) bool {
	if g == nil {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.validLocked() && g.authority == authority
}

func (g *incrementalPublicationSnapshotGeneration) authenticateAuthority(
	authority *incrementalPublicationSnapshotAuthority,
) error {
	if !g.validFor(authority) {
		return errors.New("incremental publication snapshot generation has invalid ownership")
	}
	return nil
}

func (g *incrementalPublicationSnapshotGeneration) revoke() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.active = false
	if g.shards != nil && g.shards.seal == g.shards {
		for index := range incrementalPublicationSnapshotShardCount {
			g.shards.sources[index].values = nil
			g.shards.derived[index].values = nil
		}
	}
	g.shards = nil
	g.mu.Unlock()
}

func incrementalPublicationSourceShardIndex(location string) uint64 {
	return maphash.String(incrementalPublicationSnapshotShardSeed, location) &
		(incrementalPublicationSnapshotShardCount - 1)
}

func incrementalPublicationDerivedShardIndex(binding incrementalPublicationSnapshotBinding) uint64 {
	return maphash.Comparable(incrementalPublicationSnapshotShardSeed, binding) &
		(incrementalPublicationSnapshotShardCount - 1)
}

func (r *incrementalRenderSession) releasePublicationFrames() {
	if r == nil {
		return
	}
	r.publicationGeneration.revoke()
}

func incrementalPublicationSnapshotSourceInput(
	group, location, cell, key, rank string,
	encoded []byte,
) incremental.Input {
	return incremental.Input{
		Key: incremental.NewInputKey(incremental.EncodeTuple(
			"publication-snapshot", group, location, cell, key, rank,
		)),
		Revision: exactBytesRevision("publication-snapshot", encoded),
		Found:    true,
		Value:    encoded,
	}
}

func incrementalPublicationSnapshotBindingFromInput(input incremental.Input) incrementalPublicationSnapshotBinding {
	return incrementalPublicationSnapshotBinding{
		key: input.Key, revision: input.Revision, found: input.Found, encoded: string(input.Value),
	}
}

func newIncrementalPublicationSnapshot(
	generation *incrementalPublicationSnapshotGeneration,
	binding incrementalPublicationSnapshotBinding,
	value any,
	certificate *templating.IncrementalImmutableCertificate,
) *incrementalPublicationSnapshot {
	snapshot := &incrementalPublicationSnapshot{
		generation: generation, binding: binding, value: value, certificate: certificate,
	}
	snapshot.seal = snapshot
	proof := &incrementalPublicationSnapshotProof{
		generation: generation, binding: binding, value: value, certificate: certificate,
	}
	proof.seal = proof
	snapshot.proof = proof
	return snapshot
}

func (s *incrementalPublicationSnapshot) authenticateLocked(
	generation *incrementalPublicationSnapshotGeneration,
	binding incrementalPublicationSnapshotBinding,
) error {
	if s == nil || s.seal != s || s.generation != generation || s.binding != binding ||
		s.certificate == nil || !s.certificate.Guards(s.value) || s.proof == nil ||
		s.proof.seal != s.proof || s.proof.generation != s.generation ||
		s.proof.binding != s.binding || s.proof.certificate != s.certificate ||
		!s.proof.certificate.Guards(s.value) ||
		!s.proof.certificate.Guards(s.proof.value) {
		return errors.New("incremental publication snapshot has invalid provenance")
	}
	return nil
}

func (g *incrementalPublicationSnapshotGeneration) capture(
	group string,
	owner incrementalGroupInstanceID,
	publicationIndex int,
	cell, key, rank string,
	detached *templating.IncrementalDetachedValue,
) ([]byte, *incrementalPublicationSnapshot, error) {
	if publicationIndex < 0 {
		return nil, nil, errors.New("incremental publication snapshot index is negative")
	}
	owned, err := templating.ConsumeIncrementalDetachedValue(detached)
	if err != nil {
		return nil, nil, err
	}
	normalized, encoded, err := normalizeIncrementalPublicationValue(owned)
	if err != nil {
		return nil, nil, err
	}
	if g == nil {
		return encoded, nil, nil
	}
	location := string(incrementalGroupLocationKey(owner, uint64(publicationIndex)))
	binding := incrementalPublicationSnapshotBindingFromInput(
		incrementalPublicationSnapshotSourceInput(group, location, cell, key, rank, encoded),
	)
	certificate := templating.CertifyIncrementalImmutableInputs(normalized)
	if certificate == nil || !certificate.Guards(normalized) {
		return nil, nil, errors.New("incremental publication snapshot has no immutable certificate")
	}
	candidate := newIncrementalPublicationSnapshot(g, binding, normalized, certificate)

	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.validLocked() {
		return nil, nil, errors.New("incremental publication snapshot generation is unavailable")
	}
	shard := &g.shards.sources[incrementalPublicationSourceShardIndex(location)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if shard.values == nil {
		return nil, nil, errors.New("incremental publication snapshot generation is unavailable")
	}
	if existing, exists := shard.values[location]; exists {
		if err := existing.authenticateLocked(g, binding); err != nil {
			return nil, nil, err
		}
		return encoded, existing, nil
	}
	shard.values[location] = candidate
	g.liveSources.Add(1)
	return encoded, candidate, nil
}

func (g *incrementalPublicationSnapshotGeneration) authenticateSource(
	location string,
	expected *incrementalPublicationSnapshot,
	binding incrementalPublicationSnapshotBinding,
) error {
	if g == nil {
		return errors.New("incremental publication snapshot generation is unavailable")
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.validLocked() {
		return errors.New("incremental publication snapshot generation is unavailable")
	}
	shard := &g.shards.sources[incrementalPublicationSourceShardIndex(location)]
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	if shard.values == nil {
		return errors.New("incremental publication snapshot generation is unavailable")
	}
	stored, exists := shard.values[location]
	if !exists || stored != expected {
		return errors.New("incremental publication snapshot does not match its source")
	}
	return stored.authenticateLocked(g, binding)
}

func authenticateIncrementalPublicationResultSnapshot(
	result *incrementalComponentResult,
	publication *incrementalPublishedValue,
	publicationIndex int,
) (bool, error) {
	if result == nil || publication == nil || publicationIndex < 0 {
		return false, errors.New("incremental publication snapshot result is unavailable")
	}
	if result.publicationGeneration == nil {
		if publication.snapshot != nil || result.publicationGroup != "" ||
			result.publicationOwner != (incrementalGroupInstanceID{}) {
			return false, errors.New("incremental publication snapshot has incomplete provenance")
		}
		return false, nil
	}
	if result.publicationGroup == "" || result.publicationOwner.component == "" ||
		publication.snapshot == nil {
		return false, errors.New("incremental publication snapshot has incomplete provenance")
	}
	location := string(incrementalGroupLocationKey(result.publicationOwner, uint64(publicationIndex)))
	binding := incrementalPublicationSnapshotBindingFromInput(
		incrementalPublicationSnapshotSourceInput(
			result.publicationGroup,
			location,
			publication.Cell,
			publication.Key,
			publication.Rank,
			publication.Value,
		),
	)
	if err := result.publicationGeneration.authenticateSource(location, publication.snapshot, binding); err != nil {
		return false, err
	}
	return true, nil
}

func validateIncrementalPublicationResultOwner(
	result *incrementalComponentResult,
	owner incrementalGroupInstanceID,
) error {
	if result != nil && result.publicationGeneration != nil && result.publicationOwner != owner {
		return errors.New("incremental publication snapshot does not match its component instance")
	}
	return nil
}

func validateIncrementalPublicationResultGroup(
	result *incrementalComponentResult,
	group string,
) error {
	if result != nil && result.publicationGeneration != nil && result.publicationGroup != group {
		return errors.New("incremental publication snapshot does not match its component group")
	}
	return nil
}

func (g *incrementalPublicationSnapshotGeneration) resolveSource(
	group string,
	winner *incrementalPublishedWinner,
) (*incrementalPublicationSnapshot, bool, error) {
	if g == nil || winner == nil {
		return nil, false, nil
	}
	location := string(winner.location)
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.validLocked() {
		return nil, false, errors.New("incremental publication snapshot generation is unavailable")
	}
	shard := &g.shards.sources[incrementalPublicationSourceShardIndex(location)]
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	if shard.values == nil {
		return nil, false, errors.New("incremental publication snapshot generation is unavailable")
	}
	snapshot, exists := shard.values[location]
	if !exists {
		return nil, false, nil
	}
	// The binding is only the authentication input, so a miss — every winner
	// on a query outside an actively publishing render — never pays for
	// encoding the winner's value into one.
	binding := incrementalPublicationSnapshotBindingFromInput(
		incrementalPublicationSnapshotSourceInput(
			group, location, winner.value.Cell, winner.value.Key, winner.value.Rank, winner.value.Value,
		),
	)
	if err := snapshot.authenticateLocked(g, binding); err != nil {
		return nil, false, err
	}
	return snapshot, true, nil
}

func (g *incrementalPublicationSnapshotGeneration) resolveDerived(
	binding incrementalPublicationSnapshotBinding,
) (*incrementalPublicationSnapshot, bool, error) {
	if g == nil {
		return nil, false, nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.validLocked() {
		return nil, false, errors.New("incremental publication snapshot generation is unavailable")
	}
	shard := &g.shards.derived[incrementalPublicationDerivedShardIndex(binding)]
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	if shard.values == nil {
		return nil, false, errors.New("incremental publication snapshot generation is unavailable")
	}
	snapshot, exists := shard.values[binding]
	if !exists {
		return nil, false, nil
	}
	if err := snapshot.authenticateLocked(g, binding); err != nil {
		return nil, false, err
	}
	return snapshot, true, nil
}

func (g *incrementalPublicationSnapshotGeneration) storeDerived(
	binding incrementalPublicationSnapshotBinding,
	value any,
	certificate *templating.IncrementalImmutableCertificate,
) (*incrementalPublicationSnapshot, error) {
	if g == nil || certificate == nil || !certificate.Guards(value) {
		return nil, errors.New("incremental publication snapshot has invalid derived provenance")
	}
	candidate := newIncrementalPublicationSnapshot(g, binding, value, certificate)
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.validLocked() {
		return nil, errors.New("incremental publication snapshot generation is unavailable")
	}
	shard := &g.shards.derived[incrementalPublicationDerivedShardIndex(binding)]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if shard.values == nil {
		return nil, errors.New("incremental publication snapshot generation is unavailable")
	}
	if existing, exists := shard.values[binding]; exists {
		if err := existing.authenticateLocked(g, binding); err != nil {
			return nil, err
		}
		return existing, nil
	}
	shard.values[binding] = candidate
	return candidate, nil
}

func (g *incrementalPublicationSnapshotGeneration) resolveSelector(
	group string,
	input incremental.Input,
	winner *incrementalPublishedWinner,
) (resolved any, certificate *templating.IncrementalImmutableCertificate, found bool, err error) {
	binding := incrementalPublicationSnapshotBindingFromInput(input)
	if cached, exists, err := g.resolveDerived(binding); err != nil {
		return nil, nil, false, err
	} else if exists {
		return cached.value, cached.certificate, true, nil
	}
	source, exists, err := g.resolveSource(group, winner)
	if err != nil || !exists {
		return nil, nil, false, err
	}
	stored, err := g.storeDerived(binding, source.value, source.certificate)
	if err != nil {
		return nil, nil, false, err
	}
	return stored.value, stored.certificate, true, nil
}

// resolveSelectorValues resolves a cell's values when at least one winner is
// live in this generation. It leaves the decoded winners in the index memo:
// only the memo-miss path in certifiedPublishedValues recorded them, so a
// cell that changed every render decoded all of its winners every render.
func (g *incrementalPublicationSnapshotGeneration) resolveSelectorValues(
	index *incrementalGroupIndex,
	group string,
	input incremental.Input,
	winners []incrementalPublishedWinner,
) (values []any, certificate *templating.IncrementalImmutableCertificate, found bool, err error) {
	binding := incrementalPublicationSnapshotBindingFromInput(input)
	if cached, exists, err := g.resolveDerived(binding); err != nil {
		return nil, nil, false, err
	} else if exists {
		values, ok := cached.value.([]any)
		if !ok {
			return nil, nil, false, errors.New("incremental publication values snapshot is not an array")
		}
		return values, cached.certificate, true, nil
	}

	values = make([]any, len(winners))
	decoded := make(map[string]incrementalDecodedWinner, len(winners))
	live := false
	cell := ""
	for position := range winners {
		winner := &winners[position]
		cell = winner.value.Cell
		source, exists, err := g.resolveSource(group, winner)
		if err != nil {
			return nil, nil, false, err
		}
		switch {
		case exists:
			values[position] = source.value
			live = true
		case winner.decoded != nil:
			values[position] = winner.decoded
		default:
			value, err := decodeResourceValue(winner.value.Value)
			if err != nil {
				return nil, nil, false, fmt.Errorf(
					"decoding incremental publication %q/%q: %w", winner.value.Cell, winner.value.Key, err,
				)
			}
			values[position] = value
		}
		decoded[winner.value.Key] = incrementalDecodedWinner{
			encoded: string(winner.value.Value), value: values[position],
		}
	}
	if !live {
		return nil, nil, false, nil
	}
	if index != nil {
		index.memo.storeDecodedWinners(cell, decoded)
	}
	certificate = templating.CertifyIncrementalImmutableInputs(values)
	stored, err := g.storeDerived(binding, values, certificate)
	if err != nil {
		return nil, nil, false, err
	}
	resolved, ok := stored.value.([]any)
	if !ok {
		return nil, nil, false, errors.New("incremental publication values snapshot is not an array")
	}
	return resolved, stored.certificate, true, nil
}

func normalizeIncrementalPublicationValue(value any) (normalized any, encoded []byte, err error) {
	normalized, supported, err := normalizePlainIncrementalPublicationValue(
		value,
		make(map[resourceCodecVisit]struct{}),
		0,
	)
	if err != nil {
		if _, encodeErr := encodeResourceValue(value); encodeErr != nil {
			return nil, nil, encodeErr
		}
		return nil, nil, err
	}
	if supported {
		encoded, err := encodeResourceValue(normalized)
		if err != nil {
			return nil, nil, err
		}
		return normalized, encoded, nil
	}
	_, err = encodeResourceValue(value)
	if err != nil {
		return nil, nil, err
	}
	return nil, nil, errors.New("incremental publication value has no canonical normalization")
}

func normalizePlainIncrementalPublicationValue(
	value any,
	active map[resourceCodecVisit]struct{},
	depth int,
) (normalized any, supported bool, err error) {
	if depth > resourceValueMaxDepth {
		return nil, true, errors.New("resource value exceeds the maximum depth")
	}
	switch typed := value.(type) {
	case nil, bool:
		return typed, true, nil
	case string:
		if !utf8.ValidString(typed) {
			return nil, true, errors.New("resource value contains an invalid UTF-8 string")
		}
		return typed, true, nil
	case int:
		return int64(typed), true, nil
	case int8:
		return int64(typed), true, nil
	case int16:
		return int64(typed), true, nil
	case int32:
		return int64(typed), true, nil
	case int64:
		return typed, true, nil
	case uint:
		return normalizeIncrementalPublicationUint(uint64(typed)), true, nil
	case uint8:
		return int64(typed), true, nil
	case uint16:
		return int64(typed), true, nil
	case uint32:
		return int64(typed), true, nil
	case uint64:
		return normalizeIncrementalPublicationUint(typed), true, nil
	case float32:
		normalized, err := normalizeIncrementalPublicationFloat(float64(typed), 32)
		return normalized, true, err
	case float64:
		normalized, err := normalizeIncrementalPublicationFloat(typed, 64)
		return normalized, true, err
	case map[string]any:
		return normalizePlainIncrementalPublicationMap(typed, active, depth)
	case []any:
		return normalizePlainIncrementalPublicationList(typed, active, depth)
	default:
		return nil, false, nil
	}
}

func normalizeIncrementalPublicationFloat(value float64, bits int) (any, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf("resource value contains a non-finite float%d", bits)
	}
	format := byte('f')
	absolute := math.Abs(value)
	if absolute != 0 && (bits == 64 && (absolute < 1e-6 || absolute >= 1e21) ||
		bits == 32 && (float32(absolute) < 1e-6 || float32(absolute) >= 1e21)) {
		format = 'e'
	}
	var storage [32]byte
	encoded := strconv.AppendFloat(storage[:0], value, format, -1, bits)
	if format == 'e' {
		length := len(encoded)
		if length >= 4 && encoded[length-4] == 'e' && encoded[length-3] == '-' && encoded[length-2] == '0' {
			encoded[length-2] = encoded[length-1]
			encoded = encoded[:length-1]
		}
	}
	return normalizeDecodedResourceNumber(json.Number(encoded))
}

func normalizeIncrementalPublicationUint(value uint64) any {
	if value <= math.MaxInt64 {
		return int64(value)
	}
	return value
}

func normalizePlainIncrementalPublicationMap(
	value map[string]any,
	active map[resourceCodecVisit]struct{},
	depth int,
) (normalized any, supported bool, err error) {
	if value == nil {
		return nil, true, nil
	}
	visit, err := beginResourceCodecVisit(value, active)
	if err != nil {
		return nil, true, err
	}
	defer delete(active, visit)
	for key, item := range value {
		if !utf8.ValidString(key) {
			return nil, true, errors.New("resource value contains an invalid UTF-8 map key")
		}
		normalized, supported, err := normalizePlainIncrementalPublicationValue(item, active, depth+1)
		if err != nil {
			return nil, true, fmt.Errorf("resource map key %q: %w", key, err)
		}
		if !supported {
			return nil, false, nil
		}
		value[key] = normalized
	}
	return value, true, nil
}

func normalizePlainIncrementalPublicationList(
	value []any,
	active map[resourceCodecVisit]struct{},
	depth int,
) (normalized any, supported bool, err error) {
	if value == nil {
		return nil, true, nil
	}
	visit, err := beginResourceCodecVisit(value, active)
	if err != nil {
		return nil, true, err
	}
	defer delete(active, visit)
	for index, item := range value {
		normalized, supported, err := normalizePlainIncrementalPublicationValue(item, active, depth+1)
		if err != nil {
			return nil, true, fmt.Errorf("resource list index %d: %w", index, err)
		}
		if !supported {
			return nil, false, nil
		}
		value[index] = normalized
	}
	return value, true, nil
}

func observeExactIncrementalPublicationInput(reader incremental.Reader, expected incremental.Input) error {
	if reader == nil {
		return errors.New("incremental publication has no dependency reader")
	}
	if observer, ok := reader.(incremental.ExactImmutableInputObserver); ok {
		return observer.ObserveExactImmutableInput(incremental.ImmutableInput{
			Key: expected.Key, Revision: expected.Revision, Found: expected.Found, Value: string(expected.Value),
		})
	}
	if observer, ok := reader.(incremental.ExactInputValueObserver); ok {
		observed := expected
		observed.Value = bytes.Clone(expected.Value)
		return observer.ObserveExactInputValue(observed)
	}
	observed, err := exactOwnedIncrementalInput(reader, expected.Key)
	if err != nil {
		return err
	}
	if observed.Key != expected.Key || observed.Revision != expected.Revision ||
		observed.Found != expected.Found || !bytes.Equal(observed.Value, expected.Value) {
		return incremental.ErrRevisionConflict
	}
	return nil
}

func (r *incrementalRenderSession) publicationInput(
	reader incremental.Reader,
	key incremental.InputKey,
) (value any, certificate *templating.IncrementalImmutableCertificate, found bool, err error) {
	if r.publicationGeneration != nil {
		if err := r.publicationGeneration.authenticateAuthority(r.publicationAuthority); err != nil {
			return nil, nil, false, err
		}
	}
	if identity, ok := parseIncrementalSelectorInputKey(key); ok {
		return r.selectorPublicationInput(reader, identity)
	}
	identity, ok := parseIncrementalSelectorValuesInputKey(key)
	if !ok {
		return nil, nil, false, errors.New("incremental publication input has an invalid identity")
	}
	return r.selectorValuesPublicationInput(reader, identity)
}

func (r *incrementalRenderSession) selectorPublicationInput(
	reader incremental.Reader,
	identity incrementalSelectorIdentity,
) (value any, certificate *templating.IncrementalImmutableCertificate, found bool, err error) {
	index := r.groupIndexes[identity.group]
	expected, winner, err := incrementalSelectorInputWithWinner(
		index, identity.group, identity.cell, identity.key,
	)
	if err != nil {
		return nil, nil, false, err
	}
	if err := observeExactIncrementalPublicationInput(reader, expected); err != nil {
		return nil, nil, false, err
	}
	if !expected.Found {
		return nil, nil, false, nil
	}
	if r.publicationGeneration != nil {
		value, certificate, resolved, err := r.publicationGeneration.resolveSelector(
			identity.group, expected, winner,
		)
		if err != nil || resolved {
			return value, certificate, resolved, err
		}
	}
	certified, err := r.certifyDecodedValueIdentity(string(expected.Value), expected.Value, nil)
	if err != nil {
		return nil, nil, false, err
	}
	return certified.value, certified.certificate, true, nil
}

func (r *incrementalRenderSession) selectorValuesPublicationInput(
	reader incremental.Reader,
	identity incrementalSelectorIdentity,
) (values any, certificate *templating.IncrementalImmutableCertificate, found bool, err error) {
	index := r.groupIndexes[identity.group]
	expected, winners, err := incrementalSelectorValuesInputWithWinners(index, identity.group, identity.cell)
	if err != nil {
		return nil, nil, false, err
	}
	if err := observeExactIncrementalPublicationInput(reader, expected); err != nil {
		return nil, nil, false, err
	}
	if !expected.Found {
		return nil, nil, false, nil
	}
	if r.publicationGeneration != nil {
		values, certificate, resolved, err := r.publicationGeneration.resolveSelectorValues(
			index, identity.group, expected, winners,
		)
		if err != nil || resolved {
			return values, certificate, resolved, err
		}
	}
	certified, err := r.certifyDecodedValueIdentity(string(expected.Value), expected.Value, nil)
	if err != nil {
		return nil, nil, false, err
	}
	if _, ok := certified.value.([]any); !ok {
		return nil, nil, false, fmt.Errorf(
			"incremental publication values %q/%q must be an array, got %T",
			identity.group,
			identity.cell,
			certified.value,
		)
	}
	return certified.value, certified.certificate, true, nil
}

func (r *incrementalRenderSession) certifiedPublicationValues(
	index *incrementalGroupIndex,
	group, cell string,
) ([]any, *templating.IncrementalImmutableCertificate, error) {
	if r.publicationGeneration != nil {
		if err := r.publicationGeneration.authenticateAuthority(r.publicationAuthority); err != nil {
			return nil, nil, err
		}
	}
	// Without live sources the generation resolves nothing: derived entries
	// are only stored on live hits, so the whole winner walk is a guaranteed
	// miss and the memoized index values are the answer.
	if !r.publicationGeneration.hasLiveSources() {
		return index.certifiedPublishedValues(cell)
	}
	input, winners, err := incrementalSelectorValuesInputWithWinners(index, group, cell)
	if err != nil {
		return nil, nil, err
	}
	values, certificate, resolved, err := r.publicationGeneration.resolveSelectorValues(
		index, group, input, winners,
	)
	if err != nil || resolved {
		return values, certificate, err
	}
	return index.certifiedPublishedValues(cell)
}

func (r *coldIncrementalRenderer) certifiedPublicationValues(
	index *incrementalGroupIndex,
	group, cell string,
) ([]any, *templating.IncrementalImmutableCertificate, error) {
	if r.publicationGeneration != nil {
		if err := r.publicationGeneration.authenticateAuthority(r.publicationAuthority); err != nil {
			return nil, nil, err
		}
	}
	// Without live sources the generation resolves nothing: derived entries
	// are only stored on live hits, so the whole winner walk is a guaranteed
	// miss and the memoized index values are the answer.
	if !r.publicationGeneration.hasLiveSources() {
		return index.certifiedPublishedValues(cell)
	}
	input, winners, err := incrementalSelectorValuesInputWithWinners(index, group, cell)
	if err != nil {
		return nil, nil, err
	}
	values, certificate, resolved, err := r.publicationGeneration.resolveSelectorValues(
		index, group, input, winners,
	)
	if err != nil || resolved {
		return values, certificate, err
	}
	return index.certifiedPublishedValues(cell)
}
