// Package auxiliaryfiles holds the file types a render produces next to
// haproxy.cfg: general files (error pages, ACL files), SSL certificates, map
// files, crt-lists, CA files and secret files. The renderer builds them, the
// plan describes them and the agent writes them.
package auxiliaryfiles

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// FileItem is one auxiliary file, whatever its kind.
type FileItem interface {
	// GetIdentifier returns the path or filename the file is stored under.
	GetIdentifier() string

	// GetContent returns the file content.
	GetContent() string
}

// GeneralFile represents a general-purpose file (error files, custom response files, etc.).
// The agent writes these files onto the pod and the configuration references them in the
// HAProxy configuration (e.g., in http-errors sections).
type GeneralFile struct {
	// Filename is the base file name (used as API 'id').
	// Example: "400.http"
	Filename string

	// Path is the absolute file path where the file is stored.
	// This is computed from the configured GeneralStorageDir + Filename.
	// Example: "/etc/haproxy/general/400.http"
	Path string

	// Content is the file contents as a string. This maps to the 'file' field in
	// multipart parts of an apply.
	// json:"-" on every aux Content keeps key material out of /debug/vars; the
	// tls-ticket-keys STEK file is a general file, so this is not SSL-only.
	Content string `json:"-"`

	// IsCaFile marks this general file as an SSL CA / trust bundle referenced by
	// the config as `ca-file <path>` (frontend client-cert verify or backend mTLS
	// server verify). When set, a CONTENT-only update can be applied to the live
	// worker via the runtime API (`add ssl ca-file` + commit, which replaces the
	// file with the payload) without a reload — it is what makes the plan file
	// kind `ca` rather than `general`. Metadata only: GetContent (used for
	// diffing) ignores it, so it never causes a spurious diff against the
	// content-keyed current state.
	IsCaFile bool

	// ReloadOnPush carries the CRD's files[].reloadOnPush (or the 4th argument
	// to fileRegistry.Register). Nil — the zero value — means true, so a file
	// built without thinking about this flag keeps reloading. Read it through
	// ReloadsOnPush. Metadata only: GetContent ignores it, so it never causes a
	// spurious diff against the content-keyed current state.
	ReloadOnPush *bool
}

// ReloadsOnPush reports whether pushing this file's content must reload HAProxy.
// False only for a sidecar-owned file the config never references.
func (g GeneralFile) ReloadsOnPush() bool {
	return g.ReloadOnPush == nil || *g.ReloadOnPush
}

// GetIdentifier implements the FileItem interface.
func (g GeneralFile) GetIdentifier() string {
	return g.Filename
}

// GetContent implements the FileItem interface.
func (g GeneralFile) GetContent() string {
	return g.Content
}

// SSLCertificate represents an SSL/TLS certificate file containing certificates and keys.
// These files are used for HTTPS termination and client certificate authentication.
type SSLCertificate struct {
	// Path is the absolute file path to the certificate.
	// Example: "/etc/haproxy/certs/example.com.pem"
	Path string

	// Content is the PEM-encoded certificate and key data.
	Content string `json:"-"`
}

// GetIdentifier implements the FileItem interface.
func (s SSLCertificate) GetIdentifier() string {
	return s.Path
}

// GetContent implements the FileItem interface.
func (s SSLCertificate) GetContent() string {
	return s.Content
}

// MapFile represents a HAProxy map file for dynamic key-value lookups.
// Map files enable runtime configuration changes without reloading HAProxy.
type MapFile struct {
	// Path is the absolute file path to the map file.
	// Example: "/etc/haproxy/maps/domains.map"
	Path string

	// Content is the map file contents (one key-value pair per line).
	Content string `json:"-"`
}

// GetIdentifier implements the FileItem interface.
func (m MapFile) GetIdentifier() string {
	return m.Path
}

// GetContent implements the FileItem interface.
func (m MapFile) GetContent() string {
	return m.Content
}

// CRTListFile represents a HAProxy crt-list file for SSL certificate lists with per-certificate options.
// CRT-list files allow specifying multiple certificates with individual SSL options and SNI filters.
type CRTListFile struct {
	// Path is the absolute file path to the crt-list file.
	// Example: "/etc/haproxy/crt-lists/crt-list.txt"
	Path string

	// Content is the crt-list file contents (one certificate entry per line).
	// Format: <cert-path> [ssl-options] [sni-filter]
	// Example: "/etc/haproxy/ssl/cert.pem [ocsp-update on] example.com"
	Content string `json:"-"`
}

// GetIdentifier implements the FileItem interface.
func (c CRTListFile) GetIdentifier() string {
	return c.Path
}

// GetContent implements the FileItem interface.
func (c CRTListFile) GetContent() string {
	return c.Content
}

// SSLCaFile represents an SSL CA certificate file containing trusted CA certificates.
// These files are used for client certificate verification and SSL chain validation.
// A content-only update reaches the running worker over the runtime API.
type SSLCaFile struct {
	// Path is the file path or name of the CA file.
	// Example: "ca-bundle.pem" or "/etc/haproxy/ssl/ca/trusted-cas.pem"
	Path string

	// Content is the PEM-encoded CA certificate data.
	// Can contain multiple CA certificates concatenated together.
	Content string `json:"-"`
}

// GetIdentifier implements the FileItem interface.
func (s SSLCaFile) GetIdentifier() string {
	return s.Path
}

// GetContent implements the FileItem interface.
func (s SSLCaFile) GetContent() string {
	return s.Content
}

// SecretFile is a file for a sidecar that HAProxy never reads. It is published
// as a Kubernetes Secret and written by the agent without a reload.
type SecretFile struct {
	// Path is the plan path, e.g. "secrets/sso.client-secret".
	Path string

	Content string `json:"-"`

	// Seal is SealContent of Content. Every identity a reader without Secret
	// access can see hashes the seal, never the content.
	Seal string
}

// GetIdentifier implements the FileItem interface.
func (s SecretFile) GetIdentifier() string {
	return s.Path
}

// GetContent implements the FileItem interface.
func (s SecretFile) GetContent() string {
	return s.Content
}

// SealContent is a keyed digest of content: an unkeyed one would let anyone who
// reads it test guesses of a low-entropy secret offline.
func SealContent(key []byte, content string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(content))
	return hex.EncodeToString(mac.Sum(nil))
}

// IdentityContent is what content identities hash for file: its seal for a
// SecretFile, its content otherwise.
func IdentityContent(file FileItem) string {
	if secret, ok := file.(SecretFile); ok {
		return secret.Seal
	}
	return file.GetContent()
}
