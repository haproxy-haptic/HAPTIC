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

package rendercontext

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func TestPlanRegistrySection(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		section string
		wantErr string
	}{
		{name: "backend", kind: "backend", section: "be_app"},
		{name: "profile", kind: "profile", section: "haptic-be-0f1e"},
		{name: "name with dots and colons", kind: "backend", section: "ns:svc.local-1"},
		{name: "unknown kind", kind: "frontend", section: "fe", wantErr: `kind must be "profile" or "backend"`},
		{name: "core is not registrable", kind: "core", section: "x", wantErr: "kind must be"},
		{name: "empty name", kind: "backend", section: "", wantErr: "must match"},
		{name: "name with space", kind: "backend", section: "be app", wantErr: "must match"},
		{name: "name with token terminator", kind: "backend", section: "be@", wantErr: "must match"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewPlanRegistry(nil)

			token, err := registry.Section(tc.kind, tc.section, "text\n")

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, token)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "# @haptic:"+registry.nonce+":section:"+tc.kind+":"+tc.section+"@\n", token)
		})
	}
}

func TestPlanTokenAuthorityRejectsCopiesAndMutation(t *testing.T) {
	authority := NewPlanTokenAuthority()
	registry, err := NewPlanRegistryWithAuthority(nil, authority)
	require.NoError(t, err)
	token, err := registry.Section("backend", "be", "backend be\n")
	require.NoError(t, err)

	copied := *authority
	_, err = NewPlanRegistryWithAuthority(nil, &copied)
	require.ErrorContains(t, err, "authentication seal")

	authority.nonce = "poisoned"
	_, _, err = registry.Assemble(t.Context(), token, nil)
	require.ErrorContains(t, err, "does not match its namespace")
}

func TestPlanRegistrySectionIdempotent(t *testing.T) {
	registry := NewPlanRegistry(nil)

	first, err := registry.Section("profile", "shared", "defaults shared\n")
	require.NoError(t, err)
	second, err := registry.Section("profile", "shared", "defaults shared\n")
	require.NoError(t, err)
	assert.Equal(t, first, second, "re-registering identical text is a no-op")

	_, err = registry.Section("profile", "shared", "defaults shared\n    timeout connect 5s\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registered twice with different text")
}

func TestPlanRegistrySectionConcurrent(t *testing.T) {
	registry := NewPlanRegistry(nil)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := registry.Section("profile", "shared", "defaults shared\n")
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	assert.Len(t, registry.sections, 1)
}

func TestPlanRegistryBackendStrictKeys(t *testing.T) {
	tests := []struct {
		name    string
		record  map[string]any
		wantErr string
	}{
		{name: "minimal record", record: map[string]any{"name": "be_app"}},
		{
			name: "full record",
			record: map[string]any{
				"name": "be_app", "mode": "http", "guid": "be:app", "balance": "roundrobin",
				"hashType": "consistent", "shape": "dynamic", "shapeReason": "",
				"servers": []any{map[string]any{
					"name": "SRV_1", "address": "10.0.0.1", "port": 8080, "weight": 128,
					"disabled": false, "guid": "srv:1", "comment": "pod-a",
					"extra": []any{map[string]any{"name": "check", "args": []any{"inter", "2s"}}},
				}},
				"defaultServer": []any{map[string]any{"name": "check-sni", "args": []any{"example.com"}}},
				"body":          []any{"    stick-table type ip size 1m"},
				"comments":      []any{"# from Ingress default/app"},
			},
		},
		{name: "missing name", record: map[string]any{"mode": "http"}, wantErr: `"name" is required`},
		{
			name:    "unknown key suggests the nearest",
			record:  map[string]any{"name": "be_app", "bodyy": []any{"x"}},
			wantErr: `unknown key "bodyy" (did you mean "body"?)`,
		},
		{
			name:    "unrelated key lists the valid ones",
			record:  map[string]any{"name": "be_app", "frontendish": true},
			wantErr: "valid keys are name, profile, mode, guid",
		},
		{
			name:    "wrong type for name",
			record:  map[string]any{"name": 7},
			wantErr: `"name" must be a string, got int`,
		},
		{
			name:    "body must be strings",
			record:  map[string]any{"name": "be_app", "body": []any{7}},
			wantErr: `"body" must contain strings, got int`,
		},
		{
			name:    "unknown mode",
			record:  map[string]any{"name": "be_app", "mode": "htp"},
			wantErr: `mode "htp", want one of http, tcp, spop`,
		},
		{
			name:    "unknown shape",
			record:  map[string]any{"name": "be_app", "shape": "elastic"},
			wantErr: `shape "elastic", want one of dynamic, structural`,
		},
		{
			name:    "unknown server key",
			record:  map[string]any{"name": "be_app", "servers": []any{map[string]any{"name": "s", "pot": 8080}}},
			wantErr: `unknown key "pot" (did you mean "port"?)`,
		},
		{
			name:    "server port must be a number",
			record:  map[string]any{"name": "be_app", "servers": []any{map[string]any{"name": "s", "port": "8080"}}},
			wantErr: `"port" must be a number, got string`,
		},
		{
			name:    "server without a name",
			record:  map[string]any{"name": "be_app", "servers": []any{map[string]any{"address": "10.0.0.1"}}},
			wantErr: "server without a name",
		},
		{
			name:    "keyword without a name",
			record:  map[string]any{"name": "be_app", "defaultServer": []any{map[string]any{"args": []any{"x"}}}},
			wantErr: `"defaultServer" needs a keyword name`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewPlanRegistry(nil)

			token, err := registry.Backend(tc.record, "backend be_app\n")

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Contains(t, err.Error(), "planRegistry.Backend")
				assert.Empty(t, token)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, token, ":section:backend:be_app@")
			assert.Len(t, registry.backends, 1)
		})
	}
}

