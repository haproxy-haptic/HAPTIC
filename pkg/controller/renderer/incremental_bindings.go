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

package renderer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

type incrementalBinding struct {
	component  string
	source     string
	props      []byte
	projection *incrementalResourceProjection
}

type incrementalResourceProjection struct {
	Cell string   `json:"cell"`
	Key  string   `json:"key"`
	Keys []string `json:"keys"`
	Rank string   `json:"rank,omitempty"`

	digest   string
	identity string
}

func decodeIncrementalResourceProjection(props []byte) (*incrementalResourceProjection, error) {
	decoder := json.NewDecoder(bytes.NewReader(props))
	decoder.DisallowUnknownFields()
	var projection incrementalResourceProjection
	if err := decoder.Decode(&projection); err != nil {
		return nil, fmt.Errorf("decoding resource projection: %w", err)
	}
	if err := requireIncrementalBindingsEOF(decoder); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(projection)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing resource projection: %w", err)
	}
	if !bytes.Equal(props, canonical) {
		return nil, errors.New("resource projection must use canonical JSON")
	}
	if len(projection.Keys) == 0 {
		return nil, errors.New("resource projection keys must not be empty")
	}
	for index, key := range projection.Keys {
		if key == "" {
			return nil, fmt.Errorf("resource projection keys[%d] is empty", index)
		}
	}
	if projection.Cell == "" {
		return nil, errors.New("resource projection cell is empty")
	}
	if projection.Key == "" {
		return nil, errors.New("resource projection key is empty")
	}
	digest := sha256.Sum256(props)
	projection.digest = hex.EncodeToString(digest[:])
	projection.identity = incremental.EncodeTuple("resource-projection-binding", projection.digest, string(props))
	return &projection, nil
}

const incrementalResourceProjectionNamespace = "$resourceProjection"

func incrementalResourceProjectionIdentity(
	projection *incrementalResourceProjection,
) (namespace, name string, ok bool) {
	if projection == nil || projection.digest == "" || projection.identity == "" {
		return "", "", false
	}
	return incrementalResourceProjectionNamespace, projection.identity, true
}

func incrementalResourceProjectionForBinding(
	binding incrementalBinding,
) (*incrementalResourceProjection, error) {
	if binding.projection != nil {
		projection, err := decodeIncrementalResourceProjection(binding.props)
		if err != nil {
			return nil, err
		}
		if projection.digest != binding.projection.digest ||
			projection.identity != binding.projection.identity {
			return nil, errors.New("resource projection binding has invalid provenance")
		}
		return projection, nil
	}
	return decodeIncrementalResourceProjection(binding.props)
}

func cloneIncrementalResourceProjection(
	projection *incrementalResourceProjection,
) *incrementalResourceProjection {
	if projection == nil {
		return nil
	}
	cloned := *projection
	cloned.Keys = slices.Clone(projection.Keys)
	return &cloned
}

func decodeIncrementalBindings(
	component string,
	encoded []byte,
	watched map[string]config.WatchedResource,
) ([]incrementalBinding, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decoding incremental bindings: %w", err)
	}
	if err := requireIncrementalBindingsEOF(decoder); err != nil {
		return nil, err
	}
	bindings, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("incremental bindings must be a JSON object, got %T", decoded)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalizing incremental bindings: %w", err)
	}
	if !bytes.Equal(encoded, canonical) {
		return nil, errors.New("incremental bindings must use canonical JSON")
	}

	aliases := slices.Sorted(maps.Keys(bindings))
	result := make([]incrementalBinding, 0, len(aliases))
	for _, alias := range aliases {
		if _, known := watched[alias]; !known {
			return nil, fmt.Errorf("incremental binding alias %q is not a watched resource", alias)
		}
		props, ok := bindings[alias].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("incremental binding alias %q props must be a JSON object", alias)
		}
		if _, reserved := props[incrementalRenderModeContextName]; reserved {
			return nil, fmt.Errorf("incremental binding alias %q cannot supply derived renderMode", alias)
		}
		canonicalProps, err := json.Marshal(props)
		if err != nil {
			return nil, fmt.Errorf("canonicalizing incremental binding alias %q props: %w", alias, err)
		}
		result = append(result, incrementalBinding{component: component, source: alias, props: canonicalProps})
	}
	return result, nil
}

func requireIncrementalBindingsEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case err == nil:
		return errors.New("incremental bindings must contain one JSON object")
	default:
		return fmt.Errorf("decoding trailing incremental bindings data: %w", err)
	}
}

func staticIncrementalBinding(component, source string) incrementalBinding {
	return incrementalBinding{component: component, source: source, props: []byte("{}")}
}
