# 0009. Unraid Community Applications template

- Status: accepted
- Date: 2026-09-28
- Amends: [ADR 0004](0004-private-gitea-public-github.md) (a new excluded path and a template gate
  in the public export)
- Amended by: [ADR 0010](0010-release-publishing.md) (automatic GitHub releases; questions in
  GitHub Discussions)

## Context

Bunkarr is built for Unraid servers (the compose example's values are Unraid's), and Unraid users
install containers from Community Applications (CA). CA lists a template from a public GitHub
repository with an OSI-approved license and a `ca_profile.xml` at its root; the image must be
pullable by anyone. CA reads the listed repository's default branch at every feed build, and it
can blacklist a whole repository, "first and ask questions later", when the repository or account
is renamed or transferred, when `ExtraParams` or `PostArgs` would run a command, or for data-loss
bugs. Installed containers never receive template changes: Unraid keeps each container's saved
template.

Bunkarr's only deployment so far is `deploy/docker-compose.yml`, which requires every host path
and the server name (`:?` guards) so that Docker never creates folders at guessed paths, sets a
stable host name for restic's lock checks (`docs/design/phase4.md` §6.7) and gives a clean stop
60 seconds. dockerMan differs from Compose in ways that matter here: it creates missing host paths
on **Apply**, has no host name field, stops every container with one global timeout (10 seconds by
default) and ignores the container's own, and injects `TZ` into every container.

Every commit on `main` reaches the public repository through the post-commit hook before CI
reports (ADR 0004), so once Bunkarr is listed, a broken template would reach CA within hours.

## Decision

- **Layout.** The template lives in this repository: `unraid/bunkarr.xml`, `ca_profile.xml` at the
  root, and the icon `unraid/icon.png`. The public owner is `sl0wz3r`, the repository `bunkarr`,
  the branch `main`, the image `ghcr.io/sl0wz3r/bunkarr:latest`. No separate templates repository:
  the template's version and `<Changes>` belong to Bunkarr's releases.
- **Generated, from one settings file.** `unraid/ca/render.sh` renders both files from
  `unraid/ca/*.tmpl` and `unraid/ca/publish.env` (owner, repository, registry, support link,
  version and date); the rendered files are committed. `unraid/ca/validate-template.sh` checks CA's
  rules and Bunkarr's own offline (`make ca-validate`, in CI), and `unraid/ca/preflight.sh` checks
  the live repository, raw URLs, icon and image anonymously (`make ca-preflight`). The tooling is
  adapted from the maintainer's DupeArr, which is listed in CA. Every file is written public-clean
  with literal public URLs: the export has no markers or rewrites.
- **The template mirrors the compose example** (`deploy/unraid_test.go` fails on drift): the image
  and container name, port 8787, every mount with its mode (`/media`, `/plex` and the *arr Backups
  folders read-only, `/backup` `rw,slave`), `PUID`/`PGID` 99/100 and `UMASK` 022, the host name and
  the stop timeout. It differs where Unraid differs:
  - no `TZ` entry: Unraid injects the server's, and an empty template entry would reset it to UTC;
  - the host name goes into `ExtraParams` as `--hostname=bunkarr-tower` (the user replaces
    `tower` with the server's name), and `stop_grace_period` as `--stop-timeout=60`. Unraid's own
    stops ignore the latter, so the `Requires` text and `unraid/README.md` tell users to raise
    Settings → Docker → Docker Stop Timeout to 60;
  - `/plex` is optional (compose requires it, but says to delete the line without the Plex DB
    backup); the *arr Backups folders are optional, as the commented-out compose lines are;
  - a `BUNKARR_LOG_LEVEL` variable, for support requests.
- **Host paths are empty except `/config`** (`/mnt/user/appdata/bunkarr`). dockerMan creates a
  missing host path, so a guessed default would create an empty share, a folder inside another
  app's appdata, or, for an unmounted `/mnt/remotes` share, a folder in RAM.
- **`/media` and `/backup` are required**, like compose's guards. `/backup` stays required for
  off-site-only setups too (its description says they map a folder that Bunkarr then leaves
  unused): a mounted share is the primary destination, and requiring it keeps a user from starting
  with nothing to back up to. The tests pin `Required="true"` on every mount compose always makes,
  except the allow-listed `/plex`.
- **The Overview states the limits.** It says that Bunkarr never modifies or deletes the sources
  and mounts them read-only, that it is a Beta, that the restore wizard is not built yet (restores
  and re-acquiring manifest-only content are manual), and that `bunkarr.key` must be backed up with
  the database. The validator fails without these, since CA's data-loss policy applies to a
  backup tool above all.
- **No capability hardening in `ExtraParams` yet** (`--cap-drop=ALL` and the entrypoint's
  capabilities): no Docker test runs Bunkarr, restic, rclone and the entrypoint under those flags
  ([DEFERRED.md](../../DEFERRED.md)).
- **Images.** `:latest` follows full releases only; a pre-release (`vX.Y.Z-rc.N`) gets only its
  own tag, which lets a release candidate be installed on a real Unraid server before `:latest`
  exists. There is no LAN variant and no registry-free (`docker save`) path.
- **Support is GitHub Issues** until a forum support thread exists (`SUPPORT_URL`, `FORUM_URL`);
  issue forms ask for what an Unraid report needs. *Amended by ADR 0010:* questions and setup help
  go to GitHub Discussions (Q&A); `<Support>` stays the Issues page, whose chooser links Q&A.
- **GitHub releases are made by hand**; `release.yml` publishes the image only. *Amended by
  ADR 0010:* `release.yml` publishes the GitHub release itself, with attestations and SBOMs.
- **Never rename** the repository or the account, and never move `unraid/bunkarr.xml`, once the
  app is listed.
- **Amends ADR 0004:** the public export also leaves out `docs/ca/` (the maintainer's submission
  checklist), and before its term scan `scripts/public/sync.sh` runs `make ca-validate` on the
  exported tree (scratch files outside it) and fails closed when the rendered files are stale or
  break a rule.

## Consequences

- A template or settings change means editing `unraid/ca/`, `make ca-template ca-profile` and
  committing the sources with the rendered files; CI and the export refuse anything else.
- Every release updates `CHANGELOG.md`, `<Changes>`, `VERSION` and `RELEASE_DATE` together
  (`deploy/unraid_changes_test.go`, from the first release on). The release commit is live in CA
  before `release.yml` has pushed the image, so a failed release is reverted at once (all four
  together) or fixed forward with the next patch version, since its pushed tag is never reused
  (CONTRIBUTING.md, Releases).
- Installed containers keep their saved template: a new setting reaches existing users only
  through `<Changes>`, the release notes and the docs.
- The template cannot enforce the host name or the stop timeout: users can clear Extra Parameters
  and must raise the Docker Stop Timeout by hand. The docs say so, and Bunkarr warns at start-up
  when its host name looks like a container ID.
- The repository, the account and the template path are fixed from the listing on.
- The template changes none of the pre-release gates in [DEFERRED.md](../../DEFERRED.md) (the
  real UNAS and B2 passes, trusted proxies, the first-run setup token); they are decided before
  the submission.
