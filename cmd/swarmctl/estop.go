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

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/cli"
	"github.com/swarmada/swarmada/internal/estopscope"
)

func newEstopCommand(o *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "estop",
		Short: "Trigger an emergency stop on a FleetZone",
		Long: `Trigger an emergency stop on a FleetZone (RFC-0001 #safety-estop-scopes): a confirmed
stop of every robot in the zone (and, per estopPolicy, its descendants) via the
estop-trigger custom verb, SelfSubjectAccessReview-gated, so RBAC and the
tamper-evident safety audit log apply. Clearing an estop is the separate,
admin-only top-level command "swarmctl estop-clear". A cleared robot waits in
Resuming, withheld from dispatch, until an operator releases it with
"swarmctl estop resume".`,
	}
	cmd.AddCommand(newEstopTriggerCommand(o))
	cmd.AddCommand(newEstopResumeCommand(o))
	return cmd
}

func newEstopTriggerCommand(o *options) *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:     "trigger <fleetzone> --reason <text>",
		Short:   "Trigger an emergency stop on a FleetZone (custom verb: estop-trigger)",
		Example: `  swarmctl estop trigger zone-aisle-b3 --reason "person detected" --yes`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.runEstopTrigger(cmd.Context(), args[0], reason, yes)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why the estop is being triggered (recorded in the safety audit log)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

func newEstopClearCommand(o *options) *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:   "estop-clear <fleetzone> --reason <text>",
		Short: "Clear an emergency stop on a FleetZone (custom verb: estop-clear, admin-only)",
		Long: `Clear an active zone emergency stop. Each robot in the zone moves to Resuming,
where it stays, withheld from dispatch, until an operator releases it with
"swarmctl estop resume" — nothing releases it automatically. A robot that
another stop still holds is not released by the zone's clear: its own
robot-scope estop (the swarmada.io/estop-triggered annotation on the Robot),
another zone's estop that covers it, or the namespace estop. It stays stopped
until that stop is cleared, and the command names each such robot and the stop
that holds it. Only the
swarmada:admin role holds the estop-clear verb (RFC-0001 #security-permission-matrix): clearing an
estop is a safety decision, so automation and non-admin operators are denied by
the SelfSubjectAccessReview gate. Actions stay operator-gated after a clear.

--reason is required and must not be empty or whitespace; it is written to the
tamper-evident safety audit log (ESTOP_CLEARED).`,
		Example: `  swarmctl estop-clear zone-aisle-b3 --reason "aisle clear, inspected" --yes`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.runEstopClear(cmd.Context(), args[0], reason, yes)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why the estop is being cleared (required, recorded in the safety audit log)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

func (o *options) runEstopTrigger(ctx context.Context, zone, reason string, yes bool) error {
	c, cs, ns, err := o.lifecycleClients()
	if err != nil {
		return err
	}
	return o.estopTrigger(ctx, c, cs, ns, zone, reason, yes)
}

// estopTrigger is the testable core of `swarmctl estop trigger`.
func (o *options) estopTrigger(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, zone, reason string, yes bool) error {
	// 1. FETCH. A zone that is not there is reported as not there — before an
	//    EMERGENCY STOP confirmation is shown for it, and before a denied caller
	//    is told they lack a permission on an object that does not exist. The
	//    order is the package rule documented in lifecycle.go.
	fz := &fleetv1.FleetZone{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: zone}, fz); err != nil {
		return fmt.Errorf("getting fleetzone/%s: %w", zone, err)
	}

	// 2. AUTHORISE.
	if err := cli.RequireVerb(ctx, cs, "estop-trigger", "fleetzones", ns, zone); err != nil {
		return err
	}

	// 3. PROMPT. The zone named here has been verified to exist.
	ok, err := cli.Confirm(o.streams, fmt.Sprintf("Trigger an EMERGENCY STOP on fleetzone %q in %q? This stops every robot in the zone.", zone, ns), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}

	// 4. WRITE, against a re-read. The confirmation above can hold for as long as
	//    an operator takes to answer it, and this is a safety path: the patch is
	//    computed from the zone as it is NOW, not as it was before the pause.
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: zone}, fz); err != nil {
		return fmt.Errorf("getting fleetzone/%s: %w", zone, err)
	}
	// The controller re-fires on a NEW annotation value and is idempotent on the
	// same one; the reason is the value (default "true" when none is given).
	val := reason
	if val == "" {
		val = "true"
	}
	if err := patchAnnotation(ctx, c, fz, annEstopTriggered, val); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(o.streams.Out, "fleetzone.swarmada.io/%s emergency stop triggered.\n", zone)
	return nil
}

