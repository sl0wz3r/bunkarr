# Bunkarr

**Your library's bunker.** Bunkarr is a self-hosted, *arr-style backup app for Plex media libraries
and the *arr stack. It knows which media can be re-downloaded:

- **Irreplaceable data** (the Plex database, Sonarr/Radarr configuration and databases, personal
  media, rare content) gets **full, versioned backups**.
- **Common, easily re-acquired content** gets a **manifest-only backup** (TMDB/TVDB/IMDb IDs,
  quality profile, root folder, path), restorable by having Sonarr/Radarr download it again.

Bunkarr never modifies or deletes your source media.

> **Status: early development (Phase 0 — foundation).** The server, login, API key, settings
> store and web UI shell work; backups do not exist yet. See [the roadmap](#roadmap).

## Quick start (Docker Compose)

```sh
git clone https://github.com/sl0wz3r/bunkarr.git
cd bunkarr
docker compose -f deploy/docker-compose.yml up -d --build
```

Open `http://<host>:8787`. On first run Bunkarr asks you to create a username and password; the
UI and API are closed until you do (the API key works from the start for automation).

Edit [`deploy/docker-compose.yml`](deploy/docker-compose.yml) first:

| Mount | Purpose |
|---|---|
| `/config` | Bunkarr's database, `bunkarr.key`, logs. **Back up `bunkarr.key` with the database** — stored credentials cannot be decrypted without it. |
| `/media` (read-only) | Your library. Mount the folder that holds both downloads and library (e.g. `/mnt/user/data`) as one volume so hardlinks are detected. |
| `/backup` | A backup destination, e.g. a UniFi UNAS share mounted on the host. |

| Variable | Default | |
|---|---|---|
| `PUID` / `PGID` | `1000` / `1000` | User/group Bunkarr runs as (Unraid: `99` / `100`). |
| `UMASK` | `002` | |
| `TZ` | `Etc/UTC` | e.g. `America/New_York`. |
| `BUNKARR_PORT` | `8787` | Web UI and API port. |
| `BUNKARR_BIND` | all interfaces | Listen address. |
| `BUNKARR_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `BUNKARR_LOG_FORMAT` | `text` | stdout format (`text` or `json`); `/config/logs/bunkarr.log` is always JSON. |

### Locked out?

```sh
docker exec -it bunkarr /entrypoint.sh reset-auth
```

This removes the login (not the API key or anything else); the next visit shows the first-run setup.

## API

REST under `/api/v1`, JSON, *arr-style authentication: the API key from **Settings → General** in
the `X-Api-Key` header or the `apikey` query parameter.

```sh
curl -H "X-Api-Key: $KEY" http://localhost:8787/api/v1/system/status
```

- `GET /api/v1/health` — unauthenticated liveness check.
- `GET /api/v1/openapi.json` — the OpenAPI 3.1 description (a test keeps it in step with the router).

## Roadmap

| Phase | |
|---|---|
| 0 | Foundation: server, auth, settings, UI shell, image, CI ✅ |
| 1 | MVP: Plex integration, scanner with hardlink detection, file-copy engine, scheduled syncs, Plex DB backup, activity/history, Apprise notifications |
| 2 | *arr awareness: Sonarr/Radarr APIs and webhooks, *arr config backups, manifest export |
| 3 | Tiering: rule engine (tags, quality, Tautulli, Seerr, Maintainerr), plan preview |
| 4 | Destinations & versioning: restic and rclone engines, B2/S3/SFTP, bandwidth windows |
| 5 | Restore & disaster recovery: restore wizard, manifest re-acquisition, restore tests |
| 6 | Release polish: Unraid CA template, metrics, notifications, docs site, hardening |

Decisions are recorded in [`docs/adr`](docs/adr); postponed items with reasons in
[`DEFERRED.md`](DEFERRED.md).

## Development

Requirements: Go 1.27, Node 24, make.

```sh
make web build        # UI into web/dist, then bin/bunkarr embedding it
make run              # serve on :8787 with ./config
make test lint        # Go (race) + web tests; gofmt, vet, typecheck, shellcheck
make docker-test      # build the image and smoke-test it
cd web && npm run dev # UI dev server on :5173, proxying /api to :8787
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[GPL-3.0-or-later](LICENSE), like the *arr ecosystem.
