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

// Package config provides data models for the controller configuration.
//
// These models represent the structure of the configuration YAML produced
// by pkg/controller/conversion.ParseCRD from a HAProxyTemplateConfig CRD,
// plus the credentials loaded from the referenced Secret.
package config

import "time"

// DefaultCompressionThreshold is the default minimum size in bytes at which configs are compressed.
// This matches the CRD kubebuilder default annotation.
const DefaultCompressionThreshold int64 = 1048576 // 1 MiB

// DefaultRenderTimeout is the default maximum duration for rendering a single template.
const DefaultRenderTimeout = 30 * time.Second

// Config is the root configuration structure that
// pkg/controller/conversion.ParseCRD produces (it converts a
// HAProxyTemplateConfig CRD into this internal shape).
type Config struct {
	// PodSelector identifies HAProxy pods to configure.
	PodSelector PodSelector `yaml:"pod_selector"`

	// Controller contains controller-level settings (ports, etc.).
	Controller ControllerConfig `yaml:"controller"`

	// Logging configures logging behavior.
	Logging LoggingConfig `yaml:"logging"`

	// Dataplane configures how the controller reaches the HAProxy pods and where
	// their files live. The block keeps its name across the agent cutover.
	Dataplane DataplaneConfig `yaml:"dataplane"`

	// TemplatingSettings configures template rendering behavior and custom variables.
	TemplatingSettings TemplatingSettings `yaml:"templating_settings" json:"templatingSettings"`

	// WatchedResourcesIgnoreFields specifies JSONPath expressions for fields
	// to remove from all watched resources to reduce memory usage.
	//
	// Example: ["metadata.managedFields"]
	WatchedResourcesIgnoreFields []string `yaml:"watched_resources_ignore_fields"`

	// WatchedResources maps resource type names to their watch configuration.
	//
	// Example:
	//   ingresses:
	//     api_version: networking.k8s.io/v1
	//     kind: Ingress
	//     index_by: ["metadata.namespace", "metadata.name"]
	WatchedResources map[string]WatchedResource `yaml:"watched_resources"`

	// Validators lists pluggable validator sidecars consulted by the
	// admission webhook before admitting changes that affect plugin
	// configuration. See ValidatorConfig for per-entry semantics.
	//
	// An empty list (the default) leaves admission validation at
	// template + HAProxy syntax dry-run only.
	Validators []ValidatorConfig `yaml:"validators,omitempty"`

	// TemplateSnippets maps snippet names to reusable template fragments.
	//
	// Snippets can be included in other templates using {{ render "name" }}.
	TemplateSnippets map[string]TemplateSnippet `yaml:"template_snippets"`

	// AbsentIncrementalGroups authenticates groups removed by effective-config
	// resolution because all of their snippets require unavailable resources.
	AbsentIncrementalGroups map[string]struct{} `yaml:"-" json:"-"`

	// Maps maps map file names to their template definitions.
	//
	// These generate HAProxy map files for backend routing and other features.
	Maps map[string]MapFile `yaml:"maps"`

	// Files maps file names to their template definitions.
	//
	// These generate auxiliary files like custom error pages.
	Files map[string]GeneralFile `yaml:"files"`

	// SSLCertificates maps certificate names to their template definitions.
	//
	// These generate SSL certificate files for HAProxy.
	SSLCertificates map[string]SSLCertificate `yaml:"ssl_certificates"`

	// K8sResources maps resource template names to their template
	// definitions. Each entry's rendered output is parsed as one or
	// more Kubernetes resources (multi-doc YAML separated by `---`)
	// and applied via Server-Side Apply by the controller.
	K8sResources map[string]K8sResource `yaml:"k8s_resources"`

	// HAProxyConfig contains the main HAProxy configuration template.
	HAProxyConfig HAProxyConfig `yaml:"haproxy_config"`

	// ValidationTests contains embedded tests for validating template rendering.
	// These tests are used both in CLI validation and webhook admission validation.
	// The map key is the test name, which must be unique.
	ValidationTests map[string]ValidationTest `yaml:"validation_tests"`
}

