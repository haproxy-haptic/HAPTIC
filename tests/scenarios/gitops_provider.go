// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/haproxy-haptic/HAPTIC/tests/fixtures"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

func (g *gitops) installManifest(ctx context.Context, name, url, namespace string, transform func([]byte) ([]byte, error)) error {
	content, err := fetchManifest(ctx, url)
	if err != nil {
		return err
	}
	if err := g.session.Save(name+"-source.json", map[string]string{"url": url, "sha256": fmt.Sprintf("%x", sha256.Sum256(content))}); err != nil {
		return err
	}
	if transform != nil {
		content, err = transform(content)
		if err != nil {
			return err
		}
	}
	args := []string{"apply", "--server-side", "-f", "-"}
	if namespace != "" {
		args = append([]string{"-n", namespace}, args...)
	}
	_, err = g.session.KubeUnscoped(ctx, bytes.NewReader(content), args...)
	return err
}

func filterDeployments(content []byte, names []string) ([]byte, error) {
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)
	var result bytes.Buffer
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if len(object.Object) == 0 || (object.GetKind() == kindDeployment && !slices.Contains(names, object.GetName())) {
			continue
		}
		content, err := json.Marshal(object.Object)
		if err != nil {
			return nil, err
		}
		result.Write(content)
		result.WriteString("\n---\n")
	}
	return result.Bytes(), nil
}

func (g *gitops) installProvider(ctx context.Context) error {
	if g.options.Provider == providerArgo {
		if err := g.installManifest(ctx, "argocd", "https://raw.githubusercontent.com/argoproj/argo-cd/"+argoVersion+"/manifests/core-install.yaml", argoNamespace, nil); err != nil {
			return err
		}
		for _, target := range []string{"deployment/argocd-repo-server", "statefulset/argocd-application-controller"} {
			if err := g.rollout(ctx, argoNamespace, target); err != nil {
				return err
			}
		}
		project := resource("argoproj.io/v1alpha1", "AppProject", argoNamespace, "haptic-test")
		project["spec"] = map[string]any{"sourceRepos": []string{gitopsRepository}, "destinations": []any{map[string]string{"namespace": g.session.Namespace, "server": "https://kubernetes.default.svc"}}, "clusterResourceWhitelist": []any{map[string]string{"group": "*", fieldKind: "*"}}}
		return g.session.Apply(ctx, project)
	}
	controllers := []string{"source-controller", "helm-controller"}
	onlyControllers := func(content []byte) ([]byte, error) { return filterDeployments(content, controllers) }
	if err := g.installManifest(ctx, "flux", "https://github.com/fluxcd/flux2/releases/download/"+fluxVersion+"/install.yaml", "", onlyControllers); err != nil {
		return err
	}
	for _, name := range controllers {
		if err := g.rollout(ctx, fluxNamespace, "deployment/"+name); err != nil {
			return err
		}
	}
	repository := resource("source.toolkit.fluxcd.io/v1", "HelmRepository", fluxNamespace, chartName)
	repository["spec"] = map[string]string{"interval": "1m", "url": gitopsRepository}
	return g.session.Apply(ctx, repository)
}

func (g *gitops) values(phase string) (map[string]any, error) {
	content, err := fixtures.ChartUpgrade("values.yaml")
	if err != nil {
		return nil, err
	}
	var values map[string]any
	if err := yaml.Unmarshal(content, &values); err != nil {
		return nil, err
	}
	values["credentials"] = map[string]any{"existingSecret": gitopsCredentialsSecret}
	controller, ok := values["controller"].(map[string]any)
	if !ok {
		return nil, errors.New("upgrade fixture has no controller values")
	}
	controller["image"] = map[string]string{"repository": chartName, "tag": g.images[phase]}
	controller["webhook"] = map[string]any{"certManager": map[string]bool{fieldEnabled: true}}
	controller["resources"] = map[string]any{"limits": map[string]int{"cpu": 4}}
	if g.options.Certificates == "external" {
		controller["webhook"] = map[string]any{"secretName": gitopsWebhookSecret, "caBundle": g.webhookCA, "certManager": map[string]bool{fieldEnabled: false}}
		certificate, ok := values["defaultSSLCertificate"].(map[string]any)
		if !ok {
			return nil, errors.New("upgrade fixture has no default certificate values")
		}
		certificate["certManager"] = map[string]bool{fieldEnabled: false}
	}
	return values, nil
}

func syncOptions() []string {
	return []string{"ServerSideApply=true", "DisableClientSideApplyMigration=true"}
}

func (g *gitops) desired(phase string) (map[string]any, error) {
	values, err := g.values(phase)
	if err != nil {
		return nil, err
	}
	if g.options.Provider == providerArgo {
		app := resource("argoproj.io/v1alpha1", "Application", argoNamespace, chartName)
		app["spec"] = map[string]any{"project": "haptic-test", "destination": map[string]string{"namespace": g.session.Namespace, "server": "https://kubernetes.default.svc"}, "source": map[string]any{"repoURL": gitopsRepository, fieldChart: chartName, "targetRevision": g.versions[phase], "helm": map[string]any{"releaseName": chartName, "valuesObject": values}}, "syncPolicy": map[string]any{"automated": map[string]bool{fieldEnabled: true, "prune": true, "selfHeal": true}, "retry": map[string]int{"limit": 1}, "syncOptions": syncOptions()}}
		return app, nil
	}
	release := resource("helm.toolkit.fluxcd.io/v2", "HelmRelease", fluxNamespace, chartName)
	release["spec"] = map[string]any{"interval": "1m", "timeout": "10m", "releaseName": chartName, "targetNamespace": g.session.Namespace, fieldChart: map[string]any{"spec": map[string]any{fieldChart: chartName, "version": g.versions[phase], "sourceRef": map[string]string{fieldKind: "HelmRepository", fieldName: chartName}}}, helmInstall: retryStrategy(), helmUpgrade: retryStrategy(), "values": values}
	return release, nil
}

