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

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"time"

	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validator"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/schemafetcher"
)

// typeBootstrapFetchTimeout bounds the entire bootstrap's wall-clock
// cost, discovery included. The bundled chart's bootstrap is about
// five apiserver round trips deep (150 ms at a 25 ms round trip,
// #410), so the deadline only fires when the apiserver is degraded,
// at which point the deadline-exceeded error fails the iteration
// startup with a clear signal for operators (no silent degradation
// to envelope-only types; see typebootstrap.Bootstrap's fail-closed
// contract).
const typeBootstrapFetchTimeout = 2 * time.Second

// typedRendererWiring carries the two outputs the renderer needs
// from typebootstrap: the engine-side declarations (consumed at
// engine construction to register typed globals) and the
// per-resource Go types (consumed at render time to wrap each
// store's snapshot into the declared shape). Bundling them in one
// struct keeps the two facets travelling together — they're
// always populated from the same Bootstrap call and always need
// the same lifetime.
type typedRendererWiring struct {
	// Declarations is the additionalDeclarations map for
	// helpers.NewEngineFromConfigWithOptions. Includes the
	// per-resource typed globals AND the currentConfig
	// declaration that the slot-preservation logic depends on.
	// Always non-nil; empty when no watched resource resolved.
	Declarations map[string]any

	// TypedResourceTypes is the per-resource generated Go type,
	// keyed by the same user-facing name templates use to reach
	// the resource. Fed to RenderService so it can wrap each
	// store's snapshot into the declared shape at render time.
	// Always non-nil; empty when no watched resource resolved.
	TypedResourceTypes map[string]reflect.Type
}

// buildEngineWiring runs the type-bootstrap pipeline and returns
// the engine + renderer wiring artefacts the reconciliation setup
// needs:
//
//   - Declarations: additionalDeclarations for the engine
//     constructor (typed globals + the currentConfig declaration
//     the slot-preservation logic depends on).
//   - TypedResourceTypes: per-resource Go types for the renderer
//     to wrap each snapshot into the typed shape at render time.
//
// Extracted from createReconciliationComponents to keep that
// function under the per-function statement budget. Any
// per-resource schema-acquisition failure aborts the iteration
// (typebootstrap.Bootstrap is fail-closed) so the operator gets
// a clear signal to investigate RBAC / CRD installation /
// apiserver health rather than running with silently degraded
// typed access.
func buildEngineWiring(
	ctx context.Context,
	cfg *config.Config,
	k8sClient *client.Client,
	logger *slog.Logger,
) (typedRendererWiring, error) {
	result, err := runTypeBootstrap(ctx, cfg, k8sClient, logger)
	if err != nil {
		// A hard failure here means a fundamental client problem
		// (no apiextensions reachability, no rest.Config) — not
		// the per-resource degradation typebootstrap fail-opens
		// on. Don't try to start the engine without a stable
		// client surface.
		return typedRendererWiring{}, fmt.Errorf("bootstrapping typed watched resources: %w", err)
	}
	wiring := typedRendererWiring{
		Declarations:       helpers.BuildAdditionalDeclarations(cfg, result),
		TypedResourceTypes: map[string]reflect.Type{},
	}
	for name, t := range result.Types {
		wiring.TypedResourceTypes[name] = t
	}
	return wiring, nil
}

