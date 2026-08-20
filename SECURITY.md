# Security Policy

## Reporting a vulnerability

Please report security vulnerabilities **privately**. Do not open a public issue,
pull request, or discussion for a suspected vulnerability.

Email **`<security@swarmada.io>`** with:

- a description of the issue and its impact,
- the version, commit, or deployment where you observed it, and
- steps to reproduce, if you have them.

You will receive an acknowledgement within **five business days**. The maintainers will work with
you on a fix and a coordinated disclosure timeline, and will credit you in the
release notes unless you prefer to remain anonymous.

## Scope

Swarmada is **task-level fleet orchestration software**. Security reports about the
control plane, the API, the Fleet Adapter protocol, authentication, or the reference
implementation are in scope.

The following are **out of scope** for this policy, because they are the
responsibility of the robot platform and its operator, not the orchestration layer:

- physical safety behaviour of a robot (motion, stopping distance, collision
  avoidance);
- the correctness or safety of a specific vendor's Fleet Adapter or robot firmware;
- physical safety hardware (light curtains, safety PLCs, functional-safety controls).

Swarmada's software emergency-stop protocol is not a substitute for physical safety
systems. See [docs/architecture.md](docs/architecture.md) → The safety boundary.

## Supported versions

Swarmada is pre-release. Until a stable release, security fixes are applied to `main`.
Supported-version ranges will be published here at the first tagged release.
