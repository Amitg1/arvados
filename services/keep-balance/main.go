// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"git.arvados.org/arvados.git/lib/config"
	"git.arvados.org/arvados.git/lib/service"
	"git.arvados.org/arvados.git/sdk/go/arvados"
	"git.arvados.org/arvados.git/sdk/go/ctxlog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

func main() {
	os.Exit(runCommand(os.Args[0], os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func runCommand(prog string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	logger := ctxlog.FromContext(context.Background())

	var options RunOptions
	flags := flag.NewFlagSet(prog, flag.ExitOnError)
	flags.BoolVar(&options.Once, "once", false,
		"balance once and then exit")
	flags.BoolVar(&options.CommitPulls, "commit-pulls", false,
		"send pull requests (make more replicas of blocks that are underreplicated or are not in optimal rendezvous probe order)")
	flags.BoolVar(&options.CommitTrash, "commit-trash", false,
		"send trash requests (delete unreferenced old blocks, and excess replicas of overreplicated blocks)")
	flags.Bool("version", false, "Write version information to stdout and exit 0")
	dumpFlag := flags.Bool("dump", false, "dump details for each block to stdout")

	loader := config.NewLoader(os.Stdin, logger)
	loader.SetupFlags(flags)

	munged := loader.MungeLegacyConfigArgs(logger, args, "-legacy-keepbalance-config")
	flags.Parse(munged)

	if *dumpFlag {
		dumper := logrus.New()
		dumper.Out = os.Stdout
		dumper.Formatter = &logrus.TextFormatter{}
		options.Dumper = dumper
	}

	// Only pass along the version flag, which gets handled in RunCommand
	args = nil
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "version" {
			args = append(args, "-"+f.Name, f.Value.String())
		}
	})

	return service.Command(arvados.ServiceNameKeepbalance,
		func(ctx context.Context, cluster *arvados.Cluster, token string, registry *prometheus.Registry) service.Handler {
			if !options.Once && cluster.Collections.BalancePeriod == arvados.Duration(0) {
				return service.ErrorHandler(ctx, cluster, fmt.Errorf("cannot start service: Collections.BalancePeriod is zero (if you want to run once and then exit, use the -once flag)"))
			}

			ac, err := arvados.NewClientFromConfig(cluster)
			ac.AuthToken = token
			if err != nil {
				return service.ErrorHandler(ctx, cluster, fmt.Errorf("error initializing client from cluster config: %s", err))
			}

			if options.Logger == nil {
				options.Logger = ctxlog.FromContext(ctx)
			}

			srv := &Server{
				Cluster:    cluster,
				ArvClient:  ac,
				RunOptions: options,
				Metrics:    newMetrics(registry),
				Logger:     options.Logger,
				Dumper:     options.Dumper,
			}

			go srv.run()
			return srv
		}).RunCommand(prog, args, stdin, stdout, stderr)
}
