# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""CLI for the cross-surface conformance suite.

    # every surface this repository can drive, in one go
    make conformance-profiles

PREFER THE MAKE TARGETS. Any fixture speaking compute.v1 over gRPC or MCP imports
the generated stubs, so it needs `proto/` on PYTHONPATH; `make conformance-profiles`,
`make conformance-endpoint` and `make conformance-controlplane` set it. Invoked
bare, the `all` subcommand reaches the first gRPC fixture and dies with
`ModuleNotFoundError: No module named 'fleet_adapter'` -- the suite is fine, the
path is not. The equivalent by hand:

    PYTHONPATH=proto python3 -m conformance all --adapter-report report.json

The HTTP fixture and the control-plane surface import no stubs and run bare:

    # one Compute Endpoint, against an in-tree fixture it starts itself
    python3 -m conformance compute-endpoint --endpoint plate-reader-http

    # one Compute Endpoint, against something you deployed
    python3 -m conformance compute-endpoint --endpoint plate-reader-grpc \\
        --target 10.0.0.4:9443

    # the deployment, statically, with no cluster
    python3 -m conformance control-plane

    # an adapter driving a physical robot: the plant.yaml is what makes it a hardware grade
    python3 -m conformance fleet-adapter --adapter-report report.json \\
        --mode physical --plant plant.yaml --robot-model <model> --firmware <version>

