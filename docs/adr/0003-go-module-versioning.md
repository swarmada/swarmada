# ADR-0003: Go module versioning strategy

- **Status:** accepted
- **Date:** 2026-07-06
- **Deciders:** Maintainers
- **Related:** ADR-0002, `docs/release-process.md`, `docs/api-principles.md`

## Context

Go's semantic import versioning ties a module's major version to its import
path: a module at v0 or v1 uses its base path
(`github.com/swarmada/swarmada`), while v2.0.0 and above must carry a `/vN`
suffix in the module path and in every import statement. Because a repository's
git tags map to its module, tagging `v2.0.0` on the repository declares Go-module
v2 and forces the `/v2` suffix on every consumer of its Go packages.

The consumers that matter are adapter authors, who import the generated gRPC
stubs under `proto/fleet_adapter/v1` and, in some cases, the CRD types under
`api/v1`. A module-path change is a breaking, mechanical migration for all of
them — the opposite of what an extension-driven, neutral standard wants to impose
on the people building on it.

Two facts shape the decision:

- **Contract breaks do not require a module-major bump.** A breaking protocol or
  API change is expressed as a new versioned package — `fleet_adapter.v1` →
  `fleet_adapter.v2`, `swarmada.io/v1` → `swarmada.io/v2` — which is a new import
  path *within the same module*. The existing versioning scheme
  (`docs/api-principles.md`, `docs/release-process.md`) already handles breaking
  changes this way.
- The only thing that forces a Go-module `/vN` is a breaking change to a shared
  Go library package at the *same* import path — which is avoidable by keeping
  the externally-imported surface small and stable.

The product's release version (container image, Helm chart, `swarmctl`) does not
have to track the Go-module version.

## Decision

Keep the Go module on its base path and decouple it from the product version.

- **Keep the umbrella module at a low major.** The module stays at `v0.x` now and,
  at most, `v1` later; it is not bumped to `/v2`. Breaking contract changes ride
  the versioned-package scheme (`fleet_adapter.v2`, `swarmada.io/v2`), not a
  module-major bump. In the expected case, a Go-module major bump never happens.
- **Keep the importable surface separable.** The proto stubs and `api/v1` types
  are kept cleanly extractable, so that if external-importer demand ever warrants
  it, that surface can be split into its own small, stable module
  (for example `github.com/swarmada/swarmada-api`) whose major version is
  managed independently of the control-plane module.
- **Revisit only at 1.0, and only if needed:** whether to keep the umbrella
  module at v1 permanently or split out a dedicated API module. Nothing about
  this requires action while the project is on `0.x`.

## Alternatives considered

- **Accept the `/vN` treadmill.** Bump the module path on every major release and
  have consumers rewrite imports. Rejected as a default: it repeatedly breaks
  adapter authors, the audience the standard most wants to keep.
- **Split the importable API into its own module now.** The right answer once
  external importers are real, but multi-module repository overhead (separate
  `go.mod` files, tag prefixes, coordinated releases) is not justified before
  that demand exists. Kept as the pre-positioned future option.

## Consequences

- Adapter authors and other importers see a stable Go import path across
  breaking contract changes, because those changes appear as new versioned
  packages rather than a new module path.
- The project must hold the discipline of never introducing a breaking change to
  a shared library package at an unchanged import path; where a break is
  unavoidable, it is handled by a new versioned package or, eventually, by the
  separate API module — never by bumping the umbrella module to `/v2`.
- A single, well-scoped decision (keep at v1 vs. split an API module) is deferred
  to the 1.0 milestone, with no cost incurred in the meantime.
