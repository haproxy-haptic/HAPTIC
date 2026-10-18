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
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

var leaseResource = coordinationv1.SchemeGroupVersion.WithResource("leases")

func intentAuthority() DeploymentAuthority {
	return DeploymentAuthority{Namespace: "default", Name: "fleet", UID: "template-uid", Epoch: 1, Claim: "old-claim", LeaseName: "leader", Identity: "old-leader"}
}

// successor is the term that takes the leader Lease from authority.
func successor(authority *DeploymentAuthority) DeploymentAuthority {
	fresh := *authority
	fresh.Epoch, fresh.Claim, fresh.Identity = authority.Epoch+1, "new-claim", "new-leader"
	return fresh
}

func setIntentLeader(t *testing.T, ctx context.Context, client *fake.Clientset, authority *DeploymentAuthority) {
	t.Helper()
	leases := client.CoordinationV1().Leases(authority.Namespace)
	lease, err := leases.Get(ctx, authority.LeaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		lease, err = leases.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: authority.LeaseName}}, metav1.CreateOptions{})
	}
	require.NoError(t, err)
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[leaderEpochAnnotation] = strconv.FormatUint(authority.Epoch, 10)
	lease.Spec.HolderIdentity = ptr.To(authority.Identity)
	_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func leaderLeaseOf(t *testing.T, ctx context.Context, client *fake.Clientset, authority *DeploymentAuthority) *coordinationv1.Lease {
	t.Helper()
	lease, err := client.CoordinationV1().Leases(authority.Namespace).Get(ctx, authority.LeaseName, metav1.GetOptions{})
	require.NoError(t, err)
	return lease
}

// setLegacyIntent writes the intent where 0.5 and 0.6 kept it.
func setLegacyIntent(t *testing.T, ctx context.Context, client *fake.Clientset, authority *DeploymentAuthority, encoded string) {
	t.Helper()
	lease := leaderLeaseOf(t, ctx, client, authority)
	lease.Annotations[deploymentIntentAnnotation] = encoded
	_, err := client.CoordinationV1().Leases(authority.Namespace).Update(ctx, lease, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func intentLeaseName(authority *DeploymentAuthority) string {
	return stableResourceName(authority.Name, "-retained-state", authority.Name)
}

func leaseWrites(client *fake.Clientset, name string) int {
	writes := 0
	for _, action := range client.Actions() {
		write, ok := action.(k8stesting.UpdateAction)
		if ok && action.GetResource() == leaseResource && write.GetObject().(*coordinationv1.Lease).Name == name {
			writes++
		}
	}
	return writes
}

// The elector must be the only writer of the leader Lease while it holds it:
// any other write fails the renewal it lands in (issue #413).
func TestDeploymentIntentNeverWritesTheLeaderLease(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	setIntentLeader(t, ctx, client, &authority)
	client.ClearActions()

	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-b", "checksum-b"))
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &authority, "plan-b", "checksum-b"))
	require.NoError(t, publisher.CheckDeploymentAuthority(ctx, &authority))

	require.Zero(t, leaseWrites(client, authority.LeaseName))
	require.Positive(t, leaseWrites(client, intentLeaseName(&authority)))
	require.NotContains(t, leaderLeaseOf(t, ctx, client, &authority).Annotations, deploymentIntentAnnotation)
}

func TestDeploymentIntentPrecedesAcknowledgementAcrossFailover(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	setIntentLeader(t, ctx, client, &authority)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-b", "checksum-b"))
	fresh := successor(&authority)
	setIntentLeader(t, ctx, client, &fresh)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &fresh))
	require.ErrorContains(t, publisher.CheckDeploymentIntent(ctx, &fresh, "plan-a", "checksum-a"), "newer deployment intent")
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &fresh, "plan-b", "checksum-b"))
	require.ErrorContains(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"), "newer controller term")
	require.ErrorContains(t, publisher.ClaimDeploymentAuthority(ctx, &authority), "newer controller term")
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &fresh, "plan-b", "checksum-b"))
}

func TestTakeoverFencesADelayedDeploymentIntent(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	setIntentLeader(t, ctx, client, &authority)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"))
	fresh := successor(&authority)
	setIntentLeader(t, ctx, client, &fresh)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &fresh))
	require.Error(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-b", "checksum-b"))
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &fresh, "plan-a", "checksum-a"))
}

