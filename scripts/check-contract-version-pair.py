#!/usr/bin/env python3
"""check-contract-version-pair.py — fail when the contract version is written down inconsistently.

The fleet-adapter contract version (ADR-0032) is written down TWICE, and the file that holds
the first of them says so in its own comment:

    internal/contract/version.go
        "The conformance harness stamps this same value into a report as `contract_version`
         (adapters/conformance/report.py CONTRACT_VERSION); the two MUST move together."

`contract.Version` is what a control plane built from this tree implements, and what
`SupportedRange()` is derived from. `CONTRACT_VERSION` is what the harness stamps into a
conformance report as the version the result was EARNED against. ADR-0032 binds assignment
eligibility to the pair: a robot is dispatchable only when the negotiated contract version is
in range AND `status.conformance` was earned against a compatible version. If the two constants
drift, the harness attests a version the control plane does not implement — a report that reads
`Passed` for a contract nothing speaks.

WHY THIS EXISTS WHEN A GO TEST ALREADY ASSERTS IT
    `internal/contract/version_test.go` (`TestVersion_MatchesConformanceHarness`) makes the same
    assertion, and it is the better place for the assertion itself. It is not the better place
    for the GATE, for two reasons:

      * no gate names it, so nothing runs it as the check at the moment a version is bumped —
        the rule was correct and had no invoker, which is the defect class this repository
        keeps paying for; and
      * it calls `t.Skipf` when it cannot read `report.py`. A skipped test is a green test. A
        check that exits 0 without running is not a pass, and that is the exact failure the
        constant's own comment is trying to prevent.

    This check FAILS CLOSED on both of those: a missing file, an unreadable file or an absent
    constant is exit 2, never exit 0.

THE ADAPTER TEMPLATE IS THE THIRD VALUE, AND IT IS GATED
    `adapters/template/` is what a new adapter is generated from, and a generated adapter keeps
    the version it was generated with until its author bumps it. The supported window is N and
    N-1 minor, so a template shipped at N-1 hands every new adapter one release before eviction
    instead of two. Its `CONTRACT_VERSION` must therefore EQUAL `contract.Version`: a template
    one minor behind fails this check (exit 1), and a template with no constant cannot run it
    (exit 2).

WHAT IT DOES NOT CHECK
    Whether the value is CORRECT. All three constants moving together to a wrong number passes
    here. It also does not gate the other in-tree adapters' `CONTRACT_VERSION` constants: the
    simulation adapter declares one minor behind by default and contract.Version with --latch
    (LATCH_CONTRACT_VERSION), and example-noop is deliberately held one minor behind so the
    N/N-1 skew is exercised end to end; both are asserted through the real
    `Supports()` by `TestInTreeAdaptersReportASupportedContractVersion`. They are PRINTED here
    as context, because the moment someone reads this output is the moment they are bumping
    the contract, and a bump evicts any adapter the bumped contract.Version leaves two minors
    behind.

Exit 0 all agree · 1 they have drifted · 2 the check could not run.
"""

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

GO_SRC = ROOT / "internal/contract/version.go"
PY_SRC = ROOT / "adapters/conformance/report.py"
TEMPLATE_SRC = (
    ROOT / "adapters/template/{{cookiecutter.adapter_slug}}"
    / "{{cookiecutter.python_package}}/adapter.py"
)

GO_CONST = re.compile(r'(?m)^const\s+Version\s*=\s*"([^"]*)"')
PY_CONST = re.compile(r'(?m)^CONTRACT_VERSION\s*=\s*"([^"]*)"')
SEMVER = re.compile(r"^\d+\.\d+\.\d+$")

# Context only, never gated, except the template — see the docstring. Kept in step with the table in
# internal/contract/adapter_versions_test.go.
ADAPTERS = (
    ("simulation", "adapters/simulation/sim_adapter.py"),
    ("example-noop", "adapters/example-noop/noop_adapter.py"),
    (
        "template",
        "adapters/template/{{cookiecutter.adapter_slug}}/{{cookiecutter.python_package}}/adapter.py",
    ),
    ("ros2", "adapters/external/fleet-adapter-ros2/fleet_adapter_ros2/adapter.py"),
    ("vda5050", "adapters/external/fleet-adapter-vda5050/fleet_adapter_vda5050/adapter.py"),
    ("mavlink", "adapters/external/fleet-adapter-mavlink/fleet_adapter_mavlink/adapter.py"),
)


