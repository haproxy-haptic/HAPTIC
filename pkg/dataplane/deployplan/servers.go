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

package deployplan

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// stateMaint is the set server state a render's disabled server asks for;
// leaving it again is `enable server`, not the opposite set.
const stateMaint = "maint"

// composer turns render records into ops for one pod. Every method returns a
// reason instead of ops when HAProxy would refuse the command; the caller
// decides whether that reason reloads (Diff) or drops the op (InPlace).
type composer struct {
	caps      Caps
	inventory *api.Inventory
	// created are the runtime-store objects this diff creates before anything
	// can name them; the agent folds them into its inventory the same way.
	created map[string]bool
	// certDir is where the plan's certificates live, which the config also
	// names as crt-base; "" when the plan registers none.
	certDir string
	// pendingServerDeletes and pendingBackendDeletes are the pod's baseline plus
	// what this diff has composed, because the cap is on the queue, not the ACK.
	pendingServerDeletes  int
	pendingBackendDeletes int
}

// addServer composes add server plus the enable that takes it out of MAINT.
func (c *composer) addServer(be *renderplan.Backend, srv *renderplan.Server) (ops []api.Op, reason string) {
	switch {
	case !c.caps.DynamicServers:
		return nil, "this HAProxy has no add server"
	case !dynamicBalance(be.Balance, be.HashType):
		return nil, fmt.Sprintf("balance %q takes no dynamic server", balanceOf(be))
	case !api.SafeToken(srv.Name):
		return nil, "the name is not a safe runtime token"
	}
	if reason := endpointReason(srv.Address, srv.Port); reason != "" {
		return nil, reason
	}
	keywords := mergeKeywords(be.DefaultServer, srv.Extra)
	if bad := c.ineligibleKeyword(keywords); bad != "" {
		return nil, fmt.Sprintf("keyword %s cannot be set on a dynamic server", bad)
	}
	keywords = c.withRuntimeKeywords(keywords, srv)
	add := api.Op{
		Kind:     api.OpServerAdd,
		Backend:  be.Name,
		Server:   srv.Name,
		Address:  srv.Address,
		Port:     srv.Port,
		Keywords: keywords,
	}
	if srv.Disabled {
		// A dynamic server starts in MAINT, which is what disabled asks for.
		return []api.Op{add}, ""
	}
	return []api.Op{add, {
		Kind:    api.OpServerEnable,
		Backend: be.Name,
		Server:  srv.Name,
		Health:  hasKeyword(keywords, keywordCheck),
	}}, ""
}

// updateServer composes the value changes HAProxy applies in place. It takes
// the backend because leaving MAINT needs the merged keyword set: `enable
// health` is only accepted on a server that carries `check`.
func (c *composer) updateServer(be *renderplan.Backend, prev, next *renderplan.Server) (ops []api.Op, reason string) {
	prevExtra, prevMaxconn := splitMaxconn(prev.Extra)
	nextExtra, nextMaxconn := splitMaxconn(next.Extra)
	if prev.GUID != next.GUID || !slices.EqualFunc(prevExtra, nextExtra, sameKeyword) {
		return nil, "keywords changed, which set server cannot express"
	}
	backend := be.Name
	ops = make([]api.Op, 0, 4)
	if prev.Address != next.Address || prev.Port != next.Port {
		if reason := endpointReason(next.Address, next.Port); reason != "" {
			return nil, reason
		}
		ops = append(ops, api.Op{
			Kind: api.OpServerSetAddr, Backend: backend, Server: next.Name,
			Address: next.Address, Port: next.Port,
		})
	}
	if !sameWeight(prev.Weight, next.Weight) {
		if next.Weight == nil {
			return nil, "the weight keyword was dropped, which set server cannot express"
		}
		weight := *next.Weight
		ops = append(ops, api.Op{Kind: api.OpServerSetWeight, Backend: backend, Server: next.Name, Weight: &weight})
	}
	if !slices.EqualFunc(prevMaxconn, nextMaxconn, sameKeyword) {
		// set maxconn server also rewrites minconn when it equals maxconn.
		if hasKeyword(mergeKeywords(be.DefaultServer, next.Extra), keywordMinconn) {
			return nil, "maxconn changed next to minconn, which set maxconn server cannot express"
		}
		maxconn, reason := maxconnValue(nextMaxconn)
		if reason != "" {
			return nil, reason
		}
		ops = append(ops, api.Op{Kind: api.OpServerSetMaxconn, Backend: backend, Server: next.Name, Maxconn: &maxconn})
	}
	switch {
	case prev.Disabled == next.Disabled:
	case next.Disabled:
		ops = append(ops, api.Op{
			Kind: api.OpServerSetState, Backend: backend, Server: next.Name, State: stateMaint,
		})
	default:
		// `set server state ready` leaves a health check that never started
		// disabled, so a dynamic server added in MAINT would take traffic with
		// no check at all; `enable server` clears the same state and takes the
		// health check with it.
		ops = append(ops, api.Op{
			Kind: api.OpServerEnable, Backend: backend, Server: next.Name,
			Health: hasKeyword(mergeKeywords(be.DefaultServer, next.Extra), keywordCheck),
		})
	}
	if len(ops) == 0 {
		return nil, ""
	}
	return ops, ""
}

