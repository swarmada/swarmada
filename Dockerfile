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

# The builder runs on the build machine's own platform and cross-compiles for the target
# (GOOS/GOARCH below): no emulation is needed for a multi-platform build, since the runtime
# stage runs nothing.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS builder
ARG TARGETOS
ARG TARGETARCH

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

# The control-plane version, linked into every audit entry (swarmada_version) and the
# swarmada_version metric. `make docker-build` passes the chart's appVersion (X.Y.Z, no "v");
# a plain `docker build` stamps "dev". Declared here, after the module download, so a new
# version reuses the cached download layer.
ARG VERSION=dev

# Static, CGO-free binary for the distroless/static base.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.buildVersion=${VERSION} -X github.com/swarmada/swarmada/internal/metrics.BuildVersion=${VERSION}" -o manager ./cmd/manager

# ─────────────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
# OCI labels. The release workflow passes the version and the commit; a plain build labels
# "dev" and an empty revision.
ARG VERSION=dev
ARG REVISION=
LABEL org.opencontainers.image.title="swarmada" \
      org.opencontainers.image.description="Swarmada control-plane manager" \
      org.opencontainers.image.source="https://github.com/swarmada/swarmada" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
WORKDIR /
COPY --from=builder /workspace/manager /manager
USER 65532:65532

ENTRYPOINT ["/manager"]
