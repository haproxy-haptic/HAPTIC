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

package testrunner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// An assertion type the CRD's enum omits makes the apiserver refuse the whole
// library object the moment it is applied, and the controller then waits
// forever for a configuration set that can never complete. Nothing offline sees
// it: the render is valid, the tests pass, and the rejection happens at apply.
func TestSupportedAssertionTypesMatchTheCRDEnum(t *testing.T) {
	for _, crd := range []string{
		"haproxy-haptic.org_haproxytemplatelibraries.yaml",
		"haproxy-haptic.org_haproxytemplateconfigs.yaml",
	} {
		t.Run(crd, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "haptic", "charts", "haptic-crds", "crds", crd))
			require.NoError(t, err)

			var doc map[string]any
			require.NoError(t, yaml.Unmarshal(raw, &doc))

			enum := findAssertionTypeEnum(doc)
			require.NotEmpty(t, enum, "no validationTests assertion `type` enum found in %s", crd)
			assert.ElementsMatch(t, SupportedAssertionTypes, enum)
		})
	}
}

// findAssertionTypeEnum walks the CRD for the assertion `type` enum. The schema
// is deep and sits at a different path in each of the two CRDs, so it is found
// by shape rather than by a hard-coded path.
func findAssertionTypeEnum(node any) []string {
	if enum := assertionEnumAt(node); enum != nil {
		return enum
	}
	for _, child := range children(node) {
		if found := findAssertionTypeEnum(child); found != nil {
			return found
		}
	}
	return nil
}

// assertionEnumAt reads this node's assertions[].type enum, or nil when the
// node is not the schema that declares one.
func assertionEnumAt(node any) []string {
	values, ok := mapPath(node, "properties", "assertions", "items", "properties", "type", "enum").([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		name, ok := v.(string)
		if !ok {
			return nil
		}
		out = append(out, name)
	}
	return out
}

func mapPath(node any, keys ...string) any {
	for _, key := range keys {
		asMap, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = asMap[key]
	}
	return node
}

func children(node any) []any {
	switch typed := node.(type) {
	case map[string]any:
		out := make([]any, 0, len(typed))
		for _, child := range typed {
			out = append(out, child)
		}
		return out
	case []any:
		return typed
	}
	return nil
}
