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

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/contract"
	"github.com/swarmada/swarmada/internal/estopscope"
	"github.com/swarmada/swarmada/internal/robotid"
	"github.com/swarmada/swarmada/internal/safety"
)

// robotGK is the GroupKind reported in admission error responses.
var robotGK = schema.GroupKind{Group: fleetv1.GroupVersion.Group, Kind: "Robot"}

// robotGR is the GroupResource used for SubjectAccessReview attributes on the
// robot-scope estop custom verbs.
var robotGR = schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "robots"}

// +kubebuilder:webhook:path=/validate-swarmada-io-v1-robot,mutating=false,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=robots,verbs=create;update,versions=v1,name=vrobot.swarmada.io,admissionReviewVersions=v1

// +kubebuilder:rbac:groups=swarmada.io,resources=fleetadapters,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=robotclasses,verbs=get;list;watch

// RobotAdmissionGate is the validating webhook that enforces the FleetAdapter
// admission keystone (RFC-0001 #fleetadapter) and the adapter-binding authorization
// check (RFC-0001 #security-fleet-adapter-authentication-robot-identity): a Robot is admissible only when the specific FleetAdapter it
// names in spec.adapter.name
//
//   - exists in the Robot's namespace, AND
//   - serves the Robot's RobotClass (or is a serves-any adapter), AND
//   - is in phase Connected, AND
//   - has Conformance == Passed, AND
//   - earned that result against a contract version this control plane supports
//     (ADR-0032; a missing version counts as unsupported, fail-closed on work).
//
// A different, otherwise-healthy adapter serving the same class does not
// satisfy the gate — authority is anchored on the named binding, not on class
// membership alone, so that revoking an adapter's authority by re-pointing
// spec.adapter (RFC-0001 #security-fleet-adapter-authentication-robot-identity "Revoking an adapter's authority over a robot") actually
// changes admission outcomes.
//
// This is a physical-safety gate, not a hygiene check: admitting a Robot whose
// class has no verified, connected adapter would let unverified software drive
// real hardware. It therefore runs with failurePolicy=Fail — if the gate itself
// cannot run, the Robot is not admitted.
//
// Divergence from vanilla Kubernetes: a Pod is schedulable the moment its own
// spec validates; a Robot is not "real" until a conformant adapter can actually
// drive it. Admission is therefore gated on the live status of a *different*
// resource (the FleetAdapter) rather than on the Robot's own well-formedness —
// closer to a dynamic scheduling predicate than a static schema check.
type RobotAdmissionGate struct {
	Client client.Client
	// APIReader reads the API server directly, bypassing the manager's cache. The zone
	// reads the invariants make (the zone itself, its child zones for the leaf test, its
	// members for the composite test) go through it: a cached list can miss a child zone
	// admitted milliseconds earlier and so admit a Robot into a zone that has just stopped
	// being terminal. Nil falls back to Client, which is what a gate built without a
	// manager (a unit test's fake client) has; cmd/manager wires the uncached reader.
	APIReader client.Reader
	// EstopAuthz authorizes the robot-scope estop-trigger / estop-clear custom verbs
	// when the swarmada.io/estop-triggered annotation is written on a Robot (RFC-0001 #safety-emergency-stop-protocol).
	// A nil authorizer makes any estop-annotation change fail closed; the
	// FleetAdapter admission gate below is unaffected.
	EstopAuthz VerbAuthorizer
}

// SetupWebhookWithManager registers the gate with the manager's webhook server.
// The registered path (/validate-swarmada-io-v1-robot) is derived from the
// group/version/kind and matches the +kubebuilder:webhook marker above.
func (g *RobotAdmissionGate) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.Robot{}).
		WithValidator(g).
		Complete()
}

var _ webhook.CustomValidator = &RobotAdmissionGate{}

// ValidateCreate runs the gate on Robot creation, refuses a create that already
// carries the estop idempotence marker, and authorizes estop-trigger when the Robot
// is created already carrying the estop-triggered annotation.
func (g *RobotAdmissionGate) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	robot, ok := obj.(*fleetv1.Robot)
	if !ok {
		return nil, fmt.Errorf("expected a Robot object but got %T", obj)
	}
	// See FleetZoneValidator.ValidateCreate. Ahead of the gate below for the same
	// reason it is ahead of the adapter early-return on update: the refusal must not
	// depend on anything else about the object resolving first.
	if err := GuardEstopProcessedMarker(ctx, robotGR, robot.Namespace, robot.Name,
		nil, robot.Annotations); err != nil {
		return nil, err
	}
	if err := guardEstopActor(ctx, robotGR, robot.Namespace, robot.Name,
		nil, robot.Annotations, robotEstopActorStampKeys); err != nil {
		return nil, err
	}
	// See FleetZoneValidator.ValidateCreate: a Robot born triggered is stopped.
	if verb, isEstopChange := estopVerbForAnnotations(nil, robot.Annotations); isEstopChange {
		if err := authorizeEstopVerb(ctx, g.EstopAuthz, robotGR, robot.Namespace, robot.Name, verb); err != nil {
			return nil, err
		}
	}
	if resumeRequested(nil, robot.Annotations) {
		if err := authorizeEstopVerb(ctx, g.EstopAuthz, robotGR, robot.Namespace, robot.Name, estopClearVerb); err != nil {
			return nil, err
		}
	}
	warns, err := g.validate(ctx, robot)
	if err != nil {
		return warns, err
	}
	return append(warns, robotUnreadFieldWarnings(nil, robot)...), nil
}

