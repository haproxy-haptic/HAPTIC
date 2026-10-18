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

package configchange

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	crdclientfake "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
)

// startStatusUpdater runs the updater's event loop for the test's lifetime.
func startStatusUpdater(t *testing.T, u *StatusUpdater) {
	t.Helper()
	u.EventBus().Start()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = u.Start(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-u.SubscriptionReady()
}

// staleLoadGateWrite is what an old controller version that cannot load the
// config writes during a rolling upgrade, after the new leader's verdict. It
// writes unconditionally, as released binaries do.
func staleLoadGateWrite(t *testing.T, crd *crdclientfake.Clientset, generation int64) {
	t.Helper()
	client := crd.HaproxyTemplateICV1alpha1().HAProxyTemplateConfigs(testNamespace)
	current, err := client.Get(context.Background(), testName, metav1.GetOptions{})
	require.NoError(t, err)
	current.Status.ObservedGeneration = generation
	current.Status.ValidationStatus = statusInvalid
	current.Status.ValidationErrors = []string{"undefined: cidr_partition"}
	setValidatedCondition(&current.Status, metav1.ConditionFalse, reasonLoadGateFailed,
		"undefined: cidr_partition", generation)
	_, err = client.UpdateStatus(context.Background(), current, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func validatedCondition(t *testing.T, crd *crdclientfake.Clientset) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(getStatus(t, crd).Conditions, conditionValidated)
}

func requireLeaderVerdict(t *testing.T, crd *crdclientfake.Clientset) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		status := getStatus(t, crd)
		cond := meta.FindStatusCondition(status.Conditions, conditionValidated)
		if !assert.NotNil(c, cond) {
			return
		}
		assert.Equal(c, metav1.ConditionTrue, cond.Status)
		assert.Equal(c, testGeneration, cond.ObservedGeneration)
		assert.Equal(c, testGeneration, status.ObservedGeneration)
		assert.Equal(c, statusValid, status.ValidationStatus)
		assert.Empty(c, status.ValidationErrors)
	}, 10*time.Second, 20*time.Millisecond)
}

// #270: an old pod in its termination grace overwrote the new leader's
// Validated=True for the same generation, and the status stayed False.
func TestStatusUpdater_LeaderVerdictWinsOverStaleWriterOfSameGeneration(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)
	startStatusUpdater(t, u)

	u.EventBus().Publish(validatedVerdict(htc, "v1"))
	requireLeaderVerdict(t, crd)

	for range 3 {
		staleLoadGateWrite(t, crd, testGeneration)
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, reasonLoadGateFailed, validatedCondition(t, crd).Reason, "precondition: the stale write landed")

	requireLeaderVerdict(t, crd)
}

func TestStatusUpdater_LeaderVerdictWinsOverStaleWriterOfOlderGeneration(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)
	startStatusUpdater(t, u)

	u.EventBus().Publish(validatedVerdict(htc, "v1"))
	requireLeaderVerdict(t, crd)

	staleLoadGateWrite(t, crd, testGeneration-1)
	requireLeaderVerdict(t, crd)
}

// A verdict for a generation the leader hasn't processed yet isn't stale: the
// leader writes its own verdict when that generation reaches it.
func TestStatusUpdater_DoesNotOverwriteANewerGeneration(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)
	u.statusGuardMinInterval = 10 * time.Millisecond
	u.statusGuardMaxInterval = 20 * time.Millisecond
	startStatusUpdater(t, u)

	u.EventBus().Publish(validatedVerdict(htc, "v1"))
	requireLeaderVerdict(t, crd)

	staleLoadGateWrite(t, crd, testGeneration+1)
	time.Sleep(200 * time.Millisecond)

	cond := validatedCondition(t, crd)
	require.NotNil(t, cond)
	assert.Equal(t, reasonLoadGateFailed, cond.Reason)
	assert.Equal(t, testGeneration+1, cond.ObservedGeneration)
}

func TestReportConfigLoadFailure_LeavesANewerGenerationAlone(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)
	u.handleConfigValidated(context.Background(), validatedVerdict(htc, "v1"))

	_, logger := testutil.NewTestBusAndLogger()
	ReportConfigLoadFailure(context.Background(), crd, events.ConfigSourceRef{
		Namespace: testNamespace, Name: testName, Generation: testGeneration - 1,
	}, []string{"boom"}, logger)

	cond := validatedCondition(t, crd)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, testGeneration, getStatus(t, crd).ObservedGeneration)
}

// A replica that leads again must not restore a verdict from its previous
// term over the one the leader in between wrote.
func TestStatusUpdater_NewTermDoesNotRestoreAPreviousTermsVerdict(t *testing.T) {
	htc := newHTC()
	u, crd := newStatusUpdaterFixture(t, htc)
	u.statusGuardMinInterval = 10 * time.Millisecond
	u.statusGuardMaxInterval = 20 * time.Millisecond
	u.EventBus().Start()
	u.handleConfigValidated(context.Background(), validatedVerdict(htc, "v1"))

	staleLoadGateWrite(t, crd, testGeneration)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = u.Start(ctx)
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	assert.Equal(t, reasonLoadGateFailed, validatedCondition(t, crd).Reason)
}
