// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/kindutil"
	"github.com/haproxy-haptic/HAPTIC/tests/process"
)

func runInstallKindnet(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("install-kindnet", flag.ContinueOnError)
	name := flags.String("cluster", "", "existing Kind cluster created with disableDefaultCNI")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" || flags.NArg() != 0 {
		return errors.New("install-kindnet requires --cluster <name>")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return kindutil.InstallKindnetCluster(ctx, process.Executor{}, *name, kindutil.DockerEnvironment(os.Getenv), 3*time.Minute)
}
