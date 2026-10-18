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

package controller

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/introspection"
	"github.com/haproxy-haptic/HAPTIC/pkg/transportsecurity/tlstest"
	pkgwebhook "github.com/haproxy-haptic/HAPTIC/pkg/webhook"
)

type probeFixture struct {
	infra       *persistentInfra
	now         time.Time
	predecessor iterationID
	leading     bool
	successor   map[string]introspection.ComponentHealth
}

func healthyIteration() map[string]introspection.ComponentHealth {
	return map[string]introspection.ComponentHealth{healthKeyInitialized: {Healthy: true}}
}

// newFailingReinit builds the ADR-0028 situation: iteration 1 completed
// startup and serves; iteration 2 is starting and reports unhealthy past the
// reinitialization grace window.
func newFailingReinit(t *testing.T) *probeFixture {
	t.Helper()
	f := &probeFixture{
		now:     time.Unix(1_000_000, 0),
		leading: true,
		successor: map[string]introspection.ComponentHealth{
			healthKeyInitialized: {Healthy: false, Error: msgStillInitializing},
		},
	}
	f.infra = &persistentInfra{
		graceNow:            func() time.Time { return f.now },
		IntrospectionServer: introspection.NewServer("localhost:0", introspection.NewRegistry()),
	}
	// refreshing pins the sibling result the test sets; no background check runs.
	f.infra.siblings = &siblingProbes{
		converged:   &siblingProbe{now: func() time.Time { return f.now }, refreshing: true},
		renderGraph: &siblingProbe{now: func() time.Time { return f.now }, refreshing: true},
	}

	f.predecessor = f.infra.NoteIterationStart()
	f.infra.NoteInitialized(f.predecessor)
	f.infra.NoteAttemptReturned(f.predecessor)
	predecessorID := f.predecessor
	f.infra.markServing(&servingIteration{
		id: predecessorID,
		health: func() map[string]introspection.ComponentHealth {
			return applyReinitGrace(f.infra, predecessorID, healthyIteration())
		},
		leading: func() bool { return f.leading },
	})

	successor := f.infra.NoteIterationStart()
	f.infra.IntrospectionServer.SetHealthChecker(func() map[string]introspection.ComponentHealth {
		entries := make(map[string]introspection.ComponentHealth, len(f.successor))
		for name, entry := range f.successor {
			entries[name] = entry
		}
		return applyReinitGrace(f.infra, successor, entries)
	})
	f.infra.NoteAttemptReturned(successor)
	f.now = f.now.Add(f.infra.reinitGraceWindow() + time.Second)
	f.setSiblings(true)
	return f
}

func (f *probeFixture) setSiblings(noneConverged bool) {
	f.infra.siblings.converged.checkedAt = f.now
	f.infra.siblings.converged.noneMatch = noneConverged
}

func (f *probeFixture) setGraphSiblings(noneMatch bool) {
	f.infra.siblings.renderGraph.checkedAt = f.now
	f.infra.siblings.renderGraph.noneMatch = noneMatch
}

func (f *probeFixture) installWebhookServer(t *testing.T) *pkgwebhook.Server {
	t.Helper()
	ca := tlstest.NewAuthority(t, "probe-test-ca")
	leaf := tlstest.NewIdentity(t, ca, "localhost", x509.ExtKeyUsageServerAuth)
	server, err := pkgwebhook.NewServer(&pkgwebhook.ServerConfig{CertPEM: leaf.Certificate, KeyPEM: leaf.Key})
	require.NoError(t, err)
	f.infra.WebhookServer = server
	return server
}

func healthy(entries map[string]introspection.ComponentHealth) bool {
	for _, entry := range entries {
		if !entry.Healthy {
			return false
		}
	}
	return true
}

func TestStartupLoadFailureRetriesWithoutBecomingReady(t *testing.T) {
	f := &probeFixture{infra: &persistentInfra{}}
	f.installWebhookServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempts := 0

	err := runIterations(ctx, testutil.NewTestLogger(), 0, func() error {
		attempts++
		id := f.infra.NoteIterationStart()
		f.infra.NoteAttemptReturned(id)
		require.True(t, healthy(f.infra.livenessHealth()))
		require.False(t, healthy(f.infra.readinessHealth()))
		if attempts == 3 {
			cancel()
		}
		return errors.New("validationTests failed")
	})

	require.NoError(t, err)
	require.Equal(t, 3, attempts)
}

// A configuration reload that keeps failing past the grace window must not
// take the leader that still serves the previous configuration out of the
// webhook Service, nor restart it into a startup that fails the same way.
func TestProbes_FailingReinitKeepsTheServingLeader(t *testing.T) {
	f := newFailingReinit(t)
	server := f.installWebhookServer(t)
	_, err := server.InstallValidatorGeneration(map[string]pkgwebhook.ValidationFunc{}, nil, nil)
	require.NoError(t, err)

	assert.False(t, healthy(f.infra.IntrospectionServer.CheckHealth()), "/healthz still reports the failing reload")
	assert.True(t, healthy(f.infra.livenessHealth()), "liveness judges the serving leader")
	readiness := f.infra.readinessHealth()
	assert.True(t, healthy(readiness), "readiness judges the serving leader: %v", readiness)
	assert.True(t, readiness[healthKeyAdmission].Healthy)
}

