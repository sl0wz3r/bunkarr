# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Phase 1: backups that replace a nightly rsync job (design: `docs/design/phase1.md`).
- Plex integration (Settings → Plex): server URL and write-only token, connection test, path
  mappings, and "Import from Plex" to add library folders as sources.
- Sources (Library): read-only, incremental scans with hardlink detection (hardlinked names are
  copied and counted once), exclude patterns, a file browser, catalog statistics. A scan refuses
  to change anything when a source looks unmounted (missing, empty, another filesystem, I/O
  errors).
- Destinations: a mounted folder such as a UNAS share over NFS or SMB (`filecopy` engine). The
  share is probed for hardlinks, case sensitivity, characters it rejects and timestamp
  precision, and a marker file makes every job refuse an unmounted or swapped share.
- Syncs with preview (dry run): atomic, verified writes (temp file, hash, fsync, rename), renames
  and moves done at the destination, hardlinks recreated where the share supports them, names
  the share cannot store reported instead of overwriting each other, a free-space check.
- Retention: deleted files and replaced versions are kept (30 days by default) and expired by a
  daily task; a file Bunkarr did not write is never overwritten or deleted. When an upgrade's new
  file cannot be backed up (copy failed or held), the old version in the same folder stays in the
  live backup until the new one is.
- Retention preview: System → Tasks has a Preview (dry run) for every schedule; for the expiry
  task it lists, per destination, the retained files a run would delete, and deletes nothing
  (`POST /api/v1/schedules/{id}/run` with `{"dryRun": true}`).
- Hardlink manifest: every sync writes `.bunkarr/links.tsv` at the destination (each hardlinked
  name, the file holding its content, linked or recorded only), so names that have no file at the
  destination can be recreated in a restore without Bunkarr (README "Restoring").
- Mass-change guard: unusually many deletions or changes, and files that shrink to less than
  half, are held until you click "Apply held changes".
- Adoption of an existing rsync copy (size and modification time, or content hash) instead of
  copying everything again.
- Crash safety: a job interrupted by a crash, kill or restart resumes where it stopped. A file
  with a record is moved into retention only after its record holds the intent, and whatever a
  failed, cancelled or crash-limited job left half done is settled by the next sync, verify or
  retention job (moved back, or recorded), so nothing sits in retention unrecorded.
- A source that has backups can change its path only to the same directory (a renamed mount
  point); a snapshot, clone or copy of it elsewhere is refused.
- Verify jobs (weekly by default) that re-read a sample or all of the backup against the recorded
  hashes and mark damaged or missing files for the next sync.
- Plex database backup while Plex runs: SQLite online backup of the library and blobs databases
  plus `Preferences.xml`, from a read-only mount, verified (integrity check), versioned (14 daily
  and 8 weekly by default), daily at 06:00 by default with a warning when it overlaps Plex's
  maintenance window.
- Schedules for syncs, verify, Plex backups and retention (System → Tasks: edit, disable, Run now).
- Activity: queue with progress, throughput and ETA; history with filters; job details with
  items, the dry-run preview and logs; cancel.
- Apprise notifications (Settings → Connect) for failed jobs, warnings (including held changes)
  and optionally completed jobs; stateless URLs or a stateful config key.
- API for all of the above under `/api/v1` (documented in `openapi.json`).
- Acceptance suite: end-to-end tests of the real binary (`make test-e2e`, sources checked
  unchanged around every job), a container kill test, a Plex backup/restore test against a real
  Plex server that also lists its sections and imports and syncs a library (`make test-plex`),
  and syncs plus kill and resume on Samba (CIFS) and NFS shares (`make test-shares`); all in
  `make test-docker`. A release is tagged only after all of them pass (the Docker tests against
  the release image itself).
- docker-compose example: host paths and PUID/PGID/TZ come from `deploy/.env` and are required
  (Compose refuses to start without them instead of creating empty folders), `/config` on an
  absolute appdata path outside the checkout, Plex data directory mount (read-only), `rw,slave`
  propagation for shares mounted after Docker starts, a stop grace period.
- Docs: the Plex DB backup's `Preferences.xml` holds the Plex token in cleartext, and an SMB
  share may not enforce its 0600 mode (README "Restoring").

- Phase 0 foundation.
- Go server (`cmd/bunkarr`) with `serve`, `version`, `healthcheck` and `reset-auth` commands,
  graceful shutdown and a single-instance lock on the config directory.
- SQLite database (pure Go, WAL mode, separate writer and read-only pools) with embedded,
  versioned migrations; refuses a database written by a newer version.
- Settings store with secrets sealed at rest (AES-256-GCM, key derived with HKDF from a generated
  master key in `/config/bunkarr.key`, bound to the setting name).
- Authentication: first-run setup of the one UI user (bcrypt), forms login with server-side
  sessions, API key (`X-Api-Key` or `?apikey=`, generated on first run, regenerable), optional
  "disabled for local addresses", login rate limiting, cross-origin (CSRF) protection, change of
  username/password that ends other sessions.
- API: `GET /api/v1/health`, `GET /api/v1/system/status`, auth endpoints, Settings > General, and
  `GET /api/v1/openapi.json` (checked against the router by a test).
- JSON logs in `/config/logs` with size rotation plus stdout; secrets redacted from both.
- Web UI shell (React, TypeScript, Vite, Tailwind): login and first-run setup pages, *arr-style
  left navigation (Activity, Library, Destinations, Settings, System), Settings > General (API
  key, authentication mode, login) and System > Status.
- Multi-arch Docker image (alpine, tini, su-exec, restic, rclone) with PUID/PGID/UMASK/TZ,
  healthcheck and an image smoke test; docker-compose example.
- CI on GitHub and Gitea (typecheck, tests, race tests, gofmt, vet, govulncheck, npm audit,
  shellcheck, image build and test); release workflow pushing multi-arch images to GHCR on tags.
