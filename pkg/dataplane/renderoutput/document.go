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

package renderoutput

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/rendercontent"
)

type configDocumentMeasurement struct {
	bytes          int
	digest         [sha256.Size]byte
	contentHash    *boundedHashWriter
	sectionAligned bool
}

const checksumStreamBufferBytes = 4096

type boundedHashWriter struct {
	hash.Hash
	buffer [checksumStreamBufferBytes]byte
}

func (w *boundedHashWriter) WriteString(value string) (int, error) {
	total := 0
	for value != "" {
		length := copy(w.buffer[:], value)
		written, err := w.Write(w.buffer[:length])
		total += written
		if err != nil {
			return total, err
		}
		if written != length {
			return total, io.ErrShortWrite
		}
		value = value[length:]
	}
	return total, nil
}

var _ io.StringWriter = (*boundedHashWriter)(nil)

func configDocumentFromString(value string) (rendercontent.Document, error) {
	var builder rendercontent.DocumentBuilder
	if _, err := builder.WriteString(value); err != nil {
		return rendercontent.Document{}, err
	}
	return builder.Build(nil)
}

func validateOutputDocumentBindings(
	document rendercontent.Document,
	sections []renderplan.Section,
	files map[string]*renderplan.File,
	artifacts *renderartifact.Snapshot,
) (int, configDocumentMeasurement, []documentChecksumItem, error) {
	measurement, err := validateConfigDocument(document, sections, files)
	if err != nil {
		return 0, configDocumentMeasurement{}, nil, err
	}
	items := make([]documentChecksumItem, 0, len(files)-1)
	artifactCount, err := validateAuxiliaryBindingsWith(
		len(files)-1, files, artifacts,
		func(descriptor renderartifact.Descriptor, content *renderartifact.Content) error {
			order, identifier, identityErr := documentChecksumIdentity(descriptor)
			if identityErr != nil {
				return identityErr
			}
			items = append(items, documentChecksumItem{
				familyOrder: order,
				identifier:  identifier,
				family:      descriptor.Family,
				path:        descriptor.Path,
				content:     content,
				seal:        descriptor.Seal,
			})
			return nil
		},
	)
	if err != nil {
		return 0, configDocumentMeasurement{}, nil, err
	}
	return artifactCount, measurement, items, nil
}

func validateConfigDocument(
	document rendercontent.Document,
	sections []renderplan.Section,
	files map[string]*renderplan.File,
) (configDocumentMeasurement, error) {
	if err := document.ValidateAuthentication(); err != nil {
		return configDocumentMeasurement{}, errors.Join(errInvalidSnapshot, err)
	}
	configFile, err := exactConfigFile(files)
	if err != nil {
		return configDocumentMeasurement{}, err
	}
	length, err := document.Bytes()
	if err != nil {
		return configDocumentMeasurement{}, errors.Join(errInvalidSnapshot, err)
	}
	if configFile.Size != int64(length) {
		return configDocumentMeasurement{}, fmt.Errorf(
			"render output config file %q does not match the rendered config",
			configFile.Path,
		)
	}
	writer := &configDocumentValidationWriter{
		config:   exactStringWriter{expected: configFile.Content},
		sections: sections,
		digest:   &boundedHashWriter{Hash: sha256.New()},
	}
	written, err := document.WriteTo(writer)
	if errors.Is(err, errOutputContentMismatch) {
		return configDocumentMeasurement{}, errors.New("render output document differs from its plan")
	}
	if err != nil {
		return configDocumentMeasurement{}, errors.Join(errInvalidSnapshot, err)
	}
	if written != int64(length) || writer.config.offset != len(configFile.Content) {
		return configDocumentMeasurement{}, errors.New("render output document differs from its plan")
	}
	if err := writer.finish(length); err != nil {
		return configDocumentMeasurement{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], writer.digest.Sum(nil))
	sectionAligned, err := configDocumentSectionAligned(document, sections)
	if err != nil {
		return configDocumentMeasurement{}, err
	}
	return configDocumentMeasurement{
		bytes: length, digest: digest, contentHash: writer.digest,
		sectionAligned: sectionAligned,
	}, nil
}

