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
	"fmt"
	"io"
	"sync"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
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
// It is NOT a security boundary. RFC-0001 §9.2.7 binds an adapter's identity to
// its mTLS client-certificate SAN and authorizes every message's robot_id
// against it; that enforcement is backlog §F. Until then these self-reported
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
// managed robot (RFC-0001 §9.2.5, §9.3.1). Implementations create or look up the
// backing DiscoveredRobot/Robot resources. Per the in-band error model (§9.2.4),
// a registration outcome — including a control-plane-side failure — is reported
// as an ack with accepted=false and a message, never as a gRPC status code
// (those are reserved for connection-level faults). Implementations MUST be safe
// for concurrent use: one Registrar serves every adapter stream.
type Registrar interface {
	// Register handles a reconnect/registration of an already-admitted robot and
	// returns the RegisterAck to send back.
	Register(ctx context.Context, id AdapterIdentity, msg *fav1.RegisterRobot) *fav1.RegisterAck
	// Discover handles a first-time announce of an unadmitted robot and returns
	// the DiscoverAck to send back. tlsID is the VERIFIED mTLS identity (§9.2.7):
	// the registrar derives the auto-admit suggested class from the adapter's own
	// FleetAdapter, keyed by tlsID.AdapterName — never from the self-reported
	// AdapterIdentity (ADR-0027).
	Discover(ctx context.Context, id AdapterIdentity, tlsID TLSIdentity, msg *fav1.DiscoverRobot) *fav1.DiscoverAck
}

// Authorizer decides whether the mTLS-identified adapter may act on a robot
// (RFC-0001 §9.5.1.2, §9.2.7). It MUST fail closed: an unverified identity, an
// unknown robot, an adapter that does not own or serve the robot, or any lookup
// error all return a non-nil error, and the message is refused. Implementations
// MUST be safe for concurrent use across adapter streams.
type Authorizer interface {
	// AuthorizeRobot authorizes a message acting on an already-admitted robot.
	AuthorizeRobot(ctx context.Context, adapter TLSIdentity, robotID string) error
	// AuthorizeAnnounce authorizes a first-time Discover of a robot: the id must
	// not already be bound to a different adapter in the namespace.
	AuthorizeAnnounce(ctx context.Context, adapter TLSIdentity, robotID string) error
}

// AdapterPresence receives Fleet Adapter connectivity events so the FleetAdapter
// status controller can drive status.phase (RFC-0001 §9.1.12). It is called ONLY
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
	// AdapterDisconnected reports the adapter's stream closing or faulting.
	AdapterDisconnected(ctx context.Context, identity TLSIdentity)
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
// per-robot heartbeat exchange (RFC-0001 §9.6.3.2). Kept separate from CommandDispatcher,
// and reached by type assertion, so an implementation that does not need the confirming
// exchange — the tests' stub dispatchers, and any embedder — keeps compiling unchanged.
type HeartbeatRouter interface {
	RouteHeartbeat(namespace, robotID string)
}

// CommandDispatcher owns server→adapter command-push over ControlStream (RFC-0001
// §9.2, backlog §E-2). The Server registers each live ControlStream with it and
// routes inbound CommandResults to it, correlated by command_id; the dispatcher
// pushes Commands (verify_*, renew_lease, model_update, …) and awaits their
// result. Implementations MUST be safe for concurrent use across adapter streams.
// [github.com/swarmada/swarmada/internal/command.Dispatcher] satisfies it.
type CommandDispatcher interface {
	// RegisterStream records a live ControlStream and returns a deregister func.
	RegisterStream(identity TLSIdentity, sender CommandSender) func()
	// RouteResult delivers an inbound CommandResult for correlation.
	RouteResult(result *fav1.CommandResult)
}

// SafetyDispatcher owns estop delivery over SafetyStream (RFC-0001 §9.6.2). The
// Server registers each live stream with it and routes inbound EstopAcks to it;
// the dispatcher pushes estops and tracks their CONFIRMED outcome. Implementations
// MUST be safe for concurrent use across adapter streams.
// [github.com/swarmada/swarmada/internal/safety.Dispatcher] satisfies it.
type SafetyDispatcher interface {
	// RegisterStream records a live SafetyStream and returns a deregister func.
	RegisterStream(identity TLSIdentity, sender SafetySender) func()
	// RouteAck delivers an inbound EstopAck for correlation.
	RouteAck(ack *fav1.EstopAck)
}

