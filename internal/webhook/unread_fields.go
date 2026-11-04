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
	"fmt"
	"reflect"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// SERVED FIELDS WITH NO READER.
//
// A field the API accepts, validates and stores, and that no controller acts on, is a
// promise the release does not keep. An author who sets robot.spec.constraints.maxSpeedMs
// sees the write succeed and has every reason to believe the speed is limited. Removing
// the field is a breaking change, and so is refusing it; so the write is admitted and
// the author is told, at the write, that it does nothing. Implementing the field removes
// its warning, in the same change.
//
// Each warning names the field path and says, first, that setting it has no effect in
// this release, then what does happen instead where that is not obvious. RFC-0001's
// maturity table lists the same fields; a field leaves this file when it leaves that list.

// unreadFieldWarning renders the one sentence every warning in this file starts with.
func unreadFieldWarning(path, detail string) string {
	msg := path + ": setting this field has no effect in this release"
	if detail != "" {
		msg += ": " + detail
	}
	return msg
}

// onUnresolvedParameterDetail says what an unresolved binding does, since the policy
// field that names a consequence selects none.
const onUnresolvedParameterDetail = "no controller reads it, so none of Fail, UseDefault or Omit is " +
	"applied. An unresolved binding is left in the child's parameters as written, and the miss is " +
	"recorded in its status.parameterResolution when a FleetTask spec.flow starts it"

// executorUnreadFieldWarnings warns about the unread fields of one ActionExecutor at path.
func executorUnreadFieldWarnings(ex *fleetv1.ActionExecutor, path string) []string {
	if ex == nil || ex.OnUnresolvedParameter == nil {
		return nil
	}
	return []string{unreadFieldWarning(path+".onUnresolvedParameter", onUnresolvedParameterDetail)}
}

// fleetTaskUnreadFieldWarnings warns about every executor a FleetTask carries, from the
// one enumeration of executors the binding checks also use.
func fleetTaskUnreadFieldWarnings(t *fleetv1.FleetTask) []string {
	var out []string
	for _, te := range taskExecutors(t) {
		out = append(out, executorUnreadFieldWarnings(te.executor, te.path.String())...)
	}
	return out
}

// robotUnreadField is one unread Robot field: where it lives, how to read it, and what
// the warning adds after the no-effect sentence.
type robotUnreadField struct {
	path   string
	value  func(*fleetv1.Robot) any // nil when unset
	detail string
}

// adapterEnforcesDetail is the warning for the two limits an integrator is most likely
// to assume are enforced. The robot's own adapter is the only thing that can enforce
// them: no Fleet Adapter protocol message carries either value.
const adapterEnforcesDetail = "Swarmada does not enforce this limit and does not send it to the " +
	"adapter; the adapter must enforce it"

// robotUnreadFields is the Robot row of RFC-0001's maturity table (the spec fields
// "validated and inherited and acted on by nothing"), in the order the table lists them.
var robotUnreadFields = []robotUnreadField{
	{"spec.constraints.maxPayloadKg", func(r *fleetv1.Robot) any {
		if r.Spec.Constraints == nil || r.Spec.Constraints.MaxPayloadKg == nil {
			return nil
		}
		return *r.Spec.Constraints.MaxPayloadKg
	}, adapterEnforcesDetail},
	{"spec.constraints.maxSpeedMs", func(r *fleetv1.Robot) any {
		if r.Spec.Constraints == nil || r.Spec.Constraints.MaxSpeedMs == nil {
			return nil
		}
		return *r.Spec.Constraints.MaxSpeedMs
	}, adapterEnforcesDetail},
	{"spec.charging.minBatteryPctToCharge", func(r *fleetv1.Robot) any {
		if r.Spec.Charging == nil || r.Spec.Charging.MinBatteryPctToCharge == nil {
			return nil
		}
		return *r.Spec.Charging.MinBatteryPctToCharge
	}, "no controller sends a robot to charge"},
	{"spec.charging.targetBatteryPct", func(r *fleetv1.Robot) any {
		if r.Spec.Charging == nil || r.Spec.Charging.TargetBatteryPct == nil {
			return nil
		}
		return *r.Spec.Charging.TargetBatteryPct
	}, "no controller sends a robot to charge"},
	{"spec.motionThresholdMeters", func(r *fleetv1.Robot) any {
		if r.Spec.MotionThresholdMeters == nil {
			return nil
		}
		return *r.Spec.MotionThresholdMeters
	}, ""},
	{"spec.maxIdleIntervalSeconds", func(r *fleetv1.Robot) any {
		if r.Spec.MaxIdleIntervalSeconds == nil {
			return nil
		}
		return *r.Spec.MaxIdleIntervalSeconds
	}, ""},
	{"spec.adapter.version", func(r *fleetv1.Robot) any {
		if r.Spec.Adapter.Version == "" {
			return nil
		}
		return r.Spec.Adapter.Version
	}, "nothing compares it with the version the adapter reports; it is only copied, as written, " +
		"into the safety audit entry for an estop that breached its latency bound"},
	{"spec.installedModels[].requiredHardware", func(r *fleetv1.Robot) any {
		var all [][]string
		for _, m := range r.Spec.InstalledModels {
			if len(m.RequiredHardware) > 0 {
				all = append(all, m.RequiredHardware)
			}
		}
		if len(all) == 0 {
			return nil
		}
		return all
	}, "a model's capability is granted whatever the state of the hardware it names"},
}

// robotUnreadFieldWarnings warns about each unread Robot field that newRobot sets and
// oldRobot (nil on create) did not set to the same value.
//
// On an update only a CHANGED field is warned about. The Robot defaulter fills several
// of these fields from the RobotClass and the SwarmadaConfig defaults, and the control
// plane writes Robot metadata; warning on every write would repeat the same sentence to
// the manager on each label or annotation change, and a warning repeated where nobody
// acts on it trains its readers to ignore it.
func robotUnreadFieldWarnings(oldRobot, newRobot *fleetv1.Robot) []string {
	var out []string
	for _, f := range robotUnreadFields {
		v := f.value(newRobot)
		if v == nil {
			continue
		}
		if oldRobot != nil && reflect.DeepEqual(f.value(oldRobot), v) {
			continue
		}
		out = append(out, unreadFieldWarning(f.path, f.detail))
	}
	return out
}

// swarmadaConfigUnreadFieldWarnings warns about the unread SwarmadaConfig fields cfg sets.
func swarmadaConfigUnreadFieldWarnings(cfg *fleetv1.SwarmadaConfig) []string {
	if cfg.Spec.Provisioning.Mode != fleetv1.ProvisioningModeDirectRegister {
		return nil
	}
	return []string{unreadFieldWarning("spec.provisioning.mode",
		fmt.Sprintf("%s is not implemented, so robots are onboarded as under %s: a robot that "+
			"connects is recorded as a DiscoveredRobot and becomes a Robot only when it is admitted",
			fleetv1.ProvisioningModeDirectRegister, fleetv1.ProvisioningModeTwoPhase))}
}