// ValidateUpdate splits the gate in two, because its two halves have different
// reasons to run.
//
// The INVARIANTS (rules 1-3 in validateInvariants) are properties of the Robot
// itself resolved against other objects' specs: a dangling RobotClass or zone, a
// non-leaf zone, an unreachable charging dock, a duplicated swarmada.io/robot-id.
// Nothing about them is transient, so they are re-checked on every update. The
// robot-id rule in particular is the guard that stops the Scheduler dispatching
// two tasks to one physical robot (RFC-0001 #robot-field-validation-rules rule 7) — skipping it on update left
// `kubectl annotate robot B swarmada.io/robot-id=X --overwrite` free to clone an
// identity that create-time admission had already refused.
//
// One invariant is the exception: the refusal of a composite spec.zone (rule 2a)
// runs only when spec.zone changes. A zone can gain members after a robot declares
// it, and for that robot the per-robot estop is the only one that reaches it — the
// zone estop is refused on a composite (FZ-C5). Re-checking the rule on every write
// would refuse that estop too.
//
// One write is exempt from the invariants altogether: an update whose only change is
// to the estop annotations (see onlyEstopAnnotationsChanged). Every invariant resolves
// against other objects, any of which can change after admission, and none of them is
// a reason to refuse a stop.
//
// The ADAPTER BINDING gate (rule 4 in validateAdapterBinding) is different: it
// reads the LIVE status of a FleetAdapter, so re-running it on every write would
// make a transient adapter outage block label and annotation writes on every
// Robot in the namespace — including the annotations an operator reaches for
// during that very outage. It is therefore re-run only when the binding it
// authorizes actually changes (RFC-0001 #security-fleet-adapter-authentication-robot-identity "Revoking an adapter's authority").
//
// Status writes are not a consideration here either way: the webhook is
// registered for resources=robots, not robots/status, and every controller
// writes robot status through Status().Patch on the subresource, which does not
// route to this hook.
func (g *RobotAdmissionGate) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	newRobot, ok := newObj.(*fleetv1.Robot)
	if !ok {
		return nil, fmt.Errorf("expected a Robot object but got %T", newObj)
	}
	oldRobot, ok := oldObj.(*fleetv1.Robot)
	if !ok {
		return nil, fmt.Errorf("expected a Robot object but got %T", oldObj)
	}

	// Ahead of everything else, and for every identity: neither rule depends on who
	// writes or on anything else about the write.
	if err := robotIDChange(oldRobot, newRobot); err != nil {
		return nil, err
	}
	if err := g.lastFinalizerOfAHeldRobot(ctx, oldRobot, newRobot); err != nil {
		return nil, err
	}

	// Authorize a robot-scope estop when the estop-triggered annotation changes
	// Adding or re-valuing needs estop-trigger; removing needs estop-clear.
	// Checked before the adapter-binding early-return below (an estop write leaves
	// class/adapter unchanged, so it would otherwise slip through unauthorized).
	if verb, isEstopChange := estopVerbForAnnotations(oldRobot.Annotations, newRobot.Annotations); isEstopChange {
		if err := authorizeEstopVerb(ctx, g.EstopAuthz, robotGR, newRobot.Namespace, newRobot.Name, verb); err != nil {
			return nil, err
		}
	}
	// An operator's request to release the robot from the post-clear estop window is
	// the release decision itself, so writing it (adding or re-valuing it) takes the
	// same custom verb as the clear. Removing it releases nothing and is not gated.
	if resumeRequested(oldRobot.Annotations, newRobot.Annotations) {
		if err := authorizeEstopVerb(ctx, g.EstopAuthz, robotGR, newRobot.Namespace, newRobot.Name, estopClearVerb); err != nil {
			return nil, err
		}
	}

	// Unconditional, NOT inside the branch above: a forged marker arrives on an
	// update that leaves the estop-triggered annotation alone, so the verb gate is never
	// consulted, and the marker then suppresses the fan-out of the trigger that follows.
	// Ahead of the class/adapter early-return below for the same reason.
	if err := GuardEstopProcessedMarker(ctx, robotGR, newRobot.Namespace, newRobot.Name,
		oldRobot.Annotations, newRobot.Annotations); err != nil {
		return nil, err
	}
	// Unconditional and ahead of the early-return for the same reasons: a rewritten actor
	// arrives on an update that leaves the trigger alone.
	if err := guardEstopActor(ctx, robotGR, newRobot.Namespace, newRobot.Name,
		oldRobot.Annotations, newRobot.Annotations, robotEstopActorStampKeys); err != nil {
		return nil, err
	}

	// The control plane's own writes of metadata alone (the stop finalizer it holds on a
	// carrier, the processed marker, a withdrawn firmware instruction) change nothing the
	// invariants below resolve against, and must not be refused by a zone or class that
	// changed since admission: a refused marker write would issue a stop again for ever.
	if req, err := admission.RequestFromContext(ctx); err == nil && isControllerServiceAccount(req.UserInfo.Username) &&
		onlyMetadataChanged(oldRobot, newRobot) {
		return nil, nil
	}

	// An over-the-air pull to a robot a stop covers is dispatch: a write that adds or changes a
	// swarmada.io/pending-firmware-* key on such a robot is refused (the stop guard).
	if err := g.refuseFirmwareInstructionUnderStop(ctx, oldRobot, newRobot); err != nil {
		return nil, err
	}

	// An emergency stop is never refused for something unrelated to the stop. A write
	// that changes nothing but the estop annotations has been authorized above, and
	// it is the only write that per-robot estop and clear are; if the invariants ran
	// on it, a zone that gained a child, a RobotClass deleted after admission or a dock
	// removed from the zone would each refuse the stop with that invariant's error. The
	// composite rule is scoped to a change of spec.zone for the same reason. RFC-0001
	// #safety-emergency-stop-protocol: a safe stop is always honoured.
	if onlyEstopAnnotationsChanged(oldRobot, newRobot) {
		return nil, nil
	}

	// A Robot being deleted is going, and the only write left to it is the removal of its
	// finalizers. The invariants resolve against other objects, and in a namespace being
	// torn down those go first: a RobotClass deleted before the Robot made every finalizer
	// removal fail rule 1, so the Robot, and with it the namespace, never went. Such a write
	// is admitted without them. The two rules above that do not resolve against other
	// objects still ran: the robot-id cannot move, and a held robot keeps its last
	// finalizer except to the manager or in a terminating namespace.
	if onlyFinalizersChangedWhileDeleting(oldRobot, newRobot) {
		return nil, nil
	}

	// A move out of a zone whose stop is still being applied would leave the robot outside
	// that stop for good (estop_pending_scope.go); every identity is refused alike.
	if err := refuseMoveOutOfPendingStop(ctx, g.zoneReader(), oldRobot, newRobot); err != nil {
		return nil, err
	}

	// Invariants hold on every other write, whatever changed — the composite rule only
	// when spec.zone itself changed (see above).
	if warns, err := g.validateInvariants(ctx, newRobot, oldRobot.Spec.Zone != newRobot.Spec.Zone); err != nil {
		return warns, err
	}

	// Re-run only the liveness-dependent adapter gate when the class or adapter
	// changed; annotation and label updates must not be blocked by a transient adapter.
	unread := robotUnreadFieldWarnings(oldRobot, newRobot)
	if oldRobot.Spec.RobotClass == newRobot.Spec.RobotClass &&
		oldRobot.Spec.Adapter.Name == newRobot.Spec.Adapter.Name {
		return unread, nil
	}
	warns, err := g.validateAdapterBinding(ctx, newRobot)
	if err != nil {
		return warns, err
	}
	return append(warns, unread...), nil
}

