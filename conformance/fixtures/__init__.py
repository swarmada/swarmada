# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""In-tree conforming executors, one per binding.

They exist so the public suite stands alone: a stranger with a clone and a
Python interpreter can run every Compute Endpoint check without deploying
anything, without a cluster, and without a private repository. The HTTP and MCP
fixtures are standard library only; the gRPC one uses the stubs ``make proto``
already generates.

They recognise nothing. Plates are synthesised deterministically from the
invocation id, so a test asserts an exact string and no real plate number is
ever produced or stored -- a plate with a timestamp and a location is personal
data in several jurisdictions, and a CI fixture is the least controlled place
for such records to accumulate. The fixtures are synthetic by design, not for
convenience.

Each fixture also implements a ``misbehave`` mode. A check that has never been
seen to fail is not known to be a check, and the negative runs are how this
suite demonstrates its own falsifiability.
"""
