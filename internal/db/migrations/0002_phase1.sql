-- Phase 1: integrations, sources and catalog, destinations, jobs and schedules, Plex DB snapshots,
-- notifications. Times are RFC3339 UTC text (db.TimeFormat); JSON columns are text. Contract:
-- docs/design/phase1.md.

CREATE TABLE integrations (
    id         INTEGER PRIMARY KEY,
    type       TEXT    NOT NULL CHECK (type IN ('plex', 'sonarr', 'radarr', 'lidarr', 'tautulli', 'seerr', 'maintainerr')),
    name       TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    url        TEXT    NOT NULL,
    -- sealed with AAD "integration:<id>:apiKey"; '' = none
    api_key    TEXT    NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    settings   TEXT    NOT NULL DEFAULT '{}',
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
) STRICT;

CREATE TABLE sources (
    id                  INTEGER PRIMARY KEY,
    name                TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    -- absolute path as Bunkarr sees it (inside the container)
    path                TEXT    NOT NULL,
    -- folder under each destination target that mirrors this source
    dest_folder         TEXT    NOT NULL,
    -- JSON array of glob patterns matched against the relative path and the base name
    exclude             TEXT    NOT NULL DEFAULT '[]',
    enabled             INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    plex_integration_id INTEGER REFERENCES integrations (id) ON DELETE SET NULL,
    plex_section_id     TEXT,
    -- the library location as Plex sees it
    plex_path           TEXT,
    arr_integration_id  INTEGER REFERENCES integrations (id) ON DELETE SET NULL,
    last_scan_at        TEXT,
    created_at          TEXT    NOT NULL,
    updated_at          TEXT    NOT NULL
) STRICT;

CREATE TABLE destinations (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    engine      TEXT    NOT NULL CHECK (engine IN ('filecopy', 'restic', 'rclone')),
    target      TEXT    NOT NULL,
    -- uuid written to <target>/.bunkarr/destination.json (safety rule S3)
    marker_id   TEXT    NOT NULL,
    -- sealed JSON (Phase 4 engines); '' = none
    credentials TEXT    NOT NULL DEFAULT '',
    -- {"verify":{"mode":"off|sample|full","samplePercent":5},"hardlinks":"recreate|copy","adoptExisting":true}
    settings    TEXT    NOT NULL DEFAULT '{}',
    -- {"deletedDays":30,"plexDbVersions":14}
    retention   TEXT    NOT NULL DEFAULT '{}',
    -- Phase 4: bandwidth limits and windows
    bandwidth   TEXT    NOT NULL DEFAULT '{}',
    -- probed hardlink support: NULL unknown, 0 no, 1 yes
    hardlinks_supported INTEGER CHECK (hardlinks_supported IN (0, 1)),
    enabled     INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at  TEXT    NOT NULL,
    updated_at  TEXT    NOT NULL
) STRICT;

CREATE TABLE destination_sources (
    destination_id INTEGER NOT NULL REFERENCES destinations (id) ON DELETE CASCADE,
    source_id      INTEGER NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    PRIMARY KEY (destination_id, source_id)
) STRICT;

