// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

// accessLogContainer is the container whose stdout carries the access log.
//
// With the vector sidecar enabled (the chart default) HAProxy logs to vector
// over loopback UDP and vector prints the records to ITS stdout, so the log is in
// the `vector` container. With vector.enabled=false HAProxy logs to its own
// stdout. Resolved once per run against the actual pod spec rather than assumed,
// so the suite keeps working in both configurations.
var (
	accessLogContainerOnce sync.Once
	accessLogContainerName string
)

func resolveAccessLogContainer(ctx context.Context, t *testing.T) string {
	t.Helper()
	accessLogContainerOnce.Do(func() {
		accessLogContainerName = "haproxy"
		pods := listHAProxyPods(t)
		if len(pods) == 0 {
			return
		}
		cmd := exec.CommandContext(ctx, "kubectl",
			kubeconfigFlag, kubeconfigPath,
			"-n", ControllerNamespace,
			"get", "pod", pods[0],
			"-o", `jsonpath={.spec.initContainers[*].name} {.spec.containers[*].name}`,
		)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Run(); err != nil {
			return
		}
		for _, name := range strings.Fields(stdout.String()) {
			if name == "vector" {
				accessLogContainerName = "vector"
				return
			}
		}
	})
	return accessLogContainerName
}

// readHAProxyAccessLog returns the access-log stdout from `since` onwards. With
// no pod argument it reads every HAProxy pod; otherwise it reads only the named
// pods. That stdout IS the access log in this chart
// (`log ... format raw`), so it is the only place the emitted log record can be
// observed — the chart's validationTests can prove the log-format directive
// renders, but not that HAProxy produces a parseable record from it.
//
// The window is bounded by TIME, not by a line count. A `--tail=N` read races the
// log volume of whatever else is running: on a busy shard (api-gateway, with WAF
// and schema tests driving traffic in parallel) this test's own record scrolled
// out of a 500-line tail before the poll observed it, and the test failed while
// the feature was fine.
func readHAProxyAccessLog(ctx context.Context, t *testing.T, since time.Time, pods ...string) (string, error) {
	t.Helper()
	container := resolveAccessLogContainer(ctx, t)
	if len(pods) == 0 {
		pods = listHAProxyPods(t)
	}
	var all strings.Builder
	for _, pod := range pods {
		cmd := exec.CommandContext(ctx, "kubectl",
			kubeconfigFlag, kubeconfigPath,
			"-n", ControllerNamespace,
			"logs", pod, "-c", container,
			"--since-time="+since.UTC().Format(time.RFC3339),
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("kubectl logs %s -c %s: %w (stderr: %s)", pod, container, err, stderr.String())
		}
		all.WriteString(stdout.String())
	}
	return all.String(), nil
}

// findAccessLogRecord waits for an access-log record whose `path` field equals
// marker and returns it decoded.
//
// Matching on the request's own unique marker path is what makes this safe under
// -parallel: every other test's traffic is on a different path.
// findAccessLogRecord returns the record of the request that succeeded. The
// callers poll until a route answers 200, and every poll before that writes a
// 404 record with the same marker path — those describe the default backend,
// not the route under test.
func findAccessLogRecord(ctx context.Context, t *testing.T, since time.Time, marker string) map[string]any {
	t.Helper()
	return findAccessLogRecordWhere(ctx, t, since, "path "+marker+" with status 200", servedMarker(marker))
}

func findAccessLogRecordInPod(ctx context.Context, t *testing.T, pod string, since time.Time, marker string) map[string]any {
	t.Helper()
	return findAccessLogRecordWhere(ctx, t, since, "path "+marker+" with status 200 in pod "+pod, servedMarker(marker), pod)
}

func servedMarker(marker string) func(map[string]any) bool {
	return func(rec map[string]any) bool {
		status, _ := rec["status"].(float64)
		return rec["path"] == marker && status == 200
	}
}

