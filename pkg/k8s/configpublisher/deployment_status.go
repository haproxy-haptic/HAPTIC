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

package configpublisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
)

// ErrRuntimeConfigNotPublished reports that the target HAProxyCfg does not
// publish the pod's checksum yet: it is missing, its publication has not
// written the auxiliary references, or it publishes other content. The update
// is incomplete: retry it after the next publication.
var ErrRuntimeConfigNotPublished = errors.New("HAProxyCfg not published yet")

const (
	statusSubresource = "status"
	runtimeConfigKind = "HAProxyCfg"

	// Auxiliary-file CRD kinds, used in SSA payloads, ownership references, and
	// cleanup/error messages across this package.
	kindMapFile     = "HAProxyMapFile"
	kindGeneralFile = "HAProxyGeneralFile"
	kindCRTListFile = "HAProxyCRTListFile"
	kindSecret      = "Secret"

	mapFileDescription     = "map file"
	generalFileDescription = "general file"
	crtListFileDescription = "crt-list file"
)

// apiVersionV1Alpha1 is the CRD API version used in SSA payloads. Hard-coded
// rather than derived because v1alpha1 is the only version we serve and a
// future bump is a deliberate operation that should update this constant too.
const apiVersionV1Alpha1 = "haproxy-haptic.org/v1alpha1"

// podStatusFieldManager returns the SSA field manager used for a specific
// pod's status entry. Each pod owns ONLY its own entry in deployedToPods —
// concurrent writes from different pods merge naturally via listType=map
// (keyed on podName) instead of fighting each other through last-write-wins
// on the full-object UpdateStatus path that preceded this design.
//
// The format embeds the pod name so cleanup paths can identify which
// field manager owns which entry without consulting external state. K8s
// field-manager names are limited to 128 chars and pod names cap at 63, so
// the combined length always fits.
func podStatusFieldManager(podName string) string {
	return "haptic-pod-status-" + podName
}

// UpdateDeploymentStatus updates the per-pod deployment status entry on
// HAProxyCfg and on the auxiliary files of the publication at the pod's
// checksum (see ErrRuntimeConfigNotPublished).
//
// Uses Server-Side Apply with one field manager per pod (see
// `podStatusFieldManager`). The CRDs declare deployedToPods as
// `listType=map listMapKey=podName`, so the API server merges
// each per-pod SSA into the existing list without overwriting entries owned
// by other pods' field managers. The previous read-modify-write design
// had a race where two pods completing deploys ~50ms apart would each read
// the same snapshot of deployedToPods and last-write-wins — silently
// dropping one pod's status entry. Confirmed via debug-logs/ from CI
// pipeline 2559320164: HAProxyCfg.status.deployedToPods ended up with
// only one of two HAProxy pods listed, and TestIngress* polls timed out
// because the missing pod never showed up at the latest spec.checksum.
//
// A drift check reads the HAProxyCfg and one list per auxiliary kind live and
// re-applies an entry only where the object no longer shows what this pod's
// identical previous apply left there (issue #376).
//
// Precondition — single-writer-per-pod: the caller must serialize all calls
// for a given pod. The controller's processAllPendingStatusWork runs one
// goroutine per pod key; statusStamps.retainLivePodKeys relies on it.
func (p *Publisher) UpdateDeploymentStatus(ctx context.Context, update *DeploymentStatusUpdate) error {
	p.logger.Debug("Updating deployment status (SSA)",
		"runtime_config", update.RuntimeConfigName,
		"pod", update.PodName,
		"drift_check", update.IsDriftCheck,
	)

	reads, err := p.readDriftTargets(ctx, update)
	if err != nil {
		return err
	}

	podStatus := buildPodStatus(update)

	// On a FAILED sync the pod did not receive update.Checksum, so advancing
	// deployedToPods[pod].checksum to it would make the entry equal
	// spec.checksum and falsely read as converged — the convergence signal is
	// checksum-equality (LastError is advisory and ignored by e.g. the e2e
	// poll). Preserve the last successfully-deployed checksum instead. Blanking
	// it is not an option: an empty checksum is omitted from the SSA payload
	// (buildPodStatusSSAPayload) and SSA would then DELETE the field this
	// manager already owns, losing the last-good value. So re-emit the existing
	// checksum unchanged; if the pod has no prior success, leave it empty so the
	// pod correctly reads as never-converged.
	if update.Error != "" {
		podStatus.Checksum = ""
		// The plan the pod still runs is unchanged by a failed sync, and it is
		// the baseline the next apply diffs against — same re-emit rule as the
		// checksum above, or SSA deletes it.
		if existing, ok := p.existingPodStatus(ctx, update, reads.runtimeConfig); ok &&
			existing.PodUID == update.PodUID && existing.PodRuntimeID == update.PodRuntimeID {
			podStatus.Checksum = existing.Checksum
			podStatus.AppliedPlanID = existing.AppliedPlanID
			podStatus.RunningPlanID = existing.RunningPlanID
			podStatus.Mode = existing.Mode
			podStatus.Reasons = existing.Reasons
		}
	}

	runtimeKey := stampKey{kind: runtimeConfigKind, namespace: update.RuntimeConfigNamespace, name: update.RuntimeConfigName, podName: update.PodName}
	check := stampCheck{observed: podStatusFootprint(reads.runtimeConfig, update.PodName)}
	if err := p.applyPodStatusToRuntimeConfig(ctx, update, runtimeKey, &podStatus, check); err != nil {
		return fmt.Errorf("applying pod status to HAProxyCfg: %w", err)
	}

	// On failure the pod received neither the new config nor the new auxiliary
	// files, so skip advancing their per-pod checksums — leaving the last-good
	// values untouched (same preserve-on-failure rationale as the main config
	// checksum above). The next successful deploy or reconcile re-applies them.
	if update.Error != "" {
		return nil
	}

	auxFiles, err := p.resolveAuxiliaryFileReferences(ctx, update, reads.runtimeConfig)
	if errors.Is(err, ErrRuntimeConfigNotPublished) {
		return err
	}
	if err != nil {
		// Auxiliary file lookups are best-effort during status updates —
		// the next reconcile retries. Don't fail the whole call.
		p.logger.Debug("Auxiliary file reference lookup failed (proceeding with HAProxyCfg-only SSA)",
			"runtime_config", update.RuntimeConfigName,
			"error", err,
		)
		return nil
	}
	if update.IsDriftCheck {
		p.listDriftChildren(ctx, update, auxFiles, &reads)
	}

	live := p.applyPodStatusToAuxiliaryFiles(ctx, auxFiles, update, &reads)
	live[runtimeKey] = struct{}{}
	p.statusStamps.retainLivePodKeys(update.PodName, live)
	return nil
}

