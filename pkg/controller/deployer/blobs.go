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

package deployer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	agentclient "github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/client"
)

// BlobStore is the controller's blob store as delivery uses it (ADR-0041):
// the source of every push, and where a blob taken back from a pod goes.
type BlobStore interface {
	Open(digest string) (*os.File, error)
	Ingest(ctx context.Context, body io.Reader, spec blob.Spec) (blob.Ref, error)
	MaxSize() int64
}

// errAgentStoresNoBlobs refuses to send a configuration that names blobs to a
// pod that could not hold them; the pod keeps what it runs.
var errAgentStoresNoBlobs = errors.New("the configuration needs blobs, but this pod's agent does not store them; " +
	"upgrade the HAProxy pods and enable their blobs volume")

// blobDelivery is one deployment's required blobs, and the fleet a blob the
// controller lost is taken back from.
type blobDelivery struct {
	required []string
	fleet    []dataplane.Endpoint

	mu        sync.Mutex
	recovered map[string]*blobRecovery
}

// blobRecovery takes one blob back once per deployment, however many pods
// need it.
type blobRecovery struct {
	once sync.Once
	err  error
}

// newBlobDelivery returns nil when the controller has no blob store or the
// render names no blob, so the apply path is exactly what it was without blobs.
func (c *Component) newBlobDelivery(contents map[string]string, fleet []dataplane.Endpoint) *blobDelivery {
	if c.blobs == nil {
		return nil
	}
	required := blob.ReferencedDigests(slices.Collect(maps.Values(contents))...)
	if len(required) == 0 {
		return nil
	}
	return &blobDelivery{required: required, fleet: fleet, recovered: map[string]*blobRecovery{}}
}

func (d *blobDelivery) requiredBlobs() []string {
	if d == nil {
		return nil
	}
	return d.required
}

// deliverBlobs pushes every required blob the pod does not report holding,
// before the apply that requires them.
func (c *Component) deliverBlobs(ctx context.Context, attempt *podApply) error {
	delivery := attempt.req.blobs
	if delivery == nil {
		return nil
	}
	if !slices.Contains(attempt.state.Features, api.FeatureBlobs) {
		return errAgentStoresNoBlobs
	}
	held := make(map[string]struct{}, len(attempt.state.Blobs))
	for _, reported := range attempt.state.Blobs {
		held[reported.SHA256] = struct{}{}
	}
	var missing []string
	for _, digest := range delivery.required {
		if _, ok := held[digest]; !ok {
			missing = append(missing, digest)
		}
	}
	return c.pushBlobs(ctx, attempt, missing)
}

// resendMissingBlobs answers an apply refused with missing_blobs by pushing
// the blobs it names, once per apply; resent reports that the apply should be
// sent again.
func (c *Component) resendMissingBlobs(ctx context.Context, attempt *podApply, applyErr error) (resent bool, err error) {
	var missing *agentclient.MissingBlobsError
	if !errors.As(applyErr, &missing) || attempt.blobsResent || attempt.req.blobs == nil {
		return false, nil
	}
	c.Logger().Debug("Agent is missing required blobs, pushing them",
		"pod", attempt.endpoint.PodName, "blobs", len(missing.Missing))
	attempt.blobsResent = true
	return true, c.pushBlobs(ctx, attempt, missing.Missing)
}

func (c *Component) pushBlobs(ctx context.Context, attempt *podApply, digests []string) error {
	for _, digest := range digests {
		if err := c.pushBlob(ctx, attempt, digest); err != nil {
			return err
		}
		c.noteBlobPushed(attempt.endpoint, digest)
	}
	return nil
}

func (c *Component) pushBlob(ctx context.Context, attempt *podApply, digest string) error {
	file, err := c.openBlob(ctx, attempt.req.blobs, digest)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("reading blob %s: %w", digest, err)
	}
	if err := attempt.client.PutBlob(ctx, digest, info.Size(), file); err != nil {
		return fmt.Errorf("pushing blob %s: %w", digest, err)
	}
	return nil
}

