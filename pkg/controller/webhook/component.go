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

// Package webhook connects the admission server to controller validation.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/timeouts"
	"github.com/haproxy-haptic/HAPTIC/pkg/webhook"
)

const (
	// ComponentName is the unique identifier for this component.
	ComponentName = "webhook"

	// DefaultWebhookPort is the default HTTPS port for the webhook server.
	DefaultWebhookPort = 9443

	// DefaultWebhookPath is the default URL path for validation requests.
	DefaultWebhookPath = "/validate"

	// DefaultResourceAdmissionTimeout bounds watched-resource (for example,
	// Ingress) dry-run admission. The chart pairs it with a 10-second Kubernetes
	// webhook timeout. Keeping this path at nine seconds limits per-request work
	// and preserves fail-closed admission under untrusted routing-resource load.
	DefaultResourceAdmissionTimeout = 9 * time.Second

	// MaximumAdmissionTimeout is the largest safe controller-side deadline.
	// Kubernetes caps ValidatingWebhookConfiguration.timeoutSeconds at 30;
	// keeping one second of response margin makes 29 seconds the hard maximum.
	MaximumAdmissionTimeout = 29 * time.Second

	// unregisteredGVKLabel stands in for the real GVK on the metric emitted
	// when an AdmissionReview arrives for a kind no validator backs. Deliberately
	// not a valid GVK string, so it cannot collide with a real one.
	unregisteredGVKLabel = "<unregistered>"

	// webhookResponseGrace keeps the HTTP server's write deadline beyond the
	// longest validator deadline. The API server's own timeout remains the
	// authoritative outer bound; this only prevents net/http from cutting off a
	// valid response before that bound.
	webhookResponseGrace        = 2 * time.Second
	validationUnavailableReason = "validation is unavailable; retry after controller initialization"
)

// Component is the webhook adapter component that manages webhook lifecycle.
//
// It coordinates the pure webhook library server with the event-driven controller architecture.
type Component struct {
	// Dependencies
	logger          *slog.Logger
	metrics         MetricsRecorder
	restMapper      meta.RESTMapper
	dryRunValidator DryRunValidator

	// Webhook library components
	server *webhook.Server
	// generation is the validator table this component installed on the
	// shared server, retired on stop only while it is still the installed one.
	generation *webhook.ValidatorGeneration

	// Configuration
	config Config

	// Runtime state
	serverCtx    context.Context
	serverCancel context.CancelFunc

	// listening is closed once the underlying *webhook.Server has bound
	// its listener. The iteration sequencer reads Listening() before
	// flipping the controller's readiness probe so the chart's
	// ValidatingWebhookConfiguration doesn't get routed admission requests
	// while the listener is still pending.
	listening chan struct{}
}

// MetricsRecorder defines the interface for recording webhook metrics.
// This allows the component to work with or without metrics.
type MetricsRecorder interface {
	RecordWebhookRequest(gvk, result string, durationSeconds float64)
	RecordWebhookValidation(gvk, result string)
}

// DryRunValidator defines the synchronous interface the webhook uses to
// validate resources. The implementation in pkg/controller/dryrunvalidator
// is a library, not a lifecycle component.
//
// Warnings are surfaced via AdmissionResponse.Warnings on both allow and
// deny paths so kubectl prints them as "Warning:" lines without blocking
// admission. Pluggable validators (e.g. the SPOA hub running in
// --validate-socket mode) populate this slice from their non-fatal
// diagnostics; the standard render+validate pipeline does not.
type DryRunValidator interface {
	ValidateDirect(ctx context.Context, gvk, namespace, name string, object, oldObject any, operation string) (allowed bool, reason string, warnings []string)
}

// Config configures the webhook component.
type Config struct {
	// Port is the HTTPS port for the webhook server.
	// Default: 9443
	Port int

	// Path is the URL path for validation requests.
	// Default: "/validate"
	Path string

	// CertDir, when set, is a directory containing tls.crt and tls.key
	// (typically the mounted webhook-cert Secret). The server reads them
	// per-handshake and hot-reloads a rotated certificate without a restart.
	// Takes precedence over CertPEM/KeyPEM; production uses this path.
	CertDir string

	// CertPEM is the PEM-encoded TLS certificate.
	// Used when CertDir is unset (e.g. tests pass certs directly).
	CertPEM []byte

	// KeyPEM is the PEM-encoded TLS private key.
	// Used when CertDir is unset.
	KeyPEM []byte

	// Rules defines which resources the webhook validates.
	// Used for registering validators by GVK.
	Rules []WebhookRule

	// DryRunValidator performs dry-run validation of resources.
	// Required when Rules is non-empty.
	DryRunValidator DryRunValidator

	// ResourceAdmissionTimeout bounds watched-resource dry-run validation.
	// Default: 9s. Keep it below the corresponding Kubernetes webhook
	// timeoutSeconds value so the controller can return a structured decision.
	ResourceAdmissionTimeout time.Duration

	// Server is a caller-owned listener; the component replaces only its validator generation.
	Server *webhook.Server

	// OnGenerationRetired releases resources captured by the installed table.
	OnGenerationRetired func()
}

