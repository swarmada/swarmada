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
	"encoding/json"
	"fmt"
	"strconv"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// Coordinate-system annotations stamped from SwarmadaConfig.spec.coordinateSystem
// (ADR-0017): informational metadata so consumers read the facility convention off
// the Robot without a second config lookup. The control plane never transforms
// coordinates — these only inform.
const (
	annLengthUnit  = "swarmada.io/length-unit"
	annAngleUnit   = "swarmada.io/angle-unit"
	annGroundFloor = "swarmada.io/ground-floor"
)

// +kubebuilder:webhook:path=/mutate-swarmada-io-v1-robot,mutating=true,failurePolicy=fail,sideEffects=None,groups=swarmada.io,resources=robots,verbs=create;update,versions=v1,name=mrobot.swarmada.io,admissionReviewVersions=v1

// +kubebuilder:rbac:groups=swarmada.io,resources=robotclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=swarmada.io,resources=swarmadaconfigs,verbs=get;list;watch

// RobotDefaulter is the mutating webhook that performs the RobotClass
// admission-time merge (RFC-0001 §5.2.1.2): when spec.robotClass is set, the
// class's hardware inventory, default models, base capabilities, operational
// defaults, and base adapter are merged onto the Robot once, at admission, and
// the resolved values are persisted on the object. Mutating webhooks run
// before CRD schema/CEL validation, which lets this defaulter fill
// spec.adapter.name/version from the class's baseAdapter before the
// MinLength=1 requirement on those fields is checked.
//
// The merge runs on Create, and again on Update only if spec.robotClass
// itself changed — mirroring RobotAdmissionGate.ValidateUpdate's trigger
// condition. An unrelated update (status, labels, a manual edit to an
// already-resolved field) must NOT be re-merged: updating a RobotClass does
// not retroactively mutate robots already admitted against it (§5.2.1.3).
type RobotDefaulter struct {
	Client client.Client
}

// SetupWebhookWithManager registers the defaulter with the manager's webhook
// server. The registered path (/mutate-swarmada-io-v1-robot) is derived from
// the group/version/kind and matches the +kubebuilder:webhook marker above.
func (d *RobotDefaulter) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1.Robot{}).
		WithDefaulter(d).
		Complete()
}

var _ webhook.CustomDefaulter = &RobotDefaulter{}

// Default implements webhook.CustomDefaulter.
func (d *RobotDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	robot, ok := obj.(*fleetv1.Robot)
	if !ok {
		return fmt.Errorf("expected a Robot object but got %T", obj)
	}

	// Stamp the namespace coordinate convention (ADR-0017) regardless of RobotClass —
	// purely informational, fail-safe to the CRD defaults.
	d.stampCoordinateAnnotations(ctx, robot)

	// Carry the authenticated operator onto a robot-scoped estop trigger/clear (ADR-0046).
	// Robot is the one carrier that already had a mutating webhook, so the stamp rides
	// here rather than in its own; the other four live in estop_actor_webhooks.go.
	//
	// NOTE the failurePolicy asymmetry this creates: this webhook is failurePolicy=fail
	// (the RobotClass merge must not be skipped, or a Robot is admitted un-merged), while
	// the four dedicated stampers are failurePolicy=ignore. A robot estop therefore
	// inherits Robot admission's existing availability posture — unchanged by this
	// change, since the validating RobotAdmissionGate already fails closed on the same
	// write.
	stampEstopActor(ctx, robot)

	// Backfill the telemetry-projection identity (RFC-0001 §9.3.1): default
	// swarmada.io/robot-id to metadata.name when unset, so operator-created Robots
	// (and any predating auto-admit stamping) resolve robot_id → Robot for telemetry
	// status. Auto-admit stamps it explicitly; this covers everything else.
	if robot.Annotations[fleetv1.RobotIDAnnotation] == "" {
		if robot.Annotations == nil {
			robot.Annotations = map[string]string{}
		}
		robot.Annotations[fleetv1.RobotIDAnnotation] = robot.Name
	}

	if robot.Spec.RobotClass == "" {
		return nil
	}

	log := logf.FromContext(ctx).WithValues(
		"robot", client.ObjectKey{Namespace: robot.Namespace, Name: robot.Name},
		"robotClass", robot.Spec.RobotClass,
	)

	if req, err := admission.RequestFromContext(ctx); err == nil && req.Operation == admissionv1.Update {
		var old fleetv1.Robot
		if err := json.Unmarshal(req.OldObject.Raw, &old); err == nil {
			if old.Spec.RobotClass == robot.Spec.RobotClass {
				// Class reference unchanged: already resolved at a prior
				// admission. Do not re-merge over whatever the object holds
				// now (§5.2.1.3 — no retroactive mutation).
				return nil
			}
			log.V(1).Info("RobotClass reference changed on update; re-resolving merge",
				"oldRobotClass", old.Spec.RobotClass)
		}
	}

	var class fleetv1.RobotClass
	key := client.ObjectKey{Namespace: robot.Namespace, Name: robot.Spec.RobotClass}
	if err := d.Client.Get(ctx, key, &class); err != nil {
		if apierrors.IsNotFound(err) {
			// Leave the dangling reference for RobotAdmissionGate to reject
			// with an actionable error; nothing to merge here.
			return nil
		}
		return fmt.Errorf("resolving RobotClass %q: %w", robot.Spec.RobotClass, err)
	}

	mergeRobotClass(robot, &class)
	log.V(1).Info("merged RobotClass onto Robot at admission")
	return nil
}

