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
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/ast"
	"github.com/haproxy-haptic/scriggo/ast/astutil"
	"github.com/haproxy-haptic/scriggo/native"
)

const (
	incrementalVectorTemplatePath     = "__haptic_vector_entrypoint.txt"
	incrementalVectorIdentifierPrefix = "__haptic_vector_"
	incrementalVectorIndexName        = incrementalVectorIdentifierPrefix + "index"
	incrementalVectorIndicesName      = incrementalVectorIdentifierPrefix + "indices"
	incrementalVectorRuntimeName      = incrementalVectorIdentifierPrefix + "runtime"
	incrementalVectorBoundaryName     = incrementalVectorIdentifierPrefix + "boundary"
)

var incrementalVectorBaseBindingNames = [...]string{
	declController,
	declHTTP,
	declItem,
	declPlanRegistry,
	declProps,
	declRenderMode,
	declRenderSubject,
	declResources,
	declShared,
	declSource,
}

type incrementalVectorBinding struct {
	name         string
	variableType reflect.Type
}

type incrementalVectorEntryPoint struct {
	template *scriggo.Template
	original *scriggo.Template
	bindings []incrementalVectorBinding
	seal     *incrementalVectorEntryPoint
}

func compileIncrementalVectorEntryPoint(
	allTemplates map[string]string,
	privateNames map[string]struct{},
	templateName string,
	original *scriggo.Template,
	globals native.Declarations,
	profiling bool,
) (entryPoint *incrementalVectorEntryPoint) {
	defer func() {
		if recover() != nil {
			entryPoint = nil
		}
	}()
	if original == nil || globals == nil {
		return nil
	}
	if _, collision := allTemplates[incrementalVectorTemplatePath]; collision {
		return nil
	}

	bindings, bindingSet, ok := incrementalVectorBindingsFor(globals)
	if !ok {
		return nil
	}
	vectorGlobals, ok := incrementalVectorGlobals(globals)
	if !ok {
		return nil
	}

	templates := maps.Clone(allTemplates)
	templates[incrementalVectorTemplatePath] = incrementalVectorSource(templateName)
	hiddenTemplates := maps.Clone(privateNames)
	delete(hiddenTemplates, templateName)
	unsafe := false
	compiled, err := scriggo.BuildTemplate(
		&scriggoTemplateFS{
			templates:       templates,
			hiddenTemplates: hiddenTemplates,
			exposedTemplate: templateName,
		},
		incrementalVectorTemplatePath,
		&scriggo.BuildOptions{
			Globals:             vectorGlobals,
			EnableProfiling:     profiling,
			AllowGoStmt:         false,
			ExpandedTransformer: distributeOnOwnLines,
			UnexpandedTransformer: func(tree *ast.Tree) error {
				if err := rejectIncrementalVectorUnsafeSource(tree, bindingSet); err != nil {
					unsafe = true
					return err
				}
				return nil
			},
		},
	)
	if err != nil || unsafe {
		return nil
	}
	if err := compiled.DeterministicSafe(); err != nil {
		return nil
	}
	if err := compiled.RunBatch(nil); err != nil {
		return nil
	}
	expectedUsed := append(original.UsedVars(), incrementalVectorIndicesName, incrementalVectorBoundaryName)
	for _, binding := range bindings {
		expectedUsed = append(expectedUsed, binding.name)
	}
	slices.Sort(expectedUsed)
	actualUsed := compiled.UsedVars()
	slices.Sort(actualUsed)
	if !slices.Equal(actualUsed, expectedUsed) {
		return nil
	}
	entryPoint = &incrementalVectorEntryPoint{
		template: compiled,
		original: original,
		bindings: bindings,
	}
	entryPoint.seal = entryPoint
	return entryPoint
}

func incrementalVectorBindingsFor(
	globals native.Declarations,
) ([]incrementalVectorBinding, map[string]struct{}, bool) {
	bindingNames := slices.Clone(incrementalVectorBaseBindingNames[:])
	slices.Sort(bindingNames)
	bindingSet := make(map[string]struct{}, len(bindingNames))
	bindings := make([]incrementalVectorBinding, 0, len(bindingNames))
	for _, name := range bindingNames {
		declaration, exists := globals[name]
		if !exists {
			return nil, nil, false
		}
		variableType, ok := declarationVariableType(declaration)
		if !ok || !incrementalVectorReplaceableTypeSafe(variableType) {
			return nil, nil, false
		}
		bindingSet[name] = struct{}{}
		bindings = append(bindings, incrementalVectorBinding{name: name, variableType: variableType})
	}
	return bindings, bindingSet, true
}

