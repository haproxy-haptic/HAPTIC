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
	"net"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// Keywords the composer reasons about by name.
const (
	keywordCheck     = "check"
	keywordGUID      = "guid"
	keywordInitState = "init-state"
	keywordWeight    = "weight"
	keywordMaxconn   = "maxconn"
	keywordMinconn   = "minconn"
	keywordCAFile    = "ca-file"
	keywordCRT       = "crt"
	keywordCRLFile   = "crl-file"
)

// addServerKeywords is what `add server` accepts on HAProxy 3.0 to 3.4,
// verified command by command (plan, "Verified facts"). Everything outside it
// — enabled, no-check, resolvers, init-addr, sni-auto, no-ssl — makes the
// server, and therefore its backend, structural.
var addServerKeywords = map[string]bool{
	keywordCheck: true, "inter": true, "fastinter": true, "downinter": true,
	"rise": true, "fall": true, "port": true, "addr": true, keywordWeight: true,
	keywordMaxconn: true, "maxqueue": true, keywordMinconn: true, "backup": true,
	"cookie": true, keywordGUID: true, "ssl": true, "sni": true, "verify": true,
	"verifyhost": true, keywordCAFile: true, keywordCRT: true, keywordCRLFile: true,
	"alpn": true, "ciphers": true, "ciphersuites": true, "ssl-min-ver": true,
	"ssl-max-ver": true, "proto": true, "send-proxy": true, "send-proxy-v2": true,
	"slowstart": true, "agent-check": true, "agent-port": true, "agent-inter": true,
	"agent-send": true, "on-marked-down": true, "observe": true, "disabled": true,
	keywordInitState: true,
}

// dynamicBalance reports whether the algorithm accepts servers added at
// runtime: static-rr and the map-based hash families refuse them.
func dynamicBalance(balance, hashType string) bool {
	switch algorithm(balance) {
	case "", "roundrobin", "leastconn", "random", "first":
		return true
	case "static-rr":
		return false
	default:
		return algorithm(hashType) == "consistent"
	}
}

func balanceOf(be *renderplan.Backend) string {
	if be.Balance == "" {
		return "roundrobin"
	}
	return be.Balance
}

// algorithm is the bare keyword of a balance or hash-type setting, without its
// arguments: "hdr(Host)" is "hdr", "consistent sdbm" is "consistent".
func algorithm(setting string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(setting), " ")
	name, _, _ = strings.Cut(name, "(")
	return name
}

// mergeKeywords folds the backend's default-server keywords into the server's
// own, server level winning, because add server inherits no default-server.
func mergeKeywords(defaults, extra []renderplan.KeywordArg) []api.KeywordArg {
	merged := make([]api.KeywordArg, 0, len(defaults)+len(extra))
	merged = append(merged, apiKeywords(defaults)...)
	for _, kw := range extra {
		replacement := api.KeywordArg{Name: kw.Name, Args: slices.Clone(kw.Args)}
		if at := slices.IndexFunc(merged, func(m api.KeywordArg) bool { return m.Name == kw.Name }); at >= 0 {
			merged[at] = replacement
			continue
		}
		merged = append(merged, replacement)
	}
	return merged
}

// apiKeywords copies render keywords onto the wire type.
func apiKeywords(keywords []renderplan.KeywordArg) []api.KeywordArg {
	if len(keywords) == 0 {
		return nil
	}
	copied := make([]api.KeywordArg, 0, len(keywords))
	for _, kw := range keywords {
		copied = append(copied, api.KeywordArg{Name: kw.Name, Args: slices.Clone(kw.Args)})
	}
	return copied
}

func hasKeyword(keywords []api.KeywordArg, name string) bool {
	return slices.ContainsFunc(keywords, func(kw api.KeywordArg) bool { return kw.Name == name })
}

// ineligibleKeyword names the first keyword `add server` would refuse: one
// outside the verified set, init-state below 3.1, a file the running worker
// has not loaded, or an argument that is not a safe CLI token.
func (c *composer) ineligibleKeyword(keywords []api.KeywordArg) string {
	for i := range keywords {
		kw := &keywords[i]
		switch {
		case !addServerKeywords[kw.Name]:
			return kw.Name
		case kw.Name == keywordInitState && !c.caps.ServerInitState:
			return kw.Name
		case !api.SafeToken(kw.Name) || !allSafeTokens(kw.Args):
			return kw.Name
		case !c.filesLoaded(kw):
			return kw.Name
		}
	}
	return ""
}

// filesLoaded reports whether a file-referencing keyword names something the
// running worker already loaded; HAProxy refuses add server otherwise.
func (c *composer) filesLoaded(kw *api.KeywordArg) bool {
	var loaded []string
	switch kw.Name {
	case keywordCRT:
		loaded = c.inventory.Certs
	case keywordCAFile:
		loaded = c.inventory.CAFiles
	case keywordCRLFile:
		loaded = c.inventory.CRLFiles
	default:
		return true
	}
	if len(kw.Args) != 1 {
		return false
	}
	name := kw.Args[0]
	if kw.Name == keywordCRT && !slices.Contains(loaded, name) && !c.created[name] {
		name = c.underCRTBase(name)
	}
	return slices.Contains(loaded, name) || c.created[name]
}

// underCRTBase resolves a crt argument as HAProxy does on the config line and
// on add server alike: a relative path is joined onto crt-base, which the
// config points at the plan's certificate directory.
func (c *composer) underCRTBase(arg string) string {
	if c.certDir == "" || strings.HasPrefix(arg, "/") {
		return arg
	}
	return c.certDir + "/" + arg
}

func allSafeTokens(args []string) bool {
	for _, arg := range args {
		if !api.SafeToken(arg) {
			return false
		}
	}
	return true
}

// endpointReason rejects an address the runtime API cannot take: add server
// and set server addr want a literal IP and a real port.
func endpointReason(address string, port int) string {
	if net.ParseIP(address) == nil {
		return "address " + address + " is not an IP, which the runtime API requires"
	}
	if port < 1 || port > 65535 {
		return "the port is outside 1-65535"
	}
	return ""
}
