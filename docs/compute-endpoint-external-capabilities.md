# External capabilities reachable through a ComputeEndpoint

A survey of what can sit behind `spec.executor.endpointRef`, and what each costs to reach.

Companion to `compute-endpoint-integration-patterns.md`, which covers the shapes. This one covers
the inventory. Neither is normative; RFC-0001's `ComputeEndpoint` chapter is.

## The two boundaries that decide where something goes

**A robot goes behind a `FleetAdapter`, not a `ComputeEndpoint`.** The chapter is explicit: a
`ComputeEndpoint` is how a `FleetAction` dispatches work to *something that is not a robot*. VDA 5050,
ROS 2/Nav2, MAVLink, Matter RVC, MiR and Open-RMF are robot protocols and belong to the adapter
contract. Reaching them through a compute endpoint would put fleet authority behind an object whose
defining rule is that its result is never a command.

**The binding set is closed at three.** `Http`, `Grpc`, `Mcp` — and the chapter says adding a fourth
is a specification change. So an integration is either natively one of those, or it needs a shim
service in front of it. **The shim is where the conformance obligation lands**: it is the endpoint,
and `CE1`–`CE8` apply to it, not to the system behind it.

That second point is the main cost driver below, so each row names it.

---

## 1 · Perception and inference

The original case. A model scores an input and returns a value.

| Capability | Binding | Native? |
| :-- | :-- | :-- |
| Object / package classification | `Http` or `Grpc` | most serving runtimes speak both |
| OCR — labels, plates, placards, meters | `Http` | usually |
| Barcode / QR / DataMatrix decode | `Http` | usually |
| Anomaly detection on a sensor stream | `Grpc` + `contract: Service` | resident by nature |
| Speech-to-text for a spoken instruction | `Http` | usually |
| Pose or dimension estimation | `Grpc` | — |
| Defect or damage inspection | `Http` | — |

**The constraint that bites:** `CE2.8` — a model-derived result carries the model's identity and
version. Without it a fleet decision cannot be traced to the weights that made it, which matters the
first time a classifier is retrained and behaviour changes with no manifest change.

---

## 2 · Language models, agents and tool servers

Three distinct shapes, and conflating them is the usual mistake.

| Capability | Binding | Contract | Note |
| :-- | :-- | :-- | :-- |
| A model API called once, structured output | `Http` | `App` | the reply is the run |
| An MCP tool server | `Mcp` | `App` | `Service` is **refused at admission** by a CEL rule |
| An agent orchestrator that runs a multi-step plan | `Grpc` | `Service` | resident; streams events |
| A retrieval / knowledge service | `Http` or `Mcp` | `App` | — |
| A human-in-the-loop console — chat, web, voice | `Grpc` | `Service` | see the patterns doc §3 |

**This is the category with a stated fleet-safety threat**, and it is worth quoting rather than
paraphrasing:

> With an MCP-bound executor in the loop, anything written in a robot's field of view can reach a
> language model whose output becomes a result; prompt injection stops being a chat-safety concern
> and becomes a fleet-safety one.

A sign taped to a crate is untrusted input. The mitigations are architectural, not prompt-level:

- give the model tools that **read** and **propose**, never tools that execute
- a proposal returns a structured refusal reason, and has no confirm path at all
- the response reaches an actuator only through a construct the task author wrote in advance — a
  `select` over an enumerated case set, a loop `until`, or a trigger with a closed response set
- where a response would commit new physical work, an approval gate is the default

---

## 3 · Industrial and building systems

None of these speak `Http`, `Grpc` or `Mcp` natively. **Every one needs a shim**, and the shim is
the conformant endpoint.

| System | Native protocol | Shim cost |
| :-- | :-- | :-- |
| OPC UA server — machine tools, PLC-fronted lines | OPC UA binary / TCP | moderate; a session and a subscription model to map |
| MQTT broker — sensors, telemetry, lightweight actuators | MQTT | low; but pub/sub to request/response is a real impedance mismatch |
| Modbus TCP / RTU device | Modbus | low; register maps are the work |
| BACnet — HVAC, building automation | BACnet/IP | moderate |
| KNX — lighting, blinds, building services | KNX/IP | moderate |
| Warehouse Control System (WCS) / Material Handling System | vendor REST or proprietary | varies; often already HTTP |
| SCADA historian | OPC UA or vendor | moderate |

