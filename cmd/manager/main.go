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
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/probe"
	"github.com/swarmada/swarmada/internal/registrar"
	"github.com/swarmada/swarmada/internal/safety"
	"github.com/swarmada/swarmada/internal/streamauth"
	"github.com/swarmada/swarmada/internal/tde"
	"github.com/swarmada/swarmada/internal/telemetry"
	swarmadawebhook "github.com/swarmada/swarmada/internal/webhook"
)

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// Required for --leader-elect (LeaderElection/LeaderElectionID below):
// controller-runtime's leader election coordinates multiple manager
// replicas via a Lease in the manager's own namespace. This was missing
// from config/rbac/role.yaml until the in-cluster deploy path first
// exercised --leader-elect=true (`make run`'s default is false, so this
// gap was invisible until config/manager/manager.yaml's Deployment set it).
//
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// Required by the leader-election recorder (emits a "became leader" Event on
// acquisition) and by every controller's mgr.GetEventRecorderFor(...) — same
// invisibility reason as the leases rule above.

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(fleetv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr                 string
		probeAddr                   string
		enableLeaderElection        bool
		leaderElectionID            string
		leaderElectionLeaseDur      time.Duration
		leaderElectionRenewDeadline time.Duration
		leaderElectionRetryPeriod   time.Duration
		fleetAdapterAddr            string
		modelWebhookAddr            string
		auditLogFile                string
		fleetAdapterInsecure        bool
		fleetAdapterTLSCertFile     string
		fleetAdapterTLSKeyFile      string
		fleetAdapterClientCAFile    string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for health probes.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller-manager (required when running multiple replicas).")
	flag.StringVar(&leaderElectionID, "leader-election-id", "swarmada-leader",
		"Lease name used for leader election.")
	// Defaults here (4s/3s/1s) are deliberately short, NOT controller-runtime's
	// stock 15s/10s/2s. This manager normally runs as a single replica (dev and
	// this repo's warehouse-quickstart demo), where leader election exists only
	// to make a pod restart safe, never to arbitrate between concurrently-live
	// replicas. With the stock 15s LeaseDuration and this Deployment's
	// terminationGracePeriodSeconds: 10 (config/manager/manager.yaml), a pod
	// that doesn't finish releasing its lock before SIGKILL strands the lease
	// for up to 15s — long enough to blow past warehouse-quickstart's own
	// (deliberately tight, simulator-tuned) 15s scheduling-assignment timeout.
	// A real multi-replica production deployment should override these back
	// toward the standard 15s/10s/2s (or larger) via flags for a wider
	// clock-skew/network-partition safety margin — this default optimizes for
	// "single replica, restart quickly" over "many replicas, never double-lead."
	flag.DurationVar(&leaderElectionLeaseDur, "leader-election-lease-duration", 4*time.Second,
		"Leader election LeaseDuration. See in-code comment: short by default for this single-replica dev/demo manager.")
	flag.DurationVar(&leaderElectionRenewDeadline, "leader-election-renew-deadline", 3*time.Second,
		"Leader election RenewDeadline. Must be less than the lease duration.")
	flag.DurationVar(&leaderElectionRetryPeriod, "leader-election-retry-period", 1*time.Second,
		"Leader election RetryPeriod. Must be less than the renew deadline.")
	flag.StringVar(&fleetAdapterAddr, "fleet-adapter-bind-address", ":9443",
		"Address the ControlStream gRPC server listens on for Fleet Adapter connections. Empty disables it.")
	flag.StringVar(&modelWebhookAddr, "model-webhook-bind-address", ":9444",
		"Address the ModelPolicy training-completion webhook (HMAC) listens on. Empty disables it.")
	flag.StringVar(&auditLogFile, "audit-log-file", "",
		"Path to a durable append-only §9.5.4 audit log (NDJSON). Empty uses an in-memory sink (dev only).")
	flag.BoolVar(&fleetAdapterInsecure, "fleet-adapter-insecure-authz", false,
		"DEV ONLY. Disable per-robot ControlStream authorization (RFC-0001 §9.2.7, §9.5.1.2) so a "+
			"plaintext adapter with no mTLS identity is authorized for every message. This activates "+
			"the existing dev-mode path in internal/controlstream.Server (a nil Authorizer authorizes "+
			"everything and logs a loud warning at connect) — it does not add a new bypass. The "+
			"ControlStream listener is already plaintext by default (backlog §F); this flag additionally "+
			"disables the authorization check that otherwise fail-closes every message from a connection "+
			"with no verified mTLS identity. Off by default. NEVER set true outside a local dev/demo "+
			"cluster — see examples/warehouse-quickstart's --scenario live path for the intended use.")
	flag.StringVar(&fleetAdapterTLSCertFile, "fleet-adapter-tls-cert-file", "",
		"PEM server certificate for ControlStream mTLS (RFC-0001 §9.2.7). Set together with "+
			"--fleet-adapter-tls-key-file and --fleet-adapter-client-ca-file to terminate mTLS; the "+
			"keypair is re-read on each handshake so cert rotation needs no restart.")
	flag.StringVar(&fleetAdapterTLSKeyFile, "fleet-adapter-tls-key-file", "",
		"PEM private key for the ControlStream server certificate (see --fleet-adapter-tls-cert-file).")
	flag.StringVar(&fleetAdapterClientCAFile, "fleet-adapter-client-ca-file", "",
		"PEM CA bundle that ControlStream client (adapter) certificates must verify against. With all "+
			"three TLS flags set, clients are required and verified (tls.RequireAndVerifyClientCert). "+
			"If the bind address is set but no TLS material is provided, the ControlStream server is NOT "+
			"registered unless --fleet-adapter-insecure-authz is set (fail closed).")

	opts := zap.Options{
		Development: true,
		TimeEncoder: zapcore.ISO8601TimeEncoder,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: ctrlmetrics.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
		LeaseDuration:          &leaderElectionLeaseDur,
		RenewDeadline:          &leaderElectionRenewDeadline,
		RetryPeriod:            &leaderElectionRetryPeriod,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Register the RFC-0001 §9.3.8 metrics into the controller-runtime registry so
	// the manager's existing /metrics endpoint (--metrics-bind-address, §9.3.8
	// default :9090) serves them. Additive instrumentation only — no behaviour
	// change, RA-1 untouched.
	metrics.Register(crmetrics.Registry)

	// Periodically recompute the §9.3.8 aggregate resource gauges (fleetactions/
	// robots/adapters by phase, robots in estop). Runs on every replica, reads
	// only — never writes resource status (RA-1).
	if err = mgr.Add(&controller.MetricsSweeper{Client: mgr.GetClient()}); err != nil {
		setupLog.Error(err, "unable to add metrics sweeper")
		os.Exit(1)
	}

	// Shared tamper-evident §9.5.4 audit recorder (per-namespace hash chain), used by
	// every audit producer (action cancel, rollout creation, zone estop). A configured
	// --audit-log-file selects the durable FileSink; otherwise an in-memory sink (dev
	// only). A configured-but-unopenable audit log is fatal: we do NOT run a safety
	// control plane whose audit trail is silently absent (fail closed).
	var auditSink audit.Sink = &audit.MemorySink{}
	if auditLogFile != "" {
		fs, ferr := audit.NewFileSink(auditLogFile)
		if ferr != nil {
			setupLog.Error(ferr, "unable to open durable audit log; refusing to start", "path", auditLogFile)
			os.Exit(1)
		}
		auditSink = fs
	}
	auditRecorder := audit.New(auditSink, "v0.1.0")

	// Command-push dispatcher (RFC-0001 §9.2, §E-2): pushes verify_*/model_update/
	// assign_action/renew_lease Commands to a robot's adapter over ControlStream and
	// correlates the CommandResult. Built only when the ControlStream server is
	// enabled (below); it is registered as that server's command sink, the RobotProbe
	// Prober, the ModelRollout model_update Pusher, and the FleetAction lease-wire
	// Commander. When ControlStream is disabled the consumer-facing interfaces stay
	// nil (probes report Unknown; ModelRollout observe-only; FleetAction no wire push)
	// rather than wrapping a nil pointer.
	var prober probe.Prober
	var modelPusher command.ModelUpdatePusher
	var actionCommander command.ActionCommander
	var actionValidator command.ActionValidator
	var cmdDispatcher *command.Dispatcher
	var zoneAdmit controller.ZoneAdmissionNotifier
	if fleetAdapterAddr != "" {
		cmdDispatcher = command.New(mgr.GetClient())
		prober = cmdDispatcher
		modelPusher = cmdDispatcher
		actionCommander = cmdDispatcher
		actionValidator = cmdDispatcher
		zoneAdmit = cmdDispatcher
	}

	// The Robot controller uses the dispatcher as its LivenessProber, so the dispatcher is
	// built before the controller is registered. With ControlStream disabled it stays nil and
	// the Offline transition falls back to elapsed time alone (§9.6.3.2).
	var liveness controller.LivenessProber
	if cmdDispatcher != nil {
		liveness = cmdDispatcher
	}
	if err = (&controller.RobotReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Audit:    auditRecorder,
		Liveness: liveness,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Robot")
		os.Exit(1)
	}

	// RobotClass status controller (RFC-0001 §5.2.1): read/aggregate only — reports
	// status.referencingRobots + the observed (merge) generation. No behavior.
	if err = (&controller.RobotClassReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RobotClass")
		os.Exit(1)
	}

	// DiscoveredRobot TTL/Stale sweeper (RFC-0001 §9.2.5): marks an un-admitted
	// DiscoveredRobot Stale as it nears its ttlExpiresAt and deletes it once expired.
	if err = (&controller.DiscoveredRobotReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("discoveredrobot"),
		Audit:    auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DiscoveredRobot")
		os.Exit(1)
	}

	// FleetAdapter status controller (RFC-0001 §9.1.12): drives status.phase from
	// adapter connectivity. It doubles as the ControlStream presence sink (wired
	// into the server below) and the liveness-staleness backstop.
	fleetAdapterReconciler := &controller.FleetAdapterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
	if err = fleetAdapterReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "FleetAdapter")
		os.Exit(1)
	}

	// One shared Traffic Deconfliction Engine (RFC-0001 §9.4, TDE-4 single
	// authority): the FleetAction gate reserves through it before committing an
	// assignment, and the Zone Controller signals zone entry/exit on it — both must
	// act on the same authoritative reservation state.
	tdeEngine := tde.New(mgr.GetClient(), tde.DefaultConfig())
	// Per-namespace TDE tunables (§9.1.11.10): resolve reservation TTLs from the
	// namespace's SwarmadaConfig.spec.trafficDeconfliction. FAIL-SAFE — a read error
	// or absent config yields DefaultConfig (a zero field never zeroes a TTL). Read
	// off the hot path (once per reservation/phase change), so a cached-client list
	// per call is fine.
	tdeEngine.WithConfigResolver(func(ns string) tde.Config {
		var configs fleetv1.SwarmadaConfigList
		if err := mgr.GetClient().List(context.Background(), &configs, client.InNamespace(ns)); err != nil || len(configs.Items) == 0 {
			return tde.DefaultConfig()
		}
		td := configs.Items[0].Spec.TrafficDeconfliction
		return tde.ConfigFromTDE(td.ReservationTTLSeconds, td.DisconnectedReservationTTLSeconds)
	})

	if err = (&controller.FleetActionReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		TDE:       tdeEngine,
		Commander: actionCommander,
		Validator: actionValidator,
		Audit:     auditRecorder,
		Recorder:  mgr.GetEventRecorderFor("fleetaction-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "FleetAction")
		os.Exit(1)
	}

	// FleetTask controller (RFC-0001 §9.1.5): the composite meta-controller — owns child
	// FleetActions, evaluates the dependency graph, aggregates phase, and runs the
	// completion/failure/compensation policy. Control-plane only; never contacts a robot.
	if err = (&controller.FleetTaskReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("fleettask-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "FleetTask")
		os.Exit(1)
	}

	// OTA/Model Update Manager (RFC-0001 §9.3.6): drives ModelRollout — batch
	// selection under safety constraints, pushes the model_update Command and marks
	// the model Updating on an acknowledged batch entry (suspending its derived
	// capabilities), and projects granted capabilities into
	// Robot.status.modelGrantedCapabilities[] once the model reports Active.
	if err = (&controller.ModelRolloutReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("modelrollout-controller"),
		Pusher:   modelPusher,
		Audit:    auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelRollout")
		os.Exit(1)
	}

	// RobotProbe controller (RFC-0001 §9.1.6): active verify_* health checks,
	// binding the proto ProbeStatus into RobotProbe.status. With the command-push
	// Prober wired, probes run live end-to-end; a probe that cannot confirm health
	// (unreachable/timeout/unsupported) is never reported Healthy.
	if err = (&controller.RobotProbeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Prober: prober,
		Audit:  auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RobotProbe")
		os.Exit(1)
	}

	// ZoneMaintenance controller (RFC-0001 §9.1.11): drives the maintenance-window
	// lifecycle (Scheduled→Active→Completed), pausing Idle robots in scope into the
	// Maintenance phase and restoring them on auto-resume or deletion (finalizer).
	if err = (&controller.ZoneMaintenanceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("zonemaintenance-controller"),
		Audit:    auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ZoneMaintenance")
		os.Exit(1)
	}

	// FirmwareRollout controller (RFC-0001 §9.1.7, §9.2.8): verifies the artifact
	// signature/checksum against the configured trust roots BEFORE any dispatch and
	// fails closed — an unverified artifact is never annotated onto a robot.
	if err = (&controller.FirmwareRolloutReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("firmwarerollout-controller"),
		Audit:    auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "FirmwareRollout")
		os.Exit(1)
	}

	// ModelPolicy controller (RFC-0001 §9.1.9): evaluates the quality gate on a
	// training-completion trigger and auto-creates a ModelRollout when it passes.

	if err = (&controller.ModelPolicyReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("modelpolicy-controller"),
		Audit:    auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelPolicy")
		os.Exit(1)
	}

	if err = (&controller.SwarmadaConfigReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("swarmadaconfig-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SwarmadaConfig")
		os.Exit(1)
	}

	if err = (&controller.SwarmadaConfigBootstrapReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SwarmadaConfigBootstrap")
		os.Exit(1)
	}

	// Zone Controller (RFC-0001 §9.3.4): maintains FleetZone topology
	// (isLeaf/childZones/robotCount) and derives Robot.status.currentZone from
	// live position telemetry via point-in-polygon containment. It subscribes to
	// the telemetry Ingestor's position fan-out below.
	zoneController := &controller.ZoneController{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("zone-controller"),
		TDE:      tdeEngine, // same engine the FleetAction gate reserves through
		// Zone-capacity hold/admit push (§9.3.4); nil when ControlStream is disabled.
		Admission: zoneAdmit,
	}
	if err = zoneController.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Zone")
		os.Exit(1)
	}

	// Shared confirmed-estop dispatcher (§9.6.2) and safety audit log (§9.5.4): one
	// instance backs both the ControlStream SafetyStream (which registers each
	// adapter's live stream on it, below) and the zone-estop fan-out. When
	// ControlStream is disabled the dispatcher has no streams, so a zone estop
	// reports every robot unconfirmed (escalate) — never a false Stopped.
	safetyDispatcher := safety.New(mgr.GetClient(), mgr.GetEventRecorderFor("estop"))
	// Seal ESTOP_LATENCY_VIOLATION into the §9.5.4 chain as well as emitting the Event: an
	// SLA breach is a durable conformance fact about an adapter, and an Event alone is
	// subject to namespace retention.
	safetyDispatcher.Audit = auditRecorder

	// Zone-estop fan-out (§9.6.2.5): on a `swarmada.io/estop-triggered` FleetZone
	// annotation, confirmed-estop every robot in the zone — and, per
	// estopPolicy.propagateToChildren, in descendant zones — and notify the parent
	// (propagateToParent) without auto-estopping it.
	if err = (&controller.ZoneEstopReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Estopper: safetyDispatcher,
		Recorder: mgr.GetEventRecorderFor("zone-estop"),
		Audit:    auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ZoneEstop")
		os.Exit(1)
	}

	// Robot-scope estop (§9.6.2 estop scopes): a `swarmada.io/estop-triggered`
	// annotation on a single Robot confirmed-estops that one robot; removing it
	// clears. SAR-gated at admission by the Robot webhook (EstopAuthz below).
	if err = (&controller.RobotEstopReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Estopper: safetyDispatcher,
		Audit:    auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RobotEstop")
		os.Exit(1)
	}

	// Namespace-scope estop (§9.6.2 estop scopes): a `swarmada.io/estop-triggered`
	// annotation on the namespace's SwarmadaConfig confirmed-estops every robot in
	// the namespace; removing it clears. SAR-gated by the SwarmadaConfig webhook.
	if err = (&controller.NamespaceEstopReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Estopper: safetyDispatcher,
		Audit:    auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NamespaceEstop")
		os.Exit(1)
	}

	// ── Telemetry live feed (RFC-0001 §9.3.7) ──────────────────────────────────
	// ControlStream server → Ingestor → Projector → StatusSink. The Projector is
	// the RA-1 gate: it forwards to the StatusSink ONLY on a material transition,
	// so Robot.status is never written on a per-tick telemetry frame. The
	// high-cadence TSDB plane defaults to drop (NoopWriter) until the §9.1.11
	// SwarmadaConfig telemetry-sink block (next §E item) selects a real store.
	//
	// SECURITY (RFC-0001 §9.2.7, ADR-0025): the ControlStream terminates mTLS when
	// the three --fleet-adapter-tls-* flags are set. If the bind address is set but
	// no TLS material is provided, the server is registered only under
	// --fleet-adapter-insecure-authz (dev/demo); otherwise it fails closed (logged,
	// not served) rather than serving plaintext.
	if fleetAdapterAddr != "" {
		var controlStreamTLS *tls.Config
		if controlStreamTLS, err = buildControlStreamTLS(fleetAdapterTLSCertFile, fleetAdapterTLSKeyFile, fleetAdapterClientCAFile); err != nil {
			setupLog.Error(err, "invalid ControlStream mTLS configuration")
			os.Exit(1)
		}
		statusSink := &controller.RobotStatusSink{Client: mgr.GetClient()}
		// Per-namespace telemetry resolution (§9.1.11.7): each frame is routed to the
		// TSDB sink and status write-policy configured in its namespace's
		// SwarmadaConfig.spec.telemetry. Resolution is FAIL-SAFE — an unsynced cache,
		// a read error, or an absent config yields the zero PlaneConfig (Drop sink +
		// default policy), so an unreadable policy drops high-cadence samples rather
		// than forcing them onto etcd (RA-1). The Router memoizes per namespace, so
		// the high-cadence ingest path never lists config per frame.
		telemetryResolver := func(ns string) telemetry.PlaneConfig {
			var configs fleetv1.SwarmadaConfigList
			if err := mgr.GetClient().List(context.Background(), &configs, client.InNamespace(ns)); err != nil || len(configs.Items) == 0 {
				return telemetry.PlaneConfig{}
			}
			tc := configs.Items[0].Spec.Telemetry
			return telemetry.PlaneConfig{
				SinkType:  string(tc.Sink.Type),
				Endpoint:  tc.Sink.Endpoint,
				Projector: telemetry.ConfigFromTelemetry(tc.StatusWriteMinIntervalSeconds, tc.MaxStatusWritesPerMinutePerRobot, tc.MaterialBatteryThresholds),
			}
		}
		// Zone derivation consumes the position fan-out upstream of the sink (§9.3.7).
		ingestor := telemetry.NewRouter(telemetryResolver, statusSink,
			telemetry.WithPositionObserver(zoneController))
		// --fleet-adapter-insecure-authz (dev only): a nil Authorizer activates the
		// existing dev-mode path in controlstream.Server — every message is
		// authorized and a loud warning is logged at connect. Off by default; the
		// real, fail-closed Authorizer is always used unless explicitly disabled.
		var authorizer controlstream.Authorizer = &streamauth.Authorizer{Client: mgr.GetClient()}
		if fleetAdapterInsecure {
			authorizer = nil
			setupLog.Info("WARNING: --fleet-adapter-insecure-authz=true — ControlStream authorization " +
				"is DISABLED. Every connected adapter is authorized for every robot regardless of mTLS " +
				"identity. This is a dev/demo-only posture (RFC-0001 §9.2.7, §9.5.1.2) and must never be " +
				"set in a production deployment.")
		}
		csServer := &controlstream.Server{
			Ingestor: ingestor,
			// Robot-reported action state (§9.2.3): advances an assigned FleetAction to
			// InProgress + startedAt on RUNNING. Terminal transitions are handled by
			// the reconciler under the single-executor invariant.
			ActionStatus: &controller.ActionStatusIngestor{Client: mgr.GetClient()},
			// Advisory per-robot rollout progress (§6.6/§6.7): surfaced as the active
			// FirmwareRollout/ModelRollout's currentBatch updatePhase.
			UpdateProgress: &controller.UpdateProgressIngestor{Client: mgr.GetClient()},
			// Adapter action-catalog projection (§9.2): projects supported_actions
			// from a CapabilitiesSnapshot onto FleetAdapter.status.supportedActions.
			Capabilities: &controller.CapabilitiesIngestor{Client: mgr.GetClient()},
			// Two-phase discovery handshake (§9.2.5, §9.3.1): Discover creates a
			// status-only DiscoveredRobot (populating macAddress + reported
			// inventory) for an unadmitted robot and rejects a malformed robot_id
			// with INVALID_ROBOT_ID; Register confirms an admitted Robot.
			Registrar: &registrar.Registrar{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()},
			// Per-message robot_id authorization (§9.5.1.2): every robot-scoped
			// message is checked against the adapter's mTLS identity and refused on
			// failure (fail closed). nil (dev only, --fleet-adapter-insecure-authz)
			// disables this — see the construction above.
			Authorizer: authorizer,
			// Confirmed estop delivery over SafetyStream (§9.6.2): pushes estops and
			// records STOPPED only on an adapter-CONFIRMED EstopAck. Shared with the
			// zone-estop fan-out so both push over the same live SafetyStreams.
			Safety: safetyDispatcher,
			// Server→adapter command-push (§9.2, §E-2): the RobotProbe Prober built
			// above; registers each live ControlStream and routes CommandResults.
			Commands: cmdDispatcher,
			// Adapter-initiated shared-resource reservations (§5.4.5) go through the
			// shared TDE engine, so adapter and scheduler act on one authority.
			Reservations: tdeResourceReserver{tdeEngine},
			// On a release that promotes a queued waiter, push it the async
			// reservation_granted Command over its own adapter stream (§5.4.5).
			GrantNotifier: cmdDispatcher,
			// Adapter connectivity → FleetAdapter.status.phase (§9.1.12). NOT driven
			// by telemetry frames (RA-1).
			Presence: fleetAdapterReconciler,
			// Tamper-evident safety audit log (§9.5.4): a denied authorization is
			// recorded, never silently dropped. Shared with the zone-estop fan-out. An
			// in-memory sink for now; a durable/external sink is a deployment concern.
			Audit: auditRecorder,
			Log:   ctrl.Log.WithName("controlstream"),
		}
		// Fail closed: without mTLS material and without the explicit dev opt-in, do
		// NOT register the server — never serve plaintext by default (ADR-0025).
		if !controlStreamShouldServe(controlStreamTLS, fleetAdapterInsecure) {
			setupLog.Error(nil, "ControlStream requires mTLS but no TLS material was provided: set "+
				"--fleet-adapter-tls-cert-file, --fleet-adapter-tls-key-file and --fleet-adapter-client-ca-file "+
				"(or --fleet-adapter-insecure-authz on a dev/demo cluster). NOT serving ControlStream (fail closed).")
		} else if err = mgr.Add(controlstream.NewGRPCRunnable(fleetAdapterAddr, csServer, ctrl.Log.WithName("controlstream"), controlStreamTLS)); err != nil {
			setupLog.Error(err, "unable to add ControlStream server")
			os.Exit(1)
		} else {
			setupLog.Info("ControlStream telemetry live feed enabled", "address", fleetAdapterAddr, "mtls", controlStreamTLS != nil)
		}
	}

	// ModelPolicy training-completion webhook (§9.1.9.3): verifies the HMAC
	// signature and writes the model-trigger annotation the ModelPolicyReconciler
	// consumes. Runs on every replica (it must accept POSTs regardless of leadership).
	if modelWebhookAddr != "" {
		if err = mgr.Add(&controller.ModelPolicyWebhook{Client: mgr.GetClient(), Addr: modelWebhookAddr}); err != nil {
			setupLog.Error(err, "unable to add ModelPolicy webhook")
			os.Exit(1)
		}
		setupLog.Info("ModelPolicy webhook enabled", "address", modelWebhookAddr)
	}

	// Webhooks: the RobotClass admission-time merge (mutating, RFC-0001
	// §5.2.1.2) runs before the FleetAdapter admission gate (validating,
	// §5.2.12) so the gate observes the fully-resolved spec.
	// Set ENABLE_WEBHOOKS=false for envtest / local `make run` without serving certs.
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err = (&swarmadawebhook.RobotDefaulter{
			Client: mgr.GetClient(),
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Robot defaulter")
			os.Exit(1)
		}

		if err = (&swarmadawebhook.RobotAdmissionGate{
			Client:     mgr.GetClient(),
			EstopAuthz: &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Robot")
			os.Exit(1)
		}

		// SwarmadaConfig cross-field validation (§9.1.11): AfterTimeout⇒timeout,
		// disconnectedReservationTTLSeconds>disconnectTimeoutSeconds, real-sink⇒endpoint,
		// requireSignatureVerification⇒trustRoots.
		if err = (&swarmadawebhook.SwarmadaConfigValidator{
			EstopAuthz: &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
			Audit:      auditRecorder,
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "SwarmadaConfig")
			os.Exit(1)
		}

		// FleetZone cross-resource validation (§9.3.1): parentZone existence + acyclicity,
		// simple-polygon boundary, and delete-with-children rejection.
		if err = (&swarmadawebhook.FleetZoneValidator{
			Client:     mgr.GetClient(),
			EstopAuthz: &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "FleetZone")
			os.Exit(1)
		}

		// ZoneMaintenance scope resolution (§9.3.1): a Zone-scoped window must name a
		// FleetZone that exists, or it silently pauses nothing.
		if err = (&swarmadawebhook.ZoneMaintenanceValidator{
			Client: mgr.GetClient(),
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "ZoneMaintenance")
			os.Exit(1)
		}

		// FleetAction cancel-verb SAR enforcement (§9.5.3): writing the
		// swarmada.io/cancel-requested annotation requires the `cancel` verb.
		if err = (&swarmadawebhook.FleetActionValidator{
			CancelAuthz: &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
			Reader:      mgr.GetClient(),
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "FleetAction")
			os.Exit(1)
		}

		// FleetTask append-only validator (RFC-0001 §9.1.5): enforces append-only spec.actions +
		// immutable policies, gates action appends behind the scoped `append` SAR verb, and refuses
		// `delete` on a live composite (cancel first — the confirmed-cancel path).
		if err = (&swarmadawebhook.FleetTaskValidator{
			AppendAuthz: &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "FleetTask")
			os.Exit(1)
		}

		// Rollout delete guards (`docs/operations.md`): only a terminal (Succeeded/Failed) rollout
		// record may be deleted, so a rollout in flight cannot be stranded mid-update — for
		// ModelRollout that would leave model-granted capabilities suspended with no owner.
		if err = (&swarmadawebhook.FirmwareRolloutValidator{Client: mgr.GetClient()}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "FirmwareRollout")
			os.Exit(1)
		}
		if err = (&swarmadawebhook.ModelRolloutValidator{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "ModelRollout")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// TDE recovery (RFC-0001 §9.4.7) runs as a LEADER-ELECTED runnable: reservation
	// state is rebuilt from durable FleetZone.status each time this replica acquires
	// leadership — at process start AND on a leader failover — so a promoted standby
	// never serves grants against empty/stale in-memory state. The engine's grant
	// gate fails closed (denies with tde_unavailable) until recovery completes, so no
	// reconciler can over-grant before the rebuild. It uses the manager's cached
	// client (synced before leader-elected runnables start), so no direct client is
	// needed. With --leader-elect=false the manager runs it once at startup.
	if err = mgr.Add(&tde.RecoveryRunnable{
		Engine: tdeEngine,
		Client: mgr.GetClient(),
		Mode:   tde.RecoverValidate,
		// Wait for the Zone Controller to load zone geometry before validating
		// Occupied reservations against Robot.status.currentZone; on timeout fall back
		// to the §9.4.7 conservative action (release all). Defaults per §9.1.11.10.
		Ready:        zoneController.Ready,
		ReadyTimeout: 30 * time.Second,
		Fallback:     tde.RecoverReleaseAll,
		// Recovery is cluster-wide, so its tunables come from the manager's own
		// namespace SwarmadaConfig (spec.trafficDeconfliction.recovery.*), not per
		// workload namespace (ADR-0015). Empty POD_NAMESPACE keeps the static defaults.
		ConfigNamespace: os.Getenv("POD_NAMESPACE"),
		Log:             ctrl.Log.WithName("tde-recovery"),
	}); err != nil {
		setupLog.Error(err, "unable to add TDE recovery runnable")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// controlStreamShouldServe reports whether the ControlStream server may be
// registered: only with an mTLS config, or under the explicit dev opt-in. Absent
// both, the server is not served — plaintext is never the default (ADR-0025).
func controlStreamShouldServe(tlsCfg *tls.Config, insecure bool) bool {
	return tlsCfg != nil || insecure
}

// buildControlStreamTLS assembles the server TLS config for ControlStream mTLS
// (RFC-0001 §9.2.7, ADR-0025). It returns (nil, nil) when none of the three files
// is set — the caller then chooses between the dev-insecure path and failing
// closed. When any file is set, all three are required. The server keypair is
// served through a GetCertificate closure that re-reads it on each handshake, so a
// cert-manager renewal takes effect without restarting the manager; the client-CA
// pool is read once at startup. ClientAuth is RequireAndVerifyClientCert, so the
// TLS handshake verifies the client certificate against the CA pool before any
// stream is accepted (verify-then-trust), and the version floor is TLS 1.3.
func buildControlStreamTLS(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" && clientCAFile == "" {
		return nil, nil
	}
	if certFile == "" || keyFile == "" || clientCAFile == "" {
		return nil, fmt.Errorf("ControlStream mTLS requires all of --fleet-adapter-tls-cert-file, " +
			"--fleet-adapter-tls-key-file and --fleet-adapter-client-ca-file")
	}
	getCertificate := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		crt, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load ControlStream server keypair: %w", err)
		}
		return &crt, nil
	}
	if _, err := getCertificate(nil); err != nil { // fail fast if the keypair is unreadable at startup
		return nil, err
	}
	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read ControlStream client CA %q: %w", clientCAFile, err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("ControlStream client CA %q contained no valid certificates", clientCAFile)
	}
	return &tls.Config{
		GetCertificate: getCertificate,
		ClientCAs:      clientCAs,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		MinVersion:     tls.VersionTLS13,
	}, nil
}

// tdeResourceReserver adapts the TDE engine to controlstream.ResourceReserver,
// mapping the engine's ResourceOutcome onto the server's ResourceDecision.
type tdeResourceReserver struct{ e *tde.Engine }

func (r tdeResourceReserver) ReserveResource(ctx context.Context, ns, res, robot string) controlstream.ResourceDecision {
	return toResourceDecision(r.e.ReserveResource(ctx, ns, res, robot))
}

func (r tdeResourceReserver) ReleaseResource(ctx context.Context, ns, res, robot string) controlstream.ResourceDecision {
	return toResourceDecision(r.e.ReleaseResource(ctx, ns, res, robot))
}

func toResourceDecision(out tde.ResourceOutcome, _ error) controlstream.ResourceDecision {
	return controlstream.ResourceDecision{
		State:           controlstream.ResourceReserveState(out.State),
		QueuePosition:   out.QueuePosition,
		Message:         out.Message,
		Released:        out.Released,
		PromotedRobotID: out.PromotedRobotID,
	}
}
