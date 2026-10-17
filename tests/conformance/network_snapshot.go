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
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

var (
	gatewayGVR         = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
	serviceL2StatusGVR = schema.GroupVersionResource{Group: "metallb.io", Version: "v1beta1", Resource: "servicel2statuses"}
)

// Eleven values at most (seven fixed keys plus maxCapturedNodes) stay under the 1 MiB ConfigMap limit.
const (
	networkCaptureLimit = 64 * 1024
	maxCapturedNodes    = 4
)

// captureNetwork records which layer still refuses a VIP: the LB Service and its endpoints, MetalLB's
// announcement, kube-proxy's rules, and the ARP state on both ends of the dial.
func (s *snapshottingRoundTripper) captureNetwork(testName string, ip netip.Addr, capture int, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	data := map[string]string{}
	record := func(key string, payload []byte, err error) {
		if err != nil {
			payload = append(payload, []byte(fmt.Sprintf("\n# error: %v\n", err))...)
		}
		data[key] = truncateCapture(payload, networkCaptureLimit)
	}

	services, err := s.cs.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	var selected []corev1.Service
	if err == nil {
		selected = servicesAnnouncing(services.Items, ip)
	}
	for i := range selected {
		selected[i].ManagedFields = nil
	}
	record("services.yaml", marshalCapture(selected), err)

	var slices []discoveryv1.EndpointSlice
	var sliceErr error
	for i := range selected {
		list, listErr := s.cs.DiscoveryV1().EndpointSlices(selected[i].Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: discoveryv1.LabelServiceName + "=" + selected[i].Name,
		})
		sliceErr = errors.Join(sliceErr, listErr)
		if list != nil {
			slices = append(slices, list.Items...)
		}
	}
	for i := range slices {
		slices[i].ManagedFields = nil
	}
	record("endpointslices.yaml", marshalCapture(slices), sliceErr)

	gateways, err := s.dyn.Resource(gatewayGVR).List(ctx, metav1.ListOptions{})
	var matched []unstructured.Unstructured
	if err == nil {
		matched = gatewaysWithAddress(gateways.Items, ip)
	}
	record("gateways.yaml", marshalUnstructured(matched), err)

	l2, err := s.dyn.Resource(serviceL2StatusGVR).List(ctx, metav1.ListOptions{})
	var statuses []unstructured.Unstructured
	if err == nil {
		statuses = l2.Items
	}
	record("servicel2statuses.yaml", marshalUnstructured(statuses), err)

	clientARP, err := os.ReadFile("/proc/net/arp")
	record("client-arp.txt", clientARP, err)

	s.captureNodes(ctx, ip, record)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      snapshotConfigMapName("conformance-network-", fmt.Sprintf("%s-%s-%d", testName, ip, capture)),
			Namespace: snapshotNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/component": "conformance-snapshot",
				testNameKey:                   sanitizeForLabel(testName),
				"haptic.io/snapshot-kind":     "network",
			},
			Annotations: map[string]string{
				testNameKey:           testName,
				"haptic.io/timestamp": time.Now().UTC().Format(time.RFC3339Nano),
				"haptic.io/reason":    reason,
				"haptic.io/dialed-ip": ip.String(),
			},
		},
		Data: data,
	}
	_, _ = s.cs.CoreV1().ConfigMaps(snapshotNamespace).Create(ctx, cm, metav1.CreateOptions{})
}

func (s *snapshottingRoundTripper) captureNodes(ctx context.Context, ip netip.Addr, record func(string, []byte, error)) {
	pods, err := s.cs.CoreV1().Pods(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=kube-proxy"})
	if err != nil {
		record("nodes.txt", nil, err)
		return
	}
	script := nodeNetworkScript(ip)
	for i := range pods.Items {
		if i == maxCapturedNodes {
			record("nodes.txt", []byte(fmt.Sprintf("# %d kube-proxy pods, first %d captured\n", len(pods.Items), maxCapturedNodes)), nil)
			break
		}
		pod := &pods.Items[i]
		out, execErr := s.exec(ctx, pod.Namespace, pod.Name, "kube-proxy", []string{"sh", "-c", script})
		record("node-"+pod.Spec.NodeName+".txt", out, execErr)
	}
}

func marshalUnstructured(items []unstructured.Unstructured) []byte {
	objects := make([]map[string]any, 0, len(items))
	for i := range items {
		unstructured.RemoveNestedField(items[i].Object, "metadata", "managedFields")
		objects = append(objects, items[i].Object)
	}
	return marshalCapture(objects)
}

func marshalCapture(v any) []byte {
	out, err := yaml.Marshal(v)
	if err != nil {
		return []byte(fmt.Sprintf("# marshal failed: %v\n", err))
	}
	return out
}
