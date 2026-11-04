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
# `| xargs ls` was the wrong terminator for the two glob arms and hid TWO different
# failures, because xargs exits 0 whether or not it ran anything:
#   * GNU xargs (Linux, CI) runs the command once even with no input, so `xargs ls`
#     became a bare `ls` and printed the whole tree root. GO took that listing as its
#     value, `ifeq ($(GO),)` was false, and the careful $(error ...) below never fired —
#     the user saw `make: 11labs_connect.sh: No such file or directory` instead.
#   * BSD xargs (macOS, which this file targets) does NOT run the command on empty
#     input — but still exits 0, so the `||` chain STOPPED at arm 2 with an empty value
#     and arms 3-5 became unreachable. A Mac with go at /opt/homebrew/bin/go but not on
#     PATH was told "Cannot find the 'go' binary. ... Or install: brew install go",
#     which it already had, sitting at an arm the chain could not reach.
# `read` fails on empty input, so the arm now fails when the glob matches nothing, and
# `[ -x ]` means a match that is not an executable does not win either.
GO ?= $(shell \
  command -v go 2>/dev/null || \
  ls -d $(HOME)/go*/bin/go 2>/dev/null | grep -v pkg | sort -V | tail -1 | \
    { read -r p && [ -x "$$p" ] && printf '%s' "$$p"; } || \
  ls -d $(HOME)/sdk/go*/bin/go 2>/dev/null | sort -V | tail -1 | \
    { read -r p && [ -x "$$p" ] && printf '%s' "$$p"; } || \
  ls /opt/homebrew/bin/go 2>/dev/null || \
  ls /usr/local/go/bin/go 2>/dev/null || \
  echo "")

# setup-macos is exempt: its entire job is installing Go (and the other
# prerequisites) on a machine that doesn't have them yet, so it cannot be
# gated on Go already being found — that made `make setup-macos` itself
# fail with this same error on a genuinely empty machine (swarmada#25).
ifeq ($(GO),)
ifeq ($(filter setup-macos,$(MAKECMDGOALS)),)
$(error Cannot find the 'go' binary.\
  GoLand users: Settings > Go > GOROOT  shows the exact path. Then:\
    export PATH="<goroot>/bin:$$PATH" >> ~/.zshrc\
  Or install: brew install go\
  Or override: GO=/your/go/bin/go make setup)
endif
endif

# Make the located go binary visible to all child processes (golangci-lint,
# controller-gen, etc.) even when the shell PATH is incomplete -- WITHOUT putting
# Go's directory in front of the caller's PATH when it does not have to be there.
# It used to be prepended unconditionally. On Homebrew that directory is
# /opt/homebrew/bin, which also holds Homebrew's python3, ruff, pytest, mypy and
# black, so every recipe ran those instead of the caller's active venv. And since
# $(shell) does not see exported variables before GNU make 4.4, a $(shell) probe
# and the recipe that used its answer resolved DIFFERENT binaries. Three cases:
#   * PATH already resolves `go` to $(GO) (the common case): PATH is untouched.
#   * no `go` on PATH at all: Go's directory is APPENDED -- visible, shadowing nothing.
#   * PATH resolves a different `go` (an explicit GO=... override): prepended, so
#     child tools use the Go that was asked for. Only this case can still shadow.
GO_ON_PATH := $(shell command -v go 2>/dev/null)
ifeq ($(GO_ON_PATH),)
export PATH := $(PATH):$(dir $(GO))
else ifneq ($(GO_ON_PATH),$(GO))
export PATH := $(dir $(GO)):$(PATH)
endif

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
MYPY           ?= mypy
IMG            ?= ghcr.io/swarmada/swarmada:latest
# The version the manager image stamps (Dockerfile ARG VERSION): the chart's appVersion.
VERSION        ?= $(shell awk -F'"' '/^appVersion:/ {print $$2}' deploy/swarmada/Chart.yaml)

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
	  echo "make run: admission webhooks are DISABLED (no local serving certs). No admission check runs:"; \
	  echo "  - the estop checks: estop-trigger and estop-clear are not checked on the estop annotation,"; \
	  echo "    and neither are the cancel and append verbs; patch on the object is enough;"; \
	  echo "  - the draft gate: activate is not checked, and anyone, a ServiceAccount included, who can"; \
	  echo "    patch a FleetTask can clear spec.draft and dispatch a drafted task;"; \
	  echo "  - the robots/status guard: anyone who can write robots/status can set a stopped robot's"; \
	  echo "    estopState to Normal, with no clear, no resume checks and no audit entry;"; \
	  echo "  - the delete guard: a Robot that holds a stop can be deleted and created again Normal;"; \
	  echo "  - RobotClass defaulting and the admission gates (incl. the FleetAdapter dispatch gate)."; \
	  echo "  A request this manager admits may still be rejected in-cluster. Use it on a single-user"; \
	  echo "  development cluster only (docs/known-limitations.md, installs without admission webhooks)."; \
	  echo "  With certs in place: make run ENABLE_WEBHOOKS=true"; \
	fi
	ENABLE_WEBHOOKS=$(ENABLE_WEBHOOKS) $(GO) run ./cmd/manager

fmt:            ##! Run gofmt + goimports
	$(GO) fmt ./...

vet:            ##! Run go vet
	$(GO) vet ./...

# Kubernetes version for the envtest control-plane binaries (docs/testing.md layer 2): the
# newest setup-envtest offers, and never below the chart's kubeVersion floor (1.31), so the
# suites do not assert the behaviour of an API server the chart does not support.
ENVTEST_K8S_VERSION ?= 1.37.0
# The setup-envtest tool itself, pinned: `@latest` made the CI install a different tool on
# every run. .github/workflows/ci.yml installs the same version; change both together.
SETUP_ENVTEST_VERSION ?= v0.25.1

setup-envtest:  ##! Install the envtest control-plane binaries (kube-apiserver, etcd) for controller tests
	$(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)
	@setup-envtest use $(ENVTEST_K8S_VERSION)
	@echo "envtest assets installed. 'make test-go' will now run the controller envtest suite."

