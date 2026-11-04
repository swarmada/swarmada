# Copyright 2026 The Swarmada Authors.
# Licensed under the Apache License, Version 2.0.
"""Swarmada cross-surface conformance: profiles, catalogs, and the ConformanceReport.

The Fleet Adapter harness in ``adapters/conformance`` drives ``C0``-``C16`` and
emits a flat result set. This package adds the layer above it: it reads the
machine-readable registry in ``profiles.yaml``, groups results into named
profiles at Core and Extended levels, and emits the ``ConformanceReport``
specified in ``PROFILES.md``. It also drives the two catalogs the Draft 4
external-action surface adds -- ``COMPUTE-ENDPOINT.md`` and
``REACTIVE-BLOCK.md`` -- and the control-plane catalog in ``CONTROL-PLANE.md``.

Nothing here needs a cluster, a private repository, or a network. That is a
requirement, not a happy accident: a public suite a stranger cannot run is not a
public suite (``*0.3``).
"""
