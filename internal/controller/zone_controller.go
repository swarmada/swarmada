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

package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/kubeclientmetrics"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
	"github.com/swarmada/swarmada/internal/zonegeometry"
)

// ZoneController maintains the FleetZone hierarchy topology (isLeaf/childZones/
// robotCount) and derives Robot.status.currentZone from live position telemetry
// via point-in-polygon containment (RFC-0001 #control-plane-zone-controller, #fleetzone-zone-controller-current-zone-derivation).
//
// It is both a FleetZone reconciler and a telemetry.PositionObserver. The
// reconciler rebuilds an in-memory per-namespace geometry cache; the observer
// reads that cache on each position frame and writes currentZone ONLY on a zone
// transition — never per tick (RA-1). Per-robot transition writes are serialised
// by a compare-and-set so concurrent frames never double-write.
type ZoneController struct {
	client.Client
	// APIReader reads the API server, bypassing the cache, for the stop a currentZone move is
	// held on (holdZoneMove). Nil falls back to Client, which only a test without a manager has.
	// It is read only where a currentZone write may follow: a frame that moves nothing, and a
	// robot a declared zone has nothing to write for, are decided on the cache (zoneReader).
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	// TDE receives zone entry/exit signals so a reservation moves Reserved→Occupied
	// on entry and is released on exit (RFC-0001 #tde-reservation-state). Optional; nil disables the signal.
	TDE ZoneReservationNotifier
	// Admission pushes zone_admission holds/admits on capacity changes (RFC-0001 #control-plane-zone-controller). Nil
	// disables the push (no behaviour beyond the existing robotCount status).
	Admission ZoneAdmissionNotifier
	// Constraints pushes the operating rules in force where a robot now is, on a
	// containment transition and on a restriction-projection change (FA-R7, ADR-0059).
	// Nil disables the push; status.containingZones is still derived and written, so the
	// API still reports what binds a robot where no adapter is listening.
	Constraints ZoneConstraintsNotifier

	zonesMu sync.RWMutex
	zones   map[string][]zone.Candidate // namespace → zone geometries (immutable slices)
	// declaredOnly holds, per namespace, the zones whose geometry state is not
	// Containment: the zones a robot is placed in by its spec.zone rather than by its
	// position (FZ-G5). It is built from the same zone listing as zones and swapped
	// with it under zonesMu, so a position frame never reads the two from different
	// listings. Both are rebuilt on every FleetZone reconcile, and every FleetZone
	// create, update and delete enqueues one, so this set is exactly as current as the
	// candidate set the observer already relies on.
	declaredOnly map[string]map[string]bool // namespace → zone name → declared-only

	lastMu   sync.Mutex
	lastZone map[string]string // zoneMemoKey(namespace, robot_id) → last written currentZone
	// lastContaining is the last written containment SET, and lastPushed the fingerprint
	// of the last rule set SENT. Two memos rather than one because the two events are
	// different sizes: a robot crossing between two unrestricted zones changes its
	// containment and changes nothing an adapter must do (ADR-0059; zone_containment.go).
	lastContaining map[string][]string
	// lastRead is the robot's resourceVersion the memos were last decided against: the copy a
	// frame read past the cache, or the one a write returned. It saves reads, and is not what
	// makes the cache-decided frame safe (that is recordsHold's check that the cached copy
	// records what the frame derives): once the informer delivers a write by anyone else, the
	// cached version differs from it, and the robot's next frame reads it past the cache.
	lastRead   map[string]string
	lastPushed map[types.NamespacedName]pushedConstraints
	// positioned records, per zoneMemoKey, that at least one frame carrying a position
	// has reached ObservePosition for the robot since this process started. It is the
	// only evidence that separates "reported a position that matched no polygon" from
	// "never reported a position": both leave currentZone and containingZones empty.
	// The Robot reconciler reads it through PositionObserved for the ZonePositionOverdue
	// condition. It is memory only and is never written to status.
	positioned map[string]bool
	// robotMu serializes, per robot, each attempt to decide and write its zone: the read past the
	// cache, the stop check, the status write and the memos it advances (ObservePosition,
	// placeDeclaredMembers). Two streams can deliver frames for one robot at once (an adapter's
	// reconnect). Without it, a frame could read the robot while another frame's write was in
	// flight, decide on that copy and note its version, and the robot would stay recorded where
	// that write put it, with the frames that follow decided on a cache that shows it elsewhere.
	// It is taken before lastMu, never while lastMu is held; the calls made under it carry
	// robotLockedTimeout. Nothing on the stop path takes it.
	robotMu robotLocks

	// ready reports whether the controller has loaded zone geometry at least once,
	// so currentZone-based TDE recovery validation can be trusted (RFC-0001 #tde-restart-recovery).
	ready atomic.Bool

	admitMu sync.Mutex
	held    map[string]bool // "namespace/robot" → currently held at its leaf zone

	edgeMu      sync.Mutex
	edgeAlerted map[string]bool // "namespace/zone" → EdgeFeedUnavailable warning is outstanding

	// restrictMu guards the restriction projection memo (FZ-R5). restrictFP holds,
	// per zone, a fingerprint of everything the last resolution READ — the ancestor
	// chain's names and generations. The projection is recomputed only when that
	// fingerprint moves, which is precisely "a spec or topology change" and
	// precisely NOT a telemetry tick: a position frame changes no generation and no
	// chain membership.
	//
	// A DeepEqual guard on the write alone would not be enough. It would stop the
	// per-tick WRITE, which is what RA-1 forbids, but FZ-R5 is stated as a rule about
	// RECOMPUTATION, and a resolver that runs on every frame is a resolver whose cost
	// scales with telemetry volume. The memo makes the rule true rather than merely
	// unobservable.
	restrictMu           sync.Mutex
	restrictFP           map[string]string // "namespace/zone" → fingerprint of the inputs last resolved
	restrictRecomputes   atomic.Int64      // observable proof of FZ-R5; read by RestrictionRecomputes
	contradictionAlerted map[string]bool   // "namespace/zone" → RestrictionContradiction warning is outstanding
	// occupantAlerted latches occupantAction: Report's one-shot event the same way
	// contradictionAlerted latches the contradiction warning — same map shape, same
	// mutex, same "was it already true" test. It records a STATE, so lifting a denial
	// re-arms it and a second denial reports again (zone_occupant_report.go).
	occupantAlerted map[string]bool // "namespace/zone" → the zone is denied-with-occupants
}

// RestrictionRecomputes reports how many times this controller has actually folded
// an ancestor chain into a restriction projection.
//
// It exists so FZ-R5 can be TESTED rather than asserted. "The projection is not
// recomputed on a telemetry tick" is otherwise an invisible property: a correct
// implementation and one that recomputes on every position frame produce identical
// status, and differ only in work done. This counter is the difference.
func (z *ZoneController) RestrictionRecomputes() int64 { return z.restrictRecomputes.Load() }

// ZoneAdmissionNotifier pushes a zone_admission hold/admit Command to a robot at a
// leaf-zone boundary (RFC-0001 #control-plane-zone-controller). The command Dispatcher satisfies it structurally.
type ZoneAdmissionNotifier interface {
	NotifyZoneAdmission(ctx context.Context, namespace, robotID, zoneName string, admit bool)
}

// Ready reports whether the Zone Controller has loaded zone geometry (its position
// derivation is live). The TDE recovery runnable waits on it before validating
// Occupied reservations against Robot.status.currentZone (RFC-0001 #tde-restart-recovery).
func (z *ZoneController) Ready() bool { return z.ready.Load() }

// ZoneReservationNotifier is the narrow view of the TDE the Zone Controller needs:
// it signals when a robot enters or leaves a zone. The signal is derived from the
// existing currentZone transition — no second position path is introduced.
type ZoneReservationNotifier interface {
	OnRobotEnteredZone(ctx context.Context, namespace, zone, robotID string) error
	OnRobotExitedZone(ctx context.Context, namespace, zone, robotID string) error
}

// +kubebuilder:rbac:groups=swarmada.io,resources=fleetzones,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=fleetzones/status,verbs=get;update;patch

