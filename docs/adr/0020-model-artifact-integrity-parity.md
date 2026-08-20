# ADR-0020: Model artifact integrity and provenance parity with firmware

- **Status:** accepted
- **Date:** 2026-07-23
- **Deciders:** Security Reviewer, API Designer, Principal Software Architect
- **Related:** RFC-0001 §9.1.7.1 and §9.2.8 (firmware signing and verification), §9.1.9 (ModelPolicy quality gate), ADR-0006 (north side is the Kubernetes API)

## Context

Two rollout kinds deliver artifacts to physical robots. `FirmwareRolloutSpec`
requires `firmwareChecksum` (a `sha256:<hex>` digest the robot re-verifies over
the downloaded bytes before install) and supports `firmwareSignatureRef`, a
detached signature verified against `SwarmadaConfig.spec.signing.trustRoots`
before any dispatch; when `requireSignatureVerification` is set, an unsigned
firmware rollout fails closed (§9.1.7.1, §9.2.8).

`ModelRolloutSpec` carries only `modelUri` and a version string. There is no
checksum, no signature reference, and no trust-root verification before the
artifact is dispatched to robots — even though a model rollout reaches the same
physical machines *and* mutates their capability set (`grantsCapabilities` /
`revokesCapabilities`), so the deployed artifact directly governs what the fleet
will physically attempt.

The promotion side has the same gap on its input. A `ModelPolicy` decides
whether to deploy by comparing reported metrics (`ReportedMetrics`,
`map[string]float64`) against a quality gate. The webhook that delivers those
metrics (`WebhookTriggerConfig.AuthSecretRef`) is optional; when the secret is
empty the endpoint "accepts unauthenticated requests (dev mode only)." So the
one input to the safety gate can arrive unauthenticated, and even a passing
decision then ships an artifact whose bytes and origin were never verified.

The forces in tension: the model path should reach the same integrity bar as
firmware given equal or greater physical consequence; a digest and a signature
are neutral primitives that add no model-specific coupling to the API;
simulation-first and local development workflows must not be blocked by
mandatory signing; and the metrics that gate deployment are themselves a trust
input, not only the artifact.

## Decision

Bring model artifacts to firmware parity, reusing the existing signing trust
root rather than introducing a second trust system.

- Add `modelChecksum` (required, `sha256:<hex>`) to `ModelRolloutSpec`. The
  robot re-verifies the downloaded bytes against it before install, exactly as
  for firmware.
- Add `modelSignatureRef` (optional field) verified against
  `SwarmadaConfig.spec.signing.trustRoots` before any dispatch. When
  `requireSignatureVerification` is set, a model rollout without a valid
  signature fails closed — the same rule and the same trust root as firmware.
- Invert the webhook authentication default. Authentication is required by
  default; the unauthenticated path becomes an explicit, named opt-in
  (`allowUnauthenticated: true`) documented as development/simulation only,
  rather than the silent behavior when no secret is set.
- Optionally bind the metrics to the artifact they describe: the evaluation
  job signs `{modelDigest, metrics}`, referenced by an optional
  `attestationRef`; the ModelPolicy verifies the signature and that the digest
  matches the `modelUri` it is about to deploy, so a passing decision provably
  describes the artifact being shipped. Enforcement is configurable and starts
  optional (see Consequences).

## Alternatives considered

- **Rely on OCI registry integrity alone.** Pinning `modelUri` by digest gives
  byte integrity, but `modelUri` may be a mutable tag, and registry integrity is
  not provenance or authorization. Firmware already rejected this reasoning for
  the same class of artifact; accepting it for models would be an unjustified
  asymmetry. Rejected.
- **Checksum only, no signature.** Detects corruption but not a well-formed
  artifact from an unauthorized source, and remains asymmetric with firmware,
  which requires signature verification when configured. Rejected as
  insufficient on its own; retained as one half of the decision.
- **Verify the artifact but leave metrics unauthenticated (or vice versa).**
  Leaves half the path open: a verified artifact promoted on forged metrics
  still deploys, and verified metrics can be paired with a swapped artifact
  absent the digest binding. Rejected — the binding is the point.
- **Keep webhook authentication optional.** The metrics channel is the quality
  gate's only input; leaving the highest-consequence input with the weakest
  default is precisely the hole this ADR closes. Rejected.

## Consequences

- The model path reaches firmware parity: a passing quality gate plus a
  verified, signed artifact is required before capability-granting code reaches
  a physical robot.
- New obligations. The ModelRollout controller MUST verify `modelChecksum` and,
  when required, `modelSignatureRef` against `trustRoots` before dispatch; the
  robot re-verifies bytes before install; the training/evaluation pipeline MUST
  emit a checksum and (when required) a signature. Implementing this is a CRD
  change (`modelChecksum`, `modelSignatureRef`, optional `attestationRef`) — the
  spec/diff must be shown and confirmed before editing the existing types, and
  `make generate manifests` plus controller and conformance tests follow.
- Reuses `SwarmadaConfig.spec.signing.trustRoots`; no second trust system is
  introduced, and the firmware verification flow is the implementation
  reference.
- Accepted drawback: more setup friction for real deployments and a new required
  field on `ModelRollout`. Mitigated by the explicit, named development opt-in so
  simulation-first work is unaffected but never the silent default, and by
  mirroring an already-understood firmware flow.
- Leaves a clean seam: metric-to-artifact attestation can ship optional and be
  tightened to required per namespace later without another schema change.

## Follow-up test obligations

- An unauthenticated webhook call is rejected unless `allowUnauthenticated` is
  explicitly set.
- A model rollout with a missing or invalid signature fails closed when
  `requireSignatureVerification` is set.
- A byte mismatch against `modelChecksum` fails the robot's pre-install check.
