# Phase 1 design — replace the rsync job

Status: implemented, revision 4 (2026-09-25). Revision 2 was the contract for implementation,
written after an adversarial design review (data safety, crash/resume, Unraid/UNAS storage, spec
completeness) and the Plex DB spike (docs/spikes/0001-plex-db-backup.md, ADR 0005). Revision 3
folded in what the implementation changed: deviations the package authors made that are now the
real behaviour, and the fixes from the code review. Revision 4 adds the fixes from the final
verification: retention intents and their reconciliation (§4.2), the hardlink manifest (§2), the
retain hold after a failed upgrade copy (S6), the same-root rules for a source's path (§2), the
token read together with its URL (S8), the retention preview (S9, §7) and the share tests (§9).
Every Phase 1 package is built against this document; changing a contract here means updating
every package that uses it.

Goal (spec §8 Phase 1): Plex integration, incremental catalog with hardlink detection, `filecopy`
engine to a mounted destination (UniFi UNAS share over SMB or NFS), scheduled + manual syncs with
dry run, Plex DB + Preferences backup (versioned, retention), Activity/History/job logs, Apprise
notifications.

Acceptance (exact tests in §9; all pass):
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
  opened `O_RDONLY|O_NOFOLLOW` and checked to be the file the lstat saw (same dev/inode), so a
  symlink swapped in is never followed. No chmod/chown/utimes/rename/unlink under a source, ever.
  Symlinks are never followed: the scanner uses lstat, re-checks device and inode when it enters
  each directory, and counts symlinks and other non-regular files as `skipped` (reported, not
  copied). A source root that is itself a symlink is resolved once when the source is saved (the
  resolved path is stored).
- **S2 Destinations are fenced.** Every destination operation of a job goes through one `os.Root`
  opened on the target at job start (`destinations.Open`; no path can escape it; if the share
  unmounts mid-job the open handle returns errors instead of writing into the empty mountpoint).
  Bunkarr creates, renames and deletes only: the live tree `<destFolder>/…`, its temp files
  `.bunkarr-tmp-*`, and `.bunkarr/…`; no tree operation accepts `.bunkarr` itself or the marker
  as its target. It never deletes or overwrites a file it has no record for: an **unmanaged**
  file at a path Bunkarr needs is either adopted (§4.4) or **displaced** into retention, never
  renamed over. (A directory symlink planted inside the target is followed while it stays inside
  it; see DEFERRED.md.)
- **S3 The destination is the right filesystem.**
  - Create requires the target to exist already (Bunkarr never creates the target or its parents),
    probes it (§3), and refuses when: `.bunkarr/destination.json` already exists (use
    `attach: true` to adopt that marker's id instead; attach on a target without a marker writes
    a new one); the target overlaps the config directory, `/` or another destination's target;
    the filesystem is tmpfs/ramfs/overlay/rootfs, or its `st_dev` equals that of `/` or of the
    config directory — unless `allowLocal: true` (a local disk destination is legitimate but must
    be a deliberate choice). Create calls are serialized.
  - Create writes the marker (`{"id","name","createdAt"}`) first, then the row (the marker is
    removed if the insert fails), and records `marker_id` (UNIQUE: one row per marker, so a share
    cannot be added twice under two paths), `fs_type` (statfs `f_type`) and `root_dev`.
  - Every job touching a destination (sync, verify, retention, plexdb_backup, including dry runs)
    first checks: marker present and matching, `f_type` equal to the recorded one. `root_dev` is
    recorded but not compared (NFS and CIFS get a new device number on every mount). Mismatch or
    missing marker → the job fails with "destination not mounted?" and writes nothing. The marker
    is re-checked through the job's `os.Root` every 500 files and before retention/expiry work.
- **S4 No overlap.** On save, resolved (EvalSymlinks) paths: a destination target may not equal,
  contain or be inside any source, the config directory, `/` or another destination; a source may
  not equal, contain or be inside a destination target, nor be the config directory or inside it
  (a source that contains the config directory is allowed: the scanner skips it). At scan time
  the scanner also skips (and warns about) any directory whose `(dev, ino)` equals a destination
  root or the config directory (bind-mount aliases).
- **S5 Deletes are not propagated.** A file that disappears from a source is moved (same-filesystem
  rename) into retention and deleted only after `retention.deletedDays` (1–3650, default 30).
  Content that is still referenced by a live name is never expired (§4.3 promotion). Records of a
  source that was unlinked from the destination or deleted (orphans) are never retained, moved,
  changed or expired.
- **S6 New before old.** Within a sync, every copy/update/move/link item runs before any retain
  item (so a Radarr upgrade that renames `X-1080p.mkv` → `X-2160p.mkv` backs up the new file
  before the old one goes to retention). An update copies and verifies the new version to a temp
  file before the old version moves to retention (§4.2). Execution order alone does not cover an
  upgrade whose new file fails to copy (unreadable, changed during copy, a name the share cannot
  store) or is held: a **retain waits** while any live catalog file in the same source folder is
  not backed up (no current live record with its size and mtime, and its content not kept in
  retention). The retain fails with a warning ("not retained yet: … is not backed up"), the old
  version stays live and recorded, and the first sync after the new file is backed up retains it.
  The catalog and records are read once per source and attempt, when the first retain runs.
- **S7 Atomic, verified writes.** Temp file `<dir>/.bunkarr-tmp-<base>-<rand>` in the final
  directory (path recorded in the item detail **before** creation, created exclusively) → copy
  while hashing (when verify ≠ off) → fsync → size check against the bytes read and against an
  `fstat` of the source taken before and after the copy (a source changed during the copy fails
  the item: "changed during copy", retried next run) → optional re-read verification (page cache
  dropped) → chtimes to the source mtime → no-replace rename (`renameat2 RENAME_NOREPLACE` on
  Linux, `renameatx_np RENAME_EXCL` on macOS; lstat + rename where the filesystem lacks it, e.g.
  NFS) → fsync directory. A crash leaves at most a temp file, never a partial final file.
