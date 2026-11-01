package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/loader"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	chartv2loader "helm.sh/helm/v4/pkg/chart/v2/loader"
	"sigs.k8s.io/yaml"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
)

var (
	preflightValuesFiles    []string
	preflightNamespace      string
	preflightRelease        string
	preflightChartDir       string
	preflightAPIVersions    []string
	preflightSchemaDir      string
	preflightKubeconfig     string
	preflightPrepareUpgrade bool
	preflightPrepareTimeout time.Duration
)

// gatewayAPIVersion is always declared because the chart gates its Gateway API
// library on this capability. Rendering without it silently drops that library,
// so the check would validate a configuration the cluster will never receive.
const gatewayAPIVersion = "gateway.networking.k8s.io/v1/GatewayClass"

// sidecarCheckTimeout bounds one vector/varnish container run. Generous
// because a cold runner pulls the image first; the point is a ceiling, not a
// performance budget.
const sidecarCheckTimeout = 10 * time.Minute

var preflightExpectChartVersion string

var preflightCmd = &cobra.Command{
	Use:   "preflight",
	Short: "Validate a configuration against your own values before deploying it",
	Long: "Render the bundled Helm chart with your values and run the controller's own " +
		"load gate against the result: structural validation, effective-config resolution " +
		"and the bundled validationTests (which include `haproxy -c`).\n\n" +
		"Exits non-zero when the configuration would not load, so a delivery pipeline " +
		"fails instead of new controller pods staying NotReady.\n\n" +
		"The chart's own CI can only prove the DEFAULTS work — it never sees your values, " +
		"and several checks only fail once a feature is switched on.",
	RunE: runPreflight,
	// A failing check is a finding, not a usage error; main.go prints it.
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	preflightCmd.Flags().StringArrayVarP(&preflightValuesFiles, "values", "f", nil,
		"Values file to render with (repeatable, later wins). Required: the point of "+
			"this check is YOUR values, not the chart defaults.")
	preflightCmd.Flags().StringVarP(&preflightNamespace, "namespace", "n", defaultReleaseName,
		"Release namespace. Use the one you deploy to — chart output depends on it.")
	preflightCmd.Flags().StringVar(&preflightRelease, "release", defaultReleaseName,
		"Release name. Resource names are derived from it.")
	preflightCmd.Flags().StringVar(&preflightChartDir, "chart", "",
		"Chart directory. Defaults to $HAPTIC_CHART_DIR, then the chart embedded in this image.")
	preflightCmd.Flags().StringArrayVar(&preflightAPIVersions, "api-versions", nil,
		"Extra API versions the target cluster serves. "+gatewayAPIVersion+" is always included.")
	preflightCmd.Flags().StringVar(&preflightSchemaDir, "schema-dir", os.Getenv("HAPTIC_SCHEMA_DIR"),
		"Directory of Kubernetes API schemas for typed-resource access, for running "+
			"fully offline (default: read them from the target cluster). Also reads HAPTIC_SCHEMA_DIR.")
	preflightCmd.Flags().StringVar(&preflightKubeconfig, "kubeconfig", "",
		"Kubeconfig for reading the target cluster's API schemas (default: $KUBECONFIG, "+
			"then in-cluster credentials). Ignored with --schema-dir.")
	preflightCmd.Flags().StringVar(&preflightExpectChartVersion, "expect-chart-version", os.Getenv("HAPTIC_EXPECT_CHART_VERSION"),
		"Fail unless the chart being rendered has exactly this version. The pre-upgrade "+
			"hook sets it to the installing chart's version, so validating a DIFFERENT "+
			"chart (a drifted image tag) fails loudly instead of passing on the wrong "+
			"input. Also reads HAPTIC_EXPECT_CHART_VERSION.")
	preflightCmd.Flags().BoolVar(&preflightPrepareUpgrade, "prepare-upgrade", false,
		"After validation, roll an existing controller to this image before applying new templates.")
	preflightCmd.Flags().DurationVar(&preflightPrepareTimeout, "prepare-timeout", 5*time.Minute,
		"Time from preflight start by which controller preparation must finish; keep it below the Job deadline.")

	_ = preflightCmd.MarkFlagRequired("values")
	rootCmd.AddCommand(preflightCmd)
}

