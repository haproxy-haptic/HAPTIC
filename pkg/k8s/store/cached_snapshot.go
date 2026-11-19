package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/indexer"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

type cachedReadRoot struct {
	source           uint64
	sequence         uint64
	listVersion      uint64
	numKeys          int
	exactUnsupported bool
	refs             *iradix.Tree[[]resourceRef]
	locations        *iradix.Tree[resourceRef]
	keyVersions      *iradix.Tree[uint64]
	identityVersions *iradix.Tree[uint64]
	warm             *iradix.Tree[cachedSnapshotValue]
}

func newCachedReadRoot(numKeys int, revisions *revisionState) *cachedReadRoot {
	return &cachedReadRoot{
		source:           revisions.source,
		sequence:         revisions.sequence,
		listVersion:      revisions.listVersion,
		numKeys:          numKeys,
		exactUnsupported: revisions.exactUnsupported,
		refs:             iradix.New[[]resourceRef](),
		locations:        iradix.New[resourceRef](),
		keyVersions:      iradix.New[uint64](),
		identityVersions: iradix.New[uint64](),
		warm:             iradix.New[cachedSnapshotValue](),
	}
}

func (s *CachedStore) publishCachedReadRootLocked(
	dataKeys []string,
	identities []resourceIdentity,
	keySets ...[]string,
) {
	s.readRootMu.Lock()
	defer s.readRootMu.Unlock()

	current := s.readRoot.Load()
	if current == nil {
		current = newCachedReadRoot(s.numKeys, &s.revisions)
	}

	refs := s.updatedCachedRefsRoot(current, dataKeys)
	locations, identityVersions := s.updatedCachedIdentityRoots(current, identities)
	keyVersions := s.revisions.updatedKeyVersionRoot(current.keyVersions, keySets)

	s.readRoot.Store(&cachedReadRoot{
		source:           s.revisions.source,
		sequence:         s.revisions.sequence,
		listVersion:      s.revisions.listVersion,
		numKeys:          s.numKeys,
		exactUnsupported: s.revisions.exactUnsupported,
		refs:             refs,
		locations:        locations,
		keyVersions:      keyVersions,
		identityVersions: identityVersions,
		warm:             current.warm,
	})
}

func (s *CachedStore) publishCachedWarmValue(cacheKey string, value cachedSnapshotValue) {
	s.readRootMu.Lock()
	defer s.readRootMu.Unlock()

	current := s.readRoot.Load()
	if current == nil {
		return
	}
	warm := current.warm.Txn()
	warm.Insert([]byte(cacheKey), value)
	updated := *current
	updated.warm = warm.Commit()
	s.readRoot.Store(&updated)
}

func (s *CachedStore) removeCachedWarmValue(cacheKey string, evicted *cacheEntry) {
	s.readRootMu.Lock()
	defer s.readRootMu.Unlock()

	current := s.readRoot.Load()
	if current == nil {
		return
	}
	value, found := current.warm.Get([]byte(cacheKey))
	if !found || evicted == nil || value.generation != evicted.generation ||
		value.resourceVersion != evicted.resourceVersion || !value.expiresAt.Equal(evicted.expiresAt) {
		return
	}
	warm := current.warm.Txn()
	warm.Delete([]byte(cacheKey))
	updated := *current
	updated.warm = warm.Commit()
	s.readRoot.Store(&updated)
}

func (s *CachedStore) updatedCachedRefsRoot(current *cachedReadRoot, dataKeys []string) *iradix.Tree[[]resourceRef] {
	refsTxn := current.refs.Txn()
	seenData := make(map[string]struct{}, len(dataKeys))
	for _, key := range dataKeys {
		if _, seen := seenData[key]; seen {
			continue
		}
		seenData[key] = struct{}{}
		refs, exists := s.refs[key]
		if !exists {
			refsTxn.Delete([]byte(key))
			continue
		}
		refsTxn.Insert([]byte(key), cloneResourceRefs(refs))
	}
	return refsTxn.Commit()
}

func (s *CachedStore) updatedCachedIdentityRoots(
	current *cachedReadRoot,
	identities []resourceIdentity,
) (locations *iradix.Tree[resourceRef], identityVersions *iradix.Tree[uint64]) {
	locationTxn := current.locations.Txn()
	identityVersionTxn := current.identityVersions.Txn()
	seenIdentities := make(map[resourceIdentity]struct{}, len(identities))
	for _, identity := range identities {
		if _, seen := seenIdentities[identity]; seen {
			continue
		}
		seenIdentities[identity] = struct{}{}
		key := []byte(resourceCacheKey(identity.namespace, identity.name))
		if ref, found := s.referenceForIdentityLocked(identity); found {
			locationTxn.Insert(key, cloneResourceRef(&ref))
		} else {
			locationTxn.Delete(key)
		}
		if version, exists := s.revisions.identityVersions[identity]; exists {
			identityVersionTxn.Insert(key, version)
		} else {
			identityVersionTxn.Delete(key)
		}
	}
	return locationTxn.Commit(), identityVersionTxn.Commit()
}

