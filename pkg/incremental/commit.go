package incremental

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental/internal/orderedset"
	"github.com/haproxy-haptic/HAPTIC/pkg/persistenttree"
)

// CommitPublication contains the infallible phases surrounding graph publication.
type CommitPublication struct {
	Publish  func()
	Complete func()
	Abort    func()
}

// CommitPublicationPreparer prepares all caller-owned state before publication starts.
type CommitPublicationPreparer func([]InputKey) (CommitPublication, error)

// Commit verifies exact inputs and publishes the transaction with generation CAS.
func (s *Session) Commit(ctx context.Context, verifier RevisionVerifier) error {
	return s.CommitWithPublisher(ctx, verifier, nil)
}

// CommitWithPublisher runs an infallible publication after verification and before graph visibility.
func (s *Session) CommitWithPublisher(
	ctx context.Context,
	verifier RevisionVerifier,
	publish func(),
) error {
	return s.CommitWithPreparedPublisher(ctx, verifier, func([]InputKey) (CommitPublication, error) {
		return CommitPublication{Publish: publish}, nil
	})
}

// CommitWithPreparedPublisher prepares graph and caller state before either becomes visible.
func (s *Session) CommitWithPreparedPublisher(
	ctx context.Context,
	verifier RevisionVerifier,
	prepare CommitPublicationPreparer,
) error {
	if err := s.beginPublication(ctx); err != nil {
		return err
	}
	if verifier == nil {
		err := s.fail(ErrVerifierRequired)
		s.discard()
		return err
	}
	if s.replacement {
		prepared, err := s.prepareGraphCommitReady(ctx)
		if err != nil {
			return err
		}
		return prepared.PublishWithPreparedPublisher(ctx, verifier, prepare)
	}
	if !s.graph.matchesGeneration(s.baseGeneration) {
		s.discard()
		return ErrCommitConflict
	}
	s.committing = true
	plan, observations, err := s.prepareGraphCommit()
	if err != nil {
		s.discard()
		return err
	}
	return s.verifyAndPublishGraphCommit(ctx, verifier, prepare, plan, observations)
}

func (s *Session) prepareGraphCommit() (*graphCommitPlan, []InputRevision, error) {
	var plan *graphCommitPlan
	var observations []InputRevision
	var err error
	if s.replacement {
		plan, observations, err = s.prepareReplacementCommit()
		if err != nil {
			s.discard()
			return nil, nil, fmt.Errorf("preparing incremental graph publication: %w", err)
		}
	} else {
		if err := s.validateNodeChanges(); err != nil {
			s.committing = false
			return nil, nil, s.fail(err)
		}
		observations, err = s.commitObservations()
		if err != nil {
			s.committing = false
			return nil, nil, s.fail(err)
		}
	}
	return plan, observations, nil
}

