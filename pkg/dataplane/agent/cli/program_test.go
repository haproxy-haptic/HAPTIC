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

package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

func fileContent(files map[string]string) Content {
	return func(path string) ([]byte, error) {
		content, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("no content for %s", path)
		}
		return []byte(content), nil
	}
}

func intPtr(v int) *int { return &v }

func TestCompileCoversEveryOpKind(t *testing.T) {
	declared := []string{
		api.OpBackendAdd, api.OpBackendPublish, api.OpBackendUnpublish, api.OpBackendDel,
		api.OpBackendWaitRemovable, api.OpServerAdd, api.OpServerEnable, api.OpServerDisable,
		api.OpServerSetAddr, api.OpServerSetWeight, api.OpServerSetMaxconn, api.OpServerSetState, api.OpServerWaitRemovable,
		api.OpShutdownSessions, api.OpServerDel, api.OpMapAdd, api.OpMapSet, api.OpMapDel,
		api.OpMapReplace, api.OpCertSet, api.OpCertNew, api.OpCASet, api.OpCANew,
		api.OpCRTListAdd, api.OpCRTListDel,
	}
	assert.ElementsMatch(t, declared, Kinds())
}

func TestCompileCommandTable(t *testing.T) {
	content := fileContent(map[string]string{
		"certs/tls.crt": "-----BEGIN PRIVATE KEY-----\n",
		"certs/ca.crt":  "-----BEGIN CERTIFICATE-----\n",
	})
	tests := []struct {
		name  string
		op    api.Op
		texts []string
	}{
		{
			name:  "backend add with guid",
			op:    api.Op{Kind: api.OpBackendAdd, Backend: "be-a", Profile: "prof", Mode: "http", GUID: "g1"},
			texts: []string{"add backend be-a from prof mode http guid g1"},
		},
		{
			name:  "backend add without guid",
			op:    api.Op{Kind: api.OpBackendAdd, Backend: "be-a", Profile: "prof", Mode: "tcp"},
			texts: []string{"add backend be-a from prof mode tcp"},
		},
		{
			name:  "backend publish",
			op:    api.Op{Kind: api.OpBackendPublish, Backend: "be-a"},
			texts: []string{"publish backend be-a"},
		},
		{
			name:  "backend unpublish",
			op:    api.Op{Kind: api.OpBackendUnpublish, Backend: "be-a"},
			texts: []string{"unpublish backend be-a"},
		},
		{
			name:  "backend del",
			op:    api.Op{Kind: api.OpBackendDel, Backend: "be-a"},
			texts: []string{"del backend be-a"},
		},
		{
			name:  "wait be-removable",
			op:    api.Op{Kind: api.OpBackendWaitRemovable, Backend: "be-a", TimeoutMs: 2000},
			texts: []string{"wait 2000 be-removable be-a"},
		},
		{
			name: "server add with keywords",
			op: api.Op{
				Kind: api.OpServerAdd, Backend: "be-a", Server: "srv1", Address: "10.0.0.1", Port: 8080,
				Keywords: []api.KeywordArg{{Name: "check"}, {Name: "inter", Args: []string{"2s"}}, {Name: "init-state", Args: []string{"up"}}},
			},
			texts: []string{"add server be-a/srv1 10.0.0.1:8080 check inter 2s init-state up"},
		},
		{
			name:  "server add without port",
			op:    api.Op{Kind: api.OpServerAdd, Backend: "be-a", Server: "srv1", Address: "/var/run/app.sock"},
			texts: []string{"add server be-a/srv1 /var/run/app.sock"},
		},
		{
			name:  "server enable with health",
			op:    api.Op{Kind: api.OpServerEnable, Backend: "be-a", Server: "srv1", Health: true},
			texts: []string{"enable health be-a/srv1", "enable server be-a/srv1"},
		},
		{
			name:  "server enable without health",
			op:    api.Op{Kind: api.OpServerEnable, Backend: "be-a", Server: "srv1"},
			texts: []string{"enable server be-a/srv1"},
		},
		{
			name:  "server disable",
			op:    api.Op{Kind: api.OpServerDisable, Backend: "be-a", Server: "srv1"},
			texts: []string{"disable server be-a/srv1"},
		},
		{
			name:  "server set addr with port",
			op:    api.Op{Kind: api.OpServerSetAddr, Backend: "be-a", Server: "srv1", Address: "10.0.0.2", Port: 9090},
			texts: []string{"set server be-a/srv1 addr 10.0.0.2 port 9090"},
		},
		{
			name:  "server set addr without port",
			op:    api.Op{Kind: api.OpServerSetAddr, Backend: "be-a", Server: "srv1", Address: "10.0.0.2"},
			texts: []string{"set server be-a/srv1 addr 10.0.0.2"},
		},
		{
			name:  "server set weight",
			op:    api.Op{Kind: api.OpServerSetWeight, Backend: "be-a", Server: "srv1", Weight: intPtr(0)},
			texts: []string{"set server be-a/srv1 weight 0"},
		},
		{
			name:  "server set maxconn",
			op:    api.Op{Kind: api.OpServerSetMaxconn, Backend: "be-a", Server: "srv1", Maxconn: intPtr(750)},
			texts: []string{"set maxconn server be-a/srv1 750"},
		},
		{
			name:  "server set state",
			op:    api.Op{Kind: api.OpServerSetState, Backend: "be-a", Server: "srv1", State: "drain"},
			texts: []string{"set server be-a/srv1 state drain"},
		},
		{
			name:  "wait srv-removable",
			op:    api.Op{Kind: api.OpServerWaitRemovable, Backend: "be-a", Server: "srv1", TimeoutMs: 2000},
			texts: []string{"wait 2000 srv-removable be-a/srv1"},
		},
		{
			name:  "shutdown sessions",
			op:    api.Op{Kind: api.OpShutdownSessions, Backend: "be-a", Server: "srv1"},
			texts: []string{"shutdown sessions server be-a/srv1"},
		},
		{
			name:  "server del",
			op:    api.Op{Kind: api.OpServerDel, Backend: "be-a", Server: "srv1"},
			texts: []string{"del server be-a/srv1"},
		},
		{
			name:  "map add",
			op:    api.Op{Kind: api.OpMapAdd, Path: "maps/host.map", Key: "example.com", Value: "be-a x"},
			texts: []string{"add map maps/host.map"},
		},
		{
			name:  "map set",
			op:    api.Op{Kind: api.OpMapSet, Path: "maps/host.map", Key: "example.com", Value: "be-b"},
			texts: []string{"set map maps/host.map example.com be-b"},
		},
		{
			name:  "map del",
			op:    api.Op{Kind: api.OpMapDel, Path: "maps/host.map", Key: "example.com"},
			texts: []string{"del map maps/host.map example.com"},
		},
		{
			name: "cert set",
			op:   api.Op{Kind: api.OpCertSet, Path: "certs/tls.crt"},
			texts: []string{
				"set ssl cert certs/tls.crt",
				"commit ssl cert certs/tls.crt",
			},
		},
		{
			name: "cert new",
			op:   api.Op{Kind: api.OpCertNew, Path: "certs/tls.crt"},
			texts: []string{
				"new ssl cert certs/tls.crt",
				"set ssl cert certs/tls.crt",
				"commit ssl cert certs/tls.crt",
			},
		},
		{
			name: "ca set",
			op:   api.Op{Kind: api.OpCASet, Path: "certs/ca.crt"},
			texts: []string{
				"set ssl ca-file certs/ca.crt",
				"commit ssl ca-file certs/ca.crt",
			},
		},
		{
			name: "ca new",
			op:   api.Op{Kind: api.OpCANew, Path: "certs/ca.crt"},
			texts: []string{
				"new ssl ca-file certs/ca.crt",
				"set ssl ca-file certs/ca.crt",
				"commit ssl ca-file certs/ca.crt",
			},
		},
		{
			name: "crt-list add",
			op: api.Op{
				Kind: api.OpCRTListAdd, Path: "certs/list.txt", Cert: "certs/tls.crt",
				Options: []api.KeywordArg{{Name: "alpn", Args: []string{"h2,http/1.1"}}}, SNIFilters: []string{"*.example.com"},
			},
			texts: []string{"add ssl crt-list certs/list.txt"},
		},
		{
			name:  "crt-list del",
			op:    api.Op{Kind: api.OpCRTListDel, Path: "certs/list.txt", Cert: "certs/tls.crt"},
			texts: []string{"del ssl crt-list certs/list.txt certs/tls.crt"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			program, err := Compile(&tc.op, content)
			require.NoError(t, err)
			var texts []string
			for _, c := range program.Commands {
				texts = append(texts, c.Text)
			}
			assert.Equal(t, tc.texts, texts)
		})
	}
}

