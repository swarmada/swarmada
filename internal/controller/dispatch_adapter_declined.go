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
	"fmt"
	"unicode/utf8"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// An adapter's reason for declining an action, on the action (RFC-0001 #fleetaction-the-payload-field).
//
// Assignment-time validation (§9.2.3) asks a candidate's adapter whether it can serve the
// concrete action, and an adapter that cannot says why. That reason used to reach the
// manager's log only: the action stayed Pending with a tally that said a robot passed every
// filter and no adapter would serve it, so an operator could see THAT a robot refused but not
// which one, nor why — and a refusal on screen has to say who refused.
//
// The LAST decline is carried as condition AdapterDeclined=True, naming the adapter, the robot
// and the adapter's own words, and announced by one Warning Event per new reason. It is
// written on the transition only (RA-1): a Pending action requeues on a timer for as long as it
// is refused, and the same reason on every pass writes nothing. Nothing here changes retry or
// placement: the next candidate is still tried, the action stays Pending, retryCount is not
// touched.

// conditionAdapterDeclined is the condition type, and the reason of the condition and of its
// Warning Event.
const conditionAdapterDeclined = "AdapterDeclined"

// maxDeclineReason bounds the adapter's words on the action. They come from outside the
// control plane, and a condition message longer than the API allows would refuse the whole
// status write, the Pending message with it.
const maxDeclineReason = 1024

// adapterDecline is one adapter's servable=false answer, with its reason.
type adapterDecline struct {
	Adapter, Robot, Reason string
}

func declineMessage(d *adapterDecline) string {
	why := d.Reason
	if len(why) > maxDeclineReason {
		why = why[:maxDeclineReason]
		for !utf8.ValidString(why) { // never half a character
			why = why[:len(why)-1]
		}
		why += "…"
	}
	if why == "" {
		why = "(the adapter gave no reason)"
	}
	return fmt.Sprintf("adapter %q declined this action for robot %q: %s", d.Adapter, d.Robot, why)
}

// setAdapterDeclined records the last decline on the action and reports whether that is a
// transition: a decline whose message differs from the one already carried. nil (no adapter
// declined on this pass) leaves the condition as it is and is never a transition.
func setAdapterDeclined(action *fleetv1.FleetAction, d *adapterDecline) bool {
	if d == nil {
		return false
	}
	msg := declineMessage(d)
	if c := apimeta.FindStatusCondition(action.Status.Conditions, conditionAdapterDeclined); c != nil &&
		c.Status == metav1.ConditionTrue && c.Message == msg {
		return false
	}
	apimeta.SetStatusCondition(&action.Status.Conditions, metav1.Condition{
		Type:               conditionAdapterDeclined,
		Status:             metav1.ConditionTrue,
		Reason:             conditionAdapterDeclined,
		Message:            msg,
		ObservedGeneration: action.Generation,
	})
	return true
}
