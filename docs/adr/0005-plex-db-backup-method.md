# 0005. Plex database backup method

- Status: accepted
- Date: 2026-09-25
- Updated: 2026-09-25, after the Phase 1 implementation and acceptance suite (collation stubs on a
  private driver, folder names, Plex API access in tests)

## Context

Phase 1 must back up Plex's `com.plexapp.plugins.library.db`, `…library.blobs.db` and
`Preferences.xml` while Plex Media Server (PMS) runs. It must never copy the live database files
as raw files, and the copy must pass `integrity_check` (acceptance 4). Spike 0001 tested this
against PMS 1.43.4 (whose `Plex SQLite` is 3.53.3) with `modernc.org/sqlite` v1.59.0 (SQLite
3.53.4). Findings:

- The online backup API makes a consistent copy while PMS writes. 46 of 46 copies were good,
  taken under about 100 PMS commits per second, on both read-write and read-only mounts.
- Incremental `Step(n)` never finishes against a live PMS, because every PMS commit restarts it.
- The spike's copy restored into a fresh PMS with its sections, items and watch state intact.
- A raw `.db` copy silently loses the transactions still in the WAL. `immutable=1` on the live
  file produced a corrupt copy once and a silently stale one once.
- Plex's schema uses the custom collation `icu_root` (one index), FTS4 tables with Plex's
  `collating` tokenizer, and `spellfix1` tables. `modernc` has none of these.
  - Without the collation, `integrity_check` and `quick_check` both fail with
    `no such collation sequence: icu_root`.
  - No Go collation reproduces `icu_root` exactly. It sorts digits numerically, and Plex's sort
    titles can differ from the displayed titles, such as `0degaard`.
- A read-only mount works when a `-wal` file exists, whether PMS is running or crashed.
- A read-only mount fails with `SQLITE_CANTOPEN` when PMS stopped cleanly. In that case a
  read-write mount with `mode=ro` leaves `-wal`/`-shm` files in Plex's folder.
- Plex's own dated backups exist only every 3 days (4 kept). They are written in place during
  the butler window.

## Decision

- **Method: SQLite online backup, one step.**
  - Source connection: `file:<dataPath>/Plug-in Support/Databases/<db>?mode=ro` plus
    `PRAGMA busy_timeout=5000`. Reach the driver through `sql.Conn.Raw`, then call
    `NewBackup(stagingPath)` and a single `Step(-1)`.
  - Never use incremental steps, a read-write connection, or `immutable=1` while `<db>-wal` (or
    a rollback `<db>-journal`) exists.
  - If `<db>-wal` does not exist, PMS is stopped cleanly and the file is self-contained. Open it
    with `mode=ro&immutable=1`, and compare the file's size, mtime and inode before and after the
    copy. If anything changed or a `-wal` appeared, discard the copy and retry, at most 3 times.
  - Apply this to both `library.db` and `library.blobs.db`.
- **Staging.**
  - The backup API writes to local staging, `/config/staging/plexdb-job<jobID>/`, never
    directly over SMB/NFS: temp name, fsync, rename.
  - Verify the file there. The `filecopy` engine then copies it to
    `<target>/.bunkarr/plex/<slug>-<integrationId>/<ts>/` with size and sha256 checks (S7), next
    to `manifest.json`. Then remove the staging directory. The integration id in the folder name
    keeps two servers with similar names apart and survives a rename.
  - `Preferences.xml` is a plain file that PMS replaces atomically. Copy it with size and sha256,
    and read it twice to confirm it did not change.
