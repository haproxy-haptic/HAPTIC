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

package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/logging"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/server"
	"github.com/haproxy-haptic/HAPTIC/pkg/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/transportsecurity"
)

// Credentials come from the Secret the HAProxy pod already mounts.
const (
	agentUsernameEnv = "DATAPLANE_USERNAME"
	agentPasswordEnv = "DATAPLANE_PASSWORD" // #nosec G101 -- environment variable name, not a credential
	agentLogLevelEnv = "LOG_LEVEL"
)

var (
	agentBaseDir           string
	agentConfigFile        string
	agentMasterSocket      string
	agentWorkerSocket      string
	agentListen            string
	agentMetricsListen     string
	agentDrainSocket       string
	agentAdminSocket       string
	agentDrainQuietPeriod  time.Duration
	agentDrainMaxWait      time.Duration
	agentDrainIgnore       []string
	agentStateFile         string
	agentReloadIntervalMin time.Duration
	agentReloadTimeout     time.Duration
	agentTLSDirectory      string
	agentTLSClientName     string
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Run the HAPTIC agent inside an HAProxy pod",
	Long: `Run the HAPTIC agent.

The agent owns one HAProxy pod's file tree and its runtime socket. The
controller sends it the complete desired file set plus the typed runtime
commands it composed for this pod; the agent writes the files transactionally,
runs the commands or reloads, and reports what happened.

It makes no HAProxy decisions of its own: an op it cannot execute, a baseline
it does not recognise and a command HAProxy rejects all fall back to reloading
the file set on disk.

Example usage:
  # Run with the chart's defaults
  haptic agent

  # Run against a different tree
  haptic agent --base-dir /etc/haproxy --listen :5555`,
	RunE: runAgent,
}

func init() {
	agentCmd.PersistentFlags().StringVar(&agentTLSDirectory, "tls-dir", "",
		"Directory containing tls.crt, tls.key, and ca.crt (env: AGENT_TLS_DIR)")
	agentCmd.Flags().StringVar(&agentTLSClientName, "tls-client-name", "",
		"Required controller certificate DNS SAN (env: AGENT_TLS_CLIENT_NAME)")
	agentCmd.PersistentFlags().StringVar(&agentBaseDir, "base-dir", "/etc/haproxy",
		"Directory the agent owns; every manifest path is relative to it")
	agentCmd.Flags().StringVar(&agentConfigFile, "config", "haproxy.cfg",
		"Manifest path of the HAProxy configuration, which is always written last")
	agentCmd.Flags().StringVar(&agentMasterSocket, "master-socket", "haproxy-master.sock",
		"Master CLI socket, used only for reload and show proc (relative to --base-dir unless absolute)")
	agentCmd.Flags().StringVar(&agentWorkerSocket, "worker-socket", "haproxy-worker.sock",
		"Worker stats socket that carries every runtime command (relative to --base-dir unless absolute)")
	agentCmd.PersistentFlags().StringVar(&agentListen, "listen", ":5555",
		"Address the apply and state API listens on")
	agentCmd.Flags().StringVar(&agentMetricsListen, "metrics-listen", ":9101",
		"Address the Prometheus endpoint listens on; empty disables it")
	agentCmd.PersistentFlags().StringVar(&agentAdminSocket, "admin-socket", "haptic-agent.sock",
		"Read-only local state and health socket (relative to --base-dir); empty disables it")
	agentCmd.Flags().StringVar(&agentDrainSocket, "drain-socket", "haptic-drain.sock",
		"Unix socket (relative to --base-dir) serving GET /drain for the pod's preStop hook; empty disables it")
	agentCmd.Flags().DurationVar(&agentDrainQuietPeriod, "drain-quiet-period", server.DefaultDrainQuietPeriod,
		"The drain ends once the traffic frontends accepted no new connection for this long")
	agentCmd.Flags().DurationVar(&agentDrainMaxWait, "drain-max-wait", server.DefaultDrainMaxWait,
		"Upper bound of one drain; keep it inside the pod's termination grace period together with hard-stop-after")
	agentCmd.Flags().StringSliceVar(&agentDrainIgnore, "drain-ignore-frontend", nil,
		"Frontends whose connections do not count as traffic for the drain (probes, scrapes)")
	agentCmd.Flags().StringVar(&agentStateFile, "state-file", ".haptic-agent.json",
		"Name of the agent's state file inside --base-dir")
	agentCmd.Flags().DurationVar(&agentReloadIntervalMin, "reload-interval-min", 5*time.Second,
		"Shortest interval between two reloads, at most 60s; a reload inside the window is scheduled, never dropped")
	agentCmd.Flags().DurationVar(&agentReloadTimeout, "reload-timeout", server.DefaultReloadTimeout,
		"How long a reload may take before the apply reports what it knows; capped at the API's reload limit")
}

func runAgent(_ *cobra.Command, _ []string) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logging.ParseLogLevel(os.Getenv(agentLogLevelEnv)),
	}))
	// Everything this process writes is the JSON the chart promises: the metrics
	// server and net/http take their logger from the default one.
	slog.SetDefault(logger)
	configureMemoryLimit(logger)

	username, password := os.Getenv(agentUsernameEnv), os.Getenv(agentPasswordEnv)
	directory := cmp.Or(agentTLSDirectory, os.Getenv("AGENT_TLS_DIR"))
	peerName := cmp.Or(agentTLSClientName, os.Getenv("AGENT_TLS_CLIENT_NAME"))
	var tlsSource *transportsecurity.Source
	if directory != "" || peerName != "" {
		var err error
		tlsSource, err = transportsecurity.NewSource(directory, peerName)
		if err != nil {
			return err
		}
	}
	if tlsSource == nil && (username == "" || password == "") {
		return errors.New("the agent needs " + agentUsernameEnv + " and " + agentPasswordEnv + " from the credentials Secret")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	registry := prometheus.NewRegistry()
	agent, err := server.New(ctx, &server.Config{
		BaseDir:              agentBaseDir,
		ConfigFile:           agentConfigFile,
		MasterSocket:         resolveSocket(agentBaseDir, agentMasterSocket),
		WorkerSocket:         resolveSocket(agentBaseDir, agentWorkerSocket),
		StateFile:            agentStateFile,
		Listen:               agentListen,
		ReloadIntervalMin:    agentReloadIntervalMin,
		ReloadTimeout:        agentReloadTimeout,
		Username:             username,
		Password:             password,
		TLS:                  tlsSource,
		AgentVersion:         version,
		Logger:               logger,
		Registry:             registry,
		AdminSocket:          localSocketPath(agentBaseDir, agentAdminSocket),
		DrainSocket:          localSocketPath(agentBaseDir, agentDrainSocket),
		DrainQuietPeriod:     agentDrainQuietPeriod,
		DrainMaxWait:         agentDrainMaxWait,
		DrainIgnoreFrontends: agentDrainIgnore,
	})
	if err != nil {
		return err
	}

	logger.Info("starting the HAPTIC agent",
		"base_dir", agentBaseDir, "listen", agentListen, "reload_interval_min", agentReloadIntervalMin)

	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return agent.Start(groupCtx) })
	if agentMetricsListen != "" {
		group.Go(func() error { return metrics.NewServer(agentMetricsListen, registry).Start(groupCtx) })
	}
	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	logger.Info("the HAPTIC agent stopped")
	return nil
}

func localSocketPath(baseDir, socket string) string {
	if socket == "" {
		return ""
	}
	return resolveSocket(baseDir, socket)
}

func resolveSocket(baseDir, socket string) string {
	if filepath.IsAbs(socket) {
		return socket
	}
	return filepath.Join(baseDir, socket)
}
