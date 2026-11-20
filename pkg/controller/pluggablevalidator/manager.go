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

package pluggablevalidator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validation"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
)

// DefaultMaxParallelDispatch caps the number of concurrent
// (validator, file) round-trips ValidateAll runs in parallel.
// Per-validator connection-pool ceilings still apply, so this
// is a top-level safety net rather than a primary throttle —
// a render with 200 files spread across 3 validators won't
// spawn 600 goroutines all racing for socket time. Sized for
// typical reconciliation bursts.
const DefaultMaxParallelDispatch = 16

// ManagerConfig captures one entry from `spec.validators` on
// HAProxyTemplateConfig. The API schema validates fields it can express;
// core config validation and this constructor also reject malformed globs.
type ManagerConfig struct {
	// Name is the operator-facing validator identifier.
	Name string
	// SocketPath is the absolute filesystem path to the validator's
	// unix domain socket.
	SocketPath string
	// Files is the list of glob patterns matched against rendered
	// file paths to decide which files to send to this validator.
	// Patterns follow Go's `path/filepath.Match` rules.
	Files []string
	// DataFiles is the list of glob patterns for files this validator needs
	// in order to check the ones it validates, but must not validate on
	// their own — a WAF ruleset a hub config `Include`s. Every matching
	// file is attached to every request sent to this validator, marked
	// FileKindData.
	//
	// A file matching both lists is data: it is the reference target, and
	// parsing it as a config would produce a spurious error rather than a
	// finding about the config that references it.
	DataFiles []string
	// Timeout is the per-call deadline for one (file, validator)
	// request-response cycle. Zero falls back to DefaultTimeout.
	Timeout time.Duration
	// MaxConnections caps the controller-side pool to this
	// validator's socket. Zero falls back to DefaultMaxConnections.
	MaxConnections int
}

// Manager is the controller-side entry point for the validator-
// sidecar feature. It owns one Client per configured validator.
// Concurrent calls are safe; per-validator pools handle in-process
// parallelism.
//
// Routing model: the pipeline hands the Manager every rendered file.
// For each (file, validator) pair where
// the validator's globs match the file's path, the Manager sends a
// single-file request frame over the socket. All resulting diagnostics
// are concatenated and returned as one slice. Errors fail every pipeline
// caller; admission also surfaces warnings through
// `AdmissionResponse.Warnings`.
//
// Construction validates every invariant needed for safe dispatch. A failure
// aborts controller iteration construction.
type Manager struct {
	logger  *slog.Logger
	clients map[string]*Client
	configs []ManagerConfig // preserved for Healthy() iteration order
	// stagedRoot is where the rendered files will live on the HAProxy pod. It
	// describes the controller's own file namespace, so it is one value for
	// every validator rather than a per-validator setting.
	stagedRoot string
}

// ManagerOption configures a Manager beyond its per-validator entries.
type ManagerOption func(*Manager)

// WithStagedRoot declares the directory rendered files will live in at
// runtime, so a validator can resolve a config's references to the data files
// sent alongside it. See Request.StagedRoot.
func WithStagedRoot(root string) ManagerOption {
	return func(m *Manager) { m.stagedRoot = root }
}

// validateManagerConfig checks one validator entry against the entries already
// accepted. Split out of NewManager to keep that function under the cognitive-
// complexity limit as the config surface grows.
func validateManagerConfig(cfg *ManagerConfig, seen map[string]*Client) error {
	if cfg.Name == "" {
		return errors.New("validator config: empty name")
	}
	if cfg.SocketPath == "" {
		return fmt.Errorf("validator %q: empty socketPath", cfg.Name)
	}
	if len(cfg.Files) == 0 {
		return fmt.Errorf("validator %q: empty files glob list", cfg.Name)
	}
	if _, exists := seen[cfg.Name]; exists {
		return fmt.Errorf("validator %q: duplicate name", cfg.Name)
	}
	// Every glob is checked by matching an arbitrary string: filepath.Match
	// returns ErrBadPattern for a syntactically broken pattern, and the match
	// result itself is irrelevant.
	for _, g := range cfg.Files {
		if _, err := filepath.Match(g, "/probe"); err != nil {
			return fmt.Errorf("validator %q: invalid file glob %q: %w", cfg.Name, g, err)
		}
	}
	for _, g := range cfg.DataFiles {
		if _, err := filepath.Match(g, "/probe"); err != nil {
			return fmt.Errorf("validator %q: invalid data-file glob %q: %w", cfg.Name, g, err)
		}
	}
	return nil
}

