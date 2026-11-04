# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""`make run` says what an install without admission webhooks leaves open.

Why this file exists. `make run` runs the manager as a local process with
`ENABLE_WEBHOOKS=false`, one of the four ways Swarmada runs with no admission webhooks.
Its startup warning named RobotClass defaulting and the admission gates only. It did not
say that the estop checks, the `robots/status` guard and the delete guard are off too,
so anyone who can write `robots/status` can set a stopped robot `Normal`.

The test prints the warning as `make run` does: `make -n` gives the recipe without running
it, and the warning's `if` block is then run on its own, so no cluster and no manager are
needed.
"""

from __future__ import annotations

import re
import shutil
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


def _warning(enable_webhooks: str) -> str:
    make = shutil.which("make")
    if make is None:
        pytest.fail("make is not on PATH; this test reads the Makefile's run recipe through it")
    recipe = subprocess.run(
        [make, "-n", "--no-print-directory", "run", f"ENABLE_WEBHOOKS={enable_webhooks}"],
        cwd=ROOT, capture_output=True, text=True, check=True).stdout
    block = re.search(r'^if \[ "\w+" = "false" \]; then.*?\bfi$', recipe, flags=re.M | re.S)
    assert block, f"the run recipe has no ENABLE_WEBHOOKS warning block:\n{recipe}"
    return subprocess.run(["sh", "-c", block.group(0)], capture_output=True, text=True,
                          check=True).stdout


def test_make_run_names_what_is_open_without_webhooks():
    text = " ".join(_warning("false").split())
    assert "admission webhooks are DISABLED" in text, text
    for claim in ("estop-trigger", "estop-clear", "cancel", "append", "activate", "spec.draft",
                  "robots/status", "estopState to Normal", "delete guard",
                  "docs/known-limitations.md"):
        assert claim in text, f"make run's warning does not name {claim!r}: {text!r}"


def test_the_docs_name_the_draft_gate_make_run_prints():
    # make run prints the draft gate (activate) among what it leaves open; the two documents
    # that list what an install without webhooks leaves open name it too.
    assert "activate" in " ".join(_warning("false").split())
    kl = " ".join((ROOT / "docs" / "known-limitations.md").read_text(encoding="utf-8").split())
    said = kl.split("`make run` prints what it leaves open when it starts:", 1)[1].split(".", 1)[0]
    assert "draft gate (`activate`)" in said, f"known-limitations' make run list: {said!r}"
    ops = " ".join((ROOT / "docs" / "operations.md").read_text(encoding="utf-8").split())
    assert "`cancel`, `append` and `activate` checks and the delete refusal do not run" in ops, (
        "operations.md's list of the checks an install without webhooks skips omits activate")


def test_make_run_prints_no_warning_with_webhooks_on():
    assert _warning("true") == ""
