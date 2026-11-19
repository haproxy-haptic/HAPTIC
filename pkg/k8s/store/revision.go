package store

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"sync/atomic"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/indexer"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const defaultRevisionJournalCapacity = 4096

var nextRevisionSource atomic.Uint64

type revisionState struct {
	source            uint64
	sequence          uint64
	listVersion       uint64
	keyVersions       map[string]uint64
	keyCounts         map[string]uint64
	identityVersions  map[resourceIdentity]uint64
	identityKeys      map[resourceIdentity][]string
	journal           []stores.RevisionChange
	journalStart      int
	journalCapacity   int
	incompleteThrough uint64
	exactUnsupported  bool
}

func newRevisionState(journalCapacity int) revisionState {
	return revisionState{
		source:           allocateRevisionSource(&nextRevisionSource),
		keyVersions:      make(map[string]uint64),
		keyCounts:        make(map[string]uint64),
		identityVersions: make(map[resourceIdentity]uint64),
		identityKeys:     make(map[resourceIdentity][]string),
		journalCapacity:  max(journalCapacity, 0),
	}
}

func allocateRevisionSource(counter *atomic.Uint64) uint64 {
	for {
		current := counter.Load()
		if current == ^uint64(0) {
			panic("store revision source exhausted")
		}
		if counter.CompareAndSwap(current, current+1) {
			return current + 1
		}
	}
}

func (r *revisionState) listRevision() stores.Revision {
	return r.token("list", "", r.listVersion)
}

func (r *revisionState) getRevision(keys []string) stores.Revision {
	encoded := indexer.EncodeKey(keys)
	return r.token("get", encoded, r.keyVersions[encoded])
}

func (r *revisionState) identityRevision(identity resourceIdentity) stores.Revision {
	encoded := resourceCacheKey(identity.namespace, identity.name)
	return r.token("identity", encoded, r.identityVersions[identity])
}

func (r *revisionState) token(scope, target string, version uint64) stores.Revision {
	return revisionToken(r.source, scope, target, version)
}

func revisionToken(source uint64, scope, target string, version uint64) stores.Revision {
	return stores.Revision(indexer.EncodeKey([]string{
		strconv.FormatUint(source, 10), scope, target, strconv.FormatUint(version, 10),
	}))
}

func (r *revisionState) recordUpsert(identity resourceIdentity, identified bool, newKeys []string) {
	if !identified {
		r.recordUnknown(newKeys)
		return
	}

	oldKeys := cloneStrings(r.identityKeys[identity])
	sequence := r.nextSequence()
	r.updateKeyVersions(sequence, oldKeys, newKeys)
	r.identityVersions[identity] = sequence
	r.identityKeys[identity] = cloneStrings(newKeys)
	r.appendChange(&stores.RevisionChange{
		Sequence:  sequence,
		Namespace: identity.namespace,
		Name:      identity.name,
		OldKeys:   oldKeys,
		NewKeys:   cloneStrings(newKeys),
	})
}

func (r *revisionState) recordDelete(identity resourceIdentity) {
	oldKeys := cloneStrings(r.identityKeys[identity])
	sequence := r.nextSequence()
	r.updateKeyVersions(sequence, oldKeys, nil)
	delete(r.identityVersions, identity)
	delete(r.identityKeys, identity)
	r.appendChange(&stores.RevisionChange{
		Sequence:  sequence,
		Namespace: identity.namespace,
		Name:      identity.name,
		Deleted:   true,
		OldKeys:   oldKeys,
	})
}

func (r *revisionState) recordClear(resourceCount int) {
	identities := make([]resourceIdentity, 0, len(r.identityKeys))
	for identity := range r.identityKeys {
		identities = append(identities, identity)
	}
	slices.SortFunc(identities, func(a, b resourceIdentity) int {
		if compared := cmp.Compare(a.namespace, b.namespace); compared != 0 {
			return compared
		}
		return cmp.Compare(a.name, b.name)
	})
	for _, identity := range identities {
		r.recordDelete(identity)
	}
	if resourceCount > len(identities) {
		r.recordUnknown(nil)
	}
}

func (r *revisionState) recordUnknown(keys []string) {
	sequence := r.nextSequence()
	r.incompleteThrough = sequence
	r.exactUnsupported = true
	r.appendChange(&stores.RevisionChange{Sequence: sequence, NewKeys: cloneStrings(keys)})
}

