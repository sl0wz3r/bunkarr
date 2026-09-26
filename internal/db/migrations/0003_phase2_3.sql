-- Phases 2 and 3: the *arr index and the other metadata caches (Plex library, Tautulli, Seerr,
-- Maintainerr), webhook events, tier rules and flags, manifests; the job types refresh,
-- arr_backup and manifest_export; *arr config backups in snapshots; the 'released' retention
-- reason; the per-integration webhook key. Times are RFC3339 UTC text (db.TimeFormat); JSON
-- columns are text. Contract: docs/design/phase2-3.md, revision 2 (§14.1 lists what changes and
-- why). internal/db copies the database (VACUUM INTO) before applying this migration.
--
-- How the rebuilds work. Widening a CHECK constraint needs a table rebuild, and this migration
-- runs inside the runner's transaction with foreign_keys ON, where PRAGMA foreign_keys cannot be
-- changed. A plain DROP TABLE of a parent would then run its children's ON DELETE actions
-- (CASCADE would empty job_items and job_logs, SET NULL would clear snapshots.job_id). So:
--   1. each rebuilt parent is copied into <name>_new;
--   2. its children are rebuilt as <child>_new, referencing <name>_new;
--   3. the old tables are dropped, children first: by then nothing references them, so their
--      implicit DELETE runs no foreign key action;
--   4. ALTER TABLE ... RENAME (legacy_alter_table OFF, the default) renames <name>_new and
--      rewrites every reference to it, so the children end up referencing <name> again;
--   5. the indexes are created again under their old names.
-- destination_files references itself (link_of, ON DELETE RESTRICT): the old table's links are
-- cut before it is dropped (RESTRICT would refuse the implicit DELETE), after its rows were copied.
-- Foreign keys are deferred for the whole migration, so rows may be copied in any order; they are
-- all checked at commit. internal/db TestMigration0003KeepsPhase1Data covers every table.

PRAGMA defer_foreign_keys = ON;

-- jobs, job_items, job_logs, snapshots -------------------------------------------------------

CREATE TABLE jobs_new (
    id             INTEGER PRIMARY KEY,
    type           TEXT    NOT NULL CHECK (type IN ('scan', 'sync', 'plexdb_backup', 'retention', 'verify',
                                                    'refresh', 'arr_backup', 'manifest_export')),
    status         TEXT    NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'completed_with_warnings', 'failed', 'cancelled')),
    trigger        TEXT    NOT NULL CHECK (trigger IN ('schedule', 'manual', 'webhook', 'resume', 'startup')),
    dry_run        INTEGER NOT NULL DEFAULT 0 CHECK (dry_run IN (0, 1)),
    params         TEXT    NOT NULL DEFAULT '{}',
    -- denormalized from params for per-destination serialization and filtering
    destination_id INTEGER,
    -- denormalized from params (integrationId) for filtering: refresh, plexdb_backup, arr_backup
    integration_id INTEGER,
    attempt        INTEGER NOT NULL DEFAULT 1,
    -- set in the same transaction as the last plan batch; items without it are an interrupted plan
    planned_at     TEXT,
    progress       TEXT    NOT NULL DEFAULT '{}',
    stats          TEXT    NOT NULL DEFAULT '{}',
    warnings       INTEGER NOT NULL DEFAULT 0,
    summary        TEXT    NOT NULL DEFAULT '',
    error          TEXT,
    queued_at      TEXT    NOT NULL,
    started_at     TEXT,
    finished_at    TEXT,
    heartbeat_at   TEXT
) STRICT;

INSERT INTO jobs_new (id, type, status, trigger, dry_run, params, destination_id, integration_id, attempt,
                      planned_at, progress, stats, warnings, summary, error, queued_at, started_at, finished_at,
                      heartbeat_at)
SELECT id, type, status, trigger, dry_run, params, destination_id,
       CASE WHEN json_valid(params) THEN
           CASE WHEN json_type(params, '$.integrationId') = 'integer' THEN json_extract(params, '$.integrationId') END
       END,
       attempt, planned_at, progress, stats, warnings, summary, error, queued_at, started_at, finished_at, heartbeat_at
FROM jobs;

