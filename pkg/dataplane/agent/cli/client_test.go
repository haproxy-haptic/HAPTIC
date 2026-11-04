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

package cli_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/cli"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/haproxytest"
)

func newClient(t *testing.T) (*cli.Client, *haproxytest.HAProxy) {
	t.Helper()
	model := haproxytest.Start(t)
	client, err := cli.New(t.Context(), cli.Config{
		WorkerSocket: model.WorkerSocket(),
		MasterSocket: model.MasterSocket(),
		Logger:       slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	return client, model
}

func content(files map[string]string) cli.Content {
	return func(path string) ([]byte, error) {
		body, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("no content for %s", path)
		}
		return []byte(body), nil
	}
}

func compileAll(t *testing.T, ops []api.Op, files map[string]string) []cli.Program {
	t.Helper()
	programs := make([]cli.Program, 0, len(ops))
	for i := range ops {
		program, err := cli.Compile(&ops[i], content(files))
		require.NoError(t, err)
		programs = append(programs, program)
	}
	return programs
}

func TestExecuteRunsTheOrderedCreateSequence(t *testing.T) {
	client, model := newClient(t)
	ops := []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-a", Profile: "prof", Mode: "http", GUID: "g1"},
		{Kind: api.OpServerAdd, Backend: "be-a", Server: "srv1", Address: "10.0.0.1", Port: 8080,
			Keywords: []api.KeywordArg{{Name: "check"}}},
		{Kind: api.OpServerEnable, Backend: "be-a", Server: "srv1", Health: true},
		{Kind: api.OpBackendPublish, Backend: "be-a"},
		{Kind: api.OpMapAdd, Path: "maps/host.map", Key: "example.com", Value: "be-a"},
	}

	results, err := client.Execute(compileAll(t, ops, nil))
	require.NoError(t, err)
	require.Len(t, results, len(ops))
	for _, r := range results {
		assert.True(t, r.OK, "%s: %s", r.Kind, r.Output)
	}

	model.With(func(m *haproxytest.Model) {
		backend := m.Backends["be-a"]
		require.NotNil(t, backend)
		assert.Equal(t, "prof", backend.Profile)
		assert.Equal(t, "http", backend.Mode)
		assert.Equal(t, "g1", backend.GUID)
		assert.True(t, backend.Published)
		require.Len(t, backend.Servers, 1)
		assert.True(t, backend.Servers[0].Enabled)
		assert.True(t, backend.Servers[0].Health)
	})
	assert.Equal(t, []haproxytest.MapEntry{{Key: "example.com", Value: "be-a"}}, model.MapEntries("maps/host.map"))
}

func TestExecuteReportsANameCollision(t *testing.T) {
	client, model := newClient(t)
	add := []api.Op{{Kind: api.OpBackendAdd, Backend: "be-a", Profile: "prof", Mode: "http"}}
	_, err := client.Execute(compileAll(t, add, nil))
	require.NoError(t, err)

	results, err := client.Execute(compileAll(t, add, nil))
	require.ErrorIs(t, err, cli.ErrNameCollision)
	require.Len(t, results, 1)
	assert.False(t, results[0].OK)
	assert.Contains(t, results[0].Output, "already used by other proxy")
	model.With(func(m *haproxytest.Model) { assert.Len(t, m.Backends, 1) })
}

func TestExecuteStopsAtTheFirstRejection(t *testing.T) {
	client, model := newClient(t)
	ops := []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-a", Profile: "prof", Mode: "http"},
		{Kind: api.OpServerAdd, Backend: "missing", Server: "srv1", Address: "10.0.0.1"},
		{Kind: api.OpBackendPublish, Backend: "be-a"},
	}
	results, err := client.Execute(compileAll(t, ops, nil))
	require.ErrorIs(t, err, cli.ErrRejected)
	assert.True(t, results[0].OK)
	assert.False(t, results[1].OK)
	assert.False(t, results[2].OK, "an op after the failure has an unknown outcome")
	assert.True(t, model.HasBackend("be-a"), "the op before the failure did run")
}

