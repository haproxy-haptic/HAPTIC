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

package main

import (
	"cmp"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller"
	controllerwebhook "github.com/haproxy-haptic/HAPTIC/pkg/controller/webhook"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/logging"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/transportsecurity"
)

var (
	runCRDName                         string
	runSecretName                      string
	runWebhookCertDir                  string
	runWebhookResourceAdmissionTimeout time.Duration
	runKubeconfig                      string
	runDebugPort                       int
	runKubeClientQPS                   float32
	runKubeClientBurst                 int
	runAgentTLSDirectory               string
	runAgentTLSServerName              string
)

// runCmd represents the run command (controller main loop).
var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the HAProxy Template Ingress Controller",
	Long: `Run the HAProxy Template Ingress Controller.

The controller watches a HAProxyTemplateConfig CRD and Kubernetes resources,
renders HAProxy configurations from templates, and synchronizes them to HAProxy
pods via the HAPTIC agent.

Configuration is loaded from:
1. Command-line flags (highest priority)
2. Environment variables
3. Default values (lowest priority)

Example usage:
  # Run with default configuration
  haptic run

  # Run with custom CRD name
  haptic run --crd-name my-haproxy-config

  # Run with kubeconfig (out-of-cluster development)
  haptic run --kubeconfig ~/.kube/config

  # Enable debug server
  haptic run --debug-port 6060`,
	RunE: runController,
}

func init() {
	runCmd.Flags().StringVar(&runAgentTLSDirectory, "agent-tls-dir", "",
		"Directory containing the controller TLS identity and agent CA (env: AGENT_TLS_DIR)")
	runCmd.Flags().StringVar(&runAgentTLSServerName, "agent-tls-server-name", "",
		"Required agent certificate DNS SAN (env: AGENT_TLS_SERVER_NAME)")
	runCmd.Flags().StringVar(&runCRDName, "crd-name", "",
		"Name of the HAProxyTemplateConfig holding controller configuration (env: CRD_NAME). Template library "+
			"content is pulled in through its spec.libraryRefs.")
	runCmd.Flags().StringVar(&runSecretName, "secret-name", "",
		"Name of the Secret containing the HAPTIC agent credentials (env: SECRET_NAME)")
	runCmd.Flags().StringVar(&runWebhookCertDir, "webhook-cert-dir", "",
		"Directory holding the webhook TLS cert (tls.crt/tls.key); read per-handshake so a rotated cert is served without restart. Empty disables the webhook (env: WEBHOOK_CERT_DIR)")
	runCmd.Flags().DurationVar(&runWebhookResourceAdmissionTimeout, "webhook-resource-admission-timeout", 0,
		"Controller-side deadline for watched-resource admission; must be shorter than the ValidatingWebhookConfiguration timeout (env: WEBHOOK_RESOURCE_ADMISSION_TIMEOUT, default: 9s)")
	runCmd.Flags().StringVar(&runKubeconfig, "kubeconfig", "",
		"Path to kubeconfig file (for out-of-cluster development)")
	runCmd.Flags().IntVar(&runDebugPort, "debug-port", 0,
		"Port for debug HTTP server (0 to disable, env: DEBUG_PORT)")
	runCmd.Flags().Float32Var(&runKubeClientQPS, "kube-client-qps", -1,
		"Client-side apiserver QPS. <=0 disables client-side throttling and relies on apiserver Priority & Fairness (env: KUBE_CLIENT_QPS)")
	runCmd.Flags().IntVar(&runKubeClientBurst, "kube-client-burst", 0,
		"Client-side apiserver burst; used only when --kube-client-qps > 0, 0 means 2*qps (env: KUBE_CLIENT_BURST)")
}

// resolveFloat32Option applies flag > env > flag-default. The flag's zero value
// is a meaningful setting, so an unchanged flag (flagChanged=false) falls through
// to the env var.
func resolveFloat32Option(flagChanged bool, flagValue float32, envName string) (float32, error) {
	if flagChanged {
		return flagValue, nil
	}
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return flagValue, nil
	}
	v, err := strconv.ParseFloat(raw, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing %s=%q as a number: %w", envName, raw, err)
	}
	return float32(v), nil
}

// resolveIntOption applies flag > env > flag-default.
func resolveIntOption(flagChanged bool, flagValue int, envName string) (int, error) {
	if flagChanged {
		return flagValue, nil
	}
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return flagValue, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parsing %s=%q as an integer: %w", envName, raw, err)
	}
	return v, nil
}

// resolveConfigName applies the flag > env > default priority.
func resolveConfigName(fromFlag string) string {
	if name := strings.TrimSpace(fromFlag); name != "" {
		return name
	}
	if name := strings.TrimSpace(os.Getenv("CRD_NAME")); name != "" {
		return name
	}
	return defaultCRDName
}

