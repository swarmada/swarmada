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
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/executorguard"
	"github.com/swarmada/swarmada/internal/safety"
)

// RobotDeletePath is where the Robot delete guard is served.
const RobotDeletePath = "/validate-swarmada-io-v1-robot-delete"

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-robot-delete,mutating=false,failurePolicy=ignore,sideEffects=None,groups=swarmada.io,resources=robots,verbs=delete,versions=v1,name=vrobotdelete.swarmada.io,admissionReviewVersions=v1

// RobotDeleteGuard refuses to delete a robot whose status holds a stop: an estop state
// other than Normal, the Error phase, or a Failed component (safety.HeldBy). All three
// live only in Robot.status, so a robot deleted and created again, by hand or by
// auto-admit after Offline auto-removal, came back Normal and dispatchable, with no clear,
// no resume and no audit record (RFC-0001 #safety-estop-state-machine). The refusal holds
// for every identity, the manager's and swarmada:admin's included.
//
// One delete is admitted whatever the robot holds: one in a namespace that is being
// deleted (namespaceTerminating, the FleetZone delete guard's rule). Every object in the
// namespace is going, and a refusal would leave the namespace Terminating. The signal is
// the namespace's deletionTimestamp, not the requester, because which identity the
// namespace controller deletes as depends on how the cluster runs it.
//
// # When the webhook is unavailable
//
// failurePolicy=Ignore, and it is the only Robot rule that is: DELETE on robots is all
// this webhook matches, and vrobot.swarmada.io, which keeps Fail, no longer does. Under
// Fail, while no manager replica served, no Robot delete was admitted, and a namespace
// delete reported success and stayed Terminating until a replica came back; the exemption
// above runs inside the webhook, so it could not help. Under Ignore a manager outage no
// longer blocks a Robot delete. It still blocks a namespace delete whenever the namespace
// holds a FleetZone, FleetTask, FirmwareRollout or ModelRollout: their DELETE checks
// (vfleetzone, vfleettask, vfirmwarerollout, vmodelrollout) stay failurePolicy=Fail, and
// every namespace with a Robot holds a FleetZone. The trade: while no replica serves, a held robot can be
// deleted, by anyone RBAC lets delete it, and created again it comes back with none of
// its stop. The chart renders a literal Ignore, which webhooks.failurePolicy does not
// change.
//
// # A stop issued and not yet applied
//
// A robot whose status holds nothing yet can still be under a stop: its zone's, an
// ancestor's that propagates down to it, or the namespace's, issued (the carrier holds
// swarmada.io/estop-triggered) and not yet applied (no swarmada.io/estop-processed). The
// fan-out finds the robots it stops when it runs. A robot deleted in that window is not
// found, so the physical robot is never sent the stop; created again elsewhere under the
// same robot-id, it comes back outside the stop, dispatchable. Its delete is refused, naming
// the stop, until the stop is applied; the robot then holds it in its status, and the refusal
// above takes over. The zones and the config are read through APIReader; an unreadable one
// refuses the delete.
//
// # A robot that records an action
//
// A robot whose status.assignedAction names an action is not deleted until the executor guard
// has proof that it is off it (refuseLosingAnExecutorRecord): the record is the guard's key, and
// with the robot gone the action, or one created again under its name, would go to a second robot
// while the first may still run it. While no replica serves, this refusal is lost with the rest.
//
// The guard does not hold either where the webhooks are not installed (deploy-minimal,
// the quickstart overlays), nor against an identity that can remove the webhook
// configuration or edit the CRD.
type RobotDeleteGuard struct {
	// APIReader reads the namespace uncached: a namespace deleted a moment ago must read
	// as terminating. cmd/manager wires the manager's API reader.
	APIReader client.Reader
}

// SetupWebhookWithManager serves the guard at RobotDeletePath on the manager's webhook
// server. It is registered by hand, not through the builder, because the builder's path
// for Robot is vrobot's.
func (g *RobotDeleteGuard) SetupWebhookWithManager(mgr ctrl.Manager) error {
	mgr.GetWebhookServer().Register(RobotDeletePath,
		admission.WithCustomValidator(mgr.GetScheme(), &fleetv1.Robot{}, g))
	return nil
}

var _ webhook.CustomValidator = &RobotDeleteGuard{}

