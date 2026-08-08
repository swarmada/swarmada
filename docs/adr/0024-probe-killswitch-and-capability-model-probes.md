# ADR-0024: Namespace probe kill-switch, and capability/model probe execution over the command-push path

- **Status:** accepted
- **Date:** 2026-07-24
- **Deciders:** Principal Architect, Kubernetes/controller maintainers
- **Related:** RFC-0001 §9.1.6 (active probes), §9.2 (ControlStream command-push); ADR-0012 (probe debounce); AGENTS.md RA-1

## Context

Two `api/v1` surfaces are defined but marked "controller pending". Both are
implemented behind existing machinery; no CRD schema change is required.

1. **`SwarmadaConfig.spec.health.disableAllProbes`** (`*bool`, default `false`)
   is a namespace-wide kill-switch. When true the probe loop must issue **no**
   `VerifyHardware` / `VerifyCapability` / `VerifyModel` RPCs and report probes as
   Unknown/paused, while **passive telemetry is unaffected**. The probe loop is
   `RobotProbeReconciler.Reconcile` — one self-requeuing reconcile per RobotProbe
   at `intervalSeconds`. The open question is how a probe *cycle already in flight*
   when the switch flips is handled.

2. **`RobotProbe.spec.probeType = capability|model`** are defined but only
   `hardware` executes. The command-push plumbing already exists:
   `internal/command/dispatcher.go` `Dispatcher` **is** the concrete
   `probe.Prober` (wired in `main.go`), and its `buildVerifyCommand` already
   switches on all three types, emitting `VerifyHardware`/`VerifyCapability`/
   `VerifyModel`. Two gaps remain: the controller always passes
   `spec.targetComponent` as the target (ignoring `targetCapability`/
   `targetModel`), and `synthetic_input` is never carried because the
   `Prober.Verify` signature has no field for it — so `VerifyModel.synthetic_input`
   is always empty. The fail-safe binding (RPC error → Failed; `unsupported` →
   Unknown; HEALTHY-but-metrics-short → Degraded; never optimistically Healthy)
   already exists in `probeRobot` and must be preserved verbatim.

RA-1 constrains both: `RobotProbe.status` is written only on a material change
(the existing `stripProbeTime` + `DeepEqual` guard), never per probe tick.

## Decision

### 1. Evaluate the kill-switch once per cycle; short-circuit to a paused projection

`RobotProbeReconciler.Reconcile` resolves `disableAllProbes` from the namespace
`SwarmadaConfig` (via the existing fail-safe `namespaceConfig` helper) **once, at
the top of the cycle, before any RPC**. When true it takes a dedicated **paused
branch** that:

- issues **zero** `Verify*` RPCs;
- lists the matched robots (a cached read, not an adapter RPC) and builds a
  per-robot result of `ProbeStatus = Unknown` with a static message
  (`"probing disabled (SwarmadaConfig.spec.health.disableAllProbes)"`), carrying
  each robot's debounce streaks and `failedAt` **frozen** (unchanged) so probing
  resumes cleanly;
- sets `status.lastProbeResult = Unknown` directly (there is no `Paused` enum
  value; Unknown is the fail-safe projection, "paused" conveyed by the message),
  **bypassing the debounce aggregate** — a pause is not a failing cycle;
- writes only through the existing `stripProbeTime`/`DeepEqual` guard, so the
  first disabled cycle flips to paused (one write) and subsequent disabled cycles
  are byte-identical (no write) — RA-1 holds;
- requeues at `intervalSeconds` as normal.

**Mid-flight cycle handling (the explicit rule):** the switch is evaluated once
per cycle, at the cycle boundary. A reconcile that has **already passed** the
check when an operator flips the flag runs to completion — its in-flight
`Verify*` RPCs are **not cancelled**; each binds normally under the fail-safe
rules and the per-probe timeout bounds it. The switch therefore takes effect no
later than the next cycle. To make "namespace-wide" enforcement prompt rather
than up to `intervalSeconds` late, the controller **watches `SwarmadaConfig`** and
enqueues every RobotProbe in that namespace on a change, so a flip (either
direction) is applied on the next reconcile.

**Scope boundary:** `disableAllProbes` gates **only** active RobotProbe
verification. It does not touch `telemetryIntervalSeconds`,
`capabilityRescanIntervalSeconds`, connectivity/offline detection, or any passive
ingestion path — passive telemetry, heartbeats, and capability snapshots continue
unchanged.

**Fail-open on unreadable config.** An unreadable/absent `SwarmadaConfig` resolves
`disableAllProbes` to `false` (probes run), matching the field default and the
repo-wide "unreadable policy never blocks normal operation" pattern. This is safe
because a probe that cannot confirm health still reports Unknown/Failed, never
Healthy.

### 2. Execute capability/model probes by selecting the target per type and plumbing synthetic input

Reuse the hardware-probe machinery end-to-end; add only target selection and
`synthetic_input`.

- **Controller** (`probeRobot`): select the verify target by `spec.probeType` —
  `hardware → spec.targetComponent`, `capability → spec.targetCapability`,
  `model → spec.targetModel` — and pass `spec.syntheticInput` for model probes.
