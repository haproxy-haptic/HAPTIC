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

package templating

import (
	"errors"
	"fmt"

	"github.com/haproxy-haptic/scriggo/native"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
)

// HTTPOptionBlob carries a blob.Spec through http.Fetch's options. Templates
// cannot build a blob.Spec, so only blob.Fetch declares blob sources.
const HTTPOptionBlob = "blob"

// defaultBlobTimeout replaces http.Fetch's 30 s default, which is too short
// for a download of DefaultMaxSize.
const defaultBlobTimeout = "5m"

// BlobHandle is what blob.Fetch returns. An empty Digest and Path mean no
// version is accepted yet.
type BlobHandle struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Path   string `json:"path"`
}

func newBlobPackage() native.Package {
	return native.Package{
		Name:         declBlob,
		Declarations: native.Declarations{memberFetch: scriggoBlobFetch},
	}
}

// scriggoBlobFetch declares a blob source through the render's http fetcher,
// so the source shares http.Fetch's refresh, acceptance, and render tracking.
// A fetch failure returns an empty handle; only an invalid declaration or the
// fetcher's own render errors return an error.
func scriggoBlobFetch(env native.Env, url string, options ...map[string]any) (BlobHandle, error) {
	if len(options) > 1 {
		return BlobHandle{}, fmt.Errorf("blob.Fetch takes a URL and one options map, got %d options maps", len(options))
	}
	var declared map[string]any
	if len(options) == 1 {
		declared = options[0]
	}
	httpOptions, err := blobHTTPOptions(declared)
	if err != nil {
		return BlobHandle{}, fmt.Errorf("blob.Fetch: %w", err)
	}
	ctx := env.Context()
	value, _ := lookupRenderContextValue(ctx, declHTTP)
	fetcher, _ := value.(HTTPFetcher)
	if fetcher == nil {
		return BlobHandle{}, nil
	}
	content, err := fetcher.Fetch(url, httpOptions)
	if err != nil {
		return BlobHandle{}, err
	}
	text, _ := content.(string)
	ref, ok := blob.DecodeRef(text)
	if !ok {
		return BlobHandle{}, nil
	}
	return BlobHandle{Digest: ref.Digest, Size: ref.Size, Path: ref.Path()}, nil
}

var blobOptionKeys = []string{httpOptionInterval, httpOptionTimeout, httpOptionRetries, "sha256", "unpack", "member", "maxSize"}

// blobHTTPOptions translates blob.Fetch options into the http.Fetch options of
// a non-critical blob source.
func blobHTTPOptions(options map[string]any) (map[string]any, error) {
	spec := blob.Spec{Unpack: blob.UnpackNone}
	httpOptions := map[string]any{httpOptionTimeout: defaultBlobTimeout}
	for key, value := range options {
		var err error
		switch key {
		case httpOptionInterval, httpOptionTimeout:
			httpOptions[key], err = blobStringOption(key, value)
		case httpOptionRetries:
			httpOptions[key], err = blobWholeNumberOption(key, value)
		case "sha256":
			spec.SHA256, err = blobStringOption(key, value)
		case "unpack":
			var unpack string
			unpack, err = blobStringOption(key, value)
			spec.Unpack = blob.Unpack(unpack)
		case "member":
			spec.Member, err = blobStringOption(key, value)
		case "maxSize":
			var size int
			size, err = blobWholeNumberOption(key, value)
			if err == nil && size <= 0 {
				err = fmt.Errorf("option %q must be positive, got %d", key, size)
			}
			spec.MaxSize = int64(size)
		default:
			err = fmt.Errorf("unknown option %q; use %v", key, blobOptionKeys)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	httpOptions[HTTPOptionBlob] = spec
	return httpOptions, nil
}

func blobStringOption(key string, value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("option %q must be a string, got %T", key, value)
	}
	return text, nil
}

func blobWholeNumberOption(key string, value any) (int, error) {
	if err := rejectIncrementalHTTPNativeType(value); err != nil {
		return 0, fmt.Errorf("option %q: %w", key, err)
	}
	scalar, err := deterministicScalarOf(value)
	if err != nil {
		return 0, fmt.Errorf("option %q: %w", key, err)
	}
	switch scalar.kind {
	case deterministicSignedScalar, deterministicUnsignedScalar:
	default:
		return 0, fmt.Errorf("option %q must be a whole number, got %T", key, value)
	}
	return deterministicScalarInt(scalar)
}

// canonicalIncrementalBlobSpec admits the spec blobHTTPOptions produced.
func canonicalIncrementalBlobSpec(value any) (blob.Spec, error) {
	spec, ok := value.(blob.Spec)
	if !ok || !spec.Declared() {
		return blob.Spec{}, errors.New(`http.Fetch: option "blob" is reserved for blob.Fetch`)
	}
	if err := spec.Validate(); err != nil {
		return blob.Spec{}, fmt.Errorf("http.Fetch: option %q: %w", HTTPOptionBlob, err)
	}
	return spec, nil
}
