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

"""A harness report names the suite revision that produced it.

A conformance result is earned against a contract version AND a suite revision, and
ADR-0032 Amendment 1 records the revision beside the contract version, never folded
into it. The raw harness report carried the contract version only, so a report kept
on disk could not say which suite graded it: only the profiled ConformanceReport
carried `suiteRevision`, stamped at profiling time from whatever `profiles.yaml` said
then. A report that cannot name its suite cannot back a conformance claim.
"""

from __future__ import annotations

import json
import re
from pathlib import Path

from adapters.conformance import report

ROOT = Path(__file__).resolve().parents[2]
PROFILES = ROOT / "conformance" / "profiles.yaml"


def _profiles_revision() -> str:
    m = re.search(r'(?m)^revision:\s*"(?P<rev>[^"]+)"', PROFILES.read_text(encoding="utf-8"))
    assert m, "conformance/profiles.yaml declares no `revision:`; this guard cannot run"
    return m["rev"]


def test_the_harness_names_the_revision_profiles_yaml_declares() -> None:
    declared = getattr(report, "SUITE_REVISION", None)
    assert declared == _profiles_revision(), (
        f"the harness says it is suite {declared!r}; conformance/profiles.yaml says "
        f"{_profiles_revision()!r}. A revision bump moves both")


def test_the_json_report_carries_the_suite_revision() -> None:
    payload = json.loads(report.Report(adapter="a").to_json())
    assert payload.get("suite_revision") == _profiles_revision(), (
        "the JSON report does not name the suite revision that produced it")
    assert payload.get("contract_version") == report.CONTRACT_VERSION, (
        "the suite revision is recorded beside the contract version, not in place of it")


def test_the_text_report_names_the_suite_revision() -> None:
    assert f"suite {_profiles_revision()}" in report.Report(adapter="a").to_text()
