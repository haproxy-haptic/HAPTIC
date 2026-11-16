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
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// diffCerts applies rule 6. A certificate the running worker does not hold is
// created before anything can reference it, because add ssl crt-list resolves
// against the runtime store rather than the filesystem.
func (b *builder) diffCerts() {
	for i := range b.next.Files {
		f := &b.next.Files[i]
		old, existed := b.prevIndex.files[f.Path]
		if existed && sameFileContent(old, f) {
			continue
		}
		switch f.Kind {
		case renderplan.FileKindCert:
			b.storeFile(f.Path, api.OpCertSet, api.OpCertNew, b.inventory.Certs)
		case renderplan.FileKindCA:
			b.storeFile(f.Path, api.OpCASet, api.OpCANew, b.inventory.CAFiles)
		}
	}
	// After every store file: an added entry needs its certificate in the store.
	for i := range b.next.Files {
		f := &b.next.Files[i]
		old, existed := b.prevIndex.files[f.Path]
		if f.Kind == renderplan.FileKindCRTList && (!existed || !sameFileContent(old, f)) {
			b.diffCRTList(f.Path, existed)
		}
	}
	for i := range b.prev.Files {
		f := &b.prev.Files[i]
		if _, kept := b.nextIndex.files[f.Path]; !kept && f.Kind == renderplan.FileKindCRTList {
			b.failf("crt-list %s removed, which only a reload takes out of the config", f.Path)
		}
	}
}

// storeFile creates or replaces one runtime-store object and records the
// creation, because the agent folds a created object into its inventory too —
// without that agreement the next diff would create it a second time.
func (b *builder) storeFile(path, set, create string, loaded []string) {
	if !api.SafeToken(path) {
		b.failf("file %s is not a safe runtime token", path)
		return
	}
	kind := create
	if slices.Contains(loaded, path) || b.created[path] {
		kind = set
	}
	b.created[path] = true
	b.push(groupCert, api.Op{Kind: kind, Path: path})
}

// diffCRTList turns the entry lists of two renders into crt-list ops. A list
// the config gains or loses is a core change, and one the render did not
// describe cannot be reached entry by entry.
func (b *builder) diffCRTList(path string, existed bool) {
	if !existed {
		b.failf("crt-list %s added, which only a reload puts into the config", path)
		return
	}
	prev, hadEntries := b.prev.CRTLists[path]
	next, hasEntries := b.next.CRTLists[path]
	switch {
	case !hadEntries || !hasEntries:
		b.failf("crt-list %s changed, but crt-list entries are not declared by the render yet", path)
	case !slices.Contains(b.inventory.CRTLists, path):
		b.notef("crt-list %s is not loaded at runtime, its file is written only", path)
	default:
		b.crtListOps(path, prev.Entries, next.Entries)
	}
}

// crtListOps composes per-entry ops only when replaying them leaves the worker
// in the file's order: `add ssl crt-list` appends, and HAProxy serves the first
// entry to a handshake without a matching SNI, so an entry that would have to
// move is a reload rather than a silently different default certificate.
func (b *builder) crtListOps(path string, prev, next []renderplan.CRTListEntry) {
	before, after := crtListIndex(prev), crtListIndex(next)
	for i := range next {
		if reason := crtListEntryReason(&next[i]); reason != "" {
			b.failf("crt-list %s: %s", path, reason)
			return
		}
	}
	var ops []api.Op
	running := make([]string, 0, len(prev)+len(next))
	for i := range prev {
		cert := prev[i].Cert
		if _, kept := after[cert]; kept {
			running = append(running, cert)
			continue
		}
		if !api.SafeToken(cert) {
			b.failf("crt-list %s: certificate %s is not a safe runtime token", path, cert)
			return
		}
		ops = append(ops, b.crtListDel(path, cert))
	}
	for i := range next {
		cert := next[i].Cert
		old, existed := before[cert]
		if existed && sameCRTListEntry(old, &next[i]) {
			continue
		}
		if existed {
			// Options and SNI filters are only replaceable as a whole entry,
			// which re-adds it at the end of the running list.
			ops = append(ops, b.crtListDel(path, cert))
			running = slices.DeleteFunc(running, func(c string) bool { return c == cert })
		}
		add := crtListAdd(path, &next[i])
		if file := b.unloadedFile(&add); file != "" {
			b.failf("crt-list %s: %s is not loaded at runtime", path, file)
			return
		}
		ops = append(ops, add)
		running = append(running, cert)
	}
	if !slices.Equal(running, crtListOrder(next)) {
		b.failf("crt-list %s: the entry order changed, which only a reload applies", path)
		return
	}
	b.push(groupCRTList, ops...)
}

// crtListEntryReason names the first token of an entry the runtime API cannot
// carry, or "" when every one of them travels.
func crtListEntryReason(entry *renderplan.CRTListEntry) string {
	if !api.SafeToken(entry.Cert) {
		return "certificate " + entry.Cert + " is not a safe runtime token"
	}
	for i := range entry.Options {
		if !api.SafeToken(entry.Options[i].Name) || !allSafeTokens(entry.Options[i].Args) {
			return "option " + entry.Options[i].Name + " is not a safe runtime token"
		}
	}
	for _, sni := range entry.SNIFilters {
		if !api.SafeToken(sni) {
			return "SNI filter " + sni + " is not a safe runtime token"
		}
	}
	return ""
}

// unloadedFile names the first store file an added entry references that the
// worker neither holds nor gets from this diff; add ssl crt-list refuses it.
func (b *builder) unloadedFile(add *api.Op) string {
	files := append([]api.KeywordArg{{Name: keywordCRT, Args: []string{add.Cert}}}, add.Options...)
	for i := range files {
		if !b.filesLoaded(&files[i]) {
			return strings.Join(append([]string{files[i].Name}, files[i].Args...), " ")
		}
	}
	return ""
}

func crtListOrder(entries []renderplan.CRTListEntry) []string {
	order := make([]string, 0, len(entries))
	for i := range entries {
		order = append(order, entries[i].Cert)
	}
	return order
}

func crtListAdd(path string, entry *renderplan.CRTListEntry) api.Op {
	return api.Op{
		Kind:       api.OpCRTListAdd,
		Path:       path,
		Cert:       entry.Cert,
		Options:    apiKeywords(entry.Options),
		SNIFilters: slices.Clone(entry.SNIFilters),
	}
}

// crtListDel names the entry by its store name: unlike add ssl crt-list, del
// does not join crt-base onto a bare line token (pinned in tests/agent).
func (b *builder) crtListDel(path, cert string) api.Op {
	if !slices.Contains(b.inventory.Certs, cert) {
		cert = b.underCRTBase(cert)
	}
	return api.Op{Kind: api.OpCRTListDel, Path: path, Cert: cert}
}

func crtListIndex(entries []renderplan.CRTListEntry) map[string]*renderplan.CRTListEntry {
	index := make(map[string]*renderplan.CRTListEntry, len(entries))
	for i := range entries {
		index[entries[i].Cert] = &entries[i]
	}
	return index
}

func sameCRTListEntry(prev, next *renderplan.CRTListEntry) bool {
	return slices.EqualFunc(prev.Options, next.Options, sameKeyword) &&
		slices.Equal(prev.SNIFilters, next.SNIFilters)
}
