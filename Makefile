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

# ══════════════════════════════════════════════════════════════════════════════
#  Swarmada – unified Makefile
#  Works from the repo root for both Go (GoLand) and Python (PyCharm) components
#
#  Every target here runs against what this repository actually contains. The
#  edge node, swarmctl, the Python simulation/ml packages, the conformance
#  harness and the Helm chart ship in a later release; their targets are not
#  declared here rather than declared and broken.
# ══════════════════════════════════════════════════════════════════════════════

# ── Preflight: find Go ────────────────────────────────────────────────────────
#
# Search order:
#   1. $PATH  (CI, Homebrew, asdf, mise)
#   2. ~/go*/bin/go      — GoLand custom SDK path  (e.g. ~/go1.26.4/bin/go)
#   3. ~/sdk/go*/bin/go  — GoLand "Download SDK" default
#   4. /opt/homebrew/bin/go  — Homebrew Apple Silicon
#   5. /usr/local/go/bin/go  — official pkg installer
#
# To override a single run: GO=/path/to/go make lint
#
GO ?= $(shell \
  command -v go 2>/dev/null || \
  ls -d $(HOME)/go*/bin/go 2>/dev/null | grep -v pkg | sort -V | tail -1 | xargs ls 2>/dev/null || \
  ls -d $(HOME)/sdk/go*/bin/go 2>/dev/null | sort -V | tail -1 | xargs ls 2>/dev/null || \
  ls /opt/homebrew/bin/go 2>/dev/null || \
  ls /usr/local/go/bin/go 2>/dev/null || \
  echo "")

ifeq ($(GO),)
$(error Cannot find the 'go' binary.\
  GoLand users: Settings > Go > GOROOT  shows the exact path. Then:\
    export PATH="<goroot>/bin:$$PATH" >> ~/.zshrc\
  Or install: brew install go\
  Or override: GO=/your/go/bin/go make setup)
endif

# Make the located go binary visible to all child processes (golangci-lint,
# controller-gen, etc.) even when the shell PATH is incomplete.
export PATH := $(dir $(GO)):$(PATH)

# ── GOPATH safety ─────────────────────────────────────────────────────────────
# GoLand sometimes sets GOPATH to the project directory (legacy GOPATH-mode
# behavior). When the project lives inside GOPATH, `go mod tidy` prints
# "ignoring go.mod in $GOPATH" and refuses to run.
#
# Fix: always use the standard module-cache location.
# This is safe — Go modules never look in GOPATH for source code.
# Module cache → $(GOPATH)/pkg/mod   Installed bins → $(GOPATH)/bin
#
# GoLand fix (permanent): Settings → Go → GOPATH → remove the project path
#                         Settings → Go → Go Modules → enable integration
export GOPATH := $(HOME)/go
export GOMODCACHE := $(GOPATH)/pkg/mod
export GOWORK := off

# ── GOPROXY safety ────────────────────────────────────────────────────────────
# GoLand can write a broken GOPROXY into the shell environment (empty commas,
# whitespace-only). Go rejects it with "contains no entries". We detect that
# here and fall back to the public proxy so `make setup` works out of the box.
#
# To use a corporate proxy:
#   go env -w GOPROXY=https://your-proxy.company.com,https://proxy.golang.org,direct
#
_GOPROXY_STRIPPED := $(shell printf '%s' '$(GOPROXY)' | tr -d ' ,')
ifeq ($(_GOPROXY_STRIPPED),)
  export GOPROXY := https://proxy.golang.org,direct
endif
export GONOSUMDB ?= *

GOBIN          ?= $(GOPATH)/bin
# Put go-installed plugins (protoc-gen-go, protoc-gen-go-grpc) on PATH so protoc
# can find them — install them once with `make proto-tools`.
export PATH := $(GOBIN):$(PATH)

CONTROLLER_GEN ?= $(GOBIN)/controller-gen
PROTOC         ?= protoc
PYTHON         ?= python3
IMG            ?= ghcr.io/swarmada/swarmada:latest

.PHONY: all help check-env

all: generate manifests build lint test

help:           ## Show core targets (run: make help-all for everything)
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n",$$1,$$2}'

help-all:       ## Show ALL targets (advanced / CI-only included)
	@grep -E '^[a-zA-Z_-]+:.*?##!? .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?##!? "}{printf "  \033[36m%-22s\033[0m %s\n",$$1,$$2}'

