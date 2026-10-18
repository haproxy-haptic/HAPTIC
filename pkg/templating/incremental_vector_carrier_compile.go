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
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/ast"
	"github.com/haproxy-haptic/scriggo/native"
)

const (
	incrementalVectorCarrierTemplatePath = "__haptic_vector_carrier_entrypoint.txt"
	incrementalVectorCarrierOrderName    = incrementalVectorIdentifierPrefix + "carrier_order"
	incrementalVectorCarrierStartsName   = incrementalVectorIdentifierPrefix + "carrier_starts"
	incrementalVectorCarrierEndsName     = incrementalVectorIdentifierPrefix + "carrier_ends"
	incrementalVectorCarrierLaneName     = incrementalVectorIdentifierPrefix + "carrier_lane"
	incrementalVectorCarrierWaveName     = incrementalVectorIdentifierPrefix + "carrier_wave"
)

type incrementalVectorCarrier struct {
	template                  *scriggo.Template
	sourceTransactionTemplate *scriggo.Template
	sourceTransactionErr      error
	entryPoints               []string
	bindings                  []incrementalVectorBinding
	laneByName                map[string]int
	originals                 []*scriggo.Template
	seal                      *incrementalVectorCarrier
}

func compileIncrementalVectorCarrier(
	allTemplates map[string]string,
	privateNames map[string]struct{},
	originals map[string]*scriggo.Template,
	entryPoints map[string]*incrementalVectorEntryPoint,
	globals native.Declarations,
	profiling bool,
) (carrier *incrementalVectorCarrier, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			carrier = nil
			err = fmt.Errorf("carrier compiler panic: %v", recovered)
		}
	}()
	if globals == nil {
		return nil, fmt.Errorf("incremental globals are unavailable")
	}
	if _, collision := allTemplates[incrementalVectorCarrierTemplatePath]; collision {
		return nil, fmt.Errorf("reserved carrier template path %q is declared", incrementalVectorCarrierTemplatePath)
	}
	names := certifiedIncrementalVectorLanes(originals, entryPoints)
	if len(names) == 0 {
		return nil, fmt.Errorf("no certified vector entrypoints are available")
	}
	bindings, bindingSet, err := incrementalVectorCarrierBindings(entryPoints, names)
	if err != nil {
		return nil, err
	}
	vectorGlobals, err := incrementalVectorCarrierGlobals(globals)
	if err != nil {
		return nil, err
	}

	templates := maps.Clone(allTemplates)
	templates[incrementalVectorCarrierTemplatePath] = incrementalVectorCarrierSource(names)
	exposed := make(map[string]struct{}, len(names))
	for _, name := range names {
		exposed[name] = struct{}{}
	}
	unsafe := false
	compiled, err := scriggo.BuildTemplate(
		&scriggoTemplateFS{
			templates:        templates,
			hiddenTemplates:  privateNames,
			exposedTemplates: exposed,
		},
		incrementalVectorCarrierTemplatePath,
		&scriggo.BuildOptions{
			Globals:                vectorGlobals,
			EnableProfiling:        profiling,
			AllowGoStmt:            false,
			ExpandedTransformer:    distributeOnOwnLines,
			IsolateRootRenderState: true,
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
		if err != nil {
			return nil, fmt.Errorf("compiling carrier over %d entrypoints: %w", len(names), err)
		}
		return nil, fmt.Errorf("carrier source failed the vector safety certificate")
	}
	if err := compiled.DeterministicSafe(); err != nil {
		return nil, fmt.Errorf("carrier is not deterministic: %w", err)
	}
	if err := compiled.RunBatch(nil); err != nil {
		return nil, fmt.Errorf("carrier is not batch certified: %w", err)
	}
	originalTemplates := make([]*scriggo.Template, len(names))
	for index, name := range names {
		originalTemplates[index] = originals[name]
	}
	if err := verifyIncrementalVectorCarrierVariables(compiled, originalTemplates, bindings); err != nil {
		return nil, err
	}
	if err := certifyIncrementalNativeFunctionFrames(
		originalTemplates,
		incrementalVectorWaveControllerTrampolines,
	); err != nil {
		return nil, fmt.Errorf("carrier native direct-frame certificate: %w", err)
	}
	laneByName := make(map[string]int, len(names))
	for index, name := range names {
		laneByName[name] = index
	}
	sourceTransactionTemplate, sourceTransactionErr := compileIncrementalSourceTransactionCarrier(
		allTemplates,
		privateNames,
		names,
		originalTemplates,
		bindingSet,
		globals,
		profiling,
	)
	carrier = &incrementalVectorCarrier{
		template:                  compiled,
		sourceTransactionTemplate: sourceTransactionTemplate,
		sourceTransactionErr:      sourceTransactionErr,
		entryPoints:               slices.Clone(names),
		bindings:                  bindings,
		laneByName:                laneByName,
		originals:                 originalTemplates,
	}
	carrier.seal = carrier
	return carrier, nil
}

