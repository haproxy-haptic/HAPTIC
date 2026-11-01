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

package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCommandTargetDropsKeysAndValues(t *testing.T) {
	for text, want := range map[string]string{
		"set map maps/apikeys.map secret-key consumer": "set map maps/apikeys.map",
		"del map maps/host.map app.example.com":        "del map maps/host.map",
		"add map @7 maps/host.map":                     "add map @7 maps/host.map",
		"commit map @7 maps/host.map":                  "commit map @7 maps/host.map",
		"prepare map maps/host.map":                    "prepare map maps/host.map",
		"set server be/srv addr 10.0.0.1 port 80":      "set server be/srv",
		"show info": "show info",
	} {
		assert.Equal(t, want, commandTarget(text), text)
	}
}