func (o *options) runEstopClear(ctx context.Context, zone, reason string, yes bool) error {
	c, cs, ns, err := o.lifecycleClients()
	if err != nil {
		return err
	}
	return o.estopClear(ctx, c, cs, ns, zone, reason, yes)
}

// estopClear is the testable core of `swarmctl estop-clear`. --reason is required
// and must be non-empty (validated here so callers of the core enforce it too);
// it is recorded in the safety audit log as ESTOP_CLEARED.
func (o *options) estopClear(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, zone, reason string, yes bool) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason is required and must not be empty")
	}
	// 1. FETCH — see estopTrigger and the rule in lifecycle.go.
	fz := &fleetv1.FleetZone{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: zone}, fz); err != nil {
		return fmt.Errorf("getting fleetzone/%s: %w", zone, err)
	}
	if err := nothingToClear(fz, zone); err != nil {
		return err
	}

	// 2. AUTHORISE.
	if err := cli.RequireVerb(ctx, cs, "estop-clear", "fleetzones", ns, zone); err != nil {
		return err
	}

	// 3. PROMPT.
	ok, err := cli.Confirm(o.streams, fmt.Sprintf("Clear the emergency stop on fleetzone %q in %q? Its robots move to Resuming and stay there until released with \"swarmctl estop resume\".", zone, ns), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}

	// 4. WRITE, against a re-read. This matters more here than anywhere else in
	//    the package: removeAnnotation is a no-op when the trigger is absent from
	//    the copy it is handed, so clearing from a copy read before the prompt
	//    could report a zone resumed while a trigger raised during the prompt
	//    still stands.
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: zone}, fz); err != nil {
		return fmt.Errorf("getting fleetzone/%s: %w", zone, err)
	}
	if err := nothingToClear(fz, zone); err != nil {
		return err
	}
	// Clearing = removing the trigger annotation; the controller sees the trigger
	// gone (with its processed marker still set) and resumes the zone's robots.
	if err := removeAnnotation(ctx, c, fz, annEstopTriggered); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(o.streams.Out, "fleetzone.swarmada.io/%s emergency stop cleared (reason: %s); its robots are Resuming until released with \"swarmctl estop resume\".\n", zone, reason)
	o.printHeldAfterClear(ctx, c, ns, fz)
	return nil
}

// nothingToClear refuses a clear of a zone that carries no emergency stop: removing an
// annotation that is not there writes nothing, so reporting the stop cleared, its robots
// Resuming and its reason recorded would be untrue. A zone whose trigger is gone but whose
// processed marker stands is being cleared already.
func nothingToClear(fz *fleetv1.FleetZone, zone string) error {
	if _, triggered := fz.Annotations[annEstopTriggered]; triggered {
		return nil
	}
	if _, clearing := fz.Annotations[estopscope.ProcessedAnnotation]; clearing {
		return fmt.Errorf("fleetzone.swarmada.io/%s has no emergency stop to clear: its stop was already cleared and "+
			"the control plane is applying that clear (it still carries %s); nothing was changed", zone, estopscope.ProcessedAnnotation)
	}
	return fmt.Errorf("fleetzone.swarmada.io/%s has no emergency stop to clear (it carries no %s annotation); "+
		"nothing was changed", zone, annEstopTriggered)
}

