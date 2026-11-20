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

// Package validation provides pure validation services for HAProxy configuration.
package validation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
)

// Default directory paths for HAProxy validation.
const (
	// DefaultBaseDir is the default base directory for HAProxy configuration.
	DefaultBaseDir = "/etc/haproxy"

	// DefaultMapsDir is the default relative directory name for map files.
	DefaultMapsDir = "maps"

	// DefaultSSLCertsDir is the default relative directory name for SSL certificates.
	DefaultSSLCertsDir = "ssl"

	// DefaultGeneralDir is the default relative directory name for general files.
	DefaultGeneralDir = "general"
)

// Timeout constants for validation operations.
const (
	// DefaultValidationTimeout is the default timeout for validation operations.
	// This is used by event handlers and webhook validators to prevent indefinite hangs.
	// 30 seconds allows sufficient time for render + validate while preventing stuck requests.
	DefaultValidationTimeout = 30 * time.Second
)

var ErrValidatorUnavailable = errors.New("external validation unavailable; retry the request")

// ValidationResult contains the output of a validation operation.
type ValidationResult struct {
	// Valid is true if the configuration passed all validation phases.
	Valid bool

	// Error contains the validation error if Valid is false.
	Error error

	// Phase indicates which validation phase failed (semantic, render, setup).
	// Empty if validation succeeded.
	Phase string

	// DurationMs is the total validation duration in milliseconds.
	DurationMs int64

	// Warnings contains non-fatal diagnostics from additional validation stages.
	Warnings []string
}

// ErrorMessage returns a user-friendly error message.
// Returns empty string if validation succeeded.
func (r *ValidationResult) ErrorMessage() string {
	if r.Valid || r.Error == nil {
		return ""
	}
	return r.Error.Error()
}

// ValidationService is a pure service that validates HAProxy configuration.
//
// This service encapsulates temp directory lifecycle internally:
// - Creates an isolated temp directory for each validation
// - Writes config and auxiliary files
// - Runs haproxy -c for semantic validation
// - Cleans up temp directory after validation
//
// HAProxy's own verdict is the whole check: it is a strict superset of any
// parse HAPTIC could run over the same bytes (ADR-0022).
//
// The service can be called concurrently from multiple goroutines.
type ValidationService struct {
	logger *slog.Logger

	// skipDNSValidation controls whether to skip DNS resolution failures.
	// Use true for runtime validation (permissive) and false for webhook validation (strict).
	skipDNSValidation bool

	// checkGate serializes this service's `haproxy -c` runs. Nil shares the
	// process-wide default gate with the admission webhook.
	checkGate *dataplane.CheckGate

	verdicts      *dataplane.CheckVerdicts
	checkObserver CheckObserver

	// baseDir is the production base directory used in "default-path origin".
	// During validation, this is replaced with the temp directory path so
	// relative paths resolve correctly.
	baseDir string

	// Relative directory names for auxiliary files (must match RenderService output)
	mapsDir     string
	sslCertsDir string
	generalDir  string
}

// ValidationServiceConfig contains configuration for creating a ValidationService.
type ValidationServiceConfig struct {
	// Logger is the structured logger for logging.
	Logger *slog.Logger

	// SkipDNSValidation controls whether to skip DNS resolution failures during validation.
	// When true, servers with unresolvable hostnames start in DOWN state instead of failing.
	// When false (strict mode), DNS resolution failures cause validation to fail.
	SkipDNSValidation bool

	// BaseDir is the production base directory used in "default-path origin" directive.
	// During local validation, this is replaced with the temp directory path so that
	// HAProxy resolves relative paths from the temp directory instead of production paths.
	// Example: "/etc/haproxy"
	BaseDir string

	// MapsDir is the relative directory name for map files (e.g., "maps").
	// Should match the basename of the dataplane MapsDir config.
	MapsDir string

	// SSLCertsDir is the relative directory name for SSL certificates (e.g., "ssl").
	// Should match the basename of the dataplane SSLCertsDir config.
	SSLCertsDir string

	// GeneralDir is the relative directory name for general files (e.g., "general").
	// Should match the basename of the dataplane GeneralStorageDir config.
	GeneralDir string

	// CheckGate serializes this service's `haproxy -c` runs. Give the render
	// gate its own so admission never waits out a fleet-sized config check.
	CheckGate *dataplane.CheckGate

	// Verdicts answers an input set HAProxy already judged; services sharing
	// one never check the same bytes twice. Nil checks every time, and a
	// strict-DNS service never reuses a verdict.
	Verdicts *dataplane.CheckVerdicts

	// CheckObserver is told about every check this service asks HAProxy for.
	CheckObserver CheckObserver
}

