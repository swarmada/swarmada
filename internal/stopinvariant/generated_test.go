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

package stopinvariant

import (
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/executorguard"
)

// The generated stop-invariant test (RFC-0001 #safety-estop-scopes,
// #safety-assignment-lease-and-the-single-executor-guarantee).
//
// The world is small: two to four robots, a zone tree that grows to any depth the operations
// reach (re-parents included), one namespace with a SwarmadaConfig, actions that are created,
// dispatched, paused, requeued, cancelled, deleted and created again; robots deleted while they
// record an action; two zone edits admitted against the same tree; a cancel, a delete and the
// member created again; a requeue just after a stop's clear; an adapter that reports its action
// failed while a stop covers its robot, before or after the pause is written; a requeue asked by a
// named requester, before a stop or under one, and an action deleted and created again carrying
// one; a robot that never confirms its stop and whose lease runs out. Every operation that the
// control plane would decide goes through the real guards: admission of a zone edit, a robot
// move, a robot delete or a finalizer removal through the stop guard's coverage; the fan-out
// and the late-arrival pass through the stop guard's coverage; dispatch and the release of a
// robot's record through the executor guard; a failure report, a requeue, the move of work off an
// unconfirmed stop and every status write through the decisions the controller and the admission
// checks make (executorguard). A stale-cache injector makes the position hold
// decide on an older recorded zone when the model says the control plane reads the cache; the
// guards read the model's truth (the API server). After every step these checks run against an
// oracle that walks the tree without a bound (a chain that cycles is reached by every standing stop
// that propagates, as the fan-out stops more robots, never fewer):
//
//  1. every robot a stop in force covers is held, and no command but a stop reached it since;
//  2. no action has two executors whose lease windows overlap;
//  3. no admitted edit took a covered robot out of a stop's reach;
//  4. every stop that is issued is in force (its processed marker written) once the controllers
//     have run one fan-out pass after it, and from then on check 1 holds for it;
//  5. no action leaves a terminal phase, except Failed by its onFailure retry;
//  6. work a stop interrupted on a robot is given to no other robot while that stop stands, except
//     by a requeue whose requester holds estop-clear on it;
//  7. no work leaves a robot under a stop it has not confirmed, for Pending or for another robot,
//     except by an operator's requeue or the robot's own end report: a provably dead lease moves
//     nothing (invariant I1), whether or not the robot reported the work started. After the
//     controllers run, such work whose lease is provably dead waits for a person, and work that
//     was deleted is only kept until the stop no longer holds the robot.
//
// The bound in check 4 is one fan-out pass. The model's tree, robots and stops do not change
// inside a pass, and the controller applies every pending stop of a carrier in one reconcile; a
// pass that cannot apply a stop is retried by the controller against the same objects, so a stop
// still pending after one pass would still be pending after any number of retries: it reaches no
// robot. That is a shed whichever robot it covers (a stop that cannot be fanned out completely
// stops more robots, never fewer), so check 4 needs no covered robot to fail. A stop in force that
// does not hold a robot it covers is check 1's.
//
// A failure prints the seed and the shrunk operation sequence.

var longRun = flag.Bool("stop-invariant-long", false, "run the long generated stop-invariant sequence set (20,000 sequences)")

const (
	defaultSequences = 2000
	longSequences    = 20000
	opsPerSequence   = 40
	namespace        = "ns"
	configName       = "swarmada-config"
)

// ── The model ──

type zone struct {
	name, parent    string
	propagate       bool
	trigger, marker bool
	finalizer       bool
}

type robot struct {
	name                 string
	specZone, curZone    string
	cachedCurZone        string // what a lagging cache shows
	trigger, marker      bool
	records              map[string]bool // "kind/name" of stops recorded
	stopped              bool
	confirmed            bool // the robot confirmed its stop (estopState Stopped)
	terminating          bool
	stopFinalizerOnly    bool // deleted while held: only the stop finalizer keeps it
	assignedAction       string
	commandsSinceCovered []string
	// physically running: the action the robot executes and until when (its own lease)
	runningAction string
	runningUntil  time.Time
}

type action struct {
	name            string
	phase           fleetv1.ActionPhase
	bound           string
	horizon         *time.Time
	pendingSince    *time.Time
	completion      *time.Time
	created         time.Time
	desiredPaused   bool
	cancelConfirmed map[string]bool // recorders whose adapter acknowledged a cancel
	requeue         *requeueReq     // a standing requeue request
	authorized      bool            // moved by a requeue whose requester holds estop-clear
	awaitsPerson    bool            // the controller holds it for a person: unconfirmed stop, dead lease
	retries         int
	legalExit       bool                // this step left Failed by the onFailure retry
	wasTerminal     fleetv1.ActionPhase // the terminal phase the action reached, if any
	started         bool                // the robot reported it running (status.startedAt)
	terminating     bool                // deleted, kept by the stop finalizer while a stopped robot records it
	// executions: robot -> [start, end) windows the oracle checks for overlap
	executions []execution
}

// requeueReq is a standing requeue request: who asked, and whether the controller has handled it
// and under which stops it first did (the wait message).
type requeueReq struct {
	who       string
	seen      bool
	seenUnder []string
}

type execution struct {
	robot      string
	start, end time.Time
}

type world struct {
	now      time.Time
	zones    map[string]*zone
	robots   map[string]*robot
	actions  map[string]*action
	config   bool
	nsTrig   bool
	nsMarker bool
	leaseDur time.Duration
	skew     time.Duration
	// adapters answer cancels (true) or are deaf (false), per robot
	acks map[string]bool
	// log of commands sent to robots: "robot:kind:action"
	log []string
	// passJustRan: the last operation was a fan-out or a late-arrival pass, after which every
	// robot a stop in force covers must be stopped.
	passJustRan bool
	// estopAcks: the robot's adapter answers a stop (it is confirmed), per robot
	estopAcks map[string]bool
	// stopHeld (truth): the work a stop interrupted, by name, and the robot it was on
	stopHeld map[string]string
	// orphanSeen: when the controller first saw a robot record an action that no longer exists
	orphanSeen map[string]time.Time
	// gone (truth): the executions of a deleted action, by name; a robot may still run it
	gone map[string][]execution
	// ghosts (truth): the stops a stopped robot recorded when its object was deleted; the robot
	// itself is still there, stopped, until those stops are cleared
	ghosts map[string][]string
	// phases: each action object's phase after the last step, for check 5
	phases map[*action]fleetv1.ActionPhase
	// violation: a check that an operation itself found (6, and the legality of a move)
	violation string
	// unconfirmedHeld: before the current operation, the work bound to a robot under a stop it has
	// not confirmed, and that robot (check 7); decided: the work an operator's requeue or the
	// robot's own end report moved in the current operation
	unconfirmedHeld map[string]string
	decided         map[string]bool
}

func newWorld(seed int64) *world {
	w := &world{now: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), zones: map[string]*zone{}, robots: map[string]*robot{},
		actions: map[string]*action{}, config: true, leaseDur: 30 * time.Second, skew: 5 * time.Second, acks: map[string]bool{},
		estopAcks: map[string]bool{}, stopHeld: map[string]string{}, orphanSeen: map[string]time.Time{}, phases: map[*action]fleetv1.ActionPhase{},
		gone: map[string][]execution{}, ghosts: map[string][]string{}, unconfirmedHeld: map[string]string{}, decided: map[string]bool{}}
	w.zones["z0"] = &zone{name: "z0", propagate: true}
	w.zones["z1"] = &zone{name: "z1", parent: "z0", propagate: true}
	w.zones["out"] = &zone{name: "out", propagate: true}
	rng := rand.New(rand.NewSource(seed))
	n := 2 + rng.Intn(3)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("r%d", i)
		z := []string{"z0", "z1", "out"}[rng.Intn(3)]
		w.robots[name] = &robot{name: name, specZone: z, curZone: z, cachedCurZone: z, records: map[string]bool{}}
		w.acks[name] = rng.Intn(3) != 0
		w.estopAcks[name] = rng.Intn(4) != 0
	}
	return w
}

// ── Views of the model as API objects, for the guards ──

func (w *world) tree() *estopscope.Tree {
	var zs []fleetv1.FleetZone
	for _, z := range w.sortedZones() {
		zs = append(zs, w.zoneObj(z))
	}
	return estopscope.TreeOf(zs)
}

