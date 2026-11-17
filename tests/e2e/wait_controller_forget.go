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
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"

	hapticclient "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

// controllerForgetTimeout caps the best-effort cleanup wait for the controller to
// drop a deleted namespace from its rendered config. Reload-independent (it tracks
// re-render+publish, not a fleet reload), so a generous flat budget — not the
// reload-progress gate — absorbs the publish backlog under ~70 parallel teardowns;
// on timeout the caller only logs, never fails.
const controllerForgetTimeout = 90 * time.Second

type controllerForgetCleanupKey struct {
	test      *testing.T
	namespace string
}

var controllerForgetCleanupKeys sync.Map

// namespaceOwners maps a NamespaceForTest namespace to the test that deletes it.
var namespaceOwners sync.Map

// registerControllerForgetNamespaceCleanup schedules one wait after every Ingress cleanup in the namespace.
// It runs on the namespace's owner: a subtest's wait would poll while its parent's Ingresses still render.
func registerControllerForgetNamespaceCleanup(t *testing.T, client klient.Client, namespace string) {
	t.Helper()
	owner := t
	if v, ok := namespaceOwners.Load(namespace); ok {
		owner = v.(*testing.T)
	}
	key := controllerForgetCleanupKey{test: owner, namespace: namespace}
	if _, loaded := controllerForgetCleanupKeys.LoadOrStore(key, struct{}{}); loaded {
		return
	}

	owner.Cleanup(func() {
		defer controllerForgetCleanupKeys.Delete(key)
		waitForControllerForgetNamespace(context.Background(), owner, client, namespace)
	})
}

// waitForControllerForgetNamespace polls the controller's /debug/vars/rendered
// endpoint until the rendered haproxy.cfg no longer contains the given
// namespace. This is used during test cleanup, after an Ingress has been
// explicitly deleted from the API server, to bound the window in which the
// controller's resource-store still contains the (now stale) Ingress.
//
// The race we close: apiserver Delete returns synchronously, but the
// controller's watcher has its own latency. While the watcher is catching
// up, the controller's render still includes the Ingress; if another
// parallel test's webhook validation fires during that window and the
// referenced Secret has already been cascade-deleted, the render fails
// with an [ALERT]-level "unable to find userlist" / "Secret does not
// exist" error and admission is denied for the unrelated resource.
//
// On timeout we log and return without failing the test — cleanup is
// best-effort, and the test that triggered the cleanup has already
// completed. The cap exists so a stuck controller can't wedge the suite.
func waitForControllerForgetNamespace(ctx context.Context, t *testing.T, client klient.Client, namespace string) {
	t.Helper()

	// Read the rendered config straight from the HAProxyCfg CR (the same source
	// waitForControllerDeployed uses) rather than the apiserver service-proxy to
	// the controller's debug endpoint. The proxy path (ProxyGet -> controller
	// :8080, fetching the full rendered config every poll) strains under
	// parallel-test cleanup churn and returns the opaque
	// `an error on the server ("unknown")`; a direct CR GET is served from the
	// apiserver's watch cache and never proxies to a pod.
	hc, err := hapticclient.NewForConfig(client.RESTConfig())
	if err != nil {
		t.Logf("waitForControllerForgetNamespace: build haptic clientset: %v (skipping wait)", err)
		return
	}
	cfgName := HAProxyConfigName + "-haproxycfg"

	cfg := testutil.FastWaitConfig()
	cfg.Timeout = controllerForgetTimeout

	err = testutil.WaitForConditionWithDescription(ctx, cfg,
		"controller forgot namespace "+namespace,
		func(ctx context.Context) (bool, error) {
			obj, err := hc.HaproxyTemplateICV1alpha1().HAProxyCfgs(ControllerNamespace).Get(ctx, cfgName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if obj.Spec.Compressed {
				return false, fmt.Errorf("HAProxyCfg.spec.content is compressed; forget-namespace wait does not decompress")
			}
			return !strings.Contains(obj.Spec.Content, namespace), nil
		})
	if err != nil {
		t.Logf("waitForControllerForgetNamespace %q: %v (cleanup proceeding anyway)", namespace, err)
	}
}