check-env:      ## Show which go / python Make resolved — run this first when debugging
	@echo "──────────────────────────────────────────"
	@echo "  go binary : $(GO)"
	@echo "  go version: $(shell $(GO) version)"
	@echo "  GOPATH    : $(shell $(GO) env GOPATH)"
	@echo "  GOROOT    : $(shell $(GO) env GOROOT)"
	@echo "  python    : $(shell command -v $(PYTHON))"
	@echo "  py version: $(shell $(PYTHON) --version)"
	@echo "──────────────────────────────────────────"
	@echo "  Tip: to set permanently, add to ~/.zshrc:"
	@echo "  export PATH=\"$(dir $(GO)):$$PATH\""
	@echo "──────────────────────────────────────────"

# ── Go ────────────────────────────────────────────────────────────────────────

.PHONY: build swarmctl run fmt vet test-go lint-go generate manifests mod-tidy setup controller-gen setup-envtest install-py-ci

# go.sum is the gate: if it is missing or stale, lint and build will both fail
# with "missing go.sum entry" errors. Run `make mod-tidy` once after cloning,
# and again whenever go.mod changes.
mod-tidy:       ##! Resolve dependency graph and write go.sum (run once after clone)
	$(GO) mod tidy
	$(GO) mod verify

setup: mod-tidy install-py proto-tools  ## First-time setup: tidy Go deps + install Python deps
	@echo "Setup complete. Run: make lint test"

build: swarmctl  ## Compile the controller-manager, swarmctl, and (if present) the edge-node
	$(GO) build -o bin/manager ./cmd/manager
	@if [ -d ./cmd/edge ]; then $(GO) build -o bin/edge ./cmd/edge; fi

swarmctl:       ##! Compile the swarmctl operator CLI
	$(GO) build -o bin/swarmctl ./cmd/swarmctl

# ── Edge node cross-compile ─────────────────────────────────────────────────
# `build` above only produces a host-OS/arch bin/edge, which is fine for local
# dev but not for shipping to real facility-LAN hardware — commonly arm64
# (Raspberry Pi-class or industrial ARM boxes) rather than the dev machine's
# amd64. These targets cross-compile static binaries for both, matching the
# CGO_ENABLED=0 static-binary approach the Dockerfile already uses for manager.
.PHONY: build-edge-linux-amd64 build-edge-linux-arm64 build-edge-cross

build-edge-linux-amd64:  ## Cross-compile bin/edge-linux-amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w" -o bin/edge-linux-amd64 ./cmd/edge

build-edge-linux-arm64:  ## Cross-compile bin/edge-linux-arm64 (Raspberry Pi-class / industrial ARM edge hosts)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags="-s -w" -o bin/edge-linux-arm64 ./cmd/edge

build-edge-cross: build-edge-linux-amd64 build-edge-linux-arm64  ##! Cross-compile bin/edge for both supported edge-host architectures

# The manager serves its admission webhooks from a cert dir that only exists IN-CLUSTER (mounted
# from the webhook Secret). Run locally and there are no serving certs, so the manager starts every
# controller and then dies on "serving-certs/tls.crt: no such file or directory". cmd/manager/main.go
# carries the documented escape hatch for exactly this case (ENABLE_WEBHOOKS=false); this target now
# sets it, and says so, because silently running WITHOUT admission is the thing that misleads.
# Override with `make run ENABLE_WEBHOOKS=true` once serving certs are in place.
ENABLE_WEBHOOKS ?= false

run:            ## Run the controller-manager locally against the cluster in your kubeconfig
	@kubectl config current-context >/dev/null 2>&1 || { \
	  echo "make run: no current kubeconfig context is set, so the manager has no cluster to talk to."; \
	  echo "  kind clusters found: $$(kind get clusters 2>/dev/null | tr '\n' ' ' | sed 's/ $$//')"; \
	  echo "  select one:      kubectl config use-context kind-<name>"; \
	  echo "  or bring one up: make quickstart"; \
	  exit 1; }
	@kubectl cluster-info >/dev/null 2>&1 || { \
	  echo "make run: kubeconfig context '$$(kubectl config current-context)' is set but unreachable"; \
	  echo "  (a deleted kind cluster leaves its context behind — 'kind get clusters' shows what is live)."; \
	  exit 1; }
	@if [ "$(ENABLE_WEBHOOKS)" = "false" ]; then \
	  echo "make run: admission webhooks are DISABLED (no local serving certs)."; \
	  echo "  RobotClass defaulting and the admission gates (incl. the FleetAdapter dispatch gate)"; \
	  echo "  are NOT exercised — a request this manager admits may still be rejected in-cluster."; \
	  echo "  With certs in place: make run ENABLE_WEBHOOKS=true"; \
	fi
	ENABLE_WEBHOOKS=$(ENABLE_WEBHOOKS) $(GO) run ./cmd/manager