func (s *Session) verifyAndPublishGraphCommit(
	ctx context.Context,
	verifier RevisionVerifier,
	prepare CommitPublicationPreparer,
	plan *graphCommitPlan,
	observations []InputRevision,
) (err error) {
	publication := CommitPublication{}
	published := false
	defer func() {
		if !published && publication.Abort != nil {
			err = errors.Join(err, callCommitPublicationPhase("abort", publication.Abort))
		}
		s.discard()
	}()
	s.graph.commitMu.Lock()
	defer s.graph.commitMu.Unlock()
	if !s.graph.matchesGeneration(s.baseGeneration) {
		return ErrCommitConflict
	}

	s.verifying = true
	verifyErr := verifyCommitInputs(ctx, verifier, observations)
	s.verifying = false
	if verifyErr != nil {
		return verifyErr
	}
	if err := s.publicationActive(); err != nil {
		return err
	}
	s.graph.mu.Lock()
	defer s.graph.mu.Unlock()
	if !s.graph.currentValidLocked() || s.graph.current.number != s.baseGeneration {
		return ErrCommitConflict
	}
	plan, err = s.resolveGraphCommitPlanLocked(plan)
	if err != nil {
		return err
	}
	if prepare != nil {
		publication, err = callCommitPublicationPreparer(
			prepare,
			append([]InputKey(nil), plan.retiredInputs...),
		)
		if err != nil {
			return fmt.Errorf("preparing incremental publication: %w", err)
		}
	}
	if err := s.publicationActive(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.runCommitPublishPhaseLocked(publication); err != nil {
		return err
	}
	if err := s.publishPreparedGraphCommitLocked(plan, publication); err != nil {
		return err
	}
	published = true
	return nil
}

func (s *Session) resolveGraphCommitPlanLocked(plan *graphCommitPlan) (*graphCommitPlan, error) {
	if plan == nil {
		prepared, err := s.prepareIncrementalGraphCommitLocked()
		if err != nil {
			return nil, fmt.Errorf("preparing incremental graph publication: %w", err)
		}
		plan = prepared
	}
	if !plan.replacementGeneration.valid(s.graph) ||
		plan.replacementGeneration.number != plan.generation {
		return nil, fmt.Errorf("preparing incremental graph publication: generation has invalid provenance")
	}
	return plan, nil
}

func (s *Session) runCommitPublishPhaseLocked(publication CommitPublication) error {
	if publication.Publish == nil {
		return nil
	}
	if err := callCommitPublicationPhase("publish", publication.Publish); err != nil {
		return err
	}
	return s.publicationActive()
}

func (s *Session) publishPreparedGraphCommitLocked(
	plan *graphCommitPlan,
	publication CommitPublication,
) error {
	rollback := prepareGraphCommitRollbackLocked(s, plan)
	if err := callCommitPublicationPhase("graph", func() { s.publishGraphCommitLocked(plan) }); err != nil {
		rollback.restoreLocked(s)
		return err
	}
	if err := s.publicationActive(); err != nil {
		rollback.restoreLocked(s)
		return err
	}
	if publication.Complete != nil {
		if err := callCommitPublicationPhase("complete", publication.Complete); err != nil {
			rollback.restoreLocked(s)
			return err
		}
		if err := s.publicationActive(); err != nil {
			rollback.restoreLocked(s)
			return err
		}
	}
	if !s.completePublication() {
		rollback.restoreLocked(s)
		return ErrSessionClosed
	}
	return nil
}

type graphCommitRollback struct {
	previous               *graphGeneration
	previousAuthentication *graphCurrentAuthentication
	retiredInputs          []InputKey
}

func prepareGraphCommitRollbackLocked(s *Session, _ *graphCommitPlan) graphCommitRollback {
	return graphCommitRollback{
		previous:               s.graph.current,
		previousAuthentication: s.graph.currentAuthentication,
		retiredInputs:          append([]InputKey(nil), s.retiredInputs...),
	}
}

func (r graphCommitRollback) restoreLocked(s *Session) {
	if r.previous == nil {
		return
	}
	s.graph.current = r.previous
	s.graph.currentAuthentication = r.previousAuthentication
	s.retiredInputs = append([]InputKey(nil), r.retiredInputs...)
}

type graphCommitPlanResult struct {
	plan *graphCommitPlan
	err  error
}

func (s *Session) prepareReplacementCommit() (*graphCommitPlan, []InputRevision, error) {
	prepared := make(chan graphCommitPlanResult, 1)
	go func() {
		plan, err := s.prepareReplacementGraphCommit()
		prepared <- graphCommitPlanResult{plan: plan, err: err}
	}()
	validationErr := s.validateNodeChanges()
	observations, observationErr := s.commitObservations()
	result := <-prepared
	if validationErr != nil {
		return nil, nil, validationErr
	}
	if observationErr != nil {
		return nil, nil, observationErr
	}
	return result.plan, observations, result.err
}

func (s *Session) validateNodeChanges() error {
	for key, entry := range s.nodeChanges {
		if err := entry.value.validateOwned(s.graph.valueAuthority, key); err != nil {
			return fmt.Errorf("incremental query %s value: %w", key, err)
		}
	}
	return nil
}

func verifyCommitInputs(
	ctx context.Context,
	verifier RevisionVerifier,
	observations []InputRevision,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("incremental input verifier panicked: %v", recovered)
		}
	}()
	verified, err := verifier(ctx, observations)
	if err != nil {
		return fmt.Errorf("incremental input verification failed: %w", err)
	}
	if !verified {
		return ErrRevisionConflict
	}
	return ctx.Err()
}

