# Compute Endpoint Conformance

This document defines what a **Compute Endpoint** — the executor side of the
Draft 4 external-action surface — must, should, and may do to be a conforming
Swarmada executor. It is derived from RFC-0001's `ComputeEndpoint` chapter — its
bindings (the `Http` binding's JSON envelope), normative requirements and result
rules. Its rules for events from a source that is not a fleet adapter (`CE6`) are
stated here in full. Where this document and RFC-0001 differ in
the interpretation of a shared term, **RFC-0001 governs**. The key words MUST,
MUST NOT, SHOULD, and MAY are used as in RFC 2119.

Conformance is what makes an endpoint part of the standard — not whether it
lives in this repository, and not who wrote it. An inference server, an MCP
server, a partner API, a team's own FastAPI container: all of them are
executors, and the checks below are the whole of what "conforming executor"
means.

> **The party under test is not an adapter.** A fleet adapter dials the control
> plane; a Compute Endpoint is dialled *by* it (`compute.proto`'s topology note).
> The two surfaces share the `TaskEvent` message and nothing else, and a report
> against this catalog is a `ComputeEndpoint` report — never a `FleetAdapter`
> one. See [`PROFILES.md`](PROFILES.md) for the surface split.

> **Two of the skip causes mean "never an obligation here".** A check belonging
> to a binding or a contract the endpoint did not declare records
> `binding-not-declared` or `contract-not-declared` and is counted
> `notApplicable`, not `skip` — several checks in this catalog come in mutually
> exclusive pairs (`CE6.2`/`CE6.3` most clearly), and no endpoint can satisfy
> both halves. See [`PROFILES.md`](PROFILES.md#support-levels).

> **Status:** `conformance/computeendpoint.py` drives this catalog against a
> target named on the command line, with in-tree fixtures for all three
> bindings. The `service-contract` profile is specified and **not driven**: no
> resident fixture exists in the tree, so `CE7.*` records `skip` with an
> enumerated cause rather than a pass. What is verified and what is not is the
> report's own output, not this sentence.

## The capability tier of an endpoint is fixed

An endpoint's tier is **Read Only**, always, and a report declaring anything
else is malformed. This is not a modesty convention: an executor's result is
**data and never a command**, so there is no
authority over the fleet for a tier to grade. `CE2.5` is the check that holds
the line, and it is Core.

Stating it as a fixed tier rather than omitting the field is deliberate — a
missing field cannot be distinguished from an incomplete report.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CE0 — Report integrity (self-checks on this suite)

- **CE0.1 (MUST)** A skip reason must be consistent with what the run observed.
  Skips record an enumerated cause rather than free text, and a cause the run
  itself contradicts fails. A report's account of what it did **not** verify is
  part of its output, and an unchecked justification drifts exactly like any
  other unverified claim.
- **CE0.2 (MUST)** The report names exactly one binding and one contract, and
  they are the ones the endpoint under test declares. A report that ran the HTTP
  driver and recorded `binding: Grpc` is asserting the harness, not the endpoint.
- **CE0.3 (MUST)** The run stands alone: every artefact it read resolves inside
  the public repository, and no profile, check id, check title or fixture path
  names a specification this suite cannot read. A stranger cloning
  `github.com/swarmada/swarmada` runs this suite with nothing else present. A
  dependency on a private repository is a defect, not a convenience.
- **CE0.5 (MUST)** Every check this registry names is defined in this document,
  and every check defined in this document appears in the registry. Drift
  between the prose and the partition is a failing check, not a review comment.

### CE0.x — harness context

`CE0.4` and above record what the harness established about **the run** before
asserting anything about the endpoint. **They do not count toward conformance**;
a failing context row invalidates the checks that depend on it, whose results
must then be read as unestablished rather than as passes.

- **CE0.4 (context)** The resolved target and transport the harness dialled.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CE1 — Invocation and idempotency

The rule this family exists for: **Swarmada retry plus transport retry otherwise
double-writes against an external system**.

- **CE1.1 (MUST)** Accept the invocation key in the documented shape
  `<namespace>/<action>/<runGeneration>/<attempt>` and treat the whole string as
  the idempotency key. An endpoint MUST NOT require the key to be parseable, and
  MUST NOT derive behaviour from its parts.
- **CE1.2 (MUST)** Return the **same reply** for a repeated invocation key,
  byte-for-byte in `status`, `values`, `artifacts` and `events`, rather than
  performing the work twice.
- **CE1.3 (MUST)** Perform the work's side effects at most once per invocation
  key. A repeated key that returns a cached body while writing a second row to a
  sink satisfies `CE1.2` and violates this, which is why the two are separate
  checks: the observable the harness uses is an identity carried in the reply
  (`rowId` in the reference fixture) that changes when work is redone.
- **CE1.4 (MUST)** Honour `deadlineMs` and return rather than hang. The control
  plane's own timeout is a backstop, not the contract.
- **CE1.5 (MUST)** Not require the caller to hold a call open for the run's
  duration. For `Grpc`, `Activate` returns promptly and progress is observed via
  `StreamStatus`/`Result` on a later reconcile; for `Http` and `Mcp`, the reply
  is the run. An endpoint whose latency is unbounded and whose caller must wait
  has made its own outage into a control-plane outage.
- **CE1.6 (MUST NOT)** Deduplicate on a **prefix** of the invocation key. Two
  attempts of the same run generation are different invocations and differ only
  in the last segment; an endpoint that keys on `<namespace>/<action>` collapses
  every attempt of every run into one and silently returns a stale result.
- **CE1.7 (SHOULD)** Mark a replayed reply — the reference envelope uses
  `Swarmada-Idempotent-Replay: true` — so a caller can see that deduplication
  happened rather than inferring it from equality.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CE2 — The reply envelope

One shape across all three bindings. `Grpc` carries it as `ComputeResult`,
`Http` as the JSON body of `POST /v1/invoke`, `Mcp` as the content of a
`tools/call` result.

- **CE2.1 (MUST)** Set `status` to exactly `Succeeded` or `Failed`. No third
  value, and no vocabulary of its own.
- **CE2.2 (MUST)** Report **"worked, found nothing" as `Succeeded`** with the
  value absent — never as `Failed`. A consumer distinguishes the two by testing
  whether the value is present, not by testing `status`, and an endpoint that
  reports an empty search as a failure makes that impossible.
- **CE2.3 (MUST)** Keep `values` to scalars only, at most 16 entries and at most
  4 KiB in total. Bulk data is an artifact reference.
- **CE2.4 (MUST)** Carry artifacts as **references with a digest** — `uri`,
  `mediaType`, `digest` — never as inline bytes.
- **CE2.5 (MUST NOT)** Return anything interpretable as an action, a target, a
  pose, a `desiredState`, or any other instruction. There is no response field
  for a command and one must never be added. With a model-backed executor in the
  loop, anything written in the robot's field of view is an input to that model;
  this is the check that keeps prompt injection a data problem rather than a
  fleet-safety one.
- **CE2.6 (MUST)** Return only result fields the endpoint's contract declares —
  `ComputeEndpoint.spec.results` for `Http`, the service definition for `Grpc`,
  the tool's own output for `Mcp`. An undeclared field is dropped by the control
  plane and MUST NOT be relied on.
- **CE2.7 (MUST)** Carry events in the reply as `{type, payload}` with **no
  sequence number**. `CE6.3` governs who assigns `seq`, and it is never the
  executor for a request/response contract.
- **CE2.8 (SHOULD)** Where a result is model-derived, carry the model's identity
  and version so a later reader can tell which model produced it.
- **CE2.9 (MUST)** Not return a success status with a malformed body. A non-2xx
  response, an unparseable body, or a timeout is a `Failed` outcome with the
  transport reason attached by the caller; an endpoint that answers `200 OK` with
  a body the envelope does not describe has produced an undetectable failure.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CE3 — The `Http` binding (App contract only)

- **CE3.1 (MUST)** Serve `POST /v1/invoke` accepting `Content-Type:
  application/json` and returning `200 OK` with the envelope body.
- **CE3.2 (MUST)** Treat the **body's** `invocation.id` as authoritative. The
  same value is sent as a `Swarmada-Invocation-Id` header so a proxy can see it;
  an endpoint MUST NOT key on the header alone.
- **CE3.3 (MUST)** Return the envelope of `CE2` for both outcomes — the found
  case and the "worked, found nothing" case — in the same shape.
- **CE3.4 (MUST NOT)** Claim the `Service` contract. `Http` is request/response
  and serves `App` only; a `Service` contract on this binding is refused at
  admission and MUST be refused by the endpoint's own declaration too.
- **CE3.5 (MUST, when a probe is declared)** Answer the declared probe path
  (`spec.probe.path`, conventionally `/healthz`) so readiness is observable.
  Readiness is not otherwise observable for this binding.
- **CE3.6 (MUST NOT)** Require any request field the envelope does not define. An
  endpoint needing a field the envelope lacks is a spec change, not a local
  convention.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CE4 — The `Grpc` binding (App and Service contracts)

The topology is the **inverse** of `fleet_adapter.v1`: the control plane is the
client and the endpoint is the server (`compute.proto`). No reverse dial-out is
needed or attempted.

- **CE4.1 (MUST)** Implement `compute.v1.ComputeRuntimeService` as the **server**
  and accept the control plane as the client. An endpoint MUST NOT attempt a
  reverse dial, and MUST NOT require one to function.
- **CE4.2 (MUST)** Return promptly from `Activate`, and idempotently re-ack a
  repeated `Invocation.id` rather than starting a second run. `run_generation` is
  the run's fencing value; an `Activate` carrying a lower one for the same
  invocation MUST NOT supersede the run in flight.
- **CE4.3 (MUST)** Server-stream `fleet_adapter.v1.TaskEvent` from `since_seq` on
  `StreamStatus`, and honour a resume: a reconnect with `since_seq: n` re-sends
  from `n`, and re-sending an already-delivered `(invocation, seq)` is correct
  and safe.
- **CE4.4 (MUST)** Return from `Result` the write-once terminal `ComputeResult`
  for an `App` run, and the running **summary** for a `Service` run — counts,
  duration, last observation — never the observations themselves. This is not a
  per-tick channel (the RA-1 discipline, applied on this side).
- **CE4.5 (MUST)** Return a valid `CancelDisposition` on `Cancel`, so the control
  plane can tell a safe stop from a completion or a recovery.
- **CE4.6 (SHOULD, and MUST for a `Service` contract)** Implement `RenewLease`
  and self-stop when renewal ceases. `CE7.2` states the obligation for a resident
  run; an `App`-contract endpoint that never outlives its invocation may decline
  this RPC.
- **CE4.7 (MUST NOT)** Carry a sensor-stream frame on this service. Frames flow
  adapter → runtime directly, on the `EdgeService` pattern; they never traverse
  this control channel and never traverse the control plane.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CE5 — The `Mcp` binding (App contract only)

MCP supplies the one thing Kubernetes did not: a parameter and result contract
that is **discoverable at runtime from the server itself**, so the declaration
cannot drift from the implementation because it *is* the implementation's
declaration.

- **CE5.1 (MUST)** Answer `tools/list` with the tool's **input schema as JSON
  Schema**. This is the cheap pre-filter, exactly as `SupportedAction` is on the
  adapter surface.
- **CE5.2 (MUST)** Execute `tools/call` and return content that maps onto the
  `CE2` envelope — `status`, `values`, `artifacts`, `events`.
- **CE5.3 (MUST)** Validate arguments against the schema `tools/list` declared,
  and refuse a call that violates it. The call itself is the authoritative check,
  exactly as `ValidateAction` is on the adapter surface.
- **CE5.4 (MUST NOT)** Claim the `Service` contract. MCP is request/response and
  has no concept of a binary frame stream; admission rejects `binding: Mcp` with
  `contract: Service`, and reading "MCP servers" as "and therefore the video
  pipeline too" is the specific error this check exists to catch.
- **CE5.5 (MUST)** Deduplicate on the invocation key carried in the call
  arguments, as `CE1` requires of every binding.
- **CE5.6 (SHOULD)** Make a change to the declared schema observable, so a
  consumer holding a contract digest can detect drift rather than discovering it
  in a failed call.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CE6 — Events from a source that is not a fleet adapter

These rules bind an executor that raises semantic events and is not a fleet
adapter. They are stated here in full and rest on nothing outside this catalog and
RFC-0001's `ComputeEndpoint` chapter. RFC-0003, a draft published for review and not
part of the draft 4 contract, numbers the same rules `E10.1`–`E10.6`; the numbers are
given for its readers, and no row here depends on that draft. Beyond the rows, these
rules bind such an endpoint, and no row grades them. It MUST set an event's time from
its own clock at observation, not at transmission. It MUST NOT raise an event in place
of a lifecycle transition. It MAY send an already-delivered `(subject, seq)` again after
a stream break, and the control plane MUST treat the repeat as safe. It MUST mark an
event `terminal` only on a `Continuous` execution, and at most once per execution.

> **`CE6.1`–`CE6.5` are registered `excluded` and graded nowhere.** RFC-0001 states
> none of these rules at v0.4, nor the rules above this list: its `ComputeEndpoint`
> chapter calls `spec.emitsEvents` declarative only. Under RFC-0001's precedence rule
> (conformance profiles, *Precedence*) a row here grades no RFC-0001 obligation the
> chapter does not state, so since suite revision `2026.10.13`
> [`profiles.yaml`](profiles.yaml) lists `CE6.1`–`CE6.5` under the surface's
> `excluded:`, in no profile. The suite still runs them and records each row with its
> grade, and no profile result and no verdict counts them until RFC-0001 states them.
> The `events` profile holds `CE6.6` alone, as Extended, and is never required. The
> control plane's half of the same rules, `CP2.3`–`CP2.5`, is excluded with them
> ([`CONTROL-PLANE.md`](CONTROL-PLANE.md)).

- **CE6.1 (MUST, = E10.1)** Declare on `ComputeEndpoint.spec.emitsEvents` every
  event `type` this endpoint can raise. An undeclared type is rejected by the
  control plane, which acts on nothing from it (`CP2.5`). RFC-0001 does not state
  the rejection at v0.4, so this row is registered `excluded` and graded nowhere until
  the chapter states it (see the note above this list).
- **CE6.2 (MUST, = E10.2)** For a **resident** (`Service`) executor: assign `seq`
  itself, strictly monotonic per `action_id` across every run of the action, never
  reset on reconnect or by a later run, never reused. The contract keeps one `seq`
  space per action: the control plane records each `seq` at most once per action,
  across every run and invocation of it (the `StreamStatus` comment in
  `proto/compute/v1/compute.proto`), and an event whose `seq` is not above the
  action's `status.eventSeqHigh` is discarded.
- **CE6.3 (MUST, = E10.3)** For a **request/response** (`App`) executor: emit
  events in the reply with **no** sequence number. The control plane assigns
  `seq` in reply order, continuing after the highest `seq` the invoking
  `FleetAction` has accepted, and attributes the events to that `FleetAction`:
  one `seq` space per action, across every invocation (`CE6.2`).
- **CE6.4 (MUST, = E10.4)** Return the same reply for a repeated invocation key,
  so a retried invocation's events are recorded **once** (`CP2.4`). Deduplication rides
  on the invocation, not on the event.
- **CE6.5 (MUST NOT, = E10.5)** Address a consumer of its events, or raise an
  event against an execution other than the one it was invoked for. Consumers
  subscribe by naming the producer; the direction of naming is always
  consumer → producer. An executor that named its consumers would be
  unshareable, would choose what fires, and could raise an event against an
  execution it is not part of.
- **CE6.6 (SHOULD, = E10.6)** Document whether events arrive as observed or in
  the reply. A `loop.until` written against a resident source and pointed at a
  request/response one **changes meaning without changing shape**,
  and this is the declaration that lets a task author see it.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CE7 — The `Service` contract (resident runs)

Applicable only when the endpoint declares `contract: Service`, which requires
`binding: Grpc` and `streamPolicy.pinEndpoint: true`.

- **CE7.1 (MUST)** Stay resident, and end only on an explicit `Cancel`, a lease
  expiry, or a self-reported `terminal: true` event. A resident run that exits on
  its own without one of the three has ended an execution the control plane still
  believes is live.
- **CE7.2 (MUST)** Self-stop when renewal ceases. The safe direction is **off**:
  a control-plane partition stops a resident run and cannot leave it running.
  This is the inverse of the estop lease direction and deliberately so.
- **CE7.3 (MUST)** Serve the run from the **one** address the control plane
  pinned, for the run's lifetime. An endpoint MUST NOT hand a pinned run to
  another replica: splitting a stream across replicas silently degrades tracking,
  motion estimates and pre-roll rather than failing.
- **CE7.4 (MUST)** Mark a re-placement after endpoint loss with a semantic event,
  so a consumer **sees** the temporal discontinuity rather than inferring it from
  a gap.
- **CE7.5 (MUST)** Report a summary from `Result`, never per-observation data.
- **CE7.6 (MUST NOT)** Treat an absence of frames as an absence of the thing
  being looked for. A run whose stream dries up fails on its timeout; it does not
  silently report "nothing detected". This is the `PositionFrame` rule copied
  verbatim to a second place where silence would otherwise read as signal.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## CE8 — Provenance

- **CE8.1 (MUST)** Report the identity of the bytes serving the run — the image
  digest, or the equivalent identity the deployment can resolve to bytes. Every
  run must be attributable to specific bytes; this is the endpoint's half of that
  obligation, and `CP4` is the control plane's.
- **CE8.2 (SHOULD)** Echo model identity and version onto the result where the
  result is model-derived, so a handoff between a robot's detector and an
  endpoint's can be compared rather than assumed comparable.
- **CE8.3 (MUST NOT)** Change what it computes without changing what it reports
  as its identity. An endpoint that swaps its model behind a stable identity
  makes every recorded result unattributable retroactively — including results
  already used to make a decision.

<!-- ─────────────────────────────────────────────────────────────────────── -->
## Passing

An endpoint conforms when it satisfies every **Core** check of every profile
that applies to it — the profiles for its declared binding and contract — and
declares the Extended features it implements. Declining an Extended feature is
conformant; declining it silently is not.

## Self-certification

Conformance to this catalog is **self-certified**, on the same terms as every
other Swarmada profile (ADR-0007): the harness produces the result, the
implementer publishes it, and there is no certification authority, no gate, and
no fee. Running the suite requires no permission and no registration.

A published claim MUST carry the `ConformanceReport` that supports it, and MUST
state the binding, the contract, and the contract version the result was earned
against. "Conforming executor" without those is not a claim this specification
recognises, because the profiles that applied — and therefore the meaning of the
word — differ between them.
