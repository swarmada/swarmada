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

package controlstream

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// GRPCRunnable serves the FleetAdapterService ControlStream ([Server]) as a
// controller-runtime manager.Runnable, so the live telemetry feed shares the
// manager's lifecycle and graceful-shutdown signal.
//
// Transport security: when tlsCfg is non-nil the listener terminates mTLS
// (RFC-0001 §9.2.7, ADR-0025). The crypto policy — RequireAndVerifyClientCert, the
// client-CA pool, and the TLS 1.3 floor — is built by the caller
// (cmd/manager/main.go) and applied here; the runnable does not decide policy. When
// tlsCfg is nil the listener is plaintext: [Start] logs a prominent warning and that
// mode is intended for development only (reachable only via the manager's
// --fleet-adapter-insecure-authz posture).
type GRPCRunnable struct {
	addr   string
	server *Server
	log    logr.Logger
	tlsCfg *tls.Config
}

// NewGRPCRunnable builds a GRPCRunnable that will listen on addr (host:port) and
// dispatch to server. When tlsCfg is non-nil the server terminates mTLS with it;
// when nil the server is plaintext (development only).
func NewGRPCRunnable(addr string, server *Server, log logr.Logger, tlsCfg *tls.Config) *GRPCRunnable {
	return &GRPCRunnable{addr: addr, server: server, log: log, tlsCfg: tlsCfg}
}

// Start implements manager.Runnable. It listens on the configured address and
// serves until ctx is cancelled, then performs a graceful stop. It blocks for
// the server's lifetime, as the manager requires.
func (g *GRPCRunnable) Start(ctx context.Context) error {
	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", g.addr)
	if err != nil {
		return fmt.Errorf("controlstream listen on %q: %w", g.addr, err)
	}

	var gs *grpc.Server
	if g.tlsCfg != nil {
		gs = grpc.NewServer(grpc.Creds(credentials.NewTLS(g.tlsCfg)))
	} else {
		gs = grpc.NewServer()
	}
	fav1.RegisterFleetAdapterServiceServer(gs, g.server)

	if g.tlsCfg != nil {
		g.logger().Info("serving ControlStream with mTLS (RFC-0001 §9.2.7)",
			"address", lis.Addr().String())
	} else {
		g.logger().Info(
			"serving ControlStream WITHOUT mTLS (dev mode) — RFC-0001 §9.2.7 requires mTLS in production",
			"address", lis.Addr().String())
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- gs.Serve(lis) }()

	select {
	case <-ctx.Done():
		gs.GracefulStop()
		g.logger().Info("ControlStream server stopped")
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("controlstream serve: %w", err)
		}
		return nil
	}
}

// NeedLeaderElection returns false: Fleet Adapters dial in and every control-plane
// replica must accept streams, so the server runs on all replicas rather than the
// leader alone. Multi-replica Robot.status projection coordination is future work;
// the reference single-instance deployment is unaffected.
func (g *GRPCRunnable) NeedLeaderElection() bool { return false }

func (g *GRPCRunnable) logger() logr.Logger {
	if g.log.GetSink() == nil {
		return logr.Discard()
	}
	return g.log
}
