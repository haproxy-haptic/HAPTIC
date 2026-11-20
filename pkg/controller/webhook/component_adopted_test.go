package webhook

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	pkgwebhook "github.com/haproxy-haptic/HAPTIC/pkg/webhook"
)

// TestAdoptedServer_SurvivesIterationTeardown keeps admission reachable while
// iterations replace fail-closed validator generations (#110).
func TestAdoptedServer_SurvivesIterationTeardown(t *testing.T) {
	certPEM, keyPEM, pool := generateLoopbackCert(t)

	procCtx, procCancel := context.WithCancel(context.Background())
	defer procCancel()

	server := startServerOnFreePort(t, procCtx, certPEM, keyPEM)

	post := admissionPoster(t, server.Addr(), pool)

	// Phase 1 — iteration A installs its table and serves from it.
	iterA, cancelA := context.WithCancel(procCtx)
	componentA := newAdoptingComponent(t, server, "iteration-A")
	doneA := make(chan error, 1)
	go func() { doneA <- componentA.Start(iterA) }()
	<-componentA.Listening()

	allowed, msg := post()
	require.False(t, allowed, "iteration A's validator must deny")
	require.Contains(t, msg, "iteration-A")

	// Phase 2 — the iteration is torn down, exactly as a config change does.
	// The listener stays bound with an empty fail-closed generation.
	cancelA()
	select {
	case err := <-doneA:
		require.NoError(t, err, "component must exit cleanly on iteration teardown")
	case <-time.After(10 * time.Second):
		t.Fatal("component did not exit after its iteration was cancelled")
	}

	allowed, msg = post()
	require.False(t, allowed, "iteration teardown must leave a fail-closed listener")
	require.Contains(t, msg, "no validator registered")

	// Phase 3 — iteration B takes over and its table replaces A's.
	iterB, cancelB := context.WithCancel(procCtx)
	defer cancelB()
	componentB := newAdoptingComponent(t, server, "iteration-B")
	go func() { _ = componentB.Start(iterB) }()
	<-componentB.Listening()

	allowed, msg = post()
	require.False(t, allowed)
	require.Contains(t, msg, "iteration-B", "the new iteration's table must take over")
	require.NotContains(t, msg, "iteration-A")
}

func TestAdoptedServer_RejectsPartialReplacement(t *testing.T) {
	certPEM, keyPEM, pool := generateLoopbackCert(t)
	procCtx, procCancel := context.WithCancel(context.Background())
	defer procCancel()

	server := startServerOnFreePort(t, procCtx, certPEM, keyPEM)
	post := admissionPoster(t, server.Addr(), pool)

	iterA, cancelA := context.WithCancel(procCtx)
	componentA := newAdoptingComponent(t, server, "iteration-A")
	go func() { _ = componentA.Start(iterA) }()
	<-componentA.Listening()

	componentB := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		&Config{
			Server: server,
			Rules: []WebhookRule{
				{APIGroup: "networking.k8s.io", APIVersion: "v1", Resource: "ingresses"},
				{APIGroup: "example.com", APIVersion: "v1", Resource: "missing"},
			},
			DryRunValidator: staticDenyValidator{reason: "denied by iteration-B"},
		},
		ingressRESTMapper(),
		nil,
	)

	err := componentB.Start(procCtx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "example.com/v1/missing")

	allowed, message := post()
	require.False(t, allowed)
	require.Contains(t, message, "iteration-A")
	require.NotContains(t, message, "iteration-B")
	cancelA()
}

func TestAdoptedServer_TeardownRetiresPreviousTable(t *testing.T) {
	certPEM, keyPEM, pool := generateLoopbackCert(t)
	procCtx, procCancel := context.WithCancel(context.Background())
	defer procCancel()

	server := startServerOnFreePort(t, procCtx, certPEM, keyPEM)
	post := admissionPoster(t, server.Addr(), pool)
	iterA, cancelA := context.WithCancel(procCtx)
	componentA := newAdoptingComponent(t, server, "iteration-A")
	retiredA := make(chan struct{})
	componentA.config.OnGenerationRetired = func() { close(retiredA) }
	doneA := make(chan error, 1)
	go func() { doneA <- componentA.Start(iterA) }()
	<-componentA.Listening()

	cancelA()
	require.NoError(t, <-doneA)
	select {
	case <-retiredA:
	case <-time.After(time.Second):
		t.Fatal("component returned before its validator generation retired")
	}

	allowed, message := post()
	require.False(t, allowed)
	require.Contains(t, message, "no validator registered")
	require.NotContains(t, message, "iteration-A")
}

func TestAdoptedServer_UsesIterationContext(t *testing.T) {
	certPEM, keyPEM, pool := generateLoopbackCert(t)
	procCtx, procCancel := context.WithCancel(context.Background())
	defer procCancel()

	server := startServerOnFreePort(t, procCtx, certPEM, keyPEM)
	component := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		&Config{
			Server: server,
			Rules: []WebhookRule{{
				APIGroup:   "networking.k8s.io",
				APIVersion: "v1",
				Resource:   "ingresses",
			}},
			DryRunValidator: contextStateValidator{},
		},
		ingressRESTMapper(),
		nil,
	)
	iterCtx, cancelIter := context.WithCancel(context.WithValue(procCtx, iterationContextKey{}, true))
	done := make(chan error, 1)
	go func() { done <- component.Start(iterCtx) }()
	<-component.Listening()
	post := admissionPoster(t, server.Addr(), pool)

	allowed, message := post()
	require.False(t, allowed)
	require.Contains(t, message, "iteration context active")

	cancelIter()
	require.NoError(t, <-done)
	allowed, message = post()
	require.False(t, allowed)
	require.Contains(t, message, "no validator registered")
}

