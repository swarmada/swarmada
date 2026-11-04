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
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/telemetry"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// ProtocolVersion is the ControlStream protocol version this control plane
// speaks, negotiated in HelloAck (RFC-0001 §9.2.3). An AdapterHello requesting
// this version — or leaving protocol_version empty — is accepted.
const ProtocolVersion = "fleet_adapter.v1"

// AdapterIdentity is the provisional identity of a connected adapter, taken from
// its AdapterHello.
//
// It is NOT a security boundary. RFC-0001 #fleet-adapter-protocol-transport-security binds an adapter's identity to
// its mTLS client-certificate SAN and authorizes every message's robot_id
// against it; that enforcement is deferred. Until then these self-reported
// fields are untrusted hints for logging and for the Registrar's own checks.
type AdapterIdentity struct {
	// Vendor matches FleetAdapter.spec.vendor.
	Vendor string
	// ContractVersion is the fleet-adapter contract semver the adapter reported (ADR-0032). Like
	// every other field here it is a self-reported hint, NOT a security boundary.
	ContractVersion string
	// ContractCompatible is the control plane's verdict on ContractVersion against the compiled-in
	// supported range, decided once at the handshake. False refuses robot registration on this
	// stream (VERSION_MISMATCH) while leaving telemetry, heartbeat and estop untouched.
	ContractCompatible bool
	// NegotiatedContractVersion is the contract version agreed for this connection, as the
	// HelloAck stated it: 1.4.0, a 1.3.x, or empty when none was agreed (contract.Negotiate).
	NegotiatedContractVersion string
	// AdapterVersion is the adapter build/semver it reported.
	AdapterVersion string
	// ProtocolVersion is the version the adapter requested at handshake.
	ProtocolVersion string
	// Namespace is the Swarmada namespace the adapter claims to serve.
	Namespace string
	// PeerAddr is the transport remote address, for diagnostics only.
	PeerAddr string
}

// Registrar answers the two-phase discovery messages an adapter sends for each
// managed robot (RFC-0001 #fleet-adapter-protocol-connection-lifecycle, RFC-0001 #control-plane-swarmada-api-server). Implementations create or look up the
// backing DiscoveredRobot/Robot resources. Per the in-band error model (RFC-0001 #fleet-adapter-protocol-in-band-error-model),
// a registration outcome — including a control-plane-side failure — is reported
// as an ack with accepted=false and a message, never as a gRPC status code
// (those are reserved for connection-level faults). Implementations MUST be safe
// for concurrent use: one Registrar serves every adapter stream.
type Registrar interface {
	// Register handles a reconnect/registration of an already-admitted robot and
	// returns the RegisterAck to send back.
	Register(ctx context.Context, id AdapterIdentity, msg *fav1.RegisterRobot) *fav1.RegisterAck
	// Discover handles a first-time announce of an unadmitted robot and returns
	// the DiscoverAck to send back. tlsID is the VERIFIED mTLS identity (RFC-0001 #fleet-adapter-protocol-transport-security):
	// the registrar derives the auto-admit suggested class from the adapter's own
	// FleetAdapter, keyed by tlsID.AdapterName — never from the self-reported
	// AdapterIdentity (ADR-0027).
	Discover(ctx context.Context, id AdapterIdentity, tlsID TLSIdentity, msg *fav1.DiscoverRobot) *fav1.DiscoverAck
}

// ErrRobotNotAdmitted is returned (wrapped) by an Authorizer when the robot_id is
// authentic and simply has no Robot object yet — the robot exists physically and has
// not been through operator admission.
//
// It is deliberately NARROWER than "denied". An adapter reaching for a robot bound to
// SOMEONE ELSE, an unverified identity, or a class it does not serve are all genuine
// authorization violations and must stay opaque: answering those with NOT_ADMITTED
// would tell a hostile adapter that a robot is unclaimed and invite it to announce
// one it has no business announcing. This error means only "no such Robot", which is
// exactly what AuthorizeAnnounce already permits an adapter to discover for itself
// ("not yet admitted — a first announce is permitted"), so distinguishing it leaks
// nothing the same adapter could not learn by sending a Discover.
var ErrRobotNotAdmitted = errors.New("robot is not admitted")

// ErrClassNotServed is returned (wrapped) by an Authorizer when the one condition a
// message fails is the class: the adapter's identity is verified, the robot exists in its
// namespace and is bound to it, and the adapter's FleetAdapter lists classes, none of them the
// robot's RobotClass. An Authorizer wraps it only when every other condition holds; a lookup
// error never wraps it, and neither does an empty list, which serves every class at admission.
//
// It is NARROWER than "denied", as ErrRobotNotAdmitted is. The class is a condition of the
// admission gate, and the gate MUST NOT block observation or stopping (RFC-0001
// #fleetadapter-admission-gating). So a message that observes or stops the robot — telemetry,
// a heartbeat naming it, an EstopAck, a LocalStopReport — is taken from the robot's own
// adapter after a spec.servesRobotClasses edit drops its class (authorizeObserve), and every
// other arm is still refused. Candidate filter 9 withholds the robot from new work on the same
// condition (internal/controller.adapterListsClass): its telemetry keeps it Idle, so its phase no
// longer does.
var ErrClassNotServed = errors.New("adapter does not serve the robot's class")

// ErrAdapterNotFound is returned (wrapped) by an Authorizer when the one condition a message
// fails is the FleetAdapter's existence: the adapter's identity is verified, the robot exists in
// its namespace and is bound to it, and a read of the FleetAdapter the robot names found no such
// object. An Authorizer wraps it only when every other condition holds; any other lookup error
// never wraps it.
//
// It is NARROWER than "denied", as ErrClassNotServed is. The FleetAdapter's existence is a
// condition of the admission gate, which MUST NOT block observation or stopping (RFC-0001
// #fleetadapter-admission-gating). So once that FleetAdapter is deleted, the robot's own adapter
// is still heard on the four arms authorizeObserve serves, and every other arm is still refused.
// Candidate filter 9 withholds the robot from new work on the same condition
// (internal/controller.filterDispatchEligible: an absent adapter is not dispatch-ready).
var ErrAdapterNotFound = errors.New("the robot's FleetAdapter does not exist")

// Authorizer decides whether the mTLS-identified adapter may act on a robot
// (RFC-0001 #security-fleet-adapter-authentication-robot-identity, #fleet-adapter-protocol-transport-security). It MUST fail closed: an unverified identity, an
// unknown robot, an adapter that does not own or serve the robot, or any lookup
// error all return a non-nil error, and the message is refused. Two wrapped errors,
// ErrClassNotServed and ErrAdapterNotFound, let the arms that observe or stop the robot
// through; an implementation MUST wrap each only in the one case its doc comment names.
// Implementations MUST be safe for concurrent use across adapter streams.
type Authorizer interface {
	// AuthorizeRobot authorizes a message acting on an already-admitted robot.
	AuthorizeRobot(ctx context.Context, adapter TLSIdentity, robotID string) error
	// AuthorizeAnnounce authorizes a first-time Discover of a robot: the id must
	// not already be bound to a different adapter in the namespace.
	AuthorizeAnnounce(ctx context.Context, adapter TLSIdentity, robotID string) error
}

// AdapterPresence receives Fleet Adapter connectivity events so the FleetAdapter
// status controller can drive status.phase (RFC-0001 #fleetadapter-status-fields-and-phase-machine). It is called ONLY
// on connect, on a liveness Heartbeat, and on stream loss — NEVER on a telemetry
// frame (adapter liveness is not a telemetry tick, RA-1). Implementations MUST be
// safe for concurrent use and non-blocking-friendly (called from the stream loop).
type AdapterPresence interface {
	// AdapterConnected reports an accepted handshake from a verified adapter, along with what the
	// handshake agreed. An INCOMPATIBLE contract version is reported here rather than suppressed:
	// the connection is real and must be reflected on status, it just may not be given work
	// (ADR-0032) — hiding it would make a version mismatch look like an absent adapter.
	AdapterConnected(ctx context.Context, identity TLSIdentity, negotiated Negotiation)
	// AdapterHeartbeat reports a liveness Heartbeat from a verified adapter.
	AdapterHeartbeat(ctx context.Context, identity TLSIdentity)
	// AdapterDisconnected reports the adapter's stream closing or faulting. It is not
	// called for a stream that another stream under the same identity replaced: the
	// adapter is still connected, on the newer stream.
	AdapterDisconnected(ctx context.Context, identity TLSIdentity)
}

// AdapterSupersession is satisfied by an AdapterPresence that records one ControlStream
// replacing another under the same adapter identity. It is reached by type assertion, as
// HeartbeatRouter is, so a presence sink that does not record it keeps compiling.
// supersededPeer and newPeer are transport addresses, for the operator only.
type AdapterSupersession interface {
	AdapterSuperseded(ctx context.Context, identity TLSIdentity, supersededPeer, newPeer string)
}

// SafetyStreamSupersession is AdapterSupersession for the SafetyStream: an AdapterPresence
// that satisfies it records one SafetyStream replacing another under the same adapter
// identity. Reached by type assertion, like AdapterSupersession.
type SafetyStreamSupersession interface {
	SafetyStreamSuperseded(ctx context.Context, identity TLSIdentity, supersededPeer, newPeer string)
}

// Negotiation is what an AdapterHello handshake agreed. It carries two different kinds of version:
// ProtocolVersion is the wire-package IDENTITY ("fleet_adapter.v1"), which cannot express
// compatibility, and ContractVersion is the ADR-0032 contract semver, which can.
//
// ContractVersion is the version the adapter REPORTED, recorded whether or not it is supported, so
// an operator can see what an incompatible adapter actually speaks. ContractCompatible is the
// verdict; it is the field to branch on, never a string comparison at the call site.
type Negotiation struct {
	ProtocolVersion    string
	ContractVersion    string
	ContractCompatible bool
}

