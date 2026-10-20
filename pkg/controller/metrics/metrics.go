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

package metrics

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validator"
	pkgmetrics "github.com/haproxy-haptic/HAPTIC/pkg/metrics"
)

const (
	reasonLabel    = "reason"
	resultLabel    = "result"
	validatorLabel = "validator"
)

// configRejectedByCoordinator labels a rejection no single validator made: one timed out or didn't answer.
const configRejectedByCoordinator = "coordinator"

// Render cache states, the cache_state label of the render counters.
const (
	renderCold   = "cold"
	renderWarm   = "warm"
	renderReplay = "replay"
)

// Metrics holds all controller-specific Prometheus metrics.
//
// IMPORTANT: Create one instance per application iteration.
// When the iteration ends (e.g., on config reload), metrics are garbage collected.
// This prevents stale state from surviving across reinitialization cycles.
type Metrics struct {
	RetainedConfigActive prometheus.Gauge
	// Reconciliation metrics
	ReconciliationDuration prometheus.Histogram
	ReconciliationTotal    prometheus.Counter
	ReconciliationErrors   prometheus.Counter

	// Deployment metrics
	DeploymentDuration prometheus.Histogram
	DeploymentTotal    prometheus.Counter
	DeploymentErrors   prometheus.Counter

	// HAProxyReloadsTotal is populated from DeploymentCompletedEvent. Reloads
	// are the canonical capacity/SLO signal: a reload momentarily forks the
	// HAProxy process, so a high reload rate (vs runtime ops) is what to
	// capacity-plan and alert on.
	HAProxyReloadsTotal prometheus.Counter

	// Fleet-convergence + config-staleness gauges. Populated LEADER-ONLY from
	// DeploymentCompletedEvent (only the leader deploys), so followers hold the
	// reset values below and never false-alert. Unlike alerting on
	// rate(haptic_deployment_errors_total) — which the self-healing scheduler
	// makes noisy, since transient deploy failures now self-heal — these give a
	// true "is the fleet converged, and for how long has it not been" signal:
	//   - FleetSize / FleetConverged: an operator alerts on converged < size.
	//   - LastFullSyncTimestamp: staleness is time() - this gauge.
	//   - DeploymentConsecutiveFailures: the noise-free replacement for alerting
	//     on the error counter (resets to 0 the moment the fleet fully converges).
	HAProxyFleetSize              prometheus.Gauge
	HAProxyFleetConverged         prometheus.Gauge
	LastFullSyncTimestamp         prometheus.Gauge
	DeploymentConsecutiveFailures prometheus.Gauge

	// consecutiveFailures is the running count backing DeploymentConsecutiveFailures.
	// Touched only from the metrics component's single-goroutine event loop
	// (SetFleetConvergence / ResetFleetConvergence), so it needs no lock.
	consecutiveFailures int

	// Per-pod apply counters. AgentApplyTotal is labelled by the mode the agent
	// reported, so the reload-free share of a deployment is a query away;
	// ApplyRejectedTotal counts NACKs, each of which costs that pod a
	// full-state reload on the next apply.
	AgentApplyTotal    *prometheus.CounterVec
	ApplyRejectedTotal *prometheus.CounterVec

	// AgentVersionSkewTotal counts applies to pods whose agent reported a
	// contract this controller does not fully speak. Never a refusal, but
	// sustained growth means a rollout is stuck half-upgraded.
	AgentVersionSkewTotal prometheus.Counter

	// Runtime op counters, recorded from what the pods ACKed. Labelled by op
	// kind: which lifecycle operation dominates is the actionable part.
	RuntimeBackendOpsTotal *prometheus.CounterVec
	RuntimeServerOpsTotal  *prometheus.CounterVec

	// RuntimeBackendFallbackTotal counts runtime backend batches a pod could
	// not run and reloaded instead, by reason. A `name_collision` is a leftover
	// backend still holding the name a new one wants (A5): HAProxy exposes no
	// way to read a backend's shape, so the apply reloads rather than risk
	// publishing traffic onto the wrong section. Steady growth means deferred
	// backend deletes are lagging behind fresh routes reusing their names.
	RuntimeBackendFallbackTotal *prometheus.CounterVec

	// RuntimeMapDivergence counts runtime maps whose post-apply read-back
	// still disagreed with the desired content, each costing that sync its
	// reload-free lane (issue #48). Labelled by map because which one
	// degrades is the actionable part. The reload fallback is convergent, so
	// occasional counts during pod churn are expected; steady growth in
	// STEADY STATE means endpoint changes are reloading HAProxy, defeating
	// the runtime lane.
	RuntimeMapDivergence *prometheus.CounterVec

	// ValidationTotal and ValidationErrors count HAProxy's verdicts on rendered
	// configurations; the configuration itself is judged in ConfigValidation*.
	ValidationTotal  prometheus.Counter
	ValidationErrors prometheus.Counter

	// ConfigValidated is this replica's Validated condition: one series per
	// known status and reason, the current one at 1.
	ConfigValidated *prometheus.GaugeVec
	// ConfigValidationTotal counts this replica's verdicts on its configuration.
	ConfigValidationTotal *prometheus.CounterVec
	// configValidated is the state ConfigValidated reports; nil before the
	// first verdict. It is what CarryConfigValidation reads.
	configValidated atomic.Pointer[configValidatedState]

	// HAProxyChecksTotal counts the `haproxy -c` runs each validator asked for,
	// on every replica; HAProxyChecksReusedTotal counts the ones answered by an
	// identical input set's verdict instead of a process.
	HAProxyChecksTotal       *prometheus.CounterVec
	HAProxyChecksReusedTotal *prometheus.CounterVec

	// Resource metrics
	ResourceCount         *prometheus.GaugeVec
	RejectedWatchedInputs prometheus.Gauge

	// Event metrics
	EventSubscribers           prometheus.Gauge
	EventsPublished            prometheus.Counter
	EventsDroppedCritical      prometheus.Counter     // Drops from critical subscribers (alert-worthy)
	EventsDroppedBySubscriber  *prometheus.CounterVec // Drops by subscriber and event type
	EventsDroppedObservability prometheus.Gauge       // Drops from observability subscribers (polled, expected)

	// Queue wait metrics - time events spend waiting in channels before processing
	ReconciliationQueueWait prometheus.Histogram

	// Webhook metrics
	WebhookRequestsTotal    *prometheus.CounterVec
	WebhookRequestDuration  prometheus.Histogram
	WebhookValidationTotal  *prometheus.CounterVec
	AdmissionRenderSlotWait prometheus.Histogram

	// Leader election metrics
	LeaderElectionIsLeader            prometheus.Gauge
	LeaderElectionTransitionsTotal    prometheus.Counter
	LeaderElectionTimeAsLeaderSeconds prometheus.Counter

	// Discovery metrics — surfaces rejection of HAProxy pods that the
	// controller refuses to talk to (most commonly version-incompatible).
	// Without this counter, a misconfigured cluster (e.g., controller image
	// bundling HAProxy 3.3 while chart deploys 3.2) presents only as
	// "deployment.skipped" in the pipeline status, masking a real fault.
	HAProxyPodsRejectedTotal *prometheus.CounterVec

	// ConfigRejectedTotal counts HAProxyTemplateConfig loads refused by the
	// config-validation gate, labelled by the validator that rejected it
	// (basic / template / jsonpath / validationtests), or "coordinator" when the
	// scatter-gather itself failed — a validator timed out or didn't respond —
	// rather than a specific validator refusing the config. Persistent growth
	// means the controller is repeatedly refusing a new config and continuing to
	// serve the last-good one — the operator's config never took effect.
	ConfigRejectedTotal *prometheus.CounterVec

	// HTTPContentRevokedTotal counts accepted HTTP content taken back because
	// HAProxy refused a render containing it (ADR-0030).
	HTTPContentRevokedTotal prometheus.Counter

	// HTTPContentRejectedTotal counts pending content refused before acceptance.
	HTTPContentRejectedTotal prometheus.Counter

	// ConfigPinned is 1 while the render gate is holding renders it has
	// already refused once: nothing new reaches the fleet, which keeps serving
	// the last config HAProxy accepted, until the operator's input changes.
	ConfigPinned prometheus.Gauge

	// ControllerReinitializationsTotal counts iteration restarts after the
	// first. An iteration restart stops the leader-only components and rebuilds
	// them from a freshly resolved configuration, so nothing propagates
	// endpoint changes to HAProxy until it completes — measured at 54-65s on
	// the bundled chart, dominated by the load gate re-running validationTests.
	// Growth means something keeps changing the effective configuration, most
	// often a CRD appearing or disappearing.
	ControllerReinitializationsTotal prometheus.Counter

	// RenderProfiles is the number of distinct backend profiles the most recent
	// reconcile render emitted (`defaults haptic-be-*`). Profile cardinality is
	// what backends collapse onto, so it tracks the config's structural size
	// independently of the raw backend count.
	RenderProfiles prometheus.Gauge

	// RenderWarnings counts current template warnings by reason.
	RenderWarnings *prometheus.GaugeVec

	// RenderTotal counts reconcile renders by the cache state they ran from.
	// Every replica renders: the leader to deploy, a follower to keep its
	// incremental graph warm for the next leadership change.
	RenderTotal *prometheus.CounterVec

	// RenderErrors counts the reconcile renders in RenderTotal that produced no output.
	RenderErrors *prometheus.CounterVec

	// Build info metric
	BuildInfo *prometheus.GaugeVec
}

