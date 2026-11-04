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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// conditionZoneStopped is the condition and the Warning Event of an action whose destination is
// in a zone under a stop (RFC-0001 #safety-action-table, row A15).
const conditionZoneStopped = "ZoneStopped"

// holdForStoppedDestination is row A15's destination rule: the control plane MUST NOT dispatch an
// action whose destination, its spec.zone, is in a zone under a stop, that zone or a zone above it
// whose stop propagates to children. Such an action stays Pending with the condition ZoneStopped,
// which holds it back from A15 only: it runs after the deadline check (A16), and before candidate
// selection, so no robot is chosen and no preemption victim displaced for it. A Warning Event
// ZoneStopped names the zone once per transition into the hold.
//
// A stop counts while its zone carries the trigger, or the processed marker of a stop whose clear
// is not yet applied (estopscope.Carries): the cautious reading, a clear takes the hold away only
// once it has been applied. The tree is read from the API server, as the dispatch holds read it: a
// trigger written a moment ago is in force now. A read that fails, or a chain deeper than the
// walk's bound, holds the action too: the hold fails closed. A namespace stop is not a zone under a
// stop; it withholds every robot already (namespaceStop), so it writes no condition here.
//
// When the destination is no longer under a stop, the condition is removed from the action in
// memory, so the write this pass makes, the commit's included, carries the removal. It reports
// whether the action is held, and the result to return when it is.
func (r *FleetActionReconciler) holdForStoppedDestination(ctx context.Context, action, original *fleetv1.FleetAction) (ctrl.Result, bool, error) {
	if action.Spec.Zone == "" {
		apimeta.RemoveStatusCondition(&action.Status.Conditions, conditionZoneStopped)
		return ctrl.Result{}, false, nil
	}
	var msg string
	tree, _, err := estopscope.ReadWorld(ctx, r.liveReader(), action.Namespace)
	if err == nil {
		var reaching []string
		reaching, err = tree.Reaching(action.Spec.Zone, estopscope.Carries)
		switch {
		case err != nil:
		case len(reaching) == 0:
			apimeta.RemoveStatusCondition(&action.Status.Conditions, conditionZoneStopped)
			return ctrl.Result{}, false, nil
		case len(reaching) == 1 && reaching[0] == action.Spec.Zone:
			msg = fmt.Sprintf("the destination zone %s is under an emergency stop; the action is not dispatched "+
				"until the stop is cleared", action.Spec.Zone)
		default:
			msg = fmt.Sprintf("the destination zone %s is under the emergency stop of %s; the action is not dispatched "+
				"until the stop is cleared", action.Spec.Zone, strings.Join(reaching, ", "))
		}
	}
	if err != nil {
		msg = fmt.Sprintf("the zone tree above the destination zone %s could not be read, so whether a stop reaches it "+
			"is not known; the action is not dispatched until it is (%v)", action.Spec.Zone, err)
	}
	transition := setZoneStopped(action, msg)
	action.Status.Phase = fleetv1.ActionPhasePending
	if transition {
		if perr := r.patchActionStatus(ctx, action, original); perr != nil {
			return ctrl.Result{}, true, fmt.Errorf("holding an action whose destination is under a stop: %w", perr)
		}
		log.FromContext(ctx).Info("action held: its destination is in a zone under a stop", "task", action.Name,
			"zone", action.Spec.Zone, "message", msg)
		if r.Recorder != nil {
			r.Recorder.Event(action, corev1.EventTypeWarning, conditionZoneStopped, msg)
		}
	}
	return ctrl.Result{RequeueAfter: pendingRecheckInterval}, true, nil
}

// setZoneStopped sets the condition ZoneStopped with msg and reports whether that is a transition:
// the condition was absent, not True, or carried another message.
func setZoneStopped(action *fleetv1.FleetAction, msg string) bool {
	if c := apimeta.FindStatusCondition(action.Status.Conditions, conditionZoneStopped); c != nil &&
		c.Status == metav1.ConditionTrue && c.Message == msg {
		return false
	}
	apimeta.SetStatusCondition(&action.Status.Conditions, metav1.Condition{
		Type:               conditionZoneStopped,
		Status:             metav1.ConditionTrue,
		Reason:             conditionZoneStopped,
		Message:            msg,
		ObservedGeneration: action.Generation,
	})
	return true
}
