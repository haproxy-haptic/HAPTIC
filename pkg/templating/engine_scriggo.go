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

package templating

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/ast"
	"github.com/haproxy-haptic/scriggo/ast/astutil"
	"github.com/haproxy-haptic/scriggo/native"
)

// ScriggoEngine provides template compilation and rendering capabilities using Scriggo.
// It pre-compiles all templates at initialization for optimal runtime performance
// and early detection of syntax errors.
//
// Scriggo uses Go template syntax, which is different from Jinja2:
//   - Loops: {% for x := range items %}...{% end %}
//   - Conditionals: {% if cond %}...{% else if other %}...{% end %}
//   - Variables: {{ .name }} or {{ name }} when in globals
//
// This engine offers excellent performance and low memory usage with Go-style
// template syntax.
type ScriggoEngine struct {
	exactCycleRootEntryPointsOnce sync.Once
	exactCycleRootEntryPointsMemo []string
	rawTemplates                  map[string]string
	compiledTemplates             map[string]*scriggo.Template
	incrementalEntryPoints        map[string]struct{}
	incrementalVectorEntryPoints  map[string]*incrementalVectorEntryPoint
	incrementalVectorCarrier      *incrementalVectorCarrier
	incrementalVectorCarrierError error
	incrementalResourceBindings   map[string]*incrementalResourceBindingPlan
	incrementalBindingEntryPoints map[string]struct{}
	incrementalBindingInputs      map[string][]string
	usedGlobals                   map[string]struct{}
	globalUsageUnknown            bool
	postProcessors                map[string][]PostProcessor
	postProcessCache              *postProcessCache
	postProcessCacheIdentities    map[string]*postProcessCacheIdentity
	postProcessReuseProofs        map[string]*PostProcessReuseProof
	customDeclarationNames        map[string]struct{}
	additionalDeclarationNames    map[string]struct{}
	tracing                       *scriggoTracingConfig
	globals                       native.Declarations

	// Profiling support using Scriggo's built-in profiler
	profilingEnabled bool
	lastProfile      *scriggo.Profile
	profilingMu      sync.Mutex // Protects lastProfile
}

// Verify ScriggoEngine implements Engine interface at compile time.
var _ Engine = (*ScriggoEngine)(nil)
var _ GlobalUsageIntrospector = (*ScriggoEngine)(nil)
var _ IncrementalComponentBatchExecutor = (*ScriggoEngine)(nil)
var _ IncrementalComponentVectorRenderer = (*ScriggoEngine)(nil)
var _ IncrementalComponentVectorCarrierWavesRenderer = (*ScriggoEngine)(nil)
var _ IncrementalComponentSourceTransactionsRenderer = (*ScriggoEngine)(nil)
var _ IncrementalResourceBinder = (*ScriggoEngine)(nil)
var _ IncrementalSourceTransactionResourceBinder = (*ScriggoEngine)(nil)
var _ RawTextRenderer = (*ScriggoEngine)(nil)
var _ PostProcessReuseProver = (*ScriggoEngine)(nil)

// Options configures a template engine. The zero value (or a nil *Options)
// compiles every template as an entry point, with no custom filters,
// functions, post-processors, type declarations, or profiling.
type Options struct {
	// EntryPoints lists template names to compile explicitly; the remaining
	// templates are snippets, discovered and compiled automatically when
	// referenced via render/render_glob statements with inherit_context.
	// nil means every template is an entry point.
	EntryPoints []string
	// IncrementalEntryPoints is the subset of EntryPoints compiled against
	// the deterministic incremental declaration set.
	IncrementalEntryPoints []string
	// IncrementalBindingEntryPoints is the subset compiled as deterministic
	// dynamic binding planners.
	IncrementalBindingEntryPoints []string
	// Filters are custom filters merged over the built-in set (can be nil).
	Filters map[string]FilterFunc
	// Functions are custom global functions merged over the built-in set
	// (can be nil).
	Functions map[string]GlobalFunc
	// PostProcessors configures per-template post-processing (can be nil).
	PostProcessors map[string][]PostProcessorConfig
	// Declarations registers domain-specific types with Scriggo (e.g.
	// currentConfig for slot-aware server assignment). Use this when
	// templates need access to types from other packages (can be nil).
	Declarations map[string]any
	// Profiling enables Scriggo's built-in profiler, which collects timing
	// data for function calls, macros, and includes during execution
	// (see RenderWithProfiling). Profiling adds minimal runtime overhead.
	Profiling bool
}

// New creates a Scriggo (Go template syntax) template engine.
//
// Only opts.EntryPoints are compiled explicitly — or every template, when
// opts is nil or opts.EntryPoints is nil — so syntax errors are caught
// early. Templates not listed as entry points are snippets, compiled
// automatically when referenced via render/render_glob statements with
// inherit_context.
func New(templates map[string]string, opts *Options) (*ScriggoEngine, error) {
	if opts == nil {
		opts = &Options{}
	}
	entryPoints := opts.EntryPoints
	if entryPoints == nil {
		entryPoints = make([]string, 0, len(templates))
		for name := range templates {
			entryPoints = append(entryPoints, name)
		}
	}
	if err := validateIncrementalEntryPoints(entryPoints, opts.IncrementalEntryPoints); err != nil {
		return nil, err
	}
	if err := validateIncrementalBindingEntryPoints(entryPoints, opts.IncrementalBindingEntryPoints); err != nil {
		return nil, err
	}
	if err := validateDistinctIncrementalEntryPoints(
		opts.IncrementalEntryPoints,
		opts.IncrementalBindingEntryPoints,
	); err != nil {
		return nil, err
	}
	return newScriggoEngine(
		templates,
		entryPoints,
		opts.IncrementalEntryPoints,
		opts.IncrementalBindingEntryPoints,
		opts.Filters,
		opts.Functions,
		opts.PostProcessors,
		opts.Declarations,
		opts.Profiling,
	)
}

func validateIncrementalEntryPoints(entryPoints, incrementalEntryPoints []string) error {
	return validateEntryPointSubset("incremental", entryPoints, incrementalEntryPoints)
}