// New creates a new webhook component.
//
// Parameters:
//   - logger: Structured logger
//   - config: Component configuration (must include CertPEM and KeyPEM)
//   - restMapper: RESTMapper for resolving resource kinds from GVR
//   - metrics: Optional metrics recorder (can be nil)
//
// Returns:
//   - A new Component instance ready to be started
func New(logger *slog.Logger, config *Config, restMapper meta.RESTMapper, metrics MetricsRecorder) *Component {
	// Apply defaults
	if config.Port == 0 {
		config.Port = DefaultWebhookPort
	}
	if config.Path == "" {
		config.Path = DefaultWebhookPath
	}
	if config.ResourceAdmissionTimeout <= 0 {
		config.ResourceAdmissionTimeout = DefaultResourceAdmissionTimeout
	}

	return &Component{
		logger:          logger.With("component", ComponentName),
		config:          *config,
		restMapper:      restMapper,
		metrics:         metrics,
		dryRunValidator: config.DryRunValidator,
		listening:       make(chan struct{}),
	}
}

// Listening returns a channel that is closed once the underlying webhook
// server has bound its TLS listener. Until this channel is closed, an
// admission request routed at the controller fails with "connection
// refused", because the listening socket simply doesn't exist yet.
func (c *Component) Listening() <-chan struct{} {
	return c.listening
}

// Start starts the webhook component.
//
// This method:
// 1. Validates TLS certificates from configuration
// 2. Creates and starts the webhook HTTPS server
// 3. Publishes lifecycle events
//
// The server continues running until the context is cancelled.
func (c *Component) Start(ctx context.Context) error {
	defer func() {
		if c.config.OnGenerationRetired != nil {
			c.config.OnGenerationRetired()
			c.config.OnGenerationRetired = nil
		}
	}()

	c.logger.Info("Starting webhook component",
		"port", c.config.Port,
		"path", c.config.Path)

	// Adoption is decided BEFORE the certificate check: the shared server was
	// built with its own TLS material, so an adopting component carries none and
	// would fail the check below — silently, because Start() returning early
	// leaves Listening() open and every caller waiting on it blocks forever.
	if c.config.Server != nil {
		return c.startAdopted(ctx)
	}
	return c.startOwned(ctx)
}

func (c *Component) startOwned(ctx context.Context) error {
	// Validate a certificate source is configured.
	if c.config.CertDir == "" {
		if len(c.config.CertPEM) == 0 {
			return errors.New("no webhook TLS certificate configured (set CertDir or CertPEM)")
		}
		if len(c.config.KeyPEM) == 0 {
			return errors.New("tls private key is empty")
		}
		c.logger.Info("Loading webhook TLS certificate from configuration",
			"cert_size", len(c.config.CertPEM), "key_size", len(c.config.KeyPEM))
	} else {
		// The mounted Secret directory is read per-handshake, so a
		// cert-manager rotation is picked up without an iteration restart.
		c.logger.Info("Loading webhook TLS certificate from directory (hot-reload on rotation)",
			"cert_dir", c.config.CertDir)
	}

	server, err := webhook.NewServer(&webhook.ServerConfig{
		Port:         c.config.Port,
		Path:         c.config.Path,
		CertDir:      c.config.CertDir,
		CertPEM:      c.config.CertPEM,
		KeyPEM:       c.config.KeyPEM,
		ReadTimeout:  timeouts.HTTPServerTimeout,
		WriteTimeout: c.serverWriteTimeout(),

		OnUnregisteredGVK: c.reportUnregisteredGVK,
	})
	if err != nil {
		return fmt.Errorf("creating webhook server: %w", err)
	}
	c.server = server

	if err := c.registerValidators(); err != nil {
		return err
	}

	// Create server context
	c.serverCtx, c.serverCancel = context.WithCancel(ctx)
	defer c.serverCancel()

	// Start server in goroutine
	serverErrCh := make(chan error, 1)
	go func() {
		err := c.server.Start(c.serverCtx)
		if err != nil {
			c.logger.Error("Webhook server error", "error", err)
		}
		serverErrCh <- err
	}()

	// Wait for the underlying listener to actually bind before logging
	// "started" or signalling readiness to the iteration sequencer. The
	// pure server signals via Listening() once net.Listen has returned;
	// this prevents the controller from advertising readiness while the
	// API server's first AdmissionReview would still bounce with
	// "connection refused".
	select {
	case <-c.server.Listening():
		close(c.listening)
	case err := <-serverErrCh:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			err = errors.New("webhook server stopped before binding")
		}
		return fmt.Errorf("webhook server failed before bind: %w", err)
	case <-ctx.Done():
		c.serverCancel()
		return errors.Join(ctx.Err(), <-serverErrCh)
	}

	c.logger.Info("Webhook server started",
		"port", c.config.Port,
		"path", c.config.Path)

	// Wait for shutdown or error. When CertDir is set (production) the
	// certificate is hot-reloaded in-process by the server's GetCertificate
	// callback, so a rotation is served without an iteration restart. The
	// fixed CertPEM/KeyPEM fallback (tests) is only refreshed on restart.
	select {
	case err := <-serverErrCh:
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = errors.New("webhook server stopped unexpectedly")
		}
		return fmt.Errorf("webhook server failed: %w", err)
	case <-ctx.Done():
		c.logger.Info("Webhook component shutting down")
		c.serverCancel()
		return <-serverErrCh
	}
}

