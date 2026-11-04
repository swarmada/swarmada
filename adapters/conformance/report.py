# Copyright 2026 The Swarmada Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Conformance report model: per-check results and a machine + human summary."""

from __future__ import annotations

import enum
import json
from dataclasses import asdict, dataclass, field


class SkipCause(str, enum.Enum):  # noqa: UP042 -- StrEnum changes str()/format()
    """Why a documented check was not exercised.

    Enumerated rather than free text because a prose justification is not checkable, and
    two of the first nine written here turned out to be factually wrong — pointing a
    reader at the wrong problem. A cause from this set is falsifiable: NO_TELEMETRY_IN_RUN
    recorded in a run that DID observe telemetry is a detectable contradiction, which is
    what the harness asserts at the end of a run.
    """

    #: The property lives inside the adapter and produces no observable wire effect.
    NOT_OBSERVABLE_ON_WIRE = "not-observable-on-wire"
    #: Needs a reconnect in which registration SUCCEEDS; this suite's only reconnect
    #: refuses registration by design (the contract-version path).
    REQUIRES_SUCCESSFUL_REREGISTRATION = "requires-successful-reregistration"
    #: The adapter streamed no TelemetryPayload during the run, so nothing about
    #: telemetry content or cadence can be asserted.
    NO_TELEMETRY_IN_RUN = "no-telemetry-in-run"
    #: A deployment property (TLS material, process supervision) the in-process harness
    #: does not reproduce; asserted in the control plane's own tests instead.
    DEPLOYMENT_PROPERTY = "deployment-property"
    #: The adapter declined an OPTIONAL command, which is itself conformant.
    OPTIONAL_DECLINED = "optional-declined"
    #: The adapter reached the state an OPTIONAL (MAY) behaviour applies to and did not
    #: exhibit it, which the MAY permits. Nothing was declined: the command the behaviour
    #: hangs off was answered, and the adapter used its permission to stay silent. The one
    #: case today is C10.1, an accepted install with no advisory progress. Kept apart from
    #: OPTIONAL_DECLINED because that cause means `unsupported=true` on the arm and C0.1
    #: holds it to that. Borrowing it here made C0.1 fail a conformant adapter.
    OPTIONAL_NOT_EXHIBITED = "optional-not-exhibited"
    #: Would require several forced drops or a long run to observe a timing curve.
    NEEDS_EXTENDED_RUN = "needs-extended-run"
    #: The adapter did not reach the state the check needs (no registration, no redial).
    ADAPTER_DID_NOT_REACH_STATE = "adapter-did-not-reach-state"
    #: The implementation satisfied the requirement by another route the
    #: specification permits, leaving this check unreachable -- so it was never
    #: an obligation of THIS implementation (ADR-0048 rule B). Named for the
    #: class, not for any one check: CE6.2/CE6.3 are the same shape.
    #: NOT an escape hatch. It requires that the alternative route is actually
    #: permitted AND that the check is unreachable BECAUSE it was taken. An
    #: implementation that simply did not do the thing records a cause that says
    #: so.
    LEGAL_ALTERNATIVE_PATH = "legal-alternative-path"
    #: Every core operating subject is one the adapter DECLARED it honours, so the run
    #: could construct no rule it cannot honour and FA-R5's core-seven arm had nothing to
    #: probe. Distinct from a decline and from a state the adapter failed to reach: the
    #: adapter answered, and what it answered removed the antecedent.
    ALL_CORE_SUBJECTS_HONOURED = "all-core-subjects-honoured"
    #: Needs the harness to terminate and respawn the adapter PROCESS. It spawns the
    #: adapter once via --adapter-cmd and never restarts it, so anything that only a
    #: restart can show (durable state) is unverified by this suite.
    NEEDS_ADAPTER_RESTART = "needs-adapter-restart"
    #: The adapter declared NO honoured operating subject, so the run could construct no
    #: rule it claims it CAN honour and FA-R2's arrival arm had nothing to probe. The
    #: mirror of ALL_CORE_SUBJECTS_HONOURED: the adapter answered, and what it answered
    #: removed the antecedent. Declaring none is conformant (FA-R1) and also makes the
    #: whole `zone-restrictions` profile not applicable (Part E), so this cause is not a
    #: coverage hole for any adapter the profile grades.
    ADAPTER_DECLARES_NO_OPERATING_SUBJECT = "adapter-declares-no-operating-subject"
    #: The adapter declared NO sensor stream, so there is nothing to activate and the duty
    #: cycle has no subject. Conformant: a robot with no sensor to offer offers none, and
    #: the checks that switch one on and off are then not obligations of it. Distinct from
    #: OPTIONAL_DECLINED, which means it HAS streams and refuses to drive them.
    ADAPTER_DECLARES_NO_SENSOR_STREAM = "adapter-declares-no-sensor-stream"
    #: The adapter declared NO SupportedAction, so there is no action type an assignment
    #: could carry that the adapter claims to serve, and every row that needs an ACCEPTED
    #: assignment had nothing to probe with. Not a decline of an optional feature: an
    #: adapter offering no action cannot hold the `dispatch` profile, and the rows stay
    #: unestablished rather than inapplicable.
    ADAPTER_DECLARES_NO_ACTION = "adapter-declares-no-action"
    #: No SupportedAction the adapter declared marks any parameter `required`, so the run
    #: could construct no assignment that omits one. The declaration removed the antecedent;
    #: an adapter whose actions do need a parameter it never declared is the case this
    #: leaves unestablished.
    ADAPTER_DECLARES_NO_REQUIRED_PARAM = "adapter-declares-no-required-param"
    #: No SupportedAction the adapter declared has an enum parameter with `allowed` values,
    #: so the run could construct no value outside a declared set. Conformant: an adapter
    #: that declares no closed set of values has none to enforce, and C19.4 is then not an
    #: obligation of it.
    ADAPTER_DECLARES_NO_ENUM_PARAM = "adapter-declares-no-enum-param"
    #: No motion witness was supplied to the run (`--witness-file`), so the time of a STOPPED
    #: could not be compared with the robot's actual moment of rest.
    NO_MOTION_WITNESS = "no-motion-witness"
    #: The adapter reported a supported contract version older than the one that made the row
    #: an obligation, so the row does not bind it (C5.8: the stop latch binds a connection at
    #: 1.4.0, and a control plane sends an older one no EstopClear). Not applicable: an
    #: adapter that reports 1.4.0 is graded.
    CONTRACT_PREDATES_OBLIGATION = "contract-predates-obligation"
    #: No declared Custom entry names a verb, by ``custom_type`` or by an ``operation`` enum,
    #: so the run could construct no Custom action the adapter claims to serve (C19.5). The
    #: declaration removed the antecedent: an adapter that serves no Custom verb has none to
    #: accept.
    ADAPTER_DECLARES_NO_CUSTOM_VERB = "adapter-declares-no-custom-verb"


