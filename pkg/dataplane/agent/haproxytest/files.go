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

package haproxytest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func (h *HAProxy) add(rest, payload string) reply {
	object, args := cut(rest)
	switch object {
	case objBackend:
		return h.addBackend(args)
	case objServer:
		return h.addServer(args)
	case objMap:
		return h.addMap(args, payload)
	case objSSL:
		return h.addSSL(args, payload)
	}
	return failure("Unknown command 'add %s'.", object)
}

func (h *HAProxy) del(rest, _ string) reply {
	object, args := cut(rest)
	switch object {
	case objBackend:
		return h.delBackend(args)
	case objServer:
		return h.delServer(args)
	case objMap:
		return h.delMap(args)
	case objSSL:
		return h.delSSL(args)
	}
	return failure("Unknown command 'del %s'.", object)
}

func (h *HAProxy) set(rest, payload string) reply {
	object, args := cut(rest)
	switch object {
	case objServer:
		return h.setServer(args)
	case "maxconn":
		return h.setMaxconn(args)
	case objMap:
		return h.setMap(args)
	case objSSL:
		return h.setSSL(args, payload)
	}
	return failure("Unknown command 'set %s'.", object)
}

func (h *HAProxy) create(rest, _ string) reply {
	object, args := cut(rest)
	if object != objSSL {
		return failure("Unknown command 'new %s'.", object)
	}
	kind, name := cut(args)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch kind {
	case objCert:
		h.m.Certs[name] = ""
		return message("New empty certificate store '%s'!", name)
	case objCAFile:
		h.m.CAFiles[name] = ""
		return message("New CA file created '%s'!", name)
	case objCRLFile:
		h.m.CRLFiles[name] = ""
		return message("New CRL file created '%s'!", name)
	}
	return failure("Unknown command 'new ssl %s'.", kind)
}

func (h *HAProxy) commit(rest, _ string) reply {
	object, args := cut(rest)
	if object == objMap {
		return h.commitMap(args)
	}
	if object != objSSL {
		return failure("Unknown command 'commit %s'.", object)
	}
	kind, name := cut(args)
	h.mu.Lock()
	defer h.mu.Unlock()
	pending, store := h.transaction(kind)
	if pending == nil {
		return failure("No ongoing transaction!")
	}
	content, staged := pending[name]
	if !staged {
		return failure("No ongoing transaction for '%s'!", name)
	}
	if kind == objCert && !strings.Contains(content, "PRIVATE KEY") {
		return failure("Missing private key for '%s'.", name)
	}
	store[name] = content
	delete(pending, name)
	return message("Committing %s\nSuccess!", name)
}

func (h *HAProxy) abort(rest, _ string) reply {
	object, args := cut(rest)
	if object != objSSL {
		return failure("Unknown command 'abort %s'.", object)
	}
	kind, name := cut(args)
	h.mu.Lock()
	defer h.mu.Unlock()
	pending, _ := h.transaction(kind)
	if pending == nil {
		return failure("Unknown command 'abort ssl %s'.", kind)
	}
	delete(pending, name)
	return message("Transaction aborted for certificate '%s'!", name)
}

func (h *HAProxy) transaction(kind string) (pending, store map[string]string) {
	switch kind {
	case objCert:
		return h.m.pendingCert, h.m.Certs
	case objCAFile:
		return h.m.pendingCA, h.m.CAFiles
	case objCRLFile:
		return h.m.pendingCRL, h.m.CRLFiles
	}
	return nil, nil
}

func (h *HAProxy) addSSL(rest, payload string) reply {
	kind, args := cut(rest)
	switch kind {
	case objCRTList:
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, line := range strings.Split(strings.TrimSpace(payload), "\n") {
			if line != "" {
				h.m.CRTLists[args] = append(h.m.CRTLists[args], line)
			}
		}
		return message("Success!")
	case objCAFile:
		return h.setSSL(objCAFile+" "+args, payload)
	}
	return failure("Unknown command 'add ssl %s'.", kind)
}

