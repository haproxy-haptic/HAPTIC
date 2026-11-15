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

// Package cli turns the agent's typed ops into HAProxy runtime commands and
// runs them against the worker stats socket. It knows command strings and
// success strings; it makes no decisions and parses no configuration.
package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

// ErrUnsafeToken is what every negative-space rejection wraps. A token that
// reaches HAProxy unchecked can split the command line and run a second
// command, so the check is a refusal, never a sanitisation.
var ErrUnsafeToken = errors.New("unsafe runtime token")

// validateToken accepts one command word: a name, a path, a map key, a keyword
// or a keyword argument. The verdict is api.SafeToken, the predicate the
// controller composes against, so the two ends cannot disagree.
func validateToken(field, s string) error {
	if s == "" {
		return fmt.Errorf("%w: %s is empty", ErrUnsafeToken, field)
	}
	if !api.SafeToken(s) {
		return fmt.Errorf("%w: %s contains whitespace, a control character or one of ;<>\\", ErrUnsafeToken, field)
	}
	return nil
}

// validatePayloadValue accepts a value that travels in a payload block, where
// only the line framing is significant (api.SafePayloadValue).
func validatePayloadValue(field, s string) error {
	if !api.SafePayloadValue(s) {
		return fmt.Errorf("%w: %s spans lines", ErrUnsafeToken, field)
	}
	return nil
}

// validatePayloadBlock refuses content that would end its own payload block.
// It is a refusal, never a rewrite: the bytes are a certificate or a map the
// controller composed, and the agent edits neither.
func validatePayloadBlock(payload string) error {
	for line := range strings.SplitSeq(payload, "\n") {
		if strings.TrimRight(line, "\r") == PayloadTerminator {
			return fmt.Errorf("%w: a payload line is the terminator %q", ErrUnsafeToken, PayloadTerminator)
		}
	}
	return nil
}

// validateEnum accepts one of a fixed set, so a typo cannot reach HAProxy as a
// silently different command.
func validateEnum(field, s string, allowed ...string) error {
	for _, a := range allowed {
		if s == a {
			return nil
		}
	}
	return fmt.Errorf("%w: %s %q is not one of %s", ErrUnsafeToken, field, s, strings.Join(allowed, "|"))
}
