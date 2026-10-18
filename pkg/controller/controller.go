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

// Package controller provides the main controller orchestration for the HAProxy template ingress controller.
//
// The controller follows an event-driven architecture with a reinitialization loop:
// 1. Fetch and validate initial configuration
// 2. Create EventBus and components
// 3. Start components and watchers
// 4. Wait for configuration changes
// 5. Reinitialize on valid config changes
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/commentator"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/configchange"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/configloader"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/credentialsloader"
	leaderelectionctrl "github.com/haproxy-haptic/HAPTIC/pkg/controller/leaderelection"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validator"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/introspection"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	k8stypes "github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
	"github.com/haproxy-haptic/HAPTIC/pkg/lifecycle"
	pkgmetrics "github.com/haproxy-haptic/HAPTIC/pkg/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/transportsecurity"
	pkgwebhook "github.com/haproxy-haptic/HAPTIC/pkg/webhook"
)

const (
	// RetryDelay is the duration to wait before retrying after an iteration failure.
	RetryDelay = 5 * time.Second
	// ConfigPollInterval is the interval for polling HAProxyTemplateConfig availability.
	ConfigPollInterval = 5 * time.Second
	// DebugEventBufferSize is the size of the event buffer for debug/introspection.
	DebugEventBufferSize = busevents.DebugSubscriberBuffer
	// ShutdownTimeout is the maximum time to wait for goroutines to finish during shutdown.
	// Set to 25s to allow clean exit before Kubernetes' default 30s terminationGracePeriodSeconds.
	ShutdownTimeout = 25 * time.Second
	// ShutdownProgressInterval is how often to log progress during shutdown.
	ShutdownProgressInterval = 5 * time.Second
	// ProcessShutdownTimeout leaves one second before the chart's SIGKILL deadline.
	ProcessShutdownTimeout = 89 * time.Second
)

// buildVersionInfo holds build-time version information exposed via haptic_build_info metric.
// Set this before calling Run() using SetBuildInfo().
var buildVersionInfo struct {
	version        string
	haproxyVersion string
	goVersion      string
}

func init() {
	buildVersionInfo.version = "dev"
	buildVersionInfo.haproxyVersion = "unknown"
	buildVersionInfo.goVersion = runtime.Version()
}

// SetBuildInfo configures version information exposed via the haptic_build_info Prometheus metric.
// Must be called before Run() to take effect.
//
// Parameters:
//   - version: Controller version (e.g., "0.1.0-alpha.10")
//   - haproxyVersion: HAProxy version the controller was built for (e.g., "3.2")
func SetBuildInfo(version, haproxyVersion string) {
	buildVersionInfo.version = version
	buildVersionInfo.haproxyVersion = haproxyVersion
}

// GVRs for Kubernetes resources used by the controller.
const (
	// hapticAPIGroup and hapticAPIVersion identify HAPTIC's own CRDs.
	hapticAPIGroup   = "haproxy-haptic.org"
	hapticAPIVersion = "v1alpha1"
)

var (
	// crdGVR is the GVR for HAProxyTemplateConfig custom resource.
	crdGVR = schema.GroupVersionResource{
		Group:    hapticAPIGroup,
		Version:  hapticAPIVersion,
		Resource: "haproxytemplateconfigs",
	}
	// libraryGVR is the GVR for HAProxyTemplateLibrary custom resource.
	libraryGVR = schema.GroupVersionResource{
		Group:    hapticAPIGroup,
		Version:  hapticAPIVersion,
		Resource: "haproxytemplatelibraries",
	}
	// secretGVR is the GVR for Kubernetes Secrets.
	secretGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "secrets",
	}
	// haproxyCfgGVR is the GVR for HAProxyCfg custom resource.
	haproxyCfgGVR = schema.GroupVersionResource{
		Group:    hapticAPIGroup,
		Version:  hapticAPIVersion,
		Resource: "haproxycfgs",
	}
	// haproxyMapFileGVR, haproxyGeneralFileGVR, and haproxyCRTListFileGVR are the
	// GVRs for HAPTIC's own published auxiliary-file CRDs, read back to prime
	// `currentFiles` on a cold iteration.
	haproxyMapFileGVR     = v1alpha1.SchemeGroupVersion.WithResource("haproxymapfiles")
	haproxyGeneralFileGVR = v1alpha1.SchemeGroupVersion.WithResource("haproxygeneralfiles")
	haproxyCRTListFileGVR = v1alpha1.SchemeGroupVersion.WithResource("haproxycrtlistfiles")
)

