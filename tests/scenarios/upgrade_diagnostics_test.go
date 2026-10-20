// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/tests/kindutil"
	"github.com/haproxy-haptic/HAPTIC/tests/kubeexec"
	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

func TestUpgradeDiagnosticOutputIsBoundedAndRetainsTimestampedEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.jsonl.gz")
	output, err := newDiagnosticOutput(path, 256)
	require.NoError(t, err)
	payload := []byte("old pod drain finished: bound\n")
	n, err := output.Write(payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	_, err = output.Write([]byte(strings.Repeat("x", 256)))
	require.ErrorContains(t, err, "byte limit")
	require.ErrorContains(t, output.close(), "byte limit")

	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	require.NoError(t, err)
	defer compressed.Close()
	var record struct{ Observed, Output string }
	decoder := json.NewDecoder(compressed)
	require.NoError(t, decoder.Decode(&record))
	require.Equal(t, string(payload), record.Output)
	_, err = time.Parse(time.RFC3339Nano, record.Observed)
	require.NoError(t, err)
	require.ErrorIs(t, decoder.Decode(&record), io.EOF)
}

type diagnosticRunner struct {
	run   func(context.Context, *process.Command) (process.Result, error)
	start func(context.Context, *process.Command) (process.Running, error)
}

func (r diagnosticRunner) Run(ctx context.Context, command *process.Command) (process.Result, error) {
	return r.run(ctx, command)
}

func (r diagnosticRunner) Start(ctx context.Context, command *process.Command) (process.Running, error) {
	return r.start(ctx, command)
}

type diagnosticChild struct {
	done    chan struct{}
	stopped bool
	err     error
}

func (c *diagnosticChild) Done() <-chan struct{}  { return c.done }
func (c *diagnosticChild) Output() process.Result { return process.Result{} }
func (c *diagnosticChild) Wait() (process.Result, error) {
	return process.Result{}, c.err
}
func (c *diagnosticChild) Stop() error {
	c.stopped = true
	return nil
}

func TestUpgradeDiagnosticCleanupPreservesStreamFailureAndRemovesOnlyCreatedContainer(t *testing.T) {
	directory := t.TempDir()
	child := &diagnosticChild{done: make(chan struct{}), err: errors.New("watch disconnected")}
	close(child.done)
	removed := false
	runner := diagnosticRunner{
		start: func(_ context.Context, command *process.Command) (process.Running, error) {
			_, err := command.Stdout.Write([]byte("endpoint evidence"))
			require.NoError(t, err)
			return child, nil
		},
		run: func(ctx context.Context, command *process.Command) (process.Result, error) {
			require.NoError(t, ctx.Err())
			require.Equal(t, "docker", command.Name)
			require.Equal(t, []string{"rm", "--force", "created-container-id"}, command.Args)
			removed = true
			return process.Result{}, nil
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	session := &Session{Runner: runner, Cluster: &kindutil.Cluster{}, Artifacts: directory}
	observer := &upgradeDiagnostics{session: session, directory: directory, cancel: cancel, containers: []string{"created-container-id"}}
	require.NoError(t, observer.startStream(ctx, "watch", &process.Command{Name: "kubectl"}))
	original := errors.New("HTTPS connection refused")
	err := errors.Join(original, observer.finish())
	require.ErrorIs(t, err, original)
	require.ErrorContains(t, err, "watch disconnected")
	require.True(t, child.stopped)
	require.True(t, removed)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	report, err := os.ReadFile(filepath.Join(directory, "result.txt"))
	require.NoError(t, err)
	require.Contains(t, string(report), "watch disconnected")
}

func TestUpgradeNodeSampleRecordsFailureWithExplicitOwnershipAndBounds(t *testing.T) {
	directory := t.TempDir()
	output, err := newDiagnosticOutput(filepath.Join(directory, "node.jsonl.gz"), 4096)
	require.NoError(t, err)
	runner := scenarioRunner{run: func(ctx context.Context, command *process.Command) (process.Result, error) {
		require.Equal(t, "docker", command.Name)
		require.Equal(t, []string{"exec", "owned-node-id"}, command.Args[:2])
		require.Contains(t, command.Args[6], "head -c 262145")
		require.Equal(t, "private-namespace", command.Args[len(command.Args)-1])
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), 3*time.Second)
		return process.Result{ExitCode: 124, Stdout: "retired pod log", Stderr: "deadline"}, errors.New("node timeout")
	}}
	observer := &upgradeDiagnostics{
		session:    &Session{Runner: runner, Cluster: &kindutil.Cluster{}, Namespace: "private-namespace"},
		nodeOutput: output,
	}
	require.NoError(t, observer.sampleNode(t.Context(), "owned-node-id"))
	require.ErrorContains(t, observer.nodeError, "node timeout")
	require.NoError(t, output.close())
}

