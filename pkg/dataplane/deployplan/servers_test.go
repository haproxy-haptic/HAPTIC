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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// TestDiffServerValueChanges covers the changes rule 4 applies in place.
func TestDiffServerValueChanges(t *testing.T) {
	weighted := srv("SRV_1", "10.0.0.1", 8080)
	weighted.Weight = ptr(10)
	reweighted := srv("SRV_1", "10.0.0.1", 8080)
	reweighted.Weight = ptr(20)
	unweighted := srv("SRV_1", "10.0.0.1", 8080)
	disabled := srv("SRV_1", "10.0.0.1", 8080)
	disabled.Disabled = true
	rekeyworded := srv("SRV_1", "10.0.0.1", 8080)
	rekeyworded.Extra = []renderplan.KeywordArg{{Name: "maxqueue", Args: []string{"100"}}}
	limited := srvWith("SRV_1", "10.0.0.1", maxconn("1500"))
	halved := srvWith("SRV_1", "10.0.0.1", maxconn("750"))
	halvedAndRequeued := srvWith("SRV_1", "10.0.0.1", maxconn("750"), renderplan.KeywordArg{Name: "maxqueue", Args: []string{"5"}})
	rehomed := srv("SRV_1", "10.0.0.9", 9090)
	hostnamed := srv("SRV_1", "api.svc.cluster.local", 8080)

	tests := []struct {
		name    string
		before  renderplan.Server
		after   renderplan.Server
		want    []string
		verdict deployplan.Verdict
		reason  string
	}{
		{
			name:    "weight change",
			before:  weighted,
			after:   reweighted,
			want:    []string{api.OpServerSetWeight},
			verdict: deployplan.VerdictRuntime,
		},
		{
			name:    "address and port change",
			before:  srv("SRV_1", "10.0.0.1", 8080),
			after:   rehomed,
			want:    []string{api.OpServerSetAddr},
			verdict: deployplan.VerdictRuntime,
		},
		{
			name:    "server disabled",
			before:  srv("SRV_1", "10.0.0.1", 8080),
			after:   disabled,
			want:    []string{api.OpServerSetState},
			verdict: deployplan.VerdictRuntime,
		},
		{
			name:    "server enabled again",
			before:  disabled,
			after:   srv("SRV_1", "10.0.0.1", 8080),
			want:    []string{api.OpServerEnable},
			verdict: deployplan.VerdictRuntime,
		},
		{
			name:    "nothing changed",
			before:  srv("SRV_1", "10.0.0.1", 8080),
			after:   srv("SRV_1", "10.0.0.1", 8080),
			verdict: deployplan.VerdictFileOnly,
		},
		{
			name:    "keyword change has no runtime command",
			before:  srv("SRV_1", "10.0.0.1", 8080),
			after:   rekeyworded,
			verdict: deployplan.VerdictReload,
			reason:  "keywords changed",
		},
		{
			name:    "maxconn change",
			before:  limited,
			after:   halved,
			want:    []string{api.OpServerSetMaxconn},
			verdict: deployplan.VerdictRuntime,
		},
		{
			name:    "maxconn added",
			before:  srv("SRV_1", "10.0.0.1", 8080),
			after:   halved,
			want:    []string{api.OpServerSetMaxconn},
			verdict: deployplan.VerdictRuntime,
		},
		{
			name:    "dropped maxconn has no runtime command",
			before:  limited,
			after:   srv("SRV_1", "10.0.0.1", 8080),
			verdict: deployplan.VerdictReload,
			reason:  "maxconn keyword was dropped",
		},
		{
			name:    "maxconn change next to another keyword change",
			before:  limited,
			after:   halvedAndRequeued,
			verdict: deployplan.VerdictReload,
			reason:  "keywords changed",
		},
		{
			name:    "dropped weight has no runtime command",
			before:  weighted,
			after:   unweighted,
			verdict: deployplan.VerdictReload,
			reason:  "weight keyword was dropped",
		},
		{
			name:    "a hostname is not addressable at runtime",
			before:  srv("SRV_1", "10.0.0.1", 8080),
			after:   hostnamed,
			verdict: deployplan.VerdictReload,
			reason:  "is not an IP",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := basePlan(withBackend(dynBackend("be-a", tt.before)))
			next := basePlan(withBackend(dynBackend("be-a", tt.after)))

			got := deployplan.Diff(next, on34(prev))

			require.Equal(t, tt.verdict, got.Verdict, got.Reasons)
			assert.Equal(t, tt.want, kinds(got.Ops))
			if tt.reason != "" {
				reasonsContain(t, got.Reasons, tt.reason)
			}
		})
	}
}