// ValidationTest defines a single validation test with fixtures and assertions.
//
// The test name is provided by the map key in ValidationTests.
type ValidationTest struct {
	// UseDeploymentWatchScopes applies configured watch namespaces to this test.
	UseDeploymentWatchScopes bool `yaml:"useDeploymentWatchScopes,omitempty" json:"useDeploymentWatchScopes,omitempty"`

	// Description explains what this test validates.
	Description string `yaml:"description"`

	// Fixtures contains mock Kubernetes resources for this test.
	// The map key is the resource type name (e.g., "services", "ingresses").
	// The map value is a list of resources in unstructured format.
	Fixtures map[string][]any `yaml:"fixtures"`

	// HTTPFixtures contains mock HTTP responses for this test.
	// When templates call http.Fetch() for a URL that matches a fixture,
	// the fixture content is returned instead of making an actual HTTP request.
	HTTPFixtures []HTTPResourceFixture `yaml:"httpResources,omitempty"`

	// CurrentServers are the servers of a previous deployment, keyed by backend
	// name and then by server name, exposed to templates as
	// `currentConfig.ServerIndex` for testing slot-aware server assignment.
	CurrentServers map[string]map[string]ServerAddr `yaml:"currentServers,omitempty" json:"currentServers,omitempty"`

	// CurrentFiles are the currently-deployed general auxiliary files
	// (filename → content) exposed to templates under `currentFiles`. Used to
	// test templates that read their own prior output — e.g. self-rotating TLS
	// session-ticket keys inspecting the current key file's date marker.
	CurrentFiles map[string]string `yaml:"currentFiles,omitempty" json:"currentFiles,omitempty"`

	// ExtraContext provides custom variables that override the global extraContext for this test.
	// This allows testing template behavior with different extraContext values.
	ExtraContext map[string]any `yaml:"extraContext,omitempty" json:"extraContext,omitempty"`

	// MinHAProxyVersion specifies the minimum HAProxy version required to run this test.
	// Format: "major.minor" (e.g., "3.3"). When set, the test is skipped if the local
	// HAProxy version is below this threshold.
	MinHAProxyVersion string `yaml:"minHAProxyVersion,omitempty"`

	// Requires lists watched-resource names this test depends on. When an
	// optional watched resource has no served candidate version, every test
	// requiring it is stripped from the effective config at load time.
	// Each entry must name a key of WatchedResources.
	Requires []string `yaml:"requires,omitempty"`

	// RequiresFields lists schema field paths this test depends on, each in
	// the form "<watchedResourceKey>.<field.path>" (e.g.
	// "httproutes.spec.rules.filters.cors"). When ANY referenced field is
	// absent from the RESOLVED schema generation of its watched resource,
	// the test is stripped from the effective config at load time. This
	// covers clusters that serve the resource at the same version string
	// as newer releases but with an older schema generation lacking the
	// field — resource-level Requires stripping cannot fire there. The
	// first dot-segment of each entry must name a key of WatchedResources.
	RequiresFields []string `yaml:"requiresFields,omitempty"`

	// Assertions contains validation checks to run against the rendered config.
	Assertions []ValidationAssertion `yaml:"assertions"`
}

// ServerAddr is one server of a previously deployed backend, as templates see
// it in currentConfig.ServerIndex.
type ServerAddr struct {
	// Address is the server's address — an IP or a hostname.
	Address string `yaml:"address" json:"address"`

	// Port is the server's port. Nil for a server line that carries none.
	Port *int64 `yaml:"port,omitempty" json:"port,omitempty"`
}