// TelemetryIngestor receives one translated frame per TelemetryPayload.
// [github.com/swarmada/swarmada/internal/telemetry.Ingestor] satisfies it
// directly. Implementations MUST be safe for concurrent use across adapter
// streams.
type TelemetryIngestor interface {
	// Ingest handles one telemetry frame (two-data-plane split, §9.3.7).
	Ingest(ctx context.Context, f telemetry.Frame) error
}

// ActionStatusIngestor consumes an adapter ActionStatusUpdate (§9.2.3), advancing the
// FleetAction lifecycle from the robot's reported state. Implementations MUST be
// safe for concurrent use and MUST respect the fencing token (single-executor).
type ActionStatusIngestor interface {
	IngestActionStatus(ctx context.Context, namespace string, u *fav1.ActionStatusUpdate) error
}

// UpdateProgressIngestor consumes an adapter UpdateProgress (§6.6/§6.7), surfacing
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
// shared-resource reservations (§5.4.5). The engine implements it (adapted). It
// MUST be safe for concurrent use and fail closed (Denied) on any uncertainty.
type ResourceReserver interface {
	ReserveResource(ctx context.Context, namespace, resourceName, robotID string) ResourceDecision
	ReleaseResource(ctx context.Context, namespace, resourceName, robotID string) ResourceDecision
}

// ResourceGrantNotifier pushes an async reservation_granted Command to a robot
// whose queued shared-resource reservation was promoted to holder (§5.4.5). The
// command Dispatcher satisfies it. Best-effort: a failed push leaves the robot
// waiting at the boundary (fail-safe — it never proceeds un-granted).
type ResourceGrantNotifier interface {
	NotifyReservationGranted(ctx context.Context, namespace, robotID, resourceName string)
}

// Server is the control-plane ControlStream endpoint (RFC-0001 §9.2). Its zero
// value is usable: an unset Registrar safely rejects all registrations and an
// unset Ingestor drops telemetry (no Robot.status write ever happens on a
// telemetry tick — RA-1). It implements fav1.FleetAdapterServiceServer;
// SafetyStream remains Unimplemented until backlog §F.
type Server struct {
	fav1.UnimplementedFleetAdapterServiceServer

	// Registrar answers Discover/Register. Nil rejects all registrations.
	Registrar Registrar
	// Ingestor receives translated telemetry frames. Nil drops telemetry.
	Ingestor TelemetryIngestor
	// ActionStatus consumes adapter ActionStatusUpdate messages (§9.2.3). Nil drops
	// action status (the FleetAction lifecycle then advances only via the reconciler).
	ActionStatus ActionStatusIngestor
	// UpdateProgress consumes adapter UpdateProgress messages (§6.6/§6.7). Nil drops
	// them (rollout currentBatch phases then stay at the controller's initial value).
	UpdateProgress UpdateProgressIngestor
	// Capabilities projects an adapter CapabilitiesSnapshot's supported-action
	// catalog onto FleetAdapter.status (§9.2). Nil drops the snapshot.
	Capabilities CapabilitiesIngestor
	// Authorizer enforces per-message robot_id authorization (§9.5.1.2). When set,
	// every robot-scoped message is checked against the adapter's mTLS identity and
	// refused on failure (fail closed). Nil disables per-robot authorization — a
	// dev-mode posture, logged loudly at connect, that MUST NOT be used in
	// production (RFC-0001 §9.2.7).
	Authorizer Authorizer
	// Safety handles estop delivery over SafetyStream (§9.6.2). Nil leaves
	// SafetyStream Unavailable.
	Safety SafetyDispatcher
	// Commands handles server→adapter command-push over ControlStream (§9.2,
	// §E-2): each verified stream is registered with it and inbound CommandResults
	// are routed to it. Nil disables command-push (active probes report Unknown).
	Commands CommandDispatcher
	// Reservations services adapter-initiated shared-resource reservations (§5.4.5).
	// Nil denies every ResourceRequest (fail closed — a resource is never granted
	// when the TDE view is absent).
	Reservations ResourceReserver
	// GrantNotifier pushes async reservation_granted Commands on queue promotion
	// (§5.4.5). Nil skips the push (the promotion is still persisted to status).
	GrantNotifier ResourceGrantNotifier
	// Presence receives adapter connectivity events (connect / liveness Heartbeat /
	// disconnect) so the FleetAdapter status controller can drive status.phase
	// (§9.1.12). Nil disables presence reporting.
	Presence AdapterPresence
	// Audit records tamper-evident safety/security events (§9.5.4). Nil disables
	// audit recording (dev). A denied authorization is always recorded when set —
	// denied actions are never silently dropped.
	Audit audit.Recorder
	// Log is the structured logger. The zero (unset) value discards output.
	Log logr.Logger
}

