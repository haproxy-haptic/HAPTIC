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

package dataplane

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// checkKeyVersion changes whenever the key stops covering exactly what
// `haproxy -c` reads.
const checkKeyVersion = "haproxy-check-v1"

type checkKey [sha256.Size]byte

// CheckVerdicts remembers `haproxy -c` verdicts by a SHA-256 over the check's
// inputs: the binary, its arguments and environment, and the validation tree
// with the config and every file written into it. The check is deterministic
// over those, so an
// identical input set gets the answer HAProxy already gave instead of a second
// process. Only passes and refusals are remembered; a check that could not run
// is retried.
//
// DNS is the input the key cannot cover, so a strict-DNS check (no -dr) is
// never answered from here. See ADR-0035.
type CheckVerdicts struct {
	capacity int

	mu       sync.Mutex
	verdicts map[checkKey]checkVerdict
	order    []checkKey
}

// checkVerdict is HAProxy's answer: nil passed, otherwise its refusal.
type checkVerdict struct{ err error }

// NewCheckVerdicts returns a store holding the capacity most recently used
// verdicts (minimum 1).
func NewCheckVerdicts(capacity int) *CheckVerdicts {
	return &CheckVerdicts{capacity: max(capacity, 1), verdicts: make(map[checkKey]checkVerdict, capacity)}
}

func (v *CheckVerdicts) lookup(key checkKey) (checkVerdict, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	verdict, found := v.verdicts[key]
	if found {
		v.touchLocked(key)
	}
	return verdict, found
}

func (v *CheckVerdicts) remember(key checkKey, verdict checkVerdict) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, exists := v.verdicts[key]; exists {
		v.touchLocked(key)
	} else {
		v.order = append(v.order, key)
	}
	v.verdicts[key] = verdict
	for len(v.order) > v.capacity {
		delete(v.verdicts, v.order[0])
		v.order = v.order[1:]
	}
}

func (v *CheckVerdicts) touchLocked(key checkKey) {
	index := slices.Index(v.order, key)
	if index >= 0 && index < len(v.order)-1 {
		v.order = append(slices.Delete(v.order, index, index+1), key)
	}
}

// key identifies the check's input set. reusable is false when some input
// lies outside what the key covers: no -dr, a validation tree that is not
// fresh, or a file written outside it.
//
// The validation tree's own name is not an input: every path is keyed relative
// to it, and the config is keyed as the pieces between its occurrences.
func (v *CheckVerdicts) key(mainConfig string, auxFiles *AuxiliaryFiles, paths *ValidationPaths, skipDNSValidation bool) (key checkKey, reusable bool) {
	if v == nil || !skipDNSValidation || paths == nil || paths.TempDir == "" {
		return key, false
	}
	if entries, err := os.ReadDir(paths.TempDir); err != nil || len(entries) > 0 {
		return key, false
	}
	identity, err := HAProxyExecutorIdentity()
	if err != nil {
		return key, false
	}
	// The validation directories exist even when empty, so their layout is an input.
	layout := make([]string, 0, 5)
	for _, path := range []string{paths.ConfigFile, paths.MapsDir, paths.SSLCertsDir, paths.CRTListDir, paths.GeneralStorageDir} {
		relative, within := pathWithin(paths.TempDir, path)
		if !within {
			return key, false
		}
		layout = append(layout, relative)
	}

	h := keyHasher{sha256.New()}
	h.field(checkKeyVersion)
	h.field(identity)
	h.fields(checkArgs(skipDNSValidation, filepath.Base(paths.ConfigFile)))
	h.fields(os.Environ())
	h.fields(layout)
	h.fields(strings.Split(mainConfig, paths.TempDir))
	err = eachAuxiliaryFile(auxFiles, paths, func(resolved, content, _ string) error {
		relative, within := pathWithin(paths.TempDir, resolved)
		if !within {
			return errOutsideValidationTree
		}
		h.field(relative)
		h.field(content)
		return nil
	})
	if err != nil {
		return key, false
	}
	h.Sum(key[:0])
	return key, true
}

var errOutsideValidationTree = errors.New("auxiliary file outside the validation tree")

func pathWithin(base, target string) (string, bool) {
	if ensureContainedPath(base, target) != nil {
		return "", false
	}
	relative, err := filepath.Rel(base, target)
	return relative, err == nil
}

// keyHasher length-prefixes every field and counts every list, so no two
// field sequences hash the same byte stream.
type keyHasher struct{ hash.Hash }

func (h keyHasher) length(n int) {
	var encoded [binary.MaxVarintLen64]byte
	_, _ = h.Write(encoded[:binary.PutVarint(encoded[:], int64(n))])
}

func (h keyHasher) field(value string) {
	h.length(len(value))
	_, _ = io.WriteString(h, value)
}

func (h keyHasher) fields(values []string) {
	h.length(len(values))
	for _, value := range values {
		h.field(value)
	}
}
