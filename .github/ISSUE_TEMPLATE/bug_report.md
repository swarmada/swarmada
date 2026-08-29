---
name: Bug report
about: Something in Swarmada doesn't behave the way the spec or docs say it should
title: ""
labels: bug
---

## What happened

A clear description of the incorrect behavior.

## What you expected

What RFC-0001, an ADR, or the docs say should happen instead — link the section if you
can (e.g. `rfcs/dist/RFC-0001-core-spec.md#some-anchor`).

## Steps to reproduce

1.
2.
3.

Include the exact `kubectl`/`swarmctl` commands and manifests where relevant. A minimal
repro (fewest CRDs, smallest cluster) is easier to act on than a full deployment.

## Environment

- Swarmada version / commit:
- Kubernetes version and distribution (kind, minikube, EKS, ...):
- `swarmctl version` output:

## Relevant logs or output

```
paste here
```

## Additional context

Anything else that narrows it down — a controller log excerpt, a `kubectl describe`,
whether this is new or a regression.