- **S8 Secrets** (Plex token, Apprise URLs) are sealed (ADR 0003), never returned by the API
  (`hasApiKey`/`hasUrls` instead), sent only in headers/bodies — never in URLs (Plex:
  `X-Plex-Token` header only). A stored secret only goes where it was saved for: a stored Plex
  token only to the integration's stored URL, stored Apprise URLs only to the stored `apiUrl`
  (changing the URL needs the secret entered again; a Test with a saved id and another URL is
  refused). The token is bound to its URL at the read: `integrations.Store.TokenFor(ctx, id, url)`
  reads the stored URL and the sealed token from one row in one query and returns
  `ErrURLChanged` when the URL is not the one the caller is about to use, so a caller that loaded
  the integration before its URL changed never sends the new token to the old URL (the Plex
  backup then skips its maintenance-window check, `plex/sections` answers 409 and a Test with a
  saved id and another URL 400). `Token` (no URL check) is not used to send the key anywhere.
  The Plex client sends `/identity` without the token and checks it before anything that sends
  the token; neither the Plex nor the Apprise client follows redirects. Errors from
  HTTP clients are rebuilt so no URL, query, header or body appears in them. `internal/logging`
  additionally redacts secret **values** anywhere in log/job-log messages and attributes, longest
  first: stored secrets are held per row (`logging.SetSecrets(owner, values...)`, released when
  the row changes; released values stay redacted, bounded), secrets read but not stored
  (Plex's `PlexOnlineToken`) are pinned with `RegisterSecret`, and values held for one request
  (a token typed into a Test form) are redacted with `RedactValues` without entering the
  registry.
- **S9 Dry run.** `dryRun: true` scans (the catalog is updated), plans, runs the guards, persists
  the plan as items and changes nothing at the destination (not even the marker check writes) and
  no destination records. Its items are the preview. Every job type has one: sync and verify
  (their endpoints), Plex backup (§5), and every schedule through `POST /schedules/{id}/run
  {dryRun: true}` (System → Tasks "Preview"). A retention dry run plans the `expire` items (what a
  run would delete) and executes none; the global retention job's dry run queues a dry run per
  destination and prunes no job history. Dry runs skip reconciliation (§4.2) and never write the
  link manifest.
- **S10 Mass-change guard.**
  (a) A scan has two phases. The **walk** reads only (it writes nothing); the **commit** writes
  the changes in 1000-row batches, then one transaction marks unseen rows deleted, stores the
  hardlink groups, stats and filesystem identity. Every refusal happens in the walk, so a refused
  scan is **fatal** and leaves the catalog untouched: the source root is missing, not a directory
  or now a symlink; it is on a different filesystem than recorded; no regular, non-excluded file
  is found anywhere while the catalog has live files ("empty"); or any `ENOTCONN`/`EIO`/`ESTALE`/
  `ETIMEDOUT`/`EHOSTDOWN` occurs. "Different filesystem": another `fs_type`, or another `root_dev`
  — except on an anonymous device (device major 0, FUSE such as Unraid's `/mnt/user`, NFS, CIFS,
  btrfs, ZFS), whose number the kernel assigns at mount time: a changed number is logged and
  recorded, and the root inode (`root_ino`) is compared instead where inode numbers survive a
  remount (not on FUSE or CIFS/SMB). Every save of a source clears the recorded identity (a
  deliberate change is accepted by re-saving it). An unreadable subdirectory (EACCES, or any
  other non-fatal error) is a warning and its subtree keeps its catalog rows unchanged; a
  directory that vanished during the walk has its rows marked deleted; one whose device or inode
  changed between lstat and open keeps its rows (warning). A cancelled scan marks nothing deleted.
  (b) Before executing, a sync counts its planned changes per source: retain + update + relink of
  a changed name + a copy-mode promote standing for a vanished name (items already failed by S11
  are not counted). If they exceed the destination's `settings.maxChangePercent` (default 10 %,
  1–100) of the source's live files after the scan **and** 20 files, or exceed
  `settings.maxChangeFiles` (default 1000), those
  items are **held** (status `held`, nothing moved), everything else runs, the job ends
  `completed_with_warnings`, and a warning notification says so. An update (or relink) whose new
  size is 0 or less than half the old size is always held (truncation/ransomware), and this is
  checked again when the item runs (the source may shrink after planning). Items that depend on
  a held item (its promote, links to it) are held too. A held item's error text starts with
  "held". Held changes run on the next sync started with `allowChanges: true` (UI: "Apply held
  changes"). Dry runs report what would be held.
- **S11 Names the destination cannot store.** The destination probe (§3) records whether it is
  case-insensitive and which characters it rejects. The planner fails (warning, not fatal) any
  item whose name the destination cannot store or whose case-folded path collides with another
  live path of the same destination, and never lets one overwrite the other. The UI shows these;
  docs recommend NFS or SMB with POSIX extensions / `mapposix` for such libraries.

## 2. Destination layout (filecopy)

```
<target>/
  .bunkarr/destination.json                          marker (S3)
  .bunkarr/links.tsv                                 hardlink manifest (below), after every sync
  .bunkarr/probe/<run>/                              capability probes (§3), removed after use
  .bunkarr/retention/<jobQueuedAt yyyymmddThhmmssZ>-job<id>/<destFolder>/<relPath>
  .bunkarr/plex/<integration-slug>-<integrationId>/<yyyymmddThhmmssZ>[-job<id>]/   Plex DB version
  .bunkarr/plex/<integration-slug>-<integrationId>/.partial-job<id>/               being written
  .bunkarr/plex/<integration-slug>-<integrationId>/.prune-<version>/               being pruned
      com.plexapp.plugins.library.db
      com.plexapp.plugins.library.blobs.db   (when present)
      Preferences.xml                          (sensitive: contains PlexOnlineToken; mode 0600)
      manifest.json  {format, createdAt, method, plexVersion, sqliteVersion, integrationId,
                      integrationName, jobId, jobQueuedAt, result, integrity:{...},
                      files:[{name,size,sha256}], warnings}
  <destFolder>/<relPath>                              live mirror of each source
```

The retention directory name uses the job's `queued_at`, so a resumed job reuses it; a name
already taken there gets a numeric suffix (`movie.1.mkv`). Reconciliation (§4.2) finds the job of
a retention directory by its `-job<id>` suffix. The Plex folder name carries the integration id
so two servers with similar names stay apart and a resumed job finds its folder after a rename;
`-job<id>` is appended only when a version of the same second already exists.

**Hardlink manifest** (`.bunkarr/links.tsv`, format 1). A `link_recorded` name has no file at the
destination, and its relationship would otherwise live only in Bunkarr's database (which Phase 1
does not back up). Every sync that completes (not a dry run) writes the manifest through the job's
root with `WriteFileAtomic` (temp + rename), and only when its content changed: a `#` comment
header explaining the format, then one line per live `linked` or `link_recorded` record, sorted by
name: `<name> TAB <primary> TAB <state>`, both paths relative to the target (`<destFolder>/…`).
In paths `\`, tab, newline and carriage return are escaped as `\\`, `\t`, `\n`, `\r`, and a
leading `#` as `\#`, so only comment lines start with `#`. A failure to write it is a job warning.
The README's "Restoring" section shows how to recreate the names (`ln`, or `cp`).

`destFolder`: per source, default slug of the source name; a clean relative path whose parts do
not start with ".", end in "." or a space, or contain `\ : * ? " < > |` or control characters
(255 bytes per part, 1024 in total); unique ignoring case, and no source's destFolder may contain
another's; immutable once any destination holds files for the source (API 409). While
destinations hold backups the source's path may only change to the **same directory** (a renamed
mount point), else 409 (`catalog.Store.checkSameRoot`):
- the new path's `(dev, ino)` equals the root recorded by the last scan (or, with none recorded,
  the current path's) → accepted;
- same device but another inode, a non-anonymous device, or another filesystem type → refused;
- an anonymous device whose inode numbers survive a remount (NFS, btrfs, ZFS, tmpfs) with another
  device number → refused: a snapshot, clone or replica has the same inodes, sizes and mtimes on
  another device. After such a remount the source is scanned at its current path first (the scan
  records the new device), then the path changes;
- an anonymous device with run-time inode numbers (FUSE such as `/mnt/user`, CIFS/SMB) → the new
  directory is recognised by its files: at least half of the 16 oldest live catalog rows are
  regular files there with their recorded size and mtime. This is accepted only while the current
  path no longer holds the source: it must not be the recorded root, must not hold those files,
  and must not be on a filesystem of the recorded type (else the new path is taken to be a copy).

Mirroring an existing rsync tree: set destFolder to the existing folder name (§4.4 adoption).

## 3. Destination capability probe

