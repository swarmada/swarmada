# Swarmada Operations Surface — Roles, API, and swarmctl

> **Status:** Informative, non-normative. RFC-0001 is the authoritative specification for the operations surface described here.

Read this top-down: **roles first** (who exists), then the
**API + RBAC** contract (what each operation is and who may call it), then **swarmctl**
(the CLI that maps one command to one API operation).

## 1. Roles

The six built-in roles (RFC-0001 §9.5.3), in increasing privilege. `robot` and
`edge` are separate service-account roles, not part of the operator ladder.

| Role | What it can do | Typical holder |
| :-- | :-- | :-- |
| `swarmada:viewer` | Read all Swarmada resources (`get`, `list`, `watch`). Nothing else. | dashboards, monitoring |
| `swarmada:operator` | Everything `viewer` can, plus create and manage **FleetTasks**. Not robots, zones, or config. | WMS and upstream integrations, shift supervisors |
| `swarmada:fleet-manager` | Everything `operator` can, plus create and update **all fleet resources** (robots, zones, rollouts, probes, maintenance, robotclasses) and the `admit`, `reject`, and `estop-trigger` verbs. **Not** `SwarmadaConfig`; **not** `estop-clear`. | fleet operations engineers |
| `swarmada:admin` | Everything, including `SwarmadaConfig`, `estop-clear`, audit export, and CRD management. | platform team |
| `swarmada:robot` | The Fleet Adapter's service-account role: writes `status` for the robots it serves (per-message `robot_id` enforced server-side). Cannot create resources or act cross-namespace. | Fleet Adapters |
| `swarmada:edge` | The Zone Controller edge node's service-account role: reads the `FleetZone`s it guards and their `Robot`s, and writes `fleetzones/status` for one purpose — raising and clearing `status.edgeFeedUnavailable`. No task, estop, or config authority. Bind it in every namespace whose zones declare `spec.edgeNode`: **omit it and edge feed loss goes unreported**, because the write that reports it is the permission being withheld. | Zone Controller edge nodes |

**`estop-clear`, config changes, and audit export are admin-only** — automation cannot
clear an estop (RFC-0006 is the supervised exception).

## 2. API surface and RBAC (the contract)