// A deposed term is refused as soon as the leader Lease names its successor,
// before the successor's first claim reaches the retained-state Lease.
func TestDeposedTermCannotRecordBeforeItsSuccessorClaims(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	setIntentLeader(t, ctx, client, &authority)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"))
	fresh := successor(&authority)
	setIntentLeader(t, ctx, client, &fresh)

	require.ErrorIs(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-b", "checksum-b"), ErrDeploymentAuthorityLost)
	require.ErrorIs(t, publisher.CheckDeploymentAuthority(ctx, &authority), ErrDeploymentAuthorityLost)
	require.ErrorIs(t, publisher.CheckDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"), ErrDeploymentAuthorityLost)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &fresh))
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &fresh, "plan-a", "checksum-a"))
}

// The leader Lease is read again after the intent lands: a term deposed while
// its write was in flight must not apply. Its successor inherits an intent no
// pod received, which refuses older checkpoints rather than trusting one.
func TestRecordFailsWhenLeadershipMovesDuringTheWrite(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	setIntentLeader(t, ctx, client, &authority)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
	fresh := successor(&authority)
	moved := false
	client.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lease := action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease).DeepCopy()
		if moved || lease.Name != intentLeaseName(&authority) {
			return false, nil, nil
		}
		moved = true
		require.NoError(t, client.Tracker().Update(leaseResource, lease, authority.Namespace))
		stored, err := client.Tracker().Get(leaseResource, authority.Namespace, authority.LeaseName)
		require.NoError(t, err)
		leader := stored.(*coordinationv1.Lease)
		leader.Spec.HolderIdentity = ptr.To(fresh.Identity)
		leader.Annotations[leaderEpochAnnotation] = strconv.FormatUint(fresh.Epoch, 10)
		require.NoError(t, client.Tracker().Update(leaseResource, leader, authority.Namespace))
		return true, lease, nil
	})

	require.ErrorIs(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-b", "checksum-b"), ErrDeploymentAuthorityLost)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &fresh))
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &fresh, "plan-b", "checksum-b"))
}

func TestDeploymentAuthorityCheckFailsOnceANewerTermClaims(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	setIntentLeader(t, ctx, client, &authority)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
	require.NoError(t, publisher.CheckDeploymentAuthority(ctx, &authority))

	sameHolder := authority
	sameHolder.Claim = "restarted-term"
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &sameHolder))
	require.ErrorContains(t, publisher.CheckDeploymentAuthority(ctx, &authority), "newer controller term")

	fresh := successor(&authority)
	setIntentLeader(t, ctx, client, &fresh)
	require.ErrorContains(t, publisher.CheckDeploymentAuthority(ctx, &sameHolder), "newer controller term")
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &fresh))
	require.NoError(t, publisher.CheckDeploymentAuthority(ctx, &fresh))
}

func TestDeploymentIntentRechecksAuthorityAfterConflict(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	setIntentLeader(t, ctx, client, &authority)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"))
	fresh := successor(&authority)
	raced := false
	client.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lease := action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease)
		if raced || lease.Name != intentLeaseName(&authority) {
			return false, nil, nil
		}
		raced = true
		stored, err := client.Tracker().Get(leaseResource, authority.Namespace, authority.LeaseName)
		require.NoError(t, err)
		leader := stored.(*coordinationv1.Lease)
		leader.Spec.HolderIdentity = ptr.To(fresh.Identity)
		leader.Annotations[leaderEpochAnnotation] = strconv.FormatUint(fresh.Epoch, 10)
		require.NoError(t, client.Tracker().Update(leaseResource, leader, authority.Namespace))
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, lease.Name, errors.New("leadership changed"))
	})
	require.ErrorContains(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-b", "checksum-b"), "newer controller term")
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &fresh))
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &fresh, "plan-a", "checksum-a"))
}

// A claim delayed past its successor's is refused by the retained-state Lease
// itself, even when its leader Lease read predates the takeover.
func TestDelayedClaimOfADeposedTermIsRefused(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	setIntentLeader(t, ctx, client, &authority)
	stale := leaderLeaseOf(t, ctx, client, &authority)
	fresh := successor(&authority)
	setIntentLeader(t, ctx, client, &fresh)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &fresh))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &fresh, "plan-a", "checksum-a"))
	client.PrependReactor("get", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetName() != authority.LeaseName {
			return false, nil, nil
		}
		return true, stale.DeepCopy(), nil
	})

	var ahead *IntentEpochAheadError
	require.ErrorAs(t, publisher.ClaimDeploymentAuthority(ctx, &authority), &ahead)
	require.Equal(t, fresh.Epoch, ahead.Epoch)
}

