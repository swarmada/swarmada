// Copyright 2026 The Swarmada Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/swarmada/swarmtop/internal/k8sclient"
)

func TestNewFocused_OpensDetailWhenRobotPresent(t *testing.T) {
	m := step(NewFocused(k8sclient.NewStaticStore(sampleFleet()), "robot-3"),
		tea.WindowSizeMsg{Width: 120, Height: 40})

	if m.mode != modeDetail {
		t.Fatalf("--robot should open detail view, got mode %v", m.mode)
	}
	if r := m.selectedRobot(); r == nil || r.Name != "robot-3" {
		t.Fatalf("--robot should select robot-3, got %+v", r)
	}
}

func TestNewFocused_AppliesOnceRobotAppears(t *testing.T) {
	// Live store starts empty; the robot shows up in a later snapshot.
	store := k8sclient.NewStaticStore(k8sclient.Fleet{})
	m := step(NewFocused(store, "robot-3"), tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.mode == modeDetail {
		t.Fatal("should not focus before the robot exists")
	}

	store.Set(sampleFleet())
	m = step(m, fleetMsg{store.Snapshot()})

	if m.mode != modeDetail || m.selectedRobot() == nil || m.selectedRobot().Name != "robot-3" {
		t.Fatalf("focus should apply once robot-3 appears, mode=%v", m.mode)
	}
}

func TestNewFocused_EmptyNameIsNormalList(t *testing.T) {
	m := step(NewFocused(k8sclient.NewStaticStore(sampleFleet()), ""),
		tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.mode != modeList {
		t.Fatalf("empty --robot should stay on the list, got %v", m.mode)
	}
}

// Guard against an unknown robot name leaving focus pending forever without
// crashing: it simply never opens detail.
func TestNewFocused_UnknownRobotStaysList(t *testing.T) {
	m := step(NewFocused(k8sclient.NewStaticStore(sampleFleet()), "does-not-exist"),
		tea.WindowSizeMsg{Width: 120, Height: 40})
	m = step(m, tickMsg(time.Now()))
	if m.mode != modeList {
		t.Fatalf("unknown --robot should stay on the list, got %v", m.mode)
	}
}
