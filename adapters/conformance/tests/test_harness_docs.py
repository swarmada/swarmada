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

"""The harness must describe the catalog it actually drives.

The module docstring named "C1-C12" and "Scope: all of C1-C10" while
the file recorded ids from ``C0.1`` to ``C16.1``. A reader auditing coverage
against the docstring would conclude six families were out of scope that are not.
"""

from __future__ import annotations

import re
from pathlib import Path

_SRC = (Path(__file__).resolve().parents[1] / "harness.py").read_text()
_RECORDED = re.compile(r'"(C\d+\.\d+)"')


def _families(ids):
    return {int(i.split(".")[0][1:]) for i in ids}


def test_docstring_states_the_range_the_file_records():
    doc = _SRC.split('"""', 2)[1]
    recorded = _families(set(_RECORDED.findall(_SRC)))
    lo, hi = min(recorded), max(recorded)

    assert f"C{lo}" in doc and f"C{hi}" in doc, (
        f"the file records C{lo}-C{hi}; the docstring says: {doc.splitlines()[0]!r}")
    for stale in ("C1–C12", "C1-C12", "Scope: all of C1–C10", "Scope: all of C1-C10"):
        assert stale not in doc, f"stale range {stale!r} still in the docstring"


def test_every_family_the_docstring_claims_is_recorded():
    """The docstring must not claim a family the file never records, either."""
    doc = _SRC.split('"""', 2)[1]
    recorded = _families(set(_RECORDED.findall(_SRC)))
    claimed = {int(m) for m in re.findall(r"\bC(\d+)\b", doc)}
    assert claimed <= recorded, f"docstring names unrecorded families: {sorted(claimed - recorded)}"
