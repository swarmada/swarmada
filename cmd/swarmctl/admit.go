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
	"fmt"
	"io"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/admission"
	"github.com/swarmada/swarmada/internal/cli"
)

// admitOptions carries the operator overrides for `swarmctl admit`.
type admitOptions struct {
	name         string
	zone         string
	robotClass   string
	adapter      string
	dock         string
	manufacturer string
	model        string
	yes          bool
	force        bool
}

func newAdmitCommand(o *options) *cobra.Command {
	a := &admitOptions{}
	cmd := &cobra.Command{
		Use:   "admit robot <name> --zone <zone>",
		Short: "Admit a discovered robot, or re-admit an admitted one (custom verb: admit)",
		Long: `Admit promotes a staged DiscoveredRobot to a full, schedulable Robot — the
mandatory two-phase provisioning gate (RFC-0001 §9.1.2). The Robot spec is built
from the discovered hardware, optionally merged with a RobotClass template, with
operator flags taking final precedence; the DiscoveredRobot is then removed.

With --force the same command re-admits an already-admitted Robot: it re-applies
a RobotClass template to the live Robot (RFC-0001 §9.1.1), preserving identity
and zone. Re-admit requires --class and takes effect as the controller
reconciles, so drain in-progress actions first.

Authorization goes through the discoveredrobots/admit custom verb: swarmctl
issues a SelfSubjectAccessReview for that verb before acting, so a caller
lacking the grant is denied even if it could otherwise create a Robot.`,
		Example: `  swarmctl admit robot dr-acme-a3f9 --zone zone-aisle-c1 \
    --class acme-picker-v2 --name amr-acme-042
  swarmctl admit robot amr-acme-042 --force --class acme-picker-v2`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.runAdmit(cmd.Context(), args, a)
		},
	}
	f := cmd.Flags()
	f.StringVar(&a.zone, "zone", "", "Zone to assign the admitted robot to (required on first admission)")
	f.StringVar(&a.robotClass, "class", "", "RobotClass template to merge into the robot (required with --force)")
	f.StringVar(&a.name, "name", "", "Name for the admitted Robot (defaults to the DiscoveredRobot name)")
	f.StringVar(&a.adapter, "adapter", "", "FleetAdapter name (required unless --class supplies one)")
	f.StringVar(&a.dock, "dock", "", "Charging dock to assign")
	f.StringVar(&a.manufacturer, "manufacturer", "", "Override the discovered manufacturer")
	f.StringVar(&a.model, "model", "", "Override the discovered model")
	f.BoolVar(&a.yes, "yes", false, "Skip the confirmation prompt")
	f.BoolVar(&a.force, "force", false, "Re-admit an already-admitted Robot (re-apply its RobotClass)")
	return cmd
}

func (o *options) runAdmit(ctx context.Context, args []string, a *admitOptions) error {
	// Optional leading "robot" kind token, per the verb-first grammar
	// (`admit robot <name>`); a bare `admit <name>` is also accepted.
	if robotDef, err := resolveResource("robot"); err == nil {
		args = stripKind(args, robotDef)
	}
	if len(args) != 1 {
		return fmt.Errorf("admit takes exactly one robot name")
	}
	name := args[0]

	// --force re-admits an existing Robot via the preserved reAdmit path.
	if a.force {
		if a.robotClass == "" {
			return fmt.Errorf("--class is required with --force (re-admit re-applies a RobotClass)")
		}
		c, err := o.factory.Client()
		if err != nil {
			return err
		}
		ns, err := o.factory.Namespace()
		if err != nil {
			return err
		}
		return o.reAdmit(ctx, c, ns, name, a.robotClass, a.dock, a.yes)
	}

	// First admission promotes a DiscoveredRobot; a new Robot needs a zone.
	if a.zone == "" {
		return fmt.Errorf("--zone is required to admit a discovered robot")
	}
	c, cs, ns, err := o.lifecycleClients()
	if err != nil {
		return err
	}
	return o.admit(ctx, c, cs, ns, name, a)
}

// admit is the testable core of `swarmctl admit`: SSAR-gate, validate the parameters, then
// MARK the DiscoveredRobot so the control plane creates the Robot and removes the staging
// object.
//
// The CLI used to create the Robot itself and then delete the DiscoveredRobot. That made the
// operator the creator of a schedulable robot, which meant admitting required blanket
// `create` on robots — a permission that bypasses the admission gate (§6.6) entirely — and it
// split one transition across two actors: a delete that failed after a successful create left
// an orphaned staging object behind, reported only as a warning.
//
// Validation still happens here, against the same builder the controller will use, so an
// unusable parameter set is refused synchronously with the error the operator can act on
// rather than surfacing later as a controller-side event.
func (o *options) admit(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, drName string, a *admitOptions) error {
	// Custom-verb gate: authorize `admit` before doing anything, fail closed.
	if err := cli.RequireVerb(ctx, cs, "admit", "discoveredrobots", ns, drName); err != nil {
		return err
	}

	dr := &fleetv1.DiscoveredRobot{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: drName}, dr); err != nil {
		return fmt.Errorf("getting discoveredrobot/%s: %w", drName, err)
	}

	var class *fleetv1.RobotClass
	if a.robotClass != "" {
		class = &fleetv1.RobotClass{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: a.robotClass}, class); err != nil {
			return fmt.Errorf("getting robotclass/%s: %w", a.robotClass, err)
		}
	}

	params := a.params()
	// Build and discard: this is the validation, and it runs through the exact code the
	// controller will run, so a set of parameters that passes here cannot fail to build there
	// for any reason other than the cluster changing underneath it.
	robot, err := admission.BuildRobot(dr, params, class, ns)
	if err != nil {
		return err
	}

	encoded, err := params.Encode()
	if err != nil {
		return err
	}
	base := dr.DeepCopy()
	if dr.Annotations == nil {
		dr.Annotations = map[string]string{}
	}
	dr.Annotations[admission.AdmitAnnotation] = encoded
	if err := c.Patch(ctx, dr, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("marking discoveredrobot/%s admitted: %w", drName, err)
	}

	_, _ = fmt.Fprintf(o.streams.Out,
		"discoveredrobot.swarmada.io/%s admitted into zone %s; the control plane will create robot.swarmada.io/%s\n",
		drName, a.zone, robot.Name)
	return nil
}

