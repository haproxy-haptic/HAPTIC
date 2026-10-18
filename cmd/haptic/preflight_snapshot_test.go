package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/haproxy-haptic/HAPTIC/pkg/buildinfo"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/rollout"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func preparationFixture() (cluster *liveCluster, config, library *unstructured.Unstructured) {
	config = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "haproxy-haptic.org/v1alpha1", "kind": "HAProxyTemplateConfig",
		"metadata": map[string]any{"namespace": "test", "name": "old-config", "uid": "config-uid", "generation": int64(2)},
		"spec":     map[string]any{"libraryRefs": []any{map[string]any{"name": "base", "revision": "r1"}}},
	}}
	library = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "haproxy-haptic.org/v1alpha1", "kind": "HAProxyTemplateLibrary",
		"metadata": map[string]any{"namespace": "test", "name": "base", "uid": "library-uid", "generation": int64(3)},
		"spec":     map[string]any{"revision": "r1", "haproxyConfig": map[string]any{"template": "global\n"}},
	}}
	return &liveCluster{dyn: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), config, library)}, config, library
}

func TestLiveConfigurationRejectsChangedSourceIdentity(t *testing.T) {
	for _, change := range []string{"generation", "uid"} {
		t.Run(change, func(t *testing.T) {
			cluster, _, library := preparationFixture()
			snapshot, err := readLiveConfiguration(t.Context(), cluster, "test", "old-config")
			require.NoError(t, err)
			if change == "generation" {
				library.SetGeneration(4)
			} else {
				library.SetUID("replacement")
			}
			_, err = cluster.dyn.Resource(templateLibraryGVR).Namespace("test").Update(t.Context(), library, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.ErrorContains(t, snapshot.requireUnchanged(t.Context(), cluster), "changed during preparation")
		})
	}
}

func TestLiveConfigurationDistinguishesMissingLibraryFromAPIAndTornApply(t *testing.T) {
	t.Run("confirmed missing library remains repairable", func(t *testing.T) {
		cluster, _, _ := preparationFixture()
		require.NoError(t, cluster.dyn.Resource(templateLibraryGVR).Namespace("test").Delete(t.Context(), "base", metav1.DeleteOptions{}))
		snapshot, err := readLiveConfiguration(t.Context(), cluster, "test", "old-config")
		require.NoError(t, err)
		require.Len(t, snapshot.missing, 1)
		require.NoError(t, snapshot.requireUnchanged(t.Context(), cluster))
	})
	t.Run("forbidden is not a missing object", func(t *testing.T) {
		cluster, _, _ := preparationFixture()
		cluster.dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "haproxytemplatelibraries", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "haproxytemplatelibraries"}, "base", errors.New("denied"))
		})
		_, err := readLiveConfiguration(t.Context(), cluster, "test", "old-config")
		require.ErrorContains(t, err, "denied")
	})
	t.Run("revision mismatch is not recovery", func(t *testing.T) {
		cluster, _, library := preparationFixture()
		require.NoError(t, unstructured.SetNestedField(library.Object, "r2", "spec", "revision"))
		_, err := cluster.dyn.Resource(templateLibraryGVR).Namespace("test").Update(t.Context(), library, metav1.UpdateOptions{})
		require.NoError(t, err)
		_, err = readLiveConfiguration(t.Context(), cluster, "test", "old-config")
		require.ErrorContains(t, err, "different revision")
	})
	t.Run("missing member appears during preparation", func(t *testing.T) {
		cluster, _, library := preparationFixture()
		require.NoError(t, cluster.dyn.Resource(templateLibraryGVR).Namespace("test").Delete(t.Context(), "base", metav1.DeleteOptions{}))
		snapshot, err := readLiveConfiguration(t.Context(), cluster, "test", "old-config")
		require.NoError(t, err)
		_, err = cluster.dyn.Resource(templateLibraryGVR).Namespace("test").Create(t.Context(), library, metav1.CreateOptions{})
		require.NoError(t, err)
		require.ErrorContains(t, snapshot.requireUnchanged(t.Context(), cluster), "appeared during preparation")
	})
}

