# Contributing to Bunkarr

Thanks for helping. Bunkarr handles people's backups, so safety and clarity come before features.

## Ground rules

- **Never write to or delete from source paths.** Source media is read-only in every example and
  test; code that could touch a source path needs discussion first.
- **Dry run everywhere.** Any sync, deletion (retention expiry) or restore must be previewable
  before it runs; every scheduled task has a Preview in System → Tasks.
- **Secrets** (API keys, tokens, passwords) are encrypted at rest and never logged. Use
  `internal/logging` (it redacts secret-looking attributes and query parameters, and registered
  secret values). A stored secret is only ever sent to the URL it was saved with.
- **No placeholder stubs.** A merged feature is either implemented or listed in
  [DEFERRED.md](DEFERRED.md) with the reason.
- **The design is the contract.** Phase work is built against `docs/design/phaseN.md`; a change
  to a contract there is made in the same change as the code.

## Setup

Go 1.27, Node 24, make, and optionally Docker and shellcheck.

```sh
make web build   # UI + binary
make test lint   # Go (race) and web tests; gofmt, vet (incl. the e2e files), typecheck, shellcheck
make test-e2e    # acceptance suite against the real binary (no Docker; a few minutes)
make docker-test # image smoke test (needs Docker)
make test-docker # image smoke, container kill, Plex backup/restore and share tests (Docker and Go)
make test-plex   # the Plex backup/restore test only (slow; pulls plexinc/pms-docker once)
make test-shares # sync and kill tests on Samba (CIFS) and NFS shares (privileged containers)
make test-arr    # real Sonarr, Radarr and Lidarr containers, incl. tiers (Docker, Go and internet; slow)
make help        # every target
```

`make test-arr` (`docker/test-arr.sh`) builds the image and runs the *arr acceptance tests
against real Sonarr, Radarr and Lidarr containers of pinned versions (`SONARR_IMAGE`,
`RADARR_IMAGE`, `LIDARR_IMAGE` override them): imports and upgrades posted as webhooks with the
integration's webhook key must reach the destination by a targeted sync within 60 s, a manifest
must round-trip against the *arr's own API state, tier rules must decide from the real Radarr's
tags and quality profiles (`TestDockerArrTiers`, with fake Plex, Tautulli, Seerr and Maintainerr),
and `internal/arrbackup`'s backup tests run with the Backups folder mounted read-only and a login
required. It needs internet (the *arrs'
metadata lookups); without it the tests skip. It removes its containers, volumes and network. It
is not in CI yet ([DEFERRED.md](DEFERRED.md)): run it before merging a change to webhooks,
*arr backups, manifests, the metadata index, targeted syncs, tiers or the *arr client.

The *arr client's tests use `internal/integrations/arr/arrtest`, a fake that serves responses
recorded from the real apps under `testdata/arr/<app>/` (`testdata/arr/record_slice2.py`
records more). Add a recording rather than a hand-written response when the client learns a new
request. Tautulli, Seerr and Maintainerr work the same way: `tautullitest`, `seerrtest` and
`maintainerrtest` serve `testdata/tautulli/`, `testdata/seerr/` and `testdata/maintainerr/`.

Tier facts and decisions are table-tested in `internal/tiers` (the evaluator is pure: facts in,
decision out); the end-to-end tier cases, including stale caches and releases, are
`TestTiersE2E` in `internal/e2e`. The syncer's package tests are slow: run a subset with
`go test -short -run <Name> ./internal/syncer/` while you work.

The Go binary embeds `web/dist`; a Go-only build works (the UI then answers with a notice). Build
the UI with `make web`: `npm run build` empties `web/dist`, including the tracked `.gitkeep`,
which `make web` puts back.

CI runs lint, the race tests, the e2e suite, govulncheck and npm audit on every push, the image
build with the smoke, kill and share tests, and the Plex test on `workflow_dispatch` and tags.
The release workflow tags an image only after the e2e suite and the whole Docker suite pass
against it. The GitHub and Gitea workflows are kept identical below their headers.

## Layout

