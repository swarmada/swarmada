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

"""Every capability tier can hold what it requires, and a declined command is no feature.

Two registry rules, each of which the registry once broke:

    * A Read Only or Traffic Light adapter accepts no task assignment (RFC-0001
      conformance-profiles, "Adapter capability tiers"), and the harness records every row
      that needs an accepted assignment skipped `adapter-declares-no-action` for an adapter
      that declares no action -- a cause that leaves the obligation unestablished. A Core
      row of that kind in a profile such a tier requires made the tier unattainable: C6.4
      sat in `telemetry`, so no Read Only adapter could hold `telemetry`.
    * A feature is supported when at least one of its checks passed, none failed and none
      was declined. C10.2 (the terminal outcome of an install) is shared by
      `provisioning.push-firmware` and `provisioning.model-update`; without the decline
      test, an adapter that declined `model_update` and completed a firmware install read
      as supporting both.
"""

from __future__ import annotations

import conformance.model as m
from adapters.conformance import harness
from conformance.registry import Registry

#: The profiles that grade assignment and execution. A tier that requires neither accepts
#: no assignment.
_ASSIGNING = {"dispatch", "lease"}


def _needs_served_action() -> set[str]:
    return {row[0] for rows in harness._NEEDS_SERVED_ACTION.values() for row in rows}


def _fleet_adapter() -> tuple[dict, dict[str, dict]]:
    fa = Registry().surface("FleetAdapter")
    return fa, {p["name"]: p for p in fa["profiles"]}


def test_the_harness_table_this_reads_is_not_empty() -> None:
    # Fail, never pass vacuously: an empty table would make the next test prove nothing.
    assert "C16.1" in _needs_served_action()


def test_a_tier_without_assignment_requires_no_core_row_that_needs_one() -> None:
    fa, profiles = _fleet_adapter()
    needs = _needs_served_action()
    checked = 0
    for tier, names in fa["tierRequires"].items():
        if _ASSIGNING & set(names):
            continue
        checked += 1
        for name in names:
            core = set(profiles[name].get("core") or [])
            assert not core & needs, (
                f"tier {tier} requires profile {name!r}, whose Core rows "
                f"{sorted(core & needs)} need an accepted assignment; an adapter of that "
                f"tier takes none, so it can never hold the profile")
    assert checked == 2, f"expected the Read Only and Traffic Light tiers, checked {checked}"


def test_read_only_holds_telemetry_with_no_action_declared() -> None:
    rs = m.ResultSet("FleetAdapter")
    rs.add("C6.1", "snapshot then delta", m.CheckStatus.PASS)
    rs.add("C6.2", "explicit presence", m.CheckStatus.PASS)
    rs.add("C6.4", "Echo the assigned fencing_token in ActionStatusUpdate",
           m.CheckStatus.SKIP, detail=m.SkipCause.ADAPTER_DECLARES_NO_ACTION.value + ": none")
    by_name = {p.name: p for p in Registry().profile_results(
        "FleetAdapter", rs, tier="ReadOnly", applicable={})}
    assert by_name["telemetry"].required
    assert by_name["telemetry"].core.result == "pass"
    assert not by_name["dispatch"].required


def _provisioning(rows: list[tuple[str, m.CheckStatus, str]]) -> m.LevelResult:
    rs = m.ResultSet("FleetAdapter")
    for cid, status, detail in rows:
        rs.add(cid, cid, status, detail=detail)
    by_name = {p.name: p for p in Registry().profile_results(
        "FleetAdapter", rs, tier="FullControl", applicable={})}
    return by_name["provisioning"].extended


_DECLINED = m.SkipCause.OPTIONAL_DECLINED.value + ": CommandResult.unsupported = true"


def test_a_declined_model_update_is_not_supported_beside_a_firmware_install() -> None:
    ext = _provisioning([("C7.2", m.CheckStatus.SKIP, _DECLINED),
                         ("C7.3", m.CheckStatus.PASS, ""),
                         ("C10.2", m.CheckStatus.PASS, "")])
    assert ext.supported_features == ["provisioning.push-firmware"]
    assert "provisioning.model-update" in ext.unsupported_features


def test_a_declined_firmware_push_is_not_supported_beside_a_model_install() -> None:
    ext = _provisioning([("C7.2", m.CheckStatus.PASS, ""),
                         ("C7.3", m.CheckStatus.SKIP, _DECLINED),
                         ("C10.2", m.CheckStatus.PASS, "")])
    assert ext.supported_features == ["provisioning.model-update"]
    assert "provisioning.push-firmware" in ext.unsupported_features


def test_both_installs_answered_support_both_features() -> None:
    ext = _provisioning([("C7.2", m.CheckStatus.PASS, ""),
                         ("C7.3", m.CheckStatus.PASS, ""),
                         ("C10.2", m.CheckStatus.PASS, "")])
    assert ext.supported_features == ["provisioning.model-update",
                                      "provisioning.push-firmware"]
