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

package configpublisher

import (
	"context"
	"errors"
	"log/slog"
)

// logAPIFailure logs a failed apiserver call at WARN, or at INFO when the
// component's own lifecycle ended and that cancellation is the only cause.
func (c *Component) logAPIFailure(lifecycle context.Context, msg string, err error, args ...any) {
	level := slog.LevelWarn
	if lifecycle.Err() != nil && onlyCanceled(err) {
		level = slog.LevelInfo
	}
	c.logger.Log(lifecycle, level, msg, append([]any{"error", err}, args...)...)
}

// onlyCanceled reports whether every leaf of err's tree is context.Canceled.
// errors.Is is not enough: it matches a joined error with one canceled branch.
func onlyCanceled(err error) bool {
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		causes := wrapped.Unwrap()
		for _, cause := range causes {
			if !onlyCanceled(cause) {
				return false
			}
		}
		return len(causes) > 0
	case interface{ Unwrap() error }:
		return onlyCanceled(wrapped.Unwrap())
	default:
		return errors.Is(err, context.Canceled)
	}
}
