// Copyright 2026 Philipp Hossner
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

package renderer

import (
	"context"
	"errors"
	"log/slog"
	"reflect"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// ColdIncrementalRenderConfig supplies one non-caching render transaction.
type ColdIncrementalRenderConfig struct {
	Config             *config.Config
	Engine             templating.Engine
	StoreProvider      stores.StoreProvider
	Mode               rendercontext.RenderMode
	TemplateContext    map[string]any
	ResourceErrors     *rendercontext.ResourceErrorCollector
	NamespaceScopes    func(string) *rendercontext.NamespaceScope
	Logger             *slog.Logger
	TypedResourceTypes map[string]reflect.Type
	// Analysis, when set, must come from AnalyzeColdIncrementalRenders over
	// this Config and Engine; nil analyzes them for this render alone.
	Analysis *ColdIncrementalRenderAnalysis
}

// ColdIncrementalRenderAnalysis is the part of a cold render that depends on
// the config and engine alone, shared by every cold render of the pair.
type ColdIncrementalRenderAnalysis struct {
	analysis *incrementalRenderAnalysis
	config   *config.Config
	engine   templating.Engine
}

// AnalyzeColdIncrementalRenders prepares what cold renders of cfg and engine
// share. The result is read-only and safe for concurrent renders.
func AnalyzeColdIncrementalRenders(cfg *config.Config, engine templating.Engine) *ColdIncrementalRenderAnalysis {
	return &ColdIncrementalRenderAnalysis{
		analysis: analyzeIncrementalRender(cfg, engine),
		config:   cfg,
		engine:   engine,
	}
}

// ColdIncrementalRender runs components for offline tools without retaining a cache.
type ColdIncrementalRender struct {
	renderer *coldIncrementalRenderer
}

// NewColdIncrementalRender prepares one offline render over an existing static context.
func NewColdIncrementalRender(
	ctx context.Context,
	cfg *ColdIncrementalRenderConfig,
) (*ColdIncrementalRender, error) {
	if cfg == nil || cfg.Config == nil {
		return nil, errors.New("cold incremental render requires a config")
	}
	analysis := cfg.Analysis
	if analysis == nil {
		analysis = AnalyzeColdIncrementalRenders(cfg.Config, cfg.Engine)
	} else if analysis.config != cfg.Config ||
		(cfg.Engine != nil && !reflect.ValueOf(cfg.Engine).Comparable()) || analysis.engine != cfg.Engine {
		return nil, errors.New("cold incremental render analysis belongs to a different config or engine")
	}
	state := newIncrementalRenderStateFromAnalysis(analysis.analysis)
	if state == nil {
		return &ColdIncrementalRender{}, nil
	}
	state.scopeFor = cfg.NamespaceScopes
	if cfg.StoreProvider == nil {
		return nil, errors.New("cold incremental render requires a store provider")
	}
	runtime, err := newColdIncrementalRenderer(
		ctx,
		state,
		cfg.StoreProvider,
		cfg.Mode,
		cfg.TemplateContext,
		cfg.ResourceErrors,
		incrementalLoggerContext{logger: cfg.Logger, typedResourceTypes: cfg.TypedResourceTypes},
	)
	if err != nil {
		return nil, err
	}
	return &ColdIncrementalRender{renderer: runtime}, nil
}

// Context attaches the cold transaction to a render context.
func (r *ColdIncrementalRender) Context(ctx context.Context) context.Context {
	if r == nil || r.renderer == nil {
		return ctx
	}
	return templating.WithIncrementalRenderer(ctx, r.renderer)
}

// ValidateIncrementalCalls verifies complete canonical group placement.
func (r *ColdIncrementalRender) ValidateIncrementalCalls() error {
	if r == nil || r.renderer == nil {
		return nil
	}
	return r.renderer.ValidateIncrementalCalls()
}
