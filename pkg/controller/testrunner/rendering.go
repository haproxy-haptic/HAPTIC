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
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/logging"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// createTestPaths creates per-test temp directories for isolated HAProxy validation.
//
// This creates a subdirectory structure under the base temp directory:
//
//	<base>/worker-<workerID>/test-<testNum>/maps/
//	<base>/worker-<workerID>/test-<testNum>/ssl/
//	<base>/worker-<workerID>/test-<testNum>/files/
//	<base>/worker-<workerID>/test-<testNum>/haproxy.cfg
//
// Each test gets its own isolated directories to prevent file conflicts during
// parallel test execution, even when multiple tests are processed by the same worker.
func (r *Runner) createTestPaths(workerID, testNum int) (*dataplane.ValidationPaths, error) {
	// Extract base temp directory from the shared validation paths
	baseTempDir := filepath.Dir(r.validationPaths.ConfigFile)

	// Create test-specific subdirectory within worker space
	testDir := filepath.Join(baseTempDir, fmt.Sprintf("worker-%d", workerID), fmt.Sprintf("test-%d", testNum))

	// Create base path configuration
	// IMPORTANT: Subdirectory names are derived from configured dataplane paths
	// using path.Base() (slash-only — the configured dirs are HAProxy target
	// paths) to ensure consistency between production and validation.
	// HAProxy requires absolute paths to locate files, so we create absolute paths
	// within the isolated test directory (e.g., /tmp/haproxy-validate-12345/worker-0/test-1/maps).
	basePaths := dataplane.PathConfig{
		MapsDir:    filepath.Join(testDir, path.Base(r.config.Dataplane.MapsDir)),
		SSLDir:     filepath.Join(testDir, path.Base(r.config.Dataplane.SSLCertsDir)),
		GeneralDir: filepath.Join(testDir, path.Base(r.config.Dataplane.GeneralStorageDir)),
		ConfigFile: filepath.Join(testDir, names.MainTemplateName),
	}

	// Use centralized path resolution to get capability-aware paths
	// This ensures CRTListDir is set correctly for HAProxy < 3.2
	resolvedPaths := dataplane.ResolvePaths(basePaths)

	// Browser/WASM: no writable filesystem. Nothing writes to these paths when
	// the binary is replaced (render is in-memory; haproxy_valid uses the
	// caller's check), so return the resolved path strings without MkdirAll.
	if r.checkWithoutBinary != nil {
		return resolvedPaths.ToValidationPaths(), nil
	}

	// Create all directories (CRTListDir may be same as GeneralDir or SSLDir)
	dirsToCreate := []string{resolvedPaths.MapsDir, resolvedPaths.SSLDir, resolvedPaths.GeneralDir}
	if resolvedPaths.CRTListDir != resolvedPaths.SSLDir && resolvedPaths.CRTListDir != resolvedPaths.GeneralDir {
		dirsToCreate = append(dirsToCreate, resolvedPaths.CRTListDir)
	}

	for _, dir := range dirsToCreate {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("creating test directory %s: %w", dir, err)
		}
	}

	return resolvedPaths.ToValidationPaths(), nil
}

// removeTestPaths deletes a finished test's directory, so scratch use is bounded
// by the worker count rather than the suite size, including what a killed
// container leaves in a pod-lifetime emptyDir.
func (r *Runner) removeTestPaths(paths *dataplane.ValidationPaths) {
	if r.checkWithoutBinary != nil {
		return
	}
	if err := os.RemoveAll(filepath.Dir(paths.ConfigFile)); err != nil {
		r.logger.Warn("Failed to remove test directory", "path", filepath.Dir(paths.ConfigFile), "error", err)
	}
}

// RenderOutput bundles every artifact produced by a single test render so
// callers don't have to thread six positional return values.
type RenderOutput struct {
	HAProxyConfig  string
	AuxiliaryFiles *dataplane.AuxiliaryFiles
	K8sResources   map[string]string
	StatusPatches  map[string]string
	// Events is the newline-joined serialization of the Kubernetes Events the
	// templates recorded via recordEvent(), one per line, so validation tests
	// can assert on them with the `target: events` resolver.
	Events       string
	IncludeStats []templating.IncludeStats

	// Plan is what the templates declared about this render, built exactly as
	// the production renderer builds it.
	Plan *renderplan.Plan
}