// printHeldAfterClear names each robot in the cleared zone's scope that another stop still
// holds: its own robot-scope trigger, another zone's stop that covers it, or the
// namespace's stop. The control plane does not release such a robot on this clear; the
// clear of the stop that holds it does (RFC-0001 #safety-estop-scopes). The scope is
// resolved as the control plane resolves it: the zone, and its descendants unless
// estopPolicy.propagateToChildren is false. A read that fails is reported, not guessed past.
func (o *options) printHeldAfterClear(ctx context.Context, c client.Client, ns string, fz *fleetv1.FleetZone) {
	zones, configs, err := standingStops(ctx, c, ns)
	if err != nil {
		_, _ = fmt.Fprintf(o.streams.Err, "could not name the robots another stop still holds: %v\n", err)
		return
	}
	var robots fleetv1.RobotList
	if err := c.List(ctx, &robots, client.InNamespace(ns)); err != nil {
		_, _ = fmt.Fprintf(o.streams.Err, "could not list the robots to name those another stop still holds: %v\n", err)
		return
	}
	parent := make(map[string]string, len(zones))
	for i := range zones {
		parent[zones[i].Name] = zones[i].Spec.ParentZone
	}
	toChildren := fz.Spec.EstopPolicy == nil || fz.Spec.EstopPolicy.PropagatesToChildren()
	inScope := func(zone string) bool {
		if zone == fz.Name {
			return true
		}
		for depth := 0; toChildren && zone != "" && depth < 64; depth++ {
			zone = parent[zone]
			if zone == fz.Name {
				return true
			}
		}
		return false
	}
	for i := range robots.Items {
		r := &robots.Items[i]
		zone := r.Status.CurrentZone
		if zone == "" {
			zone = r.Spec.Zone
		}
		recorded := false
		var others []fleetv1.EstopHold
		for _, e := range r.Status.EstopHeldBy {
			if e.Kind == fleetv1.EstopHoldFleetZone && e.Name == fz.Name {
				recorded = true
				continue
			}
			others = append(others, e)
		}
		if !inScope(zone) && !recorded {
			continue
		}
		// The control plane asks about the robot without this zone's own record, as it
		// does when it applies the clear, so the answer is the stop that still holds it.
		r.Status.EstopHeldBy = others
		by, held := estopscope.StandingStop(r, zones, configs)
		if !held {
			continue
		}
		_, _ = fmt.Fprintf(o.streams.Out, "robot.swarmada.io/%s is not released by this clear: it is still held by %s; %s\n",
			r.Name, by, holderClear(by, r.Name, ns))
	}
}

// standingStops reads the namespace's FleetZones and SwarmadaConfigs, which
// estopscope.StandingStop reads to say which stop holds a robot.
func standingStops(ctx context.Context, c client.Client, ns string) ([]fleetv1.FleetZone, []fleetv1.SwarmadaConfig, error) {
	var zones fleetv1.FleetZoneList
	if err := c.List(ctx, &zones, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing zones: %w", err)
	}
	var configs fleetv1.SwarmadaConfigList
	if err := c.List(ctx, &configs, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("listing SwarmadaConfigs: %w", err)
	}
	return zones.Items, configs.Items, nil
}

// holderClear is the operator's next action: the clear of the stop that holds the robot.
func holderClear(by estopscope.Holder, robot, ns string) string {
	if by.Orphaned {
		return estopscope.OrphanedRelease(robot, ns)
	}
	if by.Clearing {
		return fmt.Sprintf("retry the release once that clear is applied: swarmctl estop resume %s --reason \"<text>\" -n %s", robot, ns)
	}
	switch by.Kind {
	case estopscope.KindRobot:
		return fmt.Sprintf("its own clear: kubectl annotate robot %s %s- -n %s", robot, annEstopTriggered, ns)
	case estopscope.KindZone:
		return fmt.Sprintf("that stop's clear: swarmctl estop-clear %s --reason \"<text>\" -n %s", by.Name, ns)
	default:
		return fmt.Sprintf("that stop's clear: kubectl annotate swarmadaconfig %s %s- -n %s", by.Name, annEstopTriggered, ns)
	}
}