func runController(cmd *cobra.Command, _ []string) error {
	ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Configuration priority: CLI flags > Environment variables > Defaults

	runCRDName = resolveConfigName(runCRDName)

	if runSecretName == "" {
		runSecretName = os.Getenv("SECRET_NAME")
	}
	if runSecretName == "" {
		runSecretName = defaultSecretName
	}

	// Webhook certificate directory (optional - empty means webhooks disabled)
	if runWebhookCertDir == "" {
		runWebhookCertDir = os.Getenv("WEBHOOK_CERT_DIR")
	}
	var err error
	runWebhookResourceAdmissionTimeout, err = resolveDurationOption(
		runWebhookResourceAdmissionTimeout,
		"WEBHOOK_RESOURCE_ADMISSION_TIMEOUT",
		controllerwebhook.DefaultResourceAdmissionTimeout,
	)
	if err != nil {
		return err
	}

	if runDebugPort == 0 {
		if envDebugPort := os.Getenv("DEBUG_PORT"); envDebugPort != "" {
			if port, err := strconv.Atoi(envDebugPort); err == nil {
				runDebugPort = port
			}
		}
	}
	if runDebugPort == 0 {
		runDebugPort = defaultDebugPort
	}

	// Set up structured logging with dynamic level support
	// LOG_LEVEL env var: TRACE, DEBUG, INFO (default), WARN, ERROR (case-insensitive)
	// The level can be overridden at runtime via ConfigMap's logging.level field
	logLevelEnv := os.Getenv("LOG_LEVEL")
	logger := logging.NewDynamicLogger(logLevelEnv)
	slog.SetDefault(logger)

	// Route client-go's klog output (leader election, informers) through slog
	// so it shares the same logfmt format and dynamic level as everything else.
	klog.SetSlogLogger(logging.WithCancellationLogging(logger, ctx))

	configureMemoryLimit(logger)
	mutexProfileFraction, blockProfileRate, err := configureContentionProfiling()
	if err != nil {
		return err
	}

	// Log detected resource limits for observability.
	// GOGC: report the env override if set, otherwise "default" (Go's Green Tea
	// GC reduces per-cycle overhead, making manual GOGC tuning unnecessary;
	// automemlimit's GOMEMLIMIT provides the OOM safety net).
	gomaxprocs := runtime.GOMAXPROCS(0)
	gomemlimit := "unlimited"
	if limit := debug.SetMemoryLimit(-1); limit != math.MaxInt64 {
		gomemlimit = fmt.Sprintf("%d bytes (%.2f MiB)", limit, float64(limit)/(1024*1024))
	}

	logger.Info("HAProxy Template Ingress Controller starting",
		"version", version,
		"source_hash", sourceHash,
		"crd_name", runCRDName,
		"secret", runSecretName,
		"webhook_cert_dir", runWebhookCertDir,
		"webhook_resource_admission_timeout", runWebhookResourceAdmissionTimeout,
		"debug_port", runDebugPort,
		"log_level", logging.GetLevel(),
		"gomaxprocs", gomaxprocs,
		"gomemlimit", gomemlimit,
		"gogc", cmp.Or(os.Getenv("GOGC"), "default"),
		"mutex_profile_fraction", mutexProfileFraction,
		"block_profile_rate", blockProfileRate)

	// Configure build info for metrics.
	// HAPROXY_MINOR is set by the haproxytech/haproxy-debian base image and contains
	// the full semver of the bundled HAProxy (e.g. "3.2.11").
	controller.SetBuildInfo(version, os.Getenv("HAPROXY_MINOR"))
	controller.SetBuildSourceHash(sourceHash)
	directory := cmp.Or(runAgentTLSDirectory, os.Getenv("AGENT_TLS_DIR"))
	peerName := cmp.Or(runAgentTLSServerName, os.Getenv("AGENT_TLS_SERVER_NAME"))
	var agentTLS *transportsecurity.Source
	if directory != "" || peerName != "" {
		var err error
		agentTLS, err = transportsecurity.NewSource(directory, peerName)
		if err != nil {
			return err
		}
	}

	k8sClient, err := controllerKubernetesClient(cmd)
	if err != nil {
		return fmt.Errorf("creating Kubernetes client: %w", err)
	}

	logger.Info("Kubernetes client created successfully",
		"namespace", k8sClient.Namespace(),
		"in_cluster", runKubeconfig == "")

	if err := controller.Run(
		ctx,
		k8sClient,
		runCRDName,
		runSecretName,
		runWebhookCertDir,
		controller.WebhookAdmissionTimeouts{
			Resource: runWebhookResourceAdmissionTimeout,
		},
		runDebugPort,
		agentTLS,
	); err != nil && ctx.Err() == nil {
		return fmt.Errorf("controller failed: %w", err)
	}

	logger.Info("Controller shutdown complete")
	return nil
}

func resolveDurationOption(flagValue time.Duration, envName string, defaultValue time.Duration) (time.Duration, error) {
	if flagValue != 0 {
		if flagValue < 0 {
			return 0, fmt.Errorf("admission timeout %s must be positive", envName)
		}
		if flagValue > controllerwebhook.MaximumAdmissionTimeout {
			return 0, fmt.Errorf("admission timeout %s must not exceed %s", envName, controllerwebhook.MaximumAdmissionTimeout)
		}
		return flagValue, nil
	}

	raw := os.Getenv(envName)
	if raw == "" {
		return defaultValue, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parsing %s=%q as a duration: %w", envName, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive", envName)
	}
	if d > controllerwebhook.MaximumAdmissionTimeout {
		return 0, fmt.Errorf("%s must not exceed %s", envName, controllerwebhook.MaximumAdmissionTimeout)
	}
	return d, nil
}

func controllerKubernetesClient(cmd *cobra.Command) (*client.Client, error) {
	kubeClientQPS, err := resolveFloat32Option(cmd.Flags().Changed("kube-client-qps"), runKubeClientQPS, "KUBE_CLIENT_QPS")
	if err != nil {
		return nil, err
	}
	kubeClientBurst, err := resolveIntOption(cmd.Flags().Changed("kube-client-burst"), runKubeClientBurst, "KUBE_CLIENT_BURST")
	if err != nil {
		return nil, err
	}
	return client.New(client.Config{
		Kubeconfig: runKubeconfig,
		QPS:        kubeClientQPS,
		Burst:      kubeClientBurst,
	})
}
