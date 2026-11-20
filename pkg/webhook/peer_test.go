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
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestForwardAdmissionRequiresMatchingIdentityAndVerdict(t *testing.T) {
	sender := newTestServer(t, &ServerConfig{})
	cert, err := sender.getCertificate(nil)
	require.NoError(t, err)
	_, request := configMapAdmissionRequest(t)
	review, err := readAdmissionReview(request.Body)
	require.NoError(t, err)
	dry := true
	review.Request.DryRun = &dry
	handler := http.NewServeMux()
	handler.HandleFunc("/validate/peer", sender.handlePeer)
	sender.RegisterValidator("v1.ConfigMap", func(v *ValidationContext) (bool, string, []string, error) {
		if !v.FromPeer || v.Request.DryRun == nil || !*v.Request.DryRun {
			t.Error("forwarded review lost peer or dry-run metadata")
		}
		return false, "content denial", []string{"warning"}, nil
	})
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*cert}, ClientAuth: tls.RequestClientCert}
	server.StartTLS()
	defer server.Close()
	addr := server.Listener.Addr().String()
	response, err := sender.ForwardAdmission(t.Context(), addr, "localhost", review.Request)
	require.NoError(t, err)
	require.Equal(t, review.Request.UID, response.UID)
	require.Equal(t, "content denial", response.Result.Message)
	require.Equal(t, []string{"warning"}, response.Warnings)

	unauthenticated := httptest.NewRecorder()
	sender.handlePeer(unauthenticated, request)
	require.Equal(t, http.StatusForbidden, unauthenticated.Code)
	other := newTestServer(t, &ServerConfig{})
	_, err = other.ForwardAdmission(t.Context(), addr, "localhost", review.Request)
	require.Error(t, err)
	_, err = sender.ForwardAdmission(t.Context(), addr, "other-release.test", review.Request)
	require.Error(t, err)
}

func TestForwardAdmissionRejectsUnrelatedUID(t *testing.T) {
	sender := newTestServer(t, &ServerConfig{})
	cert, err := sender.getCertificate(nil)
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(admissionv1.AdmissionReview{TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"}, Response: &admissionv1.AdmissionResponse{UID: "other", Allowed: true}})
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*cert}}
	server.StartTLS()
	defer server.Close()
	_, err = sender.ForwardAdmission(t.Context(), server.Listener.Addr().String(), "localhost", &admissionv1.AdmissionRequest{UID: "original"})
	require.ErrorContains(t, err, "does not match")
}
