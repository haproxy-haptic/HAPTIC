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

package pluggablevalidator_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	pv "github.com/haproxy-haptic/HAPTIC/pkg/controller/pluggablevalidator"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pluggablevalidator/testutil"
	controllertestutil "github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validation"
)

func tomlFile() pv.File {
	return pv.File{
		Path:    "/etc/haproxy-spoa-hub/config.toml",
		Content: "[hub]\nlisten = \"0.0.0.0:9000\"\n",
	}
}

func tomlGlob() []string {
	return []string{"/etc/haproxy-spoa-hub/*.toml"}
}

type cancelOnErrCheckContext struct {
	context.Context
	cancel   context.CancelCauseFunc
	cause    error
	lookups  int
	cancelAt int
}

func (c *cancelOnErrCheckContext) Err() error {
	c.lookups++
	if c.lookups == c.cancelAt {
		c.cancel(c.cause)
	}
	return c.Context.Err()
}

func TestManager_NewManager_RejectsDuplicateNames(t *testing.T) {
	configs := []pv.ManagerConfig{
		{Name: "coraza", SocketPath: "/x", Files: tomlGlob()},
		{Name: "coraza", SocketPath: "/y", Files: tomlGlob()},
	}
	if _, err := pv.NewManager(nil, configs); err == nil {
		t.Fatal("expected error for duplicate validator names")
	}
}

func TestManager_NewManager_RejectsEmptyName(t *testing.T) {
	if _, err := pv.NewManager(nil, []pv.ManagerConfig{{SocketPath: "/x", Files: tomlGlob()}}); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestManager_NewManager_RejectsEmptySocketPath(t *testing.T) {
	if _, err := pv.NewManager(nil, []pv.ManagerConfig{{Name: "coraza", Files: tomlGlob()}}); err == nil {
		t.Fatal("expected error for empty socketPath")
	}
}

func TestManager_NewManager_RejectsEmptyFiles(t *testing.T) {
	if _, err := pv.NewManager(nil, []pv.ManagerConfig{{Name: "coraza", SocketPath: "/x"}}); err == nil {
		t.Fatal("expected error for empty files glob list")
	}
}

func TestManager_NewManager_RejectsBadGlob(t *testing.T) {
	bad := []pv.ManagerConfig{
		{Name: "coraza", SocketPath: "/x", Files: []string{"/etc/[unclosed"}},
	}
	if _, err := pv.NewManager(nil, bad); err == nil {
		t.Fatal("expected error for malformed glob")
	}
}

func TestManager_NoValidators(t *testing.T) {
	mgr, err := pv.NewManager(nil, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if mgr.Configured() {
		t.Fatal("Configured() must be false when no validators are registered")
	}
	ok, failures := mgr.Healthy()
	if !ok || failures != nil {
		t.Fatalf("empty Manager Healthy() = (%v, %v), want (true, nil)", ok, failures)
	}
	out := mgr.ValidateAll(context.Background(), []pv.File{tomlFile()})
	if out == nil {
		t.Fatal("ValidateAll must return non-nil outcome")
	}
	if out.Result() != pv.ResultValid {
		t.Fatalf("no-validator outcome must be Valid, got %q", out.Result())
	}
}

func TestManager_ValidateAll_PreCanceledContextFailsClosed(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	if err := srv.SetResponse(&pv.Response{ProtocolVersion: pv.ProtocolVersion, Result: pv.ResultValid}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}
	mgr, err := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: srv.SocketPath, Files: tomlGlob(), Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	authorityErr := errors.New("validator authority expired")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(authorityErr)
	out := mgr.ValidateAll(ctx, []pv.File{tomlFile()})

	if out.Result() != pv.ResultError {
		t.Fatalf("result=%q want %q", out.Result(), pv.ResultError)
	}
	if !errors.Is(out.Err, authorityErr) {
		t.Fatalf("error=%v does not preserve cancellation cause %v", out.Err, authorityErr)
	}
	if got := len(srv.Requests()); got != 0 {
		t.Fatalf("pre-canceled validation dispatched %d requests, want 0", got)
	}
}

func TestManager_ValidateAll_CancellationDuringNoMatchRoutingFailsClosed(t *testing.T) {
	mgr, err := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: "/unused", Files: tomlGlob(), Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	authorityErr := errors.New("validator authority expired")
	baseCtx, cancel := context.WithCancelCause(context.Background())
	ctx := &cancelOnErrCheckContext{
		Context:  baseCtx,
		cancel:   cancel,
		cause:    authorityErr,
		cancelAt: 2,
	}
	out := mgr.ValidateAll(ctx, []pv.File{{
		Path:    "/etc/haproxy/haproxy.cfg",
		Content: "global\n    daemon\n",
	}})

	if out.Result() != pv.ResultError {
		t.Fatalf("result=%q want %q", out.Result(), pv.ResultError)
	}
	if !errors.Is(out.Err, authorityErr) {
		t.Fatalf("error=%v does not preserve cancellation cause %v", out.Err, authorityErr)
	}
}

func TestManager_ValidateAll_CanceledDispatchFailsClosed(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	if err := srv.SetResponse(&pv.Response{ProtocolVersion: pv.ProtocolVersion, Result: pv.ResultValid}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}
	responseGate := make(chan struct{})
	srv.SetResponseGate(responseGate)
	mgr, err := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: srv.SocketPath, Files: tomlGlob(), Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	authorityErr := errors.New("validator authority expired")
	ctx, cancel := context.WithCancelCause(context.Background())
	result := make(chan *pv.ValidationOutcome, 1)
	go func() {
		result <- mgr.ValidateAll(ctx, []pv.File{tomlFile()})
	}()

	select {
	case <-srv.RequestReceived():
	case <-time.After(time.Second):
		t.Fatal("validator did not receive the dispatched request")
	}
	cancel(authorityErr)
	close(responseGate)

	select {
	case out := <-result:
		if out.Result() != pv.ResultError {
			t.Fatalf("result=%q want %q", out.Result(), pv.ResultError)
		}
		if !errors.Is(out.Err, authorityErr) {
			t.Fatalf("error=%v does not preserve cancellation cause %v", out.Err, authorityErr)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled validation did not return")
	}
}

func TestManager_ValidateAll_RoutesByGlob(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	if err := srv.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultValid,
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: srv.SocketPath, Files: tomlGlob(), Timeout: time.Second},
	})

	files := []pv.File{
		tomlFile(), // matches glob → sent
		{Path: "/etc/haproxy/haproxy.cfg", Content: "frontend foo"}, // doesn't match → skipped
	}
	out := mgr.ValidateAll(context.Background(), files)
	if out.Result() != pv.ResultValid {
		t.Fatalf("result=%q want %q", out.Result(), pv.ResultValid)
	}
	if got := len(srv.Requests()); got != 1 {
		t.Fatalf("server saw %d requests, want 1 (only glob-matching file should be sent)", got)
	}
}