CREATE TABLE job_items_new (
    id       INTEGER PRIMARY KEY,
    job_id   INTEGER NOT NULL REFERENCES jobs_new (id) ON DELETE CASCADE,
    -- catalog_files.id or destination_files.id, depending on the action; informational
    file_id  INTEGER,
    rel_path TEXT    NOT NULL,
    action   TEXT    NOT NULL CHECK (action IN ('copy', 'update', 'move', 'adopt', 'link', 'promote', 'retain', 'expire', 'verify', 'backup', 'skip')),
    status   TEXT    NOT NULL CHECK (status IN ('pending', 'done', 'failed', 'skipped', 'held')),
    bytes    INTEGER NOT NULL DEFAULT 0,
    error    TEXT,
    -- runner-specific JSON (source path, temp path, link target, reason, tier decision)
    detail   TEXT    NOT NULL DEFAULT '{}'
) STRICT;

INSERT INTO job_items_new (id, job_id, file_id, rel_path, action, status, bytes, error, detail)
SELECT id, job_id, file_id, rel_path, action, status, bytes, error, detail FROM job_items;

CREATE TABLE job_logs_new (
    id      INTEGER PRIMARY KEY,
    job_id  INTEGER NOT NULL REFERENCES jobs_new (id) ON DELETE CASCADE,
    at      TEXT    NOT NULL,
    level   TEXT    NOT NULL CHECK (level IN ('debug', 'info', 'warn', 'error')),
    message TEXT    NOT NULL,
    fields  TEXT    NOT NULL DEFAULT '{}'
) STRICT;

INSERT INTO job_logs_new (id, job_id, at, level, message, fields)
SELECT id, job_id, at, level, message, fields FROM job_logs;

-- Plex DB versions and *arr config backups (owned by internal/snapshots).
CREATE TABLE snapshots_new (
    id                 INTEGER PRIMARY KEY,
    destination_id     INTEGER NOT NULL REFERENCES destinations (id) ON DELETE CASCADE,
    job_id             INTEGER REFERENCES jobs_new (id) ON DELETE SET NULL,
    -- plexdb: a Plex DB version (internal/plexdb); arr: an *arr backup zip (internal/arrbackup)
    kind               TEXT    NOT NULL CHECK (kind IN ('plexdb', 'arr')),
    integration_id     INTEGER REFERENCES integrations (id) ON DELETE SET NULL,
    -- version directory relative to the destination target
    engine_snapshot_id TEXT    NOT NULL,
    created_at         TEXT    NOT NULL,
    size               INTEGER NOT NULL,
    -- plexdb: online_backup | online_backup_immutable; arr: arr_api_folder | arr_api_http
    method             TEXT    NOT NULL,
    integrity          TEXT    NOT NULL CHECK (integrity IN ('ok', 'failed')),
    manifest           TEXT    NOT NULL DEFAULT '{}'
) STRICT;

INSERT INTO snapshots_new (id, destination_id, job_id, kind, integration_id, engine_snapshot_id, created_at, size,
                           method, integrity, manifest)
SELECT id, destination_id, job_id, kind, integration_id, engine_snapshot_id, created_at, size, method, integrity,
       manifest
FROM snapshots;

DROP TABLE job_items;
DROP TABLE job_logs;
DROP TABLE snapshots;
DROP TABLE jobs;

ALTER TABLE jobs_new RENAME TO jobs;
ALTER TABLE job_items_new RENAME TO job_items;
ALTER TABLE job_logs_new RENAME TO job_logs;
ALTER TABLE snapshots_new RENAME TO snapshots;

CREATE INDEX jobs_status ON jobs (status, queued_at);
CREATE INDEX jobs_finished ON jobs (finished_at) WHERE finished_at IS NOT NULL;
CREATE INDEX jobs_destination ON jobs (destination_id, queued_at);
CREATE INDEX jobs_integration ON jobs (integration_id, queued_at) WHERE integration_id IS NOT NULL;
CREATE INDEX job_items_job_status ON job_items (job_id, status, id);
CREATE INDEX job_items_job_action ON job_items (job_id, action, id);
CREATE INDEX job_logs_job ON job_logs (job_id, id);
CREATE INDEX snapshots_destination ON snapshots (destination_id, kind, created_at);
-- a version directory is recorded at most once (a resumed job records an unrecorded one)
CREATE UNIQUE INDEX snapshots_path ON snapshots (destination_id, engine_snapshot_id);

-- schedules ----------------------------------------------------------------------------------