// statusReads are a drift check's live reads of the objects a pod's status is
// applied to. A nil field was not read, so its objects are applied unverified.
type statusReads struct {
	runtimeConfig *haproxyv1alpha1.HAProxyCfg
	children      *childListing
}

func (r *statusReads) listed(key stampKey) metav1.Object {
	if r.children == nil || key.namespace != r.children.namespace {
		return nil
	}
	obj, _ := r.children.lookup(key.kind, key.name)
	return obj
}

// stampCheck says when an apply whose payload equals the last one may be
// skipped.
type stampCheck struct {
	// elideUnchanged skips on the payload alone: issue #163's elision between
	// drift checks, for auxiliary files.
	elideUnchanged bool
	// observed is the footprint a drift check read live; "" when not read.
	observed string
}

func (c stampCheck) skips(last stampedEntry) bool {
	return c.elideUnchanged || (c.observed != "" && c.observed == last.footprint)
}

// readDriftTargets reads the HAProxyCfg live for a drift check. A failed read
// leaves the round unverified: every entry is re-applied.
func (p *Publisher) readDriftTargets(ctx context.Context, update *DeploymentStatusUpdate) (statusReads, error) {
	var reads statusReads
	if !update.IsDriftCheck {
		return reads, nil
	}
	runtimeConfig, err := p.crdClient.HaproxyTemplateICV1alpha1().
		HAProxyCfgs(update.RuntimeConfigNamespace).
		Get(ctx, update.RuntimeConfigName, metav1.GetOptions{})
	switch {
	case err == nil:
		reads.runtimeConfig = runtimeConfig
	case apierrors.IsNotFound(err):
		return reads, fmt.Errorf("%w: %s/%s", ErrRuntimeConfigNotPublished,
			update.RuntimeConfigNamespace, update.RuntimeConfigName)
	default:
		p.logger.Debug("Reading HAProxyCfg for drift verification failed; applying pod status unverified",
			"runtime_config", update.RuntimeConfigName, "error", err)
	}
	return reads, nil
}