// resumeRequested reports whether a write adds or re-values the operator's request to
// release the robot from the post-clear estop window (fleetv1.EstopResumeAnnotation).
func resumeRequested(oldAnn, newAnn map[string]string) bool {
	newVal, has := newAnn[fleetv1.EstopResumeAnnotation]
	if !has {
		return false
	}
	oldVal, had := oldAnn[fleetv1.EstopResumeAnnotation]
	return !had || oldVal != newVal
}

// ValidateDelete admits every delete. vrobot.swarmada.io is not registered for DELETE:
// the Robot delete guard is its own webhook (RobotDeleteGuard), because it runs with a
// different failurePolicy.
func (g *RobotAdmissionGate) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// robotIDChange refuses a write that moves the robot's wire id, robotid.Wire: the
// swarmada.io/robot-id annotation, or the name when it is unset. The registrar, the
// status sink and the command dispatcher route a physical robot's frames and commands to
// the Robot that carries its id, and a stop is held on that Robot's status. Moved off a
// stopped Robot, or onto a fresh one once freed, the id handed the physical robot a clean
// status: Normal and dispatchable, with no clear, no resume and no audit record (RFC-0001
// #safety-estop-state-machine). So the id is fixed at create, for every identity, the
// manager's and system:masters' included; the manager never relabels a robot.
//
// The one change admitted is to an unset id: it may take the robot's name, which is what
// the defaulter backfills and leaves the wire id where it was. Removing the id is refused
// unless it was the name, for the same reason: the wire id would fall back to the name.
//
// Stage A of moving the stop to the robot's identity; the CRD cannot say this itself,
// because x-kubernetes-validations cannot read annotations, so it holds only where the
// webhooks are installed.
func robotIDChange(oldR, newR *fleetv1.Robot) error {
	was := robotid.Wire(oldR)
	now := robotid.Wire(newR)
	if was == now {
		return nil
	}
	return apierrors.NewForbidden(robotGR, newR.Name, fmt.Errorf(
		"%s cannot change after the Robot is created (%q -> %q): it is the identity the control plane routes "+
			"the physical robot's frames and commands by, and a stop is held against the Robot that carries "+
			"it. To give the robot another id, delete the Robot and create it again; the delete of a Robot that "+
			"holds a stop is refused (RFC-0001 #safety-estop-state-machine)",
		fleetv1.RobotIDAnnotation, was, now))
}

