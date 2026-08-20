# Issue and Pull Request Labels

The labels used on issues and pull requests. They are **namespaced**
(`key/value`, Kubernetes-style) so that each label states which axis it belongs
to. A well-labeled issue carries one value from each axis that applies to it.

New values are added to this document first; a label is never coined inline on an
issue. The label set is kept in sync with a machine-readable source so it does
not drift (see the automation phase).

## Axes

### `kind/` — what type of work it is

| Label | Use for |
| :---- | :------ |
| `kind/bug` | A defect: behavior diverges from the spec or documentation. |
| `kind/enhancement` | A new capability or improvement. |
| `kind/docs` | Documentation only. |
| `kind/question` | A support question (usually redirected to Discussions). |
| `kind/rfc` | Tracking a proposal that goes through the RFC process. |
| `kind/adapter` | A Fleet Adapter: coverage request, or an issue in a reference adapter. |
| `kind/security` | A security-relevant issue. Report privately first (see [`SECURITY.md`](../SECURITY.md)). |
| `kind/test` | Test coverage or test infrastructure. |
| `kind/refactor` | Internal change with no user-visible behavior change. |

### `area/` — which part of the system

`api` · `proto` · `controller` · `webhook` · `scheduler` · `telemetry` · `tde` ·
`security` · `safety` · `spec` · `tooling` · `sdk` · `adapters` · `examples` ·
`docs`

This axis is **extensible**: when a new subsystem appears, add its value here
rather than inventing a label on the issue.

### `severity/` — impact

| Label | Meaning |
| :---- | :------ |
| `severity/blocker` | Must be resolved before the next release; breaks a core path or a safety guarantee. |
| `severity/major` | Significant impact, but with a workaround or limited scope. |
| `severity/minor` | Low impact; cosmetic or edge-case. |

This is the only severity scale.

### `triage/` — workflow state

| Label | Meaning |
| :---- | :------ |
| `triage/needs-triage` | Newly opened; not yet reviewed by a maintainer. Applied automatically. |
| `triage/accepted` | Validated and accepted as something the project will act on. |
| `triage/needs-info` | Waiting on more information from the reporter. |

### Lifecycle (terminal)

`duplicate` · `wontfix` · `invalid` · `stale`

### Community (contributor funnel)

| Label | Meaning |
| :---- | :------ |
| `good first issue` | Small, well-scoped, and a good entry point for a first contribution. |
| `help wanted` | The maintainers would welcome someone picking this up. |

## How labels are applied

1. A new issue is opened through a template and lands with `triage/needs-triage`
   plus the `kind/` its template implies.
2. During triage a maintainer validates it, sets `area/` and `severity/`, and
   either marks it `triage/accepted`, requests more detail with
   `triage/needs-info`, or closes it with a terminal label.
3. Scheduling is expressed with a milestone (a release), not a label.

## What labels do not carry

Specification maturity is **not** a label. A CRD's or subsystem's stability and
implementation status are recorded in the specification itself as `stage:`
(`alpha` / `beta` / `stable`) and `impl:` (`specified` / `partial` /
`implemented`) badges, not on GitHub issues.

## Adding or changing a label

Propose the change in a pull request that edits this document. Because the label
set is synchronized from a machine-readable source, the label is created or
renamed by that synchronization once the change merges — not by hand in the
repository settings.
