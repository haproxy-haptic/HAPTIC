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

package rendercontext

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/planblob"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

var sensitiveMapSealKey = []byte("test seal key")

func sensitiveMapRegistry(t *testing.T) *PlanRegistry {
	t.Helper()
	registry := NewPlanRegistry(&templating.PathResolver{BaseDir: "/etc/haproxy", MapsDir: "maps"})
	require.NoError(t, registry.SensitiveMap("hmac.map"))
	require.NoError(t, registry.SensitiveMap("maps/hmac.map"), "a resolved path names the same map")
	return registry
}

func sensitiveMapPlan(t *testing.T, registry *PlanRegistry, content string) (*dataplane.AuxiliaryFiles, *renderplan.Plan) {
	t.Helper()
	aux, err := registry.SealSensitiveMaps(&dataplane.AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{
		{Path: "hmac.map", Content: content},
		{Path: "host.map", Content: "example.com be_app\n"},
	}}, sensitiveMapSealKey)
	require.NoError(t, err)
	config, _, err := registry.Assemble(context.Background(), "global\n", nil)
	require.NoError(t, err)
	plan, err := registry.Plan(config, aux)
	require.NoError(t, err)
	return aux, plan
}

func TestSealSensitiveMapsMovesOnlyDeclaredMaps(t *testing.T) {
	aux, _ := sensitiveMapPlan(t, sensitiveMapRegistry(t), "route-a hunter2\n")

	assert.Equal(t, []auxiliaryfiles.MapFile{{Path: "host.map", Content: "example.com be_app\n"}}, aux.MapFiles)
	assert.Equal(t, []auxiliaryfiles.SensitiveMapFile{{
		Path: "hmac.map", Content: "route-a hunter2\n",
		Seal: auxiliaryfiles.SealContent(sensitiveMapSealKey, "route-a hunter2\n"),
	}}, aux.SensitiveMapFiles)
	assert.Empty(t, aux.CurrentFiles()["hmac.map"], "currentFiles never carries a sensitive map")
}

func TestSealSensitiveMapsWithoutKeyIsNeverPublishable(t *testing.T) {
	aux, err := sensitiveMapRegistry(t).SealSensitiveMaps(&dataplane.AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{
		{Path: "hmac.map", Content: "route-a hunter2\n"},
	}}, nil)
	require.NoError(t, err)
	require.Len(t, aux.SensitiveMapFiles, 1)
	assert.NotEmpty(t, aux.SensitiveMapFiles[0].Seal)
	assert.False(t, auxiliaryfiles.IsKeyedSeal(aux.SensitiveMapFiles[0].Seal))
}

func TestPlanCarriesSensitiveMapOnlyAsSealedFile(t *testing.T) {
	_, plan := sensitiveMapPlan(t, sensitiveMapRegistry(t), "route-a hunter2\n")

	assert.NotContains(t, plan.Maps, "maps/hmac.map")
	assert.Contains(t, plan.Maps, "maps/host.map")
	var sealed *renderplan.File
	for i := range plan.Files {
		if plan.Files[i].Path == "maps/hmac.map" {
			sealed = &plan.Files[i]
		}
	}
	require.NotNil(t, sealed)
	assert.Equal(t, renderplan.FileKindMap, sealed.Kind)
	assert.Equal(t, auxiliaryfiles.SealContent(sensitiveMapSealKey, "route-a hunter2\n"), sealed.Seal)
	assert.NotContains(t, string(plan.Canonical()), "hunter2")
	assert.NotContains(t, string(plan.Canonical()), renderplan.DigestString("route-a hunter2\n"))

	checkpoint, err := planblob.EncodeCheckpoint(plan)
	require.NoError(t, err)
	decoded, err := planblob.DecodeCheckpoint(checkpoint, "global\n", map[string]string{
		"haproxy.cfg": "global\n", "maps/hmac.map": "route-a hunter2\n", "maps/host.map": "example.com be_app\n",
	})
	require.NoError(t, err)
	assert.NotContains(t, decoded.Maps, "maps/hmac.map", "a checkpoint carries no sensitive entries")
}

// The plan ID moves with the content only through the keyed seal.
func TestSensitiveMapPlanIDFollowsTheSeal(t *testing.T) {
	_, first := sensitiveMapPlan(t, sensitiveMapRegistry(t), "route-a hunter2\n")
	_, same := sensitiveMapPlan(t, sensitiveMapRegistry(t), "route-a hunter2\n")
	_, changed := sensitiveMapPlan(t, sensitiveMapRegistry(t), "route-a hunter3\n")
	assert.Equal(t, first.ID, same.ID)
	assert.NotEqual(t, first.ID, changed.ID)

	tampered := first.Clone()
	for i := range tampered.Files {
		if tampered.Files[i].Path == "maps/hmac.map" {
			tampered.Files[i].Digest = renderplan.DigestString("route-a other\n")
			tampered.Files[i].Content = "route-a other\n"
		}
	}
	assert.Equal(t, first.Canonical(), tampered.Canonical(), "the canonical form ignores sealed content")
}

func TestSensitiveMapDeclarationRejectsAnEmptyPath(t *testing.T) {
	require.Error(t, NewPlanRegistry(nil).SensitiveMap(""))
}
