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
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/audit"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/metrics"
	"github.com/swarmada/swarmada/internal/safety"
)

const (
	// annEstopTriggered fires a zone-wide estop (set by the `estop-trigger` verb,
	// RFC-0001 #security-authorization-swarmada-rbac-roles). Its value is the estop reason/id — a NEW value re-fires; the same
	// value is idempotent.
	annEstopTriggered = "swarmada.io/estop-triggered"
	// annEstopProcessed records the last estop-trigger value the controller acted
	// on, so a re-reconcile does not re-fan-out the same estop.
	annEstopProcessed = "swarmada.io/estop-processed"
	// zoneEstopWalkCap bounds the parentZone ancestor walk: a zone and zoneEstopWalkCap
	// ancestors, the farthest the stop guard reaches (estopscope.MaxAncestors).
	zoneEstopWalkCap = estopscope.MaxAncestors
	// stopInForceResync is how often a stop in force re-evaluates the robots it covers when no
	// event has woken it: a robot a missed event left unstopped is stopped on the next pass.
	stopInForceResync = 30 * time.Second
)

// ZoneEstopper is the confirmed per-robot estop primitive the zone fan-out reuses.
// [github.com/swarmada/swarmada/internal/safety.Dispatcher] satisfies it: it pushes
// the estop over the robot's SafetyStream and marks Stopped only on a CONFIRMED
// EstopAck — a robot it cannot confirm resolves to Failed (escalate), never Stopped.
type ZoneEstopper interface {
	// scope names the OPERATOR ACTION that produced this stop (robot / zone / namespace),
	// not the object being written. A zone or namespace estop fans out per robot through
	// this same primitive, so nothing below this call can distinguish the three — the
	// RFC-0001 #control-plane-observability-prometheus-metrics-contract `scope` label has to be carried in from the reconciler that fanned out.
	// carrier names the object whose stop it is (the robot, a FleetZone or the namespace's
	// SwarmadaConfig), which the stop's record on the robot names; the zero Carrier on a
	// robot-scope stop is the robot.
	TriggerEstop(ctx context.Context, namespace, robotID, reason, issuedBy string,
		scope metrics.EstopScope, carrier safety.Carrier) (safety.Result, error)
	// ClearEstop moves a robot's estop to Resuming on an operator-authorized clear
	// (RFC-0001 #safety-estop-state-machine, row S5); only an operator's later release returns it
	// to Normal. No-op when the robot is not estopped; the action stays operator-gated. ends is
	// what the clear ends on the robot beside its own stop: the records it drops and the recorded
	// zone and namespace stops that have ended (clearEnds).
	ClearEstop(ctx context.Context, namespace, robotID, clearedBy string, ends safety.Ends) (fleetv1.RobotEstopState, error)
}

// ZoneEstopReconciler drives zone-level emergency stops with hierarchical
// propagation (RFC-0001 #safety-estop-scopes): on an estop-trigger it confirmed-estops every
// robot in the zone — and, when estopPolicy.propagateToChildren, every robot in a
// descendant zone — and notifies the parent (propagateToParent) with a
// ChildEstopTriggered event, WITHOUT auto-estopping the parent.
type ZoneEstopReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Estopper issues the confirmed per-robot estop. Nil disables zone estop.
	Estopper ZoneEstopper
	// Recorder emits the ChildEstopTriggered event on the parent. Nil skips it.
	Recorder record.EventRecorder
	// Audit records the ESTOP_TRIGGERED safety event. Nil skips it.
	Audit audit.Recorder
	// APIReader reads the API server directly, bypassing the manager's cache. The hold
	// records are read and written through it, and the clear lists the robots through it,
	// so a record written moments before is not missed. Nil falls back to Client.
	APIReader client.Reader
	// markers is the processed marker this process last wrote or removed on each zone.
	markers writtenMarkers
}

