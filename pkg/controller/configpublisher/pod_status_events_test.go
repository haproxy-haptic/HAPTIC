// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package configpublisher

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
)

func (env testEnv) publishRuntimeConfig(t *testing.T, uid types.UID, checksum string) {
	t.Helper()
	_, err := env.publisher.PublishConfig(env.ctx, &configpublisher.PublishRequest{
		TemplateConfigName:      "test-config",
		TemplateConfigNamespace: "default",
		TemplateConfigUID:       uid,
		Config:                  "global\n  daemon\n",
		ConfigPath:              "/etc/haproxy/haproxy.cfg",
		Checksum:                checksum,
	})
	require.NoError(t, err)
}

func (env testEnv) publishTemplateForPodCleanup() {
	templateConfig := &v1alpha1.HAProxyTemplateConfig{ObjectMeta: metav1.ObjectMeta{
		Name: "test-config", Namespace: "default", UID: "test-uid-123",
	}}
	env.bus.Publish(events.NewConfigValidatedEvent(nil, templateConfig, "v1", "secret-v1"))
}

func (env testEnv) waitForDeployedPods(t *testing.T, count int) []v1alpha1.PodDeploymentStatus {
	t.Helper()
	var pods []v1alpha1.PodDeploymentStatus
	err := wait.PollUntilContextTimeout(env.ctx, 10*time.Millisecond, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		cfg, err := env.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		pods = cfg.Status.DeployedToPods
		return len(pods) == count, nil
	})
	require.NoErrorf(t, err, "expected %d deployed pods, last observed %v", count, pods)
	return pods
}

func TestComponent_ConfigAppliedToPodEvent(t *testing.T) {
	env := newTestComponent(t)
	env.start()

	env.publishRuntimeConfig(t, "test-uid-123", "checksum123")

	env.bus.Publish(events.NewConfigAppliedToPodEvent(
		"test-config-haproxycfg",
		"default",
		"haproxy-pod-1",
		"haproxy-ns",
		"",
		"",
		"checksum123",
		false, // isDriftCheck
		nil,
	))

	pod := env.waitForDeployedPods(t, 1)[0]
	assert.Equal(t, "haproxy-pod-1", pod.PodName)
	assert.Equal(t, "checksum123", pod.Checksum)
}

func TestComponent_HAProxyPodTerminatedEvent(t *testing.T) {
	env := newTestComponent(t)
	env.start()

	env.publishTemplateForPodCleanup()
	env.publishRuntimeConfig(t, "test-uid-123", "checksum123")

	env.bus.Publish(events.NewConfigAppliedToPodEvent(
		"test-config-haproxycfg",
		"default",
		"haproxy-pod-1",
		"haproxy-ns",
		"",
		"",
		"checksum123",
		false, // isDriftCheck
		nil,
	))

	env.waitForDeployedPods(t, 1)

	env.bus.Publish(events.NewHAProxyPodTerminatedEvent("haproxy-pod-1", "haproxy-ns", ""))

	env.waitForDeployedPods(t, 0)
}

func TestComponent_MultiplePods(t *testing.T) {
	env := newTestComponent(t)
	env.start()

	env.publishTemplateForPodCleanup()
	env.publishRuntimeConfig(t, "test-uid-123", "checksum123")

	for i := 1; i <= 3; i++ {
		env.bus.Publish(events.NewConfigAppliedToPodEvent(
			"test-config-haproxycfg",
			"default",
			fmt.Sprintf("haproxy-pod-%d", i),
			"haproxy-ns",
			"",
			"",
			"checksum123",
			false, // isDriftCheck
			nil,
		))
	}

	env.waitForDeployedPods(t, 3)

	env.bus.Publish(events.NewHAProxyPodTerminatedEvent("haproxy-pod-2", "haproxy-ns", ""))

	pods := env.waitForDeployedPods(t, 2)

	podNames := make([]string, len(pods))
	for i, pod := range pods {
		podNames[i] = pod.PodName
	}
	assert.Contains(t, podNames, "haproxy-pod-1")
	assert.Contains(t, podNames, "haproxy-pod-3")
	assert.NotContains(t, podNames, "haproxy-pod-2")
}

func TestComponent_ConfigAppliedToPodEvent_WithSyncMetadata(t *testing.T) {
	env := newTestComponent(t)
	env.start()

	env.publishRuntimeConfig(t, "test-uid-123", "checksum-sync")

	syncMetadata := &events.SyncMetadata{
		ReloadTriggered: true,
		SyncDuration:    100 * time.Millisecond,
		OperationCounts: events.OperationCounts{
			TotalAPIOperations: 5,
			BackendsAdded:      2,
			ServersAdded:       3,
		},
		Error: "",
	}

	env.bus.Publish(events.NewConfigAppliedToPodEvent(
		"test-config-haproxycfg",
		"default",
		"haproxy-pod-sync",
		"haproxy-ns",
		"",
		"",
		"checksum-sync",
		false, // isDriftCheck
		syncMetadata,
	))

	pod := env.waitForDeployedPods(t, 1)[0]
	assert.Equal(t, "haproxy-pod-sync", pod.PodName)
	assert.Equal(t, "checksum-sync", pod.Checksum)
}

// The deployer filters no-op drift checks before they reach the publisher.
func TestComponent_ConfigAppliedToPodEvent_DriftCheck_WithChanges(t *testing.T) {
	env := newTestComponent(t)
	env.start()

	env.publishRuntimeConfig(t, "test-uid-drift", "checksum-drift")

	env.bus.Publish(events.NewConfigAppliedToPodEvent(
		"test-config-haproxycfg",
		"default",
		"haproxy-pod-drift",
		"haproxy-ns",
		"",
		"",
		"checksum-drift",
		true, // isDriftCheck
		&events.SyncMetadata{
			SyncDuration: 100 * time.Millisecond,
			OperationCounts: events.OperationCounts{
				TotalAPIOperations: 1,
				ServersAdded:       1,
			},
		},
	))

	pod := env.waitForDeployedPods(t, 1)[0]
	assert.Equal(t, "haproxy-pod-drift", pod.PodName)
	assert.Equal(t, "checksum-drift", pod.Checksum)
}

func TestComponent_ConfigAppliedToPodEvent_WithError(t *testing.T) {
	env := newTestComponent(t)
	env.start()

	env.publishRuntimeConfig(t, "test-uid-error", "checksum-error")

	syncMetadata := &events.SyncMetadata{
		ReloadTriggered: false,
		SyncDuration:    50 * time.Millisecond,
		OperationCounts: events.OperationCounts{
			TotalAPIOperations: 0,
		},
		Error: "connection refused to dataplane API",
	}

	env.bus.Publish(events.NewConfigAppliedToPodEvent(
		"test-config-haproxycfg",
		"default",
		"haproxy-pod-error",
		"haproxy-ns",
		"",
		"",
		"checksum-error",
		false,
		syncMetadata,
	))

	pod := env.waitForDeployedPods(t, 1)[0]
	assert.Equal(t, "haproxy-pod-error", pod.PodName)
	assert.Equal(t, "connection refused to dataplane API", pod.LastError)
}