// configState tracks initialization state for health checks.
// It allows the health endpoint to report unhealthy status until
// the HAProxyTemplateConfig is successfully loaded AND the staged
// startup (resource watchers, event bus, reconciliation components,
// leader election, webhook, debug servers) has finished. /healthz
// returns 503 until both gates pass, then 200 — operators (and the
// e2e suite) get a single "is the controller ready to accept work"
// signal without polling internal pipeline state that's wiped on
// every reconciliation trigger.
type configState struct {
	iterationID  iterationID
	mu           sync.RWMutex
	configLoaded bool
	initialized  bool
	message      string
}

type iterationID uint64

// SetLoaded marks the config as successfully loaded.
func (s *configState) SetLoaded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configLoaded = true
	s.message = ""
}

// SetWaiting marks the config as not yet loaded with a status message.
func (s *configState) SetWaiting(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configLoaded = false
	s.message = msg
}

// SetInitialized marks the controller as having finished its staged
// startup. Called once at the end of runIteration, right before the
// event loop. After this flips true, /healthz can report 200 (subject
// to the rest of the lifecycle.Registry components being healthy too).
func (s *configState) SetInitialized() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialized = true
}

// IsLoaded returns true if the config has been successfully loaded.
func (s *configState) IsLoaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.configLoaded
}

// IsInitialized returns true if the controller has finished its
// staged startup. Reset implicitly when a new iteration starts because
// configState is constructed fresh per runIteration call.
func (s *configState) IsInitialized() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.initialized
}

// Message returns the current status message (empty if config is loaded).
func (s *configState) Message() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.message
}

// persistentInfra holds infrastructure servers that persist across controller iterations.
// These servers are started once and reused to prevent port binding race conditions
// during rapid reinitializations.
type persistentInfra struct {
	AgentTLS              *transportsecurity.Source
	IntrospectionRegistry *introspection.Registry
	IntrospectionServer   *introspection.Server
	MetricsServer         *pkgmetrics.Server
	serverStarted         bool // True after first iteration has started the server
	metricsServerStarted  bool // True after first iteration has started the metrics server
	eventDropMetrics      *persistentEventDropMetrics
	processCancel         context.CancelFunc
	draining              atomic.Bool
	// leadership is the newest iteration's term, drained on process shutdown.
	leadership       atomic.Pointer[leaderCallbackState]
	introspectionRun *persistentServerRun
	metricsRun       *persistentServerRun
	eventSource      *eventSourceDelegate
	// lastMetrics is the newest iteration's metrics, which the next continues.
	lastMetrics *metrics.Metrics

	// The process-owned admission listener stays bound between validator generations.
	webhookMu     sync.Mutex
	WebhookServer *pkgwebhook.Server
	webhookRun    *persistentServerRun
	// webhookServerConfig is what the listener was actually built with, kept so
	// a later iteration passing something different is reported rather than
	// silently ignored.
	webhookServerConfig *pkgwebhook.ServerConfig

	loadGateTimeout time.Duration

	graceMu              sync.Mutex
	graceNow             func() time.Time
	currentIterationID   iterationID
	iterationInitialized bool
	reinitStartedAt      time.Time
	// attemptStartedAt is zero once the current startIteration call returned.
	attemptStartedAt time.Time

	servingMu sync.Mutex
	// serving is the newest iteration that completed startup and is not torn down.
	serving *servingIteration
	// siblings is nil when this replica cannot look for others; probes then
	// assume a converged sibling may exist.
	siblings *siblingProbes
}

type persistentServerRun struct {
	done     chan struct{}
	err      error
	stopping atomic.Bool
}

type namedPersistentServerRun struct {
	name string
	run  *persistentServerRun
}

func newPersistentServerRun() *persistentServerRun {
	return &persistentServerRun{done: make(chan struct{})}
}

func (r *persistentServerRun) finish(err error) {
	r.err = err
	close(r.done)
}

func (r *persistentServerRun) Done() <-chan struct{} {
	return r.done
}

func (r *persistentServerRun) Wait() error {
	<-r.done
	return r.err
}

type persistentWebhookServerError struct {
	err error
}

