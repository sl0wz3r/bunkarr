# unraid/ca: the Community Applications template, from one settings file

`unraid/bunkarr.xml` (the one public Unraid template) and the repository-root `ca_profile.xml` are
**generated** from the sources here. The public owner, repository, registry and support link are
set in exactly one place, [`publish.env`](publish.env); every URL in both files is derived from it.
This page is for maintainers and contributors; installing Bunkarr on Unraid is described in
[`unraid/README.md`](../README.md).

| File | Committed | Purpose |
|---|---|---|
| [`publish.env`](publish.env) | yes | **The one settings file**: `PUBLIC_OWNER`, `PUBLIC_REPO`, `PUBLIC_HOST`, `PUBLIC_BRANCH`, `REGISTRY`, `IMAGE_NAME`, `IMAGE_TAG`, `ICON_URL`, `SUPPORT_URL`, `FORUM_URL`, `VERSION`, `RELEASE_DATE` |
| [`bunkarr.xml.tmpl`](bunkarr.xml.tmpl) | yes | Source of `unraid/bunkarr.xml`: Overview, Requires, Changes, Config entries |
| [`ca_profile.xml.tmpl`](ca_profile.xml.tmpl) | yes | Source of `ca_profile.xml` (maintainer profile, required at the repository root) |
| [`render.sh`](render.sh) | yes | POSIX sh renderer: parses (never sources) the env file, derives the URLs, fills the placeholders, refuses to write anything with a placeholder left |
| [`validate-template.sh`](validate-template.sh) | yes | Offline check of the Community Applications rules and Bunkarr's own rules listed below (needs `xmllint`) |
| [`preflight.sh`](preflight.sh) | yes | Online, read-only, anonymous check of the live public repository, raw URLs, icon and image |
| [`.gitignore`](.gitignore) | yes | Keeps `out/` out of the repository |
| `out/` | **no** (git-ignored) | Rendered scratch files (`CA_OUT`) |

Generated elsewhere: [`../bunkarr.xml`](../bunkarr.xml) (`make ca-template`),
[`../../ca_profile.xml`](../../ca_profile.xml) (`make ca-profile`) and [`../icon.png`](../icon.png)
(`make ca-icon`, from `web/public/favicon.svg`). Edit the sources, never these files.

## Commands

```sh
make ca-vars          # print every derived value (URLs to paste into the CA form)
make ca-template      # render + validate -> unraid/bunkarr.xml
make ca-profile       # render + validate -> ./ca_profile.xml (repository root)
make ca-validate      # offline: committed files are current and pass every rule (+ repository scan)
make ca-preflight     # online, read-only: repo public/active/licensed, raw URLs, icon, image amd64/arm64
make ca-icon          # render unraid/icon.png (512x512 RGBA) from web/public/favicon.svg
```

`ca-template`, `ca-profile` and `ca-icon` render into `out/`, validate there, and only then copy
the result into place, so a failing rule never replaces a good file. Override the settings file
with `make ca-template CA_ENV=path/to/other.env` (for example to try a different owner) and the
scratch folder with `CA_OUT=...`.

`ca-validate` runs in CI. It also fits an exported tree that is not a git work tree (a GitHub
archive, or an export unpacked inside another repository): the repository scan then lists every
file in it instead of `git ls-files`, leaving out the scratch folder `CA_OUT`.

Tools: a POSIX `sh` (dash works), `xmllint` (macOS ships it; Debian and Ubuntu:
`apt install libxml2-utils`), `sed`, `grep`, `awk`, `od`, `cmp` (GNU or BSD); `curl` for
`ca-preflight`; `rsvg-convert` for `ca-icon` (macOS: `brew install librsvg`; Debian and Ubuntu:
`apt install librsvg2-bin`).

## Placeholders

In a `.tmpl` file, `{{KEY}}` is replaced by the XML-escaped value of `KEY`; `{{?KEY}}` does the
same but drops the whole line when the value is empty (used for `<Forum>`); a line starting with
`{{#` is a comment for maintainers and is removed. Unknown keys, malformed values (owner,
repository, version, date, lower-case image, URLs), a first `<Changes>` heading other than
`### VERSION (RELEASE_DATE)` and anything left looking like `{{...}}` fail the render. The
environment is ignored: values come only from the `-e` files.

