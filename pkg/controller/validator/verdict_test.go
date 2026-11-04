package validator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/configtest"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
)

func TestValidationTestsVerdict(t *testing.T) {
	for _, tt := range []struct {
		name         string
		result       configtest.Result
		err          error
		valid        bool
		inconclusive bool
		failures     []string
	}{
		{name: "passed", result: configtest.Result{Passed: true}, valid: true},
		{name: "zero result"},
		{name: "failure", result: configtest.Result{Failures: []string{"assertion failed"}}, failures: []string{"assertion failed"}},
		{name: "incomplete", result: configtest.Result{Incomplete: true}, failures: []string{"suite deadline"}, inconclusive: true},
		{name: "incomplete pass", result: configtest.Result{Passed: true, Incomplete: true}, failures: []string{"suite deadline"}, inconclusive: true},
		{name: "incomplete failure", result: configtest.Result{Incomplete: true, Failures: []string{"partial failure"}}, failures: []string{"suite deadline"}},
		{name: "error overrides pass", result: configtest.Result{Passed: true}, err: errors.New("schema unavailable"), failures: []string{"schema unavailable"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			verdict := validationTestsVerdict(tt.result, tt.err, "suite deadline")
			assert.Equal(t, tt.valid, verdict.Valid)
			assert.Equal(t, tt.failures, verdict.Errors)
			assert.Equal(t, tt.inconclusive, verdict.Inconclusive)
		})
	}
}

func TestSchemaAcquisitionFailureVerdict(t *testing.T) {
	for _, tt := range []struct {
		name         string
		err          error
		inconclusive bool
	}{
		{name: "deadline", err: fmt.Errorf("type bootstrap: %w", context.DeadlineExceeded), inconclusive: true},
		{name: "apiserver unavailable", err: apierrors.NewServiceUnavailable("overloaded"), inconclusive: true},
		{name: "apiserver timeout", err: apierrors.NewTimeoutError("openapi", 1), inconclusive: true},
		{name: "transport", err: &url.Error{Op: "Get", URL: "https://apiserver/openapi/v3", Err: syscall.ECONNREFUSED}, inconclusive: true},
		{name: "one leg timed out", err: errors.Join(context.DeadlineExceeded, errors.New("schema not found")), inconclusive: true},
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Resource: "customresourcedefinitions"}, "", errors.New("rbac"))},
		{name: "missing schema", err: errors.New("CRD widgets.example.com has no openAPIV3Schema for version v1")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bootstrap := func(context.Context, *coreconfig.Config) (*typebootstrap.Result, error) { return nil, tt.err }
			cfg := &coreconfig.Config{ValidationTests: map[string]coreconfig.ValidationTest{"t": {}}}

			templates := validateTemplates(t.Context(), cfg, bootstrap)
			assert.False(t, templates.Valid)
			assert.Equal(t, tt.inconclusive, templates.Inconclusive)
			require.Len(t, templates.Errors, 1)
			assert.Contains(t, templates.Errors[0], "schema acquisition failed")
			assert.Contains(t, templates.Errors[0], tt.err.Error())

			result, err := RunValidationTestsSync(t.Context(), cfg, bootstrap, 0, slog.Default())
			tests := validationTestsVerdict(result, err, "suite deadline")
			assert.False(t, tests.Valid)
			assert.Equal(t, tt.inconclusive, tests.Inconclusive)
		})
	}
}