func validateIncrementalBindingEntryPoints(entryPoints, bindingEntryPoints []string) error {
	return validateEntryPointSubset("incremental binding", entryPoints, bindingEntryPoints)
}

func validateEntryPointSubset(kind string, entryPoints, subset []string) error {
	if len(subset) == 0 {
		return nil
	}
	entryPointNames := make(map[string]struct{}, len(entryPoints))
	for _, name := range entryPoints {
		entryPointNames[name] = struct{}{}
	}
	for _, name := range subset {
		if _, ok := entryPointNames[name]; !ok {
			return fmt.Errorf("%s entry point %q is not in EntryPoints", kind, name)
		}
	}
	return nil
}

func validateDistinctIncrementalEntryPoints(componentEntryPoints, bindingEntryPoints []string) error {
	components := make(map[string]struct{}, len(componentEntryPoints))
	for _, name := range componentEntryPoints {
		components[name] = struct{}{}
	}
	for _, name := range bindingEntryPoints {
		if _, exists := components[name]; exists {
			return fmt.Errorf("entry point %q cannot be both an incremental component and binding planner", name)
		}
	}
	return nil
}

// newScriggoEngine is the internal constructor that handles both profiling and non-profiling modes.
//
// Parameters:
//   - templates: All template content (entry points + snippets) for the filesystem
//   - entryPoints: Template names to compile explicitly
//   - customFilters, customFunctions, postProcessorConfigs: Standard engine options
//   - additionalDeclarations: Domain-specific type declarations for Scriggo (can be nil)
//   - enableProfiling: Whether to enable Scriggo's built-in profiler
//
// Only entryPoints are compiled explicitly. Template snippets in templates but not in
// entryPoints are discovered and compiled automatically by Scriggo when referenced
// via render/render_glob statements with inherit_context.
func newScriggoEngine(
	templates map[string]string,
	entryPoints []string,
	incrementalEntryPoints []string,
	incrementalBindingEntryPoints []string,
	customFilters map[string]FilterFunc,
	customFunctions map[string]GlobalFunc,
	postProcessorConfigs map[string][]PostProcessorConfig,
	additionalDeclarations map[string]any,
	enableProfiling bool,
) (*ScriggoEngine, error) {
	engine := &ScriggoEngine{
		rawTemplates:                  make(map[string]string, len(templates)),
		compiledTemplates:             make(map[string]*scriggo.Template, len(entryPoints)),
		incrementalEntryPoints:        make(map[string]struct{}, len(incrementalEntryPoints)),
		incrementalVectorEntryPoints:  make(map[string]*incrementalVectorEntryPoint, len(incrementalEntryPoints)),
		incrementalResourceBindings:   make(map[string]*incrementalResourceBindingPlan, len(incrementalEntryPoints)),
		incrementalBindingEntryPoints: make(map[string]struct{}, len(incrementalBindingEntryPoints)),
		incrementalBindingInputs:      make(map[string][]string, len(incrementalBindingEntryPoints)),
		usedGlobals:                   make(map[string]struct{}),
		globalUsageUnknown:            declarationsHaveUnknownGlobalUsage(additionalDeclarations),
		postProcessors:                make(map[string][]PostProcessor),
		postProcessCache:              newPostProcessCache(),
		postProcessCacheIdentities:    make(map[string]*postProcessCacheIdentity),
		postProcessReuseProofs:        make(map[string]*PostProcessReuseProof),
		customDeclarationNames:        make(map[string]struct{}, len(customFilters)+len(customFunctions)),
		additionalDeclarationNames:    make(map[string]struct{}, len(additionalDeclarations)),
		tracing: &scriggoTracingConfig{
			enabled: false,
			traces:  nil,
		},
		profilingEnabled: enableProfiling,
	}
	for name := range customFilters {
		engine.customDeclarationNames[name] = struct{}{}
	}
	for name := range customFunctions {
		if name != FuncFail {
			engine.customDeclarationNames[name] = struct{}{}
		}
	}
	for name := range additionalDeclarations {
		engine.additionalDeclarationNames[name] = struct{}{}
	}
	for _, name := range incrementalEntryPoints {
		engine.incrementalEntryPoints[name] = struct{}{}
	}
	for _, name := range incrementalBindingEntryPoints {
		engine.incrementalBindingEntryPoints[name] = struct{}{}
	}

	// Build globals (filters become functions in Scriggo)
	engine.globals = buildScriggoGlobals(customFilters, customFunctions, additionalDeclarations)

	// Override sort_by with a debug-aware variant: when EnableFilterDebug()
	// (the `validate --debug-filters` flag) is active, it logs each comparison.
	// The flag is read dynamically so a post-construction toggle (the testrunner
	// builds worker engines and enables debug afterwards) is honored.
	//
	// This must stay an AdaptiveFunc to match the declaration
	// registerScriggoCustomFunctions installs — a plain func here would shadow
	// it and silently drop both the comparator call shape and type preservation.
	engine.globals[FilterSortBy] = native.Synchronous(sortByAdaptive(engine.IsFilterDebugEnabled))

	// Store raw templates (all templates, not just entry points)
	maps.Copy(engine.rawTemplates, templates)

	// Compile only entry points (snippets compiled on-demand by Scriggo)
	if err := engine.compileTemplates(
		templates,
		entryPoints,
		incrementalEntryPoints,
		incrementalBindingEntryPoints,
		additionalDeclarations,
	); err != nil {
		return nil, err
	}

	if err := buildScriggoPostProcessors(engine, postProcessorConfigs); err != nil {
		return nil, err
	}

	return engine, nil
}

