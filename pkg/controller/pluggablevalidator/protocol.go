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

// Package pluggablevalidator implements the controller-side client for the
// pluggable-validator-sidecar wire protocol. The authoritative wire-protocol
// specification lives at docs/development/validator-protocol.md.
package pluggablevalidator

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// ProtocolVersion is the current wire-protocol major version. Bumped only on
// breaking changes per docs/development/validator-protocol.md "Versioning".
const ProtocolVersion = 1

// MaxFrameSize is the largest frame (length + payload) the client will encode
// or accept on the wire. Must match the validator's own limit — the hub's
// `DEFAULT_MAX_FRAME_BYTES`.
//
// 8 MiB rather than the original 1 MiB because a request now carries the data
// files a config references, and those are whole rule sets. Measured: the
// OWASP CRS the coraza plugin embeds (51 files, 713 KB on disk) JSON-encodes to
// 794 KB — 76% of 1 MiB before an operator adds a single custom rule, and the
// config file sharing the frame grows with the number of routes it describes.
// The old ceiling would have been reached in ordinary use, and every admission
// would fail identically until someone read the message.
const MaxFrameSize = 8 << 20 // 8 MiB

// Severity is the diagnostic severity in the wire format.
type Severity string

const (
	// SeverityWarning is a non-blocking finding.
	SeverityWarning Severity = "warning"
	// SeverityError blocks admission.
	SeverityError Severity = "error"
)

// Result is the aggregate outcome string returned in a Response.
type Result string

const (
	// ResultValid is "no errors and no warnings".
	ResultValid Result = "valid"
	// ResultWarning is "warnings but no errors".
	ResultWarning Result = "warning"
	// ResultError is "at least one error".
	ResultError Result = "error"
)

// File is one entry in a Request's files array.
//
// Path is an opaque identifier echoed back in diagnostics — the validator
// MUST NOT open it from disk. Content is the file's UTF-8 bytes (TOML text
// for hub configs).
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Kind tells the validator whether to validate this file or merely make
	// it available to whatever validates the others. Empty means
	// FileKindConfig and is omitted from the wire, so a request carrying no
	// data files is byte-identical to one from before this field existed.
	Kind FileKind `json:"kind,omitempty"`
}

// FileKind distinguishes a file to validate from one a validated file merely
// references.
type FileKind string

const (
	// FileKindConfig is a file the validator parses and checks. The default.
	FileKindConfig FileKind = ""

	// FileKindData is a file the validator must not parse, sent so that a
	// config referencing it can be checked. A WAF ruleset a hub config
	// `Include`s is the motivating case: the validator sidecar runs in the
	// controller pod and cannot read the HAProxy pod's filesystem, so
	// without the content travelling with the request there is nothing to
	// resolve the reference against.
	FileKindData FileKind = "data"
)

// dataFileFootprint reports how many data files a request carries and how many
// content bytes they account for.
func dataFileFootprint(files []File) (count, bytes int) {
	for _, f := range files {
		if f.Kind == FileKindData {
			count++
			bytes += len(f.Content)
		}
	}
	return count, bytes
}

// Request is the JSON payload the controller writes onto a validator socket.
type Request struct {
	ProtocolVersion int    `json:"protocol_version"`
	Files           []File `json:"files"`
	// StagedRoot is the directory the data files' paths are relative to, as
	// the process that will load them sees it at runtime.
	//
	// A validated config references its files by their runtime path
	// (`/etc/haproxy/general/crs-*.conf`) while the request carries them under
	// the controller's own identifiers (`general/crs-….conf`). The validator
	// cannot bridge the two on its own, and inferring the link by matching a
	// path suffix would resolve a mistyped directory just as readily as the
	// right one — so it is stated rather than guessed.
	//
	// Omitted when empty, so a request without data files is byte-identical to
	// one from before this field existed.
	StagedRoot string `json:"staged_root,omitempty"`
}

// Diagnostic is one finding in a Response.
//
// Line and Column are 1-based; 0 means "unknown / file-level". For
// protocol-level diagnostics (frame errors, version mismatch, missing
// fields), Path is the empty string.
type Diagnostic struct {
	Path    string `json:"path"`
	Line    uint32 `json:"line"`
	Column  uint32 `json:"column"`
	Message string `json:"message"`
}

// Response is the JSON payload a validator returns. Result is computed from
// the warning/error counts per the wire-protocol contract.
//
// The unexported `synthetic` field marks responses produced locally by
// ProtocolError after transport or protocol-decode failures. Conforming
// validator responses (parsed via DecodeResponse) leave it false. Callers use
// the marker to distinguish a validator verdict from a transport failure.
//
// `synthetic` is unexported so JSON encoders skip it — the wire format
// is unchanged.
type Response struct {
	ProtocolVersion int          `json:"protocol_version"`
	Result          Result       `json:"result"`
	Warnings        []Diagnostic `json:"warnings"`
	Errors          []Diagnostic `json:"errors"`

	synthetic bool
}

// IsSynthetic reports whether the response was produced by ProtocolError.
func (r *Response) IsSynthetic() bool {
	return r != nil && r.synthetic
}

// Protocol v1 identifies a server-side timeout by its prescribed diagnostic envelope.
func (r *Response) unavailable() bool {
	if r.IsSynthetic() {
		return true
	}
	if r.Result != ResultError || len(r.Warnings) != 0 || len(r.Errors) != 1 {
		return false
	}
	d := r.Errors[0]
	duration, ok := strings.CutPrefix(d.Message, "validation timed out after ")
	if !ok || d.Path != "" || d.Line != 0 || d.Column != 0 {
		return false
	}
	timeout, err := time.ParseDuration(duration)
	return err == nil && timeout > 0
}

