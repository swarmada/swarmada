# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""The manager image stamps the control-plane version at link time.

Why this file exists. `cmd/manager/main.go` stamps `main.buildVersion` onto every audit
entry as `swarmada_version`, and `internal/metrics.BuildVersion` is the `swarmada_version`
metric's label. Both are set only by the linker. The image was built with `-ldflags="-s -w"`
alone, so every audit entry an image wrote read `dev`, and the metric `unknown`, whatever
version the chart deployed.

THE RULE. The Dockerfile's builder stage takes a `VERSION` build argument (default `dev`)
and passes it to both variables with `-X`. `make docker-build` passes the chart's
`appVersion` as `VERSION`, in the component form `X.Y.Z`, with no `v`
(`docs/release-process.md`, *Versioning*).
"""

import re
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def _module() -> str:
    m = re.search(r"(?m)^module\s+(\S+)\s*$", (ROOT / "go.mod").read_text())
    assert m, "go.mod names no module"
    return m.group(1)


def _builder_stage() -> str:
    text = (ROOT / "Dockerfile").read_text().replace("\\\n", " ")
    for stage in re.split(r"(?m)^FROM\s+", text)[1:]:
        if re.match(r"\S+\s+AS\s+builder\b", stage):
            return stage
    raise AssertionError("Dockerfile has no stage named builder")


def _chart_app_version() -> str:
    chart = (ROOT / "deploy/swarmada/Chart.yaml").read_text()
    m = re.search(r'(?m)^appVersion:\s*"?([^"\s]+)"?\s*$', chart)
    assert m, "Chart.yaml declares no appVersion"
    return m.group(1)


def test_builder_stage_takes_a_version_argument_defaulting_to_dev():
    assert re.search(r"(?m)^ARG VERSION=dev\s*$", _builder_stage())


def test_manager_build_stamps_both_version_variables():
    m = re.search(r'go build [^\n]*-ldflags="([^"]*)"[^\n]*\./cmd/manager', _builder_stage())
    assert m, "no go build of ./cmd/manager with -ldflags in the builder stage"
    flags = m.group(1)
    assert "-X main.buildVersion=${VERSION}" in flags
    assert f"-X {_module()}/internal/metrics.BuildVersion=${{VERSION}}" in flags


def test_chart_app_version_is_the_component_form():
    assert re.fullmatch(r"\d+\.\d+\.\d+", _chart_app_version())


def test_make_docker_build_passes_the_chart_app_version():
    out = subprocess.run(
        ["make", "--no-print-directory", "-n", "docker-build", "IMG=example.invalid/swarmada:test"],
        cwd=ROOT, capture_output=True, text=True, check=True,
    ).stdout
    assert f"--build-arg VERSION={_chart_app_version()}" in out, out