// runTypeBootstrap is the iteration-time entry point that drives
// Phase 3 of the typed-watched-resources pipeline. It returns the
// raw typebootstrap.Result so callers can choose how to consume
// the Types map (the engine declarations come from
// typebootstrap.BuildEngineDeclarations; the renderer also wants
// the bare Types map for its render-time wrapping).
//
// All schema acquisition happens through the cluster — see
// typebootstrap_clients.go for the apiextensions / discovery
// adapters. The schemafetcher and typegen packages themselves
// never touch the K8s client; they receive plain interfaces and
// data shapes from this wiring layer.
func runTypeBootstrap(
	ctx context.Context,
	cfg *config.Config,
	k8sClient *client.Client,
	logger *slog.Logger,
) (*typebootstrap.Result, error) {
	if cfg == nil || len(cfg.WatchedResources) == 0 {
		// No watched resources declared yet. Return an
		// empty-but-non-nil Result so callers don't have to
		// nil-check.
		return &typebootstrap.Result{
			Types:  map[string]reflect.Type{},
			Errors: map[string]error{},
		}, nil
	}

	apiextClient, err := apiextensionsclientset.NewForConfig(k8sClient.RestConfig())
	if err != nil {
		// Failing the whole boot here is the right call: every
		// watched resource needs schema acquisition, and a
		// broken apiextensions client means we have no path to
		// any CRD's schema. Operator should investigate the
		// rest.Config (RBAC, host reachability).
		return nil, fmt.Errorf("constructing apiextensions client for type bootstrap: %w", err)
	}

	// One discovery fetch serves both Kind resolution and every CRD lookup.
	d := memory.NewMemCacheClient(k8sClient.Clientset().Discovery())
	return RunTypeBootstrap(ctx, cfg, NewClusterSchemaFetcher(apiextClient, d), d, logger)
}

// RunTypeBootstrap is the schema-source-agnostic core of the live type
// bootstrap: GVK resolution through the supplied discovery interface,
// schema acquisition through the supplied fetcher. The controller's
// iteration startup reaches it via runTypeBootstrap (which builds both
// from the iteration's K8s client); a CLI can call it directly with clients
// built from the operator's kubeconfig.
func RunTypeBootstrap(
	ctx context.Context,
	cfg *config.Config,
	fetcher schemafetcher.Fetcher,
	d discovery.DiscoveryInterface,
	logger *slog.Logger,
) (*typebootstrap.Result, error) {
	if cfg == nil || len(cfg.WatchedResources) == 0 {
		return &typebootstrap.Result{
			Types:  map[string]reflect.Type{},
			Errors: map[string]error{},
		}, nil
	}

	// Cap the bootstrap's wall-clock cost so a slow apiserver can't
	// stall Stage 5 indefinitely. Every request below, discovery
	// included, honours ctx. Deadline-exceeded surfaces as a hard
	// bootstrap error (Bootstrap is fail-closed) — iteration
	// startup aborts with a clear signal rather than running with
	// silently degraded typed access.
	ctx, cancel := context.WithTimeout(ctx, typeBootstrapFetchTimeout)
	defer cancel()

	resources, err := buildBootstrapResources(ctx, cfg, d, logger)
	if err != nil {
		return nil, fmt.Errorf("type bootstrap: %w", err)
	}

	result, err := typebootstrap.Bootstrap(ctx, typebootstrap.Config{
		Resources:          resources,
		GlobalIgnoreFields: cfg.WatchedResourcesIgnoreFields,
		Fetcher:            fetcher,
		Logger:             logger,
	})
	if err != nil {
		return nil, fmt.Errorf("type bootstrap: %w", err)
	}

	// Always log at info level, even when every resource
	// succeeded. Two reasons:
	//
	//   - Operator visibility: the typed-watched-resources path
	//     is silent today (no chart template uses it yet), so
	//     without a successful-boot log nothing in the runtime
	//     indicates that the pipeline ran at all.
	//
	//   - CI / acceptance test inspection: when adoption begins
	//     (Phase 7, chart-side), reviewers need a single grep
	//     target to confirm a particular resource got typed.
	//     "type bootstrap" is the operator-facing identifier;
	//     downstream code searches anchor on it.
	logger.Info("Type bootstrap completed",
		"typed_count", len(result.Types),
		"degraded_count", len(result.Errors),
		"typed_resources", typedResourceNames(result))

	return result, nil
}

// typedResourceNames extracts the resource names whose typed view
// resolved successfully, sorted for deterministic log output. Lifts
// the small projection out of the log call so the format-args stay
// readable.
func typedResourceNames(result *typebootstrap.Result) []string {
	if result == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(result.Types))
}

