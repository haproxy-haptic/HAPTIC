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

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/compression"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
)

type delayedPublishedSyncer struct {
	store types.Store
}

func (s *delayedPublishedSyncer) WaitForSync(context.Context) (int, error) {
	return 1, nil
}

func (s *delayedPublishedSyncer) Store() types.Store {
	return s.store
}

func auxUnstructured(name, filePath, contentField, content string, compressed bool) *unstructured.Unstructured {
	spec := map[string]any{"path": filePath, contentField: content}
	if compressed {
		spec["compressed"] = true
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "haproxy-haptic.org/v1alpha1",
		"kind":       "HAProxyGeneralFile",
		"metadata":   map[string]any{"name": name, "namespace": "haptic"},
		"spec":       spec,
	}}
}

func publishedKind(t *testing.T, gvr string) *publishedAuxCRD {
	t.Helper()
	for _, kind := range publishedAuxCRDList() {
		if kind.gvr.String() == gvr {
			return &kind
		}
	}
	t.Fatalf("published kind %s not found", gvr)
	return nil
}

func publishedSnapshot(t *testing.T, published *publishedAuxFiles) map[string]string {
	t.Helper()
	files, err := published.get()
	require.NoError(t, err)
	return files
}

func TestSyncAndRefreshPublishedStoreDoesNotDependOnDelayedCallbacks(t *testing.T) {
	const (
		runtimeConfigName = "example-haproxycfg"
		setID             = "sha256:set-a"
	)
	mapGVR := haproxyMapFileGVR.String()
	published := newPublishedAuxFiles("haptic")

	childStore := store.NewMemoryStore(1)
	child := auxUnstructured("routes", "maps/routes.map", "entries", "example.test backend", false)
	child.SetAnnotations(map[string]string{"haproxy-haptic.org/auxiliary-set-id": setID})
	require.NoError(t, childStore.Add(child, []string{"routes"}))
	require.NoError(t, syncAndRefreshPublishedStore(t.Context(), &delayedPublishedSyncer{store: childStore},
		func(s types.Store, _ types.ChangeStats) {
			files, err := publishedAuxFilesFromStore(s, publishedKind(t, mapGVR))
			require.NoError(t, err)
			published.setForGVR(mapGVR, files)
		}))

	parentStore := store.NewMemoryStore(1)
	parent := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": runtimeConfigName},
		"status": map[string]any{"auxiliaryFiles": map[string]any{
			"setID": setID,
			"mapFiles": []any{map[string]any{
				"kind": "HAProxyMapFile", "name": "routes", "namespace": "haptic",
			}},
		}},
	}}
	require.NoError(t, parentStore.Add(parent, []string{runtimeConfigName}))
	require.NoError(t, syncAndRefreshPublishedStore(t.Context(), &delayedPublishedSyncer{store: parentStore},
		func(s types.Store, _ types.ChangeStats) {
			commit, found, err := publishedAuxCommitFromStore(s, runtimeConfigName)
			require.NoError(t, err)
			require.True(t, found)
			published.setCommit(commit)
		}))

	require.NoError(t, published.readinessError())
	assert.Equal(t, map[string]string{"routes.map": "example.test backend"}, publishedSnapshot(t, published))
}

// auxFilesFromStore must flatten aux CRD objects into base-filename → content,
// reading content from the kind's content field, decompressing compressed values
// and skipping objects with no path.
func TestAuxFilesFromStore(t *testing.T) {
	plain := "key-a\nkey-b\nkey-c\n"
	body := "HTTP/1.0 503 Service Unavailable\r\n\r\nlong error page body ...\n"

	s := store.NewMemoryStore(1)
	require.NoError(t, s.Add(auxUnstructured("gf-ticket", "general/tls-ticket-keys", "content", plain, false), []string{"gf-ticket"}))
	require.NoError(t, s.Add(auxUnstructured("gf-503", "general/503.http", "content", compression.Compress(body), true), []string{"gf-503"}))
	// No path → skipped.
	require.NoError(t, s.Add(auxUnstructured("gf-bad", "", "content", "orphan", false), []string{"gf-bad"}))

	got, err := publishedAuxFilesFromStore(s, publishedKind(t, haproxyGeneralFileGVR.String()))
	require.NoError(t, err)
	assert.Equal(t, plain, got["gf-ticket"].content)
	assert.Equal(t, body, got["gf-503"].content)
	assert.Len(t, got, 2)
}

