// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

func TestCoordinatorChangedSnapshotPreservesAcceptedStateAndNextTrigger(t *testing.T) {
	bus, logger := testutil.NewTestBusAndLogger()
	observed := bus.Subscribe("changed-snapshot", 100)
	bus.Start()
	executor := &mockPipeline{err: &pipeline.PipelineError{
		Phase: pipeline.PhaseRender,
		Cause: fmt.Errorf("preparing base changes: %w", stores.ErrSnapshotChanged),
	}}
	authority := &recordingOutputCurrentFilesAuthority{
		recordingCurrentFilesAuthority: recordingCurrentFilesAuthority{
			files: map[string]string{"routes.map": "accepted"},
		},
	}
	coordinator := NewCoordinator(&CoordinatorConfig{
		EventBus: bus, Pipeline: executor, Logger: logger,
		StoreProvider: stores.NewRealStoreProvider(nil), CurrentFiles: authority,
	})
	generation := authority.BeginTerm()
	coordinator.ledger.deployedFrom.Store(17)
	coordinator.criticalFailureReported = true

	occurrence := coordinator.handleReconciliationTriggered(t.Context(),
		events.NewReconciliationTriggeredEvent("resource_change", true), generation)
	require.Nil(t, occurrence)
	require.Len(t, executor.optionCounts, 1)
	require.Empty(t, authority.acceptedOutputs)
	require.Equal(t, uint64(17), coordinator.ledger.deployedFrom.Load())
	require.True(t, coordinator.criticalFailureReported)

	executor.err = nil
	executor.result = &pipeline.PipelineResult{
		CycleSnapshot: testutil.NewRenderCycleFixture(t).Snapshot(t, "newer config", nil, nil),
	}
	occurrence = coordinator.handleReconciliationTriggered(context.Background(),
		events.NewReconciliationTriggeredEvent("resource_change", true), generation)
	require.NotNil(t, occurrence)
	require.Len(t, executor.optionCounts, 2)
	require.Len(t, authority.acceptedOutputs, 1)
	require.False(t, coordinator.criticalFailureReported)

	timeout := time.NewTimer(testutil.EventTimeout)
	defer timeout.Stop()
	starts, renders := 0, 0
	for {
		select {
		case event := <-observed:
			switch event := event.(type) {
			case *events.ReconciliationStartedEvent:
				starts++
			case *events.TemplateRenderedEvent:
				renders++
				assert.Equal(t, "newer config", event.HAProxyConfig)
			case *events.ReconciliationCompletedEvent:
				assert.Equal(t, 2, starts)
				assert.Equal(t, 1, renders)
				return
			case *events.TemplateRenderFailedEvent, *events.ValidationFailedEvent, *events.ReconciliationFailedEvent:
				t.Fatalf("published failure for superseded inputs: %T", event)
			}
		case <-timeout.C:
			t.Fatal("the newer trigger did not complete")
		}
	}
}
