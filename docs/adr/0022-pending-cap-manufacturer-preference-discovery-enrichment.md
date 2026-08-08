# ADR-0022: Per-zone pending-action cap, manufacturer-preference tiebreak, and discovery hardware enrichment

- **Status:** accepted
- **Date:** 2026-07-24
- **Deciders:** Principal Architect, Kubernetes/controller maintainers
- **Related:** RFC-0001 §9.4 (Traffic Deconfliction), §9.2.5 (discovery handshake); ADR-0009 (deconfliction gate at the commit site); AGENTS.md RA-1 (status-write discipline)

## Context

Three scheduler/registrar behaviours are specified in `api/v1` but marked
"controller pending". All three CRD/config fields already exist — this ADR
decides *how* they are enforced, not *what* they are. No CRD schema change is
required by any of the three.

1. **`maxPendingActionsPerZone`** — `SwarmadaConfig.spec.scheduling.MaxPendingActionsPerZone`
   (`int32`, `0` = unbounded) is a namespace-configured cap applied *per named
   zone* on the number of `Pending` `FleetAction`s. The field comment currently
   reads "caps the number the scheduler admits per zone before applying
   backpressure", which pre-supposes an enforcement point that has not actually
   been chosen. Two enforcement points are on the table: a validating admission
   webhook (reject at the door) vs. scheduler backpressure (let it be created,
   refuse to schedule it). We must also decide how the per-zone `Pending` count
   is computed without an O(all-actions) list on every check.

2. **`preferSameManufacturer`** — `SwarmadaConfig.spec.scheduling.PreferSameManufacturer`
   (`*bool`, default `true`) plus the per-action hint
   `FleetAction.spec.preferredManufacturer` should make eligible robots whose
   `Robot.spec.manufacturer` matches the hint sort *first*. This must be a
   tiebreak only — never a hard filter — and must be a no-op when the hint is
   absent or the namespace flag is off. The scheduler
   (`internal/scheduler.DefaultScheduler`) is a **pure decision function** today
   (no client, no writes; see ADR-0009); that property must be preserved.

3. **`DiscoveredRobot.status.reportedHardware` full population** — the Discover
   handler (`internal/registrar`) currently maps only `{Name, Type, Status}`
   into `DiscoveredHardwareComponent` and collapses any unrecognised component
   type to `Custom`, **dropping the original type string**. The CRD type already
   carries the richer attribute set (`Model`, `CustomType`, `RangeM`,
   `HorizontalFovDeg`, `DepthCapable`, `FrameRateFps`, `PlatformLengthMm`,
   `PlatformWidthMm`, and the older `MaxPayloadKg`/`ResolutionMp`). **However,
   the wire contract `fleet_adapter.v1.HardwareComponent` carries only
   `{name, type, model, status, degradation_reason}`** — none of the physical
   measurement attributes are transmitted at discovery today. "Full population"
   therefore has a hard dependency the CRD shape hides: the source data does not
   exist on the wire.

RA-1 (never write CRD status on a telemetry tick; status writes are
transition-driven only) constrains all three: (1) and (2) must not introduce a
status write, and (3) must not turn discovery into a periodic status writer.

## Decision

### 1. Enforce the pending cap at a validating admission webhook, counted via a field index

Enforce `maxPendingActionsPerZone` at **admission** — extend the existing
`FleetActionValidator` (`internal/webhook/fleetaction_webhook.go`,
`ValidateCreate`) to reject a create that would push a named zone's `Pending`
count over the cap. The rejection is `apierrors.NewForbidden(fleetTaskGR, name, …)`
whose error text leads with the machine-greppable reason
**`PendingActionLimitExceeded`**.

