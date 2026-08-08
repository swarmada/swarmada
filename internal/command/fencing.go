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

package command

import (
	"math"
	"time"
)

// Fencing-token conversions between the CRD and the wire.
//
// FleetAction.status.assignmentGeneration is an int64; the proto's fencing_token is a uint64. A
// bare conversion in either direction is a latent trap, because the fencing rule is "a token lower
// than the highest already accepted is STALE and refused":
//
//   - int64 -> uint64 on a NEGATIVE generation yields ~1.8e19. That is not merely wrong, it is
//     unrecoverable: the adapter persists the highest token it has accepted, so one poisoned
//     assignment leaves a robot refusing every subsequent one — no later generation can exceed it.
//   - uint64 -> int64 above MaxInt64 wraps negative, and the negative value would then compare
//     unequal to a legitimate generation.
//
// Neither is reachable in normal operation (generations start at 0 and only increment), but
// `status` is writable by anything holding status permission, so "unreachable" rests on every
// writer being correct rather than on the type.

// FencingToken converts an assignment generation to its wire form, failing CLOSED.
//
// A negative generation is corrupt state; it becomes 0 — the lowest possible token, which an
// adapter refuses as stale. That loses one assignment and is visibly wrong in logs, where the
// wrapped value would silently and permanently disable the robot.
func FencingToken(generation int64) uint64 {
	if generation < 0 {
		return 0
	}
	return uint64(generation)
}

// GenerationFromToken converts a wire fencing token back to a generation.
//
// A token above MaxInt64 cannot correspond to any generation this control plane issued, so it is
// reported as not-representable rather than wrapped into a negative number that might accidentally
// compare equal to something.
func GenerationFromToken(token uint64) (int64, bool) {
	if token > math.MaxInt64 {
		return 0, false
	}
	return int64(token), true
}

// LeaseDurationMs converts a lease duration to the wire's uint32 milliseconds, clamping rather
// than wrapping.
//
// uint32 milliseconds overflows at ~49.7 days. A lease that long is a misconfiguration, but the
// bare conversion turns it into a SMALL number — so an operator asking for an absurdly long lease
// would get an almost-immediate self-stop, which looks like a robot fault rather than a bad config.
// Clamping keeps the error legible: the robot holds the longest lease the wire can express.
func LeaseDurationMs(d time.Duration) uint32 {
	ms := d.Milliseconds()
	if ms < 0 {
		return 0
	}
	if ms > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(ms)
}