func runPreflight(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	prepareDeadline := time.Now().Add(preflightPrepareTimeout)
	logger := newValidateLogger()
	configureMemoryLimit(logger)

	// Schemas decide whether typed resource access works. Without any, the
	// render silently falls back to untyped and passes on a weaker check than
	// the controller runs — the one outcome this command must never produce.
	// Default to the live cluster: it is authoritative for the API surface
	// being deployed onto, including which optional CRDs are installed.
	schemas, err := preflightSchemas(logger)
	if err != nil {
		return err
	}

	chartDir, err := resolveChartDir(preflightChartDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "==> rendering %s with %s (release %s, namespace %s)\n",
		chartDir, strings.Join(preflightValuesFiles, ", "), preflightRelease, preflightNamespace)

	caps, err := preflightCapabilities(schemas, logger)
	if err != nil {
		return err
	}

	manifests, err := renderChartManifests(chartDir, preflightValuesFiles, preflightExpectChartVersion, caps)
	if err != nil {
		return err
	}

	configDocs, err := collectConfigDocuments(manifests)
	if err != nil {
		return err
	}

	// Hand the rendered objects to `validate` unchanged, so preflight and
	// validate cannot drift into checking different things.
	tmp, err := os.CreateTemp("", "haptic-preflight-*.yaml")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(configDocs); err != nil {
		return fmt.Errorf("writing rendered config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	fmt.Fprintln(os.Stderr, "==> running the load gate")
	validateConfigFiles = []string{tmp.Name()}
	results, err := validateAndReport(ctx, schemas, logger)
	if err != nil {
		// Nil results mean the gate never reached a verdict on the configuration.
		if results == nil {
			return fmt.Errorf("the check could not run, so this configuration is unverified: %w", err)
		}
		return fmt.Errorf("this configuration would not load — new controller pods would stay NotReady and the rollout would stall: %w", err)
	}

	if err := checkRenderedSidecarConfigs(ctx, manifests, results); err != nil {
		return err
	}
	if err := checkPreflightResources(ctx, tmp.Name(), schemas, logger); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "✓ configuration is valid for these values")
	if preflightPrepareUpgrade {
		return prepareControllerUpgrade(ctx, prepareDeadline, manifests, schemas, logger)
	}
	return nil
}

// preflightCapabilities builds the render Capabilities. Against a live
// cluster they come from discovery — the render must prune the same
// conditional subcharts the real install would, and a hardcoded list
// validates a SUPERSET of what deploys on clusters missing optional CRDs,
// which proves nothing about the subset. Offline (--schema-dir) keeps the
// old assumption-based list, with --api-versions as the escape hatch, and
// says so.
func preflightCapabilities(schemas schemaSource, logger *slog.Logger) (*common.Capabilities, error) {
	caps := common.DefaultCapabilities.Copy()
	if schemas.live == nil {
		caps.APIVersions = append(caps.APIVersions, gatewayAPIVersion)
		caps.APIVersions = append(caps.APIVersions, preflightAPIVersions...)
		logger.Warn("Offline run: assuming Gateway API is present; pass --api-versions to adjust",
			"assumed", gatewayAPIVersion)
		return caps, nil
	}

	_, resourceLists, err := schemas.live.discovery().ServerGroupsAndResources()
	if err != nil {
		// Partial discovery (a broken aggregated API) still names every
		// healthy group; treat it like helm does and keep what arrived.
		if resourceLists == nil {
			return nil, fmt.Errorf("discovering the cluster's API versions: %w", err)
		}
		logger.Warn("Partial API discovery; capabilities may be incomplete", "error", err)
	}
	versions := make([]string, 0, len(resourceLists)*8)
	for _, list := range resourceLists {
		versions = append(versions, list.GroupVersion)
		for i := range list.APIResources {
			versions = append(versions, list.GroupVersion+"/"+list.APIResources[i].Kind)
		}
	}
	caps.APIVersions = append(caps.APIVersions, versions...)
	caps.APIVersions = append(caps.APIVersions, preflightAPIVersions...)
	return caps, nil
}

// preflightSchemas picks the schema source: --schema-dir when given, the live
// cluster otherwise. Never neither — a run with no schemas would report a pass
// the controller's own load would not.
func preflightSchemas(logger *slog.Logger) (schemaSource, error) {
	if preflightSchemaDir != "" {
		schemas, err := newDirSchemaSource(preflightSchemaDir, logger)
		if err != nil {
			return schemaSource{}, err
		}
		// A missing or empty directory loads as an empty fetcher rather than an
		// error — deliberate for the controller, wrong here: it degrades the
		// check to untyped access without saying so.
		if schemas.dir.Len() == 0 {
			return schemaSource{}, fmt.Errorf("--schema-dir %s holds no schemas, so the render would fall back to "+
				"untyped access and check less than the controller does\n"+
				"Hint: point it at a directory of CRD manifests or OpenAPI v3 schemas, or drop the flag to read "+
				"them from the cluster you deploy to", preflightSchemaDir)
		}
		return schemas, nil
	}
	schemas, err := newLiveSchemaSource(preflightKubeconfig)
	if err != nil {
		return schemaSource{}, fmt.Errorf("%w\nHint: preflight reads the API schemas from the cluster you deploy to, "+
			"so it can tell which optional CRDs are installed. Pass --schema-dir <dir> to run fully offline", err)
	}
	return schemas, nil
}

// renderChartManifests renders the chart in-process with the operator's values.
// Template-only, exactly like `helm template`: no cluster access, so `lookup`
// returns empty — the same blind spot a GitOps render has.
func renderChartManifests(chartDir string, valuesFiles []string, expectVersion string, caps *common.Capabilities) (map[string]string, error) {
	chrt, err := loader.Load(chartDir)
	if err != nil {
		return nil, fmt.Errorf("loading chart %s: %w", chartDir, err)
	}
	c, ok := chrt.(*chartv2.Chart)
	if !ok {
		return nil, fmt.Errorf("chart %s: unsupported chart apiVersion (got %T)", chartDir, chrt)
	}

	// The version guard is what makes the embedded-chart default sound in the
	// pre-upgrade hook: without it, an image tag drifted from the chart being
	// installed would validate the WRONG chart and pass on the wrong input —
	// worse than no gate. Never warn-and-continue here.
	if expectVersion != "" && c.Metadata.Version != expectVersion {
		return nil, fmt.Errorf(
			"chart version mismatch: this image embeds chart %s but the release being installed is chart %s.\n"+
				"The controller image tag has drifted from the chart version (they are released in lockstep).\n"+
				"Fix the image override, or disable the pre-rollout gate if the drift is deliberate",
			c.Metadata.Version, expectVersion)
	}

	overrides := map[string]any{}
	for _, f := range valuesFiles {
		// Clean the path to prevent traversal, as loadConfigFromFiles does.
		data, readErr := os.ReadFile(filepath.Clean(f))
		if readErr != nil {
			return nil, fmt.Errorf("reading values file %s: %w", f, readErr)
		}
		var vals map[string]any
		if err := yaml.Unmarshal(data, &vals); err != nil {
			return nil, fmt.Errorf("parsing values file %s: %w", f, err)
		}
		overrides = chartv2loader.MergeMaps(overrides, vals)
	}

	return renderChart(c, overrides, common.ReleaseOptions{
		Name:      preflightRelease,
		Namespace: preflightNamespace,
		Revision:  1,
		IsInstall: true,
	}, caps)
}

// The HAProxy load gate cannot validate other processes' configuration formats.
func checkRenderedSidecarConfigs(ctx context.Context, manifests map[string]string, results *testrunner.TestResults) error {
	configs, err := collectSidecarConfigs(manifests, results)
	if err != nil {
		return err
	}
	if len(configs) == 0 {
		return nil
	}

	runtimeBin, err := containerRuntime()
	if err != nil {
		return err
	}
	if runtimeBin == "" {
		fmt.Fprintln(os.Stderr, "WARNING: no container runtime found — skipping the vector and varnish checks")
		return nil
	}

	return runSidecarChecks(ctx, configs, runtimeBin, runSidecarCommand)
}

type sidecarCommandRunner func(context.Context, string, ...string) ([]byte, error)

func runSidecarCommand(ctx context.Context, bin string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, bin, args...).CombinedOutput()
}

