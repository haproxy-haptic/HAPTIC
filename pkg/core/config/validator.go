package config

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// logLevelInfo is the canonical default log level; also keeps the repeated
// "INFO" literal (validator + tests) constified for goconst.
const logLevelInfo = "INFO"

// client-go rejects renew deadlines within its 20% retry jitter window.
const leaderElectionRetryJitterFactor = 1.2

// ValidateStructure performs basic structural validation on the configuration.
// Validates required fields, value ranges, and non-empty slices.
// Does NOT validate template syntax or JSONPath expressions.
func ValidateStructure(cfg *Config) error {
	if cfg == nil {
		return errors.New("config is nil")
	}

	// Completeness first: a missing podSelector or template is more fundamental
	// than a mistuned port, and reporting the port instead would bury it.
	if err := ValidateMergedCompleteness(cfg); err != nil {
		return err
	}

	if err := validateLeaderElectionConfig(&cfg.Controller.LeaderElection); err != nil {
		return fmt.Errorf("controller.leader_election: %w", err)
	}

	if err := validateLoggingConfig(&cfg.Logging); err != nil {
		return fmt.Errorf("logging: %w", err)
	}

	if err := validateDataplaneConfig(&cfg.Dataplane); err != nil {
		return fmt.Errorf("dataplane: %w", err)
	}

	// Validate WatchedResources (full per-resource checks; the prospective gate
	// below only enforces that at least one exists)
	if err := validateWatchedResources(cfg.WatchedResources); err != nil {
		return fmt.Errorf("watched_resources: %w", err)
	}

	if err := validateValidators(cfg.Validators); err != nil {
		return fmt.Errorf("validators: %w", err)
	}

	return ValidateTemplateStructure(cfg)
}

func validateLeaderElectionConfig(cfg *LeaderElectionConfig) error {
	leaseDuration, err := parseLeaderElectionDuration(
		"lease_duration",
		cfg.LeaseDuration,
		DefaultLeaderElectionLeaseDuration,
	)
	if err != nil {
		return err
	}
	renewDeadline, err := parseLeaderElectionDuration(
		"renew_deadline",
		cfg.RenewDeadline,
		DefaultLeaderElectionRenewDeadline,
	)
	if err != nil {
		return err
	}
	retryPeriod, err := parseLeaderElectionDuration(
		"retry_period",
		cfg.RetryPeriod,
		DefaultLeaderElectionRetryPeriod,
	)
	if err != nil {
		return err
	}
	if leaseDuration <= renewDeadline {
		return errors.New("lease_duration must be greater than renew_deadline")
	}
	if float64(renewDeadline) <= leaderElectionRetryJitterFactor*float64(retryPeriod) {
		return errors.New("renew_deadline must be more than 20% greater than retry_period")
	}
	return nil
}

func parseLeaderElectionDuration(field, value string, defaultValue time.Duration) (time.Duration, error) {
	if value == "" {
		return defaultValue, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a duration: %w", field, value, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", field)
	}
	return duration, nil
}

func validateValidators(validators []ValidatorConfig) error {
	for _, validator := range validators {
		for _, pattern := range validator.Files {
			if _, err := filepath.Match(pattern, ""); err != nil {
				return fmt.Errorf("%s.files: invalid glob %q: %w", validator.Name, pattern, err)
			}
		}
		for _, pattern := range validator.DataFiles {
			if _, err := filepath.Match(pattern, ""); err != nil {
				return fmt.Errorf("%s.data_files: invalid glob %q: %w", validator.Name, pattern, err)
			}
		}
	}
	return nil
}

// ValidateMergedCompleteness checks the requirements the CRD schema can no
// longer express, because a single HAProxyTemplateConfig of a merged set is
// legitimately incomplete (ADR-0014): a template-library object carries only
// template content, and several libraries each contribute part of the shared
// `_global` validationTests baseline.
//
// It is deliberately narrower than ValidateStructure: it covers exactly the
// fields whose `+kubebuilder:validation:Required` markers were dropped, so the
// gates that judge a *prospective* config — the admission webhook and
// `controller validate` — restore the apiserver-side guarantee without also
// demanding the runtime defaults (dataplane directories, ports) that the chart
// supplies but a hand-written config need not.
func ValidateMergedCompleteness(cfg *Config) error {
	if cfg == nil {
		return errors.New("config is nil")
	}

	if err := validatePodSelector(&cfg.PodSelector); err != nil {
		return fmt.Errorf("pod_selector: %w", err)
	}

	// Only the MinProperties=1 the CRD used to carry. The per-resource checks
	// in validateWatchedResources (indexBy and friends) were never expressed in
	// the schema, so applying them to a prospective config would tighten
	// admission for reasons unrelated to the merge.
	if len(cfg.WatchedResources) == 0 {
		return errors.New("watched_resources: at least one resource must be configured")
	}

	if err := validateHAProxyConfig(&cfg.HAProxyConfig); err != nil {
		return fmt.Errorf("haproxy_config: %w", err)
	}

	return validateValidationTests(cfg)
}

