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

"""Ensure the repository root is importable so in-tree packages (adapters,
simulation, ml) resolve under pytest regardless of the editable-install layout.

The SDK needs its own entry. It is a SEPARATE distribution rooted at
``sdk/python/src`` (sdk/python/pyproject.toml, ``where = ["src"]``) and is not
part of the root editable install, so the repo root alone does not make
``swarmada_sdk`` importable -- this docstring used to claim it did.
"""

import pathlib
import sys

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
if str(_REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(_REPO_ROOT))

_SDK_SRC = _REPO_ROOT / "sdk" / "python" / "src"
if str(_SDK_SRC) not in sys.path:
    sys.path.insert(1, str(_SDK_SRC))
