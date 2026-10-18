// Package incremental is a generic incremental computation graph. Queries are
// opaque keys; a session records the exact inputs each evaluation read —
// including reads that found nothing — and commits transactionally against a
// generation, so a later session reruns exactly the queries whose recorded
// reads changed. It knows nothing about templating or Kubernetes.
//
// See docs/adr/0023-incremental-render-graph.md.
package incremental

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental/internal/immutablevector"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental/internal/orderedset"
	"github.com/haproxy-haptic/HAPTIC/pkg/persistenttree"
)

type dependencyKind uint8

const (
	inputDependency dependencyKind = iota + 1
	queryDependency
)

type dependencyKey struct {
	kind  dependencyKind
	input InputKey
	query QueryKey
}

type dependency struct {
	key       dependencyKey
	changedAt uint64
	revision  Revision
	found     bool
}

type inputEntry struct {
	revision  Revision
	found     bool
	value     []byte
	changedAt uint64
}

type nodeEntry struct {
	value     ExactValueRoot
	deps      []dependency
	inputs    []InputRevision
	changedAt uint64
	dirty     bool
}

type committedInputEntry struct {
	revision  Revision
	found     bool
	value     string
	changedAt uint64
}

type committedNodeEntry struct {
	value     ExactValueRoot
	deps      immutablevector.Root[dependency]
	inputs    immutablevector.Root[InputRevision]
	changedAt uint64
	dirty     bool
}

type graphGeneration struct {
	graph          *Graph
	seal           *graphGeneration
	authentication *graphGenerationAuthentication
	number         uint64
	inputs         *persistenttree.Tree[committedInputEntry]
	nodes          *persistenttree.Tree[committedNodeEntry]
	reverse        *persistenttree.Tree[orderedset.Root]
	dirty          *persistenttree.Tree[struct{}]
	counters       *persistenttree.Tree[NodeCounters]
}

type graphGenerationAuthentication struct {
	seal       *graphGenerationAuthentication
	generation *graphGeneration
	number     uint64

	inputs       *persistenttree.Tree[committedInputEntry]
	inputsRoot   *persistenttree.Node[committedInputEntry]
	inputsLen    int
	nodes        *persistenttree.Tree[committedNodeEntry]
	nodesRoot    *persistenttree.Node[committedNodeEntry]
	nodesLen     int
	reverse      *persistenttree.Tree[orderedset.Root]
	reverseRoot  *persistenttree.Node[orderedset.Root]
	reverseLen   int
	dirty        *persistenttree.Tree[struct{}]
	dirtyRoot    *persistenttree.Node[struct{}]
	dirtyLen     int
	counters     *persistenttree.Tree[NodeCounters]
	countersRoot *persistenttree.Node[NodeCounters]
	countersLen  int
}

type graphCurrentAuthentication struct {
	seal       *graphCurrentAuthentication
	graph      *Graph
	generation *graphGeneration
	number     uint64
}

func newGraphGeneration(
	graph *Graph,
	number uint64,
	inputs map[InputKey]inputEntry,
	nodes map[QueryKey]nodeEntry,
	reverse map[dependencyKey]orderedset.Root,
	dirty map[QueryKey]struct{},
	counters map[QueryKey]NodeCounters,
) (*graphGeneration, error) {
	inputTree, err := buildCommittedInputTree(number, inputs)
	if err != nil {
		return nil, err
	}
	nodeTree, err := buildCommittedNodeTree(graph, number, nodes)
	if err != nil {
		return nil, err
	}
	reverseTree, err := buildCommittedReverseTree(graph, reverse)
	if err != nil {
		return nil, err
	}
	dirtyTree, err := buildCommittedDirtyTree(dirty)
	if err != nil {
		return nil, err
	}
	if err := validateCommittedDirtyTree(nodeTree, dirtyTree); err != nil {
		return nil, err
	}
	counterTree, err := buildCommittedCounterTree(counters)
	if err != nil {
		return nil, err
	}
	return newGraphGenerationFromTrees(
		graph,
		number,
		inputTree,
		nodeTree,
		reverseTree,
		dirtyTree,
		counterTree,
	)
}

