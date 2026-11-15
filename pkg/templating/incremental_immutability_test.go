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

package templating

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/scriggo/builtin"
)

func TestImmutableCertificateSliceIdentityBindsExactHeader(t *testing.T) {
	var nilValues []int
	emptyValues := make([]int, 0)
	base := []int{1, 2}
	separate := []int{1, 2}
	zeroLength := base[:0]

	assert.Equal(t, immutableCertificateSlice(base), immutableCertificateSlice(base))
	assert.NotEqual(t, immutableCertificateSlice(nilValues), immutableCertificateSlice(emptyValues))
	assert.Equal(t, immutableCertificateSlice(zeroLength), immutableCertificateSlice(base[:0]))
	assert.NotEqual(t, immutableCertificateSlice(zeroLength), immutableCertificateSlice(emptyValues))
	assert.NotEqual(t, immutableCertificateSlice(base), immutableCertificateSlice(base[:1]))
	assert.NotEqual(t, immutableCertificateSlice(base), immutableCertificateSlice(base[:1:1]))
	assert.False(t, immutableCertificateSlice(base) == immutableCertificateSlice(separate))
}

func TestImmutableRangeIndexMatchesExactRanges(t *testing.T) {
	ranges := []immutableRange{
		{start: 800, end: 900},
		{start: 100, end: 200},
		{start: 400, end: 450},
		{start: 300, end: 700},
		{start: 100, end: 250},
		{start: 50, end: 50},
	}
	var index immutableRangeIndex
	for _, value := range ranges {
		index.insert(value)
	}

	for pointer := uintptr(0); pointer < 1000; pointer++ {
		want := false
		for _, value := range ranges {
			if pointer >= value.start && pointer < value.end {
				want = true
				break
			}
		}
		assert.Equal(t, want, index.contains(pointer), "pointer %d", pointer)
	}
	assertImmutableRangeIndex(t, index.root, nil, nil)
}

func TestImmutableRangeIndexInsertionOrders(t *testing.T) {
	orders := map[string][]uintptr{
		"ascending":   {10, 20, 30, 40, 50, 60, 70},
		"descending":  {70, 60, 50, 40, 30, 20, 10},
		"alternating": {40, 10, 70, 20, 60, 30, 50},
	}
	for name, starts := range orders {
		t.Run(name, func(t *testing.T) {
			var index immutableRangeIndex
			for _, start := range starts {
				index.insert(immutableRange{start: start, end: start + 5})
			}
			for _, start := range starts {
				assert.True(t, index.contains(start))
				assert.True(t, index.contains(start+4))
				assert.False(t, index.contains(start+5))
			}
			assertImmutableRangeIndex(t, index.root, nil, nil)
		})
	}
}

func TestImmutableRangeIndexMatchesDenseOverlaps(t *testing.T) {
	ranges := make([]immutableRange, 513)
	for index := range ranges {
		start := uintptr((index * 7919) % 8000)
		ranges[index] = immutableRange{start: start, end: start + uintptr(1+(index*37)%192)}
	}
	orders := map[string][]int{
		"ascending":  make([]int, len(ranges)),
		"descending": make([]int, len(ranges)),
		"permuted":   make([]int, len(ranges)),
	}
	for index := range ranges {
		orders["ascending"][index] = index
		orders["descending"][index] = len(ranges) - index - 1
		orders["permuted"][index] = index * 257 % len(ranges)
	}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			var index immutableRangeIndex
			for _, position := range order {
				index.insert(ranges[position])
			}
			for pointer := uintptr(0); pointer < 8300; pointer++ {
				want := false
				for _, value := range ranges {
					if pointer >= value.start && pointer < value.end {
						want = true
						break
					}
				}
				assert.Equal(t, want, index.contains(pointer), "pointer %d", pointer)
			}
			assertImmutableRangeIndex(t, index.root, nil, nil)
		})
	}
}

func assertImmutableRangeIndex(
	t *testing.T,
	node *immutableRangeNode,
	minimum, maximum *uintptr,
) (height int, maxEnd uintptr) {
	t.Helper()
	if node == nil {
		return 0, 0
	}
	if minimum != nil {
		assert.Greater(t, node.rangeValue.start, *minimum)
	}
	if maximum != nil {
		assert.Less(t, node.rangeValue.start, *maximum)
	}
	leftHeight, leftMax := assertImmutableRangeIndex(t, node.left, minimum, &node.rangeValue.start)
	rightHeight, rightMax := assertImmutableRangeIndex(t, node.right, &node.rangeValue.start, maximum)
	wantHeight := max(leftHeight, rightHeight) + 1
	wantMax := max(node.rangeValue.end, leftMax, rightMax)
	assert.Equal(t, wantHeight, node.height)
	assert.Equal(t, wantMax, node.maxEnd)
	assert.LessOrEqual(t, leftHeight-rightHeight, 1)
	assert.GreaterOrEqual(t, leftHeight-rightHeight, -1)
	return wantHeight, wantMax
}

var immutableRangeBenchmarkSink bool

func BenchmarkImmutableRangeIndexContains(b *testing.B) {
	for _, count := range []int{1, 128, 8192} {
		b.Run(fmt.Sprintf("ranges=%d", count), func(b *testing.B) {
			var index immutableRangeIndex
			for value := range count {
				start := uintptr(value*32 + 16)
				index.insert(immutableRange{start: start, end: start + 16})
			}
			pointer := uintptr((count-1)*32 + 23)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				immutableRangeBenchmarkSink = index.contains(pointer)
			}
		})
	}
}

func TestIncrementalImmutableInputsComposeWithoutMutatingParent(t *testing.T) {
	first := map[string]any{"value": "first"}
	second := map[string]any{"value": "second"}

	parent := WithIncrementalImmutableInputs(t.Context(), first)
	child := WithIncrementalImmutableInputs(parent, second)
	parentStorage, ok := parent.Value(immutableStorageContextKey{}).(*immutableStorage)
	require.True(t, ok)
	childStorage, ok := child.Value(immutableStorageContextKey{}).(*immutableStorage)
	require.True(t, ok)

	assert.True(t, parentStorage.contains(reflect.ValueOf(first)))
	assert.False(t, parentStorage.contains(reflect.ValueOf(second)))
	assert.True(t, childStorage.contains(reflect.ValueOf(first)))
	assert.True(t, childStorage.contains(reflect.ValueOf(second)))
}

