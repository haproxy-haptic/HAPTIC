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

package templating

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/haproxy-haptic/scriggo"
)

var exactCycleReplayProtocolGlobals = map[string]struct{}{
	declController:             {},
	declCurrentConfig:          {},
	declCurrentFiles:           {},
	declFileRegistry:           {},
	declHTTP:                   {},
	declPlanRegistry:           {},
	ResourceDeriverContextName: {},
	declResources:              {},
	declShared:                 {},
}

var exactCycleReplayAmbientGlobals = map[string]struct{}{
	"admissionSubject":   {},
	"capabilities":       {},
	"dataplane":          {},
	"extraContext":       {},
	declItem:             {},
	declPathResolver:     {},
	declRenderMode:       {},
	"runtimeEnvironment": {},
	"templateSnippets":   {},
}

var exactCycleReplayPreviousOutputGlobals = map[string]struct{}{
	declCurrentConfig: {},
	declCurrentFiles:  {},
}

var exactCycleReplayPureFunctions = map[string]struct{}{
	builtinAbbreviate: {}, builtinAbs: {}, builtinBase64: {}, "b64decode": {}, "b64encode": {},
	"basename": {}, builtinCapitalize: {}, builtinCapitalizeAll: {}, "ceil": {}, "cidr_partition": {}, "coalesce": {},
	"condition": {}, "dig": {}, "dig_string": {},
	"fail": {}, "fallback": {}, "filter": {}, "flat_map": {},
	builtinFormatFloat: {}, builtinFormatInt: {}, "glob_match": {}, "group_by": {}, builtinHasPrefix: {},
	builtinHasSuffix: {}, builtinHex: {}, builtinHmacSHA1: {}, builtinHmacSHA256: {}, builtinHTMLEscape: {},
	builtinIndex: {}, builtinIndexAny: {}, "indent": {}, "isdigit": {}, "isNil": {},
	"join": {}, "join_key": {}, "jsonpathGet": {}, "keys": {}, builtinLastIndex: {},
	"make_guid": {}, "map": {}, builtinMapExtract: {}, builtinMarshalJSON: {}, builtinMarshalJSONIndent: {},
	builtinMarshalYAML: {}, builtinMax: {}, builtinMD5: {}, "merge": {}, builtinMin: {},
	"namespace": {}, funcOnOwnLinesJoin: {}, funcOnOwnLinesStart: {}, builtinParseDuration: {}, builtinParseFloat: {}, builtinParseInt: {}, builtinParseTime: {},
	builtinPow: {}, builtinPublicKeyInfo: {}, builtinParseYAML: {}, builtinQueryEscape: {}, declRegexp: {}, "regex_search": {}, "reject": {},
	"replace": {}, builtinReplaceAll: {}, builtinRuneCount: {}, "sanitize_regex": {}, "selectattr": {},
	"semver_gte": {}, "seq": {}, builtinSHA1: {}, builtinSHA256: {}, "shard_slice": {},
	"sort_by": {}, "sort_ints": {}, "sort_strings": {}, builtinSplit: {}, builtinSplitAfter: {},
	builtinSplitAfterN: {}, builtinSplitN: {}, "strings_contains": {},
	"strings_lower": {}, "strings_split": {}, "strings_splitn": {},
	"strings_trim": {}, "strip": {}, "title": {}, "toJSON": {}, builtinToKebab: {},
	builtinToLower: {}, "toSlice": {}, "toStringSlice": {}, builtinToUpper: {}, "to_str_map": {},
	"tofloat": {}, "toint": {}, "tostring": {}, builtinTrim: {}, builtinTrimLeft: {},
	builtinTrimPrefix: {}, builtinTrimRight: {}, builtinTrimSpace: {}, builtinTrimSuffix: {}, "unmarshalJSON": {},
	"unmarshalYAML": {}, "unique": {}, "unique_by": {}, "untar_gz": {},
}

var exactCycleReplayObservedFunctions = map[string]struct{}{
	FuncCycleRandomBytes:                  {},
	FuncCycleTimeBucket:                   {},
	FuncDeriveResource:                    {},
	FuncIncrementalRankedFragmentBytes:    {},
	FuncIncrementalRankedFragments:        {},
	FuncIncrementalRankedFragmentsJoin:    {},
	FuncIncrementalRankedTextFragment:     {},
	FuncIncrementalRankedTextFragmentJoin: {},
	FuncIncrementalRender:                 {},
	FuncIncrementalValueCount:             {},
	FuncIncrementalValues:                 {},
	FuncRecordEvent:                       {},
	FuncResource:                          {},
	FuncStatusPatch:                       {},
}