// New creates all controller metrics and registers them with the provided registry.
//
// IMPORTANT: Pass an instance-based registry (prometheus.NewRegistry()), NOT
// prometheus.DefaultRegisterer. Metrics are scoped to the registry's lifetime.
// When the registry is garbage collected (iteration ends), metrics are freed.
//
// This is critical for supporting application reinitialization on configuration
// changes without leaking metrics or accumulating stale state.
//
// Example:
//
//	registry := prometheus.NewRegistry()  // Create per iteration
//	metrics := metrics.NewMetrics(registry)  // Metrics tied to iteration
//	// ... use metrics ...
//	// When iteration ends, both registry and metrics are GC'd
func NewMetrics(registry prometheus.Registerer) *Metrics {
	m := &Metrics{
		// Reconciliation metrics
		ReconciliationDuration: pkgmetrics.NewHistogramWithBuckets(
			registry,
			"haptic_reconciliation_duration_seconds",
			"Time spent in reconciliation cycles",
			pkgmetrics.DurationBuckets(),
		),
		ReconciliationTotal: pkgmetrics.NewCounter(
			registry,
			"haptic_reconciliation_total",
			"Total number of reconciliation cycles",
		),
		ReconciliationErrors: pkgmetrics.NewCounter(
			registry,
			"haptic_reconciliation_errors_total",
			"Total number of failed reconciliation cycles",
		),

		// Deployment metrics
		DeploymentDuration: pkgmetrics.NewHistogramWithBuckets(
			registry,
			"haptic_deployment_duration_seconds",
			"Time spent deploying configurations",
			pkgmetrics.DeploymentDurationBuckets(),
		),
		DeploymentTotal: pkgmetrics.NewCounter(
			registry,
			"haptic_deployment_total",
			"Total number of deployment attempts",
		),
		DeploymentErrors: pkgmetrics.NewCounter(
			registry,
			"haptic_deployment_errors_total",
			"Total number of failed deployments",
		),
		HAProxyReloadsTotal: pkgmetrics.NewCounter(
			registry,
			"haptic_haproxy_reloads_total",
			"Total HAProxy reloads triggered by config deployments. A reload forks the HAProxy process; a high reload rate (vs runtime-API server updates) is the key capacity/SLO signal.",
		),

		// Fleet-convergence + config-staleness gauges (leader-only; see struct docs).
		HAProxyFleetSize: pkgmetrics.NewGauge(
			registry,
			"haptic_haproxy_fleet_size",
			"Number of HAProxy pods the last deployment targeted (leader-only; 0 on followers).",
		),
		HAProxyFleetConverged: pkgmetrics.NewGauge(
			registry,
			"haptic_haproxy_fleet_converged",
			"Number of HAProxy pods now at the desired config after the last deployment. Alert on haptic_haproxy_fleet_converged < haptic_haproxy_fleet_size (leader-only; 0 on followers).",
		),
		LastFullSyncTimestamp: pkgmetrics.NewGauge(
			registry,
			"haptic_last_full_sync_timestamp_seconds",
			"Unix timestamp (seconds) of the last time the whole HAProxy fleet converged on the desired config. Query staleness as time() - haptic_last_full_sync_timestamp_seconds. Seeded to controller start time so pre-first-sync staleness reads as uptime.",
		),
		DeploymentConsecutiveFailures: pkgmetrics.NewGauge(
			registry,
			"haptic_deployment_consecutive_failures",
			"Count of consecutive deployments that did not fully converge the fleet; resets to 0 on the first full sync. The noise-free replacement for alerting on rate(haptic_deployment_errors_total) now that transient deploy failures self-heal (leader-only; 0 on followers).",
		),

		// Per-pod apply metrics
		AgentApplyTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_deploy_apply_total",
			"Applies the HAPTIC agent accepted, by pod and by the mode it reported (runtime, file_only, reload, scheduled, noop). The reload-free share of a rollout is the runtime+file_only+noop fraction.",
			[]string{"pod", "mode"},
		),
		ApplyRejectedTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_apply_rejected_total",
			"Applies the HAPTIC agent refused (NACK), by pod. The pod keeps serving its previous config and its next apply carries the complete file set plus a reload; sustained growth means a render HAProxy will not accept.",
			[]string{"pod"},
		),
		AgentVersionSkewTotal: pkgmetrics.NewCounter(
			registry,
			"haptic_agent_version_skew_total",
			"Applies sent to a pod whose agent reported a different API major or a missing op kind. Never a refusal: a different API major gets the complete file set and a reload, a missing op kind reloads only the changes that need it. Sustained growth means a controller/agent rollout is stuck half-upgraded.",
		),
		RuntimeBackendOpsTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_runtime_backend_ops_total",
			"Backend lifecycle operations the fleet applied at runtime, by op kind.",
			[]string{"op"},
		),
		RuntimeServerOpsTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_runtime_server_ops_total",
			"Server lifecycle operations the fleet applied at runtime, by op kind.",
			[]string{"op"},
		),
		RuntimeBackendFallbackTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_runtime_backend_fallback_total",
			"Runtime backend batches a pod reloaded instead of running, by reason. `name_collision` is a fresh backend whose name a not-yet-deleted one still holds; the apply reloads because HAProxy cannot reveal a backend's shape.",
			[]string{reasonLabel},
		),

		RuntimeMapDivergence: pkgmetrics.NewCounterVec(
			registry,
			"haptic_runtime_map_divergence_total",
			"Runtime maps whose post-apply read-back disagreed with the desired content, forcing a reload fallback and so losing the reload-free lane for that sync.",
			[]string{"map"},
		),

		// Validation metrics
		ValidationTotal: pkgmetrics.NewCounter(
			registry,
			"haptic_validation_total",
			"HAProxy verdicts on rendered configurations: render gate checks and failed render validations (leader-only). Verdicts on the HAProxyTemplateConfig itself count in haptic_config_validation_total.",
		),
		ValidationErrors: pkgmetrics.NewCounter(
			registry,
			"haptic_validation_errors_total",
			"Rendered configurations HAProxy refused or could not check (leader-only). A configuration that fails or cannot finish validation counts in haptic_config_validation_total instead.",
		),
		ConfigValidated: pkgmetrics.NewGaugeVec(
			registry,
			"haptic_config_validated",
			"This replica's view of the HAProxyTemplateConfig Validated condition: 1 for the current status and reason, 0 for the others, all 0 before the first verdict. Only the leader reports reason HAProxyValidationFailed.",
			[]string{"status", reasonLabel},
		),
		ConfigValidationTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_config_validation_total",
			"Verdicts this replica reached on its HAProxyTemplateConfig, startup load gate included, by result (valid, invalid, inconclusive). An inconclusive validation is retried, so each retry counts.",
			[]string{resultLabel},
		),

		HAProxyChecksTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_haproxy_checks_total",
			"`haproxy -c` runs on this replica, by validator (input, http_content, render_gate, retained, admission) and result (passed, refused, error, cancelled).",
			[]string{validatorLabel, resultLabel},
		),
		HAProxyChecksReusedTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_haproxy_checks_reused_total",
			"`haproxy -c` runs avoided on this replica because HAProxy had already judged the identical input set, by validator and reused result (passed, refused).",
			[]string{validatorLabel, resultLabel},
		),

		// Resource metrics
		ResourceCount: pkgmetrics.NewGaugeVec(
			registry,
			"haptic_resource_count",
			"Number of resources by type",
			[]string{"type"},
		),

		// Event metrics
		EventSubscribers: pkgmetrics.NewGauge(
			registry,
			"haptic_event_subscribers",
			"Number of active event subscribers",
		),
		EventsPublished: pkgmetrics.NewCounter(
			registry,
			"haptic_events_published_total",
			"Total number of events published",
		),
		EventsDroppedCritical: pkgmetrics.NewCounter(
			registry,
			"haptic_events_dropped_critical_total",
			"Events dropped from critical subscribers (alert if > 0)",
		),
		EventsDroppedBySubscriber: pkgmetrics.NewCounterVec(
			registry,
			"haptic_events_dropped_by_subscriber_total",
			"Events dropped per subscriber and event type",
			[]string{"subscriber", "event_type"},
		),
		EventsDroppedObservability: pkgmetrics.NewGauge(
			registry,
			"haptic_events_dropped_observability_total",
			"Events dropped from observability subscribers (expected, non-alerting)",
		),

		// Queue wait metrics
		ReconciliationQueueWait: pkgmetrics.NewHistogramWithBuckets(
			registry,
			"haptic_reconciliation_queue_wait_seconds",
			"Time a reconciliation event spends waiting in the coordinator queue before processing starts",
			pkgmetrics.DurationBuckets(),
		),

		// Webhook metrics
		WebhookRequestsTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_webhook_requests_total",
			"Total number of webhook admission requests",
			[]string{"gvk", resultLabel},
		),
		WebhookRequestDuration: pkgmetrics.NewHistogramWithBuckets(
			registry,
			"haptic_webhook_request_duration_seconds",
			"Time spent processing webhook requests",
			pkgmetrics.AdmissionDurationBuckets(),
		),
		AdmissionRenderSlotWait: pkgmetrics.NewHistogramWithBuckets(
			registry,
			"haptic_admission_render_slot_wait_seconds",
			"Time an admission dry-run render waited for a render slot",
			pkgmetrics.AdmissionDurationBuckets(),
		),
		WebhookValidationTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_webhook_validation_total",
			"Total number of webhook validation results",
			[]string{"gvk", resultLabel},
		),

		// Leader election metrics
		LeaderElectionIsLeader: pkgmetrics.NewGauge(
			registry,
			"haptic_leader_election_is_leader",
			"Indicates if this replica is the leader (1) or follower (0)",
		),
		LeaderElectionTransitionsTotal: pkgmetrics.NewCounter(
			registry,
			"haptic_leader_election_transitions_total",
			"Total number of leadership transitions",
		),
		LeaderElectionTimeAsLeaderSeconds: pkgmetrics.NewCounter(
			registry,
			"haptic_leader_election_time_as_leader_seconds_total",
			"Cumulative time spent as leader in seconds",
		),

		// Discovery metrics
		HAProxyPodsRejectedTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_haproxy_pods_rejected_total",
			"Total number of HAProxy pods refused admission by the discovery component, labelled by reason. Persistent non-zero growth indicates the controller cannot talk to the deployed HAProxy pods (e.g., bundled HAProxy major.minor differs from the chart's haproxyVersion).",
			[]string{reasonLabel},
		),

		ConfigRejectedTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_config_rejected_total",
			"Total number of HAProxyTemplateConfig loads refused by the config-validation gate, labelled by the validator that rejected the config (basic, template, jsonpath, validationtests) or \"coordinator\" when a validator timed out / didn't respond. Non-zero growth means the controller is refusing a new config and continuing on the last-good one.",
			[]string{validatorLabel},
		),

		HTTPContentRejectedTotal: pkgmetrics.NewCounter(
			registry,
			"haptic_http_content_rejected_total",
			"Total number of pending HTTP content versions rejected before acceptance.",
		),

		HTTPContentRevokedTotal: pkgmetrics.NewCounter(
			registry,
			"haptic_http_content_revoked_total",
			"Total number of HTTP content acceptances revoked after a render was refused.",
		),

		ControllerReinitializationsTotal: pkgmetrics.NewCounter(
			registry,
			"haptic_controller_reinitializations_total",
			"Total controller iteration restarts after the first. A restart rebuilds the components from a freshly resolved configuration while the running iteration keeps serving, then hands leadership over on the same replica. Growth means the effective configuration keeps changing, most often a CRD appearing or disappearing.",
		),

		ConfigPinned: pkgmetrics.NewGauge(
			registry,
			"haptic_config_pinned",
			"1 while the render gate holds renders HAProxy refused: the fleet keeps serving the last accepted config and no new render reaches it until the input is fixed (leader-only; 0 on followers).",
		),
		RetainedConfigActive: pkgmetrics.NewGauge(registry, "haptic_retained_config_active", "1 while the leader uses retained configuration to start HAProxy pods after a failed render."),
		RejectedWatchedInputs: pkgmetrics.NewGauge(
			registry,
			"haptic_rejected_watched_inputs",
			"Watched resource changes held at their last validated revision while independent changes continue.",
		),

		RenderProfiles: pkgmetrics.NewGauge(
			registry,
			"haptic_render_profiles",
			"Distinct backend profiles (defaults haptic-be-*) in the most recent reconcile render. Profile cardinality is what backends collapse onto, tracking the config's structural size (leader-only; 0 on followers).",
		),

		RenderWarnings: pkgmetrics.NewGaugeVec(
			registry,
			"haptic_render_warnings",
			"Current template-recorded warnings by reason in the last successful reconciliation (leader-only).",
			[]string{reasonLabel},
		),

		RenderTotal: pkgmetrics.NewCounterVec(
			registry,
			"haptic_render_total",
			"Reconcile renders, failed ones included, by cache_state: cold re-evaluated every template, warm reused the incremental graph, replay reused the previous output unchanged. Counts on every replica, since followers render to keep their graph warm for a leadership change; a replica that keeps counting cold pays full render cost on each change.",
			[]string{"cache_state"},
		),

		RenderErrors: pkgmetrics.NewCounterVec(
			registry,
			"haptic_render_errors_total",
			"Reconcile renders that failed, by the cache_state they ran from; each also counts in haptic_render_total. Counts on every replica.",
			[]string{"cache_state"},
		),

		// Build info metric
		BuildInfo: pkgmetrics.NewGaugeVec(
			registry,
			"haptic_build_info",
			"Controller build information (version labels, value always 1)",
			[]string{"version", "haproxy_version", "go_version"},
		),
	}

	// Seed last-full-sync to construction time so that, before the first full
	// fleet sync, staleness (time() - gauge) reads as uptime rather than the
	// entire Unix epoch.
	m.LastFullSyncTimestamp.Set(float64(time.Now().Unix()))
	registry.MustRegister(newMailboxDepthCollector())
	m.initKnownSeries()

	return m
}