func TestRejectedConfigurationRequiresAConfigVerdict(t *testing.T) {
	tests := []struct {
		name    string
		results *testrunner.TestResults
		err     error
		want    bool
	}{
		{"compile", nil, templating.NewCompilationError("t", "bad", errors.New("syntax")), true},
		{"structure", nil, &configurationError{errors.New("invalid config")}, true},
		{"failed assertion", &testrunner.TestResults{FailedTests: 1, TestResults: []testrunner.TestResult{{Assertions: []testrunner.AssertionResult{{ConfigRejected: true}}}}}, errors.New("assertion"), true},
		{"unavailable checker", &testrunner.TestResults{FailedTests: 1, TestResults: []testrunner.TestResult{{RenderError: "creating test temp directory: disk full"}}}, errors.New("test failed"), false},
		{"passed negative fixture", &testrunner.TestResults{FailedTests: 1, TestResults: []testrunner.TestResult{{Passed: true, Assertions: []testrunner.AssertionResult{{ConfigRejected: true}}}, {RenderError: "disk full"}}}, errors.New("test failed"), false},
		{"missing tests", nil, errNoValidationTests, true},
		{"API unavailable", nil, apierrors.NewServiceUnavailable("apiserver"), false},
		{"unknown error", nil, errors.New("cannot create temp file"), false},
		{"unavailable test run", &testrunner.TestResults{}, errors.New("timed out"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, rejectedConfiguration(tt.results, tt.err)) })
	}
}

func TestPreparationReadsExistingConfigName(t *testing.T) {
	name, err := controllerConfigName(&corev1.Container{Env: []corev1.EnvVar{{Name: "CRD_NAME", Value: "old-name"}}})
	require.NoError(t, err)
	assert.Equal(t, "old-name", name)
	_, err = controllerConfigName(&corev1.Container{Env: []corev1.EnvVar{{Name: "CRD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}})
	require.ErrorContains(t, err, "valueFrom")
	name, err = controllerConfigName(&corev1.Container{Args: []string{"--crd-name=explicit"}, Env: []corev1.EnvVar{{Name: "CRD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}})
	require.NoError(t, err)
	assert.Equal(t, "explicit", name)
}

func TestLiveConfigurationRejectsDuplicateSourceContent(t *testing.T) {
	cluster, config, library := preparationFixture()
	require.NoError(t, unstructured.SetNestedField(library.Object, map[string]any{"same": map[string]any{"template": "first"}}, "spec", "templateSnippets"))
	second := library.DeepCopy()
	second.SetName("second")
	second.SetUID("second")
	require.NoError(t, unstructured.SetNestedSlice(config.Object, []any{map[string]any{"name": "base", "revision": "r1"}, map[string]any{"name": "second", "revision": "r1"}}, "spec", "libraryRefs"))
	_, err := cluster.dyn.Resource(templateConfigGVR).Namespace("test").Update(t.Context(), config, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = cluster.dyn.Resource(templateLibraryGVR).Namespace("test").Update(t.Context(), library, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = cluster.dyn.Resource(templateLibraryGVR).Namespace("test").Create(t.Context(), second, metav1.CreateOptions{})
	require.NoError(t, err)
	snapshot, err := readLiveConfiguration(t.Context(), cluster, "test", "old-config")
	require.NoError(t, err)
	require.Error(t, snapshot.validateAssembly(), "a stable invalid merged set must select repair rather than an unavailable-check error")
}

func TestPreparationRejectsChangedConfigIdentityBeforeClusterAccess(t *testing.T) {
	current := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: controllerContainer, Env: []corev1.EnvVar{{Name: "CRD_NAME", Value: "old"}}}}}}}}
	target := current.DeepCopy()
	target.Spec.Template.Spec.Containers[0].Env[0].Value = "new"
	err := prepareExistingController(t.Context(), current, target, schemaSource{}, buildinfo.Identity{}, slog.Default())
	require.ErrorContains(t, err, "keep controller.configName unchanged")
}

func TestRenderedControllerFromBundledChart(t *testing.T) {
	values := filepath.Join(t.TempDir(), "values.yaml")
	require.NoError(t, os.WriteFile(values, []byte("{}\n"), 0o600))
	manifests, err := renderChartManifests(repoPath(t, "charts", "haptic"), []string{values}, "", offlineCaps())
	require.NoError(t, err)
	deployment, err := renderedController(manifests)
	require.NoError(t, err)
	assert.NotNil(t, rollout.Container(deployment.Spec.Template.Spec.Containers, controllerContainer))
}
