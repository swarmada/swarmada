# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The report model: check results, profile results, and the ConformanceReport.

``StrEnum`` below is defined here rather than imported from ``enum``.
``enum.StrEnum`` arrived in Python 3.11, and THIS MODULE HAS TO IMPORT ON 3.10:
the robot's vendor container is Python 3.10.12, it is where the adapter harness
runs because DDS discovery does not cross a virtual-machine boundary to a LAN
host, and the plant declaration this module grades a run against lives on the
robot. The project's own floor (``requires-python``) is 3.12 and stays there --
numpy needs it -- so ``conformance/`` is the one package held lower, and
``tests/python/test_conformance_runs_on_the_harness_floor.py`` is the gate that
keeps it there. The shim keeps ``enum.StrEnum`` semantics (``str(member)`` is the
value), which this module's callers depend on; the sibling model in
``adapters/conformance/report.py`` deliberately keeps the plain ``Enum``
spelling of ``str()`` and says so on its own classes.

Deliberately independent of ``adapters.conformance.report``. That module is the
Fleet Adapter harness's own model and stays the single home of the ``C0``-``C16``
run; this one consumes the JSON it emits (see ``adapter.py``) rather than
importing it, so a ``ComputeEndpoint`` run has no reason to load an adapter
harness and the two can move at different speeds.
"""

from __future__ import annotations

import enum
import hashlib
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


class StrEnum(str, enum.Enum):  # noqa: UP042 -- 3.10 has no enum.StrEnum; see the module docstring
    """``enum.StrEnum`` on every interpreter this package runs on, 3.10 included.

    Spelled once, unconditionally, rather than as a version test that selects the
    stdlib class above 3.10: a version test means two behaviours, and the class of
    defect this repository keeps finding is exactly the one that is green on the
    developer's interpreter and red on the robot's. ``__str__`` and ``__format__``
    are the two members ``(str, Enum)`` inherits from ``Enum`` rather than ``str``,
    and they are what makes the plain mix-in print ``CheckStatus.PASS`` where
    ``enum.StrEnum`` prints ``pass``.
    """

    __str__ = str.__str__
    __format__ = str.__format__


class CheckStatus(StrEnum):
    PASS = "pass"
    FAIL = "fail"
    SKIP = "skip"


class SkipCause(StrEnum):
    """Why a documented check was not exercised.

    Enumerated rather than free text for the reason ``CE0.1`` states: a prose
    justification is not falsifiable. A cause recorded here can be contradicted
    by the run that recorded it, and that contradiction is itself a check.
    """

    #: The property lives inside the implementation and produces no observable
    #: effect on any wire this suite can watch.
    NOT_OBSERVABLE_ON_WIRE = "not-observable-on-wire"
    #: The surface exists in the specification and not yet in the tree, so there
    #: is nothing to drive. NEVER a pass: a specified-and-unimplemented
    #: obligation is unestablished, not satisfied.
    SURFACE_NOT_IMPLEMENTED = "surface-not-implemented"
    #: The implementation declined an OPTIONAL feature, which is conformant.
    OPTIONAL_DECLINED = "optional-declined"
    #: The implementation reached the state an OPTIONAL (MAY) behaviour applies to and did
    #: not exhibit it, which the MAY permits (C10.1: an accepted install with no advisory
    #: progress). Not a decline, since nothing was declined, and not an unmet obligation,
    #: since a MAY sets none. Graded like a decline: the feature is observed absent. Kept OUT
    #: of NOT_APPLICABLE_CAUSES for the reason OPTIONAL_DECLINED is. The behaviour applied
    #: and the run saw it not taken; it was never inapplicable.
    OPTIONAL_NOT_EXHIBITED = "optional-not-exhibited"
    #: The check belongs to a BINDING the target did not declare.
    BINDING_NOT_DECLARED = "binding-not-declared"
    #: The check belongs to a CONTRACT the target did not declare. Distinct from
    #: the binding cause on purpose: "binding-not-declared: the endpoint declares
    #: contract: App" is a contradiction in terms, and a skip reason that cannot
    #: be read literally is the beginning of a skip reason that is simply wrong.
    CONTRACT_NOT_DECLARED = "contract-not-declared"
    #: Needs a resident (Service-contract) target; no resident fixture is in tree.
    NEEDS_RESIDENT_TARGET = "needs-resident-target"
    #: Needs a live cluster (an API server, admission, a running controller).
    NEEDS_LIVE_CLUSTER = "needs-live-cluster"
    #: The target did not reach the state the check needs.
    TARGET_DID_NOT_REACH_STATE = "target-did-not-reach-state"
    #: The implementation satisfied the requirement by another route the
    #: specification permits, leaving this check unreachable -- so it was never
    #: an obligation of THIS implementation (ADR-0048 rule B). The class the
    #: CE6.2/CE6.3 mutually exclusive pair belongs to, named for the class
    #: rather than for any one check.
    LEGAL_ALTERNATIVE_PATH = "legal-alternative-path"
    #: A deployment property (TLS material, process supervision) this harness
    #: does not reproduce.
    DEPLOYMENT_PROPERTY = "deployment-property"
    #: Would need several forced faults or a long run to observe a curve.
    NEEDS_EXTENDED_RUN = "needs-extended-run"
    #: Needs the harness to terminate and respawn the target PROCESS.
    NEEDS_TARGET_RESTART = "needs-target-restart"
    #: The target streamed no TelemetryPayload during the run, so nothing about
    #: telemetry content or cadence can be asserted. Could-not-ESTABLISH, so it
    #: is NOT in NOT_APPLICABLE_CAUSES -- the obligation stands, unverified.
    NO_TELEMETRY_IN_RUN = "no-telemetry-in-run"
    #: Needs a reconnect in which registration SUCCEEDS; this suite's only
    #: reconnect refuses registration by design (the contract-version path).
    #: Also could-not-establish.
    REQUIRES_SUCCESSFUL_REREGISTRATION = "requires-successful-reregistration"
    #: Every core operating subject is one the adapter DECLARED it honours, so the
    #: run could construct no rule it cannot honour and C17.1 had nothing to probe
    #: (FA-R5). COULD-NOT-ESTABLISH, so it is deliberately NOT in
    #: NOT_APPLICABLE_CAUSES: the decline obligation still stands for a subject
    #: this run happened not to be able to send, and reporting the check as
    #: not-applicable would say the adapter was never held to it.
    ALL_CORE_SUBJECTS_HONOURED = "all-core-subjects-honoured"
    #: The adapter declared NO honoured operating subject, so the run could construct
    #: no rule it CLAIMS it can honour and C17.4 had nothing to probe (FA-R2 arrival).
    #: The exact mirror of the cause above, and treated the same way: COULD-NOT-
    #: ESTABLISH, deliberately NOT in NOT_APPLICABLE_CAUSES. Applicability of the
    #: whole `zone-restrictions` profile is decided by its own `conditional` in
    #: profiles.yaml, which is the right place for it; a skip cause that ALSO
    #: declared the check not-applicable would be a second answer to that question,
    #: and the two would drift. Spelled with "adapter" rather than "target" because
    #: the obligation is an adapter one -- no ComputeEndpoint or control plane
    #: declares honoured operating subjects -- so there is no generic form to name.
    ADAPTER_DECLARES_NO_OPERATING_SUBJECT = "adapter-declares-no-operating-subject"
    #: The adapter declared NO sensor stream, so there is nothing to activate and the
    #: duty cycle (C18) has no subject. NOT-APPLICABLE, and unlike the two causes above
    #: it genuinely is: those are obligations the run could not construct a probe for,
    #: whereas this one has no antecedent at all. C18's rules are all of the form "when
    #: a stream is activated ...", and a robot that offers no stream is never in that
    #: case -- there is no unverified obligation left standing behind the skip.
    #: Distinct from OPTIONAL_DECLINED, which means the adapter HAS streams and refuses
    #: to drive them: that leaves the duty cycle unexercised on a robot that could have
    #: been asked, which is a different fact.
    ADAPTER_DECLARES_NO_SENSOR_STREAM = "adapter-declares-no-sensor-stream"
    #: The adapter declared NO SupportedAction, so no assignment could carry a type it
    #: claims to serve. Obligations stand: an adapter that serves nothing cannot hold
    #: `dispatch`, and the rows are left unestablished, not inapplicable.
    ADAPTER_DECLARES_NO_ACTION = "adapter-declares-no-action"
    #: No declared SupportedAction marks a parameter required, so no assignment could omit
    #: one. Kept out of NOT_APPLICABLE_CAUSES on purpose: an adapter whose actions need a
    #: target it never declared is exactly the case this leaves unestablished.
    ADAPTER_DECLARES_NO_REQUIRED_PARAM = "adapter-declares-no-required-param"
    #: No declared SupportedAction has an enum parameter with `allowed` values, so no value
    #: an assignment carries can fall outside a declared set (C19.4). NOT-APPLICABLE, like
    #: ADAPTER_DECLARES_NO_SENSOR_STREAM: the rule is about values outside a DECLARED set,
    #: and with no declared set it has no antecedent. An adapter that reads a closed-set
    #: parameter it never declared is not caught by this row, and could not be: a value
    #: probe can only be judged against a declaration.
    ADAPTER_DECLARES_NO_ENUM_PARAM = "adapter-declares-no-enum-param"
    #: No declared Custom entry names a verb, by custom_type or an `operation` enum, so no
    #: Custom action the adapter claims to serve could be sent (C19.5). NOT-APPLICABLE, like
    #: ADAPTER_DECLARES_NO_ENUM_PARAM: the rule is about a verb the adapter DECLARED, and with
    #: none declared it has no antecedent.
    ADAPTER_DECLARES_NO_CUSTOM_VERB = "adapter-declares-no-custom-verb"
    #: The adapter reported a supported contract version older than the one that made the row
    #: an obligation (C5.8: the stop latch binds a connection at 1.4.0). NOT-APPLICABLE: the
    #: rule does not bind the version the adapter declared.
    CONTRACT_PREDATES_OBLIGATION = "contract-predates-obligation"
    #: The run had no motion witness, so the time of a STOPPED could not be compared with
    #: the robot's actual rest (C5.7).
    NO_MOTION_WITNESS = "no-motion-witness"
    #: The plant declaration shows no motion channel independent of the command path that
    #: can confirm rest (ADR-0071 precondition). The safety obligation is unestablished on
    #: this platform, not inapplicable, so this is NOT in NOT_APPLICABLE_CAUSES: it keeps
    #: the verdict off `pass`.
    NO_INDEPENDENT_MOTION_CHANNEL = "no-independent-motion-channel"

    @classmethod
    def of(cls, detail: str) -> SkipCause | None:
        for c in cls:
            if detail.startswith(c.value):
                return c
        for spelling, c in _CAUSE_ALIASES.items():
            if detail.startswith(spelling):
                return c
        return None


#: The FleetAdapter harness (adapters/conformance/report.py) carries its own
#: SkipCause vocabulary and is a separately published surface. It says "adapter"
#: where this module, generic over three surfaces, says "target". The two drifted
#: and nothing noticed, because an unrecognised cause silently became the literal
#: string "unenumerated". The strict guard in _level() now makes that
#: an error, and these aliases are how the two vocabularies are reconciled
#: WITHOUT forcing either surface to rename its own members.
_CAUSE_ALIASES: dict[str, SkipCause] = {
    "adapter-did-not-reach-state": SkipCause.TARGET_DID_NOT_REACH_STATE,
    "needs-adapter-restart":       SkipCause.NEEDS_TARGET_RESTART,
}


@dataclass
class CheckResult:
    check_id: str
    title: str
    status: CheckStatus
    level: str = "MUST"          # MUST | MUST NOT | SHOULD | MAY | context
    detail: str = ""

    @property
    def is_blocking_failure(self) -> bool:
        return self.status is CheckStatus.FAIL and self.level in ("MUST", "MUST NOT")


class ResultSet:
    """Checks recorded during one run, on one surface."""

    def __init__(self, surface: str) -> None:
        self.surface = surface
        self.results: list[CheckResult] = []
        #: Every artefact path the run read. ``*0.3`` asserts over this.
        self.artefacts: list[str] = []
        #: Free-form context the report carries (target, versions, digests).
        self.context: dict[str, Any] = {}

    def add(self, check_id: str, title: str, status: CheckStatus,
            level: str = "MUST", detail: str = "") -> None:
        if status is CheckStatus.SKIP and SkipCause.of(detail) is None:
            raise AssertionError(
                f"{check_id}: a SKIP must begin with an enumerated SkipCause; got {detail!r}. "
                f"Valid causes: {', '.join(c.value for c in SkipCause)}")
        # A check id is the report's primary key. Two rows sharing one can
        # DISAGREE and both be tallied, so a self-contradicting report still
        # adds up and nothing downstream can detect it. Fail loudly instead.
        for r in self.results:
            if r.check_id == check_id:
                raise AssertionError(
                    f"{check_id}: already recorded as {r.title!r}, now {title!r}. A check id "
                    f"names exactly one check in a report.")
        self.results.append(CheckResult(check_id, title, status, level, detail))

    def ok(self, check_id: str, title: str, detail: str = "", level: str = "MUST") -> None:
        self.add(check_id, title, CheckStatus.PASS, level, detail)

    def bad(self, check_id: str, title: str, detail: str, level: str = "MUST") -> None:
        self.add(check_id, title, CheckStatus.FAIL, level, detail)

    def skip(self, check_id: str, title: str, cause: SkipCause, note: str = "",
             level: str = "MUST") -> None:
        detail = cause.value + (f": {note}" if note else "")
        self.add(check_id, title, CheckStatus.SKIP, level, detail)

    def ctx(self, check_id: str, title: str, detail: str = "") -> None:
        self.add(check_id, title, CheckStatus.PASS, "context", detail)

    def by_id(self) -> dict[str, CheckResult]:
        return {r.check_id: r for r in self.results}

    def skip_causes(self) -> list[tuple[str, str]]:
        out = []
        for r in self.results:
            if r.status is CheckStatus.SKIP:
                cause = SkipCause.of(r.detail)
                if cause is not None:
                    out.append((r.check_id, cause.value))
        return out


# ── Profile aggregation ──────────────────────────────────────────────────────

@dataclass
class LevelResult:
    result: str                                   # pass | fail | notVerified
    statistics: dict[str, int]
    skipped: list[dict[str, str]] = field(default_factory=list)
    supported_features: list[str] | None = None
    unsupported_features: list[str] | None = None
    #: Every check id the level grades, INCLUDING any the run never recorded. The
    #: verdict reads this list, not the recorded rows, so a Core check that produced no
    #: row at all cannot pass by absence. Not serialised: the sheet carries the rows.
    check_ids: list[str] = field(default_factory=list)


@dataclass
class ProfileResult:
    name: str
    required: bool
    applicable: bool = True
    core: LevelResult | None = None
    extended: LevelResult | None = None
    never_core: bool = False


#: Skip causes that mean the check COULD NOT APPLY to this implementation, as
#: opposed to could-not-be-established. A Grpc-only check against an Http
#: endpoint is not an unverified obligation; it is not an obligation at all.
#: Counting it as `skip` would leave a profile permanently `notVerified` for
#: every implementation that declared one of two mutually exclusive things --
#: which is every implementation, since the pairs are exclusive by construction.
#: OPTIONAL_DECLINED is still ABSENT, and the reason has changed. Rule A's logic
#: would admit it -- a declined optional feature genuinely was never an
#: obligation -- and the cause is now honest: it was recorded for two opposite
#: facts, and the FleetAdapter harness has been split so that it means a decline
#: and only a decline. `unsupported=true` on the arm the check depends on is the
#: observable, C10.1 and the three no-CommandResult paths that borrowed the cause
#: now record `adapter-did-not-reach-state` instead, and `C0.1` contradicts an
#: `optional-declined` the run cannot corroborate, so a recurrence is a failed
#: check rather than a silent one.
#:
#: IT STAYS OUT, and this is a decision rather than an oversight. ADR-0048
#: ("A verdict grades obligations", accepted 2026-09-03) records the exclusion
#: explicitly under "What is deliberately not changed", and an accepted ADR is not
#: reopened by editing the set it governs. Note that the precondition the ADR names
#: -- that the cause "must be made honest before it can be trusted with a verdict"
#: -- has since been MET: the C10.1 path the ADR cites now records
#: `adapter-did-not-reach-state` for a path made unreachable by an earlier refusal,
#: and `optional-declined` is left to genuine declines. So the argument for
#: promotion is now unopposed on correctness grounds, and what remains is cost:
#: promotion moves two verdicts in `conformance-reports/fleet-adapter.yaml` and five
#: Compute Endpoint levels from `notVerified` to `notApplicable` (ADR-0048, "Scope of
#: the verdict change"), and flips `test_optional_declined_is_deliberately_still_a_skip`
#: in `tests/python/test_conformance_skip_causes.py`, whose docstring still gives the
#: now-stale reason and says a promoter should be
#: sent to the ADR. Promoting therefore means amending the ADR, regenerating the
#: reports, and rewriting that test together -- which is a reviewed change, not a
#: tidy-up. Until that is taken, this set is correct as it stands.
NOT_APPLICABLE_CAUSES = {SkipCause.BINDING_NOT_DECLARED,
                         SkipCause.CONTRACT_NOT_DECLARED,
                         SkipCause.LEGAL_ALTERNATIVE_PATH,
                         SkipCause.ADAPTER_DECLARES_NO_SENSOR_STREAM,
                         SkipCause.ADAPTER_DECLARES_NO_ENUM_PARAM,
                         SkipCause.ADAPTER_DECLARES_NO_CUSTOM_VERB,
                         SkipCause.CONTRACT_PREDATES_OBLIGATION}


class UnenumeratedSkipCause(ValueError):  # noqa: N818 -- public name; callers catch it
    """A skip recorded a detail string that is not an enumerated ``SkipCause``.

    Raised rather than tolerated. PROFILES.md requires a cause "from the
    enumerated SkipCause set -- never free text, because a prose justification
    is not falsifiable", and the previous behaviour wrote the literal string
    ``"unenumerated"`` into the report and carried on. That silence is why
    C5.6's misspelling survived: the harness wrote `adapter-did-not-reach-state`,
    this module's enum says `target-did-not-reach-state`, and the mismatch
    produced a valid-LOOKING report instead of a failure.
    """


def _level(results: list[CheckResult], level: str = "core") -> LevelResult:
    """Grade one support level. ``level`` is ``"core"`` or ``"extended"``.

    ADR-0048: a verdict grades OBLIGATIONS. Two consequences are implemented
    here and nowhere else, so there is one place to read them.
    """
    stats = {"pass": 0, "fail": 0, "skip": 0, "notApplicable": 0}
    skipped: list[dict[str, str]] = []
    for r in results:
        cause = SkipCause.of(r.detail) if r.status is CheckStatus.SKIP else None
        if r.status is CheckStatus.SKIP and cause is None:
            # ERROR, not a fallthrough. See UnenumeratedSkipCause.
            raise UnenumeratedSkipCause(
                f"{r.check_id}: skip detail is not an enumerated SkipCause: "
                f"{r.detail[:120]!r}. Record one of: "
                f"{', '.join(sorted(c.value for c in SkipCause))}")
        if cause in NOT_APPLICABLE_CAUSES:
            stats["notApplicable"] += 1
            continue
        stats[r.status.value] += 1
        if r.status is CheckStatus.SKIP:
            skipped.append({"checkId": r.check_id, "cause": cause.value})

    # ADR-0048 rule A: EXTENDED NEVER REPORTS `fail`. Extended is optional, so a
    # failed Extended check makes its FEATURE unsupported -- which the report
    # already states in unsupportedFeatures -- and does not make the LEVEL fail.
    # A `fail` here would read the tier as mandatory, which is the defect
    # The failure keeps its count in `statistics` and its
    # `status: fail` in the checks list, so nothing is hidden; only the verdict
    # changes. PROFILES.md's three-value rule was always written for
    # `core.result` and never defined `extended.result`.
    #
    # THE COST, stated because it is real: an Extended level with a failure and
    # one with a skip now both read `notVerified`, and telling them apart means
    # reading statistics.fail or unsupportedFeatures. Accepted, because a
    # verdict of `fail` for something no implementation was obliged to do is
    # worse than a verdict that needs one more line of reading.
    if stats["fail"] and level == "core":
        verdict = "fail"
    elif stats["fail"] or stats["skip"]:
        # notVerified, never pass. Collapsing the two reports an unestablished
        # obligation as an established one.
        verdict = "notVerified"
    elif stats["pass"]:
        verdict = "pass"
    else:
        # Every check in the level was inapplicable. Reported as such rather
        # than as a pass earned by having nothing to do.
        verdict = "notApplicable"
    return LevelResult(verdict, stats, skipped, check_ids=[r.check_id for r in results])


# ── The capability sheet (ADR-0070) ──────────────────────────────────────────
#
# ADR-0048 made a verdict grade obligations. This extends it to the ROW: every
# capability row carries exactly one grade, the observation it rests on, and -- when
# it was not measured -- the concrete thing that would make it measurable. The
# report's verdict is then read off the Core rows, and a skipped Core row can no
# longer sit inside a `conformant: true`.

class Grade(StrEnum):
    """What a report establishes about ONE row. Exactly one per row, never folded.

    ``measured-*`` and ``failed`` come from the check's own outcome. The three
    ``not-*`` grades come from the row's SkipCause through ``CAUSE_GRADE``, and each
    of them owes the reader an ``unlock``.
    """

    #: Ran and passed, against a plant an operator declared physical in plant.yaml.
    MEASURED_HARDWARE = "measured-hardware"
    #: Ran and passed with no physical plant under the measurement: a simulator, an
    #: in-tree fixture, or a static read of shipped artefacts. ``mode`` says which.
    MEASURED_SIMULATION = "measured-simulation"
    #: Ran and did not pass, at any level. The verdict decides what a failure costs;
    #: the row does not hide it.
    FAILED = "failed"
    #: Observed or declared absent: the implementation declined it, does not declare
    #: it, took another permitted route, or does not implement the surface.
    NOT_SUPPORTED_BY_PLATFORM = "not-supported-by-platform"
    #: The obligation stands and this setting cannot observe it: no wire the suite
    #: watches carries it, or the target never reached the state it needs. The
    #: unlock lies outside the suite -- a witness, a sensor, a different run.
    NOT_TESTABLE_HERE = "not-testable-here"
    #: The suite's own limit. The unlock names the harness change.
    NOT_TESTED_SUITE_GAP = "not-tested-suite-gap"


MEASURED = frozenset({Grade.MEASURED_HARDWARE, Grade.MEASURED_SIMULATION})
#: Grades that owe an ``unlock``. ``failed`` does not: the row WAS measured, and what
#: changes its grade is the implementation, not the setting.
UNLOCK_DUE = frozenset({Grade.NOT_SUPPORTED_BY_PLATFORM, Grade.NOT_TESTABLE_HERE,
                        Grade.NOT_TESTED_SUITE_GAP})

#: Every SkipCause, mapped ONCE. ADR-0070 publishes this table and a test compares
#: the two. The grade of a skipped row is a function of its cause and of nothing
#: else, so there is no per-check override: a cause that honestly fits two grades is
#: a defect in the cause (it should be split), recorded as a finding, never settled
#: row by row here.
CAUSE_GRADE: dict[SkipCause, Grade] = {
    SkipCause.NOT_OBSERVABLE_ON_WIRE: Grade.NOT_TESTABLE_HERE,
    SkipCause.SURFACE_NOT_IMPLEMENTED: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.OPTIONAL_DECLINED: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.OPTIONAL_NOT_EXHIBITED: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.BINDING_NOT_DECLARED: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.CONTRACT_NOT_DECLARED: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.NEEDS_RESIDENT_TARGET: Grade.NOT_TESTED_SUITE_GAP,
    SkipCause.NEEDS_LIVE_CLUSTER: Grade.NOT_TESTED_SUITE_GAP,
    SkipCause.TARGET_DID_NOT_REACH_STATE: Grade.NOT_TESTABLE_HERE,
    SkipCause.LEGAL_ALTERNATIVE_PATH: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.DEPLOYMENT_PROPERTY: Grade.NOT_TESTED_SUITE_GAP,
    SkipCause.NEEDS_EXTENDED_RUN: Grade.NOT_TESTED_SUITE_GAP,
    SkipCause.NEEDS_TARGET_RESTART: Grade.NOT_TESTED_SUITE_GAP,
    SkipCause.NO_TELEMETRY_IN_RUN: Grade.NOT_TESTABLE_HERE,
    SkipCause.REQUIRES_SUCCESSFUL_REREGISTRATION: Grade.NOT_TESTED_SUITE_GAP,
    SkipCause.ALL_CORE_SUBJECTS_HONOURED: Grade.NOT_TESTABLE_HERE,
    SkipCause.ADAPTER_DECLARES_NO_OPERATING_SUBJECT: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.ADAPTER_DECLARES_NO_SENSOR_STREAM: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.ADAPTER_DECLARES_NO_ACTION: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.ADAPTER_DECLARES_NO_REQUIRED_PARAM: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.ADAPTER_DECLARES_NO_ENUM_PARAM: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.ADAPTER_DECLARES_NO_CUSTOM_VERB: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.CONTRACT_PREDATES_OBLIGATION: Grade.NOT_SUPPORTED_BY_PLATFORM,
    SkipCause.NO_MOTION_WITNESS: Grade.NOT_TESTABLE_HERE,
    SkipCause.NO_INDEPENDENT_MOTION_CHANNEL: Grade.NOT_SUPPORTED_BY_PLATFORM,
}


class Verdict(StrEnum):
    PASS = "pass"
    FAIL = "fail"
    #: No required Core row failed, and at least one was not measured. Never a pass.
    NOT_ESTABLISHED = "not-established"


class Fidelity(StrEnum):
    """Whether a physical robot stood under the measurements."""

    SIMULATED = "simulated"
    HARDWARE = "hardware"


class FidelityRefused(ValueError):  # noqa: N818 -- public name; the CLI catches it
    """A run asked to be graded as hardware without the evidence that it was."""


class MissingUnlock(ValueError):  # noqa: N818 -- public name; callers catch it
    """A row that was not measured has no ``unlock``.

    Raised rather than tolerated, for the reason ``UnenumeratedSkipCause`` is: a
    report that says what it could not establish and not what would establish it
    leaves the reader where the old boolean left them.
    """


def resolve_fidelity(mode: str, plant: dict[str, Any] | None) -> Fidelity:
    """The fidelity a run may be graded at, or a refusal.

    ``hardware`` needs a ``plant.yaml`` whose ``plant.kind`` an OPERATOR answered
    ``hardware`` (conformance/PLANT.md: no reading proves a plant is physical), AND
    ``--mode physical``. Anything else is ``simulated``. A contradiction between the
    two is refused rather than resolved, because either resolution is a guess about
    which of the two the operator got wrong.
    """
    kind = ((plant or {}).get("plant") or {}).get("kind") if isinstance(plant, dict) else None
    kind = kind if isinstance(kind, dict) else {}
    value = kind.get("value", "unknown")
    if value == "hardware" and kind.get("kind") != "operator":
        raise FidelityRefused(
            f"plant.kind is 'hardware' with kind {kind.get('kind')!r}: no reading proves a "
            "plant is physical, so only an operator answer may declare it (conformance/PLANT.md)")
    if mode == "physical":
        if plant is None:
            raise FidelityRefused(
                "--mode physical needs --plant naming the plant.yaml of the robot under test; "
                "without one the run cannot be graded as a hardware measurement")
        if value != "hardware":
            raise FidelityRefused(
                f"--mode physical, but the plant.yaml declares plant.kind {value!r}; only an "
                "operator-declared hardware plant supports a physical run")
        return Fidelity.HARDWARE
    if value == "hardware":
        raise FidelityRefused(
            f"the plant.yaml declares a hardware plant and --mode is {mode!r}; pass "
            "--mode physical, or supply the plant.yaml of the plant this run drove")
    return Fidelity.SIMULATED


def plant_block(path: Path, doc: dict[str, Any], *, robot_model: str | None,
                firmware: str | None) -> dict[str, Any]:
    """The report's account of the plant.yaml a run was graded against.

    ``robotModel`` and ``firmware`` have no field in plant.yaml, so they are taken
    from the operator and say so; absent, they are ``unknown`` with the reason.
    """
    kind = (doc.get("plant") or {}).get("kind") or {}

    def operator(value: str | None, flag: str) -> dict[str, str]:
        if value:
            return {"value": value, "source": "operator"}
        return {"value": "unknown", "reason": f"not supplied ({flag})"}

    return {
        "supplied": True,
        "path": str(path),
        "digest": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(),
        "kind": kind.get("value", "unknown") if isinstance(kind, dict) else "unknown",
        "robotModel": operator(robot_model, "--robot-model"),
        "firmware": operator(firmware, "--firmware"),
    }


UNLOCKS_PATH = Path(__file__).resolve().parent / "unlocks.yaml"


class Unlocks:
    """The unlock table: per check first, then per cause.

    A ``byCheck`` key ending in ``*`` matches every id with that prefix, and the
    longest match wins. ``byCause`` holds a default only for a cause whose unlock is
    the same whichever check records it; a cause without one must be covered check
    by check, and a row that is covered by neither raises ``MissingUnlock``.
    """

    def __init__(self, doc: dict[str, Any]) -> None:
        self.by_cause: dict[str, str] = doc.get("byCause") or {}
        self.by_check: dict[str, dict[str, str]] = doc.get("byCheck") or {}

    @classmethod
    def load(cls, path: Path = UNLOCKS_PATH) -> Unlocks:
        import yaml
        return cls(yaml.safe_load(path.read_text()))

    def resolve(self, surface: str, check_id: str, cause: SkipCause | None) -> str | None:
        table = self.by_check.get(surface) or {}
        if check_id in table:
            return table[check_id]
        prefixed = [k for k in table if k.endswith("*") and check_id.startswith(k[:-1])]
        if prefixed:
            return table[max(prefixed, key=len)]
        return self.by_cause.get(cause.value) if cause is not None else None


@dataclass(frozen=True)
class SheetRow:
    check_id: str
    grade: Grade
    evidence: dict[str, Any]
    unlock: str | None
    #: False when the cause is in NOT_APPLICABLE_CAUSES: the row is disclosed and
    #: graded, and it was never an obligation of this implementation (ADR-0048 B).
    obligation: bool


def _observation(r: CheckResult, cause: SkipCause | None) -> str:
    """The detail with the cause spelling removed: what the run actually saw."""
    text = r.detail
    if cause is not None:
        spellings = [cause.value] + [a for a, c in _CAUSE_ALIASES.items() if c is cause]
        for s in spellings:
            if text.startswith(s):
                text = text[len(s):].lstrip(":").strip()
                break
    return text.strip()


def build_sheet(surface: str, results: list[CheckResult], fidelity: Fidelity,
                unlocks: Unlocks, measurements: dict[str, dict[str, Any]] | None = None,
                ) -> dict[str, SheetRow]:
    """Grade every non-context row. Raises on a skip with no cause or no unlock."""
    rows: dict[str, SheetRow] = {}
    for r in results:
        if r.level == "context":
            continue
        cause = SkipCause.of(r.detail) if r.status is CheckStatus.SKIP else None
        if r.status is CheckStatus.PASS:
            grade = (Grade.MEASURED_HARDWARE if fidelity is Fidelity.HARDWARE
                     else Grade.MEASURED_SIMULATION)
        elif r.status is CheckStatus.FAIL:
            grade = Grade.FAILED
        elif cause is None:
            raise UnenumeratedSkipCause(
                f"{r.check_id}: skip detail is not an enumerated SkipCause: {r.detail[:120]!r}")
        else:
            grade = CAUSE_GRADE[cause]
        evidence: dict[str, Any] = {}
        if cause is not None:
            evidence["cause"] = cause.value
        observed = _observation(r, cause)
        evidence["observed"] = observed or None
        if not observed:
            evidence["note"] = "the run recorded an outcome and no observation for this row"
        if measurements and r.check_id in measurements:
            evidence["measurements"] = measurements[r.check_id]
        unlock = None
        if grade in UNLOCK_DUE:
            unlock = unlocks.resolve(surface, r.check_id, cause)
            if not unlock:
                raise MissingUnlock(
                    f"{r.check_id} is {grade.value} ({cause.value if cause else 'no cause'}) "
                    f"and conformance/unlocks.yaml names nothing that would make it testable. "
                    f"Add a byCheck entry for it under {surface}, or a byCause default if the "
                    f"unlock is the same for every check that records this cause")
        rows[r.check_id] = SheetRow(r.check_id, grade, evidence, unlock,
                                    obligation=cause not in NOT_APPLICABLE_CAUSES)
    return rows


#: What this suite revision CANNOT establish, stated on every report it writes,
#: whatever that report graded.
#:
#: A limitation here is a property of the SUITE, not of the run: it holds for a run
#: that passed every row as much as for one that failed, so it is not evidence and
#: does not belong in a row's evidence. It is printed unconditionally for the reason
#: the unlock table exists -- a reader who is told only what a report established
#: cannot tell a silence from a measurement.
#:
#: The one entry: the conformance fixture replaces a goal after a fixed dwell, and on
#: a plant whose in-place reversal takes longer than that dwell the replacement lands
#: before the reversal completes. No row failed on hardware because of it, and the
#: next suite revision sizes the dwell from the plant's declared turn time; until it
#: does, the limitation is stated rather than left to be re-derived.
SUITE_LIMITATIONS: tuple[str, ...] = (
    "goals needing a reversal longer than the fixture's dwell are pre-empted before the "
    "reversal completes, so motion rows attest one-directional travel only",
)


@dataclass
class ConformanceReport:
    """The document PROFILES.md specifies. One report, one surface."""

    surface: str
    implementation: dict[str, str]
    date: str
    protocol_version: str
    contract_version: str
    suite_revision: str
    mode: str
    capability_tier: str
    standalone: bool
    profiles: list[ProfileResult]
    results: list[CheckResult]
    target: dict[str, str] | None = None
    context: dict[str, Any] = field(default_factory=dict)
    #: ``simulated`` unless an operator-declared hardware plant stood under the run.
    fidelity: str = Fidelity.SIMULATED
    #: The plant.yaml the run was graded against (``plant_block``), or None.
    plant: dict[str, Any] | None = None
    #: The unlock table; None loads the shipped conformance/unlocks.yaml.
    unlocks: Unlocks | None = None
    #: Numbers a check measured, keyed by the check that measured them.
    measurements: dict[str, dict[str, Any]] = field(default_factory=dict)
    sheet: dict[str, SheetRow] = field(init=False)

    def __post_init__(self) -> None:
        self.fidelity = Fidelity(self.fidelity)
        # A hardware grade is refused here as well as at the CLI, so no caller can
        # construct one without the plant that justifies it.
        if self.fidelity is Fidelity.HARDWARE and (self.plant or {}).get("kind") != "hardware":
            raise FidelityRefused(
                "a hardware-fidelity report needs the plant.yaml of the robot under test, "
                "declaring plant.kind hardware; this report has none")
        self.sheet = build_sheet(self.surface, self.results, self.fidelity,
                                 self.unlocks or Unlocks.load(), self.measurements)

    @property
    def subject(self) -> dict[str, Any]:
        """What was graded: the implementation, the fidelity, and the plant."""
        return {
            "implementation": {"project": self.implementation.get("project", "?"),
                               "version": self.implementation.get("version", "?"),
                               "contractVersion": self.contract_version},
            "fidelity": self.fidelity.value,
            "plant": self.plant if self.plant is not None else {"supplied": False},
        }

    @property
    def verdict_blockers(self) -> list[dict[str, str]]:
        """Every Core row of a required, applicable profile that prevents ``pass``.

        A row prevents ``pass`` when it failed or was not measured -- unless its cause
        says it was never an obligation (ADR-0048 rule B, unchanged). A Core check the
        run recorded no row for prevents ``pass`` too: absence is not a measurement.
        """
        out: list[dict[str, str]] = []
        for p in self.profiles:
            if not p.applicable or not p.required or p.core is None:
                continue
            for cid in p.core.check_ids:
                row = self.sheet.get(cid)
                if row is None:
                    out.append({"checkId": cid, "profile": p.name,
                                "grade": Grade.NOT_TESTED_SUITE_GAP.value,
                                "reason": "the run recorded no row for this Core check"})
                elif row.obligation and row.grade not in MEASURED:
                    out.append({"checkId": cid, "profile": p.name, "grade": row.grade.value})
        return out

    @property
    def verdict(self) -> Verdict:
        blockers = self.verdict_blockers
        if any(b["grade"] == Grade.FAILED.value for b in blockers):
            return Verdict.FAIL
        return Verdict.NOT_ESTABLISHED if blockers else Verdict.PASS

    @property
    def conformant(self) -> bool:
        """True only when every required Core row was measured and passed.

        Before ADR-0070 this read "no failed Core check", so a required profile whose
        Core rows were skipped -- ``notVerified`` -- still produced ``true``, and the
        boolean said more than the run had established. It now agrees with the
        definition PROFILES.md always gave it ("every Core check of every required,
        applicable profile passed"), and ``verdict`` beside it says which of the two
        other outcomes a ``false`` is.
        """
        return self.verdict is Verdict.PASS

    def grade_counts(self) -> dict[str, int]:
        # Only the grades reachable at this fidelity: a simulated report does not print
        # the hardware grade even as a zero, and a hardware report does not print the
        # simulation one.
        unreachable = (Grade.MEASURED_SIMULATION if self.fidelity is Fidelity.HARDWARE
                       else Grade.MEASURED_HARDWARE)
        counts = {g.value: 0 for g in Grade if g is not unreachable}
        for row in self.sheet.values():
            counts[row.grade.value] += 1
        return counts

    def to_dict(self) -> dict[str, Any]:
        doc: dict[str, Any] = {
            "apiVersion": "conformance.swarmada.io/v1alpha1",
            "kind": "ConformanceReport",
            "surface": self.surface,
            "implementation": self.implementation,
            "date": self.date,
            "protocolVersion": self.protocol_version,
            "contractVersion": self.contract_version,
            "suiteRevision": self.suite_revision,
            "mode": self.mode,
            "capabilityTier": self.capability_tier,
            "standalone": self.standalone,
            "subject": self.subject,
            "verdict": self.verdict.value,
            "verdictBlockedBy": self.verdict_blockers,
            "conformant": self.conformant,
            "grades": self.grade_counts(),
            "limitations": list(SUITE_LIMITATIONS),
        }
        if self.target is not None:
            doc["target"] = self.target
        if self.context:
            doc["context"] = self.context
        profiles: list[dict[str, Any]] = []
        for p in self.profiles:
            entry: dict[str, Any] = {"name": p.name, "required": p.required}
            if p.never_core:
                entry["neverCore"] = True
            if not p.applicable:
                entry["applicable"] = False
                profiles.append(entry)
                continue
            for key, lv in (("core", p.core), ("extended", p.extended)):
                if lv is None:
                    continue
                block: dict[str, Any] = {"result": lv.result, "statistics": lv.statistics}
                if lv.skipped:
                    block["skipped"] = lv.skipped
                if lv.supported_features is not None:
                    block["supportedFeatures"] = lv.supported_features
                    block["unsupportedFeatures"] = lv.unsupported_features or []
                entry[key] = block
            profiles.append(entry)
        doc["profiles"] = profiles
        checks: list[dict[str, Any]] = []
        for r in self.results:
            entry = {"checkId": r.check_id, "title": r.title, "status": r.status.value,
                     "level": r.level, "detail": r.detail}
            row = self.sheet.get(r.check_id)
            if row is not None:
                entry["grade"] = row.grade.value
                entry["evidence"] = row.evidence
                if row.unlock is not None:
                    entry["unlock"] = row.unlock
            checks.append(entry)
        doc["checks"] = checks
        return doc

    def to_yaml(self) -> str:
        import yaml
        return yaml.safe_dump(self.to_dict(), sort_keys=False, width=100,
                              default_flow_style=False)

    def to_json(self) -> str:
        return json.dumps(self.to_dict(), indent=2)

    def to_text(self) -> str:
        """The sheet first, the profile table under it, the verdict last."""
        head = (f"{self.surface} conformance — {self.implementation.get('project', '?')} "
                f"({self.protocol_version}, contract {self.contract_version}, "
                f"suite {self.suite_revision})")
        fid = (f"fidelity: {self.fidelity.value} — "
               + ("measured against the plant declared in "
                  f"{(self.plant or {}).get('path', '?')}" if self.fidelity is Fidelity.HARDWARE
                  else "no physical plant stood under these measurements")
               + f"   mode: {self.mode}")
        lines = [head, "=" * len(head), fid]
        lines += [f"limitation: {text}" for text in SUITE_LIMITATIONS]
        lines.append("-" * len(head))
        width = max(len(g.value) for g in Grade)
        for r in self.results:
            row = self.sheet.get(r.check_id)
            if row is None:
                continue
            observed = row.evidence.get("observed") or row.evidence.get("note", "")
            lines.append(f"{row.grade.value:<{width}}  {r.check_id:<7} {r.title}"
                         + (f"  — {observed}" if observed else ""))
            if row.unlock:
                lines.append(f"{'':<{width}}  {'':<7} unlock: {row.unlock}")
        lines.append("-" * len(head))
        lines.append("grades: " + " · ".join(f"{k} {v}" for k, v in self.grade_counts().items()))
        lines.append("-" * len(head))
        lines.append(f"{'PROFILE':<20} {'LEVEL':<9} {'REQ':<5} {'RESULT':<12} "
                     "pass/fail/skip(+notApplicable)")
        for p in self.profiles:
            if not p.applicable:
                lines.append(f"{p.name:<20} {'—':<9} {'—':<5} {'notApplicable':<12}")
                continue
            for label, lv in (("core", p.core), ("extended", p.extended)):
                if lv is None:
                    continue
                req = "yes" if (p.required and label == "core") else "no"
                s = lv.statistics
                extra = ""
                if lv.supported_features is not None:
                    extra = f"   features: {', '.join(lv.supported_features) or '(none declared)'}"
                na = f"+{s['notApplicable']}na" if s.get("notApplicable") else ""
                lines.append(f"{p.name:<20} {label:<9} {req:<5} {lv.result:<12} "
                             f"{s['pass']}/{s['fail']}/{s['skip']}{na}{extra}")
        lines.append("-" * len(head))
        lines.append(f"tier: {self.capability_tier}   standalone: {self.standalone}   "
                     f"verdict: {self.verdict.value}   conformant: "
                     f"{'true' if self.conformant else 'false'}")
        blockers = self.verdict_blockers
        if blockers:
            lines.append("Core rows preventing pass: " + ", ".join(
                f"{b['checkId']} ({b['profile']}, {b['grade']})" for b in blockers))
        return "\n".join(lines)
