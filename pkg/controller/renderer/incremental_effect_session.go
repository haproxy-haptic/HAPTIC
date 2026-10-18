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

package renderer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type incrementalDerivedResourceResolver struct {
	session *incrementalRenderSession
}

type incrementalQueryDerivedResourceResolver struct {
	ctx     context.Context
	reader  incremental.Reader
	session *incrementalRenderSession
	mu      sync.Mutex
	owner   incrementalDerivedOwnerResolution
	owners  map[string]incrementalDerivedOwnerResolution
	view    *rendercontext.DerivedResourceView
}

type incrementalDerivedOwnerResolution struct {
	source    string
	owner     string
	supported bool
	found     bool
	input     incremental.ImmutableInput
}

func (r *incrementalQueryDerivedResourceResolver) ResolveDerivedResource(
	identity rendercontext.DerivedResourceIdentity,
) (rendercontext.DerivedResource, bool, error) {
	found, err := r.resolveOwner(identity.Resource)
	if err != nil || !found {
		return rendercontext.DerivedResource{}, false, err
	}
	encoded, err := r.reader.Query(r.ctx, derivedProjectionQueryKey(
		identity.Resource,
		identity.Namespace,
		identity.Name,
	))
	if err != nil || len(encoded) == 0 {
		return rendercontext.DerivedResource{}, false, err
	}
	entry, err := decodeDerivedProjection(encoded)
	return entry, err == nil, err
}

func (r *incrementalQueryDerivedResourceResolver) DerivedResourceSupported(resource string) bool {
	found, err := r.resolveOwner(resource)
	return found || err != nil
}

func (r *incrementalQueryDerivedResourceResolver) project(
	resource string,
	items []any,
) ([]any, error) {
	if r == nil {
		return nil, errors.New("incremental derived-resource resolver has invalid provenance")
	}
	if len(items) == 0 {
		return items, nil
	}
	found, err := r.resolveOwner(resource)
	if err != nil {
		return nil, err
	}
	if !found {
		return items, nil
	}
	r.mu.Lock()
	if r.view == nil {
		r.view = rendercontext.NewDerivedResourceViewWithResolver(r)
	}
	view := r.view
	r.mu.Unlock()
	return view.Project(resource, items)
}

func (r *incrementalQueryDerivedResourceResolver) resolveOwner(source string) (bool, error) {
	resolution, err := r.resolveOwnerResolution(source, false)
	return resolution.found, err
}

func (r *incrementalQueryDerivedResourceResolver) resolveOwnerForProjection(
	source string,
) (incrementalDerivedOwnerResolution, error) {
	return r.resolveOwnerResolution(source, true)
}

func (r *incrementalQueryDerivedResourceResolver) resolveOwnerResolution(
	source string,
	observeCached bool,
) (incrementalDerivedOwnerResolution, error) {
	if r == nil || r.reader == nil {
		return incrementalDerivedOwnerResolution{}, errors.New(
			"incremental derived-resource owner proof has invalid provenance",
		)
	}
	r.mu.Lock()
	if resolution, resolved := r.cachedOwnerResolution(source); resolved {
		if err := resolution.authenticate(r.session, source); err != nil {
			r.mu.Unlock()
			return incrementalDerivedOwnerResolution{}, err
		}
		r.mu.Unlock()
		if observeCached {
			if err := resolution.observe(r.reader); err != nil {
				return incrementalDerivedOwnerResolution{}, err
			}
		}
		return resolution, nil
	}
	if r.session == nil || r.session.state == nil {
		r.mu.Unlock()
		return incrementalDerivedOwnerResolution{}, errors.New(
			"incremental derived-resource owner proof has no render session",
		)
	}
	_, supported := r.session.state.deriveSources[source]
	resolution := incrementalDerivedOwnerResolution{source: source, supported: supported}
	if supported {
		if err := r.resolveDerivedOwnerInput(source, &resolution); err != nil {
			r.mu.Unlock()
			return incrementalDerivedOwnerResolution{}, err
		}
	}
	if err := resolution.authenticate(r.session, source); err != nil {
		r.mu.Unlock()
		return incrementalDerivedOwnerResolution{}, err
	}
	r.cacheOwnerResolution(&resolution)
	r.mu.Unlock()
	return resolution, nil
}

