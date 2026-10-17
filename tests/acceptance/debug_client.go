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

//go:build acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/haproxy-haptic/HAPTIC/pkg/compression"
	hapticclient "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/podclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

// DebugClient reaches the controller's loopback-only debug endpoints.
type DebugClient struct {
	loopback  *podclient.Client
	haptic    hapticclient.Interface
	namespace string
}

// NewDebugClient creates a client that port-forwards to ready controller pods.
func NewDebugClient(config *rest.Config, clientset kubernetes.Interface, namespace string, port int32) (*DebugClient, error) {
	configCopy := rest.CopyConfig(config)
	configCopy.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()
	haptic, err := hapticclient.NewForConfig(configCopy)
	if err != nil {
		return nil, fmt.Errorf("create HAPTIC client: %w", err)
	}
	return &DebugClient{
		loopback: podclient.New(
			config,
			clientset,
			namespace,
			"app="+ControllerDeploymentName,
			int(port),
		),
		haptic:    haptic,
		namespace: namespace,
	}, nil
}

// proxyGet retries idempotent requests when a pod restarts or a tunnel drops.
func (dc *DebugClient) proxyGet(ctx context.Context, path string) ([]byte, error) {
	const (
		maxRetries        = 4
		initialBackoff    = 200 * time.Millisecond
		maxBackoff        = 2 * time.Second
		minTimeForRetries = 5 * time.Second
	)

	if deadline, ok := ctx.Deadline(); ok {
		if time.Until(deadline) < minTimeForRetries {
			return dc.loopback.Get(ctx, path)
		}
	}

	var lastErr error
	backoff := initialBackoff

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Wait before retry with exponential backoff
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}

		body, err := dc.loopback.Get(ctx, path)

		if err == nil {
			return body, nil
		}

		lastErr = err

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	return nil, fmt.Errorf("debug GET failed after %d attempts: %w", maxRetries, lastErr)
}

// GetConfig retrieves the current controller configuration from the debug server.
// GetHeapDump fetches a runtime/debug heap dump. It is heap-sized, so callers
// should not hold several at once.
func (dc *DebugClient) GetHeapDump(ctx context.Context) ([]byte, error) {
	return dc.proxyGet(ctx, "/debug/heapdump")
}

func (dc *DebugClient) GetConfig(ctx context.Context) (map[string]any, error) {
	return dc.getJSON(ctx, DebugPathConfig)
}

// GetRenderedConfig retrieves the rendered HAProxy configuration.
func (dc *DebugClient) GetRenderedConfig(ctx context.Context) (string, error) {
	data, err := dc.getJSON(ctx, DebugPathRendered)
	if err != nil {
		return "", err
	}

	// Extract config string from response
	if config, ok := data["config"].(string); ok {
		return config, nil
	}

	return "", fmt.Errorf("rendered config not found in response")
}

// GetRenderedConfigWithRetry retrieves the rendered HAProxy configuration with retry logic.
// This is useful when the controller may be temporarily unavailable during startup.
func (dc *DebugClient) GetRenderedConfigWithRetry(ctx context.Context, timeout time.Duration) (string, error) {
	var result string

	cfg := testutil.DefaultWaitConfig()
	cfg.Timeout = timeout

	err := testutil.WaitForConditionWithDescription(ctx, cfg, "rendered config",
		func(ctx context.Context) (bool, error) {
			config, err := dc.GetRenderedConfig(ctx)
			if err != nil {
				return false, err
			}
			result = config
			return true, nil
		})

	if err != nil {
		return "", err
	}
	return result, nil
}

// waitFor is a helper method that wraps testutil.WaitForConditionWithDescription
// with a standard timeout configuration. This reduces boilerplate in wait functions.
func (dc *DebugClient) waitFor(ctx context.Context, timeout time.Duration, description string, check func(context.Context) (bool, error)) error {
	cfg := testutil.DefaultWaitConfig()
	cfg.Timeout = timeout
	return testutil.WaitForConditionWithDescription(ctx, cfg, description, check)
}

