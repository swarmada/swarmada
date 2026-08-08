# Architecture Decision Records

An Architecture Decision Record (ADR) captures a single architectural decision,
its context, and its consequences at the time it was made. ADRs are immutable
once accepted: a decision that is later reversed is not edited but superseded by
a new ADR that references it. Editorial corrections (typos, formatting, broken
links, or wording that does not change the decision) may still be made in place;
only a change to the decision itself requires a superseding ADR.

## ADR vs. RFC

Two different instruments, deliberately kept separate:

- An **RFC** (`rfcs/`) proposes a *change* to the standard and is reviewed
  before acceptance. It is a forward-looking proposal.
- An **ADR** (this directory) records a *decision already made* and the reasons
  for it. It is a backward-looking record that answers the question every new
  maintainer asks: "why is it built this way?"

A large RFC may generate several ADRs. A small implementation decision that
needs no RFC may still warrant an ADR.

## Lifecycle

An ADR has one status at a time:

- `proposed` — under discussion, not yet binding.
- `accepted` — the decision is in force.
- `superseded by ADR-NNNN` — replaced; the body is left intact for the record.
- `deprecated` — no longer relevant, but not replaced.

## Authoring a new ADR

1. Copy `0000-adr-template.md` to `NNNN-short-title.md`, where `NNNN` is the
   next unused four-digit number.
2. Fill in every section. Keep it to the decision at hand; link related ADRs
   and RFCs rather than restating them.
3. Open a pull request. The ADR is reviewed like any other change; on merge with
   status `accepted`, the decision is in force.
4. Add a row to the index below.

## Index

| ADR | Title | Status |
| :-- | :---- | :----- |
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | accepted |
| [0002](0002-repository-topology.md) | Repository topology | accepted |
| [0003](0003-go-module-versioning.md) | Go module versioning strategy | accepted |
| [0004](0004-rfc-amendment-and-errata-policy.md) | RFC amendment and errata policy | accepted |
| [0005](0005-reference-adapter-policy.md) | Reference adapter policy | accepted |
| [0006](0006-north-side-is-the-kubernetes-api.md) | The task-source surface is the Kubernetes API | accepted |
| [0007](0007-conformance-self-certification.md) | Conformance is self-certified; no certification authority in the standard repo | accepted |
| [0008](0008-spec-document-structure.md) | Specification document structure — Proposal overview + Detailed Specification appendix | accepted |
| [0009](0009-deconfliction-gate-at-commit-site.md) | Traffic-deconfliction reservation gate at the commit site | accepted |
| [0010](0010-edge-zone-controller-site-connectivity.md) | The edge Zone Controller is the site connectivity boundary | accepted |
| [0011](0011-connectivity-critical-escalation.md) | Surface prolonged-offline "Critical" connectivity as a Robot condition | accepted |
| [0012](0012-probe-status-debounce.md) | Debounce RobotProbe status with pointer-typed thresholds | accepted |
| [0013](0013-graceful-drain-timeout.md) | Bound Graceful zone-maintenance wind-down with a drain timeout | accepted |
| [0014](0014-auto-admit-discovered-robots.md) | Auto-admit DiscoveredRobots via a class+zone match in the DiscoveredRobot controller | accepted |
| [0015](0015-tde-recovery-and-reservation-expiry-config.md) | Source TDE reservation-expiry per namespace and recovery tunables from the manager namespace | accepted |
| [0016](0016-estop-delivery-config.md) | Resolve estop delivery tuning per namespace; implement partial-delivery response | accepted |
| [0017](0017-coordinate-system-annotation.md) | Surface the namespace coordinate system as stamped annotations, not transformation | accepted |
| [0018](0018-swarmtop-repository-placement.md) | swarmtop stays in-tree and demo-only for now; repository extraction and distribution deferred | accepted |
| [0019](0019-adapter-action-discovery-and-validation.md) | Adapter action discovery and validation | accepted |
| [0020](0020-model-artifact-integrity-parity.md) | Model artifact integrity and provenance parity with firmware | accepted |
| [0021](0021-quality-gate-fail-closed-semantics.md) | Quality-gate evaluation is fail-closed on missing and simulation-only metrics | accepted |
| [0022](0022-pending-cap-manufacturer-preference-discovery-enrichment.md) | Per-zone pending-action cap, manufacturer-preference tiebreak, and discovery hardware enrichment | accepted |
| [0023](0023-status-projections-suspension-zone-conditions-resume-gate-counts.md) | Wire four controller-pending status projections (model suspension stamp, FleetZone conditions, maintenance resume-estop gate, maintenance counts) | accepted |
| [0024](0024-probe-killswitch-and-capability-model-probes.md) | Namespace probe kill-switch, and capability/model probe execution over the command-push path | accepted |
| [0025](0025-fleet-adapter-controlstream-mtls.md) | Terminate mTLS on the Fleet Adapter ControlStream (fail-closed by default; three manager flags; cert-manager overlay) | accepted |
| [0026](0026-robot-liveness-projection.md) | Project per-robot adapter liveness onto Robot connectivity so a robot served by a live Fleet Adapter reaches Ready | accepted |
| [0027](0027-suggested-robot-class-on-discover.md) | Populate DiscoveredRobot.status.suggestedRobotClass on discover so zero-touch auto-admit can fire | accepted |
| [0028](0028-robot-id-annotation-telemetry-join-key.md) | `swarmada.io/robot-id` is the canonical telemetry↔Robot join key: stamped at admission, defaulted when absent | accepted |
| [0029](0029-robot-phase-state-machine.md) | Robot.status.phase is a derived state machine (Discovered→Idle/InProgress→Offline), not a sticky Discovered label | accepted |
| [0030](0030-auto-remove-offline-auto-admitted-robots.md) | Opt-in auto-removal of Offline auto-admitted Robots once adapter presence is gone and any lease is provably dead | accepted |
| [0031](0031-hardware-status-disabled.md) | HardwareStatus=Disabled models an intentionally-off component (benign, non-critical), distinct from Failed | accepted |
| [0032](0032-fleet-adapter-contract-versioning-and-conformance-gating.md) | Fleet-adapter contract versioning, compatibility skew, and version-bound conformance gating | accepted |
| [0033](0033-install-outcome-reporting.md) | Firmware/model install outcome is an adapter-reported fact: streamed for latency, snapshotted for recovery, projected onto Robot.status; failure sealed only when confirmed | accepted |
| [0034](0034-preferred-robot-soft-scheduling-preference.md) | `spec.preferredRobot`: a soft "this robot if it can" scheduling preference | accepted |
| [0035](0035-async-operation-outcome-protocol.md) | A general async-operation outcome mechanism for the Fleet Adapter protocol (generalizes ADR-0033) | proposed |
| [0036](0036-reachability-aware-conformance-gate.md) | The Required-Events/conformance gate must assert reachability, not just reference | proposed |
