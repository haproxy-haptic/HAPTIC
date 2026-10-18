// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/haproxy-haptic/HAPTIC/tests/process"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

type controllerLog struct {
	pod    *corev1.Pod
	stream process.Running
}

type controllerObservation struct {
	logs []controllerLog
	err  error
}

func (u *upgrade) observeControllers(ctx context.Context) (func() error, error) {
	s := u.session
	pods, err := s.Pods(ctx, controllerComponent)
	if err != nil {
		return nil, err
	}
	if len(pods) != 2 {
		return nil, fmt.Errorf("handover needs two released controller Pods, got %d", len(pods))
	}
	events, err := s.validationFailureCounts(ctx)
	if err != nil {
		return nil, err
	}
	logs, err := s.followControllerLogs(ctx, pods)
	if err != nil {
		return nil, err
	}
	metricsCtx, cancel := context.WithCancel(ctx)
	metricsDone := make(chan controllerObservation, 1)
	go func() { metricsDone <- s.monitorControllerRejections(ctx, metricsCtx, logs) }()
	return func() error {
		cancel()
		return s.completeControllerObservation(ctx, pods, events, <-metricsDone)
	}, nil
}

func (s *Session) completeControllerObservation(ctx context.Context, pods []corev1.Pod, events map[types.UID]int32, observed controllerObservation) error {
	result := observed.err
	current, err := s.Pods(ctx, controllerComponent)
	if err != nil {
		return errors.Join(result, err, s.finishControllerLogs(observed.logs, nil))
	}
	observed.logs, err = s.captureCandidateLogs(ctx, observed.logs, current)
	result = errors.Join(result, err)
	live := make(map[types.UID]bool, len(current))
	for i := range current {
		live[current[i].UID] = true
	}
	result = errors.Join(result, s.finishControllerLogs(observed.logs, live))
	for i := range pods {
		if live[pods[i].UID] {
			result = errors.Join(result, fmt.Errorf("old controller Pod %s survived template application", pods[i].Name))
		}
	}
	for i := range current {
		if controllerContainerRestarted(&current[i]) {
			result = errors.Join(result, fmt.Errorf("controller Pod %s restarted during ordinary upgrade", current[i].Name))
		}
		result = errors.Join(result, s.requireNoRejectionMetrics(ctx, &current[i]))
	}
	after, err := s.validationFailureCounts(ctx)
	if err != nil {
		return errors.Join(result, err)
	}
	for uid, count := range after {
		if count > events[uid] {
			result = errors.Join(result, fmt.Errorf("ordinary upgrade emitted ValidationFailed event %s", uid))
		}
	}
	return result
}

func (s *Session) followControllerLogs(ctx context.Context, pods []corev1.Pod) ([]controllerLog, error) {
	logs := make([]controllerLog, 0, len(pods))
	for i := range pods {
		if err := s.requireNoRejectionMetrics(ctx, &pods[i]); err != nil {
			return nil, errors.Join(err, s.finishControllerLogs(logs, nil))
		}
		stream, err := s.Client.Start(ctx, nil, "logs", pods[i].Name, "-c", controllerComponent, "--follow", "--timestamps")
		if err != nil {
			return nil, errors.Join(err, s.finishControllerLogs(logs, nil))
		}
		logs = append(logs, controllerLog{pod: &pods[i], stream: stream})
		if err := poll(ctx, 15*time.Second, 50*time.Millisecond, "old controller log stream", func(context.Context) (testutil.PollResult, error) {
			select {
			case <-stream.Done():
				_, err := stream.Wait()
				return testutil.PollFailed, errors.Join(err, errors.New("controller log stream ended before upgrade"))
			default:
			}
			if stream.Output().Stdout != "" {
				return testutil.PollSucceeded, nil
			}
			return testutil.PollPending, nil
		}); err != nil {
			return nil, errors.Join(err, s.finishControllerLogs(logs, nil))
		}
	}
	return logs, nil
}

func (s *Session) finishControllerLogs(logs []controllerLog, live map[types.UID]bool) error {
	var result error
	for _, log := range logs {
		if live != nil && !live[log.pod.UID] {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			select {
			case <-log.stream.Done():
			case <-ctx.Done():
				result = errors.Join(result, fmt.Errorf("controller log stream %s did not finish after handover", log.pod.Name))
			}
			cancel()
		}
		select {
		case <-log.stream.Done():
			_, err := log.stream.Wait()
			result = errors.Join(result, err)
		default:
			result = errors.Join(result, log.stream.Stop())
		}
		output := log.stream.Output()
		result = errors.Join(result, os.WriteFile(filepath.Join(s.Artifacts, "handover-"+log.pod.Name+".log"), []byte(output.Combined), 0o600))
		result = errors.Join(result, requireNoValidationRejections(output.Stdout))
	}
	return result
}

func requireNoValidationRejections(logs string) error {
	for _, message := range []string{"Template validation failed", "ValidationTests could not run", "Config validation failed", "Configuration validation failed"} {
		if strings.Contains(logs, message) {
			return fmt.Errorf("controller rejected configuration during ordinary upgrade: %s", message)
		}
	}
	return nil
}