func (r *revisionState) nextSequence() uint64 {
	if r.sequence == ^uint64(0) {
		panic("store revision sequence exhausted")
	}
	r.sequence++
	r.listVersion = r.sequence
	return r.sequence
}

func (r *revisionState) updateKeyVersions(sequence uint64, oldKeys, newKeys []string) {
	deltas := make(map[string]int8, len(oldKeys)+len(newKeys))
	for count := 1; count <= len(oldKeys); count++ {
		deltas[indexer.EncodeKey(oldKeys[:count])]--
	}
	for count := 1; count <= len(newKeys); count++ {
		deltas[indexer.EncodeKey(newKeys[:count])]++
	}
	for encoded, delta := range deltas {
		count := r.keyCounts[encoded]
		switch delta {
		case -1:
			if count == 0 {
				panic("store revision key count underflow")
			}
			if count == 1 {
				delete(r.keyCounts, encoded)
				delete(r.keyVersions, encoded)
				continue
			}
			r.keyCounts[encoded] = count - 1
		case 0:
			if count == 0 {
				panic("store revision key count is missing")
			}
		case 1:
			if count == ^uint64(0) {
				panic("store revision key count overflow")
			}
			r.keyCounts[encoded] = count + 1
		default:
			panic("store revision key count delta is invalid")
		}
		r.keyVersions[encoded] = sequence
	}
}

func (r *revisionState) updatedKeyVersionRoot(current *iradix.Tree[uint64], keySets [][]string) *iradix.Tree[uint64] {
	txn := current.Txn()
	seenPrefixes := map[string]struct{}{}
	for _, keys := range keySets {
		for count := 1; count <= len(keys); count++ {
			encoded := indexer.EncodeKey(keys[:count])
			if _, seen := seenPrefixes[encoded]; seen {
				continue
			}
			seenPrefixes[encoded] = struct{}{}
			if version, exists := r.keyVersions[encoded]; exists {
				txn.Insert([]byte(encoded), version)
			} else {
				txn.Delete([]byte(encoded))
			}
		}
	}
	return txn.Commit()
}

func (r *revisionState) appendChange(change *stores.RevisionChange) {
	if r.journalCapacity == 0 {
		return
	}
	entry := *change
	entry.OldKeys = cloneStrings(entry.OldKeys)
	entry.NewKeys = cloneStrings(entry.NewKeys)
	if len(r.journal) < r.journalCapacity {
		if len(r.journal) == cap(r.journal) {
			capacity := min(max(1, 2*len(r.journal)), r.journalCapacity)
			journal := make([]stores.RevisionChange, len(r.journal), capacity)
			copy(journal, r.journal)
			r.journal = journal
		}
		r.journal = append(r.journal, entry)
		return
	}
	r.journal[r.journalStart] = entry
	r.journalStart = (r.journalStart + 1) % r.journalCapacity
}

func (r *revisionState) changesSince(sequence uint64) (uint64, []stores.RevisionChange, bool) {
	current := r.sequence
	if sequence > current || sequence < r.incompleteThrough {
		return current, nil, false
	}
	if sequence == current {
		return current, nil, true
	}
	if len(r.journal) == 0 {
		return current, nil, false
	}
	oldest := r.journal[r.journalStart].Sequence
	if oldest == 0 || sequence < oldest-1 {
		return current, nil, false
	}

	span := sequence + 1 - oldest
	if span > math.MaxInt {
		return current, nil, false
	}
	start := int(span)
	if start >= len(r.journal) {
		return current, nil, false
	}
	changes := make([]stores.RevisionChange, 0, len(r.journal)-start)
	expected := sequence + 1
	for offset := start; offset < len(r.journal); offset++ {
		change := r.journal[(r.journalStart+offset)%r.journalCapacity]
		if change.Sequence != expected {
			return current, nil, false
		}
		change.OldKeys = cloneStrings(change.OldKeys)
		change.NewKeys = cloneStrings(change.NewKeys)
		changes = append(changes, change)
		expected++
	}
	return current, changes, true
}

func cloneStrings(values []string) []string {
	return slices.Clone(values)
}