// startServerOnFreePort binds the server to a free loopback port.
//
// ServerConfig.Port 0 means "unset" and defaults to 9443, so an ephemeral port
// cannot be requested directly; the port is reserved and released first. That
// leaves a race against anything else binding in between, so a lost race is
// retried with a fresh port. This retries ACQUIRING A PORT, never a failed
// assertion — a genuine bind failure still fails the test after the attempts
// are spent.
func startServerOnFreePort(
	t *testing.T,
	ctx context.Context,
	certPEM, keyPEM []byte,
) *pkgwebhook.Server {
	t.Helper()

	const attempts = 5
	var lastErr error
	for range attempts {
		reserve, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := reserve.Addr().(*net.TCPAddr).Port
		require.NoError(t, reserve.Close())

		server, err := pkgwebhook.NewServer(&pkgwebhook.ServerConfig{
			Port:        port,
			BindAddress: "127.0.0.1",
			Path:        "/validate",
			CertPEM:     certPEM,
			KeyPEM:      keyPEM,
		})
		require.NoError(t, err)

		serverErr := make(chan error, 1)
		go func() { serverErr <- server.Start(ctx) }()

		select {
		case <-server.Listening():
			return server
		case err := <-serverErr:
			lastErr = err
		case <-time.After(10 * time.Second):
			t.Fatal("server did not bind within 10s")
		}
	}
	t.Fatalf("could not bind a free loopback port in %d attempts: %v", attempts, lastErr)
	return nil
}

// newAdoptingComponent builds a component that adopts server and whose config
// validator denies with a marker naming its iteration, so a response identifies
// which table answered.
func newAdoptingComponent(t *testing.T, server *pkgwebhook.Server, marker string) *Component {
	t.Helper()
	return New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		&Config{
			Port:   0,
			Path:   "/validate",
			Server: server,
			Rules: []WebhookRule{{
				APIGroup:   "networking.k8s.io",
				APIVersion: "v1",
				Resource:   "ingresses",
			}},
			DryRunValidator: staticDenyValidator{reason: "denied by " + marker},
		},
		ingressRESTMapper(),
		nil,
	)
}

// staticDenyValidator answers every admission with a fixed denial, so a
// response identifies which iteration's table served it.
type staticDenyValidator struct{ reason string }

func (v staticDenyValidator) ValidateAdmission(context.Context, string, string, string, any, any, string) (allowed bool, reason string, warnings []string, failure error) {
	return false, v.reason, nil, nil
}

type contextStateValidator struct{}

type iterationContextKey struct{}

func (contextStateValidator) ValidateAdmission(ctx context.Context, _, _, _ string, _, _ any, _ string) (allowed bool, reason string, warnings []string, failure error) {
	if active, _ := ctx.Value(iterationContextKey{}).(bool); !active {
		return false, "iteration context missing", nil, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err.Error(), nil, err
	}
	return false, "iteration context active", nil, nil
}

// ingressRESTMapper resolves ingresses -> Ingress without a cluster.
func ingressRESTMapper() meta.RESTMapper {
	gv := schema.GroupVersion{Group: "networking.k8s.io", Version: "v1"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	mapper.Add(gv.WithKind("Ingress"), meta.RESTScopeNamespace)
	return mapper
}

// generateLoopbackCert returns a self-signed certificate valid for 127.0.0.1
// plus a pool trusting it, so the test client verifies the chain normally
// rather than disabling verification.
func generateLoopbackCert(t *testing.T) (certPEM, keyPEM []byte, pool *x509.CertPool) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"haptic-unit-test"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	pool = x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(certPEM))
	return certPEM, keyPEM, pool
}

func admissionPoster(t *testing.T, addr string, pool *x509.CertPool) func() (allowed bool, message string) {
	t.Helper()

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}
	url := fmt.Sprintf("https://%s/validate", addr)

	return func() (bool, string) {
		t.Helper()

		review := admissionv1.AdmissionReview{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "admission.k8s.io/v1",
				Kind:       "AdmissionReview",
			},
			Request: &admissionv1.AdmissionRequest{
				UID:       "test-uid",
				Operation: admissionv1.Update,
				Namespace: "default",
				Name:      "echo",
				Kind: metav1.GroupVersionKind{
					Group:   "networking.k8s.io",
					Version: "v1",
					Kind:    "Ingress",
				},
				Object: runtime.RawExtension{
					Raw: []byte(`{"apiVersion":"networking.k8s.io/v1",` +
						`"kind":"Ingress",` +
						`"metadata":{"name":"echo","namespace":"default"},` +
						`"spec":{}}`),
				},
			},
		}
		body, err := json.Marshal(review)
		require.NoError(t, err)

		resp, err := client.Post(url, "application/json", bytes.NewReader(body))
		require.NoError(t, err, "admission request must reach a bound listener")
		defer func() { _ = resp.Body.Close() }()

		var out admissionv1.AdmissionReview
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		require.NotNil(t, out.Response)

		message := ""
		if out.Response.Result != nil {
			message = out.Response.Result.Message
		}
		return out.Response.Allowed, message
	}
}
