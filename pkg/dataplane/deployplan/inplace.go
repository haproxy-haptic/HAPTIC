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
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// inPlaceKinds are the ops a pod may run while a reload is pending: each one
// changes an object the running worker already has and survives the reload,
// because the file set the reload loads carries the same state.
var inPlaceKinds = map[string]bool{
	api.OpServerSetAddr:    true,
	api.OpServerSetWeight:  true,
	api.OpServerSetMaxconn: true,
	api.OpServerSetState:   true,
	api.OpServerAdd:        true,
	api.OpServerEnable:     true,
	api.OpServerDisable:    true,
	api.OpMapSet:           true,
	api.OpMapDel:           true,
	api.OpCertSet:          true,
}

// inPlaceOps composes the subset that runs while this pod waits for its
// reload — one already pending, or the one this render asks for, which the
// pod paces if its window is closed — against the plan the worker actually
// has, and returns the plan the worker holds once the subset ran. A change it
// cannot express is dropped rather than reported: the reload applies all of
// it. Without a worker-ops baseline nothing is composed: the agent fences
// in-place ops on that id, and Running or Applied is not what the worker
// holds once one in-place batch ran.
func (b *builder) inPlaceOps() ([]api.Op, *renderplan.Plan) {
	if !b.baseline.ReloadPending && !b.reload {
		return nil, nil
	}
	worker := b.baseline.WorkerOps
	if worker == nil {
		return nil, nil
	}
	// The ops that create runtime-store objects never run while a reload is
	// pending, so the worker holds nothing this diff composed.
	b.created = nil
	ops := b.inPlaceServerOps(worker)
	ops = append(ops, b.inPlaceCertOps(worker)...)
	mapOps := b.inPlaceMapOps(worker)
	if len(ops)+len(mapOps) <= api.MaxOpsPerApply {
		ops = append(ops, mapOps...)
	}
	kept := ops[:0]
	for i := range ops {
		if inPlaceKinds[ops[i].Kind] && b.caps.executes(ops[i].Kind) {
			kept = append(kept, ops[i])
		}
	}
	if len(kept) > api.MaxOpsPerApply {
		kept = kept[:api.MaxOpsPerApply]
	}
	if len(kept) == 0 {
		return nil, nil
	}
	return kept, workerAfter(worker, b.next, kept)
}

// workerAfter is the worker's plan with exactly the composed in-place ops
// applied — the baseline the next in-place batch is composed against, and the
// id the agent records for it. The in-place subset never brings the worker to
// the render (a new map key waits for the reload), so naming the render here
// would compose the next batch against state the worker does not have.
func workerAfter(worker, next *renderplan.Plan, ops []api.Op) *renderplan.Plan {
	after := *worker
	after.Backends = maps.Clone(worker.Backends)
	after.Maps = maps.Clone(worker.Maps)
	after.Files = slices.Clone(worker.Files)
	for i := range ops {
		applyInPlaceOp(&after, next, &ops[i])
	}
	encoded, err := json.Marshal(ops)
	if err != nil {
		panic(fmt.Sprintf("deployplan: encoding in-place ops failed: %v", err))
	}
	after.ID = renderplan.DigestString(worker.ID + "\x00" + next.ID + "\x00" + string(encoded))
	return &after
}

func applyInPlaceOp(after, next *renderplan.Plan, op *api.Op) {
	switch op.Kind {
	case api.OpServerAdd:
		if added, ok := serverIn(next, op.Backend, op.Server); ok {
			be := after.Backends[op.Backend]
			be.Servers = append(slices.Clone(be.Servers), added)
			after.Backends[op.Backend] = be
		}
	case api.OpServerEnable, api.OpServerDisable, api.OpServerSetAddr, api.OpServerSetWeight,
		api.OpServerSetMaxconn, api.OpServerSetState:
		mutateServer(after, op)
	case api.OpMapSet, api.OpMapDel:
		mutateMap(after, op)
	case api.OpCertSet:
		updated, ok := fileIndex(next.Files)[op.Path]
		if !ok {
			return
		}
		for i := range after.Files {
			if after.Files[i].Path == op.Path {
				after.Files[i].Digest, after.Files[i].Size = updated.Digest, updated.Size
				after.Files[i].Content, after.Files[i].ContentKnown = updated.Content, updated.ContentKnown
			}
		}
	}
}

func serverIn(plan *renderplan.Plan, backend, name string) (renderplan.Server, bool) {
	be, ok := plan.Backends[backend]
	if !ok {
		return renderplan.Server{}, false
	}
	for i := range be.Servers {
		if be.Servers[i].Name == name {
			return be.Servers[i], true
		}
	}
	return renderplan.Server{}, false
}

func mutateServer(after *renderplan.Plan, op *api.Op) {
	be, ok := after.Backends[op.Backend]
	if !ok {
		return
	}
	be.Servers = slices.Clone(be.Servers)
	for i := range be.Servers {
		srv := &be.Servers[i]
		if srv.Name != op.Server {
			continue
		}
		switch op.Kind {
		case api.OpServerEnable:
			srv.Disabled = false
		case api.OpServerDisable:
			srv.Disabled = true
		case api.OpServerSetState:
			srv.Disabled = op.State == stateMaint
		case api.OpServerSetAddr:
			srv.Address, srv.Port = op.Address, op.Port
		case api.OpServerSetWeight:
			if op.Weight != nil {
				weight := *op.Weight
				srv.Weight = &weight
			}
		case api.OpServerSetMaxconn:
			if op.Maxconn != nil {
				srv.Extra = withMaxconn(srv.Extra, *op.Maxconn)
			}
		}
	}
	after.Backends[op.Backend] = be
}