type graphCommitPlan struct {
	generation            uint64
	replacement           bool
	replacementGeneration *graphGeneration
	inputs                map[InputKey]inputEntry
	nodes                 map[QueryKey]nodeEntry
	reverse               map[dependencyKey]reverseSetChange
	counters              map[QueryKey]NodeCounters
	removed               []QueryKey
	retiredInputs         []InputKey
}

type reverseSetChange struct {
	root  orderedset.Root
	empty bool
}

func (s *Session) publishGraphCommitLocked(plan *graphCommitPlan) {
	s.graph.installGenerationLocked(plan.replacementGeneration)
	s.retiredInputs = plan.retiredInputs
}

func (s *Session) prepareReplacementGraphCommit() (*graphCommitPlan, error) {
	inputs, nodes := s.prepareReplacementEntries()
	reverse, err := s.replacementReverseRoots()
	if err != nil {
		return nil, err
	}
	if err := authenticateReplacementReverseRoots(s.graph.reverseAuthority, reverse); err != nil {
		return nil, err
	}
	if err := s.validateReplacementRemovedQueries(reverse); err != nil {
		return nil, err
	}
	retired, err := s.prepareReplacementRetirement(inputs, reverse)
	if err != nil {
		return nil, err
	}
	counters, err := s.prepareReplacementCounters()
	if err != nil {
		return nil, err
	}
	generation, err := newGraphGeneration(
		s.graph,
		s.targetGeneration,
		inputs,
		nodes,
		reverse,
		map[QueryKey]struct{}{},
		counters,
	)
	if err != nil {
		return nil, err
	}
	return &graphCommitPlan{
		generation:            s.targetGeneration,
		replacement:           true,
		replacementGeneration: generation,
		retiredInputs:         retired,
	}, nil
}

func (s *Session) prepareReplacementEntries() (
	inputs map[InputKey]inputEntry,
	nodes map[QueryKey]nodeEntry,
) {
	inputs = s.inputChanges
	if inputs == nil {
		inputs = map[InputKey]inputEntry{}
	} else if s.graph.options.RetireUnreferencedInputs {
		// Retirement must preserve the transaction snapshots used by the commit verifier.
		inputs = maps.Clone(inputs)
	}
	nodes = s.nodeChanges
	if nodes == nil {
		nodes = map[QueryKey]nodeEntry{}
	}
	if len(s.removedQueries) != 0 {
		retained := make(map[QueryKey]nodeEntry, len(nodes))
		for key, entry := range nodes {
			if _, removed := s.removedQueries[key]; !removed {
				retained[key] = entry
			}
		}
		nodes = retained
	}
	return inputs, nodes
}

func authenticateReplacementReverseRoots(
	authority *orderedset.Authority,
	reverse map[dependencyKey]orderedset.Root,
) error {
	for dependency, root := range reverse {
		size, err := root.Len(authority, reverseScope(dependency))
		if err != nil {
			return fmt.Errorf("authenticating incremental replacement reverse dependency: %w", err)
		}
		if size == 0 {
			return fmt.Errorf("incremental replacement reverse dependency is empty")
		}
	}
	return nil
}

func (s *Session) validateReplacementRemovedQueries(
	reverse map[dependencyKey]orderedset.Root,
) error {
	for key := range s.removedQueries {
		dependency := queryDep(key)
		root, exists := reverse[dependency]
		if !exists {
			root = s.graph.reverseAuthority.Empty()
		}
		size, err := root.Len(s.graph.reverseAuthority, reverseScope(dependency))
		if err != nil {
			return fmt.Errorf("checking removed replacement query dependents: %w", err)
		}
		if size != 0 {
			return fmt.Errorf("removed incremental query %s retains dependents", key)
		}
	}
	return nil
}