func TestExecuteBatchesCommandsWithinTheLineLimit(t *testing.T) {
	client, model := newClient(t)
	setup := []api.Op{{Kind: api.OpBackendAdd, Backend: "be-a", Profile: "prof", Mode: "http"}}
	_, err := client.Execute(compileAll(t, setup, nil))
	require.NoError(t, err)

	var ops []api.Op
	for i := 0; i < 400; i++ {
		ops = append(ops, api.Op{
			Kind: api.OpServerAdd, Backend: "be-a", Server: fmt.Sprintf("srv%03d", i),
			Address: "10.0.0.1", Port: 8080,
		})
	}
	_, err = client.Execute(compileAll(t, ops, nil))
	require.NoError(t, err)
	assert.Len(t, model.ServerNames("be-a"), 400)
}

func TestExecuteSwitchesAMapAtomically(t *testing.T) {
	client, model := newClient(t)
	seed := []api.Op{{Kind: api.OpMapAdd, Path: "m", Key: "old", Value: "1"}}
	_, err := client.Execute(compileAll(t, seed, nil))
	require.NoError(t, err)

	var body strings.Builder
	for i := 0; i < 1500; i++ {
		fmt.Fprintf(&body, "key-%04d value-%04d\n", i, i)
	}
	replace := []api.Op{{Kind: api.OpMapReplace, Path: "m"}}
	_, err = client.Execute(compileAll(t, replace, map[string]string{"m": body.String()}))
	require.NoError(t, err)

	entries := model.MapEntries("m")
	require.Len(t, entries, 1500)
	assert.Equal(t, haproxytest.MapEntry{Key: "key-0000", Value: "value-0000"}, entries[0])
	assert.Equal(t, haproxytest.MapEntry{Key: "key-1499", Value: "value-1499"}, entries[1499])
}

// A blank line inside a payload ends HAProxy's default block; the agent frames
// its payloads with a pattern instead, so a certificate keeps every byte.
func TestACertificateWithABlankLineArrivesWhole(t *testing.T) {
	client, model := newClient(t)
	pem := "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n\n" +
		"-----BEGIN PRIVATE KEY-----\nBBB\n-----END PRIVATE KEY-----\n"
	ops := []api.Op{{Kind: api.OpCertNew, Path: "ssl/a.pem"}}

	_, err := client.Execute(compileAll(t, ops, map[string]string{"ssl/a.pem": pem}))
	require.NoError(t, err)

	var stored string
	model.With(func(m *haproxytest.Model) { stored = m.Certs["ssl/a.pem"] })
	assert.Equal(t, pem, stored)
}

// The CA listing carries a certificate count per row and HAProxy's built-in
// store, which is not a file any op can name.
func TestInventoryReadsTheCAListing(t *testing.T) {
	client, model := newClient(t)
	model.With(func(m *haproxytest.Model) { m.CAFiles["ssl/ca.crt"] = "" })

	worker, err := client.Info()
	require.NoError(t, err)
	inventory, err := client.Inventory(worker, 3)
	require.NoError(t, err)
	assert.Equal(t, []string{"ssl/ca.crt"}, inventory.CAFiles)
	assert.Equal(t, uint64(3), inventory.Generation)
}

func TestInventoryCannotSpanWorkers(t *testing.T) {
	client, model := newClient(t)
	model.With(func(m *haproxytest.Model) {
		m.Maps["maps/current.map"] = nil
		m.Reject = func(command string) (string, bool) {
			if command == "show map" {
				m.Pid++
				m.Reject = nil
			}
			return "", false
		}
	})
	worker, err := client.Info()
	require.NoError(t, err)
	_, err = client.Inventory(worker, 0)
	require.Error(t, err)
}

func TestInventoryRejectsListingRefusals(t *testing.T) {
	for _, command := range []string{"show map", "show ssl cert", "show ssl ca-file", "show ssl crl-file", "show ssl crt-list"} {
		t.Run(command, func(t *testing.T) {
			client, model := newClient(t)
			model.With(func(m *haproxytest.Model) {
				m.Reject = func(got string) (string, bool) {
					return "inventory unavailable", got == command
				}
			})
			worker, err := client.Info()
			require.NoError(t, err)
			_, err = client.Inventory(worker, 0)
			require.ErrorIs(t, err, cli.ErrRejected)
		})
	}
}

