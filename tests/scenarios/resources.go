// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

func resource(version, kind, namespace, name string) map[string]any {
	metadata := map[string]any{fieldName: name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	return map[string]any{"apiVersion": version, fieldKind: kind, "metadata": metadata}
}

func (s *Session) Apply(ctx context.Context, object any) error {
	content, err := json.Marshal(object)
	if err != nil {
		return err
	}
	_, err = s.KubeUnscoped(ctx, bytes.NewReader(content), "apply", "--server-side", "--field-manager=haptic-gitops-test", "-f", "-")
	return err
}

func (s *Session) Save(name string, object any) error {
	content, err := json.MarshalIndent(object, "", "  ")
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(s.Artifacts)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.WriteFile(filepath.Clean(name), append(content, '\n'), 0o600)
}

func (s *Session) RunInput(ctx context.Context, input io.Reader, name string, args ...string) (process.Result, error) {
	ctx, cancel := s.commandContext(ctx)
	defer cancel()
	result, err := s.Runner.Run(ctx, &process.Command{Name: name, Args: args, Dir: s.Root, Env: s.Cluster.Environment, Stdin: input})
	return s.record(name, args, result, err)
}

const (
	certManagerCAInjector   = "cert-manager-cainjector"
	helmUpgrade             = "upgrade"
	helmInstall             = "install"
	helmCreateNamespace     = "--create-namespace"
	loopbackHost            = "127.0.0.1"
	conditionTrue           = "True"
	providerArgo            = "argo"
	chartName               = "haptic"
	fieldName               = "name"
	fieldKind               = "kind"
	kindDeployment          = "Deployment"
	fieldEnabled            = "enabled"
	fieldChart              = "chart"
	gitopsCredentialsSecret = "gitops-credentials"
	gitopsWebhookSecret     = "gitops-webhook"
)

const certManagerWebhook = "cert-manager-webhook"