var exactCycleReplayRejectedFunctions = map[string]struct{}{
	FilterDebug:        {},
	FuncJSONPathSet:    {},
	FuncTransitionTime: {},
	builtinCountBy:     {},
	builtinDate:        {},
	builtinIndexBy:     {},
	"now":              {},
	"randBytes":        {},
	"reverse":          {},
	"sprint":           {},
	"sprintf":          {},
	builtinUnixTime:    {},
}

type exactCycleReplayProgramAuthentication struct {
	entryPoints       []string
	rootEntryPoints   []string
	templates         []*scriggo.Template
	ambientNames      []string
	protocolNames     []string
	postProcessProofs []*PostProcessReuseProof
	outputOnlyRoots   []bool
	recordsEffects    bool
	requiresAllRoots  bool
	usesCurrentConfig bool
	usesCurrentFiles  bool
}

// ExactCycleReplayProgram certifies the statically used root-template surface.
type ExactCycleReplayProgram struct {
	owner           *ScriggoEngine
	entryPoints     []string
	rootEntryPoints []string
	templates       []*scriggo.Template
	ambientNames    []string
	protocolNames   []string
	// outputOnlyRoots marks, per entry point, a root whose only product is
	// its text: no protocol global, no effect, no post-processing. Such a
	// root's output is a function of its ambient inputs and what it observed.
	outputOnlyRoots   []bool
	postProcessProofs []*PostProcessReuseProof
	recordsEffects    bool
	requiresAllRoots  bool
	usesCurrentConfig bool
	usesCurrentFiles  bool
	auth              exactCycleReplayProgramAuthentication
	seal              *ExactCycleReplayProgram
}

type exactCycleReplayAmbientValue struct {
	name  string
	found bool
	value any
}

// ExactCycleProtocolState is an authenticated immutable root for hidden
// attempt-local state consumed through an engine protocol global.
type ExactCycleProtocolState interface {
	ValidateExactCycleProtocolState() error
	SameExactCycleProtocolState(ExactCycleProtocolState) (bool, error)
}

// ExactCycleProtocolStateProvider exposes the initial semantic root of one
// attempt-local protocol object.
type ExactCycleProtocolStateProvider interface {
	ExactCycleProtocolState() (ExactCycleProtocolState, error)
}

type exactCycleReplayProtocolValue struct {
	name  string
	state ExactCycleProtocolState
}

type exactCycleReplayExecutionContextKey struct{}

type exactCycleRootInvocationContextKey struct{}

// ExactCycleRootInvocation identifies one rendered artifact occurrence.
type ExactCycleRootInvocation struct {
	Kind string
	Name string
}

// WithExactCycleRootInvocation binds the artifact occurrence rendered by one root call.
func WithExactCycleRootInvocation(
	ctx context.Context,
	invocation ExactCycleRootInvocation,
) context.Context {
	return context.WithValue(ctx, exactCycleRootInvocationContextKey{}, invocation)
}

type exactCycleReplayExecution struct {
	program *ExactCycleReplayProgram
	attempt *exactCycleEffectAttempt
	seal    *exactCycleReplayExecution
}

// ExactCycleReplayExecutionActive reports whether root scheduling must be deterministic.
func ExactCycleReplayExecutionActive(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	execution, ok := ctx.Value(exactCycleReplayExecutionContextKey{}).(*exactCycleReplayExecution)
	return ok && execution != nil && execution.seal == execution && execution.program != nil
}

// ExactCycleReplayInputs is an isolated snapshot of only compiled-used ambient globals.
type ExactCycleReplayInputs struct {
	program      *ExactCycleReplayProgram
	generation   uint64
	values       []exactCycleReplayAmbientValue
	auth         []exactCycleReplayAmbientValue
	protocols    []exactCycleReplayProtocolValue
	protocolAuth []exactCycleReplayProtocolValue
	effects      []exactCycleEffectObservation
	effectAuth   []exactCycleEffectObservation
	roots        []ExactCycleRootInvocation
	rootAuth     []ExactCycleRootInvocation
	attempt      *exactCycleEffectAttempt
	finalized    bool
	seal         *ExactCycleReplayInputs
}

// ExecutionContext binds the program's deterministic and immutable root semantics.
func (p *ExactCycleReplayProgram) ExecutionContext(ctx context.Context) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("exact cycle replay context is nil")
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	execution := &exactCycleReplayExecution{program: p}
	execution.seal = execution
	return context.WithValue(ctx, exactCycleReplayExecutionContextKey{}, execution), nil
}

func exactCycleReplayExecutionFor(
	ctx context.Context,
	engine *ScriggoEngine,
	templateName string,
) (*exactCycleReplayExecution, bool, error) {
	if ctx == nil {
		return nil, false, nil
	}
	execution, _ := ctx.Value(exactCycleReplayExecutionContextKey{}).(*exactCycleReplayExecution)
	if execution == nil {
		return nil, false, nil
	}
	if execution.seal != execution || execution.program == nil {
		return nil, false, errors.New("exact cycle replay execution has invalid provenance")
	}
	program := execution.program
	if err := program.validate(); err != nil {
		return nil, false, err
	}
	if program.owner != engine {
		return nil, false, errors.New("exact cycle replay execution belongs to another engine")
	}
	_, found := slices.BinarySearch(program.entryPoints, templateName)
	if !found {
		return nil, false, fmt.Errorf("template %q is outside the exact cycle replay program", templateName)
	}
	return execution, true, nil
}