// Reconcile recomputes a FleetZone's topology status and refreshes the namespace
// geometry cache used by the position observer.
func (z *ZoneController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("fleetzone", req.NamespacedName)

	var list fleetv1.FleetZoneList
	if err := z.List(ctx, &list, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing zones: %w", err)
	}
	childrenOf, depthOf := buildTree(list.Items)
	declaredOnly := declaredOnlyFrom(list.Items)
	z.setZoneIndex(req.Namespace, candidatesFrom(list.Items, childrenOf, depthOf), declaredOnly)

	zoneObj := &fleetv1.FleetZone{}
	if err := z.Get(ctx, req.NamespacedName, zoneObj); err != nil {
		// Deleted: its parent (re-queued via the watch mapper) will drop it from
		// childZones. Nothing else to do for the vanished zone.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	children := childrenOf[zoneObj.Name]
	robotCount, err := z.countSubtreeRobots(ctx, req.Namespace, zoneObj.Name, childrenOf)
	if err != nil {
		return ctrl.Result{}, err
	}

	original := zoneObj.DeepCopy()
	zoneObj.Status.IsLeaf = len(children) == 0
	zoneObj.Status.ChildZones = children
	zoneObj.Status.RobotCount = robotCount
	state, frameID := geometryStateOf(&zoneObj.Spec)
	zoneObj.Status.Geometry = &fleetv1.ZoneGeometryStatus{State: state, FrameID: frameID}
	computeZoneConditions(zoneObj)
	z.reconcileRestrictions(ctx, zoneObj, list.Items)

	// Did the PUBLISHED projection move? Computed before the patch, because the patch is
	// what makes it readable to anyone else, and used after it for the same reason: the
	// re-push below re-reads the zone list through the client, and reading it before the
	// write would resolve against the projection this reconcile just replaced.
	restrictionsMoved := !equality.Semantic.DeepEqual(original.Status.Restrictions, zoneObj.Status.Restrictions)

	if !equality.Semantic.DeepEqual(original.Status, zoneObj.Status) {
		if err := z.Status().Patch(ctx, zoneObj, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching zone status: %w", err)
		}
		logger.V(1).Info("zone topology updated", "isLeaf", zoneObj.Status.IsLeaf,
			"childZones", children, "robotCount", robotCount)
	}

	// The temporal half: a rule written AFTER a robot entered the zone reaches it (FA-R7).
	// Gated on the PROJECTION having moved rather than on the reconcile having run, so a
	// resync, an unrelated spec edit, or a robotCount change sends nobody anything — and
	// FZ-R5 already guarantees the projection is a spec-or-topology product and never a
	// telemetry tick, so this is a material transition by construction.
	if restrictionsMoved {
		// The audit record (ADR-0058 §11). After the patch, so a failed write that
		// retries does not announce the same change twice.
		z.announceRestrictionsChanged(zoneObj, original.Status.Restrictions, zoneObj.Status.Restrictions)
		z.repushOnRestrictionChange(ctx, zoneObj)
		// occupantAction: Report's one observable effect (ADR-0058 §10). On the SAME gate
		// and for the same reason: the projection having moved is a spec-or-topology change
		// by FZ-R5, so this cannot fire on a telemetry tick. After the patch, because the
		// occupant read goes through the client and the denial it tests for is the one this
		// reconcile just published.
		z.reportOccupantsOnEntryDenial(ctx, zoneObj)
	}

	z.reconcileEdgeFeed(zoneObj)

	if err := z.placeDeclaredMembers(ctx, zoneObj, declaredOnly); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// placeDeclaredMembers writes declared membership (FZ-G5) for every robot whose
// spec.zone names zoneObj, when zoneObj's geometry state is not Containment:
// status.currentZone becomes the zone's name and status.specZoneMatchesCurrent
// becomes true. It does nothing for a zone with usable geometry.
//
// It also withdraws a robot that reports currentZone = zoneObj but whose spec.zone no
// longer names it, for example after the robot is re-homed. Nothing places such a
// robot in zoneObj any more: not a declaration, and not a derivation, because the zone
// has no usable geometry. The robot moves to the zone its new declaration places it in,
// or to "" when only a position frame could place it, which candidate filter 2 reads as
// unconfirmed. Without this, a robot re-homed out of an identity-only zone and sending
// no telemetry would keep receiving that zone's work.
//
// # Why this site as well as ObservePosition
//
// ObservePosition runs on a position frame. A robot whose platform reports no
// position never reaches it, and would hold currentZone "" indefinitely; candidate
// filter 2 then excludes it from every zone-scoped action. The zone reconcile is the
// one place that sees the robot without needing a frame from it.
//
// # RA-1
//
// A FleetZone reconcile is enqueued by an object event: a zone write, or a Robot
// create, delete, spec.zone change or currentZone change. No telemetry frame enqueues
// it directly. Each robot is compared against its persisted status and patched only
// when that status differs from the declared placement, so a reconcile of an
// unchanged namespace writes nothing. A currentZone write here re-enqueues the
// namespace's zones through robotToZones, and that pass finds nothing to write, which
// ends the cycle. A currentZone change written by ObservePosition also enqueues this
// path, and also finds nothing to write, because ObservePosition places a
// declared-only robot at the same zone (placeRobot).
func (z *ZoneController) placeDeclaredMembers(
	ctx context.Context, zoneObj *fleetv1.FleetZone, declaredOnly map[string]bool,
) error {
	if !declaredOnly[zoneObj.Name] {
		return nil
	}
	var robots fleetv1.RobotList
	if err := z.List(ctx, &robots, client.InNamespace(zoneObj.Namespace)); err != nil {
		return fmt.Errorf("listing robots for declared zone membership: %w", err)
	}
	reader := z.zoneReader(readerZonePlacement)
	for i := range robots.Items {
		robot := &robots.Items[i]
		// Decided on the cache first: a robot the cache shows nothing to write for is not read
		// past it. A cache behind the API server can only delay a write here, never make one:
		// the write below is decided again on the API server's copy. And the fields this
		// decision reads (spec.zone, status.currentZone, status.specZoneMatchesCurrent) each
		// enqueue this reconcile again when they change (robotZoneChanged), so a robot skipped
		// on a lagging cache is placed once the cache catches up.
		if _, write := declaredPlacement(robot, zoneObj.Name, declaredOnly); !write {
			continue
		}
		if err := z.placeDeclaredMember(ctx, robot, zoneObj.Name, declaredOnly, reader); err != nil {
			return err
		}
	}
	return nil
}

// placeDeclaredMember decides robot's declared placement in zoneName again on the API server's
// copy and writes it, holding the robot's lock (robotMu) from the read to the memos, as a frame's
// attempt does.
func (z *ZoneController) placeDeclaredMember(
	ctx context.Context, robot *fleetv1.Robot, zoneName string, declaredOnly map[string]bool, reader client.Reader,
) error {
	unlock := z.robotMu.lock(client.ObjectKeyFromObject(robot))
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, robotLockedTimeout)
	defer cancel()
	if reader != nil {
		// The hold below decides on the recorded zone as the API server has it, not as the
		// cache has it (the stop guard, estopscope).
		fresh := &fleetv1.Robot{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(robot), fresh); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("re-reading robot %q for declared zone membership: %w", robot.Name, err)
		}
		robot = fresh
	}
	placed, write := declaredPlacement(robot, zoneName, declaredOnly)
	if !write {
		return nil // the API server already records it; RA-1
	}
	if z.holdZoneMove(ctx, robot, robot.Status.CurrentZone, placed) {
		return nil // applied once the stop is: see ObservePosition
	}
	oldZone := robot.Status.CurrentZone
	original := robot.DeepCopy()
	robot.Status.CurrentZone = placed
	matches := placed != "" && placed == robot.Spec.Zone
	robot.Status.SpecZoneMatchesCurrent = &matches
	if err := z.Status().Patch(ctx, robot, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("writing declared currentZone for robot %q: %w", robot.Name, err)
	}
	// Advance the position observer's memo to the value just written, so the
	// robot's next frame is compared against it rather than against a value this
	// write replaced.
	if id := robot.Annotations[RobotIDAnnotation]; id != "" {
		key := zoneMemoKey(robot.Namespace, id)
		z.recordZone(key, placed, placed)
		z.noteRead(key, robot.ResourceVersion)
	}
	if oldZone != placed {
		z.signalZoneTransition(ctx, robot, oldZone, placed)
	}
	return nil
}

