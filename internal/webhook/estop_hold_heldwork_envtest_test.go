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

package webhook

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controller"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/registrar"
	"github.com/swarmada/swarmada/internal/scheduler"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// Rows of the estop hold invariant about the work a stopped robot holds
// (RFC-0001 #safety-fleetaction-behaviour-during-estop, #safety-zonemaintenance-vs-estop-explicit-separation).
//
// Two paths moved held work on with no operator decision:
//
//   - a robot whose action went Revoking (it was unreachable) and was then stopped had the
//     action re-adopted, and a fresh lease pushed to it, when it came back — although the
//     stop may never have reached it;
//   - a ZoneMaintenance window (Immediate, or Graceful past its drain timeout) requeued the
//     action an estop paused, so it left its stopped robot and ran on another one.
//
// Each row drives the real FleetAction and ZoneMaintenance reconcilers, as the control
// plane, with a Commander that records every wire push, alongside the rig's estop
// controllers and dispatcher.

// wireLog records the Commands the FleetAction controller pushes. A cancel is
// acknowledged, so a requeue that reaches the wire completes.
type wireLog struct {
	mu                       sync.Mutex
	assigns, renews, cancels []string // "robot/action"
}

func (w *wireLog) PushAssignAction(_ context.Context, _, robot string, a command.AssignAction) (command.AssignActionOutcome, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.assigns = append(w.assigns, robot+"/"+a.ActionID)
	return command.AssignActionOutcome{Accepted: true, AcceptedFencingToken: a.FencingToken}, nil
}

func (w *wireLog) PushRenewLease(_ context.Context, _, robot string, rl command.RenewLease) (command.RenewLeaseOutcome, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.renews = append(w.renews, robot+"/"+rl.ActionID)
	return command.RenewLeaseOutcome{Renewed: true, Running: true, CurrentGeneration: uint64(rl.LeaseGeneration)}, nil
}

func (w *wireLog) PushCancelAction(_ context.Context, _, robot string, c command.CancelAction) (command.CancelActionOutcome, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancels = append(w.cancels, robot+"/"+c.ActionID)
	return command.CancelActionOutcome{Acknowledged: true, Disposition: command.CancelStoppedSafely}, nil
}

func (w *wireLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return fmt.Sprintf("assign=%v renew=%v cancel=%v", w.assigns, w.renews, w.cancels)
}

func (w *wireLog) count(kind, robot string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	list := map[string][]string{"assign": w.assigns, "renew": w.renews, "cancel": w.cancels}[kind]
	n := 0
	for _, e := range list {
		if len(e) > len(robot) && e[:len(robot)+1] == robot+"/" {
			n++
		}
	}
	return n
}

// work reconciles FleetAction name once, as the control plane, pushing over wl.
func (r *rig) work(wl *wireLog, name string) *fleetv1.FleetAction {
	r.t.Helper()
	ctx := context.Background()
	fr := &controller.FleetActionReconciler{GenerationStoreNamespace: rigStoreNamespace,
		Client: r.w.as(r.t, controllerUser), Scheme: r.w.scheme, Scheduler: scheduler.NewDefaultScheduler(),
		Commander: wl, Recorder: record.NewFakeRecorder(256),
	}
	_, _ = fr.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: name}})
	return r.action(name)
}

func (r *rig) action(name string) *fleetv1.FleetAction {
	r.t.Helper()
	var fa fleetv1.FleetAction
	r.must("reading "+name, r.w.admin.Get(context.Background(), client.ObjectKey{Namespace: r.w.ns, Name: name}, &fa))
	return &fa
}

// holdUnpinned gives robot an unpinned action, InProgress: created while robot is the only
// robot, so any robot may take it once it is Pending again.
func (r *rig) holdUnpinned(wl *wireLog, name, robot string) {
	r.t.Helper()
	r.must("creating "+name, r.w.admin.Create(context.Background(), &fleetv1.FleetAction{
		ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: name},
		Spec:       fleetv1.FleetActionSpec{Type: fleetv1.ActionTypeNavigate},
	}))
	fa := r.action(name)
	for i := 0; i < 20 && (fa.Status.Phase == "" || fa.Status.Phase == fleetv1.ActionPhasePending); i++ {
		fa = r.work(wl, name)
		if fa.Status.Phase == fleetv1.ActionPhasePending {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if fa.Status.Phase != fleetv1.ActionPhaseAssigned || fa.Status.AssignedRobot != robot {
		r.t.Fatalf("setup: %s was not assigned to %s: phase=%q assignedRobot=%q message=%q", name, robot,
			fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.Message)
	}
	a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns}}
	r.must("marking "+name+" InProgress", r.update(r.w.as(r.t, controllerUser), a, true, func() {
		a.Status.Phase = fleetv1.ActionPhaseInProgress
		started := metav1.Now() // as the robot's running report writes it
		a.Status.StartedAt = &started
	}))
}

