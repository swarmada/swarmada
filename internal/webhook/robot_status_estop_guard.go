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
	"net/http"
	"sort"

	"k8s.io/apimachinery/pkg/api/equality"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// RobotStatusEstopPath is where the robots/status estop guard is served.
const RobotStatusEstopPath = "/validate-swarmada-io-v1-robot-status"

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-robot-status,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=robots/status,verbs=update,versions=v1,name=vrobotstatus.swarmada.io,admissionReviewVersions=v1

// RobotStatusEstopGuard refuses a robots/status write that does not come from the
// control plane's own identity when it changes one of the robot's safety inputs:
//
//   - status.estopState, status.estopBaseline or status.estopHeldBy, the estop state
//     machine and the record of which zone and namespace stops hold the robot;
//   - status.currentZone, the zone the robot is in, which decides which zone stops reach
//     it: the fan-out, the late-arrival stop and the dispatch hold read it;
//   - status.phase, when it moves the robot out of Error, or into or out of Maintenance
//     or Offline;
//   - a status.hardware entry reported Failed, removed or changed;
//   - status.capabilities;
//   - the inputs the operator's resume derives the capabilities from: the state of any
//     status.hardware entry (added, removed or changed), the status of any
//     status.installedModels entry, and status.modelGrantedCapabilities.
//
// The estop state and baseline have one writer: the estop dispatcher (RFC-0001
// #safety-estop-state-machine). The dispatcher leaves Stopped only on an authorized
// clear, and leaves Resuming only when an operator confirms the resume and the resume
// checks pass, with an audit entry. The hold record is written by the zone- and
// namespace-estop controllers when a stop is issued to the robot, and removed only by
// that stop's clear; while it stands the robot is withheld and its release refused. The
// zone the robot is in decides which zone stops reach it, and the control plane derives
// it from the robot's own position reports. The other three are what those checks and the
// scheduler read: candidate filter 1 refuses a robot in Error and filter 3 reads its
// capabilities (RFC-0001 #control-plane-scheduler), and the resume check refuses while a
// component is Failed or a capability that was Active at the stop is not. That check
// derives the capabilities again from the stored hardware, models and grants, so those
// are guarded as well: a Degraded component written Healthy made the resume pass on a
// value the robot never reported. The resume check reads three phases too: it refuses
// while the control plane has marked the robot Error or Offline, and it derives every
// pauseable capability Paused, which does not hold the release, while the robot is in
// Maintenance; a forged Maintenance let a resume pass with a capability that was Active
// at the stop now Degraded. The control plane writes all of them, from the robot's own
// reports over ControlStream, from adapter liveness, from the maintenance windows it runs
// and from the model rollouts it runs.
//
// robots/status is written by more than the control plane: swarmada:admin holds update
// and patch on it, and so may any role an installer adds. (swarmada:robot, the Fleet
// Adapter's role, no longer does: an adapter reports over ControlStream.) Without this
// guard one status patch from such a holder moved a stopped robot to Normal, took a robot
// still reporting Error out of the halt, or cleared the Failed component a resume check
// was holding on, past the checks and the audit record.
//
// Every other status field is left alone: battery, connectivity, position other than the
// zone it places the robot in, a hardware entry's fields other than its state, a model's
// fields other than its status, and phase changes that do not leave Error and neither
// enter nor leave Maintenance or Offline. A write into Error is admitted: it holds the
// robot, and the resume check refuses on it.
//
// # When the webhook is unavailable
//
// failurePolicy=Fail, like the estop marker guard. Ignore would admit every write while
// the webhook server is unreachable, an estop release included, and an outage is exactly
// when nobody is watching. The chart renders Ignore only when an operator also sets
// webhooks.acknowledgeFailOpenSecurity, the break-glass that opens every estop validator.
//
// The rule matches UPDATE on robots/status and nothing else. Robot creates, spec,
// label and annotation writes, and estop and resume requests on the Robot object go to
// the Robot webhooks, and every other kind to its own. So an outage refuses only
// robots/status updates, and only while no manager replica is serving: the webhook
// server runs on every replica, not only the leader. The manager is the only in-tree
// writer of robots/status, since adapters' telemetry reaches it over ControlStream,
// so no in-tree write is lost. A client that writes robots/status directly (an
// out-of-tree adapter, kubectl) is refused until a replica is back.
//
// Why not a ValidatingAdmissionPolicy, which the API server evaluates itself and so has
// no availability gap: the rule has to name the manager's own principal, and a policy
// can only carry it written down, in its CEL or a parameter, at install time. This
// webhook reads it at run time from the manager's own token. A written-down name is right
// for one install: the kustomize namespace and name transformers do not rewrite a
// string inside CEL, and a Helm release not named "swarmada" renders a different
// ServiceAccount. That is the defect the estop marker guard records against a fixed
// name. No policy ships in config/ or the chart either, so it would be a second
// admission mechanism to install and test for one rule.
//
// It admits by controllerPrincipal, which cmd/manager reads from its own mounted token;
// webhooks run only in-cluster, where that token exists. With no principal, every write
// of these fields is refused, the manager's own included, and cmd/manager logs that at
// startup.
type RobotStatusEstopGuard struct {
	decoder *admission.Decoder
}

