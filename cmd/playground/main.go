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

//go:build js && wasm

// Command playground is the WebAssembly entrypoint for the browser template
// playground. It renders a HAProxyTemplateConfig against example resources
// entirely client-side by driving the controller's PRODUCTION render path —
// renderer.RenderService.Render — so playground output is identical to what the
// controller deploys at the same release. It contains no render orchestration
// of its own; the only difference from production is that stores are built from
// in-memory example resources instead of live informers, and the HAProxy
// version/capabilities are supplied rather than detected.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"syscall/js"
	"time"

	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/haproxy-haptic/HAPTIC/cmd/playground/internal/migratecheck"
	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/schemafetcher"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// renderResult is the flat, JSON-friendly shape returned across the JS boundary.
type renderResult struct {
	HAProxyConfig string
	Maps          map[string]string
	Files         map[string]string
	Certs         map[string]string
	CertsDecoded  any // certs path -> decoded x509 summary (certs/keys/errors) for the certs tab
	CRTLists      map[string]string
	StatusPatches map[string]string // target "<Kind> <ns>/<name>" -> rendered .status YAML
	Events        []string          // Kubernetes Events templates recorded via recordEvent(), one formatted line each
	Applied       map[string]string // target "<Kind> <ns>/<name>" -> full k8sResources object YAML the controller applies
	DurationMs    int64
	Bucket        *BucketReport // where each pasted resource landed / why it was dropped
	Trace         []any         // per-snippet render stats (JS-ready): {name, count, totalMs, avgMs, maxMs}
	Migration     any           // migration report for pasted Ingresses and ConfigMaps (nil when no coverage or nothing to classify)
	SchemaCheck   string        // per-field schema violation in the rendered config (empty = passed)
	ReloadImpact  any           // reload verdict from deployplan.Diff vs the baseline (nil on the first render)
	Provenance    any           // output tab -> per-line config-editor line that produced it (0 = none)
	ProvBase      any           // aux file id -> config line of its template block (for the "# name" header)
	ProvBlocks    any           // template name -> config line of its block scalar (trace tab jump targets)
	WatchedLines  any           // watched-resource name -> config line of its declaration (resources tab jump)
}

// warmEngine holds the compiled engine + render service between renders, so a
// resource-only edit renders without recompiling templates or re-running type
// bootstrap. Package-global state is safe here because wasm is single-threaded:
// every JS->Go call runs to completion before the next begins.
type warmEngine struct {
	cfg        *config.Config
	svc        *renderer.RenderService // holds the compiled engine
	runner     *testrunner.Runner      // owns CreateStoresFromFixtures (also holds the engine)
	byKey      map[string]schema.GroupVersionKind
	coverage   []migratecheck.CoverageSource
	ver        *dataplane.Version // selected HAProxy version, for the schema check
	configYAML []byte             // raw config, for provenance line lookups
}

var (
	warm      *warmEngine
	loadCount int // number of engine compiles — the compile-count probe reads this

	// Reload-impact baseline: the plan a new render is diffed against with
	// deployplan.Diff — the controller's own decision layer — to show whether
	// deploying it would reload HAProxy or apply over the runtime socket. The
	// auxiliary files ride along because the next render reads them back as
	// `currentFiles`. prevBaseline tracks the last render (the default
	// baseline); pinnedBaseline freezes a baseline the user chose.
	prevBaseline   *renderBaseline
	pinnedBaseline *renderBaseline
)

// renderBaseline is a render's plan and its auxiliary files, the unit a later
// render is diffed against for the reload-impact verdict.
type renderBaseline struct {
	plan *renderplan.Plan
	aux  *dataplane.AuxiliaryFiles
}