// GlobalValidationTestName is the reserved validationTests entry that is a
// shared baseline rather than a test: it contributes fixtures and an
// extraContext baseline to every other test, and the runner never executes its
// assertions.
const GlobalValidationTestName = "_global"

// validateValidationTests rejects a test that would silently pass because it
// asserts nothing.
//
// The CRD schema cannot express this. Several template libraries each
// contribute part of the shared `_global` baseline, so each of their objects
// carries an incomplete `_global` that only becomes whole after the merge —
// which is why `assertions` is optional in the schema and enforced here, on the
// merged config, instead.
func validateValidationTests(cfg *Config) error {
	if cfg.ValidationTests[GlobalValidationTestName].UseDeploymentWatchScopes {
		return fmt.Errorf("validationTests._global.useDeploymentWatchScopes cannot apply to shared fixtures. Set it on each test that checks deployment scopes")
	}
	for name := range cfg.ValidationTests {
		if name == GlobalValidationTestName {
			continue
		}
		if len(cfg.ValidationTests[name].Assertions) == 0 {
			return fmt.Errorf("validation_tests.%s: must declare at least one assertion "+
				"(a test with none passes without checking anything)", name)
		}
	}
	return nil
}

// ValidateTemplateStructure checks cross-field template references, test requirements, and incremental metadata.
func ValidateTemplateStructure(cfg *Config) error {
	if cfg == nil {
		return errors.New("config is nil")
	}

	if err := validatePrivateTemplateNames(cfg); err != nil {
		return err
	}
	for name, snippet := range cfg.TemplateSnippets {
		for _, req := range snippet.Requires {
			if _, ok := cfg.WatchedResources[req]; !ok {
				return fmt.Errorf("template_snippets.%s: requires %q does not name a watched resource", name, req)
			}
		}
		if snippet.Incremental == nil {
			continue
		}
		if err := validateIncrementalTemplate(cfg, name, snippet); err != nil {
			return err
		}
	}
	for name := range cfg.ValidationTests {
		test := cfg.ValidationTests[name]
		if err := validateTestRequires(cfg, name, &test); err != nil {
			return err
		}
	}
	return validateIncrementalDependencies(cfg)
}

func validatePrivateTemplateNames(cfg *Config) error {
	if err := validatePrivateNamesIn("template_snippets", cfg.TemplateSnippets); err != nil {
		return err
	}
	if err := validatePrivateNamesIn("maps", cfg.Maps); err != nil {
		return err
	}
	if err := validatePrivateNamesIn("files", cfg.Files); err != nil {
		return err
	}
	if err := validatePrivateNamesIn("ssl_certificates", cfg.SSLCertificates); err != nil {
		return err
	}
	return validatePrivateNamesIn("k8s_resources", cfg.K8sResources)
}

func validatePrivateNamesIn[V any](field string, values map[string]V) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		for _, prefix := range []string{IncrementalTemplatePrefix, IncrementalBindingsTemplatePrefix} {
			if strings.HasPrefix(name, prefix) {
				return fmt.Errorf("%s.%s: names starting with %q are reserved", field, name, prefix)
			}
		}
	}
	return nil
}