// findAccessLogRecordWhere waits for an access-log record matching want and
// returns it decoded. desc names what was being waited for in the failure
// message.
//
// A stray non-JSON line on the same stream is skipped (HAProxy's own process
// messages are not JSON, by design), but a JSON-looking line that fails to parse
// fails the test — that is what truncation at `len` or a leftover syslog prefix
// would look like.
func findAccessLogRecordWhere(ctx context.Context, t *testing.T, since time.Time, desc string, want func(map[string]any) bool, pods ...string) map[string]any {
	t.Helper()
	var found map[string]any
	var rawLine string

	// Baseline the loss counter BEFORE waiting. HAProxy never blocks traffic on
	// its log transport, so a record the receiver could not take is discarded.
	// A discarded record never arrives, which is a different fault from a slow
	// one and must not be reported as a plain timeout: waiting longer cannot fix it.
	droppedBefore, haveCounter := accessLogDropsTotal(ctx, t)

	err := testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(c context.Context) (bool, error) {
		logs, err := readHAProxyAccessLog(c, t, since, pods...)
		if err != nil {
			return false, err
		}
		for _, line := range strings.Split(logs, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "{") {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				return false, fmt.Errorf("access-log line is not valid JSON: %w (line: %s)", err, line)
			}
			if want(rec) {
				found, rawLine = rec, line
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		if droppedAfter, ok := accessLogDropsTotal(ctx, t); ok && haveCounter && droppedAfter > droppedBefore {
			t.Fatalf("no JSON access-log record matching %s arrived, and %.0f access-log record(s) were dropped while waiting "+
				"(%.0f→%.0f): the record was LOST on the way to the %s container, not merely late. Raising the wait cannot "+
				"fix this; look at why the receiver stopped reading. Original wait error: %v",
				desc, droppedAfter-droppedBefore, droppedBefore, droppedAfter, resolveAccessLogContainer(ctx, t), err)
		}
		t.Fatalf("no JSON access-log record matching %s appeared within timeout: %v", desc, err)
	}

	// `format raw` must be in effect: with the default syslog framing every
	// record would be prefixed with "<134>Jul 25 21:01:17 haproxy[1]: ", which
	// no JSON parser accepts.
	if strings.Contains(rawLine, "<134>") {
		t.Fatalf("access-log line carries a syslog prefix, so the stream is not parseable as JSON: %s", rawLine)
	}
	return found
}

// accessLogDropsTotal sums the access-log records lost in transport across the
// HAProxy pods. The bool reports whether every counter was read, so a read
// failure is never mistaken for "zero drops".
//
// With vector, the kernel drops what vector's UDP receive buffer cannot hold
// and counts it in the socket's `drops` column of /proc/net/udp. That counter
// lives as long as the socket, so HAProxy reloads do not reset it, and reading
// it directly avoids the sampling delay of haptic_access_log_dropped_records_total.
// Without vector, HAProxy writes to its own stdout and counts failures in
// haproxy_process_dropped_logs_total.
func accessLogDropsTotal(ctx context.Context, t *testing.T) (float64, bool) {
	t.Helper()
	container := resolveAccessLogContainer(ctx, t)
	var total float64
	for _, pod := range listHAProxyPods(t) {
		var v float64
		var ok bool
		if container == "vector" {
			v, ok = vectorLogSocketDrops(ctx, pod)
		} else {
			v, ok = haproxyDroppedLogs(ctx, pod)
		}
		if !ok {
			return 0, false
		}
		total += v
	}
	return total, true
}

// vectorLogSocketKey is 127.0.0.1:9514 as /proc/net/udp prints it.
const vectorLogSocketKey = "0100007F:252A"

func vectorLogSocketDrops(ctx context.Context, pod string) (float64, bool) {
	out, err := exec.CommandContext(ctx, "kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
		"exec", pod, "-c", "vector", "--", "cat", "/proc/net/udp").Output()
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != vectorLogSocketKey {
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		return v, err == nil
	}
	return 0, false
}

func haproxyDroppedLogs(ctx context.Context, pod string) (float64, bool) {
	body, err := apiProxyGet(ctx, pod, HAProxyStatsPort, "metrics")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(body, "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && name == "haproxy_process_dropped_logs_total" {
			v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			return v, err == nil
		}
	}
	return 0, false
}

func recordString(t *testing.T, rec map[string]any, field string) string {
	t.Helper()
	v, ok := rec[field]
	if !ok {
		t.Fatalf("access-log record has no %q field: %v", field, rec)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("access-log field %q is %T, want a JSON string: %v", field, v, v)
	}
	return s
}