func main() {
	js.Global().Set("hapticLoadConfig", js.FuncOf(hapticLoadConfigJS))
	js.Global().Set("hapticRender", js.FuncOf(hapticRenderJS))
	js.Global().Set("hapticRunTests", js.FuncOf(hapticRunTestsJS))
	js.Global().Set("hapticPinBaseline", js.FuncOf(func(_ js.Value, _ []js.Value) any { pinnedBaseline = prevBaseline; return pinnedBaseline != nil }))
	js.Global().Set("hapticUnpinBaseline", js.FuncOf(func(_ js.Value, _ []js.Value) any { pinnedBaseline = nil; return nil }))
	js.Global().Set("hapticResetBaseline", js.FuncOf(func(_ js.Value, _ []js.Value) any { prevBaseline = nil; pinnedBaseline = nil; return nil }))

	// Signal readiness so the host can drop its loading state.
	if ready := js.Global().Get("hapticOnReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}

	// The Go runtime must stay alive to service future JS calls; returning from
	// main would tear down the instance.
	select {}
}

// hapticLoadConfigJS compiles the engine + render service and holds them warm.
// Signature: hapticLoadConfig(configYAML, schemasJSON, haproxyVersion,
// migrationCoverageJSON). Call it when any input changes, then call hapticRender
// as resources change. Omitting the fourth argument reads coverage from an old
// config snapshot. YAML is unmarshalled inside wasm so numerics stay JSON-native.
func hapticLoadConfigJS(_ js.Value, args []js.Value) any {
	if len(args) < 3 {
		return jsError("hapticLoadConfig requires configYAML, schemasJSON, and haproxyVersion")
	}
	var coverageJSON []byte
	if len(args) >= 4 {
		coverageJSON = []byte(args[3].String())
	}
	if err := loadConfig([]byte(args[0].String()), []byte(args[1].String()), args[2].String(), coverageJSON); err != nil {
		warm = nil // a failed load invalidates any previous warm engine
		return jsError(err.Error())
	}
	return map[string]any{"ok": true, "loadCount": float64(loadCount)}
}

// hapticRenderJS renders the example resources against the warm engine.
// Signature: hapticRender(resourcesYAML). hapticLoadConfig must have succeeded first.
func hapticRenderJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return jsError("hapticRender requires 1 argument: resourcesYAML")
	}
	res, err := renderWarm([]byte(args[0].String()))
	if err != nil {
		return jsError(err.Error())
	}
	return map[string]any{
		"haproxyCfg":    res.HAProxyConfig,
		"maps":          toAnyMap(res.Maps),
		"files":         toAnyMap(res.Files),
		"certs":         toAnyMap(res.Certs),
		"certsDecoded":  res.CertsDecoded,
		"crtLists":      toAnyMap(res.CRTLists),
		"durationMs":    float64(res.DurationMs),
		"loadCount":     float64(loadCount),
		"bucketReport":  bucketReportToJS(res.Bucket),
		"trace":         orEmptySlice(res.Trace),
		"statusPatches": toAnyMap(res.StatusPatches),
		"events":        stringsToAny(res.Events),
		"applied":       toAnyMap(res.Applied),
		"migration":     res.Migration,
		"schemaCheck":   res.SchemaCheck,
		"reloadImpact":  res.ReloadImpact,
		"provenance":    res.Provenance,
		"provBase":      res.ProvBase,
		"provBlocks":    res.ProvBlocks,
		"watchedLines":  res.WatchedLines,
		// Count of runnable spec.validationTests so the UI can offer the Tests
		// tab without re-parsing the config. Excludes the special "_global" key
		// (shared fixtures, not a test — the runner skips it too), so the count
		// matches TestResults.TotalTests. Tests run on demand via hapticRunTests.
		"testCount": float64(runnableTestCount(warm.cfg.ValidationTests)),
	}
}

// runnableTestCount counts real validation tests, excluding the special
// "_global" entry, which only carries fixtures shared across tests and is
// skipped by testrunner.RunTests (so TotalTests never counts it).
func runnableTestCount(tests map[string]config.ValidationTest) int {
	n := 0
	for name := range tests {
		if name != "_global" {
			n++
		}
	}
	return n
}

// hapticRunTestsJS runs the config's spec.validationTests against the warm
// engine and returns per-test / per-assertion results for the Tests tab.
// Signature: hapticRunTests(). hapticLoadConfig must have succeeded first.
//
// The runner has no filesystem and no haproxy binary in the browser, so
// `haproxy_valid` assertions run the pure-Go syntax+schema check — the UI
// labels them as such.
func hapticRunTestsJS(_ js.Value, _ []js.Value) any {
	if warm == nil {
		return jsError("no config loaded: call hapticLoadConfig first")
	}
	results, err := warm.runner.RunTests(context.Background(), "")
	if err != nil {
		return jsError(templating.RenderErrorMessage(err))
	}
	return testResultsToJS(results)
}

// testResultsToJS converts a *testrunner.TestResults into js.ValueOf-compatible
// data for the Tests tab.
func testResultsToJS(r *testrunner.TestResults) map[string]any {
	tests := make([]any, 0, len(r.TestResults))
	for i := range r.TestResults {
		t := &r.TestResults[i]
		assertions := make([]any, 0, len(t.Assertions))
		for j := range t.Assertions {
			a := &t.Assertions[j]
			assertions = append(assertions, map[string]any{
				"type":        a.Type,
				"description": a.Description,
				"passed":      a.Passed,
				"error":       a.Error,
				"target":      a.Target,
			})
		}
		tests = append(tests, map[string]any{
			"name":        t.TestName,
			"description": t.Description,
			"passed":      t.Passed,
			"skipped":     t.Skipped,
			"skipReason":  t.SkipReason,
			"renderError": t.RenderError,
			"durationMs":  float64(t.Duration.Milliseconds()),
			"assertions":  assertions,
		})
	}
	return map[string]any{
		"total":      float64(r.TotalTests),
		"passed":     float64(r.PassedTests),
		"failed":     float64(r.FailedTests),
		"skipped":    float64(r.SkippedTests),
		"durationMs": float64(r.Duration.Milliseconds()),
		"tests":      tests,
	}
}

