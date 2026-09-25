# Phase 1 design — replace the rsync job

Status: contract for implementation (2026-09-24). Every Phase 1 package is built against this
document. Changing a contract here means updating every package that uses it.

Goal (spec §8 Phase 1): Plex integration, incremental catalog with hardlink detection, `filecopy`
engine to a mounted destination (UniFi UNAS share), scheduled + manual syncs with dry run, Plex DB
+ Preferences backup (versioned, retention), Activity/History/job logs, Apprise notifications.

Acceptance:
1. Initial full sync of a test library; a second run copies only changes.
2. Hardlinked files are copied once; reported size equals unique bytes.
3. Killing the container mid-job and restarting resumes without corrupting the destination.
4. Plex DB backup passes `integrity_check` and restores into a scratch Plex container in a test.

## 1. Safety rules (non-negotiable, tested)

- **S1 Sources are read-only.** Source files are only ever opened `O_RDONLY`; no chmod, chown,
  utimes, rename or unlink on anything under a source path. Scanner and copier never write there.
- **S2 Destinations are fenced.** Bunkarr only writes under a destination's `target`, and only
  creates/renames/deletes paths it manages: the live tree `<target>/<destFolder>/…`, its own temp
  files (`.bunkarr-tmp-*`), and `<target>/.bunkarr/…`. It never deletes a file it has no
  `destination_files` / `snapshots` record for.
- **S3 Marker check.** Creating a destination writes `<target>/.bunkarr/destination.json`
  (`{"id": "<uuid>", "name": …, "createdAt": …}`); the uuid is stored in `destinations.marker_id`.
  Every job that writes to a destination first verifies the marker exists and matches. A missing
  marker means "share not mounted" (an unmounted UNAS share leaves an empty directory in the
  container) and **fails the job** without writing anything.
- **S4 No overlap.** A destination target may not equal, contain, or be inside any source path,
  `/config`, or `/`; a source may not be inside a destination. Enforced on create/update of both.
- **S5 Deletes are not propagated.** A file that disappears from a source is moved (rename, same
  filesystem) into the destination's retention area and deleted only after `retention.deletedDays`
  (default 30).
- **S6 Upgrades.** When a source file changes (size or mtime differ), the new version is copied to
  a temp file and verified **before** the old destination file is moved into retention; then the
  temp file is renamed into place.
- **S7 Atomic writes.** Data is written to `<dir>/.bunkarr-tmp-<name>-<rand>`, fsynced, size
  checked (and hash checked per verify mode), mtime set to the source mtime, then renamed over the
  final name and the directory fsynced. A crash leaves at most a temp file, never a partial file
  under a final name. Temp paths are recorded in `job_items.detail` before creation so a resumed
  job removes them.
- **S8 Secrets** (Plex token, Apprise URLs) are sealed with the keyring (ADR 0003), never logged,
  never returned by the API (responses carry `hasApiKey: true` instead).
- **S9 Dry run.** Every sync and every Plex DB backup can run with `dryRun: true`: it scans and
  plans, persists the plan as job items, and changes nothing at the destination.

## 2. Destination layout (filecopy)

```
<target>/
  .bunkarr/destination.json                         marker (S3)
  .bunkarr/retention/<UTC yyyymmddThhmmssZ>-job<id>/<destFolder>/<relPath>   retained files (S5/S6)
  .bunkarr/plex/<integration-slug>/<UTC yyyymmddThhmmssZ>/                  Plex DB versions
      com.plexapp.plugins.library.db
      com.plexapp.plugins.library.blobs.db        (when present)
      Preferences.xml
      manifest.json   {createdAt, method, files:[{name,size,sha256}], integrity, plexVersion}
  <destFolder>/<relPath>                            live mirror of each source
```

`destFolder` is per source (default: a slug of the source name, e.g. `movies`); two sources synced
to the same destination may not share a destFolder. Mirroring an existing rsync layout: set the
destFolder to the existing folder name; existing files whose size and mtime match the source are
**adopted** (recorded, not copied) — so switching from rsync does not re-copy the library.

## 3. Schema (migration 0002_phase1.sql)

See `internal/db/migrations/0002_phase1.sql` — it is the source of truth. Tables: `integrations`,
`sources`, `destinations`, `destination_sources`, `schedules`, `catalog_files`,
`destination_files`, `jobs`, `job_items`, `job_logs`, `snapshots`, `notifications`.

Conventions: times are text in `db.TimeFormat` (UTC); JSON columns are text; sealed secrets use
AAD `integration:<id>:apiKey` and `notification:<id>:urls` (insert the row, then seal with its id,
in one transaction).

## 4. Packages and ownership