- **Scope of the gate:** only `CREATE` of a zone-scoped action
  (`spec.zone != ""`) whose resulting phase is `Pending`/unset. Actions with an
  empty `spec.zone` (any-zone) are **exempt** — they cannot be attributed to a
  single zone's queue, consistent with the field name `…PerZone`. The
  controller's own requeue-to-`Pending` transitions (Revoking→Pending,
  Failed→Requeue) are **not** gated — they are existing work re-entering the
  queue, not new admission, and they arrive as status-subresource writes the
  spec webhook never sees.
- **Cheap count via a controller-runtime field index (new pattern in this tree).**
  Register one field index at manager start
  (`mgr.GetFieldIndexer().IndexField(&FleetAction{}, indexPendingByZone, fn)`)
  whose index function emits `[]string{action.Spec.Zone}` **only when**
  `status.phase == Pending` **and** `spec.zone != ""`, and an empty slice
  otherwise. The webhook then counts with
  `List(ctx, &list, client.InNamespace(ns), client.MatchingFields{indexPendingByZone: zone})`
  and compares `len(list.Items)` (`+1` for the incoming object) against the cap.
  The list is served from the shared informer cache (the webhook already uses
  `mgr.GetClient()`), so the check is O(pending-in-this-zone), not
  O(all-actions-in-namespace). An action drops out of the index automatically as
  its cached phase leaves `Pending`.
- **Fail-open on config-unreadable, fail-closed on nothing.** Resolve the cap
  via the existing `namespaceConfig` fail-safe helper; an unreadable/absent
  `SwarmadaConfig` or a `0` value means *unbounded* (no rejection). Admission is
  never blocked by an unreadable policy — the cap is a guardrail, not a safety
  invariant.
- **RA-1 clean:** the webhook is `mutating=false` and performs a **read-only**
  cached `List`. It writes no status, on any resource. It is not a telemetry
  tick.

### 2. Add a soft manufacturer tiebreak as a resolved scheduler input, keeping the scheduler pure

Add a manufacturer-preference **sort stage** to `DefaultScheduler.SelectRobot`
that runs *after* hard eligibility filtering and *above* the existing
battery-descending order:

- The controller resolves an effective boolean `preferSameManufacturer` from
  `SwarmadaConfig.spec.scheduling.PreferSameManufacturer` (default `true` on
  unreadable/absent config) and passes it into the scheduler — **the same
  "caller resolves namespace defaults" pattern already used for
  `acceptDegraded`.** The scheduler gains no client and stays pure.
- The tiebreak fires only when `preferSameManufacturer == true` **and**
  `action.Spec.PreferredManufacturer != ""`. When active, eligible robots with
  `robot.Spec.Manufacturer == action.Spec.PreferredManufacturer` sort ahead of
  non-matching robots; **within each group the existing battery-descending order
  is preserved** (use `sort.SliceStable` with a two-level comparator: match-first,
  then battery-desc). When inactive, selection is byte-identical to today.
- **Never a hard filter:** a non-matching robot remains fully eligible and is
  still selected when it is the best (or only) candidate. The hint changes
  *order*, never *membership*.
- Interface change is confined to `internal/scheduler` and its callers: add one
  resolved `bool` parameter to `Scheduler.SelectRobot`. (A future `ScheduleOptions`
  struct is the seam if more hints accrue; a single bool is preferred now for
  symmetry with `acceptDegraded` and minimal churn.)

### 3. Populate discovery hardware in two sequenced tiers, gated by the wire contract

Split "full population" by *what the adapter actually transmits*:

- **Tier A — do now, no contract change** (`internal/registrar.mapDiscoveredHardware`):
  - Set `Model` from `c.GetModel()` (transmitted today, currently dropped).
  - When the reported type string is unrecognised, keep the existing
    "collapse to `Custom`, never drop a component" behaviour **but preserve the
    original string in `CustomType`** so the operator-defined subtype survives
    admission round-trip. Known enum types leave `CustomType` empty.
  - This lands inside the *existing* handshake-driven `Status().Update` in
    `Discover` — no new write, no new cadence. RA-1 clean (a discovery handshake
    is a discrete lifecycle event, explicitly not a telemetry tick — see the
    registrar package doc).
