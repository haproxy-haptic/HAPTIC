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

package deployplan_test

import (
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

const (
	certPath = "certs/tls.pem"
	caPath   = "ca/bundle.pem"
	listPath = "certs/default.crt-list"
)

var loadedCerts = []string{certPath, "certs/other.pem", "certs/third.pem"}

// TestDiffCertificates covers rule 6 for the certificate and CA stores: a file
// the worker already holds is set, one it does not is created first.
func TestDiffCertificates(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		path      string
		inventory api.Inventory
		want      string
	}{
		{
			name:      "known certificate is replaced",
			kind:      renderplan.FileKindCert,
			path:      certPath,
			inventory: api.Inventory{Certs: []string{certPath}},
			want:      api.OpCertSet,
		},
		{
			name: "new certificate enters the store first",
			kind: renderplan.FileKindCert,
			path: certPath,
			want: api.OpCertNew,
		},
		{
			name:      "known CA file is replaced",
			kind:      renderplan.FileKindCA,
			path:      caPath,
			inventory: api.Inventory{CAFiles: []string{caPath}},
			want:      api.OpCASet,
		},
		{
			name: "new CA file enters the store first",
			kind: renderplan.FileKindCA,
			path: caPath,
			want: api.OpCANew,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := basePlan(withFile(&renderplan.File{Path: tt.path, Kind: tt.kind, Digest: "before"}))
			next := basePlan(withFile(&renderplan.File{Path: tt.path, Kind: tt.kind, Digest: "after"}))
			base := on34(prev)
			base.Inventory = tt.inventory

			got := deployplan.Diff(next, base)

			require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
			assert.Equal(t, []api.Op{{Kind: tt.want, Path: tt.path}}, got.Ops)
		})
	}
}

func TestDiffUnchangedCertificateIsNotTouched(t *testing.T) {
	plan := func() *renderplan.Plan {
		return basePlan(withFile(&renderplan.File{Path: certPath, Kind: renderplan.FileKindCert, Digest: "same"}))
	}
	base := on34(plan())
	base.Inventory = api.Inventory{Certs: []string{certPath}}

	got := deployplan.Diff(plan(), base)

	assert.Equal(t, deployplan.VerdictFileOnly, got.Verdict)
	assert.Empty(t, got.Ops)
}