# ENVTEST IS REQUIRED when CI=true (GitHub Actions sets it) or SWARMADA_REQUIRE_ENVTEST=1:
# then no assets is a FAILURE, not a skip. Without either variable, a developer with no
# assets keeps the documented skip. Installing the assets in CI is not enough on its own:
# an install that silently stops working would put CI back to skipping every envtest behind
# an `ok`, which is how no envtest ran in CI before this. The two suites' TestMain enforce the
# same rule, so a direct `go test` is covered too.
#
# internal/webhook runs in its own `go test` with an explicit -timeout $(WEBHOOK_TEST_TIMEOUT) (interim;
# it goes when that package's envtests share one API server and it runs well inside the default).
# Under -race it takes about 660 s on the Mac mini and 1179 s on a GitHub ubuntu-latest runner
# (measured 2026-11-03), past the old 15m; the timeout is 1.5 x the runner time, rounded up to
# 5 minutes, and at least 30m. It runs beside the other packages, which keep the default, and its
# cover profile is appended to coverage.txt.
WEBHOOK_TEST_TIMEOUT ?= 30m
test-go:        ##! Run Go unit tests with race detector (+ envtest suites; REQUIRED when CI=true or SWARMADA_REQUIRE_ENVTEST=1)
	@ASSETS=""; \
	if command -v setup-envtest >/dev/null 2>&1; then \
	  ASSETS=$$(setup-envtest use $(ENVTEST_K8S_VERSION) -p path 2>/dev/null || true); \
	fi; \
	if [ -n "$$ASSETS" ]; then echo "envtest assets: $$ASSETS"; \
	elif [ "$${CI:-}" = "true" ] || [ "$${SWARMADA_REQUIRE_ENVTEST:-}" = "1" ]; then \
	  echo "test-go: envtest is REQUIRED here (CI=$${CI:-unset}, SWARMADA_REQUIRE_ENVTEST=$${SWARMADA_REQUIRE_ENVTEST:-unset}) and no envtest assets were found for $(ENVTEST_K8S_VERSION). Run 'make setup-envtest' first; skipping the envtest suites here would report a green run that never exercised them." >&2; \
	  exit 1; \
	else echo "envtest assets not found; controller envtest tests will skip (run 'make setup-envtest')"; fi; \
	WHD=$$(mktemp -d); \
	KUBEBUILDER_ASSETS="$$ASSETS" $(GO) test -race -timeout $(WEBHOOK_TEST_TIMEOUT) -coverprofile="$$WHD/cover.txt" ./internal/webhook/ > "$$WHD/out.log" 2>&1 & WHPID=$$!; \
	rc=0; KUBEBUILDER_ASSETS="$$ASSETS" $(GO) test -race -coverprofile=coverage.txt $$($(GO) list ./... | grep -v '/internal/webhook$$') || rc=$$?; \
	wait $$WHPID || rc=$$?; cat "$$WHD/out.log"; \
	if [ -f "$$WHD/cover.txt" ]; then \
	  if [ -f coverage.txt ]; then tail -n +2 "$$WHD/cover.txt" >> coverage.txt; else cp "$$WHD/cover.txt" coverage.txt; fi; \
	fi; \
	rm -f "$$WHD/cover.txt" "$$WHD/out.log"; rmdir "$$WHD"; exit $$rc

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
# The Python stub generator, pinned for the same reason and more: it decides the runtime floor
# baked into the committed stubs every adapter imports (pyproject.toml, `codegen` extra, says
# why this version). It has no wheel above Python 3.12, so proto-py runs it in an isolated
# uv environment when the active interpreter does not carry exactly this version.
GRPCIO_TOOLS_VERSION := 1.64.1
# The protoc the committed Go stubs were generated with. protoc-gen-go writes the compiler's
# version into the header of every .pb.go (`protoc v7.35.1` is libprotoc 35.1), so any other
# protoc rewrites all four headers on every run although the generated code is identical.
# proto-go refuses another version rather than leave that diff in the tree.
PROTOC_VERSION := 35.1

proto-tools:    ##! Install proto codegen plugins: protoc-gen-go, protoc-gen-go-grpc, grpcio-tools (one-time)
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	@$(PYTHON) -c 'from importlib.metadata import version as v; assert v("grpcio-tools") == "$(GRPCIO_TOOLS_VERSION)"' >/dev/null 2>&1 && \
	  echo "grpcio-tools $(GRPCIO_TOOLS_VERSION) already available to $$($(PYTHON) -V 2>&1) — skipping pip" || { \
	    echo "installing grpcio-tools for $$($(PYTHON) -V 2>&1) ($$(command -v $(PYTHON)))"; \
	    $(PYTHON) -m pip install grpcio-tools==$(GRPCIO_TOOLS_VERSION) || { \
	      echo ""; \
	      echo "pip refused: this interpreter is externally managed (PEP 668)."; \
	      echo "Install into an environment you own rather than forcing it into the system one:"; \
	      echo "  * activate your project venv (python -m venv / uv), then re-run: make proto-tools"; \
	      echo "  * or point the target at it:  make proto-tools PYTHON=/path/to/env/bin/python"; \
	      echo "  * last resort (per-user, not system-wide):  $(PYTHON) -m pip install --user grpcio-tools==$(GRPCIO_TOOLS_VERSION)"; \
	      exit 1; }; }

proto: proto-tools proto-go proto-py  ## Generate gRPC stubs (installs tools, Go + Python) — the one codegen target

proto-go:       ##! Generate only the Go gRPC stubs (no Python toolchain required; refuses a protoc other than PROTOC_VERSION)
	@v="$$($(PROTOC) --version 2>/dev/null)"; \
	if [ "$$v" != "libprotoc $(PROTOC_VERSION)" ]; then \
	  echo "proto-go: '$(PROTOC)' is '$${v:-not runnable}', not the pinned libprotoc $(PROTOC_VERSION)." >&2; \
	  echo "  The committed stubs record protoc $(PROTOC_VERSION); another version rewrites their headers." >&2; \
	  echo "  Install protoc $(PROTOC_VERSION) from github.com/protocolbuffers/protobuf/releases and" >&2; \
	  echo "  re-run with PROTOC=/path/to/that/protoc." >&2; exit 2; fi
	$(PROTOC) \
	  -Iproto \
	  --go_out=proto --go_opt=paths=source_relative \
	  --go-grpc_out=proto --go-grpc_opt=paths=source_relative \
	  proto/fleet_adapter/v1/fleet_adapter.proto \
	  proto/compute/v1/compute.proto
	@# -Iproto matches buf's module root (proto/buf.yaml) so compute/v1's
	@# `import "fleet_adapter/v1/fleet_adapter.proto"` resolves identically under
	@# both buf and protoc; --go_out=proto (not `.`) keeps paths=source_relative
	@# output under proto/<pkg>/v1/ rather than a second tree at the repo root,
	@# since source_relative computes paths against the -I root, not the CLI arg.
	@# Both files are listed together so protoc-gen-go resolves the cross-package
	@# Go import in one pass.

PROTO_PY_ARGS = -Iproto --python_out=proto --grpc_python_out=proto \
  fleet_adapter/v1/fleet_adapter.proto compute/v1/compute.proto

proto-py:       ##! Regenerate the committed Python gRPC stubs after a .proto change (needs uv or the codegen extra)
	@have=$$($(PYTHON) -c 'from importlib.metadata import version as v; print(v("grpcio-tools"))' 2>/dev/null); \
	if [ "$$have" = "$(GRPCIO_TOOLS_VERSION)" ]; then \
	  $(PYTHON) -m grpc_tools.protoc $(PROTO_PY_ARGS); \
	elif command -v uv >/dev/null 2>&1; then \
	  echo "proto-py: grpcio-tools $${have:-absent} here; generating with $(GRPCIO_TOOLS_VERSION) in an isolated uv environment"; \
	  uv run --no-project --isolated --python 3.12 --with grpcio-tools==$(GRPCIO_TOOLS_VERSION) \
	    python -m grpc_tools.protoc $(PROTO_PY_ARGS); \
	else \
	  echo "proto-py: refusing to generate with grpcio-tools $${have:-absent}; the committed stubs are"; \
	  echo "  generated by $(GRPCIO_TOOLS_VERSION) and another version changes the runtime floor every"; \
	  echo "  adapter inherits. Install uv, or on Python <=3.12: pip install '.[codegen]'"; \
	  exit 2; \
	fi
	@# The stubs are committed, and the conformance targets and the adapters use them as they
	@# are; only regenerating them needs grpcio-tools $(GRPCIO_TOOLS_VERSION) (uv, or the codegen extra).
	@# Import root is `proto/` so the stubs import as `fleet_adapter.v1.*` /
	@# `compute.v1.*`; consumers put `proto/` on PYTHONPATH. Regenerate after
	@# changing this flag.

