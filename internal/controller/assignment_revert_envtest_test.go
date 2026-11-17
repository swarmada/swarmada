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
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/command"
	"github.com/swarmada/swarmada/internal/controlstream"
	"github.com/swarmada/swarmada/internal/robotid"
	"github.com/swarmada/swarmada/internal/scheduler"
	fav1 "github.com/swarmada/swarmada/proto/fleet_adapter/v1"
)

// assignSender is one adapter ControlStream that accepts every AssignAction it is sent and
// records it.
type assignSender struct {
	mu      sync.Mutex
	d       *command.Dispatcher
	assigns []*fav1.Command
}

func (s *assignSender) Send(msg *fav1.ControlPlaneMessage) error {
	cmd := msg.GetCommand()
	if cmd.GetAssignAction() == nil {
		return nil
	}
	s.mu.Lock()
	s.assigns = append(s.assigns, cmd)
	s.mu.Unlock()
	tok := cmd.GetAssignAction().GetFencingToken()
	go func() {
		_ = s.d.RouteResult(s, &fav1.CommandResult{
			CommandId: cmd.GetCommandId(), RobotId: cmd.GetRobotId(),
			Result: &fav1.CommandResult_AssignAction{AssignAction: &fav1.AssignActionResult{
				Accepted: true, AcceptedFencingToken: &tok,
			}},
		})
	}()
	return nil
}

func (s *assignSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.assigns)
}

// Row A24's second trigger against a real API server and the real command dispatcher, as the
// manager runs it (RequireRegister): an assignment made before the robot's register is processed
// on its adapter's stream is not sent; the action returns to Pending and the Robot is freed at the
// API server within the pass (under 1 s). The robot is then held off the action, so the next pass
// sends nothing, until its register is processed: then the next pass sends it, and it is accepted.
func TestEnvtest_A24_AnAssignmentBeforeTheRegisterRevertsAndIsSentAfterIt(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	// Warm the client first: the first requests a fresh process makes to the API server pay for
	// discovery (2–4 s measured), which the 1 s bound below is not about. One full pass, refused at
	// the send, in a namespace of its own.
	warm := envtestNamespace(t)
	f12SeedAdapter(t, warm)
	zrSeedZone(t, warm, "ward", "", nil, nil, true)
	f12SeedRobot(t, warm, "amr-1", "ward")
	wd := command.New(envK8s)
	wd.APIReader, wd.RequireRegister = envK8s, true
	wd.RegisterStream(controlstream.TLSIdentity{Verified: true, Namespace: warm, AdapterName: "acme-adapter"}, &assignSender{d: wd})
	wr := &FleetActionReconciler{Client: envK8s, APIReader: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: wd}
	if _, err := wr.Reconcile(ctx, zrSeedAction(t, warm, "act-1", "ward")); err != nil {
		t.Fatalf("warm-up reconcile: %v", err)
	}

	ns := envtestNamespace(t)
	f12SeedAdapter(t, ns)
	zrSeedZone(t, ns, "ward", "", nil, nil, true)
	f12SeedRobot(t, ns, "amr-1", "ward")
	req := zrSeedAction(t, ns, "act-1", "ward")

	d := command.New(envK8s)
	d.APIReader = envK8s
	d.RequireRegister = true
	stream := &assignSender{d: d}
	d.RegisterStream(controlstream.TLSIdentity{Verified: true, Namespace: ns, AdapterName: "acme-adapter"}, stream)
	rec := &objectRecorder{}
	r := &FleetActionReconciler{Client: envK8s, APIReader: envK8s, Scheme: envScheme,
		Scheduler: scheduler.NewDefaultScheduler(), Commander: d, Recorder: rec}

	start := time.Now()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	took := time.Since(start)

	act := &fleetv1.FleetAction{}
	if err := envK8s.Get(ctx, req.NamespacedName, act); err != nil {
		t.Fatal(err)
	}
	robot := &fleetv1.Robot{}
	if err := envK8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "amr-1"}, robot); err != nil {
		t.Fatal(err)
	}
	if act.Status.Phase != fleetv1.ActionPhasePending || act.Status.AssignedRobot != "" || act.Status.AssignmentGeneration < 1 {
		t.Fatalf("INVARIANT (A24): an assignment not sent left the action %s on %q at generation %d; want Pending, "+
			"unbound, the generation kept", act.Status.Phase, act.Status.AssignedRobot, act.Status.AssignmentGeneration)
	}
	if robot.Status.AssignedAction != "" || robot.Status.Phase != fleetv1.RobotPhaseIdle {
		t.Fatalf("robot amr-1 records %q in phase %s at the API server; want freed to Idle",
			robot.Status.AssignedAction, robot.Status.Phase)
	}
	if took > time.Second {
		t.Errorf("the revert took %s; want under 1 s", took)
	}
	if n := stream.count(); n != 0 {
		t.Fatalf("%d AssignActions reached the stream before the register; want 0", n)
	}
	if n := len(rec.on("FleetAction", "act-1", EventRobotHeldOff)); n != 1 {
		t.Fatalf("%d RobotHeldOff Events; want 1", n)
	}

	// Held off: the next pass sends nothing and commits nothing.
	gen := act.Status.AssignmentGeneration
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile inside the hold-off: %v", err)
	}
	if err := envK8s.Get(ctx, req.NamespacedName, act); err != nil {
		t.Fatal(err)
	}
	if act.Status.AssignmentGeneration != gen || act.Status.Phase != fleetv1.ActionPhasePending {
		t.Fatalf("pass inside the hold-off: %s at generation %d; want Pending at %d", act.Status.Phase,
			act.Status.AssignmentGeneration, gen)
	}

	// The register is processed: the hold ends, and the next pass sends the assignment.
	d.RobotRegistered(ctx, stream, ns, robotid.Wire(robot))
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile after the register: %v", err)
	}
	if err := envK8s.Get(ctx, req.NamespacedName, act); err != nil {
		t.Fatal(err)
	}
	if stream.count() != 1 || act.Status.Phase != fleetv1.ActionPhaseAssigned || act.Status.AssignedRobot != "amr-1" ||
		act.Status.AssignmentGeneration <= gen {
		t.Fatalf("after the register: %d AssignActions, %s on %q at generation %d; want 1, Assigned to amr-1 above %d",
			stream.count(), act.Status.Phase, act.Status.AssignedRobot, act.Status.AssignmentGeneration, gen)
	}
}
