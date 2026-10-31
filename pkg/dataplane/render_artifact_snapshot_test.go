// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dataplane

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
)

func TestAuxiliaryFileSnapshotRoundTripIsDetached(t *testing.T) {
	input := auxiliaryFileSnapshotFixture()
	authority := renderartifact.NewAuthority()
	snapshot, err := BuildAuxiliaryFileSnapshot(authority, nil, input)
	require.NoError(t, err)
	require.NoError(t, authority.ValidateSnapshot(snapshot))

	input.MapFiles[0].Path = "poison.map"
	input.MapFiles[0].Content = "poison"
	input.GeneralFiles[0].Filename = "poison"
	input.GeneralFiles[0].Content = "poison"
	input.GeneralFiles[0].ReloadOnPush = boolPointer(true)

	first, err := MaterializeAuxiliaryFileSnapshot(snapshot)
	require.NoError(t, err)
	assertAuxiliarySnapshotFixture(t, first)

	first.MapFiles[0].Path = "poison.map"
	first.MapFiles[0].Content = "poison"
	first.GeneralFiles[0].Filename = "poison"
	first.GeneralFiles[0].Content = "poison"
	*first.GeneralFiles[1].ReloadOnPush = !*first.GeneralFiles[1].ReloadOnPush

	second, err := MaterializeAuxiliaryFileSnapshot(snapshot)
	require.NoError(t, err)
	assertAuxiliarySnapshotFixture(t, second)
	require.Nil(t, first.GeneralFiles[0].ReloadOnPush)
	require.Nil(t, second.GeneralFiles[0].ReloadOnPush)
	for index := 1; index < len(second.GeneralFiles); index++ {
		assert.NotSame(t, first.GeneralFiles[index].ReloadOnPush, second.GeneralFiles[index].ReloadOnPush)
	}
}

func TestAuxiliaryFileSnapshotReusesOnlyExactPreviousOutput(t *testing.T) {
	input := auxiliaryFileSnapshotFixture()
	authority := renderartifact.NewAuthority()
	first, err := BuildAuxiliaryFileSnapshot(authority, nil, input)
	require.NoError(t, err)
	second, err := BuildAuxiliaryFileSnapshot(authority, first, CloneAuxiliaryFiles(input))
	require.NoError(t, err)
	assert.Same(t, first, second)

	changed := CloneAuxiliaryFiles(input)
	changed.MapFiles[0].Content = "changed"
	third, err := BuildAuxiliaryFileSnapshot(authority, second, changed)
	require.NoError(t, err)
	assert.NotSame(t, second, third)
	equal, err := SnapshotContentEqual("config", second, "config", third)
	require.NoError(t, err)
	assert.False(t, equal)

	_, err = BuildAuxiliaryFileSnapshot(authority, buildForeignSnapshot(t, input), input)
	require.Error(t, err)
}

func TestSnapshotContentEqualUsesExactBytesAndMetadata(t *testing.T) {
	input := auxiliaryFileSnapshotFixture()
	left := buildForeignSnapshot(t, input)
	right := buildForeignSnapshot(t, reverseAuxiliaryFiles(input))
	equal, err := SnapshotContentEqual("config", left, "config", right)
	require.NoError(t, err)
	assert.True(t, equal)

	equal, err = SnapshotContentEqual("left", left, "right", right)
	require.NoError(t, err)
	assert.False(t, equal)

	changed := CloneAuxiliaryFiles(input)
	changed.GeneralFiles[1].ReloadOnPush = boolPointer(false)
	changedSnapshot := buildForeignSnapshot(t, changed)
	leftChecksum, err := ComputeSnapshotContentChecksum("config", left)
	require.NoError(t, err)
	changedChecksum, err := ComputeSnapshotContentChecksum("config", changedSnapshot)
	require.NoError(t, err)
	assert.Equal(t, leftChecksum, changedChecksum)
	equal, err = SnapshotContentEqual("config", left, "config", changedSnapshot)
	require.NoError(t, err)
	assert.False(t, equal)

	caMetadata := CloneAuxiliaryFiles(input)
	caMetadata.GeneralFiles[0].ReloadOnPush = boolPointer(false)
	caSnapshot := buildForeignSnapshot(t, caMetadata)
	equal, err = SnapshotContentEqual("config", left, "config", caSnapshot)
	require.NoError(t, err)
	assert.True(t, equal)
}

