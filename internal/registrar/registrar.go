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

// Package registrar implements the control-plane side of the two-phase robot
// discovery handshake (RFC-0001 §9.2.5, §9.3.1): it answers the Discover and
// Register messages a Fleet Adapter sends over ControlStream. It satisfies
// [github.com/swarmada/swarmada/internal/controlstream.Registrar].
//
// Discover creates (or idempotently refreshes) a status-only DiscoveredRobot for
// an unadmitted robot; Register confirms an already-admitted Robot. Neither ever
// writes Robot.status — a discovery handshake is not a telemetry tick, and the
// admitted-robot lifecycle is owned by the Robot reconciler (RA-1).
package registrar

import (
	"context"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controlstream"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// DefaultTTL is the final backstop for how long a DiscoveredRobot lingers awaiting
// an admission decision before a sweeper (separate controller) may reap it. The
// live value is resolved per-namespace from SwarmadaConfig
// (spec.provisioning.discoveredRobotTTLMinutes); DefaultTTL applies only when no
// config is readable and no explicit r.TTL override is set. Recorded in
// status.ttlExpiresAt so operators and the sweeper share one deadline.
const DefaultTTL = 30 * time.Minute

// Registrar answers Discover/Register against the Kubernetes API. One instance
// serves every adapter stream, so it holds no per-call state and is safe for
// concurrent use (the client is; write conflicts are transient and retried by
// the adapter re-announcing).
type Registrar struct {
	// Client is the control-plane client used to create/look up DiscoveredRobot
	// and Robot resources.
	Client client.Client
	// APIReader is an uncached reader (the manager's APIReader) used to re-fetch an
	// existing DiscoveredRobot on a concurrent create (AlreadyExists), so a lagging
	// informer cache never NotFounds a just-created object. Nil falls back to Client
	// (test injection).
	APIReader client.Reader
	// TTL overrides the SwarmadaConfig-derived TTL for status.ttlExpiresAt. It is
	// consulted only as a fallback when the namespace's SwarmadaConfig is
	// unreadable or carries no positive discoveredRobotTTLMinutes; zero then means
	// DefaultTTL. Primarily a test-injection seam.
	TTL time.Duration
	// Now overrides the clock (tests inject a fixed time). Nil means time.Now.
	Now func() time.Time
}

var _ controlstream.Registrar = (*Registrar)(nil)

func (r *Registrar) clock() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// reader returns the uncached API reader when one is wired, else the client. It is
// used only to re-fetch an existing DiscoveredRobot on a concurrent create, where a
// cached read could still miss the object.
func (r *Registrar) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// ttlFor resolves the DiscoveredRobot TTL for a namespace from its SwarmadaConfig
// singleton (spec.provisioning.discoveredRobotTTLMinutes, §9.1.11). It FAILS SAFE
// to fallbackTTL on any problem — no SwarmadaConfig, a list error, or a
// non-positive minutes value — so an unreadable policy never breaks discovery; it
// just uses the built-in deadline instead of an operator-tuned one.
func (r *Registrar) ttlFor(ctx context.Context, namespace string) time.Duration {
	var configs fleetv1.SwarmadaConfigList
	if err := r.Client.List(ctx, &configs, client.InNamespace(namespace)); err != nil || len(configs.Items) == 0 {
		return r.fallbackTTL()
	}
	minutes := configs.Items[0].Spec.Provisioning.DiscoveredRobotTTLMinutes
	if minutes <= 0 {
		return r.fallbackTTL()
	}
	return time.Duration(minutes) * time.Minute
}

// fallbackTTL is the TTL used when SwarmadaConfig carries no usable value: the
// explicit r.TTL override if set, else DefaultTTL.
func (r *Registrar) fallbackTTL() time.Duration {
	if r.TTL > 0 {
		return r.TTL
	}
	return DefaultTTL
}

// suggestedClass returns the RobotClass to suggest for zero-touch auto-admit
// (ADR-0014 gate, ADR-0027): the sole entry of the VERIFIED adapter's
// FleetAdapter.spec.servesRobotClasses. It is empty — so auto-admit does not fire
// and the two-phase operator path stands — when the adapter is unknown, serves zero
// or several classes, or the lookup fails. The class is derived only from the
// mTLS-verified adapter name (§9.2.7); a self-reported AdapterHello never
// contributes, so a forged Hello cannot influence this privileged decision. A
// lookup error never blocks discovery (fail safe).
func (r *Registrar) suggestedClass(ctx context.Context, tlsID controlstream.TLSIdentity, namespace string) string {
	if tlsID.AdapterName == "" {
		return ""
	}
	var fa fleetv1.FleetAdapter
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: tlsID.AdapterName}, &fa); err != nil {
		return ""
	}
	if len(fa.Spec.ServesRobotClasses) == 1 {
		return fa.Spec.ServesRobotClasses[0]
	}
	return ""
}

