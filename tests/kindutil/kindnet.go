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

	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

//go:embed kindnet.yaml
var kindnetManifest []byte

var manifestImage = regexp.MustCompile(`(?m)^\s*image:\s*(\S+)\s*$`)

const kindnetPullAttempts = 3

func kindnetImage() (string, error) {
	var images []string
	for _, match := range manifestImage.FindAllSubmatch(kindnetManifest, -1) {
		images = append(images, string(match[1]))
	}
	images = slices.Compact(images)
	if len(images) != 1 || !strings.Contains(images[0], "@sha256:") {
		return "", fmt.Errorf("kindnet manifest must use one digest-pinned image, found %v", images)
	}
	return images[0], nil
}

// InstallKindnetCluster installs kindnet on a caller-selected, already-created Kind cluster without taking ownership.
func InstallKindnetCluster(ctx context.Context, runner process.Runner, name string, environment map[string]string, readyTimeout time.Duration) error {
	if runner == nil || !clusterNamePattern.MatchString(name) {
		return errors.New("cluster runner and valid name are required")
	}
	cluster := &Cluster{Name: name, Environment: normalizedDockerEnvironment(environment), ReadyTimeout: readyTimeout, runner: runner}
	nodes, err := cluster.nodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return errors.New("selected Kind cluster has no nodes")
	}
	return cluster.installKindnet(ctx, nodes)
}

// installKindnet waits for readiness only when ReadyTimeout is set, as kind create --wait did.
func (c *Cluster) installKindnet(ctx context.Context, nodes []string) error {
	image, err := kindnetImage()
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if err := c.pullOnNode(ctx, node, image); err != nil {
			return err
		}
	}
	controlPlane, err := c.controlPlane(ctx, nodes)
	if err != nil {
		return err
	}
	if _, err := c.nodeKubectl(ctx, controlPlane, kindnetManifest, "apply", "-f", "-"); err != nil {
		return err
	}
	if c.ReadyTimeout <= 0 {
		return nil
	}
	deadline := time.Now().Add(c.ReadyTimeout)
	for _, wait := range [][]string{
		{"--namespace", "kube-system", "rollout", "status", "daemonset/kindnet"},
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
	for attempt := 1; attempt <= kindnetPullAttempts; attempt++ {
		if _, err = c.run(ctx, "docker", "exec", node, "crictl", "pull", image); err == nil || attempt == kindnetPullAttempts {
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
