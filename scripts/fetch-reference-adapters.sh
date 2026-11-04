#!/usr/bin/env bash
# Copyright 2026 The Swarmada Authors. All rights reserved.

#
# Clones (or updates) the own-repo reference Fleet Adapters — ROS 2, VDA5050,
# MAVLink — into a local, gitignored workspace so they can be developed
# side-by-side with core Swarmada without ever being committed to this
# repository (docs/adr/0005-reference-adapter-policy.md: these are separate
# repos under the org, never in-tree).
#
# This is deliberately NOT a git submodule: a submodule adds a .gitmodules
# entry and a gitlink that DO get committed here, which is the opposite of
# what this script is for. Each cloned repo stays a fully independent git
# repo (own .git/, own remote, own commits); adapters/external/ is listed in
# .gitignore and never shows up in `git status`.
#
# Usage:
#   scripts/fetch-reference-adapters.sh [org] [dest]
#
# If a repo doesn't exist yet under the org (not yet created), this script
# creates an empty local directory instead so it can be scaffolded from
# adapters/template/ (the vendor cookiecutter) and pushed up later.

set -euo pipefail

ORG="${1:-swarmada}"
DEST="${2:-adapters/external}"

REPOS=(
  fleet-adapter-ros2
  fleet-adapter-vda5050
  fleet-adapter-mavlink
)

mkdir -p "$DEST"

for repo in "${REPOS[@]}"; do
  path="$DEST/$repo"
  url="https://github.com/${ORG}/${repo}.git"

  if [ -d "$path/.git" ]; then
    echo "→ updating $repo"
    git -C "$path" pull --ff-only
  elif git ls-remote "$url" >/dev/null 2>&1; then
    echo "→ cloning $repo"
    git clone "$url" "$path"
  else
    echo "→ $repo not found at $url yet — creating an empty local dir to scaffold from adapters/template/"
    mkdir -p "$path"
  fi
done

echo
echo "Reference adapters are in $DEST/ — gitignored, never committed to swarmada."
echo "See adapters/template/ and adapters/CONFORMANCE.md to build one."