// TestDiffCRTListEntries covers the crt-list half of rule 6.
func TestDiffCRTListEntries(t *testing.T) {
	first := renderplan.CRTListEntry{Cert: certPath, SNIFilters: []string{"a.example.com"}}
	second := renderplan.CRTListEntry{Cert: "certs/other.pem", SNIFilters: []string{"b.example.com"}}
	retuned := renderplan.CRTListEntry{
		Cert:       certPath,
		Options:    []renderplan.KeywordArg{{Name: "alpn", Args: []string{"h2,http/1.1"}}},
		SNIFilters: []string{"a.example.com"},
	}

	tests := []struct {
		name   string
		before []renderplan.CRTListEntry
		after  []renderplan.CRTListEntry
		want   []api.Op
	}{
		{
			name:   "entry added",
			before: []renderplan.CRTListEntry{first},
			after:  []renderplan.CRTListEntry{first, second},
			want: []api.Op{{
				Kind: api.OpCRTListAdd, Path: listPath, Cert: "certs/other.pem", SNIFilters: []string{"b.example.com"},
			}},
		},
		{
			name:   "entry removed",
			before: []renderplan.CRTListEntry{first, second},
			after:  []renderplan.CRTListEntry{first},
			want:   []api.Op{{Kind: api.OpCRTListDel, Path: listPath, Cert: "certs/other.pem"}},
		},
		{
			name:   "options changed on an entry other than the default",
			before: []renderplan.CRTListEntry{second, first},
			after:  []renderplan.CRTListEntry{second, retuned},
			want: []api.Op{
				{Kind: api.OpCRTListDel, Path: listPath, Cert: certPath},
				{
					Kind: api.OpCRTListAdd, Path: listPath, Cert: certPath,
					Options:    []api.KeywordArg{{Name: "alpn", Args: []string{"h2,http/1.1"}}},
					SNIFilters: []string{"a.example.com"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := basePlan(withCRTList(renderplan.CRTList{Path: listPath, Entries: tt.before}))
			next := basePlan(withCRTList(renderplan.CRTList{Path: listPath, Entries: tt.after}))
			base := on34(prev)
			base.Inventory = api.Inventory{CRTLists: []string{listPath}, Certs: loadedCerts}

			got := deployplan.Diff(next, base)

			require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
			assert.Equal(t, tt.want, got.Ops)
		})
	}
}

// TestDiffCRTListOrderIsNotReachablePerEntry pins that only a change the
// running list can reproduce stays runtime: `add ssl crt-list` appends, and the
// first entry is the certificate HAProxy serves without a matching SNI.
func TestDiffCRTListOrderIsNotReachablePerEntry(t *testing.T) {
	first := renderplan.CRTListEntry{Cert: certPath}
	other := renderplan.CRTListEntry{Cert: "certs/other.pem"}
	third := renderplan.CRTListEntry{Cert: "certs/third.pem"}
	retuned := renderplan.CRTListEntry{
		Cert:    certPath,
		Options: []renderplan.KeywordArg{{Name: "alpn", Args: []string{"h2"}}},
	}

	tests := []struct {
		name   string
		before []renderplan.CRTListEntry
		after  []renderplan.CRTListEntry
		reason string
	}{
		{
			name:   "a new default certificate is not an append",
			before: []renderplan.CRTListEntry{first},
			after:  []renderplan.CRTListEntry{other, first},
			reason: "the default certificate changes",
		},
		{
			name:   "retained entries that swap places",
			before: []renderplan.CRTListEntry{first, other},
			after:  []renderplan.CRTListEntry{other, first},
			reason: "the default certificate changes",
		},
		{
			name:   "an options change on an entry other than the last",
			before: []renderplan.CRTListEntry{first, other},
			after:  []renderplan.CRTListEntry{retuned, other},
			reason: "the default certificate changes",
		},
		{
			name:   "a certificate the line form cannot name",
			before: []renderplan.CRTListEntry{first},
			after:  []renderplan.CRTListEntry{first, {Cert: "certs/a;b.pem"}},
			reason: "is not a safe runtime token",
		},
		{
			name:   "an SNI filter the line form cannot name",
			before: []renderplan.CRTListEntry{first},
			after:  []renderplan.CRTListEntry{first, {Cert: "certs/other.pem", SNIFilters: []string{"a b.example.com"}}},
			reason: "is not a safe runtime token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := basePlan(withCRTList(renderplan.CRTList{Path: listPath, Entries: tt.before}))
			next := basePlan(withCRTList(renderplan.CRTList{Path: listPath, Entries: tt.after}))
			base := on34(prev)
			base.Inventory = api.Inventory{CRTLists: []string{listPath}, Certs: loadedCerts}

			got := deployplan.Diff(next, base)

			require.Equal(t, deployplan.VerdictReload, got.Verdict)
			assert.Empty(t, got.Ops)
			reasonsContain(t, got.Reasons, tt.reason)
		})
	}

	t.Run("an append past every retained entry stays runtime", func(t *testing.T) {
		prev := basePlan(withCRTList(renderplan.CRTList{
			Path: listPath, Entries: []renderplan.CRTListEntry{first, other},
		}))
		next := basePlan(withCRTList(renderplan.CRTList{
			Path: listPath, Entries: []renderplan.CRTListEntry{first, other, third},
		}))
		base := on34(prev)
		base.Inventory = api.Inventory{CRTLists: []string{listPath}, Certs: loadedCerts}

		got := deployplan.Diff(next, base)

		require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
		assert.Equal(t, []api.Op{{Kind: api.OpCRTListAdd, Path: listPath, Cert: "certs/third.pem"}}, got.Ops)
	})
}

func TestDiffCRTListFileAppearingOrDisappearingReloads(t *testing.T) {
	withList := basePlan(withCRTList(renderplan.CRTList{Path: listPath}))
	without := basePlan()

	added := deployplan.Diff(withList, on34(without))
	assert.Equal(t, deployplan.VerdictReload, added.Verdict)
	reasonsContain(t, added.Reasons, "added, which only a reload puts into the config")

	removed := deployplan.Diff(without, on34(withList))
	assert.Equal(t, deployplan.VerdictReload, removed.Verdict)
	reasonsContain(t, removed.Reasons, "removed, which only a reload takes out of the config")
}

func TestDiffCRTListWithoutEntriesReloads(t *testing.T) {
	prev := basePlan(withFile(&renderplan.File{Path: listPath, Kind: renderplan.FileKindCRTList, Digest: "before"}))
	next := basePlan(withFile(&renderplan.File{Path: listPath, Kind: renderplan.FileKindCRTList, Digest: "after"}))
	base := on34(prev)
	base.Inventory = api.Inventory{CRTLists: []string{listPath}, Certs: loadedCerts}

	got := deployplan.Diff(next, base)

	require.Equal(t, deployplan.VerdictReload, got.Verdict)
	reasonsContain(t, got.Reasons, "crt-list entries are not declared by the render yet")
}

// TestDiffCertificateCreatedInThisDiffCountsAsLoaded pins that a server keyword
// may name a certificate the same diff creates: the agent folds an object it
// created into its inventory, so both ends see the same runtime store.
func TestDiffCertificateCreatedInThisDiffCountsAsLoaded(t *testing.T) {
	added := srv("SRV_2", "10.0.0.2", 8080)
	added.Extra = []renderplan.KeywordArg{{Name: "crt", Args: []string{path.Base(certPath)}}}
	cert := renderplan.File{Path: certPath, Kind: renderplan.FileKindCert}
	certBefore := withDigest(&cert, "before")
	certAfter := withDigest(&cert, "after")
	prev := basePlan(
		withFile(&certBefore),
		withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))),
	)
	next := basePlan(
		withFile(&certAfter),
		withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080), added)),
	)

	got := deployplan.Diff(next, on34(prev))

	require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
	assert.Equal(t, []string{api.OpServerAdd, api.OpServerEnable, api.OpCertNew}, kinds(got.Ops))
}

