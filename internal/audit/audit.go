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

// Package audit is the tamper-evident, hash-chained safety audit log (RFC-0001
// §9.5.4, §9.6.5). Entries form a per-namespace chain: each entry's chain_hash is
// SHA-256(previous chain_hash ‖ this entry's content), so any modification,
// deletion, reordering, or sequence gap is detectable by Verify. Denied actions
// are recorded, never dropped (§9.5.4).
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrSinkNotResumable is returned by [Log.Resume] when the configured sink cannot be
// read back, so the chain necessarily restarts at genesis on every process restart.
var ErrSinkNotResumable = errors.New("audit sink cannot be resumed; the chain restarts at genesis on each restart")

// genesisHash seeds each namespace's chain (§9.6.5.2 — genesis chain_hash is all
// zeros). It is the prev-hash for the first real entry.
const genesisHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// logVersion is the audit schema version stamped on every entry.
const logVersion = "1"

// ActorType classifies the identity that caused an event (§9.5.4).
type ActorType string

// ActorType values.
const (
	ActorUser           ActorType = "user"
	ActorServiceAccount ActorType = "service-account"
	ActorRobot          ActorType = "robot"
)

// Outcome is the result of an audited action (§9.5.4). Denied actions are always
// recorded.
type Outcome string

// Outcome values.
const (
	OutcomeAllowed Outcome = "Allowed"
	OutcomeDenied  Outcome = "Denied"
	OutcomeError   Outcome = "Error"
)

// Required audit event types (§9.5.4, §9.6.5.1) — the subset wired so far plus the
// safety/security events; more producers append using these constants.
const (
	EventRobotAuthzDenied     = "ROBOT_AUTHZ_DENIED"
	EventRobotAdmitted        = "ROBOT_ADMITTED"
	EventRobotRejected        = "ROBOT_REJECTED"
	EventEstopTriggered       = "ESTOP_TRIGGERED"
	EventEstopCleared         = "ESTOP_CLEARED"
	EventEstopClearRejected   = "ESTOP_CLEAR_REJECTED"
	EventFleetActionCancelled = "FLEETACTION_CANCELLED"
	EventFirmwareRolloutCreat = "FIRMWARE_ROLLOUT_CREATED"
	EventModelRolloutCreated  = "MODEL_ROLLOUT_CREATED"
	EventSwarmadaConfigMod    = "SWARMADA_CONFIG_MODIFIED"

	// Rollout pause and operator resume (§9.6.5.1). A paused rollout has stopped
	// dispatching to a fleet mid-update, and the resume that releases it is an operator
	// OVERRIDE of an automated safety halt — it abandons the robots that failed rather
	// than retrying them (ADR-0041). Both halves are sealed: an incident review needs to
	// know that the halt fired, and that a human chose to proceed past it and past which
	// robots.
	EventFirmwareRolloutPaused = "FIRMWARE_ROLLOUT_PAUSED"
	EventModelRolloutPaused    = "MODEL_ROLLOUT_PAUSED"
	EventRolloutResumed        = "ROLLOUT_RESUMED"

	// Robot connectivity and capability lifecycle (§9.6.5.1). These seal the
	// transitions an incident reconstruction turns on: when a robot dropped, how long
	// it was gone, when it came back, and when a capability stopped being schedulable.
	EventRobotOffline       = "ROBOT_OFFLINE"
	EventRobotCritical      = "ROBOT_CRITICAL"
	EventRobotReconnected   = "ROBOT_RECONNECTED"
	EventCapabilityDegraded = "CAPABILITY_DEGRADED"

	// ZoneMaintenance window lifecycle (§9.6.5.1). A maintenance window is the
	// administrative record that robots were deliberately taken out of service — the
	// counterpart an incident review reads next to the estop entries, and the thing that
	// distinguishes "the fleet stopped" from "the fleet was stopped on purpose".
	EventZoneMaintenanceActivated   = "ZONE_MAINTENANCE_ACTIVATED"
	EventZoneMaintenanceDeactivated = "ZONE_MAINTENANCE_DEACTIVATED"
	// EventActionRequeuedByMaintenance records maintenance interrupting in-flight work.
	// Named for what the control plane actually does — the action returns to Pending and is
	// re-schedulable elsewhere — rather than Paused, which is the estop disposition and
	// binds the action to the robot it was taken from.
	EventActionRequeuedByMaintenance = "ACTION_REQUEUED_BY_MAINTENANCE"

	// EventActionPausedByEstop records an in-flight action halted by an emergency stop.
	// Unlike the maintenance requeue above, a Paused action stays bound to its robot and
	// is never auto-resumed: an operator decides (§9.6.2.4).
	EventActionPausedByEstop = "ACTION_PAUSED_BY_ESTOP"

	// Firmware install and artifact-verification lifecycle (§9.6.5.1). Signature outcomes
	// are the most safety-relevant records this specification defines: they are the
	// evidence that unverified code was never dispatched to a robot, which is exactly the
	// claim a safety case has to be able to make.
	EventFirmwareInstallStarted   = "FIRMWARE_INSTALL_STARTED"
	EventFirmwareInstallSucceeded = "FIRMWARE_INSTALL_SUCCEEDED"
	// EventFirmwareInstallFailed records a CONFIRMED install failure reported by the
	// robot (ADR-0033). A rollout abandoning an unresponsive robot is not this event.
	EventFirmwareInstallFailed     = "FIRMWARE_INSTALL_FAILED"
	EventFirmwareSignatureVerified = "FIRMWARE_SIGNATURE_VERIFIED"
	EventFirmwareSignatureFailed   = "FIRMWARE_SIGNATURE_FAILED"

	// Model update and artifact-verification lifecycle (§9.6.5.1). Model signature
	// outcomes are reported by the ADAPTER on the model_update acknowledgement, unlike
	// firmware's, which the control plane verifies itself before dispatch — so these
	// entries record what the robot attested, not what this process checked.
	EventModelUpdateStarted     = "MODEL_UPDATE_STARTED"
	EventModelUpdateSucceeded   = "MODEL_UPDATE_SUCCEEDED"
	EventModelUpdateFailed      = "MODEL_UPDATE_FAILED"
	EventModelSignatureVerified = "MODEL_SIGNATURE_VERIFIED"
	EventModelSignatureFailed   = "MODEL_SIGNATURE_FAILED"

	// EventEstopLatencyViolation records an estop acknowledgement that missed the 500 ms
	// SLA (§9.6.2.2). The stop still happened — this is not a failed estop — but the
	// margin an operator was relying on did not, and that is a durable fact about an
	// adapter's conformance rather than a transient operational blip.
	EventEstopLatencyViolation = "ESTOP_LATENCY_VIOLATION"

	// EventProbeFailure records an active health check crossing its failure threshold.
	// A single failed probe is noise; a SUSTAINED failure is what demotes a component and
	// takes capabilities out of scheduling, which is the fact worth sealing.
	EventProbeFailure = "PROBE_FAILURE"
)