CREATE TABLE schedules_new (
    id          INTEGER PRIMARY KEY,
    job_type    TEXT    NOT NULL CHECK (job_type IN ('scan', 'sync', 'plexdb_backup', 'retention', 'verify',
                                                     'refresh', 'arr_backup', 'manifest_export')),
    -- jobs.Params JSON, e.g. {"destinationId":1}
    params      TEXT    NOT NULL DEFAULT '{}',
    -- standard 5-field cron expression, evaluated in the container's TZ
    cron        TEXT    NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    last_run_at TEXT,
    created_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL,
    UNIQUE (job_type, params)
) STRICT;

INSERT INTO schedules_new (id, job_type, params, cron, enabled, last_run_at, created_at, updated_at)
SELECT id, job_type, params, cron, enabled, last_run_at, created_at, updated_at FROM schedules;

DROP TABLE schedules;
ALTER TABLE schedules_new RENAME TO schedules;

-- destination_files: reason 'released' -------------------------------------------------------

CREATE TABLE destination_files_new (
    id             INTEGER PRIMARY KEY,
    destination_id INTEGER NOT NULL REFERENCES destinations (id) ON DELETE CASCADE,
    source_id      INTEGER REFERENCES sources (id) ON DELETE SET NULL,
    -- relative to the destination target: "<destFolder>/<relPath>"
    rel_path       TEXT    NOT NULL,
    -- the path inside the source (catalog_files.rel_path)
    source_rel_path TEXT   NOT NULL,
    -- the SOURCE file's size and mtime when copied (the planner compares the catalog to these)
    size           INTEGER NOT NULL,
    mtime_ns       INTEGER NOT NULL,
    hash           TEXT,
    -- the row this path is a hardlink of (recreated or only recorded); RESTRICT: a primary with
    -- dependents cannot disappear without promotion (phase1.md §4.2)
    link_of        INTEGER REFERENCES destination_files_new (id) ON DELETE RESTRICT,
    -- present: a file at rel_path; linked: a hardlink at rel_path to link_of's file;
    -- link_recorded: no file at rel_path (no hardlinks at the destination), content is link_of's;
    -- missing: verify found the file gone or damaged (the next sync copies it again);
    -- retained: moved to retained_path until expires_at
    state          TEXT    NOT NULL CHECK (state IN ('present', 'linked', 'link_recorded', 'missing', 'retained')),
    retained_path  TEXT,
    -- why a retained row was retained: deleted (gone from the source), replaced (old version of an
    -- update), displaced (an unmanaged file that was in the way, S2), damaged (a file verify
    -- marked missing that was still there when it was replaced), released (the user released a
    -- file kept at the destination after its tier stopped being full, phase2-3.md S15)
    reason         TEXT CHECK (reason IS NULL OR reason IN ('deleted', 'replaced', 'displaced', 'damaged', 'released')),
    job_id         INTEGER,
    copied_at      TEXT,
    verified_at    TEXT,
    retained_at    TEXT,
    expires_at     TEXT,
    CHECK (state NOT IN ('linked', 'link_recorded') OR link_of IS NOT NULL),
    CHECK (state <> 'retained' OR (retained_path IS NOT NULL AND expires_at IS NOT NULL))
) STRICT;

INSERT INTO destination_files_new (id, destination_id, source_id, rel_path, source_rel_path, size, mtime_ns, hash,
                                   link_of, state, retained_path, reason, job_id, copied_at, verified_at,
                                   retained_at, expires_at)
SELECT id, destination_id, source_id, rel_path, source_rel_path, size, mtime_ns, hash, link_of, state,
       retained_path, reason, job_id, copied_at, verified_at, retained_at, expires_at
FROM destination_files;

-- Cut the old table's self-references (its rows are copied): otherwise ON DELETE RESTRICT refuses
-- the implicit DELETE of DROP TABLE. Linked names become 'present' only in the table being dropped.
UPDATE destination_files
SET state   = CASE WHEN state IN ('linked', 'link_recorded') THEN 'present' ELSE state END,
    link_of = NULL
WHERE link_of IS NOT NULL;

DROP TABLE destination_files;
ALTER TABLE destination_files_new RENAME TO destination_files;

CREATE UNIQUE INDEX destination_files_live ON destination_files (destination_id, rel_path)
    WHERE state IN ('present', 'linked', 'link_recorded', 'missing');
CREATE INDEX destination_files_source ON destination_files (destination_id, source_id, source_rel_path);
CREATE INDEX destination_files_link_of ON destination_files (link_of) WHERE link_of IS NOT NULL;
CREATE INDEX destination_files_expiry ON destination_files (destination_id, expires_at) WHERE state = 'retained';

