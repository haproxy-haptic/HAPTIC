package proposalvalidator

import (
	"context"
	"errors"
	"fmt"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validation"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

func (c *Service) validateAdmission(ctx context.Context, provider *stores.OverlayStoreProvider, opts ...rendercontext.Option) validationOutcome {
	observed := c.runWithBaselineCheck(ctx, provider, opts...)
	if errors.Is(observed.Error, validation.ErrValidatorUnavailable) {
		return observed
	}
	isolated, used := c.validateIsolatedAdmission(ctx, provider, observed, opts...)
	if isolated.Admit || ctx.Err() != nil {
		return isolated
	}
	refreshed := c.refreshObservedAdmission(ctx, provider, observed, opts...)
	if refreshed.Admit || ctx.Err() != nil || !used {
		return refreshed
	}
	return isolated
}

func (c *Service) validateIsolatedAdmission(ctx context.Context, provider *stores.OverlayStoreProvider, outcome validationOutcome, opts ...rendercontext.Option) (validationOutcome, bool) {
	if outcome.Admit || c.admissionStoreProvider == nil || ctx.Err() != nil ||
		errors.Is(outcome.Error, context.Canceled) || errors.Is(outcome.Error, context.DeadlineExceeded) {
		return outcome, false
	}
	observed := make(map[string]stores.Store)
	for _, alias := range provider.StoreNames() {
		observed[alias] = provider.GetBaseStore(alias)
	}
	isolated, ready, err := c.admissionStoreProvider(ctx, stores.NewRealStoreProvider(observed))
	if err != nil {
		return validationOutcome{Phase: renderPhase, Error: err}, true
	}
	if !ready {
		return outcome, false
	}
	overlay := rebaseAdmissionProvider(provider, isolated)
	if err := overlay.Validate(); err != nil {
		return validationOutcome{Phase: setupPhase, Error: err}, true
	}
	result, verdict, err := c.pipeline.ExecuteWithResult(ctx, overlay, rendercontext.RenderModeAdmission, opts...)
	if err != nil {
		return validationOutcome{Phase: pipelineFailurePhase(err), Error: err}, true
	}
	if verdict == nil {
		return validationOutcome{Phase: "validation", Error: errors.New("isolated-input validation returned no verdict")}, true
	}
	if !verdict.Valid {
		return validationOutcome{Phase: verdict.Phase, Error: verdict.Error, Warnings: verdict.Warnings}, true
	}
	return validationOutcome{Admit: true, PipelineResult: result, Warnings: append(verdict.Warnings,
		"Some observed changes are rejected. This request passed complete validation with only those rejected revisions withheld.")}, true
}

func (c *Service) refreshObservedAdmission(ctx context.Context, provider *stores.OverlayStoreProvider, outcome validationOutcome, opts ...rendercontext.Option) validationOutcome {
	if outcome.Admit || outcome.Phase != renderPhase || c.freshStoreProvider == nil ||
		ctx.Err() != nil || errors.Is(outcome.Error, context.Canceled) || errors.Is(outcome.Error, context.DeadlineExceeded) {
		return outcome
	}
	select {
	case c.refreshSlot <- struct{}{}:
		defer func() { <-c.refreshSlot }()
	case <-ctx.Done():
		return validationOutcome{Phase: renderPhase, Error: ctx.Err()}
	}
	fresh, err := c.freshStoreProvider(ctx)
	if err != nil {
		return validationOutcome{Phase: renderPhase, Error: fmt.Errorf("refreshing admission inputs: %w", err)}
	}
	if fresh == nil {
		return validationOutcome{Phase: renderPhase, Error: fmt.Errorf("refreshing admission inputs returned no stores")}
	}
	if c.observedStoreProvider != nil {
		fresh, err = c.observedStoreProvider(ctx, fresh)
		if err != nil {
			return validationOutcome{Phase: renderPhase, Error: err}
		}
	}
	if err := ctx.Err(); err != nil {
		return validationOutcome{Phase: renderPhase, Error: err}
	}
	refreshed := rebaseAdmissionProvider(provider, fresh)
	if err := refreshed.Validate(); err != nil {
		return validationOutcome{Phase: setupPhase, Error: err}
	}
	freshService := *c
	freshService.baseStore = fresh
	return freshService.runWithBaselineCheck(ctx, refreshed, opts...)
}

func rebaseAdmissionProvider(provider *stores.OverlayStoreProvider, base stores.StoreProvider) *stores.OverlayStoreProvider {
	overlays := make(map[string]*stores.StoreOverlay)
	for _, name := range provider.StoreNames() {
		if overlay := provider.GetK8sOverlay(name); overlay != nil {
			overlays[name] = overlay
		}
	}
	return stores.NewOverlayStoreProvider(base, stores.NewValidationContext(overlays))
}