func TestSnapshotContentChecksumMatchesSortedLegacyOrder(t *testing.T) {
	input := auxiliaryFileSnapshotFixture()
	snapshot := buildForeignSnapshot(t, input)
	legacy := CloneAuxiliaryFiles(input)
	legacy.Sort()

	want := ComputeContentChecksum("global\n", legacy)
	got, err := ComputeSnapshotContentChecksum("global\n", snapshot)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	reversed := buildForeignSnapshot(t, reverseAuxiliaryFiles(input))
	reversedChecksum, err := ComputeSnapshotContentChecksum("global\n", reversed)
	require.NoError(t, err)
	assert.Equal(t, got, reversedChecksum)

	equal, err := SnapshotContentEqual("global\n", snapshot, "global\n", reversed)
	require.NoError(t, err)
	assert.True(t, equal)
}

func TestAuxiliaryFileSnapshotRuntimePathResolver(t *testing.T) {
	input := auxiliaryFileSnapshotFixture()
	defaultSnapshot := buildForeignSnapshot(t, input)
	var calls []string
	resolved, err := BuildAuxiliaryFileSnapshotWithRuntimePaths(
		renderartifact.NewAuthority(),
		nil,
		input,
		func(family renderartifact.Family, filePath string) (string, error) {
			calls = append(calls, fmt.Sprintf("%d:%s", family, filePath))
			return fmt.Sprintf("runtime/%d/%s", family, filePath), nil
		},
	)
	require.NoError(t, err)
	assert.Len(t, calls, len(input.MapFiles)+len(input.SSLCertificates)+len(input.CRTListFiles))

	err = resolved.Walk(func(artifact *renderartifact.Artifact) error {
		descriptor, descriptorErr := artifact.Descriptor()
		require.NoError(t, descriptorErr)
		switch descriptor.Family {
		case renderartifact.Map, renderartifact.Certificate, renderartifact.CRTList:
			assert.Equal(t, fmt.Sprintf("runtime/%d/%s", descriptor.Family, descriptor.Path), descriptor.RuntimePath)
		case renderartifact.General, renderartifact.GeneralCA, renderartifact.CA, renderartifact.Secret:
			assert.Equal(t, descriptor.Path, descriptor.RuntimePath)
		default:
			t.Fatalf("unexpected family %d", descriptor.Family)
		}
		return nil
	})
	require.NoError(t, err)

	equal, err := SnapshotContentEqual("config", defaultSnapshot, "config", resolved)
	require.NoError(t, err)
	assert.False(t, equal)
	defaultChecksum, err := ComputeSnapshotContentChecksum("config", defaultSnapshot)
	require.NoError(t, err)
	resolvedChecksum, err := ComputeSnapshotContentChecksum("config", resolved)
	require.NoError(t, err)
	assert.Equal(t, defaultChecksum, resolvedChecksum)

	defaultFiles, err := MaterializeAuxiliaryFileSnapshot(defaultSnapshot)
	require.NoError(t, err)
	resolvedFiles, err := MaterializeAuxiliaryFileSnapshot(resolved)
	require.NoError(t, err)
	assert.Equal(t, defaultFiles, resolvedFiles)

	sentinel := errors.New("resolution failed")
	_, err = BuildAuxiliaryFileSnapshotWithRuntimePaths(
		renderartifact.NewAuthority(),
		nil,
		input,
		func(renderartifact.Family, string) (string, error) { return "", sentinel },
	)
	require.ErrorIs(t, err, sentinel)
}