Every operation is a Kubernetes API call on a `swarmada.io` CRD. **In Kubernetes the
API verb and the RBAC verb are the same thing** — you authorize the exact verb the
client calls — so one "Verb" column covers both. The verbs are the standard ones
(`get`, `list`, `watch`, `create`, `update`, `patch`, `delete`) plus five **custom
verbs** marked *(custom)*: `admit`, `reject`, `cancel`, `estop-trigger`, `estop-clear`.
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
    <tr><td>delete an admitted robot</td><td><code>delete</code></td><td>fleet-manager</td><td><code>delete robot &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>set a robot's charging policy</td><td><code>update</code></td><td>fleet-manager</td><td><em>(planned)</em> — today <code>kubectl patch robot &lt;n&gt; --type merge -p '{"spec":{"charging":{"dockName":"&lt;dock&gt;"}}}'</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Tasks</strong></td></tr>
    <tr><td>create a task</td><td><code>create</code></td><td>operator</td><td><code>kubectl apply -f &lt;fleetaction.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a task</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get fleetaction [-n &lt;ns&gt;]</code>, <code>describe fleetaction &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>resume a system-paused task</td><td><code>update</code></td><td>operator</td><td><em>(planned)</em> — today <code>kubectl annotate fleetaction &lt;n&gt; swarmada.io/requeue-requested="&lt;reason&gt;"</code></td></tr>
    <tr><td>cancel a task (confirmed stop → Cancelled)</td><td><code>cancel</code> <em>(custom)</em></td><td>operator</td><td><code>cancel &lt;fleetaction&gt; --reason … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a settled task (Pending or terminal only)</td><td><code>delete</code></td><td>operator</td><td><code>kubectl delete fleetaction &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Zones</strong></td></tr>
    <tr><td>create a zone</td><td><code>create</code></td><td>fleet-manager</td><td><code>kubectl apply -f &lt;fleetzone.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a zone</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get fleetzone [-n &lt;ns&gt;]</code>, <code>describe fleetzone &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a zone</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete fleetzone &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Maintenance</strong></td></tr>
    <tr><td>create maintenance</td><td><code>create</code></td><td>fleet-manager</td><td><code>kubectl apply -f &lt;zonemaintenance.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read maintenance</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get zonemaintenance [-n &lt;ns&gt;]</code>, <code>describe zonemaintenance &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>activate a scheduled one now</td><td><code>update</code></td><td>fleet-manager</td><td>creation is activation — omit <code>spec.scheduledStart</code></td></tr>
    <tr><td>end an active one (keeps the record)</td><td><code>update</code></td><td>fleet-manager</td><td><em>(planned)</em> — no operator intake; a window closes on its <code>autoResumeAfterMinutes</code> deadline, or on delete (record removed)</td></tr>
    <tr><td>remove a maintenance (finalizer resumes robots)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete zonemaintenance &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Emergency stop</strong></td></tr>
    <tr><td>estop a zone</td><td><code>estop-trigger</code> <em>(custom)</em></td><td>fleet-manager</td><td><code>estop trigger &lt;zone&gt; --reason … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>clear an estop on a zone</td><td><code>estop-clear</code> <em>(custom)</em></td><td><strong>admin</strong></td><td><code>estop-clear &lt;zone&gt; --reason … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td><em>(planned)</em> robot- and namespace-scoped estop / clear</td><td><code>estop-trigger</code> / <code>estop-clear</code> <em>(custom)</em></td><td>fleet-manager / <strong>admin</strong></td><td>not in the current release — the controllers back only zone scope today</td></tr>
    <tr><td colspan="4" align="center"><strong>Rollouts (firmware and model)</strong></td></tr>
    <tr><td>create a rollout</td><td><code>create</code></td><td>fleet-manager</td><td><code>kubectl apply -f &lt;rollout.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a rollout</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get|describe firmwarerollout|modelrollout &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td><em>(planned)</em> pause, resume, undo, or stop a rollout</td><td>—</td><td>—</td><td>not in the current release — no rollout controller carries an operator intake for these today</td></tr>
    <tr><td>delete a terminal rollout record (Succeeded/Failed only)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete firmwarerollout|modelrollout &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Model policy</strong></td></tr>
    <tr><td>create a policy</td><td><code>create</code></td><td>fleet-manager</td><td><code>kubectl apply -f &lt;modelpolicy.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a policy</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get modelpolicy [-n &lt;ns&gt;]</code>, <code>describe modelpolicy &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a policy (stops auto-deploy)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete modelpolicy &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>reset a suspended policy</td><td><code>policy-reset</code> <em>(custom)</em></td><td>fleet-manager</td><td><code>modelpolicy reset &lt;n&gt; --reason … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td><em>(planned)</em> evaluate or deploy a policy</td><td><code>update</code></td><td>fleet-manager</td><td>today <code>kubectl annotate modelpolicy &lt;n&gt; swarmada.io/model-trigger='&lt;payload&gt;'</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Probes, adapters, robotclasses, config</strong></td></tr>
    <tr><td>create a probe</td><td><code>create</code></td><td>fleet-manager</td><td><code>kubectl apply -f &lt;robotprobe.yaml&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a probe</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete robotprobe &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a robotclass (template; admitted robots keep their spec)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete robotclass &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete an adapter (deregisters; guard planned)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>kubectl delete fleetadapter &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
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

- **`swarmctl <VERB> <RESOURCE> [NAME] [--flags]`** — verb first, always; no noun-first
  forms. `get`, `describe`, `apply`, `create`, `delete`, and `edit` are the kubectl
  verbs; the Swarmada verbs (`admit`, `estop`, `cancel`, `undo`, …) use the same shape.
- **Resource is the singular CRD name** (`get robot`, `get fleetaction`) — or its
  plural, lowercased kind, or registered short name (`rob`, `fz`, `mp`). Abbreviations
  the CLI does not register (`task`, `zone`, `rollout`, `policy`, `maintenance`) do not
  resolve.
- **`get` is the only read command** — no name lists everything, a name returns one
  (`get robot` vs `get robot <name>`); there is no separate `list` command. At the API
  layer this is two Kubernetes verbs (`list` with no name, `get` with a name) plus
  `watch`; RBAC grants them together, which is why the §2 Verb column lists `get` and
  `list` even though the CLI has a single `get`.
- **Namespace** is `-n <namespace>` (alias `--namespace`), shown on every command,
  defaulting to the context namespace; `get` also takes `-A`. The only exceptions are
  the namespace-scoped estops, where the namespace is the positional target.
- **`--reason` is required and non-empty on every `estop` and `estop-clear`** — the API
  rejects an empty or whitespace-only reason; the value is written to the safety audit
  log (`ESTOP_TRIGGERED`, `ESTOP_CLEARED`).
- **`kubectl apply -f`** is the declarative path for every resource. `swarmctl` has no
  `apply` and no `create`: it is a read and lifecycle-verb tool over the same API, not a
  replacement for `kubectl`. An imperative `create <resource> --flags` is a possible
  future convenience, not a current command.
