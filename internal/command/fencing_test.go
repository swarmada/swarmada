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
	"testing"
	"time"
)

// The fencing rule is "a token lower than the highest already accepted is stale and refused", and
// the adapter PERSISTS that high-water mark across reconnects. That makes an over-large token
// unrecoverable rather than merely wrong: nothing the control plane issues afterwards can exceed
// it, so the robot refuses every future assignment. These tests exist for that asymmetry — too low
// costs one assignment, too high costs the robot.

func TestFencingToken_NormalGenerations(t *testing.T) {
	for _, gen := range []int64{0, 1, 42, math.MaxInt64} {
		if got := FencingToken(gen); got != uint64(gen) {
			t.Errorf("FencingToken(%d) = %d, want %d", gen, got, uint64(gen))
		}
	}
}

// THE CASE THIS EXISTS FOR. A bare uint64(-1) is 18446744073709551615 — accepted by the adapter as
// the new high-water mark, after which no legitimate token is ever higher.
func TestFencingToken_NegativeFailsClosedNotOpen(t *testing.T) {
	for _, gen := range []int64{-1, -42, math.MinInt64} {
		got := FencingToken(gen)
		if got != 0 {
			t.Errorf("FencingToken(%d) = %d, want 0", gen, got)
		}
		if got > math.MaxInt64 {
			t.Errorf("FencingToken(%d) produced %d — an unreachably high token permanently "+
				"disables the robot, because the adapter keeps the highest token it has seen", gen, got)
		}
	}
}

func TestGenerationFromToken_Representable(t *testing.T) {
	for _, tok := range []uint64{0, 1, 42, math.MaxInt64} {
		got, ok := GenerationFromToken(tok)
		if !ok || got != int64(tok) {
			t.Errorf("GenerationFromToken(%d) = %d,%v, want %d,true", tok, got, ok, int64(tok))
		}
	}
}

// A token past MaxInt64 is reported not-representable rather than wrapped negative, so it can
// never accidentally compare equal to a real generation.
func TestGenerationFromToken_OverflowIsNotRepresentable(t *testing.T) {
	for _, tok := range []uint64{math.MaxInt64 + 1, math.MaxUint64} {
		if got, ok := GenerationFromToken(tok); ok {
			t.Errorf("GenerationFromToken(%d) = %d,true — want not-representable", tok, got)
		}
	}
}

// Round-tripping a legitimate generation is lossless, which is what the assignment path relies on.
func TestFencingToken_RoundTrip(t *testing.T) {
	for _, gen := range []int64{0, 1, 999, math.MaxInt64} {
		back, ok := GenerationFromToken(FencingToken(gen))
		if !ok || back != gen {
			t.Errorf("round trip of %d gave %d,%v", gen, back, ok)
		}
	}
}

func TestLeaseDurationMs_ClampsRatherThanWraps(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    time.Duration
		want uint32
	}{
		{"typical", 30 * time.Second, 30_000},
		{"zero", 0, 0},
		{"negative", -time.Second, 0},
		{"at the uint32 ceiling", time.Duration(math.MaxUint32) * time.Millisecond, math.MaxUint32},
		// ~49.7 days is where uint32 ms overflows. A bare conversion turns 60 days into a small
		// number, so the robot self-stops almost immediately and the misconfiguration reads as a
		// robot fault.
		{"beyond the ceiling", 60 * 24 * time.Hour, math.MaxUint32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LeaseDurationMs(tc.d); got != tc.want {
				t.Errorf("LeaseDurationMs(%v) = %d, want %d", tc.d, got, tc.want)
			}
		})
	}
}
