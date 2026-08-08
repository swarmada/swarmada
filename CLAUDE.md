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
- Sign off commits with `git commit -s` (DCO).
