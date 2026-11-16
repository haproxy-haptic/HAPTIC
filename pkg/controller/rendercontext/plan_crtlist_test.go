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

package rendercontext

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

const (
	crtListTestPath = "general/certificate-list.txt"
	defaultLine     = "default.pem [ocsp-update on] !*\n"
	apiLine         = "api.pem [ocsp-update on] api.example.com\n"
	webLine         = "web.pem [ocsp-update on] web.example.com\n"
)

func crtListPlan(t *testing.T, list string, certs ...string) *renderplan.Plan {
	t.Helper()
	registry := NewPlanRegistry(&templating.PathResolver{
		BaseDir: "/etc/haproxy", SSLDir: "ssl", CRTListDir: "general", GeneralDir: "general",
	})
	aux := &dataplane.AuxiliaryFiles{
		CRTListFiles: []auxiliaryfiles.CRTListFile{{Path: crtListTestPath, Content: list}},
	}
	for _, cert := range certs {
		aux.SSLCertificates = append(aux.SSLCertificates, auxiliaryfiles.SSLCertificate{Path: cert, Content: cert})
	}
	plan, err := registry.Plan("global\n", aux)
	require.NoError(t, err)
	return plan
}

func TestPlanDeclaresCRTListEntriesFromContent(t *testing.T) {
	plan := crtListPlan(t, defaultLine+apiLine, "default.pem", "api.pem")
	require.Contains(t, plan.CRTLists, crtListTestPath)
	onOCSP := []renderplan.KeywordArg{{Name: "ocsp-update", Args: []string{"on"}}}
	assert.Equal(t, []renderplan.CRTListEntry{
		{Cert: "default.pem", Options: onOCSP, SNIFilters: []string{"!*"}},
		{Cert: "api.pem", Options: onOCSP, SNIFilters: []string{"api.example.com"}},
	}, plan.CRTLists[crtListTestPath].Entries)

	unparsable := crtListPlan(t, defaultLine+"api.pem [frobnicate] api.example.com\n", "default.pem", "api.pem")
	assert.Empty(t, unparsable.CRTLists, "a list that does not parse exactly declares nothing")
}

// TestCRTListChangesFromRenderedContentStayRuntime runs the render's plan
// through the deploy decision: what the generator wrote reaches the worker
// entry by entry unless the order HAProxy serves from would change.
func TestCRTListChangesFromRenderedContentStayRuntime(t *testing.T) {
	onOCSP := []api.KeywordArg{{Name: "ocsp-update", Args: []string{"on"}}}
	loaded := func(applied *renderplan.Plan) *deployplan.Baseline {
		return &deployplan.Baseline{
			Applied: applied, Caps: deployplan.CapsFor("3.4.3", nil),
			Inventory: api.Inventory{
				CRTLists: []string{crtListTestPath}, Certs: []string{"ssl/default.pem", "ssl/api.pem"},
			},
		}
	}
	twoHosts := crtListPlan(t, defaultLine+apiLine, "default.pem", "api.pem")

	t.Run("a host added after the retained entries", func(t *testing.T) {
		next := crtListPlan(t, defaultLine+apiLine+webLine, "default.pem", "api.pem", "web.pem")
		got := deployplan.Diff(next, loaded(twoHosts))
		require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
		assert.Empty(t, got.Reasons)
		assert.Equal(t, []api.Op{
			{Kind: api.OpCertNew, Path: "ssl/web.pem"},
			{
				Kind: api.OpCRTListAdd, Path: crtListTestPath, Cert: "web.pem",
				Options: onOCSP, SNIFilters: []string{"web.example.com"},
			},
		}, got.Ops, "the certificate is created before the entry naming it")
	})

	t.Run("a host added ahead of a retained entry", func(t *testing.T) {
		next := crtListPlan(t, defaultLine+"app.pem [ocsp-update on] app.example.com\n"+apiLine,
			"default.pem", "app.pem", "api.pem")
		got := deployplan.Diff(next, loaded(twoHosts))
		require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
		require.Len(t, got.Ops, 2)
		assert.Equal(t, api.Op{
			Kind: api.OpCRTListAdd, Path: crtListTestPath, Cert: "app.pem",
			Options: onOCSP, SNIFilters: []string{"app.example.com"},
		}, got.Ops[1])
	})

	t.Run("a host removed", func(t *testing.T) {
		got := deployplan.Diff(crtListPlan(t, defaultLine, "default.pem"), loaded(twoHosts))
		require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
		assert.Equal(t, []api.Op{{Kind: api.OpCRTListDel, Path: crtListTestPath, Cert: "ssl/api.pem"}}, got.Ops,
			"del ssl crt-list names the store entry, not the bare line token")
	})

	t.Run("a new default certificate reloads", func(t *testing.T) {
		got := deployplan.Diff(crtListPlan(t, apiLine+defaultLine, "default.pem", "api.pem"), loaded(twoHosts))
		assert.Equal(t, deployplan.VerdictReload, got.Verdict)
		assert.Empty(t, got.Ops)
		assert.Contains(t, got.Reasons,
			"crt-list "+crtListTestPath+": the default certificate changes, which only a reload applies")
	})

	t.Run("an unparsable list reloads", func(t *testing.T) {
		next := crtListPlan(t, defaultLine+apiLine+"web.pem [frobnicate] web.example.com\n",
			"default.pem", "api.pem", "web.pem")
		got := deployplan.Diff(next, loaded(twoHosts))
		assert.Equal(t, deployplan.VerdictReload, got.Verdict)
		assert.Contains(t, got.Reasons,
			"crt-list "+crtListTestPath+" changed, but crt-list entries are not declared by the render yet")
	})
}