// uuidV7Pattern is the RFC 9562 UUIDv7 shape: the version nibble is 7 and the
// variant nibble is 8/9/a/b.
var uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestHAProxyJSONAccessLog proves the structured access log end to end: that
// HAProxy actually emits one parseable JSON object per request, that the core
// fields carry the values and JSON types the chart declares, and that the
// request id is an opaque UUIDv7 rather than the historical
// %ci:%cp_%fi:%fp_%Ts_%rt:%pid format, which embedded the client IP (personal
// data) and the frontend address in a value forwarded upstream.
//
// The chart's validationTests cover the rendered log-format directive; only a
// live request can show that the record parses, that the typed fields really are
// JSON numbers, and that `format raw` suppressed the syslog framing.
func TestHAProxyJSONAccessLog(t *testing.T) {
	t.Parallel()
	host := "haproxy-json-access-log.localdev.me"
	// A real W3C traceparent; trace-id is the second dash-separated field.
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	const wantTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	var marker, ns string
	// Bound every log read to this test's own window.
	since := time.Now().Add(-5 * time.Second)

	feature := features.New("HAProxy access log: one parseable JSON record per request").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns = NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackend(ctx, t, client, ns)
			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name:           "echo-json-log",
				Host:           host,
				BackendService: backend.Service,
				BackendPort:    backend.Port,
			})
			// The marker doubles as this test's log filter, so it must be unique
			// across a parallel run; the namespace name is.
			marker = "/json-log-" + ns
			return ctx
		}).
		Assess("the record carries the core fields with their declared JSON types", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			httpclient.New(t).GET(host, marker).
				WithHeader("traceparent", traceparent).
				ExpectOK(t)

			rec := findAccessLogRecord(ctx, t, since, marker)

			assertHTTPAccessLogCoreFields(t, rec, host, ns)
			return ctx
		}).
		Assess("the request id is an opaque UUIDv7 carrying no client address", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			rec := findAccessLogRecord(ctx, t, since, marker)
			reqID := recordString(t, rec, "req_id")
			if !uuidV7Pattern.MatchString(reqID) {
				t.Errorf("req_id = %q, want an RFC 9562 UUIDv7 (unique-id-format %%[uuid(7)])", reqID)
			}
			// The historical format was %ci:%cp_%fi:%fp_%Ts_%rt:%pid — colons
			// and dots are what an embedded address would bring with it.
			if strings.ContainsAny(reqID, ":.") {
				t.Errorf("req_id = %q contains an address-like separator; the id must carry no client or frontend address", reqID)
			}
			return ctx
		}).
		Assess("an inbound W3C traceparent joins the record to a trace", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			rec := findAccessLogRecord(ctx, t, since, marker)
			if got := recordString(t, rec, "trace_id"); got != wantTraceID {
				t.Errorf("trace_id = %q, want %q (extracted from the traceparent header)", got, wantTraceID)
			}
			return ctx
		}).
		Feature()

	testEnv.Test(t, feature)
}

func assertHTTPAccessLogCoreFields(t *testing.T, rec map[string]any, host, ns string) {
	t.Helper()
	if got := recordString(t, rec, "method"); got != "GET" {
		t.Errorf("method = %q, want GET", got)
	}
	if got := recordString(t, rec, "host"); got != host {
		t.Errorf("host = %q, want %q", got, host)
	}
	// Typed items must decode as JSON numbers, not strings: a log store
	// with a numeric mapping rejects the string form.
	status, ok := rec["status"].(float64)
	if !ok {
		t.Fatalf("status is %T, want a JSON number (declared %%(status:sint)ST): %v", rec["status"], rec["status"])
	}
	if status != 200 {
		t.Errorf("status = %v, want 200", status)
	}
	if _, ok := rec["total_time_ms"].(float64); !ok {
		t.Errorf("total_time_ms is %T, want a JSON number", rec["total_time_ms"])
	}
	if got := recordString(t, rec, "ts"); got == "" {
		t.Error("ts is empty: `format raw` drops the syslog timestamp, so the record must carry its own")
	}
	// resource is the join key back to Kubernetes.
	if want, got := ns+"/echo-json-log", recordString(t, rec, "resource"); got != want {
		t.Errorf("resource = %q, want %q", got, want)
	}
	// An allowed request must not look denied. The field is normally ABSENT
	// here rather than empty: vector's omit-empty transform
	// (vector.omitEmptyLogFields, on by default) strips fields whose value is
	// the empty string, and no gate fired. Accept either, so the assertion
	// holds with the transform on or off — but never accept a non-empty value.
	if v, ok := rec["denied_by"]; ok {
		got, isStr := v.(string)
		if !isStr {
			t.Errorf("denied_by is %T, want a JSON string: %v", v, v)
		} else if got != "" {
			t.Errorf("denied_by = %q on an allowed request, want empty or absent", got)
		}
	}
}