// startAdopted runs the component against a caller-owned, already-bound server.
//
// It installs this iteration's validator table. On teardown it replaces that
// table with an empty fail-closed generation and drains every active request;
// the caller-owned listener remains bound for the next iteration.
func (c *Component) startAdopted(ctx context.Context) error {
	c.server = c.config.Server
	c.serverCtx = ctx

	if err := c.registerValidators(); err != nil {
		return err
	}

	// The caller bound the listener before handing it over, so readiness is
	// already satisfied — the sequencer must not block waiting for a bind that
	// happened in an earlier iteration.
	close(c.listening)

	c.logger.Info("Webhook validators installed on the persistent server",
		"port", c.config.Port,
		"path", c.config.Path)

	<-ctx.Done()
	// Only this iteration's own table is retired: after a hand-over the
	// successor's generation is already installed and keeps serving.
	if err := c.server.RetireValidatorGenerationIfCurrent(c.generation); err != nil {
		c.logger.Debug("Persistent webhook server stopped while retiring validators", "error", err)
	}
	c.logger.Info("Webhook validators retired; leaving the shared listener bound")
	return nil
}

func (c *Component) serverWriteTimeout() time.Duration {
	return max(
		c.config.ResourceAdmissionTimeout,
		timeouts.HTTPServerTimeout,
	) + webhookResponseGrace
}

// resolveKind uses RESTMapper to convert GVR (Group/Version/Resource) to Kind.
//
// This queries the Kubernetes API server's discovery information to get the
// authoritative mapping from resource names to kinds.
//
// Parameters:
//   - apiGroup: API group (empty string for core resources)
//   - apiVersion: API version (e.g., "v1", "v1beta1")
//   - resource: Plural resource name (e.g., "ingresses", "services")
//
// Returns:
//   - kind: Singular kind name (e.g., "Ingress", "Service")
//   - error: If resolution fails
func (c *Component) resolveKind(apiGroup, apiVersion, resource string) (string, error) {
	gvr := schema.GroupVersionResource{
		Group:    apiGroup,
		Version:  apiVersion,
		Resource: resource,
	}

	c.logger.Debug("Resolving kind from GVR",
		"group", apiGroup,
		"version", apiVersion,
		"resource", resource)

	gvk, err := c.restMapper.KindFor(gvr)
	if err != nil {
		return "", fmt.Errorf("resolving kind for %v: %w", gvr, err)
	}

	c.logger.Debug("Resolved kind",
		"resource", resource,
		"kind", gvk.Kind)

	return gvk.Kind, nil
}