// reader is the uncached reader when one is wired, else the client.
func (r *ZoneEstopReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=swarmada.io,resources=fleetzones,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=swarmada.io,resources=robots,verbs=get;list;watch

// Reconcile fans out a zone estop when the trigger annotation is (newly) present.
func (r *ZoneEstopReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	zone := &fleetv1.FleetZone{}
	if err := r.Get(ctx, req.NamespacedName, zone); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return r.reconcileZone(ctx, req, zone)
}

// reconcileZone is the pass on zone as it was read: from the cache, or from the API server when a
// clear finds a trigger written while it ran.
func (r *ZoneEstopReconciler) reconcileZone(ctx context.Context, req ctrl.Request, zone *fleetv1.FleetZone) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("fleetzone", req.NamespacedName)

	if r.Estopper == nil {
		return ctrl.Result{}, nil
	}
	trigger, firing := zone.Annotations[annEstopTriggered]
	processed, wasProcessed := zone.Annotations[annEstopProcessed]
	if r.markers.behind(req.NamespacedName, trigger, firing, processed, wasProcessed) {
		// The cache does not show this process's last marker write: the marker it wrote for this
		// trigger, or the removal of the marker of a trigger it cleared. The API server says
		// which is right. A failed read leaves the cached copy: the stop, or the clear, goes.
		fresh := &fleetv1.FleetZone{}
		if err := r.reader().Get(ctx, req.NamespacedName, fresh); err == nil {
			zone = fresh
			trigger, firing = zone.Annotations[annEstopTriggered]
			processed, wasProcessed = zone.Annotations[annEstopProcessed]
		}
	}

	// The stop finalizer: held from the trigger until the clear has been applied, so a zone
	// deleted while no replica serves the delete check stays until the stop is applied and
	// cleared (estopscope.StopFinalizer). A namespace being torn down releases it.
	if done, err := r.holdCarrier(ctx, zone, firing || wasProcessed); done || err != nil {
		return ctrl.Result{}, err
	}

	switch {
	case firing && wasProcessed && processed == trigger:
		// This exact estop already fanned out (the marker's presence is compared as well as
		// its value: an absent marker reads as "", and an empty trigger is a stop like any
		// other). Every robot it has not reached is stopped now (holdLateArrivals): one that
		// came into its scope since, and one that records it but whose stop state was never
		// written. The status projection still runs: it is how a projection that failed
		// after the fan-out is retried, how a zone whose tree changed under it is brought
		// up to date, and how a status another identity wrote over is written back.
		if err := r.holdLateArrivals(ctx, req.Namespace, zone, trigger); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: stopInForceResync}, r.projectEstopStatus(ctx, req.Namespace, zone, nil, nil)
	case !firing && wasProcessed:
		// The estop-trigger annotation was removed (the operator-authorized
		// estop-clear verb): resume the robots and drop the processed marker.
		r.markers.forget(req.NamespacedName)
		return r.clearZoneEstop(ctx, req, zone)
	case !firing:
		return ctrl.Result{}, r.projectEstopStatus(ctx, req.Namespace, zone, nil, nil) // no estop active here
	}

	toChildren, toParent := true, false // estop-vs-preemption documented defaults
	if p := zone.Spec.EstopPolicy; p != nil {
		toChildren, toParent = p.PropagatesToChildren(), p.PropagateToParent
	}
	reason := estopReason(trigger, "zone emergency stop")
	// The operator the mutating webhook stamped at admission (ADR-0046); falls back to
	// an explicitly unattributed actor when none was resolvable — never to a synthetic
	// name that reads like a real one.
	actor := estopActor(zone.Annotations, "zone-estop:"+zone.Name)
	issuedBy := actor.Identity

	// ── Fan out to every robot in scope (confirmed estop each) ─────────────────
	robots, err := r.robotsInEstopScope(ctx, req.Namespace, zone.Name, toChildren)
	if err != nil {
		return ctrl.Result{}, err
	}
	stopped, failed, unwritten := 0, 0, 0
	// Collected for the audit entry: WHICH robots were in scope (RFC-0001 #safety-required-events
	// robots_in_scope) and the worst ack latency across the fan-out. A per-robot
	// rpc_sent_at/ack_received_at pair cannot collapse into one entry for a fan-out,
	// so the robot-scope entries carry those; here the worst latency is the number
	// that matters, because the RFC-0001 #safety-timing-requirements SLA is breached if ANY robot exceeds it.
	inScope := make([]string, 0, len(robots))
	var worstLatency time.Duration
	// Issued in PARALLEL (RFC-0001 #safety-estop-scopes): one goroutine per robot, so a slow acknowledgement
	// from one robot never delays the estop signal to the others. estopFanout also returns
	// the episode's wall-clock duration — trigger to last robot resolved — which is the
	// interval the per-robot latency histogram structurally cannot see (ADR-0042).
	ref := zoneHold(zone)
	outcomes, fanout := estopFanoutHeld(ctx, r.Estopper, req.Namespace, robots, reason, issuedBy,
		metrics.ScopeZone, carrierOf(ref), func(ctx context.Context, robot string) error {
			return recordHoldAndWithdraw(ctx, r.Client, r.reader(), req.Namespace, robot, ref)
		})
	// Aggregated after the join, in robot order: the counts, the worst latency and the audit
	// entry must not depend on which goroutine happened to finish first.
	for _, o := range outcomes {
		inScope = append(inScope, o.robot)
		if o.result.Delivered && o.result.Latency > worstLatency {
			worstLatency = o.result.Latency
		}
		if outcomeUnwritten(o) {
			unwritten++
			logger.Info("zone estop outcome or hold record not written for robot (retrying)", "robot", o.robot,
				"state", o.result.State, "err", errString(o.err), "recordErr", errString(o.recErr))
		}
		if o.err != nil || o.result.State != fleetv1.RobotEstopStopped {
			failed++
			logger.Info("zone estop not confirmed for robot (escalate)", "robot", o.robot,
				"state", o.result.State, "err", errString(o.err))
			continue
		}
		stopped++
	}
	// Observed for every episode that fans out, including one that stopped nothing: a zone
	// whose robots all failed to confirm is exactly the episode an operator needs timed.
	metrics.ObserveEstopFanout(req.Namespace, metrics.ScopeZone, fanout)
	logger.Info("zone estop fanned out", "zone", zone.Name, "reason", reason,
		"robots", len(robots), "stopped", stopped, "unconfirmed", failed, "fanout", fanout)

	if r.Audit != nil {
		outcome := audit.OutcomeAllowed
		if failed > 0 {
			outcome = audit.OutcomeError // some robots could not be confirmed stopped
		}
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: req.Namespace,
			EventType: audit.EventEstopTriggered,
			Action:    "estop-trigger",
			Outcome:   outcome,
			Actor:     actor,
			Resource:  audit.Resource{Kind: "FleetZone", Namespace: req.Namespace, Name: zone.Name},
			Detail: map[string]string{
				"reason": reason,
				// Comma-separated with no spaces — the list encoding a flat
				// map[string]string forces (RFC-0001 #safety-log-entry-format). Readers split on ",".
				"robots_in_scope":    strings.Join(inScope, ","),
				"robots":             itoa(len(robots)),
				"stopped":            itoa(stopped),
				"unconfirmed":        itoa(failed),
				"unwritten":          itoa(unwritten),
				"max_ack_latency_ms": itoa(int(worstLatency.Milliseconds())),
			},
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_TRIGGERED audit entry")
		}
	}

	// A robot whose outcome or hold record was not written is not recorded as reached.
	// Nothing is marked done: the reconcile returns before the status stamp and the
	// marker, and the requeue fans out again (RFC-0001 #safety-estop-state-machine).
	if unwritten > 0 {
		return ctrl.Result{}, fmt.Errorf("zone %s estop: the outcome or hold record of %d robot(s) was not written; "+
			"fanning out again", zone.Name, unwritten)
	}

	// ── Notify the parent (no auto-estop) ──────────────────────────────────────
	if toParent && zone.Spec.ParentZone != "" && r.Recorder != nil {
		parent := &fleetv1.FleetZone{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: zone.Spec.ParentZone}, parent); err == nil {
			r.Recorder.Event(parent, corev1.EventTypeWarning, "ChildEstopTriggered",
				fmt.Sprintf("child zone %s triggered an emergency stop (%s); this zone is NOT auto-estopped", zone.Name, reason))
		}
	}

	// Record on the zone itself that its estop has fanned out, BEFORE the processed marker,
	// so the zone reads Triggered from the moment the marker stands. A failure here returns
	// before the marker, so the fan-out is retried.
	sbase := zone.DeepCopy()
	stampAt := metav1.Now().Rfc3339Copy()
	zone.Status.EstopStatus = fleetv1.ZoneEstopTriggered
	zone.Status.LastEstopAt = &stampAt
	if err := r.Status().Patch(ctx, zone, client.MergeFrom(sbase)); err != nil {
		return ctrl.Result{}, fmt.Errorf("writing zone estop status: %w", err)
	}

	// Mark this trigger processed so it is not re-fanned-out.
	base := zone.DeepCopy()
	if zone.Annotations == nil {
		zone.Annotations = map[string]string{}
	}
	zone.Annotations[annEstopProcessed] = trigger
	consumeEstopActor(zone.Annotations)
	if err := r.Patch(ctx, zone, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("recording estop processed: %w", err)
	}
	r.markers.wrote(req.NamespacedName, trigger)

	// Reflect the active estop in the status of every zone it reaches (RFC-0001
	// #fleetzone-schema-root-zone).
	return ctrl.Result{}, r.projectEstopStatus(ctx, req.Namespace, zone, zone, &stampAt)
}

