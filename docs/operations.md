# Swarmada Operations Surface — Roles, API, and swarmctl

> **Status:** Informative, non-normative. RFC-0001 is the authoritative specification for the operations surface described here.

Read this top-down: **roles first** (who exists), then the
**API + RBAC** contract (what each operation is and who may call it), then **swarmctl**
(the CLI that maps one command to one API operation).

## 1. Roles

The six built-in roles (RFC-0001 #security-role-definitions), in increasing privilege. `robot` and
`edge` are separate service-account roles, not part of the operator ladder.

| Role | What it can do | Typical holder |
| :-- | :-- | :-- |
| `swarmada:viewer` | Read (`get`, `list`, `watch`) thirteen Swarmada kinds: `Robot`, `DiscoveredRobot`, `RobotClass`, `FleetAction`, `FleetTask`, `FleetZone`, `ZoneMaintenance`, `RobotProbe`, `FirmwareRollout`, `ModelRollout`, `ModelPolicy`, `SwarmadaConfig` and `FleetAdapter`. Read (`get`, `list`) core `Event`s, so `swarmctl describe` and `kubectl describe` can list an object's Events. Nothing else. | dashboards, monitoring |
| `swarmada:operator` | Everything `viewer` can, plus `create`, `update`, `patch`, `delete` and `cancel` on **FleetActions**, and the same plus `append` and `activate` on **FleetTasks**. `activate` clears `spec.draft` and dispatches a drafted task; the webhook refuses it to every ServiceAccount, whatever role it holds. Not robots, zones, or config. | shift supervisors |
| `swarmada:fleet-manager` | Everything `operator` can, plus `create`, `update`, `patch` and `delete` on robots, zones, robotclasses, probes, firmware and model rollouts, and model policies; `create` and `delete` on zone maintenances; the `admit` and `reject` verbs; `policy-reset` and `rollout-resume`; `estop-trigger` at robot and zone scope; and `read` and `verify` on the safety audit log. **Not** `SwarmadaConfig` or `FleetAdapter` writes, so **not** a namespace-scope estop; **not** `estop-clear`. | fleet operations engineers |
| `swarmada:admin` | Full management of the same thirteen kinds, including `SwarmadaConfig` and `FleetAdapter`; every custom verb, including `estop-clear` and namespace-scope estop; `robots/status` writes; audit export; core `Event` reads; and CRD management. | platform team |
| `swarmada:robot` | The Fleet Adapter's service-account role: reads the robots, actions and zones in its namespace and writes nothing through the Kubernetes API. The adapter reports robot status over ControlStream (per-message `robot_id` enforced server-side), and the control plane writes `Robot.status`. Cannot create resources or act cross-namespace. | Fleet Adapters |
| `swarmada:edge` | The Zone Controller edge node's service-account role: reads the `FleetZone`s it guards and their `Robot`s, and patches `fleetzones/status` for one purpose — raising and clearing `status.edgeFeedUnavailable`. RBAC cannot hold the patch to that field or to one zone: the role can rewrite the status of every `FleetZone` in its namespace, so bind it only to the edge node's ServiceAccount. No task, estop, or config authority. Bind it in every namespace whose zones declare `spec.edgeNode`: **omit it and edge feed loss goes unreported**, because the write that reports it is the permission being withheld. | Zone Controller edge nodes |

No built-in role, `swarmada:admin` included, grants any verb on `TaskTrigger`,
`SemanticEventSource`, `SemanticEvent`, `ArtifactStore`, `ComputeEndpoint`,
`ComputeWorkloadTemplate`, `EdgeNode` or `ResultSink`. A subject that holds only built-in roles is
refused on each of them: `swarmctl get tasktriggers` returns `forbidden`. Grant those kinds with a
Role or ClusterRole of your own.

**`estop-clear`, config changes, and audit export are admin-only** — automation must
not be granted `estop-clear`. That is a deployment obligation: the control plane authorizes
a clear by the verb alone and does not tell a person from a service account
(RFC-0001 #safety-estop-state-machine).

## 2. API surface and RBAC (the contract)

Every operation is a Kubernetes API call on a `swarmada.io` CRD. **In Kubernetes the
API verb and the RBAC verb are the same thing** — you authorize the exact verb the
client calls — so one "Verb" column covers both. The verbs are the standard ones
(`get`, `list`, `watch`, `create`, `update`, `patch`, `delete`) plus the **custom
verbs** marked *(custom)*: `admit`, `reject`, `cancel`, `append`, `activate`,
`estop-trigger`, `estop-clear`, `policy-reset` and `rollout-resume`. `admit`, `reject`, `policy-reset`
and `rollout-resume` are checked only by `swarmctl`; `update` or `patch` on the
resource is sufficient to perform them with `kubectl` (RFC-0001
[Authorization](../rfcs/dist/RFC-0001-core-spec.md#security-authorization-swarmada-rbac-roles)).
"Least-privileged role" is the lowest role (from §1) that may call the operation.

<table>
  <thead>
    <tr><th>Operation</th><th>Verb (API = RBAC)</th><th>Least-privileged role</th><th>swarmctl</th></tr>
  </thead>
  <tbody>
    <tr><td colspan="4" align="center"><strong>Robots</strong></td></tr>
    <tr><td>read robots</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get robot [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read pre-admission robots</td><td><code>list</code></td><td>viewer</td><td><code>get robot --discovered [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>describe a robot</td><td><code>get</code></td><td>viewer</td><td><code>describe robot &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>admit a discovered robot (the control plane creates the <code>Robot</code>)</td><td><code>admit</code> <em>(custom)</em></td><td>fleet-manager</td><td><code>admit robot &lt;n&gt; --zone … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>re-admit an admitted robot</td><td><code>admit</code> <em>(custom)</em></td><td>fleet-manager</td><td><code>admit robot &lt;n&gt; --force --class … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>reject a discovered robot (records reason)</td><td><code>reject</code> <em>(custom)</em></td><td>fleet-manager</td><td><code>delete robot &lt;n&gt; --reason … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete an admitted robot</td><td><code>delete</code></td><td>fleet-manager</td><td><code>delete robot &lt;n&gt; [-n &lt;ns&gt;]</code>; refused while the robot holds a stop or may still be running an action it records (see below)</td></tr>
    <tr><td>set a robot's charging policy</td><td><code>update</code></td><td>fleet-manager</td><td>no <code>swarmctl</code> command — <code>kubectl patch robot &lt;n&gt; --type merge -p '{"spec":{"charging":{"dockName":"&lt;dock&gt;"}}}'</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Tasks</strong></td></tr>
    <tr><td>create a task</td><td><code>create</code></td><td>operator</td><td><code>kubectl apply -f &lt;fleetaction.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a task</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get fleetaction [-n &lt;ns&gt;]</code>, <code>describe fleetaction &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>resume a system-paused task</td><td><code>update</code>; on an action a FleetTask controls, also <code>cancel</code> <em>(custom)</em></td><td>operator</td><td>no <code>swarmctl</code> command — <code>kubectl annotate fleetaction &lt;n&gt; swarmada.io/requeue-requested="&lt;reason&gt;"</code>; while an emergency stop covers a robot the action is bound to or recorded on, it needs <code>estop-clear</code> on that stop (<strong>admin</strong>), and an operator requeues it once the robot is released (<code>estop resume</code>)</td></tr>
    <tr><td>cancel a task (confirmed stop → Cancelled)</td><td><code>cancel</code> <em>(custom)</em></td><td>operator</td><td><code>cancel &lt;fleetaction&gt; --reason … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a task record (cancel a running one first)</td><td><code>delete</code></td><td>operator</td><td><code>kubectl delete fleetaction &lt;n&gt; [-n &lt;ns&gt;]</code> — not refused in any phase, and it sends a running task's robot no stop</td></tr>
    <tr><td colspan="4" align="center"><strong>Zones</strong></td></tr>
    <tr><td>create a zone</td><td><code>create</code></td><td>fleet-manager</td><td><code>kubectl apply -f &lt;fleetzone.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a zone</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get fleetzone [-n &lt;ns&gt;]</code>, <code>describe fleetzone &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a zone</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete fleetzone &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Maintenance</strong></td></tr>
    <tr><td>create maintenance</td><td><code>create</code></td><td>fleet-manager</td><td><code>kubectl apply -f &lt;zonemaintenance.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read maintenance</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get zonemaintenance [-n &lt;ns&gt;]</code>, <code>describe zonemaintenance &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>activate a scheduled one now</td><td><code>create</code></td><td>fleet-manager</td><td>creation is activation — omit <code>spec.scheduledStart</code></td></tr>
    <tr><td>end an active one (keeps the record)</td><td><code>update</code></td><td><strong>admin</strong></td><td><code>kubectl patch zonemaintenance &lt;n&gt; [-n &lt;ns&gt;] --type=merge -p '{"spec":{"desiredState":"Completed"}}'</code> — requests a close: the window resumes the robots it paused through the same gate the <code>autoResumeAfterMinutes</code> deadline uses, and reaches <code>Completed</code> only once every one of them is resumable (an uncleared estop holds it, and says so in <code>status.conditions[ResumeBlockedByEstop]</code>)</td></tr>
    <tr><td>remove a maintenance (finalizer resumes robots)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete zonemaintenance &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Emergency stop</strong></td></tr>
    <tr><td>estop a zone</td><td><code>patch</code>, <code>estop-trigger</code> <em>(custom)</em></td><td>fleet-manager</td><td><code>estop trigger &lt;zone&gt; --reason … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>clear an estop on a zone</td><td><code>patch</code>, <code>estop-clear</code> <em>(custom)</em></td><td><strong>admin</strong></td><td><code>estop-clear &lt;zone&gt; --reason … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>estop a robot</td><td><code>patch</code>, <code>estop-trigger</code> <em>(custom)</em></td><td>fleet-manager</td><td>no <code>swarmctl</code> command (<code>estop trigger</code> takes a zone) — <code>kubectl annotate robot &lt;n&gt; swarmada.io/estop-triggered="&lt;reason&gt;" [-n &lt;ns&gt;]</code>. The annotation's presence is the stop: an empty value stops the robot too, and the audit log records the reason <code>none given</code>. A zone or namespace clear does not release a robot that carries this annotation; removing it is the robot's clear, and it does not release a robot that a zone's or the namespace's estop still holds: that estop's clear does</td></tr>
    <tr><td>estop a namespace</td><td><code>patch</code> on <code>swarmadaconfigs</code>, <code>estop-trigger</code> <em>(custom)</em></td><td><strong>admin</strong></td><td>no <code>swarmctl</code> command — <code>kubectl annotate swarmadaconfig swarmada-config swarmada.io/estop-triggered="&lt;reason&gt;" -n &lt;ns&gt;</code>. An empty value stops every robot in the namespace too, with the reason <code>none given</code>. A zone's clear does not release a robot this estop still holds, and this estop's clear does not release one a zone's estop still holds</td></tr>
    <tr><td>clear a robot or namespace estop</td><td><code>patch</code>, <code>estop-clear</code> <em>(custom)</em></td><td><strong>admin</strong></td><td>no <code>swarmctl</code> command (<code>estop-clear</code> takes a zone) — <code>kubectl annotate robot|swarmadaconfig &lt;n&gt; swarmada.io/estop-triggered- [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Rollouts (firmware and model)</strong></td></tr>
    <tr><td>create a rollout</td><td><code>create</code></td><td>fleet-manager</td><td><code>kubectl apply -f &lt;rollout.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a rollout</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get|describe firmwarerollout|modelrollout &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>pause or resume a rollout (operator hold)</td><td><code>update</code></td><td>fleet-manager</td><td><code>kubectl patch firmwarerollout|modelrollout &lt;n&gt; [-n &lt;ns&gt;] --type=merge -p '{"spec":{"desiredState":"Paused"}}'</code>, and <code>"Running"</code> to release it. The hold stops the NEXT robot entering a batch; an install already in flight is not interrupted. It cannot release a <code>pauseOnError</code> halt — <code>status.conditions[OperatorHold]</code> says which hold is in force (<a href="adr/0057-operator-intent-into-a-controller-owned-lifecycle-is-a-spec-desiredstate.md">ADR-0057</a>)</td></tr>
    <tr><td>release a <code>pauseOnError</code> halt (abandons the failed robots)</td><td><code>rollout-resume</code> <em>(custom)</em></td><td>fleet-manager</td><td><code>rollout resume &lt;n&gt; --kind firmware|model --reason … [-n &lt;ns&gt;]</code> — moves the failed robots to <code>status.excludedRobots</code> rather than retrying them</td></tr>
    <tr><td>undo or stop a rollout</td><td>—</td><td>—</td><td>not supported — <code>spec.desiredState</code> carries <code>Running</code> and <code>Paused</code> only, and neither rollout controller reverts a robot to its previous version on operator request. The <code>FirmwareRollout</code> controller does not read <code>spec.rollbackPolicy</code>; the <code>ModelRollout</code> controller reads it, and under <code>Auto</code> reverts a robot whose install failed to the version it ran before</td></tr>
    <tr><td>delete a terminal rollout record (Succeeded/Failed only)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete firmwarerollout|modelrollout &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Model policy</strong></td></tr>
    <tr><td>create a policy</td><td><code>create</code></td><td>fleet-manager</td><td><code>kubectl apply -f &lt;modelpolicy.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a policy</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get modelpolicy [-n &lt;ns&gt;]</code>, <code>describe modelpolicy &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a policy (stops auto-deploy)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete modelpolicy &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>reset a suspended policy</td><td><code>policy-reset</code> <em>(custom)</em></td><td>fleet-manager</td><td><code>modelpolicy reset &lt;n&gt; --reason … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>evaluate or deploy a policy</td><td><code>update</code></td><td>fleet-manager</td><td>no <code>swarmctl</code> command — <code>kubectl annotate modelpolicy &lt;n&gt; swarmada.io/model-trigger='&lt;payload&gt;'</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Probes, adapters, robotclasses, config</strong></td></tr>
    <tr><td>create a probe</td><td><code>create</code></td><td>fleet-manager</td><td><code>kubectl apply -f &lt;robotprobe.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a probe</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete robotprobe &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a robotclass (template; admitted robots keep their spec)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete robotclass &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete an adapter (deregisters; no admission check refuses it)</td><td><code>delete</code></td><td><strong>admin</strong></td><td><code>kubectl delete fleetadapter &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a probe, adapter, or robotclass</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get robotprobe|fleetadapter|robotclass [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read config</td><td><code>get</code></td><td>viewer</td><td><code>get swarmadaconfig [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>change config</td><td><code>update</code></td><td><strong>admin</strong></td><td><code>kubectl edit swarmadaconfig swarmada-config [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Audit</strong></td></tr>
    <tr><td>read the safety audit log</td><td><code>read</code></td><td>fleet-manager</td><td><code>get audit [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>verify the audit hash chain</td><td><code>verify</code></td><td>fleet-manager</td><td><code>verify audit [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>export the audit log</td><td><code>export</code></td><td><strong>admin</strong></td><td><code>export audit [-n &lt;ns&gt;]</code></td></tr>
  </tbody>
</table>

## 3. swarmctl

### Conventions

- **`swarmctl <VERB> <RESOURCE> [NAME] [--flags]`** — verb first. `get`, `describe`
  and `delete` take the kubectl shape; `admit`, `cancel`, `estop trigger`,
  `estop-clear`, `verify audit` and `export audit` follow it. Three command groups are
  named for the resource they operate on instead: `rollout resume`,
  `modelpolicy reset` and `robotclass rollout`. `apply`, `create` and `edit` are
  `kubectl` verbs; `swarmctl` has none of them.
- **Resource is the singular CRD name** (`get robot`, `get fleetaction`) — or its
  plural, lowercased kind, or registered short name (`rob`, `fz`, `mp`). Abbreviations
  the CLI does not register (`task`, `zone`, `rollout`, `policy`, `maintenance`) do not
  resolve.
- **`get` is the only read command** — no name lists everything, a name returns one
  (`get robot` vs `get robot <name>`); there is no separate `list` command. At the API
  layer this is two Kubernetes verbs (`list` with no name, `get` with a name) plus
  `watch`; RBAC grants them together, which is why the §2 Verb column lists `get` and
  `list` even though the CLI has a single `get`.
- **Namespace** is `-n <namespace>` (alias `--namespace`), a global flag accepted by
  every command, defaulting to the context namespace. There is no all-namespaces flag:
  `get` has no `-A`, so list another namespace with `-n`, or across namespaces with
  `kubectl get <kind> -A`.
- **`--reason` is required and non-empty on `estop-clear` and `estop resume`** — swarmctl
  refuses an empty or whitespace-only reason before it writes anything; no webhook checks
  it. On `estop trigger` it is optional: without one, `"true"` is written as the reason.
  The value is written to the safety audit log (`ESTOP_TRIGGERED`, `ESTOP_CLEARED`,
  `ESTOP_RESUME_CONFIRMED`, `ESTOP_RESUME_REFUSED`).
- **Every mutating command checks existence first** — `cancel`, `estop trigger`,
  `estop-clear`, `admit`, `delete robot`, `modelpolicy reset` and `rollout resume` each
  resolve the named object, then check your permission, then ask you to confirm. A name
  that is not there is reported as not there: you are never told you lack a permission on
  an object that does not exist, and never shown a destructive confirmation for one.
- **`kubectl apply -f`** is the declarative path for every resource. `swarmctl` has no
  `apply` and no `create`: it is a read and lifecycle-verb tool over the same API, not a
  replacement for `kubectl`. There is no imperative
  `create <resource> --flags` command.
- **A named resource resolves on its own** — filter flags (`--discovered`) apply only
  to `get`, never when a resource is named.

### Robots and discovery

```
swarmctl get robot [-n <namespace>]                  # admitted robots in one namespace
swarmctl get robot --discovered [-n <namespace>]     # pre-admission (DiscoveredRobot) robots
swarmctl describe robot <name> [-n <namespace>]      # name resolves whether admitted or discovered
swarmctl admit robot <name> --zone <leaf-zone> [--class <robotclass>] [--adapter <fleetadapter>] \
                            [--name <override>] [--dock <dock>] [--manufacturer <m>] [--model <m>] [-n <ns>]
                                                     # --adapter is REQUIRED unless --class supplies one.
                                                     # --class is REQUIRED when the adapter lists
                                                     # spec.servesRobotClasses, or reports a version
                                                     # that is not X.Y.Z (the class pins one).
                                                     # --manufacturer/--model override the discovered values.
swarmctl admit robot <name> --force --class <robotclass> [-n <namespace>]
                                                     # re-admit an already-admitted Robot: re-applies the
                                                     # RobotClass. --class is REQUIRED here; --zone and
                                                     # --name are not read on the re-admit path.
kubectl patch robot <name> --type merge -p '{"spec":{"charging":{"dockName":"<dock>"}}}' -n <namespace>
                                                     # swarmctl has no command that sets charging
swarmctl delete robot <name> [--reason "<text>"] [-n <namespace>]
                                                     # discovered ⇒ reject (reason recorded); admitted ⇒ delete
```
**Admit and re-admit are the same command** — re-admit is only `admit robot` with
`--force`, so it takes the same flags. First admission of a `--discovered` robot
requires `--zone` (a new `Robot` needs one) and optionally `--class` and `--name`. If
the name already resolves to an admitted `Robot`, the command is rejected unless
`--force` is passed. On that re-admit path `--class` is **required** (it names the
RobotClass to re-apply); `--zone` and `--name` are optional and default to the
robot's current values — pass them only to change something. (So re-admit isn't a
smaller command; `admit robot <name> --force` is shorthand for it.)

`delete robot` is one command: a discovered target invokes the `reject` custom verb
(with the optional reason), an admitted target is a plain `delete` — RBAC still
distinguishes the two verbs, so "who may reject" and "who may delete a live robot" stay
separate.

**A robot that holds a stop cannot be deleted.** On an install with admission webhooks
(`make deploy`, the Helm chart by default), `delete robot` and `kubectl delete robot` are refused, for
every identity, admin included, while the robot's `estopState` is set and not `Normal`, its
phase is `Error`, or a hardware component is reported `Failed`. The stop is kept only on the
`Robot`, so a robot deleted and re-applied would come back `Normal` and dispatchable. Nor can
such a robot lose its last finalizer, except to the control plane. It stays held until it is
released: `swarmctl estop resume` for an estop, or the robot's own report that the phase or the
component recovered. A robot held by an estop or a `Failed` component that never reports again
cannot be released, so it stays held, and its `Robot` stays, until it reports again. A robot held
only by `Error` that falls silent is marked `Offline`, which ends that hold; it reports `Error`
again when it is heard from if the fault remains. Deleting its namespace removes it; no command
decommissions one robot. Offline auto-removal skips such a robot
and records a `Warning` Event, `AutoRemovalHeld`, on it (`kubectl describe robot <name>`).

**A robot that may still be running an action cannot be deleted.** While the robot records an
action in `status.assignedAction` and the control plane cannot prove it off that action, `delete robot`
and `kubectl delete robot` are refused for every identity, and only the control plane may remove its
last finalizer. The record is what keeps the action from a second robot. The refusal names the
action. If the action is bound to the robot, cancel or requeue it first: while the robot is
reachable its lease is renewed and waiting does not lift the refusal; the delete is admitted once the
adapter acknowledges the cancel or the lease is provably dead. If it is bound to no robot, the
refusal names the time at which the lease is provably dead, or, when no time is known, waits for the
adapter to acknowledge a cancel or report the action ended. If the action no longer exists, the refusal
stands until the control plane releases the record. The check fails open like the stop refusals, and admits the delete
while the robot's namespace is being deleted.

The delete check is its own webhook, `vrobotdelete.swarmada.io`, with `failurePolicy: Ignore`.
While no manager replica serves the webhooks, a `Robot` delete is admitted, a held robot's
included, and a held robot deleted during the outage comes back `Normal` if it is created again.
Deleting its namespace can still wait for the manager: see
[Deleting a namespace during a manager outage](#4-deleting-a-namespace-during-a-manager-outage). A `Robot` being deleted gets no new work. A
Robot's `swarmada.io/robot-id` cannot change after it is created, for any identity. An install
without admission webhooks refuses no delete
([Known limitations](known-limitations.md#an-install-without-admission-webhooks-checks-nothing-at-admission)).

A connected adapter is not told that a `Robot` was deleted, so the simulation adapter does not
announce that robot again until it reconnects: restart the adapter before admitting the robot
again, as the [samples README's first run](../config/samples/README.md#the-secure-first-run-first-run) does.

**Admit and reject complete asynchronously.** Both verbs record the decision on the
`DiscoveredRobot` and return; the control plane then creates the `Robot` (admit) or seals
the `ROBOT_REJECTED` audit entry (reject), and removes the staging object either way. So a
successful command means *the decision is recorded*, not that the robot is already
schedulable — `get robot` is the confirmation. Two things follow from that split:

- Admitting does **not** require permission to create a `Robot`. The admission gate (RFC-0001 §6.6)
  is the only route from discovered to schedulable, and an operator who could create Robots
  directly would be able to step around it.
- A rejection is distinguishable from a TTL sweep. Both end in the same delete, so without
  a recorded decision the audit chain could not tell an operator's refusal from a robot
  that was never admitted.

If the control plane cannot complete a marked admission (say the `--class` was deleted in
between, or the Robot admission gate refuses the `Robot` while its adapter is not yet
`Connected`), it records an `AdmissionFailed` event on the `DiscoveredRobot`, with the reason,
rather than creating a partly-configured robot, and retries a refusal that can clear. What
`admit` can know will be refused, it refuses before recording anything: an adapter-reported
version the `Robot` CRD's `X.Y.Z` pattern refuses (the simulation adapter reports
`0.1.0-sim`; a `--class` whose `spec.baseAdapter.version` is a release replaces it), and a
missing or unserved `--class` for an adapter that lists `spec.servesRobotClasses`.

**When a robot was admitted.** A `Robot` records no admission time and no count of re-admissions.
`kubectl get robot` prints `AGE`, the age of the `Robot` object; `swarmctl admit robot --force`
re-applies a class to the existing object and leaves its age as it was.

### Tasks

`FleetAction` is the atomic unit of dispatch; `FleetTask` is the composite that owns a
graph of them. The commands below name the CRD kind, because the word "task" alone does
not distinguish the two and does not resolve at the CLI.

```
kubectl apply -f <fleetaction.yaml> [-n <namespace>]  # or a fleettask.yaml for the composite
swarmctl get fleetaction [-n <namespace>]            # alias: fact
swarmctl describe fleetaction <name> [-n <namespace>]
swarmctl cancel <name> --reason "<text>" [-n <namespace>]        # a bare name is a FleetAction
swarmctl cancel action <name> --reason "<text>" [-n <namespace>]
kubectl annotate fleetaction <name> swarmada.io/cancel-requested="<reason>" [-n <namespace>]
                                                     # what `cancel action` writes; terminal: confirmed stop → Cancelled (NOT resumable)
kubectl annotate fleetaction <name> swarmada.io/requeue-requested="<reason>" [-n <namespace>]
                                                     # return a Paused action to Pending once its robot is provably off it:
                                                     # the adapter acknowledges the cancel, or the lease has provably expired;
                                                     # while a stop covers a robot bound to it or recording it,
                                                     # it needs estop-clear on that stop
kubectl delete fleetaction <name> [-n <namespace>]   # remove the record; not refused while it runs, and stops nothing: cancel first
```

The composite has a `swarmctl` read and cancel surface; it has no `swarmctl delete`.

```
swarmctl get fleettask [-n <namespace>]              # alias: ft
swarmctl describe fleettask <name> [-n <namespace>]
swarmctl cancel task <name> --reason "<text>" [-n <namespace>]
kubectl delete fleettask <name> [-n <namespace>]     # `swarmctl delete` wires only `delete robot`
```

Both kind tokens are local to `cancel`: `action` and `task` name the two kinds this
command operates on, and are accepted alongside the registered spellings
(`fleetaction`/`fact`, `fleettask`/`ft`). They are not registered as resource
aliases, so `get task` still does not resolve — see Conventions above.

A bare name that belongs to the other kind is reported as such rather than as a
missing object: `swarmctl cancel restock-7` on a `FleetTask` says it is a `FleetTask`
and names `cancel task restock-7`. That check runs before the RBAC gate and before the
confirmation prompt, so a mistyped or wrong-kind name never draws a destructive prompt
and never reports a permission failure for an object that is not there.

`cancel task` sets `spec.desiredState: Cancelled` on the `FleetTask` and lets the
controller cancel each member action; it does not write the `cancel-requested`
annotation, because the mechanism is a spec write rather than a request the controller
finalizes. `--reason` has no field to land on for a `FleetTask`, and the controller's
cancellation of each member action carries none, so the reason is not recorded anywhere;
the command prints a note saying so. The task ends `Cancelled` once every member is
terminal, including members of a dependency chain that were never created because the
member they wait on was cancelled. A task that has already ended (`Succeeded`, `Failed`, `Cancelled` or
`Compensated`) is not reconciled again: `cancel task` on it cancels none of its members, and says so;
cancel a member that is still running with `cancel action`. A failure an adapter reports while an
emergency stop covers its robot pauses the member rather than failing it, so the task does not end
`Failed` from it and `cancel task` reaches the paused member. No `swarmctl` verb
writes the requeue annotation; the annotation is the operator intake the
`FleetAction` controller reconciles.
A ready-to-apply example ships at `config/samples/fleettask_sample.yaml` — a
three-member `receiving-round` chain whose members gate on `dependsOn`:

```
kubectl apply -f config/samples/fleettask_sample.yaml -n warehouse-a
```

A `FleetAction` reaches `Paused` two ways: an estop on its assigned robot, or an
operator or trigger setting `spec.desiredState: Paused` (or `Returning`, which first
confirms the robot stopped) on an action that is `Assigned` or `InProgress`. `Paused` sends
the robot `cancel_action`, and an adapter that acknowledges it stops the robot. On an action
a `FleetTask` owns, the task sets that field and overrides a direct write. There is
no `swarmctl` pause verb; the spec field is written with `kubectl patch`. Writing
`Running` back does not lift the hold: the requeue annotation returns the action to
`Pending`, and `cancel` ends it. The other two interruptions do not produce `Paused`.
`ZoneMaintenance` in `Immediate` mode (or `Graceful` past its drain timeout) writes the
requeue annotation itself, so the action returns to `Pending`, unless a stop, issued or
recorded, covers the robot, or the robot is `Resuming`. Under RFC-0001 the window never marks work
a stop held. At v0.4 it marks that work once the robot is released, and a lease deadline that
passed during the stop also moves it (see *After a stop, an operator moves the work* in
[Known limitations](known-limitations.md)). Preemption by a
`Critical` action produces `Preempted`, which returns to `Pending` on its own once its
former robot has switched to the `Critical` action or its lease is provably dead.

`cancel` and `delete` are **not** interchangeable. `cancel` *stops* a running task
through the confirmed-stop path (the robot is freed only once it is proven off the action) and
leaves a `Cancelled` record for audit. `delete` *removes the record* and nothing else, and
it is not refused in any phase. On an active action (`Assigned`/`InProgress`/`Revoking`)
the robot is not freed at once, because it may still be running the action: it stays
`InProgress`, gets no new work and cannot be deleted until the control plane releases its
record, when the adapter acknowledges the `cancel_action` it is then sent, or once the
longest lease plus the clock-skew margin has passed
([Known limitations](known-limitations.md#deleting-a-running-fleetaction-holds-its-robot-until-the-record-is-released)).
`cancel` first, wait for `Cancelled`, then delete if you want the record gone.

### Zones and maintenance

```
kubectl apply -f <fleetzone.yaml> [-n <namespace>]
swarmctl get fleetzone [-n <namespace>]
swarmctl describe fleetzone <name> [-n <namespace>]
kubectl delete fleetzone <name> [-n <namespace>]
kubectl apply -f <zonemaintenance.yaml> [-n <namespace>]
                                                     # creation IS activation: omit spec.scheduledStart and the
                                                     # controller activates on first reconcile; scope.type
                                                     # Zone|Namespace selects a zone or the whole namespace
swarmctl get zonemaintenance [-n <namespace>]
swarmctl describe zonemaintenance <name> [-n <namespace>]
kubectl delete zonemaintenance <name> [-n <namespace>]       # closes an Active window (the zonemaintenance-resume
                                                     # finalizer resumes every paused robot) and cancels a Scheduled one
```

### Emergency stop

```
swarmctl estop trigger <zone> --reason "<text>" [-n <namespace>]
swarmctl estop-clear <zone> --reason "<text>" [-n <namespace>]
swarmctl estop resume <robot>... --reason "<text>" [-n <namespace>]
```
`estop trigger` and `estop-clear` act on a FleetZone (zone scope), via the zone-estop
controller. `estop resume` acts on Robots: it releases each named robot from `Resuming`
after a clear of any scope. Robot- and namespace-scope estops have no `swarmctl` command: they are
triggered and cleared by writing the `swarmada.io/estop-triggered` annotation on the Robot
or on the namespace's `SwarmadaConfig` (§2 table; RFC-0001 #safety-estop-scopes). The
`estop-clear` command has no option to requeue or cancel paused actions in
bulk.

`estop-clear` and `estop resume` **require a non-empty `--reason`** (swarmctl rejects an empty or
whitespace value before acting); the reason is written to the safety audit log. `estop
trigger` records its reason there too. Each command writes the annotation with a merge
patch, so it needs `patch` on the carrier as well as the custom verb: `estop trigger` needs
`patch` and `estop-trigger` on `fleetzones` (fleet-manager and admin); `estop-clear` needs
`patch` and `estop-clear` (admin-only). `estop resume` needs `patch` and `estop-clear` on
`robots` (admin-only): releasing a robot is the release half of a clear. A namespace-scope
estop needs `patch` on `swarmadaconfigs`, which only admin holds.

**What these commands do.** `estop trigger` sends a *confirmed* stop to every robot in
scope and logs the reason. `estop-clear` sends nothing to a robot: it logs the reason and
moves each robot the zone's stop holds to `Resuming`, wherever that robot is. A clear of any
scope leaves a robot that another stop still holds (its own robot-scope trigger, another zone's
stop or the namespace's stop) in its state, with a `Warning` Event on the robot,
`EstopHeldByOwnTrigger` or `EstopHeldByWiderStop`, that names that stop; `estop-clear` also prints
`robot.swarmada.io/<name> is not released by this clear: it is still held by …` and that stop's
clear. The robot reaches `Resuming` on the clear of the last stop that holds it. On a zone
that carries no stop it changes nothing, says the zone has no emergency stop to clear, and
exits non-zero. A robot or namespace clear is a `kubectl annotate ... swarmada.io/estop-triggered-`;
on a `Robot` or `SwarmadaConfig` that carries no `swarmada.io/estop-triggered` annotation it
changes nothing, though kubectl still prints `annotated` and exits 0. A robot in `Resuming` stays there, withheld from
dispatch, until an operator releases it with `swarmctl estop resume`; nothing releases it
automatically, and nothing times the window out (RFC-0001 #safety-estop-state-machine).
On an install with admission webhooks (`make deploy`, the Helm chart by default) this is
enforced at the API: a write to a Robot's `status.estopState` or `status.estopBaseline` is refused
unless it comes from the control plane's own service account, so `swarmada:admin` cannot
release a robot with a `robots/status` patch. The same check refuses, from any identity but
the control plane's, a write that moves `status.phase` out of `Error`, or into or out of
`Maintenance` or `Offline`, removes or changes a
`status.hardware` entry reported `Failed`, or changes `status.capabilities`: the inputs the
resume checks and the scheduler read. It also refuses a change to the state of any
`status.hardware` entry, to the `status` of any `status.installedModels` entry, and to
`status.modelGrantedCapabilities`, which the resume check derives the capabilities from again,
and a change to `status.estopHeldBy`, the record of the zone and namespace stops that hold the
robot, or to `status.stops`, the record of each stop the control plane sent it, or to
`status.currentZone`, which decides which zone stops reach it, and a change to
`status.assignedAction`, the record of the action a robot may still be running, or to the
`status.phase` of a robot that is `Assigned` or `InProgress`. The check is the validating webhook
`vrobotstatus.swarmada.io`, with `failurePolicy: Fail`: while no manager replica serves it,
every `robots/status` update is refused. The chart renders `Ignore` only when
`webhooks.acknowledgeFailOpenSecurity` is also set. It guards those fields only; battery,
connectivity and a component's or a model's other fields are not checked. A Fleet Adapter
cannot write `robots/status` on any install: its `swarmada:robot` role holds no such write,
and it reports over ControlStream. `make deploy-minimal`, the quickstart overlays, the Helm
chart with `webhooks.enabled=false` and `make run` install no admission webhooks: there, any
other principal allowed to write `robots/status` can set the estop state directly, skipping
the clear, the resume checks and the audit entry, and can rewrite the phase, hardware and
capabilities those checks read. The estop-annotation, `cancel`, `append` and `activate` checks
and the delete refusal do not run there either
([Known limitations](known-limitations.md#an-install-without-admission-webhooks-checks-nothing-at-admission)).
On any install, `swarmada:admin` bound cluster-wide can remove the Robot CRD's status
subresource and write status past this guard
([Known limitations](known-limitations.md#write-access-to-crds-is-outside-the-robot-status-guard)).
The release is **refused**, and `estop resume` prints why, while the robot has sent no
telemetry since the clear, while its latest report is not yet written to `Robot.status`
(the status write failed or the write rate cap holds it), while its own latest report puts
it in `Error` or `Offline`, while a hardware component is reported `Failed`, or while a
capability that was Active when the stop was taken is not Active. A capability that was
not Active before the stop does not block the release, nor does one the robot no longer
declares, and one a `ZoneMaintenance` window has paused never does. The stop records its
Active capabilities on `status.estopBaseline`; a stop with no record, taken before the
control plane kept one, requires every capability that is not paused to be Active. There is no override: a robot with a `Failed`
component, or with a capability that was Active at the stop and is not Active now, stays
`Resuming` until the robot reports the component repaired, or until the
component or capability is removed from its RobotClass (`spec.hardware` or
`spec.baseCapabilities`) and applied with `swarmctl robotclass rollout <class>`. A refused
release is not retried; run `estop resume` again once the cause is resolved. An `estop` pushes an
`Estop` down each robot's `SafetyStream`;
the Fleet Adapter commands a hardware stop and confirms within 500 ms (`EstopAck =
STOPPED`) — a robot the control plane cannot confirm resolves to `Failed`/escalate,
never a false "stopped". Each affected robot's running task moves to `Paused`. A
software estop is **not** a substitute for physical safety hardware (RFC-0001 §9.7.1).

**Worked examples.**

```
# A zone — fire alarm on floor 2 (confirmed-stops every robot in the zone):
swarmctl estop trigger floor-2 --reason "fire alarm" -n hospital-east
#   → each robot confirmed-stopped; running tasks → Paused; ESTOP_TRIGGERED audit entry (reason recorded).

# Clear the zone after inspecting:
swarmctl estop-clear floor-2 --reason "all clear after drill" -n hospital-east
#   → estop state → Resuming (withheld from dispatch), except a robot another stop still holds,
#     which the command names; paused actions stay Paused (operator-gated).

# Release each inspected robot:
swarmctl estop resume amr-01 amr-02 --reason "inspected, path clear" -n hospital-east
#   → each robot → Normal, or "refused" with the reason and the robot stays Resuming;
#     ESTOP_RESUME_CONFIRMED / ESTOP_RESUME_REFUSED audit entry. Then requeue each paused
#     action with the annotation above.
```
Automation must not be granted `estop-clear` (RFC-0001 §9.7.2.3); the control plane does not
enforce this, so it holds only while no automation identity holds the `estop-clear` verb.
`estop` stops the *robot*; `cancel` stops the *action* — different layers.

### Rollouts (one family — FirmwareRollout and ModelRollout)

```
kubectl apply -f <rollout.yaml> [-n <namespace>]     # FirmwareRollout or ModelRollout
swarmctl get firmwarerollout|modelrollout [-n <namespace>]
swarmctl describe firmwarerollout|modelrollout <name> [-n <namespace>]
kubectl delete firmwarerollout|modelrollout <name> [-n <namespace>]
                                                     # remove a terminal (Succeeded/Failed) record only
kubectl patch firmwarerollout|modelrollout <name> --type=merge -p '{"spec":{"desiredState":"Paused"}}' [-n <namespace>]
                                                     # operator hold; "Running" releases it
swarmctl rollout resume <name> --kind firmware|model --reason "<text>" [-n <namespace>]
                                                     # release a pauseOnError halt, excluding the failed robots
# no command undoes or stops a rollout — see below
```
`FirmwareRollout` and `ModelRollout` are separate kinds and are named separately; there
is no single `rollout` spelling that resolves to both.

**Pause and resume are supported; undo and stop are not.** An
operator pauses an in-flight rollout by setting `spec.desiredState: Paused`: no further
robot enters a batch, an install already in flight is not interrupted, and
`status.conditions[OperatorHold]` is `True`. Setting `Running` releases that hold and
nothing else. A rollout halted by `pauseOnError` stays halted under `Running`; the exit
from that halt is `swarmctl rollout resume` (custom verb `rollout-resume`), which moves
the failed robots to `status.excludedRobots` rather than retrying them. Under
`rollbackPolicy: Auto` a model rollout also halts on a robot it reverted after a failed
update, and resume adds those robots to `status.excludedRobots` too. The two holds
are independent, and the rollout dispatches only when neither is in force. `undo` and
`stop` have no backing: `spec.desiredState` has no value for them, and neither rollout
controller reverts a robot on operator request. `delete` only removes a **settled**
record and is accepted **only** on a terminal rollout (`Succeeded`/`Failed`), rejected
while `Progressing`/`Paused`.

**Where the artifact lives.** A rollout is authored as YAML and applied with
`kubectl apply -f`; `swarmctl` has no flags for these fields. `spec.firmwareUri` /
`spec.modelUri` is the location of the binary. The CRD constrains `modelUri` to an
`oci://`, `s3://` or `https://` URI; it does not constrain the scheme of
`firmwareUri`. **Firmware** requires `firmwareChecksum` (`sha256:<64 hex>`) and may set
`firmwareSignatureRef` (a `bundle:`/`https://`/`oci://` ref), which the
`FirmwareRollout` controller verifies against `SwarmadaConfig.spec.signing.trustRoots`
before dispatch, fail-closed. **Model** requires `modelChecksum` (`sha256:<64 hex>`)
and may set `modelSignatureRef`; with admission webhooks serving and signing enforced,
the `ModelRollout` validating webhook refuses a rollout without `modelSignatureRef`.
The control plane does not verify a model signature itself: it carries both values to
the adapter, which verifies the artifact on the robot before install (RFC-0001, "Fleet
Adapter compliance checklist"). `spec.modelName` names the model.

**Why `modelName` (models have it, firmware doesn't).** A robot runs several named
models at once (`item-recognition`, a grasp policy, …); `modelName` is the stable
identity used to track which installed model this rollout upgrades and to gate the
capabilities that model provides (RFC-0001 §6.10.2). Firmware is singular per robot, so
`newVersion` + `firmwareUri` suffice — no name.

**Descriptors come from the `ModelRollout` spec.** The control plane does not read
`modelName`, `requiredHardware` or `grantsCapabilities` from the artifact's OCI labels
or a model manifest.

### Model policy

```
kubectl apply -f <modelpolicy.yaml> [-n <namespace>]
swarmctl get modelpolicy [-n <namespace>]
swarmctl describe modelpolicy <name> [-n <namespace>]
swarmctl modelpolicy reset <name> --reason "<text>" [-n <namespace>]
                                                     # clears a FailedRepeatedly suspension (custom verb: policy-reset)
kubectl annotate modelpolicy <name> swarmada.io/model-trigger='<metrics-json>' [-n <namespace>]
                                                     # the single evaluation path; swarmctl has no evaluate or deploy verb
kubectl patch modelpolicy <name> --type merge -p '{"spec":{"qualityGate":{...}}}' [-n <namespace>]
kubectl delete modelpolicy <name> [-n <namespace>]   # stops auto-deploy; in-flight rollouts already created are unaffected
```
**What it's for.** `ModelPolicy` is the **deploy gate** between a model-training pipeline
and the fleet: it watches for a new model version (webhook, registry-watch, or manual),
runs a **quality gate**, and only if the gate passes auto-creates a `ModelRollout`.
Training = CI; the policy = the release gate that keeps bad models off robots.

*Example.* A policy for `item-recognition` with `minPickSuccessRate: 0.90`,
`minEvalEpisodes: 200`, `autoDeployOn: QualityGatePass`. Isaac Lab finishes `v2` and
posts metrics (pick-success 0.94, 500 episodes) → gate passes → a `ModelRollout` deploys
`v2` automatically. Next night `v3` posts 0.85 → gate fails → rejected with the metrics
in a Kubernetes event, nothing deploys, robots stay on `v2`. After
`consecutiveRejectionLimit` rejections the policy suspends until `modelpolicy reset`.
Writing the `model-trigger` annotation forces a run; it is also the manual deploy path.

### Probes, adapters, robotclasses, config

```
kubectl apply -f <robotprobe.yaml> [-n <namespace>]
swarmctl get robotprobe | fleetadapter | robotclass [-n <namespace>]
swarmctl describe robotprobe <name> | fleetadapter <name> | robotclass <name> [-n <namespace>]
kubectl delete robotprobe <name> [-n <namespace>]    # stops the active check; nothing to drain (plain delete)
kubectl delete robotclass <name> [-n <namespace>]    # removes the template; already-admitted robots keep their merged spec
kubectl delete fleetadapter <name> [-n <namespace>]  # admin only; deregisters the adapter — robots that name it get no new work until it is created again, and are still observed and stopped
swarmctl get swarmadaconfig [-n <namespace>]
kubectl edit swarmadaconfig swarmada-config [-n <namespace>]
```
No resource has an imperative `create` — `swarmctl` has no `create` verb. For
`RobotClass`, `ModelPolicy` and `FleetAdapter` the spec (hardware inventory, quality
gates, trust material) would not reduce to flags in any case; every resource is authored
with `kubectl apply -f`.

Two resources have **no `delete`** on purpose. `SwarmadaConfig` is a namespace singleton
the control plane auto-creates with defaults — deleting it is a no-op (it is recreated),
so change it with `kubectl edit`, never delete it. The **safety audit log** is append-only
and has no `delete` verb by design. Its hash chain makes an edited, removed or reordered
entry detectable (`verify audit` recomputes it), with two exceptions: a log whose newest
entries were removed still chains, and so does a rewrite re-sealed from the changed entry
on, and `verify audit` passes both (see
[Known limitations](known-limitations.md#a-verified-audit-chain-can-still-be-missing-its-newest-entries)).
`delete adapter` is the one delete
that touches live operation — see the caveat above; no admission check rejects it,
even while the adapter is serving robots.

**What a probe is for.** Passive telemetry reports what a robot *says*; a `RobotProbe`
makes the control plane *actively verify* a hardware component, capability, or model via
the adapter's `Verify*` RPCs — catching **silent failures** telemetry misses (a camera
that reports Healthy while dropping 60% of frames). Probes are debounced
(`failureThreshold`/`recoveryThreshold`) so noise doesn't flap capabilities.

*Example.* A `RobotProbe` named `cam-liveness` with `robotSelector` matching
`class: acme`, `probeType: hardware`, `targetComponent: camera-front` and
`intervalSeconds: 30`, applied with `kubectl apply -f`
([`config/samples/robotprobe_demo.yaml`](../config/samples/robotprobe_demo.yaml) is a
complete one). Every 30s the control plane verifies `camera-front`
(e.g. frame-rate ≥ 80%); after 3 bad cycles it goes `Degraded` → the `item-pick.ai-guided`
capability (needs a healthy camera) stops being schedulable → the scheduler stops sending pick
tasks to that robot **before** it fails one; two good cycles later it recovers. (The capability
status written in that state is `Inactive`. RFC-0001's truth table specifies `Unavailable`; the
control plane does not write that value at v0.3, so tooling should match on `Inactive`.)

### Miscellaneous

```
swarmctl version                                     # client version, git commit, Go version and platform
swarmctl robotclass rollout <class> [-n <ns>]        # re-admit every Robot of a class after a class change
swarmctl modelpolicy reset <policy> --reason "<text>" [-n <ns>]
                                                     # clear a suspended ModelPolicy (--reason required)
```

`--yes` skips the interactive confirmation on every command that prompts (`admit --force`,
`delete robot`, `cancel`, `estop trigger`, `estop-clear`, `estop resume`, `modelpolicy reset`,
`rollout resume`, `robotclass rollout`). Use it only in automation; the prompt exists
because each of these commands changes robot state.

### Audit (safety audit log)

```
swarmctl get audit    [--file <chain>] [-o yaml|json]       # print the chain
swarmctl verify audit [--file <chain>]                      # recompute the hash chain; non-zero on tamper
swarmctl export audit [--file <chain>] [--out-file <file>]  # re-emit as JSONL (admin-only)
```
Today these operate on an **exported** chain (newline-delimited JSON from `--file` or
stdin), so they compose: `swarmctl export audit | swarmctl verify audit`. There is
no server-side read API, no `--since` / `--type` filter and no live `-n <namespace>`
scope. (`export audit` uses `--out-file`; the persistent `-o/--output`
selects table/yaml/json.)
**What it's for.** The **hash-chained, tamper-evident** safety audit log records every
safety- and security-relevant event (estop trigger/clear, admit/reject, model
deploy/reject, config change) with structured fields — timestamp, identity, resource,
reason, outcome. Hash-chaining means an entry can't be altered, deleted or reordered
without breaking the chain, except at the end: removing the newest entries leaves a
shorter chain that still verifies.

*Example.* `estop trigger floor-2 --reason "fire alarm"` writes `ESTOP_TRIGGERED{scope,
triggered_by, reason, ts}`. Incident review: `get audit` shows who stopped what, when,
and why (there is no `--since` or `--type` filter). For a safety case, `verify audit`
recomputes the chain and shows that what remains is intact and in order. It does not
show that none of the newest entries were removed: record each namespace's highest
sequence number outside the cluster and compare. `export audit` (admin-only) hands a
regulator the record. This is why `--reason` is required on `estop-clear` and
recorded on every `estop trigger`.

## 4. Deleting a namespace during a manager outage

While no manager replica serves the admission webhooks, `kubectl delete namespace` prints
`deleted` and the namespace stays `Terminating` if it holds a `FleetZone`, `FleetTask`,
`FirmwareRollout`, `ModelRollout` or `ZoneMaintenance`. A namespace with robots always holds a
`FleetZone`. The delete checks on the first four kinds fail closed, and only the manager removes a
`ZoneMaintenance`'s finalizer
([Known limitations](known-limitations.md#a-namespace-deleted-during-a-manager-outage-stays-terminating)).
`Robot` deletes are admitted.

See what holds the namespace:

```sh
kubectl get namespace <namespace> \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.message}{"\n"}{end}'
```

`NamespaceDeletionContentFailure=True` with `failed calling webhook "vfleetzone.swarmada.io"` (or
`vfleettask`, `vfirmwarerollout`, `vmodelrollout`) is a refused delete.
`NamespaceFinalizersRemaining=True` naming `swarmada.io/zonemaintenance-resume` is a
`ZoneMaintenance` waiting for the manager.

### Restore the manager

This is the first choice, and it gives nothing up. Find the manager from the webhook
configuration, which names the Service it calls:

```sh
VWC=$(kubectl get validatingwebhookconfigurations -o json \
  | jq -r '.items[] | select(any(.webhooks[]; .name == "vfleetzone.swarmada.io")) | .metadata.name')
MGR_NS=$(kubectl get validatingwebhookconfiguration "$VWC" \
  -o jsonpath='{.webhooks[0].clientConfig.service.namespace}')
kubectl -n "$MGR_NS" get deployments,pods
```

Bring a replica back: scale the Deployment up if it was scaled down, or fix what keeps its pod from
becoming `Ready` (`kubectl -n "$MGR_NS" describe pod <pod>`). Once a replica serves, the namespace
controller retries by itself and the namespace goes; no further command is needed. That is not so
for a namespace that holds an active `ZoneMaintenance`, a `FleetTask` that is still running, or a
`ModelRollout` that has not finished: those hold it with the manager serving too
([Known limitations](known-limitations.md#a-namespace-holding-an-active-maintenance-window-a-running-task-or-an-unfinished-model-rollout-can-stay-terminating)).
Use the next two sections for it.

### Take the delete checks out

Do this only when the namespace must go before the manager can return, or when, with the manager
serving, a running `FleetTask` or an unfinished rollout still holds it. It is an operator's
decision, and it holds for **every namespace in the cluster** until you put the checks back: any
other namespace already being deleted can go in the same moment.
While they are out:

- a `FleetZone` that a child zone, a robot or an unfinished action still refers to can be deleted,
  and a zone deleted while its estop is `Triggered` takes that stop with it;
- a `FleetTask` that is still running can be deleted, and its `FleetAction`s and their assignment
  leases go with it while a robot may still be moving;
- a `FirmwareRollout` or `ModelRollout` that has not finished can be deleted, and a `ModelRollout`'s
  status is what un-suspends the capabilities of the models it marked `Updating`.

The steps remove only `DELETE` from the four checks; their create and update checks stay, and stay
fail-closed. Run them in one shell, from a directory where the backup file can stay until the last
step:

```sh
# 1. Find the configuration and save it as it is.
VWC=$(kubectl get validatingwebhookconfigurations -o json \
  | jq -r '.items[] | select(any(.webhooks[]; .name == "vfleetzone.swarmada.io")) | .metadata.name')
kubectl get validatingwebhookconfiguration "$VWC" -o json \
  | jq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp,
            .metadata.generation, .metadata.managedFields)' > "$VWC.backup.json"

# 2. Remove DELETE from the FleetZone, FleetTask, FirmwareRollout and ModelRollout checks.
jq '.webhooks |= map(if (.name | IN("vfleetzone.swarmada.io", "vfleettask.swarmada.io",
                                    "vfirmwarerollout.swarmada.io", "vmodelrollout.swarmada.io"))
                     then .rules |= map(.operations -= ["DELETE"]) else . end)' \
  "$VWC.backup.json" | kubectl replace -f -

# 3. Wait for the namespace to go.
kubectl wait --for=delete namespace/<namespace> --timeout=300s

# 4. Put the checks back. Do not skip this: until it runs, no zone, task or rollout delete
#    is checked anywhere in the cluster, and a stopped zone can be deleted with its stop.
kubectl replace -f "$VWC.backup.json"

# 5. Read the four checks back.
kubectl get validatingwebhookconfiguration "$VWC" -o json \
  | jq -r '.webhooks[] | select(.name | IN("vfleetzone.swarmada.io", "vfleettask.swarmada.io",
           "vfirmwarerollout.swarmada.io", "vmodelrollout.swarmada.io"))
           | "\(.name) failurePolicy=\(.failurePolicy) \([.rules[].operations[]] | sort | join(","))"'
```

Step 5 must print these four lines, in any order:

```
vfirmwarerollout.swarmada.io failurePolicy=Fail CREATE,DELETE,UPDATE
vfleettask.swarmada.io failurePolicy=Fail CREATE,DELETE,UPDATE
vfleetzone.swarmada.io failurePolicy=Fail CREATE,DELETE,UPDATE
vmodelrollout.swarmada.io failurePolicy=Fail CREATE,DELETE,UPDATE
```

If a line is missing, lacks `DELETE`, or does not say `failurePolicy=Fail`, the checks are not
back. Run step 4 again and repeat step 5. If the lines are still wrong, re-run the install by the
path that made it. Step 1 of *Give the configuration back to Helm*, below, prints which:

- **`Apply`, a Helm 4 release** (Helm writes it server-side): run that section's steps 1 and 2 in
  this shell. Its step 2 is the upgrade that puts the checks back. A plain `helm upgrade` fails
  after `kubectl replace` and marks the release `failed`.
- **`Update`, a Helm 3 release:** `helm upgrade` with the same values.
- **Nothing, an install made with `make deploy`** (kustomize): `make deploy`.

Then repeat step 5 until it prints the four lines. On a Helm install with
`webhooks.failurePolicy=Ignore`, the lines say `failurePolicy=Ignore`, which is what that
install chose.

On a Helm install, finish in the same shell with
[Give the configuration back to Helm](#give-the-configuration-back-to-helm), unless the recovery
above already ran it.

### Remove a ZoneMaintenance finalizer

A namespace that holds a `ZoneMaintenance` with its `swarmada.io/zonemaintenance-resume` finalizer
is not released by the steps above, and not by the manager either once the window's `FleetZone` is
gone: the `ZoneMaintenance` check refuses the update that removes the finalizer, the manager's
included. Taking the check's `UPDATE` out lets you remove the finalizer by hand. That skips the
resume of the robots the window paused; in a namespace being deleted those robots are deleted too.
While the check is out, no `ZoneMaintenance` update is checked anywhere in the cluster. Run the
steps in one shell:

```sh
# 1. Find the configuration and save it as it is.
VWC=$(kubectl get validatingwebhookconfigurations -o json \
  | jq -r '.items[] | select(any(.webhooks[]; .name == "vfleetzone.swarmada.io")) | .metadata.name')
kubectl get validatingwebhookconfiguration "$VWC" -o json \
  | jq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp,
            .metadata.generation, .metadata.managedFields)' > "$VWC.backup.json"

# 2. Remove UPDATE from the ZoneMaintenance check.
jq '.webhooks |= map(if .name == "vzonemaintenance.swarmada.io"
                     then .rules |= map(.operations -= ["UPDATE"]) else . end)' \
  "$VWC.backup.json" | kubectl replace -f -

# 3. Remove the finalizer from each ZoneMaintenance in the namespace, and wait for it to go.
kubectl -n <namespace> get zonemaintenances -o name \
  | xargs -r -I{} kubectl -n <namespace> patch {} --type=json \
      -p '[{"op":"remove","path":"/metadata/finalizers"}]'
kubectl wait --for=delete namespace/<namespace> --timeout=300s

# 4. Put the check back. Do not skip this.
kubectl replace -f "$VWC.backup.json"

# 5. Read it back.
kubectl get validatingwebhookconfiguration "$VWC" -o json \
  | jq -r '.webhooks[] | select(.name == "vzonemaintenance.swarmada.io")
           | "\(.name) failurePolicy=\(.failurePolicy) \([.rules[].operations[]] | sort | join(","))"'
```

Step 5 must print `vzonemaintenance.swarmada.io failurePolicy=Fail CREATE,UPDATE`. If it does not,
run step 4 again and repeat step 5. If it still does not, re-run the install by the path that made
it, as in the namespace procedure above: on a Helm 4 release, steps 1 and 2 of
*Give the configuration back to Helm*, below, not a plain `helm upgrade`; on a Helm 3 release, `helm upgrade` with the same values; on a `make deploy`
install, `make deploy`. On a Helm install, finish in the same shell with
[Give the configuration back to Helm](#give-the-configuration-back-to-helm), unless the recovery
above already ran it.

### Give the configuration back to Helm

`kubectl replace` in steps 2 and 4 of either procedure above takes the webhook rules it wrote
away from whoever installed them. A release that Helm writes server-side (Helm 4 installs this
way) then cannot be upgraded: every later `helm upgrade` of it fails with
`conflict occurred while applying object … ValidatingWebhookConfiguration … conflicts with
"kubectl-replace"` and marks the release `failed`, although the checks themselves are back. A
release installed with Helm 3, and an install made with `make deploy`, which applies with
`--force-conflicts`, upgrade as before. Run these in the shell that ran the procedure, so `$VWC`
is still set:

```sh
# 1. Print how Helm writes the configuration: Apply (server-side), Update (Helm 3) or nothing.
kubectl get validatingwebhookconfiguration "$VWC" --show-managed-fields \
  -o jsonpath='{range .metadata.managedFields[?(@.manager=="helm")]}{.operation}{"\n"}{end}'

# 2. Only if step 1 printed Apply: upgrade the release with its own values, taking the rules back.
REL=$(kubectl get validatingwebhookconfiguration "$VWC" \
  -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}')
REL_NS=$(kubectl get validatingwebhookconfiguration "$VWC" \
  -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-namespace}')
helm upgrade "$REL" <chart> -n "$REL_NS" --reuse-values --force-conflicts
```

`<chart>` is the chart the release runs, at the version it runs (`deploy/swarmada` in a checkout
of that version). After step 2, `helm upgrade` with the same values succeeds again without
`--force-conflicts`. Then read the checks back with step 5 of the procedure you ran.