// HTTPResourceFixture defines mock HTTP content for validation tests.
//
// This allows templates that use http.Fetch() to receive pre-defined content
// during validation tests without making actual HTTP requests.
type HTTPResourceFixture struct {
	// URL is the HTTP URL that will be matched.
	// When a template calls http.Fetch() with this URL, the fixture content is returned.
	URL string `yaml:"url" json:"url"`

	// Content is the response body to return when this URL is fetched.
	Content string `yaml:"content" json:"content"`

	// Pending makes the URL's content wait for acceptance: http.Fetch returns
	// empty content and http.Pending reports true.
	Pending bool `yaml:"pending,omitempty" json:"pending,omitempty"`
}

// ValidationAssertion defines a single validation check.
type ValidationAssertion struct {
	// Type is the assertion type: "haproxy_valid", "contains", "not_contains", "match_count", "equals", "jsonpath", "match_order", "deterministic".
	Type string `yaml:"type"`

	// Description explains what this assertion validates.
	Description string `yaml:"description"`

	// Target specifies what to validate: "haproxy.cfg", "map:<name>", "file:<name>", "cert:<name>".
	// Only used for non-haproxy_valid assertions.
	Target string `yaml:"target"`

	// Pattern is the regex pattern to match (for "contains" and "not_contains").
	Pattern string `yaml:"pattern"`

	// Expected is the expected value (for "equals" and "jsonpath").
	Expected string `yaml:"expected"`

	// JSONPath is the JSONPath expression to query (for "jsonpath").
	JSONPath string `yaml:"jsonpath"`

	// Patterns is a list of regex patterns that must appear in the specified order (for "match_order").
	Patterns []string `yaml:"patterns"`
}

// PodSelector identifies which HAProxy pods to configure.
type PodSelector struct {
	// MatchLabels are the labels to match HAProxy pods.
	//
	// Example:
	//   app: haproxy
	//   component: loadbalancer
	MatchLabels map[string]string `yaml:"match_labels"`
}

// ControllerConfig contains controller-level configuration.
type ControllerConfig struct {
	// LeaderElection configures leader election for high availability.
	LeaderElection LeaderElectionConfig `yaml:"leader_election"`

	// ConfigPublishing configures how rendered configs are stored in CRDs.
	ConfigPublishing ConfigPublishingConfig `yaml:"config_publishing"`

	// RenderGateInterval is the shortest start-to-start spacing of the render
	// gate's `haproxy -c` runs, capping its duty cycle so a render storm cannot
	// slow the admission webhook.
	// Format: Go duration string (e.g., "1s", "500ms")
	// Default: 1s
	RenderGateInterval string `yaml:"render_gate_interval"`
}

// LeaderElectionConfig configures leader election for running multiple replicas.
type LeaderElectionConfig struct {
	// Enabled determines whether leader election is active.
	// If false, the controller assumes it is the sole instance (single-replica mode).
	// Default: true
	Enabled bool `yaml:"enabled"`

	// LeaseName is the name of the Lease resource used for coordination.
	// Default: haptic-leader
	LeaseName string `yaml:"lease_name"`

	// LeaseDuration is the duration that non-leader candidates will wait
	// to force acquire leadership (measured against time of last observed ack).
	// Format: Go duration string (e.g., "60s", "1m")
	// Default: 30s
	LeaseDuration string `yaml:"lease_duration"`

	// RenewDeadline is the duration that the acting leader will retry
	// refreshing leadership before giving up.
	// Format: Go duration string (e.g., "15s")
	// Default: 20s
	// Must be less than LeaseDuration
	RenewDeadline string `yaml:"renew_deadline"`

	// RetryPeriod is the duration the LeaderElector clients should wait
	// between tries of actions.
	// Format: Go duration string (e.g., "5s")
	// Default: 5s
	// Must be less than RenewDeadline
	RetryPeriod string `yaml:"retry_period"`
}