func TestInventoryKeepsAFullCertificateListing(t *testing.T) {
	client, model := newClient(t)
	model.With(func(m *haproxytest.Model) {
		for i := range api.MaxInventoryEntries {
			m.Certs[fmt.Sprintf("ssl/%04d-%s.pem", i, strings.Repeat("x", 128))] = ""
		}
	})
	worker, err := client.Info()
	require.NoError(t, err)
	inventory, err := client.Inventory(worker, 0)
	require.NoError(t, err)
	assert.Len(t, inventory.Certs, api.MaxInventoryEntries)
}

func TestExecuteRepeatsMapDelUntilTheKeyIsGone(t *testing.T) {
	client, model := newClient(t)
	seed := []api.Op{
		{Kind: api.OpMapAdd, Path: "m", Key: "dup", Value: "1"},
		{Kind: api.OpMapAdd, Path: "m", Key: "dup", Value: "2"},
		{Kind: api.OpMapAdd, Path: "m", Key: "keep", Value: "3"},
	}
	_, err := client.Execute(compileAll(t, seed, nil))
	require.NoError(t, err)

	_, err = client.Execute(compileAll(t, []api.Op{{Kind: api.OpMapDel, Path: "m", Key: "dup"}}, nil))
	require.NoError(t, err)
	assert.Equal(t, []haproxytest.MapEntry{{Key: "keep", Value: "3"}}, model.MapEntries("m"))
}

func TestExecuteAbortsACertTransactionOnFailure(t *testing.T) {
	client, model := newClient(t)
	files := map[string]string{"certs/tls.crt": "-----BEGIN CERTIFICATE-----\nno key here\n"}

	_, err := client.Execute(compileAll(t, []api.Op{{Kind: api.OpCertSet, Path: "certs/tls.crt"}}, files))
	require.ErrorIs(t, err, cli.ErrRejected)

	var aborted bool
	for _, c := range model.Sent() {
		if c == "abort ssl cert certs/tls.crt" {
			aborted = true
		}
	}
	assert.True(t, aborted, "a failed commit must abort the transaction: %v", model.Sent())
	model.With(func(m *haproxytest.Model) { assert.Empty(t, m.Certs["certs/tls.crt"]) })
}

func TestExecuteCommitsAValidCertificate(t *testing.T) {
	client, model := newClient(t)
	pem := "-----BEGIN PRIVATE KEY-----\nx\n-----BEGIN CERTIFICATE-----\ny\n"
	files := map[string]string{"certs/tls.crt": pem}

	_, err := client.Execute(compileAll(t, []api.Op{{Kind: api.OpCertNew, Path: "certs/tls.crt"}}, files))
	require.NoError(t, err)
	model.With(func(m *haproxytest.Model) { assert.Equal(t, pem, m.Certs["certs/tls.crt"]) })
}

func TestInfoReadsTheWorkerIdentity(t *testing.T) {
	client, model := newClient(t)
	info, err := client.Info()
	require.NoError(t, err)
	model.With(func(m *haproxytest.Model) {
		assert.Equal(t, m.Pid, info.WorkerPID)
		assert.Equal(t, m.StartTimeUnixMicros, info.WorkerStartTimeUnixMicros)
	})
	assert.Contains(t, model.Sent(), "show info float")
	assert.Equal(t, "3.4.3-1deb11u1", info.Version)
}

func TestReloadReportsFailureWithHAProxysWords(t *testing.T) {
	client, model := newClient(t)
	require.NoError(t, waitForWorker(client))

	model.With(func(m *haproxytest.Model) {
		m.ReloadFails = true
		m.ReloadLog = "[ALERT] config : parsing [haproxy.cfg:12] : unknown keyword 'nonsense'."
	})
	logs, err := client.Reload()
	require.Error(t, err)
	assert.Contains(t, logs, "unknown keyword")

	var before int
	model.With(func(m *haproxytest.Model) {
		m.ReloadFails = false
		m.ReloadLog = "Loading success."
		before = m.Pid
	})
	logs, err = client.Reload()
	require.NoError(t, err)
	assert.Contains(t, logs, "Loading success")
	model.With(func(m *haproxytest.Model) { assert.Equal(t, before+1, m.Pid) })
}

