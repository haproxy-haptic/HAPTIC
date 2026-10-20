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

// Package haproxytest serves an in-memory model of the HAProxy runtime over a
// worker stats socket and a master socket. It is the oracle the agent's unit
// and fault-simulation tests compare against; the real wire framing is pinned
// by the docker test, not here.
package haproxytest

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Model is everything the fake knows. A test reaches it only through
// HAProxy.With, which holds the lock the socket goroutines take.
type Model struct {
	Version             string
	Pid                 int
	StartTimeUnixMicros int64
	Backends            map[string]*Backend
	Maps                map[string][]MapEntry
	Certs               map[string]string
	CAFiles             map[string]string
	CRLFiles            map[string]string
	CRTLists            map[string][]string

	// ProbeConnections and TrafficConnections are the conn_tot counters of
	// the `status` and `http-tcp` frontends that `show stat` reports.
	ProbeConnections   uint64
	TrafficConnections uint64
	// ReloadFails makes the master `reload` answer Success=0.
	ReloadFails bool
	// ReloadLog is the startup log a reload returns.
	ReloadLog string
	// Commands records every command the agent sent, in order.
	Commands []string
	// BlockedServers makes `wait … srv-removable` expire for a server, the way
	// an in-flight request does.
	BlockedServers map[string]bool
	// Reject answers the matching command with an error instead of running it.
	Reject func(command string) (message string, rejected bool)
	// OnReload installs the state a newly started worker inherits from its
	// config file. Runtime-only objects are gone by the time it runs.
	OnReload func(m *Model)

	pendingCert map[string]string
	pendingCA   map[string]string
	pendingCRL  map[string]string
	mapVersions map[string][]MapEntry
	preparedFor map[string]string
	nextMapVer  int
}

// HAProxy is the model plus its two sockets.
type HAProxy struct {
	mu sync.Mutex
	m  Model

	workerPath     string
	masterPath     string
	masterListener net.Listener
}

// Backend is one proxy of the model.
type Backend struct {
	Profile   string
	Mode      string
	GUID      string
	Published bool
	Servers   []*Server
}

// Server is one server of a backend.
type Server struct {
	Name    string
	Address string
	Weight  int
	Maxconn int
	State   string
	Enabled bool
	Health  bool
}

// MapEntry is one key/value pair of a map file, duplicates included.
type MapEntry struct {
	Key   string
	Value string
}

// Start serves the model on two unix sockets in a short-named temp directory:
// tb.TempDir() embeds the test name and a unix socket path is capped at 108
// bytes, so a long subtest name would fail bind(2).
func Start(tb testing.TB) *HAProxy {
	tb.Helper()
	dir, err := os.MkdirTemp("", "hp")
	if err != nil {
		tb.Fatalf("socket dir: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	h := &HAProxy{
		m: Model{
			Version:             "3.4.3-1deb11u1",
			Pid:                 1000,
			StartTimeUnixMicros: 1_700_000_000_000_000,
			Backends:            map[string]*Backend{},
			Maps:                map[string][]MapEntry{},
			Certs:               map[string]string{},
			CAFiles:             map[string]string{},
			CRLFiles:            map[string]string{},
			CRTLists:            map[string][]string{},
			BlockedServers:      map[string]bool{},
			ReloadLog:           "Loading success.",
			pendingCert:         map[string]string{},
			pendingCA:           map[string]string{},
			pendingCRL:          map[string]string{},
			mapVersions:         map[string][]MapEntry{},
			preparedFor:         map[string]string{},
		},
		workerPath: filepath.Join(dir, "haproxy-worker.sock"),
		masterPath: filepath.Join(dir, "haproxy-master.sock"),
	}
	h.serve(tb, h.workerPath)
	h.masterListener = h.serve(tb, h.masterPath)
	return h
}

// StopMaster closes the master socket, which is what the agent sees while the
// HAProxy container is restarting: reload and show proc fail at the transport,
// with no verdict on any configuration.
func (h *HAProxy) StopMaster() {
	_ = h.masterListener.Close()
	_ = os.Remove(h.masterPath)
}

// WorkerSocket is the stats socket every runtime command goes to.
func (h *HAProxy) WorkerSocket() string { return h.workerPath }

// MasterSocket carries reload and show proc.
func (h *HAProxy) MasterSocket() string { return h.masterPath }

// With runs fn against the model under the lock the socket goroutines hold.
func (h *HAProxy) With(fn func(m *Model)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(&h.m)
}

// Sent returns the commands the agent has issued so far.
func (h *HAProxy) Sent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.m.Commands...)
}

