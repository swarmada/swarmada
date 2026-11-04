# Copyright 2026 The Swarmada Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# ─────────────────────────────────────────────────────────────────────────────
# The floor-mapping demo's zone-proposal worker (zone_proposal_worker.py) and the
# map-to-fleetzone tools it runs. A DEMO image.
#
#   docker build -f examples/floor-mapping/worker/Dockerfile \
#     -t swarmada-zone-proposal-worker:dev .
#
# Built from the repository root. Dockerfile.dockerignore beside this file narrows
# the context to the worker and the tools.
# ─────────────────────────────────────────────────────────────────────────────

FROM python:3.12-slim

# PyYAML is the only dependency of the worker and of the tools; pinned to the version
# uv.lock resolves for the repository.
RUN pip install --no-cache-dir pyyaml==6.0.3

WORKDIR /app
COPY tools/map-to-fleetzone/map_to_fleetzone.py \
     tools/map-to-fleetzone/floor_to_member_zones.py \
     tools/map-to-fleetzone/traversable.py \
     tools/map-to-fleetzone/render_zones.py \
     tools/map-to-fleetzone/
COPY examples/floor-mapping/worker/zone_proposal_worker.py ./
RUN chmod -R a+rX /app

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

USER 65532:65532

ENTRYPOINT ["python3", "/app/zone_proposal_worker.py"]
