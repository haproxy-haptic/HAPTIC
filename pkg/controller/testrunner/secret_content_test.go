// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package testrunner

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

const secretFileContent = "s3cret-client-credential"

func nondeterministicFileConfig(fileType, name string, assertions ...config.ValidationAssertion) (cfg *config.Config, functions map[string]templating.GlobalFunc) {
	var sequence atomic.Int64
	functions = map[string]templating.GlobalFunc{
		"next": func(...any) (any, error) { return sequence.Add(1), nil },
	}
	cfg = &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: `global
  daemon
{%- var n, _ = next() %}
{%- var path, _ = fileRegistry.Register("` + fileType + `", "` + name + `", "` + secretFileContent + `-" + tostring(n)) %}
# {{ path }}
`},
		ValidationTests: map[string]config.ValidationTest{"leak": {Assertions: assertions}},
	}
	return cfg, functions
}

func requireNoSecretInResults(t *testing.T, results *TestResults) {
	t.Helper()
	for i := range results.TestResults {
		encoded, err := json.Marshal(results.TestResults[i].Assertions)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), secretFileContent)
	}
	output, err := FormatResults(results, OutputOptions{Format: OutputFormatSummary, Verbose: true})
	require.NoError(t, err)
	assert.NotContains(t, output, secretFileContent)
}

func TestDeterminismReportNamesButNeverPrintsSecretContent(t *testing.T) {
	for _, tc := range []struct{ fileType, name, report string }{
		{"secret", "client.secret", "secret files: "},
		{"cert", "tls.pem", "SSL certificates: "},
	} {
		t.Run(tc.fileType, func(t *testing.T) {
			cfg, functions := nondeterministicFileConfig(tc.fileType, tc.name)
			results := runSharedAnalysisGate(t, cfg, functions)

			result := results.TestResults[0]
			require.False(t, result.Passed, "a nondeterministic %s file must fail the test", tc.fileType)
			var determinism *AssertionResult
			for i := range result.Assertions {
				if result.Assertions[i].Type == "deterministic" {
					determinism = &result.Assertions[i]
				}
			}
			require.NotNil(t, determinism)
			assert.False(t, determinism.Passed)
			assert.Contains(t, determinism.Error, tc.report)
			assert.Contains(t, determinism.Error, tc.name+" differs between renders (content not shown)")
			requireNoSecretInResults(t, results)
		})
	}
}

func TestCompareAuxiliaryFilesNeverPrintsSensitiveContent(t *testing.T) {
	for name, files := range map[string]func(content string) *dataplane.AuxiliaryFiles{
		"secret file": func(c string) *dataplane.AuxiliaryFiles {
			return &dataplane.AuxiliaryFiles{SecretFiles: []auxiliaryfiles.SecretFile{{Path: "secrets/k", Content: c}}}
		},
		"SSL certificate": func(c string) *dataplane.AuxiliaryFiles {
			return &dataplane.AuxiliaryFiles{SSLCertificates: []auxiliaryfiles.SSLCertificate{{Path: "ssl/k", Content: c}}}
		},
		"sensitive map": func(c string) *dataplane.AuxiliaryFiles {
			return &dataplane.AuxiliaryFiles{SensitiveMapFiles: []auxiliaryfiles.SensitiveMapFile{{Path: "maps/k", Content: c}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			diff := compareAuxiliaryFiles(files("a "+secretFileContent), files("b "+secretFileContent))
			assert.Contains(t, diff, "/k differs between renders (content not shown)")
			assert.NotContains(t, diff, secretFileContent)
			assert.Empty(t, compareAuxiliaryFiles(files(secretFileContent), files(secretFileContent)))
		})
	}
}

func TestFailedSecretAndCertAssertionsShowNoContent(t *testing.T) {
	for _, tc := range []struct{ fileType, name, target string }{
		{"secret", "client.secret", "secret:client.secret"},
		{"cert", "tls.pem", "cert:tls.pem"},
	} {
		t.Run(tc.fileType, func(t *testing.T) {
			cfg, functions := nondeterministicFileConfig(tc.fileType, tc.name,
				config.ValidationAssertion{Type: "equals", Target: tc.target, Expected: "nope"},
				config.ValidationAssertion{Type: "contains", Target: tc.target, Pattern: "absent"},
				config.ValidationAssertion{Type: "not_contains", Target: tc.target, Pattern: "s3cret"},
			)
			results := runSharedAnalysisGate(t, cfg, functions)
			assertions := results.TestResults[0].Assertions
			require.GreaterOrEqual(t, len(assertions), 3)
			for _, assertion := range assertions[:3] {
				assert.False(t, assertion.Passed, "%s on %s", assertion.Type, tc.target)
			}
			requireNoSecretInResults(t, results)
		})
	}
}

func nondeterministicK8sResourceConfig(sensitive bool, assertions ...config.ValidationAssertion) (cfg *config.Config, functions map[string]templating.GlobalFunc) {
	var sequence atomic.Int64
	functions = map[string]templating.GlobalFunc{
		"next": func(...any) (any, error) { return sequence.Add(1), nil },
	}
	cfg = &config.Config{
		HAProxyConfig: config.HAProxyConfig{Template: "global\n  daemon\n"},
		K8sResources: map[string]config.K8sResource{"credentials": {Sensitive: sensitive, Template: `
{%- var n, _ = next() -%}
apiVersion: v1
kind: Secret
metadata:
  name: credentials
  namespace: default
stringData:
  token: "` + secretFileContent + `-{{ n }}"
`}},
		ValidationTests: map[string]config.ValidationTest{"leak": {Assertions: assertions}},
	}
	return cfg, functions
}

func determinismResult(t *testing.T, result *TestResult) *AssertionResult {
	t.Helper()
	for i := range result.Assertions {
		if result.Assertions[i].Type == "deterministic" {
			return &result.Assertions[i]
		}
	}
	require.FailNow(t, "no deterministic assertion in the result")
	return nil
}

func TestSensitiveK8sResourceStaysOutOfValidationDiagnostics(t *testing.T) {
	cfg, functions := nondeterministicK8sResourceConfig(true,
		config.ValidationAssertion{Type: "equals", Target: "k8s:credentials", Expected: "nope"},
		config.ValidationAssertion{Type: "contains", Target: "k8s:credentials", Pattern: "absent"},
		config.ValidationAssertion{Type: "not_contains", Target: "k8s:credentials", Pattern: "s3cret"},
	)
	results := runSharedAnalysisGate(t, cfg, functions)

	result := results.TestResults[0]
	require.False(t, result.Passed, "a nondeterministic sensitive k8sResource must still fail the test")
	for _, assertion := range result.Assertions[:3] {
		assert.False(t, assertion.Passed, "%s on k8s:credentials", assertion.Type)
	}
	determinism := determinismResult(t, &result)
	assert.False(t, determinism.Passed)
	assert.Equal(t, "k8sResource credentials differs between renders (content not shown)", determinism.Error)
	requireNoSecretInResults(t, results)
}

func TestUnmarkedK8sResourceDeterminismReportKeepsTheDiff(t *testing.T) {
	cfg, functions := nondeterministicK8sResourceConfig(false)
	results := runSharedAnalysisGate(t, cfg, functions)

	determinism := determinismResult(t, &results.TestResults[0])
	assert.False(t, determinism.Passed)
	assert.Contains(t, determinism.Error, secretFileContent)
}