func TestPlanRegistryBackendDefaultsAndDigests(t *testing.T) {
	registry := NewPlanRegistry(nil)

	_, err := registry.Backend(map[string]any{
		"name":     "be_app",
		"body":     []string{"    stick-table type ip size 1m"},
		"comments": []string{"# from Ingress default/app"},
	}, "backend be_app\n")
	require.NoError(t, err)

	backend := registry.backends["be_app"]
	assert.Equal(t, renderplan.ShapeStructural, backend.Shape, "shape defaults to structural")
	assert.Empty(t, backend.Mode, "an undeclared mode stays undeclared rather than guessed")
	assert.Equal(t, renderplan.DigestString("    stick-table type ip size 1m"), backend.BodyDigest)
	assert.Equal(t, renderplan.DigestString("# from Ingress default/app"), backend.CommentsDigest)
	assert.Len(t, backend.RecordDigest, 16)
	assert.Empty(t, backend.TextDigest, "the text digest only exists once the section is assembled")
}

// A dynamic backend inherits http from its profile and carries no `mode` line,
// but `add backend … mode <mode>` needs one: the record has to name the
// effective mode or the composed op is refused and the fleet reloads.
func TestPlanRegistryDynamicBackendModeDefaultsToHTTP(t *testing.T) {
	registry := NewPlanRegistry(nil)

	_, err := registry.Backend(map[string]any{
		"name":    "gtw_app",
		"profile": "haptic-be-1",
		"shape":   renderplan.ShapeDynamic,
		"servers": []any{map[string]any{"name": "pod-a", "address": "10.0.0.1", "port": 8080}},
	}, "backend gtw_app from haptic-be-1\n    server pod-a 10.0.0.1:8080\n")
	require.NoError(t, err)
	assert.Equal(t, "http", registry.backends["gtw_app"].Mode,
		"a dynamic backend's effective mode must be recorded for add backend")

	// An explicit mode is kept, never overwritten — a dynamic tcp backend
	// (ssl-passthrough, TCPRoute, a custom-CRD tcp Route) records tcp.
	_, err = registry.Backend(map[string]any{
		"name":    "gtw_tcp",
		"profile": "haptic-be-2",
		"shape":   renderplan.ShapeDynamic,
		"mode":    "tcp",
		"servers": []any{map[string]any{"name": "pod-b", "address": "10.0.0.2", "port": 8080}},
	}, "backend gtw_tcp from haptic-be-2\n    server pod-b 10.0.0.2:8080\n")
	require.NoError(t, err)
	assert.Equal(t, "tcp", registry.backends["gtw_tcp"].Mode)

	// A structural backend never sees add backend, so an undeclared mode is
	// left as-is rather than guessed.
	_, err = registry.Backend(map[string]any{
		"name": "be_static",
		"body": []string{"    stick-table type ip size 1m"},
	}, "backend be_static\n    stick-table type ip size 1m\n")
	require.NoError(t, err)
	assert.Empty(t, registry.backends["be_static"].Mode, "a structural backend's undeclared mode stays undeclared")
}

