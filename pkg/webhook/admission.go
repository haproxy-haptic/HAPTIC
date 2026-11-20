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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
)

// An UPDATE carries both objects plus admission metadata.
const maxAdmissionReviewBytes = 16 << 20

func (s *Server) handleValidation(w http.ResponseWriter, r *http.Request) {
	draining := s.activity.start()
	defer s.activity.finish()
	if draining {
		w.Header().Set("Connection", "close")
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAdmissionReviewBytes)
	defer r.Body.Close()
	review, err := readAdmissionReview(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}

	response := s.validateRequest(r.Context(), review.Request, r.URL.Path == s.config.Path+"/peer")
	response.UID = review.Request.UID
	answer := admissionv1.AdmissionReview{TypeMeta: review.TypeMeta, Response: response}
	responseBytes, err := json.Marshal(answer)
	if err != nil {
		http.Error(w, fmt.Sprintf("encoding response: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBytes)
}

func readAdmissionReview(body io.Reader) (*admissionv1.AdmissionReview, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("reading admission request: %w", err)
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(raw, &review); err != nil {
		return nil, fmt.Errorf("decoding admission request: %w", err)
	}
	if review.APIVersion != admissionv1.SchemeGroupVersion.String() || review.Kind != "AdmissionReview" {
		return nil, errors.New("expected an admission.k8s.io/v1 AdmissionReview")
	}
	if review.Request == nil {
		return nil, errors.New("AdmissionReview.request is required")
	}
	if review.Request.UID == "" {
		return nil, errors.New("AdmissionReview.request.uid is required")
	}
	return &review, nil
}
