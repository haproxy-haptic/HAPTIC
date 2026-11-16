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

package deployplan_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

var orderCerts = []string{certPath, "certs/a.pem", "certs/a2.pem", "certs/b.pem", "certs/c.pem", "certs/w.pem"}

func crtEntry(cert string, filters ...string) renderplan.CRTListEntry {
	return renderplan.CRTListEntry{Cert: cert, SNIFilters: filters}
}

var (
	dflt    = crtEntry(certPath, "!*")
	entryA  = crtEntry("certs/a.pem", "a.example.com")
	entryB  = crtEntry("certs/b.pem", "b.example.com")
	entryC  = crtEntry("certs/c.pem", "c.example.com")
	entryW  = crtEntry("certs/w.pem", "*.example.com")
	unnamed = crtEntry("certs/b.pem")
)

func orderBaseline(entries ...renderplan.CRTListEntry) *deployplan.Baseline {
	base := on34(basePlan(withCRTList(renderplan.CRTList{Path: listPath, Entries: entries})))
	base.Inventory = api.Inventory{CRTLists: []string{listPath}, Certs: orderCerts}
	return base
}

func listPlan(entries ...renderplan.CRTListEntry) *renderplan.Plan {
	return basePlan(withCRTList(renderplan.CRTList{Path: listPath, Entries: entries}))
}

// TestDiffCRTListOrderMattersOnlyWhereSNIsCompete pins the rule: a runtime add
// lands last in the worker's lookup, which is the file's answer unless the
// default changes or the entry shares a lookup name with one it now precedes.
func TestDiffCRTListOrderMattersOnlyWhereSNIsCompete(t *testing.T) {
	tests := []struct {
		name   string
		before []renderplan.CRTListEntry
		after  []renderplan.CRTListEntry
		reason string // "" means runtime
	}{
		{
			name:   "a mid-list add with its own SNI",
			before: []renderplan.CRTListEntry{dflt, entryA, entryC},
			after:  []renderplan.CRTListEntry{dflt, entryA, entryB, entryC},
		},
		{
			name:   "an exact name ahead of a wildcard covering it, which the exact tree wins anyway",
			before: []renderplan.CRTListEntry{dflt, entryW, entryC},
			after:  []renderplan.CRTListEntry{dflt, entryB, entryW, entryC},
		},
		{
			name:   "an add that becomes the first entry",
			before: []renderplan.CRTListEntry{dflt, entryA},
			after:  []renderplan.CRTListEntry{entryB, dflt, entryA},
			reason: "the default certificate changes",
		},
		{
			name:   "an add ahead of an entry with the same SNI",
			before: []renderplan.CRTListEntry{dflt, entryA, entryC},
			after:  []renderplan.CRTListEntry{dflt, crtEntry("certs/b.pem", "a.example.com"), entryA, entryC},
			reason: "entries certs/b.pem and certs/a.pem both match SNI a.example.com",
		},
		{
			name:   "SNI names compete case-insensitively",
			before: []renderplan.CRTListEntry{dflt, entryA},
			after:  []renderplan.CRTListEntry{dflt, crtEntry("certs/b.pem", "A.Example.COM"), entryA},
			reason: "both match SNI a.example.com",
		},
		{
			name:   "an add behind an entry with the same SNI",
			before: []renderplan.CRTListEntry{dflt, entryA, entryC},
			after:  []renderplan.CRTListEntry{dflt, entryA, crtEntry("certs/b.pem", "a.example.com"), entryC},
		},
		{
			name:   "an add ahead of an entry with the same wildcard",
			before: []renderplan.CRTListEntry{dflt, entryW},
			after:  []renderplan.CRTListEntry{dflt, crtEntry("certs/b.pem", "*.example.com"), entryW},
			reason: "both match SNI *.example.com",
		},
		{
			name:   "a second explicit default ahead of the first one's '*'",
			before: []renderplan.CRTListEntry{crtEntry(certPath, "*"), entryA},
			after:  []renderplan.CRTListEntry{crtEntry(certPath, "*"), crtEntry("certs/b.pem", "*"), entryA},
		},
		{
			name:   "an add without SNI filters mid-list: its certificate's names are unknown",
			before: []renderplan.CRTListEntry{dflt, entryA},
			after:  []renderplan.CRTListEntry{dflt, unnamed, entryA},
			reason: "entry certs/b.pem has no SNI filter",
		},
		{
			name:   "an add without SNI filters past every entry",
			before: []renderplan.CRTListEntry{dflt, entryA},
			after:  []renderplan.CRTListEntry{dflt, entryA, unnamed},
		},
		{
			name:   "a mid-list add past an entry without SNI filters",
			before: []renderplan.CRTListEntry{crtEntry(certPath), entryA},
			after:  []renderplan.CRTListEntry{crtEntry(certPath), entryB, entryA},
		},
		{
			name:   "an options change on a non-default entry with its own SNI",
			before: []renderplan.CRTListEntry{dflt, entryA, entryC},
			after: []renderplan.CRTListEntry{dflt, {
				Cert: "certs/a.pem", Options: []renderplan.KeywordArg{{Name: "alpn", Args: []string{"h2"}}},
				SNIFilters: []string{"a.example.com"},
			}, entryC},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deployplan.Diff(listPlan(tt.after...), orderBaseline(tt.before...))

			if tt.reason == "" {
				require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
				assert.NotEmpty(t, got.Ops)
				return
			}
			require.Equal(t, deployplan.VerdictReload, got.Verdict)
			assert.Empty(t, got.Ops)
			reasonsContain(t, got.Reasons, tt.reason)
		})
	}
}