```
cmd/bunkarr/               main: serve, version, healthcheck, reset-auth
internal/api/              HTTP handlers, router, openapi.json, SPA serving, Plex sign-in, the
                           webhook routes' auth; App (app.go) wires the services for main and the
                           tests
internal/auth/             users, sessions, API key, login limiter, middleware
internal/config/           bootstrap env, master key + secret sealing, settings store
internal/db/               SQLite (modernc), embedded migrations, pre-migration copies
internal/logging/          slog setup, rotation, redaction (key names and secret values)
internal/lock/             single instance per config directory
internal/jobs/             the job contract shared by runners (types and interfaces only)
internal/jobqueue/         job manager, scheduler (robfig/cron), jobs/items/logs/schedules store
internal/faultinject/      named fault points for the crash matrix and kill tests
internal/integrations/     integrations store, Plex and *arr settings, path mappings, webhook keys,
                           the *arr connection test
internal/integrations/plex/ Plex client, plex.tv sign-in (PINs, resources), connection probe;
                           plextest/ fake server; testdata/ recorded responses
internal/integrations/arr/ read-only Sonarr/Radarr/Lidarr client (fixed request allow-list);
                           arrtest/ fake *arr serving testdata/arr/ recordings
internal/integrations/tautulli/, seerr/, maintainerr/
                           read-only clients (fixed request allow-lists, paging integrity);
                           *test/ fakes serving testdata/<app>/ recordings
internal/netguard/         dialer for every outbound client: refuses link-local and cloud metadata
                           addresses
internal/mediaindex/       metadata index of the *arrs' items and files, the Plex library index and
                           the Tautulli, Seerr and Maintainerr caches; refresh runner, reconcile,
                           follow-up syncs
internal/tiers/            tier rules store (revisions), facts, the pure evaluator, presets,
                           preview, irreplaceable flags
internal/webhooks/         *arr webhook intake (webhook_events), parsing, the coalescing processor
internal/arrbackup/        *arr config backup runner: fetch (folder or HTTP), zip verification,
                           versions (snapshots kind arr)
internal/manifest/         manifest build (JSON + CSV, streamed), export runner, parse, re-import
                           plan and compare; manifesttest/ independent *arr state decoder
internal/snapshots/        snapshots table and the versioned-backup rules shared by plexdb and
                           arrbackup (partial recovery, keep daily/weekly, prune)
internal/testhooks/        values an e2e build (-tags e2e) may override: webhook windows, plex.tv
                           URLs, clock skew; constants in production builds
internal/version/          build information set at link time
internal/catalog/          sources, read-only scanner, hardlink groups, catalog queries, scan runner
internal/destinations/     destinations store, marker, capability probe, Open (safety rule S3)
internal/engines/filecopy/ filesystem primitives over os.Root: atomic copy, link, retain, expire, hashes
internal/syncer/           planner and the sync, verify and retention runners (destination_files);
                           targeted syncs (Params.Paths); tiers in a sync (kept files, releases)
internal/plexdb/           Plex DB backup runner, verification, version pruning (snapshots)
internal/notify/           Apprise targets and the notification dispatcher
internal/e2e/              acceptance suite (build tag e2e): binary and Docker tests, including the
                           *arr suite (TestDockerArr*)
testdata/arr/, testdata/webhooks/  recorded *arr API responses and webhook payloads
testdata/tautulli/, seerr/, maintainerr/  recorded Tautulli, Seerr and Maintainerr responses
web/                       React + TypeScript + Vite + Tailwind UI
deploy/                    docker-compose example
docker/                    entrypoint; image smoke, container kill, Plex restore and share test wrappers
docs/design/               phase designs (the contracts packages are built against)
docs/adr/                  architecture decision records
docs/spikes/               spike reports
```

Each package owns its tables' SQL; other packages use its exported API
(`docs/design/phase1.md` §8, `docs/design/phase2-3.md` §14.2). The one exception is
`internal/manifest/queries.go`, which reads the catalog, syncer and destinations tables directly
and read-only, inside the manifest's single read transaction, until those packages expose
manifest queries.

## Conventions