// Actor is who caused an event.
type Actor struct {
	Type     ActorType `json:"type"`
	Identity string    `json:"identity"`
	SourceIP string    `json:"source_ip,omitempty"`
}

// Resource is the object an event acted on.
type Resource struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Entry is one immutable audit record (§9.5.4 / §9.6.5.2 fields). SequenceNumber,
// EventTime, LogVersion, SwarmadaVersion, and ChainHash are set by Record.
type Entry struct {
	SequenceNumber  uint64            `json:"sequence_number"`
	EventType       string            `json:"event_type"`
	EventTime       string            `json:"event_time"`
	Namespace       string            `json:"namespace"`
	Actor           Actor             `json:"actor"`
	Resource        Resource          `json:"resource"`
	Action          string            `json:"action"`
	Outcome         Outcome           `json:"outcome"`
	Detail          map[string]string `json:"detail,omitempty"`
	SwarmadaVersion string            `json:"swarmada_version"`
	LogVersion      string            `json:"log_version"`
	ChainHash       string            `json:"chain_hash"`
}

// Sink is the append-only backing store for sealed entries. Implementations MUST
// be safe for concurrent use. A durable/external (SIEM, append-only file) sink and
// retention (§9.6.5.4) are deployment concerns; MemorySink suffices for tests/dev.
type Sink interface {
	Append(Entry) error
}

// ResumableSink is a Sink that can report where each namespace's chain left off.
//
// Without this the chain is only tamper-evident WITHIN one process lifetime: Log keeps
// the running sequence and previous hash in memory, so a restarted control plane starts
// every namespace again at sequence 1 chained to genesis. An auditor verifying the file
// then sees the chain restart, and — worse — cannot distinguish that restart from an
// attacker truncating the log and re-sealing a shorter one, which is precisely the
// property §9.6.5.2 exists to provide.
//
// Optional: a Sink that cannot be read back (a write-only SIEM forwarder) simply does not
// implement it, and Log.Resume says so rather than pretending the chain is continuous.
type ResumableSink interface {
	Sink
	// Tail returns the highest-sequence entry recorded for each namespace, keyed by
	// namespace. A namespace with no entries is absent from the map.
	Tail() (map[string]Entry, error)
}

// Recorder is the write side depended on by event producers.
type Recorder interface {
	Record(Entry) (Entry, error)
}

// Log is the hash-chained audit log. Its zero value is not usable; use New.
type Log struct {
	mu      sync.Mutex
	sink    Sink
	state   map[string]*nsState // namespace → running seq + prev hash
	now     func() time.Time
	version string
}

type nsState struct {
	seq      uint64
	prevHash string
}

// New builds a Log backed by sink, stamping swarmadaVersion on entries.
func New(sink Sink, swarmadaVersion string) *Log {
	if swarmadaVersion == "" {
		swarmadaVersion = "v0.1.0"
	}
	return &Log{sink: sink, state: map[string]*nsState{}, version: swarmadaVersion}
}

