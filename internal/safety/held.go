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

package safety

import (
	"fmt"
	"sort"
	"strings"

	fleetv1 "github.com/swarmada/swarmada/api/v1"
	"github.com/swarmada/swarmada/internal/estopscope"
)

// HeldBy names the stop a robot's status holds, or returns "" when it holds none: an
// estop state that is set and not Normal, a stop the robot records (its own estop trigger,
// or a zone's or the namespace's stop in status.estopHeldBy, which hold it whatever its
// estop state reads), the Error phase, or a component reported Failed. Each is released only on its own path. An estop leaves only through an
// authorized clear and an operator's confirmed resume, and a Failed component only when the
// robot reports otherwise. Error leaves when the robot reports another phase, or when it
// falls silent and the Robot controller marks it Offline: Error is the robot's own report,
// which it makes again if the fault remains (RFC-0001 #safety-estop-state-machine).
//
// All three live only in Robot.status, so nothing carries them across a delete of the
// Robot: a robot deleted and created again comes back with none of them. The Robot
// admission gate refuses such a delete, and Offline auto-removal leaves such a robot in
// place; both read this.
func HeldBy(robot *fleetv1.Robot) string {
	var held []string
	if s := robot.Status.EstopState; s != "" && s != fleetv1.RobotEstopNormal {
		held = append(held, fmt.Sprintf("estopState %s", s))
	}
	if by, ok := estopscope.HeldOnRecord(robot); ok {
		held = append(held, by.String())
	}
	if robot.Status.Phase == fleetv1.RobotPhaseError {
		held = append(held, "phase Error")
	}
	var failed []string
	for _, hw := range robot.Status.Hardware {
		if hw.Status == fleetv1.HardwareFailed {
			failed = append(failed, hw.Name)
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		held = append(held, fmt.Sprintf("Failed %s %s", plural(len(failed), "component", "components"),
			strings.Join(failed, ", ")))
	}
	return strings.Join(held, "; ")
}
