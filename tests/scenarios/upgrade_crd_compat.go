// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/semver"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

const hapticAPIGroup = "haproxy-haptic.org"

// requireStorableUnderBaselineCRDs dry-runs the target release's HAPTIC custom
// resources against the newest release's CRDs, as Argo CD's server-side diff
// does before its PreSync hook upgrades them (#556). Older baselines are out of
// scope: skipping releases under a server-side diff needs the CRDs applied first.
func (u *upgrade) requireStorableUnderBaselineCRDs(ctx context.Context) error {
	s := u.session
	newest, err := u.newestRelease(ctx)
	if err != nil {
		return err
	}
	if u.baseline != newest {
		s.Infof("Upgrade %s: CRD fit is checked from the newest release, %s", u.baseline, newest)
		return nil
	}
	if err := u.applyTargetCustomResources(ctx, "--dry-run=server", "--field-manager=baseline-crd-check"); err != nil {
		return fmt.Errorf("the target chart's custom resources do not fit the %s CRDs, so a server-side diff fails before any hook runs: %w", u.baseline, err)
	}
	return nil
}

// applyTargetCustomResources server-side applies the target release's HAPTIC
// custom resources of every kind the cluster serves, as Argo CD's sync does.
func (u *upgrade) applyTargetCustomResources(ctx context.Context, flags ...string) error {
	s := u.session
	rendered, err := s.Helm(ctx, append([]string{"template"}, u.targetArgs(u.chart)[1:]...)...)
	if err != nil {
		return err
	}
	served, err := s.Kube(ctx, nil, "api-resources", "--api-group="+hapticAPIGroup, "--no-headers")
	if err != nil {
		return err
	}
	list, err := servedCustomResources([]byte(rendered.Stdout), servedKinds(served.Stdout))
	if err != nil {
		return err
	}
	args := append([]string{"apply", "--server-side", "--force-conflicts"}, flags...)
	_, err = s.Kube(ctx, bytes.NewReader(list), append(args, "-f", "-")...)
	return err
}

func (u *upgrade) newestRelease(ctx context.Context) (string, error) {
	version, err := os.ReadFile(filepath.Join(u.session.Root, "VERSION"))
	if err != nil {
		return "", err
	}
	baselines, err := DiscoverUpgradeBaselines(ctx, http.DefaultClient, strings.TrimSpace(string(version)))
	if err != nil {
		return "", err
	}
	return newestStable(baselines)
}

func newestStable(baselines []string) (string, error) {
	newest := ""
	for _, baseline := range baselines {
		if stableVersion.MatchString(baseline) && (newest == "" || semver.Compare("v"+baseline, "v"+newest) > 0) {
			newest = baseline
		}
	}
	if newest == "" {
		return "", errors.New("no stable upgrade baseline")
	}
	return newest, nil
}

// servedKinds reads the KIND column of `kubectl api-resources --no-headers`.
func servedKinds(table string) map[string]bool {
	kinds := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(table), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			kinds[fields[len(fields)-1]] = true
		}
	}
	return kinds
}

// servedCustomResources returns the HAPTIC objects of a served kind as a v1 List.
// A kind the baseline lacks is left out: its CRD, not a field, is what is missing.
func servedCustomResources(manifest []byte, kinds map[string]bool) ([]byte, error) {
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	items := []any{}
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if strings.HasPrefix(object.GetAPIVersion(), hapticAPIGroup+"/") && kinds[object.GetKind()] {
			items = append(items, object.Object)
		}
	}
	if len(items) == 0 {
		return nil, errors.New("the target chart renders no HAPTIC custom resource of a kind the baseline serves")
	}
	return json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
}
