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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validation"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const renderPhase = "render"
const setupPhase = "setup"

// Service validates proposed changes against the current stores.
type Service struct {
	pipeline               *pipeline.Pipeline
	baseStore              stores.StoreProvider
	freshStoreProvider     func(context.Context) (stores.StoreProvider, error)
	acceptedStoreProvider  func() (stores.StoreProvider, bool)
	admissionStoreProvider func(context.Context, stores.StoreProvider) (stores.StoreProvider, bool, error)
	observedStoreProvider  func(context.Context, stores.StoreProvider) (stores.StoreProvider, error)
	refreshSlot            chan struct{}
	currentFilesProvider   func() (map[string]string, error)
	logger                 *slog.Logger
}

// ServiceConfig supplies the validation pipeline and its inputs.
type ServiceConfig struct {
	Pipeline          *pipeline.Pipeline
	BaseStoreProvider stores.StoreProvider
	// FreshStoreProvider supplies API-backed inputs when a cached render rejects admission.
	FreshStoreProvider func(context.Context) (stores.StoreProvider, error)
	// AcceptedStoreProvider supplies inputs that passed complete output validation.
	AcceptedStoreProvider func() (stores.StoreProvider, bool)
	// AdmissionStoreProvider excludes only rejected revisions from observed inputs.
	AdmissionStoreProvider func(context.Context, stores.StoreProvider) (stores.StoreProvider, bool, error)
	// ObservedStoreProvider preserves the reconciliation input revision family.
	ObservedStoreProvider func(context.Context, stores.StoreProvider) (stores.StoreProvider, error)
	// CurrentFilesProvider pins one published baseline across both renders.
	CurrentFilesProvider func() (map[string]string, error)
	Logger               *slog.Logger
}

// NewService creates a validator without an event subscription or lifecycle.
func NewService(cfg *ServiceConfig) *Service {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		pipeline:               cfg.Pipeline,
		baseStore:              cfg.BaseStoreProvider,
		freshStoreProvider:     cfg.FreshStoreProvider,
		acceptedStoreProvider:  cfg.AcceptedStoreProvider,
		admissionStoreProvider: cfg.AdmissionStoreProvider,
		observedStoreProvider:  cfg.ObservedStoreProvider,
		refreshSlot:            make(chan struct{}, 1),
		currentFilesProvider:   cfg.CurrentFilesProvider,
		logger:                 logger.With("component", ComponentName),
	}
}

// HTTP content promotion must reject invalid output even when the baseline is identical.
func (c *Service) validateProposal(ctx context.Context, overlays map[string]*stores.StoreOverlay, httpOverlay stores.HTTPContentOverlay) (result *validation.ValidationResult, err error) {
	validationCtx := stores.NewValidationContext(overlays)
	if httpOverlay != nil {
		validationCtx = validationCtx.WithHTTPOverlay(httpOverlay)
	}
	base := c.baseStore
	if c.acceptedStoreProvider != nil {
		if accepted, ready := c.acceptedStoreProvider(); ready {
			base = accepted
		}
	}
	overlayProvider := stores.NewOverlayStoreProvider(base, validationCtx)
	if err := overlayProvider.Validate(); err != nil {
		return proposalFailure(setupPhase, err), err
	}

	ctx, cancel := context.WithTimeout(ctx, validation.DefaultValidationTimeout)
	defer cancel()
	opts := admissionSubjectOpts(overlays)
	if len(overlays) == 0 && httpOverlay != nil && !httpOverlay.IsEmpty() {
		opts = append(opts, rendercontext.WithAdmissionHTTPURLs(httpOverlay.PendingURLs()))
	}
	opts, snapshotErr := c.withCurrentFilesSnapshot(opts)
	if snapshotErr != nil {
		return proposalFailure(renderPhase, snapshotErr), snapshotErr
	}
	_, result, err = c.pipeline.ExecuteWithResult(ctx, overlayProvider, rendercontext.RenderModeAdmission, opts...)
	if err != nil {
		return proposalFailure(pipelineFailurePhase(err), err), err
	}
	return result, nil
}

func proposalFailure(phase string, err error) *validation.ValidationResult {
	return &validation.ValidationResult{Phase: phase, Error: err}
}

func admissionSubjectOpts(overlays map[string]*stores.StoreOverlay) []rendercontext.Option {
	var opts []rendercontext.Option
	count := 0
	for storeName, overlay := range overlays {
		if overlay == nil {
			continue
		}
		for _, obj := range overlay.Additions {
			if accessor, err := meta.Accessor(obj); err == nil {
				count++
				opts = []rendercontext.Option{rendercontext.WithAdmissionSubject(storeName, accessor.GetNamespace(), accessor.GetName())}
			}
		}
		for _, obj := range overlay.Modifications {
			if accessor, err := meta.Accessor(obj); err == nil {
				count++
				opts = []rendercontext.Option{rendercontext.WithAdmissionSubject(storeName, accessor.GetNamespace(), accessor.GetName())}
			}
		}
		for _, key := range overlay.Deletions {
			count++
			opts = []rendercontext.Option{rendercontext.WithAdmissionSubject(storeName, key.Namespace, key.Name)}
		}
	}
	if count != 1 {
		return nil
	}
	return opts
}

