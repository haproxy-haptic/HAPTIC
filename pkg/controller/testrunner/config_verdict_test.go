package testrunner

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/dataplanetest"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func TestPureAssertionRejectionRequiresSuccessfulRendering(t *testing.T) {
	runner := &Runner{}
	for _, renderError := range []string{"", "writing temporary file: disk full"} {
		result := runner.runAssertion(t.Context(), &config.ValidationAssertion{Type: "contains", Pattern: "expected"}, "actual", nil, nil, nil, "", nil, renderError, nil, nil)
		assert.False(t, result.Passed)
		assert.Equal(t, renderError == "", result.ConfigRejected)
	}
}

func TestRenderFailureClassifiesOnlyUnexpectedTemplateRejection(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		expected bool
		rejected bool
	}{
		{"explicit template refusal", templating.NewRenderError("haproxy.cfg", &templating.FailError{Message: "invalid setting"}), false, true},
		{"expected refusal", &templating.FailError{Message: "negative fixture"}, true, false},
		{"unavailable render", templating.NewRenderError("haproxy.cfg", errors.New("disk full")), false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := &TestResult{}
			recordRenderFailure(result, test.err, test.expected)
			assert.Equal(t, test.rejected, result.Assertions[0].ConfigRejected)
		})
	}
}

func TestHAProxyAssertionDistinguishesRefusalFromUnavailableChecker(t *testing.T) {
	for _, test := range []struct {
		name     string
		checker  dataplanetest.Option
		rejected bool
	}{
		{"refused configuration", dataplanetest.WithRejectAll("unknown keyword 'invalid'"), true},
		{"unavailable checker", dataplanetest.WithCheck(func(string, []string) ([]byte, error) {
			return nil, errors.New("cannot start HAProxy: permission denied")
		}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Cleanup(dataplanetest.InstallFakeHAProxy(test.checker))
			directory := t.TempDir()
			paths := &dataplane.ValidationPaths{
				MapsDir: filepath.Join(directory, "maps"), SSLCertsDir: filepath.Join(directory, "ssl"),
				GeneralStorageDir: filepath.Join(directory, "general"), ConfigFile: filepath.Join(directory, "haproxy.cfg"),
			}
			result := (&Runner{}).assertHAProxyValid(t.Context(), "global\n    daemon\n", nil, &config.ValidationAssertion{}, paths)
			assert.False(t, result.Passed)
			assert.Equal(t, test.rejected, result.ConfigRejected)
		})
	}
}