// revoke takes robot off the network and lets its action enter Revoking.
func (r *rig) revoke(wl *wireLog, name, robot string) {
	r.t.Helper()
	holdSetPhase(r.t, r.w, robot, fleetv1.RobotPhaseOffline, false)
	if fa := r.work(wl, name); fa.Status.Phase != fleetv1.ActionPhaseRevoking {
		r.t.Fatalf("setup: %s did not enter Revoking when %s went Offline: phase=%q message=%q",
			name, robot, fa.Status.Phase, fa.Status.Message)
	}
}

// stopCarrier is the object a scope's stop is triggered on.
func (r *rig) stopCarrier(scope string) client.Object {
	switch scope {
	case "robot":
		return r.robotObj("r1")
	case "namespace":
		return r.configObj()
	}
	return r.zoneObj("z")
}

// window opens a zone-scoped ZoneMaintenance over z and reconciles it to Active. A drain
// timeout already elapsed is simulated by moving status.activatedAt back an hour.
func (r *rig) window(name string, mode fleetv1.ZoneMaintenanceMode, drainExpired bool) {
	r.t.Helper()
	ctx := context.Background()
	r.must("creating window "+name, r.w.admin.Create(ctx, &fleetv1.ZoneMaintenance{
		ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: name},
		Spec: fleetv1.ZoneMaintenanceSpec{
			Scope: fleetv1.ZoneMaintenanceScope{Type: fleetv1.MaintenanceScopeZone, ZoneName: "z"},
			Mode:  mode, Reason: "floor inspection",
		},
	}))
	r.windowPass(name, 2)
	zm := &fleetv1.ZoneMaintenance{ObjectMeta: metav1.ObjectMeta{Namespace: r.w.ns, Name: name}}
	r.must("reading window "+name, r.w.admin.Get(ctx, client.ObjectKeyFromObject(zm), zm))
	if zm.Status.Phase != fleetv1.ZoneMaintenancePhaseActive {
		r.t.Fatalf("setup: window %s is %q, not Active", name, zm.Status.Phase)
	}
	if drainExpired {
		r.must("moving activatedAt back", r.update(r.w.as(r.t, controllerUser), zm, true, func() {
			zm.Status.ActivatedAt = &metav1.Time{Time: time.Now().Add(-time.Hour)}
		}))
	}
	r.windowPass(name, 2)
}

func (r *rig) windowPass(name string, n int) {
	r.t.Helper()
	zr := &controller.ZoneMaintenanceReconciler{Client: r.w.as(r.t, controllerUser), Scheme: r.w.scheme}
	for i := 0; i < n; i++ {
		if _, err := zr.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: name}}); err != nil {
			r.t.Fatalf("window %s reconcile: %v", name, err)
		}
	}
}

// reconnectAck is what the control plane answers the robot's adapter on reconnect.
func (r *rig) reconnectAck(robot string) string {
	reg := &registrar.Registrar{Client: r.w.as(r.t, controllerUser)}
	ack := reg.Register(context.Background(),
		controlstream.AdapterIdentity{Namespace: r.w.ns, ContractCompatible: true},
		&fav1.RegisterRobot{RobotId: robot})
	st := ack.GetAuthoritativeActionState()
	return fmt.Sprintf("accepted=%v action_state=%s action_id=%q", ack.GetAccepted(), st.GetPhase(), st.GetActionId())
}

// assertRevokingHeld: the Revoking action a stop holds is never re-adopted, its lease is
// never pushed, and it stays bound to robot (not released to Pending).
func (r *rig) assertRevokingHeld(wl *wireLog, name, robot string) {
	r.t.Helper()
	var fa *fleetv1.FleetAction
	for i := 0; i < 5; i++ {
		fa = r.work(wl, name)
		r.t.Logf("RESULT %s reconcile %d: phase=%q assignedRobot=%q message=%q wire: %s", name, i+1,
			fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.Message, wl)
		if fa.Status.Phase == fleetv1.ActionPhaseInProgress {
			r.t.Errorf("INVARIANT %s: a stop holds %s and its Revoking action was re-adopted (InProgress)", name, robot)
			break
		}
	}
	if fa.Status.Phase != fleetv1.ActionPhaseRevoking || fa.Status.AssignedRobot != robot {
		r.t.Errorf("INVARIANT %s: a stop holds %s and its action is %q bound to %q, not held at Revoking on it",
			name, robot, fa.Status.Phase, fa.Status.AssignedRobot)
	}
	if n := wl.count("renew", robot); n != 0 {
		r.t.Errorf("INVARIANT %s: a stop holds %s and %d renew_lease were pushed to it", name, robot, n)
	}
}