func validateCommittedDirtyTree(
	nodes *persistenttree.Tree[committedNodeEntry],
	dirty *persistenttree.Tree[struct{}],
) error {
	if nodes == nil || dirty == nil {
		return errors.New("incremental committed dirty index has invalid storage")
	}
	var validationErr error
	nodes.Root().Walk(func(key string, entry committedNodeEntry) bool {
		_, indexed := dirty.Root().Get([]byte(key))
		if indexed != entry.dirty {
			validationErr = errors.New("incremental committed dirty index disagrees with query state")
			return true
		}
		return false
	})
	if validationErr != nil {
		return validationErr
	}
	dirty.Root().Walk(func(key string, _ struct{}) bool {
		entry, exists := nodes.Root().Get([]byte(key))
		if !exists || !entry.dirty {
			validationErr = errors.New("incremental committed dirty index contains an invalid query")
			return true
		}
		return false
	})
	return validationErr
}

func newGraphGenerationFromTrees(
	graph *Graph,
	number uint64,
	inputs *persistenttree.Tree[committedInputEntry],
	nodes *persistenttree.Tree[committedNodeEntry],
	reverse *persistenttree.Tree[orderedset.Root],
	dirty *persistenttree.Tree[struct{}],
	counters *persistenttree.Tree[NodeCounters],
) (*graphGeneration, error) {
	if graph == nil || inputs == nil || nodes == nil || reverse == nil || dirty == nil || counters == nil {
		return nil, errors.New("incremental graph generation has invalid storage")
	}
	generation := &graphGeneration{
		graph:    graph,
		number:   number,
		inputs:   inputs,
		nodes:    nodes,
		reverse:  reverse,
		dirty:    dirty,
		counters: counters,
	}
	generation.seal = generation
	authentication := &graphGenerationAuthentication{
		generation: generation,
		number:     number,
		inputs:     inputs, inputsRoot: inputs.Root(), inputsLen: inputs.Len(),
		nodes: nodes, nodesRoot: nodes.Root(), nodesLen: nodes.Len(),
		reverse: reverse, reverseRoot: reverse.Root(), reverseLen: reverse.Len(),
		dirty: dirty, dirtyRoot: dirty.Root(), dirtyLen: dirty.Len(),
		counters: counters, countersRoot: counters.Root(), countersLen: counters.Len(),
	}
	authentication.seal = authentication
	generation.authentication = authentication
	return generation, nil
}

func (g *graphGeneration) valid(graph *Graph) bool {
	if g == nil || g.seal != g || g.graph != graph || g.authentication == nil {
		return false
	}
	authentication := g.authentication
	return authentication.seal == authentication && authentication.generation == g &&
		authentication.number == g.number &&
		authenticatedTree(g.inputs, authentication.inputs, authentication.inputsRoot, authentication.inputsLen) &&
		authenticatedTree(g.nodes, authentication.nodes, authentication.nodesRoot, authentication.nodesLen) &&
		authenticatedTree(g.reverse, authentication.reverse, authentication.reverseRoot, authentication.reverseLen) &&
		authenticatedTree(g.dirty, authentication.dirty, authentication.dirtyRoot, authentication.dirtyLen) &&
		authenticatedTree(g.counters, authentication.counters, authentication.countersRoot, authentication.countersLen)
}

func authenticatedTree[V any](
	current,
	authenticated *persistenttree.Tree[V],
	root *persistenttree.Node[V],
	size int,
) bool {
	return current != nil && current == authenticated && current.Root() == root && current.Len() == size
}

// Graph stores committed incremental query state across sessions.
type Graph struct {
	definitions map[QueryKey]QueryFunc
	provider    DefinitionProvider
	options     Options

	commitMu sync.Mutex
	mu       sync.RWMutex

	current               *graphGeneration
	currentAuthentication *graphCurrentAuthentication

	valueAuthority        *exactValueAuthority
	reverseAuthority      *orderedset.Authority
	dependencyAuthority   *immutablevector.Authority[dependency]
	observationAuthority  *immutablevector.Authority[InputRevision]
	retiredInputAuthority *immutablevector.Authority[InputKey]
}

// New constructs a graph with immutable query definitions.
func New(definitions ...Definition) (*Graph, error) {
	return NewWithProviderOptions(nil, Options{}, definitions...)
}

