# Phase 4 design — destinations and versioning

Status: **Phase 4 implemented** (2026-09-27), revision 3. §20 records Phase 4 as built, after the
acceptance suite and the code review, and is part of the contract: where it differs from §1–§18,
§20 applies. Revision 2 applied the design review of revision 1 (data safety, security,
correctness); §19 lists every finding and what was done with it, and §18 the open questions with
the defaults this document takes. Decisions are summarized in ADR 0008
(`docs/adr/0008-offsite-engines.md`).

It was written after the engine spike: restic 0.18.1 and rclone 1.74.1 in `bunkarr:dev`, against
MinIO (`cgr.dev/chainguard/minio`) and an OpenSSH SFTP server. The fixtures are in
`testdata/restic/` (31) and `testdata/rclone/` (27); each folder's `index.json` names the command
and exit code of every file. A follow-up probe for this document found that restic stores every
`--files-from` target in the snapshot's `paths`, and that `--parent` works across snapshots with
different path sets (§6.2).

It extends `docs/design/phase1.md` (revision 4) and `docs/design/phase2-3.md` (revision 4). Their
rules, contracts and tests still apply unless a section here amends them by number. The schema is
`internal/db/migrations/0004_phase4.sql` (unreleased), and the job contract additions are in
`internal/jobs/contract.go` (both done in this change). Every package in §13 is built against this
document; changing a contract here means updating every package that uses it.

**Goal: spec §8, Phase 4 (destinations and versioning).**
- A restic engine: create or attach a repository, back up, list snapshots, forget and prune by the
  destination's retention, check.
- An rclone engine: copy to any rclone remote, with the remote's configuration stored encrypted.
- Destination types in the UI: local or mounted path, SFTP, S3-compatible and Backblaze B2 (the
  remote ones through restic or rclone).
- Bandwidth limits and transfer windows per destination; concurrency limits.
- Encryption on by default for off-site destinations.
- One engine interface (spec: Plan, Run, Verify, List, Restore; Restore is Phase 5).

**User context.** The first destination is a UniFi UNAS reached by filecopy (Phase 1; it does not
change). The off-site copy is new, and the user wants real media copies there too (the default
tier is `full`, phase2-3.md D1). Bunkarr runs in Docker on Unraid; the image already ships restic
0.18.1 and rclone 1.74.1 (Alpine packages). Neither binary is installed on the dev Mac.

Acceptance (the exact tests are in §14; all must pass):

1. **Two destinations, one source.** One source is backed up to a filecopy destination (the UNAS
   stand-in) and to a restic destination on S3 (the B2 stand-in; plus one run against real B2 by
   hand, §14.6), with different sync schedules and different retention. Each job touches only its
   destination. A sync of one runs while a sync of the other is queued or running. Retention of one
   never changes the other.
2. **Prune respects retention.** For 40 daily snapshots and the pinned table of §14.5 (7 daily,
   4 weekly, 3 monthly, `deletedDays` 30, one retained record and one held update), the retention
   job forgets exactly the 28 snapshots the table lists. It keeps every snapshot a record still
   references and never forgets the newest or the newest recorded snapshot of a source, also with
   every `keep` value 0 and when the newest snapshot is older than every bucket. An update's old
   version stays for `deletedDays` with every `keep` value 0. Then `prune` runs and `restic check`
   passes.
3. **Engine lifecycle.** For a restic destination and for an rclone destination (S3 with crypt,
   and SFTP):
   - an initial sync;
   - an incremental sync after one change, one addition, one deletion and one rename, with the
     stats of §11.4 and the deleted and replaced versions held (S5);
   - a verify that passes, and one that finds a damaged object;
   - a container `kill -9` during an upload, then a restart: the same job resumes (attempt 2) and
     completes, and the destination then verifies.
4. **Secrets.** During every engine job of the Docker suite, no secret value (in clear, JSON-escaped
   or rclone-obscured with any IV) appears in any process's argv (recorded by argv-logging shims
   installed as the `restic` and `rclone` binaries, §14.6), in the process log at debug level, in
   job logs, in an API response other than the recovery kit, or in a file left behind. Secret files
   exist only while their command runs, with mode 0600, on tmpfs when the container has one.
5. **Bandwidth and windows.**
   - An upload limited to 2 MiB/s takes at least 90 % of the expected time, on both engines.
   - A timetable change point takes effect during a transfer (rclone, and restic through its
     rclone backend) or at the next batch (restic on a local path).
   - A sync that reaches the end of its transfer window stops cleanly: no item fails and no
     partial object is recorded. It shows as waiting, resumes in the next window, and the
     destination then verifies.
   - A file that cannot be transferred within one whole window at the rate in force fails its
     item with a warning, and the job completes (unless `window.allowOverrun`, §9.2); a job never
     defers forever.
6. **Encryption and the recovery kit.**
   - A remote destination created with the defaults is encrypted (restic, or rclone crypt).
   - A sync is refused until the destination's recovery kit custody is confirmed (the kit
     exported and its check code typed; or a secret typed at create typed again, §5.2).
   - In a fresh container without `/config`, the kit's instructions alone list the backups and
     restore a file whose sha256 equals the source's, for restic and for rclone crypt.
7. **Config backups off-site.** A Plex DB backup, an *arr backup and a manifest export each reach
   a restic and an rclone destination. They are recorded and listed, a manifest downloads through
   the API with its checksum verified, and their version retention removes exactly the versions the
   Phase 1–2 rules name.
8. **Upgrade from Phase 3.** A Phase 3 database migrates. Filecopy destinations plan exactly what
   they planned before, and while no engine destination exists nothing new is queued.
9. **Off-site needs a fresh password (S29).** The API key and the local-address bypass cannot
   create an off-site destination, change its credentials or host keys, link a source or a config
   backup to it, or accept unencrypted or insecure modes (403). A UI session that re-enters the
   user's password can; a wrong password is counted by the login limiter.

## 1. Safety rules

S1–S20 stay as written. For engine destinations (restic and rclone), S1, S2, S5, S6, S7, S9, S10
and S11 are restated below, and S3 becomes S25. S21–S29 are new. Each rule has tests (§14).

- **S1 for engines (sources are read-only).**
  - An engine only reads a source. The source is always the reading side of a command:
    `restic backup`, `rclone copy <source> <remote>`, `rclone check <source> <remote>`.
  - Bunkarr never runs `rclone move`, `moveto`, `sync`, `bisync`, `delete` or `purge` with a local
    source path, never `--delete-excluded`, and never `restic restore` into a source.
  - restic does not follow symlinks (it stores them as links); rclone skips them (no
    `-L`/`--copy-links`/`--links`).
  - Source file paths reach the engines only as the catalog's relative paths joined to the
    source's recorded root.
- **S2 for engines (the destination is fenced).**
  - Every command names the destination by a remote defined for that command only (§4.4) plus a
    path built from validated relative paths (`<destFolder>/<relPath>`, `.bunkarr/…`), never from a
    string a remote returned.
  - On rclone destinations Bunkarr writes, moves and deletes only inside `<destFolder>/…` and
    `.bunkarr/…`, under S23. An unmanaged object is never replaced or renamed over: `copy` moves it
    aside with `--backup-dir`, and the target of every `moveto`/`move` is listed first and an
    occupied one is displaced into retention (§7.3).
  - On restic destinations Bunkarr changes only snapshots that carry this destination row's tag
    (`bunkarr-dest:<engine_tag>`, a random value per row, §6.1). Its only repository-wide
    operations are `prune`, `check` and `unlock`; `unlock` runs only right before an exclusive
    operation and only when no lock can belong to a live process (§6.7).
- **S5 for engines (deletes are not propagated).** A file that disappears from a source stays at
  the destination for at least `deletedDays`:
  - rclone: it is moved into `.bunkarr/retention/<run>/`, as on filecopy;
  - restic: its last version stays in a snapshot that holds it. The record becomes `retained`
    with the snapshot that holds its version as `engine_ref` (§6.3), and retention never forgets a
    snapshot that a record references (§6.5). The old version of an update is kept the same way,
    as a `replaced` retained row, as on filecopy (§6.2 step 5).
- **S6 for engines (new before old).**
  - restic: a backup never removes a version. It adds a snapshot, and the older snapshot holding
    the old version stays referenced (S5). The S6 retain wait of phase1.md and phase2-3.md applies
    unchanged, through `syncer.Store` as on rclone: retains and releases are decided after the last
    batch, and a waiting retain keeps its record live with its snapshot referenced (§6.2 step 6).
    Without the wait, an upgrade whose new file never gets backed up would let the old version
    expire after `deletedDays` while no version is off-site.
  - rclone: an update runs as `rclone copy --backup-dir`, which moves the old version into the
    run's retention directory before it uploads the new one (spike: `sync-backup-dir.jsonl` logs
    the server-side copy and delete of the old version first). The old version is never lost, but
    the live name is empty while the upload runs or after it failed. Two rules cover that:
    - the **replaced-version hold** (§7.5): a version retained with reason `replaced` is not
      expired while the live record of its path is not `present`;
    - the S6 retain wait of phase1.md and phase2-3.md applies unchanged: a retain waits while a
      full file of its folder is not backed up.
    - an update whose old version moved into the backup dir while no matching new object arrived
      (a failed upload, a window cutoff) turns its live record `missing` (§7.3), so the hold
      applies and plans, verify and manifests treat the file as not backed up.
- **S7 for engines (atomic, verified writes).** An object counts as backed up only after the engine
  reported success *and* Bunkarr read the result back:
  - restic: the batch's snapshot exists with the batch's tags, **and its content listing**
    (`restic ls --json`) shows each file with the size and mtime Bunkarr records (§6.2 step 5).
    restic leaves files out without an error line (a file that vanished inside a directory listed
    whole; an unreadable changed file with `--parent`; an exclude restic matched), and it stores
    whatever bytes a file had when it was read. Only the listing says what a snapshot holds, so no
    record, item or reference is derived from exit codes or error lines alone;
  - rclone: a listing of the batch's paths shows each object with the source's size and mtime
    (§7.3).
  Partial uploads are never recorded. S3 and B2 objects appear only when complete, SFTP uploads go
  to `*.partial` names that rclone renames (spike), and unfinished multipart uploads under the
  destination's own prefix are removed by `backend cleanup` (§7.5). The packs of an interrupted
  restic backup stay unreferenced until `prune` removes them.
- **S9 for engines (dry run).**
  - A dry run of an engine sync plans (its items are the preview) and checks the destination's
    identity read-only (S25). It does not run `restic backup --dry-run` or `rclone --dry-run`, which
    read every file, and it writes nothing: no repository write, no restic lock (read-only restic
    commands run with `--no-lock`), no `restic unlock`, no object.
  - A retention dry run lists its `expire` items and the snapshots it would forget. A verify dry
    run lists the checks it would run.
- **S10 for engines (mass-change guard).**
  - (a) The catalog scan refusals are unchanged.
  - (b) The counts and thresholds are those of Phases 1–3, because the planner is shared (§3.3).
    On restic, a held item keeps its old version referenced: the record stays live with
    `engine_ref` set to a snapshot that holds its version, so retention keeps that snapshot
    (§6.4). A reference is never moved to a newer snapshot; it is only cleared when a new snapshot
    is read back holding the recorded version (§6.3), so a change held over many syncs keeps the
    good version. On rclone, held items are not executed, as on filecopy.
  - Every rclone command that can remove an object carries `--max-delete` (S23).
- **S11 for engines (names).** rclone's encoding maps the characters a backend rejects, so
  `invalidChars` is empty; remotes are treated as case-sensitive. A name the remote refuses fails
  its item ("name too long for the remote"). Crypt's `standard` file-name encryption makes names
  about 1.6 times longer, so this happens earlier on an encrypted SFTP remote (255-byte names).

- **S21 Encryption custody.**
  - Off-site kinds (SFTP, S3-compatible, B2) are encrypted by default: restic always encrypts, and
    rclone wraps the remote in `crypt` (§5). Plain rclone needs `encryption.mode: "none"` together
    with `acceptUnencrypted: true`; otherwise the API answers 400 naming the flag. A local restic
    repository is encrypted too (restic cannot store plain data).
  - Bunkarr generates the encryption secret: the restic repository password, or the crypt
    `password` and `password2`, each from 32 random bytes (base64url). The user may supply one
    instead, to attach an existing repository or crypt remote, or by choice: at least 16
    characters, no leading or trailing whitespace and no control characters (400; restic trims
    the password file, so such a secret would differ from what the kit prints).
  - The secret is sealed in its own column, `destinations.encryption_secret` (ADR 0003, AAD
    `destination:<id>:encryptionSecret`), written once by Create and never by Update; the
    rotatable storage credentials live in `destinations.credentials` (§4.3). It is held in the
    redaction registry in clear, in its obscured form (deterministic, §4.4) and JSON-escaped. The
    API never returns it except in the recovery kit (§5.2).
  - The secret is **immutable** after creation. A changed crypt password would make every object
    unreadable; changing a restic password is `restic key` work (DEFERRED).
  - **No job without custody.** A destination runs no sync, verify, retention or config version
    job (dry runs excepted) until its recovery kit custody is confirmed: for a generated secret,
    the kit was exported and its check code confirmed; for a secret the user typed at create, the
    user re-entered it (§5.2). Only `attach` counts as confirmed at creation, because the secret
    already opened an existing repository or crypt remote. Manual runs answer 409 "export and
    confirm the recovery kit first", schedules report it as `blockedReason`, and the destination
    card says so.
  - Deleting a destination whose secret custody was never confirmed needs
    `confirmLoseSecret=true` (409 otherwise): the secret is deleted with the row.
  - Plex DB and *arr versions (which hold the applications' credentials) never go to a remote
    destination without encryption (§8.4).
- **S22 Secrets never on a command line and never in logs.**
  - A child process gets secrets only through its environment or through files:
    - the restic password through `RESTIC_PASSWORD_FILE`;
    - storage credentials, SFTP keys and crypt passwords through `RCLONE_CONFIG_<REMOTE>_<OPTION>`
      variables (`KEY_PEM` for an SSH key; obscured values for passwords, §4.4). restic reaches
      remote repositories through its rclone backend (D21), which inherits the same variables.
    - `argv` carries only paths, flags, tags and non-secret options.
  - Secret files (`password`, `known_hosts`, `ca.pem`) are created with
    `O_CREATE|O_EXCL|O_NOFOLLOW` and mode 0600 in a per-command directory (0700) on tmpfs:
    `/dev/shm/bunkarr-run/<jobId>-<random>/`, used only after `statfs` reports `TMPFS_MAGIC`,
    else `<config>/run/<jobId>-<random>/` with a warning (appdata pools are snapshotted and
    backed up by plugins, so a secret on that disk can outlive the job). Large non-secret files
    (`files`, `excludes`, `sample`, `combined`) stay in `<config>/run/…`. Both directories are
    removed when the command ends, also on error and cancellation; start-up removes every leftover
    in both places.
  - The child's environment is built from exact names only: `PATH`, `TZ`, `LANG`, `TMPDIR`,
    `HOME` (the run directory), `RCLONE_CONFIG`, `RCLONE_BWLIMIT`, `RESTIC_REPOSITORY`,
    `RESTIC_PASSWORD_FILE`, `RESTIC_CACHE_DIR`, `RESTIC_PROGRESS_FPS`, and
    `RCLONE_CONFIG_BK(DEST|CRYPT)_<OPTION>` for the options of the closed table of §4.4. Bunkarr's
    own environment (including `BUNKARR_*` and `AWS_*`) is never inherited.
    `RCLONE_CONFIG=/dev/null`, so no `rclone.conf` is ever read or written.
  - **Commands and flags are allow-listed** (`internal/engines/proc`), not deny-listed: a `Cmd` is
    a subcommand plus flags from a per-binary table (rclone `copy`, `copyto`, `move`, `moveto`,
    `delete`, `deletefile`, `purge`, `lsjson`, `lsf`, `check`, `cat`, `rcat`, `rmdirs`, `about`,
    `backend cleanup`, `version`; restic `init`, `cat`, `list`, `unlock`, `backup`, `snapshots`,
    `ls`, `forget`, `prune`, `check`, `restore`, `dump`, `version`), each with its exact allowed
    flags. restic's `-o` takes only `rclone.program` and `rclone.connections`. Any other flag or
    `-o` key is a programming error: the job fails and the argv is logged. So `-vv`, `--dump*`,
    `--log-file`, `--rc*`, `--no-check-certificate`, `--insecure-tls`, `--insecure-no-password`,
    `--password-command` and `-o sftp.command`/`rclone.args` can never be passed (spike: `-vv`
    prints every `RCLONE_CONFIG_*` value in clear, `debug-vv-logs-env-secrets.txt`), and the
    environment table above cannot carry `RCLONE_LOG_*`, `RCLONE_RC*`, `RCLONE_DUMP*`,
    `RCLONE_NO_CHECK_CERTIFICATE` or `RESTIC_PASSWORD_COMMAND`. Verbosity is at most `-v`.
  - TLS verification is never disabled. A self-signed S3 endpoint needs `remote.caCert` (§4.2),
    passed as `--ca-cert <run>/ca.pem`.
  - Every line read from a child, stdout and stderr, passes through `logging.RedactSecrets` and
    `logging.RedactValues` (this destination's secrets: clear, obscured and JSON-escaped) before it
    is parsed into an error, logged or stored; a line longer than 1 MiB is redacted before it is
    cut. Unparsed stderr is kept only as its last 20 lines, each cut at 512 bytes.
  - Secret-bearing Go values (`proc.Cmd`, `engines.Secrets`, the destinations credential inputs)
    implement `slog.LogValuer`, `String`, `GoString` and `MarshalJSON` without their secrets, and
    the process log redacts `slog.Any` values recursively (§13.2, `internal/logging`). The SFTP
    `password` and `privateKeyPassphrase` are at least 8 characters, so the registry (which ignores
    shorter values) redacts them.
  - The environment of a process can be read by the same user and by root through
    `/proc/<pid>/environ`, like `bunkarr.key` itself (the threat model of ADR 0003).
- **S23 rclone never deletes or replaces immediately.**
  - Bunkarr never runs `rclone sync`, `bisync`, the top-level `rclone cleanup` (on B2 it removes
    old versions), `--delete-*` or `--track-renames`.
  - Every `rclone copy` carries `--backup-dir <root>/.bunkarr/retention/<run>/<destFolder>`. It lies
    outside the copy's destination, so rclone's overlap check passes (spike:
    `sync-backup-dir-overlap.txt`). An object that would be overwritten is therefore moved into
    retention first, whether Bunkarr has a record for it or not: this is S2's "displace".
  - Retains, releases, promotes and moves are server-side moves inside the destination root. The
    target of every `moveto` and `move` is listed first (`statMany`); an occupied target is first
    displaced into retention (intent, then `moveto --max-delete 1` to a numbered retention name,
    recorded `displaced`), because a server-side move over an existing object replaces it (§7.3).
  - `delete`, `deletefile` and `purge` run only in the retention job and in the config runners'
    pruning, and only on paths checked against their pattern before the command is built: files
    under `.bunkarr/retention/<run>/` that are retained rows past their expiry (§7.5), or a version
    directory of `.bunkarr/{plex,arr,manifests}` that its runner pruned or found incomplete (§8.3).
    There is no other delete: the reconciliation of an interrupted move never deletes (§3.3).
  - Every command that can remove or replace an object (`copy` with `--backup-dir`, `move`,
    `moveto`, `delete`, `deletefile`, `purge`) carries `--max-delete <n>`. For `copy`, n is the
    number of copy and update items in the batch, because any of them can displace an object (a
    `missing` record repaired as a copy, an adopted non-empty remote); on S3 and B2 the backup-dir
    move is a server-side copy plus a delete, and that delete counts. If §14.4 finds that one
    displaced object counts twice, n doubles. rclone stops with exit 7 when n is exceeded (spike:
    `sync-max-delete.jsonl`), and that fails the job, but only after the batch's after-listing
    (§7.3) has recorded every object in the batch's backup dir as retained, so a stopped move
    leaves no unrecorded copy.
  - The mass-change guard (S10(b)) holds changes above the thresholds before any of this runs.
- **S24 The newest restic snapshot is never forgotten; Bunkarr forgets by id only.**
  - Bunkarr never runs `restic forget` with a `--keep-*` policy. It computes the snapshots to forget
    itself (§6.5, a pure function with table tests) and passes their ids. (restic's own policies
    group by path lists, which change with every include list, and `--keep-within` counts from the
    newest snapshot, not from now; spike.)
  - Right before each `forget`, it lists the repository's snapshots again and drops from its list:
    - every snapshot whose Bunkarr tags do not parse into exactly one group of this destination
      row (another `engine_tag`: an earlier install, a re-created destination, another Bunkarr
      writing to the same repository; a missing or duplicated tag);
    - the newest snapshot of every group (§6.1), including other sources' and orphan groups';
    - the newest complete snapshot of every group, and the newest *recorded* snapshot of every
      source (its base, §6.3);
    - every snapshot that a record (`engine_ref`) or a config version row references;
    - every snapshot of an orphan group (a source or integration deleted in Bunkarr). Those are
      kept until a user action (DEFERRED).
  - `prune` runs only after a `forget` pass that succeeded, with `--max-unused` from the settings
    and never with `--unsafe-*` or `--repack-*` options. `restic check` (structure) runs after
    every prune. An interrupted prune is safe to run again.
- **S25 The destination is the right repository or remote (S3 for engines).**
  - A restic destination records the repository id (`restic cat config --json`, field `id`) as
    `marker_id = "restic:<id>"`. Every job, test and dry run first reads it again (with
    `--no-lock`, before any command that writes, `unlock` included) and compares:
    - a missing repository (exit 10) fails with "repository missing (share not mounted, or wrong
      bucket or path?)";
    - a wrong password (exit 12) fails with "wrong repository password";
    - another id fails with "another repository at this location".
    The job then writes nothing. Bunkarr runs `restic init` only when a destination is created
    without `attach` and the repository is missing; never in a job.
  - An rclone destination has the Phase 1 marker `.bunkarr/destination.json` at the root of its
    (crypt) remote. The check reads it with `rclone cat` and compares the parsed id. A missing object
    or content that does not parse is "marker missing". This also covers S3, where a missing object
    can stat as a directory and `cat` exit 0 with no output (spike:
    `lsjson-stat-missing-object-s3.json`), so the content is always compared.
  - Crypt remotes run with `strict_names=true`, so a wrong crypt password fails the listing (exit
    1) instead of showing an empty remote (spike: `crypt-wrong-password.txt`,
    `crypt-wrong-password-strict-names.txt`).
  - The check repeats every 20 batches and before retention and expiry work.
  - A local restic repository also records its filesystem type and compares it (S3's `f_type`
    check), and needs `allowLocal` on a local disk, like filecopy.
  - Remote hosts pass `internal/netguard` when saved and at each job start: the resolved address
    must not be a metadata or link-local address. The names checked cover every name the engine
    may dial: for S3 both the endpoint host and `<bucket>.<endpoint host>` (only the host for an
    IP endpoint), whatever `forcePathStyle` says, because rclone picks the style itself;
    `s3.<region>.amazonaws.com` and `<bucket>.s3.<region>.amazonaws.com` for AWS without an
    endpoint; `api.backblazeb2.com` (and the API host it returns) for B2; the SFTP host. A bucket
    with the Outposts alias suffix `--op-s3` is refused, since the SDK would dial
    `<bucket>.op-<id>.<endpoint host>` or `<bucket>.ec2.<endpoint host>`. The engines then
    connect by themselves; the window for DNS rebinding in between is documented.
  - A destination whose `marker_id` starts with `pending:` (a create that stopped after
    `restic init`, §4.5) never matches, so no job runs against it.
- **S26 Engine processes are bounded and classified.**
  - Commands run without a shell, in their own process group. Cancelling the context sends SIGINT
    (restic then removes its lock; spike: `backup-sigint.stderr.txt`), SIGTERM after 30 s, and
    SIGKILL to the group after 10 s more.
  - Each command has a wall-clock budget (Test and Create: 60 s; listings, marker reads and
    `unlock`: 10 min; transfers, `check` and `prune`: none) and an idle watchdog (no output line for
    15 min: a warning, repeated hourly).
  - restic retries for minutes on a missing bucket or bad S3 credentials (spike:
    `s3-bad-credentials-retrying.stderr.txt`). Bunkarr stops a command whose "returned error,
    retrying" lines last longer than `engines.retryBudget` (default 10 min; 20 s for Test and
    Create) and reports the last such line.
  - Exit codes are mapped per engine (§10.3). A per-file failure fails that item only (restic exit
    3 with `error` lines; an rclone ERROR line naming an `object`). Everything else is fatal for
    the job.
