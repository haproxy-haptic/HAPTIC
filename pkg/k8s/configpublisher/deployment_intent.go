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
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
)

const kindTemplateConfig = "HAProxyTemplateConfig"

// Releases 0.5 and 0.6 stored this annotation on the leader Lease (ADR-0033).
const deploymentIntentAnnotation = "haproxy-haptic.org/deployment-intent"

const leaderEpochAnnotation = "haptic.io/leader-epoch"

// DeploymentAuthority binds one writer to the template UID and leadership term.
type DeploymentAuthority struct {
	Namespace  string
	Name       string
	UID        types.UID
	Epoch      uint64
	Claim      string
	Standalone bool
	LeaseName  string
	Identity   string
}

type deploymentIntent struct {
	UID      types.UID `json:"uid"`
	Epoch    uint64    `json:"epoch"`
	Claim    string    `json:"claim"`
	Holder   string    `json:"holder,omitempty"`
	PlanID   string    `json:"planID,omitempty"`
	Checksum string    `json:"checksum,omitempty"`
}

// ErrDeploymentAuthorityLost reports that the leader Lease or the intent names
// a newer term.
var ErrDeploymentAuthorityLost = errors.New("deployment authority belongs to a newer controller term")

// IntentEpochAheadError means a newer term claimed the intent or the leader epoch regressed.
type IntentEpochAheadError struct {
	Epoch uint64
}

func (e *IntentEpochAheadError) Error() string {
	return fmt.Sprintf("the deployment intent was claimed at leader epoch %d, above this term's", e.Epoch)
}

// ClaimDeploymentAuthority fences previous writers without changing the last
// intent, and moves an intent a 0.5 or 0.6 leader left on the leader Lease.
func (p *Publisher) ClaimDeploymentAuthority(ctx context.Context, authority *DeploymentAuthority) error {
	if authority.Claim == "" {
		return errors.New("deployment authority has no claim")
	}
	return retry.OnError(retry.DefaultRetry, retriableWrite, func() error {
		var leader *coordinationv1.Lease
		if !authority.Standalone {
			var err error
			if leader, err = p.leaderLease(ctx, authority); err != nil {
				return err
			}
		}
		legacy, hasLegacy := p.legacyDeploymentIntent(leader, authority.UID)
		if err := p.writeDeploymentIntent(ctx, authority, func(status *deploymentIntent) error {
			if status.UID != authority.UID {
				*status = deploymentIntent{UID: authority.UID}
			}
			if !authority.Standalone && status.Epoch > authority.Epoch {
				return &IntentEpochAheadError{Epoch: status.Epoch}
			}
			if legacy != nil {
				status.PlanID, status.Checksum = legacy.PlanID, legacy.Checksum
			}
			status.Epoch, status.Claim, status.Holder = authority.Epoch, authority.Claim, authority.Identity
			return nil
		}); err != nil {
			return err
		}
		if !hasLegacy {
			return nil
		}
		// Only once the retained-state Lease holds it, and by compare-and-swap on
		// the Lease whose holder and epoch were checked above.
		delete(leader.Annotations, deploymentIntentAnnotation)
		_, err := p.k8sClient.CoordinationV1().Leases(authority.Namespace).Update(ctx, leader, metav1.UpdateOptions{})
		return err
	})
}

// RecordDeploymentIntent must succeed before the first apply for this output (ADR-0033).
func (p *Publisher) RecordDeploymentIntent(ctx context.Context, authority *DeploymentAuthority, planID, checksum string) error {
	if planID == "" || checksum == "" {
		return errors.New("deployment intent has no output identity")
	}
	return retry.OnError(retry.DefaultRetry, retriableWrite, func() error {
		if err := p.confirmLeadership(ctx, authority); err != nil {
			return err
		}
		if err := p.writeDeploymentIntent(ctx, authority, func(status *deploymentIntent) error {
			if status.UID != authority.UID || status.Claim != authority.Claim {
				return errors.New("deployment authority changed before apply")
			}
			status.Epoch, status.PlanID, status.Checksum = authority.Epoch, planID, checksum
			return nil
		}); err != nil {
			return err
		}
		return p.confirmLeadership(ctx, authority)
	})
}

// CheckDeploymentIntent refuses a checkpoint superseded before or during recovery.
func (p *Publisher) CheckDeploymentIntent(ctx context.Context, authority *DeploymentAuthority, planID, checksum string) error {
	status, err := p.claimedDeploymentIntent(ctx, authority)
	if err != nil {
		return err
	}
	if status.PlanID != planID || status.Checksum != checksum {
		return errors.New("a newer deployment intent supersedes the retained configuration")
	}
	return nil
}

// CheckDeploymentAuthority confirms that no newer term has claimed the Lease.
func (p *Publisher) CheckDeploymentAuthority(ctx context.Context, authority *DeploymentAuthority) error {
	_, err := p.claimedDeploymentIntent(ctx, authority)
	return err
}