func TestIncrementalImmutableInputsObserveLateParentRegistration(t *testing.T) {
	late := map[string]any{"value": "late"}
	parent := WithIncrementalImmutableInputs(t.Context())
	child := WithIncrementalImmutableInputs(parent)
	childStorage, ok := child.Value(immutableStorageContextKey{}).(*immutableStorage)
	require.True(t, ok)

	require.NoError(t, RegisterIncrementalImmutableInputs(parent, late))

	assert.True(t, childStorage.contains(reflect.ValueOf(late)))
}

func TestImmutableInputRegistrationFailsWithoutStorage(t *testing.T) {
	resource := map[string]any{"value": "resource"}

	require.ErrorContains(
		t,
		RegisterIncrementalImmutableInputs(t.Context(), resource),
		"storage is unavailable",
	)
	require.ErrorContains(
		t,
		RegisterIncrementalImmutableCertificate(t.Context(), CertifyIncrementalImmutableInputs(resource)),
		"storage is unavailable",
	)
	require.ErrorContains(
		t,
		BindImmutableResourceInputs(map[string]any{}, t.Context()),
		"storage is unavailable",
	)
	_, err := WithBoundImmutableResourceInputs(t.Context(), map[string]any{})
	require.ErrorContains(t, err, "storage is unavailable")
}

func TestRenderImmutableResourceInputsGuardAnUnpreparedContext(t *testing.T) {
	inherited := map[string]any{"value": "inherited"}
	templateContext := map[string]any{}

	ctx, err := withRenderImmutableResourceInputs(
		WithIncrementalImmutableInputs(t.Context(), inherited),
		templateContext,
	)
	require.NoError(t, err)

	storage, ok := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	require.True(t, ok)
	require.Same(t, storage, templateContext[immutableStorageTemplateContextKey])

	late := map[string]any{"value": "late"}
	require.NoError(t, RegisterIncrementalImmutableInputs(ctx, late))
	assert.True(t, storage.contains(reflect.ValueOf(inherited)))
	assert.True(t, storage.contains(reflect.ValueOf(late)))
}

func TestImmutableResourceInputsDoNotInheritIncrementalValues(t *testing.T) {
	incrementalValue := map[string]any{"value": "incremental"}
	parent := WithIncrementalImmutableInputs(t.Context(), incrementalValue)
	root := WithImmutableResourceInputs(parent)
	storage, ok := root.Value(immutableStorageContextKey{}).(*immutableStorage)
	require.True(t, ok)

	assert.False(t, storage.contains(reflect.ValueOf(incrementalValue)))
}

type incrementalImmutableResources struct {
	Values map[string]string
}

type incrementalImmutableMapKey struct {
	Value int
}

type incrementalImmutableResourcesWithMapKey struct {
	Values map[*incrementalImmutableMapKey]string
}

type incrementalImmutablePlanRegistrar struct{}

func (*incrementalImmutablePlanRegistrar) Profile(map[string]any) (string, error) {
	return "", nil
}

func (*incrementalImmutablePlanRegistrar) Backend(map[string]any, string) (string, error) {
	return "", nil
}

func (*incrementalImmutablePlanRegistrar) BackendWhenAny(
	map[string]any,
	string,
	string,
	[]string,
) (string, error) {
	return "", nil
}

func (*incrementalImmutablePlanRegistrar) FutureMethod() {}

type incrementalImmutableRootPlanRegistrar struct{}

func (*incrementalImmutableRootPlanRegistrar) Section(_, _, _ string) (string, error) {
	return "", nil
}

func (*incrementalImmutableRootPlanRegistrar) Profile(map[string]any) (string, error) {
	return "", nil
}

func (*incrementalImmutableRootPlanRegistrar) Backend(map[string]any, string) (string, error) {
	return "", nil
}

func (*incrementalImmutableRootPlanRegistrar) Fragment(string, TextFragment) (string, error) {
	return "", nil
}

func (*incrementalImmutableRootPlanRegistrar) ProfileGroup() string { return "" }

func (*incrementalImmutableRootPlanRegistrar) MapMeta(string, bool) error { return nil }
func (*incrementalImmutableRootPlanRegistrar) SensitiveMap(string) error  { return nil }

func (*incrementalImmutableRootPlanRegistrar) FutureMethod() {}

type incrementalImmutableFileRegistrar struct{}

func (*incrementalImmutableFileRegistrar) Register(...any) (string, error) { return "", nil }

func (*incrementalImmutableFileRegistrar) FutureMethod() {}

type incrementalImmutableAliasedSlices struct {
	Short []any
	Long  []any
}

type incrementalImmutableCertificateLeaf struct {
	Value  int
	Values []int
	Labels map[string]string
}

type incrementalImmutableCertificateResource struct {
	Value  int
	Nested incrementalImmutableCertificateLeaf
	Items  []incrementalImmutableCertificateLeaf
	Child  *incrementalImmutableCertificateLeaf
	Array  [1]incrementalImmutableCertificateLeaf
}

type incrementalImmutableOpaqueStore struct {
	state map[int]int
}

type incrementalImmutableSelector struct {
	retained map[string]any
}

type incrementalImmutableCapabilityValue struct {
	Value int
}

type incrementalImmutableCapabilitySurface struct {
	Exported *incrementalImmutableCapabilityValue
	hidden   *incrementalImmutableCapabilityValue
}

func (*incrementalImmutableSelector) Select(_, _, _ string) (value any, found bool, err error) {
	return "selected", true, nil
}

func (*incrementalImmutableSelector) SelectValues(_, _ string) ([]any, error) {
	return []any{"selected"}, nil
}