// Map files store content under `entries`, not `content` — the extractor must
// read whichever field the kind declares.
func TestAuxFilesFromStore_EntriesField(t *testing.T) {
	s := store.NewMemoryStore(1)
	require.NoError(t, s.Add(auxUnstructured("mf-host", "maps/host.map", "entries", "example.com be_x\n", false), []string{"mf-host"}))

	got, err := publishedAuxFilesFromStore(s, publishedKind(t, haproxyMapFileGVR.String()))
	require.NoError(t, err)
	assert.Equal(t, "example.com be_x\n", got["mf-host"].content)
}

func TestPublishedAuxFilesEmptyContent(t *testing.T) {
	for _, kind := range publishedAuxCRDList() {
		if kind.contentField == "" {
			continue
		}
		t.Run(kind.kind, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				spec  map[string]any
				valid bool
			}{
				{"explicit empty", map[string]any{"empty": true}, true},
				{"missing content", map[string]any{}, false},
				{"false empty marker", map[string]any{"empty": false}, false},
				{"invalid empty marker", map[string]any{"empty": "true"}, false},
				{"contradictory content", map[string]any{"empty": true, kind.contentField: "entry"}, false},
				{"compressed empty", map[string]any{"empty": true, "compressed": true}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					tc.spec["path"] = "maps/empty.map"
					obj := &unstructured.Unstructured{Object: map[string]any{
						"apiVersion": "haproxy-haptic.org/v1alpha1", "kind": kind.kind,
						"metadata": map[string]any{"name": "empty", "namespace": "haptic"},
						"spec":     tc.spec,
					}}
					obj.SetAnnotations(map[string]string{"haproxy-haptic.org/auxiliary-set-id": "sha256:set-empty"})
					s := store.NewMemoryStore(1)
					require.NoError(t, s.Add(obj, []string{"empty"}))
					files, err := publishedAuxFilesFromStore(s, &kind)
					if !tc.valid {
						require.Error(t, err)
						return
					}
					require.NoError(t, err)
					published := newPublishedAuxFiles("haptic")
					published.setForGVR(kind.gvr.String(), files)
					published.setCommit(&publishedAuxCommit{
						setID: "sha256:set-empty",
						refs: map[string][]publishedAuxRef{
							kind.referenceFields[0]: {{name: "empty", namespace: "haptic"}},
						},
					})
					require.NoError(t, published.readinessError())
					require.Equal(t, map[string]string{"empty.map": ""}, publishedSnapshot(t, published))
				})
			}
		})
	}
}

func TestPublishedSecretStoreReadsMetadataOnly(t *testing.T) {
	s := store.NewMemoryStore(1)
	secret := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":            "certificate",
			"resourceVersion": "42",
			"annotations": map[string]any{
				"haproxy-haptic.org/auxiliary-set-id": "sha256:set-a",
				"haproxy-haptic.org/checksum":         "checksum-a",
			},
		},
		"data": map[string]any{"tls.key": []any{"must", "not", "be", "read"}},
	}}
	require.NoError(t, s.Add(secret, []string{"certificate"}))

	got, err := publishedAuxFilesFromStore(s, publishedKind(t, secretGVR.String()))
	require.NoError(t, err)
	assert.Equal(t, publishedAuxFile{
		setID:           "sha256:set-a",
		checksum:        "checksum-a",
		resourceVersion: "42",
	}, got["certificate"])
}