// NewWithProvider adds a fixed graph-lifetime dynamic definition provider.
func NewWithProvider(provider DefinitionProvider, definitions ...Definition) (*Graph, error) {
	return NewWithProviderOptions(provider, Options{}, definitions...)
}

// NewWithProviderOptions configures a graph with dynamic definitions and cache behavior.
func NewWithProviderOptions(
	provider DefinitionProvider,
	options Options,
	definitions ...Definition,
) (*Graph, error) {
	runs := make(map[QueryKey]QueryFunc, len(definitions))
	for _, definition := range definitions {
		if !validQueryKey(definition.Key) {
			return nil, fmt.Errorf("incremental query key is empty")
		}
		if definition.Run == nil {
			return nil, fmt.Errorf("incremental query %s has no implementation", definition.Key)
		}
		if _, exists := runs[definition.Key]; exists {
			return nil, fmt.Errorf("incremental query %s is defined more than once", definition.Key)
		}
		runs[definition.Key] = definition.Run
	}

	graph := &Graph{
		definitions: runs,
		provider:    provider,
		options:     options,
	}
	graph.valueAuthority = newExactValueAuthority()
	graph.reverseAuthority = orderedset.NewAuthority()
	graph.dependencyAuthority = immutablevector.NewAuthority[dependency]()
	graph.observationAuthority = immutablevector.NewAuthority[InputRevision]()
	graph.retiredInputAuthority = immutablevector.NewAuthority[InputKey]()
	var err error
	initial, err := newGraphGeneration(
		graph,
		0,
		map[InputKey]inputEntry{},
		map[QueryKey]nodeEntry{},
		map[dependencyKey]orderedset.Root{},
		map[QueryKey]struct{}{},
		map[QueryKey]NodeCounters{},
	)
	if err != nil {
		return nil, fmt.Errorf("creating incremental graph generation: %w", err)
	}
	graph.installGenerationLocked(initial)
	return graph, nil
}

func (g *Graph) installGenerationLocked(generation *graphGeneration) {
	g.current = generation
	authentication := &graphCurrentAuthentication{
		graph: g, generation: generation, number: generation.number,
	}
	authentication.seal = authentication
	g.currentAuthentication = authentication
}

func (g *Graph) currentValidLocked() bool {
	if g == nil || g.current == nil || g.currentAuthentication == nil {
		return false
	}
	authentication := g.currentAuthentication
	return authentication.seal == authentication && authentication.graph == g &&
		authentication.generation == g.current && authentication.number == g.current.number &&
		g.current.valid(g)
}

func (g *Graph) definition(key QueryKey) (run QueryFunc, found bool, err error) {
	if run, exists := g.definitions[key]; exists {
		return run, true, nil
	}
	if g.provider == nil {
		return nil, false, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			run = nil
			found = false
			err = &panicError{value: recovered}
		}
	}()
	run, found = g.provider(key)
	return run, found, nil
}

// Begin starts an isolated transaction from the current generation.
func (g *Graph) Begin() (*Session, error) {
	return g.begin(false, nil, false)
}

// BeginWithResolver starts a transaction with lazy exact input discovery.
func (g *Graph) BeginWithResolver(resolver InputResolver) (*Session, error) {
	return g.begin(false, resolver, false)
}

// BeginColdReset starts a transaction that replaces all cache state after a journal gap.
func (g *Graph) BeginColdReset(inputs ...Input) (*Session, error) {
	return g.beginColdReset(nil, false, inputs)
}

// BeginColdResetWithResolver starts a cold transaction with lazy input discovery.
func (g *Graph) BeginColdResetWithResolver(
	resolver InputResolver,
	inputs ...Input,
) (*Session, error) {
	return g.beginColdReset(resolver, false, inputs)
}

// BeginColdResetWithConcurrentResolver starts a cold transaction whose resolver accepts concurrent calls.
func (g *Graph) BeginColdResetWithConcurrentResolver(
	resolver InputResolver,
	inputs ...Input,
) (*Session, error) {
	return g.beginColdReset(resolver, true, inputs)
}

func (g *Graph) beginColdReset(
	resolver InputResolver,
	resolverConcurrent bool,
	inputs []Input,
) (*Session, error) {
	session, err := g.begin(true, resolver, resolverConcurrent)
	if err != nil {
		return nil, err
	}
	if err := session.applyColdInputs(inputs); err != nil {
		session.Abort()
		return nil, err
	}
	return session, nil
}

