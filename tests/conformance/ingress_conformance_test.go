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

//go:build ingress_conformance

// Package conformance also runs the upstream Kubernetes
// `ingress-controller-conformance` suite against the chart's IngressClass.
// Builds under the `ingress_conformance` tag so it stays out of regular
// test runs and out of the gateway-conformance binary.
//
// Execution model: the test binary runs as a sibling container on the
// kind docker network (see `make test-ingress-conformance` /
// Dockerfile.ingress-conformance-test). Unlike the Gateway API
// conformance suite (which imports `sigs.k8s.io/gateway-api/conformance`
// as a Go library), the upstream Ingress suite is a standalone test
// binary built with `go test -c` from
// `github.com/kubernetes-sigs/ingress-controller-conformance`. It emits
// Cucumber JSON reports. This wrapper exec's it, parses the JSON, and
// fans every feature/scenario out as `t.Run` subtests so individual
// scenarios show up as named Go test failures (and so `-test.run`
// regexes work the same way the gateway suite does).
//
// Upstream status: dormant. Last commit
// d920ed36a0076e169a9a329a850844ab3a695ae8 on 2023-08-28, no releases,
// single maintainer, an open `lifecycle/frozen` issue about the
// deprecated `gcr.io/k8s-testimages` image registry. It remains the
// only SIG-Network conformance vehicle for the v1 Ingress resource, so
// we still wire it up, but we pin to that commit and do not auto-track
// `master`. The Makefile target builds the binary from a `git clone` at
// the pinned SHA.
//
// To run locally:
//
//	make test-e2e                  # brings up the haptic-e2e kind cluster
//	make test-ingress-conformance  # builds the test image, runs as a sibling container
//
// The suite expects an existing `haptic-e2e` kind cluster with the
// chart deployed at default values (so the `haptic` IngressClass is
// accepted by the controller). `make test-e2e` leaves that cluster in
// place by default.
//
// No skips, no opt-outs: every scenario the upstream binary reports
// surfaces as a named `t.Run` subtest. If a scenario fails on first run
// the fix is in the chart, not in this test — see
// `feedback_skipped_tests_are_shipped_bugs.md`.
//
// Vendored upstream patch (PR #101) — convergence retry:
//
// The upstream HTTP step does `client.Do(req)` once with no retry
// budget on 5xx. Combined with no propagation buffer between
// "waitForEndpoints succeeds" and "fire request," every Ingress
// controller — including upstream-blessed ones — loses scenarios to
// the inherent K8s-Endpoints-to-runtime-config gap. Upstream PR #101
// (Tam Mach, 2022) added an `awaitConvergence` wrapper requiring 3
// consecutive identical-status-code responses over up to 30s. It was
// never merged. Cilium carries it in their fork; this repo vendors it
// at tests/conformance/patches/0001-convergence-retry-mechanism.patch,
// applied via `git apply` in the test-ingress-conformance Makefile
// target after the upstream checkout. Without the patch the suite was
// flaky on 2-4 scenarios per run, randomly distributed; with it the
// suite is consistently green.
package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

// ingressClassName is the IngressClass the chart provisions. The chart's
// values default `ingressClass.name` to "haptic" — keep this in sync if
// that ever changes.
const ingressClassName = "haptic"

// upstreamBinaryPath is the path inside the conformance container where
// Dockerfile.ingress-conformance-test bakes the upstream binary. Overridable
// via $INGRESS_CONFORMANCE_BIN so a developer can point at a local build.
const upstreamBinaryPath = "/usr/local/bin/ingress-controller-conformance"

// upstreamFeaturesDir is the directory the upstream binary loads
// `<name>.feature` files from at runtime, resolved relative to the
// binary's working directory. Dockerfile.ingress-conformance-test bakes
// a snapshot of the upstream `features/` tree at `/features/` and we
// run the binary with WorkingDir=/, so the relative `features/...`
// lookup resolves. Overridable via $INGRESS_CONFORMANCE_FEATURES_DIR.
const upstreamFeaturesDir = "/"

