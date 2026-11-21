// Copyright 2026 Philipp Hossner
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

package httpstore

import "time"

// Reversible acceptance supplements the authenticated publication protocol (ADR-0030).

// acceptance is one accepted version the gate has not confirmed yet.
type acceptance struct {
	sequence   uint64
	checksum   string
	descriptor SourceDescriptor
	revision   Revision
	previous   *acceptedVersion
}

// acceptedVersion is what a revocation restores. Nil means the URL had no
// accepted content before.
type acceptedVersion struct {
	content      string
	checksum     string
	acceptedTime time.Time
}

// RevokedContent describes one revoked acceptance.
type RevokedContent struct {
	URL      string
	Checksum string
	// Restored is true when the URL went back to the version it had before;
	// false when it has no accepted content again.
	Restored bool
	// Critical is the source's failure mode: a render without it fails.
	Critical bool
	// BlobDigest is the revoked blob of a blob source, else empty.
	BlobDigest string
}

// AcceptanceSequence counts acceptances so far. A render that reads the store
// after an acceptance sees that acceptance's content.
func (s *HTTPStore) AcceptanceSequence() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.acceptanceSequence
}

// ConfirmAcceptances makes only the exact versions read by a passing render permanent.
func (s *HTTPStore) ConfirmAcceptances(observations []ObservationToken) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range observations {
		observation := &observations[index]
		record := s.acceptances[observation.url]
		if record != nil && observation.Valid() && observation.source == s.revisionSource &&
			observation.found && observation.descriptor == record.descriptor &&
			observation.revision == record.revision {
			delete(s.acceptances, observation.url)
		}
	}
}

// RevokeAcceptances restores the confirmed predecessor of unconfirmed versions up to sequence.
func (s *HTTPStore) RevokeAcceptances(sequence uint64) []RevokedContent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publicationErrorLocked() != nil {
		return nil
	}
	var revoked []RevokedContent
	for url, record := range s.acceptances {
		if record.sequence > sequence {
			continue
		}
		delete(s.acceptances, url)
		entry, exists := s.cache[url]
		if !exists || entry.AcceptedChecksum != record.checksum || entry.sourceDescriptor != record.descriptor ||
			entry.acceptedRevision != record.revision {
			continue
		}
		revokedContent := entry.AcceptedContent
		s.restoreAcceptedLocked(url, entry, record.previous)
		revocation := RevokedContent{
			URL:      url,
			Checksum: record.checksum,
			Restored: record.previous != nil,
			Critical: entry.Options.Critical,
		}
		if entry.Options.Blob.Declared() {
			revocation.BlobDigest = blobDigest(revokedContent)
		}
		revoked = append(revoked, revocation)
	}
	return revoked
}

func (s *HTTPStore) restoreAcceptedLocked(url string, entry *CacheEntry, previous *acceptedVersion) {
	s.logger.Warn("Revoking accepted HTTP content refused by HAProxy",
		"url", RedactURL(url),
		"revoked_checksum", checksumPrefix(entry.AcceptedChecksum),
		"restored", previous != nil)
	entry.ETag = ""
	entry.LastModified = ""
	entry.mutationRevision++
	entry.replayRevision++
	revision := s.recordSemanticChangeLocked(url, entry.sourceDescriptor, entry.sourceDescriptor, false)
	if previous == nil {
		entry.AcceptedContent = ""
		entry.AcceptedChecksum = ""
		entry.AcceptedTime = time.Time{}
		entry.acceptedRevision = 0
		return
	}
	entry.AcceptedContent = previous.content
	entry.AcceptedChecksum = previous.checksum
	entry.AcceptedTime = previous.acceptedTime
	entry.acceptedRevision = revision
}

// Unconfirmed replacement chains retain the last confirmed predecessor.
func (s *HTTPStore) recordAcceptanceLocked(url string, entry, previous *CacheEntry) {
	if entry == nil || entry.fixture || entry.AcceptedChecksum == "" {
		return
	}
	if previous != nil && previous.AcceptedChecksum == entry.AcceptedChecksum && previous.sourceDescriptor == entry.sourceDescriptor {
		if earlier := s.acceptances[url]; earlier == nil || earlier.revision == entry.acceptedRevision {
			return
		}
	}
	s.acceptanceSequence++
	record := &acceptance{sequence: s.acceptanceSequence, checksum: entry.AcceptedChecksum,
		descriptor: entry.sourceDescriptor, revision: entry.acceptedRevision}
	if earlier, unconfirmed := s.acceptances[url]; unconfirmed && earlier.descriptor == entry.sourceDescriptor {
		record.previous = earlier.previous
	} else if previous != nil && previous.AcceptedChecksum != "" && previous.sourceDescriptor == entry.sourceDescriptor {
		record.previous = &acceptedVersion{
			content:      previous.AcceptedContent,
			checksum:     previous.AcceptedChecksum,
			acceptedTime: previous.AcceptedTime,
		}
	}
	if s.acceptances == nil {
		s.acceptances = make(map[string]*acceptance)
	}
	s.acceptances[url] = record
}

// recordPublishedAcceptancesLocked enters every version a committed
// publication accepted, comparing against the entries it replaced.
func (s *HTTPStore) recordPublishedAcceptancesLocked(replaced map[string]*CacheEntry, candidates []*InitialCandidate) {
	for _, candidate := range candidates {
		url := candidate.url
		s.recordAcceptanceLocked(url, s.cache[url], replaced[url])
	}
}
