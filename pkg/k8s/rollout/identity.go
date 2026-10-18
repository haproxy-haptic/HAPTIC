package rollout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/haproxy-haptic/HAPTIC/pkg/buildinfo"
)

const identityResponseLimit = 4096

func ReadIdentity(containerName string) IdentityReader {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext:     (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			IdleConnTimeout: 30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(ctx context.Context, pod *corev1.Pod) (buildinfo.Identity, error) {
		address, err := identityAddress(pod, containerName)
		if err != nil {
			return buildinfo.Identity{}, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, http.NoBody)
		if err != nil {
			return buildinfo.Identity{}, err
		}
		response, err := client.Do(request)
		if err != nil {
			return buildinfo.Identity{}, fmt.Errorf("reach controller health endpoint %s; allow the preflight Pod to reach this port: %w", address, err)
		}
		defer response.Body.Close()
		return decodeIdentity(response)
	}
}

func decodeIdentity(response *http.Response) (buildinfo.Identity, error) {
	if response.StatusCode == http.StatusNotFound {
		return buildinfo.Identity{}, nil
	}
	if response.StatusCode != http.StatusOK {
		return buildinfo.Identity{}, fmt.Errorf("controller build endpoint returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, identityResponseLimit+1))
	if err != nil {
		return buildinfo.Identity{}, err
	}
	if len(data) > identityResponseLimit {
		return buildinfo.Identity{}, fmt.Errorf("controller build response exceeds %d bytes", identityResponseLimit)
	}
	var identity buildinfo.Identity
	if err := json.Unmarshal(data, &identity); err != nil {
		return identity, fmt.Errorf("decode controller build identity: %w", err)
	}
	return identity, identity.Validate()
}

func identityAddress(pod *corev1.Pod, containerName string) (string, error) {
	container := Container(pod.Spec.Containers, containerName)
	if container == nil {
		return "", fmt.Errorf("controller Pod %s has no container %s", pod.Name, containerName)
	}
	if net.ParseIP(pod.Status.PodIP) == nil {
		return "", fmt.Errorf("controller Pod %s has no valid Pod IP", pod.Name)
	}
	for _, port := range container.Ports {
		if port.Name == "healthz" && port.ContainerPort > 0 && port.ContainerPort <= 65535 {
			return "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port.ContainerPort))) + "/build", nil
		}
	}
	return "", fmt.Errorf("controller Pod %s has no healthz port", pod.Name)
}