- **A named resource resolves on its own** — filter flags (`--discovered`) apply only
  to `get`, never when a resource is named.

### Robots and discovery

```
swarmctl get robot [-n <namespace>]                  # admitted robots (-A for all namespaces)
swarmctl get robot --discovered [-n <namespace>]     # pre-admission (DiscoveredRobot) robots
swarmctl describe robot <name> [-n <namespace>]      # name resolves whether admitted or discovered
swarmctl admit robot <name> --zone <leaf-zone> [--class <robotclass>] [--adapter <fleetadapter>] \
                            [--name <override>] [--dock <dock>] [--manufacturer <m>] [--model <m>] [-n <ns>]
                                                     # --adapter is REQUIRED unless --class supplies one.
                                                     # --manufacturer/--model override the discovered values.
swarmctl admit robot <name> --force --class <robotclass> [-n <namespace>]
                                                     # re-admit an already-admitted Robot: re-applies the
                                                     # RobotClass. --class is REQUIRED here; --zone and
                                                     # --name are not read on the re-admit path.
kubectl patch robot <name> --type merge -p '{"spec":{"charging":{"dockName":"<dock>"}}}' -n <namespace>
                                                     # a `swarmctl set charging` shorthand is planned, not current
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

**Admit and reject complete asynchronously.** Both verbs record the decision on the
`DiscoveredRobot` and return; the control plane then creates the `Robot` (admit) or seals
the `ROBOT_REJECTED` audit entry (reject), and removes the staging object either way. So a
successful command means *the decision is recorded*, not that the robot is already
schedulable — `get robot` is the confirmation. Two things follow from that split:

- Admitting does **not** require permission to create a `Robot`. The admission gate (§6.6)
  is the only route from discovered to schedulable, and an operator who could create Robots
  directly would be able to step around it.
- A rejection is distinguishable from a TTL sweep. Both end in the same delete, so without
  a recorded decision the audit chain could not tell an operator's refusal from a robot
  that was never admitted.

If the control plane cannot complete a marked admission (say the `--class` was deleted in
between), it records an `AdmissionFailed` event on the `DiscoveredRobot` rather than
creating a partly-configured robot.

**Reflecting column (proposed — needs a CRD field):** add `Robot.status.admission
{admittedAt, admissionCount, lastReadmittedAt}` surfaced as `ADMITTED` and `RE-ADMITS`
columns. Not in `api/v1` today — a small status-only, RA-1-safe backlog item.

### Tasks

`FleetAction` is the atomic unit of dispatch; `FleetTask` is the composite that owns a
graph of them. The commands below name the CRD kind, because the word "task" alone does
not distinguish the two and does not resolve at the CLI.

```
kubectl apply -f <fleetaction.yaml> [-n <namespace>]  # or a fleettask.yaml for the composite
swarmctl get fleetaction [-n <namespace>]
swarmctl describe fleetaction <name> [-n <namespace>]
swarmctl cancel <fleetaction> [-n <namespace>]          # no reason flag; use the annotation below to record one
kubectl annotate fleetaction <name> swarmada.io/cancel-requested="<reason>" [-n <namespace>]
                                                     # terminal: confirmed stop → Cancelled (NOT resumable)
kubectl annotate fleetaction <name> swarmada.io/requeue-requested="<reason>" [-n <namespace>]
                                                     # return a SYSTEM-paused action to Pending (estop, maintenance, preemption)
kubectl delete fleetaction <name> [-n <namespace>]   # remove a settled record — Pending or terminal (Succeeded/Failed/Cancelled) only
```
**`swarmctl` has no `FleetTask` surface today** — the composite is not registered as a
CLI resource, so read and delete it with `kubectl`. A `swarmctl resume` verb over the
requeue annotation is planned; the annotation is the operator intake the `FleetAction`
controller already reconciles.
A ready-to-apply example ships at `config/samples/fleettask_sample.yaml` — a
three-member `receiving-round` chain whose members gate on `dependsOn`:

```
kubectl apply -f config/samples/fleettask_sample.yaml -n warehouse-a
```

There is **no operator pause** — a `FleetAction` reaches `Paused` only via estop,
`ZoneMaintenance`, or preemption (§9.6.2.4). The requeue annotation un-pauses it; `cancel`
ends it. (An operator-initiated pause is a possible backlog capability, not a current
command.)

`cancel` and `delete` are **not** interchangeable. `cancel` *stops* a running task
through the confirmed-stop path (the robot is freed only once it provably stopped) and
leaves a `Cancelled` record for audit. `delete` *removes the record* and is accepted
**only** when nothing is executing — a `Pending` action or a terminal one. On an active
action (`Assigned`/`InProgress`/`Revoking`) it is rejected; `cancel` first, then delete if
you want the record gone. (Making `delete` safe on an active action needs a drain
finalizer — tracked in the backlog, not in the current release.)

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
```
the current release is **zone-scoped**: `estop trigger` and `estop-clear` act on a FleetZone via the
zone-estop controller. Robot- and namespace-scoped estops — and the `estop-clear`
`--requeue-paused-actions` / `--cancel-paused-actions` bulk options — are planned
but not implemented at v0.3.

