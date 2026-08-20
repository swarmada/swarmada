# Swarmada Governance

Swarmada is an open, vendor-neutral project. This document describes how it is
governed: the roles people hold, how decisions are made, and how the project stays
neutral as it grows. It is intentionally written for the community Swarmada intends
to become, not only the contributors it has today.

## Principles

- **Open.** Design happens in public, through the RFC process and issues. Anyone may
  participate on equal terms.
- **Meritocratic.** Influence is earned through sustained, high-quality
  contribution, and is granted to individuals, not to the organizations that employ
  them.
- **Vendor-neutral.** No single organization controls the project's direction. The
  governance structure below exists to keep it that way as commercial interest
  arrives.

## Project maturity

Swarmada is pre-1.0 (`0.x`). Until a maintainer group is established, Alex Bahel is the sole
maintainer and lands changes in bulk; contributions are staged for the maintainer to review and
commit. A `1.0` release will be considered once additional maintainers are onboarded, consistent
with the CNCF Sandbox path the project targets. Current maintainers are in `MAINTAINERS.md`.

## Roles

Swarmada uses a three-rung contributor ladder.

**Contributors** are anyone who opens an issue, comments on an RFC, or submits a
pull request. No prior status is required.

**Reviewers** are contributors trusted to review changes in an area of the codebase.
They are proposed by a maintainer on the basis of a track record of quality reviews
and merged contributions, and are listed in `MAINTAINERS.md`.

**Maintainers** hold merge rights and are responsible for the health of the project:
reviewing and approving changes, shepherding RFCs, cutting releases, upholding the
Code of Conduct, and mentoring the next rung. Maintainers act as individuals in the
project's interest, never as representatives of an employer. Current maintainers are
listed in `MAINTAINERS.md`.

Advancement is by nomination from an existing maintainer and lazy-consensus approval
of the maintainers (see below). A maintainer with no substantive contribution or
review for an extended period (guideline: twelve months) is moved to **emeritus** by
lazy consensus of the remaining maintainers. Emeritus status is a recognition of
prior service, not a penalty: emeritus maintainers keep their acknowledgement in
`MAINTAINERS.md`, step back from merge rights and votes, and may be reinstated on a
return to sustained activity. Keeping the maintainer list current — adding and
retiring maintainers as activity changes — is itself a governance responsibility.

## Technical Steering Committee

The Technical Steering Committee (TSC) is the body responsible for cross-cutting
technical direction, the roadmap, and any decision that affects the project's
neutrality or governance. It exists from day one, with seats deliberately reserved
so that the structure is in place before it is needed:

- **Founding seats** — held by the project's founding maintainer(s).
- **Partner seats (reserved)** — up to two seats for contributing organizations that
  invest sustained engineering in the project (for example, a hardware-platform
  partner and an enterprise operator). These seats are **vacant** and are
  filled by TSC invitation as such partners emerge.
- **Community-elected seats (reserved)** — two to four seats elected by contributors,
  activated once the contributor base supports a meaningful election (target: within
  twelve months of the first external maintainers). **Vacant.**

Swarmada is, at this stage, founder-led: the project was founded by **Alex Bahel**, who
holds the founding maintainer seat while the reserved seats are unfilled. Standard-
critical decisions rest with the founding maintainer, consistent with the
code-ownership rules in the repository. This is stated plainly rather than obscured.
The reserved-seat structure is the mechanism by which the project distributes
authority as partners and community contributors arrive — governance is designed for
multiple organizations from the start, not retrofitted later, and the founder's
intent is to move toward shared, multi-organization governance as the community
grows and, ultimately, to donate the project and its trademark to a neutral
foundation (CNCF).

## Decision-making

- **Lazy consensus** governs day-to-day work. A proposal (a pull request, a
  maintainer nomination) is accepted if no maintainer objects within a reasonable
  review window. Silence is assent.
- **The RFC process** governs any significant or user-visible change. RFCs are
  authored in `rfcs/`, opened for a public comment period — two weeks for minor
  changes, four weeks for major or API-breaking ones — and require maintainer
  approval to merge.
- **A TSC vote** is required for API-breaking changes, changes to this governance
  document, and anything affecting the project's neutrality. Decisions are by simple
  majority of filled seats; the TSC prefers consensus and votes only when consensus
  cannot be reached.

## Vendor neutrality

Swarmada's value is that it belongs to no one vendor. To protect that:

- Maintainer and TSC authority is held by individuals, and no single organization may
  hold a majority of maintainer or TSC positions.
- **Multi-employer maintainer threshold.** A maintainer group drawn from a single
  employer cannot demonstrate the property this section describes, whatever its
  members intend. The project therefore commits to a maintainer group of at least
  three individuals employed by no fewer than two distinct organizations, with no
  single organization holding more than half of maintainer seats, to be met before
  the project seeks CNCF Incubation. The threshold is stated here rather than in
  positioning material because it is a governance obligation, and the project's
  current position against it — including where it falls short — is stated in
  `MAINTAINERS.md`.
- Commercial products built on or around Swarmada — managed services, support
  offerings, certified-adapter programs — are developed **outside** this project and
  its organization, and confer no governance influence. Contributing to the standard
  and operating a commercial product on top of it are independent activities.
- The project targets donation to a neutral foundation (CNCF), and this governance is
  written to meet that foundation's neutrality expectations.

## Code of Conduct

All participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).

## Amending this document

Changes to this document follow the RFC process and require a TSC vote. Amendments
are expected as the project matures — in particular, activating the reserved TSC
seats and formalizing elections.