func TestSnapshotCurrentFilesPreservesLegacyProjection(t *testing.T) {
	reload := false
	input := &AuxiliaryFiles{
		MapFiles: []auxiliaryfiles.MapFile{
			{Path: "/maps/shared", Content: "map"},
			{Path: "/maps/map-only", Content: "map-only"},
		},
		GeneralFiles: []auxiliaryfiles.GeneralFile{
			{Filename: "general-id", Path: "/general/shared", Content: "general", ReloadOnPush: &reload},
			{Filename: "trust-id", Path: "/general/trust.pem", Content: "trust", IsCaFile: true},
		},
		CRTListFiles: []auxiliaryfiles.CRTListFile{
			{Path: "/crt/shared", Content: "crt"},
			{Path: "/crt/list-only", Content: "list-only"},
		},
		SSLCertificates: []auxiliaryfiles.SSLCertificate{{Path: "/ssl/cert.pem", Content: "secret"}},
		SSLCaFiles:      []auxiliaryfiles.SSLCaFile{{Path: "/ca/ca.pem", Content: "secret-ca"}},
		SecretFiles:     []auxiliaryfiles.SecretFile{{Path: "secrets/client-secret", Content: "hunter2", Seal: "seal"}},
	}
	legacy := CloneAuxiliaryFiles(input)
	legacy.Sort()
	snapshot := buildForeignSnapshot(t, input)

	got, err := SnapshotCurrentFiles(snapshot)
	require.NoError(t, err)
	assert.Equal(t, legacy.CurrentFiles(), got)
	assert.Equal(t, "crt", got["shared"])
	assert.NotContains(t, got, "cert.pem")
	assert.NotContains(t, got, "ca.pem")
	assert.NotContains(t, got, "trust.pem")
	assert.NotContains(t, got, "client-secret")

	got["shared"] = "poison"
	again, err := SnapshotCurrentFiles(snapshot)
	require.NoError(t, err)
	assert.Equal(t, "crt", again["shared"])
}

func TestAuxiliaryFileSnapshotAPIsRejectInvalidSnapshots(t *testing.T) {
	valid := buildForeignSnapshot(t, auxiliaryFileSnapshotFixture())
	poisoned := *valid

	_, err := MaterializeAuxiliaryFileSnapshot(nil)
	require.Error(t, err)
	_, err = MaterializeAuxiliaryFileSnapshot(&poisoned)
	require.Error(t, err)

	_, err = ComputeSnapshotContentChecksum("config", nil)
	require.Error(t, err)
	_, err = ComputeSnapshotContentChecksum("config", &poisoned)
	require.Error(t, err)

	_, err = SnapshotCurrentFiles(nil)
	require.Error(t, err)
	_, err = SnapshotCurrentFiles(&poisoned)
	require.Error(t, err)

	_, err = SnapshotContentEqual("different", valid, "config", nil)
	require.Error(t, err)
	_, err = SnapshotContentEqual("config", &poisoned, "config", valid)
	require.Error(t, err)
	_, err = SnapshotContentEqual("config", valid, "config", &poisoned)
	require.Error(t, err)
}

func TestAuxiliaryFileSnapshotEmptyIsAuthenticated(t *testing.T) {
	authority := renderartifact.NewAuthority()
	empty, err := BuildAuxiliaryFileSnapshot(authority, nil, nil)
	require.NoError(t, err)
	require.NoError(t, authority.ValidateSnapshot(empty))
	length, err := empty.Len()
	require.NoError(t, err)
	assert.Zero(t, length)

	materialized, err := MaterializeAuxiliaryFileSnapshot(empty)
	require.NoError(t, err)
	require.NotNil(t, materialized)
	assert.Empty(t, materialized.GeneralFiles)

	checksum, err := ComputeSnapshotContentChecksum("config", empty)
	require.NoError(t, err)
	assert.Equal(t, ComputeContentChecksum("config", &AuxiliaryFiles{}), checksum)

	foreign, err := BuildAuxiliaryFileSnapshot(renderartifact.NewAuthority(), nil, &AuxiliaryFiles{})
	require.NoError(t, err)
	equal, err := SnapshotContentEqual("config", empty, "config", foreign)
	require.NoError(t, err)
	assert.True(t, equal)

	current, err := SnapshotCurrentFiles(empty)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.Empty(t, current)
}

func TestMaterializationIgnoresRuntimePath(t *testing.T) {
	authority := renderartifact.NewAuthority()
	builder, err := renderartifact.NewBuilder(authority, nil)
	require.NoError(t, err)
	require.NoError(t, builder.Add(renderartifact.Descriptor{
		Family:      renderartifact.Map,
		Path:        "legacy/routes.map",
		RuntimePath: "runtime/routes.map",
	}, renderartifact.NewLiteralContent("safe")))
	snapshot, err := builder.Build()
	require.NoError(t, err)

	files, err := MaterializeAuxiliaryFileSnapshot(snapshot)
	require.NoError(t, err)
	require.Len(t, files.MapFiles, 1)
	assert.Equal(t, "legacy/routes.map", files.MapFiles[0].Path)
}

