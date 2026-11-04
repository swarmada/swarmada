# ComputeEndpoint integration patterns

A `ComputeEndpoint` is a reference to an addressable executor — a binding, a provisioning mode, and
a contract. It is how a `FleetAction` dispatches work to something that is not a robot. This
document shows the range that reference covers, from a stateless classifier to a wall switch, and
states which rule constrains each.

It is not normative. RFC-0001's `ComputeEndpoint` chapter is; where the two disagree, the chapter
wins. Every rule cited below (`CE1.x`, `CE2.x`, …) is defined in `adapters/CONFORMANCE.md` and
`conformance/COMPUTE-ENDPOINT.md`.

One rule governs every pattern here:

> **An endpoint's result is data. It is never a command.**

---

## 1 · Stateless inference — a classifier

The simplest integration. A model scores an input and returns a value the task branches on.

```yaml
apiVersion: swarmada.io/v1
kind: ComputeEndpoint
metadata: { name: package-classifier, namespace: dock-3 }
spec:
  binding: Http
  contract: App                      # request/response; the reply is the run
  provision: { existing: { service: package-classifier, port: http } }
  parameters:
    - { name: minConfidence, kind: number, min: 0, max: 1, required: false, default: "0.8" }
  results:
    - { name: label,      kind: string }
    - { name: confidence, kind: number }
  inputs:
    - { name: frame, kind: SensorStream, mediaTypes: ["image/jpeg"], required: true }
  emitsEvents: [PackageClassified, NoPackageFound]
  probe: { path: /healthz, intervalSeconds: 30, failureThreshold: 3 }
```

**What constrains it.** `CE2.2` — "worked, found nothing" is `Succeeded`, not `Failed`; an empty
result is an answer. `CE2.6` — return only declared result fields. `CE2.8` — where a result is
model-derived, carry the model's identity and version, so a fleet decision can be traced to the
weights that produced it.

**Branching on it.** The task author writes the case set in advance:

```yaml
flow:
  select:
    subject: { fromResult: { member: classify, field: label } }
    cases:
      - { when: { equals: "pallet" }, then: { run: route-to-forklift } }
      - { when: { equals: "parcel" }, then: { run: route-to-amr } }
    default: { run: hold-for-operator }        # required by the schema
```

`default` is a required key. A model returning something outside the case set lands there, so an
unfamiliar label parks the work instead of dropping it.

---

## 2 · A model with tools — the MCP binding

An MCP tool server lets a model call functions. The binding exists so the control plane can address
one without inventing a protocol for it.

```yaml
spec:
  binding: Mcp
  contract: App                      # Service is REJECTED at admission — see below
  provision: { existing: { service: site-knowledge, port: http } }
  results:
    - { name: answer,   kind: string }
    - { name: citation, kind: string }
  emitsEvents: [AnswerFound, NoAnswer]
```

**`binding: Mcp` with `contract: Service` is refused by a CEL rule at the API server.** MCP is
request/response; a resident MCP run is not a shape the protocol has, and the rule stops the
mismatch reaching a cluster.

**What constrains it, and this is the pattern that needs the most care.** The chapter states the
threat directly:

> With an MCP-bound executor in the loop, anything written in a robot's field of view can reach a
> language model whose output becomes a result; prompt injection stops being a chat-safety concern
> and becomes a fleet-safety one.

A label on a crate is untrusted input. So:

- `CE2.5` — the reply must contain nothing interpretable as an action, a target or an instruction.
- The control plane **must not** contain a path by which a response reaches an actuator without
  passing through a construct the task author wrote in advance: a `select` over an enumerated case
  set, a loop `until`, or a trigger whose response set is closed.
- Where a response would commit new physical work, an approval gate is the default rather than the
  exception.

The practical consequence: **give the model tools that read and propose, never tools that
execute.** A three-tool surface of `list`, `status` and `propose` — where `propose` returns a
structured `{ok, reason, hint}` and has no confirm path — keeps the model's authority at zero while
still letting it turn a vague request into a specific question.

---

## 3 · A resident agent — long-running, and the one that can ask a human

`contract: Service` is a run that stays up. It is the shape for work that outlives a request:
watching a stream, supervising a sequence, or waiting for a person.