// initKnownSeries exports every known label value at zero, so increase() sees
// the first increment and an absent series means a broken scrape.
func (m *Metrics) initKnownSeries() {
	for _, cacheState := range []string{renderCold, renderWarm, renderReplay} {
		m.RenderTotal.WithLabelValues(cacheState)
	}
	for _, cacheState := range []string{renderCold, renderWarm} {
		m.RenderErrors.WithLabelValues(cacheState)
	}
	for _, result := range configValidationResults {
		m.ConfigValidationTotal.WithLabelValues(result)
	}
	for _, state := range configValidatedStates {
		m.ConfigValidated.WithLabelValues(state.status, state.reason)
	}
	for _, validatorName := range append(validator.AllValidatorNames(), configRejectedByCoordinator) {
		m.ConfigRejectedTotal.WithLabelValues(validatorName)
	}
}

// RecordReconciliation records a completed reconciliation cycle.
//
// Parameters:
//   - durationSeconds: Time spent in reconciliation (use time.Since(start).Seconds())
//   - success: Whether the reconciliation completed successfully
func (m *Metrics) RecordReconciliation(durationSeconds float64, success bool) {
	m.ReconciliationTotal.Inc()
	m.ReconciliationDuration.Observe(durationSeconds)
	if !success {
		m.ReconciliationErrors.Inc()
	}
}