func (s *Session) validationFailureCounts(ctx context.Context) (map[types.UID]int32, error) {
	events, err := readJSON[corev1.EventList](ctx, s, "get", "events", "--field-selector=reason=ValidationFailed")
	if err != nil {
		return nil, err
	}
	counts := make(map[types.UID]int32, len(events.Items))
	for i := range events.Items {
		event := &events.Items[i]
		counts[event.UID] = max(event.Count, 1)
	}
	return counts, nil
}

func (s *Session) monitorControllerRejections(streamCtx, ctx context.Context, logs []controllerLog) controllerObservation {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return controllerObservation{logs: logs}
		case <-ticker.C:
			var err error
			logs, err = s.observeControllerPods(streamCtx, ctx, logs)
			if err != nil {
				if ctx.Err() != nil {
					return controllerObservation{logs: logs}
				}
				return controllerObservation{logs: logs, err: err}
			}
		}
	}
}

func (s *Session) observeControllerPods(streamCtx, ctx context.Context, logs []controllerLog) ([]controllerLog, error) {
	current, err := s.Pods(ctx, controllerComponent)
	if err != nil {
		return logs, err
	}
	logs, err = s.captureCandidateLogs(streamCtx, logs, current)
	if err != nil {
		return logs, err
	}
	for i := range current {
		pod := &current[i]
		if controllerContainerRestarted(pod) {
			return logs, fmt.Errorf("controller Pod %s restarted during ordinary upgrade", pod.Name)
		}
		if pod.DeletionTimestamp != nil || !controllerContainerReady(pod) {
			continue
		}
		if err := s.requireLiveControllerMetrics(ctx, pod); err != nil {
			return logs, err
		}
	}
	return logs, nil
}

func controllerContainerRestarted(pod *corev1.Pod) bool {
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		if status.Name == controllerComponent && status.RestartCount > 0 {
			return true
		}
	}
	return false
}

func controllerContainerReady(pod *corev1.Pod) bool {
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		if status.Name == controllerComponent {
			return status.Ready
		}
	}
	return false
}

func (s *Session) requireLiveControllerMetrics(ctx context.Context, pod *corev1.Pod) error {
	err := s.requireNoRejectionMetrics(ctx, pod)
	if err == nil {
		return nil
	}
	var invalid *controllerMetricsError
	if errors.As(err, &invalid) {
		return err
	}
	latest, listErr := s.Pods(ctx, controllerComponent)
	if listErr != nil {
		return errors.Join(err, listErr)
	}
	for i := range latest {
		live := &latest[i]
		if live.UID == pod.UID && live.DeletionTimestamp == nil {
			return err
		}
	}
	return nil
}

func (s *Session) captureCandidateLogs(ctx context.Context, logs []controllerLog, pods []corev1.Pod) ([]controllerLog, error) {
	known := make(map[types.UID]bool, len(logs))
	for _, log := range logs {
		known[log.pod.UID] = true
	}
	for i := range pods {
		pod := &pods[i]
		if known[pod.UID] || !controllerContainerStarted(pod) {
			continue
		}
		stream, err := s.Client.Start(ctx, nil, "logs", pod.Name, "-c", controllerComponent, "--follow", "--timestamps")
		if err != nil {
			return logs, err
		}
		logs = append(logs, controllerLog{pod: pod, stream: stream})
		if err := poll(ctx, 15*time.Second, 50*time.Millisecond, "candidate controller logs", func(context.Context) (testutil.PollResult, error) {
			output := stream.Output()
			if output.Stdout != "" {
				return testutil.PollSucceeded, nil
			}
			select {
			case <-stream.Done():
				_, err := stream.Wait()
				return testutil.PollFailed, errors.Join(err, errors.New("candidate controller log is empty"))
			default:
				return testutil.PollPending, nil
			}
		}); err != nil {
			return logs, err
		}
	}
	return logs, nil
}

func controllerContainerStarted(pod *corev1.Pod) bool {
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		if status.Name == controllerComponent {
			return status.State.Running != nil || status.State.Terminated != nil
		}
	}
	return false
}

func (s *Session) requireNoRejectionMetrics(ctx context.Context, pod *corev1.Pod) error {
	port := int32(0)
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		if container.Name != controllerComponent {
			continue
		}
		for _, candidate := range container.Ports {
			if candidate.Name == "metrics" {
				port = candidate.ContainerPort
			}
		}
	}
	if port == 0 {
		return fmt.Errorf("controller Pod %s has no metrics port", pod.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	path := "/api/v1/namespaces/" + s.Namespace + "/pods/" + pod.Name + ":" + strconv.Itoa(int(port)) + "/proxy/metrics"
	metrics, err := s.Kube(ctx, nil, "get", "--raw", path)
	if err != nil {
		return err
	}
	if err := requireZeroRejections(metrics.Stdout); err != nil {
		return &controllerMetricsError{cause: err}
	}
	return nil
}

type controllerMetricsError struct{ cause error }

func (e *controllerMetricsError) Error() string { return e.cause.Error() }
func (e *controllerMetricsError) Unwrap() error { return e.cause }

func requireZeroRejections(metrics string) error {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(metrics))
	if err != nil {
		return err
	}
	if len(families) == 0 {
		return errors.New("controller metrics response is empty")
	}
	for _, metric := range families["haptic_config_rejected_total"].GetMetric() {
		if metric.Counter == nil {
			return errors.New("controller rejection metric is not a counter")
		}
		if metric.GetCounter().GetValue() != 0 {
			return fmt.Errorf("controller rejected configuration: %s", metric.String())
		}
	}
	return nil
}
