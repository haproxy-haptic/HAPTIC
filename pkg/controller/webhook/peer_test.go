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
	"sync/atomic"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/webhook"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type localAdmission func(context.Context) (bool, string, []string, error)

func (f localAdmission) ValidateAdmission(ctx context.Context, _, _, _ string, _, _ any, _ string) (allowed bool, reason string, warnings []string, failure error) {
	return f(ctx)
}

type peerDecisionCase struct {
	name                                     string
	pending, refused, forwarded, peerAllowed bool
	want                                     string
	calls                                    int32
}

func TestAdmissionPeerDecision(t *testing.T) {
	for _, tc := range []peerDecisionCase{
		{"local content rejection", false, false, false, true, "local deny", 0},
		{"refusal starts one peer", false, true, false, true, "", 1},
		{"peer content rejection", false, true, false, false, "peer deny", 1},
		{"forwarded refusal terminates", false, true, true, true, "unavailable", 0},
		{"slow rejection takes precedence", true, false, false, true, "local deny", 1},
		{"slow refusal uses peer", true, true, false, true, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t)
		})
	}
}

func (tc peerDecisionCase) check(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var calls atomic.Int32
	started := make(chan struct{})
	req := &admissionv1.AdmissionRequest{UID: "original-uid"}
	c := New(testutil.NewTestLogger(), &Config{ResourceAdmissionTimeout: 40 * time.Millisecond, Peer: func(_ context.Context, r *admissionv1.AdmissionRequest) (*admissionv1.AdmissionResponse, error) {
		calls.Add(1)
		if r != req {
			t.Error("did not forward original request")
		}
		close(started)
		if tc.peerAllowed {
			return &admissionv1.AdmissionResponse{Allowed: true}, nil
		}
		return &admissionv1.AdmissionResponse{Result: &metav1.Status{Message: "peer deny"}}, nil
	}}, nil, nil)
	c.dryRunValidator = localAdmission(func(ctx context.Context) (bool, string, []string, error) {
		stop := WatchExternalValidation(ctx)
		defer stop()
		if tc.pending {
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("peer did not overlap external validation")
			}
		}
		if tc.refused {
			return false, "unavailable", nil, apierrors.NewServiceUnavailable("unavailable")
		}
		return false, "local deny", nil, nil
	})
	allowed, reason, _, err := c.validateWithPeer(ctx, "v1.ConfigMap", &webhook.ValidationContext{Request: req, FromPeer: tc.forwarded})
	require.Equal(t, tc.want, reason)
	require.Equal(t, tc.want == "", allowed)
	require.Equal(t, tc.want == "unavailable", apierrors.IsServiceUnavailable(err))
	require.Equal(t, tc.calls, calls.Load())
}
