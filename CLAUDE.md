# CLAUDE.md

The contributor and agent rules for this repository live in
[`AGENTS.md`](AGENTS.md). Read it before making changes; it is the single source
of truth for build/test commands, generated-code handling, the safety
invariants, and the rules AI-assisted changes must satisfy.

Claude-specific notes:

- Always show the plan or diff and wait for confirmation before modifying an
  existing file. Creating new files is fine.
- After changing `api/v1`, run `make generate manifests`; after any code change,
  run `make test` and `go build ./...` (see [`AGENTS.md`](AGENTS.md) for the
  full list).
- After changing `cmd/swarmctl` **or any `swarmctl` command printed in `docs/`**,
  run `make docs-cli-check`. It resolves every `swarmctl …` invocation in a fenced
  block under `docs/` against the real command tree and resource registry, and
  pins the command tree itself. It needs no cluster and no robot, and runs in
  about a second. `make test` runs it too; the standalone target is for the edit
  loop. If it fails, either the document is stale or the command is missing —
  fix one; do not narrow the extractor to make it pass.
- Sign off commits with `git commit -s` (DCO).
