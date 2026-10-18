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

package watcher

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
)

// TestWatcher_LastWatchError verifies the bulk watcher records a
// watch-connection error as a non-zero timestamp (kubernetes-resource-watching
// spec: "Bulk watcher surfaces watch errors") — e.g. a watched API version
// that stopped being served after an in-place CRD upgrade must not be
// invisible.
func TestWatcher_LastWatchError(t *testing.T) {
	w := &Watcher{
		config: types.WatcherConfig{},
		logger: slog.Default(),
	}

	require.True(t, w.LastWatchError().IsZero(), "no watch error yet → zero time")

	w.handleWatchError("", errors.New("the server could not find the requested resource"))

	require.False(t, w.LastWatchError().IsZero(), "after a watch error → non-zero timestamp")
}