// compileTemplates compiles entry point templates using Scriggo.
//
// Only templates listed in entryPoints are compiled explicitly. Template snippets
// are discovered and compiled automatically by Scriggo when referenced via
// render/render_glob statements with inherit_context.
//
// Parameters:
//   - allTemplates: All template content (for filesystem - includes snippets)
//   - entryPoints: Template names to compile explicitly
//
// The filesystem contains ALL templates so Scriggo can discover snippets, but only
// entryPoints are compiled into e.compiledTemplates.
//
// If profiling is enabled, BuildOptions.EnableProfiling is set to enable Scriggo's built-in profiler.
func (e *ScriggoEngine) compileTemplates(
	allTemplates map[string]string,
	entryPoints []string,
	incrementalEntryPoints []string,
	incrementalBindingEntryPoints []string,
	additionalDeclarations map[string]any,
) error {
	incrementalNames := make(map[string]struct{}, len(incrementalEntryPoints))
	for _, name := range incrementalEntryPoints {
		incrementalNames[name] = struct{}{}
	}
	var incrementalGlobals native.Declarations
	var incrementalResourcesType reflect.Type
	if len(incrementalNames) > 0 {
		incrementalGlobals = buildScriggoIncrementalGlobals(additionalDeclarations, e.IsFilterDebugEnabled)
		incrementalResourcesType = incrementalResourceDeclarationType(incrementalGlobals[declResources])
	}
	bindingNames := make(map[string]struct{}, len(incrementalBindingEntryPoints))
	for _, name := range incrementalBindingEntryPoints {
		bindingNames[name] = struct{}{}
	}
	privateNames := make(map[string]struct{}, len(incrementalNames)+len(bindingNames))
	maps.Copy(privateNames, incrementalNames)
	maps.Copy(privateNames, bindingNames)
	var bindingGlobals native.Declarations
	if len(bindingNames) > 0 {
		bindingGlobals = buildScriggoIncrementalBindingGlobals(additionalDeclarations, e.IsFilterDebugEnabled)
	}

	scope := &scriggoEntryPointScope{
		allTemplates:             allTemplates,
		privateNames:             privateNames,
		incrementalNames:         incrementalNames,
		bindingNames:             bindingNames,
		incrementalGlobals:       incrementalGlobals,
		bindingGlobals:           bindingGlobals,
		incrementalResourcesType: incrementalResourcesType,
	}
	// Only compile entry points
	for _, name := range entryPoints {
		if err := e.compileEntryPoint(name, scope); err != nil {
			return err
		}
	}

	e.incrementalVectorCarrier, e.incrementalVectorCarrierError = compileIncrementalVectorCarrier(
		allTemplates,
		privateNames,
		e.compiledTemplates,
		e.incrementalVectorEntryPoints,
		incrementalGlobals,
		e.profilingEnabled,
	)

	return nil
}

// scriggoEntryPointScope is the per-compileTemplates state every entry point
// compiles against.
type scriggoEntryPointScope struct {
	allTemplates             map[string]string
	privateNames             map[string]struct{}
	incrementalNames         map[string]struct{}
	bindingNames             map[string]struct{}
	incrementalGlobals       native.Declarations
	bindingGlobals           native.Declarations
	incrementalResourcesType reflect.Type
}

func (e *ScriggoEngine) compileEntryPoint(name string, scope *scriggoEntryPointScope) error {
	_, incremental := scope.incrementalNames[name]
	_, binding := scope.bindingNames[name]
	fsys := &scriggoTemplateFS{templates: scope.allTemplates, hiddenTemplates: scope.privateNames}
	globals := e.globals
	if incremental {
		fsys.exposedTemplate = name
		globals = scope.incrementalGlobals
	} else if binding {
		fsys.exposedTemplate = name
		globals = scope.bindingGlobals
	}
	opts := &scriggo.BuildOptions{
		Globals:         globals,
		EnableProfiling: e.profilingEnabled,
		// Parallel rendering uses the expression form ({{ go Macro(...) }}),
		// which compiles to OpGoRender and is not gated by this flag. The flag
		// gates only {% go f() %}, whose goroutine outlives the render and
		// whose panic is unrecovered.
		AllowGoStmt:         false,
		ExpandedTransformer: distributeOnOwnLines,
	}
	if incremental {
		opts.UnexpandedTransformer = rejectDerivedRenderModeOverride
	}

	compiled, err := scriggo.BuildTemplate(fsys, name, opts)
	if err != nil {
		return NewCompilationError(name, scope.allTemplates[name], err)
	}
	if incremental {
		if err := e.compileIncrementalEntryPointArtifacts(
			scope.allTemplates, scope.privateNames, name, compiled,
			scope.incrementalGlobals, scope.incrementalResourcesType,
		); err != nil {
			return err
		}
	}
	if binding {
		if err := compiled.DeterministicSafe(); err != nil {
			return NewCompilationError(
				name,
				scope.allTemplates[name],
				fmt.Errorf("incremental binding planner is not deterministic: %w", err),
			)
		}
	}

	usedVariables := compiled.UsedVars()
	for _, variable := range usedVariables {
		e.usedGlobals[variable] = struct{}{}
	}
	e.compiledTemplates[name] = compiled
	if binding {
		e.incrementalBindingInputs[name] = usedIncrementalBindingInputs(usedVariables)
	}
	return nil
}

func (e *ScriggoEngine) compileIncrementalEntryPointArtifacts(
	allTemplates map[string]string,
	privateNames map[string]struct{},
	name string,
	compiled *scriggo.Template,
	incrementalGlobals native.Declarations,
	incrementalResourcesType reflect.Type,
) error {
	if err := validateIncrementalComponentProgram(compiled); err != nil {
		return NewCompilationError(name, allTemplates[name], err)
	}
	vector := compileIncrementalVectorEntryPoint(
		allTemplates,
		privateNames,
		name,
		compiled,
		incrementalGlobals,
		e.profilingEnabled,
	)
	var plan *incrementalResourceBindingPlan
	if incrementalResourcesType != nil {
		var planErr error
		plan, planErr = compileIncrementalResourceBindingPlan(compiled, incrementalResourcesType)
		if planErr != nil {
			return NewCompilationError(name, allTemplates[name], planErr)
		}
	}
	e.incrementalVectorEntryPoints[name] = vector
	e.incrementalResourceBindings[name] = plan
	return nil
}

func validateIncrementalComponentProgram(compiled *scriggo.Template) error {
	if err := compiled.DeterministicSafe(); err != nil {
		return fmt.Errorf("incremental component is not deterministic: %w", err)
	}
	if err := compiled.RunBatch(nil); err != nil {
		return fmt.Errorf("incremental component is not certified for batch execution: %w", err)
	}
	return nil
}

