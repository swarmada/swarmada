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

"""Fleet Adapter conformance harness.

An executable driver for ``adapters/CONFORMANCE.md``. The harness is the
control-plane side of ``fleet_adapter.v1``: it stands up the gRPC server an
adapter-under-test dials into, drives it through scenarios that map to the
CONFORMANCE.md checks, and emits a pass/fail/skip report per check.

This is a *conformance test suite*, not a certification authority: it produces a
factual result that an adapter's authors self-attest to (ADR-0005, ADR-0007). It
runs no program and issues no mark.
"""

from .report import CheckResult, CheckStatus, Report

__all__ = ["CheckResult", "CheckStatus", "Report"]
