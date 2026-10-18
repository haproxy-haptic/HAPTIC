// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/haproxy-haptic/HAPTIC/tests/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/traffic"
	"github.com/haproxy-haptic/HAPTIC/tests/tunnel"
)

func (s *Session) UpgradeTraffic(ctx context.Context, phase string) error {
	deployment, err := s.Deployment(ctx, "-haproxy")
	if err != nil {
		return err
	}
	if _, err := s.Kube(ctx, nil, "rollout", "status", "deployment/"+deployment.Name, "--timeout=7m"); err != nil {
		return err
	}
	pods, err := s.Pods(ctx, "loadbalancer")
	if err != nil {
		return err
	}
	pods = livePods(pods)
	if len(pods) != 2 {
		return fmt.Errorf("%s: expected both HAProxy replicas, found %d", phase, len(pods))
	}
	secret, err := readJSON[corev1.Secret](ctx, s, "get", "secret", "upgrade-default-tls")
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data[corev1.TLSCertKey]) {
		return errors.New("upgrade-default-tls has no valid certificate")
	}
	for i := range pods {
		if _, err := s.Kube(ctx, nil, "wait", "pod/"+pods[i].Name, "--for=condition=Ready", "--timeout=180s"); err != nil {
			return err
		}
		if err := s.probeUpgradePod(ctx, &pods[i], roots); err != nil {
			return err
		}
		s.Infof("%s: %s serves the existing HTTP and HTTPS routes", phase, pods[i].Name)
	}
	if _, err := s.Kube(ctx, nil, "get", "pods", "-o", "json"); err != nil {
		return err
	}
	return s.CheckControllerOutput(ctx)
}

func (s *Session) probeUpgradePod(ctx context.Context, pod *corev1.Pod, roots *x509.CertPool) error {
	ports, err := podRoutePorts(pod)
	if err != nil {
		return err
	}
	return traffic.Wait(ctx, &traffic.PodProbe{Client: s.Client, Target: "pod/" + pod.Name, Ports: ports, StartupTimeout: 30 * time.Second, RouteTimeout: 120 * time.Second, Interval: time.Second, Check: func(ctx context.Context, locals []int) error {
		client := httpclient.New(&httpclient.Config{Host: loopbackHost, HTTPPort: locals[0], HTTPSPort: locals[1], TLS: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}})
		defer client.CloseIdleConnections()
		for _, request := range []*httpclient.Request{client.GET("http.upgrade.test", "/upgrade-check"), client.HTTPS("tls.upgrade.test", "/upgrade-check")} {
			if err := checkUpgradeRequest(ctx, request); err != nil {
				return err
			}
		}
		return nil
	}})
}

func checkUpgradeRequest(ctx context.Context, request *httpclient.Request) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := request.Do(ctx)
	if err != nil {
		return err
	}
	if response.Status != 200 || response.Echo == nil || !strings.HasPrefix(response.Echo.PodHostname, "upgrade-backend-") || response.Echo.Path != "/upgrade-check" {
		return fmt.Errorf("route did not reach the unchanged upgrade backend: HTTP %d: %s", response.Status, response.Body)
	}
	return nil
}

func podRoutePorts(pod *corev1.Pod) ([]tunnel.Port, error) {
	result := make([]tunnel.Port, 0, 2)
	for _, name := range []string{"http", "https"} {
		var matches []int
		for i := range pod.Spec.Containers {
			for _, port := range pod.Spec.Containers[i].Ports {
				if port.Name == name {
					matches = append(matches, int(port.ContainerPort))
				}
			}
		}
		if len(matches) != 1 || matches[0] <= 0 || matches[0] > 65535 {
			return nil, fmt.Errorf("pod %s needs one valid %s container port", pod.Name, name)
		}
		result = append(result, tunnel.Port{Remote: name, Target: matches[0]})
	}
	return result, nil
}

func livePods(pods []corev1.Pod) []corev1.Pod {
	result := make([]corev1.Pod, 0, len(pods))
	for i := range pods {
		if pods[i].DeletionTimestamp == nil {
			result = append(result, pods[i])
		}
	}
	return result
}

func (s *Session) CheckControllerOutput(ctx context.Context) error {
	pods, err := s.Pods(ctx, "controller")
	if err != nil {
		return err
	}
	pods = livePods(pods)
	if len(pods) == 0 {
		return errors.New("no running controller pods")
	}
	for i := range pods {
		result, err := s.Kube(ctx, nil, "logs", "pod/"+pods[i].Name, "--all-containers", "--prefix", "--tail=-1")
		if err != nil {
			return err
		}
		for _, rejection := range []string{"Rendered output rejected", "content differs from its plan file", "ArtifactContentMismatch"} {
			if strings.Contains(result.Stdout, rejection) {
				return fmt.Errorf("controller %s rejected inconsistent output: %s", pods[i].Name, rejection)
			}
		}
	}
	return nil
}
