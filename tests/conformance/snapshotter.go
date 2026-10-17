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

//go:build gateway_conformance

package conformance

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/gateway-api/conformance/utils/roundtripper"
	"sigs.k8s.io/yaml"
)

// snapshotNamespace is where the snapshotter dumps the captured
// HAProxy state. The chart's controller deployment lives here, so
// the CI after_script already has it in its working set.
const snapshotNamespace = "haptic"

// testNameKey labels and annotates every snapshot ConfigMap with the test that captured it.
const testNameKey = "haptic.io/test-name"

// haproxyCfgGVR identifies the chart-published HAProxyCfg CRD —
// the controller's RENDERED config (what the chart produced before
// the dataplane API translated it into incremental ops). Capturing
// this alongside the running pod's /etc/haproxy/haproxy.cfg lets a
// debugger distinguish "chart rendered wrong output" from
// "dataplane sync applied ops in wrong order".
var haproxyCfgGVR = schema.GroupVersionResource{
	Group:    "haproxy-haptic.org",
	Version:  "v1alpha1",
	Resource: "haproxycfgs",
}

// snapshottingRoundTripper wraps the upstream conformance
// RoundTripper. Whenever a request errors out (TLS handshake
// rejected, connection refused, timeout, …) it dumps the running
// HAProxy pod's /etc/haproxy tree into a ConfigMap labelled for CI
// pickup. The dump fires at the exact moment of failure, while the
// upstream test framework's t.Cleanup() — which deletes the
// conformance fixtures (Gateways, TLSRoutes, …) — hasn't run yet,
// so the snapshot reflects the state HAProxy was actually serving
// when the request failed.
//
// Throttling: at most one snapshot per test name per
// throttleWindow. The conformance tests retry a failing TLS request
// once per second for 10 seconds, so without throttling each
// failing subtest would create 10 near-identical ConfigMaps.
type snapshottingRoundTripper struct {
	inner   roundtripper.RoundTripper
	cs      clientset.Interface
	dyn     dynamic.Interface
	restCfg *rest.Config

	mu                  sync.Mutex
	lastSnapshotAt      map[string]time.Time
	throttleWindow      time.Duration
	snapshotsPerTest    map[string]int
	maxSnapshotsPerTest int

	// firstCallAt tracks when each test made its first
	// CaptureRoundTrip call, so we can detect a poll-loop
	// failure (the test keeps trying for MaxTimeToConsistency
	// without ever getting an error from the request itself —
	// the response succeeds but doesn't match the expected
	// backend). That failure mode never returns err != nil
	// from CaptureRoundTrip, so the err-driven snapshot path
	// alone misses it.
	firstCallAt map[string]time.Time
	// callCount counts CaptureRoundTrip calls per test name.
	// Used together with firstCallAt to detect the poll-loop
	// pattern: more than `pollSnapshotMinCalls` calls AND more
	// than `pollSnapshotMinElapsed` since the first call
	// suggests the test is stuck retrying.
	callCount              map[string]int
	pollSnapshotMinCalls   int
	pollSnapshotMinElapsed time.Duration

	networkGate *networkCaptureGate
}

// newSnapshottingRoundTripper wraps inner with on-error HAProxy
// state capture. The clientset is used to list HAProxy pods and
// exec into them; the dynamic client reads the chart-published
// HAProxyCfg CRD; rest.Config drives the exec stream. All three
// are required.
func newSnapshottingRoundTripper(
	inner roundtripper.RoundTripper,
	cs clientset.Interface,
	dyn dynamic.Interface,
	restCfg *rest.Config,
) *snapshottingRoundTripper {
	return &snapshottingRoundTripper{
		inner:                  inner,
		cs:                     cs,
		dyn:                    dyn,
		restCfg:                restCfg,
		lastSnapshotAt:         map[string]time.Time{},
		snapshotsPerTest:       map[string]int{},
		firstCallAt:            map[string]time.Time{},
		callCount:              map[string]int{},
		throttleWindow:         2 * time.Second,
		maxSnapshotsPerTest:    3,
		pollSnapshotMinCalls:   3,
		pollSnapshotMinElapsed: 3 * time.Second,
		networkGate:            newNetworkCaptureGate(),
	}
}

