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
	"strings"
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
	"github.com/swarmada/swarmada/internal/telemetry"
)

// Rows for the estop-hold invariant table: an over-the-air update is dispatch, so a robot a
// stop holds keeps no firmware pull it was told to make before the stop, is pushed no model
// rollback, and enters no batch, until the stop that holds it is cleared and the robot
// resumed (RFC-0001 #firmwarerollout). And a zone's stop, once it has reached a robot, is
// kept across an edit of the zone's geometry that moves the robot's current zone away.
// Every row waits for its stop's processed marker before it clears that stop.

const rolloutChecksum = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// rolloutRig adds what the rollout rows need to the estop rig: signing off, a target robot,
// and the two rollout reconcilers run directly as the manager.
type rolloutRig struct {
	*rig
	pusher *recordingPusher
}

type recordingPusher struct {
	mu     sync.Mutex
	pushes []command.ModelUpdate
}

func (p *recordingPusher) PushModelUpdate(_ context.Context, _, _ string, u command.ModelUpdate) (command.ModelUpdateOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushes = append(p.pushes, u)
	return command.ModelUpdateOutcome{Acknowledged: true}, nil
}

func (p *recordingPusher) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.pushes) }

func newRolloutRig(r *rig) *rolloutRig {
	r.t.Helper()
	r.config()
	cfg := r.configObj()
	r.must("turning signing off", r.update(r.w.admin, cfg, false, func() {
		cfg.Spec.Signing.RequireSignatureVerification = boolp(false)
	}))
	return &rolloutRig{rig: r, pusher: &recordingPusher{}}
}

// target makes robot a rollout target: labelled, Idle, charged.
func (rr *rolloutRig) target(robot, zone string) {
	rr.t.Helper()
	rr.robot(robot, zone)
	rb := rr.robotObj(robot)
	rr.must("labelling "+robot, rr.update(rr.w.admin, rb, false, func() {
		if rb.Labels == nil {
			rb.Labels = map[string]string{}
		}
		rb.Labels["fleet"] = "pickers"
	}))
	pct := int32(90)
	rr.must("charging "+robot, rr.update(rr.w.as(rr.t, controllerUser), rb, true, func() {
		rb.Status.BatteryPercent = &pct
	}))
}

func (rr *rolloutRig) firmwareRollout() {
	rr.t.Helper()
	rr.must("creating the FirmwareRollout", rr.w.admin.Create(context.Background(), &fleetv1.FirmwareRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "fw", Namespace: rr.w.ns},
		Spec: fleetv1.FirmwareRolloutSpec{
			TargetSelector:   metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
			NewVersion:       "2.5.0",
			FirmwareURI:      "oci://registry.example/acme:2.5.0",
			FirmwareChecksum: rolloutChecksum,
		},
	}))
}

func (rr *rolloutRig) modelRollout(auto bool) {
	rr.t.Helper()
	mr := &fleetv1.ModelRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "mr", Namespace: rr.w.ns},
		Spec: fleetv1.ModelRolloutSpec{
			TargetSelector: metav1.LabelSelector{MatchLabels: map[string]string{"fleet": "pickers"}},
			ModelName:      "item-recognition",
			NewVersion:     "3.2.1",
			ModelURI:       "oci://registry.example/models/item-recognition:3.2.1",
			ModelChecksum:  rolloutChecksum,
		},
	}
	if auto {
		mr.Spec.RollbackPolicy = fleetv1.ModelRollbackAuto
	}
	rr.must("creating the ModelRollout", rr.w.admin.Create(context.Background(), mr))
}

// reconcileFirmware runs the FirmwareRollout reconciler once, as the manager, and returns
// the robot's pending-firmware annotation afterwards.
func (rr *rolloutRig) reconcileFirmware(robot string) string {
	rr.t.Helper()
	fr := &controller.FirmwareRolloutReconciler{Client: rr.w.as(rr.t, controllerUser), Scheme: rr.w.scheme,
		Recorder: record.NewFakeRecorder(64)}
	if _, err := fr.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: rr.w.ns, Name: "fw"}}); err != nil {
		rr.t.Logf("firmware reconcile: %v", err)
	}
	return rr.get(robot).Annotations["swarmada.io/pending-firmware-version"]
}