func TestManager_ValidateAll_FanOutToMultipleValidators(t *testing.T) {
	srv1 := testutil.NewFixtureServer(t)
	srv2 := testutil.NewFixtureServer(t)
	for _, s := range []*testutil.FixtureServer{srv1, srv2} {
		if err := s.SetResponse(&pv.Response{ProtocolVersion: pv.ProtocolVersion, Result: pv.ResultValid}); err != nil {
			t.Fatalf("SetResponse: %v", err)
		}
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: srv1.SocketPath, Files: tomlGlob(), Timeout: time.Second},
		{Name: "otel", SocketPath: srv2.SocketPath, Files: tomlGlob(), Timeout: time.Second},
	})

	out := mgr.ValidateAll(context.Background(), []pv.File{tomlFile()})
	if out.Result() != pv.ResultValid {
		t.Fatalf("result=%q want %q", out.Result(), pv.ResultValid)
	}
	if got := len(srv1.Requests()); got != 1 {
		t.Fatalf("srv1 saw %d, want 1", got)
	}
	if got := len(srv2.Requests()); got != 1 {
		t.Fatalf("srv2 saw %d, want 1 — same file routed to both matching validators", got)
	}
}

func TestManager_ValidateAll_RevalidatesRepeatedInput(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	if err := srv.SetResponse(&pv.Response{ProtocolVersion: pv.ProtocolVersion, Result: pv.ResultValid}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: srv.SocketPath, Files: tomlGlob(), Timeout: time.Second},
	})

	files := []pv.File{tomlFile()}
	if got := mgr.ValidateAll(context.Background(), files).Result(); got != pv.ResultValid {
		t.Fatalf("first result=%q want %q", got, pv.ResultValid)
	}
	if err := srv.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultError,
		Errors: []pv.Diagnostic{{
			Path:    tomlFile().Path,
			Message: "validator implementation changed",
		}},
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}
	if got := mgr.ValidateAll(context.Background(), files).Result(); got != pv.ResultError {
		t.Fatalf("second result=%q want %q", got, pv.ResultError)
	}
	if got := len(srv.Requests()); got != 2 {
		t.Fatalf("server saw %d requests, want 2", got)
	}
}

