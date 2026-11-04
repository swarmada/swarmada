# {{cookiecutter.adapter_name}}

A Swarmada Fleet Adapter for `{{cookiecutter.robot_class}}`, generated from the
Swarmada vendor template. Feature-basic: the CONFORMANCE.md safety MUSTs (C3
fencing, C4 lease self-stop, C5 confirmed estop) come from the shared
`swarmada-sdk`; a dropped stream is redialled with bounded backoff (C1.5); optional
commands are declined with `unsupported = true` (C7).

## Make it yours

Replace `SimulatedRobot` in `{{cookiecutter.python_package}}/robot.py` with a
`RobotBinding` to your fleet API. Preserve the confirmed-stop contract:
`is_stopped()` returns True only when the robot has *actually* halted — never a
guess — and `arrived()` only when it has reached the target it was sent to, since
the adapter reports the action `SUCCEEDED` on it. Each `TODO(vendor)` marks a
binding point.

## Develop

`swarmada-sdk`, the conformance harness and the `fleet_adapter.v1` Python stubs
come from a checkout of the Swarmada repository, not from any package index.
`pyproject.toml` does not list `swarmada-sdk`, so pip never looks the name up on
an index. Install it from the checkout, as below, and never by name. Python 3.10+.

```bash
pip install path/to/swarmada/sdk/python -e '.[dev]'
make test                                    # safety-wiring tests
make conformance SWARMADA=path/to/swarmada   # the conformance harness (C0-C19)
make profile SWARMADA=path/to/swarmada       # the profiled verdict
python -m {{cookiecutter.python_package}}.adapter --endpoint <control-plane>:<port> \
  --namespace {{cookiecutter.namespace}}     # PYTHONPATH must include path/to/swarmada/proto
```

Results earned with `SimulatedRobot` describe the template, not your binding:
re-run the harness once the binding is yours, and register in the Swarmada
adapter registry with that result.
