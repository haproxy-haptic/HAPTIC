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

package events

// BlobContentRevokedEvent is published when an accepted blob was taken back
// because HAProxy refused a render containing it (ADR-0030, ADR-0041).
type BlobContentRevokedEvent struct {
	URL      string // The blob source URL (credentials redacted)
	Digest   string // SHA-256 of the revoked blob
	Restored bool   // The source went back to its previous accepted blob
	timestamped
}

// NewBlobContentRevokedEvent creates a new BlobContentRevokedEvent.
func NewBlobContentRevokedEvent(url, digest string, restored bool) *BlobContentRevokedEvent {
	return &BlobContentRevokedEvent{URL: url, Digest: digest, Restored: restored, timestamped: newTimestamped()}
}

func (e *BlobContentRevokedEvent) EventType() string { return EventTypeBlobContentRevoked }

// BlobFetchFailedEvent is published when a blob source could not be fetched
// after its retries; the source keeps its accepted blob.
type BlobFetchFailedEvent struct {
	URL   string // The blob source URL (credentials redacted)
	Error string
	timestamped
}

// NewBlobFetchFailedEvent creates a new BlobFetchFailedEvent.
func NewBlobFetchFailedEvent(url, err string) *BlobFetchFailedEvent {
	return &BlobFetchFailedEvent{URL: url, Error: err, timestamped: newTimestamped()}
}

func (e *BlobFetchFailedEvent) EventType() string { return EventTypeBlobFetchFailed }
