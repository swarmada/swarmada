# External event bridge

Systems outside the cluster (a door controller, a work-order system, a site scheduler,
a static camera) raise events. An in-cluster bridge authenticates each request, maps a
small declared vocabulary to `SemanticEvent`s, and refuses everything else with the
reason. `TaskTrigger`s turn the events into **draft** FleetTasks. Nothing moves until a
person clears a draft.

```
caller ──HTTPS, HMAC-signed──► event-bridge (Deployment) ──create──► SemanticEvent
      ──► TaskTrigger controller ──► FleetTask, spec.draft: true ──► a person clears it
```

The bridge is a task source in the sense of ADR-0006: a client of the Kubernetes API.
Its Role grants `create` on `semanticevents` and nothing else. It cannot read an
event back, create a source or a trigger, touch a FleetTask, or clear a draft.

## Run it

```sh
examples/event-bridge/run.sh
```

Prerequisites: `kind`, `kubectl`, `helm`, `docker`, `go`, `openssl`, `python3`, as for
[first-run](../first-run/). The script brings up the control plane through
`examples/first-run/run.sh`, generates one key per caller, starts the bridge, sends
every case below through a port-forward, and stops with the drafts held. It never
clears a draft: it prints the `kubectl patch` a person runs to clear one.

`EVENT_BRIDGE_CLUSTER` names the cluster (default `swarmada-event-bridge`);
`EVENT_BRIDGE_USE_EXISTING=1` installs into an existing, empty cluster of that name;
`EVENT_BRIDGE_SKIP_INSTALL=1` skips the control-plane install.

**Expected result:** six SemanticEvents, five drafts, all at phase `Draft` with no child
action, and the line `PASS: signed outside events became SemanticEvents and held
drafts`. Exit code 0.

## Authentication

Each caller holds its own key. A request carries three headers:

| Header | Value |
| :----- | :---- |
| `X-Event-Bridge-Key-Id` | the caller's key id, for example `door-controller` |
| `X-Event-Bridge-Timestamp` | Unix time in seconds |
| `X-Event-Bridge-Signature` | `v1=` and the hex HMAC-SHA256 of `v1\n<key id>\n<timestamp>\n<body>` |

The bridge refuses, in this order, before it parses the body:

- a missing header;
- a key id it holds no key for;
- a timestamp more than five minutes from its clock, in either direction;
- a signature that does not match;
- a signature it has already accepted within the window (a replay).

The timestamp and the key id are inside the signed material, so a captured request
cannot be moved forward in time or presented under another caller's name. The replay
cache holds each accepted signature until its timestamp leaves the window. When the
cache is full the bridge refuses new requests rather than evict one, because an evicted
signature would be replayable for the rest of its window.

**Why HMAC and not mutual TLS.** The callers are building systems, work-order and
scheduling products, and a camera adapter. Most of them can sign a webhook with a
shared secret, and few can present a client certificate. A signature also
authenticates the message, not the connection, so it survives a TLS-terminating proxy
in front of the bridge. Mutual TLS would add a certificate per caller to issue and
rotate, and would still need a replay defence for a request resent over a new
connection.

**Transport.** The bridge serves HTTPS only, with a certificate that cert-manager issues
(Secret `event-bridge-tls`); callers verify it with that Secret's `ca.crt`. The HMAC
authenticates a request and protects its integrity; TLS keeps its content private.

**Keys.** `run.sh` generates each key with `openssl rand` and passes it to `kubectl` on
standard input, so no key is written to a file or shown in a process listing. The
signer, `event-bridge send`, reads its key from the `EVENT_BRIDGE_KEY` environment
variable for the same reason. The bridge loads keys at start: rotating a key means
updating Secret `event-bridge-keys` and restarting the Deployment.

## Vocabulary

A TaskTrigger selects on a source and one event type, and nothing else: it has no
condition and copies nothing from the payload into the task (ADR-0063). So whatever a
trigger must tell apart has to be in the type. The bridge puts it there.

| Caller | Sends | Required data | Becomes, for source |
| :----- | :---- | :------------ | :------------------ |
| `door-controller` | `door.opened` | `door` | `door.opened.after-hours` or `door.opened.in-hours`, for `building-doors` |
| `work-orders` | `workorder.created` | `workOrder`, `zone`, `task: clean` | `workorder.clean.<zone>`, for `work-orders` |
| `scheduler` | `schedule.started` | `schedule` | `schedule.started.<schedule>`, for `schedules` |
| `camera-adapter` | `camera.detection` | `class`, `zone`, `confidence`, `camera` | `camera.detection.<zone>`, for `static-cameras` |