// CaptureRoundTrip delegates to the inner RoundTripper and
// asynchronously dumps HAProxy state on signs of failure. Two
// triggers fire snapshots:
//
//  1. Connection-level error from the inner RoundTripper
//     (TLS handshake rejected, dial failure, request timeout).
//     Covers the TLSRoute / connection-rejected failure mode.
//
//  2. Test stuck in a poll loop: more than pollSnapshotMinCalls
//     CaptureRoundTrip calls AND more than pollSnapshotMinElapsed
//     elapsed since the first call for this test name. Covers the
//     HTTP failure mode where requests succeed but bodies don't
//     match the expected backend, causing the upstream conformance
//     suite to retry until MaxTimeToConsistency expires (10s).
//
// Both paths share the per-test throttle (≤ maxSnapshotsPerTest,
// ≤ 1 / throttleWindow). A network-layer error that persists also
// captures VIP state under networkGate (see captureNetwork).
// The snapshot itself is fire-and-forget
// so exec latency can't extend the test's own backoff.
func (s *snapshottingRoundTripper) CaptureRoundTrip(req roundtripper.Request) (*roundtripper.CapturedRequest, *roundtripper.CapturedResponse, error) {
	testName := ""
	if req.T != nil {
		testName = req.T.Name()
		s.recordCall(testName)
	}

	cReq, cRes, err := s.inner.CaptureRoundTrip(req)

	if testName == "" {
		return cReq, cRes, err
	}

	var reason string
	switch {
	case err != nil:
		reason = fmt.Sprintf("roundtrip-error: %v", err)
	case s.inPollLoop(testName):
		reason = "poll-loop: test stuck retrying despite successful round-trips"
	default:
		return cReq, cRes, err
	}

	if s.shouldSnapshot(testName) {
		go s.snapshot(testName, req.Host, req.URL.Host, req.ServerName, reason)
	}
	if ip, ok := dialedIP(req.URL.Host); ok && isNetworkLayerError(err) {
		if capture := s.admitNetworkCapture(testName, ip); capture > 0 {
			go s.captureNetwork(testName, ip, capture, reason)
		}
	}
	return cReq, cRes, err
}

func (s *snapshottingRoundTripper) admitNetworkCapture(testName string, ip netip.Addr) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.networkGate.admit(testName+"|"+ip.String(), time.Now())
}

// recordCall tracks the per-test call count and first-call time
// so inPollLoop can recognise the "stuck retrying" pattern.
func (s *snapshottingRoundTripper) recordCall(testName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.firstCallAt[testName]; !ok {
		s.firstCallAt[testName] = time.Now()
	}
	s.callCount[testName]++
}

// inPollLoop reports whether the named test looks like it's stuck
// in the upstream conformance suite's retry-until-consistent loop:
// many CaptureRoundTrip calls over a non-trivial window. A successful
// test typically completes in 1-2 calls; a failing one keeps hitting
// the RoundTripper for the full MaxTimeToConsistency budget.
func (s *snapshottingRoundTripper) inPollLoop(testName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	first, ok := s.firstCallAt[testName]
	if !ok {
		return false
	}
	return s.callCount[testName] >= s.pollSnapshotMinCalls &&
		time.Since(first) >= s.pollSnapshotMinElapsed
}

// shouldSnapshot enforces the per-test throttle. Returns true at
// most maxSnapshotsPerTest times per t.Name() and never more than
// once per throttleWindow.
func (s *snapshottingRoundTripper) shouldSnapshot(testName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshotsPerTest[testName] >= s.maxSnapshotsPerTest {
		return false
	}
	if last, ok := s.lastSnapshotAt[testName]; ok && time.Since(last) < s.throttleWindow {
		return false
	}
	s.lastSnapshotAt[testName] = time.Now()
	s.snapshotsPerTest[testName]++
	return true
}