// Resume seeds each namespace's chain from what the sink already holds, so a restarted
// control plane continues the existing chain instead of opening a second one at genesis.
//
// Returns ErrSinkNotResumable when the sink cannot be read back. That is a fact about the
// deployment, not a failure — the caller decides whether a chain that restarts on every
// process restart is acceptable for its safety case — but it must be surfaced, because the
// silent version of it is indistinguishable from a working one until an audit.
//
// Safe to call only before the first Record: it overwrites in-memory chain state.
func (l *Log) Resume() error {
	rs, ok := l.sink.(ResumableSink)
	if !ok {
		return ErrSinkNotResumable
	}
	tail, err := rs.Tail()
	if err != nil {
		return fmt.Errorf("reading audit chain tail: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for ns, e := range tail {
		l.state[ns] = &nsState{seq: e.SequenceNumber, prevHash: e.ChainHash}
	}
	return nil
}

func (l *Log) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// Record seals an entry into the namespace's chain and appends it. It assigns the
// sequence number (monotonic per namespace), event time, versions, and chain_hash.
// On a sink failure the chain is rolled back so a dropped write never advances the
// chain (a later entry can never silently chain over a missing one). Safe for
// concurrent use.
func (l *Log) Record(e Entry) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	st := l.state[e.Namespace]
	if st == nil {
		st = &nsState{seq: 0, prevHash: genesisHash}
		l.state[e.Namespace] = st
	}
	prevSeq, prevHash := st.seq, st.prevHash

	st.seq++
	e.SequenceNumber = st.seq
	if e.EventTime == "" {
		e.EventTime = l.clock().UTC().Format(time.RFC3339Nano)
	}
	e.LogVersion = logVersion
	e.SwarmadaVersion = l.version
	e.ChainHash = computeChainHash(prevHash, e)

	if err := l.sink.Append(e); err != nil {
		st.seq, st.prevHash = prevSeq, prevHash // roll back; the chain never advanced
		return Entry{}, err
	}
	st.prevHash = e.ChainHash
	return e, nil
}

// computeChainHash returns SHA-256(prev ‖ canonical-content-of-e), where the
// content excludes chain_hash itself. Canonicalization is deterministic:
// encoding/json emits struct fields in declaration order and map keys sorted.
func computeChainHash(prev string, e Entry) string {
	e.ChainHash = ""
	content, _ := json.Marshal(e)
	sum := sha256.Sum256(append([]byte(prev), content...))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// VerifyResult is the outcome of walking a namespace's chain.
type VerifyResult struct {
	OK             bool
	Entries        int
	SequenceGaps   int
	HashMismatches int
	GenesisOK      bool
}

// Verify walks one namespace's entries (ordered by sequence) and recomputes the
// chain. It reports any sequence gap, hash mismatch, or broken genesis — the
// signatures of a modified, deleted, or reordered entry. It recomputes each hash
// from the STORED previous hash so a single tampered entry is localised rather
// than cascading, giving an accurate mismatch count.
func Verify(entries []Entry) VerifyResult {
	res := VerifyResult{OK: true, GenesisOK: true, Entries: len(entries)}
	prev := genesisHash
	var expectedSeq uint64
	for i := range entries {
		e := entries[i]
		expectedSeq++
		if e.SequenceNumber != expectedSeq {
			res.SequenceGaps++
			res.OK = false
			expectedSeq = e.SequenceNumber // resync so a single gap isn't counted forever
		}
		if want := computeChainHash(prev, e); want != e.ChainHash {
			res.HashMismatches++
			res.OK = false
		}
		prev = e.ChainHash
	}
	if len(entries) > 0 && !strings.HasPrefix(entries[0].ChainHash, "sha256:") {
		res.GenesisOK = false
		res.OK = false
	}
	return res
}

// Tail reports the highest-sequence entry per namespace. Entries are appended in
// sequence order per namespace, so the last one seen for a namespace is its tail;
// the sequence comparison is kept anyway so the result does not depend on that.
func (m *MemorySink) Tail() (map[string]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]Entry{}
	for _, e := range m.entries {
		if cur, seen := out[e.Namespace]; !seen || e.SequenceNumber > cur.SequenceNumber {
			out[e.Namespace] = e
		}
	}
	return out, nil
}

// MemorySink is an in-memory append-only Sink for tests and dev.
type MemorySink struct {
	mu      sync.Mutex
	entries []Entry
}

// Append stores one sealed entry.
func (m *MemorySink) Append(e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

// Entries returns a copy of the stored entries in append order.
func (m *MemorySink) Entries() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Entry(nil), m.entries...)
}

// ForNamespace returns a namespace's entries in append (sequence) order.
func (m *MemorySink) ForNamespace(namespace string) []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	for _, e := range m.entries {
		if e.Namespace == namespace {
			out = append(out, e)
		}
	}
	return out
}