func (e *exactCycleReplayExecution) beginRoot(
	ctx context.Context,
	templateName string,
) (func(bool), error) {
	if e == nil || e.seal != e || e.program == nil {
		return nil, errors.New("exact cycle replay execution has invalid provenance")
	}
	if e.attempt == nil {
		return func(bool) {}, nil
	}
	attempt := e.attempt
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.finalized || attempt.failed || attempt.program != e.program || attempt.owner == nil {
		return nil, errors.New("exact cycle replay attempt is no longer active")
	}
	if attempt.rootActive || attempt.nextRoot >= len(attempt.invocations) {
		attempt.failed = true
		return nil, fmt.Errorf("exact cycle replay entry point %q executed out of order", templateName)
	}
	expected := attempt.invocations[attempt.nextRoot]
	invocation, provided := ctx.Value(exactCycleRootInvocationContextKey{}).(ExactCycleRootInvocation)
	if !provided && expected.Kind == "template" {
		invocation = expected
		provided = true
	}
	if !provided || invocation != expected || invocation.Name != templateName {
		attempt.failed = true
		return nil, fmt.Errorf(
			"exact cycle replay root %q executed at occurrence %+v, want %+v",
			templateName, invocation, expected,
		)
	}
	rootIndex := attempt.nextRoot
	attempt.rootActive = true
	return func(success bool) {
		attempt.mu.Lock()
		defer attempt.mu.Unlock()
		if attempt.finalized {
			return
		}
		if !attempt.rootActive || attempt.nextRoot != rootIndex || !success {
			attempt.failed = true
			attempt.rootActive = false
			return
		}
		attempt.rootActive = false
		attempt.nextRoot++
	}, nil
}

func (p *ExactCycleReplayProgram) immutableRootInputs(templateContext map[string]any) []any {
	values := make([]any, 0, len(p.ambientNames)+2)
	for _, name := range p.ambientNames {
		if value, found := templateContext[name]; found {
			values = append(values, value)
		}
	}
	if p.usesCurrentConfig {
		if value, found := templateContext[declCurrentConfig]; found {
			values = append(values, value)
		}
	}
	if p.usesCurrentFiles {
		if value, found := templateContext[declCurrentFiles]; found {
			values = append(values, value)
		}
	}
	return values
}

// PrepareExactCycleReplay rejects root programs with an unproved native surface.
func (e *ScriggoEngine) PrepareExactCycleReplay(
	entryPoints []string,
) (*ExactCycleReplayProgram, error) {
	if e == nil {
		return nil, errors.New("exact cycle replay engine is nil")
	}
	if err := e.validateExactCycleReplayDeclarations(); err != nil {
		return nil, err
	}
	rootEntryPoints := slices.Clone(entryPoints)
	ordered := slices.Clone(rootEntryPoints)
	slices.Sort(ordered)
	ordered = slices.Compact(ordered)
	expected := e.exactCycleRootEntryPoints()
	if !slices.Equal(ordered, expected) {
		return nil, fmt.Errorf("exact cycle replay entry points are incomplete: got %v, want %v", ordered, expected)
	}
	scan, err := e.scanExactCycleReplayEntryPoints(ordered)
	if err != nil {
		return nil, err
	}
	requiresAllRoots := false
	ambientNames := sortedExactCycleSetNames(scan.ambient)
	protocolNames := sortedExactCycleSetNames(scan.protocols)
	program := &ExactCycleReplayProgram{
		owner:             e,
		entryPoints:       ordered,
		rootEntryPoints:   rootEntryPoints,
		templates:         scan.templates,
		ambientNames:      ambientNames,
		protocolNames:     protocolNames,
		outputOnlyRoots:   scan.outputOnly,
		postProcessProofs: scan.proofs,
		recordsEffects:    scan.recordsEffects,
		requiresAllRoots:  requiresAllRoots,
		usesCurrentConfig: scan.usesCurrentConfig,
		usesCurrentFiles:  scan.usesCurrentFiles,
	}
	program.auth = exactCycleReplayProgramAuthentication{
		entryPoints:       slices.Clone(ordered),
		rootEntryPoints:   slices.Clone(rootEntryPoints),
		templates:         slices.Clone(scan.templates),
		ambientNames:      slices.Clone(ambientNames),
		protocolNames:     slices.Clone(protocolNames),
		postProcessProofs: slices.Clone(scan.proofs),
		outputOnlyRoots:   slices.Clone(scan.outputOnly),
		recordsEffects:    scan.recordsEffects,
		requiresAllRoots:  requiresAllRoots,
		usesCurrentConfig: scan.usesCurrentConfig,
		usesCurrentFiles:  scan.usesCurrentFiles,
	}
	program.seal = program
	return program, nil
}