Run on create (required) and on `POST /destinations/{id}/test`; stored in
`destinations.capabilities` (JSON) and used by the planner:
`{hardlinks: bool, unstableInodes: bool, caseInsensitive: bool, invalidChars: "<chars>",
trailingDotSpace: bool, mtimeGranularityNs: int, fsType: "cifs|smb2|nfs|ext4|...", checkedAt,
probeVersion: int}`. Probes run in a random directory inside `.bunkarr/probe/` and clean up after
themselves: hardlink (`link`, then append through one name and read the other), case (`create A`,
`stat a`), each of `: ? * " < > | \` (read back from the directory listing), trailing dot/space,
and mtime granularity (chtimes to an odd and an even second plus …123456789 ns and read back; two
samples tell 1 s from FAT's 2 s; the coarser result is used). `test` also returns free/total bytes
(statfs), marker status, whether the target is writable, and the number of entries at the top
level. A test before create writes nothing: it returns no capabilities (`null`; Create probes) and
writability is only estimated with access(2). A local filesystem or a foreign marker is a warning
(`ok` stays true: Create accepts them with `allowLocal`/`attach`).

Inode identity. Jobs ask whether two destination names are one file (a replacement's old version
hardlinked into retention, a hardlink made by an interrupted item, two spellings of a name on a
case-insensitive destination). The hardlink and case probes therefore also require every name of
one file (the first, the second, and a third linked after the content changed; `A` and `a`) to
lstat with the same device and inode number and a link count ≥ 2; where they do not,
`unstableInodes` is true. A CIFS client mounted with `noserverino` (as Unraid's Unassigned
Devices mounts shares) numbers the inode of each lookup itself: hardlinks work there, but their
names show different numbers. Inode numbers identify files only where the current probe
(`probeVersion` 1; 0 or missing is an older probe that did not check) found them stable; elsewhere
two regular files count as one when they have the same size, mtime and sha256, and a case variant
counts as the other spelling when the directory lists no entry of its own for it. A link item
never records a name whose link count is 1 as a hardlink of another name (the CIFS client still
reports the server's count): an unmanaged separate copy of the content is adopted (or displaced)
like any file in the way, not taken for the hardlink. A count of 1 is only evidence against a
hardlink, and may be wrong that way (a client reports 1 for a name whose attributes a directory
listing primed): such a hardlink is then recorded as a file of its own, which is safe; a separate
copy is never recorded as a hardlink.

A probe's finding does not last: a share may be remounted with `noserverino`, and a CIFS client
turns server inode numbers off at run time when it finds them unreliable. So every sync, verify
and retention job that is not a dry run checks again through its `os.Root` before it first
compares two names (in reconcile, or in an item), and uses the result for the rest of the job:
capabilities of an older probe are probed again as a whole when the job starts; current ones have
the hardlink and case identity checks rerun (a few small files in a run directory under
`.bunkarr/probe/`). A job that never compares two names does not check and writes nothing for it
(a sync that fails its source checks leaves the destination as it was). A changed
`unstableInodes` is stored and logged. A check that fails (it cannot write there, or the
destination no longer makes hardlinks although its capabilities say so) is a warning, and the job
trusts no inode numbers (nothing is stored). A dry run writes nothing, so it does not check: it
trusts no inode numbers either.

## 4. Sync semantics

### 4.1 Flow of a sync job
1. **Preflight**: destination enabled, S3 checks, open `os.Root`, then (not in a dry run)
   **reconcile** what stopped jobs left half done (§4.2 retention intents), then the sources
   linked and enabled (optionally narrowed by `Params.SourceIDs`).
2. **Scan** each linked source (incremental; same code as a scan job) while holding the source's
   lock across scan and plan; the source is re-read under the lock (a source deleted or disabled
   since the job started is dropped). S10(a) refusal fails the job.
3. **Plan** (skipped on resume if `jobs.planned_at` is set): diff the live catalog against
   `destination_files` (the records, not the destination) and emit items (§4.2). Items are
   persisted in batches; the final batch and `planned_at` are written in one transaction
   (`ItemStore.AddItems(..., final=true)`). A job with items but no `planned_at` (killed while
   planning) deletes its items and plans again. A resume with a complete plan does not rescan;
   instead each source root must be a directory that is not empty while its catalog lists files,
   else the job fails ("is it mounted?"), so an unmounted share cannot turn every file into a
   retain.
4. **Guards**: S10(b) holds; S11 name checks; free-space check: bytes of copy + update + (new
   side of) move items that are not adopted must fit in `free − 1 GiB`, else the job fails before
   copying anything (dry run: reported). A dry run stops here: its persisted items are the
   preview.
5. **Execute** pending items in this order: `promote`, `move`, `copy`/`update`/`adopt`, `link`,
   `retain`. The marker is re-checked every 500 items and before the retain phase. `expire` runs
   only in retention jobs.
6. **Finish**: stats, summary, warnings. Empty directories a move, retain or promote left inside
   a destFolder are pruned (never the destFolder itself). The hardlink manifest (§2) is rewritten.

Errors: an error `filecopy.Classify` calls fatal stops the job; the item in progress is salvaged
first (what it had moved into retention is recorded, a retain's file is put back, its temp file
is removed), because a failed job is never resumed. A per-file error fails the item with a
warning; an item that stops after it moved a recorded file into retention settles that record's
retention intent first (`putBack`, §4.2). Cancellation removes the job's own temp files and puts
back an interrupted retain. Whatever a failed, cancelled or crash-limited job still left is
settled by the next job of the destination (reconciliation, §4.2).

### 4.2 Items (the planner's actions)
Identity of a source file within a scan is `(source, relPath)` plus `(size, mtimeNs)`; content is
"unchanged" iff size and mtime (compared at the destination's `mtimeGranularityNs`) are equal to
the `destination_files` record. `(dev, ino)` is only used inside one scan (§4.3) and never
compared across scans.

- **copy** — live catalog file, no live record. Before writing, if the path exists at the
  destination unrecorded: adopt it if it matches (§4.4), else displace it into retention (record
  it as a retained row with `reason = displaced`) and copy. With `detail.reason = repair` it
  re-copies a record verify marked `missing`; a damaged file still there is retained
  (`reason = damaged`), never overwritten, and hardlinks sharing a damaged primary's inode are
  relinked after the repair.
- **update** — live record whose size or mtime differs. Sequence: temp complete and verified →
  if hardlinks are supported: `link(final → retentionPath)` then `rename(temp → final)` (no gap);
  else `rename(final → retentionPath)` then `rename(temp → final)` (a resume finishes a half-done
  pair from the item detail). Then the record is updated and a retained row is added
  (`reason = replaced`).
- **move** — a live catalog path with no record whose size and mtimeNs exactly equal a live
  record whose source path vanished in this scan **and** whose head/tail hash (sha256 of the
  first and last 1 MiB, the whole file up to 2 MiB; prefixed `headtail-sha256:` so it is never
  taken for a content hash) of the new source path equals that of the vanished record's
  destination file. Inode numbers are not used. Executes as a same-filesystem rename at the
  destination (cheap; covers Radarr/Sonarr "Rename files" and folder reorganisations); a
  case-only rename on a case-insensitive destination may replace, since both names are one file.
  Otherwise the pair is copy + retain. When the file's hardlink group still has a recorded name,
  a new name of the group is linked to it instead of moved.
- **link** — another name of a hardlink group (§4.3) whose primary is copied/present. With
  `capabilities.hardlinks` and `settings.hardlinks = "recreate"`: `link(primaryDest, dest)`,
  state `linked`. Otherwise: no file, state `link_recorded` (spec: copy once, record the
  relationship). Runs only after the primary's item is `done`; if the primary's item failed or was
  held, the primary is not present, or the two source names are no longer one inode, the link
  item becomes a copy. A link item on a name that already has a record (a relink after both names
  changed, or a `missing` name) first moves that name's own old file into retention
  (`reason = replaced`).
- **retain** — live record whose source path vanished (and is not a move). Rename into retention;
  record becomes `retained` with `reason = deleted` and `expires_at = now + deletedDays`. Waits
  (fails with a warning, record untouched) while a file of the same source folder is not backed
  up (S6).
  Recorded-only links whose own name vanished too are removed with it; other recorded-only links
  block it until promoted. A path that is still on disk but left the catalog (newly excluded, now
  a symlink, renamed in case only on a case-insensitive source) is retained; only a regular file
  the catalog lists again counts as "reappeared".
- **promote** — a record about to be retained (or updated) that live `link_recorded` rows depend
  on: rename its file to the first surviving dependent's path, make that row `present`, re-point
  the other dependents; the vanished name's record is then removed (its content lives on under
  the dependent). In `linked` mode the promote is **record-only** (the dependent, already a
  hardlink of the inode, becomes `present` and the others are re-pointed), followed by a retain
  of the vanished name; without it `link_of` would point at a retained row whose expiry the
  referenced-content check would block forever. A `missing` record is never blocked by its
  dependents: its content is already gone or damaged, and its repair restores it.
- **adopt** — unrecorded destination file matching the source (§4.4): record only.
- **expire** (retention job) — retained row past `expires_at`: delete the retained file (only
  inside a job directory under `.bunkarr/retention/`, refusing symlinked directories on the way,
  only if its size still equals the record, never content a live record still links to, never an
  orphan source's row), delete the row, prune empty retention directories.

`destination_files.reason` (retained rows only) is one of `deleted`, `replaced`, `displaced`,
`damaged`.

Every item is **re-checked at execution** (and on resume): source re-stat; destination state; if
the desired end state already exists (e.g. the rename happened but the DB write did not), the
item records it and is marked done. A retain or move whose rename an earlier attempt already did
is finished whatever the source shows now (the next sync copies back a file that returned). The
order of effects is always: item detail (intent: temp path, retention path) → retention intent
on the record (when a recorded file moves into retention) → filesystem → `destination_files` →
`ItemStore.Finish`. The database writer uses `synchronous=FULL`, so a
recorded intent is durable before the fsynced filesystem step it describes. If the record write
fails after the filesystem step, the item fails with a warning; the next sync adopts the
unrecorded file (or displaces it when adoption is off).

**Retention intents and reconciliation.** Before a file that has a live record (`present`,
`linked`, `missing`) is renamed or hardlinked into retention (retain, the old version of an
update or relink, a damaged file), the record gets the chosen retained path and reason in its
`retained_path`/`reason` columns (otherwise used only by retained rows). The write that records
the outcome clears it. So no file sits in retention unrecorded, and no record says present while
its file is in retention, even when the item never runs again (a failed or cancelled job is not
resumed; a job failed by the crash limit neither). Settling an intent (`intentFixer.settle`):
a file still in retention goes back to its path when the path is free (never over anything);
otherwise it stays and is recorded as a retained row (the item's description of it, or the
intent's reason with no hash). When the path then holds another file than the retained one, the
live record becomes `missing` (the next sync re-copies and keeps that file as `damaged`), never
trusted with the old hash. Nothing is deleted. It runs:
- in the item itself, when it stops with an item error (`putBack`) or a fatal one (`salvage`);
- at the start of every sync, verify and retention job that is not a dry run (`reconcile`, after
  the destination is opened): first a **sweep** of every `.bunkarr/retention/<…>-job<id>`
  directory except the running sync's own, reading that job's pending items: an unmanaged file an
  item displaced there is recorded (`reason = displaced`, an orphan row if its source was
  deleted), a replacement an item completed at the destination but did not record is recorded
  (`finishPlaced`: the path holds a file with the temp file's size and mtime, the old version is
  in retention, the record still has the item's intent), and the item's temp file is removed;
  then every remaining intent outside the running job's directory is settled. What cannot be
  settled is a job warning and is retried by the next job; fatal errors (S3) fail the job.

### 4.3 Hardlinks
- Groups are computed **within one scan and one source only**: names with equal `(dev, ino, size,
  mtimeNs, ctimeNs)` and `nlink ≥ 2`. A group with more members than `nlink`, or disagreeing
  members, is split into single files with a warning. The group id stored in
  `catalog_files.hardlink_group` is a per-scan surrogate `"<sourceId>.<scanSeq>:<n>"`
  (`sources.scan_seq` counts successful scans), never reused and never matched against earlier
  scans.
- On FUSE sources (Unraid `/mnt/user`, statfs magic `0x65735546`) inode numbers are not stable
  across time and only identify hardlinks when Unraid's "Tunable (support Hard Links)" is on: the
  source test warns about this; grouping additionally requires equal head/tail hashes (read
  through the `os.Root`, checking the file is still the one the walk saw). Docs recommend
  mounting a pool/disk path (`/mnt/cache/data`, `/mnt/diskN/data`) or the share with hard-link
  support enabled.
- Unique bytes: a group counts once (`uniqueBytes`); `bytes` counts every name.
- Every `linked` and `link_recorded` name is listed with its primary in the destination's
  hardlink manifest (§2), so a restore without Bunkarr's database can recreate it.
- Execution re-stats both names and requires equal `(dev, ino, size, mtime)`; otherwise the link
  item becomes a copy.
- Group split (an *arr replaced one name with a new inode): the remaining names are no longer in
  the same group. A name whose destination state is `link_recorded` to a primary whose content
  changes keeps the old content: a promote renames the primary's old file to that name's path (no
  data transfer); the primary's record becomes `missing` and its update writes the new version
  with nothing to retain. In recreate mode the existing hardlink already holds the old content:
  the promote is record-only and its record becomes `present`.

### 4.4 Adoption (switching from rsync)
With `settings.adoptExisting` (default `"size+mtime"`), an unrecorded destination file at the
planned path is adopted when its size equals the source and its mtime equals the source's at the
destination's granularity, within `settings.mtimeWindowSec` (default 0, max 3600; like rsync
`--modify-window`; set 1–2 for FAT/older SMB servers). `"size+hash"` additionally or instead
compares full sha256 of both sides (slow, one-time) and, on a match, sets the destination mtime.
`"off"`: never adopt (unrecorded files are displaced). A resumed item whose final file exists
unrecorded with the item's recorded temp hash/size is adopted the same way. Adopt items are not
changes for S10(b) and are not counted in `bytesPlanned`.

### 4.5 Verify job
Walks all live records: stat existence and size (cheap); re-reads and hashes a sample
(`samplePercent`, default 5 %, least recently verified first; `full` = all) and compares with the
recorded hash (files copied with verify off get their hash recorded on first verify). Before and
after each re-read the page cache is dropped where possible (`posix_fadvise(DONTNEED)` on Linux,
`F_NOCACHE` on macOS). Missing, short or mismatching files fail the item, mark the record `missing`
(the next sync copies it again) and make the job `completed_with_warnings`; a notification is
sent. A damaged hardlink also marks its primary `missing` when both names are still one file at
the destination (a resumed verify re-applies this). A file that cannot be read fails its item
without marking the record (`filesFailed`, "could not be checked"). A verify that is not a dry
run first reconciles (§4.2), so a file a stopped sync left in retention is back (or recorded)
before its record is checked. `POST .../verify` accepts `{dryRun}`: a dry run plans the items
(which files would be checked or re-read) and checks nothing. Default schedule: weekly, Sunday
05:00 (created with the destination, editable).

## 5. Plex DB backup (ADR 0005)
- Source: `<dataPath>/Plug-in Support/Databases/com.plexapp.plugins.library.db` (+ blobs db) and
  `<dataPath>/Preferences.xml`. `dataPath` is the Plex "Plex Media Server" directory mounted into
  Bunkarr, **read-only** by default. Bunkarr's PUID must be able to read `Preferences.xml` (0600,
  owned by Plex's user). It is opened `O_RDONLY|O_NOFOLLOW|O_NONBLOCK`, read twice and compared,
  staged with mode 0600, and its `PlexOnlineToken` is registered as a secret. Missing or
  unreadable (wrong PUID): the backup continues with a warning. At the destination the file is
  stored as is (cleartext token, mode 0600, which an SMB share without POSIX extensions does not
  enforce; §11, DEFERRED.md).
- Method: `file:<db>?mode=ro` (never read-write, never `query_only` alone, never `immutable` while
  a `-wal` or rollback `-journal` exists) + `busy_timeout`; `sql.Conn.Raw` → `NewBackup(staging)`
  → one `Step(-1)`. No `-wal` (Plex stopped cleanly): `mode=ro&immutable=1` with a
  size/mtime/inode guard before and after; a change or a `-wal` appearing retries, up to 3
  attempts. Staged copies go to a temp name, are fsynced and renamed.
- Driver: plexdb owns a private modernc `sqlite.Driver`, which opens Plex's databases and the
  copies. The collation stubs are registered on it (`(*sqlite.Driver).RegisterCollationUtf8`,
  guarded against its own opens), never on the shared `sqlite` driver: modernc reads a driver's
  collation map without a lock in `Open`, so registering there while Bunkarr's pools open
  connections is a concurrent map read/write. Each name is registered once per process, and the
  stubs never reach Bunkarr's own database (tested).
- Staging: `<config>/staging/plexdb-job<id>/`, then verify there, then copy with the filecopy
  engine (S7: sha256 compared with the staged copy; re-read unless the destination's verify mode
  is off) into `.bunkarr/plex/<slug>-<integrationId>/.partial-job<id>/`, write `manifest.json`,
  rename the directory to its timestamp, record the snapshot, remove staging. Staging directories
  of jobs that failed for good are removed once older than 24 h (checked at the start of each
  Plex backup job).
- Recovery (start of every run): removes every `.partial-job*` directory of the integration (no
  other job of it can run: lock key `plexdb:<integrationId>`) and finishes interrupted prunes. A
  complete version directory without a row (crash between rename and insert) is re-hashed
  against its manifest and recorded (as failed if damaged). When the job is resumed and its own
  version (manifest `jobId` + `jobQueuedAt`) is complete, recorded or not, the job finishes with
  it without backing up again. Otherwise the backup starts over (it is minutes, not hours). A
  directory named like a version with no row and no valid manifest of the integration is left
  alone, with a warning.
- Verification (`Verify(ctx, path)`, cancellable; copy opened `mode=ro&immutable=1`): stub binary
  collations for every non-built-in collation named in `COLLATE` clauses or index key collations
  (`pragma_index_xinfo`), `quick_check = ok`, `integrity_check(100000000)` ignoring only
  `row N missing from index X` for indexes using such a collation; counts of `metadata_items`
  and `media_parts` (a library copy without them fails; an empty file fails). Result in
  `snapshots.integrity` (`ok`/`failed`) and the manifest. A failed version is recorded, kept 7
  days, and the job fails (`ErrIntegrity`, so a failure notification goes out); pruning is
  skipped.
- Versions: keep the newest `plexDbDaily` (default 14, 1–365) versions with integrity `ok`, plus
  the newest `ok` version of each of the `plexDbWeekly` (default 8, 1–520) most recent ISO weeks
  that have one (restic keep-weekly semantics); never delete the newest `ok` version; failed
  versions are kept 7 days for diagnosis. Pruning runs only after a successful backup, deletes
  only directories that have a row and match `.bunkarr/plex/<folder>/<timestamp>` (every path
  component a real directory), and renames a version to `.prune-<v>` before removing it. Versions
  of a deleted integration are never pruned automatically.
- Disabled integration or destination: the job fails (manual runs answer 409 at the API; the
  scheduler does not queue it). A job whose `destinationId` is 0 uses `settings.backup.
  destinationId`.
- Dry run: the same preflight, then one `skipped` item per file (sizes, WAL state, planned
  method, destination path in the detail; `failed` for a file that would not be backed up).
  Nothing is written at the destination or in staging.
- Schedule: `settings.backup {destinationId, cron, enabled}` is mirrored by one schedules row
  (`plexdb_backup`, params `{integrationId, destinationId}`); default cron `0 6 * * *` (06:00
  daily, outside Plex's butler window, default 02–05). GET returns the row's cron/enabled, so
  edits on System → Tasks show there. The UI warns on overlap using `butlerStartHour`/
  `butlerEndHour` from `POST /integrations/test` (from `GET /:/prefs`), Plex's default otherwise.
  At run time a backup inside the window is a warning (`completed_with_warnings`), never a
  failure; the check uses Bunkarr's TZ, so run both containers in the same TZ. The Plex version
  goes into the manifest when Plex answers.
- A plexdb_backup job does not wait behind a sync of the same destination (different lock key).
  SQLite's backup step cannot be interrupted: a cancel or shutdown takes effect after it (about
  2 minutes for 5 GB); a shutdown past the 20 s grace re-queues the job, which starts over.

## 6. Jobs
### 6.1 Contract
`internal/jobs/contract.go` defines the types; `internal/jobqueue` implements the manager. Every
runner is idempotent and resumable: a job re-run after a crash has `Attempt > 1`,
`Trigger = resume`; a planned job executes only pending items, re-checking each (§4.2).
Cancellation = ctx cancellation: stop promptly, remove own temp files, return `ctx.Err()`;
bookkeeping writes after cancellation use `context.WithoutCancel`. Fatal (`filecopy.Classify`:
S3/S10(a) failures; `ENOSPC`, `EDQUOT`, `EROFS`, `EIO`, `ENOTCONN`, `ESTALE`, `EHOSTDOWN`,
`EHOSTUNREACH`, `ENETDOWN`, `ENETUNREACH`, `ETIMEDOUT` on either side; permission denied anywhere
on the destination; a cancelled context) → error → `failed`. Per-file (vanished, unreadable,
changed during copy, name not storable) → item `failed`, warning, continue →
`completed_with_warnings`. Progress via `Reporter.Progress`; logs via `Reporter.Log`.
Stats for sync: `dryRun, filesPlanned, filesCopied, filesUpdated, filesMoved, filesAdopted,
filesLinked, filesPromoted, filesRetained, filesDisplaced, filesHeld, filesFailed, filesSkipped,
bytesPlanned, bytesCopied, durationMs, sources:[{sourceId, name, files, added, changed, deleted,
skipped, changes, held, ...}]`. `bytesPlanned`/`bytesCopied` count copy + update bytes: link items
carry 0 bytes (a hardlink group counts once); adopt and move items are excluded. An item whose
outcome differs from its action (a link or move that became a copy, a copy that adopted) is
counted under its outcome within one attempt (across a resume, see DEFERRED.md). Verify:
`filesPlanned, filesChecked, filesVerified, bytesVerified, filesMissing, filesFailed,
filesSkipped, hashesRecorded`. Retention: `jobsQueued, jobsPruned, filesPlanned, filesExpired,
filesKept, filesFailed, bytesExpired`.

### 6.2 Manager
- Lock keys: sync/verify/retention → `dest:<id>`; plexdb_backup → `plexdb:<integrationId>`;
  scan → `source:<id>` (a sync also takes `source:<id>` for its sources while scanning). A job
  starts when a worker is free (setting `jobs.workers`, default 2, max 32, read at start-up) and
  its keys are free; FIFO otherwise, but a queued job whose keys are free may overtake a blocked
  one.
- Params are validated per type on enqueue and on schedule upsert (a job without its lock key
  would escape serialization): sync/verify need `destinationId`, plexdb_backup `integrationId`,
  scan at least one `sourceId`; ids are positive. Params are canonical (source ids sorted and
  de-duplicated).
- The seeded global retention schedule (daily 04:30) enqueues a retention job with no
  destination; that runner enqueues one retention job per enabled destination, deletes finished
  jobs older than `jobs.historyDays` (setting, default 90; items and logs cascade) and completes.
  As a dry run it enqueues dry runs and deletes nothing. A destination's retention job reconciles
  (§4.2) before it plans its `expire` items.
- Enqueue dedupe: an identical queued job (type, params, dry run) is returned instead of a new
  one; the check and the insert are one transaction.
- Scheduler: `jobqueue.NewScheduler(store, enqueuer, log, loc)` (`New` is already the Manager's
  constructor). robfig/cron 5-field expressions plus `@hourly`/`@daily`/`@weekly`/`@monthly`
  (exact case), evaluated in the container's TZ; `ValidateCron` rejects seconds, `@every`, `TZ=`
  prefixes and expressions that never fire. A fire is **skipped** (logged) when a non-dry-run job of the same type is running
  for the same destination (sync, verify) or with the same canonical params (other types: a Plex
  backup is never skipped for another server's backup to the same destination); a running dry
  run never blocks a scheduled run. A reload never fires a schedule twice for one minute and keeps
  the pending next run of an unchanged expression; reloads are serialized and not tied to the
  request that caused them. Runs missed while Bunkarr was down are not caught up. The API's gate
  does not queue scheduled jobs whose destination or integration is disabled (`GET /schedules`
  reports it as `blockedReason`).
- Default schedules: retention daily 04:30 (seeded); sync: none until one is chosen (the UI form
  proposes nightly 02:00); verify: weekly, Sunday 05:00, created with the destination; Plex DB
  backup: daily 06:00 when enabled without a cron.
- Graceful shutdown (SIGTERM): cancel running jobs, wait up to 20 s, set them `queued` with
  trigger `resume` **without** incrementing `attempt`; a job still running after the grace period
  is re-queued and its late result discarded. Start-up: rows still `running` crashed → `queued`,
  `resume`, `attempt + 1`; attempt > 3 → `failed` ("crashed 3 times", OnFinish fires). A runner
  panic fails the job with the redacted message (stack to the process log). The context of a job
  is cancelled only by Stop (shutdown) or Cancel (user), never by the process context.
- Progress is kept in memory and persisted with `heartbeat_at` at most every 2 s; throughput is
  an EWMA with a 10 s time constant (samples ≥ 500 ms apart); ETA from remaining bytes.
- Job logs are value-redacted (S8) and key-redacted (`logging.SensitiveKey`, recursively);
  debug lines are stored only when the process log level is debug. Lifecycle lines ("Job
  started", "Job finished", "Interrupted by shutdown") are added by the manager.
- `OnFinish(func(Job))` hooks (notifications) run once per final state, in a goroutine, panics
  recovered; Stop waits for them within its deadline.

## 7. API (all under `/api/v1`, auth required, JSON camelCase, sizes in bytes)

General: bodies are decoded with unknown fields rejected; endpoints whose body is optional
(sync, verify, plex/backup, destination test, schedule run) accept an empty body. **Paged**
results are `{page, pageSize, totalRecords, records}` (the *arr shape); `page ≥ 1`, `pageSize`
1–500 (default 50); out of range → 400. Job-starting endpoints answer 202 with the `Job` and a
`Location` header. Errors are `{message}`: validation → 400; not found → 404; conflicts → 409
(busy source, name taken, marker exists, destination not mounted / marker mismatch / filesystem
changed, job not active, delete while jobs are queued or running, manual run of a disabled
destination or integration, blocked schedule); local filesystem without `allowLocal` → 400;
Plex upstream failure → 502; `/filesystem` permission denied → 403; anything else → 500 with the
message redacted.

Integrations
- `GET /integrations` → `[Integration]`; `POST /integrations` → 201 `Integration`
- `GET|PUT|DELETE /integrations/{id}`. PUT has PUT semantics (name and url required; missing
  `settings`/`enabled` are kept; type cannot change). An empty/missing `apiKey` keeps the stored
  one; `clearApiKey: true` removes it (with `apiKey` → 400); a changed `url` needs the `apiKey`
  again. DELETE → 409 while it has queued or running jobs; removes its schedules.
- `POST /integrations/test` `{type, url, apiKey?, id?}` → `{ok, message, version?,
  machineIdentifier?, butlerStartHour?, butlerEndHour?}`. `type` is required unless `id` is given.
  With `id` and no `apiKey` the stored token is used, only when `url` is the stored URL (else
  400). Types other than plex answer `ok: false` ("not available yet").
- `GET /integrations/{id}/plex/sections` → `[{key, title, type, locations:[{path, localPath,
  exists}]}]` (`localPath` "" when no path mapping applies; identical mounts need an identity
  mapping)
- `POST /integrations/{id}/plex/backup` `{destinationId?, dryRun}` → 202 `Job` (`destinationId`
  0/missing: the integration's backup destination)

`Integration = {id, type, name, url, enabled, hasApiKey, settings, createdAt, updatedAt}`; Plex
`settings = {dataPath, pathMappings:[{plex, local}], backup:{destinationId, cron, enabled}}`
(POSIX absolute paths; at most 64 mappings, longest prefix on a path-segment boundary wins;
`dataPath` required only when the backup is enabled; stored normalized, unknown fields dropped).

Sources and catalog
- `GET /sources` → `[Source]`; `POST /sources` → 201; `GET|PUT|DELETE /sources/{id}` (changing
  path or destFolder, and DELETE → 409 while the source is scanned or has queued or running jobs;
  DELETE removes its schedules)
- `POST /sources/test` `{path}` → `{ok, path (resolved), exists, isDir, fsType, fuse, entries,
  message, warnings:[...]}` (includes the S4 check)
- `POST /sources/{id}/scan` → 202 `Job`
- `GET /sources/{id}/files?page&pageSize&search&filter=all|hardlinked|deleted` → paged
  `CatalogFile` (`all` = live files; `search` matches `%`, `_` and `\` literally)
- `GET /catalog/stats` → `{sources, files, bytes, uniqueBytes, hardlinkGroups, hardlinkedFiles}`
  (sum of the per-source stats)
- `GET /filesystem?path=` → `{path, parent, directories:[{name, path}], truncated}` (read-only
  picker: directories, including symlinks to directories; "/" when `path` is empty; relative →
  400; at most 10000 entries)

`Source = {id, name, path, destFolder, exclude:[glob], enabled, plexIntegrationId, plexSectionId,
plexPath, arrIntegrationId, fsType, lastScanAt, lastScanStatus, stats:{files, bytes, uniqueBytes,
hardlinkGroups, hardlinkedFiles, skipped}, createdAt, updatedAt}` (string fields are "" when
unset; `skipped` does not count excluded entries); `CatalogFile = {id, relPath, size, mtime,
hardlinkGroup, nlink, deleted, deletedAt?}`. Stats are written by each successful scan.

Destinations
- `GET /destinations`, `POST /destinations` `{..., attach?, allowLocal?}` → 201,
  `GET|PUT|DELETE /destinations/{id}`. The target cannot change after create. DELETE → 409 while
  the destination has queued or running jobs; it removes the destination's schedules (including
  Plex backups to it), turns off the backup of Plex integrations that used it, deletes its
  records, and never touches the data at the target (the marker stays: attach to reuse it).
- `POST /destinations/test` `{target}` (before create, writes nothing) and
  `POST /destinations/{id}/test` (re-runs the probe) → `{ok, marker:"ok|missing|mismatch|foreign",
  writable, fsType, local, capabilities, freeBytes, totalBytes, entries, message, warnings:[...]}`
- `POST /destinations/{id}/sync` `{dryRun, allowChanges}` → 202 `Job`;
  `POST /destinations/{id}/verify` `{dryRun?}` → 202 `Job`
- `GET /destinations/{id}/snapshots` → `[Snapshot]`

`Destination = {id, name, engine:"filecopy", target, enabled, sourceIds, fsType, capabilities,
schedule:{cron, enabled}, verifySchedule:{cron, enabled}, settings:{verify:{mode:"off"|"sample"|
"full", samplePercent}, hardlinks:"recreate"|"copy", adoptExisting:"size+mtime"|"size+hash"|
"off", mtimeWindowSec, maxChangePercent, maxChangeFiles}, retention:{deletedDays, plexDbDaily,
plexDbWeekly}, lastJob, lastSync, createdAt, updatedAt}`. Missing or zero settings and retention
values take the defaults. `schedule`/`verifySchedule` on create: missing sync schedule → none;
missing verify schedule → `0 5 * * 0`, enabled; on update: missing → kept; `{cron: "", enabled:
false}` → removed. `lastJob` is the newest job of any type; `lastSync` the newest sync that is not
a dry run.
`Snapshot = {id, destinationId, integrationId, jobId, path, createdAt, size, method, integrity,
manifest}` (`integrationId`/`jobId` are 0 when that row is gone).

Jobs and schedules
- `GET /jobs?state=active|finished&type&status&destinationId&page&pageSize` → paged `Job`
- `GET /jobs/{id}`, `POST /jobs/{id}/cancel` → 200 `Job` (409 when not active)
- `GET /jobs/{id}/items?action&status&page&pageSize` → paged `Item`;
  `GET /jobs/{id}/items/summary` → `[{action, status, files, bytes}]`. Sync item details
  (`syncer.Detail`) carry `reason, displace, oldSize, primary, from, for, outcome`.
- `GET /jobs/{id}/logs?afterId&limit` (limit 1–1000, default 200) → `[{id, at, level, message,
  fields}]`
- `GET /schedules` → `[{id, jobType, params, description, cron, enabled, lastRunAt, nextRunAt,
  blockedReason}]` (`nextRunAt` null when disabled or blocked)
- `PUT /schedules/{id}` `{cron?, enabled?}` (missing keeps); `POST /schedules/{id}/run`
  `{dryRun?}` (optional body) → 202 `Job` (409 when blocked). `dryRun: true` queues a dry run of
  the schedule's job (for retention: what would expire), a separate job from a real run (the
  enqueue dedupe compares the flag), not recorded as a run of the schedule (`lastRunAt`).

A destination's and an integration's schedules are the same schedules rows edited from
Destinations / Settings → Plex and from System → Tasks; every change reloads the scheduler, and
start-up removes schedules whose destination, integration or source no longer exists.

`Job` as in `contract.go` (progress includes `bytesPerSec`, `etaSeconds`).

Notifications (Settings → Connect)
- `GET|POST /notifications`, `PUT|DELETE /notifications/{id}`, `POST /notifications/test`
  `{id?, ...input}` → `{ok, message}` (an id alone tests the saved target)
`Notification = {id, name, kind:"apprise", enabled, apiUrl, configKey, hasUrls, onFailure,
onWarning, onSuccess, createdAt, updatedAt}`; `urls` (comma/whitespace separated) is
write-only. The booleans are optional in requests: missing → defaults on create (`enabled`,
`onFailure`, `onWarning` true, `onSuccess` false), kept on update. `apiUrl`: absolute http(s)
without userinfo, query or fragment. Stateless mode: `urls` required; an empty `urls` on update
keeps the stored URLs only with the same `apiUrl`. Stateful mode: `configKey`
(`[A-Za-z0-9_-]{1,64}`); `urls` and `configKey` together → 400; saving in stateful mode deletes
stored URLs. Apprise API: stateless `POST {apiUrl}/notify` `{urls, title, body, type}` or stateful
`POST {apiUrl}/notify/{configKey}` `{title, body, type}`; `type` ∈ `info|success|warning|failure`.
15 s per attempt, one retry after 2 s on 5xx, network error or timeout; no retry on 4xx, 424 or
204 (204 = nothing delivered = failure); errors name the `apiUrl` and status, never URLs or
bodies. Sent for: job failed (dry runs too), completed with warnings (incl. held changes, verify
mismatches; not for dry runs), optionally completed (not for dry runs); per-target switches apply
on top. Title `Bunkarr: <what>[ (dry run)] failed|completed with warnings|completed`, e.g.
"Sync to UNAS". Delivery is asynchronous (2 workers, queue of 100; a full queue drops with a log
line).

Every route is in `internal/api/openapi.json` (`TestOpenAPIMatchesRoutes`).

## 8. Packages and ownership

Each package owns its tables' SQL; other packages use its exported API.

| Package | Owns | Exposes (minimum) |
|---|---|---|
| `internal/integrations` | `integrations` | `Store` (List/Get/Create/Update/Delete, `Token`, `TokenFor(ctx,id,url)`, `RegisterSecrets`), `PlexSettings`, `MapPath`, `NormalizeURL`. |
| `internal/integrations/plex` | — | `Client` (`Identity`, `Sections`, `Prefs` + `InButlerWindow`, `Test`), `plextest` fake server, `testdata/` (recorded). |
| `internal/catalog` | `sources`, `catalog_files` | `Store` (sources CRUD + validation, files query, stats, `TestSource`, `LockSource`, `Live`, `Deleted`), `Scanner` (`Scan`, `ScanLocked`), `ScanRunner`, `ErrScanRefused`. |
| `internal/destinations` | `destinations`, `destination_sources` | `Store` CRUD + validation, `Probe`, `Test`, `Create` (marker), `Open(ctx, id) (*Handle, error)` = checks S3 and returns the `os.Root`; `Handle.Recheck`. `Delete` also removes the destination's `destination_files` rows, dependents first (`link_of` is RESTRICT, checked row by row). |
| `internal/engines/filecopy` | — | Filesystem primitives over `*os.Root`: `WriteTemp`/`Commit` (S7), link, move, retain, expire, adopt check, head/tail hash, full hash, verify (fadvise), temp cleanup, `WriteFileAtomic`, `RenameDir`, free space, `NameCheck`/`FoldKey`, `Classify`. |
| `internal/syncer` | `destination_files` | `Planner`, runners `sync`, `verify`, `retention`; guards S10(b), S11; retention intents and reconciliation (`reconcile.go`); the hardlink manifest (`LinkManifestRel`); `Store.HasBackups` (for catalog). |
| `internal/plexdb` | `snapshots` | runner `plexdb_backup`, `Backup`, `Verify`, `Inspect`, version pruning, `Store`; private SQLite driver. |
| `internal/notify` | `notifications` | `Store`, Apprise client, `Dispatcher` (`jobs.Manager.OnFinish`), `Test`. |
| `internal/jobs` | — | The contract only (`contract.go`): types and interfaces shared by runners. Stable; no implementation. |
| `internal/jobqueue` | `jobs`, `job_items`, `job_logs`, `schedules` | `Manager` (`New`), `Scheduler` (`NewScheduler`, robfig/cron), `Store`, `ValidateCron`; implements the `jobs` contract. |
| `internal/logging` | — | + `RegisterSecret`, `SetSecrets`, `RedactSecrets`, `RedactValues`, `SensitiveKey`. |
| `internal/faultinject` | — | `Point(name)`, `SetHook`, `CrashAt`, `InitFromEnv`. |
| `internal/api` + `cmd/bunkarr` | — | Handlers (§7), openapi.json; `api.App` (app.go) holds the wiring used by `cmd/bunkarr` and the API tests alike (stores, runners, S4 guards, scheduler gate, notifications). |
| `internal/e2e` | — | Acceptance suite (§9), build tag `e2e`. |
| `web/` | — | UI (§10). |

Schema notes (`0002_phase1.sql`): `integrations`, `sources`, `destinations` and `notifications`
ids are `AUTOINCREMENT` (they appear in job params, schedules, snapshots and group ids; a deleted
id is never reused); `sources` has `root_ino`, `stats` and `scan_seq`; `destinations.marker_id`
is UNIQUE; `destination_files.reason` (on a live row, `retained_path` + `reason` are its
retention intent, §4.2); `snapshots (destination_id, engine_snapshot_id)` is
UNIQUE (a version directory is recorded at most once). The database writer runs with
`synchronous=FULL`, readers use a separate read-only pool.

## 9. Tests (required)

Unit (every package): hardlink grouping incl. disagreeing members and inode reuse; incremental
scan (add/change/delete/rename, unreadable subdir, EIO → fatal, empty root → fatal, anonymous
device renumbering); planner for every action incl. move vs copy+retain, promote, link fallback,
displaced unmanaged files, case collision, invalid names, guard holding; retention math incl.
defaults; cron validation; value redaction; marker/fs-type checks; overlap validation; Plex
client against `testdata/`; the API's raw responses never contain a token or Apprise URL.
Revision 4 adds: retention intents settled by the item and by the next job (a retain renamed by a
stopped job, a displaced file of a deleted source, a replacement a stopped job completed, a file
in the way of it that is not trusted, damaged versions never retained with a hash, a retain
settled by another job recorded once); the retain that waits for an upgrade's new file
(`TestRetainWaitsForTheNewVersionInItsFolder`: copy fails, name not storable, then retained once
backed up); the link manifest (content, unchanged manifest not rewritten, a leading `#`
escaped); a source path change after a remount (`TestUpdatePathAfterRemount`); `TokenFor` bound
to the stored URL; the retention preview end to end through the API
(`TestRunScheduleDryRunPreviewsRetention`: the dry run lists the expiry and deletes nothing, the
real run then expires it) and `RunNow` dry runs as separate, unrecorded jobs.

