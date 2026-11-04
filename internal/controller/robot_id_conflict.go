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
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/robotid"
)

// ConditionRobotIDConflict is True on every Robot whose wire robot-id (robotid.Wire)
// another Robot in the namespace also carries. The dispatch choke point withholds all of
// them (dispatch_holds.go); this condition is how the fleet shows it. It goes False when
// the others are gone.
const ConditionRobotIDConflict = "RobotIDConflict"

const (
	reasonDuplicateRobotID = "DuplicateRobotID"
	reasonRobotIDUnique    = "RobotIDUnique"
)

// reconcileRobotIDConflict sets ConditionRobotIDConflict on robot from the namespace's
// Robots, editing the condition only when its status or the set of Robots it names
// changes, so it rides the material-change patch in Reconcile and is written once per
// transition (RA-1).
//
// It reads the cache. The condition is what an operator reads; the refusal of work is
// decided at dispatch from an uncached list. A twin the cache has not seen yet is caught
// there, and gets this condition on the reconcile its arrival in the cache triggers
// (robotsSharingWireID). A list that fails leaves the condition as it was.
func (r *RobotReconciler) reconcileRobotIDConflict(ctx context.Context, robot *fleetv1.Robot) {
	var robots fleetv1.RobotList
	if err := r.List(ctx, &robots, client.InNamespace(robot.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "listing Robots for the robot-id check; RobotIDConflict left unchanged")
		return
	}
	id := robotid.Wire(robot)
	names := wireIDHolders(robots.Items)[id]
	if !containsString(names, robot.Name) {
		names = append(names, robot.Name) // the cache may not hold this robot yet
	}
	existing := findCondition(robot, ConditionRobotIDConflict)
	if len(names) > 1 {
		msg := fmt.Sprintf("robot-id %q is carried by Robots %s: commands for that id would reach one "+
			"physical robot through either object, so none of them receives work until all but one are "+
			"deleted", id, strings.Join(names, ", "))
		if existing != nil && existing.Status == metav1.ConditionTrue && existing.Message == msg {
			return
		}
		r.setCondition(robot, ConditionRobotIDConflict, metav1.ConditionTrue, reasonDuplicateRobotID, msg)
		if r.Recorder != nil && (existing == nil || existing.Status != metav1.ConditionTrue) {
			r.Recorder.Event(robot, corev1.EventTypeWarning, ConditionRobotIDConflict, msg)
		}
		return
	}
	if existing != nil && existing.Status == metav1.ConditionTrue {
		r.setCondition(robot, ConditionRobotIDConflict, metav1.ConditionFalse, reasonRobotIDUnique,
			fmt.Sprintf("robot-id %q is carried by this Robot alone", id))
	}
}

// robotsSharingWireID maps a Robot event to the other Robots carrying the same wire
// robot-id, so that a twin's create, or its delete, re-evaluates the condition on the
// Robot it collides with. The id cannot change after create, so only creates and deletes
// matter (robotIDHolderChanged).
func (r *RobotReconciler) robotsSharingWireID(ctx context.Context, obj client.Object) []reconcile.Request {
	robot, ok := obj.(*fleetv1.Robot)
	if !ok {
		return nil
	}
	var robots fleetv1.RobotList
	if err := r.List(ctx, &robots, client.InNamespace(robot.Namespace)); err != nil {
		return nil
	}
	id := robotid.Wire(robot)
	var out []reconcile.Request
	for i := range robots.Items {
		other := &robots.Items[i]
		if other.Name != robot.Name && robotid.Wire(other) == id {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: other.Namespace, Name: other.Name}})
		}
	}
	return out
}
