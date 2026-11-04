#!/usr/bin/env bash
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

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

echo "==> building cmd/edge"
go build -o bin/edge ./cmd/edge

echo "==> starting the edge node (insecure dev mode; production requires mTLS)"
./bin/edge \
  --namespace warehouse-a \
  --config-file examples/edge-node/edge-config.json \
  --listen-address 127.0.0.1:9600 \
  --health-address 127.0.0.1:9601 \
  --safety-input-file examples/edge-node/safety-input \
  --insecure &
EDGE_PID=$!
trap 'kill "${EDGE_PID}" 2>/dev/null || true' EXIT

echo "==> waiting for readiness on http://127.0.0.1:9601/readyz"
ready=""
for _ in $(seq 1 50); do
  if command -v curl >/dev/null 2>&1 && curl -fsS http://127.0.0.1:9601/readyz >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.1
done
[ -n "${ready}" ] || { echo "edge node did not become ready"; exit 1; }

echo "==> running the edge-demo client (in-zone tee, then a boundary breach)"
go run ./examples/edge-node/demo --edge-address 127.0.0.1:9600 --robot-id amr-1

cat <<'NOTE'

==> demo complete.

To also see the LOCAL SAFETY INPUT path, while a stream is connected trip the input:
    echo 0 > examples/edge-node/safety-input     # active-low: 0 = tripped
The InputMonitor detects the rising edge and calls Node.TriggerLocalEstop, fanning a
zone-wide headless estop to every connected EdgeStream. Reset with:
    echo 1 > examples/edge-node/safety-input
NOTE
