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

package httpstore

import (
	"time"
)

// PromotePending promotes pending content to accepted for a URL.
// This should be called after successful validation.
func (s *HTTPStore) PromotePending(url string) bool {
	return s.finalizePending(url, "", 0, false, true, false)
}

// PromotePendingVersion promotes only the pending version that was validated.
func (s *HTTPStore) PromotePendingVersion(url, expectedChecksum string, expectedRevision uint64) bool {
	return s.finalizePending(url, expectedChecksum, expectedRevision, true, true, false)
}

// RejectPending discards pending content for a URL.
// This should be called when validation fails.
func (s *HTTPStore) RejectPending(url string) bool {
	return s.finalizePending(url, "", 0, false, false, false)
}

// RejectPendingVersion rejects only the pending version that was validated.
func (s *HTTPStore) RejectPendingVersion(url, expectedChecksum string, expectedRevision uint64) bool {
	return s.finalizePending(url, expectedChecksum, expectedRevision, true, false, false)
}

// DiscardPendingVersion discards one exact version without recording a validation failure.
func (s *HTTPStore) DiscardPendingVersion(url, expectedChecksum string, expectedRevision uint64) bool {
	return s.finalizePending(url, expectedChecksum, expectedRevision, true, false, true)
}

// finalizePending applies a verdict atomically and can require an exact pending version.
func (s *HTTPStore) finalizePending(
	url, expectedChecksum string,
	expectedRevision uint64,
	conditional, promote, quiet bool,
) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publicationErrorLocked() != nil {
		return false
	}

	entry, exists := s.cache[url]
	if !exists || !entry.HasPending || conditional &&
		(entry.PendingChecksum != expectedChecksum || entry.PendingRevision != expectedRevision) {
		return false
	}

	if promote {
		replaced := *entry
		defer s.recordAcceptanceLocked(url, entry, &replaced)
		s.logger.Debug("Promoting pending content to accepted",
			"url", RedactURL(url),
			"old_checksum", checksumPrefix(entry.AcceptedChecksum),
			"new_checksum", checksumPrefix(entry.PendingChecksum))
		contentChanged := entry.AcceptedChecksum == "" || entry.AcceptedContent != entry.PendingContent
		entry.AcceptedContent = entry.PendingContent
		entry.AcceptedChecksum = entry.PendingChecksum
		entry.AcceptedTime = time.Now()
		entry.ValidationState = StateAccepted
		if contentChanged {
			entry.acceptedRevision = s.recordSemanticChangeLocked(
				url,
				entry.sourceDescriptor,
				entry.sourceDescriptor,
				false,
			)
		}
	} else if quiet {
		s.logger.Debug("Discarding pending content from retired HTTP refresh",
			"url", RedactURL(url),
			"checksum", checksumPrefix(entry.PendingChecksum))
		entry.ValidationState = StateAccepted
	} else {
		s.logger.Warn("Rejecting pending content, keeping accepted version",
			"url", RedactURL(url),
			"rejected_checksum", checksumPrefix(entry.PendingChecksum),
			"keeping_checksum", checksumPrefix(entry.AcceptedChecksum))
		entry.ValidationState = StateRejected
	}

	if !promote {
		// Discarded validators must not hide the next validation attempt behind a 304.
		entry.ETag = ""
		entry.LastModified = ""
	}

	entry.PendingContent = ""
	entry.PendingChecksum = ""
	entry.PendingRevision = 0
	entry.HasPending = false
	entry.ValidationStartedAt = time.Time{}
	entry.mutationRevision++
	entry.replayRevision++
	s.recordReplayChangeLocked(url)
	s.recordActiveLeaseChangeLocked(url, entry.sourceDescriptor, entry.sourceDescriptor)

	return true
}

// checksumPrefix returns the first 16 characters of checksum followed by
// "..." for compact log output.
func checksumPrefix(checksum string) string {
	return checksum[:min(16, len(checksum))] + "..."
}

// GetPendingURLs returns all URLs with pending content awaiting validation.
func (s *HTTPStore) GetPendingURLs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.publicationErrorLocked() != nil {
		return nil
	}

	var urls []string
	for url, entry := range s.cache {
		if entry.HasPending {
			urls = append(urls, url)
		}
	}
	return urls
}

// EvictUnused removes cache entries that haven't been accessed within maxAge.
// Entries with pending validation are never evicted to protect the two-version cache.
// Returns the list of evicted URLs (empty if none evicted or eviction disabled).
//
// This method is called periodically by the event adapter to prevent unbounded
// memory growth when templates change and old URLs are no longer used.
func (s *HTTPStore) EvictUnused() []string {
	if s.maxAge == 0 {
		return nil // Eviction disabled
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publicationErrorLocked() != nil {
		return nil
	}

	now := time.Now()
	cutoff := now.Add(-s.maxAge)
	var evictedURLs []string

	for url, entry := range s.cache {
		if len(s.activeLeaseURLs[url]) != 0 {
			continue
		}
		// Never evict entries with pending validation
		if entry.HasPending {
			continue
		}

		// Evict if last access time is before cutoff
		if entry.LastAccessTime.Before(cutoff) {
			s.logger.Debug("Evicting unused HTTP cache entry",
				"url", RedactURL(url),
				"last_access", entry.LastAccessTime,
				"age", now.Sub(entry.LastAccessTime))
			delete(s.cache, url)
			delete(s.acceptances, url)
			s.recordSemanticChangeLocked(url, entry.sourceDescriptor, SourceDescriptor{}, true)
			evictedURLs = append(evictedURLs, url)
		}
	}

	if len(evictedURLs) > 0 {
		s.logger.Debug("HTTP store eviction complete",
			"evicted", len(evictedURLs),
			"remaining", len(s.cache))
	}

	return evictedURLs
}
