// Copyright 2026 Philipp Hossner
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

package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

// Drain defaults. A terminating pod keeps its listeners until kube-proxy has
// stopped routing new connections to it; the pod cannot observe kube-proxy,
// but it can observe the effect, so the hook ends after a quiet period and
// never later than the bound, which must stay inside the termination grace
// period together with hard-stop-after.
const (
	DefaultDrainQuietPeriod = 2 * time.Second
	DefaultDrainMaxWait     = 10 * time.Second
	MaxDrainWait            = 5 * time.Minute
	drainPollInterval       = 100 * time.Millisecond
)

// Drain reasons reported to the hook and the log.
const (
	DrainReasonQuiet       = "quiet"
	DrainReasonBound       = "bound"
	DrainReasonUnavailable = "unavailable"
	DrainReasonCancelled   = "cancelled"
)

// DrainResult is the answer of GET /drain on the drain socket.
type DrainResult struct {
	// Reason is why the drain ended: quiet, bound, unavailable (the worker no
	// longer answers, so there is nothing to keep open) or cancelled.
	Reason string `json:"reason"`
	// Waited is how long the caller was held.
	Waited string `json:"waited"`
	// Connections is the last accepted-connection count seen on the traffic frontends.
	Connections uint64 `json:"connections"`
}

// validateDrain fills the defaults and rejects a bound the grace period cannot hold.
func validateDrain(cfg *Config) error {
	if cfg.DrainQuietPeriod == 0 {
		cfg.DrainQuietPeriod = DefaultDrainQuietPeriod
	}
	if cfg.DrainMaxWait == 0 {
		cfg.DrainMaxWait = DefaultDrainMaxWait
	}
	if cfg.DrainQuietPeriod < 0 || cfg.DrainMaxWait < 0 || cfg.DrainMaxWait > MaxDrainWait {
		return fmt.Errorf("--drain-quiet-period %s and --drain-max-wait %s must be positive, the bound at most %s",
			cfg.DrainQuietPeriod, cfg.DrainMaxWait, MaxDrainWait)
	}
	if cfg.DrainQuietPeriod > cfg.DrainMaxWait {
		return fmt.Errorf("--drain-quiet-period %s exceeds --drain-max-wait %s", cfg.DrainQuietPeriod, cfg.DrainMaxWait)
	}
	return nil
}

// handleDrain holds the caller until the worker has seen no new connection for
// the quiet period or the bound elapsed. The chart's preStop hook calls it, so
// kubelet's stop signal reaches HAProxy only once nothing routes to the pod.
func (s *Server) handleDrain(w http.ResponseWriter, _ *http.Request) {
	shared, _, _ := s.drainFlight.Do("drain", func() (any, error) {
		result := s.drain(s.drainStop)
		s.logger.Info("drain finished", "reason", result.Reason, "waited", result.Waited, "connections", result.Connections)
		return result, nil
	})
	result, _ := shared.(DrainResult)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) drain(stop <-chan struct{}) DrainResult {
	start := time.Now()
	ignore := make(map[string]bool, len(s.cfg.DrainIgnoreFrontends))
	for _, name := range s.cfg.DrainIgnoreFrontends {
		ignore[name] = true
	}
	finish := func(reason string, connections uint64) DrainResult {
		return DrainResult{Reason: reason, Waited: time.Since(start).Round(time.Millisecond).String(), Connections: connections}
	}
	last, err := s.drainCounter(ignore)
	if err != nil {
		s.logger.Info("drain: the worker does not answer, nothing to hold open", "error", err)
		return finish(DrainReasonUnavailable, 0)
	}
	quietSince := time.Now()
	ticker := time.NewTicker(s.drainPoll)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return finish(DrainReasonCancelled, last)
		case <-ticker.C:
			current, err := s.drainCounter(ignore)
			if err != nil {
				s.logger.Info("drain: the worker stopped answering", "error", err)
				return finish(DrainReasonUnavailable, last)
			}
			now := time.Now()
			if current != last {
				last, quietSince = current, now
			}
			if now.Sub(quietSince) >= s.cfg.DrainQuietPeriod {
				return finish(DrainReasonQuiet, last)
			}
			if now.Sub(start) >= s.cfg.DrainMaxWait {
				return finish(DrainReasonBound, last)
			}
		}
	}
}

func (s *Server) serveDrain(ctx context.Context, listener net.Listener) error {
	defer close(s.drainStop)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathDrain, s.handleDrain)
	// Finish an active preStop drain before stopping the listener's handlers.
	return s.serveUnix(ctx, listener, mux, s.cfg.DrainMaxWait+shutdownGrace)
}