// ConfigPublishingConfig configures how rendered configs are stored in CRDs.
type ConfigPublishingConfig struct {
	// CompressionThreshold is the payload size in bytes above which configs are compressed.
	// It includes the encoded recovery plan when one is present.
	// Default: 1 MiB (1048576 bytes) via CRD kubebuilder default.
	// Set to 0 or negative to disable compression.
	CompressionThreshold int64 `yaml:"compression_threshold"`
}

// LoggingConfig configures logging behavior.
type LoggingConfig struct {
	// Level controls the log level: TRACE, DEBUG, INFO, WARN, ERROR (case-insensitive)
	// Empty string means use LOG_LEVEL env var or default (INFO)
	Level string `yaml:"level"`
}

// DataplaneConfig configures how the controller reaches the HAProxy pods and
// where their files live. The block keeps its name across the agent cutover.
type DataplaneConfig struct {
	// Port is the agent's apply/state API port on each HAProxy pod.
	// A value of 0 means "uninitialized" and will be replaced with the default.
	// This is a production port and MUST NOT remain 0 after defaults are applied.
	// Default: 5555
	Port int `yaml:"port"`

	// MinDeploymentInterval enforces minimum time between consecutive deployments.
	// This prevents rapid-fire deployments from hammering HAProxy instances.
	// Format: Go duration string (e.g., "2s", "500ms")
	// Default: 2s
	MinDeploymentInterval string `yaml:"min_deployment_interval"`

	// DriftPreventionInterval triggers periodic deployments to prevent configuration drift.
	// A deployment is automatically triggered if no deployment has occurred within this interval.
	// This detects and corrects a pod whose file tree drifted from the render.
	// Format: Go duration string (e.g., "60s", "5m")
	// Default: 60s
	DriftPreventionInterval string `yaml:"drift_prevention_interval"`

	// DeploymentTimeout is the maximum time to wait for a deployment to complete.
	// If exceeded, the scheduler assumes the deployment was lost and retries.
	// This is a safety net for race conditions during leadership transitions.
	// Format: Go duration string (e.g., "30s", "1m")
	// Default: 30s
	DeploymentTimeout string `yaml:"deployment_timeout"`

	// MapsDir is the directory for HAProxy map files.
	// Used for both validation and deployment.
	// Default: /etc/haproxy/maps
	MapsDir string `yaml:"maps_dir"`

	// SSLCertsDir is the directory for SSL certificates.
	// Used for both validation and deployment.
	// Default: /etc/haproxy/ssl
	SSLCertsDir string `yaml:"ssl_certs_dir"`

	// GeneralStorageDir is the directory for general files (error pages, etc.).
	// Used for both validation and deployment.
	// Default: /etc/haproxy/general
	GeneralStorageDir string `yaml:"general_storage_dir"`

	// ConfigFile is the path to the main HAProxy configuration file.
	// Used for validation.
	// Default: /etc/haproxy/haproxy.cfg
	ConfigFile string `yaml:"config_file"`

	// ConfigPublishInterval throttles how often the HAProxyCfg CRD is updated.
	// During endpoint churn the rendered config changes frequently, but each CRD update
	// writes ~500 KB to etcd. This interval limits CRD publishes while deployments to
	// HAProxy pods (via events) remain unthrottled.
	// Uses leading-edge triggering: first change publishes immediately, subsequent
	// changes within the interval are buffered and published when the interval expires.
	// Format: Go duration string (e.g., "30s", "1m")
	// Default: 30s
	ConfigPublishInterval string `yaml:"config_publish_interval"`

	// ReloadVerificationTimeout bounds how long the Dataplane sync waits for a
	// graceful HAProxy reload to be reported as completed before failing the sync.
	// Format: Go duration string (e.g., "10s", "30s")
	// Default: 10s
	ReloadVerificationTimeout string `yaml:"reload_verification_timeout"`

	// SyncTimeout is the overall timeout for one Dataplane sync to a single
	// HAProxy endpoint (parse + diff + raw-push apply + optional reload-verify).
	// Format: Go duration string (e.g., "2m", "30s")
	// Default: 2m
	SyncTimeout string `yaml:"sync_timeout"`
}