Crash matrix (Go, every push, no Docker): the filecopy, catalog, syncer, plexdb and jobqueue code
call `faultinject.Point("name")` at each step boundary (`internal/faultinject`: a single atomic
load in production; tests install `faultinject.CrashAt(point, n)`, which panics with
`faultinject.Crash`). `TestCrashMatrix` runs a sync against a fixture tree, counts how often each
point in its list is reached, "crashes" at every occurrence (panic recovered by the harness,
process state discarded), re-runs the job as a resume, and asserts: the destination verifies, no
partial final file, no temp file left, records match the destination, no item left pending,
every replaced or deleted version in retention, and a further sync plans nothing; a point that
is never reached fails the test. `-short` runs the first and last occurrence of each point.
`TestCrashMatrixResumePaths` runs the same matrix over resume scenarios: unmanaged files at
move/link/promote targets, missing and damaged primaries with recorded links, whole hardlink
groups vanishing and coming back, a source changed between crash and resume. Other tests cover a
job stopped by a fatal error (salvaged, then a fresh sync), a verify resumed between its two
marks, and plexdb crashes around the snapshot insert and inside pruning (its own matrices,
also through the manager). Points: `scan.afterBatch`, `plan.afterBatch`,
`copy.beforeTemp|afterWrite|afterSync|afterChtimes|afterRename`,
`update.afterLinkOld|afterRenameOld|afterRenameNew`, `link.afterLink`, `move.afterRename`,
`displace.afterRename`, `retain.afterRename`, `promote.afterRename`, `adopt.afterSetMtime`,
`record.afterFS|afterDB`, `verify.afterMark`, `expire.afterRemove`, `plexdb.afterStage|afterCopy|
beforeRename|afterRename|beforeRecord|afterRecord|pruneAfterTrash|pruneAfterUnrecord|
pruneAfterRemove`, `jobqueue.started|beforeFinish` (a runner panic with `faultinject.Crash` is a
simulated process crash: the row stays `running` and the next manager's start recovers it; both
jobqueue points are armed by its tests, `started` before the runner is called: the job resumes as
attempt 2).