func (*incrementalImmutableSelector) Count(_, _ string) (int, error) {
	return 1, nil
}

func (*incrementalImmutableOpaqueStore) List() []any        { return nil }
func (*incrementalImmutableOpaqueStore) Fetch(...any) []any { return nil }
func (*incrementalImmutableOpaqueStore) GetSingle(...any) any {
	return nil
}

func (r *incrementalImmutableResourcesWithMapKey) First() *incrementalImmutableMapKey {
	for key := range r.Values {
		return key
	}
	return nil
}

func TestIncrementalComponentPreservesInheritedImmutableInputs(t *testing.T) {
	engine, err := New(map[string]string{
		"component": `{% resources.Values["value"] = "changed" %}`,
	}, &Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
		Declarations: map[string]any{
			"resources": (*incrementalImmutableResources)(nil),
		},
	})
	require.NoError(t, err)
	resources := &incrementalImmutableResources{Values: map[string]string{"value": "original"}}
	ctx := WithIncrementalImmutableInputs(t.Context(), resources)

	_, err = engine.RenderIncrementalComponent(ctx, "component", incrementalComponentContext(map[string]any{
		"resources": resources,
	}))
	require.ErrorContains(t, err, "mutates an immutable input")
	assert.Equal(t, "original", resources.Values["value"])
}

func TestIncrementalSharedSelectionDoesNotRejectReadOnlyReceiver(t *testing.T) {
	engine, err := New(map[string]string{
		"component": `{% var value, found = shared.Select("group", "cell", "key") %}{% if found %}{{ value }}{% end %}`,
	}, &Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
	})
	require.NoError(t, err)
	item := map[string]any{"value": "immutable"}
	values := incrementalComponentContext(map[string]any{"item": item})
	values["shared"] = NewSharedContributionContext(
		&noOpSharedContributionRecorder{},
		&incrementalImmutableSelector{retained: item},
	)

	output, err := engine.RenderIncrementalComponent(t.Context(), "component", values)
	require.NoError(t, err)
	assert.Equal(t, "selected", output)
}

func TestIncrementalPathResolverAllowsOnlyReadMethods(t *testing.T) {
	receiver := reflect.ValueOf(&PathResolver{})
	assert.True(t, nativeMethodPreservesImmutableInputs(receiver, "GetBaseDir"))
	assert.True(t, nativeMethodPreservesImmutableInputs(receiver, "GetPath"))
	assert.False(t, nativeMethodPreservesImmutableInputs(receiver, "FutureMethod"))
}

func TestIncrementalBackendPlanAllowsOnlyAuditedMethods(t *testing.T) {
	receiver := reflect.ValueOf(&incrementalImmutablePlanRegistrar{})
	assert.True(t, nativeMethodPreservesImmutableInputs(receiver, "Profile"))
	assert.True(t, nativeMethodPreservesImmutableInputs(receiver, "Backend"))
	assert.True(t, nativeMethodPreservesImmutableInputs(receiver, "BackendWhenAny"))
	assert.False(t, nativeMethodPreservesImmutableInputs(receiver, "FutureMethod"))
}

func TestRootRenderingAllowsOnlyAuditedNativeMethods(t *testing.T) {
	rootPlan := reflect.ValueOf(&incrementalImmutableRootPlanRegistrar{})
	for _, method := range []string{"Section", "Fragment", "Profile", "Backend", "ProfileGroup", "MapMeta", "SensitiveMap"} {
		assert.True(t, nativeMethodPreservesImmutableInputs(rootPlan, method), method)
	}
	assert.False(t, nativeMethodPreservesImmutableInputs(rootPlan, "FutureMethod"))

	fileRegistry := reflect.ValueOf(&incrementalImmutableFileRegistrar{})
	assert.True(t, nativeMethodPreservesImmutableInputs(fileRegistry, "Register"))
	assert.False(t, nativeMethodPreservesImmutableInputs(fileRegistry, "FutureMethod"))

	regexp := reflect.ValueOf(builtin.RegExp("value"))
	for _, method := range []string{
		"Match", "Find", "FindAll", "FindAllSubmatch", "FindSubmatch", "ReplaceAll", "ReplaceAllFunc", "Split",
	} {
		assert.True(t, nativeMethodPreservesImmutableInputs(regexp, method), method)
	}
	assert.False(t, nativeMethodPreservesImmutableInputs(regexp, "FutureMethod"))
}

func TestIncrementalImmutableInputsIncludeMapKeys(t *testing.T) {
	key := &incrementalImmutableMapKey{Value: 1}
	storageContext := WithIncrementalImmutableInputs(t.Context(), map[*incrementalImmutableMapKey]string{key: "value"})
	storage, ok := storageContext.Value(immutableStorageContextKey{}).(*immutableStorage)
	require.True(t, ok)

	assert.True(t, storage.contains(reflect.ValueOf(key)))
}

func TestIncrementalImmutableInputsTreatResourceStoresAsOpaque(t *testing.T) {
	internal := map[int]int{1: 1}
	store := &incrementalImmutableOpaqueStore{state: internal}
	controller := map[string]ResourceStore{"store": store}
	storageContext := WithIncrementalImmutableInputs(t.Context(), controller)
	storage, ok := storageContext.Value(immutableStorageContextKey{}).(*immutableStorage)
	require.True(t, ok)

	assert.True(t, storage.contains(reflect.ValueOf(controller)))
	assert.False(t, storage.contains(reflect.ValueOf(store)))
	assert.False(t, storage.contains(reflect.ValueOf(internal)))
}

