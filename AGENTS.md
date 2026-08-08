# AGENTS.md

Guidance for AI coding agents and human contributors working in this repository.
This is the canonical rules file; tool-specific files (`CLAUDE.md`, and any
`COPILOT.md`) point here rather than restating it. It complements — and does not
replace — [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Ground yourself before changing code

- Read [`docs/coding-standards.md`](docs/coding-standards.md) before writing
  code, and [`docs/api-principles.md`](docs/api-principles.md) before changing
  any CRD (`api/v1`) or protocol (`proto/`).
- The normative specification is [`rfcs/dist/RFC-0001-core-spec.md`](rfcs/dist/RFC-0001-core-spec.md).
  Where architecture docs and RFC-0001 differ, RFC-0001 governs.
- Decisions already made are recorded in [`docs/adr/`](docs/adr/). Read the
  relevant ADR before re-opening a settled question; record a new decision as an
  ADR rather than changing course silently.

## Build, test, and generated code

The `Makefile` is the source of truth for every task (`make help` lists them).

- After changing `api/v1`: run `make generate manifests` and commit the
  regenerated deepcopy and CRD/RBAC YAML alongside the change. Generated code is
  never hand-edited.
- After changing `proto/`: run `make proto` and `make proto-lint`; commit the
  regenerated stubs.
- After any code change: `make test` and `go build ./...` must pass.
- Before opening a pull request: `make lint` must pass.

## Invariants that must not be broken

- **Status-write discipline (RA-1).** Never write `Robot` (or other resource)
  status on a telemetry tick. Telemetry is projected; per-tick status writes are
  prohibited.
- **Adapter conformance.** Any change touching the `fleet_adapter.v1` contract
  or an adapter must keep adapters conforming to
  [`adapters/CONFORMANCE.md`](adapters/CONFORMANCE.md). Safety behaviors —
  confirmed (never inferred) emergency stop, fencing-token ordering, and
  assignment-lease self-stop — are not negotiable.
- **Explicit presence on safety-relevant scalars.** Do not send or interpret a
  defaulted `0`/`false` where the true reading is unknown (see the proto's
  `RobotPosition` / `BatteryStatus`).

## Commits and pull requests

- **0.x, single maintainer.** Swarmada is pre-1.0 (`0.x`) with a single maintainer. Automated
  and AI-assisted runs do NOT `git commit` or `git push` — leave changes staged for the maintainer,
  who lands them in bulk. The DCO sign-off below applies when the maintainer commits.
- Sign off every commit under the Developer Certificate of Origin:
  `git commit -s`.
- Keep pull requests focused; include the reasoning and link the relevant RFC,
  ADR, or issue.

## Rules for AI-generated changes

A change authored with AI assistance is held to the same bar as any other, plus:

- It must compile, pass `make test` and `make lint`, and include tests for new
  behavior — especially for the safety invariants above.
- It must not introduce a dependency without a stated reason.
- It must not invent API fields, RFC section numbers, or file paths; cite the
  actual source in the repository.
- The human who opens the pull request is the author of record and is
  responsible for its correctness.
