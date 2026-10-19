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

package configpublisher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/fake"
	configpublisher "github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
)

func canceledRequest() error {
	return &url.Error{Op: "Get", URL: "https://apiserver/apis/haproxy-haptic.org", Err: context.Canceled}
}

func TestOnlyCanceled(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil"},
		{name: "bare", err: context.Canceled, want: true},
		{name: "wrapped request", err: fmt.Errorf("listing HAProxyCfgs: %w", canceledRequest()), want: true},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "api error", err: apierrors.NewInternalError(errors.New("etcd"))},
		{name: "all joined causes canceled", err: errors.Join(canceledRequest(), fmt.Errorf("b: %w", context.Canceled)), want: true},
		{name: "one joined cause is real", err: errors.Join(canceledRequest(), apierrors.NewInternalError(errors.New("etcd")))},
		{name: "real cause behind a wrapped join", err: fmt.Errorf("x: %w", errors.Join(context.Canceled, errors.New("real")))},
		{name: "empty join", err: errors.Join()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, onlyCanceled(tc.err))
		})
	}
}

const reconcilePodsFailedLog = "Failed to reconcile deployed pods status"

func apiFailureLogComponent(t *testing.T, cfgNames ...string) (*Component, *fake.Clientset, *testutil.LogRecorder) {
	t.Helper()
	objects := make([]runtime.Object, 0, len(cfgNames))
	for _, name := range cfgNames {
		objects = append(objects, &v1alpha1.HAProxyCfg{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "haptic"}})
	}
	crdClient := fake.NewSimpleClientset(objects...)
	logger, logs := testutil.NewRecordingLogger()
	c := &Component{
		logger:            logger,
		publisher:         configpublisher.NewWithListers(k8sfake.NewClientset(), crdClient, nil, testutil.NewTestLogger()),
		hasTemplateConfig: true,
		templateConfig:    &v1alpha1.HAProxyTemplateConfig{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Namespace: "haptic"}},
	}
	return c, crdClient, logs
}

func failVerb(client *fake.Clientset, verb string, errFor func(name string) error) {
	client.PrependReactor(verb, "haproxycfgs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := ""
		if get, ok := action.(k8stesting.GetAction); ok {
			name = get.GetName()
		}
		if err := errFor(name); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
}

func lifecycleContext(t *testing.T, ended bool) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if ended {
		cancel()
	}
	return ctx
}

func TestHandlePodsDiscovered_FailureLogLevel(t *testing.T) {
	realErr := apierrors.NewInternalError(errors.New("etcd unavailable"))
	for _, tc := range []struct {
		name           string
		lifecycleEnded bool
		listErr        error
		getErr         map[string]error
		want           slog.Level
	}{
		{name: "list canceled by teardown", lifecycleEnded: true, listErr: canceledRequest(), want: slog.LevelInfo},
		{name: "list fails during teardown", lifecycleEnded: true, listErr: realErr, want: slog.LevelWarn},
		{name: "list canceled while running", listErr: canceledRequest(), want: slog.LevelWarn},
		{name: "list timed out", listErr: context.DeadlineExceeded, want: slog.LevelWarn},
		{
			name: "every HAProxyCfg canceled by teardown", lifecycleEnded: true,
			getErr: map[string]error{"cfg": canceledRequest(), "cfg-retained": canceledRequest()},
			want:   slog.LevelInfo,
		},
		{
			name: "one HAProxyCfg fails during teardown", lifecycleEnded: true,
			getErr: map[string]error{"cfg": canceledRequest(), "cfg-retained": realErr},
			want:   slog.LevelWarn,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, client, logs := apiFailureLogComponent(t, "cfg", "cfg-retained")
			if tc.listErr != nil {
				failVerb(client, "list", func(string) error { return tc.listErr })
			}
			if tc.getErr != nil {
				failVerb(client, "get", func(name string) error { return tc.getErr[name] })
			}
			event := events.NewHAProxyPodsDiscoveredEvent(
				[]dataplane.Endpoint{{PodName: "haproxy-0", PodNamespace: "haptic"}}, 1)

			c.handlePodsDiscovered(lifecycleContext(t, tc.lifecycleEnded), event)

			require.Equal(t, []slog.Level{tc.want}, logs.Levels(reconcilePodsFailedLog))
		})
	}
}

func TestHandlePodTerminated_FailureLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name           string
		lifecycleEnded bool
		listErr        error
		want           slog.Level
	}{
		{name: "canceled by teardown", lifecycleEnded: true, listErr: canceledRequest(), want: slog.LevelInfo},
		{name: "real failure during teardown", lifecycleEnded: true, listErr: errors.New("connection refused"), want: slog.LevelWarn},
		{name: "canceled while running", listErr: canceledRequest(), want: slog.LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, client, logs := apiFailureLogComponent(t)
			failVerb(client, "list", func(string) error { return tc.listErr })

			c.handlePodTerminated(lifecycleContext(t, tc.lifecycleEnded), &events.HAProxyPodTerminatedEvent{
				PodName: "haproxy-0", PodNamespace: "haptic", PodUID: "uid-0",
			})

			require.Equal(t, []slog.Level{tc.want}, logs.Levels("Failed to cleanup pod references"))
		})
	}
}

func TestProcessPendingVerdict_FailureLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name           string
		lifecycleEnded bool
		getErr         error
		want           slog.Level
	}{
		{name: "canceled by teardown", lifecycleEnded: true, getErr: canceledRequest(), want: slog.LevelInfo},
		{name: "real failure during teardown", lifecycleEnded: true, getErr: errors.New("connection refused"), want: slog.LevelWarn},
		{name: "canceled while running", getErr: canceledRequest(), want: slog.LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, client, logs := apiFailureLogComponent(t, "cfg")
			failVerb(client, "get", func(string) error { return tc.getErr })
			c.pendingVerdict = &configpublisher.GateVerdict{Namespace: "haptic", Name: "cfg", PlanID: "plan", Accepted: true}

			c.processPendingVerdict(lifecycleContext(t, tc.lifecycleEnded))

			require.Equal(t, []slog.Level{tc.want}, logs.Levels("Failed to write the render gate verdict"))
		})
	}
}