// WaitForConfig waits for the controller configuration to become available.
//
// This is useful during controller startup when the debug endpoint is running
// but configuration hasn't been loaded yet. The method polls the debug endpoint
// until configuration is available or the timeout expires.
func (dc *DebugClient) WaitForConfig(ctx context.Context, timeout time.Duration) (map[string]any, error) {
	var result map[string]any

	cfg := testutil.DefaultWaitConfig()
	cfg.Timeout = timeout

	err := testutil.WaitForConditionWithDescription(ctx, cfg, "config available",
		func(ctx context.Context) (bool, error) {
			config, err := dc.GetConfig(ctx)
			if err != nil {
				return false, err
			}
			result = config
			return true, nil
		})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// WaitForConfigVersion waits for the controller to load a specific config version.
func (dc *DebugClient) WaitForConfigVersion(ctx context.Context, expectedVersion string, timeout time.Duration) error {
	return dc.waitFor(ctx, timeout, fmt.Sprintf("config version %q", expectedVersion),
		func(ctx context.Context) (bool, error) {
			config, err := dc.GetConfig(ctx)
			if err != nil {
				return false, err
			}
			if version, ok := config["version"].(string); ok && version == expectedVersion {
				return true, nil
			}
			return false, nil
		})
}

// WaitForRenderedConfigContains waits for the rendered config to contain a specific string.
func (dc *DebugClient) WaitForRenderedConfigContains(ctx context.Context, expectedSubstring string, timeout time.Duration) error {
	return dc.waitFor(ctx, timeout, fmt.Sprintf("rendered config contains %q", expectedSubstring),
		func(ctx context.Context) (bool, error) {
			rendered, err := dc.GetRenderedConfig(ctx)
			if err != nil {
				return false, err
			}
			return strings.Contains(rendered, expectedSubstring), nil
		})
}

// WaitForRenderedConfigContainsAny waits for the rendered config to contain any of the expected strings.
// On timeout, returns an error with the last seen config for debugging.
func (dc *DebugClient) WaitForRenderedConfigContainsAny(ctx context.Context, expectedSubstrings []string, timeout time.Duration) (string, error) {
	var matched string
	var lastConfig string

	cfg := testutil.DefaultWaitConfig()
	cfg.Timeout = timeout

	err := testutil.WaitForCondition(ctx, cfg,
		func(ctx context.Context) (bool, error) {
			rendered, err := dc.GetRenderedConfig(ctx)
			if err != nil {
				return false, err
			}

			lastConfig = rendered
			for _, expected := range expectedSubstrings {
				if strings.Contains(rendered, expected) {
					matched = expected
					return true, nil
				}
			}
			return false, nil
		})

	if err != nil {
		errMsg := fmt.Sprintf("timeout waiting for rendered config to contain any of %v", expectedSubstrings)
		if lastConfig != "" {
			errMsg += fmt.Sprintf("\nlast seen config:\n%s", lastConfig)
		}
		return "", fmt.Errorf("%s: %w", errMsg, err)
	}
	return matched, nil
}

// PipelineStatus represents the reconciliation pipeline status.
type PipelineStatus struct {
	LastTrigger *TriggerStatus    `json:"last_trigger"`
	Rendering   *RenderingStatus  `json:"rendering"`
	Validation  *ValidationStatus `json:"validation"`
	Deployment  *DeploymentStatus `json:"deployment"`
}

// TriggerStatus represents what triggered the last reconciliation.
type TriggerStatus struct {
	Timestamp string `json:"timestamp"`
	Reason    string `json:"reason"`
}

// RenderingStatus represents the template rendering phase status.
type RenderingStatus struct {
	Status      string `json:"status"`
	Timestamp   string `json:"timestamp"`
	DurationMs  int64  `json:"duration_ms"`
	ConfigBytes int    `json:"config_bytes"`
	Error       string `json:"error,omitempty"`
}

// ValidationStatus represents the HAProxy validation phase status.
type ValidationStatus struct {
	Status     string   `json:"status"`
	Timestamp  string   `json:"timestamp"`
	DurationMs int64    `json:"duration_ms"`
	Errors     []string `json:"errors,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// DeploymentStatus represents the deployment phase status.
type DeploymentStatus struct {
	Status             string           `json:"status"`
	Reason             string           `json:"reason,omitempty"`
	Timestamp          string           `json:"timestamp"`
	DurationMs         int64            `json:"duration_ms,omitempty"`
	EndpointsTotal     int              `json:"endpoints_total"`
	EndpointsSucceeded int              `json:"endpoints_succeeded"`
	EndpointsFailed    int              `json:"endpoints_failed"`
	FailedEndpoints    []FailedEndpoint `json:"failed_endpoints,omitempty"`
}

// FailedEndpoint contains details about a failed deployment endpoint.
type FailedEndpoint struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// ErrorSummary provides an aggregated view of recent errors.
type ErrorSummary struct {
	ConfigParseError       *ErrorInfo  `json:"config_parse_error,omitempty"`
	TemplateRenderError    *ErrorInfo  `json:"template_render_error,omitempty"`
	HAProxyValidationError *ErrorInfo  `json:"haproxy_validation_error,omitempty"`
	DeploymentErrors       []ErrorInfo `json:"deployment_errors,omitempty"`
	LastErrorTimestamp     string      `json:"last_error_timestamp,omitempty"`
}

// ErrorInfo contains details about a specific error.
type ErrorInfo struct {
	Timestamp string   `json:"timestamp"`
	Errors    []string `json:"errors"`
}

// GetPipelineStatus retrieves the reconciliation pipeline status.
func (dc *DebugClient) GetPipelineStatus(ctx context.Context) (*PipelineStatus, error) {
	data, err := dc.getJSON(ctx, DebugPathPipeline)
	if err != nil {
		return nil, err
	}

	// Re-marshal and unmarshal to convert map to struct
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal pipeline status: %w", err)
	}

	var status PipelineStatus
	if err := json.Unmarshal(jsonBytes, &status); err != nil {
		return nil, fmt.Errorf("failed to unmarshal pipeline status: %w", err)
	}

	return &status, nil
}

// GetPipelineStatusWithRetry retrieves the pipeline status with retry logic.
// This is useful when the controller may be temporarily unavailable during startup.
func (dc *DebugClient) GetPipelineStatusWithRetry(ctx context.Context, timeout time.Duration) (*PipelineStatus, error) {
	var result *PipelineStatus

	cfg := testutil.DefaultWaitConfig()
	cfg.Timeout = timeout

	err := testutil.WaitForConditionWithDescription(ctx, cfg, "pipeline status",
		func(ctx context.Context) (bool, error) {
			status, err := dc.GetPipelineStatus(ctx)
			if err != nil {
				return false, err
			}
			result = status
			return true, nil
		})

	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetErrors retrieves the error summary.
func (dc *DebugClient) GetErrors(ctx context.Context) (*ErrorSummary, error) {
	data, err := dc.getJSON(ctx, DebugPathErrors)
	if err != nil {
		return nil, err
	}

	// Re-marshal and unmarshal to convert map to struct
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal error summary: %w", err)
	}

	var summary ErrorSummary
	if err := json.Unmarshal(jsonBytes, &summary); err != nil {
		return nil, fmt.Errorf("failed to unmarshal error summary: %w", err)
	}

	return &summary, nil
}

// WaitForValidationStatus waits for the validation status to match the expected
// value and returns the pipeline status that matched. Assert on that snapshot: a
// later fetch can observe the next pipeline run, whose validation hasn't run yet.
func (dc *DebugClient) WaitForValidationStatus(ctx context.Context, expected string, timeout time.Duration) (*PipelineStatus, error) {
	var matched *PipelineStatus
	err := dc.waitFor(ctx, timeout, fmt.Sprintf("validation status %q", expected),
		func(ctx context.Context) (bool, error) {
			status, err := dc.GetPipelineStatus(ctx)
			if err != nil {
				return false, err
			}
			if status.Validation != nil && status.Validation.Status == expected {
				matched = status
				return true, nil
			}
			return false, nil
		})
	return matched, err
}

// getJSON fetches JSON from the loopback-only debug server.
func (dc *DebugClient) getJSON(ctx context.Context, path string) (map[string]any, error) {
	body, err := dc.proxyGet(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch from debug server: %w", err)
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("failed to decode JSON response: %w", err)
	}

	return data, nil
}

// GetAuxiliaryFiles retrieves the auxiliary files from the debug endpoint.
func (dc *DebugClient) GetAuxiliaryFiles(ctx context.Context) (map[string]any, error) {
	return dc.getJSON(ctx, DebugPathAuxFiles)
}

// GetGeneralFileContent reads a rendered general file from its output CRD.
func (dc *DebugClient) GetGeneralFileContent(ctx context.Context, fileName string) (string, error) {
	files, err := dc.haptic.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles(dc.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list HAProxyGeneralFiles: %w", err)
	}
	for i := range files.Items {
		file := &files.Items[i]
		if file.Spec.FileName != fileName {
			continue
		}
		if !file.Spec.Compressed {
			return file.Spec.Content, nil
		}
		content, err := compression.Decompress(file.Spec.Content)
		if err != nil {
			return "", fmt.Errorf("decompress HAProxyGeneralFile %s: %w", file.Name, err)
		}
		return content, nil
	}
	return "", fmt.Errorf("HAProxyGeneralFile for %q not found", fileName)
}

// WaitForAuxFileContains waits until a specific auxiliary file contains the expected content.
func (dc *DebugClient) WaitForAuxFileContains(ctx context.Context, fileName, expectedContent string, timeout time.Duration) error {
	var lastContent string

	cfg := testutil.DefaultWaitConfig()
	cfg.Timeout = timeout

	err := testutil.WaitForCondition(ctx, cfg,
		func(ctx context.Context) (bool, error) {
			content, err := dc.GetGeneralFileContent(ctx, fileName)
			if err != nil {
				return false, err
			}
			lastContent = content

			if strings.Contains(content, expectedContent) {
				return true, nil
			}
			return false, nil
		})

	if err != nil {
		return fmt.Errorf("waiting for file %s to contain %q (last content: %q): %w", fileName, expectedContent, lastContent, err)
	}
	return nil
}

// WaitForAuxFileContentStable waits and verifies that the file content stays unchanged for the duration.
// This is useful for verifying that invalid updates are rejected and old content is preserved.
//
// Note: This function uses a fixed 500ms polling interval to ensure frequent stability checks.
// Exponential backoff would be counter-productive here as we need to verify content hasn't changed.
func (dc *DebugClient) WaitForAuxFileContentStable(ctx context.Context, fileName, expectedContent string, stableDuration time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, stableDuration+5*time.Second)
	defer cancel()

	// Use a fast fixed interval for stability checking - we need frequent checks
	// to verify content hasn't changed, not exponential backoff
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	stableStart := time.Now()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout while verifying content stability for file %s", fileName)

		case <-ticker.C:
			content, err := dc.GetGeneralFileContent(ctx, fileName)
			if err != nil {
				return fmt.Errorf("error getting file content: %w", err)
			}

			if !strings.Contains(content, expectedContent) {
				return fmt.Errorf("file content changed unexpectedly, expected to contain %q but got %q", expectedContent, content)
			}

			if time.Since(stableStart) >= stableDuration {
				// Content has been stable for the required duration
				return nil
			}
		}
	}
}
