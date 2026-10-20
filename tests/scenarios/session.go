// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

// Package scenarios implements the cluster scenarios invoked by native Make targets.
package scenarios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/kindutil"
	"github.com/haproxy-haptic/HAPTIC/tests/kubeexec"
	"github.com/haproxy-haptic/HAPTIC/tests/process"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

const (
	kubectlGet    = "get"
	podsResource  = "pods"
	dockerCommand = "docker"
)

type Session struct {
	Runner          process.Runner
	Cluster         *kindutil.Cluster
	Client          kubeexec.Client
	Root            string
	Namespace       string
	Release         string
	Artifacts       string
	ImageRepository string
	ImageTag        string
	HAProxyVersion  string
	SPOAImage       string
	Log             io.Writer
	CommandTimeout  time.Duration
	commandNumber   atomic.Uint64
}

func (s *Session) Infof(format string, args ...any) {
	if s.Log != nil {
		_, _ = fmt.Fprintf(s.Log, format+"\n", args...)
	}
}

func (s *Session) Run(ctx context.Context, name string, args ...string) (process.Result, error) {
	ctx, cancel := s.commandContext(ctx)
	defer cancel()
	result, err := s.Runner.Run(ctx, &process.Command{Name: name, Args: args, Dir: s.Root, Env: s.Cluster.Environment})
	return s.record(name, args, result, err)
}

func (s *Session) Kube(ctx context.Context, input io.Reader, args ...string) (process.Result, error) {
	ctx, cancel := s.commandContext(ctx)
	defer cancel()
	result, err := s.Client.Run(ctx, input, args...)
	return s.record("kubectl", args, result, err)
}

// KubeUnscoped lets each manifest retain its declared namespace.
func (s *Session) KubeUnscoped(ctx context.Context, input io.Reader, args ...string) (process.Result, error) {
	ctx, cancel := s.commandContext(ctx)
	defer cancel()
	client := s.Client
	client.Namespace = ""
	result, err := client.Run(ctx, input, args...)
	return s.record("kubectl", args, result, err)
}

func (s *Session) Helm(ctx context.Context, args ...string) (process.Result, error) {
	flags := make([]string, 0, 6+len(args))
	flags = append(flags, "--kubeconfig", s.Client.Kubeconfig, "--kube-context", s.Client.Context, "--namespace", s.Namespace)
	return s.Run(ctx, "helm", append(flags, args...)...)
}

func (s *Session) record(name string, args []string, result process.Result, err error) (process.Result, error) {
	sequence := s.commandNumber.Add(1)
	text := fmt.Sprintf("%s %q\nexit=%d\nstdout:\n%s\nstderr:\n%s\n", name, args, result.ExitCode, result.Stdout, result.Stderr)
	if writeErr := os.WriteFile(filepath.Join(s.Artifacts, fmt.Sprintf("%04d-%s.log", sequence, filepath.Base(name))), []byte(text), 0o600); writeErr != nil {
		return result, errors.Join(err, fmt.Errorf("write command evidence: %w", writeErr))
	}
	if err != nil {
		return result, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(result.Stderr))
	}
	return result, nil
}

func readJSON[T any](ctx context.Context, s *Session, args ...string) (T, error) {
	var value T
	result, err := s.Kube(ctx, nil, append(args, "-o", "json")...)
	if err != nil {
		return value, err
	}
	if err = json.Unmarshal([]byte(result.Stdout), &value); err != nil {
		return value, &observationDecodeError{err}
	}
	return value, nil
}

type observationDecodeError struct{ cause error }

func (e *observationDecodeError) Error() string { return "decode observed JSON: " + e.cause.Error() }
func (e *observationDecodeError) Unwrap() error { return e.cause }

func poll(ctx context.Context, timeout, interval time.Duration, description string, observe func(context.Context) (testutil.PollResult, error)) error {
	return testutil.Poll(ctx, testutil.WaitConfig{Timeout: timeout, InitialInterval: interval, MaxInterval: interval, Multiplier: 1}, description, observe)
}

func (s *Session) LoadControllerImage(ctx context.Context) error {
	base := s.ImageRepository + ":" + s.ImageTag
	return s.loadControllerImage(ctx, base)
}

func (s *Session) loadControllerImage(ctx context.Context, source string) error {
	if _, err := s.Run(ctx, "docker", "image", "inspect", source); err != nil {
		return err
	}
	versioned := s.ImageRepository + ":" + s.ImageTag + "-haproxy" + s.HAProxyVersion
	if source == versioned {
		return s.Cluster.LoadImages(ctx, source)
	}
	if _, err := s.Run(ctx, "docker", "tag", source, versioned); err != nil {
		return err
	}
	return s.Cluster.LoadImages(ctx, source, versioned)
}

// LoadSPOAImage loads the chart's SPOA image, which a CI build has only locally.
func (s *Session) LoadSPOAImage(ctx context.Context) error {
	if _, err := s.Run(ctx, "docker", "image", "inspect", s.SPOAImage); err != nil {
		if _, err := s.Run(ctx, "docker", "pull", s.SPOAImage); err != nil {
			return err
		}
	}
	return s.Cluster.LoadImages(ctx, s.SPOAImage)
}

func (s *Session) ImageValues() []string {
	const setFlag = "--set"
	return []string{setFlag, "controller.image.repository=" + s.ImageRepository, setFlag, "controller.image.tag=" + s.ImageTag, setFlag, "haproxyVersion=" + s.HAProxyVersion}
}

func (s *Session) commandContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.CommandTimeout > 0 {
		return context.WithTimeout(ctx, s.CommandTimeout)
	}
	return context.WithCancel(ctx)
}
