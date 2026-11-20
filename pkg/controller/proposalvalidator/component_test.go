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

package proposalvalidator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validation"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/dataplanetest"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores/storetest"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type cancelingRejectingOutputValidator struct {
	cancel            context.CancelCauseFunc
	cause             error
	cancelOnCall      int
	unavailableOnCall int
	calls             int
}

func (v *cancelingRejectingOutputValidator) ValidateRenderedOutput(context.Context, *pipeline.PipelineResult) ([]string, error) {
	v.calls++
	if v.calls == v.unavailableOnCall {
		return nil, validation.ErrValidatorUnavailable
	}
	if v.calls == v.cancelOnCall {
		v.cancel(v.cause)
	}
	return nil, errors.New("rendered output rejected")
}

func defaultCapabilities() dataplane.Capabilities {
	return dataplane.Capabilities{
		SupportsCrtList:        true,
		SupportsMapStorage:     true,
		SupportsGeneralStorage: true,
	}
}

func createTestPipeline(t *testing.T, template string) *pipeline.Pipeline {
	t.Helper()
	return createTestPipelineWithOutputValidator(t, template, nil)
}

func createTestPipelineWithOutputValidator(t *testing.T, template string, outputValidator pipeline.RenderedOutputValidator) *pipeline.Pipeline {
	t.Helper()

	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{
			Template: template,
		},
	}

	engine, err := templating.New(map[string]string{"haproxy.cfg": template}, nil)
	require.NoError(t, err)

	renderSvc := renderer.NewRenderService(&renderer.RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})

	validationSvc := validation.NewValidationService(&validation.ValidationServiceConfig{
		Logger:            slog.Default(),
		SkipDNSValidation: true,
	})

	return pipeline.New(&pipeline.PipelineConfig{
		Renderer:        renderSvc,
		Validator:       validationSvc,
		OutputValidator: outputValidator,
		Logger:          slog.Default(),
	})
}

func createCurrentFilesTestPipeline(t *testing.T, template string) *pipeline.Pipeline {
	t.Helper()

	cfg := &config.Config{HAProxyConfig: config.HAProxyConfig{Template: template}}
	engine, err := templating.New(
		map[string]string{"haproxy.cfg": template},
		&templating.Options{Declarations: map[string]any{"currentFiles": (*map[string]string)(nil)}},
	)
	require.NoError(t, err)

	return pipeline.New(&pipeline.PipelineConfig{
		Renderer: renderer.NewRenderService(&renderer.RenderServiceConfig{
			Engine:       engine,
			Config:       cfg,
			Logger:       slog.Default(),
			Capabilities: defaultCapabilities(),
		}),
		Validator: validation.NewValidationService(&validation.ValidationServiceConfig{
			Logger:            slog.Default(),
			SkipDNSValidation: true,
		}),
		Logger: slog.Default(),
	})
}

func createStoreTestPipeline(t *testing.T, template string) *pipeline.Pipeline {
	t.Helper()

	cfg := &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: template},
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {APIVersion: "networking.k8s.io/v1", Resources: "ingresses", IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
	}
	declarations := typebootstrap.BuildEngineDeclarations(&typebootstrap.Result{}, "ingresses")
	engine, err := templating.New(map[string]string{"haproxy.cfg": template}, &templating.Options{
		EntryPoints:  []string{"haproxy.cfg"},
		Declarations: declarations,
	})
	require.NoError(t, err)

	renderSvc := renderer.NewRenderService(&renderer.RenderServiceConfig{
		Engine:       engine,
		Config:       cfg,
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})
	validationSvc := validation.NewValidationService(&validation.ValidationServiceConfig{
		Logger:            slog.Default(),
		SkipDNSValidation: true,
	})
	return pipeline.New(&pipeline.PipelineConfig{
		Renderer:  renderSvc,
		Validator: validationSvc,
		Logger:    slog.Default(),
	})
}