// MapEntries copies one map file's contents.
func (h *HAProxy) MapEntries(path string) []MapEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]MapEntry(nil), h.m.Maps[path]...)
}

// ServerNames lists the servers a backend holds, empty when it does not exist.
func (h *HAProxy) ServerNames(backend string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	be, exists := h.m.Backends[backend]
	if !exists {
		return nil
	}
	names := make([]string, 0, len(be.Servers))
	for _, s := range be.Servers {
		names = append(names, s.Name)
	}
	return names
}

// HasBackend reports whether the model holds a backend of that name.
func (h *HAProxy) HasBackend(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, exists := h.m.Backends[name]
	return exists
}

func (h *HAProxy) serve(tb testing.TB, path string) net.Listener {
	tb.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		tb.Fatalf("listen on %s: %v", path, err)
	}
	tb.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(path)
	})
	master := path == h.masterPath
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go h.handle(conn, master)
		}
	}()
	return listener
}

func (h *HAProxy) handle(conn net.Conn, master bool) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	h.mu.Lock()
	workerPID := h.m.Pid
	workerStart := h.m.StartTimeUnixMicros
	h.mu.Unlock()
	severity, prompt := false, false
	for {
		first, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line, payload := strings.TrimRight(first, "\n"), ""
		if head, pattern, framed := strings.Cut(line, " <<"); framed {
			line = head
			payload = readPayload(reader, strings.TrimSpace(pattern))
		}
		h.mu.Lock()
		current := h.m.Pid
		currentStart := h.m.StartTimeUnixMicros
		h.mu.Unlock()
		if !master && (current != workerPID || currentStart != workerStart) {
			return
		}
		var out strings.Builder
		for _, command := range strings.Split(line, ";") {
			command = strings.TrimSpace(command)
			switch command {
			case "prompt":
				prompt = !prompt
			case "set severity-output number":
				severity = true
				out.WriteString("\n")
			default:
				write(&out, h.dispatch(command, payload, master), severity)
			}
		}
		if prompt {
			out.WriteString("\n> ")
		}
		if _, err := conn.Write([]byte(out.String())); err != nil || !prompt {
			return
		}
	}
}

// readPayload reads a payload block, which ends at a line equal to the pattern
// the command named — an empty line when it named none, which is HAProxy's
// default and the reason a blank line inside content truncates it.
func readPayload(reader *bufio.Reader, pattern string) string {
	var b strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil || strings.TrimRight(line, "\n") == pattern {
			return b.String()
		}
		b.WriteString(line)
	}
}

// reply is one command's answer: a message carries a severity, a dump does not.
type reply struct {
	text    string
	dump    bool
	fatal   bool
	warning bool
}

func silent() reply { return reply{} }

func message(format string, a ...any) reply {
	return reply{text: fmt.Sprintf(format, a...)}
}

func failure(format string, a ...any) reply {
	return reply{text: fmt.Sprintf(format, a...), fatal: true}
}

// warning is the LOG_WARNING answer `set server addr` gives whether it changed
// something or refused; only the words tell.
func warning(format string, a ...any) reply {
	return reply{text: fmt.Sprintf(format, a...), warning: true}
}

func dump(text string) reply { return reply{text: text, dump: true} }

func write(out *strings.Builder, r reply, severity bool) {
	if r.text == "" {
		out.WriteString("\n")
		return
	}
	switch {
	case r.dump:
		out.WriteString(r.text)
	case severity && r.fatal:
		out.WriteString("[3]: " + r.text)
	case severity && r.warning:
		out.WriteString("[4]: " + r.text)
	case severity:
		out.WriteString("[6]: " + r.text)
	default:
		out.WriteString(r.text)
	}
	out.WriteString("\n\n")
}
