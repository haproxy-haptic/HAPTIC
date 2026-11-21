// Copyright 2026 Philipp Hossner
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

package deployer

import (
	"slices"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
)

// FleetCapabilitiesSink receives what the fleet's HAProxy version supports.
// The renderer implements it; templates read the result as `capabilities`.
type FleetCapabilitiesSink interface {
	SetCapabilities(dataplane.Capabilities)
}

// publishFleetCapabilities re-sources the template `capabilities` input from
// the fleet's lowest reported HAProxy version and from whether every agent
// stores blobs, so a render never uses a feature the oldest pod would reject.
// A change triggers one reconciliation: the value only reaches templates
// through a render.
func (s *DeploymentScheduler) publishFleetCapabilities(endpoints []dataplane.Endpoint) {
	if s.capabilities == nil {
		return
	}
	version := dataplane.MinimumVersion(fleetVersions(endpoints))
	if version == nil {
		return
	}
	capabilities := dataplane.CapabilitiesFromVersion(version)
	capabilities.SupportsBlobs = !slices.ContainsFunc(endpoints, func(endpoint dataplane.Endpoint) bool {
		return !endpoint.AcceptsBlobs
	})

	s.mu.Lock()
	changed := s.lastFleetCapabilities == nil || *s.lastFleetCapabilities != capabilities
	s.lastFleetCapabilities = &capabilities
	s.mu.Unlock()
	if !changed {
		return
	}

	s.logger.Info("Fleet capabilities changed, re-sourcing template capabilities",
		"version", version.Full, "blobs", capabilities.SupportsBlobs, "pods", len(endpoints))
	s.capabilities.SetCapabilities(capabilities)
	s.eventBus.Publish(events.NewReconciliationTriggeredEvent(
		"fleet_capabilities_changed", false, events.WithNewCorrelation()))
}

func fleetVersions(endpoints []dataplane.Endpoint) []string {
	versions := make([]string, 0, len(endpoints))
	for i := range endpoints {
		if endpoints[i].DetectedFullVersion != "" {
			versions = append(versions, endpoints[i].DetectedFullVersion)
		}
	}
	return versions
}