// declaredPlacement is placeDeclaredMembers' decision for one robot as read: the zone to write and
// whether a write is due. A robot declared in zoneName is placed there unless it already is
// (with specZoneMatchesCurrent true); a robot recorded in zoneName but no longer declared there is
// withdrawn to the zone its declaration places it in, or to ""; any other robot is left alone.
func declaredPlacement(robot *fleetv1.Robot, zoneName string, declaredOnly map[string]bool) (string, bool) {
	placed, declared := placeRobot(robot.Spec.Zone, declaredOnly, nil, nil)
	switch {
	case declared && placed == zoneName:
		if m := robot.Status.SpecZoneMatchesCurrent; robot.Status.CurrentZone == placed && m != nil && *m {
			return placed, false // already placed; RA-1
		}
		return placed, true
	case robot.Status.CurrentZone == zoneName:
		// Here, but no longer declared here: withdraw to placed, which differs from zoneName
		// in this branch, so this is always a transition.
		return placed, true
	default:
		return placed, false
	}
}

// placeRobot answers "which zone does this robot report" for the Zone Controller.
//
// When specZone names a zone in declaredOnly (geometry state not Containment), the
// answer is specZone and declared is true: membership in such a zone is declared,
// not derived (FZ-G5), whatever position the robot reports. Otherwise the answer is
// derived by containment from pos, or "" when there is no position to derive from.
//
// The short-circuit lives here rather than in zone.DeriveCurrentZone because it needs
// the robot's spec.zone, which the zone package does not see. DeriveCurrentZone is
// called unchanged and stays containment-only, so a robot whose declared zone HAS
// usable geometry and whose position matches no polygon still derives "" (FZ-G5
// narrows the unmatched path; it does not remove it).
func placeRobot(
	specZone string, declaredOnly map[string]bool, pos *zone.Position, candidates []zone.Candidate,
) (currentZone string, declared bool) {
	if specZone != "" && declaredOnly[specZone] {
		return specZone, true
	}
	if pos == nil {
		return "", false
	}
	return zone.DeriveCurrentZone(*pos, candidates), false
}

// FleetZone condition types (ADR-0023).
const (
	zoneCondReady             = "ZoneReady"
	zoneCondCapacityAvailable = "CapacityAvailable"
	zoneCondGeometryAvailable = "GeometryAvailable"
)

// geometryStateOf derives a zone's geometry state and frame id from its spec
// (RFC-0001 §9.1.6, FZ-G3/FZ-G4, FZ-C6). It is zonegeometry.StateOf, the single place
// the question "does this polygon participate in containment" is answered, which the
// edge node reads too: status.geometry echoes it, the GeometryAvailable condition
// reports it, the derivation candidate set is filtered on it, and the edge geofences
// only on it — so none of them can disagree about one zone.
func geometryStateOf(spec *fleetv1.FleetZoneSpec) (fleetv1.ZoneGeometryState, string) {
	return zonegeometry.StateOf(spec)
}

// isComposite reports whether a zone is a composite: it declares members (FZ-C2).
func isComposite(spec *fleetv1.FleetZoneSpec) bool { return zonegeometry.IsComposite(spec) }

// computeZoneConditions upserts the standard FleetZone conditions ZoneReady,
// GeometryAvailable and CapacityAvailable (ADR-0023, §9.1.6), derived from spec plus
// the already-computed status.
// It reuses upsertCondition, which moves LastTransitionTime only on a status flip,
// so an unchanged zone leaves the conditions byte-identical and the caller's
// DeepEqual guard suppresses the write (RA-1). Messages are STATIC — a live count
// in a message would differ every reconcile and churn a write on each
// telemetry-driven currentConcurrentRobots change; the numbers live in the numeric
// status fields, not the message.
func computeZoneConditions(fz *fleetv1.FleetZone) {
	// ZoneReady: serviceable. A leaf zone needs a polygon so the Zone Controller
	// can place robots by containment; without one it is not ready. Non-leaf
	// (aggregating) zones are ready once reconciled.
	if fz.Status.IsLeaf && fz.Spec.PhysicalBounds == nil {
		upsertCondition(&fz.Status.Conditions, zoneCondReady, metav1.ConditionFalse,
			"NoPhysicalBounds", "leaf zone declares no physicalBounds; robots cannot be placed by position")
	} else {
		upsertCondition(&fz.Status.Conditions, zoneCondReady, metav1.ConditionTrue,
			"Reconciled", "zone topology resolved and serviceable")
	}

	// GeometryAvailable: whether geometry-derived behaviour applies to this zone
	// (FZ-G7). Every such behaviour MUST degrade explicitly, never silently — while
	// this is False the control plane MUST NOT claim zone-boundary-breach detection
	// for the zone, because the edge trigger table evaluates cached zone polygons and
	// there is none to cache. The frame id is carried on status.geometry.frameId
	// rather than in the message, keeping the message static like the two above.
	// Read from spec through the same helper status.geometry is written from, so the
	// condition and the field cannot disagree and this does not depend on the caller
	// having derived status.geometry first.
	state, _ := geometryStateOf(&fz.Spec)
	switch {
	case isComposite(&fz.Spec):
		upsertCondition(&fz.Status.Conditions, zoneCondGeometryAvailable, metav1.ConditionFalse,
			"CompositeZone", "composite zone; any polygon it declares is a rendering envelope, never used for containment, and no robot is placed in it by position")
	case state == fleetv1.ZoneGeometryContainment:
		upsertCondition(&fz.Status.Conditions, zoneCondGeometryAvailable, metav1.ConditionTrue,
			"SiteAlignedPolygon", "site-aligned polygon; containment, boundary-breach detection and overlap arbitration active")
	case state == fleetv1.ZoneGeometryFrameLocal:
		upsertCondition(&fz.Status.Conditions, zoneCondGeometryAvailable, metav1.ConditionFalse,
			"FrameLocalGeometry", "polygon is not site-aligned; it is carried for consumers that own the transform and never used for containment")
	default:
		upsertCondition(&fz.Status.Conditions, zoneCondGeometryAvailable, metav1.ConditionFalse,
			"IdentityOnly", "zone declares no polygon; membership is declared rather than derived and boundary-breach detection is unavailable")
	}

	// CapacityAvailable: room for another robot. maxConcurrentRobots==0 is unlimited.
	max := fz.Spec.MaxConcurrentRobots
	if max == 0 || fz.Status.CurrentConcurrentRobots < max {
		upsertCondition(&fz.Status.Conditions, zoneCondCapacityAvailable, metav1.ConditionTrue,
			"Available", "zone can admit another robot")
	} else {
		upsertCondition(&fz.Status.Conditions, zoneCondCapacityAvailable, metav1.ConditionFalse,
			"AtCapacity", "zone is at its concurrent-robot capacity")
	}
}

// reconcileEdgeFeed emits an EdgeFeedUnavailable Warning while an edge-node zone
// carries a non-empty status.edgeFeedUnavailable (the edge node reporting robots
// whose adapter never established its EdgeStream, RFC-0001 #fleet-adapter-protocol-the-edge-stream). The edge writes the
// status; the Zone Controller owns the event so the "boundary-breach trigger is
// degraded" fact is never silent. Emission is edge-triggered (once per transition
// into the unavailable state) via an in-memory latch, so a resync does not re-warn;
// clearing the list emits a matching EdgeFeedRestored. A nil Recorder disables it.
func (z *ZoneController) reconcileEdgeFeed(zoneObj *fleetv1.FleetZone) {
	if z.Recorder == nil {
		return
	}
	key := zoneObj.Namespace + "/" + zoneObj.Name
	unavailable := zoneObj.Spec.EdgeNode != nil && len(zoneObj.Status.EdgeFeedUnavailable) > 0

	z.edgeMu.Lock()
	if z.edgeAlerted == nil {
		z.edgeAlerted = map[string]bool{}
	}
	already := z.edgeAlerted[key]
	z.edgeAlerted[key] = unavailable
	z.edgeMu.Unlock()

	switch {
	case unavailable && !already:
		z.Recorder.Event(zoneObj, corev1.EventTypeWarning, "EdgeFeedUnavailable",
			fmt.Sprintf("no EdgeStream feed for robot(s) %s in edge-node zone %q; zone-boundary-breach trigger is degraded for them",
				strings.Join(zoneObj.Status.EdgeFeedUnavailable, ", "), zoneObj.Name))
	case !unavailable && already:
		z.Recorder.Event(zoneObj, corev1.EventTypeNormal, "EdgeFeedRestored",
			fmt.Sprintf("all EdgeStream feeds restored for edge-node zone %q", zoneObj.Name))
	}
}

