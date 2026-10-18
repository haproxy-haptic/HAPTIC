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

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"

	leaderelectionctrl "github.com/haproxy-haptic/HAPTIC/pkg/controller/leaderelection"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	crdfake "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
	k8sleaderelection "github.com/haproxy-haptic/HAPTIC/pkg/k8s/leaderelection"
)

// leaseServer is one apiserver's Lease storage behind several fake clientsets.
// Unlike the fake tracker, it refuses an update carrying a stale
// resourceVersion, the way the apiserver does.
type leaseServer struct {
	mu      sync.Mutex
	tracker k8stesting.ObjectTracker
	version int
}

func newLeaseServer() *leaseServer {
	return &leaseServer{tracker: k8sfake.NewClientset().Tracker()}
}

// client is a clientset whose Lease requests this server answers. beforeUpdate
// runs ahead of each of its updates, outside the server's lock.
func (s *leaseServer) client(beforeUpdate func()) *k8sfake.Clientset {
	client := k8sfake.NewClientset()
	client.PrependReactor("*", "leases", s.serve)
	if beforeUpdate != nil {
		client.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
			beforeUpdate()
			return false, nil, nil
		})
	}
	return client
}

func (s *leaseServer) serve(action k8stesting.Action) (bool, runtime.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resource, namespace := action.GetResource(), action.GetNamespace()
	switch action.GetVerb() {
	case "get":
		obj, err := s.tracker.Get(resource, namespace, action.(k8stesting.GetAction).GetName())
		return true, obj, err
	case "create", "update":
		lease := action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease).DeepCopy()
		if action.GetVerb() == "create" {
			s.version++
			lease.ResourceVersion = strconv.Itoa(s.version)
			return true, lease, s.tracker.Create(resource, lease, namespace)
		}
		stored, err := s.tracker.Get(resource, namespace, lease.Name)
		if err != nil {
			return true, nil, err
		}
		if stored.(*coordinationv1.Lease).ResourceVersion != lease.ResourceVersion {
			return true, nil, apierrors.NewConflict(resource.GroupResource(), lease.Name,
				errors.New("the object has been modified"))
		}
		s.version++
		lease.ResourceVersion = strconv.Itoa(s.version)
		return true, lease, s.tracker.Update(resource, lease, namespace)
	}
	return false, nil, nil
}

// renewalErrors collects what client-go's elector logs at ERROR.
type renewalErrors struct {
	mu     sync.Mutex
	errors []string
}

func (*renewalErrors) Init(logr.RuntimeInfo)    {}
func (*renewalErrors) Enabled(int) bool         { return false }
func (*renewalErrors) Info(int, string, ...any) {}

func (r *renewalErrors) Error(err error, msg string, _ ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf("%s: %v", msg, err))
}

func (r *renewalErrors) WithValues(...any) logr.LogSink { return r }
func (r *renewalErrors) WithName(string) logr.LogSink   { return r }

func (r *renewalErrors) logged() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errors...)
}

// A deployment intent written between a renewal's read and its update must not
// fail that renewal (issue #413). Every update the real client-go elector sends
// is preceded by one recorded intent, the worst interleaving a busy leader can
// produce.
func TestDeploymentIntentWritesNeverFailALeaseRenewal(t *testing.T) {
	const (
		namespace = "haptic"
		leaseName = "haptic"
		identity  = "haptic-controller-0"
		deploys   = 6
	)
	logger := testutil.NewTestLogger()
	server := newLeaseServer()
	kube := server.client(nil)
	publisher := configpublisher.NewWithListers(kube, crdfake.NewSimpleClientset(), nil, logger)

	var beforeRenewal atomic.Pointer[func()]
	electorClient := server.client(func() {
		if deploy := beforeRenewal.Load(); deploy != nil {
			(*deploy)()
		}
	})
	term := leaderelectionctrl.NewTerm(
		leaderelectionctrl.NewLeaseEpoch(kube, namespace, leaseName, identity, logger), func(string) {})
	leading := make(chan context.Context, 1)
	election, err := leaderelectionctrl.New(&k8sleaderelection.Config{
		Enabled: true, Identity: identity, LeaseName: leaseName, LeaseNamespace: namespace,
		LeaseDuration: 2 * time.Second, RenewDeadline: time.Second, RetryPeriod: 20 * time.Millisecond,
	}, electorClient, testutil.NewTestBus(), k8sleaderelection.Callbacks{
		OnStartedLeading: func(ctx context.Context) { leading <- ctx },
	}, term, logger)
	require.NoError(t, err)

	renewals := &renewalErrors{}
	ctx, cancel := context.WithTimeout(klog.NewContext(context.Background(), logr.New(renewals)), 30*time.Second)
	stopped := make(chan error, 1)
	go func() { stopped <- election.Start(ctx) }()
	t.Cleanup(func() {
		beforeRenewal.Store(nil)
		cancel()
		require.NoError(t, <-stopped)
	})

	var termCtx context.Context
	select {
	case termCtx = <-leading:
	case <-ctx.Done():
		t.Fatal("the elector never acquired the Lease")
	}
	authority := &configpublisher.DeploymentAuthority{
		Namespace: namespace, Name: "haptic", UID: "template-uid", Epoch: term.LeaderEpoch(),
		Claim: "term", LeaseName: leaseName, Identity: identity,
	}
	require.NoError(t, publisher.ClaimDeploymentAuthority(ctx, authority))

	recorded := make(chan error, deploys)
	var plans atomic.Int32
	deploy := func() {
		if plan := plans.Add(1); plan <= deploys {
			recorded <- publisher.RecordDeploymentIntent(ctx, authority,
				fmt.Sprintf("plan-%d", plan), fmt.Sprintf("checksum-%d", plan))
		}
	}
	beforeRenewal.Store(&deploy)
	for range deploys {
		select {
		case err := <-recorded:
			require.NoError(t, err)
		case <-termCtx.Done():
			t.Fatalf("leadership was lost; renewals logged %q", renewals.logged())
		}
	}
	beforeRenewal.Store(nil)

	require.Empty(t, renewals.logged(), "a deployment intent write failed a renewal of the leader Lease")
	require.NoError(t, termCtx.Err())
}
