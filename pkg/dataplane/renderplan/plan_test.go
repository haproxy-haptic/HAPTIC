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

package renderplan_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

func samplePlan() *renderplan.Plan {
	return &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		Sections: []renderplan.Section{
			{Kind: renderplan.SectionKindCore, Name: "core#0", TextDigest: renderplan.DigestString("global\n"), Length: 7},
			{Kind: renderplan.SectionKindBackend, Name: "be-a", TextDigest: renderplan.DigestString("backend be-a\n"), Length: 13},
		},
		Backends: map[string]renderplan.Backend{
			"be-a": {
				Name:  "be-a",
				Shape: renderplan.ShapeStructural,
				Servers: []renderplan.Server{
					{Name: "SRV_1", Address: "10.0.0.1", Port: 8080},
					{Name: "SRV_2", Address: "10.0.0.2", Port: 8081},
				},
			},
			"be-b": {Name: "be-b", Shape: renderplan.ShapeDynamic},
		},
		Profiles: map[string]renderplan.Profile{"haptic-be-1": {Name: "haptic-be-1"}},
		Maps: map[string]renderplan.Map{
			"host.map": {Path: "host.map", Ordered: true, Entries: []renderplan.Entry{{Key: "a", Value: "b"}}},
		},
		Files: []renderplan.File{{Path: "haproxy.cfg", Kind: renderplan.FileKindConfig, Digest: "x", Size: 20}},
	}
}

func TestPlanCloneOwnsNestedState(t *testing.T) {
	weight := 10
	original := &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		ID:            "plan",
		Sections:      []renderplan.Section{{Name: "section"}},
		Backends: map[string]renderplan.Backend{
			"backend": {
				Name: "backend",
				Servers: []renderplan.Server{{
					Name: "server", Weight: &weight,
					Extra: []renderplan.KeywordArg{{Name: "check", Args: []string{"one"}}},
				}},
				DefaultServer: []renderplan.KeywordArg{{Name: "inter", Args: []string{"1s"}}},
			},
		},
		Profiles: map[string]renderplan.Profile{"profile": {Name: "profile"}},
		Maps: map[string]renderplan.Map{
			"map": {Path: "map", Entries: []renderplan.Entry{{Key: "key", Value: "value"}}},
		},
		CRTLists: map[string]renderplan.CRTList{
			"list": {
				Path: "list",
				Entries: []renderplan.CRTListEntry{{
					Cert:       "cert",
					Options:    []renderplan.KeywordArg{{Name: "alpn", Args: []string{"h2"}}},
					SNIFilters: []string{"example.test"},
				}},
			},
		},
		Files: []renderplan.File{{Path: "file"}},
	}
	cloned := original.Clone()
	require.Equal(t, original, cloned)

	original.Sections[0].Name = "poison"
	backend := original.Backends["backend"]
	backend.Servers[0].Name = "poison"
	*backend.Servers[0].Weight = 99
	backend.Servers[0].Extra[0].Args[0] = "poison"
	backend.DefaultServer[0].Args[0] = "poison"
	original.Backends["backend"] = backend
	original.Profiles["profile"] = renderplan.Profile{Name: "poison"}
	sourceMap := original.Maps["map"]
	sourceMap.Entries[0].Value = "poison"
	original.Maps["map"] = sourceMap
	crtList := original.CRTLists["list"]
	crtList.Entries[0].Options[0].Args[0] = "poison"
	crtList.Entries[0].SNIFilters[0] = "poison"
	original.CRTLists["list"] = crtList
	original.Files[0].Path = "poison"

	assert.Equal(t, "section", cloned.Sections[0].Name)
	assert.Equal(t, "server", cloned.Backends["backend"].Servers[0].Name)
	assert.Equal(t, 10, *cloned.Backends["backend"].Servers[0].Weight)
	assert.Equal(t, "one", cloned.Backends["backend"].Servers[0].Extra[0].Args[0])
	assert.Equal(t, "1s", cloned.Backends["backend"].DefaultServer[0].Args[0])
	assert.Equal(t, "profile", cloned.Profiles["profile"].Name)
	assert.Equal(t, "value", cloned.Maps["map"].Entries[0].Value)
	assert.Equal(t, "h2", cloned.CRTLists["list"].Entries[0].Options[0].Args[0])
	assert.Equal(t, "example.test", cloned.CRTLists["list"].Entries[0].SNIFilters[0])
	assert.Equal(t, "file", cloned.Files[0].Path)
	cloned.Backends["backend"].Servers[0].Extra[0].Args[0] = "clone-only"
	assert.Equal(t, "poison", original.Backends["backend"].Servers[0].Extra[0].Args[0])
	emptyClone := (&renderplan.Plan{}).Clone()
	assert.Nil(t, emptyClone.Backends)
	assert.Nil(t, emptyClone.Maps)
	assert.Nil(t, emptyClone.CRTLists)
	assert.Nil(t, (*renderplan.Plan)(nil).Clone())
}

func TestPlanCanonicalIsDeterministic(t *testing.T) {
	first := samplePlan()
	second := samplePlan()
	// Insertion order differs from the first plan's; the encoding must not.
	second.Backends = map[string]renderplan.Backend{}
	for name, backend := range samplePlan().Backends {
		second.Backends[name] = backend
	}

	assert.Equal(t, string(first.Canonical()), string(second.Canonical()))
	assert.NotContains(t, string(first.Canonical()), `"id":"deadbeef"`)
}

