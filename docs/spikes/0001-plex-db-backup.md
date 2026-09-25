# Spike 0001: backing up the live Plex database (Phase 1, §7.2)

Goal: decide how Bunkarr takes a consistent, verifiable copy of
`com.plexapp.plugins.library.db` (and `...blobs.db`) while Plex is running, without ever copying
the raw live files.

## Questions

1. Can `modernc.org/sqlite` open Plex's database at all? Plex ships its own SQLite build with
   extra FTS tokenizers/collations (the ICU-based `collating` tokenizer and related indexes).
   Opening should work; statements touching those virtual tables may fail with "no such
   tokenizer"/"no such collation sequence".
2. Does the **online backup API** (`sqlite3_backup_*`, exposed by modernc's connection
   `NewBackup`) produce a consistent copy while Plex writes? It copies pages, so it should not
   need Plex's extensions.
3. Does **`PRAGMA integrity_check`** pass on the copy without Plex's extensions? If indexes use a
   custom collation, integrity_check may error on them. Fallbacks: `PRAGMA quick_check`, register
   a stub collation for verification only, or run Plex's own "Plex SQLite" binary from the Plex
   container for verification.
4. **Read-only access**: Plex's DB is in WAL mode. A read-only connection to a WAL database needs
   the `-shm` file to be writable (or `immutable=1`, which is unsafe on a live database). If the
   Plex appdata is mounted read-only into Bunkarr, the backup API may fail to open. Measure: RW
   mount + `mode=ro` connection vs RO mount.
5. **Plex's own scheduled backups** (`Plug-in Support/Databases/*.db-YYYY-MM-DD`): always
   consistent and safe to copy from a read-only mount, but up to 3 days old and dependent on the
   user having "Backup database every three days" enabled.
6. Lock impact: how long does a full online backup hold a read transaction on a large (1–5 GB)
   library, and does Plex log `database is locked` meanwhile?

## Method

1. A test harness (Go, build tag `spike`, not shipped) running against a scratch Plex container
   (`plexinc/pms-docker`) with a generated library of a few thousand fake items, plus a copy of a
   real database when available.
2. While a load script triggers library scans and watch-state changes through the Plex API, run:
   (a) online backup via a `mode=ro` connection, (b) the same on a read-only bind mount,
   (c) copying Plex's newest scheduled backup.
3. For each result: `integrity_check` (with and without a stub collation), `quick_check`, row
   counts of `metadata_items`, `media_parts`, `metadata_item_settings`; then **restore it into a
   second scratch Plex container** and check it starts, lists the library and keeps watch state.
4. Record timings, file sizes, Plex log errors, and the SQLite version modernc embeds vs Plex's.

## Decision rule

- Prefer (a) online backup if it is consistent, restores cleanly and verifies, and document that
  Plex's appdata must be mounted read-write for it (Bunkarr opens it read-only at the SQLite
  level and never writes Plex files).
- If (a) needs a RW mount the user is not willing to give, or verification cannot be made
  reliable, use (c) Plex's scheduled backups with a freshness check and a warning when the newest
  backup is older than the configured limit.
- Preferences.xml (and other small config files) are plain files: copy them with a size + hash
  check; they are rewritten atomically by Plex.

The result becomes ADR 0005 and the Phase 1 implementation.

## Results

Run 2026-09-25. Decision: [ADR 0005](../adr/0005-plex-db-backup-method.md).

### Setup

- Docker Desktop 29.7.2 on an arm64 Mac, with bind mounts going through virtiofs.
  `plexinc/pms-docker:latest` (arm64, digest `sha256:e0ab2739…`): PMS 1.43.4.10903-e5521bd8c,
  whose bundled `Plex SQLite` is 3.53.3. The harness is a Go program built for linux/arm64
  (static, `modernc.org/sqlite` v1.59.0, which embeds SQLite **3.53.4**). It ran inside
  `alpine:3.22` helper containers against the same bind-mounted Plex `/config`, never from the
  macOS host. It lived in a scratch directory and was not committed.
- Scratch server A: unclaimed and never signed in. It had a Movies section (`/data/movies`) and
  a TV section with two locations (`/data/tv`, `/data/tv-kids`), filled with tiny fake `.mkv`
  files. The Movies section grew to 1837 items: 300 generated titles, 38 titles with Unicode or
  punctuation (Amélie, Ødegaard, Straße, 千と千尋の神隠し, 기생충, `#Alive`, `'71`, emoji…) and
  1500 added under load. The final database was 12 MB, **`page_size` 1024**, WAL mode.