// lastFinalizerOfAHeldRobot refuses, from every identity but the manager's, a write that
// removes the last finalizer of a robot that holds a stop (safety.HeldBy), or that a stop still
// being applied reaches (pendingStopsReaching), as the delete guard refuses its delete. The delete
// guard sees a DELETE, and a robot deleted while it held nothing but kept by a finalizer
// stays terminating; stopped after that, it is gone the moment its last finalizer goes,
// and that is an UPDATE. Created again, it came back with a clean status. The manager
// sets no finalizer on a Robot, so this refuses no write of its own.
//
// A finalizer that is not the last one may go: the object stays. And so may the last
// one in a namespace that is being deleted, as the delete guard admits the delete there:
// the namespace would stay Terminating otherwise.
//
// It refuses the same removal, from every identity but the manager's, while the robot records an
// action and the executor guard has no proof that it is off it (refuseLosingAnExecutorRecord), as
// the delete guard refuses that delete.
//
// This is a stop-gap until a stop is recorded against the robot's identity rather than
// its Robot object, after which removing a finalizer sheds nothing.
func (g *RobotAdmissionGate) lastFinalizerOfAHeldRobot(ctx context.Context, oldR, newR *fleetv1.Robot) error {
	if len(oldR.Finalizers) == 0 || len(newR.Finalizers) > 0 {
		return nil
	}
	held := safety.HeldBy(oldR)
	var pending []string
	if held == "" {
		// Nothing in the robot's status holds it: the stop guard says whether a stop in force
		// or pending covers it where it stands (a robot being deleted is a robot, and one
		// that entered a stopped zone before the stop reached it is covered all the same).
		covering, inForce, err := coveringStopsNamed(ctx, g.zoneReader(), oldR)
		if err != nil {
			return err
		}
		if len(covering) == 0 {
			if req, err := admission.RequestFromContext(ctx); err == nil && isControllerServiceAccount(req.UserInfo.Username) {
				return nil
			}
			return refuseLosingAnExecutorRecord(ctx, g.zoneReader(), oldR, "removing its last finalizer")
		}
		pending = covering
		if len(inForce) > 0 {
			held = strings.Join(inForce, ", ")
		}
	}
	if req, err := admission.RequestFromContext(ctx); err == nil && isControllerServiceAccount(req.UserInfo.Username) {
		return nil
	}
	terminating, err := namespaceTerminating(ctx, g.zoneReader(), newR.Namespace)
	if err != nil {
		return err
	}
	if terminating {
		return nil
	}
	if held == "" {
		return apierrors.NewForbidden(robotGR, newR.Name, fmt.Errorf(
			"robot %q in namespace %q is under the emergency stop of %s, which is issued and not yet applied (it "+
				"carries swarmada.io/estop-triggered and no swarmada.io/estop-processed), and removing its last "+
				"finalizer (%s) would complete a delete before the stop reaches it: the robot would never be sent "+
				"the stop. Retry once the stop is applied; the robot then holds it in its status, and the removal is "+
				"refused until it is released (RFC-0001 #safety-estop-state-machine)",
			newR.Name, newR.Namespace, strings.Join(pending, ", "), strings.Join(oldR.Finalizers, ", ")))
	}
	return apierrors.NewForbidden(robotGR, newR.Name, fmt.Errorf(
		"robot %q in namespace %q holds a stop (%s), and removing its last finalizer (%s) would complete a "+
			"delete the Robot delete guard refuses: a stop lives only in the Robot's status, so the robot would "+
			"come back Normal and dispatchable if created again. An estop is released by an authorized clear "+
			"and an operator's confirmed resume (`swarmctl estop resume`), a Failed component when the robot "+
			"reports it repaired over ControlStream, and Error when the robot reports another phase or falls "+
			"silent and is marked Offline. Removing it is admitted while the namespace itself is being deleted "+
			"(RFC-0001 #safety-estop-state-machine)",
		newR.Name, newR.Namespace, held, strings.Join(oldR.Finalizers, ", ")))
}

// defaulterStampedAnnotationKeys are the informational annotations the Robot
// defaulter re-stamps on every write (the namespace's coordinate convention and the
// adapter's provenance). A per-robot estop request carries whatever they resolve to
// now, so a change to the namespace's SwarmadaConfig since admission would otherwise
// turn the stop into a write that is re-validated. No invariant reads them.
var defaulterStampedAnnotationKeys = []string{annLengthUnit, annAngleUnit, annGroundFloor, annRobotSource}

// onlyEstopAnnotationsChanged reports whether newR differs from oldR in the estop
// annotations and in nothing else a writer controls (onlyEstopAnnotationsChangedOn). The
// defaulter's informational stamps are left out of the comparison as well.
func onlyEstopAnnotationsChanged(oldR, newR *fleetv1.Robot) bool {
	return onlyEstopAnnotationsChangedOn(oldR, newR, defaulterStampedAnnotationKeys,
		func(r *fleetv1.Robot) { r.Status = fleetv1.RobotStatus{} })
}

