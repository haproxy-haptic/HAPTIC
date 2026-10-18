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

package rendercontext

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores/storetest"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func TestStoreWrapper_RecordsReadFailure(t *testing.T) {
	wantErr := errors.New("list unavailable")
	errorCollector := NewResourceErrorCollector()
	wrapper := &StoreWrapper{
		readContext:    templating.WithImmutableResourceInputs(t.Context()),
		Store:          &storetest.MockStore{ListErr: wantErr},
		ResourceType:   "widgets",
		Logger:         testutil.NewTestLogger(),
		resourceErrors: errorCollector,
	}

	assert.Empty(t, wrapper.List())
	require.ErrorIs(t, errorCollector.Err(), wantErr)
	assert.Contains(t, errorCollector.Err().Error(), `resource "widgets" List failed`)
}

func TestStoreWrapper_RecordsAmbiguousGetSingle(t *testing.T) {
	errorCollector := NewResourceErrorCollector()
	wrapper := &StoreWrapper{
		readContext: templating.WithImmutableResourceInputs(t.Context()),
		Store: &storetest.MockStore{Items: []any{
			createResourceMap("first"),
			createResourceMap("second"),
		}},
		ResourceType:   "widgets",
		Logger:         testutil.NewTestLogger(),
		resourceErrors: errorCollector,
	}

	assert.Nil(t, wrapper.GetSingle("default", "shared"))
	require.Error(t, errorCollector.Err())
	assert.Contains(t, errorCollector.Err().Error(), "matched 2 objects; use Fetch or configure unique indexBy values")
}

func TestStoreWrapper_RecordsLookupOutsideNamespaces(t *testing.T) {
	byNamespace := []string{"metadata.namespace", "metadata.name"}
	tests := []struct {
		name     string
		resource config.WatchedResource
		lookup   func(*StoreWrapper) any
		wantErr  string
	}{
		{
			name:     "GetSingle outside",
			resource: config.WatchedResource{IndexBy: byNamespace, Namespaces: []string{"team-a"}},
			lookup:   func(w *StoreWrapper) any { return w.GetSingle("team-b", "tls") },
			wantErr:  "secrets team-b/tls cannot be read: watchedResources.secrets.namespaces limits the secrets watch to team-a. Reference secrets in team-a, or change watchedResources.secrets.namespaces",
		},
		{
			name:     "Fetch outside names the setting that chose the namespaces",
			resource: config.WatchedResource{IndexBy: byNamespace, Namespaces: []string{"haptic"}, NamespacesSetting: "controller.rbac.releaseNamespaceSecretsOnly"},
			lookup:   func(w *StoreWrapper) any { return w.Fetch("team-b") },
			wantErr:  "secrets in namespace team-b cannot be read: controller.rbac.releaseNamespaceSecretsOnly limits the secrets watch to haptic. Reference secrets in haptic, or change controller.rbac.releaseNamespaceSecretsOnly",
		},
		{
			name:     "lookup inside",
			resource: config.WatchedResource{IndexBy: byNamespace, Namespaces: []string{"team-a", "team-b"}},
			lookup:   func(w *StoreWrapper) any { return w.GetSingle("team-b", "tls") },
		},
		{
			name:     "a cloned wrapper keeps the scope",
			resource: config.WatchedResource{IndexBy: byNamespace, Namespaces: []string{"team-a"}},
			lookup: func(w *StoreWrapper) any {
				return w.CloneWithSnapshotViewContext(w.readContext, nil, false).GetSingle("team-b", "tls")
			},
			wantErr: "secrets team-b/tls cannot be read",
		},
		{
			name:     "unscoped watch",
			resource: config.WatchedResource{IndexBy: byNamespace},
			lookup:   func(w *StoreWrapper) any { return w.GetSingle("team-b", "tls") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errorCollector := NewResourceErrorCollector()
			wrapper := &StoreWrapper{
				readContext:    templating.WithImmutableResourceInputs(t.Context()),
				Store:          &storetest.MockStore{},
				ResourceType:   "secrets",
				Logger:         testutil.NewTestLogger(),
				IndexBy:        tt.resource.IndexBy,
				Scope:          namespaceScopeOf("secrets", &tt.resource),
				resourceErrors: errorCollector,
			}
			assert.Empty(t, tt.lookup(wrapper), "the store is empty and an out-of-scope lookup returns nothing")
			if tt.wantErr == "" {
				require.NoError(t, errorCollector.Err())
				return
			}
			require.Error(t, errorCollector.Err())
			assert.Contains(t, errorCollector.Err().Error(), tt.wantErr)
		})
	}
}