func TestImmutableSharedReadsDoNotRejectReachableInputs(t *testing.T) {
	engine, err := New(map[string]string{
		"template": `{% var value = shared.Get("resource").(map[string]any) %}{% var other, computed = shared.ComputeIfAbsent("other", func() any { return "other" }) %}{{ value["value"] }}{{ other }}{{ computed }}`,
	}, &Options{EntryPoints: []string{"template"}})
	require.NoError(t, err)
	resource := map[string]any{"value": "original"}
	shared := NewSharedContext()
	_, _ = shared.ComputeIfAbsent("resource", func() any { return resource })
	ctx := WithImmutableResourceInputs(t.Context())
	require.NoError(t, RegisterIncrementalImmutableInputs(ctx, resource))
	templateContext := map[string]any{"shared": shared}
	require.NoError(t, BindImmutableResourceInputs(templateContext, ctx))

	output, err := engine.Render(ctx, "template", templateContext)

	require.NoError(t, err)
	assert.Equal(t, "originalothertrue\n", output)
}

func TestImmutableSharedReadDoesNotPermitReturnedInputMutation(t *testing.T) {
	engine, err := New(map[string]string{
		"template": `{% var value = shared.Get("resource").(map[string]any) %}{% value["value"] = "changed" %}`,
	}, &Options{EntryPoints: []string{"template"}})
	require.NoError(t, err)
	resource := map[string]any{"value": "original"}
	shared := NewSharedContext()
	_, _ = shared.ComputeIfAbsent("resource", func() any { return resource })
	ctx := WithImmutableResourceInputs(t.Context())
	require.NoError(t, RegisterIncrementalImmutableInputs(ctx, resource))
	templateContext := map[string]any{"shared": shared}
	require.NoError(t, BindImmutableResourceInputs(templateContext, ctx))

	_, err = engine.Render(ctx, "template", templateContext)

	require.ErrorContains(t, err, "mutates an immutable input")
	assert.Equal(t, "original", resource["value"])
}

func TestIncrementalImmutableInputsDoNotTraverseConcurrentResourceStoreState(t *testing.T) {
	engine, err := New(map[string]string{"component": "stable"}, &Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
	})
	require.NoError(t, err)
	store := &incrementalImmutableOpaqueStore{state: make(map[int]int, 1024)}
	for index := range 1024 {
		store.state[index] = index
	}
	controller := map[string]ResourceStore{"store": store}
	started := make(chan struct{})
	stop := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
				for index := range 1024 {
					store.state[index]++
				}
			}
		}
	}()
	<-started
	for range 1000 {
		output, renderErr := engine.RenderIncrementalComponent(
			t.Context(),
			"component",
			incrementalComponentContext(map[string]any{"controller": controller}),
		)
		require.NoError(t, renderErr)
		assert.Equal(t, "stable", output)
	}
	close(stop)
	wait.Wait()
}

func TestTemplateRejectsMutationReachableThroughMapKey(t *testing.T) {
	engine, err := New(map[string]string{
		"component": `{% var key = resources.First() %}{% key.Value = 2 %}`,
	}, &Options{
		EntryPoints: []string{"component"},
		Declarations: map[string]any{
			"resources": (*incrementalImmutableResourcesWithMapKey)(nil),
		},
	})
	require.NoError(t, err)
	key := &incrementalImmutableMapKey{Value: 1}
	resources := &incrementalImmutableResourcesWithMapKey{Values: map[*incrementalImmutableMapKey]string{key: "value"}}
	ctx := WithIncrementalImmutableInputs(t.Context(), resources)
	values := map[string]any{"resources": resources}
	require.NoError(t, BindImmutableResourceInputs(values, ctx))
	_, err = engine.Render(ctx, "component", values)
	require.ErrorContains(t, err, "mutates an immutable input")
	assert.Equal(t, 1, key.Value)
}

func TestIncrementalImmutableInputsTraverseEverySliceAlias(t *testing.T) {
	nested := map[string]any{"value": "original"}
	base := []any{"first", nested}
	resources := &incrementalImmutableAliasedSlices{Short: base[:1], Long: base[:2]}
	storageContext := WithIncrementalImmutableInputs(t.Context(), resources)
	storage, ok := storageContext.Value(immutableStorageContextKey{}).(*immutableStorage)
	require.True(t, ok)

	assert.True(t, storage.contains(reflect.ValueOf(nested)))
}

func TestIncrementalComponentRejectsMutationThroughLongerSliceAlias(t *testing.T) {
	engine, err := New(map[string]string{
		"component": `{% var nested = resources.Long[1].(map[string]interface{}) %}{% nested["value"] = "changed" %}`,
	}, &Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
		Declarations: map[string]any{
			"resources": (*incrementalImmutableAliasedSlices)(nil),
		},
	})
	require.NoError(t, err)
	nested := map[string]any{"value": "original"}
	base := []any{"first", nested}
	resources := &incrementalImmutableAliasedSlices{Short: base[:1], Long: base[:2]}
	ctx := WithIncrementalImmutableInputs(t.Context(), resources)

	_, err = engine.RenderIncrementalComponent(ctx, "component", incrementalComponentContext(map[string]any{
		"resources": resources,
	}))
	require.ErrorContains(t, err, "mutates an immutable input")
	assert.Equal(t, "original", nested["value"])
}

func TestIncrementalComponentRejectsMutationThroughHigherCapacitySliceAlias(t *testing.T) {
	engine, err := New(map[string]string{
		"component": `{% var tail = resources.Long[1:2] %}{% tail[0] = "changed" %}`,
	}, &Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
		Declarations: map[string]any{
			"resources": (*incrementalImmutableAliasedSlices)(nil),
		},
	})
	require.NoError(t, err)
	base := []any{"first", "original"}
	resources := &incrementalImmutableAliasedSlices{Short: base[:1:1], Long: base[:1:2]}
	ctx := WithIncrementalImmutableInputs(t.Context(), resources)

	_, err = engine.RenderIncrementalComponent(ctx, "component", incrementalComponentContext(map[string]any{
		"resources": resources,
	}))
	require.ErrorContains(t, err, "mutates an immutable input")
	assert.Equal(t, "original", base[1])
}