-- catalog_files: drop the Phase 0 placeholders ------------------------------------------------
-- Tiers are per destination and evaluated (phase2-3.md §8, D5), and a file's *arr item comes from
-- arr_files (several *arr integrations); a single tier / arr_item_id / media_ids per file was the
-- wrong shape. Nothing wrote them.

ALTER TABLE catalog_files DROP COLUMN media_ids;
ALTER TABLE catalog_files DROP COLUMN arr_item_id;
ALTER TABLE catalog_files DROP COLUMN tier;

-- integrations: the webhook key --------------------------------------------------------------
-- Sonarr, Radarr and Lidarr authenticate their webhooks with a key of their own, never with
-- Bunkarr's master API key (phase2-3.md D7, S8): 16 random bytes, hex, sealed with AAD
-- "integration:<id>:webhookKey". '' = none (other types; pre-0003 *arr rows get one at start-up).

ALTER TABLE integrations ADD COLUMN webhook_key TEXT NOT NULL DEFAULT '';

-- Metadata caches (owned by internal/mediaindex) ----------------------------------------------

-- Refresh state of every integration that has a cache (*arr, Plex library index, Tautulli, Seerr,
-- Maintainerr). A cache is fresh while refreshed_at is younger than the integration's
-- staleAfterHours and instance_id names its current URL; otherwise its facts are "unknown"
-- (phase2-3.md §6, D15, S14). status and error describe the last attempt and never affect freshness.
CREATE TABLE index_state (
    integration_id INTEGER PRIMARY KEY REFERENCES integrations (id) ON DELETE CASCADE,
    status         TEXT    NOT NULL DEFAULT 'never' CHECK (status IN ('never', 'ok', 'failed')),
    -- the last complete (full) refresh
    refreshed_at   TEXT,
    -- the last refresh attempt (full or targeted) and its error
    attempted_at   TEXT,
    error          TEXT,
    -- the instance the cache describes: the integration's URL when it was refreshed (Plex index:
    -- "<url>#<machineIdentifier>"); a refresh that finds another one replaces every cache row
    instance_id    TEXT    NOT NULL DEFAULT '',
    -- the application's version as it reported it
    app_version    TEXT    NOT NULL DEFAULT '',
    -- counts of the last full refresh, plus per-type notes (e.g. Tautulli sections and the number
    -- of users without history, the *arr recycle bin)
    stats          TEXT    NOT NULL DEFAULT '{}'
) STRICT;

-- Movies (Radarr), series (Sonarr) and artists (Lidarr).
CREATE TABLE arr_items (
    id                  INTEGER PRIMARY KEY,
    integration_id      INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    kind                TEXT    NOT NULL CHECK (kind IN ('movie', 'series', 'artist')),
    -- the item's id in the *arr (movie.id, series.id, artist.id)
    arr_id              INTEGER NOT NULL,
    title               TEXT    NOT NULL,
    year                INTEGER NOT NULL DEFAULT 0,
    -- {"tmdb":10331,"imdb":"tt0063350","tvdb":…,"tvmaze":…,"mbid":"…"}; a missing key is unknown
    external_ids        TEXT    NOT NULL DEFAULT '{}',
    -- as the *arr sees them (container paths); mapped through the integration's pathMappings
    path                TEXT    NOT NULL,
    root_folder         TEXT    NOT NULL DEFAULT '',
    quality_profile_id  INTEGER NOT NULL DEFAULT 0,
    -- Lidarr only (0 otherwise)
    metadata_profile_id INTEGER NOT NULL DEFAULT 0,
    monitored           INTEGER NOT NULL CHECK (monitored IN (0, 1)),
    -- JSON array of tag ids (labels in arr_meta)
    tags                TEXT    NOT NULL DEFAULT '[]',
    genres              TEXT    NOT NULL DEFAULT '[]',
    added_at            TEXT,
    -- kind-specific fields the manifest round-trips (phase2-3.md §11.1): Sonarr seriesType,
    -- seasonFolder, monitorNewItems, useSceneNumbering, languageProfileId,
    -- seasons[{seasonNumber,monitored}], episodes[{season,episode,monitored}]; Radarr
    -- minimumAvailability; Lidarr albums[{id,mbid,title,monitored}]; and hasFile / file counts
    detail              TEXT    NOT NULL DEFAULT '{}',
    -- the refresh that last saw the item
    seen_at             TEXT    NOT NULL,
    -- set when a full refresh (or a targeted one answered 404 while system/status still named the
    -- expected app) no longer finds it; such an item supplies no facts; purged after 30 days
    deleted_at          TEXT,
    UNIQUE (integration_id, kind, arr_id)
) STRICT;