func TestProbes_ServingPredecessorExemptionConditions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*probeFixture)
		why    string
	}{
		{
			name:   "follower",
			mutate: func(f *probeFixture) { f.leading = false },
			why:    "a follower cannot see whether the leader runs its configuration",
		},
		{
			name:   "a sibling converged",
			mutate: func(f *probeFixture) { f.setSiblings(false) },
			why:    "a replica running the accepted configuration should lead instead",
		},
		{
			name:   "sibling result outdated",
			mutate: func(f *probeFixture) { f.infra.siblings.converged.checkedAt = f.now.Add(-2 * siblingRefreshInterval) },
			why:    "a check that stopped refreshing cannot rule out a converged sibling",
		},
		{
			name:   "no sibling check",
			mutate: func(f *probeFixture) { f.infra.siblings = nil },
			why:    "without a sibling check a converged sibling may exist",
		},
		{
			name:   "predecessor torn down",
			mutate: func(f *probeFixture) { f.infra.clearServing(f.predecessor) },
			why:    "nothing serves the previous configuration any more",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFailingReinit(t)
			tt.mutate(f)
			assert.False(t, healthy(f.infra.livenessHealth()), tt.why)
			assert.False(t, healthy(f.infra.readinessHealth()), tt.why)
		})
	}
}

func TestProbes_ConvergedIterationUsesHealthz(t *testing.T) {
	f := newFailingReinit(t)
	f.successor = healthyIteration()
	current := f.infra.currentIteration()
	f.infra.markServing(&servingIteration{id: current, health: func() map[string]introspection.ComponentHealth {
		t.Fatal("the current iteration is judged through /healthz")
		return nil
	}, leading: func() bool { return true }})

	assert.True(t, healthy(f.infra.livenessHealth()))
	f.successor[healthKeyInitialized] = introspection.ComponentHealth{Healthy: false, Error: "deployer failed"}
	assert.False(t, healthy(f.infra.livenessHealth()), "a converged iteration's failure still restarts the pod")
}

// A pod without an installed generation denies every admission request, so it
// leaves the webhook Service even while /healthz is softened by the grace.
func TestProbes_ReadinessRequiresInstalledValidators(t *testing.T) {
	f := newFailingReinit(t)
	f.now = f.now.Add(-f.infra.reinitGraceWindow())
	f.setSiblings(true)
	f.infra.clearServing(f.predecessor)
	server := f.installWebhookServer(t)

	require.True(t, healthy(f.infra.livenessHealth()), "the grace window keeps the pod alive")
	readiness := f.infra.readinessHealth()
	assert.False(t, healthy(readiness))
	assert.NotEmpty(t, readiness[healthKeyAdmission].Error)

	generation, err := server.InstallValidatorGeneration(map[string]pkgwebhook.ValidationFunc{}, nil, nil)
	require.NoError(t, err)
	assert.True(t, healthy(f.infra.readinessHealth()))

	require.NoError(t, server.RetireValidatorGenerationIfCurrent(generation))
	assert.False(t, healthy(f.infra.readinessHealth()), "a retired generation fails closed")
}

func TestProbes_ReadinessFollowsDrain(t *testing.T) {
	f := newFailingReinit(t)
	server := f.installWebhookServer(t)
	_, err := server.InstallValidatorGeneration(map[string]pkgwebhook.ValidationFunc{}, nil, nil)
	require.NoError(t, err)
	require.True(t, healthy(f.infra.readinessHealth()))

	f.infra.draining.Store(true)
	assert.False(t, healthy(f.infra.readinessHealth()), "a draining pod leaves the webhook Service")
}

// A failing attempt retries from live state and picks up a fix; a hung one
// never does, so it must still restart the pod.
func TestProbes_HungStartupFailsLiveness(t *testing.T) {
	f := newFailingReinit(t)
	hung := f.infra.NoteIterationStart()
	require.True(t, healthy(f.infra.livenessHealth()))

	f.now = f.now.Add(f.infra.reinitGraceWindow())
	f.setSiblings(true)
	liveness := f.infra.livenessHealth()
	assert.False(t, healthy(liveness))
	assert.Contains(t, liveness[healthKeyStartup].Error, "has not returned")

	f.infra.NoteAttemptReturned(hung)
	assert.True(t, healthy(f.infra.livenessHealth()))
}

// A replica whose first full render hasn't published a graph denies every
// admission request until it does (#285). It leaves the webhook Service while
// another replica can validate, and stays in it, with a note the other
// replicas read, while none can.
func TestProbes_ReadinessWaitsForTheFirstGraphWhileASiblingValidates(t *testing.T) {
	f := newFailingReinit(t)
	f.successor = healthyIteration()
	published := false
	f.infra.markServing(&servingIteration{
		id:             f.infra.currentIteration(),
		health:         healthyIteration,
		leading:        func() bool { return true },
		graphPublished: func() bool { return published },
	})
	server := f.installWebhookServer(t)
	_, err := server.InstallValidatorGeneration(map[string]pkgwebhook.ValidationFunc{}, nil, nil)
	require.NoError(t, err)

	f.setGraphSiblings(false)
	readiness := f.infra.readinessHealth()
	assert.False(t, healthy(readiness), "a sibling with a graph validates instead")
	assert.NotEmpty(t, readiness[healthKeyRenderGraph].Error)
	assert.True(t, healthy(f.infra.livenessHealth()), "a running first render is no reason to restart")

	f.setGraphSiblings(true)
	readiness = f.infra.readinessHealth()
	assert.True(t, healthy(readiness), "alone, the replica answers and explains its denials")
	assert.False(t, siblingRenderGraphPublished(http.StatusOK, readiness), "other replicas must not count it as validating")

	f.infra.siblings.renderGraph.checkedAt = f.now.Add(-2 * siblingRefreshInterval)
	assert.False(t, healthy(f.infra.readinessHealth()), "an outdated check cannot rule out a sibling with a graph")

	published = true
	readiness = f.infra.readinessHealth()
	assert.True(t, healthy(readiness))
	assert.True(t, siblingRenderGraphPublished(http.StatusOK, readiness))
}
