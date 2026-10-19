//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/e2ecluster"
	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// Deleting a pod runs the chart's drain hook and then HAProxy's own soft stop:
// the agent holds the hook until no new connection reaches the pod, the pod's
// listeners stay open for that time, a request in flight on the deleted pod
// completes, and the probe stream through the Service stays clean. kube-proxy
// in kind drops the endpoint faster than the probe can catch, so the
// endpoint-deprogramming race itself does not reproduce here. Serial: it takes
// a replica out of the fleet and waits for the fleet to settle before returning.
func TestHAProxyPodDeletionZeroDowntime(t *testing.T) {
	const host = "haproxy-pod-deletion.localdev.me"
	feature := features.New("HAProxy: deleting a pod drains and soft-stops").
		Assess("the drain holds the listeners, the request in flight completes and the probe stream stays clean", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			clientset, err := newClientsetForE2E(client.RESTConfig())
			require.NoError(t, err)
			namespace := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, namespace)
			backend := NewEchoServerBackend(ctx, t, client, namespace)
			NewIngress(ctx, t, client, namespace, &IngressSpec{
				Name: "echo", Host: host, BackendService: backend.Service, BackendPort: backend.Port,
			})
			httpclient.New(t).GET(host, "/").ExpectOK(t)

			pods := listHAProxyPods(t)
			require.GreaterOrEqual(t, len(pods), 2)
			victim := pods[0]
			victimPod, err := clientset.CoreV1().Pods(ControllerNamespace).Get(ctx, victim, metav1.GetOptions{})
			require.NoError(t, err)
			require.NotEmpty(t, victimPod.Status.PodIP)

			proberCtx, stopProber := context.WithCancel(ctx)
			results := &probeRecorder{snapshotter: newProberSnapshotter(t, namespace)}
			var probeWG sync.WaitGroup
			probeWG.Add(1)
			go func() {
				defer probeWG.Done()
				runProbeLoopEvery(proberCtx, t, host, results, 20*time.Millisecond)
			}()
			finish := func() {
				stopProber()
				probeWG.Wait()
				results.waitForSnapshots()
			}
			t.Cleanup(finish)

			time.Sleep(2 * time.Second)
			inFlight := holdRequestOnPod(ctx, pods[1], victimPod.Status.PodIP, host)
			deleted := time.Now()
			require.NoError(t, clientset.CoreV1().Pods(ControllerNamespace).Delete(ctx, victim, metav1.DeleteOptions{}))
			served := servedAfterDeletion(ctx, victim, deleted)
			// The chart's quiet period is 2 s: with nothing routed to the pod any
			// more, the drain hook holds the listeners for at least that long.
			require.GreaterOrEqualf(t, served, 1500*time.Millisecond,
				"%s stopped answering %s after its deletion; the drain hook must hold its listeners open", victim, served)
			select {
			case result := <-inFlight:
				require.NoErrorf(t, result.err, "the request in flight on %s during its deletion was cut (%s)", victim, result.status)
				require.Equalf(t, "200", result.status, "the request in flight on %s during its deletion did not complete", victim)
			case <-time.After(40 * time.Second):
				t.Fatalf("the request in flight on %s during its deletion never returned", victim)
			}
			waitForPodGone(ctx, t, clientset, victim)
			requireNoFailedPreStopHook(ctx, t, clientset, victim)
			require.NoError(t, waitForDeploymentRolloutComplete(ctx, client, ControllerNamespace, HAProxyDeploymentName, 2*time.Minute))
			settled := waitForQuietFleet(ctx, t, clientset)
			require.Len(t, settled, len(pods))
			require.NotContains(t, settled, victim)
			// The replacement pod joined and the fleet settled; keep probing a little
			// longer so a late endpoint change would still be seen.
			time.Sleep(5 * time.Second)
			finish()

			failures := results.snapshotFailures()
			require.Emptyf(t, failures, "%d of %d probes failed across the deletion of %s: %+v", len(failures), results.count(), victim, failures)
			require.Positive(t, results.count())
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}

// A client that pauses past the quiet period must not end the drain while
// kube-proxy still routes to the deleted pod (#446). Freezing kube-proxy keeps
// the pod's forwarding rules in place: the pod must keep its listeners, serve
// every new connection the frozen rules still send it, and end its drain only
// once the resumed kube-proxy has synced the removal. Serial: it stops
// kube-proxy on every node and takes a replica out of the fleet.
func TestHAProxyPodDeletionWaitsForKubeProxy(t *testing.T) {
	const host = "haproxy-drain-routing.localdev.me"
	feature := features.New("HAProxy: the drain waits for kube-proxy, not for a quiet client").
		Assess("the deleted pod serves until kube-proxy synced its removal", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			clientset, err := newClientsetForE2E(client.RESTConfig())
			require.NoError(t, err)
			namespace := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, namespace)
			backend := NewEchoServerBackend(ctx, t, client, namespace)
			NewIngress(ctx, t, client, namespace, &IngressSpec{
				Name: "echo", Host: host, BackendService: backend.Service, BackendPort: backend.Port,
			})
			httpclient.New(t).GET(host, "/").ExpectOK(t)
			endpoint, err := e2ecluster.ResolveTrafficEndpoint()
			require.NoError(t, err)

			pods := listHAProxyPods(t)
			require.GreaterOrEqual(t, len(pods), 2)
			victim := pods[0]
			drained := followDrainResult(ctx, t, clientset, victim)

			thaw := freezeKubeProxy(ctx, t, clientset)
			require.NoError(t, clientset.CoreV1().Pods(ControllerNamespace).Delete(ctx, victim, metav1.DeleteOptions{}))
			// No client connects for longer than the 2 s quiet period.
			time.Sleep(3 * time.Second)
			statsCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err = apiProxyGet(statsCtx, victim, HAProxyStatsPort, "metrics")
			cancel()
			require.NoErrorf(t, err, "%s closed its listeners while kube-proxy still routed to it", victim)
			// kube-proxy's frozen rules send about half of these to the victim.
			for i := range 20 {
				require.NoErrorf(t, freshConnectionGET(ctx, endpoint, host),
					"request %d after the client pause was refused while kube-proxy still routed to %s", i, victim)
			}
			thaw()

			select {
			case result := <-drained:
				require.Equalf(t, "routed-away", result.Reason,
					"the drain of %s must end on kube-proxy's sync, not on the quiet period or the bound: %+v", victim, result)
			case <-time.After(40 * time.Second):
				t.Fatalf("%s never reported the end of its drain", victim)
			}
			waitForPodGone(ctx, t, clientset, victim)
			requireNoFailedPreStopHook(ctx, t, clientset, victim)
			require.NoError(t, waitForDeploymentRolloutComplete(ctx, client, ControllerNamespace, HAProxyDeploymentName, 2*time.Minute))
			settled := waitForQuietFleet(ctx, t, clientset)
			require.Len(t, settled, len(pods))
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}