// SafetySender serialises server→adapter sends on one SafetyStream. A gRPC
// stream's Send is not safe for concurrent use; the implementation guards it.
type SafetySender interface {
	Send(*fav1.ControlPlaneSafetyMessage) error
}

// CommandSender serialises server→adapter sends on one ControlStream. A gRPC
// stream's Send is not safe for concurrent use; the implementation guards it.
// The streamWriter that backs each live ControlStream satisfies it.
type CommandSender interface {
	Send(*fav1.ControlPlaneMessage) error
}

// HeartbeatRouter is satisfied by a CommandDispatcher that can also complete a pending
// per-robot heartbeat exchange (RFC-0001 #safety-control-plane-response). Kept separate from CommandDispatcher,
// and reached by type assertion, so an implementation that does not need the confirming
// exchange — the tests' stub dispatchers, and any embedder — keeps compiling unchanged.
type HeartbeatRouter interface {
	RouteHeartbeat(namespace, robotID string)
}

// RegisterNotifier is implemented by a CommandDispatcher that gives a robot work on a stream only
// once its register has been processed there. The Server calls RobotRegistered after it has sent
// an accepting RegisterAck, while this process leads, from the stream's receive loop: the
// dispatcher sends the fences of the stops in force down sender first, then marks the robot ready.
type RegisterNotifier interface {
	RobotRegistered(ctx context.Context, sender CommandSender, namespace, robotID string)
}

// CommandDispatcher owns server→adapter command-push over ControlStream (RFC-0001
// §9.2). The Server registers each live ControlStream with it and
// routes inbound CommandResults to it, correlated by command_id; the dispatcher
// pushes Commands (verify_*, renew_lease, model_update, …) and awaits their
// result. Implementations MUST be safe for concurrent use across adapter streams.
// [github.com/swarmada/swarmada/internal/command.Dispatcher] satisfies it.
type CommandDispatcher interface {
	// RegisterStream records a live ControlStream and returns a deregister func.
	RegisterStream(identity TLSIdentity, sender CommandSender) func()
	// RouteResult delivers an inbound CommandResult for correlation. sender is the
	// stream it arrived on, the same value that stream was registered with. A result
	// resolves a command only if it arrived on the stream the command was pushed down
	// and names the robot the command was pushed for; otherwise RouteResult drops it
	// and returns an error wrapping ErrMisroutedResult. A result for a command id that
	// is not pending (late, or never issued) is dropped and returns nil.
	RouteResult(sender CommandSender, result *fav1.CommandResult) error
}

// ErrMisroutedResult is returned (wrapped) by a CommandDispatcher for a CommandResult that
// names a pending command but did not come back the way that command went: a different
// stream, or a different robot. The command id alone is not evidence of who is answering.
var ErrMisroutedResult = errors.New("CommandResult does not answer a command sent on this stream for this robot")

// SafetyDispatcher owns estop delivery over SafetyStream (RFC-0001 #safety-emergency-stop-protocol). The
// Server registers each live stream with it and routes inbound EstopAcks to it;
// the dispatcher pushes estops and tracks their CONFIRMED outcome. Implementations
// MUST be safe for concurrent use across adapter streams.
// [github.com/swarmada/swarmada/internal/safety.Dispatcher] satisfies it.
type SafetyDispatcher interface {
	// RegisterStream records a live SafetyStream and returns a deregister func.
	RegisterStream(identity TLSIdentity, sender SafetySender) func()
	// RouteAck delivers an inbound EstopAck for correlation. identity is the mTLS
	// identity of the stream it arrived on, sender that stream (the same value it was
	// registered with), and robotID its envelope robot_id, already authorized against
	// that identity. An implementation resolves only an estop it pushed down that
	// stream, to that adapter, for that robot in that namespace: the ack names the
	// estop, but the robot it confirms is the one the control plane wrote when it sent
	// it. An ack for a pending estop that did not come back the way it went is dropped
	// with an error wrapping ErrMisroutedAck; an ack for no pending estop is dropped
	// and returns nil.
	RouteAck(identity TLSIdentity, sender SafetySender, robotID string, ack *fav1.EstopAck) error
	// RouteClearAck delivers an inbound EstopClearAck: the adapter lifted the stop an EstopClear
	// named. identity, sender and robotID are as for RouteAck. An implementation takes it only from
	// the adapter's current SafetyStream, and never blocks the receive loop.
	RouteClearAck(identity TLSIdentity, sender SafetySender, robotID string, ack *fav1.EstopClearAck) error
}

// ContractObserver is told the contract version each verified adapter reported on its
// ControlStream, when the SafetyDispatcher implements it. An adapter below 1.4.0 has no
// EstopClear, and the clear then removes the stops it ends instead (RFC-0001
// #safety-robot-stop-table, Removing a recorded stop).
type ContractObserver interface {
	ContractReported(identity TLSIdentity, version string)
}

// StreamContractObserver is told the contract version agreed for each ControlStream registered with
// the CommandDispatcher, when the CommandDispatcher implements it. Row A15's send check reads it: an
// AssignAction goes only on a stream that negotiated 1.4.0 (RFC-0001 #safety-action-table, A15).
type StreamContractObserver interface {
	StreamNegotiated(sender CommandSender, version string)
}

// ErrMisroutedAck is returned (wrapped) by a SafetyDispatcher for an EstopAck that names a
// pending estop but did not arrive on the SafetyStream that estop was pushed down. The
// estop_id alone is not evidence the robot is at rest.
var ErrMisroutedAck = errors.New("EstopAck does not answer an estop sent on this stream for this robot")

// TelemetryIngestor receives one translated frame per TelemetryPayload.
// [github.com/swarmada/swarmada/internal/telemetry.Ingestor] satisfies it
// directly. Implementations MUST be safe for concurrent use across adapter
// streams.
type TelemetryIngestor interface {
	// Ingest handles one telemetry frame (two-data-plane split, RFC-0001 #control-plane-telemetry-pipeline-and-the-two-data-plane-split).
	Ingest(ctx context.Context, f telemetry.Frame) error
}

// ActionStatusIngestor consumes an adapter ActionStatusUpdate (§9.2.3), advancing the
// FleetAction lifecycle from the robot's reported state. Implementations MUST be
// safe for concurrent use and MUST respect the fencing token (single-executor).
//
// An ActionStatusUpdate names an action, not a robot, so the per-message gate in dispatch
// cannot authorize it; the robot it acts on is the one the action is bound to, and only
// the ingestor reads that. authorize is the stream's per-robot check for that robot. An
// implementation MUST call it before the report may move the action, and MUST refuse the
// report when it returns false or is nil.
type ActionStatusIngestor interface {
	IngestActionStatus(ctx context.Context, namespace string, authorize RobotAuthorization, u *fav1.ActionStatusUpdate) error
}

// RobotAuthorization reports whether the stream a message arrived on may act on robotID.
// It is the stream's authorizeRobot bound to the stream's identity, handed to a consumer
// that learns the robot only after the message has passed the dispatch gate. A denial is
// recorded by the check itself (ROBOT_AUTHZ_DENIED); the caller only honours it.
type RobotAuthorization func(ctx context.Context, robotID string) bool

// UpdateProgressIngestor consumes an adapter UpdateProgress (RFC-0001 #firmwarerollout-schema and #modelrollout-schema), surfacing
// per-robot intra-update progress as the active rollout's
// status.currentBatch[].updatePhase. It is advisory — a nil ingestor drops it and
// the phase stays at the control plane's initial value.
type UpdateProgressIngestor interface {
	IngestUpdateProgress(ctx context.Context, namespace string, u *fav1.UpdateProgress) error
}

// CapabilitiesIngestor projects an adapter's CapabilitiesSnapshot supported-action
// catalog onto FleetAdapter.status (§9.2). A nil Server.Capabilities drops it.
type CapabilitiesIngestor interface {
	IngestCapabilities(ctx context.Context, namespace string, snap *fav1.CapabilitiesSnapshot) error
}

// ResourceReserveState is the outcome of an adapter shared-resource reservation.
type ResourceReserveState string

// ResourceReserveState values (map onto the wire ReservationState).
const (
	ResourceGranted ResourceReserveState = "Granted"
	ResourceQueued  ResourceReserveState = "Queued"
	ResourceDenied  ResourceReserveState = "Denied"
)

// ResourceDecision is the reserver's synchronous verdict for a ResourceRequest.
type ResourceDecision struct {
	State         ResourceReserveState
	QueuePosition int
	Message       string
	Released      bool
	// PromotedRobotID, on a release, is the robot promoted to holder (empty if none);
	// the server pushes it an async reservation_granted.
	PromotedRobotID string
}

// ResourceReserver is the narrow TDE view the server needs for adapter-initiated
// shared-resource reservations (RFC-0001 #tde-shared-resource-queue-management). The engine implements it (adapted). It
// MUST be safe for concurrent use and fail closed (Denied) on any uncertainty.
type ResourceReserver interface {
	ReserveResource(ctx context.Context, namespace, resourceName, robotID string) ResourceDecision
	ReleaseResource(ctx context.Context, namespace, resourceName, robotID string) ResourceDecision
}

