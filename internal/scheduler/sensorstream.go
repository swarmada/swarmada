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

package scheduler

import (
	"sort"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
)

// ReasonSensorStreamNotOffered is the closed reason vocabulary entry for: no
// candidate robot declares the sensor stream a
// FleetAction's spec.executor.inputs[].sensorStream requires. Written to
// status.conditions[type=ExternalDependenciesReady].reason and
// status.failureReason — never assigned. The action stays Pending; no robot
// is committed ( "the rule": an action whose external dependencies are
// not present and ready is not assigned).
const ReasonSensorStreamNotOffered = "SensorStreamNotOffered"

// ResolveSensorStreamRobotRef resolves a SensorStreamRobotRef to the robot
// name whose sensorStreams[] must be checked. It is deliberately decoupled
// from any particular FleetTask/FleetAction lookup mechanism: `memberRobot`
// is supplied by the caller, which knows how to read the robot currently
// assigned to a named sibling member (e.g. that member's own FleetAction
// status.assignedRobot) — this function only implements the closed, two-case
// addressing rule itself: `{fromMember}` | `{name}`, never a
// template expression.
func ResolveSensorStreamRobotRef(ref fleetv1.SensorStreamRobotRef, memberRobot func(member string) (robotName string, ok bool)) (robotName string, ok bool) {
	if ref.Name != nil {
		return *ref.Name, true
	}
	if ref.FromMember != nil {
		if memberRobot == nil {
			return "", false
		}
		return memberRobot(*ref.FromMember)
	}
	return "", false
}

// SensorStreamOffered reports whether the named robot exists among candidates
// and, if so, whether it OFFERS streamID — it declares the stream in its
// status.sensorStreams[] AND every capability that stream declares as
// required is Active on the robot. This is the G2 cheap pre-filter: "at least
// one candidate robot offers every required sensor stream". `robotFound`
// distinguishes "the named robot doesn't exist yet / isn't a candidate" from
// "it exists but does not offer the stream" — both are
// ReasonSensorStreamNotOffered, but the message an operator sees should say
// which.
//
// The capability half is folded into `offered` rather than reported beside it
// because the specification defines the word that way: "a stream whose
// required_capabilities are not all Active on the robot MUST NOT be treated as
// offered" (rfcs/rfc-0001-core-spec/sensor-streams.md, Declaration). A gate
// that answered "offered" for a stream the robot cannot actually serve would
// commit the control plane to activating a camera whose capability is Paused.
// SensorStreamCapabilitiesUnmet, below, recovers WHICH capabilities were
// missing so the operator-facing message can distinguish the two causes; the
// predicate itself keeps the two-boolean shape the gate reads.
func SensorStreamOffered(candidates []fleetv1.Robot, robotName, streamID string) (offered bool, robotFound bool) {
	for _, r := range candidates {
		if r.Name != robotName {
			continue
		}
		robotFound = true
		for _, s := range r.Status.SensorStreams {
			if s.StreamID == streamID {
				return len(unmetStreamCapabilities(&r, &s)) == 0, true
			}
		}
	}
	return false, robotFound
}

// SensorStreamCapabilitiesUnmet returns the capabilities streamID declares as
// required that are NOT Active on the named robot, sorted. It is the message
// half of the rule SensorStreamOffered enforces: an empty result on a robot
// that SensorStreamOffered refused means the robot does not declare the stream
// at all, and a non-empty one names exactly what an operator has to fix.
//
// Nil when the robot is not among the candidates or does not declare the
// stream — "no capability is missing" and "there is no stream here" are
// different facts and the caller must not read the first for the second.
func SensorStreamCapabilitiesUnmet(candidates []fleetv1.Robot, robotName, streamID string) []string {
	for _, r := range candidates {
		if r.Name != robotName {
			continue
		}
		for _, s := range r.Status.SensorStreams {
			if s.StreamID == streamID {
				return unmetStreamCapabilities(&r, &s)
			}
		}
	}
	return nil
}

// unmetStreamCapabilities is the one implementation of "required_capabilities
// are all Active", so the predicate and the message can never disagree about
// which capabilities count. Degraded does NOT count, and deliberately: the
// action-level acceptDegraded flag is an author's statement about the ACTION's
// tolerance for reduced performance, and a stream's required capability is the
// adapter's statement about what the SENSOR needs to produce frames at all.
func unmetStreamCapabilities(robot *fleetv1.Robot, stream *fleetv1.SensorStreamEntry) []string {
	if len(stream.RequiredCapabilities) == 0 {
		return nil
	}
	active := make(map[string]bool, len(robot.Status.Capabilities))
	for _, c := range robot.Status.Capabilities {
		if c.Status == fleetv1.CapabilityStatusActive {
			active[c.Name] = true
		}
	}
	var unmet []string
	for _, required := range stream.RequiredCapabilities {
		if !active[required] {
			unmet = append(unmet, required)
		}
	}
	sort.Strings(unmet) // deterministic: an unchanged robot must yield an unchanged message (RA-1)
	return unmet
}
