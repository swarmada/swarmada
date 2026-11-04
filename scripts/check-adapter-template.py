#!/usr/bin/env python3
"""check-adapter-template.py — render the vendor adapter template, then check what it renders.

WHY THIS EXISTS. `adapters/template/` is the first thing a new adapter author copies, and it
was the one Python source in the tree that no gate read. It is a cookiecutter template: every
`.py` file in it carries `{{cookiecutter.*}}` in an import, a class name or a copyright line,
so it is not Python until it is rendered, and `ruff check .` has to exclude it
(pyproject.toml, `extend-exclude`). Excluded and never rendered, a syntax error, a lint
finding or a broken test in it reached every adapter generated from it, and nothing here
could see it.

WHAT IT DOES. Renders the template with the defaults in `cookiecutter.json` into a scratch
directory, then, on the rendered copy:

  1. parses `pyproject.toml` (tomllib) and every `.yml`/`.yaml` (PyYAML);
  2. compiles every `.py`;
  3. runs ruff under THIS repository's rule set (`--config pyproject.toml`), which is what
     `ruff check .` would have applied had the files been parseable where they sit;
  4. runs the rendered adapter's own tests, with the in-tree SDK on the path.

THE RENDERER IS DELIBERATELY SMALL. It substitutes `{{ cookiecutter.KEY }}` for KEYs that
`cookiecutter.json` declares and nothing else, so the gate needs no dependency beyond the
dev extra. It REFUSES, with exit 2, any other template construct left after substitution --
a `{% if %}`, a filter, a `{% raw %}` block -- rather than rendering it wrongly: the day the
template outgrows plain substitution, this says so instead of checking something that is not
what cookiecutter would produce. Use cookiecutter then.

Usage: python3 scripts/check-adapter-template.py [--ruff CMD] [--keep DIR]
  --ruff CMD   the ruff command line to run (default: `ruff`); `make` passes the pinned one
  --keep DIR   render into DIR and leave it there, for inspection
Exit:  0 clean · 1 findings · 2 the check could not run.
       A check that exits without having run is not a pass.
"""

from __future__ import annotations

import json
import os
import re
import shlex
import subprocess
import sys
import tempfile
import tomllib
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
TEMPLATE = ROOT / "adapters" / "template"
SDK_SRC = ROOT / "sdk" / "python" / "src"

SUBST = re.compile(r"\{\{\s*cookiecutter\.(\w+)\s*\}\}")
# Anything that still looks like Jinja once the substitutions are done.
LEFTOVER = re.compile(r"\{\{|\{%|\{#")
SKIP = {"__pycache__", ".DS_Store", ".pytest_cache", ".ruff_cache"}


class CannotRunError(Exception):
    """The check could not run. Never a pass."""


def render(dest: Path) -> list[Path]:
    """Render the template into dest; return the rendered files."""
    spec = TEMPLATE / "cookiecutter.json"
    try:
        context = {
            k: str(v) for k, v in json.loads(spec.read_text()).items() if not k.startswith("_")
        }
    except (OSError, ValueError) as err:
        raise CannotRunError(f"cannot read {spec.relative_to(ROOT)}: {err}") from err

    roots = [p for p in TEMPLATE.iterdir() if p.is_dir() and "{{" in p.name]
    if len(roots) != 1:
        raise CannotRunError(
            f"expected exactly one templated project directory in {TEMPLATE.relative_to(ROOT)}, "
            f"found {len(roots)}"
        )

    def sub(text: str, where: str) -> str:
        def one(m: re.Match[str]) -> str:
            if m.group(1) not in context:
                raise CannotRunError(
                    f"{where}: cookiecutter.{m.group(1)} is not in cookiecutter.json"
                )
            return context[m.group(1)]

        out = SUBST.sub(one, text)
        left = LEFTOVER.search(out)
        if left:
            line = out.count("\n", 0, left.start()) + 1
            raise CannotRunError(
                f"{where}:{line}: a template construct other than {{{{cookiecutter.KEY}}}} "
                "remains after substitution. This renderer does substitution only; render "
                "with cookiecutter and check that instead."
            )
        return out

    rendered: list[Path] = []
    for src in sorted(roots[0].rglob("*")):
        if src.is_dir() or SKIP & set(src.parts) or src.suffix == ".pyc":
            continue
        rel = src.relative_to(TEMPLATE)
        target = dest / sub(str(rel), str(rel))
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(sub(src.read_text(encoding="utf-8"), str(rel)), encoding="utf-8")
        rendered.append(target)
    if not rendered:
        raise CannotRunError("the template rendered no files")
    return rendered