// ResourceGrantNotifier pushes an async reservation_granted Command to a robot
// whose queued shared-resource reservation was promoted to holder (RFC-0001 #tde-shared-resource-queue-management). The
// command Dispatcher satisfies it. Best-effort: a failed push leaves the robot
// waiting at the boundary (fail-safe — it never proceeds un-granted).
type ResourceGrantNotifier interface {
	NotifyReservationGranted(ctx context.Context, namespace, robotID, resourceName string)
}

// Server is the control-plane ControlStream endpoint (RFC-0001 §9.2). Its zero
// value is usable: an unset Registrar safely rejects all registrations and an
// unset Ingestor drops telemetry (no Robot.status write ever happens on a
// telemetry tick — RA-1). It implements fav1.FleetAdapterServiceServer;
// SafetyStream remains Unimplemented until that enforcement lands.
type Server struct {
	fav1.UnimplementedFleetAdapterServiceServer

	// Registrar answers Discover/Register. Nil rejects all registrations.
	Registrar Registrar
	// Ingestor receives translated telemetry frames. Nil drops telemetry.
	Ingestor TelemetryIngestor
	// ActionStatus consumes adapter ActionStatusUpdate messages (§9.2.3). Nil drops
	// action status (the FleetAction lifecycle then advances only via the reconciler).
	ActionStatus ActionStatusIngestor
	// UpdateProgress consumes adapter UpdateProgress messages (RFC-0001 #firmwarerollout-schema and #modelrollout-schema). Nil drops
	// them (rollout currentBatch phases then stay at the controller's initial value).
	UpdateProgress UpdateProgressIngestor
	// Capabilities projects an adapter CapabilitiesSnapshot's supported-action
	// catalog onto FleetAdapter.status (§9.2). Nil drops the snapshot.
	Capabilities CapabilitiesIngestor
	// Authorizer enforces per-message robot_id authorization (RFC-0001 #security-fleet-adapter-authentication-robot-identity). When set,
	// every robot-scoped message is checked against the adapter's mTLS identity and
	// refused on failure (fail closed). Nil disables per-robot authorization — a
	// dev-mode posture, logged loudly at connect, that MUST NOT be used in
	// production (RFC-0001 #fleet-adapter-protocol-transport-security).
	Authorizer Authorizer
	// Safety handles estop delivery over SafetyStream (RFC-0001 #safety-emergency-stop-protocol). Nil leaves
	// SafetyStream Unavailable.
	Safety SafetyDispatcher
	// Commands handles server→adapter command-push over ControlStream (§9.2,
	// §E-2): each verified stream is registered with it and inbound CommandResults
	// are routed to it. Nil disables command-push (active probes report Unknown).
	Commands CommandDispatcher
	// Reservations services adapter-initiated shared-resource reservations (RFC-0001 #tde-shared-resource-queue-management).
	// Nil denies every ResourceRequest (fail closed — a resource is never granted
	// when the TDE view is absent).
	Reservations ResourceReserver
	// GrantNotifier pushes async reservation_granted Commands on queue promotion
	// (RFC-0001 #tde-shared-resource-queue-management). Nil skips the push (the promotion is still persisted to status).
	GrantNotifier ResourceGrantNotifier
	// Presence receives adapter connectivity events (connect / liveness Heartbeat /
	// disconnect) so the FleetAdapter status controller can drive status.phase
	// (RFC-0001 #fleetadapter-status-fields-and-phase-machine). Nil disables presence reporting.
	Presence AdapterPresence
	// Audit records tamper-evident safety/security events (RFC-0001 #safety-safety-audit-log). Nil disables
	// audit recording (dev). A denied authorization is always recorded when set —
	// denied actions are never silently dropped.
	Audit audit.Recorder
	// Log is the structured logger. The zero (unset) value discards output.
	Log logr.Logger
	// Leadership is this process's hold on leadership (internal/leadership.State). A stream
	// opened while this process does not lead is refused with Unavailable, and an open stream
	// is closed when leadership is lost (see leadership.go). Nil serves every stream.
	Leadership Leadership

	// sessions holds the live ControlStream for each verified adapter identity, keyed
	// by namespace/adapter. One identity has one stream: see claimSession.
	// safetySessions does the same for SafetyStreams. Both are guarded by sessionsMu.
	sessionsMu     sync.Mutex
	sessions       map[string]*session
	safetySessions map[string]*session
}

// session is one accepted ControlStream held under a verified adapter identity.
type session struct {
	peer       string
	superseded chan struct{} // closed when a newer stream under the same identity is accepted
	once       sync.Once
}

func (ss *session) supersede() { ss.once.Do(func() { close(ss.superseded) }) }

func sessionKey(identity TLSIdentity) string { return identity.Namespace + "/" + identity.AdapterName }

// claimSession makes a new ControlStream the live one for its verified identity, and
// returns the stream it replaces, if any. Commands, and the liveness beats that ride with
// them, can go down only one stream per identity, so a second accepted stream replaces the
// first. The newer stream wins because a reconnect is newer than the stream it replaces,
// and refusing it would strand an adapter whose old stream is half-dead and not yet
// detected. The caller closes the replaced stream with supersede once the new one can take
// commands, so no command falls between the two.
func (s *Server) claimSession(identity TLSIdentity, peer string) (ss, prev *session) {
	return s.claimIn(&s.sessions, identity, peer)
}

// claimIn is claimSession over one stream kind's session map.
func (s *Server) claimIn(sessions *map[string]*session, identity TLSIdentity, peer string) (ss, prev *session) {
	ss = &session{peer: peer, superseded: make(chan struct{})}
	key := sessionKey(identity)
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if *sessions == nil {
		*sessions = map[string]*session{}
	}
	prev = (*sessions)[key]
	(*sessions)[key] = ss
	return ss, prev
}

// supersede closes prev, the stream a newer one under identity replaced, and reports the
// replacement: a log line, and the presence sink's record of it (a Warning Event on the
// FleetAdapter).
func (s *Server) supersede(ctx context.Context, log logr.Logger, identity TLSIdentity, prev *session, peer string) {
	prev.supersede()
	log.Info("a second ControlStream under this adapter identity was accepted; closing the earlier one",
		"supersededPeer", prev.peer)
	if rec, ok := s.Presence.(AdapterSupersession); ok {
		rec.AdapterSuperseded(ctx, identity, prev.peer, peer)
	}
}

// releaseSession ends ss's hold on its identity. It reports whether ss was still the live
// stream, which is when the adapter has actually disconnected; a superseded stream's
// adapter is still connected on the stream that replaced it.
func (s *Server) releaseSession(identity TLSIdentity, ss *session) bool {
	return s.releaseIn(&s.sessions, identity, ss)
}

// releaseIn is releaseSession over one stream kind's session map.
func (s *Server) releaseIn(sessions *map[string]*session, identity TLSIdentity, ss *session) bool {
	key := sessionKey(identity)
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if (*sessions)[key] != ss {
		return false
	}
	delete(*sessions, key)
	return true
}

// supersededStatus is the status a replaced ControlStream ends with. Aborted, not
// Unavailable: the stream was closed on purpose, and an adapter that reconnects on it
// displaces the stream that replaced it.
func supersededStatus(identity TLSIdentity) error {
	return status.Errorf(codes.Aborted,
		"superseded: a newer ControlStream for adapter %q in namespace %q was accepted; one adapter identity "+
			"is served on one stream, so this one is closed", identity.AdapterName, identity.Namespace)
}

// supersededSafetyStatus is supersededStatus for a SafetyStream.
func supersededSafetyStatus(identity TLSIdentity) error {
	return status.Errorf(codes.Aborted,
		"superseded: a newer SafetyStream for adapter %q in namespace %q was accepted; one adapter identity "+
			"is served on one stream, so this one is closed", identity.AdapterName, identity.Namespace)
}

// supersedeSafety is supersede for a SafetyStream: it closes prev and reports the
// replacement through SafetyStreamSupersession (a Warning Event on the FleetAdapter).
func (s *Server) supersedeSafety(ctx context.Context, log logr.Logger, identity TLSIdentity, prev *session, peer string) {
	prev.supersede()
	log.Info("a second SafetyStream under this adapter identity was accepted; closing the earlier one",
		"supersededPeer", prev.peer)
	if rec, ok := s.Presence.(SafetyStreamSupersession); ok {
		rec.SafetyStreamSuperseded(ctx, identity, prev.peer, peer)
	}
}

// recvMessages runs stream.Recv on its own goroutine so the receive loop can also wait
// for supersession. Messages are delivered in order, and an error only after every
// message before it has been taken. The goroutine exits once done is closed.
func recvMessages(stream fav1.FleetAdapterService_ControlStreamServer, done <-chan struct{}) (<-chan *fav1.AdapterMessage, <-chan error) {
	return recvEach(stream.Recv, done)
}

// recvEach is recvMessages over any stream's Recv.
func recvEach[M any](recv func() (M, error), done <-chan struct{}) (<-chan M, <-chan error) {
	msgs := make(chan M)
	errs := make(chan error, 1)
	go func() {
		for {
			msg, err := recv()
			if err != nil {
				errs <- err
				return
			}
			select {
			case msgs <- msg:
			case <-done:
				return
			}
		}
	}()
	return msgs, errs
}