// TestDiffServerMaxconnChangeKeepsTheProfile pins issue #474: a per-server
// maxconn that follows the HAProxy replica count changes at runtime.
func TestDiffServerMaxconnChangeKeepsTheProfile(t *testing.T) {
	prev := basePlan(withBackend(dynBackend("be-a",
		srvWith("SRV_1", "10.0.0.1", maxconn("1500")),
		srvWith("SRV_2", "10.0.0.2", maxconn("1500")))))
	next := basePlan(withBackend(dynBackend("be-a",
		srvWith("SRV_1", "10.0.0.1", maxconn("750")),
		srvWith("SRV_2", "10.0.0.2", maxconn("750")))))

	got := deployplan.Diff(next, on34(prev))

	require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
	require.Equal(t, []string{api.OpServerSetMaxconn, api.OpServerSetMaxconn}, kinds(got.Ops))
	for _, op := range got.Ops {
		require.NotNil(t, op.Maxconn)
		assert.Equal(t, 750, *op.Maxconn)
	}

	floored := func(value string) *renderplan.Plan {
		be := dynBackend("be-a", srvWith("SRV_1", "10.0.0.1", maxconn(value)))
		be.DefaultServer = []renderplan.KeywordArg{{Name: "minconn", Args: []string{"100"}}}
		return basePlan(withBackend(be))
	}
	got = deployplan.Diff(floored("750"), on34(floored("1500")))
	require.Equal(t, deployplan.VerdictReload, got.Verdict)
	reasonsContain(t, got.Reasons, "next to minconn")

	older := deployplan.CapsFor("3.4.3", []string{api.OpServerSetWeight})
	got = deployplan.Diff(next, &deployplan.Baseline{Applied: prev, Caps: older})

	require.Equal(t, deployplan.VerdictReload, got.Verdict)
	reasonsContain(t, got.Reasons, "the agent does not execute server_set_maxconn")
}

func TestDiffServerDisabledGoesToMaint(t *testing.T) {
	disabled := srv("SRV_1", "10.0.0.1", 8080)
	disabled.Disabled = true
	prev := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))))

	got := deployplan.Diff(basePlan(withBackend(dynBackend("be-a", disabled))), on34(prev))

	require.Len(t, got.Ops, 1)
	assert.Equal(t, api.OpServerSetState, got.Ops[0].Kind)
	assert.Equal(t, "maint", got.Ops[0].State)
}

// TestDiffServerLeavingMaintEnablesItsHealthCheck pins that a server the render
// enables again is taken out of MAINT with `enable server`, which starts the
// health check `set server state ready` would leave off for good.
func TestDiffServerLeavingMaintEnablesItsHealthCheck(t *testing.T) {
	tests := []struct {
		name       string
		defaults   []renderplan.KeywordArg
		extra      []renderplan.KeywordArg
		wantHealth bool
	}{
		{name: "a server without a check enables alone"},
		{
			name:       "the server's own check keyword enables the health check",
			extra:      []renderplan.KeywordArg{{Name: "check"}},
			wantHealth: true,
		},
		{
			name:       "a check inherited from default-server counts too",
			defaults:   []renderplan.KeywordArg{{Name: "check"}},
			wantHealth: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disabled := srv("SRV_1", "10.0.0.1", 8080)
			disabled.Disabled, disabled.Extra = true, tt.extra
			enabled := srv("SRV_1", "10.0.0.1", 8080)
			enabled.Extra = tt.extra
			before := dynBackend("be-a", disabled)
			before.DefaultServer = tt.defaults
			after := dynBackend("be-a", enabled)
			after.DefaultServer = tt.defaults

			got := deployplan.Diff(basePlan(withBackend(after)), on34(basePlan(withBackend(before))))

			require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
			require.Len(t, got.Ops, 1)
			assert.Equal(t, api.OpServerEnable, got.Ops[0].Kind)
			assert.Equal(t, tt.wantHealth, got.Ops[0].Health)
		})
	}
}

