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
# Swarmada controller-manager image.
# Multi-stage: Go 1.26 builder -> distroless static (nonroot) runtime.
# Built by `make docker-build` (IMG defaults to ghcr.io/swarmada/swarmada:latest).
# ─────────────────────────────────────────────────────────────────────────────

FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
# The control-plane version, linked into every audit entry (swarmada_version) and the
# swarmada_version metric. `make docker-build` passes the chart's appVersion (X.Y.Z, no "v");
# a plain `docker build` stamps "dev".
ARG VERSION=dev

WORKDIR /workspace

# Resolve dependencies first so the module layer caches independently of source.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy what ./cmd/manager compiles. The generated proto stubs (proto/**/*.pb.go)
# ARE imported transitively (internal/controlstream -> proto/fleet_adapter/v1).
# They are committed, so a clean checkout's build context already carries them
# and a bare `docker build` works. After editing a .proto, run `make proto` and
# commit the regenerated stubs; the image builds whatever proto/ holds.
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
COPY proto/ proto/

# Static, CGO-free binary for the distroless/static base.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION} -X github.com/swarmada/swarmada/internal/metrics.BuildVersion=${VERSION}" -o manager ./cmd/manager

# ─────────────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager /manager
USER 65532:65532

ENTRYPOINT ["/manager"]
