# ADR-0005: Reference adapter policy

- **Status:** accepted
- **Date:** 2026-07-06
- **Deciders:** Maintainers
- **Related:** ADR-0002, `adapters/README.md`, `adapters/CONFORMANCE.md`

## Context

Fleet Adapters are the project's primary extension point (`adapters/README.md`).
A policy is needed for what the core project builds and maintains itself, versus
what lives in separate repositories, so that adapter coverage can grow without
the core taking on unbounded maintenance or compromising neutrality.

The tempting shortcut — ship many vendor-specific "starter" adapters in the core
tree for vendors to extend later — is a known failure mode in comparable
projects, which shipped in-tree vendor integrations and then spent years moving
them out-of-tree (the Kubernetes in-tree cloud providers and volume plugins are
the canonical example). In-tree vendor code creates a standing maintenance tax,
implies endorsement of some vendors over others, leaves ambiguous ownership when
a vendor "takes over" a core-authored stub, and is often impossible to build well
against proprietary or NDA-gated vendor APIs.

At the same time, the project needs at least a few real adapters to demonstrate
the standard, to serve as templates, and to keep the protocol and conformance
suite honest.

## Decision

Draw the line by **neutrality and ownership**, not by convenience.

- **The core repository's `adapters/` holds only:** the conformance suite and (as
  it becomes executable) its harness; a reference skeleton/template
  (`example-noop`); the adapter registry and compatibility matrix; and the
  authoring guide. A **simulation adapter** may also live in the core tree as
  test and demonstration infrastructure, alongside `simulation/`.
- **Reference adapters that target open, vendor-neutral interfaces** — for example
  ROS 2 / Nav2, and VDA5050 (which covers a whole class of compliant AMRs rather
  than one vendor) — are maintained as reference **modules in their own
  repositories** under the org, keeping the donated core focused. They are
  tracked through the registry.
- **Vendor-specific adapters live in the vendor's own repository** and are never
  pre-built in the core tree. The project provides a template so a vendor
  scaffolds and owns its adapter from the first commit.
- **All adapters are feature-basic but safety-complete.** A minimal adapter may
  decline optional commands with `unsupported = true` (`CONFORMANCE.md` C7.1),
  but the safety-critical MUSTs — confirmed (never inferred) emergency stop (C5),
  fencing-token ordering (C3), and assignment-lease self-stop (C4) — are complete
  even in the most basic adapter.
- **The project stays neutral:** it publishes a conformance harness and a factual
  compatibility matrix (which adapter passes which protocol version). It does not
  operate an adapter certification program.

A simulator is treated as one option behind a common pattern (for example Isaac
Sim, Gazebo, or a neutral physics engine), so the simulation path is not locked
to a single vendor.

## Alternatives considered

- **Ship N vendor-specific starter adapters in-tree for vendors to extend.**
  Rejected: maintenance tax, vendor-favoritism optics, ambiguous ownership on
  hand-off, and frequently blocked by proprietary APIs — the in-tree-to-
  out-of-tree lesson from comparable projects.
- **Keep every adapter, including reference ones, in the core repository.**
  Rejected: it bloats and dilutes the donated core; a focused, neutral core is
  what a standards body expects.

## Consequences

- The core stays focused on the contract, the conformance harness, the template,
  and the registry — not on a fleet of vendor integrations.
- Coverage grows two ways: reference adapters for open interfaces (one adapter per
  *class*), and vendor-owned adapters in their own repositories — never
  core-maintained stubs.
- Conformance run in the core CI (for in-tree pieces) and in vendors' CI (via the
  published harness) is the single gate that gives "compatible" a definite
  meaning.
- This requires the conformance suite in `adapters/CONFORMANCE.md` to become an
  executable, versioned harness, tracked as an implementation item.