func configDocumentSectionAligned(
	document rendercontent.Document,
	sections []renderplan.Section,
) (bool, error) {
	leaves, err := document.Leaves()
	if err != nil {
		return false, errors.Join(errInvalidSnapshot, err)
	}
	if leaves != len(sections) {
		return false, nil
	}
	for index := range sections {
		if sections[index].Length == 0 {
			return false, nil
		}
		bytes, err := document.LeafBytes(index)
		if err != nil {
			return false, errors.Join(errInvalidSnapshot, err)
		}
		if bytes != sections[index].Length {
			return false, nil
		}
	}
	return true, nil
}

func exactConfigFile(files map[string]*renderplan.File) (*renderplan.File, error) {
	var config *renderplan.File
	count := 0
	for path, file := range files {
		if file.Kind != renderplan.FileKindConfig {
			continue
		}
		count++
		if path != renderplan.ConfigFilePath || !file.ReloadOnChange {
			return nil, fmt.Errorf("render output config file %q does not match the rendered config", path)
		}
		config = file
	}
	if count != 1 {
		return nil, fmt.Errorf("render output plan has %d config files, want exactly one", count)
	}
	return config, nil
}

type configDocumentValidationWriter struct {
	config        exactStringWriter
	sections      []renderplan.Section
	sectionIndex  int
	sectionOffset int
	digest        *boundedHashWriter
	bytes         int
}

func (w *configDocumentValidationWriter) Write(value []byte) (int, error) {
	if _, err := w.config.Write(value); err != nil {
		return 0, err
	}
	if err := w.writeSectionBytes(value); err != nil {
		return 0, err
	}
	written, err := w.digest.Write(value)
	if err != nil {
		return 0, err
	}
	if written != len(value) {
		return written, io.ErrShortWrite
	}
	w.bytes += written
	return written, nil
}

func (w *configDocumentValidationWriter) WriteString(value string) (int, error) {
	if _, err := w.config.WriteString(value); err != nil {
		return 0, err
	}
	if err := w.writeSectionString(value); err != nil {
		return 0, err
	}
	written, err := w.digest.WriteString(value)
	if err != nil {
		return 0, err
	}
	if written != len(value) {
		return written, io.ErrShortWrite
	}
	w.bytes += written
	return written, nil
}

func (w *configDocumentValidationWriter) writeSectionBytes(value []byte) error {
	for len(value) > 0 {
		expected, length, err := w.nextSectionChunk(len(value))
		if err != nil {
			return err
		}
		for index := range length {
			if value[index] != expected[index] {
				return errOutputContentMismatch
			}
		}
		w.advanceSection(length)
		value = value[length:]
	}
	return nil
}

func (w *configDocumentValidationWriter) writeSectionString(value string) error {
	for value != "" {
		expected, length, err := w.nextSectionChunk(len(value))
		if err != nil {
			return err
		}
		if value[:length] != expected[:length] {
			return errOutputContentMismatch
		}
		w.advanceSection(length)
		value = value[length:]
	}
	return nil
}

func (w *configDocumentValidationWriter) nextSectionChunk(
	available int,
) (expected string, length int, err error) {
	for w.sectionIndex < len(w.sections) && w.sectionOffset == w.sections[w.sectionIndex].Length {
		w.sectionIndex++
		w.sectionOffset = 0
	}
	if w.sectionIndex >= len(w.sections) {
		return "", 0, errOutputContentMismatch
	}
	section := &w.sections[w.sectionIndex]
	remaining := section.Length - w.sectionOffset
	if remaining < 0 || remaining > len(section.Text)-w.sectionOffset {
		return "", 0, errOutputContentMismatch
	}
	return section.Text[w.sectionOffset:], min(available, remaining), nil
}

func (w *configDocumentValidationWriter) advanceSection(length int) {
	w.sectionOffset += length
	if w.sectionOffset == w.sections[w.sectionIndex].Length {
		w.sectionIndex++
		w.sectionOffset = 0
	}
}