// ObservePosition implements telemetry.PositionObserver: it derives the robot's
// current zone from the frame's position and writes Robot.status.currentZone only
// on a transition.
func (z *ZoneController) ObservePosition(ctx context.Context, f telemetry.Frame) {
	if f.Position == nil {
		return
	}
	robot, err := findTelemetryRobot(ctx, z.Client, f.Namespace, f.RobotID)
	if err != nil {
		log.FromContext(ctx).Error(err, "resolving robot for zone derivation", "robotID", f.RobotID)
		return
	}
	if robot == nil {
		return // robot_id not mapped to a Robot yet; drop, don't fail
	}
	// A frame that moves nothing is decided on the cache. When the cached copy records the zone
	// and containment set the memos hold, and the frame derives that same zone and set, applyFrame
	// would write nothing for that copy, so the read past the cache, and the hold check behind
	// it, are skipped. Skipping them writes nothing, so it cannot move a robot out of a stop: the
	// robot stays where it is recorded, as a held move does. The one way the API server can
	// differ is a write the cache has not delivered yet (an edit by someone else; only this
	// controller writes these fields otherwise), and that only delays the write-back: once the
	// informer delivers it, the cached copy no longer matches and the next frame reads the robot
	// past the cache. The cached copy must also be the version last decided on (lastRead), which
	// saves the reads a version already decided would cost.
	if z.frameMovesNothing(f, robot) {
		return
	}
	// The cache resolved the robot; the API server says where it is recorded now. A cached
	// copy can lag the write that put the robot into a zone whose stop is being applied, and
	// the hold below decides on the recorded zone (the stop guard, estopscope). The status
	// write then carries the resourceVersion it read, so a stale frame cannot overwrite a
	// newer record. A write refused on a conflict does not drop the frame: a stop being
	// applied writes the same robot's status (its record, estopState, estopHeldBy) just when
	// the first frame that arrives once the move is no longer held must write the zone it
	// derives (RFC-0001 #control-plane-zone-controller), and a stopped robot may send no next
	// frame. The robot is read again and this frame derived again against it, under a backoff
	// that outlasts those writes, as patchHolds' does. Only a frame refused at every attempt is
	// dropped, and the next one derives the move again.
	//
	// Each attempt holds the robot's lock (robotMu) from its read to its memos, so a frame from a
	// second stream reads the robot only once this one's write has committed or failed.
	key := client.ObjectKeyFromObject(robot)
	rd := z.zoneReader(readerPositionFrame)
	if rd == nil {
		rd = z.Client
	}
	attempts := 0
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		attempts++
		unlock := z.robotMu.lock(key)
		defer unlock()
		ctx, cancel := context.WithTimeout(ctx, robotLockedTimeout)
		defer cancel()
		if z.APIReader != nil || attempts > 1 {
			fresh := &fleetv1.Robot{}
			if err := rd.Get(ctx, key, fresh); err != nil {
				log.FromContext(ctx).Error(err, "re-reading the robot for zone derivation; frame dropped", "robot", key.Name)
				return err
			}
			robot = fresh
		}
		return z.applyFrame(ctx, f, robot)
	})
	if apierrors.IsConflict(err) {
		log.FromContext(ctx).Error(err, "currentZone write refused on a conflict at every attempt; frame dropped, the next frame retries",
			"robot", key.Name, "attempts", attempts)
	}
}

// frameMovesNothing marks the robot positioned and reports whether the frame moves nothing: the
// cached robot is the version last decided on, recorded as the memos hold, and the frame derives
// that zone and containment set (recordsHold). applyFrame would then write nothing. It reads no
// API: the robot is the cache's copy.
func (z *ZoneController) frameMovesNothing(f telemetry.Frame, robot *fleetv1.Robot) bool {
	memo := zoneMemoKey(robot.Namespace, f.RobotID)
	z.zonesMu.RLock()
	candidates := z.zones[robot.Namespace]
	declaredOnly := z.declaredOnly[robot.Namespace]
	z.zonesMu.RUnlock()
	pos := zone.Position{X: f.Position.X, Y: f.Position.Y, Floor: f.Position.Floor}
	newZone, _ := placeRobot(robot.Spec.Zone, declaredOnly, &pos, candidates)
	if !z.recordsHold(memo, robot, newZone, zone.DeriveContainingZones(pos, candidates)) {
		return false
	}
	z.markPositioned(memo)
	return true
}

// applyFrame derives the zone and the containment set of robot, as read, from the frame, and
// writes both when either moved. It returns the status write's error; the memos are then
// forgotten, so the next derivation (this frame's retry, or the next frame) compares against
// what is recorded.
func (z *ZoneController) applyFrame(ctx context.Context, f telemetry.Frame, robot *fleetv1.Robot) error {
	// Recorded before the status patch below, so the Robot reconcile that patch enqueues
	// already sees the robot as having reported a position.
	memo := zoneMemoKey(robot.Namespace, f.RobotID)
	z.markPositioned(memo)
	// The memos must describe the robot as read; if they do not, what is recorded wins.
	z.dropMemosRecordedOtherwise(memo, robot)

	z.zonesMu.RLock()
	candidates := z.zones[robot.Namespace]
	declaredOnly := z.declaredOnly[robot.Namespace]
	z.zonesMu.RUnlock()

	pos := zone.Position{
		X:     f.Position.X,
		Y:     f.Position.Y,
		Floor: f.Position.Floor,
	}
	// A robot whose declared zone has no usable geometry reports that zone whatever
	// this frame says (FZ-G5), so it is never unmatched here and the event below
	// cannot fire for it. The value equals what placeDeclaredMembers writes, so a
	// frame never undoes that write.
	newZone, _ := placeRobot(robot.Spec.Zone, declaredOnly, &pos, candidates)
	// A robot whose recorded zone is under a stop still being applied stays recorded there
	// until the stop is applied: the fan-out finds robots by this field, and a move out of
	// the stopped subtree inside that window would leave the robot outside the stop for good
	// (estopscope.PendingStopsOver). Substituting the recorded zone before the compare-and-set
	// keeps the memo where it is, so the robot's next frame derives and applies the move
	// again; containment is not held.
	if held := z.holdZoneMove(ctx, robot, robot.Status.CurrentZone, newZone); held {
		newZone = robot.Status.CurrentZone
	}
	// The containment SET, which is what BINDS the robot, beside the arbitrated winner,
	// which is where it REPORTS it is (ADR-0059). Derived from the same frame over the
	// same candidates; DeriveCurrentZone is untouched and still single-valued.
	newContaining := zone.DeriveContainingZones(pos, candidates)

	// RA-1, AND THIS IS THE LINE THE WHOLE FEATURE TURNS ON. Both compare-and-sets run,
	// because each memo must advance whether or not the other moved — short-circuiting
	// on the first would leave the second seeded from a stale value and turn the NEXT
	// frame into a false transition. A frame that moves neither returns here having
	// written nothing and sent nothing.
	zoneMoved := z.recordZone(memo, newZone, robot.Status.CurrentZone)
	containmentMoved := z.recordContainment(memo, newContaining, robot.Status.ContainingZones)
	if !zoneMoved && !containmentMoved {
		z.noteRead(memo, robot.ResourceVersion)
		return nil // unchanged since the last write (or persisted status) — RA-1
	}

	oldZone := robot.Status.CurrentZone
	original := robot.DeepCopy()
	robot.Status.CurrentZone = newZone
	robot.Status.ContainingZones = newContaining
	matches := newZone != "" && newZone == robot.Spec.Zone
	robot.Status.SpecZoneMatchesCurrent = &matches
	if err := z.Status().Patch(ctx, robot, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		// The memos advanced before the write; forget them, so the next derivation seeds
		// them from what is recorded then and applies the move again.
		z.forgetMemos(memo)
		if apierrors.IsConflict(err) {
			log.FromContext(ctx).V(1).Info("currentZone write refused: the robot changed under the frame; deriving again",
				"robot", robot.Name, "zone", newZone)
			return err
		}
		log.FromContext(ctx).Error(err, "writing currentZone", "robot", robot.Name, "zone", newZone)
		return err
	}
	// The write returned the robot's new version; the memos describe it.
	z.noteRead(memo, robot.ResourceVersion)

	// FA-R7 — tell the adapter what may be running where the robot NOW IS. Gated a
	// second time inside, on the resolved RULE set rather than on the containment set:
	// crossing from one unrestricted zone into another is a containment transition that
	// changes nothing an adapter must do, and re-sending an identical set would be a
	// command per boundary crossing.
	if containmentMoved {
		z.pushZoneConstraints(ctx, robot, newContaining)
	}

	z.signalZoneTransition(ctx, robot, oldZone, newZone)

	if newZone == "" && z.Recorder != nil {
		z.Recorder.Event(robot, corev1.EventTypeWarning, "ZonePositionUnmatched",
			"robot position matched no zone polygon; currentZone cleared")
	}
	return nil
}