// SafetyStream terminates one adapter's SafetyStream (RFC-0001 §9.6.2, RA-6). It
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

	identity := IdentityFromContext(ctx)
	log := s.logger().WithValues("stream", "safety", "peer", peerAddr(ctx))
	if identity.Verified {
		log = log.WithValues("adapterIdentity", identity.AdapterName, "identityNamespace", identity.Namespace)
	} else if s.Authorizer != nil {
		log.Info("SafetyStream without a verified mTLS identity; every EstopAck will be dropped (fail closed)")
	}

	deregister := s.Safety.RegisterStream(identity, &safetyWriter{stream: stream})
	defer deregister()
	log.Info("adapter SafetyStream established")

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			log.Info("adapter SafetyStream closed")
			return nil
		}
		if err != nil {
			return err
		}
		if rid := msg.GetRobotId(); rid != "" && !s.authorizeRobot(ctx, identity, rid) {
			continue // unauthorized EstopAck: drop, keep the stream (a forged robot_id is not a fault)
		}
		if ack := msg.GetEstopAck(); ack != nil {
			s.Safety.RouteAck(ack)
		}
	}
}

// ControlStream terminates one adapter's ControlStream. It requires AdapterHello
// as the first message, replies HelloAck, then dispatches subsequent messages
// until the adapter closes the stream or a connection-level fault occurs.
func (s *Server) ControlStream(stream fav1.FleetAdapterService_ControlStreamServer) error {
	ctx := stream.Context()
	w := &streamWriter{stream: stream}

	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		// §9.2.5: AdapterHello MUST be the first message. A non-hello opener is a
		// connection-level protocol violation, so a gRPC status code is correct.
		return status.Error(codes.FailedPrecondition,
			"protocol violation: first ControlStream message must be AdapterHello")
	}

	ack := helloAck(hello)
	if err := w.send(&fav1.ControlPlaneMessage{
		Payload: &fav1.ControlPlaneMessage_HelloAck{HelloAck: ack},
	}); err != nil {
		return err
	}
	if !ack.GetAccepted() {
		// Handshake refused (e.g. unsupported protocol version): the adapter has
		// been told why; close the stream cleanly.
		return nil
	}

	id := AdapterIdentity{
		Vendor:          hello.GetVendor(),
		AdapterVersion:  hello.GetAdapterVersion(),
		ProtocolVersion: hello.GetProtocolVersion(),
		Namespace:       hello.GetNamespace(),
		PeerAddr:        peerAddr(ctx),
		ContractVersion: hello.GetContractVersion(),
		// The ack settled compatibility; a negotiated contract version is present only when it
		// passed, so the verdict is read back off the ack rather than recomputed (one decision,
		// one place).
		ContractCompatible: ack.GetNegotiatedContractVersion() != "",
	}
	log := s.logger().WithValues("vendor", id.Vendor, "namespace", id.Namespace, "peer", id.PeerAddr)

	// The authenticated identity for per-message authorization is the mTLS
	// client-certificate SAN (§9.2.7) — NOT the self-reported AdapterHello above.
	identity := IdentityFromContext(ctx)
	switch {
	case s.Authorizer == nil:
		log.Info("serving ControlStream WITHOUT per-robot authorization (dev mode) — RFC-0001 §9.5.1.2 requires it in production")
	case !identity.Verified:
		log.Info("adapter presented no verified mTLS identity; every per-robot message will be denied (fail closed)")
	default:
		log = log.WithValues("adapterIdentity", identity.AdapterName, "identityNamespace", identity.Namespace)
	}
	log.Info("adapter ControlStream established")

	// Presence: a verified adapter's stream is the connectivity signal for its
	// FleetAdapter.status.phase (§9.1.12). The disconnect uses a fresh context —
	// the stream ctx is already cancelled once the loop exits.
	if s.Presence != nil && identity.Verified {
		s.Presence.AdapterConnected(ctx, identity, Negotiation{
			ProtocolVersion:    ack.GetNegotiatedProtocolVersion(),
			ContractVersion:    id.ContractVersion,
			ContractCompatible: id.ContractCompatible,
		})
		defer s.Presence.AdapterDisconnected(context.Background(), identity)
	}

	// Command-push: register the stream so verify_*/renew_lease/model_update
	// Commands can be pushed to this adapter and their CommandResults correlated
	// (§9.2, §E-2). Only a verified identity is registered (RegisterStream enforces
	// this) — nothing is pushed to an unauthenticated stream.
	if s.Commands != nil {
		deregister := s.Commands.RegisterStream(identity, w)
		defer deregister()
	}

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			log.Info("adapter ControlStream closed")
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.dispatch(ctx, w, id, identity, msg); err != nil {
			return err
		}
	}
}

