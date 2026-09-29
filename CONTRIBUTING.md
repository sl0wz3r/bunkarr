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
make test-engines # real restic and rclone against MinIO and an SFTP server, in containers (Docker)
make test-offsite # off-site acceptance of the image on MinIO + SFTP (Docker and Go; ~30 min)
make ca-validate # the Unraid template and ca_profile.xml are current and pass the CA rules (xmllint)
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

restic and rclone run only through `internal/engines/proc`'s `Runner`, which checks every command
against the allow-list in `allowlist.go` (the subcommands and their exact flags) before anything
runs: a new subcommand or flag goes into that list, with a test. Secrets reach the child in files
and environment variables, never on argv. Unit tests use `internal/engines/enginetest`:
`FakeRunner` answers engine commands with scripts or with the output recorded by the engine spike
under `testdata/restic/` and `testdata/rclone/` (each `index.json` names the versions and servers
recorded against); add a recording rather than hand-written output when a driver parses something
new. Crash safety of the engines is `TestCrashMatrixEngines` and `TestCrashMatrixRetention` in
`internal/enginerun` (fakes, no Docker).

`make test-engines` (`docker/test-engines.sh`) runs the tests with build tag `enginebin` of
`internal/engines/...` and `internal/enginerun/...` in a Go container with the pinned restic and
rclone (`docker/engines/`), against MinIO and an OpenSSH SFTP server; `GOTESTFLAGS="-run X -v"`
narrows it. `make test-offsite` (`docker/test-offsite.sh`) builds a test image from the image under
test (an e2e build of the checkout, the argv shims of `docker/offsite/`) and runs
`TestDockerOffsite*` in `internal/e2e` against MinIO and SFTP: lifecycle, crash and resume,
retention, bandwidth and windows, two destinations, two containers on one repository, config
versions, the password gate on off-site routes, a recovery kit restored in a fresh container, and
a secrets audit of argv, logs and files. Run one test with
`GOTESTFLAGS="-run TestDockerOffsiteKit"`; `BUNKARR_E2E_OFFSITE_HOLD=15m` keeps a failed test's
containers for inspection. The real-B2 test (`TestDockerOffsiteB2`) runs only by hand, with
`BUNKARR_E2E_B2_KEY_ID`, `BUNKARR_E2E_B2_KEY` and `BUNKARR_E2E_B2_BUCKET` set (a throwaway bucket
and a key restricted to it). The restic and rclone versions are pinned in
`docker/engines/versions.env`; to move to a new version, change them there and run both suites.

The Go binary embeds `web/dist`; a Go-only build works (the UI then answers with a notice). Build
the UI with `make web`: `npm run build` empties `web/dist`, including the tracked `.gitkeep`,
which `make web` puts back.