// onlyFinalizersChangedWhileDeleting reports whether oldR is being deleted and newR
// differs from it in metadata.finalizers and in nothing else a writer controls. The
// defaulter's informational stamps and a backfill of an unset robot-id (which
// robotIDChange has already vetted) are left out of the comparison, as are server-managed
// metadata and status.
func onlyFinalizersChangedWhileDeleting(oldR, newR *fleetv1.Robot) bool {
	if oldR.DeletionTimestamp == nil || equality.Semantic.DeepEqual(oldR.Finalizers, newR.Finalizers) {
		return false
	}
	strip := func(r *fleetv1.Robot) *fleetv1.Robot {
		c := r.DeepCopy()
		c.Finalizers = nil
		for _, k := range defaulterStampedAnnotationKeys {
			delete(c.Annotations, k)
		}
		delete(c.Annotations, fleetv1.RobotIDAnnotation)
		if len(c.Annotations) == 0 {
			c.Annotations = nil
		}
		c.ResourceVersion = ""
		c.Generation = 0
		c.ManagedFields = nil
		c.Status = fleetv1.RobotStatus{}
		return c
	}
	return equality.Semantic.DeepEqual(strip(oldR), strip(newR))
}

// zoneReader is the reader the zone invariants and the robot-id uniqueness check use: the
// uncached APIReader when one is wired, else Client (see APIReader).
func (g *RobotAdmissionGate) zoneReader() client.Reader {
	if g.APIReader != nil {
		return g.APIReader
	}
	return g.Client
}

// validate runs every rule. It is the create path; update splits the two halves
// apart (see ValidateUpdate).
func (g *RobotAdmissionGate) validate(ctx context.Context, robot *fleetv1.Robot) (admission.Warnings, error) {
	if warns, err := g.validateInvariants(ctx, robot, true); err != nil {
		return warns, err
	}
	return g.validateAdapterBinding(ctx, robot)
}

