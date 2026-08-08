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
#  swarmtop — local dev Makefile
#  Own Go module (see go.mod) so a later `git filter-repo` split is mechanical.
#  Cross-platform releases are handled by .goreleaser.yaml, not this file.
# ══════════════════════════════════════════════════════════════════════════════

GO ?= go
KUBECONFIG ?= $(HOME)/.kube/config

.PHONY: build run test lint clean help

help:            ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n",$$1,$$2}'

build:           ## Compile the local-OS/arch binary to bin/swarmtop
	$(GO) build -o bin/swarmtop ./cmd/swarmtop

run: build       ## Build and run against $(KUBECONFIG)
	./bin/swarmtop --kubeconfig $(KUBECONFIG)

test:            ## Run the Go test suite
	$(GO) test ./...

lint:            ## Lint with golangci-lint (install: https://golangci-lint.run)
	golangci-lint run

clean:           ## Remove build artifacts
	rm -rf bin/