// registerValidators installs the validator table for all configured webhook
// rules.
//
// This is called automatically during Start() after the server is created.
// It uses RESTMapper to resolve resource names to kinds.
//
// The table is built in full and installed as one generation. Incremental
// registration cannot remove a kind and exposes a half-built table.
func (c *Component) registerValidators() error {
	c.logger.Info("Registering validators")
	if len(c.config.Rules) > 0 && c.dryRunValidator == nil {
		return errors.New("webhook rules configured without a dry-run validator")
	}
	validators := make(map[string]webhook.ValidationFunc)

	// For each webhook rule, register a validator
	for _, rule := range c.config.Rules {
		// Resolve Kind from Resource using RESTMapper
		// The webhook server receives AdmissionRequests with Kind (e.g., "Ingress")
		// but we only have the resources field (e.g., "ingresses")
		// RESTMapper queries the Kubernetes API to get the authoritative mapping
		kind, err := c.resolveKind(
			rule.APIGroup,
			rule.APIVersion,
			rule.Resource,
		)
		if err != nil {
			return fmt.Errorf("registering validator for %s/%s/%s: %w",
				rule.APIGroup, rule.APIVersion, rule.Resource, err)
		}

		// Register the validator under EVERY version the apiserver serves for
		// this kind, not only the one the config resolved to.
		//
		// The chart renders the ValidatingWebhookConfiguration from the watched
		// resource's full apiVersions candidate list, while the effective config
		// carries only the single resolved version (effective.go clears the
		// candidates once one is picked). A version the webhook intercepts but
		// the table has no entry for is denied outright — failurePolicy is Fail
		// — so an HTTPRoute written as v1beta1 was rejected permanently on a
		// default install, with a message telling the operator to wait for an
		// initialization that had already finished.
		for _, version := range c.servedVersions(rule.APIGroup, kind, rule.APIVersion) {
			gvk := c.buildGVK(rule.APIGroup, version, kind)

			c.logger.Debug("Registering validator",
				"gvk", gvk,
				"kind", kind,
				"resource", rule.Resource)

			validators[gvk] = c.createResourceValidator(gvk)
		}
	}

	generation, err := c.server.InstallValidatorGeneration(
		validators,
		c.reportUnregisteredGVK,
		c.config.OnGenerationRetired,
	)
	if err != nil {
		return fmt.Errorf("installing webhook validator generation: %w", err)
	}
	c.generation = generation
	c.config.OnGenerationRetired = nil
	return nil
}

// buildGVK constructs a GVK string from API group, version, and kind.
// servedVersions returns every apiVersion the cluster serves for the given
// kind, so the validator table covers everything the webhook can be handed.
// resolved is always included and comes first, so a mapper that cannot answer
// (or a kind it does not know) degrades to exactly the previous behaviour
// rather than losing the version the config resolved to.
func (c *Component) servedVersions(apiGroup, kind, resolved string) []string {
	versions := []string{resolved}

	mappings, err := c.restMapper.RESTMappings(schema.GroupKind{Group: apiGroup, Kind: kind})
	if err != nil {
		// Not fatal: the resolved version still gets a validator, which is what
		// the watch itself uses. Other served versions stay unregistered and
		// keep being denied, so say so rather than swallowing it.
		c.logger.Warn("Cannot enumerate served versions; validating only the resolved one",
			"api_group", apiGroup,
			"kind", kind,
			"resolved_version", resolved,
			"error", err)
		return versions
	}

	for _, mapping := range mappings {
		if v := mapping.GroupVersionKind.Version; v != "" && v != resolved {
			versions = append(versions, v)
		}
	}
	return versions
}

func (c *Component) buildGVK(apiGroup, version, kind string) string {
	if apiGroup == "" {
		// Core API group
		return fmt.Sprintf("%s.%s", version, kind)
	}
	return fmt.Sprintf("%s/%s.%s", apiGroup, version, kind)
}