func (e *persistentWebhookServerError) Error() string {
	return fmt.Sprintf("persistent webhook server stopped: %v", e.err)
}

func (e *persistentWebhookServerError) Unwrap() error {
	return e.err
}

// reinitGraceMargin is how much longer than the load gate's budget /healthz
// keeps reporting healthy after a voluntary iteration restart. A reinit re-runs
// the load gate, so a smaller window lets a gate that legitimately uses its
// budget trip liveness and restart an advancing controller.
const reinitGraceMargin = 45 * time.Second

// loadGateBudget returns the load-path validationTests budget; zero means
// defaultLoadGateTimeout.
func (p *persistentInfra) loadGateBudget() time.Duration {
	if p.loadGateTimeout > 0 {
		return p.loadGateTimeout
	}
	return defaultLoadGateTimeout
}

func (p *persistentInfra) reinitGraceWindow() time.Duration {
	return p.loadGateBudget() + reinitGraceMargin
}

// EnsureWebhookServer returns the process-lifetime admission listener, creating
// and starting it on the first call and returning the same server afterwards.
//
// It blocks until the listener has actually bound, so the caller may treat a
// successful return as "admission requests are answerable" — the same contract
// the per-iteration component used to provide via Listening(). Subsequent calls
// return immediately: the socket is already up and only the validator table
// changes between iterations.
//
// ctx MUST be the process context, never an iteration context; cancelling it
// closes the listener for good.
func (p *persistentInfra) EnsureWebhookServer(
	ctx context.Context,
	config *pkgwebhook.ServerConfig,
	logger *slog.Logger,
) (*pkgwebhook.Server, error) {
	p.webhookMu.Lock()
	if p.draining.Load() {
		p.webhookMu.Unlock()
		// Keep the serving iteration alive until admission draining finishes.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	defer p.webhookMu.Unlock()

	if p.WebhookServer != nil {
		return p.reuseWebhookServer(config, logger)
	}

	server, err := pkgwebhook.NewServer(config)
	if err != nil {
		return nil, fmt.Errorf("creating persistent webhook server: %w", err)
	}

	run := p.startProcessServer(ctx, "webhook", server.Start, logger)
	p.WebhookServer = server
	p.webhookServerConfig = config
	p.webhookRun = run
	if err := waitForPersistentWebhookBind(ctx, server, run); err != nil {
		return nil, err
	}

	logger.Info("Persistent webhook listener bound; it now survives config reinitializations",
		"port", config.Port, "path", config.Path)

	return server, nil
}

func (p *persistentInfra) reuseWebhookServer(
	config *pkgwebhook.ServerConfig,
	logger *slog.Logger,
) (*pkgwebhook.Server, error) {
	if diff := describeServerConfigDiff(p.webhookServerConfig, config); diff != "" {
		logger.Warn("Webhook server config changed after the listener was bound; the bound listener keeps its original settings",
			"differences", diff,
			"note", "only the validator table changes across iterations")
	}
	if p.webhookRun == nil {
		return nil, &persistentWebhookServerError{err: errors.New("server has no completion owner")}
	}
	select {
	case <-p.webhookRun.Done():
		err := p.webhookRun.Wait()
		if err == nil {
			err = errors.New("server exited without an error")
		}
		return nil, &persistentWebhookServerError{err: err}
	default:
		return p.WebhookServer, nil
	}
}

func waitForPersistentWebhookBind(
	ctx context.Context,
	server *pkgwebhook.Server,
	run *persistentServerRun,
) error {
	select {
	case <-server.Listening():
		return nil
	case <-run.Done():
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := run.Wait()
		if err == nil {
			err = errors.New("server exited before binding")
		}
		return &persistentWebhookServerError{err: err}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *persistentInfra) currentWebhookRun() *persistentServerRun {
	p.webhookMu.Lock()
	defer p.webhookMu.Unlock()
	return p.webhookRun
}

func waitForPersistentServer(name string, run *persistentServerRun) error {
	if run == nil {
		return nil
	}
	if err := run.Wait(); err != nil {
		return fmt.Errorf("%s server: %w", name, err)
	}
	return nil
}

func persistentServerStopped(run *persistentServerRun) bool {
	select {
	case <-run.Done():
		return true
	default:
		return false
	}
}

func waitForPersistentServerStop(run *persistentServerRun, timeout <-chan time.Time) bool {
	if persistentServerStopped(run) {
		return true
	}
	select {
	case <-run.Done():
		return true
	case <-timeout:
		return false
	}
}

func collectStoppedPersistentServers(runs []namedPersistentServerRun, collected []bool) error {
	var result error
	for i, server := range runs {
		if collected[i] || server.run == nil || !persistentServerStopped(server.run) {
			continue
		}
		result = errors.Join(result, waitForPersistentServer(server.name, server.run))
	}
	return result
}

func (p *persistentInfra) waitForPersistentServers(timeout time.Duration) error {
	runs := []namedPersistentServerRun{
		{name: "introspection", run: p.introspectionRun},
		{name: "metrics", run: p.metricsRun},
		{name: "webhook", run: p.currentWebhookRun()},
	}

	remaining := max(timeout, 0)
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	collected := make([]bool, len(runs))
	var result error
	for i, server := range runs {
		if server.run == nil {
			collected[i] = true
			continue
		}
		if !waitForPersistentServerStop(server.run, timer.C) {
			result = errors.Join(result, collectStoppedPersistentServers(runs, collected))
			return errors.Join(result, fmt.Errorf("persistent servers did not stop within the remaining %s process shutdown budget", remaining))
		}
		result = errors.Join(result, waitForPersistentServer(server.name, server.run))
		collected[i] = true
	}
	return result
}

// describeServerConfigDiff names the fields on which a later EnsureWebhookServer
// call differs from the config the listener was built with, or "" when they
// agree. Only the fields that change observable server behaviour are compared.
func describeServerConfigDiff(bound, incoming *pkgwebhook.ServerConfig) string {
	if bound == nil || incoming == nil {
		return ""
	}
	var diffs []string
	if bound.Port != incoming.Port {
		diffs = append(diffs, fmt.Sprintf("port %d->%d", bound.Port, incoming.Port))
	}
	if bound.Path != incoming.Path {
		diffs = append(diffs, fmt.Sprintf("path %q->%q", bound.Path, incoming.Path))
	}
	if bound.CertDir != incoming.CertDir {
		diffs = append(diffs, fmt.Sprintf("certDir %q->%q", bound.CertDir, incoming.CertDir))
	}
	if bound.ReadTimeout != incoming.ReadTimeout {
		diffs = append(diffs, fmt.Sprintf("readTimeout %s->%s", bound.ReadTimeout, incoming.ReadTimeout))
	}
	if bound.WriteTimeout != incoming.WriteTimeout {
		diffs = append(diffs, fmt.Sprintf("writeTimeout %s->%s", bound.WriteTimeout, incoming.WriteTimeout))
	}
	return strings.Join(diffs, ", ")
}

// NoteIterationStart preserves an active episode or starts one after a completed iteration.
func (p *persistentInfra) NoteIterationStart() iterationID {
	p.graceMu.Lock()
	defer p.graceMu.Unlock()
	if p.reinitStartedAt.IsZero() && p.iterationInitialized {
		p.reinitStartedAt = p.graceTime()
	}
	p.currentIterationID++
	p.iterationInitialized = false
	p.attemptStartedAt = p.graceTime()
	return p.currentIterationID
}

// NoteAttemptReturned records that the startIteration call for id returned.
func (p *persistentInfra) NoteAttemptReturned(id iterationID) {
	p.graceMu.Lock()
	defer p.graceMu.Unlock()
	if id == p.currentIterationID {
		p.attemptStartedAt = time.Time{}
	}
}

// attemptStalled reports a startup attempt that has not returned within
// reinitGraceWindow. Failing attempts retry from live state; a hung one never does.
func (p *persistentInfra) attemptStalled() (time.Duration, bool) {
	p.graceMu.Lock()
	defer p.graceMu.Unlock()
	if p.attemptStartedAt.IsZero() {
		return 0, false
	}
	running := p.graceTime().Sub(p.attemptStartedAt)
	return running, running >= p.reinitGraceWindow()
}

func (p *persistentInfra) currentIteration() iterationID {
	p.graceMu.Lock()
	defer p.graceMu.Unlock()
	return p.currentIterationID
}

// NoteInitialized records that an iteration completed its staged startup.
func (p *persistentInfra) NoteInitialized(id iterationID) {
	p.graceMu.Lock()
	defer p.graceMu.Unlock()
	if id != 0 && id == p.currentIterationID {
		p.iterationInitialized = true
	}
}

// NoteSettled ends the current grace episode.
func (p *persistentInfra) NoteSettled(id iterationID) {
	p.graceMu.Lock()
	defer p.graceMu.Unlock()
	if id != 0 && id == p.currentIterationID {
		p.reinitStartedAt = time.Time{}
	}
}

// InReinitGrace reports whether the active reinitialization episode is within its budget.
func (p *persistentInfra) InReinitGrace() bool {
	p.graceMu.Lock()
	defer p.graceMu.Unlock()
	return !p.reinitStartedAt.IsZero() && p.graceTime().Sub(p.reinitStartedAt) < p.reinitGraceWindow()
}

func (p *persistentInfra) graceTime() time.Time {
	if p.graceNow != nil {
		return p.graceNow()
	}
	return time.Now()
}

// Run is the main entry point for the controller.
//
// It performs initial configuration fetching and validation, then enters a reinitialization
// loop where it responds to configuration changes by restarting with the new configuration.
//
// The controller uses an event-driven architecture:
//   - EventBus coordinates all components
//   - SingleWatcher monitors HAProxyTemplateConfig CRD and Secret
//   - Components react to events and publish results
//   - ConfigChangeHandler detects validated config changes and signals reinitialization
//
// Parameters:
//   - ctx: Context for cancellation (SIGTERM, SIGINT, etc.)
//   - k8sClient: Kubernetes client for API access
//   - crdName: Name of the HAProxyTemplateConfig this controller serves
//     (later wins); the last one is the primary, see primaryConfigName
//   - secretName: Name of the Secret containing the HAPTIC agent credentials
//   - webhookCertDir: Directory holding the webhook TLS cert (tls.crt/tls.key); empty disables the webhook
//   - webhookAdmissionTimeouts: Controller-side admission deadlines. Zero
//     values use the webhook component defaults.
//   - debugPort: Port for debug HTTP server (0 to disable)
//
// Returns:
//   - Error if the controller cannot start or encounters a fatal error
//   - nil if the context is cancelled (graceful shutdown)
func Run(
	ctx context.Context,
	k8sClient *client.Client,
	crdName string,
	secretName, webhookCertDir string,
	webhookAdmissionTimeouts WebhookAdmissionTimeouts,
	debugPort int,
	agentTLS *transportsecurity.Source,
) error {
	logger := slog.Default()

	logger.Debug("HAProxy Template Ingress Controller starting",
		"crd_name", crdName,
		"secret", secretName,
		"webhook_cert_dir", webhookCertDir,
		"namespace", k8sClient.Namespace())

	metricsPort, err := listenerPortFromEnv("METRICS_PORT", 9090, true)
	if err != nil {
		return err
	}
	webhookPort, err := listenerPortFromEnv("WEBHOOK_PORT", 9443, false)
	if err != nil {
		return err
	}
	loadGateTimeout, err := loadGateTimeoutFromEnv()
	if err != nil {
		return err
	}

	// Admission still needs live watchers and validators while its listener drains.
	procCtx, procCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer procCancel()
	infra := &persistentInfra{
		AgentTLS:              agentTLS,
		IntrospectionRegistry: introspection.NewRegistry(),
		eventDropMetrics:      &persistentEventDropMetrics{},
		processCancel:         procCancel,
		loadGateTimeout:       loadGateTimeout,
	}
	if debugPort > 0 {
		infra.IntrospectionServer = introspection.NewServer(fmt.Sprintf(":%d", debugPort), infra.IntrospectionRegistry)
		registerBuildIdentity(infra.IntrospectionServer)
	}
	if podName, selector := os.Getenv("POD_NAME"), os.Getenv(controllerPodSelectorEnv); podName != "" && selector != "" {
		siblings, err := newSiblingProbes(procCtx, k8sClient.Clientset(), k8sClient.Namespace(), podName, selector, logger)
		if err != nil {
			return err
		}
		infra.siblings = siblings
	}
	if metricsPort > 0 {
		infra.MetricsServer = pkgmetrics.NewServer(fmt.Sprintf(":%d", metricsPort), prometheus.NewRegistry())
	}
	shutdown := drainOnCancellation(ctx, procCtx, procCancel, func(drainCtx context.Context) error {
		err := infra.drainAdmission(drainCtx, logger)
		infra.leadership.Load().drain(logger)
		return err
	})

	iterations := &iterationSequence{logger: logger}
	err = runIterations(procCtx, logger, RetryDelay, func() error {
		return iterations.step(func(startup *configchange.ReloadRequest, previous *liveIteration) (*liveIteration, error) {
			return startIteration(procCtx, k8sClient, crdName, secretName, webhookCertDir, webhookAdmissionTimeouts, debugPort, webhookPort, infra, startup, previous, logger)
		})
	})
	err = errors.Join(err, iterations.close())
	procCancel()
	stopped := <-shutdown
	err = errors.Join(err, stopped.err)
	remainingShutdown := ProcessShutdownTimeout - time.Since(stopped.started)
	var teardownTimeout *iterationTeardownTimeoutError
	if errors.As(err, &teardownTimeout) {
		remainingShutdown = min(remainingShutdown, ProcessShutdownTimeout-ShutdownTimeout)
	}
	serverErr := infra.waitForPersistentServers(remainingShutdown)
	if ctx.Err() != nil {
		return errors.Join(stopped.err, serverErr)
	}
	if err != nil {
		return errors.Join(err, serverErr)
	}
	return serverErr
}

// iterationSequence hands leadership from one iteration to the next on the
// same replica. The serving iteration keeps rendering and deploying while its
// successor starts; the successor retires the predecessor's term right before
// entering election, resumes the kept Lease, and only then is the predecessor
// torn down. The fleet sees no vacancy and the new leader's first render is
// warm.
type iterationSequence struct {
	logger  *slog.Logger
	current *liveIteration
	startup *configchange.ReloadRequest
}

// step starts the next iteration against the current one and then waits for
// the reload that ends the new one's term. A failed start leaves the current
// iteration serving unless its leadership was already handed over, and the
// retry loads live state rather than the consumed hand-off.
func (s *iterationSequence) step(
	start func(startup *configchange.ReloadRequest, previous *liveIteration) (*liveIteration, error),
) error {
	next, err := start(s.startup, s.current)
	s.startup = nil
	if err != nil {
		var interrupted *startupInterruptedError
		if errors.As(err, &interrupted) {
			s.startup = interrupted.reload
			err = nil
		}
		if s.current != nil && s.current.retired {
			// Also on an interrupting reload: the restart starts cold with no
			// predecessor, so a kept Lease would leave no replica leading.
			s.current.releaseKeptLease()
			err = errors.Join(err, s.close())
		}
		return err
	}
	if err := s.close(); err != nil {
		return errors.Join(err, next.teardown())
	}
	s.current = next
	reload, err := next.awaitReload()
	s.startup = reload
	if reload == nil {
		err = errors.Join(err, s.close())
	}
	return err
}

// close tears the current iteration down, if any. A teardown that times out
// is reported: the process restarts rather than run with leaked goroutines.
func (s *iterationSequence) close() error {
	if s.current == nil {
		return nil
	}
	err := s.current.teardown()
	s.current = nil
	return err
}

func runIterations(ctx context.Context, logger *slog.Logger, retryDelay time.Duration, run func() error) error {
	for {
		if ctx.Err() != nil {
			logger.Info("Controller shutting down", "reason", ctx.Err())
			return nil
		}

		err := run()
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			logger.Info("Controller shutting down during iteration", "reason", ctx.Err())
			return nil
		}
		var teardownTimeout *iterationTeardownTimeoutError
		if errors.As(err, &teardownTimeout) {
			return err
		}
		var webhookServerFailure *persistentWebhookServerError
		if errors.As(err, &webhookServerFailure) {
			return err
		}

		logger.Error("Controller iteration failed, retrying",
			"error", err,
			"retry_delay", retryDelay)
		timer := time.NewTimer(retryDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			logger.Info("Controller shutting down during retry", "reason", ctx.Err())
			return nil
		}
	}
}

func listenerPortFromEnv(envName string, defaultPort int, allowDisabled bool) (int, error) {
	raw := os.Getenv(envName)
	if raw == "" {
		return defaultPort, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parsing %s=%q as a TCP port: %w", envName, raw, err)
	}
	minimum := 1
	if allowDisabled {
		minimum = 0
	}
	if port < minimum || port > 65535 {
		return 0, fmt.Errorf("%s must be between %d and 65535", envName, minimum)
	}
	return port, nil
}

// loadGateTimeoutFromEnv returns zero when LOAD_GATE_TIMEOUT is unset.
func loadGateTimeoutFromEnv() (time.Duration, error) {
	raw := os.Getenv("LOAD_GATE_TIMEOUT")
	if raw == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parsing LOAD_GATE_TIMEOUT=%q as a duration: %w", raw, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("LOAD_GATE_TIMEOUT must be positive, got %s", raw)
	}
	return value, nil
}