// SetupWebhookWithManager serves the guard on the manager's webhook server.
func (g *RobotStatusEstopGuard) SetupWebhookWithManager(mgr ctrl.Manager) error {
	g.decoder = admission.NewDecoder(mgr.GetScheme())
	mgr.GetWebhookServer().Register(RobotStatusEstopPath, &webhook.Admission{Handler: g})
	return nil
}

// Handle admits a robots/status update that leaves the safety inputs as they were, or
// that comes from the controller's ServiceAccount, and refuses every other.
func (g *RobotStatusEstopGuard) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.SubResource != "status" {
		return admission.Allowed("")
	}
	var oldRobot, newRobot fleetv1.Robot
	if err := g.decoder.DecodeRaw(req.OldObject, &oldRobot); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decoding the stored Robot: %w", err))
	}
	if err := g.decoder.DecodeRaw(req.Object, &newRobot); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decoding the Robot: %w", err))
	}
	if isControllerServiceAccount(req.UserInfo.Username) {
		return admission.Allowed("")
	}
	if oldRobot.Status.EstopState == newRobot.Status.EstopState &&
		equality.Semantic.DeepEqual(oldRobot.Status.EstopBaseline, newRobot.Status.EstopBaseline) {
		if !equality.Semantic.DeepEqual(oldRobot.Status.EstopHeldBy, newRobot.Status.EstopHeldBy) {
			return admission.Denied(fmt.Sprintf("user %q may not change status.estopHeldBy of Robot %q in namespace %q: "+
				"the control plane records there each zone and namespace emergency stop issued to the robot, and removes "+
				"a record on the clear of a stop on a carrier of the same kind and name (the stop that wrote it, or the "+
				"stop of a carrier created again under that name) or, when its carrier is gone or has another UID, on a "+
				"robot-scope stop and clear (RFC-0001 #safety-estop-scopes)",
				req.UserInfo.Username, req.Name, req.Namespace))
		}
		if !equality.Semantic.DeepEqual(oldRobot.Status.Stops, newRobot.Status.Stops) {
			return admission.Denied(fmt.Sprintf("user %q may not change status.stops of Robot %q in namespace %q: "+
				"the control plane records there each stop it sent the robot, by its stop ID, and removes an entry only "+
				"once the adapter has acknowledged the stop's clear (RFC-0001 #safety-robot-stop-table)",
				req.UserInfo.Username, req.Name, req.Namespace))
		}
		return safetyInputRefusal(req, &oldRobot, &newRobot)
	}
	return admission.Denied(fmt.Sprintf("user %q may not change status.estopState or status.estopBaseline of Robot %q "+
		"in namespace %q (estopState %s -> %s): the control plane writes the estop state, and a robot leaves an "+
		"estop only through a clear authorized by the estop-clear verb and an operator's confirmed resume "+
		"(`swarmctl estop resume`; RFC-0001 #safety-estop-state-machine)",
		req.UserInfo.Username, req.Name, req.Namespace,
		stateOrUnset(oldRobot.Status.EstopState), stateOrUnset(newRobot.Status.EstopState)))
}

