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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/native"
)

// IncrementalBindingPlannerExecutor evaluates a configured dynamic binding planner.
type IncrementalBindingPlannerExecutor interface {
	RenderIncrementalBindings(context.Context, string, map[string]any) ([]byte, error)
}

// IncrementalBindingSnapshotPlanner executes planners against an authenticated input snapshot.
type IncrementalBindingSnapshotPlanner interface {
	SnapshotIncrementalBindingInputs([]string, map[string]any) (*IncrementalBindingInputSnapshot, error)
	MatchIncrementalBindingInputs([]string, map[string]any, *IncrementalBindingInputSnapshot) bool
	RenderIncrementalBindingsSnapshot(context.Context, string, *IncrementalBindingInputSnapshot) ([]byte, error)
}

// IncrementalBindingInputSnapshot is a detached planner-input snapshot owned by one engine.
type IncrementalBindingInputSnapshot struct {
	owner       *ScriggoEngine
	entryPoints []string
	inputs      []string
	context     map[string]any
	// guard covers context, so each planner run attaches it instead of
	// walking the same inputs again.
	guard *IncrementalImmutableCertificate
}

// Equal reports exact planner-visible equality, including Go value types.
func (s *IncrementalBindingInputSnapshot) Equal(other *IncrementalBindingInputSnapshot) bool {
	return s != nil && other != nil && s.owner == other.owner &&
		slices.Equal(s.entryPoints, other.entryPoints) && slices.Equal(s.inputs, other.inputs) &&
		equalIncrementalSerialization(s.context, other.context)
}

var _ IncrementalBindingPlannerExecutor = (*ScriggoEngine)(nil)
var _ IncrementalBindingSnapshotPlanner = (*ScriggoEngine)(nil)

const incrementalExtraContextName = "extraContext"

var incrementalBindingContextNames = [...]string{
	incrementalExtraContextName,
	"capabilities",
	declCurrentConfig,
	declCurrentFiles,
	declCurrentSensitiveFiles,
	declPathResolver,
	"runtimeEnvironment",
	"templateSnippets",
}

func buildScriggoIncrementalBindingGlobals(
	additionalDeclarations map[string]any,
	debugEnabled func() bool,
) native.Declarations {
	decl := buildScriggoIncrementalGlobals(nil, debugEnabled)
	delete(decl, declItem)
	delete(decl, declShared)
	delete(decl, declHTTP)
	delete(decl, declController)
	delete(decl, declResources)
	delete(decl, declSource)
	delete(decl, declProps)
	delete(decl, declRenderSubject)
	delete(decl, declRenderMode)
	delete(decl, FuncFirstSeen)
	delete(decl, FuncDeriveResource)
	delete(decl, FuncRecordEvent)
	standard := buildScriggoGlobals(nil, nil, additionalDeclarations)
	for _, name := range &incrementalBindingContextNames {
		if value, exists := standard[name]; exists {
			decl[name] = value
		}
	}
	return decl
}

func usedIncrementalBindingInputs(usedVariables []string) []string {
	inputs := make([]string, 0, len(incrementalBindingContextNames))
	for _, name := range incrementalBindingContextNames {
		if slices.Contains(usedVariables, name) {
			inputs = append(inputs, name)
		}
	}
	return inputs
}

// RenderIncrementalBindings executes a binding planner and returns canonical JSON.
func (e *ScriggoEngine) RenderIncrementalBindings(
	ctx context.Context,
	templateName string,
	bindingContext map[string]any,
) ([]byte, error) {
	detached, err := detachIncrementalBindingContext(
		bindingContext,
		e.incrementalBindingInputs[templateName],
	)
	if err != nil {
		return nil, fmt.Errorf("detaching incremental binding planner %q context: %w", templateName, err)
	}
	return e.renderIncrementalBindingsDetached(ctx, templateName, detached)
}

// SnapshotIncrementalBindingInputs captures every input used by entryPoints once.
func (e *ScriggoEngine) SnapshotIncrementalBindingInputs(
	entryPoints []string,
	bindingContext map[string]any,
) (*IncrementalBindingInputSnapshot, error) {
	entryPoints = slices.Clone(entryPoints)
	slices.Sort(entryPoints)
	entryPoints = slices.Compact(entryPoints)
	used := make(map[string]struct{}, len(incrementalBindingContextNames))
	for _, entryPoint := range entryPoints {
		if _, configured := e.incrementalBindingEntryPoints[entryPoint]; !configured {
			return nil, fmt.Errorf("template %q is not an incremental binding planner", entryPoint)
		}
		for _, input := range e.incrementalBindingInputs[entryPoint] {
			used[input] = struct{}{}
		}
	}
	inputs := make([]string, 0, len(used))
	for _, name := range incrementalBindingContextNames {
		if _, exists := used[name]; exists {
			inputs = append(inputs, name)
		}
	}
	detached, err := detachIncrementalBindingContext(bindingContext, inputs)
	if err != nil {
		return nil, fmt.Errorf("detaching incremental binding inputs: %w", err)
	}
	return &IncrementalBindingInputSnapshot{
		owner:       e,
		entryPoints: entryPoints,
		inputs:      inputs,
		context:     detached,
		guard:       CertifyIncrementalImmutableInputs(detached),
	}, nil
}

