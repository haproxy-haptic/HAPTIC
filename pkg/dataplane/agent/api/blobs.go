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

package api

import "fmt"

// BlobPath is the URL path of one blob, named by its lowercase hex SHA-256.
func BlobPath(sha256 string) string {
	return PathBlobs + "/" + sha256
}

// ValidBlobDigest reports whether s is a blob name: 64 lowercase hex digits.
func ValidBlobDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ValidateRequiredBlobs checks RequiredBlobs against the contract: bounded,
// valid names, no duplicates.
func (m *Manifest) ValidateRequiredBlobs() error {
	if len(m.RequiredBlobs) > MaxRequiredBlobs {
		return fmt.Errorf("%d required blobs exceed the %d-blob limit", len(m.RequiredBlobs), MaxRequiredBlobs)
	}
	seen := make(map[string]struct{}, len(m.RequiredBlobs))
	for _, digest := range m.RequiredBlobs {
		if !ValidBlobDigest(digest) {
			return fmt.Errorf("required blob %q is not a lowercase hex SHA-256", digest)
		}
		if _, duplicate := seen[digest]; duplicate {
			return fmt.Errorf("required blob %s appears twice in the manifest", digest)
		}
		seen[digest] = struct{}{}
	}
	return nil
}