proto-lint:     ##! Lint protos: proto3 conventions + buf lint/breaking (hack/check-proto3.sh)
	bash hack/check-proto3.sh

# ── Python ────────────────────────────────────────────────────────────────────

.PHONY: install-py fmt-py lint-py test-py test-sdk test-conformance-harness check-error-strings \
        check-adapter-template ruff-pin

# CI installs this, not install-py. The `ml` extra pulls torch/pandas/matplotlib —
# multiple gigabytes — and NOTHING the spec/conformance gate touches imports them:
# the whole tests/python suite passes with torch, pandas, matplotlib and transforms3d
# blocked at the import hook. grpcio-tools is the codegen extra, needed only to regenerate
# the committed stubs (proto-py), so dev + codegen is the complete set. Do not "simplify"
# this back to install-py: the cost is a multi-GB download on every push, and it is
# invisible locally because a developer machine already has the extras.
install-py-ci:  ##! Install only the Python deps the CI spec/conformance gate needs (no ml extras)
	$(PYTHON) -m pip install -e ".[dev,codegen]" || \
	  $(PYTHON) -m pip install --break-system-packages -e ".[dev,codegen]"

install-py:     ##! Install Python dev dependencies into the active venv
	@# Fall back to --break-system-packages on an externally-managed interpreter
	@# (PEP 668 / Homebrew), matching the proto-tools target's convention. Prefer a
	@# per-project venv; this keeps `make install-py` working on a bare system python.
	$(PYTHON) -m pip install -e ".[dev,simulation]" || \
	  $(PYTHON) -m pip install --break-system-packages -e ".[dev,simulation]"

fmt-py: ruff-pin  ##! Format Python code with black + ruff
	black simulation/ scripts/ tests/python/
	$(RUFF) check --fix .

# ruff is PINNED, and the pin has one home: the `ruff==` line in pyproject.toml's dev extra,
# which uv.lock records too. Unpinned, lint-py was a different linter on every machine --
# PATH's ruff locally, the newest release in CI -- and agreed with itself only by luck.
# RUFF is the first of these that IS the pinned version: PATH's ruff, the ruff installed into
# $(PYTHON) (what `pip install -e .[dev]` puts there), `uvx` running exactly the pinned
# version. Failing all three it is PATH's ruff, which ruff-pin then refuses by name.
# RUFF=... overrides the choice and is held to the same pin.
# PATH="$(PATH)" is load-bearing wherever the exports above change PATH (GOBIN always, Go's
# directory on a GO=... override -- see the comment at GO_ON_PATH): $(shell) does not see
# exported variables before GNU make 4.4, so without it the version tested here is not the
# binary the recipe then runs.
RUFF_VERSION := $(shell sed -n 's/^[[:space:]]*"ruff==\([0-9][0-9.]*\)".*/\1/p' pyproject.toml)
RUFF ?= $(shell PATH="$(PATH)"; want="ruff $(RUFF_VERSION)"; \
          if [ "$$(ruff --version 2>/dev/null)" = "$$want" ]; then echo ruff; \
          elif [ "$$($(PYTHON) -m ruff --version 2>/dev/null)" = "$$want" ]; then echo "$(PYTHON) -m ruff"; \
          elif command -v uvx >/dev/null 2>&1; then echo "uvx --from ruff==$(RUFF_VERSION) ruff"; \
          else echo ruff; fi)

ruff-pin:       ##! Fail unless $(RUFF) is the ruff version pyproject.toml pins
	@if [ -z "$(RUFF_VERSION)" ]; then \
	  echo "ruff-pin: no \"ruff==X.Y.Z\" line in pyproject.toml, so no version to hold ruff to" >&2; exit 2; fi; \
	v="$$($(RUFF) --version 2>/dev/null)"; \
	if [ "$$v" != "ruff $(RUFF_VERSION)" ]; then \
	  echo "ruff-pin: '$(RUFF)' is '$${v:-not runnable}', not the pinned ruff $(RUFF_VERSION)." >&2; \
	  echo "          Install it ($(PYTHON) -m pip install ruff==$(RUFF_VERSION)) or install uv." >&2; exit 2; fi

# ruff reads the WHOLE tree: `.` plus the `extend-exclude` list in pyproject.toml, where
# every directory left out carries its reason. It used to name `simulation/ scripts/` --
# two of the eight Python directories -- so a green lint-py said nothing about adapters/,
# tests/python/ or conformance/, and a new directory was unlinted until someone noticed.
# Do not narrow this back to a list; exclude in pyproject.toml, with the reason.
# mypy is still simulation/ only: the config is `strict = true`, and extending it to the
# rest of the tree is its own piece of work, not a target edit.
lint-py: ruff-pin check-error-strings rule-index-check check-adapter-template  ##! Lint Python: ruff over the whole tree (exclusions in pyproject.toml), the rendered adapter template, mypy --strict over simulation/, message-voice rules
	$(RUFF) check .
	$(MYPY) simulation/

# The one exclusion above that is not generated or a copy: adapters/template/ is a
# cookiecutter template, not Python until rendered. This renders it and checks the result --
# TOML/YAML parse, every .py compiles, ruff under this repository's rules, and the rendered
# adapter's own tests against the in-tree SDK. It is the file a new adapter author copies
# first; excluded and unrendered, whatever was wrong in it was wrong in every adapter made
# from it.
check-adapter-template: ruff-pin  ##! Render adapters/template/ and parse, compile, lint and test the result
	$(PYTHON) scripts/check-adapter-template.py --ruff "$(RUFF)"

check-error-strings: ##! Message voice + refuse internal citations on the public surface
	# Two gates, both in check-error-strings.py:
	#   1. Voice_and_Tone on operator-facing message strings. It scans the whole tree
	#      (skipping _test.go, zz_generated, vendor, proto, bin, venv, node_modules) and was
	#      wired in 2026-08-20 — it had existed since it was written with no make target and
	#      no CI job, so it had never once run against the code it governs.
	#   2. Internal citations on the public surface (added 2026-09-04): tracker identifiers,
	#      private-document section references, internal decision records, and the scar a
	#      bad excision leaves behind. The gate lists its own patterns; they are deliberately
	#      not spelled here, because this file is content-checked and would report itself.
	#      It reads files WHOLE, which is the point — every earlier check in this series was
	#      line-based, and the 2026-09-03 sweep left four citations wrapped across a line
	#      break that no line-based grep could see. PUBLICATION.yaml C25/C26 are the
	#      pre-promote backstop and are line-based; this is the primary gate.
	$(PYTHON) scripts/check-error-strings.py .