// SafetyStream terminates one adapter's SafetyStream (RFC-0001 #safety-emergency-stop-protocol, RA-6). It
// carries only estops (control plane → adapter) and EstopAcks (adapter → control
// plane); there is no AdapterHello — the identity is the mTLS client certificate.
// The stream is registered with the SafetyDispatcher so estops can be pushed, and
// each inbound EstopAck is authorized (an adapter may not confirm-stop a robot it
// does not serve) before being routed. A stray/unauthorized ack is dropped, never
// a stream teardown.
func (s *Server) SafetyStream(stream fav1.FleetAdapterService_SafetyStreamServer) error {
	ctx := stream.Context()
	if s.Safety == nil {
		return status.Error(codes.Unavailable, "SafetyStream not configured")
	}
	// Refused before it is registered for estops, and before its headers go: a process that
	// does not lead serves no adapter stream (leadership.go).
	lost, err := s.lead()
	if err != nil {
		return err
	}

	identity := IdentityFromContext(ctx)
	log := s.logger().WithValues("stream", "safety", "peer", peerAddr(ctx))
	if identity.Verified {
		log = log.WithValues("adapterIdentity", identity.AdapterName, "identityNamespace", identity.Namespace)
	} else if s.Authorizer != nil {
		log.Info("SafetyStream without a verified mTLS identity; every EstopAck will be dropped (fail closed)")
	}

	// One adapter identity, one live SafetyStream, as for the ControlStream (see
	// claimSession). The new stream is registered for estops before the stream it
	// replaces is closed, so an estop issued during the handover goes down one of the
	// two and never down neither. A SafetyStream reports no presence, so its handover
	// cannot report a disconnect.
	w := &safetyWriter{stream: stream}
	var superseded <-chan struct{}
	var replaced *session
	if identity.Verified {
		var ss *session
		ss, replaced = s.claimIn(&s.safetySessions, identity, peerAddr(ctx))
		superseded = ss.superseded
		defer s.releaseIn(&s.safetySessions, identity, ss)
	}
	deregister := s.Safety.RegisterStream(identity, w)
	defer deregister()
	if replaced != nil {
		s.supersedeSafety(ctx, log, identity, replaced, peerAddr(ctx))
	}
	// The headers tell the adapter the stream is served. Its call to SafetyStream returns
	// before the server has accepted anything, so they are the first thing it can see that
	// says an estop for its robots now comes down this stream; until then it has no estop
	// path (RFC-0001 #safety-emergency-stop-protocol). They go only after the stream is
	// registered for estops, and only under a verified identity: a stream without one is
	// never registered (internal/safety.Dispatcher keys streams by the identity), and
	// headers on it would claim a path no estop can reach. An estop re-sent onto this
	// stream while it registered has already carried them.
	if identity.Verified {
		if err := w.sendHeader(); err != nil {
			return err
		}
	}
	log.Info("adapter SafetyStream established")

	done := make(chan struct{})
	defer close(done)
	msgs, recvErr := recvEach(stream.Recv, done)
	for {
		var msg *fav1.AdapterSafetyMessage
		select {
		case <-superseded:
			log.Info("adapter SafetyStream superseded by a newer stream under the same identity; closed")
			return supersededSafetyStatus(identity)
		case <-lost:
			log.Info("adapter SafetyStream closed: this process lost leadership")
			return leadershipLostStatus()
		case err := <-recvErr:
			if err == io.EOF {
				log.Info("adapter SafetyStream closed")
				return nil
			}
			return err
		case msg = <-msgs:
		}
		// An EstopAck, an EstopClearAck and a LocalStopReport are consumed here. Any other arm
		// (PauseResult, an empty oneof, an arm added later) reaches nothing and is dropped, so an
		// arm this loop does not name cannot confirm anything.
		switch {
		case msg.GetEstopClearAck() != nil:
			s.routeClearAck(ctx, log, identity, w, msg)
			continue
		case msg.GetLocalStop() != nil:
			if rid := msg.GetRobotId(); rid == "" {
				s.recordAuthzDenied(ctx, identity, "", "LocalStopReport with an empty robot_id")
			} else if allowed, _ := s.authorizeObserve(ctx, identity, rid); allowed {
				log.Info("local stop report dropped: the control plane does not read local-latch reports yet",
					"robot", rid, "latched", msg.GetLocalStop().GetLatched(), "setID", msg.GetLocalStop().GetSetId())
			}
			continue
		}
		ack := msg.GetEstopAck()
		if ack == nil {
			continue
		}
		// An estop acknowledgement is a safety claim about a specific robot. One that
		// names no robot cannot be authorized, and is refused rather than let past the
		// check: robot_id is REQUIRED on every arm that carries it (RFC-0001
		// #fleet-adapter-protocol-transport-security). The onboarding relaxation does not
		// apply either: an ack for a robot this adapter may not act on is dropped whether
		// the robot is unadmitted or bound elsewhere. The robot's class is not checked, and
		// neither is its FleetAdapter's existence: an ack from the robot's own adapter confirms
		// its stop after the class has left the adapter's list (ErrClassNotServed) or the
		// FleetAdapter has been deleted (ErrAdapterNotFound).
		rid := msg.GetRobotId()
		if rid == "" {
			log.Info("PERMISSION_DENIED: EstopAck with an empty robot_id", "estopID", ack.GetEstopId())
			s.recordAuthzDenied(ctx, identity, "", "EstopAck with an empty robot_id")
			continue
		}
		if allowed, _ := s.authorizeObserve(ctx, identity, rid); !allowed {
			continue // drop, keep the stream (a forged robot_id is not a fault)
		}
		// The dispatcher matches the ack against the estop it pushed down THIS stream for
		// this robot, so an authorized robot_id cannot confirm another robot's estop, and
		// another stream under the same certificate cannot confirm this one. A refusal is
		// logged and sealed like any other denial, and the stream stays up.
		if err := s.Safety.RouteAck(identity, w, rid, ack); err != nil {
			log.Info("PERMISSION_DENIED: EstopAck refused", "estopID", ack.GetEstopId(), "robot", rid,
				"reason", err.Error())
			s.recordAuthzDenied(ctx, identity, rid, err.Error())
		}
	}
}

// routeClearAck checks an EstopClearAck as an EstopAck is checked (a robot_id, authorized for
// this identity), with the robot's class checked as well: a clear begins the robot's return to
// work, which a robot outside its adapter's class list is not given. It hands the ack to the
// dispatcher, which takes it only from the adapter's current SafetyStream. A refusal is
// recorded as a denial; the stream stays up.
func (s *Server) routeClearAck(ctx context.Context, log logr.Logger, identity TLSIdentity, w *safetyWriter,
	msg *fav1.AdapterSafetyMessage) {
	ack := msg.GetEstopClearAck()
	rid := msg.GetRobotId()
	if rid == "" {
		log.Info("PERMISSION_DENIED: EstopClearAck with an empty robot_id", "estopID", ack.GetEstopId())
		s.recordAuthzDenied(ctx, identity, "", "EstopClearAck with an empty robot_id")
		return
	}
	if allowed, _ := s.authorizeRobot(ctx, identity, rid); !allowed {
		return
	}
	if err := s.Safety.RouteClearAck(identity, w, rid, ack); err != nil {
		log.Info("PERMISSION_DENIED: EstopClearAck refused", "estopID", ack.GetEstopId(), "robot", rid, "reason", err.Error())
		s.recordAuthzDenied(ctx, identity, rid, err.Error())
	}
}

