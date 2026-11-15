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
	"fmt"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// Op groups, emitted in this order so a mixed create and delete — a backend
// rename — never drops traffic: everything the new state needs exists and is
// published before anything the old state used goes away.
const (
	groupBackendAdd = iota
	groupServerUpdate
	groupBackendPublish
	groupMapUpsert
	groupCert
	groupCRTList
	groupMapDel
	groupBackendUnpublish
	groupServerDel
	groupBackendDel
	groupCount
)

// builder accumulates one pod's decision while the rules run.
type builder struct {
	composer
	next, prev    *renderplan.Plan
	nextIndex     *Index
	prevIndex     *Index
	baseline      *Baseline
	groups        [groupCount][]api.Op
	reasons       []string
	reload        bool
	sectionChange bool
	deletedByOps  map[string]bool // backends this diff removes at runtime
}

// Diff decides what the pod described by base has to do to reach next.
func Diff(next *renderplan.Plan, base *Baseline) Decision {
	return DiffIndexed(next, nil, base)
}

// DiffIndexed is Diff with next's index already built; nil builds it here.
func DiffIndexed(next *renderplan.Plan, nextIndex *Index, base *Baseline) Decision {
	if next == nil {
		return Decision{Verdict: VerdictReload, Mode: api.ModeReload, Reasons: []string{"no render"}}
	}
	if base == nil {
		base = &Baseline{}
	}
	b := &builder{
		composer: composer{
			caps:                  base.Caps,
			inventory:             &base.Inventory,
			created:               map[string]bool{},
			certDir:               certDirOf(next),
			pendingServerDeletes:  base.PendingServerDeletes,
			pendingBackendDeletes: base.PendingBackendDeletes,
		},
		next:         next,
		prev:         base.Applied,
		baseline:     base,
		deletedByOps: map[string]bool{},
	}
	b.nextIndex = nextIndex
	if b.nextIndex == nil {
		b.nextIndex = IndexPlan(next)
	}
	if b.baselineUsable() {
		b.prevIndex = base.AppliedIndex
		if b.prevIndex == nil {
			b.prevIndex = IndexPlan(b.prev)
		}
		// Ordered: a server keyword may name a certificate this diff creates, a
		// removed profile is judged by the backend deletes this diff composes,
		// and the config guard by whether any section changed.
		b.diffCerts()
		b.diffBackends()
		b.diffSections()
		b.diffMaps()
		b.diffFiles()
	}
	return b.decide()
}

// baselineUsable reports whether prev describes the pod well enough to diff
// against; anything else is a full-state reload.
func (b *builder) baselineUsable() bool {
	switch {
	case b.prev == nil:
		b.failf("no baseline")
	case b.prev.SchemaVersion != b.next.SchemaVersion:
		b.failf("baseline plan schema %d, render schema %d", b.prev.SchemaVersion, b.next.SchemaVersion)
	default:
		return true
	}
	return false
}

// decide turns the accumulated ops and reasons into the verdict (rule 8).
func (b *builder) decide() Decision {
	ops := b.orderedOps()
	if kind := b.unsupportedKind(ops); kind != "" {
		b.failf("the agent does not execute %s", kind)
	}
	inPlace, workerPlan := b.inPlaceOps()
	chunks := chunkCount(len(ops), len(inPlace))
	if chunks > MaxChunks {
		b.failf("op cap: %d ops need more than %d applies", len(ops), MaxChunks)
	}
	d := Decision{
		Verdict:    VerdictFileOnly,
		Mode:       api.ModeAuto,
		Files:      Files(b.next),
		Reasons:    b.reasons,
		InPlace:    inPlace,
		WorkerPlan: workerPlan,
	}
	switch {
	case b.reload:
		d.Verdict, d.Mode = VerdictReload, api.ModeReload
	case len(ops) > 0:
		d.Verdict, d.Ops, d.Chunks = VerdictRuntime, ops, chunks
	}
	return d
}

func (b *builder) orderedOps() []api.Op {
	total := 0
	for i := range b.groups {
		total += len(b.groups[i])
	}
	if total == 0 {
		return nil
	}
	ops := make([]api.Op, 0, total)
	for i := range b.groups {
		ops = append(ops, b.groups[i]...)
	}
	return ops
}

// unsupportedKind returns the first op kind this pod's agent does not execute.
func (b *builder) unsupportedKind(ops []api.Op) string {
	if b.caps.AgentOps == nil {
		return ""
	}
	for i := range ops {
		if !b.caps.executes(ops[i].Kind) {
			return ops[i].Kind
		}
	}
	return ""
}