```yaml
spec:
  binding: Grpc
  contract: Service
  provision: { existing: { service: operator-console, port: grpc } }
  parameters:
    - { name: question, kind: string, required: true }
    - { name: choices,  kind: enum, allowed: ["confirm", "reject", "defer"], required: true }
  results:
    - { name: answer,   kind: string }
    - { name: answered, kind: bool }
  emitsEvents: [AnswerReceived, QuestionExpired]
  streamPolicy: { pinEndpoint: true, onEndpointLoss: Replace }
```

**Why this shape and not a blocking HTTP call.** `CE1.5` — an endpoint must not require the caller
to hold a call open for the run's duration. For `Grpc`, `Activate` returns promptly and progress is
observed later via `StreamStatus` and `Result`. A person takes minutes; a reconcile loop does not
wait that way.

**What happens when nobody answers.** `CE7.2` — a resident run self-stops when renewal ceases, and
the safe direction is off. The unanswered question expires, the run ends, and the task's `select`
takes its required `default`. Nothing hangs, and the no-answer path is one the author wrote.

**`pinEndpoint: true` is required for `contract: Service`** — splitting a resident run across
replicas loses the state that made it resident.

---

## 4 · Hardware — a switch, a relay, a door

An endpoint may actuate. `CE1.3` speaks of "the work's side effects", so a physical consequence is
contemplated, not excluded. "Read Only" describes an endpoint's **fleet authority** — it cannot
command robots — not its effect on the world.

```yaml
apiVersion: swarmada.io/v1
kind: ComputeEndpoint
metadata: { name: bay-door, namespace: dock-3 }
spec:
  binding: Http
  contract: App
  provision: { existing: { service: bay-door-controller, port: http } }
  parameters:
    - { name: state, kind: enum, allowed: ["open", "closed"], required: true }   # absolute
  results:
    - { name: state,      kind: string }     # the state after the call
    - { name: changed,    kind: bool }       # whether this call moved it
  emitsEvents: [DoorOpened, DoorClosed, DoorObstructed]
  probe: { path: /healthz, intervalSeconds: 10, failureThreshold: 2 }
```

### Declare absolute states, never relative transitions

**`toggle` cannot be made conformant.** `CE1.2` requires the same reply for a repeated invocation
key, and `CE1.3` requires side effects at most once per key. A `toggle` verb violates both by
construction: its result depends on prior state, and a retry — which the control plane is entitled
to make — flips the device back.

So the parameter is `state: open|closed`, not `action: toggle`. Setting an already-open door to
`open` is a successful call that reports `changed: false`. That is `CE2.2` again: nothing to do is
a success, not a failure.

This is the single most useful thing to get right when wrapping a switch, a relay, a valve, a light
or a conveyor. **Idempotent by construction, or it is not retryable, and an endpoint that is not
retryable is one network blip away from doing the wrong thing.**

### The obstruction case

A door that will not close because something is in the way is not a failed call — it is a
successful call that reports the truth. Declare the event (`DoorObstructed`), return the actual
state, and let the task branch. A `Failed` status here would tell the control plane the endpoint
broke, when what happened is that the world pushed back.

### What an actuating endpoint must not become

An endpoint that opens a door is a peer of the fleet, not a controller of it. It returns state; it
does not schedule, assign, redirect or stop a robot. The result-is-data rule is what keeps a
compromised or confused actuator from acquiring fleet authority it was never granted.

---

## 5 · Choosing a shape

| The work | Binding | Contract | Deciding property |
| :-- | :-- | :-- | :-- |
| Score one input, return a value | `Http` | `App` | reply is the run |
| Model with tools, one question | `Mcp` | `App` | `Service` is refused at admission |
| Watch a stream, or wait for a person | `Grpc` | `Service` | `CE1.5` — do not hold the call open |
| Set a physical state | `Http` | `App` | idempotent parameters, or not retryable |

Two questions settle most cases:

1. **Does the run outlive the request?** If yes, `contract: Service`. If no, `App`.
2. **Would calling it twice with the same key be harmful?** If yes, the parameters are wrong —
   re-express them as an absolute state.

## 6 · What none of these may do

- Return anything interpretable as an action, a target or an instruction (`CE2.5`).
- Reach an actuator except through a construct the task author wrote in advance.
- Claim the `Service` contract on a request/response binding (`CE3.4`, `CE5.4`).
- Report "worked, found nothing" as `Failed` (`CE2.2`).
- Change what it computes without changing what it reports about itself (`CE8.3`).