func (w *configDocumentValidationWriter) finish(length int) error {
	for w.sectionIndex < len(w.sections) && w.sections[w.sectionIndex].Length == 0 {
		w.sectionIndex++
	}
	if w.sectionIndex != len(w.sections) || w.sectionOffset != 0 || w.bytes != length {
		return fmt.Errorf("render output sections cover %d of %d config bytes", w.bytes, length)
	}
	return nil
}

var _ io.StringWriter = (*configDocumentValidationWriter)(nil)

type documentChunkCollector struct {
	values []string
}

func (w *documentChunkCollector) Write(value []byte) (int, error) {
	if len(value) != 0 {
		w.values = append(w.values, string(value))
	}
	return len(value), nil
}

func (w *documentChunkCollector) WriteString(value string) (int, error) {
	if value != "" {
		w.values = append(w.values, value)
	}
	return len(value), nil
}

var _ io.StringWriter = (*documentChunkCollector)(nil)

type documentChunkComparator struct {
	values []string
	index  int
	offset int
}

func (w *documentChunkComparator) Write(value []byte) (int, error) {
	total := 0
	for len(value) > 0 {
		expected, length, ok := w.next(len(value))
		if !ok {
			return total, errOutputContentMismatch
		}
		for index := range length {
			if value[index] != expected[index] {
				return total, errOutputContentMismatch
			}
		}
		w.advance(length)
		total += length
		value = value[length:]
	}
	return total, nil
}

func (w *documentChunkComparator) WriteString(value string) (int, error) {
	total := 0
	for value != "" {
		expected, length, ok := w.next(len(value))
		if !ok || value[:length] != expected[:length] {
			return total, errOutputContentMismatch
		}
		w.advance(length)
		total += length
		value = value[length:]
	}
	return total, nil
}

func (w *documentChunkComparator) next(
	available int,
) (expected string, length int, found bool) {
	for w.index < len(w.values) && w.offset == len(w.values[w.index]) {
		w.index++
		w.offset = 0
	}
	if w.index >= len(w.values) {
		return "", 0, false
	}
	remaining := w.values[w.index][w.offset:]
	return remaining, min(available, len(remaining)), true
}

func (w *documentChunkComparator) advance(length int) {
	w.offset += length
	if w.offset == len(w.values[w.index]) {
		w.index++
		w.offset = 0
	}
}

func (w *documentChunkComparator) complete() bool {
	for w.index < len(w.values) && w.offset == len(w.values[w.index]) {
		w.index++
		w.offset = 0
	}
	return w.index == len(w.values) && w.offset == 0
}

var _ io.StringWriter = (*documentChunkComparator)(nil)

func exactDocumentEqual(left, right rendercontent.Document) (bool, error) {
	same, err := left.SameRoot(right)
	if err != nil || same {
		return same, err
	}
	leftBytes, err := left.Bytes()
	if err != nil {
		return false, err
	}
	rightBytes, err := right.Bytes()
	if err != nil || leftBytes != rightBytes {
		return false, err
	}
	leaves, err := left.Leaves()
	if err != nil {
		return false, err
	}
	collector := &documentChunkCollector{values: make([]string, 0, leaves)}
	written, err := left.WriteTo(collector)
	if err != nil {
		return false, err
	}
	if written != int64(leftBytes) {
		return false, errInvalidSnapshot
	}
	comparator := &documentChunkComparator{values: collector.values}
	written, err = right.WriteTo(comparator)
	if errors.Is(err, errOutputContentMismatch) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return written == int64(rightBytes) && comparator.complete(), nil
}

type documentChecksumItem struct {
	familyOrder int
	identifier  string
	family      renderartifact.Family
	path        string
	content     *renderartifact.Content
	seal        string
}

