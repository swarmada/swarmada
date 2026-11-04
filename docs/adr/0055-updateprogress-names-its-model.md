# ADR-0055: `UpdateProgress` names its model, and the correlation that replaced it declines to guess

- **Status:** accepted
- **Date:** 2026-09-06
- **Deciders:** Controller Engineer
- **Related:** ADR-0033 (`CapabilitiesSnapshot` as the recovery carrier — the path a refusal
  defers to), RFC-0001 #modelrollout/#design-details-why-is-modelrollout-rollback-manual-by-default (`ModelRollout`, `rollbackPolicy`), and the earlier
  lost-terminal-report fix that added the `targetSelector` fallback this decision extends.

## Context

`UpdateProgress` carried `robot_id`, `kind`, `phase`, `outcome`, `failure_reason` and
`resulting_version` — and **not** the name of the model it was reporting on. The control
plane recovered the model by asking which rollout the robot was in:

    for idx := range list.Items {
        ro := &list.Items[idx]
        if ro.Status.Phase != fleetv1.RolloutPhaseInProgress { continue }
        if batchIndexOf(ro.Status.CurrentBatch, robot.Name) >= 0 {
            return ro.Spec.ModelName, nil
        }
    }

The comment above it explained the choice: naming the model *"keeps the wire message small
and avoids a second source of truth for which model a robot is being moved to."* That
reasoning holds only while the question has one answer.

**It has more than one, and by design.** `classifyModel` and `upsertModelStatus`
(`modelrollout_controller.go`) key strictly on `modelName`. A robot marked `Updating` for
`nav-stack` therefore classifies as *pending* for `vision-stack`, becomes eligible, and
enters the second rollout's batch while still in the first. Two concurrent `ModelRollout`s
targeting one robot for two independent models is a **supported configuration**, and the
correlation above cannot represent it: it returns the first match, and the iteration order
is the API server's list order — name-sorted, which has nothing to do with which install
finished.

The advisory phase path (`applyModelPhase`) repeated the pattern verbatim, so a
`Downloading` from one model overwrote the other model's `updatePhase`.

**The consequence is not cosmetic.** `Robot.status.installedModels[]` is the fleet's record
of what each robot runs. A misattributed `Updating` or `Failed` takes a `model-driven`
capability off a robot that never lost it. A `resulting_version` written against the wrong
model both fails to complete the rollout that finished and corrupts the entry for the one
that did not. And under `rollbackPolicy: Auto` a `FAILED` outcome on the wrong model puts
that model in the failed set, so the rollback pass **pushes a revert of a model that never
failed** — on real hardware, an unnecessary firmware-class operation triggered by a
bookkeeping error.

**Not introduced by the lost-terminal-report fix**, which changed only *reachability*: before it,
terminal reports were dropped so consistently that the misattribution had little
opportunity to fire.

## Decision

**Name the model on the wire AND make the correlation decline to guess, because the two
compose and neither is sufficient alone.**

**1. `UpdateProgress` gains `string model_name = 8`.** A new optional field; no existing
field moved or renumbered, so the change is wire-compatible in both directions. An adapter
that implements `model_update` SHOULD set it and MUST set it if it can have two model
installs in flight. The reference adapter now does, on the advisory progress path and on
both terminal paths (success and refusal).

This reverses the earlier reasoning in writing rather than quietly overriding it: *"a
second source of truth for which model a robot is being moved to"* is precisely what was
needed, because the first source turned out to be ambiguous. A report is the robot's
account of its own state; the rollout list is the control plane's bookkeeping **about**
that state, and when they disagree about which model is in question the robot's account is
the one that was actually there.

**2. The correlation stays, and now declines rather than guessing.** An adapter that sets
no `model_name` is still correlated through `currentBatch` and, for SUCCEEDED outcomes,
through `targetSelector`. What changed is that every in-flight candidate is examined
instead of the first one found: **two candidates for different models and no reported name
returns `""`**, logged, and the ADR-0033 `CapabilitiesSnapshot` carrier settles it. A
silent wrong write is worse than a delayed right one. `applyModelPhase` does the same and
drops the advisory phase rather than writing it to the wrong rollout.

**3. `model_name` says WHICH model, never WHETHER to record.** This is the part that is
easy to get wrong. The lost-terminal-report fix deliberately correlates a **FAILED** outcome only
through `currentBatch`, because the reference adapter emits `INSTALL_OUTCOME_FAILED` when
it *refuses* an artifact — having installed nothing and changed nothing about itself — and
`enterBatch` never enters a declined robot into a batch. Recording that refusal would mark
the model Failed on a robot still happily running the previous version and pause every
rollout targeting it, which is the exact outcome the decline path exists to prevent. A
named model does **not** bypass that guard: a FAILED report for a robot in no batch is
still not recorded, whether or not it names its model.

