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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
)

const (
	// crdFieldManager is the server-side-apply field manager for CRDs applied by
	// this command, so ownership is attributable and repeated applies are
	// conflict-free (with Force).
	crdFieldManager = "haptic-crd-installer"
	crdKind         = "CustomResourceDefinition"

	// crdApplyTimeout bounds each CRD Patch at the Go level, so a hung API
	// server request fails with a clear error instead of relying on the Job's
	// activeDeadlineSeconds hard pod kill.
	crdApplyTimeout = 30 * time.Second
)

// crdGVR is the (fixed) GroupVersionResource for CustomResourceDefinitions —
// the controller's own operational identity, not a user-watched resource, so a
// hardcoded GVR here is intentional (see RULE #1's operational-identity exception).
var crdGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

var (
	applyCRDsChartDir   string
	applyCRDsKubeconfig string
)

var applyCRDsCmd = &cobra.Command{
	Use:   "apply-crds",
	Short: "Server-side apply the bundled CustomResourceDefinitions",
	Long: `Server-side applies the CustomResourceDefinitions bundled in the chart.

Helm never upgrades CRDs it installed from a chart's crds/ directory, so a chart
upgrade leaves the CRD schema stale — new fields get pruned by the API server,
new printer columns and validation don't appear. This command closes that gap:
it reads the CRDs from the image-embedded chart (or --chart / $HAPTIC_CHART_DIR)
and applies them with server-side apply, which is idempotent, never deletes, and
avoids the last-applied-configuration size limit that client-side apply hits on
large CRDs.

Run it as a Helm pre-upgrade hook (the chart wires this up when
crds.upgradeJob.enabled is true) or standalone in a GitOps PreSync step. It never
touches CRD .status, so it cannot disturb the API server's stored-version
bookkeeping.`,
	RunE: runApplyCRDs,
}

func init() {
	applyCRDsCmd.Flags().StringVar(&applyCRDsChartDir, "chart", "",
		"Chart directory containing crds/ (default: $HAPTIC_CHART_DIR, then the image-embedded chart)")
	applyCRDsCmd.Flags().StringVar(&applyCRDsKubeconfig, "kubeconfig", "",
		"Path to kubeconfig (default: in-cluster config)")
}

func runApplyCRDs(cmd *cobra.Command, _ []string) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	chartDir, err := resolveChartDir(applyCRDsChartDir)
	if err != nil {
		return err
	}
	crdDir := filepath.Join(chartDir, "charts", "haptic-crds", "crds")

	crds, err := loadCRDs(crdDir)
	if err != nil {
		return err
	}
	if len(crds) == 0 {
		return fmt.Errorf("no CustomResourceDefinitions found in %s", crdDir)
	}

	k8sClient, err := client.New(client.Config{Kubeconfig: applyCRDsKubeconfig})
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	ctx := cmd.Context()
	removeConfigWebhook(ctx, k8sClient.DynamicClient(), logger)
	for _, crd := range crds {
		if err := applyCRD(ctx, k8sClient.DynamicClient(), crd); err != nil {
			return fmt.Errorf("applying CRD %q: %w", crd.GetName(), err)
		}
		logger.Info("Server-side applied CRD", "name", crd.GetName())
	}
	logger.Info("Applied all bundled CRDs", "count", len(crds), "field_manager", crdFieldManager)
	return nil
}

// loadCRDs reads every YAML document under dir, returning those whose kind is
// CustomResourceDefinition sorted by name for deterministic apply order. A
// non-CRD document in the CRD directory is an error (the crds/ dir must hold
// only CRDs).
func loadCRDs(dir string) ([]*unstructured.Unstructured, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading CRD directory %s: %w", dir, err)
	}
	var crds []*unstructured.Unstructured
	for _, e := range entries {
		if e.IsDir() || !isYAMLFile(e.Name()) {
			continue
		}
		docs, err := decodeCRDFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		crds = append(crds, docs...)
	}
	sort.Slice(crds, func(i, j int) bool { return crds[i].GetName() < crds[j].GetName() })
	return crds, nil
}

// decodeCRDFile decodes every YAML document in a file into unstructured CRDs.
func decodeCRDFile(path string) ([]*unstructured.Unstructured, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var out []*unstructured.Unstructured
	dec := k8syaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decoding %s: %w", path, err)
		}
		if len(u.Object) == 0 {
			continue // empty document (e.g. a trailing "---")
		}
		if u.GetKind() != crdKind {
			return nil, fmt.Errorf("%s contains a non-CRD document (kind %q); the crds/ directory must hold only CustomResourceDefinitions", path, u.GetKind())
		}
		out = append(out, u)
	}
	return out, nil
}