// orEmptySlice returns a non-nil slice so the JS boundary sees [] not null.
func orEmptySlice(s []any) []any {
	if s == nil {
		return []any{}
	}
	return s
}

// stringsToAny converts a []string to a []any (never nil) for the JS boundary.
func stringsToAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// bucketReportToJS converts the bucketing report into js.ValueOf-compatible
// data ([]any of map[string]any) for the resources-inspector UI.
func bucketReportToJS(r *BucketReport) any {
	if r == nil {
		return []any{}
	}
	objs := make([]any, 0, len(r.Objects))
	for _, o := range r.Objects {
		buckets := make([]any, len(o.Buckets))
		for i, b := range o.Buckets {
			buckets[i] = b
		}
		objs = append(objs, map[string]any{
			"apiVersion": o.APIVersion,
			"kind":       o.Kind,
			"namespace":  o.Namespace,
			"name":       o.Name,
			"buckets":    buckets,
			"dropped":    o.Dropped,
			"reason":     o.Reason,
		})
	}
	return objs
}

// migrationReportToJS flattens a migration report into js.ValueOf-compatible
// data for the migration UI tab. Per source, per Ingress, each classified
// annotation carries its coverage status and note.
func migrationReportToJS(r *migratecheck.Report) map[string]any {
	toIngress := func(ir migratecheck.IngressReport) map[string]any {
		findings := make([]any, 0, len(ir.Findings))
		for _, f := range ir.Findings {
			findings = append(findings, map[string]any{
				"annotation": f.Annotation,
				"value":      f.Value,
				"status":     string(f.Status),
				"note":       f.Note,
			})
		}
		return map[string]any{
			"namespace": ir.Namespace,
			"name":      ir.Name,
			"class":     ir.Class,
			"findings":  findings,
		}
	}
	sources := make([]any, 0, len(r.Sources))
	for _, s := range r.Sources {
		ings := make([]any, 0, len(s.Ingresses))
		for _, ir := range s.Ingresses {
			ings = append(ings, toIngress(ir))
		}
		sources = append(sources, map[string]any{
			"source":     s.Source,
			"ingresses":  ings,
			"configMaps": configMapReportsToJS(s.ConfigMaps),
			"counts":     countsToJS(s.Counts),
		})
	}
	unattr := make([]any, 0, len(r.Unattributed))
	for _, ir := range r.Unattributed {
		unattr = append(unattr, toIngress(ir))
	}
	return map[string]any{
		"sources":              sources,
		"unattributed":         unattr,
		"counts":               countsToJS(r.Counts),
		"totalIngresses":       float64(r.TotalIngresses),
		"checkedAnnotations":   float64(r.CheckedAnnotations),
		"totalConfigMaps":      float64(r.TotalConfigMaps),
		"checkedConfigMapKeys": float64(r.CheckedConfigMapKeys),
	}
}

func configMapReportsToJS(reports []migratecheck.ConfigMapReport) []any {
	out := make([]any, 0, len(reports))
	for _, cr := range reports {
		findings := make([]any, 0, len(cr.Findings))
		for _, f := range cr.Findings {
			findings = append(findings, map[string]any{
				"key":     f.Key,
				"value":   f.Value,
				"status":  string(f.Status),
				"setting": f.Setting,
				"note":    f.Note,
				"problem": f.Problem,
			})
		}
		out = append(out, map[string]any{
			"namespace": cr.Namespace,
			"name":      cr.Name,
			"findings":  findings,
			"problem":   cr.Problem,
		})
	}
	return out
}

func countsToJS(c map[migratecheck.Status]int) map[string]any {
	out := make(map[string]any, len(c))
	for k, v := range c {
		out[string(k)] = float64(v)
	}
	return out
}