// clearZoneEstop clears the zone's robots after an operator clears the estop (the trigger
// annotation was removed). It reaches every robot the stop holds: each robot that records
// this zone's stop (status.estopHeldBy), wherever it is now, and each robot the tree puts in
// the zone's scope now. Each is moved to Resuming, where it waits for an operator's release
// (idempotent — a robot not under an estop is a no-op); a action Paused by the estop stays
// operator-gated (RFC-0001 #safety-fleetaction-behaviour-during-estop). A robot another stop
// still holds is not cleared: its own robot-scope trigger, another zone's stop, or the
// namespace's stop. A clear of one scope is not the clear of another (RFC-0001
// #safety-estop-scopes); the clear of that stop releases it. Each robot's record of this
// zone's stop is removed. Only once every write has landed does it drop the processed marker,
// so a fresh trigger fires cleanly; a failed write leaves the marker and the clear is retried.
// A trigger written while the clear ran is kept, and the pass runs again on it as a new stop
// (dropClearedMarker).
func (r *ZoneEstopReconciler) clearZoneEstop(ctx context.Context, req ctrl.Request, zone *fleetv1.FleetZone) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	namespace := req.Namespace
	toChildren := true // scope must match the trigger's default
	if p := zone.Spec.EstopPolicy; p != nil {
		toChildren = p.PropagatesToChildren()
	}
	actor := estopActor(zone.Annotations, "zone-estop:"+zone.Name)
	clearedBy := actor.Identity

	scoped, err := r.robotsInEstopScope(ctx, namespace, zone.Name, toChildren)
	if err != nil {
		return ctrl.Result{}, err
	}
	var all fleetv1.RobotList
	if err := r.reader().List(ctx, &all, client.InNamespace(namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing robots: %w", err)
	}
	ref := zoneHold(zone)
	robots := clearTargets(ref, scoped, all.Items)
	zones, configs, err := readStandingStops(ctx, r.reader(), namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	cleared, held, cerr := clearCarrier(ctx, r.Client, r.reader(), r.Estopper, r.Recorder, namespace, ref,
		robots, zones, configs, clearedBy, "zone "+zone.Name)
	logger.Info("zone estop cleared", "zone", zone.Name, "robots", len(robots),
		"heldByOwnEstop", held.own, "heldByWiderEstop", held.wider, "err", errString(cerr))

	if r.Audit != nil {
		outcome := audit.OutcomeAllowed
		if cerr != nil {
			outcome = audit.OutcomeError
		}
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: namespace,
			EventType: audit.EventEstopCleared,
			Action:    "estop-clear",
			Outcome:   outcome,
			Actor:     actor,
			Resource:  audit.Resource{Kind: "FleetZone", Namespace: namespace, Name: zone.Name},
			Detail:    held.detail(map[string]string{"robots": itoa(len(robots)), "cleared": itoa(cleared)}),
		}); aerr != nil {
			logger.Error(aerr, "recording ESTOP_CLEARED audit entry")
		}
	}
	if cerr != nil {
		return ctrl.Result{}, fmt.Errorf("zone %s estop clear: %w", zone.Name, cerr)
	}

	removedMarker := zone.Annotations[annEstopProcessed]
	zone, again, err := dropClearedMarker(ctx, r.Client, r.reader(), zone,
		func(*fleetv1.FleetZone) (bool, error) { return true, nil })
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("clearing estop processed marker: %w", err)
	}
	r.markers.removed(client.ObjectKeyFromObject(zone), removedMarker)
	if again {
		// A trigger written while this clear ran: its stop is issued now.
		return r.reconcileZone(ctx, req, zone)
	}

	// Re-derive the tree: this zone, the descendants its estop reached and the parent it
	// notified return to Clear unless another estop still reaches them.
	return ctrl.Result{}, r.projectEstopStatus(ctx, namespace, zone, nil, nil)
}

