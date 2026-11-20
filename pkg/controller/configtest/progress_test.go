// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package configtest

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/dataplanetest"
	"github.com/stretchr/testify/require"
)

func TestSuiteProgressRunsEachFinishedTestOnceAcrossBudgets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, engine := cfgWithContainsTest(t, "frontend http")
		fixture := coreconfig.ValidationTest{Assertions: []coreconfig.ValidationAssertion{{Type: "haproxy_valid"}}}
		cfg.ValidationTests = map[string]coreconfig.ValidationTest{"one": fixture, "two": fixture, "three": fixture, "_global": {}, "future": {MinHAProxyVersion: "99.0"}}
		var allowed, passed atomic.Int32
		var reject atomic.Bool
		t.Cleanup(dataplanetest.InstallFakeHAProxy(dataplanetest.WithCheckContext(func(ctx context.Context, _ string, _ []string) ([]byte, error) {
			if reject.Load() {
				return []byte("[ALERT] fixture rejected"), errors.New("exit status 1")
			}
			if allowed.Add(-1) >= 0 {
				passed.Add(1)
				return nil, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		})))
		var suite SuiteProgress
		run := func() Result {
			t.Helper()
			result, err := suite.Run(t.Context(), cfg, engine, &typebootstrap.Result{}, 1, time.Minute, discardLogger())
			require.NoError(t, err)
			return result
		}
		for _, remaining := range []int{2, 1, 0} {
			allowed.Store(1)
			result := run()
			require.Equal(t, remaining, result.Remaining)
			require.Equal(t, remaining != 0, result.Incomplete)
			require.Equal(t, remaining == 0, result.Passed)
			require.Empty(t, result.Failures)
		}
		require.EqualValues(t, 3, passed.Load())
		require.Empty(t, suite.pending)

		allowed.Store(1)
		require.Equal(t, 2, run().Remaining, "a new run after a verdict checks the full suite")
		reject.Store(true)
		result := run()
		require.False(t, result.Passed)
		require.False(t, result.Incomplete)
		require.NotEmpty(t, result.Failures)
		require.Empty(t, suite.pending)
	})
}

func TestSuiteProgressRequiresUnchangedInputs(t *testing.T) {
	cfg := &coreconfig.Config{AbsentIncrementalGroups: map[string]struct{}{"missing": {}}}
	schema := &typebootstrap.Result{Types: map[string]reflect.Type{"objects": reflect.TypeFor[int]()}, Kinds: map[string]string{"objects": "Object"}}
	version := dataplane.Version{Major: 3, Minor: 4, Patch: 7, Full: "3.4.7"}
	progress := SuiteProgress{generation: 3, config: cfg, schema: schema, haproxy: version, executor: "binary", pending: []string{"remaining"}}
	clone := *cfg
	require.True(t, progress.matches(3, &clone, schema, &version, "binary"))
	require.False(t, progress.matches(0, cfg, schema, &version, "binary"))
	require.False(t, progress.matches(4, cfg, schema, &version, "binary"))
	require.False(t, progress.matches(3, cfg, schema, &version, "different binary"))
	require.False(t, progress.matches(3, cfg, schema, &dataplane.Version{}, "binary"))
	clone.AbsentIncrementalGroups = nil
	require.False(t, progress.matches(3, &clone, schema, &version, "binary"))
	changed := *schema
	changed.Types = map[string]reflect.Type{"objects": reflect.TypeFor[string]()}
	require.False(t, progress.matches(3, cfg, &changed, &version, "binary"))
	changed = *schema
	changed.Kinds = map[string]string{"objects": "Other"}
	require.False(t, progress.matches(3, cfg, &changed, &version, "binary"))
}