// loadConfig runs the expensive steps — parse, type bootstrap, engine compile,
// build the production render service — and stores them in `warm`. Same
// production call graph as the controller; just split out so a resource-only
// render can reuse it.
func loadConfig(configYAML, schemasJSON []byte, haproxyVersion string, migrationCoverageJSON []byte) error {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, err := parseConfigSpec(configYAML)
	if err != nil {
		return err
	}
	coverage, legacyCoverage := migratecheck.ParseLegacyConfigCoverage(configYAML)
	if !legacyCoverage && migrationCoverageJSON != nil {
		coverage, err = migratecheck.ParseCoverage(migrationCoverageJSON)
	}
	if err != nil {
		return err
	}
	cfg, err := conversion.ConvertSpec(spec)
	if err != nil {
		return fmt.Errorf("converting config: %w", err)
	}

	byGVK, byKey, err := parseSchemaBundle(schemasJSON)
	if err != nil {
		return err
	}

	// Collapse each watched resource's candidate apiVersions to the one the
	// schema bundle serves — the controller/offline-validate paths do this via
	// config.ResolveEffective, and the render path relies on it: a multi-version
	// resource like gatewayclasses (apiVersions: [v1, v1beta1]) otherwise keeps
	// an empty singular APIVersion, so resources.<name>.APIVersion() returns ""
	// and the status templates fail ("apiVersion ... required"). Resource-agnostic.
	cfg, _, err = config.ResolveEffective(cfg, bundleServedChecker{byKey: byKey}, nil)
	if err != nil {
		return fmt.Errorf("resolving effective config: %w", err)
	}

	typed, err := bootstrapTypes(cfg, byGVK, byKey, logger)
	if err != nil {
		return err
	}

	// The browser has no HAProxy binary, so capabilities use the selected version.
	ver, err := dataplane.ParseVersionString(haproxyVersion)
	if err != nil {
		return fmt.Errorf("parsing HAProxy version %q: %w", haproxyVersion, err)
	}
	caps := dataplane.CapabilitiesFromVersion(ver)

	// Compile the engine exactly as the controller does — same helper, same
	// declarations — so template compilation behaves identically.
	decls := helpers.BuildAdditionalDeclarations(cfg, typed)
	// Profiling on: the playground surfaces per-snippet render stats (the trace
	// tab). Behaviour-neutral for output; just adds include timing collection.
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, decls, helpers.EngineOptions{EnableProfiling: true})
	if err != nil {
		return fmt.Errorf("compiling templates: %w", err)
	}

	runner := testrunner.New(cfg, engine, renderPaths(), &testrunner.Options{
		Logger:             logger,
		Capabilities:       caps,
		HAProxyVersion:     ver,
		TypedResourceTypes: typed.Types,
		// Browser: no filesystem, no haproxy binary. Run tests single-threaded
		// (js/wasm has no OS threads) and answer haproxy_valid with the pure-Go
		// syntax+schema check. The Tests tab labels it accordingly.
		Workers: 1,
		CheckWithoutBinary: func(haproxyConfig string) error {
			_, err := dataplane.ValidateSyntaxAndSchema(haproxyConfig, ver)
			return err
		},
	})
	// The production render service. This is the whole point: identical output
	// to the controller at this release.
	svc := renderer.NewRenderService(&renderer.RenderServiceConfig{
		Engine:             engine,
		Config:             cfg,
		Logger:             logger,
		Capabilities:       caps,
		TypedResourceTypes: typed.Types,
		// Feed the previously-rendered aux files back as `currentFiles`, so a
		// self-referential template (self-rotating TLS session-ticket keys) reads
		// its own prior output across successive renders — exactly as the
		// controller does from its published aux CRDs. A pinned baseline wins over
		// the last render, so pinning a set of files fixes future renders against
		// them (the same pinned/prev baseline that drives the reload-impact
		// verdict). Read at render time so it tracks pin/unpin without a recompile.
		CurrentAuxFilesProvider: func() map[string]string {
			b := pinnedBaseline
			if b == nil {
				b = prevBaseline
			}
			if b == nil {
				return nil
			}
			return b.aux.CurrentFiles()
		},
	})

	warm = &warmEngine{cfg: cfg, svc: svc, runner: runner, byKey: byKey, coverage: coverage, ver: ver, configYAML: configYAML}
	// NB: the reload-impact baseline (prev/pinned) intentionally survives a recompile,
	// so a config edit is measured against the pinned/last render. Fresh loads (preset
	// switch, shared link, session restore) drop it explicitly via hapticResetBaseline.
	loadCount++
	return nil
}