// RecordDeployment records a deployment attempt.
//
// Parameters:
//   - durationSeconds: Time spent deploying (use time.Since(start).Seconds())
//   - success: Whether the deployment completed successfully
func (m *Metrics) RecordDeployment(durationSeconds float64, success bool) {
	m.DeploymentTotal.Inc()
	m.DeploymentDuration.Observe(durationSeconds)
	if !success {
		m.DeploymentErrors.Inc()
	}
}

// RecordDeploymentReloads records how many pods a completed deployment
// reloaded — the headline capacity/SLO signal (see the metric help text). Zero
// is skipped so a reload-free deployment doesn't perturb the counter.
func (m *Metrics) RecordDeploymentReloads(reloads int) {
	if reloads > 0 {
		m.HAProxyReloadsTotal.Add(float64(reloads))
	}
}

// SetFleetConvergence records the outcome of a completed fleet deployment onto
// the fleet-convergence + config-staleness gauges. It is O(1) and must be
// called LEADER-ONLY (DeploymentCompletedEvent is leader-only).
//
// Given the deploy's total / succeeded / failed instance counts it:
//   - sets haptic_haproxy_fleet_size      = total
//   - sets haptic_haproxy_fleet_converged = succeeded
//   - on a full sync (succeeded == total && total > 0): resets the consecutive-
//     failure count to 0 and stamps haptic_last_full_sync_timestamp_seconds with now
//   - otherwise, on any failure (failed > 0): increments the consecutive-failure count
//
// A full sync is checked first so that succeeded == total always wins over a
// stray failed > 0 (the two are mutually exclusive for a well-formed result).
func (m *Metrics) SetFleetConvergence(total, succeeded, failed int) {
	m.HAProxyFleetSize.Set(float64(total))
	m.HAProxyFleetConverged.Set(float64(succeeded))

	switch {
	case total > 0 && succeeded == total:
		m.consecutiveFailures = 0
		m.LastFullSyncTimestamp.Set(float64(time.Now().Unix()))
	case failed > 0:
		m.consecutiveFailures++
	}
	m.DeploymentConsecutiveFailures.Set(float64(m.consecutiveFailures))
}