func TestIncrementalImmutableCertificatePreservesAliasesAndCycles(t *testing.T) {
	nested := map[string]any{"value": "original"}
	base := []any{"first", nested}
	aliases := &incrementalImmutableAliasedSlices{
		Short: base[:1:1],
		Long:  base[:2],
	}
	cycle := map[string]any{}
	cycle["self"] = cycle

	certificate := CertifyIncrementalImmutableInputs(aliases, cycle)

	assert.True(t, certificate.contains(reflect.ValueOf(nested)))
	assert.True(t, certificate.contains(reflect.ValueOf(base).Index(1)))
	assert.True(t, certificate.contains(reflect.ValueOf(cycle)))
}

func TestIncrementalImmutableCertificateRejectsTypedResourceMutation(t *testing.T) {
	tests := map[string]string{
		"root field":        `{% resource.Value = 2 %}`,
		"slice value field": `{% resource.Items[0].Value = 2 %}`,
		"pointer field":     `{% resource.Child.Value = 2 %}`,
		"slice element":     `{% resource.Nested.Values[0] = 2 %}`,
		"map value":         `{% resource.Nested.Labels["key"] = "changed" %}`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			resource := &incrementalImmutableCertificateResource{
				Value: 1,
				Nested: incrementalImmutableCertificateLeaf{
					Value: 1, Values: []int{1}, Labels: map[string]string{"key": "original"},
				},
				Items: []incrementalImmutableCertificateLeaf{{Value: 1}},
				Child: &incrementalImmutableCertificateLeaf{Value: 1},
				Array: [1]incrementalImmutableCertificateLeaf{{Value: 1}},
			}
			engine, err := New(map[string]string{"component": source}, &Options{
				EntryPoints: []string{"component"},
				Declarations: map[string]any{
					"resource": (*incrementalImmutableCertificateResource)(nil),
				},
			})
			require.NoError(t, err)
			certificate := CertifyIncrementalImmutableInputs([]*incrementalImmutableCertificateResource{resource})
			assert.True(t, certificate.contains(reflect.ValueOf(resource)))
			assert.True(t, certificate.contains(reflect.ValueOf(resource).Elem().FieldByName("Nested")))
			ctx := WithIncrementalImmutableCertificates(t.Context(), certificate)
			values := map[string]any{"resource": resource}
			require.NoError(t, BindImmutableResourceInputs(values, ctx))

			_, err = engine.Render(ctx, "component", values)

			require.ErrorContains(t, err, "mutates an immutable input")
			assert.Equal(t, 1, resource.Value)
			assert.Equal(t, 1, resource.Nested.Value)
			assert.Equal(t, []int{1}, resource.Nested.Values)
			assert.Equal(t, map[string]string{"key": "original"}, resource.Nested.Labels)
			assert.Equal(t, 1, resource.Items[0].Value)
			assert.Equal(t, 1, resource.Child.Value)
			assert.Equal(t, 1, resource.Array[0].Value)
		})
	}
}

func TestIncrementalImmutableCertificateRejectsPoisonedProvenance(t *testing.T) {
	newCertificate := func() (*IncrementalImmutableCertificate, []any) {
		anchor := []any{map[string]any{"value": "original"}}
		return CertifyIncrementalImmutableInputs(anchor), anchor
	}
	tests := map[string]func(*IncrementalImmutableCertificate) *IncrementalImmutableCertificate{
		"copy": func(certificate *IncrementalImmutableCertificate) *IncrementalImmutableCertificate {
			copied := *certificate
			return &copied
		},
		"seal": func(certificate *IncrementalImmutableCertificate) *IncrementalImmutableCertificate {
			certificate.seal = nil
			return certificate
		},
		"proof seal": func(certificate *IncrementalImmutableCertificate) *IncrementalImmutableCertificate {
			certificate.proof.seal = nil
			return certificate
		},
		"identity slots": func(certificate *IncrementalImmutableCertificate) *IncrementalImmutableCertificate {
			certificate.view.identitySlots = append([]immutableIdentity(nil), certificate.view.identitySlots...)
			return certificate
		},
		"identity mode": func(certificate *IncrementalImmutableCertificate) *IncrementalImmutableCertificate {
			certificate.view.identityIndex = !certificate.view.identityIndex
			return certificate
		},
		"ranges": func(certificate *IncrementalImmutableCertificate) *IncrementalImmutableCertificate {
			certificate.view.ranges = append([]immutableRange(nil), certificate.view.ranges...)
			return certificate
		},
		"anchors": func(certificate *IncrementalImmutableCertificate) *IncrementalImmutableCertificate {
			certificate.view.keep[0] = []any{map[string]any{"value": "foreign"}}
			return certificate
		},
	}
	for name, poison := range tests {
		t.Run(name, func(t *testing.T) {
			certificate, anchor := newCertificate()
			certificate = poison(certificate)

			assert.False(t, certificate.Guards(anchor))
			ctx := WithIncrementalImmutableCertificates(t.Context(), certificate)
			storage := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
			assert.False(t, storage.contains(reflect.ValueOf(anchor)))
		})
	}
}

func TestRegisteredIncrementalImmutableCertificateRejectsStructuralPoison(t *testing.T) {
	tests := map[string]func(*IncrementalImmutableCertificate){
		"certificate seal": func(certificate *IncrementalImmutableCertificate) {
			certificate.seal = nil
		},
		"certificate proof": func(certificate *IncrementalImmutableCertificate) {
			certificate.proof.seal = nil
		},
		"certificate view": func(certificate *IncrementalImmutableCertificate) {
			certificate.view = &incrementalImmutableCertificateView{}
		},
		"view seal": func(certificate *IncrementalImmutableCertificate) {
			certificate.view.seal = nil
		},
		"view proof": func(certificate *IncrementalImmutableCertificate) {
			certificate.view.proof.seal = nil
		},
	}
	for name, poison := range tests {
		t.Run(name, func(t *testing.T) {
			anchor := []any{map[string]any{"value": "original"}}
			certificate := CertifyIncrementalImmutableInputs(anchor)
			ctx := WithIncrementalImmutableCertificates(t.Context(), certificate)
			storage := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
			require.True(t, storage.contains(reflect.ValueOf(anchor[0])))

			poison(certificate)

			assert.False(t, storage.contains(reflect.ValueOf(anchor[0])))
		})
	}
}