// waitForIngressStatus and waitForReady bound how long the upstream binary
// waits for two different things:
//
//   - waitForIngressStatus: how long for HAPTIC to populate
//     Ingress.Status.LoadBalancer after we create the Ingress. This is
//     haptic's reconcile budget — render → deploy → reload → status
//     should land well under 5s on a healthy cluster, and we keep 10s
//     here so a regression in haptic's reconcile speed surfaces
//     immediately instead of being absorbed (see
//     `feedback_no_blind_timeout_bumps.md`).
//
//   - waitForReady: how long for the upstream-created BACKEND POD
//     (the conformance echo image) to become Ready and have a populated
//     Endpoints entry. This is NOT haptic's reconcile — it's the kube
//     scheduler placing the pod, kubelet pulling/starting the container,
//     and the readiness probe ack'ing. Under sustained CI load on
//     shared 2xlarge runners the upstream's default 5s-poll interval
//     plus normal container-start variance routinely needs ~10-20s for
//     a single pod. At 10s we saw two consecutive CI runs flake on
//     different scenarios at this exact step (MR !965 pipeline
//     2533822621); at 30s the budget gives 6 polls and is at the
//     documented upper bound for any test/conformance timeout — going
//     above 30s would be papering over slowness that warrants
//     investigation, but staying at 30s here just reflects realistic
//     scheduler+kubelet timing for a fresh-namespace pod under CI
//     contention, which is not haptic's bug.
const (
	waitForIngressStatus = 10 * time.Second
	waitForReady         = 30 * time.Second
)

// cucumberFeature mirrors the top-level structure godog writes for each
// `<feature>-report.json` file. The upstream binary uses godog v0.12; the
// schema is therefore frozen against our pinned commit. We only model the
// fields we actually inspect — extra keys are ignored by encoding/json.
type cucumberFeature struct {
	URI      string            `json:"uri"`
	Name     string            `json:"name"`
	Elements []cucumberElement `json:"elements"`
}

type cucumberElement struct {
	Name  string         `json:"name"`
	Type  string         `json:"type"`
	Steps []cucumberStep `json:"steps"`
}

type cucumberStep struct {
	Keyword string             `json:"keyword"`
	Name    string             `json:"name"`
	Result  cucumberStepResult `json:"result"`
}

type cucumberStepResult struct {
	Status       string `json:"status"`
	ErrorMessage string `json:"error_message"`
}

