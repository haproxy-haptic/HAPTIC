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
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"

	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
)

// ConfigParsedEvent is published when the HAProxyTemplateConfig CRD has been
// successfully parsed into a Config structure.
//
// This event does not mean the config is valid - only that it could be parsed.
// Validation occurs in a subsequent step.
// ConfigSourceRef identifies one HAProxyTemplateConfig of the merged set, at
// the generation the merge observed. Status is stamped on EVERY source: the
// verdict is a property of the merged set, and observedGeneration is only
// meaningful against the same object's metadata.generation — a designated
// primary could not even represent "your shard edit was observed" (ADR-0016).
type ConfigSourceRef struct {
	Namespace       string
	Name            string
	Generation      int64
	UID             types.UID
	ResourceVersion string
}

type ConfigParsedEvent struct {
	// Config contains the parsed configuration.
	// Type: any to avoid circular dependencies.
	// Consumers should type-assert to their expected config type.
	Config any

	// TemplateConfig is the original HAProxyTemplateConfig CRD.
	// Type: any to avoid circular dependencies.
	// Needed by ConfigPublisher to extract Kubernetes metadata (name, namespace, UID).
	TemplateConfig any

	// Version is the resourceVersion of the HAProxyTemplateConfig CRD.
	Version string

	// SecretVersion is the resourceVersion of the credentials Secret.
	SecretVersion string

	// Sources names every config of the merged set, in merge order.
	Sources []ConfigSourceRef

	timestamped
}

// NewConfigParsedEvent creates a new ConfigParsedEvent.
func NewConfigParsedEvent(config, templateConfig any, version, secretVersion string) *ConfigParsedEvent {
	return &ConfigParsedEvent{
		Config:         config,
		TemplateConfig: templateConfig,
		Version:        version,
		SecretVersion:  secretVersion,
		timestamped:    newTimestamped(),
	}
}

func (e *ConfigParsedEvent) EventType() string { return EventTypeConfigParsed }

// ConfigValidationRequest is published to request validation of a parsed config.
//
// This is a Request event used in the scatter-gather pattern. Multiple validators
// (basic, template, jsonpath) will respond with ConfigValidationResponse events.
type ConfigValidationRequest struct {
	reqID string

	// Config contains the configuration to validate.
	Config any

	// Version is the resourceVersion being validated.
	Version string

	timestamped
}

// NewConfigValidationRequest creates a new ConfigValidationRequest.
func NewConfigValidationRequest(config any, version string) *ConfigValidationRequest {
	return &ConfigValidationRequest{
		reqID:       fmt.Sprintf("config-validation-%s-%d", version, time.Now().UnixNano()),
		Config:      config,
		Version:     version,
		timestamped: newTimestamped(),
	}
}

func (e *ConfigValidationRequest) EventType() string { return EventTypeConfigValidationRequest }
func (e *ConfigValidationRequest) RequestID() string { return e.reqID }

// ConfigValidationResponse is sent by validators in response to ConfigValidationRequest.
//
// This is a Response event used in the scatter-gather pattern.
// pkg/controller/configchange.ConfigChangeHandler issues the request,
// collects all responses, and determines if the config is valid overall.
type ConfigValidationResponse struct {
	reqID     string
	responder string

	// ValidatorName identifies which validator produced this response (basic, template, jsonpath).
	ValidatorName string

	// Valid is true if this validator found no errors.
	Valid bool

	// Errors contains validation error messages, empty if Valid is true.
	Errors []string

	// Inconclusive means the validator could not judge the config (for
	// example, schemas were unavailable); Errors then say why. Never set
	// together with Valid.
	Inconclusive bool

	timestamped
}

// NewConfigValidationInconclusiveResponse reports that a validator could not
// reach a verdict, so the config is neither accepted nor rejected.
func NewConfigValidationInconclusiveResponse(requestID, validatorName string, reasons []string) *ConfigValidationResponse {
	response := NewConfigValidationResponse(requestID, validatorName, false, reasons)
	response.Inconclusive = true
	return response
}

