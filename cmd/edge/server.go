/*
Copyright 2026 The Swarmada Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/swarmada/swarmada/internal/edge"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// serveEdge serves EdgeService.EdgeStream on addr until ctx is cancelled, then
// gracefully stops. When tlsCfg is non-nil the listener enforces mTLS; when nil it
// serves plaintext and logs a prominent dev-only warning (the --insecure escape
// hatch, mirroring the ControlStream reference). The onReady callback fires once the
// listener is bound and Serve is about to run (used to flip readiness).
func serveEdge(ctx context.Context, addr string, tlsCfg *tls.Config, node *edge.Node,
	log logr.Logger, onReady func()) error {
	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("edge listen on %q: %w", addr, err)
	}
	return serveEdgeOnListener(ctx, lis, tlsCfg, node, log, onReady)
}

// serveEdgeOnListener serves on an already-bound listener. serveEdge wraps it after
// binding addr; tests call it directly with an ephemeral-port listener.
func serveEdgeOnListener(ctx context.Context, lis net.Listener, tlsCfg *tls.Config,
	node *edge.Node, log logr.Logger, onReady func()) error {
	var opts []grpc.ServerOption
	if tlsCfg != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
		log.Info("serving EdgeStream with mTLS", "address", lis.Addr().String())
	} else {
		log.Info("serving EdgeStream WITHOUT mTLS (--insecure dev mode) — "+
			"RFC-0001 #fleet-adapter-protocol-transport-security requires mTLS in production", "address", lis.Addr().String())
	}

	gs := grpc.NewServer(opts...)
	fav1.RegisterEdgeServiceServer(gs, node)

	if onReady != nil {
		onReady()
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- gs.Serve(lis) }()

	select {
	case <-ctx.Done():
		gs.GracefulStop()
		log.Info("EdgeStream server stopped")
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("edge serve: %w", err)
		}
		return nil
	}
}