func rejectDerivedRenderModeOverride(tree *ast.Tree) error {
	var override *ast.Position
	astutil.Inspect(tree, func(node ast.Node) bool {
		if override != nil || node == nil {
			return false
		}
		override = derivedRenderModeOverride(node)
		return override == nil
	})
	if override != nil {
		return fmt.Errorf("derived renderMode cannot be declared or assigned at %s", override)
	}
	return nil
}

func derivedRenderModeOverride(node ast.Node) *ast.Position {
	switch value := node.(type) {
	case *ast.Assignment:
		for _, lhs := range value.Lhs {
			if ident, ok := lhs.(*ast.Identifier); ok && isRenderModeIdentifier(ident) {
				return ident.Position
			}
		}
	case *ast.Const:
		return identifierListPosition(value.Lhs, value.Position)
	case *ast.ForIn:
		return identifierPosition(value.Ident)
	case *ast.Func:
		return functionRenderModeOverride(value)
	case *ast.Import:
		if isRenderModeIdentifier(value.Ident) || hasRenderModeIdentifier(value.For) {
			return value.Position
		}
	case *ast.TypeDeclaration:
		return identifierPosition(value.Ident)
	case *ast.Var:
		return identifierListPosition(value.Lhs, value.Position)
	}
	return nil
}

func functionRenderModeOverride(function *ast.Func) *ast.Position {
	if position := identifierPosition(function.Ident); position != nil {
		return position
	}
	parameters := append(slices.Clone(function.Type.Parameters), function.Type.Result...)
	for _, parameter := range parameters {
		if position := identifierPosition(parameter.Ident); position != nil {
			return position
		}
	}
	return nil
}

func identifierListPosition(identifiers []*ast.Identifier, position *ast.Position) *ast.Position {
	if hasRenderModeIdentifier(identifiers) {
		return position
	}
	return nil
}

func hasRenderModeIdentifier(identifiers []*ast.Identifier) bool {
	return slices.ContainsFunc(identifiers, isRenderModeIdentifier)
}

func identifierPosition(identifier *ast.Identifier) *ast.Position {
	if isRenderModeIdentifier(identifier) {
		return identifier.Position
	}
	return nil
}

func isRenderModeIdentifier(identifier *ast.Identifier) bool {
	return identifier != nil && identifier.Name == declRenderMode
}

func declarationsHaveUnknownGlobalUsage(declarations map[string]any) bool {
	for _, declaration := range declarations {
		if declarationHasUnknownGlobalUsage(declaration) {
			return true
		}
	}
	return false
}

func declarationHasUnknownGlobalUsage(declaration any) bool {
	switch value := declaration.(type) {
	case native.AdaptiveFunc, *native.AdaptiveFunc:
		return true
	case native.ImportablePackage:
		unknown := false
		err := value.LookupFunc(func(_ string, nested native.Declaration) error {
			if declarationHasUnknownGlobalUsage(nested) {
				unknown = true
				return native.StopLookup
			}
			return nil
		})
		return err != nil || unknown
	case reflect.Type:
		return declarationTypeUsesNativeEnvironment(value, map[reflect.Type]struct{}{})
	}
	typeOfDeclaration := reflect.TypeOf(declaration)
	if typeOfDeclaration == nil {
		return false
	}
	if typeOfDeclaration.Kind() == reflect.Func {
		return true
	}
	return declarationTypeUsesNativeEnvironment(typeOfDeclaration, map[reflect.Type]struct{}{})
}

func declarationTypeUsesNativeEnvironment(value reflect.Type, seen map[reflect.Type]struct{}) bool {
	if value == nil {
		return false
	}
	if _, visited := seen[value]; visited {
		return false
	}
	seen[value] = struct{}{}
	for index := range value.NumMethod() {
		if declarationFunctionUsesNativeEnvironment(value.Method(index).Type, seen) {
			return true
		}
	}
	switch value.Kind() {
	case reflect.Array, reflect.Chan, reflect.Pointer, reflect.Slice:
		return declarationTypeUsesNativeEnvironment(value.Elem(), seen)
	case reflect.Func:
		return declarationFunctionUsesNativeEnvironment(value, seen)
	case reflect.Interface:
		for index := range value.NumMethod() {
			if declarationFunctionUsesNativeEnvironment(value.Method(index).Type, seen) {
				return true
			}
		}
	case reflect.Map:
		return declarationTypeUsesNativeEnvironment(value.Key(), seen) ||
			declarationTypeUsesNativeEnvironment(value.Elem(), seen)
	case reflect.Struct:
		for index := range value.NumField() {
			if declarationTypeUsesNativeEnvironment(value.Field(index).Type, seen) {
				return true
			}
		}
	}
	return false
}

func declarationFunctionUsesNativeEnvironment(value reflect.Type, seen map[reflect.Type]struct{}) bool {
	environmentType := reflect.TypeFor[native.Env]()
	for index := range value.NumIn() {
		if value.In(index) == environmentType || declarationTypeUsesNativeEnvironment(value.In(index), seen) {
			return true
		}
	}
	for index := range value.NumOut() {
		if declarationTypeUsesNativeEnvironment(value.Out(index), seen) {
			return true
		}
	}
	return false
}

// SourceSpan / SourceFrame attribute a contiguous run of rendered output to the
// template include stack that produced it (see RenderWithSourceMap). They are
// aliases for the engine-level types so callers need not import the Scriggo package.
type SourceSpan = scriggo.SourceSpan
type SourceFrame = scriggo.SourceFrame