// componentSetup contains all resources created during component initialization.
type componentSetup struct {
	AgentTLS              *transportsecurity.Source
	Bus                   *busevents.EventBus
	Registry              *lifecycle.Registry // Component lifecycle registry
	MetricsComponent      *metrics.Component
	MetricsRegistry       *prometheus.Registry
	IntrospectionRegistry *introspection.Registry
	IntrospectionServer   *introspection.Server             // Server reference for custom handler registration
	ConfigChangeHandler   *configchange.ConfigChangeHandler // For setting initial config version
	IterCtx               context.Context
	Cancel                context.CancelFunc
	CancelCause           context.CancelCauseFunc
	ConfigChangeCh        chan *configchange.ReloadRequest
	ErrGroup              *errgroup.Group // Tracks all background goroutines for graceful shutdown
	LeaderState           *leaderCallbackState

	// Election is the running election loop, stoppable on its own so a
	// hand-over can end this iteration's term before the rest of it stops.
	// Nil when leader election is disabled.
	Election *electionRun

	// LeaderEpoch is the fencing epoch the deployer stamps on every apply, plus
	// the way to hand leadership back. It is built with the deploy stack and
	// claimed by the leader-election component, so both halves must see the
	// same object. Nil when leader election is disabled.
	LeaderEpoch *leaderelectionctrl.Term

	// ElectionRestart asks superviseElection to cancel the current election
	// attempt: the Lease is released (ReleaseOnCancel) and election re-enters
	// in place. Buffered so StandDown never blocks; created with the fence.
	ElectionRestart chan struct{}

	// SelfWrites links the status applier (writer) to the resource watchers
	// (readers) so a status write's own echo doesn't re-render.
	SelfWrites *k8stypes.SelfWriteRegistry

	// cleanups holds tear-down callbacks registered by helpers
	// during setup. RunCleanups invokes them in reverse-registration
	// order on iteration exit (mirrors `defer` semantics) so e.g. a
	// pluggable-validator manager's connection pools get drained
	// before the iteration context goes away. Mutex-guarded because
	// registration may happen from multiple stages of setup.
	cleanupsMu sync.Mutex
	cleanups   []func()
}