fmt:            ##! Run gofmt + goimports
	$(GO) fmt ./...

vet:            ##! Run go vet
	$(GO) vet ./...

# Kubernetes version for the envtest control-plane binaries (docs/testing.md layer 2).
ENVTEST_K8S_VERSION ?= 1.29.0

setup-envtest:  ##! Install the envtest control-plane binaries (kube-apiserver, etcd) for controller tests
	$(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	@setup-envtest use $(ENVTEST_K8S_VERSION)
	@echo "envtest assets installed. 'make test-go' will now run the controller envtest suite."

test-go:        ##! Run Go unit tests with race detector (+ envtest controller suite when assets are present)
	@ASSETS=""; \
	if command -v setup-envtest >/dev/null 2>&1; then \
	  ASSETS=$$(setup-envtest use $(ENVTEST_K8S_VERSION) -p path 2>/dev/null || true); \
	fi; \
	if [ -n "$$ASSETS" ]; then echo "envtest assets: $$ASSETS"; else echo "envtest assets not found; controller envtest tests will skip (run 'make setup-envtest')"; fi; \
	KUBEBUILDER_ASSETS="$$ASSETS" $(GO) test -race -coverprofile=coverage.txt ./...

GOLANGCI_MIN_VERSION := 2.0

lint-go:        ##! Run golangci-lint (requires >= v$(GOLANGCI_MIN_VERSION); see .golangci.yml)
	@command -v golangci-lint >/dev/null 2>&1 || \
	  { echo "golangci-lint not found. Install: brew install golangci-lint"; exit 1; }
	@golangci-lint config verify >/dev/null 2>&1 || { \
	  echo "'.golangci.yml' is not valid for this golangci-lint version — the config would be"; \
	  echo "silently degraded (v1 keys under a v2 schema leave linters/exclusions unapplied):"; \
	  golangci-lint config verify 2>&1 | sed 's/^/  /'; \
	  exit 1; }
	@CURRENT=$$(golangci-lint --version 2>&1 | grep -oE '[0-9]+\.[0-9]+' | head -1); \
	  REQUIRED=$(GOLANGCI_MIN_VERSION); \
	  if [ "$$(printf '%s\n' "$$REQUIRED" "$$CURRENT" | sort -V | head -1)" != "$$REQUIRED" ]; then \
	    echo "golangci-lint $$CURRENT is too old (need >= $$REQUIRED). Run: brew upgrade golangci-lint"; \
	    exit 1; \
	  fi
	golangci-lint run ./...

# Pinned, NOT `latest`. An unpinned `latest` combined with the install-only-if-
# missing rule below meant each machine kept whatever controller-gen it first
# cached, so `make manifests` output varied by host — the source of the
# intermittent RBAC-marker drop (older controller-gen mis-unions verbs when one
# resource is declared with different verb sets across markers; see
# config/rbac/role.yaml history). Bump deliberately and regenerate.
CONTROLLER_TOOLS_VERSION ?= v0.16.5

controller-gen: ##! Install controller-gen into $(GOBIN) if missing or version-mismatched
	@if ! test -x $(CONTROLLER_GEN) || ! $(CONTROLLER_GEN) --version 2>/dev/null | grep -q "$(CONTROLLER_TOOLS_VERSION)"; then \
	  echo "installing controller-gen $(CONTROLLER_TOOLS_VERSION)"; \
	  $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION); \
	fi

generate: controller-gen ## Regenerate deepcopy functions
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

manifests: controller-gen ## Regenerate CRD + RBAC YAML from Go type annotations
	$(CONTROLLER_GEN) \
	  rbac:roleName=swarmada-manager-role \
	  crd:allowDangerousTypes=true \
	  webhook \
	  paths="./..." \
	  output:crd:artifacts:config=config/crd/bases \
	  output:rbac:artifacts:config=config/rbac

# ── Protobuf ──────────────────────────────────────────────────────────────────

.PHONY: proto proto-go proto-py proto-lint proto-tools

