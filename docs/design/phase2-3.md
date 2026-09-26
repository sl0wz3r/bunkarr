# Phase 2–3 design — *arr awareness, tiering and Plex sign-in

Status: **Phase 2 implemented**; Phase 3 is the contract for implementation. Revision 3
(2026-09-25).

Revision 3 folds in what the Phase 2 implementation changed: the deviations the slice authors
made that are now the real behaviour, and the fixes from the code review. The sections they amend
are updated in place, and §20 lists every change with its reason. Phase 2's acceptances (1–5 and
10) pass (§20.1). Phase 3 (tiers, the Plex index, Tautulli, Seerr, Maintainerr) is not built yet;
its sections are unchanged. Where Phase 2 code already has the Phase 3 hook (`manifest.Options.Tiers`
with `AllFull`), every file is `full`.

Revision 1 was written after three pieces of groundwork:
- the *arr fixture spike: real Sonarr 4.0.20.3014, Radarr 6.4.4.10685 and Lidarr 3.1.0.4875. It
  recorded 59 webhook payloads (`testdata/webhooks/`) and 58 API responses (`testdata/arr/`);
- a source-level study of the Tautulli, Seerr and Maintainerr APIs;
- a port plan for Dupearr's "Sign in with Plex".

Revision 2 folds in an adversarial review with four lenses (data safety, security, correctness,
completeness). §19 lists every finding and what became of it.

It extends `docs/design/phase1.md` (revision 4). The Phase 1 rules, contracts and tests still
apply unless a section here amends them by number. The schema lives in
`internal/db/migrations/0003_phase2_3.sql`, and the new job types and params in
`internal/jobs/contract.go`. Every package in §14 is built against this document; changing a
contract here means updating every package that uses it.

**Goal: spec §8, Phase 2 (*arr awareness).**
- Sonarr and Radarr integrations over API v3. They read series and movies, file ids, TMDB/TVDB/IMDb
  ids, quality profiles, root folders and tags.
- Webhook receivers. They handle Test, Download (including `isUpgrade`), Rename, file deletes and
  series/movie deletes, and each event triggers a targeted sync, never a full scan.
- Versioned backups of each *arr's configuration and database through its built-in backup API.
- A manifest export: JSON and CSV of every library item.
- Lidarr (API v1) is included (decision D2).

**Goal: spec §8, Phase 3 (tiering and prioritization).**
- An ordered tier rule engine.
- Conditions: *arr tags, quality profile, root folder, library section, file size, age, genre,
  "requested by" (Seerr), play count and last watched (Tautulli), Maintainerr pending deletion,
  and a manual "irreplaceable" flag.
- Actions: `full`, `manifest` or `skip`, each for chosen destinations.
- Read-only Tautulli, Seerr and Maintainerr integrations.
- A plan preview: what gets backed up and why, with the estimated bytes per destination.

Phase 2 ships, green and committed, before Phase 3 starts (§18, D16).

**User decisions that override the spec.**
- Media stays backed up in full by default. With no rules, every file is `full` and a Phase 1
  install behaves exactly as before after the upgrade. The spec's default rule set (only items
  tagged `bunkarr-full` in full, everything else manifest) is an optional preset.
- "Sign in with Plex" works like the sibling project Dupearr: a plex.tv PIN, then a server picker.
  The pasted `X-Plex-Token` stays available as the manual path. The user asked for this again
  after comparing screenshots of the two apps, so it is the first slice (§18).

Acceptance (the exact tests are in §15; all must pass):

Phase 2
1. **Import.** Importing a file in real Sonarr and in real Radarr (containers) puts that file at
   the destination within 60 s of the import. It is copied by a sync with trigger `webhook` whose
   only item that is not `skip` is a `copy` of the imported file with status `done`. For Sonarr the
   series already has backed-up episodes, and none of them is copied again.
2. **Upgrade.** A 720p → 1080p upgrade in Radarr and in Sonarr copies the new file and moves the
   old one into retention. Both happen in one targeted sync, the copy before the retain (S6). This
   holds when the *arr imports the new file by copying it from another volume, which puts minutes
   between its delete event and its Download event. A same-path replacement is an `update`: the
   new version is copied and the old one retained with reason `replaced`.
3. **Manifest round trip.** A destination's manifest version (read with `ParseDir`) and the
   export from `GET /manifest/export` (read with `Parse`) both parse. For **every** item of the
   real Sonarr and Radarr, whatever the tier rules and the path mappings, `ComparePlan` finds no
   difference between the manifest's re-import plan and the state the *arr reports: ids, title
   and year, root folder, quality profile name, monitored, tag labels, the §11.1 series fields,
   and each *arr file's relative path and size. The test decodes the *arr state itself from the
   raw API JSON.
4. **Webhook authentication.** The Test event of each app returns 200 with the integration's
   webhook key and queues nothing. A wrong or missing key returns 401, and so do Bunkarr's master
   API key and a request that only carries a session cookie. The webhook key gets 401 on every
   other route.
5. **\*arr backup.** A backup produces a verified zip at the destination, recorded as an
   `arr` snapshot. This works with the backup folder mounted and over HTTP when the *arr does not
   require a login for local addresses. Over HTTP with Forms login required, the job fails and
   the error says which two settings fix it. When the *arr has a fresh scheduled backup, that one
   is copied and no `Backup` command is sent.

Phase 3

6. **Rule change.** Changing a rule and running a sync dry run shows the tier, rule and structured
   reasons of the pinned table in §15 for every live file (`GET /tiers/preview/{id}/items`) and on
   every dry-run item. The rules use a real Radarr's tags and quality profiles, plus fake
   Tautulli, Seerr and Maintainerr servers that serve recorded fixtures.
7. **Maintainerr.** With a rule "pending deletion → skip" enabled, Maintainerr's pending items
   are not copied. With the rule disabled they are copied. With the Maintainerr cache older than
   its `staleAfterHours`, they are copied too, because unknown is never true (S14).
8. **Upgrade from Phase 1.** A Phase 1 database upgraded with no rules plans exactly what Phase 1
   planned. A second sync plans nothing. While no *arr integration exists, no job type that
   Phase 1 did not run is queued (no manifest export, no refresh).
9. **Demotion.** Demoting backed-up files from `full` to `manifest` or `skip` never removes them
   from the destination (S15). Only a release moves them into retention, with reason `released`,
   and a release applies exactly the files its dry run listed that are still not `full`.

Plex sign-in

10. **Sign-in flow.** Sign in, pick a server, test its connections and save, all against a fake
    plex.tv and a fake PMS. The result is an integration whose stored token is the server's own
    token (`TokenFor`). No API response contains a token; the PIN code appears only inside the
    `authUrl` of `POST /plex/signin`, and the PIN id never. The manual URL + token path behaves
    exactly as in Phase 1.

## 1. Safety rules

S1–S11 stay as written in phase1.md. S6, S8, S9 and S10 are amended below. S12–S20 are new, and
each has tests (§15).

- **S6 amendment (a retain waits).**
  - A retain waits only for live catalog files in its folder whose tier at this destination is
    `full`. A file that is deliberately not backed up there (`manifest`, `skip`) never makes it
    wait. Otherwise, under a manifest-by-default rule set, an upgrade's old version would wait
    forever for a new version that is never copied.
  - The wait set is defined on persisted data, so a resumed job computes the same set. A live
    catalog file in the folder blocks the retain when it is not backed up (phase1.md S6) and
    either:
    - this job has a `copy`, `update`, `link` or `move` item for it whose `detail.tier.tier` is
      `full` and whose status is not `done`; or
    - this job has no item for it, and the planning attempt's in-memory decisions do not say it
      is non-full.
  - A resumed job whose plan was complete before the restart holds no decisions, so every file
    that is not backed up blocks, as in Phase 1. The retain fails with the S6 warning, and the
    next sync plans again and retains it. No rule is evaluated by the wait check itself.
  - A release retain (S15) never waits: its own source file is still there and nothing replaces
    it.
- **S8 amendment (secrets).**
  - The API keys of Sonarr, Radarr, Lidarr, Tautulli and Seerr are handled exactly like the Plex
    token:
    - sealed with AAD `integration:<id>:apiKey` and write-only (`hasApiKey`);
    - held in the redaction registry;
    - read only through `TokenFor(ctx, id, url)`;
    - sent only as the `X-Api-Key` header, only to the stored URL, never in a query string;
    - redirects are never followed, and errors carry method and path only, never a URL, header
      or body.
  - Tautulli before 2.18.0 only accepts the key in a query string, so it is refused. Maintainerr
    has no API authentication, so its `apiKey` must be empty (400 when given).
  - **Webhook keys (D7).** Each Sonarr, Radarr and Lidarr integration has its own webhook key: 16
    random bytes, hex-encoded, generated when the integration is created. It is sealed in
    `integrations.webhook_key` with AAD `integration:<id>:webhookKey` and held in the redaction
    registry. The only response that contains it is `POST /integrations/{id}/webhook/key` (§13).
    It is accepted only by the webhook routes, and the webhook routes accept nothing else: not
    Bunkarr's master API key, not a session, not "disabled for local addresses". It is compared in
    constant time and never stored with an event. The request log already redacts `apikey`
    (`logging.RedactURL`).
  - Plex sign-in secrets are covered by S19. The Seerr client never decodes an e-mail or a
    `displayName` (§4.5); a manifest names Seerr users by id only.
- **S9 amendment (dry run).** Every new job type has a dry run, and none of them writes to another
  application.
  - `refresh` fetches, computes the changes to its cache, reports the counts, writes nothing and
    queues no follow-up.
  - `arr_backup` runs the preflight and records one `skip` item for the backup it would make. It
    does **not** send the `Backup` command, because that command creates a file in the *arr.
  - `manifest_export` builds the manifest, reports its counts and whether it would be a new
    version, and writes nothing.
  - A sync dry run records a `skip` item for every live file whose tier at the destination is not
    `full`, with the decision (§8.5), so the dry run is the per-item preview. The tier preview
    (§8.6) writes nothing at all.
- **S10 amendment (mass-change guard).**
  - **Releases.** S10(b) counts release items (S15) as changes, like retains.
  - **Unknown-promoted copies.** A `copy` of a file with no live record whose decision has
    `unknownPromoted` (§8.1: it is `full` only because a rule could not be decided) counts as a
    change. Above the thresholds these copies are held with "held: tier unknown because
    <integration> is not fresh", and a warning notification is sent. `allowChanges` runs them.
    The free-space check never fails a job because of them (§8.5).
  - **Targeted syncs never retain a name on their own (D14).** A targeted sync plans a retain for
    a vanished name only when the vanished name's directory also gets a `copy`, `update`, `move`
    or `link` in the same plan: an upgrade or a rename. Every other vanished name inside a
    targeted scope stays live and recorded until the next untargeted sync, whose guard sees the
    whole change. `ScanPaths` still marks those catalog rows deleted. So webhook syncs cannot cut
    a mass deletion into jobs that each stay under the thresholds.
  - A targeted sync counts its changes against the whole source's live files, as every sync does.
  - **Refresh guard.** Every refresh is all-or-nothing (§6): a failed fetch writes no deletions
    and does not move `refreshed_at`. Then:
    - *arr items: a full refresh that would mark more than half of an integration's items, and
      more than 20, deleted, or that got an empty list while the index has items, marks nothing
      deleted.
    - *arr files: a full refresh that would delete more than half of an integration's files, and
      more than 20, deletes none. Files under a root folder that the *arr reports as
      `accessible: false` are never deleted by a refresh (a job warning names the folder).
    - Tautulli and Seerr: a refresh that would remove more than half of the cache's rows, and more
      than 20, or that got none while rows exist, keeps the old rows and does not move
      `refreshed_at`, so the cache ages into unknown.
    - Maintainerr has no shrink guard: fewer pending rows can only protect more.
    - A held refresh ends `completed_with_warnings`, because the other application may still be
      starting up. `allowChanges` on a refresh applies what the guard held.
    - *(As built, *arr.)* A held full refresh does not move `refreshed_at` either: the index
      keeps the rows and goes stale, the status is `ok` and `error` holds the reason, and the
      Connect card offers "Apply held changes" (a refresh with `allowChanges`).
    - The index only feeds tier facts and scan targets, and a missing *arr item makes its facts
      unknown (S14), so this guard protects the tiers, not files.
- **S12 Webhooks are hints, never truth.**
  - A webhook is accepted only with the integration's webhook key (D7), sent as `X-Api-Key`,
    `?apikey=` or as the password of HTTP Basic auth. Bunkarr's master API key, a session cookie
    and the "disabled for local addresses" mode never authenticate it.
  - The payload is used for one thing only: choosing which *arr items to refresh. The refresh
    reads each item from the *arr's API, and the index maps the item's folder into a source
    (S18). That mapped folder becomes the scope of a targeted scan.
  - No payload path is opened or trusted. No event deletes, retains or marks anything: whether a
    file is gone is decided by scanning the source (S5), and a targeted sync never retains a
    vanished name on its own (S10).
  - So a forged or replayed event, even with the key, can cause nothing but a refresh of that
    integration's items and a scan of folders inside sources. The webhook key grants nothing else.
  - `eventType == "Test"` is handled before any lookup (Sonarr's Test uses `series.id = 1`, which
    collides with a real series). An unknown event type is stored and ignored.
- **S13 Webhook intake is bounded.**
  - **Credential first.** The key is compared first, in constant time, against an in-memory map
    of the integrations' webhook keys (no I/O). A request with a valid key is never refused by the
    failed-authentication limiter.
  - **Failed authentications** are limited per client address: 10 a minute, then 429. The limiter
    counts and blocks only requests without a valid key. It is separate from the login limiter,
    so bad webhook attempts cannot lock the UI out, and a stale key in one *arr cannot block the
    others behind the same proxy or Docker bridge.
  - **Rates.** Each integration may send 20 events per second, with bursts of 2000 (429 beyond).
    The intake answers 503 while 10,000 events are unprocessed.
  - **Bodies.** At most 4 bodies are read at a time; a request waits up to 10 s for a slot, then
    gets 503. A body must be `Content-Type: application/json`, at most 16 MiB (413 beyond), and a
    JSON object with an `eventType` string of 1–64 characters. It is decoded as a stream: only
    `eventType`, the item ids and the fields the event list shows are kept.
  - **What is stored** (§7.1): the item ids (`targets`) and a payload of at most 64 KiB. A larger
    body is stored as a summary with `truncated` set; an ignored event type stores `{}`.
  - The handler only stores the event and returns: no outbound call, no job logic, a few
    milliseconds. Processing is asynchronous and coalesced (§7.3).
  - Stored events are pruned after 30 days, keeping at most 50,000 rows and at most 256 MiB of
    payloads. The payload budget is checked on every insert: an insert that takes the payloads
    over it wakes the processor, which prunes the oldest events down to 90 % of the budget (a
    failed prune is retried). The request never waits for the prune and never fails because of
    it.
  - The *arrs never retry a failed webhook (spike). A missed event is caught by the next full
    refresh, which reconciles (D8), and by the scheduled syncs.
- **S14 Unknown never lowers protection.**
  - Every condition evaluates to true, false or **unknown**. Unknown means one of:
    - the source is not configured;
    - its cache is not fresh (§6: one definition, the age of the last complete refresh, for the
      integration's current URL). A failed attempt alone never makes a fresh cache unknown;
    - the item is not known to it;
    - the evidence conflicts (D12; two *arr integrations claim the same file);
    - the value is only a lower bound (Tautulli with history turned off for a user, §8.2).
  - Unknown is never true, and negating unknown gives unknown.
  - A rule matches only when it is true. A rule that is unknown and more protective
    (`full` > `manifest` > `skip`) than the rule that matches later wins over that later rule.
  - The built-in fallback is `full`. So errors, stale caches, deleted integrations and unmapped
    files can only make a file *more* protected.
  - A file flagged irreplaceable is `full` at every destination whatever the rules say (§8.1).
  - Unknown decides what is protected, not how much is copied at once. A copy that exists only
    because a fact is unknown (`unknownPromoted`) is a change for S10(b), and it never makes the
    free-space check fail the job (§8.5). What is already backed up is never touched by it (S15).
- **S15 Tier changes never remove backups.** When a file's tier at a destination stops being
  `full`, what that destination holds for it is **kept**:
  - The record and the file stay as they are. They are not updated when the source file changes,
    not repaired when verify marks them missing, and not retained while the source file exists.
  - A move of backed-up content follows the content, whatever its new tier: the move pairing
    considers every live catalog file without a live record, of any tier (§8.5). The moved record
    is then kept when its new tier is not `full`.
  - A kept file leaves the destination in only two ways:
    - (a) its source name disappears (deleted, or replaced under another name), so S5 retains it
      for `deletedDays`;
    - (b) a release: a sync with `releaseDemoted` that applies a dry run (`releaseOf`). It
      releases exactly the records that dry run listed and that are still not `full` when each
      item runs, into retention with reason `released`, expiring after `deletedDays`. Release
      items are changes for S10(b).
  - Nothing else causes a retain: not a rule edit, a preset, an unknown fact, a failed or deleted
    integration, or a removed flag.
  - A kept file whose tier is `full` again is simply full again. The first sync updates it if the
    source changed meanwhile, with the old version retained, as for any update.
- **S16 Other applications are read-only, allow-listed and reached through the outbound guard.**
  - Each client (§4) has a fixed list of requests and no general "request" method, and a test
    fails on any request outside it.
  - Bunkarr makes only two writes to other applications: the *arr `Backup` command, which
    creates a backup file in the *arr's config folder and is sent only when no fresh scheduled
    backup exists (§10); and creating and checking a plex.tv PIN during sign-in.
  - Maintainerr needs no authentication and has state-changing GET routes. Its client can reach
    only the GET paths listed in §4.5, never `/api/collections/activate|deactivate/*`,
    `/api/settings*` or the database download.
  - Response bodies are size-capped, and nothing is ever echoed into an error.
  - **Outbound guard.** Every outbound client (the Plex client, the plex.tv helper, the connection
    probe, the *arr, Tautulli, Seerr and Maintainerr clients, and Apprise) dials through
    `internal/netguard`, ported from Dupearr. It checks the resolved address at dial time and
    refuses 169.254.0.0/16, fe80::/10, fd00:ec2::254 and 100.100.100.200; a request that would go
    through an HTTP(S) proxy is checked before it is handed to the proxy.
  - The plex.tv base URLs can be overridden only in binaries built with `-tags e2e`, and the
    plex.tv helper refuses `http` unless the host is loopback.
- **S17 \*arr backups are sensitive.**
  - **What a zip holds.** Everything the *arr stores: its `config.xml` with its API key, and its
    database with indexer and download-client credentials, its Plex token, its password hash and
    its connections, which include Bunkarr's webhook key (a key that can do nothing but S12).
  - **How it is stored.** At the destination exactly as received: files 0600, directories 0700.
    SMB without POSIX extensions does not enforce these modes. The destination probe records
    `enforcesModes`, and a backup to a destination where it is not true needs
    `backup.acceptInsecureModes` (§10). The README says so, like for Plex's `Preferences.xml`.
  - **How it is read.** Only to verify it, within the limits of §10 step 6: the zip structure,
    every entry's CRC, and `quick_check` of the database extracted in staging.
  - **What never happens.** It is never served by the API (there is no download endpoint), never
    logged, and never described beyond names, sizes and hashes, in `manifest.json` and in
    `snapshots.manifest` alike.
  - **Staging.** A 0700 directory, removed when the job ends, and after 24 h for a job that failed
    for good.
  - **Not sealed.** A disaster-recovery copy that needs Bunkarr's own key would be lost together
    with Bunkarr's config. A passphrase option is deferred.
- **S18 Paths from other applications are mapped and fenced.**
  - *arr and Plex report container paths. Each path goes through the integration's path mappings
    (longest prefix on a path-segment boundary, as `PlexSettings.MapPath` does), then is located
    in **every** source that contains it (longest prefix first, on a segment boundary).
  - Only clean, absolute paths are accepted.
  - A path that maps nowhere, or into no source, is **unmapped**: counted and shown in the UI,
    never an error. A located path is used only as a catalog key and as a scan scope through the
    source's `os.Root` (S1).
  - A location is not proof. An `arr_files` row applies to a catalog file only when the file's
    local path equals the row's mapped local path and their sizes are equal; otherwise the row is
    `mismatched` (counted, listed with the unmapped ones) and the file's *arr facts are unknown.
  - Before a refresh queues a targeted sync, it checks that each located folder of an item that
    has files exists (§6.1). A missing one is a job warning that names the mapping, and that
    source gets an untargeted sync instead.
  - `ScanPaths` resolves each target one component at a time without following symlinks, and
    refuses targets under excluded or forbidden directories (§9.1).
  - Backup-folder reads go through an `os.Root` on the configured `backupFolder`, with
    `O_RDONLY|O_NOFOLLOW` and regular files only. A backup's location, in the folder and over
    HTTP, is built only from its `type` (`manual`, `scheduled` or `update`) and its `name`
    (`^[A-Za-z0-9._-]{1,200}\.zip$`). The `path` field of `system/backup` is never decoded.
- **S19 Plex sign-in tokens never reach the browser.**
  - The account token and every server's access token stay in a short-lived, in-memory sign-in
    session on the server (§5). The browser holds only an opaque sign-in id and a server id.
  - Before the token is sent to a URL (Test, Save), Bunkarr calls `/identity` there without the
    token and requires its `machineIdentifier` to equal the chosen server's.
  - After that, the stored token is bound to the URL as S8 requires.
  - No response contains a token. The PIN code appears only inside the `authUrl` of
    `POST /plex/signin`, because the browser must hand it to app.plex.tv. The plex.tv PIN id is
    never returned. Logs carry only the sign-in id prefix, the username and counts.
  - Each install identifies itself to plex.tv with its own client identifier, so one install can
    be revoked alone.
- **S20 Manifests are complete, atomic and checked.**
  - **Complete.** Every non-deleted item of every enabled *arr integration appears in every
    manifest, whether it locates into a source or not. Every file that has a live record at the
    destination appears, whatever its tier. What is left out (non-*arr files of tier `skip` with
    no record, D9) is counted. An integration that is not fresh, or items that do not locate, make
    the export end `completed_with_warnings`.
  - **Atomic.** A version is written into `.partial-job<id>/` with `WriteFileAtomic`, together
    with `SHA256SUMS`. The directory is then renamed and recorded with its checksum. A manifest is
    built in one read transaction, so it reflects one state of the database. An on-the-spot export
    is staged and hashed before the first byte is sent, and a failed build answers 500 without a
    body. *(As built: the 500 carries the usual JSON error message; no manifest byte is ever
    sent.)*
  - **Checked.** The download endpoint verifies the checksum before serving the file. A version
    counts as "unchanged" only after the newest `ok` version has been read back from the
    destination and matched its checksum; a damaged one is marked `damaged` and replaced by a new
    version.
  - Pruning deletes only recorded version directories, as for Plex DB versions.
  - A manifest never holds secrets: no API keys, no integration URLs (integrations appear by id,
    type, name and version), and Seerr requesters as user ids only.

## 2. Decisions

| # | Decision | Why |
|---|---|---|
| D1 | The default tier is `full`: with no rules every file is full, the fallback after the last rule is full, and irreplaceable is always full. The spec's manifest-by-default set is a preset (§8.4), loaded into the editor and saved only after a preview. | User decision; a Phase 1 install must not change on upgrade (acceptance 8). |
| D2 | **Lidarr is in** and comes as the last Phase 2 slice (§18). It gets the index, webhooks, backups and manifests. | The spike found no blocker. Bunkarr's safety rests on scanning, not on events, so Lidarr's quirks do not matter: `isUpgrade=false` with no `deletedFiles` on replacement, `ManualImport` without a webhook, no track-file-delete event, and an `AlbumDelete` for every album after an `ArtistDelete`. The full refresh reconciles what no event reports (D8). If the slice slips it moves to Phase 6 alone; nothing else depends on it. |
| D3 | An *arr backup is fetched from the **backup folder** (`backupFolder`: the app's `Backups` directory mounted read-only) when one is set. Otherwise it is fetched **over HTTP** with the API key, which works only when the *arr does not require a login for Bunkarr's address. The *arr's UI password is **not** stored. A fresh scheduled backup is copied instead of creating a manual one (§10). | With Forms login on, the API key cannot download backup zips (spike: 302 to `/login`). A folder mount adds no secret and mirrors Plex's `dataPath`. Requiring "Disabled for Local Addresses" would weaken the *arr's security, and storing its admin password would add a secret. Both are left to the user (DEFERRED). The *arrs never prune manual backups made through the API, so creating fewer of them matters. |
| D4 | *arr backups are sensitive but not sealed (S17). | Consistent with Plex's `Preferences.xml` (phase1.md §5). A backup that only Bunkarr's key can open fails exactly when Bunkarr's config is lost. |
| D5 | Tiers are per destination and **evaluated, never stored**. The facts come from the metadata caches at evaluation time. `catalog_files.tier`, `media_ids` and `arr_item_id` are dropped. | The spec's actions carry target destinations, so one tier per file is the wrong shape. Facts change constantly, and a stored tier would go stale. |
| D6 | A demotion keeps the file; a release is explicit and applies the dry run the user confirmed (S15). | "Deletes are not propagated immediately." A mis-edited rule, or facts that change between the preview and the confirmation, must never empty a destination. |
| D7 | Webhooks authenticate with a **per-integration webhook key**, never with Bunkarr's master API key. It is sent as the Basic auth password (recommended: the *arr keeps that field private), the `X-Api-Key` header, or `?apikey=` (the spec's form). There is one URL per integration, `/api/v1/webhook/{app}/{id}`; the spec's `/api/v1/webhook/{app}` also works and picks the integration whose key matches. | The *arrs return the webhook URL and headers unmasked through their API and keep them in their backup zips, which Bunkarr itself writes to the destination. A master key there would be full Bunkarr admin for anyone with any *arr API key or read access to the share. Dupearr made the same change (`internal/auth/webhook.go`). The spec asks for "the API key"; the integration's webhook key is that key for the webhook (open question 5). |
| D8 | A webhook leads to a targeted `refresh` of the *arr items, which then queues targeted `sync` jobs for their folders (`syncAfter`). The **full refresh is the reconciler**: it compares each item's files with the index and queues targeted syncs for what changed (§6.1). It runs on its schedule and at start-up. Scheduled full syncs remain the last net. | The index must know the new file before its tier is evaluated. The *arrs never retry, events are lost during restarts, and some changes (Lidarr `ManualImport`, a disk rescan) send none. |
| D9 | `manifest`: the file is not copied, but it is listed in that destination's manifests so it can be acquired again. `skip`: not copied. An *arr file is still listed in its item (with tier `skip`), because the item state must round-trip, and a file with a live record is listed as kept. Other `skip` files (extras, files outside any item) without a record are only counted. Items are always listed, in every manifest. | "skip" still means less than "manifest" (no copy, and no listing of the extras), while the *arr state round-trips at every destination and the manifest never reports less than the destination holds. |
| D10 | Plex sign-in is ported from Dupearr, but tokens stay on the server (S19). Each install gets its own client identifier. COOP changes from `same-origin` to `same-origin-allow-popups`. An `http` connection is never recommended. | Dupearr sends the account token and every server token to the browser, which conflicts with S8. The COOP change lets Bunkarr close the plex.tv popup. The fallback, if the user objects: keep `same-origin` and show "you can close the Plex window". |
| D11 | The `snapshots` table moves from `internal/plexdb` to a new `internal/snapshots` (Store plus keep-daily/weekly selection); `kind` gains `arr`. | Plex DB versions and *arr backups share the table, the API, and the retention math. |
| D12 | A file holding several items (a multi-episode file, a hardlink group) evaluates each condition per item. The result is true only if all are true, false only if all are false, and unknown otherwise. A hardlink group takes the most protective tier among its names. Two *arr integrations claiming the same file or folder is conflicting evidence: every `arr.*` condition is then unknown, unless the source names its *arr integration (`sources.arr_integration_id`). | Mixed evidence is unknown (S14), so it can only protect. |
| D13 | Not added; each needs the user's approval: an optional reverse-proxy auth header for Maintainerr; creating the webhook connection inside the *arr through its API (that would write *arr config); storing *arr UI credentials (D3); deleting the manual backups Bunkarr created (`DELETE system/backup/{id}`, a new write). | Network-facing or config-writing features outside the spec. |
| D14 | A targeted sync retains a vanished name only when the same directory gets new content in the same plan (an upgrade or a rename). Other vanished names wait for the next untargeted sync (S10). | Webhook syncs are small, so the per-sync mass-change guard would never see a deletion spread over many events. Deferring the retain loses nothing: the file stays live and recorded at the destination. |
| D15 | Freshness has one definition: the age of the last complete refresh against a per-integration `staleAfterHours`, for the integration's current URL (§6). The outcome of later attempts never changes it. | Two readings of "fresh" gave opposite tiers for the same state (review). A failed targeted refresh must not make a whole library's facts unknown. |
| D16 | Phase 2 (slices 0–7) ships and is committed before Phase 3 starts. In Phase 3 the tier engine comes first, with the *arr, file, source and flag fields; the Plex index and the Tautulli, Seerr and Maintainerr fields follow. Plex genres (`media.genre` comes from the *arr only) and the tier column of the file browser are deferred. | Something shippable exists at every step, and the parts without an acceptance criterion do not delay the ones with one. |

## 3. Destination layout (additions to phase1.md §2)

```
<target>/.bunkarr/
  arr/<integration-slug>-<integrationId>/<yyyymmddThhmmssZ>[-job<id>]/   *arr backup version (dir 0700)
      <app>_backup_v<version>_<date>.zip        as the *arr made it (0600; sensitive, S17)
      manifest.json  {format, createdAt, app, appVersion, integrationId, integrationName, jobId,
                      jobQueuedAt, method, backup:{name, type, time}, zip:{name, size, sha256},
                      entries:[{name, size, crc32}], integrity:{zip, database}, sensitive:true,
                      warnings}
  arr/<integration-slug>-<integrationId>/.partial-job<id>/  and  .prune-<version>/
  manifests/<yyyymmddThhmmssZ>[-job<id>]/                   manifest version (§11)
      manifest.json   manifest.csv   SHA256SUMS
  manifests/.partial-job<id>/  and  .prune-<version>/
```

Folder names, the `-job<id>` suffix, recovery (a `.partial-job*` directory is removed at the start
of every run, a complete but unrecorded version is re-hashed against its manifest and recorded)
and pruning (rename to `.prune-<v>`, then remove; only recorded directories that match the
pattern) follow the Plex DB rules of phase1.md §5 exactly. The code is shared through
`internal/snapshots` and `filecopy.RenameDir`.

## 4. Integrations

### 4.1 Common
- `integrations.Input`, `Store`, sealing and `TokenFor` are unchanged. All seven types are now
  "available": `POST /integrations/test` really tests them (§13). Each type has a settings struct
  that is validated and normalized the way `PlexSettings` is (unknown fields dropped). A type's
  schedules (refresh, backup) are schedules rows mirrored from its settings, exactly like the
  Plex DB backup (phase1.md §5).
- **Test with unsaved settings.** The Test body is `{type, url, apiKey?, id?, settings?,
  plexSignIn?}`. `settings` is validated like a create and used only for that Test
  (`plexIntegrationId`, `backupFolder`, `pathMappings`). Without it, the stored settings are used
  when `id` is given; otherwise the fields that need settings come back `null` or `not-set`.
- **Initial and changed state.** Creating an integration, or an update that changes its URL, key
  or `pathMappings`, queues a full refresh (trigger `manual`) for the *arr, Tautulli, Seerr and
  Maintainerr types. A cache whose `index_state.instance_id` does not match the integration's
  current URL is not fresh (§6), and the next refresh replaces it.
- **Rows created before 0003.** Phase 1 accepted non-Plex rows with any settings object. At
  start-up, such rows are reparsed (unknown fields dropped, defaults applied). A row that fails
  validation (a Tautulli without `plexIntegrationId`, a Maintainerr with a key) is set
  `enabled = 0` and the reason is logged. Valid rows get their default refresh schedule, and *arr
  rows get a webhook key.
- **Path mappings.** The generic `integrations.MapPath(mappings, p)` replaces the body of
  `PlexSettings.MapPath`; the behaviour is unchanged. *arr settings use `{arr, local}` pairs; the
  limits and validation are those of Plex's `{plex, local}` pairs (64 pairs, absolute clean paths,
  unique prefix).