// projectEstopStatus derives status.estopStatus and status.lastEstopAt for every zone in
// the namespace from the estops in force, and writes a zone only when its pair changed
// (RA-1). An estop is in force on a zone from its fan-out until its clear: while the zone
// carries the processed marker (estopInForce). No status field decides it, so a status
// another identity writes over is written back on the reconcile that write triggers.
// A zone reads:
//
//   - Triggered when its own estop is in force, or an ancestor's is and that ancestor
//     propagates to its children — the robots it holds are stopped either way;
//   - ChildTriggered when it is not Triggered and a child's own estop is in force with
//     propagateToParent — the upward notification, which stops nothing;
//   - Clear otherwise.
//
// lastEstopAt is empty while Clear. It is stamped when a trigger reaches the zone (the
// triggered zone, the descendants it stops, the parent it notifies) and is otherwise kept.
//
// current is this pass's in-memory copy of the reconciled zone, which may be newer than
// the cache; triggered is the zone whose estop this pass fanned out, or nil, and stampAt
// the time recorded on it.
func (r *ZoneEstopReconciler) projectEstopStatus(ctx context.Context, namespace string, current, triggered *fleetv1.FleetZone, stampAt *metav1.Time) error {
	var list fleetv1.FleetZoneList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing zones for the estop status: %w", err)
	}
	zones := make(map[string]*fleetv1.FleetZone, len(list.Items)+1)
	names := make([]string, 0, len(list.Items)+1)
	for i := range list.Items {
		zones[list.Items[i].Name] = &list.Items[i]
		names = append(names, list.Items[i].Name)
	}
	if current != nil {
		if _, listed := zones[current.Name]; !listed {
			names = append(names, current.Name)
		}
		zones[current.Name] = current
	}
	sort.Strings(names)

	now := metav1.Now().Rfc3339Copy()
	if stampAt != nil {
		now = *stampAt
	}
	for _, name := range names {
		fz := zones[name]
		want := derivedEstopStatus(zones, fz)
		stamp := fz.Status.LastEstopAt
		switch {
		case want == fleetv1.ZoneEstopClear:
			stamp = nil
		case stamp == nil || (triggered != nil && estopReaches(zones, triggered, fz)):
			stamp = &now
		}
		have := fz.Status.EstopStatus
		if have == "" {
			have = fleetv1.ZoneEstopClear // empty is read as Clear, so it is not rewritten as Clear
		}
		if have == want && sameTime(stamp, fz.Status.LastEstopAt) {
			continue
		}
		// The write carries the resourceVersion it was derived from. The list is the cache,
		// and a copy taken between a fan-out's status write and its marker reads as not in
		// force: written over the zone, it cleared the status and the stamp, and the zone
		// never read Triggered again while its stop stayed in force. A stale write conflicts
		// instead, and the reconcile is retried on a fresher read.
		base := fz.DeepCopy()
		fz.Status.EstopStatus = want
		fz.Status.LastEstopAt = stamp
		if err := r.Status().Patch(ctx, fz, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("writing estop status of zone %s: %w", name, err)
		}
	}
	return nil
}

