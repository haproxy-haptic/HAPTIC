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

// The differential gate: what a render DECLARES about its output (the plan)
// must be what the render actually EMITTED. The deploy side computes runtime
// operations from the plan alone and never reads haproxy.cfg back, so a macro
// that under-describes its own output would silently ship a wrong fleet.
//
// The comparison is total, not field-wise: every declared backend, every
// section byte and every map entry is re-derived from the rendered artifacts
// with an INDEPENDENT reader — client-native's config parser for the
// configuration, a naive key/rest-of-line split for the map files — and
// compared against the record. client-native is imported here and nowhere in
// the production path; the plan exists precisely so the controller never has to
// parse HAProxy configuration.
//
// The test lives in cmd/haptic because this is the only package that can render
// the bundled chart in-process, which is where the fixture corpus comes from.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	cnparser "github.com/haproxytech/client-native/v6/config-parser"
	cnparams "github.com/haproxytech/client-native/v6/config-parser/params"
	cntypes "github.com/haproxytech/client-native/v6/config-parser/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/dataplanetest"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// planMismatches returns one line per disagreement between the plan and the
// artifacts it was produced with. An empty result is the gate passing.
func planMismatches(t *testing.T, config string, mapContents map[string]string, plan *renderplan.Plan) []string {
	t.Helper()
	parsed := parseBackendServers(t, config)

	mismatches := backendSetMismatches(parsed, plan)
	mismatches = append(mismatches, declaredServerMismatches(parsed, plan)...)
	mismatches = append(mismatches, sectionPartitionMismatches(config, plan)...)
	mismatches = append(mismatches, mapEntryMismatches(mapContents, plan)...)
	return mismatches
}

// parsedServer is one server line as client-native read it back.
type parsedServer struct {
	Name    string
	Address string
	// Options is each server keyword mapped to its argument, "" for a bare
	// keyword such as `disabled`.
	Options map[string]string
	Comment string
	// Template is a `server-template` line's slot count, 0 for a `server` line.
	Template int
}

// parsedBackend is one backend section as client-native read it back.
type parsedBackend struct {
	Servers []parsedServer
	// DefaultServer is the `default-server` line's keywords, nil when absent.
	DefaultServer map[string]string
}

// parseBackendServers reads the backend sections and their server lines out of
// the rendered configuration, with no help from the plan.
func parseBackendServers(t *testing.T, config string) map[string]parsedBackend {
	t.Helper()
	p, err := cnparser.New()
	require.NoError(t, err)
	require.NoError(t, p.Process(strings.NewReader(config)))

	sections, err := p.SectionsGet(cnparser.Backends)
	require.NoError(t, err)

	backends := make(map[string]parsedBackend, len(sections))
	for _, name := range sections {
		var backend parsedBackend
		// A dynamic backend carries `default-server` in its profile (`from
		// <defaults>`), not in the section, so resolve the profile chain — the
		// same inheritance HAProxy applies. A structural backend carries it in a
		// section too (its profile, or its own body when hand-built); either way
		// those bytes are covered by that section's TextDigest.
		backend.DefaultServer = resolveDefaultServer(t, p, cnparser.Backends, name)
		// No server line in this backend — an empty backend is legal.
		if data, err := p.Get(cnparser.Backends, name, "server"); err == nil {
			lines, ok := data.([]cntypes.Server)
			require.Truef(t, ok, "backend %q: server data is %T", name, data)
			for i := range lines {
				backend.Servers = append(backend.Servers, parsedServer{
					Name:    lines[i].Name,
					Address: lines[i].Address,
					Options: keywordOptions(lines[i].Params),
					Comment: strings.TrimSpace(lines[i].Comment),
				})
			}
		}
		if data, err := p.Get(cnparser.Backends, name, "server-template"); err == nil {
			lines, ok := data.([]cntypes.ServerTemplate)
			require.Truef(t, ok, "backend %q: server-template data is %T", name, data)
			for i := range lines {
				slots, err := strconv.Atoi(lines[i].NumOrRange)
				require.NoErrorf(t, err, "backend %q: server-template %q slot count", name, lines[i].Prefix)
				address := lines[i].Fqdn
				if lines[i].Port != 0 {
					address = fmt.Sprintf("%s:%d", address, lines[i].Port)
				}
				backend.Servers = append(backend.Servers, parsedServer{
					Name:     lines[i].Prefix,
					Address:  address,
					Options:  keywordOptions(lines[i].Params),
					Comment:  strings.TrimSpace(lines[i].Comment),
					Template: slots,
				})
			}
		}
		backends[name] = backend
	}
	return backends
}