# Proto codegen plugin versions — PINNED to match the grpc runtime in go.mod.
# protoc-gen-go-grpc v1.5.x emits the generics streaming API (SupportPackageIsVersion9),
# which REQUIRES google.golang.org/grpc >= v1.64 at runtime. Keep these and the grpc
# version in go.mod in lockstep; `@latest` here is what let codegen race ahead of a
# pinned runtime and break the bidi-stream build (grpc.BidiStreamingClient undefined).
PROTOC_GEN_GO_VERSION      ?= v1.36.11
PROTOC_GEN_GO_GRPC_VERSION ?= v1.5.1

proto-tools:    ##! Install proto codegen plugins: protoc-gen-go, protoc-gen-go-grpc, grpcio-tools (one-time)
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	@$(PYTHON) -c 'import grpc_tools' >/dev/null 2>&1 && \
	  echo "grpcio-tools already available to $$($(PYTHON) -V 2>&1) — skipping pip" || { \
	    echo "installing grpcio-tools for $$($(PYTHON) -V 2>&1) ($$(command -v $(PYTHON)))"; \
	    $(PYTHON) -m pip install grpcio-tools || { \
	      echo ""; \
	      echo "pip refused: this interpreter is externally managed (PEP 668)."; \
	      echo "Install into an environment you own rather than forcing it into the system one:"; \
	      echo "  * activate your project venv (python -m venv / uv), then re-run: make proto-tools"; \
	      echo "  * or point the target at it:  make proto-tools PYTHON=/path/to/env/bin/python"; \
	      echo "  * last resort (per-user, not system-wide):  $(PYTHON) -m pip install --user grpcio-tools"; \
	      exit 1; }; }

proto: proto-tools proto-go proto-py  ## Generate gRPC stubs (installs tools, Go + Python) — the one codegen target

proto-go:       ##! Generate only the Go gRPC stubs (what the controller-manager/docker-build needs — no Python toolchain required)
	$(PROTOC) \
	  --go_out=. --go_opt=paths=source_relative \
	  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
	  proto/fleet_adapter/v1/fleet_adapter.proto

proto-py:       ##! Generate only the Python gRPC stubs (the reference adapters import these)
	$(PYTHON) -m grpc_tools.protoc \
	  -Iproto \
	  --python_out=proto \
	  --grpc_python_out=proto \
	  fleet_adapter/v1/fleet_adapter.proto
	@# Import root is `proto/` so the stubs import as `fleet_adapter.v1.*`;
	@# consumers put `proto/` on PYTHONPATH. Regenerate after changing this flag.

proto-lint:     ##! Lint protos: proto3 conventions + buf lint/breaking (hack/check-proto3.sh)
	bash hack/check-proto3.sh

# ── Python ────────────────────────────────────────────────────────────────────

.PHONY: install-py fmt-py lint-py test-py

# CI installs this, not install-py. The `ml` extra pulls torch/pandas/matplotlib —
# multiple gigabytes — and NOTHING the spec/conformance gate touches imports them:
# the whole tests/python suite passes with torch, pandas, matplotlib and transforms3d
# blocked at the import hook. grpcio-tools (needed by proto-py) is a BASE dependency,
# so base + dev is the complete set. Do not "simplify" this back to install-py: the
# cost is a multi-GB download on every push, and it is invisible locally because a
# developer machine already has the extras.
install-py-ci:  ##! Install only the Python deps the CI spec/conformance gate needs (no ml extras)
	$(PYTHON) -m pip install -e ".[dev]" || \
	  $(PYTHON) -m pip install --break-system-packages -e ".[dev]"

install-py:     ##! Install Python dev dependencies into the active venv
	@# Fall back to --break-system-packages on an externally-managed interpreter
	@# (PEP 668 / Homebrew), matching the proto-tools target's convention. Prefer a
	@# per-project venv; this keeps `make install-py` working on a bare system python.
	$(PYTHON) -m pip install -e ".[dev,simulation]" || \
	  $(PYTHON) -m pip install --break-system-packages -e ".[dev,simulation]"

fmt-py:         ##! Format Python code with black + ruff
	black simulation/ scripts/ tests/python/
	ruff check --fix simulation/ scripts/

lint-py:        ##! Lint Python code with ruff + mypy, and enforce the message-voice rules
	ruff check simulation/ scripts/
	mypy simulation/
	# The Voice_and_Tone gate on operator-facing message strings. It scans the whole tree
	# (skipping _test.go, zz_generated, vendor, proto, bin, venv, node_modules). It had
	# existed with no make target and no CI job, so it had never once run against the
	# code it governs.
	$(PYTHON) scripts/check-error-strings.py .