E2E (`internal/e2e`, build tag `e2e`, `make test-e2e`, real binary built by the test, driven over
HTTP, fixture trees with hardlinks, Unicode and punctuation names generated in temp dirs at test
time): (1) full sync then a second sync with 1 changed, 1 added, 1 deleted, 1 renamed file:
second job stats `filesCopied=1, filesUpdated=1, filesMoved=1, filesRetained=1` and bytes equal to
those files; (2) hardlinked pair: copied once, `bytesPlanned`/`uniqueBytes` equal unique bytes;
(3) kill -9 while paused at a fault point (env `BUNKARR_FAULTPOINT=<point>`,
`BUNKARR_FAULTPOINT_FILE=<path>`: the process writes the file and blocks at that point), restart,
the same job resumes (attempt 2) and the destination verifies — at `copy.afterWrite`,
`update.afterLinkOld`, `update.afterRenameOld` (the destination's `hardlinks` capability set to
false, as on SMB without Unix extensions) and `plan.afterBatch` (1050 files, two batches);
(4) destination marker removed → sync fails and writes nothing; (5) empty source → scan fails,
nothing retained; (6) dry run writes nothing (directory mtimes included); (7) mass-change guard:
23 of 220 files deleted are held, the new file is copied, one warning notification reaches a
fake Apprise, `allowChanges` applies them. The sync and kill tests snapshot the source tree
(content, mode, mtime, ctime, owner, inode, nlink) before every job and require it unchanged
after it, apart from the test's own edits (S1; `TestSnapshotSeesMetadataChanges` proves the
snapshot catches a chmod, utimes or chown that was put back).

