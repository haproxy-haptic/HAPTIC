// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package configpublisher

import (
	"crypto/sha256"
	"fmt"
	"strings"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	contentSetPrefix            = "content-sha256:"
	auxiliaryClaimAnnotationKey = "haproxy-haptic.org/auxiliary-claim"
	// AuxiliaryPathAnnotationKey exposes a Secret's path without watching its data.
	AuxiliaryPathAnnotationKey = "haproxy-haptic.org/auxiliary-path"
)

// UsesContentIdentity identifies publications described by ADR-0031.
func UsesContentIdentity(setID string) bool {
	return strings.HasPrefix(setID, contentSetPrefix)
}

// AuxiliaryContentSuffix binds a child reference to its kind, path, role and bytes.
func AuxiliaryContentSuffix(kind, filePath, checksum string, caFile bool) string {
	identity := fmt.Sprintf("%s\x00%s\x00%s\x00%t", kind, filePath, checksum, caFile)
	return fmt.Sprintf("-content-%x", sha256.Sum256([]byte(identity)))
}

func auxiliaryFileIdentity(kind string, file auxiliaryfiles.FileItem) string {
	filePath := file.GetIdentifier()
	caFile := false
	switch value := file.(type) {
	case auxiliaryfiles.GeneralFile:
		filePath, caFile = value.Path, value.IsCaFile
	case auxiliaryfiles.SSLCaFile:
		caFile = true
	}
	return AuxiliaryContentSuffix(kind, filePath, identityChecksum(file), caFile)
}

func auxiliaryClaimAnnotations(owner *haproxyv1alpha1.HAProxyCfg, claim string) map[string]string {
	if UsesContentIdentity(owner.Annotations[AuxiliarySetIDAnnotationKey]) {
		return map[string]string{auxiliaryClaimAnnotationKey: claim}
	}
	return runtimeConfigAnnotations(owner)
}

func retainReferencedClaim(existing metav1.Object, annotations map[string]string, owner *haproxyv1alpha1.HAProxyCfg, kind string) {
	if !UsesContentIdentity(owner.Annotations[AuxiliarySetIDAnnotationKey]) || owner.Status.AuxiliaryFiles == nil ||
		existing.GetAnnotations()[auxiliaryClaimAnnotationKey] == "" {
		return
	}
	refs := owner.Status.AuxiliaryFiles
	for _, group := range [][]haproxyv1alpha1.ResourceReference{
		refs.MapFiles, refs.SSLCertificates, refs.SSLCaFiles, refs.GeneralFiles, refs.CRTListFiles, refs.SecretFiles,
	} {
		for _, ref := range group {
			if ref.Kind == kind && ref.Name == existing.GetName() &&
				(ref.Namespace == "" || ref.Namespace == existing.GetNamespace()) {
				annotations[auxiliaryClaimAnnotationKey] = existing.GetAnnotations()[auxiliaryClaimAnnotationKey]
				return
			}
		}
	}
}