func (r *incrementalQueryDerivedResourceResolver) resolveDerivedOwnerInput(
	source string,
	resolution *incrementalDerivedOwnerResolution,
) error {
	if r.session.bindingPlan == nil {
		return errors.New("incremental derived-resource owner proof has no binding plan")
	}
	component, found := r.session.bindingPlan.owners[source]
	expected := deriveOwnerInput(source, &component, found)
	observed, err := r.reader.ExactInput(expected.Key)
	if err != nil {
		return err
	}
	if observed.Key != expected.Key || observed.Revision != expected.Revision ||
		observed.Found != expected.Found || !bytes.Equal(observed.Value, expected.Value) {
		return fmt.Errorf(
			"incremental deriveResource owner for %q does not match its binding",
			source,
		)
	}
	resolution.input = incremental.ImmutableInput{
		Key: observed.Key, Revision: observed.Revision,
		Found: observed.Found, Value: string(observed.Value),
	}
	resolution.found = found
	if found {
		resolution.owner = component.name
	}
	return nil
}

func (r *incrementalDerivedOwnerResolution) observe(reader incremental.Reader) error {
	if !r.supported {
		return nil
	}
	observed, err := observeCachedIncrementalInput(
		reader, r.input.Key, r.input.Revision, r.input.Found, r.input.Value,
	)
	if err != nil || observed {
		return err
	}
	input, err := exactOwnedIncrementalInput(reader, r.input.Key)
	if err != nil {
		return err
	}
	if input.Key != r.input.Key || input.Revision != r.input.Revision ||
		input.Found != r.input.Found || !stringBytesEqual(r.input.Value, input.Value) {
		return incremental.ErrRevisionConflict
	}
	return nil
}

func (r *incrementalQueryDerivedResourceResolver) cachedOwnerResolution(
	source string,
) (incrementalDerivedOwnerResolution, bool) {
	if r.owner.source == source {
		return r.owner, true
	}
	resolution, found := r.owners[source]
	return resolution, found
}

func (r *incrementalQueryDerivedResourceResolver) cacheOwnerResolution(
	resolution *incrementalDerivedOwnerResolution,
) {
	if r.owner.source == "" {
		r.owner = *resolution
		return
	}
	if r.owners == nil {
		r.owners = map[string]incrementalDerivedOwnerResolution{}
	}
	r.owners[resolution.source] = *resolution
}

func (r *incrementalDerivedOwnerResolution) authenticate(
	session *incrementalRenderSession,
	source string,
) error {
	if session == nil || session.state == nil || r.source == "" || r.source != source {
		return errors.New("incremental derived-resource owner proof has invalid provenance")
	}
	_, supported := session.state.deriveSources[source]
	if r.supported != supported {
		return fmt.Errorf("incremental derived-resource owner proof for %q is stale", source)
	}
	if supported {
		return r.authenticateSupported(session, source)
	}
	return r.authenticateUnsupported(session, source)
}

func (r *incrementalDerivedOwnerResolution) authenticateUnsupported(
	session *incrementalRenderSession,
	source string,
) error {
	if r.found || r.owner != "" || r.input != (incremental.ImmutableInput{}) {
		return fmt.Errorf("incremental derived-resource owner proof for %q is invalid", source)
	}
	if session.bindingPlan == nil {
		return nil
	}
	if _, found := session.bindingPlan.owners[source]; found {
		return fmt.Errorf("incremental derived-resource owner proof for %q is stale", source)
	}
	return nil
}