func TestPlanCanonicalExcludesID(t *testing.T) {
	plan := samplePlan()
	withoutID := string(plan.Canonical())

	plan.ID = "deadbeefdeadbeef"
	assert.Equal(t, withoutID, string(plan.Canonical()))
}

func TestPlanComputeID(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*renderplan.Plan)
		wantSame bool
	}{
		{name: "unchanged plan keeps its id", mutate: func(*renderplan.Plan) {}, wantSame: true},
		{
			name:     "recompute is stable",
			mutate:   func(p *renderplan.Plan) { p.ComputeID() },
			wantSame: true,
		},
		{
			name:     "section digest change",
			mutate:   func(p *renderplan.Plan) { p.Sections[0].TextDigest = "0000000000000000" },
			wantSame: false,
		},
		{
			name:     "section order change",
			mutate:   func(p *renderplan.Plan) { p.Sections[0], p.Sections[1] = p.Sections[1], p.Sections[0] },
			wantSame: false,
		},
		{
			name: "new backend",
			mutate: func(p *renderplan.Plan) {
				p.Backends["be-c"] = renderplan.Backend{Name: "be-c", Shape: renderplan.ShapeDynamic}
			},
			wantSame: false,
		},
		{
			name:     "map entry value change",
			mutate:   func(p *renderplan.Plan) { p.Maps["host.map"] = renderplan.Map{Path: "host.map"} },
			wantSame: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := samplePlan()
			plan.ComputeID()
			original := plan.ID
			require.Len(t, original, 16)

			tc.mutate(plan)
			plan.ComputeID()

			if tc.wantSame {
				assert.Equal(t, original, plan.ID)
				return
			}
			assert.NotEqual(t, original, plan.ID)
		})
	}
}

func TestPlanCurrentConfig(t *testing.T) {
	plan := samplePlan()

	current := plan.CurrentConfig()

	require.Contains(t, current.ServerIndex, "be-a")
	assert.NotContains(t, current.ServerIndex, "be-b", "a backend without servers contributes no index entry")

	server := current.ServerIndex["be-a"]["SRV_2"]
	assert.Equal(t, "10.0.0.2", server.Address)
	require.NotNil(t, server.Port)
	assert.Equal(t, int64(8081), *server.Port)
}

// TestPlanCurrentConfigLeavesOutServerTemplates pins that currentConfig lists
// only servers the running configuration has under their own name: a
// server-template's slots get their names and addresses at runtime.
func TestPlanCurrentConfigLeavesOutServerTemplates(t *testing.T) {
	plan := samplePlan()
	plan.Backends["be-files"] = renderplan.Backend{
		Name: "be-files", Shape: renderplan.ShapeStructural,
		Servers: []renderplan.Server{{Name: "files", Address: "files.example.net", Port: 443, Template: 4}},
	}
	mixed := plan.Backends["be-a"]
	mixed.Servers = append(slices.Clone(mixed.Servers), renderplan.Server{Name: "pool", Address: "pool.example.net", Port: 443, Template: 2})
	plan.Backends["be-a"] = mixed

	current := plan.CurrentConfig()

	assert.NotContains(t, current.ServerIndex, "be-files", "a backend of only templates contributes no index entry")
	assert.NotContains(t, current.ServerIndex["be-a"], "pool")
	assert.Len(t, current.ServerIndex["be-a"], 2)
}

func TestDigest(t *testing.T) {
	assert.Len(t, renderplan.Digest([]byte("abc")), 16)
	assert.Equal(t, renderplan.Digest([]byte("abc")), renderplan.DigestString("abc"))
	assert.NotEqual(t, renderplan.DigestString("abc"), renderplan.DigestString("abd"))
	assert.Equal(t, renderplan.DigestString(""), renderplan.Digest(nil))
}

func TestParseMapEntries(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []renderplan.Entry
	}{
		{name: "empty content", content: "", want: nil},
		{name: "only comments and blanks", content: "# c\n\n   \n\t\n", want: nil},
		{
			name:    "key and rest of line",
			content: "example.com be_app\n",
			want:    []renderplan.Entry{{Key: "example.com", Value: "be_app"}},
		},
		{
			name:    "value keeps inner spacing",
			content: "/api  be_api  extra\n",
			want:    []renderplan.Entry{{Key: "/api", Value: "be_api  extra"}},
		},
		{
			name:    "tab separator and leading whitespace",
			content: "  key\tvalue  \n",
			want:    []renderplan.Entry{{Key: "key", Value: "value"}},
		},
		{
			name:    "key without value",
			content: "lonely\n",
			want:    []renderplan.Entry{{Key: "lonely", Value: ""}},
		},
		{
			name:    "comments interleaved",
			content: "# header\na 1\n   # indented comment\nb 2\n",
			want:    []renderplan.Entry{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}},
		},
		{
			name:    "duplicates kept in order",
			content: "a 1\nb 2\na 3\na 1\n",
			want: []renderplan.Entry{
				{Key: "a", Value: "1"},
				{Key: "b", Value: "2"},
				{Key: "a", Value: "3"},
				{Key: "a", Value: "1"},
			},
		},
		{
			name:    "no trailing newline",
			content: "a 1\nb 2",
			want:    []renderplan.Entry{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, renderplan.ParseMapEntries(tc.content))
		})
	}
}