// WatchedResource configures watching for a specific Kubernetes resource type.
type WatchedResource struct {
	// APIVersion is the Kubernetes API version (e.g., "networking.k8s.io/v1").
	// Mutually exclusive with APIVersions; equivalent to a one-element list.
	APIVersion string `yaml:"api_version"`

	// APIVersions is an ordered candidate list of API versions. At iteration
	// start the controller resolves the entry to the FIRST candidate the
	// apiserver serves, and that resolved version is used everywhere the
	// literal APIVersion would be (informer GVR, stores, schema fetch,
	// webhook registration, fixture defaulting). Mutually exclusive with
	// APIVersion.
	//
	// Example: ["gateway.networking.k8s.io/v1", "gateway.networking.k8s.io/v1beta1"]
	APIVersions []string `yaml:"api_versions,omitempty"`

	// Optional marks this resource as non-essential: when NO candidate
	// version is served by the cluster, the watch is dropped and every
	// TemplateSnippet / ValidationTest whose Requires names this resource is
	// stripped from the effective config at load time, instead of failing
	// startup. When false (default), an unserved resource fails the
	// iteration fast with a named error.
	Optional bool `yaml:"optional,omitempty"`

	// Resources is the plural form of the Kubernetes resource type (e.g., "ingresses", "services").
	// This is the name used in RBAC rules and API paths.
	Resources string `yaml:"resources"`

	// EnableValidationWebhook enables admission webhook validation for this resource.
	// Default: false
	EnableValidationWebhook bool `yaml:"enable_validation_webhook"`

	// IndexBy specifies JSONPath expressions for extracting index keys.
	//
	// Resources are indexed by these values for O(1) lookup.
	//
	// Examples:
	//   ["metadata.namespace", "metadata.name"]
	//   ["metadata.labels['kubernetes.io/service-name']"]
	IndexBy []string `yaml:"index_by"`

	// LabelSelector filters resources by labels (server-side filtering).
	//
	// Example:
	//   app: haproxy
	//   component: loadbalancer
	LabelSelector map[string]string `yaml:"label_selector,omitempty"`

	// FieldSelector filters resources using client-side JSONPath evaluation.
	// Unlike Kubernetes' native fieldSelector (which only supports limited fields),
	// this supports any JSONPath expression.
	//
	// Format: "field.path=value"
	// Example: "spec.ingressClassName=haproxy-internal"
	FieldSelector string `yaml:"field_selector,omitempty"`

	// Namespaces restricts the watch; nil watches all, and an empty list is invalid.
	// A template lookup into an unlisted namespace fails the render.
	Namespaces []string `yaml:"namespaces,omitempty"`

	// NamespacesSetting names what chose Namespaces; the error for a lookup
	// outside them quotes it.
	NamespacesSetting string `yaml:"namespaces_setting,omitempty"`

	// Store specifies the storage backend: "full" (MemoryStore) or "on-demand" (CachedStore).
	// Default: "full"
	//
	// Use "on-demand" for large resources that are accessed infrequently (e.g., Secrets).
	// Use "full" for frequently accessed resources (e.g., Services, EndpointSlices).
	Store string `yaml:"store"`

	// DebounceInterval overrides the default leading-edge refractory window
	// for this resource type's watcher. Format: Go duration string (e.g.,
	// "500ms", "10s"). When empty or invalid, the watcher falls back to
	// pkg/k8s/types.DefaultDebounceInterval (100ms).
	//
	// Lower values make the controller respond faster to bursty changes at
	// the cost of more reconciliations and GC pressure. Most workloads
	// should leave this empty.
	DebounceInterval string `yaml:"debounce_interval,omitempty"`

	// IgnoreFields lists JSONPath expressions removed from this resource
	// before it is stored, in addition to WatchedResourcesIgnoreFields.
	// `[*]` selects every element of an array. An update that changes only
	// ignored fields is not a change.
	IgnoreFields []string `yaml:"ignore_fields,omitempty"`
}