// ResetFleetConvergence clears the fleet-convergence gauges when this replica is
// no longer the leader. DeploymentCompletedEvent is leader-only, so a follower
// must not keep reporting the values it set while it was leader:
//   - fleet_size / fleet_converged / consecutive_failures → 0
//     (so converged < fleet_size becomes 0 < 0, i.e. false, and can't false-alert)
//   - last_full_sync → now, so a former leader doesn't accrue growing staleness
//   - config_pinned → 0, since the render gate's latch is per leadership term
func (m *Metrics) ResetFleetConvergence() {
	m.consecutiveFailures = 0
	m.HAProxyFleetSize.Set(0)
	m.HAProxyFleetConverged.Set(0)
	m.DeploymentConsecutiveFailures.Set(0)
	m.LastFullSyncTimestamp.Set(float64(time.Now().Unix()))
	m.ConfigPinned.Set(0)
}

// RecordAgentApply records one apply a pod's agent accepted, by the mode it
// reported.
func (m *Metrics) RecordAgentApply(pod, mode string) {
	m.AgentApplyTotal.WithLabelValues(pod, mode).Inc()
}

// RecordApplyRejected records one apply a pod's agent refused.
func (m *Metrics) RecordApplyRejected(pod string) {
	m.ApplyRejectedTotal.WithLabelValues(pod).Inc()
}

