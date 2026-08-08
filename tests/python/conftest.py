# Copyright 2026 The Swarmada Authors.
#

"""Ensure the repository root is importable so in-tree packages (adapters,
simulation, ml, sdk) resolve under pytest regardless of the editable-install
layout."""

import pathlib
import sys

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
if str(_REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(_REPO_ROOT))
