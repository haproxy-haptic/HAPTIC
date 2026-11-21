// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

//go:build e2e

package e2e

import (
	"context"
	"io"
	"testing"
	"time"

	grpcbin "github.com/moul/pb/grpcbin/go-grpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/grpcclient"
)

func TestHapticWAFGRPCStreaming(t *testing.T) {
	t.Parallel()
	const host = "waf-grpc-streaming.localdev.me"
	feature := features.New("gRPC streams through a metadata-inspecting WAF").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			const image = "kong/grpcbin@sha256:c3033da93145d15e856b32b347c2c80c2fcc4bb323fb1460180c1e0336db22a9"
			require.NoError(t, loadLocalOrPulledImageIntoKind(ctx, image))
			backend := BackendRef{Service: "grpcbin", Port: 9000}
			require.NoError(t, applyNamedEchoServerBackend(ctx, client, ns, backend, 1, func(dep *appsv1.Deployment) {
				container := &dep.Spec.Template.Spec.Containers[0]
				container.Image = image
				container.Ports[0].ContainerPort = backend.Port
				container.ReadinessProbe.ProbeHandler = corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(backend.Port)}}
			}))
			require.NoError(t, waitForServiceEndpointReady(ctx, client, ns, backend.Service))
			NewTLSSecret(ctx, t, client, ns, "grpc-cert", []string{host})
			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name: "grpc", Host: host, BackendService: backend.Service, BackendPort: backend.Port, TLSSecretName: "grpc-cert",
				Annotations: map[string]string{
					"haproxy-haptic.org/backend-protocol":  "grpc",
					"haproxy-haptic.org/waf-policy":        "grpc-api",
					"haproxy-haptic.org/request-buffering": "off",
				},
			})
			waitForSnippetOnPod(ctx, t, []string{expectedCorazaLibrarySetting(), `[plugins.params.applications."policy:grpc-api"]`})
			return ctx
		}).
		Assess("all RPC shapes progress and rejected metadata stays at the proxy", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			t.Helper()
			rpcCtx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			conn, err := grpcclient.New(t).Dial(rpcCtx, host)
			require.NoError(t, err)
			defer conn.Close()
			client := grpcbin.NewGRPCBinClient(conn)
			msg := &grpcbin.DummyMessage{FString: "first"}
			reply, err := client.DummyUnary(rpcCtx, msg)
			require.NoError(t, err)
			require.Equal(t, msg.FString, reply.FString)

			server, err := client.DummyServerStream(rpcCtx, msg)
			require.NoError(t, err)
			for range 10 {
				reply, err := server.Recv()
				require.NoError(t, err)
				require.Equal(t, msg.FString, reply.FString)
			}
			_, err = server.Recv()
			require.ErrorIs(t, err, io.EOF)

			upload, err := client.DummyClientStream(rpcCtx)
			require.NoError(t, err)
			for i := range int32(10) {
				require.NoError(t, upload.Send(&grpcbin.DummyMessage{FInt32: i}))
			}
			// grpcbin replies to message ten. Reading before CloseSend proves the
			// backend processed the upload while the request body was still open.
			var last grpcbin.DummyMessage
			require.NoError(t, upload.RecvMsg(&last))
			require.EqualValues(t, 9, last.FInt32)
			require.NoError(t, upload.CloseSend())

			chat, err := client.DummyBidirectionalStreamStream(rpcCtx)
			require.NoError(t, err)
			for _, value := range []string{"first", "second"} {
				require.NoError(t, chat.Send(&grpcbin.DummyMessage{FString: value}))
				reply, err := chat.Recv()
				require.NoError(t, err)
				require.Equal(t, value, reply.FString)
			}
			require.NoError(t, chat.CloseSend())
			_, err = chat.Recv()
			require.ErrorIs(t, err, io.EOF)

			denied := metadata.AppendToOutgoingContext(rpcCtx, "x-waf-probe", "reject")
			requireGRPCWAFRuleDenial(ctx, t, host, "/grpcbin.GRPCBin/DummyUnary", func() error {
				_, err := client.DummyUnary(denied, msg)
				return err
			})

			// The policy extends the CRS media-type list only for application/grpc.
			requireGRPCWAFRuleDenial(ctx, t, host, "/grpcbin.GRPCBin/Empty", func() error {
				_, err := client.Empty(rpcCtx, &grpcbin.EmptyMessage{}, grpc.CallContentSubtype("proto"))
				return err
			})
			return ctx
		})
	testEnv.Test(t, feature.Feature())
}

// An unavailable verdict has its own proxy response. Verify that refusal before
// sending one fresh probe that must exercise the actual metadata rule.
func requireGRPCWAFRuleDenial(ctx context.Context, t *testing.T, host, path string, call func() error) {
	t.Helper()
	since := time.Now()
	err := call()
	recordFor := func(httpStatus float64) map[string]any {
		return findAccessLogRecordWhere(ctx, t, since, "gRPC WAF refusal on "+path, func(r map[string]any) bool {
			return r["host"] == host && r["path"] == path && r["status"] == httpStatus
		})
	}
	if status.Code(err) == codes.Unavailable {
		record := recordFor(503)
		require.Equal(t, "waf_unavailable", record["denied_by"])
		require.Equal(t, "<NOSRV>", record["server_pod"])
		t.Logf("WAF verdict unavailable for %s, request %v refused at proxy", path, record["req_id"])
		since = time.Now()
		err = call()
	}
	require.Equal(t, codes.PermissionDenied, status.Code(err), "%s: %v", path, err)
	record := recordFor(403)
	require.Equal(t, "waf", record["denied_by"])
	require.Equal(t, "<NOSRV>", record["server_pod"])
}