func TestRegisteredIncrementalImmutableCertificateIndexCannotBeRedirected(t *testing.T) {
	poisons := map[string]func(*IncrementalImmutableCertificate, map[string]any){
		"identity slots header": func(
			certificate *IncrementalImmutableCertificate,
			foreign map[string]any,
		) {
			certificate.view.identitySlots = []immutableIdentity{{
				kind: reflect.Map,
				ptr:  reflect.ValueOf(foreign).Pointer(),
			}}
		},
		"identity slot entry": func(
			certificate *IncrementalImmutableCertificate,
			foreign map[string]any,
		) {
			certificate.view.identitySlots[0] = immutableIdentity{
				kind: reflect.Map,
				ptr:  reflect.ValueOf(foreign).Pointer(),
			}
		},
		"identity mode": func(certificate *IncrementalImmutableCertificate, _ map[string]any) {
			certificate.view.identityIndex = !certificate.view.identityIndex
		},
		"ranges header": func(certificate *IncrementalImmutableCertificate, _ map[string]any) {
			certificate.view.ranges = []immutableRange{{start: 1, end: ^uintptr(0)}}
		},
		"range entry": func(certificate *IncrementalImmutableCertificate, _ map[string]any) {
			certificate.view.ranges[0] = immutableRange{start: 1, end: ^uintptr(0)}
		},
	}
	for name, poison := range poisons {
		t.Run(name, func(t *testing.T) {
			anchor := []any{map[string]any{"value": "original"}}
			foreign := map[string]any{"value": "foreign"}
			certificate := CertifyIncrementalImmutableInputs(anchor)
			ctx := WithIncrementalImmutableCertificates(t.Context(), certificate)
			storage := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
			require.True(t, storage.contains(reflect.ValueOf(anchor[0])))
			require.False(t, storage.contains(reflect.ValueOf(foreign)))

			poison(certificate, foreign)

			assert.True(t, storage.contains(reflect.ValueOf(anchor[0])))
			assert.False(t, storage.contains(reflect.ValueOf(foreign)))
			assert.False(t, certificate.Guards(anchor))
		})
	}
}

func TestRegisteredIncrementalImmutableCertificateCannotBeRedirected(t *testing.T) {
	anchor := []any{map[string]any{"value": "original"}}
	foreign := []any{map[string]any{"value": "foreign"}}
	certificate := CertifyIncrementalImmutableInputs(anchor)
	ctx := WithIncrementalImmutableCertificates(t.Context(), certificate)
	storage := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	require.True(t, storage.contains(reflect.ValueOf(anchor[0])))
	require.False(t, storage.contains(reflect.ValueOf(foreign[0])))

	certificate.view.keep[0] = foreign

	assert.True(t, storage.contains(reflect.ValueOf(anchor[0])))
	assert.False(t, storage.contains(reflect.ValueOf(foreign[0])))
	assert.False(t, certificate.Guards(anchor))
	assert.Equal(
		t,
		reflect.ValueOf(anchor).Pointer(),
		reflect.ValueOf(certificate.view.proof.retained[0]).Pointer(),
	)
}

func TestIncrementalImmutableCertificateRetainsExactAnchor(t *testing.T) {
	anchor := []any{map[string]any{"value": "same"}}
	foreign := []any{map[string]any{"value": "same"}}
	certificate := CertifyIncrementalImmutableInputs(anchor)

	assert.True(t, certificate.Guards(anchor))
	assert.False(t, certificate.Guards(foreign))
	assert.True(t, certificate.contains(reflect.ValueOf(anchor[0])))
	assert.False(t, certificate.contains(reflect.ValueOf(foreign[0])))
	assert.Equal(
		t,
		reflect.ValueOf(anchor[0]).Pointer(),
		reflect.ValueOf(certificate.view.keep[0].([]any)[0]).Pointer(),
	)
}

func TestIncrementalImmutableCertificateConcurrentAuthentication(t *testing.T) {
	anchor := []any{map[string]any{"value": "original"}}
	certificate := CertifyIncrementalImmutableInputs(anchor)
	target := reflect.ValueOf(anchor[0])
	const workerCount = 64
	failed := make(chan struct{}, workerCount)
	var wait sync.WaitGroup
	for range workerCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 1000 {
				if !certificate.Guards(anchor) || !certificate.contains(target) {
					failed <- struct{}{}
					return
				}
			}
		}()
	}
	wait.Wait()
	close(failed)
	assert.Empty(t, failed)
}

func TestIncrementalImmutableCertificateTreatsResourceStoresAsOpaque(t *testing.T) {
	internal := map[int]int{1: 1}
	store := &incrementalImmutableOpaqueStore{state: internal}
	controller := map[string]ResourceStore{"store": store}

	certificate := CertifyIncrementalImmutableInputs(controller)

	assert.True(t, certificate.contains(reflect.ValueOf(controller)))
	assert.False(t, certificate.contains(reflect.ValueOf(store)))
	assert.False(t, certificate.contains(reflect.ValueOf(internal)))
}

func TestIncrementalImmutableCertificateRegistrationIsConcurrent(t *testing.T) {
	base := map[string]any{"value": "base"}
	ctx := WithImmutableResourceInputs(t.Context())
	baseCertificate := CertifyIncrementalImmutableInputs(base)
	require.NoError(t, RegisterIncrementalImmutableCertificate(ctx, baseCertificate))
	require.NoError(t, RegisterIncrementalImmutableCertificate(ctx, baseCertificate))
	storage := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	require.Equal(t, 1, storage.certifiedCount+storage.certified.len())
	registered := make([]map[string]any, 128)
	var wait sync.WaitGroup
	for index := range registered {
		registered[index] = map[string]any{"value": index}
		wait.Add(1)
		go func(value map[string]any) {
			defer wait.Done()
			assert.NoError(t, RegisterIncrementalImmutableCertificate(ctx, CertifyIncrementalImmutableInputs(value)))
			assert.True(t, storage.contains(reflect.ValueOf(base)))
		}(registered[index])
	}
	wait.Wait()
	for _, value := range registered {
		assert.True(t, storage.contains(reflect.ValueOf(value)))
	}
}