func TestPublishedAuxFilesRestartReconstructionPreservesAllArtifactFamilies(t *testing.T) {
	const setID = "sha256:all-families"
	published := newPublishedAuxFiles("haptic")

	addSetID := func(obj *unstructured.Unstructured) *unstructured.Unstructured {
		obj.SetAnnotations(map[string]string{
			"haproxy-haptic.org/auxiliary-set-id": setID,
		})
		return obj
	}
	load := func(kind *publishedAuxCRD, objects ...*unstructured.Unstructured) map[string]publishedAuxFile {
		t.Helper()
		s := store.NewMemoryStore(1)
		for _, object := range objects {
			require.NoError(t, s.Add(object, []string{object.GetName()}))
		}
		files, err := publishedAuxFilesFromStore(s, kind)
		require.NoError(t, err)
		return files
	}

	mapKind := publishedKind(t, haproxyMapFileGVR.String())
	mapFile := addSetID(auxUnstructured("routes", "maps/routes.map", "entries", "example.test backend\n", false))
	published.setForGVR(mapKind.gvr.String(), load(mapKind, mapFile))

	generalKind := publishedKind(t, haproxyGeneralFileGVR.String())
	generalFile := addSetID(auxUnstructured("error", "general/error.http", "content", "error page\n", false))
	generalCA := addSetID(auxUnstructured("dynamic-ca", "general/dynamic-ca.pem", "content", "dynamic CA\n", false))
	require.NoError(t, unstructured.SetNestedField(generalCA.Object, true, "spec", "caFile"))
	generalFiles := load(generalKind, generalFile, generalCA)
	require.True(t, generalFiles["dynamic-ca"].caFile)
	published.setForGVR(generalKind.gvr.String(), generalFiles)

	crtListKind := publishedKind(t, haproxyCRTListFileGVR.String())
	crtList := addSetID(auxUnstructured("frontends", "general/frontends.list", "entries", "ssl/site.pem\n", false))
	published.setForGVR(crtListKind.gvr.String(), load(crtListKind, crtList))

	secretKind := publishedKind(t, secretGVR.String())
	secret := func(name, fileType string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name": name,
				"annotations": map[string]any{
					"haproxy-haptic.org/auxiliary-set-id": setID,
				},
				"labels": map[string]any{"haproxy-haptic.org/type": fileType},
			},
		}}
	}
	published.setForGVR(secretKind.gvr.String(), load(secretKind,
		secret("haproxy-cert-trust", "ssl-certificate"),
		secret("haproxy-ca-trust", "ssl-ca"),
	))

	parent := map[string]any{
		"status": map[string]any{"auxiliaryFiles": map[string]any{
			"setID": setID,
			"mapFiles": []any{map[string]any{
				"kind": "HAProxyMapFile", "name": "routes", "namespace": "haptic",
			}},
			"sslCertificates": []any{map[string]any{
				"kind": "Secret", "name": "haproxy-cert-trust", "namespace": "haptic",
			}},
			"sslCaFiles": []any{map[string]any{
				"kind": "Secret", "name": "haproxy-ca-trust", "namespace": "haptic",
			}},
			"generalFiles": []any{
				map[string]any{"kind": "HAProxyGeneralFile", "name": "error", "namespace": "haptic"},
				map[string]any{"kind": "HAProxyGeneralFile", "name": "dynamic-ca", "namespace": "haptic"},
			},
			"crtListFiles": []any{map[string]any{
				"kind": "HAProxyCRTListFile", "name": "frontends", "namespace": "haptic",
			}},
		}},
	}
	commit, err := publishedAuxCommitFromObject(parent, "haptic-haproxycfg")
	require.NoError(t, err)
	assert.Equal(t, []publishedAuxRef{{name: "haproxy-cert-trust", namespace: "haptic"}}, commit.refs["sslCertificates"])
	assert.Equal(t, []publishedAuxRef{{name: "haproxy-ca-trust", namespace: "haptic"}}, commit.refs["sslCaFiles"])
	assert.NotEqual(t, commit.refs["sslCertificates"], commit.refs["sslCaFiles"])

	published.setCommit(commit)
	require.NoError(t, published.readinessError())
	assert.Equal(t, map[string]string{
		"routes.map":     "example.test backend\n",
		"error.http":     "error page\n",
		"frontends.list": "ssl/site.pem\n",
	}, publishedSnapshot(t, published))
}