- API access without a token: an unclaimed PMS answers `/identity` to anyone, but other
  endpoints return `401` to the Docker gateway address (`192.168.65.1`, which PMS logs as
  "WAN"). Setting `allowedNetworks="0.0.0.0/0.0.0.0"` in `Preferences.xml` (with PMS stopped)
  did **not** help. Explicit subnets did:
  `allowedNetworks="192.168.65.0/255.255.255.0,172.16.0.0/255.240.0.0"`. The first try also set
  `LanNetworksBandwidth` to the same value; server B needed only `allowedNetworks`. After that,
  PMS logs requests as "Allowed Network (WAN)" and serves them.
  `curl http://127.0.0.1:32400/...` inside the container also works unclaimed. Sections were
  created with
  `POST /library/sections?name=Movies&type=movie&agent=tv.plex.agents.none&scanner=Plex%20Movie&language=xn&location=/data/movies`
  (response: 201 with `Location: /library/sections/1`).
- Recorded API responses were saved to `internal/integrations/plex/testdata/`: `identity.json`,
  `sections.json` (the multi-location TV section is useful) and `unauthorized.txt`. A `401` has
  `Content-Type: text/html` even when `Accept: application/json` is sent, and a bogus
  `X-Plex-Token` gets the same response. The client must not try to decode a 401 body as JSON.
- Write load ran for 19 minutes (`load.sh`), with three loops against PMS:
  - a refresh of both sections every 2 s (560 refreshes);
  - 20 new movie folders every 3 s, up to 1500;
  - `/:/scrobble` over all movies in ascending id order, then `/:/unscrobble` in descending
    order, one call (one PMS transaction) per item: 114,631 calls, about 100 per second.

  Because of that order, a **consistent snapshot** reads, in movie-id order, as watched flags
  with **at most one transition**. The harness checks that on every copy, together with
  `quick_check`/`integrity_check`, row counts and orphan checks
  (`media_items→metadata_items`, `media_parts→media_items`).

### Q1: modernc and Plex's schema

`modernc` opens the live database with `file:…?mode=ro` and reads ordinary tables normally
(`journal_mode` = `wal`). Plex's extras:

- the collation `icu_root`, used by one index:
  `index_title_sort_icu ON metadata_items(title_sort COLLATE icu_root)`;
- FTS4 tables `fts4_metadata_titles[_icu]` and `fts4_tag_titles[_icu]`, the `_icu` ones using
  `tokenize=collating 'root@colStrength=primary;colAlternate=shifted'`;
- `spellfix1` tables (`spellfix_metadata_titles`, `spellfix_tag_titles`) and an `rtree`
  (`locations`).

`modernc` has neither the `fts4` nor the `spellfix1` module: selecting from those tables gives
`SQL logic error: no such module: fts4 (1)`. A backup never needs to query them.

### Q2: online backup under write load

- **`Step(-1)` (whole database in one step) passed every time.** 46 online backups ran under
  load: 28 on a read-write mount and 18 on a read-only mount, all with `mode=ro`. Every one
  passed `quick_check` and the filtered `integrity_check` (see Q3), had no orphans, and passed
  the one-transition snapshot test. `Plex SQLite` `PRAGMA integrity_check` returned `ok` on
  every sampled copy. The PMS database grew from 1.2 MB to 7.3 MB during the run. Durations:
  37–82 ms at 1.2 MB and 190–570 ms at 5–7 MB. `restarts=0`.
- **Incremental steps do not work against a live PMS.** `Step(50)` with 50 ms sleeps, and
  `Step(200)` with 10 ms sleeps, never finished. After 30 s they had 242 and 421 restarts,
  because every PMS commit from another process restarts the backup. Use `Step(-1)` only.
- **Scale test** on a synthetic WAL database in the same setup: a helper container wrote one
  small transaction every 10 ms throughout.
  - 1.2 GB with 1 KiB pages (1.18 M pages): **28 s** on both rw and ro mounts, about 42 MB/s or
    24 µs per page.
  - 1 GB with 4 KiB pages: 6–8.5 s.
  - The writer saw 0 errors: p50 0.25 ms, p99 0.7 ms, max 51–60 ms.
  - Estimate for a 5 GB Plex database with 1 KiB pages: about 2 minutes. `PRAGMA
    integrity_check` on the 1.2 GB copy took about 12 s.
- **Contrast: copying raw files.** 12 plain `cp` copies taken under load passed
  `integrity_check` at this small size. The two `.db`-only copies that were compared were
  **silently stale**, though: they lacked 21 and 23 committed watch changes that were still in
  the WAL. Also, merely opening a raw copy with a read-write SQLite checkpoints its `-wal` and
  deletes it. A `mode=ro&immutable=1` read of the live file on a read-only mount produced a
  **corrupt** copy (see Q4).

### Q3: verifying the copy without Plex's extensions

- Without the collation, both checks fail at once: `PRAGMA integrity_check` and `PRAGMA
  quick_check` → `SQL logic error: no such collation sequence: icu_root (257)`.
