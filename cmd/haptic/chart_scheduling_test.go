package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"helm.sh/helm/v4/pkg/strvals"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/dataplanetest"
)

func TestBundledChartRateLimitCISchedulingBudget(t *testing.T) {
	t.Cleanup(dataplanetest.InstallFakeHAProxy())
	settings := rateLimitCISettings(t)
	for _, tt := range []struct {
		name     string
		settings string
		wantCPU  int64
		fits     bool
	}{
		{name: "current profile", settings: settings, wantCPU: 3925, fits: true},
		{name: "previous overcommitted profile", settings: settings + ";controller.resources.requests.cpu=600m", wantCPU: 4025},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manifests := rateLimitCIManifests(t, tt.settings)
			for _, path := range []string{"tests/kindutil/calico.yaml", "scripts/dev-env-assets/metrics-server.yaml"} {
				data, err := os.ReadFile(repoPath(t, path))
				require.NoError(t, err)
				manifests[path] = string(data)
			}
			requests := workloadCPURequests(t, manifests)
			require.Equal(t, int64(1100), requests["haptic-haproxy"])
			require.Equal(t, int64(375), requests["haptic-rl-store"])
			// Kind's control plane and CoreDNS request 850m; the owned kube-proxy reserves 100m (#483).
			total := int64(950)
			for _, cpu := range requests {
				total += cpu
			}
			t.Logf("rendered CPU requests: %v; with Kind control plane, DNS and proxy: %dm", requests, total)
			require.Equal(t, tt.wantCPU, total)
			require.Equal(t, tt.fits, total <= 4000, "the complete rate-limit fixture must fit the four-core runner")
		})
	}
}

func rateLimitCISettings(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(repoPath(t, ".github/workflows/ci-speed-proof.yml"))
	require.NoError(t, err)
	var workflow struct {
		Jobs map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(data, &workflow))
	env := workflow.Jobs["suite_e2e_rate_limit"].Env
	require.Equal(t, "rate-limit", env["HAPTIC_E2E_PROFILE"])
	var suite struct {
		Settings string `json:"settings"`
	}
	require.NoError(t, json.Unmarshal([]byte(env["CI_SUITE"]), &suite))
	require.NotEmpty(t, suite.Settings)
	require.Equal(t, suite.Settings, env["HAPTIC_E2E_EXTRA_SET"])
	return suite.Settings
}

func rateLimitCIManifests(t *testing.T, settings string) map[string]string {
	t.Helper()
	values := map[string]any{}
	for _, setting := range strings.Split(settings+";rateLimit.shared.enabled=true;rateLimit.shared.managedStore.enabled=true", ";") {
		require.NoError(t, strvals.ParseInto(setting, values))
	}
	data, err := yaml.Marshal(values)
	require.NoError(t, err)
	valuesFile := writeValues(t, t.TempDir(), "profile", string(data))
	manifests, err := renderChartManifests(repoPath(t, "charts/haptic"),
		[]string{repoPath(t, "scripts/dev-env-assets/e2e-values.yaml"), valuesFile}, "", offlineCaps())
	require.NoError(t, err)
	docs, err := collectConfigDocuments(manifests)
	require.NoError(t, err)
	configFile := writeValues(t, t.TempDir(), "config", docs)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	schemas, err := newDirSchemaSource(repoPath(t, "tests/schemas"), logger)
	require.NoError(t, err)
	setup, err := setupValidation(t.Context(), []string{configFile}, schemas, nil, logger)
	require.NoError(t, err)
	t.Cleanup(setup.Cleanup)
	cfg, err := conversion.ConvertSpec(setup.ConfigSpec)
	require.NoError(t, err)
	store, err := setup.Engine.Render(t.Context(), "rate-limit-store", map[string]any{"extraContext": cfg.TemplatingSettings.ExtraContext})
	require.NoError(t, err)
	manifests["rate-limit-store"] = store
	return manifests
}

func workloadCPURequests(t *testing.T, manifests map[string]string) map[string]int64 {
	t.Helper()
	requests := map[string]int64{}
	for _, name := range slices.Sorted(maps.Keys(manifests)) {
		if filepath.Base(name) == "NOTES.txt" {
			continue
		}
		decoder := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(manifests[name]), 4096)
		for {
			var object struct {
				metav1.TypeMeta `json:",inline"`
				Metadata        metav1.ObjectMeta `json:"metadata"`
				Spec            struct {
					Replicas *int32                 `json:"replicas"`
					Template corev1.PodTemplateSpec `json:"template"`
				} `json:"spec"`
			}
			err := decoder.Decode(&object)
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err, name)
			if object.Kind != "Deployment" && object.Kind != "StatefulSet" && object.Kind != "DaemonSet" {
				continue
			}
			replicas := int64(1)
			if object.Spec.Replicas != nil {
				replicas = int64(*object.Spec.Replicas)
			}
			requests[object.Metadata.Name] = replicas * podRequestedMilliCPU(&object.Spec.Template.Spec)
		}
	}
	return requests
}

func podRequestedMilliCPU(pod *corev1.PodSpec) int64 {
	var regular, sidecars, initPeak int64
	for i := range pod.Containers {
		regular += pod.Containers[i].Resources.Requests.Cpu().MilliValue()
	}
	for i := range pod.InitContainers {
		container := &pod.InitContainers[i]
		cpu := container.Resources.Requests.Cpu().MilliValue()
		if container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars += cpu
			initPeak = max(initPeak, sidecars)
		} else {
			initPeak = max(initPeak, sidecars+cpu)
		}
	}
	return max(regular+sidecars, initPeak) + pod.Overhead.Cpu().MilliValue()
}

func TestPodRequestedMilliCPUIncludesRestartableInitContainers(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	container := func(cpu string, restart *corev1.ContainerRestartPolicy) corev1.Container {
		return corev1.Container{RestartPolicy: restart, Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
		}}
	}
	pod := corev1.PodSpec{
		Containers: []corev1.Container{container("250m", nil)},
		InitContainers: []corev1.Container{
			container("100m", &always), container("300m", nil), container("200m", &always),
		},
		Overhead: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m")},
	}
	require.Equal(t, int64(560), podRequestedMilliCPU(&pod))
	pod.InitContainers[1] = container("600m", nil)
	require.Equal(t, int64(710), podRequestedMilliCPU(&pod))
}
