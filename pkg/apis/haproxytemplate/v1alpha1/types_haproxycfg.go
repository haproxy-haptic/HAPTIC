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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hpcfg,scope=Namespaced
// +kubebuilder:printcolumn:name="Checksum",type=string,JSONPath=`.spec.checksum`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// HAProxyCfg contains the rendered HAProxy configuration for a specific
// HAProxyTemplateConfig.
//
// This is a read-only resource automatically created and updated by the controller
// to expose the actual runtime configuration applied to HAProxy pods.
type HAProxyCfg struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HAProxyCfgSpec   `json:"spec,omitempty"`
	Status HAProxyCfgStatus `json:"status,omitempty"`
}

// HAProxyCfgSpec contains the rendered configuration content.
type HAProxyCfgSpec struct {
	// Path is the file system path where this configuration is stored.
	//
	// Default: /etc/haproxy/haproxy.cfg
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// Content is the rendered HAProxy configuration file content.
	//
	// This is the actual haproxy.cfg content that was validated and deployed
	// to HAProxy pods.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Content string `json:"content"`

	// Checksum is the SHA-256 hash of the original (uncompressed) configuration content.
	//
	// Used to detect configuration changes and verify consistency across pods.
	// Format: sha256:<hex-digest>
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Checksum string `json:"checksum"`

	// Compressed indicates the content is zstd+base64 encoded.
	//
	// When true, consumers must decompress before use.
	// +optional
	Compressed bool `json:"compressed,omitempty"`

	// RetainedPlan contains zstd+base64 encoded deployment declarations for a retained snapshot.
	// +optional
	RetainedPlan string `json:"retainedPlan,omitempty"`
}

// HAProxyCfgStatus tracks deployment state and auxiliary files.
type HAProxyCfgStatus struct {
	// RetainedConfigs references complete, acknowledged snapshots, newest first.
	// +optional
	RetainedConfigs []RetainedConfigReference `json:"retainedConfigs,omitempty"`

	// DeployedToPods tracks which HAProxy pods currently have this configuration.
	//
	// Pods are automatically added when configuration is applied and removed when
	// the pod terminates.
	//
	// The list is keyed by podName so each pod's status update can land via
	// Server-Side Apply with its own field manager — concurrent updates from
	// different pods merge naturally instead of last-write-wins on the
	// full-object UpdateStatus path that preceded this CRD shape.
	// +optional
	// +listType=map
	// +listMapKey=podName
	DeployedToPods []PodDeploymentStatus `json:"deployedToPods,omitempty"`

	// AuxiliaryFiles references the associated map files and certificates.
	// +optional
	AuxiliaryFiles *AuxiliaryFileReferences `json:"auxiliaryFiles,omitempty"`

	// ValidationError contains the error message if this configuration failed validation.
	//
	// Only populated for HAProxyCfg resources published with the -invalid suffix.
	// When present, this configuration was not deployed to HAProxy instances.
	// +optional
	ValidationError string `json:"validationError,omitempty"`

	// ObservedGeneration reflects the generation of the spec that was most recently processed.
	//
	// This is used to track whether status is up-to-date with latest spec changes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of the resource's state.
	//
	// Standard conditions include:
	// - "Synced": Configuration has been successfully applied to all target pods
	// - "Ready": Resource is ready for use
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// RetainedConfigReference binds a recovery checkpoint to one object incarnation.
type RetainedConfigReference struct {
	Name     string `json:"name"`
	UID      string `json:"uid"`
	Checksum string `json:"checksum"`
}

// PodDeploymentStatus tracks deployment to a specific pod.
type PodDeploymentStatus struct {
	// PodName is the name of the HAProxy pod.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	PodName string `json:"podName"`

	// PodUID binds the deployment proof to the Kubernetes pod incarnation.
	// +optional
	PodUID string `json:"podUID,omitempty"`

	// PodRuntimeID binds the deployment proof to the pod's container execution epoch.
	// +optional
	PodRuntimeID string `json:"podRuntimeID,omitempty"`

	// Checksum of the configuration deployed to this pod.
	// +optional
	Checksum string `json:"checksum,omitempty"`

	// AppliedPlanID is the render plan this pod last accepted.
	// +optional
	AppliedPlanID string `json:"appliedPlanID,omitempty"`

	// RunningPlanID is the render plan this pod's running HAProxy serves.
	// +optional
	RunningPlanID string `json:"runningPlanID,omitempty"`

	// WorkerOpsPlanID identifies the configuration after accepted runtime operations.
	// +optional
	WorkerOpsPlanID string `json:"workerOpsPlanID,omitempty"`

	// Mode is how the plan was applied to this pod.
	// +optional
	// +kubebuilder:validation:Enum=runtime;file_only;reload;scheduled;noop;rejected
	Mode string `json:"mode,omitempty"`

	// Reasons explain the apply mode, most significant first.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	Reasons []string `json:"reasons,omitempty"`

	// LastError contains the error message from the most recent failed sync attempt.
	//
	// This field is cleared when a sync succeeds. Combined with ConsecutiveErrors,
	// this helps identify persistent vs transient issues.
	// +optional
	LastError string `json:"lastError,omitempty"`

	// ConsecutiveErrors is the count of consecutive sync failures.
	//
	// This counter increments on each failure and resets to 0 on success.
	// High values indicate persistent problems requiring investigation.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ConsecutiveErrors int `json:"consecutiveErrors,omitempty"`
}

// AuxiliaryFileReferences references the associated maps, certificates, CA files, general files, crt-lists, and secret files.
type AuxiliaryFileReferences struct {
	// SetID identifies the auxiliary publication committed with these references.
	// +optional
	SetID string `json:"setID,omitempty"`

	// MapFiles lists the HAProxyMapFile resources associated with this config.
	// +optional
	MapFiles []ResourceReference `json:"mapFiles,omitempty"`

	// SSLCertificates lists the Secret resources containing SSL certificates.
	// +optional
	SSLCertificates []ResourceReference `json:"sslCertificates,omitempty"`

	// SSLCaFiles lists the Secret resources containing SSL CA files.
	// +optional
	SSLCaFiles []ResourceReference `json:"sslCaFiles,omitempty"`

	// GeneralFiles lists the HAProxyGeneralFile resources associated with this config.
	// +optional
	GeneralFiles []ResourceReference `json:"generalFiles,omitempty"`

	// CRTListFiles lists the HAProxyCRTListFile resources associated with this config.
	// +optional
	CRTListFiles []ResourceReference `json:"crtListFiles,omitempty"`

	// SecretFiles lists the Secret resources containing secret files.
	// +optional
	SecretFiles []ResourceReference `json:"secretFiles,omitempty"`
}

// ResourceReference identifies a related Kubernetes resource.
type ResourceReference struct {
	// Kind is the resource type (e.g., HAProxyMapFile, Secret).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name is the resource name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the resource namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// HAProxyCfgList contains a list of HAProxyCfg.
type HAProxyCfgList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HAProxyCfg `json:"items"`
}
