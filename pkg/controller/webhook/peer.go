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

package webhook

import (
	"context"
	"sync"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/webhook"
	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type peerValidation func(context.Context, *admissionv1.AdmissionRequest) (*admissionv1.AdmissionResponse, error)
type externalWaitKey struct{}
type peerAttempt struct {
	start func()
	delay time.Duration
}

// WatchExternalValidation schedules a single speculative validation only while
// the local admission is waiting on the external validator. The caller stops
// the timer as soon as that phase finishes.
func WatchExternalValidation(ctx context.Context) func() {
	attempt, ok := ctx.Value(externalWaitKey{}).(*peerAttempt)
	if !ok {
		return func() {}
	}
	timer := time.AfterFunc(attempt.delay, attempt.start)
	return func() { timer.Stop() }
}

func (c *Component) validateWithPeer(ctx context.Context, gvk string, v *webhook.ValidationContext) (allowed bool, reason string, warnings []string, failure error) {
	var reply chan *admissionv1.AdmissionResponse
	var start func()
	if c.config.Peer != nil && !v.FromPeer && v.Request != nil {
		reply = make(chan *admissionv1.AdmissionResponse, 1)
		parent := ctx
		start = sync.OnceFunc(func() {
			go func() {
				r, err := c.config.Peer(parent, v.Request)
				if err != nil {
					c.logger.Debug("Admission peer unavailable", "uid", v.UID, "error", err)
					r = nil
				}
				reply <- r
			}()
		})
		ctx = context.WithValue(ctx, externalWaitKey{}, &peerAttempt{start: start, delay: min(time.Second, c.config.ResourceAdmissionTimeout/4)})
	}
	allowed, reason, warnings, failure = c.dryRunValidator.ValidateAdmission(ctx, gvk, v.Namespace, v.Name, v.Object, v.OldObject, v.Operation)
	if start == nil || !apierrors.IsServiceUnavailable(failure) {
		return allowed, reason, warnings, failure
	}
	// Even if the peer responds first, only an inconclusive local verdict
	// permits substitution. Preserve every conclusive local content decision.
	start()
	var peer *admissionv1.AdmissionResponse
	select {
	case peer = <-reply:
	case <-ctx.Done():
		select {
		case peer = <-reply:
		default:
		}
	}
	if peer == nil {
		return allowed, reason, warnings, failure
	}
	reason = ""
	if peer.Result != nil {
		reason = peer.Result.Message
	}
	c.logger.Info("Admission completed by peer", "uid", v.UID, "allowed", peer.Allowed)
	return peer.Allowed, reason, peer.Warnings, nil
}