func TestManager_ValidateAll_ABARevalidatesReturnedInput(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	setResult := func(result pv.Result, message string) {
		t.Helper()
		response := &pv.Response{ProtocolVersion: pv.ProtocolVersion, Result: result}
		if result == pv.ResultError {
			response.Errors = []pv.Diagnostic{{Path: tomlFile().Path, Message: message}}
		}
		if err := srv.SetResponse(response); err != nil {
			t.Fatalf("SetResponse: %v", err)
		}
	}

	mgr, err := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: srv.SocketPath, Files: tomlGlob(), Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	a := tomlFile()
	b := tomlFile()
	b.Content += "[plugins]\n"

	setResult(pv.ResultValid, "")
	if got := mgr.ValidateAll(context.Background(), []pv.File{a}).Result(); got != pv.ResultValid {
		t.Fatalf("A1 result=%q want %q", got, pv.ResultValid)
	}
	setResult(pv.ResultError, "B rejected")
	if got := mgr.ValidateAll(context.Background(), []pv.File{b}).Result(); got != pv.ResultError {
		t.Fatalf("B result=%q want %q", got, pv.ResultError)
	}
	setResult(pv.ResultError, "A rejected by current runtime")
	third := mgr.ValidateAll(context.Background(), []pv.File{a})
	if got := third.Result(); got != pv.ResultError {
		t.Fatalf("A2 result=%q want %q", got, pv.ResultError)
	}
	if len(third.Errors) != 1 || third.Errors[0].Message != "A rejected by current runtime" {
		t.Fatalf("A2 errors=%v", third.Errors)
	}
	if got := len(srv.Requests()); got != 3 {
		t.Fatalf("server saw %d requests, want 3", got)
	}
}

func TestManager_ValidateAll_ReturnMutationCannotPoisonLaterVerdict(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	setWarning := func(message string) {
		t.Helper()
		if err := srv.SetResponse(&pv.Response{
			ProtocolVersion: pv.ProtocolVersion,
			Result:          pv.ResultWarning,
			Warnings: []pv.Diagnostic{{
				Path:    tomlFile().Path,
				Message: message,
			}},
		}); err != nil {
			t.Fatalf("SetResponse: %v", err)
		}
	}

	mgr, err := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: srv.SocketPath, Files: tomlGlob(), Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	setWarning("first runtime verdict")
	first := mgr.ValidateAll(context.Background(), []pv.File{tomlFile()})
	if len(first.Warnings) != 1 {
		t.Fatalf("first warnings=%v", first.Warnings)
	}
	first.Warnings[0].Message = "caller mutation"

	setWarning("fresh runtime verdict")
	second := mgr.ValidateAll(context.Background(), []pv.File{tomlFile()})
	if len(second.Warnings) != 1 || second.Warnings[0].Message != "fresh runtime verdict" {
		t.Fatalf("second warnings=%v", second.Warnings)
	}
	if got := len(srv.Requests()); got != 2 {
		t.Fatalf("server saw %d requests, want 2", got)
	}
}

func TestManager_ValidateAll_SidecarRestartCannotInheritVerdict(t *testing.T) {
	firstRuntime := testutil.NewFixtureServer(t)
	if err := firstRuntime.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultValid,
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}
	mgr, err := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: firstRuntime.SocketPath, Files: tomlGlob(), Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	files := []pv.File{tomlFile()}
	if got := mgr.ValidateAll(context.Background(), files).Result(); got != pv.ResultValid {
		t.Fatalf("first runtime result=%q want %q", got, pv.ResultValid)
	}

	socketPath := firstRuntime.SocketPath
	firstRuntime.Stop()
	secondRuntime := testutil.NewFixtureServerAtPath(t, socketPath)
	if err := secondRuntime.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultError,
		Errors: []pv.Diagnostic{{
			Path:    tomlFile().Path,
			Message: "new runtime rejects input",
		}},
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}

	second := mgr.ValidateAll(context.Background(), files)
	if got := second.Result(); got != pv.ResultError {
		t.Fatalf("second runtime result=%q want %q", got, pv.ResultError)
	}
	if len(second.Errors) != 1 || second.Errors[0].Message != "new runtime rejects input" {
		t.Fatalf("second runtime errors=%v", second.Errors)
	}
	if got := len(firstRuntime.Requests()); got != 1 {
		t.Fatalf("first runtime saw %d requests, want 1", got)
	}
	if got := len(secondRuntime.Requests()); got != 1 {
		t.Fatalf("second runtime saw %d requests, want 1", got)
	}
}