func (r *incrementalDerivedOwnerResolution) authenticateSupported(
	session *incrementalRenderSession,
	source string,
) error {
	if session.bindingPlan == nil {
		return errors.New("incremental derived-resource owner proof has no binding plan")
	}
	component, found := session.bindingPlan.owners[source]
	if r.found != found || found && r.owner != component.name || !found && r.owner != "" {
		return fmt.Errorf("incremental derived-resource owner proof for %q is stale", source)
	}
	expected := deriveOwnerInput(source, &component, found)
	if r.input.Key != expected.Key || r.input.Revision != expected.Revision ||
		r.input.Found != expected.Found || r.input.Value != string(expected.Value) {
		return fmt.Errorf("incremental derived-resource owner proof for %q is stale", source)
	}
	if found {
		configured, exists := session.state.components[component.name]
		_, bound := session.bindingPlan.props[string(bindingKey(component.name, source))]
		if !exists || !bound || !component.deriveResource ||
			!incrementalComponentsEqual(&configured, &component) {
			return fmt.Errorf("incremental derived-resource owner proof for %q is invalid", source)
		}
	}
	return nil
}

func (r *incrementalDerivedResourceResolver) ResolveDerivedResource(
	identity rendercontext.DerivedResourceIdentity,
) (rendercontext.DerivedResource, bool, error) {
	return r.session.resolveDerivedResource(identity)
}

func (r *incrementalDerivedResourceResolver) DerivedResourceSupported(resource string) bool {
	_, supported := r.session.bindingPlan.owners[resource]
	return supported
}

func (r *incrementalRenderSession) incrementalDerivedResources(
	ctx context.Context,
	reader incremental.Reader,
) *rendercontext.DerivedResourceView {
	return rendercontext.NewDerivedResourceViewWithResolver(&incrementalQueryDerivedResourceResolver{
		ctx: ctx, reader: reader, session: r,
	})
}

func (r *incrementalRenderSession) prepareDerivedStage(ctx context.Context) error {
	keys := r.pendingDerivedOwnerQueries()
	if len(keys) == 0 {
		return r.prepareActivationStage(ctx)
	}
	runCtx := context.WithValue(ctx, incrementalRunContextKey{}, r)
	results, err := r.evaluateComponentQueries(runCtx, keys)
	if err != nil {
		return err
	}
	for start := 0; start < len(results); {
		group, end, runErr := r.derivedStageGroupRun(results, start)
		if runErr != nil {
			return runErr
		}
		if err := r.applyDerivedStageGroup(group, results[start:end]); err != nil {
			return err
		}
		start = end
	}
	return r.prepareActivationStage(ctx)
}

func (r *incrementalRenderSession) derivedStageGroupRun(
	results []incremental.ExactResult,
	start int,
) (group string, end int, err error) {
	component, ok := r.resolveQueryComponent(results[start].Key)
	if !ok {
		return "", 0, fmt.Errorf(
			"incremental derived stage returned an invalid component key %s", results[start].Key,
		)
	}
	if !component.deriveResource {
		return "", 0, fmt.Errorf("incremental derived stage returned invalid owner %q", component.name)
	}
	end = start + 1
	for end < len(results) {
		next, parsed := r.resolveQueryComponent(results[end].Key)
		if !parsed {
			return "", 0, fmt.Errorf(
				"incremental derived stage returned an invalid component key %s", results[end].Key,
			)
		}
		if !next.deriveResource {
			return "", 0, fmt.Errorf("incremental derived stage returned invalid owner %q", next.name)
		}
		if next.group != component.group {
			break
		}
		end++
	}
	return component.group, end, nil
}

func (r *incrementalRenderSession) applyDerivedStageGroup(
	group string,
	groupResults []incremental.ExactResult,
) error {
	batched, err := r.applyColdGroupAdditions(group, groupResults)
	if err != nil {
		return err
	}
	if batched {
		return nil
	}
	for index := range groupResults {
		if err := r.applyEvaluatedResult(group, &groupResults[index]); err != nil {
			return err
		}
	}
	return nil
}

