# ADR-0031: Model an intentionally-disabled hardware component as `HardwareStatus=Disabled`

- **Status:** accepted
- **Date:** 2026-07-25
- **Deciders:** Control Plane / CRD-proto + controller capability
- **Related:** RFC-0001 §5.2 (Robot hardware), §6.10 (capability derivation truth table), §9.1.11 (health), §9.3.8 (status-write throttle / RA-1); `fleet_adapter.v1 HardwareStatus`

## Context

A component the user intentionally turns **off** (an operator or edge toggle — a
robot's camera switched off, a disabled sensor) has no dedicated wire value, so
adapters report it as `HARDWARE_STATUS_FAILED`. That conflates a deliberate,
benign, reversible state with a fault, and it does real damage at two sites:

1. **Capability gate** (`robot_controller.go`): a `RequiredHardware` component
   reported `Failed` deactivates the capability with reason
   `"required hardware failed: <name>"` — the UI shows a fault for a sensor the
   user chose to turn off.
2. **Status-write throttle** (`mergeHardware` in `projector.go`): `Failed`/
   `Degraded` deltas set the projector's `critical` flag, which **bypasses** the
   RA-1 write throttle (§9.3.8). So a routine toggle-off writes status immediately
   and reads as a hardware fault, and a user flipping a sensor can amplify writes.

Intentional-off and hard-failure are different states — one is expected and
reversible, the other needs attention — and must be modeled as such. A reason
string bolted onto `Failed` is not enough, because the throttle/critical decision
is made on the enum value, not by parsing text.

## Decision

Introduce a fourth `HardwareStatus` value, **`Disabled`** — "component
intentionally not in service" — distinct from `Failed` (fault) and `Degraded`
(impaired), alongside `Healthy`.

- **Wire enum:** add `HARDWARE_STATUS_DISABLED = 4` to `fleet_adapter.v1
  HardwareStatus`. Appending an enum value is backward compatible: adapters that
  never send it are unaffected.
- **CRD:** add `HardwareDisabled = "Disabled"` to the `HardwareStatus` type and its
  validation enum.
- **Semantics:**
  - **(a) Gate.** A capability whose `RequiredHardware` is `Disabled` is `Inactive`
    with reason **`"disabled: <name>"`** — evaluated *before* the `Failed` case, so
    an off component never reads as failed.
  - **(b) Not critical.** `Disabled` does **not** set the projector's `critical`
    flag, does **not** raise `ConnectivityCritical`, and does **not** bypass the
    write throttle. A toggle-off is a normal non-critical material change, throttled
    like any other (`mergeHardware` keeps `critical` for `Degraded`/`Failed` only).
  - **(c) Reported presence.** `Disabled` is a reported status that appears in
    `status.hardware`, so the gate sees the component and says `"disabled: …"` — not
    `"required hardware not reported: …"`. Off is not absence.
- **Join key + RA-1 unaffected.** This stays a throttled status projection; no new
  status writes, none on a telemetry tick. `Disabled` changes flow through the same
  material-change throttle as any non-critical delta.

## Alternatives considered

- **Overload `Failed` with a reason string ("intentionally off").** Rejected: the
  `critical`/throttle decision is made on the enum, not the text — the projector
  would still flag it critical and bypass the throttle. Reason strings are for
  humans, not for control-flow.
- **A separate boolean/annotation `disabled` beside the status.** Rejected: two
  sources of truth for one component's state. The gate and projector already read a
  single status enum; adding a parallel flag invites disagreement between them.
- **Model off as absence (omit the component from the report).** Rejected: absence
  is "not reported" — the gate would say `"required hardware not reported"`, a
  different and worse message, and it discards the fact that the component exists but
  is off. A user who turns a sensor back on should see it transition
  `Disabled → Healthy`, which requires it to be present while off.

## Consequences

- A toggled-off required sensor reads `Inactive / "disabled: <name>"` (benign,
  throttled) instead of `Inactive / "required hardware failed: <name>"` (fault,
  critical, write-amplifying). `Degraded` still degrades; `Failed` still faults and
  is critical.
- **New obligations:** every hardware-status mapping site
  (`controlstream/translate.go`, `registrar/registrar.go`) must map the new value;
  the gate must handle `Disabled` **before** `Failed`; `mergeHardware` must keep
  `Disabled` non-critical (satisfied by leaving its critical predicate on
  `Degraded`/`Failed` only).
- **Backward compatible wire change:** existing adapters are unaffected; adapters
  that adopt the semantics report `Disabled` for toggled-off components.
- **RA-1 preserved:** `Disabled` deltas are throttled like any non-critical change,
  so a user flipping a sensor cannot amplify status writes.
- Generated proto stubs are regenerated via the repo's `make proto-go`; generated
  files are never hand-edited.