// NewManager builds a Manager from the parsed `spec.validators`
// slice. A zero-length slice produces a no-op Manager whose
// Configured() returns false.
func NewManager(logger *slog.Logger, configs []ManagerConfig, opts ...ManagerOption) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	clients := make(map[string]*Client, len(configs))
	for _, cfg := range configs {
		if err := validateManagerConfig(&cfg, clients); err != nil {
			return nil, err
		}
		clients[cfg.Name] = NewClient(cfg.Name, cfg.SocketPath, cfg.Timeout, cfg.MaxConnections)
	}

	m := &Manager{
		logger:  logger.With(slog.String("component", "pluggablevalidator")),
		clients: clients,
		configs: append([]ManagerConfig(nil), configs...),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Configured reports whether any validators are registered. Callers
// (webhook, /healthz) skip the dispatch when no validators are
// configured.
func (m *Manager) Configured() bool {
	return len(m.clients) > 0
}

// Names returns the validator names in the order they were
// registered. For deterministic iteration in callers that need a
// stable order (e.g., /healthz output formatting).
func (m *Manager) Names() []string {
	out := make([]string, 0, len(m.configs))
	for _, c := range m.configs {
		out = append(out, c.Name)
	}
	return out
}

// ValidationOutcome bundles the warnings + errors collected across
// all (file, validator) round-trips for one ValidateAll call. Err is
// set when the complete dispatch could not finish, so an interrupted
// validation can never be mistaken for a valid verdict.
type ValidationOutcome struct {
	Warnings []Diagnostic
	Errors   []Diagnostic
	Err      error
}

// Result computes the aggregate result string from the populated
// lists. Mirrors the wire-protocol's per-response `result` field
// computation but at the cross-validator aggregate level.
func (o *ValidationOutcome) Result() Result {
	if o.Err != nil || len(o.Errors) > 0 {
		return ResultError
	}
	if len(o.Warnings) > 0 {
		return ResultWarning
	}
	return ResultValid
}

// dispatchTask captures one (validator, file) round-trip the
// fan-out scheduler needs to run.
type dispatchTask struct {
	validatorName string
	client        *Client
	file          File
	// dataFiles ride along with every request to this validator so the
	// config file can be checked against what it references.
	dataFiles []File
}

type dispatchResults struct {
	mu          sync.Mutex
	unavailable bool
	warnings    []Diagnostic
	errors      []Diagnostic
}

func (r *dispatchResults) append(resp *Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unavailable = r.unavailable || resp.unavailable()
	r.warnings = append(r.warnings, resp.Warnings...)
	r.errors = append(r.errors, resp.Errors...)
}

// ValidateAll fans the rendered files out to every configured
// validator whose globs match. Tasks run in parallel, bounded by
// DefaultMaxParallelDispatch — independent (validator, file) pairs
// are independent work units (different sockets, different
// content, no shared state) so there's no benefit to running them
// serially. Per-validator connection pools throttle within-
// validator concurrency.
//
// Returns aggregated diagnostics. The returned outcome is always
// non-nil; an empty outcome means nothing matched any glob (or no
// validators are configured). Diagnostics are sorted by
// (path, line, column, validator-name) so output is deterministic
// across runs even though tasks complete out of order.
//
// On transport failure to a single validator, that validator's
// errors are surfaced and the others continue. The caller decides
// whether to treat partial failures as admission denials (typical
// fail-closed behaviour: yes).
func (m *Manager) ValidateAll(ctx context.Context, files []File) *ValidationOutcome {
	out := &ValidationOutcome{}
	if err := externalValidationContextError(ctx); err != nil {
		out.Err = err
		return out
	}
	if !m.Configured() || len(files) == 0 {
		return out
	}

	tasks, err := m.buildDispatchTasks(ctx, files)
	if err != nil {
		out.Err = err
		return out
	}
	if err := externalValidationContextError(ctx); err != nil {
		out.Err = err
		return out
	}
	if len(tasks) == 0 {
		return out
	}
	return m.dispatchTasks(ctx, tasks)
}

func (m *Manager) buildDispatchTasks(ctx context.Context, files []File) ([]dispatchTask, error) {
	var tasks []dispatchTask
	for _, vcfg := range m.configs {
		if err := externalValidationContextError(ctx); err != nil {
			return nil, err
		}
		client := m.clients[vcfg.Name]
		data, err := matchFiles(ctx, files, vcfg.DataFiles)
		if err != nil {
			return nil, err
		}
		for i := range data {
			data[i].Kind = FileKindData
		}
		matchedFiles, err := matchFiles(ctx, files, vcfg.Files)
		if err != nil {
			return nil, err
		}
		for _, f := range matchedFiles {
			// Data wins over config for a file matching both globs: it is
			// the reference target, and validating it standalone would
			// report on the wrong thing.
			matches, err := matchesAny(ctx, f.Path, vcfg.DataFiles)
			if err != nil {
				return nil, err
			}
			if matches {
				continue
			}
			tasks = append(tasks, dispatchTask{
				validatorName: vcfg.Name,
				client:        client,
				file:          f,
				dataFiles:     data,
			})
		}
	}
	return tasks, nil
}

func (m *Manager) dispatchTasks(ctx context.Context, tasks []dispatchTask) *ValidationOutcome {
	concurrency := DefaultMaxParallelDispatch
	if concurrency > len(tasks) {
		concurrency = len(tasks)
	}
	sem := make(chan struct{}, concurrency)
	results := &dispatchResults{}
	var wg sync.WaitGroup
	dispatchErr := m.startDispatchTasks(ctx, tasks, sem, &wg, results)
	wg.Wait()

	out := &ValidationOutcome{Err: dispatchErr}
	if err := externalValidationContextError(ctx); err != nil {
		out.Err = err
	}

	if results.unavailable {
		out.Err = errors.Join(validation.ErrValidatorUnavailable, out.Err)
	}

	sortDiagnostics(results.warnings)
	sortDiagnostics(results.errors)
	out.Warnings = results.warnings
	out.Errors = results.errors
	return out
}

func (m *Manager) startDispatchTasks(
	ctx context.Context,
	tasks []dispatchTask,
	sem chan struct{},
	wg *sync.WaitGroup,
	results *dispatchResults,
) error {
	for _, task := range tasks {
		if err := externalValidationContextError(ctx); err != nil {
			return err
		}
		if err := acquireDispatchSlot(ctx, sem); err != nil {
			return err
		}
		if err := externalValidationContextError(ctx); err != nil {
			<-sem
			return err
		}
		wg.Add(1)
		go func(task dispatchTask) {
			defer wg.Done()
			defer func() { <-sem }()
			results.append(m.validateOne(ctx, task.client, task.validatorName, task.file, task.dataFiles))
		}(task)
	}
	return nil
}

func acquireDispatchSlot(ctx context.Context, sem chan<- struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return externalValidationContextError(ctx)
	}
}

