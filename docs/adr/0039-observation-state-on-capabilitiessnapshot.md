# ADR-0039: An observation-state signal on `CapabilitiesSnapshot`

- **Status:** proposed
- **Date:** 2026-08-16
- **Deciders:** _(pending)_
- **Related:** ADR-0032 (contract versioning and conformance gating — the bump rules applied below), RFC-0001 §9 (capability rescan), `adapters/CONFORMANCE.md` C6.2 (explicit presence on safety scalars) and C12.1 (`scan`)

## Context

`CapabilitiesSnapshot.hardware` is a bare `repeated HardwareComponent`. An empty list has
exactly one encoding on the wire, and it is asked to carry two different meanings:

* "this robot has **no** hardware components", and
* "I have **not observed** the hardware yet".

This is not hypothetical. `scan` (C12.1) became a required command only in a late revision, and the first
adapter to implement it — `fleet-adapter-ros2` — learns hardware from `/diagnostics` at 1 Hz.
Between process start and the first message its cache is empty, so a `scan` arriving in that
window must answer something. Both available answers are wrong in a different way:

| answer | consequence |
|---|---|
| **empty list** (what the adapter does) | RFC-0001 §9 has the Zone Controller compare the response **field-by-field** against stored `Robot.status.hardware[]` and write back discrepancies. An empty snapshot is a diff against everything, so it can **clear a previously-good inventory** for a robot whose hardware is fine. The next scan then reads as hardware appearing from nowhere. |
| **a fabricated healthy inventory** | writes components into `Robot.status.hardware[]` that nobody measured, with a status nobody read, which every future scan is then compared against. Fiction, recorded as fact. |

The adapter chose empty and documents why: an invented inventory is unfalsifiable and
propagates, whereas an empty one is at least *true about what the adapter knows*. **That was the
right call, and it is what made the gap visible** — the adapter is telling the truth as
precisely as the wire allows, and the wire cannot carry it.

The inconsistency is internal to the message. Every field *inside* `HardwareComponent` is
`optional` for proto3 explicit presence, precisely so "not reported" is distinguishable from a
value. The container contradicts its own contents: it has no way to say "not reported".

Documenting "empty means unobserved" in prose does not fix it. The control plane acts on the
answer, and a rule the consumer must remember is a rule every consumer must reimplement.

## Decision

**Add an observation-state signal to `CapabilitiesSnapshot`, as a proto3 scalar with explicit
presence** — the pattern this contract already uses for exactly this problem:

```protobuf
// BatteryStatus, today:
optional bool charging = 2;   // absent = unknown, NOT "false"
```

Proposed shape (field numbers 1–6 are taken; 7 and 8 are free):

```protobuf
message CapabilitiesSnapshot {
  // ... robot_id = 1, hardware = 2, installed_models = 3,
  //     snapshot_ms = 4, supported_actions = 5, firmware = 6 ...

  // Whether `hardware` above reflects an actual observation.
  //   absent → the adapter does not state observation status. A consumer MUST NOT
  //            infer either meaning, and MUST NOT clear a stored inventory on an
  //            empty list. (Every adapter predating this field.)
  //   true   → `hardware` is what the adapter has observed. An EMPTY list therefore
  //            means the robot genuinely reports no components.
  //   false  → the adapter has not observed hardware yet. An empty list carries NO
  //            information and MUST NOT clear a stored inventory.
  optional bool hardware_observed = 7;

  // The same question for `installed_models`, which has the same failure mode for any
  // adapter that learns its model inventory asynchronously.
  optional bool installed_models_observed = 8;
}
```

**Two scalars rather than one enum.** An enum (`COMPLETE` / `NOT_YET_OBSERVED` / `PARTIAL`)
carries more, but it re-creates the same ambiguity one level up: a single value cannot describe
two independent lists, and `PARTIAL` would immediately need "partial in which list, and which
part". Two booleans keep each answer attached to the list it is about. If a third state is ever
genuinely needed for one of them, `optional` leaves room to add a sibling field without
breaking anything.

### The version question, from ADR-0032's rules

ADR-0032 says the contract "must be bumped deliberately when the proto surface, the
`SupportedAction` schema, or the conformance-suite revision changes", and that:

> Supported range is **N and N-1 minor** within the current major; a **major** bump is breaking
> and requires re-qualification. Patch and minor releases never break an adapter, so they never
> invalidate its qualification.

Applying that here:

* Adding an `optional` field is **additive and wire-compatible** under the proto3 unknown-field
  rule. An adapter built against 1.0.0 keeps working: it does not set the field, and `absent`
  is defined above as exactly the behaviour those adapters already have.
* It is a change to the proto surface, so it **does require a deliberate bump** — it is not
  free.
* It does **not** break an adapter, therefore **minor**: `fleet_adapter.v1` contract
  **1.0.0 → 1.1.0**. No re-qualification; existing conformance results stay valid; a 1.1.0
  control plane still supports 1.0 adapters under the N/N-1 rule.

**The trap, and it is the reason this cannot be waved through as "additive".** A *new
conformance MUST* requiring adapters to set the field would fail every existing adapter, which
is breaking behaviour reached through a non-breaking wire change. ADR-0032's bump rules key on
the wire, not on the suite's expectations, so the rules alone would let a minor bump invalidate
qualifications — the thing they exist to prevent.

Therefore the accompanying conformance check lands as **SHOULD**, satisfied by any of the three
states including absent, and tightens to MUST only on a subsequent **major** bump where
re-qualification is already expected. The field is useful to a control plane the moment any
adapter sets it; it does not need to be universal to be worth having.

## Consequences

- **The control plane can stop guessing.** An empty `hardware` with `hardware_observed = false`
  is unambiguously "no information", so the RFC-0001 §9 comparison can skip the write-back
  instead of clearing a good inventory. That rule becomes one place in the Zone Controller
  rather than a convention every consumer reimplements.
- **A cold-start conformance case becomes writable.** Today no check issues `scan` before any
  telemetry has flowed, so the ambiguity is not only unresolved — it is untested. With the
  field there is something to assert.
- **Two more fields on a message that already has six.** Small, and the alternative is a
  semantic rule that lives only in prose.
- **Adapters that never learn hardware asynchronously gain nothing** and may leave both absent.
  That is the intended cost of `optional`.
- **This ADR does not change any code.** The proto, the generated stubs, `CONFORMANCE.md` and
  RFC-0001 are untouched. Landing it requires, in order: the proto edit and regenerated stubs, a
  contract bump to 1.1.0 with the release→contract map updated, `CONFORMANCE.md` C12.1 text, the
  RFC-0001 capability-rescan text, and the SHOULD-level check.

## Alternatives considered

- **Document that empty means unobserved.** Rejected: the control plane acts on the answer, and
  a documented ambiguity is still an ambiguity. It also leaves "the robot genuinely has no
  components" unexpressible.
- **A control-plane rule that an empty `hardware` never clears a non-empty inventory.** Cheapest
  — no proto change — but it makes "genuinely no components" permanently unrepresentable, and it
  puts the workaround in every consumer instead of on the wire.
- **Have the adapter withhold the `scan` reply until it has observed hardware.** Rejected:
  `scan` is a required command (C12.1) and not answering is precisely what that check forbids;
  it also re-creates the `ScanCapabilitiesTimeout` warning loop RFC-0001 §9 describes.
- **Have the adapter block briefly waiting for the first observation.** Hides a data question
  behind latency, converts it into a timeout question, and still has to answer something if
  nothing arrives.
- **An enum instead of two scalars.** Covered under *Decision*.
