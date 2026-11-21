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

package renderer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
	"github.com/haproxy-haptic/scriggo/native"
)

// planWithServer builds a plan whose single backend holds one server, so
// currentConfig() can be told apart by the address it projects.
func planWithServer(id, address string) *renderplan.Plan {
	return &renderplan.Plan{
		ID: id,
		Backends: map[string]renderplan.Backend{
			"be_app": {
				Name:    "be_app",
				Servers: []renderplan.Server{{Name: "srv1", Address: address, Port: 8080}},
			},
		},
	}
}

func serverAddress(t *testing.T, current *renderplan.CurrentConfig) string {
	t.Helper()
	require.NotNil(t, current)
	servers, ok := current.ServerIndex["be_app"]
	require.True(t, ok, "the plan's backend must appear in currentConfig")
	return servers["srv1"].Address
}

func TestCurrentConfigProjectionUsesCompiledGlobalUsage(t *testing.T) {
	tests := map[string]struct {
		templates map[string]string
		want      bool
	}{
		"unused": {
			templates: map[string]string{"haproxy.cfg": "global"},
		},
		"direct": {
			templates: map[string]string{"haproxy.cfg": `{{ currentConfig.ServerIndex["be_app"]["srv1"].Address }}`},
			want:      true,
		},
		"import": {
			templates: map[string]string{
				"haproxy.cfg": `{% import "library" for Current %}{{ Current() }}`,
				"library":     `{% macro Current() string %}{{ currentConfig.ServerIndex["be_app"]["srv1"].Address }}{% end %}`,
			},
			want: true,
		},
		"render": {
			templates: map[string]string{
				"haproxy.cfg": `{{ render "partial" }}`,
				"partial":     `{{ currentConfig.ServerIndex["be_app"]["srv1"].Address }}`,
			},
			want: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			engine, err := templating.New(test.templates, &templating.Options{
				EntryPoints: []string{"haproxy.cfg"},
				Declarations: map[string]any{
					"currentConfig": (*renderplan.CurrentConfig)(nil),
				},
			})
			require.NoError(t, err)
			service := newCurrentConfigProjectionService(t, engine)

			inputs, err := service.captureRenderAttemptInputs()
			require.NoError(t, err)
			if !test.want {
				assert.Nil(t, inputs.currentConfig)
				return
			}
			assert.Equal(t, "10.0.0.1", serverAddress(t, inputs.currentConfig))
		})
	}
}

func TestCurrentConfigProjectionIsConservativeWithoutIntrospection(t *testing.T) {
	base, err := templating.New(map[string]string{"haproxy.cfg": "global"}, &templating.Options{
		EntryPoints: []string{"haproxy.cfg"},
		Declarations: map[string]any{
			"currentConfig": (*renderplan.CurrentConfig)(nil),
		},
	})
	require.NoError(t, err)
	service := newCurrentConfigProjectionService(t, &engineWithoutGlobalUsage{Engine: base})

	inputs, err := service.captureRenderAttemptInputs()
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", serverAddress(t, inputs.currentConfig))
}

func TestCurrentConfigProjectionIsConservativeForUnknownUsage(t *testing.T) {
	engine, err := templating.New(map[string]string{
		"haproxy.cfg": `{{ indirectCurrentConfig() }}`,
	}, &templating.Options{
		EntryPoints: []string{"haproxy.cfg"},
		Declarations: map[string]any{
			"currentConfig": (*renderplan.CurrentConfig)(nil),
			"indirectCurrentConfig": func(env native.Env) string {
				renderContext := env.Context().Value(templating.RenderContextContextKey).(map[string]any)
				currentConfig := renderContext["currentConfig"].(*renderplan.CurrentConfig)
				return currentConfig.ServerIndex["be_app"]["srv1"].Address
			},
		},
	})
	require.NoError(t, err)
	service := newCurrentConfigProjectionService(t, engine)

	inputs, err := service.captureRenderAttemptInputs()
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", serverAddress(t, inputs.currentConfig))
}

type engineWithoutGlobalUsage struct {
	templating.Engine
}