// Discover handles a first-time announce of an unadmitted robot. It validates the
// robot_id, refuses a robot that is already admitted, and otherwise upserts a
// DiscoveredRobot carrying the reported inventory (including the MAC address) for
// an operator to admit. Every outcome is reported in-band via the ack — never a
// gRPC error (§9.2.4).
func (r *Registrar) Discover(ctx context.Context, id controlstream.AdapterIdentity, tlsID controlstream.TLSIdentity, msg *fav1.DiscoverRobot) *fav1.DiscoverAck {
	robotID := msg.GetRobotId()
	if !validRobotID(robotID) {
		return &fav1.DiscoverAck{
			Accepted:  false,
			Rejection: fav1.RegistrationRejection_REGISTRATION_REJECTION_INVALID_ROBOT_ID,
			Message:   "robot_id is empty or not a valid identifier (must be a DNS-1123 subdomain)",
		}
	}
	if id.Namespace == "" {
		return &fav1.DiscoverAck{
			Accepted:  false,
			Rejection: fav1.RegistrationRejection_REGISTRATION_REJECTION_NAMESPACE_MISMATCH,
			Message:   "adapter did not declare a namespace",
		}
	}
	key := types.NamespacedName{Namespace: id.Namespace, Name: robotID}

	// An already-admitted Robot must Register, not Discover (§9.2.5).
	var existing fleetv1.Robot
	switch err := r.Client.Get(ctx, key, &existing); {
	case err == nil:
		return &fav1.DiscoverAck{
			Accepted:  false,
			Rejection: fav1.RegistrationRejection_REGISTRATION_REJECTION_ALREADY_EXISTS,
			Message:   "robot is already admitted; reconnect via Register",
		}
	case !apierrors.IsNotFound(err):
		return &fav1.DiscoverAck{Accepted: false, Message: "control-plane lookup failed: " + err.Error()}
	}

	// Upsert the DiscoveredRobot: create it, then (re)write status so a re-announce
	// during the discovery window refreshes connectedAt/TTL. Create populates dr
	// (name/uid/resourceVersion) directly, so we do NOT read it back from the client —
	// its informer cache may not yet have observed the just-created object, and that
	// read-after-write race returned a spurious NotFound and left status unwritten. On
	// a concurrent create (AlreadyExists) we fetch the existing object via the UNCACHED
	// API reader so a lagging cache cannot NotFound it either.
	dr := &fleetv1.DiscoveredRobot{
		ObjectMeta: metav1.ObjectMeta{Namespace: id.Namespace, Name: robotID},
	}
	if cErr := r.Client.Create(ctx, dr); cErr != nil {
		if !apierrors.IsAlreadyExists(cErr) {
			return &fav1.DiscoverAck{Accepted: false, Message: "creating DiscoveredRobot failed: " + cErr.Error()}
		}
		if gErr := r.reader().Get(ctx, key, dr); gErr != nil {
			return &fav1.DiscoverAck{Accepted: false, Message: "control-plane lookup failed: " + gErr.Error()}
		}
	}

	now := r.clock()
	ttl := metav1.NewTime(now.Add(r.ttlFor(ctx, id.Namespace)))
	dr.Status = fleetv1.DiscoveredRobotStatus{
		RobotID:              robotID, // required (minLength 1) — the announced robot_id (= metadata.name)
		Phase:                fleetv1.DiscoveredRobotPhaseDiscovered,
		ConnectedAt:          metav1.NewTime(now),
		AdapterAddress:       id.PeerAddr,
		AdapterVersion:       id.AdapterVersion,
		Manufacturer:         msg.GetManufacturer(),
		Model:                msg.GetModel(),
		FirmwareVersion:      msg.GetFirmwareVersion(),
		MacAddress:           msg.GetMac(),
		ReportedCapabilities: msg.GetReportedCapabilities(),
		ReportedHardware:     mapDiscoveredHardware(msg.GetHardware()),
		ReportedModels:       mapDiscoveredModels(msg.GetInstalledModels()),
		SuggestedRobotClass:  r.suggestedClass(ctx, tlsID, id.Namespace),
		TTLExpiresAt:         &ttl,
	}
	if err := r.Client.Status().Update(ctx, dr); err != nil {
		return &fav1.DiscoverAck{Accepted: false, Message: "recording DiscoveredRobot status failed: " + err.Error()}
	}

	return &fav1.DiscoverAck{
		Accepted:            true,
		DiscoveredRobotName: robotID,
		Message:             "discovered; awaiting operator admission",
	}
}