func (b *builder) push(group int, ops ...api.Op) {
	b.groups[group] = append(b.groups[group], ops...)
}

// failf records why a change cannot run at runtime and forces a reload.
func (b *builder) failf(format string, args ...any) {
	b.reload = true
	b.notef(format, args...)
}

// notef records a change that was written but not applied at runtime.
func (b *builder) notef(format string, args ...any) {
	if len(b.reasons) >= MaxReasons {
		return
	}
	b.reasons = append(b.reasons, fmt.Sprintf(format, args...))
}

type sectionKey struct{ kind, name string }

// diffSections applies the section guard (rule 1) to core and profile
// sections; backend sections are guarded by their record in diffBackends.
func (b *builder) diffSections() {
	for i := range b.next.Sections {
		sec := &b.next.Sections[i]
		if sec.Kind == renderplan.SectionKindBackend {
			continue
		}
		old, existed := b.prevIndex.sections[sectionKey{sec.Kind, sec.Name}]
		switch {
		case !existed:
			b.sectionAdded(sec)
		case !sameSectionText(old, sec):
			b.sectionChanged(sec)
		}
	}
	for i := range b.prev.Sections {
		sec := &b.prev.Sections[i]
		if sec.Kind == renderplan.SectionKindBackend {
			continue
		}
		if _, kept := b.nextIndex.sections[sectionKey{sec.Kind, sec.Name}]; !kept {
			b.sectionRemoved(sec)
		}
	}
}

func (b *builder) sectionAdded(sec *renderplan.Section) {
	b.sectionChange = true
	if sec.Kind == renderplan.SectionKindProfile {
		b.failf("profile %s added", sec.Name)
		return
	}
	b.failf("core section %s added", sec.Name)
}

func (b *builder) sectionChanged(sec *renderplan.Section) {
	b.sectionChange = true
	if sec.Kind == renderplan.SectionKindProfile {
		b.failf("profile %s changed", sec.Name)
		return
	}
	b.failf("core section %s changed", sec.Name)
}

func (b *builder) sectionRemoved(sec *renderplan.Section) {
	b.sectionChange = true
	if sec.Kind == renderplan.SectionKindProfile {
		b.profileRemoved(sec.Name)
		return
	}
	b.failf("core section %s removed", sec.Name)
}

// profileRemoved keeps a profile removal off the reload path only when the
// running worker can no longer reach it: nothing in the render uses it and
// every backend that did is deleted by an op in this same diff.
func (b *builder) profileRemoved(name string) {
	for i := range b.nextIndex.backends {
		be := &b.nextIndex.backends[i]
		if be.described && be.record.Profile == name {
			b.failf("profile %s removed but backend %s still uses it", name, be.name)
			return
		}
	}
	for i := range b.prevIndex.backends {
		be := &b.prevIndex.backends[i]
		if be.described && be.record.Profile == name && !b.deletedByOps[be.name] {
			b.failf("profile %s removed but backend %s is not deleted at runtime", name, be.name)
			return
		}
	}
}

// diffFiles carries rules 7 and the config guard: a file that declares
// reloadOnChange reloads when it changes, and a config whose text no section
// accounts for is an unexplained change. The config file's own flag is not
// consulted — the section guards decide whether its change reloads.
func (b *builder) diffFiles() {
	for i := range b.next.Files {
		f := &b.next.Files[i]
		if old, existed := b.prevIndex.files[f.Path]; existed && sameFileContent(old, f) {
			continue
		}
		if f.Kind == renderplan.FileKindConfig {
			if !b.sectionChange {
				b.failf("config %s changed with no section explaining it", f.Path)
			}
			continue
		}
		if _, described := b.next.Maps[f.Path]; f.Kind == renderplan.FileKindMap && !described {
			b.replaceOpaqueMap(f.Path)
		}
		if f.ReloadOnChange {
			b.failf("file %s changed and is declared reload-on-change", f.Path)
		}
	}
	b.diffRemovedFiles()
}

// diffRemovedFiles carries rule 7 for a path the render dropped: the agent's
// ownership set makes absence a delete, and a deletion is the strongest change
// a reload-on-change file can see. crt-lists are left to diffCerts, which names
// them with the config change they really are.
func (b *builder) diffRemovedFiles() {
	for i := range b.prev.Files {
		f := &b.prev.Files[i]
		if _, kept := b.nextIndex.files[f.Path]; kept || f.Kind == renderplan.FileKindCRTList {
			continue
		}
		if f.ReloadOnChange {
			b.failf("file %s was removed and is declared reload-on-change", f.Path)
			continue
		}
		b.notef("file %s was removed, which no runtime op undoes", f.Path)
	}
}