// stampCoordinateAnnotations writes the namespace's coordinate convention
// (spec.coordinateSystem) onto the Robot as informational annotations (ADR-0017).
// Fail-safe: an unreadable or absent SwarmadaConfig yields the CRD defaults
// (Meters/Radians/0). It never transforms coordinates — only records the convention.
func (d *RobotDefaulter) stampCoordinateAnnotations(ctx context.Context, robot *fleetv1.Robot) {
	lengthUnit, angleUnit, groundFloor := string(fleetv1.LengthUnitMeters), string(fleetv1.AngleUnitRadians), int32(0)

	var list fleetv1.SwarmadaConfigList
	if err := d.Client.List(ctx, &list, client.InNamespace(robot.Namespace)); err == nil && len(list.Items) > 0 {
		cs := list.Items[0].Spec.CoordinateSystem
		if cs.LengthUnit != "" {
			lengthUnit = string(cs.LengthUnit)
		}
		if cs.AngleUnit != "" {
			angleUnit = string(cs.AngleUnit)
		}
		groundFloor = cs.GroundFloor
	}

	if robot.Annotations == nil {
		robot.Annotations = map[string]string{}
	}
	robot.Annotations[annLengthUnit] = lengthUnit
	robot.Annotations[annAngleUnit] = angleUnit
	robot.Annotations[annGroundFloor] = strconv.FormatInt(int64(groundFloor), 10)
}

