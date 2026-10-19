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

// Package server is the HAPTIC agent: an HTTP endpoint that owns one HAProxy
// pod's file tree and its runtime socket. It executes what the controller
// decided and reports truthfully; it makes no HAProxy decisions of its own.
package server

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/cli"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/files"
	"github.com/haproxy-haptic/HAPTIC/pkg/transportsecurity"
)

// Timeouts of the HTTP endpoint. The read timeout has to cover a full config
// bundle on a slow link, which is why it is far above the write timeout.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 2 * time.Minute
	writeTimeout      = 3 * time.Minute
	idleTimeout       = 2 * time.Minute
	shutdownGrace     = 15 * time.Second
	startupProbeTick  = 200 * time.Millisecond
)

// Config is everything the agent needs to run.
type Config struct {
	BaseDir           string
	ConfigFile        string
	MasterSocket      string
	WorkerSocket      string
	StateFile         string
	Listen            string
	ReloadIntervalMin time.Duration
	// ReloadTimeout bounds one reload. Zero means DefaultReloadTimeout.
	ReloadTimeout time.Duration
	Username      string
	Password      string
	TLS           *transportsecurity.Source
	AgentVersion  string
	Logger        *slog.Logger
	Registry      *prometheus.Registry
	// DrainSocket is the unix socket path of GET /drain; empty disables it.
	DrainSocket string
	// AdminSocket exposes read-only state through the pod's filesystem permissions.
	AdminSocket string
	// DrainQuietPeriod ends the drain once no traffic frontend accepted a new
	// connection for this long; DrainMaxWait bounds the whole drain.
	DrainQuietPeriod time.Duration
	DrainMaxWait     time.Duration
	// DrainIgnoreFrontends names frontends whose connections are not traffic
	// (probes, scrapes); the chart passes its status frontend.
	DrainIgnoreFrontends []string
}

// Server owns the tree, the runtime plumbing and the apply state machine.
type Server struct {
	cfg       Config
	logger    *slog.Logger
	store     *files.Store
	runtime   *cli.Client
	deferrals *cli.Deferrals
	metrics   *Metrics
	states    *stateStore

	// apply serialises the state machine and every re-observation of disk:
	// at most one apply is ever in flight.
	apply sync.Mutex
	// A verified read must not adopt the worker an owned reload is still adopting.
	workerObservation sync.Mutex
	// mu guards the fields below, which /v1/state reads while an apply runs.
	mu         sync.Mutex
	state      *persistentState
	tree       map[string]api.FileAt
	treePrints map[string]files.Fingerprint // per tree path, the file its proof was issued for
	inventory  api.Inventory
	worker     api.HAProxyInfo
	lastReload time.Time
	reloadWake chan struct{}
	// reportedInventory is the inventory generation the last ACK carried, so a
	// delta rides an apply exactly once.
	reportedInventory uint64
	// baselineInvalidations counts how often the running worker became
	// unexplained, so an apply can tell whether one happened underneath it.
	baselineInvalidations uint64

	// drainCounter reads the traffic frontends' accepted-connection total;
	// drainPoll is how often the drain re-reads it. Both containers' preStop
	// hooks call /drain at the same time, so drainFlight shares one run and
	// drainStop ends it when the agent shuts down.
	drainCounter func(ignore map[string]bool) (uint64, error)
	drainPoll    time.Duration
	drainFlight  singleflight.Group
	drainStop    chan struct{}
	// appliedPlan is the opaque blob of the plan the pod applied; the state
	// file names the plan it belongs to, so a stale one is never handed out.
	appliedPlan []byte

	initialized chan struct{}
	addr        atomic.Pointer[string]
	http        *http.Server

	// background tracks the read-backs that outlive their apply handler, so
	// Start returns only after the last one is done and stopped is what
	// tells them the agent is going away.
	background sync.WaitGroup
	stopped    atomic.Bool
}