CREATE TABLE schedules (
    id          INTEGER PRIMARY KEY,
    job_type    TEXT    NOT NULL CHECK (job_type IN ('scan', 'sync', 'plexdb_backup', 'retention', 'verify')),
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

CREATE TABLE catalog_files (
    id             INTEGER PRIMARY KEY,
    source_id      INTEGER NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    rel_path       TEXT    NOT NULL,
    size           INTEGER NOT NULL,
    mtime_ns       INTEGER NOT NULL,
    dev            INTEGER NOT NULL,
    inode          INTEGER NOT NULL,
    nlink          INTEGER NOT NULL,
    -- "<dev>:<inode>" when the inode has more than one name; NULL otherwise
    hardlink_group TEXT,
    -- "sha256:<hex>" of the content at (size, mtime_ns) = (hash_size, hash_mtime_ns)
    hash           TEXT,
    hash_size      INTEGER,
    hash_mtime_ns  INTEGER,
    media_ids      TEXT    NOT NULL DEFAULT '{}',
    arr_item_id    INTEGER,
    tier           TEXT    NOT NULL DEFAULT 'full' CHECK (tier IN ('full', 'manifest', 'skip')),
    first_seen_at  TEXT    NOT NULL,
    last_seen_at   TEXT    NOT NULL,
    -- set when a scan no longer finds the file; cleared if it reappears
    deleted_at     TEXT,
    UNIQUE (source_id, rel_path)
) STRICT;

CREATE INDEX catalog_files_inode ON catalog_files (dev, inode);
CREATE INDEX catalog_files_hardlink_group ON catalog_files (hardlink_group) WHERE hardlink_group IS NOT NULL;
CREATE INDEX catalog_files_deleted ON catalog_files (source_id, deleted_at);

CREATE TABLE jobs (
    id             INTEGER PRIMARY KEY,
    type           TEXT    NOT NULL CHECK (type IN ('scan', 'sync', 'plexdb_backup', 'retention', 'verify')),
    status         TEXT    NOT NULL CHECK (status IN ('queued', 'running', 'completed', 'completed_with_warnings', 'failed', 'cancelled')),
    trigger        TEXT    NOT NULL CHECK (trigger IN ('schedule', 'manual', 'webhook', 'resume', 'startup')),
    dry_run        INTEGER NOT NULL DEFAULT 0 CHECK (dry_run IN (0, 1)),
    params         TEXT    NOT NULL DEFAULT '{}',
    -- denormalized from params for per-destination serialization and filtering
    destination_id INTEGER,
    attempt        INTEGER NOT NULL DEFAULT 1,
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

CREATE INDEX jobs_status ON jobs (status, queued_at);
CREATE INDEX jobs_finished ON jobs (finished_at) WHERE finished_at IS NOT NULL;
CREATE INDEX jobs_destination ON jobs (destination_id, queued_at);

CREATE TABLE job_items (
    id       INTEGER PRIMARY KEY,
    job_id   INTEGER NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    -- catalog_files.id or destination_files.id, depending on the action; informational
    file_id  INTEGER,
    rel_path TEXT    NOT NULL,
    action   TEXT    NOT NULL CHECK (action IN ('copy', 'update', 'adopt', 'link', 'retain', 'expire', 'verify', 'backup', 'skip')),
    status   TEXT    NOT NULL CHECK (status IN ('pending', 'done', 'failed', 'skipped')),
    bytes    INTEGER NOT NULL DEFAULT 0,
    error    TEXT,
    -- runner-specific JSON (source path, temp path, link target, reason)
    detail   TEXT    NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX job_items_job_status ON job_items (job_id, status, id);
CREATE INDEX job_items_job_action ON job_items (job_id, action, id);

CREATE TABLE job_logs (
    id      INTEGER PRIMARY KEY,
    job_id  INTEGER NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    at      TEXT    NOT NULL,
    level   TEXT    NOT NULL CHECK (level IN ('debug', 'info', 'warn', 'error')),
    message TEXT    NOT NULL,
    fields  TEXT    NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX job_logs_job ON job_logs (job_id, id);

-- What is at each destination (owned by internal/syncer).
CREATE TABLE destination_files (
    id             INTEGER PRIMARY KEY,
    destination_id INTEGER NOT NULL REFERENCES destinations (id) ON DELETE CASCADE,
    source_id      INTEGER REFERENCES sources (id) ON DELETE SET NULL,
    -- relative to the destination target: "<destFolder>/<relPath>"
    rel_path       TEXT    NOT NULL,
    size           INTEGER NOT NULL,
    mtime_ns       INTEGER NOT NULL,
    hash           TEXT,
    src_dev        INTEGER,
    src_inode      INTEGER,
    -- the destination_files row this path is a hardlink of (recreated or only recorded)
    link_of        INTEGER REFERENCES destination_files (id) ON DELETE SET NULL,
    -- present: a file at rel_path; linked: a hardlink at rel_path to link_of; link_recorded: no
    -- file at rel_path (destination has no hardlinks), content is link_of's; retained: moved to
    -- retained_path until expires_at
    state          TEXT    NOT NULL CHECK (state IN ('present', 'linked', 'link_recorded', 'retained')),
    retained_path  TEXT,
    job_id         INTEGER,
    copied_at      TEXT,
    verified_at    TEXT,
    retained_at    TEXT,
    expires_at     TEXT
) STRICT;

CREATE UNIQUE INDEX destination_files_live ON destination_files (destination_id, rel_path)
    WHERE state IN ('present', 'linked', 'link_recorded');
CREATE INDEX destination_files_expiry ON destination_files (destination_id, expires_at) WHERE state = 'retained';
CREATE INDEX destination_files_src_inode ON destination_files (destination_id, src_dev, src_inode);

-- Plex DB versions (owned by internal/plexdb).
CREATE TABLE snapshots (
    id                 INTEGER PRIMARY KEY,
    destination_id     INTEGER NOT NULL REFERENCES destinations (id) ON DELETE CASCADE,
    job_id             INTEGER REFERENCES jobs (id) ON DELETE SET NULL,
    kind               TEXT    NOT NULL CHECK (kind IN ('plexdb')),
    integration_id     INTEGER REFERENCES integrations (id) ON DELETE SET NULL,
    -- version directory relative to the destination target
    engine_snapshot_id TEXT    NOT NULL,
    created_at         TEXT    NOT NULL,
    size               INTEGER NOT NULL,
    method             TEXT    NOT NULL,
    integrity          TEXT    NOT NULL,
    manifest           TEXT    NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX snapshots_destination ON snapshots (destination_id, kind, created_at);

CREATE TABLE notifications (
    id         INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    kind       TEXT    NOT NULL CHECK (kind IN ('apprise')),
    -- {"apiUrl":"http://apprise:8000","configKey":""}
    settings   TEXT    NOT NULL DEFAULT '{}',
    -- sealed with AAD "notification:<id>:urls"; '' = none
    secret     TEXT    NOT NULL DEFAULT '',
    on_failure INTEGER NOT NULL DEFAULT 1 CHECK (on_failure IN (0, 1)),
    on_warning INTEGER NOT NULL DEFAULT 1 CHECK (on_warning IN (0, 1)),
    on_success INTEGER NOT NULL DEFAULT 0 CHECK (on_success IN (0, 1)),
    enabled    INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
) STRICT;

-- Daily expiry of retained files and old job history.
INSERT INTO schedules (job_type, params, cron, enabled, created_at, updated_at)
VALUES ('retention', '{}', '30 4 * * *', 1, strftime('%Y-%m-%dT%H:%M:%S.000000000Z', 'now'), strftime('%Y-%m-%dT%H:%M:%S.000000000Z', 'now'));
