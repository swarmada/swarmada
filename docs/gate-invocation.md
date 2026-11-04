# Which gates run, and when

`make help-all` lists every target. It does not say which of them anything runs on your
behalf, and the answer is not the one most readers assume.

**A target existing is not a target running.** Every checker in this repository is invoked by
one of exactly three things: a continuous-integration job, a git hook, or a person typing it.
Several of the gates this project's assurance claims rest on are in the third category only.

Run `make gate-integrity`. It prints the answer for the checkout you are in, computed rather
than asserted — including the case where the answer is *nothing*.

**The tables below describe the published repository.** A checkout that carries a different
target set or a different workflow file differs from them, so where this page and
`make help-all` disagree, `make help-all` is right and this page is stale.

## The three categories

### Run by CI, on every push to `main` and every pull request against it

These are the steps of the jobs in `.github/workflows/ci.yml`, which triggers on `push` and
`pull_request` for the `main` branch, and nightly on a schedule. If one fails, the pull request is
red and a reviewer sees it without doing anything. `.github/workflows/dco.yml` separately checks
the DCO sign-off on pull requests against `main`. `.github/workflows/install-smoke.yml` installs
each Python package (the SDK and the three reference adapters) into a clean virtual environment
from its own `pyproject.toml` and imports it, on a push to `main`, on every pull request, and
weekly. There is no other workflow.

| job | what runs | the gate behind it |
| :-- | :-- | :-- |
| `go` | `make mod-tidy`, `make build` | the module graph resolves; the control plane compiles |
| `go` | `make lint-go` | static analysis |
| `go` | `make setup-envtest`, then `make test-go` | the Go suite with the race detector, and the `envtest` suites. GitHub sets `CI=true`, and under it an `envtest` suite whose control-plane binaries are missing fails instead of skipping. `go test ./...` includes the `cmd/swarmctl` tests `make docs-cli-check` runs by name |
| `python` | `make lint-py` | ruff, the message-voice and citation rules, the rule index, the rendered adapter template, and `mypy --strict` over `simulation/` |
| `python` | `make test-py` | the Python suite, including the SDK's own and the conformance harness's own |
| `proto` | `make proto-lint` | proto3 conventions, `buf lint`, and `buf breaking` — see the caveat below |
| `helm` | `make helm-verify-sync`, `make helm-lint`, `make helm-template` | the chart has not drifted from `config/`, and renders |
| `quickstart` | `make quickstart-test` | one end-to-end run on a real `kind` cluster, on the LIVE path: the job installs Python, and a real sim adapter drives one robot ([quickstart](quickstart.md)) |
| `demo-test` | `make demo-test` | the full-surface demo on a real `kind` cluster, asserting every targeted robot, action and emergency-stop phase, the alert expressions and RA-1. It runs on a push to `main` and nightly, not on a pull request |
| `prose` | `vale .` | the documentation-voice rules in `.vale/styles/` |
| `conformance` | `make conformance-sim`, `make conformance-sim-fault` | the adapter conformance harness against the in-tree simulation adapter, once healthy and once with a hardware fault |

Not run by any job, although each exists as a target: `make conformance` (the harness against
`adapters/example-noop`) and `make uv-lock-check`. `make test` is not a CI step either; the jobs
above run each of its parts except `make uv-lock-check`.

### Run by a git hook, at commit and push time

A repository-operations script installs a `pre-commit` and a `pre-push` hook that run the
publication boundary guard before anything leaves the machine. They run on the machine that
commits, and only after somebody has installed them — a fresh clone has no hooks, so this
category is empty until a person makes it non-empty.

### Run only when a person types them

Everything else. In particular, **no job runs** `make spec-maturity`, `make spec-contract` or
`make gate-integrity`. Those are the gates that check that the specification's "read by no
controller" disclosures are still true of the Go tree, that the contract version is written the
same in every place it is written, and that every checker is invoked by something. They are
useful, and whether they pass is known only when somebody runs them.

The specification's own drift checks read the RFC chapter sources, which this repository does not
carry. They run where the specification is written, not here; the RFC's front matter says the
same.

`make docs-cli-check` is not a CI step by that name, but the four tests it selects are
ordinary Go tests in `cmd/swarmctl/`, so the `go` job's `make test-go` runs them.

## What this means for a reader

A green `make gate-integrity` is a true statement about the moment somebody typed it. It is not a
statement that it passed on the change you are looking at, and it is not a statement that it will
run on the next one. The same holds for every gate in the third category. When a document
elsewhere in this repository says a claim is "mechanically checked", read it as: *a mechanical
check exists, and running it is a step in a human workflow.*

That is a weaker claim than continuous integration, and it is the accurate one.

## Two caveats that cost more than they look

**The proto breaking-change check does not fail, and frequently does not run.**
`make proto-lint` runs `buf breaking --against main` only when a branch named `main` exists in
the checkout; otherwise it prints `no 'main' branch — skipping` and exits 0. Where it *does*
run, its result is advisory pre-1.0 by deliberate choice: the failure is caught and printed,
never propagated. So the guard on additive-only protocol changes is, today, a notification and
not a gate. Both facts are recorded in `scripts/gate-integrity-register.txt`, which is where
`make gate-integrity` counts them.

**A checkout with no version control has no automatic invocation at all.** Workflow files are
inert text without a remote to run them; a git hook needs a `.git` to be installed into. In
such a checkout every entry in all three categories above collapses into the third one.
`make gate-integrity` detects this and says so in as many words, because a green run of a gate
suite is otherwise indistinguishable from a green run of a gate suite that something is
watching.

## What no gate checks, by decision

The gates above check code, specifications and the checkers themselves. None of them checks a
sentence in the project's planning records that asserts a fact about the world: that a machine is
unreachable, that a capture shows a result, that a file no longer exists. **That is out of scope for
mechanism, deliberately.** Such a claim is true when written and can be false an hour later. The
checks that could be built are weak here. A path check passes a stale claim whose cited capture
still exists, and it flags a sentence that says a path is gone on purpose. A staleness or replication
check needs a notion of "same subject" that would be noisy enough to be bypassed in a checkout with
no CI.

What holds the line instead is a convention: **one record per physical fact.** Each such record
carries the timestamp and the capture its claim rests on, and every other document cites the record
rather than restating it, so a fact that decays is corrected in one place. A green gate suite
therefore says nothing about any claim of that kind. Read those claims against their record, and
against a fresh measurement when the claim is about hardware.

## Keeping this page honest

This page can go stale the same way any other document can. Two mechanical checks limit how
far:

- `make gate-integrity` fails when a checker exists that nothing invokes, when a checker
  cannot return a non-zero exit, when a check skips without being named and counted, when a
  graded obligation is reported away rather than graded, and when a documented `make` target runs
  nothing. It sees a skip written as a skip call: `t.Skip`, `pytest.skip`, a
  `@pytest.mark.skip` or `@pytest.mark.skipif` decorator, `unittest.skip`, `SkipTest`, and a
  script line that prints "skipping". It does not see a module-level
  `pytestmark = pytest.mark.skipif(...)`, a `pytest.importorskip(...)`, or a skip written as an
  early return, so a check skipped those ways is neither named nor counted.
- `make docs-cli-check` resolves every `swarmctl` invocation printed under `docs/` against the
  real command tree.

Neither of them checks that the CI table above matches `ci.yml`, and neither checks that a
`make` target printed in a document exists. Both are read by eye.