def cannot_run(msg: str) -> int:
    print(f"contract version pair — CANNOT RUN: {msg}", file=sys.stderr)
    print("  a gate that cannot find its inputs has not passed, it has not run", file=sys.stderr)
    return 2


def read_const(path: Path, pattern: re.Pattern, label: str):
    """The value, or a cannot-run reason. Absent is never treated as agreement."""
    if not path.is_file():
        return None, f"{path.relative_to(ROOT)} does not exist (it holds {label})"
    try:
        body = path.read_text(encoding="utf-8")
    except OSError as exc:
        return None, f"cannot read {path.relative_to(ROOT)}: {exc}"
    m = pattern.search(body)
    if m is None:
        return None, (
            f"{path.relative_to(ROOT)} declares no {label} — the constant was renamed "
            f"or removed, and this check can no longer see the half it guards"
        )
    return m.group(1), None


def supported_range(version: str) -> str:
    major, minor, _ = (int(p) for p in version.split("."))
    return f">={major}.{max(minor - 1, 0)}.0 <{major}.{minor + 1}.0"


def in_range(reported: str, version: str) -> bool:
    if not SEMVER.match(reported):
        return False
    r_major, r_minor, _ = (int(p) for p in reported.split("."))
    major, minor, _ = (int(p) for p in version.split("."))
    return r_major == major and max(minor - 1, 0) <= r_minor <= minor


def main() -> int:
    go_v, err = read_const(GO_SRC, GO_CONST, "contract.Version")
    if err:
        return cannot_run(err)
    py_v, err = read_const(PY_SRC, PY_CONST, "CONTRACT_VERSION")
    if err:
        return cannot_run(err)

    for path, value in ((GO_SRC, go_v), (PY_SRC, py_v)):
        if not SEMVER.match(value):
            return cannot_run(
                f'{path.relative_to(ROOT)} holds "{value}", which is not a plain '
                f"major.minor.patch semver — ADR-0032's range arithmetic is "
                f"undefined on it"
            )

    print(
        f"contract version pair — internal/contract/version.go = {go_v} · "
        f"adapters/conformance/report.py = {py_v}"
    )

    if go_v != py_v:
        print()
        print(
            f"  DRIFT. contract.Version = {go_v} but the harness stamps "
            f"CONTRACT_VERSION = {py_v}."
        )
        print("  The harness would attest a conformance result against a contract version this")
        print("  control plane does not implement, and ADR-0032 binds AssignTask eligibility to")
        print("  that pair. The two MUST be bumped together (internal/contract/version.go:45).")
        return 1

    print(f"  agreed · derived supported range {supported_range(go_v)}")

    tpl_v, err = read_const(TEMPLATE_SRC, PY_CONST, "the adapter template's CONTRACT_VERSION")
    if err:
        return cannot_run(err)
    if tpl_v != go_v:
        print()
        print(f"  TEMPLATE BEHIND. adapters/template declares CONTRACT_VERSION = {tpl_v}, but")
        print(f"  contract.Version = {go_v}. Every adapter generated from it starts at {tpl_v} and")
        print("  keeps it until its author moves it, so it would fall out of the N/N-1 window one")
        print("  release sooner than an adapter generated at the current contract. The template")
        print("  ships at contract.Version; move it with the contract.")
        return 1
    print(f"  adapter template declares {tpl_v} · equal to contract.Version, as required")

    print()
    print("  in-tree adapter declarations (context, NOT gated here — ADR-0032 allows N-1):")
    for name, rel in ADAPTERS:
        if name == "template":
            continue  # gated above
        path = ROOT / rel
        if not path.is_file():
            print(f"    {name:<12} not in this repository")
            continue
        m = PY_CONST.search(path.read_text(encoding="utf-8"))
        if m is None:
            print(f"    {name:<12} declares no CONTRACT_VERSION")
            continue
        mark = "in range" if in_range(m.group(1), go_v) else "OUT OF RANGE — registrations refused"
        print(f"    {name:<12} {m.group(1):<8} {mark}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