// Register handles a reconnect of an already-admitted robot. It validates the
// robot_id, confirms a matching Robot exists, and returns the full reconnect
// state so the adapter can resynchronise after a disconnect (§9.2.5): the binding
// hints (telemetry interval, assigned zone), the current active capabilities, the
// AUTHORITATIVE action state (phase + fencing token / lease generation, so the
// adapter resumes or self-stops correctly per §9.6.3.5), and the edge endpoints of
// the robot's zone chain. All fields are read-only projections (RA-1).
func (r *Registrar) Register(ctx context.Context, id controlstream.AdapterIdentity, msg *fav1.RegisterRobot) *fav1.RegisterAck {
	robotID := msg.GetRobotId()
	if !validRobotID(robotID) {
		return &fav1.RegisterAck{
			Accepted:  false,
			Rejection: fav1.RegistrationRejection_REGISTRATION_REJECTION_INVALID_ROBOT_ID,
			Message:   "robot_id is empty or not a valid identifier (must be a DNS-1123 subdomain)",
		}
	}
	if id.Namespace == "" {
		return &fav1.RegisterAck{
			Accepted:  false,
			Rejection: fav1.RegistrationRejection_REGISTRATION_REJECTION_NAMESPACE_MISMATCH,
			Message:   "adapter did not declare a namespace",
		}
	}

	var robot fleetv1.Robot
	switch err := r.Client.Get(ctx, types.NamespacedName{Namespace: id.Namespace, Name: robotID}, &robot); {
	case apierrors.IsNotFound(err):
		return &fav1.RegisterAck{
			Accepted:  false,
			Rejection: fav1.RegistrationRejection_REGISTRATION_REJECTION_NOT_ADMITTED,
			Message:   "no admitted Robot for this id; announce it via Discover first",
		}
	case err != nil:
		return &fav1.RegisterAck{Accepted: false, Message: "control-plane lookup failed: " + err.Error()}
	}

	ack := &fav1.RegisterAck{Accepted: true, Message: "registered"}
	if robot.Spec.TelemetryIntervalSeconds != nil {
		ack.TelemetryIntervalSeconds = *robot.Spec.TelemetryIntervalSeconds
	}
	if robot.Status.CurrentZone != "" {
		ack.AssignedZone = robot.Status.CurrentZone
	} else {
		ack.AssignedZone = robot.Spec.Zone
	}
	ack.ActiveCapabilities = activeCapabilities(&robot)
	ack.AuthoritativeActionState = r.authoritativeActionState(ctx, id.Namespace, &robot)
	ack.EdgeEndpoints = r.edgeEndpoints(ctx, id.Namespace, ack.AssignedZone)
	return ack
}