// ValidateRenderedOutput implements pipeline.RenderedOutputValidator.
func (m *Manager) ValidateRenderedOutput(ctx context.Context, result *pipeline.PipelineResult) ([]string, error) {
	files, err := buildFiles(result)
	if err != nil {
		return nil, fmt.Errorf("reading rendered files: %w", err)
	}
	outcome := m.ValidateAll(ctx, files)
	warnings := formatDiagnostics(outcome.Warnings)
	var diagnosticErr error
	if len(outcome.Errors) > 0 {
		diagnosticErr = errors.New(formatErrorReason(outcome.Errors))
	}
	return warnings, errors.Join(diagnosticErr, outcome.Err)
}

func externalValidationContextError(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return fmt.Errorf("external validation did not finish: %w; retry the request", cause)
	}
	return nil
}

func buildFiles(result *pipeline.PipelineResult) ([]File, error) {
	config, artifactSnapshot, err := renderedFileAuthority(result)
	if err != nil {
		return nil, err
	}
	files := []File{{Path: "/etc/haproxy/haproxy.cfg", Content: config}}
	return appendSnapshotFiles(files, artifactSnapshot)
}

func renderedFileAuthority(result *pipeline.PipelineResult) (string, *renderartifact.Snapshot, error) {
	if result == nil || result.CycleSnapshot == nil {
		return "", nil, errors.New("rendered output has no authenticated render cycle")
	}
	output, err := result.CycleSnapshot.OutputSnapshot()
	if err != nil {
		return "", nil, fmt.Errorf("reading render cycle output: %w", err)
	}
	config, err := output.Config()
	if err != nil {
		return "", nil, fmt.Errorf("reading rendered config: %w", err)
	}
	artifacts, err := output.ArtifactSnapshot()
	if err != nil {
		return "", nil, fmt.Errorf("reading rendered artifacts: %w", err)
	}
	return config, artifacts, nil
}

func appendSnapshotFiles(files []File, snapshot *renderartifact.Snapshot) ([]File, error) {
	err := snapshot.Walk(func(artifact *renderartifact.Artifact) error {
		file, err := validatorFile(artifact)
		if err != nil {
			return err
		}
		files = append(files, file)
		return nil
	})
	return files, err
}

func validatorFile(artifact *renderartifact.Artifact) (File, error) {
	descriptor, err := artifact.Descriptor()
	if err != nil {
		return File{}, err
	}
	content, err := artifact.Content()
	if err != nil {
		return File{}, err
	}
	text, err := content.String()
	if err != nil {
		return File{}, err
	}
	return File{Path: descriptor.Path, Content: text}, nil
}

