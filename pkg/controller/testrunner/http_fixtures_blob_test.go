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

package testrunner

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// A blob source's fixture is the stored blob: blob.Fetch sees its reference.
func TestBlobFixtureRendersAsTheStoredBlob(t *testing.T) {
	fixtures := []config.HTTPResourceFixture{{URL: "http://dbs/city", Content: "database"}}
	wrapper := NewFixtureHTTPStoreWrapper(CreateHTTPStoreFromFixtures(fixtures, slog.Default()), slog.Default())
	ref := blob.Describe([]byte("database"))
	options := func(spec blob.Spec) map[string]any {
		return map[string]any{templating.HTTPOptionBlob: spec}
	}

	content, err := wrapper.Fetch("http://dbs/city", options(blob.Spec{Unpack: blob.UnpackGzip}))
	require.NoError(t, err)
	assert.Equal(t, ref.Encode(), content)

	content, err = wrapper.Fetch("http://dbs/city", options(blob.Spec{Unpack: blob.UnpackNone, SHA256: ref.Digest}))
	require.NoError(t, err)
	assert.Equal(t, ref.Encode(), content)

	content, err = wrapper.Fetch("http://dbs/city", options(blob.Spec{Unpack: blob.UnpackNone, SHA256: blob.Describe(nil).Digest}))
	require.NoError(t, err, "a refused blob renders an empty handle, as in production")
	assert.Empty(t, content)
}
