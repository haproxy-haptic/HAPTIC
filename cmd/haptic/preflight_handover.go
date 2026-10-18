package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/haproxy-haptic/HAPTIC/pkg/buildinfo"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/rollout"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

const controllerContainer = "controller"

func prepareControllerUpgrade(ctx context.Context, deadline time.Time, manifests map[string]string, schemas schemaSource, logger *slog.Logger) error {
	if schemas.live == nil {
		return errors.New("--prepare-upgrade requires access to the target cluster; remove --schema-dir")
	}
	target, err := renderedController(manifests)
	if err != nil {
		return err
	}
	current, err := schemas.live.clientset.AppsV1().Deployments(preflightNamespace).Get(ctx, target.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		deployments, listErr := schemas.live.clientset.AppsV1().Deployments(preflightNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + preflightRelease})
		if listErr != nil {
			return fmt.Errorf("find existing release controller: %w", listErr)
		}
		for i := range deployments.Items {
			if rollout.Container(deployments.Items[i].Spec.Template.Spec.Containers, controllerContainer) != nil {
				return fmt.Errorf("existing controller Deployment %s differs from target %s; keep the controller name unchanged during this upgrade", deployments.Items[i].Name, target.Name)
			}
		}
		fmt.Fprintln(os.Stderr, "✓ no existing controller requires preparation")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read existing controller Deployment: %w", err)
	}
	if current.Labels["app.kubernetes.io/instance"] != preflightRelease {
		return errors.New("existing controller Deployment belongs to a different release")
	}
	identity := buildinfo.Identity{Version: version, SourceHash: sourceHash, HAProxyVersion: os.Getenv("HAPROXY_MINOR")}
	if err := identity.Validate(); err != nil {
		return err
	}
	if preflightPrepareTimeout <= 0 {
		return errors.New("--prepare-timeout must be positive")
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return prepareExistingController(ctx, current, target, schemas, identity, logger)
}

func prepareExistingController(ctx context.Context, current, target *appsv1.Deployment, schemas schemaSource, identity buildinfo.Identity, logger *slog.Logger) error {
	oldContainer := rollout.Container(current.Spec.Template.Spec.Containers, controllerContainer)
	nextContainer := rollout.Container(target.Spec.Template.Spec.Containers, controllerContainer)
	if oldContainer == nil || nextContainer == nil {
		return errors.New("controller Deployment has no controller container")
	}
	configName, err := controllerConfigName(oldContainer)
	if err != nil {
		return err
	}
	targetConfigName, err := controllerConfigName(nextContainer)
	if err != nil {
		return err
	}
	if configName != targetConfigName {
		return fmt.Errorf("controller config name changes from %s to %s; keep controller.configName unchanged during this upgrade", configName, targetConfigName)
	}
	plan := &rollout.Plan{Client: schemas.live.clientset, Deployment: current, Container: controllerContainer,
		Image: nextContainer.Image, PullPolicy: nextContainer.ImagePullPolicy, Identity: identity,
		ReadIdentity: rollout.ReadIdentity(controllerContainer)}
	if err := plan.VerifyUnchangedImage(ctx); err != nil {
		return err
	}
	snapshot, err := readLiveConfiguration(ctx, schemas.live, preflightNamespace, configName)
	if err != nil {
		return err
	}
	compatible, err := assessLiveConfiguration(ctx, snapshot, schemas, logger)
	if err != nil {
		return err
	}
	if err := snapshot.requireUnchanged(ctx, schemas.live); err != nil {
		return err
	}
	if !compatible {
		fmt.Fprintln(os.Stderr, "Existing configuration cannot load in the new controller; applying the validated incoming set through the startup recovery gate.")
		return nil
	}
	fmt.Fprintln(os.Stderr, "==> preparing existing controller before template application")

	if err := plan.Run(ctx); err != nil {
		return err
	}
	if err := snapshot.requireUnchanged(ctx, schemas.live); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "✓ every existing controller runs the validated image; templates can now be applied")
	return nil
}