// holdZoneMove reports whether a currentZone move from from to to is held: from is under a
// zone stop the robot does not record and to is not. That is a stop issued and not yet applied
// (the fan-out finds robots by this field, and would never find the robot), or a stop in force
// that has not reached the robot yet (it came under the stop a moment ago, and the pass that
// stops and records it has not run). A stop the robot records travels with it. An empty from is
// the robot's spec.zone, which is where the fan-out finds a robot with no currentZone, so its
// first position report cannot take it out of a stop either. The zones are read through
// APIReader (the API server), not the cache, which can miss a trigger written a moment earlier
// (the stop guard, estopscope). An unreadable zone holds the move: the hold fails closed. A move
// into a zone, or between zones with no stop, is never held.
func (z *ZoneController) holdZoneMove(ctx context.Context, robot *fleetv1.Robot, from, to string) bool {
	if from == "" {
		from = robot.Spec.Zone
	}
	if from == "" || from == to {
		return false
	}
	rd := z.zoneReader(readerZoneHold)
	if rd == nil {
		rd = z.Client
	}
	tree, configs, err := estopscope.ReadWorld(ctx, rd, robot.Namespace)
	if err != nil {
		log.FromContext(ctx).Error(err, "reading the zones to check a currentZone move against stops; holding it",
			"robot", robot.Name, "from", from, "to", to)
		return true
	}
	was := robot.DeepCopy()
	was.Status.CurrentZone = from
	now := robot.DeepCopy()
	now.Status.CurrentZone = to
	left, err := estopscope.MoveLoses(tree, configs, was, now)
	if err != nil {
		log.FromContext(ctx).Error(err, "checking a currentZone move against stops; holding it",
			"robot", robot.Name, "from", from, "to", to)
		return true
	}
	if len(left) == 0 {
		return false
	}
	names := make([]string, 0, len(left))
	for _, l := range left {
		names = append(names, string(l.Kind)+"/"+l.Name)
	}
	log.FromContext(ctx).V(1).Info("currentZone move held until the stop reaches the robot",
		"robot", robot.Name, "from", from, "to", to, "stops", names)
	return true
}

// signalZoneTransition drives what hangs off a written currentZone transition: the
// reservation lifecycle and zone-capacity admission. Both writers of currentZone call
// it (ObservePosition and placeDeclaredMembers), so a robot placed by declaration
// reaches the TDE and admission the same way a robot placed by position does, which
// FZ-G6 requires of capacity enforcement.
//
// It does nothing unless currentZone actually changed. ObservePosition also writes
// when only the containment set moved, and then oldZone == newZone. Running the pair
// for that write would exit the zone, which removes the robot's Occupied reservation
// there, and then enter it, which only promotes a Reserved entry and so restores
// nothing. The robot would lose its slot in a zone it never left, and the zone's
// maxConcurrentRobots gate would count it free.
func (z *ZoneController) signalZoneTransition(ctx context.Context, robot *fleetv1.Robot, oldZone, newZone string) {
	if oldZone == newZone {
		return
	}
	// Signal the reservation lifecycle off this same currentZone transition: the
	// robot left oldZone and entered newZone (RFC-0001 #tde-reservation-state). OnRobotEnteredZone moves a
	// held reservation Reserved→Occupied; both are no-ops when the robot holds no
	// reservation in that zone. Keyed by the Robot name (the TDE reservation's
	// RobotID, matching the gate's ReservationRequest).
	if z.TDE != nil {
		if oldZone != "" {
			_ = z.TDE.OnRobotExitedZone(ctx, robot.Namespace, oldZone, robot.Name)
		}
		if newZone != "" {
			_ = z.TDE.OnRobotEnteredZone(ctx, robot.Namespace, newZone, robot.Name)
		}
	}

	// Zone-capacity hold/admit (RFC-0001 #control-plane-zone-controller): the same currentZone transition may push a
	// leaf zone over maxConcurrentRobots (hold the entrant) or free a slot (admit a
	// held robot). Material transition only — never per tick (RA-1).
	if z.Admission != nil {
		if oldZone != "" {
			z.reconcileZoneAdmission(ctx, robot.Namespace, oldZone, robot.Name, false)
		}
		if newZone != "" {
			z.reconcileZoneAdmission(ctx, robot.Namespace, newZone, robot.Name, true)
		}
	}
}

// zoneMemoKey is the key the position observer's memos (lastZone, lastContaining)
// remember a robot by. A robot_id is unique per namespace only (RFC-0001
// #discoveredrobot-conflict-handling-duplicate-robot-id, Case 3), so two robots sharing one id in different namespaces
// must not share a memo: each frame would then look like a transition against the
// other robot's last write, and write status on every tick. A namespace name cannot
// contain "/", so the key is unambiguous.
func zoneMemoKey(namespace, robotID string) string { return namespace + "/" + robotID }

// The zone controller's uncached reads, by caller (kubeclientmetrics.UncachedReadDuration).
const (
	readerZonePlacement = "zone_placement" // placeDeclaredMembers: a robot it may write
	readerPositionFrame = "position_frame" // ObservePosition: a frame that may move the robot
	readerZoneHold      = "zone_hold"      // holdZoneMove: the stops a move is checked against
)

// zoneReader is APIReader with its reads recorded under caller, or nil without one.
func (z *ZoneController) zoneReader(caller string) client.Reader {
	return kubeclientmetrics.Reader(z.APIReader, caller)
}

// recordsHold reports whether a frame deriving zone and containing for the cached robot would
// move nothing, without advancing the memos. All of these must hold:
//
//   - both memos have been seen: a robot this process has not decided on since it started (a
//     restart, a leadership change, ForgetRobot, a refused write) has none;
//   - the cached copy is the version the memos were last decided against (lastRead): this only
//     saves reads, since a version already decided needs no second look;
//   - that copy records what the memos hold, and the frame derives it. This is what makes the
//     skip safe: applyFrame, given that copy, would write nothing.
//
// Anything else takes the read past the cache, where applyFrame compares the memos with what
// the API server records and corrects either.
func (z *ZoneController) recordsHold(key string, robot *fleetv1.Robot, zone string, containing []string) bool {
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	prevZone, zoneSeen := z.lastZone[key]
	prevSet, setSeen := z.lastContaining[key]
	read, readSeen := z.lastRead[key]
	return zoneSeen && setSeen && readSeen && read != "" && read == robot.ResourceVersion &&
		robot.Status.CurrentZone == prevZone && sameZoneSet(robot.Status.ContainingZones, prevSet) &&
		prevZone == zone && sameZoneSet(prevSet, containing)
}

// noteRead records the robot's resourceVersion the memos for key now describe.
func (z *ZoneController) noteRead(key, resourceVersion string) {
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	if z.lastRead == nil {
		z.lastRead = make(map[string]string)
	}
	z.lastRead[key] = resourceVersion
}