// RenderWithSourceMap renders a template like Render, but additionally returns a
// source map: one span per contiguous run of output attributing it to the
// template source (path + line) that produced it. The returned string is the
// RAW render output BEFORE post-processors run, so the span Length fields sum to
// its size exactly; callers that display the post-processed output must align
// the two (post-processors are whitespace-only for the bundled chart). Output
// from a parallel "{{ go … }}" render is attributed to the go-render call site.
func (e *ScriggoEngine) RenderWithSourceMap(ctx context.Context, templateName string, templateContext map[string]any) (raw string, spans []SourceSpan, err error) {
	if err := e.rejectPrivateIncrementalEntryPoint(templateName); err != nil {
		return "", nil, err
	}
	template, exists := e.compiledTemplates[templateName]
	if !exists {
		return "", nil, e.templateNotFoundError(templateName)
	}
	if templateContext == nil {
		templateContext = make(map[string]any)
	}
	if _, ok := templateContext[declShared]; !ok {
		templateContext[declShared] = NewSharedContext()
	}
	exactExecution, exact, err := exactCycleReplayExecutionFor(ctx, e, templateName)
	if err != nil {
		return "", nil, err
	}
	completeExactRoot := func(bool) {}
	if exact {
		completeExactRoot, err = exactExecution.beginRoot(ctx, templateName)
		if err != nil {
			return "", nil, err
		}
	}
	ctx, err = withRenderImmutableResourceInputs(ctx, templateContext)
	if err != nil {
		completeExactRoot(false)
		return "", nil, err
	}
	if exact {
		ctx = WithIncrementalImmutableInputs(ctx, exactExecution.program.immutableRootInputs(templateContext)...)
	}
	ctx = context.WithValue(ctx, RenderContextContextKey, templateContext)

	runOpts := &scriggo.RunOptions{
		Context:                   ctx,
		CollectSourceMap:          true,
		ObserveMutationContext:    observeIncrementalMutation,
		ObserveNativeCallContext:  observeIncrementalNativeCall,
		NativeFunctionTrampolines: incrementalRootFunctionFrameTrampolines,
		Deterministic:             exact,
	}
	var output strings.Builder
	if err := runScriggoTemplate(ctx, templateName, template, &output, templateContext, runOpts); err != nil {
		completeExactRoot(false)
		return "", nil, err
	}
	completeExactRoot(true)
	return output.String(), runOpts.SourceSpans, nil
}

// Render executes a template with the given context and returns the output.
func (e *ScriggoEngine) Render(ctx context.Context, templateName string, templateContext map[string]any) (string, error) {
	var output strings.Builder
	run, err := e.renderRawTo(ctx, templateName, templateContext, &output)
	if err != nil {
		return "", err
	}

	result := output.String()
	if result == "" || result[len(result)-1] != '\n' {
		result += "\n"
	}

	result, err = e.applyPostProcessors(ctx, templateName, result)
	if err != nil {
		return "", err
	}
	e.completeRender(templateName, run)
	return result, nil
}

// RenderRawTo streams a render before newline normalization and post-processing.
func (e *ScriggoEngine) RenderRawTo(
	ctx context.Context,
	templateName string,
	templateContext map[string]any,
	output io.Writer,
) ([]IncludeStats, error) {
	run, err := e.renderRawTo(ctx, templateName, templateContext, output)
	if err != nil {
		return nil, err
	}
	e.completeRender(templateName, run)
	if !e.profilingEnabled {
		return nil, nil
	}
	return aggregateScriggoProfile(run.profile), nil
}

// RawTextRenderInstrumented reports whether root post-processing must remain inside Render.
func (e *ScriggoEngine) RawTextRenderInstrumented() bool {
	return e.profilingEnabled || e.IsTracingEnabled()
}

type scriggoRenderRun struct {
	profile      *scriggo.Profile
	traceBuilder *strings.Builder
	started      time.Time
	tracing      bool
}

func (e *ScriggoEngine) renderRawTo(
	ctx context.Context,
	templateName string,
	templateContext map[string]any,
	output io.Writer,
) (*scriggoRenderRun, error) {
	if output == nil {
		return nil, errors.New("template output writer is nil")
	}
	if err := e.rejectPrivateIncrementalEntryPoint(templateName); err != nil {
		return nil, err
	}
	template, exists := e.compiledTemplates[templateName]
	if !exists {
		return nil, e.templateNotFoundError(templateName)
	}

	// Ensure template context exists with shared context for cross-template caching.
	// This allows first_seen and other cache functions to work even when caller
	// passes nil context (e.g., in tests).
	if templateContext == nil {
		templateContext = make(map[string]any)
	}
	if _, ok := templateContext[declShared]; !ok {
		templateContext[declShared] = NewSharedContext()
	}
	exactExecution, exact, err := exactCycleReplayExecutionFor(ctx, e, templateName)
	if err != nil {
		return nil, err
	}
	completeExactRoot := func(bool) {}
	if exact {
		completeExactRoot, err = exactExecution.beginRoot(ctx, templateName)
		if err != nil {
			return nil, err
		}
	}

	// Setup tracing if enabled
	e.tracing.mu.Lock()
	tracingEnabled := e.tracing.enabled
	e.tracing.mu.Unlock()

	var traceBuilder *strings.Builder
	var startTime time.Time
	if tracingEnabled {
		traceBuilder = &strings.Builder{}
		startTime = time.Now()
		fmt.Fprintf(traceBuilder, "Rendering: %s\n", templateName)
	}

	// Add render context (globals) for resource accessor functions like first_seen
	ctx, err = withRenderImmutableResourceInputs(ctx, templateContext)
	if err != nil {
		completeExactRoot(false)
		return nil, err
	}
	if exact {
		ctx = WithIncrementalImmutableInputs(ctx, exactExecution.program.immutableRootInputs(templateContext)...)
	}
	ctx = context.WithValue(ctx, RenderContextContextKey, templateContext)

	// Setup run options with profiling and parallelism settings
	runOpts := &scriggo.RunOptions{
		Context:                   ctx,
		ObserveMutationContext:    observeIncrementalMutation,
		ObserveNativeCallContext:  observeIncrementalNativeCall,
		NativeFunctionTrampolines: incrementalRootFunctionFrameTrampolines,
		Deterministic:             exact,
	}

	// Create profile receiver if profiling is enabled
	var profile *scriggo.Profile
	if e.profilingEnabled {
		profile = &scriggo.Profile{}
		runOpts.Profile = profile
	}

	if err := runScriggoTemplate(ctx, templateName, template, output, templateContext, runOpts); err != nil {
		completeExactRoot(false)
		return nil, err
	}
	completeExactRoot(true)

	return &scriggoRenderRun{
		profile: profile, traceBuilder: traceBuilder, started: startTime, tracing: tracingEnabled,
	}, nil
}