// renderWarm renders the example resources against the warm engine. Only the
// cheap steps run: bucket the resources into in-memory stores (via the
// production indexer) and invoke renderer.RenderService.Render.
//
// The resources may be a raw `kubectl get ... -o yaml` dump (a List, a single
// object, or a multi-document stream) or the name-keyed validationTests-fixtures
// shape; parseResources buckets each object by apiVersion+kind against the
// config's watchedResources, honouring their label/field selectors, exactly as
// the controller's watchers do.
func renderWarm(resourcesYAML []byte) (*renderResult, error) {
	if warm == nil {
		return nil, fmt.Errorf("no config loaded: call hapticLoadConfig first")
	}

	fixtures, report, err := parseResources(warm.cfg, warm.byKey, resourcesYAML)
	if err != nil {
		return nil, fmt.Errorf("parsing resources: %w", err)
	}
	storeMap, err := warm.runner.CreateStoresFromFixtures(fixtures)
	if err != nil {
		return nil, fmt.Errorf("building stores from resources: %w", err)
	}
	provider := stores.NewRealStoreProvider(storeMap)

	// The playground is a preview: render in the lenient reconcile mode so a
	// cross-family conflict surfaces as a Warning Event in the events tab rather
	// than aborting the whole preview render.
	rendered, err := warm.svc.Render(context.Background(), provider, rendercontext.RenderModeReconcile)
	if err != nil {
		return nil, fmt.Errorf("rendering: %s", templating.RenderErrorMessage(err))
	}
	out, err := materializePreview(rendered)
	if err != nil {
		return nil, fmt.Errorf("preparing preview: %w", err)
	}
	// NOTE: no client-native validation of the output — it's too lenient to be a
	// useful safety net (accepts arbitrary text; only rejects an empty string).
	// Meaningful config validation needs `haproxy -c`, which can't run in wasm.
	// Template mistakes surface as template-compile / render errors above.
	rr := toRenderResult(out)
	rr.Bucket = report
	rr.Migration = migrationReport(warm.coverage, resourcesYAML)
	// Per-field schema check on the rendered config: the same pure-Go OpenAPI
	// validators the controller runs before deploy (dataplane.ValidateSyntaxAndSchema
	// — no haproxy binary, no filesystem). Catches bad enums, out-of-range numbers,
	// and missing required fields on servers/binds/frontend/backend rules. It is a
	// strict SUBSET of `haproxy -c`: it can't see cross-references (use_backend to an
	// undefined backend), unknown keywords, or global/defaults errors — the UI labels
	// it as such, and its silence is never presented as "the config is valid".
	if _, verr := dataplane.ValidateSyntaxAndSchema(out.HAProxyConfig, warm.ver); verr != nil {
		rr.SchemaCheck = cleanSchemaError(dataplane.SimplifyValidationError(verr))
	}
	rr.ReloadImpact = reloadImpact(out, warm.ver)
	// Provenance: a second, source-map-collecting render pass over the same
	// context maps each output line back to the config-editor line that produced
	// it (works for any config — no marker convention, follows render includes).
	if sms, smErr := warm.svc.RenderSourceMaps(context.Background(), provider); smErr == nil && sms != nil {
		prov, base := buildProvenance(warm.configYAML, sms, out)
		// Applied (re-marshaled k8sResources) and status (computed values) can't be
		// source-mapped by buildProvenance; attribute them separately and merge in.
		blockBase := templateBlockBases(warm.configYAML)
		k8sNames := make([]string, 0, len(warm.cfg.K8sResources))
		for name := range warm.cfg.K8sResources {
			k8sNames = append(k8sNames, name)
		}
		ap, ab := appliedStatusChains(sms, blockBase, k8sNames, rr.Applied, rr.StatusPatches, out.StatusPatches)
		for k, v := range ap {
			prov[k] = v
		}
		for k, v := range ab {
			base[k] = v
		}
		rr.Provenance = chainsToJS(prov)
		rr.ProvBase = intMapToJSFlat(base)
		rr.ProvBlocks = intMapToJSFlat(blockBase)
	}
	rr.WatchedLines = intMapToJSFlat(watchedResourceLines(warm.configYAML))
	return rr, nil
}

// reloadImpact diffs the fresh render's plan against the baseline (the pinned
// render if set, otherwise the previous one) with deployplan.Diff — the same
// decision layer the deployer runs per pod. It returns a JS-ready verdict, or
// nil on the first render (no baseline). It also advances prevBaseline.
func reloadImpact(out *previewResult, ver *dataplane.Version) any {
	baseline := pinnedBaseline
	if baseline == nil {
		baseline = prevBaseline
	}
	var res any
	if baseline != nil && baseline.plan != nil && out.Plan != nil {
		decision := deployplan.Diff(out.Plan, &deployplan.Baseline{
			Applied:   baseline.plan,
			Running:   baseline.plan,
			WorkerOps: baseline.plan,
			Inventory: loadedInventory(baseline.plan),
			Caps:      deployplan.CapsFor(ver.Full, nil),
		})
		res = decisionToJS(&decision, out.Plan.ID != baseline.plan.ID, pinnedBaseline != nil)
	}
	prevBaseline = &renderBaseline{plan: out.Plan, aux: out.AuxiliaryFiles}
	return res
}