// resolveDefaultServer returns the `default-server` keywords in effect for a
// section: its own line, or the first one found walking the `from <defaults>`
// chain (a dynamic backend's default-server lives in its profile). nil when
// neither the section nor any ancestor declares one.
func resolveDefaultServer(t *testing.T, p cnparser.Parser, sectionType cnparser.Section, name string) map[string]string {
	t.Helper()
	if ds := readDefaultServer(t, p, sectionType, name); ds != nil {
		return ds
	}
	from, err := p.SectionsDefaultsFromGet(sectionType, name)
	seen := map[string]bool{}
	for err == nil && from != "" && !seen[from] {
		seen[from] = true
		if ds := readDefaultServer(t, p, cnparser.Defaults, from); ds != nil {
			return ds
		}
		from, err = p.SectionsDefaultsFromGet(cnparser.Defaults, from)
	}
	return nil
}

func readDefaultServer(t *testing.T, p cnparser.Parser, sectionType cnparser.Section, name string) map[string]string {
	t.Helper()
	data, err := p.Get(sectionType, name, "default-server")
	if err != nil {
		return nil
	}
	lines, ok := data.([]cntypes.DefaultServer)
	require.Truef(t, ok, "%s %q: default-server data is %T", sectionType, name, data)
	require.Lenf(t, lines, 1, "%s %q: one default-server line expected", sectionType, name)
	return keywordOptions(lines[0].Params)
}

func keywordOptions(params []cnparams.ServerOption) map[string]string {
	options := make(map[string]string, len(params))
	for _, param := range params {
		keyword, argument, _ := strings.Cut(param.String(), " ")
		options[keyword] = argument
	}
	return options
}

// keywordArgs is the record's keyword list in the same shape client-native
// reports the parsed line: keyword → joined arguments.
func keywordArgs(args []renderplan.KeywordArg) map[string]string {
	options := make(map[string]string, len(args))
	for _, arg := range args {
		options[arg.Name] = strings.Join(arg.Args, " ")
	}
	return options
}

func backendSetMismatches(parsed map[string]parsedBackend, plan *renderplan.Plan) []string {
	var mismatches []string
	for name := range parsed {
		if _, declared := plan.Backends[name]; !declared {
			mismatches = append(mismatches, fmt.Sprintf("backend %q is in the config but not in the plan", name))
		}
	}
	for name := range plan.Backends {
		if _, emitted := parsed[name]; !emitted {
			mismatches = append(mismatches, fmt.Sprintf("backend %q is in the plan but not in the config", name))
		}
	}
	sort.Strings(mismatches)
	return mismatches
}

// declaredServerMismatches compares every declared backend's servers against
// the server lines the configuration actually carries.
func declaredServerMismatches(parsed map[string]parsedBackend, plan *renderplan.Plan) []string {
	var mismatches []string
	for _, name := range sortedKeys(plan.Backends) {
		record := plan.Backends[name]
		backend, found := parsed[name]
		if !found {
			mismatches = append(mismatches, fmt.Sprintf("backend %q is in the plan but not in the config", name))
			continue
		}
		mismatches = append(mismatches, defaultServerMismatches(name, record.Shape, record.DefaultServer, backend.DefaultServer)...)
		if len(record.Servers) != len(backend.Servers) {
			mismatches = append(mismatches, fmt.Sprintf("backend %q declares %d servers, the config has %d",
				name, len(record.Servers), len(backend.Servers)))
			continue
		}
		for i := range record.Servers {
			mismatches = append(mismatches, serverMismatches(name, &record.Servers[i], &backend.Servers[i])...)
		}
	}
	return mismatches
}

// defaultServerMismatches holds a dynamic backend's declared `defaultServer`
// against the parsed line, both ways. It does NOT skip an empty declaration: a
// `default-server` carrying keywords the record cannot reconstruct is exactly how
// per-server keywords could hide in the profile text, where the deploy side
// (which reads only the record) can't compose a correct `add server`.
// optionMismatches reports every parsed keyword the record fails to declare, so a
// hidden keyword fails CI. A dynamic backend with no default-server anywhere has
// both sides empty and passes.
//
// A structural backend is exempt: it never gets `add server` (create/delete/body
// change reload), so its default-server — in its profile, or its own body when
// hand-built — is covered by that section's TextDigest, not by the structured
// field.
func defaultServerMismatches(backend, shape string, declared []renderplan.KeywordArg, parsed map[string]string) []string {
	if shape != renderplan.ShapeDynamic {
		return nil
	}
	return optionMismatches(fmt.Sprintf("backend %q default-server", backend), keywordArgs(declared), parsed)
}