- **Prober contract:** replace the positional `Verify(ctx, ns, robotID,
  probeType, target, expected)` with a value object
  `probe.VerifyRequest{ProbeType, Target, Expected, SyntheticInput []byte}` and
  `Verify(ctx, ns, robotID string, req VerifyRequest)`. This carries
  `SyntheticInput` without a seventh positional parameter and keeps the call site
  readable. (Minimal alternative: add a `syntheticInput []byte` parameter — same
  behaviour, uglier signature.)
- **Dispatcher:** `Verify` + `buildVerifyCommand` accept the request and set
  `VerifyModel.synthetic_input` from it. Hardware/capability are already correct;
  the round-trip, correlation, and `bindResult` are untouched.
- **Fail-safe binding preserved verbatim:** RPC error/timeout (`ErrUnreachable`)
  → Failed; `CommandResult.unsupported` → Unknown; HEALTHY only when the adapter
  reports HEALTHY **and** `MetricsMet` — otherwise Degraded. No probe type
  optimistically resolves Healthy. `synthetic_input` is an *input*; it never
  relaxes the binding.

## Alternatives considered

- **Cancel in-flight probe RPCs when the switch flips (rejected).** It would
  require threading cancellation into the dispatcher round-trip for a sub-second
  race, for no safety benefit — a probe is a read-only verification bounded by its
  timeout, and a stray completed probe binds fail-safe. The cycle-boundary rule is
  simpler and sufficient.
- **Kill-switch enforced by not scheduling reconciles (rejected).** Suspending
  reconciliation would leave `status` stale (still showing Healthy) instead of the
  honest paused/Unknown projection, and would lose the prompt re-enable path. The
  loop must keep reconciling to *report* paused.
- **Fail-closed (disable probes) on unreadable config (rejected as default).**
  Losing health verification on a transient config read error is worse than
  continuing — and continuing is safe because probes never falsely report Healthy.
  Consistent with every other `namespaceConfig` consumer.
- **Per-type `Prober` methods `VerifyCapability`/`VerifyModel` (rejected).** Would
  fork the one send-and-correlate path the dispatcher already generalises; a single
  `Verify(req)` keeps hardware/capability/model on one code path (the "reuse the
  machinery" requirement).
- **Overloading `spec.targetComponent` for all types (rejected).** The CRD already
  provides distinct `targetCapability`/`targetModel` fields; honouring them is the
  point, and it keeps a probe's target self-describing.

## Consequences

- **New obligations.**
  - `Prober.Verify` changes shape; the `Dispatcher`, the controller, and the
    prober/dispatcher tests update together (internal only — no CRD/proto change;
    `VerifyModel.synthetic_input` already exists on the wire).
  - The RobotProbe controller gains a `SwarmadaConfig` watch + namespace mapper;
    it already has `swarmadaconfigs get;list;watch` RBAC, so no RBAC change.
  - Tests must cover: the paused branch (Unknown everywhere, zero RPCs, RA-1
    no-churn across disabled cycles, prompt resume on re-enable); capability/model
    target selection; `synthetic_input` reaching `VerifyModel`; and the fail-safe
    mapping for error/timeout/unsupported on the new types.
- **Accepted drawbacks.**
  - Up to one probe cycle of in-flight RPCs may complete after the switch flips
    (bounded by the per-probe timeout). Deliberate — see the mid-flight rule.
  - On resume from pause, a robot's status re-confirms through the normal debounce
    (effective is Unknown, a non-empty value) rather than adopting the first result
    immediately. This is the fail-safe direction — never claims Healthy without
    `recoveryThreshold` confirmations — and consistent with ADR-0012.
- **Non-goals preserved.** No CRD field added or renamed; the four "controller
  pending" doc-comments (`targetCapability`, `targetModel`, `syntheticInput`,
  `disableAllProbes`) are dropped when the controller lands (comment-only `api/v1`
  edit; regenerates CRD descriptions). Reference-implementation decision; the
  normative behaviour lives in RFC-0001 §9.1.6.

## Implementation surface (file-by-file; no code written yet)

- `internal/probe/prober.go` — introduce `VerifyRequest{ProbeType, Target,
  Expected, SyntheticInput}`; change the `Prober.Verify` signature to take it.
- `internal/command/dispatcher.go` — `Verify` + `buildVerifyCommand` take the
  request and set `VerifyModel.synthetic_input`.
- `internal/controller/robotprobe_controller.go` — (a) resolve `disableAllProbes`
  at the top of `Reconcile` and short-circuit the paused projection; (b) add a
  `probeTarget(rp)` selecting the target by `probeType` and pass `syntheticInput`;
  (c) `SetupWithManager` watches `SwarmadaConfig` → enqueue namespace RobotProbes.
- `api/v1/robotprobe_types.go`, `api/v1/swarmadaconfig_types.go` — **doc-comment
  only**: drop the "controller pending" notes. No schema change.
- Tests: `internal/controller/robotprobe_*_test.go` (paused branch + RA-1
  no-churn + resume; capability/model targets; fail-safe on error/unsupported),
  `internal/command/dispatcher_test.go` (`VerifyModel.synthetic_input`),
  `internal/probe/prober_test.go` (request shape).
- `docs/adr/README.md` — **existing file, needs confirmation**: add the ADR-0024
  index row.