// GetDebounceInterval returns the configured debounce interval. The CRD field
// is a Go duration string. Mapping:
//
//   - empty / unparseable → 0, signalling "no override" — the watcher's
//     WatcherConfig.SetDefaults swaps it for pkg/k8s/types.DefaultDebounceInterval.
//   - explicit zero ("0", "0s", "0ms", …) → pkg/k8s/types.DebounceImmediate
//     (-1), signalling "fire on every event without batching." SetDefaults
//     normalises this to 0 before reaching the Debouncer, whose leading-edge
//     code already handles the zero case correctly.
//   - any other valid duration → parsed value.
//
// The two distinct return values for "user wrote 0" vs "user wrote nothing"
// are why this function can't just return the ParseDuration result directly:
// the WatcherConfig contract treats 0 as "unset, apply default," so a
// genuine immediate-fire intent has to ride a different value through the
// pipeline.
func (r *WatchedResource) GetDebounceInterval() time.Duration {
	if r.DebounceInterval == "" {
		return 0
	}
	d, err := time.ParseDuration(r.DebounceInterval)
	if err != nil {
		return 0
	}
	if d == 0 {
		return DebounceImmediate
	}
	return d
}

// CandidateVersions returns the ordered API-version candidate list for this
// resource: APIVersions when set, otherwise the singular APIVersion as a
// one-element list. Validation guarantees exactly one of the two fields is
// populated, so callers can treat the result as the single source of truth.
func (r *WatchedResource) CandidateVersions() []string {
	if len(r.APIVersions) > 0 {
		return r.APIVersions
	}
	return []string{r.APIVersion}
}

// DebounceImmediate mirrors pkg/k8s/types.DebounceImmediate (-1) — the
// sentinel value for "no debounce, fire on every event." arch-go.yml
// forbids pkg/core/config from importing pkg/k8s/types, so the constant
// is duplicated. tests/defaults_consistency_test.go pins the equality;
// change both together.
const DebounceImmediate time.Duration = -1

// TemplateSnippet is a reusable template fragment.
type TemplateSnippet struct {
	// Name is the snippet identifier for {{ render "name" }}.
	// This is derived from the map key in the configuration.
	Name string

	// Template is the template content.
	Template string `yaml:"template"`

	// Requires lists watched-resource names this snippet depends on. When an
	// optional watched resource has no served candidate version, every
	// snippet requiring it is stripped from the effective config at load
	// time (surviving snippets must reach stripped resources only through
	// compile-safe seams such as `render "..." default ""`).
	// Each entry must name a key of WatchedResources.
	Requires []string `yaml:"requires,omitempty"`

	// Incremental configures authenticated incremental evaluation for this snippet.
	Incremental *IncrementalTemplate `yaml:"incremental,omitempty"`
}

// IncrementalTemplate configures authenticated incremental evaluation.
type IncrementalTemplate struct {
	Mode              IncrementalMode     `yaml:"mode,omitempty"`
	Source            string              `yaml:"source,omitempty"`
	BindingsTemplate  string              `yaml:"bindings_template,omitempty"`
	WhenAnyPathExists []string            `yaml:"when_any_path_exists,omitempty"`
	Root              string              `yaml:"root,omitempty"`
	Group             string              `yaml:"group,omitempty"`
	Consumes          []string            `yaml:"consumes,omitempty"`
	OptionalConsumes  []string            `yaml:"optional_consumes,omitempty"`
	Effects           []IncrementalEffect `yaml:"effects,omitempty"`
}

// IncrementalMode selects an incremental execution protocol.
type IncrementalMode string

const (
	IncrementalModeScriggo            IncrementalMode = ""
	IncrementalModeResourceProjection IncrementalMode = "resourceProjection"
)

// IncrementalEffect identifies a declared query-local side effect.
type IncrementalEffect string

