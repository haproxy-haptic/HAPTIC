// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/kindutil"
	"github.com/haproxy-haptic/HAPTIC/tests/process"
	"github.com/haproxy-haptic/HAPTIC/tests/scenarios"
)

const defaultRelease = "haptic"

const upgradeScenario = "chart-upgrade"
const checkBaselinesScenario = "check-upgrade-baselines"
const defaultsScenario = "helm-defaults"
const gitopsScenario = "gitops-lifecycle"
const secretsScenario = "release-namespace-secrets"

func main() {
	if err := mainError(); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		var phase *scenarios.PhaseError
		if errors.As(err, &phase) {
			os.Exit(phase.Code)
		}
		os.Exit(1)
	}
}

func mainError() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return run(ctx, os.Args[1:], os.Stdout)
}

type options struct {
	name          string
	cluster       string
	image         string
	nodeImage     string
	namespace     string
	release       string
	artifacts     string
	keep          bool
	baseline      string
	certManager   string
	listBaselines bool
	spoaTag       string
	timeout       time.Duration
	provider      string
	certificates  string
}

func parseOptions(args []string) (options, error) {
	var options options
	if len(args) == 0 {
		return options, errors.New("usage: test-infra install-without-gateway-api [flags]")
	}
	options.name = args[0]
	if options.name != "install-without-gateway-api" && options.name != upgradeScenario && options.name != checkBaselinesScenario && options.name != defaultsScenario && options.name != gitopsScenario && options.name != secretsScenario {
		return options, fmt.Errorf("unknown scenario %q", options.name)
	}
	options.setDefaults()
	flags := flag.NewFlagSet(options.name, flag.ContinueOnError)
	flags.StringVar(&options.cluster, "cluster", options.cluster, "fresh Kind cluster name")
	flags.StringVar(&options.image, "image", options.image, "local controller image")
	flags.StringVar(&options.nodeImage, "kind-image", os.Getenv("KIND_NODE_IMAGE"), "Kind node image")
	flags.StringVar(&options.namespace, "namespace", options.namespace, "test namespace")
	flags.StringVar(&options.release, "release", options.release, "Helm release")
	flags.StringVar(&options.artifacts, "artifacts", options.artifacts, "artifact directory")
	flags.BoolVar(&options.keep, "keep", os.Getenv("KEEP_CLUSTER") == "true", "keep the owned cluster")
	flags.BoolVar(&options.keep, "keep-cluster", options.keep, "keep the owned cluster")
	if err := options.addScenarioFlags(flags); err != nil {
		return options, err
	}
	if err := flags.Parse(args[1:]); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	err := options.validate()
	return options, err
}

func (options *options) setDefaults() {
	options.cluster = envDefault("PLAIN_CLUSTER_NAME", "haptic-plain")
	options.image = "haptic:test"
	options.namespace = defaultRelease
	options.release = defaultRelease
	if options.name == gitopsScenario {
		options.cluster = ""
	}
	if options.name == secretsScenario {
		options.cluster = envDefault("SECRETS_CLUSTER_NAME", "haptic-secrets")
	}
	if options.name == defaultsScenario {
		options.cluster = envDefault("CLUSTER_NAME", "helm-defaults")
		options.namespace = envDefault("NAMESPACE", "haptic")
		options.release = envDefault("RELEASE_NAME", "haptic")
		options.image = os.Getenv("IMAGE")
		options.spoaTag = os.Getenv("SPOA_TAG")
	}
	if options.name == upgradeScenario || options.name == checkBaselinesScenario {
		options.cluster = envDefault("UPGRADE_CLUSTER_NAME", "haptic-upgrade")
		options.namespace = envDefault("UPGRADE_NAMESPACE", "haptic")
		options.release = envDefault("UPGRADE_RELEASE_NAME", "haptic")
		options.image = envDefault("UPGRADE_IMAGE_REPOSITORY", "haptic") + ":" + envDefault("UPGRADE_IMAGE_TAG", "test")
		options.artifacts = os.Getenv("UPGRADE_ARTIFACT_DIR")
	}
}

