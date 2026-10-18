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

//go:build acceptance

package acceptance

import (
	"bytes"
	"context"
	"testing"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/pkg/types"
)

// heapDumpHeader is the format marker runtime/debug writes first. A heap-dump
// reader keys on it, so a response that lacks it is unreadable no matter how
// large it is.
const heapDumpHeader = "go1.7 heap dump\n"

// ctxKey keeps the client out of the string keyspace the framework also uses.
type ctxKey string

const debugClientKey ctxKey = "heapdump.debugClient"

// buildHeapDumpFeature verifies /debug/heapdump on a deployed controller.
//
// The endpoint exists to answer one question — what still holds this memory —
// and it can fail at that while looking perfectly healthy: a dump written
// without collecting first is dominated by unreachable objects, and an
// unreachable object has no retainer, so a reader's "what keeps this alive"
// query comes back empty for nearly all of it. That failure reads as a broken
// analysis tool rather than a broken dump, which is exactly how it went
// unnoticed once already.
//
// The in-process test (pkg/introspection) asserts the collection happens. This
// one covers what that cannot: the endpoint is routed, reachable, and produces
// a well-formed dump from a controller running in a cluster.
func buildHeapDumpFeature() types.Feature {
	return features.New("Heap Dump Endpoint").
		Setup(setupHeapDump).
		Assess("Heap dump is served and well-formed", assessHeapDumpWellFormed).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()

			client, err := cfg.NewClient()
			if err != nil {
				t.Log("Failed to create client:", err)
				return ctx
			}
			return CleanupControllerEnvironment(ctx, t, client)
		}).
		Feature()
}

func setupHeapDump(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()

	namespace := envconf.RandomName("test-heapdump", 16)
	ctx = StoreNamespaceInContext(ctx, namespace)

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	opts := DefaultControllerEnvironmentOptions()
	if err := CreateControllerEnvironment(ctx, t, client, namespace, opts); err != nil {
		t.Fatal("Failed to create controller environment:", err)
	}

	debugClient, err := SetupDebugClient(ctx, client, Clientset(), namespace, DefaultClientSetupTimeout)
	if err != nil {
		t.Fatal("Failed to setup debug client:", err)
	}
	if _, err := debugClient.WaitForConfig(ctx, DefaultPodReadyTimeout); err != nil {
		t.Fatal("Controller did not load its configuration:", err)
	}

	return context.WithValue(ctx, debugClientKey, debugClient)
}

func assessHeapDumpWellFormed(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()

	debugClient, ok := ctx.Value(debugClientKey).(*DebugClient)
	if !ok {
		t.Fatal("debug client missing from context")
	}

	dump, err := debugClient.GetHeapDump(ctx)
	if err != nil {
		t.Fatal("Failed to fetch heap dump:", err)
	}

	if !bytes.HasPrefix(dump, []byte(heapDumpHeader)) {
		got := dump
		if len(got) > 32 {
			got = got[:32]
		}
		t.Fatalf("heap dump lacks the %q header, a reader cannot parse it; got %q",
			heapDumpHeader, got)
	}

	// A near-empty Go test binary already dumps ~960 KiB, so this floor
	// is far below any controller and far above the empty or truncated
	// body a broken handler returns.
	const minPlausibleDump = 256 << 10
	if len(dump) < minPlausibleDump {
		t.Fatalf("heap dump is %d bytes, too small to be a real heap; the handler likely wrote nothing",
			len(dump))
	}

	t.Logf("heap dump: %d bytes", len(dump))
	return ctx
}

func TestHeapDumpEndpoint(t *testing.T) {
	testEnv.Test(t, buildHeapDumpFeature())
}