// AddCleanup registers a callback to run on iteration teardown.
// Callbacks fire in reverse-registration order (LIFO), mirroring
// `defer` semantics. Safe for concurrent registration.
func (s *componentSetup) AddCleanup(fn func()) {
	if fn == nil {
		return
	}
	s.cleanupsMu.Lock()
	s.cleanups = append(s.cleanups, fn)
	s.cleanupsMu.Unlock()
}

// RunCleanups invokes registered cleanup callbacks in reverse order.
// Idempotent — calling a second time is a no-op (cleanups have
// already drained).
func (s *componentSetup) RunCleanups() {
	s.cleanupsMu.Lock()
	cleanups := s.cleanups
	s.cleanups = nil
	s.cleanupsMu.Unlock()
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
}

// setupComponents creates and starts all event-driven components.
// The introspectionRegistry is passed in from the persistent infrastructure
// to avoid recreating it on each iteration (which would require rebinding the port).
//
// typeBootstrapper is the closure the Stage-1 TemplateValidator
// uses to resolve real reflect.Types for the request's
// watchedResources during config validation. Production passes a
// closure around runTypeBootstrap with the iteration's K8s
// client; the closure must be non-nil. See
// pkg/controller/validator.TypeBootstrapper for the contract.
func setupComponents(
	ctx context.Context,
	introspectionRegistry *introspection.Registry,
	eventDropMetrics *persistentEventDropMetrics,
	previousMetrics *metrics.Metrics,
	typeBootstrapper validator.TypeBootstrapper,
	crdName string,
	logger *slog.Logger,
) *componentSetup {
	logger.Info("Stage 1: Creating config management components")

	// Create EventBus with buffer for pre-start events
	bus := busevents.NewEventBus(100)

	// Create Prometheus registry for this iteration (instance-based, not global)
	registry := prometheus.NewRegistry()

	// Create metrics collector
	domainMetrics := metrics.NewMetrics(registry)
	domainMetrics.SetBuildInfo(buildVersionInfo.version, buildVersionInfo.haproxyVersion, buildVersionInfo.goVersion)
	eventDropMetrics.Attach(domainMetrics)
	if err := domainMetrics.CarryConfigValidation(previousMetrics); err != nil {
		logger.Warn("Config validation metrics restart from zero in this iteration", "error", err)
	}
	metricsComponent := metrics.New(domainMetrics, bus)

	iterCtx, cancelCause := context.WithCancelCause(ctx)
	cancel := func() { cancelCause(nil) }
	g, gCtx := errgroup.WithContext(iterCtx)

	bus.SetDropCallback(newCriticalEventDropCallback(logger, eventDropMetrics.Record, cancelCause))

	// Create components
	eventCommentator := commentator.NewEventCommentator(bus, logger, 500)
	configLoaderComponent := configloader.NewConfigLoaderComponent(bus, crdName, logger)
	credentialsLoaderComponent := credentialsloader.NewCredentialsLoaderComponent(bus, logger)

	// Create config validators (scatter-gather responders for HAProxyTemplateConfig CRD validation)
	basicValidator := validator.NewBasicValidator(bus, logger)
	templateValidator := validator.NewTemplateValidator(bus, logger, typeBootstrapper)
	jsonpathValidator := validator.NewJSONPathValidator(bus, logger)
	// Runs the config's embedded validationTests before the config is accepted,
	// so the daemon never loads (at startup or on a live change) a config whose
	// tests fail — matching the guarantee of the `controller validate` CLI.
	validationTestsValidator := validator.NewValidationTestsValidator(bus, logger, typeBootstrapper)

	// Create config change channel for reinitialization signaling
	configChangeCh := make(chan *configchange.ReloadRequest, 1)

	// Register validators for scatter-gather validation
	validators := validator.AllValidatorNames()

	configChangeHandlerComponent := configchange.NewConfigChangeHandler(
		bus,
		logger,
		configChangeCh,
		validators,
		0, // Use default debounce interval (5s)
	)

	// Start components in errgroup (these return nil on graceful shutdown)
	startInErrGroup(g, gCtx, logger, cancelCause, "event commentator", eventCommentator.Start)
	startInErrGroup(g, gCtx, logger, cancelCause, "config loader", configLoaderComponent.Start)
	startInErrGroup(g, gCtx, logger, cancelCause, "credentials loader", credentialsLoaderComponent.Start)
	startInErrGroup(g, gCtx, logger, cancelCause, "basic validator", basicValidator.Start)
	startInErrGroup(g, gCtx, logger, cancelCause, "template validator", templateValidator.Start)
	startInErrGroup(g, gCtx, logger, cancelCause, "jsonpath validator", jsonpathValidator.Start)
	startInErrGroup(g, gCtx, logger, cancelCause, "validationtests validator", validationTestsValidator.Start)
	startInErrGroup(g, gCtx, logger, cancelCause, "config change handler", configChangeHandlerComponent.Start)

	logger.Debug("All components started")

	// Note: introspection registry is passed in from persistent infrastructure
	// to avoid port rebinding issues during rapid reinitializations

	// Create lifecycle registry for managing reconciliation components
	lifecycleRegistry := lifecycle.NewRegistry().WithLogger(logger)

	return &componentSetup{
		Bus:                   bus,
		Registry:              lifecycleRegistry,
		SelfWrites:            k8stypes.NewSelfWriteRegistry(0),
		MetricsComponent:      metricsComponent,
		MetricsRegistry:       registry,
		IntrospectionRegistry: introspectionRegistry,
		ConfigChangeHandler:   configChangeHandlerComponent,
		IterCtx:               gCtx, // Use errgroup context so cancellation propagates
		Cancel:                cancel,
		CancelCause:           cancelCause,
		ConfigChangeCh:        configChangeCh,
		ErrGroup:              g,
	}
}