// TestDiffServerAdded covers what an add server needs to be composable.
func TestDiffServerAdded(t *testing.T) {
	checked := srv("SRV_2", "10.0.0.2", 8080)
	checked.Extra = []renderplan.KeywordArg{{Name: "check"}, {Name: "inter", Args: []string{"2s"}}}
	checked.GUID = "srv-2-guid"
	disabled := srv("SRV_2", "10.0.0.2", 8080)
	disabled.Disabled = true

	tests := []struct {
		name     string
		added    renderplan.Server
		balance  string
		hashType string
		version  string
		want     []string
		keywords []api.KeywordArg
		health   bool
		reason   string
	}{
		{
			name:    "plain server on 3.4",
			added:   srv("SRV_2", "10.0.0.2", 8080),
			version: "3.4.3",
			want:    []string{api.OpServerAdd, api.OpServerEnable},
		},
		{
			name:     "checked server carries init-state on 3.1 and up",
			added:    checked,
			version:  "3.4.3",
			want:     []string{api.OpServerAdd, api.OpServerEnable},
			keywords: []api.KeywordArg{{Name: "check"}, {Name: "inter", Args: []string{"2s"}}, {Name: "guid", Args: []string{"srv-2-guid"}}, {Name: "init-state", Args: []string{"up"}}},
			health:   true,
		},
		{
			name:     "3.0 takes no init-state",
			added:    checked,
			version:  "3.0.26",
			want:     []string{api.OpServerAdd, api.OpServerEnable},
			keywords: []api.KeywordArg{{Name: "check"}, {Name: "inter", Args: []string{"2s"}}, {Name: "guid", Args: []string{"srv-2-guid"}}},
			health:   true,
		},
		{
			name:    "a disabled server stays in MAINT",
			added:   disabled,
			version: "3.4.3",
			want:    []string{api.OpServerAdd},
		},
		{
			name:    "static-rr refuses dynamic servers",
			added:   srv("SRV_2", "10.0.0.2", 8080),
			balance: "static-rr",
			version: "3.4.3",
			reason:  `balance "static-rr" takes no dynamic server`,
		},
		{
			name:    "a map-based hash refuses dynamic servers",
			added:   srv("SRV_2", "10.0.0.2", 8080),
			balance: "hdr(Host)",
			version: "3.4.3",
			reason:  `balance "hdr(Host)" takes no dynamic server`,
		},
		{
			name:     "a consistent hash takes them",
			added:    srv("SRV_2", "10.0.0.2", 8080),
			balance:  "hdr(Host)",
			hashType: "consistent",
			version:  "3.4.3",
			want:     []string{api.OpServerAdd, api.OpServerEnable},
		},
		{
			name:    "no add server without the capability",
			added:   srv("SRV_2", "10.0.0.2", 8080),
			version: "2.9.0",
			reason:  "this HAProxy has no add server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))
			before.Balance, before.HashType = tt.balance, tt.hashType
			after := dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080), tt.added)
			after.Balance, after.HashType = tt.balance, tt.hashType
			base := &deployplan.Baseline{
				Applied: basePlan(withBackend(before)),
				Caps:    deployplan.CapsFor(tt.version, nil),
			}

			got := deployplan.Diff(basePlan(withBackend(after)), base)

			if tt.reason != "" {
				require.Equal(t, deployplan.VerdictReload, got.Verdict)
				reasonsContain(t, got.Reasons, tt.reason)
				return
			}
			require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
			assert.Equal(t, tt.want, kinds(got.Ops))
			if tt.keywords != nil {
				assert.Equal(t, tt.keywords, got.Ops[0].Keywords)
			}
			if len(tt.want) > 1 {
				assert.Equal(t, tt.health, got.Ops[1].Health)
			}
		})
	}
}

func TestDiffServerAddedMergesDefaultServer(t *testing.T) {
	added := srv("SRV_2", "10.0.0.2", 8080)
	added.Extra = []renderplan.KeywordArg{{Name: "maxconn", Args: []string{"50"}}}
	before := dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))
	before.DefaultServer = []renderplan.KeywordArg{
		{Name: "check"},
		{Name: "maxconn", Args: []string{"10"}},
	}
	after := dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080), added)
	after.DefaultServer = before.DefaultServer

	got := deployplan.Diff(basePlan(withBackend(after)), on34(basePlan(withBackend(before))))

	require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
	assert.Equal(t, []api.KeywordArg{
		{Name: "check"},
		{Name: "maxconn", Args: []string{"50"}},
		{Name: "init-state", Args: []string{"up"}},
	}, got.Ops[0].Keywords)
}

