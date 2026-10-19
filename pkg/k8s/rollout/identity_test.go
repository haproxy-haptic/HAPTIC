package rollout

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReadIdentityUsesDirectHealthPortAndDistinguishesFailures(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"candidate", 200, `{"version":"0.7.0","sourceHash":"source","haproxyVersion":"3.3"}`, false},
		{"old binary", 404, "404 page not found", false},
		{"forbidden", 403, `{"message":"forbidden"}`, true},
		{"transport", 502, "bad gateway", true},
		{"redirect", 302, "", true},
		{"malformed", 200, "not json", true},
		{"oversized", 200, strings.Repeat("a", identityResponseLimit+1), true},
		{"unknown identity", 200, `{"version":"0.7.0","sourceHash":"unknown","haproxyVersion":"3.3"}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/build", r.URL.Path)
				w.Header().Set("Location", "http://192.0.2.1/another-controller")
				w.WriteHeader(tt.status)
				_, _ = fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			pod := identityTestPod(t, server.URL)
			identity, err := ReadIdentity("controller")(t.Context(), pod)
			if tt.wantError {
				require.Error(t, err)
				if tt.status == http.StatusFound {
					require.ErrorContains(t, err, "HTTP 302")
				}
				return
			}
			require.NoError(t, err)
			if tt.status == http.StatusOK {
				assert.Equal(t, candidateIdentity, identity)
			} else {
				assert.Empty(t, identity.SourceHash)
			}
		})
	}
}

func identityTestPod(t *testing.T, address string) *corev1.Pod {
	t.Helper()
	endpoint, err := url.Parse(address)
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(endpoint.Host)
	require.NoError(t, err)
	portNumber, err := strconv.ParseInt(port, 10, 32)
	require.NoError(t, err)
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "controller-1", Namespace: "test"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "sidecar", Ports: []corev1.ContainerPort{{Name: "healthz", ContainerPort: 9999}}},
			{Name: "controller", Ports: []corev1.ContainerPort{{Name: "healthz", ContainerPort: int32(portNumber)}}},
		}}, Status: corev1.PodStatus{PodIP: host}}
}

func TestIdentityAddressSupportsIPv6AndRejectsNonIPHosts(t *testing.T) {
	pod := identityTestPod(t, "http://[2001:db8::3]:8123")
	address, err := identityAddress(pod, "controller")
	require.NoError(t, err)
	assert.Equal(t, "http://[2001:db8::3]:8123/build", address)
	pod.Status.PodIP = "other-controller.example"
	_, err = identityAddress(pod, "controller")
	require.ErrorContains(t, err, "valid Pod IP")
}
