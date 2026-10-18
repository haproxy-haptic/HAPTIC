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
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type incrementalColdCarrierWaveExecution struct {
	works              []incrementalColdCarrierWork
	sourceTransactions []*incrementalSourceTransactionRender
	sourceFrames       *incrementalColdSourceFrameGeneration
	resultArena        *incrementalColdResultArena
}

type incrementalColdCarrierWavePreparation struct {
	planned *incrementalColdCarrierPlannedLane
	target  *incrementalColdCarrierRenderLane
	err     error
}

type incrementalColdCarrierFinalizedLane struct {
	err error
}

func finalizeIncrementalColdCarrierLane(
	session *incrementalRenderSession,
	vector *preparedIncrementalVectorRender,
) (items []*finalizedIncrementalComponent, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			items = nil
			err = fmt.Errorf("incremental cold carrier lane finalization panic: %v", recovered)
		}
	}()
	if session == nil {
		return nil, errors.New("incremental cold carrier lane finalization is unavailable")
	}
	_, items, err = session.finalizeComponentVectorRender(vector)
	return items, err
}

func finalizeIncrementalColdCarrierLaneIntoArena(
	session *incrementalRenderSession,
	vector *preparedIncrementalVectorRender,
	arena *incrementalColdResultArena,
	slots []int,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("incremental cold carrier lane finalization panic: %v", recovered)
		}
	}()
	if session == nil || arena == nil {
		return errors.New("incremental cold carrier lane finalization is unavailable")
	}
	return session.finalizeComponentVectorRenderIntoArena(vector, arena, slots)
}

type incrementalColdCarrierWaveError struct {
	batchIndex int
	worker     int
	err        error
}

type incrementalColdCarrierWaveCoordinator struct {
	seal     *incrementalColdCarrierWaveCoordinator
	ctx      context.Context
	session  *incrementalRenderSession
	batch    incremental.ColdExactBatch
	graph    *incrementalColdCarrierGraphSchedule
	schedule *incrementalColdCarrierWaves

	originalHTTPExecuted map[incremental.QueryKey][]incrementalHTTPEffect
	stagedHTTPExecuted   map[incremental.QueryKey][]incrementalHTTPEffect
	completed            map[string]*incrementalGroupIndex
	pending              map[string]struct{}
	completedRoots       []incremental.ExactValueRoot
	completedQueries     []bool

	mu                 sync.Mutex
	cond               *sync.Cond
	wave               int
	prepared           *incrementalColdCarrierWaveExecution
	arrived            []bool
	arrivals           int
	waveErrors         []incrementalColdCarrierWaveError
	terminal           error
	closed             bool
	httpStaged         bool
	sourceTransactions bool
	stopContext        func() bool
}

type incrementalColdCarrierWavesLifecycle struct {
	seal        *incrementalColdCarrierWavesLifecycle
	coordinator *incrementalColdCarrierWaveCoordinator
	worker      int
	offsets     []int
	items       []incrementalColdCarrierPlannedItem
	components  []*incrementalComponent

	mu       sync.Mutex
	wave     int
	loaded   bool
	sealing  bool
	terminal error
	inner    *incrementalColdCarrierLifecycle
}

func (r *incrementalRenderSession) evaluateColdComponentCarrierGraphWaves(
	ctx context.Context,
	renderer templating.IncrementalComponentVectorCarrierWavesRenderer,
	schedule *incrementalColdCarrierGraphSchedule,
) error {
	if sourceRenderer, available := any(renderer).(templating.IncrementalComponentSourceTransactionsRenderer); available && sourceRenderer.IncrementalComponentSourceTransactionsEligibility() {
		return r.evaluateColdComponentSourceTransactionGraphWaves(ctx, sourceRenderer, schedule)
	}
	if r == nil || r.graphSession == nil || renderer == nil || !schedule.executable() {
		return errors.New("incremental cold carrier wave graph execution is incomplete")
	}
	completedRoots := make([]incremental.ExactValueRoot, len(schedule.keys))
	results, err := r.graphSession.EvaluateAllColdExactBatch(
		ctx,
		func(batchCtx context.Context, batch incremental.ColdExactBatch) error {
			return r.runColdCarrierWaveBatch(batchCtx, batch, renderer, schedule, completedRoots)
		},
		schedule.keys...,
	)
	if err != nil {
		return err
	}
	if err := validateColdCarrierWaveGraphResults(schedule, results, completedRoots); err != nil {
		return err
	}
	return context.Cause(ctx)
}

func (r *incrementalRenderSession) runColdCarrierWaveBatch(
	batchCtx context.Context,
	batch incremental.ColdExactBatch,
	renderer templating.IncrementalComponentVectorCarrierWavesRenderer,
	schedule *incrementalColdCarrierGraphSchedule,
	completedRoots []incremental.ExactValueRoot,
) error {
	plan, planErr := r.planColdComponentCarriers(batch)
	if planErr != nil {
		return planErr
	}
	if plan == nil || plan.logicalQueries != batch.Len() || batch.Len() != len(schedule.keys) {
		return errors.New("incremental cold carrier wave runtime plan is incomplete")
	}
	for index, key := range schedule.keys {
		if batch.Query(index).Key() != key {
			return errors.New("incremental cold carrier wave runtime query order changed")
		}
	}
	waves, waveErr := r.planIncrementalColdCarrierWaves(
		plan,
		schedule,
		incrementalColdCarrierWorkerLimit(runtime.GOMAXPROCS(0)),
	)
	if waveErr != nil {
		return fmt.Errorf("planning incremental cold carrier waves: %w", waveErr)
	}
	coordinator, coordinatorErr := newIncrementalColdCarrierWaveCoordinator(
		batchCtx,
		r,
		batch,
		schedule,
		waves,
	)
	if coordinatorErr != nil {
		return coordinatorErr
	}
	defer coordinator.close()
	if runErr := coordinator.run(renderer); runErr != nil {
		return runErr
	}
	copy(completedRoots, coordinator.completedRoots)
	return nil
}

