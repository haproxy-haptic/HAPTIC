// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package kindutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/kubeexec"
	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

type ClusterOptions struct {
	Name          string
	Kubeconfig    string
	Artifacts     string
	Config        []byte
	Environment   map[string]string
	NetworkLabels map[string]string
	ReadyTimeout  time.Duration
}

// Cluster owns only nodes bearing its unique mount, including failed startup.
type Cluster struct {
	Name          string            `json:"name"`
	Kubeconfig    string            `json:"kubeconfig"`
	Artifacts     string            `json:"artifacts"`
	Owner         string            `json:"owner"`
	Environment   map[string]string `json:"environment"`
	NetworkLabels map[string]string `json:"networkLabels,omitempty"`
	ReadyTimeout  time.Duration     `json:"readyTimeout,omitempty"`
	runner        process.Runner
}

var clusterNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func NewCluster(runner process.Runner, options *ClusterOptions) (*Cluster, error) {
	if runner == nil || options == nil || !clusterNamePattern.MatchString(options.Name) || !filepath.IsAbs(options.Kubeconfig) || !filepath.IsAbs(options.Artifacts) {
		return nil, errors.New("cluster needs a runner, a lowercase name, and absolute kubeconfig and artifact paths")
	}
	if err := validateNetworkLabels(options.NetworkLabels, options.Environment[dockerNetworkSetting]); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(options.Artifacts, 0o750); err != nil {
		return nil, err
	}
	owner, err := os.MkdirTemp(options.Artifacts, "cluster-owner-")
	if err != nil {
		return nil, err
	}
	c := &Cluster{Name: options.Name, Kubeconfig: options.Kubeconfig, Artifacts: options.Artifacts, Owner: owner, Environment: normalizedDockerEnvironment(options.Environment), ReadyTimeout: options.ReadyTimeout, runner: runner}
	c.NetworkLabels = maps.Clone(options.NetworkLabels)
	c.Environment["KUBECONFIG"] = c.Kubeconfig
	config, err := ownedKindConfig(options.Config, owner, c.Environment["DOCKER_HOST"])
	if err != nil {
		return c, err
	}
	if err := os.WriteFile(filepath.Join(owner, "kind.yaml"), config, 0o600); err != nil {
		return c, err
	}
	state, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	if err := os.WriteFile(filepath.Join(owner, "cluster.json"), state, 0o600); err != nil {
		return c, err
	}
	return c, nil
}

func (c *Cluster) Client(namespace string) kubeexec.Client {
	return kubeexec.Client{Runner: c.runner, Kubeconfig: c.Kubeconfig, Context: "kind-" + c.Name, Namespace: namespace}
}

func (c *Cluster) Create(ctx context.Context, image string) error {
	nodes, err := c.nodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) != 0 {
		return fmt.Errorf("kind cluster %s already exists; choose a fresh name", c.Name)
	}
	if err := c.reserveKubeconfig(); err != nil {
		return err
	}
	if err := c.createNetwork(ctx); err != nil {
		return err
	}
	args := []string{"create", "cluster", "--name", c.Name, "--kubeconfig", c.Kubeconfig, "--config", filepath.Join(c.Owner, "kind.yaml"), "--retain"}
	if image != "" {
		args = append(args, "--image", image)
	}
	if _, err = c.run(ctx, "kind", args...); err != nil {
		return err
	}
	content, err := os.ReadFile(c.Kubeconfig)
	if err != nil {
		return err
	}
	content, err = patchOwnedKubeconfig(content, c.Name, c.Environment["DOCKER_HOST"])
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.Kubeconfig, content, 0o600); err != nil {
		return err
	}
	if result, err := c.Client("").Run(ctx, nil, "cluster-info"); err != nil {
		return fmt.Errorf("kind API unavailable: %w: %s", err, result.Stderr)
	}
	if err := c.Blackhole(ctx); err != nil {
		return err
	}
	if nodes, err = c.ownedNodes(ctx); err != nil {
		return err
	}
	return c.installCNI(ctx, nodes)
}