// ValidateSync admits unchanged invalid output only after an exact baseline comparison.
func (c *Service) ValidateSync(ctx context.Context, overlays map[string]*stores.StoreOverlay) (*pipeline.PipelineResult, *validation.ValidationResult) {
	return c.validateSync(ctx, overlays, admissionSubjectOpts(overlays)...)
}

// ValidateSyncWithAdmissionSubject validates one admitted object across every
// configured store alias affected by the request.
func (c *Service) ValidateSyncWithAdmissionSubject(ctx context.Context, overlays map[string]*stores.StoreOverlay, storeAliases []string, namespace, name string) (*pipeline.PipelineResult, *validation.ValidationResult) {
	var opts []rendercontext.Option
	if len(storeAliases) > 0 {
		opts = []rendercontext.Option{rendercontext.WithAdmissionSubjectStores(storeAliases, namespace, name)}
	}
	return c.validateSync(ctx, overlays, opts...)
}

func (c *Service) validateSync(ctx context.Context, overlays map[string]*stores.StoreOverlay, opts ...rendercontext.Option) (*pipeline.PipelineResult, *validation.ValidationResult) {
	startTime := time.Now()

	// Build ValidationContext from K8s overlays
	validationCtx := stores.NewValidationContext(overlays)

	// Create OverlayStoreProvider that applies K8s overlays
	base := c.baseStore
	if c.observedStoreProvider != nil {
		projected, err := c.observedStoreProvider(ctx, base)
		if err != nil {
			return nil, proposalFailure(renderPhase, err)
		}
		base = projected
	}
	overlayProvider := stores.NewOverlayStoreProvider(base, validationCtx)

	// Validate overlays reference valid stores
	if err := overlayProvider.Validate(); err != nil {
		return nil, &validation.ValidationResult{
			Valid:      false,
			Phase:      setupPhase,
			Error:      err,
			DurationMs: time.Since(startTime).Milliseconds(),
		}
	}

	opts, err := c.withCurrentFilesSnapshot(opts)
	if err != nil {
		return nil, &validation.ValidationResult{
			Valid:      false,
			Phase:      renderPhase,
			Error:      err,
			DurationMs: time.Since(startTime).Milliseconds(),
		}
	}
	outcome := c.validateAdmission(ctx, overlayProvider, opts...)
	if outcome.Admit {
		return outcome.PipelineResult, &validation.ValidationResult{
			Valid:      true,
			DurationMs: time.Since(startTime).Milliseconds(),
			Warnings:   outcome.Warnings,
		}
	}
	return nil, &validation.ValidationResult{
		Valid:      false,
		Phase:      outcome.Phase,
		Error:      outcome.Error,
		DurationMs: time.Since(startTime).Milliseconds(),
		Warnings:   outcome.Warnings,
	}
}

func (c *Service) withCurrentFilesSnapshot(opts []rendercontext.Option) ([]rendercontext.Option, error) {
	if c.currentFilesProvider == nil {
		return opts, nil
	}
	currentFiles, err := c.currentFilesProvider()
	if err != nil {
		return nil, err
	}
	return append(opts, rendercontext.WithCurrentAuxFiles(currentFiles)), nil
}

// validationOutcome is the decision the baseline-aware pipeline driver returns
// to its caller. PipelineResult is the proposed-state output and is populated
// for every admitted outcome so downstream validators inspect the same files.
type validationOutcome struct {
	Admit          bool
	Phase          string
	Error          error
	PipelineResult *pipeline.PipelineResult
	Warnings       []string
}

// runWithBaselineCheck allows an invalid proposed state only when its rendered
// HAProxy content is byte-equivalent to the already-invalid live state.
func (c *Service) runWithBaselineCheck(ctx context.Context, overlayProvider *stores.OverlayStoreProvider, proposedOpts ...rendercontext.Option) validationOutcome {
	pipelineResult, proposedResult, proposedErr := c.pipeline.ExecuteWithResult(ctx, overlayProvider, rendercontext.RenderModeAdmission, proposedOpts...)
	if authorityErr := validationAuthorityFailure(ctx, proposedErr, proposedResult); authorityErr != nil {
		return validationOutcome{Phase: pipelineFailurePhase(authorityErr), Error: authorityErr}
	}
	if proposedErr != nil {
		return validationOutcome{Phase: pipelineFailurePhase(proposedErr), Error: proposedErr}
	}
	if proposedResult == nil {
		return validationOutcome{Phase: "validation", Error: fmt.Errorf("proposal validation returned no result")}
	}
	if proposedResult.Valid {
		return validationOutcome{Admit: true, PipelineResult: pipelineResult, Warnings: proposedResult.Warnings}
	}

	baselinePipelineResult, baselineResult, baselineErr := c.runBaselineCheck(ctx, proposedOpts...)
	if authorityErr := validationAuthorityFailure(ctx, baselineErr, baselineResult); authorityErr != nil {
		return validationOutcome{Phase: pipelineFailurePhase(authorityErr), Error: authorityErr}
	}
	if baselineErr == nil && baselineResult != nil && !baselineResult.Valid &&
		sameRenderedContent(pipelineResult, baselinePipelineResult) {
		c.logger.Warn("Admitting resource because it does not change the already-invalid rendered configuration",
			"validation_phase", proposedResult.Phase,
			"validation_error", proposedResult.Error,
			"content_checksum", pipelineResult.ContentChecksum)
		// Authority may expire concurrently after the first check; recheck at admission.
		if authorityErr := validationAuthorityFailure(ctx, baselineErr, baselineResult); authorityErr != nil {
			return validationOutcome{Phase: pipelineFailurePhase(authorityErr), Error: authorityErr}
		}
		return validationOutcome{Admit: true, PipelineResult: pipelineResult, Warnings: proposedResult.Warnings}
	}

	return validationOutcome{Phase: proposedResult.Phase, Error: proposedResult.Error, Warnings: proposedResult.Warnings}
}

