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

package configpublisher

import (
	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	listersv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/generated/listers/haproxytemplate/v1alpha1"

	"k8s.io/apimachinery/pkg/types"
)

// Listers holds the informer-backed listers for cached reads.
// These avoid direct API calls when checking if updates are needed.
type Listers struct {
	MapFiles     listersv1alpha1.HAProxyMapFileLister
	GeneralFiles listersv1alpha1.HAProxyGeneralFileLister
	CRTListFiles listersv1alpha1.HAProxyCRTListFileLister
	HAProxyCfgs  listersv1alpha1.HAProxyCfgLister
}

// AuxiliaryFiles contains maps, certificates, CA files, general files, crt-lists, and secret files.
type AuxiliaryFiles struct {
	// MapFiles contains HAProxy map files.
	MapFiles []auxiliaryfiles.MapFile

	// SSLCertificates contains SSL/TLS certificate files.
	SSLCertificates []auxiliaryfiles.SSLCertificate

	// SSLCaFiles contains SSL CA certificate files for client/backend certificate verification.
	SSLCaFiles []auxiliaryfiles.SSLCaFile

	// GeneralFiles contains general-purpose files (error pages, etc.).
	GeneralFiles []auxiliaryfiles.GeneralFile

	// CRTListFiles contains crt-list files for SSL certificate lists.
	CRTListFiles []auxiliaryfiles.CRTListFile

	// SecretFiles contains files published as Secrets for sidecars.
	SecretFiles []auxiliaryfiles.SecretFile
}

func (f *AuxiliaryFiles) kinds() []string {
	var kinds []string
	for _, present := range []struct {
		kind  string
		files bool
	}{
		{kindMapFile, len(f.MapFiles) > 0},
		{kindGeneralFile, len(f.GeneralFiles) > 0},
		{kindCRTListFile, len(f.CRTListFiles) > 0},
		{kindSecret, len(f.SSLCertificates)+len(f.SSLCaFiles)+len(f.SecretFiles) > 0},
	} {
		if present.files {
			kinds = append(kinds, present.kind)
		}
	}
	return kinds
}

// PublishRequest contains the information needed to publish HAProxy runtime configuration.
type PublishRequest struct {
	// TemplateConfigName is the name of the HAProxyTemplateConfig that generated this config.
	TemplateConfigName string

	// TemplateConfigNamespace is the namespace of the HAProxyTemplateConfig.
	TemplateConfigNamespace string

	// TemplateConfigUID is the UID of the HAProxyTemplateConfig (for ownerReferences).
	TemplateConfigUID types.UID

	// Config is the rendered HAProxy configuration content.
	Config string

	// OutputSnapshot binds the production config, plan, and auxiliary artifacts.
	OutputSnapshot *renderoutput.Snapshot

	// ConfigPath is the file system path where the config is stored.
	// Default: /etc/haproxy/haproxy.cfg
	ConfigPath string

	// AuxiliaryFiles contains map files, SSL certificates, and general files.
	AuxiliaryFiles *AuxiliaryFiles

	// AuxiliaryFileSnapshot is the immutable compatibility carrier used before
	// the complete output root reaches this boundary.
	AuxiliaryFileSnapshot *renderartifact.Snapshot

	// Checksum is the SHA-256 hash of the configuration content.
	Checksum string

	// NameSuffix appends to the generated resource name (e.g., "-invalid").
	// Used to publish separate resources for invalid configurations.
	// +optional
	NameSuffix string

	// Force republishes even when the content matches the last successful
	// publication — the drift-prevention path's authoritative re-write.
	Force bool

	// ValidationError contains the error message if this configuration failed validation.
	// When set, this indicates the configuration is invalid and should not be deployed.
	// +optional
	ValidationError string

	// CompressionThreshold is the payload size in bytes above which configs are compressed.
	// It includes the encoded recovery plan when one is present.
	// Set to 0 or negative to disable compression.
	// Set to a positive value (e.g., 1048576 for 1 MiB) to enable compression.
	CompressionThreshold int64

	// RetainedPlan is present only on a recovery snapshot.
	RetainedPlan string

	// ConfirmedPod proves that a worker loaded OutputSnapshot.
	ConfirmedPod *haproxyv1alpha1.PodDeploymentStatus

	auxiliarySetID string
	auxiliaryClaim string
	// listedChildren answers each child's first read; nil reads it live.
	listedChildren *childListing
}

// PublishResult contains the result of publishing configuration resources.
type PublishResult struct {
	// RuntimeConfigName is the name of the created/updated HAProxyRuntimeConfig.
	RuntimeConfigName string

	// RuntimeConfigNamespace is the namespace of the HAProxyRuntimeConfig.
	RuntimeConfigNamespace string

	// MapFileNames lists the names of created/updated HAProxyMapFile resources.
	MapFileNames []string

	// SecretNames lists the names of created/updated certificate Secrets.
	SecretNames []string

	// SSLCaFileNames lists the names of created/updated CA Secrets.
	SSLCaFileNames []string

	// GeneralFileNames lists the names of created/updated HAProxyGeneralFile resources.
	GeneralFileNames []string

	// CRTListFileNames lists the names of created/updated HAProxyCRTListFile resources.
	CRTListFileNames []string

	// SecretFileNames lists the names of created/updated secret-file Secrets.
	SecretFileNames []string
}

// DeploymentStatusUpdate contains information about a configuration deployment to a pod.
type DeploymentStatusUpdate struct {
	// RuntimeConfigName is the name of the HAProxyRuntimeConfig to update.
	RuntimeConfigName string

	// RuntimeConfigNamespace is the namespace of the HAProxyRuntimeConfig.
	RuntimeConfigNamespace string

	// PodName is the name of the HAProxy pod that received the configuration.
	PodName string

	// PodUID identifies the HAProxy pod incarnation that received the configuration.
	PodUID string

	// PodRuntimeID identifies the container execution epoch that received the configuration.
	PodRuntimeID string

	// Checksum is the checksum of the configuration deployed to the pod.
	Checksum string

	// AppliedPlanID is the render plan the pod accepted.
	AppliedPlanID string

	// RunningPlanID is the render plan the pod's running HAProxy serves.
	RunningPlanID string

	// Mode is how the plan was applied.
	Mode string

	// Reasons explain Mode, most significant first. Written truncated to
	// maxPodStatusReasons.
	Reasons []string

	// Error contains the error message if sync failed.
	// Empty string indicates success.
	Error string

	// IsDriftCheck marks a periodic drift-prevention re-sync. Such an update
	// reads the HAProxyCfg and its auxiliary files live and re-applies the pod's
	// entry wherever an object no longer shows the pod's last identical apply,
	// so an out-of-band change self-heals within one drift interval.
	IsDriftCheck bool
}

// PodCleanupRequest contains information about a terminated pod to clean up.
type PodCleanupRequest struct {
	// PodName is the name of the terminated pod.
	PodName string

	// PodUID identifies the terminated pod incarnation.
	PodUID string

	// Namespace is the namespace where HAProxyCfg resources should be looked up.
	// This ensures namespace-scoped cleanup operations.
	Namespace string
}

// PodIdentity identifies a currently authoritative HAProxy pod incarnation.
type PodIdentity struct {
	// PodName is the stable Kubernetes pod name.
	PodName string
	// PodUID identifies the current incarnation of PodName.
	PodUID string
	// PodRuntimeID identifies the current container execution epoch of PodName.
	PodRuntimeID string
}
