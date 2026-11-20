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

package inputisolation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validation"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

type validatingFixture struct {
	fail       error
	aliasCheck bool
}

func (*validatingFixture) HeldRender(rendercontext.RenderMode) error { return nil }

func (p *validatingFixture) Execute(ctx context.Context, provider stores.StoreProvider, _ rendercontext.RenderMode, _ ...rendercontext.Option) (*pipeline.PipelineResult, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	items, err := stores.ListContext(ctx, provider.GetStore("widgets"))
	if err != nil {
		return nil, err
	}
	if p.aliasCheck {
		other, err := stores.ListContext(ctx, provider.GetStore("other"))
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(items, other) {
			return nil, errors.New("aliases disagree")
		}
	}
	for _, item := range items {
		value := item.(*unstructured.Unstructured)
		if value.GetAnnotations()["invalid"] == "true" {
			return nil, fmt.Errorf("invalid widget %s", value.GetName())
		}
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return &pipeline.PipelineResult{HAProxyConfig: string(encoded)}, nil
}

func widget(name, value string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.test/v1", "kind": "Widget",
		"metadata": map[string]any{"namespace": "default", "name": name},
		"spec":     map[string]any{"value": value},
	}}
}

func TestIndependentUpdatesAdvanceWhileInvalidResourceIsRetained(t *testing.T) {
	for _, atStartup := range []bool{false, true} {
		t.Run(fmt.Sprint("invalid-at-startup=", atStartup), func(t *testing.T) {
			live := k8sstore.NewMemoryStore(2)
			provider := stores.NewRealStoreProvider(map[string]stores.Store{"widgets": live, "other": live})
			watch := config.WatchedResource{APIVersion: "example.test/v1", Resources: "widgets", IndexBy: []string{"metadata.namespace", "metadata.name"}}
			alias := watch
			alias.APIVersion = "example.test/v2"
			service := New(&validatingFixture{aliasCheck: true}, map[string]config.WatchedResource{"widgets": watch, "other": alias}, nil)
			bad := widget("bad", "accepted-before")
			if !atStartup {
				require.NoError(t, live.Add(bad, []string{"default", "bad"}))
				_, err := service.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
				require.NoError(t, err)
			}
			bad.SetAnnotations(map[string]string{"invalid": "true"})
			require.NoError(t, live.Update(bad, []string{"default", "bad"}))
			require.NoError(t, live.Add(widget("healthy", "new-endpoint"), []string{"default", "healthy"}))
			result, err := service.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
			require.NoError(t, err)
			assert.Contains(t, result.HAProxyConfig, "new-endpoint")
			assert.NotContains(t, result.HAProxyConfig, "invalid:true")
			accepted, rejected, ready := service.Snapshot()
			require.True(t, ready)
			require.Len(t, rejected, 2, "both aliases report the same rejected object")
			items, err := accepted.GetStore("widgets").Get("default", "bad")
			require.NoError(t, err)
			assert.Equal(t, !atStartup, len(items) == 1)
			bad.SetAnnotations(nil)
			require.NoError(t, live.Update(bad, []string{"default", "bad"}))
			_, err = service.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
			require.NoError(t, err)
			_, rejected, ready = service.Snapshot()
			require.True(t, ready)
			assert.Empty(t, rejected)
		})
	}
}

func TestFailedBaselineDoesNotAuthorizeInputSelection(t *testing.T) {
	validator := &validatingFixture{}
	watch := config.WatchedResource{APIVersion: "example.test/v1", Resources: "widgets", IndexBy: []string{"metadata.namespace", "metadata.name"}}
	service := New(validator, map[string]config.WatchedResource{"widgets": watch}, nil)
	live := k8sstore.NewMemoryStore(2)
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"widgets": live})
	_, err := service.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	validator.fail = errors.New("output validator unavailable")
	require.NoError(t, live.Add(widget("healthy", "never-accepted"), []string{"default", "healthy"}))
	_, err = service.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.Error(t, err)
	accepted, _, ready := service.Snapshot()
	require.True(t, ready)
	items, err := accepted.GetStore("widgets").List()
	require.NoError(t, err)
	assert.Empty(t, items)
}

type validationFunc func(context.Context, stores.StoreProvider) (*pipeline.PipelineResult, error)

func (validationFunc) HeldRender(rendercontext.RenderMode) error { return nil }

func (f validationFunc) Execute(ctx context.Context, provider stores.StoreProvider, _ rendercontext.RenderMode, _ ...rendercontext.Option) (*pipeline.PipelineResult, error) {
	return f(ctx, provider)
}