func TestPublishedAuxFilesRejectsCertificateCAReferenceAlias(t *testing.T) {
	published := newPublishedAuxFiles("haptic")
	published.setForGVR(secretGVR.String(), map[string]publishedAuxFile{
		"trust": {setID: "sha256:set-a"},
	})
	published.setCommit(&publishedAuxCommit{setID: "sha256:set-a", refs: map[string][]publishedAuxRef{
		"sslCertificates": {{name: "trust"}},
		"sslCaFiles":      {{name: "trust", namespace: "haptic"}},
	}})

	require.EqualError(t, published.readinessError(),
		"certificate and CA references alias Secret haptic/trust")
}

func TestPublishedAuxFilesAdvancesOnlyAtCommittedReferenceBoundary(t *testing.T) {
	const (
		setA = "sha256:set-a"
		setB = "sha256:set-b"
	)
	mapGVR := haproxyMapFileGVR.String()
	generalGVR := haproxyGeneralFileGVR.String()
	secretResourceGVR := secretGVR.String()
	published := newPublishedAuxFiles("haptic")
	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map-a": {path: "maps/routes.map", content: "map-a", setID: setA},
	})
	published.setForGVR(generalGVR, map[string]publishedAuxFile{
		"general-a": {path: "general/error.http", content: "general-a", setID: setA},
	})
	published.setForGVR(secretResourceGVR, map[string]publishedAuxFile{
		"secret-a": {setID: setA},
	})
	published.setCommit(&publishedAuxCommit{setID: setA, refs: map[string][]publishedAuxRef{
		"mapFiles":        {{name: "map-a", namespace: "haptic"}},
		"generalFiles":    {{name: "general-a", namespace: "haptic"}},
		"sslCertificates": {{name: "secret-a", namespace: "haptic"}},
	}})
	require.NoError(t, published.readinessError())
	assert.Equal(t, map[string]string{"routes.map": "map-a", "error.http": "general-a"}, publishedSnapshot(t, published))

	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map-a": {path: "maps/routes.map", content: "map-a", setID: setA},
		"map-b": {path: "maps/routes.map", content: "map-b", setID: setB},
	})
	assert.Equal(t, "map-a", publishedSnapshot(t, published)["routes.map"], "uncommitted child must stay invisible")

	published.setCommit(&publishedAuxCommit{setID: setB, refs: map[string][]publishedAuxRef{
		"mapFiles":        {{name: "map-b", namespace: "haptic"}},
		"generalFiles":    {{name: "general-b", namespace: "haptic"}},
		"sslCertificates": {{name: "secret-b", namespace: "haptic"}},
	}})
	assert.Equal(t, map[string]string{"routes.map": "map-a", "error.http": "general-a"}, publishedSnapshot(t, published),
		"a parent event arriving before every child watcher must retain the prior complete snapshot")

	published.setForGVR(generalGVR, map[string]publishedAuxFile{
		"general-a": {path: "general/error.http", content: "general-a", setID: setA},
		"general-b": {path: "general/error.http", content: "general-b", setID: setB},
	})
	assert.Equal(t, map[string]string{"routes.map": "map-a", "error.http": "general-a"}, publishedSnapshot(t, published),
		"missing committed Secret metadata must retain the prior snapshot")
	published.setForGVR(secretResourceGVR, map[string]publishedAuxFile{
		"secret-a": {setID: setA},
		"secret-b": {setID: "sha256:wrong"},
	})
	assert.Equal(t, map[string]string{"routes.map": "map-a", "error.http": "general-a"}, publishedSnapshot(t, published),
		"a Secret from another set must retain the prior snapshot")
	published.setForGVR(secretResourceGVR, map[string]publishedAuxFile{
		"secret-a": {setID: setA},
		"secret-b": {setID: setB},
	})
	assert.Equal(t, map[string]string{"routes.map": "map-b", "error.http": "general-b"}, publishedSnapshot(t, published))
}

