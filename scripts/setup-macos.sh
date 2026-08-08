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
# Installs the prerequisites for building and deploying Swarmada on macOS:
# Homebrew (if missing), Go, kubectl, minikube, Docker Desktop, protoc, and
# the Go proto codegen plugins. Idempotent — safe to re-run; each step is
# skipped if already satisfied.
#
# macOS only for now (see docs/deploy-minikube.md). Linux/Windows equivalents
# are tracked as follow-up work closer to release.
#
# Usage:
#   scripts/setup-macos.sh
#
# What this does NOT do:
#   - Does not start Docker Desktop or minikube (see scripts/deploy-minikube.sh
#     and docs/deploy-minikube.md for the actual deploy flow).
#   - Does not install grpcio-tools (Python proto stubs) — that needs a
#     `--break-system-packages` or virtualenv decision this script won't make
#     for you; see the printed note at the end.

set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "This script is macOS-only for now. See docs/deploy-minikube.md." >&2
  exit 1
fi

step() { echo; echo "==> $1"; }
have() { command -v "$1" >/dev/null 2>&1; }

# ── Homebrew ──────────────────────────────────────────────────────────────
step "Homebrew"
if have brew; then
  echo "already installed: $(brew --version | head -1)"
else
  echo "installing Homebrew..."
  /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
  # Apple Silicon Homebrew lives under /opt/homebrew, not on PATH by default
  # in a fresh shell — add it for the rest of this script's steps.
  if [[ -x /opt/homebrew/bin/brew ]]; then
    eval "$(/opt/homebrew/bin/brew shellenv)"
  fi
fi

# ── Xcode Command Line Tools (make, git, etc.) ───────────────────────────
step "Xcode Command Line Tools"
if xcode-select -p >/dev/null 2>&1; then
  echo "already installed"
else
  echo "installing (a GUI prompt will appear — accept it, then re-run this script)..."
  xcode-select --install || true
  exit 0
fi

# ── Go ────────────────────────────────────────────────────────────────────
step "Go"
if have go; then
  echo "already installed: $(go version)"
else
  brew install go
fi

# ── kubectl ───────────────────────────────────────────────────────────────
step "kubectl"
if have kubectl; then
  echo "already installed: $(kubectl version --client --output=yaml 2>/dev/null | grep gitVersion || true)"
else
  brew install kubectl
fi

# ── minikube ──────────────────────────────────────────────────────────────
step "minikube"
if have minikube; then
  echo "already installed: $(minikube version --short 2>/dev/null || minikube version)"
else
  brew install minikube
fi

# ── Docker Desktop ────────────────────────────────────────────────────────
step "Docker"
if have docker; then
  echo "already installed: $(docker --version)"
else
  echo "installing Docker Desktop (cask)..."
  brew install --cask docker
  echo "Docker Desktop is installed but not started. Open it once from"
  echo "Applications (or: open -a Docker) and wait for it to report 'running'"
  echo "before continuing — minikube's docker driver needs the daemon up."
fi

# ── protoc ────────────────────────────────────────────────────────────────
step "protoc (Protocol Buffers compiler)"
if have protoc; then
  echo "already installed: $(protoc --version)"
else
  brew install protobuf
fi

# ── Go proto codegen plugins ──────────────────────────────────────────────
# Installed directly (not via `make proto-tools`) because that Makefile target
# also installs grpcio-tools (Python), which fails under Homebrew's
# PEP-668-managed python3 unless the caller opts into --break-system-packages
# or a venv — a decision this script won't make silently. Versions must stay
# in lockstep with PROTOC_GEN_GO_VERSION/PROTOC_GEN_GO_GRPC_VERSION in the
# Makefile (see its comment: pinned to match the grpc runtime in go.mod).
step "protoc-gen-go / protoc-gen-go-grpc"
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

step "Done"
cat <<'EOF'
All macOS prerequisites for the Go/Docker/minikube path are installed.

Next steps:
  1. Start Docker Desktop if you haven't (open -a Docker) and wait for it to
     report ready.
  2. Run scripts/deploy-minikube.sh (or follow docs/deploy-minikube.md by
     hand) to build the image and deploy Swarmada to a minikube cluster.

Not installed by this script (only needed if you're building Python
reference adapters, not for the Go controller-manager / docker-build path):
  pip install grpcio-tools --break-system-packages
  (Homebrew's Python is "externally managed" per PEP 668; --break-system-packages
  or a venv are both fine here — see make proto-py.)
EOF
