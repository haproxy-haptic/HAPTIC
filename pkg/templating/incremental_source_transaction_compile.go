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
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/haproxy-haptic/scriggo"
	"github.com/haproxy-haptic/scriggo/ast"
	"github.com/haproxy-haptic/scriggo/native"
)

const (
	incrementalSourceTransactionTemplatePath = "__haptic_source_transaction_entrypoint.txt"
	incrementalSourceTransactionStartsName   = incrementalVectorIdentifierPrefix + "source_transaction_starts"
	incrementalSourceTransactionEndsName     = incrementalVectorIdentifierPrefix + "source_transaction_ends"
	incrementalSourceChildStartsName         = incrementalVectorIdentifierPrefix + "source_child_starts"
	incrementalSourceChildEndsName           = incrementalVectorIdentifierPrefix + "source_child_ends"
	incrementalSourceChildIndexesName        = incrementalVectorIdentifierPrefix + "source_child_indexes"
	incrementalSourceChildLanesName          = incrementalVectorIdentifierPrefix + "source_child_lanes"
	incrementalSourceFiberBoundaryName       = incrementalVectorIdentifierPrefix + "source_fiber_boundary"
	incrementalSourceIndexName               = incrementalVectorIdentifierPrefix + "source_index"
	incrementalSourceChildOffsetName         = incrementalVectorIdentifierPrefix + "source_child_offset"
)

func compileIncrementalSourceTransactionCarrier(
	allTemplates map[string]string,
	privateNames map[string]struct{},
	entryPoints []string,
	originals []*scriggo.Template,
	bindingSet map[string]struct{},
	globals native.Declarations,
	profiling bool,
) (*scriggo.Template, error) {
	if _, collision := allTemplates[incrementalSourceTransactionTemplatePath]; collision {
		return nil, fmt.Errorf("reserved source transaction template path %q is declared", incrementalSourceTransactionTemplatePath)
	}
	transactionGlobals := maps.Clone(globals)
	generated := []string{
		incrementalSourceTransactionStartsName,
		incrementalSourceTransactionEndsName,
		incrementalSourceChildStartsName,
		incrementalSourceChildEndsName,
		incrementalSourceChildIndexesName,
		incrementalSourceChildLanesName,
		incrementalVectorRuntimeName,
		incrementalVectorBoundaryName,
		incrementalSourceFiberBoundaryName,
	}
	for _, name := range generated {
		if _, collision := transactionGlobals[name]; collision {
			return nil, fmt.Errorf("reserved source transaction global %q is declared", name)
		}
	}
	transactionGlobals[incrementalSourceTransactionStartsName] = (*[]int)(nil)
	transactionGlobals[incrementalSourceTransactionEndsName] = (*[]int)(nil)
	transactionGlobals[incrementalSourceChildStartsName] = (*[]int)(nil)
	transactionGlobals[incrementalSourceChildEndsName] = (*[]int)(nil)
	transactionGlobals[incrementalSourceChildIndexesName] = (*[]int)(nil)
	transactionGlobals[incrementalSourceChildLanesName] = (*[]int)(nil)
	transactionGlobals[incrementalVectorRuntimeName] = native.Synchronous(
		(**incrementalSourceTransactionController)(nil),
		"BeginWave",
		"EndWave",
	)
	transactionGlobals[incrementalVectorBoundaryName] = native.Synchronous(
		(**native.VectorBoundary)(nil),
		"Begin",
		"End",
	)
	transactionGlobals[incrementalSourceFiberBoundaryName] = native.Synchronous(
		(**native.VectorFiberBoundary)(nil),
		"BeginChild",
		"EndChild",
	)

	templates := maps.Clone(allTemplates)
	templates[incrementalSourceTransactionTemplatePath] = incrementalSourceTransactionSource(entryPoints)
	exposed := make(map[string]struct{}, len(entryPoints))
	for _, name := range entryPoints {
		exposed[name] = struct{}{}
	}
	unsafe := false
	compiled, err := scriggo.BuildTemplate(
		&scriggoTemplateFS{templates: templates, hiddenTemplates: privateNames, exposedTemplates: exposed},
		incrementalSourceTransactionTemplatePath,
		&scriggo.BuildOptions{
			Globals:                transactionGlobals,
			EnableProfiling:        profiling,
			AllowGoStmt:            false,
			ExpandedTransformer:    distributeOnOwnLines,
			IsolateRootRenderState: true,
			UnexpandedTransformer: func(tree *ast.Tree) error {
				if tree.Path == incrementalSourceTransactionTemplatePath {
					return nil
				}
				if err := rejectIncrementalVectorUnsafeSource(tree, bindingSet); err != nil {
					unsafe = true
					return err
				}
				return nil
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("compiling source transaction carrier: %w", err)
	}
	if unsafe {
		return nil, errors.New("source transaction carrier failed the vector safety certificate")
	}
	if err := compiled.DeterministicSafe(); err != nil {
		return nil, fmt.Errorf("source transaction carrier is not deterministic: %w", err)
	}
	if err := compiled.RunBatch(nil); err != nil {
		return nil, fmt.Errorf("source transaction carrier is not batch certified: %w", err)
	}
	if err := certifyIncrementalNativeFunctionFrames(
		originals,
		incrementalSourceTransactionControllerTrampolines,
	); err != nil {
		return nil, fmt.Errorf("source transaction native direct-frame certificate: %w", err)
	}
	return compiled, nil
}

func incrementalSourceTransactionSource(entryPoints []string) string {
	var source strings.Builder
	source.WriteString("{% for " + incrementalVectorCarrierWaveName + " := range " +
		incrementalSourceTransactionStartsName + " %}{% " + incrementalVectorRuntimeName + ".BeginWave(" +
		incrementalVectorCarrierWaveName + ") %}{% for " + incrementalSourceIndexName + " := " +
		incrementalSourceTransactionStartsName + "[" + incrementalVectorCarrierWaveName + "]; " +
		incrementalSourceIndexName + " < " + incrementalSourceTransactionEndsName + "[" +
		incrementalVectorCarrierWaveName + "]; " + incrementalSourceIndexName + "++ %}{% " +
		incrementalVectorBoundaryName + ".Begin(" + incrementalSourceIndexName + ") %}")
	for _, name := range incrementalVectorBaseBindingNames {
		source.WriteString("{% _ = " + name + " %}")
	}
	source.WriteString("{% for " + incrementalSourceChildOffsetName + " := " +
		incrementalSourceChildStartsName + "[" + incrementalSourceIndexName + "]; " +
		incrementalSourceChildOffsetName + " < " + incrementalSourceChildEndsName + "[" +
		incrementalSourceIndexName + "]; " + incrementalSourceChildOffsetName + "++ %}{% " +
		incrementalSourceFiberBoundaryName + ".BeginChild(" + incrementalSourceIndexName + ", " +
		incrementalSourceChildIndexesName + "[" + incrementalSourceChildOffsetName + "]) %}")
	writeIncrementalLaneDispatch(&source, entryPoints,
		incrementalSourceChildLanesName+"["+incrementalSourceChildOffsetName+"]", 0, len(entryPoints))
	source.WriteString("{% " + incrementalSourceFiberBoundaryName + ".EndChild(" + incrementalSourceIndexName + ", " +
		incrementalSourceChildIndexesName + "[" + incrementalSourceChildOffsetName + "]) %}{% end %}{% " +
		incrementalVectorBoundaryName + ".End(" + incrementalSourceIndexName + ") %}{% end %}{% " +
		incrementalVectorRuntimeName + ".EndWave(" + incrementalVectorCarrierWaveName + ") %}{% end %}")
	return source.String()
}
