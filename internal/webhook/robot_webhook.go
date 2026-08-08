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
// admission keystone (RFC-0001 §5.2.12) and the adapter-binding authorization
// check (§9.5): a Robot is admissible only when the specific FleetAdapter it
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
// spec.adapter (§9.5 "Revoking an adapter's authority over a robot") actually
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
	// EstopAuthz authorizes the robot-scope estop-trigger / estop-clear custom verbs
	// when the swarmada.io/estop-triggered annotation is written on a Robot (§9.6.2,
	// §F-2b). A nil authorizer makes any estop-annotation change fail closed; the
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

// ValidateCreate runs the gate on Robot creation.
func (g *RobotAdmissionGate) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	robot, ok := obj.(*fleetv1.Robot)
	if !ok {
		return nil, fmt.Errorf("expected a Robot object but got %T", obj)
	}
	return g.validate(ctx, robot)
}

// ValidateUpdate re-runs the gate only when the Robot's class assignment or
// adapter binding changes. Status, label, and annotation updates must not be
// blocked by a transiently unavailable adapter — otherwise a controller could
// be unable to record status (e.g. mark a robot Offline) during the very
// adapter outage that caused it.
func (g *RobotAdmissionGate) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	newRobot, ok := newObj.(*fleetv1.Robot)
	if !ok {
		return nil, fmt.Errorf("expected a Robot object but got %T", newObj)
	}
	oldRobot, ok := oldObj.(*fleetv1.Robot)
	if !ok {
		return nil, fmt.Errorf("expected a Robot object but got %T", oldObj)
	}

	// Authorize a robot-scope estop when the estop-triggered annotation changes
	// (§F-2b): adding/re-valuing needs estop-trigger, removing needs estop-clear.
	// Checked before the adapter-binding early-return below (an estop write leaves
	// class/adapter unchanged, so it would otherwise slip through unauthorized).
	if verb, isEstopChange := estopVerbForAnnotations(oldRobot.Annotations, newRobot.Annotations); isEstopChange {
		if err := authorizeEstopVerb(ctx, g.EstopAuthz, robotGR, newRobot.Namespace, newRobot.Name, verb); err != nil {
			return nil, err
		}
	}

	// Re-run the adapter-binding gate only when the class or adapter changed;
	// annotation/status/label updates must not be blocked by a transient adapter.
	if oldRobot.Spec.RobotClass == newRobot.Spec.RobotClass &&
		oldRobot.Spec.Adapter.Name == newRobot.Spec.Adapter.Name {
		return nil, nil
	}
	return g.validate(ctx, newRobot)
}

// ValidateDelete is a no-op; deletions are always permitted.
func (g *RobotAdmissionGate) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (g *RobotAdmissionGate) validate(ctx context.Context, robot *fleetv1.Robot) (admission.Warnings, error) {
	log := logf.FromContext(ctx).WithValues(
		"robot", client.ObjectKeyFromObject(robot),
		"robotClass", robot.Spec.RobotClass,
		"adapter", robot.Spec.Adapter.Name,
	)
	classPath := field.NewPath("spec").Child("robotClass")
	adapterPath := field.NewPath("spec").Child("adapter").Child("name")

	// 1. If a RobotClass is named, it must exist in the Robot's namespace — the
	//    admission-time merge (§5.2.1.2) cannot resolve a dangling reference.
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
		if err := g.Client.Get(ctx, zoneKey, &z); err != nil {
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
		children, err := childZonesOf(ctx, g.Client, robot.Namespace, robot.Spec.Zone)
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

		// 2b. spec.charging.dockName, when set, must name a ChargingDock declared in this
		//     robot's zone or one of its ancestors.
		//
		//     Scoping to the ancestor chain is the point, not incidental: shared resources
		//     are inherited downward, so a dock in a sibling branch is one this robot cannot
		//     reach. A dangling or out-of-branch dockName is not caught anywhere later —
		//     the charging controller would simply never find a dock to reserve, and the
		//     robot would sit at a low battery with no reservation and no error, which
		//     reads as a charging problem rather than a configuration one.
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
	//    (§9.5.1.2) and the key telemetry is routed by. Two Robots sharing one would make an
	//    adapter's messages ambiguous: the control plane could not say which object a status
	//    or a position belongs to, and the collision would surface as one robot's telemetry
	//    silently landing on the other. Uniqueness is a namespace-wide property, so it cannot
	//    be expressed in the single-object schema — this is the reason it is a webhook rule.
	if rid := robot.Annotations[fleetv1.RobotIDAnnotation]; rid != "" {
		var robots fleetv1.RobotList
		if err := g.Client.List(ctx, &robots, client.InNamespace(robot.Namespace)); err != nil {
			return nil, fmt.Errorf("listing Robots in %q: %w", robot.Namespace, err)
		}
		for i := range robots.Items {
			other := &robots.Items[i]
			// Compare by name, not UID: on update the incoming object carries the same name
			// as the stored one, and a robot must never collide with itself.
			if other.Name == robot.Name {
				continue
			}
			if other.Annotations[fleetv1.RobotIDAnnotation] == rid {
				return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
					field.Invalid(field.NewPath("metadata").Child("annotations").Key(fleetv1.RobotIDAnnotation),
						rid, fmt.Sprintf("robot-id %q is already bound to Robot %q", rid, other.Name)),
				})
			}
		}
	}

	// 4. The Robot's own adapter binding (spec.adapter.name) must resolve to a
	//    real, class-serving, Connected, conformance-passed FleetAdapter. The
	//    Security Model (§9.5) authorizes a Fleet Adapter to act on a robot
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
					fmt.Sprintf("FleetAdapter %q not found in namespace %q (RFC-0001 §9.5)",
						robot.Spec.Adapter.Name, robot.Namespace)),
			})
		}
		return nil, fmt.Errorf("resolving FleetAdapter %q: %w", robot.Spec.Adapter.Name, err)
	}

	if !adapterServesClass(&adapter, robot.Spec.RobotClass) {
		return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
			field.Invalid(adapterPath, robot.Spec.Adapter.Name,
				fmt.Sprintf("FleetAdapter %q does not serve RobotClass %s (spec.servesRobotClasses; RFC-0001 §9.5)",
					robot.Spec.Adapter.Name, classOrNone(robot.Spec.RobotClass))),
		})
	}

	if adapter.Status.Phase != fleetv1.FleetAdapterPhaseConnected || adapter.Status.Conformance != fleetv1.ConformanceStatePassed {
		return nil, apierrors.NewInvalid(robotGK, robot.Name, field.ErrorList{
			field.Invalid(adapterPath, robot.Spec.Adapter.Name,
				fmt.Sprintf("FleetAdapter %q is not ready: phase=%s conformance=%s (want Connected/Passed; RFC-0001 §9.5)",
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
				fmt.Sprintf("FleetAdapter %q conformance is not bound to a supported contract version: %s (RFC-0001 §9.5, ADR-0032)",
					robot.Spec.Adapter.Name, reason)),
		})
	}

	log.V(1).Info("Robot admitted: bound FleetAdapter is eligible")
	return nil, nil
}

// adapterServesClass reports whether the adapter is responsible for the given
// RobotClass. An adapter with no servesRobotClasses is a "serves-any" adapter
// (development only; see §5.2.12) and matches every class — including the empty
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
