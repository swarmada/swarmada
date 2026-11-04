# Zone-restriction refusals: five control-plane reasons against four adapter refusals

**Status:** reference, v0.4. Decided by
[ADR-0058](adr/0058-zone-restrictions-are-one-model-across-three-axes.md) §9. The rules it
cites are amendments that RFC-0001's assembled text does not contain; they are published in
[the rule index](rule-index.md#rules-staged-for-rfc-0001).

## Why this document exists

Refusal wording was not invented for this specification. An adapter for a household cleaning
robot, written before it, already distinguished four refusal kinds — `unknown-zone`, `off-limits`,
`no-mop` and `unconfirmed` — by a machine-readable `kind`, so a surface can tell them apart
without parsing prose. Each is stated in the tables below in enough detail to be read without that
adapter. The control plane now has five `failureReason` values for
overlapping conditions.

Two vocabularies describing the same refusal will drift unless someone says which one governs
what. This document says it.

## The two readers are different, and that is the whole point

**They are not competing wordings of one sentence.** They are addressed to different people, at
different moments, with different power to act:

| | Control-plane `failureReason` | Adapter refusal `kind` |
| :-- | :-- | :-- |
| Reader | An operator reading `kubectl describe fleetaction`, or a dashboard | A person standing in a room asking why the robot is not moving |
| Moment | The action was not dispatched | The request they made was declined |
| Surface | `status.failureReason` + `status.message` | A sentence in a chat reply or an app |
| Can act by | Editing a `FleetZone`, or the action | Asking for something different, or moving furniture |

A refusal that reads well on one surface reads badly on the other. *"ZoneCapabilityDenied:
capability `clean.mop` is denied in zone `office` by zone `office`"* is exactly right in a status
field and useless to a household. *"I can vacuum the office instead — it is the carpet I must not
get wet"* is exactly right to a household and unparseable by a dashboard.

**So neither is rewritten into the other's voice.** The adapter's four were designed for the person
standing next to the robot and are not to be restated in the control plane's register.

## Authority, per condition

The rule is one sentence: **the control plane is authoritative for the CLASSIFICATION; the
adapter is authoritative for the SENTENCE it shows its own user.** Where both can refuse the same
condition, the adapter's refusal cites the control-plane reason so the two can be correlated in an
incident, and the control plane's message carries no assumption about how the adapter phrases it.

| Condition | Control plane | Adapter | Authoritative for the classification | Notes |
| :-- | :-- | :-- | :-- | :-- |
| The named zone does not exist | `ZoneUnknown` | `unknown-zone` | **Control plane** | Both enumerate what does exist rather than guessing. Ambiguity asks; it never guesses. The adapter's list is the areas *it* can reach, which is a subset — it is the more useful list for its reader and is not a contradiction |
| Entry to the zone is forbidden | `ZoneEntryDenied` | `off-limits` | **Control plane** | The adapter's wording carries the absorbing property verbatim — *"on its own or as part of anything else"* — and the control-plane message carries it in meaning, because that clause is what stops a household believing a bundled request quietly included the forbidden room. The adapter additionally carries its divergence notice (an app-marked no-go does not reach the device over Matter); that notice is **adapter-only** and must not migrate, because it is a fact about one vendor's stack |
| The zone is open but this job is not | `ZoneCapabilityDenied` | `no-mop` | **Control plane** | Both MUST say the zone is not closed — one capability is. The adapter goes further and **offers the way through** (*"ask me to clean it with Quick"*); that is a property of knowing the device's presets and stays adapter-side. A control plane that invented an alternative would be guessing at hardware |
| The rule cannot be worked out | `ZoneRestrictionsUnresolved` | `unconfirmed` | **Neither — they are different conditions** | This is the one row where the mapping in ADR-0058 §9 is a *resemblance*, not an identity, and it is worth being precise. `ZoneRestrictionsUnresolved` means the zone chain could not be READ. `unconfirmed` means an area exists on the robot's map but its binding was never witnessed. Both hold rather than judge, and both are fail-closed; they are not the same fact and a surface must not collapse them |
| No robot here can honour the rule | `ZoneOperatingConstraintUnmet` | *(no counterpart)* | **Control plane** | New. Axis 3 has no adapter-side equivalent because the adapter's axis 2 is `no-mop`. **Implemented at v0.4** (candidate filter 12) — see below |
| Part of the request is undeliverable today | *(no counterpart)* | `undeliverable` | **Adapter** | The fifth adapter behaviour, and the one most worth importing: a partly-undeliverable bundle is *dispatched* with the exclusion **named before the human confirms**. The control-plane analogue is the narrowed leaf set on `status.message`, not a refusal — the action proceeds against the remainder. An **off-limits** member is different and refuses the whole request: a standing decision refuses; a fact about today narrows and says so |

## The split that matters more than the wording

**Terminal versus Pending**, and it is carried identically on both sides:

- `ZoneEntryDenied` and `ZoneCapabilityDenied` → **Failed**. Standing decisions that no retry and
  no fleet change will satisfy. `retryCount` is NOT incremented: nothing about retrying changes a
  standing rule, and inflating the count makes an operator read a policy refusal as a flaky robot.
- `ZoneOperatingConstraintUnmet` and `ZoneRestrictionsUnresolved` → **Pending**, failing on the
  action's deadline like any other unsatisfied wait. Facts about today: a conforming adapter
  deployed tomorrow, or a `FleetZone` that becomes readable, makes the same action dispatchable.
- `ZoneUnknown` → **Failed**.

Collapsing any two of the five costs the operator the difference between *there is no such zone*,
*you told me not to go there*, *the zone is open but this job is not*, *no robot here can honour
the rule*, and *I could not work out what the rule is*. Those are five different next actions.

## What a refusal must not claim

`ZoneEntryDenied` means **the action was not dispatched**. It does not mean no robot will enter
the zone. A control-plane refusal cannot stop a robot driven by its own vendor application, its
own buttons, its own schedule, or a person carrying it — and nothing in a control plane can
physically switch off a camera.

An operator surface that renders a denied zone as **protected** rather than as **not dispatched
to** is asserting a property this specification does not provide. The adapter's wording is
careful about this already (*"a no-mop rule is a dispatch policy, never a guarantee about the
floor"*), and the control plane's messages say the same thing in their own register.

### The same applies to the constraint push, and the difference is narrow and real

Since ADR-0059 the control plane also **tells a robot what may be running where it now is**, on a
`zone_constraints` Command, whenever its zone containment changes or a containing zone's rules are
edited. What changes is that an adapter which *wants* to comply with FA-R3 — *"honour every one of
them for the whole time the robot is in the zone, including while only passing through"* — now can:
before this, it was sent the rules for one assignment target, once, and had nothing at all for any
other zone it entered.

That is worth a lot. It is **not** a guarantee, and four limits are stated rather than left to be
discovered. Each is stated with the seam that would close it, because a limit named without its
remedy is a disclaimer and a limit named with one is a task.

| Limit | What it means | What would close it |
| :-- | :-- | :-- |
| **The rule arrives after the robot crosses.** The loop is telemetry-up then command-down | The push does not make a zone safe; it makes FA-R3 satisfiable. A robot is inside a restricted zone, with whatever it was running still running, for at least one telemetry interval plus one round trip | Adapter-local evaluation, which needs geometry on the wire — an additive `zone_geometry` arm on `ZoneConstraints`. ADR-0059 §4 records why that was not taken and what it would be for |
| **A zone with no site-aligned polygon has no containment answer.** `ZoneGeometryAbsent` and `FrameLocal` never appear in `containingZones` | Robots inside such a region are governed by the assignment path alone: filters 11 and 12 still judge an action that TARGETS the zone, and nothing is pushed to a robot that only walks in. `GeometryAvailable: False` already reports it, per zone, with the reason | A frame-alignment answer would let a `FrameLocal` zone participate with no other change. `ZoneGeometryAbsent` cannot be closed by one: a zone that declares no shape has no shape |
| **There is no time dimension.** A rule is on or off and changes only when a principal edits `spec` | *"No camera between 22:00 and 06:00"* is inexpressible. The model is edge-triggered on containment and on `spec` edits, and on nothing else — there is no clock in it | An optional window field whose ABSENCE keeps today's meaning (a rule with no window is always in force), plus a wake-up, which is the part that must be decided deliberately rather than inherited |
| **An acknowledgement is a claim, not a measurement** | The plane records that an adapter said it applied the set. It cannot verify it, and a surface rendering an ack as *enforced* is claiming a property this contract does not provide | Open. `ZoneConstraintsAck.unhonourable_subjects` reports what an adapter says it cannot honour where the robot already is — **reportable and, unlike the same list on a declined assignment, not preventable** |

**And the push never cancels work.** The robot is told what it may run; nothing takes its action
away. An adapter that honours the pushed set has stopped the camera without anyone stopping the
robot, which is the better outcome as well as the smaller claim (ADR-0059 §7).

## Implementation status at v0.4

| Reason | Producer | State |
| :-- | :-- | :-- |
| `ZoneUnknown` | candidate filter 11 | **implemented** |
| `ZoneEntryDenied` | candidate filter 11 | **implemented** (target, ancestor, and subtree-exhausted) |
| `ZoneCapabilityDenied` | candidate filter 11 | **implemented** |
| `ZoneRestrictionsUnresolved` | candidate filter 11 | **implemented**, including the fail-closed read error |
| `ZoneOperatingConstraintUnmet` | candidate filter 12 | **implemented** |

Filter 12 reads `Robot.status.honouredOperatingConstraints[]`, projected from
`CapabilitiesSnapshot.honoured_operating_constraints`; both are on the wire, the control plane
carries the resolved set on `AssignAction`, and an adapter that cannot honour a subject declines
naming it.

**Restrictions are not time-bounded in this draft.** No refusal above depends on the time of day
or on a date, because a restriction carries no time window, schedule or expiry. It applies until
someone edits or removes it, so a restriction that should apply only at certain times is changed
by editing the `FleetZone` when that time comes.

**Beside the five refusals there is now one push**, which is not a refusal and is listed here so the
two are not confused. `zone_constraints` (FA-R7, ADR-0059) tells a robot what may be running where it
NOW is — the union of the rules in force across every zone whose polygon contains it, sent when that
answer changes. A refusal says work was not dispatched; the push says what a robot already present
may have running. Neither stops a robot.
