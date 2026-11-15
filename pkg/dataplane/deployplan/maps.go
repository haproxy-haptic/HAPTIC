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

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// mapOps is one map file's delta, split by when it may run: a replacement's
// del stays next to its re-adds, only a key that disappears is deferred until
// after the new state is published. whole means no per-entry sequence reaches
// the desired content and the map is swapped atomically instead.
type mapOps struct {
	upserts []api.Op
	deletes []api.Op
	whole   bool
}

// diffMaps applies rule 5, short-circuiting on exact file content before any
// entry is compared — at 3000 routes the map diff is the larger term.
func (b *builder) diffMaps() {
	for _, name := range sortedMapNames(b.next.Maps) {
		next := b.next.Maps[name]
		prev, existed := b.prev.Maps[name]
		if existed && sameFileContent(b.prevIndex.files[name], b.nextIndex.files[name]) {
			continue
		}
		b.diffMap(&prev, &next, name)
	}
}

func (b *builder) diffMap(prev, next *renderplan.Map, name string) {
	path := next.Path
	if path == "" {
		path = name
	}
	if !slices.Contains(b.inventory.Maps, path) {
		b.notef("map %s is not loaded at runtime, its file is written only", path)
		return
	}
	if !api.SafeToken(path) {
		b.failf("map %s: the path is not a safe runtime token", path)
		return
	}
	if next.Ordered {
		b.pushMapOps(orderedMapOps(path, prev.Entries, next.Entries), path)
		return
	}
	b.pushMapOps(unorderedMapOps(path, prev.Entries, next.Entries), path)
}

// replaceOpaqueMap swaps a map the plan carries no entries for, a sensitive
// one, as a whole: per-entry ops would put its keys and values in ops, diffs
// and errors.
func (b *builder) replaceOpaqueMap(path string) {
	if !slices.Contains(b.inventory.Maps, path) {
		b.notef("map %s is not loaded at runtime, its file is written only", path)
		return
	}
	if !api.SafeToken(path) {
		b.failf("map %s: the path is not a safe runtime token", path)
		return
	}
	b.push(groupMapUpsert, api.Op{Kind: api.OpMapReplace, Path: path})
}

func (b *builder) pushMapOps(ops mapOps, path string) {
	if ops.whole {
		b.push(groupMapUpsert, api.Op{Kind: api.OpMapReplace, Path: path})
		return
	}
	b.push(groupMapUpsert, ops.upserts...)
	b.push(groupMapDel, ops.deletes...)
}

// unorderedMapOps is the per-entry delta for a map whose lookup order does not
// matter: a runtime append lands wherever HAProxy puts it. Only the window
// between the common prefix and suffix is compared: a route change touches a
// few lines of a map thousands long, and the full compare cost 6 ms per
// deployment at 2,600 routes.
func unorderedMapOps(path string, prev, next []renderplan.Entry) mapOps {
	if prevWindow, nextWindow, ok := trimCommonEntries(prev, next); ok {
		prev, next = prevWindow, nextWindow
	}
	return unorderedEntryOps(path, prev, next)
}

// unorderedEntryOps is the per-entry delta over exactly the entries given.
func unorderedEntryOps(path string, prev, next []renderplan.Entry) mapOps {
	before, after := valuesByKey(prev), valuesByKey(next)
	ops := mapOps{}
	for _, key := range keyOrder(next) {
		want := after[key]
		have, existed := before[key]
		switch {
		case existed && sameValues(have, want):
		case !api.SafeToken(key) || !payloadSafe(want):
			return mapOps{whole: true}
		case !existed:
			ops.upserts = append(ops.upserts, addEntries(path, key, want)...)
		case len(have) == 1 && len(want) == 1 && api.SafeToken(want[0]):
			ops.upserts = append(ops.upserts, api.Op{Kind: api.OpMapSet, Path: path, Key: key, Value: want[0]})
		default:
			// An absent key can disable authentication; replace it atomically.
			return mapOps{whole: true}
		}
	}
	return withRemovals(ops, path, prev, after)
}

// trimCommonEntries drops the entries both files share at the front and the
// back. The delta over the window equals the delta over the whole files only
// while no key of the window also appears in the trimmed part: values are a
// multiset per key across the file, and a del names every value of its key,
// so such a key needs the full compare and the trim is refused.
func trimCommonEntries(prev, next []renderplan.Entry) (prevWindow, nextWindow []renderplan.Entry, ok bool) {
	front := 0
	for front < len(prev) && front < len(next) && prev[front] == next[front] {
		front++
	}
	back := 0
	for back < len(prev)-front && back < len(next)-front &&
		prev[len(prev)-1-back] == next[len(next)-1-back] {
		back++
	}
	if front == 0 && back == 0 {
		return prev, next, true
	}
	prevWindow, nextWindow = prev[front:len(prev)-back], next[front:len(next)-back]
	windowKeys := make(map[string]struct{}, len(prevWindow)+len(nextWindow))
	for i := range prevWindow {
		windowKeys[prevWindow[i].Key] = struct{}{}
	}
	for i := range nextWindow {
		windowKeys[nextWindow[i].Key] = struct{}{}
	}
	if len(windowKeys) == 0 {
		return prevWindow, nextWindow, true
	}
	// The trimmed entries are the same in both files; prev stands for both.
	for i := range prev[:front] {
		if _, shared := windowKeys[prev[i].Key]; shared {
			return nil, nil, false
		}
	}
	for i := range prev[len(prev)-back:] {
		if _, shared := windowKeys[prev[len(prev)-back+i].Key]; shared {
			return nil, nil, false
		}
	}
	return prevWindow, nextWindow, true
}

