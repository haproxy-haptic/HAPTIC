//go:build e2e

package e2e

import (
	"context"
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/klient"
)

func serviceTCPAddress(ctx context.Context, t *testing.T, client klient.Client, name string, port int) string {
	t.Helper()
	service := &corev1.Service{}
	require.NoError(t, client.Resources(ControllerNamespace).Get(ctx, name, ControllerNamespace, service))
	var nodePort int32
	for _, candidate := range service.Spec.Ports {
		if int(candidate.Port) == port {
			require.Equal(t, corev1.ProtocolTCP, candidate.Protocol)
			nodePort = candidate.NodePort
			break
		}
	}
	require.Positive(t, nodePort, "Service %s has no NodePort for TCP port %d", name, port)
	node := &corev1.Node{}
	require.NoError(t, client.Resources().Get(ctx, ClusterName+"-control-plane", "", node))
	var host string
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP {
			host = address.Address
			break
		}
	}
	require.NotEmpty(t, host, "owned Kind node has no internal IP")
	return net.JoinHostPort(host, strconv.Itoa(int(nodePort)))
}
