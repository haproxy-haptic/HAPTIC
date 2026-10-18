package rendercontext

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type auditResourceMetadata struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type auditResourceSpec struct {
	Value string `json:"value"`
}

type auditResource struct {
	Metadata auditResourceMetadata `json:"metadata"`
	Spec     auditResourceSpec     `json:"spec"`
}

type auditResourceView struct {
	mu         sync.Mutex
	generation int
	static     map[string]any
}

func (v *auditResourceView) List(string, stores.Store) ([]any, error) {
	return v.values(), nil
}

func (v *auditResourceView) Get(string, stores.Store, ...string) ([]any, error) {
	return v.values(), nil
}

func (*auditResourceView) NormalizeLookupKeys(string, []any) ([]string, error) {
	return []string{"default", "route"}, nil
}

func (v *auditResourceView) values() []any {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.static != nil {
		return []any{v.static}
	}
	v.generation++
	return []any{map[string]any{
		"metadata": map[string]any{"namespace": "default", "name": "route"},
		"spec":     map[string]any{"value": strconv.Itoa(v.generation)},
	}}
}

type auditSharedRecorder struct{}

func (*auditSharedRecorder) Unique(string, string, string) {}

func auditResourceSurface(
	t *testing.T,
	ctx context.Context,
	elemType reflect.Type,
	view StoreSnapshotView,
	incremental bool,
) any {
	t.Helper()
	typed := map[string]reflect.Type{}
	if elemType != nil {
		typed["routes"] = elemType
	}
	build := BuildResourcesValueWithViews
	if incremental {
		build = BuildIncrementalResourcesValueWithViews
	}
	return build(
		ctx,
		map[string]stores.Store{"routes": k8sstore.NewMemoryStore(2)},
		typed,
		[]string{"routes"},
		func(string) []string { return []string{"metadata.namespace", "metadata.name"} },
		func(string) bool { return false },
		func(string) string { return "audit/v1" }, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewResourceErrorCollector(),
		view,
		nil,
		false,
	)
}

func TestTypedSnapshotViewDoesNotReuseDeadMapMemoEntries(t *testing.T) {
	ctx := templating.WithIncrementalImmutableInputs(t.Context())
	view := &auditResourceView{}
	resources := auditResourceSurface(t, ctx, reflect.TypeFor[auditResource](), view, false)
	resource := reflect.ValueOf(resources).Elem().Field(0).Elem()
	getSingle := resource.FieldByName("GetSingle")

	for generation := 1; generation <= 5000; generation++ {
		result := getSingle.CallSlice([]reflect.Value{reflect.ValueOf([]any{"default", "route"})})[0]
		got := result.Interface().(*auditResource).Spec.Value
		require.Equal(t, strconv.Itoa(generation), got)
		if generation%8 == 0 {
			runtime.GC()
		}
	}
}

