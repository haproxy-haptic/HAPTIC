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
