// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package renderer

import (
	"errors"
	"fmt"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

type incrementalActiveGroupIndex struct {
	instances *iradix.Tree[struct{}]
	seal      *iradix.Tree[struct{}]
}

func newIncrementalActiveGroupIndex() *incrementalActiveGroupIndex {
	return sealIncrementalActiveGroupIndex(iradix.New[struct{}]())
}

func sealIncrementalActiveGroupIndex(instances *iradix.Tree[struct{}]) *incrementalActiveGroupIndex {
	return &incrementalActiveGroupIndex{instances: instances, seal: instances}
}

func (i *incrementalActiveGroupIndex) validateAuthentication() error {
	if i == nil || i.instances == nil || i.seal == nil {
		return errors.New("incremental active-group index is unavailable")
	}
	if i.instances != i.seal {
		return errors.New("incremental active-group authentication seal does not match its root")
	}
	return nil
}

func incrementalActiveGroupInstanceKey(
	component *incrementalComponent,
	source, namespace, name string,
) []byte {
	return []byte(incremental.EncodeTuple("active-group", component.group, component.name, source, namespace, name))
}

func incrementalActiveGroupPrefix(group string) []byte {
	return []byte(incremental.EncodeTuple("active-group", group))
}

func incrementalActiveGroupExists(instances *iradix.Node[struct{}], group string) bool {
	active := false
	instances.WalkPrefix(incrementalActiveGroupPrefix(group), func(_ []byte, _ struct{}) bool {
		active = true
		return true
	})
	return active
}

func (r *incrementalRenderSession) setActivationInstanceActive(
	component *incrementalComponent,
	source, namespace, name string,
	active bool,
) error {
	if len(component.activationPaths) == 0 {
		return fmt.Errorf("incremental active-group index received ungated component %q", component.name)
	}
	key := incrementalActiveGroupInstanceKey(component, source, namespace, name)
	_, exists := r.activeGroups.Get(key)
	switch {
	case active && !exists:
		r.activeGroups.Insert(key, struct{}{})
	case !active && exists:
		r.activeGroups.Delete(key)
	}
	return nil
}