// ControlStream terminates one adapter's ControlStream. It requires AdapterHello
// as the first message, replies HelloAck, then dispatches subsequent messages
// until the adapter closes the stream or a connection-level fault occurs.
func (s *Server) ControlStream(stream fav1.FleetAdapterService_ControlStreamServer) error {
	ctx := stream.Context()
	w := &streamWriter{stream: stream}

	// A process that does not lead serves no adapter stream (leadership.go). The stream is
	// refused before its handshake, so nothing is registered for it.
	lost, err := s.lead()
	if err != nil {
		return err
	}

	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		// RFC-0001 #fleet-adapter-protocol-fleet-adapter-compliance-checklist: AdapterHello MUST be the first message. A non-hello opener is a
		// connection-level protocol violation, so a gRPC status code is correct.
		return status.Error(codes.FailedPrecondition,
			"protocol violation: first ControlStream message must be AdapterHello")
	}

	// The authenticated identity for per-message authorization is the mTLS
	// client-certificate SAN (RFC-0001 #fleet-adapter-protocol-transport-security) — NOT the self-reported AdapterHello.
	// It is read before the handshake is answered because the hello's namespace is
	// checked against it.
	identity := IdentityFromContext(ctx)
	namespace, nsRefusal := bindHelloNamespace(hello.GetNamespace(), identity)

	ack := helloAck(hello)
	if ack.GetAccepted() && nsRefusal != "" {
		s.logger().Info("refusing AdapterHello: declared namespace does not match the certificate",
			"declared", hello.GetNamespace(), "certificateNamespace", identity.Namespace,
			"adapterIdentity", identity.AdapterName, "peer", peerAddr(ctx))
		ack = &fav1.HelloAck{Accepted: false, NegotiatedProtocolVersion: ProtocolVersion, Message: nsRefusal}
	}
	if err := w.send(&fav1.ControlPlaneMessage{
		Payload: &fav1.ControlPlaneMessage_HelloAck{HelloAck: ack},
	}); err != nil {
		return err
	}
	if !ack.GetAccepted() {
		// Handshake refused (an unsupported protocol version, or a namespace the
		// certificate does not bind): the adapter has been told why; close the stream
		// cleanly.
		return nil
	}

	id := AdapterIdentity{
		Vendor:          hello.GetVendor(),
		AdapterVersion:  hello.GetAdapterVersion(),
		ProtocolVersion: hello.GetProtocolVersion(),
		Namespace:       namespace,
		PeerAddr:        peerAddr(ctx),
		ContractVersion: hello.GetContractVersion(),
		// The ack settled compatibility; a negotiated contract version is present only when it
		// passed, so the verdict is read back off the ack rather than recomputed (one decision,
		// one place).
		ContractCompatible:        ack.GetNegotiatedContractVersion() != "",
		NegotiatedContractVersion: ack.GetNegotiatedContractVersion(),
	}
	log := s.logger().WithValues("vendor", id.Vendor, "namespace", id.Namespace, "peer", id.PeerAddr)

	switch {
	case s.Authorizer == nil:
		log.Info("serving ControlStream WITHOUT per-robot authorization (dev mode) — RFC-0001 #security-fleet-adapter-authentication-robot-identity requires it in production")
	case !identity.Verified:
		log.Info("adapter presented no verified mTLS identity; every per-robot message will be denied (fail closed)")
	default:
		log = log.WithValues("adapterIdentity", identity.AdapterName, "identityNamespace", identity.Namespace)
	}
	log.Info("adapter ControlStream established")

	// One adapter identity, one live stream (RFC-0001
	// #fleet-adapter-protocol-connection-lifecycle). Claimed before presence, so the
	// stream this one replaces does not report a disconnect when it closes; that stream
	// is closed below, once this one is registered for commands. An unverified stream
	// has no identity to hold, and is never registered for commands either.
	var superseded <-chan struct{}
	var replaced *session
	if identity.Verified {
		var ss *session
		ss, replaced = s.claimSession(identity, id.PeerAddr)
		superseded = ss.superseded
		// Presence: a verified adapter's stream is the connectivity signal for its
		// FleetAdapter.status.phase (RFC-0001 #fleetadapter-status-fields-and-phase-machine). The disconnect uses a fresh context —
		// the stream ctx is already cancelled once the loop exits. A superseded stream
		// does not report a disconnect: its adapter is connected on the newer stream, and
		// reporting one would move a live adapter to Disconnected.
		defer func() {
			if s.releaseSession(identity, ss) && s.Presence != nil {
				s.Presence.AdapterDisconnected(context.Background(), identity)
			}
		}()
	}
	if s.Presence != nil && identity.Verified {
		s.Presence.AdapterConnected(ctx, identity, Negotiation{
			ProtocolVersion:    ack.GetNegotiatedProtocolVersion(),
			ContractVersion:    id.ContractVersion,
			ContractCompatible: id.ContractCompatible,
		})
	}

	// Command-push: register the stream so verify_*/renew_lease/model_update
	// Commands can be pushed to this adapter and their CommandResults correlated
	// (§9.2, §E-2). Only a verified identity is registered (RegisterStream enforces
	// this) — nothing is pushed to an unauthenticated stream.
	if s.Commands != nil {
		deregister := s.Commands.RegisterStream(identity, w)
		defer deregister()
		if co, ok := s.Commands.(StreamContractObserver); ok {
			co.StreamNegotiated(w, id.NegotiatedContractVersion)
		}
	}
	if co, ok := s.Safety.(ContractObserver); ok && identity.Verified {
		co.ContractReported(identity, id.ContractVersion)
	}
	if replaced != nil {
		s.supersede(ctx, log, identity, replaced, id.PeerAddr)
	}

	done := make(chan struct{})
	defer close(done)
	msgs, recvErr := recvMessages(stream, done)
	for {
		select {
		case <-superseded:
			log.Info("adapter ControlStream superseded by a newer stream under the same identity; closed")
			return supersededStatus(identity)
		case <-lost:
			log.Info("adapter ControlStream closed: this process lost leadership")
			return leadershipLostStatus()
		case err := <-recvErr:
			if err == io.EOF {
				log.Info("adapter ControlStream closed")
				return nil
			}
			return err
		case msg := <-msgs:
			if err := s.dispatch(ctx, w, id, identity, msg); err != nil {
				return err
			}
		}
	}
}

// dispatch handles one AdapterMessage. It returns a non-nil error only for a
// connection-level fault that should tear the stream down; per-operation
// outcomes travel in-band (RFC-0001 #fleet-adapter-protocol-in-band-error-model).
func (s *Server) dispatch(ctx context.Context, w *streamWriter, id AdapterIdentity, tlsID TLSIdentity, msg *fav1.AdapterMessage) error {
	// ── Per-message robot_id authorization (RFC-0001 #security-fleet-adapter-authentication-robot-identity, #fleet-adapter-protocol-transport-security) ──────────────────
	// Every arm is classified by robotIDOf, and an arm it does not name is refused.
	// Discover is a first announce (AuthorizeAnnounce); every other robot arm names an
	// admitted robot (AuthorizeRobot), and its robot_id is checked whatever it holds —
	// an empty one included. An arm that observes the robot is not refused on its class, or
	// its FleetAdapter's absence, alone (scopeObserve). A denial is refused in-band or
	// dropped — NEVER a stream teardown, since one stray robot_id is not a connection-level
	// fault.
	rid, scope := robotIDOf(msg)
	switch scope {
	case scopeAnnounce:
		if !s.authorizeAnnounce(ctx, tlsID, rid) {
			return w.send(&fav1.ControlPlaneMessage{
				Payload: &fav1.ControlPlaneMessage_DiscoverAck{DiscoverAck: &fav1.DiscoverAck{
					Accepted: false,
					Message:  "PERMISSION_DENIED: not authorized to announce robot " + rid,
				}},
			})
		}
	case scopeAction, scopeAdapter:
		// Authorized where the action's robot is read (scopeAction), or names no robot
		// by design (scopeAdapter). robotIDOf says which, and why, per arm.
	case scopeUnclassified:
		s.logger().Info("PERMISSION_DENIED: message arm not classified for authorization",
			"type", fmt.Sprintf("%T", msg.GetPayload()), "adapter", tlsID.AdapterName, "namespace", tlsID.Namespace)
		s.recordAuthzDenied(ctx, tlsID, "", fmt.Sprintf("message arm %T is not classified for authorization", msg.GetPayload()))
		return nil
	case scopeRobot, scopeObserve:
		authorize := s.authorizeRobot
		if scope == scopeObserve {
			authorize = s.authorizeObserve
		}
		allowed, notAdmitted := authorize(ctx, tlsID, rid)
		if !allowed {
			// A Heartbeat from an adapter whose robot is merely NOT ADMITTED still proves
			// the ADAPTER is alive, and that is a different question from whether it may
			// act on the robot. The presence signal is keyed on tlsID — the mTLS identity,
			// authenticated independently of any Robot object — and the handler below says
			// so ("a Heartbeat proves the adapter is alive"). Dropping it here is what
			// deadlocked onboarding: an adapter with no admitted robot could not prove
			// liveness, decayed Connected → Degraded → Disconnected, and vrobot then refused
			// to create the very Robot whose absence caused the decay.
			//
			// Only the ADAPTER half runs. The per-robot confirming exchange is skipped, so
			// no robot state is touched by an unauthorized message. A genuine authorization
			// violation (robot bound elsewhere, unverified identity, class not served) is
			// NOT notAdmitted and still drops everything.
			//
			// RA-1: this writes no telemetry-derived status. It is the designed adapter
			// presence path (RFC-0001 #fleetadapter-status-fields-and-phase-machine), explicitly distinguished from the telemetry tick, and
			// its status write is change-gated in AdapterHeartbeat.
			if notAdmitted && msg.GetHeartbeat() != nil {
				if s.Presence != nil && tlsID.Verified {
					s.Presence.AdapterHeartbeat(ctx, tlsID)
				}
				return nil
			}
			return s.refuse(w, msg, rid, notAdmitted)
		}
	}

	switch p := msg.GetPayload().(type) {
	case *fav1.AdapterMessage_Hello:
		// A second hello on an established stream is a protocol violation.
		return status.Error(codes.FailedPrecondition, "protocol violation: duplicate AdapterHello")
	case *fav1.AdapterMessage_Register:
		// ADR-0032 compatibility gate: an adapter whose contract version is out of range (or
		// missing/unparseable — fail closed) may not bring a robot under management, so the
		// registration is refused VERSION_MISMATCH before it reaches the Registrar. Nothing about
		// the robot is written. Telemetry, heartbeat and estop are unaffected; see helloAck.
		if !id.ContractCompatible {
			return w.send(&fav1.ControlPlaneMessage{
				Payload: &fav1.ControlPlaneMessage_RegisterAck{RegisterAck: &fav1.RegisterAck{
					Accepted:  false,
					Rejection: fav1.RegistrationRejection_REGISTRATION_REJECTION_VERSION_MISMATCH,
					Message:   versionMismatchMessage(id.ContractVersion),
				}},
			})
		}
		ack := s.registrar().Register(ctx, id, p.Register)
		if err := w.send(&fav1.ControlPlaneMessage{
			Payload: &fav1.ControlPlaneMessage_RegisterAck{RegisterAck: ack},
		}); err != nil {
			return err
		}
		// Processed: the fences of the stops in force go down this stream before any work.
		if n, ok := s.Commands.(RegisterNotifier); ok && ack.GetAccepted() && s.stillLeading() {
			n.RobotRegistered(ctx, w, id.Namespace, p.Register.GetRobotId())
		}
		return nil
	case *fav1.AdapterMessage_Discover:
		// Same gate on the discovery path: an incompatible adapter must not even create a
		// DiscoveredRobot, or an operator could admit a robot it can never be driven through.
		if !id.ContractCompatible {
			return w.send(&fav1.ControlPlaneMessage{
				Payload: &fav1.ControlPlaneMessage_DiscoverAck{DiscoverAck: &fav1.DiscoverAck{
					Accepted:  false,
					Rejection: fav1.RegistrationRejection_REGISTRATION_REJECTION_VERSION_MISMATCH,
					Message:   versionMismatchMessage(id.ContractVersion),
				}},
			})
		}
		ack := s.registrar().Discover(ctx, id, tlsID, p.Discover)
		return w.send(&fav1.ControlPlaneMessage{
			Payload: &fav1.ControlPlaneMessage_DiscoverAck{DiscoverAck: ack},
		})
	case *fav1.AdapterMessage_Heartbeat:
		// Adapter liveness — the presence signal for FleetAdapter.status. This is
		// deliberately NOT the telemetry path (RA-1): a Heartbeat proves the adapter
		// is alive; a TelemetryPayload is a per-robot data tick and never touches
		// adapter status.
		//
		// An EMPTY robot_id is the adapter-scoped reply to the periodic probe the
		// control plane pushes per connected adapter (ADR-0054). robotIDOf classifies it
		// scopeAdapter by name, so the gate above does not check it, and that is
		// correct rather than an omission: it names no robot, so there is no
		// per-robot authority to check, and the identity being asserted is the mTLS
		// one this handler already gates on (tlsID.Verified). It touches no robot
		// state — RouteHeartbeat below returns immediately on an empty robot_id, so
		// an adapter cannot use the adapter-scoped form to answer a per-robot
		// confirming exchange it was never asked.
		if s.Presence != nil && tlsID.Verified {
			s.Presence.AdapterHeartbeat(ctx, tlsID)
		}
		// Also answer a pending per-ROBOT confirming exchange (RFC-0001 #safety-control-plane-response). Adapter
		// presence and robot liveness are different questions: one adapter multiplexes
		// many robots, so an adapter being alive says nothing about whether a
		// particular robot's telemetry stall is real.
		if router, ok := s.Commands.(HeartbeatRouter); ok && tlsID.Verified {
			router.RouteHeartbeat(tlsID.Namespace, p.Heartbeat.GetRobotId())
		}
		return nil
	case *fav1.AdapterMessage_Telemetry:
		s.ingest(ctx, id, tlsID, p.Telemetry)
		return nil
	case *fav1.AdapterMessage_CommandResult:
		// The result of a pushed Command (verify_*, renew_lease, …). The robot_id
		// was already authorized above (robotIDOf covers CommandResult), so an
		// unauthorized result never reaches here. The dispatcher correlates it by
		// command_id, and accepts it only from the stream the command went down, for
		// the robot it was sent for: a guessed id on another stream answers nothing. A
		// refusal is logged and sealed like any other denial, and the stream stays up.
		if s.Commands != nil {
			if err := s.Commands.RouteResult(w, p.CommandResult); err != nil {
				s.logger().Info("PERMISSION_DENIED: CommandResult refused",
					"commandID", p.CommandResult.GetCommandId(), "robot", rid,
					"adapter", tlsID.AdapterName, "namespace", tlsID.Namespace, "reason", err.Error())
				s.recordAuthzDenied(ctx, tlsID, rid, err.Error())
			}
		}
		return nil
	case *fav1.AdapterMessage_ResourceRequest:
		// Adapter-initiated shared-resource reservation (RFC-0001 #tde-shared-resource-queue-management). The robot_id was
		// already authorized above (robotIDOf covers ResourceRequest).
		return s.handleResourceRequest(ctx, w, id, p.ResourceRequest)
	case nil:
		// Empty oneof: ignore rather than fault the stream.
		return nil
	case *fav1.AdapterMessage_ActionStatus:
		// The robot's reported action state (§9.2.3). Fencing and lifecycle rules
		// live in the ingestor; a nil ingestor drops the update. The report names an
		// action, and ending an action frees its robot, so the ingestor authorizes this
		// stream for the robot the action is bound to before the report may move it.
		// A retained outcome is read on its own path and may be acknowledged (retained.go).
		if s.ActionStatus != nil && p.ActionStatus.GetRetained() {
			return s.ingestRetained(ctx, w, id, tlsID, p.ActionStatus)
		}
		if s.ActionStatus != nil {
			authorize := func(ctx context.Context, robotID string) bool {
				allowed, _ := s.authorizeRobot(ctx, tlsID, robotID)
				return allowed
			}
			if err := s.ActionStatus.IngestActionStatus(ctx, id.Namespace, authorize, p.ActionStatus); err != nil {
				s.logger().Error(err, "ingesting task status",
					"namespace", id.Namespace, "actionID", p.ActionStatus.GetActionId())
			}
		}
		return nil
	case *fav1.AdapterMessage_UpdateProgress:
		// Advisory per-robot rollout progress (RFC-0001 #firmwarerollout-schema and #modelrollout-schema): surfaced as the active
		// rollout's currentBatch updatePhase. A nil ingestor drops it.
		if s.UpdateProgress != nil {
			if err := s.UpdateProgress.IngestUpdateProgress(ctx, id.Namespace, p.UpdateProgress); err != nil {
				s.logger().Error(err, "ingesting update progress",
					"namespace", id.Namespace, "robotID", p.UpdateProgress.GetRobotId())
			}
		}
		return nil
	case *fav1.AdapterMessage_Capabilities:
		// Adapter capability/action-catalog snapshot (§9.2). robot_id was authorized
		// above (robotIDOf covers Capabilities). A nil ingestor drops it.
		if s.Capabilities != nil {
			if err := s.Capabilities.IngestCapabilities(ctx, id.Namespace, p.Capabilities); err != nil {
				s.logger().Error(err, "ingesting capabilities snapshot",
					"namespace", id.Namespace, "robotID", p.Capabilities.GetRobotId())
			}
		}
		return nil
	default:
		s.logger().V(1).Info("ignoring ControlStream message arm not handled in this stage",
			"type", fmt.Sprintf("%T", p), "namespace", id.Namespace)
		return nil
	}
}

