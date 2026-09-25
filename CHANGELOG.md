# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Phase 0 foundation.
- Go server (`cmd/bunkarr`) with `serve`, `version`, `healthcheck` and `reset-auth` commands,
  graceful shutdown and a single-instance lock on the config directory.
- SQLite database (pure Go, WAL mode, separate writer and read-only pools) with embedded,
  versioned migrations; refuses a database written by a newer version.
- Settings store with secrets sealed at rest (AES-256-GCM, key derived with HKDF from a generated
  master key in `/config/bunkarr.key`, bound to the setting name).
- Authentication: first-run setup of the one UI user (bcrypt), forms login with server-side
  sessions, API key (`X-Api-Key` or `?apikey=`, generated on first run, regenerable), optional
  "disabled for local addresses", login rate limiting, cross-origin (CSRF) protection, change of
  username/password that ends other sessions.
- API: `GET /api/v1/health`, `GET /api/v1/system/status`, auth endpoints, Settings > General, and
  `GET /api/v1/openapi.json` (checked against the router by a test).
- JSON logs in `/config/logs` with size rotation plus stdout; secrets redacted from both.
- Web UI shell (React, TypeScript, Vite, Tailwind): login and first-run setup pages, *arr-style
  left navigation (Activity, Library, Destinations, Settings, System), Settings > General (API
  key, authentication mode, login) and System > Status.
- Multi-arch Docker image (alpine, tini, su-exec, restic, rclone) with PUID/PGID/UMASK/TZ,
  healthcheck and an image smoke test; docker-compose example.
- CI on GitHub and Gitea (typecheck, tests, race tests, gofmt, vet, govulncheck, npm audit,
  shellcheck, image build and test); release workflow pushing multi-arch images to GHCR on tags.