func TestManager_ValidateAll_TransportErrorFailsEveryCall(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sock")
	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "broken", SocketPath: missing, Files: tomlGlob(), Timeout: 200 * time.Millisecond},
	})

	files := []pv.File{tomlFile()}
	out1 := mgr.ValidateAll(context.Background(), files)
	if out1.Result() != pv.ResultError {
		t.Fatalf("result=%q want %q", out1.Result(), pv.ResultError)
	}
	out2 := mgr.ValidateAll(context.Background(), files)
	if out2.Result() != pv.ResultError {
		t.Fatalf("result=%q want %q", out2.Result(), pv.ResultError)
	}
	if !errors.Is(out1.Err, validation.ErrValidatorUnavailable) || !errors.Is(out2.Err, validation.ErrValidatorUnavailable) {
		t.Fatalf("missing unavailable classification: %v, %v", out1.Err, out2.Err)
	}
	// Both calls must surface a fresh transport error.
	if len(out1.Errors) == 0 || len(out2.Errors) == 0 {
		t.Fatal("expected at least one error per call")
	}
}

func TestManager_ValidateRenderedOutput_RejectsInconsistentResultWithoutCaching(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	if err := srv.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultError,
		Warnings:        []pv.Diagnostic{},
		Errors:          []pv.Diagnostic{},
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}

	mgr, err := pv.NewManager(nil, []pv.ManagerConfig{{
		Name:       "contract-check",
		SocketPath: srv.SocketPath,
		Files:      []string{"/etc/haproxy/haproxy.cfg"},
		Timeout:    time.Second,
	}})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()
	result := exactExternalValidatorResult(t, "global\n")

	_, validationErr := mgr.ValidateRenderedOutput(context.Background(), result)
	if validationErr == nil {
		t.Fatal("result:error without error diagnostics passed pipeline validation")
	}
	if !strings.Contains(validationErr.Error(), `use "valid"`) {
		t.Fatalf("validation error %q does not explain the response contract", validationErr)
	}

	if err := srv.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultValid,
		Warnings:        []pv.Diagnostic{},
		Errors:          []pv.Diagnostic{},
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}
	if _, err := mgr.ValidateRenderedOutput(context.Background(), result); err != nil {
		t.Fatalf("conforming response after protocol failure: %v", err)
	}
	if got := len(srv.Requests()); got != 2 {
		t.Fatalf("server saw %d requests, want 2", got)
	}

	if _, err := mgr.ValidateRenderedOutput(context.Background(), result); err != nil {
		t.Fatalf("revalidated conforming response: %v", err)
	}
	if got := len(srv.Requests()); got != 3 {
		t.Fatalf("server saw %d requests after repeated input, want 3", got)
	}
}

func exactExternalValidatorResult(t *testing.T, config string) *pipeline.PipelineResult {
	t.Helper()
	cycle := controllertestutil.NewRenderCycleFixture(t).Snapshot(t, config, nil, nil)
	output, err := cycle.OutputSnapshot()
	if err != nil {
		t.Fatalf("reading test output: %v", err)
	}
	return &pipeline.PipelineResult{CycleSnapshot: cycle, OutputSnapshot: output}
}

func TestManager_ValidateAll_RealValidatorPathlessErrorIsRevalidated(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	if err := srv.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultError,
		Warnings:        []pv.Diagnostic{},
		Errors: []pv.Diagnostic{
			{Path: "", Line: 0, Column: 0, Message: "internal validator error: panic: foo"},
		},
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: srv.SocketPath, Files: tomlGlob(), Timeout: time.Second},
	})

	files := []pv.File{tomlFile()}
	mgr.ValidateAll(context.Background(), files)
	mgr.ValidateAll(context.Background(), files)
	if got := len(srv.Requests()); got != 2 {
		t.Fatalf("server saw %d requests, want 2", got)
	}
}

