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


class SkipCause(str, enum.Enum):
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
    #: Would require several forced drops or a long run to observe a timing curve.
    NEEDS_EXTENDED_RUN = "needs-extended-run"
    #: The adapter did not reach the state the check needs (no registration, no redial).
    ADAPTER_DID_NOT_REACH_STATE = "adapter-did-not-reach-state"


class CheckStatus(str, enum.Enum):
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
# Bump it deliberately when the proto surface, the SupportedAction schema, or these checks change:
# major = breaking (adapters re-run `make conformance` and update their registry row), minor/patch =
# compatible (an existing qualification stays valid). The control-plane-side constant and the
# supported range it accepts are separate (ADR-0032's config/handshake items).
CONTRACT_VERSION = "1.0.0"


@dataclass
class Report:
    """The full conformance run: results plus adapter, protocol, and contract identity."""

    adapter: str
    protocol_version: str = "fleet_adapter.v1"
    contract_version: str = CONTRACT_VERSION
    results: list[CheckResult] = field(default_factory=list)

    def add(
        self,
        check_id: str,
        title: str,
        status: CheckStatus,
        level: str = "MUST",
        detail: str = "",
    ) -> None:
        self.results.append(CheckResult(check_id, title, status, level, detail))

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
            f"({self.protocol_version}, contract {self.contract_version})",
            "=" * 68,
        ]
        for r in self.results:
            note = f"  — {r.detail}" if r.detail else ""
            lines.append(f"[{glyph[r.status]}] {r.check_id:<6} {r.title}{note}")
        c = self.counts()
        lines.append("-" * 68)
        lines.append(
            f"{c['pass']} passed, {c['fail']} failed, {c['skip']} skipped   "
            f"=> {'CONFORMANT' if self.conformant else 'NON-CONFORMANT'} "
            "(for the checks that ran)"
        )
        if self.blocking_failures():
            lines.append("Blocking (failed MUST/MUST NOT): " + ", ".join(
                r.check_id for r in self.blocking_failures()
            ))
        return "\n".join(lines)
