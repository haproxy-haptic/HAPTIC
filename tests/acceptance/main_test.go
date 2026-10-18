//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/tests/kindutil"
	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

func init() {
	// Register HAProxyTemplateConfig CRD scheme with the global client-go scheme
	// This allows the e2e-framework to understand our custom resources
	if err := haproxyv1alpha1.AddToScheme(clientgoscheme.Scheme); err != nil {
		panic(fmt.Sprintf("failed to register haproxy scheme: %v", err))
	}
}

// TestMain is the entry point for acceptance tests.
// It sets up the test environment with a kind cluster and ensures
// all Setup/Finish actions are properly executed.
//
// When running in CI with sharding (detected via SKIP_PARALLEL_RUNNER=true),
// the cluster is pre-created by helm/kind-action and this function only
// configures the test environment to use the existing cluster.
func TestMain(m *testing.M) {
	// Create test environment with parallel execution enabled
	testEnv = env.NewParallel()

	// Check if running in CI sharding mode (cluster pre-created by helm/kind-action)
	if os.Getenv("SKIP_PARALLEL_RUNNER") == "true" {
		setupForCISharding()
	} else {
		setupForLocalDevelopment()
	}

	os.Exit(testEnv.Run(m))
}

// setupForCISharding configures the test environment to use an existing cluster
// created by helm/kind-action in CI. The cluster, image, and CRD are already set up.
func setupForCISharding() {
	// In CI mode, KUBECONFIG is already set by helm/kind-action
	// Just validate the cluster is accessible
	testEnv.Setup(
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			// Use the kubeconfig from environment (set by helm/kind-action)
			kubeconfigPath := os.Getenv("KUBECONFIG")
			if kubeconfigPath == "" {
				kubeconfigPath = os.Getenv("HOME") + "/.kube/config"
			}

			cfg.WithKubeconfigFile(kubeconfigPath)

			// Validate cluster is accessible
			client, err := cfg.NewClient()
			if err != nil {
				return ctx, fmt.Errorf("failed to create client: %w", err)
			}

			var nodeList corev1.NodeList
			if err := client.Resources().List(ctx, &nodeList); err != nil {
				return ctx, fmt.Errorf("SAFETY CHECK FAILED: Cannot list nodes: %w", err)
			}
			if len(nodeList.Items) == 0 {
				return ctx, fmt.Errorf("SAFETY CHECK FAILED: Cluster has no nodes")
			}

			// Create shared clientset with rate limiting disabled for parallel tests
			if err := initSharedClientset(client.RESTConfig()); err != nil {
				return ctx, fmt.Errorf("failed to create shared clientset: %w", err)
			}

			return ctx, nil
		},
	)
}

// initSharedClientset creates the shared Kubernetes clientset with rate limiting disabled.
// This must be called during environment setup before any tests run.
func initSharedClientset(restConfig *rest.Config) error {
	configCopy := rest.CopyConfig(restConfig)
	configCopy.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()

	clientset, err := kubernetes.NewForConfig(configCopy)
	if err != nil {
		return err
	}
	sharedClientset = clientset

	// Also store the REST config for operations that need it (like pod exec)
	SetSharedRESTConfig(configCopy)

	return nil
}

func initializeClusterResources(ctx context.Context, cluster *kindutil.Cluster) error {
	if err := cluster.LoadImages(ctx, ControllerImageName); err != nil {
		return err
	}
	client := cluster.Client("")
	if result, err := client.Run(ctx, nil, "apply", "-f", CRDDirectory); err != nil {
		return fmt.Errorf("install CRDs: %w: %s", err, result.Combined)
	}
	return client.WaitCRDsEstablished(ctx, RequiredCRDs, time.Minute)
}

func setupForLocalDevelopment() {
	var owned *kindutil.Cluster
	testEnv.Setup(func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		var err error
		owned, err = localAcceptanceCluster(ctx)
		if err != nil {
			return ctx, err
		}
		cfg.WithKubeconfigFile(owned.Kubeconfig)
		client, err := cfg.NewClient()
		if err != nil {
			return ctx, err
		}
		var nodes corev1.NodeList
		if err := client.Resources().List(ctx, &nodes); err != nil {
			return ctx, err
		}
		if len(nodes.Items) == 0 {
			return ctx, fmt.Errorf("test cluster has no nodes")
		}
		if err := initializeClusterResources(ctx, owned); err != nil {
			return ctx, err
		}
		return ctx, initSharedClientset(client.RESTConfig())
	})
	testEnv.Finish(func(ctx context.Context, _ *envconf.Config) (context.Context, error) {
		if owned == nil || os.Getenv("CI") == "true" || os.Getenv("KEEP_CLUSTER") == "true" {
			return ctx, nil
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		return ctx, owned.Close(cleanup)
	})
}

// getKindNodeImage returns the Kind node image to use for acceptance tests.
// It checks the KIND_NODE_IMAGE environment variable and falls back to a default
// known-working version (v1.32.0) if not set.
//
// The default v1.32.0 is used instead of v1.32.1 because v1.32.1 has a bug
// with containerd snapshotter detection that causes image loading to fail.
// See: https://github.com/kubernetes-sigs/kind/issues/3871
func getKindNodeImage() string {
	if image := os.Getenv("KIND_NODE_IMAGE"); image != "" {
		return image
	}
	return "kindest/node:v1.32.0"
}

func localAcceptanceCluster(ctx context.Context) (*kindutil.Cluster, error) {
	runner := process.Executor{}
	environment := kindutil.DockerEnvironment(os.Getenv)
	resumed, found, err := kindutil.ResumeCluster(ctx, runner, ClusterName, environment)
	if err != nil {
		return nil, err
	}
	if found {
		return resumed, nil
	}
	artifacts, err := os.MkdirTemp("", "haptic-acceptance-")
	if err != nil {
		return nil, err
	}
	kubeconfig := os.Getenv("HAPTIC_ACCEPTANCE_KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(artifacts, "kubeconfig")
	}
	owned, err := kindutil.NewCluster(runner, &kindutil.ClusterOptions{Name: ClusterName, Kubeconfig: kubeconfig, Artifacts: artifacts, Environment: environment, ReadyTimeout: 5 * time.Minute})
	if err != nil {
		return nil, err
	}
	if err := owned.Create(ctx, getKindNodeImage()); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		return nil, errors.Join(err, owned.Close(cleanup))
	}
	return owned, nil
}