func (s *CachedStore) referenceForIdentityLocked(identity resourceIdentity) (resourceRef, bool) {
	key, found := s.locations[identity]
	if !found {
		return resourceRef{}, false
	}
	for _, ref := range s.refs[key] {
		if ref.namespace == identity.namespace && ref.name == identity.name {
			return ref, true
		}
	}
	return resourceRef{}, false
}

func cloneResourceRefs(refs []resourceRef) []resourceRef {
	cloned := make([]resourceRef, len(refs))
	for index := range refs {
		cloned[index] = cloneResourceRef(&refs[index])
	}
	return cloned
}

func cloneResourceRef(ref *resourceRef) resourceRef {
	cloned := *ref
	cloned.indexKeys = cloneStrings(ref.indexKeys)
	return cloned
}

type cachedSnapshotValue struct {
	resource        any
	resourceVersion string
	generation      uint64
	expiresAt       time.Time
}

type cachedSnapshotLoad struct {
	done     chan struct{}
	resource any
	err      error
	retry    bool
}

type cachedReadSnapshot struct {
	root     *cachedReadRoot
	store    *CachedStore
	pinnedAt time.Time

	mu    sync.Mutex
	loads map[string]*cachedSnapshotLoad
}

func (s *CachedStore) Pin() (stores.ReadSnapshot, error) {
	s.mu.RLock()
	root := s.readRoot.Load()
	if root == nil || root.exactUnsupported {
		s.mu.RUnlock()
		return nil, stores.ErrSnapshotUnsupported
	}
	pinnedAt := time.Now()
	s.mu.RUnlock()
	return &cachedReadSnapshot{
		root:     root,
		store:    s,
		pinnedAt: pinnedAt,
		loads:    map[string]*cachedSnapshotLoad{},
	}, nil
}

func (s *cachedReadSnapshot) RevisionSource() stores.RevisionSource {
	return stores.RevisionSource(s.root.source)
}

func (s *cachedReadSnapshot) IdentityOrderSource() stores.RevisionSource {
	return s.RevisionSource()
}

func (s *cachedReadSnapshot) Sequence() uint64 {
	return s.root.sequence
}

func (s *cachedReadSnapshot) ListRevision() stores.Revision {
	return revisionToken(s.root.source, "list", "", s.root.listVersion)
}

func (s *cachedReadSnapshot) GetRevision(keys ...string) stores.Revision {
	if len(keys) == 0 || len(keys) > s.root.numKeys {
		return ""
	}
	encoded := indexer.EncodeKey(keys)
	version, _ := s.root.keyVersions.Get([]byte(encoded))
	return revisionToken(s.root.source, "get", encoded, version)
}

func (s *cachedReadSnapshot) IdentityRevision(namespace, name string) stores.Revision {
	if name == "" {
		return ""
	}
	encoded := resourceCacheKey(namespace, name)
	version, _ := s.root.identityVersions.Get([]byte(encoded))
	return revisionToken(s.root.source, "identity", encoded, version)
}

func (s *cachedReadSnapshot) Get(keys ...string) ([]any, error) {
	return s.GetContext(context.Background(), keys...)
}

func (s *cachedReadSnapshot) GetContext(ctx context.Context, keys ...string) ([]any, error) {
	items, err := s.getImmutable(ctx, keys...)
	if err != nil {
		return nil, err
	}
	return cloneMemorySnapshotItems(items)
}

func (s *cachedReadSnapshot) getImmutable(ctx context.Context, keys ...string) ([]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, &StoreError{
			Operation: opGet,
			Keys:      keys,
			Cause:     errors.New("at least one key required"),
		}
	}
	if len(keys) > s.root.numKeys {
		return nil, &StoreError{
			Operation: opGet,
			Keys:      keys,
			Cause:     fmt.Errorf("too many keys: got %d, expected %d", len(keys), s.root.numKeys),
		}
	}
	return s.readRefsImmutable(ctx, s.refsForKeys(keys))
}

func (s *cachedReadSnapshot) List() ([]any, error) {
	return s.ListContext(context.Background())
}

func (s *cachedReadSnapshot) ListContext(ctx context.Context) ([]any, error) {
	items, err := s.listImmutable(ctx)
	if err != nil {
		return nil, err
	}
	return cloneMemorySnapshotItems(items)
}

