package config

import "time"

// Default values for configuration fields.
const (
	// DefaultLevel is the default log level.
	// Empty string means use LOG_LEVEL env var or default to INFO.
	DefaultLevel = ""

	// DefaultDataplanePort is the default agent port on each HAProxy pod.
	DefaultDataplanePort = 5555

	// DefaultEnableValidationWebhook is the default webhook setting for resources.
	DefaultEnableValidationWebhook = false

	// DefaultMinDeploymentInterval is the default minimum time between consecutive deployments.
	DefaultMinDeploymentInterval = 2 * time.Second

	// DefaultDriftPreventionInterval is the default interval for periodic drift prevention deployments.
	DefaultDriftPreventionInterval = 60 * time.Second

	// DefaultDeploymentTimeout is the maximum time to wait for a deployment to complete.
	// If exceeded, the scheduler assumes the deployment was lost and retries.
	DefaultDeploymentTimeout = 30 * time.Second

	// DefaultDataplaneMapsDir is the default directory for HAProxy map files.
	DefaultDataplaneMapsDir = "/etc/haproxy/maps"

	// DefaultRenderGateInterval is the shortest start-to-start spacing of the
	// render gate's `haproxy -c` runs. At 3000 routes one check costs ~350 ms
	// of CPU, so anything below that would let a render storm keep a core busy
	// back-to-back and slow the admission webhook, whose 9 s failurePolicy:
	// Fail budget is the deadline that matters.
	DefaultRenderGateInterval = time.Second

	// DefaultConfigPublishInterval throttles HAProxyCfg CRD updates to coalesce
	// rapid renders during endpoint churn. Bounds the observable gap between
	// pod status.deployedToPods[].checksum and spec.checksum.
	DefaultConfigPublishInterval = 10 * time.Second

	// DefaultDataplaneSSLCertsDir is the Go-side fallback directory for SSL
	// certificates, used when neither the user nor the chart sets sslCertsDir.
	// The Helm chart explicitly sets sslCertsDir to /etc/haproxy/ssl in its
	// values.yaml, so chart-deployed controllers see /etc/haproxy/ssl rather
	// than this fallback. The two diverge intentionally: the chart's "ssl"
	// matches conventional HAProxy directory naming, while this constant
	// preserves the historical "certs" name for non-chart users (mostly
	// integration tests under tests/integration/).
	DefaultDataplaneSSLCertsDir = "/etc/haproxy/certs"

	// DefaultDataplaneGeneralStorageDir is the default directory for general files.
	DefaultDataplaneGeneralStorageDir = "/etc/haproxy/general"

	// DefaultDataplaneSecretsDir is the default directory for secret files.
	DefaultDataplaneSecretsDir = "/etc/haproxy/secrets"

	// DefaultDataplaneConfigFile is the default path to the main HAProxy config file.
	DefaultDataplaneConfigFile = "/etc/haproxy/haproxy.cfg"

	// DefaultLeaderElectionEnabled is the default leader election enabled setting.
	DefaultLeaderElectionEnabled = true

	// DefaultLeaderElectionLeaseName is the default name for the leader election lease.
	DefaultLeaderElectionLeaseName = "haptic-leader"

	// Leader-election timing defaults.
	//
	// These are deliberately 2x the client-go/kube-controller-manager
	// convention (15s/10s/2s). The renew deadline is the leader's budget for
	// riding out apiserver unavailability or CPU starvation without losing
	// the lease: on loaded nodes (CI runners, but also busy production
	// clusters) multi-second scheduler/apiserver stalls of 10s+ have been
	// observed (issue #57), and a renewal miss doesn't just cost leadership —
	// client-go's LeaderElector.Run returns permanently on a lost lease, so
	// the replica must go through a full controller reinitialization before
	// it can lead again (see pkg/controller/leader.go). 20s rides out any
	// stall shorter than 20s, with renewal attempts every 5s.
	//
	// The trade-off is hard-failover latency: when a leader dies WITHOUT
	// releasing the lease (node crash, OOM-kill), a standby waits up to
	// LeaseDuration (+ one RetryPeriod) ≈ 35s before taking over, vs ~17s at
	// the client-go convention. Voluntary handoffs (rolling update, graceful
	// shutdown, config-change reinitialization) are unaffected: the elector
	// runs with ReleaseOnCancel, so the lease is released immediately and a
	// standby can acquire it on its next retry. HA operators who prefer
	// faster crash-failover over starvation headroom can tune these down via
	// spec.controller.leaderElection.

	// DefaultLeaderElectionLeaseDuration is the default lease duration.
	DefaultLeaderElectionLeaseDuration = 30 * time.Second

	// DefaultLeaderElectionRenewDeadline is the default renew deadline.
	DefaultLeaderElectionRenewDeadline = 20 * time.Second

	// DefaultLeaderElectionRetryPeriod is the default retry period.
	DefaultLeaderElectionRetryPeriod = 5 * time.Second

	// DefaultReloadVerificationTimeout is the default maximum time the Dataplane
	// sync waits for a graceful HAProxy reload to be reported as completed.
	DefaultReloadVerificationTimeout = 10 * time.Second

	// DefaultSyncTimeout is the default overall timeout for one Dataplane sync
	// to a single HAProxy endpoint.
	DefaultSyncTimeout = 2 * time.Minute
)