func (e *ScriggoEngine) completeRender(templateName string, run *scriggoRenderRun) {
	if run.tracing {
		e.storeTraceOutput(templateName, run.profile, run.traceBuilder, time.Since(run.started))
	}
	if run.profile != nil {
		e.profilingMu.Lock()
		e.lastProfile = run.profile
		e.profilingMu.Unlock()
	}
}

func (e *ScriggoEngine) rejectPrivateIncrementalEntryPoint(templateName string) error {
	if _, component := e.incrementalEntryPoints[templateName]; component {
		return fmt.Errorf("template %q is a private incremental entry point", templateName)
	}
	if _, planner := e.incrementalBindingEntryPoints[templateName]; planner {
		return fmt.Errorf("template %q is a private incremental entry point", templateName)
	}
	return nil
}

// RenderIncrementalComponent executes a component entry point without entry-point post-processing.
func (e *ScriggoEngine) RenderIncrementalComponent(
	ctx context.Context,
	templateName string,
	templateContext map[string]any,
) (string, error) {
	template, ctx, templateContext, err := e.prepareIncrementalComponent(
		ctx,
		templateName,
		templateContext,
	)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	runOptions := &scriggo.RunOptions{
		Context:                  ctx,
		Deterministic:            true,
		ObserveMutationContext:   observeIncrementalMutation,
		ObserveNativeCallContext: observeIncrementalNativeCall,
		BeforeNativeCallContext:  beforeIncrementalNativeCall,
	}
	if err := runScriggoTemplate(ctx, templateName, template, &output, templateContext, runOptions); err != nil {
		return "", err
	}
	return output.String(), nil
}

// RenderIncrementalComponents executes one private entry point over isolated
// contexts while reusing Scriggo's virtual machine and global slots.
func (e *ScriggoEngine) RenderIncrementalComponents(
	ctx context.Context,
	templateName string,
	items []IncrementalComponentBatchItem,
) ([]string, error) {
	return e.renderIncrementalComponentsRunBatch(ctx, templateName, items)
}

func (e *ScriggoEngine) renderIncrementalComponentsRunBatch(
	ctx context.Context,
	templateName string,
	items []IncrementalComponentBatchItem,
) ([]string, error) {
	if len(items) == 0 {
		return []string{}, nil
	}
	var template *scriggo.Template
	builders := make([]strings.Builder, len(items))
	runs := make([]scriggo.BatchRun, len(items))
	for index := range items {
		itemCtx := items[index].Context
		if itemCtx == nil {
			itemCtx = ctx
		}
		preparedTemplate, preparedCtx, preparedValues, err := e.prepareIncrementalComponent(
			itemCtx,
			templateName,
			items[index].TemplateContext,
		)
		if err != nil {
			return nil, &IncrementalComponentBatchError{Index: index, Err: err}
		}
		if template == nil {
			template = preparedTemplate
		}
		runs[index] = scriggo.BatchRun{
			Out:    &builders[index],
			Vars:   preparedValues,
			Before: items[index].Activate,
			After:  items[index].Deactivate,
			Options: &scriggo.RunOptions{
				Context:                   preparedCtx,
				Deterministic:             true,
				ObserveMutationContext:    observeIncrementalMutation,
				ObserveNativeCallContext:  observeIncrementalNativeCall,
				BeforeNativeCallContext:   beforeIncrementalNativeCall,
				NativeFunctionTrampolines: incrementalResourceNativeFunctionTrampolines(preparedValues[declResources]),
			},
		}
	}
	if err := runIncrementalComponentBatch(ctx, template, runs); err != nil {
		var batchErr *scriggo.BatchRunError
		if errors.As(err, &batchErr) {
			return nil, &IncrementalComponentBatchError{
				Index: batchErr.Index,
				Err:   NewRenderError(templateName, batchErr.Err),
			}
		}
		return nil, NewRenderError(templateName, err)
	}
	results := make([]string, len(builders))
	for index := range builders {
		results[index] = builders[index].String()
	}
	return results, nil
}

const incrementalBatchRunsPerWorker = 16

// runIncrementalComponentBatch spreads runs over up to GOMAXPROCS workers that
// take chunks in index order. Workers after the first take each chunk only if
// ctx's WorkerGate admits them, and stop when it refuses.
func runIncrementalComponentBatch(ctx context.Context, template *scriggo.Template, runs []scriggo.BatchRun) error {
	chunkCount := (len(runs) + incrementalBatchRunsPerWorker - 1) / incrementalBatchRunsPerWorker
	workerCount := min(runtime.GOMAXPROCS(0), chunkCount)
	if workerCount <= 1 {
		return template.RunBatch(runs)
	}
	if err := template.RunBatch(nil); err != nil {
		return err
	}
	gate := WorkerGateOf(ctx)
	if gate == nil {
		gate = admitAllWorkers{}
	}
	batch := &chunkedBatch{template: template, runs: runs, chunkCount: chunkCount, firstIndex: len(runs)}
	var workers sync.WaitGroup
	for range workerCount - 1 {
		workers.Go(func() { batch.runGated(gate) })
	}
	for batch.runNext() {
	}
	workers.Wait()
	return batch.firstErr
}

// chunkedBatch hands out a batch's runs in chunks and keeps the error with
// the lowest run index.
type chunkedBatch struct {
	template   *scriggo.Template
	runs       []scriggo.BatchRun
	chunkCount int
	nextChunk  atomic.Int64
	failed     atomic.Bool

	mu         sync.Mutex
	firstIndex int
	firstErr   error
}

func (b *chunkedBatch) runGated(gate WorkerGate) {
	for gate.TryAcquire() {
		more := b.runNext()
		gate.Release()
		if !more {
			return
		}
	}
}

// runNext runs the next chunk and reports whether another may follow.
func (b *chunkedBatch) runNext() bool {
	chunk := int(b.nextChunk.Add(1) - 1)
	if chunk >= b.chunkCount || b.failed.Load() {
		return false
	}
	start := chunk * incrementalBatchRunsPerWorker
	err := b.template.RunBatch(b.runs[start:min(start+incrementalBatchRunsPerWorker, len(b.runs))])
	if err != nil {
		b.fail(start, err)
		return false
	}
	return true
}