func TestImmutableCertificateVisitsPreserveExactIdentitiesAndExtents(t *testing.T) {
	visits := immutableCertificateVisits{}
	for pointer := uintptr(1); pointer <= 4096; pointer++ {
		identity := immutableIdentity{kind: reflect.Kind(pointer%uintptr(reflect.UnsafePointer) + 1), ptr: pointer * 16}
		extent := immutableReferenceExtent{length: int(pointer), capacity: int(pointer) * 2}
		visits.set(identity, extent)
	}
	for pointer := uintptr(1); pointer <= 4096; pointer++ {
		identity := immutableIdentity{kind: reflect.Kind(pointer%uintptr(reflect.UnsafePointer) + 1), ptr: pointer * 16}
		extent, found := visits.get(identity)
		require.True(t, found)
		assert.Equal(t, immutableReferenceExtent{length: int(pointer), capacity: int(pointer) * 2}, extent)
	}
	identity := immutableIdentity{kind: reflect.Kind(1%uintptr(reflect.UnsafePointer) + 1), ptr: 16}
	visits.set(identity, immutableReferenceExtent{length: 7, capacity: 9})
	extent, found := visits.get(identity)
	require.True(t, found)
	assert.Equal(t, immutableReferenceExtent{length: 7, capacity: 9}, extent)
	assert.Equal(t, 4096, visits.count)
}

func TestImmutableCertificateVisitsPreserveSmallAndPromotedCertificates(t *testing.T) {
	smallCapacity := len((immutableCertificateVisits{}).smallIdentities)
	for _, count := range []int{1, smallCapacity, smallCapacity + 1} {
		t.Run(fmt.Sprintf("identities=%d", count), func(t *testing.T) {
			visits := immutableCertificateVisits{}
			identities := make([]immutableIdentity, count)
			for index := range identities {
				identities[index] = immutableIdentity{kind: reflect.Map, ptr: uintptr(index + 1)}
				visits.set(identities[index], immutableReferenceExtent{})
			}
			certificate := newIncrementalImmutableCertificate(
				visits.certificateIdentities(), visits.indexed, nil, nil,
			)
			for _, identity := range identities {
				assert.True(t, certificate.containsIdentity(identity))
			}
			assert.False(t, certificate.containsIdentity(immutableIdentity{kind: reflect.Map, ptr: 1024}))
			assert.Equal(t, count > len(visits.smallIdentities), certificate.view.identityIndex)
		})
	}
}

func TestImmutableStoragePreservesSmallAndPromotedIdentitySets(t *testing.T) {
	smallCapacity := len((immutableStorage{}).identitySmall)
	for _, count := range []int{1, smallCapacity, smallCapacity + 1} {
		t.Run(fmt.Sprintf("identities=%d", count), func(t *testing.T) {
			storage := &immutableStorage{}
			identities := make([]immutableIdentity, count)
			for index := range identities {
				identities[index] = immutableIdentity{kind: reflect.Map, ptr: uintptr(index + 1)}
				storage.addIdentity(identities[index])
				storage.addIdentity(identities[index])
			}
			for _, identity := range identities {
				assert.True(t, storage.hasIdentity(identity))
			}
			assert.False(t, storage.hasIdentity(immutableIdentity{kind: reflect.Map, ptr: 1024}))
			assert.Equal(t, count > smallCapacity, storage.identities != nil)
		})
	}
}

func TestIncrementalCapabilityGuardsOnlyTemplateVisiblePointers(t *testing.T) {
	exported := &incrementalImmutableCapabilityValue{Value: 1}
	hidden := &incrementalImmutableCapabilityValue{Value: 2}
	capability := &incrementalImmutableCapabilitySurface{Exported: exported, hidden: hidden}
	storage := &immutableStorage{}

	storage.addCapabilities(capability)

	assert.True(t, storage.contains(reflect.ValueOf(capability)))
	assert.True(t, storage.contains(reflect.ValueOf(exported)))
	assert.True(t, storage.contains(reflect.ValueOf(exported).Elem().FieldByName("Value")))
	assert.False(t, storage.contains(reflect.ValueOf(hidden)))
}

func TestIncrementalComponentRejectsMutationThroughVisibleCapabilityPointer(t *testing.T) {
	engine, err := New(map[string]string{
		"component": `{% capability.Exported.Value = 2 %}`,
	}, &Options{
		EntryPoints: []string{"component"},
		Declarations: map[string]any{
			"capability": (*incrementalImmutableCapabilitySurface)(nil),
		},
	})
	require.NoError(t, err)
	capability := &incrementalImmutableCapabilitySurface{
		Exported: &incrementalImmutableCapabilityValue{Value: 1},
		hidden:   &incrementalImmutableCapabilityValue{Value: 2},
	}
	ctx := WithIncrementalImmutableCapabilityInputs(t.Context(), capability)
	values := map[string]any{"capability": capability}
	require.NoError(t, BindImmutableResourceInputs(values, ctx))

	_, err = engine.Render(ctx, "component", values)

	require.ErrorContains(t, err, "mutates an immutable input")
	assert.Equal(t, 1, capability.Exported.Value)
}

func TestImmutableVisitSetPreservesSmallAndPromotedVisits(t *testing.T) {
	smallCapacity := len((immutableVisitSet{}).small)
	for _, count := range []int{1, smallCapacity, smallCapacity + 1} {
		t.Run(fmt.Sprintf("visits=%d", count), func(t *testing.T) {
			seen := immutableVisitSet{}
			visits := make([]immutableVisit, count)
			for index := range visits {
				visits[index] = immutableVisit{
					identity: immutableIdentity{kind: reflect.Slice, ptr: uintptr(index + 1)},
					length:   index,
					capacity: index + 1,
				}
				assert.True(t, seen.add(visits[index]))
				assert.False(t, seen.add(visits[index]))
			}
			assert.Equal(t, count > smallCapacity, seen.values != nil)
		})
	}
}