- **Verification (`plexdb.Verify`).**
  - Open the staged copy with `mode=ro&immutable=1`, so no side files are created.
  - Register a binary-compare stub for every non-built-in collation named in a `COLLATE` clause
    or an index key (`pragma_index_xinfo`). Today that is only `icu_root`. Register each name
    once per process, with `(*sqlite.Driver).RegisterCollationUtf8` on a private modernc driver
    that `internal/plexdb` owns and uses for Plex's databases and the copies, never with the
    package-level `sqlite.RegisterCollationUtf8` on the shared `sqlite` driver (see
    Consequences).
  - `PRAGMA quick_check` must return `ok`.
  - Run `PRAGMA integrity_check(100000000)`. Ignore only lines matching
    `row N missing from index <idx>` where `<idx>` is declared with one of those custom
    collations. Any other line fails the job.
  - `manifest.integrity` records `quickCheck`, `integrityCheck`, `ignoredLines`,
    `ignoredIndexes`, the SQLite version, and row counts of `metadata_items` and
    `media_parts`.
- **Mounts.** Mount Plex's appdata **read-only** (`…/Plex Media Server:/plex:ro`). This is the
  documented default and is sufficient; a read-write mount also works. Bunkarr's PUID must
  equal Plex's UID, because PMS writes `Preferences.xml` as `0600`.
- **Schedule.**
  - The default Plex DB backup time is outside PMS's butler window (`ButlerStartHour`–
    `ButlerEndHour`, default 02–05, read from `GET /:/prefs`).
  - The UI warns when a schedule overlaps that window, because `OptimizeDatabase` during a
    held snapshot makes PMS's WAL grow.
- **Plex's scheduled backups** (`<db>-YYYY-MM-DD`, `ButlerTaskBackupDatabase`,
  `ButlerDatabaseBackupPath`) are not used by the job in Phase 1. The restore documentation
  mentions them as a manual fallback.
- **Restore procedure** (README and test):
  1. Stop PMS.
  2. Move aside `library.db` and `library.blobs.db`, and delete their `-wal`/`-shm` files. A
     stale WAL would be replayed onto the restored database.
  3. Copy in the backup files, owned by Plex's UID with mode 0644.
  4. Restore `Preferences.xml` only when restoring the same server.
  5. Start PMS.
- **Tests.** The slow Docker suite (acceptance 4) does the following:
  - pins `plexinc/pms-docker` by digest and reaches PMS by its IP on the test network, with an
    explicit-subnet `allowedNetworks`. `allowedNetworks` only matters when PMS classifies the
    client as WAN (such as the Docker Desktop host gateway, `192.168.65.1`). For clients on the
    same Docker network the Host header decides: an unclaimed PMS answers `401` when the Host is
    its container name ("unrecognized domain / IP ... treating as non-local"), even from an
    allowed subnet, and serves requests addressed to its IP. A claimed server with a token works
    by name, because token authentication does not depend on the Host header;
  - runs `plexdb.Backup` against a `:ro` mount while a write-load loop runs;
  - checks the copy with `plexdb.Verify` and with the image's
    `/usr/lib/plexmediaserver/Plex SQLite` (`PRAGMA integrity_check` must be `ok`);
  - restores the copy into a second container and asserts sections, item counts and the watched
    count.

## Consequences

- Works while PMS is running, stopped or crashed, from a read-only mount, and never writes to
  Plex's files.
- PMS writers are never blocked: the spike saw no `database is locked` and no change in median
  API latency.
- The cost of a backup is WAL growth while the snapshot is held. PMS's `-wal` grew from 2 MB to
  245 MB during a 90 s read, under full load plus `OptimizeDatabase`.
- Throughput is about 24 µs per 1 KiB page: about 2 minutes to back up a 5 GB library database,
  plus about 1 minute to verify.
- Staging needs free space in `/config` equal to the database size.
- Verification cannot check the order of `index_title_sort_icu` or the contents of FTS4.
  Structural damage, torn copies (`wrong # of entries in index`) and truncation are caught. PMS
  can `REINDEX` or run its repair tool.
- The stubs live on `internal/plexdb`'s private driver. modernc reads a driver's collation map
  without a lock when it opens a connection and wants registrations before the first open, so
  registering on the shared `sqlite` driver while Bunkarr's database pools open connections is a
  concurrent map read/write that can crash the process. The private driver guards registration
  against its own opens, and the stubs never reach Bunkarr's own connections (a test checks
  this).
- No Plex binaries ship with Bunkarr. `Plex SQLite` is used only in the Docker test suite.
