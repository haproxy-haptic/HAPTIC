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

//go:build acceptance

package acceptance

import "os"

// CRD and API constants.
const (
	// HAProxyTemplateConfigAPIVersion is the API version for HAProxyTemplateConfig resources.
	HAProxyTemplateConfigAPIVersion = "haproxy-haptic.org/v1alpha1"

	// HAProxyTemplateConfigKind is the Kind name for HAProxyTemplateConfig resources.
	HAProxyTemplateConfigKind = "HAProxyTemplateConfig"

	// CRDDirectory is the path to the CRD manifests relative to the test directory.
	CRDDirectory = "../../charts/haptic/crds/"
)

// ControllerImageName and ClusterName let a run use its own image and kind cluster.
var (
	ControllerImageName = envOrDefault("HAPTIC_ACCEPTANCE_IMAGE", "haptic:test")
	ClusterName         = envOrDefault("HAPTIC_ACCEPTANCE_CLUSTER_NAME", "haproxy-test")
)

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// RequiredCRDs lists the CRDs that must be installed for acceptance tests.
var RequiredCRDs = []string{
	"crd/haproxytemplateconfigs.haproxy-haptic.org",
	"crd/haproxycfgs.haproxy-haptic.org",
	"crd/haproxymapfiles.haproxy-haptic.org",
}

// Debug endpoint paths.
const (
	DebugPathConfig   = "/debug/vars/config"
	DebugPathRendered = "/debug/vars/rendered"
	DebugPathPipeline = "/debug/vars/pipeline"
	DebugPathErrors   = "/debug/vars/errors"
	DebugPathAuxFiles = "/debug/vars/auxfiles"
)

// DebugServiceName returns the service name for a controller's debug endpoint.
func DebugServiceName(deploymentName string) string {
	return deploymentName + "-debug"
}

// MetricsServiceName returns the service name for a controller's metrics endpoint.
func MetricsServiceName(deploymentName string) string {
	return deploymentName + "-metrics"
}
