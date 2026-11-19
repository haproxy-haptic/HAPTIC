//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// TestIngressModSecuritySnippetRejectedByAdmission verifies the
// validator-sidecar half of ADR-0007's per-Ingress modsecurity-snippet
// path: an Ingress carrying a typo'd `nginx.ingress.kubernetes.io/modsecurity-snippet`
// annotation must be rejected at admission time, with the coraza
// plugin's parser diagnostic surfaced in the webhook denial message.
//
// Flow under test:
//
//  1. operator applies an Ingress with a broken SecLang directive
//  2. the apiserver dispatches the admission request to HAPTIC's webhook
//  3. HAPTIC's dryrun-validator simulates the Ingress's effect on the
//     in-memory resource store and re-renders the SPOA hub TOML with
//     the new snippet appended into `[plugins.params.coraza].directives`
//  4. the rendered TOML matches the chart's `general/spoa-hub-config.toml`
//     glob (configured under `controller.validators.entries[*].files`),
//     so the controller routes it to the `spoa-hub` validator socket
//  5. the validator sidecar parses the TOML, sees the
//     `[plugins.params.coraza]` subtree, and dispatches it to the coraza
//     plugin's `validate()` override (haproxy-spoa-hub-plugin-coraza
//     v0.4.1+), which compiles SecLang via Coraza and captures any
//     parser errors
//  6. the diagnostic flows back through the validator wire protocol to
//     HAPTIC's webhook, which converts it into an `AdmissionResponse`
//     with `Allowed=false` and the diagnostic in the message
//  7. the apiserver returns the denial to the client, who sees the
//     SecLang error in `kubectl apply` output
//
// The test deliberately uses a directive Coraza's parser flags as a
// hard error (an unknown keyword), not a runtime SecRule typo: the
// goal is to catch *config-time* malformed directives, which is the
// failure mode operators hit when they hand-craft per-Ingress WAF
// rules. Anomaly-score / SecRule-action mismatches are runtime
// behaviour and belong to the enforcement test in
// `ingress_modsecurity_snippet_test.go`.
func TestIngressModSecuritySnippetRejectedByAdmission(t *testing.T) {
	RequireVendorLibrary(t, nginxIngressLibrary)
	const host = "ingress-modsec-rejected.localdev.me"

	feature := features.New("Ingress: nginx.ingress.kubernetes.io/modsecurity-snippet rejected when SecLang is malformed").
		Assess("admission webhook denies an Ingress with an unknown SecLang directive", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			config, err := execInHAProxyPod(ctx, "deployment/"+ControllerDeploymentName, "validators", "cat", "/etc/haproxy-spoa-hub/config.toml")
			require.NoError(t, err)
			require.Contains(t, config, expectedCorazaLibrarySetting(), "controller-side validator configuration")

			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackend(ctx, t, client, ns)

			// `SecBogusDirective` is not a real SecLang keyword.
			// Coraza's parser surfaces it as
			//   "Failed to parse string: <line>: Unknown directive "..."
			// at directive-load time. The plugin's `validate()`
			// captures the line number from Coraza's debug logger and
			// returns a `Diagnostic` the validator sidecar relays to
			// HAPTIC. See haproxy-spoa-hub-plugin-coraza
			// `tests/ffi_test.rs::test_waf_create_invalid_directives`
			// for the FFI-level coverage of the same parser path.
			brokenSnippet := `SecBogusDirective totally-invalid`

			err = NewIngressExpectDenied(ctx, t, client, ns, &IngressSpec{
				Name:           "echo",
				Host:           host,
				BackendService: backend.Service,
				BackendPort:    backend.Port,
				Annotations: map[string]string{
					"nginx.ingress.kubernetes.io/modsecurity-snippet": brokenSnippet,
				},
			})

			// The admission denial message MUST mention the bad
			// directive name so operators can find the typo without
			// reading hub logs. Coraza's parser surfaces the
			// directive name lowercased (`unknown directive
			// "secbogusdirective"`) — match the lowercased form so
			// the test stays insensitive to coraza's normalisation
			// without needing a regex.
			lowered := strings.ToLower(err.Error())
			if !strings.Contains(lowered, "secbogusdirective") {
				t.Fatalf(
					"admission denial must mention the broken directive name; got: %v",
					err,
				)
			}
			// Also assert "denied" or "validate" appears so a future
			// change that returns a generic "request rejected"
			// without context fails this test instead of silently
			// passing. Both terms appear in HAPTIC's webhook denial
			// path today; widen the disjunction here when wording
			// changes intentionally.
			if !strings.Contains(lowered, "denied") &&
				!strings.Contains(lowered, "validate") {
				t.Fatalf(
					"admission denial must look like a webhook rejection (contain 'denied' or 'validate'); got: %v",
					err,
				)
			}
			return ctx
		}).
		Feature()

	testEnv.Test(t, feature)
}
