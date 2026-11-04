# ADR-0065: `SupportedAction.results[]` reuses `ActionParam`, and an empty `results` is silence rather than a claim of no output

- **Status:** accepted
- **Date:** 2026-09-16
- **Deciders:** Principal Software Architect, API Designer
- **Related:** ADR-0019 (adapter action discovery and validation), ADR-0032 (contract versioning and conformance gating), ADR-0039 (observation state on CapabilitiesSnapshot), ADR-0063 (a field this tree cannot evaluate is absent, not inert); `proto/fleet_adapter/v1/fleet_adapter.proto` (`SupportedAction.results`), `api/v1/fleetadapter_types.go:213`

## Context

ADR-0019 gave adapters a pulled catalog of supported actions, answering "what kinds of
action can this adapter serve at all?" The catalog describes what an action **takes**:
`ActionParam` entries for name, unit, kind, numeric range, enum values, required.

Measured 2026-09-15: **`SupportedAction` says nothing about what an action
produces**, so a static
parameter-binding check — "this task binds step 2's input to step 1's output; does step 1
emit that key?" — **cannot run for any binding onto a robot action**. The binding webhook
records the same gap from the other side: `internal/webhook/fleettask_bindings.go` lists
S2 (the field is not in the producer's declared outputs) among the rules "NOT ENFORCED
HERE", needing "the producer's declared output set resolved through the robot
`SupportedAction` catalog".

A second fact decided where the field had to land. **The binding validator resolves
against the `SupportedAction` catalog on `FleetAdapter.status`, not against the wire.**
A proto field with no CRD mirror could therefore never reach the consumer it was added
for.

A third fact bounds the compatibility story: every adapter built before this field will
send `results` empty, and so will every adapter that has nothing to say about
its outputs.

## Decision

**Add `repeated ActionParam results = 5;` to `fleet_adapter.v1.SupportedAction`,
reusing the existing descriptor rather than inventing a second shape**, and mirror it
onto the CRD as `SupportedAction.Results []ActionParam`.

- **Reuse, not a new message.** Each reused field is given its natural output reading:
  `name` is the result key, `allowed` is the closed set an enum result can take,
  `required` means the action always emits this key.
- **Additive by construction.** Field 5 was never previously used and there is no
  `reserved` statement; the message previously used 1–4.
- **Both halves, or neither.** The proto field alone would be inert (ADR-0063), because
  the consumer reads the CRD projection.
- **An empty `results` means "makes no statement about outputs" — never "produces
  nothing".** This is the compatibility rule a checker MUST honour; the alternative
  reading would reject every binding onto every adapter predating the field.

## Alternatives considered

- **A new `ActionResultParam` message** with output-specific fields (nullability,
  cardinality, a richer schema). Rejected: it doubles the descriptor surface
  adapters must populate and the control plane must project, for expressiveness nothing
  in scope consumes. `ActionParam`'s fields already carry the needed readings, and
  ADR-0019 already defers rich payload schemas to RFC-0003.
- **Infer outputs from the action `type`** via a central registry in the control plane.
  Rejected: it puts vendor-specific knowledge in the control plane, which is what the
  pulled catalog exists to avoid, and it cannot describe a vendor-private action type.
- **Treat an empty `results` as "this action produces nothing"** and reject bindings
  onto it. Rejected — this is the load-bearing rejection. It would be a breaking change
  to every adapter built before the field existed, and it makes silence
  indistinguishable from a claim.
- **Emit a warning when a binding resolves against an empty `results`.** Rejected for
  the same reason in a quieter form: since an empty `results` is contractually
  indistinguishable from "no statement", every adapter predating the field would warn
  on every binding, and a warning that always fires is noise.
- **Proto field only, and let the ingestor read the wire at validation time.** Rejected:
  the validator is an admission webhook with no ControlStream, and a synchronous
  round-trip to an adapter at admission is the coupling ADR-0019 rejected already.
- **CRD field only.** Rejected: an adapter has no way to populate it.

## Consequences

- Static parameter-binding checks onto robot actions become *possible*. They are not
  yet *running* — see the gap below.
- Adapters gain an optional obligation: declaring outputs is opt-in, and declining is
  conformant. Nothing forces a vendor to describe what it emits.
- **The field is inert end-to-end until the ingestor projects it, and this is a known
  open gap.** `mapSupportedActions` (`internal/controller/capabilities_ingestor.go:291`)
  builds its per-action literal from four fields and never reads `sa.GetResults()`, so
  an adapter that declares its outputs has them **discarded at the control-plane
  boundary**. The failure is quiet rather than loud precisely
  because of the compatibility rule above — no checker can flag the empty case without
  breaking every pre-existing adapter. This is the exact shape ADR-0063 warns about,
  and it is accepted here only because the field's own documentation states that an
  empty value is not a claim, and because the gap is on the register with a named fix.
- The CRD `ActionParam` mirror still omits `min`/`max`, an intentional v0.1
  coarse-projection decision that predates there being an output descriptor at all.
  Left alone deliberately and named as an option against the projection gap above, so
  that revisiting it is a decision rather than a side effect.
- `buf`'s breaking-change check did not run in the session that made the change (`no
  'main' branch — skipping`), so the additive-ness of field 5 is argued **structurally**
  — the message used 1–4, no `reserved` statement, 5 free — rather than machine-verified.
- RA-1 holds: `status.supportedActions[].results` is written on a capabilities snapshot
  (a discovery event pulled via `Command.scan`), never on a telemetry tick.