**The pub/sub mismatch is the one to plan for.** MQTT and OPC UA subscriptions push; a
`contract: App` endpoint is asked and answers. Either the shim holds last-known state and answers
from it — and then says how stale it may be — or it is `contract: Service`, stays resident, and
streams. Silently answering from a stale cache is the failure mode.

---

## 4 · Facility hardware and actuators

The switch-and-toggle family. Legitimate — `CE1.3` speaks of "the work's side effects" — provided
the idempotency rule holds.

| Capability | Typical reach |
| :-- | :-- |
| Bay door, roll-up door, gate | building controller or relay board, over HTTP |
| Lift / elevator call and floor request | building system; often OPC UA or vendor API |
| Automatic door, turnstile, airlock | access-control system |
| Conveyor segment start / stop / speed | WCS or PLC |
| Charger enable, dock release | charger vendor API |
| Lighting zone, HVAC setpoint | KNX, BACnet |
| Smart plug, relay, valve | vendor cloud or local API |
| Signal tower, siren, announcement | relay or PA system |

**One rule dominates the whole category: declare absolute states, never relative transitions.**
`toggle` cannot be made conformant — `CE1.2` requires the same reply for a repeated invocation key
and `CE1.3` requires side effects at most once per key, and a toggle violates both by construction.
`state: open|closed` is retryable; `action: toggle` is one network blip from doing the wrong thing.

**Second rule: the world pushing back is not a failure.** An obstructed door returns its actual
state with an `Obstructed` event, `Succeeded`. `Failed` would tell the control plane the endpoint
broke, when what happened is that reality disagreed.

---

## 5 · Enterprise and workflow systems

Where the fleet's work meets the business record.

| Capability | Binding | Note |
| :-- | :-- | :-- |
| WMS / ERP — order, inventory, putaway lookup | `Http` | usually native REST |
| CMMS — raise a maintenance ticket | `Http` | — |
| Ticketing / ITSM — incident from a fleet event | `Http` | — |
| Asset or identity directory | `Http` | — |
| Scheduling / shift system — is the floor staffed? | `Http` | a real gate for night operation |
| Chat, paging, on-call notification | `Http`, or `Grpc` if it waits for a reply | notify-only is `App`; ask-and-wait is `Service` |
| Label or document generation | `Http` | — |

Note the overlap with `ResultSink`, which has kinds `Database`, `ObjectStore` and `Webhook`. **A
sink is egress at the end of an action; an endpoint is work the action dispatches and waits on.**
Writing a detection row is a sink. Asking the WMS which bay a pallet goes to, and branching on the
answer, is an endpoint.

---

## 6 · Verification, safety-adjacent and compliance

Not safety-rated — the physical-safety-hardware boundary is a non-goal — but often the gate a task
must pass.

| Capability | Note |
| :-- | :-- |
| Access control — may this robot enter this area | a read that gates dispatch |
| Permit-to-work / lockout-tagout status | ditto |
| Environmental sensing — gas, temperature, air quality | may make an area temporarily undispatchable |
| Occupancy detection — is a human in the aisle | pairs with zone policy |
| Weight or load verification | after a pick |
| Audit or evidence archival | often a sink, not an endpoint |

**These are the strongest case for the scheduling gate.** An action whose external dependencies are
not present and ready is not assigned — it stays `Pending` with a condition naming the absent
entity. The chapter gives the physical reason: assignment occupies a robot, and under most action
types the robot has begun moving by the time a missing dependency is discovered.

---

## 7 · Choosing, in two questions

1. **Is it a robot?** Then it is a `FleetAdapter`, not this.
2. **Does the run outlive the request?** `Service` if yes, `App` if no.

Then, before writing the parameters: **would calling it twice with the same key be harmful?** If
yes, the parameters are wrong — re-express them as an absolute state.

## 8 · What this survey does not claim

Every row above is a shape that fits the contract. **None is an integration that exists in this
tree.** The reference implementation has no dispatch path at v0.4 — `spec.provision`,
`spec.parameters`, `spec.inputs`, `spec.emitsEvents`, `spec.probe` and `spec.streamPolicy` are
declared and validated but consumed by nothing, and `scheduler.ErrExternalAction` is returned for
any action carrying `spec.executor`.

So this is a map of what the contract can address, not a list of what has been built. The only
executors exercised end to end are the conformance fixtures under `conformance/fixtures/`.