func TestAuxiliaryFileSnapshotAdaptersAreConcurrentReadSafe(t *testing.T) {
	left := buildForeignSnapshot(t, auxiliaryFileSnapshotFixture())
	right := buildForeignSnapshot(t, reverseAuxiliaryFiles(auxiliaryFileSnapshotFixture()))
	const workers = 32
	errCh := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := verifyConcurrentSnapshotAdapters(left, right); err != nil {
				errCh <- err
			}
		}()
	}
	group.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
}

func verifyConcurrentSnapshotAdapters(left, right *renderartifact.Snapshot) error {
	for range 20 {
		equal, err := SnapshotContentEqual("config", left, "config", right)
		if err != nil {
			return err
		}
		if !equal {
			return errors.New("exact comparison rejected equal snapshots")
		}
		if _, err := ComputeSnapshotContentChecksum("config", left); err != nil {
			return err
		}
		if _, err := SnapshotCurrentFiles(left); err != nil {
			return err
		}
		if _, err := MaterializeAuxiliaryFileSnapshot(left); err != nil {
			return err
		}
	}
	return nil
}

func auxiliaryFileSnapshotFixture() *AuxiliaryFiles {
	return &AuxiliaryFiles{
		GeneralFiles: []auxiliaryfiles.GeneralFile{
			{Filename: "a-ca.pem", Path: "general/a-ca.pem", Content: "ca-general", IsCaFile: true},
			{Filename: "z.http", Path: "general/z.http", Content: "general", ReloadOnPush: boolPointer(true)},
			{Filename: "m.http", Path: "general/m.http", Content: "sidecar", ReloadOnPush: boolPointer(false)},
		},
		MapFiles: []auxiliaryfiles.MapFile{
			{Path: "maps/z.map", Content: "z value\n"},
			{Path: "maps/a.map", Content: "a value\n"},
		},
		SSLCertificates: []auxiliaryfiles.SSLCertificate{
			{Path: "z/a.pem", Content: "certificate-a"},
			{Path: "a/z.pem", Content: "certificate-z"},
		},
		SSLCaFiles: []auxiliaryfiles.SSLCaFile{
			{Path: "z/a-ca.pem", Content: "ca-a"},
			{Path: "a/z-ca.pem", Content: "ca-z"},
		},
		CRTListFiles: []auxiliaryfiles.CRTListFile{
			{Path: "z/a.list", Content: "list-a"},
			{Path: "a/z.list", Content: "list-z"},
		},
		SecretFiles: []auxiliaryfiles.SecretFile{
			{Path: "secrets/z", Content: "secret-z", Seal: "seal-z"},
			{Path: "secrets/a", Content: "secret-a", Seal: "seal-a"},
		},
	}
}

func assertAuxiliarySnapshotFixture(t *testing.T, files *AuxiliaryFiles) {
	t.Helper()
	require.Len(t, files.GeneralFiles, 3)
	assert.Equal(t, "a-ca.pem", files.GeneralFiles[0].Filename)
	assert.True(t, files.GeneralFiles[0].IsCaFile)
	assert.Nil(t, files.GeneralFiles[0].ReloadOnPush)
	assert.Equal(t, "ca-general", files.GeneralFiles[0].Content)
	assert.Equal(t, "m.http", files.GeneralFiles[1].Filename)
	assert.False(t, files.GeneralFiles[1].ReloadsOnPush())
	assert.Equal(t, "z.http", files.GeneralFiles[2].Filename)
	assert.True(t, files.GeneralFiles[2].ReloadsOnPush())
	require.Len(t, files.MapFiles, 2)
	assert.Equal(t, "maps/a.map", files.MapFiles[0].Path)
	assert.Equal(t, "a value\n", files.MapFiles[0].Content)
	require.Len(t, files.SSLCertificates, 2)
	assert.Equal(t, "a/z.pem", files.SSLCertificates[0].Path)
	require.Len(t, files.SSLCaFiles, 2)
	assert.Equal(t, "a/z-ca.pem", files.SSLCaFiles[0].Path)
	require.Len(t, files.CRTListFiles, 2)
	assert.Equal(t, "a/z.list", files.CRTListFiles[0].Path)
	assert.Equal(t, []auxiliaryfiles.SecretFile{
		{Path: "secrets/a", Content: "secret-a", Seal: "seal-a"},
		{Path: "secrets/z", Content: "secret-z", Seal: "seal-z"},
	}, files.SecretFiles)
}