// dropMemosRecordedOtherwise forgets the memos for key when they disagree with what robot, read
// past the cache, records: someone else edited status.currentZone or status.containingZones, or
// a race left the memos describing an older copy. The compare-and-sets then seed from what is
// recorded, so the frame writes the zone it derives over an edit, and a move a stop holds is
// held from where the robot is recorded.
func (z *ZoneController) dropMemosRecordedOtherwise(key string, robot *fleetv1.Robot) {
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	prevZone, zoneSeen := z.lastZone[key]
	prevSet, setSeen := z.lastContaining[key]
	if (zoneSeen && prevZone != robot.Status.CurrentZone) ||
		(setSeen && !sameZoneSet(prevSet, robot.Status.ContainingZones)) {
		delete(z.lastZone, key)
		delete(z.lastContaining, key)
		delete(z.lastRead, key)
	}
}

// recordZone compare-and-sets the last-derived zone for a robot under a lock, so
// concurrent frames for the same robot never both write a transition. On first
// sight it seeds from the persisted status, so a post-restart unchanged frame
// produces no write (zone-controller crash-restart behaviour). Returns true iff a write is
// warranted.
func (z *ZoneController) recordZone(key, newZone, statusZone string) bool {
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	if z.lastZone == nil {
		z.lastZone = make(map[string]string)
	}
	prev, seen := z.lastZone[key]
	if !seen {
		prev = statusZone // seed from persisted status
	}
	if prev == newZone {
		z.lastZone[key] = newZone
		return false
	}
	z.lastZone[key] = newZone
	return true
}

// forgetMemos drops the zone and containment memos for key, so the next derivation seeds
// them from the Robot's persisted status. It keeps the robot's positioned mark.
func (z *ZoneController) forgetMemos(key string) {
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	delete(z.lastZone, key)
	delete(z.lastContaining, key)
	delete(z.lastRead, key)
}

// ForgetRobot drops the position observer's memos for the robot with this wire
// robot_id in namespace, so its next frame is first sight: the memo is seeded from
// the Robot's persisted status again, and a Robot recreated under the same robot_id
// gets its currentZone written by its first frame rather than never. It is called by
// [RobotProjectionResetter] on a change of object identity, and like the projector's
// Forget it performs no write itself. It reports whether anything was held.
func (z *ZoneController) ForgetRobot(namespace, robotID string) bool {
	key := zoneMemoKey(namespace, robotID)
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	_, zoned := z.lastZone[key]
	_, contained := z.lastContaining[key]
	_, positioned := z.positioned[key]
	delete(z.lastZone, key)
	delete(z.lastContaining, key)
	delete(z.lastRead, key)
	delete(z.positioned, key)
	return zoned || contained || positioned
}

// markPositioned records that a frame carrying a position reached ObservePosition for
// the robot under key. It changes memory only.
func (z *ZoneController) markPositioned(key string) {
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	if z.positioned == nil {
		z.positioned = make(map[string]bool)
	}
	z.positioned[key] = true
}

// PositionObserved reports whether a frame carrying a position has reached
// ObservePosition for the robot with robotID in namespace since this process started.
// A false answer after a restart does not mean the robot has never reported one; the
// Robot reconciler also reads the persisted evidence (positionReported).
func (z *ZoneController) PositionObserved(namespace, robotID string) bool {
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	return z.positioned[zoneMemoKey(namespace, robotID)]
}

func (z *ZoneController) setZones(namespace string, cands []zone.Candidate) {
	z.setZoneIndex(namespace, cands, nil)
}

// setZoneIndex replaces a namespace's derivation candidates and its declared-only
// zone set together, under one lock.
func (z *ZoneController) setZoneIndex(namespace string, cands []zone.Candidate, declaredOnly map[string]bool) {
	z.zonesMu.Lock()
	defer z.zonesMu.Unlock()
	if z.zones == nil {
		z.zones = make(map[string][]zone.Candidate)
	}
	if z.declaredOnly == nil {
		z.declaredOnly = make(map[string]map[string]bool)
	}
	z.zones[namespace] = cands               // fresh slice; never mutated in place
	z.declaredOnly[namespace] = declaredOnly // fresh map; never mutated in place
	z.ready.Store(true)                      // geometry loaded → the controller is deriving zones
}

// reconcileZoneAdmission recomputes a leaf zone's admission set after robotID
// entered (or left) it, pushing zone_admission only for robots whose hold state
// changed (RFC-0001 #control-plane-zone-controller). maxConcurrentRobots==0 is unlimited (no holds). Robots are
// ranked by name; ranks below the cap are admitted, the rest held. Occupancy is
// read from the cache and corrected for the just-moved robot (cache lag), so a
// restart re-derives holds from durable currentZone rather than under-counting.
func (z *ZoneController) reconcileZoneAdmission(ctx context.Context, namespace, zoneName, movedRobot string, entered bool) {
	fz := &fleetv1.FleetZone{}
	if err := z.Get(ctx, types.NamespacedName{Namespace: namespace, Name: zoneName}, fz); err != nil {
		return
	}
	max := int(fz.Spec.MaxConcurrentRobots)
	if max <= 0 {
		return // unlimited: never hold
	}

	var robots fleetv1.RobotList
	if err := z.List(ctx, &robots, client.InNamespace(namespace)); err != nil {
		return
	}
	occ := map[string]bool{}
	for i := range robots.Items {
		if robots.Items[i].Status.CurrentZone == zoneName {
			occ[robots.Items[i].Name] = true
		}
	}
	if entered {
		occ[movedRobot] = true // may not be visible in the cache yet
	} else {
		delete(occ, movedRobot) // may still be visible in the cache
	}
	names := make([]string, 0, len(occ))
	for n := range occ {
		names = append(names, n)
	}
	sort.Strings(names)

	type admitPush struct {
		robot string
		admit bool
	}
	var pushes []admitPush
	z.admitMu.Lock()
	if z.held == nil {
		z.held = map[string]bool{}
	}
	for rank, name := range names {
		key := namespace + "/" + name
		switch shouldHold := rank >= max; {
		case shouldHold && !z.held[key]:
			z.held[key] = true
			pushes = append(pushes, admitPush{name, false})
		case !shouldHold && z.held[key]:
			delete(z.held, key)
			pushes = append(pushes, admitPush{name, true})
		}
	}
	if !entered {
		delete(z.held, namespace+"/"+movedRobot) // it left; drop its marker (no push)
	}
	z.admitMu.Unlock()

	for _, p := range pushes {
		if !p.admit && z.Recorder != nil {
			z.Recorder.Event(fz, corev1.EventTypeWarning, "ZoneCapacityReached",
				fmt.Sprintf("zone %q at maxConcurrentRobots=%d; holding robot %q at boundary", zoneName, max, p.robot))
		}
		z.Admission.NotifyZoneAdmission(ctx, namespace, p.robot, zoneName, p.admit)
	}
}

// countSubtreeRobots counts robots whose currentZone is zoneName or any of its
// descendants.
func (z *ZoneController) countSubtreeRobots(
	ctx context.Context, namespace, zoneName string, childrenOf map[string][]string,
) (int32, error) {
	subtree := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if subtree[name] {
			return // cycle guard
		}
		subtree[name] = true
		for _, c := range childrenOf[name] {
			walk(c)
		}
	}
	walk(zoneName)

	var robots fleetv1.RobotList
	if err := z.List(ctx, &robots, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("listing robots for zone count: %w", err)
	}
	var count int32
	for i := range robots.Items {
		if subtree[robots.Items[i].Status.CurrentZone] {
			count++
		}
	}
	return count, nil
}