// ValidateCreate admits: the guard's configuration matches DELETE only.
func (g *RobotDeleteGuard) ValidateCreate(context.Context, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate admits: the guard's configuration matches DELETE only.
func (g *RobotDeleteGuard) ValidateUpdate(context.Context, runtime.Object, runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete refuses the delete of a held robot outside a terminating namespace.
func (g *RobotDeleteGuard) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	robot, ok := obj.(*fleetv1.Robot)
	if !ok {
		return nil, fmt.Errorf("expected a Robot object but got %T", obj)
	}
	held := safety.HeldBy(robot)
	if held == "" {
		if err := g.refuseUnderPendingStop(ctx, robot); err != nil {
			return nil, err
		}
		return nil, refuseLosingAnExecutorRecord(ctx, g.APIReader, robot, "its delete")
	}
	if g.APIReader == nil {
		return nil, fmt.Errorf("robot delete guard has no reader; refusing to delete %s/%s, which holds a stop (%s)",
			robot.Namespace, robot.Name, held)
	}
	terminating, err := namespaceTerminating(ctx, g.APIReader, robot.Namespace)
	if err != nil {
		return nil, err
	}
	if terminating {
		return nil, nil
	}
	return nil, apierrors.NewForbidden(robotGR, robot.Name, fmt.Errorf(
		"robot %q in namespace %q holds a stop (%s), and a stop lives only in the Robot's status: "+
			"deleted and created again, the robot would come back Normal and dispatchable. It stays held, "+
			"and its delete is refused, until it is released: an estop by an authorized clear and an "+
			"operator's confirmed resume (`swarmctl estop resume`), a Failed component when the robot "+
			"reports it repaired over ControlStream, and Error when the robot reports another phase or "+
			"falls silent and is marked Offline. A robot held by an estop or a Failed component that never "+
			"reports again cannot be released and stays held, and is removed only with its namespace: no "+
			"command decommissions one robot. A delete is admitted while the namespace itself is being deleted "+
			"(RFC-0001 #safety-estop-state-machine)",
		robot.Name, robot.Namespace, held))
}

// refuseUnderPendingStop refuses the delete of a robot a pending stop reaches: a stop of its
// zone (status.currentZone and spec.zone, both, as the fan-out may read either), of an
// ancestor that propagates down to it, or of the namespace. A namespace being deleted is
// exempt, as above.
func (g *RobotDeleteGuard) refuseUnderPendingStop(ctx context.Context, robot *fleetv1.Robot) error {
	if g.APIReader == nil {
		return fmt.Errorf("robot delete guard has no reader; refusing to delete %s/%s unchecked for a stop still "+
			"being applied", robot.Namespace, robot.Name)
	}
	stops, inForce, err := coveringStopsNamed(ctx, g.APIReader, robot)
	if err != nil || len(stops) == 0 {
		return err
	}
	terminating, err := namespaceTerminating(ctx, g.APIReader, robot.Namespace)
	if err != nil {
		return err
	}
	if terminating {
		return nil
	}
	if len(inForce) > 0 {
		// Covered where it stands by a stop in force that has not reached it yet (it arrived a
		// moment ago, or is being deleted): deleted now, it would never be sent the stop.
		return apierrors.NewForbidden(robotGR, robot.Name, fmt.Errorf(
			"robot %q in namespace %q is under the emergency stop of %s, which is in force: deleted now, the robot "+
				"would never be sent the stop, and created again it would come back outside it. The stop reaches it "+
				"where it stands; its delete is refused until the stop is released "+
				"(RFC-0001 #safety-estop-state-machine)",
			robot.Name, robot.Namespace, strings.Join(inForce, ", ")))
	}
	return apierrors.NewForbidden(robotGR, robot.Name, fmt.Errorf(
		"robot %q in namespace %q is under the emergency stop of %s, which is issued and not yet applied (it "+
			"carries swarmada.io/estop-triggered and no swarmada.io/estop-processed): deleted now, the robot "+
			"would never be sent the stop, and created again it would come back outside it. Retry once the stop "+
			"is applied; the robot then holds it in its status, and its delete is refused until it is released "+
			"(RFC-0001 #safety-estop-state-machine)",
		robot.Name, robot.Namespace, strings.Join(stops, ", ")))
}

// refuseLosingAnExecutorRecord refuses what (a Robot delete, or the removal of its last
// finalizer) while the robot records an action and the executor guard has no proof that it is
// off it. The record (status.assignedAction) is the guard's key: once a hold has released the
// binding, or once the action is deleted and created again under its name, it is the only one,
// and with the Robot gone the next dispatch would give the action to a second robot while the
// first may still run it (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee).
// The decision is the guard's (executorguard.DeleteLosesKey), read from the objects: the
// action's horizon plus the widest skew margin, or, with no horizon, the longest lease plus that
// margin from its reference time. An action that no longer exists is no proof: the robot may
// still run it, and anyone who may delete a FleetAction could otherwise make the exemption true
// and then create the action again under its name. The control plane releases such a record
// itself, on the adapter's acknowledgement of the cancel it sends or once the longest lease plus
// the skew margin has passed. A namespace being deleted is exempt, as for every refusal of this
// guard. The refusal says what releases the record (executorRecordRefusal).
func refuseLosingAnExecutorRecord(ctx context.Context, rd client.Reader, robot *fleetv1.Robot, what string) error {
	name := robot.Status.AssignedAction
	if name == "" {
		return nil
	}
	if rd == nil {
		return fmt.Errorf("robot delete guard has no reader; refusing %s of %s/%s, which records FleetAction %q, unchecked",
			what, robot.Namespace, robot.Name, name)
	}
	var action fleetv1.FleetAction
	if err := rd.Get(ctx, client.ObjectKey{Namespace: robot.Namespace, Name: name}, &action); err != nil {
		if !apierrors.IsNotFound(err) {
			return apierrors.NewInternalError(fmt.Errorf("reading FleetAction %q, which robot %q records: %w", name, robot.Name, err))
		}
		if loses, _ := executorguard.DeleteLosesKey(robot.Name, executorguard.Input{ActionGone: true}); !loses {
			return nil
		}
		terminating, err := namespaceTerminating(ctx, rd, robot.Namespace)
		if err != nil || terminating {
			return err
		}
		return apierrors.NewForbidden(robotGR, robot.Name, fmt.Errorf(
			"robot %q in namespace %q records FleetAction %q, which no longer exists, and may still be running it: "+
				"%s would remove the record that keeps the action, or one created again under its name, from a second "+
				"robot. It is refused until the control plane releases the record: when the robot's adapter acknowledges "+
				"the cancel it is sent, or, failing that, once the longest lease the configuration admits plus the "+
				"clock-skew margin has passed since the control plane saw the action gone "+
				"(RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee)",
			robot.Name, robot.Namespace, name, what))
	}
	now := time.Now()
	loses, dead := executorguard.DeleteLosesKey(robot.Name, executorguard.Input{
		Horizon:   executorguard.HorizonOf(&action),
		Reference: executorguard.ReferenceOf(&action),
		Now:       now,
		Skew:      executorguard.MaxSkew,
	})
	if !loses {
		return nil
	}
	terminating, err := namespaceTerminating(ctx, rd, robot.Namespace)
	if err != nil {
		return err
	}
	if terminating {
		return nil
	}
	return apierrors.NewForbidden(robotGR, robot.Name, fmt.Errorf("%s",
		executorRecordRefusal(robot.Name, robot.Namespace, name, action.Status.AssignedRobot, what, dead)))
}

// executorRecordRefusal is the refusal refuseLosingAnExecutorRecord returns. It names the step
// that releases the record and promises no time that can move:
//   - the action is bound to this robot: while the robot is reachable the controller renews its
//     lease on every pass, so a time named at one attempt would be later at the next, and no
//     cancel need have been sent. The operator cancels or requeues the action first.
//   - the action is bound to no robot and the guard computes when the lease is provably dead
//     (from its recorded horizon, or, with none, from its reference time plus the longest
//     lease): nothing renews that, so the time is named, with the adapter's acknowledgement.
//   - the guard has no time (dead is zero: a Paused action whose hold released the horizon), or
//     the action is bound to another robot (its horizon is that robot's lease, not this one's):
//     only the adapter's word (an acknowledged cancel, or a reported end) is sure to release the
//     record, so no time is named.
func executorRecordRefusal(robot, namespace, action, boundTo, what string, dead time.Time) string {
	if boundTo == robot {
		return fmt.Sprintf("robot %q in namespace %q records FleetAction %q, which is bound to it: "+
			"%s would remove the record that keeps the action from a second robot. Cancel or requeue the "+
			"action first: %s is admitted once the robot's adapter acknowledges the cancel, or once the "+
			"action's lease is provably dead. While the robot is reachable its lease is renewed, and waiting "+
			"does not release it (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee)",
			robot, namespace, action, what, what)
	}
	until := "the robot's adapter acknowledges a cancel for the action or reports it ended; this refusal " +
		"names no time, so cancel or requeue the action first"
	if boundTo == "" && !dead.IsZero() {
		until = "the robot's adapter acknowledges a cancel for the action, or until " +
			dead.UTC().Format(time.RFC3339) + ", when the lease the robot last held is provably dead"
	}
	return fmt.Sprintf("robot %q in namespace %q records FleetAction %q and may still be running it: "+
		"%s would remove the record that keeps the action from a second robot. It is refused until %s "+
		"(RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee)",
		robot, namespace, action, what, until)
}