// estopInForce reports whether a zone's own estop has fanned out and not been cleared: the
// processed marker, which only the manager writes (the marker guard refuses it to every
// other identity, on create and on update). The marker's presence is what counts: an empty
// trigger records an empty marker. It reads no status field: FleetZone status is writable by
// another identity, and a stop's hold must not depend on it.
func estopInForce(fz *fleetv1.FleetZone) bool {
	_, marked := fz.Annotations[annEstopProcessed]
	return marked
}

func sameTime(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}

// derivedEstopStatus is a zone's estop status as projectEstopStatus defines it.
func derivedEstopStatus(zones map[string]*fleetv1.FleetZone, fz *fleetv1.FleetZone) fleetv1.ZoneEstopStatus {
	if estopInForce(fz) {
		return fleetv1.ZoneEstopTriggered
	}
	name := fz.Spec.ParentZone
	for depth := 0; name != "" && depth < zoneEstopWalkCap; depth++ {
		anc, ok := zones[name]
		if !ok {
			break
		}
		if estopInForce(anc) && propagatesDown(anc) {
			return fleetv1.ZoneEstopTriggered
		}
		name = anc.Spec.ParentZone
	}
	for _, child := range zones {
		if child.Spec.ParentZone == fz.Name && child.Name != fz.Name && estopInForce(child) &&
			child.Spec.EstopPolicy != nil && child.Spec.EstopPolicy.PropagateToParent {
			return fleetv1.ZoneEstopChildTriggered
		}
	}
	return fleetv1.ZoneEstopClear
}

// estopReaches reports whether the estop on triggered reaches fz: fz is the zone itself,
// a descendant it propagates to, or the parent it notifies.
func estopReaches(zones map[string]*fleetv1.FleetZone, triggered, fz *fleetv1.FleetZone) bool {
	if fz.Name == triggered.Name {
		return true
	}
	if triggered.Spec.EstopPolicy != nil && triggered.Spec.EstopPolicy.PropagateToParent &&
		fz.Name == triggered.Spec.ParentZone {
		return true
	}
	if !propagatesDown(triggered) {
		return false
	}
	name := fz.Spec.ParentZone
	for depth := 0; name != "" && depth < zoneEstopWalkCap; depth++ {
		if name == triggered.Name {
			return true
		}
		anc, ok := zones[name]
		if !ok {
			return false
		}
		name = anc.Spec.ParentZone
	}
	return false
}

