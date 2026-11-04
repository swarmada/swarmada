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

// Command edge is the Swarmada Zone Controller edge node (RFC-0001 #safety-edge-headless-emergency-stop,
// #fleet-adapter-protocol-the-edge-stream). It runs on facility-LAN hardware, serves EdgeService.EdgeStream over
// mTLS, and issues headless, confirmed-only estops on zone-boundary breaches — a
// safety path that survives a control-plane partition. The estop discipline lives in
// internal/edge.Node; this binary is the deployment around it.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/edge"
)

// version is the swarmada version stamped on local audit entries. Overridable at
// build time with -ldflags "-X main.version=...".
var version = "v0.1.0"

func main() {
	var (
		listenAddr    string
		healthAddr    string
		namespace     string
		configFile    string
		tlsCertFile   string
		tlsKeyFile    string
		tlsClientCA   string
		insecurePlain bool
		kubeconfig    string
		syncInterval  time.Duration
		safetyFile    string
		safetyActLow  bool
		safetyPoll    time.Duration
	)
	flag.StringVar(&listenAddr, "listen-address", ":9600",
		"host:port the EdgeStream gRPC server listens on for Fleet Adapter connections.")
	flag.StringVar(&healthAddr, "health-address", ":9601",
		"host:port for the /healthz and /readyz endpoints.")
	flag.StringVar(&namespace, "namespace", "",
		"namespace this edge node serves; identifies it in the local audit log (required).")
	flag.StringVar(&configFile, "config-file", "",
		"path to the edge config JSON (zone polygons + robot→zone assignments) (required).")
	flag.StringVar(&tlsCertFile, "tls-cert-file", "",
		"path to the edge server certificate (tls.crt from the edgeNode.tlsSecretRef Secret).")
	flag.StringVar(&tlsKeyFile, "tls-key-file", "",
		"path to the edge server private key (tls.key from the edgeNode.tlsSecretRef Secret).")
	flag.StringVar(&tlsClientCA, "tls-client-ca-file", "",
		"path to the namespace CA bundle (ca.crt) used to verify Fleet Adapter client certs.")
	flag.BoolVar(&insecurePlain, "insecure", false,
		"serve EdgeStream WITHOUT mTLS (dev/demo only; RFC-0001 #fleet-adapter-protocol-transport-security requires mTLS in production).")
	flag.StringVar(&kubeconfig, "kubeconfig", "",
		"path to a kubeconfig for live config sync; empty tries in-cluster config, else static config only.")
	flag.DurationVar(&syncInterval, "sync-interval", edge.DefaultSyncInterval,
		"how often to refresh zones/robots from the FleetZone and Robot resources.")
	flag.StringVar(&safetyFile, "safety-input-file", "",
		"path to a digital-input value file (0/1, e.g. a sysfs GPIO) wired to a local e-stop; empty disables it.")
	flag.BoolVar(&safetyActLow, "safety-input-active-low", true,
		"treat the safety input as active-low / normally-closed (0 or unreadable = tripped).")
	flag.DurationVar(&safetyPoll, "safety-input-interval", edge.DefaultSafetyPollInterval,
		"how often to poll the local safety input.")

	opts := zap.Options{Development: true, TimeEncoder: zapcore.ISO8601TimeEncoder}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	log := zap.New(zap.UseFlagOptions(&opts)).WithName("edge")

	if namespace == "" || configFile == "" {
		log.Info("--namespace and --config-file are required")
		os.Exit(2)
	}

	// Build the mTLS config unless explicitly opted out. mTLS is the production path
	// (RFC-0001 #fleet-adapter-protocol-transport-security); --insecure exists only for the no-hardware demo.
	var tlsCfg *tls.Config
	if insecurePlain {
		log.Info("WARNING: --insecure set; EdgeStream will accept UNAUTHENTICATED adapters")
	} else {
		if tlsCertFile == "" || tlsKeyFile == "" || tlsClientCA == "" {
			log.Info("mTLS requires --tls-cert-file, --tls-key-file and --tls-client-ca-file " +
				"(or pass --insecure for a dev run)")
			os.Exit(2)
		}
		var err error
		tlsCfg, err = serverTLSConfig(tlsCertFile, tlsKeyFile, tlsClientCA)
		if err != nil {
			log.Error(err, "failed to build edge mTLS config")
			os.Exit(1)
		}
	}

	cfg, err := loadConfig(configFile, namespace)
	if err != nil {
		log.Error(err, "failed to load edge config")
		os.Exit(1)
	}
	log.Info("loaded edge config", "namespace", namespace, "zones", len(cfg.Zones), "robots", len(cfg.RobotZone))

	// A persistent, tamper-evident local sink for headless-estop audit (RFC-0001 #safety-edge-headless-emergency-stop) is
	// a deployment concern; MemorySink is the reference sink for now.
	auditLog := audit.New(&audit.MemorySink{}, version)
	node := edge.New(cfg, auditLog)
	node.Log = log.WithName("edge-node")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Keep the node's zones/robots current from the control plane. The --config-file
	// above is the bootstrap/last-known-good seed; live sync refreshes it and fails
	// safe (retains the seed) through a control-plane outage.
	if err := startConfigSync(ctx, node, namespace, kubeconfig, syncInterval, log); err != nil {
		log.Error(err, "failed to start config sync")
		os.Exit(1)
	}

	// Local safety input (light curtain / e-stop button / safety-PLC output) → a
	// zone-wide headless estop. Wiring the physical line is a manual step; here we
	// poll a digital-input value file. Fail-safe: an unreadable input trips.
	if safetyFile != "" {
		input := &edge.FileSafetyInput{Path: safetyFile, ActiveLow: safetyActLow}
		monitor := edge.NewInputMonitor(input, node.TriggerLocalEstop, safetyPoll, log)
		go func() { _ = monitor.Run(ctx) }()
		log.Info("local safety input monitor started", "input", safetyFile, "activeLow", safetyActLow)
	}

	health := newHealthServer(healthAddr)
	healthErr := make(chan error, 1)
	go func() { healthErr <- health.start(ctx) }()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveEdge(ctx, listenAddr, tlsCfg, node, log, func() { health.setReady(true) })
	}()

	// served records that the select below already took serveErr's one value, so the
	// drain after it does not wait on an empty channel.
	served := false
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received; stopping")
	case err := <-serveErr:
		served = true
		if err != nil {
			log.Error(err, "edge server exited")
			stop()
			os.Exit(1)
		}
	case err := <-healthErr:
		if err != nil {
			log.Error(err, "health server exited")
			stop()
			os.Exit(1)
		}
	}
	// Drain the server goroutine so GracefulStop completes before exit.
	if !served {
		if err := <-serveErr; err != nil {
			log.Error(err, "edge server exited with an error during shutdown")
		}
	}
}
