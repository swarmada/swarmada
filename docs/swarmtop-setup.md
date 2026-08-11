# swarmtop — demo setup

`swarmtop` is a read-only terminal inspector for a running Swarmada control
plane — a live view of `Robot`, `FleetAction`, `FleetTask`, `FleetZone`, `FleetAdapter`, and `RobotProbe`
state. **For now it exists only to make the demos legible.** It is staged
in-tree at `tools/swarmtop/` and built with a single `make` target; there is no
Helm chart, no Homebrew/Scoop/krew package, and no separate repo yet — those are
deferred (see [ADR-0018](adr/0018-swarmtop-repository-placement.md)).

If you just want to watch the demo, the simplest path is `make demo`, which
builds and launches swarmtop for you. Everything below is the two-terminal
manual version and the details.

---

## Prerequisites
- **Go ≥ 1.26** (swarmtop's module toolchain) — only needed to build it.
- **Docker** — the demo brings up a local `kind` cluster.
- Nothing else. swarmtop reads through the kubeconfig the demo generates.

## Fastest path — one command
```bash
make demo                       # default scenario: full-surface
make demo SCENARIO=hardware-fault
```
This stands up the kind cluster, runs the scenario with the live simulation, and
launches swarmtop watching every field flow (`DEMO_LAUNCH_SWARMTOP=1`). Quit
swarmtop with `q` / `Ctrl-C`.

## Two-terminal path (build once, watch alongside a quickstart)
```bash
# Terminal 1 — build swarmtop, then bring up the fleet
make swarmtop                   # -> tools/swarmtop/bin/swarmtop
make quickstart                 # kind + control plane + simulated fleet (needs Docker)
#   or: make quickstart SCENARIO=hardware-fault

# Terminal 2 — point swarmtop at the demo namespace
tools/swarmtop/bin/swarmtop -n warehouse-a
```
swarmtop uses your current kubeconfig context (the one `make quickstart` leaves
active), so no flags are required beyond the namespace.

## Flags
- `-n`, `--namespace` — namespace to watch (the demos use `warehouse-a`); empty = all.
- `--kubeconfig` — path to a kubeconfig (default: `$KUBECONFIG`, else `~/.kube/config`).
- `--robot <name>` — open straight to a robot's detail view.

## Using it
Master-detail TUI (Bubble Tea):
- `s` — toggle split pane · `enter` — full-screen detail of the selection ·
  arrows / `j`,`k` — move · `q` / `Ctrl-C` — quit.
- Robot detail shows capabilities, hardware, and events; task and adapter detail
  show lifecycle/health fields plus the live phase and battery of the robots they
  touch. Updates stream live via a watch cache — no manual refresh.

To watch Demo B (camera fault → degrade → reroute → recover) unfold in swarmtop,
run the adapter-driven scenario:
```bash
make demo SCENARIO=hardware-fault      # live sim adapter drives the fault; swarmtop shows it
```

## Building from source (contributors)
```bash
make -C tools/swarmtop build    # or: make swarmtop from the repo root
make -C tools/swarmtop test
make -C tools/swarmtop run      # build + run against $KUBECONFIG
```
While staged in-tree, `tools/swarmtop/go.mod` uses
`replace github.com/swarmada/swarmada => ../../` so it builds against the sibling
core checkout. On the eventual repo split that line becomes a tagged `require` —
no import changes.

## Troubleshooting
| Symptom | Cause | Fix |
|---|---|---|
| Empty lists, no error | wrong namespace/context, or fleet not up yet | add `-n warehouse-a`; confirm `make quickstart` finished |
| `forbidden` on start | kubeconfig identity can't read `swarmada.io` | use the demo's kubeconfig context (the quickstart sets it) |
| `command not found` | not built yet | `make swarmtop` → `tools/swarmtop/bin/swarmtop` |

## Related
- [ADR-0018](adr/0018-swarmtop-repository-placement.md) — why swarmtop is demo-only and in-tree for now
- [`docs/deploy-minikube.md`](deploy-minikube.md)