-- Files the *arr knows (movieFile, episodeFile, trackFile), located in the catalog.
CREATE TABLE arr_files (
    id             INTEGER PRIMARY KEY,
    integration_id INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    item_id        INTEGER NOT NULL REFERENCES arr_items (id) ON DELETE CASCADE,
    -- a same-path replacement gets a new id (the spike): key on it, never on the path
    arr_file_id    INTEGER NOT NULL,
    -- as the *arr sees it
    path           TEXT    NOT NULL,
    -- path after the path mappings (as Bunkarr sees it); NULL when no mapping applies. Facts match
    -- a catalog file by this path (any containing source, sources may overlap) and by size;
    -- otherwise the row is "mismatched" (phase2-3.md S18)
    local_path     TEXT,
    -- the longest-prefix source that contains local_path (scan targets, expected files); NULL when
    -- it maps into no source (rel_path is only meaningful with source_id)
    source_id      INTEGER REFERENCES sources (id) ON DELETE SET NULL,
    rel_path       TEXT,
    size           INTEGER NOT NULL,
    -- quality name, e.g. "Bluray-1080p"
    quality        TEXT    NOT NULL DEFAULT '',
    date_added     TEXT,
    -- episodes [{episodeId, seasonNumber, episodeNumber, tvdbId}], album {id, mbid, title},
    -- tracks [...], relativePath, sceneName, releaseGroup
    detail         TEXT    NOT NULL DEFAULT '{}',
    seen_at        TEXT    NOT NULL,
    UNIQUE (integration_id, arr_file_id)
) STRICT;

CREATE INDEX arr_files_item ON arr_files (item_id);
CREATE INDEX arr_files_location ON arr_files (source_id, rel_path) WHERE source_id IS NOT NULL;
CREATE INDEX arr_files_local ON arr_files (local_path) WHERE local_path IS NOT NULL;
-- the unmapped listing's keyset over (path, id) (internal/mediaindex, INDEXED BY)
CREATE INDEX arr_files_path ON arr_files (path, id);

-- Quality profiles, metadata profiles (Lidarr), root folders and tags of an *arr.
CREATE TABLE arr_meta (
    integration_id INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    kind           TEXT    NOT NULL CHECK (kind IN ('quality_profile', 'metadata_profile', 'root_folder', 'tag')),
    arr_id         INTEGER NOT NULL,
    -- profile name, root folder path (as the *arr sees it) or tag label
    name           TEXT    NOT NULL,
    -- root folders: {"accessible":true,"localPath":…,"sourceId":…}
    detail         TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (integration_id, kind, arr_id)
) STRICT;

-- Plex library sections and their locations (plex.section facts, Tautulli's sections, the
-- section suggestions of the rule editor).
CREATE TABLE plex_sections (
    integration_id INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    section_key    TEXT    NOT NULL,
    title          TEXT    NOT NULL,
    -- movie | show | artist | photo
    type           TEXT    NOT NULL,
    -- JSON [{"path":…,"localPath":…|null,"sourceId":…|null,"relPath":…|null}]: each location as
    -- Plex sees it, mapped and located (null when unmapped)
    locations      TEXT    NOT NULL DEFAULT '[]',
    PRIMARY KEY (integration_id, section_key)
) STRICT;

-- Plex library index: movies, shows, seasons, episodes, artists, albums, tracks. (Plex genres are
-- deferred, phase2-3.md D16; media.genre comes from the *arr.)
CREATE TABLE plex_items (
    integration_id  INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    rating_key      TEXT    NOT NULL,
    type            TEXT    NOT NULL CHECK (type IN ('movie', 'show', 'season', 'episode', 'artist', 'album', 'track')),
    section_key     TEXT    NOT NULL,
    parent_key      TEXT,
    grandparent_key TEXT,
    -- Plex's index and parentIndex: a season's number, an episode's number and its season's
    item_index      INTEGER,
    parent_index    INTEGER,
    guid            TEXT    NOT NULL DEFAULT '',
    -- {"imdb":"tt…","tmdb":"…","tvdb":"…"} from Guid[]
    external_ids    TEXT    NOT NULL DEFAULT '{}',
    title           TEXT    NOT NULL DEFAULT '',
    added_at        TEXT,
    PRIMARY KEY (integration_id, rating_key)
) STRICT;

