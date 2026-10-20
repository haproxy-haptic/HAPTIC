// Copyright 2026 Philipp Hossner
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
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

// Condition types the render gate writes on HAProxyCfg.
const (
	// ConditionConfigValidated reports the controller's own `haproxy -c`
	// verdict on the published render.
	ConditionConfigValidated = "ConfigValidated"

	// ConditionConfigPinned reports that no new render reaches the fleet: the
	// gate refused a render it was already holding, so the pods keep serving
	// the last config HAProxy accepted.
	ConditionConfigPinned = "ConfigPinned"
)

// Condition reasons (CamelCase per Kubernetes convention).
const (
	reasonHAProxyAccepted = "HAProxyAccepted"
	reasonHAProxyRefused  = "HAProxyRefused"
	reasonGateUnavailable = "GateUnavailable"
	reasonGateHolding     = "RenderGateHolding"
	reasonGateOpen        = "RenderGateOpen"
)

// GateVerdict is one render gate verdict to write onto an HAProxyCfg.
type GateVerdict struct {
	Namespace string
	Name      string

	// PlanID identifies the render for log correlation and the reapply cache.
	PlanID string

	// Accepted is HAProxy's pass.
	Accepted bool

	// Refused separates HAProxy's own verdict from a gate that could not run.
	Refused bool

	// Pinned reports that renders are being held after a second refusal.
	Pinned bool

	// Message is HAProxy's own words, or why the gate could not run.
	Message string
}

// ApplyGateVerdict writes the ConfigValidated and ConfigPinned conditions.
// A HAProxyCfg that does not exist yet is not an error: the first publish
// creates it, and the next verdict lands on it.
//
// The read-modify-write retries on conflict, like every other status writer
// here: this object's status is written by the publish path and by every pod's
// deployment report, so a 409 is ordinary — and losing to one would drop
// HAProxy's own message, which is the operator's only pointer at what to fix.
func (p *Publisher) ApplyGateVerdict(ctx context.Context, verdict *GateVerdict) error {
	if p.skippableVerdict(verdict) {
		return nil
	}
	client := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs(verdict.Namespace)
	targetFound := true
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := client.Get(ctx, verdict.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				targetFound = false
				return nil
			}
			return fmt.Errorf("getting runtime config for the gate verdict: %w", err)
		}
		targetFound = true

		updated := current.DeepCopy()
		meta.SetStatusCondition(&updated.Status.Conditions, validatedCondition(verdict))
		meta.SetStatusCondition(&updated.Status.Conditions, pinnedCondition(verdict))
		if conditionsEqual(current.Status.Conditions, updated.Status.Conditions) {
			return nil
		}

		if _, err := client.UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
			// Returned unwrapped so RetryOnConflict can recognise a 409.
			return err
		}
		return nil
	})
	if err != nil {
		p.forgetVerdict(verdict)
		return err
	}
	// A missing target is not recorded: the first publish creates it, and the
	// next (possibly identical) verdict must land on it rather than be skipped.
	if targetFound {
		p.recordVerdict(verdict)
	}
	return nil
}

// verdictKey identifies the HAProxyCfg a gate verdict lands on.
type verdictKey struct {
	namespace string
	name      string
}

// appliedVerdict is the last verdict ApplyGateVerdict wrote for a key. The
// render gate re-emits its verdict on every render, so at a steady state the
// same verdict arrives once per render; skipping the identical re-apply elides
// the read-modify-write's GET the same way skippableResult elides the publish
// sweep, with the interval expiry as the authoritative self-heal.
type appliedVerdict struct {
	verdict GateVerdict
	at      time.Time
}

func (p *Publisher) skippableVerdict(verdict *GateVerdict) bool {
	p.publishedMu.Lock()
	defer p.publishedMu.Unlock()
	if p.republishInterval <= 0 {
		return false
	}
	state, ok := p.appliedVerdicts[verdictKey{namespace: verdict.Namespace, name: verdict.Name}]
	return ok && time.Since(state.at) < p.republishInterval && state.verdict == *verdict
}

func (p *Publisher) recordVerdict(verdict *GateVerdict) {
	p.publishedMu.Lock()
	defer p.publishedMu.Unlock()
	if p.republishInterval <= 0 {
		return
	}
	if p.appliedVerdicts == nil {
		p.appliedVerdicts = make(map[verdictKey]appliedVerdict)
	}
	p.appliedVerdicts[verdictKey{namespace: verdict.Namespace, name: verdict.Name}] = appliedVerdict{
		verdict: *verdict,
		at:      time.Now(),
	}
}

// forgetVerdict voids a key's state after a failed write: the failure may have
// landed partially, so the recorded claim about the live conditions no longer
// holds.
func (p *Publisher) forgetVerdict(verdict *GateVerdict) {
	p.publishedMu.Lock()
	defer p.publishedMu.Unlock()
	delete(p.appliedVerdicts, verdictKey{namespace: verdict.Namespace, name: verdict.Name})
}

func validatedCondition(verdict *GateVerdict) metav1.Condition {
	if verdict.Accepted {
		return metav1.Condition{
			Type:    ConditionConfigValidated,
			Status:  metav1.ConditionTrue,
			Reason:  reasonHAProxyAccepted,
			Message: "HAProxy loads this configuration",
		}
	}
	reason := reasonGateUnavailable
	if verdict.Refused {
		reason = reasonHAProxyRefused
	}
	return metav1.Condition{
		Type:    ConditionConfigValidated,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: truncateConditionMessage(verdict.Message),
	}
}

func pinnedCondition(verdict *GateVerdict) metav1.Condition {
	if !verdict.Pinned {
		return metav1.Condition{
			Type:    ConditionConfigPinned,
			Status:  metav1.ConditionFalse,
			Reason:  reasonGateOpen,
			Message: "Renders reach the HAProxy pods",
		}
	}
	return metav1.Condition{
		Type:   ConditionConfigPinned,
		Status: metav1.ConditionTrue,
		Reason: reasonGateHolding,
		Message: "HAProxy refused the last two renders, so the pods keep serving the last accepted " +
			"configuration. Fix the input the ConfigValidated condition names.",
	}
}

// maxConditionMessageBytes is the apiserver's limit on a condition message.
// A longer message makes the status write fail, which would hide the very
// failure it describes.
const maxConditionMessageBytes = 32768

func truncateConditionMessage(message string) string {
	if message == "" {
		return "HAProxy refused this configuration"
	}
	if len(message) <= maxConditionMessageBytes {
		return message
	}
	return message[:maxConditionMessageBytes]
}

// conditionsEqual compares two condition sets ignoring transition timestamps,
// so an unchanged verdict does not rewrite the status on every render.
func conditionsEqual(before, after []metav1.Condition) bool {
	strip := func(conditions []metav1.Condition) []metav1.Condition {
		out := make([]metav1.Condition, len(conditions))
		copy(out, conditions)
		for i := range out {
			out[i].LastTransitionTime = metav1.Time{}
		}
		return out
	}
	return apiequality.Semantic.DeepEqual(strip(before), strip(after))
}