func TestShowProcAnswersOnTheMasterSocket(t *testing.T) {
	client, _ := newClient(t)
	out, err := client.ShowProc()
	require.NoError(t, err)
	assert.Contains(t, out, "worker")
}

func TestInventoryListsWhatTheWorkerLoaded(t *testing.T) {
	client, _ := newClient(t)
	files := map[string]string{
		"certs/tls.crt": "-----BEGIN PRIVATE KEY-----\n",
		"certs/ca.crt":  "-----BEGIN CERTIFICATE-----\n",
	}
	ops := []api.Op{
		{Kind: api.OpMapAdd, Path: "maps/host.map", Key: "a", Value: "b"},
		{Kind: api.OpCertNew, Path: "certs/tls.crt"},
		{Kind: api.OpCANew, Path: "certs/ca.crt"},
		{Kind: api.OpCRTListAdd, Path: "certs/list.txt", Cert: "certs/tls.crt"},
	}
	_, err := client.Execute(compileAll(t, ops, files))
	require.NoError(t, err)

	worker, err := client.Info()
	require.NoError(t, err)
	inventory, err := client.Inventory(worker, 7)
	require.NoError(t, err)
	assert.Equal(t, uint64(7), inventory.Generation)
	assert.Equal(t, []string{"maps/host.map"}, inventory.Maps)
	assert.Equal(t, []string{"certs/tls.crt"}, inventory.Certs)
	// The CA listing suffixes every row with a certificate count and always
	// lists the built-in trust store, which is not a file and stays out.
	assert.Equal(t, []string{"certs/ca.crt"}, inventory.CAFiles)
	assert.Equal(t, []string{"certs/list.txt"}, inventory.CRTLists)
}

func TestReadBackSeesWhatTheOpsWrote(t *testing.T) {
	client, _ := newClient(t)
	ops := []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-a", Profile: "prof", Mode: "http"},
		{Kind: api.OpServerAdd, Backend: "be-a", Server: "srv1", Address: "10.0.0.1", Port: 80},
		{Kind: api.OpMapAdd, Path: "m", Key: "dup", Value: "one"},
		{Kind: api.OpMapAdd, Path: "m", Key: "dup", Value: "two"},
	}
	_, err := client.Execute(compileAll(t, ops, nil))
	require.NoError(t, err)

	entries, err := client.MapEntries("m")
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"dup": {"one", "two"}}, entries)

	names, err := client.ServerNames("be-a")
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"srv1": {}}, names)
}

func TestSplitSeparatesTheDeleteTail(t *testing.T) {
	ops := []api.Op{
		{Kind: api.OpBackendUnpublish, Backend: "be-a"},
		{Kind: api.OpServerDisable, Backend: "be-a", Server: "srv1"},
		{Kind: api.OpServerWaitRemovable, Backend: "be-a", Server: "srv1", TimeoutMs: 2000},
		{Kind: api.OpShutdownSessions, Backend: "be-a", Server: "srv1"},
		{Kind: api.OpServerDel, Backend: "be-a", Server: "srv1"},
		{Kind: api.OpBackendWaitRemovable, Backend: "be-a", TimeoutMs: 2000},
		{Kind: api.OpBackendDel, Backend: "be-a"},
	}
	inline, servers, backends := cli.Split(ops)

	kinds := make([]string, 0, len(inline))
	for i := range inline {
		kinds = append(kinds, inline[i].Kind)
	}
	assert.Equal(t, []string{api.OpBackendUnpublish, api.OpServerDisable}, kinds)
	assert.Equal(t, []cli.ServerRef{{Backend: "be-a", Server: "srv1"}}, servers)
	assert.Equal(t, []string{"be-a"}, backends)
}