func declarationVariableType(declaration native.Declaration) (reflect.Type, bool) {
	for {
		synchronous, ok := declaration.(native.SynchronousDeclaration)
		if !ok {
			break
		}
		declaration = synchronous.Declaration
	}
	declarationType := reflect.TypeOf(declaration)
	if declarationType == nil || declarationType.Kind() != reflect.Pointer {
		return nil, false
	}
	return declarationType.Elem(), true
}

func incrementalVectorGlobals(globals native.Declarations) (native.Declarations, bool) {
	vectorGlobals := maps.Clone(globals)
	if _, collision := vectorGlobals[incrementalVectorIndicesName]; collision {
		return nil, false
	}
	if _, collision := vectorGlobals[incrementalVectorRuntimeName]; collision {
		return nil, false
	}
	if _, collision := vectorGlobals[incrementalVectorBoundaryName]; collision {
		return nil, false
	}
	vectorGlobals[incrementalVectorIndicesName] = (*[]int)(nil)
	vectorGlobals[incrementalVectorBoundaryName] = native.Synchronous(
		(**native.VectorBoundary)(nil),
		"Begin",
		"End",
	)
	return vectorGlobals, true
}

func incrementalVectorReplaceableTypeSafe(variableType reflect.Type) bool {
	if variableType == nil {
		return false
	}
	switch variableType.Kind() {
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return false
	}
	if variableType.Kind() != reflect.Pointer &&
		reflect.PointerTo(variableType).NumMethod() > variableType.NumMethod() {
		return false
	}
	return true
}

func incrementalVectorSource(templateName string) string {
	var source strings.Builder
	source.WriteString("{% for " + incrementalVectorIndexName + " := range " + incrementalVectorIndicesName +
		" %}{% " + incrementalVectorBoundaryName + ".Begin(" + incrementalVectorIndexName + ") %}")
	for _, name := range incrementalVectorBaseBindingNames {
		source.WriteString("{% _ = " + name + " %}")
	}
	source.WriteString("{{ render " + strconv.Quote(templateName) + " }}{% " +
		incrementalVectorBoundaryName + ".End(" + incrementalVectorIndexName + ") %}{% end %}")
	return source.String()
}

func rejectIncrementalVectorUnsafeSource(tree *ast.Tree, bindings map[string]struct{}) error {
	if tree.Path == incrementalVectorTemplatePath || tree.Path == incrementalVectorCarrierTemplatePath {
		return nil
	}
	unsafe := false
	astutil.Inspect(tree, func(node ast.Node) bool {
		if unsafe || node == nil {
			return false
		}
		unsafe = incrementalVectorNodeUnsafe(node, bindings)
		return !unsafe
	})
	if unsafe {
		return strconv.ErrSyntax
	}
	return nil
}

func incrementalVectorNodeUnsafe(node ast.Node, bindings map[string]struct{}) bool {
	switch value := node.(type) {
	case *ast.Identifier:
		return value != nil && strings.HasPrefix(value.Name, incrementalVectorIdentifierPrefix)
	case *ast.UnaryOperator:
		return value != nil && value.Op == ast.OperatorAddress &&
			incrementalVectorExpressionUsesBinding(value.Expr, bindings)
	case *ast.Assignment:
		return incrementalVectorAssignmentUsesBinding(value, bindings)
	default:
		return false
	}
}

func incrementalVectorAssignmentUsesBinding(value *ast.Assignment, bindings map[string]struct{}) bool {
	if value == nil || value.Type == ast.AssignmentDeclaration {
		return false
	}
	for _, lhs := range value.Lhs {
		if identifier, ok := lhs.(*ast.Identifier); ok &&
			incrementalVectorBindingIdentifier(identifier, bindings) {
			return true
		}
	}
	return false
}

func incrementalVectorExpressionUsesBinding(
	expression ast.Expression,
	bindings map[string]struct{},
) bool {
	used := false
	astutil.Inspect(expression, func(node ast.Node) bool {
		if used || node == nil {
			return false
		}
		identifier, ok := node.(*ast.Identifier)
		used = ok && incrementalVectorBindingIdentifier(identifier, bindings)
		return !used
	})
	return used
}

func incrementalVectorBindingIdentifier(
	identifier *ast.Identifier,
	bindings map[string]struct{},
) bool {
	if identifier == nil {
		return false
	}
	_, exists := bindings[identifier.Name]
	return exists
}
