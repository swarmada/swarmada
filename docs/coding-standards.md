# Coding Standards

Conventions for contributing code to Swarmada. This complements
[api-principles.md](api-principles.md) (API design) and
[CONTRIBUTING.md](../CONTRIBUTING.md) (workflow and the DCO). It is written for
contributors and for automated assistants working in the repository.

## Languages and tooling

- **Go 1.22+** for the control plane, CRD types, scheduler, and CLI. Formatted with
  `gofmt`; linted with `golangci-lint` (config in `.golangci.yml`). Run
  `make fmt vet` before pushing.
- **Python 3.12+** for simulation, ML scoring, and tooling (the floor numpy and scikit-learn impose). Formatted with `black`,
  linted with `ruff` (config in `pyproject.toml`).
- **C++ / ROS 2** for the production Fleet Adapter.
- **Protocol Buffers** for the Fleet Adapter contract.

Any change to `api/v1` is followed by `make generate && make manifests`, with the
regenerated files committed in the same change.

## Go and controller-runtime

- **Reconcilers are idempotent.** A reconcile computes desired state from the object
  and the cluster; running it twice yields the same result. Never assume it runs
  exactly once.
- **Write status only on a material change.** Compare against a `DeepCopy` of the
  original and patch only when something actually changed, so an unchanged object
  does not generate an etcd write on every periodic reconcile. This is the RA-1
  discipline — see [architecture.md](architecture.md).
- **Keep telemetry off etcd.** Never add a reconcile path that writes status on every
  telemetry tick. High-cadence values belong in the telemetry backend.
- **Patch, don't overwrite.** Use `client.MergeFrom` against the pre-modification
  copy for status patches.
- **Wrap errors with context** (`fmt.Errorf("fetching robot: %w", err)`) and return
  them so the manager requeues.
- **Requeue deliberately.** Use `ctrl.Result{RequeueAfter: ...}` for time-based
  checks; return the error for transient failures.
- **Log through controller-runtime.** `log.FromContext(ctx)` with structured
  key/values — never `fmt.Println`.
- **Deterministic output.** Sort any collection that feeds status (for example the
  derived capability list) so an unchanged set compares equal across reconciles and
  does not trigger a spurious write.

## API types

Follow [api-principles.md](api-principles.md): pointers for optional scalars,
kubebuilder validation markers rather than controller-side checks, enums as named
string types, `conditions` + `observedGeneration` in status, and never `interface{}`
or `map[string]interface{}` in an API type.

## Package layout

- `api/v1` — CRD types (one kind per file).
- `internal/controller` — reconcilers (one per kind).
- `internal/scheduler` — robot selection (behind a pluggable `Scheduler` interface).
- `internal/controller` — capability derivation (see `robot_controller.go`).
- `internal/zone` — zone and edge logic.
- `cmd` — the manager entry point.
- `proto` — the Fleet Adapter protocol.

## Protocol Buffers

- Field numbers are permanent. Never renumber or reuse a retired field number.
- Additive changes only within a package version; a breaking change moves to a new
  package (`fleet_adapter.v2`).
- Regenerate stubs with `make proto` and commit the generated code.

## Python

- Type hints on public functions; `ruff` and `black` clean.
- Keep `simulation/`, `ml/`, and `scripts/` importable and testable — no top-level
  side effects on import.

## Commits and pull requests

- Sign off every commit (`git commit -s`) — the DCO is enforced.
- One logical change per pull request; keep diffs reviewable.
- `make test` and `make generate manifests` must leave no uncommitted diff.

## Security

- No secrets, tokens, or private keys in code, fixtures, or samples.
- Fleet Adapter connections use mutual TLS and short-lived bearer tokens; do not
  weaken that in examples or defaults.