func sameRenderedContent(left, right *pipeline.PipelineResult) bool {
	leftContent, err := authenticatedRenderedContent(left)
	if err != nil {
		return false
	}
	rightContent, err := authenticatedRenderedContent(right)
	if err != nil || leftContent.config != rightContent.config {
		return false
	}
	same, err := leftContent.artifacts.SameRoot(rightContent.artifacts)
	if err != nil || same {
		return err == nil && same
	}
	equal, err := leftContent.artifacts.ExactEqual(rightContent.artifacts)
	return err == nil && equal
}

type renderedContent struct {
	config    string
	artifacts *renderartifact.Snapshot
}

func authenticatedRenderedContent(result *pipeline.PipelineResult) (*renderedContent, error) {
	if result == nil || result.CycleSnapshot == nil {
		return nil, errors.New("pipeline result has no authenticated render cycle")
	}
	output, err := result.CycleSnapshot.OutputSnapshot()
	if err != nil {
		return nil, fmt.Errorf("reading render cycle output: %w", err)
	}
	config, err := output.Config()
	if err != nil {
		return nil, fmt.Errorf("reading render config: %w", err)
	}
	artifacts, err := output.ArtifactSnapshot()
	if err != nil {
		return nil, fmt.Errorf("reading render artifacts: %w", err)
	}
	return &renderedContent{config: config, artifacts: artifacts}, nil
}

func validationAuthorityFailure(ctx context.Context, runErr error, runResult *validation.ValidationResult) error {
	cause := context.Cause(ctx)
	if cause == nil {
		return nil
	}
	if runErr != nil && errors.Is(runErr, cause) {
		return runErr
	}

	phase := pipeline.PhaseValidation
	validationPhase := ""
	if pipelineErr, ok := errors.AsType[*pipeline.PipelineError](runErr); ok {
		phase = pipelineErr.Phase
		validationPhase = pipelineErr.ValidationPhase
	} else if runResult != nil {
		validationPhase = runResult.Phase
	}
	authorityErr := fmt.Errorf("proposal validation did not finish: %w; retry the request", cause)
	if runErr != nil {
		authorityErr = errors.Join(runErr, authorityErr)
	}
	return &pipeline.PipelineError{
		Phase:           phase,
		ValidationPhase: validationPhase,
		Cause:           authorityErr,
	}
}

func pipelineFailurePhase(err error) string {
	pipelineErr, ok := errors.AsType[*pipeline.PipelineError](err)
	if !ok {
		return string(pipeline.PhaseRender)
	}
	if pipelineErr.Phase == pipeline.PhaseValidation && pipelineErr.ValidationPhase != "" {
		return pipelineErr.ValidationPhase
	}
	if pipelineErr.Phase == "" {
		return string(pipeline.PhaseRender)
	}
	return string(pipelineErr.Phase)
}

// runBaselineCheck runs the render-validate pipeline against the live stores
// without any overlays. Used by runWithBaselineCheck to determine whether a
// proposed validation failure came from unchanged rendered content.
func (c *Service) runBaselineCheck(ctx context.Context, opts ...rendercontext.Option) (*pipeline.PipelineResult, *validation.ValidationResult, error) {
	// Wrap the base store in an OverlayStoreProvider with an empty
	// ValidationContext so the pipeline accepts a uniform StoreProvider type
	// in both the proposed and baseline paths. With no overlays the provider
	// behaves as a pass-through to the live stores.
	emptyCtx := stores.NewValidationContext(nil)
	baselineProvider := stores.NewOverlayStoreProvider(c.baseStore, emptyCtx)
	// Baseline runs in the same admission mode as the proposed render so a
	// conflict-style check that fail()s is symmetric: a pre-existing conflict
	// fails BOTH renders, which is exactly what tells runWithBaselineCheck the
	// failure is not caused by the proposed resource (→ admit, don't block an
	// unrelated change on a conflict that was already there).
	return c.pipeline.ExecuteWithResult(ctx, baselineProvider, rendercontext.RenderModeAdmission, opts...)
}