// handleResourceRequest services an adapter-initiated shared-resource reservation
// (RFC-0001 #tde-shared-resource-queue-management): reserve/release via the TDE, then reply with a ResourceResponse
// correlated by request_id. FAIL-CLOSED: no reserver or an empty request oneof
// denies rather than granting. The robot_id was authorized upstream (robotIDOf).
func (s *Server) handleResourceRequest(ctx context.Context, w *streamWriter, id AdapterIdentity, rr *fav1.ResourceRequest) error {
	robotID := rr.GetRobotId()
	resp := &fav1.ResourceResponse{RequestId: rr.GetRequestId(), RobotId: robotID}
	switch req := rr.GetRequest().(type) {
	case *fav1.ResourceRequest_Reserve:
		dec := ResourceDecision{State: ResourceDenied, Message: "reservations unavailable"}
		if s.Reservations != nil {
			dec = s.Reservations.ReserveResource(ctx, id.Namespace, req.Reserve.GetResourceName(), robotID)
		}
		resp.Response = &fav1.ResourceResponse_Reserve{Reserve: &fav1.ReserveResult{
			//nolint:gosec // queue position is a small non-negative rank.
			State: reserveState(dec.State), QueuePosition: int32(dec.QueuePosition), Message: dec.Message,
		}}
	case *fav1.ResourceRequest_Release:
		dec := ResourceDecision{Message: "reservations unavailable"}
		if s.Reservations != nil {
			dec = s.Reservations.ReleaseResource(ctx, id.Namespace, req.Release.GetResourceName(), robotID)
		}
		// A release may promote a queued waiter to holder; push it the async grant.
		if dec.PromotedRobotID != "" && s.GrantNotifier != nil {
			s.GrantNotifier.NotifyReservationGranted(ctx, id.Namespace, dec.PromotedRobotID, req.Release.GetResourceName())
		}
		resp.Response = &fav1.ResourceResponse_Release{Release: &fav1.ReleaseResult{
			Released: dec.Released, Message: dec.Message,
		}}
	default:
		// Empty request oneof: deny (fail closed) rather than fault the stream.
		resp.Response = &fav1.ResourceResponse_Reserve{Reserve: &fav1.ReserveResult{
			State: fav1.ReservationState_RESERVATION_STATE_DENIED, Message: "empty resource request",
		}}
	}
	return w.send(&fav1.ControlPlaneMessage{
		Payload: &fav1.ControlPlaneMessage_ResourceResponse{ResourceResponse: resp},
	})
}

// reserveState maps a ResourceReserveState onto the wire ReservationState.
func reserveState(s ResourceReserveState) fav1.ReservationState {
	switch s {
	case ResourceGranted:
		return fav1.ReservationState_RESERVATION_STATE_GRANTED
	case ResourceQueued:
		return fav1.ReservationState_RESERVATION_STATE_QUEUED
	default:
		return fav1.ReservationState_RESERVATION_STATE_DENIED
	}
}

// authorizeRobot authorizes a robot-scoped message. A nil Authorizer means
// per-robot authz is disabled (dev mode, warned at connect); when set, any error
// denies (fail closed) and logs a PERMISSION_DENIED security line.
// authorizeRobot reports (allowed, notAdmitted). notAdmitted is true only for the
// narrow case of a robot with no Robot object yet — see ErrRobotNotAdmitted. Every
// other denial reports notAdmitted=false and stays opaque to the adapter.
func (s *Server) authorizeRobot(ctx context.Context, tlsID TLSIdentity, robotID string) (allowed, notAdmitted bool) {
	if s.Authorizer == nil {
		return true, false
	}
	return s.authzVerdict(ctx, tlsID, robotID, s.Authorizer.AuthorizeRobot(ctx, tlsID, robotID))
}