// RecordAgentVersionSkew records one pod whose agent speaks a contract this
// controller does not fully share.
func (m *Metrics) RecordAgentVersionSkew() {
	m.AgentVersionSkewTotal.Inc()
}

// RecordRuntimeBackendOp records one backend lifecycle op a pod executed.
func (m *Metrics) RecordRuntimeBackendOp(op string) {
	m.RuntimeBackendOpsTotal.WithLabelValues(op).Inc()
}

// RecordRuntimeServerOp records one server lifecycle op a pod executed.
func (m *Metrics) RecordRuntimeServerOp(op string) {
	m.RuntimeServerOpsTotal.WithLabelValues(op).Inc()
}

// RecordRuntimeBackendFallback records one runtime backend batch a pod
// reloaded instead of running, by reason.
func (m *Metrics) RecordRuntimeBackendFallback(reason string) {
	m.RuntimeBackendFallbackTotal.WithLabelValues(reason).Inc()
}

// RecordRuntimeMapDivergence records one runtime map that failed its
// post-apply read-back and so forced a reload fallback.
func (m *Metrics) RecordRuntimeMapDivergence(mapName string) {
	m.RuntimeMapDivergence.WithLabelValues(mapName).Inc()
}

// RecordValidation records a validation attempt.
//
// Parameters:
//   - success: Whether the validation passed
func (m *Metrics) RecordValidation(success bool) {
	m.ValidationTotal.Inc()
	if !success {
		m.ValidationErrors.Inc()
	}
}

// configValidatedState is one status and reason of the Validated condition.
type configValidatedState struct {
	status string
	reason string
}