func (g *Graph) begin(cold bool, resolver InputResolver, resolverConcurrent bool) (*Session, error) {
	if g.options.RetireUnreferencedInputs && resolver == nil {
		return nil, ErrResolverRequired
	}
	g.mu.RLock()
	current := g.current
	if !g.currentValidLocked() {
		g.mu.RUnlock()
		return nil, fmt.Errorf("incremental graph generation has invalid provenance")
	}
	generation := current.number
	g.mu.RUnlock()
	if generation == ^uint64(0) {
		return nil, ErrGenerationExhausted
	}
	replacement := cold || generation == 0
	var stagedReverse map[dependencyKey]orderedset.Root
	if !replacement {
		stagedReverse = map[dependencyKey]orderedset.Root{}
	}

	return &Session{
		graph:              g,
		base:               current,
		baseGeneration:     generation,
		targetGeneration:   generation + 1,
		cold:               cold,
		replacement:        replacement,
		resolver:           resolver,
		resolverConcurrent: resolverConcurrent,
		baseInputs:         map[InputKey]inputEntry{},
		inputChanges:       map[InputKey]inputEntry{},
		inputVersions:      map[inputVersionKey]inputEntry{},
		baseNodes:          map[QueryKey]nodeEntry{},
		nodeChanges:        map[QueryKey]nodeEntry{},
		observations:       map[InputKey]InputRevision{},
		active:             map[QueryKey]int{},
		queried:            map[QueryKey]struct{}{},
		stagedReverse:      stagedReverse,
		counterDeltas:      map[QueryKey]NodeCounters{},
		removedQueries:     map[QueryKey]struct{}{},
	}, nil
}

// Generation returns the latest committed generation.
func (g *Graph) Generation() uint64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.currentValidLocked() {
		return 0
	}
	return g.current.number
}

// Value returns a cloned committed query value.
func (g *Graph) Value(key QueryKey) ([]byte, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.currentValidLocked() {
		return nil, false
	}
	entry, exists := g.current.nodes.Root().Get([]byte(key.id.String()))
	if !exists || entry.dirty {
		return nil, false
	}
	if err := entry.value.validateOwned(g.valueAuthority, key); err != nil {
		return nil, false
	}
	value, err := entry.value.Bytes()
	return value, err == nil
}

// ExactValue returns the authenticated committed value root without copying its payload.
func (g *Graph) ExactValue(key QueryKey) (ExactValueRoot, bool, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.currentValidLocked() {
		return ExactValueRoot{}, false, fmt.Errorf("incremental graph generation has invalid provenance")
	}
	entry, exists := g.current.nodes.Root().Get([]byte(key.id.String()))
	if !exists || entry.dirty {
		return ExactValueRoot{}, false, nil
	}
	if err := entry.value.validateOwned(g.valueAuthority, key); err != nil {
		return ExactValueRoot{}, false, err
	}
	return entry.value, true, nil
}

// ValidateExactValue verifies that root belongs to this graph and query in O(1).
func (g *Graph) ValidateExactValue(key QueryKey, root ExactValueRoot) error {
	if g == nil || !validQueryKey(key) {
		return fmt.Errorf("incremental exact value has invalid graph ownership")
	}
	return root.validateOwned(g.valueAuthority, key)
}

// ValidateCommittedExactValue verifies exact identity with the graph's committed node root.
func (g *Graph) ValidateCommittedExactValue(key QueryKey, root ExactValueRoot) error {
	if g == nil || !validQueryKey(key) {
		return fmt.Errorf("incremental exact value has invalid graph ownership")
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.currentValidLocked() {
		return fmt.Errorf("incremental graph generation has invalid provenance")
	}
	if err := root.validateOwned(g.valueAuthority, key); err != nil {
		return err
	}
	entry, exists := g.current.nodes.Root().Get([]byte(key.id.String()))
	if !exists {
		return fmt.Errorf("incremental query has no committed exact value")
	}
	if err := entry.value.validateOwned(g.valueAuthority, key); err != nil {
		return err
	}
	if entry.value.value != root.value {
		return fmt.Errorf("incremental exact value is not the committed query root")
	}
	return nil
}

// Counters returns committed counters for one query.
func (g *Graph) Counters(key QueryKey) NodeCounters {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.currentValidLocked() {
		return NodeCounters{}
	}
	counters, _ := g.current.counters.Root().Get([]byte(key.id.String()))
	return counters
}

// HasDependents reports whether at least one committed query directly depends on key.
func (g *Graph) HasDependents(key QueryKey) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.hasDependentsLocked(queryDep(key))
}