func (b *chunkedBatch) fail(start int, err error) {
	b.failed.Store(true)
	index := start
	var batchErr *scriggo.BatchRunError
	if errors.As(err, &batchErr) {
		index += batchErr.Index
		err = &scriggo.BatchRunError{Index: index, Err: batchErr.Err}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if index < b.firstIndex {
		b.firstIndex, b.firstErr = index, err
	}
}

func (e *ScriggoEngine) prepareIncrementalComponent(
	ctx context.Context,
	templateName string,
	templateContext map[string]any,
) (*scriggo.Template, context.Context, map[string]any, error) {
	if _, configured := e.incrementalEntryPoints[templateName]; !configured {
		return nil, nil, nil, fmt.Errorf("template %q is not an incremental component", templateName)
	}
	template, exists := e.compiledTemplates[templateName]
	if !exists {
		return nil, nil, nil, e.templateNotFoundError(templateName)
	}
	if templateContext == nil {
		templateContext = make(map[string]any)
	} else {
		templateContext = maps.Clone(templateContext)
	}
	shared, ok := templateContext[declShared].(SharedContributionContext)
	if !ok || isNilValue(shared) {
		return nil, nil, nil, fmt.Errorf(
			"incremental component %q requires a shared contribution context",
			templateName,
		)
	}
	immutableInputs, err := incrementalComponentInputs(templateName, templateContext)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, err = withBoundIncrementalImmutableInputs(ctx, templateContext, immutableInputs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("incremental component %q: %w", templateName, err)
	}
	ctx = context.WithValue(ctx, RenderContextContextKey, templateContext)
	return template, ctx, templateContext, nil
}

func incrementalComponentInputs(templateName string, templateContext map[string]any) ([]any, error) {
	if _, supplied := templateContext[declRenderMode]; supplied {
		return nil, fmt.Errorf("incremental component %q cannot supply derived renderMode", templateName)
	}
	source, ok := templateContext[declSource].(string)
	if !ok || source == "" {
		return nil, fmt.Errorf("incremental component %q requires a non-empty source string", templateName)
	}
	item, err := incrementalComponentMapInput(templateName, templateContext, declItem)
	if err != nil {
		return nil, err
	}
	props, err := incrementalComponentMapInput(templateName, templateContext, declProps)
	if err != nil {
		return nil, err
	}
	renderSubject, err := incrementalComponentMapInput(templateName, templateContext, declRenderSubject)
	if err != nil {
		return nil, err
	}
	renderMode, ok := renderSubject["mode"].(string)
	if !ok || (renderMode != renderModeReconcile && renderMode != renderModeAdmission) {
		return nil, fmt.Errorf(
			"incremental component %q requires renderSubject.mode to be reconcile or admission",
			templateName,
		)
	}
	templateContext[declRenderMode] = renderMode
	controller, exists := templateContext[declController]
	if !exists {
		controller = map[string]ResourceStore{}
		templateContext[declController] = controller
	}
	controllerStores, ok := controller.(map[string]ResourceStore)
	if !ok || controllerStores == nil {
		return nil, fmt.Errorf("incremental component %q requires controller to be a resource-store map", templateName)
	}
	return []any{item, props, renderSubject, controllerStores}, nil
}

func incrementalComponentMapInput(
	templateName string,
	templateContext map[string]any,
	name string,
) (map[string]any, error) {
	value, ok := templateContext[name].(map[string]any)
	if !ok || value == nil {
		return nil, fmt.Errorf("incremental component %q requires %s to be an object", templateName, name)
	}
	return value, nil
}

func runScriggoTemplate(
	ctx context.Context,
	templateName string,
	template *scriggo.Template,
	output io.Writer,
	templateContext map[string]any,
	runOpts *scriggo.RunOptions,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if cause := context.Cause(ctx); cause != nil && isScriggoCancellationPanic(recovered, ctx.Err(), cause) {
				err = &RenderTimeoutError{TemplateName: templateName, Cause: cause}
				return
			}
			panic(recovered)
		}
	}()
	if runOpts == nil {
		runOpts = &scriggo.RunOptions{}
	}
	if resources, exists := templateContext[declResources]; exists {
		runOpts.NativeFunctionTrampolines = mergeIncrementalResourceNativeFunctionTrampolines(
			runOpts.NativeFunctionTrampolines,
			resources,
		)
	}

	err = template.Run(output, templateContext, runOpts)
	if cause := context.Cause(ctx); cause != nil {
		return &RenderTimeoutError{TemplateName: templateName, Cause: cause}
	}
	if err != nil {
		return NewRenderError(templateName, err)
	}
	return nil
}

func isScriggoCancellationPanic(recovered any, contextErr, cause error) bool {
	if panicErr, ok := recovered.(error); ok {
		for _, cancellation := range []error{contextErr, cause} {
			if cancellation != nil && errors.Is(panicErr, cancellation) {
				return true
			}
		}
		return false
	}

	message, ok := recovered.(string)
	if !ok {
		return false
	}

	for _, cancellation := range []error{contextErr, cause} {
		if cancellation == nil {
			continue
		}
		fatalMessage := "fatal error: " + cancellation.Error()
		if message == fatalMessage || strings.HasPrefix(message, fatalMessage+"\n") {
			return true
		}
	}
	return false
}

// RenderWithProfiling renders a template and returns profiling statistics.
//
// When profiling is enabled (via Options.Profiling), this method returns
// aggregated include timing statistics. When profiling is disabled, returns nil
// for the stats slice.
func (e *ScriggoEngine) RenderWithProfiling(ctx context.Context, templateName string, templateContext map[string]any) (string, []IncludeStats, error) {
	output, err := e.Render(ctx, templateName, templateContext)
	if err != nil {
		return "", nil, err
	}

	// If profiling is not enabled, return nil stats
	if !e.profilingEnabled {
		return output, nil, nil
	}

	// Get the last profile and convert to IncludeStats
	e.profilingMu.Lock()
	profile := e.lastProfile
	e.profilingMu.Unlock()

	if profile == nil || len(profile.Calls) == 0 {
		return output, nil, nil
	}

	stats := aggregateScriggoProfile(profile)
	return output, stats, nil
}