// SetDefaults applies default values to unset configuration fields.
// This modifies the config in-place and should be called after parsing
// the configuration and before validation.
//
// Port Handling Strategy:
//   - A value of 0 for production ports (healthz, metrics, dataplane) means "uninitialized"
//     and will be replaced with the default value
//   - Debug ports may intentionally be 0 to indicate "disabled" (see cmd/haptic/main.go)
//   - After defaults are applied, production ports MUST NOT be 0 (validation will catch this)
func SetDefaults(cfg *Config) {
	// Leader election defaults
	// Note: Enabled defaults to true (zero value for bool is false, so we set it explicitly)
	if cfg.Controller.LeaderElection.LeaseName == "" {
		cfg.Controller.LeaderElection.LeaseName = DefaultLeaderElectionLeaseName
	}
	if cfg.Controller.LeaderElection.LeaseDuration == "" {
		cfg.Controller.LeaderElection.LeaseDuration = DefaultLeaderElectionLeaseDuration.String()
	}
	if cfg.Controller.LeaderElection.RenewDeadline == "" {
		cfg.Controller.LeaderElection.RenewDeadline = DefaultLeaderElectionRenewDeadline.String()
	}
	if cfg.Controller.LeaderElection.RetryPeriod == "" {
		cfg.Controller.LeaderElection.RetryPeriod = DefaultLeaderElectionRetryPeriod.String()
	}

	// Logging defaults
	// Note: Empty Level is valid (means use LOG_LEVEL env var or default to INFO)

	// Dataplane defaults
	// Note: This port should never remain 0 after defaults are applied
	if cfg.Dataplane.Port == 0 {
		cfg.Dataplane.Port = DefaultDataplanePort
	}

	// Apply dataplane path defaults
	if cfg.Dataplane.MapsDir == "" {
		cfg.Dataplane.MapsDir = DefaultDataplaneMapsDir
	}
	if cfg.Dataplane.SSLCertsDir == "" {
		cfg.Dataplane.SSLCertsDir = DefaultDataplaneSSLCertsDir
	}
	if cfg.Dataplane.GeneralStorageDir == "" {
		cfg.Dataplane.GeneralStorageDir = DefaultDataplaneGeneralStorageDir
	}
	if cfg.Dataplane.SecretsDir == "" {
		cfg.Dataplane.SecretsDir = DefaultDataplaneSecretsDir
	}
	if cfg.Dataplane.ConfigFile == "" {
		cfg.Dataplane.ConfigFile = DefaultDataplaneConfigFile
	}

	// Watched resources defaults
	// Note: EnableValidationWebhook defaults to false (zero value) which is correct
	// IndexBy must be explicitly configured, no default
}

// parseDurationOr returns the duration parsed from value, or fallback when
// value is empty or fails to parse. The "Get*" duration accessors below are
// thin wrappers around this helper so that the empty / invalid / default
// behaviour lives in exactly one place.
func parseDurationOr(value string, fallback time.Duration) time.Duration {
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return duration
}

// GetMinDeploymentInterval returns the configured minimum deployment interval
// or the default if not specified or invalid.
func (d *DataplaneConfig) GetMinDeploymentInterval() time.Duration {
	return parseDurationOr(d.MinDeploymentInterval, DefaultMinDeploymentInterval)
}

// GetDriftPreventionInterval returns the configured drift prevention interval
// or the default if not specified or invalid.
func (d *DataplaneConfig) GetDriftPreventionInterval() time.Duration {
	return parseDurationOr(d.DriftPreventionInterval, DefaultDriftPreventionInterval)
}

// GetDeploymentTimeout returns the configured deployment timeout
// or the default if not specified or invalid.
func (d *DataplaneConfig) GetDeploymentTimeout() time.Duration {
	return parseDurationOr(d.DeploymentTimeout, DefaultDeploymentTimeout)
}

// GetConfigPublishInterval returns the configured CRD publish throttle interval
// or the default if not specified or invalid.
func (d *DataplaneConfig) GetConfigPublishInterval() time.Duration {
	return parseDurationOr(d.ConfigPublishInterval, DefaultConfigPublishInterval)
}

// GetRenderGateInterval returns the configured render-gate duty-cycle interval
// or the default if not specified or invalid.
func (c *ControllerConfig) GetRenderGateInterval() time.Duration {
	return parseDurationOr(c.RenderGateInterval, DefaultRenderGateInterval)
}

// GetLeaseDuration returns the configured lease duration
// or the default if not specified or invalid.
func (le *LeaderElectionConfig) GetLeaseDuration() time.Duration {
	return parseDurationOr(le.LeaseDuration, DefaultLeaderElectionLeaseDuration)
}

// GetRenewDeadline returns the configured renew deadline
// or the default if not specified or invalid.
func (le *LeaderElectionConfig) GetRenewDeadline() time.Duration {
	return parseDurationOr(le.RenewDeadline, DefaultLeaderElectionRenewDeadline)
}

// GetRetryPeriod returns the configured retry period
// or the default if not specified or invalid.
func (le *LeaderElectionConfig) GetRetryPeriod() time.Duration {
	return parseDurationOr(le.RetryPeriod, DefaultLeaderElectionRetryPeriod)
}

// GetReloadVerificationTimeout returns the configured reload-verification
// timeout or the default if not specified or invalid.
func (d *DataplaneConfig) GetReloadVerificationTimeout() time.Duration {
	return parseDurationOr(d.ReloadVerificationTimeout, DefaultReloadVerificationTimeout)
}

// GetSyncTimeout returns the configured per-endpoint sync timeout or the
// default if not specified or invalid.
func (d *DataplaneConfig) GetSyncTimeout() time.Duration {
	return parseDurationOr(d.SyncTimeout, DefaultSyncTimeout)
}
