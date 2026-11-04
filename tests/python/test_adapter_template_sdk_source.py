# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""A generated adapter never asks a package index for swarmada-sdk.

The SDK is not published on any index, so its name there belongs to whoever registers it. A
generated adapter that lists it by bare name installs that stranger's package the first time
pip resolves the adapter on its own (a fresh venv, `--target`, `-U`, a lock tool, a container
layer), and that package would supply the fencing, lease self-stop and confirmed-estop code
(RFC-0001 C3/C4/C5). `check-adapter-template` never calls pip; it puts sdk/python/src on
PYTHONPATH, so it could not see this. This reads the rendered requirements as a resolver
would, and the rendered install commands as a shell would. It needs no network.
"""

from __future__ import annotations

import importlib.util
import re
import shlex
import tomllib
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SDK = "swarmada-sdk"
_NAME = re.compile(r"\s*([A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?)")


def _render(dest: Path) -> Path:
    """Render the template with its defaults, as scripts/check-adapter-template.py does."""
    spec = importlib.util.spec_from_file_location(
        "check_adapter_template", ROOT / "scripts" / "check-adapter-template.py"
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    mod.render(dest)
    (project,) = [p for p in dest.iterdir() if p.is_dir()]
    return project


def _canonical(name: str) -> str:
    return re.sub(r"[-_.]+", "-", name).lower()


def _from_index(req: str) -> bool:
    """True when `req` is a PEP 508 requirement on the SDK that an index would satisfy."""
    m = _NAME.match(req)
    if not m or _canonical(m.group(1)) != SDK:
        return False
    # `name[extras] @ url ; marker` is a direct reference, never an index lookup.
    return "@" not in req.split(";", 1)[0]


def test_from_index_reads_requirements_as_a_resolver_does() -> None:
    # The check itself, so a pass below cannot come from a matcher that matches nothing.
    for req in (
        "swarmada-sdk",
        "swarmada_sdk>=0.1",
        "Swarmada.SDK[dev]",
        "swarmada-sdk; python_version>'3'",
    ):
        assert _from_index(req), req
    for req in (
        "swarmada-sdk @ file:///opt/swarmada/sdk/python",
        "swarmada-sdk @ git+https://example.invalid/swarmada.git#subdirectory=sdk/python",
        "swarmada-sdk-extras",
        "grpcio>=1.64.1",
    ):
        assert not _from_index(req), req


def test_a_generated_adapter_does_not_resolve_the_sdk_from_an_index(tmp_path: Path) -> None:
    data = tomllib.loads((_render(tmp_path / "render") / "pyproject.toml").read_text())
    project = data["project"]
    reqs = [*project.get("dependencies", []), *data.get("build-system", {}).get("requires", [])]
    for extra in project.get("optional-dependencies", {}).values():
        reqs += extra
    from_index = [r for r in reqs if _from_index(r)]
    assert not from_index, (
        f"a generated adapter requires {from_index!r} by name, so pip resolves it from a package "
        "index, where the name is not ours. Do not list it by name: install it from a Swarmada "
        "checkout (adapters/template/README.md, 'Use it')."
    )


def test_a_generated_adapter_never_installs_the_sdk_by_name(tmp_path: Path) -> None:
    # The same hole through a command rather than metadata: a rendered README, Makefile or
    # workflow that runs `pip install swarmada-sdk` asks the index for it just the same.
    project = _render(tmp_path / "render")
    offenders = []
    for path in sorted(p for p in project.rglob("*") if p.is_file()):
        for n, line in enumerate(path.read_text(errors="replace").splitlines(), 1):
            if "pip install" not in line:
                continue
            try:
                words = shlex.split(line.split("pip install", 1)[1], comments=True)
            except ValueError:
                words = line.split("pip install", 1)[1].split()
            if any(_from_index(w) for w in words if not w.startswith("-")):
                offenders.append(f"{path.relative_to(project)}:{n}: {line.strip()}")
    assert not offenders, (
        "a generated adapter installs swarmada-sdk by name, which resolves it from a package "
        f"index: {offenders!r}. Install it from a Swarmada checkout by path."
    )