// EncodeRequest serialises a Request into a length-prefixed JSON frame and
// writes it to w. Returns the number of bytes written (including the 4-byte
// length prefix) or an error.
//
// The caller-supplied request MUST already have a non-empty files array and
// the expected ProtocolVersion. EncodeRequest validates these invariants
// before writing — a partial write of a malformed request would leave the
// socket in an unusable state.
func EncodeRequest(w io.Writer, req *Request) (int, error) {
	if req == nil {
		return 0, fmt.Errorf("encode request: nil request")
	}
	if req.ProtocolVersion != ProtocolVersion {
		return 0, fmt.Errorf("encode request: unsupported protocol_version %d (want %d)", req.ProtocolVersion, ProtocolVersion)
	}
	if len(req.Files) == 0 {
		return 0, fmt.Errorf("encode request: files array must be non-empty")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return 0, fmt.Errorf("encode request: marshal JSON: %w", err)
	}
	if len(body) > MaxFrameSize {
		// Name the data files: they are the only part of a request that scales
		// with something other than the config being validated, so they are
		// what an operator has to act on. Without this the message reports a
		// byte count and leaves them to guess.
		dataCount, dataBytes := dataFileFootprint(req.Files)
		return 0, fmt.Errorf(
			"encode request: payload size %d exceeds MaxFrameSize %d "+
				"(%d data file(s) contributing %d bytes); reduce the validator's dataFiles globs "+
				"or raise the frame limit on both the controller and the validator",
			len(body), MaxFrameSize, dataCount, dataBytes)
	}

	header := make([]byte, 4)
	length, err := NarrowToUint32(len(body))
	if err != nil {
		return 0, fmt.Errorf("encode request: %w", err)
	}
	binary.BigEndian.PutUint32(header, length)

	n, err := w.Write(header)
	if err != nil {
		return n, fmt.Errorf("encode request: write length prefix: %w", err)
	}
	bn, err := w.Write(body)
	if err != nil {
		return n + bn, fmt.Errorf("encode request: write payload: %w", err)
	}
	return n + bn, nil
}

// DecodeResponse reads one length-prefixed JSON response frame from r and
// returns the parsed Response. The frame size MUST NOT exceed MaxFrameSize;
// oversized frames return an error without reading the body so the client
// can drop the connection.
func DecodeResponse(r io.Reader) (*Response, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("decode response: read length prefix: %w", err)
	}
	length := binary.BigEndian.Uint32(header)
	if length == 0 {
		return nil, fmt.Errorf("decode response: zero-length frame")
	}
	if length > MaxFrameSize {
		return nil, fmt.Errorf("decode response: frame size %d exceeds MaxFrameSize %d", length, MaxFrameSize)
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("decode response: read payload (%d bytes): %w", length, err)
	}

	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode response: unmarshal JSON: %w", err)
	}
	if resp.ProtocolVersion != ProtocolVersion {
		return nil, fmt.Errorf("decode response: unsupported protocol_version %d (want %d)", resp.ProtocolVersion, ProtocolVersion)
	}
	// Normalise nil slices so callers can iterate without nil checks. The
	// wire format guarantees the fields are always present (per spec) but
	// JSON decoders may leave nil for missing/null arrays.
	if resp.Warnings == nil {
		resp.Warnings = []Diagnostic{}
	}
	if resp.Errors == nil {
		resp.Errors = []Diagnostic{}
	}
	if err := validateResponseResult(&resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &resp, nil
}

func validateResponseResult(resp *Response) error {
	if resp.Result == "" {
		return errors.New(`missing result; use "valid", "warning", or "error"`)
	}
	if resp.Result != ResultValid && resp.Result != ResultWarning && resp.Result != ResultError {
		return fmt.Errorf(`unsupported result %q; use "valid", "warning", or "error"`, resp.Result)
	}

	expected := ResultValid
	if len(resp.Errors) > 0 {
		expected = ResultError
	} else if len(resp.Warnings) > 0 {
		expected = ResultWarning
	}
	if resp.Result != expected {
		return fmt.Errorf(
			"result %q does not match %d warning(s) and %d error(s); use %q",
			resp.Result, len(resp.Warnings), len(resp.Errors), expected,
		)
	}
	return nil
}

// NarrowToUint32 converts a non-negative int to uint32, returning an error
// if the value would overflow. The explicit bounds check lets static
// analyzers (gosec G115) prove the cast is safe — relying on caller-side
// invariants would silence the warning at the cost of correctness.
//
// Exported for test helpers in subpackages.
func NarrowToUint32(n int) (uint32, error) {
	if n < 0 {
		return 0, fmt.Errorf("negative length %d", n)
	}
	if uint64(n) > math.MaxUint32 {
		return 0, fmt.Errorf("length %d exceeds uint32 max", n)
	}
	return uint32(n), nil
}

// ProtocolError builds a synthetic error-severity Response carrying a single
// protocol-level Diagnostic with the given message. Used when the client
// needs to surface a frame-level failure (oversized frame, malformed JSON,
// connection refused) as a Response so callers don't have to switch on a
// separate error type.
//
// The returned Response is marked synthetic. Real validator responses with a
// `path: ""` diagnostic (file-level errors, plugin panics surfaced by the
// sidecar) are not synthetic.
func ProtocolError(message string) *Response {
	return &Response{
		ProtocolVersion: ProtocolVersion,
		Result:          ResultError,
		Warnings:        []Diagnostic{},
		Errors: []Diagnostic{
			{Path: "", Line: 0, Column: 0, Message: message},
		},
		synthetic: true,
	}
}
