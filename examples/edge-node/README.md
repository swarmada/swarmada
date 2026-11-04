# Edge node — runnable example (no hardware)

This example demonstrates the Swarmada **Zone Controller edge node** (`cmd/edge`,
RFC-0001 #safety-edge-headless-emergency-stop / #fleet-adapter-protocol-the-edge-stream) end to end with no real hardware and no control plane:
a Fleet Adapter tees robot positions to the edge node over `EdgeService.EdgeStream`,
and the edge node issues a **headless, confirmed-only emergency stop** when a robot
crosses its zone boundary — the safety path that keeps working during a control-plane
partition.

## Files

| File | Purpose |
| :--- | :--- |
| [`../../config/samples/fleetzone_edge.yaml`](../../config/samples/fleetzone_edge.yaml) | A `FleetZone` with `spec.edgeNode` + `physicalBounds`, and a `Robot` assigned to it — the resources a real edge node syncs from. |
| [`edge-config.json`](edge-config.json) | The `cmd/edge` `--config-file` bootstrap, mirroring the FleetZone's polygon + robot assignment (so the demo runs with no cluster). |
| [`safety-input`](safety-input) | A `0`/`1` digital-input file for the local safety-input hook (`1` = safe, active-low). |
| [`demo/`](demo/) | A Go client that plays the adapter's EdgeStream role: tee positions, receive the estop, confirm it. |
| [`run-demo.sh`](run-demo.sh) | Builds and starts `cmd/edge`, then runs the demo client. |

## Run it

```bash
examples/edge-node/run-demo.sh
```

Expected output (the confirmed-only estop path):

```
teed in-zone position (10,10) for amr-1 — inside the zone, no estop expected
teed OUT-OF-ZONE position (50,50) — a boundary breach; expecting a headless estop
received headless Estop id="edge-estop-amr-1-1" reason="zone-boundary-breach: robot amr-1 outside zone warehouse-a" issuedBy="edge:warehouse-a"
sent EstopAck(STOPPED) — the edge node records a CONFIRMED headless stop
demo OK: headless estop issued on the boundary breach and CONFIRMED
```

## What the demo proves

1. **Boundary breach → headless estop.** An in-zone position produces nothing; a
   position outside the zone polygon makes the edge node push an `Estop` down the same
   stream — no control plane involved.
2. **Confirmed-only discipline (unchanged).** The edge records `Stopped` **only**
   because the demo replies with an `EstopAck{STOPPED}`. Drop that ack and the outcome
   is `Failed`, never a false `Stopped` (see `internal/edge` tests). The demo cannot
   fabricate a stop for a robot the edge doesn't guard (`TestRunScenario_UnknownRobotNoEstop`).

## Local safety input (light curtain / e-stop button / safety PLC)

While a stream is connected, trip the digital input and the edge node fans a zone-wide
headless estop to every connected `EdgeStream`:

```bash
echo 0 > examples/edge-node/safety-input   # active-low: 0 (or unreadable) = tripped
echo 1 > examples/edge-node/safety-input   # reset to safe
```

`cmd/edge --safety-input-file` polls this file; an unreadable input is treated as
**tripped** (fail-safe). Point it at a Linux sysfs GPIO value file
(`/sys/class/gpio/gpioN/value`) or a safety-PLC output bridged to a file — wiring the
physical line is a manual step, not faked here.

## Wiring the real simulation adapter

The Go `demo` client above stands in for the adapter's EdgeStream role so the example
is deterministic and self-contained. In a real deployment the
[`adapters/simulation`](../../adapters/simulation) sim adapter (or any Fleet Adapter)
reaches the edge node like this:

1. Apply `config/samples/fleetzone_edge.yaml`; the API Server advertises
   `spec.edgeNode.address` in `RegisterAck.edge_endpoints` to adapters whose robots'
   zone chain includes `warehouse-a`.
2. The adapter opens the EdgeStream to that address and tees a `PositionFrame` per
   robot per telemetry tick (C8.2), and honors an edge-issued `Estop` with the same
   confirmed `EstopAck` discipline it uses on `SafetyStream` (C8.3).

## Production transport

The demo uses `--insecure` for a zero-cert run. In production `cmd/edge` requires
mTLS: pass `--tls-cert-file` / `--tls-key-file` / `--tls-client-ca-file` from the
`tls.crt` / `tls.key` / `ca.crt` of the Secret named in
`FleetZone.spec.edgeNode.tlsSecretRef`. Only adapters with a namespace-CA-issued
client certificate can open the EdgeStream.