type exactCycleReplayEntryPointScan struct {
	templates         []*scriggo.Template
	proofs            []*PostProcessReuseProof
	outputOnly        []bool
	ambient           map[string]struct{}
	protocols         map[string]struct{}
	recordsEffects    bool
	usesCurrentConfig bool
	usesCurrentFiles  bool
}

func (e *ScriggoEngine) scanExactCycleReplayEntryPoints(
	ordered []string,
) (*exactCycleReplayEntryPointScan, error) {
	scan := &exactCycleReplayEntryPointScan{
		templates:  make([]*scriggo.Template, len(ordered)),
		proofs:     make([]*PostProcessReuseProof, len(ordered)),
		outputOnly: make([]bool, len(ordered)),
		ambient:    map[string]struct{}{},
		protocols:  map[string]struct{}{},
	}
	for index, name := range ordered {
		template := e.compiledTemplates[name]
		if template == nil {
			return nil, fmt.Errorf("exact cycle replay entry point %q is unavailable", name)
		}
		if err := template.DeterministicSafe(); err != nil {
			return nil, fmt.Errorf("exact cycle replay entry point %q is not deterministic: %w", name, err)
		}
		usesEffects, usedProtocols, err := e.validateExactCycleReplayTemplate(name, template)
		if err != nil {
			return nil, err
		}
		scan.recordsEffects = scan.recordsEffects || usesEffects
		for _, protocol := range usedProtocols {
			scan.protocols[protocol] = struct{}{}
		}
		proof, err := e.PostProcessReuseProof(name)
		if err != nil {
			return nil, err
		}
		if proof == nil {
			return nil, fmt.Errorf("exact cycle replay entry point %q has no deterministic post-process proof", name)
		}
		identity, err := proof.CertifiesIdentity(e, name)
		if err != nil {
			return nil, err
		}
		scan.templates[index] = template
		scan.proofs[index] = proof
		scan.outputOnly[index] = identity && !usesEffects && len(usedProtocols) == 0
		templateUsesConfig, templateUsesFiles := collectExactCycleAmbientGlobals(template, scan.ambient)
		scan.usesCurrentConfig = scan.usesCurrentConfig || templateUsesConfig
		scan.usesCurrentFiles = scan.usesCurrentFiles || templateUsesFiles
	}
	return scan, nil
}

func sortedExactCycleSetNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func collectExactCycleAmbientGlobals(
	template *scriggo.Template,
	ambient map[string]struct{},
) (usesCurrentConfig, usesCurrentFiles bool) {
	nativeVariables := make(map[string]struct{})
	for _, declaration := range template.UsedNativeDeclarations() {
		if declaration.Kind == scriggo.NativeDeclarationVariable {
			nativeVariables[declaration.Name] = struct{}{}
		}
	}
	for _, global := range template.UsedVars() {
		if _, nativeVariable := nativeVariables[global]; !nativeVariable {
			continue
		}
		if _, protocol := exactCycleReplayProtocolGlobals[global]; protocol {
			usesCurrentConfig = usesCurrentConfig || global == declCurrentConfig
			usesCurrentFiles = usesCurrentFiles || global == declCurrentFiles
			continue
		}
		ambient[global] = struct{}{}
	}
	return usesCurrentConfig, usesCurrentFiles
}

func (e *ScriggoEngine) validateExactCycleReplayDeclarations() error {
	standard := buildScriggoGlobals(nil, nil, nil)
	for name := range e.additionalDeclarationNames {
		if _, reserved := standard[name]; reserved {
			return fmt.Errorf("exact cycle replay additional declaration %q shadows an engine declaration", name)
		}
	}
	for name := range e.customDeclarationNames {
		if _, reserved := standard[name]; reserved {
			return fmt.Errorf("exact cycle replay custom declaration %q shadows an engine declaration", name)
		}
	}
	if declaration, found := e.globals[declCurrentFiles]; found &&
		reflect.TypeOf(declaration) != reflect.TypeFor[*map[string]string]() {
		return errors.New("exact cycle replay currentFiles declaration has an unsupported type")
	}
	if declaration, found := e.globals[declCurrentConfig]; found && !exactCycleCurrentConfigDeclaration(declaration) {
		return errors.New("exact cycle replay currentConfig declaration has an unsupported type")
	}
	if declaration, found := e.globals[declResources]; found &&
		!registeredIncrementalResourceDeclarationType(reflect.TypeOf(declaration)) {
		return errors.New("exact cycle replay resources declaration has an unsupported type")
	}
	return nil
}