// reconcileModel runs the ModelRollout reconciler once, as the manager, with the rig's
// recording Pusher, and returns how many model_update pushes have been sent so far.
func (rr *rolloutRig) reconcileModel() int {
	rr.t.Helper()
	c := rr.w.as(rr.t, controllerUser)
	mr := &controller.ModelRolloutReconciler{Client: c, APIReader: c, Scheme: rr.w.scheme,
		Recorder: record.NewFakeRecorder(64), Pusher: rr.pusher}
	if _, err := mr.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: rr.w.ns, Name: "mr"}}); err != nil {
		rr.t.Logf("model reconcile: %v", err)
	}
	return rr.pusher.count()
}

// releaseAfter clears carrier's stop, waits for the clear to be applied, and resumes robot.
func (rr *rolloutRig) releaseAfter(carrier client.Object, robot string) {
	rr.t.Helper()
	rr.clearStop(carrier)
	rr.waitMarker(carrier, false)
	if ok, last := rr.waitFor(robot+" Resuming", 10*time.Second, func() (bool, string) {
		s := rr.get(robot).Status.EstopState
		return s == fleetv1.RobotEstopResuming, string(s)
	}); !ok {
		rr.t.Fatalf("setup: %s did not reach Resuming after the clear (last %s)", robot, last)
	}
	ans := rr.resume(robot, "rollout-release")
	if ok, last := rr.waitFor(robot+" Normal", 10*time.Second, func() (bool, string) {
		s := rr.get(robot).Status.EstopState
		return s == fleetv1.RobotEstopNormal || s == "", string(s)
	}); !ok {
		rr.t.Fatalf("setup: %s was not released (resume answer %q, last %s)", robot, ans, last)
	}
}