Each package owns its tables' SQL. Other packages use its exported API, never its tables directly
(exception: `internal/api` may read through the owning package's list functions only).

| Package | Owns | Exposes (minimum) |
|---|---|---|
| `internal/integrations` | `integrations` | `Store`: `List`, `Get`, `Create`, `Update`, `Delete`, `Secret(ctx,id)` (unsealed token). Types `Integration`, `PlexSettings{DataPath string; PathMappings []PathMapping}`. |
| `internal/integrations/plex` | — | `Client` (`New(url, token, *http.Client)`), `Identity(ctx)`, `Sections(ctx)`, `Test(ctx)`. `testdata/` recorded responses; `plextest` fake server. |
| `internal/catalog` | `sources`, `catalog_files` | `Store` (sources CRUD, `Files(ctx, sourceID, query)`, `Stats(ctx)`), `Scanner.Scan(ctx, sourceID, reporter) (ScanResult, error)`, `LiveFiles(ctx, sourceIDs) iterator`, `HardlinkGroups`. Per-source scan lock. |
| `internal/destinations` | `destinations`, `destination_sources` | `Store` CRUD incl. source links, `Settings`, `Retention` types, `ValidateTarget`, `InitMarker`, `CheckMarker`. |
| `internal/engines/filecopy` | — (filesystem only) | `CopyFile`, `LinkFile`, `RetainFile`, `ExpireFile`, `ProbeHardlinks`, `FreeSpace`, `HashFile`, `CleanupTemp`, `AdoptMatch`. Pure functions over paths; no DB. |
| `internal/syncer` | `destination_files` | Runners for `sync`, `verify`, `retention` jobs; `Planner` (diff catalog vs destination files → items). |
| `internal/plexdb` | `snapshots` | Runner for `plexdb_backup`; `Backup` (online backup API or Plex's scheduled backup, per ADR 0005), `Verify`, retention of versions. |
| `internal/notify` | `notifications` | `Store` CRUD, `Apprise` client, `Dispatcher` hooked to `jobs.Manager.OnFinish`, `Test`. |
| `internal/jobs` | `jobs`, `job_items`, `job_logs`, `schedules` | `Manager` (queue, workers, resume, cancel, progress), `Scheduler` (cron), `Store` (list/get/items/logs), implements the contract in `internal/jobs/contract.go`. |
| `internal/api` | — | Handlers for §6, `openapi.json`, wiring in `cmd/bunkarr`. |
| `web/` | — | UI for §7. |

New dependency: `github.com/robfig/cron/v3` (scheduler, named in the spec's stack).

## 5. Jobs contract

`internal/jobs/contract.go` (already written) defines `Type`, `Status`, `Trigger`, `Params`, `Job`,
`Progress`, `Result`, `Item`, `ItemAction`, `ItemStatus`, `Reporter`, `ItemStore`, `Runner`.

Semantics every runner must honour:
- **Idempotent and resumable.** After a crash the manager re-queues every `running` job with
  `Attempt+1`, `Trigger=resume`. A runner that already persisted its plan (`ItemStore.HasItems`)
  executes only items still `pending`, re-checking each item against the filesystem first (the
  source may have changed; the copy may have completed but not been recorded — adopt it).
- **Cancellation** is `ctx` cancellation: stop promptly, remove own temp files, return `ctx.Err()`.
- **Fatal vs item errors.** Destination-wide failures (marker missing, `ENOSPC`, target
  unreachable/`EIO`, permission denied on the target root) return an error → job `failed`.
  Per-file failures (source file vanished, unreadable file) mark the item `failed`, log a warning,
  and continue → job `completed_with_warnings`.
- **Progress** via `Reporter.Progress` as often as convenient (the manager throttles persistence
  and computes throughput/ETA). Logs via `Reporter.Log` (persisted to `job_logs`, redacted).
- **Stats** in `Result.Stats` (JSON object); for sync: `filesPlanned, filesCopied, filesUpdated,
  filesAdopted, filesLinked, filesRetained, filesExpired, filesFailed, bytesCopied (unique),
  bytesPlanned (unique), durationMs`.

Manager rules: global worker limit 2 (setting `jobs.workers`); at most one job per destination
and one scan per source at a time; enqueueing a job identical (type, params, dryRun) to one still
`queued` returns the queued one. On graceful shutdown running jobs are cancelled and left
`running`, so the next start resumes them. Max 3 attempts, then `failed`.

## 6. API (all under `/api/v1`, auth required, JSON camelCase, sizes in bytes)

Integrations
- `GET /integrations` → `[Integration]`; `POST /integrations` → 201 `Integration`
- `GET|PUT|DELETE /integrations/{id}` (PUT with empty/missing `apiKey` keeps the stored one)
- `POST /integrations/test` `{type, url, apiKey?, id?}` → `{ok, message, version?, machineIdentifier?}`
- `GET /integrations/{id}/plex/sections` → `[{key, title, type, locations:[{path, localPath, exists}]}]`
- `POST /integrations/{id}/plex/backup` `{destinationId, dryRun}` → 202 `Job`

`Integration = {id, type, name, url, enabled, hasApiKey, settings, createdAt, updatedAt}`;
Plex `settings = {dataPath, pathMappings:[{plex, local}], backup:{destinationId, cron, enabled, keepVersions}}`.

Sources and catalog
- `GET /sources` → `[Source]`; `POST /sources` → 201; `GET|PUT|DELETE /sources/{id}`
- `POST /sources/{id}/scan` → 202 `Job`
- `GET /sources/{id}/files?page&pageSize&search` → `{page, pageSize, total, items:[CatalogFile]}`
- `GET /catalog/stats` → `{sources, files, bytes, uniqueBytes, hardlinkGroups, hardlinkedFiles}`
- `GET /filesystem?path=` → `{path, parent, directories:[{name, path}]}` (read-only path picker)

`Source = {id, name, path, destFolder, exclude:[glob], enabled, plexIntegrationId, plexSectionId,
plexPath, lastScanAt, stats:{files, bytes, uniqueBytes}}`;
`CatalogFile = {id, relPath, size, mtime, hardlinkGroup, nlink, deleted}`.

Destinations
- `GET /destinations`, `POST /destinations` (writes the marker) → 201, `GET|PUT|DELETE /destinations/{id}`
  (DELETE never removes backup data)
- `POST /destinations/{id}/test` → `{ok, marker, writable, hardlinks, freeBytes, totalBytes, entries, message}`
- `POST /destinations/{id}/sync` `{dryRun}` → 202 `Job`; `POST /destinations/{id}/verify` → 202 `Job`
- `GET /destinations/{id}/snapshots` → `[Snapshot]`

`Destination = {id, name, engine:"filecopy", target, enabled, sourceIds, schedule:{cron, enabled},
settings:{verify:{mode:"off"|"sample"|"full", samplePercent}, hardlinks:"recreate"|"copy",
adoptExisting}, retention:{deletedDays, plexDbVersions}, lastJob, createdAt, updatedAt}`.

Jobs and schedules
- `GET /jobs?state=active|finished&type&page&pageSize` → `{page, pageSize, total, items:[Job]}`
- `GET /jobs/{id}`, `POST /jobs/{id}/cancel`
- `GET /jobs/{id}/items?action&status&page&pageSize` → paged `[Item]`
- `GET /jobs/{id}/logs?afterId&limit` → `[{id, at, level, message, fields}]`
- `GET /schedules` → `[{id, jobType, params, description, cron, enabled, lastRunAt, nextRunAt}]`
- `PUT /schedules/{id}` `{cron, enabled}`; `POST /schedules/{id}/run` → 202 `Job`

`Job = {id, type, status, trigger, dryRun, params, attempt, progress:{phase, filesTotal,
filesDone, bytesTotal, bytesDone, currentFile, bytesPerSec, etaSeconds}, stats, warnings, error,
summary, queuedAt, startedAt, finishedAt}`.

Notifications (Settings → Connect)
- `GET|POST /notifications`, `PUT|DELETE /notifications/{id}`, `POST /notifications/test`
`Notification = {id, name, kind:"apprise", enabled, apiUrl, configKey, hasUrls, onFailure,
onWarning, onSuccess}`; the Apprise URLs are write-only (`urls` on create/update).
Apprise API: stateless `POST {apiUrl}/notify` `{urls, title, body, type}` or stateful
`POST {apiUrl}/notify/{configKey}` `{title, body, type}`.

Every route must be in `internal/api/openapi.json` (`TestOpenAPIMatchesRoutes`).

## 7. UI

- Activity → **Queue** (`/activity/queue`): active jobs with progress bar, bytes, throughput, ETA,
  current file, cancel. Polls every 2 s.
- Activity → **History** (`/activity/history`): finished jobs, filters (type, status), paging.
- **Job detail** (`/activity/jobs/:id`): summary, stats, items (filter by action/status; this is
  the dry-run preview), logs (auto-refresh while running).
- **Library** (`/library`): sources with stats, add/edit/delete, "Import from Plex" (sections →
  locations → local path via path mappings), Scan; `/library/sources/:id` file browser.
- **Destinations** (`/destinations`): list with last sync status; add/edit form (path picker,
  sources, schedule presets + cron, verify mode, hardlinks, retention); Test, Preview (dry run),
  Sync now, Verify.
- Settings → **Plex** (`/settings/plex`): servers (URL, token, data path, path mappings), Test,
  DB backup (destination, schedule, versions to keep), Back up now, snapshot list.
- Settings → **Connect** (`/settings/connect`): Apprise targets, events, Test.
- System → **Tasks** (`/system/tasks`): schedules with next/last run, enable, edit cron, Run now.

## 8. Tests required

- Unit: hardlink grouping, incremental scan (add/change/delete/rename), planner diff (every
  action), retention math, cron validation, redaction of tokens, marker check, overlap validation.
- Filesystem: atomic copy leaves no partial final file on error; ENOSPC → error and temp removed;
  hardlink recreation and fallback; adopt on size+mtime match; retention move and expiry fence.
- Jobs: resume after simulated crash (`running` row at start), cancellation, dedupe, per-destination
  serialization, max attempts.
- Plex client against recorded responses in `testdata/`.
- E2E (build tag `e2e`): the real binary against a fixture media tree with hardlinks: full sync,
  incremental sync copies only changes, kill -9 mid-copy + restart resumes and the destination
  verifies, unmounted destination fails without writing, dry run writes nothing.
- Plex DB: online backup of a live database under write load; integrity check; restore into a
  scratch Plex container (optional slow suite, Docker).