class CheckStatus(str, enum.Enum):  # noqa: UP042 -- StrEnum changes str()/format()
    """Outcome of a single conformance check."""

    PASS = "pass"
    FAIL = "fail"
    # SKIP: the check was not exercised because the adapter does not implement
    # or initiate the behavior (e.g. a template that never registers a robot).
    # A SKIP is not a failure, but it is not a pass either — it is recorded so a
    # reader can see exactly what was and was not verified.
    SKIP = "skip"


@dataclass
class CheckResult:
    """The result of one CONFORMANCE.md check (e.g. ``C3.2``)."""

    check_id: str
    title: str
    status: CheckStatus
    # RFC 2119 obligation level of the check: "MUST", "MUST NOT", "SHOULD", "MAY".
    level: str = "MUST"
    detail: str = ""

    @property
    def is_blocking_failure(self) -> bool:
        """A failed MUST / MUST NOT is what makes an adapter non-conforming."""
        return self.status is CheckStatus.FAIL and self.level in ("MUST", "MUST NOT")


# The fleet-adapter CONTRACT version this suite revision attests against (semver, ADR-0032).
# Distinct from the two identifiers beside it: ``protocol_version`` is the wire-package identity
# ("fleet_adapter.v1"), and an adapter's own build version is neither. A result is earned against
# THIS value, and the registry row records it (adapters/REGISTRY.md).
#
# Bump it deliberately when the proto surface or the CapabilitiesSnapshot/SupportedAction schema
# changes, or on a contract change RFC-0001 makes off the wire -- and NOT when these checks
# change. The conformance-suite revision is deliberately not a
# component of the contract version (ADR-0032, Decision bullet 1: "a suite change re-attests
# adapters, it does not move the contract they negotiate"); it is provenance recorded beside the
# result, as ``suiteRevision``. This comment said otherwise until 2026-09-13.
#
# major = breaking (adapters re-run `make conformance` and update their registry row).
# minor/patch = an existing qualification stays valid -- but a MINOR BUMP IS NOT HARMLESS: the
# supported range is N and N-1 of the control plane's contract version, within the major, so it
# moves with that version and never with time; an adapter stays in range until the control plane
# is two minors past the version it reports (ADR-0032 Amendment 1 A1.2).
#
# It MUST move together with the control-plane constant in internal/contract/version.go, and
# `make spec-contract` fails if only one of them does. The supported RANGE is derived from that
# constant and is not written down here.
CONTRACT_VERSION = "1.4.0"

