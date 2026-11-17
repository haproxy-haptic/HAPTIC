// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	hapticclient "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
	sharedhttpclient "github.com/haproxy-haptic/HAPTIC/tests/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

// TestTLSSessionTicketKeysStaySecret enables the shared session-ticket keys and
// checks that they reach every HAProxy pod only through a Secret (#674), and
// that a leadership handover keeps them, so a ticket issued before it resumes
// after it.
func TestTLSSessionTicketKeysStaySecret(t *testing.T) {
	var (
		clientset kubernetes.Interface
		host      string
		keys      string
	)
	feature := features.New("TLS session-ticket keys are published only as a Secret").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			clientset, err = newClientsetForE2E(client.RESTConfig())
			require.NoError(t, err)
			namespace := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, namespace)
			host = "session-tickets." + namespace + ".localdev.me"
			echo := NewEchoServerBackend(ctx, t, client, namespace)
			NewIngress(ctx, t, client, namespace, &IngressSpec{
				Name: "tickets", Host: host, Path: "/", BackendService: echo.Service, BackendPort: echo.Port,
				TLSSecretName: NewTLSSecret(ctx, t, client, namespace, "tickets-tls", []string{host}),
			})
			waitForIngressDeployed(ctx, t, client, namespace, "tickets")
			enableSessionTicketsDuringTest(ctx, t, cfg)
			return ctx
		}).
		Assess("every pod runs the keys the Secret holds, and no readable object carries them",
			func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
				t.Helper()
				keys = waitForFleetTicketKeys(ctx, t, clientset)
				client, err := cfg.NewClient()
				require.NoError(t, err)
				requireTicketKeysUnpublished(ctx, t, client.RESTConfig(), keys)
				return ctx
			}).
		Assess("a session issued before a leadership handover resumes after it",
			func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
				t.Helper()
				forward := ForwardService(t, HAProxyDeploymentName, 443)
				var resumed atomic.Bool
				client := sharedhttpclient.New(&sharedhttpclient.Config{
					Host: "127.0.0.1", HTTPSPort: forward.HTTPSPort,
					TLS: &tls.Config{
						ServerName: host, MinVersion: tls.VersionTLS13,
						InsecureSkipVerify: true, // #nosec G402 -- The test checks resumption, not the fixture's self-signed chain.
						ClientSessionCache: tls.NewLRUClientSessionCache(1),
						VerifyConnection: func(state tls.ConnectionState) error {
							resumed.Store(state.DidResume)
							return nil
						},
					},
				})
				t.Cleanup(client.CloseIdleConnections)
				request := func(ctx context.Context) bool {
					client.CloseIdleConnections()
					resumed.Store(false)
					response, err := client.HTTPS(host, "/").Do(ctx)
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, response.Status)
					return resumed.Load()
				}
				request(ctx)
				require.NoError(t, testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(ctx context.Context) (bool, error) {
					return request(ctx), nil
				}), "a second connection resumes the session")

				leader := currentLeader(ctx, t, clientset)
				standby := standbyController(ctx, t, clientset, leader)
				_, rendersBefore := renderCounters(ctx, t, clientset, standby)
				handLeaseTo(ctx, t, clientset, standby)
				waitForLeadershipHandover(ctx, t, clientset, leader)
				require.NoError(t, testutil.WaitForConditionWithDescription(ctx, testutil.SlowWaitConfig(),
					"the new leader renders", func(ctx context.Context) (bool, error) {
						_, renders := renderCounters(ctx, t, clientset, standby)
						return renders > rendersBefore, nil
					}))
				require.Equal(t, keys, waitForFleetTicketKeys(ctx, t, clientset), "the new leader keeps the keys")
				require.True(t, request(ctx), "the session issued before the handover resumes")
				return ctx
			}).
		Feature()
	testEnv.Test(t, feature)
}