func (options *options) addScenarioFlags(flags *flag.FlagSet) error {
	if options.name == gitopsScenario {
		flags.StringVar(&options.provider, "provider", "", "GitOps provider: argo or flux")
		flags.StringVar(&options.certificates, "certificates", "external", "certificate source: external or cert-manager")
		if options.nodeImage == "" {
			options.nodeImage = "kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0"
		}
	}
	if options.name == defaultsScenario {
		flags.StringVar(&options.certManager, "cert-manager-version", envDefault("CERT_MANAGER_VERSION", scenarios.CertManagerVersion), "cert-manager version")
		seconds, err := time.ParseDuration(envDefault("TIMEOUT", "300") + "s")
		if err != nil {
			return fmt.Errorf("TIMEOUT: %w", err)
		}
		flags.DurationVar(&options.timeout, "timeout", seconds, "readiness timeout")
	}
	if options.name == upgradeScenario || options.name == checkBaselinesScenario {
		flags.StringVar(&options.baseline, "baseline", os.Getenv("BASELINE_CHART_VERSION"), "released chart version")
		flags.StringVar(&options.certManager, "cert-manager-version", envDefault("CERT_MANAGER_VERSION", scenarios.CertManagerVersion), "cert-manager version")
		flags.BoolVar(&options.listBaselines, "list-baselines", false, "list published upgrade baselines")
	}
	return nil
}

