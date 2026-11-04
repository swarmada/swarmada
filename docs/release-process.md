# Release Process

How Swarmada versions, builds, and ships releases. For maintainers and release
engineers.

## Versioning

- **Semantic versioning.** Swarmada is pre-1.0. The contract is frozen at RFC-0001 v1.0
  (below). Until then, a `0.x` release changes the contract only through the
  contract-change process; a change it makes outside the contract is called out in the
  changelog.
- **API versioning is separate from release versioning.** CRDs are served at
  `swarmada.io/v1` and follow Kubernetes API rules: within `v1` only backward-compatible
  changes are made, and an incompatible schema change introduces `v2` (see
  [api-principles.md](api-principles.md#versioning-and-compatibility)).
- **Protocol versioning.** The Fleet Adapter protocol has its own contract version: at
  RFC-0001 v0.4 it is `1.4.0` within `fleet_adapter.v1`, with supported range
  `>=1.3.0 <1.5.0`. A change off the wire is a patch bump by default; a change that binds
  adapters is a minor bump, and the range moves. A change that binds adapters for safety
  also raises the lowest supported version to the new one. The freeze covers the contract version
  published with RFC-0001 v1.0.
- **The specification is released by RFC version.** The normative spec is the set of
  accepted RFCs. A spec release is named for its RFC and that RFC's version,
  `RFC-NNNN vX.Y`, and tagged `rfc-NNNN-vX.Y`. RFC *numbers* (`0001`, `0002`, …) are
  document IDs, not versions. The first spec release is `RFC-0001 v0.4`, tagged
  `rfc-0001-v0.4`.
- **Each release publishes one complete specification.** An amendment is a drafting tool.
  A release folds its accepted amendments into the chapters and assembles one specification
  for that version, published with a "what changed since the previous version" document.
  The two v0.4 amendments are normative for v0.4 but published as separate files; the next
  revision folds them into the chapters.
- **Components are versioned `X.Y.Z` against the RFC they implement.** The Helm chart, the
  control-plane image and the Python package carry `X.Y.Z`: `X.Y` is the RFC version they
  implement, and `Z` is the component's own counter of releases against it. The
  components that implement `RFC-0001 v0.4` are `0.4.0`. A component version is a value
  in its own file, not a tag: there is no `vX.Y.Z` tag. Bump these together — they must
  not drift: `pyproject.toml` (`version`) and `deploy/swarmada/Chart.yaml` (`version` +
  `appVersion`, the control-plane image).
- **Adapters keep their own versions.** Each adapter declares the contract version it
  implements (`fleet_adapter.v1`, a separate axis, ADR-0032, bumped only on protocol
  changes). The **SDK** (`sdk/python`) and the **adapter template** are versioned
  independently (ADR-0002).
- **Record every release in `CHANGELOG.md`** (Kubernetes-style). Each released section is
  named for the spec release, for example `[RFC-0001 v0.4]`, and names which RFCs were
  **added** or **amended**, and the API/protocol changes.

## Branching and merges

- Trunk-based: `main` is always releasable.
- `main` is protected; changes land via pull request with green CI and a DCO
  sign-off. No direct pushes.

## Cutting a release

1. Confirm `main` is green and `CHANGELOG.md` is updated (grouped Added / Changed /
   Fixed / Security).
2. Tag the spec release: `git tag rfc-NNNN-vX.Y`, for example `git tag rfc-0001-v0.4`
   (signed tags once release signing is set up), and push the tag. The components'
   `X.Y.Z` versions are the values in `pyproject.toml` and `Chart.yaml`; no tag names them.
3. Build and publish the artifacts by hand. No workflow runs on a tag:
   `.github/workflows/` holds only `ci.yml` (push to and pull requests against `main`)
   and `dco.yml` (pull requests against `main`), so pushing a tag builds and publishes
   nothing. There is no release workflow.
4. Publish a GitHub Release from that tag, carrying its changelog section
   (`[RFC-NNNN vX.Y]`).

## Release artifacts

What a release is meant to carry, and what in the tree produces each today:

- A container image (from the `Dockerfile`), published to an OCI registry:
  `make docker-build IMG=<ref>` and `make docker-push IMG=<ref>`.
- A Helm chart for installation: the chart source is `deploy/swarmada/`; no target
  packages or publishes it (`helm package` by hand).
- CRD manifests (`config/crd/bases`) for direct `kubectl apply`.
- `swarmctl` binaries: `make swarmctl` builds one for the host platform into
  `bin/swarmctl`; no target cross-compiles or publishes it.

Once release signing is in place, images and artifacts are signed (for example with
`cosign`) and checksums are published. This also pre-positions the project for CNCF
Incubation, where signed releases and an OpenSSF Best Practices badge are expected.

## Compatibility policy

- Until RFC-0001 v1.0, a release changes the proto, a CRD, or the normative text of
  RFC-0001 only as [api-principles.md](api-principles.md#versioning-and-compatibility)
  describes.
- A change outside the contract that an operator or a client can notice (an Event
  reason, a metric, a CLI flag, a printer column, a message string) is documented in
  the release notes and changelog, with the migration it needs.

## Security releases

Handled per [SECURITY.md](../SECURITY.md): a private report, a coordinated fix,
disclosure, and a patched release with credit. Once supported release lines exist,
security fixes are backported to them.

## Pre-1.0 note

Until RFC-0001 v1.0, compatibility across `0.x` releases is what a resource's `stage:`,
the contract-change process and the changelog state
([api-principles.md](api-principles.md#versioning-and-compatibility)).
