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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
)

// snapshotRenderRoot stands in for the per-run temp directory the renders point
// their file paths at. Without the substitution every rendered path differs
// between two runs of the same checkout and the comparison says nothing.
const snapshotRenderRoot = "/haptic-render"

const snapshotSecretNamesFile = "secret-files.txt"

// writeSnapshot lays every test's rendered output out as a directory tree, so
// the same corpus rendered from two checkouts can be compared with `diff -r`.
// tempRoot is the run's validation temp directory, normalised away in every
// written file. Skipped tests and tests whose render failed contribute nothing,
// which is itself visible in the diff as a missing directory.
func writeSnapshot(dir, tempRoot string, results *testrunner.TestResults) error {
	normalise := snapshotNormaliser(tempRoot)
	for i := range results.TestResults {
		test := &results.TestResults[i]
		if test.RenderedConfig == "" {
			continue
		}
		testDir := filepath.Join(dir, snapshotName(test.TestName))
		if err := os.MkdirAll(testDir, 0o750); err != nil {
			return fmt.Errorf("creating snapshot directory %s: %w", testDir, err)
		}
		if err := writeSnapshotFile(filepath.Join(testDir, "haproxy.cfg"), normalise(test.RenderedConfig)); err != nil {
			return err
		}
		// Everything HAProxy itself reads. The Kubernetes-side output
		// (k8sResources, status patches) is deliberately left out: it carries
		// wall-clock condition timestamps, so it differs between two runs of
		// the same checkout and would drown the comparison.
		for label, items := range map[string]map[string]string{
			"maps":  test.RenderedMaps,
			"files": test.RenderedFiles,
		} {
			if err := writeSnapshotDir(filepath.Join(testDir, label), items, normalise); err != nil {
				return err
			}
		}
		if err := writeSnapshotNames(filepath.Join(testDir, snapshotSecretNamesFile), test.RenderedSecretNames, normalise); err != nil {
			return err
		}
	}
	return nil
}

func writeSnapshotDir(dir string, items map[string]string, normalise func(string) string) error {
	if len(items) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating snapshot directory %s: %w", dir, err)
	}
	for name, content := range items {
		// Names are normalised too: some are absolute render paths.
		if err := writeSnapshotFile(filepath.Join(dir, snapshotName(normalise(name))), normalise(content)); err != nil {
			return err
		}
	}
	return nil
}

func writeSnapshotNames(path string, names []string, normalise func(string) string) error {
	if len(names) == 0 {
		return nil
	}
	normalised := make([]string, len(names))
	for i, name := range names {
		normalised[i] = normalise(name)
	}
	slices.Sort(normalised)
	return writeSnapshotFile(path, strings.Join(normalised, "\n")+"\n")
}

func writeSnapshotFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing snapshot file %s: %w", path, err)
	}
	return nil
}

// snapshotNormaliser replaces the run's temp directory — including the
// worker/test subdirectory each render was given, which depends on how the
// worker pool happened to schedule it — with a fixed root.
func snapshotNormaliser(tempRoot string) func(string) string {
	if tempRoot == "" {
		return func(content string) string { return content }
	}
	perTest := regexp.MustCompile(regexp.QuoteMeta(tempRoot) + `/worker-\d+/test-\d+`)
	return func(content string) string {
		content = perTest.ReplaceAllLiteralString(content, snapshotRenderRoot)
		return strings.ReplaceAll(content, tempRoot, snapshotRenderRoot)
	}
}

// snapshotName flattens a rendered artifact's name into one path segment. Map
// paths and certificate paths carry separators that would otherwise scatter the
// snapshot over nested directories — or escape it.
func snapshotName(name string) string {
	flat := strings.NewReplacer("/", "_", `\`, "_").Replace(name)
	flat = strings.TrimLeft(flat, ".")
	if flat == "" {
		return "unnamed"
	}
	return flat
}
