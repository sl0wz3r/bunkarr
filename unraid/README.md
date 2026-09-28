# Bunkarr on Unraid

Everything Unraid needs to run Bunkarr. Bunkarr is a Beta (pre-1.0): it backs up, but the restore
wizard is not built yet, so a restore is a file copy or the restic and rclone commands of a
destination's recovery kit (README, [Restoring](../README.md#restoring)).

| File | Purpose |
|---|---|
| [`bunkarr.xml`](bunkarr.xml) | The Docker template (dockerMan and Community Applications format, `version="2"`): the **one** public template. **Generated** by `make ca-template` from [`ca/bunkarr.xml.tmpl`](ca/bunkarr.xml.tmpl) and [`ca/publish.env`](ca/publish.env); edit those, never this file. |
| [`icon.png`](icon.png) | The 512x512 RGBA icon the template's `<Icon>` points at, rendered from `web/public/favicon.svg` by `make ca-icon`. |
| [`ca/`](ca/README.md) | The template sources, the one settings file, and the render, validate and preflight scripts (maintainers). |

- **From Community Applications**, once Bunkarr is listed there: **Apps** → search **Bunkarr** →
  **Install**.
- **By hand** until then, or to test a template: [section 2](#2-install-the-template).

Either way, review [the settings](#3-settings) and
[the Unraid settings outside the template](#4-unraid-settings-outside-the-template) before
**Apply**, then continue with the [first start](#5-first-start).

---

## 1. The image

The template pulls `ghcr.io/sl0wz3r/bunkarr:latest` from the GitHub Container Registry. It is
built for `linux/amd64` (every Unraid server) and `linux/arm64`, anyone can pull it (no login, no
Docker setting), and Unraid's update check follows the `latest` tag. **The first image comes with
the first release (0.1.0):** until then the template has nothing to pull, and the
[Docker Compose quick start](../README.md#quick-start-docker-compose) builds the image from a
checkout instead (Unraid's Compose Manager plugin runs it the same way).

- `:latest` moves only for a full release (`vX.Y.Z`), which also gets `:X.Y.Z` and `:X.Y`. A
  pre-release (`vX.Y.Z-rc.N`) gets only its own tag, `:X.Y.Z-rc.N`, and never moves `:latest`.
- A release tag is set only on an image that passed the end-to-end, Docker and off-site test
  suites as the very image that ships (`.github/workflows/release.yml`).
- To run exactly one image, set **Repository** to a version tag, or pin its digest as well:
  `ghcr.io/sl0wz3r/bunkarr:X.Y.Z@sha256:<digest>` (`docker buildx imagetools inspect
  ghcr.io/sl0wz3r/bunkarr:X.Y.Z` prints it). Docker then checks every byte it pulls against the
  digest. A version tag never moves, so Unraid offers no update for it: change the tag (and the
  digest) to update.

## 2. Install the template

### From Community Applications (once listed)

**Apps** → search **Bunkarr** → **Install**. Community Applications fills in the **Appdata** host
path from **Settings → Docker → Default appdata storage location** plus the container name (for
example `/mnt/cache/appdata/bunkarr`); everything else is as [section 3](#3-settings) describes.

### By hand (user template)

In the server's terminal (**>_** in the web UI, or SSH), copy the template onto the flash drive as
`my-bunkarr.xml` (the `my-` prefix and the lower-case name are what dockerMan writes itself, so
your first **Apply** updates the same file):

```sh
mkdir -p /boot/config/plugins/dockerMan/templates-user
wget -O /boot/config/plugins/dockerMan/templates-user/my-bunkarr.xml \
  https://raw.githubusercontent.com/sl0wz3r/bunkarr/main/unraid/bunkarr.xml
```

Then **Docker** → **Add Container** → **Template**: **bunkarr** under *User templates*, review the
settings ([section 3](#3-settings)) and **Apply**. A user template keeps the template's Appdata
path, `/mnt/user/appdata/bunkarr`.

To behave like a Community Applications install (the Appdata auto-fill above), put the file in a
folder of its own under `/boot/config/plugins/dockerMan/templates/` instead (never `limetech/`,
which Unraid replaces), where it shows under *Default templates*; or under
`/boot/config/plugins/community.applications/private/bunkarr/`, where **Apps** lists it under
*Private Apps*.

## 3. Settings

| Setting (container side) | Default | Notes |
|---|---|---|
| **WebUI Port** (`8787`, tcp) | `8787` | The web UI, the API and the Sonarr, Radarr and Lidarr webhooks. Readarr also listens on 8787 by default: if it runs on this server, change only the host port (for example to `8788`) and use that port in the webhook URLs. The container port stays 8787; the **WebUI** link follows the host port. |
| **Appdata** (`/config`) | `/mnt/user/appdata/bunkarr` | Bunkarr's database (`bunkarr.db`), `bunkarr.key`, `logs/`, the Plex DB staging (free space for one copy of the Plex database), the restic cache (`cache/`, several GiB for a large repository) and the pre-migration copies of the database (`backups/`). Keep it on a pool or cache disk, never on an NFS or SMB share. One appdata folder serves one Bunkarr: a second container on the same folder stops with *another Bunkarr instance is already using this config directory*. Back it up: [section 6](#6-back-up-bunkarrs-own-data). |
| **Media** (`/media`, Read Only) | empty; required | Your library. Bunkarr never modifies or deletes source media, and mounts it read-only. Map the **one** folder that holds both downloads and library (TRaSH layout: `/mnt/user/data`), so hardlinks between them are detected, and turn on **Settings → Global Share Settings → Tunable (support Hard Links)**. Media on an Unassigned Devices disk or share needs access mode *Read Only - Slave*. Unraid creates a missing host path as an empty folder (on `/mnt/user`, a new share), so check the path before the first sync: Bunkarr's scan refuses a source whose root is missing or looks unmounted, or is empty once files are catalogued, but a first scan of an empty folder just finds nothing. |
| **Plex data** (`/plex`, Read Only) | empty; optional | For the Plex DB backup: Plex's *Plex Media Server* folder, the one holding `Preferences.xml` and `Plug-in Support`. It is the Plex container's appdata folder plus `Library/Application Support/Plex Media Server`: with linuxserver's container (named `plex`), `/mnt/user/appdata/plex/Library/Application Support/Plex Media Server`; with the official one (`Plex-Media-Server`, whose template leaves its appdata folder to Unraid's default), `/mnt/user/appdata/Plex-Media-Server/Library/Application Support/Plex Media Server`. Check that the path exists: Unraid creates a missing one as an empty folder. Leave it empty if you do not use the Plex DB backup. In Bunkarr: **Settings → Plex → Data path** `/plex`. |
| **Backup destination** (`/backup`, Read/Write - Slave) | empty; required | A destination as mounted on the host: a NAS share mounted with Unassigned Devices (for example `/mnt/remotes/NAS_backup`; NFS recommended) or another disk. Keep *Read/Write - Slave* ([Unassigned Devices](#unassigned-devices)). In Bunkarr, add a destination with target `/backup` or a folder inside it. Off-site-only setups (B2, S3, SFTP) still need a folder here (the setting is required); Bunkarr then leaves it unused. |
| **Sonarr Backups folder** (`/arr/sonarr-backups`), **Radarr Backups folder** (`/arr/radarr-backups`), **Lidarr Backups folder** (`/arr/lidarr-backups`), all Read Only | empty; optional (Advanced View) | For the Sonarr, Radarr and Lidarr configuration backups while the app requires a login (Authentication: Forms, its default): its API key cannot download backups then, so Bunkarr reads the zips from the app's `Backups` folder, its `/config/Backups` on the host (for example `/mnt/user/appdata/sonarr/Backups`). **The folder must exist first:** in the app, run **System → Backup → Backup Now** once before you set the path. Unraid creates a missing path as an empty folder, and any parent folders it creates are owned by root. Then in Bunkarr: **Settings → Connect → Sonarr → Backup → Backups folder** `/arr/sonarr-backups` (Radarr and Lidarr likewise) and **Test**: *Backups folder: readable*. The zips hold the app's API key and passwords ([*arr config backups](../README.md#arr-config-backups)). |
| **PUID** / **PGID** (Advanced View) | `99` / `100` | The user and group Bunkarr runs as. Use the ones Plex runs as (Unraid: 99 `nobody`, 100 `users`): Plex keeps `Preferences.xml` at mode 0600, and the Plex DB backup must read it. The same user must read your media and the arr Backups folders, and write the backup share. |
| **UMASK** (Advanced View) | `022` | For the files and folders Bunkarr writes: `022` gives files 0644 and folders 0755, `002` keeps them group-writable. `bunkarr.key`, the Plex `Preferences.xml` copies and the arr backup zips are always 0600. |
| **Log level** (`BUNKARR_LOG_LEVEL`, Advanced View) | `info` | `debug`, `info`, `warn` or `error`, for the container log and `/config/logs/bunkarr.log` (always JSON). Use `debug` for a bug report, then set it back. |
| **Extra Parameters** (Advanced View) | `--hostname=bunkarr-tower --stop-timeout=60` | Keep both. Replace `tower` with this server's name ([Host name](#host-name)). `--stop-timeout=60` gives a plain `docker stop` a minute; Unraid's own stops use its [Docker Stop Timeout](#docker-stop-timeout) instead. |

**No TZ entry**, on purpose: Unraid passes the server's time zone (`TZ`) to every container,
Plex included, and the Plex DB backup checks Plex's maintenance hours in that zone. An empty TZ
entry would reset the container to UTC. If your Plex container sets a different `TZ`, give
Bunkarr the same value (**Add another Path, Port, Variable, Label or Device** → Variable `TZ`).
The other variables the image reads (README, [Quick start](../README.md#quick-start-docker-compose))
are left out: change the host port rather than `BUNKARR_PORT`.

### Host name

restic decides whether a repository lock is stale by the host name that wrote it, so every install
needs its **own, stable host name** ([One host name per install](../README.md#one-host-name-per-install)).
dockerMan has no host name field, so the template sets it in **Extra Parameters**:
`--hostname=bunkarr-tower`. Replace `tower` with this server's name in lower case (**Settings →
Identification**), so that no two servers share one, and never change it afterwards.

- Without the flag, the host name is the container ID, which changes whenever Unraid recreates the
  container (every update and every **Apply**). A lock the previous container left then looks like
  another machine's, and the retention and verify steps of restic destinations wait up to 30
  minutes for restic to call it stale. Bunkarr's log warns at start-up when the host name looks
  like a container ID, with the flag to add.
- Two containers with the same host name on one repository (a second server also left at
  `bunkarr-tower`, a test container) make Bunkarr stop the retention job rather than risk the
  other one's data.
- Check it with `docker exec bunkarr hostname`. The flag is saved with the container and kept when
  you edit it or update the image.

## 4. Unraid settings outside the template

### Docker Stop Timeout

Unraid stops every container (array stop, shutdown, reboot, an update, the Appdata Backup plugin)
with one global timeout, **Settings → Docker → Docker Stop Timeout**, 10 seconds by default, and
ignores the container's own `--stop-timeout`. Bunkarr needs up to about 45 seconds for a clean
stop: web requests in flight get 15 seconds, then running jobs get 20 seconds to stop and are
queued to resume, and restic removes its lock from a local repository. A container killed earlier
resumes its jobs as well, but **a job killed three times in a row fails**.

Set the timeout to **60** and **Apply**. Docker keeps running (do not disable it: that would stop
every container with the old timeout), and the new timeout applies from the next stop. The setting
applies to every container, but one that stops quickly is not slowed down.

### Unassigned Devices

- **`/backup` needs access mode *Read/Write - Slave*** (`rw,slave`, the template's default).
  Unassigned Devices mounts remote shares after Docker has started. With slave propagation the
  container sees the share when it appears; without it Bunkarr sees the empty mount point and
  refuses to sync (the destination marker is missing). Media on an Unassigned Devices disk or
  share needs *Read Only - Slave* for the same reason.
- At boot, Unraid does not autostart a container whose host path does not exist yet. If Bunkarr
  does not start after a reboot because the share's folder under `/mnt/remotes` is not there yet,
  map `/mnt/remotes` itself to `/backup` (still *Read/Write - Slave*) and use `/backup/NAS_backup`
  as the destination's target. Decide this before you add the destination in Bunkarr.
- **NFS is recommended.** Unassigned Devices mounts SMB shares with `nounix`, `noserverino` and
  `file_mode=0777`: such a share is case-insensitive, stores no hardlinks and does not enforce file
  modes, so the Plex token in the Plex DB backup's `Preferences.xml` and the arr backup zips (API
  keys, passwords) are readable by anyone who can read the share. Bunkarr refuses such a
  destination for Plex and arr backups until you tick *Accept insecure file modes on this
  destination*; restrict who can read the share on the NAS. The README's
  [Storage notes](../README.md#storage-notes) cover the rest.
- **A backup folder on this server.** A target on another `/mnt/user` share can sit on the same
  filesystem as the Appdata folder: the destination test then reports it as local, and saving it
  needs *Yes, back up to this local filesystem*. Such a copy does not survive a failure of this
  server, so use it only as an extra copy.

### Networks and webhooks

- On Unraid's default `bridge` network, containers cannot reach each other by name. Set the
  **Bunkarr address** on each connection's Webhook panel (Settings → Connect) to
  `http://SERVER-IP:8787` (or the host port you chose), not `http://bunkarr:8787`, which works only
  when both containers join the same custom Docker network.
- An arr container on `br0` (macvlan or ipvlan, with an IP address of its own) cannot reach the
  Unraid server's own address, and so neither Bunkarr's port nor its webhook URL, until
  **Settings → Docker → Host access to custom networks** is enabled (Docker must be stopped to
  change it).
- Plex usually runs on the host network: in Settings → Plex use `http://SERVER-IP:32400`.

### Reverse proxy

Keep **Settings → General → Authentication required: Enabled**, the default. Bunkarr has no
trusted-proxy setting yet: behind a reverse proxy (SWAG, Nginx Proxy Manager) every request comes
from the proxy's address, so *Disabled for local addresses* would let everyone who reaches the
proxy in without a login, and the login limiter counts all clients as one. Serve Bunkarr at the
root of a host name of its own (there is no URL base setting). The session cookie is marked
`Secure` when the proxy sends `X-Forwarded-Proto: https`.

## 5. First start

1. **Create your login right away**: container icon → **WebUI** (or `http://SERVER-IP:8787`).
   Until a user exists, whoever reaches Bunkarr first can create it; the container log warns
   meanwhile.
2. Follow the README's [First run](../README.md#first-run) with this template's container paths:
   Plex **Data path** `/plex`, path mappings from Plex's and the apps' paths to `/media` (for
   example `/data` → `/media`), destination target `/backup` (or a folder inside it).
3. For every off-site destination, export its **recovery kit** and keep it away from the server
   ([Encryption and the recovery kit](../README.md#encryption-and-the-recovery-kit)).

Maintenance:

- Unraid's **Console** (container icon → Console) is a **root** shell. Anything that writes
  Bunkarr's files, restic above all ([When restic reports damage](../README.md#when-restic-reports-damage)),
  runs as Bunkarr's user instead: `docker exec -it -u 99:100 bunkarr sh`.
- Locked out: `docker exec -it bunkarr /entrypoint.sh reset-auth` removes the login (not the API
  key); the next visit shows the first-run setup.
- Version: System → Status, or `docker exec bunkarr /entrypoint.sh version`.
- Logs: container icon → **Logs**, or `/mnt/user/appdata/bunkarr/logs/bunkarr.log` (JSON).

## 6. Back up Bunkarr's own data

Bunkarr does not back up its own database. Back up the Appdata folder, **`bunkarr.db` together
with `bunkarr.key`**, for example with the Appdata Backup plugin: the stored credentials (the Plex
token, API keys, destination credentials and encryption secrets) are sealed with the key, and a
database without its key cannot be decrypted. The plugin stops the container while it copies, so
the [Docker Stop Timeout](#docker-stop-timeout) matters here too. `cache/` can be left out (restic
rebuilds it); keep `backups/`, whose pre-migration copies are the only way back to an older
version.

The recovery kit of every off-site destination belongs in the same safe place, away from the
server: once `/config` is lost, the kit is the only way to read that backup.

## 7. Updating

- **Docker** tab → **apply update** (Unraid checks `:latest`). Running jobs are queued to resume,
  within the [Docker Stop Timeout](#docker-stop-timeout).
- Before a version changes the database schema, Bunkarr writes a copy of the database to
  `/config/backups`. To go back, follow [Upgrading and downgrading](../README.md#upgrading-and-downgrading)
  and set **Repository** to the older version's tag.
- **Installed containers keep their saved template.** A later release that adds a setting
  announces it in the template's change log (**Apps** → Bunkarr) and in
  [CHANGELOG.md](../CHANGELOG.md); add it by hand (**Edit** → **Add another Path, Port, Variable,
  Label or Device**). Your Extra Parameters, and with them the host name, are kept.
- To try a pre-release, set **Repository** to its tag (`ghcr.io/sl0wz3r/bunkarr:X.Y.Z-rc.N`), and
  back to `ghcr.io/sl0wz3r/bunkarr:latest` afterwards.

## Publishing to Community Applications

For maintainers. Community Applications lists templates from a public GitHub repository with an
OSI-approved license and a `ca_profile.xml` at its root, and the image must be pullable by anyone.
Everything public comes from **one** file, [`ca/publish.env`](ca/publish.env) (owner, repository,
registry, support link, version and date); the template and the profile are rendered from it
([`ca/README.md`](ca/README.md)):

```sh
make ca-template ca-profile   # render + validate unraid/bunkarr.xml and ./ca_profile.xml
make ca-validate              # offline: both files current and passing every rule (CI runs it too)
make ca-vars                  # every derived value, such as the URLs for the submission form
make ca-preflight             # online, read-only: public repository, license, raw URLs, icon, image amd64/arm64
```

- The ghcr.io package is created private by the first release run and must be made public on its
  package page (Package settings → Change visibility). That cannot be undone, and it makes every
  tag public, the `:candidate-<commit>` tags included.
- The submission form is at <https://ca.unraid.net/submit/new> (the public repository's URL, then
  Validate and Scan).
- Once listed, Community Applications reads the template from `main` at its next feed build.
  Installed containers keep their saved template: announce new settings in `<Changes>` and the
  release notes.
- Never rename or transfer the repository, rename the account, or move `unraid/bunkarr.xml` after
  the listing: Community Applications can blacklist the whole repository for it, and installed
  containers keep reading their `TemplateURL`.
- When the icon changes, run `make ca-icon` and append `?v=2` (then `?v=3`, ...) to `ICON_URL` in
  `ca/publish.env`: Unraid and Community Applications cache icons by URL.
- Releases (CHANGELOG, `<Changes>`, `VERSION`, `RELEASE_DATE`, the tag):
  [CONTRIBUTING.md](../CONTRIBUTING.md#releases).
