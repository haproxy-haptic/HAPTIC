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
	"context"
	"errors"
	"slices"
	"strings"

	controllerhttpstore "github.com/haproxy-haptic/HAPTIC/pkg/controller/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

type exactCycleHTTPObservations struct {
	disabled  bool
	source    httpstore.SourceID
	state     *httpstore.AcceptedReplayState
	stateAuth *httpstore.AcceptedReplayState
	seal      *exactCycleHTTPObservations
	auth      exactCycleHTTPObservationAuthentication
}

type exactCycleHTTPObservationAuthentication struct {
	disabled bool
	source   httpstore.SourceID
	state    *httpstore.AcceptedReplayState
}

func newDisabledExactCycleHTTPObservations() *exactCycleHTTPObservations {
	result := &exactCycleHTTPObservations{disabled: true}
	result.auth = exactCycleHTTPObservationAuthentication{disabled: true}
	result.seal = result
	return result
}

// errExactCycleHTTPStateUnauthenticated means the replay is unavailable, not
// that the render failed; the capture path degrades to uncacheable.
var errExactCycleHTTPStateUnauthenticated = errors.New(
	"exact cycle HTTP replay state is not authenticated",
)

func newExactCycleHTTPObservations(
	source httpstore.SourceID,
	state *httpstore.AcceptedReplayState,
) (*exactCycleHTTPObservations, error) {
	if !acceptedReplayStateAuthenticated(source, state) {
		return nil, errExactCycleHTTPStateUnauthenticated
	}
	result := &exactCycleHTTPObservations{
		source: source, state: state, stateAuth: state,
	}
	result.auth = exactCycleHTTPObservationAuthentication{
		disabled: result.disabled, source: result.source, state: result.state,
	}
	result.seal = result
	return result, nil
}

func acceptedReplayStateAuthenticated(
	source httpstore.SourceID,
	state *httpstore.AcceptedReplayState,
) bool {
	return source != 0 && state != nil && state.Source() == source &&
		state.ValidateAuthentication() == nil
}

func captureExactCycleHTTPObservations(
	wrapper *controllerhttpstore.HTTPStoreWrapper,
	component *controllerhttpstore.Component,
) (*exactCycleHTTPObservations, error) {
	observations, _, _, err := captureExactCycleHTTPObservationsForSession(wrapper, component, nil)
	return observations, err
}

func captureExactCycleHTTPObservationsForSession(
	wrapper *controllerhttpstore.HTTPStoreWrapper,
	component *controllerhttpstore.Component,
	session *incrementalRenderSession,
) (*exactCycleHTTPObservations, []httpstore.ContentSnapshot, bool, error) {
	if wrapper == nil || component == nil {
		if wrapper != nil || component != nil {
			return nil, nil, false, nil
		}
		return newDisabledExactCycleHTTPObservations(), nil, true, nil
	}
	source := component.RevisionSource()
	if source == 0 || wrapper.RevisionSource() != source {
		return nil, nil, false, nil
	}
	snapshots, cacheable := exactCycleHTTPSnapshots(wrapper, session)
	if !cacheable {
		return nil, snapshots, false, nil
	}
	committed, transactionCommitted := wrapper.CommittedAcceptedReplayState()
	if transactionCommitted {
		if !exactCycleHTTPStateMatchesSnapshots(committed, snapshots) {
			return nil, snapshots, false, nil
		}
		return captureExactCycleHTTPReplay(source, committed, snapshots)
	}
	state, ok := wrapper.CaptureAcceptedReplayState(snapshots)
	if !ok {
		return nil, snapshots, true, nil
	}
	return captureExactCycleHTTPReplay(source, state, snapshots)
}

func captureExactCycleHTTPReplay(
	source httpstore.SourceID,
	state *httpstore.AcceptedReplayState,
	snapshots []httpstore.ContentSnapshot,
) (*exactCycleHTTPObservations, []httpstore.ContentSnapshot, bool, error) {
	observations, err := newExactCycleHTTPObservations(source, state)
	if errors.Is(err, errExactCycleHTTPStateUnauthenticated) {
		return nil, snapshots, false, nil
	}
	if err != nil {
		return nil, snapshots, false, err
	}
	return observations, snapshots, true, nil
}

func exactCycleHTTPSnapshots(
	wrapper *controllerhttpstore.HTTPStoreWrapper,
	session *incrementalRenderSession,
) ([]httpstore.ContentSnapshot, bool) {
	snapshots, _ := wrapper.ContentSnapshots()
	byURL := make(map[string]httpstore.ContentSnapshot, len(snapshots))
	for index := range snapshots {
		byURL[snapshots[index].URL] = snapshots[index]
	}
	reused := session.reusedRootHTTPSnapshots()
	for index := range reused {
		if _, fetched := byURL[reused[index].URL]; !fetched {
			byURL[reused[index].URL] = reused[index]
		}
	}
	if session != nil && !session.exactCycleFullCold() {
		session.httpMu.Lock()
		observed := make([]incremental.InputKey, 0, len(session.httpObserved))
		for key := range session.httpObserved {
			observed = append(observed, key)
		}
		session.httpMu.Unlock()
		for _, key := range observed {
			spec, exists := session.state.httpInputSpec(key)
			if exists {
				delete(byURL, spec.url)
			}
		}
	}
	urls := make([]string, 0, len(byURL))
	for url := range byURL {
		urls = append(urls, url)
	}
	slices.Sort(urls)
	result := make([]httpstore.ContentSnapshot, 0, len(urls))
	cacheable := true
	for _, url := range urls {
		snapshot := byURL[url]
		result = append(result, snapshot)
		cacheable = cacheable && snapshot.Found && snapshot.Cacheable && snapshot.Token.Valid()
	}
	return result, cacheable
}

