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
	"k8s.io/apimachinery/pkg/runtime"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=htplcfg;haptpl,scope=Namespaced
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.validationStatus`
// +kubebuilder:printcolumn:name="Observed",type=integer,JSONPath=`.status.observedGeneration`,description="Spec generation the controller last processed"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// HAProxyTemplateConfig defines the configuration for the HAProxy Template Ingress Controller.
//
// As a custom resource it provides OpenAPI validation, type safety, and support for
// embedded validation tests.
type HAProxyTemplateConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HAProxyTemplateConfigSpec   `json:"spec,omitempty"`
	Status HAProxyTemplateConfigStatus `json:"status,omitempty"`
}

// HAProxyTemplateConfigSpec defines the desired state of HAProxyTemplateConfig.
//
// One object per controller. Bulk template content lives in
// HAProxyTemplateLibrary objects pulled in through LibraryRefs, so this object
// stays small enough to read, diff and `kubectl edit`.
//
// The CEL rule below runs in the apiserver at apply time, where it cannot be
// unreachable the way a webhook can. PodSelector and WatchedResources are
// required unconditionally, because no referenced library can supply them.
// HAProxyConfig may come from a library, so it is required only when nothing is
// referenced. Everything else the controller needs is enforced after the merge
// by config.ValidateStructure.
//
// +kubebuilder:validation:XValidation:rule="has(self.podSelector) && has(self.watchedResources) && size(self.watchedResources) > 0 && (has(self.haproxyConfig) || (has(self.libraryRefs) && size(self.libraryRefs) > 0))",message="a HAProxyTemplateConfig needs podSelector, at least one watchedResources entry, and haproxyConfig — either inline or supplied by a spec.libraryRefs entry"
type HAProxyTemplateConfigSpec struct {
	// LibraryRefs pulls in HAProxyTemplateLibrary objects, in merge order.
	//
	// Earlier entries are overridden by later ones, and this object's own
	// inline content wins last — so the object an operator edits is always the
	// override point, with no dependence on ordering.
	//
	// The controller renders only when every reference resolves to an object
	// whose spec.revision equals the revision named here. Otherwise it keeps
	// serving the last-good configuration, because a partially applied set is
	// not safe to render: libraries deliberately override one another, so a
	// missing member silently changes behaviour rather than removing it.
	// +optional
	// +listType=map
	// +listMapKey=name
	LibraryRefs []LibraryRef `json:"libraryRefs,omitempty"`

	// CredentialsSecretRef references the Secret containing the HAPTIC agent credentials.
	//
	// The Secret must contain the following keys, which keep their historical
	// names so a rotation set up against them keeps working:
	//   - dataplane_username: agent username
	//   - dataplane_password: agent password
	//
	// If the namespace is omitted, it defaults to the same namespace as this config resource.
	// +optional
	CredentialsSecretRef SecretReference `json:"credentialsSecretRef,omitempty"`

	// PodSelector identifies which HAProxy pods to configure.
	//
	// Required in the merged config (config.ValidateStructure rejects an empty
	// matchLabels); optional here only because the merged result is what is checked.
	// +optional
	PodSelector PodSelector `json:"podSelector,omitempty"`

	// Controller contains controller-level settings (ports, leader election, etc.).
	// +optional
	Controller ControllerConfig `json:"controller,omitempty"`

	// Logging configures logging behavior.
	// +optional
	Logging LoggingConfig `json:"logging,omitempty"`

	// Dataplane configures how the controller reaches the HAProxy pods and where
	// their files live. The block keeps its name across the agent cutover.
	// +optional
	Dataplane DataplaneConfig `json:"dataplane,omitempty"`

	// TemplatingSettings configures template rendering behavior and custom variables.
	// +optional
	TemplatingSettings TemplatingSettings `json:"templatingSettings,omitempty"`

	// WatchedResourcesIgnoreFields specifies JSONPath expressions for fields
	// to remove from all watched resources to reduce memory usage.
	//
	// Example: ["metadata.managedFields", "metadata.resourceVersion"]
	// +optional
	WatchedResourcesIgnoreFields []string `json:"watchedResourcesIgnoreFields,omitempty"`

	// WatchedResources maps resource type names to their watch configuration.
	//
	// Each key is a user-defined name for the resource type (e.g., "ingresses", "services").
	// This name is used in templates to access the resources.
	//
	// The merged config must declare at least one (config.ValidateStructure);
	// a single object may declare none.
	// +optional
	WatchedResources map[string]WatchedResource `json:"watchedResources,omitempty"`

	// Validators declares pluggable validator sidecars consulted by the
	// controller before publishing rendered configuration. See
	// `docs/site/docs/operations/pluggable-validators.md`
	// for setup; the wire protocol is at
	// `docs/development/validator-protocol.md`.
	//
	// An empty list (the default) disables pluggable validation — the
	// controller keeps performing its built-in validation only.
	// +optional
	// +listType=map
	// +listMapKey=name
	Validators []ValidatorConfig `json:"validators,omitempty"`

	// MaxProperties bounds the CEL cost estimator: every rule nested under a
	// snippet is priced per possible map entry, and unbounded put the schema
	// 9x over the apiserver's total budget. The bundled chart's largest library uses 206.

	// TemplateSnippets maps snippet names to reusable template fragments.
	//
	// Snippets can be included in other templates using {{ render "name" }}.
	// +kubebuilder:validation:MaxProperties=4096
	// +optional
	TemplateSnippets map[string]TemplateSnippet `json:"templateSnippets,omitempty"`

	// AbsentIncrementalGroups is populated only by offline effective resolution.
	AbsentIncrementalGroups []string `json:"-"`

	// Maps maps map file names to their template definitions.
	//
	// These generate HAProxy map files for backend routing and other features.
	// +optional
	Maps map[string]MapFile `json:"maps,omitempty"`

	// Files maps file names to their template definitions.
	//
	// These generate auxiliary files like custom error pages.
	// +optional
	Files map[string]GeneralFile `json:"files,omitempty"`

	// SSLCertificates maps certificate names to their template definitions.
	//
	// These generate SSL certificate files for HAProxy.
	// +optional
	SSLCertificates map[string]SSLCertificate `json:"sslCertificates,omitempty"`

	// K8sResources maps resource template names to their template
	// definitions. Each entry's rendered output is parsed as one or
	// more Kubernetes resources (multi-doc YAML, separated by `---`)
	// and applied via Server-Side Apply with field manager `haptic`.
	//
	// The controller injects an OwnerReference to the
	// HAProxyTemplateConfig CR (controller=true,
	// blockOwnerDeletion=true) on every applied resource so cascade
	// garbage collection removes them when the CR is deleted (e.g.
	// `helm uninstall`). Resources that disappear from the rendered
	// set across reconciliations are pruned via the
	// `haproxy-haptic.org/managed-by` label the applier injects.
	//
	// Templates have full access to the same engine context as
	// haproxyConfig — `resources`, filters, `templateSnippets`,
	// `fileRegistry`, `extraContext`, etc. — so a k8sResources
	// template can render extension points (`render_glob` patterns)
	// and consume cached state (`shared.ComputeIfAbsent`).
	// +optional
	K8sResources map[string]K8sResource `json:"k8sResources,omitempty"`

	// HAProxyConfig contains the main HAProxy configuration template.
	//
	// Exactly one object in a merged set supplies it (the base library);
	// config.ValidateStructure rejects a merged config whose template is empty.
	// +optional
	HAProxyConfig HAProxyConfig `json:"haproxyConfig,omitempty"`

	// ValidationTests contains embedded validation test definitions.
	//
	// The map key is the test name, which must be unique across the whole
	// merged set: the controller unions tests per source and rejects a name
	// two objects both define, because a silent override would leave one
	// author believing an assertion runs that does not. The reserved
	// `_global` entry is the exception — it is a shared baseline several
	// objects each contribute part of, and its fixtures accumulate.
	//
	// These tests are executed at startup (the load gate), on every live
	// config change, and via the "controller validate" / "preflight" CLI
	// commands before deployment.
	// +optional
	ValidationTests map[string]ValidationTest `json:"validationTests,omitempty"`
}

// SecretReference references a Secret by name and optional namespace.
type SecretReference struct {
	// Name is the name of the Secret.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the namespace of the Secret.
	//
	// If empty, defaults to the same namespace as the HAProxyTemplateConfig.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// PodSelector identifies which HAProxy pods to configure.
type PodSelector struct {
	// MatchLabels are the labels to match HAProxy pods.
	//
	// Example:
	//   app: haproxy
	//   component: loadbalancer
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinProperties=1
	MatchLabels map[string]string `json:"matchLabels"`
}

// ControllerConfig contains controller-level configuration.
type ControllerConfig struct {
	// LeaderElection configures leader election for high availability.
	// +optional
	LeaderElection LeaderElectionConfig `json:"leaderElection,omitempty"`

	// ConfigPublishing configures how rendered configs are stored in CRDs.
	// +optional
	ConfigPublishing ConfigPublishingConfig `json:"configPublishing,omitempty"`

	// RenderGateInterval is the shortest start-to-start spacing of the render
	// gate's `haproxy -c` runs.
	//
	// The gate validates each render off the reconcile wall clock, on its own
	// semaphore slot. This caps its duty cycle so a render storm cannot keep a
	// core busy back-to-back and slow the admission webhook.
	// Format: Go duration string (e.g., "1s", "500ms")
	// Default: 1s
	// +optional
	RenderGateInterval string `json:"renderGateInterval,omitempty"`
}

// LeaderElectionConfig configures leader election for running multiple replicas.
type LeaderElectionConfig struct {
	// Enabled determines whether leader election is active.
	//
	// If false, the controller assumes it is the sole instance (single-replica mode).
	// Default: true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// LeaseName is the name of the Lease resource used for coordination.
	//
	// Default: haptic-leader
	// +kubebuilder:validation:MinLength=1
	// +optional
	LeaseName string `json:"leaseName,omitempty"`

	// LeaseDuration is the duration that non-leader candidates will wait
	// to force acquire leadership (measured against time of last observed ack).
	//
	// Format: Go duration string (e.g., "15s", "30s")
	// Default: 30s (DefaultLeaderElectionLeaseDuration in pkg/core/config/defaults.go;
	// the Helm chart's values.yaml sets the same value at controller.config.controller.leaderElection.leaseDuration)
	// +optional
	LeaseDuration string `json:"leaseDuration,omitempty"`

	// RenewDeadline is the duration that the acting leader will retry
	// refreshing leadership before giving up.
	//
	// Format: Go duration string (e.g., "20s")
	// Default: 20s (DefaultLeaderElectionRenewDeadline)
	// Must be less than LeaseDuration
	// +optional
	RenewDeadline string `json:"renewDeadline,omitempty"`

	// RetryPeriod is the duration the LeaderElector clients should wait
	// between tries of actions.
	//
	// Format: Go duration string (e.g., "5s")
	// Default: 5s (DefaultLeaderElectionRetryPeriod)
	// Must be less than RenewDeadline
	// +optional
	RetryPeriod string `json:"retryPeriod,omitempty"`
}

// ConfigPublishingConfig configures how rendered configs are stored in CRDs.
type ConfigPublishingConfig struct {
	// CompressionThreshold is the payload size in bytes above which configs are compressed.
	// It includes the encoded recovery plan when one is present.
	//
	// Compression uses zstd+base64 encoding.
	//
	// Default: 1048576 (1 MiB)
	// A value of 0 is treated as unset and the default applies.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1048576
	// +optional
	CompressionThreshold int64 `json:"compressionThreshold,omitempty"`
}

// LoggingConfig configures logging behavior.
type LoggingConfig struct {
	// Level controls the log level.
	//
	// Values: TRACE, DEBUG, INFO, WARN, ERROR (case-insensitive)
	//
	// If not set, the LOG_LEVEL environment variable is used.
	// If neither is set, defaults to INFO.
	// +kubebuilder:validation:Enum=TRACE;DEBUG;INFO;WARN;ERROR;trace;debug;info;warn;error;""
	// +optional
	Level string `json:"level,omitempty"`
}

// DataplaneConfig configures how the controller reaches the HAProxy pods and
// where their files live. The block keeps its name across the agent cutover.
type DataplaneConfig struct {
	// Port is the agent's apply/state API port on each HAProxy pod.
	//
	// Default: 5555
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int `json:"port,omitempty"`

	// MinDeploymentInterval is the shortest interval between two HAProxy
	// reloads of one pod. The chart passes it to the HAPTIC agent as
	// --reload-interval-min: a reload inside the window is scheduled, never
	// dropped, and the controller holds further renders until it fires. Runtime
	// applies are never paced by it. At most 60s.
	//
	// Format: Go duration string (e.g., "5s", "500ms")
	// Default: 5s
	// +optional
	MinDeploymentInterval string `json:"minDeploymentInterval,omitempty"`

	// DriftPreventionInterval triggers periodic deployments to prevent configuration drift.
	//
	// A deployment is automatically triggered if no deployment has occurred within this interval.
	// This detects and corrects a pod whose file tree drifted from the render.
	// Format: Go duration string (e.g., "60s", "5m")
	// Default: 60s
	// +optional
	DriftPreventionInterval string `json:"driftPreventionInterval,omitempty"`

	// MapsDir is the directory for HAProxy map files.
	//
	// Used for both validation and deployment.
	// Default: /etc/haproxy/maps
	// +optional
	MapsDir string `json:"mapsDir,omitempty"`

	// SSLCertsDir is the directory for SSL certificates.
	//
	// Used for both validation and deployment.
	// Default: /etc/haproxy/certs (the Helm chart sets /etc/haproxy/ssl)
	// +optional
	SSLCertsDir string `json:"sslCertsDir,omitempty"`

	// GeneralStorageDir is the directory for general files (error pages, etc.).
	//
	// Used for both validation and deployment.
	// Default: /etc/haproxy/general
	// +optional
	GeneralStorageDir string `json:"generalStorageDir,omitempty"`

	// ConfigFile is the path to the main HAProxy configuration file.
	//
	// Used for validation.
	// Default: /etc/haproxy/haproxy.cfg
	// +optional
	ConfigFile string `json:"configFile,omitempty"`

	// DeploymentTimeout is the maximum time to wait for a deployment to complete.
	// If exceeded, the scheduler assumes the deployment was lost and retries.
	// This is a safety net for race conditions during leadership transitions.
	// Format: Go duration string (e.g., "30s", "1m")
	// Default: 30s
	// +optional
	DeploymentTimeout string `json:"deploymentTimeout,omitempty"`

	// ConfigPublishInterval throttles how often the rendered HAProxy config is
	// republished as the HAProxyCfg observability CRD. The CRD itself is not on
	// the deployment hot path, but during endpoint churn rewriting the (~500 KB)
	// CRD on every reconciliation creates significant etcd write pressure;
	// throttling republishes reduces that write load while leaving the
	// event-driven push to HAProxy pods untouched.
	//
	// Format: Go duration string (e.g., "10s", "1m").
	// Default: 10s
	// +optional
	ConfigPublishInterval string `json:"configPublishInterval,omitempty"`

	// ReloadVerificationTimeout is how long the HAPTIC agent waits for the
	// HAProxy master to report a reload as finished before it fails the apply
	// and restores the last known good file set. The chart passes it to the
	// agent as --reload-timeout; at most 60s.
	//
	// Format: Go duration string (e.g., "10s", "30s").
	// Default: 60s (the agent's ceiling)
	// +optional
	ReloadVerificationTimeout string `json:"reloadVerificationTimeout,omitempty"`

	// SyncTimeout is the overall timeout for a single Dataplane sync to one
	// HAProxy endpoint. The sync covers parse + diff + raw config push +
	// optional reload-verification. If exceeded, the sync is cancelled and the
	// scheduler retries on the next reconciliation.
	//
	// Format: Go duration string (e.g., "2m", "30s").
	// Default: 2m
	// +optional
	SyncTimeout string `json:"syncTimeout,omitempty"`
}

// TemplatingSettings configures template rendering behavior.
type TemplatingSettings struct {
	// Engine specifies which template engine to use for rendering.
	//
	// Available engines:
	//   - "scriggo" (default): Go template syntax, high performance rendering
	//
	// Default: scriggo
	// +kubebuilder:validation:Enum=scriggo
	// +optional
	Engine string `json:"engine,omitempty"`

	// ExtraContext provides custom variables that are passed to all templates.
	//
	// This allows users to add arbitrary data to the template context without
	// modifying controller code. Values can be any valid JSON type (string, number,
	// boolean, object, array).
	//
	// Example:
	//   extraContext:
	//     debug:
	//       enabled: true
	//     environment: production
	//     customValue: 42
	//
	// Templates can then reference these as: {{ debug.enabled }}, {{ environment }}, etc.
	// +optional
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	ExtraContext runtime.RawExtension `json:"extraContext,omitempty"`

	// TestExtraContext is the extraContext validationTests render with in place of
	// this config's extraContext, so a deployment's values can't change what a
	// test asserts. Library extraContext defaults apply beneath it.
	// +optional
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	TestExtraContext runtime.RawExtension `json:"testExtraContext,omitempty"`
}

// WatchedResource configures watching for a specific Kubernetes resource type.
type WatchedResource struct {
	// APIVersion is the Kubernetes API version (e.g., "networking.k8s.io/v1").
	//
	// Mutually exclusive with apiVersions (exactly one must be set; the
	// controller's config validation enforces this). Equivalent to a
	// one-element apiVersions list.
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// APIVersions is an ordered candidate list of API versions. The
	// controller resolves the entry to the FIRST candidate the apiserver
	// serves and watches that version. Mutually exclusive with apiVersion.
	//
	// Example: ["gateway.networking.k8s.io/v1", "gateway.networking.k8s.io/v1beta1"]
	// +optional
	// +kubebuilder:validation:MinItems=1
	APIVersions []string `json:"apiVersions,omitempty"`

	// Optional marks this resource as non-essential: when NO candidate
	// version is served by the cluster, the watch is dropped and every
	// templateSnippet / validationTest whose requires list names this
	// resource is stripped from the effective config at load time, instead
	// of failing startup.
	// +optional
	Optional bool `json:"optional,omitempty"`

	// Resources is the plural form of the Kubernetes resource type (e.g., "ingresses", "services").
	//
	// This is the name used in RBAC rules and API paths.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Resources string `json:"resources"`

	// EnableValidationWebhook enables admission webhook validation for this resource.
	//
	// When enabled, the controller will validate resources of this type before they're saved.
	// Default: false
	// +optional
	EnableValidationWebhook bool `json:"enableValidationWebhook,omitempty"`

	// IndexBy specifies JSONPath expressions for extracting index keys.
	//
	// Resources are indexed by these values for O(1) lookup in templates.
	//
	// Examples:
	//   - ["metadata.namespace", "metadata.name"]
	//   - ["metadata.labels['kubernetes.io/service-name']"]
	// +optional
	IndexBy []string `json:"indexBy,omitempty"`

	// LabelSelector filters resources by labels (server-side filtering).
	//
	// Equality-only: comma-separated "key=value" pairs.
	// Example: "app=nginx,environment=production"
	// Parsed with the Kubernetes label-selector grammar. Set-based syntax
	// (e.g. "tier in (frontend,api)", "!disabled", "app!=nginx") is NOT supported
	// and is rejected: the configuration fails to load rather than the selector
	// being dropped, which would silently widen the watch to every object of
	// this kind cluster-wide.
	// +optional
	LabelSelector string `json:"labelSelector,omitempty"`

	// FieldSelector filters resources using client-side JSONPath evaluation.
	// Unlike Kubernetes' native fieldSelector (which only supports limited fields),
	// this supports any JSONPath expression.
	//
	// Format: "field.path=value"
	// Example: "spec.ingressClassName=haproxy-internal"
	// +optional
	FieldSelector string `json:"fieldSelector,omitempty"`

	// Store specifies the storage backend for this resource type.
	//
	// Valid values:
	//   - "full": MemoryStore - keeps all resources in memory (faster, higher memory usage)
	//   - "on-demand": CachedStore - fetches resources on-demand with caching (slower, lower memory usage)
	//
	// Default: "full"
	//
	// Use "on-demand" for large resources accessed infrequently (e.g., Secrets).
	// Use "full" for frequently accessed resources (e.g., Ingress, Service, EndpointSlice).
	// +kubebuilder:validation:Enum=full;on-demand
	// +optional
	Store string `json:"store,omitempty"`

	// DebounceInterval overrides the watcher's refractory window.
	// "0" disables debouncing; default 100ms.
	// +optional
	DebounceInterval string `json:"debounceInterval,omitempty"`

	// IgnoreFields lists JSONPath expressions removed from this resource
	// before it is stored, in addition to watchedResourcesIgnoreFields.
	// `[*]` selects every element of an array. An update that changes only
	// ignored fields is not a change: templates never see it and it
	// triggers no render.
	//
	// Example: ["status.listeners[*].attachedRoutes"]
	// +optional
	IgnoreFields []string `json:"ignoreFields,omitempty"`
}

// TemplateSnippet defines a reusable template fragment.
type TemplateSnippet struct {
	// Template is the template content.
	//
	// Can be included in other templates using {{ render "snippet_name" }}.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Template string `json:"template"`

	// Requires lists watched-resource names this snippet depends on. When an
	// optional watched resource has no served candidate version, every
	// snippet requiring it is stripped from the effective config at load
	// time. Each entry must name a key of watchedResources.
	// +optional
	Requires []string `json:"requires,omitempty"`

	// Incremental configures authenticated incremental evaluation for this snippet.
	// +optional
	Incremental *IncrementalTemplate `json:"incremental,omitempty"`
}

// IncrementalTemplate configures authenticated incremental evaluation.
// +kubebuilder:validation:XValidation:rule="has(self.source) != has(self.bindingsTemplate)",message="exactly one of source or bindingsTemplate is required"
// +kubebuilder:validation:XValidation:rule="!has(self.whenAnyPathExists) || !has(self.effects) || !self.effects.exists(effect, effect == 'deriveResource')",message="whenAnyPathExists cannot be combined with deriveResource"
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode != 'resourceProjection' || (has(self.bindingsTemplate) && !has(self.source))",message="resourceProjection requires bindingsTemplate and forbids source"
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode != 'resourceProjection' || (has(self.effects) && self.effects.size() == 1 && self.effects[0] == 'publishValue')",message="resourceProjection requires exactly the publishValue effect"
// +kubebuilder:validation:XValidation:rule="!has(self.mode) || self.mode != 'resourceProjection' || (!has(self.whenAnyPathExists) && !has(self.root) && !has(self.consumes) && !has(self.optionalConsumes))",message="resourceProjection forbids whenAnyPathExists, root, consumes, and optionalConsumes"
type IncrementalTemplate struct {
	// Mode selects Scriggo execution when empty or exact resource publication
	// when set to resourceProjection. Resource projections emit no rendered text.
	// +kubebuilder:validation:Enum="";resourceProjection
	// +kubebuilder:validation:MaxLength=32
	// +optional
	Mode IncrementalMode `json:"mode,omitempty"`

	// Source names one watchedResources entry.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Source string `json:"source,omitempty"`

	// BindingsTemplate emits a JSON object mapping watched-resource aliases to
	// immutable component props.
	// +kubebuilder:validation:MinLength=1
	// +optional
	BindingsTemplate string `json:"bindingsTemplate,omitempty"`

	// WhenAnyPathExists makes the component empty unless at least one JSONPath
	// exists on its post-derivation source item. [*] selects any array element.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:Pattern=`^\$?\.?([^.\[\]?*@()]+|\[([0-9]+|\*|'[^']+'|"[^"]+")\])(\.?([^.\[\]?*@()]+|\[([0-9]+|\*|'[^']+'|"[^"]+")\]))*$`
	// +listType=set
	// +optional
	WhenAnyPathExists []string `json:"whenAnyPathExists,omitempty"`

	// Root groups compatible components into one authenticated Scriggo runner.
	// Each member keeps its own bindings, dependencies, effects, and result.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Root string `json:"root,omitempty"`

	// Group evaluates all members before resolving shared contributions.
	// +optional
	Group string `json:"group,omitempty"`

	// Consumes lists publication groups that must complete before this component
	// can select their exact keyed values.
	// +kubebuilder:validation:items:MinLength=1
	// +listType=set
	// +optional
	Consumes []string `json:"consumes,omitempty"`

	// OptionalConsumes lists publication groups that may be absent only when
	// effective-config resolution stripped their optional-resource snippets.
	// +kubebuilder:validation:items:MinLength=1
	// +listType=set
	// +optional
	OptionalConsumes []string `json:"optionalConsumes,omitempty"`

	// Uniqueness comes from listType=set, which the apiserver enforces; an
	// equivalent CEL rule here is quadratic in the item count and priced per
	// templateSnippets entry, which alone exceeded the schema cost budget.

	// Effects declares the query-local side effects this component may emit.
	// +kubebuilder:validation:MaxItems=5
	// +listType=set
	// +optional
	Effects []IncrementalEffect `json:"effects,omitempty"`
}

// IncrementalMode selects an incremental execution protocol.
type IncrementalMode string

const (
	IncrementalModeScriggo            IncrementalMode = ""
	IncrementalModeResourceProjection IncrementalMode = "resourceProjection"
)

// MaxLength bounds the CEL cost estimator, which ignores Enum and otherwise
// prices each comparison against the whole request; the rules over this field
// then exceed the apiserver's per-rule budget 53x and the CRD is rejected.

// IncrementalEffect identifies a declared query-local side effect.
// +kubebuilder:validation:Enum=deriveResource;recordEvent;backendPlan;publishValue;statusPatch
// +kubebuilder:validation:MaxLength=32
type IncrementalEffect string

const (
	IncrementalEffectDeriveResource IncrementalEffect = "deriveResource"
	IncrementalEffectRecordEvent    IncrementalEffect = "recordEvent"
	IncrementalEffectBackendPlan    IncrementalEffect = "backendPlan"
	IncrementalEffectPublishValue   IncrementalEffect = "publishValue"
	IncrementalEffectStatusPatch    IncrementalEffect = "statusPatch"
)

// PostProcessorConfig defines a post-processor to apply to rendered template output.
//
// IMPORTANT: This is a Kubernetes CRD type. When modifying this struct, you must also update:
//   - The internal config type: pkg/core/config/types.go (PostProcessorConfig)
//   - The conversion logic: pkg/controller/conversion/converter.go (ConvertSpec function)
type PostProcessorConfig struct {
	// Type specifies the post-processor type (e.g., "regex_replace").
	// +kubebuilder:validation:Required
	Type string `json:"type"`

	// Params contains post-processor-specific parameters.
	//
	// For "regex_replace":
	//   - pattern: Regular expression pattern to match
	//   - replace: Replacement string
	// +kubebuilder:validation:Required
	Params map[string]string `json:"params"`
}

// MapFile defines a HAProxy map file generated from a template.
//
// IMPORTANT: This is a Kubernetes CRD type. When modifying this struct, you must also update:
//   - The internal config type: pkg/core/config/types.go (MapFile)
//   - The conversion logic: pkg/controller/conversion/converter.go (ConvertSpec function - maps section)
type MapFile struct {
	// Template is the template for generating the map file content.
	//
	// The rendered output should be in HAProxy map file format (key-value pairs).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Template string `json:"template"`

	// PostProcessing defines optional post-processors to apply after rendering.
	//
	// Post-processors run in the order specified and can transform the rendered output.
	// +optional
	PostProcessing []PostProcessorConfig `json:"postProcessing,omitempty"`

	// Ordered declares whether the position of an entry inside this map file
	// changes what HAProxy does with it.
	//
	// Keep it true for a map read with map_reg, map_sub, map_dom, map_dir or
	// map_end: those are evaluated as a list, first match wins, so an entry
	// inserted in the middle must land in the middle. Set it to false for a
	// map_str, map_beg, map_ip or map_str_int lookup, where a key is found by
	// its own value and a new entry can simply be appended at runtime instead
	// of rewriting the file and reloading.
	// +optional
	// +kubebuilder:default=true
	Ordered *bool `json:"ordered,omitempty"`
}

// GeneralFile defines a general file generated from a template.
//
// The filename is derived from the map key in the configuration.
// The full path is constructed using the pathResolver.GetPath() method in templates:
//
//	Example: pathResolver.GetPath("503.http", "file") returns /etc/haproxy/general/503.http
//
// IMPORTANT: This is a Kubernetes CRD type. When modifying this struct, you must also update:
//   - The internal config type: pkg/core/config/types.go (GeneralFile)
//   - The conversion logic: pkg/controller/conversion/converter.go (ConvertSpec function - files section)
type GeneralFile struct {
	// Template is the template for generating the file content.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Template string `json:"template"`

	// PostProcessing defines optional post-processors to apply after rendering.
	//
	// Post-processors run in the order specified and can transform the rendered output.
	// +optional
	PostProcessing []PostProcessorConfig `json:"postProcessing,omitempty"`

	// ReloadOnPush controls whether a content change to this file reloads HAProxy.
	//
	// Keep it true for anything HAProxy itself reads — error pages, ca-files,
	// host-match lists — since only a reload makes the new content take effect.
	// Set it to false for a file a sidecar owns and watches on its own (the
	// spoa-hub TOML, the Vector config): HAProxy never opens those, so the
	// reload buys nothing and only costs a worker respawn.
	//
	// This governs writes. Removing a file reloads only when the rendered
	// configuration (or a crt-list) still names it, which no sidecar-owned file
	// ever does.
	// +optional
	// +kubebuilder:default=true
	ReloadOnPush *bool `json:"reloadOnPush,omitempty"`
}

// K8sResource defines a template that emits one or more Kubernetes
// resources via multi-doc YAML.
//
// IMPORTANT: This is a Kubernetes CRD type. When modifying this struct, you must also update:
//   - The internal config type: pkg/core/config/types.go (K8sResource)
//   - The conversion logic: pkg/controller/conversion/converter.go (ConvertSpec function - k8sResources section)
type K8sResource struct {
	// Template is the template for generating the resource YAML.
	//
	// The rendered output is parsed as one or more Kubernetes
	// resources (separate documents with `---`). Each document must
	// declare `apiVersion`, `kind`, and `metadata.name` (plus
	// `metadata.namespace` for namespaced kinds).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Template string `json:"template"`

	// PostProcessing defines optional post-processors to apply after rendering.
	//
	// Post-processors run in the order specified and can transform the rendered output.
	// +optional
	PostProcessing []PostProcessorConfig `json:"postProcessing,omitempty"`

	// CreateOnlyFields are dotted field paths this template sets when the
	// object is created and never again, so whoever runs the object owns them
	// afterwards.
	//
	// `spec.replicas` on a workload is the case this exists for: HAProxy
	// routes to whatever pods are there, so the count belongs to an operator
	// draining it or to an autoscaler. Without this the field is re-applied
	// every reconcile, and a deliberate `kubectl scale` is overwritten seconds
	// after it is made. The template's value is the size the object starts at,
	// not the size it keeps.
	//
	// Paths are dotted (`spec.replicas`) and must name a field the template
	// itself sets. A path absent from the rendered object is ignored.
	// +optional
	CreateOnlyFields []string `json:"createOnlyFields,omitempty"`
}

// SSLCertificate defines an SSL certificate generated from a template.
//
// IMPORTANT: This is a Kubernetes CRD type. When modifying this struct, you must also update:
//   - The internal config type: pkg/core/config/types.go (SSLCertificate)
//   - The conversion logic: pkg/controller/conversion/converter.go (ConvertSpec function - sslCertificates section)
type SSLCertificate struct {
	// Template is the template for generating the certificate content.
	//
	// The rendered output should be in PEM format (certificate + private key).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Template string `json:"template"`

	// PostProcessing defines optional post-processors to apply after rendering.
	//
	// Post-processors run in the order specified and can transform the rendered output.
	// +optional
	PostProcessing []PostProcessorConfig `json:"postProcessing,omitempty"`
}

// HAProxyConfig defines the main HAProxy configuration.
//
// IMPORTANT: This is a Kubernetes CRD type. When modifying this struct, you must also update:
//   - The internal config type: pkg/core/config/types.go (HAProxyConfig)
//   - The conversion logic: pkg/controller/conversion/converter.go (ConvertSpec function - haproxyConfig section)
type HAProxyConfig struct {
	// Template is the template for generating haproxy.cfg.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Template string `json:"template"`

	// PostProcessing defines optional post-processors to apply after rendering.
	//
	// Post-processors run in the order specified and can transform the rendered output.
	// Common use case: Normalize indentation with regex_replace.
	// +optional
	PostProcessing []PostProcessorConfig `json:"postProcessing,omitempty"`
}

// ValidationTest defines a validation test with fixtures and assertions.
//
// The test name is provided by the map key in ValidationTests.
type ValidationTest struct {
	// Description explains what this test validates.
	// +optional
	Description string `json:"description,omitempty"`

	// Fixtures defines the Kubernetes resources to use for this test.
	//
	// Keys are resource type names (matching WatchedResources keys).
	// Values are arrays of resources as raw JSON.
	//
	// Example:
	//   ingresses:
	//     - apiVersion: networking.k8s.io/v1
	//       kind: Ingress
	//       metadata:
	//         name: test-ingress
	//
	// Optional per object so several libraries can each contribute part of the
	// shared `_global` baseline; see Assertions.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Fixtures map[string][]runtime.RawExtension `json:"fixtures,omitempty"`

	// HTTPResources defines mock HTTP content for this test.
	//
	// These fixtures are used when templates call http.Fetch() to provide
	// pre-defined content without making actual HTTP requests.
	//
	// Example:
	//   httpResources:
	//     - url: http://blocklist.example.com/list.txt
	//       content: |
	//         blocked-value-1
	//         blocked-value-2
	// +optional
	HTTPResources []HTTPResourceFixture `json:"httpResources,omitempty"`

	// CurrentServers are the servers of a previous deployment, keyed by backend
	// name and then by server name, exposed to templates as
	// `currentConfig.ServerIndex`. Use it to test slot-aware server assignment
	// during rolling deployments.
	//
	// Example:
	//   currentServers:
	//     my-backend:
	//       srv1: {address: 10.0.0.1, port: 8080}
	//       srv2: {address: 10.0.0.2, port: 8080}
	// +optional
	CurrentServers map[string]map[string]ServerAddr `json:"currentServers,omitempty"`

	// CurrentFiles are the currently-deployed general auxiliary files
	// (filename → content), exposed to templates under `currentFiles`. Used to
	// test templates that read their own prior output — e.g. self-rotating TLS
	// session-ticket keys inspecting the current key file's embedded date marker.
	// +optional
	CurrentFiles map[string]string `json:"currentFiles,omitempty"`

	// ExtraContext provides custom variables that override the global extraContext for this test.
	//
	// This allows testing template behavior with different extraContext values without
	// modifying the global configuration.
	//
	// Example:
	//   extraContext:
	//     sanitize_auth_realm: true
	//     debug: true
	// +optional
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	ExtraContext runtime.RawExtension `json:"extraContext,omitempty"`

	// MinHAProxyVersion specifies the minimum HAProxy version required to run this test.
	//
	// When set, the test is skipped if the local HAProxy version is below this threshold.
	// This is useful for tests that use HAProxy features only available in newer versions
	// (e.g., shm-stats-file requires HAProxy 3.3+).
	//
	// Format: "major.minor" (e.g., "3.3")
	// +optional
	MinHAProxyVersion string `json:"minHAProxyVersion,omitempty"`

	// Requires lists watched-resource names this test depends on. When an
	// optional watched resource has no served candidate version, every test
	// requiring it is stripped from the effective config at load time.
	// Each entry must name a key of watchedResources.
	// +optional
	Requires []string `json:"requires,omitempty"`

	// RequiresFields lists schema field paths this test depends on, each in
	// the form "<watchedResourceKey>.<field.path>" (e.g.
	// "httproutes.spec.rules.filters.cors"). When any referenced field is
	// absent from the resolved schema generation of its watched resource,
	// the test is stripped from the effective config at load time. This
	// covers clusters that serve the resource at the same API version as
	// newer releases but with an older schema generation lacking the field.
	// The first dot-segment of each entry must name a key of
	// watchedResources.
	// +optional
	RequiresFields []string `json:"requiresFields,omitempty"`

	// Assertions defines the validation checks to perform.
	//
	// Optional in the schema, required in practice: config.ValidateStructure
	// rejects a merged config whose test declares none. The schema cannot
	// express it because the reserved `_global` entry is a shared baseline
	// rather than a test — the runner never executes its assertions — and
	// several template libraries each contribute part of it, so each of their
	// objects carries an incomplete `_global` that only becomes whole after
	// the merge.
	// +optional
	Assertions []ValidationAssertion `json:"assertions,omitempty"`
}

// ServerAddr is one server of a previously deployed backend, as templates see
// it in currentConfig.ServerIndex.
type ServerAddr struct {
	// Address is the server's address — an IP or a hostname.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`

	// Port is the server's port. Omit it for a server line that carries none.
	// +optional
	Port *int64 `json:"port,omitempty"`
}

// HTTPResourceFixture defines mock HTTP content for validation tests.
//
// When templates call http.Fetch() for the specified URL, the pre-defined
// content is returned instead of making an actual HTTP request.
type HTTPResourceFixture struct {
	// URL is the HTTP URL to mock.
	//
	// When http.Fetch() is called with this URL during test execution,
	// the Content field is returned instead of fetching from the network.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// Content is the mock response body to return.
	//
	// This content is returned when http.Fetch() is called with the
	// matching URL during test execution.
	// +kubebuilder:validation:Required
	Content string `json:"content"`

	// Pending renders the URL as content waiting for acceptance: http.Fetch
	// returns empty content and http.Pending reports true. Content is ignored.
	// +optional
	Pending bool `json:"pending,omitempty"`
}

// ValidationAssertion defines a single validation check.
type ValidationAssertion struct {
	// Type is the assertion type.
	//
	// Supported types:
	//   - haproxy_valid: Validates that generated HAProxy config is syntactically valid
	//   - contains: Checks if target contains pattern (regex)
	//   - not_contains: Checks if target does not contain pattern (regex)
	//   - equals: Checks if target equals expected value
	//   - jsonpath: Evaluates JSONPath expression against target
	//   - match_count: Counts how many times pattern matches in target (regex)
	//   - match_order: Validates that patterns appear in specified order
	//   - not_exists: Checks that the render produced no artefact under the target
	//   - deterministic: Verifies that rendering twice produces identical output
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=haproxy_valid;contains;not_contains;equals;jsonpath;match_count;match_order;not_exists;deterministic
	Type string `json:"type"`

	// Description explains what this assertion validates.
	// +optional
	Description string `json:"description,omitempty"`

	// Target specifies what to validate.
	//
	// Format depends on assertion type:
	//   - haproxy_valid: not used
	//   - contains/not_contains/equals: "haproxy.cfg", "map:<name>", "file:<name>", "cert:<name>"
	//   - jsonpath: the resource to query
	// +optional
	Target string `json:"target,omitempty"`

	// Pattern is the regex pattern for contains/not_contains assertions.
	// +optional
	Pattern string `json:"pattern,omitempty"`

	// Expected is the expected value for equals assertions.
	// +optional
	Expected string `json:"expected,omitempty"`

	// JSONPath is the JSONPath expression for jsonpath assertions.
	// +optional
	JSONPath string `json:"jsonpath,omitempty"`

	// Patterns is a list of regex patterns for match_order assertions.
	// The patterns must appear in the target in the order specified.
	// +optional
	Patterns []string `json:"patterns,omitempty"`
}

// HAProxyTemplateConfigStatus defines the observed state of HAProxyTemplateConfig.
type HAProxyTemplateConfigStatus struct {
	// ObservedGeneration reflects the generation most recently observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastValidated is the timestamp of the last successful validation.
	// +optional
	LastValidated *metav1.Time `json:"lastValidated,omitempty"`

	// ValidationStatus indicates the overall validation status.
	// +kubebuilder:validation:Enum=Valid;Invalid;Unknown
	// +optional
	ValidationStatus string `json:"validationStatus,omitempty"`

	// ValidationMessage contains human-readable validation details.
	// +optional
	ValidationMessage string `json:"validationMessage,omitempty"`

	// ValidationErrors contains detailed validation error messages.
	// Each entry includes the template name, error location, and context.
	// +optional
	ValidationErrors []string `json:"validationErrors,omitempty"`

	// Conditions represent the latest available observations of the config's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ConditionTypeValidated reports whether the controller accepted the observed
// generation; the reasons below are the ones it writes with it.
const (
	ConditionTypeValidated = "Validated"

	ReasonValidationSucceeded     = "ValidationSucceeded"
	ReasonConfigInvalid           = "ConfigInvalid"
	ReasonValidationInconclusive  = "ValidationInconclusive"
	ReasonHAProxyValidationFailed = "HAProxyValidationFailed"
	ReasonLoadGateFailed          = "LoadGateFailed"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true

// HAProxyTemplateConfigList contains a list of HAProxyTemplateConfig.
type HAProxyTemplateConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HAProxyTemplateConfig `json:"items"`
}
