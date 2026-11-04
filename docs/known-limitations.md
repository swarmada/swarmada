# Known limitations

Swarmada is alpha. This page lists what this release does not do that a first deployment is likely
to assume it does. Each entry says what the limitation is, what you will see, and what to do
instead. The per-resource maturity grades are in
[RFC-0001, Resource model](../rfcs/dist/RFC-0001-core-spec.md#resource-model); where this page
and the RFC differ, the RFC governs.

## Installing

### No published image, binary or package

**Limitation.** No container image, `swarmctl` binary, Helm package or Python package is
published. A release is a tag of the source. Building from source is the only install path.

**What you will see.** The chart's default image, `ghcr.io/swarmada/swarmada` at the chart's
`appVersion`, is not published. A `helm install` with default values creates a manager Pod that
cannot pull its image.

**Instead.** Build the manager image with `make docker-build IMG=<ref>` and make it available to
your cluster, then set `image.repository` and `image.tag` to the image you built; see
[deploy/swarmada/README.md](../deploy/swarmada/README.md).
Build `swarmctl` with `make swarmctl`, which writes `bin/swarmctl` for the host platform.
`make first-run` does all of this on a `kind` cluster.

### Kubernetes 1.31 or later

**Limitation.** The chart requires Kubernetes 1.31 or later. It is the oldest version measured to
admit an emergency stop on a `FleetZone` stored before one of that CRD's validation rules existed.

**What you will see.** `helm install` refuses an older cluster. Installing the CRDs with
`make install` does not check the version. On an older cluster, an estop on such a zone can be
refused at admission.

**Instead.** Use Kubernetes 1.31 or later.

### Python SDK: install from the repository, Python 3.10 or later

**Limitation.** `swarmada-sdk` and the conformance harness are not on PyPI. The SDK and the adapter
template declare Python 3.10 or later. The repository's own tooling declares Python 3.12 or later.

**What you will see.** `pip install swarmada-sdk` does not install this SDK. The project has not
published that name on any index, so whatever an index serves under it is someone else's code.
Never install it by name.

**Instead.** Install from a checkout, for example `pip install path/to/swarmada/sdk/python`. See
[Writing a Fleet Adapter, Python and runtime versions](adapter-authoring.md#python-and-runtime-versions).

## Running the control plane

### One manager replica while ControlStream is enabled

**Limitation.** Commands reach only the adapters connected to the leading replica. Multi-replica
command delivery is not implemented.

**What you will see.** `helm install` or `helm template` with `replicaCount` greater than 1 and
`controlStream.enabled=true` fails with `replicaCount=<n> with controlStream.enabled=true is refused.`

**Instead.** Run one replica. Leader election restarts a replica that crashed and carries a rollout,
but it does not keep the adapters' streams: every restart, rollout, crash or change of leader closes
them, and RFC-0001 then holds every robot until it is checked at the robot (the next entry). See
[deploy/swarmada/README.md, One replica](../deploy/swarmada/README.md#one-replica).

### A manager restart holds every robot until it is checked at the robot

**Limitation.** One manager replica serves every adapter's streams, so a manager restart, rollout,
crash or change of leader closes all of them. Every adapter then registers its robots again, and
RFC-0001 makes each such register an abnormal event (robot stop table, *Abnormal events and the
end-state stop*): every robot is held under its end-state stop, `Failed`, with its work paused, until a person has checked it at the robot and
recorded that check in an attestation (the `swarmada.io/estop-attestation` annotation), and an
operator has then released it (`swarmctl estop resume`, which takes several robots and releases each
on its own). This is the price of failing safe: the control plane cannot tell what a robot did while
its streams were closed. At v0.4 the control plane records no end-state stop, so a manager restart
holds nothing.

**What you will see.** At v0.4, robots carry on after a manager restart. Once the end state is
implemented, every manager upgrade, crash or change of leader leaves the whole fleet `Failed` until
each robot has been checked at the robot, attested and released.

**Instead.** Plan manager upgrades and restarts for a time when every robot can be checked at the
robot, attested and released, and keep the manager's node, resources and network stable so that it
is not restarted unplanned. See
[RFC-0001, Robot stop table](../rfcs/dist/RFC-0001-core-spec.md#safety-robot-stop-table),
*Abnormal events and the end-state stop*.

### The safety audit log is kept in memory by default

**Limitation.** Without `--audit-log-file`, the manager keeps the hash-chained safety audit log in
memory and loses it on restart. The Helm chart does not set the flag and mounts no volume for it.
Archival and retention of the log are not implemented.

**What you will see.** At every start, the manager logs
`WARNING: no --audit-log-file configured; the safety audit log is IN MEMORY and is lost on restart.`
`swarmctl get audit`, `verify audit` and `export audit` read an exported chain from a file or
standard input, so with the in-memory sink they have nothing to read.

**Instead.** For any fleet whose audit trail matters, run the manager with `--audit-log-file`
pointing at durable storage, and control access to that file through storage permissions. See
[RFC-0001, Accessing the audit log](../rfcs/dist/RFC-0001-core-spec.md#safety-accessing-the-audit-log).

### A namespace deleted during a manager outage stays Terminating

**Limitation.** The delete checks on `FleetZone`, `FleetTask`, `FirmwareRollout` and
`ModelRollout` are admission webhooks with `failurePolicy: Fail`. While no manager replica serves
the webhooks, every delete of those kinds is refused, the namespace controller's included. A
`ZoneMaintenance` carries a finalizer, `swarmada.io/zonemaintenance-resume`, that only the manager
removes. A namespace that holds any of these objects stays `Terminating` at least until a manager
replica serves again. Every namespace with robots holds a `FleetZone`, because a `Robot` must name
one. `Robot` deletes are not refused (see *A stop is kept on the Robot object only*).

**What you will see.** `kubectl delete namespace` prints `deleted` and the namespace stays
`Terminating`. Its `NamespaceDeletionContentFailure` condition names the webhook, for example
`failed calling webhook "vfleetzone.swarmada.io"`. When a manager replica serves again, the
deletion completes with no further command, unless the namespace holds one of the objects in *A
namespace holding an active maintenance window, a running task or an unfinished model rollout can
stay Terminating*, which hold it with the manager serving too.

**Instead.** Restore the manager. If the namespace must go first, follow
[Deleting a namespace during a manager outage](operations.md#4-deleting-a-namespace-during-a-manager-outage):
it takes the four delete checks out for every namespace in the cluster until you put them back.
While they are out, a zone that robots still refer to can be deleted, and a stopped zone goes with
its stop; so can a running `FleetTask`, with the assignment leases of its actions, and an
unfinished rollout.

### A namespace holding an active maintenance window, a running task or an unfinished model rollout can stay Terminating

**Limitation.** With the manager serving, three kinds of object can keep a namespace `Terminating`
indefinitely:

- An active `ZoneMaintenance`. The namespace controller deletes its `FleetZone` first, and the
  manager's removal of the `swarmada.io/zonemaintenance-resume` finalizer is then refused by the
  `ZoneMaintenance` check, because the zone it names no longer exists.
- A `FleetTask` in a phase the delete check refuses, such as `Running`, and a `ModelRollout` that
  is not `Succeeded` or `Failed`. Their delete checks do not exempt a namespace being deleted, and a
  task or rollout that cannot progress (no robot left to run it) never reaches a phase they admit.

A `FirmwareRollout` that is not `Succeeded` or `Failed` does not hold the namespace, although a
delete of the rollout itself is refused. Deleting the namespace deletes its `SwarmadaConfig`. The
rollout's verification, which runs on every reconcile whatever the phase, then finds no signing
policy and fails closed: the rollout is written `Failed`, a phase its delete check admits, and the
namespace goes with it. A `Pending` rollout's namespace went in 10 s on a kind cluster.

**What you will see.** The namespace stays `Terminating`. For a maintenance window,
`NamespaceFinalizersRemaining` names `swarmada.io/zonemaintenance-resume` and the manager logs
`removing finalizer: … FleetZone "<zone>" does not exist`. For a task or a model rollout,
`NamespaceDeletionContentFailure` quotes the check's refusal, for example `ModelRollout is in phase
Pending: only a terminal record (Succeeded/Failed) may be deleted`. For a firmware rollout the
namespace goes, and the manager logs `firmware rollout blocked by verification (fail closed)` with
the reason `no SwarmadaConfig in namespace "<namespace>"`.

**Instead.** Cancel running tasks (`swarmctl cancel task`) before deleting a namespace. For a
namespace already held, follow
[Deleting a namespace during a manager outage](operations.md#4-deleting-a-namespace-during-a-manager-outage):
its sections *Take the delete checks out* and *Remove a ZoneMaintenance finalizer* apply with the
manager serving too.

## Security and audit

### Some custom RBAC verbs are checked only by swarmctl

**Limitation.** `admit`, `reject`, `policy-reset` and `rollout-resume` are checked by `swarmctl`
before it writes an annotation. No admission webhook checks them.
The `read`, `export` and `verify` verbs on the audit log cannot be enforced, because the log is not
a Kubernetes API object.

**What you will see.** A principal with `patch` on `discoveredrobots`, `modelpolicies`,
`firmwarerollouts` or `modelrollouts` can admit, reject, reset or resume with `kubectl annotate`
without holding the custom verb.

**Instead.** Treat `patch` on those resources as granting the corresponding action. The built-in roles already pair them. See
[RFC-0001, Swarmada RBAC roles](../rfcs/dist/RFC-0001-core-spec.md#security-authorization-swarmada-rbac-roles)
for which verbs are checked where.

### An install without admission webhooks checks nothing at admission

**Limitation.** Four ways of running Swarmada install no admission webhooks:

- `make deploy-minimal` (`config/default`);
- the quickstart overlays: `quickstart-dev`, `quickstart-mtls`, `quickstart-projected`, and
  `demo-ab-live`, which builds on `quickstart-mtls`;
- the Helm chart with `webhooks.enabled=false`;
- `make run`, which runs the manager as a local process with `ENABLE_WEBHOOKS=false`.

On such an install none of the webhooks in `config/webhook/manifests.yaml` runs, and no rule
that RFC-0001's admission table marks *Validating webhook* is checked. Among what that opens:

- **The estop checks.** The `estop-trigger` and `estop-clear` verbs are not checked when the
  `swarmada.io/estop-triggered` annotation is added, changed or removed on a `Robot`, `FleetZone`
  or `SwarmadaConfig`, so `patch` on the object is enough to trigger or clear an estop.
- **The other custom verbs checked at admission.** `cancel` on a `FleetAction` or `FleetTask`,
  `append` on a `FleetTask` and `activate` on a `FleetTask` are not checked; `patch` is enough.
  Clearing `spec.draft` then dispatches a drafted task for any principal that can patch it, a
  ServiceAccount included.
- **The `robots/status` guard (`vrobotstatus`).** Any principal allowed to write `robots/status`,
  `swarmada:admin` as shipped, can set `status.estopState` to `Normal`, skipping the clear, the
  resume checks and the audit entry, and can rewrite the phase, hardware, capabilities and
  model status those checks read.
- **The delete guard.** A `Robot` that holds a stop can be deleted and created again `Normal`
  (see *A stop is kept on the Robot object only*).
- The RobotClass merge, the Robot admission gate and every `SwarmadaConfig` cross-field rule.

**What you will see.** Such a write succeeds with no refusal and no warning. `make run` prints
what it leaves open when it starts: the estop checks, the draft gate (`activate`), the
`robots/status` guard, the delete guard, RobotClass defaulting and the admission gates. `make deploy-minimal` prints
that it serves no admission webhooks, but not what that opens. The quickstart overlays and the
chart with `webhooks.enabled=false` print nothing about it.

**Instead.** Run anything other than a single-user development cluster with `make deploy`, or
with the Helm chart and its default `webhooks.enabled=true`. On an install without webhooks,
grant `patch` on robots, zones, `SwarmadaConfig`, actions and tasks, and write on `robots/status`,
only to people you would trust to clear an estop. See
[RFC-0001, Swarmada API server](../rfcs/dist/RFC-0001-core-spec.md#control-plane-swarmada-api-server)
for the admission table.

### Write access to CRDs is outside the Robot status guard

**Limitation.** `swarmada:admin` holds write on `customresourcedefinitions`. Bound cluster-wide
with a `ClusterRoleBinding`, it can remove the `status` subresource from the `Robot` CRD. A write
to the main `Robot` resource then carries status, the `robots/status` guard is not consulted,
and `status.estopState`, `status.phase` and `status.hardware` can be set directly, on an install
with admission webhooks too. Bound in a namespace with a `RoleBinding`, the role grants no CRD
write, because CRDs are cluster-scoped.

**What you will see.** The write is admitted, and no Swarmada Event or safety audit entry
records it.

**Instead.** Bind `swarmada:admin` with a `RoleBinding` in each namespace. Give CRD management to
a separate cluster role, held only by people trusted with the cluster's API.

### The audit log names the controller for some operator actions

**Limitation.** Estop trigger and clear, rollout resume and `SwarmadaConfig` changes record the
authenticated user. Robot admission, rejection, `FleetAction` cancellation and maintenance windows
record the controller that sealed the entry, not the operator. Where the stamping webhook did not
run, an estop entry's identity is prefixed `unattributed:`, unless the client wrote
`swarmada.io/estop-actor` itself. Where the webhooks are installed, that value is refused unless it
is the requester's name, written with a trigger, a clear or a robot's release. Where they are not
installed, nothing refuses it, and it is recorded as a `user`. A rollout's actor stamp is not
checked: where its stamping webhook did not run, a value the client wrote is recorded as a `user`.
A `Robot`'s stamp is always the authenticated user where the webhooks are installed.

**What you will see.** For example, `ROBOT_ADMITTED` with actor `robot-controller`, and
`FLEETACTION_CANCELLED` with actor `fleetaction-controller`.

**Instead.** For those events, use the Kubernetes API server audit log to find who wrote the
object. Check `FleetZone`, `SwarmadaConfig` and rollout entries there too before relying on their
`user` actor. See [RFC-0001, Audit logging](../rfcs/dist/RFC-0001-core-spec.md#security-audit-logging).

### An adapter with an empty servesRobotClasses list is refused for every robot

**Limitation.** RFC-0001 reads an empty `FleetAdapter.spec.servesRobotClasses` as "serves every
class", and admission accepts robots on that reading. The reference `ControlStream` authorizer reads
it as "serves none" and refuses every robot-scoped message from that adapter.

**What you will see.** Robots are admitted, but their registration, telemetry and status reports
are refused on the stream.

**Instead.** List the robot classes the adapter serves in `spec.servesRobotClasses`. See
[RFC-0001, Fleet adapter authentication](../rfcs/dist/RFC-0001-core-spec.md#security-fleet-adapter-authentication-robot-identity).

### A verified audit chain can still be missing its newest entries

**Limitation.** `swarmctl verify audit` detects an edited, removed or reordered entry, and a gap,
when the chain was not re-sealed after the change. It does not detect a log whose newest entries
were removed: what remains still chains, with no gap and no hash mismatch. Nor does it detect a
rewrite that was re-sealed from the changed entry on: the chain has no key and starts from a fixed
seed, so anyone who can write the audit sink can recompute it.

**What you will see.** `verify audit` passes a shortened log.

**Instead.** Read a passing `verify audit` as "what remains is intact and in order", not as "the
record is complete". To show completeness and detect a re-sealed rewrite, keep each namespace's
highest sequence number and its `chain_hash` outside the cluster, for example from a regular
`export audit`, and compare them with the entry at that number. See
[RFC-0001, Log entry format](../rfcs/dist/RFC-0001-core-spec.md#safety-log-entry-format).

### Model artifacts are not signature-verified by the control plane

**Limitation.** The control plane verifies a firmware artifact's signature itself and fails
closed. It does not verify a model artifact. It forwards `modelSignatureRef` to the Fleet Adapter,
sends no trust root, and records the signer the adapter reports without checking it.

**What you will see.** A `MODEL_SIGNATURE_VERIFIED` audit entry whose `verified_signer` is
whatever the adapter reported.

The protocol's comments on `PushFirmware.firmware_signature_ref` and
`ModelUpdate.model_signature_ref` say the adapter verifies against the namespace's
`spec.signing.trustRoots`. Nothing sends those trust roots to an adapter, so an adapter can verify
only against keys it was given some other way.

**Instead.** Provision each adapter with the public keys it verifies against, and confirm that it
refuses an unverifiable model. See
[RFC-0001, Model and firmware registry credentials](../rfcs/dist/RFC-0001-core-spec.md#security-model-and-firmware-registry-credentials).

### A ModelPolicy keeps only the latest waiting trigger

**Limitation.** While a policy's rollout is active, a new trigger waits in the
`swarmada.io/model-trigger` annotation. The annotation holds one trigger, so a trigger that arrives
while another is waiting replaces it.

**What you will see.** The replaced trigger is never evaluated. It has no `status.history` record,
no Event, and does not count toward `rejectionCount` or `consecutiveRejections`. The webhook still
answered `202 Accepted` for it.

**Instead.** Send one trigger per completed rollout, or wait until `status.activeRollout` is empty
before sending the next. Check `status.history` for every model version you sent. See
[RFC-0001, ModelPolicy schema](../rfcs/dist/RFC-0001-core-spec.md#modelpolicy-schema).

### The ModelPolicy webhook is plain HTTP and accepts a replayed request

**Limitation.** The manager serves the training-completion webhook over plain HTTP, on
`--model-webhook-bind-address` (the chart's `modelWebhook.port`, 9444 by default), and issues no
certificate for it. The HMAC signature authenticates the request and protects the body's integrity.
It does not keep the body confidential, and it covers no timestamp or nonce: the endpoint accepts a
captured request again and evaluates it again.

**What you will see.** A replayed request answered `202 Accepted` like a new one. A replayed rejection
adds to `consecutiveRejections` and can suspend the policy. A replayed pass resets
`consecutiveRejections` and, when the policy deploys automatically, creates the `ModelRollout` for
its version again if that rollout has been deleted.

**Instead.** Keep the webhook port on a network that only the training pipeline can reach, or put
TLS in front of it with an ingress or a service mesh. Protect the HMAC secret as the endpoint's only
credential. See
[RFC-0001, Training-Pipeline Integration](../rfcs/dist/RFC-0001-core-spec.md#modelpolicy-training-pipeline-integration-webhook-payload).

## Robots and safety

### Speed and payload limits are the adapter's to enforce

**Limitation.** `Robot.spec.constraints.maxSpeedMs` and `maxPayloadKg` are validated and stored.
Swarmada neither enforces them nor sends them to the adapter. The same holds for
`charging.minBatteryPctToCharge`, `charging.targetBatteryPct`, `motionThresholdMeters`,
`maxIdleIntervalSeconds`, `adapter.version` and `installedModels[].requiredHardware`.

**What you will see.** With admission webhooks on, the write succeeds with the warning
`spec.constraints.maxSpeedMs: setting this field has no effect in this release: Swarmada does not enforce this limit and does not send it to the adapter; the adapter must enforce it`.
With webhooks off, as in `make quickstart`, there is no warning.

**Instead.** Enforce speed and payload limits in the robot's own controller or safety system, and
configure them there. See [RFC-0001, Robot](../rfcs/dist/RFC-0001-core-spec.md#robot).

### A maintenance window does not halt robots

**Limitation.** A `ZoneMaintenance` window changes control-plane state only. The robot's phase
becomes `Maintenance` and no new work is assigned. No pause command is sent, so nothing confirms
that a robot has physically stopped.

**What you will see.** Robots in scope show `Maintenance` while a robot moving under its own
agent keeps moving.

**Instead.** To stop robots, use an emergency stop (`swarmctl estop trigger <zone> --reason <text>`) or the
robot's own controls, and do not enter a zone on the strength of a maintenance window alone. See
[RFC-0001, ZoneMaintenance](../rfcs/dist/RFC-0001-core-spec.md#zonemaintenance-the-pause-and-resume-commands).

### The edge node guards a robot's assigned zone

**Limitation.** The reference edge node (`cmd/edge`) measures a boundary breach against the zone
in `Robot.spec.zone`, not `status.currentZone`. It guards only a zone whose polygon takes part in
containment, so a robot whose `spec.zone` names a zone with no polygon, a frame-local polygon or
member zones is not guarded.

**What you will see.** The edge node estops a robot that leaves its assigned zone's polygon, with
the reason `zone-boundary-breach: robot <id> outside zone <zone>`.

**Instead.** When a robot is moved to another zone, change its `spec.zone`. Give every zone the
edge node is to guard a site-aligned polygon. See
[RFC-0001, Edge headless emergency stop](../rfcs/dist/RFC-0001-core-spec.md#safety-edge-headless-emergency-stop).

### An edge estop is invisible to the control plane

**Limitation.** RFC-0001 requires every adapter with an edge stream, whatever its contract
version, to hold each edge stop as a local latch at the robot, cleared only by a local reset made at
the robot, and a `1.4.0` adapter to report the latch. A report that it is set or unreadable records
the robot's end-state stop (robot stop table, *Abnormal events and the end-state stop*), held until
a person has checked the robot at the robot and
attested it, the stop is cleared by that attestation, and an operator releases the robot
(`swarmctl estop resume`); a local reset releases nothing.
The action an edge stop interrupted is never continued. At v0.4 none of this happens. A stop the
edge node issues goes to the robot's adapter and nowhere else. The control plane does not know of
it: `Robot.status.estopState` does not change, candidate filter 10 does not withhold the robot, and
the scheduler may assign it work. The simulation adapter as this repository deploys it (without
`--latch`) latches no edge stop, so it accepts and starts that work. Nothing in the control plane clears an edge stop: by design, only a
reset at the robot does.

**What you will see.** A robot the edge node stopped looks available in `kubectl get robots`, and
a task assigned to it is accepted and started: the robot moves again.

**Instead.** Treat an edge-stopped robot as stopped until it has been reset and confirmed safe at the
robot. To keep it out of dispatch, also stop it through the control plane (`swarmctl estop trigger` for
its zone, or the `swarmada.io/estop-triggered` annotation on the `Robot`): that stop is recorded,
withholds the robot, and is released in two steps: the clear (`swarmctl estop-clear` for a zone,
or removing the annotation from the `Robot`), then `swarmctl estop resume` for the robot. That clear
ends the control-plane stop only; it does not reset the edge stop. See
[RFC-0001, Edge headless emergency stop](../rfcs/dist/RFC-0001-core-spec.md#safety-edge-headless-emergency-stop).

### The edge node does not check which adapter serves a robot

**Limitation.** The edge node accepts any adapter whose client certificate its client CA
(`--tls-client-ca-file`) trusts, and does not check which robots that adapter serves. Any such
connection can send a position for any robot in the edge node's configuration: the edge node then
counts that robot's feed as present, and on a breach sends the robot's stop down that connection
and takes its acknowledgement as the confirmation.

**Instead.** Issue certificates under the edge node's client CA only to the adapters that serve
the robots in its zones. The edge node does not check a connection against the robot's
`spec.adapter`.

### An adapter that restarts can continue the work it held

**Limitation.** RFC-0001 has every adapter, whatever its contract version, start with no work: a
process that starts after a restart halts what it finds
running on a robot, and the control plane puts every robot it registers in the end state (robot
stop table, *Abnormal events and the end-state stop*; the next entry). An adapter also keeps the fencing token of
each assignment it accepted across its own restart, and never executes an assignment twice. At
v0.4 no in-tree adapter drops the work it held before a restart, and the simulation adapter and the
adapter template command the motion again on an identical re-delivery of the assignment they hold.

**What you will see.** After an adapter restart, a robot can continue, or start again, an action
that was under way before it.

**Instead.** After an adapter restarts, check at the robot what it is doing before you give it work,
and cancel any action it continued. See
[RFC-0001, The Stop Latch](../rfcs/dist/RFC-0001-core-spec.md#fleet-adapter-protocol-the-stop-latch).

### Every adapter restart or reconnect holds the adapter's robots until a check at the robot

**Limitation.** RFC-0001 puts a robot in one end state on every abnormal event: a fresh end-state
stop, which holds the robot whatever its `estopState` and resolves `Failed`, with its work paused,
until a person has checked the robot at the robot and recorded that check in an attestation (the
`swarmada.io/estop-attestation` annotation), which ends the stop, and the operator then releases it
(`swarmctl estop resume`). The release does not resume paused work: it stays bound until an
operator requeues or cancels it (row A6). Every register the
control plane accepts for a robot is such an event: after an adapter restarted or reconnected after
losing contact, and also the first after the robot is admitted, so commissioning a robot includes
one check at the robot.
So every restart or reconnect of an adapter, however short, holds every robot it registers `Failed`
until that check at the robot and its attestation. A restart of the manager holds every robot of
every adapter the same way (*A manager restart holds every robot until it is checked at the robot*). This is the price of failing safe: the control plane cannot tell
what a robot did while it could not reach it. At v0.4 no in-tree component does this: the control
plane triggers no stop on a register, a local-latch report or a reported stop, and a robot that
registers again is not stopped.

**What you will see.** At v0.4, a robot whose adapter restarted or reconnected carries on as before.
Once the end state is implemented, an adapter upgrade, or a network drop long enough to close its
streams, leaves each of its robots `Failed` until someone has checked it at the robot.

**Instead.** Plan adapter restarts and upgrades for a time when the robots can be checked at the
robot, attested and released, and keep the adapter's network path stable. See
[RFC-0001, Robot stop table](../rfcs/dist/RFC-0001-core-spec.md#safety-robot-stop-table),
*Abnormal events and the end-state stop*.

### Every robot class needs a local latch its adapter can read

**Limitation.** RFC-0001 has an adapter that cannot read a robot's local latch treat it as set: it
accepts no work for the robot and reports the latch unreadable, which records the robot's end-state
stop (robot stop table, *Abnormal events and the end-state stop*). A robot class whose local latch
no adapter can read is therefore never given work: the robot
stays under its end-state stop, which an attestation cannot end while the latch cannot be read. At v0.4 no in-tree adapter
reads a local latch, so this does not arise yet.

**Instead.** Before you adopt a robot class, check that its adapter can read the robot's local latch
at an interface the robot defines. See
[RFC-0001, Edge / Headless Emergency Stop](../rfcs/dist/RFC-0001-core-spec.md#safety-edge-headless-emergency-stop).

### A robot's adapter can be changed in place at v0.4

**Limitation.** RFC-0001 makes `Robot.spec.adapter` immutable: to move a robot to another adapter it
is deleted while `Idle` and under no stop, and admitted again (robot stop table, *The robot's
adapter*). At v0.4 the `Robot` webhook
admits a change of `spec.adapter`, and the earlier adapter, which nothing tells, can still drive the
robot.

**Instead.** Do not edit `spec.adapter`. To move a robot to another adapter, delete the `Robot` while it
is `Idle` and under no stop, and admit it again. See
[RFC-0001, Robot stop table](../rfcs/dist/RFC-0001-core-spec.md#safety-robot-stop-table),
*The robot's adapter*.

### A robot whose adapter has no SafetyStream can be given work

**Limitation.** RFC-0001 withholds a robot from dispatch while its adapter has no `SafetyStream`
registered, because no estop can reach it. The reference control plane does not check this: it
dispatches to any robot whose adapter is `Connected`, passed conformance and reports a supported
contract version, and `Connected` is decided by the `ControlStream` alone.

**What you will see.** The robot runs work normally and `swarmada_fleet_adapter_connected` reads
`1`. An estop to it resolves `Failed` at once, and the manager log records "no SafetyStream to
adapter <name>".

**Instead.** Treat a `Failed` estop with that message as an estop-path outage: stop the robot at
the robot, and keep it out of dispatch with a robot-scope stop (the `swarmada.io/estop-triggered`
annotation on the `Robot`), which dispatch honours whether or not the stop reached the robot. Then
check the adapter's `SafetyStream` dial. See
[RFC-0001, Scheduler](../rfcs/dist/RFC-0001-core-spec.md#control-plane-scheduler).

### A stop is kept on the Robot object only

**Limitation.** A robot's estop state, its `Error` phase and a `Failed` component are recorded
only in its `Robot` status. On an install with admission webhooks (`make deploy`, the Helm chart
by default), while a `Robot` holds one it cannot be deleted, by any identity, and cannot lose its
last finalizer except to the control plane, unless its namespace is being deleted. Offline
auto-removal leaves it in place. Its `swarmada.io/robot-id` cannot be changed or removed after it
is created. A robot held by its estop or a `Failed` component that never reports again cannot be
released: the resume check refuses an `Offline` robot, and only the robot's own report clears a
`Failed` component. It stays held, and its `Robot` stays, until it reports again. A hold by `Error`
alone ends differently: `Error` is the robot's own report, and a robot that falls silent is marked
`Offline`, which ends that hold and admits its delete. An `Offline` robot is not dispatched, and one
that still has the fault reports `Error` again when it is heard from.

Two `Robot`s created at the same moment with one `swarmada.io/robot-id` can both be admitted: the
uniqueness check lists and then admits, and two creates that arrive together each pass it. Neither
receives work: dispatch reads the `Robot` list uncached and withholds every `Robot` whose robot-id
another also carries, and each carries a `RobotIDConflict` condition naming the others. Delete all
but one.

A robot created into a zone whose estop is in force, moved into one, or put under one by a change of
the zone tree is not dispatched: the zone's stop withholds every robot whose `status.currentZone` or
`spec.zone`, or an ancestor zone of either, is stopped, whatever the robot's own `estopState`. The
manager also sends it the zone's stop through the same confirmed path as the fan-out, so it reads
`Stopped` (or `Failed` if the stop is not confirmed); it leaves through the zone's clear and an
operator's `swarmctl estop resume`. A robot the stop has not reached when the zone is cleared is
never stopped by it: one created into the zone between the trigger's removal and the clear being
applied (a window of milliseconds), or one created while the manager is down and the zone is cleared
before the manager returns. Dispatch withholds it until the clear is applied, as it does every robot
in the zone; after that it reads `Normal` and receives work with no `swarmctl estop resume`.

Each robot a zone's or the namespace's stop reaches records that stop in `status.estopHeldBy`. The
record is removed by the clear of a stop on a carrier of the same kind and name (that stop, or the
stop of a zone or `SwarmadaConfig` deleted and created again under that name) or, when its carrier
is gone or has another UID, by a robot-scope stop and clear on the robot. While the record stands, the robot is withheld from
dispatch, an action it holds is paused, and `swarmctl estop resume` refuses it, wherever the robot
is now. A robot the zone stopped stays `Stopped` or `Failed` after it moves out, after its zone is
re-parented away from the stopped zone, after the stopped zone's
`estopPolicy.propagateToChildren` is set to `false`, and after its own position report places it in
another zone. The zone's clear reaches it wherever it is and moves it to `Resuming`; each robot then
needs its own `swarmctl estop resume`. A robot-scope stop and clear does not release a robot a
zone's stop holds while that stop stands: the robot stays `Stopped`, and its resume is refused
naming the zone. A robot that comes back into a zone stopped again after a clear is stopped by the
new stop, like any robot that arrives in a stopped zone.

A record whose zone or `SwarmadaConfig` no longer exists, or exists under another UID, holds the
robot until an admin releases it, or until a stop on a zone or `SwarmadaConfig` of the same name is
cleared: the record names its carrier by kind and name, so a carrier deleted and created again
under that name, stopped and then cleared, removes the older record with its own.
A zone that carries a stop, or whose stop's clear has not been applied yet, is not deleted by any
identity, and neither is an edit that takes zones out of the reach of its stop admitted: a re-parent
out from under it, its `propagateToChildren` set to `false`, or the delete of a zone below it. The
refusal names the clear; once the clear is applied, the delete or the edit is admitted. A record whose
zone no longer exists arises only where that refusal does not hold: on an install without admission
webhooks, where anyone RBAC lets delete a zone can delete a stopped one, and in a namespace that is
being deleted. The resume is refused, naming the missing stop. To release the robot, an
admin stops and clears it at robot scope, which removes every record of a stop whose carrier no
longer exists, then resumes it:
`kubectl annotate robot <name> swarmada.io/estop-triggered="<reason>" -n <namespace>`; once the
`Robot` carries a `swarmada.io/estop-processed` annotation,
`kubectl annotate robot <name> swarmada.io/estop-triggered- -n <namespace>`, which moves it to
`Resuming`; then `swarmctl estop resume <name> --reason "<text>" -n <namespace>`.

A robot created in a namespace whose estop is in force is held the same way: dispatch withholds
every robot in the namespace until the stop's clear is applied, and the manager sends the new robot the
namespace's stop through the confirmed path, so it reads `Stopped` (or `Failed` if the stop is not
confirmed). It leaves through the namespace's clear and an operator's `swarmctl estop resume`.
The exception is the zone's: a robot created between the trigger's removal and the clear being
applied, or created while the manager is down and the namespace is cleared before the manager
returns, is never stopped. It is withheld until the clear is applied, then reads `Normal` and
receives work with no `swarmctl estop resume`.

The `SwarmadaConfig` that carries a namespace's stop, or whose stop's clear has not yet been
applied, is not deleted, by any identity, except while its namespace is being deleted: the refusal
names the clear, `kubectl annotate swarmadaconfig <name> swarmada.io/estop-triggered- -n <namespace>`.
The check is its own webhook, `vswarmadaconfigdelete.swarmada.io`, with `failurePolicy: Ignore`, so
that this check does not hold a namespace delete through a manager outage (see *A namespace deleted
during a manager outage stays Terminating* for the checks that do). While no manager replica serves
the webhooks, the delete is admitted: each robot the stop reached keeps it, because it records it, and a robot
created in the namespace afterwards is not held. A `SwarmadaConfig`, a `FleetZone` or a `Robot` holds the
finalizer `swarmada.io/estop-in-force` from the moment its stop is triggered until the clear has been applied,
so a delete admitted during an outage does not complete before the manager returns, applies the stop and
clears it. An admin who removes that finalizer by hand ends the stop unapplied: the robots the stop had not
reached are never stopped.

Where the delete refusal does not hold, a robot deleted and created again comes back `Normal` and
dispatchable, with no clear, no resume and no audit record:

- On an install without admission webhooks (see *An install without admission webhooks checks
  nothing at admission*) nothing refuses the delete; Offline auto-removal still skips such a
  robot.
- While no manager replica serves the webhooks, the delete check fails open: it is its own
  webhook, `vrobotdelete.swarmada.io`, with `failurePolicy: Ignore`, so that a manager outage never
  blocks deleting a `Robot`. A held robot can be deleted during the outage. Its namespace can still
  wait for the manager: see *A namespace deleted during a manager outage stays Terminating*.
- An identity that can delete the webhook configuration or change the `Robot` CRD (a cluster
  administrator, or `swarmada:admin` bound cluster-wide: see *Write access to CRDs is outside the
  Robot status guard*) can remove the refusal.
- A namespace that is being deleted is exempt, and every robot in it goes with it.

**What you will see.** `delete robot` is refused with a message naming what the robot holds and
`swarmctl estop resume`. A robot left in place by auto-removal carries a `Warning` Event,
`AutoRemovalHeld`. After a zone or namespace clear, a robot the stop never reached shows no
`estopState`, or `Normal`, while the robots it reached show `Resuming`, except a robot another stop
still holds, which keeps its state.

**Instead.** Release a robot that still reports, then delete it. A robot that will never report
again can be removed only with its namespace; no command decommissions one robot, and none
records who removed it and why. On an install without webhooks, check
`estopState`, `phase` and `status.hardware` before deleting a robot. Do not create or admit
robots in a zone or namespace while you clear its stop, and do not clear it while the manager is
down.

### Automatic release after an estop clear is not in this draft

**Limitation.** After an estop clear, every robot the clear reaches that no other stop holds stays
in `Resuming`, withheld from dispatch, until an operator releases it with
`swarmctl estop resume <robot>`. A robot that its own trigger, a zone's stop or the namespace's
stop still holds keeps its state until the clear of the last stop that holds it, and then reaches
`Resuming`. Nothing releases a
robot automatically: not its telemetry, not its adapter reconnecting, not a timer. The release
is refused while the robot has sent no telemetry since the clear, while its latest report is not
yet written to `Robot.status`, while its own latest report puts it in `Error` or `Offline`, while
a hardware component is reported `Failed`, or while a capability that was Active when the stop was
taken is not Active. A capability that was not Active at the stop, one the robot no longer
declares, and one a `ZoneMaintenance` window has paused never hold the release: a paused
capability is not checked, even one that was Active at the stop. The checks read the robot's
reports, which trail the robot itself, so a release that passes them is still the operator's
judgement. The window and its checks hold only against writes that go through the control plane
and its admission webhooks: on an install without them (see *An install without admission
webhooks checks nothing at admission*), a principal allowed to write `robots/status` can set the
robot `Normal` directly, past the window and every refusal below.

**What you will see.** After `swarmctl estop-clear`, robots show `estopState: Resuming` and the
scheduler records `robot is in the post-clear estop resume window` for each of them until they
are released. A robot another stop still holds keeps its state and carries a `Warning` Event,
`EstopHeldByOwnTrigger` or `EstopHeldByWiderStop`, that names that stop. A refused `estop resume`
prints the reason and exits non-zero, and the robot stays `Resuming`.

**The refusal on a failed component or an inactive capability has no override.** Such a robot
stays `Resuming` until the robot reports the component repaired, or until the component or the
capability is removed from its declaration. To remove it, edit the robot's RobotClass
(`spec.hardware` for a component, `spec.baseCapabilities` for a capability), then apply the
class to its robots with `swarmctl robotclass rollout <class>`, which replaces each robot's
hardware and capability lists with the class's. Then run `swarmctl estop resume` again.

**Instead.** After every clear, inspect each robot and release it with `swarmctl estop resume`;
release several at once by naming them. See
[RFC-0001, Estop state machine](../rfcs/dist/RFC-0001-core-spec.md#safety-estop-state-machine).

### A zone's stop follows the zone tree, not the zone's polygon

**Limitation.** A zone's stop reaches the robots whose `status.currentZone` (or `spec.zone` while
that is empty) is the zone or a `spec.parentZone` descendant of it. `status.currentZone` is one
zone, chosen from every zone whose polygon contains the robot (`status.containingZones`); no stop
reads `status.containingZones`. A robot inside a stopped zone's polygon whose `status.currentZone`
names an overlapping zone outside that zone's tree is neither stopped nor withheld. The stop RFC-0001
defines reads no polygon: a zone's stop follows the zone tree (FZ-G6), and zones may overlap.
Nothing refuses a new leaf zone that overlaps a stopped zone, or a change to a stopped zone's
`spec.physicalBounds` (its polygon, its floor or its frame alignment). A robot the stop has already
reached keeps it whatever its later position reports say: it records the stop (see *A stop is kept
on the Robot object only*).

**What you will see.** The robot reads `Normal` and takes work while the stopped zone reads
`Triggered` and the robot's `status.containingZones` names that zone.

**Instead.** Keep zones that overlap inside one `spec.parentZone` tree, and do not edit a stopped
zone's geometry. When zones outside one tree overlap, stop each of them, or stop the namespace.

### A robot outside a stopped zone is outside that zone's stop

**Limitation.** A zone's stop follows the robots that are in the zone, as RFC-0001 reads
membership: the robots whose recorded zone is the zone or one below it, when the stop is issued,
and any robot that enters the zone while the stop is in force. The control plane stops a robot that
enters a stopped zone, on its arrival and on a timer, so for a short time after it arrives the robot
is held only by the refusals of its zone edits (see *A zone's stop holds zone edits and a robot's
recorded zone until it is applied*). A robot that is outside the zone, and records no stop of that
zone, is outside it. The stop does not follow a robot that leaves, and does not reach one that is
on its way in. A robot that has recorded the stop keeps it wherever it goes.

**What you will see.** A robot driving towards a stopped zone reads `Normal` and takes work until
it is recorded as inside the zone.

**Instead.** For a guarantee that follows the robot, stop the robot, or stop the whole namespace (a
fleet stop on its `SwarmadaConfig`). Use a zone's stop for the robots that are in the zone.

### An update already under way on a robot continues when the robot is stopped

**Limitation.** A `FirmwareRollout` or a `ModelRollout` gives no update to a robot a stop holds,
or to a robot that is `Resuming`. A robot stopped after it was told to pull firmware has its
`swarmada.io/pending-firmware-*` annotations withdrawn, whoever wrote them, and a write of those
annotations on a robot a stop covers is refused to every identity but the control plane; a failed
robot under a stop is not pushed its model rollback. Two updates are not taken back. A firmware install the robot has
reported under way runs on: its batch entry's `updatePhase` reads `Installing`, `Verifying` or
`Rebooting`, or its `status.firmwareInstall.status` has read `Updating` since the dispatch. The
Robot Agent already holds the artifact, and nothing on the wire aborts an install. A model update
the adapter acknowledged before the stop runs on, because nothing on the wire cancels one.

**What you will see.** For firmware, a `Warning` Event `FirmwareUpdateUnderWayUnderStop` on the
rollout names the robot, and the robot stays in `status.currentBatch`. For a model, the robot stays
in `status.currentBatch` and its `status.installedModels[]` entry reads `Updating`.

**Instead.** Hold the rollout with `spec.desiredState: Paused` before you stop robots in an area it
is updating, so no further robot starts an install. A robot whose install is under way is committed
to it: rely on the hardware stop circuit while it finishes (see
[RFC-0001, Scope and responsibility boundary](../rfcs/dist/RFC-0001-core-spec.md#safety-scope-and-responsibility-boundary)).

### The ROS 2 adapter's retreat from an exhausted area can abort

**Limitation.** When an explore-floor segment of the ROS 2 adapter keeps out an area it could not explore, the robot drives back to its start mark first. A keep-out across the straight way to the mark can abort that drive; the adapter then stops commanding the robot and the segment ends failed.

### A robot that reconnects after its lease ran out can be given its action back

**Limitation.** RFC-0001 returns a `Revoking` action to `InProgress` only while its lease is not
provably dead (row A8 of the action table). The reference control plane does not
test the lease on that path. When a robot that is reachable again still records the action in
`status.assignedAction`, the control plane returns the action to `InProgress`, writes a new lease
horizon and sends `renew_lease`, even when the old horizon plus `clockSkewMarginSeconds` has passed.
After a control-plane restart it does the same for an `Assigned` or `InProgress` action whose robot
still records it, whatever its horizon. A robot that lost its lease has normally stopped on its own
lease by then, or, if a renewal was delayed in transit, may still be moving; the renewal does not
tell the two apart.

**What you will see.** After a disconnect longer than `leaseDurationSeconds` plus
`clockSkewMarginSeconds`, an action that read `Revoking` reads `InProgress` again, and
`status.leaseExpiresAt` moves forward. The robot may be at rest. If its adapter also accepts a late
renewal (see the Python SDK entry under Adapters), the renewal sets its timer again.

**Instead.** Treat a robot that returns from a long disconnect as one to inspect before it works. If
you know of the outage, hold the robot with a robot-scope stop (the `swarmada.io/estop-triggered`
annotation on the `Robot`) before its connection returns: while a stop holds a robot, a `Revoking`
action is not re-adopted and no renewal is sent. Then look at the robot and its payload, and have a
holder of `estop-clear` requeue or cancel the action, or clear the stop and resume the robot. Without
a stop, compare the action's phase with what the robot is doing once it is back, and cancel the
action with `swarmctl cancel action <name> --reason "<text>" -n <namespace>` if the robot is at rest.
Leave `SwarmadaConfig.spec.actionCancellation.onDisconnect` at its default `Never`: the action then
stays bound to its robot, and no other robot is given it. See
[RFC-0001, Action table](../rfcs/dist/RFC-0001-core-spec.md#safety-action-table).

### A robot probe cannot bound a metric whose healthy value is low

**Limitation.** Every threshold in `RobotProbe.spec.expectedMetrics` is a minimum: a metric fails
when it is missing or below its threshold. RFC-0001 names `inference_latency_ms` (an expected
maximum) and `output_stale` as standard model metrics, and the probe cannot bound either.

**What you will see.** A model with a latency above the limit you set, or an output that has stopped
changing, passes the probe. A latency below the value you set fails it.

**Instead.** Do not put `inference_latency_ms` or `output_stale` in a probe's thresholds. Watch them
from the model's own monitoring or the adapter's telemetry, and use the probe for metrics where a
higher value is the healthier one. See
[RFC-0001, RobotProbe](../rfcs/dist/RFC-0001-core-spec.md#robotprobe-the-verify-probe-commands).

## Scheduling

### A robot with no position gets no work in zones with site-aligned geometry

**Limitation.** In a zone with a site-aligned polygon (`frame.alignment: SiteAligned`, or a polygon
that declares no `frame`), a robot's `status.currentZone` is derived from its reported position. A
robot that reports no position, or a position outside every such polygon, has an empty
`currentZone`, and the scheduler does not assign zone-scoped work to it. In any other zone,
including one whose polygon is `FrameLocal` and a composite zone, a robot is placed by its
`spec.zone`.

**What you will see.** The action stays `Pending`, and the scheduler records the exclusion
`robot reports no status.currentZone, so its presence in "<zone>" cannot be confirmed`.

**Instead.** Make sure the adapter reports position, or use zones without geometry for robots that
cannot report it. See
[RFC-0001, Current-zone derivation](../rfcs/dist/RFC-0001-core-spec.md#fleetzone-zone-controller-current-zone-derivation).

### The TDE confirms zone capacity, not shared resources

**Limitation.** Before an assignment, the Traffic Deconfliction Engine checks zone capacity only.
The scheduler does not request shared resources (`FleetZone.spec.sharedResources[]`). A shared
resource is reserved only when an adapter asks for it, and such a request is granted or queued,
never denied. The `Manual` reservation policy behaves as `Priority`.

**What you will see.** An action is assigned whatever the state of the elevators, doors or
corridors declared in its zone.

**Instead.** Have the adapter reserve the shared resource before using it, and model hard
exclusivity as a zone with `maxConcurrentRobots: 1`. See
[RFC-0001, Traffic Deconfliction Engine](../rfcs/dist/RFC-0001-core-spec.md#tde).

### A shared-resource name is resolved across the namespace

**Limitation.** A reservation names a shared resource, and the Traffic Deconfliction Engine looks
that name up in every `FleetZone` of the namespace, not up the requesting robot's zone tree. A
robot in any zone can reserve a resource declared on any other zone of the namespace. Two zones
that declare the same name declare one resource: the zone whose name sorts first owns it, and
the other declaration's `capacity` and `reservationPolicy` are not used.

**What you will see.** Two `ChargingDock`s named `dock-01`, one in each wing, are one dock with
one queue. With the owning declaration's `capacity: 1`, one robot at a time charges, across both
wings.

**Instead.** Give every shared resource a name unique in its namespace, for example
`dock-2a-01` and `dock-2b-01`. See
[RFC-0001, SharedResource types and reservation policy](../rfcs/dist/RFC-0001-core-spec.md#fleetzone-sharedresource-types-and-reservation-policy).

### A hand edit of a zone's reservations is not refused

**Limitation.** No admission webhook checks a write to `FleetZone.status.reservations`: the
`FleetZone` webhook does not see the status subresource. The `swarmada:viewer`, `operator`,
`fleet-manager` and `admin` roles cannot write `FleetZone` status. An identity that can, such as
`system:masters` or another cluster administrator, the manager's service account or the edge node's,
is not refused: its edit is stored, and it stays until the Traffic Deconfliction Engine's next
write of that zone's status replaces it in part.

**Instead.** Do not edit `status.reservations` by hand; it is the engine's record. See
[RFC-0001, TDE normative invariants](../rfcs/dist/RFC-0001-core-spec.md#tde-normative-invariants).

### A shared-resource hold outlives a robot that never returns

**Limitation.** A hold an adapter took on a shared resource is kept, across manager restarts too,
for as long as the robot's `Robot` exists. Only the adapter's own release frees it. Holds do not
expire automatically.

**What you will see.** A lift or door stays held by a robot that has gone, and every other robot
that asks for it is queued.

**Instead.** Delete that robot's `Robot`: the hold is released at once and the resource passes to
the next robot in the queue, whose adapter is sent the grant. No restart is needed. A `Robot` that
holds a stop cannot be deleted (see *A stop is kept on the Robot object only*), so a robot held by
its estop or a `Failed` component that never reports again keeps its hold until it reports again or
its namespace is deleted. A robot held only by `Error` is marked `Offline` when it falls silent,
and its `Robot` can then be deleted. See
[RFC-0001, Normative Invariants](../rfcs/dist/RFC-0001-core-spec.md#tde-normative-invariants) (TDE-7).

### A composite zone is expanded only by a maintenance window

**Limitation.** A `FleetZone` with `spec.memberZones` is stored and returned, but only a
`ZoneMaintenance` zone scope expands it, all-or-nothing. The Scheduler, the TDE, estop scope,
`status.robotCount` and `status.isLeaf` do not read `spec.memberZones`, and no controller writes
`status.composition` or the `CompositionResolved` condition. Member existence, self-reference,
cycles and the depth bound are not checked at admission. The `ZoneMaintenance` chapter of RFC-0001
states its expansion: a `Zone` scope that names a composite resolves through `spec.memberZones` and
activates only once every member resolves. The Scheduler and TDE chapters do not state the
expansion rules the `FleetZone` chapter assigns them.

**What you will see.** A `FleetAction` that names a composite zone matches only a robot whose
`status.currentZone` is the composite itself, normally none, so it stays `Pending`. A composite
with no child zones reports `isLeaf: true`. An estop trigger on a composite is refused at admission.

**Instead.** Target an action at a leaf zone, or at a parent zone in the containment tree. Use a
composite for a maintenance window's scope only. See
[RFC-0001, FleetZone](../rfcs/dist/RFC-0001-core-spec.md#fleetzone).

### A FleetTask counts a member superseded in place as lost

**Limitation.** An action superseded in place (a same-type preemption the adapter reports as ended
`SUPERSEDED`) ends `Cancelled` with `status.cancellationReason: SupersededByDirective`. A
`FleetTask` counts any `Cancelled` member as lost, whatever its `cancellationReason`, and applies
its `failurePolicy`. RFC-0001 does not say whether a superseded member should count as lost,
or what a compensation should do for one.

**What you will see.** One member superseded in place fails the task: `FailFast` cancels the other
members, `Compensate` rolls back the members that succeeded, and `ContinueOthers` ends `Failed` once
the others finish. A preemption that requeues its victim (`Preempted`, then `Pending`) does not do
this.

**Instead.** Give a task's actions `priority: High` or `Critical`: preemption takes a robot only
from a `Normal` or `Low` action. See
[RFC-0001, FleetTask](../rfcs/dist/RFC-0001-core-spec.md#fleettask).

### A flow member with the default onFailure runs again after its FleetTask failed

**Limitation.** A member of a `FleetTask` with `spec.flow` whose action keeps the default
`onFailure: Requeue` is put back to `Pending` after it fails, `backoffSeconds` later, although its
failure has already ended the `FleetTask` `Failed`. The requeue does not read the owning task, so
the action is eligible for dispatch again.

**What you will see.** The `FleetTask` reads `Failed` while one of its member actions is `Pending`,
and then runs on a robot.

**Instead.** Set `onFailure` to `Alert` or `Abandon` on every flow member's action: each leaves a
failed action `Failed`. See
[RFC-0001, FleetTask](../rfcs/dist/RFC-0001-core-spec.md#fleettask).

### Priority does not order waiting actions

**Limitation.** `FleetAction.spec.priority` takes effect in preemption only. The scheduler does not
order `Pending` actions by band: when a robot becomes free, the `Pending` action that is reconciled
first takes it, whether it is `Critical` or `Low`.

**What you will see.** A `High` or `Critical` action can stay `Pending` while a `Normal` or `Low`
action created after it is assigned the robot that became free. A `Critical` or `High` action still
preempts `Normal` or `Low` work when it is the only eligible robot's.

**Instead.** Keep urgent work from competing with bulk work for the same robots: give it its own zone
or robot class, or hold bulk work back until the urgent actions are assigned. See
[RFC-0001, Priority Semantics](../rfcs/dist/RFC-0001-core-spec.md#fleetaction-priority-semantics).

### Event-driven flow arms do not fire

**Limitation.** A `FleetTask` flow that waits on a semantic event, such as a `select` on an event,
a `loop.until` event condition or a `fromMember` event binding, never sees one. Nothing in the
control plane fills `FleetAction.status.events[]`. A `select.cases[].when` CEL clause is not
evaluated either.

**What you will see.** Admission warnings on the write. At run time, a `select` on an event waits
while its producer runs and settles `Failed` once the producer settles. An event-only `loop.until`
is bounded only by `maxIterations` or `maxDurationSeconds`. A `when` case settles `Failed`, quoting
the clause.

**Instead.** Branch on member outcomes and `dependsOn`, and bound every loop by count or duration.
See [RFC-0001, The FlowNode tree](../rfcs/dist/RFC-0001-core-spec.md#fleettask-the-flownode-tree).

### A loop cannot end on what a robot action reports

**Limitation.** A `loop.until` condition reads a member's `status.result.values` or its
`status.events[]`, and an adapter's report reaches neither: it carries a message and a progress
percentage only. So a loop over robot work can't stop on the robot's own outcome, such as "no
frontier left".

**What you will see.** The loop runs every iteration its count or duration allows.

**Instead.** Bound the loop by `times`, `maxIterations` or `maxDurationSeconds`, and have the
adapter end an iteration at once when there is nothing left to do. See
[Map a floor, confirm its zones, work in them](floor-mapping.md#1-explore).

### Deleting a running FleetAction holds its robot until the record is released

**Limitation.** Nothing refuses `kubectl delete fleetaction` on an action that is `Assigned`,
`InProgress` or `Revoking`, unless a stop covers its robot: then only a holder of `estop-clear` on the
stop may delete it, and the action is kept until the stop no longer holds the robot. The `FleetAction` admission check covers create and update only, and a
`FleetAction` carries no finalizer, so the record goes at once. The control plane stops renewing the
robot's assignment lease, so a conforming adapter stops it when the lease lapses (30 seconds by
default). The robot is not freed at once: its `status.assignedAction` still names the deleted action,
because it may still be running it. The control plane sends the robot `cancel_action` for the deleted
action and releases the record when the adapter acknowledges it, or, failing that, once the longest
lease the configuration admits (300 seconds) plus the clock-skew margin has passed since it saw the
action gone. A manager restart starts that wait again. While a stop covers the robot, the record is
kept until the stop is cleared.

**What you will see.** `kubectl delete` prints `deleted`. Until the record is released, `kubectl get
robot` shows the robot `InProgress`, its `ACTION` column shows the name of the deleted action, and a
new `FleetAction` that only that robot could run stays `Pending`, its message counting the robot
`not Idle (filter 1)`. The robot's delete is refused in that time, naming the action. A `FleetAction` created again under the deleted one's name, by hand or by
its `FleetTask`, which creates a deleted member again, is given to no robot while the robot that ran the
deleted one still records it: the control plane sends that robot `cancel_action`, and the action waits for
the acknowledgement or, failing that, for the longest lease the configuration admits (300 seconds) plus the
clock-skew margin after the action is created, since the deleted action's own lease horizon is gone with
it. Its message names the robot. See *An action is given to a robot again only once its previous
executor is provably off it*.
A `FleetAction` created under a different name is not held this way: if it carries the same work, it
can be given to another robot at once, while the robot that ran the deleted action may still be running
it until that action's lease lapses.

**Instead.** Cancel a running action and wait for `Cancelled` before you delete it:
`swarmctl cancel <name> --reason "<text>" -n <namespace>`. Keep the robot's adapter connected, so that
it acknowledges the cancel and the robot is freed at once.

## Adapters

### A deleted robot is not announced again while its adapter stays connected

**Limitation.** After a `Robot` is deleted, the control plane drops that robot's messages from a
connected adapter without answering them. An adapter that announces a robot only when its
registration is refused, as the simulation adapter does, does not announce it again, and no
`DiscoveredRobot` appears until the adapter reconnects.

**Instead.** Restart the adapter before admitting the robot again, as the
[samples README's first run](../config/samples/README.md#the-secure-first-run-first-run) does.

### One live stream per Fleet Adapter identity

**Limitation.** One adapter identity, the client certificate's `FleetAdapter`, is served on one
`ControlStream` and one `SafetyStream`. A second connection under the same identity replaces the
first.

**What you will see.** The earlier stream closes with gRPC `ABORTED` and the message
`superseded: a newer ControlStream for adapter "<name>" in namespace "<ns>" was accepted`, and the
`FleetAdapter` gets a `ControlStreamSuperseded` Warning Event. Two processes sharing one identity
displace each other in turn. Under RFC-0001 each displacement is also a register again, an abnormal
event, so every robot of that identity is held again each time, and a process whose streams were
replaced may still drive a robot it held work for. RFC-0001 requires one live process per adapter
identity.

**Instead.** Run one process per adapter identity. For more capacity, give each process its own
`FleetAdapter` and client certificate. See
[RFC-0001, Connection lifecycle](../rfcs/dist/RFC-0001-core-spec.md#fleet-adapter-protocol-connection-lifecycle).

### A maintenance window does not requeue paused work

**Limitation.** A `ZoneMaintenance` window in `Immediate` mode, or in `Graceful` mode past its drain
timeout, requeues the actions of the robots it covers, except a `Paused` action and the action of a
robot a stop holds. A held action is not requeued by the window: an action an estop paused, and
one paused by `spec.desiredState: Paused`, stay with their robot until an operator requeues or
cancels them. A requeue request that was waiting on a running action when a stop reached its robot
is dropped, and the action is paused. One that was waiting on a `Paused` action is held while the stop
stands. RFC-0001 holds such a requeue until the operator release (S6). At v0.4 the reference control plane carries it out at the clear, while the robot is still `Resuming`.

**What you will see.** The window stays `Active` and lists the robot under
`status.windingDownRobots`. The action reads `Paused` (or `Revoking`, with a message that starts
`held by estop`). An action that was `InProgress` stays bound to its robot; one that was `Assigned` is
released from its robot once the robot's stop is confirmed, and the robot keeps its record of it
(`status.assignedAction`) while the stop covers it. A dropped request leaves a `Warning` Event
`RequeueOvertakenByEstop` on the action.

**Instead.** Inspect the work, then requeue the action,
`kubectl annotate fleetaction <name> swarmada.io/requeue-requested="<reason>" -n <namespace>`, or
cancel it, `swarmctl cancel action <name> --reason "<text>" -n <namespace>`.
While an emergency stop covers a robot the action is bound to or recorded on, the requeue needs
`estop-clear` on that stop, which `swarmada:operator` and `swarmada:fleet-manager` do not hold: requeue it once
the robot is released (`swarmctl estop resume`), or have a holder of `estop-clear`
(`swarmada:admin`) requeue it. RFC-0001 requires that holder after the release too (`estop-clear` on the robot); v0.4 does not check it then.

### A zone's stop holds zone edits and a robot's recorded zone until it is applied

**Limitation.** From the moment a zone carries `swarmada.io/estop-triggered` until it carries
`swarmada.io/estop-processed`, a `spec.parentZone` change that takes a zone out from under it, a
`spec.zone` change that takes a robot out of it, a change of `spec.estopPolicy.propagateToChildren` on
that zone or on a zone below it that its stop reaches, and the delete of a robot the stop reaches (or
the removal of the last finalizer of one deleted before the stop) are refused, and a robot's recorded zone (`status.currentZone`) is not moved out of it on a position
report; a robot that has reported no position is read as being in its `spec.zone`. The delete of any
robot in a namespace whose `SwarmadaConfig` carries a stop not yet applied is refused the same way.
A robot that has entered a zone whose stop is applied, and that does not yet record that stop in
`status.estopHeldBy`, is held the same way: its `spec.zone` is not moved out of the stop's reach and
its recorded zone is not moved out on a position report until the stop reaches it, which the control
plane does on the robot's arrival and on a timer. A robot that records a stop moves freely; the record
travels with it. A re-parent that would give any zone below the edited zone more than 64 ancestors
is refused. Admission judges each edit against the tree it reads, so two re-parents admitted at the
same moment can each stay within that depth and together exceed it. A zone stop then stops every robot
whose zone chain it cannot walk, records the stop on it, and names it in an `EstopScopeUndecided`
Warning Event on the zone; that stop's clear releases it. While a zone stop is issued or in force in
the namespace, an edit to that part of the tree is refused; with none, an edit that returns the tree
within the depth is admitted.
The robot delete check has `failurePolicy: Ignore`, as the `SwarmadaConfig` delete check does: while
no manager replica serves, a robot under a pending stop can be deleted. The recorded zone can lag the
reported position for as long as the stop takes to apply, and for as long as it stays unapplied when
no control plane is running. The edits stay refused in that case too. Both fail closed.

**What you will see.** The edit or delete is refused with a message that names the stop and says to
retry once it is applied.

A zone or namespace stop that is cleared before the control plane has begun to issue it to robots
stopped nothing, and leaves no entry in the Swarmada audit log: neither `ESTOP_TRIGGERED` nor
`ESTOP_CLEARED` is written. A stop cleared after the control plane began to issue it, but before the
zone or the `SwarmadaConfig` carries `swarmada.io/estop-processed` (the issue waits for each robot's
acknowledgement), stopped each robot it reached: both entries are written, and each of those robots
goes to `Resuming` and is released with `swarmctl estop resume`. The Kubernetes audit log, where it is enabled, records both annotation writes and who made
them. `status.currentZone` keeps the stopped zone while `status.containingZones`
follows the robot.

**Instead.** Retry the edit or delete once the zone, or the `SwarmadaConfig`, carries
`swarmada.io/estop-processed`. A robot the stop reached then holds it, and its delete stays refused
until it is released. The robot's next
position report after that records where it is.

### A deposed leader is fenced once the adapter has seen the new leader's term

**Limitation.** A control-plane process sends on an adapter's streams only while it holds leadership, and closes them when it loses it. A process that stalls past its lease cannot close them in time. Every `Command` and `EstopClear` carries the term of the leader that sends it, and
the adapter rejects a `Command` other than a stop, and an `EstopClear`, whose term is lower than the
highest it has seen for the robot. It always accepts a stop. The term is what the adapter compares,
so it protects a robot only after a message of the new leader's has reached the adapter. Where the new
leader cannot reach the adapter's streams, a deposed leader that still holds them can send an
`AssignAction` the adapter accepts, because no lower term has been seen. The new leader's stop then
reaches the adapter when a `SafetyStream` opens to it. The reference control plane sets the term on
every `Command` and every `EstopClear`. The Python SDK reads no term, and nor does the
simulation adapter as this repository deploys it (without `--latch`).

**What you will see.** Nothing at runtime. A robot whose adapter still holds a connection to a leader
that lost its lease can be given an assignment in that window.

**Instead.** Run one manager replica, as
[One manager replica while ControlStream is enabled](#one-manager-replica-while-controlstream-is-enabled)
requires: a change of leader then closes every stream. Treat a robot whose adapter stays connected
across a change of leader as held until it has registered again. For draft 4, a single manager replica is the only supported safety configuration; multi-replica high availability is not supported for safety.

### A manager can take 30 seconds to stop while adapters are connected

**Limitation.** On a `SIGTERM`, the manager stops its gRPC server before it gives up leadership.
The server waits for the open adapter streams to end, and those streams close only when the manager
gives up leadership. So the server waits until the manager's shutdown timeout, 30 seconds, ends. A
manager that loses its lease does not wait: its streams close at once.

**What you will see.** A manager Pod that you delete, or that a rollout replaces, takes up to 30
seconds to exit while an adapter holds a stream open. The streams close at the end of that time, and
the adapters connect to the next leader. A robot under a stop stays under it.

**Instead.** Allow 30 seconds for each manager restart in a rollout. Nothing else changes: the stops
and the work they hold are kept.

### An adapter outside the contract window cannot bring robots under management

**Limitation.** The control plane implements contract `1.4.0` and accepts `>=1.3.0 <1.5.0`, N and
N-1 within one major version. A later minor release moves the window, and it is not widened.
An adapter that declares `1.2.0` or below, such as the three external reference adapters at
`1.0.0`, can no longer bring robots under management.

**What you will see.** Registration and discovery are refused with
`REGISTRATION_REJECTION_VERSION_MISMATCH` and a message starting `VERSION_MISMATCH:` that names the
accepted range. The adapter stays connected, and telemetry, heartbeat and emergency stop are
unaffected.

**Instead.** Move the adapter to a build that declares a contract version inside the range, run
its conformance suite at that version, and replace its report. See
[Contract versions and re-qualification](adapters.md#contract-versions-and-re-qualification).

### Conformance does not check that an aerial robot's stop is a hover or a land

**Limitation.** RFC-0001 defines an aerial robot's safe state as a controlled hover or a landing.
The conformance harness drives an adapter over a simulated link that confirms any stop command, so
it grades the stop as confirmed whatever command the adapter sent, a return-to-launch included.

**What you will see.** A passing stop check (`C4.2`, the `C5` rows, `C16`) for an aerial adapter
whose stop flies the robot across the site.

**Instead.** Before admitting an aerial adapter, read which command its stop sends, and confirm on
its own simulator that the robot holds position or lands. See
[RFC-0001, Safety](../rfcs/dist/RFC-0001-core-spec.md#safety).

### Conformance does not check that an adapter refuses a command for a robot it does not serve

**Limitation.** One ControlStream carries the commands for every `Robot` bound to an adapter, and
each command names its robot in `robot_id`. No conformance check sends an adapter a command or an
estop addressed to a robot it does not serve, so an adapter that carries out such a command on its
own robot passes. The simulation adapter and the adapter template refuse them.

**What you will see.** A passing conformance report for an adapter that moves its robot on an
assignment addressed to another `Robot` bound to the same adapter. When an adapter declines an
assignment, a `Warning` Event `AssignmentDeclined` on the `FleetAction` and on the `Robot` names the
robot and the adapter's reason; Events expire (one hour by default, the API server's `--event-ttl`),
and the `FleetAction` keeps no record of the decline once another robot takes it. An
`ActionStatusUpdate` with no `fencing_token` is refused, and a `Warning` Event `AdapterOmitsFencingToken`
on the `FleetAdapter` says so for every refused terminal report and for the first refused running report
on a connection.

**Instead.** Before binding more than one `Robot` to an adapter, confirm that the adapter compares
`Command.robot_id` with the robot it serves and refuses a command addressed to any other.

### A cluster administrator can lower a namespace's fencing-token store

**Limitation.** An action is identified on the wire by its name and its token, and name-plus-token
is only as strong as the generation store's monotonicity. Every fencing token in a namespace comes
from one counter, kept in a Lease named
`fencing-tokens.<namespace>` in the control plane's own namespace. The namespace's own users can't write
it, but anyone who may write Leases in the control plane's namespace can, and so can whoever deletes the
Lease. The control plane refuses a counter below the highest token an existing action holds, or below
one it has issued since it last started. A counter lowered further after a restart, below the token of
an action that was deleted, gives an action created again under that name a token the deleted one used,
and a late report from the deleted action's run can then end the new one.

**What you will see.** A `Warning` Event `GenerationStoreBehind` on an action and its `FleetTask` when
the control plane caught a lowered counter and raised it. Nothing when the counter was lowered below
what the control plane can see.

**Instead.** Grant Lease writes in the control plane's namespace to the manager alone, and don't edit
or delete its `fencing-tokens.*` Leases.

### A robot stays bound after a stop its adapter could not confirm

**Limitation.** An adapter on a platform that can't witness rest answers a `cancel_action` with
`CANCEL_DISPOSITION_STOP_UNCONFIRMED`. That answer doesn't prove the robot stopped, so the robot stays
bound to its action until the action's lease has provably expired: for up to one lease duration plus
the clock-skew margin. On a requeue under no stop the action fails at once and keeps the robot; under a stop an operator's
requeue or cancel moves or ends the work on that decision (row A2), whatever the adapter answers. An operator cancel, a
completion timeout, a `Returning` hold and a preemption handover wait for the same expiry and then end
as they always do. An adapter that reports FAILED with the message `StopUnconfirmed` holds its own robot
bound for up to one lease duration plus the clock-skew margin, and affects no other robot or action.
A directive supersession that cancels the running action first is specified to end the same way,
`Failed` with the robot bound until the lease has provably expired, but no control-plane path issues
that cancel, so nothing runs it.

**What you will see.** The action reads `Failed` with `failureReason: StopUnconfirmed` and a Warning
Event `ActionStopUnconfirmed`. The robot gets no new work until a Normal Event
`ActionStopUnconfirmedReleased` reports the release. A cancelled action reads "awaiting confirmed stop"
until then. While a stop holds the robot, the expiry does not release it: the action stays `Failed`
with its robot, its message starts "awaiting operator decision", and a `Warning` Event
`ActionAwaitingOperatorDecision` goes on the action. A requeue by a holder of `estop-clear` on the
stop, or a cancel, then moves or ends it.

**Instead.** Plan for the hold on such a platform. Its length is
`SwarmadaConfig.spec.scheduling.leaseDurationSeconds` plus `clockSkewMarginSeconds`.

### An action is given to a robot again only once its previous executor is provably off it

**Limitation.** A robot that records an action in `status.assignedAction` may still be running it
when the action is made dispatchable again: after `spec.desiredState: Paused` on an `Assigned`
action, whose binding is released as the state table says; after a `FleetTask` creates a deleted
member again under its name; after a robot is deleted and created again. No robot is given the action
until that robot is provably off it: its adapter acknowledged the `cancel_action` the control plane
sends it, or the action's lease is provably dead. When the control plane does not know the lease
horizon (the binding was released, or the deleted action took its horizon with it), it waits the
longest lease the configuration admits, `300` seconds, plus `clockSkewMarginSeconds`, counted from the
moment the action re-entered `Pending` or was created. An adapter that acknowledges the cancel ends
the wait at once.

**What you will see.** The action reads `Pending` with a message naming the robot that still
records it and the time until which it waits. The robot keeps its `ACTION` column and takes no new
work. A `Robot.status.assignedAction` write by any identity but the control plane is refused.

A robot that records an action is not deleted, and its last finalizer is not removed, until the same
proof holds: deleted, it would take the record with it, and the action, or one created again under its
name, would go to another robot at once. The refusal names the action and the time at which its lease
is provably dead (its horizon plus `60` seconds, the widest clock-skew margin the configuration
admits), or says that the adapter must acknowledge the cancel when the action is paused and its
horizon was released. The same holds when the recorded action has been deleted: deleting it does not
prove the robot stopped, and the refusal names the action until the control plane releases the record
(*Deleting a running FleetAction holds its robot until the record is released*). The robot delete
check has `failurePolicy: Ignore`: while no manager replica serves, such a robot can still be
deleted.

**Instead.** Keep adapters connected so that the cancel is acknowledged; the wait is then the
round trip. Lowering `leaseDurationSeconds` does not shorten a wait already in progress. To delete a
robot that records an action, cancel or requeue the action first and wait for the record to clear.

### A robot keeps its record of work a stop paused until the stop is cleared

**Limitation.** While a stop covers a robot, the robot keeps its record of an action in
`status.assignedAction`, even when it is provably off the action: after the stop pauses an `Assigned`
action, after the action is cancelled, and after a `FleetTask` member is deleted. The record is what
ties the work to the stop. No other robot is given the action, or a member created again under its
name, until the stop is cleared or a requeue by a holder of `estop-clear` on that stop moves it. Once
the clear is applied, the control plane sends the robot a `cancel_action` and releases the record when
the adapter acknowledges it. A cancelled action deleted before the clear keeps the record until the
clear, and is then released as for a deleted running action (*Deleting a running FleetAction holds
its robot until the record is released*).

**What you will see.** The robot's `ACTION` column keeps the action while the stop stands. A member
created again reads `Pending` with a message naming the robot and the stop. A paused action whose
robot's adapter does not acknowledge the cancel after the clear keeps the record until the action is
requeued or cancelled.

**Instead.** Clear the stop, or have a holder of `estop-clear` on it requeue the work.

### After a stop, an operator moves the work; nothing moves it automatically

**Limitation.** RFC-0001 never moves work assigned to a robot under a stop to another robot on its
own. The work stays with the robot, `Paused` (or held `Revoking`, `Preempted` or `Failed`), until the
robot itself reports the work ended, or an operator decides: a holder of `estop-clear` on the stop,
or on the robot after the release, requeues it, or an operator cancels it (invariant I1). No expired lease, adapter acknowledgement or
refused assignment moves it. Under the stop, the requeue is carried out on that decision once the robot's stop is
confirmed or the lease horizon has passed, so that no assignment still in flight can
start the work, and after the release once the robot is proven off the action (row A2); before deciding, the operator establishes that the robot is off the work: at the robot when its stop is not confirmed
(`estopState` `Stopping` or `Failed`) or when it is held after an adapter restart or reconnect. Work
nobody decides about stays with its robot for as long as the stop stands, and RFC-0001 keeps it
there after the release too: no timeout, maintenance window, preemption or `FleetTask` sends it a
cancel. At v0.4 a requeue or a cancel of held work waits for the adapter's acknowledgement or the
lease instead of the decision alone, and the operator release ends the hold: held `Revoking`,
`Preempted` or `Failed` work whose lease horizon passed during the stop returns to `Pending`, or
frees its robot, with no report from the robot and no decision. After the release the completion
timeout, a maintenance window's requeue, a preemption and a `FleetTask`'s failure policy or cancel
also send held work a cancel, and it moves on the adapter's acknowledgement or on a lease horizon that
passed under the hold. Requeue or cancel held
work, or check it at the robot, before you release the robot. Any identity with
`cancel` on the action can cancel held work (*A cancel of work a stop holds needs only `cancel` at v0.4*).

**What you will see.** Held work reads `Paused` with its robot, a message that starts "awaiting
operator decision", and a `Warning` Event `ActionAwaitingOperatorDecision`.

**Instead.** After a stop, inspect the robot and its payload, then have a holder of `estop-clear` on the
stop, or on the robot after the release, requeue the work to another robot, or cancel it. Apart from the robot's own report, nothing else
will under RFC-0001. At v0.4, decide before you release the robot.

### Work held by a stop is tied to its name and its robot

**Limitation.** While a stop covers the robot that holds an action, the action is tied to the robot by
its name. Work you create under a new name is new work: if it repeats what the held action was doing,
nothing stops it from running on another robot while the first robot still holds the original. A
`FleetTask` you delete while one of its actions is held keeps that action, `Terminating`, until the stop
no longer holds the robot; the task does not create it again. Deleting or cancelling held work counts as moving it and
needs `estop-clear` on the stop, or on the robot once it is cleared and released; at v0.4 the delete check reads the stops in force, so it admits
a delete of held work while the robot is `Resuming` and after its release.

**What you will see.** A held action you or the task deleted reads `Terminating`, with a message that
starts "awaiting operator decision (estop-clear on". Creating an action under its name fails with
`already exists`.

**Instead.** To move held work, have a holder of `estop-clear` on the stop, or on the robot after the release, requeue it under its own name.
Do not copy held work under a new name.

### A failure a robot reports while a stop covers it pauses the action

**Limitation.** A stop often ends an action on the robot, and the adapter reports it failed: a
teleoperation gate the stop closed, a motion the stop aborted. While a stop covers the robot, the
control plane reads that report as the stop's outcome, not as a failure: the action is `Paused` with
its robot, `onFailure` does not retry it on another robot, and its `FleetTask`'s failure policy does not
fire. The report is kept only in the status message. A report that reaches the control plane before
the stop does, or a robot-side stop the control plane had not recorded when the report came, is an ordinary failure:
`onFailure` applies.

**What you will see.** The action reads `Paused` with no `failureReason`, and a message that starts
"paused by estop — the robot reported the action failed while the emergency stop of" and quotes the
adapter. A `Warning` Event `ActionFailedUnderEstop` goes on the action and its `FleetTask`.

**Instead.** Inspect the work, then cancel the action, or the task, or have a holder of `estop-clear`
requeue it.

### A requeue asked before a stop waits for the operator release

**Limitation.** A requeue of a `Paused` action that is still waiting when a stop reaches its robot, and of
started `Revoking` work that waits for an operator under a stop its robot never confirmed, is held while
the stop stands, whoever asked it, and carried out after the operator release. The control
plane tells the two cases apart by when it first handles the request, so a request it first handles
a moment after a stop reached the robot (within the time a controller takes to see a change, milliseconds)
is read as asked under the stop, which only a holder of `estop-clear` may do. Under a stop nothing moves the
work automatically (*After a stop, an operator moves the work; nothing moves it automatically*).

**What you will see.** The action reads `Paused` with a message that starts "requeue requested before
the emergency stop of", and a `Warning` Event `RequeueHeldByEstop` goes on the action and its
`FleetTask`.

RFC-0001 holds a cancel asked before the stop the same way (`Warning` Event `CancelHeldByEstop`): an
`estop-clear` holder's decision during the hold moves the work. A cancel an operator asked earlier is
carried out after the operator release, and one no operator asked, such as a `FleetTask`'s fan-out,
never takes effect on held work (row A6). At v0.4 the reference control plane holds no cancel
(*A cancel of work a stop holds needs only `cancel` at v0.4*).

**Instead.** Clear the stop, or have a holder of `estop-clear` on it withdraw the request and ask again.

### A requeue or a cancel of paused work waits for the adapter, or for the lease, even when the robot reads `Stopped`

**Limitation.** RFC-0001 carries out an operator's requeue of work under a stop once the robot's
stop is confirmed or the lease horizon has passed (row A2), and an operator's cancel
once the stop is confirmed or the lease horizon has passed (row A4), whatever the adapter answers to
the cancel; a cancel asked before the stop waits until the operator release, and a cancel a
ServiceAccount asks, such as a `FleetTask`'s fan-out, never takes effect on held work (row A6). At v0.4 the reference control plane does not: a requeue or a cancel of a
`Paused` action bound to its robot (one a stop paused, or one that was `InProgress` when a
`spec.desiredState: Paused` hold paused it) sends
`cancel_action` and waits for the adapter's acknowledgement, without
`CANCEL_DISPOSITION_STOP_UNCONFIRMED`, or for the lease to be provably dead. A confirmed stop
(`Stopped`) is not enough: it says the robot is at rest, not that it has dropped the action. While a
requeue or a cancel waits, the lease is no longer renewed, and a `Paused` action's lease is never renewed
to the robot, so an adapter that stays connected and never answers delays the requeue or the cancel by
up to `leaseDurationSeconds` plus `clockSkewMarginSeconds`. An action that a `spec.desiredState: Paused` hold
paused while `Assigned` keeps only the robot's record: a requeue then waits for the acknowledgement, or for `300` seconds plus
`clockSkewMarginSeconds` from the requeue.

**What you will see.** The action reads `Paused` with `status.assignedRobot` set and the message
"requeue requested — awaiting confirmed stop" or "cancel requested — awaiting confirmed stop". No other
robot is given the action until then.

**Instead.** Wait out the lease, or restore the adapter so that it acknowledges the cancel.

### A cancel of work a stop holds needs only `cancel` at v0.4

**Limitation.** RFC-0001 lets only a holder of `estop-clear` on the stop, or on the robot after the release, cancel work that a stop
holds or held, `Resuming` included (row A4). It holds a cancel asked before the stop until the operator
release, and never carries out on held work a cancel a ServiceAccount asks, such as a `FleetTask`'s
fan-out (row A6). At v0.4 the
reference control plane does not. Its admission webhook checks `cancel` on the action, as on any
action. It records no asker and holds no cancel. The control plane carries a cancel out once the
cancel's proof holds. For a `Paused` action that proof is the adapter's acknowledgement or the
lease's death. For a `Failed` (`StopUnconfirmed`) action it is the lease's death, while a stop holds
the robot too.

**What you will see.** A cancel from any identity with `cancel` on the action, a `FleetTask`'s
fan-out included, ends held work once that proof holds. The action reads `Cancelled` and releases
its robot. No `CancelHeldByEstop` Event is emitted.

**Instead.** Grant `cancel` on `FleetAction` and `FleetTask` objects only to the people who may
decide about held work. Before you cancel held work, check the robot as you would before a requeue
(*After a stop, an operator moves the work; nothing moves it automatically*).

### An adapter that predates `custom_type` reads the verb from the payload only

**Limitation.** The control plane sends `FleetAction.spec.customType` on the assignment as
`AssignAction.custom_type`, and on `ValidateAction`. An adapter built before that field ignores
it and takes a `Custom` action's verb from the payload, if the payload names one.

**What you will see.** A `Custom` action that names its verb only in `spec.customType` reaches
such an adapter with no verb. Depending on the adapter, it is refused or run as a `Custom`
action that names no verb.

**Instead.** While such an adapter serves the action, name the verb in the payload as well, as
`operation` with the same value as `spec.customType`. An adapter that reads `custom_type` accepts
the two when they agree and refuses them when they differ.

### Conformance and gate reports check fewer skips than they record

**Limitation.** Two reports that read skips check only some of them:

- The registry's skip-cause rows (`CE0.1`, `CP0.1` and `RB0.2`) can contradict only some kinds
  of skip cause. The others are recorded, not checked, and the row says how many of each.
- `make gate-integrity` does not see a module-level `pytestmark = pytest.mark.skipif(...)`, a
  `pytest.importorskip(...)`, or a skip written as an early return. A test skipped one of those
  ways is not named or counted, and the gate still passes.

**What you will see.** A skip-cause row prints, for example, `0 of 10 recorded skip cause(s) are
of a kind this run can contradict, and none was contradicted. The other 10 are recorded, not
checked: [...]`. `make gate-integrity` exits 0 with such a skipped module in the tree.

**Instead.** Read a skip-cause row's first number as the count it checked. Before relying on a
green `make gate-integrity`, search the test tree for `pytestmark` and `importorskip`. See
[Gate invocation](gate-invocation.md).

### Python SDK: the lease timer accepts a renewal after the robot has stopped

**Limitation.** RFC-0001 requires an adapter to answer `renewed = false`, and not to set its timer
again, for a `renew_lease` that arrives after its own lease lapsed. `LeaseMonitor.renew` in the Python
SDK (`sdk/python/src/swarmada_sdk/safety.py`) refuses only a generation below the current one, and
it keeps the generation after the robot self-stops. An adapter that delegates its lease check to it
answers `renewed = true` and sets a new deadline for an action the robot is no longer running. The
simulation adapter delegates to it without checking the action, so it answers that way too (with
`running = false`); the renewal starts no motion, and the new deadline stops nothing when it passes,
because the adapter no longer holds the action.

**What you will see.** After a self-stop, a late `renew_lease` is answered `renewed = true`, and the
adapter reports a live lease for an action it is not running.

**Instead.** In the adapter's `renew_lease` handler, check that `action_id` names the action the
robot is executing now, at its current generation, before you call `LeaseMonitor.renew`, and answer
`renewed = false` without calling it otherwise. Clear that action when the robot self-stops or
completes. See
[RFC-0001, Assignment lease](../rfcs/dist/RFC-0001-core-spec.md#safety-assignment-lease-and-the-single-executor-guarantee).

### The simulation adapter honours a cancel at any fencing token

**Limitation.** RFC-0001 and conformance row C3.2 require an adapter to reject a `CancelAction`
whose token is at or below its high-water mark, except one that names the action it is executing at
the token that assigned it. The simulation adapter compares no token on `cancel_action`. It stops the
robot for a cancel that names its current action, whatever the token, and answers a cancel for any
other action as already stopped.

**What you will see.** A cancel from a superseded assigner stops the simulated robot instead of being
answered `stale = true`.

**Instead.** Do not take the simulation adapter as a model of the cancel half of fencing. An adapter
you write or test applies the rule to `cancel_action` itself, with the exception for a cancel at the
assigning token (`adapters/CONFORMANCE.md`, C3.2). See
[RFC-0001, Fleet Adapter protocol](../rfcs/dist/RFC-0001-core-spec.md#fleet-adapter-protocol).

### Zone admissions and grants a stop withheld are lost on a change of leader

**Limitation.** While a stop holds a robot, the control plane keeps the zone admission and the
reservation grant it would send it, and sends them once an operator releases the robot. It keeps them in
memory, as the zone controller keeps its own hold. A new leader holds neither.

**What you will see.** After a change of leader, a robot released from a stop at a full zone's boundary
waits there until the next admission change for that zone.

**Instead.** Nothing moves while it waits. If it waits too long, cancel or requeue its action. See
[RFC-0001, Safety](../rfcs/dist/RFC-0001-core-spec.md#safety-how-the-two-interact).

## Compute results

### A ComputeEndpoint result declares no value domain

**Limitation.** A `ComputeEndpoint`'s `spec.results` declares each field's name and kind only: no
allowed values, bounds or unit. A result of kind `enum` names no members, and nothing constrains
the value an endpoint returns. The only check on results is by name, at admission: a
`FleetAction`'s endpoint results must be columns of each `ResultSink` it names that declares a
schema.

**What you will see.** Any value is accepted in a result field of any kind.

**Instead.** Validate a result's value where you consume it. See
[RFC-0001, ComputeEndpoint](../rfcs/dist/RFC-0001-core-spec.md#computeendpoint).

### Results have no egress path

**Limitation.** A `ResultSink` declares where results should go, but at v0.4 nothing writes a
result to a sink, nothing reads its `credentialRef`, and no grant is issued. There is no
artifact reference in the API either.

**What you will see.** A `FleetAction` naming a `ResultSink` is admitted and runs; no row reaches
the sink's endpoint.

**Instead.** Have the executor write its results itself, over a path you provide outside
Swarmada. See [RFC-0001, ResultSink](../rfcs/dist/RFC-0001-core-spec.md#resultsink).

## Floor mapping

### A map reaches the cluster by reference

**Limitation.** The adapter protocol has no message that carries a map. An `explore-floor`
action saves the map at `<namespace>/<action-name>/` in a directory an `ArtifactStore` declares,
and names it in its final status message as `map-ref=<store>/<namespace>/<name>` with
`map-sha256=<digest>`. A typed map message on the adapter protocol is not part of this contract
version.

**What you will see.** No Swarmada resource holds the map. The reference and digest are in the
action's `status.message`, and the files are in the store.

**Instead.** Read the map through the `ArtifactStore`, and check the digest before using it. See
[Map a floor, confirm its zones, work in them](floor-mapping.md).

### A zone proposal is not applied automatically

**Limitation.** The zone-proposal worker writes a proposal and never creates a `FleetZone`. Its
role has no verb on `FleetZone`.

**What you will see.** After a segment succeeds, a `zone-proposal-<action>` `ConfigMap` appears
and no zone changes.

**Instead.** Review `zones.yaml` in the proposal and apply it yourself. That apply is the confirm
step.

### Generated zones are not used for containment, and goals are not checked against them

**Limitation.** Zones generated from a map are `FrameLocal`: their polygons are in the map's
frame, and the control plane uses them for neither containment nor occupancy. A robot is in a
generated zone because its `spec.zone` says so, and a zone's robot count follows `spec.zone`, not
where the robot drives. Dispatch refuses work by the zone an action names: an action whose
`spec.zone` is an exclusion zone is refused, but the goal in its payload is never checked against
any zone's polygon.

**What you will see.** A robot that drives from one generated room into another keeps its
`currentZone`, and neither room's robot count changes. An action addressed to a room, with a goal
inside that room's keep-out, is dispatched.

**Instead.** Keep robots out of a keep-out on the robot: load the keep-out mask the zone-proposal
worker renders from the applied zones into the adapter, as the `keepouts_ref` published in the
`floor-keepouts` `ConfigMap`. Set each robot's `spec.zone` to the generated zone it stands in.
Change a zone's `frame.alignment` to `SiteAligned` only after checking the map frame against the
site frame.

### A robot checks goals against keep-outs only while it localizes on the zones' map

**Limitation.** The keep-out set's polygons are in the frame of the map the zones were generated
from. An adapter that checks a `Navigate` goal against them compares the goal, in the robot's
`map` frame, with the polygons as they are. It can't tell which map the robot localizes on, so it
can't tell whether the two frames agree.

**What you will see.** On any other map, including a fresh map started from another pose, a goal
is accepted or declined against polygons in the wrong place, and nothing reports it.

**Instead.** Before any `Navigate` that relies on the keep-outs, localize the robot on the saved
map the zones were generated from. An `explore-floor` segment doesn't need this step: it proves
its live map is aligned with its bound's map before anything moves.

## The specification

### Dated claims about other projects are not re-checked

**Limitation.** RFC-0001 and the documents beside it describe some standards and projects outside
Swarmada. A claim of the form "X has not happened yet" is true when written and turns false on its
own. No gate here checks such a claim against its date.

**What you will see.** A sentence about a third party's process can be out of date while every
`make spec-*` check passes.

**Instead.** Treat a claim about another project as true on the date it gives. Where it gives
none, check it at the source.

### The stop and action tables are not model-checked for a disconnect during a change of leader

**Limitation.** The project checks RFC-0001's
[robot stop table](../rfcs/dist/RFC-0001-core-spec.md#safety-robot-stop-table) and
[action table](../rfcs/dist/RFC-0001-core-spec.md#safety-action-table) with an exhaustive
state-space search. Two searches do not finish within the memory available to it (16 GB):

- a robot's disconnect together with a change of control-plane leader, on the wire model, ran out
  of memory after 46 minutes and about 10 GB;
- all fourteen table-model failure scenarios together were stopped after an hour and 5.3 GB.

Each scenario alone is covered, and so is every other pair of wire scenarios, five of them at a
smaller bound. For a disconnect during a
change of leader, a reviewer worked through the interleavings by hand and found no violation. RFC-0001
now states the facts that argument needs: safety does not depend on exclusive leadership, and the
adapter fences a deposed leader by the leader term (see *A deposed leader is fenced once the adapter
has seen the new leader's term*).

**What you will see.** Nothing at runtime. A safety case that cites the model check of these
tables has no exhaustive result for these two combinations.

**Instead.** In a safety case, treat a disconnect during a change of leader as covered by argument,
not by model checking. Run one manager replica, as
[One manager replica while ControlStream is enabled](#one-manager-replica-while-controlstream-is-enabled)
requires: a change of leader then closes every stream and holds every robot until it is checked at
the robot.