def check(project: Path, files: list[Path], ruff: list[str]) -> tuple[list[str], list[str]]:
    """Return (findings, what was checked)."""
    findings: list[str] = []
    done: list[str] = []

    try:
        import yaml
    except ImportError as err:
        raise CannotRunError("PyYAML is not installed (it is a base dependency)") from err

    def rel(p: Path) -> str:
        return str(p.relative_to(project.parent))

    tomls = [f for f in files if f.name == "pyproject.toml"]
    if not tomls:
        findings.append("the rendered project has no pyproject.toml")
    for f in tomls:
        try:
            tomllib.loads(f.read_text())
        except tomllib.TOMLDecodeError as err:
            findings.append(f"{rel(f)}: does not parse as TOML: {err}")
    yamls = [f for f in files if f.suffix in (".yml", ".yaml")]
    for f in yamls:
        try:
            yaml.safe_load(f.read_text())
        except yaml.YAMLError as err:
            findings.append(f"{rel(f)}: does not parse as YAML: {err}")
    done.append(f"{len(tomls)} TOML + {len(yamls)} YAML parsed")

    pys = [f for f in files if f.suffix == ".py"]
    for f in pys:
        try:
            compile(f.read_text(), rel(f), "exec")
        except SyntaxError as err:
            findings.append(f"{rel(f)}:{err.lineno}: does not compile: {err.msg}")
    done.append(f"{len(pys)} .py compiled")

    # `src` is the rendered project, as it is in the author's own repository: without it isort
    # reads the generated package as third-party and reports an import order no author sees.
    cmd = [
        *ruff, "check", "--no-cache",
        "--config", str(ROOT / "pyproject.toml"),
        "--config", f"src = [{json.dumps(str(project))}]",
        project.name,
    ]
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, cwd=project.parent)
    except OSError as err:
        raise CannotRunError(f"cannot run ruff ({shlex.join(ruff)}): {err}") from err
    if r.returncode == 1:
        findings.append("ruff, under this repository's rules:\n" + r.stdout.rstrip())
    elif r.returncode != 0:
        raise CannotRunError(f"ruff exited {r.returncode}: {(r.stderr or r.stdout).strip()}")
    done.append("ruff clean" if r.returncode == 0 else "ruff FOUND")

    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join(
        p for p in (str(SDK_SRC), str(project), env.get("PYTHONPATH", "")) if p
    )
    cmd = [sys.executable, "-m", "pytest", "tests", "-q", "-p", "no:cacheprovider"]
    r = subprocess.run(cmd, capture_output=True, text=True, cwd=project, env=env)
    tail = (r.stdout.strip().splitlines() or ["(no output)"])[-1]
    if r.returncode == 5:
        findings.append("the rendered tests collected nothing: a test suite that runs no test")
    elif r.returncode != 0 and "No module named pytest" in r.stderr:
        raise CannotRunError("pytest is not installed for " + sys.executable)
    elif r.returncode != 0:
        findings.append(f"the rendered adapter's own tests failed:\n{r.stdout.rstrip()}")
    done.append(f"rendered tests: {tail}")
    return findings, done


def main() -> int:
    argv = sys.argv[1:]
    ruff = ["ruff"]
    keep: Path | None = None
    while argv:
        arg = argv.pop(0)
        if arg == "--ruff" and argv:
            ruff = shlex.split(argv.pop(0))
        elif arg == "--keep" and argv:
            keep = Path(argv.pop(0)).resolve()
        else:
            print(f"check-adapter-template: unknown argument {arg!r}", file=sys.stderr)
            return 2

    try:
        with tempfile.TemporaryDirectory(prefix="adapter-template-") as scratch:
            dest = keep or Path(scratch)
            files = render(dest)
            projects = {f.relative_to(dest).parts[0] for f in files}
            if len(projects) != 1:
                raise CannotRunError(f"the template rendered {len(projects)} projects, expected 1")
            findings, done = check(dest / projects.pop(), files, ruff)
    except CannotRunError as err:
        print(f"check-adapter-template: could not run: {err}", file=sys.stderr)
        return 2

    if findings:
        print("adapters/template renders an adapter with findings:\n")
        for f in findings:
            print(f"  {f}\n")
        print("Fix them in the template, where every generated adapter inherits them.")
        return 1
    print(f"check-adapter-template: {len(files)} files rendered; " + "; ".join(done))
    return 0


if __name__ == "__main__":
    sys.exit(main())