func validateIncrementalTemplate(cfg *Config, name string, snippet TemplateSnippet) error {
	incremental := snippet.Incremental
	if err := validateIncrementalEffects(name, incremental.Effects); err != nil {
		return err
	}
	if err := validateIncrementalMode(name, incremental); err != nil {
		return err
	}
	hasSource := incremental.Source != ""
	hasBindings := incremental.BindingsTemplate != ""
	if incremental.Mode == IncrementalModeScriggo && hasSource == hasBindings {
		return fmt.Errorf("template_snippets.%s: incremental requires exactly one of source or bindings_template", name)
	}
	if hasSource {
		source := incremental.Source
		if _, ok := cfg.WatchedResources[source]; !ok {
			return fmt.Errorf("template_snippets.%s: incremental.source %q does not name a watched resource", name, source)
		}
		if !slices.Contains(snippet.Requires, source) {
			return fmt.Errorf("template_snippets.%s: incremental.source %q must also appear in requires", name, source)
		}
	}
	if incremental.WhenAnyPathExists != nil && len(incremental.WhenAnyPathExists) == 0 {
		return fmt.Errorf("template_snippets.%s: incremental.when_any_path_exists must not be empty", name)
	}
	paths := make(map[string]struct{}, len(incremental.WhenAnyPathExists))
	for index, path := range incremental.WhenAnyPathExists {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("template_snippets.%s: incremental.when_any_path_exists[%d] is empty", name, index)
		}
		if _, duplicate := paths[path]; duplicate {
			return fmt.Errorf("template_snippets.%s: incremental.when_any_path_exists contains duplicate path %q", name, path)
		}
		paths[path] = struct{}{}
	}
	if len(paths) > 0 && slices.Contains(incremental.Effects, IncrementalEffectDeriveResource) {
		return fmt.Errorf("template_snippets.%s: incremental.when_any_path_exists cannot be combined with deriveResource", name)
	}
	return nil
}

func validateIncrementalMode(name string, incremental *IncrementalTemplate) error {
	switch incremental.Mode {
	case IncrementalModeScriggo:
		return nil
	case IncrementalModeResourceProjection:
		return validateResourceProjection(name, incremental)
	default:
		return fmt.Errorf("template_snippets.%s: incremental.mode contains unsupported value %q", name, incremental.Mode)
	}
}

func validateResourceProjection(name string, incremental *IncrementalTemplate) error {
	const field = "template_snippets.%s: incremental.resourceProjection"
	if incremental.Source != "" {
		return fmt.Errorf(field+" cannot set source", name)
	}
	if incremental.BindingsTemplate == "" {
		return fmt.Errorf(field+" requires bindings_template", name)
	}
	if incremental.WhenAnyPathExists != nil {
		return fmt.Errorf(field+" cannot set when_any_path_exists", name)
	}
	if incremental.Root != "" {
		return fmt.Errorf(field+" cannot set root", name)
	}
	if incremental.Consumes != nil {
		return fmt.Errorf(field+" cannot set consumes", name)
	}
	if incremental.OptionalConsumes != nil {
		return fmt.Errorf(field+" cannot set optional_consumes", name)
	}
	if len(incremental.Effects) != 1 || incremental.Effects[0] != IncrementalEffectPublishValue {
		return fmt.Errorf(field+" requires exactly effects [publishValue]", name)
	}
	return nil
}

type incrementalGroupDeclaration struct {
	publishes bool
	edges     map[string]struct{}
}

func validateIncrementalDependencies(cfg *Config) error {
	if err := validateIncrementalRootNames(cfg); err != nil {
		return err
	}
	groups, names := collectIncrementalGroups(cfg)
	for _, name := range names {
		if err := validateIncrementalSnippetDependencies(cfg, groups, name); err != nil {
			return err
		}
	}
	if err := validateIncrementalDependencyCycles(groups); err != nil {
		return err
	}
	return validateIncrementalRootBarriers(cfg, groups)
}

func collectIncrementalGroups(cfg *Config) (groups map[string]*incrementalGroupDeclaration, names []string) {
	groups = make(map[string]*incrementalGroupDeclaration)
	names = make([]string, 0, len(cfg.TemplateSnippets))
	for name, snippet := range cfg.TemplateSnippets {
		if snippet.Incremental == nil {
			continue
		}
		names = append(names, name)
		group := incrementalGroupName(name, snippet.Incremental)
		declaration := groups[group]
		if declaration == nil {
			declaration = &incrementalGroupDeclaration{edges: map[string]struct{}{}}
			groups[group] = declaration
		}
		if slices.Contains(snippet.Incremental.Effects, IncrementalEffectPublishValue) {
			declaration.publishes = true
		}
	}
	slices.Sort(names)
	return groups, names
}