func TestLateResourceRegistrationRejectsAllReadMutations(t *testing.T) {
	tests := []struct {
		name     string
		typed    bool
		template string
	}{
		{"typed List", true, `{%% values := resources.routes.List(); values[0].Spec.Value = "changed" %%}{{ values[0].Spec.Value }}`},
		{"typed Fetch", true, `{%% values := resources.routes.Fetch("default", "route"); values[0].Spec.Value = "changed" %%}{{ values[0].Spec.Value }}`},
		{"typed GetSingle", true, `{%% value := resources.routes.GetSingle("default", "route"); value.Spec.Value = "changed" %%}{{ value.Spec.Value }}`},
		{"untyped List", false, `{%% values := resources.routes.List(); values[0].(map[string]any)["spec"].(map[string]any)["value"] = "changed" %%}`},
		{"untyped Fetch", false, `{%% values := resources.routes.Fetch("default", "route"); values[0].(map[string]any)["spec"].(map[string]any)["value"] = "changed" %%}`},
		{"untyped GetSingle", false, `{%% value := resources.routes.GetSingle("default", "route"); value.(map[string]any)["spec"].(map[string]any)["value"] = "changed" %%}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := map[string]any{}
			props := map[string]any{}
			renderSubject := map[string]any{"mode": "reconcile"}
			parent := templating.WithIncrementalImmutableInputs(t.Context(), item, props, renderSubject)
			source := map[string]any{
				"metadata": map[string]any{"namespace": "default", "name": "route"},
				"spec":     map[string]any{"value": "original"},
			}
			view := &auditResourceView{static: source}
			var elemType reflect.Type
			if test.typed {
				elemType = reflect.TypeFor[auditResource]()
			}
			declarationValue := auditResourceSurface(t, parent, elemType, view, false)
			declaration := reflect.Zero(reflect.TypeOf(declarationValue)).Interface()
			templating.RegisterIncrementalResourceDeclaration(declaration)
			resources := auditResourceSurface(t, parent, elemType, view, true)
			declarations := map[string]any{
				"resources": declaration,
			}
			engine, err := templating.New(map[string]string{"component": test.template}, &templating.Options{
				EntryPoints:            []string{"component"},
				IncrementalEntryPoints: []string{"component"},
				Declarations:           declarations,
			})
			require.NoError(t, err)
			child := templating.WithIncrementalImmutableInputs(parent, resources)
			_, err = engine.RenderIncrementalComponent(child, "component", map[string]any{
				"source":        "routes",
				"item":          item,
				"props":         props,
				"renderSubject": renderSubject,
				"resources":     resources,
				"shared":        templating.NewSharedContributionContext(&auditSharedRecorder{}),
			})
			require.ErrorContains(t, err, "mutates an immutable input")
			require.Equal(t, "original", source["spec"].(map[string]any)["value"])
		})
	}
}

func TestControllerReadResultsRejectAllMutations(t *testing.T) {
	tests := []struct {
		name     string
		memoize  bool
		template string
	}{
		{"unmemoized List", false, `{%% values := controller["routes"].List(); values[0].(map[string]any)["spec"].(map[string]any)["value"] = "changed" %%}`},
		{"unmemoized Fetch", false, `{%% values := controller["routes"].Fetch("default", "route"); values[0].(map[string]any)["spec"].(map[string]any)["value"] = "changed" %%}`},
		{"unmemoized GetSingle", false, `{%% value := controller["routes"].GetSingle("default", "route"); value.(map[string]any)["spec"].(map[string]any)["value"] = "changed" %%}`},
		{"memoized List", true, `{%% values := controller["routes"].List(); values[0].(map[string]any)["spec"].(map[string]any)["value"] = "changed" %%}`},
		{"memoized Fetch", true, `{%% values := controller["routes"].Fetch("default", "route"); values[0].(map[string]any)["spec"].(map[string]any)["value"] = "changed" %%}`},
		{"memoized GetSingle", true, `{%% value := controller["routes"].GetSingle("default", "route"); value.(map[string]any)["spec"].(map[string]any)["value"] = "changed" %%}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := map[string]any{}
			props := map[string]any{}
			renderSubject := map[string]any{"mode": "reconcile"}
			componentCtx := templating.WithIncrementalImmutableInputs(
				t.Context(), item, props, renderSubject,
			)
			source := map[string]any{
				"metadata": map[string]any{"namespace": "default", "name": "route"},
				"spec":     map[string]any{"value": "original"},
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			wrapper := (&StoreWrapper{
				Store:          k8sstore.NewMemoryStore(2),
				ResourceType:   "routes",
				Logger:         logger,
				readContext:    t.Context(),
				resourceErrors: NewResourceErrorCollector(),
				IndexBy:        []string{"metadata.namespace", "metadata.name"},
			}).CloneWithSnapshotViewContext(componentCtx, &auditResourceView{static: source}, test.memoize)
			controller := map[string]templating.ResourceStore{"routes": wrapper}
			engine, err := templating.New(map[string]string{"component": test.template}, &templating.Options{
				EntryPoints:            []string{"component"},
				IncrementalEntryPoints: []string{"component"},
				Declarations: map[string]any{
					"controller": map[string]templating.ResourceStore{},
				},
			})
			require.NoError(t, err)

			_, err = engine.RenderIncrementalComponent(componentCtx, "component", map[string]any{
				"source":        "routes",
				"item":          item,
				"props":         props,
				"renderSubject": renderSubject,
				"controller":    controller,
				"shared":        templating.NewSharedContributionContext(&auditSharedRecorder{}),
			})
			require.ErrorContains(t, err, "mutates an immutable input")
			got := wrapper.GetSingle("default", "route").(map[string]any)
			require.Equal(t, "original", got["spec"].(map[string]any)["value"])
		})
	}
}