// TestDiffServerCRTResolvesUnderCRTBase pins that a server's `crt` names its
// certificate the way the config line does, bare under crt-base, and still
// counts as loaded: HAProxy joins crt-base onto add server's argument too. A
// bare name no registered certificate resolves to stays a reload.
func TestDiffServerCRTResolvesUnderCRTBase(t *testing.T) {
	cert := renderplan.File{Path: certPath, Kind: renderplan.FileKindCert, Digest: "same"}
	anchor := srv("SRV_1", "10.0.0.1", 8080)
	base := on34(basePlan(withFile(&cert), withBackend(dynBackend("be-a", anchor))))
	base.Inventory = api.Inventory{Certs: []string{certPath}}

	bare := srv("SRV_2", "10.0.0.2", 8080)
	bare.Extra = []renderplan.KeywordArg{{Name: "crt", Args: []string{path.Base(certPath)}}}
	got := deployplan.Diff(basePlan(withFile(&cert), withBackend(dynBackend("be-a", anchor, bare))), base)
	require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
	assert.Equal(t, []string{api.OpServerAdd, api.OpServerEnable}, kinds(got.Ops))

	unknown := srv("SRV_3", "10.0.0.3", 8080)
	unknown.Extra = []renderplan.KeywordArg{{Name: "crt", Args: []string{"other.pem"}}}
	got = deployplan.Diff(basePlan(withFile(&cert), withBackend(dynBackend("be-a", anchor, unknown))), base)
	require.Equal(t, deployplan.VerdictReload, got.Verdict, got.Reasons)
	reasonsContain(t, got.Reasons, "keyword crt cannot be set on a dynamic server")
}

func TestDiffCRTListNotLoadedIsWrittenOnly(t *testing.T) {
	entryA := renderplan.CRTListEntry{Cert: certPath}
	prev := basePlan(withCRTList(renderplan.CRTList{Path: listPath, Entries: []renderplan.CRTListEntry{entryA}}))
	next := basePlan(withCRTList(renderplan.CRTList{
		Path:    listPath,
		Entries: []renderplan.CRTListEntry{entryA, {Cert: "certs/other.pem"}},
	}))

	got := deployplan.Diff(next, on34(prev))

	assert.Equal(t, deployplan.VerdictFileOnly, got.Verdict)
	assert.Empty(t, got.Ops)
	reasonsContain(t, got.Reasons, "is not loaded at runtime, its file is written only")
}

// TestDiffCRTListDelNamesTheStoreEntry pins that a removed bare line token is
// deleted by its store name, even when its certificate leaves the plan with it.
func TestDiffCRTListDelNamesTheStoreEntry(t *testing.T) {
	cert := renderplan.File{Path: "ssl/only.pem", Kind: renderplan.FileKindCert, Digest: "same"}
	prev := basePlan(withFile(&cert), withCRTList(renderplan.CRTList{
		Path: listPath, Entries: []renderplan.CRTListEntry{{Cert: "only.pem"}},
	}))
	base := on34(prev)
	base.Inventory = api.Inventory{CRTLists: []string{listPath}, Certs: []string{"ssl/only.pem"}}

	got := deployplan.Diff(basePlan(withCRTList(renderplan.CRTList{Path: listPath})), base)

	require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
	assert.Equal(t, []api.Op{{Kind: api.OpCRTListDel, Path: listPath, Cert: "ssl/only.pem"}}, got.Ops)
}

// TestDiffCRTListAddNeedsItsCertificateInTheStore pins that an added entry
// reaches the worker only when its certificate is loaded or created first:
// add ssl crt-list resolves the line against the runtime store.
func TestDiffCRTListAddNeedsItsCertificateInTheStore(t *testing.T) {
	first := renderplan.CRTListEntry{Cert: certPath}
	added := renderplan.CRTListEntry{Cert: "certs/other.pem"}
	prev := basePlan(withCRTList(renderplan.CRTList{Path: listPath, Entries: []renderplan.CRTListEntry{first}}))
	base := on34(prev)
	base.Inventory = api.Inventory{CRTLists: []string{listPath}, Certs: []string{certPath}}

	got := deployplan.Diff(basePlan(withCRTList(renderplan.CRTList{
		Path: listPath, Entries: []renderplan.CRTListEntry{first, added},
	})), base)
	require.Equal(t, deployplan.VerdictReload, got.Verdict)
	reasonsContain(t, got.Reasons, "crt certs/other.pem is not loaded at runtime")

	got = deployplan.Diff(basePlan(
		withFile(&renderplan.File{Path: "certs/other.pem", Kind: renderplan.FileKindCert, Digest: "new"}),
		withCRTList(renderplan.CRTList{Path: listPath, Entries: []renderplan.CRTListEntry{first, added}}),
	), base)
	require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
	assert.Equal(t, []string{api.OpCertNew, api.OpCRTListAdd}, kinds(got.Ops))
}
