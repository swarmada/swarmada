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
	"strings"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/telemetry"
	"github.com/swarmada/swarmada/internal/zone"
)

// ZoneController maintains the FleetZone hierarchy topology (isLeaf/childZones/
// robotCount) and derives Robot.status.currentZone from live position telemetry
// via point-in-polygon containment (RFC-0001 §9.3.4, §9.1.5.5).
//
// It is both a FleetZone reconciler and a telemetry.PositionObserver. The
// reconciler rebuilds an in-memory per-namespace geometry cache; the observer
// reads that cache on each position frame and writes currentZone ONLY on a zone
// transition — never per tick (RA-1). Per-robot transition writes are serialised
// by a compare-and-set so concurrent frames never double-write.
type ZoneController struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// TDE receives zone entry/exit signals so a reservation moves Reserved→Occupied
	// on entry and is released on exit (§9.4.2). Optional; nil disables the signal.
	TDE ZoneReservationNotifier
	// Admission pushes zone_admission holds/admits on capacity changes (§9.3.4). Nil
	// disables the push (no behaviour beyond the existing robotCount status).
	Admission ZoneAdmissionNotifier

	zonesMu sync.RWMutex
	zones   map[string][]zone.Candidate // namespace → zone geometries (immutable slices)

	lastMu   sync.Mutex
	lastZone map[string]string // robot_id → last written currentZone

	// ready reports whether the controller has loaded zone geometry at least once,
	// so currentZone-based TDE recovery validation can be trusted (§9.4.7).
	ready atomic.Bool

	admitMu sync.Mutex
	held    map[string]bool // "namespace/robot" → currently held at its leaf zone

	edgeMu      sync.Mutex
	edgeAlerted map[string]bool // "namespace/zone" → EdgeFeedUnavailable warning is outstanding
}

// ZoneAdmissionNotifier pushes a zone_admission hold/admit Command to a robot at a
// leaf-zone boundary (§9.3.4). The command Dispatcher satisfies it structurally.
type ZoneAdmissionNotifier interface {
	NotifyZoneAdmission(ctx context.Context, namespace, robotID, zoneName string, admit bool)
}

// Ready reports whether the Zone Controller has loaded zone geometry (its position
// derivation is live). The TDE recovery runnable waits on it before validating
// Occupied reservations against Robot.status.currentZone (§9.4.7).
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
	z.setZones(req.Namespace, candidatesFrom(list.Items, childrenOf, depthOf))

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
	computeZoneConditions(zoneObj)

	if !equality.Semantic.DeepEqual(original.Status, zoneObj.Status) {
		if err := z.Status().Patch(ctx, zoneObj, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, fmt.Errorf("patching zone status: %w", err)
		}
		logger.V(1).Info("zone topology updated", "isLeaf", zoneObj.Status.IsLeaf,
			"childZones", children, "robotCount", robotCount)
	}

	z.reconcileEdgeFeed(zoneObj)
	return ctrl.Result{}, nil
}

// FleetZone condition types (ADR-0023).
const (
	zoneCondReady             = "ZoneReady"
	zoneCondCapacityAvailable = "CapacityAvailable"
)

// computeZoneConditions upserts the standard FleetZone conditions ZoneReady and
// CapacityAvailable (ADR-0023), derived from spec plus the already-computed status.
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
// whose adapter never established its EdgeStream, §9.2.10). The edge writes the
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
	robot, err := findRobotByID(ctx, z.Client, f.RobotID)
	if err != nil {
		log.FromContext(ctx).Error(err, "resolving robot for zone derivation", "robotID", f.RobotID)
		return
	}
	if robot == nil {
		return // robot_id not mapped to a Robot yet; drop, don't fail
	}

	z.zonesMu.RLock()
	candidates := z.zones[robot.Namespace]
	z.zonesMu.RUnlock()

	newZone := zone.DeriveCurrentZone(zone.Position{
		X:     f.Position.X,
		Y:     f.Position.Y,
		Floor: f.Position.Floor,
	}, candidates)

	if !z.recordZone(f.RobotID, newZone, robot.Status.CurrentZone) {
		return // unchanged since the last write (or persisted status) — RA-1
	}

	oldZone := robot.Status.CurrentZone
	original := robot.DeepCopy()
	robot.Status.CurrentZone = newZone
	matches := newZone != "" && newZone == robot.Spec.Zone
	robot.Status.SpecZoneMatchesCurrent = &matches
	if err := z.Status().Patch(ctx, robot, client.MergeFrom(original)); err != nil {
		log.FromContext(ctx).Error(err, "writing currentZone", "robot", robot.Name, "zone", newZone)
		return
	}

	// Signal the reservation lifecycle off this same currentZone transition: the
	// robot left oldZone and entered newZone (§9.4.2). OnRobotEnteredZone moves a
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

	// Zone-capacity hold/admit (§9.3.4): the same currentZone transition may push a
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

	if newZone == "" && z.Recorder != nil {
		z.Recorder.Event(robot, corev1.EventTypeWarning, "ZonePositionUnmatched",
			"robot position matched no zone polygon; currentZone cleared")
	}
}