// withMaxconn is extra with its maxconn keyword set to maxconn, in place.
func withMaxconn(extra []renderplan.KeywordArg, maxconn int) []renderplan.KeywordArg {
	kw := renderplan.KeywordArg{Name: keywordMaxconn, Args: []string{strconv.Itoa(maxconn)}}
	extra = slices.Clone(extra)
	if at := slices.IndexFunc(extra, func(k renderplan.KeywordArg) bool { return k.Name == keywordMaxconn }); at >= 0 {
		extra[at] = kw
		return extra
	}
	return append(extra, kw)
}

// mutateMap applies a set or del to the map at op.Path with `set map` and
// `del map` semantics: every entry with the key.
func mutateMap(after *renderplan.Plan, op *api.Op) {
	for name, m := range after.Maps {
		path := m.Path
		if path == "" {
			path = name
		}
		if path != op.Path {
			continue
		}
		entries := make([]renderplan.Entry, 0, len(m.Entries))
		for _, e := range m.Entries {
			switch {
			case e.Key != op.Key:
				entries = append(entries, e)
			case op.Kind == api.OpMapSet:
				entries = append(entries, renderplan.Entry{Key: e.Key, Value: op.Value})
			}
		}
		m.Entries = entries
		after.Maps[name] = m
	}
}

func (b *builder) inPlaceServerOps(worker *renderplan.Plan) []api.Op {
	var ops []api.Op
	for i := range b.nextIndex.backends {
		next := &b.nextIndex.backends[i]
		running, has := worker.Backends[next.name]
		if !has || !next.described {
			continue
		}
		ops = append(ops, b.serverOpsAgainstWorker(&running, &next.record)...)
	}
	return ops
}

// serverOpsAgainstWorker judges an add by the backend the worker runs — that
// is the configuration HAProxy checks the command against — and takes the
// server's own values from the render.
func (b *builder) serverOpsAgainstWorker(running, next *renderplan.Backend) []api.Op {
	var ops []api.Op
	current := serverIndex(running.Servers)
	for i := range next.Servers {
		srv := &next.Servers[i]
		if old, existed := current[srv.Name]; existed {
			composed, _ := b.updateServer(next, old, srv)
			ops = append(ops, composed...)
			continue
		}
		composed, _ := b.addServer(running, srv)
		ops = append(ops, composed...)
	}
	desired := serverIndex(next.Servers)
	for i := range running.Servers {
		srv := &running.Servers[i]
		if _, kept := desired[srv.Name]; !kept {
			ops = append(ops, api.Op{Kind: api.OpServerDisable, Backend: next.Name, Server: srv.Name})
		}
	}
	return ops
}

// Map updates wait together when any dependency needs the pending reload.
func (b *builder) inPlaceMapOps(worker *renderplan.Plan) []api.Op {
	if b.addsBackends(worker) {
		return nil
	}
	baseline := *b.baseline
	baseline.Applied, baseline.Running = worker, worker
	baseline.AppliedIndex, baseline.WorkerOps = nil, nil // Avoid composing another in-place batch.
	baseline.ReloadPending = false
	if DiffIndexed(b.next, b.nextIndex, &baseline).Verdict == VerdictReload {
		return nil
	}
	var upserts, deletes []api.Op
	for _, name := range sortedMapNames(b.next.Maps) {
		next := b.next.Maps[name]
		path := next.Path
		if path == "" {
			path = name
		}
		if !slices.Contains(b.inventory.Maps, path) || !api.SafeToken(path) {
			return nil
		}
		prev := worker.Maps[name]
		delta := unorderedMapOps(path, prev.Entries, next.Entries)
		if next.Ordered {
			delta = orderedMapOps(path, prev.Entries, next.Entries)
		}
		if delta.whole {
			return nil
		}
		for i := range delta.upserts {
			if delta.upserts[i].Kind != api.OpMapSet {
				return nil
			}
		}
		upserts = append(upserts, delta.upserts...)
		deletes = append(deletes, delta.deletes...)
	}
	return append(upserts, deletes...)
}

// Map values can reference new backends indirectly through other maps.
func (b *builder) addsBackends(worker *renderplan.Plan) bool {
	for name := range b.next.Backends {
		if _, running := worker.Backends[name]; !running {
			return true
		}
	}
	return false
}

func (b *builder) inPlaceCertOps(worker *renderplan.Plan) []api.Op {
	var ops []api.Op
	running := fileIndex(worker.Files)
	for i := range b.next.Files {
		f := &b.next.Files[i]
		old, existed := running[f.Path]
		if f.Kind != renderplan.FileKindCert || !existed || sameFileContent(old, f) {
			continue
		}
		if slices.Contains(b.inventory.Certs, f.Path) {
			ops = append(ops, api.Op{Kind: api.OpCertSet, Path: f.Path})
		}
	}
	return ops
}