func controllerConfigName(container *corev1.Container) (string, error) {
	name, explicit := defaultCRDName, false
	for i, arg := range container.Args {
		if strings.HasPrefix(arg, "--crd-name=") {
			name = strings.TrimPrefix(arg, "--crd-name=")
			explicit = true
		}
		if arg == "--crd-name" && i+1 < len(container.Args) {
			name = container.Args[i+1]
			explicit = true
		}
	}
	if !explicit {
		for _, value := range container.Env {
			if value.Name != "CRD_NAME" {
				continue
			}
			if value.ValueFrom != nil {
				return "", errors.New("existing CRD_NAME uses valueFrom; set a literal controller config name before preparing the upgrade")
			}
			name = value.Value
		}
	}
	if name == "" {
		return "", errors.New("existing controller has an empty CRD_NAME")
	}
	return name, nil
}

func renderedController(manifests map[string]string) (*appsv1.Deployment, error) {
	var found *appsv1.Deployment
	for name, manifest := range manifests {
		// Helm's own rule for release notes; they are not YAML.
		if strings.HasSuffix(name, "NOTES.txt") {
			continue
		}
		deployments, err := renderedDeployments(manifest)
		if err != nil {
			return nil, err
		}
		for i := range deployments {
			deployment := &deployments[i]
			if deployment.Labels["app.kubernetes.io/instance"] != preflightRelease || rollout.Container(deployment.Spec.Template.Spec.Containers, controllerContainer) == nil {
				continue
			}
			if found != nil {
				return nil, errors.New("chart renders more than one controller Deployment")
			}
			found = deployment
		}
	}
	if found == nil {
		return nil, errors.New("chart renders no controller Deployment")
	}
	return found, nil
}

func renderedDeployments(manifest string) ([]appsv1.Deployment, error) {
	decoder := yamlutil.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	var deployments []appsv1.Deployment
	for {
		var object map[string]any
		if err := decoder.Decode(&object); errors.Is(err, io.EOF) {
			return deployments, nil
		} else if err != nil {
			return nil, err
		}
		if object["kind"] != "Deployment" {
			continue
		}
		data, err := yaml.Marshal(object)
		if err != nil {
			return nil, err
		}
		var deployment appsv1.Deployment
		if err := yaml.Unmarshal(data, &deployment); err != nil {
			return nil, err
		}
		deployments = append(deployments, deployment)
	}
}

func assessLiveConfiguration(ctx context.Context, snapshot *liveConfiguration, schemas schemaSource, logger *slog.Logger) (bool, error) {
	if len(snapshot.missing) > 0 {
		return false, nil
	}
	if err := snapshot.validateAssembly(); err != nil {
		logger.Info("Existing configuration cannot be assembled; using validated startup recovery", "error", err)
		return false, nil
	}
	data, err := snapshot.documents()
	if err != nil {
		return false, err
	}
	file, err := os.CreateTemp("", "haptic-live-config-*.yaml")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	previous := validateConfigFiles
	validateConfigFiles = []string{file.Name()}
	defer func() { validateConfigFiles = previous }()
	results, err := validateAndReport(ctx, schemas, logger)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err == nil {
		return true, nil
	}
	if rejectedConfiguration(results, err) {
		return false, nil
	}
	return false, fmt.Errorf("could not assess existing configuration; incoming templates remain unapplied: %w", err)
}

func rejectedConfiguration(results *testrunner.TestResults, err error) bool {
	var configErr *configurationError
	var compileErr *templating.CompilationError
	return errors.As(err, &configErr) || errors.As(err, &compileErr) || errors.Is(err, errNoValidationTests) || resultsHaveConfigRejection(results)
}

func resultsHaveConfigRejection(results *testrunner.TestResults) bool {
	if results == nil {
		return false
	}
	for i := range results.TestResults {
		if results.TestResults[i].Passed {
			continue
		}
		for _, assertion := range results.TestResults[i].Assertions {
			if !assertion.Passed && assertion.ConfigRejected {
				return true
			}
		}
	}
	return false
}
