# Security policy

Bunkarr holds your Plex token, your Sonarr, Radarr and Lidarr API keys, the credentials and
encryption secrets of your off-site destinations, and read access to your whole library. It must
never write to your source media. Security reports are welcome and taken seriously.

## Supported versions

Bunkarr is pre-1.0. Security fixes are made on `main` and shipped in the next release; there are
no back-ported fixes for older versions.

| Version | Supported |
|---|---|
| Latest release (and `main`) | Yes |
| Any older release | No: update to the latest release |

Until 0.1.0, the latest release is a pre-release (`0.1.0-beta.1`, then release candidates). After
1.0, the latest minor release will receive security fixes.

## Reporting a vulnerability

**Please do not open a public issue, pull request or discussion with the details of a
vulnerability.**

Report it privately through GitHub:
**[Report a vulnerability](https://github.com/sl0wz3r/bunkarr/security/advisories/new)** (the
repository's *Security* tab → *Report a vulnerability*). Only you and the maintainer
(**sl0wz3r**) can see the report. This is the only channel: the issue forms and Discussions are
public.

Please include:

- the Bunkarr version (*System → Status*, or `docker exec bunkarr /entrypoint.sh version`) and
  how you run it (Unraid template, Docker Compose, `docker run`), including the authentication
  setting, `BUNKARR_ALLOWED_HOSTS` and whether a reverse proxy is involved;
- what an attacker needs (network position, credentials, write access to a source folder, a
  malicious Plex or \*arr server, a crafted backup, …) and what they gain;
- steps to reproduce or a proof of concept, and any logs, **with tokens, keys, passwords and
  recovery kits removed** (Bunkarr redacts the secrets it knows from its logs, but check before
  sending).

What to expect:

- an acknowledgement within **7 days**;
- an assessment (accepted, needs more information, or not a vulnerability, with reasons) within
  **14 days**;
- a fix or a documented mitigation as soon as possible, targeting **30 days** for high-severity
  issues and **90 days** for the rest; you will be told if that slips;
- credit in the release notes and in [CHANGELOG.md](CHANGELOG.md), unless you prefer not to be
  named.

Please give us a reasonable time to release a fix before disclosing publicly, and coordinate the
disclosure date with the maintainer.

## Scope

In scope: the Bunkarr server (`cmd/`, `internal/`), the web UI (`web/`), the container image and
its entrypoint (`Dockerfile`, `docker/`), the deployment files (`deploy/`, `unraid/`) and the CI
and release workflows (`.github/workflows/`).

Especially interesting:

- authentication or authorization bypasses: the login and its limiter, the API key, the
  per-connection webhook keys, *Disabled for local addresses* (including through relays, DNS
  rebinding or cross-site requests), the host-name check of the first-run setup and
  `BUNKARR_ALLOWED_HOSTS`;
- any way to make Bunkarr modify, move or delete a file under a source, or read or back up
  anything outside a source's root (symlinks or FIFOs swapped in after a scan, crafted names, the
  config directory seen through another mount), or write outside a destination's target;
- disclosure of stored secrets (`bunkarr.key`, the sealed credentials in `bunkarr.db`, the Plex
  token, \*arr API keys, webhook keys, B2, S3 and SFTP credentials, restic and rclone encryption
  secrets, recovery kits, Apprise URLs) to anyone but an authenticated admin, including through
  logs, job details, command lines or the environment of the restic and rclone processes;
- crafted Plex databases or Sonarr, Radarr and Lidarr backups that make a backup check run
  their SQL, hang a job worker or exhaust memory;
- server-side request forgery through the Plex, \*arr, Tautulli, Seerr, Maintainerr or Apprise
  clients, webhooks, or B2, S3 and SFTP destinations, that turns admin features into a pivot into
  your network (link-local and cloud metadata addresses are refused);
- arguments that reach restic or rclone outside their allow-list;
- code execution, container escape or privilege escalation (the entrypoint, `PUID`/`PGID`, file
  ownership, su-exec).

