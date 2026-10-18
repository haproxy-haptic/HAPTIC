// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/configchange"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/crdwatch"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validator"

	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/introspection"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/schemafetcher"
)

// effectiveResolutionTimeout bounds one resolution's schema probes (the
// RequiresFields field checks fetch resource schemas through the cluster
// fetcher). Same rationale and order of magnitude as the type-bootstrap
// deadline: generous for a healthy apiserver, fails the resolution with a
// clear error on a degraded one (retried by the iteration loop / the next
// CRD event).
const effectiveResolutionTimeout = 10 * time.Second

// DiscoveryServedChecker answers "is this plural resource served at this
// group/version?" from live apiserver discovery, memoizing one discovery call
// per distinct group/version. It doubles as the SchemaFieldChecker for
// RequiresFields stripping: the same discovery answer carries the Kind for
// each served plural, which the schema fetcher needs to probe the resolved
// schema's fields. Instances are single-use snapshots: build a fresh one per
// resolution so CRDs installed or upgraded between config loads are seen.
// Not safe for concurrent use (resolution is sequential).
//
// Only an authoritative NotFound counts as "unserved". A TRANSIENT discovery
// error (apiserver blip, aggregated-API hiccup) is recorded in transientErr
// instead: silently treating it as unserved would strip optional features —
// and bounce the controller through a spurious reinit — on every blip. The
// caller checks TransientErr() after resolution and fails the whole
// resolution, which the iteration retry loop (or the next CRD event)
// re-attempts safely. Schema-fetch errors during FieldServed fail the
// resolution directly (the SchemaFieldChecker contract).
type DiscoveryServedChecker struct {
	discovery    discovery.DiscoveryInterface
	fetcher      schemafetcher.Fetcher
	ctx          context.Context
	logger       *slog.Logger
	cache        map[string]map[string]servedResource // groupVersion -> plural
	transientErr error
}

func NewDiscoveryServedChecker(ctx context.Context, d discovery.DiscoveryInterface, fetcher schemafetcher.Fetcher, logger *slog.Logger) *DiscoveryServedChecker {
	return &DiscoveryServedChecker{
		discovery: d,
		fetcher:   fetcher,
		ctx:       ctx,
		logger:    logger,
		cache:     make(map[string]map[string]servedResource),
	}
}

type servedResource struct {
	kind       string
	namespaced bool
}

// IsServed implements coreconfig.ServedVersionChecker.
func (c *DiscoveryServedChecker) IsServed(apiVersion, resources string) bool {
	_, ok := c.servedResources(apiVersion)[resources]
	return ok
}

// IsNamespaced implements coreconfig.NamespaceScopeChecker.
func (c *DiscoveryServedChecker) IsNamespaced(apiVersion, resources string) bool {
	return c.servedResources(apiVersion)[resources].namespaced
}

// FieldServed implements coreconfig.SchemaFieldChecker: it fetches the
// resolved schema for the resource (CRD openAPIV3Schema or aggregated
// OpenAPI v3, via the cluster fetcher) and walks it for the dot-path. Every
// error — transient fetch failure or a genuinely missing schema for a
// resource discovery says is served — fails the resolution rather than
// silently stripping, mirroring the fail-closed type-bootstrap contract.
func (c *DiscoveryServedChecker) FieldServed(apiVersion, resources, fieldPath string) (bool, error) {
	served, ok := c.servedResources(apiVersion)[resources]
	kind := served.kind
	if c.transientErr != nil {
		return false, c.transientErr
	}
	if !ok || kind == "" {
		return false, fmt.Errorf("no served kind found for %s/%s", apiVersion, resources)
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return false, fmt.Errorf("parsing apiVersion %q: %w", apiVersion, err)
	}
	sch, components, err := c.fetcher.Fetch(c.ctx, gv.WithKind(kind))
	if err != nil {
		return false, fmt.Errorf("fetching schema for %s/%s: %w", apiVersion, resources, err)
	}
	return schemafetcher.SchemaHasField(sch, components, fieldPath), nil
}

// servedResources returns the served plurals of a group/version, querying
// discovery on first use and memoizing the answer (including the negative
// one). Transient errors are recorded in transientErr; see the type comment.
func (c *DiscoveryServedChecker) servedResources(apiVersion string) map[string]servedResource {
	served, ok := c.cache[apiVersion]
	if !ok {
		served = make(map[string]servedResource)
		list, err := c.discovery.ServerResourcesForGroupVersion(apiVersion)
		switch {
		case apierrors.IsNotFound(err):
			c.logger.Debug("Discovery reports group/version not served",
				"group_version", apiVersion)
		case err != nil:
			c.logger.Warn("Transient discovery error; resolution will be retried",
				"group_version", apiVersion, "error", err)
			if c.transientErr == nil {
				c.transientErr = fmt.Errorf("discovery for %s: %w", apiVersion, err)
			}
		default:
			for i := range list.APIResources {
				resource := &list.APIResources[i]
				served[resource.Name] = servedResource{kind: resource.Kind, namespaced: resource.Namespaced}
			}
		}
		c.cache[apiVersion] = served
	}
	return served
}

// TransientErr returns the first non-NotFound discovery error observed, or
// nil when every answer was authoritative.
func (c *DiscoveryServedChecker) TransientErr() error {
	return c.transientErr
}

