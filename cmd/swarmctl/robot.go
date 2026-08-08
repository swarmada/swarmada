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

	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/admission"
	"github.com/swarmada/swarmada/internal/cli"
)

// reAdmit is the testable core: fetch the robot and the new class, re-apply the
// class template, and update the robot. It is a standard Robot update — RBAC is
// enforced by the API server on the update itself.
func (o *options) reAdmit(ctx context.Context, c client.Client, ns, name, className, dock string, yes bool) error {
	robot := &fleetv1.Robot{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, robot); err != nil {
		return fmt.Errorf("getting robot/%s: %w", name, err)
	}
	class := &fleetv1.RobotClass{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: className}, class); err != nil {
		return fmt.Errorf("getting robotclass/%s: %w", className, err)
	}

	ok, err := cli.Confirm(o.streams, fmt.Sprintf("Re-admit robot %q to class %q in %q? This changes its declared capabilities.", name, className, ns), yes)
	if err != nil {
		return err
	}
	if !ok {
		_, _ = fmt.Fprintln(o.streams.Err, "aborted.")
		return nil
	}

	base := robot.DeepCopy()
	applyClassToSpec(&robot.Spec, class, dock)
	if err := c.Patch(ctx, robot, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating robot/%s: %w", name, err)
	}

	_, _ = fmt.Fprintf(o.streams.Out, "robot.swarmada.io/%s re-admitted to class %s\n", name, className)
	return nil
}

// applyClassToSpec re-applies a RobotClass template onto a Robot spec: the class
// is authoritative for the typed collections and the base adapter; the robot's
// identity (manufacturer, model), zone, and — unless overridden — charging dock
// are preserved. Shared by robot re-admit and robotclass rollout.
func applyClassToSpec(spec *fleetv1.RobotSpec, class *fleetv1.RobotClass, dock string) {
	spec.RobotClass = class.Name
	if class.Spec.BaseAdapter.Name != "" {
		spec.Adapter.Name = class.Spec.BaseAdapter.Name
	}
	if class.Spec.BaseAdapter.Version != "" {
		spec.Adapter.Version = class.Spec.BaseAdapter.Version
	}
	spec.Hardware = class.Spec.Hardware
	spec.Capabilities = class.Spec.BaseCapabilities
	spec.InstalledModels = class.Spec.DefaultModels
	spec.Constraints = class.Spec.DefaultConstraints
	if tc := class.Spec.DefaultTelemetry; tc != nil {
		spec.TelemetryIntervalSeconds = tc.TelemetryIntervalSeconds
		spec.MotionThresholdMeters = tc.MotionThresholdMeters
		spec.MaxIdleIntervalSeconds = tc.MaxIdleIntervalSeconds
	}

	// Preserve the existing dock unless --dock overrides it.
	preservedDock := dock
	if preservedDock == "" && spec.Charging != nil {
		preservedDock = spec.Charging.DockName
	}
	spec.Charging = admission.MergeCharging(class.Spec.DefaultChargingConfig, preservedDock)
}