func buildForeignSnapshot(t *testing.T, files *AuxiliaryFiles) *renderartifact.Snapshot {
	t.Helper()
	snapshot, err := BuildAuxiliaryFileSnapshot(renderartifact.NewAuthority(), nil, files)
	require.NoError(t, err)
	return snapshot
}

func reverseAuxiliaryFiles(files *AuxiliaryFiles) *AuxiliaryFiles {
	reversed := CloneAuxiliaryFiles(files)
	slices.Reverse(reversed.GeneralFiles)
	slices.Reverse(reversed.MapFiles)
	slices.Reverse(reversed.SSLCertificates)
	slices.Reverse(reversed.SSLCaFiles)
	slices.Reverse(reversed.CRTListFiles)
	slices.Reverse(reversed.SecretFiles)
	return reversed
}

func boolPointer(value bool) *bool {
	return &value
}

func BenchmarkAuxiliaryFileSnapshot3000(b *testing.B) {
	files := &AuxiliaryFiles{MapFiles: make([]auxiliaryfiles.MapFile, 3000)}
	for index := range files.MapFiles {
		files.MapFiles[index] = auxiliaryfiles.MapFile{
			Path:    fmt.Sprintf("maps/%06d.map", index),
			Content: fmt.Sprintf("key-%06d value-%06d\n", index, index),
		}
	}
	snapshot, err := BuildAuxiliaryFileSnapshot(renderartifact.NewAuthority(), nil, files)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("checksum", func(b *testing.B) {
		benchmarkAuxiliarySnapshotChecksum(b, snapshot)
	})
	b.Run("same-root-equality", func(b *testing.B) {
		benchmarkAuxiliarySnapshotSameRoot(b, snapshot)
	})
	b.Run("current-files", func(b *testing.B) {
		benchmarkAuxiliarySnapshotCurrentFiles(b, snapshot)
	})
	b.Run("materialize", func(b *testing.B) {
		benchmarkAuxiliarySnapshotMaterialize(b, snapshot)
	})
}

func benchmarkAuxiliarySnapshotChecksum(b *testing.B, snapshot *renderartifact.Snapshot) {
	b.Helper()
	b.ReportAllocs()
	for range b.N {
		benchmarkAuxiliaryChecksum, benchmarkAuxiliaryError =
			ComputeSnapshotContentChecksum("global\n", snapshot)
		if benchmarkAuxiliaryError != nil {
			b.Fatal(benchmarkAuxiliaryError)
		}
	}
}

func benchmarkAuxiliarySnapshotSameRoot(b *testing.B, snapshot *renderartifact.Snapshot) {
	b.Helper()
	b.ReportAllocs()
	for range b.N {
		benchmarkAuxiliaryEqual, benchmarkAuxiliaryError =
			SnapshotContentEqual("global\n", snapshot, "global\n", snapshot)
		if benchmarkAuxiliaryError != nil {
			b.Fatal(benchmarkAuxiliaryError)
		}
	}
}

func benchmarkAuxiliarySnapshotCurrentFiles(b *testing.B, snapshot *renderartifact.Snapshot) {
	b.Helper()
	b.ReportAllocs()
	for range b.N {
		benchmarkAuxiliaryValue, benchmarkAuxiliaryError = SnapshotCurrentFiles(snapshot)
		if benchmarkAuxiliaryError != nil {
			b.Fatal(benchmarkAuxiliaryError)
		}
	}
}

func benchmarkAuxiliarySnapshotMaterialize(b *testing.B, snapshot *renderartifact.Snapshot) {
	b.Helper()
	b.ReportAllocs()
	for range b.N {
		benchmarkAuxiliaryValue, benchmarkAuxiliaryError = MaterializeAuxiliaryFileSnapshot(snapshot)
		if benchmarkAuxiliaryError != nil {
			b.Fatal(benchmarkAuxiliaryError)
		}
	}
}

var (
	benchmarkAuxiliaryChecksum string
	benchmarkAuxiliaryEqual    bool
	benchmarkAuxiliaryValue    any
	benchmarkAuxiliaryError    error
)
