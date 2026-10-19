// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

const upgradeProbeScript = `
set -eu
while :; do
  for scheme in http https; do
    if [ "$scheme" = http ]; then
      code=$(curl -sS --connect-timeout 1 --max-time 2 -o /dev/null -w '%{http_code}' -H 'Host: http.upgrade.test' "http://$1/upgrade-check") || { printf 'FAILED http transport at %s\n' "$(date -u +%FT%T.%NZ)"; exit 1; }
    else
      code=$(curl -sS --connect-timeout 1 --max-time 2 -o /dev/null -w '%{http_code}' --cacert /tls/tls.crt --connect-to "tls.upgrade.test:443:$1:443" https://tls.upgrade.test/upgrade-check) || { printf 'FAILED https transport at %s\n' "$(date -u +%FT%T.%NZ)"; exit 1; }
    fi
    if [ "$code" != 200 ]; then
      printf 'FAILED %s %s at %s\n' "$scheme" "$code" "$(date -u +%FT%T.%NZ)"
      exit 1
    fi
  done
  printf 'OK HTTP HTTPS at %s\n' "$(date -u +%FT%T.%NZ)"
  if [ -e /tmp/haptic-upgrade-stop ]; then
    printf 'COMPLETE\n'
    exit 0
  fi
  sleep 0.1
done
`

func (u *upgrade) observeTraffic(ctx context.Context, phase string, run func(context.Context) error) (result error) {
	if u.baseline != handoverBaseline || phase == "released baseline" {
		return run(ctx)
	}
	s := u.session
	pod, err := u.trafficProbePod(ctx)
	if err != nil {
		return err
	}
	if err := s.Apply(ctx, pod); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if result != nil {
			_, _ = s.Kube(cleanup, nil, "logs", pod.Name, "--pod-running-timeout=5s")
		}
		_, err := s.Kube(cleanup, nil, "delete", "pod", pod.Name, "--wait=true", "--timeout=45s")
		result = errors.Join(result, err)
	}()
	if err := poll(ctx, time.Minute, time.Second, "continuous upgrade traffic starts", func(ctx context.Context) (testutil.PollResult, error) {
		current, err := readJSON[corev1.Pod](ctx, s, "get", "pod", pod.Name)
		if err != nil {
			return testutil.PollFailed, err
		}
		if current.Status.Phase == corev1.PodFailed || current.Status.Phase == corev1.PodSucceeded {
			return testutil.PollFailed, fmt.Errorf("traffic probe exited: %s", current.Status.Phase)
		}
		if current.Status.Phase != corev1.PodRunning {
			return testutil.PollPending, nil
		}
		logs, err := s.Kube(ctx, nil, "logs", pod.Name)
		if err != nil {
			return testutil.PollFailed, err
		}
		if strings.Contains(logs.Stdout, "OK HTTP HTTPS") {
			return testutil.PollSucceeded, nil
		}
		return testutil.PollPending, nil
	}); err != nil {
		return err
	}
	result = errors.Join(run(ctx), u.requireConfigIdentity(ctx))
	result = errors.Join(result, s.finishTrafficProbe(ctx, pod.Name))
	logs, logErr := s.Kube(ctx, nil, "logs", pod.Name)
	if logErr != nil {
		return errors.Join(result, logErr)
	}
	if strings.Contains(logs.Stdout, "FAILED") || !strings.Contains(logs.Stdout, "COMPLETE") || strings.Count(logs.Stdout, "OK HTTP HTTPS") < 2 {
		return errors.Join(result, fmt.Errorf("%s interrupted HTTP or HTTPS traffic; inspect the recorded probe logs", phase))
	}
	s.Infof("%s: %d consecutive HTTP/HTTPS probe pairs passed", phase, strings.Count(logs.Stdout, "OK HTTP HTTPS"))
	return result
}

