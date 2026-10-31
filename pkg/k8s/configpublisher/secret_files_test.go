// Copyright 2026 Philipp Hossner
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

package configpublisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/haproxy-haptic/HAPTIC/pkg/compression"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

func secretFileSecrets(ctx context.Context, t *testing.T, kube *k8sfake.Clientset) []corev1.Secret {
	t.Helper()
	secrets, err := kube.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	var found []corev1.Secret
	for i := range secrets.Items {
		if secrets.Items[i].Labels["haproxy-haptic.org/type"] == secretFileType {
			found = append(found, secrets.Items[i])
		}
	}
	return found
}

// Anyone who can read a HAProxyCfg, or a Secret's metadata, must not be able to
// test guesses of a secret file's content against an unkeyed digest of it.
func TestSecretFileContentHasNoReadableUnkeyedDigest(t *testing.T) {
	ctx, kube, crd, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))

	sha := sha256.Sum256([]byte(testSecretFileContent))
	planDigest := renderplan.DigestString(testSecretFileContent)
	forbidden := []string{testSecretFileContent, hex.EncodeToString(sha[:]), planDigest}

	planSnapshot, err := req.OutputSnapshot.PlanSnapshot()
	require.NoError(t, err)
	plan, err := planSnapshot.SharedPlan()
	require.NoError(t, err)
	require.True(t, slicesContainDigest(plan.Files, planDigest), "the in-memory plan carries the digest the agent verifies")

	cfgs, err := crd.HaproxyTemplateICV1alpha1().HAProxyCfgs("default").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, cfgs.Items, 2, "the current configuration and its retained checkpoint")
	for i := range cfgs.Items {
		cfg := &cfgs.Items[i]
		readable, err := json.Marshal(cfg)
		require.NoError(t, err)
		texts := []string{string(readable)}
		if cfg.Spec.RetainedPlan != "" {
			payload, err := compression.Decompress(cfg.Spec.RetainedPlan)
			require.NoError(t, err)
			var decoded struct{ Plan string }
			require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
			checkpoint, err := compression.Decompress(decoded.Plan)
			require.NoError(t, err)
			require.Contains(t, checkpoint, testSecretFile.Seal)
			texts = append(texts, payload, checkpoint)
		}
		for _, text := range texts {
			for _, value := range forbidden {
				assert.NotContains(t, text, value, "HAProxyCfg %s", cfg.Name)
			}
		}
	}

	secrets := secretFileSecrets(ctx, t, kube)
	require.NotEmpty(t, secrets)
	for i := range secrets {
		secret := &secrets[i]
		assert.Equal(t, []byte(testSecretFileContent), secret.Data[secretFileDataKey])
		metadata, err := json.Marshal(secret.ObjectMeta)
		require.NoError(t, err)
		for _, value := range forbidden {
			assert.NotContains(t, string(metadata), value, "Secret %s", secret.Name)
		}
	}
}

func slicesContainDigest(files []renderplan.File, digest string) bool {
	for i := range files {
		if files[i].Digest == digest {
			return true
		}
	}
	return false
}

func TestSecretFilesArePrunedOnlyOnceDropped(t *testing.T) {
	ctx, kube, _, publisher := newTestPublisher(t)
	publish := func(files ...auxiliaryfiles.SecretFile) {
		t.Helper()
		req := basePublishRequest()
		req.Force = true
		req.AuxiliaryFiles = &AuxiliaryFiles{SecretFiles: files}
		_, err := publisher.PublishConfig(ctx, &req)
		require.NoError(t, err)
	}
	other := auxiliaryfiles.SecretFile{Path: "secrets/other", Content: "x", Seal: auxiliaryfiles.SealContent(nil, "x")}

	publish(testSecretFile, other)
	require.Len(t, secretFileSecrets(ctx, t, kube), 2)
	publish(testSecretFile, other)
	require.Len(t, secretFileSecrets(ctx, t, kube), 2, "a republication keeps every desired secret file")
	publish(testSecretFile)
	remaining := secretFileSecrets(ctx, t, kube)
	require.Len(t, remaining, 1)
	assert.Equal(t, []byte(testSecretFile.Path), remaining[0].Data["path"])
}

func TestRetainedRefusesTamperedSecretFile(t *testing.T) {
	ctx, kube, _, publisher := newTestPublisher(t)
	req := retainedPublishRequest(t)
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	require.NoError(t, publisher.PublishRetained(ctx, &req))
	loaded, err := publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.NoError(t, err)
	require.Len(t, loaded, 1)

	for _, secret := range secretFileSecrets(ctx, t, kube) {
		if !strings.HasSuffix(secret.Name, retainedChildSuffix) {
			continue
		}
		secret.Data[secretFileDataKey] = []byte("tampered")
		_, err := kube.CoreV1().Secrets("default").Update(ctx, &secret, metav1.UpdateOptions{})
		require.NoError(t, err)
	}
	_, err = publisher.LoadRetained(ctx, req.TemplateConfigNamespace, req.TemplateConfigName, req.TemplateConfigUID)
	require.ErrorContains(t, err, "auxiliary checksum differs")
}