Docker suite (`make test-docker`: image smoke test, container kill test, Plex restore test,
share test; `make test-plex` and `make test-shares` run one each; drivers are Go tests in
`internal/e2e` gated by `BUNKARR_E2E_IMAGE`/`BUNKARR_E2E_PLEX_IMAGE`/`BUNKARR_E2E_SHARES`, so the
suite needs Go and Docker). Fault injection is always compiled in and armed only by the
environment, so the normal image serves the kill test (the container command clears
`BUNKARR_FAULTPOINT` once the signal file exists, so `docker start` restarts normally). Plex restore test per the spike procedure: pinned `plexinc/pms-docker`
digest, 60 fake movies, 3 backups from an `:ro` mount under a scrobble/refresh load, every
version passes Bunkarr's verification and Plex SQLite `integrity_check`, matches its manifest and
is a consistent snapshot; the newest is restored into a second container that serves the same
sections, movie count and watched count. Before the backups the library side runs against the
same PMS: `GET /integrations/{id}/plex/sections` with the mapping `/data` → `/media` lists the
Movies location as `/media/movies` (exists), a source created from it keeps its Plex fields, and a
sync of it delivers every movie file. Plex is reached by IP: an unclaimed PMS answers 401 when
the Host header is its container name, even from an `allowedNetworks` subnet (so the token path
of a claimed server is not covered here; see DEFERRED.md). **Share test** (`TestDockerShares`,
`docker/test-shares.sh`): `TestSyncLifecycle` and `TestKillResume` run with every destination on
a real network share, as run by Unraid's user 99:100 in a privileged container of the image plus
cifs-utils and nfs-utils: a Samba server mounted over CIFS with Unassigned Devices' options
(`nounix,noserverino,vers=3.1.1,file_mode=0777`) and the kernel NFS server over NFSv4.