// validateInvariants enforces the rules that are true of a Robot at rest: its
// RobotClass and zone resolve, the zone is terminal, its charging dock is reachable
// from that zone, and its robot-id is unique in the namespace. None of them depend
// on another object's live status, so all of them run on every create and update,
// except that a composite zone is refused only when zoneChanged (see ValidateUpdate).
func (g *RobotAdmissionGate) validateInvariants(ctx context.Context, robot *fleetv1.Robot, zoneChanged bool) (admission.Warnings, error) {
	// A gate with no client cannot assert any of these. Deny rather than fall
	// through: a Robot whose fields are all empty would otherwise be admitted
	// without a single lookup having been attempted.
	if g.Client == nil {
		return nil, fmt.Errorf("robot admission gate has no client; refusing to admit %s/%s unvalidated",
			robot.Namespace, robot.Name)
	}

	classPath := field.NewPath("spec").Child("robotClass")

	// 1. If a RobotClass is named, it must exist in the Robot's namespace — the
	//    admission-time merge (RFC-0001 #robotclass) cannot resolve a dangling reference.
	if robot.Spec.RobotClass != "" {
		var class fleetv1.RobotClass
		key := client.ObjectKey{Namespace: robot.Namespace, Name: robot.Spec.RobotClass}
		if err := g.Client.Get(ctx, key, &class); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
					field.Invalid(classPath, robot.Spec.RobotClass,
						fmt.Sprintf("RobotClass %q not found in namespace %q", robot.Spec.RobotClass, robot.Namespace)),
				})
			}
			return nil, fmt.Errorf("resolving RobotClass %q: %w", robot.Spec.RobotClass, err)
		}
	}

	// 2. spec.zone must name a FleetZone that exists and is a LEAF.
	//
	//    A robot is placed in exactly one zone, and the zone tree's non-leaf nodes are
	//    aggregates: capacity, estop propagation and traffic deconfliction are all defined
	//    over the leaf a robot physically occupies. Admitting a robot into a parent zone
	//    would leave the deconfliction engine reserving capacity on a node no robot can
	//    actually be in, and the Zone Controller unable to derive currentZone.
	//
	//    Leafness is computed from the tree rather than read from status.isLeaf — see
	//    childZonesOf. Existence is checked here too: a dangling spec.zone is the same
	//    class of dangling reference as spec.robotClass above.
	zonePath := field.NewPath("spec").Child("zone")
	if robot.Spec.Zone != "" {
		var z fleetv1.FleetZone
		zoneKey := client.ObjectKey{Namespace: robot.Namespace, Name: robot.Spec.Zone}
		if err := g.zoneReader().Get(ctx, zoneKey, &z); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
					field.Invalid(zonePath, robot.Spec.Zone,
						fmt.Sprintf("FleetZone %q not found in namespace %q", robot.Spec.Zone, robot.Namespace)),
				})
			}
			// Fail closed: an unreadable zone is not an absent one, and admitting on a
			// read error would let an API-server blip place robots in unverified zones.
			return nil, fmt.Errorf("resolving spec.zone %q: %w", robot.Spec.Zone, err)
		}
		children, err := childZonesOf(ctx, g.zoneReader(), robot.Namespace, robot.Spec.Zone)
		if err != nil {
			return nil, err
		}
		if len(children) > 0 {
			return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
				field.Invalid(zonePath, robot.Spec.Zone,
					fmt.Sprintf("spec.zone %q is not a leaf zone (has children: [%s])",
						robot.Spec.Zone, strings.Join(children, ", "))),
			})
		}

		// 2a. ...and is not a COMPOSITE (RFC-0001 FZ-C2: a terminal zone has no children
		//     and no members).
		//
		//     Composition is a separate relation from containment, so a composite has no
		//     child zones and the test above passes it. A robot admitted into one is placed
		//     there by declaration (FZ-G5, a composite has no polygon), and a zone estop on
		//     a composite is refused (FZ-C5), so no zone estop could reach it. Refused only
		//     when spec.zone changes; see ValidateUpdate for why.
		if zoneChanged && len(z.Spec.MemberZones) > 0 {
			return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
				field.Invalid(zonePath, robot.Spec.Zone,
					fmt.Sprintf("spec.zone %q is a composite zone (members: [%s]); a robot must be placed "+
						"in a terminal zone, because a composite groups zones without containing a robot "+
						"and an estop on a composite is refused. Name one of its members instead",
						robot.Spec.Zone, strings.Join(z.Spec.MemberZones, ", "))),
			})
		}

		// 2b. spec.charging.dockName, when set, must name a ChargingDock declared in this
		//     robot's zone or one of its ancestors.
		//
		//     Scoping to the ancestor chain is this check's own rule, narrower than the
		//     TDE's: a reservation looks a resource name up across the whole namespace
		//     (RFC-0001 #fleetzone-sharedresource-types-and-reservation-policy), but a
		//     dock in a sibling branch is not one this robot should be sent to charge at.
		//     A dangling or out-of-branch dockName is not caught anywhere later, and the
		//     robot would sit at a low battery with no error, which reads as a charging
		//     problem rather than a configuration one.
		if dock := dockNameOf(robot); dock != "" {
			zone, err := findChargingDock(ctx, g.Client, robot.Namespace, &z, dock)
			if err != nil {
				return nil, err
			}
			if zone == "" {
				return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
					field.Invalid(field.NewPath("spec").Child("charging").Child("dockName"), dock,
						fmt.Sprintf("charging.dockName %q is not a ChargingDock in zone %q or its ancestors",
							dock, robot.Spec.Zone)),
				})
			}
		}
	}

	// 3. The swarmada.io/robot-id annotation must be unique in the namespace.
	//
	//    This annotation is the identity the Fleet Adapter authenticates against per message
	//    (RFC-0001 #security-fleet-adapter-authentication-robot-identity) and the key telemetry is routed by. Two Robots sharing one would make an
	//    adapter's messages ambiguous: the control plane could not say which object a status
	//    or a position belongs to, and the collision would surface as one robot's telemetry
	//    silently landing on the other. Uniqueness is a namespace-wide property, so it cannot
	//    be expressed in the single-object schema — this is the reason it is a webhook rule.
	//
	//    THIS CHECK IS BEST-EFFORT. It lists and then admits, and nothing makes the two one
	//    step: two creates carrying one id that arrive together each list before the other
	//    is stored, and both are admitted. The list is uncached, which closes the window a
	//    cache adds and no more. It refuses every twin of a Robot already stored, which is
	//    the common case; uniqueness is ENFORCED at dispatch, where both objects of a racing
	//    pair exist: every Robot whose wire id another carries is withheld from work and
	//    marked RobotIDConflict (internal/controller/dispatch_holds.go,
	//    robot_id_conflict.go).
	if rid := robot.Annotations[fleetv1.RobotIDAnnotation]; rid != "" {
		var robots fleetv1.RobotList
		if err := g.zoneReader().List(ctx, &robots, client.InNamespace(robot.Namespace)); err != nil {
			return nil, fmt.Errorf("listing Robots in %q: %w", robot.Namespace, err)
		}
		for i := range robots.Items {
			other := &robots.Items[i]
			// Compare by name, not UID: on update the incoming object carries the same name
			// as the stored one, and a robot must never collide with itself.
			if other.Name == robot.Name {
				continue
			}
			if robotid.Wire(other) == rid {
				return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
					field.Invalid(field.NewPath("metadata").Child("annotations").Key(fleetv1.RobotIDAnnotation),
						rid, fmt.Sprintf("robot-id %q is already bound to Robot %q", rid, other.Name)),
				})
			}
		}
	}

	return nil, nil
}

