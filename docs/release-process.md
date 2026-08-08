# Release Process

How Swarmada versions, builds, and ships releases. For maintainers and release
engineers.

## Versioning

- **Semantic versioning.** Swarmada is pre-1.0; `0.x` releases may contain breaking
  changes, which are called out in the changelog.
- **API versioning is separate from release versioning.** CRDs are served at
  `swarmada.io/v1`. Additive fields ship within `v1`; a breaking API change
  introduces `swarmada.io/v2` with a conversion path, never an in-place change to
  `v1` (see [api-principles.md](api-principles.md)).
- **Protocol versioning.** The Fleet Adapter protocol follows the same rule: additive
  within `fleet_adapter.v1`, breaking changes move to `fleet_adapter.v2`, negotiated
  at connect time.
- **Release version tracks the specification version.** The normative spec is the set
  of accepted RFCs, carried as one **spec version** (`MAJOR.MINOR`, e.g. `0.2`). RFC
  *numbers* (`0001`, `0002`, …) are document IDs, not versions. swarmada's release
  version mirrors the spec version: spec `MAJOR.MINOR` → swarmada `MAJOR.MINOR`, with
  swarmada's **patch** counting implementation releases within that spec version. So
  RFC `0.1` → swarmada `0.1.x`; when the spec bumps to `0.2` (a new or amended RFC) and
  the code lands, swarmada bumps to `0.2.0`. Record every bump in `CHANGELOG.md`
  (Kubernetes-style): each released section names the spec version, which RFCs were
  **added** or **amended**, and the API/protocol changes. Bump these together — they
  must not drift: `pyproject.toml` (`version`) and `deploy/swarmada/Chart.yaml`
  (`version` + `appVersion`). The **SDK** (`sdk/python`) and the **adapter template**
  are versioned independently (ADR-0002); the wire-contract version (`fleet_adapter.v1`)
  is a separate axis (ADR-0032), bumped only on protocol changes.

## Branching and merges

- Trunk-based: `main` is always releasable.
- `main` is protected; changes land via pull request with green CI and a DCO
  sign-off. No direct pushes.

## Cutting a release

1. Confirm `main` is green and `CHANGELOG.md` is updated (grouped Added / Changed /
   Fixed / Security).
2. Tag: `git tag vX.Y.Z` (signed tags once release signing is set up).
3. Push the tag; the release workflow builds and publishes artifacts.
4. Publish a GitHub Release carrying that version's changelog section.

## Release artifacts

- A container image (from the `Dockerfile`), published to an OCI registry.
- A Helm chart for installation.
- CRD manifests (`config/crd/bases`) for direct `kubectl apply`.
- `swarmctl` binaries.

Once release signing is in place, images and artifacts are signed (for example with
`cosign`) and checksums are published. This also pre-positions the project for CNCF
Incubation, where signed releases and an OpenSSF Best Practices badge are expected.

## Compatibility policy

- CRD changes within a version are additive and backward-compatible.
- A version bump ships a conversion so existing objects keep working.
- Any breaking change is documented prominently in the release notes and changelog.

## Security releases

Handled per [SECURITY.md](../SECURITY.md): a private report, a coordinated fix,
disclosure, and a patched release with credit. Once supported release lines exist,
security fixes are backported to them.

## Pre-1.0 note

Until 1.0 there are no compatibility guarantees across `0.x` releases beyond what the
changelog states. The first stable release will define supported-version ranges and a
deprecation policy.