var (
	configValidationSucceeded = configValidatedState{string(metav1.ConditionTrue), v1alpha1.ReasonValidationSucceeded}
	configInvalid             = configValidatedState{string(metav1.ConditionFalse), v1alpha1.ReasonConfigInvalid}
	configRefusedByHAProxy    = configValidatedState{string(metav1.ConditionFalse), v1alpha1.ReasonHAProxyValidationFailed}
	configRejectedAtLoadGate  = configValidatedState{string(metav1.ConditionFalse), v1alpha1.ReasonLoadGateFailed}
	configInconclusive        = configValidatedState{string(metav1.ConditionUnknown), v1alpha1.ReasonValidationInconclusive}

	configValidatedStates = []configValidatedState{
		configValidationSucceeded, configInvalid, configRefusedByHAProxy, configRejectedAtLoadGate, configInconclusive,
	}
)

const (
	configValidationValid        = "valid"
	configValidationInvalid      = "invalid"
	configValidationInconclusive = "inconclusive"
)

var configValidationResults = []string{configValidationValid, configValidationInvalid, configValidationInconclusive}

func (m *Metrics) setConfigValidated(current configValidatedState) {
	for _, state := range configValidatedStates {
		value := 0.0
		if state == current {
			value = 1
		}
		m.ConfigValidated.WithLabelValues(state.status, state.reason).Set(value)
	}
	m.configValidated.Store(&current)
}

func (m *Metrics) recordConfigValidation(result string) {
	m.ConfigValidationTotal.WithLabelValues(result).Inc()
}

// RecordLoadGate records the startup load gate's verdict on the configuration
// an iteration starts from. A pass leaves the reported state alone: the
// iteration's own events establish it.
func (m *Metrics) RecordLoadGate(passed bool) {
	if passed {
		m.recordConfigValidation(configValidationValid)
		return
	}
	m.recordConfigValidation(configValidationInvalid)
	m.setConfigValidated(configRejectedAtLoadGate)
}

// CarryConfigValidation continues previous's config-validation series in m. A
// reinitialization swaps in a fresh registry, and the valid verdict that
// triggered it would otherwise never be scraped. previous may be nil.
func (m *Metrics) CarryConfigValidation(previous *Metrics) error {
	if previous == nil {
		return nil
	}
	for _, result := range configValidationResults {
		var carried dto.Metric
		if err := previous.ConfigValidationTotal.WithLabelValues(result).Write(&carried); err != nil {
			return fmt.Errorf("reading %s config validations: %w", result, err)
		}
		m.ConfigValidationTotal.WithLabelValues(result).Add(carried.GetCounter().GetValue())
	}
	if state := previous.configValidated.Load(); state != nil {
		m.setConfigValidated(*state)
	}
	return nil
}

// RecordHAProxyCheck records one check a validator asked HAProxy for; reused
// means an earlier verdict answered it without running `haproxy -c`.
func (m *Metrics) RecordHAProxyCheck(validatorName, result string, reused bool) {
	if reused {
		m.HAProxyChecksReusedTotal.WithLabelValues(validatorName, result).Inc()
		return
	}
	m.HAProxyChecksTotal.WithLabelValues(validatorName, result).Inc()
}

// SetResourceCount sets the count for a specific resource type.
//
// Parameters:
//   - resourceType: The type of resource (e.g., "ingresses", "services")
//   - count: The current number of resources of this type
func (m *Metrics) SetResourceCount(resourceType string, count int) {
	m.ResourceCount.WithLabelValues(resourceType).Set(float64(count))
}

// SetEventSubscribers sets the number of active event subscribers.
//
// Parameters:
//   - count: The current number of event subscribers
func (m *Metrics) SetEventSubscribers(count int) {
	m.EventSubscribers.Set(float64(count))
}

// RecordEvent records an event publication.
// Call this for every event published to the EventBus.
func (m *Metrics) RecordEvent() {
	m.EventsPublished.Inc()
}

// RecordWebhookRequest records a webhook admission request.
//
// Parameters:
//   - gvk: The GVK of the resource being validated (e.g., "v1.ConfigMap")
//   - result: The result of the request ("allowed", "denied", or "error")
//   - durationSeconds: Time spent processing the request
func (m *Metrics) RecordWebhookRequest(gvk, result string, durationSeconds float64) {
	m.WebhookRequestsTotal.WithLabelValues(gvk, result).Inc()
	m.WebhookRequestDuration.Observe(durationSeconds)
}

// RecordAdmissionRenderSlotWait records an admission render's wait for a slot.
func (m *Metrics) RecordAdmissionRenderSlotWait(wait time.Duration) {
	m.AdmissionRenderSlotWait.Observe(wait.Seconds())
}

// RecordWebhookValidation records a webhook validation result.
//
// Parameters:
//   - gvk: The GVK of the resource being validated
//   - result: The validation result ("allowed", "denied", or "error")
func (m *Metrics) RecordWebhookValidation(gvk, result string) {
	m.WebhookValidationTotal.WithLabelValues(gvk, result).Inc()
}