// optionMismatches compares two keyword → argument maps both ways.
func optionMismatches(where string, declared, parsed map[string]string) []string {
	var mismatches []string
	for _, keyword := range sortedKeys(declared) {
		got, present := parsed[keyword]
		switch {
		case !present:
			mismatches = append(mismatches, fmt.Sprintf("%s: declares %q, the config lacks it", where, keyword))
		case got != declared[keyword]:
			mismatches = append(mismatches, fmt.Sprintf("%s: declares %s %q, the config has %q", where, keyword, declared[keyword], got))
		}
	}
	for _, keyword := range sortedKeys(parsed) {
		if _, present := declared[keyword]; !present {
			mismatches = append(mismatches, fmt.Sprintf("%s: the config has %q, the record does not declare it", where, keyword))
		}
	}
	return mismatches
}

func serverMismatches(backend string, declared *renderplan.Server, emitted *parsedServer) []string {
	var mismatches []string
	where := fmt.Sprintf("backend %q server %q", backend, declared.Name)
	if declared.Name != emitted.Name {
		return []string{fmt.Sprintf("%s: the config has %q in that position", where, emitted.Name)}
	}
	if declared.Template != emitted.Template {
		mismatches = append(mismatches, fmt.Sprintf("%s: declared template %d, the config has %d",
			where, declared.Template, emitted.Template))
	}
	if address := declaredAddress(declared); address != emitted.Address {
		mismatches = append(mismatches, fmt.Sprintf("%s: declared address %q, the config has %q",
			where, address, emitted.Address))
	}
	// `enabled` is HAProxy's explicit opposite of `disabled`; neither keyword
	// also means enabled.
	_, disabled := emitted.Options["disabled"]
	if declared.Disabled != disabled {
		mismatches = append(mismatches, fmt.Sprintf("%s: declared disabled=%t, the config says %t",
			where, declared.Disabled, disabled))
	}
	declaredWeight := ""
	if declared.Weight != nil {
		declaredWeight = fmt.Sprint(*declared.Weight)
	}
	if want, got := declaredWeight, emitted.Options["weight"]; want != got {
		mismatches = append(mismatches, fmt.Sprintf("%s: declared weight %q, the config has %q", where, want, got))
	}
	if declared.GUID != emitted.Options["guid"] {
		mismatches = append(mismatches, fmt.Sprintf("%s: declared guid %q, the config has %q",
			where, declared.GUID, emitted.Options["guid"]))
	}
	if declared.Comment != emitted.Comment {
		mismatches = append(mismatches, fmt.Sprintf("%s: declared comment %q, the config has %q",
			where, declared.Comment, emitted.Comment))
	}
	// Every other keyword on the line must be declared in Extra, and vice versa.
	structured := map[string]bool{"guid": true, "weight": true, "disabled": true, "enabled": true}
	extra := make(map[string]string, len(emitted.Options))
	for keyword, argument := range emitted.Options {
		if !structured[keyword] {
			extra[keyword] = argument
		}
	}
	mismatches = append(mismatches, optionMismatches(where, keywordArgs(declared.Extra), extra)...)
	return mismatches
}

// declaredAddress reassembles the address the server line must carry. A record
// without a port describes a server line without one.
func declaredAddress(server *renderplan.Server) string {
	if server.Port == 0 {
		return server.Address
	}
	return fmt.Sprintf("%s:%d", server.Address, server.Port)
}

// sectionPartitionMismatches checks that the sections tile the configuration:
// each one's bytes hash to its digest, and together they cover the file exactly.
func sectionPartitionMismatches(config string, plan *renderplan.Plan) []string {
	var mismatches []string
	offset := 0
	for _, section := range plan.Sections {
		if section.Length < 0 || offset+section.Length > len(config) {
			return append(mismatches, fmt.Sprintf("section %q runs past the end of the config (%d bytes at offset %d of %d)",
				section.Name, section.Length, offset, len(config)))
		}
		text := config[offset : offset+section.Length]
		if digest := renderplan.DigestString(text); digest != section.TextDigest {
			mismatches = append(mismatches, fmt.Sprintf("section %q: declared digest %s, its bytes hash to %s",
				section.Name, section.TextDigest, digest))
		}
		offset += section.Length
	}
	if offset != len(config) {
		mismatches = append(mismatches, fmt.Sprintf("the sections cover %d of %d config bytes", offset, len(config)))
	}
	return mismatches
}