func TestPlanRegistryBackendConflict(t *testing.T) {
	registry := NewPlanRegistry(nil)
	record := map[string]any{"name": "be_app", "mode": "http"}
	_, err := registry.Backend(record, "backend be_app\n")
	require.NoError(t, err)

	_, err = registry.Backend(map[string]any{"name": "be_app", "mode": "tcp"}, "backend be_app\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declared twice with different values")

	_, err = registry.Backend(record, "backend be_app\n    server s1 10.0.0.1:80\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registered twice with different text")
}

func TestPlanRegistryBackendCollisionCannotHideConflict(t *testing.T) {
	firstValue := "maps/11c714b2cc3f873f.map"
	secondValue := "maps/71b06949baa8f2ff.map"
	firstDigest := renderplan.DigestString(firstValue)
	secondDigest := renderplan.DigestString(secondValue)
	require.Equal(t, firstDigest, secondDigest)

	first := renderplan.Backend{
		Name: "be_collision", Shape: renderplan.ShapeStructural, RecordDigest: firstDigest,
	}
	second := renderplan.Backend{
		Name: "be_collision", Mode: "tcp", Shape: renderplan.ShapeStructural, RecordDigest: secondDigest,
	}
	require.False(t, sameBackendRecordExact(&first, &second))

	registry := NewPlanRegistry(nil)
	_, err := registry.registerPreparedBackend(&PreparedPlanBackend{Backend: first, Text: "backend collision\n"})
	require.NoError(t, err)
	_, err = registry.registerPreparedBackend(&PreparedPlanBackend{Backend: second, Text: "backend collision\n"})
	require.ErrorContains(t, err, "declared twice with different values")

	snapshot := NewPreparedPlanSnapshot()
	snapshot.sections, _, _ = snapshot.sections.Insert(
		preparedSectionKey(renderplan.SectionKindBackend, first.Name), "backend collision\n",
	)
	snapshot.backends, _, _ = snapshot.backends.Insert([]byte(first.Name), PreparedPlanBackend{
		Backend: second,
		Text:    "backend collision\n",
	})
	snapshot.authenticate()

	require.ErrorContains(t, registry.AttachPreparedPlan(snapshot), "declared twice with different values")

	registry = NewPlanRegistry(nil)
	registry.backends[first.Name] = first
	registry.prepared = snapshot
	_, err = registry.planBackends()
	require.ErrorContains(t, err, "declared twice with different values")
}

func TestPlanRegistryMapMeta(t *testing.T) {
	registry := NewPlanRegistry(nil)

	require.NoError(t, registry.MapMeta("host.map", false))
	require.NoError(t, registry.MapMeta("host.map", false))

	err := registry.MapMeta("host.map", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both ordered and unordered")

	require.Error(t, registry.MapMeta("", true))
}

func TestPlanRegistryPlan(t *testing.T) {
	registry := NewPlanRegistry(nil)
	profileToken, err := registry.Section("profile", "haptic-be-1", "defaults haptic-be-1\n    mode http\n")
	require.NoError(t, err)
	backendToken, err := registry.Backend(map[string]any{
		"name":    "be_app",
		"servers": []any{map[string]any{"name": "SRV_1", "address": "10.0.0.1", "port": 8080}},
	}, "backend be_app\n    server SRV_1 10.0.0.1:8080\n")
	require.NoError(t, err)
	require.NoError(t, registry.MapMeta("host.map", false))

	rendered := "global\n    daemon\n" + profileToken + backendToken
	config, _, err := registry.Assemble(context.Background(), rendered, nil)
	require.NoError(t, err)

	plan, err := registry.Plan(config, &dataplane.AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{
		{Path: "host.map", Content: "example.com be_app\nexample.com be_app2\n"},
		{Path: "other.map", Content: "# empty\n"},
	}})
	require.NoError(t, err)

	assert.Equal(t, renderplan.SchemaVersion, plan.SchemaVersion)
	assert.Len(t, plan.ID, 16)
	assert.Equal(t, []string{"haproxy.cfg", "host.map", "other.map"},
		[]string{plan.Files[0].Path, plan.Files[1].Path, plan.Files[2].Path})

	require.Contains(t, plan.Profiles, "haptic-be-1")
	// BodyDigest is over the profile body, not the `defaults …` header line.
	assert.Equal(t, renderplan.DigestString("    mode http\n"), plan.Profiles["haptic-be-1"].BodyDigest)

	require.Contains(t, plan.Backends, "be_app")
	assert.Equal(t, renderplan.DigestString("backend be_app\n    server SRV_1 10.0.0.1:8080\n"),
		plan.Backends["be_app"].TextDigest)

	assert.False(t, plan.Maps["host.map"].Ordered, "declared unordered")
	assert.True(t, plan.Maps["other.map"].Ordered, "maps are ordered unless declared otherwise")
	assert.Len(t, plan.Maps["host.map"].Entries, 2, "duplicate keys are kept")
	assert.Empty(t, plan.Maps["other.map"].Entries)

	assert.Equal(t, "global\n    daemon\ndefaults haptic-be-1\n    mode http\nbackend be_app\n    server SRV_1 10.0.0.1:8080\n", config)
}