# The conformance-suite revision this harness is: `revision:` in conformance/profiles.yaml, which
# declares its class and its pinned check set. Written into every report so a result can say which
# suite produced it (ADR-0032 Amendment 1). tests/python/test_report_names_its_suite_revision.py
# fails when the two differ, so a revision bump moves both.
SUITE_REVISION = "2026.10.22.2"


@dataclass
class Report:
    """The full conformance run: results plus adapter, protocol, and contract identity."""

    adapter: str
    protocol_version: str = "fleet_adapter.v1"
    contract_version: str = CONTRACT_VERSION
    suite_revision: str = SUITE_REVISION
    results: list[CheckResult] = field(default_factory=list)
    # The operating subjects the adapter declared it can honour (FA-R1), as read off the
    # CapabilitiesSnapshot. None and [] are DIFFERENT and both are published: None means the
    # run never got a snapshot to read (the adapter answered no scan, which C12.1 fails), and
    # [] means the adapter answered and declared none, which FA-R1 says is not a breach.
    # Collapsing them would make "we could not tell" and "it told us nothing" the same row,
    # and they select different profile verdicts.
    honoured_operating_constraints: list[str] | None = None
    # Numbers a check MEASURED rather than asserted, published beside the rows so a reader
    # does not have to parse them back out of a detail string. The sensor-stream fail-safe
    # is the first: a report that claimed "a partition stops the camera" without the
    # interval would be publishing an intention.
    measurements: dict = field(default_factory=dict)
    # Which check measured each number in ``measurements``, so a report can file the number
    # under the row it is evidence for instead of leaving the reader to match them up.
    measurement_checks: dict = field(default_factory=dict)

    def add(
        self,
        check_id: str,
        title: str,
        status: CheckStatus,
        level: str = "MUST",
        detail: str = "",
    ) -> None:
        # CONFORMANCE.md requires every SKIP to name an ENUMERATED cause, because a prose
        # justification is not falsifiable and several early ones were simply wrong. Enforce
        # it here rather than in one helper, so a skip recorded anywhere in the harness
        # cannot bypass the rule.
        if status is CheckStatus.SKIP:
            if not any(detail.startswith(c.value) for c in SkipCause):
                raise AssertionError(
                    f"{check_id}: a SKIP must begin with an enumerated SkipCause; got {detail!r}. "
                    f"Valid causes: {', '.join(c.value for c in SkipCause)}")
        # A check id is this report's PRIMARY KEY: `counts` and the CONFORMANT verdict are
        # computed over the rows. Two rows sharing an id can DISAGREE -- a PASS and a FAIL for
        # the same check -- and BOTH are tallied, so a self-contradicting report still adds up
        # and nothing downstream can detect it. Fail the run loudly instead.
        #
        # Keyed on the id ALONE, not (id, title). Several ids legitimately carry different
        # titles across MUTUALLY EXCLUSIVE branches (C2.1's discover-vs-register arms, C5.2's
        # no-ack arm, C8.1's skip arm, C4.3/C4.4's skip arms): only one such branch runs in a
        # given report, so keying on the id is both correct and strictly stronger.
        for r in self.results:
            if r.check_id == check_id:
                raise AssertionError(
                    f"{check_id}: already recorded as {r.title!r}, now {title!r}. A check id "
                    f"must name exactly one check in a report. A precondition belongs under a "
                    f"C0.x harness-context id -- see CONFORMANCE.md 'C0 - Report integrity'.")
        self.results.append(CheckResult(check_id, title, status, level, detail))

    def skip_causes(self) -> list[tuple[str, str]]:
        """(check_id, cause) for every recorded SKIP, read off the report itself.

        C0.1 contradicts skip causes against what the run observed. It must read the
        results actually recorded, not a static declaration list — a cause recorded
        dynamically (for example NO_TELEMETRY_IN_RUN from the telemetry check) never
        appears in a static list, so a contradiction test over one can never fire.
        """
        out = []
        for r in self.results:
            if r.status is not CheckStatus.SKIP:
                continue
            for c in SkipCause:
                if r.detail.startswith(c.value):
                    out.append((r.check_id, c.value))
                    break
        return out

    # ── Aggregates ────────────────────────────────────────────────────────────

    def counts(self) -> dict[str, int]:
        c = {s.value: 0 for s in CheckStatus}
        for r in self.results:
            c[r.status.value] += 1
        return c

    def blocking_failures(self) -> list[CheckResult]:
        return [r for r in self.results if r.is_blocking_failure]

    @property
    def conformant(self) -> bool:
        """Conforming = no failed MUST / MUST NOT among the checks that ran."""
        return not self.blocking_failures()

    # ── Rendering ─────────────────────────────────────────────────────────────

    def to_json(self) -> str:
        payload = {
            "adapter": self.adapter,
            "protocol_version": self.protocol_version,
            "contract_version": self.contract_version,
            "suite_revision": self.suite_revision,
            "honoured_operating_constraints": self.honoured_operating_constraints,
            "measurements": self.measurements,
            "measurement_checks": self.measurement_checks,
            "conformant": self.conformant,
            "counts": self.counts(),
            "results": [asdict(r) | {"status": r.status.value} for r in self.results],
        }
        return json.dumps(payload, indent=2)

    def to_text(self) -> str:
        glyph = {
            CheckStatus.PASS: "PASS",
            CheckStatus.FAIL: "FAIL",
            CheckStatus.SKIP: "skip",
        }
        lines = [
            f"Fleet Adapter conformance — {self.adapter} "
            f"({self.protocol_version}, contract {self.contract_version}, "
            f"suite {self.suite_revision})",
            "=" * 68,
        ]
        for r in self.results:
            note = f"  — {r.detail}" if r.detail else ""
            # A failing SHOULD is not a failing MUST, and the terminal used to print the same
            # token for both while never printing the level. In a CONFORMANT run with a failing
            # SHOULD there was then nothing on screen separating the two — the `Blocking (…)`
            # summary is emitted only when a blocking failure exists. The machine-readable
            # artifact never had this problem; it carries `level:` per row.
            tok = glyph[r.status]
            if r.status is CheckStatus.FAIL and not r.is_blocking_failure:
                tok = "warn"
            lvl = f" ({r.level})" if r.status is CheckStatus.FAIL else ""
            lines.append(f"[{tok}] {r.check_id:<6}{lvl} {r.title}{note}")
        c = self.counts()
        # `counts()` is the raw per-status tally and stays that way -- it is the shape the
        # JSON artifact publishes, and ADR-0007 self-certification reads it. The summary line
        # must not reuse it verbatim: a failing SHOULD lands in c['fail'], so a CONFORMANT run
        # printed "2 failed", contradicting both the verdict on the same line and the [warn]
        # rows above it. Split the tally here, at the point of rendering, from the same rows
        # the verdict is computed from -- never re-walked with a second predicate that could
        # drift from `blocking_failures()`.
        blocking = len(self.blocking_failures())
        advisory = c["fail"] - blocking
        lines.append("-" * 68)
        lines.append(
            f"{c['pass']} passed, {blocking} failed, {advisory} advisory, "
            f"{c['skip']} skipped   "
            f"=> {'CONFORMANT' if self.conformant else 'NON-CONFORMANT'} "
            "(for the checks that ran)"
        )
        if self.blocking_failures():
            lines.append("Blocking (failed MUST/MUST NOT): " + ", ".join(
                r.check_id for r in self.blocking_failures()
            ))
        return "\n".join(lines)