// buildTree returns the direct-children map and the depth of each zone (root = 0),
// with a cycle guard so a malformed tree cannot loop forever.
func buildTree(zones []fleetv1.FleetZone) (childrenOf map[string][]string, depthOf map[string]int) {
	parentOf := make(map[string]string, len(zones))
	childrenOf = make(map[string][]string, len(zones))
	exists := make(map[string]bool, len(zones))
	for i := range zones {
		exists[zones[i].Name] = true
	}
	for i := range zones {
		z := &zones[i]
		parentOf[z.Name] = z.Spec.ParentZone
		if z.Spec.ParentZone != "" && exists[z.Spec.ParentZone] {
			childrenOf[z.Spec.ParentZone] = append(childrenOf[z.Spec.ParentZone], z.Name)
		}
	}
	depthOf = make(map[string]int, len(zones))
	for i := range zones {
		name := zones[i].Name
		depth := 0
		seen := map[string]bool{}
		cur := name
		for {
			p := parentOf[cur]
			if p == "" || !exists[p] || seen[cur] {
				break
			}
			seen[cur] = true
			depth++
			cur = p
		}
		depthOf[name] = depth
	}
	return childrenOf, depthOf
}

// candidatesFrom builds the derivation geometries for every zone whose polygon
// participates in containment.
//
// A zone whose geometry state is not Containment is left OUT of the candidate set
// entirely (FZ-G4), which is neither of the two wrong readings: it is not treated as
// covering no space, which would leave every robot in it unmatched, nor as covering
// all space, which would match every robot in the namespace. It covers unknown space,
// and the derivation has nothing to say about it.
func candidatesFrom(
	zones []fleetv1.FleetZone, childrenOf map[string][]string, depthOf map[string]int,
) []zone.Candidate {
	out := make([]zone.Candidate, 0, len(zones))
	for i := range zones {
		z := &zones[i]
		if state, _ := geometryStateOf(&z.Spec); state != fleetv1.ZoneGeometryContainment {
			continue
		}
		poly := make([]zone.Point, 0, len(z.Spec.PhysicalBounds.Polygon))
		for _, p := range z.Spec.PhysicalBounds.Polygon {
			poly = append(poly, zone.Point{X: p.X, Y: p.Y})
		}
		out = append(out, zone.Candidate{
			Name:    z.Name,
			Floor:   z.Spec.PhysicalBounds.Floor,
			Polygon: poly,
			IsLeaf:  len(childrenOf[z.Name]) == 0,
			Depth:   depthOf[z.Name],
			// The zone's OWN entry rule, not an inherited one: the arbitration it feeds
			// separates an exclusion from the zone that encloses it, and both inherit
			// whatever their common ancestor declares.
			DeniesEntry: z.Spec.Restrictions != nil && z.Spec.Restrictions.Entry != nil,
		})
	}
	return out
}

// declaredOnlyFrom returns the zones whose geometry state is not Containment: the
// zones FZ-G5 places robots in by declaration. It reads geometryStateOf, the helper
// candidatesFrom filters on, so every zone is in exactly one of the two sets.
func declaredOnlyFrom(zones []fleetv1.FleetZone) map[string]bool {
	out := make(map[string]bool)
	for i := range zones {
		if state, _ := geometryStateOf(&zones[i].Spec); state != fleetv1.ZoneGeometryContainment {
			out[zones[i].Name] = true
		}
	}
	return out
}

// robotZoneChanged admits the Robot events that can move a zone's status or a
// robot's declared placement: create, delete, and an update that changes
// status.currentZone (robotCount), spec.zone or status.specZoneMatchesCurrent. A spec.zone
// change is included for FZ-G5: a robot re-homed into a zone with no usable geometry is
// placed by that zone's reconcile, and no other event would enqueue it.
// specZoneMatchesCurrent is included because placeDeclaredMembers decides on the cache
// whether a robot is already placed, which reads it: a change to it alone (an edit by
// someone else, or an informer relist that skipped the version between) must enqueue the
// reconcile that corrects it. Other Robot writes are ignored.
var robotZoneChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldR, ok1 := e.ObjectOld.(*fleetv1.Robot)
		newR, ok2 := e.ObjectNew.(*fleetv1.Robot)
		if !ok1 || !ok2 {
			return false
		}
		return oldR.Status.CurrentZone != newR.Status.CurrentZone || oldR.Spec.Zone != newR.Spec.Zone ||
			!equality.Semantic.DeepEqual(oldR.Status.SpecZoneMatchesCurrent, newR.Status.SpecZoneMatchesCurrent)
	},
	CreateFunc:  func(event.CreateEvent) bool { return true },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// SetupWithManager registers the Zone Controller. It reconciles FleetZones, and
// also re-reconciles a zone's parent (its childZones/isLeaf change when a child
// appears or is deleted) and the namespace's zones when a robot's currentZone
// changes (robotCount) or its spec.zone changes (declared membership, FZ-G5).
func (z *ZoneController) SetupWithManager(mgr ctrl.Manager) error {
	parentMapper := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		fz, ok := obj.(*fleetv1.FleetZone)
		if !ok || fz.Spec.ParentZone == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Name: fz.Spec.ParentZone, Namespace: fz.Namespace,
		}}}
	})

	robotToZones := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		robot, ok := obj.(*fleetv1.Robot)
		if !ok {
			return nil
		}
		var zones fleetv1.FleetZoneList
		if err := z.List(ctx, &zones, client.InNamespace(robot.Namespace)); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(zones.Items))
		for i := range zones.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Name: zones.Items[i].Name, Namespace: zones.Items[i].Namespace,
			}})
		}
		return reqs
	})

	// A restriction declared on a zone applies to every DESCENDANT (FZ-R1), so a
	// spec change must reach them. The existing parentMapper walks the other way —
	// child to parent, for childZones/isLeaf — and nothing in this controller
	// propagated downward before restrictions existed, because nothing needed to.
	//
	// FZ-R8 is what makes this mandatory rather than nice: an ancestor tightening
	// onto an existing descendant is ALWAYS admitted, and the descendant is then
	// required to publish RestrictionsConsistent: False. Without downward fan-out
	// that publication never happens, and the contradiction is silent — which is the
	// failure mode FZ-R8 exists to prevent.
	//
	// Gated on GenerationChangedPredicate so this controller's own status writes do
	// not fan out to the whole namespace: generation moves on a spec write only.
	zoneToDescendants := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		fz, ok := obj.(*fleetv1.FleetZone)
		if !ok {
			return nil
		}
		var zones fleetv1.FleetZoneList
		if err := z.List(ctx, &zones, client.InNamespace(fz.Namespace)); err != nil {
			return nil
		}
		childrenOf, _ := buildTree(zones.Items)
		var reqs []reconcile.Request
		seen := map[string]bool{}
		var walk func(string)
		walk = func(name string) {
			if seen[name] {
				return // cycle guard: a malformed tree must not hang the mapper
			}
			seen[name] = true
			for _, c := range childrenOf[name] {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
					Name: c, Namespace: fz.Namespace,
				}})
				walk(c)
			}
		}
		walk(fz.Name)
		// A zone naming the changed zone as its parent is a descendant even when the
		// changed zone is being DELETED and so is absent from the listing above: its
		// chain has just become unresolvable, which is an FZ-R6 transition it must
		// publish. buildTree links only existing parents, so these are added here.
		for i := range zones.Items {
			if zones.Items[i].Spec.ParentZone == fz.Name && !seen[zones.Items[i].Name] {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
					Name: zones.Items[i].Name, Namespace: fz.Namespace,
				}})
			}
		}
		return reqs
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.FleetZone{}).
		Watches(&fleetv1.FleetZone{}, parentMapper).
		Watches(&fleetv1.FleetZone{}, zoneToDescendants,
			builder.WithPredicates(predicate.Or(
				predicate.GenerationChangedPredicate{},
				deletedZonePredicate,
			))).
		Watches(&fleetv1.Robot{}, robotToZones, builder.WithPredicates(robotZoneChanged)).
		Complete(z)
}

// FleetZone restriction condition reasons. Kept as constants because an operator
// filters on them and a controller must not spell one two ways.
const (
	zoneReasonRestrictionsResolved   = "Resolved"
	zoneReasonChainUnresolvable      = "AncestorChainUnresolvable"
	zoneReasonRestrictionsConsistent = "Consistent"
	zoneReasonContradiction          = "RestrictionContradiction"
	// zoneReasonEntryDeniedWithOccupants is occupantAction: Report's ONE observable
	// effect (ADR-0058 §10): a single Warning naming the robots inside a zone at the
	// moment an entry denial takes effect over them.
	zoneReasonEntryDeniedWithOccupants = "ZoneEntryDeniedWithOccupants"
)

