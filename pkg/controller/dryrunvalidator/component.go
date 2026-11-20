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

// Package dryrunvalidator implements the DryRunValidator that performs
// dry-run reconciliation for webhook validation.
//
// The validator is called synchronously by pkg/controller/webhook via
// ValidateDirect. It creates an overlay store simulating the admission
// request's hypothetical state and delegates to ProposalValidator.ValidateSync
// for rendering and validation.
package dryrunvalidator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/proposalvalidator"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validation"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// ComponentName identifies this validator in log records.
const ComponentName = "dryrun-validator"

// Component implements the dry-run validator.
//
// It creates store overlays from admission requests and delegates validation
// to ProposalValidator and returns the shared pipeline's diagnostics.
type Component struct {
	proposalValidator *proposalvalidator.Service
	restMapper        meta.RESTMapper
	aliasesByGVR      map[schema.GroupVersionResource][]resourceAlias
	logger            *slog.Logger
}

// ComponentConfig contains configuration for creating a DryRunValidator.
type ComponentConfig struct {
	ProposalValidator *proposalvalidator.Service

	// RESTMapper resolves an admission request's GVK to its GVR.
	RESTMapper meta.RESTMapper

	// WatchedResources defines the configured store aliases for each GVR.
	WatchedResources map[string]config.WatchedResource

	// Logger is the structured logger.
	Logger *slog.Logger
}

// New creates a new DryRunValidator component.
func New(cfg *ComponentConfig) (*Component, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	aliasesByGVR, err := buildResourceAliases(cfg.WatchedResources)
	if err != nil {
		return nil, err
	}

	return &Component{
		proposalValidator: cfg.ProposalValidator,
		restMapper:        cfg.RESTMapper,
		aliasesByGVR:      aliasesByGVR,
		logger:            logger.With("component", ComponentName),
	}, nil
}

// ValidateAdmission builds selector-aware overlays for the request GVR and
// runs the shared proposal pipeline.
func (c *Component) ValidateAdmission(ctx context.Context, gvk, namespace, name string, object, oldObject any, operation string) (allowed bool, reason string, warnings []string, failure error) {
	c.logger.Debug("Direct validation request", "gvk", gvk, "namespace", namespace, "name", name, "operation", operation)
	aliases, err := c.mapGVKToResourceAliases(gvk)
	if err != nil {
		return false, fmt.Sprintf("unsupported resource type: %v", err), nil, nil
	}

	overlays, subjectAliases, err := c.createOverlays(aliases, namespace, name, object, oldObject, operation)
	if err != nil {
		return false, fmt.Sprintf("building admission overlay: %v", err), nil, nil
	}

	c.logger.Debug("Created store overlays for dry-run",
		"resource_types", subjectAliases,
		"operation", operation)

	pipelineResult, result := c.proposalValidator.ValidateSyncWithAdmissionSubject(
		ctx, overlays, subjectAliases, namespace, name,
	)
	warnings = append(warnings, result.Warnings...)
	if !result.Valid {
		c.logger.Info("Dry-run validation failed",
			"phase", result.Phase,
			"error", result.Error)

		if externalValidationUnavailable(result) {
			reason := "Validation is temporarily unavailable. Retry the request."
			return false, reason, warnings, apierrors.NewServiceUnavailable(reason)
		}
		simplified := c.simplifyError(result.Phase, result.Error)
		c.logger.Debug("Simplified error",
			"phase", result.Phase,
			"simplified", simplified)
		return false, simplified, warnings, nil
	}

	c.logger.Debug("Dry-run validation passed",
		"resource_types", subjectAliases,
		"duration_ms", result.DurationMs)

	// Surface the Warning events templates recorded on the object under
	// admission (kubectl prints them) instead of only in Events afterwards.
	// Events on other objects stay in their own Event streams: a render replays
	// every cached item's events, so listing them here named every violator
	// cluster-wide (#225). PipelineResult is nil on the admitted-via-baseline
	// path, where no proposed-state render exists.
	if pipelineResult != nil {
		renderedEvents, err := pipelineResult.MaterializeEvents()
		if err != nil {
			return false, fmt.Sprintf("reading rendered warnings: %v", err), warnings, nil
		}
		subject := eventSubject{kind: gvk[strings.LastIndex(gvk, ".")+1:], namespace: namespace, name: name}
		warnings = append(warnings, formatRenderedEventWarnings(renderedEvents, subject)...)
	}

	return true, "", warnings, nil
}

func externalValidationUnavailable(result *validation.ValidationResult) bool {
	return errors.Is(result.Error, validation.ErrValidatorUnavailable) ||
		(result.Phase == "external" && (errors.Is(result.Error, context.DeadlineExceeded) || errors.Is(result.Error, context.Canceled)))
}

// maxEventWarnings bounds how many template-recorded Warning events are
// surfaced per admission response; the rest collapse into a count so a
// cluster-wide breakage can't bloat every AdmissionReview.
const maxEventWarnings = 10

// eventSubject identifies the object under admission.
type eventSubject struct {
	kind, namespace, name string
}

func (s eventSubject) matches(e *templating.RenderedEvent) bool {
	return e.Kind == s.kind && e.Namespace == s.namespace && e.Name == s.name
}

// formatRenderedEventWarnings renders the Warning events recorded on the
// subject as admission warning strings, capped at maxEventWarnings.
func formatRenderedEventWarnings(events []templating.RenderedEvent, subject eventSubject) []string {
	var out []string
	suppressed := 0
	for i := range events {
		e := &events[i]
		if e.Type != templating.EventTypeWarning || !subject.matches(e) {
			continue
		}
		if len(out) >= maxEventWarnings {
			suppressed++
			continue
		}
		subject := e.Name
		if e.Namespace != "" {
			subject = e.Namespace + "/" + e.Name
		}
		out = append(out, fmt.Sprintf("%s on %s %s: %s", e.Reason, e.Kind, subject, e.Message))
	}
	if suppressed > 0 {
		out = append(out, fmt.Sprintf("... and %d more warnings (see the Kubernetes Events on the affected resources)", suppressed))
	}
	return out
}

// ValidateDirect returns the denial message to callers that do not consume Kubernetes response codes.
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//   - gvk: GroupVersionKind string (e.g., "networking.k8s.io/v1.Ingress")
//   - namespace: Resource namespace
//   - name: Resource name
//   - object: The proposed Kubernetes resource object
//   - oldObject: The existing object for UPDATE and DELETE
//   - operation: Admission operation (CREATE, UPDATE, DELETE)
//
// Returns:
//   - allowed: Whether the resource passed validation
//   - reason: Denial reason if not allowed, empty otherwise
//   - warnings: Pipeline warnings surfaced through AdmissionResponse.Warnings
func (c *Component) ValidateDirect(ctx context.Context, gvk, namespace, name string, object, oldObject any, operation string) (allowed bool, reason string, warnings []string) {
	allowed, reason, warnings, _ = c.ValidateAdmission(ctx, gvk, namespace, name, object, oldObject, operation)
	return allowed, reason, warnings
}
