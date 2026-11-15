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

package deployplan_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

const sensitiveMap = "maps/hmac-keys.map"

func sensitiveMapFile(content string) planOpt {
	return withFile(&renderplan.File{
		Path: sensitiveMap, Kind: renderplan.FileKindMap, Digest: renderplan.DigestString(content),
		Size: int64(len(content)), Content: content, ContentKnown: true, Seal: "seal-of-" + content,
	})
}

// A sensitive map carries no entries in the plan, so a change to it is only
// ever a whole replace: per-entry ops would expose its keys and values.
func TestDiffSensitiveMap(t *testing.T) {
	tests := []struct {
		name   string
		prev   *renderplan.Plan
		next   *renderplan.Plan
		loaded bool
		want   deployplan.Verdict
		ops    []api.Op
	}{
		{
			name:   "changed and loaded is replaced whole",
			prev:   basePlan(sensitiveMapFile("route-a key1\n")),
			next:   basePlan(sensitiveMapFile("route-a key2\n")),
			loaded: true,
			want:   deployplan.VerdictRuntime,
			ops:    []api.Op{{Kind: api.OpMapReplace, Path: sensitiveMap}},
		},
		{
			name:   "unchanged emits nothing",
			prev:   basePlan(sensitiveMapFile("route-a key1\n")),
			next:   basePlan(sensitiveMapFile("route-a key1\n")),
			loaded: true,
			want:   deployplan.VerdictFileOnly,
		},
		{
			name: "changed but not loaded is written only",
			prev: basePlan(sensitiveMapFile("route-a key1\n")),
			next: basePlan(sensitiveMapFile("route-a key2\n")),
			want: deployplan.VerdictFileOnly,
		},
		{
			// A checkpoint from a controller that published the map plainly.
			name:   "an old baseline with entries is replaced whole",
			prev:   basePlan(withMap(renderplan.Map{Path: sensitiveMap, Entries: []renderplan.Entry{entry("route-a", "key1")}})),
			next:   basePlan(sensitiveMapFile("route-a key2\n")),
			loaded: true,
			want:   deployplan.VerdictRuntime,
			ops:    []api.Op{{Kind: api.OpMapReplace, Path: sensitiveMap}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := on34(tt.prev)
			if tt.loaded {
				base = withMapsLoaded(base, sensitiveMap)
			}

			got := deployplan.Diff(tt.next, base)

			assert.Equal(t, tt.want, got.Verdict, got.Reasons)
			assert.Equal(t, tt.ops, got.Ops)
			for _, op := range got.Ops {
				assert.Empty(t, op.Key)
				assert.Empty(t, op.Value)
			}
		})
	}
}