- **Tier B — sequenced behind a `fleet_adapter.v1` proto extension:** the
  physical attributes (`RangeM`, `HorizontalFovDeg`, `DepthCapable`,
  `FrameRateFps`, `PlatformLengthMm`, `PlatformWidthMm`, and the older
  `MaxPayloadKg`/`ResolutionMp`) **cannot be populated from the current wire
  contract.** Populating them requires adding the corresponding optional scalar
  fields to `proto/fleet_adapter/v1` `HardwareComponent`, regenerating, having
  the reference adapters emit them, and then a 1:1 map in `mapDiscoveredHardware`.
  Until that lands, these CRD fields correctly remain `nil` — the `+optional`
  pointer types already express "unreported". **This ADR does not itself change
  the proto**; it records that Tier B is blocked on that contract change and
  must not be faked by inventing values in the registrar.

## Alternatives considered

**Pending cap — scheduler backpressure instead of admission (rejected as primary).**
Letting the action be created and merely refusing to *schedule* it does not cap
the `Pending` count at all: the object still exists in `Pending`, so the queue
the field claims to bound grows unbounded. Backpressure delays work; it does not
shed it. Admission is the only point that actually bounds queue depth, and it
fails the client fast with a clear reason instead of accumulating zombie
`Pending` objects. (Scheduler-side awareness may still exist later as a
non-binding efficiency hint, exactly as ADR-0009 frames TDE hinting — but it is
not the enforcement point.)

**Pending cap — live `List(InNamespace)` + filter (rejected).** Correct but
O(all FleetActions in the namespace) on every create; a busy namespace pays the
full scan per admission. The field index reduces this to the matching set from
the cache for the cost of one index registration. The only real cost is that it
introduces the first field index in the tree — a small, well-understood
controller-runtime pattern — which we accept.

**Pending cap — exact/serialized counting (rejected).** Admission webhooks are
not serialized transactions; two concurrent creates can both observe a count
just under the cap (TOCTOU), so the cap may overshoot by the number of in-flight
admits. Rejected pursuing exactness: this is soft backpressure, not a safety
invariant, and the approximate cap is the honest, cheap behaviour. Documented in
Consequences.

**Manufacturer preference — hard filter, or a client-reading scheduler
(rejected).** A hard filter would strand actions when no matching-manufacturer
robot is idle, violating the "tiebreak only" requirement. Giving the scheduler a
client to read `SwarmadaConfig` itself would break the purity property ADR-0009
relies on (pluggable schedulers, no writes, no hidden reads); resolving the flag
in the caller preserves it.

**Discovery — extend the proto inside this ADR, or synthesize physical values
in the registrar (both rejected).** Bundling a `fleet_adapter.v1` contract
change into this record would couple a scheduler/webhook decision to an
adapter-contract change with its own versioning, conformance, and
reference-adapter obligations; it earns its own ADR/RFC amendment. Synthesizing
plausible values in the registrar was rejected outright — it would fabricate
telemetry the robot never reported, defeating the point of a discovery
inventory.

## Consequences

- **New obligations.**
  - A field index (`indexPendingByZone`) must be registered at manager start and
    kept consistent with the webhook's use; it is the first field index in the
    tree, so the registration site and naming become a small precedent.
  - The `FleetActionValidator` gains a `Reader`-backed count path and a resolved
    cap; tests must cover cap=0 (unbounded), under/at/over cap, zone-less
    exemption, and config-unreadable fail-open.
  - `Scheduler.SelectRobot` grows one parameter; every implementation and test
    call site updates. Tests must assert the tiebreak is order-only (a
    non-matching robot still wins when it is the sole candidate) and inert when
    the hint/flag is absent.