- **S27 Windows and limits stop cleanly.** A job never starts outside its destination's transfer
  window. One that reaches the window's end stops at a clean point (after a batch, or by signalling
  the engine), records what completed, and is re-queued for the next window with its plan
  (`jobs.DeferredError`, §9.2). A deferral is not a failure: no item fails because of it, and no
  notification is sent. A job never defers forever: a file that cannot be transferred within one
  whole window at the rate in force fails its item with a warning (unless the window allows an
  overrun), and the next scheduled fire of a deferred sync supersedes it with a fresh plan (§9.2).
- **S28 The config directory is never backed up by a sync.** restic runs with
  `--exclude-if-present bunkarr.key` (the config directory always holds `bunkarr.key`) and an
  exclude file naming the config directory and every local destination target (§6.2). rclone copies
  only listed catalog files. Bunkarr's database, key and run files never enter a sync.
- **S29 Off-site targets need a fresh password.** Phase 4 is the first time Bunkarr can send data
  to an arbitrary internet host, so the credentials that can run jobs (the API key, and the local
  bypass when auth is `disabled_for_local_addresses`) cannot choose where data goes. These need
  `auth.KindSession` with a user plus `currentPassword` in the body, checked with the same bcrypt
  check and login limiter as the kit (§5.2):
  - `POST /destinations` with a `kind` other than `local`;
  - `PUT /destinations/{id}` that changes `remote.hostKeys`, `remote.caCert` or `credentials`;
  - linking a source to a destination whose kind is not `local` (the `sources` field of
    `POST`/`PUT /destinations`);
  - `PUT /integrations/{id}` whose `backup.targets` adds a destination whose kind is not `local`;
  - any body that sets `acceptUnencrypted` or `acceptInsecureModes` to true.
  `KindAPIKey` and `KindLocal` get 403 "log in and confirm your password to send data off-site";
  a wrong password is 400 and counted by the limiter. Running existing jobs (sync, verify,
  retention, backups) stays allowed with the API key.

## 2. Decisions

| # | Decision | Why |
|---|---|---|
| D17 | An engine is a driver for an external binary (restic, rclone) behind `engines.Engine` (§3.1). Filecopy stays the Phase 1 code, reached through the same job types, items and records. The syncer's planner is shared by all three engines; execution is per engine. | The dry-run preview, tiers, D14, the guards and releases behave the same on every engine, and filecopy is not rewritten (§3.2, §3.3). |
| D18 | No new job types. `sync`, `verify` and `retention` dispatch on the destination's engine. | Schedules, API, UI, notifications and history apply as they are; no table is rebuilt. |
| D19 | restic: one snapshot per source per sync (per batch, cumulative, §6.2), tagged (D32); Bunkarr keeps per-file records, read back from each snapshot (D31); include lists are compressed to whole directories. | Restores per source; exact records for manifests, tier previews and S15; a bounded `paths` list in each snapshot (the probe: every `--files-from` target is stored). |
| D20 | Retention: Bunkarr computes the keep set (newest, record references, daily/weekly/monthly/yearly) and forgets by id (S24). | restic's `--keep-within` counts from the newest snapshot, `--group-by paths` breaks with changing lists, and S5, S15 and the irreplaceable flag need per-file holds. |
| D21 | restic reaches every remote repository through its `rclone:` backend (S3, B2, SFTP); native access only for local paths. | One credential path (the environment), bandwidth timetables, B2 hard deletes (restic's default rclone args include `--b2-hard-delete`; spike), and SFTP without an ssh client in the image (spike: restic's own SFTP backend needs `ssh`, which `bunkarr:dev` lacks). The fallback is in §17. |
| D22 | rclone keeps the Phase 1 layout on the remote (the live mirror plus `.bunkarr/retention/<run>/`) and never runs `rclone sync`. Replacements use `copy --backup-dir`; renames and retains are server-side moves. | S23; a restore without Bunkarr is a plain `rclone copy`; the same records and states as filecopy. |
| D23 | Encryption on by default, and recovery kit custody that must be confirmed before the first real job (§5; only attach is confirmed at creation). | A lost `/config` must not make the off-site backup unreadable. |
| D24 | Config versions (Plex DB, *arr backups, manifests) reach engine destinations through a `VersionStore`: one restic snapshot per version, or an rclone version directory written with `manifest.json` last. The three runners gain an engine path; their retention math does not change (§8). | Requirement; their filecopy path stays as it is. |
| D25 | Plex and *arr backup settings gain up to 4 targets (`backup.targets`), each with its own schedule (§8.5). | For example, the Plex DB to the UNAS daily and to B2 weekly, independently. |
| D26 | A window defers its job (`jobs.DeferredError`). Bandwidth: rclone's timetable; restic through its rclone backend's timetable, or per batch on a local repository; a token bucket on filecopy (§9). | The job resumes in the next window with the same plan; timetables are exact where the engine supports them. |
| D27 | Global upload slots (`engines.uploadSlots`, default 2) for engine syncs, and a per-destination `transfers` setting. | Two off-site seeds must not saturate the uplink; LAN filecopy syncs are not counted. |
| D28 | All restic forgets run in the destination's retention job, which holds `dest:<id>`. Config runners request forgets through `engine_forget`. | `forget` needs restic's exclusive lock, and a seeding sync holds a shared one for days. |
| D29 | On restic, a kept file (S15) whose source changed is backed up again. | A snapshot cannot keep one old file without keeping the whole snapshot. On rclone and filecopy, kept files keep their exact Phase 3 semantics. |
| D30 | Real-binary tests run in containers (`make test-engines`); unit tests use a fake command runner fed with the spike's fixtures. | Neither binary is on the dev Mac; the fixtures keep unit tests hermetic. |
| D31 | restic records come only from a read-back of each batch snapshot's content (`restic ls --json`: path, size, mtime). `engine_ref` NULL means "in the source's base snapshot" (its newest recorded one); a reference is set from the base when a new snapshot lacks the recorded version, never moved to a newer snapshot, and cleared only when a new snapshot holds the recorded version (§6.2, §6.3). | restic omits vanished, unreadable and pattern-excluded files silently and stores bytes that changed after the scan; exit codes and error lines cannot say what a snapshot holds. `restic diff` lacks sizes, and `-v` output is lost in a crash. |
| D32 | Every restic destination row gets a random `engine_tag` (16 bytes, hex) at create; snapshot groups are (engine_tag, kind, source or integration id), and every job tag filter includes it. | Row ids restart in a new database and repeat after a delete and re-attach; a repository-derived tag is the same for every attach. |
| D33 | S29: only a logged-in user who re-enters the password can point data at an off-site location or relax encryption and mode checks; the API key and the local bypass keep running jobs. | A leaked API key or any LAN device must not be able to send the Plex token, the *arr secrets and the library to an attacker's bucket. |

## 3. Engine model

### 3.1 The interface (`internal/engines`)

```go
// Engine is a destination's backup engine (spec §8: Plan, Run, Verify, List; Restore is Phase 5).
// restic and rclone implement it in internal/engines/restic and internal/engines/rclone.
type Engine interface {
	Kind() Kind // "restic" or "rclone" ("filecopy" is native, §3.2)
	// Open checks the destination's identity (S25) and returns a session for one job. It writes
	// nothing, so a dry run uses it too.
	Open(ctx context.Context, d Destination, s Secrets, rt Runtime) (Session, error)
	// Create initializes a new destination (restic init, or the rclone marker) or attaches an
	// existing one; Test probes a location and writes nothing (§4.5).
	Create(ctx context.Context, d Destination, s Secrets, attach bool) (markerID string, err error)
	Test(ctx context.Context, d Destination, s Secrets) (TestResult, error)
}

type Session interface {
	// Plan support: the planner's view of the destination (§3.3). It never writes.
	Caps() filecopy.Capabilities
	PlanFS() PlanFS
	// Run executes the pending items of a complete plan in the Phase 1 order and records each
	// outcome (through the syncer's record store). A second call continues where the first
	// stopped: it is the resume.
	Run(ctx context.Context, in RunInput) (RunStats, error)
	// Verify checks what the destination holds against the records (§6.6, §7.6).
	Verify(ctx context.Context, in VerifyInput) (VerifyStats, error)
	// List returns the versions the destination holds: restic snapshots of sources, and
	// retention runs and config versions on both engines.
	List(ctx context.Context, f ListFilter) ([]Version, error)
	// Retain applies retention: expiry, forget and prune (restic), expiry and cleanup (rclone).
	Retain(ctx context.Context, in RetentionInput) (RetentionStats, error)
	// Versions stores config versions (§8).
	Versions() VersionStore
	Close() error
}
```

`Destination` (engine-neutral: id, kind, remote, marker id, settings, retention, bandwidth),
`Secrets` (unsealed credentials, only in memory for the job), `Runtime` (the job, its reporter,
the run directory, the window and limits, the exec runner) and the input and stats types are
defined in `internal/engines`. The Go names may change during implementation as long as the
semantics here hold.

### 3.2 How filecopy fits
Filecopy is not re-implemented behind `Engine`:
- `destinations.Handle` is its `Open`.
- The Phase 1–3 `sync`, `verify` and `retention` runners of `internal/syncer` are its `Run`,
  `Verify` and `Retain`; their crash matrices stay its contract.
- `internal/engines/filecopy` gains `List` (retention runs and config versions, read from
  `destination_files` and `snapshots`: the listing the API already serves), so the API lists every
  engine the same way.

The dispatch (§3.4) sends every job of a filecopy destination to the syncer runners exactly as
before (acceptance 8). The only filecopy changes in Phase 4 are the bandwidth limit and the window
check (§9) and the planner extraction below.

### 3.3 One planner for every engine
`internal/syncer` exports its planner as `syncer.Planner`. It is the Phase 1–3 `scanAndPlan` path:
select the sources, scan them under the source lock, load the tier decisions, `planSource`, the
S10(b) and S11 guards, and persist the items with `planned_at`. Its inputs are:
- a `PlanFS` (what it reads from the destination);
- the destination's `Capabilities`;
- `FreeSpace func() (int64, bool)` (false: unknown, and the free-space check is skipped);
- `PlanOptions{UpdateKept bool}` (restic only, D29).

The extraction moves code, not behaviour: the filecopy runner passes its `os.Root`-based
`livePlanFS`, and every Phase 1–3 planner test and crash matrix must pass unchanged.

The engines' answers:

| | restic | rclone |
|---|---|---|
| `destStat(rel)` | never exists (a snapshot holds no unmanaged file) | never exists: an object in the way of a copy is moved into retention by `--backup-dir` and one in the way of a move is displaced before the `moveto` (S23); both are recorded `displaced` (§7.3) |
| `destHeadTail(rel)` | the record's `head_tail` (§6.3). When it is NULL, `engines.ErrNoHeadTail`, which `filecopy.Classify` calls an item error, so the pairing skips the pair: the rename becomes copy + retain, which on restic uploads nothing (it deduplicates) | the record's `head_tail` (§7.4); when it is NULL, `engines.ErrNoHeadTail` (copy + retain) |
| `sourceHeadTail(rel)` | through the source's `os.Root`, as Phase 1 | as Phase 1 |
| `Capabilities` | `hardlinks: true` in `recreate` mode (other names of a group are `linked` records; restic stores each name and restores hardlinks within one restore); case-sensitive; no invalid characters; `mtimeGranularityNs: 1`; `unstableInodes: false` (no two destination names are ever compared) | `hardlinks: false` (other names are `link_recorded` and listed in the remote's `.bunkarr/links.tsv`); case-sensitive; no invalid characters; `mtimeGranularityNs`: S3 1, B2 1 000 000, SFTP 1 000 000 000 |
| free space | local repository: statfs, with copy and update bytes as the upper bound; remote: unknown | unknown (SFTP: `rclone about` when the server supports it) |

Retention intents and reconciliation (phase1.md §4.2) per engine:
- **rclone:** the same intent columns are written before a move into retention. A server-side move
  on S3 or B2 is a copy then a delete, so after a crash an object can be in both places. The
  reconciliation at the start of every rclone job lists, for each pending intent, the live path and
  the retention path (`statMany`, §7.3) and settles:
  - only the retention path holds it: recorded retained, as filecopy;
  - only the live path holds it: the intent is cleared (nothing moved);
  - both hold it, whatever the sizes: the reconciliation **never deletes**. "Same size in both
    places" is also the state of a finished update whose new version kept the old size (an
    equal-size rewrite of an NFO, a subtitle, a re-tagged FLAC), where the retention object is the
    only copy of the old version. The retention object is recorded retained (the intent's reason,
    `expires_at = now + deletedDays`). For an update, the live record becomes `present` only when
    the live object has the catalog's size and mtime (the upload finished); otherwise it becomes
    `missing` and the item runs again. For a retain or release (no live catalog file), the item
    runs again and moves the live object to a numbered retention name, recorded `displaced`. An
    interrupted move therefore costs one duplicate object for `deletedDays`, never a version. (An
    interrupted `move` item leaves both names; its re-run displaces the copy at the new name
    first, S23.)
- **restic:** nothing moves, so there are no intents. The reconciliation of a resumed job reads
  back the batch snapshots its earlier attempts made but did not record (§6.2 step 1).

### 3.4 Dispatch and the desired set
`enginerun.Dispatch` is registered for `sync`, `verify` and `retention`. It reads the job's
destination and calls the syncer runner (filecopy) or the engine runner (restic, rclone). The engine
cannot change after creation. A job whose destination is gone takes the syncer runner's Phase 1
error path.

The desired set of a destination is what the shared planner decides file by file (phase2-3.md §8.5):
`full` files are planned as in Phase 1, kept files stay (S15), and non-full files get no item (a
dry run lists them as `skip`). Tier rules apply per destination ("Applies at"), so the UNAS can hold
everything while B2 holds only what the rules make `full` there. On restic, `UpdateKept` plans a
kept file whose source changed as an `update` (D29); its bytes count in `bytesKept`.

## 4. Destination types, remotes and credentials

### 4.1 Kinds

| `kind` | Engines | Location | Encryption |
|---|---|---|---|
| `local` | filecopy (Phase 1); restic (a repository in a directory) | an absolute path inside the container: a mounted share or a disk | filecopy: none; restic: always |
| `sftp` | restic (rclone backend); rclone | host, port, user, path | restic; crypt by default |
| `s3` | restic (rclone backend); rclone | endpoint, region, bucket, prefix | restic; crypt by default |
| `b2` | restic (rclone backend); rclone | bucket, prefix | restic; crypt by default |

A restic repository on the UNAS (kind `local`) is possible and gets every restic rule, but the
UNAS destination of the acceptance is filecopy.

### 4.2 `remote` (non-secret, normalized JSON)
`remote` is decoded with `DisallowUnknownFields` (400 on an unknown field), and every field is
validated; no field name or value ever becomes an rclone option by itself (§4.4).
- **sftp** `{host, port, user, path, hostKeys}`: `host` an RFC 1123 host name or an IP literal
  (no whitespace, `@`, `,`, `*`, `?`, `[`, `]`, `!`); port 1–65535 (default 22); `user` without
  whitespace or control characters; `path` absolute, or relative to the login directory, clean,
  without control characters; `hostKeys` `[{type, key}]`, required (§4.6), each stored only after
  `ssh.ParsePublicKey(base64 key)` succeeds and `type == pk.Type()` (400 otherwise). Only
  `hostKeys` may change after creation.
- **s3** `{provider, endpoint, region, bucket, prefix, storageClass, forcePathStyle, caCert}`:
  `provider` one of `AWS`, `Minio`, `Wasabi`, `Cloudflare`, `Other`; `endpoint` an https URL
  without userinfo, query or fragment (http only for a loopback or private address, with a
  warning); `region` `^[a-z0-9-]{1,64}$`; `bucket` by the S3 naming rules; `storageClass` an enum
  per provider; `caCert` optional PEM (one certificate, `x509.ParseCertificate`) for a
  self-signed endpoint, since TLS verification is never disabled (S22).
- **b2** `{bucket, prefix}`.
- `prefix` (s3, b2) is "" or a clean relative path (`jobs.ValidTargetPath`) without control
  characters (`unicode.IsControl`, so no `\n` or `\r`).
- **local**: none; `target` is the Phase 1 path.

Two destinations may not overlap: the same storage location (the normalized endpoint host and
bucket, where a `b2` destination equals its S3 endpoints `s3.<region>.backblazeb2.com`; or the SFTP
host, port and path) with one prefix equal to or inside the other is a 400 (the S4 rule for
remotes), whatever the kinds. For remote kinds, `target` holds the display location
(`sftp://user@host:22/path`, `s3:<endpoint>/<bucket>/<prefix>`, `b2:<bucket>/<prefix>`) and
`fs_type` holds the kind.