`estop-clear` **requires a non-empty `--reason`** (swarmctl rejects an empty or
whitespace value before acting); the reason is written to the safety audit log. `estop
trigger` records its reason there too. `estop trigger` needs the `estop-trigger` verb
(fleet-manager+); `estop-clear` needs `estop-clear` (admin-only).

**What these commands do.** They send a *confirmed* stop (or clear) to every robot in
scope and log the reason. An `estop` pushes an `Estop` down each robot's `SafetyStream`;
the Fleet Adapter commands a hardware stop and confirms within 500 ms (`EstopAck =
STOPPED`) — a robot the control plane cannot confirm resolves to `Failed`/escalate,
never a false "stopped". Each affected robot's running task moves to `Paused`. A
software estop is **not** a substitute for physical safety hardware (§9.6.1).

**Worked examples.**

```
# A zone — fire alarm on floor 2 (confirmed-stops every robot in the zone):
swarmctl estop trigger floor-2 --reason "fire alarm" -n hospital-east
#   → each robot confirmed-stopped; running tasks → Paused; ESTOP_TRIGGERED audit entry (reason recorded).

# Clear the zone after inspecting:
swarmctl estop-clear floor-2 --reason "all clear after drill" -n hospital-east
#   → estop state → Normal; paused actions stay Paused (operator-gated) — requeue each with the annotation above.
```
Automation may not clear an estop (§9.6.2.3; RFC-0006 is the supervised exception).
`estop` stops the *robot*; `cancel` stops the *action* — different layers.

### Rollouts (one family — FirmwareRollout and ModelRollout)

```
kubectl apply -f <rollout.yaml> [-n <namespace>]     # FirmwareRollout or ModelRollout
swarmctl get firmwarerollout|modelrollout [-n <namespace>]
swarmctl describe firmwarerollout|modelrollout <name> [-n <namespace>]
kubectl delete firmwarerollout|modelrollout <name> [-n <namespace>]
                                                     # remove a terminal (Succeeded/Failed) record only
# (planned) pause | resume | undo | stop a rollout — see below
```
`FirmwareRollout` and `ModelRollout` are separate kinds and are named separately; there
is no single `rollout` spelling that resolves to both.

**Operator control of an in-flight rollout is planned, not in the current release.**
`pause`, `resume`, `undo` and `stop` describe the intended surface: `pause` a temporary
halt (no new robots selected) that `resume` continues, `stop` terminal — freezing robots
on their current versions and marking the rollout `Failed` while keeping the per-robot
history. None of the four is backed today: neither rollout controller carries an operator
intake for them, and `status.phase: Paused` is written by the controller alone, on
`pauseOnError`. A rollout in flight runs to completion or to its configured failure
handling. `delete` only removes a **settled** record and is accepted **only** on a
terminal rollout (`Succeeded`/`Failed`), rejected while `Progressing`/`Paused`.

**Where the artifact lives.** `--uri` sets the CRD's `firmwareUri` / `modelUri` — the
location of the binary. Supported schemes: `oci://<registry>/<repo>@sha256:<digest>`
(or `:tag`) and `https://…`. **Firmware** requires `--checksum` (`firmwareChecksum`) and
may set `--signature-ref` (`firmwareSignatureRef`: a `bundle:`/`https://`/`oci://` ref
verified against `SwarmadaConfig.spec.signing.trustRoots`, fail-closed). **Model**
artifact checksum/signature is verified adapter-side (§9.2.8), so ModelRollout carries
no checksum flag; `--model-name` sets `modelName`. The adapter fetches the artifact from
the URI, verifies it, then installs.

