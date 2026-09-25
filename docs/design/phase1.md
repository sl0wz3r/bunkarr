# Phase 1 design — replace the rsync job

Status: contract for implementation, revision 2 (2026-09-25), after an adversarial design review
(data safety, crash/resume, Unraid/UNAS storage, spec completeness) and the Plex DB spike
(docs/spikes/0001-plex-db-backup.md, ADR 0005). Every Phase 1 package is built against this
document; changing a contract here means updating every package that uses it.

Goal (spec §8 Phase 1): Plex integration, incremental catalog with hardlink detection, `filecopy`
engine to a mounted destination (UniFi UNAS share over SMB or NFS), scheduled + manual syncs with
dry run, Plex DB + Preferences backup (versioned, retention), Activity/History/job logs, Apprise
notifications.

Acceptance (exact tests in §9):
1. Initial full sync of a test library; a second run copies only changes.
2. Hardlinked files are copied once; the reported size (`Source.stats.uniqueBytes`, sync
   `bytesPlanned`) equals the unique bytes.
3. Killing the process/container mid-job and restarting resumes; the destination then verifies
   (every live file present with the source's size and hash) and holds no partial file under a
   final name.
4. The Plex DB backup passes Bunkarr's verification (ADR 0005: quick_check + filtered
   integrity_check) **and** `Plex SQLite ... "PRAGMA integrity_check"` prints `ok`, and the
   backup restores into a scratch Plex container that then serves the library.

## 1. Safety rules (non-negotiable, each has tests)

- **S1 Sources are read-only.** Sources are opened through `os.Root` on the source path; files are
  opened `O_RDONLY|O_NOFOLLOW`. No chmod/chown/utimes/rename/unlink under a source, ever.
  Symlinks are never followed: the scanner uses lstat (WalkDir), counts symlinks and other
  non-regular files as `skipped` (reported, not copied). A source root that is itself a symlink is
  resolved once when the source is saved (the resolved path is stored).
- **S2 Destinations are fenced.** Every destination operation of a job goes through one `os.Root`
  opened on the target at job start (no path can escape it; if the share unmounts mid-job the open
  handle returns errors instead of writing into the empty mountpoint). Bunkarr creates, renames
  and deletes only: the live tree `<destFolder>/…`, its temp files `.bunkarr-tmp-*`, and
  `.bunkarr/…`. It never deletes or overwrites a file it has no record for: an **unmanaged** file
  at a path Bunkarr needs is either adopted (§4.4) or **displaced** into retention, never renamed
  over.
