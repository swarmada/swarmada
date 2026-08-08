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
    <tr><td>set a robot's charging policy</td><td><code>update</code></td><td>fleet-manager</td><td><code>set charging &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Tasks</strong></td></tr>
    <tr><td>create a task</td><td><code>create</code></td><td>operator</td><td><code>create task … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a task</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get task [-n &lt;ns&gt;]</code>, <code>describe task &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>resume a system-paused task</td><td><code>update</code></td><td>operator</td><td><code>resume task &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>cancel a task (confirmed stop → Cancelled)</td><td><code>cancel</code> <em>(custom)</em></td><td>operator</td><td><code>cancel task &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a settled task (Pending or terminal only)</td><td><code>delete</code></td><td>operator</td><td><code>delete task &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Zones</strong></td></tr>
    <tr><td>create a zone</td><td><code>create</code></td><td>fleet-manager</td><td><code>create zone … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a zone</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get zone [-n &lt;ns&gt;]</code>, <code>describe zone &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a zone</td><td><code>delete</code></td><td>fleet-manager</td><td><code>delete zone &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Maintenance</strong></td></tr>
    <tr><td>create maintenance</td><td><code>create</code></td><td>fleet-manager</td><td><code>create maintenance … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read maintenance</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get maintenance [-n &lt;ns&gt;]</code>, <code>describe maintenance &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>activate a scheduled one now</td><td><code>update</code></td><td>fleet-manager</td><td><code>activate maintenance &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>end an active one (keeps the record)</td><td><code>update</code></td><td>fleet-manager</td><td><code>deactivate maintenance &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>remove a maintenance (finalizer resumes robots)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>delete maintenance &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Emergency stop</strong></td></tr>
    <tr><td>estop a zone</td><td><code>estop-trigger</code> <em>(custom)</em></td><td>fleet-manager</td><td><code>estop trigger &lt;zone&gt; --reason … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>clear an estop on a zone</td><td><code>estop-clear</code> <em>(custom)</em></td><td><strong>admin</strong></td><td><code>estop-clear &lt;zone&gt; --reason … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td><em>(planned)</em> robot- and namespace-scoped estop / clear</td><td><code>estop-trigger</code> / <code>estop-clear</code> <em>(custom)</em></td><td>fleet-manager / <strong>admin</strong></td><td>not in the current release — the controllers back only zone scope today</td></tr>
    <tr><td colspan="4" align="center"><strong>Rollouts (firmware and model)</strong></td></tr>
    <tr><td>create a rollout</td><td><code>create</code></td><td>fleet-manager</td><td><code>create rollout … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a rollout</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get rollout &lt;n&gt; [-n &lt;ns&gt;]</code>, <code>describe rollout &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>pause, resume, undo, or stop a rollout</td><td><code>update</code></td><td>fleet-manager</td><td><code>pause|resume|undo|stop rollout &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a terminal rollout record (Succeeded/Failed only)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>delete rollout &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Model policy</strong></td></tr>
    <tr><td>create a policy</td><td><code>create</code></td><td>fleet-manager</td><td><code>apply -f … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a policy</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get policy &lt;n&gt; [-n &lt;ns&gt;]</code>, <code>describe policy &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a policy (stops auto-deploy)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>delete policy &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>evaluate, deploy, reset, or set a policy</td><td><code>update</code></td><td>fleet-manager</td><td><code>evaluate|deploy|reset|set policy &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td colspan="4" align="center"><strong>Probes, adapters, robotclasses, config</strong></td></tr>
    <tr><td>create a probe</td><td><code>create</code></td><td>fleet-manager</td><td><code>create probe … [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a probe</td><td><code>delete</code></td><td>fleet-manager</td><td><code>delete probe &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete a robotclass (template; admitted robots keep their spec)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>delete robotclass &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>delete an adapter (deregisters; guard planned)</td><td><code>delete</code></td><td>fleet-manager</td><td><code>delete adapter &lt;n&gt; [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read a probe, adapter, or robotclass</td><td><code>get</code>, <code>list</code></td><td>viewer</td><td><code>get probe|adapter|robotclass [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>read config</td><td><code>get</code></td><td>viewer</td><td><code>get config [-n &lt;ns&gt;]</code></td></tr>
    <tr><td>change config</td><td><code>update</code></td><td><strong>admin</strong></td><td><code>set config --… [-n &lt;ns&gt;]</code></td></tr>
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
- **Resource is the singular noun** (`get robot`, `get task`) — no plurals.
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
- **`apply -f`** is the declarative path for every resource; **`create <resource>
  --flags`** is the imperative alternative where the spec reduces to flags.
- **A named resource resolves on its own** — filter flags (`--discovered`) apply only
  to `get`, never when a resource is named.

### Robots and discovery

```
swarmctl get robot [-n <namespace>]                  # admitted robots (-A for all namespaces)
swarmctl get robot --discovered [-n <namespace>]     # pre-admission (DiscoveredRobot) robots
swarmctl describe robot <name> [-n <namespace>]      # name resolves whether admitted or discovered
swarmctl admit robot <name> --zone <leaf-zone> [--class <robotclass>] [--name <override>] [--force] [-n <namespace>]
swarmctl set charging <name> [--dock <dock>] [--min-pct <n>] [--target-pct <n>] [-n <namespace>]
swarmctl delete robot <name> [--reason "<text>"] [-n <namespace>]
                                                     # discovered ⇒ reject (reason recorded); admitted ⇒ delete
```
**Admit and re-admit are the same command** — re-admit is just `admit robot` with
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
  that was simply never admitted.

If the control plane cannot complete a marked admission (say the `--class` was deleted in
between), it records an `AdmissionFailed` event on the `DiscoveredRobot` rather than
creating a partly-configured robot.

**Reflecting column (proposed — needs a CRD field):** add `Robot.status.admission
{admittedAt, admissionCount, lastReadmittedAt}` surfaced as `ADMITTED` and `RE-ADMITS`
columns. Not in `api/v1` today — a small status-only, RA-1-safe backlog item.

### Tasks

```
swarmctl create task <name> --type <Navigate|PickUp|DropOff|Patrol|Charge|Custom> --zone <zone> \
        [--capability <cap>]… [--priority Critical|High|Normal|Low] [--payload '<json>'] \
        [--deadline <rfc3339>] [--timeout <sec>] [--robot <name>] [-n <namespace>]   # imperative
swarmctl apply -f <fleettask.yaml> [-n <namespace>]  # declarative
swarmctl get task [-n <namespace>]
swarmctl describe task <name> [-n <namespace>]
swarmctl resume task <name> [-n <namespace>]         # resume a task the SYSTEM paused (estop, maintenance, preemption)
swarmctl cancel task <name> [-n <namespace>]         # terminal: confirmed stop → Cancelled (NOT resumable)
swarmctl delete task <name> [-n <namespace>]         # remove a settled record — Pending or terminal (Succeeded/Failed/Cancelled) only
```
There is **no operator `pause task`** — a task reaches `Paused` only via estop,
`ZoneMaintenance`, or preemption (§9.6.2.4). `resume` un-pauses; `cancel` ends it. (An
operator-initiated pause is a possible backlog capability, not a current command.)

`cancel` and `delete` are **not** interchangeable. `cancel` *stops* a running task
through the confirmed-stop path (the robot is freed only once it provably stopped) and
leaves a `Cancelled` record for audit. `delete` *removes the record* and is accepted
**only** when nothing is executing — a `Pending` task or a terminal one. On an active
task (`Assigned`/`InProgress`/`Revoking`) it is rejected; `cancel` first, then delete if
you want the record gone. (Making `delete` safe on an active task needs a drain
finalizer — tracked in the backlog, not in the current release.)

### Zones and maintenance

```
swarmctl create zone <name> [--parent <zone>] [--display-name "<text>"] [--floor <n>] [--max-robots <n>] [-n <namespace>]
                                                     # complex physicalBounds geometry ⇒ apply -f
swarmctl apply -f <fleetzone.yaml> [-n <namespace>]
swarmctl get zone [-n <namespace>]
swarmctl describe zone <name> [-n <namespace>]
swarmctl delete zone <name> [-n <namespace>]
swarmctl create maintenance --zone <zone> [--mode Graceful|Immediate] [--at <rfc3339>] [--reason "<text>"] [-n <namespace>]
                                                     # omit --zone ⇒ namespace-wide; --at schedules
swarmctl apply -f <zonemaintenance.yaml> [-n <namespace>]
swarmctl get maintenance [-n <namespace>]
swarmctl describe maintenance <name> [-n <namespace>]
swarmctl activate maintenance <name> [-n <namespace>]        # activate a Scheduled one now
swarmctl deactivate maintenance <name> [-n <namespace>]      # end an Active one → Completed (record kept)
swarmctl delete maintenance <name> [-n <namespace>]          # remove; finalizer resumes robots (also cancels a Scheduled one)
```

### Emergency stop

```
swarmctl estop trigger <zone> --reason "<text>" [-n <namespace>]
swarmctl estop-clear <zone> --reason "<text>" [-n <namespace>]
```
the current release is **zone-scoped**: `estop trigger` and `estop-clear` act on a FleetZone via the
zone-estop controller. Robot- and namespace-scoped estops — and the `estop-clear`
`--requeue-paused-tasks` / `--cancel-paused-tasks` task-handling options — are planned
but not yet implemented.

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
#   → estop state → Normal; paused tasks stay Paused (operator-gated) — resume with `resume task`.
```
Automation may not clear an estop (§9.6.2.3; RFC-0006 is the supervised exception).
`estop` stops the *robot*; `cancel task` stops the *task* — different layers.

### Rollouts (one family — FirmwareRollout and ModelRollout)

```
swarmctl create rollout <name> --type firmware|model --selector <labels> --version <v> \
        --uri <artifact-uri> [--checksum <sha256>] [--signature-ref <ref>] [--model-name <name>] \
        [--max-unavailable <n|pct>] [--min-battery <pct>] [--idle-only] [--rollback Manual|Auto] [-n <namespace>]
swarmctl apply -f <rollout.yaml> [-n <namespace>]
swarmctl get rollout <name> [-n <namespace>]
swarmctl describe rollout <name> [-n <namespace>]
swarmctl pause rollout <name> [-n <namespace>]
swarmctl resume rollout <name> [-n <namespace>]
swarmctl undo rollout <name> [-n <namespace>]
swarmctl stop rollout <name> [-n <namespace>]        # terminal: freeze robots on current versions → Failed (not resumable)
swarmctl delete rollout <name> [-n <namespace>]      # remove a terminal (Succeeded/Failed) record only
```
`--type firmware|model` disambiguates only if a name is ambiguous. There is no
`modelrollout` command family.

`pause` and `stop` are the halt pair: `pause` is a **temporary** halt (no new robots
selected; `resume` continues it), `stop` is **terminal** — it freezes robots on their
current versions, marks the rollout `Failed`, and is not resumable, while keeping the
per-robot history and leaving `undo` available. To halt an in-flight rollout use `pause`
or `stop`, not `delete`: `delete` only removes a **settled** record and is accepted
**only** on a terminal rollout (`Succeeded`/`Failed`), rejected while `Progressing`/`Paused`.

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
swarmctl apply -f <modelpolicy.yaml> [-n <namespace>]
swarmctl get policy <name> [-n <namespace>]
swarmctl describe policy <name> [-n <namespace>]
swarmctl evaluate policy <name> [-n <namespace>]
swarmctl deploy policy <name> [-n <namespace>]
swarmctl reset policy <name> [-n <namespace>]
swarmctl set policy <name> --<field>=<value> [-n <namespace>]
swarmctl delete policy <name> [-n <namespace>]       # stops auto-deploy; in-flight rollouts already created are unaffected
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
`consecutiveRejectionLimit` rejections the policy suspends until `reset policy`.
`evaluate policy` forces a run; `deploy policy` is the manual path.

### Probes, adapters, robotclasses, config

```
swarmctl create probe <name> --robot-selector <labels> --type hardware|capability|model --target <name> [--interval <sec>] [-n <namespace>]
swarmctl get probe | adapter | robotclass [-n <namespace>]
swarmctl describe probe <name> | adapter <name> | robotclass <name> [-n <namespace>]
swarmctl delete probe <name> [-n <namespace>]        # stops the active check; nothing to drain (plain delete)
swarmctl delete robotclass <name> [-n <namespace>]   # removes the template; already-admitted robots keep their merged spec
swarmctl delete adapter <name> [-n <namespace>]      # deregisters the adapter — robots it serves go unmanaged until it reconnects
swarmctl get config [-n <namespace>]
swarmctl set config --<field>=<value> [-n <namespace>]
```
`RobotClass`, `ModelPolicy`, and `FleetAdapter` have no imperative `create` — their
specs (hardware inventory, quality gates, trust material) don't reduce to flags, so
they are authored with `apply -f`. Every resource still supports `apply -f`.

Two resources have **no `delete`** on purpose. `SwarmadaConfig` is a namespace singleton
the control plane auto-creates with defaults — deleting it is a no-op (it is recreated),
so change it with `set config`, never delete it. The **safety audit log** is append-only
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
capability (needs a healthy camera) goes `Unavailable` → the scheduler stops sending pick
tasks to that robot **before** it fails one; two good cycles later it recovers.

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