func TestDependentChangesAdvanceTogetherDespiteUnrelatedInvalidInput(t *testing.T) {
	live := k8sstore.NewMemoryStore(2)
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"widgets": live})
	watch := config.WatchedResource{APIVersion: "example.test/v1", Resources: "widgets", IndexBy: []string{"metadata.namespace", "metadata.name"}}
	validate := validationFunc(func(ctx context.Context, inputs stores.StoreProvider) (*pipeline.PipelineResult, error) {
		items, err := stores.ListContext(ctx, inputs.GetStore("widgets"))
		if err != nil {
			return nil, err
		}
		values := map[string]string{}
		for _, item := range items {
			object := item.(*unstructured.Unstructured)
			value, _, _ := unstructured.NestedString(object.Object, "spec", "value")
			values[object.GetName()] = value
		}
		if values["a"] != values["b"] {
			return nil, errors.New("dependent widgets disagree")
		}
		return (&validatingFixture{}).Execute(ctx, inputs, rendercontext.RenderModeReconcile)
	})
	service := New(validate, map[string]config.WatchedResource{"widgets": watch}, nil)
	for _, name := range []string{"a", "b"} {
		require.NoError(t, live.Add(widget(name, "before"), []string{"default", name}))
	}
	_, err := service.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	for _, name := range []string{"a", "b"} {
		require.NoError(t, live.Update(widget(name, "after"), []string{"default", name}))
	}
	bad := widget("bad", "poison")
	bad.SetAnnotations(map[string]string{"invalid": "true"})
	require.NoError(t, live.Add(bad, []string{"default", "bad"}))
	result, err := service.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Contains(t, result.HAProxyConfig, "after")
	require.NotContains(t, result.HAProxyConfig, "before")
	_, rejected, _ := service.Snapshot()
	require.Len(t, rejected, 1)
	require.Equal(t, "bad", rejected[0].Name)
}

func TestCancellationCannotAcceptInputs(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	live := k8sstore.NewMemoryStore(2)
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"widgets": live})
	validator := validationFunc(func(context.Context, stores.StoreProvider) (*pipeline.PipelineResult, error) {
		cancel()
		return &pipeline.PipelineResult{HAProxyConfig: "not accepted"}, nil
	})
	service := New(validator, map[string]config.WatchedResource{
		"widgets": {APIVersion: "example.test/v1", Resources: "widgets", IndexBy: []string{"metadata.namespace", "metadata.name"}},
	}, nil)
	_, err := service.Execute(ctx, provider, rendercontext.RenderModeReconcile)
	require.ErrorIs(t, err, context.Canceled)
	_, _, ready := service.Snapshot()
	require.False(t, ready)
}

func TestStartupWithRequiredDependencyAndInvalidPeer(t *testing.T) {
	live := k8sstore.NewMemoryStore(2)
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"widgets": live})
	validator := validationFunc(func(ctx context.Context, inputs stores.StoreProvider) (*pipeline.PipelineResult, error) {
		items, err := stores.GetContext(ctx, inputs.GetStore("widgets"), "default", "required")
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, errors.New("required widget is missing")
		}
		return (&validatingFixture{}).Execute(ctx, inputs, rendercontext.RenderModeReconcile)
	})
	watch := config.WatchedResource{APIVersion: "example.test/v1", Resources: "widgets", IndexBy: []string{"metadata.namespace", "metadata.name"}}
	selector := New(validator, map[string]config.WatchedResource{"widgets": watch}, nil)
	require.NoError(t, live.Add(widget("required", "present"), []string{"default", "required"}))
	bad := widget("bad", "poison")
	bad.SetAnnotations(map[string]string{"invalid": "true"})
	require.NoError(t, live.Add(bad, []string{"default", "bad"}))
	result, err := selector.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Contains(t, result.HAProxyConfig, "present")
	require.NotContains(t, result.HAProxyConfig, "poison")

	require.NoError(t, live.Delete("default", "required", []string{"default", "required"}))
	require.NoError(t, live.Add(widget("healthy", "new-endpoint"), []string{"default", "healthy"}))
	result, err = selector.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Contains(t, result.HAProxyConfig, "present")
	require.Contains(t, result.HAProxyConfig, "new-endpoint")
	_, rejected, _ := selector.Snapshot()
	require.Len(t, rejected, 2)
	foundDeletion := false
	for _, rejection := range rejected {
		if rejection.Name == "required" {
			foundDeletion = rejection.Deleted
		}
	}
	require.True(t, foundDeletion, "an invalid deletion is visible and cannot freeze healthy updates")
}