lint-prose:     ##! Check documentation voice with vale (install: brew install vale)
	@command -v vale >/dev/null 2>&1 || { \
	  echo "vale not installed. brew install vale — rules are in .vale/styles/Swarmada/"; \
	  exit 1; }
	vale .

test-py:        ##! Run Python tests with pytest
	pytest tests/python/ -v

# ── Conformance ───────────────────────────────────────────────────────────────

.PHONY: conformance conformance-sim conformance-sim-fault

# Run the Fleet Adapter conformance harness against an adapter. Override ADAPTER
# with the command that launches the adapter-under-test ('{port}' is substituted).
# Needs the generated Python stubs (`make proto`); STUB_PATH is where they live.
STUB_PATH ?= proto
ADAPTER   ?= $(PYTHON) adapters/example-noop/noop_adapter.py --endpoint localhost:{port}

conformance: proto-py   ## Run the adapter conformance harness (see adapters/conformance/README.md)
	$(PYTHON) -m adapters.conformance --stub-path $(STUB_PATH) --adapter-cmd '$(ADAPTER)' $(CONFORMANCE_ARGS)

# --scenario healthy-fleet makes the sim actually STREAM telemetry. Without it the adapter
# is silent on that path and four documented MUSTs (C6.1/C6.2/C6.3, C2.3) are unverifiable
# for want of a single TelemetryPayload — including explicit presence, which is the whole
# reason the safety scalars are declared `optional`.
conformance-sim: proto-py  ##! Run the conformance harness against the in-tree simulation adapter
	$(PYTHON) -m adapters.conformance --stub-path $(STUB_PATH) \
	  --adapter-cmd '$(PYTHON) adapters/simulation/sim_adapter.py --endpoint localhost:{port} --scenario healthy-fleet' $(CONFORMANCE_ARGS)

# Same conformance gate, but driving the hardware-fault scenario with fast timers so
# the run also exercises the scenario loader + TelemetryPayload.hardware degrade/recover
# path (Demo B as a live conformance illustration). Requires PyYAML for the scenario loader.
FAULT_AT      ?= 2
RECOVER_AT    ?= 4
conformance-sim-fault: proto-py  ##! Conformance harness against the hardware-fault scenario (fast timers)
	$(PYTHON) -m adapters.conformance --stub-path $(STUB_PATH) \
	  --adapter-cmd '$(PYTHON) adapters/simulation/sim_adapter.py --endpoint localhost:{port} \
	    --scenario hardware-fault --fault-at $(FAULT_AT) --recover-at $(RECOVER_AT)' $(CONFORMANCE_ARGS)

# ── Reference adapters (own repos, ADR-0005) ─────────────────────────────────
# The ROS 2 / VDA5050 / MAVLink reference adapters live in their own repositories.
# Clone the one you want from its own repo into adapters/external/ (gitignored).

# ── Developer deploy (minikube) ──────────────────────────────────────────────

.PHONY: setup-macos deploy-minikube

MINIKUBE_PROFILE ?= swarmada-dev

setup-macos:    ## Install macOS prerequisites (Homebrew, Go, kubectl, minikube, Docker, protoc) — see docs/deploy-minikube.md
	bash scripts/setup-macos.sh

deploy-minikube: ##! Build the image and deploy Swarmada to a local minikube cluster — see docs/deploy-minikube.md
	bash scripts/deploy-minikube.sh $(MINIKUBE_PROFILE) $(IMG)

# ── Combined ──────────────────────────────────────────────────────────────────

.PHONY: test lint

test: test-go test-py   ## Run all tests (Go + Python)
lint: lint-go lint-py proto-lint   ## Run all linters (Go + Python)

# ── Docker ────────────────────────────────────────────────────────────────────

IMG_EDGE ?= ghcr.io/swarmada/edge:latest
# Edge hosts are commonly arm64 (Raspberry Pi-class / industrial ARM boxes)
# rather than the dev machine's amd64. A single `docker buildx build --load`
# can only load one platform into the local daemon at a time, so
# docker-build-edge defaults to the host arch for a local `docker run`-able
# image; docker-push-edge builds+pushes the full multi-arch manifest directly
# to the registry (buildx supports multi-platform only when pushing, not
# loading). Override the single-arch build with EDGE_PLATFORM, e.g.
# `make docker-build-edge EDGE_PLATFORM=linux/arm64`.
EDGE_PLATFORM  ?= linux/$(shell $(GO) env GOARCH)
EDGE_PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: docker-build docker-push docker-build-edge docker-push-edge