func retryStrategy() map[string]any {
	return map[string]any{"strategy": map[string]string{fieldName: "RetryOnFailure", "retryInterval": "1m"}}
}

func (g *gitops) state(ctx context.Context) (unstructured.Unstructured, error) {
	namespace, kind := fluxNamespace, "helmrelease"
	if g.options.Provider == providerArgo {
		namespace, kind = argoNamespace, "application"
	}
	return readJSON[unstructured.Unstructured](ctx, g.session, "-n", namespace, "get", kind, chartName)
}

func (g *gitops) sync(ctx context.Context, phase string, repeat bool) error {
	g.session.Infof("%s: %s, repeat=%t", g.options.Provider, phase, repeat)
	object, err := g.desired(phase)
	if err != nil {
		return err
	}
	if err := g.session.Apply(ctx, object); err != nil {
		return err
	}
	state, err := g.state(ctx)
	if err != nil {
		return err
	}
	previous := nestedString(state.Object, "status", "operationState", "startedAt")
	request := strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := g.triggerSync(ctx, repeat, request); err != nil {
		return err
	}
	return poll(ctx, 10*time.Minute, 2*time.Second, phase+" GitOps result", func(ctx context.Context) (testutil.PollResult, error) {
		current, err := g.state(ctx)
		if err != nil {
			return testutil.PollFailed, err
		}
		name := phase
		if repeat {
			name += "-repeat"
		}
		if err := g.session.Save(name+"-gitops.json", current.Object); err != nil {
			return testutil.PollFailed, err
		}
		return gitopsFinished(&current, g.options.Provider, g.versions[phase], phase == phaseRejected, repeat, previous, request)
	})
}

func (g *gitops) triggerSync(ctx context.Context, repeat bool, request string) error {
	if repeat && g.options.Provider == providerArgo {
		patch, err := json.Marshal(map[string]any{"operation": map[string]any{"initiatedBy": map[string]string{"username": "haptic-lifecycle-test"}, "sync": map[string]any{"prune": true, "syncOptions": syncOptions()}}})
		if err != nil {
			return err
		}
		_, err = g.session.Kube(ctx, nil, "-n", argoNamespace, "patch", "application", chartName, "--type=merge", "-p", string(patch))
		return err
	}
	if g.options.Provider == "flux" {
		_, err := g.session.Kube(ctx, nil, "-n", fluxNamespace, "annotate", "helmrelease", chartName, "reconcile.fluxcd.io/requestedAt="+request, "--overwrite")
		return err
	}
	return nil
}

func nestedString(object map[string]any, path ...string) string {
	value, _, _ := unstructured.NestedString(object, path...)
	return value
}

func gitopsFinished(current *unstructured.Unstructured, provider, version string, rejected, repeat bool, previous, request string) (testutil.PollResult, error) {
	if provider == providerArgo {
		return argoFinished(current, version, rejected, repeat, previous)
	}
	return fluxFinished(current, version, rejected, request)
}

func argoFinished(current *unstructured.Unstructured, version string, rejected, repeat bool, previous string) (testutil.PollResult, error) {
	if nestedString(current.Object, "status", "operationState", "syncResult", "revision") != version || (repeat && nestedString(current.Object, "status", "operationState", "startedAt") == previous) {
		return testutil.PollPending, nil
	}
	outcome := nestedString(current.Object, "status", "operationState", "phase")
	if outcome == "Failed" || outcome == "Error" {
		if rejected {
			return testutil.PollSucceeded, nil
		}
		return testutil.PollFailed, fmt.Errorf("argo sync failed: %s", nestedString(current.Object, "status", "operationState", "message"))
	}
	if !rejected && outcome == "Succeeded" && nestedString(current.Object, "status", "sync", "status") == "Synced" && nestedString(current.Object, "status", "health", "status") == "Healthy" {
		return testutil.PollSucceeded, nil
	}
	return testutil.PollPending, nil
}

func fluxFinished(current *unstructured.Unstructured, version string, rejected bool, request string) (testutil.PollResult, error) {
	if nestedString(current.Object, "status", "lastAttemptedRevision") != version {
		return testutil.PollPending, nil
	}
	conditions, _, err := unstructured.NestedSlice(current.Object, "status", "conditions")
	if err != nil {
		return testutil.PollFailed, err
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			return testutil.PollFailed, errors.New("invalid Flux condition")
		}
		generation, _, err := unstructured.NestedInt64(condition, "observedGeneration")
		if err != nil {
			return testutil.PollFailed, err
		}
		if generation != current.GetGeneration() {
			continue
		}
		if rejected && condition["type"] == "Released" && condition["status"] == "False" && condition["reason"] == "UpgradeFailed" {
			return testutil.PollSucceeded, nil
		}
		if !rejected && condition["type"] == "Ready" && condition["status"] == conditionTrue && nestedString(current.Object, "status", "lastHandledReconcileAt") == request {
			return testutil.PollSucceeded, nil
		}
	}
	return testutil.PollPending, nil
}
