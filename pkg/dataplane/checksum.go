package dataplane

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
)

// ComputeContentChecksum generates a SHA256 checksum covering the main HAProxy config
// and all auxiliary files (general files, map files, SSL certificates, CRT-list files).
//
// The checksum is used for content deduplication to skip redundant processing when
// config content hasn't changed. Auxiliary file slices must be pre-sorted (by
// AuxiliaryFiles.Sort()) to ensure deterministic results regardless of insertion order.
//
// Returns a hex-encoded 8-byte (16 character) checksum for brevity.
func ComputeContentChecksum(haproxyConfig string, auxFiles *AuxiliaryFiles) string {
	h := newContentChecksum(haproxyConfig)

	// Hash auxiliary files (slices are pre-sorted by AuxiliaryFiles.Sort)
	if auxFiles != nil {
		hashFileItems(h, auxFiles.GeneralFiles)
		hashFileItems(h, auxFiles.MapFiles)
		hashFileItems(h, auxFiles.SSLCertificates)
		hashFileItems(h, auxFiles.SSLCaFiles)
		hashFileItems(h, auxFiles.CRTListFiles)
		hashFileItems(h, auxFiles.SecretFiles)
	}

	return finishContentChecksum(h)
}

func newContentChecksum(haproxyConfig string) hash.Hash {
	h := sha256.New()
	_, _ = h.Write([]byte(haproxyConfig))
	return h
}

func finishContentChecksum(h hash.Hash) string {
	checksum := h.Sum(nil)
	return hex.EncodeToString(checksum[:8])
}

// hashFileItems writes each item's identifier and identity content to h in slice order.
// Slices must be pre-sorted by the caller (typically via AuxiliaryFiles.Sort)
// to keep results deterministic.
func hashFileItems[T auxiliaryfiles.FileItem](h hash.Hash, items []T) {
	for _, item := range items {
		h.Write([]byte(item.GetIdentifier()))
		h.Write([]byte(auxiliaryfiles.IdentityContent(item)))
	}
}