const (
	IncrementalEffectDeriveResource IncrementalEffect = "deriveResource"
	IncrementalEffectRecordEvent    IncrementalEffect = "recordEvent"
	IncrementalEffectBackendPlan    IncrementalEffect = "backendPlan"
	IncrementalEffectPublishValue   IncrementalEffect = "publishValue"
	IncrementalEffectStatusPatch    IncrementalEffect = "statusPatch"
)

// IncrementalTemplatePrefix is reserved for generated component entry points.
const IncrementalTemplatePrefix = "__haptic_incremental__"

// IncrementalBindingsTemplatePrefix is reserved for generated binding planner entry points.
const IncrementalBindingsTemplatePrefix = "__haptic_incremental_bindings__"

// MapFile is a HAProxy map file template.
type MapFile struct {
	// Template is the template content that generates the map file.
	Template string `yaml:"template"`

	// PostProcessing defines optional post-processors to apply after rendering.
	// Post-processors are applied in order to transform the rendered output.
	PostProcessing []PostProcessorConfig `yaml:"post_processing,omitempty"`

	// Ordered mirrors the CRD's maps[].ordered. Nil means true — only an
	// explicit false lets a new entry be appended over the runtime API instead
	// of rewriting the file and reloading.
	Ordered *bool `yaml:"ordered,omitempty"`
}

// GeneralFile is a general-purpose auxiliary file template.
type GeneralFile struct {
	// Template is the template content that generates the file.
	Template string `yaml:"template"`

	// PostProcessing defines optional post-processors to apply after rendering.
	// Post-processors are applied in order to transform the rendered output.
	PostProcessing []PostProcessorConfig `yaml:"post_processing,omitempty"`

	// ReloadOnPush mirrors the CRD's files[].reloadOnPush. Nil means true —
	// only an explicit false marks the file as sidecar-owned and lets a
	// content change deploy without reloading HAProxy.
	ReloadOnPush *bool `yaml:"reload_on_push,omitempty"`
}

// K8sResource is a Kubernetes-resource-emitting template. The
// rendered output is parsed as one or more YAML documents and applied
// via SSA by the resourceapplier component.
type K8sResource struct {
	// Template is the template content that generates the resource YAML.
	Template string `yaml:"template"`

	// PostProcessing defines optional post-processors to apply after rendering.
	// Post-processors are applied in order to transform the rendered output.
	PostProcessing []PostProcessorConfig `yaml:"post_processing,omitempty"`

	// CreateOnlyFields are dotted field paths applied when the object is
	// created and omitted from every apply after that, leaving them to
	// whoever runs the object.
	CreateOnlyFields []string `yaml:"create_only_fields,omitempty"`
}

// SSLCertificate is an SSL certificate file template.
type SSLCertificate struct {
	// Template is the template content that generates the certificate file.
	Template string `yaml:"template"`

	// PostProcessing defines optional post-processors to apply after rendering.
	// Post-processors are applied in order to transform the rendered output.
	PostProcessing []PostProcessorConfig `yaml:"post_processing,omitempty"`
}

// HAProxyConfig is the main HAProxy configuration template.
type HAProxyConfig struct {
	// Template is the template content that generates haproxy.cfg.
	Template string `yaml:"template"`

	// PostProcessing defines optional post-processors to apply after rendering.
	// Post-processors are applied in order to transform the rendered output.
	// Common use case: indentation normalization with regex_replace.
	//
	// Example:
	//   post_processing:
	//     - type: regex_replace
	//       params:
	//         pattern: "^[ ]+"
	//         replace: "  "
	PostProcessing []PostProcessorConfig `yaml:"post_processing,omitempty"`
}

// PostProcessorConfig defines a post-processor to apply to rendered template output.
type PostProcessorConfig struct {
	// Type specifies the post-processor type.
	// Supported values: "regex_replace"
	Type string `yaml:"type"`

	// Params contains type-specific configuration parameters.
	// For regex_replace:
	//   - pattern: Regular expression pattern to match (required)
	//   - replace: Replacement string (required)
	Params map[string]string `yaml:"params"`
}