// params projects the operator's flags onto the annotation payload. --force/--yes are CLI
// concerns (a different code path and a confirmation prompt); they never reach the controller.
func (a *admitOptions) params() admission.Params {
	return admission.Params{
		Name:         a.name,
		Zone:         a.zone,
		RobotClass:   a.robotClass,
		Adapter:      a.adapter,
		Dock:         a.dock,
		Manufacturer: a.manufacturer,
		Model:        a.model,
	}
}

// --- reject (invoked by `delete robot` on a discovered target) ----------------

// reject is the testable core of the discovered-robot reject path: SSAR-gate,
// confirm, delete the DiscoveredRobot, and best-effort record the reason as an
// Event. It is reached via `swarmctl delete robot <name>` when the name resolves
// to a DiscoveredRobot — the `reject` custom verb, distinct from a plain delete.
func (o *options) reject(ctx context.Context, c client.Client, cs kubernetes.Interface, ns, drName, reason string, yes bool) error {
	// Custom-verb gate first, fail closed.
	if err := cli.RequireVerb(ctx, cs, "reject", "discoveredrobots", ns, drName); err != nil {
		return err
	}

	// Reject is destructive: confirm unless --yes.
	ok, err := cli.Confirm(o.streams, fmt.Sprintf("Reject and delete discoveredrobot %q in %q?", drName, ns), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}

	dr := &fleetv1.DiscoveredRobot{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: drName}, dr); err != nil {
		return fmt.Errorf("getting discoveredrobot/%s: %w", drName, err)
	}

	// MARK, then let the control plane delete. The CLI used to delete directly, which made
	// an operator's rejection indistinguishable from a TTL sweep — both ended in the same
	// disappearing object — so ROBOT_REJECTED (§9.6.5.1) could not be recorded without a
	// sweep being able to masquerade as a refusal.
	//
	// The annotation is the discriminating signal, and the controller seals the audit entry
	// before removing the object. It cannot be written here as a chain entry directly: the
	// hash chain is single-writer by construction (internal/audit.Log holds the namespace
	// sequence and previous hash in process), so a second process would fork it. Same
	// division as the other custom verbs — the CLI records intent, the manager records fact.
	base := dr.DeepCopy()
	if dr.Annotations == nil {
		dr.Annotations = map[string]string{}
	}
	dr.Annotations[annRobotRejected] = reason
	if err := c.Patch(ctx, dr, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("marking discoveredrobot/%s rejected: %w", drName, err)
	}

	// Record the rejection reason as a namespace Event too (§9.1.2.7). Best-effort: the
	// mark is the authoritative action, so an events RBAC gap only costs the Event.
	recordRejectionEvent(ctx, cs, ns, dr, reason, o.streams.Err)

	_, _ = fmt.Fprintf(o.streams.Out,
		"discoveredrobot.swarmada.io/%s rejected; the control plane will record and remove it.\n", drName)
	return nil
}

// annRobotRejected marks a DiscoveredRobot as operator-rejected. Mirrors the controller's
// constant of the same name — the two are a contract between the CLI and the manager, and
// changing one without the other silently turns a rejection back into an unexplained delete.
const annRobotRejected = "swarmada.io/rejected"

// recordRejectionEvent emits a namespace Event capturing the rejection reason.
// It is best-effort: a failure (e.g. no RBAC for events) is warned, not fatal.
func recordRejectionEvent(ctx context.Context, cs kubernetes.Interface, ns string, dr *fleetv1.DiscoveredRobot, reason string, warn io.Writer) {
	msg := reason
	if msg == "" {
		msg = "DiscoveredRobot rejected by operator."
	}
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "dr-rejected-", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: fleetv1.GroupVersion.String(),
			Kind:       "DiscoveredRobot",
			Namespace:  ns,
			Name:       dr.Name,
			UID:        dr.UID,
		},
		Reason:         "Rejected",
		Message:        msg,
		Type:           corev1.EventTypeNormal,
		Source:         corev1.EventSource{Component: "swarmctl"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}
	if _, err := cs.CoreV1().Events(ns).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		_, _ = fmt.Fprintf(warn, "warning: could not record rejection Event: %v\n", err)
	}
}