// CheckObserver receives one check's result (see CheckResult) and whether its
// verdict was reused instead of running `haproxy -c`.
type CheckObserver func(result string, reused bool)

// Check results reported to a CheckObserver.
const (
	CheckPassed    = "passed"
	CheckRefused   = "refused"
	CheckError     = "error"
	CheckCancelled = "cancelled"
)

// CheckResult classifies a check's error: HAProxy's verdict, a cancellation,
// or a check that could not run.
func CheckResult(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return CheckPassed
	case errors.Is(err, dataplane.ErrHAProxyRefused):
		return CheckRefused
	case context.Cause(ctx) != nil:
		return CheckCancelled
	default:
		return CheckError
	}
}

// NewValidationService creates a new ValidationService.
func NewValidationService(cfg *ValidationServiceConfig) *ValidationService {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Use provided directory names or sensible defaults
	baseDir := cfg.BaseDir
	if baseDir == "" {
		baseDir = DefaultBaseDir
	}
	mapsDir := cfg.MapsDir
	if mapsDir == "" {
		mapsDir = DefaultMapsDir
	}
	sslCertsDir := cfg.SSLCertsDir
	if sslCertsDir == "" {
		sslCertsDir = DefaultSSLCertsDir
	}
	generalDir := cfg.GeneralDir
	if generalDir == "" {
		generalDir = DefaultGeneralDir
	}

	return &ValidationService{
		logger:            logger,
		skipDNSValidation: cfg.SkipDNSValidation,
		checkGate:         cfg.CheckGate,
		verdicts:          cfg.Verdicts,
		checkObserver:     cfg.CheckObserver,
		baseDir:           baseDir,
		mapsDir:           mapsDir,
		sslCertsDir:       sslCertsDir,
		generalDir:        generalDir,
	}
}

// ValidateWithChecksum validates HAProxy configuration. The checksum never
// authorizes skipping HAProxy's runtime verdict.
//
// This method:
// 1. Creates an isolated temp directory
// 2. Replaces production baseDir with temp directory in config (for default-path origin)
// 3. Writes the config and auxiliary files
// 4. Runs `haproxy -c` using the MODIFIED config
// 5. Cleans up the temp directory
//
// Parameters:
//   - ctx: Context for cancellation
//   - config: The rendered HAProxy configuration content
//   - auxFiles: Auxiliary files (maps, certificates, general files)
//   - checksum: Caller-provided content identity; it never authorizes verdict reuse
func (s *ValidationService) ValidateWithChecksum(ctx context.Context, config string, auxFiles *dataplane.AuxiliaryFiles, _ string) *ValidationResult {
	startTime := time.Now()

	// Check for context cancellation before starting
	if err := validationCancellationError(ctx); err != nil {
		return failedResult(err, "setup", startTime)
	}

	ownedAuxFiles := dataplane.CloneAuxiliaryFiles(auxFiles)
	return s.validateHAProxy(ctx, startTime, config, ownedAuxFiles)
}

// ValidateSnapshotWithChecksum validates an authenticated immutable auxiliary-file set.
func (s *ValidationService) ValidateSnapshotWithChecksum(
	ctx context.Context,
	config string,
	snapshot *renderartifact.Snapshot,
	_ string,
) *ValidationResult {
	startTime := time.Now()
	if err := validationCancellationError(ctx); err != nil {
		return failedResult(err, "setup", startTime)
	}
	if err := snapshot.ValidateAuthentication(); err != nil {
		return failedResult(fmt.Errorf("validating auxiliary-file snapshot: %w", err), "setup", startTime)
	}
	if err := validationCancellationError(ctx); err != nil {
		return failedResult(err, "setup", startTime)
	}
	auxFiles, err := dataplane.MaterializeAuxiliaryFileSnapshot(snapshot)
	if err != nil {
		return failedResult(fmt.Errorf("materializing auxiliary-file snapshot: %w", err), "setup", startTime)
	}
	return s.validateHAProxy(ctx, startTime, config, auxFiles)
}