// safetyInputRefusal refuses a write, from any identity but the controller's, that changes
// the robot's zone, moves it out of Error, removes or changes a Failed hardware entry, or
// changes the capabilities, and admits every other. Each refusal names the field and how
// the robot leaves the state it guards.
func safetyInputRefusal(req admission.Request, oldRobot, newRobot *fleetv1.Robot) admission.Response {
	who := fmt.Sprintf("user %q may not change", req.UserInfo.Username)
	where := fmt.Sprintf("of Robot %q in namespace %q", req.Name, req.Namespace)
	if oldRobot.Status.AssignedAction != newRobot.Status.AssignedAction {
		return admission.Denied(fmt.Sprintf("%s status.assignedAction %s (%q -> %q): the control plane writes it when it "+
			"gives the robot an action and clears it only once the robot is provably off that action (its adapter "+
			"acknowledged the cancel, reported the action ended, or the lease is provably dead); it is what keeps a "+
			"second robot from being given work the first may still be running "+
			"(RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee)",
			who, where, oldRobot.Status.AssignedAction, newRobot.Status.AssignedAction))
	}
	if boundPhase(oldRobot.Status.Phase) && oldRobot.Status.Phase != newRobot.Status.Phase {
		return admission.Denied(fmt.Sprintf("%s status.phase %s (phase %s -> %s): the control plane writes a bound robot's "+
			"phase, and candidate filter 1 reads it; a robot that still records an action leaves %s only once it is "+
			"provably off that action (RFC-0001 #safety-assignment-lease-and-the-single-executor-guarantee)",
			who, where, oldRobot.Status.Phase, robotPhaseOrUnset(newRobot.Status.Phase), oldRobot.Status.Phase))
	}
	if oldRobot.Status.CurrentZone != newRobot.Status.CurrentZone {
		return admission.Denied(fmt.Sprintf("%s status.currentZone %s (%q -> %q): the control plane writes it from the "+
			"robot's own position reports, and it decides which zone emergency stops reach the robot "+
			"(RFC-0001 #safety-estop-scopes)", who, where, oldRobot.Status.CurrentZone, newRobot.Status.CurrentZone))
	}
	if oldRobot.Status.Phase == fleetv1.RobotPhaseError && newRobot.Status.Phase != fleetv1.RobotPhaseError {
		to := string(newRobot.Status.Phase)
		if to == "" {
			to = "unset"
		}
		return admission.Denied(fmt.Sprintf("%s status.phase %s (phase Error -> %s): a robot leaves Error only "+
			"when it reports another phase over ControlStream, or when it falls silent and the control plane marks "+
			"it Offline; until then candidate filter 1 withholds it from dispatch and an operator's resume "+
			"(`swarmctl estop resume`) is refused "+
			"(RFC-0001 #control-plane-scheduler, #safety-estop-state-machine)",
			who, where, to))
	}
	if p, ok := resumeReadPhaseChange(oldRobot.Status.Phase, newRobot.Status.Phase); ok {
		return admission.Denied(fmt.Sprintf("%s status.phase %s (phase %s -> %s): the control plane writes %s, "+
			"from adapter liveness and the maintenance windows it runs, and the operator's resume (`swarmctl estop "+
			"resume`) reads it: it refuses while the robot is Offline, and a capability Paused by a maintenance "+
			"window does not hold the release (RFC-0001 #safety-estop-state-machine)",
			who, where, robotPhaseOrUnset(oldRobot.Status.Phase), robotPhaseOrUnset(newRobot.Status.Phase), p))
	}
	for _, was := range oldRobot.Status.Hardware {
		if was.Status != fleetv1.HardwareFailed {
			continue
		}
		now, ok := hardwareEntry(newRobot.Status.Hardware, was.Name)
		if ok && equality.Semantic.DeepEqual(was, now) {
			continue
		}
		return admission.Denied(fmt.Sprintf("%s status.hardware[%s], reported Failed, %s: the control plane writes "+
			"hardware health from the robot's own reports over ControlStream, and a robot under an estop is released "+
			"only by an operator's confirmed resume (`swarmctl estop resume`), which is refused while a component is "+
			"Failed (RFC-0001 #safety-estop-state-machine)", who, was.Name, where))
	}
	if name, was, now, changed := hardwareStateChange(oldRobot.Status.Hardware, newRobot.Status.Hardware); changed {
		return admission.Denied(fmt.Sprintf("%s status.hardware[%s] %s (%s -> %s): the control plane writes hardware "+
			"health from the robot's own reports over ControlStream, and the operator's resume (`swarmctl estop resume`) "+
			"derives the robot's capabilities from it (RFC-0001 #safety-estop-state-machine)", who, name, where, was, now))
	}
	if name, was, now, changed := modelStatusChange(oldRobot.Status.InstalledModels, newRobot.Status.InstalledModels); changed {
		return admission.Denied(fmt.Sprintf("%s status.installedModels[%s].status %s (%s -> %s): the control plane "+
			"writes a model's status from its rollouts and the robot's own reports, and the operator's resume "+
			"(`swarmctl estop resume`) derives the model-driven capabilities from it (RFC-0001 #safety-estop-state-machine)",
			who, name, where, was, now))
	}
	if !equality.Semantic.DeepEqual(oldRobot.Status.ModelGrantedCapabilities, newRobot.Status.ModelGrantedCapabilities) {
		return admission.Denied(fmt.Sprintf("%s status.modelGrantedCapabilities %s: the control plane writes it when a "+
			"model rollout completes, and the operator's resume (`swarmctl estop resume`) counts the capabilities it "+
			"grants (RFC-0001 #safety-estop-state-machine)", who, where))
	}
	if !equality.Semantic.DeepEqual(oldRobot.Status.Capabilities, newRobot.Status.Capabilities) {
		return admission.Denied(fmt.Sprintf("%s status.capabilities %s: the control plane computes it from the "+
			"robot's spec, its installed models and its own reports over ControlStream; candidate filter 3 and the "+
			"operator's resume (`swarmctl estop resume`) read it (RFC-0001 #control-plane-scheduler, "+
			"#safety-estop-state-machine)", who, where))
	}
	return admission.Allowed("")
}