func (w *world) zoneObj(z *zone) fleetv1.FleetZone {
	fz := fleetv1.FleetZone{ObjectMeta: metav1.ObjectMeta{Name: z.name, Namespace: namespace, Annotations: map[string]string{}},
		Spec: fleetv1.FleetZoneSpec{ParentZone: z.parent, EstopPolicy: &fleetv1.ZoneEstopPolicy{PropagateToChildren: &z.propagate}}}
	if z.trigger {
		fz.Annotations[estopscope.TriggerAnnotation] = "s"
	}
	if z.marker {
		fz.Annotations[estopscope.ProcessedAnnotation] = "s"
	}
	return fz
}

func (w *world) configs() []fleetv1.SwarmadaConfig {
	if !w.config {
		return nil
	}
	c := fleetv1.SwarmadaConfig{ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: namespace, Annotations: map[string]string{}}}
	if w.nsTrig {
		c.Annotations[estopscope.TriggerAnnotation] = "n"
	}
	if w.nsMarker {
		c.Annotations[estopscope.ProcessedAnnotation] = "n"
	}
	return []fleetv1.SwarmadaConfig{c}
}

func (w *world) robotObj(r *robot, stale bool) *fleetv1.Robot {
	rb := &fleetv1.Robot{ObjectMeta: metav1.ObjectMeta{Name: r.name, Namespace: namespace, Annotations: map[string]string{}}}
	rb.Spec.Zone = r.specZone
	rb.Status.CurrentZone = r.curZone
	if stale {
		rb.Status.CurrentZone = r.cachedCurZone
	}
	if r.trigger {
		rb.Annotations[estopscope.TriggerAnnotation] = "r"
	}
	if r.marker {
		rb.Annotations[estopscope.ProcessedAnnotation] = "r"
	}
	for k := range r.records {
		kind, name, _ := strings.Cut(k, "/")
		h := fleetv1.EstopHold{Name: name, Kind: fleetv1.EstopHoldFleetZone}
		if kind == "namespace" {
			h.Kind = fleetv1.EstopHoldSwarmadaConfig
		}
		rb.Status.EstopHeldBy = append(rb.Status.EstopHeldBy, h)
	}
	if r.terminating {
		rb.DeletionTimestamp = &metav1.Time{Time: w.now}
	}
	rb.Status.AssignedAction = r.assignedAction
	return rb
}

func (w *world) sortedZones() []*zone {
	names := make([]string, 0, len(w.zones))
	for n := range w.zones {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*zone, 0, len(names))
	for _, n := range names {
		out = append(out, w.zones[n])
	}
	return out
}

func (w *world) sortedRobots() []*robot {
	names := make([]string, 0, len(w.robots))
	for n := range w.robots {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*robot, 0, len(names))
	for _, n := range names {
		out = append(out, w.robots[n])
	}
	return out
}

func (w *world) sortedActions() []*action {
	names := make([]string, 0, len(w.actions))
	for n := range w.actions {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*action, 0, len(names))
	for _, n := range names {
		out = append(out, w.actions[n])
	}
	return out
}

// ── The oracle: unbounded truth ──

// oracleCovers names every stop, in force or pending, that reaches robot by the unbounded tree
// walk, its own trigger, its records and the namespace.
func (w *world) oracleCovers(r *robot) map[string]bool {
	out := map[string]bool{}
	if r.trigger || r.marker {
		out["robot/"+r.name] = true
	}
	stands := r.curZone
	if stands == "" {
		stands = r.specZone
	}
	for _, start := range []string{stands} {
		seen := map[string]bool{}
		name := start
		first := true
		for name != "" && !seen[name] {
			seen[name] = true
			z, ok := w.zones[name]
			if !ok {
				break
			}
			if (z.trigger || z.marker) && (first || z.propagate) {
				out["zone/"+z.name] = true
			}
			first = false
			name = z.parent
		}
		if name != "" && seen[name] {
			// the chain cycles: it has no root to walk to, and a stop that cannot tell whether it
			// reaches the robot reaches it (stops more robots, never fewer)
			for _, z := range w.zones {
				if (z.trigger || z.marker) && (z.propagate || z.name == start) {
					out["zone/"+z.name] = true
				}
			}
		}
	}
	if w.config && (w.nsTrig || w.nsMarker) {
		out["namespace/"+configName] = true
	}
	for k := range r.records {
		out[k] = true
	}
	return out
}

// oracleInForce names the stops in force (applied, or recorded) that cover robot: the ones that
// must hold it now.
func (w *world) oracleInForce(r *robot) map[string]bool {
	out := map[string]bool{}
	for k := range w.oracleCovers(r) {
		kind, name, _ := strings.Cut(k, "/")
		switch kind {
		case "zone":
			if z, ok := w.zones[name]; ok && z.marker {
				out[k] = true
			}
		case "namespace":
			if w.nsMarker {
				out[k] = true
			}
		case "robot":
			if r.marker {
				out[k] = true
			}
		}
		if r.records[k] {
			out[k] = true
		}
	}
	return out
}

// ── The system under test: the guards deciding every transition ──

func (w *world) guardCoverage(r *robot, stale bool) (estopscope.Coverage, error) {
	return estopscope.CoverageOf(w.tree(), w.configs(), w.robotObj(r, stale))
}

// admitZoneEdit is the FleetZone webhook's rule through the stop guard.
func (w *world) admitZoneEdit(z *zone, newParent string, newPropagate bool) bool {
	edited := w.zoneObj(z)
	edited.Spec.ParentZone = newParent
	edited.Spec.EstopPolicy = &fleetv1.ZoneEstopPolicy{PropagateToChildren: &newPropagate}
	t := w.tree()
	if newParent != z.parent {
		deepest, _, err := t.With(&edited).DeepestBelow(z.name)
		if err != nil || deepest > estopscope.MaxAncestors {
			return false
		}
		// a cycle: the new parent is at or below the zone
		sub, err := t.Subtree(z.name)
		if err != nil {
			return false
		}
		for _, s := range sub {
			if s == newParent {
				return false
			}
		}
	}
	loss, err := estopscope.EditUncovers(t, &edited)
	if err != nil {
		// a tree already past the bound: repairable only with no zone stop standing
		var deep *estopscope.TooDeepError
		if !errors.As(err, &deep) || t.StopCarrier() != "" {
			return false
		}
		loss = estopscope.EditLoss{}
	}
	// A pending propagate change is refused in both directions (the kept rule).
	if newPropagate != z.propagate {
		pend, err := estopscope.PendingStopsOverTree(t, z.name)
		if err != nil || len(pend) > 0 {
			return false
		}
	}
	return !loss.Any()
}

// admitRobotMove is the Robot webhook's rule: a move that takes the robot out of a pending stop
// is refused (a stop in force holds by record).
func (w *world) admitRobotMove(r *robot, newZone string) bool {
	after := *r
	after.specZone = newZone
	// a stop the robot records travels with it; a stop it does not yet record, pending or in
	// force, would be left behind under either reading
	lost, err := estopscope.MoveLoses(w.tree(), w.configs(), w.robotObj(r, false), w.robotObj(&after, false))
	return err == nil && len(lost) == 0
}

// admitRobotDelete is the Robot delete guard and the last-finalizer rule: refused while covered.
func (w *world) admitRobotDelete(r *robot) bool {
	cov, err := w.guardCoverage(r, false)
	if err != nil {
		return false
	}
	if cov.Covered() || r.stopped {
		return false
	}
	// a robot that records an action holds the executor guard's key, whether the action exists or not
	if r.assignedAction != "" {
		in := executorguard.Input{ActionGone: true, Now: w.now, Skew: executorguard.MaxSkew}
		if a := w.actions[r.assignedAction]; a != nil {
			in = executorguard.Input{Horizon: a.horizon, Reference: executorguard.ReferenceOf(w.actionObj(a)), Now: w.now,
				Skew: executorguard.MaxSkew}
		}
		if loses, _ := executorguard.DeleteLosesKey(r.name, in); loses {
			return false
		}
	}
	return true
}

// actionObj is the action as an API object, for the executor guard's reading of it.
func (w *world) actionObj(a *action) *fleetv1.FleetAction {
	fa := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: a.name, Namespace: namespace,
		CreationTimestamp: metav1.NewTime(a.created)}}
	fa.Status.Phase = a.phase
	fa.Status.AssignedRobot = a.bound
	at := func(t *time.Time) *metav1.Time {
		if t == nil {
			return nil
		}
		m := metav1.NewTime(*t)
		return &m
	}
	fa.Status.LeaseExpiresAt = at(a.horizon)
	fa.Status.PendingSince = at(a.pendingSince)
	fa.Status.CompletionTime = at(a.completion)
	return fa
}

