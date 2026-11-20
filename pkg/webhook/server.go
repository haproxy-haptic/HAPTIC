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

package webhook

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

// Server is an HTTPS webhook server that validates Kubernetes resources.
//
// The server handles AdmissionReview requests from the Kubernetes API server
// and calls registered validation functions to determine whether resources
// should be admitted.
//
// The server is thread-safe and can handle multiple concurrent requests.
//
// The certificate is resolved per TLS handshake through getCertificate. When
// ServerConfig.CertDir is set, that callback reloads tls.crt/tls.key from disk
// on change, so a rotated certificate (cert-manager renewal written to a
// mounted Secret) is served without restarting. Otherwise getCertificate
// returns a fixed certificate parsed once from CertPEM/KeyPEM. Either way the
// certificate is validated eagerly in NewServer so a malformed cert surfaces
// there rather than at the first handshake.
type Server struct {
	config     ServerConfig
	validators map[string]ValidationFunc
	mu         sync.RWMutex
	// onUnregisteredGVK is seeded from ServerConfig and swappable via
	// SetOnUnregisteredGVK; guarded by mu alongside validators.
	onUnregisteredGVK func(gvk string)
	// boundAddr is the listener's actual address, resolved after net.Listen so
	// a Port of 0 (tests) can be discovered instead of guessed. Guarded by mu.
	boundAddr       string
	httpServer      *http.Server
	getCertificate  func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	generation      *ValidatorGeneration
	closed          bool
	activity        requestActivity
	shutdownOnce    sync.Once
	shutdownStarted chan struct{}
	shutdownDone    chan struct{}
	shutdownErr     error

	// listening is closed once the TLS listener has been bound to the
	// configured port. Callers that need to know the server is actually
	// accepting connections (e.g., an iteration sequencer that wants the
	// controller's readiness probe to wait for admission to be reachable)
	// can read from Listening() — until then connection attempts fail with
	// "connection refused" because Go's net.Listen hasn't returned yet.
	listening chan struct{}
}

// ValidatorGeneration is one installed validator table; the installer keeps
// it to retire only its own.
type ValidatorGeneration struct {
	// installed is false for the fail-closed placeholder a server starts with
	// and falls back to when a generation is retired.
	installed         bool
	validators        map[string]ValidationFunc
	onUnregisteredGVK func(gvk string)
	onRetired         func()
	inFlight          sync.WaitGroup
	retireOnce        sync.Once
}

func newValidatorGeneration(
	validators map[string]ValidationFunc,
	onUnregisteredGVK func(gvk string),
	onRetired func(),
) *ValidatorGeneration {
	return &ValidatorGeneration{
		validators:        validators,
		onUnregisteredGVK: onUnregisteredGVK,
		onRetired:         onRetired,
	}
}

func (g *ValidatorGeneration) retire() {
	if g == nil {
		return
	}
	g.retireOnce.Do(func() {
		g.inFlight.Wait()
		if g.onRetired != nil {
			g.onRetired()
		}
	})
}

// NewServer creates a new webhook server with the given configuration.
//
// The server will not start until Start() is called. The certificate is
// loaded eagerly — from CertDir (reloading, on change) or from CertPEM/KeyPEM
// (fixed) — so configuration errors surface here rather than at the first TLS
// handshake.
func NewServer(input *ServerConfig) (*Server, error) {
	if input == nil {
		return nil, errors.New("webhook server configuration is required")
	}
	config := *input
	// Apply defaults
	if config.Port == 0 {
		config.Port = 9443
	}
	if config.BindAddress == "" {
		config.BindAddress = "0.0.0.0"
	}
	if config.Path == "" {
		config.Path = "/validate"
	}
	if config.ReadTimeout == 0 {
		config.ReadTimeout = 10 * time.Second
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = 10 * time.Second
	}
	// Above the 90s client-go transport default, so the API server closes first.
	if config.IdleTimeout == 0 {
		config.IdleTimeout = 120 * time.Second
	}

	getCertificate, err := newGetCertificate(&config)
	if err != nil {
		return nil, err
	}

	generation := newValidatorGeneration(make(map[string]ValidationFunc), config.OnUnregisteredGVK, nil)
	return &Server{
		config:            config,
		validators:        generation.validators,
		onUnregisteredGVK: config.OnUnregisteredGVK,
		getCertificate:    getCertificate,
		generation:        generation,
		listening:         make(chan struct{}),
		shutdownStarted:   make(chan struct{}),
		shutdownDone:      make(chan struct{}),
	}, nil
}