// openBlob opens a blob from the store, taking it back from a pod first when
// the store has lost it (ADR-0041 §6).
func (c *Component) openBlob(ctx context.Context, delivery *blobDelivery, digest string) (*os.File, error) {
	file, err := c.blobs.Open(digest)
	if !errors.Is(err, fs.ErrNotExist) {
		return file, err
	}
	if err := c.recoverBlob(ctx, delivery, digest); err != nil {
		return nil, err
	}
	return c.blobs.Open(digest)
}

func (c *Component) recoverBlob(ctx context.Context, delivery *blobDelivery, digest string) error {
	delivery.mu.Lock()
	recovery, started := delivery.recovered[digest]
	if !started {
		recovery = &blobRecovery{}
		delivery.recovered[digest] = recovery
	}
	delivery.mu.Unlock()
	recovery.once.Do(func() { recovery.err = c.takeBlobFromFleet(ctx, delivery.fleet, digest) })
	return recovery.err
}

// takeBlobFromFleet copies the blob from the first pod that reports it. The
// store verifies the digest, so a pod cannot hand back other content.
func (c *Component) takeBlobFromFleet(ctx context.Context, fleet []dataplane.Endpoint, digest string) error {
	var errs []error
	for i := range fleet {
		endpoint := &fleet[i]
		err := c.takeBlobFrom(ctx, endpoint, digest)
		if err == nil {
			c.Logger().Info("Took a blob the controller had lost back from a pod", "sha256", digest, "pod", endpoint.PodName)
			return nil
		}
		if !errors.Is(err, errBlobNotReported) {
			errs = append(errs, fmt.Errorf("%s: %w", endpoint.PodName, err))
		}
	}
	return fmt.Errorf("blob %s is neither on the controller nor on a pod; it arrives with its source's next fetch: %w",
		digest, errors.Join(errs...))
}

var errBlobNotReported = errors.New("the pod does not report the blob")

func (c *Component) takeBlobFrom(ctx context.Context, endpoint *dataplane.Endpoint, digest string) error {
	client, err := c.clients.For(endpoint)
	if err != nil {
		return err
	}
	state, err := client.State(ctx, api.StateRead{})
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(state.Blobs, func(held api.Blob) bool { return held.SHA256 == digest }) {
		return errBlobNotReported
	}
	body, _, err := client.GetBlob(ctx, digest)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	_, err = c.blobs.Ingest(ctx, body, blob.Spec{Unpack: blob.UnpackNone, SHA256: digest, MaxSize: c.blobs.MaxSize()})
	return err
}

// noteHeldBlobs records the blobs a pod reports, which the controller's blob
// store keeps while any pod holds them.
func (c *Component) noteHeldBlobs(endpoint *dataplane.Endpoint, held []api.Blob) {
	digests := make([]string, 0, len(held))
	for _, b := range held {
		digests = append(digests, b.SHA256)
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if len(digests) == 0 {
		delete(c.podBlobs, podKey(endpoint))
		return
	}
	c.podBlobs[podKey(endpoint)] = digests
}

func (c *Component) noteBlobPushed(endpoint *dataplane.Endpoint, digest string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	key := podKey(endpoint)
	if !slices.Contains(c.podBlobs[key], digest) {
		c.podBlobs[key] = append(c.podBlobs[key], digest)
	}
}

// retainHeldBlobs forgets the blobs of pods that left the fleet.
func (c *Component) retainHeldBlobs(endpoints []dataplane.Endpoint) {
	keep := make(map[string]struct{}, len(endpoints))
	for i := range endpoints {
		keep[podKey(&endpoints[i])] = struct{}{}
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	for key := range c.podBlobs {
		if _, ok := keep[key]; !ok {
			delete(c.podBlobs, key)
		}
	}
}

// HeldBlobs returns the sorted digests the fleet's pods last reported or were
// sent: the in-use set the controller's blob store retains.
func (c *Component) HeldBlobs() []string {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	var digests []string
	for _, held := range c.podBlobs {
		digests = append(digests, held...)
	}
	slices.Sort(digests)
	return slices.Compact(digests)
}