- With any stub collation registered (`sqlite.RegisterCollationUtf8("icu_root", …)`),
  `quick_check` returns `ok`. It does not compare index order, so the stub's ordering does not
  matter.
- `integrity_check` with a stub reports **false** `row N missing from index
  index_title_sort_icu` errors:
  - The `binary`, `nocase` and Go `x/text/collate` root stubs gave 100 errors each (the default
    error cap).
  - Plex's `icu_root` orders digits **numerically**: "Spike Movie 9" sorts before "Spike Movie
    10". A `collate.New(language.Und, collate.Numeric)` stub passed on an ASCII-only library.
    It still failed on one Unicode title: Plex stores the sort title of "Ødegaard Story" as
    `0degaard Story`. `Plex SQLite` said that same copy was `ok`.
  - Conclusion: no Go collation can stand in exactly for `icu_root`.
- **Filtered integrity check.** Register a stub, run `PRAGMA integrity_check(100000000)`, and
  drop only the lines that match `^row \d+ missing from index (\S+)$` where the index named is
  declared with a non-built-in collation. Any other line is a failure. Raising the error cap
  keeps the ignored lines from crowding out real errors.
  - It returned 0 kept errors on every good copy (up to 1233 ignored lines).
  - It caught a page overwritten with random bytes: `Tree 199 page 501 cell 94: Offset 18055
    out of range 289..1020` (`quick_check` failed too).
  - It caught a truncated file: `database disk image is malformed (11)`.
  - It caught the torn `immutable` copy: `wrong # of entries in index …`. That error does not
    depend on the collation and is never filtered.
  - Not covered: the order of `index_title_sort_icu` and the internal consistency of FTS4. Their
    shadow tables are still checked as b-trees, and the virtual tables themselves report `ok`.
    Plex can rebuild either with `REINDEX` or its repair tool.
- Open the copy with `mode=ro&immutable=1`. It is Bunkarr's private, unchanging file. The online
  backup keeps the WAL flag in the header (bytes 18/19 = 2), so a plain `mode=ro` open creates
  `-wal`/`-shm` files next to the copy.
- **Ground truth for tests:** the `Plex SQLite` shipped in the PMS image, run as
  `docker run --rm --entrypoint "/usr/lib/plexmediaserver/Plex SQLite" -v <dir>:/v
  plexinc/pms-docker /v/<copy>.db "PRAGMA integrity_check;"`. It is Plex's binary and cannot
  ship in Bunkarr.

### Q4: read-only mount and connection flags

"ro"/"rw" is the bind mount of Plex's `/config` into the helper. The flags are the URI query of
Bunkarr's source connection.

| Mount | PMS state | Flags | Result |
|---|---|---|---|
| rw | running, under load | `mode=ro` | OK, consistent (28/28) |
| ro | running, under load | `mode=ro` | **OK, consistent (18/18).** SQLite ≥ 3.22 reads through a read-only `-shm` while PMS holds it; first run 934 ms, later runs 200–570 ms |
| ro | running | `_pragma=query_only(1)` (no `mode=ro`) | OK: SQLite falls back to a read-only open on the read-only mount |
| rw | running | `_pragma=query_only(1)` (no `mode=ro`) | OK, but the connection can write at the VFS level (it checkpoints on close). Rejected |
| ro | running, under load | `mode=ro&immutable=1` | **Corrupt copy**: `quick_check` gives `wrong # of entries in index …` (35 errors). It ignores the WAL and all locking |
| rw | running | `mode=ro&immutable=1` | Passed once by luck; same hazard |
| ro | stopped cleanly (no `-wal`/`-shm`) | `mode=ro` | **Fails**: `unable to open database file (14)` (SQLITE_CANTOPEN; it cannot create `-shm`) |
| ro | stopped cleanly | `mode=ro&immutable=1` | OK, **byte-identical** to the rw `mode=ro` backup (same sha256) |
| rw | stopped cleanly | `mode=ro` | OK, but it **creates** `…library.db-wal` (0 B) and `…library.db-shm` (32 KiB) in PMS's Databases folder and leaves them there. PMS (same UID) started normally with them |
| ro | killed with SIGKILL (1 MB uncheckpointed `-wal`) | `mode=ro` | **OK and complete**: it includes the committed WAL transactions (100/100 watch changes), using a heap-memory wal-index |
| ro | killed | `mode=ro&immutable=1` | **Silently stale**: 43 of the 100 committed watch changes are missing, yet `integrity_check` passes |
| rw | killed | `mode=ro` | OK, same sha256 as the ro `mode=ro` copy |
| ro | PMS dated backup file | `mode=ro` | `unable to open database file (14)` (WAL header, no `-shm`) |
| ro | PMS dated backup file | `mode=ro&immutable=1` | OK |