// removeServer composes the deferred delete: stop traffic, wait for the last
// session, then delete. The agent owns the shutdown-sessions retry.
func (c *composer) removeServer(backend string, srv *renderplan.Server) (ops []api.Op, reason string) {
	switch {
	case !c.caps.DynamicServers:
		return nil, "this HAProxy has no del server"
	case c.pendingServerDeletes >= api.MaxPendingServerDeletes:
		return nil, fmt.Sprintf("%d server deletes already pending", c.pendingServerDeletes)
	case !api.SafeToken(srv.Name):
		return nil, "the name is not a safe runtime token"
	}
	c.pendingServerDeletes++
	return []api.Op{
		{Kind: api.OpServerDisable, Backend: backend, Server: srv.Name},
		{Kind: api.OpServerWaitRemovable, Backend: backend, Server: srv.Name, TimeoutMs: removableTimeoutMs},
		{Kind: api.OpServerDel, Backend: backend, Server: srv.Name},
	}, ""
}

// withRuntimeKeywords adds what the record expresses outside the keyword list:
// the server's weight and GUID and, where HAProxy takes it, an immediately
// usable server. add server inherits no default-server, so a nil weight leaves
// HAProxy's default of 1; a weight of 0 is a real weight that must be emitted.
func (c *composer) withRuntimeKeywords(keywords []api.KeywordArg, srv *renderplan.Server) []api.KeywordArg {
	if srv.Weight != nil && !hasKeyword(keywords, keywordWeight) {
		keywords = append(keywords, api.KeywordArg{Name: keywordWeight, Args: []string{strconv.Itoa(*srv.Weight)}})
	}
	if srv.GUID != "" && !hasKeyword(keywords, keywordGUID) {
		keywords = append(keywords, api.KeywordArg{Name: keywordGUID, Args: []string{srv.GUID}})
	}
	if c.caps.ServerInitState && hasKeyword(keywords, keywordCheck) && !hasKeyword(keywords, keywordInitState) {
		keywords = append(keywords, api.KeywordArg{Name: keywordInitState, Args: []string{"up"}})
	}
	return keywords
}

// splitMaxconn separates the maxconn keyword, which `set maxconn server`
// changes in place, from the keywords only a reload can change.
func splitMaxconn(extra []renderplan.KeywordArg) (rest, maxconn []renderplan.KeywordArg) {
	for _, kw := range extra {
		if kw.Name == keywordMaxconn {
			maxconn = append(maxconn, kw)
		} else {
			rest = append(rest, kw)
		}
	}
	return rest, maxconn
}

func maxconnValue(keywords []renderplan.KeywordArg) (maxconn int, reason string) {
	if len(keywords) != 1 {
		return 0, "the maxconn keyword was dropped or repeated, which set maxconn server cannot express"
	}
	if len(keywords[0].Args) == 1 {
		if value, err := strconv.Atoi(keywords[0].Args[0]); err == nil && value >= 0 {
			return value, ""
		}
	}
	return 0, fmt.Sprintf("maxconn %v is not a connection count", keywords[0].Args)
}

func sameWeight(prev, next *int) bool {
	if prev == nil || next == nil {
		return prev == next
	}
	return *prev == *next
}

func sameKeyword(prev, next renderplan.KeywordArg) bool {
	return prev.Name == next.Name && slices.Equal(prev.Args, next.Args)
}
