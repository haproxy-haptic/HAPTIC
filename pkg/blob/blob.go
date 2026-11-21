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

// Package blob defines content-addressed binary blobs (ADR-0041): their
// declaration, their reference, and the path the data plane sees them at.
// It depends on the standard library only, so the template engine, the stores,
// and the agent can share it.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// PathPrefix is the directory the data plane reads blobs from: the agent
// mounts its blob volume here, and a handle's path is PathPrefix + digest.
const PathPrefix = "/var/lib/haptic/blobs/"

// DefaultMaxSize bounds a blob whose declaration names no maxSize.
const DefaultMaxSize int64 = 256 << 20

// digestLength is the length of a hex-encoded SHA-256 digest.
const digestLength = 2 * sha256.Size

// ErrRefused marks content that a retry cannot fix: it is too large, does not
// match its pinned digest, or lacks its archive member.
var ErrRefused = errors.New("blob refused")

// Unpack selects how a response body becomes the blob.
type Unpack string

const (
	// UnpackNone keeps the body as it is.
	UnpackNone Unpack = "none"
	// UnpackGzip decompresses a gzip body.
	UnpackGzip Unpack = "gzip"
	// UnpackTarGz keeps one member of a gzip-compressed tar archive.
	UnpackTarGz Unpack = "tar.gz"
)

// Spec declares how a fetched body becomes a blob. It is comparable, so it can
// be part of a source declaration. The zero value declares no blob.
type Spec struct {
	Unpack Unpack
	// Member is the archive path of the file UnpackTarGz keeps.
	Member string
	// SHA256 pins the blob's digest; any other content is refused.
	SHA256 string
	// MaxSize bounds the unpacked bytes; zero means DefaultMaxSize.
	MaxSize int64
}

// Declared reports whether the spec declares a blob.
func (s Spec) Declared() bool {
	return s.Unpack != ""
}

// Validate refuses a spec a store cannot honour.
func (s Spec) Validate() error {
	switch s.Unpack {
	case UnpackNone, UnpackGzip:
		if s.Member != "" {
			return fmt.Errorf("member %q needs unpack %q", s.Member, UnpackTarGz)
		}
	case UnpackTarGz:
		if CleanMember(s.Member) == "" {
			return fmt.Errorf("unpack %q needs a member", UnpackTarGz)
		}
	default:
		return fmt.Errorf("unpack %q is unknown; use %q, %q, or %q", s.Unpack, UnpackNone, UnpackGzip, UnpackTarGz)
	}
	if s.SHA256 != "" && !ValidDigest(s.SHA256) {
		return fmt.Errorf("sha256 %q is not 64 lowercase hex digits", s.SHA256)
	}
	if s.MaxSize < 0 {
		return fmt.Errorf("maxSize %d is negative", s.MaxSize)
	}
	return nil
}

// Key is a Spec in one comparable string, so a declaration that carries it
// stays small. The empty Key declares no blob.
type Key string

// Key returns the spec's Key.
func (s Spec) Key() Key {
	if !s.Declared() {
		return ""
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		panic("blob: a Spec of strings and an integer always encodes: " + err.Error())
	}
	return Key(encoded)
}

// Declared reports whether the key declares a blob.
func (k Key) Declared() bool {
	return k != ""
}

// Spec decodes the key and validates the spec.
func (k Key) Spec() (Spec, error) {
	var spec Spec
	if err := json.Unmarshal([]byte(k), &spec); err != nil {
		return Spec{}, fmt.Errorf("blob declaration %q is invalid: %w", string(k), err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// CleanMember normalises an archive path so "./db.mmdb" and "db.mmdb" match.
func CleanMember(member string) string {
	for {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(member, "./"), "/")
		if trimmed == member {
			return member
		}
		member = trimmed
	}
}

// Ref identifies one blob version.
type Ref struct {
	Digest string
	Size   int64
}

// Describe returns the reference of data.
func Describe(data []byte) Ref {
	sum := sha256.Sum256(data)
	return Ref{Digest: hex.EncodeToString(sum[:]), Size: int64(len(data))}
}

// Path is where the data plane reads the blob.
func (r Ref) Path() string {
	return PathPrefix + r.Digest
}

// Encode returns the text a source holds for this version. DecodeRef reverses it.
func (r Ref) Encode() string {
	return r.Digest + " " + strconv.FormatInt(r.Size, 10)
}

// DecodeRef parses Encode's output; anything else is not a reference.
func DecodeRef(content string) (Ref, bool) {
	digest, size, found := strings.Cut(content, " ")
	if !found || !ValidDigest(digest) {
		return Ref{}, false
	}
	n, err := strconv.ParseInt(size, 10, 64)
	if err != nil || n < 0 {
		return Ref{}, false
	}
	return Ref{Digest: digest, Size: n}, true
}

// ValidDigest reports whether digest is a lowercase hex SHA-256 digest.
func ValidDigest(digest string) bool {
	if len(digest) != digestLength {
		return false
	}
	for i := range len(digest) {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ReferencedDigests returns the sorted digests whose data-plane path occurs in
// any of texts: the blobs a rendered configuration needs on its pods.
func ReferencedDigests(texts ...string) []string {
	var digests []string
	for _, text := range texts {
		for {
			index := strings.Index(text, PathPrefix)
			if index < 0 {
				break
			}
			text = text[index+len(PathPrefix):]
			if len(text) >= digestLength && ValidDigest(text[:digestLength]) &&
				(len(text) == digestLength || !isHexDigit(text[digestLength])) {
				digests = append(digests, text[:digestLength])
			}
		}
	}
	slices.Sort(digests)
	return slices.Compact(digests)
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}