func runSidecarChecks(ctx context.Context, configs []sidecarConfig, runtimeBin string, run sidecarCommandRunner) error {
	dir, err := os.MkdirTemp("", "haptic-preflight-sidecars-")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	for i := range configs {
		config := &configs[i]
		configDir := filepath.Join(dir, strconv.Itoa(i))
		// The host-only parent stays private; non-root image users can read this mount.
		if err := os.Mkdir(configDir, 0o755); err != nil {
			return fmt.Errorf("creating sidecar config directory: %w", err)
		}
		if err := os.Chmod(configDir, 0o755); err != nil {
			return fmt.Errorf("setting sidecar config directory permissions: %w", err)
		}
		fmt.Fprintf(os.Stderr, "==> validating %s/%s with %s\n", config.testName, config.name, config.image)
		if err := writeAndRun(ctx, configDir, config, runtimeBin, run); err != nil {
			return fmt.Errorf("%s/%s fails validation with %s; fix the rendered config: %w",
				config.testName, config.name, config.image, err)
		}
	}
	return nil
}

func writeAndRun(ctx context.Context, dir string, config *sidecarConfig, runtimeBin string, run sidecarCommandRunner) error {
	path := filepath.Join(dir, config.name)
	if err := os.WriteFile(path, []byte(config.content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return fmt.Errorf("setting %s permissions: %w", path, err)
	}
	// Bound the run: a stalled image pull or a wedged compiler would otherwise
	// hang the pipeline step forever with nothing to read.
	ctx, cancel := context.WithTimeout(ctx, sidecarCheckTimeout)
	defer cancel()

	args := []string{"run", "--rm", "-v", dir + ":/w:ro"}
	if config.kind == vectorSidecar {
		args = append(args, config.image, "validate", "--no-environment", "/w/"+config.name)
	} else {
		args = append(args, "--cap-drop=ALL", "--cap-add=IPC_LOCK", "--security-opt=no-new-privileges")
		// Varnish resolves cluster DNS names at compile time, outside the cluster.
		for _, host := range vclBackendHosts(config.content) {
			args = append(args, "--add-host", host+":127.0.0.1")
		}
		args = append(args, config.image, "varnishd", "-C", "-f", "/w/"+config.name)
	}
	out, err := run(ctx, runtimeBin, args...)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("timed out after %s (image pull or compiler stalled)\n%s", sidecarCheckTimeout, out)
		}
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

var vclHostPattern = regexp.MustCompile(`\.host\s*=\s*"([^"]+)"`)

func vclBackendHosts(vcl string) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, m := range vclHostPattern.FindAllStringSubmatch(vcl, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			hosts = append(hosts, m[1])
		}
	}
	sort.Strings(hosts)
	return hosts
}