func formatDiagnostics(diags []Diagnostic) []string {
	if len(diags) == 0 {
		return nil
	}
	formatted := make([]string, 0, len(diags))
	for _, diagnostic := range diags {
		formatted = append(formatted, formatDiagnostic(diagnostic))
	}
	return formatted
}

func formatErrorReason(diags []Diagnostic) string {
	formatted := make([]string, 0, len(diags))
	for _, diagnostic := range diags {
		formatted = append(formatted, formatDiagnostic(diagnostic))
	}
	return strings.Join(formatted, "\n")
}

func formatDiagnostic(diagnostic Diagnostic) string {
	if diagnostic.Path == "" {
		return diagnostic.Message
	}
	if diagnostic.Line == 0 {
		return fmt.Sprintf("%s: %s", diagnostic.Path, diagnostic.Message)
	}
	if diagnostic.Column == 0 {
		return fmt.Sprintf("%s:%d: %s", diagnostic.Path, diagnostic.Line, diagnostic.Message)
	}
	return fmt.Sprintf("%s:%d:%d: %s", diagnostic.Path, diagnostic.Line, diagnostic.Column, diagnostic.Message)
}

// sortDiagnostics sorts in place by (path, line, column, message)
// so concurrent dispatch produces deterministic output for tests
// and deterministic admission denial messages for operators
// looking at `kubectl apply` errors twice in a row.
func sortDiagnostics(diags []Diagnostic) {
	sort.Slice(diags, func(i, j int) bool {
		a, b := diags[i], diags[j]
		switch {
		case a.Path != b.Path:
			return a.Path < b.Path
		case a.Line != b.Line:
			return a.Line < b.Line
		case a.Column != b.Column:
			return a.Column < b.Column
		default:
			return a.Message < b.Message
		}
	})
}

// validateOne dispatches a single file to a validator. Used internally by
// ValidateAll.
func (m *Manager) validateOne(
	ctx context.Context,
	client *Client,
	validatorName string,
	file File,
	dataFiles []File,
) *Response {
	req := &Request{
		ProtocolVersion: ProtocolVersion,
		Files:           append([]File{file}, dataFiles...),
		StagedRoot:      m.stagedRoot,
	}
	resp, err := client.Validate(ctx, req)
	if err != nil {
		// Misuse path (nil request); we don't get here in normal
		// use since we just built the request. Surface defensively.
		return ProtocolError(fmt.Sprintf(
			"validator %q: %v", validatorName, err,
		))
	}

	if resp.IsSynthetic() {
		m.logger.Debug("Validator protocol failure",
			slog.String("validator", validatorName),
			slog.String("path", file.Path))
	}
	return resp
}

// matchesAny reports whether path matches any of the globs. A malformed glob
// cannot reach here — the Manager rejects those at construction.
func matchesAny(ctx context.Context, path string, globs []string) (bool, error) {
	if err := externalValidationContextError(ctx); err != nil {
		return false, err
	}
	for _, g := range globs {
		if ok, err := filepath.Match(g, path); err == nil && ok {
			return true, nil
		}
	}
	return false, externalValidationContextError(ctx)
}

// matchFiles returns the matching files or the context cancellation error.
func matchFiles(ctx context.Context, files []File, globs []string) ([]File, error) {
	if len(files) == 0 || len(globs) == 0 {
		return nil, externalValidationContextError(ctx)
	}
	matched := make([]File, 0, len(files))
	for _, f := range files {
		if err := externalValidationContextError(ctx); err != nil {
			return nil, err
		}
		for _, g := range globs {
			ok, err := filepath.Match(g, f.Path)
			if err != nil || !ok {
				continue
			}
			matched = append(matched, f)
			break
		}
	}
	return matched, externalValidationContextError(ctx)
}

// Healthy reports whether every configured validator socket passes
// HealthCheck. Returns (true, nil) when all are healthy; otherwise
// returns (false, failures) where each failure entry is
// "<validator-name>: <reason>". Iteration order matches Names().
//
// Designed for /healthz injection — sub-millisecond happy path so it
// can run on every Kubernetes probe interval.
func (m *Manager) Healthy() (ok bool, failures []string) {
	if len(m.configs) == 0 {
		return true, nil
	}
	failures = make([]string, 0, len(m.configs))
	for _, c := range m.configs {
		if err := HealthCheck(c.SocketPath); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", c.Name, err))
		}
	}
	if len(failures) > 0 {
		return false, failures
	}
	return true, nil
}

// Close shuts every per-validator pool. Used during iteration
// teardown.
func (m *Manager) Close() {
	for _, c := range m.clients {
		c.Close()
	}
}