// snapshot lists HAProxy pods, tars /etc/haproxy from the first
// ready one, fetches the chart-published HAProxyCfg CRDs, and
// stores both in a ConfigMap with metadata describing the failing
// request. Errors are swallowed — a best-effort capture beats
// interrupting the test on telemetry failure.
//
// The two payloads serve different debug purposes:
//
//   - etc-haproxy.tar.gz: the dataplane API's actual on-disk
//     config (haproxy.cfg + maps/ + ssl/ + general/) — what
//     HAProxy is serving at the moment of failure.
//   - haproxycfg-crds.yaml: the controller's RENDERED config,
//     as published to the HAProxyCfg CRD before the dataplane
//     API translated it into incremental ops. Comparing the two
//     pins whether a missing/wrong rule was the chart's fault
//     or the dataplane sync's fault.
func (s *snapshottingRoundTripper) snapshot(testName, host, urlHost, sni, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pod, err := s.firstReadyHAProxyPod(ctx)
	if err != nil {
		return
	}

	// `tar -cz` writes a gzip-compressed tarball of the chart's
	// HAProxy state. /etc/haproxy holds haproxy.cfg, maps/,
	// general/ (errorfiles + crt-lists), and ssl/ (certs) — every
	// file the controller manages through the dataplane API.
	tarball, err := s.exec(ctx, snapshotNamespace, pod, "haproxy", []string{"tar", "-czf", "-", "-C", "/etc/haproxy", "."})
	if err != nil {
		return
	}

	// HAProxyCfg CRD dump — best-effort; an error here doesn't
	// invalidate the rest of the snapshot.
	crdYAML, crdErr := s.dumpHAProxyCfgs(ctx)
	if crdErr != nil {
		crdYAML = []byte(fmt.Sprintf("# haproxycfg dump failed: %v\n", crdErr))
	}

	cmName := snapshotConfigMapName("conformance-snapshot-", testName)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: snapshotNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/component": "conformance-snapshot",
				testNameKey:                   sanitizeForLabel(testName),
			},
			Annotations: map[string]string{
				testNameKey:                  testName,
				"haptic.io/timestamp":        time.Now().UTC().Format(time.RFC3339Nano),
				"haptic.io/reason":           reason,
				"haptic.io/request-host":     host,
				"haptic.io/url-host":         urlHost,
				"haptic.io/request-sni":      sni,
				"haptic.io/source-pod":       pod,
				"haptic.io/tarball-bytes":    fmt.Sprintf("%d", len(tarball)),
				"haptic.io/haproxycfg-bytes": fmt.Sprintf("%d", len(crdYAML)),
			},
		},
		BinaryData: map[string][]byte{
			// Tarball of /etc/haproxy on the pod — the dataplane
			// API's current on-disk state.
			"etc-haproxy.tar.gz": tarball,
			// YAML dump of every HAProxyCfg CR cluster-wide — the
			// chart's RENDERED config. binaryData rather than data
			// because the CRD's spec.content may contain bytes that
			// don't survive a Go string -> kubernetes string round
			// trip (the chart compresses with zstd+base64 when the
			// payload exceeds a threshold; the YAML wrapper handles
			// both the compressed and plaintext shapes uniformly).
			"haproxycfg-crds.yaml": crdYAML,
		},
	}

	// Tolerate AlreadyExists — within a single test multiple
	// snapshot attempts collide on the same suffix when the
	// retry loop fires fast enough to race the throttle. Update
	// in place so the latest payload wins.
	_, err = s.cs.CoreV1().ConfigMaps(snapshotNamespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Read existing CM for the resourceVersion, then Update.
		existing, getErr := s.cs.CoreV1().ConfigMaps(snapshotNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if getErr == nil {
			cm.ResourceVersion = existing.ResourceVersion
			_, _ = s.cs.CoreV1().ConfigMaps(snapshotNamespace).Update(ctx, cm, metav1.UpdateOptions{})
		}
	}
}