// applyCRD server-side applies one CRD. It strips .status and the
// controller-gen `metadata.creationTimestamp: null` first: the API server owns
// CRD status (acceptedNames, conditions, and crucially storedVersions), so
// applying an empty status would clobber the stored-version bookkeeping and can
// break future version migrations.
func applyCRD(ctx context.Context, dyn dynamic.Interface, crd *unstructured.Unstructured) error {
	unstructured.RemoveNestedField(crd.Object, "status")
	unstructured.RemoveNestedField(crd.Object, "metadata", "creationTimestamp")

	payload, err := json.Marshal(crd.Object)
	if err != nil {
		return fmt.Errorf("marshalling CRD: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, crdApplyTimeout)
	defer cancel()
	_, err = dyn.Resource(crdGVR).Patch(
		ctx,
		crd.GetName(),
		types.ApplyPatchType,
		payload,
		metav1.PatchOptions{FieldManager: crdFieldManager, Force: new(true)},
	)
	return err
}

func isYAMLFile(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// vwcGVR is HAPTIC's own webhook configuration — the operational-identity
// exception, like crdGVR above.
var vwcGVR = schema.GroupVersionResource{
	Group:    "admissionregistration.k8s.io",
	Version:  "v1",
	Resource: "validatingwebhookconfigurations",
}

// removeConfigWebhook deletes any live HAProxyTemplateConfig webhook entry
// before the release's own objects are applied.
//
// The controller no longer serves config admission at all (ADR-0016): a
// per-object webhook cannot judge a multi-object change, so the config gate
// moved to the pre-upgrade preflight hook and the fail-closed load gate.
// During the upgrade that removes it, ordering is the hazard — Helm does not
// guarantee the updated ValidatingWebhookConfiguration is applied before the
// HAProxyTemplateConfigs it governs (measured: the config went first), so the
// RUNNING old webhook would judge each new per-library shard standalone and
// deny it as incomplete. Removing the entry here, in a hook that always
// precedes manifests, closes that window on Helm, Flux, and Argo alike.
//
// Idempotent, and silent when there is nothing to remove: a fresh install has
// no webhook configuration yet.
//
// Best-effort on purpose, so no failure here returns an error. Every way this
// can fail degrades to the behaviour that existed before it — the old webhook
// keeps fronting the config, and the upgrade's shard objects may be denied
// until a retry — whereas aborting would turn a webhook we merely could not
// reach into an upgrade the operator cannot run at all. That matters most in
// the case this exists to serve: recovering a fleet that is already broken.
func removeConfigWebhook(ctx context.Context, dyn dynamic.Interface, logger *slog.Logger) {
	list, err := dyn.Resource(vwcGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		logger.Info("Cannot list ValidatingWebhookConfigurations; leaving the config webhook where it is", "error", err)
		return
	}

	for i := range list.Items {
		item := list.Items[i]
		changed, err := removeConfigHooksIn(&item)
		if err != nil {
			logger.Info("Cannot read webhooks; leaving this configuration alone",
				"webhookConfiguration", item.GetName(), "error", err)
			continue
		}
		if !changed {
			continue
		}
		if _, err := dyn.Resource(vwcGVR).Update(ctx, &item, metav1.UpdateOptions{}); err != nil {
			logger.Info("Cannot remove config webhook; leaving it in place",
				"webhookConfiguration", item.GetName(), "error", err)
			continue
		}
		logger.Info("Removed legacy HAProxyTemplateConfig webhook entry",
			"webhookConfiguration", item.GetName())
	}
}

// removeConfigHooksIn drops every HAProxyTemplateConfig webhook entry from one
// configuration, reporting whether anything changed. The entry is removed, not
// the configuration: the same object carries the watched-resource webhook,
// which must keep working.
func removeConfigHooksIn(item *unstructured.Unstructured) (bool, error) {
	hooks, found, _ := unstructured.NestedSlice(item.Object, "webhooks")
	if !found {
		return false, nil
	}

	kept := make([]any, 0, len(hooks))
	for j := range hooks {
		hook, ok := hooks[j].(map[string]any)
		if ok {
			name, _, _ := unstructured.NestedString(hook, "name")
			if strings.HasPrefix(name, "haproxytemplateconfig.") {
				continue
			}
		}
		kept = append(kept, hooks[j])
	}
	if len(kept) == len(hooks) {
		return false, nil
	}
	if err := unstructured.SetNestedSlice(item.Object, kept, "webhooks"); err != nil {
		return false, fmt.Errorf("updating %s: %w", item.GetName(), err)
	}
	return true, nil
}