func TestIngressConformance(t *testing.T) {
	// KUBECONFIG must be provided by the caller. When run as a sibling
	// container via `make test-ingress-conformance`, the kubeconfig is
	// baked into the image at /etc/kubeconfig and KUBECONFIG is set on
	// the docker run command.
	require.NotEmpty(t, os.Getenv("KUBECONFIG"),
		"KUBECONFIG must point at the haptic-e2e cluster's kubeconfig")

	bin := upstreamBinary(t)
	workDir := stagedFeaturesDir(t)

	outDir := t.TempDir()

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()

	// Flag set matches the upstream's `conformance_test.go`:
	//   - `-format=cucumber`           machine-readable reports we parse
	//   - `-output-directory=<dir>`    where the reports land
	//   - `-ingress-class=haptic`      both the legacy annotation AND
	//                                   spec.ingressClassName on every
	//                                   Ingress the suite creates, so the
	//                                   chart's field-selector watch picks
	//                                   them up
	//   - `-wait-time-for-...=10s`     honors the reconcile-budget rule;
	//                                   upstream defaults are 5m
	//   - `-no-colors`                 cleaner CI logs (the cucumber JSON
	//                                   is what we parse; stdout is for
	//                                   humans/debug only)
	cmd := &process.Command{Name: bin, Args: []string{
		"-format=cucumber",
		"-output-directory=" + outDir,
		"-ingress-class=" + ingressClassName,
		"-wait-time-for-ingress-status=" + waitForIngressStatus.String(),
		"-wait-time-for-ready=" + waitForReady.String(),
		"-no-colors",
	}}
	cmd.Dir = workDir

	// Tee the binary's stdout/stderr into t.Log so a binary-level crash
	// (segfault, kubeconfig error, etc. — anything that prevents the
	// Cucumber JSON from being written) leaves a trail. The JSON parse
	// below is the source of truth for pass/fail; the streams are for
	// debugging when the JSON parse turns up nothing.
	//
	// One writer per stream — os/exec runs the stdout and stderr copy
	// loops in separate goroutines, so a shared testLogWriter would race
	// on its line-buffer. testing.T.Log is internally synchronized, so
	// two writers feeding the same *testing.T is safe.
	stdoutWriter := &testLogWriter{t: t}
	stderrWriter := &testLogWriter{t: t}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter

	t.Logf("running upstream binary: %s %s", bin, strings.Join(cmd.Args, " "))

	// Spawn a watcher on the cucumber-report tempdir so the CI log
	// shows feature-by-feature progress while the upstream binary is
	// running, instead of going silent for ~14 minutes between the
	// command line above and the final PASS. Cancel before the binary
	// exit completes so the watcher stops cleanly; one final scan()
	// after the cancel catches anything written between the last tick
	// and the exit.
	progCtx, progCancel := context.WithCancel(ctx)
	progress := newProgressReporter(t, outDir)
	progDone := make(chan struct{})
	go func() {
		defer close(progDone)
		progress.run(progCtx)
	}()

	_, runErr := (process.Executor{}).Run(ctx, cmd)
	progCancel()
	<-progDone
	progress.scan() // catch any reports written after the last tick

	// Flush any trailing partial line the binary left without a final
	// newline — common for crash messages, godog's summary, progress
	// indicators. testLogWriter only emits on `\n`, so anything in
	// the buffer at exit would otherwise be silently dropped exactly
	// when debugging value is highest.
	stdoutWriter.flush()
	stderrWriter.flush()
	// runErr != nil is expected whenever any scenario fails — godog
	// returns non-zero on test failure. We do NOT fail the test here;
	// we want every failing scenario to surface as a named subtest
	// instead. The only reason to surface runErr is if it indicates a
	// genuine binary-level failure (no JSON written), which we detect
	// below from the empty-report case.

	reports, err := filepath.Glob(filepath.Join(outDir, "*-report.json"))
	require.NoError(t, err, "glob cucumber report files")
	if len(reports) == 0 {
		require.Failf(t, "no Cucumber JSON reports produced",
			"upstream binary wrote no `*-report.json` under %s — "+
				"likely a binary-level crash before any scenario ran "+
				"(binary exit: %v). Check the streamed output above for "+
				"the underlying cause.", outDir, runErr)
	}

	for _, reportPath := range reports {
		reportScenarios(t, reportPath)
	}
}

// upstreamBinary returns the upstream ingress-controller-conformance binary.
func upstreamBinary(t *testing.T) string {
	t.Helper()
	bin := upstreamBinaryPath
	if override := os.Getenv("INGRESS_CONFORMANCE_BIN"); override != "" {
		bin = override
	}
	_, err := os.Stat(bin)
	require.NoErrorf(t, err,
		"upstream ingress-controller-conformance binary not found at %s — "+
			"the Makefile target should have built it; set "+
			"$INGRESS_CONFORMANCE_BIN to override", bin)
	return bin
}

// stagedFeaturesDir returns the directory the upstream binary runs from: its
// features/ subdirectory holds this shard's features.
func stagedFeaturesDir(t *testing.T) string {
	t.Helper()
	workDir := upstreamFeaturesDir
	if override := os.Getenv("INGRESS_CONFORMANCE_FEATURES_DIR"); override != "" {
		workDir = override
	}
	// Sharded runs (SHARD_ID + SHARD_COUNT set by GitLab CI's `parallel:`
	// keyword) generate a per-shard `features/` dir at runtime from the
	// baked-in one and point the binary at it. See prepareShardedFeatures
	// for the assignment strategy. We do this BEFORE the directory-exists
	// check below so that check validates the shard's view, not the
	// (unsharded) source.
	shardID, shardCount, sharded, err := shardEnv()
	require.NoError(t, err, "parse SHARD_ID/SHARD_COUNT env")
	if sharded {
		shardDir := t.TempDir()
		err := prepareShardedFeatures(workDir, shardDir, shardID, shardCount)
		require.NoErrorf(t, err, "prepare shard %d/%d features", shardID, shardCount)
		t.Logf("[conformance] shard %d/%d: rewrote features into %s", shardID, shardCount, shardDir)
		workDir = shardDir
	}
	// The upstream binary's feature loader uses paths like
	// `features/default_backend.feature` resolved relative to its CWD.
	// Verify the directory we'd run it from actually has those features
	// staged, so a misconfigured image fails with a clearer message than
	// "no features found".
	featuresPath := filepath.Join(workDir, "features")
	entries, err := os.ReadDir(featuresPath)
	require.NoErrorf(t, err,
		"upstream features directory %s is missing — "+
			"Dockerfile.ingress-conformance-test must COPY the upstream "+
			"features/ tree into the image", featuresPath)
	require.NotEmptyf(t, entries,
		"upstream features directory %s exists but is empty", featuresPath)
	return workDir
}