// newGetCertificate builds the tls.Config GetCertificate callback: a reloading
// file source when CertDir is set, otherwise a fixed certificate parsed once
// from CertPEM/KeyPEM.
func newGetCertificate(config *ServerConfig) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	if config.CertDir != "" {
		reloader, err := newCertReloader(
			filepath.Join(config.CertDir, "tls.crt"),
			filepath.Join(config.CertDir, "tls.key"),
		)
		if err != nil {
			return nil, fmt.Errorf("loading webhook certificate from %s: %w", config.CertDir, err)
		}
		return reloader.GetCertificate, nil
	}

	cert, err := tls.X509KeyPair(config.CertPEM, config.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading initial TLS certificate: %w", err)
	}
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }, nil
}

// Listening returns a channel that is closed once the TLS listener has
// been bound to the configured port. Until this channel is closed,
// admission requests sent to the server's address fail with "connection
// refused". The controller uses this signal so its Pod readiness probe
// only flips healthy after the webhook is actually reachable.
func (s *Server) Listening() <-chan struct{} {
	return s.listening
}

// RegisterValidator registers a validation function for a specific resource type.
//
// The gvk parameter should be in the format "version.Kind" (e.g., "v1.Ingress").
// For resources with a group, use "group/version.Kind" (e.g., "networking.k8s.io/v1.Ingress").
//
// If a validator is already registered for this gvk, it will be replaced.
//
// This method is thread-safe.
func (s *Server) RegisterValidator(gvk string, fn ValidationFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validators[gvk] = fn
}

// Addr returns the address the listener actually bound, or "" before it has.
// Callers that configure Port 0 — tests, which must not fight over a fixed
// port — read the kernel-assigned port from here once Listening() has closed.
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.boundAddr
}

// SetOnUnregisteredGVK REPLACES the unregistered-GVK reporter.
//
// A server that outlives the wiring which built its validator table (the
// controller keeps one listener bound across config reinitializations, so an
// admission request never meets a closed port) must be able to re-point this
// callback at the current wiring's metrics recorder. It is read under the same
// lock as the validator table, so it swaps atomically with respect to in-flight
// requests.
func (s *Server) SetOnUnregisteredGVK(fn func(gvk string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onUnregisteredGVK = fn
	s.generation.onUnregisteredGVK = fn
}

// SetValidators atomically replaces the validator table.
func (s *Server) SetValidators(validators map[string]ValidationFunc) {
	s.mu.RLock()
	onUnregisteredGVK := s.onUnregisteredGVK
	s.mu.RUnlock()
	_ = s.ReplaceValidatorGeneration(validators, onUnregisteredGVK, nil)
}

// ReplaceValidatorGeneration installs one complete table and retires the old
// table after all requests that acquired it have returned.
func (s *Server) ReplaceValidatorGeneration(
	validators map[string]ValidationFunc,
	onUnregisteredGVK func(gvk string),
	onRetired func(),
) error {
	_, err := s.InstallValidatorGeneration(validators, onUnregisteredGVK, onRetired)
	return err
}

// InstallValidatorGeneration is ReplaceValidatorGeneration returning the
// installed generation, which the installer hands to
// RetireValidatorGenerationIfCurrent when it stops.
func (s *Server) InstallValidatorGeneration(
	validators map[string]ValidationFunc,
	onUnregisteredGVK func(gvk string),
	onRetired func(),
) (*ValidatorGeneration, error) {
	replacement := make(map[string]ValidationFunc, len(validators))
	maps.Copy(replacement, validators)
	next := newValidatorGeneration(replacement, onUnregisteredGVK, onRetired)
	next.installed = true

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("webhook server is closed")
	}
	previous := s.generation
	s.generation = next
	s.validators = next.validators
	s.onUnregisteredGVK = next.onUnregisteredGVK
	s.mu.Unlock()

	previous.retire()
	return next, nil
}

