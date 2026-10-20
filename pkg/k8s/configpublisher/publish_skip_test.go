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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

// skipTestRequest returns a request with aux files so a skipped republish
// spares the whole per-file get/create sweep, not just the HAProxyCfg calls.
func skipTestRequest() PublishRequest {
	req := basePublishRequest()
	req.AuxiliaryFiles = &AuxiliaryFiles{
		MapFiles: []auxiliaryfiles.MapFile{
			{Path: "/etc/haproxy/maps/host.map", Content: "example.com backend1\n"},
			{Path: "/etc/haproxy/maps/path-prefix.map", Content: "example.com/api/ BACKEND:b1\n"},
		},
		GeneralFiles: []auxiliaryfiles.GeneralFile{
			{Filename: "400.http", Content: "HTTP/1.0 400 Bad Request\n"},
		},
	}
	return req
}

func crdActionCount(crdClient interface{ Actions() []k8stesting.Action }) int {
	return len(crdClient.Actions())
}

func TestPublishConfig_SkipsUnchangedRepublish(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	publisher.SetRepublishInterval(time.Hour)

	req := skipTestRequest()
	first, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	crdClient.ClearActions()

	again := skipTestRequest()
	second, err := publisher.PublishConfig(ctx, &again)
	require.NoError(t, err)

	assert.Zero(t, crdActionCount(crdClient),
		"an unchanged republish inside the republish interval must not touch the API")
	assert.Equal(t, first, second, "the skipped publish must return the last result")
}

func TestPublishConfig_SkipReturnsIndependentResult(t *testing.T) {
	ctx, _, _, publisher := newTestPublisher(t)
	publisher.SetRepublishInterval(time.Hour)

	req := skipTestRequest()
	first, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)
	first.MapFileNames[0] = "mutated-by-consumer"

	again := skipTestRequest()
	second, err := publisher.PublishConfig(ctx, &again)
	require.NoError(t, err)
	assert.NotEqual(t, "mutated-by-consumer", second.MapFileNames[0],
		"a consumer mutating a returned result must not poison the cached one")
}

func TestPublishConfig_RepublishesAfterInterval(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	publisher.SetRepublishInterval(time.Nanosecond)

	req := skipTestRequest()
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	crdClient.ClearActions()

	again := skipTestRequest()
	_, err = publisher.PublishConfig(ctx, &again)
	require.NoError(t, err)
	assert.NotZero(t, crdActionCount(crdClient),
		"past the republish interval the publish is the authoritative self-heal and must hit the API")
}

func TestPublishConfig_NoSkipWhenIntervalUnset(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)

	req := skipTestRequest()
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	crdClient.ClearActions()

	again := skipTestRequest()
	_, err = publisher.PublishConfig(ctx, &again)
	require.NoError(t, err)
	assert.NotZero(t, crdActionCount(crdClient),
		"skip is opt-in; a publisher without a republish interval keeps the old behavior")
}

func TestPublishConfig_RepublishesOnContentChange(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	publisher.SetRepublishInterval(time.Hour)

	req := skipTestRequest()
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	crdClient.ClearActions()

	changed := skipTestRequest()
	changed.AuxiliaryFiles.MapFiles[0].Content = "example.com backend2\n"
	_, err = publisher.PublishConfig(ctx, &changed)
	require.NoError(t, err)
	assert.NotZero(t, crdActionCount(crdClient))
}

func TestPublishConfig_RepublishesOnValidationErrorChange(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	publisher.SetRepublishInterval(time.Hour)

	req := skipTestRequest()
	req.ValidationError = "haproxy: parsing error"
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	crdClient.ClearActions()

	changed := skipTestRequest()
	changed.ValidationError = "haproxy: a different parsing error"
	_, err = publisher.PublishConfig(ctx, &changed)
	require.NoError(t, err)
	assert.NotZero(t, crdActionCount(crdClient),
		"ValidationError lands in the HAProxyCfg status; skipping its change leaves the status stale")
}

func TestPublishConfig_RepublishesOnCompressionThresholdChange(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	publisher.SetRepublishInterval(time.Hour)

	req := skipTestRequest()
	req.CompressionThreshold = 1 << 20
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	crdClient.ClearActions()

	changed := skipTestRequest()
	changed.CompressionThreshold = 1
	_, err = publisher.PublishConfig(ctx, &changed)
	require.NoError(t, err)
	assert.NotZero(t, crdActionCount(crdClient),
		"CompressionThreshold decides the stored content encoding; skipping its change leaves the spec stale")
}

func TestPublishConfig_ForceBypassesSkip(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	publisher.SetRepublishInterval(time.Hour)

	req := skipTestRequest()
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	crdClient.ClearActions()

	forced := skipTestRequest()
	forced.Force = true
	_, err = publisher.PublishConfig(ctx, &forced)
	require.NoError(t, err)
	assert.NotZero(t, crdActionCount(crdClient))
}

func TestPublishConfig_ErrorClearsPublishedState(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	publisher.SetRepublishInterval(time.Hour)

	req := skipTestRequest()
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	// A changed publish fails midway: it may already have rewritten the
	// HAProxyCfg spec, so the cached "original content is live" claim is void.
	failing := errors.New("injected update failure")
	crdClient.PrependReactor("patch", "haproxycfgs",
		func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, failing })
	changed := skipTestRequest()
	changed.Config = "global\n  daemon\n  maxconn 1\n"
	_, err = publisher.PublishConfig(ctx, &changed)
	require.Error(t, err)
	crdClient.ReactionChain = crdClient.ReactionChain[1:]

	crdClient.ClearActions()

	original := skipTestRequest()
	_, err = publisher.PublishConfig(ctx, &original)
	require.NoError(t, err)
	assert.NotZero(t, crdActionCount(crdClient),
		"after a failed publish the state is unknown; the next publish must not skip")
}

func TestPublishConfig_NameSuffixKeysStateSeparately(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	publisher.SetRepublishInterval(time.Hour)

	req := skipTestRequest()
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	crdClient.ClearActions()

	suffixed := skipTestRequest()
	suffixed.NameSuffix = "rejected"
	_, err = publisher.PublishConfig(ctx, &suffixed)
	require.NoError(t, err)
	assert.NotZero(t, crdActionCount(crdClient),
		"a suffixed publication is a different object set and must not be skipped")

	crdClient.ClearActions()

	again := skipTestRequest()
	_, err = publisher.PublishConfig(ctx, &again)
	require.NoError(t, err)
	assert.Zero(t, crdActionCount(crdClient),
		"the unsuffixed publication's state must survive a suffixed publish")
}

func TestPublishConfig_RepublishesOnOwnerUIDChange(t *testing.T) {
	ctx, _, crdClient, publisher := newTestPublisher(t)
	publisher.SetRepublishInterval(time.Hour)

	req := skipTestRequest()
	_, err := publisher.PublishConfig(ctx, &req)
	require.NoError(t, err)

	crdClient.ClearActions()

	recreated := skipTestRequest()
	recreated.TemplateConfigUID = types.UID("recreated-uid-456")
	_, err = publisher.PublishConfig(ctx, &recreated)
	require.NoError(t, err)
	assert.NotZero(t, crdActionCount(crdClient),
		"a recreated template config needs fresh owner references on every child")
}
