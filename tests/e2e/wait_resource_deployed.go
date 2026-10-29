//go:build e2e

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

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/e2e-framework/klient"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
)

// Wait for the resource the test just applied to report the controller's
// deployed-phase status, instead of inspecting the shared HAProxyCfg.
//
// The controller already writes exactly this signal per resource, and since
// statusapplier gates the "deployed" variant on Succeeded == Total it means
// "every HAProxy replica has this configuration" — which is the question these
// tests are asking. Reading it from the resource under test removes the whole
// class of problems that came from inferring it out of a shared object: no
// marker to choose, no checksum-to-generation join, no dependence on which spec
// versions this process's watch happened to receive, and no ~2MB object rescanned
// by ~30 concurrent waiters. A watch that misses events cannot lie here, because
// the predicate is level-triggered on the resource's own current state.
//
// Kubernetes convention does the ordering: the chart stamps the route's own
// metadata.generation into each condition's observedGeneration, so "this status
// describes THIS version of my resource" is decidable from the object alone.

// gatewayGVR and httpRouteGVR are declared in gateway_churn_test.go.
var (
	ingressGVR   = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	grpcRouteGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "grpcroutes"}
)

// waitForIngressDeployed blocks until the Ingress carries a load-balancer
// address. The chart writes the address under the "deployed" variant and an
// EMPTY list under "deployFailed", so a non-empty list is unambiguous.
func waitForIngressDeployed(ctx context.Context, t *testing.T, client klient.Client, namespace, name string) {
	t.Helper()
	waitForResourceDeployed(ctx, t, client, ingressGVR, namespace, name,
		func(obj *unstructured.Unstructured) (bool, string) {
			addrs, found, err := unstructured.NestedSlice(obj.Object, "status", "loadBalancer", "ingress")
			if err != nil || !found || len(addrs) == 0 {
				return false, "status.loadBalancer.ingress is empty (not deployed to every replica yet)"
			}
			return true, ""
		})
}

// waitForRouteDeployed blocks until every parent of the route reports conditions
// for the route's CURRENT generation. The chart emits route status under the
// "deployed" variant only, so the presence of up-to-date parents is itself the
// deployed signal; comparing observedGeneration to metadata.generation is what
// makes a status left over from a previous version of the route not count.
func waitForRouteDeployed(ctx context.Context, t *testing.T, client klient.Client, gvr schema.GroupVersionResource, namespace, name string) {
	t.Helper()
	waitForResourceDeployed(ctx, t, client, gvr, namespace, name,
		routeParentsAtCurrentGeneration)
}

// waitForResourceDeployed polls the named resource until ready reports true,
// bounded by fleet reload progress (reloadFreeReaction) so a reload backlog on
// the shared reload-gated shards never trips a flat deadline (#174). A poll, not
// a watch: the predicate is level-triggered on the object's own state, so a 410
// resync cannot drop the transition we needed — how the shared-object watch
// failed (#122).
func waitForResourceDeployed(
	ctx context.Context,
	t *testing.T,
	client klient.Client,
	gvr schema.GroupVersionResource,
	namespace, name string,
	ready func(*unstructured.Unstructured) (bool, string),
) {
	t.Helper()

	dyn, err := dynamic.NewForConfig(client.RESTConfig())
	if err != nil {
		t.Fatalf("waitForResourceDeployed: dynamic client: %v", err)
	}
	// reloadFreeReaction reads fleet reload progress from a clientset; derive it
	// here so the callers keep passing only klient.Client.
	cs, err := newClientsetForE2E(client.RESTConfig())
	if err != nil {
		t.Fatalf("waitForResourceDeployed: clientset: %v", err)
	}

	reloadFreeReaction(ctx, t, cs,
		fmt.Sprintf("%s %s/%s deployed to every replica", gvr.Resource, namespace, name),
		func(ctx context.Context) (bool, error) {
			obj, getErr := dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
			if getErr != nil {
				return false, fmt.Errorf("get %s %s/%s: %w", gvr.Resource, namespace, name, getErr)
			}
			done, reason := ready(obj)
			if done {
				return true, nil
			}
			return false, errors.New(reason)
		})
}

func routeParentsAtCurrentGeneration(obj *unstructured.Unstructured) (ready bool, reason string) {
	parents, found, err := unstructured.NestedSlice(obj.Object, "status", "parents")
	if err != nil || !found || len(parents) == 0 {
		return false, "status.parents is empty (controller has not reported a deploy for this route)"
	}
	gen := obj.GetGeneration()
	for _, p := range parents {
		pm, ok := p.(map[string]any)
		if !ok {
			return false, "malformed status.parents entry"
		}
		conds, _, _ := unstructured.NestedSlice(pm, "conditions")
		if len(conds) == 0 {
			return false, "a parent has no conditions yet"
		}
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if !ok {
				return false, "malformed condition"
			}
			og, _, _ := unstructured.NestedInt64(cm, "observedGeneration")
			if og != gen {
				typ, _, _ := unstructured.NestedString(cm, "type")
				return false, fmt.Sprintf("condition %q is at observedGeneration %d, route is at %d", typ, og, gen)
			}
		}
	}
	return true, ""
}

func configMarkerRemovalReloaded(cfg *haproxyv1alpha1.HAProxyCfg, marker string, replicas int) (bool, error) {
	content, err := haproxyCfgContent(cfg)
	if err != nil {
		return false, err
	}
	if replicas < 1 || cfg.Spec.Checksum == "" || strings.Contains(content, marker) || len(cfg.Status.DeployedToPods) < replicas {
		return false, nil
	}
	for index := range cfg.Status.DeployedToPods {
		pod := &cfg.Status.DeployedToPods[index]
		if pod.Checksum != cfg.Spec.Checksum || pod.AppliedPlanID == "" || pod.RunningPlanID != pod.AppliedPlanID || pod.LastError != "" {
			return false, nil
		}
	}
	return true, nil
}