CI runs lint, the race tests, the e2e suite, the image smoke, kill and share tests on every push;
the Plex job on `workflow_dispatch` and on tags. The release workflow builds the multi-arch image
once as a candidate, runs the e2e suite and the whole Docker suite against that image (by digest,
on amd64 and arm64), and only when all pass puts the release tags on the same digest.

## 10. UI
- Activity → **Queue** (`/activity/queue`): active jobs, progress bar, bytes, throughput, ETA,
  phase, current file, cancel (a queued job is only taken off the queue); polls every 2 s.
- Activity → **History** (`/activity/history`): finished jobs, filters (type, status, in the URL),
  paging.
- **Job detail** (`/activity/jobs/:id`): summary, stats (a figure grid plus a per-source table
  with skip reasons and scan warnings; dry-run wording "To copy", "Would be held"), item summary
  by action/status, items (filters; this is the dry-run preview, with "Run this sync"; held items
  with "Apply held changes", which confirms, starts a sync with `allowChanges` and opens it),
  logs (incremental by `afterId`; at most 2000 lines kept while live, earlier ones on demand).
- **Library** (`/library`): catalog stats, sources with stats, add/edit (path picker + test),
  delete, "Import from Plex" (sections → locations → local path via path mappings, with the
  reason a folder cannot be imported), Scan; `/library/sources/:id` file browser.
