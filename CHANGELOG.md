# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Phase 3: backup tiers (design: `docs/design/phase2-3.md` revision 4, decisions in ADR 0007).
- Tiers (Settings → Tiers): each destination holds a file as `full` (copied), `manifest` (not
  copied, listed in the manifests) or `skip`. Ordered rules with all/any conditions, an action
  and "Applies at" (all or chosen destinations), between a built-in "Irreplaceable → full" first
  row and an "Everything else → full" last row. **With no rules every file is full**, and a sync
  plans exactly what it did before. Presets (including the manifest-by-default set) only fill the
  editor; saving checks the rule revision (a stale save is refused).
- Conditions: `arr.managed`, `arr.monitored`, `arr.qualityProfile`, `arr.rootFolder`, `arr.tag`,
  `media.genre`, `file.age`, `file.size`, `flag.irreplaceable`, `plex.section`,
  `tautulli.playCount`, `tautulli.lastWatched`, `seerr.requested`, `seerr.requestedBy` and
  `maintainerr.pendingDelete`. A fact Bunkarr cannot know (not set up, stale cache, conflicting
  evidence, unmapped folder, deleted *arr) is unknown and never lowers protection: a more
  protective rule that is unknown wins, and the fallback is full.
- Preview ("what gets backed up and why"): per destination, files and bytes that are stored, full,
  manifest, skip, unknown, to copy, kept and moved to non-full, with each file's rule and
  structured reasons. Sync dry runs carry the tier and reasons on every item; job items can be
  summarised and filtered by tier (`GET /jobs/{id}/items?by=tier`, `tier`, `ruleId`).
- Demotion never removes a backup: a file that stops being full is kept (not updated, repaired or
  retained) until you release it. A release is a dry run followed by "Apply release", which
  releases only the files that dry run listed, still not full and at the same rule revision; they
  go into retention with reason `released`.
- Irreplaceable flags on a file or folder (Library item view, Settings → Tiers): always full
  everywhere, following renames and moves; retention never expires a flagged file.
- Library item view: a file's tier and reasons per destination, its facts and which are unknown,
  and its flags (`GET /catalog/files/{id}`).
- Tautulli (2.18+), Seerr and Maintainerr (3.4+) connections (Settings → Connect): read-only,
  allow-listed requests, each linked to one Plex server, with a test, scheduled refreshes and
  freshness. The Plex library index (sections, items, files) is turned on per Plex integration.
  Maintainerr's pending deletions follow the server's version (ADR 0007).
- Copies made only because a fact is unknown count as changes for the mass-change guard and are
  held, not failed, when free space runs short.
- Tests: recorded fixtures from real Tautulli 2.18.1, Seerr 3.4.1 and Maintainerr 3.4.1 and
  3.29.0; the pinned tier table end to end; `TestDockerArrTiers` against real Radarr; an upgrade
  test that runs the real Phase 2 binary and this one on the same database.

- Phase 2: Sonarr, Radarr and Lidarr awareness (design: `docs/design/phase2-3.md`, decisions in
  ADR 0006).
- *arr connections (Settings → Connect): URL, write-only API key, path mappings and a connection
  test. Bunkarr keeps an index of each app's items, files, quality profiles, root folders and tags,
  refreshed at start-up and on a schedule. A refresh that would remove unusually much is held until
  "Apply held changes".
- Webhooks: each *arr posts its events to its own URL with a per-integration webhook key (sent as
  the HTTP Basic password, recommended; "Regenerate key" on the Connect card). An import or upgrade
  is backed up within about a minute by a sync of just that item's folder, and a burst of events
  becomes one refresh and one sync per destination. A file deleted in an *arr stays in the live
  backup until the next full sync, where the mass-change guard sees it, then moves into retention.
  Missed webhooks are caught up by the full refresh.
- *arr backups: each app's own backup zip, read from its `Backups` folder mounted read-only (works
  with Forms login) or downloaded over HTTP. A recent scheduled backup is reused; otherwise Bunkarr
  asks the *arr to make one. Every zip is verified (entry CRCs, database check) and versioned at the
  destination (weekly by default; 14 daily and 8 weekly versions kept). The zips hold the *arr's
  secrets: they are stored unsealed with mode 0600, and never served or logged.
- Manifests: `manifest.json` and `manifest.csv` under `.bunkarr/manifests/` at each destination
  list every *arr's items, ids, profiles, root folders and backed-up files, so a library can be
  rebuilt without the *arr. Written after a full sync when an *arr is connected, versioned (30
  daily and 12 weekly by default), downloadable or exported on demand from Destinations →
  Manifests.
- Sign in with Plex (Settings → Plex): sign in at plex.tv, pick a server, see each connection
  tested, and save. The saved token is that server's own; tokens never reach the browser, and an
  unencrypted connection needs "Use anyway". The URL + token path still works.
- Activity and notifications for the new jobs (Refresh, *arr backup, Manifest export). Webhook
  syncs and refreshes notify about warnings and failures at most once per destination or
  integration per 24 h; held changes always notify.
- Upgrade safety: before migrating its database, Bunkarr saves a copy under `/config/backups`. To
  downgrade, stop the container, restore that copy as `/config/bunkarr.db` and run the older
  image.
- `make test-arr`: acceptance tests against real Sonarr, Radarr and Lidarr containers.

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

### Changed

- `*.partial~` and `*.backup~` (the *arrs' temporary copy names) are excluded by default, so stale
  temp files from the *arrs are no longer backed up.
- The Plex, *arr and Apprise clients refuse link-local and cloud metadata addresses.
- A running webhook sync no longer makes a scheduled sync or verify skip its turn: the scheduled
  job waits for it. A scheduled run is skipped only while an untargeted job of its type runs on
  the same destination and covers all of its sources.
- `Cross-Origin-Opener-Policy` is `same-origin-allow-popups`, so Bunkarr can close the plex.tv
  sign-in popup.