func (c *Cluster) reserveKubeconfig() error {
	if err := os.MkdirAll(filepath.Dir(c.Kubeconfig), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(c.Kubeconfig, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("reserve private kubeconfig: %w", err)
	}
	return file.Close()
}

func (c *Cluster) LoadImages(ctx context.Context, images ...string) error {
	nodes, err := c.ownedNodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return errors.New("owned Kind cluster has no nodes")
	}
	args := append([]string{"load", "docker-image", "--name", c.Name}, images...)
	_, err = c.run(ctx, "kind", args...)
	return err
}

func DockerEnvironment(getenv func(string) string) map[string]string {
	environment := map[string]string{"DOCKER_CONTEXT": ""}
	for _, key := range []string{"DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", dockerNetworkSetting} {
		environment[key] = getenv(key)
	}
	return environment
}

func (c *Cluster) Blackhole(ctx context.Context) error {
	nodes, err := c.ownedNodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return errors.New("owned Kind cluster has no nodes")
	}
	return c.blackholeNodes(ctx, nodes)
}

// Close leaves diagnostic files and the private kubeconfig available for inspection.
func (c *Cluster) Close(ctx context.Context) error {
	nodes, err := c.ownedNodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return c.closeNetwork(ctx)
	}
	// Delete verified container IDs rather than a name that another invocation can reuse.
	if _, err := c.run(ctx, "docker", append([]string{"rm", "--force", "--volumes"}, nodes...)...); err != nil {
		return err
	}
	return c.closeNetwork(ctx)
}

func (c *Cluster) nodes(ctx context.Context) ([]string, error) {
	result, err := c.run(ctx, "docker", "ps", "--all", "--quiet", "--no-trunc", "--filter", "label=io.x-k8s.kind.cluster="+c.Name)
	if err != nil {
		return nil, err
	}
	return strings.Fields(result.Stdout), nil
}

type nodeIdentity struct {
	ID     string `json:"Id"`
	Mounts []struct {
		Source      string
		Destination string
		RW          bool
	} `json:"Mounts"`
}

func (c *Cluster) ownedNodes(ctx context.Context) ([]string, error) {
	nodes, err := c.nodes(ctx)
	if err != nil || len(nodes) == 0 {
		return nodes, err
	}
	result, err := c.run(ctx, "docker", append([]string{dockerInspect}, nodes...)...)
	if err != nil {
		return nil, err
	}
	var identities []nodeIdentity
	if err = json.Unmarshal([]byte(result.Stdout), &identities); err != nil {
		return nil, fmt.Errorf("read node ownership: %w", err)
	}
	if len(identities) != len(nodes) {
		return nil, errors.New("docker did not return every node's ownership")
	}
	for i, identity := range identities {
		if identity.ID != nodes[i] || !hasOwnerMount(identity, c.Owner) {
			return nil, fmt.Errorf("kind cluster %s contains a foreign node %s; refusing mutation", c.Name, nodes[i])
		}
	}
	return nodes, nil
}

func hasOwnerMount(identity nodeIdentity, owner string) bool {
	for _, mount := range identity.Mounts {
		if mount.Source == owner && mount.Destination == ownerMount && !mount.RW {
			return true
		}
	}
	return false
}

func (c *Cluster) run(ctx context.Context, name string, args ...string) (process.Result, error) {
	return c.runInput(ctx, nil, name, args...)
}

func (c *Cluster) runInput(ctx context.Context, input io.Reader, name string, args ...string) (process.Result, error) {
	result, err := c.runner.Run(ctx, &process.Command{Name: name, Args: args, Env: maps.Clone(c.Environment), Stdin: input})
	if err != nil {
		return result, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(result.Stderr))
	}
	return result, nil
}

func normalizedDockerEnvironment(environment map[string]string) map[string]string {
	result := maps.Clone(environment)
	if result == nil {
		result = map[string]string{}
	}
	result["DOCKER_CONTEXT"] = ""
	result["DOCKER_HOST"] = environment["DOCKER_HOST"]
	result[dockerNetworkSetting] = environment[dockerNetworkSetting]
	return result
}

const dockerInspect = "inspect"