// A recreated leader Lease restarts the epoch counter but leaves the
// retained-state Lease, so the intent survives once the term lifts its epoch.
func TestRecreatedLeaderLeaseKeepsTheDeploymentIntent(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	authority.Epoch = 5
	setIntentLeader(t, ctx, client, &authority)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"))
	require.NoError(t, client.CoordinationV1().Leases(authority.Namespace).Delete(ctx, authority.LeaseName, metav1.DeleteOptions{}))
	restarted := authority
	restarted.Epoch, restarted.Claim = 1, "restarted"
	setIntentLeader(t, ctx, client, &restarted)

	var ahead *IntentEpochAheadError
	require.ErrorAs(t, publisher.ClaimDeploymentAuthority(ctx, &restarted), &ahead)
	require.Equal(t, uint64(5), ahead.Epoch)

	restarted.Epoch = ahead.Epoch + 1
	setIntentLeader(t, ctx, client, &restarted)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &restarted))
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &restarted, "plan-a", "checksum-a"))
}

func TestStandaloneDeploymentIntentSurvivesRestart(t *testing.T) {
	ctx, _, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	authority.Standalone = true
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-b", "checksum-b"))
	fresh := authority
	fresh.Claim = "restarted"
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &fresh))
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &fresh, "plan-b", "checksum-b"))
	require.Error(t, publisher.CheckDeploymentIntent(ctx, &fresh, "plan-a", "checksum-a"))
	require.Error(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"))
}