func validateColdCarrierWaveGraphResults(
	schedule *incrementalColdCarrierGraphSchedule,
	results []incremental.ExactResult,
	completedRoots []incremental.ExactValueRoot,
) error {
	if len(results) != len(schedule.keys) {
		return errors.New("incremental cold carrier wave graph returned an incomplete result set")
	}
	for index := range results {
		if results[index].Key != schedule.keys[index] {
			return errors.New("incremental cold carrier wave graph returned results out of order")
		}
		same, sameErr := results[index].Value.SameRoot(completedRoots[index])
		if sameErr != nil || !same {
			return errors.New("incremental cold carrier wave graph returned a different exact root")
		}
	}
	return nil
}

func newIncrementalColdCarrierWaveCoordinator(
	ctx context.Context,
	session *incrementalRenderSession,
	batch incremental.ColdExactBatch,
	graph *incrementalColdCarrierGraphSchedule,
	schedule *incrementalColdCarrierWaves,
) (*incrementalColdCarrierWaveCoordinator, error) {
	if ctx == nil || session == nil || session.state == nil || session.httpExecuted == nil ||
		batch.Len() == 0 || graph == nil || graph.seal != graph || schedule == nil ||
		schedule.seal != schedule || len(schedule.workers) == 0 || batch.Len() != len(graph.keys) {
		return nil, errors.New("incremental cold carrier wave coordinator is incomplete")
	}
	if err := schedule.validate(graph); err != nil {
		return nil, err
	}
	coordinator := &incrementalColdCarrierWaveCoordinator{
		ctx:                  ctx,
		session:              session,
		batch:                batch,
		graph:                graph,
		schedule:             schedule,
		originalHTTPExecuted: session.httpExecuted,
		stagedHTTPExecuted:   make(map[incremental.QueryKey][]incrementalHTTPEffect),
		completed:            make(map[string]*incrementalGroupIndex, len(graph.groupOrder)),
		pending:              make(map[string]struct{}, len(graph.groupOrder)),
		completedRoots:       make([]incremental.ExactValueRoot, len(graph.keys)),
		completedQueries:     make([]bool, len(graph.keys)),
		arrived:              make([]bool, len(schedule.workers)),
	}
	for _, group := range graph.groupOrder {
		coordinator.pending[group] = struct{}{}
	}
	coordinator.seal = coordinator
	coordinator.cond = sync.NewCond(&coordinator.mu)
	session.httpExecuted = coordinator.stagedHTTPExecuted
	coordinator.httpStaged = true
	coordinator.stopContext = context.AfterFunc(ctx, func() {
		coordinator.fail(context.Cause(ctx))
	})
	return coordinator, nil
}

func (c *incrementalColdCarrierWaveCoordinator) run(
	renderer templating.IncrementalComponentVectorCarrierWavesRenderer,
) error {
	if !c.valid() || renderer == nil {
		return errors.New("incremental cold carrier wave coordinator has invalid provenance")
	}
	lifecycles := make([]*incrementalColdCarrierWavesLifecycle, len(c.schedule.workers))
	for workerIndex := range c.schedule.workers {
		lifecycle, err := newIncrementalColdCarrierWavesLifecycle(c, workerIndex)
		if err != nil {
			c.fail(err)
			return err
		}
		lifecycles[workerIndex] = lifecycle
	}
	var renderGroup errgroup.Group
	for workerIndex := range lifecycles {
		lifecycle := lifecycles[workerIndex]
		input := templating.IncrementalComponentVectorCarrierWavesInput{
			Waves:     incrementalColdCarrierTemplatingWaves(&c.schedule.workers[workerIndex]),
			Lifecycle: lifecycle,
		}
		renderGroup.Go(func() error {
			err := renderIncrementalColdCarrierWaves(c.ctx, renderer, input)
			if err != nil {
				batchIndex, mapped := lifecycle.remapError(err)
				lifecycle.fail(batchIndex, mapped)
				return mapped
			}
			if err := lifecycle.validateComplete(); err != nil {
				lifecycle.fail(math.MaxInt, err)
				return err
			}
			return nil
		})
	}
	renderErr := renderGroup.Wait()
	c.mu.Lock()
	terminal := c.terminal
	complete := c.wave == len(c.schedule.waves)
	c.mu.Unlock()
	if terminal != nil {
		return terminal
	}
	if renderErr != nil {
		return renderErr
	}
	if !complete {
		return errors.New("incremental cold carrier wave renderer stopped before the final barrier")
	}
	for batchIndex, completed := range c.completedQueries {
		if !completed {
			return fmt.Errorf("incremental cold carrier wave graph omitted batch item %d", batchIndex)
		}
	}
	return nil
}

func renderIncrementalColdCarrierWaves(
	ctx context.Context,
	renderer templating.IncrementalComponentVectorCarrierWavesRenderer,
	input templating.IncrementalComponentVectorCarrierWavesInput,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("incremental cold carrier wave renderer panic: %v", recovered)
		}
	}()
	return renderer.RenderIncrementalComponentVectorCarrierWaves(ctx, input)
}

func incrementalColdCarrierTemplatingWaves(
	worker *incrementalColdCarrierPlannedWorker,
) []templating.IncrementalComponentVectorCarrierWave {
	if worker == nil {
		return nil
	}
	waves := make([]templating.IncrementalComponentVectorCarrierWave, len(worker.waves))
	for waveIndex := range worker.waves {
		planned := &worker.waves[waveIndex]
		lanes := make([]templating.IncrementalComponentVectorCarrierWaveLane, len(planned.lanes))
		for laneIndex := range planned.lanes {
			lanes[laneIndex] = templating.IncrementalComponentVectorCarrierWaveLane{
				TemplateName: planned.lanes[laneIndex].entryPoint,
				Count:        len(planned.lanes[laneIndex].items),
			}
		}
		waves[waveIndex].Lanes = lanes
	}
	return waves
}