The working hours, the zones a request may name and the schedules are in ConfigMap
`event-bridge-config`. Whether a door opened after hours is judged by the event's
`occurredAt` when it has one, so a backlog delivered in the morning still reports a
night-time opening as after hours.

An envelope is one JSON object:

```json
{"id": "WO-1182", "type": "workorder.created", "occurredAt": "2026-09-28T09:12:00Z",
 "data": {"workOrder": "WO-1182", "zone": "warehouse-a", "task": "clean"}}
```

`id` is the caller's own identifier for the occurrence. The SemanticEvent is named
`evb-` plus a hash of the caller and the id, so the same occurrence delivered twice is
one SemanticEvent and one draft: the second delivery is answered `200 duplicate`.

## Refusals

Every refusal is a JSON body with a code and a reason.

| Code | Status | When |
| :--- | :----- | :--- |
| `MissingSignature`, `UnknownCaller`, `BadTimestamp`, `StaleTimestamp`, `BadSignature`, `Replayed` | 401 | authentication, above |
| `NotPermitted` | 403 | the caller may not send that type |
| `Malformed` | 400 | not exactly one envelope, or an unknown top-level field |
| `TooLarge` | 413 | body over 16 KiB |
| `UnknownType`, `UnknownField`, `MissingField`, `BadValue`, `UndeclaredZone`, `UndeclaredSchedule`, `UnsupportedWorkOrder`, `BadOccurredAt` | 422 | the envelope does not map |
| `APIRefused` | 502 | the API server refused the SemanticEvent |
| `ReplayCacheFull`, `APIUnavailable` | 503 | try again later |

Behind the bridge the control plane refuses too: a SemanticEvent naming a source that
does not exist, or a type its source does not declare, is `Rejected` and fires nothing.

## Least privilege

RBAC grants the bridge `create semanticevents`, which alone would let it raise an event
for any source in the namespace and fire that source's triggers. The
ValidatingAdmissionPolicy in `overlay/bridge-policy.yaml` holds it to its four sources,
the `evb-` names and the `managed-by` label. It is evaluated by the API server and needs
Kubernetes 1.30 or later.

## What the operator sees

A draft from the bridge is an ordinary held FleetTask:

```sh
swarmctl get fleettasks -n warehouse-a
swarmctl describe fleettask <draft> -n warehouse-a
```

The draft names its origin (`swarmada.io/draft-origin`: the SemanticEvent and the
trigger), and `swarmctl get semanticevents -n warehouse-a` shows each event with the
caller and the caller's id in its payload.

**A draft for a robot that is not connected.** The work-order trigger drafts a clean
pinned to `vacuum-01`. The draft is written whether or not that robot is connected: a
trigger checks nothing about robots, and a held draft looks the same either way. Check
`swarmctl get robots` before clearing it. If the robot is absent when the draft is
cleared:

- when no connected adapter advertises an action catalog, the child action is created
  and stays `Pending` with `no eligible robot`, and dispatches when the robot connects;
- when a connected adapter advertises a catalog without `Clean`, admission refuses the
  child action. The task stays `Pending` with no child action, and the refusal is on the
  task: its `ChildrenAdmitted` condition is `False` with reason `AdmissionRefused` and a
  message that names the action and the admission error, and a `ChildAdmissionRefused`
  Warning Event is recorded against it. `swarmctl describe fleettask <draft> -n warehouse-a`
  shows both. The controller retries the create; once the child is created the condition
  turns `True` with reason `Admitted`.

## Limits

- One replica. The replay cache is in memory; a second replica would not see the
  first one's signatures. A restart empties the cache. A request replayed within the
  window after a restart is authenticated again, reaches the API server under the same
  SemanticEvent name, and is answered as a duplicate. No second draft is created.
- The bridge cannot confirm that a zone in its config exists as a FleetZone: its Role
  cannot read FleetZones. Keep the zones in the config, the sources' event types and
  the triggers in step by hand.
- The camera path is for a camera adapter that can reach this bridge and not the API
  server. An adapter that can reach the API server creates its SemanticEvents directly.