// freezeKubeProxy stops kube-proxy on every kind node, leaving its rules in
// place. The returned thaw, also run at cleanup, resumes it.
func freezeKubeProxy(ctx context.Context, t *testing.T, cs kubernetes.Interface) func() {
	t.Helper()
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	signal := func(sig string) error {
		for i := range nodes.Items {
			out, err := exec.CommandContext(context.WithoutCancel(ctx), "docker", "exec", nodes.Items[i].Name, "pkill", "-"+sig, "-x", "kube-proxy").CombinedOutput()
			if err != nil {
				return fmt.Errorf("pkill -%s kube-proxy on %s: %w: %s", sig, nodes.Items[i].Name, err, out)
			}
		}
		return nil
	}
	var once sync.Once
	thaw := func() {
		once.Do(func() { require.NoError(t, signal("CONT")) })
	}
	t.Cleanup(thaw)
	require.NoError(t, signal("STOP"))
	return thaw
}

// freshConnectionGET sends one request on a new connection, so kube-proxy
// chooses an endpoint for it.
func freshConnectionGET(ctx context.Context, endpoint e2ecluster.TrafficEndpoint, host string) error {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	url := "http://" + net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.HTTPPort)) + "/"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	req.Host = host
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

type drainResult struct {
	Reason  string `json:"reason"`
	Waited  string `json:"waited"`
	Routing string `json:"routing"`
}

// followDrainResult streams the pod's agent log and delivers its drain verdict;
// the log is gone once the pod is.
func followDrainResult(ctx context.Context, t *testing.T, cs kubernetes.Interface, pod string) <-chan drainResult {
	t.Helper()
	stream, err := cs.CoreV1().Pods(ControllerNamespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: "agent", Follow: true, SinceTime: &metav1.Time{Time: time.Now()},
	}).Stream(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	result := make(chan drainResult, 1)
	go func() {
		scanner := bufio.NewScanner(stream)
		for scanner.Scan() {
			var line struct {
				Msg string `json:"msg"`
				drainResult
			}
			if json.Unmarshal(scanner.Bytes(), &line) == nil && line.Msg == "drain finished" {
				result <- line.drainResult
				return
			}
		}
	}()
	return result
}

type heldRequest struct {
	status string
	err    error
}

// Run the client in a surviving pod so container shutdown cannot kill it.
func holdRequestOnPod(ctx context.Context, clientPod, destinationIP, host string) <-chan heldRequest {
	done := make(chan heldRequest, 1)
	go func() {
		out, err := execInHAProxyPod(ctx, clientPod, "haproxy", "curl",
			"-sS", "--connect-timeout", "2", "--max-time", "30",
			"-o", "/dev/null", "-w", "%{http_code}",
			"-H", "Host: "+host, "http://"+net.JoinHostPort(destinationIP, "80")+"/?echo_time=8000")
		done <- heldRequest{status: strings.TrimSpace(out), err: err}
	}()
	time.Sleep(time.Second)
	return done
}

// requireNoFailedPreStopHook fails when kubelet recorded that the drain hook
// failed or timed out on the deleted pod.
func requireNoFailedPreStopHook(ctx context.Context, t *testing.T, cs kubernetes.Interface, pod string) {
	t.Helper()
	events, err := cs.CoreV1().Events(ControllerNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: fields.AndSelectors(
			fields.OneTermEqualSelector("involvedObject.name", pod),
			fields.OneTermEqualSelector("reason", "FailedPreStopHook"),
		).String(),
	})
	require.NoError(t, err)
	for i := range events.Items {
		t.Errorf("the drain hook failed on %s: %s", pod, events.Items[i].Message)
	}
}

// servedAfterDeletion polls the deleted pod's own stats port and returns how
// long it kept answering, up to the drain bound. The stats frontend is not
// traffic for the drain, so this polling does not extend it.
func servedAfterDeletion(ctx context.Context, pod string, deleted time.Time) time.Duration {
	for time.Since(deleted) < 12*time.Second {
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := apiProxyGet(callCtx, pod, HAProxyStatsPort, "metrics")
		cancel()
		if err != nil {
			return time.Since(deleted)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return time.Since(deleted)
}

// waitForPodGone returns once the pod no longer exists; its termination, preStop
// included, is the window the probe must survive.
func waitForPodGone(ctx context.Context, t *testing.T, cs kubernetes.Interface, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := cs.CoreV1().Pods(ControllerNamespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		require.NoError(t, err)
		require.False(t, time.Now().After(deadline), "pod %s is still present", name)
		time.Sleep(500 * time.Millisecond)
	}
}
