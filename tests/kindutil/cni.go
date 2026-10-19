// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package kindutil

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

//go:embed calico.yaml
var cniManifest []byte

// PodSubnet is Kind's default pod subnet; Calico's IP pool must match kube-proxy's cluster CIDR.
const PodSubnet = "10.244.0.0/16"

var manifestImage = regexp.MustCompile(`(?m)^\s*image:\s*(\S+)\s*$`)

const (
	cniPullAttempts        = 3
	defaultCNIReadyTimeout = 3 * time.Minute
)

func cniImages() ([]string, error) {
	var images []string
	for _, match := range manifestImage.FindAllSubmatch(cniManifest, -1) {
		image := string(match[1])
		if !strings.Contains(image, "@sha256:") {
			return nil, fmt.Errorf("CNI manifest image %s is not digest-pinned", image)
		}
		images = append(images, image)
	}
	slices.Sort(images)
	images = slices.Compact(images)
	if len(images) == 0 {
		return nil, errors.New("CNI manifest has no images")
	}
	return images, nil
}

// InstallCNICluster installs the test CNI on a caller-selected, already-created Kind cluster without taking ownership.
func InstallCNICluster(ctx context.Context, runner process.Runner, name string, environment map[string]string) error {
	if runner == nil || !clusterNamePattern.MatchString(name) {
		return errors.New("cluster runner and valid name are required")
	}
	cluster := &Cluster{Name: name, Environment: normalizedDockerEnvironment(environment), runner: runner}
	nodes, err := cluster.nodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return errors.New("selected Kind cluster has no nodes")
	}
	return cluster.installCNI(ctx, nodes)
}

func (c *Cluster) installCNI(ctx context.Context, nodes []string) error {
	images, err := cniImages()
	if err != nil {
		return err
	}
	pulls, pullCtx := errgroup.WithContext(ctx)
	for _, node := range nodes {
		for _, image := range images {
			pulls.Go(func() error { return c.pullOnNode(pullCtx, node, image) })
		}
	}
	if err := pulls.Wait(); err != nil {
		return err
	}
	controlPlane, err := c.controlPlane(ctx, nodes)
	if err != nil {
		return err
	}
	// Calico's CRDs exceed the client-side last-applied annotation limit.
	if _, err := c.nodeKubectl(ctx, controlPlane, cniManifest, "apply", "--server-side", "-f", "-"); err != nil {
		return err
	}
	readyTimeout := c.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = defaultCNIReadyTimeout
	}
	deadline := time.Now().Add(readyTimeout)
	for _, wait := range [][]string{
		{"--namespace", "kube-system", "rollout", "status", "daemonset/calico-node"},
		{"--namespace", "kube-system", "rollout", "status", "deployment/calico-kube-controllers"},
		{"wait", "--for", "condition=Ready", "nodes", "--all"},
	} {
		// kubectl treats a zero timeout as unbounded.
		remaining := max(time.Until(deadline), time.Second).Round(time.Second)
		if _, err := c.nodeKubectl(ctx, controlPlane, nil, append(wait, "--timeout", remaining.String())...); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cluster) pullOnNode(ctx context.Context, node, image string) error {
	var err error
	for attempt := 1; attempt <= cniPullAttempts; attempt++ {
		if _, err = c.run(ctx, "docker", "exec", node, "crictl", "pull", image); err == nil || attempt == cniPullAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	return err
}

func (c *Cluster) controlPlane(ctx context.Context, nodes []string) (string, error) {
	result, err := c.run(ctx, "docker", "ps", "--all", "--quiet", "--no-trunc", "--filter", "label=io.x-k8s.kind.cluster="+c.Name, "--filter", "label=io.x-k8s.kind.role=control-plane")
	if err != nil {
		return "", err
	}
	ids := strings.Fields(result.Stdout)
	if len(ids) != 1 || !slices.Contains(nodes, ids[0]) {
		return "", fmt.Errorf("kind cluster %s has no single control-plane among its nodes", c.Name)
	}
	return ids[0], nil
}

func (c *Cluster) nodeKubectl(ctx context.Context, node string, input []byte, args ...string) (process.Result, error) {
	command := append([]string{"exec", "-i", node, "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf"}, args...)
	return c.runInput(ctx, bytes.NewReader(input), "docker", command...)
}