- **Commits:** [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`,
  `docs:`, `test:`, `chore:`, `refactor:`, `ci:`).
- **Changelog:** update `CHANGELOG.md` (Keep a Changelog) in the same change.
- **Decisions:** anything significant gets a short ADR in `docs/adr/NNNN-title.md`.
- **API:** every route under `/api/v1` must be in `internal/api/openapi.json`;
  `TestOpenAPIMatchesRoutes` fails otherwise.
- **Migrations:** add `internal/db/migrations/NNNN_description.sql`; never edit a released one.
- **Destination writes** go only through `internal/engines/filecopy` on the job's `os.Root` from
  `destinations.Open`, never through a raw path. Classify errors with `filecopy.Classify`
  (fatal stops the job, anything else fails one item).
- **Write order in runners:** save the intent in the item detail (temp path, retention path)
  before the filesystem step; before a file that has a record moves into retention, also put the
  retention intent on the record (`retained_path`/`reason`); then filesystem → database record →
  `ItemStore.Finish`. Every item re-checks its state when it runs, so running it twice is
  harmless, and an intent its job never finishes is settled by the next job (design §4.2).
  Writes after cancellation use `context.WithoutCancel`.
- **Secrets in logs:** a stored secret is held with `logging.SetSecrets(owner, values...)` (per
  row, released when the row changes); a secret Bunkarr reads but does not store with
  `logging.RegisterSecret`; a value held for one request (a Test form) is redacted with
  `logging.RedactValues` and never registered.
- **Outbound HTTP:** every client that calls another service (Plex, plex.tv, the *arrs,
  Tautulli, Seerr, Maintainerr, Apprise) uses `netguard.NewTransport()`. The *arr, Tautulli, Seerr
  and Maintainerr clients have no general request method: a new request is a new method on the
  allow-list in its package, read-only unless the design says otherwise (the *arr Backup command
  is the only write). Decode only the fields the design lists (no Seerr names or e-mail
  addresses).
- **Tiers never lower protection:** an unknown fact must never make a file less than `full`
  when a more protective rule might match, and demoting a file never removes it from a
  destination; only a confirmed release does (design §8, S14, S15). A change to the evaluator
  or to tiers in a sync needs a test of both.
- **Webhooks are hints:** a webhook payload only chooses which *arr items to refresh; paths and
  files always come from the *arr's API and a scan (design S12).
- **Test hooks:** values an end-to-end test must change in a real binary (timings, plex.tv URLs)
  go through `internal/testhooks`, honoured only with `-tags e2e`; never read such an override
  from the environment in production code.
- **Dependencies:** prefer the standard library. Justify each new dependency in the commit
  message. Current Go dependencies:
  - `github.com/go-chi/chi/v5` — router (spec'd stack; route groups and middleware).
  - `modernc.org/sqlite` — pure-Go SQLite, keeps `CGO_ENABLED=0` static builds.
  - `golang.org/x/crypto` — bcrypt for the login password.
  - `github.com/robfig/cron/v3` — cron expression parsing for schedules.
  - `golang.org/x/sys` — system calls the standard library lacks: `fstatfs`, no-replace rename
    (`renameat2`, `renameatx_np`), page-cache dropping (`fadvise`, `F_NOCACHE`), `access`,
    device numbers.

  Web runtime dependencies: React, React Router, `@tanstack/react-query` (polling and caching)
  and `lucide-react` (icons).
- **Tests:** Go tests with `-race`; UI tests with Vitest and Testing Library. Security-relevant
  behaviour (auth, redaction, CSRF, where secrets are sent) needs a test.
- **Actions and base images** are pinned by commit SHA / digest; Renovate proposes updates.

### Fault points and the crash matrix

Crash safety is tested by stopping the program at named step boundaries.

- Code that changes the destination or the job state calls `faultinject.Point("<step>.<when>")`
  at each boundary (`copy.afterWrite`, `update.afterRenameOld`, `plexdb.beforeRecord`, …).
  Document each point where it is defined (exported `Point…` constants in syncer, plexdb,
  jobqueue, db, integrations, mediaindex, webhooks, arrbackup and manifest; the package comment
  in filecopy). In production `Point` is one atomic load;
  injection is always compiled in and armed only by a test hook or the environment.
- **Go crash matrix** (`internal/syncer` `TestCrashMatrix`, `TestCrashMatrixResumePaths` and the
  targeted-sync matrix, and the matrices of plexdb, arrbackup, manifest and mediaindex): a clean run counts how often each listed point is reached; then, for every
  occurrence, `faultinject.SetHook(faultinject.CrashAt(point, n))` makes the run panic with
  `faultinject.Crash`, the harness recovers it, discards in-memory state and resumes the same job
  (attempt 2, trigger `resume`). The result must converge: the destination verifies, no temp or
  partial file, records match the files, no item pending, every old version in retention, and
  the next sync plans nothing. Reset the hook with `defer faultinject.SetHook(nil)`.
- A **new fault point** goes into the matrix's point list; the matrix fails when a listed point
  is never reached. `go test -short` runs only the first and last occurrence of each point; CI
  runs them all.
- `internal/jobqueue` treats a runner panic with `faultinject.Crash` as a simulated process
  crash: the job stays `running` and the next manager's `Start` recovers it.
- **Kill tests** (`internal/e2e`, Docker kill test): `BUNKARR_FAULTPOINT=<point>` and
  `BUNKARR_FAULTPOINT_FILE=<path>` make the process write the file and block at that point; the
  test kills it with SIGKILL and restarts it without the variable.

## Security

Please report vulnerabilities privately through GitHub's "Report a vulnerability" (Security tab),
not in a public issue.

## License

By contributing you agree that your contributions are licensed under GPL-3.0-or-later.