// covering names the stop that covers r, as the controller asks it (stopCovering): the stop
// guard's coverage, or the robot's own stop state; an unanswerable question covers.
func (w *world) covering(r *robot) string {
	cov, err := w.guardCoverage(r, false)
	if err != nil {
		return "undecided"
	}
	if cov.Covered() {
		return strings.Join(cov.Names(), ",")
	}
	if r.stopped {
		return "robot/" + r.name
	}
	return ""
}

// holdPosition is the Zone Controller's hold: a frame moving the recorded zone out of a pending
// stop is held. With stale, the hold is decided on the cache's view of the robot.
func (w *world) holdPosition(r *robot, to string, stale bool) bool {
	rb := w.robotObj(r, stale)
	from := rb.Status.CurrentZone
	if from == "" {
		from = rb.Spec.Zone
	}
	if from == "" || from == to {
		return false
	}
	was := rb.DeepCopy()
	was.Status.CurrentZone = from
	now := rb.DeepCopy()
	now.Status.CurrentZone = to
	lost, err := estopscope.MoveLoses(w.tree(), w.configs(), was, now)
	return err != nil || len(lost) > 0
}

// fanOut applies every pending stop: each robot the guard says the carrier covers is stopped
// and records it; the marker is written.
func (w *world) fanOut() {
	for _, z := range w.sortedZones() {
		if z.trigger && !z.marker {
			robots := w.sortedRobots()
			in, _, err := w.tree().FanOutScope(z.name, fanOutZones(robots), z.propagate)
			if err != nil {
				continue // the pass fails and is retried: no robot is stopped, no marker is written
			}
			for i, r := range robots {
				if in[i] {
					w.stopRobot(r, "zone/"+z.name)
				}
			}
			z.marker = true
		}
	}
	if w.config && w.nsTrig && !w.nsMarker {
		for _, r := range w.sortedRobots() {
			w.stopRobot(r, "namespace/"+configName)
		}
		w.nsMarker = true
	}
	for _, r := range w.sortedRobots() {
		if r.trigger && !r.marker {
			w.stopRobot(r, "robot/"+r.name)
			r.marker = true
		}
	}
	// the reconcile of a stop in force is its late-arrival pass
	w.lateArrivals()
}

// lateArrivals is the late-arrival pass of every stop in force: a robot the guard says a stop
// in force covers and that does not record it is stopped now.
func (w *world) lateArrivals() {
	robots := w.sortedRobots()
	for _, z := range w.sortedZones() {
		if !z.marker {
			continue
		}
		in, _, err := w.tree().FanOutScope(z.name, fanOutZones(robots), z.propagate)
		if err != nil {
			continue
		}
		for i, r := range robots {
			if in[i] && !r.records["zone/"+z.name] {
				w.stopRobot(r, "zone/"+z.name)
			}
		}
	}
	if w.config && w.nsMarker {
		for _, r := range robots {
			if !r.records["namespace/"+configName] {
				w.stopRobot(r, "namespace/"+configName)
			}
		}
	}
}

// fanOutZones is the zone each robot stands in as the zone fan-out reads it: the recorded
// zone, else the declared one.
func fanOutZones(robots []*robot) []string {
	out := make([]string, len(robots))
	for i, r := range robots {
		out[i] = r.curZone
		if out[i] == "" {
			out[i] = r.specZone
		}
	}
	return out
}

func (w *world) stopRobot(r *robot, key string) {
	if !r.stopped {
		// the work the stop interrupts on this robot
		for _, a := range w.sortedActions() {
			if (a.bound == r.name || r.assignedAction == a.name) && !terminal(a.phase) {
				w.stopHeld[a.name] = r.name
			}
		}
		// a record of work that had already ended (its robot reported it) is not work the stop
		// interrupted; a record of a deleted action is
		if a := w.actions[r.assignedAction]; r.assignedAction != "" && (a == nil || !terminal(a.phase)) {
			w.stopHeld[r.assignedAction] = r.name
		}
	}
	r.stopped = true
	r.records[key] = true
	if w.estopAcks[r.name] {
		// the stop reaches the robot and is confirmed: it stops executing
		r.confirmed = true
		r.runningAction, r.runningUntil = "", time.Time{}
	}
	// a robot that never confirms its stop may still move until its own lease runs out
	w.log = append(w.log, r.name+":stop:"+key)
}

func terminal(p fleetv1.ActionPhase) bool {
	return p == fleetv1.ActionPhaseSucceeded || p == fleetv1.ActionPhaseFailed || p == fleetv1.ActionPhaseCancelled
}

// coveringNames is covering as a list.
func (w *world) coveringNames(r *robot) []string {
	c := w.covering(r)
	if c == "" {
		return nil
	}
	return strings.Split(c, ",")
}