func (u *upgrade) trafficProbePod(ctx context.Context) (*corev1.Pod, error) {
	s := u.session
	deployment, err := s.Deployment(ctx, "-haproxy")
	if err != nil {
		return nil, err
	}
	image := ""
	for i := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[i]
		if container.Name == "haproxy" {
			image = container.Image
		}
	}
	if image == "" {
		return nil, errors.New("HAProxy image unavailable for the continuous traffic probe")
	}
	return &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "upgrade-continuous-traffic", Namespace: s.Namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{Name: "probe", Image: image,
				Command:      []string{"/bin/sh", "-c", upgradeProbeScript, "sh", deployment.Name},
				VolumeMounts: []corev1.VolumeMount{{Name: "tls", MountPath: "/tls", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "upgrade-default-tls"}}}},
		},
	}, nil
}

func (s *Session) finishTrafficProbe(ctx context.Context, name string) error {
	if _, err := s.Kube(ctx, nil, "exec", name, "-c", "probe", "--", "touch", "/tmp/haptic-upgrade-stop"); err != nil {
		return err
	}
	return poll(ctx, 15*time.Second, time.Second, "continuous traffic final successful pair", func(ctx context.Context) (testutil.PollResult, error) {
		pod, err := readJSON[corev1.Pod](ctx, s, "get", "pod", name)
		if err != nil {
			return testutil.PollFailed, err
		}
		return probeExit(&pod)
	})
}

var errProbeFailed = errors.New("continuous HTTP/HTTPS probe failed before its final pair")

// probeExit reads the container's own exit: kubelet can report the terminal
// pod phase many seconds after the container stopped.
func probeExit(pod *corev1.Pod) (testutil.PollResult, error) {
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		if status.Name != "probe" || status.State.Terminated == nil {
			continue
		}
		if status.State.Terminated.ExitCode != 0 {
			return testutil.PollFailed, errProbeFailed
		}
		return testutil.PollSucceeded, nil
	}
	if pod.Status.Phase == corev1.PodFailed {
		return testutil.PollFailed, errProbeFailed
	}
	return testutil.PollPending, nil
}

func (u *upgrade) rollbackBaseline(ctx context.Context) error {
	if u.baseline != handoverBaseline {
		return nil
	}
	s := u.session
	if _, err := s.Helm(ctx, "rollback", s.Release, "1", "--wait", "--timeout", "20m"); err != nil {
		return err
	}
	if err := s.requireReleaseDeployed(ctx); err != nil {
		return err
	}
	if err := s.ControllerRollout(ctx); err != nil {
		return err
	}
	deployment, err := s.Deployment(ctx, controllerComponent)
	if err != nil {
		return err
	}
	if !restoredControllerImage(deployment.Spec.Template.Spec.Containers, u.baselineImage) {
		return errors.New("rollback did not restore the released controller image")
	}
	if err := s.WaitValidated(ctx, 180*time.Second); err != nil {
		return err
	}
	return s.UpgradeTraffic(ctx, "rolled back")
}

func restoredControllerImage(containers []corev1.Container, image string) bool {
	for i := range containers {
		container := &containers[i]
		if container.Name == controllerComponent {
			return image != "" && container.Image == image
		}
	}
	return false
}

func (u *upgrade) recordBaselineImage(ctx context.Context) error {
	deployment, err := u.session.Deployment(ctx, controllerComponent)
	if err != nil {
		return err
	}
	for i := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[i]
		if container.Name == controllerComponent {
			u.baselineImage = container.Image
		}
	}
	if u.baseline == handoverBaseline && u.baselineImage == "" {
		return errors.New("released controller image was not recorded")
	}
	return nil
}

func (u *upgrade) requireConfigIdentity(ctx context.Context) error {
	if u.baseline != handoverBaseline {
		return nil
	}
	list, err := readJSON[unstructured.UnstructuredList](ctx, u.session, "get", "haproxytemplateconfigs")
	if err != nil {
		return err
	}
	current := make(map[string]types.UID, len(list.Items))
	for _, config := range list.Items {
		current[config.GetName()] = config.GetUID()
	}
	if len(current) == 0 {
		return errors.New("upgrade has no configuration ownership identity")
	}
	for name, uid := range u.configUIDs {
		if current[name] != uid {
			return fmt.Errorf("configuration %s changed UID across upgrade or rollback", name)
		}
	}
	u.configUIDs = current
	return nil
}