### 4.3 Credentials (sealed, write-only)
Two sealed columns:
- `destinations.credentials` (AAD `destination:<id>:credentials`) holds the storage credentials,
  which may be rotated:
  - **sftp:** `privateKey` (PEM, OpenSSH or PKCS#8) with an optional `privateKeyPassphrase`, or
    `password` (both at least 8 characters, S22);
  - **s3:** `accessKeyId`, `secretAccessKey`;
  - **b2:** `keyId`, `applicationKey`.
- `destinations.encryption_secret` (AAD `destination:<id>:encryptionSecret`) holds
  `resticPassword`, or `cryptPassword` and `cryptPassword2`. It is written once, by Create, and no
  Update statement names the column.

The API returns `hasCredentials: {<field>: true, …}` and never a value. On update, a field that is
sent replaces the stored one; `credentials: {}` or a null field is 400. The unseal, merge and seal
happen inside the `db.Write` transaction, so concurrent updates cannot drop each other's fields.
The new storage credentials are tested against the **stored** remote before they are saved (the
S25 check must pass with them). Encryption fields cannot change (400). Every value, and the
obscured and JSON-escaped form of each password, is registered with
`logging.SetSecrets("destination:<id>", …)`.

A stored secret goes only where it was saved (the S8 rule for engines):
`destinations.SecretsFor(ctx, id)` returns `{remote, hostKeys, credentials, encryptionSecret}`
read from one row in one query, and the engines build a child's environment only from that
pair. There is no call that returns credentials without their location.

### 4.4 How the engines get their configuration
The remote names are fixed and exist only in the child's environment. The environment is produced
from a **closed table per kind**: the options below are the only ones Bunkarr ever sets, and no
user-supplied name ever becomes an option (so `_SSH`, `_SERVER_COMMAND`, `_MD5SUM_COMMAND`,
`_SHA1SUM_COMMAND`, `_SHARED_CREDENTIALS_FILE`, `_PROFILE` and the like can never appear).
- `BKDEST` is the storage remote:
  - s3: `RCLONE_CONFIG_BKDEST_TYPE=s3`, `_PROVIDER`, `_ENDPOINT`, `_REGION`, `_ACCESS_KEY_ID`,
    `_SECRET_ACCESS_KEY`, `_ENV_AUTH=false`, `_NO_CHECK_BUCKET=true`, `_STORAGE_CLASS`,
    `_FORCE_PATH_STYLE`;
  - b2: `_TYPE=b2`, `_ACCOUNT`, `_KEY`, `_HARD_DELETE=true`;
  - sftp: `_TYPE=sftp`, `_HOST`, `_PORT`, `_USER`, `_KEY_PEM` (or `_PASS`, obscured),
    `_KEY_FILE_PASS` (obscured), `_KEY_USE_AGENT=false`, `_ASK_PASSWORD=false`,
    `_KNOWN_HOSTS_FILE=<run>/known_hosts` (0600, the pinned keys). rclone checks no host key when
    that option is empty, so the builder (`rclone.Env` for kind sftp) returns an error when
    `len(hostKeys) == 0`: the check fails closed in the builder, not only in the API. The file is
    rendered only by `knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort(host,
    port))}, pk)`, one line per parsed key (so port 2222 becomes `[host]:2222`, and a key value can
    never add a line, a wildcard or `@cert-authority`).
- `BKCRYPT` is the crypt remote over `BKDEST:<bucket>/<prefix>` (or `BKDEST:<path>`):
  `RCLONE_CONFIG_BKCRYPT_TYPE=crypt`, `_REMOTE`, `_PASSWORD` and `_PASSWORD2` (obscured),
  `_FILENAME_ENCRYPTION=standard`, `_DIRECTORY_NAME_ENCRYPTION=true`, `_STRICT_NAMES=true`.
- The destination root `<root>` is `BKCRYPT:` when encrypted, else `BKDEST:<bucket>/<prefix>`.
- Always: `RCLONE_CONFIG=/dev/null`. Flags that are not secret go in the environment too when they
  contain spaces: `RCLONE_BWLIMIT` (§9.1).
- Passwords are obscured in Go (`rclone.Obscure`: rclone's documented AES-CTR scheme with its fixed
  key and base64url), never by running `rclone obscure`. The IV is **deterministic**: IV =
  HMAC-SHA256(HKDF(bunkarr.key, "bunkarr rclone obscure v1"), "<destinationId>:<field>")[:16]. So
  each password has exactly one obscured form, and that string is what `SetSecrets` registers, what
  the environment carries and what the kit prints (a random IV per command would register a
  string no child ever sees). Test and Create, which have no destination id yet, obscure the
  request's values once and pass the clear and obscured forms to `RedactValues` for every line of
  that request. A real-binary test checks that rclone reads the result back (§14.4).
- restic: `RESTIC_REPOSITORY` is `rclone:BKDEST:<bucket>/<prefix>` (or `rclone:BKDEST:<path>` for
  SFTP; the local path for `local`), plus `RESTIC_PASSWORD_FILE=<run>/password`,
  `RESTIC_CACHE_DIR=<config>/cache/restic/<destinationId>`, `RESTIC_PROGRESS_FPS=0.5`, and the
  `RCLONE_CONFIG_BKDEST_*` variables, which the `rclone serve restic --stdio` child inherits
  (spike). `-o rclone.program=<rclone path>` is passed as a flag.
- The binaries come from the bootstrap environment, `BUNKARR_RESTIC_PATH` and
  `BUNKARR_RCLONE_PATH` (`config.Env`; default: `PATH` lookup), never from a database setting:
  whoever sets the program receives every destination's secrets. At start-up each must be an
  absolute path to a regular file after `filepath.EvalSymlinks`, without whitespace or quote
  characters (restic shell-splits `-o rclone.program=`), and not writable by Bunkarr's UID;
  otherwise the engine is unavailable with the reason.

### 4.5 Test, create and attach
- **`POST /destinations/test`** (before create; writes nothing). It never accepts a destination id
  and never loads stored credentials; it uses only the body's.
  1. netguard checks the host (the dial host of S25);
  2. for SFTP, `remote.hostKeys` must be set; otherwise the answer carries the presented keys
     (§4.6) and `ok: false`, and **no rclone or restic command runs** (the typed password never
     reaches an unverified host);
  3. reachability: `rclone lsf --max-depth 1 BKDEST:<bucket>/<prefix>` (exit 3: bucket or path not
     found; exit 1: credentials);
  4. the repository or marker: restic `cat config --json --no-lock` gives `missing` (exit 10),
     `exists` (with the id), `wrong-password` (exit 12) or `locked` (exit 11); rclone reads the
     marker through the (crypt) remote and gives `missing`, `ok` (id, name), `unreadable` (a wrong
     crypt password or a foreign crypt) or `foreign` (the marker of another destination of this
     database). The first 1000 top-level entries are counted.
  The answer is `{ok, reachable, repository, marker, entries, hostKeys, freeBytes, engineVersion,
  message, warnings}`. For B2, Test (and Create) also call `b2_authorize_account` through netguard
  (the key only in the header) and warn when `allowed.bucketId` is null: the key is not restricted
  to the bucket, so whoever obtains `/config` can erase every bucket of the account (§16).
- **`POST /destinations/{id}/test`** of an engine destination accepts no `remote` and no
  `credentials` (400 "test an engine destination without connection fields"): it tests the stored
  remote with the stored secrets. New storage credentials are tested by the update itself (§4.3),
  against the stored remote only.
- **Create** without `attach` requires a missing repository or marker. The order keeps a generated
  secret from being lost:
  1. the row is inserted with `enabled = 0`, `marker_id = "pending:<uuid>"`, the sealed
     `encryption_secret`, `secret_origin` and a new `engine_tag` (§6.1), in one transaction;
  2. restic: `restic init --json --repository-version 2`, then `cat config` gives the id; rclone:
     the marker is written with `rclone rcat <root>/.bunkarr/destination.json` (content on stdin)
     and read back. A non-empty rclone remote is accepted with a warning, as a filecopy target is:
     `rclone copy` skips objects that already match (adoption), and moves the others into
     retention (S23);
  3. `marker_id` is set and the row enabled.
  If step 2 fails before `restic init` succeeded, the row is deleted (an rclone marker that was
  written is removed, as filecopy does). If init succeeded and a later step fails (or Bunkarr
  crashes, crash point `create.afterInit`), the pending row stays: its secret is still sealed, the
  kit can be exported, and the user can finish the create (a retry of Create for the same row
  attaches the repository it initialized) or delete the row with `confirmLoseSecret=true`. A
  pending row runs no job (S25).
- **Attach** requires an existing repository or marker:
  - restic: the user supplies the password (`secret_origin = user`), and the id is recorded. The
    row gets a new `engine_tag`, so snapshots already in the repository (an earlier install, a
    re-created destination, another Bunkarr) belong to other tags and are never forgotten (S24).
    Attach counts the snapshots with other Bunkarr tags and warns when one is younger than 7 days
    ("another Bunkarr may be writing to this repository");
  - rclone: the marker's id is adopted, as filecopy's `attach` does.
- `marker_id` is UNIQUE, so one repository or remote is one destination.
- Create runs under the Phase 1 create lock, with a 60 s budget (S26).

### 4.6 SFTP host keys
`POST /destinations/sftp/hostkeys {host, port}` answers `[{type, fingerprint, key}]`. Bunkarr
connects with `golang.org/x/crypto/ssh` through netguard, once per key type (ed25519, ecdsa
nistp256/384/521, rsa-sha2-512), and never authenticates. The user confirms the fingerprints
(`SHA256:…`), and every presented key is pinned, because a `known_hosts` with only the ed25519 key
failed when the server negotiated another type (spike:
`rclone-backend-hostkey-mismatch.stderr.txt`). A changed host key fails the job with "SFTP host key
changed" until the user pins the new one (a `PUT` of `remote.hostKeys`, which needs S29's fresh
password).

## 5. Encryption and the recovery kit

### 5.1 What is encrypted
- restic: the whole repository (restic's format: AES-256 and Poly1305 under the repository's
  master key, which the password unlocks).
- rclone crypt: file contents (NaCl secretbox), and file and directory names (EME). The crypt
  remote covers the whole destination root, so `.bunkarr/` (marker, `links.tsv`, retention, Plex,
  *arr and manifest versions) is encrypted too. Sizes, modification times and the directory shape
  stay visible to the storage provider.

### 5.2 The recovery kit
- **Export.** `POST /destinations/{id}/recovery-kit {currentPassword, includeStorageCredentials}`
  answers `200 text/plain` as an attachment. The header is built with
  `mime.FormatMediaType("attachment", {"filename": …})`, and the file name is
  `bunkarr-recovery-<slug>-<yyyymmdd>.txt` with the slug limited to `[a-z0-9-]{1,40}`.
  - It is the only response that contains an encryption secret.
  - It requires `p.Kind == auth.KindSession && p.User != nil`, exactly like `changeCredentials`,
    plus the user's password (`currentPassword`, checked like a login and counted by the login
    limiter). The API key and the local bypass get 403.
  - It sets `kit_exported_at`. The kit is generated from the sealed secret on each export; Bunkarr
    never stores, logs or uploads it.
  - Every successful export sends a warning notification, "Recovery kit for <destination> exported
    at <time> from <client address>", and a process-log line without content, on every export.
- **Contents** (plain text, with the same data as JSON in a delimited block at the end):
  - the destination's name, id, engine and kind; the location (endpoint, region, bucket, prefix,
    or SFTP host, port, user, path and pinned host keys); Bunkarr's version; the creation date;
  - the encryption secret: the restic password, or the crypt `password` and `password2` in clear
    and in rclone-obscured form;
  - the storage credentials only when `includeStorageCredentials` is true; otherwise "create a new
    application key for bucket X in your provider's console";
  - the **check code**;
  - the layout: the tags (with this row's `engine_tag`) and groups (§6.1) or the remote layout
    (§7.1), `links.tsv`, and where the Plex DB, *arr and manifest versions are;
  - restore steps without Bunkarr:
    - restic: the `export` lines (`RESTIC_REPOSITORY`, `RESTIC_PASSWORD_FILE`, the
      `RCLONE_CONFIG_*` lines, and for SFTP `RCLONE_CONFIG_BKDEST_KNOWN_HOSTS_FILE`),
      `restic snapshots --tag bunkarr-dest:<engine_tag>`,
      `restic restore <snapshot>:<source path> --target <dir>` and `restic dump`;
    - rclone: a complete `rclone.conf` with `[bunkarr-dest]` and `[bunkarr-crypt]` (obscured
      passwords; for SFTP `known_hosts_file = ./bunkarr_known_hosts`),
      `rclone lsd bunkarr-crypt:` and `rclone copy bunkarr-crypt:<destFolder> <dir>`;
    - for SFTP, the exact `known_hosts` lines (rendered as in §4.4) in a heredoc that writes
      `bunkarr_known_hosts`, so a restore verifies the host too;
    - recreating hardlinks from `links.tsv`;
  - a warning that whoever holds the kit (and access to the storage) can read the backup.
- **Quoting.** Every value in the kit's shell lines is single-quoted by one `shellQuote` helper
  (`'` becomes `'\''`), so a prefix such as `a$(touch /tmp/p)` or `My Backups` pastes safely. The
  `rclone.conf` lines are built from validated values only, and the renderer refuses CR or LF in
  any value (§4.2 already refuses control characters).
- **Check code.** The first 8 characters of `base32(sha256("bunkarr-kit-v1\x00" + <destination
  id> + "\x00" + secret))`, printed as `XXXX-XXXX` only inside the kit. The id never changes and is
  never reused, so a kit exported while the create is still pending (`pending:` marker) still
  confirms after the create finishes (§20.2).
  `POST /destinations/{id}/recovery-kit/confirm {checkCode}` (case and dashes ignored, compared with
  `subtle.ConstantTimeCompare` on the normalized code) answers 204 and sets `kit_confirmed_at`; a
  wrong code answers 400. Attempts are rate limited like logins. Confirm also requires a UI session
  (the API key and the local bypass get 403), so custody is proven by a person.
- A secret the user typed at **create** is confirmed only by `POST …/recovery-kit/confirm
  {secret}`, which re-enters it (constant-time compare, rate limited like logins), or by the normal
  kit flow; it was typed once in a browser field, and nothing else shows it was stored. A secret
  given to **attach** counts as confirmed at creation, because it already opened the repository or
  crypt remote. The kit is available either way.
- **Reminders.** The destination card shows "Recovery kit confirmed on <date>" or a red banner. A
  warning notification goes out once a day while a destination's secret custody is not confirmed
  (and nothing but dry runs runs for it, S21).

### 5.3 Without `/config`
The kit alone restores (acceptance 6); Bunkarr's database is not needed. restic snapshots carry the
source paths and tags. The rclone remote holds the plain Phase 1 layout (inside crypt), with
`links.tsv` and the manifests. Re-attaching the destination to a new Bunkarr is `attach`: the
records start empty, and the first sync reads the sources again and uploads only what differs
(rclone skips matching objects; restic deduplicates). On restic the new row has a new `engine_tag`,
so the pre-disaster snapshots are never grouped with the new sources and are kept until a user
action (S24; forgetting them by hand is DEFERRED). Rebuilding records from a snapshot is Phase 5.

## 6. restic engine

### 6.1 Repository, tags and groups
- One repository per destination. `restic init --repository-version 2` (compression `auto`).
  `--pack-size` comes from `settings.restic.packSizeMiB` (4–128; default 64 for remote kinds, to
  keep the object count and B2 transactions down, and 16 for local).
- Every restic destination row has an `engine_tag`: 16 random bytes in hex, generated at create
  and attach, unique, never changed (D32). Row ids (sources, integrations, jobs) restart at 1 in a
  new database and repeat after a destination is deleted and attached again, so no tag is built
  from them alone.
- Every snapshot Bunkarr makes has `--host bunkarr` and the tags:
  - `bunkarr`, `bunkarr-dest:<engine_tag>`, `bunkarr-kind:<media|plexdb|arr|manifest>`,
    `bunkarr-job:<jobId>`;
  - media: `bunkarr-source:<sourceId>`, `bunkarr-batch:<n>`;
  - Plex DB and *arr: `bunkarr-integration:<id>`, `bunkarr-version:<version name>`;
  - manifest: `bunkarr-version:<version name>`.
- Every tag filter Bunkarr uses includes `bunkarr-dest:<engine_tag>` (reconciliation, §8.2
  recovery, listings), so a job id or source id of another row or install never matches.
- Config versions are backed up with `--time` = the version's time, so the snapshot time equals
  the version name.
- A **group** is (engine_tag, media, source id), (engine_tag, plexdb or arr, integration id) or
  (engine_tag, manifest). A group of this row whose source or integration no longer exists is an
  **orphan group**. A snapshot whose Bunkarr tags do not parse into exactly one group of this row
  belongs to no group of this row and is never forgotten (S24).
- Media snapshots are recorded in `engine_snapshots`; config versions in `snapshots` and `manifests`
  with `engine_ref` (§8.2).

### 6.2 A sync
Two terms (D31):
- The **base** of a source is its newest snapshot recorded in `engine_snapshots` at this
  destination. Every live record of the source whose `engine_ref` is NULL has its recorded version
  (path, size, mtime) in the base. The base changes only in a batch's recording transaction
  (step 5), which re-establishes this invariant in the same transaction.
- A snapshot **holds** a record's version when its content listing (`restic ls --json`) has the
  record's absolute path (the source's root plus `source_rel_path`) as a file with the record's
  size and mtime (to the nanosecond when both sides have it, else to the second; §14.4 confirms
  restic's precision).

1. **Preflight.**
   - The destination is enabled, its kit custody is confirmed (S21, not for a dry run), and its
     window is open (§9.2).
   - The S25 check (`cat config --json --no-lock`) runs first. A sync never runs `restic unlock`:
     backups take shared locks, which a stale lock does not block (§6.7).
   - The recorded snapshots still exist: `restic snapshots --json --no-lock --tag
     bunkarr-dest:<engine_tag>`. A recorded snapshot missing from the listing (removed outside
     Bunkarr) is a warning, "snapshot <id> was removed outside Bunkarr": its `engine_snapshots` row
     is deleted; retained rows that reference it are deleted with "version lost"; live records that
     reference it, and when it was a source's base that source's NULL records, become `missing`,
     so the plan uploads them again (restic deduplicates what is still stored). `--parent` is only
     ever a snapshot of this listing.
   - A resumed job then reconciles: `restic snapshots --json --no-lock --tag
     bunkarr-dest:<engine_tag>,bunkarr-job:<id>` lists the batch snapshots of its earlier
     attempts. Each one not in `engine_snapshots` (a crash between restic's exit and Bunkarr's
     record) is read back and recorded, in batch order, exactly as in step 5, except that an item
     of that batch whose file the snapshot does not hold stays pending instead of failing (its
     error line died with the process): the batch runs again, and restic re-reads without
     uploading twice. Nothing is assumed done because a snapshot exists.
2. **Plan.** The shared planner (§3.3) with restic's answers. Items: `copy`, `update`, `move`,
   `link`, `promote` (record-only in linked mode), `retain`, `release`, and `skip` in a dry run;
   the guard marks items `held`. A `move`'s new name is a content item: restic reads the file again
   and uploads nothing, and the record takes the new path only when the snapshot holds it. The
   plan is persisted with `planned_at`. A dry run stops here.
3. **Batches.** The content items (copy, update, and the new side of move, link and promote) are
   cut in plan order into batches of at most `settings.restic.batchBytes` (default 64 GiB) of copy
   and update bytes and `settings.restic.batchFiles` (default 20 000) items; moves, links and
   promotes count no bytes. With a transfer window, a batch also holds at most 0.8 × the window's
   length × the rate in force (the limit, or `engine_state.throughput_bps` when lower) of copy and
   update bytes; a file larger than that gets a batch of its own, which starts only at the window's
   opening, and a file that cannot fit the whole window fails its item (§9.2). A plan with retains
   or releases but no content item makes no snapshot (step 6). Before each batch, the head/tail
   hash of every copied or updated file is read through the source's `os.Root` (for later move
   pairing) and stored when its item completes.
4. **Batch k.**
   - The include set I_k holds:
     - every live catalog file of the source that has a live record (`present`, `linked` or
       `missing`) and no item in this job that is pending, held or failed, **whatever its
       `engine_ref`** (a file whose change was reverted is backed up again, and the read-back then
       clears its reference);
     - the files of the content items of batches 1..k that are pending or done.
     It leaves out: the files of retain and release items; held and failed items; content items
     of later batches; files without a record that the plan does not copy (not full here).
   - **Compression.** A directory D is listed whole when every live catalog file under D is in
     I_k; otherwise its included files and its whole subdirectories are listed. The list is
     written NUL-separated to `<run>/files` (absolute paths: the source's root plus the relative
     path) and passed as `--files-from-raw`. With no tier rules the list is the source root alone.
   - **Exclusions.** restic must never exclude a file the catalog includes (its excludes are a
     subset of the catalog's), because such a file would be recorded as backed up while no
     snapshot holds it:
     - `--exclude-file <run>/excludes` holds, anchored and escaped: the config directory, every
       local destination target, and every directory the scan skipped as an alias (S4);
     - the catalog's default patterns (matched case-insensitively by the catalog) go to
       `--iexclude-file <run>/iexcludes`; the source's own patterns (case-sensitive in the catalog,
       `parsePattern(u, false)`) go to the case-sensitive exclude file;
     - an anchored pattern, and an unanchored pattern that contains `/` (the catalog matches it
       against the whole relative path only, restic at any depth), is written as
       `<source root>/<pattern>`, escaped;
     - a pattern whose restic form could match more than `catalog.excluded` does is dropped with a
       debug line, for example a directory-only pattern (`@eaDir/`, `x/`) where restic cannot
       restrict a pattern to directories. Excluding less only uploads junk, which is not recorded.
     `--exclude-if-present bunkarr.key` (S28). Any remaining divergence is caught by the read-back:
     a file restic excluded is not held by the snapshot, so its item fails and nothing claims it.
   - Inside a whole directory, restic also stores what the catalog does not list: excluded junk
     restic's patterns did not catch, symlinks (as links), files created after the scan. They are
     not recorded, and they cost little.
   - The command:
     ```
     restic backup --json --host bunkarr --tag … --files-from-raw <run>/files
       --exclude-file <run>/excludes --iexclude-file <run>/iexcludes
       --exclude-if-present bunkarr.key
       [--parent <the source's base>] [--ignore-inode]
       [--limit-upload <KiB/s> --limit-download <KiB/s>]   (local repositories, §9.1)
     ```
   - `--parent` is always explicit: restic's own lookup needs the same path list, which changes
     with every include list (probe). When the base is missing from the preflight listing, no
     `--parent` is passed, with a warning (restic then reads every file once).
   - `--ignore-inode` is set for sources whose catalog says inode numbers are unstable (FUSE such
     as `/mnt/user`, CIFS with `noserverino`). Without it restic reads every file again on each
     run there.
5. **Record the batch** after exit 0 or 3:
   - **Read-back** (S7):
     - `restic snapshots --json --no-lock <id>` for the summary's `snapshot_id`: it must exist
       with `bunkarr-dest:<engine_tag>`, `bunkarr-job:<id>` and `bunkarr-batch:<k>`;
     - `restic ls --json --no-lock <id>`: the snapshot's nodes (path, type, size, mtime), streamed
       into a per-job temporary table keyed by path.
   - Then one transaction, with B the base before this batch and N the new snapshot:
     - the `engine_snapshots` row of N is inserted, so N becomes the base;
     - **content items of batch k.** An item is done when N holds its file with the catalog's size
       and mtime at plan time. Its record becomes `present` (or `linked`) with `engine_ref` NULL,
       that size and mtime, `head_tail` (when the file did not change since it was read),
       `copied_at` and `job_id`; a move's record takes its new path. For an update (a D29 update
       of a kept file included), the old version first gets a row of its own, as on filecopy:
       state `retained`, reason `replaced` (`damaged` when the record was `missing`),
       `engine_ref` = the old record's `engine_ref`, or B when that was NULL, `retained_path` = the
       absolute path of the old version in that snapshot, `expires_at = now + deletedDays`. The
       live partial unique index allows it beside the live row;
     - an item N does not hold fails: with the text of the `error` line that named it ("could not
       be read: …"), else "not in the snapshot (vanished, excluded or changed during the backup)".
       A failed copy gets no record; a failed update or move keeps its record (the old version);
     - **every other live record of the source**, the failed items' records included: when N
       holds its recorded version, `engine_ref` becomes NULL; otherwise
       `engine_ref = COALESCE(engine_ref, B)`. B holds every NULL record's version (the invariant),
       and a set reference already names a snapshot that holds the version, so **a reference is
       never moved to a snapshot that lacks the version**. This one rule covers a file that
       vanished or was renamed after the scan, a file restic could not read, a file truncated or
       changed after the scan (N holds the new bytes, the record keeps the old version in B, and
       the next sync's update goes through the mass-change guard, S10(b)), a held update in every
       job it stays held, and the files of retain and release items.
   - Exit 3 does not fail the job (spike: the snapshot is saved, `backup-partial.jsonl`). Other
     exits are classified by §10.3: fatal ones leave the batch's items pending, and the resumed job
     runs the batch again.
6. **Finish**, in one transaction after the last batch:
   - The last batch's row gets `complete = 1`.
   - The `retain` and `release` items are decided here, after every content item, with the S6
     wait of phase1.md and phase2-3.md unchanged (through `syncer.Store`): a retain waits while a
     full file of its folder is not backed up (no live record with its size and mtime). A waiting
     item fails with the S6 warning ("not retained yet: … is not backed up"); its record stays
     live with `engine_ref = COALESCE(engine_ref, base)`, and the first sync after the folder's new
     files are backed up retains it. Every other retain or release becomes done: its record turns
     `retained` with `engine_ref = COALESCE(engine_ref, base)` (step 5 already set it when a batch
     ran; never "the newest snapshot"), `retained_path` = the absolute path the file had in that
     snapshot, `expires_at = now + deletedDays`, and the item's reason.
   - When the plan has no content item, no snapshot is made (`unchanged: true`); the base still
     describes the source. No `links.tsv` is written: restic restores hardlinks itself. The stats
     are computed, and the manifest export follows (phase2-3.md §9.2).

A targeted sync (the webhook path) plans only its paths, as the planner does, and still makes a
whole-source snapshot: the include list comes from the whole catalog, and restic only stats the
unchanged files through the parent. Several targeted syncs a day therefore make several snapshots,
which is safe because every record's version stays referenced by the base or by `engine_ref`.

### 6.3 Records
- States: `present`; `linked` (other names of a hardlink group); `missing` (verify, §6.6, or a
  snapshot removed outside Bunkarr, §6.2 step 1); `retained`.
- `engine_ref` NULL means the recorded version is in the source's base (§6.2). Set, it names a
  snapshot that the read-back showed to hold the recorded version. Its only transitions are NULL →
  the previous base (a new snapshot lacks the version) and set → NULL (a new snapshot holds it). A
  reference is never replaced by another snapshot, and a retained row's reference never changes.
- `hash` stays NULL until a verify sample hashes the file. `head_tail` is set at backup (§6.2),
  so renames pair as moves, as on filecopy and rclone, and the mass-change guard counts the same
  changes on every engine.
- A source whose path changed to the same directory (phase1.md §2) gets new absolute paths: the
  next backup reads every file once (restic finds no parent entries) and uploads nothing new.

### 6.4 Guards
- S10(b) as in §1: a held item leaves the file out of the include set, and its record keeps (or
  gets, from the base) its reference, in every job the item stays held, so retention keeps the
  snapshot with the good version however many syncs run before the user decides.
- Free space: a local repository applies the Phase 1 check with the copy and update bytes (an upper
  bound: restic compresses and deduplicates); remote kinds skip it.

### 6.5 Retention job
1. **Preflight**: the kit custody, the window, then S25 (`--no-lock`). A dry run stops its
   repository access there. `restic unlock` runs only in step 3 and step 4, right before the
   exclusive command, under the rules of §6.7.
2. **Expire.** Retained rows past `expires_at` get `expire` items with the Phase 1–3 holds: the
   irreplaceable flag, and the **replaced-version hold** (a `replaced` row whose live record at
   `rel_path` is not `present` is held with "the new version is not backed up yet", §7.5), which
   applies to restic's `replaced` rows (§6.2 step 5) as to rclone's. An expired row is deleted;
   nothing is deleted in the repository by this step.
3. **Forget.** The keep set is computed per group; artifact forgets come from `engine_forget`. The
   repository's snapshots are listed again (S24). `restic forget --json <id>…` runs in chunks of
   100, one `expire` item per snapshot (`detail: {snapshot, group, reason}`). Forgotten media rows
   are deleted from `engine_snapshots` and served requests from `engine_forget`.
   - **Keep set of a media group** (`KeepMedia(snapshots, recorded, refs, retention, now, loc)`):
     - the newest snapshot and the newest complete snapshot of the group in the repository;
     - the source's **base**, its newest recorded snapshot (an unrecorded snapshot left by a crash
       of a job that was then cancelled can be newer than the base, and every NULL record depends
       on the base);
     - every snapshot a record references (`engine_ref` of a live row, or of a retained row not yet
       expired, `replaced` rows included);
     - the newest complete snapshot of each of the last `snapshotDaily` days, `snapshotWeekly` ISO
       weeks, `snapshotMonthly` months and `snapshotYearly` years that have one (restic's bucket
       semantics: only a period with a snapshot counts), in the container's TZ.
     An incomplete snapshot (a batch of a job) is kept only by the first three rules or by a
     reference.
   - Groups are (engine_tag, kind, source or integration id) (§6.1). A snapshot of another
     `engine_tag`, or whose tags do not parse into exactly one group of this row, is never
     forgotten (S24).
   - Defaults: daily 7, weekly 4, monthly 6, yearly 0 (ranges 0–3650, 0–520, 0–120, 0–100). S5's
     `deletedDays` is carried by the references, for deleted files and for updates' old versions
     alike, so every `keep` value may be 0.
   - Plex DB, *arr and manifest versions: their runners' own pruning decides (phase1.md §5,
     phase2-3.md §10 and §11.2). On restic the runner deletes the version's row and inserts an
     `engine_forget` row in one transaction; this step forgets the snapshot.