// dispatch handles one AdapterMessage. It returns a non-nil error only for a
// connection-level fault that should tear the stream down; per-operation
// outcomes travel in-band (§9.2.4).
func (s *Server) dispatch(ctx context.Context, w *streamWriter, id AdapterIdentity, tlsID TLSIdentity, msg *fav1.AdapterMessage) error {
	// ── Per-message robot_id authorization (§9.5.1.2, §9.2.7) ──────────────────
	// Discover is a first announce (AuthorizeAnnounce); every other robot-scoped
	// arm names an admitted robot (AuthorizeRobot). A denial is refused in-band or
	// dropped — NEVER a stream teardown, since one stray robot_id is not a
	// connection-level fault.
	if d := msg.GetDiscover(); d != nil {
		if !s.authorizeAnnounce(ctx, tlsID, d.GetRobotId()) {
			return w.send(&fav1.ControlPlaneMessage{
				Payload: &fav1.ControlPlaneMessage_DiscoverAck{DiscoverAck: &fav1.DiscoverAck{
					Accepted: false,
					Message:  "PERMISSION_DENIED: not authorized to announce robot " + d.GetRobotId(),
				}},
			})
		}
	} else if rid := robotIDOf(msg); rid != "" {
		if !s.authorizeRobot(ctx, tlsID, rid) {
			return s.refuse(w, msg, rid)
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
		return w.send(&fav1.ControlPlaneMessage{
			Payload: &fav1.ControlPlaneMessage_RegisterAck{RegisterAck: ack},
		})
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
		if s.Presence != nil && tlsID.Verified {
			s.Presence.AdapterHeartbeat(ctx, tlsID)
		}
		// Also answer a pending per-ROBOT confirming exchange (§9.6.3.2). Adapter
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
		// unauthorized result never reaches here. Correlate it by command_id.
		if s.Commands != nil {
			s.Commands.RouteResult(p.CommandResult)
		}
		return nil
	case *fav1.AdapterMessage_ResourceRequest:
		// Adapter-initiated shared-resource reservation (§5.4.5). The robot_id was
		// already authorized above (robotIDOf covers ResourceRequest).
		return s.handleResourceRequest(ctx, w, id, p.ResourceRequest)
	case nil:
		// Empty oneof: ignore rather than fault the stream.
		return nil
	case *fav1.AdapterMessage_ActionStatus:
		// The robot's reported action state (§9.2.3). Fencing and lifecycle rules
		// live in the ingestor; a nil ingestor drops the update.
		if s.ActionStatus != nil {
			if err := s.ActionStatus.IngestActionStatus(ctx, id.Namespace, p.ActionStatus); err != nil {
				s.logger().Error(err, "ingesting task status",
					"namespace", id.Namespace, "actionID", p.ActionStatus.GetActionId())
			}
		}
		return nil
	case *fav1.AdapterMessage_UpdateProgress:
		// Advisory per-robot rollout progress (§6.6/§6.7): surfaced as the active
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
// (§5.4.5): reserve/release via the TDE, then reply with a ResourceResponse
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
func (s *Server) authorizeRobot(ctx context.Context, tlsID TLSIdentity, robotID string) bool {
	if s.Authorizer == nil {
		return true
	}
	if err := s.Authorizer.AuthorizeRobot(ctx, tlsID, robotID); err != nil {
		s.logger().Info("PERMISSION_DENIED: adapter not authorized for robot",
			"robot", robotID, "adapter", tlsID.AdapterName, "namespace", tlsID.Namespace, "reason", err.Error())
		s.recordAuthzDenied(ctx, tlsID, robotID, err.Error())
		return false
	}
	return true
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
// (§9.5.4). A denied action is always recorded — never silently dropped. The
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
// never torn down (§9.2.7 — one stray robot_id is not a connection-level fault).
func (s *Server) refuse(w *streamWriter, msg *fav1.AdapterMessage, robotID string) error {
	if msg.GetRegister() != nil {
		return w.send(&fav1.ControlPlaneMessage{
			Payload: &fav1.ControlPlaneMessage_RegisterAck{RegisterAck: &fav1.RegisterAck{
				Accepted: false,
				Message:  "PERMISSION_DENIED: not authorized for robot " + robotID,
			}},
		})
	}
	return nil
}

// robotIDOf returns the robot_id a robot-scoped AdapterMessage carries, or "" for
// arms that are not robot-addressed (Hello, Discover — handled separately — and
// ActionStatus, which is action-addressed).
func robotIDOf(msg *fav1.AdapterMessage) string {
	switch p := msg.GetPayload().(type) {
	case *fav1.AdapterMessage_Telemetry:
		return p.Telemetry.GetRobotId()
	case *fav1.AdapterMessage_Register:
		return p.Register.GetRobotId()
	case *fav1.AdapterMessage_CommandResult:
		return p.CommandResult.GetRobotId()
	case *fav1.AdapterMessage_Heartbeat:
		return p.Heartbeat.GetRobotId()
	case *fav1.AdapterMessage_Capabilities:
		return p.Capabilities.GetRobotId()
	case *fav1.AdapterMessage_ResourceRequest:
		return p.ResourceRequest.GetRobotId()
	case *fav1.AdapterMessage_UpdateProgress:
		return p.UpdateProgress.GetRobotId()
	}
	return ""
}

// ingest translates one TelemetryPayload and forwards it to the Ingestor. A
// missing robot_id or ingestion error is logged, never surfaced as a
// connection-level fault: a bad or unstorable frame must not tear down a stream
// that multiplexes every robot the adapter manages.
func (s *Server) ingest(ctx context.Context, id AdapterIdentity, tlsID TLSIdentity, p *fav1.TelemetryPayload) {
	// Count every TelemetryPayload received on ControlStream (§9.3.8), the
	// write-amplification denominator for swarmada_telemetry_status_writes_total.
	// The adapter label is the mTLS-verified FleetAdapter identity (empty in dev
	// mode without a verified certificate).
	metrics.IncTelemetryFramesReceived(tlsID.Namespace, tlsID.AdapterName)
	if p.GetRobotId() == "" {
		s.logger().V(1).Info("dropping telemetry with empty robot_id", "namespace", id.Namespace)
		return
	}
	if s.Ingestor == nil {
		// No live feed wired yet (backlog §E step 2): drop the frame. Nothing is
		// written to Robot.status on a telemetry tick (RA-1).
		return
	}
	frame := TelemetryFrame(p)
	frame.Adapter = tlsID.AdapterName // for the dropped-frames metric label (§9.3.8)
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
		compatible, reason := contract.Supports(h.GetContractVersion())
		ack := &fav1.HelloAck{
			Accepted:                  true,
			NegotiatedProtocolVersion: ProtocolVersion,
			Message:                   "welcome",
		}
		if compatible {
			// Only a negotiated connection carries a contract version: this is the version the
			// control plane will hold the adapter to.
			ack.NegotiatedContractVersion = contract.Version
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
	stream fav1.FleetAdapterService_SafetyStreamServer
}

// Send serialises one server→adapter SafetyStream message, stamping the seq.
func (w *safetyWriter) Send(msg *fav1.ControlPlaneSafetyMessage) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	msg.Seq = w.seq
	return w.stream.Send(msg)
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