// authorizeObserve authorizes a message that observes or stops a robot: telemetry, a
// heartbeat naming the robot, an EstopAck, a LocalStopReport. It is authorizeRobot, except
// that a denial on the class alone (ErrClassNotServed), or on the FleetAdapter's absence alone
// (ErrAdapterNotFound), allows: the adapter's identity, the robot and its binding to this
// adapter are checked, and fail closed, as before. The admission gate MUST NOT block observation
// or stopping (RFC-0001 #fleetadapter-admission-gating), and both are among its conditions.
func (s *Server) authorizeObserve(ctx context.Context, tlsID TLSIdentity, robotID string) (allowed, notAdmitted bool) {
	if s.Authorizer == nil {
		return true, false
	}
	err := s.Authorizer.AuthorizeRobot(ctx, tlsID, robotID)
	switch {
	case errors.Is(err, ErrClassNotServed):
		s.logger().V(1).Info("robot's class is not one its adapter serves; observation and stopping stay open",
			"robot", robotID, "adapter", tlsID.AdapterName, "namespace", tlsID.Namespace)
		return true, false
	case errors.Is(err, ErrAdapterNotFound):
		s.logger().V(1).Info("robot's FleetAdapter does not exist; observation and stopping stay open",
			"robot", robotID, "adapter", tlsID.AdapterName, "namespace", tlsID.Namespace)
		return true, false
	}
	return s.authzVerdict(ctx, tlsID, robotID, err)
}

// authzVerdict turns an Authorizer's answer into authorizeRobot's (allowed, notAdmitted).
func (s *Server) authzVerdict(ctx context.Context, tlsID TLSIdentity, robotID string, err error) (allowed, notAdmitted bool) {
	if err == nil {
		return true, false
	}
	notAdmitted = errors.Is(err, ErrRobotNotAdmitted)
	// An unadmitted robot is an expected step in onboarding, not a security event: it is
	// logged at V(1) and not sealed as ROBOT_AUTHZ_DENIED, so the audit chain keeps
	// meaning "someone was refused" rather than filling with the provisioning flow. A
	// real denial is logged and sealed exactly as before.
	if notAdmitted {
		s.logger().V(1).Info("robot not admitted; adapter routed to discovery",
			"robot", robotID, "adapter", tlsID.AdapterName, "namespace", tlsID.Namespace)
		return false, true
	}
	s.logger().Info("PERMISSION_DENIED: adapter not authorized for robot",
		"robot", robotID, "adapter", tlsID.AdapterName, "namespace", tlsID.Namespace, "reason", err.Error())
	s.recordAuthzDenied(ctx, tlsID, robotID, err.Error())
	return false, false
}

// authorizeAnnounce authorizes a first-time Discover.
func (s *Server) authorizeAnnounce(ctx context.Context, tlsID TLSIdentity, robotID string) bool {
	if s.Authorizer == nil {
		return true
	}
	if err := s.Authorizer.AuthorizeAnnounce(ctx, tlsID, robotID); err != nil {
		s.logger().Info("PERMISSION_DENIED: adapter not authorized to announce robot",
			"robot", robotID, "adapter", tlsID.AdapterName, "namespace", tlsID.Namespace, "reason", err.Error())
		s.recordAuthzDenied(ctx, tlsID, robotID, err.Error())
		return false
	}
	return true
}

// recordAuthzDenied writes a ROBOT_AUTHZ_DENIED entry to the safety audit log
// (RFC-0001 #security-audit-logging). A denied action is always recorded — never silently dropped. The
// actor is the adapter's mTLS identity; the namespace is the authenticated one.
func (s *Server) recordAuthzDenied(ctx context.Context, tlsID TLSIdentity, robotID, reason string) {
	if s.Audit == nil {
		return
	}
	if _, err := s.Audit.Record(audit.Entry{
		Namespace: tlsID.Namespace,
		EventType: audit.EventRobotAuthzDenied,
		Action:    "stream-message",
		Outcome:   audit.OutcomeDenied,
		Actor: audit.Actor{
			Type:     audit.ActorRobot,
			Identity: tlsID.AdapterName + "." + tlsID.Namespace,
			SourceIP: peerAddr(ctx),
		},
		Resource: audit.Resource{Kind: "Robot", Namespace: tlsID.Namespace, Name: robotID},
		Detail:   map[string]string{"reason": reason},
	}); err != nil {
		s.logger().Error(err, "recording ROBOT_AUTHZ_DENIED audit entry")
	}
}

// refuse handles an authorization denial in-band: a Register gets an explicit
// accepted:false ack; every other robot-scoped arm is dropped. The stream is
// never torn down (RFC-0001 #security-fleet-adapter-authentication-robot-identity — one stray robot_id is not a connection-level fault).
func (s *Server) refuse(w *streamWriter, msg *fav1.AdapterMessage, robotID string, notAdmitted bool) error {
	if msg.GetRegister() == nil {
		return nil
	}
	// An empty robot_id is refused with the code the contract names for it, as the
	// registrar would have: the gate reaching it first changes who refuses, not the answer.
	if robotID == "" {
		return w.send(&fav1.ControlPlaneMessage{
			Payload: &fav1.ControlPlaneMessage_RegisterAck{RegisterAck: &fav1.RegisterAck{
				Accepted:  false,
				Rejection: fav1.RegistrationRejection_REGISTRATION_REJECTION_INVALID_ROBOT_ID,
				Message:   "robot_id is empty",
			}},
		})
	}
	// A robot with no Robot object gets the code the contract declares for exactly this
	// case — RegistrationRejection NOT_ADMITTED, "Register: no Robot CRD exists" — because
	// adapters/CONFORMANCE.md C2.1 specifies registering FIRST and falling through to
	// DiscoverRobot only on that rejection. Leaving it UNSPECIFIED gave a conformant
	// adapter no basis to fall through, so it never announced and the robot could never be
	// admitted.
	if notAdmitted {
		return w.send(&fav1.ControlPlaneMessage{
			Payload: &fav1.ControlPlaneMessage_RegisterAck{RegisterAck: &fav1.RegisterAck{
				Accepted:  false,
				Rejection: fav1.RegistrationRejection_REGISTRATION_REJECTION_NOT_ADMITTED,
				Message: "robot " + robotID + " is not admitted; send DiscoverRobot to announce it " +
					"for operator admission (CONFORMANCE C2.1)",
			}},
		})
	}
	return w.send(&fav1.ControlPlaneMessage{
		Payload: &fav1.ControlPlaneMessage_RegisterAck{RegisterAck: &fav1.RegisterAck{
			Accepted: false,
			Message:  "PERMISSION_DENIED: not authorized for robot " + robotID,
		}},
	})
}

// armScope is how the per-message gate in dispatch authorizes one AdapterMessage arm.
type armScope int

const (
	// scopeUnclassified is what robotIDOf returns for an arm it does not name, and the
	// gate refuses it. A new oneof arm is therefore denied until it is classified here,
	// rather than passed unchecked because nothing looked for a robot in it.
	scopeUnclassified armScope = iota
	// scopeRobot: the arm carries robot_id and is authorized with authorizeRobot on it,
	// whatever it holds. An empty robot_id is refused by the Authorizer like any other
	// value it cannot bind, never skipped.
	scopeRobot
	// scopeObserve: the arm observes the robot it names (telemetry, a heartbeat naming a
	// robot). It is authorized as scopeRobot is, with authorizeObserve: the robot's class
	// alone does not refuse it (ErrClassNotServed), and neither does its FleetAdapter's
	// absence alone (ErrAdapterNotFound).
	scopeObserve
	// scopeAnnounce: a first announce of a robot, authorized with authorizeAnnounce.
	scopeAnnounce
	// scopeAction: the arm names an action. The robot it acts on is the one the action
	// is bound to, which only the ingestor reads, so the ingestor runs authorizeRobot on
	// it (see ActionStatusIngestor).
	scopeAction
	// scopeAdapter: the arm names no robot by design, and each such case says why.
	scopeAdapter
)

// robotIDOf classifies an AdapterMessage for the per-message gate and returns the
// robot_id it carries, if it carries one. Every arm of the oneof is named here. An arm
// that is not falls to scopeUnclassified, which the gate refuses: the default denies.
func robotIDOf(msg *fav1.AdapterMessage) (string, armScope) {
	switch p := msg.GetPayload().(type) {
	case *fav1.AdapterMessage_Telemetry:
		return p.Telemetry.GetRobotId(), scopeObserve
	case *fav1.AdapterMessage_Register:
		return p.Register.GetRobotId(), scopeRobot
	case *fav1.AdapterMessage_CommandResult:
		return p.CommandResult.GetRobotId(), scopeRobot
	case *fav1.AdapterMessage_Capabilities:
		return p.Capabilities.GetRobotId(), scopeRobot
	case *fav1.AdapterMessage_ResourceRequest:
		return p.ResourceRequest.GetRobotId(), scopeRobot
	case *fav1.AdapterMessage_UpdateProgress:
		return p.UpdateProgress.GetRobotId(), scopeRobot
	case *fav1.AdapterMessage_Heartbeat:
		// A Heartbeat naming a robot answers a per-robot confirming exchange and is
		// authorized for that robot. One with an empty robot_id is the adapter-scoped
		// reply to the periodic probe (ADR-0054): it proves the adapter, touches no robot
		// state, and its handler gates on the verified mTLS identity itself.
		if rid := p.Heartbeat.GetRobotId(); rid != "" {
			return rid, scopeObserve
		}
		return "", scopeAdapter
	case *fav1.AdapterMessage_Discover:
		return p.Discover.GetRobotId(), scopeAnnounce
	case *fav1.AdapterMessage_ActionStatus:
		return "", scopeAction
	case *fav1.AdapterMessage_Hello:
		// The handshake names no robot. Only a duplicate reaches dispatch, and its
		// handler faults the stream as a protocol violation.
		return "", scopeAdapter
	case nil:
		// An empty oneof carries nothing, and its handler does nothing.
		return "", scopeAdapter
	}
	return "", scopeUnclassified
}