// propagatesDown is estopPolicy.propagateToChildren with its default (true) applied.
func propagatesDown(fz *fleetv1.FleetZone) bool {
	return fz.Spec.EstopPolicy == nil || fz.Spec.EstopPolicy.PropagatesToChildren()
}

// robotsInEstopScope lists the robots a zone estop targets: every robot in the
// zone, plus — when toChildren — every robot in a descendant zone, and every robot whose
// zone chain cannot be walked (estopscope FanOutScope: a stop that cannot be fanned out
// completely stops more robots, never fewer). Each of those is named in a Warning Event.
func (r *ZoneEstopReconciler) robotsInEstopScope(ctx context.Context, namespace, zoneName string, toChildren bool) ([]fleetv1.Robot, error) {
	// Read from the API server, not the cache: a robot admitted or moved a moment ago is in
	// the stop's scope now (the stop guard, estopscope). A read error fails the pass, which is
	// retried; it never reads as "not covered".
	var all fleetv1.RobotList
	if err := r.reader().List(ctx, &all, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing robots: %w", err)
	}
	tree, err := estopscope.ReadTree(ctx, r.reader(), namespace)
	if err != nil {
		return nil, fmt.Errorf("zone %s estop: %w", zoneName, err)
	}
	zones := make([]string, len(all.Items))
	for i := range all.Items {
		zones[i] = all.Items[i].Status.CurrentZone
		if zones[i] == "" {
			zones[i] = all.Items[i].Spec.Zone
		}
	}
	in, undecidedAt, err := tree.FanOutScope(zoneName, zones, toChildren)
	if err != nil {
		return nil, fmt.Errorf("zone %s estop: %w", zoneName, err)
	}
	out := make([]fleetv1.Robot, 0, len(all.Items))
	for i := range all.Items {
		if in[i] {
			out = append(out, all.Items[i])
		}
	}
	var undecided []string
	for _, i := range undecidedAt {
		undecided = append(undecided, fmt.Sprintf("%s (zone %s)", all.Items[i].Name, zones[i]))
	}
	if len(undecided) > 0 {
		log.FromContext(ctx).Info("zone estop: robots whose zone chain cannot be walked are stopped with it",
			"zone", zoneName, "robots", undecided)
		if r.Recorder != nil {
			z := &fleetv1.FleetZone{}
			if err := r.reader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: zoneName}, z); err == nil {
				r.Recorder.Eventf(z, corev1.EventTypeWarning, "EstopScopeUndecided",
					"the stop of zone %s cannot decide whether it reaches %d robot(s), so it stops them: %s",
					zoneName, len(undecided), strings.Join(undecided, "; "))
			}
		}
	}
	return out, nil
}

// holdCarrier holds the stop finalizer on a carrier while it carries a stop (hold), and
// releases it when it does not, or when the namespace is being torn down. It reports done
// when the caller should return: the carrier is going with its namespace.
func (r *ZoneEstopReconciler) holdCarrier(ctx context.Context, zone *fleetv1.FleetZone, hold bool) (bool, error) {
	return holdStopFinalizer(ctx, r.Client, r.reader(), zone, hold)
}

// holdStopFinalizer is holdCarrier for any carrier.
func holdStopFinalizer(ctx context.Context, c client.Client, reader client.Reader, obj client.Object, hold bool) (bool, error) {
	has := controllerutil.ContainsFinalizer(obj, estopscope.StopFinalizer)
	if obj.GetDeletionTimestamp() != nil && has {
		terminating, err := namespaceBeingDeleted(ctx, reader, obj.GetNamespace())
		if err != nil {
			return false, err
		}
		if terminating {
			base := obj.DeepCopyObject().(client.Object)
			controllerutil.RemoveFinalizer(obj, estopscope.StopFinalizer)
			if err := c.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
				return false, fmt.Errorf("releasing the stop finalizer of %s in a namespace being deleted: %w", obj.GetName(), err)
			}
			return true, nil
		}
	}
	switch {
	case hold && !has:
		base := obj.DeepCopyObject().(client.Object)
		controllerutil.AddFinalizer(obj, estopscope.StopFinalizer)
		if err := c.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
			return false, fmt.Errorf("holding the stop finalizer on %s: %w", obj.GetName(), err)
		}
	case !hold && has:
		base := obj.DeepCopyObject().(client.Object)
		controllerutil.RemoveFinalizer(obj, estopscope.StopFinalizer)
		if err := c.Patch(ctx, obj, client.MergeFrom(base)); err != nil {
			return false, fmt.Errorf("releasing the stop finalizer of %s: %w", obj.GetName(), err)
		}
	}
	return false, nil
}