func enableSessionTicketsDuringTest(ctx context.Context, t *testing.T, cfg *envconf.Config) {
	t.Helper()
	client, err := cfg.NewClient()
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(client.RESTConfig())
	require.NoError(t, err)
	configs := dyn.Resource(configGVR).Namespace(ControllerNamespace)
	original, err := configs.Get(ctx, HAProxyConfigName, metav1.GetOptions{})
	require.NoError(t, err)
	_, hasTLS, err := unstructured.NestedMap(original.Object, "spec", "templatingSettings", "extraContext", "tls")
	require.NoError(t, err)
	path, value := "/spec/templatingSettings/extraContext/tls", any(map[string]any{"sessionTickets": map[string]any{"enabled": true}})
	if hasTLS {
		path, value = path+"/sessionTickets", map[string]any{"enabled": true}
	}
	patch := func(ctx context.Context, expected *unstructured.Unstructured, operation string, value any) *unstructured.Unstructured {
		operations := []map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": expected.GetUID()},
			{"op": "test", "path": "/metadata/generation", "value": expected.GetGeneration()},
			{"op": operation, "path": path},
		}
		if value != nil {
			operations[2]["value"] = value
		}
		body, err := json.Marshal(operations)
		require.NoError(t, err)
		patched, err := configs.Patch(ctx, HAProxyConfigName, types.JSONPatchType, body, metav1.PatchOptions{})
		require.NoError(t, err)
		require.NoError(t, waitForConfigValidation(ctx, configs, patched))
		return patched
	}
	patched := patch(ctx, original, "add", value)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), testutil.SlowWaitConfig().Timeout)
		defer cancel()
		patch(cleanupCtx, patched, "remove", nil)
	})
}

// waitForFleetTicketKeys returns the keys once every sensitive-file Secret and
// every HAProxy pod hold the same ones.
func waitForFleetTicketKeys(ctx context.Context, t *testing.T, cs kubernetes.Interface) string {
	t.Helper()
	var keys string
	require.NoError(t, testutil.WaitForConditionWithDescription(ctx, testutil.SlowWaitConfig(),
		"every HAProxy pod runs the published session-ticket keys", func(ctx context.Context) (bool, error) {
			published, err := publishedTicketKeys(ctx, cs)
			if err != nil {
				return false, err
			}
			for _, pod := range listHAProxyPods(t) {
				running, err := execInHAProxyPod(ctx, pod, "haproxy", "cat", "/etc/haproxy/ssl/tls-ticket-keys")
				if err != nil {
					return false, err
				}
				if running != published {
					return false, fmt.Errorf("pod %s runs keys that differ from the published Secret", pod)
				}
			}
			keys = published
			return true, nil
		}))
	return keys
}

func publishedTicketKeys(ctx context.Context, cs kubernetes.Interface) (string, error) {
	secrets, err := cs.CoreV1().Secrets(ControllerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "haproxy-haptic.org/type=sensitive-file",
	})
	if err != nil {
		return "", err
	}
	contents := map[string]bool{}
	for i := range secrets.Items {
		if string(secrets.Items[i].Data["path"]) != "ssl/tls-ticket-keys" {
			continue
		}
		content, err := configpublisher.SensitiveFileContent(&secrets.Items[i])
		if err != nil {
			return "", fmt.Errorf("sensitive-file Secret %s: %w", secrets.Items[i].Name, err)
		}
		contents[content] = true
	}
	if len(contents) != 1 {
		return "", fmt.Errorf("%d distinct published session-ticket key sets, want 1", len(contents))
	}
	for content := range contents {
		return content, nil
	}
	return "", nil
}

func requireTicketKeysUnpublished(ctx context.Context, t *testing.T, restConfig *rest.Config, keys string) {
	t.Helper()
	hc, err := hapticclient.NewForConfig(restConfig)
	require.NoError(t, err)
	generalFiles, err := hc.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles(ControllerNamespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	cfgs, err := hc.HaproxyTemplateICV1alpha1().HAProxyCfgs(ControllerNamespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	readable := make([]any, 0, len(generalFiles.Items)+len(cfgs.Items))
	for i := range generalFiles.Items {
		require.NotContains(t, generalFiles.Items[i].Spec.Path, "tls-ticket-keys", "HAProxyGeneralFile %s", generalFiles.Items[i].Name)
		readable = append(readable, &generalFiles.Items[i])
	}
	for i := range cfgs.Items {
		readable = append(readable, &cfgs.Items[i])
	}
	for _, object := range readable {
		encoded, err := json.Marshal(object)
		require.NoError(t, err)
		for _, key := range strings.Fields(keys) {
			require.NotContains(t, string(encoded), key)
		}
	}
}