func TestNew(t *testing.T) {
	template := testutil.MinimalHAProxyConfig

	bus := busevents.NewEventBus(100)
	testPipeline := createTestPipeline(t, template)
	baseStore := stores.NewRealStoreProvider(map[string]stores.Store{})

	component := New(bus, &ServiceConfig{
		Pipeline:          testPipeline,
		BaseStoreProvider: baseStore,
		Logger:            slog.Default(),
	})

	require.NotNil(t, component)
	assert.NotNil(t, component.Base, "async mode must embed the component.Base event loop")
	assert.NotNil(t, component.service.pipeline)
	assert.NotNil(t, component.service.baseStore)
	assert.NotNil(t, component.service.logger)
}

func TestService_ValidateSync_ValidConfig(t *testing.T) {
	template := testutil.MinimalHAProxyConfig

	pipelineInstance := createTestPipeline(t, template)
	baseStore := stores.NewRealStoreProvider(map[string]stores.Store{})

	component := NewService(&ServiceConfig{
		Pipeline:          pipelineInstance,
		BaseStoreProvider: baseStore,
		Logger:            slog.Default(),
	})

	// Empty overlays should result in valid config
	overlays := map[string]*stores.StoreOverlay{}

	pipelineResult, result := component.ValidateSync(context.Background(), overlays)

	require.NotNil(t, result)
	assert.True(t, result.Valid)
	assert.Nil(t, result.Error)
	assert.Empty(t, result.Phase)
	assert.GreaterOrEqual(t, result.DurationMs, int64(0))
	// Successful validation must populate the pipeline result so callers
	// can feed the rendered files to downstream consumers (e.g. pluggable
	// validators in dryrunvalidator).
	require.NotNil(t, pipelineResult)
	assert.NotEmpty(t, pipelineResult.HAProxyConfig)
}

func TestService_ValidateSync_InvalidOverlay(t *testing.T) {
	template := `global
    daemon

defaults
    mode http
`

	pipelineInstance := createTestPipeline(t, template)
	// Empty base store - no stores registered
	baseStore := stores.NewRealStoreProvider(map[string]stores.Store{})

	component := NewService(&ServiceConfig{
		Pipeline:          pipelineInstance,
		BaseStoreProvider: baseStore,
		Logger:            slog.Default(),
	})

	// Overlay references non-existent store
	overlays := map[string]*stores.StoreOverlay{
		"nonexistent": stores.NewStoreOverlay(),
	}

	pipelineResult, result := component.ValidateSync(context.Background(), overlays)

	require.NotNil(t, result)
	assert.False(t, result.Valid)
	assert.Equal(t, "setup", result.Phase)
	assert.NotNil(t, result.Error)
	assert.Nil(t, pipelineResult, "failed validation must not leak a partial pipeline result")
}

func TestService_ValidateSync_UnchangedInvalidContent_Admits(t *testing.T) {
	t.Cleanup(dataplanetest.InstallFakeHAProxy(
		dataplanetest.WithRejectAll("parsing [haproxy.cfg:3] : unknown keyword 'nosuch_directive_haproxy_will_reject'")))
	template := `global
    daemon
    nosuch_directive_haproxy_will_reject this_is_an_alert_trigger
`

	pipelineInstance := createTestPipeline(t, template)
	baseStore := stores.NewRealStoreProvider(map[string]stores.Store{"ingresses": &storetest.MockStore{}})

	component := NewService(&ServiceConfig{
		Pipeline:          pipelineInstance,
		BaseStoreProvider: baseStore,
		Logger:            slog.Default(),
	})

	overlays := map[string]*stores.StoreOverlay{
		"ingresses": stores.NewStoreOverlayForCreate(unstructuredObj("default", "unrelated")),
	}
	pipelineResult, result := component.ValidateSync(context.Background(), overlays)

	require.NotNil(t, result)
	assert.True(t, result.Valid)
	assert.Empty(t, result.Phase)
	assert.Nil(t, result.Error)
	require.NotNil(t, pipelineResult)
	assert.NotEmpty(t, pipelineResult.ContentChecksum)
}

