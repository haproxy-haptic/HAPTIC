package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/indexer"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/typegen"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"k8s.io/apimachinery/pkg/runtime"
)

type memoryReadRoot struct {
	source           uint64
	sequence         uint64
	listVersion      uint64
	numKeys          int
	exactUnsupported bool
	data             *iradix.Tree[memoryReadBucket]
	locations        *iradix.Tree[any]
	keyVersions      *iradix.Tree[uint64]
	identityVersions *iradix.Tree[uint64]
}

type memoryReadBucket struct {
	items        []any
	encodedItems [][]byte
	encoded      bool
}

type memoryProjectionItem struct {
	value   any
	encoded []byte
}

type memorySnapshotCloneVisit struct {
	kind    reflect.Kind
	typeOf  reflect.Type
	pointer uintptr
}

func newMemoryReadRoot(numKeys int, revisions *revisionState) *memoryReadRoot {
	return &memoryReadRoot{
		source:           revisions.source,
		sequence:         revisions.sequence,
		listVersion:      revisions.listVersion,
		numKeys:          numKeys,
		exactUnsupported: revisions.exactUnsupported,
		data:             iradix.New[memoryReadBucket](),
		locations:        iradix.New[any](),
		keyVersions:      iradix.New[uint64](),
		identityVersions: iradix.New[uint64](),
	}
}

// Pin returns the current immutable store root in constant time.
func (s *MemoryStore) Pin() (stores.ReadSnapshot, error) {
	root := s.readRoot.Load()
	if root == nil || root.exactUnsupported {
		return nil, stores.ErrSnapshotUnsupported
	}
	return &memoryReadSnapshot{root: root, journal: s}, nil
}

func (s *MemoryStore) publishReadRootLocked(
	dataKeys []string,
	identities []resourceIdentity,
	keySets ...[]string,
) {
	current := s.readRoot.Load()
	if current == nil {
		current = newMemoryReadRoot(s.numKeys, &s.revisions)
	}

	data := s.updatedMemoryDataRoot(current, dataKeys)
	locations, identityVersions := s.updatedMemoryIdentityRoots(current, identities)
	keyVersions := s.revisions.updatedKeyVersionRoot(current.keyVersions, keySets)

	s.readRoot.Store(&memoryReadRoot{
		source:           s.revisions.source,
		sequence:         s.revisions.sequence,
		listVersion:      s.revisions.listVersion,
		numKeys:          s.numKeys,
		exactUnsupported: s.revisions.exactUnsupported,
		data:             data,
		locations:        locations,
		keyVersions:      keyVersions,
		identityVersions: identityVersions,
	})
}

func (s *MemoryStore) updatedMemoryDataRoot(
	current *memoryReadRoot,
	dataKeys []string,
) *iradix.Tree[memoryReadBucket] {
	dataTxn := current.data.Txn()
	seenData := make(map[string]struct{}, len(dataKeys))
	for _, key := range dataKeys {
		if _, seen := seenData[key]; seen {
			continue
		}
		seenData[key] = struct{}{}
		items, exists := s.data[key]
		if !exists {
			dataTxn.Delete([]byte(key))
			continue
		}
		dataTxn.Insert([]byte(key), newMemoryReadBucket(items))
	}
	return dataTxn.Commit()
}

func newMemoryReadBucket(items []any) memoryReadBucket {
	bucket := memoryReadBucket{
		items:        slices.Clone(items),
		encodedItems: make([][]byte, len(items)),
		encoded:      true,
	}
	for index, item := range items {
		encoded, err := typegen.MarshalImmutableJSON(item)
		if err != nil {
			bucket.encodedItems = nil
			bucket.encoded = false
			return bucket
		}
		bucket.encodedItems[index] = encoded
	}
	return bucket
}