// containerRuntime returns "" when there is none, which downgrades the sidecar
// checks to a warning. An explicitly configured runtime that isn't there is an
// error instead — silently skipping a check someone asked for is how a gate
// stops gating.
func containerRuntime() (string, error) {
	if bin := os.Getenv("HAPTIC_CONTAINER_RUNTIME"); bin != "" {
		path, err := exec.LookPath(bin)
		if err != nil {
			return "", fmt.Errorf("HAPTIC_CONTAINER_RUNTIME=%s is not executable: %w", bin, err)
		}
		return path, nil
	}
	for _, bin := range []string{"docker", "podman"} {
		if path, err := exec.LookPath(bin); err == nil {
			return path, nil
		}
	}
	return "", nil
}

// collectConfigDocuments picks the HAProxyTemplateConfig and its
// HAProxyTemplateLibrary objects out of the rendered manifests and returns them
// as a single multi-document stream.
//
// Document order carries no meaning: the consumer assembles by
// spec.libraryRefs. Ordering here is only for reproducibility.
func collectConfigDocuments(manifests map[string]string) (string, error) {
	names := make([]string, 0, len(manifests))
	for name := range manifests {
		names = append(names, name)
	}
	// Deterministic order, so a failure reproduces from the same inputs.
	sort.Strings(names)

	var docs []string
	for _, name := range names {
		content := manifests[name]
		if strings.TrimSpace(content) == "" {
			continue
		}
		for _, doc := range strings.Split(content, "\n---") {
			if !strings.Contains(doc, "kind: HAProxyTemplateConfig") &&
				!strings.Contains(doc, "kind: HAProxyTemplateLibrary") {
				continue
			}
			docs = append(docs, strings.TrimSpace(doc))
		}
	}
	if len(docs) == 0 {
		return "", fmt.Errorf("the render produced no HAProxyTemplateConfig — is controller.config set in your values?")
	}
	return strings.Join(docs, "\n---\n") + "\n", nil
}