func TestPlanRegistryPreparedProfileSealsPostProcessedOutput(t *testing.T) {
	profile, err := PreparePlanProfile(map[string]any{"mode": "http"})
	require.NoError(t, err)
	prepared, err := NewPreparedPlanSnapshot().WithProfile(profile)
	require.NoError(t, err)
	registry := NewPlanRegistry(nil)
	require.NoError(t, registry.AttachPreparedPlan(prepared))

	post := func(_ context.Context, text string) (string, error) {
		return strings.ReplaceAll(text, "    ", "  "), nil
	}
	config, _, err := registry.Assemble(context.Background(), registry.ProfileGroup(), post)
	require.NoError(t, err)
	plan, err := registry.Plan(config, &dataplane.AuxiliaryFiles{})
	require.NoError(t, err)

	artifactAuthority := renderartifact.NewAuthority()
	builder, err := renderartifact.NewBuilder(artifactAuthority, nil)
	require.NoError(t, err)
	artifacts, err := builder.Build()
	require.NoError(t, err)
	outputAuthority, err := renderoutput.NewAuthority(renderplan.NewAuthority(), artifactAuthority)
	require.NoError(t, err)
	_, err = renderoutput.NewSnapshot(outputAuthority, config, plan, artifacts, nil)
	require.NoError(t, err)
}

// Every plan path is the base-relative string the config references the file
// by, which is also HAProxy's runtime name for it — a static name, a name a
// template registered, and a path the file registry already resolved all
// converge on it.
func TestPlanRegistryPlanPathsAreConfigReferences(t *testing.T) {
	registry := NewPlanRegistry(&templating.PathResolver{
		BaseDir: "/etc/haproxy", MapsDir: "maps", SSLDir: "ssl", CRTListDir: "general", GeneralDir: "general",
	})
	require.NoError(t, registry.MapMeta("host.map", false))

	plan, err := registry.Plan("global\n", &dataplane.AuxiliaryFiles{
		MapFiles:        []auxiliaryfiles.MapFile{{Path: "host.map", Content: "a b\n"}},
		SSLCertificates: []auxiliaryfiles.SSLCertificate{{Path: "api.example.com.pem"}, {Path: "ssl/other_pem.pem"}},
		CRTListFiles:    []auxiliaryfiles.CRTListFile{{Path: "general/list.txt"}},
		GeneralFiles:    []auxiliaryfiles.GeneralFile{{Path: "general/503.http"}},
	})
	require.NoError(t, err)

	paths := make([]string, 0, len(plan.Files))
	for _, file := range plan.Files {
		paths = append(paths, file.Path)
	}
	assert.Equal(t, []string{"general/503.http", "general/list.txt", "haproxy.cfg", "maps/host.map",
		"ssl/api_example_com.pem", "ssl/other_pem.pem"}, paths)

	require.Contains(t, plan.Maps, "maps/host.map")
	assert.Equal(t, "maps/host.map", plan.Maps["maps/host.map"].Path)
	assert.False(t, plan.Maps["maps/host.map"].Ordered, "meta declared by the bare name applies to the resolved path")
	resolved, err := registry.MapPath("host.map")
	require.NoError(t, err)
	assert.Equal(t, "maps/host.map", resolved)
}