**Why `--model-name` (models have it, firmware doesn't).** A robot runs several named
models at once (`item-recognition`, a grasp policy, …); `modelName` is the stable
identity used to track which installed model this rollout upgrades and to gate the
capabilities that model provides (§6.10.2). Firmware is singular per robot, so
`newVersion` + `firmwareUri` suffice — no name.

**Future (backlog, not in the current release): derive descriptors from a signed model manifest.**
`modelName`, `newVersion`, `requiredHardware`, and `grantsCapabilities` can be read from
the artifact's own metadata (OCI labels / a model manifest) so the operator supplies
only `--uri` and `--selector` — eliminating hand-transcription errors. Hard rule: the
metadata is read **only from the signed payload, after signature verification** against
`SwarmadaConfig.spec.signing.trustRoots` (read-after-verify, never before), so a hostile
artifact cannot declare its own capabilities. An explicit flag still overrides the
artifact value. Tracked as an RFC-scoped enhancement.

### Model policy

```
kubectl apply -f <modelpolicy.yaml> [-n <namespace>]
swarmctl get modelpolicy [-n <namespace>]
swarmctl describe modelpolicy <name> [-n <namespace>]
swarmctl modelpolicy reset <name> --reason "<text>" [-n <namespace>]
                                                     # clears a FailedRepeatedly suspension (custom verb: policy-reset)
kubectl annotate modelpolicy <name> swarmada.io/model-trigger='<metrics-json>' [-n <namespace>]
                                                     # (planned as `evaluate`/`deploy`) the single evaluation path
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
kubectl delete fleetadapter <name> [-n <namespace>]  # deregisters the adapter — robots it serves go unmanaged until it reconnects
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
and tamper-evident (`verify audit` recomputes its hash chain); it has no `delete` verb by
design, so the record cannot be edited or truncated. `delete adapter` is the one delete
that touches live operation — see the caveat above; a guard is planned so it is rejected
while the adapter is actively serving robots.

**What a probe is for.** Passive telemetry reports what a robot *says*; a `RobotProbe`
makes the control plane *actively verify* a hardware component, capability, or model via
the adapter's `Verify*` RPCs — catching **silent failures** telemetry misses (a camera
that reports Healthy while dropping 60% of frames). Probes are debounced
(`failureThreshold`/`recoveryThreshold`) so noise doesn't flap capabilities.

*Example.* `create probe cam-liveness --robot-selector class=acme --type hardware
--target camera-front --interval 30`. Every 30s the control plane verifies `camera-front`
(e.g. frame-rate ≥ 80%); after 3 bad cycles it goes `Degraded` → the `item-pick.ai-guided`
capability (needs a healthy camera) stops being schedulable → the scheduler stops sending pick
tasks to that robot **before** it fails one; two good cycles later it recovers. (The capability
status written in that state is `Inactive`. RFC-0001's truth table specifies `Unavailable`; the
control plane does not write that value at v0.3, so tooling should match on `Inactive`.)

### Miscellaneous

```
swarmctl version                                     # client version and the contract version it targets
swarmctl robotclass rollout <class> [-n <ns>]        # re-admit every Robot of a class after a class change
swarmctl modelpolicy reset <policy> --reason "<text>" [-n <ns>]
                                                     # clear a suspended ModelPolicy (--reason required)
```

`--yes` skips the interactive confirmation on every command that prompts (`admit --force`,
`delete`, `cancel`, `estop`, `robotclass rollout`). Use it only in automation; the prompt exists
because each of these commands changes robot state.

### Audit (safety audit log)

```
swarmctl get audit    [--file <chain>] [-o yaml|json]       # print the chain
swarmctl verify audit [--file <chain>]                      # recompute the hash chain; non-zero on tamper
swarmctl export audit [--file <chain>] [--out-file <file>]  # re-emit as JSONL (admin-only)
```
Today these operate on an **exported** chain (newline-delimited JSON from `--file` or
stdin), so they compose: `swarmctl export audit | swarmctl verify audit`. The
server-side read API — with `--since` / `--type` filters and live `-n <namespace>`
scope — is planned. (`export audit` uses `--out-file`; the persistent `-o/--output`
selects table/yaml/json.)
**What it's for.** The **hash-chained, tamper-evident** safety audit log records every
safety- and security-relevant event (estop trigger/clear, admit/reject, model
deploy/reject, config change) with structured fields — timestamp, identity, resource,
reason, outcome. Hash-chaining means an entry can't be altered or deleted without
breaking the chain.

*Example.* `estop trigger floor-2 --reason "fire alarm"` writes `ESTOP_TRIGGERED{scope,
triggered_by, reason, ts}`. Incident review: `get audit` shows who stopped what, when,
and why (`--since` / `--type` filters are planned). For a safety case, `verify audit`
recomputes the chain to prove nothing was tampered with; `export audit` (admin-only)
hands a regulator the record. This is why `--reason` is required on `estop-clear` and
recorded on every `estop trigger`.