Every report grades each row (conformance/PROFILES.md, "The capability sheet") and
states a verdict. Exit status is the worst verdict across the reports written:
0 pass · 1 fail · 3 not-established (nothing failed; a required Core row was not
measured) · 2 the run was refused or could not start.
"""

from __future__ import annotations

import argparse
import datetime as _dt
import socket
import sys
import time
from pathlib import Path

from . import adapter as adapter_ingest
from . import computeendpoint, controlplane, reactive
from .model import (
    ConformanceReport,
    Fidelity,
    FidelityRefused,
    ResultSet,
    Verdict,
    plant_block,
    resolve_fidelity,
)
from .registry import Registry, self_checks

REPO = Path(__file__).resolve().parent.parent


def _now() -> str:
    # `_dt.timezone.utc`, not `_dt.UTC`: the alias arrived in Python 3.11 and this
    # package has to import on 3.10 (conformance/README.md, Prerequisites).
    return _dt.datetime.now(_dt.timezone.utc).replace(microsecond=0).isoformat().replace(
        "+00:00", "Z")


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _wait(port: int, timeout: float = 5.0) -> None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), 0.2):
                return
        except OSError:
            time.sleep(0.05)
    raise RuntimeError(f"fixture on port {port} never came up")


# ── Surfaces ─────────────────────────────────────────────────────────────────

def run_compute_endpoint(args, reg: Registry) -> ConformanceReport:
    rs = ResultSet("ComputeEndpoint")
    decl = computeendpoint.load_declaration(args.endpoint, rs)
    binding = decl["spec"]["binding"]
    contract = decl["spec"]["contract"]

    started_fixture = None
    target = args.target
    if not target:
        port = _free_port()
        if binding == "Http":
            from .fixtures import plate_reader_http as fx
            started_fixture, _ = fx.serve(port, args.misbehave)
            target = f"http://127.0.0.1:{port}"
        elif binding == "Mcp":
            from .fixtures import plate_reader_mcp as fx
            started_fixture, _ = fx.serve(port, args.misbehave)
            target = f"http://127.0.0.1:{port}"
        else:
            from .fixtures import plate_reader_grpc as fx
            started_fixture, _ = fx.serve(port, "service" if contract == "Service"
                                          else "app", args.misbehave)
            target = f"127.0.0.1:{port}"
        _wait(port)
        rs.artefacts.append(fx.__file__)

    driver_cls = {"Http": computeendpoint.HttpDriver, "Mcp": computeendpoint.McpDriver,
                  "Grpc": computeendpoint.GrpcDriver}[binding]
    driver = driver_cls(target)
    try:
        applicable = computeendpoint.run(driver, decl, rs, run_id=args.run_id)
    finally:
        if started_fixture is not None:
            if hasattr(started_fixture, "shutdown"):
                started_fixture.shutdown()          # http.server
            else:
                started_fixture.stop(0)             # grpc.Server

    self_checks(reg, "ComputeEndpoint", rs, "CE")
    return ConformanceReport(
        surface="ComputeEndpoint",
        implementation={"organization": args.org, "project": args.endpoint,
                        "version": args.impl_version,
                        "url": "https://github.com/swarmada/swarmada"},
        date=_now(), protocol_version="compute.v1", contract_version=args.contract_version,
        suite_revision=reg.revision, mode=args.mode, capability_tier="ReadOnly",
        standalone=not args.target,
        profiles=reg.profile_results("ComputeEndpoint", rs, tier="ReadOnly",
                                     applicable=applicable),
        results=rs.results, target={"binding": binding, "contract": contract},
        context={"endpoint": target if args.target else "in-tree fixture"},
        # An endpoint has no plant of its own, so it is never graded as hardware.
        fidelity=resolve_fidelity(args.mode, None))


def run_control_plane(args, reg: Registry) -> ConformanceReport:
    rs = ResultSet("ControlPlane")
    applicable = controlplane.run(rs, deployment=args.deployment)
    self_checks(reg, "ControlPlane", rs, "CP")
    return ConformanceReport(
        surface="ControlPlane",
        implementation={"organization": args.org, "project": args.deployment,
                        "version": args.impl_version,
                        "url": "https://github.com/swarmada/swarmada"},
        date=_now(), protocol_version="swarmada.io/v1",
        contract_version=args.contract_version, suite_revision=reg.revision,
        mode="static", capability_tier="NotApplicable", standalone=True,
        profiles=reg.profile_results("ControlPlane", rs, tier="", applicable=applicable),
        results=rs.results, fidelity=Fidelity.SIMULATED)


def run_fleet_adapter(args, reg: Registry) -> ConformanceReport:
    plant_doc, plant = None, None
    if args.plant:
        import yaml
        plant_path = Path(args.plant)
        plant_doc = yaml.safe_load(plant_path.read_text())
        if not isinstance(plant_doc, dict):
            raise FidelityRefused(f"{plant_path} is not a plant.yaml document")
        plant = plant_block(plant_path, plant_doc, robot_model=args.robot_model,
                            firmware=args.firmware)
    fidelity = resolve_fidelity(args.mode, plant_doc)
    rs = ResultSet("FleetAdapter")
    doc = adapter_ingest.ingest(Path(args.adapter_report), rs)
    applicable = adapter_ingest.applicability(rs)
    known = set(reg.bindings("FleetAdapter")) | set(reg.excluded("FleetAdapter"))
    # ADR-0071: on a plant declaration, C5.3 is graded against the platform precondition.
    adapter_ingest.apply_platform_precondition(rs, plant_doc)
    applicable.update(reactive.run(rs, known,
                                   adapter_grammar_version=args.reactive_grammar_version))
    # RB0.2 checks the MERGED result set -- the adapter harness's C0.1 saw only
    # its own skips, and the reactive rows arrived after it ran.
    self_checks(reg, "FleetAdapter", rs, "RB")
    return ConformanceReport(
        surface="FleetAdapter",
        implementation={"organization": args.org, "project": doc.get("adapter", "?"),
                        "version": args.impl_version,
                        "url": "https://github.com/swarmada/swarmada"},
        date=_now(), protocol_version=doc.get("protocol_version", "fleet_adapter.v1"),
        contract_version=doc.get("contract_version", args.contract_version),
        suite_revision=reg.revision, mode=args.mode, capability_tier=args.tier,
        standalone=True,
        profiles=reg.profile_results("FleetAdapter", rs, tier=args.tier,
                                     applicable=applicable),
        results=rs.results, fidelity=fidelity, plant=plant,
        measurements=rs.context.get("measurements_by_check") or {})


# ── CLI ──────────────────────────────────────────────────────────────────────

def _emit(report: ConformanceReport, args, label: str) -> None:
    print(report.to_text())
    print()
    if args.out:
        out = Path(args.out)
        if out.is_dir() or args.command == "all":
            out.mkdir(parents=True, exist_ok=True)
            out = out / f"{label}.yaml"
        out.write_text(report.to_yaml())
        print(f"wrote {out}")


def main() -> int:
    ap = argparse.ArgumentParser(prog="conformance", description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("command", choices=["compute-endpoint", "control-plane",
                                        "fleet-adapter", "all"])
    ap.add_argument("--endpoint", default="plate-reader-http",
                    help="name of a ComputeEndpoint declaration to test against")
    ap.add_argument("--target", default="",
                    help="an endpoint you deployed; omitted, an in-tree fixture is started")
    ap.add_argument("--misbehave", default="",
                    help="ask the fixture to exhibit a named defect, so the check that "
                         "catches it can be seen to fail")
    ap.add_argument("--adapter-report", default="",
                    help="JSON written by `python -m adapters.conformance --json`")
    ap.add_argument("--reactive-grammar-version", default=None)
    ap.add_argument("--tier", default="FullControl",
                    choices=["ReadOnly", "TrafficLight", "FullControl"])
    ap.add_argument("--deployment", default="swarmada")
    ap.add_argument("--org", default="swarmada")
    ap.add_argument("--impl-version", default="0.x")
    ap.add_argument("--contract-version", default="1.0.0")
    ap.add_argument("--mode", default="simulated", choices=["simulated", "physical"],
                    help="physical is refused unless --plant names an operator-declared "
                         "hardware plant.yaml")
    ap.add_argument("--plant", default="",
                    help="the plant.yaml of the robot under test (conformance/PLANT.md); "
                         "its digest is recorded as the report's subject")
    ap.add_argument("--robot-model", default=None,
                    help="the robot model, as the operator states it; plant.yaml has no field "
                         "for it")
    ap.add_argument("--firmware", default=None,
                    help="the robot firmware version, as the operator states it")
    ap.add_argument("--run-id", default="conformance")
    ap.add_argument("--out", default="")
    args = ap.parse_args()

    reg = Registry()
    reports: list[tuple[str, ConformanceReport]] = []
    try:
        _run(args, ap, reg, reports)
    except FidelityRefused as exc:
        print(f"conformance: refused: {exc}", file=sys.stderr)
        return 2

    if args.out:
        stored = (REPO / "conformance-reports").resolve()
        out = Path(args.out).resolve()
        into_reports = out == stored or stored in out.parents
        unnamed = [label for label, r in reports if r.surface == "FleetAdapter"
                   and r.implementation.get("project") == "adapter-under-test"]
        if into_reports and unnamed:
            print("conformance: refused: the FleetAdapter report names 'adapter-under-test', "
                  "and --out writes into conformance-reports/. Run the harness with "
                  "--adapter-name set to the adapter's registry name", file=sys.stderr)
            return 2
    for label, report in reports:
        _emit(report, args, label)
    verdicts = {report.verdict for _, report in reports}
    if Verdict.FAIL in verdicts:
        return 1
    return 3 if Verdict.NOT_ESTABLISHED in verdicts else 0


def _run(args, ap, reg: Registry, reports: list[tuple[str, ConformanceReport]]) -> None:
    if args.command in ("compute-endpoint", "all"):
        names = ([args.endpoint] if args.command == "compute-endpoint"
                 else ["plate-reader-http", "plate-reader-grpc", "plate-reader-mcp",
                       "plate-watcher-grpc"])
        for name in names:
            args.endpoint = name
            reports.append((f"compute-endpoint-{name}", run_compute_endpoint(args, reg)))
    if args.command in ("control-plane", "all"):
        reports.append(("control-plane", run_control_plane(args, reg)))
    if args.command in ("fleet-adapter", "all"):
        if args.adapter_report:
            reports.append(("fleet-adapter", run_fleet_adapter(args, reg)))
        elif args.command == "fleet-adapter":
            ap.error("--adapter-report is required: run `make conformance "
                     "CONFORMANCE_ARGS='--json report.json'` first")
        else:
            print("note: no --adapter-report given, so the FleetAdapter surface was not "
                  "profiled. Run `make conformance CONFORMANCE_ARGS='--json report.json'` "
                  "and pass it with --adapter-report.\n", file=sys.stderr)


if __name__ == "__main__":
    raise SystemExit(main())
