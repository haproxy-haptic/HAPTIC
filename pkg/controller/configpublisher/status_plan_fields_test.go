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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
)

// The apply mode and the plan a pod runs are only observable through
// status.deployedToPods, so the sync metadata must survive the whole hop from
// the deployer's event to the HAProxyCfg status.
func TestProcessStatusWork_WritesThePlanFields(t *testing.T) {
	ctx := context.Background()
	crdClient, c := newStatusWorkComponent(t)

	_, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").Create(ctx,
		&haproxyv1alpha1.HAProxyCfg{
			ObjectMeta: metav1.ObjectMeta{Name: "test-config-haproxycfg", Namespace: "default"},
		}, metav1.CreateOptions{})
	require.NoError(t, err)

	c.processStatusWork(ctx, &statusWorkItem{event: &events.ConfigAppliedToPodEvent{
		RuntimeConfigName:      "test-config-haproxycfg",
		RuntimeConfigNamespace: "default",
		PodName:                "haproxy-1",
		PodNamespace:           "default",
		Checksum:               "abc123",
		SyncMetadata: &events.SyncMetadata{
			AppliedPlanID: "plan-abc",
			RunningPlanID: "plan-abc",
			Mode:          "reload",
			Reasons:       []string{"backend added"},
		},
	}})

	cfg, err := crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").
		Get(ctx, "test-config-haproxycfg", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, cfg.Status.DeployedToPods, 1)

	pod := cfg.Status.DeployedToPods[0]
	assert.Equal(t, "plan-abc", pod.AppliedPlanID)
	assert.Equal(t, "plan-abc", pod.RunningPlanID)
	assert.Equal(t, "reload", pod.Mode)
	assert.Equal(t, []string{"backend added"}, pod.Reasons)
}
