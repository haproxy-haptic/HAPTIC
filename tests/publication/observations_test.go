// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package publication

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	agentapi "github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

type fixture struct {
	config  api.HAProxyCfg
	agents  map[string]agentapi.State
	secrets []corev1.Secret
}

func publicationFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{agents: map[string]agentapi.State{}}
	require.NoError(t, json.Unmarshal([]byte(`{"metadata":{"uid":"config-uid","annotations":{"haproxy-haptic.org/auxiliary-set-id":"current"}},"spec":{"checksum":"current-checksum"},"status":{"auxiliaryFiles":{"setID":"current","sslCertificates":[{"name":"certificate"}]}}}`), &f.config))
	for _, uid := range []string{"pod-a", "pod-b"} {
		f.config.Status.DeployedToPods = append(f.config.Status.DeployedToPods, api.PodDeploymentStatus{PodUID: uid, Checksum: "current-checksum", AppliedPlanID: "applied", RunningPlanID: "running", WorkerOpsPlanID: "worker"})
		f.agents[uid] = agentapi.State{AppliedPlanID: "applied", RunningPlanID: "running", WorkerOpsPlanID: "worker"}
	}
	f.secrets = []corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: "certificate", OwnerReferences: []metav1.OwnerReference{{UID: "config-uid"}}, Annotations: map[string]string{annotationPrefix + "auxiliary-set-id": "current"}}}}
	return f
}

func TestPublicationRequiresExactFleetAndCertificateCleanup(t *testing.T) {
	for name, mutate := range map[string]func(*fixture){
		"new plan before publication": func(f *fixture) { state := f.agents["pod-a"]; state.AppliedPlanID = "new"; f.agents["pod-a"] = state },
		"stale running proof":         func(f *fixture) { state := f.agents["pod-a"]; state.RunningPlanID = "new"; f.agents["pod-a"] = state },
		"pending reload": func(f *fixture) {
			state := f.agents["pod-a"]
			state.ReloadPendingAt = "2026-10-08T00:00:00Z"
			f.agents["pod-a"] = state
		},
		"replaced pod":                 func(f *fixture) { f.agents["replacement"] = f.agents["pod-b"]; delete(f.agents, "pod-b") },
		"partial fleet":                func(f *fixture) { delete(f.agents, "pod-b") },
		"stale auxiliary refs":         func(f *fixture) { f.config.Status.AuxiliaryFiles.SetID = "previous" },
		"pending certificate creation": func(f *fixture) { f.secrets = nil },
		"pending certificate cleanup": func(f *fixture) {
			old := f.secrets[0].DeepCopy()
			old.Name = "previous"
			f.secrets = append(f.secrets, *old)
		},
		"stale certificate":  func(f *fixture) { f.secrets[0].Annotations[annotationPrefix+"auxiliary-set-id"] = "previous" },
		"deployment error":   func(f *fixture) { f.config.Status.DeployedToPods[0].LastError = "NACK" },
		"consecutive errors": func(f *fixture) { f.config.Status.DeployedToPods[0].ConsecutiveErrors = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			f := publicationFixture(t)
			require.True(t, Matches(&f.config, f.agents, f.secrets, 2))
			mutate(f)
			require.False(t, Matches(&f.config, f.agents, f.secrets, 2))
		})
	}
	f := publicationFixture(t)
	f.secrets = append(f.secrets, corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unowned-credentials"}})
	require.True(t, Matches(&f.config, f.agents, f.secrets, 2))
}

func TestContentAddressedCertificatePublication(t *testing.T) {
	for _, role := range []string{roleCertificate, roleCA, roleSecretFile} {
		f := publicationFixture(t)
		setID := "content-sha256:current"
		f.config.Annotations[annotationPrefix+"auxiliary-set-id"] = setID
		f.config.Status.AuxiliaryFiles.SetID = setID
		metadata := &f.secrets[0].ObjectMeta
		metadata.Name = "certificate-content-261e534be7bee0881c31c50d5e8ef6303d6326428393529f500ca817e0484f3c"
		if role == roleCA {
			metadata.Name = "certificate-content-f6985c9509794eb715d72fdaf927bb62de0f3980f3850f4baab4817c352c252b"
		}
		metadata.Annotations = map[string]string{annotationPrefix + "auxiliary-path": "/certs/site.pem", annotationPrefix + "checksum": "sha256:certificate", annotationPrefix + "auxiliary-claim": "retained"}
		metadata.Labels = map[string]string{annotationPrefix + "type": role}
		refs := []api.ResourceReference{{Name: metadata.Name}}
		auxiliaryFiles := f.config.Status.AuxiliaryFiles
		auxiliaryFiles.SSLCertificates = nil
		switch role {
		case roleCA:
			auxiliaryFiles.SSLCaFiles = refs
		case roleSecretFile:
			auxiliaryFiles.SecretFiles = refs
		default:
			auxiliaryFiles.SSLCertificates = refs
		}
		require.True(t, Matches(&f.config, f.agents, f.secrets, 2))
		metadata.Name += "-publication"
		refs[0].Name = metadata.Name
		require.True(t, Matches(&f.config, f.agents, f.secrets, 2))
		for _, key := range []string{"auxiliary-path", "checksum"} {
			old := metadata.Annotations[annotationPrefix+key]
			metadata.Annotations[annotationPrefix+key] = "different"
			require.False(t, Matches(&f.config, f.agents, f.secrets, 2))
			delete(metadata.Annotations, annotationPrefix+key)
			require.False(t, Matches(&f.config, f.agents, f.secrets, 2))
			metadata.Annotations[annotationPrefix+key] = old
		}
		metadata.Labels[annotationPrefix+"type"] = "wrong-role"
		require.False(t, Matches(&f.config, f.agents, f.secrets, 2))
		metadata.Labels[annotationPrefix+"type"] = role
		metadata.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
		require.False(t, Matches(&f.config, f.agents, f.secrets, 2))
		metadata.DeletionTimestamp = nil
		old := f.secrets[0].DeepCopy()
		old.Name = "previous"
		f.secrets = append(f.secrets, *old)
		require.False(t, Matches(&f.config, f.agents, f.secrets, 2))
	}
}

func TestPublicationAllowsReceiptWithoutOptionalRuntimeOperationProof(t *testing.T) {
	f := publicationFixture(t)
	for i := range f.config.Status.DeployedToPods {
		f.config.Status.DeployedToPods[i].WorkerOpsPlanID = ""
	}
	require.True(t, Matches(&f.config, f.agents, f.secrets, 2))
}
