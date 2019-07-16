// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"context"
	"net/http"

	"git.curoverse.com/arvados.git/sdk/go/ctxlog"
	"git.curoverse.com/arvados.git/sdk/go/httpserver"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

type server struct {
	httpserver.Server
	Config *Config
}

func (srv *server) Start() error {
	h := &handler{Config: srv.Config}
	reg := prometheus.NewRegistry()
	h.Config.Cache.registry = reg
	ctx := ctxlog.Context(context.Background(), logrus.StandardLogger())
	mh := httpserver.Instrument(reg, nil, httpserver.HandlerWithContext(ctx, httpserver.AddRequestIDs(httpserver.LogRequests(h))))
	h.MetricsAPI = mh.ServeAPI(h.Config.ManagementToken, http.NotFoundHandler())
	srv.Handler = mh
	srv.Addr = srv.Config.Listen
	return srv.Server.Start()
}