# test-sdk is a PREREQUISITE of test-py, not a sibling nobody calls. The SDK's suite
# existed and no target, script or workflow ran it -- which is how FenceGuard.adopt()
# and its four C3.8 tests sat green in sdk/python/tests/ for weeks while the reference
# adapter shipped without the primitive and failed C3.8. CI's single Python entry point
# is `make test-py`; anything not reachable from it is not a gate. Run it alone with
# `make test-sdk` when you want the SDK suite by itself.
test-py: test-sdk test-conformance-harness   ##! Run Python tests with pytest (SDK suite, harness suite, then the repo suite)
	$(PYTHON) -m pytest tests/python/ -v

# The conformance harness's OWN unit tests -- 45 of them, driving _Driver checks against an
# in-memory session, no gRPC server, no adapter. Same shape as test-sdk above and for the same
# reason: the suite existed and nothing ran it, and its re-drift guard (test_harness_docs.py,
# the docstring must name the families the file records) went red when C18 landed and nobody
# saw. It is outside the root `testpaths`, so `pytest tests/python/` never collects it. It
# uses the root pytest config and the committed stubs under proto/ (conftest.py loads them).
test-conformance-harness: ##! Run the conformance harness's own unit tests (adapters/conformance/tests)
	$(PYTHON) -m pytest adapters/conformance/tests/ -v

# A SEPARATE distribution (sdk/python/pyproject.toml, `where = ["src"]`), so it carries
# its own pytest config -- different rootdir, and asyncio_mode STRICT against the root's
# AUTO. It cannot simply be added to the root `testpaths`; the two configs would fight.
# It is also not part of the root editable install, hence the explicit PYTHONPATH.
test-sdk:       ##! Run the SDK's own test suite (swarmada-sdk, separate distribution)
	PYTHONPATH=sdk/python/src $(PYTHON) -m pytest sdk/python/tests/ -v

# ── Conformance ───────────────────────────────────────────────────────────────

.PHONY: conformance conformance-sim conformance-sim-fault

# Run the Fleet Adapter conformance harness against an adapter. Override ADAPTER
# with the command that launches the adapter-under-test ('{port}' is substituted).
# Needs the generated Python stubs (`make proto`); STUB_PATH is where they live.
STUB_PATH ?= proto
ADAPTER   ?= $(PYTHON) adapters/example-noop/noop_adapter.py --endpoint localhost:{port}
# The name the report carries. Defaults to example-noop only while ADAPTER is the default;
# an overridden ADAPTER with no ADAPTER_NAME keeps the harness's own default name, which the
# harness refuses to store in conformance-reports/ -- a wrong name is worse than none.
ifeq ($(origin ADAPTER),file)
ADAPTER_NAME ?= example-noop
else
ADAPTER_NAME ?= adapter-under-test
endif

conformance:            ## Run the adapter conformance harness (see adapters/conformance/README.md)
	$(PYTHON) -m adapters.conformance --stub-path $(STUB_PATH) --adapter-name '$(ADAPTER_NAME)' \
	  --port $(CONFORMANCE_PORT) --adapter-cmd '$(ADAPTER)' $(CONFORMANCE_ARGS)

# --scenario healthy-fleet makes the sim actually STREAM telemetry. Without it the adapter
# is silent on that path and four documented MUSTs (C6.1/C6.2/C6.3, C2.3) are unverifiable
# for want of a single TelemetryPayload — including explicit presence, which is the whole
# reason the safety scalars are declared `optional`.
# The simulation adapter's plant model writes the robot's true speed to SIM_WITNESS, and the
# harness reads it as the motion witness C5.7 grades an early STOPPED against. The witness is
# the simulated plant, never the adapter's own confirmation path (ADR-0071 Amendment 1).
#
# ONE PATH PER RUN. The adapter opens the witness for writing, so two runs started together
# -- in this tree and a copy of it, or in two sessions -- truncate and interleave each
# other's samples, and C5.7 grades one run's STOPPED against a mix of two plants. Filtering
# by robot id does not separate them: every run's robot is robot-conformance-1. The witness
# is therefore named after the port, which two concurrent runs cannot share: the harness
# listens on it and the adapter dials it. Start a second run beside the first by setting
# CONFORMANCE_PORT, and its witness follows. Override SIM_WITNESS to pin a path instead.
#
# The path must carry nothing for a shell to expand. The harness is given it in the recipe's
# own quotes and the adapter is given it inside --adapter-cmd, which a DIFFERENT shell runs,
# so anything that resolves per shell -- a pid, a process substitution -- becomes two
# different files, and C5.7 then reads a witness its own run never wrote.
CONFORMANCE_PORT ?= 9090
SIM_WITNESS ?= $(or $(TMPDIR),/tmp)/swarmada-sim-witness-$(CONFORMANCE_PORT).jsonl

# --latch: the simulation adapter keeps the stop latch and reports contract 1.4.0 only with it,
# and reports 1.3.0 without it (adapters/simulation/sim_adapter.py, CONTRACT_VERSION). Every
# conformance run below attests the 1.4.0 adapter, so C5.8 grades the latch.
conformance-sim:           ##! Run the conformance harness against the in-tree simulation adapter
	$(PYTHON) -m adapters.conformance --stub-path $(STUB_PATH) --adapter-name simulation \
	  --port $(CONFORMANCE_PORT) --witness-file '$(SIM_WITNESS)' \
	  --adapter-cmd '$(PYTHON) adapters/simulation/sim_adapter.py --endpoint localhost:{port} --latch --scenario healthy-fleet --witness-file $(SIM_WITNESS)' $(CONFORMANCE_ARGS)

# Same conformance gate, but driving the hardware-fault scenario with fast timers so
# the run also exercises the scenario loader + TelemetryPayload.hardware degrade/recover
# path (Demo B as a live conformance illustration). Requires PyYAML for the scenario loader.
FAULT_AT      ?= 2
RECOVER_AT    ?= 4
conformance-sim-fault:           ##! Conformance harness against the hardware-fault scenario (fast timers)
	$(PYTHON) -m adapters.conformance --stub-path $(STUB_PATH) --adapter-name simulation \
	  --port $(CONFORMANCE_PORT) --witness-file '$(SIM_WITNESS)' \
	  --adapter-cmd '$(PYTHON) adapters/simulation/sim_adapter.py --endpoint localhost:{port} --latch --witness-file $(SIM_WITNESS) \
	    --scenario hardware-fault --fault-at $(FAULT_AT) --recover-at $(RECOVER_AT)' $(CONFORMANCE_ARGS)

.PHONY: stream-lease-measure
# THE SENSOR-STREAM FAIL-SAFE, MEASURED. Stands up a control plane, lets the simulation
# adapter hold a stream under a renewed lease, then KILLS the control plane hard (RPCs
# cancelled, port closed -- not a graceful close, which the adapter correctly reads as
# "we are done" and shuts down on) and times how long the stream stays on.
#
# C18.3 in the conformance suite establishes the DIRECTION -- held across renewals, gone
# after the horizon -- and cannot report the interval: the only wire observable for
# stream state is DeactivateStream, which consumes the activation it reports on, so a
# poll would measure its own round trip. This target reports the number, taken on the
# adapter's own clock at the moment it drops the stream.
#
# Needs no cluster and no robot; runs in about ten seconds.
stream-lease-measure:           ##! Measure the partition-to-stream-off interval (the sensor-stream fail-safe)
	PYTHONPATH=$(STUB_PATH):.:sdk/python/src $(PYTHON) scripts/measure-stream-lease.py