func TestRepeatedInvalidRevisionDoesNotRepeatWarning(t *testing.T) {
	live := k8sstore.NewMemoryStore(2)
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"widgets": live})
	var log bytes.Buffer
	attempts := 0
	validator := validationFunc(func(ctx context.Context, inputs stores.StoreProvider) (*pipeline.PipelineResult, error) {
		result, err := (&validatingFixture{}).Execute(ctx, inputs, rendercontext.RenderModeReconcile)
		if err != nil {
			attempts++
			return nil, fmt.Errorf("validation process %d: %w", attempts, err)
		}
		return result, nil
	})
	watch := config.WatchedResource{APIVersion: "example.test/v1", Resources: "widgets", IndexBy: []string{"metadata.namespace", "metadata.name"}}
	selector := New(validator, map[string]config.WatchedResource{"widgets": watch}, slog.New(slog.NewTextHandler(&log, nil)))
	bad := widget("bad", "poison")
	bad.SetAnnotations(map[string]string{"invalid": "true"})
	require.NoError(t, live.Add(bad, []string{"default", "bad"}))
	for range 3 {
		_, err := selector.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
		require.NoError(t, err)
	}
	require.Equal(t, 1, strings.Count(log.String(), "level=WARN"))
	require.Contains(t, log.String(), "reason=")
	require.Contains(t, log.String(), "invalid widget bad")
	bad.SetAnnotations(map[string]string{"invalid": "true", "attempt": "correction"})
	require.NoError(t, live.Update(bad, []string{"default", "bad"}))
	_, err := selector.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(log.String(), "level=WARN"))
}

func TestRejectionNotificationsTrackCurrentSnapshot(t *testing.T) {
	live := k8sstore.NewMemoryStore(2)
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"widgets": live})
	watch := config.WatchedResource{APIVersion: "example.test/v1", Resources: "widgets", IndexBy: []string{"metadata.namespace", "metadata.name"}}
	var current []Rejection
	selector := New(&validatingFixture{}, map[string]config.WatchedResource{"widgets": watch}, nil, Callbacks{Updated: func(rejected []Rejection) { current = rejected }})
	bad := widget("bad", "before")
	bad.SetUID("original-uid")
	bad.SetResourceVersion("1")
	require.NoError(t, live.Add(bad, []string{"default", "bad"}))
	_, err := selector.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Empty(t, current)
	bad.SetResourceVersion("2")
	bad.SetAnnotations(map[string]string{"invalid": "true"})
	require.NoError(t, live.Update(bad, []string{"default", "bad"}))
	_, err = selector.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Len(t, current, 1)
	require.Equal(t, corev1.ObjectReference{APIVersion: "example.test/v1", Kind: "Widget", Namespace: "default", Name: "bad", UID: "original-uid", ResourceVersion: "2"}, current[0].Object)
	current[0].Reason = "mutated observer copy"
	_, snapshot, _ := selector.Snapshot()
	require.NotEqual(t, current[0].Reason, snapshot[0].Reason)
	bad.SetAnnotations(nil)
	require.NoError(t, live.Update(bad, []string{"default", "bad"}))
	_, err = selector.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Empty(t, current, "repair must clear the rejection notification")
}

func TestInputIndependentRenderFailureSkipsTheInputSearch(t *testing.T) {
	live := k8sstore.NewMemoryStore(2)
	for index := range 100 {
		name := fmt.Sprintf("widget-%03d", index)
		require.NoError(t, live.Add(widget(name, "healthy"), []string{"default", name}))
	}
	renders := 0
	validator := validationFunc(func(context.Context, stores.StoreProvider) (*pipeline.PipelineResult, error) {
		renders++
		return nil, &pipeline.PipelineError{Phase: pipeline.PhaseRender, Cause: errors.New("invalid chart value")}
	})
	watch := config.WatchedResource{APIVersion: "example.test/v1", Resources: "widgets", IndexBy: []string{"metadata.namespace", "metadata.name"}}
	selector := New(validator, map[string]config.WatchedResource{"widgets": watch}, nil)
	_, err := selector.Execute(t.Context(), stores.NewRealStoreProvider(map[string]stores.Store{"widgets": live}), rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "invalid chart value")
	require.Equal(t, 2, renders)
}

func TestUnavailableValidatorPreservesObservedInputsForRetry(t *testing.T) {
	live := k8sstore.NewMemoryStore(2)
	watch := config.WatchedResource{APIVersion: "example.test/v1", Resources: "widgets", IndexBy: []string{"metadata.namespace", "metadata.name"}}
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"widgets": live})
	calls := 0
	validator := validationFunc(func(context.Context, stores.StoreProvider) (*pipeline.PipelineResult, error) {
		calls++
		if calls == 1 {
			return nil, &pipeline.PipelineError{Phase: pipeline.PhaseValidation, ValidationPhase: "external", Cause: validation.ErrValidatorUnavailable}
		}
		return &pipeline.PipelineResult{}, nil
	})
	selector := New(validator, map[string]config.WatchedResource{"widgets": watch}, nil)
	require.NoError(t, live.Add(widget("healthy", "pending"), []string{"default", "healthy"}))
	_, err := selector.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.ErrorIs(t, err, validation.ErrValidatorUnavailable)
	require.Equal(t, 1, calls)
	_, rejected, ready := selector.Snapshot()
	require.False(t, ready)
	require.Empty(t, rejected)
	_, err = selector.Execute(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	accepted, _, ready := selector.Snapshot()
	require.True(t, ready)
	items, err := accepted.GetStore("widgets").Get("default", "healthy")
	require.NoError(t, err)
	require.Len(t, items, 1)
}