func (s *Session) prepareReplacementRetirement(
	inputs map[InputKey]inputEntry,
	reverse map[dependencyKey]orderedset.Root,
) ([]InputKey, error) {
	if !s.graph.options.RetireUnreferencedInputs {
		return nil, nil
	}
	retired := make([]InputKey, 0)
	for key := range inputs {
		root, exists := reverse[inputDep(key)]
		if !exists {
			root = s.graph.reverseAuthority.Empty()
		}
		size, err := root.Len(s.graph.reverseAuthority, reverseScope(inputDep(key)))
		if err != nil {
			return nil, fmt.Errorf("checking replacement input retirement: %w", err)
		}
		if size == 0 {
			delete(inputs, key)
			retired = append(retired, key)
		}
	}
	sortInputKeys(retired)
	return retired, nil
}

func (s *Session) prepareReplacementCounters() (map[QueryKey]NodeCounters, error) {
	s.graph.mu.RLock()
	if !s.graph.currentValidLocked() || s.graph.current.number != s.baseGeneration {
		s.graph.mu.RUnlock()
		return nil, ErrCommitConflict
	}
	counters, err := cloneCommittedCounters(s.graph.current)
	s.graph.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return s.applyReplacementCounterDeltas(counters)
}

func (s *Session) applyReplacementCounterDeltas(
	counters map[QueryKey]NodeCounters,
) (map[QueryKey]NodeCounters, error) {
	for _, change := range s.bulkCounterDeltas {
		if _, removed := s.removedQueries[change.key]; removed {
			continue
		}
		next, err := addCountersChecked(counters[change.key], change.delta)
		if err != nil {
			return nil, fmt.Errorf("incremental query %s counters: %w", change.key, err)
		}
		counters[change.key] = next
	}
	for key, delta := range s.counterDeltas {
		if _, removed := s.removedQueries[key]; !removed {
			next, err := addCountersChecked(counters[key], delta)
			if err != nil {
				return nil, fmt.Errorf("incremental query %s counters: %w", key, err)
			}
			counters[key] = next
		}
	}
	return counters, nil
}

func (s *Session) prepareIncrementalGraphCommitLocked() (*graphCommitPlan, error) {
	removed := s.incrementalRemovedQueries()
	inputs, nodes := s.incrementalChangedEntries()
	retirementCandidates, err := s.incrementalRetirementCandidates(removed, nodes)
	if err != nil {
		return nil, err
	}
	reverse, err := s.incrementalReverseChanges(removed, nodes)
	if err != nil {
		return nil, err
	}
	retired, err := s.incrementalRetiredInputs(retirementCandidates, reverse)
	if err != nil {
		return nil, err
	}
	counters, err := s.incrementalCounterChanges()
	if err != nil {
		return nil, err
	}
	plan := &graphCommitPlan{
		generation:    s.targetGeneration,
		inputs:        inputs,
		nodes:         nodes,
		reverse:       reverse,
		counters:      counters,
		removed:       removed,
		retiredInputs: retired,
	}
	next, err := buildIncrementalGraphGeneration(s.graph, s.graph.current, plan)
	if err != nil {
		return nil, err
	}
	plan.replacementGeneration = next
	return plan, nil
}

func applyIncrementalCommittedInputs(
	inputs *persistenttree.Txn[committedInputEntry],
	plan *graphCommitPlan,
) error {
	inputKeys := make([]InputKey, 0, len(plan.inputs))
	for key := range plan.inputs {
		inputKeys = append(inputKeys, key)
	}
	sortInputKeys(inputKeys)
	for _, key := range inputKeys {
		entry, err := sealCommittedInputEntry(plan.inputs[key])
		if err != nil || entry.changedAt > plan.generation {
			if err == nil {
				err = fmt.Errorf("changed generation %d exceeds %d", entry.changedAt, plan.generation)
			}
			return fmt.Errorf("incremental committed input %s: %w", key, err)
		}
		inputs.Insert([]byte(key.id.String()), entry)
	}
	for _, key := range plan.retiredInputs {
		inputs.Delete([]byte(key.id.String()))
	}
	return nil
}

