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

package api_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

func TestBlobPath(t *testing.T) {
	valid := strings.Repeat("0123456789abcdef", 4)
	assert.Equal(t, "/v1/blobs/"+valid, api.BlobPath(valid))
}

func TestManifestRequiredBlobs(t *testing.T) {
	digest := func(i int) string { return fmt.Sprintf("%064x", i) }
	tooMany := make([]string, api.MaxRequiredBlobs+1)
	for i := range tooMany {
		tooMany[i] = digest(i)
	}
	tests := []struct {
		name     string
		required []string
		valid    bool
	}{
		{name: "none", valid: true},
		{name: "distinct", required: []string{digest(1), digest(2)}, valid: true},
		{name: "at the limit", required: tooMany[:api.MaxRequiredBlobs], valid: true},
		{name: "over the limit", required: tooMany},
		{name: "duplicate", required: []string{digest(1), digest(1)}},
		{name: "not a digest", required: []string{"maxmind.mmdb"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := api.Manifest{RequiredBlobs: tt.required}
			err := m.ValidateRequiredBlobs()
			if tt.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}