// activeCapabilities lists the robot's currently-Active capability names (sorted).
func activeCapabilities(robot *fleetv1.Robot) []string {
	var out []string
	for _, c := range robot.Status.Capabilities {
		if c.Status == fleetv1.CapabilityStatusActive {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// authoritativeActionState projects the robot's assigned FleetAction into the wire
// RobotActionState the adapter uses to resume/self-stop after a reconnect (§9.6.3.5):
// no assigned action ⇒ IDLE; a claimed action with no control-plane record ⇒ UNKNOWN
// (halt and report); otherwise the mapped phase, carrying the fencing token /
// lease generation only for an actively-executing action.
func (r *Registrar) authoritativeActionState(ctx context.Context, namespace string, robot *fleetv1.Robot) *fav1.RobotActionState {
	actionName := robot.Status.AssignedAction
	if actionName == "" {
		return &fav1.RobotActionState{Phase: fav1.RobotActionPhase_ROBOT_ACTION_PHASE_IDLE}
	}
	var action fleetv1.FleetAction
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: actionName}, &action); err != nil {
		return &fav1.RobotActionState{Phase: fav1.RobotActionPhase_ROBOT_ACTION_PHASE_UNKNOWN, ActionId: actionName}
	}
	phase := mapActionPhase(action.Status.Phase)
	st := &fav1.RobotActionState{Phase: phase}
	switch phase {
	case fav1.RobotActionPhase_ROBOT_ACTION_PHASE_IN_PROGRESS,
		fav1.RobotActionPhase_ROBOT_ACTION_PHASE_REVOKING,
		fav1.RobotActionPhase_ROBOT_ACTION_PHASE_CANCELLED:
		gen := command.FencingToken(action.Status.AssignmentGeneration)
		st.ActionId = action.Name
		st.FencingToken = gen
		st.LeaseGeneration = gen
	}
	return st
}

// mapActionPhase maps a FleetAction phase to the wire RobotActionPhase. Assigned and
// InProgress mean the robot should continue executing under its live lease;
// Revoking means self-stop on lease expiry; Cancelled means halt now; every other
// phase (Pending/Paused/Preempted/terminal) means the robot is not actively
// executing this action and should be idle.
func mapActionPhase(p fleetv1.ActionPhase) fav1.RobotActionPhase {
	switch p {
	case fleetv1.ActionPhaseAssigned, fleetv1.ActionPhaseInProgress:
		return fav1.RobotActionPhase_ROBOT_ACTION_PHASE_IN_PROGRESS
	case fleetv1.ActionPhaseRevoking:
		return fav1.RobotActionPhase_ROBOT_ACTION_PHASE_REVOKING
	case fleetv1.ActionPhaseCancelled:
		return fav1.RobotActionPhase_ROBOT_ACTION_PHASE_CANCELLED
	default:
		return fav1.RobotActionPhase_ROBOT_ACTION_PHASE_IDLE
	}
}

// edgeWalkCap bounds the parentZone walk when collecting edge endpoints (defence
// against a cyclic chain the FleetZone webhook would normally reject).
const edgeWalkCap = 64

// edgeEndpoints collects the edge-node endpoints of the robot's zone and its
// ancestors (§9.2.10): a Fleet Adapter dials these facility-LAN endpoints for the
// partition-surviving edge/estop channel. Leaf zone first, then ancestors.
func (r *Registrar) edgeEndpoints(ctx context.Context, namespace, zone string) []*fav1.EdgeEndpoint {
	var out []*fav1.EdgeEndpoint
	seen := map[string]bool{}
	for depth := 0; zone != "" && depth < edgeWalkCap; depth++ {
		if seen[zone] {
			break
		}
		seen[zone] = true
		var fz fleetv1.FleetZone
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: zone}, &fz); err != nil {
			break
		}
		if fz.Spec.EdgeNode != nil && fz.Spec.EdgeNode.Address != "" {
			out = append(out, &fav1.EdgeEndpoint{Zone: zone, Address: fz.Spec.EdgeNode.Address})
		}
		zone = fz.Spec.ParentZone
	}
	return out
}

// validRobotID reports whether a robot_id is a usable Kubernetes resource name:
// non-empty and a valid DNS-1123 subdomain (the name a DiscoveredRobot/Robot
// carries). Anything else is INVALID_ROBOT_ID.
func validRobotID(id string) bool {
	return id != "" && len(validation.IsDNS1123Subdomain(id)) == 0
}