// Regression: ValidateAll dispatches (validator, file) tasks in
// parallel rather than serialising them. Three validators each
// with a 200ms response delay must complete in well under
// 3×200ms = 600ms (sequential lower bound) — closer to 200ms +
// overhead.
func TestManager_ValidateAll_RunsValidatorsInParallel(t *testing.T) {
	srv1 := testutil.NewFixtureServer(t)
	srv2 := testutil.NewFixtureServer(t)
	srv3 := testutil.NewFixtureServer(t)
	for _, s := range []*testutil.FixtureServer{srv1, srv2, srv3} {
		if err := s.SetResponse(&pv.Response{ProtocolVersion: pv.ProtocolVersion, Result: pv.ResultValid}); err != nil {
			t.Fatalf("SetResponse: %v", err)
		}
		s.SetResponseDelay(200 * time.Millisecond)
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "v1", SocketPath: srv1.SocketPath, Files: tomlGlob(), Timeout: 2 * time.Second},
		{Name: "v2", SocketPath: srv2.SocketPath, Files: tomlGlob(), Timeout: 2 * time.Second},
		{Name: "v3", SocketPath: srv3.SocketPath, Files: tomlGlob(), Timeout: 2 * time.Second},
	})

	start := time.Now()
	out := mgr.ValidateAll(context.Background(), []pv.File{tomlFile()})
	elapsed := time.Since(start)
	if out.Result() != pv.ResultValid {
		t.Fatalf("result=%q want %q", out.Result(), pv.ResultValid)
	}
	// Sequential lower bound is 600ms (3 × 200ms). Parallel
	// completes near 200ms + dispatch overhead. Allow generous
	// CI slack — we're guarding against accidental serialisation,
	// not benchmarking.
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("ValidateAll elapsed=%v — looks serialised, expected ~200ms (parallel)", elapsed)
	}
}

// Regression: diagnostics are sorted deterministically across
// concurrent dispatch so admission denial messages are stable
// from one run to the next.
func TestManager_ValidateAll_DiagnosticsSortedDeterministically(t *testing.T) {
	srvA := testutil.NewFixtureServer(t)
	if err := srvA.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultError,
		Errors: []pv.Diagnostic{
			{Path: "/etc/x/c.toml", Line: 5, Message: "second"},
		},
	}); err != nil {
		t.Fatalf("SetResponse A: %v", err)
	}
	srvB := testutil.NewFixtureServer(t)
	if err := srvB.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultError,
		Errors: []pv.Diagnostic{
			{Path: "/etc/x/a.toml", Line: 3, Message: "first"},
		},
	}); err != nil {
		t.Fatalf("SetResponse B: %v", err)
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "v-a", SocketPath: srvA.SocketPath, Files: []string{"/etc/x/c.toml"}, Timeout: time.Second},
		{Name: "v-b", SocketPath: srvB.SocketPath, Files: []string{"/etc/x/a.toml"}, Timeout: time.Second},
	})

	files := []pv.File{
		{Path: "/etc/x/c.toml", Content: "c"},
		{Path: "/etc/x/a.toml", Content: "a"},
	}
	out := mgr.ValidateAll(context.Background(), files)
	if len(out.Errors) != 2 {
		t.Fatalf("got %d errors, want 2", len(out.Errors))
	}
	// Sorted by path: /etc/x/a.toml comes before /etc/x/c.toml.
	if out.Errors[0].Path != "/etc/x/a.toml" || out.Errors[1].Path != "/etc/x/c.toml" {
		t.Fatalf("errors not sorted by path: %+v", out.Errors)
	}
}