func (r *incrementalRenderSession) pendingDerivedOwnerQueries() []incremental.QueryKey {
	set := make(map[incremental.QueryKey]struct{})
	add := func(key incremental.QueryKey) {
		component, source, _, _, ok := r.resolveComponentQuery(key)
		if !ok {
			return
		}
		owner, owned := r.bindingPlan.owners[source]
		if !component.deriveResource || !owned || owner.name != component.name {
			return
		}
		if _, retired := r.retired.Get([]byte(key.Opaque())); retired {
			return
		}
		set[key] = struct{}{}
	}
	for key := range r.newQueries {
		add(key)
	}
	for key := range r.dirtyQueries {
		add(key)
	}
	keys := make([]incremental.QueryKey, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(left, right incremental.QueryKey) int {
		return strings.Compare(left.Opaque(), right.Opaque())
	})
	return keys
}

func (r *incrementalRenderSession) executeDerivedProjection(
	ctx context.Context,
	reader incremental.Reader,
	source, namespace, name string,
) ([]byte, error) {
	ownerInput, err := reader.ExactInput(deriveOwnerInputKey(source))
	if err != nil {
		return nil, err
	}
	if !ownerInput.Found {
		return nil, nil
	}
	component, exists := r.state.components[string(ownerInput.Value)]
	if !exists || !component.deriveResource {
		return nil, fmt.Errorf("incremental deriveResource owner %q for %q is invalid", ownerInput.Value, source)
	}
	if _, active := r.bindingPlan.props[string(bindingKey(component.name, source))]; !active {
		return nil, fmt.Errorf("incremental deriveResource owner %q is not bound to %q", component.name, source)
	}
	encoded, err := reader.Query(ctx, r.registerComponentQuery(&component, source, namespace, name))
	if err != nil {
		return nil, err
	}
	result, err := decodeIncrementalComponentResult(encoded)
	if err != nil {
		return nil, fmt.Errorf("decoding deriveResource owner %q result: %w", component.name, err)
	}
	if len(result.Derivations) == 0 {
		return nil, nil
	}
	identity := rendercontext.DerivedResourceIdentity{Resource: source, Namespace: namespace, Name: name}
	entry := &result.Derivations[0]
	if entry.Identity != identity {
		return nil, fmt.Errorf("deriveResource owner %q returned another identity", component.name)
	}
	encoded, err = json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func decodeDerivedProjection(encoded []byte) (rendercontext.DerivedResource, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var entry rendercontext.DerivedResource
	if err := decoder.Decode(&entry); err != nil {
		return rendercontext.DerivedResource{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return rendercontext.DerivedResource{}, err
	}
	view := rendercontext.NewDerivedResourceView()
	if err := view.Replay(&entry); err != nil {
		return rendercontext.DerivedResource{}, err
	}
	return cloneDerivedResource(&entry), nil
}

func (r *incrementalRenderSession) resolveDerivedResource(
	identity rendercontext.DerivedResourceIdentity,
) (rendercontext.DerivedResource, bool, error) {
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	_, exists := r.bindingPlan.owners[identity.Resource]
	if !exists {
		return rendercontext.DerivedResource{}, false, nil
	}
	entry, found := r.derived.Get(derivedKey(identity))
	if !found {
		return rendercontext.DerivedResource{}, false, nil
	}
	return entry.materialize(), true, nil
}

func (r *incrementalRenderSession) replaceDerivations(
	key []byte,
	previous, next incremental.ExactValueRoot,
	fresh *incrementalComponentResult,
	component *incrementalComponent,
	source, namespace, name string,
) error {
	oldResult, err := decodeOptionalExactComponentResult(previous)
	if err != nil {
		return fmt.Errorf("decoding cached incremental component %q result: %w", component.name, err)
	}
	newResult := fresh
	if newResult == nil {
		decoded, err := decodeExactComponentResult(next)
		if err != nil {
			return fmt.Errorf("decoding incremental component %q result: %w", component.name, err)
		}
		newResult = &decoded
	}
	if err := validateIncrementalEffects(component, source, namespace, name, newResult); err != nil {
		return err
	}
	for index := range oldResult.Derivations {
		entry := &oldResult.Derivations[index]
		current, exists := r.derived.Get(derivedKey(entry.Identity))
		if !exists || !current.matches(entry) {
			return fmt.Errorf("incremental derived resource cache does not match result %s", incremental.DescribeTuple(string(key)))
		}
		r.derived.Delete(derivedKey(entry.Identity))
	}
	for index := range newResult.Derivations {
		entry := ownValidatedIncrementalDerivedResource(&newResult.Derivations[index])
		if current, exists := r.derived.Get(derivedKey(entry.Identity)); exists &&
			current != entry {
			return fmt.Errorf("incremental derived resource %q has conflicting owners", entry.Identity.Name)
		}
		r.derived.Insert(derivedKey(entry.Identity), entry)
	}
	return nil
}

func (r *incrementalRenderSession) deleteResult(
	component *incrementalComponent,
	source, namespace, name string,
) error {
	key := resultKey(component, source, namespace, name)
	previous, exists := r.results.Get(key)
	resultRoot := r.results.Root()
	if err := r.verifyGroupIndexResult(component, source, namespace, name, previous, exists, key); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	result, err := decodeExactComponentResult(previous)
	if err != nil {
		return fmt.Errorf("decoding removed incremental component %q result: %w", component.name, err)
	}
	index, indexed := r.groupIndexes[component.group]
	if !indexed || index == nil {
		return fmt.Errorf("incremental group %q has no assembly index", component.group)
	}
	updated, err := index.remove(component.name, source, namespace, name)
	if err != nil {
		return err
	}
	if err := r.stageIncrementalSelectorReplacement(
		component.group,
		index,
		updated,
		incrementalGroupInstanceID{
			component: component.name,
			source:    source,
			namespace: namespace,
			name:      name,
		},
		nil,
	); err != nil {
		return err
	}
	for index := range result.Derivations {
		entry := &result.Derivations[index]
		current, found := r.derived.Get(derivedKey(entry.Identity))
		if !found || !current.matches(entry) {
			return fmt.Errorf("incremental derived resource cache does not match removed result")
		}
		r.derived.Delete(derivedKey(entry.Identity))
	}
	r.results.Delete(key)
	if _, err := r.replaceHTTPEffects(key, nil); err != nil {
		return err
	}
	preparedPlan := r.preparedPlan
	if preparedPlan != nil && !r.preparedPlanBootstrapPending {
		preparedPlan, err = preparedPlan.applyGroupReplacement(
			component,
			component.group,
			index,
			updated,
			incrementalGroupInstanceID{
				component: component.name,
				source:    source,
				namespace: namespace,
				name:      name,
			},
			resultRoot,
			r.results.Root(),
		)
		if err != nil {
			return err
		}
	}
	statusPlan := r.statusPlan
	if !r.statusPlanBootstrapPending {
		statusPlan, err = replaceIncrementalStatusPatchPlanInstance(
			r.statusPlan,
			component.group,
			index,
			updated,
			incrementalGroupInstanceID{
				component: component.name,
				source:    source,
				namespace: namespace,
				name:      name,
			},
		)
		if err != nil {
			return err
		}
	}
	r.groupIndexes[component.group] = updated
	r.preparedPlan = preparedPlan
	r.statusPlan = statusPlan
	r.groupChanged[component.group] = true
	return nil
}

func (r *incrementalRenderSession) verifyGroupIndexResult(
	component *incrementalComponent,
	source, namespace, name string,
	root incremental.ExactValueRoot,
	exists bool,
	key []byte,
) error {
	index, configured := r.groupIndexes[component.group]
	if !configured || index == nil {
		return fmt.Errorf("incremental group %q has no assembly index", component.group)
	}
	if err := index.validateAuthentication(); err != nil {
		return err
	}
	id := incrementalGroupInstanceID{
		component: component.name,
		source:    source,
		namespace: namespace,
		name:      name,
	}
	indexed, indexedExists := index.instances.Root().Get(incrementalGroupInstanceKey(id))
	httpEffects, httpExists := r.httpEffects.Get(key)
	if exists != indexedExists || !exists && httpExists {
		return fmt.Errorf("incremental group %q assembly index does not match its result cache", component.group)
	}
	if !exists {
		return nil
	}
	queryKey := componentQueryKey(component, source, namespace, name)
	if err := r.validateGroupIndexResultRoot(queryKey, key, root); err != nil {
		return fmt.Errorf("authenticating cached incremental component %q result: %w", component.name, err)
	}
	encoded, err := root.String()
	if err != nil {
		return fmt.Errorf("authenticating cached incremental component %q result: %w", component.name, err)
	}
	if _, err := decodeIncrementalComponentResultString(encoded); err != nil {
		return fmt.Errorf("decoding cached incremental component %q result: %w", component.name, err)
	}
	if !httpExists {
		httpEffects = incrementalEmptyIndexedHTTPEffects
	}
	sameHTTP, err := sameIndexedHTTPEffects(indexed.httpEffects, httpEffects)
	if err != nil {
		return err
	}
	if indexed.id != id || indexed.encodedResult != encoded || !sameHTTP {
		return fmt.Errorf("incremental group %q assembly index does not match its result cache", component.group)
	}
	return nil
}

func (r *incrementalRenderSession) validateGroupIndexResultRoot(
	queryKey incremental.QueryKey,
	resultKey []byte,
	root incremental.ExactValueRoot,
) error {
	if r.base != nil {
		if r.base.preparedPlan == nil {
			return errors.New("incremental base result authentication is unavailable")
		}
		if err := r.base.preparedPlan.validateAuthentication(r.base.results.Root()); err != nil {
			return err
		}
		baseRoot, exists := r.base.results.Get(resultKey)
		if exists {
			same, err := baseRoot.SameRoot(root)
			if err != nil {
				return err
			}
			if same {
				return r.state.graph.ValidateExactValue(queryKey, root)
			}
		}
	}
	if r.graphSession == nil {
		return errors.New("incremental result has no active graph transaction")
	}
	return r.graphSession.ValidateCurrentExactValue(queryKey, root)
}

func decodeOptionalExactComponentResult(
	root incremental.ExactValueRoot,
) (incrementalComponentResult, error) {
	if root == (incremental.ExactValueRoot{}) {
		return incrementalComponentResult{}, nil
	}
	return decodeExactComponentResult(root)
}

func decodeExactComponentResult(root incremental.ExactValueRoot) (incrementalComponentResult, error) {
	encoded, err := root.String()
	if err != nil {
		return incrementalComponentResult{}, err
	}
	return decodeIncrementalComponentResultString(encoded)
}

func decodeIncrementalComponentResult(encoded []byte) (incrementalComponentResult, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var result incrementalComponentResult
	if err := decoder.Decode(&result); err != nil {
		return incrementalComponentResult{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return incrementalComponentResult{}, err
	}
	canonical, err := json.Marshal(result)
	if err != nil {
		return incrementalComponentResult{}, err
	}
	if !bytes.Equal(canonical, encoded) {
		return incrementalComponentResult{}, errors.New("incremental component result is not canonical JSON")
	}
	return result, nil
}

func decodeIncrementalComponentResultString(encoded string) (incrementalComponentResult, error) {
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var result incrementalComponentResult
	if err := decoder.Decode(&result); err != nil {
		return incrementalComponentResult{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return incrementalComponentResult{}, err
	}
	canonical, err := json.Marshal(result)
	if err != nil {
		return incrementalComponentResult{}, err
	}
	if !stringBytesEqual(encoded, canonical) {
		return incrementalComponentResult{}, errors.New("incremental component result is not canonical JSON")
	}
	return result, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("JSON value has trailing data")
	}
	return err
}

func validateIncrementalEffects(
	component *incrementalComponent,
	source, namespace, name string,
	result *incrementalComponentResult,
) error {
	if err := validateDeclaredPublishValueEffect(component, result); err != nil {
		return err
	}
	if err := validateDeclaredBackendPlanEffect(component, result); err != nil {
		return err
	}
	if len(result.Derivations) > 0 && !component.deriveResource {
		return fmt.Errorf("incremental component %q returned an undeclared deriveResource effect", component.name)
	}
	if len(result.Derivations) > 1 {
		return fmt.Errorf("incremental component %q returned multiple derivations for one source", component.name)
	}
	identity := rendercontext.DerivedResourceIdentity{Resource: source, Namespace: namespace, Name: name}
	for index := range result.Derivations {
		entry := &result.Derivations[index]
		if entry.Identity != identity {
			return fmt.Errorf("incremental component %q returned a derivation for another source", component.name)
		}
		if err := validateDerivedResource(entry); err != nil {
			return fmt.Errorf("incremental component %q returned an invalid derivation: %w", component.name, err)
		}
	}
	if len(result.Events) > 0 && !component.recordEvent {
		return fmt.Errorf("incremental component %q returned an undeclared recordEvent effect", component.name)
	}
	if (len(result.StatusPatches) > 0 || result.StatusPatchDigest != "") && !component.statusPatch {
		return fmt.Errorf("incremental component %q returned an undeclared statusPatch effect", component.name)
	}
	collector := templating.NewEventCollector()
	for index := range result.Events {
		event := &result.Events[index]
		if err := collector.Register(event.Namespace, event.Name, event.APIVersion, event.Kind,
			event.Type, event.Reason, event.Message); err != nil {
			return fmt.Errorf("incremental component %q returned an invalid event: %w", component.name, err)
		}
	}
	if !slices.Equal(collector.Events(), result.Events) {
		return fmt.Errorf("incremental component %q returned duplicate or noncanonical events", component.name)
	}
	return validateIncrementalInstanceResult(result)
}

func replayIncrementalStatusPatches(
	baseContext map[string]any,
	groupIndexes map[string]*incrementalGroupIndex,
) error {
	_, err := replayIncrementalStatusPatchPlan(
		baseContext,
		groupIndexes,
		templating.NewStatusPatchProjectionPlan(),
		nil,
	)
	return err
}

func replayIncrementalStatusPatchPlan(
	baseContext map[string]any,
	groupIndexes map[string]*incrementalGroupIndex,
	plan *templating.StatusPatchProjectionPlan,
	changed map[string]bool,
) (*templating.StatusPatchProjectionPlan, error) {
	if err := plan.ValidateAuthentication(); err != nil {
		return nil, fmt.Errorf("authenticating incremental statusPatch plan: %w", err)
	}
	next := plan
	for _, group := range incrementalStatusPatchReplayGroups(groupIndexes, changed) {
		index := groupIndexes[group]
		var compiled *templating.StatusPatchProjection
		if index != nil {
			var err error
			compiled, err = index.compiledStatusPatchProjection()
			if err != nil {
				return nil, fmt.Errorf("reading incremental group %q statusPatch effects: %w", group, err)
			}
			replay, err := compiled.PrepareReplay()
			if err != nil {
				return nil, fmt.Errorf("authenticating incremental group %q statusPatch effects: %w", group, err)
			}
			if replay.Empty() {
				compiled = nil
			}
		}
		var err error
		next, err = next.ReplaceGroup(group, compiled)
		if err != nil {
			return nil, fmt.Errorf("updating incremental statusPatch group %q: %w", group, err)
		}
	}
	return stageIncrementalStatusPatchPlan(baseContext, next)
}

func incrementalStatusPatchReplayGroups(
	groupIndexes map[string]*incrementalGroupIndex,
	changed map[string]bool,
) []string {
	groups := make([]string, 0, len(groupIndexes))
	if changed == nil {
		for group := range groupIndexes {
			groups = append(groups, group)
		}
	} else {
		for group, groupChanged := range changed {
			if groupChanged {
				groups = append(groups, group)
			}
		}
	}
	slices.Sort(groups)
	return groups
}

func stageIncrementalStatusPatchPlan(
	baseContext map[string]any,
	plan *templating.StatusPatchProjectionPlan,
) (*templating.StatusPatchProjectionPlan, error) {
	if plan == nil {
		plan = templating.NewStatusPatchProjectionPlan()
	}
	if err := plan.ValidateAuthentication(); err != nil {
		return nil, fmt.Errorf("authenticating incremental statusPatch plan: %w", err)
	}
	replay, err := plan.PrepareReplay()
	if err != nil {
		return nil, fmt.Errorf("authenticating incremental statusPatch plan replay: %w", err)
	}
	collector, _ := baseContext["statusPatchCollector"].(*templating.StatusPatchCollector)
	if collector == nil {
		if !replay.Empty() {
			return nil, errors.New("incremental statusPatch collector is unavailable")
		}
		return plan, nil
	}
	if err := collector.ReplayProjectionPlan(replay); err != nil {
		return nil, fmt.Errorf("replaying incremental statusPatch plan: %w", err)
	}
	return plan, nil
}

func validateDeclaredPublishValueEffect(
	component *incrementalComponent,
	result *incrementalComponentResult,
) error {
	if (len(result.Published) > 0 || result.PublishedDigest != "") && !component.publishValue {
		return fmt.Errorf("incremental component %q returned an undeclared publishValue effect", component.name)
	}
	if component.publishValue && (result.Text != "" || len(result.Unique) > 0) {
		return fmt.Errorf("incremental component %q cannot combine publishValue with text or shared.Unique", component.name)
	}
	return nil
}

func validateDeclaredBackendPlanEffect(
	component *incrementalComponent,
	result *incrementalComponentResult,
) error {
	if (len(result.BackendPlan) > 0 || len(result.BackendPlanOutput) > 0 || result.BackendPlanDigest != "") &&
		!component.backendPlan {
		return fmt.Errorf("incremental component %q returned an undeclared backendPlan effect", component.name)
	}
	if component.backendPlan && len(result.Unique) > 0 {
		return fmt.Errorf("incremental component %q cannot combine backendPlan with shared.Unique", component.name)
	}
	if component.backendPlan && result.Text != "" {
		return fmt.Errorf("incremental component %q returned nonlogical backendPlan output", component.name)
	}
	return nil
}

func validateDerivedResource(entry *rendercontext.DerivedResource) error {
	view := rendercontext.NewDerivedResourceView()
	if err := view.Replay(entry); err != nil {
		return err
	}
	for _, encoded := range [][]byte{entry.Source, entry.Value} {
		value, err := decodeResourceValue(encoded)
		if err != nil {
			return err
		}
		canonical, err := encodeResourceValue(value)
		if err != nil {
			return err
		}
		if !bytes.Equal(encoded, canonical) {
			return errors.New("derived resource value is not canonical JSON")
		}
	}
	return nil
}

func cloneDerivedResource(entry *rendercontext.DerivedResource) rendercontext.DerivedResource {
	return rendercontext.DerivedResource{
		Identity: entry.Identity,
		Source:   slices.Clone(entry.Source),
		Value:    slices.Clone(entry.Value),
	}
}
