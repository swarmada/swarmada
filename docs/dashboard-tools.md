# Kubernetes dashboard options for watching Fleet Adapter / Robot state

Research pass (July 2026) on which Kubernetes UI to run alongside Swarmada so
`Robot`/`FleetTask`/`RobotProbe`/`FleetAdapter` status — including position,
battery, capability health, and adapter connectivity — is visible in a
friendlier form than `kubectl get` or `k9s` (`kubectl get` is what the demos in
`examples/` use; both are terminal-only, with no map view).

## The honest headline finding

**No general-purpose Kubernetes dashboard renders a warehouse/zone floor-map
view of robot position out of the box — and at v0.3 there is nothing on the CRD
for one to render.** `Robot.status.position` is specified but written by no
controller: position reaches the telemetry TSDB sink and the Zone Controller's
live zone derivation, and never etcd. A map view therefore has to be fed from
the TSDB, not from the resource. Every tool below shows CRD
fields as a table, tree, or JSON/YAML panel — genuinely useful for status,
capability health, and adapter connectivity, but "see robots move on a map"
needs either (a) Grafana's Geomap panel fed by `kube-state-metrics`'
`customResourceState` config, which requires mapping `x`/`y` into
lat/long-shaped series (an unnatural fit for indoor coordinates), or (b)
**`swarmview`**, a purpose-built web fleet dashboard for this project, which does
not exist: Swarmada has no web dashboard. Treat the tools below as the near-term "inspect status
honestly" answer, not a floor-map replacement. See also **`swarmtop`**
([`tools/swarmtop/`](../tools/swarmtop/README.md)), a Swarmada-native
*terminal* fleet inspector — now implemented, staged in-tree — a different
tool from `swarmview`, not a floor-map replacement either, but purpose-built
for Swarmada's fields the way none of the general tools below are. See the
dedicated section below.

## Comparison table

| Tool | Type | Custom Resource (CRD) support | Location/position visualization | Maintenance status (2026) | Deployment | Best fit for Swarmada |
|---|---|---|---|---|---|---|
| **Headlamp** | General K8s UI, CNCF Sandbox | Strong — plugin architecture lets you build a dedicated `Robot`/`FleetTask` view; ships a "Map" feature showing per-namespace resource relationship diagrams | No native geospatial map; the "Map" feature is a resource-relationship graph, not a floor plan | Actively maintained — officially adopted by Kubernetes SIG UI, Microsoft-backed (ex-Kinvolk), regular 2026 releases and a KubeCon EU 2026 presence | Desktop app or web app | **Best overall recommendation.** Officially the community's Kubernetes Dashboard successor; a Swarmada-specific plugin (position/battery/capability columns) is realistic scope, unlike hacking a generic tool |
| **Grafana + Prometheus + kube-state-metrics** | Metrics dashboard, not a K8s resource browser | Strong, but requires setup — `kube-state-metrics`'s `customResourceState` config can expose arbitrary CRD status fields (battery, phase, capability state) as Prometheus series | Possible via the Geomap panel, but built for lat/long — indoor `x`/`y`/`floor` needs a coordinate-transform hack, not a clean fit | Actively maintained, industry-standard | Self-hosted or Grafana Cloud | **Best for trend/history and alerting** (battery over time, fleet health over time) — already the direction `RobotProbe`/metrics are headed per `docs/architecture.md`. Complements Headlamp, doesn't replace it for "what does this Robot look like right now" |
| **Rancher** | Multi-cluster enterprise platform | Good — has CRD list/detail views, but they're a secondary feature next to its core multi-cluster/RBAC/fleet-of-clusters management | None | Actively maintained (SUSE) | Server-based (its own management cluster) | Overkill for a single-cluster quickstart; the right tool only once Swarmada is managing fleets *across* multiple clusters/sites (multi-cluster fleet management is a non-goal of RFC-0001), not for today's demo/dev loop |
| **Lens / OpenLens** | Desktop K8s IDE | Historically strong CRD browsing, but the open-source Lens Desktop was retired — Lens is now a Mirantis commercial product; OpenLens is a community fork with uncertain long-term maintenance | None | Mixed — commercial Lens active, OpenLens fork status less certain | Desktop only | Usable today, but the licensing/fork uncertainty makes it a riskier long-term recommendation to put in public docs than Headlamp |
| **`kubectl` / `k9s`** (current baseline) | Terminal | Full — anything `kubectl get -o yaml` shows | None | Actively maintained (k9s), core (kubectl) | Terminal | What the demos in `examples/` print for watching state (`kubectl get … -w`). Suited to live-demo narration in a terminal, but not a fit for a non-terminal audience — the gap the other tools here are compared on |
| **CR(D) Wizard** (`pehlicd/crd-wizard`) | CRD-specialized, web + TUI | Purpose-built for exactly this — visualizing CRDs and their CRs, multi-cluster kubeconfig-context aware | None | Small/community project — verify activity level before relying on it for a public-facing recommendation | Web or TUI | Worth a hands-on trial specifically because it's the only tool here built *around* CRDs rather than treating them as a secondary feature — but confirm it's not abandoned before recommending it publicly |
| **KubeDash** (`devopstales/KubeDash`) | General K8s web UI | General resource browsing; not CRD-specialized | None | Community project, activity unverified | Web | Lower priority than Headlamp — general-purpose with less clear CRD-first design |
| **Kubernetes Dashboard (official)** | Official K8s UI | Weak — CRD support has been a long-standing open gap (tracked since `kubernetes/dashboard#2493`) | None | **Archived** — no longer the recommended path | Web (in-cluster) | Do not recommend; explicitly superseded by Headlamp per Kubernetes SIG UI guidance |
| **Octant** (VMware) | Developer-centric K8s UI | Was strong (plugin architecture, app-centric view) | None | **Archived since late 2022** — still runs on many clusters but unmaintained | Desktop/local | Do not recommend for anything public-facing; superseded by Headlamp/Skooner |
| **Portainer** | General container platform (Docker-first) | Weaker — Kubernetes/CRD support is secondary to its Docker roots | None | Actively maintained | Server-based | Not a good fit — advanced CRD management is explicitly one of its weaker areas per its own comparison content |

