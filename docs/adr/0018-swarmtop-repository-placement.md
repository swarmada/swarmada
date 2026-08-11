# ADR-0018: swarmtop repository placement

- **Status:** accepted
- **Date:** 2026-07-11
- **Deciders:** Maintainers
- **Related:** ADR-0002 (repository topology), ADR-0005 (reference adapter policy), ADR-0006 (north side is the Kubernetes API), `tools/swarmtop/`

## Context

`swarmtop` is a read-only, live-updating terminal inspector for Swarmada fleet
state (`Robot`, `FleetAction`, `FleetTask`, `FleetZone`, `FleetAdapter`,
`RobotProbe`). It is a **client** of
the control plane, not part of the normative standard: per ADR-0006 the north
side is the Kubernetes API, so `swarmtop` reads the same CRDs any client would.

Today it is staged in-tree at `tools/swarmtop/` as **its own Go module**
(`github.com/swarmada/swarmtop`), consuming the parent repo's types through a
local `replace github.com/swarmada/swarmada => ../../`. Its only consumer right
now is the demo flow: `make swarmtop` builds `tools/swarmtop/bin/swarmtop`, and
`make demo` launches it (`DEMO_LAUNCH_SWARMTOP=1`) to show fleet fields updating
live during a scenario.

A decision is needed on where `swarmtop` lives and how it is distributed. Two
forces are in tension: **neutral-standard framing** (the core repo should read as
the specification plus its reference implementation, not a grab-bag of tools)
versus **cost and focus right now** (packaged distribution — GoReleaser,
Homebrew/Scoop, krew, a second repo with its own CI/releases — is real ongoing
overhead that buys nothing while swarmtop's only job is to make demos legible).

## Decision

**Keep `swarmtop` in-tree at `tools/swarmtop/` as demo-only tooling for now, and
defer both repository extraction and packaged distribution** until its v1 scope
is proven end-to-end and there is external demand to install it standalone.

- **Scope now:** swarmtop exists to be built and run by the demo flow
  (`make swarmtop`, then run against the demo's kind cluster). No Helm chart, no
  Homebrew/Scoop tap, no krew plugin, no `go install`-as-a-product, no separate
  repo — none of that is set up until it's needed.
- **Dependency direction stays one-way.** swarmtop requires the core
  `api/v1` types; the core never depends on swarmtop. The module boundary and the
  `replace => ../../` are retained precisely so a later split is mechanical.
- **The extraction seam is reserved, not taken.** When the eventual move to
  `github.com/swarmada/swarmtop` happens, it is a history-preserving
  `git filter-repo` of `tools/swarmtop/` and the `replace` becomes a tagged
  `require` — no import rewriting. This is the ADR-0002 growth seam, exercised
  later.
- **Publication:** `tools/swarmtop/` is **not** part of the core repo's public
  `PUBLICATION.yaml` surface and does not gate core releases. For an initial demo
  release it may be published in-tree as demo tooling (labeled demo-only), or
  left private and built locally — decided at publish time, not here.

## Alternatives considered

- **Extract to a separate repo and set up packaged distribution now**
  (GoReleaser → Homebrew/Scoop/krew). This is the likely eventual end state, but
  rejected *for now*: it adds a second repo, release workflow, tap, and bucket to
  maintain before anyone needs to install swarmtop outside a demo. Premature
  overhead — the same reasoning ADR-0002 uses to defer polyrepo generally.
- **Publish swarmtop in the core tree as a first-class product.** Rejected: it
  bloats what a CNCF reviewer reads as "the standard" and couples a UI's churn to
  the donated specification — the in-tree-tooling failure mode ADR-0005 rejects
  for adapters.
- **Fold swarmtop into `swarmctl`.** Rejected: a Bubble Tea full-screen TUI and a
  scriptable `kubectl`-style CLI have different interaction models and release
  rhythms; merging serves neither.

## Consequences

- **Good:** minimal overhead — one module, one `make` target, zero distribution
  machinery to maintain; the core repo stays a clean neutral-standard surface;
  the split seam is preserved for when demand is real.
- **Cost:** anyone who wants swarmtop today builds it from the tree
  (`make swarmtop`); there is no `brew install`. Acceptable while its only role is
  the demo.
- **Deferred obligations (revisit at extraction):** a `swarmada/swarmtop` repo,
  a `.github/workflows/release.yml` driving GoReleaser (referenced by the staged
  `.goreleaser.yaml` but intentionally not wired up yet), a Homebrew tap, a Scoop
  bucket, and optionally a krew manifest. None are created now.
- **Trigger to revisit:** swarmtop's v1 feature scope works end-to-end **and**
  someone needs to run it against a real (non-demo) cluster without a Go
  toolchain. At that point, execute the split and stand up distribution.