docker-build: proto-go  ## Build the controller-manager Docker image (regenerates Go proto stubs first — the build needs them; see Dockerfile. No Python toolchain required.)
	docker build -t $(IMG) .

docker-push:    ##! Push the controller-manager Docker image
	docker push $(IMG)

docker-build-edge: proto-go  ##! Build a local, host-arch edge-node image (see Dockerfile.edge); override with EDGE_PLATFORM
	docker buildx build --platform $(EDGE_PLATFORM) -t $(IMG_EDGE) -f Dockerfile.edge --load .

docker-push-edge: proto-go  ##! Build + push the multi-arch edge-node image (amd64+arm64) directly to the registry
	docker buildx build --platform $(EDGE_PLATFORMS) -t $(IMG_EDGE) -f Dockerfile.edge --push .

# ── Helm chart ────────────────────────────────────────────────────────────────

.PHONY: helm-sync helm-verify-sync helm-lint helm-template

HELM_CHART ?= deploy/swarmada

helm-sync:      ##! Regenerate the chart CRDs + webhooks from config/ (run after `make manifests`)
	bash hack/sync-helm-crds.sh
	$(PYTHON) hack/sync-helm-webhooks.py

helm-verify-sync: ##! Fail if the chart CRDs or webhooks have drifted from config/ (CI guard)
	@tmp=$$(mktemp); OUT=$$tmp bash hack/sync-helm-crds.sh >/dev/null; \
	  if ! diff -u $(HELM_CHART)/templates/crds.yaml $$tmp; then \
	    echo "ERROR: $(HELM_CHART)/templates/crds.yaml is out of sync with config/crd/bases."; \
	    echo "       Run 'make helm-sync' and commit the result."; \
	    rm -f $$tmp; exit 1; \
	  fi; \
	  rm -f $$tmp; echo "chart CRDs are in sync with config/crd/bases"
	@tmp=$$(mktemp); OUT=$$tmp $(PYTHON) hack/sync-helm-webhooks.py >/dev/null; \
	  if ! diff -u $(HELM_CHART)/templates/webhook.yaml $$tmp; then \
	    echo "ERROR: $(HELM_CHART)/templates/webhook.yaml is out of sync with config/webhook/manifests.yaml."; \
	    echo "       A webhook declared in config/ but missing from the chart is NOT enforced on a"; \
	    echo "       Helm-installed cluster. Run 'make helm-sync' and commit the result."; \
	    rm -f $$tmp; exit 1; \
	  fi; \
	  rm -f $$tmp; echo "chart webhooks are in sync with config/webhook/manifests.yaml"

helm-lint:      ##! Lint the Helm chart (requires helm; brew install helm)
	@command -v helm >/dev/null 2>&1 || { echo "helm not found. Install: brew install helm"; exit 1; }
	helm lint $(HELM_CHART)

helm-template:  ##! Render the chart as a smoke test (default + metrics toggles on)
	@helm template swarmada $(HELM_CHART) >/dev/null
	@helm template swarmada $(HELM_CHART) \
	  --set metrics.serviceMonitor.enabled=true \
	  --set metrics.prometheusRule.enabled=true >/dev/null
	@echo "helm template OK (default and metrics.serviceMonitor/prometheusRule enabled)"

# ── K8s deploy ────────────────────────────────────────────────────────────────
#
# Every target below talks to a cluster, and without one kubectl falls back to
# localhost:8080 and reports "failed to download openapi ... connection refused"
# — which reads like a broken manifest rather than "you have no cluster". The
# require-cluster preflight turns that into the actual next step. It still exits
# non-zero: a deploy target that prints advice and reports success would be worse
# than the confusing error it replaces.

.PHONY: install uninstall deploy undeploy require-cluster

# DEPLOY_IMG is the tag config/default/ pins for the manager. `make docker-build`
# defaults to $(IMG) (a registry tag), so the two do not line up unless you pass
# IMG explicitly — the guidance below spells that out rather than leaving it to
# an ImagePullBackOff.
DEPLOY_IMG ?= swarmada-controller:dev