func TestNamespaceScopeOfUnscopedWatches(t *testing.T) {
	assert.Nil(t, namespaceScopeOf("secrets", &config.WatchedResource{IndexBy: []string{"metadata.namespace"}}))
	assert.Nil(t, namespaceScopeOf("secrets", &config.WatchedResource{IndexBy: []string{"metadata.name"}, Namespaces: []string{"a"}}),
		"validation rejects this config; without a namespace key there is nothing to check")
	assert.NoError(t, namespaceScopeOf("secrets", &config.WatchedResource{IndexBy: []string{"metadata.namespace"}, Namespaces: []string{"a"}}).Check(nil))
	var unscoped *NamespaceScope
	assert.NoError(t, unscoped.Check([]string{"any"}))
}

func TestStoreWrapper_RecordsInvalidKeyCount(t *testing.T) {
	tests := []struct {
		name string
		keys []any
		want string
	}{
		{name: "no keys", want: `resource "widgets" Fetch lookup has 0 keys; pass between 1 and 2`},
		{name: "too many keys", keys: []any{"one", "two", "three"}, want: `resource "widgets" Fetch lookup has 3 keys; pass between 1 and 2`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errorCollector := NewResourceErrorCollector()
			wrapper := &StoreWrapper{
				readContext:    templating.WithImmutableResourceInputs(t.Context()),
				Store:          &storetest.MockStore{},
				ResourceType:   "widgets",
				Logger:         testutil.NewTestLogger(),
				IndexBy:        []string{"metadata.namespace", "metadata.name"},
				resourceErrors: errorCollector,
			}

			assert.Empty(t, wrapper.Fetch(tt.keys...))
			err := errorCollector.Err()
			require.Error(t, err)
			assert.EqualError(t, err, tt.want)
		})
	}
}

func TestBuilder_RecordsTypedMaterializationFailure(t *testing.T) {
	elemType := reflect.StructOf([]reflect.StructField{
		{Name: "Count", Type: reflect.TypeOf(0), Tag: `json:"count"`},
	})
	cfg := &config.Config{
		WatchedResources: map[string]config.WatchedResource{
			"widgets": {IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
	}
	storeMap := map[string]stores.Store{
		"widgets": &storetest.MockStore{Items: []any{
			map[string]any{
				"metadata": map[string]any{"namespace": "default", "name": "bad"},
				"count":    "not-a-number",
			},
		}},
	}
	bctx := NewBuilder(
		t.Context(),
		cfg,
		&templating.PathResolver{},
		testutil.NewTestLogger(),
		WithStores(storeMap),
		WithTypedResources(map[string]reflect.Type{"widgets": elemType}),
	).Build()

	resources := reflect.ValueOf(bctx.Context["resources"]).Elem()
	list := resources.FieldByName("Widgets").Elem().FieldByName("List")
	result := list.Call(nil)[0]
	assert.Zero(t, result.Len())

	err := bctx.Err(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `resource "widgets" List could not materialize its typed object`)
}

func TestBuildResult_ErrPreservesContextCause(t *testing.T) {
	readErr := errors.New("read failed")
	collector := NewResourceErrorCollector()
	collector.Record(readErr)
	bctx := &BuildResult{ResourceErrors: collector}

	wantCause := errors.New("leader term ended")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(wantCause)

	assert.Same(t, wantCause, bctx.Err(ctx))
}

func TestBuildResult_ErrClassifiesResourceInputFailure(t *testing.T) {
	wantErr := errors.New("read failed")
	collector := NewResourceErrorCollector()
	collector.Record(wantErr)
	bctx := &BuildResult{ResourceErrors: collector}

	err := bctx.Err(t.Context())
	var resourceInputErr *ResourceInputError
	require.ErrorAs(t, err, &resourceInputErr)
	assert.ErrorIs(t, err, wantErr)
}

func TestResourceErrorCollector_ConcurrentRecord(t *testing.T) {
	collector := NewResourceErrorCollector()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			collector.Record(fmt.Errorf("read-%d", value%4))
		}(i)
	}
	wg.Wait()

	err := collector.Err()
	require.Error(t, err)
	for i := 0; i < 4; i++ {
		assert.Contains(t, err.Error(), fmt.Sprintf("read-%d", i))
	}
}