// validateAdapterBinding enforces the FleetAdapter admission keystone (RFC-0001 #fleetadapter,
// #security-fleet-adapter-authentication-robot-identity) and the ADR-0032 contract-version condition. Unlike the invariants above
// this reads the LIVE status of another object, so update re-runs it only when
// spec.robotClass or spec.adapter.name changes — see ValidateUpdate.
func (g *RobotAdmissionGate) validateAdapterBinding(ctx context.Context, robot *fleetv1.Robot) (admission.Warnings, error) {
	if g.Client == nil {
		return nil, fmt.Errorf("robot admission gate has no client; refusing to admit %s/%s unvalidated",
			robot.Namespace, robot.Name)
	}

	log := logf.FromContext(ctx).WithValues(
		"robot", client.ObjectKeyFromObject(robot),
		"robotClass", robot.Spec.RobotClass,
		"adapter", robot.Spec.Adapter.Name,
	)
	adapterPath := field.NewPath("spec").Child("adapter").Child("name")

	// 4. The Robot's own adapter binding (spec.adapter.name) must resolve to a
	//    real, class-serving, Connected, conformance-passed FleetAdapter. The
	//    Security Model (RFC-0001 #security-fleet-adapter-authentication-robot-identity) authorizes a Fleet Adapter to act on a robot
	//    precisely because spec.adapter names it — a different, otherwise-
	//    healthy adapter serving the same class confers no authority, and
	//    revocation works by re-pointing this field. Admission therefore
	//    resolves this one named adapter directly rather than scanning the
	//    namespace for any eligible candidate.
	var adapter fleetv1.FleetAdapter
	adapterKey := client.ObjectKey{Namespace: robot.Namespace, Name: robot.Spec.Adapter.Name}
	if err := g.Client.Get(ctx, adapterKey, &adapter); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
				field.Invalid(adapterPath, robot.Spec.Adapter.Name,
					fmt.Sprintf("FleetAdapter %q not found in namespace %q (RFC-0001 #fleetadapter-admission-gating)",
						robot.Spec.Adapter.Name, robot.Namespace)),
			})
		}
		return nil, fmt.Errorf("resolving FleetAdapter %q: %w", robot.Spec.Adapter.Name, err)
	}

	if !adapterServesClass(&adapter, robot.Spec.RobotClass) {
		return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
			field.Invalid(adapterPath, robot.Spec.Adapter.Name,
				fmt.Sprintf("FleetAdapter %q does not serve RobotClass %s (spec.servesRobotClasses; RFC-0001 #fleetadapter-admission-gating)",
					robot.Spec.Adapter.Name, classOrNone(robot.Spec.RobotClass))),
		})
	}

	if adapter.Status.Phase != fleetv1.FleetAdapterPhaseConnected || adapter.Status.Conformance != fleetv1.ConformanceStatePassed {
		return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
			field.Invalid(adapterPath, robot.Spec.Adapter.Name,
				fmt.Sprintf("FleetAdapter %q is not ready: phase=%s conformance=%s (want Connected/Passed; RFC-0001 #fleetadapter-admission-gating)",
					robot.Spec.Adapter.Name, phaseOrPending(adapter.Status.Phase), conformanceOrUnknown(adapter.Status.Conformance))),
		})
	}

	// 3. ADR-0032 assignment gate, second condition: a Passed result is only binding against a
	//    contract version this build can actually drive. A result earned against an out-of-range
	//    contract — or a report that names no contract version at all — is not a defect in the
	//    report, so status.conformance is NOT rewritten to Failed (see
	//    fleetadapter_controller.verifyConformance); the ROBOT is refused instead, which is the
	//    honest split: the attestation stands, it just does not apply here.
	//
	//    Missing counts as incompatible (ADR-0032: "never as an implicit pass"). The practical
	//    consequence is deliberate: an adapter qualified by a pre-versioning harness stops admitting
	//    robots until `make conformance` is re-run. It keeps streaming telemetry and stays
	//    stoppable throughout, and in-flight work is not cancelled.
	if ok, reason := contract.Supports(adapter.Status.ConformanceContractVersion); !ok {
		return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
			field.Invalid(adapterPath, robot.Spec.Adapter.Name,
				fmt.Sprintf("FleetAdapter %q conformance is not bound to a supported contract version: %s (RFC-0001 #fleetadapter-admission-gating, ADR-0032)",
					robot.Spec.Adapter.Name, reason)),
		})
	}

	log.V(1).Info("Robot admitted: bound FleetAdapter is eligible")
	return nil, nil
}

// adapterServesClass reports whether the adapter is responsible for the given
// RobotClass. An adapter with no servesRobotClasses is a "serves-any" adapter
// (development only; see RFC-0001 #fleetadapter) and matches every class — including the empty
// class of a fully self-described Robot, which is precisely why a classless
// Robot still requires a Connected, conformant serves-any adapter to be admitted.
// dockNameOf returns spec.charging.dockName, tolerating an absent charging block.
func dockNameOf(robot *fleetv1.Robot) string {
	if robot.Spec.Charging == nil {
		return ""
	}
	return robot.Spec.Charging.DockName
}