func applyIncrementalCommittedReverseDependencies(
	graph *Graph,
	reverse *persistenttree.Txn[orderedset.Root],
	plan *graphCommitPlan,
) error {
	reverseKeys := make([]dependencyKey, 0, len(plan.reverse))
	for key := range plan.reverse {
		reverseKeys = append(reverseKeys, key)
	}
	slices.SortFunc(reverseKeys, compareDependencyKeys)
	for _, key := range reverseKeys {
		change := plan.reverse[key]
		opaque := dependencyTreeKey(key)
		if opaque == "" {
			return fmt.Errorf("incremental committed reverse dependency key is invalid")
		}
		if change.empty {
			reverse.Delete([]byte(opaque))
			continue
		}
		size, err := change.root.Len(graph.reverseAuthority, reverseScope(key))
		if err != nil {
			return fmt.Errorf("incremental committed reverse dependency: %w", err)
		}
		if size == 0 {
			return fmt.Errorf("incremental committed reverse dependency is empty")
		}
		reverse.Insert([]byte(opaque), change.root)
	}
	return nil
}

func buildIncrementalGraphGeneration(
	graph *Graph,
	base *graphGeneration,
	plan *graphCommitPlan,
) (*graphGeneration, error) {
	if graph == nil || plan == nil || !base.valid(graph) || plan.generation != base.number+1 {
		return nil, fmt.Errorf("incremental graph update has invalid provenance")
	}

	inputs := base.inputs.Txn()
	if err := applyIncrementalCommittedInputs(inputs, plan); err != nil {
		return nil, err
	}

	nodes := base.nodes.Txn()
	dirty := base.dirty.Txn()
	counters := base.counters.Txn()
	for _, key := range plan.removed {
		nodes.Delete([]byte(key.id.String()))
		dirty.Delete([]byte(key.id.String()))
		counters.Delete([]byte(key.id.String()))
	}
	for _, key := range sortedNodeEntryKeys(plan.nodes) {
		entry, err := sealCommittedNodeEntry(graph, key, plan.nodes[key], plan.generation)
		if err != nil {
			return nil, fmt.Errorf("incremental committed query %s: %w", key, err)
		}
		nodes.Insert([]byte(key.id.String()), entry)
		if entry.dirty {
			dirty.Insert([]byte(key.id.String()), struct{}{})
		} else {
			dirty.Delete([]byte(key.id.String()))
		}
	}
	counterKeys := make([]QueryKey, 0, len(plan.counters))
	for key := range plan.counters {
		counterKeys = append(counterKeys, key)
	}
	sortQueryKeys(counterKeys)
	for _, key := range counterKeys {
		counters.Insert([]byte(key.id.String()), plan.counters[key])
	}

	reverse := base.reverse.Txn()
	if err := applyIncrementalCommittedReverseDependencies(graph, reverse, plan); err != nil {
		return nil, err
	}

	return newGraphGenerationFromTrees(
		graph,
		plan.generation,
		inputs.Commit(),
		nodes.Commit(),
		reverse.Commit(),
		dirty.Commit(),
		counters.Commit(),
	)
}

func (s *Session) incrementalRemovedQueries() []QueryKey {
	removed := make([]QueryKey, 0, len(s.removedQueries))
	for key := range s.removedQueries {
		removed = append(removed, key)
	}
	sortQueryKeys(removed)
	return removed
}

func (s *Session) incrementalChangedEntries() (
	inputs map[InputKey]inputEntry,
	nodes map[QueryKey]nodeEntry,
) {
	inputs = make(map[InputKey]inputEntry, len(s.inputChanges))
	for key, entry := range s.inputChanges {
		inputs[key] = cloneInputEntry(entry)
	}
	nodes = make(map[QueryKey]nodeEntry, len(s.nodeChanges))
	for key, entry := range s.nodeChanges {
		if _, isRemoved := s.removedQueries[key]; !isRemoved {
			nodes[key] = cloneNodeEntry(&entry)
		}
	}
	return inputs, nodes
}

