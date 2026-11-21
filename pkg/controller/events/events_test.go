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

package events

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
)

func TestConfigEvents(t *testing.T) {
	t.Run("ConfigParsedEvent", func(t *testing.T) {
		config := map[string]string{"key": "value"}
		templateConfig := map[string]string{"template": "config"}
		event := NewConfigParsedEvent(config, templateConfig, "v1", "secret-v1")
		require.NotNil(t, event)
		assert.Equal(t, config, event.Config)
		assert.Equal(t, templateConfig, event.TemplateConfig)
		assert.Equal(t, "v1", event.Version)
		assert.Equal(t, "secret-v1", event.SecretVersion)
		assert.Equal(t, EventTypeConfigParsed, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ConfigValidationRequest", func(t *testing.T) {
		config := map[string]string{"key": "value"}
		event := NewConfigValidationRequest(config, "v1")
		require.NotNil(t, event)
		assert.Equal(t, config, event.Config)
		assert.Equal(t, "v1", event.Version)
		assert.Equal(t, EventTypeConfigValidationRequest, event.EventType())
		assert.NotEmpty(t, event.RequestID())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ConfigValidationResponse", func(t *testing.T) {
		errors := []string{"error1", "error2"}
		event := NewConfigValidationResponse("req-123", "basic", false, errors)
		require.NotNil(t, event)
		assert.Equal(t, "basic", event.ValidatorName)
		assert.False(t, event.Valid)
		assert.Equal(t, errors, event.Errors)
		assert.Equal(t, EventTypeConfigValidationResponse, event.EventType())
		assert.Equal(t, "req-123", event.RequestID())
		assert.Equal(t, "basic", event.Responder())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ConfigValidationResponse_DefensiveCopy", func(t *testing.T) {
		errors := []string{"error1", "error2"}
		event := NewConfigValidationResponse("req-123", "basic", false, errors)

		// Modify original slice
		errors[0] = "modified"

		// Event should have the original value
		assert.Equal(t, "error1", event.Errors[0])
	})

	t.Run("ConfigValidationResponse_EmptyErrors", func(t *testing.T) {
		event := NewConfigValidationResponse("req-123", "basic", true, nil)
		require.NotNil(t, event)
		assert.True(t, event.Valid)
		assert.Nil(t, event.Errors)
	})

	t.Run("ConfigValidatedEvent", func(t *testing.T) {
		config := map[string]string{"key": "value"}
		templateConfig := map[string]string{"template": "config"}
		event := NewConfigValidatedEvent(config, templateConfig, "v1", "secret-v1")
		require.NotNil(t, event)
		assert.Equal(t, config, event.Config)
		assert.Equal(t, templateConfig, event.TemplateConfig)
		assert.Equal(t, "v1", event.Version)
		assert.Equal(t, "secret-v1", event.SecretVersion)
		assert.False(t, event.ActiveSnapshotRestore)
		assert.Equal(t, EventTypeConfigValidated, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ConfigInvalidEvent", func(t *testing.T) {
		validationErrors := map[string][]string{
			"basic":    {"error1"},
			"template": {"error2", "error3"},
		}
		event := NewConfigInvalidEvent("v1", nil, validationErrors)
		require.NotNil(t, event)
		assert.Equal(t, "v1", event.Version)
		assert.Nil(t, event.TemplateConfig)
		assert.Equal(t, validationErrors, event.ValidationErrors)
		assert.Equal(t, EventTypeConfigInvalid, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ConfigInvalidEvent_DefensiveCopy", func(t *testing.T) {
		validationErrors := map[string][]string{
			"basic": {"error1"},
		}
		event := NewConfigInvalidEvent("v1", nil, validationErrors)

		// Modify original map
		validationErrors["basic"][0] = "modified"
		validationErrors["new"] = []string{"new error"}

		// Event should have the original values
		assert.Equal(t, "error1", event.ValidationErrors["basic"][0])
		assert.NotContains(t, event.ValidationErrors, "new")
	})

	t.Run("ConfigResourceChangedEvent", func(t *testing.T) {
		resource := map[string]string{"name": "my-config"}
		event := NewConfigResourceChangedEvent(resource)
		require.NotNil(t, event)
		assert.Equal(t, resource, event.Resource)
		assert.Equal(t, EventTypeConfigResourceChanged, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})
}

func TestResourceEvents(t *testing.T) {
	t.Run("ResourceIndexUpdatedEvent", func(t *testing.T) {
		changeStats := types.ChangeStats{
			Created:       2,
			Modified:      1,
			Deleted:       0,
			IsInitialSync: false,
		}
		event := NewResourceIndexUpdatedEvent("ingresses", changeStats)
		require.NotNil(t, event)
		assert.Equal(t, "ingresses", event.ResourceTypeName)
		assert.Equal(t, changeStats, event.ChangeStats)
		assert.Equal(t, EventTypeResourceIndexUpdated, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ResourceSyncCompleteEvent", func(t *testing.T) {
		event := NewResourceSyncCompleteEvent("services", 10)
		require.NotNil(t, event)
		assert.Equal(t, "services", event.ResourceTypeName)
		assert.Equal(t, 10, event.InitialCount)
		assert.Equal(t, EventTypeResourceSyncComplete, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("IndexSynchronizedEvent", func(t *testing.T) {
		resourceCounts := map[string]int{
			"ingresses": 5,
			"services":  10,
		}
		event := NewIndexSynchronizedEvent(resourceCounts)
		require.NotNil(t, event)
		assert.Equal(t, resourceCounts, event.ResourceCounts)
		assert.Equal(t, EventTypeIndexSynchronized, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("IndexSynchronizedEvent_DefensiveCopy", func(t *testing.T) {
		resourceCounts := map[string]int{
			"ingresses": 5,
		}
		event := NewIndexSynchronizedEvent(resourceCounts)

		// Modify original map
		resourceCounts["ingresses"] = 100
		resourceCounts["new"] = 50

		// Event should have the original values
		assert.Equal(t, 5, event.ResourceCounts["ingresses"])
		assert.NotContains(t, event.ResourceCounts, "new")
	})
}

func TestLeaderEvents(t *testing.T) {
	t.Run("LeaderElectionStartedEvent", func(t *testing.T) {
		event := NewLeaderElectionStartedEvent("pod-1", "my-lease", "default")
		require.NotNil(t, event)
		assert.Equal(t, "pod-1", event.Identity)
		assert.Equal(t, "my-lease", event.LeaseName)
		assert.Equal(t, "default", event.LeaseNamespace)
		assert.Equal(t, EventTypeLeaderElectionStarted, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("BecameLeaderEvent", func(t *testing.T) {
		event := NewBecameLeaderEvent("pod-1")
		require.NotNil(t, event)
		assert.Equal(t, "pod-1", event.Identity)
		assert.Equal(t, EventTypeBecameLeader, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("LostLeadershipEvent", func(t *testing.T) {
		event := NewLostLeadershipEvent("pod-1", "graceful_shutdown")
		require.NotNil(t, event)
		assert.Equal(t, "pod-1", event.Identity)
		assert.Equal(t, "graceful_shutdown", event.Reason)
		assert.Equal(t, EventTypeLostLeadership, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("NewLeaderObservedEvent", func(t *testing.T) {
		event := NewNewLeaderObservedEvent("pod-2", false)
		require.NotNil(t, event)
		assert.Equal(t, "pod-2", event.NewLeaderIdentity)
		assert.False(t, event.IsSelf)
		assert.Equal(t, EventTypeNewLeaderObserved, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})
}

func TestReconciliationEvents(t *testing.T) {
	t.Run("ReconciliationTriggeredEvent", func(t *testing.T) {
		event := NewReconciliationTriggeredEvent("config_change", true)
		require.NotNil(t, event)
		assert.Equal(t, "config_change", event.Reason)
		assert.Equal(t, EventTypeReconciliationTriggered, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ReconciliationTriggeredEvent_WithNewCorrelation", func(t *testing.T) {
		event := NewReconciliationTriggeredEvent("config_change", true, WithNewCorrelation())
		require.NotNil(t, event)
		assert.NotEmpty(t, event.CorrelationID())
		assert.NotEmpty(t, event.EventID())
	})

	t.Run("ReconciliationStartedEvent", func(t *testing.T) {
		event := NewReconciliationStartedEvent("config_change")
		require.NotNil(t, event)
		assert.Equal(t, "config_change", event.Trigger)
		assert.Equal(t, EventTypeReconciliationStarted, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ReconciliationStartedEvent_WithCorrelation", func(t *testing.T) {
		event := NewReconciliationStartedEvent("config_change",
			WithCorrelation("corr-123", "cause-456"))
		require.NotNil(t, event)
		assert.Equal(t, "corr-123", event.CorrelationID())
		assert.Equal(t, "cause-456", event.CausationID())
	})

	t.Run("ReconciliationCompletedEvent", func(t *testing.T) {
		event := NewReconciliationCompletedEvent(100, "", nil, nil)
		require.NotNil(t, event)
		assert.Equal(t, int64(100), event.DurationMs)
		assert.Nil(t, event.RenderedResources)
		assert.Equal(t, EventTypeReconciliationCompleted, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ReconciliationFailedEvent", func(t *testing.T) {
		event := NewReconciliationFailedEvent("template error", "render", nil)
		require.NotNil(t, event)
		assert.Equal(t, "template error", event.Error)
		assert.Equal(t, "render", event.Phase)
		assert.Equal(t, EventTypeReconciliationFailed, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})
}

func TestCredentialsEvents(t *testing.T) {
	t.Run("SecretResourceChangedEvent", func(t *testing.T) {
		resource := map[string]string{"name": "my-secret"}
		event := NewSecretResourceChangedEvent(resource)
		require.NotNil(t, event)
		assert.Equal(t, resource, event.Resource)
		assert.Equal(t, EventTypeSecretResourceChanged, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("CredentialsUpdatedEvent", func(t *testing.T) {
		credentials := map[string]string{"user": "admin"}
		event := NewCredentialsUpdatedEvent(credentials, "v1")
		require.NotNil(t, event)
		assert.Equal(t, credentials, event.Credentials)
		assert.Equal(t, "v1", event.SecretVersion)
		assert.Equal(t, EventTypeCredentialsUpdated, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})
}

func TestHTTPEvents(t *testing.T) {
	t.Run("HTTPResourceUpdatedEvent", func(t *testing.T) {
		event := NewHTTPResourceUpdatedEvent("https://example.com/resource", "abc123", 1024)
		require.NotNil(t, event)
		assert.Equal(t, "https://example.com/resource", event.URL)
		assert.Equal(t, "abc123", event.ContentChecksum)
		assert.Equal(t, 1024, event.ContentSize)
		assert.Equal(t, EventTypeHTTPResourceUpdated, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("HTTPResourceAcceptedEvent", func(t *testing.T) {
		event := NewHTTPResourceAcceptedEvent("https://example.com/resource", "abc123", 1024)
		require.NotNil(t, event)
		assert.Equal(t, "https://example.com/resource", event.URL)
		assert.Equal(t, "abc123", event.ContentChecksum)
		assert.Equal(t, 1024, event.ContentSize)
		assert.Equal(t, EventTypeHTTPResourceAccepted, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})
}

func TestTemplateEvents(t *testing.T) {
	t.Run("TemplateRenderedEvent", func(t *testing.T) {
		auxFiles := &dataplane.AuxiliaryFiles{}
		event := NewTemplateRenderedEvent(
			"haproxy config",
			auxFiles,
			nil, // statusPatches
			nil, // renderedResources
			5,
			100,
			"resource_change",
			"",   // contentChecksum
			nil,  // plan
			"",   // planID
			true, // coalescible
		)
		require.NotNil(t, event)
		assert.Equal(t, "haproxy config", event.HAProxyConfig)
		assert.Equal(t, auxFiles, event.AuxiliaryFiles)
		assert.Equal(t, 5, event.AuxiliaryFileCount)
		assert.Equal(t, int64(100), event.DurationMs)
		assert.Equal(t, len("haproxy config"), event.ConfigBytes)
		assert.Equal(t, EventTypeTemplateRendered, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("TemplateRenderedEvent_WithCorrelation", func(t *testing.T) {
		event := NewTemplateRenderedEvent("cfg", nil, nil, nil, 0, 0, "", "", nil, "", true,
			WithCorrelation("corr-123", "cause-456"))
		require.NotNil(t, event)
		assert.Equal(t, "corr-123", event.CorrelationID())
		assert.Equal(t, "cause-456", event.CausationID())
		assert.NotEmpty(t, event.EventID())
	})

	t.Run("TemplateRenderFailedEvent", func(t *testing.T) {
		event := NewTemplateRenderFailedEvent("haproxy.cfg", "syntax error", "stack trace line 1\nstack trace line 2")
		require.NotNil(t, event)
		assert.Equal(t, "haproxy.cfg", event.TemplateName)
		assert.Equal(t, "syntax error", event.Error)
		assert.Contains(t, event.StackTrace, "stack trace")
		assert.Equal(t, EventTypeTemplateRenderFailed, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("TemplateRenderFailedEvent_WithCorrelation", func(t *testing.T) {
		event := NewTemplateRenderFailedEvent("template", "error", "stack",
			WithNewCorrelation())
		require.NotNil(t, event)
		assert.NotEmpty(t, event.CorrelationID())
		assert.NotEmpty(t, event.EventID())
	})
}

func TestValidationEvents(t *testing.T) {
	t.Run("RenderGateCompletedEvent", func(t *testing.T) {
		event := NewRenderGateCompletedEvent("plan-1", false, true, true, "boom", true, 50)
		require.NotNil(t, event)
		assert.Equal(t, "plan-1", event.PlanID)
		assert.False(t, event.OK)
		assert.True(t, event.Refused)
		assert.True(t, event.Pinned)
		assert.Equal(t, "boom", event.Message)
		assert.Equal(t, int64(50), event.DurationMs)
		assert.Equal(t, EventTypeRenderGateCompleted, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ValidationFailedEvent", func(t *testing.T) {
		errors := []string{"error1", "error2"}
		event := NewValidationFailedEvent(errors, 100, "drift_prevention")
		require.NotNil(t, event)
		assert.Equal(t, errors, event.Errors)
		assert.Equal(t, int64(100), event.DurationMs)
		assert.Equal(t, "drift_prevention", event.TriggerReason)
		assert.Equal(t, EventTypeValidationFailed, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ValidationFailedEvent_DefensiveCopy", func(t *testing.T) {
		errors := []string{"error1"}
		event := NewValidationFailedEvent(errors, 100, "")

		// Modify original
		errors[0] = "modified"

		// Event should have original value
		assert.Equal(t, "error1", event.Errors[0])
	})

	t.Run("ValidationFailedEvent_EmptyErrors", func(t *testing.T) {
		event := NewValidationFailedEvent(nil, 100, "")
		require.NotNil(t, event)
		assert.Nil(t, event.Errors)
	})
}

func TestDeploymentEvents(t *testing.T) {
	t.Run("DeploymentStartedEvent", func(t *testing.T) {
		event := NewDeploymentStartedEvent(2)
		require.NotNil(t, event)
		assert.Equal(t, 2, event.EndpointCount)
		assert.Equal(t, EventTypeDeploymentStarted, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("DeploymentStartedEvent_EmptyEndpoints", func(t *testing.T) {
		event := NewDeploymentStartedEvent(0)
		require.NotNil(t, event)
		assert.Zero(t, event.EndpointCount)
	})

	t.Run("DeploymentStartedEvent_WithCorrelation", func(t *testing.T) {
		event := NewDeploymentStartedEvent(0, WithCorrelation("corr-123", "cause-456"))
		require.NotNil(t, event)
		assert.Equal(t, "corr-123", event.CorrelationID())
		assert.Equal(t, "cause-456", event.CausationID())
	})

	t.Run("InstanceDeploymentFailedEvent", func(t *testing.T) {
		event := NewInstanceDeploymentFailedEvent("endpoint1", "connection refused", true)
		require.NotNil(t, event)
		assert.Equal(t, "endpoint1", event.Endpoint)
		assert.Equal(t, "connection refused", event.Error)
		assert.True(t, event.Retryable)
		assert.Equal(t, EventTypeInstanceDeploymentFailed, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("InstanceDeploymentFailedEvent_WithCorrelation", func(t *testing.T) {
		event := NewInstanceDeploymentFailedEvent("ep", "error", false,
			WithCorrelation("corr", "cause"))
		require.NotNil(t, event)
		assert.Equal(t, "corr", event.CorrelationID())
	})

	t.Run("DeploymentCompletedEvent", func(t *testing.T) {
		event := NewDeploymentCompletedEvent(&DeploymentResult{
			DeploymentID:       "deployment-1",
			Total:              10,
			Succeeded:          8,
			Failed:             2,
			DurationMs:         500,
			ReloadsTriggered:   3,
			TotalAPIOperations: 25,
			ContentChecksum:    "content-hash",
			PodSetHash:         "pod-set-hash",
		})
		require.NotNil(t, event)
		assert.Equal(t, "deployment-1", event.DeploymentID)
		assert.Equal(t, 10, event.Total)
		assert.Equal(t, 8, event.Succeeded)
		assert.Equal(t, 2, event.Failed)
		assert.Equal(t, int64(500), event.DurationMs)
		assert.Equal(t, 3, event.ReloadsTriggered)
		assert.Equal(t, 25, event.TotalAPIOperations)
		assert.Equal(t, "content-hash", event.ContentChecksum)
		assert.Equal(t, "pod-set-hash", event.PodSetHash)
		assert.Equal(t, EventTypeDeploymentCompleted, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("DeploymentCompletedEvent_WithCorrelation", func(t *testing.T) {
		event := NewDeploymentCompletedEvent(&DeploymentResult{
			Total:              5,
			Succeeded:          5,
			DurationMs:         100,
			ReloadsTriggered:   5,
			TotalAPIOperations: 15,
		}, WithNewCorrelation())
		require.NotNil(t, event)
		assert.NotEmpty(t, event.CorrelationID())
	})

	t.Run("DeploymentScheduledEvent", func(t *testing.T) {
		endpoints := []dataplane.Endpoint{
			{URL: "http://ep1:5555"},
			{URL: "http://ep2:5555"},
		}
		auxFiles := &dataplane.AuxiliaryFiles{}
		event := NewDeploymentScheduledEvent(
			"haproxy config",
			auxFiles,
			endpoints,
			"my-config",
			"default",
			"config_validation",
			"",   // contentChecksum
			nil,  // plan
			"",   // planID
			nil,  // statusPatches
			true, // coalescible
		)
		require.NotNil(t, event)
		assert.Equal(t, "haproxy config", event.Config)
		assert.Equal(t, auxFiles, event.AuxiliaryFiles)
		assert.Len(t, event.Endpoints, 2)
		assert.Equal(t, "my-config", event.RuntimeConfigName)
		assert.Equal(t, "default", event.RuntimeConfigNamespace)
		assert.Equal(t, "config_validation", event.Reason)
		assert.Equal(t, EventTypeDeploymentScheduled, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("DeploymentScheduledEvent_DefensiveCopy", func(t *testing.T) {
		endpoints := []dataplane.Endpoint{{URL: "http://ep1:5555"}}
		event := NewDeploymentScheduledEvent("cfg", nil, endpoints, "n", "ns", "r", "", nil, "", nil, true)

		// Modify original
		endpoints[0] = dataplane.Endpoint{URL: "http://modified:5555"}

		// Event should have original value
		assert.Equal(t, "http://ep1:5555", event.Endpoints[0].URL)
	})

	t.Run("DeploymentScheduledEvent_WithCorrelation", func(t *testing.T) {
		event := NewDeploymentScheduledEvent("cfg", nil, nil, "n", "ns", "r", "", nil, "", nil, true,
			WithCorrelation("corr", "cause"))
		require.NotNil(t, event)
		assert.Equal(t, "corr", event.CorrelationID())
		assert.Equal(t, "cause", event.CausationID())
	})

	t.Run("DriftPreventionTriggeredEvent", func(t *testing.T) {
		event := NewDriftPreventionTriggeredEvent(30 * time.Minute)
		require.NotNil(t, event)
		assert.Equal(t, 30*time.Minute, event.TimeSinceLastDeployment)
		assert.Equal(t, EventTypeDriftPreventionTriggered, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})
}

func TestDiscoveryEvents(t *testing.T) {
	t.Run("HAProxyPodsDiscoveredEvent", func(t *testing.T) {
		endpoints := []dataplane.Endpoint{
			{URL: "http://ep1:5555"},
			{URL: "http://ep2:5555"},
			{URL: "http://ep3:5555"},
		}
		event := NewHAProxyPodsDiscoveredEvent(endpoints, 3)
		require.NotNil(t, event)
		assert.Len(t, event.Endpoints, 3)
		assert.Equal(t, 3, event.Count)
		assert.Equal(t, EventTypeHAProxyPodsDiscovered, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("HAProxyPodsDiscoveredEvent_DefensiveCopy", func(t *testing.T) {
		endpoints := []dataplane.Endpoint{{URL: "http://ep1:5555"}}
		event := NewHAProxyPodsDiscoveredEvent(endpoints, 1)

		// Modify original
		endpoints[0] = dataplane.Endpoint{URL: "http://modified:5555"}

		// Event should have original value
		assert.Equal(t, "http://ep1:5555", event.Endpoints[0].URL)
	})

	t.Run("HAProxyPodsDiscoveredEvent_EmptyEndpoints", func(t *testing.T) {
		event := NewHAProxyPodsDiscoveredEvent(nil, 0)
		require.NotNil(t, event)
		assert.Nil(t, event.Endpoints)
		assert.Equal(t, 0, event.Count)
	})

	t.Run("HAProxyPodTerminatedEvent", func(t *testing.T) {
		event := NewHAProxyPodTerminatedEvent("pod-1", "default", "uid-1")
		require.NotNil(t, event)
		assert.Equal(t, "pod-1", event.PodName)
		assert.Equal(t, "default", event.PodNamespace)
		assert.Equal(t, "uid-1", event.PodUID)
		assert.Equal(t, EventTypeHAProxyPodTerminated, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})
}

func TestPublishingEvents(t *testing.T) {
	t.Run("ConfigPublishedEvent", func(t *testing.T) {
		event := NewConfigPublishedEvent("my-config", "default", 5, 2)
		require.NotNil(t, event)
		assert.Equal(t, "my-config", event.RuntimeConfigName)
		assert.Equal(t, "default", event.RuntimeConfigNamespace)
		assert.Equal(t, 5, event.MapFileCount)
		assert.Equal(t, 2, event.SecretCount)
		assert.Equal(t, EventTypeConfigPublished, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ConfigAppliedToPodEvent", func(t *testing.T) {
		syncMetadata := &SyncMetadata{
			ReloadTriggered: true,
			SyncDuration:    100 * time.Millisecond,
			OperationCounts: OperationCounts{
				TotalAPIOperations: 10,
				BackendsAdded:      2,
				ServersAdded:       5,
			},
		}
		event := NewConfigAppliedToPodEvent(
			"my-config",
			"default",
			"pod-1",
			"haproxy",
			"uid-1",
			"runtime-1",
			"checksum-abc",
			false,
			syncMetadata,
		)
		require.NotNil(t, event)
		assert.Equal(t, "my-config", event.RuntimeConfigName)
		assert.Equal(t, "default", event.RuntimeConfigNamespace)
		assert.Equal(t, "pod-1", event.PodName)
		assert.Equal(t, "haproxy", event.PodNamespace)
		assert.Equal(t, "uid-1", event.PodUID)
		assert.Equal(t, "runtime-1", event.PodRuntimeID)
		assert.Equal(t, "checksum-abc", event.Checksum)
		assert.False(t, event.IsDriftCheck)
		assert.NotNil(t, event.SyncMetadata)
		assert.True(t, event.SyncMetadata.ReloadTriggered)
		assert.Equal(t, 10, event.SyncMetadata.OperationCounts.TotalAPIOperations)
		assert.Equal(t, EventTypeConfigAppliedToPod, event.EventType())
		assert.False(t, event.Timestamp().IsZero())
	})

	t.Run("ConfigAppliedToPodEvent_DriftCheck", func(t *testing.T) {
		event := NewConfigAppliedToPodEvent(
			"my-config", "default", "pod-1", "haproxy", "uid-1", "runtime-1", "checksum-abc", true, nil)
		require.NotNil(t, event)
		assert.True(t, event.IsDriftCheck)
		assert.Nil(t, event.SyncMetadata)
	})
}

func TestCorrelation(t *testing.T) {
	t.Run("NewCorrelation_NoOptions", func(t *testing.T) {
		c := newCorrelation()
		assert.NotEmpty(t, c.EventID())
		assert.Empty(t, c.CorrelationID())
		assert.Empty(t, c.CausationID())
	})

	t.Run("NewCorrelation_WithNewCorrelation", func(t *testing.T) {
		c := newCorrelation(WithNewCorrelation())
		assert.NotEmpty(t, c.EventID())
		assert.NotEmpty(t, c.CorrelationID())
		assert.Empty(t, c.CausationID())
	})

	t.Run("NewCorrelation_WithCorrelation", func(t *testing.T) {
		c := newCorrelation(WithCorrelation("corr-123", "cause-456"))
		assert.NotEmpty(t, c.EventID())
		assert.Equal(t, "corr-123", c.CorrelationID())
		assert.Equal(t, "cause-456", c.CausationID())
	})

	t.Run("NewCorrelation_MultipleOptions", func(t *testing.T) {
		// WithCorrelation should override WithNewCorrelation
		c := newCorrelation(WithNewCorrelation(), WithCorrelation("corr-123", "cause-456"))
		assert.NotEmpty(t, c.EventID())
		assert.Equal(t, "corr-123", c.CorrelationID())
		assert.Equal(t, "cause-456", c.CausationID())
	})

	t.Run("NewCorrelation_UniqueEventIDs", func(t *testing.T) {
		c1 := newCorrelation()
		c2 := newCorrelation()
		assert.NotEqual(t, c1.EventID(), c2.EventID())
	})

	t.Run("PropagateCorrelation_FromCorrelatedEvent", func(t *testing.T) {
		// Create a source event with correlation
		sourceEvent := NewReconciliationTriggeredEvent("test", true, WithNewCorrelation())
		sourceCorrelationID := sourceEvent.CorrelationID()
		sourceEventID := sourceEvent.EventID()

		// Propagate correlation to a new event
		newEvent := NewReconciliationStartedEvent("test",
			PropagateCorrelation(sourceEvent))

		// New event should have same correlation ID
		assert.Equal(t, sourceCorrelationID, newEvent.CorrelationID())
		assert.Equal(t, sourceEventID, newEvent.CausationID())
		// New event should have its own unique event ID
		assert.NotEqual(t, sourceEventID, newEvent.EventID())
	})

	t.Run("PropagateCorrelation_FromNonCorrelatedEvent", func(t *testing.T) {
		// Create a non-correlated source (just an any)
		source := "not a correlated event"

		opt := PropagateCorrelation(source)

		// Option should be empty
		assert.Empty(t, opt.correlationID)
		assert.Empty(t, opt.causationID)

		c := newCorrelation(opt)
		assert.Empty(t, c.CorrelationID())
		assert.Empty(t, c.CausationID())
	})

	t.Run("PropagateCorrelation_ChainPropagation", func(t *testing.T) {
		// Simulate a chain: Event1 -> Event2 -> Event3
		event1 := NewReconciliationTriggeredEvent("start", true, WithNewCorrelation())

		event2 := NewReconciliationStartedEvent("middle",
			PropagateCorrelation(event1))

		event3 := NewReconciliationCompletedEvent(100, "", nil, nil,
			PropagateCorrelation(event2))

		// All events should share the same correlation ID
		assert.Equal(t, event1.CorrelationID(), event2.CorrelationID())
		assert.Equal(t, event2.CorrelationID(), event3.CorrelationID())

		// Each event should have unique event ID
		assert.NotEqual(t, event1.EventID(), event2.EventID())
		assert.NotEqual(t, event2.EventID(), event3.EventID())

		// Causation chain: event1 -> event2 -> event3
		assert.Equal(t, event1.EventID(), event2.CausationID())
		assert.Equal(t, event2.EventID(), event3.CausationID())
	})

	t.Run("WithCorrelation_EmptyValues", func(t *testing.T) {
		opt := WithCorrelation("", "")
		c := newCorrelation(opt)
		assert.Empty(t, c.CorrelationID())
		assert.Empty(t, c.CausationID())
	})
}

// TestTimestampNotZero is a comprehensive test ensuring all events have non-zero timestamps.
func TestTimestampNotZero(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name  string
		event interface {
			Timestamp() time.Time
		}
	}{
		{"ConfigParsed", NewConfigParsedEvent(nil, nil, "v1", "v2")},
		{"ConfigValidationRequest", NewConfigValidationRequest(nil, "v1")},
		{"ConfigValidationResponse", NewConfigValidationResponse("req", "validator", true, nil)},
		{"ConfigValidated", NewConfigValidatedEvent(nil, nil, "v1", "v2")},
		{"ConfigInvalid", NewConfigInvalidEvent("v1", nil, nil)},
		{"ConfigResourceChanged", NewConfigResourceChangedEvent(nil)},
		{"ResourceIndexUpdated", NewResourceIndexUpdatedEvent("type", types.ChangeStats{})},
		{"ResourceSyncComplete", NewResourceSyncCompleteEvent("type", 0)},
		{"IndexSynchronized", NewIndexSynchronizedEvent(nil)},
		{"LeaderElectionStarted", NewLeaderElectionStartedEvent("id", "lease", "ns")},
		{"BecameLeader", NewBecameLeaderEvent("id")},
		{"LostLeadership", NewLostLeadershipEvent("id", "reason")},
		{"NewLeaderObserved", NewNewLeaderObservedEvent("id", false)},
		{"ReconciliationTriggered", NewReconciliationTriggeredEvent("reason", true)},
		{"ReconciliationStarted", NewReconciliationStartedEvent("trigger")},
		{"ReconciliationCompleted", NewReconciliationCompletedEvent(0, "", nil, nil)},
		{"ReconciliationFailed", NewReconciliationFailedEvent("error", "phase", nil)},
		{"SecretResourceChanged", NewSecretResourceChangedEvent(nil)},
		{"CredentialsUpdated", NewCredentialsUpdatedEvent(nil, "v1")},
		{"HTTPResourceUpdated", NewHTTPResourceUpdatedEvent("url", "checksum", 0)},
		{"HTTPResourceAccepted", NewHTTPResourceAcceptedEvent("url", "checksum", 0)},
		{"HTTPContentAcceptanceRequested", NewHTTPContentAcceptanceRequestedEvent()},
		{"HTTPContentRevoked", NewHTTPContentRevokedEvent("url", "checksum", false, false)},
		{"BlobContentRevoked", NewBlobContentRevokedEvent("url", "digest", false)},
		{"BlobFetchFailed", NewBlobFetchFailedEvent("url", "error")},
		{"TemplateRendered", NewTemplateRenderedEvent("cfg", nil, nil, nil, 0, 0, "", "", nil, "", true)},
		{"TemplateRenderFailed", NewTemplateRenderFailedEvent("name", "error", "stack")},
		{"RenderGateCompleted", NewRenderGateCompletedEvent("plan-1", true, false, true, "", false, 0)},
		{"ValidationFailed", NewValidationFailedEvent(nil, 0, "")},
		{"DeploymentStarted", NewDeploymentStartedEvent(0)},
		{"InstanceDeploymentFailed", NewInstanceDeploymentFailedEvent(nil, "error", false)},
		{"DeploymentCompleted", NewDeploymentCompletedEvent(&DeploymentResult{})},
		{"DeploymentScheduled", NewDeploymentScheduledEvent("cfg", nil, nil, "n", "ns", "r", "", nil, "", nil, true)},
		{"DriftPreventionTriggered", NewDriftPreventionTriggeredEvent(0)},
		// Discovery events
		{"HAProxyPodsDiscovered", NewHAProxyPodsDiscoveredEvent(nil, 0)},
		{"HAProxyPodTerminated", NewHAProxyPodTerminatedEvent("name", "ns", "uid")},
		// Publishing events
		{"ConfigPublished", NewConfigPublishedEvent("n", "ns", 0, 0)},
		{"ConfigAppliedToPod", NewConfigAppliedToPodEvent("n", "ns", "pod", "ns", "uid", "runtime", "checksum", false, nil)},
		{"StatusUpdateCompleted", NewStatusUpdateCompletedEvent(StatusPatchPhaseDeployed, 3, 1, 50)},
		{"StatusUpdateFailed", NewStatusUpdateFailedEvent("ns", "name", "networking.k8s.io/v1/ingresses", "error", true)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := tt.event.Timestamp()
			assert.False(t, ts.IsZero(), "Timestamp should not be zero")
			assert.True(t, !ts.Before(now), "Timestamp should be >= test start time")
		})
	}
}