require-cluster:
	@kubectl config current-context >/dev/null 2>&1 || { \
	  echo ""; \
	  echo "No Kubernetes cluster is selected, so there is nothing to deploy to."; \
	  echo ""; \
	  contexts=$$(kubectl config get-contexts -o name 2>/dev/null | tr '\n' ' '); \
	  clusters=$$(kind get clusters 2>/dev/null | tr '\n' ' '); \
	  [ -n "$$contexts" ] && echo "  kubeconfig contexts defined : $$contexts"; \
	  [ -n "$$clusters" ] && echo "  live kind clusters          : $$clusters"; \
	  echo ""; \
	  echo "  Pick an existing one:"; \
	  echo "    kubectl config use-context <name>"; \
	  echo ""; \
	  echo "  Or bring one up and deploy into it:"; \
	  echo "    kind create cluster --name swarmada-dev"; \
	  echo "    make docker-build IMG=$(DEPLOY_IMG)"; \
	  echo "    kind load docker-image $(DEPLOY_IMG) --name swarmada-dev"; \
	  echo "    make install && make deploy"; \
	  echo ""; \
	  echo "  Or let the quickstart do all of it:  make quickstart"; \
	  echo ""; \
	  exit 1; }
	@kubectl cluster-info >/dev/null 2>&1 || { \
	  echo ""; \
	  echo "kubeconfig context '$$(kubectl config current-context)' is selected but the cluster"; \
	  echo "is unreachable — a deleted kind/minikube cluster leaves its context behind."; \
	  echo ""; \
	  clusters=$$(kind get clusters 2>/dev/null | tr '\n' ' '); \
	  [ -n "$$clusters" ] && echo "  live kind clusters: $$clusters"; \
	  echo ""; \
	  echo "  Switch to a live one:  kubectl config use-context <name>"; \
	  echo "  Recreate this one:     kind create cluster --name swarmada-dev"; \
	  echo "  Or start fresh:        make quickstart"; \
	  echo ""; \
	  exit 1; }

install: require-cluster  ## Install CRDs into the cluster
	kubectl apply -k config/crd/bases/

uninstall: require-cluster  ##! Remove CRDs from the cluster
	kubectl delete -k config/crd/bases/ --ignore-not-found

deploy: require-cluster  ## Deploy the controller-manager to the cluster
	kubectl apply -k config/default/
	@echo ""
	@echo "Applied. The manager Deployment runs image '$(DEPLOY_IMG)' (pinned in"
	@echo "config/default/kustomization.yaml). If the pod reports ImagePullBackOff,"
	@echo "that image is not in the cluster yet:"
	@echo "    make docker-build IMG=$(DEPLOY_IMG)"
	@echo "    kind load docker-image $(DEPLOY_IMG) --name <cluster>   # or: minikube image load $(DEPLOY_IMG)"
	@echo "    kubectl -n system rollout restart deploy/swarmada-controller-manager"
	@echo ""
	@echo "Watch it come up:  kubectl -n system rollout status deploy/swarmada-controller-manager"

undeploy: require-cluster  ##! Remove the controller-manager from the cluster
	kubectl delete -k config/default/ --ignore-not-found

# ── Demo ──────────────────────────────────────────────────────────────────────

.PHONY: demo-a

# The samples are namespaced into warehouse-a, which nothing here creates — so a
# bare `kubectl apply` fails with "namespaces \"warehouse-a\" not found". Create it
# first (idempotent) rather than making the caller decode that.
DEMO_NAMESPACE ?= warehouse-a

demo-a: require-cluster  ##! Demo A: schedule tasks across simulated fleet
	@kubectl get namespace $(DEMO_NAMESPACE) >/dev/null 2>&1 || { \
	  echo "creating namespace $(DEMO_NAMESPACE) (the samples are namespaced into it)"; \
	  kubectl create namespace $(DEMO_NAMESPACE); }
	kubectl apply -f config/samples/

# Demo B (camera fault → degrade → reroute → recover) runs via the live
# simulation adapter in the quickstart:  make quickstart SCENARIO=hardware-fault
# (or the human-in-the-loop  make demo SCENARIO=hardware-fault). The old manual
# demo-b-inject/demo-b-recover targets were removed — they wrote Robot.status by
# hand (RA-1 anti-pattern) against a since-changed schema.

# ── Quickstart ─────────────────────────────────────────────────────────────────

.PHONY: demo demo-test quickstart quickstart-test swarmtop

demo:           ## Human-in-the-loop: kind + live sim + swarmtop watching every field flow. Pick a scenario: make demo SCENARIO=hardware-fault (default: full-surface)
	DEMO_LAUNCH_SWARMTOP=1 bash examples/warehouse-quickstart/run.sh --scenario $(if $(SCENARIO),$(SCENARIO),full-surface) --pace off

demo-test:      ## Headless CI gate: full-surface on kind, assert every phase/estop + §9.3.8 alerts + RA-1, delete the cluster. Fails on any miss.
	bash examples/full-surface-demo/run.sh

