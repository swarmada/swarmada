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
	"errors"
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
	rbacpreflight "github.com/swarmada/swarmada/internal/rbac"
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

// buildVersion is stamped onto every audit entry as swarmada_version (RFC-0001 #safety-log-entry-format).
//
// Set at link time: -ldflags "-X main.buildVersion=v0.3.1". The default is deliberately
// "dev" rather than a plausible release number: an auditor reading swarmada_version
// needs to know WHICH build sealed an entry, and a hardcoded version that never tracks
// the binary is worse than an obviously-unset one — it reads as provenance while
// carrying none. This field was pinned to "v0.1.0" through every release up to v0.3.
var buildVersion = "dev"

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(fleetv1.AddToScheme(scheme))
}

// The command dispatcher is the safety dispatcher's fence sender, the cleared stop's fence included.
var (
	_ safety.FenceSender        = (*command.Dispatcher)(nil)
	_ safety.ClearedFenceSender = (*command.Dispatcher)(nil)
)

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
		requireLatchContract        bool
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
	// 9445, not 9443: the admission webhook server always binds 9443, so a ControlStream
	// default of 9443 made a webhooks-on manager that kept the default fail to bind one of them.
	flag.StringVar(&fleetAdapterAddr, "fleet-adapter-bind-address", ":9445",
		"Address the ControlStream gRPC server listens on for Fleet Adapter connections. Empty disables it.")
	flag.StringVar(&modelWebhookAddr, "model-webhook-bind-address", ":9444",
		"Address the ModelPolicy training-completion webhook (HMAC) listens on. Empty disables it.")
	flag.StringVar(&auditLogFile, "audit-log-file", "",
		"Path to a durable append-only audit log (NDJSON; RFC-0001 #safety-safety-audit-log). Empty uses an in-memory sink (dev only).")
	flag.BoolVar(&fleetAdapterInsecure, "fleet-adapter-insecure-authz", false,
		"DEV ONLY. Disable per-robot ControlStream authorization (RFC-0001 #fleet-adapter-protocol-transport-security, RFC-0001 #security-fleet-adapter-authentication-robot-identity) so a "+
			"plaintext adapter with no mTLS identity is authorized for every message. This activates "+
			"the existing dev-mode path in internal/controlstream.Server (a nil Authorizer authorizes "+
			"everything and logs a loud warning at connect) — it does not add a new bypass. The "+
			"ControlStream listener is already plaintext by default; this flag additionally "+
			"disables the authorization check that otherwise fail-closes every message from a connection "+
			"with no verified mTLS identity. Off by default. NEVER set true outside a local dev/demo "+
			"cluster. Refused at startup together with the three --fleet-adapter-tls-* flags: with mTLS "+
			"configured every adapter has a verified identity, and the authorization check is what "+
			"scopes that identity to its own robots.")
	flag.StringVar(&fleetAdapterTLSCertFile, "fleet-adapter-tls-cert-file", "",
		"PEM server certificate for ControlStream mTLS (RFC-0001 #fleet-adapter-protocol-transport-security). Set together with "+
			"--fleet-adapter-tls-key-file and --fleet-adapter-client-ca-file to terminate mTLS; the "+
			"keypair is re-read on each handshake so cert rotation needs no restart.")
	flag.StringVar(&fleetAdapterTLSKeyFile, "fleet-adapter-tls-key-file", "",
		"PEM private key for the ControlStream server certificate (see --fleet-adapter-tls-cert-file).")
	flag.StringVar(&fleetAdapterClientCAFile, "fleet-adapter-client-ca-file", "",
		"PEM CA bundle that ControlStream client (adapter) certificates must verify against. With all "+
			"three TLS flags set, clients are required and verified (tls.RequireAndVerifyClientCert). "+
			"If the bind address is set but no TLS material is provided, the ControlStream server is NOT "+
			"registered unless --fleet-adapter-insecure-authz is set (fail closed).")

	flag.BoolVar(&requireLatchContract, "require-latch-contract", false,
		"Give work only to a robot whose adapter's ControlStream negotiated fleet-adapter contract 1.4.0, "+
			"the version the Stop Latch binds: candidate filter 9's version condition and row A15's version "+
			"send check (RFC-0001 #control-plane-scheduler, #safety-action-table). A 1.3.x connection is still "+
			"sent every stop. Off at v0.4: the in-tree deployments run the simulation adapter at 1.3.0, which "+
			"this would leave with no work.")

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

	// Register the metrics-contract metrics into the controller-runtime registry so
	// the manager's existing /metrics endpoint (--metrics-bind-address, RFC-0001 #control-plane-observability-prometheus-metrics-contract
	// default:8080) serves them. Additive instrumentation only — no behaviour
	// change, RA-1 untouched.
	metrics.Register(crmetrics.Registry)

	// Periodically recompute the metrics-contract aggregate resource gauges (fleetactions/
	// robots/adapters by phase, robots in estop). Runs on every replica, reads
	// only — never writes resource status (RA-1).
	if err = mgr.Add(&controller.MetricsSweeper{Client: mgr.GetClient()}); err != nil {
		setupLog.Error(err, "unable to add metrics sweeper")
		os.Exit(1)
	}

	// Shared tamper-evident audit recorder (per-namespace hash chain; RFC-0001 #safety-safety-audit-log), used by
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
	} else {
		// Said out loud, every start. The in-memory sink loses the entire safety audit
		// trail when the process exits, which is indistinguishable at runtime from a
		// working one — an operator who never set --audit-log-file has no signal that
		// safety-audit-log evidence is not being kept until they go looking for it after an
		// incident. Not fatal: `make run` and the envtest suite rely on this default.
		setupLog.Info("WARNING: no --audit-log-file configured; the safety audit log is IN MEMORY " +
			"and is lost on restart. Do not run a production fleet in this mode (RFC-0001 #safety-log-entry-format).")
	}
	auditRecorder := audit.New(auditSink, buildVersion)

	// Continue each namespace's hash chain where the sink left off. Without this the
	// chain is tamper-evident only within one process lifetime: every restart reopens
	// each namespace at sequence 1 chained to genesis, which an auditor cannot tell
	// apart from a truncate-and-reseal attack (RFC-0001 #safety-log-entry-format).
	if rerr := auditRecorder.Resume(); rerr != nil {
		if errors.Is(rerr, audit.ErrSinkNotResumable) {
			setupLog.Info("audit chain starts at genesis: the configured sink cannot be read back, " +
				"so per-namespace sequence numbers restart on every process restart")
		} else {
			setupLog.Error(rerr, "unable to resume the audit chain; refusing to start", "path", auditLogFile)
			os.Exit(1)
		}
	}

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
	var zoneConstraints controller.ZoneConstraintsNotifier
	if fleetAdapterAddr != "" {
		cmdDispatcher = command.New(mgr.GetClient())
		// The motion gate and the register's fences read the Robot past the cache; a fence that
		// cannot be sent on a new ControlStream is an Event on the Robot (internal/command/fence.go).
		cmdDispatcher.APIReader = mgr.GetAPIReader()
		cmdDispatcher.Recorder = mgr.GetEventRecorderFor("command-dispatcher")
		prober = cmdDispatcher
		modelPusher = cmdDispatcher
		actionCommander = cmdDispatcher
		actionValidator = cmdDispatcher
		zoneAdmit = cmdDispatcher
		zoneConstraints = cmdDispatcher
	}

	// The Robot controller uses the dispatcher as its LivenessProber, so the dispatcher is
	// built before the controller is registered. With ControlStream disabled it stays nil and
	// the Offline transition falls back to elapsed time alone (RFC-0001 #safety-control-plane-response).
	var liveness controller.LivenessProber
	if cmdDispatcher != nil {
		liveness = cmdDispatcher
	}
	// The Robot controller is also where the telemetry projector's per-robot cache is
	// rebound to the Robot generation it describes (see RobotProjectionResetter). The
	// projector lives inside the telemetry Router, which is built further down with
	// the ControlStream server, so the resetter is created here, handed to the
	// controller, and pointed at the Router once that exists. It stays inert if the
	// ControlStream is disabled — no ControlStream, no projector to reset.
	robotProjection := &controller.RobotProjectionResetter{}
	// Positions is set once the Zone Controller exists, below; reconcilers do not run
	// before the manager starts.
	robotReconciler := &controller.RobotReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Audit:      auditRecorder,
		Liveness:   liveness,
		Projection: robotProjection,
		Recorder:   mgr.GetEventRecorderFor("robot"),
	}
	if err = robotReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Robot")
		os.Exit(1)
	}

	// RobotClass status controller (RFC-0001 #robotclass): read/aggregate only — reports
	// status.referencingRobots + the observed (merge) generation. No behavior.
	if err = (&controller.RobotClassReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RobotClass")
		os.Exit(1)
	}

	// DiscoveredRobot TTL/Stale sweeper (RFC-0001 #discoveredrobot-state-machine): marks an un-admitted
	// DiscoveredRobot Stale as it nears its ttlExpiresAt and deletes it once expired.
	if err = (&controller.DiscoveredRobotReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("discoveredrobot"),
		Audit:    auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DiscoveredRobot")
		os.Exit(1)
	}

	// FleetAdapter status controller (RFC-0001 #fleetadapter-status-fields-and-phase-machine): drives status.phase from
	// adapter connectivity. It doubles as the ControlStream presence sink (wired
	// into the server below) and the liveness-staleness backstop.
	fleetAdapterReconciler := &controller.FleetAdapterReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("fleetadapter-controller"),
	}
	if err = fleetAdapterReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "FleetAdapter")
		os.Exit(1)
	}

	// One shared Traffic Deconfliction Engine (RFC-0001 #tde-normative-invariants, TDE-4 single
	// authority): the FleetAction gate reserves through it before committing an
	// assignment, and the Zone Controller signals zone entry/exit on it — both must
	// act on the same authoritative reservation state.
	tdeEngine := tde.New(mgr.GetClient(), tde.DefaultConfig())
	// Per-namespace TDE tunables (RFC-0001 #swarmadaconfig-traffic-deconfliction-engine-configuration): resolve reservation TTLs from the
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
	// A waiter the engine promotes itself (a deleted Robot's hold released, or recovery)
	// is pushed its reservation_granted like one promoted by an adapter's release.
	if cmdDispatcher != nil {
		tdeEngine.WithGrantNotifier(cmdDispatcher.NotifyReservationGranted)
	}
	// Deleting a Robot releases its shared-resource holds at once (TDE-7).
	robotReconciler.Holds = tdeEngine

	// Each namespace's generation store is kept in the manager's own namespace, where the users of
	// the namespace it serves cannot write (RFC-0001 #control-plane-scheduler). Without
	// POD_NAMESPACE each store is kept in the namespace it serves, which any writer of Leases there
	// can lower, so say so loudly.
	storeNamespace := os.Getenv("POD_NAMESPACE")
	if storeNamespace == "" {
		setupLog.Info("POD_NAMESPACE is not set: each namespace's fencing-token generation store is kept in that " +
			"namespace, where anyone who can write Leases there can lower it; set POD_NAMESPACE to the manager's namespace")
	}
	if err = (&controller.FleetActionReconciler{
		Client:                   mgr.GetClient(),
		APIReader:                mgr.GetAPIReader(),
		Scheme:                   mgr.GetScheme(),
		TDE:                      tdeEngine,
		Commander:                actionCommander,
		Validator:                actionValidator,
		Audit:                    auditRecorder,
		Recorder:                 mgr.GetEventRecorderFor("fleetaction-controller"),
		GenerationStoreNamespace: storeNamespace,
		RequireLatchContract:     requireLatchContract,
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

	// TaskTrigger: a semantic event drafts a FleetTask and stops there. The draft
	// carries spec.draft, which the FleetTask controller holds at phase Draft — so
	// nothing on this path dispatches until a human clears the flag.
	if err = (&controller.TaskTriggerReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("tasktrigger-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TaskTrigger")
		os.Exit(1)
	}

	// One ledger of acknowledged model_update pushes, written by the ModelRollout controller
	// and read by the UpdateProgress ingestor: a failure the robot reports after
	// acknowledging is recorded before the batch is written (ModelAckLedger).
	modelAcks := controller.NewModelAckLedger()

	// OTA/Model Update Manager (RFC-0001 #control-plane-ota-model-update-manager): drives ModelRollout — batch
	// selection under safety constraints, pushes the model_update Command and marks
	// the model Updating on an acknowledged batch entry (suspending its derived
	// capabilities), and projects granted capabilities into
	// Robot.status.modelGrantedCapabilities[] once the model reports Active.
	if err = (&controller.ModelRolloutReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("modelrollout-controller"),
		Pusher:    modelPusher,
		Audit:     auditRecorder,
		APIReader: mgr.GetAPIReader(),
		Acks:      modelAcks,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelRollout")
		os.Exit(1)
	}

	// RobotProbe controller (RFC-0001 #robotprobe): active verify_* health checks,
	// binding the proto ProbeStatus into RobotProbe.status. With the command-push
	// Prober wired, probes run live end-to-end; a probe that cannot confirm health
	// (unreachable/timeout/unsupported) is never reported Healthy.
	if err = (&controller.RobotProbeReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Prober:    prober,
		Audit:     auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RobotProbe")
		os.Exit(1)
	}

	// ZoneMaintenance controller (RFC-0001 #zonemaintenance): drives the maintenance-window
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

	// FirmwareRollout controller (RFC-0001 #firmwarerollout, #fleet-adapter-protocol-fleet-adapter-compliance-checklist): verifies the artifact
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

	// ModelPolicy controller (RFC-0001 #modelpolicy): evaluates the quality gate on a
	// training-completion trigger and auto-creates a ModelRollout when it passes.

	if err = (&controller.ModelPolicyReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("modelpolicy-controller"),
		Audit:     auditRecorder,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelPolicy")
		os.Exit(1)
	}

	if err = (&controller.SwarmadaConfigReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("swarmadaconfig-controller"),
		Audit:    auditRecorder,
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

	// Zone Controller (RFC-0001 #control-plane-zone-controller): maintains FleetZone topology
	// (isLeaf/childZones/robotCount) and derives Robot.status.currentZone from
	// live position telemetry via point-in-polygon containment. It subscribes to
	// the telemetry Ingestor's position fan-out below.
	zoneController := &controller.ZoneController{
		Client: mgr.GetClient(),
		// The stop a currentZone move is held on is read uncached (holdZoneMove).
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("zone-controller"),
		TDE:       tdeEngine, // same engine the FleetAction gate reserves through
		// Zone-capacity hold/admit push (RFC-0001 #control-plane-zone-controller); nil when ControlStream is disabled.
		Admission: zoneAdmit,
		// The operating rules in force where a robot NOW IS (FA-R7, ADR-0059); nil when
		// ControlStream is disabled, which leaves status.containingZones derived and
		// published and only the push silent.
		Constraints: zoneConstraints,
	}
	// The ZonePositionOverdue condition reads which robots have sent a position frame.
	robotReconciler.Positions = zoneController
	// An adapter process that connects or reconnects holds no zone rule set (FA-R7), and a
	// robot standing still sends no containment event that would push one, so each
	// registered ControlStream is sent the rule set of every robot its adapter serves.
	//
	// This process's hold on leadership (cmd/manager/leadership.go): the ControlStream server
	// serves adapter streams, and the dispatcher sends commands stamped with the leader term,
	// only while it leads.
	var leaderState *leaderStateT
	if cmdDispatcher != nil {
		if leaderState, err = setupLeadership(mgr, enableLeaderElection, leaderElectionID); err != nil {
			setupLog.Error(err, "unable to add the leadership runnable")
			os.Exit(1)
		}
		cmdDispatcher.Term = leaderState.Term
		cmdDispatcher.OnStreamRegistered = func(id controlstream.TLSIdentity) {
			zoneController.ResendConstraintsForAdapter(context.Background(), id.Namespace, id.AdapterName)
		}
	}
	if err = zoneController.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Zone")
		os.Exit(1)
	}

	// Shared confirmed-estop dispatcher (RFC-0001 #safety-emergency-stop-protocol) and safety audit log (RFC-0001 #safety-safety-audit-log): one
	// instance backs both the ControlStream SafetyStream (which registers each
	// adapter's live stream on it, below) and the zone-estop fan-out. When
	// ControlStream is disabled the dispatcher has no streams, so a zone estop
	// reports every robot unconfirmed (escalate) — never a false Stopped.
	safetyDispatcher := safety.New(mgr.GetClient(), mgr.GetEventRecorderFor("estop"))
	// The estopState write compares against the stored Robot, read past the cache: a
	// cached read can miss another trigger's outcome written a moment before.
	safetyDispatcher.APIReader = mgr.GetAPIReader()
	// The resume confirmation judges a cleared robot on capabilities derived from the
	// hardware its witnessing frame reported, not on the stored list, which the Robot
	// reconciler writes later (RFC-0001 #safety-estop-state-machine).
	safetyDispatcher.DeriveCapabilities = controller.DeriveCapabilities
	// Seal ESTOP_LATENCY_VIOLATION into the audit chain (RFC-0001 #safety-safety-audit-log) as well as emitting the Event: an
	// SLA breach is a durable conformance fact about an adapter, and an Event alone is
	// subject to namespace retention.
	safetyDispatcher.Audit = auditRecorder
	// The leader term (cmd/manager/leadership.go): a process that no longer leads writes no
	// stop outcome and records nothing for a stop's answer.
	if leaderState != nil {
		safetyDispatcher.Term = leaderState.Term
	}
	// Each stop's fence goes down the robot's ControlStream in order with its AssignActions, and
	// an AssignAction goes only to a robot no stop in force holds, whose register the stream has
	// processed (internal/command/fence.go).
	if cmdDispatcher != nil {
		// The command dispatcher also sends a cleared stop's fence (safety.ClearedFenceSender), which
		// answers an adapter's report of a stop whose entry was removed.
		safetyDispatcher.Fence = cmdDispatcher
		cmdDispatcher.RequireRegister = true
		cmdDispatcher.RequireContract = requireLatchContract
	}

	// Zone-estop fan-out (RFC-0001 #safety-estop-scopes): on a `swarmada.io/estop-triggered` FleetZone
	// annotation, confirmed-estop every robot in the zone — and, per
	// estopPolicy.propagateToChildren, in descendant zones — and notify the parent
	// (propagateToParent) without auto-estopping it.
	if err = (&controller.ZoneEstopReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Estopper:  safetyDispatcher,
		Recorder:  mgr.GetEventRecorderFor("zone-estop"),
		Audit:     auditRecorder,
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ZoneEstop")
		os.Exit(1)
	}

	// Robot-scope estop (RFC-0001 #safety-emergency-stop-protocol estop scopes): a `swarmada.io/estop-triggered`
	// annotation on a single Robot confirmed-estops that one robot; removing it
	// clears. SAR-gated at admission by the Robot webhook (EstopAuthz below).
	// It also owns the second half of the clear: the dispatcher writes Resuming when
	// the operator clears, and this controller drives ConfirmResume until telemetry
	// confirms the robot may leave it (RFC-0001 #safety-estop-state-machine).
	if err = (&controller.RobotEstopReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Estopper:  safetyDispatcher,
		Resumer:   safetyDispatcher,
		Audit:     auditRecorder,
		Recorder:  mgr.GetEventRecorderFor("robotestop"),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RobotEstop")
		os.Exit(1)
	}

	// Namespace-scope estop (RFC-0001 #safety-emergency-stop-protocol estop scopes): a `swarmada.io/estop-triggered`
	// annotation on the namespace's SwarmadaConfig confirmed-estops every robot in
	// the namespace; removing it clears. SAR-gated by the SwarmadaConfig webhook.
	if err = (&controller.NamespaceEstopReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Estopper:  safetyDispatcher,
		Audit:     auditRecorder,
		Recorder:  mgr.GetEventRecorderFor("namespace-estop"),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NamespaceEstop")
		os.Exit(1)
	}

	// ── Telemetry live feed (RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split) ──────────────────────────────────
	// ControlStream server → Ingestor → Projector → StatusSink. The Projector is
	// the RA-1 gate: it forwards to the StatusSink ONLY on a material transition,
	// so Robot.status is never written on a per-tick telemetry frame. The
	// high-cadence TSDB plane defaults to drop (NoopWriter) until the RFC-0001 #swarmadaconfig
	// SwarmadaConfig telemetry-sink block (next §E item) selects a real store.
	//
	// SECURITY (RFC-0001 #fleet-adapter-protocol-transport-security, ADR-0025): the ControlStream terminates mTLS when
	// the three --fleet-adapter-tls-* flags are set. If the bind address is set but
	// no TLS material is provided, the server is registered only under
	// --fleet-adapter-insecure-authz (dev/demo); otherwise it fails closed (logged,
	// not served) rather than serving plaintext. The flag together with the TLS
	// material is refused: see controlStreamPosture.
	if fleetAdapterAddr != "" {
		var controlStreamTLS *tls.Config
		if controlStreamTLS, err = buildControlStreamTLS(fleetAdapterTLSCertFile, fleetAdapterTLSKeyFile, fleetAdapterClientCAFile); err != nil {
			setupLog.Error(err, "invalid ControlStream mTLS configuration")
			os.Exit(1)
		}
		if err = controlStreamPosture(controlStreamTLS, fleetAdapterInsecure); err != nil {
			setupLog.Error(err, "invalid ControlStream mTLS configuration")
			os.Exit(1)
		}
		statusSink := &controller.RobotStatusSink{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}
		// Per-namespace telemetry resolution (RFC-0001 #swarmadaconfig-telemetry-pipeline-and-status-projection): each frame is routed to the
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
		// Zone derivation consumes the position fan-out upstream of the sink (RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split).
		ingestor := telemetry.NewRouter(telemetryResolver, statusSink,
			telemetry.WithPositionObserver(zoneController))
		// Bind the projector cache to Robot object identity. Without this, a Robot
		// deleted and recreated under the same swarmada.io/robot-id keeps the previous
		// generation's projector state, an unchanging telemetry stream matches it, and
		// the recreated Robot never gets status.batteryPercent or status.hardware again
		// until this process restarts. Reconciles do not begin until mgr.Start() below,
		// so the Router is always attached before the first Observe.
		robotProjection.SetForgetter(ingestor, zoneController)
		// The Router is also the per-robot last-frame clock: it sees every robot's own
		// frames, so the Robot reconciler can mark a robot silent behind a live adapter
		// Offline. Set before mgr.Start(), like the forgetter above.
		robotReconciler.Frames = ingestor
		// The same clock is the estop resume witness: a cleared robot leaves Resuming
		// only on a frame of its own received after the clear, never on adapter
		// presence (RFC-0001 #safety-estop-state-machine). Without ControlStream there
		// is no witness, and a cleared robot stays Resuming.
		safetyDispatcher.Telemetry = ingestor
		// --fleet-adapter-insecure-authz (dev only): a nil Authorizer activates the
		// existing dev-mode path in controlstream.Server — every message is
		// authorized and a loud warning is logged at connect. Off by default; the
		// real, fail-closed Authorizer is always used unless explicitly disabled.
		var authorizer controlstream.Authorizer = &streamauth.Authorizer{Client: mgr.GetClient()}
		if fleetAdapterInsecure {
			authorizer = nil
			setupLog.Info("WARNING: --fleet-adapter-insecure-authz=true — ControlStream authorization " +
				"is DISABLED. Every connected adapter is authorized for every robot regardless of mTLS " +
				"identity. This is a dev/demo-only posture (RFC-0001 #fleet-adapter-protocol-transport-security, RFC-0001 #security-fleet-adapter-authentication-robot-identity) and must never be " +
				"set in a production deployment.")
		}
		// The D1 in-place supersession transition is written by the action-status ingestor rather
		// than in the reconciler (no cancel_action is sent on that path, so the reconciler never
		// sees it), which is why the Event recorder and the audit chain are wired onto it too. A
		// failure report under a stop, and every retained outcome, is decided on the API server's
		// copy: the API reader is required.
		actionStatus, asErr := controller.NewActionStatusIngestor(mgr.GetClient(), mgr.GetAPIReader(),
			mgr.GetEventRecorderFor("actionstatus-ingestor"), auditRecorder)
		if asErr != nil {
			setupLog.Error(asErr, "unable to build the action status ingestor")
			os.Exit(1)
		}
		csServer := &controlstream.Server{
			Ingestor: ingestor,
			// Robot-reported action state (§9.2.3): advances an assigned FleetAction to
			// InProgress + startedAt on RUNNING. Terminal transitions are handled by
			// the reconciler under the single-executor invariant.
			ActionStatus: actionStatus,
			// Advisory per-robot rollout progress (RFC-0001 #firmwarerollout-schema and #modelrollout-schema): surfaced as the active
			// FirmwareRollout/ModelRollout's currentBatch updatePhase.
			UpdateProgress: &controller.UpdateProgressIngestor{Client: mgr.GetClient(), Acks: modelAcks},
			// Adapter action-catalog projection (§9.2): projects supported_actions
			// from a CapabilitiesSnapshot onto FleetAdapter.status.supportedActions.
			Capabilities: &controller.CapabilitiesIngestor{Client: mgr.GetClient()},
			// Two-phase discovery handshake (RFC-0001 #fleet-adapter-protocol-connection-lifecycle, RFC-0001 #control-plane-swarmada-api-server): Discover creates a
			// status-only DiscoveredRobot (populating macAddress + reported
			// inventory) for an unadmitted robot and rejects a malformed robot_id
			// with INVALID_ROBOT_ID; Register confirms an admitted Robot.
			Registrar: &registrar.Registrar{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()},
			// Per-message robot_id authorization (RFC-0001 #security-fleet-adapter-authentication-robot-identity): every robot-scoped
			// message is checked against the adapter's mTLS identity and refused on
			// failure (fail closed). nil (dev only, --fleet-adapter-insecure-authz)
			// disables this — see the construction above.
			Authorizer: authorizer,
			// Confirmed estop delivery over SafetyStream (RFC-0001 #safety-emergency-stop-protocol): pushes estops and
			// records STOPPED only on an adapter-CONFIRMED EstopAck. Shared with the
			// zone-estop fan-out so both push over the same live SafetyStreams.
			Safety: safetyDispatcher,
			// Server→adapter command-push (§9.2, §E-2): the RobotProbe Prober built
			// above; registers each live ControlStream and routes CommandResults.
			Commands: cmdDispatcher,
			// Adapter-initiated shared-resource reservations (RFC-0001 #tde-shared-resource-queue-management) go through the
			// shared TDE engine, so adapter and scheduler act on one authority.
			Reservations: tdeResourceReserver{tdeEngine},
			// On a release that promotes a queued waiter, push it the async
			// reservation_granted Command over its own adapter stream (RFC-0001 #tde-shared-resource-queue-management).
			GrantNotifier: cmdDispatcher,
			// Adapter connectivity → FleetAdapter.status.phase (RFC-0001 #fleetadapter-status-fields-and-phase-machine). NOT driven
			// by telemetry frames (RA-1).
			Presence: fleetAdapterReconciler,
			// Tamper-evident safety audit log (RFC-0001 #security-audit-logging): a denied authorization is
			// recorded, never silently dropped. Shared with the zone-estop fan-out. An
			// in-memory sink for now; a durable/external sink is a deployment concern.
			Audit: auditRecorder,
			Log:   ctrl.Log.WithName("controlstream"),
			// Streams are served only while this process leads, and closed when it stops
			// (RFC-0001 #safety-robot-stop-table).
			Leadership: leaderState,
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

	// ModelPolicy training-completion webhook (RFC-0001 #modelpolicy-training-pipeline-integration-webhook-payload): verifies the HMAC
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
	// RFC-0001 #robotclass) runs before the FleetAdapter admission gate (validating,
	// RFC-0001 #fleetadapter) so the gate observes the fully-resolved spec.
	// Set ENABLE_WEBHOOKS=false for envtest / local `make run` without serving certs.
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		// The estop-processed marker guard admits marker writes from exactly the principal
		// this manager runs as, read from its own mounted token, so it follows whatever
		// ServiceAccount name and namespace the install rendered. Without it every marker
		// write is refused, the controller's own included, so say so loudly. The same
		// principal is what exempts the FleetTask cancel fan-out from the FleetAction
		// webhook's cancel check; without it that fan-out is refused too.
		if principal, perr := swarmadawebhook.ControllerPrincipalFromToken(swarmadawebhook.InClusterTokenPath); perr != nil {
			setupLog.Error(perr, "estop guards have no controller principal; every estop-processed marker write and every robots/status write of the estop state or a safety input (the zone, the assigned action, a bound robot's phase, a phase out of Error or into or out of Maintenance or Offline, hardware and model state, capabilities) will be refused, the manager's own included")
		} else if perr := swarmadawebhook.SetControllerPrincipal(principal); perr != nil {
			setupLog.Error(perr, "estop guards have no controller principal; every estop-processed marker write and every robots/status write of the estop state or a safety input (the zone, the assigned action, a bound robot's phase, a phase out of Error or into or out of Maintenance or Offline, hardware and model state, capabilities) will be refused, the manager's own included")
		} else {
			setupLog.Info("estop guards admit the controller principal", "principal", principal)
		}

		if err = (&swarmadawebhook.RobotDefaulter{
			Client: mgr.GetClient(),
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Robot defaulter")
			os.Exit(1)
		}

		// Estop / rollout-resume actor stamping (ADR-0046). Four mutators that carry the
		// authenticated operator from the admission request onto the object, so the
		// controllers can attribute ESTOP_TRIGGERED / ESTOP_CLEARED / ROLLOUT_RESUMED to a
		// person instead of a synthetic scope name. Robot's stamp rides RobotDefaulter
		// above (one mutating webhook per resource).
		//
		// All four are failurePolicy=Ignore: attribution is best-effort and must never
		// block a safe stop. Authorization is unaffected — the validating webhooks on the
		// same resources keep failing closed on the estop-trigger/estop-clear SAR.
		for name, setup := range map[string]func(ctrl.Manager) error{
			"FleetZone estop actor":        (&swarmadawebhook.FleetZoneEstopActorDefaulter{}).SetupWebhookWithManager,
			"SwarmadaConfig estop actor":   (&swarmadawebhook.SwarmadaConfigEstopActorDefaulter{}).SetupWebhookWithManager,
			"FirmwareRollout resume actor": (&swarmadawebhook.FirmwareRolloutResumeActorDefaulter{}).SetupWebhookWithManager,
			"ModelRollout resume actor":    (&swarmadawebhook.ModelRolloutResumeActorDefaulter{}).SetupWebhookWithManager,
		} {
			if err = setup(mgr); err != nil {
				setupLog.Error(err, "unable to create webhook", "webhook", name)
				os.Exit(1)
			}
		}

		// The zone reads (existence, leaf, composite) go through the uncached API reader: a
		// cached list can miss a child zone admitted just before the Robot.
		if err = (&swarmadawebhook.RobotAdmissionGate{
			Client:     mgr.GetClient(),
			APIReader:  mgr.GetAPIReader(),
			EstopAuthz: &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Robot")
			os.Exit(1)
		}
		// Robot DELETE: refuses the delete of a robot that holds a stop, on its own
		// webhook with failurePolicy Ignore so that a manager outage does not block a
		// Robot delete. A namespace delete still waits for a replica while the namespace
		// holds a FleetZone, FleetTask, FirmwareRollout or ModelRollout, whose DELETE
		// checks stay failurePolicy=Fail.
		if err = (&swarmadawebhook.RobotDeleteGuard{
			APIReader: mgr.GetAPIReader(),
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Robot delete")
			os.Exit(1)
		}
		// FleetAction DELETE: refuses the delete of work a robot under a stop records to anyone
		// without estop-clear on that stop, on its own webhook with failurePolicy Ignore; the stop
		// finalizer keeps such work through a manager outage.
		if err = (&swarmadawebhook.FleetActionDeleteGuard{
			APIReader:  mgr.GetAPIReader(),
			EstopAuthz: &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "FleetAction delete")
			os.Exit(1)
		}
		// SwarmadaConfig DELETE: refuses the delete of the config that carries the
		// namespace's emergency stop, outside a terminating namespace, on its own webhook
		// with failurePolicy Ignore so that this check does not hold a namespace delete
		// through a manager outage.
		if err = (&swarmadawebhook.SwarmadaConfigDeleteGuard{
			APIReader: mgr.GetAPIReader(),
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "SwarmadaConfig delete")
			os.Exit(1)
		}
		// robots/status: only the control plane moves status.estopState and
		// status.estopBaseline, takes a robot out of Error, clears a Failed component or
		// changes its capabilities. swarmada:admin writes robots/status too; the adapter
		// role does not.
		if err = (&swarmadawebhook.RobotStatusEstopGuard{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Robot status estop")
			os.Exit(1)
		}

		// SwarmadaConfig cross-field validation (RFC-0001 #swarmadaconfig): AfterTimeout⇒timeout,
		// disconnectedReservationTTLSeconds>disconnectTimeoutSeconds, real-sink⇒endpoint,
		// requireSignatureVerification⇒trustRoots.
		if err = (&swarmadawebhook.SwarmadaConfigValidator{
			EstopAuthz: &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "SwarmadaConfig")
			os.Exit(1)
		}

		// FleetZone cross-resource validation (RFC-0001 #control-plane-swarmada-api-server): parentZone existence + acyclicity,
		// simple-polygon boundary, and refusing the delete of a zone anything still references
		// (RFC-0001 #fleetzone-deleting-a-zone). Every check reads through the uncached API
		// reader: a cached list can miss a robot or action admitted just before the delete, and
		// a cached parent can miss the write that would make a new parentZone close a cycle.
		if err = (&swarmadawebhook.FleetZoneValidator{
			Client:     mgr.GetClient(),
			APIReader:  mgr.GetAPIReader(),
			EstopAuthz: &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "FleetZone")
			os.Exit(1)
		}

		// ZoneMaintenance scope resolution (RFC-0001 #control-plane-swarmada-api-server): a Zone-scoped window must name a
		// FleetZone that exists, or it silently pauses nothing.
		if err = (&swarmadawebhook.ZoneMaintenanceValidator{
			Client: mgr.GetClient(),
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "ZoneMaintenance")
			os.Exit(1)
		}

		// FleetAction cancel-verb SAR enforcement (RFC-0001 #security-authorization-swarmada-rbac-roles): writing the
		// swarmada.io/cancel-requested annotation requires the `cancel` verb.
		if err = (&swarmadawebhook.FleetActionValidator{
			CancelAuthz: &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
			Reader:      mgr.GetClient(),
			APIReader:   mgr.GetAPIReader(),
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "FleetAction")
			os.Exit(1)
		}

		// FleetTask append-only validator (RFC-0001 §9.1.5): enforces append-only spec.actions +
		// immutable policies, gates action appends behind the scoped `append` SAR verb and a
		// desiredState change to Cancelled behind the `cancel` SAR verb, gates clearing
		// spec.draft behind the `activate` SAR verb and refuses it to every ServiceAccount,
		// and refuses `delete` on a live composite (cancel first — the confirmed-cancel path).
		// Reader resolves the ComputeEndpoint behind a select subject's producer, so
		// admission decides the result-versus-event binding from the producer's declared
		// contract rather than leaving it to the author (check S6).
		if err = (&swarmadawebhook.FleetTaskValidator{
			AppendAuthz:   &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
			CancelAuthz:   &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
			ActivateAuthz: &swarmadawebhook.SARAuthorizer{Client: mgr.GetClient()},
			Reader:        mgr.GetClient(),
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "FleetTask")
			os.Exit(1)
		}

		// ComputeWorkloadTemplate containment: refuses a template whose pod spec could
		// escape to the node (hostPath, a host namespace, privileged mode, added
		// capabilities, a host port). This is the container half of the policy; the
		// pod-level half is CEL on the CRD. It runs failurePolicy=Fail because the
		// workload it guards is created under THIS manager's ServiceAccount — without
		// it, create on computeworkloadtemplates would be create on privileged pods.
		if err = (&swarmadawebhook.ComputeWorkloadTemplateValidator{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "ComputeWorkloadTemplate")
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

	// TDE recovery (RFC-0001 #tde-restart-recovery) runs as a LEADER-ELECTED runnable: reservation
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
		// to the conservative action (release all, RFC-0001 #tde-restart-recovery). Defaults per RFC-0001 #swarmadaconfig-traffic-deconfliction-engine-configuration.
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

	// RBAC preflight: confirm this ServiceAccount can actually read the
	// resources the G1 admission gate consults, BEFORE serving admission. A missing
	// grant would otherwise degrade that gate to a silent no-op rather than failing
	// visibly — see internal/rbac.Preflight for why the ordinary error path does not
	// surface it. Uses the uncached API reader deliberately. The check is cluster-wide,
	// not the manager's own namespace: G1 reads in the namespace of every action it
	// admits, so a grant confined to POD_NAMESPACE would pass here and leave the gate
	// open everywhere else. "" is every namespace.
	if err := rbacpreflight.Preflight(context.Background(), mgr.GetAPIReader(), ""); err != nil {
		setupLog.Error(err, "RBAC preflight failed")
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

// controlStreamPosture refuses --fleet-adapter-insecure-authz alongside a working
// ControlStream mTLS configuration. ADR-0025 makes the flag the single switch for the
// dev posture: no transport identity and no per-message authorization, together. With
// mTLS configured the identity exists, and the flag would switch off only the check
// that scopes each adapter to its own robots — a deployment that reads as
// authenticated while any adapter the CA has issued may act for every robot in the
// namespace. The manager says so and exits rather than serving it.
func controlStreamPosture(tlsCfg *tls.Config, insecure bool) error {
	if tlsCfg != nil && insecure {
		return errors.New("--fleet-adapter-insecure-authz cannot be combined with ControlStream mTLS " +
			"(--fleet-adapter-tls-cert-file, --fleet-adapter-tls-key-file, --fleet-adapter-client-ca-file): " +
			"it would disable the per-robot authorization that the verified adapter identity exists for; " +
			"drop the flag and list each robot's class in its FleetAdapter's spec.servesRobotClasses")
	}
	return nil
}

// buildControlStreamTLS assembles the server TLS config for ControlStream mTLS
// (RFC-0001 #fleet-adapter-protocol-transport-security, ADR-0025). It returns (nil, nil) when none of the three files
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