func TestPublishedAuxFilesRejectsFailedPartialSet(t *testing.T) {
	mapGVR := haproxyMapFileGVR.String()
	published := newPublishedAuxFiles("haptic")
	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map-a": {path: "maps/routes.map", content: "committed", setID: "sha256:set-a"},
	})
	published.setCommit(&publishedAuxCommit{setID: "sha256:set-a", refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "map-a", namespace: "haptic"}},
	}})

	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map-a": {path: "maps/routes.map", content: "committed", setID: "sha256:set-a"},
		"map-b": {path: "maps/routes.map", content: "failed-partial", setID: "sha256:set-b"},
	})
	assert.Equal(t, map[string]string{"routes.map": "committed"}, publishedSnapshot(t, published))

	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map-a": {path: "maps/routes.map", content: "committed", setID: "sha256:set-a"},
	})
	published.setCommit(&publishedAuxCommit{setID: "sha256:set-b", refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "map-b", namespace: "haptic"}},
	}})
	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map-a": {path: "maps/routes.map", content: "committed", setID: "sha256:set-a"},
		"map-b": {path: "maps/routes.map", content: "wrong-set", setID: "sha256:set-c"},
	})
	assert.Equal(t, map[string]string{"routes.map": "committed"}, publishedSnapshot(t, published))
}

func TestPublishedAuxFilesFailsColdStartOnIncompleteCommit(t *testing.T) {
	mapGVR := haproxyMapFileGVR.String()
	published := newPublishedAuxFiles("haptic")
	published.setForGVR(mapGVR, map[string]publishedAuxFile{})
	published.setCommit(&publishedAuxCommit{setID: "sha256:set-a", refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "missing", namespace: "haptic"}},
	}})

	require.EqualError(t, published.readinessError(), "committed HAProxyMapFile haptic/missing is unavailable")
	assert.Empty(t, publishedSnapshot(t, published))
}

func TestPublishedAuxFilesRejectsCrossNamespaceSecretReference(t *testing.T) {
	secretResourceGVR := secretGVR.String()
	published := newPublishedAuxFiles("haptic")
	published.setForGVR(secretResourceGVR, map[string]publishedAuxFile{
		"certificate": {setID: "sha256:set-a"},
	})
	published.setCommit(&publishedAuxCommit{setID: "sha256:set-a", refs: map[string][]publishedAuxRef{
		"sslCertificates": {{name: "certificate", namespace: "other"}},
	}})

	require.EqualError(t, published.readinessError(), "committed Secret other/certificate is outside namespace haptic")
}

func TestPublishedAuxFilesRejectsLegacyMutationUntilSetIDAppears(t *testing.T) {
	mapGVR := haproxyMapFileGVR.String()
	published := newPublishedAuxFiles("haptic")
	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map": {path: "maps/routes.map", content: "legacy"},
	})
	published.setCommit(&publishedAuxCommit{refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "map", namespace: "haptic"}},
	}})
	assert.Equal(t, "legacy", publishedSnapshot(t, published)["routes.map"])

	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map":        {path: "maps/routes.map", content: "legacy"},
		"modern-map": {path: "maps/routes.map", content: "modern", setID: "sha256:set-b"},
	})
	assert.Equal(t, "legacy", publishedSnapshot(t, published)["routes.map"],
		"an immutable modern child is not authoritative before its parent commit")

	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map":        {path: "maps/routes.map", content: "legacy-updated"},
		"modern-map": {path: "maps/routes.map", content: "modern", setID: "sha256:set-b"},
	})
	_, err := published.get()
	require.ErrorContains(t, err, "legacy auxiliary publication changed without a set ID")

	published.setCommit(&publishedAuxCommit{setID: "sha256:set-b", refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "modern-map", namespace: "haptic"}},
	}})
	assert.Equal(t, "modern", publishedSnapshot(t, published)["routes.map"])
}

