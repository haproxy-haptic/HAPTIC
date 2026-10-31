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

package rendercontext

import (
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// SeparateHAProxyPodStore separates the haproxy-pods store from resource stores.
//
// This is needed because haproxy-pods goes in the controller namespace (controller.haproxy_pods),
// not in the resources namespace. This function extracts haproxy-pods and returns both the
// filtered resource stores and the haproxy pod store separately.
//
// Returns:
//   - resourceStores: All stores except haproxy-pods
//   - haproxyPodStore: The haproxy-pods store, or nil if not present
func SeparateHAProxyPodStore(storeMap map[string]stores.Store) (resourceStores map[string]stores.Store, haproxyPodStore stores.Store) {
	resourceStores = make(map[string]stores.Store)
	for resourceTypeName, store := range storeMap {
		if resourceTypeName == names.HAProxyPodsResourceType {
			haproxyPodStore = store
		} else {
			resourceStores[resourceTypeName] = store
		}
	}
	return resourceStores, haproxyPodStore
}

// PathResolverFromValidationPaths creates a PathResolver from ValidationPaths.
//
// This is a convenience function for creating PathResolvers in contexts where
// ValidationPaths is available (testrunner, dryrunvalidator, benchmark).
// The TempDir from ValidationPaths becomes the BaseDir used with "default-path origin"
// in HAProxy's global section to resolve relative paths.
func PathResolverFromValidationPaths(vp *dataplane.ValidationPaths) *templating.PathResolver {
	return &templating.PathResolver{
		BaseDir:    vp.TempDir,
		MapsDir:    vp.MapsDir,
		SSLDir:     vp.SSLCertsDir,
		CRTListDir: vp.CRTListDir,
		GeneralDir: vp.GeneralStorageDir,
		SecretDir:  vp.SecretDir,
	}
}