func testWorkerInfo() api.HAProxyInfo {
	return api.HAProxyInfo{WorkerPID: 1000, WorkerStartTimeUnixMicros: 1_700_000_000_000_000}
}

func TestDeferralsRemoveServersAndBackendsOffTheApplyPath(t *testing.T) {
	client, model := newClient(t)
	setup := []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-a", Profile: "prof", Mode: "http"},
		{Kind: api.OpServerAdd, Backend: "be-a", Server: "srv1", Address: "10.0.0.1", Port: 80},
	}
	_, err := client.Execute(compileAll(t, setup, nil))
	require.NoError(t, err)

	deferrals := cli.NewDeferrals(client, slog.New(slog.DiscardHandler), nil)
	deferrals.SetWorker(testWorkerInfo())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = deferrals.Start(ctx) }()

	require.NoError(t, deferrals.Enqueue(testWorkerInfo(), []cli.ServerRef{{Backend: "be-a", Server: "srv1"}}, []string{"be-a"}))
	deferrals.Wake()
	if !assert.Eventually(t, func() bool {
		return !model.HasBackend("be-a")
	}, 10*time.Second, 20*time.Millisecond) {
		t.Fatalf("sent: %v", model.Sent())
	}
	assert.Empty(t, deferrals.Pending().Servers)
}

func TestDeferralsShutDownSessionsWhenTheWaitExpires(t *testing.T) {
	client, model := newClient(t)
	setup := []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-a", Profile: "prof", Mode: "http"},
		{Kind: api.OpServerAdd, Backend: "be-a", Server: "srv1", Address: "10.0.0.1", Port: 80},
	}
	_, err := client.Execute(compileAll(t, setup, nil))
	require.NoError(t, err)
	model.With(func(m *haproxytest.Model) { m.BlockedServers["be-a/srv1"] = true })

	deferrals := cli.NewDeferrals(client, slog.New(slog.DiscardHandler), nil)
	deferrals.SetWorker(testWorkerInfo())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = deferrals.Start(ctx) }()

	require.NoError(t, deferrals.Enqueue(testWorkerInfo(), []cli.ServerRef{{Backend: "be-a", Server: "srv1"}}, nil))
	deferrals.Wake()
	if !assert.Eventually(t, func() bool {
		return len(model.ServerNames("be-a")) == 0
	}, 10*time.Second, 20*time.Millisecond) {
		t.Fatalf("sent: %v", model.Sent())
	}

	var shutdown bool
	for _, c := range model.Sent() {
		if c == "shutdown sessions server be-a/srv1" {
			shutdown = true
		}
	}
	assert.True(t, shutdown)
}

func TestDeferralsRefuseMoreThanTheCap(t *testing.T) {
	client, _ := newClient(t)
	deferrals := cli.NewDeferrals(client, slog.New(slog.DiscardHandler), nil)

	servers := make([]cli.ServerRef, 0, api.MaxPendingServerDeletes+1)
	for i := range api.MaxPendingServerDeletes + 1 {
		servers = append(servers, cli.ServerRef{Backend: "be-a", Server: fmt.Sprintf("srv%d", i)})
	}
	assert.ErrorIs(t, deferrals.Enqueue(testWorkerInfo(), servers, nil), cli.ErrDeferralOverflow)

	backends := make([]string, 0, api.MaxPendingBackendDeletes+1)
	for i := range api.MaxPendingBackendDeletes + 1 {
		backends = append(backends, fmt.Sprintf("be-%d", i))
	}
	assert.ErrorIs(t, deferrals.Enqueue(testWorkerInfo(), nil, backends), cli.ErrDeferralOverflow)
}

