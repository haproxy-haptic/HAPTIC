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

//go:build agentdocker

package agent

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

func TestCertificateOpsRunAtRuntime(t *testing.T) {
	e, s := converged(t)
	worker := e.workerPID()

	require.Equal(t, int64(1001), e.peerCertificate("default.test").SerialNumber.Int64())

	rotated := makeCertificate(t, "default.test", 1002)
	s.set(defaultCertPath, rotated.pem)
	rotation := s.next(api.ModeAuto)
	rotation.Ops = []api.Op{{Kind: api.OpCertSet, Path: defaultCertPath}}
	result := s.apply(rotation, s.allParts())
	require.True(t, result.OK, "cert_set was rejected: %+v", result.Error)
	assert.Equal(t, api.ResultRuntime, result.Mode)
	assert.Equal(t, worker, e.workerPID(), "rotating a certificate must not reload")
	assert.Equal(t, int64(1002), e.peerCertificate("default.test").SerialNumber.Int64())

	extra := makeCertificate(t, "extra.test", 2001)
	s.set(extraCertPath, extra.pem)
	s.set(crtListPath, defaultCertFile+"\n"+extraCertFile+" [ocsp-update on alpn h2,http/1.1] extra.test\n")
	introduced := s.next(api.ModeAuto)
	introduced.Ops = []api.Op{
		{Kind: api.OpCertNew, Path: extraCertPath},
		// The crt-list line token: HAProxy prepends crt-base to it, so it is
		// the bare filename, not the store name cert_new addressed.
		{
			Kind: api.OpCRTListAdd, Path: crtListPath, Cert: extraCertFile,
			Options: []api.KeywordArg{
				{Name: "ocsp-update", Args: []string{"on"}}, {Name: "alpn", Args: []string{"h2,http/1.1"}},
			},
			SNIFilters: []string{"extra.test"},
		},
	}
	result = s.apply(introduced, s.allParts())
	require.True(t, result.OK, "cert_new/crtlist_add were rejected: %+v", result.Error)
	assert.Equal(t, api.ResultRuntime, result.Mode)
	assert.Equal(t, worker, e.workerPID(), "serving a new SNI must not reload")
	assert.Equal(t, "extra.test", e.peerCertificate("extra.test").Subject.CommonName)

	s.set(crtListPath, defaultCertFile+"\n")
	s.remove(extraCertPath)
	withdrawn := s.next(api.ModeAuto)
	// Unlike add, del ssl crt-list does not join crt-base: it takes the store name.
	withdrawn.Ops = []api.Op{{Kind: api.OpCRTListDel, Path: crtListPath, Cert: extraCertPath}}
	result = s.apply(withdrawn, s.allParts())
	require.True(t, result.OK, "crtlist_del was rejected: %+v", result.Error)
	assert.Equal(t, api.ResultRuntime, result.Mode)
	assert.Equal(t, worker, e.workerPID(), "withdrawing an SNI must not reload")
	assert.Equal(t, "default.test", e.peerCertificate("extra.test").Subject.CommonName,
		"the withdrawn SNI must fall back to the default certificate")
	assert.False(t, e.exists(extraCertPath), "a file absent from the manifest must be deleted")
}

// TestCRTListLookupOrderAtRuntime pins the HAProxy behaviour deployplan's
// crt-list order rule rests on: a runtime add lands last in the SNI lookup and
// the default stays, so a mid-list add with its own SNI serves exactly what a
// reload of the file would; where two entries share an SNI, the one inserted
// first wins, and commit ssl cert re-inserts its entries last.
func TestCRTListLookupOrderAtRuntime(t *testing.T) {
	e, s := converged(t)
	worker := e.workerPID()

	certs := map[string]certificate{}
	for i, name := range []string{"a", "b", "c", "dup"} {
		certs[name] = makeCertificate(t, name+".test", int64(3001+i))
	}
	line := func(name, sni string) string { return name + ".pem " + sni + "\n" }
	runtimeApply := func(what string, ops ...api.Op) {
		t.Helper()
		m := s.next(api.ModeAuto)
		m.Ops = ops
		result := s.apply(m, s.allParts())
		require.True(t, result.OK, "%s was rejected: %+v", what, result.Error)
		require.Equal(t, api.ResultRuntime, result.Mode, what)
		require.Equal(t, worker, e.workerPID(), "%s must not reload", what)
	}
	introduce := func(name, sni string) []api.Op {
		s.set("ssl/"+name+".pem", certs[name].pem)
		return []api.Op{
			{Kind: api.OpCertNew, Path: "ssl/" + name + ".pem"},
			{Kind: api.OpCRTListAdd, Path: crtListPath, Cert: name + ".pem", SNIFilters: []string{sni}},
		}
	}
	served := func(sni string) string { return e.peerCertificate(sni).Subject.CommonName }

	s.set(crtListPath, defaultCertFile+"\n"+line("a", "a.test")+line("c", "c.test"))
	runtimeApply("appending a and c", append(introduce("a", "a.test"), introduce("c", "c.test")...)...)

	s.set(crtListPath, defaultCertFile+"\n"+line("a", "a.test")+line("b", "b.test")+line("c", "c.test"))
	runtimeApply("adding b mid-list", introduce("b", "b.test")...)
	for sni, want := range map[string]string{
		"b.test": "b.test", "a.test": "a.test", "c.test": "c.test",
		"unknown.test": "default.test", "": "default.test",
	} {
		assert.Equal(t, want, served(sni), "SNI %q after the mid-list add", sni)
	}

	s.set(crtListPath, defaultCertFile+"\n"+line("a", "a.test")+line("b", "b.test")+line("c", "c.test")+line("dup", "c.test"))
	runtimeApply("adding a second entry for c.test", introduce("dup", "c.test")...)
	assert.Equal(t, "c.test", served("c.test"), "the entry inserted first wins a shared SNI")

	rotated := makeCertificate(t, "c.test", 3999)
	s.set("ssl/c.pem", rotated.pem)
	runtimeApply("rotating c", api.Op{Kind: api.OpCertSet, Path: "ssl/c.pem"})
	assert.Equal(t, "dup.test", served("c.test"), "commit ssl cert re-inserts c behind dup")

	result := s.apply(s.next(api.ModeReload), s.allParts())
	require.True(t, result.OK, "reload was rejected: %+v", result.Error)
	e.waitForReady(http.StatusOK)
	assert.NotEqual(t, worker, e.workerPID())
	assert.Equal(t, int64(3999), e.peerCertificate("c.test").SerialNumber.Int64(), "a reload restores file order")
}