// listDriftChildren lists each referenced auxiliary kind once. A kind it could
// not list stays unverified.
func (p *Publisher) listDriftChildren(ctx context.Context, update *DeploymentStatusUpdate, auxFiles *haproxyv1alpha1.AuxiliaryFileReferences, reads *statusReads) {
	reads.children = newChildListing(update.RuntimeConfigNamespace, runtimeConfigSelector(update.RuntimeConfigName))
	for _, group := range []struct {
		kind string
		refs []haproxyv1alpha1.ResourceReference
	}{
		{kindMapFile, auxFiles.MapFiles},
		{kindGeneralFile, auxFiles.GeneralFiles},
		{kindCRTListFile, auxFiles.CRTListFiles},
	} {
		if len(group.refs) == 0 {
			continue
		}
		if err := p.listInto(ctx, reads.children, []string{group.kind}); err != nil {
			p.logger.Debug("Listing auxiliary files for drift verification failed; applying pod status unverified",
				"runtime_config", update.RuntimeConfigName, "kind", group.kind, "error", err)
		}
	}
}

// existingPodStatus returns the status currently recorded for the update's pod
// in the HAProxyCfg: from live when read, else lister cache first, API fallback.
func (p *Publisher) existingPodStatus(ctx context.Context, update *DeploymentStatusUpdate, live *haproxyv1alpha1.HAProxyCfg) (haproxyv1alpha1.PodDeploymentStatus, bool) {
	find := func(cfg *haproxyv1alpha1.HAProxyCfg) (haproxyv1alpha1.PodDeploymentStatus, bool) {
		for i := range cfg.Status.DeployedToPods {
			if cfg.Status.DeployedToPods[i].PodName == update.PodName {
				return cfg.Status.DeployedToPods[i], true
			}
		}
		return haproxyv1alpha1.PodDeploymentStatus{}, false
	}
	if live != nil {
		return find(live)
	}
	if p.listers != nil && p.listers.HAProxyCfgs != nil {
		if cfg, err := p.listers.HAProxyCfgs.HAProxyCfgs(update.RuntimeConfigNamespace).Get(update.RuntimeConfigName); err == nil {
			return find(cfg)
		}
	}
	cfg, err := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs(update.RuntimeConfigNamespace).Get(ctx, update.RuntimeConfigName, metav1.GetOptions{})
	if err != nil {
		return haproxyv1alpha1.PodDeploymentStatus{}, false
	}
	return find(cfg)
}

// resolveAuxiliaryFileReferences finds the auxiliary file references of the
// publication the pod runs: the committed set of a HAProxyCfg whose
// spec.checksum is the pod's. It uses live when read, else the lister cache
// first and the typed client when the cache misses or shows another publication.
func (p *Publisher) resolveAuxiliaryFileReferences(ctx context.Context, update *DeploymentStatusUpdate, live *haproxyv1alpha1.HAProxyCfg) (*haproxyv1alpha1.AuxiliaryFileReferences, error) {
	if live == nil && p.listers != nil && p.listers.HAProxyCfgs != nil {
		if cached, err := p.listers.HAProxyCfgs.HAProxyCfgs(update.RuntimeConfigNamespace).Get(update.RuntimeConfigName); err == nil {
			if refs, ok := referencesAtChecksum(cached, update.Checksum); ok {
				return refs, nil
			}
		}
	}
	if live == nil {
		runtimeConfig, err := p.crdClient.HaproxyTemplateICV1alpha1().
			HAProxyCfgs(update.RuntimeConfigNamespace).
			Get(ctx, update.RuntimeConfigName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting HAProxyCfg: %w", err)
		}
		live = runtimeConfig
	}
	refs, ok := referencesAtChecksum(live, update.Checksum)
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s has not published checksum %q with its auxiliary references", ErrRuntimeConfigNotPublished,
			update.RuntimeConfigNamespace, update.RuntimeConfigName, update.Checksum)
	}
	return refs, nil
}

// referencesAtChecksum returns cfg's committed references when cfg publishes
// the content a pod reports at checksum. Another publication's set may hold
// files the pod never ran, so an entry there would claim content it lacks.
func referencesAtChecksum(cfg *haproxyv1alpha1.HAProxyCfg, checksum string) (*haproxyv1alpha1.AuxiliaryFileReferences, bool) {
	if cfg.Spec.Checksum != checksum {
		return nil, false
	}
	return committedReferences(cfg)
}

