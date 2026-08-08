# Container registry and image naming

Where Swarmada's container images are published, and the naming scheme across
every image-producing component (`cmd/manager`, `cmd/edge`, the reference Fleet
Adapters, the in-tree simulation adapter). Research current as of July 2026.

## Recommendation: GHCR as primary, Docker Hub as a mirror

**Primary: GitHub Container Registry (`ghcr.io/swarmada/...`).** This is
already the Makefile's default (`IMG ?= ghcr.io/swarmada/swarmada:latest`) —
this doc just makes that choice explicit and extends the same convention to
every other image. Reasons to keep it as the primary:

- **No pull-rate limiting** on public images, unlike Docker Hub's 100 (anonymous)
  / 200 (authenticated) pulls per 6 hours — a real problem for a project whose
  quickstart, CI, and conformance harness all pull images repeatedly.
- **Zero-config auth in GitHub Actions** — the built-in `GITHUB_TOKEN` pushes
  to GHCR with no separate credential to manage or leak, which matters for a
  project that's about to open its Actions workflows to public contributors.
- **Lives next to the code and the org** (`github.com/swarmada`) — one fewer
  external account/dependency for a CNCF Sandbox application to explain.

**Secondary: Docker Hub (`docker.io/swarmada/...`), mirrored on release.**
Docker Hub is still the first registry most people reach for
(`docker pull swarmada/swarmada` reads more naturally to a newcomer than the
GHCR path), so mirroring tagged releases there costs little and buys
discoverability — accept its rate limits for that audience, don't depend on
it for CI. **Action item:** confirm the `swarmada` namespace is available on
Docker Hub before committing to this name in public docs (not yet checked).

**Worth naming, not adopting yet: Quay.io.** Red Hat-operated, unlimited public
repos, built-in Clair vulnerability scanning, and native `cosign` signing
support. That last point is the interesting one: `FirmwareRollout` already
verifies OCI image signatures via Rekor
(`internal/controller/firmwarerollout_rekor_test.go`), so publishing Swarmada's
own images signed with `cosign` and logged to Rekor's transparency log would
be dogfooding the exact supply-chain-security story the project already tells
about *robot* firmware. Not urgent for the initial launch, but a natural
Stage-3/4 (neutrality proof / CNCF application) talking point — "we sign our
own control-plane images the same way FirmwareRollout expects a fleet's
firmware to be signed."

**Not recommended now: Harbor (self-hosted).** Harbor is itself a CNCF
Graduated project and the natural end-state for a security-mature org
(RBAC, replication, audit log, Trivy scanning), but it means operating
infrastructure — wrong tradeoff at the current $0-budget, pre-Sandbox stage.
Revisit if/when there's a design partner or funding covering the hosting cost.

## Image naming scheme

All images follow `ghcr.io/swarmada/<name>:<tag>`, tagged both `latest` and
the release semver (`vMAJOR.MINOR.PATCH`), consistent with the existing `IMG` convention.

| Image | Source | Purpose | Status |
|---|---|---|---|
| `ghcr.io/swarmada/swarmada` | `Dockerfile` (root) → `cmd/manager` | The controller-manager | **Already the Makefile default** — this doc formalizes it, doesn't change it |
| `ghcr.io/swarmada/swarmada-edge` | `cmd/edge` | The Zone Controller edge node (`examples/edge-node`'s binary, containerized) | Not yet built — no Dockerfile for `cmd/edge` exists today |
| `ghcr.io/swarmada/fleet-adapter-sim` | `adapters/simulation` | The in-tree simulation adapter, containerized for the quickstart/CI (rather than requiring a local Python install) | Not yet built |
| `ghcr.io/swarmada/fleet-adapter-ros2` | own repo: `fleet-adapter-ros2` | ROS 2 / Nav2 reference adapter | Not yet built — adapter itself is `partial`/conformant; no image yet |
| `ghcr.io/swarmada/fleet-adapter-vda5050` | own repo: `fleet-adapter-vda5050` | VDA5050 reference adapter | Not yet built |
| `ghcr.io/swarmada/fleet-adapter-mavlink` | own repo: `fleet-adapter-mavlink` | MAVLink/PX4 reference adapter | Not yet built |
| `ghcr.io/swarmada/swarmtop` | own repo (planned): `swarmtop` | The terminal fleet inspector — optional container image for CI/devcontainer use; primary distribution stays the GoReleaser binary + Homebrew/Scoop, not a container | Not yet built, low priority |
| `ghcr.io/swarmada/swarmctl` | `cmd/swarmctl` | The CLI, containerized for CI/bastion-host use — same caveat as `swarmtop`, binary is primary | Not yet built, low priority |

**Naming rule going forward:** one image per binary entrypoint, named after
the binary, not the repository it lives in — `fleet-adapter-ros2` (repo and
image share a name) is the pattern to keep; don't let an image accumulate a
generic name like `swarmada-tools` that hides which binary it actually runs.

## What to mirror to Docker Hub, and when

Not everything needs a Docker Hub mirror on day one. Mirror in this order,
gated on the image actually existing and being demo/quickstart-relevant:

1. `swarmada` (controller-manager) — first, since it's the one every
   quickstart user pulls.
2. `fleet-adapter-sim` — second, once built, since it's the adapter the
   quickstart's `--scenario` flow depends on.
3. The three external reference adapters and `swarmada-edge` — later, once
   each has a Dockerfile at all; no reason to mirror an image that doesn't
   exist yet.
4. `swarmtop` / `swarmctl` — lowest priority; these are binary-first tools
   per their own build systems (GoReleaser → Homebrew/Scoop), a container
   image is a convenience, not the primary distribution path.

## Sources

- [GHCR: Complete Guide to GitHub Container Registry (Gecko Security)](https://www.gecko.security/blog/ghcr-github-container-registry-guide)
- [Best Free Container Registries in 2026 — Docker Hub, GHCR, Quay & Harbor (Tools.Fun)](https://tools.fun/resources/best-free-container-registries)
- [DockerHub and Quay.io returning 429 due to rate limits (Sonatype)](https://support.sonatype.com/hc/en-us/articles/32093607704723-DockerHub-and-Quay-io-returning-429-due-to-rate-limits)
- [Possible rate limits for pulling images from ghcr.io? (GitHub Discussions)](https://github.com/orgs/community/discussions/49671)