func computeDocumentSnapshotContentChecksum(
	configHash *boundedHashWriter,
	items []documentChecksumItem,
) (string, error) {
	if configHash == nil {
		return "", errInvalidSnapshot
	}
	slices.SortFunc(items, func(left, right documentChecksumItem) int {
		if order := cmp.Compare(left.familyOrder, right.familyOrder); order != 0 {
			return order
		}
		if order := strings.Compare(left.identifier, right.identifier); order != 0 {
			return order
		}
		if order := cmp.Compare(left.family, right.family); order != 0 {
			return order
		}
		return strings.Compare(left.path, right.path)
	})
	for _, item := range items {
		if _, err := configHash.WriteString(item.identifier); err != nil {
			return "", err
		}
		if item.family == renderartifact.Secret {
			if _, err := configHash.WriteString(item.seal); err != nil {
				return "", err
			}
			continue
		}
		if _, err := item.content.WriteTo(configHash); err != nil {
			return "", err
		}
	}
	sum := configHash.Sum(nil)
	return hex.EncodeToString(sum[:8]), nil
}

// PreparedContentHash is the document's share of a content checksum, hashed
// ahead of the snapshot that will carry it so the hashing overlaps the plan
// work that precedes the snapshot. It serves one snapshot whose document is
// the one it hashed.
type PreparedContentHash struct {
	document rendercontent.Document
	hasher   *boundedHashWriter
	written  int64
	err      error
}

// PrepareContentHash hashes the document part of the content checksum.
func PrepareContentHash(document rendercontent.Document) *PreparedContentHash {
	hasher := &boundedHashWriter{Hash: sha256.New()}
	written, err := document.WriteTo(hasher)
	return &PreparedContentHash{document: document, hasher: hasher, written: written, err: err}
}

func (p *PreparedContentHash) matches(document rendercontent.Document) bool {
	if p == nil || p.err != nil || p.hasher == nil {
		return false
	}
	same, err := p.document.SameRoot(document)
	return err == nil && same
}

func computeSnapshotContentChecksum(
	document rendercontent.Document,
	artifacts *renderartifact.Snapshot,
	prepared *PreparedContentHash,
) (string, error) {
	var hasher *boundedHashWriter
	var written int64
	if prepared.matches(document) {
		hasher, written = prepared.hasher, prepared.written
		prepared.hasher = nil
	} else {
		hasher = &boundedHashWriter{Hash: sha256.New()}
		var err error
		if written, err = document.WriteTo(hasher); err != nil {
			return "", err
		}
	}
	bytes, err := document.Bytes()
	if err != nil || written != int64(bytes) {
		if err != nil {
			return "", err
		}
		return "", errInvalidSnapshot
	}
	items := make([]documentChecksumItem, 0)
	err = artifacts.Walk(func(artifact *renderartifact.Artifact) error {
		descriptor, err := artifact.Descriptor()
		if err != nil {
			return err
		}
		content, err := artifact.Content()
		if err != nil {
			return err
		}
		order, identifier, err := documentChecksumIdentity(descriptor)
		if err != nil {
			return err
		}
		items = append(items, documentChecksumItem{
			familyOrder: order, identifier: identifier, family: descriptor.Family,
			path: descriptor.Path, content: content, seal: descriptor.Seal,
		})
		return nil
	})
	if err != nil {
		return "", err
	}
	return computeDocumentSnapshotContentChecksum(hasher, items)
}

func documentChecksumIdentity(
	descriptor renderartifact.Descriptor,
) (familyOrder int, identifier string, err error) {
	switch descriptor.Family {
	case renderartifact.General, renderartifact.GeneralCA:
		return 0, descriptor.Name, nil
	case renderartifact.Map:
		return 1, descriptor.Path, nil
	case renderartifact.Certificate:
		return 2, descriptor.Path, nil
	case renderartifact.CA:
		return 3, descriptor.Path, nil
	case renderartifact.Secret:
		return 5, descriptor.Path, nil
	case renderartifact.CRTList:
		return 4, descriptor.Path, nil
	default:
		return 0, "", fmt.Errorf("checksumming auxiliary file: invalid family %d", descriptor.Family)
	}
}