func (s *cachedReadSnapshot) listImmutable(ctx context.Context) ([]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var refs []resourceRef
	iterator := s.root.refs.Root().Iterator()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, bucket, found := iterator.Next()
		if !found {
			break
		}
		refs = append(refs, bucket...)
	}
	sortResourceRefs(refs)
	return s.readRefsImmutable(ctx, refs)
}

func (s *cachedReadSnapshot) ListWarm() ([]any, error) {
	refs := make([]resourceRef, 0, s.root.warm.Len())
	iterator := s.root.warm.Root().Iterator()
	for {
		cacheKey, value, found := iterator.Next()
		if !found {
			break
		}
		ref, live := s.root.locations.Get(cacheKey)
		if !live || !s.pinnedAt.Before(value.expiresAt) ||
			value.generation != ref.generation || value.resourceVersion != ref.resourceVersion {
			continue
		}
		refs = append(refs, ref)
	}
	sortResourceRefs(refs)
	result := make([]any, 0, len(refs))
	for index := range refs {
		value, _ := s.root.warm.Get([]byte(resourceCacheKey(refs[index].namespace, refs[index].name)))
		detached, err := cloneMemorySnapshotValue(value.resource)
		if err != nil {
			return nil, err
		}
		result = append(result, detached)
	}
	return result, nil
}

func (s *cachedReadSnapshot) GetIdentity(namespace, name string) (item any, found bool, err error) {
	return s.GetIdentityContext(context.Background(), namespace, name)
}

func (s *cachedReadSnapshot) GetIdentityContext(
	ctx context.Context,
	namespace, name string,
) (item any, found bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if name == "" {
		return nil, false, errResourceNameRequired
	}
	ref, found := s.root.locations.Get([]byte(resourceCacheKey(namespace, name)))
	if !found {
		return nil, false, nil
	}
	item, err = s.readRef(ctx, &ref)
	if err != nil {
		return nil, false, err
	}
	return item, true, nil
}

func (s *cachedReadSnapshot) refsForKeys(keys []string) []resourceRef {
	encoded := indexer.EncodeKey(keys)
	if len(keys) == s.root.numKeys {
		refs, found := s.root.refs.Get([]byte(encoded))
		if !found {
			return nil
		}
		return cloneResourceRefs(refs)
	}
	var refs []resourceRef
	iterator := s.root.refs.Root().Iterator()
	iterator.SeekPrefix([]byte(encoded))
	for {
		_, bucket, found := iterator.Next()
		if !found {
			break
		}
		refs = append(refs, bucket...)
	}
	sortResourceRefs(refs)
	return cloneResourceRefs(refs)
}

func (s *cachedReadSnapshot) readRefsImmutable(ctx context.Context, refs []resourceRef) ([]any, error) {
	result := make([]any, 0, len(refs))
	for index := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resource, err := s.readRefImmutable(ctx, &refs[index])
		if err != nil {
			return nil, err
		}
		result = append(result, resource)
	}
	return result, nil
}

func (s *cachedReadSnapshot) readRef(ctx context.Context, ref *resourceRef) (any, error) {
	resource, err := s.readRefImmutable(ctx, ref)
	if err != nil {
		return nil, err
	}
	return cloneMemorySnapshotValue(resource)
}

func (s *cachedReadSnapshot) readRefImmutable(ctx context.Context, ref *resourceRef) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cacheKey := resourceCacheKey(ref.namespace, ref.name)
	loadKey := fmt.Sprintf("%s\x00%d", cacheKey, ref.generation)
	for {
		s.mu.Lock()
		load, found := s.loads[loadKey]
		if !found {
			load = &cachedSnapshotLoad{done: make(chan struct{})}
			s.loads[loadKey] = load
			s.mu.Unlock()

			resource, err := s.loadRef(ctx, ref)
			retry := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
			s.mu.Lock()
			load.resource = resource
			load.err = err
			load.retry = retry
			if retry {
				delete(s.loads, loadKey)
			}
			close(load.done)
			s.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return resource, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-load.done:
		}
		if load.retry {
			continue
		}
		if load.err != nil {
			return nil, load.err
		}
		return load.resource, nil
	}
}

