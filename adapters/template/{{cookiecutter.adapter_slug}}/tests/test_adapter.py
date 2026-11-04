# Copyright {{cookiecutter.year}} {{cookiecutter.author}}
# SPDX-License-Identifier: Apache-2.0

"""Safety-wiring tests for {{cookiecutter.adapter_name}}.

The safety MUSTs themselves (C3/C4/C5) are tested in `swarmada-sdk`; these confirm
this adapter WIRES them correctly and that the default robot binding honours the
confirmed-stop contract. Add your own tests as you bind the real fleet API.
"""

from __future__ import annotations

from swarmada_sdk.safety import ESTOP_FAILED, ESTOP_STOPPED, confirm_estop

from {{cookiecutter.python_package}}.robot import SimulatedRobot


def test_default_robot_confirms_a_real_stop() -> None:
    # C5: STOPPED only when the robot has actually halted.
    robot = SimulatedRobot()
    robot.spawn("r1")
    robot.command_move("r1", 5.0, 0.0)
    assert confirm_estop(robot, "r1") == ESTOP_STOPPED
    assert robot.is_stopped("r1")


class _NeverStops:
    def command_stop(self, robot_id: str) -> None: ...
    def tick(self, dt: float) -> None: ...
    def is_stopped(self, robot_id: str) -> bool:
        return False


def test_unconfirmed_stop_is_failed_never_stopped() -> None:
    # A binding that cannot confirm a halt MUST NOT be reported STOPPED.
    assert confirm_estop(_NeverStops(), "r1", tick_dt=0.01, max_ticks=5) == ESTOP_FAILED


def test_default_robot_pauses_to_confirmed_rest() -> None:
    # C11.1: pause is answered, and with require_stop only once rest is confirmed.
    robot = SimulatedRobot()
    robot.spawn("r1")
    robot.command_move("r1", 5.0, 0.0)
    assert robot.pause("r1", require_stop=True)
    assert robot.is_stopped("r1")
    assert robot.resume("r1")


def test_default_robot_arrives_and_a_stop_short_of_the_target_is_not_arrival() -> None:
    # C16.1: the adapter reports SUCCEEDED on arrived(). A pause holds the move and resume
    # drives on to the target; a stop abandons it, and neither counts as arrival.
    robot = SimulatedRobot()
    robot.spawn("r1")
    robot.command_move("r1", 1.0, 0.0)
    robot.tick(0.5)
    assert not robot.arrived("r1")
    assert robot.pause("r1", require_stop=True)
    robot.tick(10.0)
    assert not robot.arrived("r1")
    assert robot.resume("r1")
    robot.tick(10.0)
    assert robot.arrived("r1") and robot.is_stopped("r1")
    robot.command_move("r1", 5.0, 0.0)
    robot.command_stop("r1")
    robot.tick(10.0)
    assert not robot.arrived("r1")


def test_the_catalog_is_what_an_assignment_is_judged_against() -> None:
    # C19: a type the robot does not serve, or a Navigate with no target, is refused.
    from {{cookiecutter.python_package}}.adapter import _servable

    catalog = SimulatedRobot().catalog()
    assert _servable(catalog, "Navigate", b'{"x": 1, "y": 2}') == ((1.0, 2.0), "")
    assert _servable(catalog, "Clean", b'{"x": 1, "y": 2}')[0] is None
    assert _servable(catalog, "Navigate", b'{"x": 1}')[0] is None
    assert _servable(catalog, "", b"")[0] is None


def test_a_custom_verb_is_judged_by_its_own_catalog_entry() -> None:
    # Contract 1.4.0: one catalog entry per Custom verb, keyed by custom_type, and the verb
    # read from AssignAction.custom_type before the payload's `operation`. A verb the robot
    # does not serve, or one the payload contradicts, is refused -- never run as another.
    from {{cookiecutter.python_package}}.adapter import _servable

    catalog = [("Custom", [], [("bound_ref", "", "string")], "explore-floor")]
    payload = b'{"bound_ref": "room-1"}'
    assert _servable(catalog, "Custom", payload, "explore-floor")[1] == ""
    assert _servable(catalog, "Custom", b"{}", "explore-floor")[0] is None   # its own param
    assert _servable(catalog, "Custom", payload, "mow")[0] is None           # not served
    assert _servable(catalog, "Custom", b'{"bound_ref": "r", "operation": "mow"}',
                     "explore-floor")[0] is None                             # contradiction
    assert _servable(catalog, "Custom", b'{"bound_ref": "r", "operation": "explore-floor"}',
                     "")[1] == ""                                            # payload names it


def test_reconnect_backoff_is_bounded_and_grows() -> None:
    # C1.5: the first redial comes quickly, the ceiling doubles per failure, and it is capped.
    from {{cookiecutter.python_package}}.adapter import _BACKOFF_CAP, _backoff_delay

    for _ in range(200):
        assert 0.0 <= _backoff_delay(0) <= 0.5
        assert 0.0 <= _backoff_delay(3) <= 4.0
        assert 0.0 <= _backoff_delay(50) <= _BACKOFF_CAP