// TestDiffServerAddedCarriesWeight guards weighted routing: an add server must
// carry the server's structured weight, including weight 0, or the runtime
// collapses the distribution to equal and a zero-weight backend takes traffic.
func TestDiffServerAddedCarriesWeight(t *testing.T) {
	tests := []struct {
		name   string
		weight *int
		want   *api.KeywordArg // the weight keyword add server must carry, nil for none
	}{
		{name: "weight 70", weight: ptr(70), want: &api.KeywordArg{Name: "weight", Args: []string{"70"}}},
		{name: "weight 30", weight: ptr(30), want: &api.KeywordArg{Name: "weight", Args: []string{"30"}}},
		{name: "weight 0 gets no traffic", weight: ptr(0), want: &api.KeywordArg{Name: "weight", Args: []string{"0"}}},
		{name: "no weight leaves HAProxy's default", weight: nil, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			added := srv("SRV_2", "10.0.0.2", 8080)
			added.Weight = tt.weight
			before := dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))
			after := dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080), added)

			got := deployplan.Diff(basePlan(withBackend(after)), on34(basePlan(withBackend(before))))

			require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
			require.Equal(t, []string{api.OpServerAdd, api.OpServerEnable}, kinds(got.Ops))
			if tt.want == nil {
				assert.Empty(t, got.Ops[0].Keywords, "a weightless server must not carry a weight keyword")
				return
			}
			assert.Contains(t, got.Ops[0].Keywords, *tt.want)
		})
	}
}

func TestDiffServerRemoved(t *testing.T) {
	prev := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080), srv("SRV_2", "10.0.0.2", 8080))))
	next := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))))

	got := deployplan.Diff(next, on34(prev))

	require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
	assert.Equal(t, []string{api.OpServerDisable, api.OpServerWaitRemovable, api.OpServerDel}, kinds(got.Ops))
	assert.Equal(t, "SRV_2", got.Ops[0].Server)
}

func TestDiffServerRemovedAtPendingCap(t *testing.T) {
	base := on34(basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080), srv("SRV_2", "10.0.0.2", 8080)))))
	base.PendingServerDeletes = api.MaxPendingServerDeletes

	got := deployplan.Diff(basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080)))), base)

	require.Equal(t, deployplan.VerdictReload, got.Verdict)
	reasonsContain(t, got.Reasons, "1000 server deletes already pending")
}

// TestDiffServerDeletesCrossTheCapMidDiff pins that the cap counts the deletes
// this diff composes, not only the ones the pod already queued.
func TestDiffServerDeletesCrossTheCapMidDiff(t *testing.T) {
	before := dynBackend("be-a",
		srv("SRV_1", "10.0.0.1", 8080), srv("SRV_2", "10.0.0.2", 8080), srv("SRV_3", "10.0.0.3", 8080))
	base := on34(basePlan(withBackend(before)))
	base.PendingServerDeletes = api.MaxPendingServerDeletes - 1

	got := deployplan.Diff(basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080)))), base)

	require.Equal(t, deployplan.VerdictReload, got.Verdict)
	reasonsContain(t, got.Reasons, "1000 server deletes already pending")
}

func TestDiffBackendDeletesCrossTheCapMidDiff(t *testing.T) {
	prev := basePlan(
		withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))),
		withBackend(dynBackend("be-b", srv("SRV_2", "10.0.0.2", 8080))),
	)
	base := on34(prev)
	base.PendingBackendDeletes = api.MaxPendingBackendDeletes - 1

	got := deployplan.Diff(basePlan(), base)

	require.Equal(t, deployplan.VerdictReload, got.Verdict)
	reasonsContain(t, got.Reasons, "100 backend deletes already pending")
}

func TestDiffServerNameMustBeASafeToken(t *testing.T) {
	prev := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))))
	next := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080), srv("SRV;2", "10.0.0.2", 8080))))

	got := deployplan.Diff(next, on34(prev))

	require.Equal(t, deployplan.VerdictReload, got.Verdict)
	reasonsContain(t, got.Reasons, "not a safe runtime token")
}

func maxconn(value string) renderplan.KeywordArg {
	return renderplan.KeywordArg{Name: "maxconn", Args: []string{value}}
}

func srvWith(name, address string, extra ...renderplan.KeywordArg) renderplan.Server {
	server := srv(name, address, 8080)
	server.Extra = extra
	return server
}