// reusedRootHTTPSnapshots returns the previous cycle's HTTP reads when this
// cycle reused a root, whose fetches therefore didn't run (#604).
func (r *incrementalRenderSession) reusedRootHTTPSnapshots() []httpstore.ContentSnapshot {
	if r == nil || r.rootReuser.reused() == 0 {
		return nil
	}
	previous := r.rootReuser.candidate.http
	if previous == nil || previous.disabled || previous.state == nil {
		return nil
	}
	return previous.state.Snapshots()
}

func exactCycleHTTPStateMatchesSnapshots(
	state *httpstore.AcceptedReplayState,
	snapshots []httpstore.ContentSnapshot,
) bool {
	if state == nil || state.ValidateAuthentication() != nil {
		return false
	}
	committed := state.Snapshots()
	if len(committed) != len(snapshots) {
		return false
	}
	slices.SortFunc(snapshots, func(left, right httpstore.ContentSnapshot) int {
		return strings.Compare(left.URL, right.URL)
	})
	for index := range committed {
		if !sameHTTPSnapshot(&committed[index], &snapshots[index]) {
			return false
		}
	}
	return true
}

func (o *exactCycleHTTPObservations) sameReplayRoot(
	wrapper *controllerhttpstore.HTTPStoreWrapper,
	component *controllerhttpstore.Component,
) (bool, error) {
	if err := o.validate(); err != nil {
		return false, err
	}
	if o.disabled {
		return wrapper == nil && component == nil, nil
	}
	if wrapper == nil || component == nil || wrapper.RevisionSource() != o.source ||
		component.RevisionSource() != o.source {
		return false, nil
	}
	advanced, ok := component.AdvanceAcceptedReplayState(o.state)
	if !ok {
		return false, nil
	}
	return advanced != nil, nil
}

func (o *exactCycleHTTPObservations) matches(
	ctx context.Context,
	wrapper *controllerhttpstore.HTTPStoreWrapper,
	component *controllerhttpstore.Component,
	_ bool,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := o.validate(); err != nil {
		return false, err
	}
	if o.disabled {
		return wrapper == nil && component == nil, nil
	}
	if wrapper == nil || component == nil || wrapper.RevisionSource() != o.source ||
		component.RevisionSource() != o.source {
		return false, nil
	}
	advanced, replayable := requireReplayableAcceptedState(wrapper, o.state)
	return replayable && advanced != nil, nil
}

// requireReplayableAcceptedState registers the state on the transaction end
// fence; a failure means the replay is unavailable, not a render error.
func requireReplayableAcceptedState(
	wrapper *controllerhttpstore.HTTPStoreWrapper,
	state *httpstore.AcceptedReplayState,
) (*httpstore.AcceptedReplayState, bool) {
	advanced, err := wrapper.RequireAcceptedReplayState(state)
	if err != nil {
		return nil, false
	}
	return advanced, true
}

func (o *exactCycleHTTPObservations) rebaseCommitted(
	wrapper *controllerhttpstore.HTTPStoreWrapper,
) (*exactCycleHTTPObservations, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	if o.disabled {
		if wrapper != nil {
			return nil, errors.New("exact cycle HTTP successor unexpectedly enabled HTTP")
		}
		return o, nil
	}
	if wrapper == nil {
		return nil, errors.New("exact cycle HTTP successor has no committed wrapper")
	}
	committed, ok := wrapper.CommittedAcceptedReplayState()
	if !ok || committed == nil || committed.ValidateAuthentication() != nil ||
		committed.Source() != o.source {
		return nil, errors.New("exact cycle HTTP successor has no committed replay state")
	}
	if committed == o.state {
		return o, nil
	}
	return newExactCycleHTTPObservations(o.source, committed)
}

func (o *exactCycleHTTPObservations) validate() error {
	if o == nil || o.seal != o || o.disabled != o.auth.disabled || o.source != o.auth.source ||
		o.state != o.auth.state ||
		o.disabled && (o.source != 0 || o.state != nil || o.stateAuth != nil) ||
		!o.disabled && (o.source == 0 || o.state == nil || o.state != o.stateAuth ||
			o.state.Source() != o.source || o.state.ValidateAuthentication() != nil) {
		return errors.New("exact cycle HTTP observations have invalid provenance")
	}
	return nil
}

func (o *exactCycleHTTPObservations) leaseState() *httpstore.AcceptedReplayState {
	if o == nil || o.disabled {
		return nil
	}
	return o.state
}