CI (`ci.yml`, on every branch push and pull request) runs lint, the race tests, the e2e suite,
govulncheck and npm audit, the Unraid template check (`make ca-validate`), the engine binary
tests, the image build with the smoke, compose, kill and share tests (and a warning when the image
was built with an older Go patch release than the latest, `docker/check-go-version.sh --warn`)
and the off-site acceptance; the Plex test runs on `workflow_dispatch`. Tags run only the release
workflow, which repeats those gates and tags an image only after the e2e suite passes on the tagged
source and the whole Docker suite (the Plex test included) and the off-site acceptance pass
against the image ([Releases](#releases)).
The development repository runs the same CI jobs; its copies differ only where its shared runner
needs it (per-run image names, removed afterwards; shellcheck installed when missing).

The Unraid template (`unraid/bunkarr.xml`) and `ca_profile.xml` are generated from `unraid/ca/`
([`unraid/ca/README.md`](unraid/ca/README.md)): edit the `.tmpl` files or `publish.env`, run
`make ca-template ca-profile`, and commit the sources and the rendered files together. The
template mirrors `deploy/docker-compose.yml`, so a change to either runs `make ca-validate` and
`go test ./deploy/` before it is committed; the public export refuses a stale or invalid template,
and once Bunkarr is listed in Community Applications, CA reads the template straight from `main`.

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
internal/destinations/     destinations store, marker, capability probe, Open (safety rule S3);
                           kinds and remotes, sealed credentials and encryption secret, the
                           recovery kit, SFTP host keys, B2 key checks, engine settings
internal/engines/          engine contract (Engine, Session, VersionStore), destination, remote and
                           secret types, transfer windows, discovery of the restic/rclone binaries
internal/engines/proc/     exec layer: command and flag allow-list, secret files on tmpfs, exact
                           environment, redaction, exit status, process-group signals
internal/engines/restic/   restic driver: repository and environment, backup, snapshots, forget,
                           prune, check, restore, dump, guarded unlock; JSON parsers
internal/engines/rclone/   rclone driver: per-kind remote config and crypt, known_hosts, bwlimit
                           timetable, copy/move/delete, lsjson, check, cat/rcat; log parser
internal/engines/bwlimit/  bandwidth timetable and transfer-window math (pure); token-bucket writer
internal/engines/enginetest/ FakeRunner (scripted, or from testdata/restic and testdata/rclone),
                           fake Engine, Session and VersionStore; the enginebin test environment
internal/engines/filecopy/ filesystem primitives over os.Root: atomic copy, link, retain, expire, hashes
internal/enginerun/        Dispatch and the sync, verify and retention runners of restic and rclone
                           destinations (engine_snapshots, engine_forget, engine_state)
internal/syncer/           planner and the sync, verify and retention runners (destination_files);
                           targeted syncs (Params.Paths); tiers in a sync (kept files, releases);
                           the shared Planner and record store the engine runners use
internal/plexdb/           Plex DB backup runner, verification, version pruning (snapshots)
internal/notify/           Apprise targets and the notification dispatcher
internal/e2e/              acceptance suite (build tag e2e): binary and Docker tests, including the
                           *arr suite (TestDockerArr*)
testdata/arr/, testdata/webhooks/  recorded *arr API responses and webhook payloads
testdata/tautulli/, seerr/, maintainerr/  recorded Tautulli, Seerr and Maintainerr responses
testdata/restic/, testdata/rclone/  restic and rclone output recorded by the engine spike
web/                       React + TypeScript + Vite + Tailwind UI
deploy/                    docker-compose example; tests tying it and the release workflow's image
                           to the Unraid template
unraid/                    the Unraid template (bunkarr.xml, generated), icon and install guide;
                           ca/ holds its sources, settings file and render/validate/preflight
                           scripts
docker/                    entrypoint; image smoke, compose, container kill, Plex restore, share,
                           engine and off-site test wrappers; engines/ (test image and the pinned
                           restic/rclone versions), offsite/ (argv shims, SFTP setup, kit restore)
scripts/                   the dependency report (dependency-report.sh)
docs/design/               phase designs (the contracts packages are built against)
docs/adr/                  architecture decision records
docs/spikes/               spike reports
```

Each package owns its tables' SQL; other packages use its exported API
(`docs/design/phase1.md` §8, `docs/design/phase2-3.md` §14.2, `docs/design/phase4.md` §13.2).
The one exception is
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
- **Actions and base images** are pinned by commit SHA / digest; how they are updated is in
  [Dependency updates](#dependency-updates).
- **Pull requests:** the [template](.github/pull_request_template.md) asks what could go wrong for
  users' sources or backups and how the change was tested, and has a short checklist.

### Fault points and the crash matrix

Crash safety is tested by stopping the program at named step boundaries.

- Code that changes the destination or the job state calls `faultinject.Point("<step>.<when>")`
  at each boundary (`copy.afterWrite`, `update.afterRenameOld`, `plexdb.beforeRecord`, …).
  Document each point where it is defined (exported `Point…` constants in syncer, plexdb,
  jobqueue, db, integrations, mediaindex, webhooks, arrbackup, manifest, enginerun,
  destinations and snapshots; the package comment in filecopy). In production `Point` is one
  atomic load; injection is always compiled in and armed only by a test hook or the environment.
- **Go crash matrix** (`internal/syncer` `TestCrashMatrix`, `TestCrashMatrixResumePaths` and the
  targeted-sync matrix, the matrices of plexdb, arrbackup, manifest and mediaindex, and
  `internal/enginerun`'s `TestCrashMatrixEngines` and `TestCrashMatrixRetention` for restic and
  rclone): a clean run counts how often each listed point is reached; then, for every
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

## Dependency updates

Every dependency is pinned: Go modules by `go.sum`, the web UI's npm packages by
`web/package-lock.json`, base images and the release workflow's builder images (binfmt, BuildKit,
the SBOM scanner) by digest, and CI actions by full commit SHA with the release as a `# vX.Y.Z`
comment (`go test ./deploy/` enforces the digests and SHAs).
[Renovate](https://docs.renovatebot.com/) is configured to keep them current, with the rules in
[`.github/renovate.json`](.github/renovate.json):

- grouped pull requests, at most 5 open at a time (each one runs the Docker suites): CI actions,
  base image digests, builder images, Go modules, the web UI's runtime packages, and its build and
  test tooling; `web/package-lock.json` is refreshed once a month;
- a new release of an action, a builder image, a Go module or an npm package waits 3 days before
  it is proposed (a compromised or retracted release is usually caught by then); base image
  digests do not wait;
- major updates, and a CI action whose release tag was moved to another commit (how a hijacked tag
  shows up; the 3-day wait cannot catch it), wait on the *Dependency Dashboard* issue until they
  are ticked there;
- nothing is merged automatically: every update goes through CI like any other change;
- by hand, all places together: the Go and Node.js release lines (go.mod's `go` directive, the
  minimum Go version, with no `toolchain` line; the `golang` and `node` image tags in the
  Dockerfiles; the workflows' `go-version` and `node-version`; the `@types/node` major), and
  restic and rclone (`docker/engines/versions.env`, then `make test-engines` and
  `make test-offsite`).

Renovate is set up to run weekly in the private development repository, not on GitHub, but it is
not switched on there yet: until it is, the weekly dependency report (below) and GitHub's
Dependabot alerts flag updates, and they are applied by hand. Either way, updates are merged in the
development repository and reach the public repository with the next sync, and a pull request
merged on GitHub would be overwritten by it. So please do not open pull requests that only bump a
dependency. If an update is urgent (a security fix), open an issue, or report a vulnerability
privately ([SECURITY.md](SECURITY.md)).

The dependency report the development repository's CI writes every week (Go modules and npm
packages with newer releases, the Go and Node.js release lines, base image digests behind their
tag, the restic and rclone packages the image's alpine release ships, govulncheck, npm audit) also
runs by hand; it needs Go, npm, jq, curl and tar, and no token. It exits 0 however many updates are
available, 1 on CI's vulnerability gates and 2 when a check could not run:

```sh
(cd web && npm ci) && sh scripts/dependency-report.sh > dependency-report.md
```

## Releases

Maintainers only. The development repository publishes every commit on `main` to the public
repository through the sanitizing export (`make publish`, ADR 0004); a release tag there starts
`.github/workflows/release.yml`, which tests the image, tags and attests it, and publishes the
GitHub release with its SBOMs (ADR 0010). Nothing is done by hand after the tag.

A full release (`vX.Y.Z`):

1. `CHANGELOG.md`: move the `[Unreleased]` entries under `## [X.Y.Z] - YYYY-MM-DD` (the tag's
   date) and start a new, empty `## [Unreleased]` above it. That section becomes the release
   notes, so write it for users. Update the compare links at the end
   (`[Unreleased]: .../compare/vX.Y.Z...HEAD`, `[X.Y.Z]: .../compare/<previous>...vX.Y.Z`).
2. `unraid/ca/bunkarr.xml.tmpl`: add `### X.Y.Z (YYYY-MM-DD)` and a few user-facing lines at the
   top of `<Changes>` (plain ASCII, no square brackets, `arr` rather than `*arr`). It is the change
   log Community Applications shows, and the only way to tell installed containers about a new
   setting: they keep their saved template.
3. `unraid/ca/publish.env`: set `VERSION` and `RELEASE_DATE`.
4. `make ca-template` (and `make ca-profile` when its source or settings changed), then
   `make ca-validate` and `go test ./deploy/`: `deploy/unraid_changes_test.go` fails until the
   CHANGELOG heading, the `<Changes>` heading, `VERSION` and `RELEASE_DATE` agree.
5. Commit on `main` (`chore(release): X.Y.Z`); the post-commit hook publishes it. To review the
   export first, commit with `BUNKARR_NO_MIRROR=1`, run `make publish-dry`, check the result in
   `.git/public-mirror`, then run `make publish`. The export also runs `go test ./deploy/` on the
   exported tree and publishes nothing when it fails.
6. `git tag -a vX.Y.Z -m "Bunkarr vX.Y.Z"` on exactly that commit, the current `HEAD`, and run
   `make publish` at once. The hook ran before the tag existed, and the export pushes only the
   tags on the commit it publishes, so only this second run pushes the tag. The export creates the
   public tag (annotated) only when the public repository does not have it yet, and never moves or
   pushes again a tag it has: never move a published tag, and never tag an older commit. Push the
   tag to the development repository too (`git push origin vX.Y.Z`).
7. `release.yml` runs; the off-site suite alone may take two hours. It checks the tag, repeats
   CI's gates (`ci.yml` does not run on tags), builds the multi-arch image once as
   `:candidate-<commit>`, runs the Docker suite on amd64 and arm64 and the off-site suite against
   that digest, and the e2e suite on the tagged source. Then:
   - the `image` job tags that digest `:X.Y.Z`, `:vX.Y.Z`, `:X.Y` and `:latest`, attests it, and
     signs it when `COSIGN_SIGN` is `true` (below);
   - the `sbom` job writes SPDX 2.3 and CycloneDX 1.6 SBOMs of the source and of each image
     platform;
   - the `github-release` job publishes the GitHub release (`--verify-tag`): the SBOMs and
     `checksums.txt`, attested together, and notes that name the image and its digest-pinned
     reference, the tests it passed, the `gh attestation verify` commands, the SBOM files and the
     version's CHANGELOG section. The notes can be edited on the release page afterwards.

   A release fails when the image was built with an older Go patch release than the latest one of
   its line (`docker/check-go-version.sh`; CI only warns): bump the `golang` digest in the
   Dockerfiles and release the next version.

**Pre-releases** (`vX.Y.Z-beta.N`, `vX.Y.Z-rc.N`) skip steps 1 to 4: no CHANGELOG heading, no
`<Changes>` entry, `publish.env` unchanged, so the template, which Community Applications reads
from `main`, never names a pre-release. Tag the published `HEAD` as in step 6. The image gets only
`:X.Y.Z-rc.N` and `:vX.Y.Z-rc.N`, never `:latest` or `:X.Y`, which Unraid's update check follows.
The GitHub release is created with `--prerelease --latest=false`, and its notes take the
`[Unreleased]` section as of the tag, under "Pre-release: changes not yet in a released version".

**When a release run fails:**

- Before the `image` job (a test, the Go check), nothing but the candidate is published. A flaky
  test: re-run the failed jobs. Anything else: fix it on `main` and release the next version (the
  pushed tag is never moved or reused).
- In the `sbom` or `github-release` job, the image is already tagged and has no GitHub release
  yet. Re-run only the failed jobs: `gh run rerun <run id> --failed`, or *Re-run failed jobs* on
  the run's page. They reuse this run's tested candidate and its digest; `github-release` replaces
  the files of an existing release (`gh release upload --clobber`) and keeps its notes.
- Never *Re-run all jobs* once the `image` job has tagged: that builds a new candidate, and the
  `image` job then fails, because a version's tags never move to another digest.
- Never delete a released version's `:candidate-<commit>` tag in the package settings: it is the
  same package version (one digest) as the release tags, so the release image goes with it.

**Public record.** The repository is public, so the two attestations (the image's and the release
files') are signed through Sigstore's public-good instance and recorded in the public Rekor
transparency log with the workflow's identity (repository, workflow file, tag, commit, run), for
good: pushing a release tag makes these entries, and they cannot be removed.

**Signing.** `COSIGN_SIGN` is an optional repository variable (Settings → Secrets and variables →
Actions → Variables). Exactly `true`, in lower case (the `image` job checks the value in a shell
step, so `True` signs nothing), makes the `image` job also sign the digest with cosign, keyless
(Sigstore, the workflow's GitHub OIDC identity), and adds a `cosign verify` command to the notes.
It is unset: a signature is a second, separate entry in the same public Rekor log for each
release, and the attestations already prove where the image was built. Deleting the variable
only stops new ones.

**The development repository** (the private Gitea) has its own release workflow: on every tag it
runs CI's test gates, builds the image for its runner's platform and runs the smoke, kill and Go
version checks on it, and publishes nothing. It pushes an image to that Gitea's registry and publishes a release there only once its
`PUBLISH_TO_GITEA` variable and registry secrets are set, which they are not. Public releases are
made on GitHub only.

Once Bunkarr is listed in Community Applications:

- **A release commit goes live in Community Applications at once**: CA reads `main` at its next
  feed build, before `release.yml` has pushed the image (the off-site suite alone may run for two
  hours). If the release fails, act right away, so CA does not announce a version that cannot
  be pulled. Either revert the release together: the `CHANGELOG.md` heading (its entries go back
  under `[Unreleased]`), `<Changes>`, `VERSION` and `RELEASE_DATE` (`deploy/unraid_changes_test.go`
  fails on a partial revert). Or fix forward with the next patch version: a new CHANGELOG heading,
  `<Changes>` entry, `VERSION`, `RELEASE_DATE` and tag. The pushed tag is never moved or reused,
  so the fixed release always ships under a new version.
- **Never rename or transfer the repository, rename the account, or move or rename
  `unraid/bunkarr.xml`**: CA can blacklist the whole repository for it, and installed containers
  keep reading their `TemplateURL`.
- **When the icon changes**, run `make ca-icon`, append `?v=2` (then `?v=3`, ...) to `ICON_URL`
  in `unraid/ca/publish.env` and run `make ca-template ca-profile`: Unraid and CA cache icons by
  URL.

## Security

Please report vulnerabilities privately, never in a public issue: [SECURITY.md](SECURITY.md) (the
Security tab's "Report a vulnerability"). It also says how to verify a release. Questions and
setup help: [SUPPORT.md](SUPPORT.md).

## License

By contributing you agree that your contributions are licensed under GPL-3.0-or-later.