// renderWithStores renders HAProxy configuration using test fixture stores and worker-specific engine.
//
// This follows the same pattern as DryRunValidator.renderWithOverlayStores.
// When profileIncludes is enabled, it returns timing statistics for included templates.
// The currentConfig parameter enables slot-aware server assignment testing (nil for first deployment).
// extraContext is the whole extraContext the render sees (see AssertionExtraContext).
//
// Returns rendered haproxy.cfg, auxiliary files, k8sResources (template name → YAML),
// status patches (key `<ns>/<name>:<phase>` → JSON-marshalled status content), and
// include-stats (when profiling) bundled in a RenderOutput, plus the render error.
func (r *Runner) renderWithStores(ctx context.Context, engine templating.Engine, storeMap map[string]stores.Store, validationPaths *dataplane.ValidationPaths, httpStore *FixtureHTTPStoreWrapper, currentConfig *renderplan.CurrentConfig, currentFiles map[string]string, extraContext map[string]any) (RenderOutput, error) {
	bctx, err := r.buildRenderingContext(ctx, storeMap, validationPaths, httpStore, currentConfig, currentFiles, extraContext)
	if err != nil {
		return RenderOutput{}, err
	}
	renderCtx := bctx.Context
	renderMode := extraContextRenderMode(extraContext)
	coldRender, err := renderer.NewColdIncrementalRender(ctx, &renderer.ColdIncrementalRenderConfig{
		Config:             r.config,
		Engine:             engine,
		StoreProvider:      stores.NewRealStoreProvider(storeMap),
		Mode:               renderMode,
		TemplateContext:    renderCtx,
		ResourceErrors:     bctx.ResourceErrors,
		Logger:             r.logger,
		TypedResourceTypes: r.typedResourceTypes,
		Analysis:           r.coldAnalysisFor(engine),
	})
	if err != nil {
		return RenderOutput{}, fmt.Errorf("starting cold incremental render: %w", err)
	}
	ctx = coldRender.Context(ctx)

	// Render main HAProxy configuration using worker-specific engine
	mainCtx := templating.WithIncrementalScope(ctx, names.MainTemplateName)
	mainRender, err := rendercontext.RenderMain(mainCtx, engine, renderCtx, bctx.PlanRegistry, r.profileIncludes)
	if resourceErr := bctx.Err(ctx); resourceErr != nil {
		return RenderOutput{}, resourceErr
	}
	if err != nil {
		return RenderOutput{}, fmt.Errorf("rendering %s: %w", names.MainTemplateName, err)
	}
	haproxyConfig, includeStats := mainRender.Config, mainRender.IncludeStats

	// Render auxiliary files using worker-specific engine (pre-declared files)
	staticFiles, err := r.renderAuxiliaryFiles(ctx, engine, renderCtx, validationPaths)
	if resourceErr := bctx.Err(ctx); resourceErr != nil {
		return RenderOutput{}, resourceErr
	}
	if err != nil {
		return RenderOutput{}, fmt.Errorf("rendering auxiliary files: %w", err)
	}

	k8sResources, err := r.renderK8sResources(ctx, engine, renderCtx, bctx)
	if err != nil {
		return RenderOutput{}, err
	}
	if err := coldRender.ValidateIncrementalCalls(); err != nil {
		return RenderOutput{}, err
	}

	statusPatches, err := collectStatusPatches(renderCtx)
	if err != nil {
		return RenderOutput{}, err
	}

	renderedEvents := collectEvents(renderCtx)

	// Extract dynamic files registered during template rendering
	fileRegistry := renderCtx["fileRegistry"].(*rendercontext.FileRegistry)
	dynamicFiles := fileRegistry.GetFiles()

	// Merge static (pre-declared) and dynamic (registered) files
	auxiliaryFiles, err := rendercontext.MergeAuxiliaryFiles(staticFiles, dynamicFiles)
	if err != nil {
		return RenderOutput{}, fmt.Errorf("merging auxiliary files: %w", err)
	}

	// Debug logging
	staticCount := len(staticFiles.MapFiles) + len(staticFiles.GeneralFiles) + len(staticFiles.SSLCertificates) + len(staticFiles.CRTListFiles)
	dynamicCount := len(dynamicFiles.MapFiles) + len(dynamicFiles.GeneralFiles) + len(dynamicFiles.SSLCertificates) + len(dynamicFiles.CRTListFiles)
	if dynamicCount > 0 {
		r.logger.Log(context.Background(), logging.LevelTrace, "Merged auxiliary files",
			"static_count", staticCount,
			"dynamic_count", dynamicCount)
	}

	plan, err := bctx.PlanRegistry.Plan(haproxyConfig, auxiliaryFiles)
	if err != nil {
		return RenderOutput{}, fmt.Errorf("building the render plan: %w", err)
	}
	return RenderOutput{
		HAProxyConfig:  haproxyConfig,
		AuxiliaryFiles: auxiliaryFiles,
		K8sResources:   k8sResources,
		StatusPatches:  statusPatches,
		Events:         renderedEvents,
		IncludeStats:   includeStats,
		Plan:           plan,
	}, nil
}

