# 0004. Private Gitea, public GitHub

- Status: accepted
- Date: 2026-09-24

## Context

Development happens on a private Gitea on the maintainer's LAN; the public home (issues, GHCR
images, the future Unraid CA listing) is GitHub. Private history may mention LAN hosts, account
names and home paths.

## Decision

- The Gitea repository is the source of truth (`origin`). GitHub is never a remote of it.
- `scripts/public/sync.sh` publishes a commit: it exports the tree at that commit, leaves out
  private-only paths (`.gitea/`, `scripts/public/`), fails closed if any private term appears,
  and commits the result as one new commit on the public branch with the original message
  (trailers dropped). `v*` tags on the commit are mirrored and trigger the release workflow.
- The Go module path is the public one (`github.com/sl0wz3r/bunkarr`), so no rewriting is needed.
- `.gitea/workflows` and `.github/workflows` run the same CI; Gitea ignores `.github/workflows`
  while `.gitea/workflows` exists.

## Consequences

- Public history is linear and one commit per sync; it does not share hashes with Gitea.
- A leak found by the scan blocks publishing until it is fixed in the private repository.