// TestDiffCRTListOrderFollowsTheWorkerAcrossDiffs pins the pitfall of a lookup
// order that has drifted from the file: after b was added mid-list, the worker
// meets c before b, so the next diff must not reason from file order alone.
func TestDiffCRTListOrderFollowsTheWorkerAcrossDiffs(t *testing.T) {
	first := deployplan.Diff(listPlan(dflt, entryA, entryB, entryC), orderBaseline(dflt, entryA, entryC))
	require.Equal(t, deployplan.VerdictRuntime, first.Verdict, first.Reasons)

	applied := []renderplan.CRTListEntry{dflt, entryA, entryB, entryC}
	sameAsB := crtEntry("certs/a2.pem", "b.example.com")
	sameAsC := crtEntry("certs/a2.pem", "c.example.com")
	tests := []struct {
		name   string
		after  []renderplan.CRTListEntry
		reason string
	}{
		{name: "behind the runtime-added entry it shares an SNI with", after: []renderplan.CRTListEntry{dflt, entryA, entryB, sameAsB, entryC}},
		{
			name:   "ahead of the runtime-added entry it shares an SNI with",
			after:  []renderplan.CRTListEntry{dflt, entryA, sameAsB, entryB, entryC},
			reason: "both match SNI b.example.com",
		},
		{
			name:   "ahead of an entry the worker loaded before the runtime add",
			after:  []renderplan.CRTListEntry{dflt, entryA, entryB, sameAsC, entryC},
			reason: "both match SNI c.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deployplan.Diff(listPlan(tt.after...), orderBaseline(applied...))

			if tt.reason == "" {
				require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
				return
			}
			require.Equal(t, deployplan.VerdictReload, got.Verdict)
			reasonsContain(t, got.Reasons, tt.reason)
		})
	}
}

// TestDiffCRTListOrderAfterStoreCommits pins that a commit moves entries too:
// commit ssl cert re-inserts the certificate's entries last, and commit ssl
// ca-file re-inserts its users in an order the plan does not know.
func TestDiffCRTListOrderAfterStoreCommits(t *testing.T) {
	cert := renderplan.File{Path: "certs/a.pem", Kind: renderplan.FileKindCert}
	ca := renderplan.File{Path: caPath, Kind: renderplan.FileKindCA}
	sameAsA := crtEntry("certs/a2.pem", "a.example.com")

	tests := []struct {
		name    string
		entries []renderplan.CRTListEntry
		file    renderplan.File
		reason  string
	}{
		{name: "a renewed certificate with its own SNI", entries: []renderplan.CRTListEntry{dflt, entryA, entryC}, file: cert},
		{
			name:    "a renewed certificate ahead of an entry with the same SNI",
			entries: []renderplan.CRTListEntry{dflt, entryA, sameAsA},
			file:    cert,
			reason:  "entries certs/a.pem and certs/a2.pem both match SNI a.example.com",
		},
		{name: "a CA file change where no entries compete", entries: []renderplan.CRTListEntry{dflt, entryA, entryC}, file: ca},
		{
			name:    "a CA file change where two entries compete",
			entries: []renderplan.CRTListEntry{dflt, entryA, sameAsA},
			file:    ca,
			reason:  "both match SNI a.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list := withCRTList(renderplan.CRTList{Path: listPath, Entries: tt.entries})
			before, after := withDigest(&tt.file, "before"), withDigest(&tt.file, "after")
			base := on34(basePlan(withFile(&before), list))
			base.Inventory = api.Inventory{CRTLists: []string{listPath}, Certs: orderCerts, CAFiles: []string{caPath}}

			got := deployplan.Diff(basePlan(withFile(&after), list), base)

			if tt.reason == "" {
				require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
				assert.Len(t, got.Ops, 1)
				return
			}
			require.Equal(t, deployplan.VerdictReload, got.Verdict)
			reasonsContain(t, got.Reasons, tt.reason)
		})
	}
}