// mergeRobotClass applies RobotClassSpec's documented merge semantics onto
// robot in place: list fields merge union-by-name with the Robot's own
// entries winning on a name collision; manufacturer/model, the base adapter
// name/version, the constraints and charging sub-fields, and the defaultTelemetry
// cadence scalars all fill in only where the Robot left the field unset.
func mergeRobotClass(robot *fleetv1.Robot, class *fleetv1.RobotClass) {
	robot.Spec.Hardware = unionByName(class.Spec.Hardware, robot.Spec.Hardware,
		func(h fleetv1.HardwareComponent) string { return h.Name })

	robot.Spec.InstalledModels = unionByName(class.Spec.DefaultModels, robot.Spec.InstalledModels,
		func(m fleetv1.ClassModel) string { return m.Name })

	robot.Spec.Capabilities = unionByName(class.Spec.BaseCapabilities, robot.Spec.Capabilities,
		func(c fleetv1.ClassCapability) string { return c.Name })

	// Identity fills in from the class where the Robot omits it. Both fields are
	// Required with MinLength=1 on the Robot, so until this assignment existed a Robot
	// that relied on its class for identity — the case §9.1.1 documents as the point of
	// a class — was rejected at admission on a field the operator was told not to repeat.
	if robot.Spec.Manufacturer == "" {
		robot.Spec.Manufacturer = class.Spec.Manufacturer
	}
	if robot.Spec.Model == "" {
		robot.Spec.Model = class.Spec.Model
	}

	// Constraints inherit PER FIELD, not as a whole block. Whole-block inheritance meant
	// a Robot that set one sub-field silently lost every other limit its class declared:
	// overriding maxSpeedMs dropped the class's minBatteryPctForAction floor, so a safety
	// limit disappeared on a write that never mentioned it.
	if dc := class.Spec.DefaultConstraints; dc != nil {
		if robot.Spec.Constraints == nil {
			robot.Spec.Constraints = &fleetv1.ClassConstraints{}
		}
		rc := robot.Spec.Constraints
		if rc.MaxPayloadKg == nil && dc.MaxPayloadKg != nil {
			v := *dc.MaxPayloadKg
			rc.MaxPayloadKg = &v
		}
		if rc.MinBatteryPctForAction == nil && dc.MinBatteryPctForAction != nil {
			v := *dc.MinBatteryPctForAction
			rc.MinBatteryPctForAction = &v
		}
		if rc.MaxSpeedMs == nil && dc.MaxSpeedMs != nil {
			v := *dc.MaxSpeedMs
			rc.MaxSpeedMs = &v
		}
	}

	// Same per-field rule for charging. dockName is Robot-only: a dock is a zone-scoped
	// shared resource and a class spans zones, so a class cannot name one.
	//
	// One consequence to expect: mixing a Robot-set minBatteryPctToCharge with a
	// class-supplied targetBatteryPct can now produce a pair the RobotChargingConfig CEL
	// rule rejects (target must exceed min). That is the operator having declared an
	// inconsistent combination, and it fails closed at admission rather than silently
	// discarding the class's value, which is what the whole-block merge did.
	if dcc := class.Spec.DefaultChargingConfig; dcc != nil {
		if robot.Spec.Charging == nil {
			robot.Spec.Charging = &fleetv1.RobotChargingConfig{}
		}
		ch := robot.Spec.Charging
		if ch.MinBatteryPctToCharge == nil && dcc.MinBatteryPctToCharge != nil {
			v := *dcc.MinBatteryPctToCharge
			ch.MinBatteryPctToCharge = &v
		}
		if ch.TargetBatteryPct == nil && dcc.TargetBatteryPct != nil {
			v := *dcc.TargetBatteryPct
			ch.TargetBatteryPct = &v
		}
	}

	if robot.Spec.Adapter.Name == "" {
		robot.Spec.Adapter.Name = class.Spec.BaseAdapter.Name
	}
	if robot.Spec.Adapter.Version == "" {
		robot.Spec.Adapter.Version = class.Spec.BaseAdapter.Version
	}

	// Telemetry-cadence scalars: fill each field the Robot left unset from the class's
	// defaultTelemetry (value-copied so the Robot never aliases the class object). The
	// Robot's own value always wins.
	if dt := class.Spec.DefaultTelemetry; dt != nil {
		if robot.Spec.TelemetryIntervalSeconds == nil && dt.TelemetryIntervalSeconds != nil {
			v := *dt.TelemetryIntervalSeconds
			robot.Spec.TelemetryIntervalSeconds = &v
		}
		if robot.Spec.MotionThresholdMeters == nil && dt.MotionThresholdMeters != nil {
			v := *dt.MotionThresholdMeters
			robot.Spec.MotionThresholdMeters = &v
		}
		if robot.Spec.MaxIdleIntervalSeconds == nil && dt.MaxIdleIntervalSeconds != nil {
			v := *dt.MaxIdleIntervalSeconds
			robot.Spec.MaxIdleIntervalSeconds = &v
		}
	}
}

// unionByName merges base and override slices keyed by name(): an override
// entry fully replaces a base entry of the same name; base entries with no
// override counterpart pass through unchanged; the result preserves base
// order with override-only entries appended after.
func unionByName[T any](base, override []T, name func(T) string) []T {
	if len(base) == 0 {
		return override
	}

	overrideByName := make(map[string]T, len(override))
	for _, o := range override {
		overrideByName[name(o)] = o
	}

	merged := make([]T, 0, len(base)+len(override))
	seen := make(map[string]bool, len(base))
	for _, b := range base {
		n := name(b)
		seen[n] = true
		if o, ok := overrideByName[n]; ok {
			merged = append(merged, o)
		} else {
			merged = append(merged, b)
		}
	}
	for _, o := range override {
		if !seen[name(o)] {
			merged = append(merged, o)
		}
	}
	return merged
}