func TestPublishedAuxFilesLeaderTermLiftsLegacyLatch(t *testing.T) {
	mapGVR := haproxyMapFileGVR.String()
	legacyCommit := &publishedAuxCommit{refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "map", namespace: "haptic"}},
	}}
	published := newPublishedAuxFiles("haptic")
	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map": {path: "maps/routes.map", content: "legacy-1"},
	})
	published.setCommit(legacyCommit)
	assert.Equal(t, "legacy-1", publishedSnapshot(t, published)["routes.map"])

	// A standby latches when an old-version leader mutates the legacy set.
	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map": {path: "maps/routes.map", content: "legacy-2"},
	})
	_, err := published.get()
	require.ErrorContains(t, err, "legacy auxiliary publication changed without a set ID")

	// Becoming leader accepts the visible legacy snapshot, exactly as a
	// restarted process would, instead of failing every render until a restart.
	published.beginLeaderTerm()
	assert.Equal(t, "legacy-2", publishedSnapshot(t, published)["routes.map"])

	// A late legacy write landing during the term must not re-latch the leader.
	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map": {path: "maps/routes.map", content: "legacy-3"},
	})
	assert.Equal(t, "legacy-3", publishedSnapshot(t, published)["routes.map"])
	published.setError(errors.New("transient list failure"))
	assert.Equal(t, "legacy-3", publishedSnapshot(t, published)["routes.map"])

	// Back to standby: legacy mutations latch again until a set ID appears.
	published.endLeaderTerm()
	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map": {path: "maps/routes.map", content: "legacy-4"},
	})
	_, err = published.get()
	require.ErrorContains(t, err, "legacy auxiliary publication changed without a set ID")
	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map":        {path: "maps/routes.map", content: "legacy-4"},
		"modern-map": {path: "maps/routes.map", content: "modern", setID: "sha256:set-b"},
	})
	published.setCommit(&publishedAuxCommit{setID: "sha256:set-b", refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "modern-map", namespace: "haptic"}},
	}})
	assert.Equal(t, "modern", publishedSnapshot(t, published)["routes.map"])
}

func TestPublishedAuxFilesLeaderTermKeepsModernDowngradeLatch(t *testing.T) {
	mapGVR := haproxyMapFileGVR.String()
	published := newPublishedAuxFiles("haptic")
	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map-a": {path: "maps/routes.map", content: "modern-a", setID: "sha256:set-a"},
	})
	published.setCommit(&publishedAuxCommit{setID: "sha256:set-a", refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "map-a", namespace: "haptic"}},
	}})
	assert.Equal(t, "modern-a", publishedSnapshot(t, published)["routes.map"])

	published.setCommit(&publishedAuxCommit{refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "map-a", namespace: "haptic"}},
	}})
	published.beginLeaderTerm()
	_, err := published.get()
	require.ErrorContains(t, err, "auxiliary publication lost its set ID",
		"a set that lost its ID means another writer exists; leadership must not accept it")
}

func TestPublishedAuxFilesRejectsLegacySecretMutationByResourceVersion(t *testing.T) {
	secretResourceGVR := secretGVR.String()
	secretKind := publishedKind(t, secretResourceGVR)
	parse := func(resourceVersion, certificate string) publishedAuxFile {
		t.Helper()
		_, file, found, err := publishedAuxFileFromObject(map[string]any{
			"metadata": map[string]any{
				"name":            "certificate",
				"resourceVersion": resourceVersion,
				"annotations": map[string]any{
					"haproxy-haptic.org/checksum": "unchanged",
				},
			},
			"data": map[string]any{"certificate": certificate},
		}, secretKind)
		require.NoError(t, err)
		require.True(t, found)
		return file
	}
	published := newPublishedAuxFiles("haptic")
	published.setForGVR(secretResourceGVR, map[string]publishedAuxFile{
		"certificate": parse("1", "legacy-a"),
	})
	published.setCommit(&publishedAuxCommit{refs: map[string][]publishedAuxRef{
		"sslCertificates": {{name: "certificate", namespace: "haptic"}},
	}})
	require.NoError(t, published.readinessError())

	published.setForGVR(secretResourceGVR, map[string]publishedAuxFile{
		"certificate": parse("2", "legacy-b"),
	})
	_, err := published.get()
	require.ErrorContains(t, err, "legacy auxiliary publication changed without a set ID")
}