func (s *Session) incrementalRetirementCandidates(
	removed []QueryKey,
	nodes map[QueryKey]nodeEntry,
) (map[InputKey]struct{}, error) {
	retirementCandidates := map[InputKey]struct{}{}
	if s.graph.options.RetireUnreferencedInputs {
		for key := range s.inputChanges {
			retirementCandidates[key] = struct{}{}
		}
	}
	for _, key := range removed {
		if previous, exists := s.graph.current.nodes.Root().Get([]byte(key.id.String())); exists {
			entry, err := openCommittedNodeEntry(s.graph, key, previous)
			if err != nil {
				return nil, err
			}
			s.markRetirementCandidates(entry.deps, retirementCandidates)
		}
	}
	for _, key := range sortedNodeEntryKeys(nodes) {
		entry := nodes[key]
		committed, exists := s.graph.current.nodes.Root().Get([]byte(key.id.String()))
		var previous nodeEntry
		if exists {
			var err error
			previous, err = openCommittedNodeEntry(s.graph, key, committed)
			if err != nil {
				return nil, err
			}
		}
		if exists && sameDependencyKeys(previous.deps, entry.deps) {
			continue
		}
		if exists {
			s.markRetirementCandidates(previous.deps, retirementCandidates)
		}
	}
	return retirementCandidates, nil
}

func (s *Session) markRetirementCandidates(
	dependencies []dependency,
	retirementCandidates map[InputKey]struct{},
) {
	if !s.graph.options.RetireUnreferencedInputs {
		return
	}
	for _, dep := range dependencies {
		if dep.key.kind == inputDependency {
			retirementCandidates[dep.key.input] = struct{}{}
		}
	}
}

func (s *Session) incrementalReverseChanges(
	removed []QueryKey,
	nodes map[QueryKey]nodeEntry,
) (map[dependencyKey]reverseSetChange, error) {
	editor := reverseSetEditor{
		graph: s.graph,
		roots: map[dependencyKey]orderedset.Root{},
	}
	if err := s.retireRemovedReverseDependencies(&editor, removed); err != nil {
		return nil, err
	}
	if err := s.replaceChangedReverseDependencies(&editor, nodes); err != nil {
		return nil, err
	}
	if err := editor.validateRemovedQueries(removed); err != nil {
		return nil, err
	}
	return editor.changes()
}