// namespaceBeingDeleted reports whether the namespace carries a deletion timestamp.
func namespaceBeingDeleted(ctx context.Context, reader client.Reader, namespace string) (bool, error) {
	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("reading namespace %q: %w", namespace, err)
	}
	return ns.DeletionTimestamp != nil, nil
}

// lateArrivalActor is who the audit entry for a late arrival's stop names. The operator who
// triggered the zone's estop did not ask for this robot by name, and the stamp they left
// was consumed with the fan-out, so the entry names the controller that issued it.
const lateArrivalActor = "zoneestop-controller"

// holdLateArrivals brings every robot under a zone estop that is in force that the stop has
// not reached (inForceTargets): a robot created into the zone after the fan-out, moved into
// it, or put under it by a change of the zone tree, and a robot that records the stop but
// whose stop state was never written (the write failed), wherever it is now. Its robots in
// scope are found as the fan-out finds them (robotsInEstopScope), so the stop reaches the
// same robots it would have reached had they been there first. A robot already in a stop
// state is only recorded, which is also how a robot stopped before the record existed gets
// one on the first reconcile of a stop in force.
//
// Each stop goes through the confirmed per-robot primitive, as the fan-out does: Stopped on a
// confirmed acknowledgement, Failed when it cannot be confirmed, never Normal. Both hold the
// robot, and both leave only through the zone's clear and an operator's confirmed resume,
// never on their own (RFC-0001 #safety-estop-state-machine); nothing here writes a stop state
// itself. Nothing is remembered between passes but the robot's own record, so a restart, a
// re-entry or a recreated zone changes nothing: a robot is tried until its stop state and its
// record are both written, and any failed write is returned so the pass is retried.
func (r *ZoneEstopReconciler) holdLateArrivals(ctx context.Context, namespace string, zone *fleetv1.FleetZone, trigger string) error {
	ref := zoneHold(zone)
	sent := sentUnrecorded(r.Estopper, namespace, ref)
	scoped, err := r.robotsInEstopScope(ctx, namespace, zone.Name, propagatesDown(zone))
	if err != nil {
		return err
	}
	var all fleetv1.RobotList
	if err := r.reader().List(ctx, &all, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing robots: %w", err)
	}
	late, recordOnly := inForceTargets(ref, scoped, all.Items, sent)
	var errs []error
	for i := range recordOnly {
		if err := recordHoldAndWithdraw(ctx, r.Client, r.reader(), namespace, recordOnly[i].Name, ref); err != nil {
			errs = append(errs, err)
		}
	}
	if len(late) == 0 {
		return errors.Join(errs...)
	}
	reason := fmt.Sprintf("%s (zone %s; the robot arrived after the stop)",
		estopReason(trigger, "zone emergency stop"), zone.Name)
	outcomes, fanout := estopFanoutHeld(ctx, r.Estopper, namespace, late, reason, lateArrivalActor, metrics.ScopeZone,
		carrierOf(ref), func(ctx context.Context, robot string) error {
			return recordHoldAndWithdraw(ctx, r.Client, r.reader(), namespace, robot, ref)
		})
	names := make([]string, 0, len(outcomes))
	stopped, failed, unwritten := 0, 0, 0
	for _, o := range outcomes {
		names = append(names, o.robot)
		if outcomeUnwritten(o) {
			unwritten++
		}
		if o.err != nil || o.result.State != fleetv1.RobotEstopStopped {
			failed++
			continue
		}
		stopped++
	}
	log.FromContext(ctx).Info("zone estop in force: stopped robots that arrived after it", "zone", zone.Name,
		"robots", names, "stopped", stopped, "unconfirmed", failed, "unwritten", unwritten, "fanout", fanout)
	if r.Audit != nil {
		outcome := audit.OutcomeAllowed
		if failed > 0 {
			outcome = audit.OutcomeError
		}
		if _, aerr := r.Audit.Record(audit.Entry{
			Namespace: namespace,
			EventType: audit.EventEstopTriggered,
			Action:    "estop-trigger",
			Outcome:   outcome,
			Actor:     audit.Actor{Type: audit.ActorServiceAccount, Identity: lateArrivalActor},
			Resource:  audit.Resource{Kind: "FleetZone", Namespace: namespace, Name: zone.Name},
			Detail: map[string]string{
				"reason":          reason,
				"robots_in_scope": strings.Join(names, ","),
				"robots":          itoa(len(late)),
				"stopped":         itoa(stopped),
				"unconfirmed":     itoa(failed),
				"unwritten":       itoa(unwritten),
				"late_arrival":    "true",
			},
		}); aerr != nil {
			log.FromContext(ctx).Error(aerr, "recording ESTOP_TRIGGERED audit entry for late arrivals")
		}
	}
	if unwritten > 0 {
		errs = append(errs, fmt.Errorf("zone %s estop: the outcome or hold record of %d late robot(s) was not written; "+
			"retrying", zone.Name, unwritten))
	}
	return errors.Join(errs...)
}