// eventuallyGot re-runs pass until it returns want, for up to d.
func eventuallyGot[T comparable](d time.Duration, pass func() T, want T) (T, bool) {
	deadline := time.Now().Add(d)
	for {
		got := pass()
		if got == want || time.Now().After(deadline) {
			return got, got == want
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func rolloutRows() []invariantRow {
	var rows []invariantRow

	// ── A robot told to pull firmware is stopped before its install began ──
	for _, scope := range []string{"robot", "zone", "namespace"} {
		scope := scope
		rows = append(rows, invariantRow{name: "rollout-batch-member-stopped/" + scope + "/firmware",
			run: func(t *testing.T, r *rig) {
				rr := newRolloutRig(r)
				r.zone("z", "", nil)
				rr.target("r1", "z")
				rr.firmwareRollout()
				if got, ok := eventuallyGot(5*time.Second, func() string { return rr.reconcileFirmware("r1") }, "2.5.0"); !ok {
					t.Fatalf("setup: r1 was not told to pull firmware (pending-firmware %q)", got)
				}
				var carrier client.Object
				switch scope {
				case "robot":
					carrier = r.robotObj("r1")
				case "zone":
					carrier = r.zoneObj("z")
				default:
					carrier = r.configObj()
				}
				r.trigger(carrier, scope+"-stop")
				r.waitMarker(carrier, true)
				r.waitStopped("r1")
				for i := 0; i < 3; i++ {
					if got := rr.reconcileFirmware("r1"); got != "" {
						t.Errorf("INVARIANT r1: a %s stop holds it and it keeps the pull it was told to make (pending-firmware %q, pass %d)",
							scope, got, i+1)
						break
					}
				}
				rr.releaseAfter(carrier, "r1")
				got, ok := eventuallyGot(5*time.Second, func() string { return rr.reconcileFirmware("r1") }, "2.5.0")
				t.Logf("RESULT after the clear and resume: pending-firmware=%q", got)
				if !ok {
					t.Errorf("r1 was not offered the update again after its stop was cleared and it resumed")
				}
			}})
	}

	// ── A failed robot under a zone stop is not pushed its Auto rollback ──
	rows = append(rows, invariantRow{name: "rollout-batch-member-stopped/zone/model-rollback",
		run: func(t *testing.T, r *rig) {
			rr := newRolloutRig(r)
			r.zone("z", "", nil)
			rr.target("r1", "z")
			rb := r.robotObj("r1")
			r.must("reporting the failed model", r.update(r.w.as(t, controllerUser), rb, true, func() {
				rb.Status.InstalledModels = []fleetv1.InstalledModelStatusEntry{{Name: "item-recognition",
					Status: fleetv1.ModelStatusFailed, FailureReason: "inference health check failed"}}
			}))
			rr.modelRollout(true)
			mr := &fleetv1.ModelRollout{ObjectMeta: metav1.ObjectMeta{Name: "mr", Namespace: r.w.ns}}
			r.must("recording the revert target", r.update(r.w.as(t, controllerUser), mr, true, func() {
				mr.Status.RollbackVersions = map[string]string{"r1": "3.2.0"}
			}))
			r.trigger(r.zoneObj("z"), "z-stop")
			r.waitMarker(r.zoneObj("z"), true)
			r.waitStopped("r1")
			for i := 0; i < 3; i++ {
				if n := rr.reconcileModel(); n != 0 {
					t.Errorf("INVARIANT r1: a zone stop holds it and it was pushed a model_update (%d pushes)", n)
					break
				}
			}
			rr.releaseAfter(r.zoneObj("z"), "r1")
			n, ok := eventuallyGot(5*time.Second, rr.reconcileModel, 1)
			t.Logf("RESULT after the clear and resume: rollback pushes=%d", n)
			if !ok {
				t.Errorf("r1 was not pushed its rollback after its stop was cleared and it resumed (%d pushes)", n)
			}
		}})

	// ── A robot below a stopped zone that does not propagate to its children ──
	rows = append(rows, invariantRow{name: "rollout-entry-under-a-non-propagating-ancestor-stop",
		run: func(t *testing.T, r *rig) {
			rr := newRolloutRig(r)
			r.zone("anc", "", boolp(false))
			r.zone("c", "anc", nil)
			rr.target("r1", "c")
			r.trigger(r.zoneObj("anc"), "anc-stop")
			r.waitMarker(r.zoneObj("anc"), true)
			rr.firmwareRollout()
			rr.modelRollout(false)
			for i := 0; i < 3; i++ {
				fw, pushes := rr.reconcileFirmware("r1"), rr.reconcileModel()
				if fw != "" || pushes != 0 {
					t.Errorf("INVARIANT r1: dispatch withholds it under zone anc's stop and a rollout reached it "+
						"(pending-firmware %q, model_update pushes %d)", fw, pushes)
					break
				}
			}
			r.clearStop(r.zoneObj("anc"))
			r.waitMarker(r.zoneObj("anc"), false)
			fw, ok := eventuallyGot(5*time.Second, func() string { return rr.reconcileFirmware("r1") }, "2.5.0")
			pushes, okm := eventuallyGot(5*time.Second, rr.reconcileModel, 1)
			t.Logf("RESULT after anc's clear: pending-firmware=%q model_update pushes=%d", fw, pushes)
			if !ok || !okm {
				t.Errorf("r1 was not reached by the rollouts after the stop that withheld it was cleared")
			}
		}})

	// ── Control: a stop that does not hold the robot leaves its update alone ──
	rows = append(rows, invariantRow{name: "control-rollout-batch-member-without-a-stop-keeps-its-update",
		run: func(t *testing.T, r *rig) {
			rr := newRolloutRig(r)
			r.zone("z", "", nil)
			r.zone("other", "", nil)
			rr.target("r1", "z")
			rr.firmwareRollout()
			if got, ok := eventuallyGot(5*time.Second, func() string { return rr.reconcileFirmware("r1") }, "2.5.0"); !ok {
				t.Fatalf("setup: r1 was not told to pull firmware (pending-firmware %q)", got)
			}
			r.trigger(r.zoneObj("other"), "other-stop")
			r.waitMarker(r.zoneObj("other"), true)
			for i := 0; i < 3; i++ {
				if got := rr.reconcileFirmware("r1"); got != "2.5.0" {
					t.Fatalf("r1, which no stop holds, lost its pending firmware (%q)", got)
				}
			}
		}})

	// ── A geometry edit of a stopped zone moves the robot's current zone away ──
	for _, edit := range []string{"shrink", "floor", "frame-local"} {
		edit := edit
		rows = append(rows, invariantRow{name: "polygon-edit-under-a-stop-keeps-the-hold/" + edit,
			run: func(t *testing.T, r *rig) { geometryEditRow(t, r, edit) }})
	}
	return rows
}

func square(x0, y0, x1, y1 float64) []fleetv1.Point {
	return []fleetv1.Point{{X: x0, Y: y0}, {X: x1, Y: y0}, {X: x1, Y: y1}, {X: x0, Y: y1}}
}

// geometryEditRow: robot r1, declared in zone out, reports a position inside zone z, so its
// current zone is z. z is stopped, which reaches r1. Then z's polygon, floor or frame is
// edited so that r1's next frames derive another current zone. The stop is kept: r1 stays
// Stopped, its release is refused naming zone z, and it is given no new work.
func geometryEditRow(t *testing.T, r *rig, edit string) {
	ctx := context.Background()
	for name, poly := range map[string][]fleetv1.Point{"z": square(-10, -10, 10, 10), "out": square(20, 20, 40, 40)} {
		r.must("creating zone "+name, r.w.admin.Create(ctx, &fleetv1.FleetZone{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.w.ns},
			Spec:       fleetv1.FleetZoneSpec{PhysicalBounds: &fleetv1.PhysicalBounds{Polygon: poly}},
		}))
	}
	rb := markerRobot(r.w.ns, "r1", map[string]string{controller.RobotIDAnnotation: "r1-wire"})
	rb.Spec.Zone = "out"
	r.must("creating r1", r.w.admin.Create(ctx, rb))
	holdSetPhase(t, r.w, "r1", fleetv1.RobotPhaseIdle, false)

	zc := &controller.ZoneController{Client: r.w.as(t, controllerUser), Scheme: r.w.scheme,
		Recorder: record.NewFakeRecorder(1024)}
	reindex := func() {
		for _, z := range []string{"z", "out"} {
			if _, err := zc.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: r.w.ns, Name: z}}); err != nil {
				t.Logf("zone controller reconcile %s: %v", z, err)
			}
		}
	}
	at := func(x, y float64) string {
		zc.ObservePosition(ctx, telemetry.Frame{RobotID: "r1-wire", Namespace: r.w.ns,
			Position: &telemetry.Position{X: x, Y: y, Floor: 0}})
		return r.get("r1").Status.CurrentZone
	}
	reindex()
	if cz := at(0, 0); cz != "z" {
		t.Fatalf("setup: r1's frame at (0,0) did not derive zone z (currentZone %q)", cz)
	}
	r.trigger(r.zoneObj("z"), "z-stop")
	r.waitMarker(r.zoneObj("z"), true)
	r.waitStopped("r1")

	z := r.zoneObj("z")
	r.must("editing zone z under its stop", r.update(r.w.admin, z, false, func() {
		switch edit {
		case "shrink":
			z.Spec.PhysicalBounds.Polygon = square(-10, -10, -5, -5)
		case "floor":
			z.Spec.PhysicalBounds.Floor = 1
		default:
			z.Spec.PhysicalBounds.Frame = &fleetv1.CoordinateFrame{Alignment: fleetv1.FrameFrameLocal}
		}
	}))
	reindex()
	away := at(0, 0)
	moved := at(30, 30)
	t.Logf("RESULT %s: currentZone at (0,0) after the edit %q, at (30,30) %q", edit, away, moved)
	if away == "z" || moved != "out" {
		t.Fatalf("setup: the edit did not move r1's current zone away from z (at (0,0) %q, at (30,30) %q)", away, moved)
	}

	r.ownRelease("r1")
	ans := r.resume("r1", "geometry-release")
	t.Logf("RESULT %s: release answer %s", edit, ans)
	if !strings.HasPrefix(ans, "EstopResumeRefused") || !strings.Contains(ans, "zone z") {
		t.Errorf("INVARIANT r1: after the edit its release was not refused naming zone z: %q", ans)
	}
	r.assertHeld("r1", true)
}