- **Accepted drawbacks.**
  - The pending cap is **approximate under concurrency** and **fails open** when
    config is unreadable — both deliberate for a soft guardrail. Two independent
    sources let the Pending count overshoot the cap: concurrent in-flight admits
    that cannot see each other (TOCTOU), and **informer-cache lag** — the webhook
    counts from the cache, which trails the API server, so a burst of recent
    creates not yet reflected in the cache is undercounted. The overshoot bound is
    therefore *in-flight admits + un-synced create backlog*, larger than TOCTOU
    alone; the same lag can also briefly overcount (an action already transitioned
    out of Pending but not yet re-indexed), yielding a spurious rejection when the
    zone in fact has room. All three deviations are acceptable only because the cap
    is soft backpressure, never a safety limit — a hard limit would need a quota
    object with optimistic concurrency or admission-time serialization.
  - Requeue-to-`Pending` transitions bypass the cap by design; a zone can
    momentarily exceed the cap via internal requeues even though new admissions
    are refused.
  - `spec.zone` is enforced **immutable** on update (FleetAction `ValidateUpdate`):
    the controller keys TDE reservation acquire and release on it, so an in-place
    re-target would release the slot against the wrong zone (leaking the original
    zone's reservation) and slip past the create-time cap. Re-targeting is
    cancel-and-recreate. This is admission-webhook enforcement, not a CRD schema
    constraint, so it holds only while the webhook is reachable (failurePolicy=Fail
    backs it).
  - `reportedHardware` remains **partially populated** until the Tier-B proto
    extension ships. The `nil` physical fields are honest ("unreported"), not a
    bug, but consumers must not read absence as "robot lacks the sensor".
- **Seams left for change.** Manufacturer preference is one resolved bool today;
  a `ScheduleOptions` struct absorbs further soft hints without another interface
  break. The Tier-B proto extension is the single, well-scoped unblock for full
  hardware enrichment and is the natural subject of a follow-up ADR/RFC
  amendment.
- This is a reference-implementation decision; the normative fields live in
  `api/v1` and the RFC. An alternative control plane may enforce the same field
  semantics differently.

## Implementation surface (file-by-file; no controller code written yet)

- `internal/webhook/fleetaction_webhook.go` — add cap enforcement to
  `ValidateCreate` (zone-scoped, Pending-only), the `PendingActionLimitExceeded`
  reason, cap resolution via `namespaceConfig`, and the `MatchingFields` count.
- `internal/controller/` (index registration; likely `fleetaction_controller.go`
  `SetupWithManager` or `cmd/manager/main.go`) — register `indexPendingByZone`
  on `mgr.GetFieldIndexer()`; define the index-key constant + index function.
- `internal/scheduler/scheduler.go` — add resolved `preferSameManufacturer bool`
  to the `Scheduler` interface + `DefaultScheduler.SelectRobot`; add the
  stable two-level comparator (match-first, then battery-desc).
- `internal/controller/fleetaction_controller.go` — resolve
  `PreferSameManufacturer` from config (default `true`) and pass it into the
  `SelectRobot` call (mirroring `acceptDegraded`).
- `internal/registrar/registrar.go` — Tier A: extend `mapDiscoveredHardware` to
  set `Model` and preserve the raw type string into `CustomType`.
- `api/v1/discoveredrobot_types.go` — **doc-comment only**: correct the stale
  `DiscoveredHardwareComponent` note (it claims `MaxPayloadKg`/`ResolutionMp`
  are populated today; they are not) to reflect the Tier A/Tier B split. No
  schema change.
- `proto/fleet_adapter/v1/*` — **Tier B, deferred:** add optional scalar fields
  to `HardwareComponent`; out of scope for this ADR, tracked as the follow-up.
- `docs/adr/README.md` — **existing file, needs your confirmation:** add the
  index row for ADR-0022.
- Tests to add/update: `internal/webhook/fleetaction_*_test.go`,
  `internal/scheduler/*_test.go`, `internal/registrar/registrar_test.go`.