// New builds the agent. It probes the mounts under the base directory and
// loads the state file, but it does not touch HAProxy: Start does that, and
// readiness is what reports it.
func New(ctx context.Context, cfg *Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Registry == nil {
		cfg.Registry = prometheus.NewRegistry()
	}
	if err := files.ValidatePath(cfg.ConfigFile); err != nil {
		return nil, fmt.Errorf("--config: %w", err)
	}
	maxInterval := api.MaxReloadIntervalMs * time.Millisecond
	if cfg.ReloadIntervalMin < 0 || cfg.ReloadIntervalMin > maxInterval {
		return nil, fmt.Errorf("--reload-interval-min %s is outside 0..%s", cfg.ReloadIntervalMin, maxInterval)
	}
	if cfg.ReloadTimeout == 0 {
		cfg.ReloadTimeout = DefaultReloadTimeout
	}
	if cfg.ReloadTimeout < 0 || cfg.ReloadTimeout > DefaultReloadTimeout {
		return nil, fmt.Errorf("--reload-timeout must be between 0 and %s, got %s", DefaultReloadTimeout, cfg.ReloadTimeout)
	}
	if err := validateDrain(cfg); err != nil {
		return nil, err
	}
	if err := validateLocalSocketPaths(cfg); err != nil {
		return nil, err
	}
	// Local sockets are reserved from file deployment and stray-file cleanup.
	reserved := []string{cfg.MasterSocket, cfg.WorkerSocket}
	if cfg.DrainSocket != "" {
		reserved = append(reserved, cfg.DrainSocket)
	}
	if cfg.AdminSocket != "" {
		reserved = append(reserved, cfg.AdminSocket)
	}
	store, err := files.NewStore(cfg.BaseDir, cfg.Logger, reserved...)
	if err != nil {
		return nil, err
	}
	runtimeClient, err := cli.New(ctx, cli.Config{
		WorkerSocket: cfg.WorkerSocket,
		MasterSocket: cfg.MasterSocket,
		Logger:       cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	metrics := NewMetrics(cfg.Registry, cfg.Logger)
	s := &Server{
		cfg:         *cfg,
		logger:      cfg.Logger,
		store:       store,
		runtime:     runtimeClient,
		deferrals:   cli.NewDeferrals(runtimeClient, cfg.Logger, metrics),
		metrics:     metrics,
		states:      newStateStore(store.BaseDir(), cfg.StateFile),
		reloadWake:  make(chan struct{}, 1),
		drainPoll:   drainPollInterval,
		drainStop:   make(chan struct{}),
		initialized: make(chan struct{}),
	}
	s.drainCounter = runtimeClient.FrontendConnections
	if s.state, err = s.states.load(); err != nil {
		return nil, err
	}
	s.loadPlanBlob()
	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	return s, nil
}

// routes wires the API. Only the two probes are unauthenticated: kubelet
// cannot present credentials without putting them in the pod spec, and neither
// probe reveals anything about the configuration.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathHealthz, s.handleHealthz)
	mux.HandleFunc("GET "+api.PathReadyz, s.handleReadyz)
	mux.Handle("GET "+api.PathState, s.authenticated(s.handleState))
	mux.Handle("POST "+api.PathApply, s.authenticated(s.handleApply))
	mux.Handle("PUT "+api.PathPlan, s.authenticated(s.handlePlan))
	return mux
}

// Start serves the API until ctx ends. Startup initialisation runs first and
// is what flips readiness; a failure there is retried, never fatal, because
// HAProxy may simply be slower to come up than the agent.
func (s *Server) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Listen, err)
	}
	if s.cfg.TLS != nil {
		listener = tls.NewListener(listener, s.cfg.TLS.ServerConfig())
	}
	bound := listener.Addr().String()
	s.addr.Store(&bound)
	// Bind the local sockets before any goroutine can flip readiness, so a ready
	// agent always has them.
	drain, admin, err := s.listenLocalSockets()
	if err != nil {
		_ = listener.Close()
		return err
	}

	group, groupCtx := errgroup.WithContext(ctx)
	s.http.BaseContext = func(net.Listener) context.Context { return groupCtx }
	group.Go(func() error { return s.deferrals.Start(groupCtx) })
	group.Go(func() error { return s.pacer(groupCtx) })
	group.Go(func() error { return s.initialise(groupCtx) })
	if drain != nil {
		group.Go(func() error { return s.serveDrain(groupCtx, drain) })
	}
	if admin != nil {
		group.Go(func() error { return s.serveAdmin(groupCtx, admin) })
	}
	group.Go(func() error {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	group.Go(func() error {
		<-groupCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	})
	err = group.Wait()
	s.stopped.Store(true)
	s.background.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// listenLocalSockets binds the configured drain and admin sockets; a nil
// listener means that socket isn't configured.
func (s *Server) listenLocalSockets() (drain, admin net.Listener, err error) {
	if s.cfg.DrainSocket != "" {
		if drain, err = listenUnix(s.cfg.DrainSocket); err != nil {
			return nil, nil, err
		}
	}
	if s.cfg.AdminSocket != "" {
		if admin, err = listenUnix(s.cfg.AdminSocket); err != nil {
			if drain != nil {
				_ = drain.Close()
			}
			return nil, nil, err
		}
	}
	return drain, admin, nil
}

// Addr is the address the endpoint bound to, which is the resolved port when
// the configured address asked for any.
func (s *Server) Addr() string {
	if bound := s.addr.Load(); bound != nil {
		return *bound
	}
	return s.cfg.Listen
}

// Ready reports whether startup initialisation finished.
func (s *Server) Ready() bool {
	select {
	case <-s.initialized:
		return true
	default:
		return false
	}
}

// initialise runs the startup sequence readiness reports: both sockets answer,
// the tree is hashed against the state file, crash recovery has run and the
// inventory is built.
func (s *Server) initialise(ctx context.Context) error {
	if err := s.store.SweepTemp(); err != nil {
		s.logger.Warn("could not clear stale temp files", "error", err)
	}
	for {
		err := s.probeHAProxy()
		if err == nil {
			break
		}
		s.logger.Info("waiting for HAProxy", "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(startupProbeTick):
		}
	}
	if err := s.adoptDisk(); err != nil {
		return err
	}
	if err := s.recoverFromCrash(); err != nil {
		return err
	}
	close(s.initialized)
	s.logger.Info("agent ready",
		"applied_plan_id", s.snapshot().AppliedPlanID,
		"worker_pid", s.workerIdentity().WorkerPID)
	return nil
}

// probeHAProxy is the readiness gate: the worker answers `show info` and the
// master answers `show proc`.
func (s *Server) probeHAProxy() error {
	info, err := s.runtime.Info()
	if err != nil {
		return err
	}
	if _, err := s.runtime.ShowProc(); err != nil {
		return err
	}
	inventory, err := s.runtime.Inventory(1)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.worker = info
	s.deferrals.SetWorker(info)
	s.inventory = inventory
	return nil
}

// adoptDisk compares the tree with the state file. A mismatch means something
// other than the agent wrote the tree — a container restart that copied the
// bootstrap config, most often — so the baseline is unknown and the next apply
// is a full reload.
func (s *Server) adoptDisk() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tree, err := s.store.HashTree(s.state.ManifestPaths)
	if err != nil {
		return err
	}
	digest := treeDigest(tree)
	if s.state.TreeDigest != "" && s.state.TreeDigest != digest {
		s.logger.Warn("the file tree changed while the agent was away; baseline unknown",
			"expected", s.state.TreeDigest, "observed", digest)
		s.state.AppliedPlanID = ""
		s.state.RunningPlanID = ""
		s.state.WorkerOpsPlanID = ""
	}
	// Proofs attest observations made by this agent process. The state file's
	// digest is only an integrity hint, so a restart cannot carry those proofs
	// across the exact-observation boundary.
	s.state.AppliedPlanProof = ""
	s.state.RunningPlanProof = ""
	s.state.WorkerOpsPlanProof = ""
	s.state.LKGPlanProof = ""
	s.state.PendingReloadPlanID = ""
	s.state.PendingReloadPlanProof = ""
	s.state.ReloadPendingAt = time.Time{}
	s.state.PlanBlobPlanProof = ""
	s.state.NACK = nil
	s.tree = tree
	s.state.TreeDigest = digest
	s.state.ExpectedWorker = s.worker
	return s.states.save(s.state)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, "ok")
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.Ready() {
		writeText(w, http.StatusServiceUnavailable, "initialising")
		return
	}
	writeText(w, http.StatusOK, "ready")
}

// TLS mode requires the controller certificate; plaintext mode uses Basic auth.
func (s *Server) authenticated(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.TLS != nil {
			if err := s.cfg.TLS.VerifyClient(r.TLS); err != nil {
				writeText(w, http.StatusUnauthorized, "client certificate rejected")
				return
			}
			next(w, r)
			return
		}
		user, password, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.cfg.Username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.Password)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="haptic-agent"`)
			writeText(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	})
}

// snapshot copies the persisted state for readers that must not hold the lock.
func (s *Server) snapshot() persistentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.state
}

func (s *Server) workerIdentity() api.HAProxyInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.worker
}