func (s *MemoryStore) updatedMemoryIdentityRoots(
	current *memoryReadRoot,
	identities []resourceIdentity,
) (locations *iradix.Tree[any], identityVersions *iradix.Tree[uint64]) {
	locationTxn := current.locations.Txn()
	identityVersionTxn := current.identityVersions.Txn()
	seenIdentities := make(map[resourceIdentity]struct{}, len(identities))
	for _, identity := range identities {
		if _, seen := seenIdentities[identity]; seen {
			continue
		}
		seenIdentities[identity] = struct{}{}
		key := []byte(resourceCacheKey(identity.namespace, identity.name))
		item, found := s.getIdentityLocked(identity.namespace, identity.name)
		if found {
			locationTxn.Insert(key, item)
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

type memoryReadSnapshot struct {
	root    *memoryReadRoot
	journal stores.RevisionJournal
}

func (s *memoryReadSnapshot) RevisionSource() stores.RevisionSource {
	return stores.RevisionSource(s.root.source)
}

func (s *memoryReadSnapshot) IdentityOrderSource() stores.RevisionSource {
	return s.RevisionSource()
}

func (s *memoryReadSnapshot) Sequence() uint64 {
	return s.root.sequence
}

func (s *memoryReadSnapshot) ListRevision() stores.Revision {
	return revisionToken(s.root.source, "list", "", s.root.listVersion)
}

func (s *memoryReadSnapshot) GetRevision(keys ...string) stores.Revision {
	if len(keys) == 0 || len(keys) > s.root.numKeys {
		return ""
	}
	encoded := indexer.EncodeKey(keys)
	version, _ := s.root.keyVersions.Get([]byte(encoded))
	return revisionToken(s.root.source, "get", encoded, version)
}

func (s *memoryReadSnapshot) IdentityRevision(namespace, name string) stores.Revision {
	if name == "" {
		return ""
	}
	encoded := resourceCacheKey(namespace, name)
	version, _ := s.root.identityVersions.Get([]byte(encoded))
	return revisionToken(s.root.source, "identity", encoded, version)
}

func (s *memoryReadSnapshot) Get(keys ...string) ([]any, error) {
	items, err := s.getImmutable(context.Background(), keys...)
	if err != nil {
		return nil, err
	}
	return cloneMemorySnapshotItems(items)
}

func (s *memoryReadSnapshot) getImmutable(ctx context.Context, keys ...string) ([]any, error) {
	items, _, _, err := s.getImmutableProjection(ctx, keys...)
	return items, err
}

func (s *memoryReadSnapshot) getImmutableProjection(
	ctx context.Context,
	keys ...string,
) (resources []any, encodedResources [][]byte, encodedReady bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	if len(keys) == 0 {
		return nil, nil, false, &StoreError{
			Operation: opGet,
			Keys:      keys,
			Cause:     errors.New("at least one key required"),
		}
	}
	if len(keys) > s.root.numKeys {
		return nil, nil, false, &StoreError{
			Operation: opGet,
			Keys:      keys,
			Cause:     fmt.Errorf("too many keys: got %d, expected %d", len(keys), s.root.numKeys),
		}
	}

	encoded := indexer.EncodeKey(keys)
	if len(keys) == s.root.numKeys {
		bucket, found := s.root.data.Get([]byte(encoded))
		if !found {
			return []any{}, [][]byte{}, true, nil
		}
		return bucket.items, bucket.encodedItems, bucket.encoded, nil
	}

	var projected []memoryProjectionItem
	allEncoded := true
	iterator := s.root.data.Root().Iterator()
	iterator.SeekPrefix([]byte(encoded))
	for {
		_, bucket, found := iterator.Next()
		if !found {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, false, err
		}
		allEncoded = allEncoded && bucket.encoded
		for index, item := range bucket.items {
			var encodedItem []byte
			if bucket.encoded {
				encodedItem = bucket.encodedItems[index]
			}
			projected = append(projected, memoryProjectionItem{value: item, encoded: encodedItem})
		}
	}
	slices.SortFunc(projected, func(left, right memoryProjectionItem) int {
		return compareByNamespaceName(left.value, right.value)
	})
	items, encodedItems, encodedReady := materializeMemoryProjection(projected, allEncoded)
	return items, encodedItems, encodedReady, nil
}

func (s *memoryReadSnapshot) List() ([]any, error) {
	items, err := s.listImmutable(context.Background())
	if err != nil {
		return nil, err
	}
	return cloneMemorySnapshotItems(items)
}

func (s *memoryReadSnapshot) listImmutable(ctx context.Context) ([]any, error) {
	items, _, _, err := s.listImmutableProjection(ctx)
	return items, err
}

func (s *memoryReadSnapshot) listImmutableProjection(
	ctx context.Context,
) (resources []any, encodedResources [][]byte, encodedReady bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	var projected []memoryProjectionItem
	allEncoded := true
	iterator := s.root.data.Root().Iterator()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, err
		}
		_, bucket, found := iterator.Next()
		if !found {
			break
		}
		allEncoded = allEncoded && bucket.encoded
		for index, item := range bucket.items {
			var encodedItem []byte
			if bucket.encoded {
				encodedItem = bucket.encodedItems[index]
			}
			projected = append(projected, memoryProjectionItem{value: item, encoded: encodedItem})
		}
	}
	slices.SortFunc(projected, func(left, right memoryProjectionItem) int {
		return compareByNamespaceName(left.value, right.value)
	})
	items, encodedItems, encodedReady := materializeMemoryProjection(projected, allEncoded)
	return items, encodedItems, encodedReady, nil
}

func materializeMemoryProjection(
	projected []memoryProjectionItem,
	encoded bool,
) (items []any, encodedItems [][]byte, encodedReady bool) {
	if projected == nil {
		return nil, nil, encoded
	}
	items = make([]any, len(projected))
	if encoded {
		encodedItems = make([][]byte, len(projected))
	}
	for index, item := range projected {
		items[index] = item.value
		if encoded {
			encodedItems[index] = item.encoded
		}
	}
	return items, encodedItems, encoded
}

func (s *memoryReadSnapshot) GetIdentity(namespace, name string) (item any, found bool, err error) {
	item, found = s.root.locations.Get([]byte(resourceCacheKey(namespace, name)))
	if !found {
		return nil, false, nil
	}
	item, err = cloneMemorySnapshotValue(item)
	if err != nil {
		return nil, false, err
	}
	return item, true, nil
}

func cloneMemorySnapshotItems(items []any) ([]any, error) {
	if items == nil {
		return nil, nil
	}
	cloned := make([]any, len(items))
	for index, item := range items {
		value, err := cloneMemorySnapshotValue(item)
		if err != nil {
			return nil, err
		}
		cloned[index] = value
	}
	return cloned, nil
}

func detachMemoryStoreReadItems(items []any) ([]any, error) {
	if items == nil {
		return nil, nil
	}
	detached := make([]any, len(items))
	for index, item := range items {
		value, err := detachMemoryStoreReadValue(item)
		if err != nil {
			return nil, err
		}
		detached[index] = value
	}
	return detached, nil
}

func detachMemoryStoreReadValue(value any) (any, error) {
	return cloneMemorySnapshotValue(value)
}

func ownMemorySnapshotResource(resource any) (any, error) {
	if immutable, ok := resource.(*ImmutableResource); ok {
		if immutable == nil || immutable.resource().Object == nil {
			return nil, fmt.Errorf("immutable resource is nil: %w", stores.ErrSnapshotUnsupported)
		}
		return immutable.resource().Object, nil
	}
	return cloneMemorySnapshotValue(resource)
}

func cloneMemorySnapshotValue(value any) (any, error) {
	return cloneMemorySnapshotValueActive(value, make(map[memorySnapshotCloneVisit]struct{}))
}

func cloneMemorySnapshotValueActive(
	value any,
	active map[memorySnapshotCloneVisit]struct{},
) (any, error) {
	if object, ok := value.(runtime.Object); ok {
		if isNilMemorySnapshotObject(object) {
			return nil, fmt.Errorf("resource value is nil: %w", stores.ErrSnapshotUnsupported)
		}
		copied := object.DeepCopyObject()
		if copied == nil {
			return nil, fmt.Errorf("resource copy is nil: %w", stores.ErrSnapshotUnsupported)
		}
		return copied, nil
	}
	switch typed := value.(type) {
	case nil, string, bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return typed, nil
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return value, nil
	case reflect.Map:
		return cloneMemorySnapshotMap(reflected, active)
	case reflect.Slice:
		return cloneMemorySnapshotSlice(reflected, active)
	default:
		return nil, fmt.Errorf("resource value type %T is not immutable: %w", value, stores.ErrSnapshotUnsupported)
	}
}

func cloneMemorySnapshotMap(
	value reflect.Value,
	active map[memorySnapshotCloneVisit]struct{},
) (any, error) {
	if value.IsNil() {
		return reflect.Zero(value.Type()).Interface(), nil
	}
	visit, err := beginMemorySnapshotCloneVisit(value, active)
	if err != nil {
		return nil, err
	}
	defer delete(active, visit)
	cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
	iterator := value.MapRange()
	for iterator.Next() {
		key := iterator.Key()
		if !memorySnapshotMapKeyIsImmutable(key.Type()) {
			return nil, fmt.Errorf("resource map key type %v is not immutable: %w", key.Type(), stores.ErrSnapshotUnsupported)
		}
		item, err := cloneMemorySnapshotValueActive(iterator.Value().Interface(), active)
		if err != nil {
			return nil, err
		}
		reflectedItem, err := memorySnapshotCloneValue(item, value.Type().Elem())
		if err != nil {
			return nil, err
		}
		cloned.SetMapIndex(key, reflectedItem)
	}
	return cloned.Interface(), nil
}

func cloneMemorySnapshotSlice(
	value reflect.Value,
	active map[memorySnapshotCloneVisit]struct{},
) (any, error) {
	if value.IsNil() {
		return reflect.Zero(value.Type()).Interface(), nil
	}
	visit, err := beginMemorySnapshotCloneVisit(value, active)
	if err != nil {
		return nil, err
	}
	defer delete(active, visit)
	cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
	for index := range value.Len() {
		item, err := cloneMemorySnapshotValueActive(value.Index(index).Interface(), active)
		if err != nil {
			return nil, err
		}
		reflectedItem, err := memorySnapshotCloneValue(item, value.Type().Elem())
		if err != nil {
			return nil, err
		}
		cloned.Index(index).Set(reflectedItem)
	}
	return cloned.Interface(), nil
}

func beginMemorySnapshotCloneVisit(
	value reflect.Value,
	active map[memorySnapshotCloneVisit]struct{},
) (memorySnapshotCloneVisit, error) {
	visit := memorySnapshotCloneVisit{
		kind: value.Kind(), typeOf: value.Type(), pointer: value.Pointer(),
	}
	if visit.pointer == 0 {
		return visit, nil
	}
	if _, exists := active[visit]; exists {
		return memorySnapshotCloneVisit{}, fmt.Errorf("resource value contains a reference cycle: %w", stores.ErrSnapshotUnsupported)
	}
	active[visit] = struct{}{}
	return visit, nil
}

func memorySnapshotCloneValue(value any, target reflect.Type) (reflect.Value, error) {
	if value == nil {
		return reflect.Zero(target), nil
	}
	reflected := reflect.ValueOf(value)
	if reflected.Type().AssignableTo(target) {
		return reflected, nil
	}
	return reflect.Value{}, fmt.Errorf(
		"resource clone type %v cannot populate %v: %w",
		reflected.Type(),
		target,
		stores.ErrSnapshotUnsupported,
	)
}

func memorySnapshotMapKeyIsImmutable(typeOf reflect.Type) bool {
	switch typeOf.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}

func isNilMemorySnapshotObject(object runtime.Object) bool {
	if object == nil {
		return true
	}
	value := reflect.ValueOf(object)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

var (
	_ stores.SnapshotProvider            = (*MemoryStore)(nil)
	_ stores.ReadSnapshot                = (*memoryReadSnapshot)(nil)
	_ stores.IdentityOrderedReadSnapshot = (*memoryReadSnapshot)(nil)
)