// ingest translates one TelemetryPayload and forwards it to the Ingestor. A
// missing robot_id or ingestion error is logged, never surfaced as a
// connection-level fault: a bad or unstorable frame must not tear down a stream
// that multiplexes every robot the adapter manages.
func (s *Server) ingest(ctx context.Context, id AdapterIdentity, tlsID TLSIdentity, p *fav1.TelemetryPayload) {
	// Count every TelemetryPayload received on ControlStream (RFC-0001 #control-plane-observability-prometheus-metrics-contract), the
	// write-amplification denominator for swarmada_telemetry_status_writes_total.
	// The adapter label is the mTLS-verified FleetAdapter identity (empty in dev
	// mode without a verified certificate).
	metrics.IncTelemetryFramesReceived(tlsID.Namespace, tlsID.AdapterName)
	if p.GetRobotId() == "" {
		s.logger().V(1).Info("dropping telemetry with empty robot_id", "namespace", id.Namespace)
		return
	}
	if s.Ingestor == nil {
		// No Ingestor configured (a Server built without one): drop the frame.
		// Nothing is written to Robot.status on a telemetry tick (RA-1).
		return
	}
	frame := TelemetryFrame(p)
	// The same namespace the three sibling ingestors are given (dispatch, above): a
	// robot_id is unique per namespace only, so without it a Robot cannot be resolved.
	frame.Namespace = id.Namespace
	frame.Adapter = tlsID.AdapterName // for the dropped-frames metric label (RFC-0001 #control-plane-observability-prometheus-metrics-contract)
	if err := s.Ingestor.Ingest(ctx, frame); err != nil {
		s.logger().Error(err, "telemetry ingest failed", "robot", frame.RobotID)
	}
}

func (s *Server) registrar() Registrar {
	if s.Registrar != nil {
		return s.Registrar
	}
	return rejectAllRegistrar{}
}

func (s *Server) logger() logr.Logger {
	if s.Log.GetSink() == nil {
		return logr.Discard()
	}
	return s.Log
}

// bindHelloNamespace settles the namespace a ControlStream is served in, from the
// namespace the AdapterHello declares and the verified mTLS identity. Everything the
// stream does afterwards — telemetry, action status, update progress, capability
// reports, reservations — is resolved in that namespace, so it must be the one the
// certificate binds, not whatever the adapter chose to write in its hello.
//
// With a verified certificate the certificate's namespace is authoritative:
//   - a hello that declares no namespace is served in the certificate's;
//   - a hello that declares the certificate's namespace is served in it;
//   - a hello that declares a different namespace is refused, and refusal is
//     non-empty and names both. It is not corrected silently: an adapter configured
//     for the wrong namespace is misconfigured, and serving it anyway would hide that.
//
// Without a verified certificate (identity.Namespace is empty: the dev-mode
// posture with no client CA configured, or a certificate whose SAN does not parse)
// nothing is available to check the declaration against, and none of this is
// enforced. The declared namespace is used as declared, and may be empty; the
// telemetry resolvers keep a defined behaviour for an empty namespace
// (findTelemetryRobot). This does not widen what such a stream may do: with an
// Authorizer configured, an unverified identity is denied every per-robot message,
// and it is only with authorization disabled that a hello's namespace goes
// unchecked.
func bindHelloNamespace(declared string, identity TLSIdentity) (namespace, refusal string) {
	bound := identity.Namespace
	switch {
	case bound == "":
		return declared, ""
	case declared == "" || declared == bound:
		return bound, ""
	default:
		return "", fmt.Sprintf("namespace %q in AdapterHello does not match namespace %q bound by the "+
			"adapter's client certificate; declare %q or omit the namespace", declared, bound, bound)
	}
}

// helloAck negotiates the handshake. An empty or matching protocol_version is
// accepted; any other value is refused with an explanatory message.
func helloAck(h *fav1.AdapterHello) *fav1.HelloAck {
	req := h.GetProtocolVersion()
	if req == "" || req == ProtocolVersion {
		// Contract-version compatibility (ADR-0032). The range is the COMPILED-IN one
		// (contract.SupportedRange): SwarmadaConfig.status.supportedContractRange is populated FROM
		// that constant, so it can only ever be stale or absent relative to the manager actually
		// serving this handshake — and reading it here would make a namespace with no
		// SwarmadaConfig reject every adapter.
		//
		// An INCOMPATIBLE adapter is still accepted onto the ControlStream, deliberately: it may
		// stream telemetry, heartbeat, and (on the independent SafetyStream) always receive estop.
		// What it may not do is register a robot — every Discover/Register on this stream is
		// refused REGISTRATION_REJECTION_VERSION_MISMATCH, so no robot is ever admitted through it
		// and therefore no work can be dispatched to it. The reason rides HelloAck.message so the
		// adapter learns at connect rather than at its first registration attempt.
		_, reason := contract.Supports(h.GetContractVersion())
		ack := &fav1.HelloAck{
			Accepted:                  true,
			NegotiatedProtocolVersion: ProtocolVersion,
			Message:                   "welcome",
		}
		if agreed, compatible := contract.Negotiate(h.GetContractVersion()); compatible {
			// Only a negotiated connection carries a contract version: this is the version the
			// control plane will hold the adapter to, agreed per connection (RFC-0001
			// #fleet-adapter-protocol-package-and-versioning). A 1.3.x adapter is held to 1.3.x:
			// it is sent every stop, and no EstopClear and no ActionOutcomeAck.
			ack.NegotiatedContractVersion = agreed
		} else {
			// Left EMPTY on purpose — there is no agreed version to report.
			ack.Message = "connected, but robot registration is refused: " + reason
		}
		return ack
	}
	return &fav1.HelloAck{
		Accepted:                  false,
		NegotiatedProtocolVersion: ProtocolVersion,
		Message: fmt.Sprintf("unsupported protocol version %q; this control plane speaks %q",
			req, ProtocolVersion),
	}
}

// versionMismatchMessage explains a VERSION_MISMATCH refusal on the wire. It restates the reason
// the handshake already reported, because an adapter may retry registration long after connecting
// and the operator reading the ack should not have to correlate it with a connect-time message.
func versionMismatchMessage(reported string) string {
	_, reason := contract.Supports(reported)
	return "VERSION_MISMATCH: " + reason + " — this control plane accepts " + contract.SupportedRange() +
		". Registration is refused; telemetry, heartbeat and emergency stop are unaffected."
}

func peerAddr(ctx context.Context) string {
	if pr, ok := peer.FromContext(ctx); ok && pr.Addr != nil {
		return pr.Addr.String()
	}
	return ""
}

// streamWriter serialises sends on one ControlStream. A gRPC stream's Send is not
// safe for concurrent use; guarding it now keeps the stream correct once later
// stages add server-initiated pushes (commands, heartbeat requests) from other
// goroutines alongside the receive loop's ack replies. It also owns the outbound
// per-stream seq counter (§9.2.3 — monotonic; audit/gap-detection only).
type streamWriter struct {
	mu     sync.Mutex
	seq    uint64
	stream fav1.FleetAdapterService_ControlStreamServer
}

func (w *streamWriter) send(msg *fav1.ControlPlaneMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	msg.Seq = w.seq
	return w.stream.Send(msg)
}

// Send satisfies CommandSender so the command dispatcher can push server→adapter
// Commands down this stream from a goroutine other than the receive loop; the
// mutex in send keeps the gRPC Send safe and stamps the outbound seq.
func (w *streamWriter) Send(msg *fav1.ControlPlaneMessage) error {
	return w.send(msg)
}

// safetyWriter serialises server→adapter sends on one SafetyStream and owns its
// monotonic seq (§9.2 — audit/gap-detection). Multiple TriggerEstop calls for
// different robots on the same adapter may send concurrently; the mutex keeps the
// gRPC Send safe.
type safetyWriter struct {
	mu     sync.Mutex
	seq    uint64
	headed bool // the stream's headers are sent, on their own or with a message
	stream fav1.FleetAdapterService_SafetyStreamServer
}

// Send serialises one server→adapter SafetyStream message, stamping the seq.
func (w *safetyWriter) Send(msg *fav1.ControlPlaneSafetyMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	msg.Seq = w.seq
	err := w.stream.Send(msg)
	if err == nil {
		w.headed = true
	}
	return err
}

// sendHeader sends the stream's headers unless a message has already carried them. gRPC
// refuses a second header write, and an estop pushed between the stream's registration
// and this call is not a reason to end the stream.
func (w *safetyWriter) sendHeader() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.headed {
		return nil
	}
	if err := w.stream.SendHeader(metadata.MD{}); err != nil {
		return err
	}
	w.headed = true
	return nil
}

// rejectAllRegistrar is the safe default when no Registrar is configured: it
// refuses every registration in-band so an unconfigured server never panics and
// never silently accepts a robot.
type rejectAllRegistrar struct{}

// Register refuses every registration in-band.
func (rejectAllRegistrar) Register(context.Context, AdapterIdentity, *fav1.RegisterRobot) *fav1.RegisterAck {
	return &fav1.RegisterAck{Accepted: false, Message: "control plane registration handler not configured"}
}

// Discover refuses every announce in-band.
func (rejectAllRegistrar) Discover(context.Context, AdapterIdentity, TLSIdentity, *fav1.DiscoverRobot) *fav1.DiscoverAck {
	return &fav1.DiscoverAck{Accepted: false, Message: "control plane discovery handler not configured"}
}