func (s *cachedReadSnapshot) loadRef(ctx context.Context, ref *resourceRef) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cacheKey := resourceCacheKey(ref.namespace, ref.name)
	if warm, found := s.root.warm.Get([]byte(cacheKey)); found && s.pinnedAt.Before(warm.expiresAt) &&
		warm.generation == ref.generation && warm.resourceVersion == ref.resourceVersion {
		return warm.resource, nil
	}
	if ref.resourceVersion == "" {
		return nil, fmt.Errorf("resource %s/%s has no resourceVersion: %w",
			ref.namespace, ref.name, stores.ErrSnapshotUnsupported)
	}
	if !s.store.generationMatches(ref) {
		return nil, snapshotChangedError(ref, "its informer generation changed before the API read")
	}
	resource, resourceVersion, err := s.store.fetchProcessedResource(ctx, ref)
	if err != nil {
		if isNotFound(err) {
			return nil, snapshotChangedError(ref, "the pinned object is absent from the API")
		}
		return nil, err
	}
	if resourceVersion == "" || resourceVersion != ref.resourceVersion {
		return nil, snapshotChangedError(ref, fmt.Sprintf(
			"the API returned resourceVersion %q instead of %q", resourceVersion, ref.resourceVersion))
	}
	identity, identified := identifyResource(resource)
	if !identified || identity.namespace != ref.namespace || identity.name != ref.name {
		return nil, snapshotChangedError(ref, "the API returned a different identity")
	}
	keys, err := s.store.indexer.ExtractKeys(resource)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(keys, ref.indexKeys) {
		return nil, snapshotChangedError(ref, "the API returned different index keys")
	}
	if !s.store.generationMatches(ref) {
		return nil, snapshotChangedError(ref, "its informer generation changed during the API read")
	}
	resource, err = s.store.cacheFetchedResource(ref, resource, resourceVersion)
	if err != nil {
		return nil, err
	}
	return resource, nil
}

func (s *CachedStore) generationMatches(ref *resourceRef) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	generation, found := s.refGenerations[resourceCacheKey(ref.namespace, ref.name)]
	return found && generation == ref.generation
}

func snapshotChangedError(ref *resourceRef, reason string) error {
	return fmt.Errorf("resource %s/%s no longer matches its pinned snapshot: %s: %w",
		ref.namespace, ref.name, reason, stores.ErrSnapshotChanged)
}

func (s *CachedStore) RevisionSource() stores.RevisionSource {
	root := s.readRoot.Load()
	if root == nil || root.exactUnsupported {
		return 0
	}
	return stores.RevisionSource(root.source)
}

func (s *CachedStore) ListRevision() stores.Revision {
	root := s.readRoot.Load()
	if root == nil || root.exactUnsupported {
		return ""
	}
	return revisionToken(root.source, "list", "", root.listVersion)
}

func (s *CachedStore) GetRevision(keys ...string) stores.Revision {
	root := s.readRoot.Load()
	if root == nil || root.exactUnsupported || len(keys) == 0 || len(keys) > root.numKeys {
		return ""
	}
	encoded := indexer.EncodeKey(keys)
	version, _ := root.keyVersions.Get([]byte(encoded))
	return revisionToken(root.source, "get", encoded, version)
}

func (s *CachedStore) IdentityRevision(namespace, name string) stores.Revision {
	root := s.readRoot.Load()
	if root == nil || root.exactUnsupported || name == "" {
		return ""
	}
	encoded := resourceCacheKey(namespace, name)
	version, _ := root.identityVersions.Get([]byte(encoded))
	return revisionToken(root.source, "identity", encoded, version)
}

func (s *CachedStore) ListSnapshot() (items []any, sequence uint64, err error) {
	snapshot, err := s.Pin()
	if err != nil {
		return nil, 0, err
	}
	items, err = snapshot.List()
	return items, snapshot.Sequence(), err
}

func (s *CachedStore) GetSnapshot(
	keys ...string,
) (items []any, revision stores.Revision, sequence uint64, err error) {
	snapshot, err := s.Pin()
	if err != nil {
		return nil, "", 0, err
	}
	items, err = snapshot.Get(keys...)
	return items, snapshot.GetRevision(keys...), snapshot.Sequence(), err
}

func (s *CachedStore) IdentitySnapshot(
	namespace, name string,
) (item any, found bool, revision stores.Revision, sequence uint64, err error) {
	snapshot, err := s.Pin()
	if err != nil {
		return nil, false, "", 0, err
	}
	item, found, err = snapshot.GetIdentity(namespace, name)
	return item, found, snapshot.IdentityRevision(namespace, name), snapshot.Sequence(), err
}

func (s *CachedStore) ChangesSince(sequence uint64) (uint64, []stores.RevisionChange, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revisions.changesSince(sequence)
}

func (s *CachedStore) ExactRevisionJournalSource() stores.RevisionSource {
	return s.RevisionSource()
}

var (
	_ stores.Revisioned                  = (*CachedStore)(nil)
	_ stores.RevisionJournal             = (*CachedStore)(nil)
	_ stores.ExactRevisionJournal        = (*CachedStore)(nil)
	_ stores.SnapshotReader              = (*CachedStore)(nil)
	_ stores.SnapshotProvider            = (*CachedStore)(nil)
	_ stores.ReadSnapshot                = (*cachedReadSnapshot)(nil)
	_ stores.ContextReadSnapshot         = (*cachedReadSnapshot)(nil)
	_ stores.IdentityOrderedReadSnapshot = (*cachedReadSnapshot)(nil)
)
