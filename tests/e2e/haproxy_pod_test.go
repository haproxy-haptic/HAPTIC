// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/availability"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

func execInHAProxyPod(ctx context.Context, pod, container string, argv ...string) (string, error) {
	args := make([]string, 0, 9+len(argv))
	args = append(args,
		kubeconfigFlag, kubeconfigPath,
		"-n", ControllerNamespace,
		"exec", pod, "-c", container, "--",
	)
	args = append(args, argv...)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("kubectl exec %s -c %s %v: %w (stderr: %s)",
			pod, container, argv, err, stderr.String())
	}
	return stdout.String(), nil
}

// waitForHAProxyPodsToReach waits until a GET of url succeeds within budget
// from every HAProxy pod. kube-proxy can program a new Service seconds after
// its endpoints turn ready; what a pod sends there before that is lost.
func waitForHAProxyPodsToReach(ctx context.Context, t *testing.T, client klient.Client, url string, budget time.Duration) {
	t.Helper()
	var pods corev1.PodList
	if err := client.Resources(ControllerNamespace).List(ctx, &pods, resources.WithLabelSelector(LabelSelectorHAProxy)); err != nil {
		t.Fatalf("list HAProxy pods: %v", err)
	}
	pending := map[string]bool{}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			pending[pods.Items[i].Name] = true
		}
	}
	if len(pending) == 0 {
		t.Fatalf("no HAProxy pod matches %q", LabelSelectorHAProxy)
	}
	maxTime := strconv.FormatFloat(budget.Seconds(), 'f', -1, 64)
	start := time.Now()
	err := testutil.WaitForConditionWithDescription(ctx, testutil.FastWaitConfig(), "every HAProxy pod to reach "+url,
		func(c context.Context) (bool, error) {
			var lastErr error
			for pod := range pending {
				if _, err := execInHAProxyPod(c, pod, "haproxy", "curl", "-fsS", "-o", "/dev/null", "--max-time", maxTime, url); err != nil {
					lastErr = err
					continue
				}
				delete(pending, pod)
			}
			return len(pending) == 0, lastErr
		})
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Logf("every HAProxy pod reached %s after %v", url, time.Since(start).Round(time.Millisecond))
}

