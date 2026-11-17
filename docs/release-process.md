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
  components that implement `RFC-0001 v0.4` are `0.4.x`. A component release is tagged
  `vX.Y.Z`; the spec keeps its own tag, `rfc-NNNN-vX.Y`. Bump these together before the
  tag — they must not drift: `pyproject.toml` (`version`) and `deploy/swarmada/Chart.yaml`
  (`version` + `appVersion`, the control-plane image). The release workflow refuses a tag
  that differs from the chart's `appVersion`.
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
2. For a spec release, tag it `rfc-NNNN-vX.Y`, for example `git tag rfc-0001-v0.4`, and push
   the tag.
3. For a component release, tag `vX.Y.Z`, for example `git tag v0.4.1`, and push the tag. A
   maintainer pushes it. `.github/workflows/release.yml` runs on it: it builds the manager
   image for `linux/amd64` and `linux/arm64`, pushes it as `X.Y.Z` and `latest`, attaches the
   SBOM and provenance, signs it keylessly with cosign, and verifies the signature. On a pull
   request that changes the `Dockerfile` or the workflow, it runs the same build with no push.
   The other workflows are `ci.yml`, `dco.yml` and `install-smoke.yml`.
   - The image is pushed by digest, signed and verified, and only then tagged `X.Y.Z`. A
     version already published is refused, so `X.Y.Z` never changes. `latest` moves only when
     the tag is the highest `vX.Y.Z`.
   - The publish job runs in the `release` environment. In the repository settings, that
     environment requires a maintainer's approval and accepts only `v*` tags, and a tag
     ruleset lets only maintainers create, move or delete `v*` tags.
   - After the first release, a maintainer makes the `swarmada` package public in its GHCR
     package settings, once; until then, anonymous pulls fail.
4. Publish a GitHub Release from that tag, carrying its changelog section.

## Release artifacts

What a release is meant to carry, and what in the tree produces each today:

- The controller-manager image (from the `Dockerfile`), published to
  `ghcr.io/swarmada/swarmada` by the release workflow; `make docker-build IMG=<ref>` builds
  it locally, and `make docker-buildx` checks the two-platform build.
- A Helm chart for installation: the chart source is `deploy/swarmada/`; no target
  packages or publishes it (`helm package` by hand).
- CRD manifests (`config/crd/bases`) for direct `kubectl apply`.
- `swarmctl` binaries: `make swarmctl` builds one for the host platform into
  `bin/swarmctl`; no target cross-compiles or publishes it.

The manager image is signed with cosign, with SBOM and provenance attestations; see
[container-registry.md](container-registry.md#verifying-a-release-image). The other
artifacts are not signed yet, and no checksums are published. Signed releases and an
OpenSSF Best Practices badge are expected for CNCF Incubation.

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