4. **Prune** when `params.prune` is set, or when `engine_state.last_prune_at` is older than
   `settings.restic.pruneEveryDays` (default 7, 1–90):
   `restic prune --max-unused <settings.restic.pruneMaxUnused, default 10%>`. prune prints text
   even with `--json` (spike: `prune-json-flag.txt`), so only its exit code counts. Then
   `restic check` (structure). A failed prune leaves a consistent repository (restic's design); the
   job fails, and the next retention job tries again.
5. **Dry run**: the `expire` items and the snapshots the forget step would forget; no forget, no
   prune, no unlock.

### 6.6 Verify
- **Repository:** `restic check --json`, with `--read-data-subset=<n>/<t>` where
  `t = ceil(100 / samplePercent)` and n rotates 1..t (`engine_state.read_subset_next`), so every pack
  is read once every t runs. `full` mode (or `params.readData`) uses `--read-data`, and `off` checks
  the structure only. The result is `num_errors` (spike: `check.json`); any error fails the check's
  item and sends a notification.
- **Content sample:** the least recently verified present records with `engine_ref` NULL (their
  version is in the base), up to `samplePercent` of the files and `settings.verify.sampleMaxBytes`
  (default 4 GiB), whose source file still has the recorded size and mtime:
  `restic restore <base>:<source root> --target <config>/staging/verify-job<id>
  --include-file <run>/sample`. Each restored file is hashed and compared with the source's sha256.
  A match sets `verified_at` and records `hash`. A mismatch marks the record `missing`, fails the
  item and notifies. The staging directory is removed when the job ends.
- A mismatch or a check error on restic is not repaired by a new copy: restic deduplicates by
  content, so a damaged blob would be referenced again. The README gives restic's repair procedure
  (`restic repair index`, `repair packs`, then a backup), and automating it is DEFERRED. A later
  verify tries a `missing` record again first.

### 6.7 Locks and resume
- Only exclusive operations need stale locks gone (spike: a stale lock blocks `forget` and `prune`
  even with `--retry-lock`); backups and `--no-lock` reads do not. So `restic unlock` never runs in
  a dry run, a sync, a config version job or a read-only verify step, and never before S25. It
  runs only right before `forget`, `prune`, or a `check` that takes an exclusive lock, and only
  after `restic list locks --no-lock` and `restic cat lock <id>` for each lock show none that could
  belong to a live process: a lock with **this container's host name** whose time is later than
  this Bunkarr process's start and whose PID is not one of its live children comes from another
  container with the same host name (a second install, the dev image, the old container during an
  Unraid replace). restic would call that lock stale (same host, PID gone), and removing it would
  let `prune` delete the other process's packs. Bunkarr then fails with "another restic process
  with host name <X> is using this repository". Locks of other host names are left to restic's own
  staleness rule (30 minutes without a refresh).
- Exclusive operations run with `--retry-lock 30m`; after that the job fails and names the lock's
  host and age (`restic cat lock`, spike `lock.json`).
- `POST /destinations/{id}/unlock {removeAll: true}` runs `restic unlock --remove-all`. It answers
  409 while any job of the destination is running or deferred: a job of any of the six types whose
  `params.destinationId` is this destination (`plexdb_backup`, `arr_backup` and `manifest_export`
  hold no `dest:<id>` key but run `restic backup` here, §9.3). The handler holds `dest:<id>` while
  it runs and refuses while a config version job of the destination is running.
- Host names: every install needs its own, stable host name. The compose example and the Unraid
  notes set `hostname: bunkarr-<server name>` (never a name shared by two containers that can reach
  the same repository). A restarted or recreated container with the same host name and the same
  config directory sees its predecessor's lock as stale at once, which is correct only because the
  predecessor is gone: `<config>/bunkarr.lock` admits one process per config directory, and every
  restic child of a command without `--no-lock` is written to `<config>/cache/restic/children.json`
  (its PID and the start of its run as soon as it is found, before restic can take its lock; its
  end when it ends; records are kept until their run is 30 minutes past). A lock with this host
  name whose PID an earlier process's record names and whose time lies within that child's run (a
  run without an end, the child of a killed process: until this process started) is left by that
  child, like a lock an ended child of this process left (an interrupt stops restic's rclone
  backend with it, so restic cannot remove its lock). Only a live restic process of another
  container with this host name, which the rule above forbids, with the same PID and a lock time
  within that run could be mistaken for it.
- Any other lock with this host name that is older than this process's start may still belong to
  a live process in another container that refreshed it shortly before this process started
  (restic refreshes every 5 minutes and gives up after 22.5 minutes without a refresh; another
  host's lock is stale after 30). A job then waits until the lock is 30 minutes old and inspects
  the locks again (a live process has replaced it by a newer lock by then, which refuses the
  unlock); the unlock endpoint refuses and names the time from which the lock counts as stale. A
  lost `children.json` therefore costs at most that wait after a kill, never a wrong unlock.
- A job killed during batch k resumes at batch k, since its plan is complete. restic reuses the
  data of the killed run only as far as it had written its index. The spike's kill after 6 s
  re-uploaded everything; restic writes index files during long backups, which the Docker test
  checks (§17), and whether packs uploaded before a SIGINT are reused is confirmed in §14.4 before
  batches rely on spanning windows. Batches bound the repeat to one batch.
- Artifact backups run next to a sync's backup (restic's shared locks allow concurrent backups).

### 6.8 Output
- stdout carries one JSON object per line, by `message_type`:
  - `status`: `percent_done`, `total_files`, `files_done`, `total_bytes`, `bytes_done`,
    `current_files`, `seconds_remaining`; fields are omitted when zero (spike);
  - `summary`: `files_new`, `files_changed`, `files_unmodified`, `data_added`, `data_added_packed`,
    `total_files_processed`, `total_bytes_processed`, `total_duration`, `snapshot_id`, `dry_run`;
  - `error`: `{error: {message}, during, item}`.
- stderr carries `exit_error` (`{code, message}`), `error` lines and plain text (retry lines, the
  signal line); lines that are not JSON are skipped.
- `bytes_done` counts bytes read, not uploaded, and reaches 100 % early when the upload is
  throttled (spike). Progress therefore says "processed", and the upload rate comes from the
  summary. Hardlinked files count twice in the totals and are stored once.

## 7. rclone engine

### 7.1 Layout
Inside `<root>` (§4.4) the layout is exactly phase1.md §2 and phase2-3.md §3: the marker,
`links.tsv`, `.bunkarr/retention/<run>/…`, `.bunkarr/plex/`, `.bunkarr/arr/`, `.bunkarr/manifests/`
and the live mirror `<destFolder>/<relPath>`. There is no `.bunkarr/probe`. A restore without
Bunkarr is a plain `rclone copy`.

### 7.2 Plan
The shared planner with rclone's answers (§3.3): `copy`, `update`, `move`, `link` (always
`link_recorded`), `promote`, `retain`, `release`, and `skip` in a dry run.

### 7.3 Execution
`statMany(paths)` is `rclone lsjson --files-only --no-mimetype --files-from-raw <list> <dir>`: the
objects among the listed paths, with size and modtime. Every "does it exist" question uses it; a
single-object `lsjson --stat` is never trusted, because a missing S3 object can stat as a directory.

A server-side `moveto` or `move` over an existing object replaces it, and neither carries
`--backup-dir`. So before every `moveto` and `move`, the targets are listed with `statMany`; an
occupied target is first displaced: an intent (reason `displaced`), then `rclone moveto <target>
<root>/.bunkarr/retention/<run>/<numbered name> --max-delete 1`, then `statMany` of both, then the
retained row. Only then does the move run. (Another writer that creates the target in between is
outside Bunkarr's control; §14.4 records what rclone does then.)

In the Phase 1 order:
1. **promote**: the intent is recorded, the target checked as above, then `rclone moveto
   <root>/<primary> <root>/<dependent> --max-delete 1`, then `statMany` of both paths, then the
   record.
2. **move**: per item, the target checked as above, then `rclone moveto` (server-side) with
   `--max-delete 1`; `head_tail` moves with the record.
3. **copy / update / adopt**, in batches of at most `settings.rclone.batchFiles` (default 1000)
   files and `settings.rclone.batchBytes` (default 64 GiB):
   - Before the batch: intents on the records of update items (`retained_path` = the backup-dir
     path, reason `replaced`); the `head_tail` of every file, read through the source's
     `os.Root`; and a `statMany` of the batch's backup-dir paths. An item whose retention path is
     already taken (a resumed job) runs alone, with a numbered retention name (`name.1.ext`, as
     `RetentionTarget` does): `rclone moveto` of the live object to it, then `rclone copyto`.
   - The command:
     ```
     rclone copy <source root> <root>/<destFolder> --files-from-raw <run>/files --no-traverse
       --backup-dir <root>/.bunkarr/retention/<run>/<destFolder> --max-delete <n>
       --use-json-log -v --stats 5s --stats-log-level NOTICE
       --transfers <t> --checkers <2t> [--max-duration <left> --cutoff-mode soft]
     ```
   - After the batch, whatever the exit: `statMany` of the batch's paths under `<destFolder>` and
     under the backup dir. Then per item:
     - the object is there with the catalog's size and mtime (at the backend's granularity): done.
       The outcome is `adopted` when rclone logged no "Copied" line for it and nothing moved into
       retention, else copied or updated;
     - an object that moved into the backup dir is recorded retained: reason `replaced` for an
       update, `displaced` when the path had no record, `damaged` when the record was `missing`;
     - no matching object: the item fails with rclone's ERROR line for that object, or with "not
       transferred". After a window cutoff (exit 10, or Bunkarr's SIGINT at the end plus grace)
       it stays pending instead (§9.2);
     - in both cases, when the old version of a live record moved into the backup dir and no
       matching live object arrived, the live record becomes `missing` in the same transaction.
       The replaced-version hold (§7.5) then keeps the old version, and plans, verify, manifests
       and tier previews treat the file as not backed up.
   - `--max-delete <n>`: n = the batch's copy and update items (S23).
4. **link**: record-only (`link_recorded`).
5. **retain / release**: the S6 wait check first, per item (phase1.md S6 and phase2-3.md S6,
   through `syncer.Store`); waiting items stay out of the batch. Intents; the retention targets
   pre-listed as above (a taken name gets a numbered one). Then `rclone move <root>/<destFolder>
   <root>/.bunkarr/retention/<run>/<destFolder> --files-from-raw <run>/files --no-traverse
   --max-delete <n>`, then `statMany` of both sides, and the §3.3 settle rules.
6. **Finish**: `links.tsv` is uploaded with `rclone rcat` when its content changed (an S3 or B2 PUT
   is atomic; on SFTP rclone writes a `.partial` and renames it). On SFTP, `rclone rmdirs
   --leave-root` removes the directories a move or retain left empty inside a destFolder; S3 and B2
   have no directories.

The marker is checked again every 20 batches (S25).

### 7.4 Records
States: `present`, `link_recorded`, `missing`, `retained`; never `linked`. `head_tail` is set when
the object is uploaded, and `hash` when a verify sample hashes it.

### 7.5 Retention job
- **Expire**, with the fences of S23: a retained row under `.bunkarr/retention/<run>/`, past
  `expires_at`, not covered by an irreplaceable flag, and not under the **replaced-version hold**: a
  version with reason `replaced` whose live record at `rel_path` is not `present` is held with "the
  new version is not backed up yet". A `statMany` checks the size; a different size is skipped with
  a warning (as `filecopy.Expire`). Then, in batches: `rclone delete <root>/.bunkarr/retention
  --files-from-raw <run>/files --max-delete <n>` (relative paths), and on SFTP `rmdirs`.
- **Cleanup** at most once a day (`engine_state.last_cleanup_at`), for S3 and B2:
  `rclone backend cleanup BKDEST:<bucket>/<prefix> -o max-age=<age>` removes unfinished multipart
  uploads under this destination's own prefix only (S2: other Bunkarr destinations and other tools
  may share the bucket; both backends scope the command to the path, which §14.4 confirms). `<age>`
  is the larger of 7 days and twice the longest expected single-file upload (the largest pending
  file divided by the lowest rate in force), so a 60 GB upload at 512 KiB/s is never aborted. The
  global `--max-age` flag is ignored there (spike). The top-level `rclone cleanup` is never used
  (S23).
- The runners of config versions prune their own versions (§8.3).

### 7.6 Verify
- **Listing:** `rclone lsjson -R --files-only --no-mimetype <root>/<destFolder>` per linked source,
  compared with the live records. A missing object, or one with another size, marks its record
  `missing`; the next sync repairs it (and a damaged object still there goes to retention with
  reason `damaged`, through `--backup-dir`).
- **Content sample:** the least recently verified present records, up to `samplePercent` of the
  files and `settings.verify.sampleMaxBytes` (default 16 GiB), whose source still has the recorded
  size and mtime:
  `rclone check <source root> <root>/<destFolder> --one-way --download --files-from-raw <run>/sample
  --combined <run>/combined`. The combined file marks each path (spike: `= * - + !`): `=` sets
  `verified_at` and records the source's sha256 (Bunkarr hashes the source file); `*` and `-` mark
  the record `missing`; `!` fails the item ("could not be checked").
- `rclone check` without `--download` compares sizes only on crypt and on SFTP without hashes, and
  still exits 0 (spike: `check-on-crypt-size-only.txt`, `sftp-check-no-hashes.txt`), so it is never
  used for content. `cryptcheck` is not used either: it needs remote hashes, which multipart uploads
  lack.
- **Config versions:** every recorded version's files are listed by size, and one version per kind
  and run is downloaded (`rclone cat`) and hashed against its manifest.
- `params.readData` checks every file's content once.

### 7.7 Output
- The log is JSON (`--use-json-log`), one line per event: `{time, level, msg, source, object?,
  objectType?, size?, skipped?}`.
- `stats` lines carry `bytes`, `totalBytes`, `transfers`, `totalTransfers`, `speed`, `eta`, `errors`,
  `fatalError`, `retryError`, `deletes`, `renames`, `serverSideCopies`, `serverSideMoves` and, during
  large transfers, `transferring[{name, bytes, size, percentage, speed}]` (spike). They become
  progress.
- INFO lines ("Copied (new)", "Copied (replaced existing)", "Copied (server-side copy)", "Deleted",
  "Moved into backup dir") are informational: the records come from the listings (§7.3).
- ERROR lines that name an `object` become that item's error text, redacted.

## 8. Config versions off-site (Plex DB, *arr backups, manifests)

### 8.1 `VersionStore` (`internal/engines`)

```go
// VersionStore keeps config versions (".bunkarr/<kind>/<folder>/<version>", phase1.md §2,
// phase2-3.md §3) at an engine destination.
type VersionStore interface {
	// Put stores a complete version from a local directory (manifest.json among its files) and
	// returns its reference only after it reads back complete: a restic snapshot id, or the
	// rclone version path.
	Put(ctx context.Context, v PutVersion) (Ref, error)
	// List returns the versions under a kind folder, complete ones and leftovers.
	List(ctx context.Context, kindFolder string) ([]StoredVersion, error)
	// ReadFile reads one small file of a version (manifest.json, SHA256SUMS), at most limit bytes.
	ReadFile(ctx context.Context, ref Ref, name string, limit int64) ([]byte, error)
	// Fetch copies one file of a version into a local directory (downloads, verify).
	Fetch(ctx context.Context, ref Ref, name, dstDir string) error
	// Remove deletes a version: at once on rclone (fenced, S23); on restic by an engine_forget
	// request in the caller's transaction (D28).
	Remove(ctx context.Context, tx *sql.Tx, ref Ref, kind string) error
}
```

### 8.2 restic
- `Put`: the staged version directory is renamed (inside `<config>/staging`) to
  `<config>/staging/versions/<destinationId>/<logical path>`, so the snapshot's path ends with the
  logical path. Then `restic backup --json --host bunkarr --tag … --time <version time> <that
  directory>` (`--retry-lock 30m`). The snapshot is checked for its tags, and the reference is its
  id.
- `ReadFile`: `restic dump --no-lock <id> <absolute path>` (stdout, capped). `Fetch`: the same into
  a file.
- `List`: `restic snapshots --json --no-lock --tag
  bunkarr-dest:<engine_tag>,bunkarr-kind:<kind>`.
- An unrecorded snapshot of the runner's own job (`bunkarr-dest:<engine_tag>,bunkarr-job:<id>`) is
  adopted by the runner's recovery, after `ReadFile(manifest.json)` parses and matches (the
  Phase 1 rules: this integration and application, this job). The `engine_tag` filter keeps a job
  or integration id of an earlier install from matching (D32).

### 8.3 rclone
- `Put`: `rclone copy <dir> <root>/<logical path> --exclude /manifest.json`, then
  `rclone copyto <dir>/manifest.json <root>/<logical path>/manifest.json`, then `statMany` of every
  file (sizes as staged). `manifest.json` is written last, so a version directory without it is
  incomplete.
- `ReadFile`/`Fetch`: `statMany` of the one name first. A name that is not a file there, missing
  or a prefix with objects under it (whoever can write the bucket decides which), is not found,
  and nothing is downloaded. Then `rclone copyto <root>/<logical path>/<name> <local>
  --max-transfer <3 × listed size + 1 MiB>B --cutoff-mode hard`: rclone stops at the cap (exit 8)
  whatever the remote serves. A `ReadFile` whose file is listed larger than its limit reads only
  the first limit bytes (`rclone cat --count`). `cat` output is collected up to its count at
  most, and the command is stopped there, because a directory at the path prints every object
  under it.
- `Remove`: `rclone purge <root>/<logical path> --max-delete <files of the version + 1>`, after the
  path is checked against the version pattern of its kind (`snapshots.Layout.SplitVersionPath`).