func (r *Runner) coldAnalysisFor(engine templating.Engine) *renderer.ColdIncrementalRenderAnalysis {
	if r.coldAnalysis == nil || (engine != nil && !reflect.ValueOf(engine).Comparable()) || engine != r.engineTemplate {
		return nil
	}
	return r.coldAnalysis()
}

// AssertionExtraContext is the extraContext a test's assertions render with:
// the config's testExtraContext, then _global, then the test's own. The
// deployment's extraContext never reaches it, the same way live resources never
// reach a fixture store.
func AssertionExtraContext(cfg *config.Config, test *config.ValidationTest) map[string]any {
	return withTestLayers(cfg, cfg.TemplatingSettings.TestExtraContext, test)
}

// DeploymentExtraContext is what the deployment check renders a test's fixtures
// with: the deployment's extraContext beneath _global and the test's own.
// _global stays on top because it binds names to fixtures (the default
// certificate) that the deployment's values point elsewhere.
func DeploymentExtraContext(cfg *config.Config, test *config.ValidationTest) map[string]any {
	return withTestLayers(cfg, cfg.TemplatingSettings.ExtraContext, test)
}

func withTestLayers(cfg *config.Config, base map[string]any, test *config.ValidationTest) map[string]any {
	merged := base
	if globalTest, ok := cfg.ValidationTests["_global"]; ok {
		merged = deepMergeMaps(merged, globalTest.ExtraContext)
	}
	return deepMergeMaps(merged, test.ExtraContext)
}

// ExtraContextOptions hands a test's extraContext to the context builder. A
// test simulates an admission with extraContext.renderMode and
// extraContext.admissionSubject; the builder owns both globals and would
// otherwise overwrite the promoted keys.
func ExtraContextOptions(extraContext map[string]any) ([]rendercontext.Option, error) {
	detached, err := rendercontext.DetachExtraContext(extraContext)
	if err != nil {
		return nil, fmt.Errorf("copying extraContext: %w", err)
	}
	opts := []rendercontext.Option{
		rendercontext.WithDetachedExtraContext(detached),
		rendercontext.WithRenderMode(extraContextRenderMode(extraContext)),
	}
	if subject, ok := extraContext["admissionSubject"].(map[string]any); ok {
		field := func(key string) string {
			value, _ := subject[key].(string)
			return value
		}
		aliases := []string{field("store")}
		if storeSet, ok := subject["stores"].(map[string]any); ok {
			aliases = slices.Sorted(maps.Keys(storeSet))
		}
		opts = append(opts, rendercontext.WithAdmissionSubjectStores(aliases, field("namespace"), field("name")))
	}
	return opts, nil
}

func extraContextRenderMode(extraContext map[string]any) rendercontext.RenderMode {
	if extraContext["renderMode"] == string(rendercontext.RenderModeAdmission) {
		return rendercontext.RenderModeAdmission
	}
	return rendercontext.RenderModeReconcile
}

// replaceSentinelKey, when present (with any truthy value) in a test
// extraContext map, makes that map REPLACE the deployment's map wholesale
// instead of deep-merging into it. The sentinel key itself is stripped from
// the result. This is the escape hatch for map-valued registries (e.g.
// extraContext.waf.policies.inline) where merge semantics would otherwise
// let deployment-defined sibling keys join a test's pinned set — a baked
// test that needs the EXACT key set pins it with:
//
//	inline:
//	  __replace__: true
//	  approved-policy: {}
const replaceSentinelKey = "__replace__"