func (s *Session) retireRemovedReverseDependencies(
	editor *reverseSetEditor,
	removed []QueryKey,
) error {
	for _, key := range removed {
		committed, exists := s.graph.current.nodes.Root().Get([]byte(key.id.String()))
		if !exists {
			continue
		}
		previous, err := openCommittedNodeEntry(s.graph, key, committed)
		if err != nil {
			return err
		}
		if err := editor.replace(key, previous.deps, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) replaceChangedReverseDependencies(
	editor *reverseSetEditor,
	nodes map[QueryKey]nodeEntry,
) error {
	for _, key := range sortedNodeEntryKeys(nodes) {
		entry := nodes[key]
		committed, exists := s.graph.current.nodes.Root().Get([]byte(key.id.String()))
		var previous nodeEntry
		if exists {
			var err error
			previous, err = openCommittedNodeEntry(s.graph, key, committed)
			if err != nil {
				return err
			}
		}
		if exists && sameDependencyKeys(previous.deps, entry.deps) {
			continue
		}
		var previousDeps []dependency
		if exists {
			previousDeps = previous.deps
		}
		if err := editor.replace(key, previousDeps, entry.deps); err != nil {
			return err
		}
	}
	return nil
}

type reverseSetEditor struct {
	graph   *Graph
	roots   map[dependencyKey]orderedset.Root
	pending map[dependencyKey]map[string]bool
}

func (e *reverseSetEditor) replace(key QueryKey, previous, next []dependency) error {
	leftIndex := 0
	rightIndex := 0
	for leftIndex < len(previous) && rightIndex < len(next) {
		switch comparison := compareDependencyKeys(previous[leftIndex].key, next[rightIndex].key); {
		case comparison < 0:
			if err := e.delete(previous[leftIndex].key, key); err != nil {
				return err
			}
			leftIndex++
		case comparison > 0:
			if err := e.add(next[rightIndex].key, key); err != nil {
				return err
			}
			rightIndex++
		default:
			leftIndex++
			rightIndex++
		}
	}
	for ; leftIndex < len(previous); leftIndex++ {
		if err := e.delete(previous[leftIndex].key, key); err != nil {
			return err
		}
	}
	for ; rightIndex < len(next); rightIndex++ {
		if err := e.add(next[rightIndex].key, key); err != nil {
			return err
		}
	}
	return nil
}

func (e *reverseSetEditor) add(dependency dependencyKey, dependent QueryKey) error {
	return e.setMembership(dependency, dependent, true)
}

func (e *reverseSetEditor) delete(dependency dependencyKey, dependent QueryKey) error {
	return e.setMembership(dependency, dependent, false)
}

func (e *reverseSetEditor) setMembership(dependency dependencyKey, dependent QueryKey, present bool) error {
	root, err := e.baseRoot(dependency)
	if err != nil {
		return err
	}
	current, pending := e.pending[dependency][dependent.id.String()]
	if !pending {
		current, err = root.Contains(e.graph.reverseAuthority, reverseScope(dependency), dependent.id.String())
		if err != nil {
			return fmt.Errorf("reading incremental reverse dependency: %w", err)
		}
	}
	if current == present {
		if present {
			return fmt.Errorf("incremental reverse dependency already contains query %s", dependent)
		}
		return fmt.Errorf("incremental reverse dependency does not contain query %s", dependent)
	}
	if e.pending == nil {
		e.pending = make(map[dependencyKey]map[string]bool)
	}
	if e.pending[dependency] == nil {
		e.pending[dependency] = make(map[string]bool)
	}
	e.pending[dependency][dependent.id.String()] = present
	return nil
}

func (e *reverseSetEditor) root(dependency dependencyKey) (orderedset.Root, error) {
	root, err := e.baseRoot(dependency)
	if err != nil {
		return orderedset.Root{}, err
	}
	if membership := e.pending[dependency]; len(membership) != 0 {
		root, err = root.Update(e.graph.reverseAuthority, reverseScope(dependency), membership)
		if err != nil {
			return orderedset.Root{}, fmt.Errorf("updating incremental reverse dependency: %w", err)
		}
		e.roots[dependency] = root
		delete(e.pending, dependency)
	}
	return root, nil
}

func (e *reverseSetEditor) baseRoot(dependency dependencyKey) (orderedset.Root, error) {
	if root, changed := e.roots[dependency]; changed {
		if err := root.ValidateOwnership(e.graph.reverseAuthority, reverseScope(dependency)); err != nil {
			return orderedset.Root{}, fmt.Errorf("incremental reverse dependency change: %w", err)
		}
		return root, nil
	}
	return e.graph.reverseRootLocked(dependency)
}

func (e *reverseSetEditor) validateRemovedQueries(removed []QueryKey) error {
	for _, key := range removed {
		dependency := queryDep(key)
		root, err := e.root(dependency)
		if err != nil {
			return err
		}
		size, err := root.Len(e.graph.reverseAuthority, reverseScope(dependency))
		if err != nil {
			return fmt.Errorf("checking removed incremental query dependents: %w", err)
		}
		if size != 0 {
			return fmt.Errorf("removed incremental query %s retains dependents", key)
		}
		e.roots[dependency] = root
	}
	return nil
}

func (e *reverseSetEditor) changes() (map[dependencyKey]reverseSetChange, error) {
	for dependency := range e.pending {
		if _, err := e.root(dependency); err != nil {
			return nil, err
		}
	}
	return prepareReverseSetChanges(e.graph.reverseAuthority, e.roots)
}

func (s *Session) incrementalRetiredInputs(
	retirementCandidates map[InputKey]struct{},
	reverse map[dependencyKey]reverseSetChange,
) ([]InputKey, error) {
	retired := make([]InputKey, 0, len(retirementCandidates))
	for key := range retirementCandidates {
		dependency := inputDep(key)
		change, changed := reverse[dependency]
		var root orderedset.Root
		if !changed {
			var err error
			root, err = s.graph.reverseRootLocked(dependency)
			if err != nil {
				return nil, err
			}
		} else {
			root = change.root
		}
		size, err := root.Len(s.graph.reverseAuthority, reverseScope(dependency))
		if err != nil {
			return nil, fmt.Errorf("checking incremental input retirement: %w", err)
		}
		if size == 0 {
			retired = append(retired, key)
		}
	}
	sortInputKeys(retired)
	return retired, nil
}

func (s *Session) incrementalCounterChanges() (map[QueryKey]NodeCounters, error) {
	counters := make(map[QueryKey]NodeCounters, len(s.counterDeltas))
	for key, delta := range s.counterDeltas {
		if _, isRemoved := s.removedQueries[key]; !isRemoved {
			current, _ := s.graph.current.counters.Root().Get([]byte(key.id.String()))
			next, err := addCountersChecked(current, delta)
			if err != nil {
				return nil, fmt.Errorf("incremental query %s counters: %w", key, err)
			}
			counters[key] = next
		}
	}
	return counters, nil
}

func addCountersChecked(left, right NodeCounters) (NodeCounters, error) {
	maximum := ^uint64(0)
	if maximum-left.Executions < right.Executions || maximum-left.CacheHits < right.CacheHits ||
		maximum-left.Backdates < right.Backdates || maximum-left.Changes < right.Changes ||
		maximum-left.Invalidations < right.Invalidations {
		return NodeCounters{}, fmt.Errorf("counter capacity exhausted")
	}
	return addCounters(left, right), nil
}

func (s *Session) commitObservations() ([]InputRevision, error) {
	observations := make(map[InputKey]InputRevision, len(s.observations))
	if err := mergeObservations(observations, sortedObservations(s.observations)); err != nil {
		return nil, err
	}
	for key, entry := range s.nodeChanges {
		if entry.dirty {
			continue
		}
		if _, removed := s.removedQueries[key]; removed {
			continue
		}
		if err := mergeObservations(observations, entry.inputs); err != nil {
			return nil, err
		}
	}
	return sortedObservations(observations), nil
}

func (g *Graph) matchesGeneration(generation uint64) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.currentValidLocked() && g.current.number == generation
}

func addReverseEdges(
	authority *orderedset.Authority,
	reverse map[dependencyKey]orderedset.Root,
	dependent QueryKey,
	dependencies []dependency,
) error {
	for _, dep := range dependencies {
		root, exists := reverse[dep.key]
		if !exists {
			root = authority.Empty()
		}
		next, changed, err := root.Add(authority, reverseScope(dep.key), dependent.id.String())
		if err != nil {
			return fmt.Errorf("building incremental reverse dependencies of query %s: %w", dependent, err)
		}
		if !changed {
			return fmt.Errorf("incremental query %s contains a duplicate dependency", dependent)
		}
		reverse[dep.key] = next
	}
	return nil
}

func prepareReverseSetChanges(
	authority *orderedset.Authority,
	roots map[dependencyKey]orderedset.Root,
) (map[dependencyKey]reverseSetChange, error) {
	changes := make(map[dependencyKey]reverseSetChange, len(roots))
	for dependency, root := range roots {
		size, err := root.Len(authority, reverseScope(dependency))
		if err != nil {
			return nil, fmt.Errorf("authenticating incremental reverse dependency: %w", err)
		}
		changes[dependency] = reverseSetChange{root: root, empty: size == 0}
	}
	return changes, nil
}

func sortedNodeEntryKeys(entries map[QueryKey]nodeEntry) []QueryKey {
	keys := make([]QueryKey, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sortQueryKeys(keys)
	return keys
}