// boundPhase reports whether a phase says the robot holds an action: Assigned or InProgress.
func boundPhase(p fleetv1.RobotPhase) bool {
	return p == fleetv1.RobotPhaseAssigned || p == fleetv1.RobotPhaseInProgress
}

// resumeReadPhases are the phases, other than Error, that the operator's resume reads
// from status.phase: resumeRefusal refuses on Offline (internal/safety/dispatcher.go),
// and DeriveCapabilities pauses every pauseable capability on Maintenance
// (internal/controller/robot_controller.go). Error is guarded on its own, and only on the
// way out.
var resumeReadPhases = []fleetv1.RobotPhase{fleetv1.RobotPhaseMaintenance, fleetv1.RobotPhaseOffline}

// resumeReadPhaseChange names the phase in resumeReadPhases that a change from was to
// now enters or leaves, if there is one.
func resumeReadPhaseChange(was, now fleetv1.RobotPhase) (fleetv1.RobotPhase, bool) {
	if was == now {
		return "", false
	}
	for _, p := range resumeReadPhases {
		if was == p || now == p {
			return p, true
		}
	}
	return "", false
}

// robotPhaseOrUnset names an empty phase.
func robotPhaseOrUnset(p fleetv1.RobotPhase) string {
	if p == "" {
		return "unset"
	}
	return string(p)
}

// hardwareStateChange names the first component, by name, whose state differs between
// was and now, an entry added or removed included, and the two states.
func hardwareStateChange(was, now []fleetv1.HardwareComponentStatus) (name, from, to string, changed bool) {
	before := make(map[string]string, len(was))
	for _, h := range was {
		before[h.Name] = string(h.Status)
	}
	after := make(map[string]string, len(now))
	for _, h := range now {
		after[h.Name] = string(h.Status)
	}
	return firstChange(before, after)
}

// modelStatusChange is hardwareStateChange for status.installedModels.
func modelStatusChange(was, now []fleetv1.InstalledModelStatusEntry) (name, from, to string, changed bool) {
	before := make(map[string]string, len(was))
	for _, m := range was {
		before[m.Name] = string(m.Status)
	}
	after := make(map[string]string, len(now))
	for _, m := range now {
		after[m.Name] = string(m.Status)
	}
	return firstChange(before, after)
}

// firstChange names the first key, in sorted order, whose value differs between before
// and after, reading a missing key as absent.
func firstChange(before, after map[string]string) (name, from, to string, changed bool) {
	names := make([]string, 0, len(before)+len(after))
	for n := range before {
		names = append(names, n)
	}
	for n := range after {
		if _, ok := before[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		b, hadB := before[n]
		a, hasA := after[n]
		if hadB == hasA && b == a {
			continue
		}
		if !hadB {
			b = "absent"
		}
		if !hasA {
			a = "absent"
		}
		return n, b, a, true
	}
	return "", "", "", false
}

// hardwareEntry is the entry for the named component in hw, if there is one.
func hardwareEntry(hw []fleetv1.HardwareComponentStatus, name string) (fleetv1.HardwareComponentStatus, bool) {
	for _, h := range hw {
		if h.Name == name {
			return h, true
		}
	}
	return fleetv1.HardwareComponentStatus{}, false
}

// stateOrUnset names an empty estopState.
func stateOrUnset(s fleetv1.RobotEstopState) string {
	if s == "" {
		return "unset"
	}
	return string(s)
}