func (c *incrementalColdCarrierWaveCoordinator) valid() bool {
	return c != nil && c.seal == c && c.ctx != nil && c.session != nil && c.batch.Len() > 0 &&
		c.graph != nil && c.graph.seal == c.graph && c.schedule != nil && c.schedule.seal == c.schedule &&
		c.cond != nil
}

func (c *incrementalColdCarrierWaveCoordinator) close() {
	if c == nil {
		return
	}
	if c.stopContext != nil {
		c.stopContext()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.abortPreparedLocked(errors.New("incremental cold carrier wave coordinator closed"))
	c.session.httpExecuted = c.originalHTTPExecuted
	c.httpStaged = false
	c.cond.Broadcast()
}

func (c *incrementalColdCarrierWaveCoordinator) fail(cause error) {
	if c == nil {
		return
	}
	if cause == nil {
		cause = errors.New("incremental cold carrier wave execution aborted")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terminal == nil {
		c.terminal = cause
	}
	c.abortPreparedLocked(c.terminal)
	c.cond.Broadcast()
}

func (c *incrementalColdCarrierWaveCoordinator) abortPreparedLocked(cause error) {
	if c.prepared == nil {
		return
	}
	abortIncrementalColdCarrierWorks(c.prepared.works, cause)
	for _, render := range c.prepared.sourceTransactions {
		if render != nil && render.execution != nil {
			render.execution.Abort(-1, cause)
		}
	}
	if c.prepared.sourceFrames != nil {
		c.prepared.sourceFrames.revoke()
	}
	if c.prepared.resultArena != nil {
		c.prepared.resultArena.revoke()
	}
}

func (c *incrementalColdCarrierWaveCoordinator) loadWave(
	worker int,
	wave int,
) ([]templating.IncrementalComponentVectorCarrierLane, *incrementalColdCarrierLifecycle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid() || c.closed || worker < 0 || worker >= len(c.schedule.workers) ||
		wave < 0 || wave >= len(c.schedule.waves) {
		return nil, nil, errors.New("incremental cold carrier wave load has invalid provenance")
	}
	if c.terminal != nil {
		return nil, nil, c.terminal
	}
	if wave != c.wave {
		return nil, nil, fmt.Errorf("incremental cold carrier wave %d is unavailable", wave)
	}
	if c.prepared == nil {
		prepared, err := c.prepareWaveLocked(wave)
		if err != nil {
			c.terminal = err
			c.abortPreparedLocked(err)
			c.cond.Broadcast()
			return nil, nil, err
		}
		c.prepared = prepared
	}
	work := &c.prepared.works[worker]
	lanes := make([]templating.IncrementalComponentVectorCarrierLane, len(work.lanes))
	for laneIndex := range work.lanes {
		lane := &work.lanes[laneIndex]
		if lane.vector == nil {
			return nil, nil, errors.New("incremental cold carrier wave load has an incomplete lane")
		}
		lanes[laneIndex] = templating.IncrementalComponentVectorCarrierLane{
			TemplateName: lane.entryPoint,
			Count:        len(lane.prepared),
			Bindings:     lane.vector.columns,
			Contexts:     lane.vector.contexts,
		}
	}
	return lanes, work.lifecycle, nil
}

func (c *incrementalColdCarrierWaveCoordinator) prepareWaveLocked(
	waveIndex int,
) (*incrementalColdCarrierWaveExecution, error) {
	if c.terminal != nil || waveIndex != c.wave || c.prepared != nil || len(c.stagedHTTPExecuted) != 0 {
		return nil, errors.New("incremental cold carrier wave preparation is unavailable")
	}
	if cause := context.Cause(c.ctx); cause != nil {
		return nil, cause
	}
	authority, err := newIncrementalColdGraphAuthority(c.session, c.completed)
	if err != nil {
		return nil, err
	}
	runCtx := context.WithValue(c.ctx, incrementalRunContextKey{}, c.session)
	waveCtx := context.WithValue(runCtx, incrementalColdGraphContextKey{}, authority)
	execution := &incrementalColdCarrierWaveExecution{
		works: make([]incrementalColdCarrierWork, len(c.schedule.workers)),
	}
	if c.sourceTransactions {
		execution.sourceTransactions = make([]*incrementalSourceTransactionRender, len(c.schedule.workers))
	}
	batchIndexes, keys, err := c.waveBatchSlots(waveIndex)
	if err != nil {
		return nil, err
	}
	if len(batchIndexes) == 0 {
		return execution, nil
	}
	resultArena, err := newIncrementalColdResultArena(c.session, waveIndex, batchIndexes, keys)
	if err != nil {
		return nil, err
	}
	execution.resultArena = resultArena
	sourceFrames, err := newIncrementalColdSourceFrameGeneration(
		c.session,
		waveIndex,
		c.batch.Len(),
	)
	if err != nil {
		resultArena.revoke()
		return nil, err
	}
	execution.sourceFrames = sourceFrames
	abort := func(cause error) {
		abortColdCarrierWaveExecution(execution, cause)
	}
	preparations, err := c.bindWaveLanes(waveIndex, execution, sourceFrames)
	if err != nil {
		abort(err)
		return nil, err
	}
	if err := sourceFrames.sealGeneration(); err != nil {
		abort(err)
		return nil, err
	}
	if c.sourceTransactions {
		if err := c.prepareSourceTransactionWave(
			waveIndex, waveCtx, execution, sourceFrames, resultArena,
		); err != nil {
			abort(err)
			return nil, err
		}
		return execution, nil
	}
	if err := c.prepareCarrierWave(waveIndex, waveCtx, execution, sourceFrames, preparations); err != nil {
		abort(err)
		return nil, err
	}
	return execution, nil
}

func abortColdCarrierWaveExecution(execution *incrementalColdCarrierWaveExecution, cause error) {
	abortIncrementalColdCarrierWorks(execution.works, cause)
	for _, render := range execution.sourceTransactions {
		if render != nil && render.execution != nil {
			render.execution.Abort(-1, cause)
		}
	}
	execution.sourceFrames.revoke()
	execution.resultArena.revoke()
	for workerIndex := range execution.works {
		for laneIndex := range execution.works[workerIndex].lanes {
			vector := execution.works[workerIndex].lanes[laneIndex].vector
			if vector != nil && vector.execution != nil {
				vector.execution.Abort(-1, cause)
			}
		}
	}
}

func (c *incrementalColdCarrierWaveCoordinator) waveBatchSlots(
	waveIndex int,
) ([]int, []incremental.QueryKey, error) {
	plannedWave := &c.schedule.waves[waveIndex]
	batchIndexes := make([]int, 0, plannedWave.logicalQueries)
	for laneIndex := range plannedWave.lanes {
		for itemIndex := range plannedWave.lanes[laneIndex].items {
			batchIndexes = append(batchIndexes, plannedWave.lanes[laneIndex].items[itemIndex].batchIndex)
		}
	}
	if len(batchIndexes) == 0 {
		return nil, nil, c.validateEmptyWaveSlots(waveIndex, plannedWave)
	}
	slices.Sort(batchIndexes)
	keys := make([]incremental.QueryKey, len(batchIndexes))
	for slot, batchIndex := range batchIndexes {
		if batchIndex < 0 || batchIndex >= c.batch.Len() {
			return nil, nil, errors.New("incremental cold carrier wave has an invalid result slot")
		}
		keys[slot] = c.batch.Query(batchIndex).Key()
	}
	return batchIndexes, keys, nil
}

func (c *incrementalColdCarrierWaveCoordinator) validateEmptyWaveSlots(
	waveIndex int,
	plannedWave *incrementalColdCarrierPlannedWave,
) error {
	if plannedWave.logicalQueries != 0 || len(plannedWave.lanes) != 0 {
		return fmt.Errorf(
			"incremental cold carrier wave %d has no result slots for %d logical queries in %d lanes",
			waveIndex, plannedWave.logicalQueries, len(plannedWave.lanes),
		)
	}
	for workerIndex := range c.schedule.workers {
		if len(c.schedule.workers[workerIndex].waves[waveIndex].lanes) != 0 {
			return errors.New("incremental cold carrier empty wave has worker lanes")
		}
	}
	return nil
}

func (c *incrementalColdCarrierWaveCoordinator) bindWaveLanes(
	waveIndex int,
	execution *incrementalColdCarrierWaveExecution,
	sourceFrames *incrementalColdSourceFrameGeneration,
) ([]incrementalColdCarrierWavePreparation, error) {
	preparations := make([]incrementalColdCarrierWavePreparation, 0)
	for workerIndex := range c.schedule.workers {
		planned := &c.schedule.workers[workerIndex].waves[waveIndex]
		work := &execution.works[workerIndex]
		work.lanes = make([]incrementalColdCarrierRenderLane, len(planned.lanes))
		for laneIndex := range planned.lanes {
			plannedLane := &planned.lanes[laneIndex]
			lane := &work.lanes[laneIndex]
			lane.entryPoint = plannedLane.entryPoint
			lane.component = plannedLane.component
			lane.indexes = make([]int, len(plannedLane.items))
			lane.arenaSlots = make([]int, len(plannedLane.items))
			lane.prepared = make([]*preparedIncrementalComponent, len(plannedLane.items))
			if !c.sourceTransactions {
				preparations = append(preparations, incrementalColdCarrierWavePreparation{
					planned: plannedLane, target: lane,
				})
			}
			for itemIndex := range plannedLane.items {
				item := plannedLane.items[itemIndex]
				slot, exists := execution.resultArena.slotForBatchIndex(item.batchIndex)
				if !exists || execution.resultArena.keys[slot] != item.queryKey {
					return nil, errors.New("incremental cold carrier wave result slot changed")
				}
				lane.arenaSlots[itemIndex] = slot
				lane.indexes[itemIndex] = item.batchIndex
				if err := sourceFrames.bind(
					item.batchIndex,
					item.queryKey,
					plannedLane.component,
					item.source,
					item.namespace,
					item.name,
				); err != nil {
					return nil, err
				}
			}
		}
	}
	return preparations, nil
}

func (c *incrementalColdCarrierWaveCoordinator) prepareSourceTransactionWave(
	waveIndex int,
	waveCtx context.Context,
	execution *incrementalColdCarrierWaveExecution,
	sourceFrames *incrementalColdSourceFrameGeneration,
	resultArena *incrementalColdResultArena,
) error {
	preparationErrors := make([]error, len(c.schedule.workers))
	parallelism := min(len(c.schedule.workers), incrementalColdCarrierWorkerLimit(runtime.GOMAXPROCS(0)))
	var preparationGroup sync.WaitGroup
	for slot := range parallelism {
		preparationGroup.Add(1)
		go func() {
			defer preparationGroup.Done()
			for workerIndex := slot; workerIndex < len(c.schedule.workers); workerIndex += parallelism {
				render, lifecycle, prepareErr := c.prepareSourceTransactionWorker(
					waveIndex,
					waveCtx,
					sourceFrames,
					&c.schedule.workers[workerIndex].waves[waveIndex],
					resultArena,
				)
				execution.sourceTransactions[workerIndex] = render
				execution.works[workerIndex].lifecycle = lifecycle
				preparationErrors[workerIndex] = prepareErr
			}
		}()
	}
	preparationGroup.Wait()
	for workerIndex, prepareErr := range preparationErrors {
		if prepareErr != nil {
			return fmt.Errorf("preparing incremental cold source transaction worker %d: %w", workerIndex, prepareErr)
		}
	}
	if len(c.stagedHTTPExecuted) != 0 {
		return errors.New("incremental cold source transaction left unclaimed execution records")
	}
	return nil
}

func (c *incrementalColdCarrierWaveCoordinator) prepareCarrierWave(
	waveIndex int,
	waveCtx context.Context,
	execution *incrementalColdCarrierWaveExecution,
	sourceFrames *incrementalColdSourceFrameGeneration,
	preparations []incrementalColdCarrierWavePreparation,
) error {
	parallelism := min(len(preparations), incrementalColdCarrierWorkerLimit(runtime.GOMAXPROCS(0)))
	var preparationGroup sync.WaitGroup
	for slot := range parallelism {
		preparationGroup.Add(1)
		go func() {
			defer preparationGroup.Done()
			for preparationIndex := slot; preparationIndex < len(preparations); preparationIndex += parallelism {
				preparation := &preparations[preparationIndex]
				preparation.err = c.prepareWaveLane(
					waveIndex,
					waveCtx,
					sourceFrames,
					preparation.planned,
					preparation.target,
				)
			}
		}()
	}
	preparationGroup.Wait()
	for index := range preparations {
		if preparations[index].err != nil {
			return preparations[index].err
		}
	}
	for workerIndex := range execution.works {
		work := &execution.works[workerIndex]
		if len(work.lanes) == 0 {
			continue
		}
		lifecycle, err := newIncrementalColdCarrierLifecycle(work.lanes)
		if err != nil {
			return err
		}
		work.lifecycle = lifecycle
	}
	if len(c.stagedHTTPExecuted) != 0 {
		return errors.New("incremental cold carrier wave left unclaimed execution records")
	}
	return nil
}

func (c *incrementalColdCarrierWaveCoordinator) prepareWaveLane(
	waveIndex int,
	waveCtx context.Context,
	sourceFrames *incrementalColdSourceFrameGeneration,
	planned *incrementalColdCarrierPlannedLane,
	lane *incrementalColdCarrierRenderLane,
) error {
	if sourceFrames == nil || planned == nil || lane == nil || planned.component == nil ||
		len(lane.prepared) != len(planned.items) || len(lane.indexes) != len(planned.items) {
		return errors.New("incremental cold carrier wave lane preparation is incomplete")
	}
	for itemIndex := range planned.items {
		if cause := context.Cause(c.ctx); cause != nil {
			return cause
		}
		item := planned.items[itemIndex]
		query := c.batch.Query(item.batchIndex)
		if query.Key() != item.queryKey {
			return fmt.Errorf("incremental cold carrier wave %d query order changed", waveIndex)
		}
		frames, err := sourceFrames.refsFor(
			item.batchIndex,
			item.queryKey,
			planned.component,
			item.source,
			item.namespace,
			item.name,
		)
		if err != nil {
			return attributeToWatchedObject(item.queryKey, err)
		}
		prepared, _, _, err := c.session.prepareComponentInputsDetachedWithSourceFrames(
			waveCtx,
			query,
			planned.component,
			item.source,
			item.namespace,
			item.name,
			frames,
		)
		if err != nil {
			return attributeToWatchedObject(item.queryKey, err)
		}
		if prepared == nil {
			return fmt.Errorf(
				"incremental cold carrier wave query %s became inactive after its shape was sealed",
				item.queryKey,
			)
		}
		lane.indexes[itemIndex] = item.batchIndex
		lane.prepared[itemIndex] = prepared
	}
	vector, err := c.session.prepareComponentVectorRender(
		waveCtx,
		planned.component,
		lane.prepared,
	)
	if err != nil {
		return fmt.Errorf("preparing incremental cold carrier wave lane %q: %w", lane.entryPoint, err)
	}
	if err := validatePreparedColdComponentVector(vector, len(lane.prepared)); err != nil {
		wrapped := fmt.Errorf("preparing incremental cold carrier wave lane %q: %w", lane.entryPoint, err)
		vector.execution.Abort(-1, wrapped)
		return wrapped
	}
	lane.vector = vector
	return nil
}

func (c *incrementalColdCarrierWaveCoordinator) sealWave(worker, wave int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid() || c.closed || worker < 0 || worker >= len(c.arrived) {
		return errors.New("incremental cold carrier wave barrier has invalid provenance")
	}
	if c.terminal != nil {
		return c.terminal
	}
	if wave != c.wave || c.prepared == nil || c.arrived[worker] {
		return fmt.Errorf("incremental cold carrier wave %d barrier is unavailable", wave)
	}
	c.arrived[worker] = true
	c.arrivals++
	if c.arrivals == len(c.arrived) {
		if len(c.waveErrors) > 0 {
			c.terminal = selectIncrementalColdCarrierWaveError(c.waveErrors)
			c.abortPreparedLocked(c.terminal)
		} else if err := c.finishWaveLocked(wave); err != nil {
			c.terminal = err
			c.abortPreparedLocked(err)
		} else {
			c.wave++
			c.prepared = nil
			c.arrived = make([]bool, len(c.schedule.workers))
			c.arrivals = 0
			c.waveErrors = nil
		}
		c.cond.Broadcast()
	}
	for c.terminal == nil && c.wave == wave {
		c.cond.Wait()
	}
	return c.terminal
}

func (c *incrementalColdCarrierWaveCoordinator) workerFailed(
	worker int,
	wave int,
	batchIndex int,
	cause error,
) {
	if cause == nil {
		cause = errors.New("incremental cold carrier wave worker aborted")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid() || c.closed || c.terminal != nil {
		return
	}
	if worker < 0 || worker >= len(c.arrived) || wave != c.wave {
		c.terminal = cause
		c.abortPreparedLocked(cause)
		c.cond.Broadcast()
		return
	}
	if c.arrived[worker] {
		for index := range c.waveErrors {
			if c.waveErrors[index].worker == worker && batchIndex < c.waveErrors[index].batchIndex {
				c.waveErrors[index] = incrementalColdCarrierWaveError{
					batchIndex: batchIndex,
					worker:     worker,
					err:        cause,
				}
			}
		}
		return
	}
	c.arrived[worker] = true
	c.arrivals++
	c.waveErrors = append(c.waveErrors, incrementalColdCarrierWaveError{
		batchIndex: batchIndex,
		worker:     worker,
		err:        cause,
	})
	if c.arrivals == len(c.arrived) {
		c.terminal = selectIncrementalColdCarrierWaveError(c.waveErrors)
		c.abortPreparedLocked(c.terminal)
		c.cond.Broadcast()
	}
}

func (c *incrementalColdCarrierWaveCoordinator) finishWaveLocked(waveIndex int) error {
	if c.prepared == nil || waveIndex != c.wave || waveIndex < 0 || waveIndex >= len(c.schedule.waves) ||
		!c.httpStaged {
		return errors.New("incremental cold carrier wave finalization is unavailable")
	}
	if cause := context.Cause(c.ctx); cause != nil {
		return cause
	}
	plannedWave := &c.schedule.waves[waveIndex]
	arena := c.prepared.resultArena
	if plannedWave.logicalQueries == 0 {
		return c.finishEmptyWaveLocked(waveIndex, plannedWave, arena)
	}
	if err := arena.validateAuthority(); err != nil || arena.wave != waveIndex {
		return errors.New("incremental cold carrier wave result arena is unavailable")
	}
	if c.sourceTransactions {
		if err := c.finalizeSourceTransactionWave(arena); err != nil {
			return err
		}
	} else if err := c.finalizeCarrierWaveLanes(arena); err != nil {
		return err
	}
	if len(arena.fresh) != plannedWave.logicalQueries {
		return fmt.Errorf(
			"incremental cold carrier wave %d finalized %d queries, want %d",
			waveIndex,
			len(arena.fresh),
			plannedWave.logicalQueries,
		)
	}
	indexes := arena.batchIndexes
	stageResults, err := c.collectWaveStageResults(waveIndex, arena, indexes)
	if err != nil {
		return err
	}
	return c.publishWaveResultsLocked(waveIndex, plannedWave, arena, indexes, stageResults)
}

func (c *incrementalColdCarrierWaveCoordinator) finishEmptyWaveLocked(
	waveIndex int,
	plannedWave *incrementalColdCarrierPlannedWave,
	arena *incrementalColdResultArena,
) error {
	if len(plannedWave.lanes) != 0 || arena != nil || c.prepared.sourceFrames != nil {
		return errors.New("incremental cold carrier empty wave has execution state")
	}
	for workerIndex := range c.prepared.works {
		if len(c.prepared.works[workerIndex].lanes) != 0 ||
			c.sourceTransactions && c.prepared.sourceTransactions[workerIndex] != nil {
			return errors.New("incremental cold carrier empty wave has worker state")
		}
	}
	c.session.httpExecuted = c.originalHTTPExecuted
	c.httpStaged = false
	if err := c.session.applyColdGraphStageResults(
		waveIndex,
		plannedWave.groups,
		nil,
		c.completed,
		c.pending,
	); err != nil {
		return err
	}
	c.session.httpExecuted = c.stagedHTTPExecuted
	c.httpStaged = true
	if len(c.stagedHTTPExecuted) != 0 {
		return errors.New("incremental cold carrier empty wave retained staged HTTP effects")
	}
	return nil
}

func (c *incrementalColdCarrierWaveCoordinator) finalizeSourceTransactionWave(
	arena *incrementalColdResultArena,
) error {
	finalizedTransactions := make([]error, len(c.prepared.sourceTransactions))
	var finalizeGroup errgroup.Group
	for workerIndex := range c.prepared.sourceTransactions {
		render := c.prepared.sourceTransactions[workerIndex]
		if render == nil {
			continue
		}
		finalizeGroup.Go(func() error {
			finalizedTransactions[workerIndex] = c.session.finalizeSourceTransactionRenderIntoArena(render, arena)
			return nil
		})
	}
	_ = finalizeGroup.Wait()
	for workerIndex, finalizeErr := range finalizedTransactions {
		if finalizeErr != nil {
			return fmt.Errorf("finishing incremental cold source transaction worker %d: %w", workerIndex, finalizeErr)
		}
	}
	return nil
}

func (c *incrementalColdCarrierWaveCoordinator) finalizeCarrierWaveLanes(
	arena *incrementalColdResultArena,
) error {
	finalizedLanes := make([][]incrementalColdCarrierFinalizedLane, len(c.prepared.works))
	var finalizeGroup errgroup.Group
	for workerIndex := range c.prepared.works {
		lanes := c.prepared.works[workerIndex].lanes
		finalizedLanes[workerIndex] = make([]incrementalColdCarrierFinalizedLane, len(lanes))
		if len(lanes) == 0 {
			continue
		}
		finalizeGroup.Go(func() error {
			for laneIndex := range lanes {
				err := finalizeIncrementalColdCarrierLaneIntoArena(
					c.session,
					lanes[laneIndex].vector,
					arena,
					lanes[laneIndex].arenaSlots,
				)
				finalizedLanes[workerIndex][laneIndex] = incrementalColdCarrierFinalizedLane{
					err: err,
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
	}
	_ = finalizeGroup.Wait()
	for workerIndex := range c.prepared.works {
		work := &c.prepared.works[workerIndex]
		for laneIndex := range work.lanes {
			lane := &work.lanes[laneIndex]
			laneResult := &finalizedLanes[workerIndex][laneIndex]
			if laneResult.err != nil {
				return fmt.Errorf("finishing incremental cold carrier wave lane %q: %w", lane.entryPoint, laneResult.err)
			}
		}
	}
	return nil
}

func (c *incrementalColdCarrierWaveCoordinator) collectWaveStageResults(
	waveIndex int,
	arena *incrementalColdResultArena,
	indexes []int,
) ([]incremental.ExactResult, error) {
	if c.sourceTransactions {
		stageResults, err := c.session.completeStagedColdResultArenaWave(c.batch, arena, c.completedQueries)
		if err != nil {
			return nil, fmt.Errorf("completing incremental cold source transaction wave %d: %w", waveIndex, err)
		}
		return stageResults, nil
	}
	stageResults := make([]incremental.ExactResult, len(indexes))
	for resultIndex, batchIndex := range indexes {
		query := c.batch.Query(batchIndex)
		fresh := &arena.fresh[resultIndex]
		if fresh.key != query.Key() || c.completedQueries[batchIndex] {
			return nil, fmt.Errorf(
				"incremental cold carrier wave query %s has an invalid finalized result",
				query.Key(),
			)
		}
		if err := validatePendingAuthenticatedFreshComponentResult(fresh, fresh.key); err != nil {
			return nil, fmt.Errorf(
				"authenticating pending incremental cold carrier wave query %s: %w",
				query.Key(),
				err,
			)
		}
		root, err := query.Complete(fresh.encoded)
		if err != nil {
			return nil, fmt.Errorf("completing incremental cold carrier wave query %s: %w", query.Key(), err)
		}
		if err := c.session.authenticateColdFreshComponent(fresh.key, fresh.encoded, fresh, root); err != nil {
			return nil, fmt.Errorf("authenticating incremental cold carrier wave query %s: %w", query.Key(), err)
		}
		stageResults[resultIndex] = incremental.ExactResult{Key: query.Key(), Value: root}
	}
	return stageResults, nil
}

func (c *incrementalColdCarrierWaveCoordinator) publishWaveResultsLocked(
	waveIndex int,
	plannedWave *incrementalColdCarrierPlannedWave,
	arena *incrementalColdResultArena,
	indexes []int,
	stageResults []incremental.ExactResult,
) error {
	if len(stageResults) > 0 {
		stage := &incrementalColdCarrierStageResult{indexes: indexes, results: stageResults}
		if err := validateIncrementalColdCarrierGraphStageOrder(c.graph, stage); err != nil {
			return fmt.Errorf("authenticating incremental cold carrier wave %d order: %w", waveIndex, err)
		}
	}
	c.session.httpExecuted = c.originalHTTPExecuted
	c.httpStaged = false
	if len(arena.fresh) > 0 {
		if err := c.session.installColdResultArena(arena); err != nil {
			return fmt.Errorf("installing incremental cold carrier wave %d: %w", waveIndex, err)
		}
	}
	if err := c.session.applyColdGraphStageResults(
		waveIndex,
		plannedWave.groups,
		stageResults,
		c.completed,
		c.pending,
	); err != nil {
		return err
	}
	if len(stageResults) > 0 {
		if err := c.batch.SealWave(stageResults...); err != nil {
			return fmt.Errorf("sealing incremental cold carrier wave %d: %w", waveIndex, err)
		}
	}
	for resultIndex, batchIndex := range indexes {
		c.completedQueries[batchIndex] = true
		c.completedRoots[batchIndex] = stageResults[resultIndex].Value
	}
	c.session.httpExecuted = c.stagedHTTPExecuted
	c.httpStaged = true
	if len(c.stagedHTTPExecuted) != 0 {
		return errors.New("incremental cold carrier wave retained staged HTTP effects")
	}
	for _, key := range arena.keys {
		if _, retained := c.session.freshResults[key]; retained {
			return errors.New("incremental cold carrier wave retained a fresh result")
		}
	}
	c.prepared.sourceFrames.revoke()
	arena.revoke()
	return nil
}

func newIncrementalColdCarrierWavesLifecycle(
	coordinator *incrementalColdCarrierWaveCoordinator,
	worker int,
) (*incrementalColdCarrierWavesLifecycle, error) {
	if coordinator == nil || !coordinator.valid() || worker < 0 || worker >= len(coordinator.schedule.workers) {
		return nil, errors.New("incremental cold carrier waves lifecycle is incomplete")
	}
	planned := &coordinator.schedule.workers[worker]
	lifecycle := &incrementalColdCarrierWavesLifecycle{
		coordinator: coordinator,
		worker:      worker,
		offsets:     make([]int, len(planned.waves)+1),
	}
	for waveIndex := range planned.waves {
		lifecycle.offsets[waveIndex] = len(lifecycle.items)
		for laneIndex := range planned.waves[waveIndex].lanes {
			lane := &planned.waves[waveIndex].lanes[laneIndex]
			lifecycle.items = append(lifecycle.items, lane.items...)
			for range lane.items {
				lifecycle.components = append(lifecycle.components, lane.component)
			}
		}
	}
	lifecycle.offsets[len(planned.waves)] = len(lifecycle.items)
	if len(lifecycle.items) == 0 {
		return nil, errors.New("incremental cold carrier waves lifecycle has no items")
	}
	lifecycle.seal = lifecycle
	return lifecycle, nil
}

func (l *incrementalColdCarrierWavesLifecycle) LoadWave(
	ctx context.Context,
	wave int,
) ([]templating.IncrementalComponentVectorCarrierLane, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ctx == nil || !l.validLocked() || l.terminal != nil || l.loaded || l.sealing || wave != l.wave ||
		wave >= len(l.offsets)-1 {
		return nil, errors.New("incremental cold carrier wave lifecycle cannot load this wave")
	}
	if cause := context.Cause(ctx); cause != nil {
		l.terminal = cause
		return nil, cause
	}
	lanes, inner, err := l.coordinator.loadWave(l.worker, wave)
	if err != nil {
		l.terminal = err
		return nil, err
	}
	wantItems := l.offsets[wave+1] - l.offsets[wave]
	gotItems := 0
	for laneIndex := range lanes {
		gotItems += lanes[laneIndex].Count
	}
	if gotItems != wantItems || (wantItems == 0) != (inner == nil) {
		err := fmt.Errorf("incremental cold carrier wave %d loaded an invalid shape", wave)
		l.terminal = err
		l.coordinator.workerFailed(l.worker, wave, math.MaxInt, err)
		return nil, err
	}
	l.inner = inner
	l.loaded = true
	return lanes, nil
}

func (l *incrementalColdCarrierWavesLifecycle) Begin(index int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.validLocked() || l.terminal != nil || !l.loaded || l.sealing || l.inner == nil ||
		index < l.offsets[l.wave] || index >= l.offsets[l.wave+1] {
		return errors.New("incremental cold carrier wave item cannot begin")
	}
	return l.inner.Begin(index - l.offsets[l.wave])
}

func (l *incrementalColdCarrierWavesLifecycle) End(index int, output string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.validLocked() || l.terminal != nil || !l.loaded || l.sealing || l.inner == nil ||
		index < l.offsets[l.wave] || index >= l.offsets[l.wave+1] {
		return errors.New("incremental cold carrier wave item cannot end")
	}
	return l.inner.End(index-l.offsets[l.wave], output)
}

func (l *incrementalColdCarrierWavesLifecycle) SealWave(wave int) error {
	l.mu.Lock()
	if !l.validLocked() || l.terminal != nil || !l.loaded || l.sealing || wave != l.wave {
		l.mu.Unlock()
		return errors.New("incremental cold carrier wave lifecycle cannot seal this wave")
	}
	if l.inner != nil {
		if err := l.inner.validateComplete(); err != nil {
			l.terminal = err
			batchIndex := l.firstBatchIndexLocked()
			l.mu.Unlock()
			l.coordinator.workerFailed(l.worker, wave, batchIndex, err)
			return err
		}
	}
	l.sealing = true
	l.mu.Unlock()

	err := l.coordinator.sealWave(l.worker, wave)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sealing = false
	if err != nil {
		l.terminal = err
		return err
	}
	l.inner = nil
	l.loaded = false
	l.wave++
	return nil
}

func (l *incrementalColdCarrierWavesLifecycle) Abort(activeIndex int, cause error) {
	l.mu.Lock()
	if !l.validLocked() || l.terminal != nil {
		l.mu.Unlock()
		return
	}
	if cause == nil {
		cause = errors.New("incremental cold carrier wave lifecycle aborted")
	}
	wave := l.wave
	batchIndex := l.firstBatchIndexLocked()
	local := -1
	if activeIndex >= 0 && wave < len(l.offsets)-1 &&
		activeIndex >= l.offsets[wave] && activeIndex < l.offsets[wave+1] {
		local = activeIndex - l.offsets[wave]
		batchIndex = l.items[activeIndex].batchIndex
	}
	if l.inner != nil {
		l.inner.Abort(local, cause)
	}
	mappedIndex, mapped := l.remapErrorLocked(cause)
	if mappedIndex < batchIndex {
		batchIndex = mappedIndex
	}
	l.terminal = mapped
	l.mu.Unlock()
	l.coordinator.workerFailed(l.worker, wave, batchIndex, mapped)
}

func (l *incrementalColdCarrierWavesLifecycle) fail(batchIndex int, cause error) {
	l.mu.Lock()
	if !l.validLocked() || l.terminal != nil {
		l.mu.Unlock()
		return
	}
	if cause == nil {
		cause = errors.New("incremental cold carrier wave renderer failed")
	}
	wave := l.wave
	if l.inner != nil {
		l.inner.Abort(-1, cause)
	}
	l.terminal = cause
	l.mu.Unlock()
	l.coordinator.workerFailed(l.worker, wave, batchIndex, cause)
}

func (l *incrementalColdCarrierWavesLifecycle) validateComplete() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.validLocked() {
		return errors.New("incremental cold carrier wave lifecycle has invalid provenance")
	}
	if l.terminal != nil {
		return l.terminal
	}
	if l.loaded || l.sealing || l.inner != nil || l.wave != len(l.offsets)-1 {
		return errors.New("incremental cold carrier wave lifecycle did not complete")
	}
	return nil
}

func (l *incrementalColdCarrierWavesLifecycle) remapError(err error) (batchIndex int, remapped error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.remapErrorLocked(err)
}

func (l *incrementalColdCarrierWavesLifecycle) remapErrorLocked(err error) (batchIndex int, remapped error) {
	var itemErr *templating.IncrementalComponentBatchError
	if errors.As(err, &itemErr) && itemErr.Index >= 0 && itemErr.Index < len(l.items) {
		item := l.items[itemErr.Index]
		component := l.components[itemErr.Index]
		if component != nil {
			templateErr := remapIncrementalTemplateError(component.name, component.entryPoint, itemErr.Err)
			return item.batchIndex, attributeToWatchedObject(item.queryKey, templateErr)
		}
	}
	return l.firstBatchIndexLocked(), err
}

func (l *incrementalColdCarrierWavesLifecycle) firstBatchIndexLocked() int {
	if l.wave < 0 || l.wave >= len(l.offsets)-1 {
		return math.MaxInt
	}
	start := l.offsets[l.wave]
	end := l.offsets[l.wave+1]
	if start >= end || start < 0 || end > len(l.items) {
		return math.MaxInt
	}
	first := math.MaxInt
	for _, item := range l.items[start:end] {
		first = min(first, item.batchIndex)
	}
	return first
}

func (l *incrementalColdCarrierWavesLifecycle) validLocked() bool {
	return l != nil && l.seal == l && l.coordinator != nil && l.coordinator.valid() &&
		l.worker >= 0 && l.worker < len(l.coordinator.schedule.workers) && len(l.offsets) > 1 &&
		len(l.components) == len(l.items)
}

func selectIncrementalColdCarrierWaveError(
	errorsInWave []incrementalColdCarrierWaveError,
) error {
	if len(errorsInWave) == 0 {
		return nil
	}
	ordered := slices.Clone(errorsInWave)
	slices.SortFunc(ordered, func(left, right incrementalColdCarrierWaveError) int {
		if left.batchIndex != right.batchIndex {
			return left.batchIndex - right.batchIndex
		}
		return left.worker - right.worker
	})
	return ordered[0].err
}
