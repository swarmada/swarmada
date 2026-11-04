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
# The external event bridge. A DEMO image.
#
#   docker build -f examples/event-bridge/Dockerfile -t swarmada-event-bridge:dev .
#
# Built from the repository root. Dockerfile.dockerignore beside this file narrows
# the context to the module files, api/ and the example.
# ─────────────────────────────────────────────────────────────────────────────

FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY examples/event-bridge/ examples/event-bridge/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o event-bridge ./examples/event-bridge

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/event-bridge /event-bridge
USER 65532:65532
ENTRYPOINT ["/event-bridge"]
CMD ["serve"]