// NewConfigValidationResponse creates a new ConfigValidationResponse.
// Performs defensive copy of the errors slice.
func NewConfigValidationResponse(requestID, validatorName string, valid bool, errors []string) *ConfigValidationResponse {
	return &ConfigValidationResponse{
		reqID:         requestID,
		responder:     validatorName,
		ValidatorName: validatorName,
		Valid:         valid,
		Errors:        copySlice(errors),
		timestamped:   newTimestamped(),
	}
}

func (e *ConfigValidationResponse) EventType() string { return EventTypeConfigValidationResponse }
func (e *ConfigValidationResponse) RequestID() string { return e.reqID }
func (e *ConfigValidationResponse) Responder() string { return e.responder }

// ConfigValidatedEvent is published when all validators have confirmed the config is valid.
//
// After receiving this event, the controller proceeds to start resource watchers.
// with the validated configuration.
type ConfigValidatedEvent struct {
	Config any

	// TemplateConfig is the original HAProxyTemplateConfig CRD.
	// Type: any to avoid circular dependencies.
	// Needed by ConfigPublisher to extract Kubernetes metadata (name, namespace, UID).
	TemplateConfig any

	Version       string
	SecretVersion string

	// CandidateGeneration identifies a live config-change validation owned by
	// ConfigChangeHandler. Zero denotes state published outside that handler.
	CandidateGeneration uint64

	// ActiveSnapshotRestore replays the running iteration's state after a newer
	// parsed generation retires an accepted candidate. State consumers apply it;
	// validation and reload consumers must not treat it as a new verdict.
	ActiveSnapshotRestore bool

	// VerdictSequence orders the verdicts one iteration reaches on the
	// configuration: a larger value is newer, and zero is not a verdict.
	VerdictSequence uint64

	// Sources names every config of the merged set, in merge order.
	Sources []ConfigSourceRef

	timestamped
}

// NewConfigValidatedEvent creates a new ConfigValidatedEvent.
func NewConfigValidatedEvent(config, templateConfig any, version, secretVersion string) *ConfigValidatedEvent {
	return &ConfigValidatedEvent{
		Config:         config,
		TemplateConfig: templateConfig,
		Version:        version,
		SecretVersion:  secretVersion,
		timestamped:    newTimestamped(),
	}
}

func (e *ConfigValidatedEvent) EventType() string { return EventTypeConfigValidated }

// ConfigInvalidEvent is published when config validation fails.
//
// The controller will continue running with the previous valid config and wait
// for the next HAProxyTemplateConfig CRD update.
type ConfigInvalidEvent struct {
	Version string

	// TemplateConfig is the original HAProxyTemplateConfig CRD.
	// Type: any to avoid circular dependencies.
	// Used by status updater to set validation errors on the CRD status.
	TemplateConfig any

	// ValidationErrors maps validator names to their error messages.
	ValidationErrors map[string][]string

	// VerdictSequence is as on ConfigValidatedEvent.
	VerdictSequence uint64

	// Sources names every config of the merged set, in merge order.
	Sources []ConfigSourceRef

	timestamped
}

// NewConfigInvalidEvent creates a new ConfigInvalidEvent.
// Performs defensive copy of the validation errors map and its slice values.
func NewConfigInvalidEvent(version string, templateConfig any, validationErrors map[string][]string) *ConfigInvalidEvent {
	return &ConfigInvalidEvent{
		Version:          version,
		TemplateConfig:   templateConfig,
		ValidationErrors: copyStringSlicesMap(validationErrors),
		timestamped:      newTimestamped(),
	}
}

func (e *ConfigInvalidEvent) EventType() string { return EventTypeConfigInvalid }