// loadedInventory is what a pod running plan would have loaded at runtime. The
// playground has no pod, so the plan's own files stand in: everything the
// baseline render declared is what the imagined worker holds.
func loadedInventory(plan *renderplan.Plan) api.Inventory {
	inv := api.Inventory{}
	for i := range plan.Files {
		file := &plan.Files[i]
		switch file.Kind {
		case renderplan.FileKindMap:
			inv.Maps = append(inv.Maps, file.Path)
		case renderplan.FileKindCert:
			inv.Certs = append(inv.Certs, file.Path)
		case renderplan.FileKindCA:
			inv.CAFiles = append(inv.CAFiles, file.Path)
		case renderplan.FileKindCRTList:
			inv.CRTLists = append(inv.CRTLists, file.Path)
		}
	}
	return inv
}

// decisionToJS flattens a deployplan.Decision into the JS verdict shape: the
// reasons are the changes that cost more than a runtime op, the ops are what
// applies over the socket.
func decisionToJS(d *deployplan.Decision, changed, pinned bool) map[string]any {
	reload := d.Verdict == deployplan.VerdictReload
	details := make([]any, 0, len(d.Reasons)+len(d.Ops))
	for _, reason := range d.Reasons {
		details = append(details, map[string]any{"text": reason, "reload": reload})
	}
	counts := map[string]int{}
	kinds := make([]string, 0, len(d.Ops))
	for i := range d.Ops {
		kind := d.Ops[i].Kind
		if counts[kind] == 0 {
			kinds = append(kinds, kind)
		}
		counts[kind]++
	}
	for _, kind := range kinds {
		details = append(details, map[string]any{
			"text":   fmt.Sprintf("%s × %d", kind, counts[kind]),
			"reload": false,
		})
	}
	structural := 0
	if reload {
		structural = len(d.Reasons)
	}
	return map[string]any{
		"pinned":          pinned,
		"changed":         changed,
		"wouldReload":     reload,
		"runtimeEligible": float64(len(d.Ops)),
		"structural":      float64(structural),
		"details":         details,
	}
}

// cleanSchemaError strips the schema-validator's wrapper prefix and bullet
// markers that SimplifyValidationError leaves on the "API schema validation
// failed:\n  - <detail>" error shape, keeping just the constraint detail(s).
func cleanSchemaError(msg string) string {
	if i := strings.LastIndex(msg, "API schema validation failed:"); i >= 0 {
		msg = msg[i+len("API schema validation failed:"):]
	}
	var details []string
	for _, line := range strings.Split(msg, "\n") {
		if line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- ")); line != "" {
			details = append(details, line)
		}
	}
	if len(details) == 0 {
		return strings.TrimSpace(msg)
	}
	return strings.Join(details, "; ")
}

// migrationReport classifies every pasted Ingress's annotations and every
// source-controller ConfigMap's keys against the separately loaded coverage
// assets. It returns nil when no coverage is loaded or neither was pasted. RenderError stays empty because the render tabs
// carry that verdict.
func migrationReport(coverage []migratecheck.CoverageSource, resourcesYAML []byte) any {
	if len(coverage) == 0 {
		return nil
	}
	docs, err := decodeYAMLDocuments(resourcesYAML)
	if err != nil {
		return nil
	}
	var ings []migratecheck.Ingress
	var configMaps []migratecheck.ConfigMap
	for _, doc := range docs {
		for _, obj := range expandList(doc) {
			m, ok := obj.(map[string]any)
			if !ok {
				continue
			}
			u := &unstructured.Unstructured{Object: m}
			switch kind, _ := m["kind"].(string); kind {
			case "Ingress":
				ings = append(ings, migratecheck.FromUnstructured(u))
			case "ConfigMap":
				configMaps = append(configMaps, migratecheck.ConfigMapFromUnstructured(u))
			}
		}
	}
	report := migratecheck.Classify(coverage, ings, configMaps)
	if len(ings) == 0 && report.TotalConfigMaps == 0 {
		return nil
	}
	return migrationReportToJS(report)
}

