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

"""A report stored in conformance-reports/ names the adapter it describes.

Every stored report said ``adapter-under-test``, because no Makefile target passed
``--adapter-name``, so a stored report could not be matched to the registry row it
supports. The default name stays usable for a scratch run and is refused for a report
written into the repository's conformance-reports/.
"""

from __future__ import annotations

from adapters.conformance.__main__ import (
    DEFAULT_ADAPTER_NAME,
    REPORTS_DIR,
    refuses_default_name,
)


def test_the_default_name_is_refused_for_a_stored_report():
    assert refuses_default_name(DEFAULT_ADAPTER_NAME, str(REPORTS_DIR / "_adapter.json"))


def test_a_real_name_is_accepted_for_a_stored_report():
    assert not refuses_default_name("simulation", str(REPORTS_DIR / "_adapter.json"))


def test_the_default_name_is_accepted_outside_conformance_reports(tmp_path):
    assert not refuses_default_name(DEFAULT_ADAPTER_NAME, str(tmp_path / "report.json"))
    assert not refuses_default_name(DEFAULT_ADAPTER_NAME, None)