// recordZone compare-and-sets the last-derived zone for a robot under a lock, so
// concurrent frames for the same robot never both write a transition. On first
// sight it seeds from the persisted status, so a post-restart unchanged frame
// produces no write (§9.3.4 crash-restart behaviour). Returns true iff a write is
// warranted.
func (z *ZoneController) recordZone(robotID, newZone, statusZone string) bool {
	z.lastMu.Lock()
	defer z.lastMu.Unlock()
	if z.lastZone == nil {
		z.lastZone = make(map[string]string)
	}
	prev, seen := z.lastZone[robotID]
	if !seen {
		prev = statusZone // seed from persisted status
	}
	if prev == newZone {
		z.lastZone[robotID] = newZone
		return false
	}
	z.lastZone[robotID] = newZone
	return true
}

func (z *ZoneController) setZones(namespace string, cands []zone.Candidate) {
	z.zonesMu.Lock()
	defer z.zonesMu.Unlock()
	if z.zones == nil {
		z.zones = make(map[string][]zone.Candidate)
	}
	z.zones[namespace] = cands // fresh slice; never mutated in place
	z.ready.Store(true)        // geometry loaded → the controller is deriving zones
}

// reconcileZoneAdmission recomputes a leaf zone's admission set after robotID
// entered (or left) it, pushing zone_admission only for robots whose hold state
// changed (§9.3.4). maxConcurrentRobots==0 is unlimited (no holds). Robots are
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

// candidatesFrom builds the derivation geometries for every zone that declares a
// polygon.
func candidatesFrom(
	zones []fleetv1.FleetZone, childrenOf map[string][]string, depthOf map[string]int,
) []zone.Candidate {
	out := make([]zone.Candidate, 0, len(zones))
	for i := range zones {
		z := &zones[i]
		if z.Spec.PhysicalBounds == nil {
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
		})
	}
	return out
}

// SetupWithManager registers the Zone Controller. It reconciles FleetZones, and
// also re-reconciles a zone's parent (its childZones/isLeaf change when a child
// appears or is deleted) and the namespace's zones when a robot's currentZone
// changes (robotCount).
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

	// Only a currentZone change affects robotCount — ignore other Robot writes.
	currentZoneChanged := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldR, ok1 := e.ObjectOld.(*fleetv1.Robot)
			newR, ok2 := e.ObjectNew.(*fleetv1.Robot)
			if !ok1 || !ok2 {
				return false
			}
			return oldR.Status.CurrentZone != newR.Status.CurrentZone
		},
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.FleetZone{}).
		Watches(&fleetv1.FleetZone{}, parentMapper).
		Watches(&fleetv1.Robot{}, robotToZones, builder.WithPredicates(currentZoneChanged)).
		Complete(z)
}

// findRobotByID resolves a wire robot_id to its Robot via the swarmada.io/robot-id
// annotation. robot_id is globally unique (§9.2.3), so the lookup is cluster-wide
// and expects exactly one match: zero → nil (unmapped), more than one → an error
// (a spec violation; refuse to guess). The List is served from the cache.
func findRobotByID(ctx context.Context, c client.Client, robotID string) (*fleetv1.Robot, error) {
	if robotID == "" {
		return nil, nil
	}
	var list fleetv1.RobotList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing robots to resolve robot_id %q: %w", robotID, err)
	}
	var match *fleetv1.Robot
	for i := range list.Items {
		if list.Items[i].Annotations[RobotIDAnnotation] == robotID {
			if match != nil {
				return nil, fmt.Errorf("robot_id %q maps to more than one Robot", robotID)
			}
			match = &list.Items[i]
		}
	}
	return match, nil
}
