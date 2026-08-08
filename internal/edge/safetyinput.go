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

package edge

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

// SafetyInput is a physical local safety signal wired to the edge node — a light
// curtain, an e-stop button, or a safety-PLC digital output (§9.6.2.5). Tripped
// reports whether the input is CURRENTLY asserting an emergency stop. Implementations
// wrap the real hardware; the reference FileSafetyInput reads a digital line exposed
// as a file (e.g. Linux sysfs GPIO). Wiring the actual line is a manual step.
type SafetyInput interface {
	// Name identifies the input in logs and estop reasons.
	Name() string
	// Tripped is true when the input is asserting a stop. A non-nil error means the
	// line could not be read; the InputMonitor treats that fail-safe (as tripped).
	Tripped(ctx context.Context) (bool, error)
}

// FileSafetyInput reads a digital input exposed as a file whose contents are "0" or
// "1" — the shape of a Linux sysfs GPIO value file (/sys/class/gpio/gpioN/value) or a
// safety-PLC output bridged to a file. With ActiveLow set (the default for a
// normally-closed fail-safe circuit) the line reads LOW (0) when tripped and HIGH (1)
// when safe, so a cut wire or lost power reads as tripped.
type FileSafetyInput struct {
	Path      string
	ActiveLow bool
}

// Name returns the file path.
func (f *FileSafetyInput) Name() string { return f.Path }

// Tripped reads the line and maps its level to a stop assertion.
func (f *FileSafetyInput) Tripped(context.Context) (bool, error) {
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return false, fmt.Errorf("read safety input %q: %w", f.Path, err)
	}
	switch strings.TrimSpace(string(raw)) {
	case "1":
		return !f.ActiveLow, nil // high: tripped only if active-high
	case "0":
		return f.ActiveLow, nil // low: tripped only if active-low (normally-closed)
	default:
		return false, fmt.Errorf("safety input %q: unexpected level %q (want 0 or 1)", f.Path, strings.TrimSpace(string(raw)))
	}
}

// DefaultSafetyPollInterval is the poll cadence when none is set. A physical safety
// input should be polled tightly; 100ms bounds worst-case detection latency.
const DefaultSafetyPollInterval = 100 * time.Millisecond

// InputMonitor polls a SafetyInput and fires an edge-node estop on a rising edge
// (safe→tripped) via trigger, which the binary wires to Node.TriggerLocalEstop. It is
// edge-triggered: a held input asserts once (the Node then runs the confirmed-only
// estop discipline unchanged). A read error is treated FAIL-SAFE as tripped — an
// unreadable input must stop robots, not be silently ignored.
type InputMonitor struct {
	input    SafetyInput
	trigger  func(ctx context.Context, reason string)
	interval time.Duration
	log      logr.Logger

	lastTripped bool // edge-detection state
	fires       int  // rising edges acted on (observability/tests)
}

// NewInputMonitor builds a monitor. A non-positive interval uses the default.
func NewInputMonitor(input SafetyInput, trigger func(context.Context, string),
	interval time.Duration, log logr.Logger) *InputMonitor {
	if interval <= 0 {
		interval = DefaultSafetyPollInterval
	}
	return &InputMonitor{input: input, trigger: trigger, interval: interval, log: log}
}

// Run polls until ctx is cancelled. A poll evaluates the input; on a transition into
// the tripped state it triggers a zone-wide local estop.
func (m *InputMonitor) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		m.poll(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (m *InputMonitor) poll(ctx context.Context) {
	tripped, err := m.input.Tripped(ctx)
	if err != nil {
		// FAIL SAFE: an unreadable safety input is treated as tripped.
		m.logger().Info("safety input unreadable; treating as TRIPPED (fail-safe)",
			"input", m.input.Name(), "error", err.Error())
		tripped = true
	}
	if tripped && !m.lastTripped {
		m.fires++
		m.logger().Info("local safety input asserted; triggering zone-wide estop", "input", m.input.Name())
		m.trigger(ctx, fmt.Sprintf("safety input %q asserted", m.input.Name()))
	} else if !tripped && m.lastTripped {
		m.logger().Info("local safety input cleared", "input", m.input.Name())
	}
	m.lastTripped = tripped
}

func (m *InputMonitor) logger() logr.Logger {
	if m.log.GetSink() == nil {
		return logr.Discard()
	}
	return m.log
}