func newEstopResumeCommand(o *options) *cobra.Command {
	var reason string
	var yes bool
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "resume <robot>... --reason <text>",
		Short: "Release robots from Resuming after an estop clear (custom verb: estop-clear, admin-only)",
		Long: `Release robots from the post-clear window (Robot.status.estopState Resuming) back to
Normal (RFC-0001 #safety-estop-state-machine). After an estop clear a robot stays in
Resuming, withheld from dispatch, until an operator releases it here; nothing
releases it automatically.

The control plane refuses the release, and this command prints why, in these cases:

  - another stop still holds the robot: its own robot-scope estop (the
    swarmada.io/estop-triggered annotation on the Robot), a zone's estop that
    covers it, or the namespace estop, whatever else was cleared;
  - the robot has sent no telemetry since the clear;
  - its latest report is not yet in Robot.status, because the status write failed or
    is held by the write rate cap;
  - its own latest report puts it in Error or Offline;
  - a hardware component is reported Failed;
  - a capability that was Active when the stop was taken is not Active now.

A capability that was not Active before the stop does not block the release, nor does
one the robot no longer declares, and one a maintenance window has paused never does.
If the stop left no record of its Active capabilities, which happens when it was taken
before the control plane kept one, every capability that is not paused must be Active.
A refused robot stays in Resuming; run the command again once the cause is resolved.

Releasing a robot is a safety decision, so it takes the same admin-only estop-clear
verb as the clear, checked by a SelfSubjectAccessReview here and again at
admission. --reason is required and is recorded in the tamper-evident safety audit
log (ESTOP_RESUME_CONFIRMED or ESTOP_RESUME_REFUSED).`,
		Example: `  swarmctl estop resume amr-01 amr-02 --reason "inspected, path clear" --yes`,
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cs, ns, err := o.lifecycleClients()
			if err != nil {
				return err
			}
			return o.estopResume(cmd.Context(), c, cs, ns, args, reason, yes, wait)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why the robots are being released (required, recorded in the safety audit log)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	cmd.Flags().DurationVar(&wait, "wait", 30*time.Second, "How long to wait for the control plane's answer for each robot")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

// resumePollInterval is how often estopResume re-reads a robot while waiting for the
// control plane's answer. A variable so tests can shorten it.
var resumePollInterval = 500 * time.Millisecond

// estopResume is the testable core of `swarmctl estop resume`. Each robot is handled
// in turn and on its own: a refusal for one does not stop the others, and the command
// fails if any robot was not released.
func (o *options) estopResume(ctx context.Context, c client.Client, cs kubernetes.Interface, ns string, robots []string, reason string, yes bool, wait time.Duration) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--reason is required and must not be empty")
	}
	var notReleased []string
	for _, name := range robots {
		released, err := o.estopResumeOne(ctx, c, cs, ns, name, reason, yes, wait)
		if err != nil {
			_, _ = fmt.Fprintf(o.streams.Err, "robot.swarmada.io/%s: %v\n", name, err)
		}
		if !released {
			notReleased = append(notReleased, name)
		}
	}
	if len(notReleased) > 0 {
		return fmt.Errorf("%d of %d robot(s) not released: %s", len(notReleased), len(robots), strings.Join(notReleased, ", "))
	}
	return nil
}

