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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

// The chart passes blobs.maxSize as a Kubernetes quantity.
func TestParseBlobMaxSize(t *testing.T) {
	defaultSize, err := parseBlobMaxSize(agentCmd.Flags().Lookup("blob-max-size").DefValue)
	require.NoError(t, err)
	assert.Equal(t, int64(api.DefaultMaxBlobBytes), defaultSize)

	size, err := parseBlobMaxSize("300Mi")
	require.NoError(t, err)
	assert.Equal(t, int64(300<<20), size)

	for _, raw := range []string{"", "0", "-1Mi", "0.5", "lots"} {
		_, err := parseBlobMaxSize(raw)
		assert.Error(t, err, raw)
	}
}
