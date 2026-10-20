// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package configpublisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
)

type jsonPatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

const (
	jsonPatchTest       = "test"
	jsonPatchAdd        = "add"
	ownerReferencesPath = "/metadata/ownerReferences"
)

func validateRuntimePublication(expected, current *haproxyv1alpha1.HAProxyCfg) error {
	if expected.UID == "" {
		return errors.New("runtime publication has no UID")
	}
	if current.UID != expected.UID ||
		!apiequality.Semantic.DeepEqual(current.Spec, expected.Spec) ||
		!apiequality.Semantic.DeepEqual(current.OwnerReferences, expected.OwnerReferences) ||
		current.Annotations[AuxiliarySetIDAnnotationKey] != expected.Annotations[AuxiliarySetIDAnnotationKey] {
		return fmt.Errorf("HAProxyCfg %s/%s publication was superseded; publish the current output", expected.Namespace, expected.Name)
	}
	return nil
}

func (p *Publisher) patchRuntimeConfigStatusField(
	ctx context.Context,
	expected, current *haproxyv1alpha1.HAProxyCfg,
	field string,
	value any,
) error {
	if err := validateRuntimePublication(expected, current); err != nil {
		return err
	}
	client := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs(expected.Namespace)
	if apiequality.Semantic.DeepEqual(current.Status, haproxyv1alpha1.HAProxyCfgStatus{}) {
		// Merge an empty object so a missing status exists without replacing concurrent fields.
		if _, err := client.Patch(ctx, expected.Name, types.MergePatchType, []byte(`{"status":{}}`), metav1.PatchOptions{}, statusSubresource); err != nil {
			return fmt.Errorf("initializing runtime config status: %w", err)
		}
	}
	annotationPath := "/metadata/annotations/" + strings.ReplaceAll(AuxiliarySetIDAnnotationKey, "/", "~1")
	operations := []jsonPatchOperation{
		{Op: jsonPatchTest, Path: "/metadata/uid", Value: expected.UID},
		{Op: jsonPatchTest, Path: ownerReferencesPath, Value: expected.OwnerReferences},
		{Op: jsonPatchTest, Path: "/spec", Value: expected.Spec},
		{Op: jsonPatchTest, Path: annotationPath, Value: expected.Annotations[AuxiliarySetIDAnnotationKey]},
		{Op: jsonPatchAdd, Path: "/status/" + field, Value: value},
	}
	payload, err := json.Marshal(operations)
	if err != nil {
		return fmt.Errorf("encoding runtime config status patch: %w", err)
	}
	if _, err := client.Patch(ctx, expected.Name, types.JSONPatchType, payload, metav1.PatchOptions{}, statusSubresource); err != nil {
		return fmt.Errorf("patching runtime config status %s: %w", field, err)
	}
	return nil
}