// assertPausedHeld: the action an estop paused stays Paused on robot, the window wrote no
// requeue request on it, no cancel went to robot, and no robot was given it.
func (r *rig) assertPausedHeld(wl *wireLog, name, robot string, phase fleetv1.ActionPhase) {
	r.t.Helper()
	var fa *fleetv1.FleetAction
	for i := 0; i < 4; i++ {
		fa = r.work(wl, name)
		r.t.Logf("RESULT %s reconcile %d: phase=%q assignedRobot=%q message=%q annotations=%v wire: %s", name, i+1,
			fa.Status.Phase, fa.Status.AssignedRobot, fa.Status.Message, fa.Annotations, wl)
	}
	if fa.Status.Phase != phase || fa.Status.AssignedRobot != robot {
		r.t.Errorf("INVARIANT %s: the work a stop holds on %s is %q bound to %q, not %s on it",
			name, robot, fa.Status.Phase, fa.Status.AssignedRobot, phase)
	}
	if v, ok := fa.Annotations["swarmada.io/requeue-requested"]; ok {
		r.t.Errorf("INVARIANT %s: a requeue request %q stands on the work a stop holds", name, v)
	}
	if n := wl.count("cancel", robot); n != 0 {
		r.t.Errorf("INVARIANT %s: %d cancel_action went to %s to move its held work on", name, n, robot)
	}
	if n := wl.count("assign", "r2"); n != 0 {
		r.t.Errorf("INVARIANT %s: the held work was assigned to r2 (%d)", name, n)
	}
}