| Key | Default (derived) | Used for |
|---|---|---|
| `PUBLIC_REPO_URL` | `https://github.com/OWNER/REPO` | `<Project>`, profile `<WebPage>` and text |
| `PUBLIC_RAW_BASE` | `https://raw.githubusercontent.com/OWNER/REPO/BRANCH` | base of `ICON_URL` |
| `TEMPLATE_URL` | `PUBLIC_RAW_BASE/unraid/bunkarr.xml` | `<TemplateURL>` |
| `ICON_URL` | `PUBLIC_RAW_BASE/unraid/icon.png` | `<Icon>`, profile `<Icon>` |
| `README_URL` | `PUBLIC_REPO_URL/blob/BRANCH/unraid/README.md` | `<ReadMe>` |
| `IMAGE` | `ghcr.io/<lower-case owner>/bunkarr:latest` | `<Repository>`, profile text |
| `REGISTRY_URL` | `https://github.com/OWNER/REPO/pkgs/container/bunkarr` | `<Registry>` |
| `SUPPORT_URL` | `PUBLIC_REPO_URL/issues` until a forum support thread exists | `<Support>`, profile text |
| `FORUM_URL` | empty (line dropped) | profile `<Forum>` |
| `VERSION`, `RELEASE_DATE` | from `publish.env` | `<Date>`; the newest `<Changes>` entry (written in the `.tmpl`) must name them, like the newest release in `CHANGELOG.md` |
| `ENV_NAME` | the env file names | header comment |

## Rules the template keeps

Checked by `validate-template.sh`:

- **CA format.** Each `<Config>` on one line; `&amp;` for `&`; no `<...>` text; plain ASCII; no
  `[` or `]` in `Overview` or `Changes` (CA turns them into tags and strips them, which also
  breaks Markdown links); write arr, never `*arr` (an odd number of asterisks starts Markdown
  emphasis); `Support`/`Project`; `TemplateURL` = the raw URL of this file on `main`; an https
  `Icon` that is a square, transparent, not animated PNG of at least 256 px (ours: 512x512 RGBA)
  holding image chunks only (an allow-list) and nothing after its end (text, EXIF, time stamps,
  ICC profiles or private chunks would be published unscanned, since the public export's term
  scan skips binary files); valid `Category` tokens; `WebUI` on container
  port 8787, which exists as a Port entry; `Privileged` false; `Beta` true while 0.x; no
  `MaxVer`; `ExtraParams` docker flags only (no `--privileged`, no environment variables); no
  shell metacharacters in `PostArgs`; a `:latest` image on a public registry; https URLs only and
  no private addresses or LAN host names.
- **No TZ entry.** Unraid injects the server's `TZ` into every container, and an empty template
  `TZ` would come after it and reset the container to UTC.
- **Data safety up front.** The Overview says that Bunkarr never modifies or deletes the sources
  and mounts them read-only, that `bunkarr.key` must be backed up with the database, that it is a
  Beta, and that the restore wizard is not built yet. It promises no automatic restore: until the
  restore wizard exists, restores and re-acquiring manifest-only content are manual. Drop the
  validator's `restore wizard` rule in the same change that updates that sentence.
- **Bunkarr's entries.** Port 8787 (tcp); `/config` rw; `/media` and `/plex` ro; `/backup`
  `rw,slave` (Unassigned Devices mounts shares after Docker starts); `/arr/sonarr-backups`,
  `/arr/radarr-backups` and `/arr/lidarr-backups` ro; `PUID`, `PGID` (99 and 100 on Unraid) and
  `UMASK`; and `--hostname=bunkarr-...` in `ExtraParams` (restic tells stale repository locks
  apart by host name).

Checked by the Go tests in `deploy/` (`go test ./deploy/`): the template mirrors
`deploy/docker-compose.yml` (image, container name, mounts and modes, port, variables, host name,
stop timeout) both ways, and a compose volume, environment or port entry the test cannot read
fails it rather than going unchecked. Host paths stay empty except `/config`, since Unraid creates missing host folders
and a guessed path would create an empty share; the mounts compose requires are required in the
template too, except `/plex` (the Plex database backup is optional). `README.md` and
`unraid/README.md` must give the template URL, and `unraid/README.md` must describe every Config
target. From the first release on, `deploy/unraid_changes_test.go` also checks that the newest
`<Changes>` entry, `VERSION` and `RELEASE_DATE` match the newest release in `CHANGELOG.md`.

## Releases

For every release: add its entry at the top of `<Changes>` in `bunkarr.xml.tmpl`
(`### X.Y.Z (YYYY-MM-DD)` and user-facing lines, no square brackets), set `VERSION` and
`RELEASE_DATE` in `publish.env`, run `make ca-template` and commit `unraid/bunkarr.xml` with the
`CHANGELOG.md` entry. Once the app is listed, Community Applications reads `main` directly: the
release commit is live in CA at its next feed build, before the release workflow has pushed the
image. If a release fails, act at once: revert the `CHANGELOG.md` heading, `<Changes>`, `VERSION`
and `RELEASE_DATE` together, or fix forward with the next patch version; a pushed tag is never
reused (CONTRIBUTING.md, Releases).

When the icon changes after the listing, run `make ca-icon` and append `?v=2` (then `?v=3`, ...)
to `ICON_URL` in `publish.env`, since CA caches icons by URL.

## Never rename

Renaming or transferring the repository, renaming the account or moving `unraid/bunkarr.xml`
after the CA listing can get the whole repository blacklisted, and installed containers keep
reading their `TemplateURL`. Decide `PUBLIC_OWNER` and `PUBLIC_REPO` before the listing.

Submission: [Publishing to Community Applications](../README.md#publishing-to-community-applications)
in `unraid/README.md`.