- Recovery (the runner's, at the start of each run):
  - a version directory without `manifest.json`, without a row, whose version time is older than
    24 h, is purged (it can only be a crashed upload);
  - one with `manifest.json` and no row is adopted when its files list with the manifest's sizes
    (the Phase 1 adopt rule; content is checked by verify);
  - `.partial-job*` and `.prune-*` directories are never created on remotes.

### 8.4 Runner changes (`internal/plexdb`, `internal/arrbackup`, `internal/manifest`)
Each runner gains an engine path, chosen by the destination's engine; the filecopy path does not
change. The engine path:
1. **Preflight**: `engines.Open` (S25), the kit (S21), and the runner's Phase 1–2 checks. The
   transfer window does not apply (§9.2); the bandwidth limits do.
2. Backup, staging and verification exactly as today (staging in `<config>/staging`).
3. `Put`, then the row (`snapshots` or `manifests`, with `engine_ref` on restic).
4. **Prune** with the unchanged keep math (`snapshots.Keep`, the manifest rules): for each pruned
   version, `Remove` and the row delete. On restic both are one transaction; on rclone `Remove` runs
   first, and a crash in between leaves a row whose version is gone, which the existing `dropLost`
   handles.
5. **"Unchanged"** (*arr backups, manifests): `ReadFile` of `manifest.json` (and `SHA256SUMS`) must
   parse and match the recorded checksum, and the files must list with their sizes. The content is
   checked by the destination's verify job (§6.6, §7.6).
6. **Credentials stay encrypted off-site** (S17 for engines). Plex DB versions (`Preferences.xml`
   holds the `PlexOnlineToken`, an account-level token) and *arr versions (API keys, indexer and
   download-client passwords) are refused on a destination whose `kind` is not `local` and whose
   `encryption` is `none`: saving such a target answers 400 ("this backup holds <app>'s
   credentials; use an encrypted destination"), and the runner fails the job if the destination
   changed underneath it. An encrypted engine destination counts as `enforcesModes: true` (the zip
   and `Preferences.xml` are encrypted at rest). `acceptInsecureModes` applies only to `local`
   destinations whose probe reports `enforcesModes: false` (SMB without POSIX extensions), and it
   is per target (§8.5), so a flag set for the UNAS never covers a later remote target. Plex
   targets get the same per-target flag for `Preferences.xml` on local SMB; Plex had no gate
   before (a DEFERRED item this closes). Manifests hold no secrets (S20) and may go to any
   destination.
7. **Dry run**: the preflight and the planned item, nothing stored.

The shared parts (put, recover, prune through a `VersionStore`) live in `internal/snapshots`
(`EngineVersions`), used by plexdb and arrbackup; manifest uses the same helper with its own table.

### 8.5 Several targets per integration
`PlexSettings.backup` and `ArrSettings.backup` gain `targets: [{destinationId, cron, enabled,
acceptInsecureModes}]` (at most 4, distinct destinations). Each target is mirrored by one schedule
row (`plexdb_backup` or `arr_backup`, params `{integrationId, destinationId}`), as the single form
was. The Phase 1–2 form `{destinationId, cron, enabled}` (and the *arr's top-level
`acceptInsecureModes`) is still accepted on input and becomes `targets[0]`; GET returns both (the
old fields mirror `targets[0]`). Adding a target whose destination is not `local`, or setting
`acceptInsecureModes`, needs S29's fresh password. A manual backup without `destinationId` uses
`targets[0]`. Deleting a destination removes it from every integration's targets. Manifests are
per destination already (`manifest_export {destinationId}`, after that destination's full syncs).

### 8.6 Manifest downloads
`GET /manifests/{id}/download` on an engine destination `Fetch`es the file into a staging directory
(0700), verifies it against the recorded checksum and `SHA256SUMS`, then streams it, as
phase2-3.md §11.2 describes. A fetch that fails answers 409 "destination not reachable" (S25
errors) or 409 "manifest damaged: checksum mismatch".

## 9. Bandwidth, windows and concurrency

### 9.1 Limits
`destinations.bandwidth`:

```json
{"uploadKiBps": 0, "downloadKiBps": 0,
 "timetable": [{"days": ["mon", "tue", "wed", "thu", "fri"], "from": "08:00", "to": "23:00",
                "uploadKiBps": 1024, "downloadKiBps": 0}],
 "window": {"days": ["mon", "tue", "wed", "thu", "fri", "sat", "sun"], "from": "01:00",
            "to": "07:00", "graceMinutes": 15, "allowOverrun": false}}
```

- 0 means unlimited. Times are `HH:MM` in the container's TZ; `to` before `from` crosses midnight
  (a Monday 23:00–06:00 entry runs into Tuesday). At most 16 timetable entries; entries of one day
  may not overlap (400). At a time no entry covers, the base values apply. KiB/s is 0–10^7;
  `graceMinutes` 0–120 (default 15); `allowOverrun` (default false, §9.2). `window: null` means
  always open.
- Enforcement:
  - **rclone**: `RCLONE_BWLIMIT` holds the timetable as change points in rclone's `UP:DOWN` form
    (`Mon-08:00,1024k:off Mon-23:00,off:off …`; spike: weekday forms are accepted, an invalid value
    exits 2). rclone switches the rate itself during a transfer.
  - **restic, remote kinds**: the same `RCLONE_BWLIMIT` in the environment of `rclone serve restic`,
    which carries restic's traffic. restic's own `--limit-upload` is not set.
  - **restic, local**: `--limit-upload`/`--limit-download` (KiB/s; spike: 4096 gave about 4 MiB/s),
    with the value in force when each batch starts.
  - **filecopy**: a token bucket (`internal/engines/bwlimit`) through `CopyOptions.WrapWriter` (the
    hook DEFERRED.md named), which reads the timetable again every second.
  - Config version jobs to the destination use the same limits.
- The limit in force is reported as `Progress.LimitBytesPerSec`.

### 9.2 Transfer windows
- With a window, a destination's `sync`, `verify` and `retention` jobs run only inside it; dry runs
  run at any time. Config version jobs ignore the window (their own schedules choose the time, and
  they are small); the limits apply to them.
- A job that starts outside the window returns `jobs.Defer(nextOpen, "waiting for the transfer
  window (01:00–07:00)")` before it does anything. The manager re-queues it with `not_before`
  (§11.2).
- **Files that cannot fit a window.** The rate of a file is min(the limit in force,
  `engine_state.throughput_bps`); with neither known, the check is skipped. When the plan's
  batches are cut, a file whose size divided by its rate exceeds the whole window's length plus
  `graceMinutes` fails its item with a warning, "larger than the transfer window at <rate>: allow
  the window to overrun, or raise the limit". The job then completes instead of deferring forever
  (a 45–90 GB remux at 2 MiB/s in a 6-hour window would otherwise never start, and the job would
  block every later sync). As a backstop for an unknown rate, an item (or a restic batch of one
  file) cut by the window's end in two consecutive windows fails with the same warning. With
  `window.allowOverrun`, such a file is instead started alone at the window's opening and may run
  past the end; nothing else starts after the end.
- Inside, the runner knows the window's end (`Progress.WindowEndsAt`) and stops at it:
  - **restic**: batches are capped to 0.8 × the window's length × the rate in force (§6.2 step 3),
    so a batch normally ends before the window does. At the end plus `graceMinutes`, SIGINT to
    restic and a wait for its exit (it removes its lock). The batch's items stay pending, the job
    returns `Defer(nextOpen)`, and the resumed job runs that batch again from the base (its
    parent). How much of the interrupted upload is reused is confirmed in §14.4.
  - **rclone**: every copy and move batch gets `--max-duration <time left> --cutoff-mode soft`
    (spike: exit 10; the finished file stays, no new transfer starts). A transfer still running at
    the end plus `graceMinutes` is stopped (SIGINT); its partial object is never recorded, an
    update whose old version already moved into retention turns its record `missing` (§7.3), and
    unfinished multipart uploads are removed by the cleanup (§7.5). After the batch's listing
    (§7.3), the job returns `Defer(nextOpen)`. Bunkarr does not start a file whose expected time
    is longer than the time left; it stays pending for the next window.
  - **filecopy**: the executor checks between items. An item still copying at the end plus grace
    is cancelled (its temp file removed) and stays pending.
  - **verify, retention**: between steps; a running `restic check` or `prune` is interrupted at the
    end plus grace (both can be run again safely).
- A seed across many windows keeps its work: restic's cumulative batches each end in a recorded
  snapshot, and rclone copies file by file.
- **A deferred sync does not block its schedule.** A fire of the destination's `sync` schedule
  while a deferred sync of that destination is queued supersedes it: the deferred job ends
  `cancelled` with the log line "superseded by the scheduled run of <time>" and no notification,
  and the new job plans again. Its recorded batches and done items stay, so nothing is uploaded
  twice, and new files and deletes reach the plan. A fire of `verify` or `retention` while a
  deferred job of that type and destination is queued is skipped (that job covers it). A manual run
  outside the window queues a waiting job, and the UI says when it will start.
- Warnings: at the first deferral whose remaining plan holds an item that cannot fit any window
  (only possible with an unknown rate), and when a job has been deferred 14 times in a row ("the
  seed needs more windows than two weeks at <rate>").

### 9.3 Concurrency
- **Upload slots.** `engines.uploadSlots` (setting, default 2, 1–8, read at start-up): an engine
  sync that is not a dry run holds one slot while it runs. The manager keeps a job queued, holding
  no worker, while no slot is free: a counting semaphore next to the lock keys
  (`jobqueue.Options.Slots`, filled by the wiring, which knows each destination's engine).
  Filecopy syncs, verify, retention and config version jobs hold no slot. The upload slots are
  workers of their own: the manager runs `jobs.workers` + `engines.uploadSlots` jobs at once, and a
  second pool keeps every job that takes no upload slot (refresh jobs excepted, they have their
  own pool) to `jobs.workers`, so off-site seeds that upload for days never hold the workers of
  the filecopy syncs, the Plex DB and *arr backups and the manifest exports.
- **Per destination.** `settings.transfers` (1–32, default 4): rclone `--transfers` (and
  `--checkers` twice that), restic `-o rclone.connections=<n>` for remote kinds.
- restic allows concurrent backups (shared locks), so a Plex DB backup to a restic destination runs
  during that destination's sync. Exclusive operations run in retention and verify jobs, which
  `dest:<id>` serializes with syncs. A config version job holds no `dest:<id>` key but still counts
  as a job of the destination wherever this document says "active" (the unlock endpoint, §6.7).

## 10. Exec layer (`internal/engines/proc`)

### 10.1 Runner
- `Runner.Start(ctx, Cmd) (Process, error)`. `Cmd` holds the binary (`restic` or `rclone`), the
  subcommand and its flags (checked against the per-binary allow-list of S22 before anything
  runs), the environment variables (checked against the exact names of S22), the secret files
  (name → content, written to the tmpfs run directory), stdin, the budget, the idle timeout and
  the retry watch. `Process` yields redacted stdout and stderr lines and
  `Wait() (ExitStatus, error)`. `Cmd` implements `slog.LogValuer` (binary and arguments only).
- Production runs `os/exec` with `Setpgid`. The binaries come from `BUNKARR_RESTIC_PATH` and
  `BUNKARR_RCLONE_PATH` (validated at start-up, §4.4), else `PATH`; `restic version` and
  `rclone version` are recorded. restic 0.17 or newer and rclone 1.66 or newer are required;
  otherwise the engine is unavailable, with the reason, in `GET /system/status` and in Create and
  Test (400 "restic is not installed").
- Tests use `enginetest.FakeRunner` (§14.2).

### 10.2 Lines
Every line is redacted before anything else reads it (S22), and a line longer than 1 MiB is
redacted before it is truncated, so a secret across the cut is never half-kept. restic's JSON and
rclone's JSON log have their own parsers (§6.8, §7.7); `restic ls --json` node lines are parsed
into the read-back table (§6.2).

### 10.3 Exit codes

| restic | Meaning (spike) | Bunkarr |
|---|---|---|
| 0 | ok | ok |
| 3 | backup: some files could not be read, the snapshot is saved | the batch is recorded through the read-back (§6.2 step 5); items it does not hold fail |
| 10 | repository does not exist | fatal, S25 |
| 11 | repository locked | §6.7, then fatal |
| 12 | wrong password | fatal, never retried |
| 1 | other errors (also: already initialized, no password, SIGINT) | fatal with `exit_error.message`; after Bunkarr's own SIGINT: cancellation or deferral |

| rclone | Meaning | Bunkarr |
|---|---|---|
| 0 | ok | ok |
| 1 | errors (also bad credentials, unknown remote) | per object when ERROR lines name objects; else fatal |
| 2 | usage | fatal (a Bunkarr bug; the arguments are logged, never the environment) |
| 3 | directory not found (bucket, path or source missing) | fatal: "bucket or path not found" |
| 4 | file not found | the item fails |
| 5 | temporary error | the command is retried once after 1 min, then fatal |
| 6 | less serious errors | per object |
| 7 | fatal (`--max-delete` reached, overlap) | fatal, with the message |
| 10 | `--max-duration` reached | window deferral (§9.2) |
| 130, 143 | SIGINT, SIGTERM | cancellation or deferral |

A fatal error fails the job. Nothing needs salvaging on an engine: pending items stay, and the next
job of the destination reconciles (§3.3).

### 10.4 Progress
- restic: `Phase: "backing-up"`; `FilesTotal`/`FilesDone` and `BytesTotal`/`BytesDone` from the
  batch's `status` lines plus the finished batches; `CurrentFile` from `current_files[0]` as a
  relative path; `Batch`, `Batches`.
- rclone: `Phase: "uploading"` (or "moving", "retaining"); bytes and files from `stats`;
  `CurrentFile` from `transferring[0].name`.
- The manager computes throughput and ETA as in Phase 1.

## 11. Jobs

### 11.1 Types, params and validation
- No new types (D18). `sync`, `verify` and `retention` dispatch on the engine (§3.4).
- New params (in `contract.go`): `prune` (retention of one destination: prune a restic repository
  now) and `readData` (verify: read everything once). Validation refuses `prune` on anything but a
  retention job with a `destinationId`, and `readData` on anything but verify.
- A retention job with a destination can now be started by hand
  (`POST /destinations/{id}/retention`); the global retention job still queues one per enabled
  destination.
- The Phase 1 gate (a disabled destination or integration) also refuses jobs of a destination whose
  recovery kit is not confirmed (S21, dry runs excepted) or whose engine is unavailable.

### 11.2 Deferral
`jobs.DeferredError` (contract). The manager:
- sets the job `queued` with `not_before = Until` and `deferrals + 1`; attempt, trigger, items and
  `planned_at` stay; it releases the job's worker, keys and slot;
- logs "Waiting for the transfer window until <time>";
- runs no `OnFinish` hook (no notification);
- does not start a queued job whose `not_before` is in the future, and wakes at the earliest one.

Start-up recovery treats a deferred job as a queued one. The dedupe and the coalescing never return
or merge into a deferred job (it has a plan, phase2-3.md §12.2). Cancelling a deferred job makes it
`cancelled` (hooks run). A deferred sync superseded by its schedule's next fire (§9.2) ends
`cancelled` without a notification. A deferral with a zero or past `Until` fails the job.

### 11.3 Lock keys and slots
Unchanged keys (`dest:<id>` for sync, verify and retention). Engine syncs also hold an upload slot
(§9.3).

### 11.4 Stats
- **Engine sync**: the Phase 1–3 sync stats, plus `engine`, `batches`, `bytesUploaded` (restic: the
  sum of `data_added_packed`; rclone: the stats' `bytes`), `bytesRead` (restic: the sum of
  `total_bytes_processed`), `snapshots: [{sourceId, snapshotId, batch, filesNew, filesChanged,
  filesUnmodified, dataAdded}]` (restic), `unchanged` (no snapshot needed), `deferrals` and
  `limitKiBps`.
- **Engine verify**: the Phase 1 keys, plus `check: {numErrors, readSubset}` (restic),
  `sampleFiles` and `sampleBytes`.
- **Engine retention**: the Phase 1 keys, plus `snapshotsForgotten`, `snapshotsKept`,
  `forgetRequests`, `pruned`, `pruneDurationMs` and `cleanup`.
- Summaries: "Backed up 1,234 files (12.3 GiB new, 2 batches) to B2; snapshot 1a2b3c4d", "Waiting
  for the transfer window until 01:00".

### 11.5 Notifications
As in Phases 1–3, plus:
- "Recovery kit not confirmed for <destination>" (warning, once a day while it blocks jobs);
- "Recovery kit for <destination> exported at <time> from <client address>" (warning, on every
  export, §5.2);
- a verify that finds damage (restic check errors, content mismatches);
- a file larger than the transfer window (§9.2), through the job's warnings;
- a snapshot removed outside Bunkarr (§6.2 step 1).
A deferral never notifies. A job cancelled while deferred notifies as a cancellation does, except
a sync superseded by its schedule (§9.2).

## 12. API (additions; Phase 1–3 rules apply)

**Destinations**
- **S29 applies** to the routes marked (S29) below: a UI session plus `currentPassword` in the
  body; the API key and the local bypass get 403.
- `POST /destinations` accepts (S29 when `kind` is not `local`, when `sources` links a source to
  it, or when `acceptUnencrypted` is true):
  - `kind` (`local`, `sftp`, `s3`, `b2`; default `local`) and `engine` (`filecopy` only with
    `local`; `restic` or `rclone` otherwise);
  - `remote` (§4.2) and `credentials` (§4.3, write-only);
  - `encryption: {mode, generate, acceptUnencrypted, secret}`: `mode` `restic` for restic,
    `crypt` (the default) or `none` for rclone; `generate` (default true) or the user's `secret`
    (§4.3: stored in `encryption_secret`); `none` needs `acceptUnencrypted`;
  - `bandwidth` (§9.1), `currentPassword` (S29), and Phase 1's `attach` and `allowLocal`.
- `Destination` gains `kind`, `remote`, `hasCredentials`, `encryption: {mode, origin,
  kitExportedAt, kitConfirmedAt}`, `bandwidth`, `engineVersion` and `blockedReason`. For remote
  kinds, `target` is the display location. `settings` gains `transfers`, `verify.sampleMaxBytes`,
  `restic: {packSizeMiB, batchBytes, batchFiles, pruneEveryDays, pruneMaxUnused}` and `rclone:
  {batchFiles, batchBytes}`; `retention` gains `snapshotDaily`, `snapshotWeekly`,
  `snapshotMonthly` and `snapshotYearly` (§6.5). Missing values take the defaults; fields of
  another engine are refused (400).
- `PUT /destinations/{id}` may change name, enabled, sources, schedules, settings, retention,
  bandwidth, `remote.hostKeys` and `remote.caCert` (S29), and the storage `credentials` (S29;
  tested first against the stored remote). Linking a source to a destination whose kind is not
  `local` is S29. It may never change kind, engine, the location or the encryption (400).
- `DELETE /destinations/{id}` gains `?confirmLoseSecret=true` (S21).
- `POST /destinations/test` accepts the same connection body and never an id (§4.5);
  `POST /destinations/{id}/test` of an engine destination accepts no connection fields (400).
- `POST /destinations/sftp/hostkeys {host, port}` → `[{type, fingerprint, key}]` (§4.6).
- `POST /destinations/{id}/recovery-kit {currentPassword, includeStorageCredentials}` → the kit;
  `POST /destinations/{id}/recovery-kit/confirm {checkCode}` or `{secret}` → 204 (§5.2). Both
  need a UI session (the API key and the local bypass get 403).
- `POST /destinations/{id}/unlock {removeAll}` → 204 (restic; 409 while any job of the destination
  is running or deferred, of any type, §6.7).
- `POST /destinations/{id}/retention {dryRun?, prune?}` → 202 `Job`.
- `POST /destinations/{id}/verify` accepts `readData`.
- `GET /destinations/{id}/snapshots` lists media snapshots too: `Snapshot` gains `kind: "media"`,
  `sourceId`, `engineRef`, `complete`, `files` and `dataAdded`.

**Settings and system**
- `GET|PUT /settings/engines` `{uploadSlots, retryBudgetMinutes}` (the slot count applies after a
  restart; the response says so).
- `GET /system/status` gains `engines: {restic: {available, version, path, reason},
  rclone: {…}}`.
- Integrations: `settings.backup.targets` (§8.5); adding a target whose destination is not `local`,
  or setting `acceptInsecureModes`, is S29. A Plex or *arr target on a remote destination without
  encryption is 400 (§8.4).
- The binary paths are not settings (`BUNKARR_RESTIC_PATH`, `BUNKARR_RCLONE_PATH`, §4.4);
  `PUT /settings/engines` stays `{uploadSlots, retryBudgetMinutes}`.

Every route is added to `openapi.json` (`TestOpenAPIMatchesRoutes`). The raw-response tests extend
to every new route: no response contains a storage credential, an encryption secret (clear,
obscured or JSON-escaped) or a private key, except the recovery kit.

## 13. Data model and packages

### 13.1 Schema (`0004_phase4.sql`)
Only additive changes; no table is rebuilt, so the upgrade copies no job history.
- `destinations` gains `kind`, `remote`, `encryption`, `encryption_secret` (sealed, written once,
  S21), `secret_origin`, `kit_exported_at`, `kit_confirmed_at` and `engine_tag` (restic, unique
  through a partial index, D32), and starts to use the columns 0002 reserved: `engine` (`restic`,
  `rclone`), `credentials` (storage credentials only), `bandwidth`, and `marker_id`/`target`/
  `fs_type` with their engine meanings (the file header lists them; `pending:<uuid>` marks a create
  that stopped after `restic init`, §4.5).
- `destination_files` gains `engine_ref` (restic) and `head_tail` (restic and rclone), with an index on
  `engine_ref`.
- `snapshots` and `manifests` gain `engine_ref`.
- `jobs` gains `not_before` and `deferrals`, with an index on queued deferred jobs.
- New tables: `engine_snapshots` (restic snapshots of sources), `engine_forget` (forget requests of
  config runners) and `engine_state` (prune, check and cleanup times, the subset rotation, the
  throughput).
- New setting keys: `engines.uploadSlots`, `engines.retryBudgetMinutes`. The binary paths are
  bootstrap environment variables, not settings (§4.4).

`internal/db` `TestMigration0004` upgrades a populated Phase 3 database: no row is lost, the
defaults are those above, the CHECKs accept the new values and refuse others, and the Phase 1–3
suites pass on the migrated schema (the whole `-short` suite passes with 0004 applied as of this
change).

### 13.2 Ownership

Each package owns its tables' SQL, as before. Dependencies point one way:
`engines/restic`, `engines/rclone` → `engines` → `engines/proc`, `engines/filecopy`;
`enginerun` → `syncer`, `destinations`, `catalog`, `tiers`, `engines`; `destinations` →
`engines` (through `Options.Engines`); `plexdb`, `arrbackup`, `manifest` → `engines` (the
`VersionStore` they are handed).

| Package | Owns | Exposes (minimum) |
|---|---|---|
| `internal/engines` | — | `Engine`, `Session`, `VersionStore`, `PlanFS`, the neutral types (§3.1); the registry |
| `internal/engines/proc` | — | `Runner`, `Cmd`, `Process`, run directories (secret files on tmpfs) and secret files, the command and flag allow-list, the exact environment names, redaction, line readers, the retry watch, process-group signals, exit status (§10) |
| `internal/engines/bwlimit` | — | timetable and window math (in force now, next change, next open, time left; pure, table-tested) and the token-bucket writer |
| `internal/engines/restic` | — | the driver: repository strings and environment, init, `cat config`, the lock inspection and guarded unlock (§6.7), backup (include-list compression, the exclude translation), snapshots, `ls` (the read-back), forget, prune, check, restore, dump; the JSON parsers; `KeepMedia` |
| `internal/engines/rclone` | — | the driver: the closed per-kind environment table and crypt, `Obscure` (deterministic IV), `known_hosts` rendering, the bwlimit timetable string, copy, move, moveto, copyto, delete, purge, lsjson/`statMany`, check, cat, rcat, rmdirs, `backend cleanup`; the log parser |
| `internal/engines/filecopy` | — | + `List` (§3.2) and the bandwidth writer hook in use |
| `internal/engines/enginetest` | — | `FakeRunner` (scripted from `testdata/restic` and `testdata/rclone`), a fake `Engine` and `VersionStore` for runner tests |
| `internal/enginerun` | `engine_snapshots`, `engine_forget`, `engine_state` | `Dispatch`; the sync, verify and retention runners of engine destinations; the restic and rclone executors; the restic read-back and reference rules (§6.2 step 5); reconciliation; the forget requests (`RequestForget(tx, …)`) |
| `internal/syncer` | `destination_files` | + `Planner` (§3.3); the record operations the engine executors need (`Store`: batch record, the read-back reference update, `replaced` rows for restic updates, retain with `engine_ref`, intents, refs of a destination, the S6 wait query); `LinkManifest` content; the window check and bandwidth writer in the filecopy executor |
| `internal/destinations` | `destinations`, `destination_sources` | + kinds, `remote` (strict decoding and validation), sealed `credentials` and `encryption_secret`, encryption and origin, `engine_tag`, the kit (render with `shellQuote`, check code, confirm by code or secret), bandwidth validation, engine settings and retention fields, create (pending row first)/test/attach through `Options.Engines`, host-key scan (netguard), `SecretsFor(ctx, id)` (location and secrets from one row) for jobs; `Open` refuses an engine destination (`ErrEngineDestination`), so filecopy-only code cannot take one |
| `internal/snapshots` | `snapshots` | + `engine_ref`; `EngineVersions` (put, recover, prune through a `VersionStore`) |
| `internal/plexdb`, `internal/arrbackup` | — | + the engine path (§8.4) |
| `internal/manifest` | `manifests` | + the engine path and engine downloads (§8.4, §8.6) |
| `internal/integrations` | `integrations` | + `backup.targets` with per-target `acceptInsecureModes` (§8.5) |
| `internal/logging` | — | + `redactAttr` for `slog.KindAny` marshals, redacts recursively and value-redacts (as jobqueue's `redactTree`); `sensitiveKey` also matches `privatekey`, `passphrase`, `applicationkey`, `keypem`, `key_pem`, `secretaccesskey`; registering a secret also registers its JSON-escaped form |
| `internal/config` | — | + `BUNKARR_RESTIC_PATH`, `BUNKARR_RCLONE_PATH` (validated at start-up, §4.4) |
| `internal/jobqueue` | `jobs`, `job_items`, `job_logs`, `schedules` | + `not_before` and deferral, `Options.Slots`, the scheduler's deferred-sync supersede and deferred verify/retention skip, validation of `prune` and `readData`, several backup schedules per integration |
| `internal/jobs` | — | + `DeferredError`, `Defer`, `AsDeferred`, `Params.Prune`, `Params.ReadData`, `Job.NotBefore`, `Job.Deferrals`, the progress fields (done in this change) |
| `internal/db` | migrations | + 0004 (done in this change) and its test |
| `internal/testhooks` | — | + `BUNKARR_TEST_WINDOW` (e2e builds only): a window given as absolute open, close and reopen times for one destination |
| `internal/api` + `cmd/bunkarr` | — | routes (§12), S29 (session plus password on the off-site routes), wiring (engine discovery at start-up, the run-directory sweep on tmpfs and in `<config>/run`, `Dispatch`, slots, the kit endpoints with the password check) |
| `web/` | — | §15 |

## 14. Tests (required)

### 14.1 Unit tests
- **proc**: no secret in argv (the runner refuses a `Cmd` whose arguments contain a registered
  secret); secret files 0600 in a 0700 directory on tmpfs when `statfs` says so (else in
  `<config>/run` with the warning), gone after `Wait`, also after a panic or cancellation; the
  start-up sweep of both places; the exact environment names (Bunkarr's own `BUNKARR_*` and
  `AWS_*` variables are not inherited; `RCLONE_LOG_FILE`, `RCLONE_RC*`, `RCLONE_DUMP*`,
  `RCLONE_NO_CHECK_CERTIFICATE`, `RESTIC_PASSWORD_COMMAND` refused); a table over every refused
  flag and `-o` key (`-vv`, `--dump*`, `--log-file`, `--rc`, `--rc-addr`, `--rc-no-auth`,
  `--no-check-certificate`, `--insecure-tls`, `--insecure-no-password`, `--password-command`,
  `-o sftp.command`, `-o rclone.args`) and every allowed subcommand; redaction of every line,
  including a secret split across the 1 MiB cut and a JSON-escaped secret; `Cmd` logged at debug
  shows no environment or secret file; the retry watch stops a command after the budget; SIGINT,
  SIGTERM, SIGKILL in order to the process group.
- **bwlimit**: timetables across midnight and weekdays; overlapping entries refused; the next open
  and time left around DST changes; the rclone timetable string for each table case; the token
  bucket's rate within 5 % over 5 s.
- **restic**: every fixture of `testdata/restic` parses (status with omitted fields, summary, error,
  exit_error, non-JSON lines, `ls --json` nodes); exit codes → the §10.3 classes; include-list
  compression (no rules → the root only; a held file splits its directories; nested whole
  directories; names with newlines, `*`, `[` and `\` escaped in the exclude file); the tags (with
  `engine_tag`); `--parent` (none when the base is missing) and `--ignore-inode` chosen as §6.2
  says; the lock inspection (a lock with our host name, newer than the process start and not our
  child's PID refuses the unlock; an older one allows it).
- **exclude translation**: a table test over one corpus (case variants, nested paths,
  directory-only patterns, anchored and unanchored patterns with and without `/`) asserts that
  every path restic's translated excludes match is also excluded by `catalog.excluded`, and that
  the source's own patterns land in the case-sensitive file.
- **`KeepMedia`**: the pinned table (§14.5) and its variants; restic's bucket semantics (a period
  without a snapshot is not counted); incomplete snapshots; the TZ; an orphan group; another
  `engine_tag` (an earlier install with the same source ids, a re-created destination, a second
  install writing to the repository) untouched; a snapshot with unparseable or duplicate Bunkarr
  tags untouched; an unrecorded snapshot newer than the base keeps the base; `replaced` rows keep
  the pre-update snapshot until `deletedDays` with every `keep` value 0.
- **rclone**: every fixture of `testdata/rclone` parses (log lines, stats with `transferring`, the
  combined check file); exit codes; the environment for each kind with and without crypt, pinned
  to its exact key set (no key ends in `_SSH`, `_SERVER_COMMAND`, `_MD5SUM_COMMAND`,
  `_SHA1SUM_COMMAND`, `_SHARED_CREDENTIALS_FILE` or `_PROFILE`); the sftp builder errors with no
  host keys; `known_hosts` rendering (port 2222 → `[h]:2222`, one line per key); `Obscure`
  matches rclone's scheme (a vector recorded from `rclone obscure` with a throwaway value) and is
  identical across two builds of the environment, and `logging.ContainsSecret` is true for it;
  `--max-delete` present and correct on every removing command (a table over every builder).
- **destinations**: kinds, remotes and credentials validation (unknown `remote` fields, a host
  with `@` or whitespace, a bad region, a host key with a newline or a type mismatch → 400;
  prefixes with control characters → 400; SFTP passwords under 8 characters → 400; user secrets
  with surrounding whitespace → 400); the overlap rule for remotes (a `b2` destination and an `s3`
  one on B2's endpoint overlap); credentials never in `Destination` JSON; `encryption_secret`
  byte-identical after 50 concurrent PUTs that rotate `secretAccessKey`; `credentials: {}` → 400;
  `SecretsFor` returns the stored location; the kit's content (every section; no storage
  credential unless asked; SFTP `known_hosts` lines); the kit's quoting (a prefix `a$(touch
  /tmp/p)` and one containing `'`: `sh -n` accepts the export block, the export lines round-trip
  through `sh -c 'printf %s "$RESTIC_REPOSITORY"'`, and the `rclone.conf` parses to exactly two
  sections with the expected keys); the check code (constant-time compare); confirm with a wrong
  code; confirm by `{secret}` for a user secret at create, and attach confirmed at once; the
  password check and the 403 for the API key and for the local bypass; the export notification;
  the `Content-Disposition` header with a name containing `"`; delete with unconfirmed custody →
  409; create: a crash at `create.afterInit` leaves a pending row whose sealed secret opens the
  repository, and a pending row runs no job; attach warns about another tag's recent snapshots;
  the host-key scan against a test SSH server (and a metadata address refused by netguard, also
  through the dial host `<bucket>.<host>`).
- **enginerun** (with `FakeRunner`):
  - restic sync: batches (sizes, files, the window cap); the include set per batch (a reverted
    file with a reference is included and its reference cleared); the read-back with the
    FakeRunner's `ls` output for each case where restic omits or changes a file: (a) a file of a
    whole directory deleted after the scan (exit 0, no error line), (b) an unchanged file that
    cannot be read (exit 3, no item), (c) a file truncated after the scan, (d) a batch-1 update
    and a move's new side that fail, (e) a crash after restic's exit, reconciled from the listing
    (items not held stay pending); in every case the record references a snapshot that holds its
    version and no NULL record's version is missing from the base; a held update across three
    syncs keeps S1, and retention with every `keep` value 0 keeps S1; a held-then-deleted file's
    retained row references S1; an update's old version becomes a `replaced` row; an upgrade
    whose new file is unreadable keeps the old snapshot after `deletedDays` (the S6 wait); retains
    decided after the last batch; no snapshot when nothing changed; a targeted sync makes a
    whole-source snapshot; a recorded snapshot removed outside Bunkarr (warning, records
    `missing`, no `--parent`); no `unlock` in a sync;
  - restic retention: expiry with the irreplaceable and replaced-version holds, then forget of
    exactly `KeepMedia`'s complement, the S24 re-check dropping a snapshot that became the newest
    and every snapshot of another `engine_tag`, `engine_forget` requests served, the guarded
    unlock before forget and prune only, prune by interval and by `params.prune`, check after
    prune; a dry run forgets nothing and unlocks nothing;
  - rclone sync: every item kind; `statMany` outcomes (adopted, copied, displaced, replaced,
    damaged); collisions in the retention directory; a `moveto` or `move` onto an occupied target
    displaces it first; `--max-delete` counts every copy and update item (a `missing` record
    repaired onto a differing object, an adopted remote); the S6 wait; the settle rules of §3.3
    for each crash state, including an equal-size update (never a delete); `links.tsv` rewritten
    only on change; a window cutoff mid-update turns the live record `missing`, and retention at
    `deletedDays` + 1 keeps the `replaced` row;
  - rclone retention: the fences (a path outside `.bunkarr/retention/` never reaches a command),
    the replaced-version hold, the irreplaceable hold, the size check, the daily cleanup scoped to
    `<bucket>/<prefix>` with its max-age;
  - verify on both engines: the subset rotation, the sample selection (least recently verified,
    the byte cap, a changed source skipped), marks and notifications;
  - windows: a start outside → deferred with the next open; the end reached mid-batch (restic
    SIGINT, rclone exit 10) → deferred, items pending, nothing failed or recorded partially; a
    file larger than the window fails with the warning and the job completes; with
    `allowOverrun` it runs alone from the window's opening; an item cut in two consecutive
    windows fails;
  - S21: jobs refused before custody is confirmed, dry runs allowed.
- **syncer**: the Phase 1–3 planner tests and crash matrices pass unchanged after the extraction;
  the planner with restic's and rclone's `PlanFS` and capabilities (moves by `head_tail` on both,
  copy + retain when it is NULL, `linked` on restic, `link_recorded` on rclone, `UpdateKept`).
- **plexdb, arrbackup, manifest**: the engine path against the fake `VersionStore`: put, adopt an
  unrecorded version (only with this row's `engine_tag`), purge a leftover older than 24 h, prune
  (restic: row and request in one transaction), "unchanged" read back, `enforcesModes` for
  encrypted destinations; a Plex or *arr target on an unencrypted `s3` or rclone destination → 400
  and the job fails; `acceptInsecureModes` on the UNAS target does not allow a second target on
  plain rclone; a manifest target on plain rclone is allowed; the several targets and their
  schedules.
- **jobqueue**: deferral (status, `not_before`, attempt and trigger kept, hooks not run, the worker
  and keys released, the start at `not_before`); a past `Until` fails the job; dedupe and
  coalescing skip deferred jobs; a sync fire supersedes a deferred sync (cancelled, no
  notification, a new plan) and a verify or retention fire is skipped; slots (a third engine sync
  waits with 2 slots, a filecopy sync does not); validation of `prune` and `readData`.
- **api**: the new routes; raw responses without secrets; the kit endpoints; S29 on every route it
  names (403 for the API key, 403 for the local bypass, 400 for a session with a wrong password,
  counted by the limiter, 200 with the right one); `{id}/test` with connection fields → 400 and
  zero runner calls; an SFTP Test without keys → zero runner calls; the unlock endpoint → 409 with
  a running `plexdb_backup`; `openapi.json`.
- **logging**: each secret-bearing type (`proc.Cmd`, `engines.Secrets`, the credential inputs)
  logged at debug through `logging.New` shows no secret and no key-named field value;
  `slog.Any` of a nested struct is redacted recursively.
- **config**: a binary path with a space, a relative path or a writable file → the engine is
  unavailable with the reason.
- **web** (Vitest): the add-destination flow per kind and engine; write-only credential fields
  (`hasCredentials`); the host-key confirmation; the "no encryption" acknowledgement; the kit
  download and confirmation; the timetable and window editor (overlap errors); the waiting state in
  the queue.

### 14.2 Fake command runner
`enginetest.FakeRunner` answers each expected command with a script: stdout and stderr lines (from
the fixtures, or written in the test), an exit code, delays, and a hook that can change the fake
repository's state (its snapshots, their `ls` content, its locks). It asserts what the real runner
guarantees: the argv holds no registered secret and only allow-listed flags; the expected
environment keys are present and nothing else; every sftp command has
`RCLONE_CONFIG_BKDEST_KNOWN_HOSTS_FILE` pointing to a non-empty 0600 file; each secret file exists
with mode 0600 while the command runs and is gone afterwards. A command no script expects fails
the test.

### 14.3 Crash matrix (Go, every push, no Docker)
New points: `engine.beforeBatch`, `engine.afterBatchExit` (the engine finished, nothing recorded),
`engine.afterRecord`, `rclone.afterIntent`, `rclone.afterMove` (a server-side move copied, not
deleted: the fake leaves both objects), `rclone.afterStat`, `restic.afterForget`,
`versions.afterPut`, `versions.beforeRecord`, `create.afterInit`. `TestCrashMatrixEngines` runs a
restic and an rclone sync against the fakes, with a change set that includes an **equal-size
update**, a file deleted after the scan and a held update, crashes at every occurrence of every
point, resumes, and asserts: every record matches the fake destination; no version was lost
(every replaced or deleted version is held, S5); nothing partial is recorded; on restic, the path
of every live record with `engine_ref` NULL exists with its size and mtime in the newest recorded
snapshot, and every set reference names a snapshot that holds the version; on rclone, no
reconciliation deleted anything; a further sync plans nothing. The retention matrix (with held
items and `replaced` rows) asserts that no crash makes a later run forget a referenced, base or
newest snapshot.

### 14.4 Real-binary tests (`make test-engines`; every push)
Built from `golang:1.27-alpine` plus `apk add restic rclone` (the image's versions), with
`cgr.dev/chainguard/minio` and `atmoz/sftp:alpine` (both pinned by digest, as in the spike) on one
network. `go test -tags enginebin ./internal/engines/... ./internal/enginerun/...` runs inside.
They confirm each assumption of §17 before the code relies on it:
- restic: init, backup with `--files-from-raw` of whole directories plus files, `--parent`
  across different lists, `--exclude-if-present bunkarr.key`, forget by id, prune, check with a
  subset, `restore snapshot:subfolder --include-file`, `dump`; `ls --json` (fields, mtime
  precision, time for 200k files outside `-short`); the read-back cases (a) and (b) of §14.1 with
  the real binary; a backup with the translated exclude files, then `restic ls` compared with the
  catalog (restic excluded nothing the catalog includes); whether packs uploaded before a SIGINT
  are reused by the next backup; `list locks` and `cat lock` fields; the rclone backend for S3 and
  SFTP with the pinned host keys and `RCLONE_BWLIMIT`;
- rclone: copy with `--backup-dir` (update and displace), move into retention, `--max-delete`
  counting backup-dir moves (how many per displaced object, on S3 and SFTP), `moveto` onto an
  existing object (replaced, as §7.3 assumes), `statMany`, crypt with `strict_names`, `Obscure`
  read back, `check --download --combined`, `backend cleanup BKDEST:<bucket>/<prefix> -o max-age`
  touching only uploads under the prefix, `--max-duration --cutoff-mode soft`;
- the secrets probe (§14.6, item 4) on each command.

### 14.5 The pinned retention table (acceptance 2)
One source; 40 complete snapshots, one a day at 02:00 UTC from 2026-08-01 (Saturday) to 2026-09-09
(Wednesday); now 2026-09-09 03:00; TZ UTC; retention daily 7, weekly 4, monthly 3, yearly 0,
`deletedDays` 30. Records: a file deleted at the source was last in the 2026-08-12 snapshot
(retained 2026-08-13, expires 2026-09-12); a held update keeps its old version in 2026-08-20; a
retained row whose last snapshot is 2026-08-04 expired on 2026-09-04 (its `expire` item runs first).

| Kept | Why |
|---|---|
| 2026-09-09 | newest, daily, weekly 2026-W37, monthly 2026-09 |
| 2026-09-03 … 2026-09-08 | daily (09-06 is also weekly 2026-W36) |
| 2026-08-31 | monthly 2026-08 |
| 2026-08-30 | weekly 2026-W35 |
| 2026-08-23 | weekly 2026-W34 |
| 2026-08-20 | referenced (held update) |
| 2026-08-12 | referenced (retained, not expired) |

The other 28 (2026-08-01 … 08-11, 08-13 … 08-19, 08-21, 08-22, 08-24 … 08-29, 09-01, 09-02) are
forgotten; 2026-08-04 only after its row expired. Variants: every keep value 0 → only 09-09 and the
two referenced ones stay; every keep value 0 plus a `replaced` row (an update on 2026-09-02 whose
old version is in 09-01, expiring 2026-10-02) → 09-01 stays as well; now 2027-01-01 with no new
snapshot → 09-09 stays (newest); an unrecorded snapshot of 2026-09-09 04:00 (a crashed, cancelled
job) → 09-09 02:00 stays as the base; a second source's snapshots and an orphan group in the same
repository → untouched; snapshots of another `engine_tag` with the same source id (an earlier
install) → untouched. The Docker acceptance builds the same series with `restic backup --time`
and checks `restic snapshots` after the retention job.

### 14.6 Docker acceptance (`make test-offsite`; every push, like the share tests)
The Bunkarr image, MinIO and the SFTP server on one network; the UNAS stand-in is a volume with
`allowLocal`. Driven over HTTP, as the Phase 1 Docker tests are.
1. **Two destinations** (acceptance 1): a filecopy destination (sync `0 2 * * *`, `deletedDays` 30)
   and a restic destination on MinIO (sync `0 3 * * *`, daily 7, `deletedDays` 14) for one source.
   Both run (by `POST /schedules/{id}/run`) at the same time; each job's items and records name
   only its destination; a retention run of each changes only its own.
2. **Retention** (acceptance 2): §14.5 against the real repository; then prune and `restic check`.
3. **Lifecycle** (acceptance 3), for restic on MinIO, rclone crypt on MinIO and rclone crypt on
   SFTP: initial sync; the change set (1 changed, 1 added, 1 deleted, 1 renamed; stats
   `filesCopied=1, filesUpdated=1, filesMoved=1, filesRetained=1` on every engine, and
   `bytesUploaded` about the changed and added files only: rclone's move is server-side, restic's
   uploads nothing); verify;
   an object damaged in MinIO (a pack for restic, a file object for rclone) found by the next
   verify; `docker kill` during a 400 MiB upload at 4 MiB/s, then `docker start`: attempt 2
   completes and verify passes. The source tree is snapshotted before and after every job and
   must not change (S1).
4. **Secrets** (acceptance 4): argv-recording shims are installed as the `restic` and `rclone`
   paths (`BUNKARR_RESTIC_PATH`, `BUNKARR_RCLONE_PATH`); each appends its `$@` to a log and execs
   the real binary, so short-lived commands (`rcat`, `cat config`) are recorded too (a 200 ms
   `/proc/*/cmdline` sampler would miss them). The argv log, the process log (debug), job logs,
   every API response and the container's files (except the sealed database) are searched for
   every secret in clear and JSON-escaped, and every base64url token of at least 24 characters in
   them is passed to `rclone.Reveal`; the test fails if one reveals a secret (this covers every
   IV). The engine children's environment is where the secrets are expected, and nowhere else.
5. **Bandwidth and windows** (acceptance 5): 40 MiB at 2 MiB/s takes ≥ 18 s on both engines; a
   timetable switching from 1 MiB/s to 8 MiB/s 10 s into the transfer shows both rates in the
   progress; a window (through `BUNKARR_TEST_WINDOW`) closing 15 s into a 100 MiB transfer at
   2 MiB/s → the job is queued with `not_before`, no item failed; it resumes when the window
   reopens and verify passes.
6. **Kit** (acceptance 6): a sync before confirmation → 409; export and confirm; then, in a fresh
   `golang:1.27-alpine` + `apk add restic rclone` container with no `/config`, a script that uses
   only the kit's text lists the snapshots and restores one file per engine; the sha256 matches.
7. **Config versions** (acceptance 7): a Plex DB backup (the fake PMS data of the Phase 1 tests), an
   *arr backup (the fake *arr of Phase 2) and a manifest export to a restic and an rclone
   destination; listed; the manifest downloaded and verified; three more versions with a
   retention of daily 2 → the oldest pruned (rclone purge; restic forget at the next retention
   run).
8. **Upgrade** (acceptance 8): the Phase 3 binary (`git archive` of `7d3477d`) writes a database
   with filecopy jobs; the Phase 4 binary on a copy plans identical dry-run items and stats, and
   queues nothing new.
9. **Real B2** (acceptance 1's B2 half; by hand before the release, not in CI): the same lifecycle
   against a B2 bucket through restic and rclone, gated by `BUNKARR_E2E_B2_KEY_ID`,
   `BUNKARR_E2E_B2_KEY` and `BUNKARR_E2E_B2_BUCKET`; afterwards the bucket holds no hidden file
   versions (hard deletes) and no unfinished large file under the prefix; a bucket-unrestricted
   key gets the warning; `backend cleanup` leaves an upload under another prefix alone.
10. **S29** (acceptance 9): over HTTP with the API key and from a local address with the bypass on,
    creating an S3 destination, rotating its credentials, pinning SFTP host keys, linking a source
    and adding a Plex backup target on it → 403; the same with a session and the right password
    → 200.
11. **Two containers, one repository** (§6.7): a second Bunkarr container with the same host name
    and the same restic repository runs a sync while the first starts a retention job; the first
    refuses to unlock ("another restic process with host name …"), the second's snapshot is
    complete, and `restic check` passes.

## 15. UI

- **Destinations → Add** is a short wizard:
  1. **Where**: "Local or mounted folder", "SFTP server", "S3-compatible storage", "Backblaze B2".
  2. **How**: local offers "Plain files (filecopy)" (the default, Phase 1) and "restic repository";
     the others offer "restic (snapshots, deduplicated)" (the default) and "rclone (plain copy of
     the files)", each with one line on what it means for restores and costs.
  3. **Connection**: the kind's fields (§4.2) and write-only credentials (password-type inputs that
     show "saved" when `hasCredentials` says so). SFTP: "Fetch host keys" lists the fingerprints to
     confirm. S3: provider presets fill the endpoint pattern.
  4. **Encryption**: restic: "Generate a password (recommended)" or "Use my own / existing
     repository"; rclone: "Encrypt with rclone crypt (recommended)" or "Do not encrypt", which needs
     a checkbox acknowledging that the provider can read every file.
  5. **Test** (required before create, as in Phase 1), then **Create**.
  6. **Recovery kit**: for a generated secret, "Download recovery kit", then "Type the check code
     from the kit"; for a secret the user typed, "Type your password again" (or the kit flow);
     until then the card shows the red banner and the actions are disabled with the reason.
  - Every S29 action (creating an off-site destination, changing its credentials or host keys,
    linking a source or a backup target to it, accepting no encryption or insecure modes) asks for
    the user's password in the same dialog.
  7. Sources, schedules (sync, verify, and retention for engines), retention (the engine's fields
     with the server's ranges and help text), bandwidth (base limits, the timetable editor, the
     window with "Let a file larger than the window run past its end"), verify (sample percent,
     byte cap), transfers.
- **Destination card**: kind and engine badges, "Encrypted" or "Not encrypted", the kit state, the
  location, last sync, snapshot count and the repository's last known size (from `engine_state`),
  "Waiting for the window until 01:00", and for restic "Prune now" and "Remove stale locks" (with
  "Remove all locks" behind a confirmation).
- **Snapshots dialog**: restic media snapshots per source (time, batch, complete, files, data
  added), and the Plex DB, *arr and manifest versions as before.
- **Queue and job page**: the batch counter, the limit in force, the window's end; a deferred job
  shows "Waiting for the transfer window, resumes at <time>" and its deferral count; engine stats.
- **Settings → Plex / Connect**: the backup targets list (up to 4), each with destination,
  schedule and enabled.
- **System → Tasks**: retention schedules per engine destination; a deferred job as the reason a
  fire was skipped. **System → Status**: engine versions and availability.
- Every new page follows phase1.md §10's phone rules.

## 16. Docs and deployment notes

- **README**: off-site destinations; choosing restic or rclone (restores, costs, deduplication,
  kept-file semantics D29); encryption and the recovery kit (what it contains, where to keep it:
  a password manager and a paper copy; that Bunkarr cannot recover a lost kit); restoring without
  Bunkarr (the kit's steps); B2 and S3 costs (class B and C transactions, restic's pack size, egress
  for verify samples and prune, B2's free egress allowance); the restic cache in
  `<config>/cache/restic` (it can reach several GiB for large repositories; it holds encrypted data
  only); windows and bandwidth; the restic repair procedure (§6.6).
- **README** also: a bucket-restricted B2 application key with only the capabilities restic and
  rclone need (never the master key), and Object Lock for users who want the off-site copy to
  survive a compromise of `/config` (hard deletes cannot be undone); a self-signed S3 endpoint
  needs `caCert`; a file larger than the window fails unless the window may overrun.
- **Compose example**: `hostname: bunkarr-<server name>` (unique per install: restic's stale-lock
  detection trusts the host name, §6.7), `stop_grace_period: 60s` (restic removes its lock on
  SIGINT), and the optional `BUNKARR_RESTIC_PATH` / `BUNKARR_RCLONE_PATH`.
- **Unraid template notes**: the same settings; never two containers with the same host name on
  the same repository (a second install, the dev image, an old and a new container during a
  replace).
- **DEFERRED.md**:
  - add: changing a restic password (`restic key`); rebuilding records from a snapshot on attach
    (Phase 5); forgetting orphan groups by hand; automated restic repair; `rclone rcd` to batch
    moves in one process; restic's native `s3:` backend as an option (D21); a Bunkarr database
    backup to engine destinations (the Phase 1 row stays, with the kit as the minimum); forgetting
    snapshots of other `engine_tag`s by hand (an earlier install); B2 hide-only deletes
    (`settings.b2.hardDelete: false` plus a lifecycle rule, §19);
  - remove: "Bandwidth limiting" (Phase 1).
- **CHANGELOG**, **ADR 0008** (`docs/adr/0008-offsite-engines.md`, "Off-site engines: restic and
  rclone, encryption custody and the recovery kit": D17–D23, D31–D33, S21–S29, with S29 as its own
  section) and **`openapi.json`**.

## 17. Order of work and uncertainties

Order of work (each slice ends green on lint, race, crash, web and e2e, plus `make test-engines`
from slice 13 on, and is committed on its own):

12. **Foundations**: schema 0004 and the jobs contract (done); `engines/proc`, `engines/bwlimit`,
    `enginetest.FakeRunner` with the fixtures; `jobqueue` deferral, `not_before`, slots and the
    scheduler rule; `destinations` columns, kinds, credentials, settings and bandwidth validation.
13. **rclone driver**, destination create, test, attach and host keys, the recovery kit, and
    `make test-engines` for rclone. The §17 assumptions about rclone are confirmed first.
14. **The planner extraction** (`syncer.Planner`, Phase 1–3 suites unchanged), then `enginerun`
    with the rclone sync, verify and retention, the reconciliation and the crash matrix.
15. **restic driver** and `enginerun`'s restic sync, verify and retention (`KeepMedia`, the forget
    requests), with the real-binary tests.
16. **Config versions off-site**: `VersionStore`, the three runners' engine paths, backup targets.
17. **Bandwidth and windows** on all three engines (filecopy included).
18. **UI**.
19. **Docker acceptance** (`make test-offsite`), docs, ADR 0008, and a manual pass against real B2
    and the real UNAS.

Uncertainties, each checked by a real-binary test (§14.4) in its slice before the code relies on
it:
- restic resume: how much of a killed or SIGINT-stopped backup is reused depends on when restic
  writes its index (the spike's kill after 6 s reused nothing). Batches bound the loss either way,
  and the window cap (§6.2 step 3) keeps a batch inside one window, so progress never depends on
  reuse.
- restic `ls --json`: its fields and mtime precision in 0.18.1, and its time on a 200k-file
  snapshot. The read-back runs after every batch and is never skipped or sampled; a slow listing
  lowers the default `batchFiles` instead.
- restic: `--files-from-raw` with whole directories and `--exclude-if-present`; `restore
  <snapshot>:<path> --include-file` (0.17+); whether `check` takes an exclusive lock; the effect of
  `-o rclone.connections`.
- `RCLONE_BWLIMIT` and the `RCLONE_CONFIG_*` variables reaching `rclone serve restic` through
  restic (the spike confirmed the credentials, not the bandwidth variable).
- rclone: how many `--max-delete` units one backup-dir move costs on each backend (a review probe
  on MinIO showed that `--max-delete 0` refuses the delete after the server-side copy, exit 7);
  how `move --files-from-raw` treats a listed file that is gone; whether a `moveto` onto an
  existing object replaces it on every backend (§7.3 assumes so); the log lines of an update on
  SFTP and B2.
- B2: not tested in the spike (no account). Hard deletes, `backend cleanup` scoped to a prefix and
  `b2_authorize_account`'s `allowed.bucketId` are taken from the documentation until the manual
  pass (§14.6, item 9).
- The restic snapshot `paths` size with tier rules that scatter files (compression keeps it to the
  number of whole directories plus files in partial ones; a benchmark at 200k files runs outside
  `-short`).

## 18. Open questions

Each open question has a default that this document already applies; where the review offered a
choice, the default is the safer option. The user can change any of them before its slice starts.

1. **The kit gate** (S21): no real job runs until the recovery kit custody is confirmed (a
   generated secret: exported and check code confirmed; a secret typed at create: typed again).
   Alternative: a warning only. Default: the gate.
2. **restic through rclone for S3** (D21). Alternative: restic's native `s3:` backend, with a fixed
   upload limit per batch instead of timetables. Default: through rclone.
3. **Default snapshot retention**: 7 daily, 4 weekly, 6 monthly, 0 yearly, plus `deletedDays` 30
   (deleted files and updates' old versions are held by references, so buckets are extra history).
4. **Kept files on restic** (D29): backed up again when they change; the old version is kept as a
   `replaced` row for `deletedDays`. Alternative: keep the old snapshot for as long as the file is
   kept, which holds the whole snapshot. Default: back up again, old version for `deletedDays`.
5. **Storage credentials in the kit**: off by default.
6. **Upload slots**: 2 engine syncs at a time.
7. **Windows** do not apply to Plex DB, *arr and manifest jobs (limits do).
8. **A file larger than the transfer window** (§9.2): it fails its item with a warning every run,
   and the job completes. Alternative: `window.allowOverrun` on by default (the file starts at the
   window's opening and runs past its end). Default: off; the window is the user's explicit
   bandwidth decision, and the warning names the file and the fix.
9. **Fresh password for off-site targets** (S29): the API key and the local bypass cannot send data
   to a new place. Alternative: allow the API key (automation) and only log it. Default: refuse.
10. **SFTP passwords shorter than 8 characters** are refused (S22's redaction cannot cover them).
    Alternative: accept with a warning. Default: refuse; a key is the better choice anyway.
11. **B2 deletes**: hard deletes stay (as revision 1), with the warning for keys that are not
    restricted to the bucket and the README's Object Lock advice; hide-only deletes are DEFERRED
    (§19). Alternative: hide-only deletes by default, which needs a lifecycle rule and a custom
    restic `rclone.args`.

## 19. Decision log (review of revision 1)

One line per finding: accepted, accepted in part, rejected or deferred, and why. DS = data safety,
SE = security, CO = correctness. The findings list this revision received ended inside CO-4; any
later finding was not seen and is not covered here.

| # | Sev. | Finding | Decision |
|---|---|---|---|
| DS-1 | critical | restic records assume NULL = in the newest snapshot, which restic does not guarantee (vanished, unreadable, changed files; failed batch-1 updates; reconciliation) | **Accepted**, merged with CO-1: S7 is a content read-back with `restic ls --json` (sizes and mtimes; `restic diff` has none), NULL means "in the base", references follow the COALESCE rule, reconciliation reads back instead of assuming done (§6.2 steps 1 and 5, §6.3, D31, tests (a)–(e), crash-matrix assertion). The separate pre-batch pin is folded into the recording transaction, which changes the base atomically, so it would add nothing. |
| DS-2 | critical | the pin and the retain overwrite an existing `engine_ref` with a snapshot lacking the file | **Accepted**: `engine_ref = COALESCE(engine_ref, base)` everywhere; a reference is never moved, only cleared when a new snapshot holds the version (§6.2 steps 5–6, §6.3, §6.4, S10(b); held-over-three-syncs and held-then-deleted tests; retention matrix with held items). |
| DS-3 | high | dropping the S6 retain wait on restic lets an upgrade's old version expire while the new one is not off-site | **Accepted**: the S6 wait applies unchanged through `syncer.Store`; retains and releases are decided after the last batch (S6, §6.2 step 6, test). |
| DS-4 | high | a restic update leaves no row for the old version, so it is held only by buckets | **Accepted**: a `replaced` retained row with the old snapshot as reference and `deletedDays` expiry, D29 updates included; both expiry holds apply (§6.2 step 5, §6.5, §14.5 variant). |
| DS-5 | high | snapshot groups are not unique across installs, re-created destinations and shared repositories | **Accepted**, merged with CO-3: `destinations.engine_tag` (random, unique) in every tag and filter; unparseable or foreign tags are never forgotten; attach warns about recent foreign snapshots (D32, §4.5, §6.1, S24, 0004). |
| DS-6 | high | the rclone reconciliation deletes the only copy of an equal-size update's old version | **Accepted**: reconciliation never deletes; the retention object is recorded retained and the live record is present only if it matches the catalog (§3.3, S23, equal-size update in the crash matrix). |
| DS-7 | high | `--iexclude` of a source's case-sensitive patterns, and unanchored `/` patterns, exclude files the catalog includes | **Accepted**: defaults to `--iexclude-file`, source patterns to the case-sensitive file, `/` and anchored patterns rooted, over-matching patterns dropped; subset table test and a real-binary comparison (§6.2 step 4, §14.1, §14.4). |
| DS-8 | high | a file longer than the window is never started, the job defers forever and blocks every later sync | **Accepted**: such a file fails with a warning (opt-in `window.allowOverrun`, open question 8), a two-window backstop for unknown rates, the restic batch cap and single-file batch, a sync fire supersedes a deferred sync, a warning at the first such deferral, pack reuse after SIGINT confirmed in §14.4 (S27, §6.2 step 3, §9.2, §11.2). |
| DS-9 | medium | a window cutoff mid-update leaves a `present` record whose old version can expire | **Accepted**: the live record becomes `missing` in the after-batch transaction (§7.3, §9.2, test). |
| DS-10 | medium | `restic unlock` before S25, in dry runs, and with a shared host name removes live locks | **Accepted**: S25 first with `--no-lock`; no unlock in dry runs, syncs, config jobs or read-only steps; unlock only before an exclusive operation after a lock inspection; unique host names; two-container test (S9, S25, §6.5, §6.7, §16, §14.6 item 11). |
| DS-11 | medium | `unlock --remove-all` does not count config jobs as active | **Accepted**: active = any running or deferred job of the destination, any type; the handler holds `dest:<id>` (§6.7, §9.3, §12, api test). |
| DS-12 | medium | `moveto` replaces an unmanaged object at the target | **Accepted**: `statMany` of every move target and displace first (not `--backup-dir` on `moveto`, whose behaviour is unconfirmed); §14.4 records rclone's overwrite behaviour (S2, S23, §3.3, §7.3). |
| DS-13 | medium | `backend cleanup` runs on the whole bucket with a 24 h max-age | **Accepted**: scoped to `<bucket>/<prefix>`, max-age ≥ 7 days or twice the longest upload; overlap compared on the normalized storage location across kinds (§7.5, §4.2). |
| DS-14 | medium | a secret typed at create counts as confirmed; restic trims whitespace | **Accepted**: create with a user secret needs a re-entry (or the kit flow); attach stays confirmed; surrounding whitespace and control characters refused (S21, §5.2). |
| DS-15 | low | KeepMedia can forget the newest recorded snapshot, and a missing parent breaks every sync | **Accepted in part**: the base is kept by KeepMedia and S24; a missing recorded snapshot is detected at preflight (warning, records `missing`, no `--parent`). **Rejected**: recording unrecorded snapshots at retention preflight, because recording one would change what NULL means without a read-back; such snapshots are kept while newest and otherwise age out (§6.2 step 1, §6.5, §14.5 variant). |
| SE-1 | high | the API key or the local bypass can create an attacker's destination and send the Plex token, *arr secrets and library there | **Accepted**: S29 (session plus password on every route that chooses where data goes or relaxes encryption), D33, acceptance 9, §12, api and Docker tests, ADR 0008. |
| SE-2 | high | Plex DB versions have no mode gate; the *arr flag carries over to remote targets | **Accepted**: Plex and *arr versions refused on unencrypted remote destinations (400, job fails); `acceptInsecureModes` per target and only for local destinations; Plex gets the per-target flag (§8.4 step 6, §8.5, tests). |
| SE-3 | medium | host-key pinning can fail open, be injected into `known_hosts`, or be skipped by Test and the kit | **Accepted**: keys parsed and typed at save; `knownhosts.Line` rendering; the sftp env builder fails without keys; Test without keys runs nothing; the kit carries the `known_hosts` lines (§4.2, §4.4, §4.5, §5.2, tests). |
| SE-4 | medium | random-IV obscuring registers a string no child sees | **Accepted**: deterministic IV (HMAC of the destination and field under a key derived from `bunkarr.key`); request-scoped registration for Test and Create; acceptance 4 reveals every token and uses argv shims (§4.4, §14.6 item 4). |
| SE-5 | medium | the flag deny-list misses `--log-file`, `--rc*`, TLS and password-command flags | **Accepted**: per-binary subcommand and flag allow-list, `-o` limited to two keys, exact env names, TLS never disabled with an optional `caCert` (S22, §4.2, §10.1, proc tests). |
| SE-6 | medium | `remote` fields could map to arbitrary rclone options | **Accepted**: closed per-kind env table, `DisallowUnknownFields`, field validation, agent and password prompts off (§4.2, §4.4, env key-set test). |
| SE-7 | medium | the kit's shell lines and `rclone.conf` can be injected through the prefix and path | **Accepted**: control characters refused; `shellQuote` for every value; CR/LF refused by the renderer; `sh -n` and round-trip tests (§4.2, §5.2, §14.1). |
| SE-8 | medium | struct values, credential field names, short secrets and JSON-escaped secrets escape redaction | **Accepted**: `LogValuer` and friends on secret types, recursive `slog.Any` redaction, more sensitive key names, SFTP secrets ≥ 8 characters (open question 10), JSON-escaped forms registered, redaction before the 1 MiB cut (S22, §10.2, §13.2, tests). |
| SE-9 | medium | a Test with an id can send stored secrets to a new host | **Accepted**: `SecretsFor` returns location and secrets from one row; `/destinations/test` takes no id; `{id}/test` takes no connection fields (§4.3, §4.5, §12, tests). |
| SE-10 | medium | the immutable encryption secret is resealed with every credential rotation | **Accepted**: `destinations.encryption_secret` column, written only by Create; merges inside the write transaction; `{}` refused (0004, §4.3, S21, concurrency test). |
| SE-11 | medium | binary paths as API-writable settings choose the program that receives every secret | **Accepted**: `BUNKARR_RESTIC_PATH` / `BUNKARR_RCLONE_PATH` bootstrap variables, validated at start-up; the setting keys removed (§4.4, §10.1, §13.1, 0004 header). |
| SE-12 | low | netguard checks the endpoint host, not the host rclone dials | **Accepted**: both the endpoint host and `<bucket>.<endpoint host>`, whatever `forcePathStyle` says; the Outposts alias bucket suffix `--op-s3` refused; `region` validated (S25, §4.2, real-rclone tests). |
| SE-13 | low | secret files on the appdata disk end up in pool snapshots and backups | **Accepted**: secret files on tmpfs when `statfs` confirms it, with a warning fallback (S22, proc test). |
| SE-14 | low | the kit endpoint allows the local bypass, lacks constant-time compare, audit and a safe header | **Accepted**: session-only, constant-time compare, a notification on every export, `mime.FormatMediaType`, confirm needs a session (§5.2, §11.5, tests). |
| SE-15 | low | a create that fails after `restic init` loses a generated password | **Accepted**: the pending row is inserted before init and kept after it; `pending:` markers run no job; crash point `create.afterInit` (§4.5, S25, 0004). |
| SE-16 | low | B2 hard deletes with an unrestricted key let a `/config` compromise erase the off-site copy | **Accepted in part**: the unrestricted-key warning and README advice (restricted keys, Object Lock). **Deferred**: `settings.b2.hardDelete: false`, because restic's hard delete comes from its default `rclone.args`, which S22's allow-list does not let Bunkarr override yet, and hidden files need a lifecycle rule; revisit with the manual B2 pass (open question 11). |
| CO-1 | critical | step 5 marks items done without an error line although restic omits files silently | **Accepted**, merged with DS-1. The `-v` alternative is rejected: `verbose_status` lines carry no size or mtime and are lost in a crash; `ls --json` covers both, and the reconciliation re-runs items it cannot confirm. |
| CO-2 | high | the batch-1 pin overwrites references; a reverted file is never put back into a snapshot | **Accepted**: COALESCE pin (DS-2); I_k holds every live file without a pending, held or failed item whatever its reference, and the read-back clears the reference (§6.2 step 4). |
| CO-3 | high | tags built from row ids collide across databases (groups, reconciliation, version adoption) | **Accepted**, merged with DS-5; a per-row `engine_tag` rather than a per-install id, because it also separates a destination deleted and re-attached in the same database. |
| CO-4 | high | `--max-delete` counts backup-dir deletes, so copies onto `missing` or unrecorded objects stop with exit 7 (text truncated after this) | **Accepted**: n counts every copy and update item of the batch (doubled if §14.4 shows two units per move); after an exit 7 every object in the batch's backup dir is recorded retained (S23, §7.3, tests). |

## 20. Revision 3: Phase 4 as built

This section records Phase 4 as it was built (slices 12–19 of §17) and is part of the contract:
where it differs from §1–§18, this section applies. Decisions are summarized in ADR 0008.

### 20.1 Acceptance results

Measured on the development machine (Docker 29.7, arm64; the SFTP server emulated as amd64)
with `docker/test-offsite.sh` (`make test-offsite`) against the image built from this checkout
(the derived test image replaces its binary with this checkout's e2e build either way), the
binary suite (`go test -tags e2e ./internal/e2e/...`) and `make test-engines`. The table records
the first full run. After the fixes of §20.4 and of the Phase 4 code review, the whole suite ran
again on the final tree: `make test-offsite` in full, not sharded, passes in 1178 s (every test
but `TestDockerOffsiteB2`, which is manual and skips), and so do `make test-engines`,
`make test-docker`, `make test-arr`, the binary suite and `go test -race ./...`.

| §14.6 | Test | Result |
|---|---|---|
| 1 | `TestDockerOffsiteTwoDestinations` | passes (41 s): filecopy on a volume (`0 2 * * *`, deletedDays 30) and restic on MinIO (`0 3 * * *`, daily 7, deletedDays 14) for one source; both schedules run at once and one sync runs while the other is queued or running; every record was written by a job of its own destination, `engine_snapshots` only for restic, the volume holds no restic data and the bucket prefix only restic's; a retention run of each (rows made to expire) expires only its own row and leaves the other's records and snapshots unchanged. |
| 2 | `TestDockerOffsiteRetention` | passes (71 s): the §14.5 series made with `restic backup --time` under the row's `engine_tag` and recorded as the table says, plus an unrecorded 2026-09-09 04:00 snapshot and one of another `engine_tag`; the dry run lists the forgets and changes nothing; the real run expires the 2026-08-04 row first and forgets exactly the 28 snapshots of the table; prune and `restic check` pass; with every keep value 0 only the newest (04:00), the base (09-09 02:00), 08-20, 08-12 and the other tag's stay, and `restic check --read-data` passes. The container clock is weeks after the series, so every run is also the "long after the last snapshot" variant. |
| 3 | `TestDockerOffsiteLifecycle` | passes (340 s) since the fixes of §20.4; the first run failed one check (rclone `bytesUploaded`, §20.4) and passed everything else. restic on MinIO, rclone crypt on MinIO and rclone crypt on SFTP: the initial sync; the change set gives exactly filesCopied=1, filesUpdated=1, filesMoved=1, filesRetained=1 on all three, with the deleted and the replaced version held (restic: retained rows with their snapshot; rclone: rows and objects under `.bunkarr/retention/`); `bytesUploaded` is the changed and added files only (2 625 536 bytes) on all three (the first run also counted the replaced version on rclone crypt on MinIO, and the first fix then left out the update on SFTP, §20.4); verify passes; a pack damaged in MinIO (restic) and a file object damaged in MinIO (rclone crypt) are found by the next verify (§20.4 for how); `docker kill -s KILL` 20 s into a 400 MiB upload at 4 MiB/s and `docker start`: the same job resumes as attempt 2, completes, and the destination verifies, on restic and on rclone; the source tree is unchanged by every job (S1). |
| 4 | the audit of every test, `TestDockerOffsiteSecrets` | every test's audit passes: no secret (S3 keys, the SFTP key's lines, its passphrase and the account password, restic and crypt passwords and every rclone-obscured form, a wrong secret sent to `POST /destinations/test`; in the config test also the Plex token and the Radarr key) in the argv log, the argv of any process the sampler saw under the server, the process log at debug level, any job log, any API answer but the kits, or any file of the container but the sealed database; no run directory is left. `TestDockerOffsiteSecrets` (163 s: restic and crypt on MinIO, crypt on SFTP with a key and with a password, test, sync, change, sync, verify, retention with prune, a credential rotation, all at 2 MiB/s) also requires what the sampler saw: 1462 samples, 1395 with an engine process, 91 secret files (named `password` and `known_hosts` among them), each 0600 in a 0700 directory under `/dev/shm/bunkarr-run`, none under `<config>/run`. |
| 5 | `TestDockerOffsiteBandwidth` | passes (352 s): 40 MiB at 2 MiB/s took 22 s (restic) and 20 s (rclone); a timetable switching from 1 MiB/s to 8 MiB/s at a minute boundary about 12 s into both transfers showed both limits (`limitBytesPerSec` 1048576 and 8388608) on both engines; a `BUNKARR_TEST_WINDOW` closing 15 s into 100 MiB at 2 MiB/s deferred each job (queued, `notBefore` at the reopening, one deferral, no failed item), which resumed then, completed with every file and verified; a 100 MiB file in a 41 s window at 2 MiB/s failed its item with "larger than the transfer window" while the other file was backed up (`completed_with_warnings`). |
| 6 | `TestDockerOffsiteKit` | passes (21 s): destinations created with the defaults are encrypted (restic, crypt) with a generated secret; sync, verify and retention answer 409 until the kit is confirmed (a dry run runs); the API key cannot export the kit and a wrong password is refused; after the export and the check code the sync runs; in a fresh `golang:1.27-alpine` container with only `apk add restic rclone` and no `/config`, a script built from the kit's text lists the snapshots and restores one file per engine (`restic restore` and `restic dump`, `rclone copy` through crypt) with the source's sha256. |
| 7 | `TestDockerOffsiteConfigVersions` | passes (96 s): Plex DB (a stopped server's data directory), *arr (Phase 2's fake Radarr, served in the network by this package's test binary) and manifest versions on restic and rclone crypt destinations; listed with `engineRef` on restic; the newest manifest downloads with its sha256 equal to `X-Bunkarr-SHA256` and the recorded checksum; after three more Plex DB and *arr versions with daily 2 (weekly 0) two of each are listed; rclone purged the older version directories at once, restic still holds 4 snapshots per kind until the next retention job forgets the 4 pruned versions. |
| 8 | `TestUpgradeFromPhase3` (binary) | passes (7 s): the Phase 3 binary (`git archive` of `7d3477d`) writes schema 3 with two filecopy destinations and their jobs; the Phase 4 binary on a copy migrates to 4, queues no job and no schedule by itself, shows the destinations as local/filecopy/unencrypted without a retention schedule, plans identical dry-run items and stats, and applies them. |
| 9 | `TestDockerOffsiteB2` | not run: it needs a B2 account (manual pass before the release). |
| 10 | `TestDockerOffsiteS29` | passes (36 s): creating an S3 destination, rotating its credentials, pinning SFTP host keys, linking a source, adding a Plex backup target and accepting an unencrypted remote answer 403 with the API key and through the local bypass (with the right password in the body); a session with the right password gets 2xx; a wrong password answers 400, and repeated ones get 429 for the password check and for logins alike; an existing destination's sync and verify still run with the API key. |
| 11 | `TestDockerOffsiteTwoContainers` | passes (112 s): the second container (same host name) attaches the repository (confirmed at creation, with the "another Bunkarr" warning) and backs up 160 MiB at 2 MiB/s; the first's retention with prune fails with "another restic process with host name bunkarr-e2e-offsite …" while the second's lock is held; the second's snapshot lists both files with their sizes; `restic check --read-data` passes, and the first's prune runs once the second is idle. |

`make test-engines` passes (engines 0.01 s, bwlimit 5 s, enginetest 0.1 s, filecopy 0.5 s, proc 12 s,
rclone 92 s, restic 117 s, enginerun 124 s; on the final tree rclone 86 s, restic 148 s, enginerun
127 s), and so does the binary suite (`go test -tags e2e -count=1 ./internal/e2e/...`, 53 s; 59 s on
the final tree).

`make test-offsite` builds a derived test image FROM the image under test (an e2e build of this
checkout over `/app/bunkarr`, the argv shims of `docker/offsite/argv-shim.sh` as
`/opt/bunkarr-e2e/bin/{restic,rclone}` named by `BUNKARR_RESTIC_PATH`/`BUNKARR_RCLONE_PATH`, and
curl, so PUT and DELETE requests and request bodies never go through an argv), starts MinIO, the
SFTP server and a tools container (restic, rclone, the media volume read-write) on one network,
and ends every test with the secrets audit of acceptance 4 on every Bunkarr container: the argv
log, a `/proc` sampler (`docker/offsite/sampler.sh`, about every 100 ms, the server's descendants
only), the process log at debug level, every job log, every API answer except recovery kits, and
every file of the container's own layer and writable mounts except the sealed database, each for
every secret in clear, JSON-escaped and as any base64url token that `rclone.Reveal` turns into a
secret. The real-B2 test (`TestDockerOffsiteB2`) is manual: it runs only with
`BUNKARR_E2E_B2_KEY_ID`, `BUNKARR_E2E_B2_KEY` and `BUNKARR_E2E_B2_BUCKET` set and never in CI.
`BUNKARR_E2E_OFFSITE_HOLD` (a duration) keeps a failed test's containers for inspection.

### 20.2 Deviations that are now the contract

- **Session (§3.1).** `engines.Session` carries `Caps`, `PlanFS`, `List`, `Versions` and `Close`.
  `Run`, `Verify` and `Retain` are not interface methods: they live in `internal/enginerun`'s
  concrete restic and rclone sessions, because they need the syncer's record store; their
  semantics are those of §6 and §7. `Engine.Create` returns `CreateResult{MarkerID,
  Initialized, Warnings}` (Initialized keeps the pending row after a failure past `restic init`).
- **Listings (§3.2).** `internal/engines/filecopy` has no `List`: `GET /destinations/{id}/snapshots`
  composes the Phase 1–3 `snapshots` rows with `enginerun`'s media snapshots (`kind: "media"`,
  `sourceId`, `engineRef`, `complete`, `files`, `dataAdded`) for engine destinations, newest first.
- **Remote updates (§4.2, §12).** `PUT /destinations/{id}` takes the whole `remote` object; the
  store refuses any difference but `hostKeys` (sftp) and `caCert` (s3). A partial remote is a
  validation error, not a merge.
- **SFTP paths (§4.5).** Create and Test list the path; a folder that does not exist on the server
  is "bucket or path not found" (rclone exit 3). The user creates it first.
- **Job status.** An engine job with warnings (a check error, a failed sample, a file larger than
  the window) ends `completed_with_warnings`, as Phase 1–3 jobs do.
- **Upgrade test binary.** `TestUpgradeFromPhase3` takes the Phase 3 binary from
  `BUNKARR_E2E_PHASE3_BINARY` (else `git archive` of the release commit, `7d3477d` or its public
  mirror `fab6b96`); `BUNKARR_E2E_PREVIOUS_BINARY` stays the Phase 2 binary of
  `TestUpgradeFromPhase2`, since one variable cannot name both. With `CI=true` both tests fetch a
  missing release commit by hash (from `origin`, then from the public GitHub repository; 2-minute
  timeout) and fail instead of skipping when they cannot; locally a missing release still skips.

The implementation and the code review (§20.4, §20.5) settled the following; each has a test.

- **Command lines (§10.1, S22).** Extra environment name `RCLONE_CA_CERT` (the only way restic's
  rclone backend gets a `caCert`). Extra flags: rclone `--retries`, `--count`, `--size`; restic
  `--pack-size` on backup and prune, not on `init` (it is not a repository setting); restic
  `list snapshots` and `cat snapshot <id>`; `restic check` without `--no-lock` (it runs after
  `GuardedUnlock`). Stricter: rclone paths are absolute local paths or `BKDEST:`/`BKCRYPT:`;
  `move`, `moveto`, `delete`, `deletefile` and `purge` take remote paths only; every rclone command
  carries `RCLONE_CONFIG=/dev/null`. An argument is refused whenever the output redaction would
  change it (the process-wide registry and the command's own `Redact`, clear or JSON-escaped; a
  destination's own access key id and B2 keyId included), and so is a `--files-from-raw` list in
  the command's data directory whose entries would all read back redacted (one entry is enough for
  an rclone listing; a single such file of a restic batch fails only its own item). A bucket,
  prefix, destination folder or source root that contains a registered value therefore fails at
  run time (DEFERRED: refuse it with 400 at input).
- **Long restic listings (§6.1).** `restic snapshots --json` prints one line. A listing too long
  for the exec layer's line limit is read as `restic list snapshots`, then `snapshots --json <ids>`
  in parts of 32 (halved while a line is still too long) and `restic cat snapshot <id>` for one
  snapshot too long by itself, filtered by tag and sorted by time in Go.
- **Credentials (§4.3, S29).** An SFTP update that sends `privateKey` or `password` replaces the
  whole login (key, passphrase and password); one that sends only `privateKeyPassphrase` keeps the
  stored key, and a stored passphrase is kept when it unlocks a newly pasted encrypted key.
  Removing a stored `caCert` or `hostKeys` (a missing or null field reads as empty) changes trust
  and needs S29.
- **crypt secrets (S21, §5.2, §5.3).** `encryption.secret2` sets crypt's `password2`; without it a
  user `secret` sets `password` only and `password2` stays rclone's default. It gets the checks of
  `secret` and is refused for restic, filecopy, mode `none` or without `secret`. Re-attaching a
  crypt remote whose kit lists a password2 needs both (an attach with only the password fails and
  names the kit's "rclone crypt password2"). A create with `secret2` is confirmed by the kit's check
  code, not by re-entry. A resumed create is not custody confirmed; only a user's attach is.
- **Kit and remotes (§5.2, S25).** The check code hashes the destination id (§5.2). A value that
  rclone's config parser reads back differently (leading or trailing white space, NBSP and NEL
  included; a leading backtick or `"""`; `%(name)s` interpolation) is refused by the kit's
  `rclone.conf` renderer and by `rclone.Env` for every crypt command, so no crypt backup is written
  where the kit cannot name it. S3 buckets ending in `--op-s3` (Outposts aliases) are refused, and
  netguard checks the endpoint host and `<bucket>.<endpoint host>` whatever `forcePathStyle` says.
- **Drivers (§3, §6, §7).** rclone `Capabilities` take the destination, not the kind (crypt decides
  `EnforcesModes`). An unanchored exclude pattern without `/` becomes `<root>/**/<glob>`, so it
  never matches the source root's own path components. `ReadFile`/`Fetch` use `restic restore` or
  `rclone copyto` (byte-exact); `Dump` and `Cat` are text-only. `KeepMedia` uses restic's bucket
  semantics (the last N periods that have a snapshot, not counted back from now), `Now` only
  protects snapshots dated in the future, its input gained `LiveSources`/`LiveIntegrations` for
  orphan groups, and `FilterForget` filters forget requests. `Move` only moves a destination folder
  into retention. `Refs` includes every retained row whatever its expiry (an expired row that a
  hold keeps still keeps its snapshot). A failed rclone create removes its marker (on its own 60 s
  budget) before the pending row is dropped; if the marker cannot be removed, the pending row (the
  only copy of a generated secret) is kept and creating the same location again resumes it.
- **restic runs (§6.2–§6.7).** A source's base is the snapshot recorded last, not the one with the
  latest restic time (a clock set back). The include set's first rule reads the record (a file is
  included when its record holds the catalog version and it has no pending item). A promote is a
  content item, recorded with `PromoteTx` on the records only. Adoption is limited to the running
  job's own snapshot. A backup that exits 11 (an exclusive lock) runs `GuardedUnlock` and retries
  once, so a sync can unlock after all (§6.7 said it never does). A retention dry run computes what
  it would forget from the recorded snapshots without listing the repository; it keeps a deleted
  source's snapshots and leaves out forget requests for referenced ones. A "check after prune" that
  was interrupted runs in the next retention job. The upload rate is measured only on batches that
  mostly upload new data. The verify sample is limited to the config directory's free space, and a
  partial restore on a full disk fails the job instead of marking files missing.
- **rclone runs (§7.3, §7.6).** A damaged object cannot go to retention through `--backup-dir`
  (`copy` skips an object of equal size and mtime, and the allow-list has no `--ignore-times`): it
  is moved with `moveto` (recorded damaged) and uploaded again with `copyto`, and recorded present
  as it is only when this job already moved the damaged object into retention. Names rclone or
  restic cannot carry (line breaks, control characters) fail or skip only their own item. After a
  cancel the batch is still recorded. Progress bytes add up across batches.
- **Retention holds (§6.5, §7.5).** A `replaced` version's hold ends once a newer, not displaced,
  version of its path is recorded; a moved file takes its replaced and damaged versions to the new
  path; rows of unlinked (orphan) sources never expire; the holds are checked again when each item
  runs. The global retention job queues nothing for a destination whose create did not finish or
  whose kit is unconfirmed.
- **Windows (§9.2, S27).** A file may start when it fits the time left plus grace. One that does
  not fit waits while later files that fit still run, and the files that waited (up to 32 per
  destination, kept in the engine state) run first in the next attempt, whatever its plan. Without
  `allowOverrun`, a file that is the first transfer of its third window and still does not fit
  fails ("not started in 3 transfer windows"). With `allowOverrun`, a file larger than the window
  starts only within 30 minutes of the opening, or after one earlier wait. A restic batch cut by
  the window's end halves the file count and bytes of the next batches (a cut cap in the engine
  state that survives a superseding job), which double again when a batch ends with plenty of time
  left.
- **Deferral and supersede (§11.2).** A scheduled sync supersedes a deferred sync only if it covers
  that job's sources; never a release sync, and an `AllowChanges` sync only when the schedule has
  `AllowChanges` too. A superseded job records `superseded_by`; the superseding job's plan takes
  over the window cuts of matching items (path, source and planned size), and its `deferrals`
  continue the predecessor's count. `Job.Deferrals` therefore counts the consecutive windows of the
  chain, and the two-week warning (§9.2) fires; the comment in `internal/jobs/contract.go` still
  says per job.
- **Config versions (§8).** The legacy single backup form stays a valid stored form: `targets` is
  stored only when a client sends it, a single-form update replaces `targets[0]` and keeps the
  rest, and GET returns both forms only with `WithBackupTargets()`; `targets: []` means none and
  survives re-encoding. A single-form Plex update withdraws the first target's
  `acceptInsecureModes` (400 on a destination that does not keep modes); the UI always sends the
  full list. `acceptInsecureModes` is per target, and a Plex backup to a local destination that
  does not keep file modes fails without its target's flag (stale capabilities are probed again
  first). The runners check kit custody themselves. A manifest is "unchanged" on an engine when
  `SHA256SUMS` reads back with the recorded checksum and all three files are listed (no per-file
  sizes are stored). The restic version store removes leftovers under `<staging>/versions` and
  passes the destination's bandwidth limits.
- **API (§12).** Editing a backup schedule in System → Tasks also saves the time into the
  integration's target. `GET /schedules` returns `lastSkip`. While a Plex DB, *arr or manifest
  version store of a destination is open, `/unlock` answers 409, and a version store waits for a
  running unlock. Deleting a restic destination removes `<config>/cache/restic/<id>`. A restic
  destination on SFTP, S3 or B2 is blocked while rclone is unavailable (create and test answer
  400). `snapshotCount` counts media snapshots only (not the Plex, *arr or manifest versions in the
  repository) and is computed when read; `repositoryBytes` is not recorded (DEFERRED). The Phase 2
  test `TestArrBackupRefusals` now expects 403 for the API key (S29).
- **UI (§15).** The add-destination wizard is numbered sections of one dialog, still saved with
  "Save". The destination list stays a table whose rows carry the card's content, with a red banner
  for an unconfirmed kit and a "Finish create" action for a pending create. The first Plex/*arr
  target keeps the single-target fields; targets 2–4 are added below. An SFTP private key is typed
  into a visible text area (a password field drops its line breaks). Save warnings keep the dialog
  open. Previews say "to forget" and "to keep" and hide the figures a preview never changes.
- **Image and CI (§16).** restic and rclone are pinned in `docker/engines/versions.env`
  (`restic~0.18.1`, `rclone~1.74.1`), which both Dockerfiles read and the image test checks
  against `/system/status`; the release workflow runs `docker/test-offsite.sh` on the amd64
  candidate before tagging. The compose example requires `SERVER_NAME` (`docker/test-compose.sh`,
  in `make test-docker` and CI, checks it). On a PUID/PGID change the entrypoint also re-owns
  `/config/run`, `/config/cache`, `/config/staging` and `/config/backups`.

### 20.3 The uncertainties of §17, as measured

restic 0.18.1 and rclone 1.74.1 (alpine), MinIO RELEASE.2026-09-22 and OpenSSH 10.3
(`make test-engines`; the tests write each measured answer next to its assertion):

- restic `--files-from-raw` takes whole directories and files together; `--parent` works across
  snapshots with different path lists; `--exclude-if-present bunkarr.key` leaves the directory out.
- restic `ls --json` gives path, type, size and mtime with nanoseconds, equal to the filesystem's.
  On 200k files the backup took 2.6 s and `ls --json` 1.5 s (local repository): the read-back
  after every batch is cheap, and `batchFiles` keeps its default.
- A file deleted after the scan inside a directory listed whole is simply not in the snapshot
  (exit 0, no error line): only the read-back sees it.
- `restore <snapshot>:<subfolder> --include-file` restores just the included files.
- `RCLONE_BWLIMIT` reaches `rclone serve restic`: 8 MiB at 2 MiB/s took 4.06 s through the backend.
- Nothing a SIGINT'd (or killed) backup uploaded is reused by the next one (24 MiB interrupted at
  5 s: the next backup added all 24 MiB), so batches never rely on reuse, and a window cutoff or a
  kill costs at most one batch. Signalling the process group also stops `rclone serve restic`, so
  restic cannot remove its lock: the lock stays, and `GuardedUnlock` recognizes it as left by a
  child of this process. A `docker kill` leaves the lock too; after `docker start` the lock is
  recognized through `<config>/cache/restic/children.json` as left by a child of the killed
  process (§6.7), so the resumed job's verify unlocks at once instead of waiting 30 minutes.
- `restic check` takes an exclusive lock, so `GuardedUnlock` runs before it in verify and
  retention jobs.
- rclone: displacing one object into `--backup-dir` costs one `--max-delete` unit on S3 (a
  server-side copy plus a delete) and none on SFTP (a rename): n stays the batch's copy and update
  count. `moveto` onto an existing object replaces it on S3 and SFTP, as §7.3 assumes.
  `move --files-from-raw` with a listed file that is gone moves the others and exits 0.
- `check --download --combined` marks a changed file `*` and a file missing at the destination `+`
  (the spike index's note had `-` and `+` the other way round).
- `backend cleanup BKDEST:<bucket>/<prefix>` removes only unfinished multipart uploads under the
  prefix (MinIO lists none for a prefix, so the scoped command left the other prefix alone).
- `--max-duration <d> --cutoff-mode soft` exits 10, keeps the finished file and starts no other.
- B2 remains unmeasured until the manual pass (`TestDockerOffsiteB2`).

### 20.4 Found by the acceptance suite

- **rclone `bytesUploaded` counts server-side copies.** rclone's stats `bytes` includes
  `serverSideCopyBytes` (spike `sync-backup-dir.jsonl`: bytes 10 with 7 server-side), and on S3 the
  `--backup-dir` move of an update's old version is a server-side copy. The change set's sync on
  rclone crypt on MinIO reports 3 674 832 bytes for 2 625 536 bytes of changed and added files: the
  replaced version (1 MiB) is counted as uploaded. Fixed: `uploadedBytes` in
  `internal/enginerun/rclone_copy.go` counts **`bytes - serverSideCopyBytes`** (never below 0). A
  first fix also subtracted `serverSideMoveBytes`, and the next acceptance run failed on rclone
  crypt on SFTP (`bytesUploaded` 1 577 680: the 1 MiB + 4 KiB update left out). rclone 1.74.1
  counts a `--backup-dir` move on SFTP only in `serverSideMoveBytes`, never in `bytes` (it runs as
  a checking transfer), while S3's server-side copy is counted in both, so only the copy bytes are
  subtracted. Unit tests `TestUploadedBytes` (fails on the first fix with exactly 1 577 680) and
  `TestRcloneSyncLifecycle`; the fake rclone counts a `--backup-dir` move as rclone does.
- **A restic sample restore that meets a damaged pack fails the verify job.** When the damaged pack
  holds data of a sampled file, `restic restore` exits 1 ("There were N errors") and the whole job
  fails with that error and without stats, after its repository-check item already failed with the
  check's error count (in 3 of 5 runs; which pack is damaged decides). §6.6 wants the sample's
  items to fail (records `missing`) and the job to complete with warnings. The test accepts either
  as "found". Fixed after this run (the re-run confirms it: the verify of the damaged sample ends
  `completed_with_warnings`, "1 damaged or missing"): a restore that ends with restic's "There were
  N errors" (exit 1)
  is partial; the files it did not restore fail their items ("could not be restored from the
  repository") and become `missing`, the rest are compared, and the job completes with warnings. Any
  other restore failure still fails the job and marks nothing (`TestResticVerifySampleDamagedPack`).
- **A damaged rclone crypt object is "could not be checked", not missing.** A changed block does not
  decrypt, so `check --download` marks the file `!` (an error), not `*`; the sample item fails with
  a warning (the verify finds the damage), but the record stays `present`, so no later sync copies
  the file again. §7.6 marks only `*` and `-` records missing; whether a decryption failure should
  count as `*` is open.
- **After a kill, the resumed rclone sync adopts** a file the killed attempt finished (it counts
  only its own attempt's outcomes, as DEFERRED "Stats across a resume" says): `filesCopied +
  filesAdopted` is the file count.
- Contract clarifications, now §20.2: `PUT` takes the whole remote; an SFTP folder must exist;
  `POST /destinations/test` takes no `currentPassword` (it stores nothing, so it is not an S29 route)
  and refuses the field as unknown.
- Test infrastructure: a MinIO bucket must be created without `no_check_bucket` (rclone then does
  not create it and every write answers NoSuchBucket); arrtest's own backup zip holds an empty
  database, which the *arr verification refuses, so the suite's fake Radarr serves one with a small
  SQLite database.

### 20.5 Open

- `make test-offsite` has been re-run in full since the two engine fixes of §20.4 and the fixes of
  the Phase 4 code review, and passes (§20.1, 1178 s, not sharded). The restic verify of a damaged
  sample completes with warnings, and after the restic kill and resume the verify unlocks at once
  (the restart lock recognition of §6.7).
- The crypt-damage repair is a design question (§7.6, §20.4): a damaged crypt object is "could not
  be checked", its record stays `present` and no sync uploads it again.
- The manual passes: real Backblaze B2 (`TestDockerOffsiteB2`: hard deletes, unfinished large
  files, the unrestricted-key warning, a scoped `backend cleanup`) and the real UNAS.
- Not in the Docker suite: a kill during an SFTP upload (the SFTP server runs emulated on arm64),
  a timetable change on a local restic repository (per batch by design), and the orphan-group and
  second-source variants of §14.5 (unit tests and the retention crash matrix cover them).
- `make test-offsite` takes 20 to 30 minutes on the development machine (the lifecycle's two
  400 MiB uploads at 4 MiB/s and the bandwidth test's waits dominate). `docker/offsite/*.sh` pass
  shellcheck but are outside `make lint`'s `docker/*.sh` glob.
- The review items still open are rows of `DEFERRED.md` (Phase 4).