# ── Conformance profiles (cross-surface) ─────────────────────────────────────
#
# The layer above the C0-C16 harness: named profiles at Core/Extended levels, the
# machine-readable ConformanceReport, and the two catalogs the Draft 4
# external-action surface adds (conformance/PROFILES.md). Needs no cluster and no
# network -- the Compute Endpoint fixtures are in-tree and the control-plane
# checks read the shipped CRD and RBAC YAML.
.PHONY: conformance-profiles conformance-reports-register conformance-endpoint conformance-controlplane

CONFORMANCE_REPORT_DIR ?= conformance-reports

# The reports' gate-integrity entries come from two places, and which of them apply depends on
# which levels this tree's registry declares. scripts/conformance-reports-register.txt holds the
# entries for levels conformance/profiles.yaml declares. A level that only a registry overlay
# declares (rfcs/*/conformance-registry.yaml, which a tree may not carry; conformance/registry.py
# merges one when it is present) keeps its entries beside that overlay, in
# conformance-reports-register.txt. They are installed only where the overlay is, so a tree
# without the overlay installs neither the level nor its entries.
CONFORMANCE_OVERLAYS ?= $(wildcard rfcs/*/conformance-registry.yaml)
CONFORMANCE_REGISTER_SOURCES ?= scripts/conformance-reports-register.txt \
  $(wildcard $(CONFORMANCE_OVERLAYS:conformance-registry.yaml=conformance-reports-register.txt))

# The first half runs the adapter harness and writes _adapter.json; the second half profiles
# that file and is what conformance-reports/fleet-adapter.yaml and the REGISTRY cell citing it
# are earned from. It used to run as `>/dev/null 2>&1 || true`, which left a run that refused
# to start, died, or wrote nothing looking exactly like a clean one -- and the profiler then
# graded whatever _adapter.json happened to be on disk, which could be the previous run's.
#
# So: the old report is REMOVED BEFORE the run, so nothing on disk afterwards can be stale;
# the run's own output is shown; its exit status is printed rather than discarded; and an
# absent or empty report stops the target instead of being profiled.
#
# `|| true` is gone, but the reason it existed has not: the harness exits non-zero for a
# non-conformant adapter and the simulation adapter is not conformant, so the status is
# recorded and the recipe continues. That is what `; status=$$?; echo` does -- the line's own
# status is the echo's. A run that produces no report is the case that stops the target, and
# that case is now caught by the check below rather than by an exit code that also fires for
# an honestly-graded red row.
#
# The profiler half follows the same rule: 1 (a surface failed) and 3 (not established) are
# verdicts, written into the reports and printed, and the recipe continues; 2 or anything else
# means no report was written, and stops it. Last, the reports' gate-integrity entries are
# installed beside them (conformance-reports-register, below). conformance-reports/ is not
# published, so a clone gets those entries only from here; without them `make gate-integrity`
# reports every counted site in the fresh reports as NEW.
conformance-profiles:           ## Profile every surface and write the ConformanceReports
	@mkdir -p $(CONFORMANCE_REPORT_DIR)
	rm -f $(CONFORMANCE_REPORT_DIR)/_adapter.json
	$(PYTHON) -m adapters.conformance --stub-path $(STUB_PATH) --adapter-name simulation \
	  --port $(CONFORMANCE_PORT) --witness-file '$(SIM_WITNESS)' \
	  --adapter-cmd '$(PYTHON) adapters/simulation/sim_adapter.py --endpoint localhost:{port} --latch --scenario healthy-fleet --witness-file $(SIM_WITNESS)' \
	  --json $(CONFORMANCE_REPORT_DIR)/_adapter.json; \
	  status=$$?; echo "conformance-profiles: the harness run exited $$status"
	@test -s $(CONFORMANCE_REPORT_DIR)/_adapter.json || { \
	  echo "conformance-profiles: the harness run above wrote no $(CONFORMANCE_REPORT_DIR)/_adapter.json, so there is nothing to profile." >&2; \
	  echo "  An earlier run's report is deliberately not reused: it was removed before the run. Fix the run above and try again." >&2; \
	  exit 1; }
	PYTHONPATH=$(STUB_PATH):. $(PYTHON) -m conformance all \
	  --adapter-report $(CONFORMANCE_REPORT_DIR)/_adapter.json \
	  --out $(CONFORMANCE_REPORT_DIR); \
	  status=$$?; echo "conformance-profiles: the profiler exited $$status"; \
	  case $$status in 0|1|3) ;; *) exit $$status ;; esac
	$(MAKE) --no-print-directory conformance-reports-register

# Writes the entries for exactly the levels this tree's registry declares (see
# CONFORMANCE_REGISTER_SOURCES above), so the installed file names no level that only an
# overlay this tree lacks would declare. The counts in the sources are kept by hand on purpose: derived from the reports,
# a report that skipped one more check would register itself instead of failing the gate.
# `awk 1`, not cat: it ends every source's last line, so one file's last entry cannot run into
# the next file's first.
conformance-reports-register:
	@mkdir -p $(CONFORMANCE_REPORT_DIR)
	awk 1 $(CONFORMANCE_REGISTER_SOURCES) > $(CONFORMANCE_REPORT_DIR)/gate-integrity-register.txt

conformance-endpoint:           ##! Profile one Compute Endpoint (ENDPOINT=plate-reader-http)
	PYTHONPATH=$(STUB_PATH):. $(PYTHON) -m conformance compute-endpoint \
	  --endpoint $(or $(ENDPOINT),plate-reader-http) $(PROFILE_ARGS)

conformance-controlplane:  ##! Profile the deployment statically (no cluster)
	$(PYTHON) -m conformance control-plane $(PROFILE_ARGS)

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

# ── Run the GitHub workflow locally ──────────────────────────────────────────
#
# Runs .github/workflows/ci.yml in a container with `act` (nektos/act), so a change to
# the workflow can be tested before it is pushed. It needs Docker running and `act`
# installed (`brew install act`); it is deliberately NOT a prerequisite of `test` or
# `lint`, because those must run with neither.
#
# WHY THE IMAGE IS NAMED HERE. act's default runner image for `ubuntu-latest` is a
# micro image built on node:*-slim, and this workflow does not run on it: every step
# shells out to `make`, one installs golangci-lint with `curl … | sh`, and another
# calls `sudo apt-get`. The micro image has none of make, curl, sudo or git. The
# medium image below carries them and is the one act's own documentation points at for
# "most things work". The large image (~17GB) adds preinstalled toolchains this
# workflow installs for itself, and buys nothing here.
#
# WHAT THIS IS NOT. It is not GitHub. Caches are cold every run (no cache service),
# `github.sha`/`github.ref` resolve to nothing in a checkout with no git metadata, and
# a job needing a real secret or the artifact service behaves differently. Use it to
# find a broken workflow early, never as the evidence that CI passes.
ACT              ?= act
ACT_WORKFLOW     ?= .github/workflows/ci.yml
ACT_JOB          ?= go
ACT_RUNNER_IMAGE ?= catthehacker/ubuntu:act-latest
ACT_ARTIFACTS    ?= .act-artifacts
# WHY THE NETWORK IS SET. act starts its runner with host networking. On a Docker host
# whose network carries an IPv6 address, kube-apiserver's external-host autodetection
# picks that address and then refuses to start, because envtest passes an IPv4 service
# range: `service IP family "10.0.0.0/24" must match public address family "<v6 addr>"`.
# Every envtest in the run then fails at harness start -- correctly, and for a reason
# that is the local network rather than this repository. A bridge network gives the
# container one IPv4 address and the autodetection matches. GitHub's runners are IPv4,
# so this is a local-only workaround, not a difference in what CI tests.
ACT_NETWORK      ?= bridge

.PHONY: ci-local
ci-local:       ##! Run one job of .github/workflows/ci.yml locally with act (needs Docker; ACT_JOB=go)
	@command -v $(ACT) >/dev/null 2>&1 || { \
	  echo "ci-local: '$(ACT)' not found. Install it (brew install act) or set ACT=<path>." >&2; exit 2; }
	@docker info >/dev/null 2>&1 || { \
	  echo "ci-local: the Docker daemon is not reachable; act needs it to start a runner." >&2; exit 2; }
	@mkdir -p $(ACT_ARTIFACTS)
	$(ACT) -W $(ACT_WORKFLOW) -j $(ACT_JOB) \
	  -P ubuntu-latest=$(ACT_RUNNER_IMAGE) \
	  --network $(ACT_NETWORK) \
	  --artifact-server-path $(ACT_ARTIFACTS)

# ── Combined ──────────────────────────────────────────────────────────────────

.PHONY: test lint

# Two cheap structural gates run FIRST, and both are part of `test` rather than only
# `lint`, because each catches a correctness failure rather than a style one — and
# each has already caught a real one that every other gate passed clean.
#
#   helm-verify-sync   a CRD or webhook declared in config/ but missing from the
#                      chart is simply NOT ENFORCED on a Helm-installed cluster, so
#                      admission rules the Go tests just proved are absent in
#                      production. The same holds for RBAC in both directions: a chart
#                      role granting less than config/rbac is a controller that 403s
#                      only on Helm installs, one granting more is over-privileged.
#                      Renders the chart (needs the helm binary; fails without it);
#                      no cluster.
#
#   check-error-strings  a published file citing the unpublished tracker is a broken
#                      reference for every reader outside this repository, and the
#                      generated CRDs inherit Go doc comments verbatim — so a citation
#                      can reach the public surface from a source file that looks
#                      private. It also gates operator-facing message voice.
#                      It is in `lint-py` too; Make runs it once per invocation.
#
#   docs-cli-check     a `swarmctl` command printed in docs/ that does not resolve
#                      against the CLI is an instruction that fails when an operator
#                      types it. The registry test walks the registry outward; this
#                      is the only check that reads a document inward. Markdown plus
#                      an in-process cobra tree; no cluster, no robot.
#
#   rule-index-check   a rule id cited on the public surface -- in a proto comment, a
#                      CRD description, harness output -- that docs/rule-index.md does
#                      not list is one a reader has nothing to look up in, and an
#                      index that differs from its sources states a rule the
#                      specification does not. check-error-strings reads its rule ids
#                      from that index; this is what keeps the index honest.
#
#   ungraded-obligations-check  RFC-0001's list of control-plane rules no check
#                      grades is generated from a ledger that classifies every
#                      keyword occurrence in scope. A new MUST with no disposition,
#                      or a list that differs from the ledger, fails here; a rule the
#                      reference does not meet cannot render as a row.
#
# All of them fail in about a second, before the long Go run rather than after it.
test: helm-verify-sync check-error-strings rule-index-check ungraded-obligations-check uv-lock-check docs-cli-check test-go test-py   ## Run all tests (structural gates + Go + Python)
lint: lint-go lint-py proto-lint   ## Run all linters (Go + Python)

# ── swarmctl documentation-resolution gate ───────────────────────────────────
# Every `swarmctl …` command printed in a fenced block in docs/ is resolved
# against the real cobra command tree and the real resource registry, and the
# tree itself is pinned. docs/operations.md once printed seven commands in its
# Tasks section of which one existed; nothing in the tree noticed for months,
# because the registry test walks the registry outward and never reads a
# document.
#
# It needs no cluster, no robot and no network — it reads markdown and builds
# the cobra tree in-process, so it runs in about a second and belongs in the
# fast pre-Go set.
#
# It CAN fail, and TestDocGateCanFail asserts that it can: the resolver is fed
# one invocation of each drift class it exists to catch and must reject all of
# them, so a refactor that turns this into a no-op fails here instead of going
# green. A gate that cannot go red is worse than no gate, because it consumes
# the attention a real one would get.
.PHONY: docs-cli-check
docs-cli-check: ##! Fail if a `swarmctl` command printed in docs/ does not resolve against the CLI
	$(GO) test ./cmd/swarmctl/ -count=1 -v \
	  -run 'TestCommandTreeSurface|TestDocumentedInvocationsResolve|TestExampleInvocationsResolve|TestDocGateCanFail'

# ── Rules index ───────────────────────────────────────────────────────────────
# docs/rule-index.md lists every rule id with its normative statement and where the
# specification states it. It is GENERATED -- from the assembled RFC-0001 and the staged
# amendments to it -- and committed, because the amendments are not published and the index
# is how a reader of the published tree looks up a rule a public file cites.
#
# rule-index-check fails, exit 1, when the committed index differs from a fresh generation
# (it prints the diff) or when a public file cites a rule id the index does not list. Where
# the amendment sources are absent it says so, carries their rows over from the committed
# index, and still regenerates and compares every assembled row.
# ── Python lock ───────────────────────────────────────────────────────────────
# uv.lock is committed, so it is a generated artefact like the stubs: a lock that no longer
# describes pyproject.toml gives whoever reproduces the environment from it a different set
# of dependencies than the project declares. uv-lock-check fails when it has drifted. It
# needs uv; a machine without it is refused, not passed.
.PHONY: uv-lock-check
uv-lock-check: ##! Fail if uv.lock no longer matches pyproject.toml (run `uv lock` to update it)
	@command -v uv >/dev/null 2>&1 || { echo "uv-lock-check: uv is not installed, so the lock cannot be checked; install uv (https://docs.astral.sh/uv/)" >&2; exit 2; }
	uv lock --check

.PHONY: rule-index rule-index-check
rule-index: ##! Regenerate docs/rule-index.md from the RFC-0001 sources
	$(PYTHON) scripts/gen-rule-index.py

rule-index-check: ##! Fail if docs/rule-index.md is stale, or a public file cites a rule id it does not list
	$(PYTHON) scripts/gen-rule-index.py --check

# ── Control-plane obligations no check grades ────────────────────────────────
# rfcs/rfc-0001-core-spec/ungraded-obligations.md lists every rule RFC-0001 states with a
# BCP-14 keyword that binds the control plane, that the reference meets and that no
# conformance check grades, with the test (or code) that shows it. It is GENERATED from
# obligations.yaml beside it, a public ledger that gives every keyword occurrence in the
# chapters in scope a disposition (graded, not met, adapter, deployment, ...). Only rules
# the reference meets can render; a Not-met rule has no way into the list.
#
# ungraded-obligations-check fails, exit 1, when the committed chapter differs from a fresh
# generation (it prints the diff) or when a keyword occurrence in scope has no ledger entry,
# so a new MUST cannot land without a disposition. It exits 2 on a ledger entry whose quote,
# keyword, evidence function, note or check id does not resolve. Where the chapter sources
# are absent (the published tree) it checks that every row the assembled RFC-0001 lists
# still names evidence that exists, and says so.
.PHONY: ungraded-obligations-check
ungraded-obligations-check: ##! Fail if that list is stale, or a keyword occurrence in scope has no ledger entry
	$(PYTHON) scripts/gen-ungraded-obligations.py --check


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

docker-build:   ## Build the controller-manager Docker image from the committed Go proto stubs (see Dockerfile)
	docker build --build-arg VERSION=$(VERSION) -t $(IMG) .

docker-push:    ##! Push the controller-manager Docker image
	docker push $(IMG)

docker-build-edge: ##! Build a local, host-arch edge-node image (see Dockerfile.edge); override with EDGE_PLATFORM
	docker buildx build --platform $(EDGE_PLATFORM) -t $(IMG_EDGE) -f Dockerfile.edge --load .

docker-push-edge: ##! Build + push the multi-arch edge-node image (amd64+arm64) directly to the registry
	docker buildx build --platform $(EDGE_PLATFORMS) -t $(IMG_EDGE) -f Dockerfile.edge --push .

# ── Helm chart ────────────────────────────────────────────────────────────────

.PHONY: helm-sync helm-verify-sync helm-lint helm-template

HELM_CHART ?= deploy/swarmada

helm-sync:      ##! Regenerate the chart CRDs + webhooks from config/ (run after `make manifests`)
	bash hack/sync-helm-crds.sh
	$(PYTHON) hack/sync-helm-webhooks.py

helm-verify-sync: ##! Fail if the chart CRDs, webhooks or RBAC have drifted from config/ (CI guard; needs helm)
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
	@HELM_CHART=$(HELM_CHART) $(PYTHON) hack/verify-helm-render.py

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

.PHONY: install uninstall deploy deploy-minimal undeploy undeploy-minimal require-cluster require-cert-manager

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
	# --server-side: config/crd/bases/swarmada.io_computeworkloadtemplates.yaml is ~526KB.
	# Client-side apply stores a full copy of the object in the
	# kubectl.kubernetes.io/last-applied-configuration annotation, which the API server
	# caps at 262144 bytes -- client-side apply of THIS CRD fails deterministically
	# ("metadata.annotations: Too long"), independent of any overlay.
	# Server-side apply tracks field ownership instead of a stored annotation, so it has
	# no such limit. --force-conflicts: safe on a fresh cluster; only matters on a reused
	# one where some other manager already owns a field.
	kubectl apply -k config/crd/bases/ --server-side --force-conflicts

uninstall: require-cluster  ##! Remove CRDs from the cluster
	kubectl delete -k config/crd/bases/ --ignore-not-found

# config/controlstream-tls issues its certificates with cert-manager, and without the
# cert-manager CRDs the apply fails half-way with "no matches for kind Certificate".
require-cert-manager:
	@kubectl get crd certificates.cert-manager.io >/dev/null 2>&1 || { \
	  echo ""; \
	  echo "cert-manager is not installed in this cluster. 'make deploy' needs it: it issues the"; \
	  echo "ControlStream CA, the server certificate and the admission webhook serving certificate."; \
	  echo ""; \
	  echo "  kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml"; \
	  echo "  kubectl -n cert-manager rollout status deploy/cert-manager-webhook"; \
	  echo ""; \
	  echo "  Or deploy without admission webhooks and without ControlStream: make deploy-minimal"; \
	  echo ""; \
	  exit 1; }

deploy: require-cluster require-cert-manager  ## Deploy the control plane with admission webhooks and ControlStream mTLS (config/controlstream-tls; needs cert-manager)
	# --server-side: config/controlstream-tls/ includes config/crd/bases/, which hits the
	# same oversized-CRD/client-side-apply failure as `make install` above.
	kubectl apply -k config/controlstream-tls/ --server-side --force-conflicts
	@echo ""
	@echo "Applied config/controlstream-tls: admission webhooks on, ControlStream on"
	@echo "swarmada-controlstream.system.svc:9445 with mTLS. An adapter needs a client"
	@echo "certificate from Issuer system/controlstream-ca-issuer whose DNS SAN is"
	@echo "<adapter>.<namespace>.svc.cluster.local (see docs/adapters.md)."
	@echo ""
	@echo "The manager Deployment runs image '$(DEPLOY_IMG)' (pinned in"
	@echo "config/default/kustomization.yaml). If the pod reports ImagePullBackOff,"
	@echo "that image is not in the cluster yet:"
	@echo "    make docker-build IMG=$(DEPLOY_IMG)"
	@echo "    kind load docker-image $(DEPLOY_IMG) --name <cluster>   # or: minikube image load $(DEPLOY_IMG)"
	@echo "    kubectl -n system rollout restart deploy/swarmada-controller-manager"
	@echo ""
	@echo "Watch it come up:  kubectl -n system rollout status deploy/swarmada-controller-manager"

deploy-minimal: require-cluster  ##! Deploy the manager with NO admission webhooks and NO ControlStream: it serves no adapters (config/default)
	# --server-side: config/default/ includes config/crd/bases/, which hits the same
	# oversized-CRD/client-side-apply failure as `make install` above.
	kubectl apply -k config/default/ --server-side --force-conflicts
	@echo ""
	@echo "Applied config/default: NO admission webhooks and NO ControlStream. The manager"
	@echo "logs 'NOT serving ControlStream (fail closed)' and no adapter can connect."
	@echo "For a control plane that serves adapters, use 'make deploy'."
	@echo ""
	@echo "Applied. The manager Deployment runs image '$(DEPLOY_IMG)' (pinned in"
	@echo "config/default/kustomization.yaml). If the pod reports ImagePullBackOff,"
	@echo "that image is not in the cluster yet:"
	@echo "    make docker-build IMG=$(DEPLOY_IMG)"
	@echo "    kind load docker-image $(DEPLOY_IMG) --name <cluster>   # or: minikube image load $(DEPLOY_IMG)"
	@echo "    kubectl -n system rollout restart deploy/swarmada-controller-manager"
	@echo ""
	@echo "Watch it come up:  kubectl -n system rollout status deploy/swarmada-controller-manager"

undeploy: require-cluster  ##! Remove what 'make deploy' applied
	kubectl delete -k config/controlstream-tls/ --ignore-not-found

undeploy-minimal: require-cluster  ##! Remove what 'make deploy-minimal' applied
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
	# Webhooks-off posture: leave out the one document that needs webhooks on (its label).
	kubectl apply -f config/samples/ -l 'swarmada.io/sample-posture!=webhooks-on'

# Demo B (camera fault → degrade → reroute → recover) runs via the live
# simulation adapter in the quickstart:  make quickstart SCENARIO=hardware-fault
# (or the human-in-the-loop  make demo SCENARIO=hardware-fault). The old manual
# demo-b-inject/demo-b-recover targets were removed — they wrote Robot.status by
# hand (RA-1 anti-pattern) against a since-changed schema.

# ── Quickstart ─────────────────────────────────────────────────────────────────

.PHONY: demo demo-test quickstart quickstart-test swarmtop first-run

demo:           ## Human-in-the-loop: kind + live sim + swarmtop watching every field flow. Pick a scenario: make demo SCENARIO=hardware-fault (default: full-surface)
	DEMO_LAUNCH_SWARMTOP=1 bash examples/warehouse-quickstart/run.sh --scenario $(if $(SCENARIO),$(SCENARIO),full-surface) --pace off

demo-test:      ## Headless gate (no CI job runs it): full-surface on kind, assert every phase/estop + RFC-0001 #control-plane-observability-prometheus-metrics-contract alerts + RA-1, delete the cluster. Fails on any miss.
	bash examples/full-surface-demo/run.sh

demo-test-walkthrough:  ##! Same gate, narrated: explains each step, what to expect, and how to watch it (Enter between phases; DEMOTEST_NO_PROMPT=1 to auto-continue).
	DEMOTEST_WALKTHROUGH=1 bash examples/full-surface-demo/run.sh

swarmtop:       ##! Build the swarmtop terminal fleet inspector (tools/swarmtop, its own module)
	$(MAKE) -C tools/swarmtop build
	@echo "built tools/swarmtop/bin/swarmtop — run 'make quickstart', then in a 2nd terminal: tools/swarmtop/bin/swarmtop -n warehouse-a"

quickstart:     ## Bring up the warehouse quickstart on kind (needs Docker). Pick a scenario: make quickstart SCENARIO=hardware-fault
	bash examples/warehouse-quickstart/run.sh $(if $(SCENARIO),--scenario $(SCENARIO)) $(if $(ROBOTS),--robots $(ROBOTS))

first-run:      ## Secure first run on a new kind cluster: Helm install with webhooks + ControlStream mTLS, in-cluster sim adapter, admit a robot, run a FleetTask to Succeeded
	bash examples/first-run/run.sh

quickstart-test: ## Run the quickstart end-to-end on kind, assert ✅, then delete the cluster (CI)
	@cluster="$${QUICKSTART_CLUSTER:-swarmada-quickstart}"; \
	trap 'kind delete cluster --name '"$$cluster"' >/dev/null 2>&1 || true' EXIT; \
	bash examples/warehouse-quickstart/run.sh


# ── Spec gates that need no chapter source ─────────────────────────────────────
# These read the Go tree, the contract version and the gate register, so they run
# wherever this Makefile does and are outside the fences above and below.
.PHONY: spec-maturity spec-contract gate-integrity gate-integrity-test

spec-maturity:  ##! Test every "read by no controller" disclosure against the Go tree
	@$(PYTHON) scripts/check-maturity-disclosures.py

# The ninth gate, and the only one that reads the CONTRACT VERSION rather than the spec, the
# Go tree or the demo fixtures. ADR-0032's contract version is written down twice -- as
# `contract.Version` in internal/contract/version.go, which SupportedRange() is derived from,
# and as CONTRACT_VERSION in adapters/conformance/report.py, which the harness stamps into a
# report as the version a result was EARNED against. Drift between them means the harness
# attests a contract the control plane does not implement, and ADR-0032 binds AssignTask
# eligibility to exactly that pair.
#
# version.go's own comment has said "the two MUST move together" since the constant landed, and
# for that whole time nothing ran at the moment of a bump. A Go test asserts the same equality,
# and it t.Skipf's when it cannot read the Python half -- a skipped test is a green test, which
# is the same "exits 0 without running" failure the rule exists to prevent. This gate fails
# CLOSED on a missing file, an unreadable file or an absent constant.
#
#   0  the pair agrees
#   1  they have drifted -- bump the other one
#   2  the check could not run. Never silently 0
spec-contract:  ##! Fail when contract.Version, the harness's CONTRACT_VERSION and the adapter template's disagree
	@$(PYTHON) scripts/check-contract-version-pair.py


# The tenth gate, and the only one whose subject is THE OTHER GATES. Every gate above checks
# a document or the code against something; this one checks that a check RAN.
#
# The class it closes has produced more defects in this repository than any single feature
# area, and every one of them was a checker that was correct, was shipped, and silently did
# nothing: an assembly target commented out; `sys.exit(0 if ok else 0)`; a smoke harness
# reporting ALL ASSERTIONS PASSED for faults it never injected; the boundary guard run against
# the wrong checkout; a demo checker reachable only from comments inside the files it
# validates; a composite target whose first failure hid six verdicts; a breaking-change check
# that prints "skipping" and exits 0; a test that skips when it cannot read the half it exists
# to compare against; per-adapter subtests that skip because the adapter is released
# elsewhere; and a conformance level reporting notApplicable for the checks a rule demands be
# red. Each was caught by an attentive reader and fixed once. THE CATCH RATE DEPENDED ON WHO
# WAS READING.
#
# Five rules, and a register (scripts/gate-integrity-register.txt) that every skip, swallowed
# failure, inert verdict and unwired gate must be named in, with a reason and a count. The
# register is a ratchet: an unlisted site is NEW, a higher count is WORSE, a lower count is
# SHRUNK, and a listed site that no longer exists is REPAIRED. It can only be accurate.
#
# A SKIP IS NOT THE ENEMY. The optional envtest path is real and adapters released from other
# repositories are really absent. The requirement is not zero skips; it is that every skip is
# named, expected and counted, so a NEW one cannot hide inside a green run.
#
# Prove it rather than trusting it: `make gate-integrity-test` plants one instance of each of
# the five shapes in a scratch tree and asserts each goes red. A gate that has only ever seen
# a clean tree has not been tested, which is exactly the defect this one exists to catch.
#
#   0  every rule held
#   1  a finding
#   2  the check could not run -- no register, or a malformed one. Never silently 0
gate-integrity: ##! Fail on a checker that cannot run, cannot fail, or is invoked by nothing
	@$(PYTHON) scripts/check-gate-integrity.py

gate-integrity-test: ##! Prove the gate-integrity checker REFUSES each of the five shapes
	@bash scripts/test-check-gate-integrity.sh


# ── Publication boundary ──────────────────────────────────────────────────────
# The guard's ruleset enumerates precisely what must never be published, so it is not
# itself publishable and lives in the authoring tree. For most of this repository's
# life that meant the guard only ever ran there, against that tree — where it passes.
# That was never evidence about what is published: two deny-severity findings reached
# GitHub in 6e74560 while the guard reported clean, because it read the other tree.
#
# These targets run it against THIS checkout. Point them at the guard once with
#   git config swarmada.boundary-guard /path/to/the/guard/script
#
# check-boundary fails on deny-severity findings, matching the guard's own exit codes.
# check-boundary-strict also fails on warnings, which is the standard for a published
# tree: the guard's warn severity is tuned for the authoring tree, and a gate that
# ignores warnings here is a gate that certifies the defect it was built to catch.

.PHONY: check-boundary check-boundary-strict hooks

check-boundary: ##! Check this checkout against the publication boundary ruleset
	bash scripts/check-publication-boundary.sh

check-boundary-strict: ## Check the publication boundary, warnings fatal (pre-push standard)
	STRICT=1 bash scripts/check-publication-boundary.sh

hooks:          ## Install the git hooks (pre-push publication boundary guard)
	@git config core.hooksPath hack/hooks
	@echo "hooks: core.hooksPath -> hack/hooks (pre-push boundary guard active)"
	@echo "hooks: uninstall with 'git config --unset core.hooksPath'"

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

