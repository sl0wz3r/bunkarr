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
- [ADR 0009](0009-unraid-community-applications.md) adds `docs/ca/` to the excluded paths and a
  gate before the term scan: the export's Unraid template must be current and valid
  (`make ca-validate`), or nothing is published.
- [ADR 0010](0010-release-publishing.md) adds a second gate before the term scan: the deploy
  tests pass on the export (`go test ./deploy/`, without `.gitea/`). A `v*` tag is created on the
  public repository, annotated, only when that repository lacks it; a tag it already has is never
  moved or pushed again.
- `.gitea/workflows` and `.github/workflows` run the same CI; Gitea ignores `.github/workflows`
  while `.gitea/workflows` exists. The release workflows differ (ADR 0010): the public one
  publishes the image and the GitHub release, the Gitea one only tests unless publishing is
  switched on there.

## Consequences

- Public history is linear and one commit per sync; it does not share hashes with Gitea.
- A leak found by the scan blocks publishing until it is fixed in the private repository.