// configureEffectiveConfig exposes an accepted resolution, installs the same
// resolver on the live config-change path, and watches for schema changes.
func configureEffectiveConfig(
	ctx context.Context,
	snapshot *configchange.ValidatedSnapshot,
	k8sClient *client.Client,
	setup *componentSetup,
	infra *persistentInfra,
	logger *slog.Logger,
) {
	infra.IntrospectionRegistry.Publish("effectiveConfigResolution",
		introspection.Func(func() (any, error) { return snapshot.Resolution, nil }))

	setup.ConfigChangeHandler.SetEffectiveResolver(func(c *coreconfig.Config) (*configchange.ResolvedConfig, error) {
		resolved, resolution, resolveErr := resolveEffectiveConfig(ctx, c, k8sClient, logger)
		if resolveErr != nil {
			return nil, resolveErr
		}
		return &configchange.ResolvedConfig{Config: resolved, Resolution: resolution}, nil
	})

	startCRDWatch(ctx, setup, snapshot, k8sClient, logger)
}

// startCRDWatch launches the CRD watch (runIteration step 4.2). Groups come
// from the RAW config's candidate lists — an unavailable optional resource's
// CRD appearing is exactly the event this watch exists for. The reload only
// fires when a fresh resolution differs from this iteration's; a resolution
// ERROR is inconclusive — the watch neither reloads on it (a discovery blip
// must not bounce the controller) nor accepts it as final (it re-checks on
// the component's bounded recheck cadence until the answer settles).
func startCRDWatch(
	ctx context.Context,
	setup *componentSetup,
	snapshot *configchange.ValidatedSnapshot,
	k8sClient *client.Client,
	logger *slog.Logger,
) {
	rawCfg := snapshot.RawConfig
	resolution := snapshot.Resolution
	crdWatch := crdwatch.New(k8sClient, crdwatch.RelevantGroups(rawCfg),
		func() (bool, error) {
			_, freshResolution, resolveErr := resolveEffectiveConfig(ctx, rawCfg, k8sClient, logger)
			if resolveErr != nil {
				// Transient discovery errors surface as resolveErr too — a
				// reload on a blip would bounce the controller for nothing.
				// Report the answer as inconclusive: the watch never reloads
				// on it directly, but keeps re-checking on its bounded
				// cadence. A PERSISTENT failure (e.g. a required resource's
				// CRD genuinely removed) trips the watch's error-streak bound
				// (DefaultMaxErrorStreak), which escalates to a reload so the
				// iteration restart fails fast and surfaces via /healthz.
				return false, resolveErr
			}
			return !resolution.Equal(freshResolution), nil
		},
		func() {
			setup.ConfigChangeHandler.RequestEffectiveReload()
		},
		logger)
	startInErrGroup(setup.ErrGroup, setup.IterCtx, logger, setup.CancelCause, "crd watch", crdWatch.Start)
}

// resolveEffectiveConfig resolves the config's watched resources against live
// discovery and returns the effective config the iteration consumes (see
// coreconfig.ResolveEffective). RequiresFields probing goes through a FRESH
// cluster schema fetcher per call — the fetcher caches CRDs per instance,
// and a stale CRD would blind the CRD-watch re-resolution to an in-place
// schema upgrade. Resolution outcomes are logged: per-resource
// resolved versions at debug, feature stripping at info (an operator whose
// routing silently lost a kind must find the cause in the log).
func resolveEffectiveConfig(ctx context.Context, cfg *coreconfig.Config, k8sClient *client.Client, logger *slog.Logger) (*coreconfig.Config, *coreconfig.Resolution, error) {
	apiextClient, err := apiextensionsclientset.NewForConfig(k8sClient.RestConfig())
	if err != nil {
		return nil, nil, fmt.Errorf("constructing apiextensions client for schema probing: %w", err)
	}
	fetcher := NewClusterSchemaFetcher(apiextClient, k8sClient.Clientset().Discovery())
	ctx, cancel := context.WithTimeout(ctx, effectiveResolutionTimeout)
	defer cancel()

	checker := NewDiscoveryServedChecker(ctx, k8sClient.Clientset().Discovery(), fetcher, logger)
	effective, resolution, err := coreconfig.ResolveEffective(cfg, checker, checker)
	if err != nil {
		if schemaErr := (&validator.SchemaAcquisitionError{Subject: "the effective config", Err: err}); schemaErr.Transient() {
			return nil, nil, schemaErr
		}
		return nil, nil, err
	}
	if terr := checker.TransientErr(); terr != nil {
		// Fail the whole resolution rather than act on a partial view: an
		// optional feature must not strip (nor a reload fire) because of an
		// apiserver blip. The config-change handler retries a transient error.
		return nil, nil, &validator.SchemaAcquisitionError{Subject: "the effective config", Err: terr}
	}

	for name, version := range resolution.ResolvedVersions {
		logger.Debug("Watched resource resolved", "resource", name, "api_version", version)
	}
	if len(resolution.Unavailable) > 0 {
		logger.Info("Optional watched resources unavailable — dependent features stripped",
			"unavailable", resolution.Unavailable,
			"stripped_snippets", len(resolution.StrippedSnippets),
			"stripped_tests", len(resolution.StrippedTests))
	}
	if len(resolution.StrippedFieldTests) > 0 {
		logger.Info("Validation tests requiring schema fields absent from this cluster's generation stripped",
			"stripped_tests", resolution.StrippedFieldTests)
	}
	return effective, resolution, nil
}