So a **read-only mount is enough**. Use `mode=ro` whenever a `-wal` file exists (PMS running or
crashed). Use `mode=ro&immutable=1` only when no `-wal` exists (PMS stopped cleanly, or a
finished dated backup), and guard it by `stat`-ing the file before and after the copy. File
modes: the database files are `0644`, but PMS rewrites `Preferences.xml` as **`0600`** by
atomic replace (the inode changes on every pref change). Bunkarr must therefore run as PMS's UID
(PUID) to read it.

### Q5: Plex's own scheduled backups

- Controlled by `ButlerTaskBackupDatabase` (default `true`, "Backup database every three
  days"). They are written to `ButlerDatabaseBackupPath` (default `/config/Library/Application
  Support/Plex Media Server/Plug-in Support/Databases`), inside the butler window
  `ButlerStartHour`–`ButlerEndHour` (default 2–5, server-local time). They can be triggered
  with `POST /butler/BackupDatabase`. All of these prefs can be read from `GET /:/prefs`.
- Names are `com.plexapp.plugins.library.db-YYYY-MM-DD` and
  `com.plexapp.plugins.library.blobs.db-YYYY-MM-DD`, using the server-local date (the container
  had `TZ=UTC`). They have a WAL header and no `-wal`/`-shm` files. `Plex SQLite` and the
  filtered check both returned `ok`.
- PMS keeps the **4 newest** per database. With fake older files dated 08-20 … 09-22 present,
  a run kept 09-16, 09-19, 09-22 and 09-25 and deleted the rest. A second run on the same day
  **overwrites the same name in place**. There is no temp name: the log shows `Beginning
  database backup` … `Database backup completed: 0`.
- While backing up, PMS logs `Captured session 0..19`, meaning it takes all of its own
  connections. For a 5.7 MB database this lasted 110 ms.
- Verdict: a finished dated file is safe to copy (take it only when its date is before today
  or it has been unchanged for several minutes, then verify it). It is up to 3 days old, absent
  when the user disabled the task, and may be mid-write inside the butler window. It is a
  fallback, not the primary method.

### Q6: lock impact

- No `database is locked`, `SQLITE_BUSY` or `busy` lines appeared in `Plex Media Server.log`
  in any captured window. The logs rotate at 10 MB × 5 and DEBUG logging under load filled
  them in about 3.5 min, so the first rw run (15 backups) was not covered. Coverage from then
  on was complete via `tail -F`: the ro loops, the later rw loops, the raw copies, a 90 s held
  read transaction, `BackupDatabase` and `OptimizeDatabase`.
- "Held transaction for too long (0.11–0.21 s)" warnings came from PMS's own write load and
  appeared before any backup ran too. All 59 `database schema has changed` INFO lines came from
  `OptimizeDatabase`.
- With a read transaction held for 90 s (`mode=ro`, rw mount) under full load, plus
  `BackupDatabase` and `OptimizeDatabase` triggered during it:
  - API scrobble latency was p50 2.6 ms (baseline 2.7 ms), max 78 ms (baseline 5.8 ms).
  - **the `-wal` grew from 2 MB to 245 MB**: checkpoints cannot pass a live reader, and
    `OptimizeDatabase` rewrites the whole database into the WAL. The file stays that size until
    PMS truncates it or restarts. Normal nightly activity writes far less.
- Readers never block PMS writers in WAL mode. The cost of a long backup is WAL growth, so
  Bunkarr should avoid the butler window.

### Restore test

1. Scratch server B (`config-b`) was started fresh with `-e PLEX_UID/PLEX_GID` and the same
   `/data` mounted read-only, then stopped with `docker stop -t 60` (a clean stop removes
   `-wal`/`-shm`).
2. B's fresh `com.plexapp.plugins.library.db` and `…blobs.db` were moved aside, along with any
   `-wal`/`-shm`. A stale `-wal` must never sit next to a restored database, because SQLite
   would replay it.
3. The online backup taken on the rw mount under load (`rw-final-8`, sha256 `2447b4fa…`; the
   copy passed `quick_check` and the filtered check) was copied in as
   `com.plexapp.plugins.library.db`. The online backup of the blobs database went in as
   `…blobs.db`. Mode 0644, owned by PMS's UID.
4. `allowedNetworks` was added to B's own `Preferences.xml` so the test could call the API.
   A's `Preferences.xml` was not restored, which would clone A's identity.
5. `docker start`. Result: `GET /library/sections` → Movies (`/data/movies`) and TV Shows
   (`/data/tv`, `/data/tv-kids`); 1837 movies and 6 shows; **502 watched**, exactly the
   snapshot in the copy; part paths resolved (e.g. `/data/movies/Amélie (2001)/Amélie
   (2001).mkv`). PMS logged no errors. `Plex SQLite` `integrity_check` on the running
   database → `ok`.