func TestDeploymentIntentRefusesUnknownOrForeignState(t *testing.T) {
	for _, tc := range []struct{ name, annotation string }{
		{"absent", ""}, {"corrupt", "{"}, {"foreign", `{"uid":"foreign","claim":"old-claim","planID":"plan-a","checksum":"checksum-a"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, client, _, publisher := newTestPublisher(t)
			authority := intentAuthority()
			setIntentLeader(t, ctx, client, &authority)
			require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
			require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"))
			leases := client.CoordinationV1().Leases(authority.Namespace)
			lease, err := leases.Get(ctx, intentLeaseName(&authority), metav1.GetOptions{})
			require.NoError(t, err)
			lease.Annotations[deploymentIntentAnnotation] = tc.annotation
			_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.Error(t, publisher.CheckDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"))
		})
	}
}

// The previous template's retained-state Lease is not this template's; it
// stays refused until garbage collection removes it with its owner.
func TestNewTemplateUIDCannotInheritPreviousDeployment(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	setIntentLeader(t, ctx, client, &authority)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "previous-plan", "previous-checksum"))
	fresh := authority
	fresh.Epoch, fresh.Claim, fresh.UID = 2, "fresh-claim", "recreated-template"
	setIntentLeader(t, ctx, client, &fresh)
	require.ErrorContains(t, publisher.ClaimDeploymentAuthority(ctx, &fresh), "foreign owner")

	require.NoError(t, client.CoordinationV1().Leases(authority.Namespace).Delete(ctx, intentLeaseName(&authority), metav1.DeleteOptions{}))
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &fresh))
	require.Error(t, publisher.CheckDeploymentIntent(ctx, &fresh, "previous-plan", "previous-checksum"))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &fresh, "new-plan", "new-checksum"))
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &fresh, "new-plan", "new-checksum"))
}

// legacyIntentOf is the annotation 0.5 and 0.6 wrote on the leader Lease.
func legacyIntentOf(uid, planID, checksum string) string {
	return `{"uid":"` + uid + `","epoch":1,"claim":"old-claim","planID":"` + planID + `","checksum":"` + checksum + `"}`
}

// After an upgrade from 0.6, the intent the previous leader left on the leader
// Lease must keep refusing older checkpoints: the first claim moves it.
func TestClaimMovesTheIntentAnEarlierReleaseLeftOnTheLeaderLease(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	previous := intentAuthority()
	setIntentLeader(t, ctx, client, &previous)
	setLegacyIntent(t, ctx, client, &previous, legacyIntentOf("template-uid", "plan-a", "checksum-a"))
	upgraded := successor(&previous)
	setIntentLeader(t, ctx, client, &upgraded)

	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &upgraded))

	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &upgraded, "plan-a", "checksum-a"))
	leader := leaderLeaseOf(t, ctx, client, &upgraded)
	require.NotContains(t, leader.Annotations, deploymentIntentAnnotation)
	require.Equal(t, "2", leader.Annotations[leaderEpochAnnotation])
	client.ClearActions()
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &upgraded))
	require.Zero(t, leaseWrites(client, upgraded.LeaseName), "the move happens once")
}

func TestClaimDropsTheLeaderLeaseIntentOfAnotherTemplate(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	setIntentLeader(t, ctx, client, &authority)
	setLegacyIntent(t, ctx, client, &authority, legacyIntentOf("previous-template", "plan-a", "checksum-a"))

	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))

	require.Error(t, publisher.CheckDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"))
	require.NotContains(t, leaderLeaseOf(t, ctx, client, &authority).Annotations, deploymentIntentAnnotation)
}

// A rollback to 0.6 writes the intent on the leader Lease again. That leader
// held the Lease after the last claim, so its intent is the newer one.
func TestLeaderLeaseIntentWrittenAfterARollbackSupersedes(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	upgraded := intentAuthority()
	setIntentLeader(t, ctx, client, &upgraded)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &upgraded))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &upgraded, "plan-a", "checksum-a"))
	rolledBack := successor(&upgraded)
	setIntentLeader(t, ctx, client, &rolledBack)
	setLegacyIntent(t, ctx, client, &rolledBack, legacyIntentOf("template-uid", "plan-b", "checksum-b"))
	reupgraded := successor(&rolledBack)
	setIntentLeader(t, ctx, client, &reupgraded)

	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &reupgraded))

	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &reupgraded, "plan-b", "checksum-b"))
	require.Error(t, publisher.CheckDeploymentIntent(ctx, &reupgraded, "plan-a", "checksum-a"))
}

func TestUnreadableLeaderLeaseIntentRefusesEveryCheckpoint(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	authority := intentAuthority()
	setIntentLeader(t, ctx, client, &authority)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &authority))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &authority, "plan-a", "checksum-a"))
	fresh := successor(&authority)
	setIntentLeader(t, ctx, client, &fresh)
	setLegacyIntent(t, ctx, client, &fresh, "{")

	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &fresh))

	require.Error(t, publisher.CheckDeploymentIntent(ctx, &fresh, "plan-a", "checksum-a"))
	require.NotContains(t, leaderLeaseOf(t, ctx, client, &fresh).Annotations, deploymentIntentAnnotation)
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &fresh, "plan-b", "checksum-b"))
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &fresh, "plan-b", "checksum-b"))
}

func TestClaimOnlyAfterRollbackPreservesRetainedIntent(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	upgraded := intentAuthority()
	setIntentLeader(t, ctx, client, &upgraded)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &upgraded))
	require.NoError(t, publisher.RecordDeploymentIntent(ctx, &upgraded, "plan-a", "checksum-a"))
	rolledBack := successor(&upgraded)
	setIntentLeader(t, ctx, client, &rolledBack)
	setLegacyIntent(t, ctx, client, &rolledBack, legacyIntentOf("template-uid", "", ""))
	reupgraded := successor(&rolledBack)
	setIntentLeader(t, ctx, client, &reupgraded)

	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &reupgraded))

	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &reupgraded, "plan-a", "checksum-a"))
	require.NotContains(t, leaderLeaseOf(t, ctx, client, &reupgraded).Annotations, deploymentIntentAnnotation)
}

// The intent reaches the retained-state Lease before it leaves the leader
// Lease, so a claim cut short between the two loses nothing.
func TestInterruptedMoveKeepsTheIntentForTheNextClaim(t *testing.T) {
	ctx, client, _, publisher := newTestPublisher(t)
	previous := intentAuthority()
	setIntentLeader(t, ctx, client, &previous)
	setLegacyIntent(t, ctx, client, &previous, legacyIntentOf("template-uid", "plan-a", "checksum-a"))
	upgraded := successor(&previous)
	setIntentLeader(t, ctx, client, &upgraded)
	interrupted := false
	client.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if interrupted || action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease).Name != upgraded.LeaseName {
			return false, nil, nil
		}
		interrupted = true
		return true, nil, apierrors.NewServiceUnavailable("apiserver restarting")
	})

	require.Error(t, publisher.ClaimDeploymentAuthority(ctx, &upgraded))
	require.Contains(t, leaderLeaseOf(t, ctx, client, &upgraded).Annotations, deploymentIntentAnnotation)

	restarted := successor(&upgraded)
	setIntentLeader(t, ctx, client, &restarted)
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, &restarted))
	require.NoError(t, publisher.CheckDeploymentIntent(ctx, &restarted, "plan-a", "checksum-a"))
	require.NotContains(t, leaderLeaseOf(t, ctx, client, &restarted).Annotations, deploymentIntentAnnotation)
}
