// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const baseline = "51779e9755e7bc1766122c9c7302e2064aa63a0b"

type phaseResult struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Error  string `json:"error,omitempty"`
}

func main() {
	if err := compare(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func compare() error {
	candidate := os.Getenv("CI629_CANDIDATE_SHA")
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(candidate) || candidate == baseline {
		return fmt.Errorf("invalid candidate SHA %q", candidate)
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "ci629-comparison-")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 33*time.Minute)
	defer cancel()
	sampler := filepath.Join(directory, "sampler")
	if err := run(ctx, root, nil, "go", "build", "-o", sampler, "./.github/scripts/vector-stall-diagnostic"); err != nil {
		return err
	}
	var failures []error
	var results []phaseResult
	for _, phase := range []phaseResult{{Name: "before", Source: baseline}, {Name: "after", Source: candidate}} {
		err := exercise(ctx, root, directory, sampler, phase)
		if err != nil {
			phase.Error = err.Error()
			failures = append(failures, err)
		}
		results = append(results, phase)
	}
	body, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, ".pilot", "comparison.json"), body, 0600); err != nil {
		return err
	}
	return errors.Join(failures...)
}

func exercise(ctx context.Context, root, directory, sampler string, phase phaseResult) error {
	if err := requireEmptyKindHost(ctx); err != nil {
		return fmt.Errorf("%s: %w", phase.Name, err)
	}
	worktree := filepath.Join(directory, phase.Name)
	if err := run(ctx, root, nil, "git", "worktree", "add", "--detach", worktree, phase.Source); err != nil {
		return err
	}
	pilot := filepath.Join(worktree, ".pilot")
	if err := os.Mkdir(pilot, 0700); err != nil {
		return err
	}
	captureLog, err := os.Create(filepath.Join(pilot, "scheduling-capture.log"))
	if err != nil {
		return err
	}
	defer captureLog.Close()
	capture := exec.CommandContext(ctx, sampler)
	capture.Dir, capture.Stdout, capture.Stderr = worktree, captureLog, captureLog
	if err := capture.Start(); err != nil {
		return err
	}
	env := append(os.Environ(), "PILOT_SOURCE_SHA="+phase.Source, "TEST_RUN_PATTERN=^TestVectorSidecar$", "COUNT=1")
	testErr := run(ctx, worktree, env, "bash", ".github/scripts/run-arm-pilot.sh")
	outputEnv := append(env, "KUBECONFIG=/tmp/haproxy-e2e-kubeconfig", "HAPTIC_E2E_CLUSTER_NAME=haptic-e2e", "CONTROLLER_OUTPUT_DIR="+filepath.Join(pilot, "controller-output"))
	outputErr := run(ctx, worktree, outputEnv, "make", "check-controller-output")
	markerErr := os.WriteFile(filepath.Join(pilot, "test-finished"), nil, 0600)
	captureErr := capture.Wait()
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cleanupErr := run(cleanupCtx, root, nil, "kind", "delete", "cluster", "--name", "haptic-e2e", "--kubeconfig", "/tmp/haproxy-e2e-kubeconfig")
	cleanupErr = errors.Join(cleanupErr, requireEmptyKindHost(cleanupCtx))
	copyErr := os.CopyFS(filepath.Join(root, ".pilot", "comparison", phase.Name), os.DirFS(pilot))
	debug := filepath.Join(worktree, "debug-logs")
	var debugErr error
	if _, err := os.Stat(debug); err == nil {
		debugErr = os.CopyFS(filepath.Join(root, ".pilot", "comparison", phase.Name, "debug-logs"), os.DirFS(debug))
	}
	return errors.Join(testErr, outputErr, markerErr, captureErr, cleanupErr, copyErr, debugErr)
}

func requireEmptyKindHost(ctx context.Context) error {
	clusters, err := exec.CommandContext(ctx, "kind", "get", "clusters").Output()
	if err != nil {
		return fmt.Errorf("list Kind clusters: %w", err)
	}
	if strings.TrimSpace(string(clusters)) != "" {
		return fmt.Errorf("expected an empty Kind host, found %q", clusters)
	}
	return nil
}

func run(ctx context.Context, directory string, env []string, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir, command.Env = directory, env
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	return command.Run()
}