// toRenderResult flattens the render output into the JS-boundary shape.
func toRenderResult(out *previewResult) *renderResult {
	rr := &renderResult{
		HAProxyConfig: out.HAProxyConfig,
		DurationMs:    out.DurationMs,
		Maps:          map[string]string{},
		Files:         map[string]string{},
		Certs:         map[string]string{},
		CRTLists:      map[string]string{},
		StatusPatches: map[string]string{},
		Applied:       map[string]string{},
	}
	if out.AuxiliaryFiles != nil {
		for _, m := range out.AuxiliaryFiles.MapFiles {
			rr.Maps[m.Path] = m.Content
		}
		for _, f := range out.AuxiliaryFiles.GeneralFiles {
			rr.Files[f.Filename] = f.Content
		}
		for _, c := range out.AuxiliaryFiles.SSLCertificates {
			rr.Certs[c.Path] = c.Content
		}
		rr.CertsDecoded = decodeCerts(rr.Certs, time.Now())
		for _, c := range out.AuxiliaryFiles.CRTListFiles {
			rr.CRTLists[c.Path] = c.Content
		}
	}
	for _, s := range out.IncludeStats {
		rr.Trace = append(rr.Trace, map[string]any{
			"name": s.Name, "count": float64(s.Count),
			"totalMs": s.TotalMs, "avgMs": s.AvgMs, "maxMs": s.MaxMs,
		})
	}
	// Status write-backs the controller applies to watched resources' .status. A
	// patch declares one payload per pipeline phase; show them all, labelled, so
	// the phase is explicit. Runtime-derived values (e.g. a LoadBalancer address
	// from address discovery) are null here — the playground renders but never
	// deploys, so those are filled from the live cluster at deploy time.
	for _, sp := range out.StatusPatches {
		var b strings.Builder
		for _, phase := range []string{"rendered", "deployed", "renderFailed", "deployFailed"} {
			variant, ok := sp.Variants[phase]
			if !ok {
				continue
			}
			y, err := yaml.Marshal(map[string]any{"status": variant})
			if err != nil {
				continue
			}
			fmt.Fprintf(&b, "# %s phase\n%s\n", phase, y)
		}
		if b.Len() > 0 {
			rr.StatusPatches[targetKey(sp.Kind, sp.Namespace, sp.Name)] = b.String()
		}
	}
	// Kubernetes Events templates recorded via recordEvent() (e.g. a
	// RouteConflict Warning on an Ingress that lost its route). Format matches
	// what `kubectl get events` shows: "<Type> <Reason> <apiVersion> <Kind>
	// <ns>/<name>: <message>".
	for _, e := range out.Events {
		rr.Events = append(rr.Events, fmt.Sprintf("%s %s %s %s %s/%s: %s",
			e.Type, e.Reason, e.APIVersion, e.Kind, e.Namespace, e.Name, e.Message))
	}
	// Full Kubernetes objects the templates declare (spec.k8sResources) and the
	// controller owns + applies via server-side apply (e.g. the HAProxy Service).
	for _, res := range out.RenderedResources {
		if y, err := yaml.Marshal(res.Object); err == nil {
			rr.Applied[targetKey(res.Kind, res.Namespace, res.Name)] = string(y)
		}
	}
	return rr
}

// parseConfigSpec decodes a HAProxyTemplateConfig, accepting either the full
// Kubernetes resource form or a bare spec. YAML-only (no k8s codec) keeps the
// wasm small.
func parseConfigSpec(data []byte) (*v1alpha1.HAProxyTemplateConfigSpec, error) {
	// A `kubectl get htplcfg -A -o yaml` dump is a List wrapper; unwrap the first
	// config so users can paste that output verbatim. (A bare spec / single object
	// has no "items", so this branch is skipped for them.)
	var list struct {
		Items []v1alpha1.HAProxyTemplateConfig `json:"items"`
	}
	if err := yaml.Unmarshal(data, &list); err == nil && len(list.Items) > 0 {
		return &list.Items[0].Spec, nil
	}

	var full v1alpha1.HAProxyTemplateConfig
	if err := yaml.Unmarshal(data, &full); err == nil {
		if full.Kind == "HAProxyTemplateConfig" || full.Spec.HAProxyConfig.Template != "" {
			return &full.Spec, nil
		}
	}

	var bare v1alpha1.HAProxyTemplateConfigSpec
	if err := yaml.Unmarshal(data, &bare); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &bare, nil
}

// schemaBundleEntry is one resolved schema in the JSON bundle the host build
// step produces (via schemafetcher.DirFetcher over tests/schemas). Keyed by
// "<apiVersion>|<plural>" so watched resources resolve to a GVK without any
// singularization guessing.
type schemaBundleEntry struct {
	Group   string          `json:"group"`
	Version string          `json:"version"`
	Kind    string          `json:"kind"`
	Schema  json.RawMessage `json:"schema"`
}

// parseSchemaBundle parses the JSON schema bundle into a GVK->schema map (for the
// MapFetcher) and an "<apiVersion>|<plural>"->GVK map (for resolving watched
// resources to kinds). Both are nil when no bundle is supplied — the untyped
// path, where resources reach templates through dig().
// bundleServedChecker implements config.ServedVersionChecker against the offline
// schema bundle. A (apiVersion, plural) pair is served when the bundle carries
// its schema. If the plural is present under a *different* version, this version
// isn't served (so ResolveEffective prefers the bundled one). If the plural is
// absent from the bundle entirely — an untyped/custom resource the bundle
// doesn't cover — it's treated as served so those configs still render untyped,
// preserving the pre-resolution leniency.
type bundleServedChecker struct {
	byKey map[string]schema.GroupVersionKind
}