// mapEntryMismatches compares every map file's declared entries against a naive
// re-read of the content that was rendered for it.
func mapEntryMismatches(contents map[string]string, plan *renderplan.Plan) []string {
	var mismatches []string
	for _, path := range sortedKeys(contents) {
		declared, found := plan.Maps[path]
		if !found {
			mismatches = append(mismatches, fmt.Sprintf("map %q was rendered but is not in the plan", path))
			continue
		}
		emitted := naiveMapEntries(contents[path])
		if len(declared.Entries) != len(emitted) {
			mismatches = append(mismatches, fmt.Sprintf("map %q declares %d entries, its content has %d",
				path, len(declared.Entries), len(emitted)))
			continue
		}
		for i := range emitted {
			if declared.Entries[i] != emitted[i] {
				mismatches = append(mismatches, fmt.Sprintf("map %q entry %d: declared %v, its content has %v",
					path, i, declared.Entries[i], emitted[i]))
			}
		}
	}
	for path := range plan.Maps {
		if _, rendered := contents[path]; !rendered {
			mismatches = append(mismatches, fmt.Sprintf("map %q is in the plan but was not rendered", path))
		}
	}
	sort.Strings(mismatches)
	return mismatches
}

// mapContentsByPlanPath keys the rendered map contents by the path the plan
// lists them under (`maps/<name>`), matching content by digest so the test
// never re-derives the resolver's naming. A map the plan lacks stays keyed by
// its rendered name and shows up as "not in the plan".
func mapContentsByPlanPath(files []auxiliaryfiles.MapFile, plan *renderplan.Plan) map[string]string {
	planned := make(map[string][]string, len(files))
	if plan != nil {
		for _, file := range plan.Files {
			if file.Kind == renderplan.FileKindMap && file.Seal == "" {
				planned[file.Digest] = append(planned[file.Digest], file.Path)
			}
		}
	}
	contents := make(map[string]string, len(files))
	for _, file := range files {
		key := file.Path
		digest := renderplan.DigestString(file.Content)
		if paths := planned[digest]; len(paths) > 0 {
			key, planned[digest] = paths[0], paths[1:]
		}
		contents[key] = file.Content
	}
	return contents
}

// naiveMapEntries is this test's own reader of HAPTIC's map-file format, kept
// deliberately independent of renderplan.ParseMapEntries: comparing a function
// against itself would prove nothing.
func naiveMapEntries(content string) []renderplan.Entry {
	var entries []renderplan.Entry
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		key := fields[0]
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), key))
		entries = append(entries, renderplan.Entry{Key: key, Value: rest})
	}
	return entries
}

// --- synthetic cases -------------------------------------------------------

// renderSynthetic renders one hand-written main template through the same
// registry + assembly path production uses, and returns the config with its plan.
func renderSynthetic(t *testing.T, source string, mapContents map[string]string) (string, *renderplan.Plan) {
	t.Helper()
	engine, err := templating.New(map[string]string{names.MainTemplateName: source}, nil)
	require.NoError(t, err)

	registry := rendercontext.NewPlanRegistry(nil)
	main, err := rendercontext.RenderMain(context.Background(), engine,
		map[string]any{"planRegistry": registry}, registry, false)
	require.NoError(t, err)

	aux := &dataplane.AuxiliaryFiles{}
	for _, name := range sortedKeys(mapContents) {
		aux.MapFiles = append(aux.MapFiles, auxiliaryfiles.MapFile{Path: name, Content: mapContents[name]})
	}
	plan, err := registry.Plan(main.Config, aux)
	require.NoError(t, err)
	return main.Config, plan
}

// backendTemplate is a main template that declares one backend as data and
// emits the section text it claims to describe.
func backendTemplate(record, text string) string {
	return `{% var token, err = planRegistry.Backend(` + record + `, "` + text + `") %}` +
		`{% if err != nil %}{{ fail("backend declaration rejected") }}{% end %}` +
		"global\n    daemon\n{{ token }}"
}

const twoServerText = `backend be_api\n    default-server check inter 2s\n    server SRV_1 10.0.0.1:8080 weight 20 guid srv:be_api:SRV_1 send-proxy-v2  # Pod: api-1\n    server SRV_2 10.0.0.2:8080 disabled\n`

// twoServerRecord describes twoServerText exactly.
const twoServerRecord = `map[string]any{
	"name": "be_api", "mode": "http", "shape": "dynamic",
	"defaultServer": []any{map[string]any{"name": "check"}, map[string]any{"name": "inter", "args": []any{"2s"}}},
	"servers": []any{
		map[string]any{"name": "SRV_1", "address": "10.0.0.1", "port": 8080, "weight": 20, "guid": "srv:be_api:SRV_1",
			"comment": "Pod: api-1", "extra": []any{map[string]any{"name": "send-proxy-v2"}}},
		map[string]any{"name": "SRV_2", "address": "10.0.0.2", "port": 8080, "disabled": true},
	},
}`

