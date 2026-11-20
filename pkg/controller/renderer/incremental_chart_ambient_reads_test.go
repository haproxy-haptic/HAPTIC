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

package renderer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

var (
	chartLiteralRenderPattern = regexp.MustCompile(`\b(?:render|import)\s+"([^"]+)"`)
	chartRenderGlobPattern    = regexp.MustCompile(`\brender_glob\s+"([^"]+)"`)
	chartAmbientReadPattern   = regexp.MustCompile(
		`resources\.[A-Za-z0-9_-]+\.(?:GetSingle|Fetch|List|APIVersion)\s*\(|` +
			`controller\.(?:GetSingle|Fetch|List)\s*\(|http\.Fetch\s*\(`,
	)
)

type chartAmbientAuditSnippet struct {
	Template    string         `yaml:"template"`
	Incremental map[string]any `yaml:"incremental"`
}

type chartAmbientAuditRoot struct {
	Template string `yaml:"template"`
}

type chartAmbientAuditLibrary struct {
	HelmLoad struct {
		Inject []struct {
			Path string `yaml:"path"`
			From string `yaml:"from"`
		} `yaml:"inject"`
	} `yaml:"_helm_load"`
	TemplateSnippets map[string]chartAmbientAuditSnippet `yaml:"templateSnippets"`
	Maps             map[string]chartAmbientAuditRoot    `yaml:"maps"`
	Files            map[string]chartAmbientAuditRoot    `yaml:"files"`
	SSLCertificates  map[string]chartAmbientAuditRoot    `yaml:"sslCertificates"`
	K8sResources     map[string]chartAmbientAuditRoot    `yaml:"k8sResources"`
	HAProxyConfig    chartAmbientAuditRoot               `yaml:"haproxyConfig"`
}

type chartAmbientAuditNode struct {
	name        string
	template    string
	incremental bool
	path        string
	// alternates are snippets the chart loader injects in place of this one.
	alternates []string
}

type chartAmbientAuditTraversal struct {
	reachable map[string]string
	problems  []string
}

func TestBundledChartAmbientReadsStayBehindExactBoundaries(t *testing.T) {
	t.Parallel()
	nodes, roots := loadBundledChartAmbientAudit(t)

	rootTraversal := traverseChartAmbientAudit(nodes, roots, false)
	incrementalRoots := make(map[string]string)
	for name, node := range nodes {
		if node.incremental {
			incrementalRoots["incremental "+name] = node.template
		}
	}
	incrementalTraversal := traverseChartAmbientAudit(nodes, incrementalRoots, true)

	exactCycleReaders := map[string][]string{
		"features-011-ip-blocklist": {"http.Fetch("},
		"util-emit-gatewayclass":    {"resources.gatewayclasses.APIVersion("},
		"util-waf-crs-source":       {"http.Fetch("},
	}
	var failures []string
	failures = append(failures, rootTraversal.problems...)
	for name, node := range nodes {
		if node.incremental || !chartAmbientReadPattern.MatchString(node.template) {
			continue
		}
		if chain, reached := rootTraversal.reachable[name]; reached {
			allowed, exception := exactCycleReaders[name]
			reads := slices.Compact(chartAmbientReadPattern.FindAllString(node.template, -1))
			if !exception || !slices.Equal(reads, allowed) {
				failures = append(failures, fmt.Sprintf(
					"%s: static root chain %s reaches ambient reads %v", node.path, chain, reads,
				))
			}
			continue
		}
		if _, reached := incrementalTraversal.reachable[name]; !reached {
			failures = append(failures, fmt.Sprintf(
				"%s: ambient reader %s is neither root-classified nor component-reachable", node.path, name,
			))
		}
	}
	slices.Sort(failures)
	require.Empty(t, failures, strings.Join(failures, "\n"))
	for name := range exactCycleReaders {
		_, reached := rootTraversal.reachable[name]
		require.Truef(t, reached, "exact-cycle reader %s is no longer root-reachable", name)
	}
}

