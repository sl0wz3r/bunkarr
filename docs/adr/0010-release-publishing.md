# 0010. Release publishing: GitHub releases, attestations and SBOMs

- Status: accepted
- Date: 2026-09-29
- Amends: [ADR 0009](0009-unraid-community-applications.md) (GitHub releases are no longer made by
  hand; questions move to Discussions) and [ADR 0004](0004-private-gitea-public-github.md) (the
  public export runs the deploy tests and creates annotated tags)

## Context

Up to 0.1.0-beta.1, `.github/workflows/release.yml` built the multi-arch image once as a
candidate, ran the Docker suite (amd64 and arm64) and the off-site suite against that digest and
the e2e suite on the tagged source, and only then put the release tags on it. Everything after that was by hand: the
GitHub release, its notes and the digest to pin. Users had no way to check where an image came
from beyond BuildKit's own provenance inside the index, and no bill of materials to scan the
version they run against advisories published later.

The maintainer's DupeArr publishes its releases with GitHub artifact attestations, Syft SBOMs and
an automatic GitHub release. Bunkarr differs in two ways that shape the port: its candidate flow
tests the exact bytes that ship (DupeArr builds and pushes in one job), and it has no native
install path, since it needs restic and rclone at pinned versions, tini, su-exec and its
entrypoint. The development repository's CI (ADR 0004) lacked DupeArr's opt-in release workflow,
self-hosted Renovate and weekly dependency report.

## Decision

- **The tested digest is what is published.** The candidate flow stays. After every test job
  passes, the image job tags the candidate's digest `:X.Y.Z`, `:vX.Y.Z`, `:X.Y` and `:latest`
  (a pre-release: `:X.Y.Z-pre` and `:vX.Y.Z-pre` only). A version is released once: the job
  refuses to move `:X.Y.Z` or `:vX.Y.Z` to another digest, so a full re-run, which builds a new
  candidate, fails there; re-running the failed jobs reuses the tested candidate.
- **Attestations.** The image job attests that digest (`actions/attest`, SLSA build provenance
  signed with a Sigstore certificate for the run that built and tested it) and pushes the
  attestation next to the image. The release files are covered by one attestation whose subjects
  are every file the release publishes: the SBOMs and `checksums.txt` itself. No separate SBOM
  attestation on the image: BuildKit's SBOM is part of the index the provenance covers. The
  repository is public, so both attestations are signed through Sigstore's public-good instance
  and recorded in the public Rekor transparency log with the workflow's identity (repository,
  workflow file, tag, commit, run): every release tag makes two permanent public entries.
- **SBOMs.** A `sbom` job runs Syft on the tagged source (Go modules, the web UI's runtime npm
  packages, the workflows' actions) and, from the registry by digest, on each image platform
  (alpine packages, the Go modules in the binary and in restic and rclone), in SPDX 2.3 and
  CycloneDX 1.6 JSON with pinned format versions, and refuses a file that misses a package the
  release is known to contain.
- **The GitHub release is automatic.** A `github-release` job, after the image and SBOM jobs,
  publishes the six SBOMs and `checksums.txt` with `gh release create --verify-tag`. The notes
  name the image and its digest-pinned reference, say what was tested (the Docker and off-site
  suites against that digest, the e2e suite on the tagged source), give the
  `gh attestation verify` commands and the SBOM names, and end with the version's CHANGELOG
  section. A pre-release takes the `[Unreleased]` section as of its tag and is created with
  `--prerelease --latest=false`, so `CHANGELOG.md`, the Unraid template's `<Changes>` and
  `publish.env` never name a pre-release (`deploy/unraid_changes_test.go` reads the newest
  CHANGELOG heading as the newest release). A re-run uploads the files again (`--clobber`) and
  keeps the notes, so the repository keeps immutable releases off.
- **cosign is opt-in** (the repository variable `COSIGN_SIGN`, exactly `true`: one shell step
  reads it and both the signing steps and the notes follow that step, since `if:` compares
  strings case-insensitively): keyless signing of the digest, and a `cosign verify` line in the
  notes. It stays off: the attestations already prove the build, and a signature would add a
  second, separate permanent Rekor entry for each release.
- **The image names its documentation**: the `org.opencontainers.image.documentation` label is
  the README at the release tag.
- **No native binaries**, and still no LAN template variant and no `docker save` path (ADR 0009):
  the image is the only supported install, so the release's files are its SBOMs and checksums.
- **The Go patch release gates the release.** The docker jobs run `docker/check-go-version.sh`
  strictly: a release fails when the image was built with an older Go patch release than the
  latest one of its line. `ci.yml` only warns.
- **Tags run only `release.yml`.** `ci.yml` runs on branches, pull requests and manual runs; the
  release workflow runs the gates the tagged commit would otherwise skip (gofmt, vet, shellcheck,
  the compose test, `make ca-validate`) itself.
- **The development repository's CI** gains an opt-in release workflow for its own registry and
  releases (off until its variable and secrets are set; a separate build, never the public
  image), self-hosted Renovate (idle until its token is set), and a weekly dependency report
  (`scripts/dependency-report.sh`, public, also run by hand). Renovate now also manages Go modules
  and npm packages. Updates are merged there and reach GitHub with the sync; the Go and Node.js
  release lines and the pinned restic and rclone versions still move by hand.
- **Amends ADR 0009:** its "GitHub releases are made by hand" no longer holds. Questions and setup
  help move from blank issues to GitHub Discussions (Q&A, Ideas); the issue forms remain for bugs
  and features, and the template's `<Support>` stays the Issues page, whose chooser links Q&A.
- **Amends ADR 0004:** `scripts/public/sync.sh` also runs `go test ./deploy/` on the exported tree
  before its term scan and fails closed, so every deploy test must pass (or skip) without the
  excluded `.gitea/`. It creates a `v*` tag on the public repository, annotated, only when that
  repository lacks it, and never moves, replaces or re-pushes a tag it already has (the
  lightweight `v0.1.0-beta.1` stays as it is).

## Consequences

- A release needs no manual step after the tag; the release notes are the CHANGELOG section, so
  the CHANGELOG entry is written for users.
- Everything after the image job runs for the first time on a real tag: attestations, Syft, the
  SBOM checks and `gh release`. The first release under this ADR should be a pre-release. If a
  job after the image job fails, the image is published without a GitHub release until the
  failed jobs are re-run.
- A release tag cannot be taken back once its attestations exist (the image's from the image job
  on, the release files' from the github-release job): they are permanent public Rekor entries,
  whether or not the image or the release is deleted later.
- `0.1.0-beta.1` has no GitHub release, attestations or SBOM files: an attestation must come from
  the run that built the digest.
- Deleting a released version's `:candidate-<commit>` tag in the package settings deletes the
  release image: they are one package version.
- The public export needs `go` on the maintainer's `PATH`; a commit from a client without it
  fails closed and publishes nothing.
