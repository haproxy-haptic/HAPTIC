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
	// After every store file: an added entry needs its certificate in the
	// store, and a committed store object moves its entries in the SNI lookup.
	for i := range b.next.Files {
		f := &b.next.Files[i]
		old, existed := b.prevIndex.files[f.Path]
		changed := !existed || !sameFileContent(old, f)
		if f.Kind == renderplan.FileKindCRTList && (changed || len(b.groups[groupCert]) > 0) {
			b.diffCRTList(f.Path, existed, changed)
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
// describe cannot be reached entry by entry. An unchanged list is checked
// because a store commit can move its entries.
func (b *builder) diffCRTList(path string, existed, changed bool) {
	if !existed {
		b.failf("crt-list %s added, which only a reload puts into the config", path)
		return
	}
	prev, hadEntries := b.prev.CRTLists[path]
	next, hasEntries := b.next.CRTLists[path]
	loaded := slices.Contains(b.inventory.CRTLists, path)
	switch {
	case !changed && (!hadEntries || !hasEntries || !loaded):
	case !hadEntries || !hasEntries:
		b.failf("crt-list %s changed, but crt-list entries are not declared by the render yet", path)
	case !loaded:
		b.notef("crt-list %s is not loaded at runtime, its file is written only", path)
	default:
		b.crtListOps(path, prev.Entries, next.Entries)
	}
}

// crtListOps composes per-entry ops when the worker, after replaying them,
// selects the same entry for every SNI as a reload of the file would.
func (b *builder) crtListOps(path string, prev, next []renderplan.CRTListEntry) {
	before, after := crtListIndex(prev), crtListIndex(next)
	var ops []api.Op
	kept := make([]string, 0, len(prev))
	var added []string
	for i := range prev {
		cert := prev[i].Cert
		if _, ok := after[cert]; ok {
			kept = append(kept, cert)
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
		if reason := crtListEntryReason(&next[i]); reason != "" {
			b.failf("crt-list %s: %s", path, reason)
			return
		}
		if existed {
			// Options and SNI filters are only replaceable as a whole entry.
			ops = append(ops, b.crtListDel(path, cert))
			kept = slices.DeleteFunc(kept, func(c string) bool { return c == cert })
		}
		add := crtListAdd(path, &next[i])
		if file := b.unloadedFile(&add); file != "" {
			b.failf("crt-list %s: %s is not loaded at runtime", path, file)
			return
		}
		ops = append(ops, add)
		added = append(added, cert)
	}
	if reason := sniOrderReason(prev, next, b.sniOrder(kept, added), b.caCommitted()); reason != "" {
		b.failf("crt-list %s: %s, which only a reload applies", path, reason)
		return
	}
	b.push(groupCRTList, ops...)
}

// sniOrder is the worker's SNI lookup order after this diff: the previous
// render's order, replaying that a commit or an add re-inserts at the end.
func (b *builder) sniOrder(kept, added []string) []string {
	order := slices.Clone(kept)
	for i := range b.groups[groupCert] {
		op := &b.groups[groupCert][i]
		if op.Kind != api.OpCertSet {
			continue
		}
		var moved []string
		order = slices.DeleteFunc(order, func(cert string) bool {
			hit := cert == op.Path || b.underCRTBase(cert) == op.Path
			if hit {
				moved = append(moved, cert)
			}
			return hit
		})
		order = append(order, moved...)
	}
	return append(order, added...)
}

// caCommitted reports a CA-file commit, which re-inserts every entry using
// the file, bind-level ones included, in an order the plan does not know.
func (b *builder) caCommitted() bool {
	return slices.ContainsFunc(b.groups[groupCert], func(op api.Op) bool { return op.Kind == api.OpCASet })
}

// sniOrderReason names why the worker could select a different entry than the
// file for some SNI, or returns "". See "CRT-list entry order" in
// docs/site/docs/development/agent.md.
func sniOrderReason(prev, next []renderplan.CRTListEntry, running []string, unknownOrder bool) string {
	if len(next) == 0 {
		return ""
	}
	if len(prev) == 0 || prev[0].Cert != next[0].Cert || !sameCRTListEntry(&prev[0], &next[0]) {
		return "the default certificate changes"
	}
	runningAt := make(map[string]int, len(running))
	for i, cert := range running {
		runningAt[cert] = i
	}
	kept := func(x, y string) bool { return !unknownOrder && runningAt[x] < runningAt[y] }
	names, byName, unnamed := groupBySNI(next)
	for _, name := range names {
		certs := byName[name]
		for j := 1; j < len(certs); j++ {
			if !kept(certs[j-1], certs[j]) {
				return "entries " + certs[j-1] + " and " + certs[j] + " both match SNI " + name + " and their order is not kept"
			}
		}
	}
	for _, i := range unnamed {
		for j := range next {
			if j != i && !kept(next[min(i, j)].Cert, next[max(i, j)].Cert) {
				return "entry " + next[i].Cert + " has no SNI filter and its order against " + next[j].Cert + " is not kept"
			}
		}
	}
	return ""
}

// groupBySNI lists each lookup name's certificates in file order, the names in
// first-seen order, and the indexes of entries without filters. The first
// entry also holds the default's name, "*".
func groupBySNI(entries []renderplan.CRTListEntry) (names []string, byName map[string][]string, unnamed []int) {
	byName = make(map[string][]string)
	for i := range entries {
		keys, known := sniKeys(&entries[i])
		if !known {
			unnamed = append(unnamed, i)
			continue
		}
		if i == 0 && !slices.Contains(keys, "*") {
			keys = append(keys, "*")
		}
		for _, key := range keys {
			if _, seen := byName[key]; !seen {
				names = append(names, key)
			}
			byName[key] = append(byName[key], entries[i].Cert)
		}
	}
	return names, byName, unnamed
}

// sniKeys are the lookup names an entry's filters insert, lowercased as
// HAProxy stores them; a negative filter only excludes its own wildcard.
// known is false for an entry without filters.
func sniKeys(entry *renderplan.CRTListEntry) (keys []string, known bool) {
	if len(entry.SNIFilters) == 0 {
		return nil, false
	}
	for _, filter := range entry.SNIFilters {
		if strings.HasPrefix(filter, "!") {
			continue
		}
		if key := strings.ToLower(filter); !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys, true
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