// RecordHAProxyPodRejected increments the rejection counter for a stable
// reason label (e.g. "version_mismatch_older", "version_check_failed").
// Discovery publishes HAProxyPodRejectedEvent; the metrics component
// translates each event into a counter increment.
func (m *Metrics) RecordHAProxyPodRejected(reason string) {
	m.HAProxyPodsRejectedTotal.WithLabelValues(reason).Inc()
}

// RecordConfigRejected increments the config-rejection counter for the given
// validator (the one whose check failed).
func (m *Metrics) RecordConfigRejected(validatorName string) {
	m.ConfigRejectedTotal.WithLabelValues(validatorName).Inc()
}

// RecordHTTPContentRevoked counts one revoked HTTP content acceptance.
func (m *Metrics) RecordHTTPContentRevoked() {
	m.HTTPContentRevokedTotal.Inc()
}

func (m *Metrics) RecordHTTPContentRejected() {
	m.HTTPContentRejectedTotal.Inc()
}

// SetRenderProfiles records how many distinct backend profiles the most recent
// reconcile render emitted.
func (m *Metrics) SetRenderProfiles(count int) {
	m.RenderProfiles.Set(float64(count))
}

// SetRenderWarnings replaces the warning counts, removing resolved reasons.
func (m *Metrics) SetRenderWarnings(counts map[string]int) {
	m.RenderWarnings.Reset()
	for reason, count := range counts {
		m.RenderWarnings.WithLabelValues(reason).Set(float64(count))
	}
}

// RecordRender records one reconcile render by the cache state it ran from.
func (m *Metrics) RecordRender(cacheState string) {
	m.RenderTotal.WithLabelValues(cacheState).Inc()
}

// RecordRenderFailure records a reconcile render that produced no output;
// graphWarm is whether it had a committed graph to build on.
func (m *Metrics) RecordRenderFailure(graphWarm bool) {
	cacheState := renderCold
	if graphWarm {
		cacheState = renderWarm
	}
	m.RenderTotal.WithLabelValues(cacheState).Inc()
	m.RenderErrors.WithLabelValues(cacheState).Inc()
}

// SetConfigPinned sets whether the render gate is holding renders.
func (m *Metrics) SetConfigPinned(pinned bool) {
	if pinned {
		m.ConfigPinned.Set(1)
		return
	}
	m.ConfigPinned.Set(0)
}

// SetIsLeader sets whether this replica is the leader.
//
// Parameters:
//   - isLeader: true if this replica is the leader, false otherwise
func (m *Metrics) SetIsLeader(isLeader bool) {
	if isLeader {
		m.LeaderElectionIsLeader.Set(1)
	} else {
		m.LeaderElectionIsLeader.Set(0)
	}
}

// RecordLeadershipTransition records a leadership state change.
// Call this whenever leadership is gained or lost.
func (m *Metrics) RecordLeadershipTransition() {
	m.LeaderElectionTransitionsTotal.Inc()
}

// AddTimeAsLeader adds time spent as leader to the cumulative counter.
//
// Parameters:
//   - seconds: Time spent as leader in seconds
func (m *Metrics) AddTimeAsLeader(seconds float64) {
	m.LeaderElectionTimeAsLeaderSeconds.Add(seconds)
}

// RecordEventDrop records an event drop due to full subscriber buffer.
// This increments the critical total and the per-subscriber counter.
// Call this from the drop callback registered with EventBus.SetDropCallback().
//
// Parameters:
//   - subscriberName: The name of the subscriber that dropped the event
//   - eventType: The event type that was dropped
func (m *Metrics) RecordEventDrop(subscriberName, eventType string) {
	m.AddEventDrops(subscriberName, eventType, 1)
}

// AddEventDrops restores process-lifetime critical-drop totals into a new
// per-iteration registry.
func (m *Metrics) AddEventDrops(subscriberName, eventType string, count uint64) {
	increment := float64(count)
	m.EventsDroppedCritical.Add(increment)
	m.EventsDroppedBySubscriber.WithLabelValues(subscriberName, eventType).Add(increment)
}

// SetObservabilityDrops sets the observability drop gauge from EventBus statistics.
// This should be called periodically since observability drops don't trigger callbacks.
func (m *Metrics) SetObservabilityDrops(count uint64) {
	m.EventsDroppedObservability.Set(float64(count))
}

// SetBuildInfo sets the build info metric with version labels.
// Call once at startup with the version information for this binary.
//
// Parameters:
//   - version: The controller version (e.g., "0.1.0-alpha.10")
//   - haproxyVersion: The HAProxy major.minor version (e.g., "3.2")
//   - goVersion: The Go runtime version (e.g., "go1.27.0")
func (m *Metrics) SetBuildInfo(version, haproxyVersion, goVersion string) {
	m.BuildInfo.WithLabelValues(version, haproxyVersion, goVersion).Set(1)
}

// RecordQueueWait records how long a reconciliation event waited in the coordinator
// queue before processing started.
func (m *Metrics) RecordQueueWait(seconds float64) {
	m.ReconciliationQueueWait.Observe(seconds)
}
