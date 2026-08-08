# swarmtop — a terminal fleet inspector for Swarmada

**Status: demo-only, in-tree; distribution deferred (see
[ADR-0018](../../docs/adr/0018-swarmtop-repository-placement.md)).** For now
swarmtop exists only to make the demos legible — built with `make swarmtop` and
launched by `make demo`. Repository extraction and packaged distribution
(GoReleaser → Homebrew/Scoop/krew) are **not** set up yet and are deferred until
its v1 scope is proven end-to-end and someone needs to run it against a real,
non-demo cluster. The build/release details below describe that eventual plan,
not the current state.

It will become its own repository (`github.com/swarmada/swarmtop`) at that point
— staged here for now the same way the reference Fleet Adapters are staged
before graduating to their own repo (see
[ADR-0005](../../docs/adr/0005-reference-adapter-policy.md)). It's a separate
Go module (its own `go.mod`) specifically so that split is mechanical later.
One name for both the repo and the binary — no separate "-tui" suffix to
track. Day-to-day usage lives in
[`docs/swarmtop-setup.md`](../../docs/swarmtop-setup.md).

A terminal tool rather than a web app, built on Go + Bubble Tea, released via
GoReleaser (Homebrew/Scoop, cross-compiled for macOS, Linux, and Windows).

`swarmtop` is terminal-only and read-only. Don't call it a "dashboard" — it is
a terminal viewer, not a web UI.

## What it does

A read-only, live-updating terminal fleet inspector over the Swarmada CRDs
(`Robot`, `FleetTask`, `FleetAdapter`) plus the `Event`s they emit — the
fleet-specific answer to "I want something friendlier than `kubectl get` or a
generic K8s UI to watch battery, capability health, and adapter connectivity."
It does not `apply`/`admit`/`delete` anything — that stays `swarmctl`'s job. It
also doubles as a demo tool: fleet-aware columns (battery, capability health,
adapter connectivity, a warning-event badge) that generic `k9s` has no concept
of.

Live updates are driven by controller-runtime informers (push, not poll):
`swarmtop` watches `Robot`, `FleetTask`, `FleetAdapter`, and `Event`, keeps an
in-memory snapshot, and re-renders on every change.

### Views and keys

| View | How to reach it | Shows |
| :--- | :--- | :--- |
| Robot list | default | name, phase, battery, zone (`*` = drift), capability summary, adapter telemetry age, `!N` warning-event badge, task |
| Split | `s` | the robot list beside a live detail pane for the highlighted robot |
| Robot detail | `enter` | full capabilities, hardware, position (coarse; RA-1), current task, health & connectivity, firmware & models, conditions, and recent events |
| FleetTask list | `t` | phase, assigned robot, priority, progress, retries |
| Task split / detail | `s` / `enter` (in the task list) | the task list beside — or full-screen — a live detail pane: phase, priority, progress, retries, deadline countdown, and the assigned robot's live phase + battery |
| Adapter health | `a` | phase, conformance, negotiated protocol, connected robots, last heartbeat |
| Adapter split / detail | `s` / `enter` (in the adapter list) | the adapter list beside — or full-screen — a live detail pane: phase, conformance, protocol, handshake freshness, and every served robot's live phase + battery |

Navigation: `↑`/`↓` (or `k`/`j`) move the list cursor in every view (robots,
tasks, and adapters); in a detail view `↑`/`↓`/`PgUp`/`PgDn`/`g`/`G` scroll, and
in split mode `PgUp`/`PgDn`/`Home`/`End` scroll the detail pane while the arrows
keep moving the cursor. `enter` opens the highlighted row's full-screen detail
and `s` toggles the split pane — the same in the robot, task, and adapter lists.
`esc` backs out of a view; `q` quits. All state is coarse status the controllers
already publish (battery/position/connectivity are throttled per RA-1), shown
with an explicit staleness age — not live telemetry.

## Building and running

```bash
cd tools/swarmtop
make build                              # -> bin/swarmtop, local OS/arch
make run KUBECONFIG=~/.kube/config      # build + run against a cluster

# Scope the view to one namespace (empty = all namespaces):
./bin/swarmtop -n warehouse-a
./bin/swarmtop --kubeconfig ~/.kube/config --namespace warehouse-a

# Open straight into one robot's detail view:
./bin/swarmtop -n warehouse-a --robot sim-robot-001
```

`--robot <name>` boots directly into that robot's full-screen detail view (it
applies as soon as the robot first appears in a snapshot); press `esc` to drop
back to the list.

From the repo root, `make swarmtop` builds the binary and prints the
`swarmtop -n warehouse-a` command the warehouse quickstart suggests as its
live fleet view.

## Cross-platform releases

The build/release workflow has landed (`.goreleaser.yaml` +
`.github/workflows/release.yml`, see the plan doc): a `v*` tag push builds
Linux/macOS/Windows binaries (amd64 + arm64, Windows/arm64 excluded for now)
via GoReleaser and publishes to a Homebrew tap and a Scoop bucket, so most
users never need Go installed at all. A separate PR-triggered workflow
(`.github/workflows/test.yml`) builds, tests, and lints across all three
target OSes on every change under `tools/swarmtop/`.
