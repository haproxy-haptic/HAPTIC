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

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// DumpLogsOnFailure registers a t.Cleanup that, if the test fails, writes
// diagnostic artifacts under debug-logs/<sanitised-test-name>/ at the repo
// root. CI uploads this directory as an artifact.
//
// The dump captures everything a future debugger would want without
// requiring a re-run:
//   - Controller pod logs (all containers)
//   - HAProxy pod logs (haproxy, agent, spoa-hub containers)
//   - Backend fixture pod logs (echo-server, auth-server, blocklist-server)
//   - Pod events from the test namespace and the controller namespace
//   - HAProxyCfg JSON (with status), so the rendered config and per-pod
//     deployment state survive
//   - The full manifest list of the test namespace
//
// Errors during the dump are logged but never fail the test — this is a
// best-effort diagnostic, not a correctness check.
func DumpLogsOnFailure(t *testing.T, namespace string) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		dumpDir, err := failureDumpDir(t)
		if err != nil {
			t.Logf("DumpLogsOnFailure: setup dump dir: %v", err)
			return
		}
		t.Logf("Test failed — dumping diagnostics to %s", dumpDir)

		// Best-effort dumps. Each in its own helper so one failure
		// doesn't prevent the others.
		// --prefix for the same reason the HAProxy dump below uses it, and one
		// more: this selector spans BOTH controller replicas, and kubectl
		// concatenates them pod by pod rather than merging by time. Without the
		// prefix the file interleaves leader and follower with no way to tell
		// them apart, and a reader grepping it by timestamp silently mixes the
		// two — which reads as a controller doing impossible things (renders
		// with no matching deploy, ops that never appear). The per-pod
		// continuous tailers stay the instrument for a single replica's
		// timeline; this dump is the wider net.
		dumpCommand(t, dumpDir, "controller-logs.txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"logs", "-l", LabelSelectorController, "--all-containers", "--prefix", "--tail=50000")

		// --prefix tags each line with [pod/<name> container/<name>] so the
		// haproxy / agent / spoa-hub containers can be told apart when
		// reading back the dump. Without it, all three containers'
		// stdout collapses into one un-attributable stream — investigating
		// publish-step latency or apply errors requires re-running the
		// test just to know which container said what.
		//
		// --tail=50000 (vs the historical 500): at debug level the agent
		// logs a line per apply and HAProxy one per reload, and the
		// spoa-hub emits ~200 lines/s under parallel-test load. With three
		// containers shared in one selector, 500 lines amounts to <1 s of
		// real-time coverage — a CI failure happening more than a second
		// before dump time has its log clipped out of the artifact
		// entirely. 50000 covers ~4 minutes of activity per container,
		// which fits within the artifact upload limit and reliably
		// preserves the window around a probe-loop failure (typical e2e
		// probe loop <= 30 s).
		dumpCommand(t, dumpDir, "haproxy-logs.txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"logs", "-l", LabelSelectorHAProxy, "--all-containers", "--prefix", "--tail=50000")

		// A sidecar that died and was restarted took its stdout with it: the
		// current instance's log says nothing about why the previous one
		// exited, and the pod is Ready again by dump time. Without --previous
		// a restart is only visible as a number in `kubectl get pods`. This is
		// what makes an OOMKill or a silent exit(0) attributable rather than
		// merely observable — see the per-container state dump below for which
		// container to look at.
		dumpCommand(t, dumpDir, "haproxy-logs-previous.txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"logs", "-l", LabelSelectorHAProxy, "--all-containers", "--prefix", "--previous", "--tail=50000")

		// Per-container ready/restart/lastState. `reason` separates an OOMKill
		// from a clean exit, and `exitCode` separates a crash from a graceful
		// shutdown — the pod-level READY column shows neither.
		dumpCommand(t, dumpDir, "haproxy-container-states.txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"get", "pods", "-l", LabelSelectorHAProxy, "-o",
			"jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{range .status.initContainerStatuses[*]}"+
				"  {.name}{\"\\tready=\"}{.ready}{\"\\trestarts=\"}{.restartCount}"+
				"{\"\\tlastState=\"}{.lastState}{\"\\tsidecar\\n\"}{end}{range .status.containerStatuses[*]}"+
				"  {.name}{\"\\tready=\"}{.ready}{\"\\trestarts=\"}{.restartCount}"+
				"{\"\\tlastState=\"}{.lastState}{\"\\n\"}{end}{end}")

		// Node memory pressure and allocatable-vs-limits. A node-scoped
		// SystemOOM kills the container with the highest oom_score_adj, which
		// is not necessarily the one that grew — so a container's own limit
		// cannot be read as the cause without this.
		dumpCommand(t, dumpDir, "nodes.txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "describe", "nodes")

		dumpCommand(t, dumpDir, "backend-fixtures-logs.txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", SharedFixturesNamespace,
			"logs", "--all-containers", "--prefix", "--tail=200", "-l", "app")
		dumpCommand(t, dumpDir, "test-backend-logs.txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", namespace,
			"logs", "--all-containers", "--prefix", "--tail=2000", "-l", "app")

		// Valkey and Sentinel stdout from the shared rate-limit store. A stalled
		// failover is only diagnosable from Sentinel's own election log
		// (+odown / +selected-slave / +promoted-slave / +failover-end-for-timeout);
		// the controller and HAProxy logs show the downstream symptom and never
		// say which node Sentinel picked. --prefix separates the two containers,
		// which disagree by design during a failover.
		dumpCommand(t, dumpDir, "rate-limit-store-logs.txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"logs", "-l", labelSelectorRateLimitStore, "--all-containers", "--prefix", "--tail=5000")

		// Replication as each node sees it. Sentinel can report a promotion that
		// the node itself contradicts (role:slave with master_link_status:down),
		// and only the node's own INFO shows that split.
		dumpRateLimitStoreReplication(t, dumpDir)

		dumpCommand(t, dumpDir, "controller-namespace-events.txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"get", "events", "--sort-by=.lastTimestamp")

		dumpCommand(t, dumpDir, "test-namespace-events.txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", namespace,
			"get", "events", "--sort-by=.lastTimestamp")

		dumpCommand(t, dumpDir, "test-namespace-resources.yaml",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", namespace,
			"get", "all,ingresses,httproutes,secrets,configmaps", "-o", "yaml")

		dumpCommand(t, dumpDir, "haproxycfg.yaml",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"get", "haproxycfg", "-o", "yaml")

		dumpCommand(t, dumpDir, "controller-pods.yaml",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"get", "pods", "-o", "yaml")

		// EndpointSlices for the test namespace AND across the cluster. Two
		// files because the test-namespace ones are the load-bearing data for
		// rolling-restart analysis (which IPs / conditions did our backend
		// expose at failure time), while the cluster-wide dump is what
		// disambiguates the aggregated "endpoints modified=1" counters in
		// the controller log when sibling parallel tests churn at the same
		// time.
		dumpCommand(t, dumpDir, "test-namespace-endpointslices.yaml",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", namespace,
			"get", "endpointslices", "-o", "yaml")
		// -A after the subcommand: --all-namespaces belongs to `get`, not to
		// kubectl itself, so leading it makes kubectl read it as a plugin name.
		dumpCommand(t, dumpDir, "all-endpointslices.yaml",
			"kubectl", kubeconfigFlag, kubeconfigPath,
			"get", "endpointslices", "-A", "-o", "yaml")

		// HAProxy's view of every server — admin state, operational state,
		// address and port. Ground truth for "where does this server point
		// right now" that no controller log can match, read from HAProxy
		// itself. One file per HAProxy pod keeps the dump readable when
		// there are multiple replicas.
		dumpHAProxyRuntimeServers(t, dumpDir)
	})
}

// dumpHAProxyRuntimeServers writes, per HAProxy pod, what the agent believes
// it applied and what HAProxy is actually running. Without it, post-mortem
// inspection has to reverse-engineer the pod's state from the rendered config
// alone — which is exactly the kind of forensics that takes hours.
func dumpHAProxyRuntimeServers(t *testing.T, dumpDir string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*1_000_000_000) // 30s
	defer cancel()

	// Get the HAProxy pod names.
	podsCmd := exec.CommandContext(ctx, "kubectl",
		kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
		"get", "pods", "-l", LabelSelectorHAProxy,
		"-o", "jsonpath={.items[*].metadata.name}")
	podsOut, err := podsCmd.Output()
	if err != nil {
		t.Logf("dumpHAProxyRuntimeServers: list pods: %v", err)
		return
	}

	pods := bytes.Fields(podsOut)
	if len(pods) == 0 {
		return
	}

	for _, podBytes := range pods {
		pod := string(podBytes)
		dumpCommand(t, dumpDir, "spoa-metrics-"+pod+".txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pod, "-c", "haproxy", "--", "curl", "-fsS", "--max-time", "5",
			"http://127.0.0.1:9095/metrics")
		dumpCommand(t, dumpDir, "haproxy-agent-state-"+pod+".json",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pod, "-c", "agent", "--", "haptic", "agent", "state", "-o", "json")

		// The configuration HAProxy is running, read from disk — single shot,
		// no per-backend iteration needed.
		dumpCommand(t, dumpDir, "haproxy-config-raw-"+pod+".cfg",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pod, "-c", "haproxy", "--",
			"cat", "/etc/haproxy/haproxy.cfg")

		// The map files, which is where routing actually lives: the config
		// selects a backend through map lookups, so a request can 404 with the
		// right backend present and the map missing its host. /v1/state reports
		// only each map's digest and size, which cannot answer "is this host in
		// there" — and that is the question every routing failure asks.
		dumpCommand(t, dumpDir, "haproxy-maps-"+pod+".txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pod, "-c", "haproxy", "--",
			"sh", "-c", `for m in /etc/haproxy/maps/*.map; do `+
				`echo "=== $m ($(wc -l < "$m") entries)"; cat "$m"; done`)

		// HAProxy's runtime (in-memory) server table via `show servers
		// state` on the master socket, routed to worker 1 with the
		// `@1` prefix. This dumps every backend's server in a single
		// shot: srv_addr / srv_port / srv_op_state / srv_admin_state
		// per row. Ground truth for "what address was this server
		// carrying at the moment of the failure".
		//
		// The master socket relays exactly one command per connection,
		// which is all this needs; the worker stats socket the agent
		// uses would do as well.
		dumpCommand(t, dumpDir, "haproxy-show-servers-state-"+pod+".txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pod, "-c", "haproxy", "--",
			"sh", "-c", `printf '@1 show servers state\n' | socat - UNIX-CONNECT:/etc/haproxy/haproxy-master.sock`)

		// DroppedLogs counts records HAProxy failed to send at all. `show info`
		// also carries Uptime, so a value here is attributable to the run.
		dumpCommand(t, dumpDir, "haproxy-show-info-"+pod+".txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pod, "-c", "haproxy", "--",
			"sh", "-c", `printf '@1 show info\n' | socat - UNIX-CONNECT:/etc/haproxy/haproxy-master.sock`)

		// Records the kernel discarded because vector's UDP receive buffer was
		// full: the `drops` column of the 0100007F:252A row (127.0.0.1:9514).
		// It separates "served but never logged" (#109, #677) from "never emitted".
		dumpCommand(t, dumpDir, "vector-udp-sockets-"+pod+".txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pod, "-c", "vector", "--", "cat", "/proc/net/udp")

		// Whether vector was alive: while it restarts nothing reads the port.
		dumpCommand(t, dumpDir, "vector-container-state-"+pod+".txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"get", "pod", pod, "-o",
			`jsonpath={range .status.containerStatuses[?(@.name=="vector")]}restarts={.restartCount}{"\n"}ready={.ready}{"\n"}lastState={.lastState}{"\n"}{end}`)

		// HAProxy's captured request/response parse errors via `show errors`
		// on the master socket (`@1` → worker 1, same routing as above).
		// HAProxy retains the last erroring request and the last erroring
		// response PER PROXY, including the full offending bytes and the byte
		// offset where parsing failed. This is the ground truth for a
		// `<BADREQ>` 400 (which the access log reports only as
		// `"term":"PR--"` with an empty request):
		// it shows exactly which request HAProxy could not parse and where.
		// Load-bearing for diagnosing 400s that surface under reload churn —
		// HAProxy must serve correctly across reloads, not merely avoid 503s
		// (a malformed-request 400 mid-reload is just as much a real bug).
		dumpCommand(t, dumpDir, "haproxy-show-errors-"+pod+".txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pod, "-c", "haproxy", "--",
			"sh", "-c", `printf '@1 show errors\n' | socat - UNIX-CONNECT:/etc/haproxy/haproxy-master.sock`)
	}
}

// dumpRateLimitStoreReplication records each store node's own view of
// replication, plus Sentinel's, so the two can be compared directly.
//
// The pair is what makes a stalled failover readable: Sentinel reporting a node
// as `master` while that node reports `role:slave` — worse, with master_host
// equal to its own address — is the signature of Sentinel holding one pod under
// two identities and reconfiguring the node it just promoted.
func dumpRateLimitStoreReplication(t *testing.T, dumpDir string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	podsCmd := exec.CommandContext(ctx, "kubectl",
		kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
		"get", "pods", "-l", labelSelectorRateLimitStore,
		"-o", "jsonpath={.items[*].metadata.name}")
	podsOut, err := podsCmd.Output()
	if err != nil {
		// The store only exists in the rate-limit shard; absence is normal.
		return
	}

	for _, podBytes := range bytes.Fields(podsOut) {
		pod := string(podBytes)
		dumpCommand(t, dumpDir, "rate-limit-store-replication-"+pod+".txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pod, "-c", "valkey", "--",
			"valkey-cli", "-p", "6379", "info", "replication")
		dumpCommand(t, dumpDir, "rate-limit-store-sentinel-"+pod+".txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pod, "-c", "sentinel", "--",
			"valkey-cli", "-p", "26379", "sentinel", "master", "haptic-rate-limit")
		// Why a stalled failover picked nobody. `-failover-abort-no-good-slave`
		// names no replica and no reason; sentinelSelectReplica's rejections are
		// only visible as per-replica flags, master-link-down-time and
		// info-refresh, which the `master` view above does not carry.
		dumpCommand(t, dumpDir, "rate-limit-store-sentinel-replicas-"+pod+".txt",
			"kubectl", kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			"exec", pod, "-c", "sentinel", "--",
			"valkey-cli", "-p", "26379", "sentinel", "replicas", "haptic-rate-limit")
	}
}

// dumpCommand runs cmd and writes its combined output to filename inside
// dumpDir. Failures are logged via t.Logf but do not fail the test.
func dumpCommand(t *testing.T, dumpDir, filename, cmd string, args ...string) {
	t.Helper()
	out := runCommandCapture(30*time.Second, cmd, args...)
	if writeErr := os.WriteFile(filepath.Join(dumpDir, filename), out, 0o600); writeErr != nil {
		t.Logf("DumpLogsOnFailure: write %s: %v", filename, writeErr)
	}
}

// runCommandCapture runs cmd with the given timeout and returns its combined
// output, appending a failure note (including stderr) when the command errors
// so the captured artifact is self-contained even when the command failed.
func runCommandCapture(timeout time.Duration, cmd string, args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c := exec.CommandContext(ctx, cmd, args...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	runErr := c.Run()

	out := stdout.Bytes()
	if runErr != nil {
		out = append(out, []byte(fmt.Sprintf(
			"\n--- command failed: %v\nstderr:\n%s\n", runErr, stderr.String()))...)
	}
	return out
}

// failureDumpDir returns (and creates if needed) a per-test directory
// under <repo>/debug-logs/. The directory name is the test name with
// non-DNS-safe characters replaced by underscores.
func failureDumpDir(t *testing.T) (string, error) {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	safe := sanitizeForFilesystem(t.Name())
	dir := filepath.Join(root, "debug-logs", safe)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// sanitizeForFilesystem replaces characters that are awkward in filesystem
// paths (slashes from t.Name() of subtests, etc.) with underscores.
func sanitizeForFilesystem(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_':
			out[i] = c
		default:
			out[i] = '_'
		}
	}
	return string(out)
}