func TestService_ValidateSync_PinsCurrentFilesAcrossBaseline(t *testing.T) {
	t.Cleanup(dataplanetest.InstallFakeHAProxy(dataplanetest.WithRejectAll("invalid configuration")))
	template := `global
    daemon
# current-files: {{ currentFiles["gate"] }}
`
	calls := 0
	component := NewService(&ServiceConfig{
		Pipeline:          createCurrentFilesTestPipeline(t, template),
		BaseStoreProvider: stores.NewRealStoreProvider(map[string]stores.Store{}),
		CurrentFilesProvider: func() (map[string]string, error) {
			calls++
			return map[string]string{"gate": fmt.Sprintf("snapshot-%d", calls)}, nil
		},
		Logger: slog.Default(),
	})

	pipelineResult, result := component.ValidateSync(context.Background(), nil)

	require.NotNil(t, result)
	assert.True(t, result.Valid)
	require.NotNil(t, pipelineResult)
	assert.Equal(t, 1, calls)
}

func TestService_ValidateSyncRejectsUnavailableCurrentFiles(t *testing.T) {
	authorityErr := errors.New("published currentFiles unavailable")
	component := NewService(&ServiceConfig{
		BaseStoreProvider: stores.NewRealStoreProvider(map[string]stores.Store{}),
		CurrentFilesProvider: func() (map[string]string, error) {
			return nil, authorityErr
		},
		Logger: slog.Default(),
	})

	pipelineResult, result := component.ValidateSync(t.Context(), nil)

	assert.Nil(t, pipelineResult)
	require.NotNil(t, result)
	assert.False(t, result.Valid)
	assert.Equal(t, "render", result.Phase)
	assert.ErrorIs(t, result.Error, authorityErr)
}

func TestService_ValidateSync_CanceledUnchangedInvalidContent_Denies(t *testing.T) {
	authorityErr := errors.New("admission authority expired")
	ctx, cancel := context.WithCancelCause(context.Background())
	outputValidator := &cancelingRejectingOutputValidator{
		cancel:       cancel,
		cause:        authorityErr,
		cancelOnCall: 1,
	}
	component := NewService(&ServiceConfig{
		Pipeline: createTestPipelineWithOutputValidator(
			t,
			testutil.MinimalHAProxyConfig,
			outputValidator,
		),
		BaseStoreProvider: stores.NewRealStoreProvider(map[string]stores.Store{}),
		Logger:            slog.Default(),
	})

	pipelineResult, result := component.ValidateSync(ctx, map[string]*stores.StoreOverlay{})

	assert.Nil(t, pipelineResult)
	require.NotNil(t, result)
	assert.False(t, result.Valid)
	assert.Equal(t, "external", result.Phase)
	assert.ErrorIs(t, result.Error, authorityErr)
	assert.Equal(t, 1, outputValidator.calls)
}

func TestService_ValidateSync_CancellationDuringBaselineDenies(t *testing.T) {
	authorityErr := errors.New("admission authority expired")
	ctx, cancel := context.WithCancelCause(context.Background())
	outputValidator := &cancelingRejectingOutputValidator{
		cancel:       cancel,
		cause:        authorityErr,
		cancelOnCall: 2,
	}
	component := NewService(&ServiceConfig{
		Pipeline: createTestPipelineWithOutputValidator(
			t,
			testutil.MinimalHAProxyConfig,
			outputValidator,
		),
		BaseStoreProvider: stores.NewRealStoreProvider(map[string]stores.Store{}),
		Logger:            slog.Default(),
	})

	pipelineResult, result := component.ValidateSync(ctx, map[string]*stores.StoreOverlay{})

	assert.Nil(t, pipelineResult)
	require.NotNil(t, result)
	assert.False(t, result.Valid)
	assert.Equal(t, "external", result.Phase)
	assert.ErrorIs(t, result.Error, authorityErr)
	assert.Equal(t, 2, outputValidator.calls)
}