func exactCycleCurrentConfigDeclaration(declaration any) bool {
	return registeredExactCyclePreviousOutputType(reflect.TypeOf(declaration))
}

// exactCycleRootEntryPoints lists the non-private templates once: the set is
// fixed after construction, and the program authenticates against it on
// every root and auxiliary render.
func (e *ScriggoEngine) exactCycleRootEntryPoints() []string {
	e.exactCycleRootEntryPointsOnce.Do(func() {
		entryPoints := make([]string, 0, len(e.compiledTemplates))
		for name := range e.compiledTemplates {
			if _, private := e.incrementalEntryPoints[name]; private {
				continue
			}
			if _, private := e.incrementalBindingEntryPoints[name]; private {
				continue
			}
			entryPoints = append(entryPoints, name)
		}
		slices.Sort(entryPoints)
		e.exactCycleRootEntryPointsMemo = entryPoints
	})
	return e.exactCycleRootEntryPointsMemo
}

func (e *ScriggoEngine) validateExactCycleReplayTemplate(
	name string,
	template *scriggo.Template,
) (recordsEffects bool, protocolNames []string, err error) {
	protocols := map[string]struct{}{}
	declarations := template.UsedNativeDeclarations()
	for index := range declarations {
		usesEffects, err := e.validateExactCycleReplayDeclaration(name, &declarations[index], protocols)
		if err != nil {
			return false, nil, err
		}
		recordsEffects = recordsEffects || usesEffects
	}
	callables := template.UsedNativeCallables()
	for index := range callables {
		callable := &callables[index]
		if !validExactCycleCallable(callable) {
			return false, nil, fmt.Errorf(
				"exact cycle replay entry point %q calls unproved native %q member %q",
				name,
				callable.DeclarationName,
				callable.MemberPath,
			)
		}
		switch callable.DeclarationName {
		case declShared, declFileRegistry, declPlanRegistry:
			protocols[callable.DeclarationName] = struct{}{}
		}
	}
	for _, access := range template.UsedNativeValueAccesses() {
		if _, protocol := exactCycleReplayProtocolGlobals[access.DeclarationName]; !protocol {
			continue
		}
		if _, previousOutput := exactCycleReplayPreviousOutputGlobals[access.DeclarationName]; previousOutput {
			continue
		}
		return false, nil, fmt.Errorf(
			"exact cycle replay entry point %q consumes protocol native %q outside an observed call",
			name,
			access.DeclarationName,
		)
	}
	return recordsEffects, sortedExactCycleSetNames(protocols), nil
}

func (e *ScriggoEngine) validateExactCycleReplayDeclaration(
	name string,
	declaration *scriggo.UsedNativeDeclaration,
	protocols map[string]struct{},
) (bool, error) {
	if _, custom := e.customDeclarationNames[declaration.Name]; custom {
		return false, fmt.Errorf("exact cycle replay entry point %q uses custom native %q", name, declaration.Name)
	}
	if _, rejected := exactCycleReplayRejectedFunctions[declaration.Name]; rejected {
		return false, fmt.Errorf("exact cycle replay entry point %q uses unproved native %q", name, declaration.Name)
	}
	_, additional := e.additionalDeclarationNames[declaration.Name]
	switch declaration.Kind {
	case scriggo.NativeDeclarationVariable:
		if !additional {
			if _, ambient := exactCycleReplayAmbientGlobals[declaration.Name]; !ambient {
				if _, protocol := exactCycleReplayProtocolGlobals[declaration.Name]; !protocol {
					return false, fmt.Errorf("exact cycle replay entry point %q uses unclassified global %q", name, declaration.Name)
				}
			}
		}
	case scriggo.NativeDeclarationFunction:
		return e.validateExactCycleReplayFunction(name, declaration, additional, protocols)
	case scriggo.NativeDeclarationType, scriggo.NativeDeclarationConstant:
		if !additional {
			return false, fmt.Errorf("exact cycle replay entry point %q uses unclassified native %q", name, declaration.Name)
		}
	default:
		return false, fmt.Errorf("exact cycle replay entry point %q uses native %q with unknown kind", name, declaration.Name)
	}
	if declaration.Name == declResources && !registeredIncrementalResourceDeclarationType(
		reflect.TypeOf(declaration.Declaration),
	) {
		return false, fmt.Errorf("exact cycle replay entry point %q has an unregistered resources declaration", name)
	}
	return false, nil
}

