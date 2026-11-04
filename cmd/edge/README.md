# `cmd/edge` — Swarmada Zone Controller edge node

The edge node is a standalone binary that runs on **facility-LAN hardware** and serves
`EdgeService.EdgeStream` (RFC-0001 #safety-edge-headless-emergency-stop, #fleet-adapter-protocol-the-edge-stream). Fleet Adapters tee robot
positions to it, and it issues **headless emergency stops** on a zone-boundary breach —
a safety channel that keeps working during a **control-plane partition**. It wraps the
audited `internal/edge.Node` library; this binary is the deployment around it.

## The safety invariant (never weakened)

The edge node applies the **confirmed-only estop discipline** (RFC-0001 #safety-estop-state-machine), unchanged by
any of the deployment scaffolding below:

- A robot is recorded **`Stopped` only** on an adapter-**confirmed** `EstopAck.state = STOPPED`.
- **Absence is never a breach.** A missing position frame, a nil position, or absent
  coordinates never triggers an estop.
- A dropped ack, silence, an adapter `FAILED`, or `STOPPING`-without-`STOPPED` in the
  window all resolve to **`Failed`, never `Stopped`** — recorded in the edge node's local
  log; it is never mistaken for a safe stop.
- A frame for a robot **not in the node's config** is ignored — the edge cannot be made
  to estop (or vouch for) an unknown/forged robot.

Every headless estop is appended to the edge node's **local audit log** (RFC-0001 #safety-safety-audit-log), which
the reference edge node keeps in memory. Nothing reports an edge estop to the control plane: it does
not record it, does not withhold the robot from dispatch, and has no clear path for it
([known limitations](../../docs/known-limitations.md#an-edge-estop-is-invisible-to-the-control-plane)).

---

## 1. Serving the EdgeStream (mTLS + health)

```bash
go build -o bin/edge ./cmd/edge

bin/edge \
  --namespace warehouse-a \
  --config-file /etc/swarmada-edge/config.json \
  --listen-address :9600 \
  --health-address :9601 \
  --tls-cert-file  /etc/swarmada-edge/tls/tls.crt \
  --tls-key-file   /etc/swarmada-edge/tls/tls.key \
  --tls-client-ca-file /etc/swarmada-edge/tls/ca.crt
```

**Transport security (RFC-0001 #fleet-adapter-protocol-transport-security, #security-transport-security).** The listener enforces **mTLS**: it presents
the server certificate from `--tls-cert-file`/`--tls-key-file` and **requires** every
Fleet Adapter to present a client certificate that verifies against the namespace CA in
`--tls-client-ca-file` (`RequireAndVerifyClientCert`, TLS 1.3). Those three files are
the `tls.crt` / `tls.key` / `ca.crt` of the Secret named in
`FleetZone.spec.edgeNode.tlsSecretRef`, **projected onto the edge host as files** — the
node holds its certs locally so it does not depend on the API server at connect time
(that is what lets it survive a partition). A client with no certificate, or one not
issued by the namespace CA, is rejected at the handshake and never reaches EdgeStream.

> `--insecure` serves plaintext for a dev/demo run and logs a prominent warning. Never
> use it in production.

**Health / readiness.** `--health-address` serves:

- `GET /healthz` — liveness (the process is up).
- `GET /readyz` — readiness; returns 200 only once the EdgeStream listener is bound, so
  a supervisor/load balancer does not route before the node can accept adapter streams.

**Config file** (`--config-file`) is the bootstrap Config — the zones the node guards
and the robot→zone assignments:

```json
{
  "zones": [
    { "name": "warehouse-a", "floor": 0,
      "polygon": [ {"x":0,"y":0}, {"x":20,"y":0}, {"x":20,"y":20}, {"x":0,"y":20} ] }
  ],
  "robotZone": { "amr-1": "warehouse-a" }
}
```

A polygon needs ≥ 3 vertices; a robot must reference a known zone; unknown fields are
rejected (strict decoding).

---

## 2. Config sync (fail-safe, last-known-good)

Hand-editing the config file does not scale. With a Kubernetes client the node keeps its
zones/robots current from the live resources:

```bash
bin/edge --namespace warehouse-a --config-file /etc/swarmada-edge/config.json \
         --kubeconfig /etc/swarmada-edge/kubeconfig \
         --sync-interval 30s   ... (tls flags as above)
```

- Source of truth: each **`FleetZone.spec.physicalBounds`** → a guarded zone polygon;
  each **`Robot.spec.zone`** → a robot→zone assignment (namespace-scoped).
- `--kubeconfig` empty ⇒ in-cluster config; if neither is available the node runs on the
  static `--config-file` only.

**Fail-safe (the reason the edge node exists).** A control-plane outage surfaces as a
sync error, and the syncer then **retains the last-synced config** — it never calls
`SetConfig` on error, and it **refuses to apply a zero-zone config over a non-empty one**
(a transient/partial read must not blank out active guards). The node keeps guarding its
zones through the outage; zone/robot assignments are never silently dropped. Config swaps
are atomic, so a boundary check always sees one coherent config.

---

## 3. Local safety input (light curtain / e-stop button / safety PLC)

A physical safety input can drive a zone-wide headless estop via `Node.TriggerLocalEstop`:

```bash
bin/edge ... --safety-input-file /sys/class/gpio/gpio17/value \
             --safety-input-active-low=true \
             --safety-input-interval 100ms
```

- The reference `FileSafetyInput` reads a digital line exposed as a **file** whose
  contents are `0`/`1` — the shape of a Linux **sysfs GPIO** value file or a safety-PLC
  output bridged to a file.
- `--safety-input-active-low` (default) models **normally-closed** wiring: the line reads
  `0` when tripped and `1` when safe, so a **cut wire or lost power reads as tripped**.
- The monitor is **edge-triggered**: a held button asserts once (the Node then runs the
  confirmed-only estop discipline).
- **Fail-safe:** an unreadable input (missing file, I/O error) is treated as **tripped**,
  not ignored.

**Wiring the physical line is a manual step** — export/configure the GPIO (or bridge a
safety-PLC output to a file) and point `--safety-input-file` at it. Swarmada does not ship
a GPIO abstraction layer; the file is the seam. Known limitation: edge-triggered
assertion does not retro-estop an adapter that connects *after* a held trip.

---

## 4. Runnable example (no hardware)

[`examples/edge-node/`](../../examples/edge-node/) demonstrates the whole path with no
control plane and no hardware:

```bash
examples/edge-node/run-demo.sh
```

It starts `cmd/edge` and runs a small EdgeService client that tees an in-zone position
(no estop), then an out-of-zone position (a breach) → receives the headless `Estop` →
confirms it. See that directory's README, and the sample resource at
[`config/samples/fleetzone_edge.yaml`](../../config/samples/fleetzone_edge.yaml).

---

## Kubernetes deployment notes

- **Certificates:** mount the `spec.edgeNode.tlsSecretRef` Secret as a volume and pass
  its `tls.crt`/`tls.key`/`ca.crt` to the three `--tls-*` flags.
- **Config sync:** grant the node a ServiceAccount with `get`/`list`/`watch` on
  `fleetzones` and `robots` in its namespace, and pass in-cluster config (omit
  `--kubeconfig`).
- **Placement:** run the edge node on facility-LAN hardware close to the robots, so the
  EdgeStream and any local safety input remain available when the link to the control
  plane is down — the scenario this node exists for.
- **Probes:** wire `livenessProbe` → `/healthz` and `readinessProbe` → `/readyz` on
  `--health-address`.
