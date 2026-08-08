# ADR-0002: Repository topology

- **Status:** accepted
- **Date:** 2026-07-06
- **Deciders:** Maintainers
- **Related:** ADR-0001, RFC-0001

## Context

Several components depend on one evolving contract: a Go control plane (`api/`,
`internal/`, `cmd/`, `config/`), a gRPC adapter contract (`proto/`), a reference
fleet adapter (`fleet-adapter/`, `adapters/`), a Python SDK (`sdk/`), and Python
simulation and ML tooling. A decision is needed on how many repositories these
occupy and how future growth is accommodated without reorganizing the tree
later.

Splitting components across repositories before the shared CRD and protocol
surface is stable would multiply cross-repository pull requests, version skew,
and CI cost with no offsetting benefit at the current contributor volume.

## Decision

Keep the project in a single **monorepo**, and pre-declare the seams along which
components may later be extracted so that growth does not require reorganizing
the tree.

- **Idiomatic in-repo layout is retained.** The kubebuilder layout
  (`api/ internal/ cmd/ config/ hack/`) and the buf-managed `proto/` tree stay as
  they are.
- **Three future extraction seams are pre-declared:**
  1. the Python SDK (`sdk/python/`) extracts to its own repository when it needs
     a release cadence independent of the control plane;
  2. a vendor-owned adapter extracts to its own repository when a third party,
     not the core project, owns it (for license, CLA, and code-ownership
     scoping);
  3. governance and community material may move to a dedicated community
     repository at standards-body onboarding time.

Extracting along any of these seams later is a history-preserving move
(`git filter-repo`), not a source reorganization, because the boundaries are
reserved now.

## Alternatives considered

- **Polyrepo from the start** (separate repositories for control plane, SDK,
  adapters). Appropriate once components diverge in release cadence and
  ownership, but premature now: it imposes cross-repository change coordination
  and multiplied CI before there is contributor volume to justify it.
- **A single repository with no reserved seams.** Simplest today, but a later
  split would then require reorganizing import paths and history rather than a
  clean extraction.

## Consequences

- One clone, one CI configuration, and atomic cross-component changes while the
  shared contract is still moving.
- Extraction to a separate repository along a pre-declared seam is available when
  a component's cadence or ownership diverges, without a tree reorganization.
- New top-level directories are added deliberately, keeping the layout legible as
  the project grows.