func (e *ScriggoEngine) validateExactCycleReplayFunction(
	name string,
	declaration *scriggo.UsedNativeDeclaration,
	additional bool,
	protocols map[string]struct{},
) (bool, error) {
	if additional {
		return false, fmt.Errorf("exact cycle replay entry point %q uses additional native function %q", name, declaration.Name)
	}
	if declaration.Name == FuncFirstSeen {
		protocols[declShared] = struct{}{}
		return false, nil
	}
	if _, pure := exactCycleReplayPureFunctions[declaration.Name]; pure {
		return false, nil
	}
	if _, observed := exactCycleReplayObservedFunctions[declaration.Name]; observed {
		switch declaration.Name {
		case FuncDeriveResource:
			protocols[ResourceDeriverContextName] = struct{}{}
		case FuncRecordEvent:
			protocols["recordEventCollector"] = struct{}{}
		case FuncStatusPatch:
			protocols["statusPatchCollector"] = struct{}{}
		}
		return declaration.Name == FuncCycleRandomBytes || declaration.Name == FuncCycleTimeBucket, nil
	}
	return false, fmt.Errorf("exact cycle replay entry point %q uses unclassified native function %q", name, declaration.Name)
}

// RequiresUnchangedInputRoots reports whether legacy shared state hides dependencies
// behind root execution order and therefore requires every input root to stay fixed.
func (p *ExactCycleReplayProgram) RequiresUnchangedInputRoots() (bool, error) {
	if err := p.validate(); err != nil {
		return false, err
	}
	return p.requiresAllRoots, nil
}

// UsesPreviousOutput reports whether a compiled root reads the named prior output.
func (p *ExactCycleReplayProgram) UsesPreviousOutput(name string) (bool, error) {
	if err := p.validate(); err != nil {
		return false, err
	}
	switch name {
	case declCurrentConfig:
		return p.usesCurrentConfig, nil
	case declCurrentFiles:
		return p.usesCurrentFiles, nil
	default:
		return false, fmt.Errorf("exact cycle replay previous output %q is unknown", name)
	}
}

func validExactCycleCallable(callable *scriggo.UsedNativeCallable) bool {
	if callable.Constructed {
		return false
	}
	switch callable.DeclarationName {
	case declResources:
		return validExactCycleResourceCallable(callable)
	case declController:
		return callable.Kind == scriggo.NativeCallableMethod &&
			(callable.Name == memberList || callable.Name == memberFetch || callable.Name == memberGetSingle)
	case declFileRegistry:
		return callable.MemberPath == "Register" || callable.MemberPath == "Register[1].Error"
	case declHTTP:
		return slices.Contains([]string{memberFetch, memberPending}, callable.MemberPath)
	case declPathResolver:
		return slices.Contains([]string{"GetBaseDir", "GetPath", "GetPath[1].Error"}, callable.MemberPath)
	case declPlanRegistry:
		return validExactCyclePlanCallable(callable.MemberPath)
	case declShared:
		return callable.MemberPath == "ComputeIfAbsent" || callable.MemberPath == "Get"
	case declRegexp:
		return slices.Contains([]string{"FindSubmatch", "Match", memberReplaceAll}, callable.MemberPath)
	case "unmarshalJSON", "unmarshalYAML":
		return callable.MemberPath == "Error"
	case builtinParseYAML:
		return callable.MemberPath == "[1].Error"
	case builtinParseDuration:
		return slices.Contains([]string{
			"[0].Hours", "[0].Milliseconds", "[0].Minutes", "[0].Nanoseconds", "[0].Seconds", "[0].String",
		}, callable.MemberPath)
	case builtinDate, builtinParseTime, builtinUnixTime:
		return validExactCycleTimeCallable(callable.MemberPath)
	default:
		return false
	}
}

func validExactCycleResourceCallable(callable *scriggo.UsedNativeCallable) bool {
	if callable.DeclarationName != declResources || callable.Kind != scriggo.NativeCallableFunctionField ||
		!registeredIncrementalResourceDeclarationType(reflect.TypeOf(callable.Declaration)) {
		return false
	}
	switch callable.Name {
	case memberList, memberFetch, memberGetSingle:
		return true
	default:
		return strings.HasSuffix(callable.MemberPath, ".APIVersion") && callable.Name == memberAPIVersion
	}
}

func validExactCyclePlanCallable(path string) bool {
	for _, method := range []string{
		memberBackend, "BackendWhenAny", "Fragment", "MapMeta", memberProfile, "ProfileGroup", "Section",
	} {
		if path == method || path == method+"[1].Error" {
			return true
		}
	}
	return false
}

func validExactCycleTimeCallable(path string) bool {
	for _, method := range []string{
		"Add", "AddDate", "After", "Before", "Day", "Equal", "Format", "Hour", "IsZero",
		"Minute", "Month", "Nanosecond", "Round", "Second", "String", "Sub", "Truncate",
		"UTC", "Unix", "UnixMicro", "UnixMilli", "UnixNano", "Weekday", "Year", "YearDay",
	} {
		if path == method || strings.HasSuffix(path, "."+method) {
			return true
		}
	}
	return false
}