// claimedDeploymentIntent reads the intent before the leader Lease: a newer term
// bumps the epoch before it claims, so the second read sees any claim the first missed.
func (p *Publisher) claimedDeploymentIntent(ctx context.Context, authority *DeploymentAuthority) (deploymentIntent, error) {
	lease, err := p.intentLease(ctx, authority, false)
	if err != nil {
		return deploymentIntent{}, err
	}
	status, err := readDeploymentIntent(lease, authority)
	if err != nil {
		return status, err
	}
	if status.UID != authority.UID || status.Claim != authority.Claim {
		return status, ErrDeploymentAuthorityLost
	}
	return status, p.confirmLeadership(ctx, authority)
}

// writeDeploymentIntent is one compare-and-swap on the retained-state Lease.
func (p *Publisher) writeDeploymentIntent(ctx context.Context, authority *DeploymentAuthority, update func(*deploymentIntent) error) error {
	lease, err := p.intentLease(ctx, authority, true)
	if err != nil {
		return err
	}
	status, err := readDeploymentIntent(lease, authority)
	if err != nil {
		return err
	}
	if err := update(&status); err != nil {
		return err
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		return err
	}
	if lease.Annotations[deploymentIntentAnnotation] == string(encoded) {
		return nil
	}
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	lease.Annotations[deploymentIntentAnnotation] = string(encoded)
	_, err = p.k8sClient.CoordinationV1().Leases(authority.Namespace).Update(ctx, lease, metav1.UpdateOptions{})
	return err
}

// intentLease is the template's retained-state Lease. Only the deployer writes
// it, so the elector stays the only writer of the leader Lease.
func (p *Publisher) intentLease(ctx context.Context, authority *DeploymentAuthority, create bool) (*coordinationv1.Lease, error) {
	name := stableResourceName(authority.Name, "-retained-state", authority.Name)
	client := p.k8sClient.CoordinationV1().Leases(authority.Namespace)
	lease, err := client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && create {
		lease, err = client.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: authority.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: apiVersionV1Alpha1, Kind: kindTemplateConfig, Name: authority.Name, UID: authority.UID, Controller: ptr.To(true)}},
		}}, metav1.CreateOptions{})
	}
	if err != nil {
		return nil, err
	}
	if !ownedByTemplate(lease, authority.Name, authority.UID) {
		return nil, errors.New("retained-state Lease has a foreign owner")
	}
	return lease, nil
}

// leaderLease reads the leader-election Lease, provided it names this term.
func (p *Publisher) leaderLease(ctx context.Context, authority *DeploymentAuthority) (*coordinationv1.Lease, error) {
	lease, err := p.k8sClient.CoordinationV1().Leases(authority.Namespace).Get(ctx, authority.LeaseName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	epoch, err := strconv.ParseUint(lease.Annotations[leaderEpochAnnotation], 10, 64)
	if err != nil || epoch != authority.Epoch || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != authority.Identity {
		return nil, ErrDeploymentAuthorityLost
	}
	return lease, nil
}

func (p *Publisher) confirmLeadership(ctx context.Context, authority *DeploymentAuthority) error {
	if authority.Standalone {
		return nil
	}
	_, err := p.leaderLease(ctx, authority)
	return err
}

// legacyDeploymentIntent is the intent a 0.5 or 0.6 leader left on the leader
// Lease, which supersedes the retained-state Lease's (ADR-0033). Needed while an
// upgrade or rollback can involve 0.6, as TestUpgradedLeaderKeepsTheDeploymentIntentOfAnEarlierRelease pins.
func (p *Publisher) legacyDeploymentIntent(leader *coordinationv1.Lease, uid types.UID) (adopt *deploymentIntent, present bool) {
	if leader == nil {
		return nil, false
	}
	encoded, present := leader.Annotations[deploymentIntentAnnotation]
	if !present {
		return nil, false
	}
	var legacy deploymentIntent
	if err := json.Unmarshal([]byte(encoded), &legacy); err != nil {
		p.logger.Warn("Discarding an unreadable deployment intent from the leader Lease; retained recovery stays unavailable until the next deployment",
			"lease", leader.Name, "error", err)
		return &deploymentIntent{}, true
	}
	if legacy.UID != uid {
		return nil, true
	}
	if legacy.PlanID == "" && legacy.Checksum == "" && legacy.Claim != "" && legacy.Epoch > 0 {
		return nil, true
	}
	return &legacy, true
}

func readDeploymentIntent(lease *coordinationv1.Lease, authority *DeploymentAuthority) (deploymentIntent, error) {
	status := deploymentIntent{UID: authority.UID}
	if encoded := lease.Annotations[deploymentIntentAnnotation]; encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &status); err != nil {
			return status, fmt.Errorf("decoding deployment intent: %w", err)
		}
	}
	if authority.UID == "" {
		return status, errors.New("deployment intent belongs to another template UID")
	}
	return status, nil
}