// HasInputDependents reports whether at least one committed query directly depends on key.
func (g *Graph) HasInputDependents(key InputKey) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.hasDependentsLocked(inputDep(key))
}

func (g *Graph) hasDependentsLocked(key dependencyKey) bool {
	if !g.currentValidLocked() {
		return false
	}
	return g.hasDependentsOfLocked(g.current, key)
}

func (g *Graph) hasDependentsOfLocked(generation *graphGeneration, key dependencyKey) bool {
	root, err := g.reverseRootOfLocked(generation, key)
	if err != nil {
		return false
	}
	size, err := root.Len(g.reverseAuthority, reverseScope(key))
	return err == nil && size != 0
}

func (g *Graph) reverseRootLocked(key dependencyKey) (orderedset.Root, error) {
	if !g.currentValidLocked() {
		return orderedset.Root{}, fmt.Errorf("incremental graph generation has invalid provenance")
	}
	return g.reverseRootOfLocked(g.current, key)
}

func (g *Graph) reverseRootOfLocked(generation *graphGeneration, key dependencyKey) (orderedset.Root, error) {
	root, exists := generation.reverse.Root().Get([]byte(dependencyTreeKey(key)))
	if !exists {
		root = g.reverseAuthority.Empty()
	}
	if err := root.ValidateOwnership(g.reverseAuthority, reverseScope(key)); err != nil {
		return orderedset.Root{}, fmt.Errorf("incremental reverse dependency: %w", err)
	}
	return root, nil
}

func cloneInputEntry(entry inputEntry) inputEntry {
	entry.value = cloneBytes(entry.value)
	return entry
}

func openCommittedInputEntry(entry committedInputEntry) inputEntry {
	return inputEntry{
		revision:  entry.revision,
		found:     entry.found,
		value:     []byte(entry.value),
		changedAt: entry.changedAt,
	}
}

func sealCommittedInputEntry(entry inputEntry) (committedInputEntry, error) {
	if !validRevision(entry.revision) || entry.changedAt == 0 || (!entry.found && len(entry.value) != 0) {
		return committedInputEntry{}, errors.New("incremental committed input is invalid")
	}
	return committedInputEntry{
		revision:  entry.revision,
		found:     entry.found,
		value:     strings.Clone(string(entry.value)),
		changedAt: entry.changedAt,
	}, nil
}

func cloneNodeEntry(entry *nodeEntry) nodeEntry {
	cloned := *entry
	cloned.deps = append([]dependency(nil), entry.deps...)
	cloned.inputs = append([]InputRevision(nil), entry.inputs...)
	return cloned
}

func sealCommittedNodeEntry(
	graph *Graph,
	key QueryKey,
	entry nodeEntry,
	generation uint64,
) (committedNodeEntry, error) {
	if graph == nil || !validQueryKey(key) || entry.changedAt == 0 || entry.changedAt > generation {
		return committedNodeEntry{}, errors.New("incremental committed query is invalid")
	}
	if err := entry.value.validateOwned(graph.valueAuthority, key); err != nil {
		return committedNodeEntry{}, err
	}
	if err := validateExactQueryObservationDependencies(entry.deps, entry.inputs, generation); err != nil {
		return committedNodeEntry{}, err
	}
	dependencies, err := graph.dependencyAuthority.Own(entry.deps)
	if err != nil {
		return committedNodeEntry{}, err
	}
	observations, err := graph.observationAuthority.Own(entry.inputs)
	if err != nil {
		return committedNodeEntry{}, err
	}
	return committedNodeEntry{
		value:     entry.value,
		deps:      dependencies,
		inputs:    observations,
		changedAt: entry.changedAt,
		dirty:     entry.dirty,
	}, nil
}