// deepMergeMaps returns a new map with override folded into base: keys whose
// values are maps on both sides merge recursively, any other value replaces
// the base value. A nested override map carrying the __replace__ sentinel
// replaces the base map wholesale (sentinel stripped). Neither input map is
// mutated.
func deepMergeMaps(base, override map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(override))
	maps.Copy(merged, base)
	for key, value := range override {
		baseMap, baseOk := merged[key].(map[string]any)
		overrideMap, overrideOk := value.(map[string]any)
		if overrideOk {
			if _, replace := overrideMap[replaceSentinelKey]; replace {
				merged[key] = stripReplaceSentinel(overrideMap)
				continue
			}
		}
		if baseOk && overrideOk {
			merged[key] = deepMergeMaps(baseMap, overrideMap)
			continue
		}
		merged[key] = value
	}
	return merged
}

// stripReplaceSentinel returns a copy of m without the __replace__ key,
// recursing into nested maps so a replaced subtree can itself contain
// further sentinels. The input map is not mutated.
func stripReplaceSentinel(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for key, value := range m {
		if key == replaceSentinelKey {
			continue
		}
		if nested, ok := value.(map[string]any); ok {
			out[key] = stripReplaceSentinel(nested)
			continue
		}
		out[key] = value
	}
	return out
}

// collectStatusPatches drains the StatusPatchCollector that the templates'
// statusPatch() calls populated during the haproxy.cfg render. Each patch's
// variants (rendered / deployed / renderFailed / deployFailed) flatten into
// one map entry per phase keyed by `<ns>/<name>:<phase>` (or `:<phase>` for
// cluster-scoped resources without a namespace, e.g. GatewayClass). Values
// are JSON-marshalled so chart validation tests can assert on substrings via
// the standard contains / not_contains machinery (see assertion_helpers.go's
// `target: status:` resolver).
func collectStatusPatches(renderCtx map[string]any) (map[string]string, error) {
	out := make(map[string]string)
	collector, ok := renderCtx["statusPatchCollector"].(*templating.StatusPatchCollector)
	if !ok || collector == nil {
		return out, nil
	}
	patches, err := collector.Patches()
	if err != nil {
		return nil, fmt.Errorf("snapshotting status patches: %w", err)
	}
	for index := range patches {
		patch := &patches[index]
		keyPrefix := patch.Namespace + "/" + patch.Name
		for phase, payload := range patch.Variants {
			bytes, err := json.Marshal(payload)
			if err != nil {
				return nil, fmt.Errorf("marshalling status patch for %s/%s phase %s: %w", patch.Namespace, patch.Name, phase, err)
			}
			out[keyPrefix+":"+phase] = string(bytes)
		}
	}
	return out, nil
}

// collectEvents drains the EventCollector that the templates' recordEvent()
// calls populated during rendering and serializes each Event to one line so
// validation tests can assert on them via the `target: events` resolver.
// Format: `<Type> <Reason> <apiVersion> <Kind> <ns>/<name>: <message>`.
func collectEvents(renderCtx map[string]any) string {
	collector, ok := renderCtx["recordEventCollector"].(*templating.EventCollector)
	if !ok || collector == nil {
		return ""
	}
	events := collector.Events()
	if len(events) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "%s %s %s %s %s/%s: %s\n",
			e.Type, e.Reason, e.APIVersion, e.Kind, e.Namespace, e.Name, e.Message)
	}
	return b.String()
}

// buildRenderingContext builds the template rendering context using fixture stores.
//
// This method delegates to the centralized rendercontext.Builder to ensure consistent
// context creation across all usages (renderer, testrunner, benchmark, dryrunvalidator).
//
// Special handling for TestRunner:
//   - Creates PathResolver from ValidationPaths (not from config.Dataplane)
//   - Separates haproxy-pods store from resource stores
//   - Accepts optional currentConfig for slot-aware server assignment testing
//
// renderK8sResources renders every `k8sResources` template and returns the
// rendered text per template, which assertions reach via `target:
// k8s:<template-name>` and --dump-rendered prints alongside haproxy.cfg.
//
// Each document also goes through the registration and validation the
// controller performs, against one collector shared by every template exactly
// as production shares it. Assertions only ever see the text, so without this a
// document the controller rejects still passes every assertion and fails first
// in a live cluster, as an admission denial.
func (r *Runner) renderK8sResources(
	ctx context.Context,
	engine templating.Engine,
	renderCtx map[string]any,
	bctx *rendercontext.BuildResult,
) (map[string]string, error) {
	k8sResources := make(map[string]string, len(r.config.K8sResources))
	collector := templating.NewRenderedResourceCollector()
	for name := range r.config.K8sResources {
		scopedCtx := templating.WithIncrementalScope(ctx, name)
		rendered, err := engine.Render(scopedCtx, name, renderCtx)
		if resourceErr := bctx.Err(ctx); resourceErr != nil {
			return nil, resourceErr
		}
		if err != nil {
			return nil, fmt.Errorf("rendering k8sResources %s: %w", name, err)
		}
		k8sResources[name] = rendered

		if err := renderer.RegisterK8sResourceDocs(
			name, rendered, collector,
			r.config.K8sResources[name].CreateOnlyFields,
		); err != nil {
			return nil, err
		}
	}
	if err := collector.Validate(); err != nil {
		return nil, err
	}
	return k8sResources, nil
}