// createResourceValidator creates a ValidationFunc for a specific GVK.
//
// This validator performs:
// 1. Basic structural validation (metadata checks)
// 2. Dry-run validation via DryRunValidator (render + HAProxy validation).
func (c *Component) createResourceValidator(gvk string) webhook.ValidationFunc {
	return func(valCtx *webhook.ValidationContext) (bool, string, []string, error) {
		start := time.Now()

		c.logger.Debug("Validating resource",
			"gvk", gvk,
			"operation", valCtx.Operation,
			"namespace", valCtx.Namespace,
			"name", valCtx.Name)

		parent := c.serverCtx
		if parent != nil && parent.Err() != nil {
			c.recordValidationUnavailable(gvk, valCtx, start)
			return false, validationUnavailableReason, nil, nil
		}

		// Basic structural validation runs inline before delegating to ValidateDirect.
		if err := c.validateBasicStructure(validationObject(valCtx)); err != nil {
			c.logger.Info("Basic validation failed",
				"gvk", gvk,
				"namespace", valCtx.Namespace,
				"name", valCtx.Name,
				"error", err)

			duration := time.Since(start).Seconds()
			if c.metrics != nil {
				c.metrics.RecordWebhookRequest(gvk, "denied", duration)
				c.metrics.RecordWebhookValidation(gvk, "denied")
			}

			return false, err.Error(), nil, nil
		}

		// Dry-run validation (synchronous call into dryrunvalidator.ValidateDirect).
		if c.dryRunValidator == nil {
			c.recordValidationUnavailable(gvk, valCtx, start)
			return false, validationUnavailableReason, nil, nil
		}

		// Derive from c.serverCtx (set in Start()) so iteration shutdown
		// cancels in-flight validations promptly. Using context.Background()
		// would orphan validation work past server cancellation, delaying
		// graceful shutdown. ResourceAdmissionTimeout still bounds each admission
		// individually; watched-resource admission remains fail closed.
		// Fall back to context.Background() if serverCtx hasn't been set
		// yet — happens in unit tests that call this validator without
		// going through Start(); in production Start() always sets
		// serverCtx before RegisterValidator wires this closure up.
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, c.config.ResourceAdmissionTimeout)
		defer cancel()

		allowed, reason, warnings := c.dryRunValidator.ValidateDirect(
			ctx,
			gvk,
			valCtx.Namespace,
			valCtx.Name,
			valCtx.Object,
			valCtx.OldObject,
			valCtx.Operation,
		)
		if parent.Err() != nil {
			c.recordValidationUnavailable(gvk, valCtx, start)
			return false, validationUnavailableReason, nil, nil
		}

		// Record metrics
		duration := time.Since(start).Seconds()
		if c.metrics != nil {
			resultStr := "allowed"
			if !allowed {
				resultStr = "denied"
			}
			c.metrics.RecordWebhookRequest(gvk, resultStr, duration)
			c.metrics.RecordWebhookValidation(gvk, resultStr)
		}

		c.logger.Log(context.Background(), admissionLogLevel(allowed, len(warnings)), "Validation completed",
			"gvk", gvk,
			"operation", valCtx.Operation,
			"namespace", valCtx.Namespace,
			"name", valCtx.Name,
			"allowed", allowed,
			"reason", reason,
			"warnings", len(warnings),
			"duration_ms", time.Since(start).Milliseconds())

		return allowed, reason, warnings, nil
	}
}

func validationObject(valCtx *webhook.ValidationContext) any {
	if valCtx.Object != nil {
		return valCtx.Object
	}
	return valCtx.OldObject
}

func (c *Component) recordValidationUnavailable(gvk string, valCtx *webhook.ValidationContext, start time.Time) {
	c.logger.Error("Validation unavailable; denying resource",
		"gvk", gvk,
		"namespace", valCtx.Namespace,
		"name", valCtx.Name)

	if c.metrics != nil {
		c.metrics.RecordWebhookRequest(gvk, "denied", time.Since(start).Seconds())
		c.metrics.RecordWebhookValidation(gvk, "denied")
	}
}

// validateBasicStructure performs basic structural validation on a Kubernetes resource.
//
// The check is intentionally inlined here rather than living in a separate
// component — it's trivial enough that a dedicated subscriber + event hop
// would add latency on the admission path for no benefit.
//
// Checks:
//   - Object is a valid unstructured resource
//   - Metadata.name or metadata.generateName exists
func (c *Component) validateBasicStructure(object any) error {
	obj, ok := object.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("invalid object type: %T", object)
	}

	name := obj.GetName()
	generateName := obj.GetGenerateName()

	if name == "" && generateName == "" {
		return errors.New("metadata.name or metadata.generateName is required")
	}

	return nil
}

// reportUnregisteredGVK records a routed AdmissionReview that was denied
// because its kind has no validator.
func (c *Component) reportUnregisteredGVK(gvk string) {
	c.logger.Error("Admission request denied because its kind has no registered validator",
		"gvk", gvk)
	if c.metrics != nil {
		// The gvk is read off the AdmissionReview, and the listener does not
		// require client certificates — anything that can reach the Service can
		// invent one. A Prometheus label value keeps its series forever, so the
		// counter carries a fixed sentinel and the real gvk stays in the log
		// line above, where volume is bounded by retention rather than memory.
		c.metrics.RecordWebhookValidation(unregisteredGVKLabel, "unregistered")
	}
}

// admissionLogLevel returns slog.LevelDebug for a clean allow (admitted with
// no warnings) so steady-state successful admissions don't spam the log at
// INFO, and slog.LevelInfo for denials or warning-bearing admissions — the
// cases an operator actually wants to see. Mirrors the commentator's
// "demote to DEBUG when nothing notable happened" rule (see
// pkg/controller/commentator/log_levels.go).
//
// Only for watched resources, which arrive at cluster traffic rates. The
// HAProxyTemplateConfig gate logs unconditionally at INFO — see
// configAdmissionLogLevel.
func admissionLogLevel(allowed bool, warnings int) slog.Level {
	if allowed && warnings == 0 {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
