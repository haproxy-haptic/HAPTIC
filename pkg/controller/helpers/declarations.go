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

package helpers

import (
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// BuildAdditionalDeclarations returns the additionalDeclarations
// map that every template engine constructed by this controller
// needs at compile time. Single source of truth — folds together
// the static `currentConfig` slot (needed by BackendServers and
// other slot-preservation logic) with one typed-resource global
// per watched-resource entry derived from typebootstrap.
//
// `result` MUST be non-nil and reflect a successful bootstrap
// against the cluster (or the embedded builtin set, for offline
// validate). Callers that don't have a real Result yet — e.g.
// the Stage-1 template validator — must obtain one via the
// injected TypeBootstrapper before calling this helper rather
// than passing nil. The previous envelope-only fallback path was
// removed because it false-positively rejected charts that used
// typed Spec/Status access (envelope only carries Metadata) and
// silently bound them to a mismatched shape elsewhere.
//
// # Adding a new engine consumer
//
// Call this function with the Result your caller obtained from
// typebootstrap.Bootstrap (or runTypeBootstrap in production),
// then pass the returned map straight into
// `templating.Options.Declarations` (or
// `helpers.NewEngineFromConfigWithOptions`'s
// `additionalDeclarations` parameter). Don't hand-merge — every
// site that did that previously had to be updated independently
// when the contract grew (Phase 4 added currentConfig; Phase
// 10–11 added typed globals); the helper bundles both.
func BuildAdditionalDeclarations(cfg *config.Config, result *typebootstrap.Result) map[string]any {
	if result == nil {
		panic("helpers: BuildAdditionalDeclarations requires non-nil Result " +
			"— see the doc comment for why envelope-only fallback was removed")
	}
	// The engine cannot name this package, so its owner registers the type the
	// exact-cycle replay gate accepts as currentConfig.
	templating.RegisterExactCyclePreviousOutputDeclaration((*renderplan.CurrentConfig)(nil))
	decls := map[string]any{
		"currentConfig": (*renderplan.CurrentConfig)(nil),
		// Current general aux files (filename → content) — lets a template read
		// its own prior output, e.g. self-rotating TLS session-ticket keys.
		// Declared as a pointer (Scriggo requires pointers for variable
		// declarations, like currentConfig); the engine derefs it, so templates
		// index it directly as a map. Always injected non-nil (empty map).
		"currentFiles": (*map[string]string)(nil),
		// Previous content of "sensitive-file" files, kept out of currentFiles.
		"currentSensitiveFiles": (*map[string]string)(nil),
	}
	// Surface every watched resource as a field on the `resources`
	// declared struct, even ones typebootstrap had no schema for
	// (core K8s types fetched without a CRD-style OpenAPI schema in
	// the offline path). The engine-declared shape must match what
	// rendercontext's addTypedResources populates at render time —
	// any missing field would surface as a runtime "wrong type"
	// error on bind. We pass them through as `extraResourceNames`;
	// BuildEngineDeclarations falls them back to the untyped store
	// shape (List/Fetch/GetSingle returning any / []any).
	var extras []string
	if cfg != nil {
		for name := range cfg.WatchedResources {
			if _, typed := result.Types[name]; typed {
				continue
			}
			if _, failed := result.Errors[name]; failed {
				continue
			}
			extras = append(extras, name)
		}
	}
	for name, decl := range typebootstrap.BuildEngineDeclarations(result, extras...) {
		decls[name] = decl
	}
	return decls
}
