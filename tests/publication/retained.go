// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package publication

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
)

// RetainedMatches waits for checkpoint publication and cascading certificate cleanup.
func RetainedMatches(config *api.HAProxyCfg, configs []api.HAProxyCfg, secrets []corev1.Secret) bool {
	owner := metav1.GetControllerOf(config)
	if owner == nil || owner.UID == "" || len(config.Status.RetainedConfigs) == 0 {
		return false
	}
	byUID := make(map[types.UID]*api.HAProxyCfg, len(configs))
	for i := range configs {
		byUID[configs[i].UID] = &configs[i]
	}
	current := byUID[config.UID]
	if current == nil || current.ResourceVersion != config.ResourceVersion || !retainedReferencesMatch(config, byUID, secrets, owner) {
		return false
	}
	return retainedCleanupComplete(config, byUID, secrets, owner)
}

func retainedReferencesMatch(config *api.HAProxyCfg, configs map[types.UID]*api.HAProxyCfg, secrets []corev1.Secret, owner *metav1.OwnerReference) bool {
	covered := make(map[string]bool)
	seen := make(map[types.UID]bool)
	for _, ref := range config.Status.RetainedConfigs {
		uid := types.UID(ref.UID)
		retained := configs[uid]
		if uid == "" || seen[uid] || retained == nil || retained.Namespace != config.Namespace || !retainedReferenceMatches(retained, ref, owner) {
			return false
		}
		seen[uid] = true
		if !retainedCertificatesMatch(retained, secrets, owner) {
			return false
		}
		if retained.Spec.Checksum == config.Spec.Checksum {
			for i := range retained.Status.DeployedToPods {
				pod := &retained.Status.DeployedToPods[i]
				if retainedAcknowledges(pod, config.Spec.Checksum) {
					covered[pod.AppliedPlanID] = true
				}
			}
		}
	}
	return len(config.Status.DeployedToPods) > 0 && !slices.ContainsFunc(config.Status.DeployedToPods, func(pod api.PodDeploymentStatus) bool {
		return !covered[pod.AppliedPlanID]
	})
}

// retainedCertificatesMatch accepts certificates the checkpoint owns, and
// certificates shared by every checkpoint of the template.
func retainedCertificatesMatch(config *api.HAProxyCfg, secrets []corev1.Secret, owner *metav1.OwnerReference) bool {
	setID := config.Annotations[annotationPrefix+"auxiliary-set-id"]
	refs := config.Status.AuxiliaryFiles
	if setID == "" || refs == nil || refs.SetID != setID {
		return false
	}
	var owned []corev1.Secret
	shared := make(map[string]*corev1.Secret)
	for i := range secrets {
		if sharedRetainedSecret(&secrets[i], owner) {
			shared[secrets[i].Name] = &secrets[i]
		} else {
			owned = append(owned, secrets[i])
		}
	}
	trimmed := config.DeepCopy()
	trimmed.Status.AuxiliaryFiles.SSLCertificates = unsharedReferences(refs.SSLCertificates, shared, setID, roleCertificate)
	trimmed.Status.AuxiliaryFiles.SSLCaFiles = unsharedReferences(refs.SSLCaFiles, shared, setID, roleCA)
	trimmed.Status.AuxiliaryFiles.SecretFiles = unsharedReferences(refs.SecretFiles, shared, setID, roleSecretFile)
	return certificatesMatch(trimmed, owned, setID)
}

// unsharedReferences returns the references no valid shared Secret satisfies.
func unsharedReferences(refs []api.ResourceReference, shared map[string]*corev1.Secret, setID, role string) []api.ResourceReference {
	var remaining []api.ResourceReference
	for _, ref := range refs {
		if secret := shared[ref.Name]; secret == nil || !FileSecretIdentityMatches(&secret.ObjectMeta, setID, role) {
			remaining = append(remaining, ref)
		}
	}
	return remaining
}

func sharedRetainedSecret(secret *corev1.Secret, owner *metav1.OwnerReference) bool {
	parent := metav1.GetControllerOf(secret)
	return parent != nil && parent.UID == owner.UID && parent.Kind == owner.Kind && parent.APIVersion == owner.APIVersion &&
		secret.Labels[annotationPrefix+"retained-for"] == string(owner.UID)
}

func retainedAcknowledges(pod *api.PodDeploymentStatus, checksum string) bool {
	return pod.Checksum == checksum && pod.AppliedPlanID != "" && pod.RunningPlanID != "" && pod.LastError == "" && pod.ConsecutiveErrors == 0
}

func retainedReferenceMatches(retained *api.HAProxyCfg, ref api.RetainedConfigReference, owner *metav1.OwnerReference) bool {
	parent := metav1.GetControllerOf(retained)
	return retained.Name == ref.Name && retained.Spec.Checksum == ref.Checksum && retained.Spec.RetainedPlan != "" &&
		retained.DeletionTimestamp == nil && parent != nil && parent.UID == owner.UID && parent.Name == owner.Name && parent.Kind == owner.Kind && parent.APIVersion == owner.APIVersion &&
		retained.Labels[annotationPrefix+"retained-for"] == string(owner.UID)
}

func retainedCleanupComplete(config *api.HAProxyCfg, configs map[types.UID]*api.HAProxyCfg, secrets []corev1.Secret, owner *metav1.OwnerReference) bool {
	for uid, candidate := range configs {
		parent := metav1.GetControllerOf(candidate)
		if candidate.Spec.RetainedPlan == "" || parent == nil || parent.UID != owner.UID {
			continue
		}
		if !slices.ContainsFunc(config.Status.RetainedConfigs, func(ref api.RetainedConfigReference) bool { return ref.UID == string(uid) }) {
			return false
		}
	}
	referenced := retainedCertificateNames(config, configs)
	for i := range secrets {
		if sharedRetainedSecret(&secrets[i], owner) && !referenced[secrets[i].Name] {
			return false
		}
		parent := metav1.GetControllerOf(&secrets[i])
		if parent != nil && parent.Kind == "HAProxyCfg" && parent.APIVersion == api.SchemeGroupVersion.String() {
			if live := configs[parent.UID]; live == nil || live.Name != parent.Name || live.DeletionTimestamp != nil {
				return false
			}
		}
	}
	return true
}

func retainedCertificateNames(config *api.HAProxyCfg, configs map[types.UID]*api.HAProxyCfg) map[string]bool {
	names := make(map[string]bool)
	for _, ref := range config.Status.RetainedConfigs {
		retained := configs[types.UID(ref.UID)]
		if retained == nil || retained.Status.AuxiliaryFiles == nil {
			continue
		}
		for _, group := range secretRoleGroups(retained.Status.AuxiliaryFiles) {
			for _, ref := range group.refs {
				names[ref.Name] = true
			}
		}
	}
	return names
}