// ConfigValidationInconclusiveEvent is published when validators could not
// judge a config version. The running config stays active and the same
// version is validated again after RetryIn.
type ConfigValidationInconclusiveEvent struct {
	Version        string
	TemplateConfig any
	// Reasons maps validator names to why they could not reach a verdict.
	Reasons map[string][]string
	RetryIn time.Duration
	// VerdictSequence is as on ConfigValidatedEvent.
	VerdictSequence uint64
	Sources         []ConfigSourceRef

	timestamped
}

// NewConfigValidationInconclusiveEvent creates a new ConfigValidationInconclusiveEvent.
func NewConfigValidationInconclusiveEvent(version string, templateConfig any, reasons map[string][]string, retryIn time.Duration) *ConfigValidationInconclusiveEvent {
	return &ConfigValidationInconclusiveEvent{
		Version:        version,
		TemplateConfig: templateConfig,
		Reasons:        copyStringSlicesMap(reasons),
		RetryIn:        retryIn,
		timestamped:    newTimestamped(),
	}
}

func (e *ConfigValidationInconclusiveEvent) EventType() string {
	return EventTypeConfigValidationInconclusive
}

// ConfigVerdict is a ConfigValidatedEvent, ConfigInvalidEvent or
// ConfigValidationInconclusiveEvent.
type ConfigVerdict interface {
	busevents.Event
	configVerdict()
}

func (e *ConfigValidatedEvent) configVerdict()              {}
func (e *ConfigInvalidEvent) configVerdict()                {}
func (e *ConfigValidationInconclusiveEvent) configVerdict() {}

// ConfigVerdictReplayedEvent restores status without counting or applying a verdict again.
type ConfigVerdictReplayedEvent struct {
	Verdict ConfigVerdict

	timestamped
}

// NewConfigVerdictReplayedEvent creates a new ConfigVerdictReplayedEvent.
func NewConfigVerdictReplayedEvent(verdict ConfigVerdict) *ConfigVerdictReplayedEvent {
	return &ConfigVerdictReplayedEvent{Verdict: verdict, timestamped: newTimestamped()}
}

func (e *ConfigVerdictReplayedEvent) EventType() string { return EventTypeConfigVerdictReplayed }

// ConfigResourceChangedEvent is published when the HAProxyTemplateConfig CRD resource is added, updated, or deleted.
//
// This is a low-level event published directly by the SingleWatcher callback in the controller package.
// The ConfigLoaderComponent subscribes to this event and handles parsing.
type ConfigResourceChangedEvent struct {
	// Resource contains the raw HAProxyTemplateConfig CRD resource.
	// Type: any to avoid circular dependencies.
	// Consumers should type-assert to *unstructured.Unstructured.
	Resource any

	timestamped
}

// NewConfigResourceChangedEvent creates a new ConfigResourceChangedEvent.
func NewConfigResourceChangedEvent(resource any) *ConfigResourceChangedEvent {
	return &ConfigResourceChangedEvent{
		Resource:    resource,
		timestamped: newTimestamped(),
	}
}

func (e *ConfigResourceChangedEvent) EventType() string { return EventTypeConfigResourceChanged }

// LibrarySetChangedEvent carries every HAProxyTemplateLibrary the controller
// can currently see, whenever any of them changes.
//
// It is a whole-set snapshot rather than a per-object delta because the loader
// needs the complete set to decide whether a config's references resolve, and
// because a deletion then needs no representation of its own: the object is
// simply absent from the next snapshot.
type LibrarySetChangedEvent struct {
	// Snippets holds the raw HAProxyTemplateLibrary resources.
	// Type: []any to avoid circular dependencies.
	// Consumers should type-assert elements to *unstructured.Unstructured.
	Snippets []any

	timestamped
}

// NewLibrarySetChangedEvent creates a new LibrarySetChangedEvent.
// Performs a defensive copy of the slice.
func NewLibrarySetChangedEvent(snippets []any) *LibrarySetChangedEvent {
	return &LibrarySetChangedEvent{
		Snippets:    copySlice(snippets),
		timestamped: newTimestamped(),
	}
}

func (e *LibrarySetChangedEvent) EventType() string { return EventTypeLibrarySetChanged }
