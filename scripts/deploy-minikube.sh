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
# Builds the controller-manager image and deploys Swarmada to a local
# minikube cluster — the Developer/Prototype topology
# (rfcs/rfc-0001-core-spec/architecture.md's "Deployment Topology" section).
# See docs/deploy-minikube.md for the full explanation of every step and the
# known limitation (admission webhooks disabled — no cert-manager wiring yet).
#
# Prerequisites: run scripts/setup-macos.sh first (or install manually per
# docs/deploy-minikube.md), and have Docker Desktop running.
#
# Usage:
#   scripts/deploy-minikube.sh [profile] [image]
#
#   scripts/deploy-minikube.sh                        # profile=swarmada-dev, image=swarmada-controller:dev
#   scripts/deploy-minikube.sh my-profile my-image:tag

set -euo pipefail

PROFILE="${1:-swarmada-dev}"
IMG="${2:-swarmada-controller:dev}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

step() { echo; echo "==> $1"; }
have() { command -v "$1" >/dev/null 2>&1; }

for tool in minikube kubectl docker make protoc go; do
  if ! have "$tool"; then
    echo "Missing required tool: $tool" >&2
    echo "Run scripts/setup-macos.sh first, or see docs/deploy-minikube.md." >&2
    exit 1
  fi
done

if ! docker info >/dev/null 2>&1; then
  echo "Docker does not appear to be running." >&2
  echo "Start Docker Desktop (open -a Docker) and wait for it to be ready, then re-run." >&2
  exit 1
fi

step "1/6 — Start minikube (profile: $PROFILE)"
if minikube status --profile "$PROFILE" >/dev/null 2>&1; then
  echo "profile '$PROFILE' already running"
else
  minikube start --profile "$PROFILE"
fi
kubectl config use-context "$PROFILE"

step "2/6 — Build the controller-manager image ($IMG)"
make docker-build IMG="$IMG"

step "3/6 — Load the image into minikube"
minikube image load "$IMG" --profile "$PROFILE"

step "4/6 — Install CRDs"
make install

step "5/6 — Deploy the controller-manager"
make deploy IMG="$IMG"

step "6/6 — Wait for the manager pod to be ready"
kubectl -n system wait --for=condition=Available deployment/swarmada-controller-manager --timeout=120s

echo
echo "Deployed. Verify with:"
echo "  kubectl get pods -n system"
echo "  kubectl logs -n system deployment/swarmada-controller-manager"
echo
echo "Try it:"
echo "  kubectl create namespace warehouse-a"
echo "  kubectl apply -f config/samples/"
echo "  kubectl get robots,fleetactions -n warehouse-a"
echo
echo "Tear down:"
echo "  make undeploy && make uninstall && minikube delete --profile $PROFILE"
echo
echo "Known limitation: admission webhooks are disabled in this deploy"
echo "(ENABLE_WEBHOOKS=false, no cert-manager wiring yet) — see"
echo "docs/deploy-minikube.md for why."
