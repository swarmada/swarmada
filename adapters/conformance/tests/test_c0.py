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

"""C0.1 — a skip cause must be contradictable by the run that recorded it.

Having split ``optional-declined`` from the paths that were
never a decline, assert that the split holds. A cause is only worth enumerating
if it can be shown false, and this one now can: the observable is
``unsupported=true`` on the arm the check depends on.
"""

from __future__ import annotations

from adapters.conformance.report import CheckStatus, SkipCause


def _status(report, check_id):
    for r in report.results:
        if r.check_id == check_id:
            return r
    raise AssertionError(f"{check_id} not recorded")


def test_c01_fails_an_optional_declined_the_run_cannot_corroborate(pb, driver):
    """A check that recorded a decline for a command the adapter never declined."""
    drv, _session, report = driver
    report.add("C9.1", "verify_* answers with actualMetrics", CheckStatus.SKIP,
               level="SHOULD",
               detail=f"{SkipCause.OPTIONAL_DECLINED.value}: adapter declined verify_*")

    drv._record_unverified()

    row = _status(report, "C0.1")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "C9.1" in row.detail
    assert "not a feature that was declined" in row.detail


def test_c01_passes_a_decline_the_run_observed(pb, driver):
    """The same skip, with the decline actually on the wire."""
    drv, _session, report = driver
    drv._declined_arms.add("verify_hardware")
    report.add("C9.1", "verify_* answers with actualMetrics", CheckStatus.SKIP,
               level="SHOULD",
               detail=f"{SkipCause.OPTIONAL_DECLINED.value}: adapter declined verify_*")

    drv._record_unverified()

    assert _status(report, "C0.1").status is CheckStatus.PASS


def test_c01_still_contradicts_a_false_no_telemetry_claim(pb, driver):
    """The original contradiction is unchanged by the new one."""
    drv, _session, report = driver
    drv.telemetry_seen = True
    report.add("C2.3", "Honour telemetry_interval_seconds from RegisterAck",
               CheckStatus.SKIP, detail=SkipCause.NO_TELEMETRY_IN_RUN.value)

    drv._record_unverified()

    row = _status(report, "C0.1")
    assert row.status is CheckStatus.FAIL
    assert "the stated reason is false" in row.detail


def test_c01_contradicts_optional_not_exhibited_on_a_declined_arm(pb, driver):
    """``optional-not-exhibited`` claims the arm was answered. A decline of it says otherwise."""
    drv, _session, report = driver
    drv._declined_arms.add("push_firmware")
    report.add("C10.1", "push_firmware may emit UpdateProgress", CheckStatus.SKIP,
               level="MAY",
               detail=f"{SkipCause.OPTIONAL_NOT_EXHIBITED.value}: accepted, no progress")

    drv._record_unverified()

    row = _status(report, "C0.1")
    assert row.status is CheckStatus.FAIL, row.detail
    assert "C10.1" in row.detail


def test_c01_accepts_optional_not_exhibited_on_an_answered_arm(pb, driver):
    drv, _session, report = driver
    report.add("C10.1", "push_firmware may emit UpdateProgress", CheckStatus.SKIP,
               level="MAY",
               detail=f"{SkipCause.OPTIONAL_NOT_EXHIBITED.value}: accepted, no progress")

    drv._record_unverified()

    assert _status(report, "C0.1").status is CheckStatus.PASS