func podJSONPath(ctx context.Context, pod, expr string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl",
		kubeconfigFlag, kubeconfigPath,
		"-n", ControllerNamespace,
		"get", "pod", pod, "-o", "jsonpath="+expr,
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func apiProxyGet(ctx context.Context, pod string, port int, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl",
		kubeconfigFlag, kubeconfigPath,
		"get", "--raw",
		fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/%s", ControllerNamespace, pod, port, path),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func selectedHAProxyStatus(ctx context.Context, pod, host, path string) (string, error) {
	out, err := execInHAProxyPod(ctx, pod, "haproxy", "curl",
		"-sS", "--connect-timeout", "1", "--max-time", "2",
		"-o", "/dev/null", "-w", "%{http_code}",
		"-H", "Host: "+host, "http://127.0.0.1"+path)
	return strings.TrimSpace(out), err
}

func availabilityMonitorToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate availability monitor token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

func stopRemoteHAProxyAvailabilityMonitor(pod, stateDir, token string, waitForState bool) error {
	waitForStateArg := "0"
	if waitForState {
		waitForStateArg = "1"
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := execInHAProxyPod(cleanupCtx, pod, "haproxy", "/bin/sh", "-c", availability.StopScript,
		"haproxy-availability-monitor-cleanup", stateDir, token, waitForStateArg)
	if err != nil {
		return fmt.Errorf("stop remote selected-pod availability monitor: %w", err)
	}
	return nil
}

func startSelectedHAProxyAvailabilityMonitor(
	ctx context.Context,
	pod string,
	host string,
	path string,
) (func() error, error) {
	token, err := availabilityMonitorToken()
	if err != nil {
		return nil, err
	}
	stateDir := "/tmp/haptic-availability-monitor-" + token
	monitorCtx, cancel := context.WithCancel(context.Background())
	args := []string{
		kubeconfigFlag, kubeconfigPath,
		"-n", ControllerNamespace,
		"exec", pod, "-c", "haproxy", "--", "/bin/sh", "-c", availability.MonitorScript,
		"haproxy-availability-monitor", host, path, stateDir, token,
	}
	cmd := exec.CommandContext(monitorCtx, "kubectl", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create selected-pod availability output pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start selected-pod availability monitor: %w", err)
	}
	return runAvailabilityMonitorLifecycle(ctx, cmd, cancel, stdout, &stderr, func(waitForState bool) error {
		return stopRemoteHAProxyAvailabilityMonitor(pod, stateDir, token, waitForState)
	})
}

func runAvailabilityMonitorLifecycle(
	ctx context.Context,
	cmd *exec.Cmd,
	cancel context.CancelFunc,
	stdout io.Reader,
	stderr *bytes.Buffer,
	stopRemote func(bool) error,
) (func() error, error) {
	done := make(chan struct{})
	var lifecycleMu sync.Mutex
	commandExited := false
	terminationRequested := false
	unexpectedExit := false
	var commandErr error
	go func() {
		err := cmd.Wait()
		lifecycleMu.Lock()
		commandErr = err
		commandExited = true
		unexpectedExit = !terminationRequested
		lifecycleMu.Unlock()
		close(done)
	}()

	var terminateOnce sync.Once
	terminated := make(chan struct{})
	var terminateErr error
	handshakeReceived := false
	terminate := func() error {
		terminateOnce.Do(func() {
			lifecycleMu.Lock()
			terminationRequested = true
			waitForState := !handshakeReceived && !commandExited
			lifecycleMu.Unlock()

			terminateErr = stopRemote(waitForState)
			cancel()
			<-done
			if terminateErr != nil {
				retryErr := stopRemote(false)
				if retryErr == nil {
					terminateErr = nil
				} else {
					terminateErr = errors.Join(terminateErr, retryErr)
				}
			}
			close(terminated)
		})
		<-terminated
		return terminateErr
	}

	go func() {
		select {
		case <-ctx.Done():
			_ = terminate()
		case <-done:
			_ = terminate()
		case <-terminated:
		}
	}()

	ready := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() {
			ready <- fmt.Errorf("monitor produced no startup handshake: %v", scanner.Err())
			return
		}
		if scanner.Text() != "READY" {
			ready <- fmt.Errorf("unexpected monitor startup handshake %q", scanner.Text())
			return
		}
		ready <- nil
		for scanner.Scan() {
		}
	}()

	recordHandshake := func() {
		lifecycleMu.Lock()
		handshakeReceived = true
		lifecycleMu.Unlock()
	}
	exitedOutcome := func() availabilityMonitorOutcome {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		return availabilityMonitorOutcome{
			err:            commandErr,
			unexpectedExit: unexpectedExit,
			stderr:         strings.TrimSpace(stderr.String()),
		}
	}
	if err := awaitAvailabilityMonitor(ctx, ready, done, terminate, recordHandshake, exitedOutcome); err != nil {
		return nil, err
	}
	return availabilityMonitorStop(terminate, exitedOutcome), nil
}

type availabilityMonitorOutcome struct {
	err            error
	unexpectedExit bool
	stderr         string
}

func awaitAvailabilityMonitor(
	ctx context.Context,
	ready <-chan error,
	done <-chan struct{},
	terminate func() error,
	recordHandshake func(),
	exitedOutcome func() availabilityMonitorOutcome,
) error {
	select {
	case err := <-ready:
		if err == nil {
			recordHandshake()
			if ctxErr := ctx.Err(); ctxErr != nil {
				cleanupErr := terminate()
				return errors.Join(ctxErr, cleanupErr)
			}
			return nil
		}
		cleanupErr := terminate()
		outcome := exitedOutcome()
		if strings.Contains(outcome.stderr, availability.FailureMarker) {
			return errors.Join(err, fmt.Errorf("selected-pod HAProxy request failed before startup: %v (stderr: %s)",
				outcome.err, outcome.stderr), cleanupErr)
		}
		return errors.Join(err, cleanupErr)
	case <-done:
		cleanupErr := terminate()
		outcome := exitedOutcome()
		if strings.Contains(outcome.stderr, availability.FailureMarker) {
			return errors.Join(fmt.Errorf("selected-pod HAProxy request failed before startup: %v (stderr: %s)",
				outcome.err, outcome.stderr), cleanupErr)
		}
		return fmt.Errorf("selected-pod availability monitor exited before startup: %v (stderr: %s)",
			errors.Join(outcome.err, cleanupErr), outcome.stderr)
	case <-ctx.Done():
		cleanupErr := terminate()
		return errors.Join(ctx.Err(), cleanupErr)
	}
}

func availabilityMonitorStop(terminate func() error, exitedOutcome func() availabilityMonitorOutcome) func() error {
	var stopOnce sync.Once
	var stopErr error
	return func() error {
		stopOnce.Do(func() {
			cleanupErr := terminate()
			outcome := exitedOutcome()
			if strings.Contains(outcome.stderr, availability.FailureMarker) {
				stopErr = errors.Join(fmt.Errorf("selected-pod HAProxy request failed: %w (stderr: %s)",
					outcome.err, outcome.stderr), cleanupErr)
				return
			}
			if outcome.unexpectedExit {
				stopErr = errors.Join(fmt.Errorf("selected-pod availability monitor exited early: %v (stderr: %s)",
					outcome.err, outcome.stderr), cleanupErr)
				return
			}
			stopErr = cleanupErr
		})
		return stopErr
	}
}