func validateIncrementalSnippetDependencies(
	cfg *Config,
	groups map[string]*incrementalGroupDeclaration,
	name string,
) error {
	incremental := cfg.TemplateSnippets[name].Incremental
	group := incrementalGroupName(name, incremental)
	seen := make(map[string]string, len(incremental.Consumes)+len(incremental.OptionalConsumes))
	for _, dependency := range []struct {
		field    string
		groups   []string
		optional bool
	}{
		{field: "consumes", groups: incremental.Consumes},
		{field: "optional_consumes", groups: incremental.OptionalConsumes, optional: true},
	} {
		for _, target := range dependency.groups {
			err := validateIncrementalDependencyTarget(cfg, groups, seen, incrementalDependencyEdge{
				snippet: name, group: group, field: dependency.field,
				optional: dependency.optional, target: target,
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

type incrementalDependencyEdge struct {
	snippet  string
	group    string
	field    string
	optional bool
	target   string
}

func validateIncrementalDependencyTarget(
	cfg *Config,
	groups map[string]*incrementalGroupDeclaration,
	seen map[string]string,
	edge incrementalDependencyEdge,
) error {
	if edge.target == "" {
		return fmt.Errorf("template_snippets.%s: incremental.%s contains an empty group", edge.snippet, edge.field)
	}
	if previous, duplicate := seen[edge.target]; duplicate {
		return fmt.Errorf("template_snippets.%s: incremental.%s contains group %q already declared in %s",
			edge.snippet, edge.field, edge.target, previous)
	}
	seen[edge.target] = edge.field
	if edge.target == edge.group {
		return fmt.Errorf("template_snippets.%s: incremental.%s group %q depends on itself", edge.snippet, edge.field, edge.group)
	}
	producer := groups[edge.target]
	if producer == nil {
		if _, authenticated := cfg.AbsentIncrementalGroups[edge.target]; edge.optional && authenticated {
			return nil
		}
		return fmt.Errorf("template_snippets.%s: incremental.%s %q does not name an incremental group",
			edge.snippet, edge.field, edge.target)
	}
	if !producer.publishes {
		return fmt.Errorf("template_snippets.%s: incremental.%s group %q has no publishValue component",
			edge.snippet, edge.field, edge.target)
	}
	groups[edge.group].edges[edge.target] = struct{}{}
	return nil
}

func validateIncrementalRootNames(cfg *Config) error {
	names := make([]string, 0, len(cfg.TemplateSnippets))
	for name, snippet := range cfg.TemplateSnippets {
		if snippet.Incremental != nil && snippet.Incremental.Root != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		root := cfg.TemplateSnippets[name].Incremental.Root
		if strings.TrimSpace(root) != root {
			return fmt.Errorf("template_snippets.%s: incremental.root must not contain surrounding whitespace", name)
		}
	}
	return nil
}

func validateIncrementalRootBarriers(
	cfg *Config,
	groups map[string]*incrementalGroupDeclaration,
) error {
	rootGroups := make(map[string]map[string]struct{})
	for name, snippet := range cfg.TemplateSnippets {
		if snippet.Incremental == nil || snippet.Incremental.Root == "" {
			continue
		}
		root := snippet.Incremental.Root
		if rootGroups[root] == nil {
			rootGroups[root] = map[string]struct{}{}
		}
		rootGroups[root][incrementalGroupName(name, snippet.Incremental)] = struct{}{}
	}
	roots := make([]string, 0, len(rootGroups))
	for root := range rootGroups {
		roots = append(roots, root)
	}
	slices.Sort(roots)
	for _, root := range roots {
		members := rootGroups[root]
		groupNames := make([]string, 0, len(members))
		for group := range members {
			groupNames = append(groupNames, group)
		}
		slices.Sort(groupNames)
		for _, group := range groupNames {
			targets := maps.Clone(members)
			delete(targets, group)
			if path := incrementalDependencyPath(groups, group, targets, nil); len(path) > 0 {
				return fmt.Errorf(
					"incremental.root %q crosses a dependency barrier: %s",
					root,
					strings.Join(path, " -> "),
				)
			}
		}
	}
	return nil
}

func incrementalDependencyPath(
	groups map[string]*incrementalGroupDeclaration,
	group string,
	targets map[string]struct{},
	visiting map[string]struct{},
) []string {
	if len(targets) == 0 {
		return nil
	}
	if visiting == nil {
		visiting = map[string]struct{}{}
	}
	if _, seen := visiting[group]; seen {
		return nil
	}
	visiting[group] = struct{}{}
	defer delete(visiting, group)
	targetNames := make([]string, 0, len(groups[group].edges))
	for target := range groups[group].edges {
		targetNames = append(targetNames, target)
	}
	slices.Sort(targetNames)
	for _, target := range targetNames {
		if _, matched := targets[target]; matched {
			return []string{group, target}
		}
		if suffix := incrementalDependencyPath(groups, target, targets, visiting); len(suffix) > 0 {
			return append([]string{group}, suffix...)
		}
	}
	return nil
}

func incrementalGroupName(name string, incremental *IncrementalTemplate) string {
	if incremental.Group != "" {
		return incremental.Group
	}
	return name
}

func validateIncrementalDependencyCycles(groups map[string]*incrementalGroupDeclaration) error {
	const (
		incrementalGroupUnvisited = iota
		incrementalGroupVisiting
		incrementalGroupVisited
	)
	states := make(map[string]int, len(groups))
	stack := make([]string, 0, len(groups))
	var visit func(string) error
	visit = func(group string) error {
		states[group] = incrementalGroupVisiting
		stack = append(stack, group)
		targets := make([]string, 0, len(groups[group].edges))
		for target := range groups[group].edges {
			targets = append(targets, target)
		}
		slices.Sort(targets)
		for _, target := range targets {
			switch states[target] {
			case incrementalGroupVisiting:
				start := slices.Index(stack, target)
				cycle := append(slices.Clone(stack[start:]), target)
				return fmt.Errorf("incremental group dependency cycle: %s", strings.Join(cycle, " -> "))
			case incrementalGroupUnvisited:
				if err := visit(target); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		states[group] = incrementalGroupVisited
		return nil
	}
	groupNames := make([]string, 0, len(groups))
	for group := range groups {
		groupNames = append(groupNames, group)
	}
	slices.Sort(groupNames)
	for _, group := range groupNames {
		if states[group] == incrementalGroupUnvisited {
			if err := visit(group); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateIncrementalEffects(name string, effects []IncrementalEffect) error {
	seen := make(map[IncrementalEffect]struct{}, len(effects))
	for _, effect := range effects {
		switch effect {
		case IncrementalEffectDeriveResource, IncrementalEffectRecordEvent, IncrementalEffectBackendPlan,
			IncrementalEffectPublishValue, IncrementalEffectStatusPatch:
		default:
			return fmt.Errorf("template_snippets.%s: incremental.effects contains unsupported value %q", name, effect)
		}
		if _, duplicate := seen[effect]; duplicate {
			return fmt.Errorf("template_snippets.%s: incremental.effects contains duplicate value %q", name, effect)
		}
		seen[effect] = struct{}{}
	}
	return nil
}

// validateTestRequires checks one validation test's requires and
// requiresFields entries against the watchedResources keys.
func validateTestRequires(cfg *Config, name string, test *ValidationTest) error {
	for _, req := range test.Requires {
		if _, ok := cfg.WatchedResources[req]; !ok {
			return fmt.Errorf("validation_tests.%s: requires %q does not name a watched resource", name, req)
		}
	}
	for _, entry := range test.RequiresFields {
		key, fieldPath, ok := strings.Cut(entry, ".")
		if !ok || fieldPath == "" {
			return fmt.Errorf("validation_tests.%s: requiresFields entry %q must be of the form \"<watchedResource>.<field.path>\"", name, entry)
		}
		if _, ok := cfg.WatchedResources[key]; !ok {
			return fmt.Errorf("validation_tests.%s: requiresFields entry %q does not name a watched resource (first segment %q)", name, entry, key)
		}
	}
	return nil
}

// validatePodSelector validates the pod selector configuration.
func validatePodSelector(ps *PodSelector) error {
	if len(ps.MatchLabels) == 0 {
		return errors.New("match_labels cannot be empty")
	}

	for key, value := range ps.MatchLabels {
		if key == "" {
			return errors.New("match_labels key cannot be empty")
		}
		if value == "" {
			return fmt.Errorf("match_labels value for key %q cannot be empty", key)
		}
	}

	return nil
}

// validateLoggingConfig validates the logging configuration.
func validateLoggingConfig(lc *LoggingConfig) error {
	if lc.Level == "" {
		return nil // Empty is valid - means use LOG_LEVEL env var or default
	}

	switch strings.ToUpper(lc.Level) {
	case "TRACE", "DEBUG", logLevelInfo, "WARN", "WARNING", "ERROR":
		// WARNING is accepted as an alias for WARN.
		return nil
	default:
		return fmt.Errorf("level must be TRACE, DEBUG, INFO, WARN, or ERROR (case-insensitive), got %q", lc.Level)
	}
}

// validateDataplaneConfig validates the dataplane configuration.
// This validation is called AFTER setDefaults(), so production ports must be 1-65535.
// A value of 0 indicates defaults were not applied properly.
func validateDataplaneConfig(dc *DataplaneConfig) error {
	// Port validation - must not be 0 after defaults
	// See pkg/core/config/defaults.go for port handling strategy
	if dc.Port < 1 || dc.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535 (got %d, expected default %d)", dc.Port, DefaultDataplanePort)
	}

	// Path validations - must not be empty after defaults
	if dc.MapsDir == "" {
		return fmt.Errorf("maps_dir cannot be empty (expected default %q)", DefaultDataplaneMapsDir)
	}
	if dc.SSLCertsDir == "" {
		return fmt.Errorf("ssl_certs_dir cannot be empty (expected default %q)", DefaultDataplaneSSLCertsDir)
	}
	if dc.GeneralStorageDir == "" {
		return fmt.Errorf("general_storage_dir cannot be empty (expected default %q)", DefaultDataplaneGeneralStorageDir)
	}
	if dc.SecretsDir == "" {
		return fmt.Errorf("secrets_dir cannot be empty (expected default %q)", DefaultDataplaneSecretsDir)
	}
	if dc.ConfigFile == "" {
		return fmt.Errorf("config_file cannot be empty (expected default %q)", DefaultDataplaneConfigFile)
	}

	return nil
}

// validateWatchedResources validates the watched resources configuration.
func validateWatchedResources(resources map[string]WatchedResource) error {
	if len(resources) == 0 {
		return errors.New("at least one resource must be configured")
	}

	for name := range resources {
		resource := resources[name]
		if err := validateWatchedResource(name, &resource); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}

	return nil
}

// validateWatchedResource validates a single watched resource configuration.
func validateWatchedResource(name string, resource *WatchedResource) error {
	if resource.APIVersion == "" && len(resource.APIVersions) == 0 {
		return fmt.Errorf("resource %q: one of api_version or api_versions must be set", name)
	}
	if resource.APIVersion != "" && len(resource.APIVersions) > 0 {
		return fmt.Errorf("resource %q: api_version and api_versions are mutually exclusive", name)
	}
	for i, v := range resource.APIVersions {
		if v == "" {
			return fmt.Errorf("resource %q: api_versions[%d] cannot be empty", name, i)
		}
	}

	if resource.Resources == "" {
		return fmt.Errorf("resource %q: resources cannot be empty", name)
	}

	if len(resource.IndexBy) == 0 {
		return fmt.Errorf("resource %q: index_by must have at least one expression", name)
	}

	// Validate that index_by expressions are not empty strings
	for i, expr := range resource.IndexBy {
		if expr == "" {
			return fmt.Errorf("resource %q: index_by[%d] cannot be empty", name, i)
		}
	}

	return validateWatchNamespaces(name, resource)
}

// NamespaceIndexPath is the indexBy expression whose lookup key is a namespace.
const NamespaceIndexPath = "metadata.namespace"

var namespaceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validateWatchNamespaces(name string, resource *WatchedResource) error {
	if resource.Namespaces == nil {
		if resource.NamespacesSetting != "" {
			return fmt.Errorf("resource %q: namespaces_setting has no namespaces to describe; set namespaces or remove it", name)
		}
		return nil
	}
	if len(resource.Namespaces) == 0 {
		return fmt.Errorf("resource %q: namespaces is empty; list at least one namespace or remove the field to watch all namespaces", name)
	}
	// Without the namespace as first key a lookup outside the list would
	// silently find nothing instead of failing.
	if resource.IndexBy[0] != NamespaceIndexPath {
		return fmt.Errorf("resource %q: namespaces requires index_by to start with %s", name, NamespaceIndexPath)
	}
	seen := make(map[string]bool, len(resource.Namespaces))
	for i, namespace := range resource.Namespaces {
		if len(namespace) > 63 || !namespaceName.MatchString(namespace) {
			return fmt.Errorf("resource %q: namespaces[%d] %q is not a valid namespace name", name, i, namespace)
		}
		if seen[namespace] {
			return fmt.Errorf("resource %q: namespaces lists %q twice", name, namespace)
		}
		seen[namespace] = true
	}
	return nil
}

// validateHAProxyConfig validates the HAProxy configuration.
func validateHAProxyConfig(hc *HAProxyConfig) error {
	if hc.Template == "" {
		return errors.New("template cannot be empty")
	}

	return nil
}

// ValidateCredentials ensures all required credential fields are present and non-empty.
func ValidateCredentials(creds *Credentials) error {
	if creds == nil {
		return errors.New("credentials are nil")
	}

	if creds.DataplaneUsername == "" {
		return errors.New("dataplane_username cannot be empty")
	}

	if creds.DataplanePassword == "" {
		return errors.New("dataplane_password cannot be empty")
	}

	return nil
}