// reportScenarios turns one Cucumber JSON report into a subtest per feature
// and scenario, failing each scenario on its first step that did not pass.
func reportScenarios(t *testing.T, reportPath string) {
	t.Helper()
	raw, err := os.ReadFile(reportPath)
	require.NoErrorf(t, err, "read %s", reportPath)

	var features []cucumberFeature
	require.NoErrorf(t, json.Unmarshal(raw, &features),
		"parse %s as Cucumber JSON", reportPath)

	for _, feature := range features {
		t.Run(sanitize(feature.Name), func(t *testing.T) {
			for _, element := range feature.Elements {
				t.Run(sanitize(element.Name), func(t *testing.T) {
					for _, step := range element.Steps {
						if step.Result.Status == "passed" {
							continue
						}
						t.Fatalf(
							"step %q (%s): %s",
							strings.TrimSpace(step.Keyword)+" "+step.Name,
							step.Result.Status,
							step.Result.ErrorMessage,
						)
					}
				})
			}
		})
	}
}

// sanitize converts a Cucumber feature / scenario name into a token that
// `go test -test.run` regexes can address. Spaces and punctuation become
// underscores; repeated underscores collapse; the result is trimmed.
var nonRunSafe = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func sanitize(s string) string {
	if s == "" {
		return "unnamed"
	}
	s = nonRunSafe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "unnamed"
	}
	return s
}

// testLogWriter routes the upstream binary's stdout/stderr into t.Log so
// the streamed output stays anchored to the running test in `go test -v`
// output (and in GitLab's job log) and gets the standard "    " indent.
// Buffered to whole lines so we don't fragment log entries on partial
// writes. Lines matching noisySkipLine are silently dropped — see that
// regex for the rationale.
type testLogWriter struct {
	t   *testing.T
	buf strings.Builder
}

// noisySkipLine matches output lines that fire repeatedly and add no
// debug value. Today the only entry is the k8s v1.33+ deprecation
// header that client-go logs every time the upstream binary polls a
// `v1 Endpoints` object — and the upstream polls it constantly, so a
// full run drops ~150 identical W-level lines into the log:
//
//	W0521 22:06:17.537610      12 warnings.go:67] v1 Endpoints is deprecated
//	  in v1.33+; use discovery.k8s.io/v1 EndpointSlice
//
// The upstream binary is pinned to a dormant repo (no v1 EndpointSlice
// migration coming) so the warning will recur on every run for the
// foreseeable future. Silencing here keeps the GitLab job log readable
// without touching the upstream itself.
var noisySkipLine = regexp.MustCompile(`v1 Endpoints is deprecated in v1\.\d+\+`)

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	s := w.buf.String()
	for {
		nl := strings.IndexByte(s, '\n')
		if nl < 0 {
			break
		}
		line := s[:nl]
		s = s[nl+1:]
		if noisySkipLine.MatchString(line) {
			continue
		}
		w.t.Log(line)
	}
	// Preserve any trailing partial line for the next write.
	w.buf.Reset()
	if s != "" {
		w.buf.WriteString(s)
	}
	return len(p), nil
}

// flush emits any trailing partial line (no terminating newline) and
// resets the buffer. Call after the writer's source closes so the last
// line of binary output isn't silently dropped — typically the most
// useful line when debugging a crash.
func (w *testLogWriter) flush() {
	if w.buf.Len() == 0 {
		return
	}
	w.t.Log(w.buf.String())
	w.buf.Reset()
}