func newCurrentConfigProjectionService(tb testing.TB, engine templating.Engine) *RenderService {
	tb.Helper()
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine,
		Config: &config.Config{
			HAProxyConfig: config.HAProxyConfig{Template: "global"},
			Dataplane:     testDataplaneConfig(),
		},
		Logger: slog.Default(),
	})
	service.SetAckedPlan(planWithServer("acked", "10.0.0.1"))
	return service
}

func BenchmarkCurrentConfigProjectionUnusedPlanSize(b *testing.B) {
	engine, err := templating.New(map[string]string{"haproxy.cfg": "global"}, &templating.Options{
		EntryPoints: []string{"haproxy.cfg"},
		Declarations: map[string]any{
			"currentConfig": (*renderplan.CurrentConfig)(nil),
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	for _, serverCount := range []int{0, 100_000} {
		servers := make([]renderplan.Server, serverCount)
		for index := range servers {
			servers[index] = renderplan.Server{
				Name: fmt.Sprintf("server-%06d", index), Address: "192.0.2.1",
			}
		}
		plan := &renderplan.Plan{Backends: map[string]renderplan.Backend{
			"backend": {Name: "backend", Servers: servers},
		}}
		b.Run(fmt.Sprintf("servers-%d", serverCount), func(b *testing.B) {
			benchmarkUnusedCurrentConfigProjection(b, engine, plan)
		})
	}
}

func benchmarkUnusedCurrentConfigProjection(
	b *testing.B,
	engine templating.Engine,
	plan *renderplan.Plan,
) {
	b.Helper()
	service := newCurrentConfigProjectionService(b, engine)
	b.ReportAllocs()
	for range b.N {
		service.SetAckedPlan(plan)
		service.rememberPlan(rendercontext.RenderModeReconcile, plan)
		inputs, err := service.captureRenderAttemptInputs()
		if err != nil {
			b.Fatal(err)
		}
		if inputs.currentConfig != nil {
			b.Fatal("unused currentConfig was projected")
		}
		if service.ackedPlan != nil || service.ackedCurrentConfigRoot != nil {
			b.Fatal("unused currentConfig retained a plan projection")
		}
	}
}

func TestCurrentConfig_RenderTimePlanIsTheFreshInstallFallback(t *testing.T) {
	service := &RenderService{}

	service.rememberPlan(rendercontext.RenderModeReconcile, planWithServer("plan-1", "10.0.0.1"))

	assert.Equal(t, "10.0.0.1", serverAddress(t, service.currentConfig()),
		"until the fleet ACKs a plan, the last reconcile render is the only "+
			"description of what the pods were asked to run")
}

func TestCurrentConfig_AckedPlanOutranksTheRenderTimeFallback(t *testing.T) {
	service := &RenderService{}
	service.rememberPlan(rendercontext.RenderModeReconcile, planWithServer("plan-1", "10.0.0.1"))

	service.SetAckedPlan(planWithServer("plan-2", "10.0.0.2"))

	assert.Equal(t, "10.0.0.2", serverAddress(t, service.currentConfig()),
		"the ACKed plan is what the fleet runs; the render-time plan is only a proposal")
}

func TestCurrentConfigExactRootSurvivesEquivalentACKClone(t *testing.T) {
	service := &RenderService{config: &config.Config{}}
	service.rememberPlan(rendercontext.RenderModeReconcile, planWithServer("rendered", "10.0.0.1"))

	before, err := service.captureRenderAttemptInputs(rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	service.SetAckedPlan(planWithServer("different-untrusted-id", "10.0.0.1"))
	afterACK, err := service.captureRenderAttemptInputs(rendercontext.RenderModeReconcile)
	require.NoError(t, err)

	same, err := before.currentConfigSource.SameRoot(afterACK.currentConfigSource)
	require.NoError(t, err)
	assert.True(t, same)

	materialized, err := afterACK.currentConfigSource.MaterializeCurrentConfig()
	require.NoError(t, err)
	server := materialized.ServerIndex["be_app"]["srv1"]
	server.Address = "poisoned"
	materialized.ServerIndex["be_app"]["srv1"] = server
	stable, err := afterACK.currentConfigSource.MaterializeCurrentConfig()
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", serverAddress(t, stable))

	service.SetAckedPlan(planWithServer("same-untrusted-id", "10.0.0.2"))
	changed, err := service.captureRenderAttemptInputs(rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	same, err = afterACK.currentConfigSource.SameRoot(changed.currentConfigSource)
	require.NoError(t, err)
	assert.False(t, same)
}

func TestCurrentConfig_RenderTimeFallbackStopsAfterTheFirstAck(t *testing.T) {
	service := &RenderService{}
	service.SetAckedPlan(planWithServer("plan-acked", "10.0.0.2"))

	service.rememberPlan(rendercontext.RenderModeReconcile, planWithServer("plan-newer", "10.0.0.3"))

	assert.Equal(t, "10.0.0.2", serverAddress(t, service.currentConfig()),
		"a render that no pod has taken must not displace the fleet's ACK")
}

func TestCurrentConfig_AdmissionRenderNeverBecomesTheFallback(t *testing.T) {
	service := &RenderService{}
	service.rememberPlan(rendercontext.RenderModeReconcile, planWithServer("plan-1", "10.0.0.1"))

	service.rememberPlan(rendercontext.RenderModeAdmission, planWithServer("proposal", "203.0.113.9"))

	assert.Equal(t, "10.0.0.1", serverAddress(t, service.currentConfig()),
		"admission renders are proposals, not fleet state")
}

func TestCurrentConfig_SetAckedPlanIgnoresNil(t *testing.T) {
	service := &RenderService{}
	service.rememberPlan(rendercontext.RenderModeReconcile, planWithServer("plan-1", "10.0.0.1"))

	service.SetAckedPlan(nil)

	assert.Equal(t, "10.0.0.1", serverAddress(t, service.currentConfig()))
}

func TestCurrentConfig_OwnsPlansAcrossTrustBoundaries(t *testing.T) {
	t.Run("acked plan", func(t *testing.T) {
		service := &RenderService{}
		plan := planWithServer("acked", "10.0.0.1")
		service.SetAckedPlan(plan)
		mutatePlanServer(plan, "203.0.113.1")

		assert.Equal(t, "10.0.0.1", serverAddress(t, service.currentConfig()))
	})

	t.Run("remembered plan", func(t *testing.T) {
		service := &RenderService{}
		plan := planWithServer("remembered", "10.0.0.2")
		service.rememberPlan(rendercontext.RenderModeReconcile, plan)
		mutatePlanServer(plan, "203.0.113.2")

		assert.Equal(t, "10.0.0.2", serverAddress(t, service.currentConfig()))
	})

	t.Run("staged plan", func(t *testing.T) {
		service := &RenderService{}
		plan := planWithServer("staged", "10.0.0.3")
		transaction := service.stagePlanPublication(nil, plan)
		mutatePlanServer(plan, "203.0.113.3")
		require.NoError(t, transaction.Commit(t.Context()))

		assert.Equal(t, "10.0.0.3", serverAddress(t, service.currentConfig()))
	})
}

func TestCurrentConfig_ReturnedProjectionCannotMutateStoredPlan(t *testing.T) {
	service := &RenderService{}
	service.SetAckedPlan(planWithServer("acked", "10.0.0.1"))
	projection := service.currentConfig()
	server := projection.ServerIndex["be_app"]["srv1"]
	server.Address = "203.0.113.1"
	*server.Port = 1
	projection.ServerIndex["be_app"]["srv1"] = server

	current := service.currentConfig()
	assert.Equal(t, "10.0.0.1", serverAddress(t, current))
	assert.Equal(t, int64(8080), *current.ServerIndex["be_app"]["srv1"].Port)
}

func mutatePlanServer(plan *renderplan.Plan, address string) {
	backend := plan.Backends["be_app"]
	backend.Servers[0].Address = address
	plan.Backends["be_app"] = backend
}

func TestCurrentConfig_NoPlanAndNoStoreYieldsNothing(t *testing.T) {
	service := &RenderService{}

	assert.Nil(t, service.currentConfig())
}

func TestCurrentConfig_StagedPlanPublishesOnlyAfterCommit(t *testing.T) {
	service := &RenderService{}
	service.rememberPlan(rendercontext.RenderModeReconcile, planWithServer("old", "10.0.0.1"))

	aborted := service.stagePlanPublication(nil, planWithServer("aborted", "10.0.0.2"))
	assert.Equal(t, "10.0.0.1", serverAddress(t, service.currentConfig()))
	aborted.Abort()
	assert.Equal(t, "10.0.0.1", serverAddress(t, service.currentConfig()))

	failedInner := &planTestTransaction{commitErr: errors.New("rejected")}
	failed := service.stagePlanPublication(failedInner, planWithServer("failed", "10.0.0.3"))
	require.ErrorContains(t, failed.Commit(t.Context()), "rejected")
	assert.Equal(t, "10.0.0.1", serverAddress(t, service.currentConfig()))

	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	canceled := service.stagePlanPublication(nil, planWithServer("canceled", "10.0.0.4"))
	require.ErrorIs(t, canceled.Commit(canceledCtx), context.Canceled)
	assert.Equal(t, "10.0.0.1", serverAddress(t, service.currentConfig()))

	committed := service.stagePlanPublication(nil, planWithServer("new", "10.0.0.5"))
	require.NoError(t, committed.Commit(t.Context()))
	assert.Equal(t, "10.0.0.5", serverAddress(t, service.currentConfig()))
}

func TestPlanPublicationCommitReportsPriorAbort(t *testing.T) {
	service := &RenderService{}
	inner := &planTestTransaction{}
	transaction := service.stagePlanPublication(
		inner,
		planWithServer("aborted", "10.0.0.1"),
	)

	transaction.Abort()

	require.ErrorIs(t, transaction.Commit(t.Context()), errPlanPublicationAborted)
	assert.True(t, inner.aborted)
	assert.Nil(t, service.currentConfig())
}

type planTestTransaction struct {
	commitErr error
	aborted   bool
}

func (*planTestTransaction) HasCandidates() bool { return false }

func (t *planTestTransaction) Commit(context.Context) error { return t.commitErr }

func (t *planTestTransaction) Abort() { t.aborted = true }

// TestCurrentConfigProjectionLeavesOutServerTemplates holds the projection to
// Plan.CurrentConfig for server-templates, built at once and incrementally,
// including a backend whose plain server becomes a template.
func TestCurrentConfigProjectionLeavesOutServerTemplates(t *testing.T) {
	plain := renderplan.Server{Name: "files", Address: "files.example.net", Port: 443}
	template := renderplan.Server{Name: "files", Address: "files.example.net", Port: 443, Template: 4}
	member := renderplan.Server{Name: "SRV_1", Address: "192.0.2.1", Port: 80}
	before := map[string]renderplan.Backend{
		"be_files": {Name: "be_files", Servers: []renderplan.Server{plain}},
		"be_mixed": {Name: "be_mixed", Servers: []renderplan.Server{member}},
	}
	after := map[string]renderplan.Backend{
		"be_files": {Name: "be_files", Servers: []renderplan.Server{template}},
		"be_mixed": {Name: "be_mixed", Servers: []renderplan.Server{member, template}},
	}
	want := (&renderplan.Plan{Backends: after}).CurrentConfig()
	require.NotContains(t, want.ServerIndex, "be_files")
	require.Len(t, want.ServerIndex["be_mixed"], 1)

	materialize := func(projection *exactCycleCurrentConfigProjection) renderplan.CurrentConfig {
		current, err := sealCurrentConfigRoot(projection, nil).materialize()
		require.NoError(t, err)
		return current
	}
	assert.Equal(t, want, materialize(currentConfigProjection(after)))

	changes := make([]renderplan.NamedChange[renderplan.Backend], 0, len(after))
	for _, name := range []string{"be_files", "be_mixed"} {
		previous, next := before[name], after[name]
		changes = append(changes, renderplan.NamedChange[renderplan.Backend]{Name: name, Before: &previous, After: &next})
	}
	assert.Equal(t, want, materialize(applyCurrentConfigBackendChanges(currentConfigProjection(before), changes)))
}
