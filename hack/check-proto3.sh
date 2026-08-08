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

# Enforce proto3 + Swarmada proto conventions. Safe to run with or without buf.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROTO_DIR="$ROOT/proto"
fail=0

# 1. every .proto must declare proto3 (and must not declare proto2)
while IFS= read -r f; do
  if ! grep -qE '^[[:space:]]*syntax[[:space:]]*=[[:space:]]*"proto3"[[:space:]]*;' "$f"; then
    echo "PROTO3: $f does not declare syntax=\"proto3\""; fail=1
  fi
  if grep -qE 'syntax[[:space:]]*=[[:space:]]*"proto2"' "$f"; then
    echo "PROTO2: $f declares proto2 — not allowed"; fail=1
  fi
done < <(find "$PROTO_DIR" -name '*.proto')

# 2. buf lint + breaking, if buf is installed
if command -v buf >/dev/null 2>&1; then
  ( cd "$PROTO_DIR" && buf lint ) || fail=1
  # Breaking-change check is ADVISORY pre-1.0: the fleet_adapter.v1 contract has no
  # external consumers yet and is still being shaped (e.g. the FleetTask→FleetAction
  # rename), so intentional breaking changes are expected and must not fail the build.
  # Re-enable as a hard gate (flip the `|| echo` back to `|| { …; fail=1; }`, and prefer
  # `--against` a release tag rather than `main`) once the contract is first published.
  if git -C "$ROOT" rev-parse --verify --quiet main >/dev/null 2>&1; then
    ( cd "$PROTO_DIR" && buf breaking --against "../.git#branch=main,subdir=proto" ) \
      || echo "buf: breaking changes vs main (advisory, pre-1.0 — not failing the build)"
  else
    echo "buf: no 'main' branch — skipping breaking-change check (wire this in CI)"
  fi
else
  echo "buf not installed — ran proto3 checks only."
  echo "      install: brew install bufbuild/buf/buf   (or go install github.com/bufbuild/buf/cmd/buf@latest)"
fi

if [ "$fail" -eq 0 ]; then echo "proto checks passed."; fi
exit $fail