// A name the agent would have to quote never reaches the queue: the deferred
// delete builds its command line itself, so a ';' would run a second command.
func TestDeferralsRefuseAnUnsafeName(t *testing.T) {
	client, _ := newClient(t)
	deferrals := cli.NewDeferrals(client, slog.New(slog.DiscardHandler), nil)

	injected := []cli.ServerRef{{Backend: "be-a", Server: "srv1;shutdown sessions server be-a/srv2"}}
	assert.ErrorIs(t, deferrals.Enqueue(testWorkerInfo(), injected, nil), cli.ErrUnsafeToken)
	assert.ErrorIs(t, deferrals.Enqueue(testWorkerInfo(), nil, []string{"be-a;del backend be-b"}), cli.ErrUnsafeToken)
	assert.Empty(t, deferrals.Pending().Servers)
	assert.Empty(t, deferrals.Pending().Backends)
}

// A delete the drain is inside is still outstanding: it has to count against
// the per-pod caps and show up in /v1/state, or the caps never fire and the
// runbook's diagnostic reports nothing while thousands are queued.
func TestADeleteInFlightStaysOutstanding(t *testing.T) {
	client, model := newClient(t)
	setup := []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-a", Profile: "prof", Mode: "http"},
		{Kind: api.OpServerAdd, Backend: "be-a", Server: "srv1", Address: "10.0.0.1", Port: 80},
	}
	_, err := client.Execute(compileAll(t, setup, nil))
	require.NoError(t, err)

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	model.With(func(m *haproxytest.Model) {
		m.Reject = func(command string) (string, bool) {
			if strings.HasPrefix(command, "wait ") {
				once.Do(func() { close(entered) })
				<-release
			}
			return "", false
		}
	})
	deferrals := cli.NewDeferrals(client, slog.New(slog.DiscardHandler), nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = deferrals.Start(ctx) }()

	deferrals.SetWorker(testWorkerInfo())
	require.NoError(t, deferrals.Enqueue(testWorkerInfo(), []cli.ServerRef{{Backend: "be-a", Server: "srv1"}}, nil))
	deferrals.Wake()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain never reached the wait")
	}

	assert.Equal(t, []string{"be-a/srv1"}, deferrals.Pending().Servers)
	full := make([]cli.ServerRef, 0, api.MaxPendingServerDeletes)
	for i := range api.MaxPendingServerDeletes {
		full = append(full, cli.ServerRef{Backend: "be-a", Server: fmt.Sprintf("s%d", i)})
	}
	assert.ErrorIs(t, deferrals.Enqueue(testWorkerInfo(), full, nil), cli.ErrDeferralOverflow,
		"the cap has to see the delete that is running")
	close(release)
}

// waitForWorker primes client-native's process-wide version cache, which its
// Reload() gate consults.
func waitForWorker(client *cli.Client) error {
	_, err := client.Info()
	return err
}

// TestExecuteReadsSetServerAddrByItsWords: HAProxy answers `set server addr`
// at WARNING severity whether it changed something, had nothing to change or
// refused, so the verdict comes from the words, never the level.
func TestExecuteReadsSetServerAddrByItsWords(t *testing.T) {
	client, model := newClient(t)
	setup := []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-a", Profile: "prof", Mode: "http"},
		{Kind: api.OpServerAdd, Backend: "be-a", Server: "srv1", Address: "10.0.0.1", Port: 8080},
	}
	_, err := client.Execute(compileAll(t, setup, nil))
	require.NoError(t, err)

	moved := []api.Op{{Kind: api.OpServerSetAddr, Backend: "be-a", Server: "srv1", Address: "10.0.0.2", Port: 9090}}
	results, err := client.Execute(compileAll(t, moved, nil))
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].OK, "%s", results[0].Output)
	model.With(func(m *haproxytest.Model) {
		assert.Equal(t, "10.0.0.2:9090", m.Backends["be-a"].Servers[0].Address)
	})

	results, err = client.Execute(compileAll(t, moved, nil))
	require.NoError(t, err)
	assert.True(t, results[0].OK, "an address the server already has is not a refusal: %s", results[0].Output)

	bad := []api.Op{{Kind: api.OpServerSetAddr, Backend: "be-a", Server: "srv1", Address: "not-an-ip"}}
	results, err = client.Execute(compileAll(t, bad, nil))
	require.ErrorIs(t, err, cli.ErrRejected)
	assert.False(t, results[0].OK)
	assert.Contains(t, results[0].Output, "Invalid addr")
}