- **Locating a path in a source.** `catalog.Store.Locate(ctx, localPath)` returns every source
  whose stored (resolved) path is a segment-boundary prefix of `localPath`, as
  `[]Location{SourceID, Rel}`, longest prefix first. `Rel == ""` means the source root. Sources may
  overlap (Phase 1 refuses only overlaps with destinations). Sources are few, so the refresh loads
  them once. This is the only way external paths reach the catalog (S18).
- **`sources.arr_integration_id`** (a Phase 1 column) is defined now. "Import from
  Radarr/Sonarr" sets it. It must name an integration of type sonarr, radarr or lidarr (400
  otherwise). When it is set, §8.3 looks up a file's *arr item only in that integration.
- **Deleting an integration** still returns 409 while it has queued or running jobs. It removes
  the integration's schedules. Its caches and index state go too (the foreign keys CASCADE). Its
  *arr-item flags stay: `integration_id` becomes NULL and they keep matching by external id
  (§8.7). Webhook events keep their rows with the reference cleared.
- **Outbound guard.** Every client's transport uses `netguard` (S16).

### 4.2 Settings per type (stored normalized; missing fields take the defaults)

| Type | `settings` | Defaults |
|---|---|---|
| plex | Phase 1 fields + `index: {enabled, cron, staleAfterHours}` (library index, §6.3) | index disabled; cron `0 1 * * *` when enabled without one; stale after 72 h |
| sonarr, radarr, lidarr | `pathMappings: [{arr, local}]`, `backupFolder` (absolute path of the app's `Backups` directory as Bunkarr sees it, `""` = HTTP), `backup: {destinationId, cron, enabled, maxScheduledAgeDays, acceptInsecureModes}`, `refresh: {cron, enabled, staleAfterHours}` | refresh enabled, `15 */6 * * *`, created with the integration, stale after 24 h; backup disabled, cron `30 6 * * 0` (weekly) when enabled without one, `maxScheduledAgeDays` 7 (1–90), `acceptInsecureModes` false |
| tautulli | `plexIntegrationId` (required: the Plex server it watches), `refresh: {cron, enabled, staleAfterHours}` | refresh enabled, `0 2 * * *`, stale after 72 h |
| seerr | `plexIntegrationId` (optional: enables the rating-key fallback, §8.2), `refresh: {cron, enabled, staleAfterHours}` | refresh enabled, `30 2 * * *`, stale after 72 h |
| maintainerr | `plexIntegrationId` (required), `refresh: {cron, enabled, staleAfterHours}` | refresh enabled, `45 */6 * * *`, stale after 24 h |

- `staleAfterHours` is 1–720.
- An enabled backup needs a destination and a cron, as for Plex. `plexIntegrationId` must name a
  Plex integration.
- The webhook key is not a setting; it lives in its own sealed column (S8).

### 4.3 \*arr client (`internal/integrations/arr`)

`arr.New(kind, baseURL, apiKey, Options)`, where kind is `sonarr|radarr|lidarr`. The API prefix
is `/api/v3`, or `/api/v1` for Lidarr, after any URL base in the stored URL.
- **Headers.** `X-Api-Key`, `Accept: application/json` and `User-Agent: Bunkarr/<version>`.
- **Safety.** Redirects are not followed. Errors are `*arr.Error{Method, Path, StatusCode, Err}`
  with the API key redacted. Timeouts are 30 s per request, or 5 min for the full lists. The
  transport uses `netguard`.
- **Body caps.** 256 MiB for the full lists (streamed JSON decode), 32 MiB for everything else.
  A backup download is streamed to staging with a cap of 8 GiB.
- `Status` requires `appName` to equal the kind (`Radarr`/`Sonarr`/`Lidarr`); otherwise it
  returns `ErrWrongApp`, for a Sonarr URL saved as Radarr.
- Versions: Sonarr ≥ 3 and Radarr ≥ 3 (API v3), Lidarr ≥ 1 (API v1). The fixtures come from
  4.0.20, 6.4.4 and 3.1.0; older versions are best effort.

The allowed requests (S16). Every other request is refused by construction:

| Kind | Requests |
|---|---|
| all | `GET system/status`, `GET qualityprofile`, `GET rootfolder`, `GET tag`, `GET config/mediamanagement`, `GET system/backup`, `POST command {"name":"Backup"}`, `GET command/{id}`; the non-API `GET <base>/backup/{manual\|scheduled\|update}/<name>.zip` |
| radarr | `GET movie` (the list holds nested `movieFile`), `GET movie/{id}`, `GET moviefile?movieId=` |
| sonarr | `GET series`, `GET series/{id}`, `GET episodefile?seriesId=`, `GET episode?seriesId=` |
| lidarr | `GET artist`, `GET artist/{id}`, `GET trackfile?artistId=`, `GET album?artistId=`, `GET metadataprofile` |

Decoding follows the spike's API facts:
- Radarr's `folderName` holds a full path. A movie without a file has `movieFileId: 0` and no
  `movieFile`.
- Sonarr's `episodefile` lacks episode numbers, so files are mapped to episodes through
  `episode?seriesId=` (`episodeFileId`, `seasonNumber`, `episodeNumber`, `tvdbId`, `monitored`).
- Lidarr's artist `mbId` is null in the API; use `foreignArtistId`.
- Items store tag ids and webhooks send tag labels. The labels come from `arr_meta`.
- A quality profile `cutoff` may be a group id ≥ 1000.
- `rootfolder` gives `accessible`; `config/mediamanagement` gives `recycleBin` and `fileDate`.
- `system/backup` is decoded as `{id, name, type, size, time}` only; `path` is never decoded
  (S18). A backup file's URL is `url.JoinPath(base, "backup", type, name)` after `type` and `name`
  are validated.

`arrtest` is a fake server with an explicit route table: method, path and sorted query → fixture
file under `testdata/arr/<app>/`, and 404 for any route not in the table. It checks the header
(and fails the test when a key appears in a query), records the requests, and can script 404,
302-to-login, 500, a slow response, an oversized one, `accessible: false` and a `path` that points
elsewhere. Fixtures to record again from the spike's containers before slice 2:
`sonarr/episode-seriesId-1.json` (all seasons), `sonarr/episode-seriesId-2.json`,
`sonarr/series-2.json` and `lidarr/config-mediamanagement.json`; and a second Radarr root folder
that no mapping covers (`/movies-4k`, with one movie), for the unlocated-item tests.

### 4.4 Tautulli client (`internal/integrations/tautulli`, ported from Dupearr)

- **Auth.** The key goes in the `X-Api-Key` header only. `Test` requires `get_tautulli_info`
  ≥ 2.18.0 and a `get_server_info.pms_identifier` equal to the linked Plex server's
  `machineIdentifier`, read through that integration's `/identity`.
- **Allowed commands.** `get_tautulli_info`, `get_server_info`, `get_library` (for
  `keep_history`), `get_users` (decoding only `user_id` and `keep_history`), and `get_history`
  with the parameters below.
- **Refresh.** One `get_history` pass per section with
  `section_id=N&media_type=movie,episode,track&grouping=0&include_activity=0&order_column=date&order_dir=asc&start=…&length=1000`.
  Plays are the number of rows; last watched is the latest `stopped`. Both are counted by
  `rating_key` and, separately, by `guid`.
- **Paging integrity** (ported from Dupearr's History paging). Rows are deduplicated by `row_id`.
  The total comes from page 1 (`recordsFiltered`). The refresh fails when a later page reports a
  smaller total, or when a page comes back short before the total is reached. At most 250,000 rows
  are read per refresh.
- **Never sent.** `refresh=true`, or any `get_library_media_info` call.
- **Decoding.** The `{"response":{result,message,data}}` envelope, HTML-escaped strings, and
  numbers that may arrive as strings.
- **Status codes.** 401 means a bad key, 404 means the API is disabled, 400 means a bad command.

### 4.5 Seerr client (`internal/integrations/seerr`) and Maintainerr client (`internal/integrations/maintainerr`)

- **Seerr** (seerr-team/seerr ≥ 3; Overseerr and Jellyseerr best effort).
  - The key goes in `X-Api-Key` only. A bad key answers 403. `X-API-User` is never sent.
  - Allowed requests:
    - `GET /api/v1/status` (no key);
    - `GET /api/v1/auth/me`;
    - `GET /api/v1/request?take=100&skip=…&sort=added&sortDirection=asc`;
    - `GET /api/v1/user?take=100&skip=…` (rule-editor labels, live only).
  - Decoded request fields: `id, status, type, is4k, seasons[].seasonNumber, requestedBy.id,
    createdAt, media{tmdbId, tvdbId, ratingKey}`. Nothing else: no e-mail, no names, and not
    `serviceId`/`externalServiceId`.
  - Decoded user fields: `id, username, plexUsername, jellyfinUsername`. `displayName` and `email`
    are never decoded, because Seerr fills `displayName` with the e-mail when a user has no user
    name. The label is the first non-empty name, else "Seerr user #<id>".
  - Paging integrity as for Tautulli: deduplicate by request id, take the total from page 1
    (`pageInfo.results`), fail on a shrinking total or a short page, at most 250,000 requests.
- **Maintainerr** (≥ 3.4.0).
  - No key (S8). `/api/app/status` answers a JSON string as `text/html`; the body is parsed
    whatever its content type says.
  - Allowed requests:
    - `GET /api/app/status`;
    - `GET /api/collections/overlay-data`;
    - `GET /api/collections`;
    - `GET /api/collections/media/?collectionId=N` (the fallback);
    - `GET /api/rules`;
    - `GET /api/rules/exclusion?rulegroupId=N`.

  Nothing else, ever (S16).

### 4.6 Plex client additions (`internal/integrations/plex`)

- **Library listing.** `AllItems(ctx, sectionKey, type)` is ported from Dupearr's
  `library.go`. It calls `/library/sections/{key}/all?type=1|2|3|4|8|9|10&includeGuids=1`, paged
  by `X-Plex-Container-Start/Size` (500) and advancing by the rows actually returned. Each row
  gives the `ratingKey`, `type`, `index`, `parentIndex`, `parentRatingKey`,
  `grandparentRatingKey`, `guid`, `Guid[]`, `title`, `addedAt` and `Media[].Part[].file`.
- **Genres** are deferred (D16). The `Genre[]` of an item listing is truncated and is never used.
- **Sign-in.** plex.tv PIN, user, resources and connection probe (§5).
- **Unchanged.** The token is sent only as the header, and only after `/identity` (S8). The
  transport now uses `netguard` (S16), a hardening of the Phase 1 client.

## 5. Plex sign-in (D10, S19)

**plex.tv client** (`plextv.go`, ported from Dupearr `internal/integrations/plex/plextv.go`):
- `CreatePin` sends `POST https://plex.tv/api/v2/pins?strong=true` without a token.
  `CheckPin(id, code)` sends `GET /api/v2/pins/{id}?code=<code>` with the client identifier, as
  Plex documents the check. `authToken` stays empty until the user approves, and a 404 means the
  PIN expired.
- `User(token)` sends `GET https://plex.tv/api/v2/user` and decodes only `username` (never
  `email`, `authToken` or the subscription). "Signed in as X" is how the user notices a PIN
  approved by another Plex account.
- `AuthURL(code)` builds `https://app.plex.tv/auth#?clientID=…&code=…&context%5Bdevice%5D%5Bproduct%5D=Bunkarr`.
- `Resources(token)` sends `GET https://clients.plex.tv/api/v2/resources?includeHttps=1&includeRelay=1&includeIPv6=1`
  with the token in the header, and keeps resources whose `provides` includes `server`.
- The decoders stay lenient, accepting the JSON, XML-attribute and XML-element shapes. The types
  `Pin`, `Resource` and `Connection` are ported, with `Resource.AccessToken` tagged `json:"-"`.
  `OwnerOf` and `Client.Ownership` are not ported.
- The `tvDo` helper validates the base URL (https, or http only for a loopback host) and sets the
  full X-Plex header set (Product, Version, Client-Identifier, Platform, Device, Device-Name; the
  token only as a header). It never follows a redirect and reads at most 8 MiB. It maps 401 to
  `ErrUnauthorized` and 404 to `ErrNotFound`, and returns `*Error` with no body.
- `Options` gains `Product`, `Device` (`Docker` in a container, else the OS), `DeviceName`,
  `PlexTVURL` and `ClientsPlexTVURL`. The two URLs can be overridden only in binaries built with
  `-tags e2e` (`internal/testhooks`, §14.2); production builds use the constants.
- `flexString` and `flexInt` move to `flex.go`, together with the ported `flexBool`, `list[T]`,
  `parseIntText`, `parseBoolText`, `firstNonEmpty`, `trimBody` and `unixTime`.

**Client identifier.**
- `api.NewApp` creates a UUIDv4 once and stores it as the setting `plex.clientIdentifier`. It is
  used by every Plex request, including the plexdb runner: pass `a.plexOpts`, not `o.Plex`.
- `bunkarr` remains only the tests' default.
- Tokens pasted by hand are not bound to a client id, so Phase 1 integrations keep working.

**Connection probe** (`probe.go`, new).
- **Candidates.** `Candidates(Resource)`:
  - the plex.tv connections, plus a derived `http://<address>:<port>` for each local
    `*.plex.direct` connection (home routers' DNS-rebind protection often blocks plex.direct
    names);
  - for a resource the user does not own (`owned: false`), `local` connections are dropped and
    nothing is derived: those addresses are in the owner's network, chosen by the owner;
  - at most 16 candidates per resource.
- **Probing.** `ProbeConnections(ctx, opts, machineID, token, cands)` runs 4 at a time, 5 s per
  candidate and 15 s in total.
  - Every dial goes through `netguard` (S16), after name resolution, so a plex.direct name that
    resolves to a metadata address is refused without a request.
  - Each candidate gets `/identity` without the token first. The token goes, to `/library/sections`,
    only where the identity matched.
  - *(As built, review fix.)* The token also goes only over https, or over http to a loopback IP
    literal (the request never leaves the host). An http candidate that answers as the chosen
    server is reported reachable with the token withheld ("The token was not sent: this
    connection is not encrypted"), so neither the server token nor the account token travels in
    clear text before the user picks "Use anyway". Saving after "Use anyway" sends the token to
    that URL, as the user chose.
- **Messages.** Plain causes for a plex.direct DNS failure, a TLS certificate error, a timeout, a
  refused connection, a refused address (netguard) and a 401.
- **Ranking.** Working first, then local non-relay, remote and relay last; within a tier, https
  before http, then by latency. Relays and `http` candidates are never "recommended"; an `http`
  row is badged "Unencrypted" and needs "Use anyway".
- `ProbeResult = {uri, local, relay, derived, protocol, ok, identityMatches, tokenAccepted,
  version, latencyMs, message}`.

**Sign-in registry** (`internal/api/plexsignin.go`, replacing Dupearr's three handlers). Each
sign-in holds:
- an id of 32 hex characters from crypto/rand;
- its creator's principal (a hash of the session token, or `apikey`, or `local`); another
  principal gets 404;
- the PIN id and code;
- an expiry: the PIN's, capped at 10 min while pending, then 20 min after the claim;
- once claimed, the account token, the username and the resources with their tokens.

Its rules:
- `CheckPin` runs at most every 2 s, and resources are fetched again at most every 10 s.
- At most 8 sign-ins are live (429 beyond). Expired ones are swept lazily.
- While live, the tokens are registered with `logging.SetSecrets("plexsignin:<id>", …)`. They are
  released when the sign-in is consumed, cancelled or expires, and when `App.Stop` clears the
  registry.
- Sign-ins do not survive a restart: the UI says to sign in again.
- *(As built.)* An expired sign-in answers `expired` for 10 minutes, then 404. Sign-in ids are
  also registered for redaction, so the request log shows `[REDACTED]` in their place.

**Using a sign-in.** `POST/PUT /integrations` and `POST /integrations/test` accept
`plexSignIn: {id, serverId, useAccountToken?}` in place of `apiKey`. The body is
`struct{integrations.Input; PlexSignIn *plexSignInRef}`, so `DisallowUnknownFields` still works.
`resolveSignInToken` then:
1. requires type plex, and refuses `apiKey` or `clearApiKey` in the same request (400);
2. requires the sign-in to exist and be claimed (400 "sign in again");
3. finds the server by `serverId` (`^[0-9A-Za-z-]{1,128}$`);
4. picks the token:
   - the server's `accessToken`;
   - if there is none, an owned server needs `useAccountToken: true` (else 400), and a shared
     server gets 400 "enter a token manually" (Dupearr SEC-033);
5. normalizes the URL and runs the identity check without the token (400 on a mismatch; the
   token was not sent);
6. sets `in.APIKey` and runs the existing `Store.Create` or `Update`, which writes the URL and the
   sealed token in one transaction, then consumes the sign-in. Test does not consume it.

**Browser.**
- `window.open('', 'bunkarr-plex-auth', …)` runs inside the click handler, followed by
  `popup.opener = null` and a placeholder page. Once the POST returns, `isTrustedPlexAuthUrl`
  accepts exactly `https:`, host `app.plex.tv` and path `/auth`; only then does the popup
  navigate. A blocked popup falls back to a link with `rel="noopener noreferrer"`.
- The UI state machine (`plexSignInMachine.ts`, rewritten from Dupearr's) holds no token in any
  state: `idle → creating → waiting{signInId, expiresAt, popupBlocked} →
  authenticated{signInId, username} → serverChosen{serverId, results} → saved | expired | error`.
- CSP is unchanged: the SPA never fetches plex.tv. COOP becomes `same-origin-allow-popups`.
- The UI never relies on `popup.closed` after the popup has navigated.
- *(As built, review fixes.)* The countdown runs from the time the server says is left (its
  answer's `Date` header against `expiresAt`), capped at 10 min, so a browser clock that is off
  does not end a sign-in early; it is kept out of the live region. The server's `warning` is shown
  while waiting and after the approval. Unticking the account-token box or choosing another
  server drops the connection already taken, so Save never sends a choice the user took back. A
  failed connection test shows its error, not "no connection".
- CSRF is already covered by `http.NewCrossOriginProtection` on `/api/v1`.
- The only outbound calls are HTTPS to plex.tv and clients.plex.tv, and only when the user clicks
  "Sign in with Plex".

## 6. Metadata index (`internal/mediaindex`)

The index caches what other applications know about the library, for tier facts (§8.3), scan
targets, the Library item view and manifests. Each integration's cache is refreshed by `refresh`
jobs (§12) and has one `index_state` row.

**Freshness (D15).** A cache is fresh when all of these hold:
- `refreshed_at` (the last complete refresh) is set, and `now − refreshed_at <
  staleAfterHours` (§4.2);
- `index_state.instance_id` names the integration's current URL (for the Plex index:
  `<url>#<machineIdentifier>`, and the URL part must match).

The status and error of any attempt, full or targeted, never affect freshness. A cache that is not
fresh makes every fact it provides unknown (S14).

**All-or-nothing.** A refresh fetches everything it needs before it deletes anything:
- An *arr full refresh upserts in batches while it fetches (upserted data is correct data), and
  marks items deleted and deletes files only after the complete fetch, under the S10 guard.
- Tautulli, Seerr and Maintainerr refreshes fetch everything first, then replace their rows in one
  transaction, and only if every request succeeded.
- On any failure the status becomes `failed`, and the previous rows and `refreshed_at` stay, so
  the cache ages into unknown.

**Instance change.** A refresh that finds `instance_id` different from the integration's current
URL (or, for Plex, a different `machineIdentifier` at the same URL) deletes all of that
integration's cache rows in the transaction of its first write, and records the new instance id.
A URL change also queues a full refresh (§4.1).

### 6.1 \*arr refresh
- **Full refresh.**
  1. `Status`, which records the app version (`ErrWrongApp` fails the job).
  2. The metadata: quality profiles, root folders (with `accessible`) and tags, plus Lidarr's
     metadata profiles. These replace the integration's `arr_meta` rows. `config/mediamanagement`
     is read too:
     - `recycleBin` is mapped and located. Inside a source whose excludes do not cover it, it is
       a job warning and is shown on the Connect card with an offer to add the exclusion (§16).
     - `fileDate` other than `none` is a job warning: every version of a title then gets the same
       mtime, so an equal-size replacement looks unchanged (§8.5).
  3. The items:
     - Radarr: `GET movie`.
     - Sonarr: `GET series`, then `episodefile` and `episode` per series.
     - Lidarr: `GET artist`, then `trackfile` and `album` per artist.
     - The per-item calls run 4 at a time.
  4. Before each item is upserted, its set of `(arr_file_id, path)` and its folder are compared
     with the index. Items whose set or folder changed, and new items that have files, form the
     **changed set** (with their old and new folders).
  5. Each item is upserted into `arr_items`, and each file into `arr_files`, keyed on the *arr
     file id (a same-path replacement gets a new id). A file's path goes through
     `pathMappings`, then `catalog.Store.Locate`; the row keeps the longest-prefix location
     (`source_id`, `rel_path`) and the mapped `local_path`. An unmapped file keeps them NULL and
     is counted per root folder.
  6. Items and files are written in batches of 1000. Only after the complete fetch are the items
     not seen marked `deleted_at` and the files not seen deleted, subject to the S10 refresh
     guard.
  7. Items deleted for more than 30 days are purged.
  8. **Reconcile (D8).** Unless it is a dry run or the first complete refresh of this instance
     (nothing to reconcile against), the refresh queues targeted syncs for the changed set exactly
     as `SyncAfter` does below, with the refresh's own trigger (`schedule`, `startup` or
     `manual`; a resumed reconciling refresh uses `startup`).
     - *(As built.)* The changed set is recorded as the job's items (action `skip`, pending until
       their syncs are queued, then done) before the index changes, so a crash or a failed
       refresh still follows them up. A refresh also follows up the pending intents of earlier
       refresh jobs of the integration that ended without their runner (failed by crash
       recovery, or cancelled before they resumed): the index holds their changes already, so no
       later refresh would find them again.
- **Targeted refresh** (`Params.ArrItemIDs`).
  - `GET movie/{id}` (or `series/{id}` or `artist/{id}`) plus that item's files.
  - A 404 marks the item deleted only if `GET system/status` in the same job answers with the
    expected `appName`. Otherwise (a proxy misroute, a changed URL base) the job fails and the
    item is left alone.
  - An unknown tag or profile id triggers a metadata refresh first.
  - It updates only those items, and does not change `refreshed_at`.
- **Follow-up syncs** (`SyncAfter`, and the reconcile of a full refresh).
  - The **targets** are each item's folder: the one it had in the index before the refresh (a
    delete, or a move to another root folder) and the one it has now. Each is mapped and located
    in every containing source (S18).
  - For an item that has files (`hasFile`, `movieFileId ≠ 0`, `episodeFileCount > 0`,
    `trackFileCount > 0`), each located target is lstat'ed through the source's root. A missing
    target is a job warning ("Radarr folder /movies/X maps to /media/X, which does not exist:
    check the path mappings"), and that source gets an untargeted sync instead. *(As built:
    only the item's current folder is checked; the old folder of a move or a delete is
    expected to be gone. The check requires the folder's exact spelling in its parent's
    listing, and listings are cached per folder while its stat is unchanged.)*
  - Per source, it then queues `sync {destinationId, sourceIds:[s], paths}` for every enabled
    destination linked to that source whose `settings.syncOnArrChange` is on (default on).
    *(As built: destinations do not store `syncOnArrChange` yet, so it is on for every
    destination; DEFERRED.md.)*
  - A folder that locates to a source root, or more than `jobs.MaxTargetPaths` targets in one
    source, queues that source untargeted. An unmappable folder is a job warning that names the
    mapping to add.
  - A `SyncAfter` follow-up is also queued when the refresh fails, using the folders the index
    already had. It is not queued after a cancel.
  - An **untargeted refresh with `SyncAfter`** (only an overflow merge creates one, §12.2) queues,
    when it ends, an untargeted sync `{destinationId, sourceIds:[s]}` for every source that a root
    folder of the integration locates into, for each linked destination with `syncOnArrChange`.
- **Start-up.** `App.Start` queues a full refresh (trigger `startup`, deduplicated) of every
  enabled *arr integration, so events lost while Bunkarr was down are reconciled. An upgraded
  Phase 1 install has no *arr integration, so it queues nothing new.

### 6.2 Tautulli, Seerr, Maintainerr refresh
- **Tautulli.** The version and server checks from §4.4. The sections come from the linked Plex's
  `plex_sections`. The sections whose `keep_history` is 0 and the number of users whose
  `keep_history` is 0 are recorded in `index_state.stats` (counts and section keys only, no
  names). It then aggregates `get_history` into `watch_stats`, replacing the rows by `rating_key`
  and by `guid`.
- **Seerr.** Every request, paged, replaces `seerr_requests`: status, type, tmdb/tvdb ids, rating
  key, 4K, seasons, user id, date. The evaluator counts statuses 1, 2, 4 and 5.
- **Maintainerr.** Reads `overlay-data`, then `rules` (rule group ↔ collection), then
  `rules/exclusion?rulegroupId=N` per group. It stores in `maintainerr_items` only the members
  that are **pending deletion**, which mirrors Maintainerr's own worker. All of these must hold:
  - the collection is active;
  - its `arrAction` is 0, 1, 2 or 5 (the actions that delete files);
  - `deleteAfterDays` is set;
  - the member did not fail its rule check (unless it was added manually);
  - the member is not excluded (a show or season exclusion covers its children).

  Each row stores `plex_integration_id` (the Maintainerr integration's `plexIntegrationId` at
  refresh time), `library_id` (the collection's Plex library), the level, the rating key, the
  tmdb/tvdb ids at the collection's level, the season and episode numbers from the linked Plex
  index (NULL when unresolved), and a **state**:
  - `pending`: Maintainerr will delete it;
  - `undecided`: the member's rule group, or the global exclusions, hold a show or season
    exclusion, and the member's ancestors are not in a fresh `plex_items` of the linked Plex, so
    whether the exclusion covers it cannot be decided. When the linked Plex index is not fresh
    during the refresh, every season- and episode-level member is `undecided`.

  The deletion date is `addDate + deleteAfterDays`. Versions below 3.4.0 are refused: 2.x used a
  numeric `plexId`, 3.0 renamed it to `mediaServerId`, and 3.4.0 added `overlay-data` and `tvdbId`.

### 6.3 Plex library index
It runs when `index.enabled` is set; the UI turns that on when a Tautulli or Maintainerr
integration is linked, or a rule uses a Plex-based condition.
1. Store `Sections()` in `plex_sections`: key, title, type, and each location as Plex sees it,
   mapped and located.
2. Fill `plex_items` from `AllItems` for every movie, show, season, episode, artist, album and
   track row, with `index` and `parentIndex`.
3. Fill `plex_files` from each part's `file`, mapped (`local_path`) and located.
4. Record `instance_id = <url>#<machineIdentifier>`. A different machine identifier at the same
   URL replaces the whole index.

## 7. Webhooks (`internal/webhooks`)

### 7.1 Routes and intake (S12, S13)
- **Routes.** `POST /api/v1/webhook/{app}/{integrationId}` and `POST /api/v1/webhook/{app}`, with
  `app` one of `sonarr`, `radarr` or `lidarr`. They are mounted outside the session group and use
  their own `webhookAuth` middleware (webhook key only, D7).
- **Credential.** The key is taken from the Basic auth password (the user name is ignored), the
  `X-Api-Key` header or `?apikey=`, and compared in constant time with the in-memory map of
  integration id → webhook key. The map is loaded at start and updated whenever an integration is
  created, changed, deleted or has its key rotated.
- **Integration.**
  - With an id: the key must be that integration's (401 otherwise). An unknown id gets 404; an
    integration that is disabled or of another app gets 409.
  - Without an id: the integration whose key matches and whose type is `app` (401 when none).
- **Checks, in order**, cheapest first, so a flood is refused before its body is read:
  1. the credential: a valid key proceeds; an invalid one gets 401, or 429 when the client address
     is blocked by the failed-authentication limiter (it is checked before `{app}`, so a request
     without a valid key gets 401 even for an unknown app);
  2. the integration (404, 409);
  3. the rate (429) and the backlog (503);
  4. a body-read slot (wait up to 10 s, then 503);
  5. `Content-Type` (415), size (413 beyond 16 MiB) and JSON shape (400).
- **What is stored.** A `webhook_events` row:
  - `event_type`, the scheduling `class` (§7.3), and `targets`, the *arr item ids the event names;
  - `payload`: the compact body of a handled event when it is at most 64 KiB; a larger one becomes
    `{"truncated":true,"eventType":…,"ids":[…]}` with `truncated = 1`; an ignored event type
    stores `{}`.
  - Headers, the query and the client address are never stored.
- **Answers.** 200 `{}`. For a Test event the row gets outcome `test` and `processed_at` at once,
  and nothing more is done.
- The CSRF middleware does not affect the *arrs: they send no `Origin` or `Sec-Fetch-Site`.
- *(As built.)* `Store.Insert` does no work after its commit, so an error always means the event
  was not stored; once stored, the answer is 200 and the processor is notified. The note that
  another integration used the generic route (a warning of `GET /integrations/{id}/webhook`, §13)
  is kept in memory and resets at a restart.

### 7.2 Events

| App | `eventType` | Refreshes | Class |
|---|---|---|---|
| all | `Test` | nothing (no lookup at all) | `test` |
| radarr | `Download` (new, `isUpgrade`, download-client, same-path), `MovieFileDelete` (manual, upgrade), `Rename`, `MovieAdded`, `MovieDelete` | `movie.id` | `download`; `MovieFileDelete` with `deleteReason: upgrade`: `upgrade_delete`; `MovieDelete` with `deletedFiles: true`: `delete`; the rest `change` |
| sonarr | `Download` (single, multi-episode, upgrade, and the ImportComplete form: `episodeFiles[]` plural, `sourcePath`, `destinationPath`, no `isUpgrade`), `EpisodeFileDelete`, `Rename`, `SeriesAdd`, `SeriesDelete` | `series.id` | as for Radarr |
| lidarr | `Download` (also replacing existing files), `Rename`, `Retag`, `ArtistAdd`, `AlbumDelete`, `ArtistDelete` | `artist.id` | `download`; the deletes with `deletedFiles: true`: `delete`; the rest `change` |
| all | `Grab`, `Health`, `HealthRestored`, `ManualInteractionRequired`, `ApplicationUpdate`, anything else | nothing (outcome `ignored`) | `ignored` |

- **The upgrade pair.** A `*FileDelete` with `deleteReason: upgrade` arrives before the `Download`
  with `isUpgrade`. When the new file is copied from another volume, the gap is minutes (the *arr
  deletes the old file first, then copies the new one, then sends Download). So the delete holds
  its item until the Download arrives (§7.3). Bunkarr still holds the old file from the previous
  sync, and S6 orders the targeted sync.
- An add emits no `Download` for files found on disk, so adds are refreshed and synced too.
- The 60 s delay of the `delete` class exists because a delete with `deletedFiles` arrives before
  the folder is removed (spike).
- Payload details (`deletedFiles`, `recycleBinPath`, `renamed*Files`) are shown in the event list
  only; apart from the class, they drive nothing (S12).

### 7.3 Processing and latency
- **The processor.** One goroutine, started by `App.Start`. On start it re-reads the unprocessed
  events in id order, in pages of 500, from their `targets` and `class` (payloads are never
  parsed again), then consumes a channel.
- **Coalescing: a due time per item.** The pending set maps `(integration, item id)` to a due
  time:
  - `change`, `download`: `due = max(due, eventAt + 5 s)`, capped at the item's first pending
    event + 20 s;
  - `delete`: `due = max(due, eventAt + 60 s)`, with no cap;
  - `upgrade_delete`: the item is held until a `download` for the same item arrives (then the
    5 s rule applies) or 30 min pass.
- *(As built.)* The `download` that releases an upgrade hold restarts that item's quiet window.
- **Flushing.** A flush takes the due items of one integration and enqueues one
  `refresh {integrationId, arrItemIds, syncAfter: true}` with trigger `webhook`. *(As built: it
  also takes that integration's items that are only waiting out a quiet window ending within one
  more window, so one burst is not split into two refreshes. Deletes and upgrade holds never join
  early.)* In one write, it
  then marks their events processed with the job id: outcome `queued`, or `coalesced` when the
  returned job's `queued_at` is earlier than the flush (Enqueue returned an existing job). An
  event without an item id, or whose enqueue failed three times, gets outcome `failed`.
- **After a crash.** A crash between the enqueue and that write re-processes the events after the
  restart; the enqueue merges them into the same job (§12.2).
- **A cancelled refresh.** When a refresh that carries webhook events is cancelled, an `OnFinish`
  hook clears `processed_at` of the events that name that job and notifies the processor, so
  their items are flushed again. (A cancelled sync is not re-armed: the user cancelled it, and the
  next full refresh and sync reconcile.) *(As built, review fixes:)* the write that marks events
  processed checks the job's status in the same transaction; when the refresh was cancelled
  between the enqueue and that write, nothing is marked and the items are flushed again later.
  `Processor.Start` also re-arms the events of refreshes cancelled before their hook ran (a stop
  between the cancel and the hook).
- **Pruning.** At start, hourly, and when an insert took the payloads over 256 MiB (the
  processor loop prunes then, down to 90 %, S13): events older than 30 days, and the oldest
  beyond 50,000 rows or 256 MiB.
- *(As built.)* When `Start` fails, the loop never runs and `Stop` returns at once, so the job
  manager stopped after it keeps its shutdown grace.
- **Refresh pool.** Refresh jobs run in their own pool of 2 slots, not counted against
  `jobs.workers`, so a long sync and a verify cannot starve the targeted refresh.
- **Latency budget, import to copy start:**

  | Step | Time |
  |---|---|
  | webhook | ≤ 0.1 s (spike) |
  | quiet window | 5 s (≤ 20 s) |
  | targeted refresh | ~1 s |
  | queue | 0 s |
  | scan of one item folder and plan | < 5 s |
  | **total** | ≤ 30 s |

  This holds when a worker and the destination are free and the file is visible to Bunkarr.
  What delays it, all common at night when the *arrs import:
  - a full refresh of the same integration holds `integration:<id>` (Sonarr needs two requests
    per series);
  - both `jobs.workers` busy (for example a nightly sync and a weekly verify) delay the targeted
    sync, though no longer the refresh;
  - another destination's sync scanning and planning the same source holds `source:<id>`;
  - a sync, verify or retention job of the same destination holds `dest:<id>` (a 5 % verify of
    50 TB can take hours);
  - a file not yet visible through Bunkarr's mount (NFS attribute cache): up to 60 s more (§9.1).

  The README states this. Acceptance 1 measures the idle case.

## 8. Tiers (`internal/tiers`)

### 8.1 Evaluation
For each live catalog file and each destination linked to the file's source, the evaluator
decides a **tier** (`full`, `manifest` or `skip`) and the reasons:

1. **Irreplaceable.** A file flagged irreplaceable (§8.7) is `full`, with reason "irreplaceable
   (flag #id)". It is built in and cannot be overridden.
2. **The rules.** These are the enabled rules whose `destinationIds` is NULL (all) or contains
   the destination, taken in `priority` order. The UI labels the field "Applies at": a rule is
   not evaluated at other destinations, which is not the same as "skip" there. ("Full at D1 only"
   is rule A `full` at [D1] plus rule B, the same conditions, `skip` at [D2].) Each rule
   evaluates to true, false or unknown (§8.3):
   - `match: all`: false if any condition is false, true if all are true, otherwise unknown;
   - `match: any`: true if any condition is true, false if all are false, otherwise unknown;
   - no conditions: true.
3. **The first rule that is true decides**, unless an earlier rule was unknown and its action is
   more protective. In that case the most protective unknown rule decides, and the decision has
   `unknownPromoted: true` (S14).
4. **No rule is true.** The built-in fallback `full` applies ("no rule matched"; rule id 0).
   Nothing is more protective than `full`, so a fallback decision is never unknown-promoted.
5. **Hardlink groups** (same scan) take the most protective tier among their names. A file
   holding several items follows D12. A sidecar follows its media file (§8.3).

`Decision = {tier, ruleId, ruleName, reasons:[Reason], unknown:[Reason], unknownPromoted,
revision}`. A reason is structured, and the UI renders its text (one Go function renders it for
job logs):
`Reason = {ruleId, conditionIndex, field, op, value, actual, result: true|false|unknown, source:
{kind, integrationId?}, why?}`. `actual` is the fact that was compared ("HD-1080p", 12 plays, a
tag label list, a deletion date); `why` says why a result is unknown ("Radarr cache is 31 h old",
"two *arr integrations claim this file"). A Seerr user appears by id only.

### 8.2 Conditions

A condition is `{field, op, value}`. A rule has at most 32 conditions and a rule set at most 100
rules. An unknown field or op, or a value of the wrong type, is a 400 that names the rule and
condition index. A value that no fresh index knows (a tag, profile, root folder, section, Seerr
user) is accepted with a warning (§8.7).

| Field | Ops | Value | From | Unknown when |
|---|---|---|---|---|
| `arr.managed` | `is` | bool | whether an *arr item claims the file | a claim cannot be decided (see the next rows) |
| `arr.tag` | `has`, `hasNot` | tag label (case-insensitive) | the file's *arr item | no *arr item for the file, its *arr cache not fresh, the row mismatched (S18), or two integrations claim it (D12). A file outside every mapped root folder of every *arr integration, while every *arr cache is fresh, is **unmanaged**: its `arr.*` conditions are false (`hasNot`, `isNot`: true), not unknown |
| `arr.qualityProfile` | `is`, `isNot` | profile name (case-insensitive) | *arr item | as above |
| `arr.rootFolder` | `is`, `isNot` | root folder path as the *arr sees it | *arr item | as above |
| `arr.monitored` | `is` | bool | *arr item | as above |
| `plex.section` | `is`, `isNot` | `"<plexIntegrationId>:<sectionKey>"` | the source's `plexSectionId`, else the `plex_sections` location that contains the file | neither applies, or the Plex index is not fresh |
| `source` | `is`, `isNot` | source id | catalog | never |
| `file.size` | `gt`, `gte`, `lt`, `lte` | bytes | catalog | never |
| `file.age` | `olderThan`, `newerThan` | days | `addedAt`: the *arr file's `dateAdded`, else Plex `addedAt`, else the catalog's `first_seen_at` | never |
| `media.genre` | `has`, `hasNot` | genre (case-insensitive) | the *arr item's genres (Plex genres are deferred, D16) | no *arr item, or its cache not fresh |
| `seerr.requested` | `is` | bool | a request with status 1, 2, 4 or 5 for the item's tmdb id (movie) or tvdb id (series; tmdb as fallback). The Plex rating key is a further fallback only when the Seerr integration has `plexIntegrationId`, looked up in that Plex's `plex_files`. For TV, a request whose `seasons` list is empty covers every season; otherwise it covers the listed seasons, and the file's season must be among them | no id known, or the Seerr cache not fresh. A file whose season is unknown (an extra in the series folder): true if one counted request covers every season of the series, false if the series has no counted request, unknown otherwise |
| `seerr.requestedBy` | `in`, `notIn` | Seerr user ids | as above | as above |
| `tautulli.playCount` | `gt`, `gte`, `lt`, `lte`, `eq` | int | `watch_stats` of the Tautulli integration, by the file's rating key in the `plex_files` of `tautulli.plexIntegrationId`, else by guid (no row = 0 plays) | no Plex item for the file, the Tautulli or linked Plex cache not fresh. When any user or the file's section has `keep_history = 0`, the count is a lower bound: `gt` and `gte` are true when the observed count satisfies them and unknown otherwise; `lt`, `lte` and `eq` are unknown |
| `tautulli.lastWatched` | `olderThan`, `newerThan`, `never` | days (none for `never`) | as above. With no row: `never` true, `olderThan N` true, `newerThan N` false. Day boundaries are strict: older than N means `now − lastWatched > N × 24 h` | as above. With history off for a user or the section: `newerThan` is true when observed and unknown otherwise; `olderThan` and `never` are unknown |
| `maintainerr.pendingDelete` | `is` | bool | `maintainerr_items` rows of this Maintainerr integration with state `pending`, matched by the file's Plex item in the `plex_files` of `maintainerr.plexIntegrationId` (its own rating key, its season's or its show's). The tmdb/tvdb fallback applies only to movie- and show-level rows, only when the row's rating key is not in that Plex's `plex_items` (key churn), and only when the file's `plex.section` is `<plexIntegrationId>:<library_id>`. No row = false | a matching row is `undecided`; the file has no Plex item and its *arr item's ids match a season- or episode-level row; the file has no Plex item and no ids; the Maintainerr or the linked Plex cache not fresh |
| `flag.irreplaceable` | `is` | bool | `item_flags` (§8.7) | never |

### 8.3 Facts and "unknown"
`tiers.LoadFacts(ctx, sourceID)` builds, once per sync and source, the facts of every live file
from the caches, read through the owners' exported queries (§14). Per file:
- **\*arr item.** Only non-deleted items supply facts.
  - By exact location: the `arr_files` row whose `local_path` equals the file's local path (the
    source's path plus `rel_path`, so overlapping sources match too) and whose size equals the
    catalog's. A row at that path with another size is `mismatched` (S18): facts unknown.
  - When the source has `arr_integration_id`, only that integration's rows and items count.
    Otherwise, two integrations claiming the file, or its folder, make every `arr.*` fact unknown
    (D12).
- **Sidecars and extras.** A file with no `arr_files` row is attributed in this order:
  1. to the media file in the same directory whose name is the longest stem prefix of its name
     (`X.en.srt` and `X.nfo` go to `X.mkv`). The sidecar takes that media file's decision at each
     destination (reason "follows X.mkv"), unless its own flag makes it `full`;
  2. otherwise by folder: the item whose mapped folder is the longest prefix. It gets the item's
     item-level facts; episode-level facts (plays, season requests, episode-level Maintainerr
     rows) are unknown.
- **Unmanaged.** A file outside every mapped root folder of every *arr integration, while every
  *arr cache is fresh, is unmanaged (§8.2). A file inside a root folder with no matching item
  stays unknown: it may be an import that is not indexed yet.
- **Plex item.** Found by `local_path` in `plex_files`, together with its parent and grandparent
  keys and the section. Tautulli and Maintainerr facts use only the rows of their linked Plex
  integration.
- **Freshness** per integration (§6).
- **Flags** (§8.7).

The evaluator is pure (facts in, decision out) and is table-tested.
`tiers.FactsFor(ctx, fileID)` serves the Library item view and the execution-time check of a
release (§8.5).

### 8.4 Presets (`GET /tiers/presets`; loaded into the editor, never applied directly)
- **Back up everything** (the default): no rules.
- **Spec default: manifest by default.** Rule 1: `arr.tag has bunkarr-full` → `full`. Rule 2: no
  conditions → `manifest`. Plex DB and *arr config backups are always full; they are not tiered.
  - The UI explains that this preset stops copying new untagged media. Media already backed up is
    kept (S15) until you release it, or until it is deleted or replaced under another name at the
    source (then it is retained for `deletedDays`, and the new version is not copied unless it is
    tagged).
  - A file whose *arr facts are unknown stays `full` (S14: rule 1 is unknown, and `full` is more
    protective than rule 2's `manifest`). Such copies are guarded (S10), so a stale Radarr cannot
    flood the destination or stop the other backups.
  - A file outside every *arr root folder (home videos, a Plex-only source) is unmanaged and gets
    `manifest` while the *arr caches are fresh.
- **Keep Maintainerr deletions as manifest only.** Rule 1: `maintainerr.pendingDelete is true` →
  `manifest`, so the files stay listed for re-acquisition. The description says that Maintainerr
  has no authentication: anyone who can reach it can add items to a deleting collection.

### 8.5 Tiers in a sync (`internal/syncer`)
`syncer.Options.Tiers` returns a decision function per destination and source. The planner then
works like this:
- **`full` files** are planned exactly as in Phase 1. Their copy, update, move, link and adopt
  items carry `detail.tier` (the Decision).
- **Non-full files with no live record** get no item of their own. A dry run records a `skip`
  item for each ("not copied: manifest (rule 'X')"), with the decision in its detail. They can
  still be the new side of a move (below).
- **Non-full files with a live record** are **kept** (S15). The record counts as seen, so it is
  never taken for a vanished file, and it gets no update, repair or link. A dry run records a
  `skip` item with `detail.reason = "kept"`.
- **Moves.** The move pairing considers every live catalog file without a live record, whatever
  its tier. A paired move is planned and executed like any move (no bytes). The moved record is
  then kept when its new tier is not `full`. A vanished name that pairs with nothing is retained
  (S5), subject to D14 in a targeted sync.
  - Content that moved to another source cannot be paired. When a backed-up file's content
    reappears in another source under a non-full tier, the sync counts it in
    `movedToNonFull`, and the preview shows it.
- **Release.** A real run with `releaseDemoted` must carry `releaseOf` (the dry run the user
  confirmed) and `releaseRevision` (§12.1 validation).
  - It plans a `retain` item with `detail.reason = "released"` and `detail.tierRevision` only for
    records that were release items of that dry run and are still kept now.
  - When the item runs, it reads `tiers.revision` again and re-evaluates the file (`FactsFor`).
    If the revision changed, the item ends `skipped` ("rules changed since the preview; not
    released"). If the tier is `full` again, it ends `skipped` ("tier is full again; not
    released").
  - It then runs through the Phase 1 retain, promote and intent machinery and records reason
    `released` with `expires_at = now + deletedDays`. A retain with reason `released` skips the
    "reappeared at the source" check and the S6 wait; every other retain keeps both.
  - A dry run with `releaseDemoted` plans a release item for each kept record (`present`,
    `linked`, `link_recorded`) and records `stats.tierRevision`. A `missing` kept record is left
    alone. `releaseDemoted` with `paths` is a 400.
- **Guards.** S6 as amended. S10(b) counts releases and unknown-promoted copies.
- **Free space.** The check counts the `full` items that are not held. It fails the job only when
  the copies whose tier was decided (not unknown-promoted) do not fit on their own. When the
  unknown-promoted copies are what overflows, all of them are held ("held: tier unknown because
  <integration> is not fresh; not enough free space"), a warning notification is sent, and the
  rest runs.
- **Same-path replacement with equal size and mtime.** When the *arr reports a new file id at a
  path (the `arr_files` row's `date_added` is after the record's `copied_at`) and the record has
  the same size and mtime, the planner compares the head and tail hashes of the source and
  destination files and plans an `update` on a mismatch.
- **Stale references.** One job warning per rule value that no fresh index knows (§8.7), so such
  a sync ends `completed_with_warnings`.
- **Stats.** `bytesPlanned` counts `full` items only. The sync stats gain `tiers: {full, manifest,
  skip, unknownPromoted}` (each `{files, bytes}` of the live files), `filesKept`, `bytesKept`,
  `filesReleased`, `bytesReleased`, `movedToNonFull`, `staleReferences` and `tierRevision`.
- **Real runs** record no `skip` items: those files are counted in the stats, not listed. This
  keeps a manifest-by-default library from writing 100k items on every sync.

### 8.6 Preview
`POST /tiers/preview` evaluates the saved rules, or a draft sent in the body, over every live
file of every enabled destination's sources. It computes, per destination:
- `stored`: what the live records hold now;
- `full` (with `uniqueBytes`: a hardlink group counts once), `manifest` and `skip`;
- `unknownPromoted`: full only because a fact is unknown;
- `toCopy`: full files without an up-to-date record;
- `kept`: non-full files with a record, which a release would free;
- `movedToNonFull`;
- counts and bytes per rule;
- the config backups, which are always full: each Plex DB and *arr backup aimed at it, with the
  size of its last version.

It also lists `unknownSources` (the caches that are not fresh, and why) and `staleReferences`.
The result stays in memory for 10 min, with at most 4 previews; `GET /tiers/preview/{id}/items`
pages through it. Nothing is written. For 100k files it should answer within 5 s; a benchmark at
200k files runs outside `-short`.

### 8.7 Rules store and flags
`tier_rules` plus the setting `tiers.revision`, an integer bumped by every save.
- `PUT /tiers/rules` replaces the whole ordered set in one transaction.
  - It must carry the revision it was based on; a mismatch is a 409.
  - Rules sent with an `id` keep it; new rules get new ids.
  - It returns `warnings: [{ruleIndex, conditionIndex, message}]` for each value no fresh index
    knows (a tag, profile, root folder, section or Seerr user that was renamed or deleted). A
    renamed profile otherwise makes a condition silently false everywhere.
- Deleting a destination removes its id from every `destination_ids` array. An array left empty
  means the rule applies nowhere, never "all" (the UI flags it).
- **Flags** (`item_flags`): `POST` and `DELETE`. Two kinds:
  - **\*arr item.** Set on an item (`integrationId, arrKind, arrId`). The flag copies the item's
    external ids from the index (tmdb, tvdb, imdb, mbid; POST is a 400 when the item has none) and
    records the item's located folder (`last_source_id`, `last_rel_path`), refreshed whenever the
    flag resolves.
    - It applies to the files of every non-deleted item of that kind, in any integration, that
      shares an external id with it (the same tmdb for a movie, tvdb for a series, mbid for an
      artist). `arr_id` is only a cache: an item at that id with other external ids is ignored.
    - When no item resolves (removed from the *arr but files kept, purged, re-added with a new
      id, or the integration deleted), the flag falls back to its last folder, as a path flag.
    - Deleting the integration sets `integration_id` to NULL; the flag stays.
  - **Path.** A file, or every file under a folder (`sourceId, relPath`).
    - An exact-file path flag follows a paired move: when the syncer records the move, it updates
      the flag's `rel_path` in the same transaction (`tiers.MoveFlagsTx`).
    - A folder flag follows a folder rename: when a sync's moves take files from under F to under
      G with the same remainder and no live catalog file remains under F, the flag's `rel_path`
      becomes G.
    - The same updates apply to an *arr flag's `last_rel_path`.
  - The UI flags the item by default when the file has one.
  - `GET /tiers/flags` lists each flag with `resolved` and, when false, why ("no item with tmdb
    10331"; "folder not found").
  - **Expiry.** The retention job does not expire a retained record whose source path is covered
    by an irreplaceable flag (a path flag, or an *arr flag's item folder). Its `expire` item is
    held with a warning ("irreplaceable: not expired; remove the flag to let it expire").

## 9. Sync changes (`internal/syncer`, `internal/catalog`)

### 9.1 Targeted sync (`Params.Paths`)
A sync whose `paths` is set scans and plans only those subtrees of its one source:
- **Preflight and reconciliation** are unchanged, including the whole destination's
  reconciliation (§4.2 of phase1.md) when it is not a dry run. A follow-up sync (trigger
  `webhook`, or queued by a refresh) whose S3 marker check fails ends `completed_with_warnings`
  ("destination not mounted; skipped") instead of failing, and notifies as §12.5 says. *(As
  built: a follow-up sync is one with trigger `webhook` or with `paths`; only a refresh queues
  syncs with `paths`. An untargeted follow-up of a scheduled, start-up or manual refresh (a
  folder that is a source root, missing, or too many paths) carries no marker, so it fails like
  an ordinary sync when the destination is not mounted; DEFERRED.md.)*
- **Scan.** `catalog.Scanner.ScanPaths(ctx, sourceID, paths)` holds the source's lock.
  - The S10(a) root checks apply as usual: the root exists, and has the same filesystem and
    identity.
  - **Resolving a target.** Each target is resolved one component at a time from the source root
    with lstat. Every ancestor must be a real directory (not a symlink), must not match the
    source's exclude matcher (the defaults plus the source's patterns, anchored on the relative
    path), and must not be a forbidden root by `(dev, ino)` (S4). Each component is opened with
    `O_NOFOLLOW` and its device and inode re-checked (S1). A target that fails is dropped with a
    warning, and its rows are left alone.
  - Each target is then walked like the full scan (excludes, symlinks skipped, dev/ino checks).
    Rows under a target whose walk completed are upserted, and those not seen are marked deleted.
  - A target that no longer exists marks its rows deleted, but only while its parent directory
    exists and is not "empty" in the S10(a) sense: a parent with no entries while the catalog has
    live rows under it outside the target is refused (warning, rows kept). An unmounted nested
    mount is caught this way.
  - *(As built.)* Rows are keyed by the name the folder lists, never by the caller's spelling.
    A target whose last name its folder lists only under another spelling (case or Unicode
    normalization, on a filesystem that ignores them) is gone under the spelling given, as the
    full scan sees it, so a sync that plans both spellings pairs the rename. Another spelling of
    an ancestor drops the target. A path whose parent folders fail the checks is dropped.
  - `ESTALE` during a targeted scan is retried once after 5 s before it is fatal.
  - An error inside a target keeps that subtree's rows (as for an unreadable directory).
  - Nothing outside the targets is read or changed.
  - The scan increments `scan_seq`, so group ids never collide, and recomputes the source's stats
    from the rows. `last_scan_at` is left alone: it means the last full scan.
- **Expected files** (trigger `webhook`). After the scan, the `arr_files` rows located under the
  targets are compared with the catalog. When an expected file is missing or has another size,
  that target is scanned again after 5, 15 and 30 s (at most 60 s in all). A file still missing
  is a warning that names it and its *arr, and the job ends `completed_with_warnings`. This covers
  mounts that show a new file late (NFS attribute caching) and sidecars written after the import
  event. *(As built:)*
  - Files the source excludes are not expected.
  - A file that is not live with the *arr's size is looked up on disk the way the scan resolves
    it, and only a **pending** one is waited for: nothing at its path yet, a regular file the scan
    did not list, a size on disk other than the catalog's (read from an open file, which
    revalidates an NFS client's cached attributes), or a lookup failing with EIO or ESTALE. A
    symlink, an excluded or symlinked folder, a non-regular file or an unreadable folder
    (**never** catalogued), a name its folder lists only under another spelling, and a file the
    catalog lists with another size than the *arr's (transcoded in place) are reported once and
    not waited for.
  - The **source lock is released during each wait** (the `dest:<id>` lock stays), so another
    destination's sync of the same source scans and plans meanwhile; the rescan and the plan run
    under the lock again. A source deleted, disabled or given another path or destination folder
    meanwhile is not synced by this job (logged). (With the lock held, a late file made a second
    destination's sync wait behind the first and miss acceptance 1's 60 s.)
  - A lookup refused with EACCES or EPERM counts as never catalogued (the scan keeps that
    folder's rows); a missing path, EIO and ESTALE are waited for. A rescan's warnings are logged
    once, and the job's `files` count is the live files under the targets when the plan reads
    them, whoever scanned them meanwhile.
- **Hardlinks.** A targeted file with `nlink ≥ 2` has its possible partners looked up in the
  catalog: rows of the same source that had the same `(dev, ino)` in the last scan. Each partner
  is lstat'ed **now**, and grouped only when its current stats satisfy phase1.md §4.3 (on FUSE,
  equal head/tail hashes too). The old numbers only nominate candidates; grouping uses stats
  read in this scan, so the "never compared across scans" rule holds. Partners found this way
  belong to this scan: their rows are refreshed, never marked deleted.
- **Plan.** It covers the catalog files and live records whose source path is at or under a
  target path, and *(as built)* the other names of those files' hardlink groups, even outside
  the targets, so a new name linked to an already backed-up file is linked, not copied again. It
  reads only the records of its scope *(review fix)*: the S11 name index holds the live records
  its new names can collide with, other-case spellings included (found by index lookups; a path
  that is not valid UTF-8 falls back to the whole destination), and the S6 check before a retain
  reads one folder. A vanished name is retained only under D14 (S10). Moves are found only inside
  the targets; a move across folders outside them becomes copy + retain at the next full sync.
  Everything else is as in §8.5.
- **Finish.** Empty directories are pruned inside the targets only. The link manifest is
  rewritten from the records. No manifest export follows (§9.2).

### 9.2 Follow-ups
A sync that is neither a dry run nor targeted, and ends `completed`, `completed_with_warnings`
or `failed` (not `cancelled`), enqueues `manifest_export {destinationId}` with the same trigger
(deduplicated) when the destination's `settings.manifest.afterSync` allows it:
- `auto` (the default): when at least one enabled *arr integration exists;
- `on` or `off`.

*(As built: destinations do not store `manifest.afterSync` yet, so every destination behaves as
`auto`; DEFERRED.md.)*

The manifest does not depend on the sync having succeeded, and it is the only protection
manifest-tier files have, so a failing sync must not stop it. An upgraded Phase 1 install without
an *arr integration runs exactly the jobs it ran before. `syncer.Options.Enqueuer` provides the
enqueue.

### 9.3 Default excludes
`*.partial~` and `*.backup~` join the default excludes: they are the temporary names of the
*arrs' transactional copies, and webhook scans now run while a season pack is still being copied
into the folder. (To confirm with the spike harness: a cross-device import in copy mode.) This is
a Phase 1 behaviour change and goes into the CHANGELOG; its only effect is that stale temp files
from the *arrs are no longer copied. *(As built: the matcher is unexported, so `internal/api`
(expected files) and `internal/syncer` (`expected.go`) each carry a copy of its rules, checked
against the scanner by tests; an exported matcher would remove them, DEFERRED.md.)*

## 10. \*arr backups (`internal/arrbackup`, runner `arr_backup`)

1. **Preflight.**
   - The integration is an enabled *arr.
   - The destination is `params.destinationId`, or `settings.backup.destinationId` when that is
     0. It must be enabled, and the S3 checks apply.
   - The destination's `capabilities.enforcesModes` must be true, or
     `settings.backup.acceptInsecureModes` must be set (S17). Otherwise the job fails with the
     explanation, and saving such settings is a 400 that names the flag. A destination probed
     before this field existed counts as false until it is probed again.
   - Recovery follows phase1.md §5: `.partial-job*` directories and interrupted prunes are
     cleaned up. Staging of jobs that failed for good is removed after 24 h.
     *(As built:)*
     - A complete but unrecorded version is recorded only when its manifest is of this
       integration's application and it lies in this integration's folder, or its manifest names
       an `arr_backup` job of this integration in this database (id and queue time). Integration
       ids start over with a new Bunkarr database, so a folder with this id may hold another,
       deleted integration's versions, which this integration's pruning must not take.
     - A run removes its own staging directory whenever it returns. An `OnFinish` hook removes
       the staging of a job that crash recovery failed for good, and start-up sweeps staging
       directories nothing has touched for a day.
   - *(As built.)* Before the *arr is asked for a backup, the access is checked: the Backups
     folder is read, or a 1-byte range request of the newest backup is sent over HTTP. So a
     misconfigured job fails before it leaves a manual backup behind in the *arr.
   - The lock key is `arrbackup:<integrationId>`.
2. **Dry run.** One `skip` item: "would copy <App>'s scheduled backup <name>" or "would create a
   <App> backup", with the method (`folder` or `http`) in the detail, and copy it to the
   destination. No command is sent (S9).
3. **Choose the backup.** `GET system/backup`.
   - If the newest `scheduled` entry is younger than `backup.maxScheduledAgeDays` (default 7, the
     *arrs' own default interval), it is used, and no command is sent. If this destination already
     has a snapshot of this integration with that backup name, the job completes with
     `unchanged: true` and copies nothing.
   - Otherwise `POST command {"name":"Backup"}`, then poll `GET command/{id}` every second, for at
     most 10 min, until `status: completed`. A `result` other than `successful` fails the job. The
     backup is the newest `manual` entry whose `time` is no earlier than the command's `queued`
     minus 5 s.
   - *(As built:)*
     - "Unchanged" needs the recorded copy to read back: its directory holds `manifest.json` and
       the zip with the recorded size and sha256. A copy that does not is marked `failed` (not by
       a dry run) and the backup is copied again.
     - A scheduled backup whose earlier copy failed verification is not copied again: the job
       makes a new backup.
     - The command's id is recorded as a pending job item. A resumed job follows that command
       (unless the *arr no longer knows it), else reuses a manual backup made since the job was
       queued, so a crash or a shutdown does not make the *arr create another one.
4. **Entries.** Only `id`, `name`, `type`, `size` and `time` are decoded; `path` never is (S18).
   `type` must be `manual`, `scheduled` or `update`, and `name` must match
   `^[A-Za-z0-9._-]{1,200}\.zip$`. The destination file name is that validated name.
5. **Fetch it.**
   - **`folder`:** read `<backupFolder>/<type>/<name>` (S18). Its size must equal the API's size;
     while it is smaller, retry up to 5 times, 1 s apart.
   - **`http`:** `GET url.JoinPath(base, "backup", type, name)` with the key and no redirects. It
     needs a 200 with a zip content type (*as built*: `application/zip`,
     `application/x-zip-compressed`, which Lidarr 3.1.0 sends, or `application/octet-stream`;
     Lidarr also ignores `Range`). A 302 or 401 fails with: "<App> requires a login to
     download backups: set its Backups folder in Bunkarr (Settings → Connect → <App>) or set
     Authentication Required to 'Disabled for Local Addresses' in <App>".
6. **Stage and verify.** The zip goes to `<config>/staging/arrbackup-job<id>/` (directory 0700,
   file 0600) and is hashed with sha256. Before any entry is read:
   - at most 64 entries, each name at most 255 bytes;
   - the total `UncompressedSize64` at most 20 times the zip's size or 256 MiB, whichever is
     larger, and at most 32 GiB (*as built*: a fresh Radarr 6.4.4 zip expands 23.5 times, a
     604 KiB database in a 26 KiB zip);
   - free space in staging (statfs) at least the database entry's size plus 1 GiB.

   Then:
   - every entry's CRC is read, streamed;
   - `config.xml` and `<app>.db` must be present at the top level;
   - only the entry named exactly `<app>.db` is extracted, to
     `staging/arrbackup-job<id>/check.db`, opened with `O_CREATE|O_EXCL|O_NOFOLLOW` and mode 0600,
     with `io.CopyN` of the declared size; the check fails if more data follows;
   - `check.db` passes `PRAGMA quick_check`, run by `plexdb.QuickCheck` (exported for this). It
     opens the copy `mode=ro&immutable=1` on plexdb's private driver, with its collation stubs, so
     an unknown collation is never reported as damage.

   The result is `integrity` `ok` or `failed`. A failed version is recorded and kept 7 days, and
   the job fails, as for Plex. *(As built: `manifest.json`'s `integrity.database` can also be
   `not-checked`, when the zip failed before the database was checked, and each entry's `crc32`
   is 8 hex digits.)*
7. **Copy.** The zip goes to `.bunkarr/arr/<slug>-<id>/.partial-job<id>/` with the filecopy
   engine (S7, sha256 compared). Then `manifest.json` is written, the directory is renamed to its
   timestamp, the `snapshots` row (`kind = arr`, method `arr_api_folder` or `arr_api_http`) is
   recorded, and staging is removed.
8. **Prune.** Keep the newest `arrDaily` (default 14, 1–365) `ok` versions, plus the newest `ok`
   version of each of the `arrWeekly` (default 8, 1–520) most recent ISO weeks. The newest `ok`
   version is never deleted.

**The *arr's own manual backups.** The *arrs prune only their scheduled backups; a backup made
through the API is a manual one and stays in `Backups/manual` for good (to confirm with the spike
containers: `BackupService` skips the cleanup for manual backups). Bunkarr never deletes anything
in the *arr (deleting its own manual backups is a D13 question). To keep the *arr's appdata from
growing, the default schedule is weekly, a fresh scheduled backup is reused (step 3), and the
Connect card shows the number and size of manual backups, with a warning above 10.

Fault points:
`arrbackup.afterStage|afterCopy|beforeRename|afterRename|beforeRecord|afterRecord|pruneAfterTrash|pruneAfterUnrecord|pruneAfterRemove`,
and *(as built)* `arrbackup.afterCommand` (after the Backup command and the item that records its
id, before the *arr has made the backup).

*(As built, review fixes.)* A resumed job whose recorded command the *arr reports `orphaned`,
`aborted` or `cancelled` (a restart of the *arr marks every started command orphaned) asks for a
new backup, and does not reuse a manual backup listed since the job was queued: a killed backup
can leave a half-written zip in the Backups folder. A `failed` command fails the resumed job as it
would the first attempt. A version outside the integration's current folder is recorded only when
its manifest names this running job or an `arr_backup` job of this integration in this database;
one whose job row was already pruned from the history is left alone with a warning.

## 11. Manifests (`internal/manifest`)

### 11.1 Content (format 1)
`manifest.json` holds one object:
- `format` (`"bunkarr-manifest"`), `formatVersion` (1), `createdAt` and
  `generator {name, version}`.
- `scope`: `{kind:"destination", destinationId, destinationName}` or `{kind:"export"}`. The export
  scope covers every enabled source.
- `job {id, queuedAt}` (destination versions only).
- `integrations`: `[{id, type, name, appVersion, refreshedAt, status, fresh, lastError,
  qualityProfiles, metadataProfiles, rootFolders, tags}]`.
- `sources`: `[{id, name, destFolder}]`.
- `items`: one object for **every** non-deleted item of every enabled *arr integration, in every
  manifest, whether it has files or not and whether it locates into a source or not. The *arr
  state is the disaster record, whatever the destination and the rules:
  - `{integrationId, kind, arrId, title, year, externalIds, path, located, rootFolder,
    qualityProfile (name), metadataProfile, monitored, tags (labels), genres, added, detail,
    files, extraFiles, skippedFiles}`;
  - `located` is false when the item's folder maps into no source; its files then have
    `source: null`;
  - `detail`: Sonarr `seriesType, seasonFolder, monitorNewItems, useSceneNumbering,
    languageProfileId, seasons[{seasonNumber, monitored}], episodes[{season, episode,
    monitored}]`; Radarr `minimumAvailability`; Lidarr `albums[{id, mbid, title, monitored}]`;
  - `files`: every *arr file of the item, `{arrFileId, path, relativePath, size, quality,
    dateAdded, episodes [{season, episode}] | albumId, source {id, relPath} | null, tier, rule
    {id, name}, kept, backedUp, sha256}`;
  - `extraFiles`: catalog files attributed to the item (sidecars, extras; §8.3), `{source, relPath,
    size, tier, kept, backedUp}`. A `skip`-tier extra without a live record is left out;
  - `skippedFiles`: the number of extras left out (D9).
- `otherFiles`: catalog files of the scope's sources that belong to no item, in the shape of
  `extraFiles`, with the same rule.
- `summary`: `{items, unlocatedItems, files, bytes, tiers:{full, manifest, skip}, keptFiles,
  backedUpBytes, leftOut}`.

*(As built.)*
- Lidarr artists carry `monitorNewItems` in `detail` too (the index, the manifest and the test
  decoder all have it). Sonarr series carry `externalIds.tvmaze` (review fix); the CSV has no
  tvmaze column.
- Every live record at the destination is listed on its own (review fix). A record whose source
  was deleted, or a second record of one source file, is listed with `source: null` and `relPath`
  set to its path at the destination (unique among live records), so two deleted sources that
  both held `Avatar (2009)/Avatar (2009).mkv` are two files, each where a restore finds it.
- A linked record without its own hash shows its primary's; the CSV names integrations and
  sources rather than giving their ids.

In a destination version:
- For files in the destination's linked sources, `tier`, `kept` and `backedUp` are that
  destination's. Files elsewhere have `tier: null, kept: false, backedUp: false`.
- `backedUp` is true when a live record has the file's size and mtime: a `present` record, or a
  `linked` or `link_recorded` record whose primary is `present` or `linked` with the same size and
  mtime (a primary that verify marked `missing` does not count).
- `sha256` is the record's hash, or null.

In an export (`GET /manifest/export`) every file is listed with `tier`, `kept` and `backedUp`
null.

`manifest.csv` has one row per listed file, plus one row for each item that has no files. Its
header row:

```
integration,kind,arrId,title,year,tmdbId,tvdbId,imdbId,mbid,season,episode,qualityProfile,rootFolder,monitored,tags,arrPath,located,source,relPath,size,quality,tier,kept,backedUp
```

- Quoting follows RFC 4180, and `tags` are joined with `;`.
- A multi-episode file is one row: `season` holds its season number and `episode` the episode
  numbers joined with `;` (`4;5`).
- A cell that starts with `=`, `+`, `-`, `@`, a tab or a CR gets a leading `'` (spreadsheet
  safety). Because of that, the CSV is informational and the JSON is canonical.

`SHA256SUMS` holds the lines `<hex>  manifest.json` and `<hex>  manifest.csv`.

`manifest.Parse(r)` validates the format and version and returns `*Manifest`; `ParseDir` also
checks `SHA256SUMS`. `(*Manifest).ReimportPlan()` returns, for each item, what a restore would
send to a fresh *arr: kind, external ids, title and year, root folder, quality profile name,
metadata profile, monitored, tag labels, the `detail` fields above, and each *arr file's relative
path and size. `manifest.ComparePlan(plan, live)` compares a plan with a live state: tag labels
and profile names case-insensitively, lists order-insensitively, exactly the field set above, and
it reports every difference. Acceptance 3 uses it. A tool that actually re-imports is deferred.

### 11.2 Job `manifest_export` and download
1. **Preflight.** The destination is enabled and S3 applies. `.partial-job*` recovery runs. The
   lock keys are `manifest:<destinationId>` and the shared `manifest-build`, so manifest jobs
   of all destinations take turns in the queue and a waiting one holds no worker.
2. **Build.** The manifest is built from the catalog, `mediaindex`, the tier evaluator and the
   records **in one read transaction**, so every table is read from one WAL snapshot. A file
   renamed by a concurrent targeted scan therefore appears exactly once. Each owner exposes its
   manifest queries on a `db.Queryer`, which `*sql.Tx` satisfies. *(As built: catalog, syncer and
   destinations expose no such queries yet, so `internal/manifest/queries.go` reads their tables
   directly, read-only and in that one file; sources and integrations are listed just before the
   transaction starts. Only one manifest is built at a time, by jobs and on-the-spot exports
   together (`Options.MaxBuilds`, default 1): a build holds the library in memory and a read
   connection.)*
3. **Unchanged content.** The `content_hash` is computed over the canonical JSON without
   `createdAt`, `job`, and each integration's `refreshedAt`, `appVersion` and `lastError`
   (`status` and `fresh` stay in, so a cache going stale is a change). If it equals the newest
   `ok` version's, the runner reads that version's `manifest.json` and `SHA256SUMS` through the
   job's root and compares them with `manifests.checksum`:
   - they match: no version is written, the stat `unchanged` is true;
   - they are missing or differ: that row is marked `damaged`, a warning is logged, and a new
     version is written.
4. **Dry run.** The counts and `unchanged` only.
5. **Write the version.** `manifest.json`, then `manifest.csv`, then `SHA256SUMS` go into
   `.partial-job<id>/` through the job's root with `WriteFileAtomic`. The directory is then
   renamed and the `manifests` row inserted (fault points
   `manifest.afterWrite|beforeRename|afterRename|beforeRecord`). *(As built, review fix:
   `manifest.json` and `manifest.csv` are streamed, one list element at a time, byte for byte
   what `encoding/json` writes, so a large library's manifest is never held in memory; each file
   goes through a temp file, fsync and a no-replace rename, the steps of `WriteFileAtomic`. A
   counting pass sizes them first for the free-space check.)*
6. **Prune.** Keep the newest version of each of the last `manifestDays` days (default 30,
   1–3650), plus the newest of each of the last `manifestWeeks` ISO weeks (default 12, 0–520).
   The newest `ok` version is always kept. Only recorded directories are pruned
   (`manifest.pruneAfterRemove`). *(As built: a day or week counts only when it has a version; a
   damaged version is kept 7 days; a missing or out-of-range value takes its default, but a
   stored `manifestWeeks: 0` means no weekly versions.)*
7. **Warnings.** An integration that is not fresh, or `unlocatedItems > 0`, ends the job
   `completed_with_warnings` (notified as §12.5 says).

It runs after full syncs (§9.2). There is no default schedule; one can be added from System →
Tasks. *(As built: §13 has no endpoint that creates a schedule, so a manifest schedule cannot be
added yet; DEFERRED.md. "Export now" and the follow-up after full syncs are the ways to run it.)*

**Downloads.**
- `GET /manifests/{id}/download?format=json|csv` reads the file through `destinations.Open`
  (S3), verifies it against the recorded checksum and `SHA256SUMS`, and streams it. It answers
  409 if the destination is not mounted, or if the file is damaged ("manifest damaged: checksum
  mismatch"); a damaged file also marks the row `damaged`.
- `GET /manifest/export?format=json|csv[&destinationId=]` builds a manifest on the spot, in one
  read transaction, into `<config>/staging/manifest-export-<rand>/` (0700). It computes the file's
  sha256, then serves it with `Content-Length`, `X-Bunkarr-SHA256` and `Content-Disposition:
  attachment`, and removes the staging copy. A build that fails answers 500 before any byte is
  sent. At most 2 exports run at a time (429 beyond). With `destinationId` it is that
  destination's view, otherwise the export scope. *(As built: the build streams straight into
  the staging file and waits for the build slot; a failed build answers 500 with the usual JSON
  error and no manifest bytes. The UI fetches a download before saving it, so an error answer
  (409, 429, 500) is shown rather than saved as the manifest.)*

## 12. Jobs

### 12.1 Types, params, validation, lock keys

| Type | Params (required / optional) | Lock key | Notes |
|---|---|---|---|
| `sync` | `destinationId`; opt `sourceIds`, `allowChanges`, `paths` (exactly one source id; each path satisfies `jobs.ValidTargetPath`; ≤ 1000), `releaseDemoted` (not with `paths`; a real run also needs `releaseOf` and `releaseRevision`) | `dest:<id>` (+ `source:<id>` while scanning) | Phase 1, plus §8.5 and §9 |
| `refresh` | `integrationId`; opt `arrItemIds` (only for *arr integrations, checked at run time; ≤ 500), `syncAfter` (with `arrItemIds`, or untargeted after an overflow merge, §6.1), `allowChanges` (applies what the refresh guard held) | `integration:<id>` | §6; runs in the refresh pool (2 slots, not counted against `jobs.workers`) |
| `arr_backup` | `integrationId`; opt `destinationId` | `arrbackup:<integrationId>` | §10 |
| `manifest_export` | `destinationId` | `manifest:<destinationId>` + `manifest-build` (shared by every manifest job: one build at a time, a waiting job stays queued) | §11 |

- **Paths.** `jobs.ValidTargetPath(p)`: a clean (`path.Clean(p) == p`), relative,
  slash-separated path that is not `""` or `.`, has no `..` element and no NUL. Names such as
  `.hack SIGN (2002)` or `Movie..Name` are valid. The source's `os.Root` is the real fence (S1).
- **Canonical params.** `paths` are sorted and de-duplicated, and a path inside another listed
  path is dropped. `arrItemIds` are sorted and de-duplicated.
- **Release.** A real `sync` with `releaseDemoted` is refused with 409 "rules changed since the
  preview" unless `releaseOf` names a finished dry-run sync of the same destination with
  `releaseDemoted`, and its `stats.tierRevision`, `releaseRevision` and the current
  `tiers.revision` are equal. A dry run with `releaseDemoted` needs neither field.
- **`jobs.integration_id`** is denormalized from `params.integrationId` for filtering
  (`GET /jobs?integrationId=`).
- **The scheduler's skip rule** (phase1.md §6.2) is amended. A fire of a sync or verify is skipped
  only while a non-dry-run job of that type and destination runs **without** `paths`
  (`json_extract(params, '$.paths') IS NULL`). A running targeted job never causes a scheduled
  untargeted job to be skipped: the scheduled job is queued and waits for `dest:<id>`. Other types
  keep "the same canonical params", so a running targeted refresh never skips a scheduled full
  one.
- **Enqueue validation** rejects an unknown type, a missing id or an invalid path with a
  `ValidationError`, exactly as in Phase 1. *(As built: it also refuses `paths` and the release
  params on jobs other than `sync`, `arrItemIds` and `syncAfter` on jobs other than `refresh`,
  and `syncAfter` without `arrItemIds`, schedules included.)*
- *(As built, review fix.)* The skip rule also needs the running job to **cover** the scheduled
  one's sources: its `sourceIds` is empty (all sources) or holds every source the schedule names.
  A running untargeted follow-up of one source (`{destinationId, sourceIds:[s]}`) does not make
  the nightly all-source sync skip its turn.

### 12.2 Coalescing (`jobqueue.Enqueue`, one transaction with the dedupe)

These rules apply only to targeted specs that are not dry runs, and only against queued jobs
that are not dry runs either. Dry runs keep Phase 1's exact dedupe.

| Case | Result |
|---|---|
| a sync with `paths`, and a queued (not running) sync of the same destination with no `paths` whose `sourceIds` is empty or contains the source | returns that job |
| a refresh with `arrItemIds` and no `syncAfter`, and a queued refresh of the same integration with no `arrItemIds` | returns that job |
| a refresh with `arrItemIds` and `syncAfter`, and a queued refresh of the same integration with no `arrItemIds` and with `syncAfter` | returns that job |
| a queued targeted job of the same type and equal other params (`destinationId`, `sourceIds`, `allowChanges`; or `integrationId`, `syncAfter`) | the paths or ids are merged into it (its `queued_at`, and so its place in the queue, stays) and the merged job is returned |
| a merge that would exceed `jobs.MaxTargetPaths` or `jobs.MaxTargetItems` | the queued job becomes untargeted and keeps `syncAfter` (a refresh then queues untargeted syncs per source, §6.1); it logs this when it starts |

- A queued refresh without `syncAfter` never absorbs a spec with `syncAfter`, so a scheduled
  full refresh waiting for a slot cannot swallow a webhook's follow-up sync.
- A running job is never merged into.
- The returned job's `queued_at` tells the caller whether it existed before the call (§7.3).
- Overflow load is bounded by the coalescing itself: at most one untargeted `syncAfter` refresh
  of an integration is queued at a time, and it absorbs every later targeted `syncAfter` spec.
- *(As built.)*
  - A queued untargeted job covers a targeted spec only when it has `allowChanges` whenever the
    spec does, so "Apply held changes" is never answered by a job that would hold them again.
  - Coalescing skips queued jobs that already started once (re-queued after a crash or a
    shutdown): their plan may be complete, and merged targets would be lost.
  - The overflow note goes into the job's log when the merge happens, not when it starts.
  - *(Review fix.)* The exact dedupe of a spec that is not a dry run never returns a re-queued
    `sync`, `verify` or `retention` job whose plan is already complete (`planned_at` set): such a
    job resumes that plan without scanning, so a new import in its folder would be lost. The
    request gets a new job. `plexdb_backup` and `arr_backup` keep the exact dedupe: their resume
    starts over or follows the recorded Backup command.

### 12.3 Default schedules
Every schedule is created with its integration or setting and edited on its page and in System →
Tasks. The migration seeds none, and the start-up refresh (§6.1) covers only *arr integrations,
so an upgraded Phase 1 install runs exactly the jobs it ran before.

| Schedule | Default |
|---|---|
| *arr refresh | `15 */6 * * *` |
| *arr backup | `30 6 * * 0` (weekly) when enabled |
| Plex index | `0 1 * * *` when enabled |
| Tautulli | `0 2 * * *` |
| Seerr | `30 2 * * *` |
| Maintainerr | `45 */6 * * *` |

### 12.4 Stats
- **`refresh`:** `{integrationId, integrationType, targeted, items, itemsAdded, itemsUpdated,
  itemsDeleted, files, filesMapped, filesUnmapped, filesMismatched, filesDeleted,
  unmappedFolders:[…≤20], changedItems, inaccessibleRootFolders:[…], recycleBin, requests,
  guardHeld, followUpJobs:[ids], durationMs}`.
- **`arr_backup`:** `{dryRun, method, backupName, backupType, reusedScheduled, unchanged, bytes,
  integrity, snapshotId, versionsPruned, durationMs}`.
- **`manifest_export`:** `{dryRun, items, unlocatedItems, files, bytes, staleIntegrations,
  unchanged, damagedFound, manifestId, versionsPruned, durationMs}`.
- **`sync`:** the Phase 1 stats plus `targeted`, `paths`, `expectedMissing`, and the tier stats of
  §8.5.
- *(As built.)* `manifest_export` adds `path` and `recovered`; `sync` adds `retainsDeferred`
  (D14), `manifestExportJob` (§9.2) and `skipped` ("not mounted; skipped"). The Job detail page
  does not show the `arr_backup` stats yet.

### 12.5 Notifications
- `refresh` and `manifest_export` notify only on failure or warnings, at most once per
  integration or destination per 24 h while that lasts.
- Syncs with trigger `webhook`, or queued by a refresh, never send `onSuccess`; their warnings and
  failures notify at most once per destination per 24 h. Held changes (S10) always notify, as in
  Phase 1.
- `arr_backup` behaves like `plexdb_backup`.
- Titles: "Refresh of <integration>", "Backup of <App> <name>", "Manifest for <destination>".
- *(As built, `internal/notify`, review fixes.)*
  - Warnings and failures are limited separately, per key: `refresh:<integrationId>`,
    `manifest:<destinationId>`, and per destination for follow-up syncs (trigger `webhook`, or a
    sync with `paths` or `sourceIds`, which only a refresh sets). A failure is never held back by a
    warning.
  - A clean (completed, not dry-run) job ends a problem only when it covered what the notifying
    job covered: a full refresh ends a targeted refresh's problem, never the reverse; a sync of
    source A never ends source B's.
  - A slot is given back when the message reached nobody (no subscribed target, the send failed,
    the queue was full). A gap of 24 h or more either way (a clock set back) starts a new window.
  - The limits live in memory: after a restart one more notification can go out. A refresh whose
    guard held changes (`guardHeld`) stays inside the limit; only syncs' held changes always
    notify.
  - Fallback titles when a job has no description: "Refresh", "Backup", "Manifest export".

## 13. API (additions; all under `/api/v1`; Phase 1's general rules apply)

**Integrations**
- `POST /integrations/test` `{type, url, apiKey?, id?, settings?, plexSignIn?}` (§4.1) now tests
  every type:
  - it returns `{ok, message, version?, appName?}` plus the type's own fields;
  - Plex adds the Phase 1 fields;
  - *arr adds:
    - `backup: {folder: "ok"|"missing"|"unreadable"|"not-set", http:
      "ok"|"login-required"|"unknown"}`, found by a 1-byte range `GET` of the newest backup
      (`unknown` when none exists);
    - `manualBackups: {count, bytes}`;
    - `rootFolders: [{path, accessible, localPath, sourceId, exists, reason}]`, computed with the
      body's `pathMappings`;
    - `recycleBin: {path, sourceId, relPath, excluded} | null` and `fileDate`;
  - Tautulli and Maintainerr add `plexMatches` (null without a `plexIntegrationId`).
- Create, update and test accept `plexSignIn` (§5). Create, and an update of the URL, key or
  mappings, queue a full refresh (§4.1).
- `GET /integrations/{id}/index` → `{status, refreshedAt, attemptedAt, error, appVersion, stats,
  fresh, instanceMatches, staleAfterHours}`.
- `POST /integrations/{id}/refresh` `{dryRun?, allowChanges?}` → 202 `Job`.
- `POST /integrations/{id}/arr/backup` `{destinationId?, dryRun}` → 202 `Job`.
- `GET /integrations/{id}/arr/rootfolders` → `[{id, path, accessible, localPath, sourceId,
  exists}]`, fetched live with `GET rootfolder` (502 on upstream failure) and mapped through the
  stored mappings. This serves "Import from Radarr/Sonarr", which works like Import from Plex.
- `GET /integrations/{id}/arr/metadata` → `{qualityProfiles:[{id,name}],
  metadataProfiles:[{id,name}], rootFolders:[{id,path}], tags:[{id,label}], refreshedAt}`, read
  from the index.
- `GET /integrations/{id}/seerr/users` → `[{id, label}]`, fetched live (502 on upstream failure),
  labels as in §4.5.
- `GET /integrations/{id}/webhook` → `{path, genericPath, hasKey, lastEventAt, lastTestAt,
  last24h, warnings:[…], recent:[WebhookEvent]}`. The warnings: a linked destination without a
  sync schedule; another integration of the same app that received events on the generic route
  in the last 7 days (it may have the wrong key).
- `POST /integrations/{id}/webhook/key` `{rotate: bool}` → `{key}`. `rotate: false` reveals the
  current key, `rotate: true` replaces it (the old key stops working at once). This is the only
  response that contains a webhook key.
- `GET /catalog/unmapped?integrationId&page&pageSize` → the *arr or Plex files that do not apply
  to a catalog file, paged `{path, localPath?, reason: unmapped|no-source|mismatched}`, for fixing
  mappings.

**Plex sign-in** (no response contains a token; the PIN code only inside `authUrl`)
- `POST /plex/signin` (empty body) → 201 `{id, authUrl, expiresAt}`; 429 when 8 are live; 502
  when plex.tv fails.
- `GET /plex/signin/{id}` → `{status: pending|authenticated|expired, expiresAt, username?,
  warning?}`. An unknown id or another principal gets 404.
- `GET /plex/signin/{id}/servers` → `[PlexServerChoice]`, owned first; 409 while pending.
- `POST /plex/signin/{id}/servers/{serverId}/test` → `{recommended, results:[ProbeResult]}`.
- `DELETE /plex/signin/{id}` → 204 (idempotent).
- `PlexServerChoice = {id, name, owned, productVersion, platform, hasAccessToken,
  connections:[{uri, protocol, address, port, local, relay, ipv6}]}`.
- The sign-in id must match `^[0-9a-f]{32}$`.

**Webhooks**
- `POST /webhook/{app}` and `POST /webhook/{app}/{integrationId}`: §7.1. The webhook key goes in
  the Basic password, `X-Api-Key` or `?apikey=`.
- `GET /webhooks/events?integrationId&eventType&outcome&page&pageSize` → paged `WebhookEvent =
  {id, integrationId, source, eventType, class, receivedAt, processedAt, outcome, jobId,
  truncated, summary:{title, itemIds, files}}`. `GET /webhooks/events/{id}` adds `payload`.

**Tiers**
- `GET /tiers/rules` → `{revision, rules:[TierRule]}`, where `TierRule = {id, priority, name,
  enabled, match, conditions:[{field, op, value}], action, destinationIds: null|[id], createdAt,
  updatedAt}`.
- `PUT /tiers/rules` `{revision, rules:[{id?, name, enabled, match, conditions, action,
  destinationIds}]}` → 200 `{revision, rules, warnings}`. The array order is the priority. A stale
  revision gets 409.
- `GET /tiers/presets` → `[{id, name, description, rules}]`.
- `GET /tiers/fields` → `[{field, label, ops, valueType, source, available, reason?,
  suggestions:[{value, label}]}]`. The suggestions are tags, profiles, root folders, sections,
  sources and Seerr users (labels fetched live), so the editor needs no other call. A field whose
  provider is not implemented yet reports `available: false` (D16).
- `POST /tiers/preview` `{rules?, destinationIds?}` → `TierPreview = {id, revision|"draft",
  createdAt, unknownSources:[{integrationId, name, reason}], staleReferences:[{ruleIndex,
  conditionIndex, message}], destinations:[{destinationId, name, stored, full, manifest, skip,
  unknownPromoted, toCopy, kept, movedToNonFull, byRule:[{ruleId, name, action, files, bytes}],
  configBackups:[{kind, integrationId, name, lastBytes}]}]`. Each count is `{files, bytes}`, and
  `full` adds `uniqueBytes`.
- `GET /tiers/preview/{id}/items?destinationId&tier&ruleId&state&search&page&pageSize` → paged
  `{fileId, sourceId, sourceName, relPath, size, tier, ruleId, ruleName, reasons, unknown,
  unknownPromoted, state: stored|to-copy|kept|not-copied}`. An expired preview gets 404.
- `GET /tiers/flags` → `[ItemFlag]` with `resolved` and `reason`.
- `POST /tiers/flags` `{flag, target:{integrationId, kind, arrId}|{sourceId, relPath}, note}` →
  201. `DELETE /tiers/flags/{id}` → 204.

**Catalog, destinations, manifests, jobs**
- `GET /catalog/files/{id}` → `FileDetail = {file, source:{id, name}, facts:{arr?, plex?, watch?,
  requests?, maintainerr?, flags, unknown:[{source, reason}]}, tiers:[{destinationId,
  destinationName, tier, ruleId, ruleName, reasons, unknownPromoted, record?:{state, relPath,
  size, copiedAt, verifiedAt}}]}`.
- `POST /destinations/{id}/sync` accepts `releaseDemoted`, `releaseOf` and `releaseRevision`.
- `Destination.settings` gains `syncOnArrChange` (bool, default true) and `manifest: {afterSync:
  "auto"|"on"|"off"}` (default `auto`).
- `Destination.capabilities` gains `enforcesModes` (the probe creates a file with mode 0600 and
  reads the mode back).
- `Destination.retention` gains `arrDaily`, `arrWeekly`, `manifestDays` and `manifestWeeks`,
  with the defaults and ranges of §10 and §11.
- `Snapshot` gains `kind`, and `GET /destinations/{id}/snapshots` lists both kinds.
- `POST /destinations/{id}/manifest` `{dryRun?}` → 202 `Job`.
- `GET /destinations/{id}/manifests` → `[Manifest = {id, destinationId, jobId, createdAt, path,
  format, itemCount, fileCount, bytes, checksum, integrity}]`.
- `GET /manifests/{id}/download` and `GET /manifest/export`: §11.2.
- `GET /jobs` accepts the new types and `integrationId`. `GET /jobs/{id}/items` accepts `tier` and
  `ruleId` filters, and `/items/summary` gains a `tier` dimension (both read `detail.tier`). `GET
  /schedules` lists the new types with descriptions.

*(As built, Phase 2.)* The 22 Phase 2 routes are in `openapi.json`: the integration index,
refresh, `arr/backup`, `arr/metadata`, `arr/rootfolders`, `webhook` and `webhook/key` routes; the
five sign-in routes; the two webhook intake routes and the two event routes; the four manifest
routes; `GET /catalog/unmapped`; and one route §13 does not list, `GET /integrations/{id}/arr/snapshots`
(the *arr backup versions of the integration). Differences from the list above:
- `POST /integrations/{id}/refresh` refreshes only *arr integrations; the other types answer 400
  "not available yet" until Phase 3.
- `GET /catalog/unmapped` rows also carry `integrationId`, `size` and `catalogSize`.
- `Destination.retention` has `arrDaily`, `arrWeekly`, `manifestDays` and `manifestWeeks` (a sent
  `manifestWeeks: 0` is kept: no weekly versions). `Destination.settings` does **not** store
  `syncOnArrChange` or `manifest.afterSync` yet: every destination behaves as `true` and `auto`.
- `Snapshot.kind` is required in `openapi.json` (optional in the web types).
- The Phase 3 routes (tiers, `seerr/users`, `GET /catalog/files/{id}`, the `tier`/`ruleId` job
  item filters and the release params) are not built.

Every route is added to `openapi.json` (`TestOpenAPIMatchesRoutes`). The raw-response tests of
Phase 1 are extended. No response contains:
- any API key, sign-in token or the *arr's `config.xml` key;
- a webhook key, except in `POST /integrations/{id}/webhook/key`;
- the PIN code, except inside `authUrl` of `POST /plex/signin`; the PIN id anywhere;
- a Seerr e-mail (the fake Seerr has a user with only an e-mail).

## 14. Data model and packages

### 14.1 Schema (`0003_phase2_3.sql`)

Rebuilds (the method is explained in the file header):
- `jobs`, `job_items`, `job_logs` and `snapshots`: `jobs.type` and `schedules.job_type` gain
  `refresh`, `arr_backup` and `manifest_export`. `jobs` gains `integration_id`, backfilled from
  params. `snapshots.kind` gains `arr`.
- `destination_files.reason` gains `released`.

Other changes to existing tables:
- `catalog_files` drops `media_ids`, `arr_item_id` and `tier` (D5).
- `integrations` gains `webhook_key` (sealed, S8).

New tables:
- `index_state` (with `instance_id`), `arr_items`, `arr_files` (with `local_path`), `arr_meta`,
  `plex_sections`, `plex_items` (with `item_index`, `parent_index`; no genres, D16), `plex_files`
  (with `local_path`), `watch_stats`, `seerr_requests`, `maintainerr_items` (scoped to its Plex
  integration and library, with a `state`) (§6);
- `tier_rules`, `item_flags` (`kind` arr or path, external ids and the last folder; the
  integration reference is `ON DELETE SET NULL`) (§8);
- `webhook_events` (`class`, `targets`, `truncated`, payload ≤ 64 KiB, checked outcomes) (§7);
- `manifests` (with `integrity`) (§11).

New setting keys: `plex.clientIdentifier` and `tiers.revision`.

**Pre-migration copy.** Before it applies any pending migration, `internal/db` writes
`VACUUM INTO '<db dir>/backups/bunkarr-v<old version>-<utc>.db'` (0600; the newest 3 are kept)
and refuses to migrate when that copy fails. 0003 rebuilds tables and a Phase 1 binary refuses a
newer schema, so this copy is the only way back. The CHANGELOG and README describe the downgrade:
stop, restore that file, run the old image. *(As built: a brand-new database (schema version 0)
gets no copy. The copy just written is always kept, and the others are pruned to the newest 2 by
the time in their names (review fix: a clock behind older copies deleted the new one). Copies
named with a future time still outlive a real-clock one until the clock passes them; DEFERRED.md.)*

*(As built.)* 0003 also creates `arr_files_path ON arr_files (path, id)` (review fix): the
unmapped listing pages its mismatched files by a keyset over it (`INDEXED BY`), so each chunk is a
range scan instead of a scan and sort of the whole table.

`internal/db` `TestMigration0003KeepsPhase1Data` upgrades a populated Phase 1 database. It
checks that no row is lost and that no foreign key action ran (no cascade, no SET NULL). It also
checks that links whose primary has a higher id survive, that references point at the renamed
tables, that cascades and RESTRICT still work, that the widened constraints accept the new values
and that the indexes exist. It passes, and so does the whole Phase 1 suite
(`go test -short ./internal/...`) on the migrated schema.

### 14.2 Ownership

Each package owns its tables' SQL, as in Phase 1. Dependencies point one way:
- `tiers` → `mediaindex`, `catalog`, `integrations`;
- `syncer` → `tiers`;
- `manifest` → `tiers`, `mediaindex`, `catalog`, `syncer` (records), `destinations`;
- `webhooks` → `integrations`, `jobs`;
- every outbound client → `netguard`.

The tier preview gets record state through a function `api.App` wires from `syncer.Store`, so
`tiers` never imports `syncer`. The manifest queries of `catalog`, `mediaindex` and `syncer` take a
`db.Queryer`, so `manifest` runs them all in one read transaction.

| Package | Owns | Exposes (minimum) |
|---|---|---|
| `internal/netguard` | — | Ported from Dupearr: `CheckDialAddress`, `NewDialer`, `Proxy`, `Blocked` |
| `internal/testhooks` | — | Overrides read from `BUNKARR_TEST_*` only in `-tags e2e` builds: the webhook windows (quiet, cap, delete delay, upgrade hold), the plex.tv URLs, a freshness clock skew. Production builds return the constants |
| `internal/integrations` | `integrations` | + `ArrSettings`, `TautulliSettings`, `SeerrSettings`, `MaintainerrSettings`, `PlexSettings.Index`, generic `MapPath`; per-type validation; webhook keys (generate, rotate, reveal, the key map for `webhookAuth`); start-up normalization |
| `internal/integrations/plex` | — | + `AllItems`; `CreatePin`, `CheckPin`, `User`, `AuthURL`, `Resources`; `Candidates`, `ProbeConnections`, `RankResults`; `plextest` + fake plex.tv |
| `internal/integrations/arr` | — | `Client` (the §4.3 requests only), DTOs, `ErrWrongApp`; `arrtest` fake with its route table |
| `internal/integrations/tautulli`, `…/seerr`, `…/maintainerr` | — | Allow-listed clients (§4.4, §4.5) and fakes that serve recorded fixtures |
| `internal/mediaindex` | `index_state`, `arr_*`, `plex_sections`, `plex_items`, `plex_files`, `watch_stats`, `seerr_requests`, `maintainerr_items` | runner `refresh`; `Store` (fact queries per source and per file, metadata for the API, freshness, unmapped listing, manifest queries) |
| `internal/webhooks` | `webhook_events` | `Parse(app, body)` (streaming), `Store`, `Processor` (`Start`, `Stop`, `Notify`, the cancel hook), intake handler (auth is done by `api`) |
| `internal/tiers` | `tier_rules`, `item_flags` (+ `tiers.revision`) | `Store`, `Validate`, `Evaluator`, `LoadFacts`, `FactsFor`, `Presets`, `Fields`, `Preview`, `MoveFlagsTx`, `Covered` (for expiry) |
| `internal/snapshots` | `snapshots` (moved from plexdb) | `Store`, `Keep(versions, daily, weekly, loc)` |
| `internal/plexdb` | — | Unchanged, except that it uses `internal/snapshots` and exports `QuickCheck(ctx, path)` for arrbackup |
| `internal/arrbackup` | — (snapshots of kind `arr`) | runner `arr_backup`, `VerifyZip` |
| `internal/manifest` | `manifests` | `Build`, `WriteJSON`, `WriteCSV`, `Parse`, `ParseDir`, `ReimportPlan`, `ComparePlan`, runner `manifest_export` |
| `internal/catalog` | `sources`, `catalog_files` | + `Scanner.ScanPaths`, `Store.Locate`; default excludes (§9.3) |
| `internal/syncer` | `destination_files` | + `Options.Tiers`, `Options.Enqueuer`; targeted plans, kept/release, S6 amendment, D14, expected files, flag moves, the expiry hold |
| `internal/destinations` | `destinations`, `destination_sources` | + the `enforcesModes` probe |
| `internal/jobs` | — | + types, params, limits and `ValidTargetPath` (done in this change) |
| `internal/jobqueue` | `jobs`, `job_items`, `job_logs`, `schedules` | + validation, lock keys, coalescing, the refresh pool, the amended skip rule, `integration_id` |
| `internal/db` | migrations | + the pre-migration copy |
| `internal/api` + `cmd/bunkarr` | — | routes (§13), sign-in registry, `webhookAuth`, COOP, wiring (processor start/stop, start-up refreshes, client id) |
| `web/` | — | §16 |

*(As built.)*
- There is no `db.Queryer`: `mediaindex.Queryer` is defined locally with the same method set, and
  `catalog.LiveFilesAt` takes an unexported interface of the same kind.
- `internal/manifest/queries.go` reads the catalog, syncer and destinations tables itself
  (§11.2); moving those queries to their owners is left to do.
- `jobqueue` exports `ManifestBuildKey` (`manifest-build`, §12.1) and `JobQuery.IntegrationID`;
  `arrbackup` exports `OnJobFinish` (the job manager's hook that removes a failed job's staging)
  and `SweepStaging` (start-up); `snapshots` exports `Layout` with the version-directory helpers.
- `PlexSettings.MapPath` still has its own body; `TestMapPathIsPlexMapPath` shows it gives the
  same results as the generic `MapPath`.
- The e2e harness builds the binary with `-tags e2e`, so `internal/testhooks` works there; with
  no hook variable set it behaves like a release build. Each value can also come from
  `<VAR>_FILE`, read again on every call.

## 15. Tests (required)

**Unit tests**
- **integrations:** per-type settings (validation, defaults, *arr mappings and `backupFolder`,
  `plexIntegrationId`, `staleAfterHours`); Maintainerr refuses a key; `sources.arr_integration_id`
  must be an *arr; the start-up normalization of a seeded Phase 1 database; webhook key generate,
  rotate and reveal.
- **netguard:** the ported tests; a test resolver maps a plex.direct-style name to
  169.254.169.254 and the dial is refused.
- **arr:** every allowed request against `arrtest` for all three apps; every fixture decodes with
  its quirks; the body caps; no redirects; the key only in the header; errors without key or body;
  `ErrWrongApp`; a backup entry whose `path` points elsewhere: the request log shows only
  `/backup/manual/<name>`.
- **tautulli, seerr, maintainerr** (against recorded fixtures, §18 slice 9):
  - the allow-list: a request outside it fails the test;
  - version gates (Tautulli 2.17 refused, Maintainerr 3.3 refused);
  - the envelope and string numbers;
  - paging: a shrinking total and a short page fail the refresh;
  - the Seerr statuses counted; no e-mail decoded; a user with only an e-mail gets "Seerr user
    #<id>";
  - each pending-deletion clause of Maintainerr, including exclusions of a show and of a season,
    asserted against a hand-written expected list for the recording.
- **webhooks:**
  - `Parse` of every file in `testdata/webhooks` (59): event type, item id, class, the
    ImportComplete form, a multi-episode file, `deletedFiles`; a 3 MiB synthetic Rename is
    accepted and stored truncated with its ids;
  - Test never looks anything up;
  - the auth matrix: Basic, header and query with the webhook key succeed; a wrong or missing key,
    the master API key, a session cookie alone and local mode all get 401; the webhook key gets
    401 on `/api/v1/system/status`; after the master key is regenerated, the webhooks still work;
  - the generic route picks the integration by key, with 0, 1 and 2 integrations;
  - 413, 415, 429 and 503; the failed-auth limiter; an address blocked by 10 failures, then a
    request with the correct key, gets 200;
  - disabled → 409;
  - no header or query stored;
  - the processor: per-item due times (a `SeriesDelete` with `deletedFiles` for A, then a
    `Download` for B 1 s later: B's refresh is queued by t ≈ 6 s and A's by t ≈ 60 s); the upgrade
    pair `MovieFileDelete-upgrade.json` then `Download-upgrade.json` 3 minutes apart (fake clock)
    gives one refresh; re-processing after a restart (a crash at `webhook.afterEnqueue`) merges
    into the same job; a cancelled refresh re-arms its events; pruning by age, rows and bytes.
- **mediaindex:**
  - a full refresh from fixtures: items, files, the episode mapping (including the multi-episode
    file 7, S01E04–E05), metadata, mapped, unmapped and mismatched files, the unlocated
    `/movies-4k` root folder, the recycle bin and `fileDate` warnings;
  - the refresh guards: an empty answer; more than half of the items or of the files vanishing;
    `accessible: false` with `movieFileId: 0` everywhere deletes no file; `allowChanges`;
  - the reconcile: with no webhook, a file changed in `arrtest`, then a full refresh queues a sync
    with `paths = [that folder]`; the first complete refresh queues none;
  - a targeted refresh: 404 with a matching `system/status` → deleted; 404 with a wrong
    `appName` → the job fails and nothing is deleted; old and new folders become the targets;
    follow-up syncs per linked destination with `syncOnArrChange`; a missing located folder →
    warning and an untargeted sync; the follow-up after a failure;
  - an instance change: a URL update makes the cache not fresh, and the next refresh replaces it;
  - the Plex index: paging by returned rows, sections, index and parent index;
  - Tautulli: aggregation by rating key and guid, `keep_history` of sections and users; an empty
    history keeps the old rows (shrink guard);
  - Seerr: page 3 failing keeps the old rows and `refreshed_at`;
  - Maintainerr: one exclusion call answering 500 keeps the old rows; `undecided` members;
  - freshness: one definition; a failed targeted refresh leaves the facts known.
- **tiers:**
  - the evaluator table: every field and op, tri-state, `all`/`any`, and the protective unknown
    rule (S14) in both directions, with `unknownPromoted`;
  - the irreplaceable override; destination scopes (NULL, a list, empty = none, a deleted
    destination);
  - D12: mixed items → unknown; two integrations claim a file → unknown; `arr_integration_id`
    resolves it; the most protective tier of a group;
  - unmanaged files (outside every root folder, all caches fresh) → `arr.*` false; a deleted item
    supplies no facts; the spec preset on a source outside the root folders gives `manifest`;
  - a mismatched size → unknown;
  - sidecars follow their media file; folder attribution of extras;
  - Seerr: an empty `seasons` list; an unknown season; a season outside every request;
  - Tautulli: no row → `never` true, `olderThan` true, `newerThan` false; lower bounds with
    history off;
  - Maintainerr: two Plex servers with colliding rating keys; the 4K library (the HD collection's
    tmdb must not match); a season-level pending row plus a new episode with no Plex item →
    unknown → full;
  - a sign-in that switches the server makes Maintainerr and Tautulli facts unknown until the
    Plex index refreshes;
  - flags: re-added in Radarr with a new id; removed from Radarr with files kept; a folder rename;
    the integration deleted; an id reused by another movie;
  - presets; validation errors with indexes; revision 409; stale-reference warnings (a profile
    renamed in `arrtest`).
- **catalog:** `ScanPaths`:
  - a subtree deleted; a missing parent refused; an empty parent refused; an error keeping rows;
    nothing outside the paths touched;
  - a symlinked ancestor, an excluded ancestor and an aliased destination root each leave the
    catalog as a full scan would;
  - `ESTALE` retried once;
  - hardlink partners re-stat'ed (and on FUSE also hashed);
  - `scan_seq` and stats; `Locate` with overlapping sources; the new default excludes.
- **syncer:**
  - no rules → a plan identical to Phase 1 (acceptance 8: the Phase 1 planner tests run with a
    `Tiers` of "all full");
  - manifest and skip files not copied;
  - kept files: no update, repair or retain; moved on a rename; a full record renamed into a
    manifest-tier folder of the same source → a move, then kept, and no retain;
  - release: reason `released`, counted by the guard, held above the limit; facts that change
    between preview and apply → only the confirmed records are released; a rule reverted after
    planning → nothing released; a revision mismatch → 409; the released retain skips the
    reappeared check;
  - S6 amended: a retain does not wait for a manifest-tier new version;
  - D14: 100 `MovieDelete` events 70 s apart → the targeted syncs retain nothing; the next full
    sync holds the retains above the threshold; an upgrade in the same folder still retains;
  - the spec preset with a stale Radarr and a small destination → `completed_with_warnings`,
    decided-full copies and retains run, the library is not copied;
  - an equal-size, equal-mtime replacement with a new *arr file id → `update`;
  - expected files: a fake source that hides a file for its first two listings;
  - a targeted plan limited to its paths;
  - the dry run's skip items with decisions; the tier stats;
  - the follow-up manifest export after full syncs (also failed ones) when `afterSync` allows it,
    never after targeted ones; with no *arr integration, none;
  - a flagged path's retained record is not expired.
- **jobqueue:** validation and canonical forms of the new params (`ValidTargetPath`: `.hack SIGN`
  and `a..b` accepted, `./x`, `a/../b`, `/x` and `..` refused); lock keys; every coalescing case of
  §12.2, including `TestCoalesceWebhookRefreshKeepsSyncAfter` (a queued full refresh plus a
  webhook refresh → a sync is still queued for the item's folder), an overflow that keeps
  `syncAfter` and queues follow-ups, and "never into a running job"; the refresh pool;
  `TestSchedulerDoesNotSkipFullSyncForRunningTargetedSync`.
- **arrbackup:**
  - the folder and HTTP methods against a fake *arr; 302 → the guidance error;
  - a fresh scheduled backup is copied and no command is sent; one already copied → `unchanged`;
  - zip verification (no `config.xml`, a bad CRC, `quick_check` failing); a zip bomb →
    `integrity: failed` and no file larger than the cap; 10^5 entries refused before extraction;
  - `enforcesModes` false without `acceptInsecureModes` → the job fails;
  - recovery; pruning; a dry run sends no command;
  - the test zip's `config.xml` key appears in no log, job log, `manifest.json` or
    `snapshots.manifest`.
- **manifest:**
  - the JSON and CSV golden files (the formula guard, quoting, the multi-episode row);
  - golden cases: an unlocated item; a stale integration (warning); a kept `skip` file listed; a
    `skip` *arr file listed; a linked record whose primary is missing → `backedUp: false`;
  - `Parse` and `ParseDir` round trip; `ComparePlan`; a checksum mismatch detected; an unchanged
    version not written; a damaged newest version → a new version;
  - pruning math;
  - the export: a fault injected mid-build → 500 and no partial body; the digest header matches.
- **snapshots:** the moved keep-set tests.
- **db:** `TestMigration0003KeepsPhase1Data` (done); the pre-migration copy is readable at the
  old version.
- **Plex sign-in:** everything in the port plan:
  - `plextv_test.go` ported from Dupearr, plus the header set, no redirect, no `accessToken` in
    JSON, `CheckPin` sending the code, `User` decoding only `username`, and an http non-loopback
    base refused;
  - `probe_test.go`: a mismatched machine id never receives `X-Plex-Token`; a metadata address
    refused without a request (netguard); relay last; the derived http candidate never
    recommended; a shared resource yields no local or derived candidates; at most 16 candidates;
  - `plexsignin_test.go`: the full flow; no token in any raw body and the PIN code only inside
    `authUrl`; `TokenFor` returns the server token; a second use → 400; a URL answering as
    another server → 400 with no token sent; `apiKey` together with `plexSignIn` → 400; the owned
    and shared fallbacks; expiry, 404, the other principal, the 9th → 429, the throttle,
    cross-site POST → 403, logs;
  - `app_test.go`: the client id is created once, is not `bunkarr`, and reaches plexdb.
- **api:**
  - `TestOpenAPIMatchesRoutes`; the COOP header;
  - raw responses with no secret of any kind (§13);
  - tiers PUT 409 and warnings; preview paging and expiry; job items filtered by tier;
  - webhook routes outside the session group; the webhook key endpoints.
- **notify:** the §12.5 policy and titles.
- **web** (Vitest, React Testing Library):
  - `plexSignInMachine.test.ts` and `PlexSignIn.test.tsx`, ported from Dupearr against the new
    states (no state holds a token);
  - `isTrustedPlexAuthUrl` accepts only `https://app.plex.tv/auth`;
  - closing the form sends `DELETE /plex/signin/{id}`;
  - the Tiers save 409 offers a reload;
  - "Apply release" sends `releaseDemoted`, `allowChanges`, `releaseOf` and `releaseRevision`;
    "Apply held changes" on a release job copies them;
  - the *arr Connect modal renders the webhook panel from `GET /integrations/{id}/webhook`, and
    "Show key" calls `POST …/webhook/key`.

**Crash matrix** (additions to phase1.md §9)
- New points: `refresh.afterBatch`, `refresh.beforeMarkDeleted`, `webhook.afterStore`,
  `webhook.afterEnqueue`, the `arrbackup.*` points of §10 and the `manifest.*` points of §11,
  each in its package's matrix.
- `TestCrashMatrixTiers` runs a sync whose rule set produces copy (full), skip (manifest), kept
  and release items. It crashes at every syncer and filecopy point and resumes. It then asserts
  the Phase 1 invariants, and also:
  - no file of a non-full tier left the destination except the released ones;
  - every released file is in retention with reason `released`;
  - a further sync plans nothing;
  - a crash after planning, then a resume, with a manifest-tier file in the retain's folder: the
    retain waits with a warning, and the next sync retains.
- `TestCrashMatrixTargeted` does the same for a targeted sync with a rename and an upgrade inside
  the target.

**E2E** (`internal/e2e`, tag `e2e`, the real binary built with `-tags e2e`, no Docker; fakes for
the other apps; the webhook windows shortened through `internal/testhooks`)
- The webhook path with `arrtest` serving the fixtures: POST `Download` → `refresh` → a targeted
  sync done within 60 s; a burst of 200 events → one refresh and one sync per destination.
- `Rename.json` → one `move` item and `bytesCopied = 0`; `MovieFileDelete-manual.json` → no retain
  in the targeted sync, then one retain with reason `deleted` at the next full sync;
  `SeriesDelete-deletedFiles.json` → the refresh is queued no earlier than the delete delay.
- An import while a full refresh of the same integration runs: the targeted sync starts after
  it, and nothing is lost.
- A manifest round trip against the fake *arr state.
- A tier dry run with fake Tautulli, Seerr and Maintainerr, whose reasons equal the pinned table.
- Acceptance 7's stale case: the freshness clock skew makes the Maintainerr cache stale, and X is
  copied.
- The Plex sign-in flow against the fake plex.tv and `plextest` (acceptance 10).

**The pinned table** (acceptance 6; the Radarr fixtures). Rules R1 `arr.tag has bunkarr-full` →
`full` and R2 (no conditions) → `manifest`:
- movie 1, Night of the Living Dead (tag 1 `bunkarr-full`) → `full` by R1;
- movies 2 and 3 (tags `[2]` and `[]`) → `manifest` by R2;
- movie 4 has no file.

Then R0 `maintainerr.pendingDelete is true` → `skip` is inserted first, with movie 3 pending:
movie 3 → `skip` by R0. With the Maintainerr cache stale: movie 3 → `manifest` by R2, with R0
unknown (skip is less protective than manifest, so R0 does not win). The reasons are compared as
structured `Reason` values.

**Docker suite** (`make test-arr`, gated by `BUNKARR_E2E_ARR`; pinned `linuxserver/sonarr` and
`radarr` digests, the spike's versions, with the Bunkarr image on one network; the harness
generates small real MKVs as the spike did, and configures the webhook in each *arr through its
API with the integration's webhook key as the Basic password; it needs internet for the *arr
metadata lookups and skips with a message without it)
1. **Import** (acceptance 1).
   - Radarr: `DownloadedMoviesScan`. Sonarr: `DownloadedEpisodesScan`, into a series whose other
     episodes are already backed up.
   - Assert a `webhook_events` row, and a sync with trigger `webhook` and `paths = [the item
     folder]` finished within 60 s of the import command completing.
   - Assert exactly one item that is not `skip`: a `copy`, status `done`, `relPath` the imported
     file, bytes equal to its size; and that the destination file's sha256 equals the source's.
2. **Upgrade** (acceptance 2). An upgrade to 1080p in both apps gives, in one targeted sync, a
   `copy` done before a `retain` (item ids and done order). The retained file equals the old
   one's hash. Once more with the new file imported by copy from a separate downloads volume. The
   same-path case gives an `update` with the old version kept as `replaced`.
3. **Manifest** (acceptance 3). `manifest_export`, then `ParseDir(<target>/.bunkarr/manifests/
   <version>)`; a download of `/manifests/{id}/download` and of `/manifest/export`, each read with
   `Parse`. The expected state is decoded by a test-local decoder over the raw `GET movie`,
   `GET series`, `episodefile?seriesId=` and `episode?seriesId=` JSON, without the `arr` DTOs.
   `ComparePlan` finds no difference, including a movie under a root folder that no mapping
   covers.
4. **Backup** (acceptance 5). `arr_backup` with Radarr's `/config/Backups` mounted `:ro` → an
   `ok` snapshot. HTTP with Forms login → the guidance error. HTTP with Disabled for Local
   Addresses → `ok`.
5. **Tiers** (acceptances 6 and 7, the fresh cases).
   - A rule "`arr.tag has bunkarr-full` → full; else manifest" plus a quality-profile rule on the
     real Radarr, with the fake Tautulli, Seerr and Maintainerr joined to a fake Plex index. The
     dry run's reasons equal the expected table. A rule is changed, and a new dry run shows the
     new reasons.
   - Maintainerr marks movie X pending. With the skip rule, a real sync does not copy X. With the
     rule disabled, X is copied. (The stale case runs in the E2E suite, which has the clock hook.)

CI runs unit, race, crash, web and e2e on every push. `make test-arr` runs on `workflow_dispatch`
and on tags, like `make test-plex`. The release workflow adds it to the candidate image's Docker
suite.

*(As built, Phase 2.)*
- **E2E:** `TestWebhookPath` (the webhook path, the burst of 200, Rename, `MovieFileDelete-manual`,
  `SeriesDelete-deletedFiles`, an import during a full refresh) and `TestPlexSignInE2E`
  (acceptance 10) pass against the binary built with `-tags e2e`. The tier dry run and
  acceptance 7's stale case wait for Phase 3.
- **Docker suite:** `make test-arr` runs `TestDockerArr` (Radarr and Sonarr: acceptances 1, 2 and
  4), `TestDockerArrLidarr`, `TestDockerArrManifestRoundTrip` (acceptance 3) and, in
  `internal/arrbackup`, `TestDockerArrBackup` (Radarr) and `TestDockerArrBackupLidarr`
  (acceptance 5). The images are pinned by digest (`SONARR_IMAGE`, `RADARR_IMAGE`,
  `LIDARR_IMAGE` override them). The manifest and Lidarr tests run the Bunkarr binary on the host
  rather than the image on the test network. Not covered yet: a Sonarr backup against a real
  container; Lidarr's Rename, Retag and a real `isUpgrade` (their parsing is unit-tested);
  acceptance 10 (only the native e2e suite runs it); item 5 (Phase 3).
- **CI:** the workflows do not run `make test-arr` yet (DEFERRED.md).
- **Timing:** `go test -race ./internal/syncer` needs more than Go's default 10 minutes on a busy
  machine; the harness now copies one migrated template database per test binary, which brings
  the whole race suite to about 450 s.

## 16. UI

- **Settings → Connect** (`/settings/connect`). One card per connection, ported from Dupearr's
  `ConnectionCard` and `ConnectionModal` pattern (`ArrInstanceModal`, `TautulliModal`,
  `WebhookInfo`).
  - **Sonarr, Radarr, Lidarr.**
    - URL and a write-only API key.
    - Path mappings, with a check against the *arr's root folders (from Test, so unsaved mappings
      are checked too) that shows each folder's local path and source, or why it has none, and
      flags an inaccessible root folder.
    - A recycle bin inside a source: a warning with "Exclude /<relPath>/ from <source>" (an
      anchored, directory-only pattern added through the normal source update). A `fileDate`
      other than `none`: a warning.
    - The backup folder, the backup destination and schedule (weekly by default), the manual
      backup count and size (warning above 10), and `acceptInsecureModes` when the destination
      needs it.
    - The refresh schedule and `staleAfterHours`, with "Refresh now".
    - An index status line: fresh or stale, counts, unmapped and mismatched files with a link to
      the list.
    - A **webhook panel**: the URL to paste; the auth to choose, Basic first (user name anything,
      password the webhook key; the *arr keeps that field private), then the `X-Api-Key` header;
      "Show key" and "Regenerate key"; the triggers to tick (On Import, On Upgrade, On Rename, On
      Movie/Series Delete, On File Delete, On Movie Added/Series Add); "last Test received"; the
      warnings of §13; and the recent events.
    - "Import from Radarr/Sonarr" (root folders → sources, setting `arr_integration_id`).
  - **Tautulli.** URL, key and the linked Plex server.
  - **Seerr.** URL, key and optionally the linked Plex server.
  - **Maintainerr.** URL and the linked Plex server, with the notice: "Maintainerr has no API
    authentication; Bunkarr only reads from it."
  - **Apprise**, as in Phase 1.
- **Settings → General.** The API key's help text no longer mentions webhooks.
- **Settings → Plex.**
  - "Sign in with Plex": the popup, the countdown, "Signed in as X", then the server picker (owned
    first, Owner/Shared badges). Choosing a server shows its tested connections, with badges
    Local/Remote/Relay, HTTPS/Unencrypted, Derived and Recommended, and each result with its
    latency or error. "Use" takes a working row; a failed or unencrypted row needs "Use anyway".
  - Warnings for a shared server, a missing server token (owned: an account-token checkbox;
    shared: manual entry) and a relay.
  - Then "Or enter the details manually", which behaves exactly as in Phase 1.
  - While a sign-in is chosen the token field reads "Token: from Plex sign-in — <server> (owner)",
    with "Use a token instead". Typing a token clears the sign-in, and closing the form without
    saving sends DELETE.
  - The library index switch and its schedule.
- **Settings → Tiers** (`/settings/tiers`).
  - A fixed first row, "Irreplaceable → full (built-in)", and a fixed last row, "Everything else →
    full (built-in)". An info row says Plex DB and *arr config backups are always full.
  - The ordered rules: up/down, enable, name, a condition builder (field, op and value pickers fed
    by `GET /tiers/fields`; unavailable fields greyed with the reason), `all`/`any`, the action,
    and "Applies at" ("All destinations" or chosen), with the example of §8.1.
  - Presets. **Preview** (the draft) gives, per destination, cards for Stored, Full, Manifest,
    Skip, Unknown (full only because a fact is unknown), To copy, Kept and Moved to a non-full
    location, with files and bytes; the per-rule counts; banners for unknown sources and stale
    references; and a table of files with tier, rule and reasons (filters: destination, tier,
    rule, state, search).
  - Save sends the revision; a 409 offers a reload; warnings are shown next to their conditions.
    After a save that leaves kept files, a banner offers "Release N files (X GiB) at
    <destination>…". That starts a dry run with `releaseDemoted`, opens its job, and "Apply
    release" confirms and starts the real run with `releaseDemoted`, `allowChanges`, `releaseOf`
    (the dry run) and `releaseRevision`.
- **Library.**
  - **Item view** (`/library/files/:id`):
    - the facts table (source, *arr item with its tags, profile, root folder and monitored
      state, Plex section, plays and last watched, requested by, Maintainerr status, flags), with
      unknown facts marked and the reason given;
    - the tier per destination with reasons;
    - the destination records;
    - "Mark irreplaceable": the item by default when there is one, else the path.
  - "Export manifest" (JSON or CSV).
  - The file browser's tier column is deferred (D16).
- **Destinations.**
  - The snapshots dialog lists both kinds, with a kind column and no download for `arr`.
  - A Manifests tab: versions, counts, integrity, Download JSON/CSV and "Export now".
  - The retention settings gain `arrDaily`, `arrWeekly`, `manifestDays` and `manifestWeeks`; the
    settings gain "Sync on *arr changes" and "Manifest after each sync" (auto/on/off).
- **Activity and System.**
  - Job names for the new types.
  - Sync job detail: the tier stats, and for dry runs the skip items grouped by tier with reasons
    (`tier` filter), and for a release preview "Apply release". "Apply held changes" on a release
    job copies its release params.
  - A refresh job lists the follow-up jobs it queued.
  - System → Tasks lists the new schedule types.
- Phone width: every new page follows phase1.md §10 (drawer, no sideways scroll on the new pages).

*(As built, Phase 2.)*
- The webhook panel has a **Bunkarr address** field (this page's origin by default), because the
  *arr may reach Bunkarr by another name; it warns about a loopback or invalid address. The
  panel re-reads its activity every 5 s while open and has "Check again". Copy buttons fall back
  to a hidden text area over plain http, where the clipboard API does not exist.
- A card whose last full refresh was held by the refresh guard says so and offers "Apply held
  changes" (a refresh with `allowChanges`).
- A Test result, and its backup-access and manual-backup rows, is shown only while the URL, key,
  mappings and backup folder are the ones it tested.
- Not built yet: "Import from Radarr/Sonarr"; the destination settings "Sync on *arr changes" and
  "Manifest after each sync"; the `arr_backup` stats on the Job detail page. The retention form
  has the *arr and manifest fields.
- Settings → Plex keeps the Phase 1 error text "Enter the Plex token." for the manual path.
- Settings → General: the API key's help says not to give it to Sonarr, Radarr or Lidarr and
  points to Settings → Connect (review fix: it still told users to put it into their webhooks).

## 17. Docs and deployment notes

- **README.**
  - *arr connections: the API key; path mappings, e.g. Radarr's `/movies` → Bunkarr's
    `/media/movies` when both mount `/mnt/user/data/media`.
  - The backup-folder mount: `/mnt/user/appdata/radarr/Backups:/arr/radarr-backups:ro`; the Forms
    login note (D3); the backups are sensitive (S17) and SMB does not enforce their modes; manual
    backups pile up in the *arr, so prefer its scheduled backups.
  - Webhook setup per app with the integration's webhook key, Basic auth recommended over the
    header and over `?apikey=` (which ends up in proxy logs). Webhooks can be missed, and the full
    refresh catches up. What delays a webhook sync (§7.3).
  - Tiers: the default is full; presets; "kept" and "release"; "unknown is safe", and unknown
    copies are guarded; Tautulli history turned off for a user makes play counts a lower bound.
  - Tautulli ≥ 2.18; Seerr; Maintainerr ≥ 3.4 and its lack of authentication.
  - Sign in with Plex versus URL + token. Outbound plex.tv access only when signing in.
  - Revoking under plex.tv → Authorized Devices ("Bunkarr — Docker"). The plex.direct DNS-rebind
    note. What a shared server cannot do: read `/:/prefs`, so there is no butler-window check.
  - The pre-migration copy and how to downgrade.
- **DEFERRED.md.**
  - Remove the rows for webhooks and for the catalog placeholder columns.
  - Add:
    - storing *arr UI credentials for the backup download (D3);
    - creating the webhook connection inside the *arr (D13);
    - the Maintainerr proxy auth header (D13);
    - deleting the manual backups Bunkarr created in the *arr (D13);
    - sealing *arr backups and `Preferences.xml` with a user passphrase (S17);
    - a manifest re-import tool (§11.1), and with it a Docker test that re-imports into a second,
      empty *arr;
    - incremental Tautulli history;
    - Seerr 4K-specific rules;
    - automatic release of kept files after a hold period (S15: today only explicit);
    - Plex genres for `media.genre`, and the tier column of the file browser (D16);
    - a split lock so a targeted refresh does not wait for a full one (§7.3);
    - `https://<ip>` derived Plex candidates with the plex.direct name as TLS server name;
    - an mtime-only change with identical content updating only the record (after `fileDate` is
      switched on);
    - a manifest check in the verify job.
- **CHANGELOG** (including the Phase 1 behaviour changes: the new default excludes, netguard in
  the Plex and Apprise clients, the amended scheduler skip rule), **ADR 0006** ("Tiers are
  evaluated per destination; demotion keeps; unknown protects") and **`openapi.json`**.
- *(Revision 3.)* The Phase 2 docs are done: README (*arr connections, webhooks, the Backups
  folder, Plex sign-in, manifests, upgrading and downgrading), CHANGELOG, DEFERRED.md,
  CONTRIBUTING.md and the compose example. ADR 0006 records Phase 2's decisions instead
  (`docs/adr/0006-phase2-arr-integration.md`: webhook keys D7, *arr backups D3/D4, webhook
  deletes D14, Plex sign-in D10); the tiers ADR above becomes **ADR 0007** in Phase 3. The README
  items on tiers, Tautulli, Seerr and Maintainerr are Phase 3's.

## 18. Order of work and open questions

Order of work (each slice ends green on lint, race, crash, web and e2e, and is committed on its
own).

**Phase 2**

0. **Plex sign-in** (§5; the user asked for it first, and it is independent), with
   `internal/netguard`.
1. **Foundations.** Schema 0003 and the jobs contract (both done), `jobqueue` validation, lock
   keys, coalescing, the refresh pool and the amended skip rule, `internal/snapshots` extracted
   from plexdb (behaviour unchanged), the pre-migration copy.
2. **The *arr client and settings** with `arrtest` (after recording the missing fixtures, §4.3),
   webhook keys, and Test.
3. **The *arr index** (full, targeted and reconciling refresh, `Locate`), its API and the Connect
   UI for the *arrs.
4. **Webhooks and targeted syncs.** Intake and processor, `ScanPaths`, targeted planning, D14,
   the follow-ups; then `make test-arr` for acceptances 1, 2 and 4.
5. **\*arr backups** (acceptance 5).
6. **Manifests**: export, versions and download (acceptance 3). Every tier is `full` until
   Phase 3.
7. **Lidarr** (D2).

Phase 2 ships when acceptances 1–5 and 10 pass; it is committed before Phase 3 starts.
*(Revision 3: slices 0–7 are built, reviewed and fixed, and acceptances 1–5 and 10 pass, §20.1.
The open questions below are still open; the defaults apply.)*

**Phase 3**

8. **Tiers**: store, evaluator, the *arr, file, source and flag fields, presets, the sync
   integration, release and preview (acceptances 6 with *arr fields, 8 and 9). Fields whose
   provider does not exist yet report `available: false`.
9. **Fixture spike** for Tautulli 2.18, Seerr and Maintainerr 3.4 (pinned containers, with a
   real Plex container for Maintainerr's collections, as the Plex DB spike did; plus the 2.17 and
   3.3 status responses for the version gates). Then **the Plex index**, then the Tautulli, Seerr
   and Maintainerr refreshes (acceptances 6 in full, and 7).
10. **UI**: the Tiers page, the Library item view, manifests and snapshots.
11. **Docs**: README, DEFERRED, CHANGELOG and ADR 0006; a manual pass with a real plex.tv
    account and a real UNAS.

Open questions for the user; the defaults above apply until answered:
1. COOP relaxed to `same-origin-allow-popups` (D10). The fallback keeps `same-origin`, and the
   popup is then not closed automatically.
2. The account-token fallback for an owned server without a server token (§5). It is kept, with
   an explicit checkbox, and can be removed.
3. The features of D13, each only if wanted: a Maintainerr proxy header, creating the webhook in
   the *arr, storing *arr UI credentials, and deleting the manual backups Bunkarr made in the *arr.
4. *arr backups stored unsealed (D4/S17), like `Preferences.xml`. A user passphrase is the
   deferred alternative.
5. The webhook credential (D7). The default is a per-integration webhook key, and the master API
   key is refused on the webhook routes. The spec says "the API key"; accepting the master key
   there would have to be an explicit setting with a warning, because it would put an admin
   credential into every *arr's database and backups.
6. Deletes reported by webhooks are retained at the next full sync, not at once (D14). The
   alternative is a cumulative 24 h guard over webhook syncs, which retains sooner but is more
   complex.
7. Copies that exist only because a fact is unknown are held above the mass-change thresholds
   (S10, S14). With no rules (the user's default) nothing is ever unknown-promoted, so this only
   matters once a rule set such as the spec preset is saved.

## 19. Decision log (revision 2 review)

Findings of the four-lens review, in the order they were raised. Ids: DS data safety, SEC
security, COR correctness, CMP completeness. Outcomes: **accepted** (in the sections named),
**partly** (what was taken and why the rest was not), **deferred** (DEFERRED.md) or **rejected**.

| Id | Sev | Finding | Outcome |
|---|---|---|---|
| DS1 | high | Spec preset + a stale *arr makes the whole library `full`: the free-space check stops every sync, or the excluded library is copied and kept forever. | Accepted: `unknownPromoted` counted by S10(b) and held; the free-space check ignores held items and fails only on decided copies; stats and preview (S10, S14, §8.1, §8.5, §8.6). |
| DS2 | high | Webhook syncs slice a mass deletion under the S10(b) thresholds, so a wiped library expires silently. | Accepted, option (a): D14, S10; the cumulative guard is open question 6. The empty-parent rule of ScanPaths (§9.1) covers the nested-mount case. |
| DS3 | high | Manifests leave out unlocated items, stale integrations, 404-deleted items, kept skip files, and report `backedUp` for links to a missing primary. | Accepted (S20, §11.1, §11.2 step 7, §6.1 404 check). "Never unchanged when stale" adapted: `status` and `fresh` are in the content hash, and the job warns. |
| DS4 | medium | A backed-up file moved into a non-full folder of the same source is retained instead of moved (a demotion removing a backup). | Accepted: pairing over every tier, then kept; `movedToNonFull`; the preset text (S15, §8.4, §8.5). |
| DS5 | medium | "Apply release" re-plans with whatever facts and rules hold then; items are not re-checked; exec's reappeared check conflicts with releases. | Accepted: `releaseOf`, `releaseRevision`, 409, the execution-time re-check, `released` skips reappeared (§8.5, §12.1, contract). |
| DS6 | medium | Maintainerr, Tautulli and Seerr joins by Plex rating key or tmdb are not scoped to a server, library or season. | Accepted: `plex_integration_id`, `library_id`, season and episode columns, scoped joins, Seerr `plexIntegrationId` (§6.2, §8.2, schema). |
| DS7 | medium | Tautulli, Seerr and Maintainerr refreshes are not all-or-nothing; an empty or partial answer is saved as fresh truth. | Accepted: all-or-nothing, shrink guard for Tautulli and Seerr (none for Maintainerr, where fewer rows only protect more) (§6, S10). |
| DS8 | medium | Irreplaceable flags stop applying after a re-add, a removal with files kept, a URL pointed at a new instance, an integration delete or a folder rename. | Accepted: external ids, last folder fallback, `ON DELETE SET NULL`, flags follow moves, unresolved flags listed (§8.7, schema). |
| DS9 | medium | Locate compares strings only, so a wrong mapping gives wrong facts; "lower integration id wins" contradicts D12. | Accepted: local path + size must match (`mismatched` otherwise), two claims → unknown, lstat of located targets (S18, §6.1, §8.3, D12). |
| DS10 | medium | "Unchanged" trusts a version directory that may be gone or damaged; manifests stop while syncs fail. | Partly: the read-back check, `integrity`, and manifests after failed syncs are accepted (§9.2, §11.2). The verify-job check is deferred: `syncer` cannot import `manifest` (dependency cycle), and the export's own check runs after every full sync. |
| DS11 | medium | The export streams while it builds (truncated CSV looks valid); pages read in separate reads can miss a renamed file. | Accepted: one read transaction, staging, `Content-Length` and `X-Bunkarr-SHA256`, 500 before any byte (S20, §11.2). |
| DS12 | medium | API-created manual backups are never pruned by the *arr and fill its appdata. | Partly: option (a) accepted (reuse a fresh scheduled backup, weekly default, count and warning, §10). Option (b), deleting Bunkarr's own manual backups, is a new write: a D13 question for the user. |
| DS13 | low | A URL or server change keeps the old instance's caches fresh. | Accepted in a read-time form: `index_state.instance_id` must match the current URL, and a refresh replaces a foreign cache (§6). This avoids `integrations` writing `mediaindex` tables. |
| DS14 | low | The S6 wait set is undefined on a resume without in-memory decisions. | Accepted: defined on persisted data; the crash-matrix case (S6, §15). |
| DS15 | low | ScanPaths follows intermediate symlinks and accepts an empty nested mountpoint one folder at a time. | Accepted with COR5 (§9.1). |
| SEC1 | critical | The webhook credential is the master API key, which the *arrs expose through their API and backups. | Accepted: per-integration webhook key, sealed, reveal/rotate endpoint, key map, master key refused on webhook routes (D7, S8, S12, §7.1, §13, schema). Open question 5 for the literal-spec alternative. |
| SEC2 | medium | The failed-auth limiter per address blocks valid webhooks behind a shared proxy or Docker bridge. | Accepted: credential first; the limiter only counts and blocks invalid requests (S13, §7.1). |
| SEC3 | medium | The probe refuses only IP literals, shared servers' LAN addresses are probed, candidates are unbounded, and no client has a dial guard. | Accepted: `internal/netguard` for every client, no local candidates for shared servers, 16 at most (S16, §5). |
| SEC4 | medium | Seerr's `displayName` falls back to the e-mail. | Accepted: never decoded; label from user names or "Seerr user #id"; reasons carry ids (§4.5, §8.1). |
| SEC5 | medium | Zip verification has no limits (zip bomb, huge central directory). | Accepted (§10 step 6). |
| SEC6 | medium | Stored payloads of up to 1 MiB × 50,000 rows and 200 concurrent reads can fill the disk and memory. | Accepted, merged with COR20: 64 KiB stored, `targets` parsed at intake, 4 body slots, a 256 MiB budget (S13, §7.1, schema). |
| SEC7 | low | An overflow merge leaves `syncAfter` without ids; random ids can force continuous full refreshes. | Partly: the overflow keeps `syncAfter` with defined untargeted follow-ups (COR1). Load is bounded by coalescing (one queued untargeted refresh absorbs the rest) and the per-integration key. Rejected: "refresh unknown ids only for add events", which would drop imports whose add event was missed. |
| SEC8 | low | `authUrl` must contain the PIN code, contrary to S19; `username` has no source. | Accepted: S19 restated, `CheckPin` sends the code, `User` via `GET plex.tv/api/v2/user` decoding only `username` (§5). |
| SEC9 | low | `system/backup` `path` could send the key to another host. | Accepted: `path` never decoded, URL from type and name (S18, §10). |
| SEC10 | low | A derived `http` candidate becomes "Recommended" and the token then travels in cleartext. | Partly: never recommended, "Unencrypted" badge, "Use anyway" (§5). Deferred: `https://<ip>` with the plex.direct TLS server name. |
| SEC11 | low | A plex.tv URL override honored in production could leak the account token. | Accepted: overrides only in `-tags e2e` builds, https unless loopback (S16, §5, `internal/testhooks`). |
| SEC12 | low | SMB does not enforce the zips' 0600 modes. | Accepted: `enforcesModes` probe and `acceptInsecureModes` (S17, §10). |
| SEC13 | low | The Maintainerr preset's `skip` drops unauthenticated-selected items from manifests. | Accepted: the preset uses `manifest`, with the notice (§8.4). |
| SEC14 | low | The paths rule rejects `.hack SIGN` and `a..b`. | Accepted: `jobs.ValidTargetPath` (§12.1, contract). |
| COR1 | high | A queued full refresh absorbs a webhook refresh and loses `syncAfter`; the overflow leaves it invalid. | Accepted (§12.2, §6.1, contract). |
| COR2 | high | `hasRunning` skips the nightly full sync while a webhook sync runs. | Accepted: the amended skip rule (§12.1). |
| COR3 | high | Lost events are never reconciled: a full refresh queues no syncs and Phase 1 has no default sync schedule. | Accepted: the reconciling full refresh, the start-up refresh, the webhook panel warnings (D8, §6.1, §13). |
| COR4 | high | Plex-id joins are unscoped (the 4K copy skipped for the HD collection). | Accepted with DS6. |
| COR5 | high | ScanPaths follows symlinked ancestors and ignores excludes and forbidden roots. | Accepted (S18, §9.1). |
| COR6 | medium | Unresolvable show/season exclusions make a member pending (less protection from missing data). | Accepted: state `undecided` → unknown (§6.2, §8.2, schema). |
| COR7 | medium | The flush rules contradict each other and treat a whole integration as one unit. | Accepted: a due time per item (§7.3). |
| COR8 | medium | The latency budget ignores the integration lock, the worker pool, the source lock and long jobs. | Partly: the refresh pool and the documented list are accepted (§7.3). Deferred: the split lock for targeted refreshes (complexity; the targeted refresh waits minutes at most). |
| COR9 | medium | NFS attribute caching can hide a new file from the targeted scan. | Accepted: expected files with rescans, `ESTALE` retry (§9.1). |
| COR10 | medium | A recycle bin inside the source doubles every upgrade's transfer and storage. | Accepted: `config/mediamanagement`, warning, the exclusion offer (§4.3, §6.1, §13, §16). |
| COR11 | medium | The refresh guard ignores files; an unmounted *arr share empties the file index. | Accepted: files guard, `accessible: false`, `allowChanges` on refresh, a minimum of 20 (S10, contract). |
| COR12 | medium | Files outside every root folder stay unknown, so the preset never applies to them; deleted items' facts are unclear. | Accepted: unmanaged files, `arr.managed`, deleted items supply nothing (§8.2, §8.3). |
| COR13 | medium | A renamed tag or profile silently turns a rule false. | Accepted: warnings on save, in the preview and in syncs (§8.5, §8.7). |
| COR14 | medium | The round-trip definition does not hold (skip files, scope, missing Sonarr fields, the content hash, a tautological test). | Partly: items always listed with every *arr file, the extra fields, `ComparePlan`, the content hash without `refreshedAt`, a test-local decoder (§11). Deferred: re-importing into a second *arr, with the re-import tool. |
| COR15 | medium | Seerr requests with empty `seasons`, and extras without a season. | Accepted (§8.2). |
| COR16 | medium | Offset paging can skip rows; history turned off for a user reads as 0 plays. | Accepted: paging integrity; `get_users` and the lower-bound semantics (§4.4, §8.2). |
| COR17 | medium | The limiter blocks correctly keyed webhooks. | Accepted with SEC2. |
| COR18 | medium | Path flags do not follow upgrades; *arr flags die on a re-add. | Accepted with DS8, plus the expiry hold for flagged content (§8.7). |
| COR19 | low | `*.partial~` and `*.backup~` temp files are copied and fail. | Accepted, to confirm with the spike harness (§9.3). |
| COR20 | low | 413 and 429 lose events the *arrs never resend. | Accepted: 16 MiB streamed, bursts of 2000 (S13). |
| COR21 | low | An equal-size, equal-mtime replacement looks unchanged; `fileDate` rewrites every mtime. | Partly: the head/tail compare for a new *arr file id and the `fileDate` warning (§6.1, §8.5). Deferred: the mtime-only update, a Phase 1 planner change that S10(b) already holds. |
| COR22 | low | The manifest follow-up is a new job for Phase 1 installs. | Accepted with CMP9 (`afterSync: auto`). |
| COR23 | low | Overlapping sources: `Locate` returns one source. | Accepted: every source, `local_path` matching (§4.1, §8.3, schema). |
| COR24 | low | Sidecars get only series-level facts and a tier that disagrees with their episode. | Accepted: stem attribution, the sidecar follows its media file (§8.3). |
| COR25 | low | `destinationIds` reads as "only there". | Partly: the "Applies at" label and the example (§8.1, §16). Rejected: an "other destinations" option; two rules already express it. |
| COR26 | low | S6 on resume. | Accepted with DS14. |
| CMP1 | high | A queued full refresh swallows a webhook refresh's `syncAfter`. | Accepted with COR1; `TestCoalesceWebhookRefreshKeepsSyncAfter`. |
| CMP2 | high | The 5 s window flushes an upgrade before the new file exists when the *arr copies it. | Accepted: `upgrade_delete` holds until the Download or 30 min; the copy-import Docker case (§7.2, §7.3, §15). |
| CMP3 | high | Freshness has two definitions; the stale e2e step needs 24 h. | Accepted: D15, `staleAfterHours`, the test clock hook, acceptance 7 reworded, the stale case in E2E (§6, §15). |
| CMP4 | high | *arr zips hold the master key that the webhooks use. | Accepted with SEC1; S17 says what the zips hold. |
| CMP5 | high | The export scope, skip files and the ParseDir step make acceptance 3 impossible or circular. | Accepted: the export scope, every item in every manifest, `Parse`/`ParseDir` per artifact, a test-local decoder, an unmapped root folder in the fixture (§11, §15). |
| CMP6 | high | Tautulli, Seerr and Maintainerr fakes would be written from the same reading as the code. | Accepted: a fixture spike from pinned containers before those refreshes (§18 slice 9, §15). |
| CMP7 | medium | Sonarr fixtures lack all-season episodes and series 2. | Accepted: re-record list and an explicit route table (§4.3). |
| CMP8 | medium | The PIN code rule contradicts `authUrl`. | Accepted with SEC8. |
| CMP9 | medium | The manifest follow-up changes what an upgraded Phase 1 install runs. | Accepted as `manifest.afterSync: auto` (no migration rewrite of settings needed) and acceptance 8 (§9.2). |
| CMP10 | medium | Held releases never run: "Apply held changes" does not send `releaseDemoted`. | Accepted: "Apply release" sends both flags plus the release params; "Apply held changes" copies them (§16, §15). |
| CMP11 | medium | The scheduler skip rule restated differently from the code. | Accepted with COR2. |
| CMP12 | medium | Every import syncs every destination, even an offline one. | Partly: `syncOnArrChange` per destination and the "not mounted; skipped" soft end. The default is on rather than "on when scheduled", because a destination without a schedule is exactly where webhook syncs are the only automatic copies. |
| CMP13 | medium | No refresh after create; `/arr/rootfolders` source unclear. | Accepted (§4.1, §13). |
| CMP14 | medium | The Test body cannot carry unsaved settings. | Accepted (§4.1, §13). |
| CMP15 | medium | Free-text reasons and no pinned table make acceptance 6 untestable. | Accepted: structured reasons, the preview items, the pinned table (§8.1, §15). |
| CMP16 | medium | `lastWatched` of a never-watched file is undefined. | Accepted (§8.2). |
| CMP17 | medium | Plex sections have no table. | Accepted: `plex_sections` (§6.3, schema). |
| CMP18 | medium | `sources.arr_integration_id` is never defined. | Accepted (§4.1, §8.3, D12). |
| CMP19 | medium | *arr flags keyed only by `arr_id`. | Accepted with DS8. |
| CMP20 | medium | No way back from a failed Phase 2 upgrade. | Accepted: the pre-migration `VACUUM INTO` copy (§14.1). |
| CMP21 | medium | No frontend tests; the ported state machine holds a token. | Accepted: the new states and web tests (§5, §15). |
| CMP22 | medium | No e2e for Rename and deletes; constant delays make them slow. | Accepted: e2e cases and `internal/testhooks` (§15, §14.2). |
| CMP23 | medium | Nothing ships until all integrations work; genres and the tier column add surface without acceptance. | Accepted: D16 and the re-sliced order (§18). |
| CMP24 | low | Acceptance 1 does not check "just that file". | Accepted (acceptance 1, §15). |
| CMP25 | low | Dry-run skip items cannot be grouped by tier. | Accepted: `tier` and `ruleId` filters, `tier` summary dimension (§13). |
| CMP26 | low | No notification policy for the new jobs. | Accepted (§12.5). |
| CMP27 | low | Phase 1 rows of the new types have no normalization. | Accepted (§4.1). |
| CMP28 | low | The paths rule rejects leading dots. | Accepted with SEC14. |
| CMP29 | low | The limiter order is unspecified. | Accepted with SEC2. |
| CMP30 | low | Webhook outcomes are not checked or defined; "coalesced" is not detectable. | Accepted: `CHECK`, definitions, `unmapped` dropped (a refresh-level warning), `coalesced` from `queued_at` (§7.3, schema). |
| CMP31 | low | The CSV has no encoding for multi-episode files. | Accepted (§11.1). |

## 20. Revision 3: Phase 2 as built

Phase 2 (slices 0–7 of §18) was built in eight slices, reconciled (API, `openapi.json`, UI
wiring), run against real Sonarr, Radarr and Lidarr containers, then put through a six-lens code
review (data safety, crash safety, security, concurrency, correctness, API/UI) with two skeptics
per finding and a verified fix per owning package. The sections above are amended in place where
the built code differs from revision 2; the amendments are marked *(as built)*. This section lists
them with their reasons. Where something was not done, DEFERRED.md has it.

### 20.1 Acceptance

| # | Result | How |
|---|---|---|
| 1 Import | pass | `make test-arr`: each import at the destination 5–6 s after the *arr finished it (Radarr 3 movies; Sonarr E01–E03 into a series with backed-up episodes; Lidarr 5 s), by a sync with trigger `webhook` whose only non-skip item is a `copy` with the source's sha256. Native e2e: about 1 s after the Download POST, on both destinations. |
| 2 Upgrade | pass | One targeted sync, copy before retain, the old file's hash in retention; also with the new file copied from another volume (Sonarr from `/downloads2`), and the same-path case as an `update` retained as `replaced`. Lidarr: FLAC over MP3 in one sync. |
| 3 Manifest | pass | Real Radarr and Sonarr: `ParseDir` of the version, the download and `/manifest/export` compare with no difference against the state decoded from the raw API JSON, the unmapped `/movies-4k` root included. |
| 4 Webhook auth | pass | Real Radarr, Sonarr and Lidarr Test events: 200, nothing queued. A wrong key, the master API key and a session cookie alone: 401; the webhook key on another route: 401. |
| 5 *arr backup | pass | Real Radarr and Lidarr with `/config/Backups` mounted `:ro`: Forms login → an `ok` snapshot by folder and the guidance error over HTTP; a fresh scheduled backup copied with no `Backup` command, then `unchanged`; "Disabled for Local Addresses" → `ok` over HTTP. |
| 10 Plex sign-in | pass | `TestPlexSignInE2E` against the e2e binary, a fake plex.tv and a fake PMS: the PMS saw only the server's own token; no answer and no log held a token; the PIN code only inside `authUrl`, the PIN id never; the manual path as in Phase 1. |

The final run on the reviewed tree: `gofmt -l` clean; `go vet ./...` and `-tags e2e`, native and
`GOOS=linux GOARCH=amd64`; `go test -race -count=1 ./...` (about 450 s); web typecheck, 380 tests
and build; `go test -tags e2e ./internal/e2e/...`; `make docker`; `make test-docker` (image, kill,
Plex restore, SMB and NFS shares); `make test-arr`. All pass.

### 20.2 Implementation deviations (now the contract)

| Area | Change | Why | § |
|---|---|---|---|
| Sign-in | An expired sign-in answers `expired` for 10 min, then 404; sign-in ids are redacted in the request log. | The UI can tell "expired" from "unknown"; the id is a bearer handle. | §5 |
| Jobs | Validation refuses params on the wrong job type; queued untargeted jobs cover a targeted spec only with the same `allowChanges`; coalescing skips re-queued jobs; the overflow note is logged at merge. | Held changes must not be re-held; a re-queued plan may be complete. | §12.1, §12.2 |
| Snapshots | `GET /destinations/{id}/snapshots` lists both kinds and `Snapshot` gains `kind`; a new database gets no pre-migration copy. | One listing for both kinds; nothing to go back to. | §13, §14.1 |
| *arr client | Backup downloads accept `application/x-zip-compressed` (Lidarr 3.1.0) and `application/octet-stream`; Lidarr ignores `Range`. The 4K movie's file id is 5 in the fixtures; `series-2.json` is `series.json[1]`. | Real apps (re-recorded in fresh containers of the spike's versions, `testdata/arr/record_slice2.py`). | §10, §4.3 |
| Index | A held full refresh does not move `refreshed_at` (status `ok`, reason in `error`); only the item's current folder is checked for existence; the reconcile's changed set is recorded as job items first; a resumed reconciling refresh queues with trigger `startup`; only *arr types refresh. | Crash safety of the reconcile; the old folder of a move is expected to be gone. | S10, §6.1, §13 |
| Webhooks | A flush also takes items whose quiet window ends within one more window; the Download that releases an upgrade hold restarts the window; the key is checked before `{app}`; the generic-route warning is in memory. | One burst, one refresh; no information for an unauthenticated caller. | §7 |
| Targeted sync | A path whose parent folders fail the checks is dropped; the plan includes the other names of its files' hardlink groups; a follow-up sync is one with trigger `webhook` or `paths`. | A new link to a backed-up file is linked, not copied. | §9.1 |
| *arr backups | The zip may expand 20× its size or 256 MiB, whichever is larger; a scheduled backup whose earlier copy failed verification is not reused; access is checked before the Backup command; a resumed job follows its recorded command; `integrity.database` may be `not-checked`, `crc32` is 8 hex digits; an extra route lists an integration's versions. | A fresh Radarr zip expands 23.5×; never leave manual backups behind in the *arr. | §10, §13 |
| Manifests | Other packages' tables are read directly by `queries.go`; a day or week of retention counts only with a version; damaged versions are kept 7 days; a failed build answers 500 with a JSON error; the job stats add `path` and `recovered`; the CSV names integrations and sources. | No owner queries existed yet. | §11 |
| Lidarr | `monitorNewItems` is in Lidarr's `detail` too. | The index, manifest and decoder carry it. | §11.1 |
| Destinations | `syncOnArrChange` and `manifest.afterSync` are not stored: every destination syncs on *arr changes and behaves as `auto`. | Left for the destination form; the defaults are the designed ones. | §6.1, §9.2, §13 |
| E2E | The e2e binary is built with `-tags e2e`. | The webhook windows and plex.tv URLs come from `internal/testhooks`. | §14.2, §15 |

### 20.3 Code review (Phase 2 gate)

The six lenses reported 48 findings: 1 high, 20 medium and 27 low. Every non-low finding was
confirmed by at least one of two skeptics (none refuted); the lows went to their owners directly.
One known issue from the acceptance run was added (late-visible files and the source lock). Each
fix has a regression test that fails without it. A verifier per package then tried to break the
fixes and found problems in eight packages, some of them introduced by the fixes; a second pass
fixed them, except those listed in §20.4.

| Sev | Finding | Fix | § |
|---|---|---|---|
| high | Settings → General told users to put the master API key into *arr webhooks, which refuse it (D7). | The help text points to Settings → Connect. | §16 |
| medium | The Apprise client did not dial through netguard. | `netguard.NewTransport()`; a refused address fails at once. | S16 |
| medium | The §12.5 notification policy was not implemented. | Per-key limits; see the amended §12.5 (second pass: scope-aware clearing, separate warning and failure slots, slots given back when undelivered). | §12.5 |
| medium | The connection probe sent the server or account token over plain http before the user chose it. | Token only over https or loopback http. | §5 |
| medium | Unticking "Use my Plex account token" after choosing a connection still saved the account token. | The selection is dropped. | §5 |
| medium | Targeted scans keyed catalog rows by the *arr's spelling on case- or normalization-insensitive filesystems. | Every component must be listed exactly; a leaf listed only under another spelling is gone, so a case-only rename pairs as a move (second pass). `DirExists` caches listings per folder (second pass: it had become quadratic). | §6.1, §9.1 |
| medium | A manifest dropped kept backups of deleted sources that shared a relative path. | Every live record is listed; orphans by their destination path. | §11.1 |
| medium | Manifest builds held the library and full JSON/CSV copies in memory, with no bound. | Streamed writes; one build at a time; the `manifest-build` lock key (second pass: a waiting job held a worker). | §11.2, §12.1 |
| medium | The dedupe returned a re-queued sync whose plan was complete, so a new import in its folder was never copied. | Planned sync, verify and retention jobs are not deduplicated into (second pass: backups keep the exact dedupe). | §12.2 |
| medium | The skip rule treated a one-source follow-up sync as a full sync and skipped the nightly one. | The running job must cover the schedule's sources. | §12.1 |
| medium | A failed overflow refresh queued no follow-up, and the webhook items it absorbed were lost (found twice). | Untargeted syncs of the root folders' sources after a failure. | §6.1 |
| medium | A cancel or shutdown during the follow-up enqueue completed the refresh and lost the sync. | The job ends cancelled and its intents stay pending. | §6.1 |
| medium | Every webhook sync read the whole destination and source. | Scoped reads through index ranges. | §9.1 |
| medium | The expected-file check waited 50 s, holding the locks, for files the scan never catalogues. | Files classified as the scan sees them (second pass: unreadable folders and other spellings); only pending ones are waited for. | §9.1 |
| medium | Late-visible files: rescans held the source lock, so a second destination missed 60 s (acceptance run). | The source lock is freed during each wait. | §9.1 |
| medium | `Processor.Stop` hung until the shutdown deadline after a failed `Start`. | The loop's done channel is closed. | §7.3 |
| medium | The payload-budget prune had no hysteresis and scanned all payloads on every insert. | 90 % low water, a tracked total, the prune in the processor loop (second pass: retried after `RetryDelay`). | S13, §7.3 |
| medium | An event committed by `Insert` was never processed when the post-insert prune failed. | `Insert` does nothing after its commit. | §7.1 |
| medium | Manifest downloads were plain links: an error was saved as the manifest. | The UI fetches first and shows the error. | §11.2 |
| medium | The webhook panel never refreshed ("Last Test received: never"). | Polled every 5 s, "Check again". | §16 |
| medium | Nothing in the UI applied what the refresh guard held; the index stayed stale. | "Apply held changes" on the card. | S10, §16 |
| low | *arr backups: staging with the zip's secrets could stay forever; a resumed job sent a second Backup command; recovery adopted another app's versions after ids were reused; "unchanged" was reported without reading the stored zip back. | Staging removed on return, by the `OnFinish` hook and at start-up; the command id is recorded (second pass: orphaned commands ask again, earlier jobs' versions after a rename are recorded); the manifest's app and job are checked; the copy is re-hashed. | S17, §10 |
| low | Webhooks: re-arming a cancelled refresh was not durable, a cancel between enqueue and mark lost the events, `Insert` could fail after its commit. | Re-armed at start; the mark checks the job's status; nothing after the commit. | §7.1, §7.3 |
| low | Index: early refresh failures skipped the failure handling; a cancelled reconcile dropped its intents; the unmapped page loaded every file. | One failure path; intents followed up, also those stranded by jobs that ended without their runner (second pass); SQL paging with the `arr_files_path` index (second pass: the keyset was quadratic without it). | §6.1, §14.1 |
| low | Expected files: a nested read-pool query while a cursor was open; the source's excludes were ignored; rescans inflated the stats. | Rows collected first; the excludes applied; counts from the plan's own read. | §9.1, §9.3 |
| low | Manifests: `manifestWeeks: 0` was ignored and neither manifest setting could be set; Sonarr's `tvMazeId` was never decoded. | `manifestDays`/`manifestWeeks` in the destination retention; `externalIds.tvmaze`. | §11, §13 |
| low | The pre-migration prune could delete the copy just made (clock behind older copies). | The new copy is always kept. | §14.1 |
| low | `openapi.json` said truncated payloads are cut at 1 MiB. | 64 KiB, checked by a test against `webhooks.MaxPayload`. | §13 |
| low | Web: a stale *arr Test result, Copy doing nothing over plain http, the webhook URL built from a loopback origin, the countdown read out every second, the browser clock used for the sign-in expiry, sign-in warnings dropped, a failed connection test shown as "no connection". | Each fixed. | §5, §16 |
| low | Notify: untargeted refresh follow-ups were not recognised; a clock set back switched the limit off. | Recognised by `sourceIds`; a 24 h gap either way starts a new window. | §12.5 |

### 20.4 Left open

These are in DEFERRED.md with their reasons:
- an untargeted follow-up sync of a scheduled, start-up or manual refresh carries no marker, so
  it fails (instead of "not mounted; skipped") when the destination is not mounted;
- `syncOnArrChange`, `manifest.afterSync`, a manifest schedule, "Import from Radarr/Sonarr" and
  the `sources.arr_integration_id` type check;
- `catalog.LiveUnder` and `LiveInGroups` still make SQLite read the source's rows (about 70 ms a
  call at 1M files); copies of the exclude matcher in `api` and `syncer`;
- pre-migration copies named with a future time; a *arr backup version whose job row was pruned
  from the history; the Plex DB staging swept only by the next backup;
- the in-memory notification limits; the tvmaze column of the CSV;
- `make test-arr` in CI, a real Sonarr backup test, Lidarr Rename/Retag/`isUpgrade` in Docker;
- test gaps the verifiers named (the Plex server switch-back path, the *arr Test error scoping,
  ctime alone in the listing cache on APFS).
