# 0008. Off-site engines: restic and rclone, encryption custody and the recovery kit

- Status: accepted
- Date: 2026-09-27
- Design: [`docs/design/phase4.md`](../design/phase4.md) revision 2 and its §20 "as built" (D17–D23,
  D29–D33; S21–S29; §3–§7, §9, §10, §14, §16)

## Context

Phase 4 sends backups off the machine for the first time: to Backblaze B2, any S3-compatible
service or an SFTP server, through restic and rclone, which the image already ships. Three risks
shape every decision below. A backup Bunkarr cannot read back is worthless, and restic and rclone
report what they did only loosely (restic leaves files out of a snapshot without an error line;
rclone's server-side moves replace what is in the way). A lost `/config` must not make the
off-site copy unreadable, although the encryption secret lives there. And whoever can choose an
off-site target, or read a command line, receives the library, the Plex token and every *arr's
secrets, so neither may be available to anything that can merely run jobs or list processes.

The facts about restic 0.18.1 and rclone 1.74.1 come from the engine spike (`testdata/restic`,
`testdata/rclone`) and are confirmed by the real-binary tests (`make test-engines`) and the
off-site acceptance suite (`make test-offsite`).

## Decision 1: restic and rclone run as external binaries, behind one planner (D17, D18, D21, D30)

- Bunkarr drives the `restic` and `rclone` programs as child processes (`internal/engines/proc`),
  never as linked libraries. restic has no library API, and linking rclone would tie every Bunkarr
  release to rclone's dependency tree and put the storage secrets in Bunkarr's own memory for the
  lifetime of the process. As children they get only the secrets of one command, can be signalled
  as a process group, and are replaced by updating the image.
- The image installs pinned versions from one file, `docker/engines/versions.env` (`restic~0.18.1`,
  `rclone~1.74.1`: any Alpine rebuild of the same upstream version, nothing newer), which the image
  test checks against `/system/status`. When Alpine moves to a newer version the build fails
  instead of shipping an untested engine, until `versions.env` is bumped and the engine tests and
  the off-site suite (which the release workflow runs before tagging) pass with it.
- The binaries come only from `BUNKARR_RESTIC_PATH` / `BUNKARR_RCLONE_PATH` or `PATH`, checked at
  start-up (absolute, a regular file, not writable by Bunkarr's user, no whitespace or quotes),
  never from a setting: whoever chooses the program receives every destination's secrets. A
  restic remote needs rclone too (restic reaches every remote through its rclone backend: one
  credential path, rclone's bandwidth timetable, SFTP without an ssh client); without it the
  destination is blocked and create and test answer 400.
- Each engine is a driver behind `engines.Engine`; a destination's engine is fixed at creation.
  Filecopy stays the Phase 1 code, reached through the same job types (`sync`, `verify`,
  `retention`), items and records, so an upgraded install plans exactly what it planned before
  (`TestUpgradeFromPhase3`). The syncer's planner is shared by all three engines: tiers, the
  mass-change guard, retention of deleted and replaced versions, releases and dry runs behave the
  same everywhere; execution is per engine, in `internal/enginerun`.
- Every command has a wall-clock budget, an idle watchdog and a retry budget; exit codes are mapped
  per engine; a per-file failure fails that item only. Unit tests use a fake command runner fed
  with the spike's fixtures; the real binaries run in containers.

**Rejected:** re-implementing filecopy behind the engine interface (risk to Phase 1's crash
matrices for no gain); new job types per engine (every schedule, UI and notification path would
fork); restic's native `s3:` backend (a fixed upload limit per batch instead of timetables;
deferred as an option).

## Decision 2: secrets never on a command line, in a log or on the appdata disk (S22)

- A child gets secrets only through its environment, built from exact variable names (nothing of
  Bunkarr's own environment is inherited; `RCLONE_CONFIG=/dev/null`), and through 0600 files in a
  0700 per-command directory on tmpfs (`/dev/shm/bunkarr-run`, only after `statfs` confirms it),
  removed when the command ends and swept at start-up.
- **Nothing secret in argv.** Subcommands and flags are allow-listed per binary; restic's `-o`
  takes only `rclone.program` and `rclone.connections`. So `-vv` (which prints every
  `RCLONE_CONFIG_*` value), `--dump*`, `--log-file`, `--rc*`, password commands and every
  TLS-disabling flag can never be passed. Every argument is refused when the output redaction
  would change it (clear or JSON-escaped), whether the value is one of the command's own secrets
  or any registered one, and so is a `--files-from-raw` list that would read back redacted (a
  single such file fails only its own item): a command that names a secret fails before it
  starts, and nothing is uploaded under a name Bunkarr could not read back.
- rclone passwords are obscured in Go with a deterministic IV, so each has one obscured form that
  the redaction registry, the environment and the kit share. Every child line is redacted (clear,
  obscured and JSON-escaped; overlapping secrets as one) before it is parsed, logged or stored.
- Acceptance 4 checks this with argv-recording shims installed as the binaries, a `/proc` sampler,
  and a search of the process log at debug level, the job logs, every API answer but the kit and
  every file in the container, for every secret in clear, JSON-escaped and rclone-obscured.

## Decision 3: encryption on by default, and custody before the first job (D23, S21)

- Off-site kinds are encrypted by default: restic always, rclone through `crypt` (contents and
  names, `strict_names`, so a wrong password fails instead of listing empty). Plain rclone needs
  `encryption.mode: "none"` plus `acceptUnencrypted` (and a fresh password, Decision 5). Plex and
  *arr versions, which hold the applications' credentials, never go to an unencrypted remote.
- Bunkarr generates the secret (32 random bytes; for crypt, `password` and `password2`), or the
  user supplies one (at least 16 characters, no surrounding whitespace or control characters; for
  crypt `secret` and optionally `secret2`, the kit's password2). It is sealed in its own column,
  written once by Create and never by Update, so rotating storage credentials cannot drop it. It
  never changes: a changed crypt password would orphan every object.
- **No job but a dry run runs until custody is confirmed**: for a generated secret, the recovery
  kit was exported and its check code typed; for a secret typed at create, it was typed again (or
  the check code, which a crypt `secret2` requires). Only `attach` counts as confirmed at creation,
  because the secret already opened the repository or crypt remote; a resumed create does not. The
  API, the scheduler, the global retention job and the runners all check it. Deleting a
  destination whose custody was never confirmed needs `confirmLoseSecret=true`.

**Rejected:** a warning only (open question 1): a user who never saves the secret loses the
off-site copy with `/config`, which is exactly the disaster it exists for.

## Decision 4: the recovery kit is the only way the secret leaves Bunkarr (§5.2, §5.3)

- `POST /destinations/{id}/recovery-kit` needs a UI session and the user's password (checked like
  a login and counted by the login limiter); the API key and the local bypass get 403. Every export
  sends a warning notification with the client's address; the kit is generated from the sealed
  secret on each export and never stored, logged or uploaded. It is the only API answer that
  contains an encryption secret.
- The kit holds the location (with the pinned SFTP host keys), the secret (in clear and, for
  rclone, obscured), optionally the storage credentials, the check code
  (`base32(sha256("bunkarr-kit-v1\0" + destination id + "\0" + secret))[:8]`, so a kit exported
  while the create is still pending stays valid), the layout and the tags, and a shell block plus
  commands that list and restore with nothing but restic and rclone. Every shell value is
  single-quoted by one helper. `rclone.conf` lines are rendered from validated values only, and a
  value rclone's config parser would read back differently (surrounding whitespace, a leading
  backtick or `"""`, `%(name)s`) is refused, for the kit and for every crypt job alike, so no
  backup is written where the kit cannot name it.
- Acceptance 6 restores one file per engine in a fresh container that has only restic and rclone,
  from a script built from the kit's text alone.

## Decision 5: off-site targets need a fresh password (D33, S29)

A leaked API key or any device on the LAN (with the local-address bypass on) can run jobs. It must
not be able to point the library, the Plex token and the *arr secrets at an attacker's bucket.

- These requests need `auth.KindSession` with a user plus `currentPassword` in the body, checked
  with the login's bcrypt check and limiter: `POST /destinations` of a kind other than `local`;
  `PUT /destinations/{id}` that changes `credentials`, `remote.hostKeys` or `remote.caCert`
  (removing a pinned value included); linking a source to a remote destination; adding a Plex or
  *arr backup target on a remote destination; any body with `acceptUnencrypted` or
  `acceptInsecureModes`.
- The API key and the local bypass get 403 "log in and confirm your password to send data
  off-site"; a wrong password is 400 and counts toward the limiter, so guessing is throttled like
  logins. Running existing jobs (sync, verify, retention, backups) stays allowed with the API key.
- Acceptance 9 (`TestDockerOffsiteS29`) runs every one of these routes with the API key, through
  the bypass, with a wrong password and with the right one.

**Rejected:** allowing the API key and only logging it (open question 9): automation that needs to
create off-site targets is rare, and the log would be read after the data left.

## Decision 6: restic records come from a content read-back (D19, D20, D31, D32, S24)

- One snapshot per source per sync (per batch, cumulative), tagged with a random per-row
  `engine_tag`, so an earlier install, a re-created destination or another Bunkarr writing to the
  same repository never shares a group with this row.
- A file counts as backed up only when `restic ls --json` of the new snapshot holds it with the
  recorded size and mtime. A record's `engine_ref` is NULL while its version is in the source's
  base (the snapshot recorded last); it is set to the previous base when a new snapshot lacks the
  version, never moved to another snapshot, and cleared when a new snapshot holds it again.
- Bunkarr computes what to keep (the newest snapshot, the base, every referenced snapshot, and the
  newest complete snapshot of each daily/weekly/monthly/yearly bucket that has one) and forgets by
  id only, after listing the repository again. Snapshots it cannot attribute (other tags, orphan
  groups, unrecorded ones) are never forgotten. `prune` runs only after a successful forget pass,
  and `restic check` after every prune (again in the next retention job if it was interrupted).

**Rejected:** restic's `--keep-*` policies (they group by path list, which changes with every
include list, and count `--keep-within` from the newest snapshot, not from now); deriving records
from exit codes or `-v` lines (restic omits vanished, unreadable and excluded files silently).

## Decision 7: rclone copies with `--backup-dir`, never `sync` deletes (D22, S23)

- The remote holds the live mirror plus `.bunkarr/retention/<run>/`, `links.tsv` and the config
  versions, exactly as a mounted destination, inside a crypt remote by default; a restore without
  Bunkarr is a plain `rclone copy`.
- Uploads are `rclone copy` of the batch's files; an update's old version goes into the run's
  retention folder through `--backup-dir`, so replacing a file never destroys the previous
  version. Renames, retains and repairs of damaged objects are server-side moves (`moveto`) whose
  targets are listed first; an occupied target is displaced into retention. Every command that can
  remove an object carries `--max-delete`. Deletes run only in the retention job (expired retention
  folders) and in the config runners' pruning, on paths checked against their pattern.
- Records come from listings after each batch, never from rclone's log lines, and a reconciliation
  never deletes: an object it cannot place is recorded retained.

**Rejected:** `rclone sync` or `--delete-*` (a deletion or a truncation at the source would
propagate at once, and the mass-change guard could not hold it); `--track-renames`; the top-level
`rclone cleanup` (on B2 it removes old versions).

## Decision 8: B2 uses hard deletes, with keys restricted to the bucket (SE-16, open question 11)

- Deletes on B2 are hard deletes: restic's rclone backend runs with its default arguments, which
  include `--b2-hard-delete`, and rclone's B2 remote gets `hard_delete = true`. A retention expiry
  or a `restic prune` then frees the space at once, B2 keeps no hidden versions to pay for, and
  what the bucket holds is what Bunkarr's retention says it holds. `backend cleanup` is scoped to
  the destination's prefix and removes only unfinished large files, with a max-age of at least 7
  days or twice the longest upload.
- The price is that a deleted object cannot be undeleted, and a compromised `/config` holds a key
  that can delete. So create and test call `b2_authorize_account` over HTTPS (the key only in the
  Authorization header, through the outbound guard): a key restricted to another bucket is
  refused, and a key not restricted to this bucket gets a warning, since it could erase every
  bucket of the account. The README asks for an application key restricted to the bucket, with
  `listBuckets`, `listFiles`, `readFiles`, `writeFiles` and `deleteFiles`, and Object Lock for a
  copy that survives a compromise of `/config`.

**Rejected (deferred):** hide-only deletes by default (`settings.b2.hardDelete: false`): they need
a bucket lifecycle rule, and restic's hard delete comes from its default `rclone.args`, which the
S22 allow-list does not let Bunkarr override yet. To revisit with the manual B2 pass.

## Decision 9: the destination is the right one, and bounded (S25, S26, S27, §6.7, §9)

- restic records the repository id and rclone the marker; every job, test and dry run reads it
  again first (without a lock) and writes nothing on a mismatch, a missing repository or a wrong
  password. Remote hosts, including every host rclone may dial for an S3 bucket, pass netguard at
  save and at each job start.
- `restic unlock` runs only right before an exclusive operation (and before the one retry of a
  backup that found an exclusive lock), after inspecting the locks. A lock of this container's host
  name that is younger than this process and not held by one of its children comes from another
  container with the same host name, and Bunkarr refuses. A lock left by a child of this process or
  of a killed predecessor (recorded in `<config>/cache/restic/children.json`) is removed at once;
  any other lock of this host name waits until restic would call it stale. Every install needs its
  own stable host name, which the compose example requires (`SERVER_NAME`).
- A job never starts outside its destination's transfer window; at the window's end it stops
  cleanly and is deferred with its plan to the next opening. A file larger than a whole window, or
  one that never gets its turn in three windows, fails its item with a warning, so a job never
  defers forever. At most `engines.uploadSlots` engine syncs upload at a time, on workers of their
  own, so a seed that uploads for days never holds the local jobs' workers.

## Consequences

- Users must store a recovery kit before their first off-site backup, and understand that Bunkarr
  cannot recover a lost one; the README says where to keep it.
- A restic destination's retention is bucket-based on top of Phase 1's per-file retention, and a
  kept (demoted) file that changes is backed up again there (D29).
- A folder, bucket or prefix whose name contains a registered secret cannot be backed up: its
  commands are refused at run time.
- restic's cache (`<config>/cache/restic`) can grow to several GiB; it holds encrypted metadata
  only and is removed with its destination.
- Engine upgrades are deliberate: the pinned versions change only together with a green
  `make test-engines` and `make test-offsite`.
- Changing a restic password, forgetting other tags' or orphan groups' snapshots, rebuilding
  records on attach, automated repair, B2 hide-only deletes and the real-B2 pass are deferred
  (DEFERRED.md).