// dumpHAProxyCfgs returns a YAML rendering of every HAProxyCfg
// CR cluster-wide. The chart's configpublisher writes the
// rendered config here as a strongly-typed CR (with optional
// zstd+base64 compression of the haproxy.cfg payload), so this
// captures what the controller PRODUCED — independent of what
// the dataplane API later applied to the running pods.
func (s *snapshottingRoundTripper) dumpHAProxyCfgs(ctx context.Context) ([]byte, error) {
	list, err := s.dyn.Resource(haproxyCfgGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing HAProxyCfg: %w", err)
	}
	out, err := yaml.Marshal(list)
	if err != nil {
		return nil, fmt.Errorf("marshalling HAProxyCfg list: %w", err)
	}
	return out, nil
}

// firstReadyHAProxyPod returns the name of the first HAProxy pod
// in Ready state. The chart's loadbalancer pods carry the
// `app.kubernetes.io/component=loadbalancer` label.
func (s *snapshottingRoundTripper) firstReadyHAProxyPod(ctx context.Context) (string, error) {
	pods, err := s.cs.CoreV1().Pods(snapshotNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=loadbalancer",
	})
	if err != nil {
		return "", err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if !isPodReady(p) {
			continue
		}
		return p.Name, nil
	}
	return "", fmt.Errorf("no ready HAProxy pod in namespace %q", snapshotNamespace)
}

// isPodReady reports whether the pod's Ready condition is True.
// Snapshotting from a NotReady pod risks reading a half-applied
// config (the controller stops syncing pods that aren't Ready,
// and the dataplane API may be flapping), so we skip them.
func isPodReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// exec runs command in a pod container and returns its stdout; the
// test container has no kubectl.
func (s *snapshottingRoundTripper) exec(ctx context.Context, namespace, podName, container string, command []string) ([]byte, error) {
	req := s.cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(s.restCfg, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("creating SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// snapshotConfigMapName turns a Go test name into a DNS-1123 RFC
// 1123 subdomain (the constraint Kubernetes ConfigMap names must
// satisfy). Test names contain `/` and uppercase letters that the
// API server rejects.
//
// After truncation we strip any trailing `-` or `.` left at the new
// boundary — DNS-1123 names must end in an alphanumeric — so a
// pathological test name (~230+ chars ending in a `/` that
// sanitizeForName converted to `-`) doesn't make the API server
// reject the ConfigMap. Matches the same trim sanitizeForLabel
// applies for the label-value alphabet.
func snapshotConfigMapName(prefix, testName string) string {
	sanitized := sanitizeForName(testName)
	const maxNameLen = 253
	if len(prefix)+len(sanitized) > maxNameLen {
		sanitized = sanitized[:maxNameLen-len(prefix)]
		sanitized = strings.TrimRight(sanitized, "-.")
	}
	return prefix + sanitized
}

// labelValuePattern matches characters Kubernetes label values
// allow: alphanumerics plus `-`, `_`, `.` (plus length and
// boundary constraints handled separately).
var labelValuePattern = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// sanitizeForLabel collapses any character outside the label-value
// alphabet to `-`, truncates to 63 chars (the K8s label-value
// limit), and trims leading/trailing punctuation. Used for the
// `haptic.io/test-name` label that operators filter by.
func sanitizeForLabel(s string) string {
	s = labelValuePattern.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-._")
	const maxLabelValueLength = 63
	if len(s) > maxLabelValueLength {
		s = s[:maxLabelValueLength]
		s = strings.TrimRight(s, "-._")
	}
	return s
}

// nameValuePattern matches characters DNS-1123 subdomain names
// allow: lowercase alphanumerics plus `-` and `.`.
var nameValuePattern = regexp.MustCompile(`[^a-z0-9.-]`)

// sanitizeForName lowercases the input and collapses non-DNS-1123
// characters to `-`. Used to derive a ConfigMap name from a Go
// test name.
func sanitizeForName(s string) string {
	s = strings.ToLower(s)
	s = nameValuePattern.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	return s
}