func TestIncrementalComponentRejectsMutationThroughCertificate(t *testing.T) {
	engine, err := New(map[string]string{
		"component": `{% item["value"] = "changed" %}`,
	}, &Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
	})
	require.NoError(t, err)
	item := map[string]any{"value": "original"}
	ctx := WithIncrementalImmutableCertificates(t.Context(), CertifyIncrementalImmutableInputs(item))

	_, err = engine.RenderIncrementalComponent(ctx, "component", incrementalComponentContext(map[string]any{
		"item": item,
	}))

	require.ErrorContains(t, err, "mutates an immutable input")
	assert.Equal(t, "original", item["value"])
}

var incrementalImmutableCertificateSink *IncrementalImmutableCertificate
var incrementalImmutableStorageSink *immutableStorage
var incrementalImmutableContextSink any
var incrementalImmutableContainsSink bool

func BenchmarkCertifyIncrementalImmutableInputs(b *testing.B) {
	resources := make([]any, 64)
	for index := range resources {
		resources[index] = map[string]any{
			"metadata": map[string]any{
				"namespace": "default",
				"name":      fmt.Sprintf("route-%d", index),
			},
			"spec": map[string]any{
				"hostnames": []any{fmt.Sprintf("route-%d.example.com", index)},
				"rules": []any{map[string]any{
					"backendRefs": []any{map[string]any{"name": fmt.Sprintf("svc-%d", index)}},
				}},
			},
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		incrementalImmutableCertificateSink = CertifyIncrementalImmutableInputs(resources)
	}
}

func BenchmarkCertifySmallIncrementalImmutableInput(b *testing.B) {
	value := map[string]any{"value": "item"}
	b.ReportAllocs()
	for range b.N {
		incrementalImmutableCertificateSink = CertifyIncrementalImmutableInputs(value)
	}
}

func BenchmarkCollectIncrementalImmutableInputs(b *testing.B) {
	resources := make([]any, 64)
	for index := range resources {
		resources[index] = map[string]any{
			"metadata": map[string]any{"namespace": "default", "name": fmt.Sprintf("route-%d", index)},
			"spec": map[string]any{
				"hostnames": []any{fmt.Sprintf("route-%d.example.com", index)},
				"rules": []any{map[string]any{
					"backendRefs": []any{map[string]any{"name": fmt.Sprintf("svc-%d", index)}},
				}},
			},
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		storage := &immutableStorage{identities: map[immutableIdentity]struct{}{}}
		storage.add(resources)
		incrementalImmutableStorageSink = storage
	}
}

func BenchmarkAttachIncrementalImmutableCertificates(b *testing.B) {
	item := map[string]any{"value": "item"}
	props := map[string]any{"value": "props"}
	subject := map[string]any{"mode": "reconcile"}
	itemCertificate := CertifyIncrementalImmutableInputs(item)
	propsCertificate := CertifyIncrementalImmutableInputs(props)
	subjectCertificate := CertifyIncrementalImmutableInputs(subject)
	ctx := b.Context()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		incrementalImmutableContextSink = WithIncrementalImmutableCertificates(
			ctx,
			itemCertificate,
			propsCertificate,
			subjectCertificate,
		)
	}
}

func BenchmarkCollectIncrementalImmutableCapability(b *testing.B) {
	capability := &incrementalImmutableCapabilitySurface{
		Exported: &incrementalImmutableCapabilityValue{Value: 1},
		hidden:   &incrementalImmutableCapabilityValue{Value: 2},
	}
	b.ReportAllocs()
	for range b.N {
		storage := &immutableStorage{}
		storage.addCapabilities(capability)
		incrementalImmutableStorageSink = storage
	}
}

func BenchmarkIncrementalImmutableCertificateContains(b *testing.B) {
	item := map[string]any{"nested": map[string]any{"value": "item"}}
	ctx := WithIncrementalImmutableCertificates(
		b.Context(),
		CertifyIncrementalImmutableInputs(item),
		CertifyIncrementalImmutableInputs(map[string]any{"value": "props"}),
		CertifyIncrementalImmutableInputs(map[string]any{"mode": "reconcile"}),
	)
	storage := ctx.Value(immutableStorageContextKey{}).(*immutableStorage)
	target := reflect.ValueOf(item["nested"])
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		incrementalImmutableContainsSink = storage.contains(target)
	}
}

func BenchmarkIncrementalImmutableCertificateContainsFullAuthentication(b *testing.B) {
	item := map[string]any{"nested": map[string]any{"value": "item"}}
	certificate := CertifyIncrementalImmutableInputs(item)
	target := reflect.ValueOf(item["nested"])
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		incrementalImmutableContainsSink = certificate.contains(target)
	}
}

// collectMap skips boxing a key whose kind collect would ignore. That is only
// equivalent while this helper matches collect's switch exactly — if collect
// learns to descend a new kind and this does not, keys of that kind stop being
// visited and an immutability violation goes unseen.
func TestImmutableKindHoldsReferencesMatchesCollect(t *testing.T) {
	descended := map[reflect.Kind]bool{
		reflect.Pointer: true, reflect.Map: true, reflect.Slice: true,
		reflect.Array: true, reflect.Struct: true,
		// collect unwraps an interface before switching, so it may reach any
		// of the above through one.
		reflect.Interface: true,
	}
	for kind := reflect.Invalid; kind <= reflect.UnsafePointer; kind++ {
		want := descended[kind]
		if got := immutableKindHoldsReferences(kind); got != want {
			t.Errorf("immutableKindHoldsReferences(%v) = %v, want %v — it has drifted from collect's switch",
				kind, got, want)
		}
	}
}