// mapDiscoveredHardware projects the reported hardware inventory into the CRD
// status shape. An unrecognised component type is recorded as Custom (a valid
// enum value) so no reported component is dropped; an unknown status collapses
// to the zero value the API server accepts.
//
// Attribute fidelity is bounded by the wire contract (ADR-0022 Tier A): the
// adapter's HardwareComponent carries only name, type, model and status, so
// Model is mapped here and the original type string is preserved into CustomType
// when it was collapsed to Custom (nothing reported is silently lost). The
// physical-measurement attributes (rangeM, fov, depthCapable, frameRateFps,
// platform dimensions, maxPayloadKg, resolutionMp) are not transmitted at
// discovery and remain unset until the proto is extended (Tier B) — an unset
// optional is the honest "unreported", not "absent".
func mapDiscoveredHardware(comps []*fav1.HardwareComponent) []fleetv1.DiscoveredHardwareComponent {
	if len(comps) == 0 {
		return nil
	}
	out := make([]fleetv1.DiscoveredHardwareComponent, 0, len(comps))
	for _, c := range comps {
		if c.GetName() == "" {
			continue
		}
		dc := fleetv1.DiscoveredHardwareComponent{
			Name:   c.GetName(),
			Type:   mapHardwareType(c.GetType()),
			Status: mapHardwareStatus(c.GetStatus()),
			Model:  c.GetModel(),
		}
		// An unrecognised, non-empty type string is collapsed to Custom above;
		// preserve the operator-defined subtype in CustomType so it survives the
		// admission round-trip into Robot.spec.hardware[]. A raw string that is
		// already the Custom enum value carries no extra subtype.
		if raw := c.GetType(); dc.Type == fleetv1.HardwareTypeCustom &&
			raw != "" && raw != string(fleetv1.HardwareTypeCustom) {
			dc.CustomType = raw
		}
		// Numeric attributes (fleet_adapter.v1 HardwareComponent tags 6-13). Explicit presence is
		// carried THROUGH rather than flattened: the wire fields are proto3 `optional` and the CRD
		// fields are pointers, so an attribute the adapter did not report stays nil instead of
		// becoming 0/false.
		//
		// Assigned directly, NOT via the Get*() accessors: those return the zero value for an unset
		// field, which would publish a 0 kg payload ceiling or a 0 m sensing range as though it had
		// been measured — the exact confusion AGENTS.md's explicit-presence invariant exists to
		// prevent, and one a scheduler would act on.
		dc.MaxPayloadKg = c.MaxPayloadKg
		dc.ResolutionMp = c.ResolutionMp
		dc.RangeM = c.RangeM
		dc.HorizontalFovDeg = c.HorizontalFovDeg
		dc.DepthCapable = c.DepthCapable
		dc.FrameRateFps = c.FrameRateFps
		dc.PlatformLengthMm = c.PlatformLengthMm
		dc.PlatformWidthMm = c.PlatformWidthMm
		out = append(out, dc)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapDiscoveredModels projects the reported installed-model list into the CRD
// status shape.
func mapDiscoveredModels(models []*fav1.InstalledModel) []fleetv1.DiscoveredModel {
	if len(models) == 0 {
		return nil
	}
	out := make([]fleetv1.DiscoveredModel, 0, len(models))
	for _, m := range models {
		if m.GetName() == "" {
			continue
		}
		out = append(out, fleetv1.DiscoveredModel{
			Name:    m.GetName(),
			Version: m.GetVersion(),
			Status:  mapModelStatus(m.GetStatus()),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapHardwareType maps the adapter's free-form component type string onto the CRD
// enum, defaulting an unrecognised value to Custom so it stays enum-valid.
func mapHardwareType(t string) fleetv1.HardwareComponentType {
	switch fleetv1.HardwareComponentType(t) {
	case fleetv1.HardwareTypeLidar, fleetv1.HardwareTypeCamera, fleetv1.HardwareTypeGripper,
		fleetv1.HardwareTypeLoadPlatform, fleetv1.HardwareTypeArm, fleetv1.HardwareTypeThermal,
		fleetv1.HardwareTypeMicrophone, fleetv1.HardwareTypeDisplay, fleetv1.HardwareTypeCustom:
		return fleetv1.HardwareComponentType(t)
	default:
		return fleetv1.HardwareTypeCustom
	}
}

// mapHardwareStatus maps the proto HardwareStatus enum onto the CRD one. An
// UNSPECIFIED/unknown value collapses to the empty status.
func mapHardwareStatus(s fav1.HardwareStatus) fleetv1.HardwareStatus {
	switch s {
	case fav1.HardwareStatus_HARDWARE_STATUS_HEALTHY:
		return fleetv1.HardwareHealthy
	case fav1.HardwareStatus_HARDWARE_STATUS_DEGRADED:
		return fleetv1.HardwareDegraded
	case fav1.HardwareStatus_HARDWARE_STATUS_FAILED:
		return fleetv1.HardwareFailed
	case fav1.HardwareStatus_HARDWARE_STATUS_DISABLED:
		return fleetv1.HardwareDisabled
	default:
		return ""
	}
}

// mapModelStatus maps the proto ModelStatus enum onto the CRD one. An
// UNSPECIFIED/unknown value collapses to Inactive.
func mapModelStatus(s fav1.ModelStatus) fleetv1.ModelStatus {
	switch s {
	case fav1.ModelStatus_MODEL_STATUS_ACTIVE:
		return fleetv1.ModelStatusActive
	case fav1.ModelStatus_MODEL_STATUS_UPDATING:
		return fleetv1.ModelStatusUpdating
	case fav1.ModelStatus_MODEL_STATUS_FAILED:
		return fleetv1.ModelStatusFailed
	default:
		return fleetv1.ModelStatusInactive
	}
}