## Recommendation

**Headlamp** for the near-term "watch adapter/robot status in a friendlier
way than k9s" ask — it's the officially blessed Kubernetes Dashboard
successor, actively maintained, plugin-extensible, and a Swarmada-specific
plugin (a `Robot`/`FleetTask` table with battery/capability/adapter-status
columns) is realistic scope rather than fighting a generic tool's assumptions.
Pair it with **Grafana + kube-state-metrics** once fleet history/alerting
matters (battery trend, fault-rate over time), since that's a different job
(trends, not live per-robot detail) that Headlamp doesn't try to do.

Neither gives you a real floor-map view of robot position — and, per the
headline above, that would have to be fed from the telemetry TSDB rather than
from `Robot.status.position`, which no controller writes at v0.3. It still
points back to `swarmview`, which does not exist; no tool here renders
an actual 2D zone map instead
of a generic resource table. `swarmtop` (terminal, built and staged in-tree)
covers the "friendlier than k9s" gap in the meantime without waiting on a web
app. See "Swarmada-native option: `swarmtop`" below.

## Swarmada-native option: `swarmtop`

Everything above is a *general-purpose* Kubernetes tool bent toward Swarmada's
CRDs. **`swarmtop`** is the inverse — a terminal fleet inspector built for
`Robot`/`FleetTask`/`RobotProbe`/`FleetAdapter` specifically, so battery
(color-coded), capability health, adapter connectivity and last handshake,
estop state, position, conditions, and recent events are first-class columns
and detail fields instead of something you assemble from a generic table or a
`jq` one-liner. It updates live over informers (push, not poll), and is what
the warehouse quickstart points at for the scenarios whose payoff isn't a
`kubectl get` printer column (`hardware-fault`, `comms-flaky`, `estop-drill`).

It lives at [`tools/swarmtop/`](../tools/swarmtop/README.md) today — staged
in-tree as its own Go module — and graduates to `github.com/swarmada/swarmtop`
once it splits to its own repository. Build it with `make -C tools/swarmtop build`.

**What it doesn't do (yet):** no floor-map / 2D position view — it shows plain
`x`/`y`/`floor` fields, and a real zone map stays `swarmview`'s job; and it is
**read-only** (no apply/admit/delete — mutating the fleet stays `swarmctl`'s job).

## Sources

- [Best Kubernetes Dashboard Alternatives & Competitors in 2026 (Portainer)](https://www.portainer.io/blog/kubernetes-dashboard-alternative)
- [Kubernetes Dashboard Is Archived: 9 Alternatives (2026)](https://alexandre-vazquez.com/kubernetes-dashboard-alternatives-2026/)
- [Best Kubernetes UI Tools for On-Prem Clusters: Rancher, Lens, Headlamp & More](https://medium.com/@PlanB./best-kubernetes-ui-tools-for-on-prem-clusters-rancher-lens-headlamp-more-113eb635283f)
- [7 Best Kubernetes GUI Tools in 2026 (Compared)](https://srexpert.cloud/blog/best-kubernetes-gui-tools-compared-2026)
- [Rancher vs Kubernetes Dashboard: Feature Comparison](https://oneuptime.com/blog/post/2026-03-20-rancher-vs-kubernetes-dashboard/view)
- [Headlamp in 2025: Project Highlights (kubernetes.io)](https://kubernetes.io/blog/2026/01/22/headlamp-in-2025-project-highlights/)
- [Headlamp Cluster API plugin announcement (kubernetes.io)](https://kubernetes.io/blog/2026/06/25/headlamp-cluster-api-plugin/)
- [Headlamp GitHub (kubernetes-sigs/headlamp)](https://github.com/kubernetes-sigs/headlamp)
- [Headlamp official site](https://headlamp.dev/)
- [CR(D) Wizard GitHub (pehlicd/crd-wizard)](https://github.com/pehlicd/crd-wizard)
- [KubeDash GitHub (devopstales/KubeDash)](https://github.com/devopstales/KubeDash)
- [kubernetes/dashboard CRD support issue #2493](https://github.com/kubernetes/dashboard/issues/2493)
- [Octant official site](https://octant.dev/)
- [Lens vs K9s vs Octant: Kubernetes UI Showdown](https://atmosly.com/blog/lens-vs-k9s-vs-octant-the-ultimate-kubernetes-ui-showdown-2025-edition)
- [kube-state-metrics custom resource state metrics guide](https://oneuptime.com/blog/post/2026-01-19-kubernetes-kube-state-metrics-custom/view)
- [Kube State Metrics v2 Grafana dashboard](https://grafana.com/grafana/dashboards/21742-object-s-health-kube-state-metrics-v2/)