func (o *options) estopResumeOne(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, name, reason string, yes bool, wait time.Duration) (bool, error) {
	// 1. FETCH — see estopTrigger and the rule in lifecycle.go.
	robot := &fleetv1.Robot{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, robot); err != nil {
		return false, fmt.Errorf("getting robot/%s: %w", name, err)
	}

	// 2. AUTHORISE. Releasing a robot is the release half of a clear.
	if err := cli.RequireVerb(ctx, cs, "estop-clear", "robots", ns, name); err != nil {
		return false, err
	}

	// A robot another stop still holds is not released, whatever else was cleared; the
	// control plane refuses its release for the same reason.
	if own, held := robot.Annotations[annEstopTriggered]; held {
		return false, fmt.Errorf("still held by its own robot-scope estop (%s=%q, estopState %s); its own clear comes first: kubectl annotate robot %s %s- -n %s",
			annEstopTriggered, own, stateOrNormalCLI(robot.Status.EstopState), name, annEstopTriggered, ns)
	}
	zones, configs, err := standingStops(ctx, c, ns)
	if err != nil {
		return false, fmt.Errorf("could not check which stops still hold the robot: %w", err)
	}
	if by, held := estopscope.StandingStop(robot, zones, configs); held {
		return false, fmt.Errorf("still held by %s; estopState %s; %s",
			by, stateOrNormalCLI(robot.Status.EstopState), holderClear(by, name, ns))
	}

	// A robot outside the window has nothing to release; saying so here, before the
	// prompt, is kinder than a request the control plane will only refuse.
	if robot.Status.EstopState != fleetv1.RobotEstopResuming {
		return false, fmt.Errorf("not in Resuming (estopState %s); only a robot whose estop has been cleared can be released",
			stateOrNormalCLI(robot.Status.EstopState))
	}

	// 3. PROMPT.
	ok, err := cli.Confirm(o.streams, fmt.Sprintf("Release robot %q in %q from Resuming and return it to dispatch?", name, ns), yes)
	if err != nil {
		return false, err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return false, nil
	}

	// 4. WRITE, against a re-read. The value is unique per request, so the control
	//    plane acts on it exactly once and a repeated reason is still a new request.
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, robot); err != nil {
		return false, fmt.Errorf("getting robot/%s: %w", name, err)
	}
	request := time.Now().UTC().Format(time.RFC3339Nano) + " " + reason
	if err := patchAnnotation(ctx, c, robot, fleetv1.EstopResumeAnnotation, request); err != nil {
		return false, err
	}

	// 5. ANSWER. The control plane handles the request once and records that it did;
	//    the robot's estopState then says whether it was released.
	deadline := time.Now().Add(wait)
	for {
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, robot); err != nil {
			return false, fmt.Errorf("getting robot/%s: %w", name, err)
		}
		if robot.Annotations[fleetv1.EstopResumeProcessedAnnotation] == request {
			break
		}
		if !time.Now().Before(deadline) {
			return false, fmt.Errorf("requested; no answer from the control plane within %s (the request stays pending; see: kubectl describe robot %s)", wait, name)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(resumePollInterval):
		}
	}
	if robot.Status.EstopState == fleetv1.RobotEstopNormal {
		_, _ = fmt.Fprintf(o.streams.Out, "robot.swarmada.io/%s released: estopState Normal.\n", name)
		return true, nil
	}
	return false, errors.New(refusalReason(ctx, cs, ns, name, robot.Status.EstopState))
}

// stateOrNormalCLI names an unset estopState as the Normal it means.
func stateOrNormalCLI(s fleetv1.RobotEstopState) fleetv1.RobotEstopState {
	if s == "" {
		return fleetv1.RobotEstopNormal
	}
	return s
}

// refusalReason is the reason the control plane gave, from the robot's most recent
// EstopResumeRefused Event. The Event is the only place the reason is recorded besides
// the safety audit log, so a caller that cannot read Events is pointed at it instead.
func refusalReason(ctx context.Context, cs kubernetes.Interface, ns, name string, state fleetv1.RobotEstopState) string {
	list, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Robot,involvedObject.name=" + name + ",reason=EstopResumeRefused",
	})
	if err != nil || len(list.Items) == 0 {
		return fmt.Sprintf("resume refused; the robot stays %s; the reason is on its EstopResumeRefused Event (kubectl describe robot %s)", state, name)
	}
	latest := list.Items[0]
	for _, e := range list.Items[1:] {
		if e.LastTimestamp.After(latest.LastTimestamp.Time) {
			latest = e
		}
	}
	return latest.Message
}

// lifecycleClients builds the controller-runtime client, the clientset (for the
// SSAR gate), and resolves the namespace — the trio every lifecycle verb needs.
func (o *options) lifecycleClients() (client.Client, kubernetes.Interface, string, error) {
	c, err := o.factory.Client()
	if err != nil {
		return nil, nil, "", err
	}
	cs, err := o.factory.Clientset()
	if err != nil {
		return nil, nil, "", err
	}
	ns, err := o.factory.Namespace()
	if err != nil {
		return nil, nil, "", err
	}
	return c, cs, ns, nil
}