func TestUpgradeNodeSampleCancellationDoesNotBecomeCaptureFailure(t *testing.T) {
	output, err := newDiagnosticOutput(filepath.Join(t.TempDir(), "node.jsonl.gz"), 4096)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := scenarioRunner{run: func(sampleCtx context.Context, _ *process.Command) (process.Result, error) {
		cancel()
		<-sampleCtx.Done()
		return process.Result{ExitCode: -1}, sampleCtx.Err()
	}}
	observer := &upgradeDiagnostics{
		session:    &Session{Runner: runner, Cluster: &kindutil.Cluster{}},
		nodeOutput: output,
	}
	require.NoError(t, observer.sampleNode(ctx, "owned-node-id"))
	require.NoError(t, observer.nodeError)
	require.NoError(t, output.close())
}

func TestUpgradeConntrackFiltersTheObservedServiceAddress(t *testing.T) {
	for _, address := range []string{"10.96.0.42", "", "None"} {
		t.Run(address, func(t *testing.T) {
			directory := t.TempDir()
			child := &diagnosticChild{done: make(chan struct{})}
			created := false
			runner := diagnosticRunner{
				run: func(_ context.Context, command *process.Command) (process.Result, error) {
					args := strings.Join(command.Args, " ")
					switch {
					case strings.Contains(args, "get deployments"):
						return process.Result{Stdout: `{"items":[{"metadata":{"name":"private-haproxy"}}]}`}, nil
					case strings.Contains(args, "get service private-haproxy"):
						return process.Result{Stdout: `{"spec":{"clusterIP":"` + address + `"}}`}, nil
					case command.Args[0] == "inspect":
						require.Equal(t, "owned-node-id", command.Args[len(command.Args)-1])
						return process.Result{Stdout: "sha256:owned-node-image\n"}, nil
					case command.Args[0] == "create":
						require.Contains(t, args, "--network container:owned-node-id")
						require.Contains(t, args, "sha256:owned-node-image -E -p tcp --orig-dst 10.96.0.42")
						created = true
						return process.Result{Stdout: "created-container-id\n"}, nil
					case command.Args[0] == "rm":
						require.Equal(t, []string{"rm", "--force", "created-container-id"}, command.Args)
						return process.Result{}, nil
					default:
						t.Fatalf("unexpected command: %+v", command)
						return process.Result{}, nil
					}
				},
				start: func(_ context.Context, command *process.Command) (process.Running, error) {
					require.Equal(t, []string{"start", "--attach", "created-container-id"}, command.Args)
					return child, nil
				},
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			session := &Session{
				Runner: runner, Cluster: &kindutil.Cluster{}, Artifacts: directory,
				Client: kubeexec.Client{Runner: runner, Kubeconfig: filepath.Join(directory, "kubeconfig"), Context: "kind-owned"},
			}
			observer := &upgradeDiagnostics{session: session, directory: directory, cancel: cancel}
			err := observer.startConntrack(ctx, "owned-node-id")
			if address == "10.96.0.42" {
				require.NoError(t, err)
				require.True(t, created)
			} else {
				require.ErrorContains(t, err, "has no ClusterIP")
				require.False(t, created)
			}
			require.NoError(t, observer.finish())
		})
	}
}