// applyPodStatusToRuntimeConfig SSA-applies this pod's status entry to
// HAProxyCfg. Field manager is per-pod (`haptic-pod-status-<podName>`) so
// concurrent writes for different pods merge cleanly via listType=map.
func (p *Publisher) applyPodStatusToRuntimeConfig(ctx context.Context, update *DeploymentStatusUpdate, key stampKey, podStatus *haproxyv1alpha1.PodDeploymentStatus, check stampCheck) error {
	ssaBytes, err := buildPodStatusSSAPayload(runtimeConfigKind, update.RuntimeConfigName, update.RuntimeConfigNamespace, podStatus)
	if err != nil {
		return err
	}
	client := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCfgs(update.RuntimeConfigNamespace)
	err = p.stampPodStatus(key, ssaBytes, check, func(data []byte, opts metav1.PatchOptions) (metav1.Object, error) {
		return client.Patch(ctx, update.RuntimeConfigName, types.ApplyPatchType, data, opts, statusSubresource)
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Surface the miss so the caller retries once the resource is
			// published. This was previously swallowed as success, which
			// permanently lost the pod's entry (see ErrRuntimeConfigNotPublished).
			return fmt.Errorf("%w: %s/%s", ErrRuntimeConfigNotPublished,
				update.RuntimeConfigNamespace, update.RuntimeConfigName)
		}
		return fmt.Errorf("ssa pod status on HAProxyCfg: %w", err)
	}
	return nil
}

// applyPodStatusToAuxiliaryFiles SSA-applies this pod's status entry to
// every child auxiliary file and returns the keys it covered.
//
// Each auxiliary file has its OWN content checksum (spec.checksum). The
// pod's status entry on that aux file records that aux file's checksum,
// not the main HAProxyCfg's. This matches the pre-SSA behaviour and lets
// operators see "did pod X get aux file Y?" independently of the main
// config — a content-unchanged aux file keeps its checksum across main
// config updates instead of being rewritten on every reconcile.
//
// Errors are logged but don't fail the whole status update — the next
// reconcile retries any failures.
func (p *Publisher) applyPodStatusToAuxiliaryFiles(ctx context.Context, auxFiles *haproxyv1alpha1.AuxiliaryFileReferences, update *DeploymentStatusUpdate, reads *statusReads) map[stampKey]struct{} {
	live := make(map[stampKey]struct{}, len(auxFiles.MapFiles)+len(auxFiles.GeneralFiles)+len(auxFiles.CRTListFiles)+1)
	var stamps errgroup.Group
	stamps.SetLimit(auxiliaryPublishConcurrency)
	stamp := func(kind string, ref haproxyv1alpha1.ResourceReference, lookup checksumLookup, patch statusPatcher) {
		key := stampKey{kind: kind, namespace: ref.Namespace, name: ref.Name, podName: update.PodName}
		live[key] = struct{}{}
		stamps.Go(func() error {
			p.stampAuxiliaryFile(ctx, key, update, reads.listed(key), lookup, patch)
			return nil
		})
	}

	client := p.crdClient.HaproxyTemplateICV1alpha1()
	for _, ref := range auxFiles.MapFiles {
		stamp(kindMapFile, ref, p.lookupMapFileChecksum, func(ctx context.Context, name string, data []byte, opts metav1.PatchOptions) (metav1.Object, error) {
			return client.HAProxyMapFiles(ref.Namespace).Patch(ctx, name, types.ApplyPatchType, data, opts, statusSubresource)
		})
	}
	for _, ref := range auxFiles.GeneralFiles {
		stamp(kindGeneralFile, ref, p.lookupGeneralFileChecksum, func(ctx context.Context, name string, data []byte, opts metav1.PatchOptions) (metav1.Object, error) {
			return client.HAProxyGeneralFiles(ref.Namespace).Patch(ctx, name, types.ApplyPatchType, data, opts, statusSubresource)
		})
	}
	for _, ref := range auxFiles.CRTListFiles {
		stamp(kindCRTListFile, ref, p.lookupCRTListFileChecksum, func(ctx context.Context, name string, data []byte, opts metav1.PatchOptions) (metav1.Object, error) {
			return client.HAProxyCRTListFiles(ref.Namespace).Patch(ctx, name, types.ApplyPatchType, data, opts, statusSubresource)
		})
	}
	_ = stamps.Wait()
	return live
}

