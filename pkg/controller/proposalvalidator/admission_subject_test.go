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

package proposalvalidator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ktypes "k8s.io/apimachinery/pkg/types"

	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

func unstructuredObj(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata":   map[string]any{"namespace": ns, "name": name},
	}}
}

// admissionSubjectOpts derives the render-context admission subject from
// webhook overlays: exactly one object in one store yields the subject;
// anything else (bulk overlays, HTTP-only proposals) yields none, so
// route-scoped template checks degrade to warn-and-fail-closed instead of
// denying an unattributable admission.
func TestAdmissionSubjectOpts(t *testing.T) {
	t.Run("single modification yields subject option", func(t *testing.T) {
		opts := admissionSubjectOpts(map[string]*stores.StoreOverlay{
			"ingresses": stores.NewStoreOverlayForUpdate(unstructuredObj("team-a", "app")),
		})
		assert.Len(t, opts, 1)
	})

	t.Run("single deletion yields subject option", func(t *testing.T) {
		opts := admissionSubjectOpts(map[string]*stores.StoreOverlay{
			"ingresses": {Deletions: []ktypes.NamespacedName{{Namespace: "team-a", Name: "app"}}},
		})
		assert.Len(t, opts, 1)
	})

	t.Run("no overlays yields no subject", func(t *testing.T) {
		assert.Nil(t, admissionSubjectOpts(nil))
		assert.Nil(t, admissionSubjectOpts(map[string]*stores.StoreOverlay{}))
		assert.Nil(t, admissionSubjectOpts(map[string]*stores.StoreOverlay{"ingresses": nil}))
	})

	t.Run("multiple objects yield no subject", func(t *testing.T) {
		opts := admissionSubjectOpts(map[string]*stores.StoreOverlay{
			"ingresses": {Deletions: []ktypes.NamespacedName{
				{Namespace: "a", Name: "x"},
				{Namespace: "b", Name: "y"},
			}},
		})
		assert.Nil(t, opts)
	})
}

type pendingHTTPSubjectOverlay struct{ stores.HTTPContentOverlay }

func (pendingHTTPSubjectOverlay) IsEmpty() bool { return false }
func (pendingHTTPSubjectOverlay) PendingURLs() []string {
	return []string{"https://keys.example.com/jwks"}
}

func TestHTTPProposalIdentifiesChangedContentAndPreservesValidation(t *testing.T) {
	for _, tc := range []struct {
		name, template string
		valid          bool
		rejectOutput   bool
	}{
		{"unrelated rejected resource remains closed", `{% if len(admissionSubject) == 0 %}{{ fail("unattributed proposal") }}{% end %}{% if len(admissionSubject["httpURLs"].([]string)) != 1 %}{{ fail("missing URL") }}{% end %}global
    maxconn 2000
defaults
    mode http
    timeout connect 5s
    timeout client 30s
    timeout server 30s
frontend closed
    bind 127.0.0.1:8080
    http-request deny deny_status 503
`, true, false},
		{"invalid fetched content still rejected", `{{ fail("invalid fetched content") }}`, false, false},
		{"invalid output still rejected", `global`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance := createTestPipeline(t, tc.template)
			if tc.rejectOutput {
				instance = createTestPipelineWithOutputValidator(t, tc.template, &cancelingRejectingOutputValidator{})
			}
			service := NewService(&ServiceConfig{Pipeline: instance, BaseStoreProvider: stores.NewRealStoreProvider(map[string]stores.Store{})})
			result, err := service.validateProposal(t.Context(), nil, pendingHTTPSubjectOverlay{})
			if !tc.valid && !tc.rejectOutput {
				require.ErrorContains(t, err, "invalid fetched content")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.valid, result.Valid)
		})
	}
}