const templateText = `backend be_files\n    server-template s3 4 files.example.net:443 init-addr none  # ExternalName: files.example.net\n`

// templateRecord describes templateText with the given slot count.
func templateRecord(slots int) string {
	return fmt.Sprintf(`map[string]any{
	"name": "be_files", "shape": "structural",
	"servers": []any{
		map[string]any{"name": "s3", "address": "files.example.net", "port": 443, "template": %d,
			"comment": "ExternalName: files.example.net", "extra": []any{map[string]any{"name": "init-addr", "args": []any{"none"}}}},
	},
}`, slots)
}

// twoServerRecordWith returns twoServerRecord with the SRV_1 record replaced.
func twoServerRecordWith(srv1 string) string {
	return `map[string]any{
	"name": "be_api", "mode": "http", "shape": "dynamic",
	"defaultServer": []any{map[string]any{"name": "check"}, map[string]any{"name": "inter", "args": []any{"2s"}}},
	"servers": []any{
		` + srv1 + `,
		map[string]any{"name": "SRV_2", "address": "10.0.0.2", "port": 8080, "disabled": true},
	},
}`
}

func TestPlanDifferentialSynthetic(t *testing.T) {
	tests := []struct {
		name        string
		record      string
		text        string
		maps        map[string]string
		wantMissing string // "" means the plan must match the config
	}{
		{
			name:   "a record that describes its section exactly",
			record: twoServerRecord,
			text:   twoServerText,
		},
		{
			name:   "map entries match the rendered content",
			record: twoServerRecord,
			text:   twoServerText,
			maps: map[string]string{
				"host.map": "# a comment\nexample.com be_api\n\nother.example.com be_other\n",
			},
		},
		{
			name:   "a server-template record that describes its line exactly",
			record: templateRecord(4),
			text:   templateText,
		},
		{
			name:        "a server-template record with a wrong slot count",
			record:      templateRecord(6),
			text:        templateText,
			wantMissing: `backend "be_files" server "s3": declared template 6, the config has 4`,
		},
		{
			name: "a record with one server fewer than its text",
			record: `map[string]any{
				"name": "be_api", "shape": "dynamic",
				"servers": []any{
					map[string]any{"name": "SRV_1", "address": "10.0.0.1", "port": 8080, "weight": 20, "guid": "srv:be_api:SRV_1"},
				},
			}`,
			text:        twoServerText,
			wantMissing: `backend "be_api" declares 1 servers, the config has 2`,
		},
		{
			name: "a record that drops an extra keyword",
			record: twoServerRecordWith(
				`map[string]any{"name": "SRV_1", "address": "10.0.0.1", "port": 8080, "weight": 20, "guid": "srv:be_api:SRV_1", "comment": "Pod: api-1"}`),
			text:        twoServerText,
			wantMissing: `the config has "send-proxy-v2", the record does not declare it`,
		},
		{
			name: "a record with a wrong comment",
			record: twoServerRecordWith(
				`map[string]any{"name": "SRV_1", "address": "10.0.0.1", "port": 8080, "weight": 20, "guid": "srv:be_api:SRV_1", "comment": "Pod: api-9", "extra": []any{map[string]any{"name": "send-proxy-v2"}}}`),
			text:        twoServerText,
			wantMissing: `declared comment "Pod: api-9", the config has "Pod: api-1"`,
		},
		{
			name: "a record with a wrong default-server keyword",
			record: `map[string]any{
				"name": "be_api", "mode": "http", "shape": "dynamic",
				"defaultServer": []any{map[string]any{"name": "check"}, map[string]any{"name": "inter", "args": []any{"5s"}}},
				"servers": []any{
					map[string]any{"name": "SRV_1", "address": "10.0.0.1", "port": 8080, "weight": 20, "guid": "srv:be_api:SRV_1",
						"comment": "Pod: api-1", "extra": []any{map[string]any{"name": "send-proxy-v2"}}},
					map[string]any{"name": "SRV_2", "address": "10.0.0.2", "port": 8080, "disabled": true},
				},
			}`,
			text:        twoServerText,
			wantMissing: `default-server: declares inter "5s", the config has "2s"`,
		},
		{
			name: "a record with a wrong server address",
			record: `map[string]any{
				"name": "be_api", "shape": "dynamic",
				"servers": []any{
					map[string]any{"name": "SRV_1", "address": "10.9.9.9", "port": 8080, "weight": 20, "guid": "srv:be_api:SRV_1"},
					map[string]any{"name": "SRV_2", "address": "10.0.0.2", "port": 8080, "disabled": true},
				},
			}`,
			text:        twoServerText,
			wantMissing: `declared address "10.9.9.9:8080", the config has "10.0.0.1:8080"`,
		},
		{
			name: "a record that forgets a server is disabled",
			record: `map[string]any{
				"name": "be_api", "shape": "dynamic",
				"servers": []any{
					map[string]any{"name": "SRV_1", "address": "10.0.0.1", "port": 8080, "weight": 20, "guid": "srv:be_api:SRV_1"},
					map[string]any{"name": "SRV_2", "address": "10.0.0.2", "port": 8080},
				},
			}`,
			text:        twoServerText,
			wantMissing: "declared disabled=false, the config says true",
		},
		{
			name: "a record with a wrong server guid",
			record: `map[string]any{
				"name": "be_api", "shape": "dynamic",
				"servers": []any{
					map[string]any{"name": "SRV_1", "address": "10.0.0.1", "port": 8080, "weight": 20, "guid": "srv:be_api:SRV_9"},
					map[string]any{"name": "SRV_2", "address": "10.0.0.2", "port": 8080, "disabled": true},
				},
			}`,
			text:        twoServerText,
			wantMissing: `declared guid "srv:be_api:SRV_9", the config has "srv:be_api:SRV_1"`,
		},
		{
			name: "a record that omits a declared weight",
			record: `map[string]any{
				"name": "be_api", "shape": "dynamic",
				"servers": []any{
					map[string]any{"name": "SRV_1", "address": "10.0.0.1", "port": 8080, "guid": "srv:be_api:SRV_1"},
					map[string]any{"name": "SRV_2", "address": "10.0.0.2", "port": 8080, "disabled": true},
				},
			}`,
			text:        twoServerText,
			wantMissing: `declared weight "", the config has "20"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, plan := renderSynthetic(t, backendTemplate(tt.record, tt.text), tt.maps)

			mismatches := planMismatches(t, config, tt.maps, plan)

			if tt.wantMissing == "" {
				assert.Empty(t, mismatches)
				return
			}
			require.NotEmpty(t, mismatches, "the comparison passed a record that under-describes its section")
			assert.Contains(t, strings.Join(mismatches, "\n"), tt.wantMissing)
		})
	}
}

// The section list is the deploy side's only map from plan to bytes; a hole in
// it is a section that could be rewritten without anyone noticing.
func TestPlanDifferentialCatchesABrokenPartition(t *testing.T) {
	config, plan := renderSynthetic(t, backendTemplate(twoServerRecord, twoServerText), nil)
	require.Empty(t, planMismatches(t, config, nil, plan))

	t.Run("a section that under-reports its length", func(t *testing.T) {
		shortened := *plan
		shortened.Sections = append([]renderplan.Section(nil), plan.Sections...)
		shortened.Sections[0].Length--

		assert.NotEmpty(t, planMismatches(t, config, nil, &shortened))
	})

	t.Run("a section whose digest does not match its bytes", func(t *testing.T) {
		corrupted := *plan
		corrupted.Sections = append([]renderplan.Section(nil), plan.Sections...)
		corrupted.Sections[0].TextDigest = renderplan.DigestString("something else")

		assert.NotEmpty(t, planMismatches(t, config, nil, &corrupted))
	})
}

func TestPlanDifferentialCatchesAWrongMapEntry(t *testing.T) {
	contents := map[string]string{"host.map": "example.com be_api\nother.example.com be_other\n"}
	config, plan := renderSynthetic(t, backendTemplate(twoServerRecord, twoServerText), contents)
	require.Empty(t, planMismatches(t, config, contents, plan))

	t.Run("an entry with the wrong value", func(t *testing.T) {
		wrong := *plan
		wrong.Maps = map[string]renderplan.Map{"host.map": {
			Path:    "host.map",
			Entries: []renderplan.Entry{{Key: "example.com", Value: "be_wrong"}, {Key: "other.example.com", Value: "be_other"}},
		}}

		assert.NotEmpty(t, planMismatches(t, config, contents, &wrong))
	})

	t.Run("a map file the plan does not list", func(t *testing.T) {
		missing := *plan
		missing.Maps = nil

		assert.NotEmpty(t, planMismatches(t, config, contents, &missing))
	})
}

func TestPlanDifferentialCorpus(t *testing.T) {
	restoreHAProxy := dataplanetest.InstallFakeHAProxy()
	t.Cleanup(restoreHAProxy)

	runner, testNames, cleanup := bundledChartRunner(t)
	t.Cleanup(cleanup)
	require.NotEmpty(t, testNames, "the bundled chart contributed no validation tests")

	renders := renderCorpus(t, runner, testNames)
	assert.Greater(t, len(renders), len(testNames)/2, "most of the corpus must actually render")
	t.Run("configuration and maps", func(t *testing.T) {
		// client-native's parser writes a package-level global, so comparisons stay serial.
		for _, rendered := range renders {
			assert.Emptyf(t, planMismatches(t, rendered.config, rendered.maps, rendered.plan),
				"%s: the plan does not describe what the render emitted", rendered.name)
		}
	})
	t.Run("structured default server keywords", func(t *testing.T) {
		assertCorpusDefaultServerKeywords(t, renders)
	})
	t.Run("dynamic backend TLS", func(t *testing.T) {
		assertCorpusBackendTLS(t, renders)
	})
}

func assertCorpusDefaultServerKeywords(t *testing.T, renders []corpusRender) {
	t.Helper()
	sawCheck, sawBTLS := false, false
	for _, rendered := range renders {
		for name := range rendered.plan.Backends {
			keywords := rendered.plan.Backends[name].DefaultServer
			kws := make(map[string]bool, len(keywords))
			for _, kw := range keywords {
				kws[kw.Name] = true
			}
			if kws["check"] {
				sawCheck = true
			}
			if kws["ssl"] && kws["verify"] && kws["ca-file"] {
				sawBTLS = true
			}
		}
	}
	assert.True(t, sawCheck, "no backend records `check` in DefaultServer — a runtime-added server would have no health check")
	assert.True(t, sawBTLS, "no backend records ssl+verify+ca-file in DefaultServer — a BackendTLS add server would drop the TLS control")
}

func assertCorpusBackendTLS(t *testing.T, renders []corpusRender) {
	t.Helper()
	sawCA, sawCrtBackend := false, false
	for _, rendered := range renders {
		for i := range rendered.plan.Files {
			f := &rendered.plan.Files[i]
			if f.Kind == renderplan.FileKindCA && strings.Contains(f.Path, "backend-tls-ca") {
				sawCA = true
			}
		}
		certs := deployplan.InventoryOf(rendered.plan).Certs
		for name := range rendered.plan.Backends {
			backend := rendered.plan.Backends[name]
			if assertClientCertDynamic(t, name, &backend, certs) {
				sawCrtBackend = true
			}
		}
	}
	assert.True(t, sawCA, "no backend-tls CA registered as a runtime-rotatable ca-file (FileKindCA)")
	assert.True(t, sawCrtBackend, "no mTLS backend with a crt keyword — the dynamic client-cert guard exercised nothing")
}

// assertClientCertDynamic checks a backend carrying a client cert and reports
// whether it carries one. `add server ... crt` takes the config line's bare
// filename: HAProxy joins crt-base, the certificate directory, onto it, so the
// result must be a file the plan registers or the worker refuses the add; and
// the backend must stay dynamic, or pod churn on it reloads.
func assertClientCertDynamic(t *testing.T, name string, backend *renderplan.Backend, certs []string) bool {
	t.Helper()
	seen := false
	for _, kw := range backend.DefaultServer {
		if kw.Name != keywordCrt {
			continue
		}
		seen = true
		if assert.Lenf(t, kw.Args, 1, "backend %q carries a crt keyword with %d args", name, len(kw.Args)) {
			assert.Truef(t, slices.ContainsFunc(certs, func(p string) bool { return path.Dir(p)+"/"+kw.Args[0] == p }),
				"backend %q carries crt %q, which resolves to no cert file the plan registers", name, kw.Args[0])
		}
		assert.Equalf(t, renderplan.ShapeDynamic, backend.Shape,
			"backend %q carries a client cert but is %q (%s): pod churn on it would reload",
			name, backend.Shape, backend.ShapeReason)
	}
	return seen
}

const keywordCrt = "crt"

// corpusRender is one fixture's rendered output with the plan it declared.
type corpusRender struct {
	name   string
	config string
	maps   map[string]string
	plan   *renderplan.Plan
}

func renderCorpus(t *testing.T, runner *testrunner.Runner, testNames []string) []corpusRender {
	t.Helper()
	ctx := context.Background()
	pending := make(chan string, len(testNames))
	for _, name := range testNames {
		pending <- name
	}
	close(pending)

	var mu sync.Mutex
	var renders []corpusRender
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range pending {
				rendered, err := runner.Render(ctx, name)
				if err != nil {
					// Tests that assert a rendering_error have no config to compare.
					continue
				}
				maps := mapContentsByPlanPath(rendered.AuxiliaryFiles.MapFiles, rendered.Plan)
				mu.Lock()
				renders = append(renders, corpusRender{
					name: name, config: rendered.HAProxyConfig, maps: maps, plan: rendered.Plan,
				})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	sort.Slice(renders, func(i, j int) bool { return renders[i].name < renders[j].name })
	for i := range renders {
		require.NotNilf(t, renders[i].plan, "%s: the render produced no plan", renders[i].name)
	}
	return renders
}

// bundledChartSetup renders the bundled chart with every template library
// enabled and returns the converted config, the validation setup and the logger,
// plus a cleanup. It takes testing.TB so both the differential tests and the
// admission-render benchmark drive the same in-process chart render.
func bundledChartSetup(tb testing.TB) (*coreconfig.Config, *ValidationSetup, *slog.Logger, func()) {
	tb.Helper()
	chartDir := repoPath(tb, "charts", "haptic")
	values := filepath.Join(tb.TempDir(), "values.yaml")
	require.NoError(tb, os.WriteFile(values, []byte(allLibrariesValues), 0o600))

	manifests, err := renderChartManifests(chartDir, []string{values}, "", offlineCaps())
	require.NoError(tb, err)
	docs, err := collectConfigDocuments(manifests)
	require.NoError(tb, err)

	configFile := filepath.Join(tb.TempDir(), "config.yaml")
	require.NoError(tb, os.WriteFile(configFile, []byte(docs), 0o600))

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	restoreFlags := setValidateFlags(tb, configFile, repoPath(tb, "tests", "schemas"))
	schemas, err := newDirSchemaSource(validateSchemaDir, logger)
	require.NoError(tb, err)
	setup, err := setupValidation(context.Background(), validateConfigFiles, schemas, nil, logger)
	require.NoError(tb, err)

	cfg, err := conversion.ConvertSpec(setup.ConfigSpec)
	require.NoError(tb, err)
	// A chart-scale cold render exceeds the 30s production default once `make
	// test` instruments it with -race and -coverpkg. Exactness assertions are
	// unaffected; none of these tests measure latency.
	cfg.TemplatingSettings.RenderTimeout = "5m"

	return cfg, setup, logger, func() {
		setup.Cleanup()
		restoreFlags()
	}
}

// bundledChartRunner renders the bundled chart with every template library
// enabled and returns a runner over its validation tests, plus their names in a
// stable order.
func bundledChartRunner(t *testing.T) (runner *testrunner.Runner, testNames []string, cleanup func()) {
	t.Helper()
	cfg, setup, logger, cleanup := bundledChartSetup(t)
	runner = testrunner.New(cfg, setup.Engine, setup.ValidationPaths, &testrunner.Options{
		Logger:             logger,
		Capabilities:       setup.Capabilities,
		HAProxyVersion:     setup.HAProxyVersion,
		TypedResourceTypes: setup.TypedResourceTypes,
	})

	// "_global" is a shared fixture baseline, never a render of its own.
	testNames = make([]string, 0, len(cfg.ValidationTests))
	for name := range cfg.ValidationTests {
		if name != "_global" {
			testNames = append(testNames, name)
		}
	}
	sort.Strings(testNames)

	return runner, testNames, cleanup
}

// allLibrariesValues turns on every bundled template library, so the corpus is
// the whole shipped surface rather than the default subset.
const allLibrariesValues = `controller:
  templateLibraries:
    gateway:
      enabled: true
      experimentalChannel: true
    hapticAnnotations:
      enabled: true
    haproxytech:
      enabled: true
    haproxyIngress:
      enabled: true
    nginxIngress:
      enabled: true
`

// setValidateFlags points the command's package-level flags at this test's
// inputs and restores them afterwards.
func setValidateFlags(tb testing.TB, configFile, schemaDir string) func() {
	tb.Helper()
	previousFiles, previousSchemaDir := validateConfigFiles, validateSchemaDir
	validateConfigFiles, validateSchemaDir = []string{configFile}, schemaDir
	return func() { validateConfigFiles, validateSchemaDir = previousFiles, previousSchemaDir }
}

// repoPath resolves a path relative to the repository root, which is two levels
// above this package.
func repoPath(tb testing.TB, elements ...string) string {
	tb.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(tb, err)
	return filepath.Join(append([]string{root}, elements...)...)
}