// progressReporter polls the godog cucumber-report output directory and
// emits a t.Logf line every time a new `*-report.json` file appears
// (one per feature, written by godog when the feature finishes). The
// upstream binary itself is silent during execution — no per-feature
// progress on stdout, no per-scenario marker. Without this watcher the
// GitLab job log shows ~14 minutes of nothing-but-our-Logf for the
// initial command line and the periodic v1-Endpoints warnings
// (themselves now suppressed), then a single PASS at the very end.
// With the watcher, each feature reports as it completes:
//
//	[conformance] feature "Default backend" passed (134s, 6 scenarios)
//
// so a CI log reader can see real progress AND spot which feature was
// in flight when the suite hangs or stalls.
type progressReporter struct {
	t       *testing.T
	dir     string
	startAt time.Time

	mu   sync.Mutex
	seen map[string]bool
}

func newProgressReporter(t *testing.T, dir string) *progressReporter {
	t.Helper()
	return &progressReporter{
		t:       t,
		dir:     dir,
		startAt: time.Now(),
		seen:    map[string]bool{},
	}
}

// run polls the directory until ctx is cancelled. Each new
// `*-report.json` file emits one Logf line. Designed for `go func()`
// invocation — it returns when ctx is done.
//
// Poll interval is 2s: small enough that the lag between a feature
// finishing and our Logf emitting is short (single-digit seconds),
// large enough that the watcher itself doesn't dominate the runtime
// load on a tiny tempdir. The upstream binary writes the JSON to the
// final filename atomically via bufio.Writer.Flush in `defer`, so
// we won't see partial files.
func (r *progressReporter) run(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		r.scan()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (r *progressReporter) scan() {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		// Tempdir might not exist yet on the first tick; ignore.
		return
	}
	// Sort so the order matches what the upstream binary writes:
	// alphabetical by feature filename, which is also roughly the
	// order it processes them in.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, "-report.json") {
			continue
		}
		r.mu.Lock()
		if r.seen[name] {
			r.mu.Unlock()
			continue
		}
		r.mu.Unlock()
		// Only mark "seen" once emit succeeds — godog's testFeature
		// creates the file at the start of the feature run but content
		// is buffered through a bufio.NewWriter that only Flush()es
		// on defer. So the file exists in the dir well before its
		// content is parseable; an early read race would log
		// "parse failed: unexpected end of JSON input" and then never
		// re-try because `seen` was set unconditionally. Re-poll until
		// the parse succeeds — the file's still there, godog will Flush
		// when the feature finishes.
		if r.emit(name) {
			r.mu.Lock()
			r.seen[name] = true
			r.mu.Unlock()
		}
	}
}

// emit reads one report and Logf's a one-line summary. We parse the
// JSON here rather than waiting for the post-run pass — gives the
// log line a useful pass/fail count instead of just the filename.
// Returns true once the file has been parsed and reported; returns
// false on any read/parse failure so scan() keeps polling (the file
// is racy with godog's bufio.Writer — see the comment in scan()).
func (r *progressReporter) emit(filename string) bool {
	raw, err := os.ReadFile(filepath.Join(r.dir, filename))
	if err != nil {
		// File appeared in the directory listing but a read failed —
		// almost certainly because the upstream binary is mid-write.
		// Stay silent and let the next poll retry; this is the
		// expected path during normal operation.
		return false
	}
	var features []cucumberFeature
	if err := json.Unmarshal(raw, &features); err != nil {
		// Partial JSON — godog's bufio.Writer hasn't flushed yet.
		// Same story as the read-fail case; retry on the next tick.
		return false
	}
	elapsed := time.Since(r.startAt).Round(time.Second)
	for _, f := range features {
		passed, failed := 0, 0
		for _, el := range f.Elements {
			scenarioFailed := false
			for _, s := range el.Steps {
				if s.Result.Status != "passed" {
					scenarioFailed = true
					break
				}
			}
			if scenarioFailed {
				failed++
			} else {
				passed++
			}
		}
		status := fmt.Sprintf("passed (%d scenarios)", passed)
		if failed > 0 {
			status = fmt.Sprintf("FAILED (%d/%d scenarios)", failed, passed+failed)
		}
		r.t.Logf("[conformance] feature %q %s, t+%s", f.Name, status, elapsed)
	}
	return true
}