func TestService_UnavailableValidatorCannotAuthorizeBaselineRecovery(t *testing.T) {
	for _, unavailableOn := range []int{1, 2} {
		validator := &cancelingRejectingOutputValidator{unavailableOnCall: unavailableOn}
		service := NewService(&ServiceConfig{
			Pipeline:          createTestPipelineWithOutputValidator(t, testutil.MinimalHAProxyConfig, validator),
			BaseStoreProvider: stores.NewRealStoreProvider(map[string]stores.Store{}),
		})
		_, result := service.ValidateSync(t.Context(), nil)
		require.False(t, result.Valid)
		require.ErrorIs(t, result.Error, validation.ErrValidatorUnavailable)
		require.Equal(t, unavailableOn, validator.calls)
	}
}

func TestPipelineFailurePhase(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "external validation",
			err: &pipeline.PipelineError{
				Phase:           pipeline.PhaseValidation,
				ValidationPhase: "external",
				Cause:           errors.New("canceled"),
			},
			want: "external",
		},
		{
			name: "validation without subphase",
			err: &pipeline.PipelineError{
				Phase: pipeline.PhaseValidation,
				Cause: errors.New("canceled"),
			},
			want: "validation",
		},
		{
			name: "render",
			err: &pipeline.PipelineError{
				Phase: pipeline.PhaseRender,
				Cause: errors.New("render failed"),
			},
			want: "render",
		},
		{
			name: "unexpected error",
			err:  errors.New("unexpected"),
			want: "render",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pipelineFailurePhase(tt.err))
		})
	}
}

func TestService_ValidateSync_AdmissionSubjectOnlyDifference_Admits(t *testing.T) {
	t.Cleanup(dataplanetest.InstallFakeHAProxy(dataplanetest.WithRejectAll("invalid configuration")))
	template := `global
    daemon
# subject: {{ admissionSubject | dig("name") | fallback("") }}
`

	component := NewService(&ServiceConfig{
		Pipeline:          createTestPipeline(t, template),
		BaseStoreProvider: stores.NewRealStoreProvider(map[string]stores.Store{"ingresses": &storetest.MockStore{}}),
		Logger:            slog.Default(),
	})
	overlays := map[string]*stores.StoreOverlay{
		"ingresses": stores.NewStoreOverlayForCreate(unstructuredObj("default", "new-failure")),
	}

	pipelineResult, result := component.ValidateSync(context.Background(), overlays)

	require.NotNil(t, result)
	assert.True(t, result.Valid)
	assert.Empty(t, result.Phase)
	assert.NoError(t, result.Error)
	require.NotNil(t, pipelineResult)
}

func TestService_ValidateSync_ChangedInvalidContent_Denies(t *testing.T) {
	t.Cleanup(dataplanetest.InstallFakeHAProxy(dataplanetest.WithRejectAll("invalid configuration")))
	template := `global
    daemon
# ingress-count: {{ len(resources.ingresses.List()) }}
`

	component := NewService(&ServiceConfig{
		Pipeline:          createStoreTestPipeline(t, template),
		BaseStoreProvider: stores.NewRealStoreProvider(map[string]stores.Store{"ingresses": &storetest.MockStore{}}),
		Logger:            slog.Default(),
	})
	overlays := map[string]*stores.StoreOverlay{
		"ingresses": stores.NewStoreOverlayForCreate(unstructuredObj("default", "new-failure")),
	}

	pipelineResult, result := component.ValidateSync(context.Background(), overlays)

	require.NotNil(t, result)
	assert.False(t, result.Valid)
	assert.Equal(t, "semantic", result.Phase)
	assert.Error(t, result.Error)
	assert.Nil(t, pipelineResult)
}