- **Destinations** (`/destinations`): list with capability badges, schedules, last sync and last
  job; add/edit (path picker; a Test is required before create and shows capability/warnings;
  attach/local confirmations; sources, sync and verify schedules with presets, verify mode,
  hardlinks, adoption, guard limits, retention with the server's ranges); Test, Preview (dry
  run), Sync now, Verify, snapshots.
- Settings → **Plex** (`/settings/plex`): servers (URL, write-only token, data path, path
  mappings), Test (its result applies only to the URL and token it tested), DB backup
  (destination, schedule, butler-window warning), Back up now.
- Settings → **Connect** (`/settings/connect`): Apprise targets (stateless URLs or stateful key),
  events, Test.
- System → **Tasks** (`/system/tasks`): schedules with description, next/last run (or why the
  schedule is blocked), enable, edit cron, Run now, **Preview** (a dry run through
  `POST /schedules/{id}/run {dryRun: true}`, then opens the job; the retention preview's log links
  each destination's dry run, whose items are the files a run would delete).
- The left navigation becomes a drawer at phone widths; no page scrolls sideways at 375 px.

## 11. Deployment notes (docs)
- The compose example takes every host path and PUID/PGID/TZ from `deploy/.env` with `${VAR:?}`
  guards (Compose refuses to start without them, so Docker never creates empty directories at
  guessed paths); `/config` is `${BUNKARR_CONFIG:-/mnt/user/appdata/bunkarr}`, an absolute path
  outside the checkout (a `./config` next to the compose file would enter the build context).
- Mount the media parent read-only as ONE volume (hardlink detection); prefer a pool/disk path or
  enable Unraid's hard-link support tunable for `/mnt/user`.
- Unassigned Devices shares are mounted after Docker starts: map them with `rw,slave` propagation
  (`/mnt/remotes/UNAS:/backup:rw,slave`), otherwise the container sees an empty mountpoint.
- Plex appdata: `/mnt/user/appdata/plex/Library/Application Support/Plex Media Server:/plex:ro`;
  PUID must match Plex's user (Preferences.xml is 0600). Run Bunkarr in Plex's TZ.
- Plex URL: use the server's IP. A claimed server with a token also works by container name
  (token auth does not depend on the Host header); an unclaimed one answers 401 to a name it
  does not recognise. `allowedNetworks` matters only for clients PMS classifies as WAN.
- SMB without POSIX extensions is case-insensitive and rejects `: ? * " < > | \`; NFS avoids both.
  It also takes file modes from the mount options (Unassigned Devices: `file_mode=0777`), so the
  0600 of each Plex version's `Preferences.xml`, which holds `PlexOnlineToken`, is not enforced:
  the README says so and recommends restricting read access to the share (DEFERRED.md: an option
  to skip or seal the file).
- A restore without Bunkarr recreates `link_recorded` names from `.bunkarr/links.tsv` (README
  "Restoring").
- Give the container time to stop (`stop_grace_period` ≥ 45 s): a killed job resumes too, but
  three crashes in a row fail it.