type checksumLookup func(ctx context.Context, namespace, name string) (string, bool)

type statusPatcher func(ctx context.Context, name string, data []byte, opts metav1.PatchOptions) (metav1.Object, error)

// stampAuxiliaryFile applies the pod's entry to one referenced auxiliary file.
// listed is the drift check's live copy, nil when there is none.
func (p *Publisher) stampAuxiliaryFile(ctx context.Context, key stampKey, update *DeploymentStatusUpdate, listed metav1.Object, lookup checksumLookup, patch statusPatcher) {
	checksum, ok := listedChecksum(listed)
	if !ok {
		checksum, ok = lookup(ctx, key.namespace, key.name)
	}
	if !ok {
		return
	}
	entry := haproxyv1alpha1.PodDeploymentStatus{
		PodName: update.PodName, PodUID: update.PodUID, PodRuntimeID: update.PodRuntimeID, Checksum: checksum,
	}
	ssaBytes, err := buildPodStatusSSAPayload(key.kind, key.name, key.namespace, &entry)
	if err != nil {
		p.logger.Debug("Ssa payload build failed", "kind", key.kind, "name", key.name, "error", err)
		return
	}
	check := stampCheck{elideUnchanged: !update.IsDriftCheck, observed: podStatusFootprint(listed, update.PodName)}
	err = p.stampPodStatus(key, ssaBytes, check, func(data []byte, opts metav1.PatchOptions) (metav1.Object, error) {
		return patch(ctx, key.name, data, opts)
	})
	if err != nil && !apierrors.IsNotFound(err) {
		p.logger.Debug("Ssa pod status on auxiliary file failed", "kind", key.kind, "name", key.name, "error", err)
	}
}

// stampPodStatus SSA-applies one pod's status payload to one object unless
// check shows the apply would store nothing, and records a successful apply.
// A failed or raced apply is not recorded, so the next update applies again.
func (p *Publisher) stampPodStatus(key stampKey, payload []byte, check stampCheck, apply func([]byte, metav1.PatchOptions) (metav1.Object, error)) error {
	last, ok, gen := p.statusStamps.beginStamp(key)
	if ok && last.payload == string(payload) && check.skips(last) {
		return nil
	}
	applied, err := apply(payload, metav1.PatchOptions{FieldManager: podStatusFieldManager(key.podName), Force: new(true)})
	if err != nil {
		return err
	}
	p.statusStamps.commitStamp(key, stampedEntry{payload: string(payload), footprint: podStatusFootprint(applied, key.podName)}, gen)
	return nil
}