// workStops names the stops that cover the robot an action is bound to or, with no binding, every
// robot that records it, as the controller asks it.
func (w *world) workStops(a *action) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range w.sortedRobots() {
		if (a.bound != "" && r.name == a.bound) || (a.bound == "" && r.assignedAction == a.name) {
			for _, n := range w.coveringNames(r) {
				if !seen[n] {
					seen[n] = true
					out = append(out, n)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// ghostStands reports whether a stop a deleted, stopped robot recorded still stands.
func (w *world) ghostStands(name string) bool {
	for _, k := range w.ghosts[name] {
		kind, cname, _ := strings.Cut(k, "/")
		if w.stopStands(kind, cname) {
			return true
		}
	}
	return false
}

// keepsFinalizer is the robot estop controller's rule for the stop finalizer on a robot.
func (w *world) keepsFinalizer(r *robot) bool {
	return executorguard.KeepsStopFinalizer(r.trigger || r.marker, r.stopped)
}

// removeRobot deletes a robot object; a stopped one is still there physically.
func (w *world) removeRobot(r *robot) {
	if r.stopped {
		w.ghosts[r.name] = keys(r.records)
	}
	for _, act := range w.actions {
		if act.bound == r.name {
			act.bound = ""
		}
	}
	delete(w.robots, r.name)
}

// heldBy reports whether a stopped robot records the action name.
func (w *world) heldBy(name string) bool {
	for _, r := range w.robots {
		if r.assignedAction == name && r.stopped {
			return true
		}
	}
	return false
}

// deleteActionAs is a FleetAction delete by who (operator, admin, or outage: admitted unchecked).
// Held work is refused to anyone without estop-clear; admitted, it stays Terminating while a
// stopped robot records it. It reports whether the action object is gone.
func (w *world) deleteActionAs(name, who string) bool {
	act := w.actions[name]
	if act == nil {
		return true
	}
	if who != "outage" && who != "admin" && len(w.workStops(&action{name: name})) > 0 {
		return false
	}
	if w.heldBy(name) {
		act.terminating = true
		return false
	}
	w.gone[name] = act.executions
	delete(w.actions, name)
	return true
}

// staleWriteLands reports whether a status write computed from a stale copy lands on the API
// server: the controller's status patch carries no resourceVersion precondition.
func staleWriteLands() bool {
	orig := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: namespace, ResourceVersion: "7"}}
	mod := orig.DeepCopy()
	mod.Status.Phase = fleetv1.ActionPhasePending
	data, err := executorguard.StatusPatch(orig).Data(mod)
	return err == nil && !strings.Contains(string(data), "resourceVersion")
}

// dispatch gives every Pending action to a free robot the stop guard says no stop covers and
// the executor guard says may take it.
func (w *world) dispatch() {
	for _, a := range w.sortedActions() {
		if a.phase != fleetv1.ActionPhasePending || a.terminating {
			continue
		}
		if !w.executorGate(a, false) {
			continue
		}
		for _, r := range w.sortedRobots() {
			if r.assignedAction != "" || r.stopped || r.terminating {
				continue
			}
			cov, err := w.guardCoverage(r, false)
			if err != nil || cov.Covered() {
				continue
			}
			if h, ok := w.stopHeld[a.name]; ok && h != r.name {
				if hr := w.robots[h]; (hr != nil && len(w.oracleCovers(hr)) > 0 || hr == nil && w.ghostStands(h)) && w.violation == "" {
					if hr == nil {
						hr = &robot{name: h, records: map[string]bool{}}
					}
					w.violation = fmt.Sprintf("work %s a stop interrupted on robot %s (covered by %v) was given to %s while that "+
						"stop stands, by no estop-clear requeue", a.name, h, keys(w.oracleCovers(hr)), r.name)
				}
			}
			a.phase = fleetv1.ActionPhaseAssigned
			a.bound = r.name
			h := w.now.Add(w.leaseDur)
			a.horizon = &h
			a.pendingSince = nil
			r.assignedAction = a.name
			r.runningAction = a.name
			r.runningUntil = h
			a.executions = append(a.executions, execution{robot: r.name, start: w.now, end: h})
			w.log = append(w.log, r.name+":assign:"+a.name)
			break
		}
	}
}

// executorGate is the executor guard over the robots that record a: it asks each to cancel and
// releases those that are provably off it; it reports whether a may be given to a robot now.
func (w *world) executorGate(a *action, moveAuthorized bool) bool {
	var recs []executorguard.Recorder
	for _, r := range w.sortedRobots() {
		if r.assignedAction != a.name || r.name == a.bound {
			continue
		}
		covered := w.covering(r)
		rec := executorguard.Recorder{Robot: r.name, Covered: covered}
		if executorguard.KeepsRecord(covered, moveAuthorized) {
			recs = append(recs, rec)
			continue
		}
		if w.acks[r.name] {
			// the adapter acknowledges the cancel: the robot stops executing
			r.runningAction, r.runningUntil = "", time.Time{}
			w.closeExecution(a, r.name)
			rec.CancelConfirmed = true
		}
		w.log = append(w.log, r.name+":cancel:"+a.name)
		recs = append(recs, rec)
	}
	if len(recs) == 0 {
		return true
	}
	d := executorguard.Decide(executorguard.Input{Horizon: a.horizon, Reference: executorguard.ReferenceOf(w.actionObj(a)),
		Recorders: recs, Now: w.now, Skew: w.skew, MoveAuthorized: moveAuthorized})
	for _, name := range d.Release {
		if r := w.robots[name]; r != nil && r.assignedAction == a.name {
			r.assignedAction = ""
		}
	}
	return d.Dispatchable()
}

func (w *world) closeExecution(a *action, robot string) {
	for i := range a.executions {
		if a.executions[i].robot == robot && a.executions[i].end.After(w.now) {
			a.executions[i].end = w.now
		}
	}
}

// ── Operations ──

type op struct {
	kind string
	args []string
}

func (o op) String() string { return o.kind + "(" + strings.Join(o.args, ",") + ")" }

var opKinds = []string{"createZone", "deepChain", "reparent", "flipPropagate", "deleteZone", "triggerZone", "clearZone", "triggerNs", "clearNs",
	"triggerRobot", "clearRobot", "fanOut", "lateArrivals", "createRobot", "deleteRobot", "finishDelete", "recreateRobot", "move",
	"position", "stalePosition", "createAction", "dispatch", "pause", "requeue", "cancel", "regenerate", "advance", "leaseChange",
	"deleteConfig", "tick", "deleteRecorder", "reparentPair", "cancelRegenerate", "requeueAfterClear", "adapterFail", "failRace",
	"completeRace", "deleteAction", "recreateRequeue", "requeueThenStop", "lateConfirm", "start", "stopThenFail", "deleteActionThenRobot",
	"deleteRobotOutage", "renameCreate"}

// actionOps are the operations that exercise the executor guard; half of every sequence is
// drawn from them, so a pause, a requeue or a regeneration meets a dispatch often enough.
var actionOps = []string{"createAction", "dispatch", "pause", "requeue", "cancel", "regenerate", "advance", "tick",
	"triggerZone", "clearZone", "fanOut", "leaseChange", "deleteRecorder", "cancelRegenerate", "requeueAfterClear", "adapterFail",
	"failRace", "completeRace", "deleteAction", "recreateRequeue", "requeueThenStop", "lateConfirm", "start", "stopThenFail",
	"deleteActionThenRobot", "deleteRobotOutage", "renameCreate"}

func (w *world) randomOp(rng *rand.Rand) op {
	kind := opKinds[rng.Intn(len(opKinds))]
	if rng.Intn(2) == 0 {
		kind = actionOps[rng.Intn(len(actionOps))]
	}
	pick := func(names []string) string {
		if len(names) == 0 {
			return ""
		}
		return names[rng.Intn(len(names))]
	}
	zones := make([]string, 0, len(w.zones))
	for _, z := range w.sortedZones() {
		zones = append(zones, z.name)
	}
	robots := make([]string, 0, len(w.robots))
	for _, r := range w.sortedRobots() {
		robots = append(robots, r.name)
	}
	actions := make([]string, 0, len(w.actions))
	var bound []string // actions a robot holds: the operations on live work draw from them first
	for _, a := range w.sortedActions() {
		actions = append(actions, a.name)
		if a.bound != "" {
			bound = append(bound, a.name)
		}
	}
	live := func() string {
		if len(bound) > 0 && rng.Intn(4) != 0 {
			return pick(bound)
		}
		return pick(actions)
	}
	switch kind {
	case "createZone":
		return op{kind, []string{fmt.Sprintf("n%d", rng.Intn(1000)), pick(append(zones, ""))}}
	case "deepChain":
		// a chain long enough to reach the bound: admission admits a zone and 64 ancestors
		return op{kind, []string{fmt.Sprintf("c%d", rng.Intn(100)), pick(zones), strconv.Itoa(55 + rng.Intn(14))}}
	case "reparent":
		return op{kind, []string{pick(zones), pick(append(zones, ""))}}
	case "flipPropagate", "deleteZone", "triggerZone", "clearZone":
		return op{kind, []string{pick(zones)}}
	case "triggerRobot", "clearRobot", "deleteRobot", "finishDelete", "recreateRobot", "deleteRecorder":
		return op{kind, []string{pick(robots)}}
	case "reparentPair":
		return op{kind, []string{pick(zones), pick(append(zones, "")), pick(zones), pick(append(zones, ""))}}
	case "cancelRegenerate", "renameCreate":
		return op{kind, []string{pick(actions)}}
	case "deleteAction", "deleteActionThenRobot":
		return op{kind, []string{live(), pick([]string{"operator", "admin", "outage"})}}
	case "deleteRobotOutage":
		return op{kind, []string{pick(robots)}}
	case "adapterFail", "completeRace", "requeueThenStop", "start":
		return op{kind, []string{live()}}
	case "requeueAfterClear":
		return op{kind, []string{pick(zones), pick(actions)}}
	case "failRace", "stopThenFail":
		return op{kind, []string{live(), pick([]string{"failure-first", "pause-first"})}}
	case "recreateRequeue":
		return op{kind, []string{pick(actions), pick([]string{"operator", "admin"})}}
	case "lateConfirm":
		return op{kind, []string{pick(robots)}}
	case "createRobot":
		return op{kind, []string{fmt.Sprintf("r%d", 10+rng.Intn(90)), pick(zones)}}
	case "move", "position", "stalePosition":
		return op{kind, []string{pick(robots), pick(zones)}}
	case "createAction":
		return op{kind, []string{fmt.Sprintf("a%d", rng.Intn(6))}}
	case "pause", "cancel", "regenerate":
		return op{kind, []string{pick(actions)}}
	case "requeue":
		return op{kind, []string{pick(actions), pick([]string{"operator", "admin"})}}
	case "advance":
		return op{kind, []string{strconv.Itoa(1 + rng.Intn(400))}}
	case "leaseChange":
		return op{kind, []string{strconv.Itoa(10 + rng.Intn(291))}}
	}
	return op{kind, nil}
}

// apply runs one operation; it returns a description of an admitted edit (for check 3) or "".
func (w *world) apply(o op) (admittedEdit bool, before map[string]map[string]bool) {
	a := func(i int) string {
		if i < len(o.args) {
			return o.args[i]
		}
		return ""
	}
	snapshot := func() map[string]map[string]bool {
		m := map[string]map[string]bool{}
		for _, r := range w.sortedRobots() {
			m[r.name] = w.oracleCovers(r)
		}
		return m
	}
	if act := w.actions[a(0)]; act != nil && act.terminating {
		switch o.kind {
		case "pause", "requeue", "cancel", "adapterFail", "failRace", "completeRace", "start", "stopThenFail", "requeueThenStop",
			"cancelRegenerate":
			return false, nil // the controller does nothing for a deleted action but keep it
		}
	}
	switch o.kind {
	case "createZone":
		name, parent := a(0), a(1)
		if _, exists := w.zones[name]; exists || (parent != "" && w.zones[parent] == nil) {
			return false, nil
		}
		z := &zone{name: name, parent: parent, propagate: true}
		t := w.tree().With(func() *fleetv1.FleetZone { fz := w.zoneObj(z); return &fz }())
		if d, err := t.Depth(name); err != nil || d > estopscope.MaxAncestors {
			return false, nil
		}
		w.zones[name] = z
		return false, nil
	case "deepChain":
		prefix, parent := a(0), a(1)
		n, _ := strconv.Atoi(a(2))
		if w.zones[parent] == nil {
			return false, nil
		}
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("%s-%d", prefix, i)
			if _, exists := w.zones[name]; exists {
				break
			}
			z := &zone{name: name, parent: parent, propagate: true}
			t := w.tree().With(func() *fleetv1.FleetZone { fz := w.zoneObj(z); return &fz }())
			if d, err := t.Depth(name); err != nil || d > estopscope.MaxAncestors {
				break // admission refuses a zone past the bound
			}
			w.zones[name] = z
			parent = name
		}
		return false, nil
	case "reparent":
		z := w.zones[a(0)]
		if z == nil || a(1) == z.name || (a(1) != "" && w.zones[a(1)] == nil) {
			return false, nil
		}
		before = snapshot()
		if !w.admitZoneEdit(z, a(1), z.propagate) {
			return false, nil
		}
		z.parent = a(1)
		return true, before
	case "flipPropagate":
		z := w.zones[a(0)]
		if z == nil {
			return false, nil
		}
		before = snapshot()
		if !w.admitZoneEdit(z, z.parent, !z.propagate) {
			return false, nil
		}
		z.propagate = !z.propagate
		return true, before
	case "deleteZone":
		z := w.zones[a(0)]
		if z == nil || z.trigger || z.marker {
			return false, nil
		}
		for _, o := range w.zones {
			if o.parent == z.name {
				return false, nil // referenced
			}
		}
		for _, r := range w.robots {
			if r.specZone == z.name || r.curZone == z.name {
				return false, nil
			}
		}
		before = snapshot()
		if cov, err := estopscope.CoveringAt(w.tree(), nil, z.name); err != nil || len(cov) > 0 {
			return false, nil
		}
		delete(w.zones, z.name)
		return true, before
	case "triggerZone":
		if z := w.zones[a(0)]; z != nil && !z.trigger {
			z.trigger, z.finalizer = true, true
		}
	case "clearZone":
		if z := w.zones[a(0)]; z != nil && z.trigger {
			z.trigger = false
			w.clearStop("zone/" + z.name)
			z.marker, z.finalizer = false, false
		}
	case "triggerNs":
		if w.config {
			w.nsTrig = true
		}
	case "clearNs":
		if w.config && w.nsTrig {
			w.nsTrig = false
			w.clearStop("namespace/" + configName)
			w.nsMarker = false
		}
	case "triggerRobot":
		if r := w.robots[a(0)]; r != nil {
			r.trigger = true
		}
	case "clearRobot":
		if r := w.robots[a(0)]; r != nil && r.trigger {
			r.trigger = false
			w.clearStop("robot/" + r.name)
			r.marker = false
		}
	case "fanOut":
		w.fanOut()
	case "lateArrivals":
		w.lateArrivals()
	case "createRobot":
		if _, exists := w.robots[a(0)]; !exists && w.zones[a(1)] != nil && len(w.robots) < 4 {
			w.robots[a(0)] = &robot{name: a(0), specZone: a(1), curZone: a(1), cachedCurZone: a(1), records: map[string]bool{}}
			w.acks[a(0)] = true
		}
	case "deleteRobot":
		r := w.robots[a(0)]
		if r == nil || r.terminating {
			return false, nil
		}
		before = snapshot()
		if !w.admitRobotDelete(r) {
			return false, nil
		}
		r.terminating = true // kept by a finalizer
		return true, before
	case "finishDelete":
		r := w.robots[a(0)]
		if r == nil || !r.terminating {
			return false, nil
		}
		before = snapshot()
		if !w.admitRobotDelete(r) {
			return false, nil
		}
		// the robot is gone; its executions end with it only when its lease does
		for _, act := range w.actions {
			if act.bound == r.name {
				act.bound = ""
			}
		}
		delete(w.robots, r.name)
		delete(before, r.name)
		return true, before
	case "recreateRobot":
		// a robot deleted and created again under its name: clean status, same zone
		r := w.robots[a(0)]
		if a(0) == "" || r != nil || len(w.robots) >= 4 {
			return false, nil
		}
		w.robots[a(0)] = &robot{name: a(0), specZone: "out", curZone: "out", cachedCurZone: "out", records: map[string]bool{}}
	case "move":
		r := w.robots[a(0)]
		if r == nil || w.zones[a(1)] == nil || a(1) == r.specZone {
			return false, nil
		}
		before = snapshot()
		if !w.admitRobotMove(r, a(1)) {
			return false, nil
		}
		r.specZone = a(1)
		return true, before
	case "position", "stalePosition":
		r := w.robots[a(0)]
		if r == nil || w.zones[a(1)] == nil || a(1) == r.curZone {
			return false, nil
		}
		before = snapshot()
		if w.holdPosition(r, a(1), o.kind == "stalePosition") {
			return false, nil
		}
		// the write carries the version it read: a stale read is rejected by the API server
		if o.kind == "stalePosition" && r.cachedCurZone != r.curZone {
			return false, nil
		}
		r.cachedCurZone = r.curZone // the cache now lags behind the write below
		r.curZone = a(1)
		return true, before
	case "deleteRecorder":
		// a robot deleted and its last finalizer removed in one go, each step admitted or not
		r := w.robots[a(0)]
		if r == nil {
			return false, nil
		}
		before = snapshot()
		if !r.terminating {
			if !w.admitRobotDelete(r) {
				return false, nil
			}
			r.terminating = true
		}
		if !w.admitRobotDelete(r) {
			return true, before
		}
		for _, act := range w.actions {
			if act.bound == r.name {
				act.bound = ""
			}
		}
		delete(w.robots, r.name)
		delete(before, r.name)
		return true, before
	case "reparentPair":
		// two re-parents whose webhooks both read the tree before either write is stored
		z1, z2 := w.zones[a(0)], w.zones[a(2)]
		if z1 == nil || z2 == nil || z1 == z2 {
			return false, nil
		}
		valid := func(z *zone, p string) bool { return p != z.name && (p == "" || w.zones[p] != nil) }
		before = snapshot()
		ok1 := valid(z1, a(1)) && w.admitZoneEdit(z1, a(1), z1.propagate)
		ok2 := valid(z2, a(3)) && w.admitZoneEdit(z2, a(3), z2.propagate)
		if ok1 {
			z1.parent = a(1)
		}
		if ok2 {
			z2.parent = a(3)
		}
		return ok1 || ok2, before
	case "cancelRegenerate":
		w.apply(op{"cancel", []string{a(0)}})
		w.apply(op{"regenerate", []string{a(0)}})
	case "requeueAfterClear":
		w.apply(op{"clearZone", []string{a(0)}})
		w.apply(op{"requeue", []string{a(1), "operator"}})
	case "createAction":
		if _, exists := w.actions[a(0)]; exists || a(0) == "" {
			return false, nil
		}
		// created again under a deleted action's name, it is the same work: a robot may still run it
		w.actions[a(0)] = &action{name: a(0), phase: fleetv1.ActionPhasePending, created: w.now, cancelConfirmed: map[string]bool{},
			executions: w.gone[a(0)]}
		delete(w.gone, a(0))
		delete(w.orphanSeen, a(0))
	case "stopThenFail":
		// a stop reaches the robot of a live action, and its adapter reports the action failed,
		// before or after the pause is written
		act := w.actions[a(0)]
		if act == nil || act.bound == "" || (act.phase != fleetv1.ActionPhaseAssigned && act.phase != fleetv1.ActionPhaseInProgress) {
			return false, nil
		}
		r := w.robots[act.bound]
		if r == nil {
			return false, nil
		}
		zone := r.curZone
		if zone == "" {
			zone = r.specZone
		}
		w.apply(op{"triggerZone", []string{zone}})
		w.fanOut()
		w.apply(op{"failRace", []string{act.name, a(1)}})
	case "deleteActionThenRobot":
		// the action is deleted, then the robot that recorded it
		act := w.actions[a(0)]
		if act == nil || act.bound == "" {
			return false, nil
		}
		r := act.bound
		w.apply(op{"deleteAction", []string{act.name, a(1)}})
		if a(1) == "outage" {
			return w.apply(op{"deleteRobotOutage", []string{r}})
		}
		return w.apply(op{"deleteRecorder", []string{r}})
	case "adapterFail":
		// the robot's adapter reports the action failed: a stop closed its gate, or a fault
		act := w.actions[a(0)]
		if act == nil || act.bound == "" || (act.phase != fleetv1.ActionPhaseAssigned && act.phase != fleetv1.ActionPhaseInProgress) {
			return false, nil
		}
		r := w.robots[act.bound]
		if r == nil || (r.runningAction != act.name && !r.stopped) {
			return false, nil
		}
		w.reportFailed(act, r)
	case "failRace":
		// the adapter's failure report and the estop pause race; the pause is computed from the
		// copy read before the report was written
		act := w.actions[a(0)]
		if act == nil || act.bound == "" || (act.phase != fleetv1.ActionPhaseAssigned && act.phase != fleetv1.ActionPhaseInProgress) {
			return false, nil
		}
		r := w.robots[act.bound]
		if r == nil || !r.stopped {
			return false, nil
		}
		if a(1) == "failure-first" {
			w.reportFailed(act, r)
			if staleWriteLands() {
				act.phase = fleetv1.ActionPhasePaused // the pause, from the stale copy, over what is stored
			}
		} else {
			act.phase = fleetv1.ActionPhasePaused // the pause, then the report is dropped: the phase is no longer live
			r.runningAction, r.runningUntil = "", time.Time{}
			w.closeExecution(act, r.name)
		}
	case "completeRace":
		// the adapter reports the action succeeded while a requeue is being finalized from the copy
		// read before
		act := w.actions[a(0)]
		if act == nil || act.bound == "" || (act.phase != fleetv1.ActionPhaseAssigned && act.phase != fleetv1.ActionPhaseInProgress) {
			return false, nil
		}
		r := w.robots[act.bound]
		if r == nil || r.runningAction != act.name {
			return false, nil
		}
		r.runningAction, r.runningUntil = "", time.Time{}
		w.closeExecution(act, r.name)
		w.decided[act.name] = true // the robot's own end report
		act.phase = fleetv1.ActionPhaseSucceeded
		act.wasTerminal = act.phase
		n := w.now
		act.completion = &n
		if r.assignedAction == act.name {
			r.assignedAction = ""
		}
		if staleWriteLands() {
			act.phase = fleetv1.ActionPhasePending
			act.bound = ""
			act.horizon = nil
		}
	case "deleteAction":
		// a FleetAction delete has no admission rule; its robot is told nothing and keeps its record
		w.deleteActionAs(a(0), a(1))
	case "recreateRequeue":
		// the action is deleted and created again under its name carrying requeue-requested
		name, who := a(0), a(1)
		if name == "" {
			return false, nil
		}
		if !w.deleteActionAs(name, who) {
			return false, nil
		}
		prev := w.gone[name]
		if executorguard.RequeueChecked(true, false, true) && w.requeueRefused(&action{name: name}, who) {
			return false, nil
		}
		act := &action{name: name, phase: fleetv1.ActionPhasePending, created: w.now, cancelConfirmed: map[string]bool{},
			requeue: &requeueReq{who: who}, executions: prev}
		w.actions[name] = act
		delete(w.gone, name)
		delete(w.orphanSeen, name)
		w.processRequeue(act)
	case "requeueThenStop":
		// the operator requeues a held action with no stop standing; then a stop covers its robot
		act := w.actions[a(0)]
		if act == nil || act.bound == "" || act.requeue != nil {
			return false, nil
		}
		r := w.robots[act.bound]
		if r == nil || len(w.workStops(act)) > 0 {
			return false, nil
		}
		switch act.phase {
		case fleetv1.ActionPhaseAssigned:
			w.apply(op{"start", []string{act.name}})
			w.apply(op{"pause", []string{act.name}})
		case fleetv1.ActionPhaseInProgress:
			w.apply(op{"pause", []string{act.name}})
		}
		if act.phase != fleetv1.ActionPhasePaused || act.bound == "" {
			return false, nil
		}
		w.apply(op{"requeue", []string{act.name, "operator"}})
		zone := r.curZone
		if zone == "" {
			zone = r.specZone
		}
		w.apply(op{"triggerZone", []string{zone}})
		w.fanOut()
		// the controllers run once the robot's lease is past
		w.now = w.now.Add(w.leaseDur + w.skew)
		w.controllers()
	case "start":
		// the adapter reports the action running
		act := w.actions[a(0)]
		if act == nil || act.phase != fleetv1.ActionPhaseAssigned || act.bound == "" {
			return false, nil
		}
		if r := w.robots[act.bound]; r == nil || r.runningAction != act.name {
			return false, nil
		}
		act.phase = fleetv1.ActionPhaseInProgress
		act.started = true
	case "deleteRobotOutage":
		// a Robot delete admitted while no manager replica serves the delete check: the stop
		// finalizer keeps a robot that records work under a stop
		r := w.robots[a(0)]
		if r == nil || !r.stopped {
			return false, nil
		}
		before = snapshot()
		if w.keepsFinalizer(r) {
			r.terminating, r.stopFinalizerOnly = true, true
			return true, before
		}
		w.removeRobot(r)
		delete(before, a(0))
		return true, before
	case "renameCreate":
		// the same work created under a new name: new work by definition
		if act := w.actions[a(0)]; act != nil && w.actions[a(0)+"r"] == nil {
			w.actions[a(0)+"r"] = &action{name: a(0) + "r", phase: fleetv1.ActionPhasePending, created: w.now, cancelConfirmed: map[string]bool{}}
		}
	case "lateConfirm":
		// a robot's adapter confirms its stop late
		if r := w.robots[a(0)]; r != nil && r.stopped && !r.confirmed {
			r.confirmed = true
			r.runningAction, r.runningUntil = "", time.Time{}
		}
	case "dispatch":
		w.dispatch()
	case "pause":
		act := w.actions[a(0)]
		if act == nil || act.bound == "" || (act.phase != fleetv1.ActionPhaseAssigned && act.phase != fleetv1.ActionPhaseInProgress) {
			return false, nil
		}
		r := w.robots[act.bound]
		// a declarative pause: the robot is told to stop
		w.log = append(w.log, act.bound+":cancel:"+act.name)
		if r != nil && w.acks[r.name] {
			r.runningAction, r.runningUntil = "", time.Time{}
			w.closeExecution(act, r.name)
		}
		act.desiredPaused = true
		if act.phase == fleetv1.ActionPhaseInProgress {
			// physically committed: stays bound and Paused; the lease keeps being renewed
			act.phase = fleetv1.ActionPhasePaused
			return false, nil
		}
		// Assigned: the binding is released as the state table says; the robot's record goes
		// only on its acknowledgement (the executor guard releases it otherwise)
		if r != nil && w.acks[r.name] && !executorguard.KeepsRecord(w.covering(r), false) {
			r.assignedAction = ""
		}
		act.phase = fleetv1.ActionPhasePaused
		act.bound = ""
		act.horizon = nil
	case "requeue":
		act := w.actions[a(0)]
		if act == nil || act.phase != fleetv1.ActionPhasePaused || act.requeue != nil {
			return false, nil
		}
		// the update that adds the annotation: work a stop covers needs estop-clear on that stop
		if executorguard.RequeueChecked(false, false, true) && w.requeueRefused(act, a(1)) {
			return false, nil
		}
		act.requeue = &requeueReq{who: a(1)}
		w.processRequeue(act)
	case "cancel":
		// a terminal action is settled before a cancel request is read
		act := w.actions[a(0)]
		if act == nil || terminal(act.phase) {
			return false, nil
		}
		if act.bound != "" {
			// a bound robot is freed only on its acknowledgement; otherwise the cancel waits
			r := w.robots[act.bound]
			w.log = append(w.log, act.bound+":cancel:"+act.name)
			if r == nil || !w.acks[r.name] {
				return false, nil
			}
			r.runningAction, r.runningUntil = "", time.Time{}
			w.closeExecution(act, r.name)
			if !executorguard.KeepsRecord(w.covering(r), false) {
				r.assignedAction = ""
			}
			act.bound = ""
		}
		act.phase = fleetv1.ActionPhaseCancelled
		n := w.now
		act.completion = &n
		// the robots that still record it are released only through the executor guard
		w.executorGate(act, false)
	case "regenerate":
		act := w.actions[a(0)]
		if act == nil {
			return false, nil
		}
		// delete: the robot is told nothing and keeps running on its lease; the same name is created again
		if !w.deleteActionAs(a(0), "operator") {
			return false, nil
		}
		w.actions[a(0)] = &action{name: a(0), phase: fleetv1.ActionPhasePending, created: w.now, cancelConfirmed: map[string]bool{},
			executions: w.gone[a(0)]}
		delete(w.gone, a(0))
	case "advance":
		n, _ := strconv.Atoi(a(0))
		w.now = w.now.Add(time.Duration(n) * time.Second)
		// robots whose own lease ran out stop on their own
		for _, r := range w.robots {
			if r.runningAction != "" && !w.now.Before(r.runningUntil) {
				r.runningAction, r.runningUntil = "", time.Time{}
			}
		}
	case "leaseChange":
		n, _ := strconv.Atoi(a(0))
		w.leaseDur = time.Duration(n) * time.Second
	case "deleteConfig":
		// refused while the config carries a stop (and the finalizer holds it through an outage)
		if w.config && !w.nsTrig && !w.nsMarker {
			w.config = false
			w.leaseDur = 30 * time.Second
		}
	case "tick":
		// the controllers run: renewals, the lease decisions for bound actions, the release of the
		// records of a Paused action no robot is bound to, onFailure retries, standing requeues,
		// the wait for a person of work on a robot that never confirmed its stop, and the records of
		// actions that no longer exist
		w.controllers()
		for _, act := range w.sortedActions() {
			if act.phase == fleetv1.ActionPhasePaused && act.bound == "" {
				w.executorGate(act, false)
			}
			if act.bound != "" && (act.phase == fleetv1.ActionPhaseAssigned || act.phase == fleetv1.ActionPhaseInProgress) {
				r := w.robots[act.bound]
				if r != nil && !r.stopped {
					h := w.now.Add(w.leaseDur)
					act.horizon = &h
					r.runningUntil = h
					for i := range act.executions {
						if act.executions[i].robot == r.name && act.executions[i].end.After(w.now) {
							act.executions[i].end = h
						}
					}
					w.log = append(w.log, r.name+":renew:"+act.name)
				}
			}
		}
	}
	return false, nil
}

// clearStop releases key from every robot that records it; a robot no other stop holds resumes.
func (w *world) clearStop(key string) {
	for _, r := range w.robots {
		delete(r.records, key)
		if len(r.records) == 0 && !r.trigger && !r.marker {
			r.stopped = false
			r.confirmed = false
			r.commandsSinceCovered = nil
			for name, h := range w.stopHeld {
				if h == r.name {
					delete(w.stopHeld, name)
				}
			}
		}
	}
}

// reportFailed is the ingestor's write of an adapter's failure report for act, bound to r: a stop
// outcome (Paused, the binding kept) when stops cover the robot, else Failed. The adapter has
// ended the action either way.
func (w *world) reportFailed(act *action, r *robot) {
	if len(w.oracleCovers(r)) > 0 {
		w.stopHeld[act.name] = r.name // the stop closed the adapter's gate
	}
	r.runningAction, r.runningUntil = "", time.Time{}
	w.closeExecution(act, r.name)
	if executorguard.StopOutcome(w.coveringNames(r)) {
		act.phase = fleetv1.ActionPhasePaused
		return
	}
	act.phase = fleetv1.ActionPhaseFailed
	act.wasTerminal = act.phase
	n := w.now
	act.completion = &n
}

// requeueRefused is the requeue check: work whose robot (the bound one, or every one that records
// it) a stop covers is requeued only by a holder of estop-clear on that stop, the admin here.
func (w *world) requeueRefused(act *action, who string) bool {
	return len(w.workStops(act)) > 0 && who != "admin"
}

// processRequeue is the controller handling a standing requeue request on act: held while stops
// cover its robot that did not cover it when the request was first handled; otherwise the
// confirmed-stop gate for a bound action, then Pending, the covered recorders moved on proof.
func (w *world) processRequeue(act *action) {
	rq := act.requeue
	stops := w.workStops(act)
	if !executorguard.PausedRequeueRuns(len(stops) > 0, rq.seen, rq.seen && strings.Join(rq.seenUnder, ",") == strings.Join(stops, ",")) {
		return // held while the stop stands
	}
	if !rq.seen {
		rq.seen, rq.seenUnder = true, stops
	}
	if act.bound != "" {
		r := w.robots[act.bound]
		w.log = append(w.log, act.bound+":cancel:"+act.name)
		dead := act.horizon != nil && !w.now.Before(act.horizon.Add(w.skew))
		if r != nil && !w.acks[r.name] && !dead {
			return // waits for the acknowledgement or the lease
		}
		if r != nil {
			if w.acks[r.name] {
				r.runningAction, r.runningUntil = "", time.Time{}
				w.closeExecution(act, r.name)
			}
			if r.assignedAction == act.name {
				r.assignedAction = ""
			}
		}
		act.bound = ""
		act.horizon = nil
	}
	act.phase = fleetv1.ActionPhasePending
	n := w.now
	act.pendingSince = &n
	act.requeue = nil
	w.decided[act.name] = true // an operator's requeue
	if rq.who == "admin" {
		act.authorized = true
		delete(w.stopHeld, act.name)
	}
	// the requeue moves covered recorders on proof
	w.executorGate(act, true)
}

// controllers is one pass of the controllers over the actions.
func (w *world) controllers() {
	// the stop finalizer goes once nothing holds it: a deleted action with no stopped robot
	// recording it, and a robot deleted while held once it is not
	for _, act := range w.sortedActions() {
		if act.terminating && !w.heldBy(act.name) {
			w.gone[act.name] = act.executions
			delete(w.actions, act.name)
		}
	}
	for _, r := range w.sortedRobots() {
		if r.stopFinalizerOnly && !w.keepsFinalizer(r) {
			w.removeRobot(r)
		}
	}
	for _, act := range w.sortedActions() {
		if act.terminating {
			continue // a deleted action is only kept, never moved
		}
		switch {
		case act.phase == fleetv1.ActionPhaseFailed:
			// onFailure Requeue: the bound robot is released (it reported the end); the robots that
			// record the action through the executor guard; then Pending, three times at most
			if act.retries >= 3 {
				continue
			}
			if r := w.robots[act.bound]; r != nil && r.assignedAction == act.name {
				r.assignedAction = ""
			}
			act.bound = ""
			if !w.executorGate(act, false) {
				continue
			}
			act.retries++
			w.decided[act.name] = true // the robot reported the end
			act.phase = fleetv1.ActionPhasePending
			act.horizon, act.completion = nil, nil
			n := w.now
			act.pendingSince = &n
			act.legalExit = true
		case act.requeue != nil:
			w.processRequeue(act)
		}
		// a robot that never confirmed its stop, whose lease is provably dead: the work stays with
		// it and waits for a person, started or not (invariant I1)
		act.awaitsPerson = false
		if r := w.robots[act.bound]; r != nil && act.bound != "" && !terminal(act.phase) {
			dead := act.horizon != nil && !w.now.Before(act.horizon.Add(w.skew))
			act.awaitsPerson = executorguard.WaitsForAPerson(w.coveringNames(r), r.confirmed, dead, act.desiredPaused && act.requeue == nil)
		}
	}
	// the records of actions that no longer exist: released on the acknowledgement, or once the
	// longest lease plus the skew has passed since the controller first saw them orphaned
	for _, r := range w.sortedRobots() {
		name := r.assignedAction
		if name == "" || w.actions[name] != nil {
			continue
		}
		first, ok := w.orphanSeen[name]
		if !ok {
			first = w.now
			w.orphanSeen[name] = first
		}
		gone := &action{name: name, created: first, cancelConfirmed: map[string]bool{}, executions: w.gone[name]}
		w.executorGate(gone, false)
		w.gone[name] = gone.executions // an acknowledged cancel ends the robot's execution of it
	}
}

// ── The checks ──

func (w *world) check(step int, o op, admitted bool, before map[string]map[string]bool) error {
	// 1. every robot a stop in force covers is held, and no command but a stop reached it since.
	for _, r := range w.sortedRobots() {
		inForce := w.oracleInForce(r)
		if len(inForce) == 0 {
			continue
		}
		if !r.stopped && w.passJustRan {
			// a fan-out or late-arrival pass has just run: every robot a stop in force covers is
			// stopped by it, a robot being deleted included
			return fmt.Errorf("step %d %s: robot %s is covered in force by %v and is not stopped after a pass", step, o, r.name, keys(inForce))
		}
		if r.runningAction != "" && r.stopped && r.confirmed {
			return fmt.Errorf("step %d %s: robot %s is stopped and still executes %s", step, o, r.name, r.runningAction)
		}
	}
	// 2. no action has two executors whose lease windows overlap.
	for _, a := range w.sortedActions() {
		for i := range a.executions {
			for j := i + 1; j < len(a.executions); j++ {
				x, y := a.executions[i], a.executions[j]
				if x.robot == y.robot {
					continue
				}
				if x.start.Before(y.end) && y.start.Before(x.end) {
					return fmt.Errorf("step %d %s: action %s has two executors with overlapping leases: %s [%s,%s) and %s [%s,%s)",
						step, o, a.name, x.robot, x.start.Format("15:04:05"), x.end.Format("15:04:05"), y.robot,
						y.start.Format("15:04:05"), y.end.Format("15:04:05"))
				}
			}
		}
	}
	// 5. no action leaves a terminal phase, except Failed by its onFailure retry.
	for _, a := range w.sortedActions() {
		prev, seen := w.phases[a]
		if seen && terminal(prev) && a.phase != prev && (prev != fleetv1.ActionPhaseFailed || !a.legalExit) {
			return fmt.Errorf("step %d %s: action %s left the terminal phase %s for %s", step, o, a.name, prev, a.phase)
		}
		if a.wasTerminal != "" && a.phase != a.wasTerminal && !a.legalExit {
			return fmt.Errorf("step %d %s: action %s left the terminal phase %s for %s", step, o, a.name, a.wasTerminal, a.phase)
		}
	}
	// 6. work a stop interrupted is given to no other robot while the stop stands (found at dispatch).
	if w.violation != "" {
		return fmt.Errorf("step %d %s: %s", step, o, w.violation)
	}
	// 7. no work leaves a robot under a stop it has not confirmed, except by an operator's requeue or
	// the robot's own end report; after the controllers run, such work past its lease waits for a person.
	for _, a := range w.sortedActions() {
		rn, held := w.unconfirmedHeld[a.name]
		r := w.robots[rn]
		if !held || r == nil || w.decided[a.name] || a.terminating || len(w.oracleCovers(r)) == 0 {
			continue
		}
		// a terminal phase is an end report or an operator's cancel, not a move
		if a.phase == fleetv1.ActionPhasePending || a.bound != "" && a.bound != rn {
			return fmt.Errorf("step %d %s: action %s left robot %s, under a stop it has not confirmed (%v), for %q bound to %q "+
				"with no operator's requeue and no end report", step, o, a.name, rn, keys(w.oracleCovers(r)), a.phase, a.bound)
		}
	}
	if o.kind == "tick" {
		for _, a := range w.sortedActions() {
			r := w.robots[a.bound]
			if r == nil || a.bound == "" || terminal(a.phase) || !r.stopped || r.confirmed || len(w.oracleCovers(r)) == 0 ||
				(a.desiredPaused && a.requeue == nil) || a.terminating {
				continue
			}
			if a.horizon != nil && !w.now.Before(a.horizon.Add(w.skew)) && !a.awaitsPerson {
				return fmt.Errorf("step %d %s: action %s is bound to robot %s, whose stop is not confirmed and whose lease "+
					"was provably dead at %s, and does not wait for a person", step, o, a.name, r.name, a.horizon.Add(w.skew).Format("15:04:05"))
			}
		}
	}
	// 4. every issued stop is in force after a fan-out pass.
	if o.kind == "fanOut" {
		for _, z := range w.sortedZones() {
			if z.trigger && !z.marker {
				return fmt.Errorf("step %d %s: the stop of zone/%s is issued and still not in force after a fan-out pass", step, o, z.name)
			}
		}
		if w.config && w.nsTrig && !w.nsMarker {
			return fmt.Errorf("step %d %s: the namespace stop is issued and still not in force after a fan-out pass", step, o)
		}
		for _, r := range w.sortedRobots() {
			if r.trigger && !r.marker {
				return fmt.Errorf("step %d %s: the stop of robot/%s is issued and still not in force after a fan-out pass", step, o, r.name)
			}
		}
	}
	// 3. no admitted edit took a covered robot out of a stop's reach.
	if admitted && before != nil {
		for name, was := range before {
			r := w.robots[name]
			if r == nil {
				continue
			}
			now := w.oracleCovers(r)
			for k := range was {
				if !now[k] {
					kind, cname, _ := strings.Cut(k, "/")
					// a stop cleared by the edit itself cannot be lost by it; only a stop that still stands counts
					if w.stopStands(kind, cname) {
						return fmt.Errorf("step %d %s: the admitted edit took robot %s out of the reach of %s", step, o, name, k)
					}
				}
			}
		}
	}
	return nil
}

func (w *world) stopStands(kind, name string) bool {
	switch kind {
	case "zone":
		z := w.zones[name]
		return z != nil && (z.trigger || z.marker)
	case "namespace":
		return w.config && (w.nsTrig || w.nsMarker)
	case "robot":
		r := w.robots[name]
		return r != nil && (r.trigger || r.marker)
	}
	return false
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── The driver ──

func runSequence(seed int64, ops []op) (failure error) {
	w := newWorld(seed)
	for i, o := range ops {
		w.holdSnapshot()
		admitted, before := w.apply(o)
		w.passJustRan = o.kind == "fanOut" || o.kind == "lateArrivals"
		if err := w.check(i, o, admitted, before); err != nil {
			return err
		}
		w.phases = map[*action]fleetv1.ActionPhase{}
		for _, a := range w.actions {
			w.phases[a] = a.phase
			if terminal(a.phase) {
				a.wasTerminal = a.phase
			} else if a.legalExit {
				a.wasTerminal = ""
			}
			a.legalExit = false
		}
	}
	return nil
}

// holdSnapshot records, before an operation, the work bound to a robot under a stop it has not
// confirmed (check 7), and starts the operation with nothing decided.
func (w *world) holdSnapshot() {
	w.unconfirmedHeld, w.decided = map[string]string{}, map[string]bool{}
	for _, a := range w.sortedActions() {
		r := w.robots[a.bound]
		if r == nil || a.bound == "" || terminal(a.phase) || a.terminating || !r.stopped || r.confirmed || len(w.oracleCovers(r)) == 0 {
			continue
		}
		w.unconfirmedHeld[a.name] = r.name
	}
}

func generate(seed int64) []op {
	rng := rand.New(rand.NewSource(seed))
	w := newWorld(seed)
	ops := make([]op, 0, opsPerSequence)
	for i := 0; i < opsPerSequence; i++ {
		o := w.randomOp(rng)
		ops = append(ops, o)
		w.apply(o) // so later operations name zones and robots that exist
	}
	return ops
}

// shrink removes operations while the failure persists (greedy, one pass per round).
func shrink(seed int64, ops []op) []op {
	changed := true
	for changed {
		changed = false
		for i := 0; i < len(ops); i++ {
			cand := append(append([]op{}, ops[:i]...), ops[i+1:]...)
			if runSequence(seed, cand) != nil {
				ops = cand
				changed = true
				i--
			}
		}
	}
	return ops
}

func TestGeneratedStopInvariant(t *testing.T) {
	n := defaultSequences
	if *longRun {
		n = longSequences
	}
	if s := os.Getenv("SWARMADA_STOP_INVARIANT_SEQUENCES"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			n = v
		}
	}
	base := int64(20261005)
	if s := os.Getenv("SWARMADA_STOP_INVARIANT_SEED"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			base = v
		}
	}
	failures := 0
	for i := 0; i < n; i++ {
		seed := base + int64(i)
		ops := generate(seed)
		if err := runSequence(seed, ops); err != nil {
			failures++
			small := shrink(seed, ops)
			t.Errorf("VIOLATION seed=%d: %v\nshrunk sequence (%d of %d operations):\n  %s", seed, runSequence(seed, small),
				len(small), len(ops), opsString(small))
			if failures >= 3 {
				t.Fatalf("stopping after %d violations", failures)
			}
		}
	}
	t.Logf("RESULT %d sequences of %d operations, %d violations (base seed %d)", n, opsPerSequence, failures, base)
}

func opsString(ops []op) string {
	parts := make([]string, len(ops))
	for i, o := range ops {
		parts[i] = o.String()
	}
	return strings.Join(parts, "\n  ")
}