// Capture is unavailable because publication requires an owned execution attempt.
func (p *ExactCycleReplayProgram) Capture(
	templateContext map[string]any,
) (*ExactCycleReplayInputs, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	return nil, errors.New("exact cycle replay requires an owned execution attempt")
}

// Matches reports whether every compiled-used ambient input and temporal lease is unchanged.
func (p *ExactCycleReplayProgram) Matches(
	previous *ExactCycleReplayInputs,
	templateContext map[string]any,
) (bool, error) {
	return p.matchesAt(previous, templateContext, time.Now())
}

func (p *ExactCycleReplayProgram) matchesAt(
	previous *ExactCycleReplayInputs,
	templateContext map[string]any,
	now time.Time,
) (bool, error) {
	if err := p.validate(); err != nil {
		return false, err
	}
	if err := previous.validate(p); err != nil {
		return false, err
	}
	if p.owner.profilingEnabled || p.owner.IsTracingEnabled() || p.owner.IsFilterDebugEnabled() {
		return false, nil
	}
	if !exactCycleAmbientValuesMatch(previous.values, templateContext) {
		return false, nil
	}
	for index := range previous.protocols {
		entry := &previous.protocols[index]
		current, captured := exactCycleProtocolStateForMatch(entry.name, templateContext)
		if !captured {
			return false, nil
		}
		matched, err := entry.state.SameExactCycleProtocolState(current)
		if err != nil || !matched {
			return matched, err
		}
	}
	if !exactCycleEffectsMatch(previous.effects, now) {
		return false, nil
	}
	return true, nil
}

func exactCycleAmbientValuesMatch(
	values []exactCycleReplayAmbientValue,
	templateContext map[string]any,
) bool {
	for index := range values {
		entry := &values[index]
		current, found := templateContext[entry.name]
		if found != entry.found {
			return false
		}
		if err := validateIncrementalSerialization(current); err != nil {
			return false
		}
		if !equalIncrementalSerialization(entry.value, current) {
			return false
		}
	}
	return true
}

func exactCycleProtocolStateForMatch(
	name string,
	templateContext map[string]any,
) (ExactCycleProtocolState, bool) {
	state, err := captureExactCycleProtocolState(name, templateContext)
	if err != nil {
		return nil, false
	}
	return state, true
}

func exactCycleEffectsMatch(effects []exactCycleEffectObservation, now time.Time) bool {
	for index := range effects {
		effect := &effects[index]
		if effect.kind != exactCycleEffectTimeBucket {
			continue
		}
		if !now.Before(effect.expiresAt) || exactCycleTimeBucketResult(
			now, effect.integerArg, effect.stringArg,
		) != effect.result {
			return false
		}
	}
	return true
}

func captureExactCycleProtocolState(
	name string,
	templateContext map[string]any,
) (ExactCycleProtocolState, error) {
	value, found := templateContext[name]
	if !found || isNilValue(value) {
		return nil, fmt.Errorf("exact cycle replay protocol %q is unavailable", name)
	}
	provider, ok := value.(ExactCycleProtocolStateProvider)
	if !ok || isNilValue(provider) {
		return nil, fmt.Errorf("exact cycle replay protocol %q has no authenticated initial-state provider", name)
	}
	state, err := provider.ExactCycleProtocolState()
	if err != nil {
		return nil, fmt.Errorf("exact cycle replay protocol %q: %w", name, err)
	}
	if state == nil || isNilValue(state) {
		return nil, fmt.Errorf("exact cycle replay protocol %q returned no initial state", name)
	}
	if err := state.ValidateExactCycleProtocolState(); err != nil {
		return nil, fmt.Errorf("exact cycle replay protocol %q: %w", name, err)
	}
	return state, nil
}

func sameExactCycleProtocolStateIdentity(left, right ExactCycleProtocolState) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	return leftValue.IsValid() && rightValue.IsValid() &&
		leftValue.Type() == rightValue.Type() && leftValue.Kind() == reflect.Pointer &&
		leftValue.Pointer() == rightValue.Pointer()
}

func (p *ExactCycleReplayProgram) validate() error {
	if p == nil || p.seal != p || p.owner == nil || len(p.entryPoints) == 0 || !p.authenticated() {
		return errors.New("exact cycle replay program has invalid provenance")
	}
	for _, name := range p.rootEntryPoints {
		if _, found := slices.BinarySearch(p.entryPoints, name); !found {
			return errors.New("exact cycle replay program has an unknown root occurrence")
		}
	}
	for index, name := range p.entryPoints {
		if p.owner.compiledTemplates[name] != p.templates[index] {
			return errors.New("exact cycle replay program has a stale entry point")
		}
		if err := p.postProcessProofs[index].ValidateAuthentication(); err != nil {
			return fmt.Errorf("exact cycle replay entry point %q: %w", name, err)
		}
	}
	return nil
}

