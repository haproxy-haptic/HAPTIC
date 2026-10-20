// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const kubeSystemNamespace = "kube-system"

func (s *Session) Diagnostics(ctx context.Context) {
	// kube-system first: its kube-apiserver log is the only record of a webhook transport failure (#300),
	// and the later namespaces can exhaust the caller's one-minute budget.
	for _, namespace := range []string{kubeSystemNamespace, s.Namespace, certManagerNamespace, argoNamespace, fluxNamespace, gitopsNamespace} {
		s.namespaceDiagnostics(ctx, namespace)
	}
	for _, resource := range []string{"haproxytemplateconfigs", "haproxycfgs", "haproxytemplatelibraries", "haproxyvalidationtests"} {
		installed, err := s.Kube(ctx, nil, "get", "crd", resource+".haproxy-haptic.org", "--ignore-not-found", "-o", "name")
		if err != nil || strings.TrimSpace(installed.Stdout) == "" {
			continue
		}
		if _, err := s.Kube(ctx, nil, "get", resource, "-o", "yaml"); err != nil {
			s.Infof("diagnostics for %s: %v", resource, err)
		}
	}
}

func (s *Session) namespaceDiagnostics(ctx context.Context, namespace string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for _, args := range [][]string{{kubectlGet, "pods,events,jobs,deployments,services,endpoints", "-o", "yaml"}, {"describe", podsResource}} {
		if _, err := s.Kube(ctx, nil, append([]string{"-n", namespace}, args...)...); err != nil {
			s.Infof("diagnostics in %s: %v", namespace, err)
		}
	}
	pods, err := readJSON[corev1.PodList](ctx, s, "-n", namespace, "get", "pods")
	if err != nil {
		s.Infof("pod diagnostics in %s: %v", namespace, err)
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		containers := append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
		for j := range containers {
			container := &containers[j]
			for _, previous := range []bool{false, true} {
				args := []string{"-n", namespace, "logs", pod.Name, "-c", container.Name, "--tail=300"}
				if previous {
					args = append(args, "--previous")
				}
				_, _ = s.Kube(ctx, nil, args...)
			}
		}
	}
}
