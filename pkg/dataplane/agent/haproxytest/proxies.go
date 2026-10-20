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
	"net"
	"strconv"
	"strings"
)

func (h *HAProxy) addBackend(rest string) reply {
	fields := strings.Fields(rest)
	if len(fields) < 5 || fields[1] != "from" {
		return failure("'add backend' expects a name and a defaults section.")
	}
	name := fields[0]
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.m.Backends[name]; exists {
		return failure("backend '%s' name is already used by other proxy.", name)
	}
	be := &Backend{Profile: fields[2]}
	for i := 3; i+1 < len(fields); i += 2 {
		switch fields[i] {
		case "mode":
			be.Mode = fields[i+1]
		case "guid":
			be.GUID = fields[i+1]
		}
	}
	h.m.Backends[name] = be
	return message("New backend registered.")
}

func (h *HAProxy) addServer(rest string) reply {
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return failure("'add server' expects a backend/name and an address.")
	}
	backend, name, ok := strings.Cut(fields[0], "/")
	if !ok {
		return failure("'add server' expects <backend>/<server>.")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	be, exists := h.m.Backends[backend]
	if !exists {
		return failure("No such backend.")
	}
	if findServer(be, name) != nil {
		return failure("Already exists a server with the same name in backend.")
	}
	be.Servers = append(be.Servers, &Server{Name: name, Address: fields[1], State: "maint"})
	return message("New server registered.")
}

func (h *HAProxy) publish(rest, _ string) reply { return h.setPublished(rest, true) }

func (h *HAProxy) unpublish(rest, _ string) reply { return h.setPublished(rest, false) }