-- Media[].Part[].file of every movie, episode and track, located in the catalog.
CREATE TABLE plex_files (
    integration_id INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    rating_key     TEXT    NOT NULL,
    -- as Plex sees it
    file           TEXT    NOT NULL,
    -- file after the path mappings; facts match a catalog file by this path
    local_path     TEXT,
    source_id      INTEGER REFERENCES sources (id) ON DELETE SET NULL,
    rel_path       TEXT,
    PRIMARY KEY (integration_id, file, rating_key)
) STRICT;

CREATE INDEX plex_files_location ON plex_files (source_id, rel_path) WHERE source_id IS NOT NULL;
CREATE INDEX plex_files_local ON plex_files (local_path) WHERE local_path IS NOT NULL;
CREATE INDEX plex_files_key ON plex_files (integration_id, rating_key);

-- Tautulli play counts, counted from get_history rows. The keys are those of the Tautulli
-- integration's linked Plex server (plexIntegrationId): they are looked up only in that server's
-- plex_files and plex_items.
CREATE TABLE watch_stats (
    integration_id  INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    -- rating_key: a Plex movie/episode/track ratingKey; guid: its Plex guid (a re-added item gets
    -- a new ratingKey, the guid stays)
    key_type        TEXT    NOT NULL CHECK (key_type IN ('rating_key', 'guid')),
    key             TEXT    NOT NULL,
    plays           INTEGER NOT NULL,
    last_watched_at TEXT,
    PRIMARY KEY (integration_id, key_type, key)
) STRICT;