// TestHAProxyJSONAccessLogGatewayResource proves the owner-identity fix for
// Gateway API traffic: `resource` must be the HTTPRoute's <namespace>/<name>.
//
// Before the fix, base.yaml's identity cascade read `req.gw_rule_id` — a scope
// nothing writes — so the branch never fired and Gateway requests fell through
// to the backend-name split, yielding resource_id="gtw/<namespace>". Every
// per-resource feature map (WAF, external auth, shared rate limit, cache, schema
// validation) is looked up with that value, so the log field is also the
// cheapest observable proof that the lookup key is now correct.
func TestHAProxyJSONAccessLogGatewayResource(t *testing.T) {
	t.Parallel()
	host := "haproxy-json-access-log-gw.localdev.me"

	var marker, ns string
	var fwd ServiceForward
	since := time.Now().Add(-5 * time.Second)

	feature := features.New("HAProxy access log: Gateway traffic reports the HTTPRoute as its owning resource").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns = NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackend(ctx, t, client, ns)
			NewGateway(ctx, t, ns, "json-log-gateway")
			fwd = ForwardGateway(ctx, t, ns, "json-log-gateway", 80)
			NewHTTPRoute(ctx, t, ns, &HTTPRouteSpec{
				Name:        "echo-json-log-gw",
				GatewayName: "json-log-gateway",
				Hostnames:   []string{host},
				Rules: []HTTPRouteRule{{
					PathType: "PathPrefix",
					Path:     "/",
					BackendRefs: []HTTPRouteBackendRef{{
						Service: backend.Service,
						Port:    backend.Port,
					}},
				}},
			})
			// Route-gated convergence marker (issue #71): the fragment appears
			// only once THIS route's backend renders.
			waitForRouteDeployed(ctx, t, client, httpRouteGVR, ns, "echo-json-log-gw")
			marker = "/json-log-gw-" + ns
			return ctx
		}).
		Assess("resource is the HTTPRoute namespace/name", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			httpclient.ForForwarded(t, fwd.HTTPPort, 0).GET(host, marker).ExpectOK(t)

			rec := findAccessLogRecord(ctx, t, since, marker)
			want := ns + "/echo-json-log-gw"
			got := recordString(t, rec, "resource")
			if got != want {
				t.Errorf("resource = %q, want %q", got, want)
			}
			wantRule := "h_" + ns + "_echo-json-log-gw_0"
			if got := recordString(t, rec, "gw_route"); got != wantRule {
				t.Errorf("gw_route = %q, want %q", got, wantRule)
			}
			return ctx
		}).
		Feature()

	testEnv.Test(t, feature)
}

// TestHapticRequestIDAcceptInboundRejectsMalformed proves the bound on
// `haproxy-haptic.org/request-id-accept-inbound`: a client-supplied id is
// preserved only when it is a well-formed identifier, and anything else is
// replaced with a generated one.
//
// The chart validationTest pins the rendered ACL; only a live request shows what
// the BACKEND actually receives, which is the thing that matters — without the
// bound, a client chooses the correlation id that lands in the application's own
// logs, including an oversized value or one crafted to collide with another
// request's id. (Envoy takes the same position on a non-UUID x-request-id.)
func TestHapticRequestIDAcceptInboundRejectsMalformed(t *testing.T) {
	t.Parallel()
	host := "haptic-request-id-inbound.localdev.me"
	const wellFormedID = "client-supplied-42"
	// Spaces are not in ^[A-Za-z0-9._:-]{1,128}$, so this must be replaced.
	const malformedID = "bogus id with spaces"

	feature := features.New("Ingress: request-id-accept-inbound honours only a well-formed client id").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackend(ctx, t, client, ns)
			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name:           "echo-request-id",
				Host:           host,
				BackendService: backend.Service,
				BackendPort:    backend.Port,
				Annotations: map[string]string{
					"haproxy-haptic.org/request-id":                "true",
					"haproxy-haptic.org/request-id-accept-inbound": "true",
				},
			})
			httpclient.New(t).GET(host, "/").ExpectOK(t)
			return ctx
		}).
		Assess("a well-formed client id is preserved", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			resp := httpclient.New(t).GET(host, "/").
				WithHeader("X-Request-ID", wellFormedID).
				ExpectOK(t)
			if resp.Echo == nil {
				t.Fatalf("expected echo-server JSON, got %d bytes", len(resp.Body))
			}
			if got := resp.Echo.Headers["x-request-id"]; got != wellFormedID {
				t.Errorf("backend saw X-Request-ID %q, want the client's %q — accept-inbound should preserve a well-formed id", got, wellFormedID)
			}
			return ctx
		}).
		Assess("a malformed client id is replaced with a generated one", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			resp := httpclient.New(t).GET(host, "/").
				WithHeader("X-Request-ID", malformedID).
				ExpectOK(t)
			if resp.Echo == nil {
				t.Fatalf("expected echo-server JSON, got %d bytes", len(resp.Body))
			}
			got := resp.Echo.Headers["x-request-id"]
			if got == malformedID {
				t.Fatalf("backend saw the client's malformed X-Request-ID %q — it must be replaced", got)
			}
			if !uuidV7Pattern.MatchString(got) {
				t.Errorf("replacement X-Request-ID = %q, want a generated UUIDv7", got)
			}
			return ctx
		}).
		Feature()

	testEnv.Test(t, feature)
}