func (h *HAProxy) setSSL(rest, payload string) reply {
	kind, name := cut(rest)
	h.mu.Lock()
	defer h.mu.Unlock()
	pending, _ := h.transaction(kind)
	if pending == nil {
		return failure("Unknown command 'set ssl %s'.", kind)
	}
	pending[name] = payload
	return message("Transaction created for certificate %s!", name)
}

func (h *HAProxy) delSSL(rest string) reply {
	kind, args := cut(rest)
	if kind != objCRTList {
		return failure("Unknown command 'del ssl %s'.", kind)
	}
	list, cert := cut(args)
	h.mu.Lock()
	defer h.mu.Unlock()
	entries := h.m.CRTLists[list]
	kept := entries[:0]
	for _, e := range entries {
		if !strings.HasPrefix(e, cert) {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(entries) {
		return failure("No such certificate '%s' in crt-list.", cert)
	}
	h.m.CRTLists[list] = kept
	return silent()
}

func (h *HAProxy) addMap(rest, payload string) reply {
	entries := parseEntries(payload)
	h.mu.Lock()
	defer h.mu.Unlock()
	if version, _, versioned := strings.Cut(rest, " "); versioned {
		version = strings.TrimPrefix(version, "@")
		if _, prepared := h.m.mapVersions[version]; !prepared {
			return failure("Version '%s' does not exist.", version)
		}
		h.m.mapVersions[version] = append(h.m.mapVersions[version], entries...)
		return silent()
	}
	h.m.Maps[rest] = append(h.m.Maps[rest], entries...)
	return silent()
}

func (h *HAProxy) setMap(rest string) reply {
	path, args := cut(rest)
	key, value := cut(args)
	h.mu.Lock()
	defer h.mu.Unlock()
	found := false
	for i := range h.m.Maps[path] {
		if h.m.Maps[path][i].Key == key {
			h.m.Maps[path][i].Value = value
			found = true
		}
	}
	if !found {
		return failure("Key not found.")
	}
	return silent()
}

// delMap removes one duplicate per call, which is HAProxy 3.4's behaviour and
// the reason the agent repeats the command.
func (h *HAProxy) delMap(rest string) reply {
	path, key := cut(rest)
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, e := range h.m.Maps[path] {
		if e.Key == key {
			h.m.Maps[path] = append(h.m.Maps[path][:i], h.m.Maps[path][i+1:]...)
			return silent()
		}
	}
	return failure("Key not found.")
}

func (h *HAProxy) prepareMap(rest, _ string) reply {
	object, path := cut(rest)
	if object != objMap {
		return failure("Unknown command 'prepare %s'.", object)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m.nextMapVer++
	version := strconv.Itoa(h.m.nextMapVer)
	h.m.mapVersions[version] = nil
	h.m.preparedFor[version] = path
	return message("New version created: %s", version)
}

func (h *HAProxy) commitMap(rest string) reply {
	version, path := cut(rest)
	version = strings.TrimPrefix(version, "@")
	h.mu.Lock()
	defer h.mu.Unlock()
	entries, prepared := h.m.mapVersions[version]
	if !prepared || h.m.preparedFor[version] != path {
		return failure("Version '%s' does not exist.", version)
	}
	h.m.Maps[path] = entries
	delete(h.m.mapVersions, version)
	delete(h.m.preparedFor, version)
	// A committed version answers nothing, like the real runtime.
	return silent()
}

func (h *HAProxy) show(rest, _ string) reply {
	object, args := cut(rest)
	switch object {
	case "info":
		h.mu.Lock()
		defer h.mu.Unlock()
		return dump(fmt.Sprintf("Name: HAProxy\nVersion: %s\nPid: %d\nStart_time_sec: %d.%06d\nUptime: 0d 0h00m01s",
			h.m.Version, h.m.Pid, h.m.StartTimeUnixMicros/1_000_000, h.m.StartTimeUnixMicros%1_000_000))
	case objMap:
		return h.showMap(args)
	case objSSL:
		return h.showSSL(args)
	case "servers":
		return h.showServers(args)
	case "stat":
		return h.showStat()
	}
	return failure("Unknown command 'show %s'.", object)
}

// showStat answers the frontend rows the drain reads: a probe frontend and one
// traffic frontend whose accepted-connection counter tests advance.
func (h *HAProxy) showStat() reply {
	h.mu.Lock()
	defer h.mu.Unlock()
	return dump(fmt.Sprintf("# pxname,svname,conn_tot,\nstatus,FRONTEND,%d,\nhttp-tcp,FRONTEND,%d,\n",
		h.m.ProbeConnections, h.m.TrafficConnections))
}

func (h *HAProxy) showMap(path string) reply {
	h.mu.Lock()
	defer h.mu.Unlock()
	if path == "" {
		paths := sortedKeys(h.m.Maps)
		lines := make([]string, 0, len(paths)+1)
		lines = append(lines, "# id (file) description")
		for _, p := range paths {
			lines = append(lines, fmt.Sprintf("-1 (%s) pattern loaded from file '%s'", p, p))
		}
		return dump(strings.Join(lines, "\n"))
	}
	// Real shape: `<entry address> <key> <value>`, no header (3.0 and 3.4).
	lines := make([]string, 0, len(h.m.Maps[path]))
	for i, e := range h.m.Maps[path] {
		lines = append(lines, fmt.Sprintf("0x7f%010x %s %s", i, e.Key, e.Value))
	}
	return dump(strings.Join(lines, "\n"))
}

func (h *HAProxy) showSSL(rest string) reply {
	kind, name := cut(rest)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch kind {
	case objCert:
		return dump("# filename\n" + strings.Join(sortedKeys(h.m.Certs), "\n"))
	case objCAFile:
		// Real shape: every row carries a certificate count, and the built-in
		// store is listed alongside the files (verified on 3.0 and 3.4).
		rows := []string{"# filename"}
		for _, name := range sortedKeys(h.m.CAFiles) {
			rows = append(rows, fmt.Sprintf("%s - %d certificate(s)", name, 1))
		}
		return dump(strings.Join(append(rows, "@system-ca - 150 certificate(s)"), "\n"))
	case objCRLFile:
		return dump("# filename\n" + strings.Join(sortedKeys(h.m.CRLFiles), "\n"))
	case objCRTList:
		if entries, ok := h.m.CRTLists[strings.TrimPrefix(name, "-n ")]; ok && name != "" {
			return dump("# " + name + "\n" + strings.Join(entries, "\n"))
		}
		return dump("# filename\n" + strings.Join(sortedKeys(h.m.CRTLists), "\n"))
	}
	return failure("Unknown command 'show ssl %s'.", kind)
}

func (h *HAProxy) showServers(rest string) reply {
	kind, backend := cut(rest)
	if kind != "state" {
		return failure("Unknown command 'show servers %s'.", kind)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	be, exists := h.m.Backends[backend]
	if !exists {
		return failure("Can't find backend.")
	}
	lines := []string{"1", "# be_id be_name srv_id srv_name srv_addr srv_op_state"}
	for i, s := range be.Servers {
		lines = append(lines, fmt.Sprintf("3 %s %d %s %s %d", backend, i+1, s.Name, s.Address, boolToState(s.Enabled)))
	}
	return dump(strings.Join(lines, "\n"))
}

func boolToState(enabled bool) int {
	if enabled {
		return 2
	}
	return 0
}

// parseEntries reads a map payload the way the runtime does: every line is a
// record with no comment syntax, so a '#' header becomes a key and a blank
// line becomes an entry with none.
func parseEntries(payload string) []MapEntry {
	if payload == "" {
		return nil
	}
	var entries []MapEntry
	for _, line := range strings.Split(strings.TrimSuffix(payload, "\n"), "\n") {
		key, value, _ := strings.Cut(strings.TrimRight(line, "\r"), " ")
		entries = append(entries, MapEntry{Key: key, Value: value})
	}
	return entries
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
