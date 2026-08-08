# {{cookiecutter.adapter_name}}

A **safety-complete** Swarmada Fleet Adapter for `{{cookiecutter.robot_class}}`,
generated from the Swarmada vendor template. Feature-basic but safe: the
CONFORMANCE.md safety MUSTs (C3 fencing, C4 lease self-stop, C5 confirmed estop)
come from the audited `swarmada-sdk`; optional commands are declined with
`unsupported = true` (C7).

## Make it yours

Replace `SimulatedRobot` in `{{cookiecutter.python_package}}/robot.py` with a
`RobotBinding` to your fleet API. Preserve the confirmed-stop contract:
`is_stopped()` returns True only when the robot has *actually* halted — never a
guess. Each `TODO(vendor)` marks a binding point.

## Develop

```bash
pip install -e '.[dev]' swarmada-sdk
make test          # safety-wiring tests
make conformance   # drive against the Swarmada conformance harness (C0-C16)
```

Register in the Swarmada adapter registry once you pass conformance.