// podStatusFootprint is what a pod's status apply leaves on obj: its UID, the
// pod's entry, and the pod field manager's managed fields. Any write to them,
// an ownership change included, alters it. "" when obj lacks the entry.
func podStatusFootprint(obj metav1.Object, podName string) string {
	pods, ok := podStatusesOf(obj)
	if !ok {
		return ""
	}
	footprint := struct {
		UID     types.UID
		Entry   *haproxyv1alpha1.PodDeploymentStatus
		Managed []metav1.ManagedFieldsEntry
	}{UID: obj.GetUID()}
	for i := range pods {
		if pods[i].PodName == podName {
			footprint.Entry = &pods[i]
			break
		}
	}
	if footprint.Entry == nil {
		return ""
	}
	manager := podStatusFieldManager(podName)
	for _, managed := range obj.GetManagedFields() {
		if managed.Manager == manager {
			footprint.Managed = append(footprint.Managed, managed)
		}
	}
	encoded, err := json.Marshal(footprint)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func podStatusesOf(obj metav1.Object) ([]haproxyv1alpha1.PodDeploymentStatus, bool) {
	switch o := obj.(type) {
	case *haproxyv1alpha1.HAProxyCfg:
		if o != nil {
			return o.Status.DeployedToPods, true
		}
	case *haproxyv1alpha1.HAProxyMapFile:
		if o != nil {
			return o.Status.DeployedToPods, true
		}
	case *haproxyv1alpha1.HAProxyGeneralFile:
		if o != nil {
			return o.Status.DeployedToPods, true
		}
	case *haproxyv1alpha1.HAProxyCRTListFile:
		if o != nil {
			return o.Status.DeployedToPods, true
		}
	}
	return nil, false
}

func listedChecksum(obj metav1.Object) (string, bool) {
	switch o := obj.(type) {
	case *haproxyv1alpha1.HAProxyMapFile:
		return o.Spec.Checksum, true
	case *haproxyv1alpha1.HAProxyGeneralFile:
		return o.Spec.Checksum, true
	case *haproxyv1alpha1.HAProxyCRTListFile:
		return o.Spec.Checksum, true
	}
	return "", false
}

// lookupMapFileChecksum returns the aux file's spec.checksum (cache first,
// API fallback). Returns (checksum, true) on success, ("", false) if the
// file isn't yet visible — the next reconcile retries.
func (p *Publisher) lookupMapFileChecksum(ctx context.Context, namespace, name string) (string, bool) {
	if p.listers != nil && p.listers.MapFiles != nil {
		if obj, err := p.listers.MapFiles.HAProxyMapFiles(namespace).Get(name); err == nil {
			return obj.Spec.Checksum, true
		}
	}
	obj, err := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyMapFiles(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", false
	}
	return obj.Spec.Checksum, true
}

// lookupGeneralFileChecksum: same shape as lookupMapFileChecksum.
func (p *Publisher) lookupGeneralFileChecksum(ctx context.Context, namespace, name string) (string, bool) {
	if p.listers != nil && p.listers.GeneralFiles != nil {
		if obj, err := p.listers.GeneralFiles.HAProxyGeneralFiles(namespace).Get(name); err == nil {
			return obj.Spec.Checksum, true
		}
	}
	obj, err := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyGeneralFiles(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", false
	}
	return obj.Spec.Checksum, true
}

// lookupCRTListFileChecksum: same shape as lookupMapFileChecksum.
func (p *Publisher) lookupCRTListFileChecksum(ctx context.Context, namespace, name string) (string, bool) {
	if p.listers != nil && p.listers.CRTListFiles != nil {
		if obj, err := p.listers.CRTListFiles.HAProxyCRTListFiles(namespace).Get(name); err == nil {
			return obj.Spec.Checksum, true
		}
	}
	obj, err := p.crdClient.HaproxyTemplateICV1alpha1().HAProxyCRTListFiles(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", false
	}
	return obj.Spec.Checksum, true
}

// buildPodStatusSSAPayload constructs the minimal Server-Side Apply payload
// containing exactly this pod's status entry. The payload claims ownership
// only of the single deployedToPods[].podName=<this pod> entry — every
// other field on the object (and every other entry in the list) is owned
// by some other field manager and untouched by this apply.
func buildPodStatusSSAPayload(kind, name, namespace string, podStatus *haproxyv1alpha1.PodDeploymentStatus) ([]byte, error) {
	// The status entry's fields need to be marshaled by name (matching the
	// CRD's openAPI schema) rather than as a typed Go struct, so the
	// resulting payload can be applied via the dynamic ApplyPatchType
	// codepath without referencing the typed scheme.
	entry := map[string]any{
		"podName": podStatus.PodName,
	}
	if podStatus.PodUID != "" {
		entry["podUID"] = podStatus.PodUID
	}
	if podStatus.PodRuntimeID != "" {
		entry["podRuntimeID"] = podStatus.PodRuntimeID
	}
	if podStatus.Checksum != "" {
		entry["checksum"] = podStatus.Checksum
	}
	if podStatus.AppliedPlanID != "" {
		entry["appliedPlanID"] = podStatus.AppliedPlanID
	}
	if podStatus.RunningPlanID != "" {
		entry["runningPlanID"] = podStatus.RunningPlanID
	}
	if podStatus.Mode != "" {
		entry["mode"] = podStatus.Mode
	}
	if len(podStatus.Reasons) > 0 {
		entry["reasons"] = podStatus.Reasons
	}
	if podStatus.LastError != "" {
		entry["lastError"] = podStatus.LastError
	}
	if podStatus.ConsecutiveErrors > 0 {
		entry["consecutiveErrors"] = podStatus.ConsecutiveErrors
	}

	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}

	payload := map[string]any{
		"apiVersion": apiVersionV1Alpha1,
		"kind":       kind,
		"metadata":   metadata,
		statusSubresource: map[string]any{
			"deployedToPods": []any{entry},
		},
	}

	return json.Marshal(payload)
}