func openCommittedNodeEntry(graph *Graph, key QueryKey, entry committedNodeEntry) (nodeEntry, error) {
	if graph == nil || !validQueryKey(key) {
		return nodeEntry{}, errors.New("incremental committed query has invalid ownership")
	}
	if err := entry.value.validateOwned(graph.valueAuthority, key); err != nil {
		return nodeEntry{}, err
	}
	dependencies, err := entry.deps.Values(graph.dependencyAuthority)
	if err != nil {
		return nodeEntry{}, fmt.Errorf("incremental committed query dependencies: %w", err)
	}
	observations, err := entry.inputs.Values(graph.observationAuthority)
	if err != nil {
		return nodeEntry{}, fmt.Errorf("incremental committed query observations: %w", err)
	}
	return nodeEntry{
		value:     entry.value,
		deps:      dependencies,
		inputs:    observations,
		changedAt: entry.changedAt,
		dirty:     entry.dirty,
	}, nil
}

func dependencyTreeKey(key dependencyKey) string {
	switch key.kind {
	case inputDependency:
		return string([]byte{byte(inputDependency)}) + key.input.id.String()
	case queryDependency:
		return string([]byte{byte(queryDependency)}) + key.query.id.String()
	default:
		return ""
	}
}

func parseDependencyTreeKey(value string) (dependencyKey, bool) {
	if len(value) < 2 {
		return dependencyKey{}, false
	}
	switch dependencyKind(value[0]) {
	case inputDependency:
		return inputDep(NewInputKey(value[1:])), true
	case queryDependency:
		return queryDep(NewQueryKey(value[1:])), true
	default:
		return dependencyKey{}, false
	}
}

func buildCommittedInputTree(
	generation uint64,
	inputs map[InputKey]inputEntry,
) (*persistenttree.Tree[committedInputEntry], error) {
	keys := make([]InputKey, 0, len(inputs))
	for key := range inputs {
		keys = append(keys, key)
	}
	sortInputKeys(keys)
	entries := make([]persistenttree.Entry[committedInputEntry], len(keys))
	for index, key := range keys {
		if !validInputKey(key) {
			return nil, errors.New("incremental committed input key is empty")
		}
		entry, err := sealCommittedInputEntry(inputs[key])
		if err != nil {
			return nil, fmt.Errorf("incremental committed input %s: %w", key, err)
		}
		if entry.changedAt > generation {
			return nil, fmt.Errorf("incremental committed input %s changed after its generation", key)
		}
		entries[index] = persistenttree.Entry[committedInputEntry]{Key: key.id.String(), Value: entry}
	}
	return persistenttree.NewFromSorted(entries)
}

func buildCommittedNodeTree(
	graph *Graph,
	generation uint64,
	nodes map[QueryKey]nodeEntry,
) (*persistenttree.Tree[committedNodeEntry], error) {
	keys := sortedNodeEntryKeys(nodes)
	entries := make([]persistenttree.Entry[committedNodeEntry], len(keys))
	for index, key := range keys {
		entry, err := sealCommittedNodeEntry(graph, key, nodes[key], generation)
		if err != nil {
			return nil, fmt.Errorf("incremental committed query %s: %w", key, err)
		}
		entries[index] = persistenttree.Entry[committedNodeEntry]{Key: key.id.String(), Value: entry}
	}
	return persistenttree.NewFromSorted(entries)
}

func buildCommittedReverseTree(
	graph *Graph,
	reverse map[dependencyKey]orderedset.Root,
) (*persistenttree.Tree[orderedset.Root], error) {
	if graph == nil {
		return nil, errors.New("incremental committed reverse dependency has no graph")
	}
	type reverseEntry struct {
		key    dependencyKey
		opaque string
		root   orderedset.Root
	}
	ordered := make([]reverseEntry, 0, len(reverse))
	for key, root := range reverse {
		opaque := dependencyTreeKey(key)
		if opaque == "" {
			return nil, errors.New("incremental committed reverse dependency key is invalid")
		}
		size, err := root.Len(graph.reverseAuthority, reverseScope(key))
		if err != nil {
			return nil, fmt.Errorf("incremental committed reverse dependency: %w", err)
		}
		if size == 0 {
			return nil, errors.New("incremental committed reverse dependency is empty")
		}
		ordered = append(ordered, reverseEntry{key: key, opaque: opaque, root: root})
	}
	slices.SortFunc(ordered, func(left, right reverseEntry) int {
		return strings.Compare(left.opaque, right.opaque)
	})
	entries := make([]persistenttree.Entry[orderedset.Root], len(ordered))
	for index, entry := range ordered {
		entries[index] = persistenttree.Entry[orderedset.Root]{Key: entry.opaque, Value: entry.root}
	}
	return persistenttree.NewFromSorted(entries)
}