func heldWorkRows() []invariantRow {
	var rows []invariantRow

	// ── A Revoking action whose robot is stopped, then reconnects ──
	for _, scope := range []string{"robot", "zone", "namespace"} {
		for _, outcome := range []string{"failed", "stopped"} {
			scope, outcome := scope, outcome
			rows = append(rows, invariantRow{name: "revoking-reconnect/" + scope + "/" + outcome, run: func(t *testing.T, r *rig) {
				wl := &wireLog{}
				r.zone("z", "", nil)
				if scope == "namespace" {
					r.config()
				}
				r.robot("r1", "z")
				r.holdUnpinned(wl, "a-held", "r1")
				r.revoke(wl, "a-held", "r1")
				if outcome == "failed" {
					r.setMode("r1", ackNone) // unreachable: no answer, the delivery window ends Failed
				}
				r.trigger(r.stopCarrier(scope), "stop")
				r.waitStopped("r1")
				t.Logf("RESULT r1 estopState=%s", r.get("r1").Status.EstopState)
				holdSetPhase(t, r.w, "r1", fleetv1.RobotPhaseIdle, false) // the adapter is back
				t.Logf("RESULT reconnect RegisterAck: %s", r.reconnectAck("r1"))
				r.assertRevokingHeld(wl, "a-held", "r1")
				r.assertHeld("r1", true)
			}})
		}
	}
	rows = append(rows, invariantRow{name: "revoking-reconnect/zone/outcome-unwritten", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.revoke(wl, "a-held", "r1")
		r.setMode("r1", ackNone)
		rule := r.rf.add(refuseState("r1", fleetv1.RobotEstopFailed, -1))
		r.trigger(r.zoneObj("z"), "stop")
		if ok, last := r.waitFor("r1 records the zone's stop", 15*time.Second, func() (bool, string) {
			return len(r.get("r1").Status.EstopHeldBy) > 0 && r.rf.hitsOf(rule) > 0, holdsOf(r.get("r1").Status.EstopHeldBy)
		}); !ok {
			t.Fatalf("setup: r1 never recorded the zone's stop with its outcome refused (last %s)", last)
		}
		holdSetPhase(t, r.w, "r1", fleetv1.RobotPhaseIdle, false)
		t.Logf("RESULT r1 estopState=%q estopHeldBy=%s", r.get("r1").Status.EstopState, holdsOf(r.get("r1").Status.EstopHeldBy))
		r.assertRevokingHeld(wl, "a-held", "r1")
		r.assertHeld("r1", false)
	}})
	rows = append(rows, invariantRow{name: "revoking-reconnect/zone/failed/manager-restart-before-reconnect", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.revoke(wl, "a-held", "r1")
		r.setMode("r1", ackNone)
		r.trigger(r.zoneObj("z"), "stop")
		r.waitStopped("r1")
		r.restart()
		holdSetPhase(t, r.w, "r1", fleetv1.RobotPhaseIdle, false)
		r.assertRevokingHeld(wl, "a-held", "r1")
		r.assertHeld("r1", true)
	}})
	rows = append(rows, invariantRow{name: "revoking-lease-dead/zone/failed/after-timeout", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.config()
		cfg := r.configObj()
		r.must("onDisconnect AfterTimeout", r.update(r.w.admin, cfg, false, func() {
			cfg.Spec.ActionCancellation.OnDisconnect = fleetv1.ActionCancellationAfterTimeout
			cfg.Spec.ActionCancellation.DisconnectTimeoutSeconds = int32p(30)
		}))
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.robot("r2", "out")
		r.revoke(wl, "a-held", "r1")
		r.setMode("r1", ackNone)
		r.trigger(r.zoneObj("z"), "stop")
		r.waitStopped("r1")
		// The robot stays unreachable; its lease and the disconnect ceiling are long past.
		a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "a-held", Namespace: r.w.ns}}
		r.must("aging the lease", r.update(r.w.as(t, controllerUser), a, true, func() {
			past := metav1.NewTime(time.Now().Add(-time.Hour))
			a.Status.LeaseExpiresAt = &past
			a.Status.DisconnectedAt = &past
		}))
		r.assertRevokingHeld(wl, "a-held", "r1")
		if n := wl.count("assign", "r2"); n != 0 {
			t.Errorf("INVARIANT a-held: the work a stop holds on r1 was assigned to r2")
		}
	}})
	rows = append(rows, invariantRow{name: "control-revoking-reconnect-no-stop-re-adopts", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.revoke(wl, "a-held", "r1")
		holdSetPhase(t, r.w, "r1", fleetv1.RobotPhaseIdle, false)
		fa := r.work(wl, "a-held")
		t.Logf("RESULT a-held: phase=%q message=%q wire: %s", fa.Status.Phase, fa.Status.Message, wl)
		if fa.Status.Phase != fleetv1.ActionPhaseInProgress || wl.count("renew", "r1") == 0 {
			t.Errorf("control: with no stop a robot back on a live lease re-adopts its action and is renewed")
		}
	}})

	// ── A maintenance window over a stopped robot that holds paused work ──
	for _, c := range []struct {
		name         string
		scope        string
		mode         fleetv1.ZoneMaintenanceMode
		drainExpired bool
	}{
		{name: "zm-immediate/robot", scope: "robot", mode: fleetv1.ZoneMaintenanceModeImmediate},
		{name: "zm-immediate/zone", scope: "zone", mode: fleetv1.ZoneMaintenanceModeImmediate},
		{name: "zm-immediate/namespace", scope: "namespace", mode: fleetv1.ZoneMaintenanceModeImmediate},
		{name: "zm-graceful-drain-expired/zone", scope: "zone", mode: fleetv1.ZoneMaintenanceModeGraceful, drainExpired: true},
	} {
		c := c
		rows = append(rows, invariantRow{name: c.name, run: func(t *testing.T, r *rig) {
			wl := &wireLog{}
			r.zone("z", "", nil)
			r.zone("out", "", nil)
			if c.scope == "namespace" {
				r.config()
			}
			r.robot("r1", "z")
			r.holdUnpinned(wl, "a-held", "r1")
			r.robot("r2", "out") // outside the window, and, for the zone and robot stops, outside the stop
			r.trigger(r.stopCarrier(c.scope), "stop")
			r.waitStopped("r1")
			if fa := r.work(wl, "a-held"); fa.Status.Phase != fleetv1.ActionPhasePaused {
				t.Fatalf("setup: a-held was not paused by the stop: %q", fa.Status.Phase)
			}
			r.window("win", c.mode, c.drainExpired)
			r.assertPausedHeld(wl, "a-held", "r1", fleetv1.ActionPhasePaused)
			r.assertHeld("r1", true)
		}})
	}
	rows = append(rows, invariantRow{name: "zm-requeue-requested-before-the-stop/zone", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.robot("r2", "out")
		// The window marks the running action; the stop lands before the action is reconciled.
		r.window("win", fleetv1.ZoneMaintenanceModeImmediate, false)
		if _, ok := r.action("a-held").Annotations["swarmada.io/requeue-requested"]; !ok {
			t.Fatalf("setup: the window did not mark the running action")
		}
		r.trigger(r.zoneObj("z"), "stop")
		r.waitStopped("r1")
		r.assertPausedHeld(wl, "a-held", "r1", fleetv1.ActionPhasePaused)
		r.windowPass("win", 2) // the window does not mark it again
		r.assertPausedHeld(wl, "a-held", "r1", fleetv1.ActionPhasePaused)
		r.assertHeld("r1", true)
	}})
	rows = append(rows, invariantRow{name: "zm-over-a-revoking-action-under-a-stop/zone", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.robot("r2", "out")
		r.revoke(wl, "a-held", "r1")
		r.setMode("r1", ackNone)
		r.trigger(r.zoneObj("z"), "stop")
		r.waitStopped("r1")
		holdSetPhase(t, r.w, "r1", fleetv1.RobotPhaseIdle, false)
		r.window("win", fleetv1.ZoneMaintenanceModeImmediate, false)
		r.assertPausedHeld(wl, "a-held", "r1", fleetv1.ActionPhaseRevoking)
		r.assertRevokingHeld(wl, "a-held", "r1")
	}})
	rows = append(rows, invariantRow{name: "zm-after-clear-and-resume/zone", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.robot("r2", "out")
		r.trigger(r.zoneObj("z"), "stop")
		r.waitStopped("r1")
		if fa := r.work(wl, "a-held"); fa.Status.Phase != fleetv1.ActionPhasePaused {
			t.Fatalf("setup: a-held was not paused by the stop: %q", fa.Status.Phase)
		}
		r.waitMarker(r.zoneObj("z"), true) // the stop was issued before it is cleared
		r.clearStop(r.zoneObj("z"))
		r.waitMarker(r.zoneObj("z"), false)
		t.Logf("RESULT r1 resume: %s", r.resume("r1", "z-1"))
		if s := r.get("r1").Status.EstopState; s != fleetv1.RobotEstopNormal {
			t.Fatalf("setup: r1 is %q after the clear and the resume", s)
		}
		// The stop is over, but the work it interrupted waits for an operator's decision.
		r.window("win", fleetv1.ZoneMaintenanceModeImmediate, false)
		r.assertPausedHeld(wl, "a-held", "r1", fleetv1.ActionPhasePaused)
	}})

	// ── Controls (green on every tree) ──
	rows = append(rows, invariantRow{name: "control-zm-requeues-a-running-action", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.robot("r2", "out")
		r.window("win", fleetv1.ZoneMaintenanceModeImmediate, false)
		var fa *fleetv1.FleetAction
		for i := 0; i < 6; i++ {
			fa = r.work(wl, "a-held")
		}
		t.Logf("RESULT a-held: phase=%q assignedRobot=%q wire: %s", fa.Status.Phase, fa.Status.AssignedRobot, wl)
		if fa.Status.AssignedRobot != "r2" || wl.count("cancel", "r1") == 0 {
			t.Errorf("control: a window requeues a running action off r1 after a confirmed stop and r2 takes it")
		}
	}})
	rows = append(rows, invariantRow{name: "control-operator-requeue-of-an-estop-paused-action", run: func(t *testing.T, r *rig) {
		wl := &wireLog{}
		r.zone("z", "", nil)
		r.zone("out", "", nil)
		r.robot("r1", "z")
		r.holdUnpinned(wl, "a-held", "r1")
		r.robot("r2", "out")
		r.trigger(r.zoneObj("z"), "stop")
		r.waitStopped("r1")
		if fa := r.work(wl, "a-held"); fa.Status.Phase != fleetv1.ActionPhasePaused {
			t.Fatalf("setup: a-held was not paused by the stop: %q", fa.Status.Phase)
		}
		a := &fleetv1.FleetAction{ObjectMeta: metav1.ObjectMeta{Name: "a-held", Namespace: r.w.ns}}
		r.setAnnotation(a, "swarmada.io/requeue-requested", strp("aisle inspected"))
		var fa *fleetv1.FleetAction
		for i := 0; i < 6; i++ {
			fa = r.work(wl, "a-held")
		}
		t.Logf("RESULT a-held: phase=%q assignedRobot=%q wire: %s", fa.Status.Phase, fa.Status.AssignedRobot, wl)
		if fa.Status.AssignedRobot != "r2" {
			t.Errorf("control: an operator's requeue of estop-paused work moves it to r2 after a confirmed stop")
		}
	}})
	return rows
}

func int32p(v int32) *int32 { return &v }