// orderedMapOps is the delta for a map HAProxy matches in order. Only appends
// that land past every retained key, in-place value changes and deletes keep
// the order intact; anything else is swapped as a whole.
func orderedMapOps(path string, prev, next []renderplan.Entry) mapOps {
	before, after := valuesByKey(prev), valuesByKey(next)
	if !sameRelativeOrder(prev, next) {
		return mapOps{whole: true}
	}
	ops := mapOps{}
	appended := appendedKeys(prev, next)
	for _, key := range keyOrder(next) {
		want := after[key]
		have, existed := before[key]
		switch {
		case existed && sameValues(have, want):
		case !api.SafeToken(key) || !payloadSafe(want):
			return mapOps{whole: true}
		case !existed && appended[key]:
			ops.upserts = append(ops.upserts, addEntries(path, key, want)...)
		case existed && len(have) == 1 && len(want) == 1 && api.SafeToken(want[0]):
			ops.upserts = append(ops.upserts, api.Op{Kind: api.OpMapSet, Path: path, Key: key, Value: want[0]})
		default:
			return mapOps{whole: true}
		}
	}
	return withRemovals(ops, path, prev, after)
}

// appendedKeys are the keys the new render gained that sit after every key it
// keeps from the old one. `add map` appends to the end of the runtime list, so
// only a suffix of the file reaches its own position that way — a lexicographic
// comparison would put a middle insertion last and route it to the wrong entry.
func appendedKeys(prev, next []renderplan.Entry) map[string]bool {
	before := valuesByKey(prev)
	keys := keyOrder(next)
	lastKept := -1
	for i, key := range keys {
		if _, kept := before[key]; kept {
			lastKept = i
		}
	}
	appended := make(map[string]bool, len(keys)-lastKept-1)
	for _, key := range keys[lastKept+1:] {
		appended[key] = true
	}
	return appended
}

// withRemovals appends a del for every key the render dropped, in the order
// the map had them; a key no line-form command can name blocks the per-entry
// path for the whole map.
func withRemovals(ops mapOps, path string, prev []renderplan.Entry, after map[string][]string) mapOps {
	for _, key := range keyOrder(prev) {
		if _, kept := after[key]; kept {
			continue
		}
		if !api.SafeToken(key) {
			return mapOps{whole: true}
		}
		ops.deletes = append(ops.deletes, api.Op{Kind: api.OpMapDel, Path: path, Key: key})
	}
	return ops
}

// addEntries writes new values in payload form, which is byte exact where the
// line form mangles spaces, ';' and backslashes.
func addEntries(path, key string, values []string) []api.Op {
	ops := make([]api.Op, 0, len(values))
	for _, value := range values {
		ops = append(ops, api.Op{Kind: api.OpMapAdd, Path: path, Key: key, Value: value})
	}
	return ops
}

// sameRelativeOrder reports whether the entries both renders share still come
// in the same order; a reordering is not reachable per entry.
func sameRelativeOrder(prev, next []renderplan.Entry) bool {
	before, after := valuesByKey(prev), valuesByKey(next)
	return slices.Equal(keysPresentIn(prev, after), keysPresentIn(next, before))
}

// keysPresentIn lists the keys of entries that the other render also has.
func keysPresentIn(entries []renderplan.Entry, other map[string][]string) []string {
	keys := make([]string, 0, len(entries))
	for i := range entries {
		if _, ok := other[entries[i].Key]; ok {
			keys = append(keys, entries[i].Key)
		}
	}
	return keys
}

// valuesByKey groups a map file's entries, keeping duplicates: HAProxy map
// lookups are a multiset and a delta over them must see every occurrence.
func valuesByKey(entries []renderplan.Entry) map[string][]string {
	grouped := make(map[string][]string, len(entries))
	for i := range entries {
		grouped[entries[i].Key] = append(grouped[entries[i].Key], entries[i].Value)
	}
	return grouped
}

// keyOrder lists each key once, in the order the file first has it.
func keyOrder(entries []renderplan.Entry) []string {
	seen := make(map[string]bool, len(entries))
	keys := make([]string, 0, len(entries))
	for i := range entries {
		if seen[entries[i].Key] {
			continue
		}
		seen[entries[i].Key] = true
		keys = append(keys, entries[i].Key)
	}
	return keys
}

// payloadSafe reports whether every value can travel in the payload form the
// adds use, which only the line framing constrains.
func payloadSafe(values []string) bool {
	for _, value := range values {
		if !api.SafePayloadValue(value) {
			return false
		}
	}
	return true
}

func sameValues(prev, next []string) bool {
	if len(prev) != len(next) {
		return false
	}
	a, b := slices.Clone(prev), slices.Clone(next)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
