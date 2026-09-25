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