func TestManager_ValidateAll_AggregatesWarningAndError(t *testing.T) {
	srvWarn := testutil.NewFixtureServer(t)
	if err := srvWarn.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultWarning,
		Warnings: []pv.Diagnostic{
			{Path: "/etc/haproxy-spoa-hub/config.toml", Line: 2, Message: "deprecated directive"},
		},
		Errors: []pv.Diagnostic{},
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}
	srvErr := testutil.NewFixtureServer(t)
	if err := srvErr.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultError,
		Warnings:        []pv.Diagnostic{},
		Errors: []pv.Diagnostic{
			{Path: "/etc/haproxy-spoa-hub/config.toml", Line: 5, Message: "syntax error"},
		},
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "warn-validator", SocketPath: srvWarn.SocketPath, Files: tomlGlob(), Timeout: time.Second},
		{Name: "err-validator", SocketPath: srvErr.SocketPath, Files: tomlGlob(), Timeout: time.Second},
	})

	out := mgr.ValidateAll(context.Background(), []pv.File{tomlFile()})
	if out.Result() != pv.ResultError {
		t.Fatalf("result=%q want %q (any error wins over warning)", out.Result(), pv.ResultError)
	}
	if len(out.Warnings) != 1 || out.Warnings[0].Message != "deprecated directive" {
		t.Fatalf("warnings = %v, want exactly the warn-validator's entry", out.Warnings)
	}
	if len(out.Errors) != 1 || out.Errors[0].Message != "syntax error" {
		t.Fatalf("errors = %v, want exactly the err-validator's entry", out.Errors)
	}
}

func TestManager_Names_PreservesOrder(t *testing.T) {
	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "first", SocketPath: "/a", Files: tomlGlob()},
		{Name: "second", SocketPath: "/b", Files: tomlGlob()},
		{Name: "third", SocketPath: "/c", Files: tomlGlob()},
	})
	got := mgr.Names()
	want := []string{"first", "second", "third"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}

func TestManager_Healthy_AllUp(t *testing.T) {
	srv1 := testutil.NewFixtureServer(t)
	srv2 := testutil.NewFixtureServer(t)
	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: srv1.SocketPath, Files: tomlGlob()},
		{Name: "otel", SocketPath: srv2.SocketPath, Files: tomlGlob()},
	})
	ok, failures := mgr.Healthy()
	if !ok || failures != nil {
		t.Fatalf("Healthy() = (%v, %v), want (true, nil)", ok, failures)
	}
}

func TestManager_Healthy_OneDown(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	missing := filepath.Join(t.TempDir(), "missing.sock")
	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: srv.SocketPath, Files: tomlGlob()},
		{Name: "otel", SocketPath: missing, Files: tomlGlob()},
	})
	ok, failures := mgr.Healthy()
	if ok {
		t.Fatal("Healthy() returned true with one socket missing")
	}
	if len(failures) != 1 {
		t.Fatalf("failures=%v, want exactly 1 entry", failures)
	}
	if !strings.HasPrefix(failures[0], "otel:") {
		t.Fatalf("failure should identify the validator name; got %q", failures[0])
	}
}

// A validator that needs a file in order to check another one gets both in a
// single request: the config it validates, plus every dataFiles match marked
// as data. The motivating case is a hub config that Includes a WAF ruleset —
// the validator runs in the controller pod and cannot read the HAProxy pod's
// disk, so without the content travelling along there is nothing to resolve
// the reference against.
func TestManager_ValidateAll_AttachesDataFiles(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	if err := srv.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultValid,
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{{
		Name:       "coraza",
		SocketPath: srv.SocketPath,
		Files:      tomlGlob(),
		DataFiles:  []string{"/etc/haproxy/general/crs/*"},
		Timeout:    time.Second,
	}})

	files := []pv.File{
		tomlFile(),
		{Path: "/etc/haproxy/general/crs/REQUEST-901-INIT.conf", Content: "SecAction id:901000"},
		{Path: "/etc/haproxy/general/crs/lfi-os-files.data", Content: "/etc/passwd"},
		{Path: "/etc/haproxy/haproxy.cfg", Content: "frontend foo"},
	}

	out := mgr.ValidateAll(context.Background(), files)
	if out.Result() != pv.ResultValid {
		t.Fatalf("result=%q want %q", out.Result(), pv.ResultValid)
	}

	// One request — the data files ride along, they are not dispatched
	// separately. Sending them on their own would have the validator parse a
	// SecLang ruleset as TOML.
	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}

	var req pv.Request
	if err := json.Unmarshal(reqs[0], &req); err != nil {
		t.Fatalf("decoding request: %v", err)
	}
	if len(req.Files) != 3 {
		t.Fatalf("request carried %d files, want 3 (config + 2 data)", len(req.Files))
	}
	if req.Files[0].Kind != pv.FileKindConfig {
		t.Fatalf("first file kind=%q, want config", req.Files[0].Kind)
	}
	for _, f := range req.Files[1:] {
		if f.Kind != pv.FileKindData {
			t.Fatalf("file %q kind=%q, want %q", f.Path, f.Kind, pv.FileKindData)
		}
	}
}