-- Seerr requests (every status is stored; the evaluator counts 1, 2, 4 and 5).
CREATE TABLE seerr_requests (
    integration_id INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    request_id     INTEGER NOT NULL,
    -- 1 pending, 2 approved, 3 declined, 4 failed, 5 completed
    status         INTEGER NOT NULL,
    media_type     TEXT    NOT NULL CHECK (media_type IN ('movie', 'tv')),
    tmdb_id        INTEGER,
    tvdb_id        INTEGER,
    -- media.ratingKey (the show's key for tv): the Plex fallback of the join, used only when the
    -- Seerr integration names its Plex server (settings.plexIntegrationId)
    rating_key     TEXT,
    is_4k          INTEGER NOT NULL DEFAULT 0 CHECK (is_4k IN (0, 1)),
    -- requested season numbers (tv); [] = every season
    seasons        TEXT    NOT NULL DEFAULT '[]',
    -- the Seerr user id only: no name or e-mail is stored (the rule editor asks Seerr for labels)
    user_id        INTEGER NOT NULL,
    requested_at   TEXT,
    PRIMARY KEY (integration_id, request_id)
) STRICT;

CREATE INDEX seerr_requests_tmdb ON seerr_requests (tmdb_id) WHERE tmdb_id IS NOT NULL;
CREATE INDEX seerr_requests_tvdb ON seerr_requests (tvdb_id) WHERE tvdb_id IS NOT NULL;

-- Maintainerr collection members that are pending deletion, or that may be (only those are
-- stored, phase2-3.md §6.2). Rating keys only mean something on one Plex server: they are matched
-- only against the plex_files of plex_integration_id.
CREATE TABLE maintainerr_items (
    integration_id      INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    -- the Plex integration the member's keys belong to: the Maintainerr integration's
    -- plexIntegrationId when it was refreshed
    plex_integration_id INTEGER NOT NULL REFERENCES integrations (id) ON DELETE CASCADE,
    collection_id       INTEGER NOT NULL,
    collection_title    TEXT    NOT NULL,
    -- the collection's Plex library (section key): the tmdb/tvdb fallback applies only to files
    -- in this section
    library_id          TEXT    NOT NULL,
    -- the collection's level; rating_key is the Plex ratingKey at that level (mediaServerId)
    level               TEXT    NOT NULL CHECK (level IN ('movie', 'show', 'season', 'episode')),
    rating_key          TEXT    NOT NULL,
    -- the movie's or the show's ids (Maintainerr stores the show's for seasons and episodes)
    tmdb_id             INTEGER,
    tvdb_id             INTEGER,
    -- from the linked Plex index; NULL when unresolved or not applicable
    season_number       INTEGER,
    episode_number      INTEGER,
    -- pending: Maintainerr will delete it; undecided: a show or season exclusion may cover it but
    -- its ancestors could not be resolved (evaluated as unknown, S14)
    state               TEXT    NOT NULL CHECK (state IN ('pending', 'undecided')),
    -- addDate + deleteAfterDays
    delete_after        TEXT,
    PRIMARY KEY (integration_id, collection_id, rating_key)
) STRICT;

CREATE INDEX maintainerr_items_key ON maintainerr_items (plex_integration_id, rating_key);
CREATE INDEX maintainerr_items_tmdb ON maintainerr_items (integration_id, tmdb_id) WHERE tmdb_id IS NOT NULL;
CREATE INDEX maintainerr_items_tvdb ON maintainerr_items (integration_id, tvdb_id) WHERE tvdb_id IS NOT NULL;

-- Tiers (owned by internal/tiers) -------------------------------------------------------------

-- Ordered rules; the first enabled rule whose conditions are all true decides a file's tier at a
-- destination, unless an earlier unknown rule is more protective; no match (or no rules) = full
-- (phase2-3.md §8.1). The rule set's revision is the setting tiers.revision.
CREATE TABLE tier_rules (
    -- AUTOINCREMENT: rule ids appear in job item details (a dry run's reasons); never reuse one
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    -- evaluation order, ascending; every save rewrites 1..n
    priority        INTEGER NOT NULL,
    name            TEXT    NOT NULL,
    enabled         INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    -- all: every condition true; any: at least one true (unknown is never true)
    match           TEXT    NOT NULL DEFAULT 'all' CHECK (match IN ('all', 'any')),
    -- JSON array of {field, op, value} (phase2-3.md §8.2); [] matches every file
    conditions      TEXT    NOT NULL DEFAULT '[]',
    action          TEXT    NOT NULL CHECK (action IN ('full', 'manifest', 'skip')),
    -- "Applies at". NULL: every destination; a JSON array of destination ids: only those (the rule
    -- is not evaluated elsewhere). Deleting a destination removes its id; an array left empty
    -- makes the rule apply nowhere (never "all")
    destination_ids TEXT,
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL
) STRICT;

CREATE INDEX tier_rules_priority ON tier_rules (priority, id);

-- The manual "irreplaceable" flag (phase2-3.md §8.7).
--   arr:  set on an *arr item. It applies to every non-deleted item of arr_kind, in any
--         integration, that shares an external id with it; arr_id is only a cache. When no item
--         resolves it falls back to its last folder as a path flag. Deleting the integration sets
--         integration_id NULL and keeps the flag.
--   path: a file, or every file under a folder, of a source. It follows paired moves and folder
--         renames (the syncer updates rel_path in the move's transaction).
CREATE TABLE item_flags (
    id             INTEGER PRIMARY KEY,
    flag           TEXT    NOT NULL CHECK (flag IN ('irreplaceable')),
    kind           TEXT    NOT NULL CHECK (kind IN ('arr', 'path')),
    -- arr: the integration the flag was set on (NULL once it is deleted)
    integration_id INTEGER REFERENCES integrations (id) ON DELETE SET NULL,
    arr_kind       TEXT    CHECK (arr_kind IS NULL OR arr_kind IN ('movie', 'series', 'artist')),
    arr_id         INTEGER,
    -- arr: {"tmdb":…,"tvdb":…,"imdb":…,"mbid":…} copied from the index when the flag was set
    external_ids   TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(external_ids)),
    -- arr: the item's folder as last located (refreshed whenever the flag resolves); the fallback
    last_source_id INTEGER REFERENCES sources (id) ON DELETE SET NULL,
    last_rel_path  TEXT,
    -- path: the source and the file or folder (every file under it); "" is the whole source
    source_id      INTEGER REFERENCES sources (id) ON DELETE CASCADE,
    rel_path       TEXT,
    note           TEXT    NOT NULL DEFAULT '',
    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,
    -- integration_id is not required for arr flags: ON DELETE SET NULL must never violate a CHECK
    CHECK (kind <> 'arr' OR (arr_kind IS NOT NULL AND arr_id IS NOT NULL AND source_id IS NULL AND rel_path IS NULL)),
    CHECK (kind <> 'path' OR (source_id IS NOT NULL AND rel_path IS NOT NULL AND integration_id IS NULL
                              AND arr_kind IS NULL AND arr_id IS NULL AND last_source_id IS NULL AND last_rel_path IS NULL))
) STRICT;

CREATE UNIQUE INDEX item_flags_arr ON item_flags (flag, integration_id, arr_kind, arr_id)
    WHERE kind = 'arr' AND integration_id IS NOT NULL;
CREATE UNIQUE INDEX item_flags_path ON item_flags (flag, source_id, rel_path) WHERE kind = 'path';
CREATE INDEX item_flags_last_folder ON item_flags (last_source_id, last_rel_path) WHERE last_source_id IS NOT NULL;

-- Webhooks (owned by internal/webhooks) -------------------------------------------------------

-- Every authenticated webhook call. Payloads are hints (S12): they select what to refresh and
-- scan, never what to delete. Headers, the query (the key) and the client address are never
-- stored. The processor schedules from class and targets alone and never parses payload again.
CREATE TABLE webhook_events (
    id             INTEGER PRIMARY KEY,
    integration_id INTEGER REFERENCES integrations (id) ON DELETE SET NULL,
    source         TEXT    NOT NULL CHECK (source IN ('sonarr', 'radarr', 'lidarr')),
    -- eventType as sent (Test, Download, Rename, MovieFileDelete, ...)
    event_type     TEXT    NOT NULL,
    -- how the processor schedules it (phase2-3.md §7.2, §7.3): change and download wait 5 s (at
    -- most 20 s), delete (deletedFiles: true) 60 s, upgrade_delete until its Download or 30 min
    class          TEXT    NOT NULL CHECK (class IN ('test', 'ignored', 'change', 'download', 'delete', 'upgrade_delete')),
    -- JSON array of the *arr item ids the event names (movie.id, series.id, artist.id), parsed at
    -- intake
    targets        TEXT    NOT NULL DEFAULT '[]',
    -- the compact body of a handled event when it is at most 64 KiB; a larger one is stored as
    -- {"truncated":true,"eventType":…,"ids":[…]} (truncated = 1); ignored event types store {}
    payload        TEXT    NOT NULL CHECK (length(CAST(payload AS BLOB)) <= 65536),
    truncated      INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
    received_at    TEXT    NOT NULL,
    -- set once the event's work is queued (or it was ignored); unprocessed events are picked up
    -- again after a restart, and cleared again when the refresh they were queued into is cancelled
    processed_at   TEXT,
    -- test: a Test event; ignored: an event type Bunkarr does not act on; queued: a new refresh
    -- job was queued; coalesced: an existing queued job absorbed it; failed: no item id, or the
    -- enqueue failed
    outcome        TEXT    CHECK (outcome IS NULL OR outcome IN ('test', 'ignored', 'queued', 'coalesced', 'failed')),
    -- the refresh job the event was queued into (informational; not a foreign key: job history is pruned)
    job_id         INTEGER,
    CHECK ((processed_at IS NULL) = (outcome IS NULL))
) STRICT;

CREATE INDEX webhook_events_pending ON webhook_events (id) WHERE processed_at IS NULL;
CREATE INDEX webhook_events_integration ON webhook_events (integration_id, received_at);
CREATE INDEX webhook_events_received ON webhook_events (received_at);
CREATE INDEX webhook_events_job ON webhook_events (job_id) WHERE job_id IS NOT NULL;

-- Manifests (owned by internal/manifest) ------------------------------------------------------

-- Manifest versions written to a destination (.bunkarr/manifests/<version>/).
CREATE TABLE manifests (
    id             INTEGER PRIMARY KEY,
    destination_id INTEGER NOT NULL REFERENCES destinations (id) ON DELETE CASCADE,
    job_id         INTEGER REFERENCES jobs (id) ON DELETE SET NULL,
    created_at     TEXT    NOT NULL,
    -- version directory relative to the destination target
    path           TEXT    NOT NULL,
    format         INTEGER NOT NULL,
    item_count     INTEGER NOT NULL,
    file_count     INTEGER NOT NULL,
    -- bytes of the files listed
    bytes          INTEGER NOT NULL,
    -- "sha256:<hex>" of manifest.json as written (also in the version's SHA256SUMS)
    checksum       TEXT    NOT NULL,
    -- "sha256:<hex>" of the canonical content without createdAt, job and the integrations'
    -- refreshedAt, appVersion and lastError: an export whose content equals the newest ok
    -- version's (read back and verified first) writes no new version
    content_hash   TEXT    NOT NULL,
    -- damaged: the version's files were missing or did not match checksum when read back (the
    -- export then writes a new version; downloads answer 409)
    integrity      TEXT    NOT NULL DEFAULT 'ok' CHECK (integrity IN ('ok', 'damaged'))
) STRICT;

CREATE UNIQUE INDEX manifests_path ON manifests (destination_id, path);
CREATE INDEX manifests_destination ON manifests (destination_id, created_at);