func (r *Runner) buildRenderingContext(ctx context.Context, storeMap map[string]stores.Store, validationPaths *dataplane.ValidationPaths, httpStore *FixtureHTTPStoreWrapper, currentConfig *renderplan.CurrentConfig, currentFiles map[string]string, extraContext map[string]any) (*rendercontext.BuildResult, error) {
	extraContextOpts, err := ExtraContextOptions(extraContext)
	if err != nil {
		return nil, err
	}

	// Create PathResolver from ValidationPaths
	pathResolver := rendercontext.PathResolverFromValidationPaths(validationPaths)

	// Separate haproxy-pods from resource stores (goes in controller namespace)
	resourceStores, haproxyPodStore := rendercontext.SeparateHAProxyPodStore(storeMap)
	if haproxyPodStore != nil {
		r.logger.Log(context.Background(), logging.LevelTrace, "wrapping haproxy-pods store for rendering context")
	}

	// Build context using centralized builder. typedResourceTypes is
	// nil unless the CLI wired typebootstrap (see cmd/haptic/
	// validate.go) — when populated, the builder emits one *[]*T
	// top-level global per typed resource so chart templates that
	// use the typed shape compile against the same surface the
	// production renderer provides.
	opts := append([]rendercontext.Option{
		rendercontext.WithStores(resourceStores),
		rendercontext.WithHAProxyPodStore(haproxyPodStore),
		rendercontext.WithHTTPFetcher(httpStore),
		rendercontext.WithCurrentConfig(currentConfig),
		rendercontext.WithCurrentAuxFiles(currentFiles),
		rendercontext.WithTypedResources(r.typedResourceTypes),
		rendercontext.WithCapabilities(r.capabilities),
	}, extraContextOpts...)

	return rendercontext.NewBuilder(ctx, r.config, pathResolver, r.logger, opts...).Build(), nil
}

// renderAuxiliaryFiles renders all auxiliary files (maps, general files, SSL certificates) using worker-specific engine.
func (r *Runner) renderAuxiliaryFiles(ctx context.Context, engine templating.Engine, renderCtx map[string]any, validationPaths *dataplane.ValidationPaths) (*dataplane.AuxiliaryFiles, error) {
	auxFiles := &dataplane.AuxiliaryFiles{}

	// Render map files using worker-specific engine
	for name := range r.config.Maps {
		scopedCtx := templating.WithIncrementalScope(ctx, name)
		rendered, err := engine.Render(scopedCtx, name, renderCtx)
		if err != nil {
			return nil, fmt.Errorf("rendering map file %s: %w", name, err)
		}

		auxFiles.MapFiles = append(auxFiles.MapFiles, auxiliaryfiles.MapFile{
			Path:    name,
			Content: rendered,
		})
	}

	// Render general files using worker-specific engine
	for name := range r.config.Files {
		scopedCtx := templating.WithIncrementalScope(ctx, name)
		rendered, err := engine.Render(scopedCtx, name, renderCtx)
		if err != nil {
			return nil, fmt.Errorf("rendering general file %s: %w", name, err)
		}

		auxFiles.GeneralFiles = append(auxFiles.GeneralFiles, auxiliaryfiles.GeneralFile{
			Filename: name,
			Path:     filepath.Join(validationPaths.GeneralStorageDir, name),
			Content:  rendered,
		})
	}

	// Render SSL certificates using worker-specific engine
	for name := range r.config.SSLCertificates {
		scopedCtx := templating.WithIncrementalScope(ctx, name)
		rendered, err := engine.Render(scopedCtx, name, renderCtx)
		if err != nil {
			return nil, fmt.Errorf("rendering SSL certificate %s: %w", name, err)
		}

		auxFiles.SSLCertificates = append(auxFiles.SSLCertificates, auxiliaryfiles.SSLCertificate{
			Path:    name,
			Content: rendered,
		})
	}

	return auxFiles, nil
}