- **S3 The destination is the right filesystem.**
  - Create requires the target to exist already (Bunkarr never creates the target or its parents),
    probes it (§3), and refuses when: `.bunkarr/destination.json` already exists (use
    `attach: true` to adopt that marker's id instead); the filesystem is tmpfs/ramfs/overlay/
    rootfs, or its `st_dev` equals that of `/` or of the config directory — unless
    `allowLocal: true` (a local disk destination is legitimate but must be a deliberate choice).
  - Create writes the marker (`{"id","name","createdAt"}`) and records `marker_id`, `fs_type`
    (statfs `f_type`) and `root_dev`.
  - Every job touching a destination (sync, verify, retention, plexdb_backup, including dry runs)
    first checks: marker present and matching, `f_type` equal to the recorded one. Mismatch or
    missing marker → the job fails with "destination not mounted?" and writes nothing. The marker
    is re-checked through the job's `os.Root` every 500 files and before retention/expiry work.
- **S4 No overlap.** On save, resolved (EvalSymlinks) paths: a destination target may not equal,
  contain or be inside any source, the config directory or `/`; a source may not be inside a
  destination. At scan time the scanner also skips (and warns about) any directory whose
  `(dev, ino)` equals a destination root or the config directory (bind-mount aliases).
- **S5 Deletes are not propagated.** A file that disappears from a source is moved (same-filesystem
  rename) into retention and deleted only after `retention.deletedDays` (≥ 1, default 30). Content
  that is still referenced by a live name is never expired (§4.3 promotion).
- **S6 New before old.** Within a sync, every copy/update/move/link item runs before any retain
  item (so a Radarr upgrade that renames `X-1080p.mkv` → `X-2160p.mkv` backs up the new file
  before the old one goes to retention). An update copies and verifies the new version to a temp
  file before the old version moves to retention (§4.2).
- **S7 Atomic, verified writes.** Temp file `<dir>/.bunkarr-tmp-<base>-<rand>` in the final
  directory (path recorded in the item detail **before** creation) → copy while hashing (when
  verify ≠ off) → fsync → size check against the bytes read and against an `fstat` of the source
  taken before and after the copy (a source changed during the copy fails the item: "changed
  during copy", retried next run) → optional re-read verification → chtimes to the source mtime →
  rename → fsync directory. A crash leaves at most a temp file, never a partial final file.
- **S8 Secrets** (Plex token, Apprise URLs) are sealed (ADR 0003), never returned by the API
  (`hasApiKey`/`hasUrls` instead), sent only in headers/bodies — never in URLs (Plex:
  `X-Plex-Token` header only). Errors from HTTP clients are wrapped so no URL query or header
  appears in them. `internal/logging` additionally redacts registered secret **values** anywhere
  in log/job-log messages and attributes (`logging.RegisterSecret`).
- **S9 Dry run.** `dryRun: true` scans, plans, runs the guards, persists the plan as items and
  changes nothing at the destination (not even the marker check writes). Its items are the preview.
- **S10 Mass-change guard.** (a) A scan is **fatal** — the catalog is left untouched — when the
  source root is missing, not a directory, on a different filesystem than recorded (`fs_type`,
  `root_dev`; a deliberate change is accepted by editing and re-saving the source), empty while
  the catalog has live files, or when any `ENOTCONN`/`EIO`/`ESTALE` occurs. An unreadable
  subdirectory (EACCES) is a warning and its subtree keeps its catalog rows unchanged (no
  deletions). Deletions (`deleted_at`) are committed only after the whole walk finished.
  (b) Before executing, a sync counts its planned `retain` + `update` items per source. If they
  exceed `sync.maxChangePercent` (default 10 %) of the source's live files **and** 20 files, or
  exceed `sync.maxChangeFiles` (default 1000), those items are **held** (status `held`, nothing
  moved), everything else runs, the job ends `completed_with_warnings`, and a warning
  notification says so. An update whose new size is 0 or less than half the old size is always
  held (truncation/ransomware). Held changes run on the next sync started with
  `allowChanges: true` (UI: "Apply held changes"). Dry runs report what would be held.
- **S11 Names the destination cannot store.** The destination probe (§3) records whether it is
  case-insensitive and which characters it rejects. The planner fails (warning, not fatal) any
  item whose name the destination cannot store or whose case-folded path collides with another
  live path of the same destination, and never lets one overwrite the other. The UI shows these;
  docs recommend NFS or SMB with POSIX extensions / `mapposix` for such libraries.

## 2. Destination layout (filecopy)

```
<target>/
  .bunkarr/destination.json                          marker (S3)
  .bunkarr/probe/                                    capability probes (§3), emptied after use
  .bunkarr/retention/<jobQueuedAt yyyymmddThhmmssZ>-job<id>/<destFolder>/<relPath>
  .bunkarr/plex/<integration-slug>/<yyyymmddThhmmssZ>/         Plex DB version (complete)
  .bunkarr/plex/<integration-slug>/.partial-job<id>/          being written (renamed when done)
      com.plexapp.plugins.library.db
      com.plexapp.plugins.library.blobs.db   (when present)
      Preferences.xml                          (sensitive: contains PlexOnlineToken)
      manifest.json  {createdAt, method, plexVersion, files:[{name,size,sha256}], integrity:{...}}
  <destFolder>/<relPath>                              live mirror of each source
```

The retention directory name uses the job's `queued_at`, so a resumed job reuses it.
`destFolder`: per source, default slug of the source name; unique across sources (DB constraint);
immutable once any destination holds files for the source (API 409). Mirroring an existing rsync
tree: set destFolder to the existing folder name (§4.4 adoption).

## 3. Destination capability probe

Run on create (required) and on `POST /destinations/{id}/test`; stored in
`destinations.capabilities` (JSON) and used by the planner:
`{hardlinks: bool, caseInsensitive: bool, invalidChars: "<chars>", trailingDotSpace: bool,
mtimeGranularityNs: int, fsType: "cifs|smb2|nfs|ext4|...", checkedAt}`. Probes run inside
`.bunkarr/probe/` and clean up after themselves: hardlink (`link`), case (`create A`, `stat a`),
each of `: ? * " < > | \`, trailing dot/space, and mtime granularity (chtimes to …123456789 ns
and read back). `test` also returns free/total bytes (statfs), marker status, whether the target
is writable, and the number of entries at the top level.

## 4. Sync semantics

### 4.1 Flow of a sync job
1. **Preflight**: destination enabled, S3 checks, open `os.Root`, sources linked and enabled.
2. **Scan** each linked source (incremental; same code as a scan job; per-source lock). S10(a).
3. **Plan** (skipped on resume if `jobs.planned_at` is set): diff the catalog against
   `destination_files` (the records, not the destination) and emit items (§4.2). Items are
   persisted in batches; the final batch and `planned_at` are written in one transaction
   (`ItemStore.AddItems(..., final=true)`). A job with items but no `planned_at` (killed while
   planning) deletes its items and plans again.
4. **Guards**: S10(b) holds; S11 name checks; free-space check: bytes of copy + update + (new
   side of) move items that are not adopted must fit in `free − 1 GiB`, else the job fails before
   copying anything (dry run: reported).
5. **Execute** pending items in this order: `promote`, `move`, `copy`/`update`/`adopt`, `link`,
   `retain`. `expire` runs only in retention jobs.
6. **Finish**: stats, summary, warnings.

### 4.2 Items (the planner's actions)
Identity of a source file within a scan is `(source, relPath)` plus `(size, mtimeNs)`; content is
"unchanged" iff size and mtime (compared at the destination's `mtimeGranularityNs`) are equal to
the `destination_files` record. `(dev, ino)` is only used inside one scan (§4.3).

- **copy** — live catalog file, no live record. Before writing, if the path exists at the
  destination unrecorded: adopt it if it matches (§4.4), else displace it into retention (record
  it as a retained row with `detail.reason = "displaced"`) and copy.
- **update** — live record whose size or mtime differs. Sequence: temp complete and verified →
  if hardlinks are supported: `link(final → retentionPath)` then `rename(temp → final)` (no gap);
  else `rename(final → retentionPath)` then `rename(temp → final)` (a resume finishes a half-done
  pair from the item detail). Then the record is updated and a retained row is added.
- **move** — a live catalog path with no record whose size and mtimeNs exactly equal a live
  record whose source path vanished in this scan **and** whose content is confirmed the same
  (same `(dev, ino)` as the vanished path had in this scan's pre-walk snapshot, or equal sha256 of
  the first and last 1 MiB of the source file and the destination file). Executes as a
  same-filesystem rename at the destination (cheap; covers Radarr/Sonarr "Rename files" and folder
  reorganisations). Otherwise the pair is copy + retain.
- **link** — another name of a hardlink group (§4.3) whose primary is copied/present. With
  `capabilities.hardlinks` and `settings.hardlinks = "recreate"`: `link(primaryDest, dest)`,
  state `linked`. Otherwise: no file, state `link_recorded` (spec: copy once, record the
  relationship). Runs only after the primary's item is `done`; if the primary's item failed or was
  held, the link item becomes a copy.
- **retain** — live record whose source path vanished (and is not a move). Rename into retention;
  record becomes `retained` with `expires_at = now + deletedDays`.
- **promote** — a record about to be retained (or updated) that live `link_recorded` rows depend
  on: rename its file to the first surviving dependent's path, make that row `present`, re-point
  the other dependents; the vanished name's record is then removed (its content lives on under
  the dependent). In `linked` mode no promotion is needed (the inode survives through the other
  names).
- **adopt** — unrecorded destination file matching the source (§4.4): record only.
- **expire** (retention job) — retained row past `expires_at`: delete the retained file (only
  inside `.bunkarr/retention/`, only if its size still equals the record), delete the row, prune
  empty retention directories.

Every item is **re-checked at execution** (and on resume): source re-stat; destination state; if
the desired end state already exists (e.g. the rename happened but the DB write did not), the
item records it and is marked done. The order of effects is always: filesystem → destination_files
→ `ItemStore.Finish`.

### 4.3 Hardlinks
- Groups are computed **within one scan only**: names with equal `(dev, ino, size, mtimeNs,
  ctimeNs)` and `nlink ≥ 2`. A group with more members than `nlink`, or disagreeing members, is
  split into single files with a warning. The group id stored in `catalog_files.hardlink_group`
  is a per-scan surrogate (`"<scanId>:<n>"`) and is never matched against earlier scans.
- On FUSE sources (Unraid `/mnt/user`, statfs magic `0x65735546`) inode numbers are not stable
  across time and only identify hardlinks when Unraid's "Tunable (support Hard Links)" is on: the
  source test warns about this; grouping additionally requires equal sha256 of the first and last
  1 MiB. Docs recommend mounting a pool/disk path (`/mnt/cache/data`, `/mnt/diskN/data`) or the
  share with hard-link support enabled.
- Unique bytes: a group counts once (`uniqueBytes`); `bytes` counts every name.
- Execution re-stats both names and requires equal `(dev, ino, size, mtime)`; otherwise the link
  item becomes a copy.
- Group split (an *arr replaced one name with a new inode): the remaining names are no longer in
  the same group; a name whose destination state is `link_recorded`/`linked` to a primary whose
  content changes gets its own copy before the primary's update (in recreate mode the existing
  hardlink already holds the right old content: its record becomes `present`).

### 4.4 Adoption (switching from rsync)
With `settings.adoptExisting` (default `"size+mtime"`), an unrecorded destination file at the
planned path is adopted when its size equals the source and its mtime equals the source's at the
destination's granularity, within `settings.mtimeWindowSec` (default 0; like rsync
`--modify-window`; set 1–2 for FAT/older SMB servers). `"size+hash"` additionally or instead
compares full sha256 of both sides (slow, one-time) and, on a match, sets the destination mtime.
`"off"`: never adopt (unrecorded files are displaced). A resumed item whose final file exists
unrecorded with the item's recorded temp hash/size is adopted the same way.

### 4.5 Verify job
Walks all live records: stat existence and size (cheap); re-reads and hashes a sample
(`samplePercent`, default 5 %; `full` = all) and compares with the recorded hash (files copied
with verify off get their hash recorded on first verify). Before each re-read the page cache is
dropped where possible (`posix_fadvise(DONTNEED)`, Linux). Missing, short or mismatching files
fail the item, mark the record `missing` (the next sync copies it again) and make the job
`completed_with_warnings`; a notification is sent. Default schedule: weekly (created with the
destination, editable).

## 5. Plex DB backup (ADR 0005)
- Source: `<dataPath>/Plug-in Support/Databases/com.plexapp.plugins.library.db` (+ blobs db) and
  `<dataPath>/Preferences.xml`. `dataPath` is the Plex "Plex Media Server" directory mounted into
  Bunkarr, **read-only** by default. Bunkarr's PUID must be able to read `Preferences.xml` (0600,
  owned by Plex's user).
- Method: `file:<db>?mode=ro` (never read-write, never `query_only` alone, never `immutable` while
  a `-wal` exists) + `busy_timeout`; `sql.Conn.Raw` → `NewBackup(staging)` → one `Step(-1)`.
  No `-wal` (Plex stopped cleanly): `mode=ro&immutable=1` with a size/mtime/inode guard before and
  after; retry up to 3 times.
- Staging: `<config>/staging/plexdb-job<id>/`, then verify there, then copy with the filecopy
  engine (S7) into `.bunkarr/plex/<slug>/.partial-job<id>/`, write `manifest.json`, rename the
  directory to its timestamp, record the snapshot. A resumed job removes its own partial
  directory and staging and starts the backup again (it is minutes, not hours).
- Verification (copy opened `mode=ro&immutable=1`): stub binary collations for every non-built-in
  collation in `sqlite_master`, `quick_check = ok`, `integrity_check(100000000)` ignoring only
  `row N missing from index X` for indexes using a custom collation; counts of `metadata_items`
  and `media_parts`. Result in `snapshots.integrity` (`ok`/`failed`) and the manifest.
- Versions: keep the newest `plexDbDaily` (default 14) versions with integrity `ok`, plus the
  newest version of each of the last `plexDbWeekly` (default 8) ISO weeks; never delete the newest
  `ok` version; failed versions are kept 7 days for diagnosis. Pruning runs only after a
  successful backup.
- Default schedule 06:00 daily (outside Plex's butler window, default 02–05; the UI warns on
  overlap using `GET /:/prefs` `ButlerStartHour/ButlerEndHour`).
- A plexdb_backup job does not wait behind a sync of the same destination (different lock key).

## 6. Jobs
### 6.1 Contract
`internal/jobs/contract.go` defines the types; `internal/jobqueue` implements the manager. Every runner is idempotent and resumable: a job
re-run after a crash has `Attempt > 1`, `Trigger = resume`; a planned job executes only pending
items, re-checking each (§4.2). Cancellation = ctx cancellation: stop promptly, remove own temp
files, return `ctx.Err()`. Fatal (destination-wide: S3/S10(a) failures, `ENOSPC`, `EIO`/
`ENOTCONN`/`ESTALE` on the destination, permission denied on the target root) → error → `failed`.
Per-file (vanished, unreadable, changed during copy, name not storable) → item `failed`, warning,
continue → `completed_with_warnings`. Progress via `Reporter.Progress`; logs via `Reporter.Log`.
Stats for sync: `filesPlanned, filesCopied, filesUpdated, filesMoved, filesAdopted, filesLinked,
filesRetained, filesDisplaced, filesHeld, filesFailed, filesSkipped, bytesPlanned (unique),
bytesCopied (unique), durationMs`.

### 6.2 Manager
- Lock keys: sync/verify/retention → `dest:<id>`; plexdb_backup → `plexdb:<integrationId>`;
  scan → `source:<id>` (a sync also takes `source:<id>` for its sources while scanning). A job
  starts when a worker is free (setting `jobs.workers`, default 2) and its keys are free; FIFO
  otherwise, but a queued job whose keys are free may overtake a blocked one.
- The seeded global retention schedule enqueues a retention job with no destination; that runner
  enqueues one retention job per enabled destination and completes.
- Enqueue dedupe: an identical queued job (type, params, dry run) is returned instead of a new
  one. A scheduled sync for a destination whose sync is already running is skipped (logged).
- Graceful shutdown (SIGTERM): cancel running jobs, wait up to 20 s, set them `queued` with
  trigger `resume` **without** incrementing `attempt`. Start-up: rows still `running` crashed →
  `queued`, `resume`, `attempt + 1`; attempt > 3 → `failed` ("crashed 3 times").
- Progress persisted at most every 2 s; throughput is an EWMA over 10 s; ETA from remaining bytes.
- Job history retention: finished jobs older than 90 days (setting) are deleted by the retention
  job (items and logs cascade).
- `OnFinish(func(Job))` hooks (notifications).

## 7. API (all under `/api/v1`, auth required, JSON camelCase, sizes in bytes)

Integrations
- `GET /integrations` → `[Integration]`; `POST /integrations` → 201 `Integration`
- `GET|PUT|DELETE /integrations/{id}` (PUT with empty/missing `apiKey` keeps the stored one)
- `POST /integrations/test` `{type, url, apiKey?, id?}` → `{ok, message, version?, machineIdentifier?}`
- `GET /integrations/{id}/plex/sections` → `[{key, title, type, locations:[{path, localPath, exists}]}]`
- `POST /integrations/{id}/plex/backup` `{destinationId, dryRun}` → 202 `Job`

`Integration = {id, type, name, url, enabled, hasApiKey, settings, createdAt, updatedAt}`; Plex
`settings = {dataPath, pathMappings:[{plex, local}], backup:{destinationId, cron, enabled}}`.

Sources and catalog
- `GET /sources` → `[Source]`; `POST /sources` → 201; `GET|PUT|DELETE /sources/{id}`
- `POST /sources/test` `{path}` → `{ok, exists, isDir, fsType, fuse, entries, message, warnings:[...]}`
- `POST /sources/{id}/scan` → 202 `Job`
- `GET /sources/{id}/files?page&pageSize&search&filter=all|hardlinked|deleted` → paged `CatalogFile`
- `GET /catalog/stats` → `{sources, files, bytes, uniqueBytes, hardlinkGroups, hardlinkedFiles}`
- `GET /filesystem?path=` → `{path, parent, directories:[{name, path}]}` (read-only picker)

`Source = {id, name, path, destFolder, exclude:[glob], enabled, plexIntegrationId, plexSectionId,
plexPath, fsType, lastScanAt, lastScanStatus, stats:{files, bytes, uniqueBytes, hardlinkGroups,
skipped}}`; `CatalogFile = {id, relPath, size, mtime, hardlinkGroup, nlink, deleted}`.

Destinations
- `GET /destinations`, `POST /destinations` `{..., attach?, allowLocal?}` → 201,
  `GET|PUT|DELETE /destinations/{id}` (DELETE never removes backup data)
- `POST /destinations/test` `{target}` (before create) and `POST /destinations/{id}/test` →
  `{ok, marker:"ok|missing|mismatch|foreign", writable, fsType, local, capabilities, freeBytes,
  totalBytes, entries, message, warnings:[...]}`
- `POST /destinations/{id}/sync` `{dryRun, allowChanges}` → 202 `Job`;
  `POST /destinations/{id}/verify` → 202 `Job`
- `GET /destinations/{id}/snapshots` → `[Snapshot]`

`Destination = {id, name, engine:"filecopy", target, enabled, sourceIds, fsType, capabilities,
schedule:{cron, enabled}, verifySchedule:{cron, enabled}, settings:{verify:{mode:"off"|"sample"|
"full", samplePercent}, hardlinks:"recreate"|"copy", adoptExisting:"size+mtime"|"size+hash"|
"off", mtimeWindowSec, maxChangePercent, maxChangeFiles}, retention:{deletedDays, plexDbDaily,
plexDbWeekly}, lastJob, createdAt, updatedAt}`.
`Snapshot = {id, integrationId, path, createdAt, size, method, integrity, manifest}`.

Jobs and schedules
- `GET /jobs?state=active|finished&type&status&page&pageSize` → paged `Job`
- `GET /jobs/{id}`, `POST /jobs/{id}/cancel`
- `GET /jobs/{id}/items?action&status&page&pageSize` → paged `Item`;
  `GET /jobs/{id}/items/summary` → `[{action, status, files, bytes}]`
- `GET /jobs/{id}/logs?afterId&limit` → `[{id, at, level, message, fields}]`
- `GET /schedules` → `[{id, jobType, params, description, cron, enabled, lastRunAt, nextRunAt}]`
- `PUT /schedules/{id}` `{cron, enabled}`; `POST /schedules/{id}/run` → 202 `Job`

`Job` as in `contract.go` (progress includes `bytesPerSec`, `etaSeconds`).

Notifications (Settings → Connect)
- `GET|POST /notifications`, `PUT|DELETE /notifications/{id}`, `POST /notifications/test`
`Notification = {id, name, kind:"apprise", enabled, apiUrl, configKey, hasUrls, onFailure,
onWarning, onSuccess}`; `urls` is write-only. Apprise API: stateless `POST {apiUrl}/notify`
`{urls, title, body, type}` or stateful `POST {apiUrl}/notify/{configKey}` `{title, body, type}`;
`type` ∈ `info|success|warning|failure`. Sent for: job failed, completed with warnings (incl.
held changes, verify mismatches), optionally completed. Not for dry runs unless they fail.

Every route is in `internal/api/openapi.json` (`TestOpenAPIMatchesRoutes`).

## 8. Packages and ownership

Each package owns its tables' SQL; other packages use its exported API.

| Package | Owns | Exposes (minimum) |
|---|---|---|
| `internal/integrations` | `integrations` | `Store` (List/Get/Create/Update/Delete, `Token(ctx,id)`), `PlexSettings`, `MapPath`. |
| `internal/integrations/plex` | — | `Client` (`Identity`, `Sections`, `Prefs`), `plextest` fake server, `testdata/` (recorded). |
| `internal/catalog` | `sources`, `catalog_files` | `Store` (sources CRUD + validation, files query, stats), `Scanner`, `TestSource`, per-source lock, `Live(ctx, sourceID)` iterator. |
| `internal/destinations` | `destinations`, `destination_sources` | `Store` CRUD + validation, `Probe`, `Create` (marker), `Open(ctx, id) (*Handle, error)` = checks S3 and returns the `os.Root`. |
| `internal/engines/filecopy` | — | Filesystem primitives over `*os.Root`: copy (S7), link, rename, displace/retain, expire, adopt check, head/tail hash, full hash, fadvise, temp cleanup, free space. |
| `internal/syncer` | `destination_files` | `Planner`, runners `sync`, `verify`, `retention`; guards S10(b), S11. |
| `internal/plexdb` | `snapshots` | runner `plexdb_backup`, `Backup`, `Verify`, version pruning. |
| `internal/notify` | `notifications` | `Store`, Apprise client, `Dispatcher` (`jobs.Manager.OnFinish`), `Test`. |
| `internal/jobs` | — | The contract only (`contract.go`): types and interfaces shared by runners. Stable; no implementation. |
| `internal/jobqueue` | `jobs`, `job_items`, `job_logs`, `schedules` | `Manager`, `Scheduler` (robfig/cron), `Store`; implements the `jobs` contract. |
| `internal/logging` | — | + `RegisterSecret(value)`, `RedactSecrets(text)` value-based redaction (done). |
| `internal/faultinject` | — | `Point(name)`, `SetHook`, `CrashAt`, `InitFromEnv` (done). |
| `internal/api` + `cmd/bunkarr` | — | Handlers (§7), openapi.json, wiring. |
| `web/` | — | UI (§10). |

## 9. Tests (required)

Unit (every package): hardlink grouping incl. disagreeing members and inode reuse; incremental
scan (add/change/delete/rename, unreadable subdir, EIO → fatal, empty root → fatal); planner for
every action incl. move vs copy+retain, promote, link fallback, displaced unmanaged files, case
collision, invalid names, guard holding; retention math incl. defaults; cron validation; value
redaction; marker/fs-type checks; overlap validation; Plex client against `testdata/`.

Crash matrix (Go, every push, no Docker): the filecopy, syncer, plexdb and jobs code call
`faultinject.Point("name")` at each step boundary (`internal/faultinject`: a single atomic load in
production; tests install `faultinject.CrashAt(point, n)`, which panics with `faultinject.Crash`). A table test runs a sync against a fixture tree, "crashes" at each point
(panic recovered by the harness, process state discarded), re-runs the job as a resume, and
asserts: the destination verifies, no partial final file, no temp file left, records match the
destination. Points include planning after the first batch, after temp write, after rename,
between the two renames of an update, before the DB record, during retain, during promote.

E2E (build tag `e2e`, real binary, fixture tree with hardlinks under `testdata/` generated at
test time): (1) full sync then a second sync with 1 changed, 1 added, 1 deleted, 1 renamed file:
second job stats `filesCopied=1, filesUpdated=1, filesMoved=1, filesRetained=1` and bytes equal to
those files; (2) hardlinked pair: copied once, `bytesPlanned`/`uniqueBytes` equal unique bytes;
(3) kill -9 while paused at `copy.afterWrite` (env `BUNKARR_FAULTPOINT=copy.afterWrite`,
`BUNKARR_FAULTPOINT_FILE=<path>`: the process writes the file and blocks at that point), restart, the job resumes and the destination
verifies; (4) destination marker removed → sync fails and writes nothing; (5) empty source →
scan fails, nothing retained; (6) dry run writes nothing.

Docker suite (`make test-docker`, run locally and in a CI job with Docker): image smoke test; the
container kill test (same fault point via env `BUNKARR_FAULTPOINT` in a `faultinject` image);
Plex restore test per the spike procedure (pinned `plexinc/pms-docker` digest, backup from an
`:ro` mount under load, Plex SQLite integrity `ok`, restore into a second container, sections and
counts match).

## 10. UI
- Activity → **Queue** (`/activity/queue`): active jobs, progress bar, bytes, throughput, ETA,
  current file, cancel; polls every 2 s.
- Activity → **History** (`/activity/history`): finished jobs, filters (type, status), paging.
- **Job detail** (`/activity/jobs/:id`): summary, stats, item summary by action/status, items
  (filters; this is the dry-run preview; held items with "Apply held changes"), logs.
- **Library** (`/library`): sources with stats, add/edit (path picker + test), delete,
  "Import from Plex" (sections → locations → local path via path mappings), Scan;
  `/library/sources/:id` file browser.
- **Destinations** (`/destinations`): list with last sync status; add/edit (path picker, test
  with capability/warnings display, attach/local confirmations, sources, schedules, verify mode,
  hardlinks, adoption, guard limits, retention); Test, Preview (dry run), Sync now, Verify,
  snapshots.
- Settings → **Plex** (`/settings/plex`): servers (URL, token, data path, path mappings), Test,
  DB backup (destination, schedule), Back up now.
- Settings → **Connect** (`/settings/connect`): Apprise targets, events, Test.
- System → **Tasks** (`/system/tasks`): schedules with next/last run, enable, edit cron, Run now.

## 11. Deployment notes (docs)
- Mount the media parent read-only as ONE volume (hardlink detection); prefer a pool/disk path or
  enable Unraid's hard-link support tunable for `/mnt/user`.
- Unassigned Devices shares are mounted after Docker starts: map them with `rw,slave` propagation
  (`/mnt/remotes/UNAS:/backup:rw,slave`), otherwise the container sees an empty mountpoint.
- Plex appdata: `/mnt/user/appdata/plex/Library/Application Support/Plex Media Server:/plex:ro`;
  PUID must match Plex's user (Preferences.xml is 0600).
- SMB without POSIX extensions is case-insensitive and rejects `: ? * " < > |`; NFS avoids both.