// reconcileRestrictions projects the resolved restriction set onto the zone's
// status and publishes RestrictionsResolved / RestrictionsConsistent
// (FZ-R3 … FZ-R6).
//
// It is called from Reconcile with the zone list Reconcile ALREADY fetched — no
// second List, and no per-zone Get walk up the tree.
//
// # FZ-R5, which is RA-1
//
// The fold runs only when the fingerprint of its inputs has changed. Reconcile is
// enqueued by telemetry: a robot crossing a zone boundary writes
// Robot.status.currentZone, and the robotToZones watch re-reconciles every zone in
// the namespace to refresh robotCount. That path MUST NOT drag the resolver along
// with it, and this guard is why it does not.
//
// # FZ-R6, which fails closed
//
// An unresolvable chain publishes RestrictionsResolved: False and the zone is not
// dispatchable until it resolves. What it does NOT do is empty the effective set:
// the rules this zone declares itself are still known, and discarding them would
// make a denied zone read as unrestricted — the one direction that must never
// happen. Fail closed means "say the answer is untrustworthy", not "forget it".
func (z *ZoneController) reconcileRestrictions(ctx context.Context, zoneObj *fleetv1.FleetZone, all []fleetv1.FleetZone) {
	chain, unresolvable := ancestorChain(zoneObj.Name, all)
	fp := restrictionsFingerprint(chain, unresolvable)

	key := zoneObj.Namespace + "/" + zoneObj.Name
	z.restrictMu.Lock()
	if z.restrictFP == nil {
		z.restrictFP = map[string]string{}
	}
	unchanged := z.restrictFP[key] == fp
	z.restrictFP[key] = fp
	z.restrictMu.Unlock()

	if unchanged && zoneObj.Status.Restrictions != nil {
		// Nothing the resolution reads has changed. Leave status.restrictions and both
		// conditions exactly as they were read, so the caller's DeepEqual guard
		// suppresses the write entirely.
		return
	}

	z.restrictRecomputes.Add(1)
	effective, contradictions := resolveZoneRestrictions(chain)
	zoneObj.Status.Restrictions = &fleetv1.ZoneRestrictionsStatus{Effective: effective}

	if unresolvable != "" {
		upsertCondition(&zoneObj.Status.Conditions, fleetv1.ZoneCondRestrictionsResolved,
			metav1.ConditionFalse, zoneReasonChainUnresolvable,
			fmt.Sprintf("ancestor chain does not resolve (%s); restrictions declared above this "+
				"zone cannot be read, so the zone is not dispatchable until it resolves", unresolvable))
	} else {
		upsertCondition(&zoneObj.Status.Conditions, fleetv1.ZoneCondRestrictionsResolved,
			metav1.ConditionTrue, zoneReasonRestrictionsResolved,
			"effective restriction set resolved from this zone and its ancestor chain")
	}

	if len(contradictions) == 0 {
		upsertCondition(&zoneObj.Status.Conditions, fleetv1.ZoneCondRestrictionsConsistent,
			metav1.ConditionTrue, zoneReasonRestrictionsConsistent,
			"no subject is both Denied and Required in this zone chain")
	} else {
		msgs := make([]string, 0, len(contradictions))
		for _, c := range contradictions {
			msgs = append(msgs, c.String())
		}
		upsertCondition(&zoneObj.Status.Conditions, fleetv1.ZoneCondRestrictionsConsistent,
			metav1.ConditionFalse, zoneReasonContradiction, strings.Join(msgs, "; "))
	}

	z.warnOnContradiction(zoneObj, contradictions)
}

// warnOnContradiction emits one RestrictionContradiction Warning per transition
// into the contradictory state (FZ-R3), latched like reconcileEdgeFeed so a resync
// does not re-warn. An event stream that repeats an unchanged fact is one an
// operator stops reading.
func (z *ZoneController) warnOnContradiction(zoneObj *fleetv1.FleetZone, contradictions []zoneRestrictionContradiction) {
	if z.Recorder == nil {
		return
	}
	key := zoneObj.Namespace + "/" + zoneObj.Name
	inContradiction := len(contradictions) > 0

	z.restrictMu.Lock()
	if z.contradictionAlerted == nil {
		z.contradictionAlerted = map[string]bool{}
	}
	already := z.contradictionAlerted[key]
	z.contradictionAlerted[key] = inContradiction
	z.restrictMu.Unlock()

	if !inContradiction || already {
		return
	}
	msgs := make([]string, 0, len(contradictions))
	for _, c := range contradictions {
		msgs = append(msgs, c.String())
	}
	z.Recorder.Event(zoneObj, corev1.EventTypeWarning, zoneReasonContradiction,
		fmt.Sprintf("zone %q: %s. No robot may work under both rules, so the subject is treated "+
			"as Denied; edit one of the two zones named to clear it",
			zoneObj.Name, strings.Join(msgs, "; ")))
}

// ancestorChain returns the zone's chain ROOT-DOWNWARD, including the zone itself,
// following spec.parentZone ONLY.
//
// NEVER spec.memberZones (FZ-R1). Composition is a targeting relation that asserts
// nothing physical; a restriction crossing a composition edge would let a group
// grant itself authority over zones it does not contain. This is the same edge set
// estop propagation and shared-resource inheritance already follow, and the
// asymmetry is deliberate rather than an oversight.
//
// The second return is empty when the chain resolves, and otherwise describes why
// it does not — a parentZone naming a zone that is not in the namespace, or a cycle.
// Both are FZ-R6 cases.
func ancestorChain(name string, all []fleetv1.FleetZone) ([]fleetv1.FleetZone, string) {
	byName := make(map[string]*fleetv1.FleetZone, len(all))
	for i := range all {
		byName[all[i].Name] = &all[i]
	}
	self, ok := byName[name]
	if !ok {
		return nil, fmt.Sprintf("zone %q is not in the namespace listing", name)
	}

	var upward []fleetv1.FleetZone
	seen := map[string]bool{}
	cur := self
	for {
		if seen[cur.Name] {
			return reverseZones(upward), fmt.Sprintf("parentZone chain revisits zone %q", cur.Name)
		}
		seen[cur.Name] = true
		upward = append(upward, *cur)

		parent := cur.Spec.ParentZone
		if parent == "" {
			return reverseZones(upward), ""
		}
		next, ok := byName[parent]
		if !ok {
			return reverseZones(upward), fmt.Sprintf("zone %q names parentZone %q, which does not exist in this namespace",
				cur.Name, parent)
		}
		cur = next
	}
}

func reverseZones(in []fleetv1.FleetZone) []fleetv1.FleetZone {
	out := make([]fleetv1.FleetZone, len(in))
	for i := range in {
		out[len(in)-1-i] = in[i]
	}
	return out
}

// restrictionsFingerprint summarises everything a resolution depends on: which
// zones are in the chain, in order, and which SPEC each of them currently carries.
//
// metadata.generation is the spec version. FleetZone enables the status subresource,
// so generation advances on a spec write and NOT on a status write — which is
// exactly the discrimination FZ-R5 needs, and is why this is keyed on generation
// rather than on resourceVersion. A telemetry-driven reconcile changes neither the
// chain membership nor any generation in it, so the fingerprint holds and the fold
// is skipped.
//
// Using the whole generation rather than hashing spec.restrictions alone means an
// unrelated spec edit (a displayName, say) also recomputes. That is deliberate and
// on the correct side: FZ-R5 REQUIRES recomputation on a spec change, and being
// slightly eager there costs one fold, while being clever risks missing one.
func restrictionsFingerprint(chain []fleetv1.FleetZone, unresolvable string) string {
	var b strings.Builder
	b.WriteString(unresolvable)
	for i := range chain {
		b.WriteString("|")
		b.WriteString(chain[i].Name)
		b.WriteString("@")
		b.WriteString(strconv.FormatInt(chain[i].Generation, 10))
	}
	return b.String()
}

// deletedZonePredicate passes deletes only. GenerationChangedPredicate drops them
// (a delete carries no generation transition), and a deleted zone is exactly when a
// descendant's chain becomes unresolvable and FZ-R6 must be published.
var deletedZonePredicate = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return false },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}