// MatchIncrementalBindingInputs compares live private inputs without cloning them on a cache hit.
func (e *ScriggoEngine) MatchIncrementalBindingInputs(
	entryPoints []string,
	bindingContext map[string]any,
	snapshot *IncrementalBindingInputSnapshot,
) bool {
	if snapshot == nil || snapshot.owner != e {
		return false
	}
	entryPoints = slices.Clone(entryPoints)
	slices.Sort(entryPoints)
	entryPoints = slices.Compact(entryPoints)
	if !slices.Equal(entryPoints, snapshot.entryPoints) {
		return false
	}
	if _, completeContext := bindingContext[incrementalExtraContextName]; bindingContext != nil && !completeContext {
		bindingContext = map[string]any{incrementalExtraContextName: bindingContext}
	}
	for _, name := range snapshot.inputs {
		expected, expectedExists := snapshot.context[name]
		current, exists := bindingContext[name]
		if !exists && name == incrementalExtraContextName {
			current = map[string]any{}
			exists = true
		}
		if exists != expectedExists || exists && !equalIncrementalSerialization(current, expected) {
			return false
		}
	}
	return true
}

// RenderIncrementalBindingsSnapshot executes one planner from a matching snapshot.
func (e *ScriggoEngine) RenderIncrementalBindingsSnapshot(
	ctx context.Context,
	templateName string,
	snapshot *IncrementalBindingInputSnapshot,
) ([]byte, error) {
	if snapshot == nil || snapshot.owner != e || !slices.Contains(snapshot.entryPoints, templateName) {
		return nil, fmt.Errorf("incremental binding planner %q has no matching input snapshot", templateName)
	}
	inputs := e.incrementalBindingInputs[templateName]
	detached := make(map[string]any, len(inputs))
	for _, name := range inputs {
		if value, exists := snapshot.context[name]; exists {
			detached[name] = value
		}
	}
	ctx = WithIncrementalImmutableCertificates(ctx, snapshot.guard)
	return e.renderIncrementalBindingsDetached(ctx, templateName, detached)
}

func (e *ScriggoEngine) renderIncrementalBindingsDetached(
	ctx context.Context,
	templateName string,
	detached map[string]any,
) ([]byte, error) {
	if _, configured := e.incrementalBindingEntryPoints[templateName]; !configured {
		return nil, fmt.Errorf("template %q is not an incremental binding planner", templateName)
	}
	template, exists := e.compiledTemplates[templateName]
	if !exists {
		return nil, e.templateNotFoundError(templateName)
	}
	ctx = WithIncrementalImmutableInputs(ctx, detached)
	ctx = context.WithValue(ctx, RenderContextContextKey, detached)
	var output strings.Builder
	runOptions := &scriggo.RunOptions{
		Context:                  ctx,
		Deterministic:            true,
		ObserveMutationContext:   observeIncrementalMutation,
		ObserveNativeCallContext: observeIncrementalNativeCall,
	}
	if err := runScriggoTemplate(ctx, templateName, template, &output, detached, runOptions); err != nil {
		return nil, err
	}
	canonical, err := canonicalIncrementalBindings(output.String())
	if err != nil {
		return nil, fmt.Errorf("incremental binding planner %q: %w", templateName, err)
	}
	return canonical, nil
}

func detachIncrementalBindingContext(value map[string]any, inputs []string) (map[string]any, error) {
	if _, completeContext := value[incrementalExtraContextName]; value != nil && !completeContext {
		value = map[string]any{incrementalExtraContextName: value}
	}
	detached := make(map[string]any, len(inputs))
	for _, name := range inputs {
		input, exists := value[name]
		if !exists {
			if name == incrementalExtraContextName {
				detached[name] = map[string]any{}
			}
			continue
		}
		cloned, err := cloneIncrementalExportedSerialization(input)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		detached[name] = cloned
	}
	return detached, nil
}

func canonicalIncrementalBindings(output string) ([]byte, error) {
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("output must be a JSON object: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, fmt.Errorf("output must be a JSON object, got %T", value)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("output must contain one JSON object")
		}
		return nil, fmt.Errorf("decoding trailing output: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encoding canonical bindings: %w", err)
	}
	return canonical, nil
}
