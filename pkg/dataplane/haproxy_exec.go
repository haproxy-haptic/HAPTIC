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

package dataplane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
)

// HAProxyExecutor abstracts execution of the local HAProxy binary so that
// unit tests never shell out to an external process. Production code uses
// the default binaryHAProxyExecutor; tests install a fake via
// SetHAProxyExecutor (most conveniently through
// pkg/dataplane/dataplanetest.InstallFakeHAProxy in a TestMain).
type HAProxyExecutor interface {
	// Version returns the combined output of `haproxy -v`.
	Version(ctx context.Context) (string, error)
	// Check runs `haproxy <args...>` with the working directory set to
	// workDir and returns the combined output. The working directory
	// matters: semantic validation invokes haproxy with a relative config
	// path so that relative file references inside the config resolve.
	Check(ctx context.Context, workDir string, args ...string) ([]byte, error)
}

// binaryHAProxyExecutor is the production implementation: it locates the
// haproxy binary on PATH and executes it.
type binaryHAProxyExecutor struct{}

func (binaryHAProxyExecutor) Version(ctx context.Context) (string, error) {
	haproxyBin, err := exec.LookPath("haproxy")
	if err != nil {
		return "", fmt.Errorf("haproxy binary not found: %w", err)
	}

	output, err := exec.CommandContext(ctx, haproxyBin, "-v").CombinedOutput()
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return "", cause
		}
		return "", fmt.Errorf("running haproxy -v: %w", err)
	}

	return string(output), nil
}

func (binaryHAProxyExecutor) Check(ctx context.Context, workDir string, args ...string) ([]byte, error) {
	haproxyBin, err := exec.LookPath("haproxy")
	if err != nil {
		return nil, fmt.Errorf("haproxy binary not found: %w", err)
	}

	cmd := exec.CommandContext(ctx, haproxyBin, args...)
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()
	if cause := context.Cause(ctx); cause != nil {
		return output, cause
	}
	return output, err
}

// Identity names the binary Check runs, so a verdict is never reused across
// a replaced binary.
func (binaryHAProxyExecutor) Identity() (string, error) {
	haproxyBin, err := exec.LookPath("haproxy")
	if err != nil {
		return "", fmt.Errorf("haproxy binary not found: %w", err)
	}
	info, err := os.Stat(haproxyBin)
	if err != nil {
		return "", fmt.Errorf("inspecting haproxy binary: %w", err)
	}
	return fmt.Sprintf("%s %d %d", haproxyBin, info.Size(), info.ModTime().UnixNano()), nil
}

// identifiedExecutor is an executor that can name the binary it runs.
type identifiedExecutor interface {
	Identity() (string, error)
}

// executorGeneration changes with every installed executor, so a verdict one
// executor gave is never an answer for another.
var executorGeneration atomic.Uint64

// HAProxyExecutorIdentity names the active executor and, when available, its binary.
func HAProxyExecutorIdentity() (string, error) {
	generation := strconv.FormatUint(executorGeneration.Load(), 10)
	identified, ok := getHAProxyExecutor().(identifiedExecutor)
	if !ok {
		return generation, nil
	}
	identity, err := identified.Identity()
	if err != nil {
		return "", err
	}
	return generation + " " + identity, nil
}

// haproxyExecutor holds the active executor. An atomic pointer (rather than
// a plain package variable) keeps concurrent reads in parallel validations
// race-free against a test installing a fake.
var haproxyExecutor atomic.Pointer[HAProxyExecutor]

func init() {
	var e HAProxyExecutor = binaryHAProxyExecutor{}
	haproxyExecutor.Store(&e)
}

func getHAProxyExecutor() HAProxyExecutor {
	return *haproxyExecutor.Load()
}

// SetHAProxyExecutor replaces the executor used by DetectLocalVersion and
// semantic validation, returning a function that restores the previous one.
// It exists so unit tests can substitute a fake instead of shelling out;
// production code must not call it. Prefer installing the fake once per test
// package (TestMain) — per-test installs interleave badly with t.Parallel.
func SetHAProxyExecutor(e HAProxyExecutor) (restore func()) {
	prev := haproxyExecutor.Swap(&e)
	executorGeneration.Add(1)
	return func() {
		haproxyExecutor.Store(prev)
		executorGeneration.Add(1)
	}
}
