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
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
)

func TestDumpRenderedContentListsSecretFilesByName(t *testing.T) {
	results := &testrunner.TestResults{TestResults: []testrunner.TestResult{{
		TestName:            "test-a",
		RenderedConfig:      "global\n",
		RenderedFiles:       map[string]string{"vector.yaml": "sources: {}"},
		RenderedSecretNames: []string{"cert:/ssl/tls.pem", "secret:ssl/tls-ticket-keys"},
	}}}

	out := captureStdout(t, func() { dumpRenderedContent(results) })

	assert.Contains(t, out, "### Secret Files (content not shown)\ncert:/ssl/tls.pem\nsecret:ssl/tls-ticket-keys\n")
	assert.Contains(t, out, "#### vector.yaml\n", "general files keep their content sections")
	assert.NotContains(t, out, "#### cert:", "a secret file must not get a content section")
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	stdout := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = stdout })
	fn()
	require.NoError(t, writer.Close())
	os.Stdout = stdout
	out, err := io.ReadAll(reader)
	require.NoError(t, err)
	return string(out)
}