// zonesAboveRobot maps a Robot event to the zones whose estop could reach it: its zone
// (status.currentZone, else spec.zone, as robotsInEstopScope reads it, and both, to be safe)
// and every parentZone ancestor of each, and every zone whose stop the robot records
// (status.estopHeldBy), wherever the robot is now. Each zone with an estop in force then
// stops it if it has not reached it (holdLateArrivals); the others do nothing.
func (r *ZoneEstopReconciler) zonesAboveRobot(ctx context.Context, obj client.Object) []reconcile.Request {
	robot, ok := obj.(*fleetv1.Robot)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []reconcile.Request
	for _, start := range []string{robot.Status.CurrentZone, robot.Spec.Zone} {
		for _, name := range r.zoneChain(ctx, robot.Namespace, start) {
			if !seen[name] {
				seen[name] = true
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
					Namespace: robot.Namespace, Name: name}})
			}
		}
	}
	for _, e := range robot.Status.EstopHeldBy {
		if e.Kind == fleetv1.EstopHoldFleetZone && !seen[e.Name] {
			seen[e.Name] = true
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: robot.Namespace, Name: e.Name}})
		}
	}
	return out
}

// zoneChain is start and every parentZone ancestor of it, up to zoneEstopWalkCap ancestors, the
// bound the stop guard's walk shares, so a stop on the farthest ancestor the fan-out reaches is
// woken by a robot's arrival below it; a zone the cache does not hold ends the chain.
func (r *ZoneEstopReconciler) zoneChain(ctx context.Context, namespace, start string) []string {
	var out []string
	seen := map[string]bool{}
	name := start
	for depth := 0; name != "" && !seen[name] && depth <= zoneEstopWalkCap; depth++ {
		seen[name] = true
		out = append(out, name)
		fz := &fleetv1.FleetZone{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, fz); err != nil {
			break
		}
		name = fz.Spec.ParentZone
	}
	return out
}

// ancestorsOfZone maps a FleetZone event to every parentZone ancestor of that zone. A zone
// created under a stopped zone, or re-parented under one, or given a new estopPolicy, puts
// robots under that stop without any event on the stopped zone itself, and no Robot event
// either; this enqueues the stopped zone so its late-arrival stop runs (holdLateArrivals).
func (r *ZoneEstopReconciler) ancestorsOfZone(ctx context.Context, obj client.Object) []reconcile.Request {
	zone, ok := obj.(*fleetv1.FleetZone)
	if !ok || zone.Spec.ParentZone == "" {
		return nil
	}
	var out []reconcile.Request
	for _, name := range r.zoneChain(ctx, zone.Namespace, zone.Spec.ParentZone) {
		if name == zone.Name {
			break
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: zone.Namespace, Name: name}})
	}
	return out
}

// robotEnteredAZone admits the Robot events that can bring a robot under a zone's stop or
// show that a stop it records has not been written: its create, and an update that changes
// spec.zone, status.currentZone or status.estopState.
var robotEnteredAZone = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return true },
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok1 := e.ObjectOld.(*fleetv1.Robot)
		n, ok2 := e.ObjectNew.(*fleetv1.Robot)
		return ok1 && ok2 && (o.Spec.Zone != n.Spec.Zone || o.Status.CurrentZone != n.Status.CurrentZone ||
			o.Status.EstopState != n.Status.EstopState)
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// SetupWithManager registers the zone-estop controller. A Robot created into a zone, moved
// into one, or whose estopState changes, reconciles the zones above it and the zones whose
// stop it records; a FleetZone event reconciles its ancestors. So a stop in force reaches a
// robot that comes under it by any path, without waiting on another controller's write
// (holdLateArrivals).
func (r *ZoneEstopReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1.FleetZone{}).
		Watches(&fleetv1.FleetZone{}, handler.EnqueueRequestsFromMapFunc(r.ancestorsOfZone)).
		Watches(&fleetv1.Robot{}, handler.EnqueueRequestsFromMapFunc(r.zonesAboveRobot),
			builder.WithPredicates(robotEnteredAZone)).
		Named("zoneestop").
		Complete(r)
}