// buildBootstrapResources translates HAProxyTemplateConfig.spec.
// watchedResources entries into the GVK-shaped slice typebootstrap
// expects. The (apiVersion, resources-plural) → Kind resolution
// goes through a RESTMapper built from the cluster's discovery —
// the same approach pkg/controller/webhook.go uses for its
// admission rules.
//
// A resource whose Kind can't be resolved is skipped with a
// warning rather than failing the whole boot. The chart keeps the
// generic map-backed access for that resource (just no typed
// shortcut). The most common reason for a miss is a misconfigured
// apiVersion — operator-visible via the warn log and easier to
// debug than a hard boot failure. A failed discovery is returned
// instead: skipping on it would strip typed access from every
// resource because the apiserver was slow.
func buildBootstrapResources(
	ctx context.Context,
	cfg *config.Config,
	d discovery.DiscoveryInterface,
	logger *slog.Logger,
) ([]typebootstrap.Resource, error) {
	cached, ok := d.(discovery.CachedDiscoveryInterface)
	if !ok {
		cached = memory.NewMemCacheClient(d)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapperWithContext(discovery.ToCachedDiscoveryInterfaceWithContext(cached))

	out := make([]typebootstrap.Resource, 0, len(cfg.WatchedResources))
	for name := range cfg.WatchedResources {
		wr := cfg.WatchedResources[name]
		gvk, err := resolveKind(ctx, mapper, &wr)
		if errors.Is(err, errKindUnresolvable) {
			logger.Warn("Type bootstrap: skipping resource whose Kind couldn't be resolved",
				"resource", name,
				"apiVersion", wr.APIVersion,
				"resources", wr.Resources,
				"error", err)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("watched resource %q: %w", name, err)
		}
		out = append(out, typebootstrap.Resource{
			Name: name,
			GVK:  gvk,
			// Per-resource IgnoreFields isn't currently exposed
			// on WatchedResource (the watcher only honours the
			// global list today; see
			// pkg/controller/resourcewatcher/watcher.go's
			// mergeIgnoreFields call). When the watcher gets
			// per-resource ignores, this is the line that
			// surfaces them to typegen.
			IgnoreFields: nil,
		})
	}
	return out, nil
}

// newIterationTypeBootstrapper returns the validator.TypeBootstrapper
// closure the iteration hands to setupComponents. It captures the
// iteration's K8s client + logger so the Stage-1 TemplateValidator
// can resolve real reflect.Types for the candidate config during
// scatter-gather validation, rather than degrading to an envelope-
// only declaration set that would false-positively reject charts
// using typed Spec/Status access.
func newIterationTypeBootstrapper(k8sClient *client.Client, logger *slog.Logger) validator.TypeBootstrapper {
	return func(ctx context.Context, cfg *config.Config) (*typebootstrap.Result, error) {
		return runTypeBootstrap(ctx, cfg, k8sClient, logger)
	}
}

// errKindUnresolvable marks a watched resource whose apiVersion is malformed
// or names a resource the cluster doesn't serve.
var errKindUnresolvable = errors.New("kind not resolvable")

// resolveKind converts a (apiVersion, resources-plural) pair into a
// fully-qualified GVK by querying the RESTMapper. A config the cluster
// can't map wraps errKindUnresolvable; any other error is a failed
// discovery.
func resolveKind(ctx context.Context, mapper meta.RESTMapperWithContext, wr *config.WatchedResource) (schema.GroupVersionKind, error) {
	gv, err := schema.ParseGroupVersion(wr.APIVersion)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("%w: parsing apiVersion %q: %w", errKindUnresolvable, wr.APIVersion, err)
	}
	gvr := gv.WithResource(wr.Resources)
	gvk, err := mapper.KindForWithContext(ctx, gvr)
	if meta.IsNoMatchError(err) {
		return schema.GroupVersionKind{}, fmt.Errorf("%w: %w", errKindUnresolvable, err)
	}
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("resolving Kind for %s: %w", gvr, err)
	}
	return gvk, nil
}