Out of scope:

- attacks that require an already authenticated admin, unless they cross a boundary listed above;
- setups that deliberately weaken the defaults: *Disabled for local addresses* on a network you
  do not trust, a reverse proxy's public name in `BUNKARR_ALLOWED_HOSTS` with that bypass on, or
  the port forwarded to the internet without a reverse proxy;
- the known limits recorded in [DEFERRED.md](DEFERRED.md) (for example trusted reverse proxies,
  the first-run setup token, what the restic and rclone processes can read), unless you find a
  way past what that file describes;
- denial of service by flooding the network or the host;
- vulnerabilities in Plex, Sonarr, Radarr, Lidarr, restic, rclone or other third-party software
  (report those upstream; do tell us when the version Bunkarr ships is affected).

## Verifying a release

Every [GitHub release](https://github.com/sl0wz3r/bunkarr/releases) names the image digest in its
notes, so you can pin exactly the image that was tested:
`ghcr.io/sl0wz3r/bunkarr:<version>@sha256:<digest>` (Docker checks every byte it pulls against
it). Its files are:

- `checksums.txt`, the SHA-256 of every other file (`sha256sum -c checksums.txt --ignore-missing`);
- SBOMs (software bills of materials) in SPDX 2.3 and CycloneDX 1.6 JSON:
  `bunkarr_<version>_source.spdx.json` and `.cdx.json` list the Go modules and the web UI's
  runtime npm packages of the tagged source; `bunkarr_<version>_image_linux_amd64.spdx.json` and
  `.cdx.json` (and `_linux_arm64`) list the alpine packages (restic, rclone, tini and su-exec
  among them) and the Go modules in that platform of the image. Give them to a scanner such as
  Grype, Trivy or Dependency-Track to check the version you run against advisories published
  after its release.

There are no binaries: the image is the only supported install.

GitHub artifact attestations (signed with Sigstore, and recorded in its public Rekor transparency
log) prove that the image and every file of the release, `checksums.txt` included, were built by
this repository's release workflow from the tagged commit:

```sh
gh attestation verify oci://ghcr.io/sl0wz3r/bunkarr:<version> --owner sl0wz3r
gh attestation verify bunkarr_<version>_source.spdx.json --owner sl0wz3r
gh attestation verify checksums.txt --owner sl0wz3r
```

The image also carries BuildKit's own SBOM and provenance, part of the image index that the
attestation covers:
`docker buildx imagetools inspect ghcr.io/sl0wz3r/bunkarr:<version> --format '{{ json .SBOM }}'`.
When a release is also signed with cosign, its notes give the `cosign verify` command.

`0.1.0-beta.1` predates all of this: it has no GitHub release, attestations or SBOM files, only
BuildKit's SBOM and provenance in its image (the command above, or
`--format '{{ json .Provenance }}'`); `docker buildx imagetools inspect
ghcr.io/sl0wz3r/bunkarr:0.1.0-beta.1` prints its digest. They start with the next release.

## Safe harbor

We will not pursue anyone who reports in good faith, tests only against their **own**
installation and their own storage accounts, avoids accessing or destroying other people's data
and backups, and gives us the chance to fix the issue before disclosure.

## Hardening your installation

- Keep *Settings → General → Authentication required: Enabled* (the default), and put Bunkarr
  behind a reverse proxy with TLS rather than forwarding its port
  ([`unraid/README.md`, Reverse proxy](unraid/README.md#reverse-proxy); the same holds for Docker
  Compose).
- Create the login right after the install: until a user exists, whoever reaches Bunkarr first
  (under one of its own host names) can create it.
- Mount the sources read-only, as the compose example and the Unraid template do.
- Back up `bunkarr.key` with the database, and keep each off-site destination's recovery kit
  away from the server: they unlock every stored credential and backup.
- Pin a version and its digest from the release notes, and update when a release fixes a
  vulnerability.