The one place a name is accepted on its own is a **SUCCEEDED** report naming a model no
rollout is driving — a manual push, a model restored on the robot. That is the case
`targetSelector` cannot serve at all, and it is why naming the model on the wire is the
actual fix rather than a convenience: the robot is describing its own state, and
`installedModels` is where its own state goes.

## Consequences

- Two concurrent `ModelRollout`s over an overlapping target set now each complete against
  their own model.
- An adapter that predates this field loses nothing it had: unambiguous correlation is
  unchanged. What it gains is that the ambiguous case defers instead of misattributing, at
  a cost bounded by `capabilityRescanIntervalSeconds` (default 300).
- The ambiguity is now visible. `activeModelName` logs the candidate models and names the
  field to set; previously the wrong write was silent.
- **`applyFirmwarePhase` still takes the first match and is deliberately untouched.** A
  robot has one firmware, so two `FirmwareRollout`s holding the same robot is a conflict
  rather than a supported configuration, and it is a different question from this one.
  Recorded here so the asymmetry is a decision rather than an oversight.

## The contract version implication — NOT taken here

ADR-0032 defines `contract.Version` as a semver over *"the proto surface, the
`SupportedAction` schema, and the conformance-suite revision"*, and this decision changes
the proto surface. That is a **MINOR** bump (compatible; it never invalidates an existing
qualification), so `1.1.0 → 1.2.0`.

It is **not** taken in this change, deliberately. The value is pinned in
`internal/contract/version.go`, asserted by name in `internal/contract/version_test.go`,
stamped by `adapters/conformance/report.py`, and stated three times in RFC-0001 — a
published document currently inside a review window. Bumping it is a decision with owners
beyond this fix, not a side effect of it. Until it is taken, contract `1.1.0` describes a
marginally wider proto surface than it did, which is a register defect rather than a
runtime one: every adapter that negotiated `1.1.0` still negotiates, and every field added
here is optional to an adapter that ignores it.

> **Implementation note (2026-09-24).** The contract version has moved past `1.1.0` since this
> was written — to `1.2.0` under ADR-0032 Amendment 1 §A1.1, and again since — so the "until it is
> taken" state above has ended and `1.1.0` is not the current version. The implemented version is
> stated in RFC-0001 *Package and versioning*. The definition quoted at the head of this section — a
> semver over the proto surface, the `SupportedAction` schema *and the conformance-suite
> revision* — is the reading ADR-0032 Amendment 1 §A1.1 records as not governing: the
> suite revision is not a component of the contract version. The decision recorded here is
> unchanged.

It is tracked as its own decision.

## Alternatives rejected

- **Disambiguate by `resulting_version`.** Looks like a fix; leaves
  the FAILED path — the one that drives Auto rollback — still wrong, since a failure names
  no meaningful version. Also breaks down when two rollouts share a version string. The
  item's own instruction was *do not take (2) alone*, and there is nothing (2) adds on top
  of (1) and (3).
- **Forbid the precondition** (option 4) — admission-refuse a second `ModelRollout` whose
  target set overlaps a live one. Makes the correlation sound by construction and is a real
  restriction on operators who legitimately want to roll two independent models at once.
  The configuration is supported; the bookkeeping was not.

## Amendment 1 — a FAILED report is recorded for a robot whose push was acknowledged

- **Date:** 2026-09-28
- **Basis:** `docs/adr/README.md` — the decision is unchanged (a name says which model, never
  whether to record, and a decline is never recorded), so no superseding ADR. What stands for
  "the control plane entered this robot" is extended.
- **What changes.** Decision 3 used `currentBatch` as the evidence that a push was accepted. The
  batch is written by the rollout status patch at the end of the reconcile, after every push in
  it, so a failure the robot reported between its acknowledgement and that write found no batch
  and was dropped: the robot read `Updating` for ever and `pauseOnError` never fired. A FAILED
  report no batch holds is now also recorded when the robot's latest push of that model was
  acknowledged. The ModelRollout controller records the acknowledgement, on the forward and the
  rollback push, in a ledger the ingestor reads (`ModelAckLedger`,
  `internal/controller/model_ack_ledger.go`).
- **Why a decline still cannot be recorded.** A ControlStream's messages are handled in order,
  and an adapter's decline report precedes its reply, so it is ingested before the push returns;
  the controller clears the ledger entry before each push and sets it only on an
  acknowledgement.
- **What is unchanged:** a FAILED report for a robot whose latest push was declined, or never
  made, is not recorded, whether or not it names its model. The ledger is in memory: a manager
  that restarts between an acknowledgement and the batch write loses the entry, and such a
  report is dropped as before.