// findChargingDock walks from `start` up the parentZone chain looking for a sharedResource
// named `dock` whose type is ChargingDock, returning the zone that declares it ("" if none).
//
// Two deliberate strictnesses. A resource of the right NAME but the wrong TYPE does not
// match: reserving a Corridor as a charging dock would queue the robot behind a resource
// that never charges it. And the walk is depth-bounded like detectCycle's — a
// mis-parented chain must fail admission rather than spin a webhook, which would stall
// every Robot write in the namespace behind a request that never returns.
func findChargingDock(ctx context.Context, c client.Client, namespace string, start *fleetv1.FleetZone, dock string) (string, error) {
	zone := start
	for depth := 0; depth < maxZoneAncestryDepth; depth++ {
		for i := range zone.Spec.SharedResources {
			r := &zone.Spec.SharedResources[i]
			if r.Name == dock && r.Type == fleetv1.SharedResourceChargingDock {
				return zone.Name, nil
			}
		}
		if zone.Spec.ParentZone == "" {
			return "", nil
		}
		var parent fleetv1.FleetZone
		key := client.ObjectKey{Namespace: namespace, Name: zone.Spec.ParentZone}
		if err := c.Get(ctx, key, &parent); err != nil {
			if apierrors.IsNotFound(err) {
				// The chain ends at a missing ancestor. The dock was not found below it, and
				// an unreadable-because-absent parent cannot declare one, so this is a
				// definite "not found" rather than an unknown.
				return "", nil
			}
			// Fail closed: an unreadable ancestor might be the one declaring the dock.
			return "", fmt.Errorf("walking parentZone chain at %q: %w", zone.Spec.ParentZone, err)
		}
		zone = &parent
	}
	return "", fmt.Errorf("parentZone chain from %q exceeds %d levels; refusing to resolve charging.dockName",
		start.Name, maxZoneAncestryDepth)
}

func adapterServesClass(a *fleetv1.FleetAdapter, class string) bool {
	if len(a.Spec.ServesRobotClasses) == 0 {
		return true
	}
	for _, c := range a.Spec.ServesRobotClasses {
		if c == class {
			return true
		}
	}
	return false
}

// classOrNone renders an empty RobotClass reference legibly in denial messages.
func classOrNone(class string) string {
	if class == "" {
		return "<none> (self-described Robot; requires a Connected serves-any FleetAdapter)"
	}
	return class
}

// phaseOrPending renders an unset phase as Pending (the implicit initial state)
// so denial messages never show an empty value.
func phaseOrPending(p fleetv1.FleetAdapterPhase) string {
	if p == "" {
		return string(fleetv1.FleetAdapterPhasePending)
	}
	return string(p)
}

// conformanceOrUnknown renders an unset conformance result as Unknown.
func conformanceOrUnknown(c fleetv1.ConformanceState) string {
	if c == "" {
		return string(fleetv1.ConformanceStateUnknown)
	}
	return string(c)
}

// onlyMetadataChanged reports whether newR differs from oldR in annotations, labels or
// finalizers only: spec and status are equal.
func onlyMetadataChanged(oldR, newR *fleetv1.Robot) bool {
	return equality.Semantic.DeepEqual(oldR.Spec, newR.Spec) && equality.Semantic.DeepEqual(oldR.Status, newR.Status)
}

// pendingFirmwareKeys are the PullOnIdle dispatch annotations the firmware rollout controller
// writes and the Robot Agent acts on at its next idle transition.
var pendingFirmwareKeys = []string{
	"swarmada.io/pending-firmware-version", "swarmada.io/pending-firmware-uri", "swarmada.io/pending-firmware-checksum",
}

// refuseFirmwareInstructionUnderStop refuses, to every identity but the control plane, a write
// that adds or changes a pending-firmware key on a robot a stop covers, in force or pending: an
// over-the-air pull to a stopped robot is dispatch, and the instruction would stand on the robot
// for its agent to act on. Removing a key is admitted.
func (g *RobotAdmissionGate) refuseFirmwareInstructionUnderStop(ctx context.Context, oldR, newR *fleetv1.Robot) error {
	var changed []string
	for _, k := range pendingFirmwareKeys {
		nv, has := newR.Annotations[k]
		ov, had := oldR.Annotations[k]
		if has && (!had || ov != nv) {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if req, err := admission.RequestFromContext(ctx); err == nil && isControllerServiceAccount(req.UserInfo.Username) {
		return nil
	}
	covering, _, err := coveringStopsNamed(ctx, g.zoneReader(), oldR)
	if err != nil {
		return err
	}
	if len(covering) == 0 {
		return nil
	}
	return apierrors.NewForbidden(robotGR, newR.Name, fmt.Errorf(
		"robot %q in namespace %q is under the emergency stop of %s, and %s would tell it to pull and install "+
			"firmware at its next idle transition: an over-the-air update to a robot a stop covers is dispatch, "+
			"which a stop withholds. Clear the stop and resume the robot first (swarmctl estop resume) "+
			"(RFC-0001 #safety-estop-scopes)",
		newR.Name, newR.Namespace, strings.Join(covering, ", "), strings.Join(changed, ", ")))
}

// coveringStopsNamed names the stops the stop guard says cover robot, as the refusals name
// them, and separately those that are in force (applied, or recorded) rather than pending.
func coveringStopsNamed(ctx context.Context, rd client.Reader, robot *fleetv1.Robot) (all, inForce []string, err error) {
	cov, err := estopscope.Covering(ctx, rd, robot)
	if err != nil {
		return nil, nil, err
	}
	for _, s := range cov.Stops {
		var name string
		switch s.Kind {
		case estopscope.KindZone:
			name = fmt.Sprintf("zone %q", s.Name)
		case estopscope.KindNamespace:
			name = fmt.Sprintf("the namespace (SwarmadaConfig %q)", s.Name)
		default:
			name = "its own stop"
		}
		all = append(all, name)
		if !s.Pending {
			inForce = append(inForce, name)
		}
	}
	return all, inForce, nil
}