func TestValidationResult_ErrorMessage(t *testing.T) {
	tests := []struct {
		name     string
		result   *validation.ValidationResult
		expected string
	}{
		{
			name: "valid result returns empty",
			result: &validation.ValidationResult{
				Valid: true,
			},
			expected: "",
		},
		{
			name: "invalid with error returns message",
			result: &validation.ValidationResult{
				Valid: false,
				Error: assert.AnError,
			},
			expected: assert.AnError.Error(),
		},
		{
			name: "invalid with nil error returns empty",
			result: &validation.ValidationResult{
				Valid: false,
				Error: nil,
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.result.ErrorMessage())
		})
	}
}

// HTTP proposals always deny invalid candidate content instead of comparing
// it with the live Kubernetes-only baseline.
func TestComponent_Start_AsyncPath_DeniesOnFailure(t *testing.T) {
	bus := busevents.NewEventBus(100)
	failingEngine, err := templating.New(map[string]string{"haproxy.cfg": `{{ fail("pretend HTTP content is bad") }}`}, nil)
	require.NoError(t, err)

	renderSvc := renderer.NewRenderService(&renderer.RenderServiceConfig{
		Engine:       failingEngine,
		Config:       &config.Config{},
		Logger:       slog.Default(),
		Capabilities: defaultCapabilities(),
	})
	validationSvc := validation.NewValidationService(&validation.ValidationServiceConfig{
		Logger:            slog.Default(),
		SkipDNSValidation: true,
	})
	pipelineInstance := pipeline.New(&pipeline.PipelineConfig{
		Renderer:  renderSvc,
		Validator: validationSvc,
		Logger:    slog.Default(),
	})

	baseStore := stores.NewRealStoreProvider(map[string]stores.Store{})
	component := New(bus, &ServiceConfig{
		Pipeline:          pipelineInstance,
		BaseStoreProvider: baseStore,
		Logger:            slog.Default(),
	})

	resultChan := bus.Subscribe("test", 10)
	bus.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = component.Start(ctx) }()
	time.Sleep(50 * time.Millisecond)

	req := events.NewProposalValidationRequestedEvent(
		map[string]*stores.StoreOverlay{},
		nil,
		"http-store",
		"pending-content-promotion",
	)
	bus.Publish(req)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-resultChan:
			completed, ok := event.(*events.ProposalValidationCompletedEvent)
			if !ok {
				continue
			}
			if completed.RequestID != req.ID {
				continue
			}
			assert.False(t, completed.Valid,
				"async path MUST publish Valid=false on validation failure — "+
					"HTTPStore.handleProposalValidationCompleted promotes pending "+
					"content on Valid=true, so admitting here would compound broken "+
					"state by promoting BAD content into the live config; phase=%q err=%q",
				completed.Phase, completed.Error)
			return
		case <-deadline:
			t.Fatal("timeout waiting for validation completion event — async handler may not be running, " +
				"or it published nothing (which would also be a regression)")
		}
	}
}

func TestComponent_Start_ProcessesEvents(t *testing.T) {
	template := testutil.MinimalHAProxyConfig

	bus := busevents.NewEventBus(100)
	pipelineInstance := createTestPipeline(t, template)
	baseStore := stores.NewRealStoreProvider(map[string]stores.Store{})

	component := New(bus, &ServiceConfig{
		Pipeline:          pipelineInstance,
		BaseStoreProvider: baseStore,
		Logger:            slog.Default(),
	})

	// Subscribe to completion events
	resultChan := bus.Subscribe("test", 10)

	// Start event bus and component
	bus.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = component.Start(ctx)
	}()

	// Allow component to start
	time.Sleep(50 * time.Millisecond)

	// Publish validation request (nil HTTP overlay for K8s-only validation)
	req := events.NewProposalValidationRequestedEvent(
		map[string]*stores.StoreOverlay{},
		nil, // no HTTP overlay
		"test",
		"test context",
	)
	bus.Publish(req)

	// Wait for completion event
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-resultChan:
			if completed, ok := event.(*events.ProposalValidationCompletedEvent); ok {
				assert.Equal(t, req.ID, completed.RequestID)
				assert.True(t, completed.Valid)
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for validation completion event")
		}
	}
}
