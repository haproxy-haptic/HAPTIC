// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package kindutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	kindconfig "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/yaml"
)

const ownerMount = "/etc/haptic-test-owner"
const allInterfaces = "0.0.0.0"

func ownedKindConfig(input []byte, owner, dockerHost string) ([]byte, error) {
	if len(input) == 0 {
		input = []byte(BaseKindConfig)
	}
	var config kindconfig.Cluster
	if err := yaml.UnmarshalStrict(input, &config); err != nil {
		return nil, fmt.Errorf("decode Kind config: %w", err)
	}
	if config.Kind != "Cluster" || config.APIVersion != "kind.x-k8s.io/v1alpha4" {
		return nil, errors.New("expected a Kind v1alpha4 Cluster")
	}
	if len(config.Nodes) == 0 {
		config.Nodes = []kindconfig.Node{{Role: kindconfig.ControlPlaneRole}}
	}
	config.Networking.DisableDefaultCNI = true
	if err := markOwnedNodes(&config, owner); err != nil {
		return nil, err
	}
	host, err := remoteDockerHost(dockerHost)
	if err != nil {
		return nil, err
	}
	if host != "" {
		config.Networking.APIServerAddress = allInterfaces
		patch, marshalErr := json.Marshal([]map[string]any{{"op": "add", "path": "/apiServer/certSANs/-", "value": host}})
		if marshalErr != nil {
			return nil, marshalErr
		}
		for _, version := range []string{"v1beta3", "v1beta4"} {
			config.KubeadmConfigPatchesJSON6902 = append(config.KubeadmConfigPatchesJSON6902, kindconfig.PatchJSON6902{Group: "kubeadm.k8s.io", Version: version, Kind: "ClusterConfiguration", Patch: string(patch)})
		}
	} else if config.Networking.APIServerAddress == "" || config.Networking.APIServerAddress == allInterfaces {
		config.Networking.APIServerAddress = "127.0.0.1"
	}
	configurePortMappings(&config, host != "")
	return yaml.Marshal(config)
}

func remoteDockerHost(address string) (string, error) {
	if !strings.HasPrefix(address, "tcp://") {
		return "", nil
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("invalid Docker host %q", address)
	}
	return parsed.Hostname(), nil
}

func patchOwnedKubeconfig(content []byte, clusterName, dockerHost string) ([]byte, error) {
	config, err := clientcmd.Load(content)
	if err != nil {
		return nil, fmt.Errorf("decode kubeconfig: %w", err)
	}
	identity := "kind-" + clusterName
	selected, ok := config.Contexts[identity]
	if !ok || len(config.Contexts) != 1 || len(config.Clusters) != 1 || selected.Cluster != identity || config.Clusters[identity] == nil {
		return nil, errors.New("kubeconfig does not identify only the owned Kind cluster")
	}
	host, err := remoteDockerHost(dockerHost)
	if err != nil {
		return nil, err
	}
	if host != "" {
		server, parseErr := url.Parse(config.Clusters[identity].Server)
		if parseErr != nil || server.Scheme != "https" || server.Port() == "" {
			return nil, errors.New("kind kubeconfig has no HTTPS API port")
		}
		server.Host = net.JoinHostPort(host, server.Port())
		config.Clusters[identity].Server = server.String()
	}
	config.CurrentContext = identity
	return clientcmd.Write(*config)
}

func markOwnedNodes(config *kindconfig.Cluster, owner string) error {
	controlPlanes := 0
	for i := range config.Nodes {
		node := &config.Nodes[i]
		if node.Role == "" || node.Role == kindconfig.ControlPlaneRole {
			controlPlanes++
		}
		for _, mount := range node.ExtraMounts {
			if mount.ContainerPath == ownerMount {
				return errors.New("kind config already declares the test ownership mount")
			}
		}
		node.ExtraMounts = append(node.ExtraMounts, kindconfig.Mount{HostPath: owner, ContainerPath: ownerMount, Readonly: true})
	}
	// Kind's implicit multi-control-plane load balancer cannot carry the ownership mount.
	if controlPlanes != 1 {
		return errors.New("test clusters require one control-plane node")
	}
	return nil
}

func configurePortMappings(config *kindconfig.Cluster, remote bool) {
	for i := range config.Nodes {
		for j := range config.Nodes[i].ExtraPortMappings {
			mapping := &config.Nodes[i].ExtraPortMappings[j]
			if remote {
				mapping.ListenAddress = allInterfaces
			} else if mapping.ListenAddress == "" || mapping.ListenAddress == allInterfaces {
				mapping.ListenAddress = "127.0.0.1"
			}
		}
	}
}