func TestPublishedAuxFilesNeverDowngradesAfterModernCommit(t *testing.T) {
	mapGVR := haproxyMapFileGVR.String()
	published := newPublishedAuxFiles("haptic")
	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map-a": {path: "maps/routes.map", content: "modern-a", setID: "sha256:set-a"},
	})
	published.setCommit(&publishedAuxCommit{setID: "sha256:set-a", refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "map-a", namespace: "haptic"}},
	}})
	assert.Equal(t, "modern-a", publishedSnapshot(t, published)["routes.map"])

	published.setCommit(nil)
	_, err := published.get()
	require.ErrorContains(t, err, "auxiliary publication lost its set ID")
	published.setCommit(&publishedAuxCommit{refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "map-a", namespace: "haptic"}},
	}})
	_, err = published.get()
	require.ErrorContains(t, err, "auxiliary publication lost its set ID")

	published.setForGVR(mapGVR, map[string]publishedAuxFile{
		"map-a": {path: "maps/routes.map", content: "modern-a", setID: "sha256:set-a"},
		"map-b": {path: "maps/routes.map", content: "modern-b", setID: "sha256:set-b"},
	})
	published.setCommit(&publishedAuxCommit{setID: "sha256:set-b", refs: map[string][]publishedAuxRef{
		"mapFiles": {{name: "map-b", namespace: "haptic"}},
	}})
	assert.Equal(t, "modern-b", publishedSnapshot(t, published)["routes.map"])
}

func TestPublishedAuxFilesAllowsInitialMissingParent(t *testing.T) {
	published := newPublishedAuxFiles("haptic")
	published.setCommit(nil)

	require.NoError(t, published.readinessError())
	assert.Empty(t, publishedSnapshot(t, published))
}

// A sensitive file is read back from its Secret's data, under its own key, so
// a new leader keeps rotating the keys the fleet runs.
func TestPublishedAuxFilesReadsSensitiveFilesApart(t *testing.T) {
	published := newPublishedAuxFiles("haptic")
	published.setForGVR(secretGVR.String(), map[string]publishedAuxFile{
		"tickets": {setID: "sha256:set-a", path: "ssl/tls-ticket-keys", sensitive: true},
	})
	published.setCommit(&publishedAuxCommit{setID: "sha256:set-a", refs: map[string][]publishedAuxRef{
		"secretFiles": {{name: "tickets"}},
	}})
	require.ErrorContains(t, published.readinessError(), "committed sensitive file haptic/tickets is unavailable")

	published.setSensitive(map[string]string{"tickets": "k1\n"})
	require.NoError(t, published.readinessError())
	assert.Equal(t, map[string]string{dataplane.SensitiveFileKeyPrefix + "tls-ticket-keys": "k1\n"}, publishedSnapshot(t, published))
}

func TestPublishedSensitiveFilesFromStoreChecksContent(t *testing.T) {
	secret := func(content, checksum string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{
				"name":        "tickets",
				"labels":      map[string]any{"haproxy-haptic.org/type": "sensitive-file"},
				"annotations": map[string]any{"haproxy-haptic.org/compressed": "false"},
			},
			"data": map[string]any{
				"file":     base64.StdEncoding.EncodeToString([]byte(content)),
				"checksum": base64.StdEncoding.EncodeToString([]byte(checksum)),
			},
		}}
	}
	sum := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("k1\n")))

	s := store.NewMemoryStore(1)
	require.NoError(t, s.Add(secret("k1\n", sum), []string{"tickets"}))
	got, err := publishedSensitiveFilesFromStore(s)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"tickets": "k1\n"}, got)

	tampered := store.NewMemoryStore(1)
	require.NoError(t, tampered.Add(secret("k2\n", sum), []string{"tickets"}))
	_, err = publishedSensitiveFilesFromStore(tampered)
	require.Error(t, err)
}