// A secret file deploys as a sealed general file that never reloads HAProxy.
func TestPlanRegistryPlanSealsSecretFiles(t *testing.T) {
	registry := NewPlanRegistry(&templating.PathResolver{BaseDir: "/etc/haproxy", SecretDir: "secrets"})
	plan, err := registry.Plan("global\n", &dataplane.AuxiliaryFiles{
		SecretFiles: []auxiliaryfiles.SecretFile{{Path: "secrets/x", Content: "hunter2", Seal: "seal"}},
	})
	require.NoError(t, err)
	require.Len(t, plan.Files, 2)
	assert.Equal(t, renderplan.File{
		Path: "secrets/x", Kind: renderplan.FileKindGeneral, Digest: renderplan.DigestString("hunter2"),
		Size: 7, Seal: "seal", Content: "hunter2", ContentKnown: true,
	}, plan.Files[1])
}

// A resolved path resolves to itself, for every kind: the registry resolves a
// map's path once in MapMeta and again in Plan, and a certificate name may
// arrive already sanitised from the file registry.
func TestPlanRegistryFilePathIsIdempotent(t *testing.T) {
	registry := NewPlanRegistry(&templating.PathResolver{
		BaseDir: "/etc/haproxy", MapsDir: "maps", SSLDir: "ssl", CRTListDir: "general", GeneralDir: "general",
	})
	for _, tc := range []struct{ kind, name string }{
		{"map", "host.map"}, {"map", "maps/host.map"},
		{"cert", "api.example.com.pem"}, {"cert", "ssl/api_example_com.pem"},
		{"crt-list", "list.txt"}, {"crt-list", "general/list.txt"},
	} {
		once, err := registry.filePath(tc.name, tc.kind)
		require.NoError(t, err, tc.name)
		twice, err := registry.filePath(once, tc.kind)
		require.NoError(t, err, once)
		assert.Equal(t, once, twice, "%s %q", tc.kind, tc.name)
	}
}

// A name the resolver refuses is an error, never a plan with an unresolved
// path that no runtime name would match.
func TestPlanRegistryPlanRefusesAnUnresolvableName(t *testing.T) {
	registry := NewPlanRegistry(&templating.PathResolver{
		BaseDir: "/etc/haproxy", MapsDir: "maps", SSLDir: "ssl", CRTListDir: "general", GeneralDir: "general",
	})
	_, err := registry.Plan("global\n", &dataplane.AuxiliaryFiles{
		MapFiles: []auxiliaryfiles.MapFile{{Path: "../escape.map", Content: "a b\n"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escape.map")
	require.Error(t, registry.MapMeta("../escape.map", false))
}

func TestPlanRegistryPlanCurrentConfigRoundTrip(t *testing.T) {
	registry := NewPlanRegistry(nil)
	token, err := registry.Backend(map[string]any{
		"name":    "be_app",
		"servers": []any{map[string]any{"name": "SRV_1", "address": "10.0.0.1", "port": 8080}},
	}, "backend be_app\n")
	require.NoError(t, err)
	_, _, err = registry.Assemble(context.Background(), token, nil)
	require.NoError(t, err)

	plan, err := registry.Plan("", nil)
	require.NoError(t, err)
	current := plan.CurrentConfig()

	require.Contains(t, current.ServerIndex, "be_app")
	server := current.ServerIndex["be_app"]["SRV_1"]
	assert.Equal(t, "10.0.0.1", server.Address)
	require.NotNil(t, server.Port)
	assert.Equal(t, int64(8080), *server.Port)
}

func TestNearestKey(t *testing.T) {
	tests := []struct {
		name    string
		unknown string
		want    string
	}{
		{name: "typo", unknown: "bodyy", want: "body"},
		{name: "case", unknown: "Servers", want: "servers"},
		{name: "transposition", unknown: "hasType", want: "hashType"},
		{name: "nothing close", unknown: "frontendish", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, nearestKey(tc.unknown, backendRecordKeys))
		})
	}
}