// A file matching both lists is data. Validating a reference target standalone
// reports on the wrong thing — and for a SecLang ruleset it would be parsed as
// TOML and produce a spurious error.
func TestManager_ValidateAll_DataFilesWinOverConfigGlobs(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	if err := srv.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultValid,
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{{
		Name:       "coraza",
		SocketPath: srv.SocketPath,
		Files:      []string{"/etc/haproxy/general/*"},
		DataFiles:  []string{"/etc/haproxy/general/rules.conf"},
		Timeout:    time.Second,
	}})

	files := []pv.File{
		{Path: "/etc/haproxy/general/config.toml", Content: "[hub]"},
		{Path: "/etc/haproxy/general/rules.conf", Content: "SecAction id:1"},
	}

	mgr.ValidateAll(context.Background(), files)

	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1 — rules.conf must not be dispatched as a config", len(reqs))
	}
}

func TestManager_ValidateAll_ChangedDataFileIsRevalidated(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	if err := srv.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultValid,
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{{
		Name:       "coraza",
		SocketPath: srv.SocketPath,
		Files:      tomlGlob(),
		DataFiles:  []string{"/etc/haproxy/general/crs/*"},
		Timeout:    time.Second,
	}})

	withRules := func(content string) []pv.File {
		return []pv.File{
			tomlFile(),
			{Path: "/etc/haproxy/general/crs/rules.conf", Content: content},
		}
	}

	mgr.ValidateAll(context.Background(), withRules("SecAction id:1"))
	if got := len(srv.Requests()); got != 1 {
		t.Fatalf("first call: server saw %d requests, want 1", got)
	}

	// Same config file, same everything except the ruleset.
	mgr.ValidateAll(context.Background(), withRules("SecAction id:2"))
	if got := len(srv.Requests()); got != 2 {
		t.Fatalf("server saw %d requests, want 2", got)
	}

	// Protocol v1 has no validator runtime generation, so even an identical
	// repeat is checked again.
	mgr.ValidateAll(context.Background(), withRules("SecAction id:2"))
	if got := len(srv.Requests()); got != 3 {
		t.Fatalf("server saw %d requests, want 3", got)
	}
}

// The validator resolves a config's references against the data files by their
// runtime path, which is not the path the controller identifies them by. It has
// to be told the root, so the request must carry it.
func TestManager_ValidateAll_SendsStagedRoot(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	if err := srv.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultValid,
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{{
		Name:       "coraza",
		SocketPath: srv.SocketPath,
		Files:      tomlGlob(),
		DataFiles:  []string{"/etc/haproxy/general/crs/*"},
		Timeout:    time.Second,
	}}, pv.WithStagedRoot("/etc/haproxy"))

	mgr.ValidateAll(context.Background(), []pv.File{
		tomlFile(),
		{Path: "/etc/haproxy/general/crs/rules.conf", Content: "SecAction id:1"},
	})

	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	var req pv.Request
	if err := json.Unmarshal(reqs[0], &req); err != nil {
		t.Fatalf("decoding request: %v", err)
	}
	if req.StagedRoot != "/etc/haproxy" {
		t.Fatalf("staged_root=%q, want /etc/haproxy — without it the validator cannot resolve the config's file references", req.StagedRoot)
	}
}

// Omitted when unset, so a deployment with no data files sends exactly what it
// sent before the field existed.
func TestManager_ValidateAll_OmitsStagedRootWhenUnset(t *testing.T) {
	srv := testutil.NewFixtureServer(t)
	if err := srv.SetResponse(&pv.Response{
		ProtocolVersion: pv.ProtocolVersion,
		Result:          pv.ResultValid,
	}); err != nil {
		t.Fatalf("SetResponse: %v", err)
	}

	mgr, _ := pv.NewManager(nil, []pv.ManagerConfig{
		{Name: "coraza", SocketPath: srv.SocketPath, Files: tomlGlob(), Timeout: time.Second},
	})

	mgr.ValidateAll(context.Background(), []pv.File{tomlFile()})

	if got := string(srv.Requests()[0]); strings.Contains(got, "staged_root") {
		t.Fatalf("request carries staged_root when none was configured: %s", got)
	}
}