// OutputOnlyRoot reports whether the named root produces nothing but its
// text: it uses no protocol global, records no effect and is not
// post-processed, so its previous output can stand in for a render whose
// ambient inputs and observed incremental values are unchanged.
func (p *ExactCycleReplayProgram) OutputOnlyRoot(name string) (bool, error) {
	if err := p.validate(); err != nil {
		return false, err
	}
	index, found := slices.BinarySearch(p.entryPoints, name)
	if !found {
		return false, nil
	}
	return p.outputOnlyRoots[index], nil
}

// ReuseExactCycleRoot accounts for a root whose previous output the caller
// reuses, so the cycle's root invocations stay complete and in order.
func (e *ScriggoEngine) ReuseExactCycleRoot(ctx context.Context, templateName string) error {
	execution, exact, err := exactCycleReplayExecutionFor(ctx, e, templateName)
	if err != nil {
		return err
	}
	if !exact {
		return errors.New("exact cycle root reuse outside an exact cycle execution")
	}
	complete, err := execution.beginRoot(ctx, templateName)
	if err != nil {
		return err
	}
	complete(true)
	return nil
}

func (p *ExactCycleReplayProgram) authenticated() bool {
	return slices.Equal(p.outputOnlyRoots, p.auth.outputOnlyRoots) &&
		len(p.outputOnlyRoots) == len(p.entryPoints) &&
		slices.Equal(p.entryPoints, p.auth.entryPoints) &&
		slices.Equal(p.rootEntryPoints, p.auth.rootEntryPoints) &&
		slices.Equal(p.templates, p.auth.templates) &&
		slices.Equal(p.ambientNames, p.auth.ambientNames) &&
		slices.Equal(p.protocolNames, p.auth.protocolNames) &&
		slices.Equal(p.postProcessProofs, p.auth.postProcessProofs) &&
		p.recordsEffects == p.auth.recordsEffects &&
		p.requiresAllRoots == p.auth.requiresAllRoots &&
		p.usesCurrentConfig == p.auth.usesCurrentConfig &&
		p.usesCurrentFiles == p.auth.usesCurrentFiles &&
		len(p.entryPoints) == len(p.templates) && len(p.entryPoints) == len(p.postProcessProofs) &&
		slices.Equal(p.entryPoints, p.owner.exactCycleRootEntryPoints())
}

func (s *ExactCycleReplayInputs) validate(program *ExactCycleReplayProgram) error {
	if s == nil || s.seal != s || !s.finalized || s.program != program || len(s.values) != len(s.auth) ||
		len(s.values) != len(program.ambientNames) || len(s.effects) != len(s.effectAuth) ||
		len(s.protocols) != len(s.protocolAuth) || len(s.protocols) != len(program.protocolNames) ||
		!slices.Equal(s.roots, s.rootAuth) || len(s.roots) != len(program.rootEntryPoints) {
		return errors.New("exact cycle replay inputs have invalid provenance")
	}
	for index := range s.roots {
		if s.roots[index].Kind == "" || s.roots[index].Name != program.rootEntryPoints[index] {
			return errors.New("exact cycle replay inputs have invalid root occurrences")
		}
	}
	if err := s.validateValues(program); err != nil {
		return err
	}
	if err := s.validateProtocols(program); err != nil {
		return err
	}
	return s.validateEffects()
}

func (s *ExactCycleReplayInputs) validateValues(program *ExactCycleReplayProgram) error {
	for index := range s.values {
		value := &s.values[index]
		auth := &s.auth[index]
		if value.name != program.ambientNames[index] || value.name != auth.name ||
			value.found != auth.found || !equalIncrementalSerialization(value.value, auth.value) {
			return errors.New("exact cycle replay inputs failed authentication")
		}
	}
	return nil
}

func (s *ExactCycleReplayInputs) validateProtocols(program *ExactCycleReplayProgram) error {
	for index := range s.protocols {
		value := &s.protocols[index]
		auth := &s.protocolAuth[index]
		if value.name != program.protocolNames[index] || value.name != auth.name ||
			!sameExactCycleProtocolStateIdentity(value.state, auth.state) {
			return errors.New("exact cycle replay protocol state failed authentication")
		}
		if err := value.state.ValidateExactCycleProtocolState(); err != nil {
			return err
		}
	}
	return nil
}

func (s *ExactCycleReplayInputs) validateEffects() error {
	for index := range s.effects {
		if !equalExactCycleEffect(&s.effects[index], &s.effectAuth[index]) {
			return errors.New("exact cycle replay effects failed authentication")
		}
		if err := validateExactCycleEffect(&s.effects[index], s.generation); err != nil {
			return err
		}
		if index > 0 && compareExactCycleEffects(&s.effects[index-1], &s.effects[index]) >= 0 {
			return errors.New("exact cycle replay effects have invalid call order")
		}
	}
	return nil
}