// Validating reports whether a caller-installed validator generation answers
// requests. A server without one denies every request.
func (s *Server) Validating() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.closed && s.generation.installed
}

// RetireValidatorGenerationIfCurrent empties the table only while generation
// is still the installed one. An iteration that stops after its successor
// installed the next generation must leave that one serving.
func (s *Server) RetireValidatorGenerationIfCurrent(generation *ValidatorGeneration) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("webhook server is closed")
	}
	if s.generation != generation {
		s.mu.Unlock()
		return nil
	}
	empty := newValidatorGeneration(make(map[string]ValidationFunc), nil, nil)
	s.generation = empty
	s.validators = empty.validators
	s.onUnregisteredGVK = nil
	s.mu.Unlock()

	generation.retire()
	return nil
}

func (s *Server) retireValidatorGeneration() {
	empty := newValidatorGeneration(make(map[string]ValidationFunc), nil, nil)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	previous := s.generation
	s.generation = empty
	s.validators = empty.validators
	s.onUnregisteredGVK = nil
	s.mu.Unlock()

	previous.retire()
}

// Start starts the HTTPS webhook server.
//
// The server binds to the configured port synchronously, closes the
// channel returned by Listening() to signal readiness, and then serves
// in a background goroutine. The method blocks until the server is shut
// down (context cancellation) or the serve loop returns an error.
//
// Splitting bind from serve matters because the Pod readiness probe
// must not flip healthy until admission is reachable — otherwise the
// API server starts routing AdmissionReview requests at the controller
// before net.Listen has returned, and every request bounces with
// "connection refused" until the listener finally binds. Callers that
// need to gate on the bind read Listening().
func (s *Server) Start(ctx context.Context) error {
	defer s.retireValidatorGeneration()

	mux := http.NewServeMux()
	mux.HandleFunc(s.config.Path, s.handleValidation)
	mux.HandleFunc(s.config.Path+"/peer", s.handlePeer)
	mux.HandleFunc("/healthz", s.handleHealthz)

	addr := fmt.Sprintf("%s:%d", s.config.BindAddress, s.config.Port)
	tlsConfig := &tls.Config{
		GetCertificate: s.getCertificate,
		ClientAuth:     tls.RequestClientCert,
		MinVersion:     tls.VersionTLS12,
	}

	httpServer := &http.Server{
		Addr:         addr,
		Handler:      mux,
		TLSConfig:    tlsConfig,
		ReadTimeout:  s.config.ReadTimeout,
		WriteTimeout: s.config.WriteTimeout,
		IdleTimeout:  s.config.IdleTimeout,
	}
	s.mu.Lock()
	s.httpServer = httpServer
	s.mu.Unlock()

	// Bind synchronously so callers can observe success before any
	// admission request is routed at us.
	tcpListener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	tlsListener := tls.NewListener(tcpListener, tlsConfig)
	s.mu.Lock()
	s.boundAddr = tcpListener.Addr().String()
	s.mu.Unlock()
	close(s.listening)

	serveDone := make(chan error, 1)
	go func() {
		err := httpServer.Serve(tlsListener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveDone <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		shutdownErr := s.Shutdown(shutdownCtx)
		cancel()
		return errors.Join(shutdownErr, <-serveDone)
	case err := <-serveDone:
		select {
		case <-s.shutdownStarted:
			<-s.shutdownDone
			return errors.Join(err, s.shutdownErr)
		default:
		}
		return err
	}
}

// handleHealthz handles health check requests.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
