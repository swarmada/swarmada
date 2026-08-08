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

"""Python SDK for the Swarmada fleet-orchestration control plane.

The public surface is intentionally small. It currently exposes the audited Fleet
Adapter safety primitives (:mod:`swarmada_sdk.safety`) so every adapter — reference
or vendor — shares one correct implementation of the CONFORMANCE.md safety MUSTs
(C3 fencing, C4 lease self-stop, C5 confirmed estop) rather than re-deriving them.
The vendor cookiecutter template (``adapters/template``) generates an adapter that
depends on this package.
"""

from swarmada_sdk.safety import (
    ESTOP_FAILED,
    ESTOP_STOPPED,
    FenceDecision,
    FenceGuard,
    LeaseMonitor,
    confirm_estop,
)

__all__ = [
    "__version__",
    "FenceDecision",
    "FenceGuard",
    "LeaseMonitor",
    "confirm_estop",
    "ESTOP_STOPPED",
    "ESTOP_FAILED",
]

__version__ = "0.0.1"