func (c bundleServedChecker) IsServed(apiVersion, resources string) bool {
	if _, ok := c.byKey[apiVersion+"|"+resources]; ok {
		return true
	}
	for key := range c.byKey {
		if strings.HasSuffix(key, "|"+resources) {
			return false
		}
	}
	return true
}

func parseSchemaBundle(schemasJSON []byte) (map[schema.GroupVersionKind]*spec.Schema, map[string]schema.GroupVersionKind, error) {
	trimmed := strings.TrimSpace(string(schemasJSON))
	if trimmed == "" || trimmed == "{}" {
		return nil, nil, nil
	}

	var bundle map[string]schemaBundleEntry
	if err := json.Unmarshal(schemasJSON, &bundle); err != nil {
		return nil, nil, fmt.Errorf("parsing schema bundle: %w", err)
	}

	byGVK := make(map[schema.GroupVersionKind]*spec.Schema, len(bundle))
	byKey := make(map[string]schema.GroupVersionKind, len(bundle))
	for key, e := range bundle {
		var sch spec.Schema
		if err := json.Unmarshal(e.Schema, &sch); err != nil {
			return nil, nil, fmt.Errorf("parsing schema for %q: %w", key, err)
		}
		gvk := schema.GroupVersionKind{Group: e.Group, Version: e.Version, Kind: e.Kind}
		byGVK[gvk] = &sch
		byKey[key] = gvk
	}
	return byGVK, byKey, nil
}

// bootstrapTypes returns the typed-resource result the engine and render context
// compile against, mirroring the controller's production type-bootstrap.
//
// With no schema bundle it returns an empty Result and every watched resource
// falls through to the untyped resources[...] path via dig() — the same
// behaviour as the offline validate CLI without --schema-dir. With a bundle it
// builds an in-memory schemafetcher.MapFetcher (no host filesystem), resolves
// each watched resource's (apiVersion, plural) to a GVK from the bundle, and
// runs the production typebootstrap so chart templates that use typed globals
// (e.g. currentConfig, ingresses[i].Spec) compile against the same shapes the
// controller uses.
func bootstrapTypes(cfg *config.Config, byGVK map[schema.GroupVersionKind]*spec.Schema, byKey map[string]schema.GroupVersionKind, logger *slog.Logger) (*typebootstrap.Result, error) {
	if len(byGVK) == 0 {
		// Non-nil empty Result: BuildAdditionalDeclarations panics on a nil *Result.
		return &typebootstrap.Result{}, nil
	}

	resources := make([]typebootstrap.Resource, 0, len(cfg.WatchedResources))
	for name := range cfg.WatchedResources {
		wr := cfg.WatchedResources[name]
		gvk, found := resolveWatchedGVK(byKey, wr)
		if !found {
			// No bundled schema for this resource: it stays on the untyped path.
			continue
		}
		resources = append(resources, typebootstrap.Resource{Name: name, GVK: gvk})
	}

	return typebootstrap.Bootstrap(context.Background(), typebootstrap.Config{
		Resources:          resources,
		GlobalIgnoreFields: cfg.WatchedResourcesIgnoreFields,
		Fetcher:            schemafetcher.NewMapFetcher(byGVK),
		Logger:             logger,
	})
}

// resolveWatchedGVK finds the bundled GVK for a watched resource, trying each of
// its candidate apiVersions against the "<apiVersion>|<plural>" index.
func resolveWatchedGVK(byKey map[string]schema.GroupVersionKind, wr config.WatchedResource) (schema.GroupVersionKind, bool) {
	for _, apiVersion := range watchedAPIVersions(&wr) {
		if gvk, ok := byKey[apiVersion+"|"+wr.Resources]; ok {
			return gvk, true
		}
	}
	return schema.GroupVersionKind{}, false
}

// renderPaths returns in-memory HAProxy target paths as plain strings. No
// directory is created or read: RenderService derives its own PathResolver from
// cfg.Dataplane, and the store-building path never touches these. They exist
// only to satisfy testrunner.New's non-nil expectation.
func renderPaths() *dataplane.ValidationPaths {
	return &dataplane.ValidationPaths{
		TempDir:           "/render",
		MapsDir:           "/render/maps",
		SSLCertsDir:       "/render/ssl",
		CRTListDir:        "/render/ssl",
		GeneralStorageDir: "/render/general",
		SecretDir:         "/render/secrets",
		ConfigFile:        "/render/haproxy.cfg",
	}
}

// jsError wraps an error message in the object shape the host expects.
func jsError(msg string) any {
	return map[string]any{"error": msg}
}

// toAnyMap converts a string map into the map[string]any syscall/js can marshal.
func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