demo-test-walkthrough:  ##! Same gate, narrated: explains each step, what to expect, and how to watch it (Enter between phases; DEMOTEST_NO_PROMPT=1 to auto-continue).
	DEMOTEST_WALKTHROUGH=1 bash examples/full-surface-demo/run.sh

swarmtop:       ##! Build the swarmtop terminal fleet inspector (tools/swarmtop, its own module)
	$(MAKE) -C tools/swarmtop build
	@echo "built tools/swarmtop/bin/swarmtop — run 'make quickstart', then in a 2nd terminal: tools/swarmtop/bin/swarmtop -n warehouse-a"

quickstart:     ## Bring up the warehouse quickstart on kind (needs Docker). Pick a scenario: make quickstart SCENARIO=hardware-fault
	bash examples/warehouse-quickstart/run.sh $(if $(SCENARIO),--scenario $(SCENARIO)) $(if $(ROBOTS),--robots $(ROBOTS))

quickstart-test: ## Run the quickstart end-to-end on kind, assert ✅, then delete the cluster (CI)
	@cluster="$${QUICKSTART_CLUSTER:-swarmada-quickstart}"; \
	trap 'kind delete cluster --name '"$$cluster"' >/dev/null 2>&1 || true' EXIT; \
	bash examples/warehouse-quickstart/run.sh

# ── RFC spec ──────────────────────────────────────────────────────────────────
# There are no spec-assembly targets in this repository, deliberately.
#
# RFC-0001 is published here as an assembled document (rfcs/dist/). The chapter
# sources it is assembled FROM, and the tooling that assembles and checks them
# (rfcs/assemble.py, scripts/specdiff.py), are not part of this repository —
# they live in the authoring tree. Targets that invoked them existed here until
# draft 3 and could not work: every recipe failed on a missing script.
#
# They had been hidden from `make help`, which was the wrong remedy. A hidden
# target that fails is worse than an absent one: it is still findable by anyone
# reading this file, and it fails in a way that reads as a broken checkout.
#
# Read the specification at rfcs/dist/RFC-0001-core-spec.md. To regenerate it,
# work in the authoring tree.

# ── Clean ─────────────────────────────────────────────────────────────────────
# `clean` removes only what the build/test targets write into the working tree,
# so a following `make build` is an ordinary rebuild.
#
# `clean-all` additionally drops the machine-local state those targets created
# OUTSIDE the tree: the Go build/test caches, the quickstart kind cluster, and
# the locally-built image. It deliberately does NOT run `go clean -modcache` —
# the module cache is shared by every Go project on the machine, so wiping it
# from a repo-level target would force an unrelated multi-GB re-download.

.PHONY: clean clean-all

QUICKSTART_CLUSTER ?= swarmada-quickstart

clean:          ## Remove build outputs (bin/, coverage.txt, the swarmtop binary)
	rm -rf bin
	rm -f coverage.txt
	@test -f tools/swarmtop/Makefile && $(MAKE) -s -C tools/swarmtop clean || true
	rm -rf tools/swarmtop/bin
	@echo "clean: build outputs removed."

clean-all: clean  ##! clean + Go build/test caches, the quickstart kind cluster, and the local image
	$(GO) clean -cache -testcache
	@command -v kind >/dev/null 2>&1 && kind delete cluster --name $(QUICKSTART_CLUSTER) >/dev/null 2>&1 \
	  && echo "  deleted kind cluster $(QUICKSTART_CLUSTER)" || true
	@command -v docker >/dev/null 2>&1 && docker image rm $(IMG) >/dev/null 2>&1 \
	  && echo "  removed image $(IMG)" || true
	@echo "clean-all: build/test caches cleared. The shared Go module cache was left intact."

.PHONY: adapters
ADAPTER_ORG ?= https://github.com/swarmada
ADAPTERS    ?= fleet-adapter-ros2 fleet-adapter-vda5050 fleet-adapter-mavlink
adapters: ## Fetch/update the reference Fleet Adapters into adapters/external/
	@mkdir -p adapters/external
	@for a in $(ADAPTERS); do \
	  if [ -d adapters/external/$$a/.git ]; then \
	    echo "updating $$a"; git -C adapters/external/$$a pull --ff-only; \
	  else \
	    echo "cloning  $$a"; git clone $(ADAPTER_ORG)/$$a.git adapters/external/$$a; \
	  fi; \
	done
