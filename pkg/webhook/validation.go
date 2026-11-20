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
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (s *Server) validateRequest(parent context.Context, request *admissionv1.AdmissionRequest, fromPeer bool) *admissionv1.AdmissionResponse {
	gvk := getGVK(request)

	s.mu.RLock()
	generation := s.generation
	generation.inFlight.Add(1)
	validator, exists := generation.validators[gvk]
	onUnregistered := generation.onUnregisteredGVK
	s.mu.RUnlock()
	defer generation.inFlight.Done()

	if !exists {
		if onUnregistered != nil {
			onUnregistered(gvk)
		}
		return deniedResponse(
			fmt.Sprintf("no validator registered for %s; retry after controller initialization", gvk),
			http.StatusServiceUnavailable,
		)
	}

	// DELETE requests may carry only OldObject. The controller's structural
	// gate decides whether the operation has enough object data to validate.
	var obj *unstructured.Unstructured
	if len(request.Object.Raw) > 0 {
		obj = &unstructured.Unstructured{}
		if err := json.Unmarshal(request.Object.Raw, obj); err != nil {
			return deniedResponse(fmt.Sprintf("parsing object: %v", err), http.StatusBadRequest)
		}
	}

	var oldObj *unstructured.Unstructured
	if len(request.OldObject.Raw) > 0 {
		oldObj = &unstructured.Unstructured{}
		if err := json.Unmarshal(request.OldObject.Raw, oldObj); err != nil {
			return deniedResponse(fmt.Sprintf("parsing old object: %v", err), http.StatusBadRequest)
		}
	}

	metadataObject := obj
	if metadataObject == nil {
		metadataObject = oldObj
	}
	namespace, name := extractMetadata(metadataObject)

	ctx := &ValidationContext{
		Context:   parent,
		Request:   request,
		FromPeer:  fromPeer,
		Object:    obj,
		OldObject: oldObj,
		Operation: string(request.Operation),
		Namespace: namespace,
		Name:      name,
		UID:       string(request.UID),
		UserInfo:  request.UserInfo,
	}

	allowed, reason, warnings, err := validator(ctx)

	if apierrors.IsServiceUnavailable(err) {
		resp := deniedResponse(err.Error(), http.StatusServiceUnavailable)
		resp.Result.Reason = metav1.StatusReasonServiceUnavailable
		resp.Warnings = warnings
		return resp
	}
	if err != nil {
		return deniedResponse(fmt.Sprintf("validation error: %v", err), http.StatusInternalServerError)
	}

	if !allowed {
		resp := deniedResponse(reason, http.StatusForbidden)
		resp.Warnings = warnings
		return resp
	}

	return &admissionv1.AdmissionResponse{
		Allowed:  true,
		Warnings: warnings,
	}
}

func deniedResponse(message string, code int32) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Message: message,
			Code:    code,
		},
	}
}

// extractMetadata extracts namespace and name from a resource object.
//
// Returns empty strings if metadata is not found.
func extractMetadata(obj *unstructured.Unstructured) (namespace, name string) {
	if obj == nil {
		return "", ""
	}

	namespace = obj.GetNamespace()
	name = obj.GetName()

	return namespace, name
}

// getGVK returns the GVK string for an AdmissionRequest.
//
// Format: "group/version.Kind" or "version.Kind" for core types.
func getGVK(request *admissionv1.AdmissionRequest) string {
	if request.Kind.Group == "" {
		return fmt.Sprintf("%s.%s", request.Kind.Version, request.Kind.Kind)
	}
	return fmt.Sprintf("%s/%s.%s", request.Kind.Group, request.Kind.Version, request.Kind.Kind)
}