// TemplatingSettings configures template rendering behavior and custom variables.
type TemplatingSettings struct {
	// Engine specifies which template engine to use for rendering.
	// Valid values: "scriggo" (default and only supported engine)
	Engine string `yaml:"engine" json:"engine"`

	// RenderTimeout is the maximum time allowed for rendering a single template.
	// Uses Go duration format (e.g., "30s", "1m"). Default: "30s"
	RenderTimeout string `yaml:"render_timeout" json:"renderTimeout"`

	// ExtraContext provides custom variables that are passed to all templates.
	//
	// This allows users to add arbitrary data to the template context without
	// modifying controller code. Values can be any valid JSON type (string, number,
	// boolean, object, array).
	//
	// Example in YAML:
	//   extra_context:
	//     debug:
	//       enabled: true
	//     environment: production
	//     custom_value: 42
	//
	// Templates can then reference these variables directly: {{ debug.enabled }}, {{ environment }}, etc.
	ExtraContext map[string]any `yaml:"extra_context" json:"extraContext"`

	// TestExtraContext is the extraContext validationTests render with instead of
	// ExtraContext: the libraries' extraContext defaults overlaid with every
	// source's testExtraContext (see conversion.MergeSpecs).
	TestExtraContext map[string]any `yaml:"test_extra_context" json:"testExtraContext"`
}

// GetRenderTimeout returns the configured render timeout or the default.
func (t *TemplatingSettings) GetRenderTimeout() time.Duration {
	if t.RenderTimeout != "" {
		if d, err := time.ParseDuration(t.RenderTimeout); err == nil {
			return d
		}
	}
	return DefaultRenderTimeout
}

// Credentials contains the HAPTIC agent credentials.
//
// Loaded by pkg/core/config.LoadCredentials from the Secret referenced by
// spec.credentialsSecretRef on the HAProxyTemplateConfig CRD — kept out of
// the CRD itself so the CRD can be stored in Git / Helm values without
// leaking secrets.
type Credentials struct {
	// DataplaneUsername is the username for production HAProxy instances.
	DataplaneUsername string

	// DataplanePassword is the password for production HAProxy instances.
	DataplanePassword string
}

// ValidatorConfig is the core-config mirror of v1alpha1.ValidatorConfig.
//
// The CRD type carries Kubernetes validation tags; this type carries
// the raw values after conversion. Consumers (controller startup, the
// pluggablevalidator package) work against this type so they don't
// take a dependency on the v1alpha1 API surface.
//
// The wire protocol this struct configures lives in
// docs/development/validator-protocol.md.
type ValidatorConfig struct {
	// Name is the operator-facing validator identifier.
	Name string `yaml:"name"`

	// SocketPath is the absolute filesystem path to the validator's
	// Unix domain socket inside the controller pod.
	SocketPath string `yaml:"socket_path"`

	// Files is the list of glob patterns matched against rendered
	// file paths to decide which files to send to this validator.
	// Globs follow Go's `path/filepath.Match` rules.
	Files []string `yaml:"files"`

	// DataFiles is the list of glob patterns for files this validator needs
	// to check the ones it validates, but must not validate on their own.
	// Attached to every request to this validator, marked as data. A file
	// matching both lists is data.
	DataFiles []string `yaml:"data_files,omitempty"`

	// TimeoutMs is the per-call deadline in milliseconds, covering
	// the request-response cycle for one file. Zero falls back to
	// 5000 (5 seconds).
	TimeoutMs int32 `yaml:"timeout_ms,omitempty"`

	// MaxConnections caps the controller-side pool size for this
	// validator's socket. Zero falls back to 4. The pool starts
	// small and grows on contention up to this cap.
	MaxConnections int32 `yaml:"max_connections,omitempty"`
}