func (options *options) validate() error {
	if options.name == defaultsScenario && options.timeout <= 0 {
		return errors.New("readiness timeout must be positive")
	}
	if options.name == gitopsScenario {
		if options.namespace != defaultRelease || options.release != defaultRelease {
			return errors.New("GitOps fixtures require namespace and release haptic")
		}
		if (options.provider != "argo" && options.provider != "flux") || (options.certificates != "external" && options.certificates != "cert-manager") {
			return errors.New("GitOps requires --provider argo|flux and --certificates external|cert-manager")
		}
		if !strings.HasPrefix(options.cluster, "haptic-gitops-") || options.artifacts == "" {
			return errors.New("GitOps requires --cluster haptic-gitops-<name> and --artifacts <directory>")
		}
	}
	return nil
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) > 0 && args[0] == "blackhole-backends" {
		return runBlackhole(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "install-cni" {
		return runInstallCNI(ctx, args[1:])
	}
	options, err := parseOptions(args)
	if err != nil {
		return err
	}
	root, err := kindutil.RepoRoot()
	if err != nil {
		return err
	}
	if (options.name == upgradeScenario || options.name == checkBaselinesScenario) && (options.baseline == "" || options.listBaselines || options.name == checkBaselinesScenario) {
		return runBaselines(ctx, root, &options, output)
	}
	return runScenario(ctx, root, &options, output)
}

func runScenario(ctx context.Context, root string, options *options, output io.Writer) error {
	artifacts, err := scenarioArtifacts(root, options)
	if err != nil {
		return err
	}
	repository, tag, series, err := controllerImage(options)
	if err != nil {
		return err
	}
	images, err := kindutil.LoadChartImages(series)
	if err != nil {
		return err
	}
	environment := kindutil.DockerEnvironment(os.Getenv)
	if options.name == gitopsScenario {
		environment["KIND_EXPERIMENTAL_DOCKER_NETWORK"] = options.cluster
	}
	runner := process.Executor{}
	var readyTimeout time.Duration
	if options.name == defaultsScenario {
		readyTimeout = 120 * time.Second
	}
	cluster, err := kindutil.NewCluster(runner, &kindutil.ClusterOptions{Name: options.cluster, Kubeconfig: filepath.Join(artifacts, options.cluster+".kubeconfig"), Artifacts: artifacts, Environment: environment, ReadyTimeout: readyTimeout})
	if err != nil {
		return err
	}
	session := &scenarios.Session{Runner: runner, Cluster: cluster, Client: cluster.Client(options.namespace), Root: root, Namespace: options.namespace, Release: options.release, Artifacts: artifacts, ImageRepository: repository, ImageTag: tag, HAProxyVersion: images.HAProxyVersion, SPOAImage: images.SPOAHub, Log: output}
	if options.name == gitopsScenario {
		session.CommandTimeout = 10 * time.Minute
	}
	return execute(ctx, session, options)
}

func execute(ctx context.Context, session *scenarios.Session, options *options) (result error) {
	defer func() {
		if result != nil {
			diagnostics, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			session.Diagnostics(diagnostics)
			cancel()
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if options.keep {
			session.Infof("Kept cluster %s; kubeconfig=%s; artifacts=%s", session.Cluster.Name, session.Client.Kubeconfig, session.Artifacts)
		} else {
			result = errors.Join(result, session.Cluster.Close(cleanup))
		}
	}()
	session.Infof("Creating cluster %s; artifacts=%s", session.Cluster.Name, session.Artifacts)
	if err := session.Cluster.Create(ctx, options.nodeImage); err != nil {
		return err
	}
	if options.name == upgradeScenario {
		return session.ChartUpgrade(ctx, options.baseline, options.certManager)
	}
	if options.name == defaultsScenario {
		return session.HelmDefaults(ctx, &scenarios.DefaultsOptions{Image: options.image, CertManagerVersion: options.certManager, SPOATag: options.spoaTag, Timeout: options.timeout})
	}
	if options.name == gitopsScenario {
		return session.GitOps(ctx, &scenarios.GitOpsOptions{Provider: options.provider, Certificates: options.certificates, Image: options.image})
	}
	if options.name == secretsScenario {
		return session.ReleaseNamespaceSecrets(ctx)
	}
	return session.InstallWithoutGatewayAPI(ctx)
}

func controllerImage(options *options) (repository, tag, series string, err error) {
	series = os.Getenv("HAPTIC_HAPROXY_VERSION")
	if options.image == "" && options.name == defaultsScenario {
		return "", "", series, nil
	}
	repository, tag, err = imageParts(options.image)
	if err != nil {
		return "", "", "", err
	}
	if options.name == defaultsScenario {
		if base, version, found := strings.Cut(tag, "-haproxy"); found {
			tag, series = base, version
		}
	}
	return repository, tag, series, nil
}

func imageParts(image string) (repository, tag string, err error) {
	colon := strings.LastIndex(image, ":")
	if colon <= 0 || strings.Contains(image, "@") || colon <= strings.LastIndex(image, "/") || colon == len(image)-1 {
		return "", "", fmt.Errorf("controller image %q needs a repository and tag", image)
	}
	return image[:colon], image[colon+1:], nil
}

func runBaselines(ctx context.Context, root string, options *options, output io.Writer) error {
	repository, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer repository.Close()
	version, err := repository.ReadFile("VERSION")
	if err != nil {
		return err
	}
	baselines, err := scenarios.DiscoverUpgradeBaselines(ctx, http.DefaultClient, strings.TrimSpace(string(version)))
	if err != nil {
		return err
	}
	if options.listBaselines {
		_, err = fmt.Fprintln(output, strings.Join(baselines, "\n"))
		return err
	}
	if options.name == checkBaselinesScenario {
		if err := checkUpgradeWorkflows(repository, baselines); err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, "Upgrade matrix covers:", strings.Join(baselines, ", "))
		return err
	}
	for _, baseline := range baselines {
		selected := *options
		selected.baseline = baseline
		selected.cluster = options.cluster + "-" + strings.ReplaceAll(baseline, ".", "-")
		if err := runScenario(ctx, root, &selected, output); err != nil {
			return err
		}
	}
	return nil
}

func checkUpgradeWorkflows(repository *os.Root, baselines []string) error {
	const workflow = ".github/workflows/ci-speed-proof.yml"
	content, err := repository.ReadFile(workflow)
	if err != nil {
		return err
	}
	if err := scenarios.CheckGitHubUpgradeMatrix(baselines, content); err != nil {
		return fmt.Errorf("%s: %w", workflow, err)
	}
	return nil
}