func TestCompilePayloadForms(t *testing.T) {
	content := fileContent(map[string]string{"certs/tls.crt": "PEM\n"})

	mapAdd, err := Compile(&api.Op{Kind: api.OpMapAdd, Path: "m", Key: "k", Value: "a value with spaces"}, content)
	require.NoError(t, err)
	assert.Equal(t, "k a value with spaces\n", mapAdd.Commands[0].Payload)

	cert, err := Compile(&api.Op{Kind: api.OpCertSet, Path: "certs/tls.crt"}, content)
	require.NoError(t, err)
	assert.Equal(t, "PEM\n", cert.Commands[0].Payload)
	require.Len(t, cert.Abort, 1)
	assert.Equal(t, "abort ssl cert certs/tls.crt", cert.Abort[0].Text)

	list, err := Compile(&api.Op{
		Kind: api.OpCRTListAdd, Path: "l", Cert: "c",
		Options:    []api.KeywordArg{{Name: "alpn", Args: []string{"h2"}}},
		SNIFilters: []string{"*.example.com", "!secret.example.com"},
	}, content)
	require.NoError(t, err)
	assert.Equal(t, "c [alpn h2] *.example.com !secret.example.com\n", list.Commands[0].Payload)
}

func TestCompileRejectsUnsafeTokens(t *testing.T) {
	tests := []struct {
		name string
		op   api.Op
	}{
		{"command separator in a name", api.Op{Kind: api.OpBackendPublish, Backend: "be;show info"}},
		{"newline in a key", api.Op{Kind: api.OpMapDel, Path: "m", Key: "k\nshow info"}},
		{"payload introducer in a path", api.Op{Kind: api.OpMapDel, Path: "m <<", Key: "k"}},
		{"backslash in a server name", api.Op{Kind: api.OpServerDisable, Backend: "be", Server: `s\x`}},
		{"space in a map_set value", api.Op{Kind: api.OpMapSet, Path: "m", Key: "k", Value: "a b"}},
		{"newline in a map_add value", api.Op{Kind: api.OpMapAdd, Path: "m", Key: "k", Value: "a\nb"}},
		{"unknown mode", api.Op{Kind: api.OpBackendAdd, Backend: "be", Profile: "p", Mode: "htp"}},
		{"unknown state", api.Op{Kind: api.OpServerSetState, Backend: "be", Server: "s", State: "up"}},
		{"empty backend", api.Op{Kind: api.OpBackendPublish}},
		{"wait beyond the budget", api.Op{Kind: api.OpBackendWaitRemovable, Backend: "be", TimeoutMs: api.MaxWaitBudgetMs + 1}},
		{"weight unset", api.Op{Kind: api.OpServerSetWeight, Backend: "be", Server: "s"}},
		{"maxconn unset", api.Op{Kind: api.OpServerSetMaxconn, Backend: "be", Server: "s"}},
		{"negative maxconn", api.Op{Kind: api.OpServerSetMaxconn, Backend: "be", Server: "s", Maxconn: intPtr(-1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(&tc.op, fileContent(nil))
			assert.ErrorIs(t, err, ErrUnsafeToken)
		})
	}
}

func TestCompileRefusesUnknownOpKind(t *testing.T) {
	_, err := Compile(&api.Op{Kind: "backend_teleport"}, fileContent(nil))
	assert.ErrorIs(t, err, ErrUnknownOp)
}

func TestCompileRefusesAnOversizedCommand(t *testing.T) {
	_, err := Compile(&api.Op{
		Kind: api.OpMapSet, Path: "m", Key: strings.Repeat("k", api.MaxCommandLineBytes), Value: "v",
	}, fileContent(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line limit")
}

func TestMapReplaceChunksThePayload(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&body, "key-%04d value-%04d\n", i, i)
	}
	program, err := Compile(&api.Op{Kind: api.OpMapReplace, Path: "m"}, fileContent(map[string]string{"m": body.String()}))
	require.NoError(t, err)

	require.Greater(t, len(program.Commands), 3, "a 2000-entry map needs more than one chunk")
	assert.Equal(t, "prepare map m", program.Commands[0].Text)
	assert.True(t, program.Commands[0].Capture)
	var rebuilt strings.Builder
	for _, c := range program.Commands[1 : len(program.Commands)-1] {
		assert.Equal(t, "add map @"+VersionPlaceholder+" m", c.Text)
		assert.LessOrEqual(t, len(c.Payload), api.MaxPayloadBytes)
		rebuilt.WriteString(c.Payload)
	}
	assert.Equal(t, body.String(), rebuilt.String())
	assert.Equal(t, "commit map @"+VersionPlaceholder+" m", program.Commands[len(program.Commands)-1].Text)
}

// The payload parser has no comment syntax and no blank-line rule, so what
// travels is the entries the plan declares, never the file's own bytes.
func TestMapReplaceSendsTheEntriesNotTheFile(t *testing.T) {
	file := "# generated by haptic\n\nexample.com be-a\n\n# ingress/default\nb.example.com\tbe-b\n"
	program, err := Compile(&api.Op{Kind: api.OpMapReplace, Path: "m"}, fileContent(map[string]string{"m": file}))
	require.NoError(t, err)

	require.Len(t, program.Commands, 3)
	assert.Equal(t, "example.com be-a\nb.example.com be-b\n", program.Commands[1].Payload)
}

// A payload line equal to the terminator would end the block early, so the
// content is refused rather than edited.
func TestAPayloadThatEndsItselfIsRefused(t *testing.T) {
	pem := "-----BEGIN CERTIFICATE-----\n" + PayloadTerminator + "\n-----END CERTIFICATE-----\n"
	_, err := Compile(&api.Op{Kind: api.OpCertSet, Path: "ssl/a.pem"}, fileContent(map[string]string{"ssl/a.pem": pem}))
	assert.ErrorIs(t, err, ErrUnsafeToken)
}

func TestMapReplaceRefusesALineOverThePayloadLimit(t *testing.T) {
	huge := "k " + strings.Repeat("v", api.MaxPayloadBytes) + "\n"
	_, err := Compile(&api.Op{Kind: api.OpMapReplace, Path: "m"}, fileContent(map[string]string{"m": huge}))
	assert.ErrorIs(t, err, ErrUnsafeToken)
}