// ValidateOutputSnapshotWithChecksum validates one authenticated complete render output.
func (s *ValidationService) ValidateOutputSnapshotWithChecksum(
	ctx context.Context,
	snapshot *renderoutput.Snapshot,
	_ string,
) *ValidationResult {
	startTime := time.Now()
	if err := validationCancellationError(ctx); err != nil {
		return failedResult(err, "setup", startTime)
	}
	if err := snapshot.ValidateAuthentication(); err != nil {
		return failedResult(fmt.Errorf("validating render output snapshot: %w", err), "setup", startTime)
	}
	authenticatedChecksum, err := snapshot.ContentChecksum()
	if err != nil {
		return failedResult(fmt.Errorf("reading render output checksum: %w", err), "setup", startTime)
	}
	config, err := snapshot.Config()
	if err != nil {
		return failedResult(fmt.Errorf("reading render output config: %w", err), "setup", startTime)
	}
	artifacts, err := snapshot.ArtifactSnapshot()
	if err != nil {
		return failedResult(fmt.Errorf("reading render output artifacts: %w", err), "setup", startTime)
	}
	result := s.ValidateSnapshotWithChecksum(ctx, config, artifacts, authenticatedChecksum)
	if !result.Valid {
		return result
	}
	if err := validationCancellationError(ctx); err != nil {
		return failedResult(err, "setup", startTime)
	}
	result.DurationMs = time.Since(startTime).Milliseconds()
	return result
}

func (s *ValidationService) validateHAProxy(
	ctx context.Context,
	startTime time.Time,
	config string,
	auxFiles *dataplane.AuxiliaryFiles,
) *ValidationResult {
	// Step 1: Create isolated temp directory for semantic validation
	tempDir, err := os.MkdirTemp("", "haproxy-validation-*")
	if err != nil {
		return failedResult(fmt.Errorf("creating temp directory: %w", err), "setup", startTime)
	}

	// Ensure cleanup happens regardless of validation outcome
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			s.logger.Warn("Failed to clean up validation temp directory",
				"temp_dir", tempDir,
				"error", err,
			)
		}
	}()

	// Step 2: Create modified config with temp directory paths for semantic validation
	// The rendered config contains "default-path origin /etc/haproxy" (or similar).
	// For local validation with haproxy -c, we need HAProxy to resolve relative paths
	// from the temp directory, so we replace the production path with the temp directory path.
	validationConfig := strings.Replace(config, "default-path origin "+s.baseDir, "default-path origin "+tempDir, 1)

	// Build validation paths using relative subdirectories
	// These must match the relative paths used by RenderService.
	// CRTListDir uses generalDir because CRT-list files are always stored alongside
	// general files.
	paths := &dataplane.ValidationPaths{
		TempDir:           tempDir,
		MapsDir:           filepath.Join(tempDir, s.mapsDir),
		SSLCertsDir:       filepath.Join(tempDir, s.sslCertsDir),
		CRTListDir:        filepath.Join(tempDir, s.generalDir),
		GeneralStorageDir: filepath.Join(tempDir, s.generalDir),
		ConfigFile:        filepath.Join(tempDir, names.MainTemplateName),
	}

	// Check for context cancellation before running semantic validation
	if err := validationCancellationError(ctx); err != nil {
		return failedResult(err, "setup", startTime)
	}

	// Step 3: Run semantic validation with haproxy -c using the MODIFIED config
	// This validates that HAProxy can actually load the config with all file references resolved.
	reused, err := dataplane.ValidateSemanticsWithVerdicts(ctx, validationConfig, auxFiles, paths, s.skipDNSValidation, s.checkGate, s.verdicts)
	if s.checkObserver != nil {
		s.checkObserver(CheckResult(ctx, err), reused)
	}
	if err != nil {
		return failedResult(err, validationPhase(err), startTime)
	}

	if err := validationCancellationError(ctx); err != nil {
		return failedResult(err, "setup", startTime)
	}

	return &ValidationResult{Valid: true, DurationMs: time.Since(startTime).Milliseconds()}
}

// failedResult builds a Valid=false ValidationResult with the elapsed time
// since startTime. Used by every error exit path in ValidateWithChecksum so the
// timing math and zero-value fields live in one place.
func failedResult(err error, phase string, startTime time.Time) *ValidationResult {
	return &ValidationResult{
		Valid:      false,
		Error:      err,
		Phase:      phase,
		DurationMs: time.Since(startTime).Milliseconds(),
	}
}

// validationPhase returns the phase tag carried by a *dataplane.ValidationError,
// or "unknown" for any other error type. Lets callers tag failed results with a
// consistent phase string regardless of the underlying error shape.
func validationPhase(err error) string {
	if valErr, ok := err.(*dataplane.ValidationError); ok {
		return valErr.Phase
	}
	return "unknown"
}

func validationCancellationError(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return fmt.Errorf("validation cancelled: %w", cause)
	}
	return nil
}
