# ADR-0007: Conformance is self-certified; no certification authority in the standard repo

- **Status:** accepted
- **Date:** 2026-07-07
- **Deciders:** Maintainers
- **Related:** ADR-0005 (reference adapter policy), `adapters/CONFORMANCE.md`, `adapters/conformance/`, `adapters/REGISTRY.md`

## Context

The project publishes a Fleet Adapter conformance specification
(`adapters/CONFORMANCE.md`) and, now, an executable harness that drives an
adapter against it (`adapters/conformance/`). A natural next question is whether
the project should also *issue* conformance certifications — an authoritative
"Swarmada Certified" result that vouches for an adapter.

ADR-0005 already draws the governing line: the project "publishes a conformance
harness and a factual compatibility matrix … It does not operate an adapter
certification program." This ADR records the consequence of that line for the
harness and the registry, so the boundary is explicit before anyone asks for a
mark.

Two functions are easy to conflate but are different in kind:

- **Conformance testing** — anyone runs an open test suite against their adapter
  and obtains a factual pass / fail / skip result. This is a tool.
- **Certification** — an authority *issues* a result others are meant to trust,
  and typically administers a trademarked mark. This is a governance and legal
  function, and it makes whoever runs it a gatekeeper.

Bundling a certification authority into the neutral standard repository would
make the maintainers that gatekeeper, which compromises vendor-neutrality and the
CNCF-Sandbox posture the project is built for. The established precedent is CNCF
Certified Kubernetes: the test suite is open and lives with the project, vendors
self-run it and submit results as a pull request to a *separate* conformance
repository, and the "Certified Kubernetes" mark is administered by CNCF — a
separate governance body — not by the project maintainers.

## Decision

Conformance is **self-certified** against the open harness. The standard
repository hosts the test suite and a factual registry; it does not host, and the
maintainers do not operate, a certification authority or mark.

- The **harness** (`adapters/conformance/`) and the **conformance specification**
  (`adapters/CONFORMANCE.md`) are published under Apache-2.0 for anyone to run.
- An adapter's authors run the harness and **self-attest** the result by opening a
  pull request that adds or updates their row in `adapters/REGISTRY.md`, recording
  the result and the protocol version it was run against. Listing is factual, not
  an endorsement (as `REGISTRY.md` already states).
- The project **does not** issue a certificate, grant a badge or mark, or maintain
  a trust list beyond the factual compatibility matrix.
- If an authoritative certification mark is ever wanted, it belongs to a **neutral
  governance body separate from the standard repository** — a CNCF conformance
  program post-donation, or a separate commercial entity — administered under that
  body's trademark, never from this repository.

## Alternatives considered

- **Operate a certification program from this repository.** Rejected: it makes the
  maintainers a gatekeeper, contradicts ADR-0005, and undermines the neutrality
  the project depends on. It also creates trademark, liability, and review-capacity
  obligations a neutral standard should not carry.
- **No harness at all; review-only conformance.** Rejected: the point of an
  executable suite is to make conformance objective and repeatable rather than a
  matter of reviewer judgement. This is why the harness exists.
- **A third-party certifies on the project's behalf now.** Deferred, not rejected:
  premature before CNCF donation, and out of scope for the standard repository
  regardless. Revisit when a neutral body exists to own it.

## Consequences

- Anyone can measure an adapter objectively and repeatably, and the registry
  states plainly what passed which protocol version — without the project taking
  on a gatekeeping role.
- "Self-certified" must be communicated honestly: a registry row is a
  self-attested test result, not a project endorsement. `REGISTRY.md` says so and
  must keep saying so.
- The seam for a future certification mark is clean: because the harness and
  registry make no trust claim, a neutral body can later administer a mark on top
  of them without any change to the standard repository.
- The harness must be trustworthy enough to self-attest against — its own tests
  and the honesty of its `skip` reporting (never a silent pass) are load-bearing.