func buildCommittedDirtyTree(
	dirty map[QueryKey]struct{},
) (*persistenttree.Tree[struct{}], error) {
	keys := make([]QueryKey, 0, len(dirty))
	for key := range dirty {
		if !validQueryKey(key) {
			return nil, errors.New("incremental committed dirty query key is empty")
		}
		keys = append(keys, key)
	}
	sortQueryKeys(keys)
	entries := make([]persistenttree.Entry[struct{}], len(keys))
	for index, key := range keys {
		entries[index] = persistenttree.Entry[struct{}]{Key: key.id.String()}
	}
	return persistenttree.NewFromSorted(entries)
}

func buildCommittedCounterTree(
	counters map[QueryKey]NodeCounters,
) (*persistenttree.Tree[NodeCounters], error) {
	keys := make([]QueryKey, 0, len(counters))
	for key := range counters {
		if !validQueryKey(key) {
			return nil, errors.New("incremental committed counter query key is empty")
		}
		keys = append(keys, key)
	}
	sortQueryKeys(keys)
	entries := make([]persistenttree.Entry[NodeCounters], len(keys))
	for index, key := range keys {
		entries[index] = persistenttree.Entry[NodeCounters]{Key: key.id.String(), Value: counters[key]}
	}
	return persistenttree.NewFromSorted(entries)
}

func cloneCommittedCounters(
	generation *graphGeneration,
) (map[QueryKey]NodeCounters, error) {
	if generation == nil || !generation.valid(generation.graph) {
		return nil, errors.New("incremental graph generation has invalid provenance")
	}
	counters := make(map[QueryKey]NodeCounters, generation.counters.Len())
	generation.counters.Root().Walk(func(key string, value NodeCounters) bool {
		counters[NewQueryKey(key)] = value
		return false
	})
	return counters, nil
}

func inputDep(key InputKey) dependencyKey {
	return dependencyKey{kind: inputDependency, input: key}
}

func queryDep(key QueryKey) dependencyKey {
	return dependencyKey{kind: queryDependency, query: key}
}

func reverseScope(key dependencyKey) orderedset.Scope {
	switch key.kind {
	case inputDependency:
		return orderedset.Scope{Domain: uint8(inputDependency), Key: key.input.id.String()}
	case queryDependency:
		return orderedset.Scope{Domain: uint8(queryDependency), Key: key.query.id.String()}
	default:
		return orderedset.Scope{}
	}
}

func sortQueryKeys(keys []QueryKey) {
	slices.SortFunc(keys, func(left, right QueryKey) int {
		return cmp.Compare(left.id.String(), right.id.String())
	})
}

func sortInputKeys(keys []InputKey) {
	slices.SortFunc(keys, func(left, right InputKey) int {
		return cmp.Compare(left.id.String(), right.id.String())
	})
}

func sortDependencies(deps []dependency) {
	slices.SortFunc(deps, func(left, right dependency) int {
		return compareDependencyKeys(left.key, right.key)
	})
}

func compareDependencyKeys(left, right dependencyKey) int {
	if byKind := cmp.Compare(left.kind, right.kind); byKind != 0 {
		return byKind
	}
	if left.kind == inputDependency {
		return cmp.Compare(left.input.id.String(), right.input.id.String())
	}
	return cmp.Compare(left.query.id.String(), right.query.id.String())
}

func sameDependencyKeys(left, right []dependency) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].key != right[index].key {
			return false
		}
	}
	return true
}

func addCounters(left, right NodeCounters) NodeCounters {
	left.Executions += right.Executions
	left.CacheHits += right.CacheHits
	left.Backdates += right.Backdates
	left.Changes += right.Changes
	left.Invalidations += right.Invalidations
	return left
}
