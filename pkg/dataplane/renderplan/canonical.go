// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package renderplan

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
)

// errCanonicalOrderUnproven reports a snapshot whose walk order this writer
// cannot prove reproduces encoding/json, so the digest has to come from the
// full plan instead. Never a corruption verdict — that is errInvalidSnapshot.
var errCanonicalOrderUnproven = errors.New("render plan snapshot canonical order is unproven")

// canonicalFragment is one entry's share of the canonical encoding. Every
// variable byte in it comes from encoding/json, so escaping matches the full
// encoding by construction rather than by imitation.
type canonicalFragment struct {
	once  sync.Once
	bytes []byte
	err   error
	// spans is the offset of every list element inside bytes, for the
	// fragments that splice their successor: see canonicalMapMemberFragment.
	spans []int
}

type canonicalWriter struct {
	target io.Writer
	err    error
}

func (w *canonicalWriter) write(value []byte) {
	if w.err != nil {
		return
	}
	_, w.err = w.target.Write(value)
}

func (w *canonicalWriter) writeString(value string) {
	if w.err != nil {
		return
	}
	_, w.err = io.WriteString(w.target, value)
}

// writeCanonicalPlan streams what (*Plan).Canonical() would encode for root,
// reusing each entry's memoised fragment. The field order is encoding/json's
// declaration order for Plan; the field inventory test pins it.
func writeCanonicalPlan(root *planRoot, target io.Writer) error {
	return writePlanJSON(root, "", canonicalFileFragment, target)
}

// writePlanJSON streams the plan's encoding/json form carrying id, out of the
// same memoised fragments the canonical form is digested from; the canonical
// form is the id-less one, with the sealed files' digests cleared.
func writePlanJSON(
	root *planRoot,
	id string,
	fileFragment func(*snapshotEntry[File]) ([]byte, error),
	target io.Writer,
) error {
	writer := &canonicalWriter{target: target}
	var digits [24]byte
	writer.writeString(`{"schemaVersion":`)
	writer.write(strconv.AppendInt(digits[:0], int64(root.schema), 10))
	writer.writeString(`,"id":`)
	encodedID, err := json.Marshal(id)
	if err != nil {
		return err
	}
	writer.write(encodedID)
	writer.writeString(`,"sections":`)
	if err := writeCanonicalSequence(writer, root.sections, canonicalValueFragment); err != nil {
		return err
	}
	writer.writeString(`,"backends":`)
	if err := writeCanonicalMap(writer, root.backends); err != nil {
		return err
	}
	writer.writeString(`,"profiles":`)
	if err := writeCanonicalMap(writer, root.profiles); err != nil {
		return err
	}
	writer.writeString(`,"maps":`)
	if err := writeCanonicalMapWith(writer, root.maps, canonicalMapMemberFragment); err != nil {
		return err
	}
	if err := writeCanonicalCRTLists(writer, root.crtLists); err != nil {
		return err
	}
	writer.writeString(`,"files":`)
	if err := writeCanonicalSequence(writer, root.files, fileFragment); err != nil {
		return err
	}
	writer.writeString("}")
	return writer.err
}

// writeCanonicalCRTLists reproduces the plan-level omitempty on CRTLists, which
// drops a non-nil empty map as well as a nil one.
func writeCanonicalCRTLists(writer *canonicalWriter, collection *snapshotCollection[CRTList]) error {
	if collection == nil {
		return errCanonicalOrderUnproven
	}
	if !collection.present || collection.entries == 0 {
		return writer.err
	}
	writer.writeString(`,"crtLists":`)
	return writeCanonicalMap(writer, collection)
}

func writeCanonicalSequence[T any](
	writer *canonicalWriter,
	collection *snapshotCollection[T],
	fragment func(*snapshotEntry[T]) ([]byte, error),
) error {
	if collection == nil {
		return errCanonicalOrderUnproven
	}
	if !collection.present {
		writer.writeString("null")
		return writer.err
	}
	writer.writeString("[")
	cursor := newSnapshotCursor(collection)
	written := 0
	for {
		entry, found, err := cursor.next()
		if err != nil {
			return err
		}
		if !found {
			break
		}
		if written > 0 {
			writer.writeString(",")
		}
		encoded, err := fragment(entry)
		if err != nil {
			return err
		}
		writer.write(encoded)
		written++
	}
	if written != collection.entries {
		return errCanonicalOrderUnproven
	}
	writer.writeString("]")
	return writer.err
}

// writeCanonicalMap relies on the in-order walk being encoding/json's key
// order, so it checks that order instead of assuming it: every map entry keys
// on index -1, which reduces compareSnapshotKeys to the same strings.Compare
// encoding/json sorts with.
func writeCanonicalMap[T any](
	writer *canonicalWriter,
	collection *snapshotCollection[T],
) error {
	return writeCanonicalMapWith(writer, collection, canonicalMemberFragment[T])
}

func writeCanonicalMapWith[T any](
	writer *canonicalWriter,
	collection *snapshotCollection[T],
	fragment func(*snapshotEntry[T]) ([]byte, error),
) error {
	if collection == nil {
		return errCanonicalOrderUnproven
	}
	if !collection.present {
		writer.writeString("null")
		return writer.err
	}
	writer.writeString("{")
	cursor := newSnapshotCursor(collection)
	written := 0
	previous := ""
	for {
		entry, found, err := cursor.next()
		if err != nil {
			return err
		}
		if !found {
			break
		}
		if entry.key.index != -1 {
			return errCanonicalOrderUnproven
		}
		if written > 0 {
			if strings.Compare(previous, entry.key.name) >= 0 {
				return errCanonicalOrderUnproven
			}
			writer.writeString(",")
		}
		encoded, err := fragment(entry)
		if err != nil {
			return err
		}
		writer.write(encoded)
		previous = entry.key.name
		written++
	}
	if written != collection.entries {
		return errCanonicalOrderUnproven
	}
	writer.writeString("}")
	return writer.err
}

func canonicalValueFragment[T any](entry *snapshotEntry[T]) ([]byte, error) {
	entry.canonical.once.Do(func() {
		entry.canonical.bytes, entry.canonical.err = json.Marshal(entry.value.value)
	})
	return entry.canonical.bytes, entry.canonical.err
}

func canonicalMemberFragment[T any](entry *snapshotEntry[T]) ([]byte, error) {
	entry.canonical.once.Do(func() {
		entry.canonical.bytes, entry.canonical.err =
			canonicalMember(entry.key.name, entry.value.value)
	})
	return entry.canonical.bytes, entry.canonical.err
}

func canonicalFileFragment(entry *snapshotEntry[File]) ([]byte, error) {
	entry.canonical.once.Do(func() {
		file, err := canonicalSnapshotFileEntry(entry)
		if err != nil {
			entry.canonical.err = err
			return
		}
		if file.Seal != "" {
			file.Digest = ""
		}
		entry.canonical.bytes, entry.canonical.err = json.Marshal(file)
	})
	return entry.canonical.bytes, entry.canonical.err
}

// fileJSONFragment is canonicalFileFragment keeping a sealed file's digest,
// which the agent verifies its bytes against.
func fileJSONFragment(entry *snapshotEntry[File]) ([]byte, error) {
	file, err := canonicalSnapshotFileEntry(entry)
	if err != nil || file.Seal == "" {
		return canonicalFileFragment(entry)
	}
	return json.Marshal(file)
}

func canonicalMember[T any](name string, value T) ([]byte, error) {
	key, err := json.Marshal(name)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	member := make([]byte, 0, len(key)+1+len(encoded))
	member = append(member, key...)
	member = append(member, ':')
	return append(member, encoded...), nil
}
