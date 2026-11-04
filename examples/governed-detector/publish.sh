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
#
# publish.sh — push each built model version to the registry as an OCI artifact, sign it with
# cosign, and print one line per version: version, modelUri, modelChecksum, modelSignatureRef.
#
#   publish.sh <build-dir> <registry host:port> <cosign private key> [versions...]
#
# The key stays where you keep it; this script reads it and writes it nowhere. cosign asks for
# its password unless COSIGN_PASSWORD is set in the environment.
#
# Signing is cosign's key-pair mode with no transparency log (the demo is offline), in the
# simple-signing format the adapter verifies. The checksum is the manifest digest, which pins
# every layer; the signature claims that same digest.
set -euo pipefail

BUILD="${1:?usage: publish.sh <build-dir> <registry> <cosign.key> [versions...]}"
REG="${2:?registry host:port}"
KEY="${3:?cosign private key}"
shift 3
REPO="swarmada/floor-hazard"
# A fixed creation time, so the same build pushed to any registry has the same manifest
# digest; oras would otherwise stamp the push time and every push would be a new artifact.
CREATED="${CREATED:-2026-09-28T00:00:00Z}"
[ $# -gt 0 ] || set -- $(cd "$BUILD" && ls -d */ | tr -d /)

plain=()
case "$REG" in localhost:*|127.0.0.1:*) plain=(--plain-http) ;; esac

for v in "$@"; do
  d="$BUILD/$v"
  [ -f "$d/model.onnx" ] || { echo "publish.sh: no $d/model.onnx" >&2; exit 1; }
  (cd "$d" && oras push "${plain[@]}" --artifact-type application/vnd.swarmada.model.v1 \
    --annotation "org.opencontainers.image.version=$v" \
    --annotation "org.opencontainers.image.created=$CREATED" \
    --annotation "org.opencontainers.image.licenses=Apache-2.0" \
    "$REG/$REPO:$v" \
    model.onnx:application/vnd.swarmada.model.onnx \
    labels.json:application/vnd.swarmada.model.labels+json \
    selfcheck.npz:application/vnd.swarmada.model.selfcheck+npz >/dev/null)
  digest="$(oras resolve "${plain[@]}" "$REG/$REPO:$v")"
  cosign sign --yes --key "$KEY" --use-signing-config=false --new-bundle-format=false \
    --tlog-upload=false --allow-http-registry "$REG/$REPO@$digest" >/dev/null 2>&1 \
    || { echo "publish.sh: cosign sign failed for $v" >&2; exit 1; }
  printf '%s\toci://%s/%s:%s\t%s\toci://%s/%s:sha256-%s.sig\n' \
    "$v" "$REG" "$REPO" "$v" "$digest" "$REG" "$REPO" "${digest#sha256:}"
done