func (h *HAProxy) setPublished(rest string, published bool) reply {
	kind, name := cut(rest)
	if kind != objBackend {
		return failure("Unknown command.")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	be, exists := h.m.Backends[name]
	if !exists {
		return failure("No such backend.")
	}
	be.Published = published
	if published {
		return message("Backend published.")
	}
	return silent()
}

func (h *HAProxy) delBackend(name string) reply {
	h.mu.Lock()
	defer h.mu.Unlock()
	be, exists := h.m.Backends[name]
	if !exists {
		return failure("No such backend.")
	}
	if be.Published || len(be.Servers) > 0 {
		return failure("This backend cannot be removed at runtime.")
	}
	delete(h.m.Backends, name)
	return message("Backend deleted.")
}

func (h *HAProxy) delServer(ref string) reply {
	h.mu.Lock()
	defer h.mu.Unlock()
	be, srv, bad := h.locate(ref)
	if srv == nil {
		return bad
	}
	if srv.Enabled {
		return failure("Only servers in maintenance mode can be deleted.")
	}
	for i, s := range be.Servers {
		if s == srv {
			be.Servers = append(be.Servers[:i], be.Servers[i+1:]...)
			break
		}
	}
	return message("Server deleted.")
}

func (h *HAProxy) enable(rest, _ string) reply { return h.setServerFlag(rest, true) }

func (h *HAProxy) disable(rest, _ string) reply { return h.setServerFlag(rest, false) }

func (h *HAProxy) setServerFlag(rest string, on bool) reply {
	kind, ref := cut(rest)
	h.mu.Lock()
	defer h.mu.Unlock()
	_, srv, bad := h.locate(ref)
	if srv == nil {
		return bad
	}
	switch kind {
	case "health":
		srv.Health = on
	case objServer:
		srv.Enabled = on
		srv.State = map[bool]string{true: "ready", false: "maint"}[on]
	default:
		return failure("Unknown command.")
	}
	return silent()
}

func (h *HAProxy) shutdownSessions(rest, _ string) reply {
	scope, args := cut(rest)
	kind, ref := cut(args)
	if scope != "sessions" || kind != objServer {
		return failure("Unknown command.")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, srv, bad := h.locate(ref); srv == nil {
		return bad
	}
	h.m.BlockedServers[ref] = false
	return silent()
}

func (h *HAProxy) setServer(rest string) reply {
	ref, args := cut(rest)
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return failure("'set server' expects a property and a value.")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, srv, bad := h.locate(ref)
	if srv == nil {
		return bad
	}
	switch fields[0] {
	case "addr":
		return h.setServerAddr(srv, fields[1:])
	case "weight":
		srv.Weight, _ = strconv.Atoi(fields[1])
	case "state":
		srv.State = fields[1]
	default:
		return failure("'%s' is not a supported server property.", fields[0])
	}
	return silent()
}

// setMaxconn models `set maxconn server`, which answers nothing on success.
func (h *HAProxy) setMaxconn(rest string) reply {
	fields := strings.Fields(rest)
	if len(fields) != 3 || fields[0] != objServer {
		return failure("Require 'frontend', 'global' or 'server'.")
	}
	maxconn, err := strconv.Atoi(fields[2])
	if err != nil {
		return failure("maxconn string empty or preceded by garbage")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, srv, bad := h.locate(fields[1])
	if srv == nil {
		return bad
	}
	srv.Maxconn = maxconn
	return silent()
}

// setServerAddr answers as HAProxy does: at WARNING severity, with what
// changed, "nothing changed", or the refusal. The caller holds the lock.
func (h *HAProxy) setServerAddr(srv *Server, fields []string) reply {
	if net.ParseIP(fields[0]) == nil {
		return warning("Invalid addr '%s'", fields[0])
	}
	host, port, err := net.SplitHostPort(srv.Address)
	if err != nil {
		host, port = srv.Address, ""
	}
	var changes []string
	if host != fields[0] {
		changes = append(changes, fmt.Sprintf("IP changed from '%s' to '%s'", host, fields[0]))
		host = fields[0]
	}
	if len(fields) >= 3 && fields[1] == "port" {
		if _, err := strconv.Atoi(fields[2]); err != nil {
			return warning("problem converting port '%s' to an int", fields[2])
		}
		if port != fields[2] {
			changes = append(changes, fmt.Sprintf("port changed from '%s' to '%s'", port, fields[2]))
			port = fields[2]
		}
	}
	srv.Address = host
	if port != "" {
		srv.Address = net.JoinHostPort(host, port)
	}
	if len(changes) == 0 {
		return warning("nothing changed")
	}
	return warning("%s by 'stats socket command'", strings.Join(changes, ", "))
}

func (h *HAProxy) wait(rest, _ string) reply {
	fields := strings.Fields(rest)
	if len(fields) < 3 {
		return failure("'wait' expects a delay, a condition and a target.")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch fields[1] {
	case "srv-removable":
		if h.m.BlockedServers[fields[2]] {
			return message("Wait delay expired. Server still has connections attached to it, cannot remove it.")
		}
		return message("Done.")
	case "be-removable":
		be, exists := h.m.Backends[fields[2]]
		if !exists {
			return failure("Failed. No such backend.")
		}
		if be.Published || len(be.Servers) > 0 {
			return message("Wait delay expired. The backend is still referenced.")
		}
		return message("Done.")
	}
	return failure("Unknown wait condition '%s'.", fields[1])
}

// locate resolves a "<backend>/<server>" reference. The caller holds the lock.
func (h *HAProxy) locate(ref string) (*Backend, *Server, reply) {
	backend, name, ok := strings.Cut(ref, "/")
	if !ok {
		return nil, nil, failure("Expected <backend>/<server>.")
	}
	be, exists := h.m.Backends[backend]
	if !exists {
		return nil, nil, failure("No such backend.")
	}
	srv := findServer(be, name)
	if srv == nil {
		return nil, nil, failure("No such server.")
	}
	return be, srv, silent()
}

func findServer(be *Backend, name string) *Server {
	for _, s := range be.Servers {
		if s.Name == name {
			return s
		}
	}
	return nil
}