func certifiedIncrementalVectorLanes(
	originals map[string]*scriggo.Template,
	entryPoints map[string]*incrementalVectorEntryPoint,
) []string {
	names := make([]string, 0, len(entryPoints))
	for name, entryPoint := range entryPoints {
		if entryPoint == nil || entryPoint.seal != entryPoint || entryPoint.template == nil ||
			entryPoint.original == nil || originals[name] != entryPoint.original {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func incrementalVectorCarrierBindings(
	entryPoints map[string]*incrementalVectorEntryPoint,
	names []string,
) ([]incrementalVectorBinding, map[string]struct{}, error) {
	bindings := slices.Clone(entryPoints[names[0]].bindings)
	for _, name := range names[1:] {
		candidate := entryPoints[name].bindings
		if len(candidate) != len(bindings) {
			return nil, nil, fmt.Errorf("entrypoint %q has a different binding count", name)
		}
		for index := range bindings {
			if candidate[index].name != bindings[index].name ||
				candidate[index].variableType != bindings[index].variableType {
				return nil, nil, fmt.Errorf("entrypoint %q binding %d does not match the carrier schema", name, index)
			}
		}
	}
	bindingSet := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		bindingSet[binding.name] = struct{}{}
	}
	return bindings, bindingSet, nil
}

func incrementalVectorCarrierGlobals(globals native.Declarations) (native.Declarations, error) {
	vectorGlobals := maps.Clone(globals)
	for _, name := range []string{
		incrementalVectorCarrierOrderName,
		incrementalVectorCarrierStartsName,
		incrementalVectorCarrierEndsName,
		incrementalVectorRuntimeName,
		incrementalVectorBoundaryName,
	} {
		if _, collision := vectorGlobals[name]; collision {
			return nil, fmt.Errorf("reserved carrier global %q is declared", name)
		}
	}
	vectorGlobals[incrementalVectorCarrierOrderName] = (*[][]int)(nil)
	vectorGlobals[incrementalVectorCarrierStartsName] = (*[]int)(nil)
	vectorGlobals[incrementalVectorCarrierEndsName] = (*[]int)(nil)
	vectorGlobals[incrementalVectorRuntimeName] = native.Synchronous(
		(**incrementalVectorWaveController)(nil),
		"BeginWave",
		"EndWave",
	)
	vectorGlobals[incrementalVectorBoundaryName] = native.Synchronous(
		(**native.VectorBoundary)(nil),
		"Begin",
		"End",
	)
	return vectorGlobals, nil
}

func verifyIncrementalVectorCarrierVariables(
	compiled *scriggo.Template,
	originalTemplates []*scriggo.Template,
	bindings []incrementalVectorBinding,
) error {
	expectedOriginals := map[scriggo.UsedVariable]struct{}{}
	for _, original := range originalTemplates {
		for _, variable := range original.UsedVariables() {
			expectedOriginals[variable] = struct{}{}
		}
	}
	expectedGenerated := map[string]struct{}{
		incrementalVectorCarrierOrderName:  {},
		incrementalVectorCarrierStartsName: {},
		incrementalVectorCarrierEndsName:   {},
		incrementalVectorRuntimeName:       {},
		incrementalVectorBoundaryName:      {},
	}
	for _, binding := range bindings {
		expectedGenerated[binding.name] = struct{}{}
	}
	seenOriginals := make(map[scriggo.UsedVariable]struct{}, len(expectedOriginals))
	for _, variable := range compiled.UsedVariables() {
		if _, expected := expectedOriginals[variable]; expected {
			seenOriginals[variable] = struct{}{}
			continue
		}
		_, generated := expectedGenerated[variable.Name]
		if !generated || !variable.Native || variable.Package != scriggoMainPackage || variable.Path != "" ||
			variable.FunctionPath != incrementalVectorCarrierTemplatePath {
			return unexpectedIncrementalVectorCarrierVariable(&variable, expectedOriginals)
		}
		delete(expectedGenerated, variable.Name)
	}
	if len(seenOriginals) != len(expectedOriginals) || len(expectedGenerated) != 0 {
		return fmt.Errorf(
			"carrier omitted %d original and %d generated variables",
			len(expectedOriginals)-len(seenOriginals),
			len(expectedGenerated),
		)
	}
	return nil
}

func unexpectedIncrementalVectorCarrierVariable(
	variable *scriggo.UsedVariable,
	expectedOriginals map[scriggo.UsedVariable]struct{},
) error {
	candidates := make([]scriggo.UsedVariable, 0)
	for expected := range expectedOriginals {
		if expected.Name == variable.Name {
			candidates = append(candidates, expected)
		}
	}
	slices.SortFunc(candidates, func(a, b scriggo.UsedVariable) int {
		return strings.Compare(fmt.Sprintf("%#v", a), fmt.Sprintf("%#v", b))
	})
	return fmt.Errorf("carrier used unexpected variable %#v; original candidates: %#v", *variable, candidates)
}

func incrementalVectorCarrierSource(entryPoints []string) string {
	var source strings.Builder
	source.WriteString("{% for " + incrementalVectorCarrierWaveName + " := range " +
		incrementalVectorCarrierOrderName + " %}{% " + incrementalVectorRuntimeName + ".BeginWave(" +
		incrementalVectorCarrierWaveName + ") %}{% for " + incrementalVectorIndexName + " := " +
		incrementalVectorCarrierStartsName + "[" + incrementalVectorCarrierWaveName + "]; " +
		incrementalVectorIndexName + " < " + incrementalVectorCarrierEndsName + "[" +
		incrementalVectorCarrierWaveName + "]; " + incrementalVectorIndexName + "++ %}{% var " +
		incrementalVectorCarrierLaneName + " = " + incrementalVectorCarrierOrderName + "[" +
		incrementalVectorCarrierWaveName + "][" + incrementalVectorIndexName + "-" +
		incrementalVectorCarrierStartsName + "[" + incrementalVectorCarrierWaveName + "]] %}{% " +
		incrementalVectorBoundaryName + ".Begin(" +
		incrementalVectorIndexName + ") %}")
	for _, name := range incrementalVectorBaseBindingNames {
		source.WriteString("{% _ = " + name + " %}")
	}
	writeIncrementalLaneDispatch(&source, entryPoints, incrementalVectorCarrierLaneName, 0, len(entryPoints))
	source.WriteString("{% " + incrementalVectorBoundaryName + ".End(" +
		incrementalVectorIndexName + ") %}{% end %}{% " + incrementalVectorRuntimeName +
		".EndWave(" + incrementalVectorCarrierWaveName + ") %}{% end %}")
	return source.String()
}
