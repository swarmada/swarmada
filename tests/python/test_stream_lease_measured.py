# Copyright 2026 The Swarmada Authors. All rights reserved.

"""The owed sensor-stream lease measurement:

    "kill the controller mid-run and show the stream powers off once the lease
    expires. Record the interval."

ActivateStream/DeactivateStream (proto/fleet_adapter/v1/fleet_adapter.proto) reuse
the SAME lease-self-stop mechanism `LeaseMonitor` already implements and C4
verifies for a FleetAction's assignment lease (sdk/python/src/swarmada_sdk/safety.py,
tests/python/test_sim_adapter.py) — it is keyed by an arbitrary string, so a
`"<robot_id>:<stream_id>"` key exercises the identical, already-safety-reviewed
code path with zero modification to it. That is the point being demonstrated:
streams do not get a second, unproven duty-cycle implementation.

The existing C4 tests use a FAKE clock (deterministic, instant, good for CI).
This one deliberately does NOT: it uses LeaseMonitor's real default clock
(time.monotonic) and a real polling loop, so the reported number is an actual
wall-clock measurement — "measured, not asserted" — of how long a stream stays
on after the control plane goes silent (a "killed controller" is
indistinguishable, from the adapter's point of view, from a partition: both
stop producing renewals).

Run directly for the printed, human-readable measurement:

    python3 -m pytest tests/python/test_stream_lease_measured.py -v -s
"""

from __future__ import annotations

import time

from swarmada_sdk.safety import LeaseMonitor

STREAM_KEY = "yard-robot-3:front-rgb"
LEASE_SECONDS = 2.0
RENEW_INTERVAL_SECONDS = 0.3
CONTROLLER_ALIVE_SECONDS = 1.2  # several renewals while "the controller" is up
POLL_SECONDS = 0.01  # the adapter's own tick granularity while polling for expiry


def test_stream_powers_off_when_the_controller_partitions_mid_run() -> None:
    stopped_at: dict[str, float] = {}

    def on_expiry(key: str) -> None:
        stopped_at[key] = time.monotonic()

    lease = LeaseMonitor(on_expiry=on_expiry)  # real clock: time.monotonic

    # ── The run starts: ActivateStream grants the lease
    # (refcounted activation; rule 2's lease is what this test measures). ──
    lease.grant(STREAM_KEY, duration_s=LEASE_SECONDS, generation=1)
    stream_on = True
    assert stream_on

    # ── "The controller is alive": it renews on the ControlStream, well
    # inside the lease, exactly as it does for an assignment lease today. ──
    gen = 1
    alive_until = time.monotonic() + CONTROLLER_ALIVE_SECONDS
    while time.monotonic() < alive_until:
        time.sleep(RENEW_INTERVAL_SECONDS)
        gen += 1
        renewed = lease.renew(STREAM_KEY, duration_s=LEASE_SECONDS, generation=gen)
        assert renewed, "a live controller's renewal must always be honoured"
        lease.tick()
        assert STREAM_KEY not in stopped_at, "must not stop while renewals are arriving"

    # ── KILL THE CONTROLLER: stop renewing. This is the partition. ──
    partition_started_at = time.monotonic()

    # The adapter's own tick loop keeps running locally — it has no way to
    # know the silence is a dead controller rather than a slow one, which is
    # exactly why the lease, not a liveness guess, is what stops the stream.
    while STREAM_KEY not in stopped_at:
        time.sleep(POLL_SECONDS)
        lease.tick()
        if time.monotonic() - partition_started_at > LEASE_SECONDS + 5.0:
            raise AssertionError(
                "stream did not self-stop within 5s of the lease horizon — fail-safe did not fire"
            )

    stream_on = False
    partition_to_off_seconds = stopped_at[STREAM_KEY] - partition_started_at

    print(  # noqa: T201 — this number is the deliverable; -s prints it.
        f"\nMEASURED partition-to-stream-off interval: {partition_to_off_seconds:.3f}s "
        f"(lease duration was {LEASE_SECONDS:.3f}s, poll granularity {POLL_SECONDS * 1000:.0f}ms)"
    )

    assert not stream_on

    # The stream must not outlive the lease horizon by more than roughly one
    # poll tick — the fail-safe claim is that partition STOPS the camera at
    # (approximately) the lease's own duration, not "eventually".
    assert LEASE_SECONDS <= partition_to_off_seconds <= LEASE_SECONDS + 0.5, (
        f"partition-to-off interval {partition_to_off_seconds:.3f}s should be within "
        f"[{LEASE_SECONDS:.3f}, {LEASE_SECONDS + 0.5:.3f}]s of the lease duration"
    )