func collectChartAmbientAuditFile(
	chartRoot, path string,
	entry fs.DirEntry,
	walkErr error,
	nodes map[string]chartAmbientAuditNode,
	roots map[string]string,
) error {
	if walkErr != nil {
		return walkErr
	}
	if entry.IsDir() {
		if entry.Name() == "tests" {
			return filepath.SkipDir
		}
		return nil
	}
	if filepath.Ext(path) != ".yaml" {
		return nil
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		return readErr
	}
	var library chartAmbientAuditLibrary
	if unmarshalErr := yaml.Unmarshal(content, &library); unmarshalErr != nil {
		return fmt.Errorf("parsing %s: %w", path, unmarshalErr)
	}
	relative, relativeErr := filepath.Rel(chartRoot, path)
	if relativeErr != nil {
		return relativeErr
	}
	for name, snippet := range library.TemplateSnippets {
		if previous, duplicate := nodes[name]; duplicate {
			return fmt.Errorf("snippet %s is defined by both %s and %s", name, previous.path, relative)
		}
		nodes[name] = chartAmbientAuditNode{
			name: name, template: snippet.Template, incremental: snippet.Incremental != nil, path: relative,
		}
	}
	const snippetPath = "templateSnippets."
	for _, inject := range library.HelmLoad.Inject {
		target, isSnippet := strings.CutPrefix(inject.Path, snippetPath)
		source, fromSnippet := strings.CutPrefix(inject.From, snippetPath)
		if !isSnippet || !fromSnippet {
			continue
		}
		node, found := nodes[target]
		if !found {
			return fmt.Errorf("%s injects %s over undefined snippet %s", relative, source, target)
		}
		node.alternates = append(node.alternates, source)
		nodes[target] = node
	}
	appendRoots := func(kind string, entries map[string]chartAmbientAuditRoot) {
		for name, root := range entries {
			roots[kind+" "+relative+":"+name] = root.Template
		}
	}
	appendRoots("map", library.Maps)
	appendRoots("file", library.Files)
	appendRoots("certificate", library.SSLCertificates)
	appendRoots("resource", library.K8sResources)
	if library.HAProxyConfig.Template != "" {
		roots["main "+relative] = library.HAProxyConfig.Template
	}
	return nil
}

func loadBundledChartAmbientAudit(t *testing.T) (nodes map[string]chartAmbientAuditNode, roots map[string]string) {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
	nodes = make(map[string]chartAmbientAuditNode)
	roots = make(map[string]string)
	err := filepath.WalkDir(chartRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		return collectChartAmbientAuditFile(chartRoot, path, entry, walkErr, nodes, roots)
	})
	require.NoError(t, err)
	require.NotEmpty(t, nodes)
	require.NotEmpty(t, roots)
	return nodes, roots
}

func traverseChartAmbientAudit(
	nodes map[string]chartAmbientAuditNode,
	roots map[string]string,
	enterIncremental bool,
) chartAmbientAuditTraversal {
	type pendingNode struct {
		name  string
		chain string
	}
	result := chartAmbientAuditTraversal{reachable: make(map[string]string)}
	queue := make([]pendingNode, 0)
	for rootName, template := range roots {
		if chartAmbientReadPattern.MatchString(template) {
			result.problems = append(result.problems, rootName+": root template reads ambient inputs directly")
		}
		for _, name := range chartAmbientAuditReferences(template, nodes) {
			queue = append(queue, pendingNode{name: name, chain: rootName + " -> " + name})
		}
	}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if _, seen := result.reachable[current.name]; seen {
			continue
		}
		node, found := nodes[current.name]
		if !found {
			continue
		}
		result.reachable[current.name] = current.chain
		for _, alternate := range node.alternates {
			queue = append(queue, pendingNode{name: alternate, chain: current.chain + " (as " + alternate + ")"})
		}
		if node.incremental && !enterIncremental {
			continue
		}
		for _, name := range chartAmbientAuditReferences(node.template, nodes) {
			queue = append(queue, pendingNode{name: name, chain: current.chain + " -> " + name})
		}
	}
	return result
}

func chartAmbientAuditReferences(
	template string,
	nodes map[string]chartAmbientAuditNode,
) []string {
	result := make([]string, 0)
	for _, match := range chartLiteralRenderPattern.FindAllStringSubmatch(template, -1) {
		result = append(result, match[1])
	}
	for _, match := range chartRenderGlobPattern.FindAllStringSubmatch(template, -1) {
		for name := range nodes {
			matched, err := filepath.Match(match[1], name)
			if err == nil && matched {
				result = append(result, name)
			}
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}