// templateNotFoundError creates a TemplateNotFoundError with available templates.
func (e *ScriggoEngine) templateNotFoundError(templateName string) error {
	available := make([]string, 0, len(e.compiledTemplates))
	for name := range e.compiledTemplates {
		available = append(available, name)
	}
	slices.Sort(available)
	return NewTemplateNotFoundError(templateName, available)
}

// GlobalUsage reports whether any compiled entry point reads name.
func (e *ScriggoEngine) GlobalUsage(name string) (used, known bool) {
	_, used = e.usedGlobals[name]
	if used {
		return true, true
	}
	return false, !e.globalUsageUnknown
}

func (e *ScriggoEngine) EntryPointUsedGlobals(name string) []string {
	template := e.compiledTemplates[name]
	if template == nil {
		return nil
	}
	return template.UsedVars()
}

func (e *ScriggoEngine) EntryPointUsedNativeValueAccesses(name string) []scriggo.UsedNativeValueAccess {
	template := e.compiledTemplates[name]
	if template == nil {
		return nil
	}
	return template.UsedNativeValueAccesses()
}

// PostProcess applies a template's configured post-processor chain to text
// that did not come from that render pass. Plan assembly uses it so a section
// body spliced into the config is normalised exactly like its surroundings.
func (e *ScriggoEngine) PostProcess(ctx context.Context, templateName, text string) (string, error) {
	return e.applyPostProcessors(ctx, templateName, text)
}

// applyPostProcessors applies the post-processor chain to the output.
func (e *ScriggoEngine) applyPostProcessors(ctx context.Context, templateName, output string) (string, error) {
	identity := e.postProcessCacheIdentities[templateName]
	transaction := e.postProcessTransaction(ctx)
	if identity != nil && transaction != nil {
		return transaction.process(ctx, identity, output, func(ctx context.Context) (string, error) {
			return e.applyPostProcessorsUncached(ctx, templateName, output)
		})
	}
	return e.applyPostProcessorsUncached(ctx, templateName, output)
}

func (e *ScriggoEngine) applyPostProcessorsUncached(
	ctx context.Context,
	templateName,
	output string,
) (string, error) {
	processors, exists := e.postProcessors[templateName]
	if !exists || len(processors) == 0 {
		if cause := context.Cause(ctx); cause != nil {
			return "", &RenderTimeoutError{TemplateName: templateName, Cause: cause}
		}
		return output, nil
	}

	result := output
	for _, processor := range processors {
		if cause := context.Cause(ctx); cause != nil {
			return "", &RenderTimeoutError{TemplateName: templateName, Cause: cause}
		}
		var err error
		if contextProcessor, ok := processor.(contextPostProcessor); ok {
			result, err = contextProcessor.processContext(ctx, templateName, result)
		} else {
			result, err = processor.Process(result)
		}
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return "", &RenderTimeoutError{TemplateName: templateName, Cause: cause}
			}
			return "", NewRenderError(templateName, err)
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return "", &RenderTimeoutError{TemplateName: templateName, Cause: cause}
	}
	return result, nil
}

// TemplateNames returns the names of all available templates, sorted alphabetically.
func (e *ScriggoEngine) TemplateNames() []string {
	names := make([]string, 0, len(e.compiledTemplates))
	for name := range e.compiledTemplates {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// HasTemplate checks if a template with the given name exists.
func (e *ScriggoEngine) HasTemplate(templateName string) bool {
	_, exists := e.compiledTemplates[templateName]
	return exists
}

// GetRawTemplate returns the original template string for the given name.
func (e *ScriggoEngine) GetRawTemplate(templateName string) (string, error) {
	raw, exists := e.rawTemplates[templateName]
	if !exists {
		return "", e.templateNotFoundError(templateName)
	}
	return raw, nil
}

// TemplateCount returns the number of templates in the engine.
func (e *ScriggoEngine) TemplateCount() int {
	return len(e.compiledTemplates)
}

// IsProfilingEnabled returns whether profiling is enabled for this engine.
func (e *ScriggoEngine) IsProfilingEnabled() bool {
	return e.profilingEnabled
}

// ClearVMPool releases pooled Scriggo VMs to allow garbage collection.
// Call after rendering completes to reduce memory from parallel rendering spikes.
//
// This is safe to call at any time - VMs currently in use are not affected
// (they're held by goroutines, not in the pool). Only pooled VMs waiting for
// reuse are released.
func (e *ScriggoEngine) ClearVMPool() {
	scriggo.ClearVMPool()
}

// buildScriggoPostProcessors builds post-processors for the Scriggo engine.
//
// The template post-processor type is handled here (not in NewPostProcessor) because
// it requires access to engine.globals for Scriggo compilation.
func buildScriggoPostProcessors(engine *ScriggoEngine, configs map[string][]PostProcessorConfig) error {
	for templateName, procConfigs := range configs {
		processors := make([]PostProcessor, 0, len(procConfigs))
		for _, cfg := range procConfigs {
			var processor PostProcessor
			var err error
			if cfg.Type == PostProcessorTypeTemplate {
				source := cfg.Params["source"]
				processor, err = NewTemplatePostProcessor(source, engine.globals)
			} else {
				processor, err = NewPostProcessor(cfg)
			}
			if err != nil {
				return err
			}
			processors = append(processors, processor)
		}
		engine.postProcessors[templateName] = processors
		if postProcessorChainCacheable(processors) {
			engine.postProcessCacheIdentities[templateName] = newPostProcessCacheIdentity()
		}
	}
	for templateName := range engine.compiledTemplates {
		processors := engine.postProcessors[templateName]
		if len(processors) == 0 || postProcessorChainCacheable(processors) {
			engine.postProcessReuseProofs[templateName] = newPostProcessReuseProof(
				engine,
				templateName,
				processors,
			)
		}
	}
	return nil
}
