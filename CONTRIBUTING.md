# Contributing to Swarmada

Thank you for your interest in Swarmada. This guide covers how to get set up, the
contribution workflow, and the two requirements every change must meet: a signed-off
commit and, for anything significant, an RFC.

## Ways to contribute

- **Discuss.** Open an issue for a bug, a question, or a proposal. Comment on RFCs in
  `rfcs/`.
- **Improve docs.** Corrections and clarifications to anything in `docs/` are always
  welcome and are a good first contribution.
- **Write code.** Pick up an issue labeled `good first issue` or `help wanted`, or
  propose a change via an RFC first if it is user-visible.
- **Write an adapter.** Implement the Fleet Adapter protocol for a robot platform. See
  the protocol in `proto/fleet_adapter/v1/` and the reference adapter in
  `fleet-adapter/`.

## Developer setup

Prerequisites:

- Go 1.22+
- `minikube` and `kubectl`
- `controller-gen` and `kubebuilder`
- Python 3.12 (in a per-project virtualenv — see **Python environment** below)
- (optional, for the simulation demos) ROS 2 Jazzy and NVIDIA Isaac Sim

### Python environment

The Python components (simulation, tooling, tests) target **Python 3.12** — the floor
declared in `pyproject.toml` (`requires-python = ">=3.12"`). Use a **per-project virtual
environment**: not the system Python, and not a shared or `conda` environment (a shared
env bleeds dependencies across repositories and is the usual cause of "passes locally,
fails in CI").

```bash
python3.12 -m venv .venv
source .venv/bin/activate
pip install -e ".[dev,simulation]"
```

`uv` is an optional, faster drop-in if you already use it:
`uv venv --python 3.12 && uv pip install -e ".[dev,simulation]"`.

CI runs on Python 3.12 — develop and test on 3.12 so local results match CI.

Common tasks (see the `Makefile` for the full set):

```bash
make generate    # regenerate DeepCopy code from the API types
make manifests   # regenerate CRD and RBAC YAML
make build       # compile the control-plane binary
make test        # run unit tests
make run         # run the control plane against your current kubecontext
```

A change to `api/v1` must be followed by `make generate && make manifests`, and the
result committed alongside the change.

## The DCO: sign off every commit

Swarmada uses the [Developer Certificate of Origin](https://developercertificate.org/).
By signing off, you certify that you wrote the change or have the right to submit it
under the project's license. Sign off by adding `-s` to your commit:

```bash
git commit -s -m "your message"
```

This appends a `Signed-off-by:` line using your `git` name and email. Commits without
a sign-off will not be merged.

## The RFC process

Any user-visible or architecturally significant change — a new CRD, a protocol
change, a new control-plane component — goes through an RFC before implementation:

1. Draft an RFC in `rfcs/`, following the structure of the existing ones.
2. Open it for comment. Minor RFCs run a two-week comment period; major or
   API-breaking RFCs run four weeks.
3. Address feedback; a maintainer approves and merges the RFC when consensus is
   reached. API-breaking RFCs additionally require a TSC vote (see
   [GOVERNANCE.md](GOVERNANCE.md)).

Small, non-user-visible changes (bug fixes, refactors, docs) do not need an RFC — open
a pull request directly.

## Pull request workflow

1. Fork and branch from `main`.
2. Make your change; keep it focused. Follow the conventions in
   [docs/api-principles.md](docs/api-principles.md) for any API change.
3. Ensure `make test` passes and `make generate manifests` produces no uncommitted
   diff.
4. Sign off your commits (`-s`).
5. Open a pull request describing the change and linking any issue or RFC.
6. A maintainer reviews. Address feedback; once approved, a maintainer merges.

## Reporting security issues

Do **not** open a public issue for a security vulnerability. Follow
[SECURITY.md](SECURITY.md).

## Code of Conduct

All participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).
