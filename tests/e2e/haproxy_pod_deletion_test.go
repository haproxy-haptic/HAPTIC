//go:build e2e

package e2e

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

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
