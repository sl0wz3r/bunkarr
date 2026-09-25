# Contributing to Bunkarr

Thanks for helping. Bunkarr handles people's backups, so safety and clarity come before features.

## Ground rules

- **Never write to or delete from source paths.** Source media is read-only in every example and
  test; code that could touch a source path needs discussion first.
- **Dry run everywhere.** Any sync or restore must be previewable before it runs.
- **Secrets** (API keys, tokens, passwords) are encrypted at rest and never logged. Use
  `internal/logging` (it redacts secret-looking attributes and query parameters).
- **No placeholder stubs.** A merged feature is either implemented or listed in
  [DEFERRED.md](DEFERRED.md) with the reason.

## Setup

Go 1.27, Node 24, make, and optionally Docker and shellcheck.

```sh
make web build   # UI + binary
make test lint   # everything CI runs, except the image test
make docker-test # image smoke test (needs Docker)
```

The Go binary embeds `web/dist`; a Go-only build works (the UI then answers with a notice).

## Layout

```
cmd/bunkarr/         main: serve, version, healthcheck, reset-auth
internal/api/        HTTP handlers, router, openapi.json, SPA serving
internal/auth/       users, sessions, API key, login limiter, middleware
internal/config/     bootstrap env, master key + secret sealing, settings store
internal/db/         SQLite (modernc), embedded migrations
internal/logging/    slog setup, rotation, redaction
internal/lock/       single instance per config directory
web/                 React + TypeScript + Vite + Tailwind UI
deploy/              docker-compose example
docker/              entrypoint and image smoke test
docs/adr/            architecture decision records
```

## Conventions

- **Commits:** [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`,
  `docs:`, `test:`, `chore:`, `refactor:`, `ci:`).
- **Changelog:** update `CHANGELOG.md` (Keep a Changelog) in the same change.
- **Decisions:** anything significant gets a short ADR in `docs/adr/NNNN-title.md`.
- **API:** every route under `/api/v1` must be in `internal/api/openapi.json`;
  `TestOpenAPIMatchesRoutes` fails otherwise.
- **Migrations:** add `internal/db/migrations/NNNN_description.sql`; never edit a released one.
- **Dependencies:** prefer the standard library. Justify each new dependency in the commit
  message. Current Go dependencies:
  - `github.com/go-chi/chi/v5` — router (spec'd stack; route groups and middleware).
  - `modernc.org/sqlite` — pure-Go SQLite, keeps `CGO_ENABLED=0` static builds.
  - `golang.org/x/crypto` — bcrypt for the login password.
- **Tests:** Go tests with `-race`; UI tests with Vitest and Testing Library. Security-relevant
  behaviour (auth, redaction, CSRF) needs a test.
- **Actions and base images** are pinned by commit SHA / digest; Renovate proposes updates.

## Security

Please report vulnerabilities privately through GitHub's "Report a vulnerability" (Security tab),
not in a public issue.

## License

By contributing you agree that your contributions are licensed under GPL-3.0-or-later.
