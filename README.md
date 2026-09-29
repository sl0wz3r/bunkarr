# Bunkarr

<p align="center">
  <a href="https://github.com/sl0wz3r/bunkarr/actions/workflows/ci.yml"><img alt="CI status" src="https://github.com/sl0wz3r/bunkarr/actions/workflows/ci.yml/badge.svg?branch=main"></a>
  <a href="https://github.com/sl0wz3r/bunkarr/actions/workflows/release.yml"><img alt="Release workflow status" src="https://github.com/sl0wz3r/bunkarr/actions/workflows/release.yml/badge.svg"></a>
  <a href="https://github.com/sl0wz3r/bunkarr/releases"><img alt="Latest release" src="https://img.shields.io/github/v/release/sl0wz3r/bunkarr?include_prereleases&amp;sort=semver"></a>
  <a href="https://github.com/sl0wz3r/bunkarr/pkgs/container/bunkarr"><img alt="Container image on ghcr.io (amd64, arm64)" src="https://img.shields.io/badge/ghcr.io-bunkarr-blue?logo=docker&amp;logoColor=white"></a>
  <a href="LICENSE"><img alt="License: GPL-3.0-or-later" src="https://img.shields.io/github/license/sl0wz3r/bunkarr"></a>
</p>

**Your library's bunker.** Bunkarr is a self-hosted, *arr-style backup app for Plex media libraries
and the *arr stack. It knows which media can be re-downloaded:

- **Irreplaceable data** (the Plex database, Sonarr/Radarr configuration and databases, personal
  media, rare content) gets **full, versioned backups**.
- **Common, easily re-acquired content** can get a **manifest-only backup** (TMDB/TVDB/IMDb IDs,
  quality profile, root folder, path), restorable by having Sonarr/Radarr download it again.
  Your [tier rules](#tiers) decide which content that is; until you add rules, everything is
  copied in full.

Bunkarr never modifies or deletes your source media.

> **Status: early development. Phase 4 (restic, rclone and remote destinations) is complete;
> Phase 5 (restore) is next.** Bunkarr mirrors your libraries to a mounted share (such as a
> UniFi UNAS over NFS or SMB) and, encrypted, to off-site storage (Backblaze B2, any
> S3-compatible service or an SFTP server) through restic or rclone; it backs up the Plex
> database and the Sonarr, Radarr and Lidarr configuration, backs up an *arr import within a
> minute through its webhook, and writes manifests of every *arr item. Tier rules decide, per
> destination, which files are copied in full and which are only listed in the manifests; with
> no rules (the default) every file is still copied in full. The restore wizard comes in Phase 5.
> See [the roadmap](#roadmap).

## Screenshots

The screenshots show a demo setup, not a real library: public-domain films and TV series as files
of random bytes, a scratch Plex Media Server and real Sonarr, Radarr and Lidarr in Docker (the
imports are tiny generated videos), and an NFS share ("UNAS") and an SMB share ("Offsite NAS")
served by containers. The off-site destinations are containers too: "Cloud bucket" is a restic
repository in a MinIO bucket (S3), "Friend's server" an rclone crypt remote on an OpenSSH SFTP
server; their keys and passwords were throwaway values. Tautulli, Seerr and Maintainerr are the
fakes from the test suite, serving their recorded answers with demo plays, requests and users;
"Sign in with Plex" ran against a fake plex.tv. No real media and no real account were involved.

<table>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/queue.png"><img src="docs/images/queue.png" alt="Activity queue: a sync to the UNAS share with its progress bar, bytes, speed, ETA and the file being copied, a Plex database backup running next to it and a verify waiting for the same share"></a>
      <p align="center"><b>Activity → Queue</b>: running jobs with progress, speed and the file being copied</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/history.png"><img src="docs/images/history.png" alt="Activity history, newest first: a manifest export, a full sync of the UNAS share and a Radarr backup, then the Radarr refreshes that webhooks started for a new film, its import and an upgrade, each followed by a one-path targeted sync to both shares, with status, duration and summary"></a>
      <p align="center"><b>Activity → History</b>: webhook-triggered refreshes and targeted syncs next to a full sync, an *arr backup and a manifest export</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/job.png"><img src="docs/images/job.png" alt="Detail of a targeted sync started by a Radarr webhook: counters, the item summary and its two items, the film's upgraded 1080p file copied and its old 720p file moved into retention"></a>
      <p align="center"><b>Job detail</b>: a Radarr upgrade backed up by a webhook: the new file copied, the old one kept in retention</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/library.png"><img src="docs/images/library.png" alt="Library with three sources, two imported from Plex, with file counts, size, unique size and hardlink groups, and the manifest export buttons"></a>
      <p align="center"><b>Library</b>: sources imported from Plex or added by hand; hardlinks are counted once</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/tiers.png"><img src="docs/images/tiers.png" alt="Settings, Tiers: the built-in Irreplaceable rule with its two flags (a Radarr movie and a folder, each with a note), then four rules in order: media tagged bunkarr-full in the *arrs is full, media requested in Seerr by two of the four users is full, films pending deletion in Maintainerr are manifest only, and at the Offsite NAS only everything else is manifest only; the built-in Everything else is full"></a>
      <p align="center"><b>Settings → Tiers</b>: ordered rules per destination; irreplaceable flags always win, and a file no rule matches is copied in full</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/preview.png"><img src="docs/images/preview.png" alt="Tier preview: for the Offsite NAS and the UNAS, the files and bytes stored, full, manifest only, skipped, unknown, to copy and kept, the files and size each rule decides and the config backups, then the Offsite NAS's files with their tier, the rule that decided and the facts behind it, such as a Seerr request or a Maintainerr deletion date"></a>
      <p align="center"><b>Preview</b>: what each destination would copy, list or skip, in files and bytes, and why for every file</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/library-item.png"><img src="docs/images/library-item.png" alt="A film in the library: its facts from Radarr, Plex, Tautulli (five plays), Seerr and Maintainerr, its irreplaceable flag, its full tier at both shares because of the flag, and its copies at each share"></a>
      <p align="center"><b>Library → a file</b>: the facts, flags and tier at each destination, and why</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/connect.png"><img src="docs/images/connect.png" alt="Settings, Connect: Lidarr, Radarr and Sonarr connections with their path mappings, refresh schedule, index status and backup settings; the Plex library index, Maintainerr, Seerr and Tautulli linked to the Plex server with their refresh schedules and fresh data; and an Apprise notification"></a>
      <p align="center"><b>Settings → Connect</b>: the *arrs with path mappings, index status and backups; Tautulli, Seerr and Maintainerr for the tier rules</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/webhook.png"><img src="docs/images/webhook.png" alt="The webhook panel of the Radarr connection: the webhook URL, Basic authentication with the webhook key as the password (hidden behind Show key), the triggers to tick and the recent events with the jobs they queued"></a>
      <p align="center"><b>Webhook</b>: the URL and key to paste into Radarr, and the events it sent</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/arr-backups.png"><img src="docs/images/arr-backups.png" alt="Radarr backup versions on the UNAS share: two verified zips taken from Radarr's Backups folder"></a>
      <p align="center"><b>*arr backups</b>: verified copies of Radarr's own backup zip, never offered for download</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/manifests.png"><img src="docs/images/manifests.png" alt="Manifests of the UNAS share: the day's newest version with item and file counts, listed size, integrity and JSON and CSV downloads, next to Export now, Preview and the current view as JSON or CSV"></a>
      <p align="center"><b>Manifests</b>: every *arr item and library file, versioned on each destination</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/plex-signin.png"><img src="docs/images/plex-signin.png" alt="Sign in with Plex: signed in, the account's owned and shared servers, and the chosen server's connections tested from Bunkarr: a recommended local HTTPS connection, its derived unencrypted LAN address, and a remote and a relay connection that did not answer"></a>
      <p align="center"><b>Sign in with Plex</b>: pick a server and one of its tested connections</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/plex.png"><img src="docs/images/plex.png" alt="Plex server form with a successful connection test, Sign in with Plex, and the manual URL, token and data path fields"></a>
      <p align="center"><b>Settings → Plex</b>: connection test, sign-in or URL and token, data path</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/destinations.png"><img src="docs/images/destinations.png" alt="Destinations list: a restic repository in an S3 bucket and an rclone crypt remote on an SFTP server, both encrypted with their recovery kits confirmed, their schedules, transfer window, upload limits and snapshot count, then an NFS and an SMB share with their capability badges; each with its last sync and buttons for test, preview, sync, verify, prune, locks, recovery kit, snapshots and manifests"></a>
      <p align="center"><b>Destinations</b>: off-site restic and rclone destinations next to the NAS shares, with schedules, window, limits and last sync</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/offsite-destination.png"><img src="docs/images/offsite-destination.png" alt="Add destination: where (S3-compatible storage chosen from a local folder, SFTP, S3 and Backblaze B2), how (restic, the default, or rclone), the MinIO endpoint, region, bucket and prefix, the access key ID and secret access key as masked password fields, a generated encryption password, and the test required before saving"></a>
      <p align="center"><b>Add an off-site destination</b>: S3 through restic; the keys are masked and stored encrypted, the password is generated</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/bandwidth.png"><img src="docs/images/bandwidth.png" alt="The bandwidth section of an off-site destination: a 12 MiB/s upload limit, a weekday 07:00 to 23:00 timetable line at 2 MiB/s, and a transfer window every day from 23:00 to 07:00 with 15 minutes of grace"></a>
      <p align="center"><b>Bandwidth</b>: upload and download limits, a weekly timetable and a nightly transfer window</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/recovery-kit.png"><img src="docs/images/recovery-kit.png" alt="Recovery kit of the Cloud bucket destination: not confirmed yet, so no backup runs; the kit was just downloaded after the Bunkarr password was typed (the storage credentials left out), and the check code printed in it is asked for next"></a>
      <p align="center"><b>Recovery kit</b>: download it with your password, then type its check code; until then no backup runs</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/queue-offsite.png"><img src="docs/images/queue-offsite.png" alt="Activity queue: a sync to the SFTP server uploading an episode at 5.4 MiB/s under its 6 MiB/s limit with its ETA, and a sync to the S3 bucket waiting for its transfer window, which opens at 01:30"></a>
      <p align="center"><b>Off-site sync</b>: upload speed under the limit, and a sync waiting for its transfer window</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/snapshots.png"><img src="docs/images/snapshots.png" alt="Snapshots of the restic destination per source: the batches of the first backup (partial) and the complete snapshots of later syncs, with files, data added and snapshot id, then a verified Plex database backup stored in the same repository"></a>
      <p align="center"><b>restic snapshots</b>: per source, a long first backup in batches, and the Plex DB versions in the same repository</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/retention-preview.png"><img src="docs/images/retention-preview.png" alt="Retention preview of the restic destination: nothing was deleted; ten snapshots the retention no longer keeps would be forgotten and two kept, each listed with its snapshot id and reason"></a>
      <p align="center"><b>Retention preview</b>: which snapshots the next retention run would forget, and which it keeps</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/destination-test.png"><img src="docs/images/destination-test.png" alt="Destination form with a test result for an SMB share: marker, filesystem, free space, capabilities and warnings about names it cannot store"></a>
      <p align="center"><b>Destination test</b>: what an SMB share can store, and what that means for your backup</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/plex-snapshots.png"><img src="docs/images/plex-snapshots.png" alt="Snapshots on a destination: Radarr and Sonarr backup zips and a Plex database backup, each with its size, integrity result and path"></a>
      <p align="center"><b>Snapshots</b>: verified Plex DB and *arr backup versions on the share</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <a href="docs/images/tasks.png"><img src="docs/images/tasks.png" alt="System tasks: sync, verify, Plex backup, retention, *arr refresh and *arr backup schedules with next and last run, Preview and Run now"></a>
      <p align="center"><b>System → Tasks</b>: every schedule, with Preview and Run now</p>
    </td>
    <td width="50%" valign="top">
      <a href="docs/images/status.png"><img src="docs/images/status.png" alt="System status with version, build, uptime, config directory and database"></a>
      <p align="center"><b>System → Status</b>: version, build and where Bunkarr keeps its data</p>
    </td>
  </tr>
  <tr>
    <td colspan="2" valign="top" align="center">
      <a href="docs/images/mobile.png"><img src="docs/images/mobile.png" alt="Bunkarr on a phone-sized screen: a sync whose deletions the mass-change guard held, with the Apply held changes button" width="220"></a>
      <p align="center"><b>Mobile</b>: a sync held by the mass-change guard, on a phone</p>
    </td>
  </tr>
</table>

## What it does today

- **Sources**: library folders, mounted read-only and never written to. Add them by hand or import
  them from Plex's libraries. Scans are incremental; hardlinked names within a source (for
  example cross-seeded torrents, or a download and its library copy when one source covers both
  folders) are detected, copied once and counted once. Hardlinks between two different sources
  are not matched yet, so such content is copied once per source.
- **Destinations**: a mounted folder, mirrored with the `filecopy` engine: one folder per source,
  every file written to a temp file, hashed, fsynced and renamed into place, hardlinks recreated
  where the share supports them.
- **Off-site destinations**: Backblaze B2, S3-compatible storage and SFTP servers (and restic
  repositories on a mounted folder), through **restic** (deduplicated, versioned snapshots) or
  **rclone** (a plain copy of the files). Encrypted by default (restic's own encryption, or rclone
  crypt) with a **recovery kit** that restores without Bunkarr; per-destination retention,
  bandwidth limits, timetables and transfer windows; verify jobs that read the data back. The Plex
  database, the *arr backups and the manifests can go off-site too. See
  [Off-site destinations](#off-site-destinations).
- **Nothing is lost**: files deleted at the source and old versions of changed files move into
  `.bunkarr/retention/` and are kept 30 days (per destination). A file Bunkarr did not write is
  never overwritten or deleted. When a Radarr/Sonarr upgrade's new file cannot be backed up, the
  old version stays in the live backup until the new one is.
- **Hardlink manifest**: `.bunkarr/links.tsv` at each destination lists every hardlinked name
  and the file that holds its content, so a restore without Bunkarr can recreate them.
- **Guards**: a sync refuses to run when the share is not mounted (marker file and filesystem type
  are checked) or a source looks unmounted (missing, empty, another filesystem); the mass-change
  guard holds unusually large deletes and shrinking files for you to confirm.
- **Preview** (dry run) of every sync, and of every scheduled task in System → Tasks (e.g. which
  retained files the expiry would delete); **resume** after a crash or restart; weekly **verify**
  that re-reads a sample of the backup against the recorded hashes.
- **Plex database backup** while Plex runs: library database, blobs database and
  `Preferences.xml`, verified, versioned (14 daily + 8 weekly by default).
- **Adopts an existing rsync copy** instead of copying everything again.
- **Schedules** (System → Tasks), **Activity** (queue with progress and ETA, history, job logs) and
  **Apprise notifications**.
- **Sonarr, Radarr and Lidarr** (Settings → Connect): Bunkarr keeps an index of their movies,
  series, artists and files, refreshed every 6 hours and at start-up. Their **webhooks** make an
  import or upgrade reach the backup within a minute: the webhook only says which item changed,
  Bunkarr asks the *arr about it and syncs just that folder. The full refresh catches up on
  anything a webhook missed. See [Sonarr, Radarr and Lidarr](#sonarr-radarr-and-lidarr).
- ***arr config backups**: each app's own backup zip (settings and database), read from its
  `Backups` folder (or downloaded over HTTP), verified and versioned at a destination. See
  [*arr config backups](#arr-config-backups).
- **Manifests**: a JSON and CSV list of every *arr item and every file, with what each
  destination holds, written after full syncs and on demand, so a library can be acquired again
  after a disaster. See [Manifests](#manifests).
- **Sign in with Plex**: pick your server after a plex.tv sign-in instead of pasting a token.
  Tokens stay on Bunkarr's server.
- **Tiers** (Settings → Tiers): ordered rules decide, per destination, whether a file is copied
  in full, only listed in the manifests, or left out, from its *arr tags, quality profile, root
  folder, genre, Plex library, size, age, Tautulli plays, Seerr requests or Maintainerr's pending
  deletions. Presets, a preview of what each destination would hold, **irreplaceable** flags that
  no rule can override, and demoted files that stay backed up until you release them. See
  [Tiers](#tiers).
- **Tautulli, Seerr and Maintainerr** (Settings → Connect): read-only connections whose play
  history, requests and pending deletions become facts for the tier rules, matched to your
  files through the Plex server they are linked to. See
  [Tautulli, Seerr and Maintainerr](#tautulli-seerr-and-maintainerr).

## Unraid

Once Bunkarr is listed in Community Applications: **Apps** → search **Bunkarr** → **Install**.
Until then, install its template ([`unraid/bunkarr.xml`](unraid/bunkarr.xml)) by hand, in the
server's terminal:

```sh
mkdir -p /boot/config/plugins/dockerMan/templates-user
wget -O /boot/config/plugins/dockerMan/templates-user/my-bunkarr.xml \
  https://raw.githubusercontent.com/sl0wz3r/bunkarr/main/unraid/bunkarr.xml
```

then **Docker** → **Add Container** → Template **bunkarr**. The template pulls
`ghcr.io/sl0wz3r/bunkarr:latest`, which arrives with the first full release (0.1.0). Until then
only pre-releases are published, each under its own tag: set **Repository** to one
(`ghcr.io/sl0wz3r/bunkarr:0.1.0-beta.1`, the first) to try it, or use the
[Quick start](#quick-start-docker-compose) below, which builds the image from the checkout.
Before **Apply**, check three things:

- **Extra Parameters** (Advanced View): `--hostname=bunkarr-tower`, with `tower` replaced by this
  server's name. Every install needs its own, stable host name
  ([One host name per install](#one-host-name-per-install)).
- **Settings → Docker → Docker Stop Timeout**: 60 seconds (Docker keeps running while you change
  it). Unraid ignores the template's `--stop-timeout=60` and kills a container after this
  timeout (10 seconds by default), and a job killed three times in a row fails.
- **Backup destination** (`/backup`): access mode *Read/Write - Slave*, so a share that
  Unassigned Devices mounts after Docker has started still reaches the container.

[`unraid/README.md`](unraid/README.md) explains every setting, the Unraid settings outside the
template, webhooks from containers on `br0`, reverse proxies, updating, and backing up
`bunkarr.key` with the database.

## Quick start (Docker Compose)

1. Get the compose file:

   ```sh
   git clone https://github.com/sl0wz3r/bunkarr.git
   cd bunkarr
   ```

2. **Before starting anything**, create `deploy/.env` with your paths and user. The values below
   are Unraid's; replace each with yours (the tables below explain them):

   ```sh
   PUID=99
   PGID=100
   TZ=America/New_York
   BUNKARR_CONFIG=/mnt/user/appdata/bunkarr
   MEDIA_DIR=/mnt/user/data
   PLEX_DIR="/mnt/user/appdata/plex/Library/Application Support/Plex Media Server"
   BACKUP_DIR=/mnt/remotes/UNAS_backup
   # this install's name, unique per install: the container's host name becomes bunkarr-tower
   SERVER_NAME=tower
   # optional: the *arr apps' Backups folders (uncomment their lines in the compose file too)
   SONARR_BACKUPS=/mnt/user/appdata/sonarr/Backups
   RADARR_BACKUPS=/mnt/user/appdata/radarr/Backups
   LIDARR_BACKUPS=/mnt/user/appdata/lidarr/Backups
   ```

   [`deploy/docker-compose.yml`](deploy/docker-compose.yml) refuses to start while `PUID`,
   `PGID`, `TZ`, `SERVER_NAME`, `MEDIA_DIR`, `PLEX_DIR` or `BACKUP_DIR` is missing, so Docker
   never creates empty folders at guessed paths and no two installs share a host name.
   `BUNKARR_CONFIG` defaults to `/mnt/user/appdata/bunkarr`. Not using the Plex DB backup? Delete
   the `/plex` line from the compose file. Git ignores `.env`; a variable set in your shell (often
   `TZ`) takes precedence over it.

3. Check what Compose will run, then start it:

   ```sh
   docker compose -f deploy/docker-compose.yml config   # shows the resolved mounts and variables
   docker compose -f deploy/docker-compose.yml up -d --build
   ```

4. Open `http://<host>:8787`. On first run Bunkarr asks you to create a username and password;
   the UI and API are closed until you do (the API key works from the start for automation).

| Mount (`.env` variable) | Purpose |
|---|---|
| `/config` (`BUNKARR_CONFIG`) | Bunkarr's database, `bunkarr.key`, logs, Plex DB staging (needs free space for one copy of the Plex database). Use an absolute appdata path outside the checkout (a folder in it would enter the image build context). **Back up `bunkarr.key` with the database**: stored credentials cannot be decrypted without it. |
| `/media` (`MEDIA_DIR`, `:ro`) | Your library. Mount the folder that holds both downloads and library (e.g. `/mnt/user/data`) as **one volume** so hardlinks are detected. |
| `/plex` (`PLEX_DIR`, `:ro`) | Plex's data directory, `…/appdata/plex/Library/Application Support/Plex Media Server`, for the Plex DB backup. Read-only is enough. |
| `/backup` (`BACKUP_DIR`, `:rw,slave`) | The destination, e.g. the UNAS share mounted on the host (`/mnt/remotes/…`). Unassigned Devices mounts remote shares after Docker starts; `slave` propagation lets the container see the share when it appears. Without it Bunkarr sees an empty folder (and refuses to sync). |
| `/arr/<app>-backups` (`SONARR_BACKUPS`, `RADARR_BACKUPS`, `LIDARR_BACKUPS`, `:ro`; optional) | The `Backups` folder of each *arr you connect (its `/config/Backups`, e.g. `/mnt/user/appdata/radarr/Backups`), for the *arr config backups. The lines are commented out in the compose file: uncomment the ones you need. The folder must exist before the container starts; the app creates it with its first backup (System → Backup → Backup Now). See [*arr config backups](#arr-config-backups). |

| Variable | Compose file | Image default | |
|---|---|---|---|
| `PUID` / `PGID` | required | `1000` / `1000` | User/group Bunkarr runs as. **Use the user Plex runs as** (Unraid: `99` / `100`): Plex keeps `Preferences.xml` at mode 0600. |
| `UMASK` | `022` | `002` | |
| `TZ` | required | `Etc/UTC` | e.g. `America/New_York`. Use Plex's time zone (the Plex backup checks Plex's maintenance hours in it). |
| `BUNKARR_PORT` | — | `8787` | Web UI and API port inside the container. |
| `BUNKARR_BIND` | — | all interfaces | Listen address. |
| `BUNKARR_ALLOWED_HOSTS` | optional | — | Extra host names of this server, comma-separated (for example `tower.lan,bunkarr.example.com`). An IP address, `localhost`, a single-label name (`tower`) and names under `.local`, `.home.arpa` or `.internal` always work. Under any other name the first-run setup is refused and *Disabled for local addresses* does not apply (log in instead), because a web page can point a name of its own at your server's address (DNS rebinding). A name listed here gets the local-address bypass too, so do not list a reverse proxy's name if that bypass is on. |
| `BUNKARR_LOG_LEVEL` | — | `info` | `debug`, `info`, `warn`, `error`. |
| `BUNKARR_LOG_FORMAT` | — | `text` | stdout format (`text` or `json`); `/config/logs/bunkarr.log` is always JSON. |
| `BUNKARR_RESTIC_PATH` / `BUNKARR_RCLONE_PATH` | optional | the image's `restic` / `rclone` | Another restic or rclone program: an absolute path to a file the Bunkarr user cannot write, without spaces or quotes. It receives every off-site destination's secrets, so it is only ever set here, never in the UI. |

The example sets `stop_grace_period: 60s`: on a clean stop running jobs are queued to resume, and
restic gets the time to remove its lock from a local repository. An off-site (S3, B2, SFTP)
repository keeps the stopped job's lock (its rclone backend stops with restic); Bunkarr removes
that lock before its next retention or verify step. A killed container resumes its jobs as well,
but a job killed three times in a row fails. It also sets `hostname: bunkarr-<server name>`: with
off-site restic destinations every install needs its own, **stable host name** (see
[One host name per install](#one-host-name-per-install)).

### Storage notes

- **Unraid `/mnt/user`** is a FUSE filesystem: its inode numbers identify hardlinks only when
  "Tunable (support Hard Links)" is on (Bunkarr then also compares the first and last MiB). A
  pool or disk path (`/mnt/cache/data`) avoids the question.
- **NFS is the recommended way to mount the UNAS share.** SMB without POSIX extensions is
  case-insensitive, rejects `\ : * ? " < > |` and names ending in a dot or space, and usually has
  no hardlinks. Bunkarr probes the share when you add it: files it cannot store fail with a
  warning (two names that differ only in case never overwrite each other) and everything else
  syncs. Mount SMB with POSIX extensions or the `mapposix` option to store such names. Such a
  share also ignores file modes, which matters for the Plex token in the Plex DB backup (see
  [Restoring](#restoring)).
- **SMB with `noserverino`** (Unraid's Unassigned Devices mounts shares so) numbers inodes per
  lookup: where the share has hardlinks, the names of one file show different inode numbers.
  The probe detects this (the destination test shows *Inode numbers not stable (content
  compared)*) and Bunkarr then decides by content whether two names are one file, which costs
  extra reads when a job resumes an interrupted replacement or retains a hardlinked name. Because
  a share can be remounted, or the CIFS client can turn server inode numbers off by itself, a
  sync, verify or retention job checks this again before it first compares two names (a few small
  files in `.bunkarr/probe`) and stores a change; a dry run does not write, so it just compares by
  content.
- A sync checks free space before copying (new data plus 1 GiB).

### Locked out?

```sh
docker exec -it bunkarr /entrypoint.sh reset-auth
```

This removes the login (not the API key or anything else); the next visit shows the first-run setup.

## First run

1. **Login.** Create the user on the first visit.
2. **Plex** (Settings → Plex, optional). Add the server, either way:
   - **Sign in with Plex**: approve Bunkarr in the plex.tv window that opens, pick your server,
     and Bunkarr tests its connections (Local/Remote/Relay, HTTPS or Unencrypted) and recommends
     one. "Use" takes it; an unencrypted (`http`) or failing connection needs "Use anyway". See
     [Sign in with Plex](#sign-in-with-plex).
   - **Or enter the details manually**, as before:
     - URL: use the server's IP, `http://192.168.1.10:32400`. A claimed server with a token also
       works by container name; an unclaimed one answers 401 to a name it does not know.
     - Token: your `X-Plex-Token` (write-only; only ever sent to this URL).
   - Data path: `/plex`.
   - Path mappings: Plex's path → Bunkarr's path for the same folder, e.g. `/data` → `/media`.
   - Test.
3. **Sources** (Library). *Import from Plex* lists each library folder with its path as Bunkarr
   sees it, or *Add source* by hand. Each source is mirrored into its **destination folder**
   (default: the source name as a slug). Switching from rsync? Set the destination folder to the
   folder name your rsync job writes to. Scan.
4. **Destination** (Destinations → Add). Target `/backup` (or a folder inside it), **Test** (shows
   the filesystem, capabilities, free space and warnings), pick the sources, then save. Bunkarr
   writes `.bunkarr/destination.json` into the target; every job checks it, so an unmounted share
   is never mistaken for an empty backup.
5. **Adopt the existing rsync copy** (if you have one):
   - Disable the rsync job first.
   - In the destination form, keep *Adopt existing files* at "When size and modification time
     match" (rsync `-a` keeps modification times). If your copy did not keep them, choose "size and content hash" (reads
     both sides once). Set *Time window* to 1–2 s for FAT or older SMB servers.
   - Click **Preview**: the job's items should be mostly *adopt*, not *copy*. A file that does not
     match is moved into retention (never overwritten) and copied again.
   - Then **Sync now**. Adopted files are recorded without being copied.
6. **Schedules.** Choose the sync schedule in the destination form (it proposes nightly 02:00;
   no sync is scheduled until you save one). New destinations get a weekly verify (Sunday
   05:00). For the Plex DB backup, pick the destination in Settings → Plex (daily 06:00 by
   default, outside Plex's 02:00–05:00 maintenance window; the form warns on overlap). Expiry of
   retained files and old job history runs daily at 04:30. System → Tasks lists them all (edit,
   disable, Run now, **Preview**). Preview starts a dry run and opens it: for the expiry task it
   queues one dry run per destination, whose items are the retained files a real run would delete
   (the global job's log links them). Nothing is deleted, and a preview does not count as a run
   of the schedule. API: `POST /api/v1/schedules/{id}/run` with `{"dryRun": true}`.
7. **Sonarr, Radarr, Lidarr** (Settings → Connect, optional): connect each app, paste the
   webhook into it and, for its config backups, set its Backups folder. See
   [Sonarr, Radarr and Lidarr](#sonarr-radarr-and-lidarr).
8. **Notifications** (Settings → Connect, optional): an Apprise API for failures and warnings.

## Sonarr, Radarr and Lidarr

Settings → Connect → Sonarr, Radarr or Lidarr → **Add**. Bunkarr only reads from the apps (and
asks them to make their own backups); it never changes their settings, items or files.

### Connecting

- **URL**: how Bunkarr reaches the app, including any URL base: `http://192.168.1.10:7878`, or
  `http://radarr:7878/radarr` on the same Docker network (Sonarr's port is 8989, Lidarr's 8686).
- **API key**: the app's Settings → General → Security → API Key. Stored encrypted, never shown
  again, and sent only in a request header to this URL.
- **Path mappings**: the app reports paths as it sees them inside its container. Map each of its
  root folders to where Bunkarr sees the same folder. Both sides are the container paths of one
  host folder:

  | The app mounts | Its root folder | Bunkarr mounts | Mapping (app path → Bunkarr path) |
  |---|---|---|---|
  | `/mnt/user/data:/data` | `/data/media/movies` | `/mnt/user/data:/media` | `/data` → `/media` |
  | `/mnt/user/data/media/movies:/movies` | `/movies` | `/mnt/user/data:/media` | `/movies` → `/media/media/movies` |
  | `/mnt/user/data/media:/media` | `/media/movies` | `/mnt/user/data/media:/media` | `/media` → `/media` |

  A path no mapping covers is unmapped, so add a mapping even when both sides are the same. The
  mapped folder must be inside one of Bunkarr's sources (Library): Bunkarr backs up sources,
  and the app tells it which folder in them changed. An item whose folder is in no source is
  still indexed and listed in [manifests](#manifests), but nothing of it is copied.
- **Test** checks that the URL answers as the chosen app and accepts the key, then shows each
  root folder with its Bunkarr path and source, and backup access (see
  [*arr config backups](#arr-config-backups)). It also warns when:
  - the app's **recycle bin** lies inside a source that does not exclude it (every upgrade's old
    file would be copied twice; the warning offers to exclude it);
  - **Change File Date** is not None (every version of a title then gets the same modification
    time, so a replacement of the same size can look unchanged).
- The connection card shows the index (items, files, last refresh) and **Unmapped files**: files
  of the app that map to no path, lie in no source, or are not in the catalog yet (scan the
  source).
- **Full refresh**: every 6 hours (`15 */6 * * *`) and at start-up, Bunkarr reads all of the
  app's items and files, updates its index and queues a sync of each folder that changed. It
  catches up on everything a webhook missed.

### Webhook

After you save the connection, its **Webhook** panel shows what to paste into the app. In the
app: Settings → Connect → **+** → **Webhook**:

| App field | Value |
|---|---|
| Name | anything, e.g. `Bunkarr` |
| Triggers | the ones the panel lists (below) |
| Webhook URL | the panel's URL, `http://<bunkarr address>/api/v1/webhook/<app>/<id>`. The panel builds it from *Bunkarr address*, which is how the app reaches Bunkarr: `http://bunkarr:8787` on the same Docker network, otherwise the host's IP and port. |
| Method | `POST` |
| Username | anything, e.g. `bunkarr` |
| Password | the connection's **webhook key** (Show key) |

Then press **Test** in the app: the panel shows *Last Test received* and the recent events.

- **The webhook key belongs to this one connection.** It works only on its webhook route, and
  Bunkarr's own API key is refused there, so the app (and its backups, which hold its settings)
  never holds a key that controls Bunkarr. Basic auth (the key as the password) is recommended
  because the app keeps its password field private; the `X-Api-Key` header also works.
  `?apikey=<key>` in the URL works too but ends up in proxy and access logs. **Regenerate key**
  stops the old key at once: paste the new one into the app (events sent meanwhile are refused;
  the next full refresh catches up).
- **Triggers:**
  - Radarr: On Import, On Upgrade, On Rename, On Movie Added, On Movie Delete, On Movie File
    Delete, On Movie File Delete For Upgrade.
  - Sonarr: On Import, On Upgrade, On Rename, On Series Add, On Series Delete, On Episode File
    Delete, On Episode File Delete For Upgrade.
  - Lidarr: On Release Import, On Upgrade, On Rename, On Track Retag, On Artist Add, On Album
    Delete, On Artist Delete. Lidarr sends no webhook for a manual import unless "Replace
    existing files" is ticked, and none when a track file is deleted; the full refresh finds
    those changes.
- **What a webhook does.** It is a hint, not the truth: its payload only chooses which movie,
  series or artist to look at. Bunkarr waits 5 s for more events of the same item (at most 20 s),
  reads the item from the app's API, then scans and syncs just its folder. A delete waits 60 s,
  because the app sends it before it removes the folder; an upgrade's file delete waits for the
  import that follows it (up to 30 min), so the new file is copied before the old one moves into
  retention. A webhook sync moves a vanished file into retention only when its folder got new
  content (an upgrade or a rename); other deletions wait for the next full sync.
- **Webhooks can be missed** (Bunkarr stopped, a network error, an event type the app does not
  send). Nothing is lost: the full refresh and the scheduled syncs catch up.
- **How fast.** With a free worker and destination, an import starts copying within about 30 s
  of the app finishing it. What delays it, all common at night when the apps import:
  - a full refresh of the same connection (Sonarr needs two requests per series);
  - both job workers busy (for example a nightly sync and a weekly verify);
  - another destination's sync scanning the same source;
  - a sync, verify or retention job of the same destination (a verify of a large backup can take
    hours);
  - a file not yet visible through Bunkarr's mount (NFS attribute cache): up to 60 s more.

## *arr config backups

Each connection's **Backup** section copies the app's own backup zip (its settings and
database) to a destination, verifies it (zip structure, checksums, database integrity) and keeps
versions. Choose the destination to turn it on: it runs weekly (Sunday 06:30) by default, and
**Back up now** on the connection card runs it at once.

**Mount the app's Backups folder** (recommended). With a login required (Authentication: Forms,
the default), an *arr does not let its API key download backups, so Bunkarr reads the zips from
the folder instead. Bunkarr never stores the app's UI password.

1. Find the app's `Backups` folder on the host: its `/config/Backups`, on Unraid
   `/mnt/user/appdata/radarr/Backups`. The app creates it with its first backup (System → Backup
   → Backup Now).
2. Set the variable in `deploy/.env` and uncomment the app's line in
   [`deploy/docker-compose.yml`](deploy/docker-compose.yml) (read-only):

   ```yaml
   - "${RADARR_BACKUPS:?set RADARR_BACKUPS in deploy/.env (Radarr's Backups folder)}:/arr/radarr-backups:ro"
   ```

3. Recreate the container: `docker compose -f deploy/docker-compose.yml up -d`.
4. Settings → Connect → Radarr → Backup → **Backups folder** `/arr/radarr-backups`, then Test:
   *Backups folder: readable*. "Not readable" means the files are not readable by `PUID`.

Without a Backups folder Bunkarr downloads the zip over HTTP with the API key. That works only
when the app does not require a login for Bunkarr's address (Settings → General → Security →
Authentication Required: "Disabled for Local Addresses"); otherwise the job fails and says so.

- **Manual backups pile up in the app.** The apps prune only their scheduled backups (every
  7 days by default); a backup made on request is a manual one and stays in `Backups/manual`
  for good. So when the app's newest scheduled backup is younger than *Reuse scheduled backups*
  (7 days), Bunkarr copies it and asks for nothing; only otherwise does it ask the app for a new
  backup. Keep Bunkarr's schedule weekly. The card shows how many manual backups the app holds
  and warns above 10: delete old ones in the app (System → Backup).
- **The zips are sensitive.** They hold the app's API key and the passwords of its indexers and
  download clients. Bunkarr writes them with mode 0600 in 0700 folders, but does not encrypt
  them (a backup only Bunkarr's key could open would fail exactly when Bunkarr's config is
  lost). An SMB share mounted without POSIX extensions ignores file modes, so Bunkarr refuses
  such a destination unless you tick *Accept insecure file modes*. Restrict who can read the
  share, or its `.bunkarr/arr` folder, on the NAS.
- **Where:** `<target>/.bunkarr/arr/<connection>-<id>/<time>/`, the zip as the app made it and a
  `manifest.json` (its sha256, entries and integrity result). Versions kept: 14 daily and
  8 weekly (per destination: Destinations → Retention).
- **Restore:** in the app, System → Backup → Restore Backup, and upload the zip (the app
  restarts with that configuration and database).

## Manifests

A manifest is the disaster record of your *arr library: every Sonarr, Radarr and Lidarr item
(movies; series with their seasons and episodes; artists with their albums) with its IDs (TMDB,
TVDB, IMDb, MusicBrainz), title, year, quality profile, root folder, monitored state and tags,
and every file with its path, size and quality, plus its [tier](#tiers) at this destination and
whether this destination holds it (`backedUp`, `sha256`, or `kept` for a demoted file). Library
files that belong to no item are listed too (except `skip`-tier extras the destination does not
hold). With it, a library can be acquired again after a disaster, including anything that was
never copied.

- **Where:** `<target>/.bunkarr/manifests/<time>/`:
  - `manifest.json`, the canonical form;
  - `manifest.csv`, one row per file (and per item without files) for a spreadsheet; a cell that
    starts with `=`, `+`, `-` or `@` gets a leading `'`, so read the JSON when exact values
    matter;
  - `SHA256SUMS` for both.
- **When:** after each full sync (when *arr connections exist) and on **Export now**. An
  unchanged manifest writes no new version. Versions kept: the newest of each of the last
  30 days and 12 weeks (Destinations → Retention). A manifest ends *completed with warnings*
  when a connection's index is stale or an item's folder is in no source.
- **In the UI:** Destinations → the **Manifests** button of a destination lists its versions
  with their counts, downloads each as JSON or CSV (checked against its checksums first; a
  damaged one is reported, not downloaded), and has **Export now**, **Preview** and *Current
  view* (built on the spot). Library → **Export manifest** builds one of every enabled source,
  without the per-destination fields.
- **API:** `GET /api/v1/manifest/export?format=json|csv[&destinationId=N]` (built on the spot;
  the `X-Bunkarr-SHA256` header carries its hash), `GET /api/v1/destinations/{id}/manifests` and
  `GET /api/v1/manifests/{id}/download?format=json|csv`.
- **Without Bunkarr:** check a version, then query it, for example every item with a file the
  destination does not hold (kind, title, year, IDs):

  ```sh
  cd /path/to/target/.bunkarr/manifests/<time>
  sha256sum -c SHA256SUMS
  jq -r '.items[] | select(any((.files // [])[]; .backedUp == false))
         | [.kind, .title, .year, (.externalIds | tojson)] | @tsv' manifest.json
  ```

  A tool that adds the items back to a fresh *arr is part of the Phase 5 restore wizard; until
  then the manifest gives each item's IDs, root folder, quality profile and monitored state to
  add it by hand.

## Tiers

Settings → Tiers decides, for each file and each destination, one of three **tiers**:

- **Full**: copied, versioned and kept in retention, as in every earlier phase.
- **Manifest only**: not copied, but listed in that destination's [manifests](#manifests) so it
  can be acquired again.
- **Skip**: not copied. A file of an *arr item is still listed under its item; other skipped
  files (extras, files outside any item) are only counted.

**The default is full.** With no rules, every file is copied in full to every destination, exactly
as before Phase 3. Plex DB and *arr config backups are always full: they are not tiered.

**Rules.** A rule has a name, conditions, a match mode (*all* or *any*), an action (a tier) and
*Applies at*: all destinations or the ones you pick. At a destination, the enabled rules that
apply there are taken in order and **the first one that matches decides**; a file no rule matches
is full (the built-in *Everything else*). A rule not applied at a destination is simply not
evaluated there, which is not the same as *skip*: "full at the UNAS only" is one rule *full* at
the UNAS plus a second rule with the same conditions, *skip* at the other shares. Up to 100 rules
of up to 32 conditions each.

| Condition | From |
|---|---|
| *arr tag, quality profile, root folder, monitored; managed by an *arr | Sonarr, Radarr, Lidarr |
| Genre | the *arr item |
| Plex library | the source's Plex library, or the Plex library index |
| Source, file size, added (days) | Bunkarr's catalog (the *arr's or Plex's date added first) |
| Play count, last watched | Tautulli |
| Requested, requested by | Seerr (users by id only) |
| Pending deletion | Maintainerr |
| Flagged irreplaceable | your flags (below) |

- **Unknown never lowers protection.** A condition whose facts are unknown (a stale cache, two
  *arrs claiming the file, a file Plex has not indexed yet) is neither true nor false. When an
  earlier, more protective rule might have matched, it decides instead (*full* > *manifest
  only* > *skip*), and the file is shown as *unknown*. Copies that are full only because of an
  unknown fact count toward the [mass-change guard](#the-mass-change-guard), so a stale Radarr
  cannot flood a destination, and they never fail a sync for lack of free space: they are held
  instead.
- A file outside every *arr root folder (home videos, a Plex-only source), while the *arr
  caches are fresh, is **not managed**: its *arr conditions are false, not unknown.
- **Sidecars** (`.srt`, `.nfo`) follow their media file; the names of a **hardlink** group all
  take the most protective tier among them.

**Presets** (the *Preset* menu, then **Load into editor**) only fill the editor; nothing changes
until you Preview and Save:

- **Back up everything**: no rules (the default).
- **Manifest by default** (opt-in; the original spec's default): media tagged `bunkarr-full` in
  Sonarr, Radarr or Lidarr is full, everything else manifest only. This **stops copying new
  untagged media**, and a file outside every *arr root folder becomes manifest only too; a file
  whose *arr facts are unknown stays full. Tag what you want copied before you save it.
- **Keep Maintainerr deletions as manifest only**: what Maintainerr is about to delete is not
  copied but stays listed. Maintainerr has no authentication, so this preset never skips.

**Preview** evaluates the rules in the editor, saved or not, over every file of every enabled
destination, and shows per destination what is *stored* now, what would be full, manifest only
and skip, what is full only because a fact is unknown, what the next sync would *copy*, and what
would be *kept*, with counts per rule, the caches that are not fresh and any rule value (a tag, a
profile) that no index knows. Each file lists the reasons for its tier. Nothing is written.
**Save** takes effect at each destination's next sync; a save based on rules someone else changed
meanwhile is refused. Library → a file shows its tier at each destination and why.

**When a file stops being full** (a rule changed, a tag was removed), nothing is deleted: the
copy the destination holds is **kept**, and syncs leave it alone. Settings → Tiers then offers
**Release N files at <destination>…**. A release first runs a preview (a dry run) listing the kept
files it would free; on that job's page, **Apply release** moves exactly those files into the
destination's retention folder, where they expire after the retention period (30 days by
default). Files that are full again, or a preview made before the rules changed again, are not
released. A kept file that is deleted at the source, or replaced by a file of another name (an
upgrade), goes into retention as usual; the new file is copied only when its tier is full.

**Irreplaceable.** Library → a file → **Mark irreplaceable** flags the file's *arr item (by
default, when it has one: the flag follows the item across *arrs by its TMDB, TVDB or
MusicBrainz ID, and falls back to its last folder when the item is removed), the file, or its
folder (flags follow renames). A flagged file is full at every destination whatever the rules
say, and its retained copies never expire while the flag exists. Settings → Tiers →
*Irreplaceable* → *Flags* lists every flag and whether it still resolves.

## Tautulli, Seerr and Maintainerr

Settings → Connect → **Tautulli, Seerr and Maintainerr**. Bunkarr only **reads** from them, with
a fixed list of requests, and turns what it reads into facts for the [tier rules](#tiers):

| App | Version | Reads | Linked Plex server | Refresh (stale after) |
|---|---|---|---|---|
| Tautulli | ≥ 2.18 | play history per library: play count, last watched | required: the server it watches (the Test checks it) | daily 02:00 (72 h) |
| Seerr | ≥ 3 (Overseerr, Jellyseerr best effort) | requests (movie, series, seasons) and requesting user ids; no names or e-mail addresses | optional: used when a file has no TMDB or TVDB ID | daily 02:30 (72 h) |
| Maintainerr | ≥ 3.4 | the collections' items that are pending deletion | required: the server it manages | every 6 h (24 h) |

- **API keys** are write-only: Tautulli (Settings → Web Interface → API key) and Seerr (Settings →
  General → API Key). Maintainerr has **no API authentication**: anyone who can reach it can add
  items to a deleting collection, so rely on its facts with care and keep it off untrusted
  networks.
- **Plex server scoping.** Tautulli's plays and Maintainerr's collections name Plex items, and a
  Plex rating key only means something on its own server. Each connection is therefore linked to
  one Plex server, and its facts are matched to your files only through **that server's library
  index**, never another's. Linking a server offers to turn its index on (daily at 01:00, stale
  after 72 h; Settings → Connect shows it next to the connections). The index also serves the *Plex
  library* condition and Plex's added dates.
- **Freshness.** A connection whose last complete refresh is older than its *stale after* hours
  (or whose linked Plex index is stale) makes its conditions unknown, which never lowers
  protection. **Refresh now** on each card refreshes it at once.
- **Tautulli history turned off** for a user or a library (Tautulli's *Keep history*) makes play
  counts there a lower bound: "played more than N" can still be true, but "played fewer than N"
  and "never watched" become unknown. The Connect card says how many users and libraries this
  affects.

## Sign in with Plex

Settings → Plex → **Sign in with Plex**, instead of pasting a token:

1. A plex.tv window opens: sign in there and approve Bunkarr (Bunkarr never sees your password).
2. Pick your server. Bunkarr tests each connection plex.tv lists for it, from where Bunkarr runs,
   and adds a plain `http://<ip>:32400` for each local `*.plex.direct` address (many routers'
   DNS-rebind protection blocks those names). Working connections come first: local, then
   remote, relays last; HTTPS before HTTP.
3. **Use** takes the recommended connection; an unencrypted (`http`) or failing one needs
   **Use anyway**. Save.

- **Tokens stay on Bunkarr's server.** The browser never receives your account token or a server
  token. Bunkarr saves the server's own access token (encrypted) and sends a token only to a
  connection that answered as your server, and only over HTTPS until you choose *Use anyway*.
- If plex.tv lists no token for a server you own, Bunkarr can save your **account token** for it
  (tick *Use my Plex account token*). That token controls your whole Plex account and stays valid
  until you sign out of all devices.
- **A server shared with you** can be used to import libraries, but Bunkarr cannot read its
  settings (no maintenance-window check), and backing up its database needs its data folder
  mounted into Bunkarr. If plex.tv lists no token for it, Bunkarr never sends your account token
  to it: enter the server's URL and a token manually.
- **Network:** Bunkarr calls plex.tv and clients.plex.tv (HTTPS) only while you sign in; otherwise
  it talks only to your server's URL. A sign-in lasts 10 minutes and does not survive a restart.
- **Revoking:** Bunkarr appears on plex.tv under Settings → **Authorized Devices** as *Bunkarr*
  on *Docker* (each install has its own client identifier). Remove it there to revoke the
  sign-in; afterwards sign in again or enter a token manually.
- Entering the URL and token manually works as before (see [First run](#first-run)).

## Off-site destinations

Destinations → Add → **Where** offers a local or mounted folder, an **SFTP server**,
**S3-compatible storage** (AWS, MinIO, Wasabi, Cloudflare R2 and others) or **Backblaze B2**;
**How** picks the engine. A mounted folder keeps the Phase 1 `filecopy` mirror by default (a
restic repository on it is possible too). Remote destinations use:

- **restic** (the default): a deduplicated, encrypted repository. Each sync adds one snapshot per
  source; unchanged data is never uploaded again, renamed files cost nothing, and restic compresses.
  Restores go through restic (a file, a folder, or a whole source at any snapshot).
- **rclone**: a plain copy of your files under the same layout as a mounted destination
  (`<destination folder>/<path>`, `.bunkarr/retention/`, `.bunkarr/links.tsv`), wrapped in rclone
  crypt by default. A restore is a plain `rclone copy`; renames are server-side moves; there is no
  deduplication.

Both engines keep the Phase 1 promises: a file deleted at the source stays at the destination for
the retention period (30 days by default), an update keeps the old version for as long, the
mass-change guard holds unusually large changes, and nothing is recorded as backed up until it was
read back from the destination. On restic, retention also keeps whole snapshots: the newest one of
each of the last 7 days, 4 weeks and 6 months that have one (per destination, 0 to turn a bucket
off), and always the newest snapshot and every snapshot a retained file still needs.

**Kept files** (a file that stopped being `full`, see [Tiers](#tiers)) behave differently on
restic: a snapshot cannot keep one old file without keeping the whole snapshot, so a kept file
whose source changed is backed up again, and its old version is kept like any replaced version
(for the retention period). On rclone and on mounted folders a kept file stays exactly as it was
until you release it.

Every change that chooses **where data goes** needs your password, in the UI session: creating an
off-site destination, changing its credentials, SFTP host keys or CA certificate, linking a
source or a Plex/*arr backup target to it, and accepting no encryption. The API key and the
local-address bypass cannot do these (403); they can still run the jobs of existing destinations.

### Encryption and the recovery kit

Off-site destinations are encrypted by default: restic always encrypts, and rclone wraps the
remote in `crypt` (file contents and names; sizes, times and the folder shape stay visible to the
provider). Bunkarr generates the secret (or you type your own, at least 16 characters); it is
sealed in the database with `bunkarr.key` and **can never change**.

Without that secret the backup cannot be read, and Bunkarr keeps it only in `/config`. So after
creating an off-site destination, **export its recovery kit** (the destination card → Recovery
kit, with your password) and **type the check code** printed in it. Until you do, the destination
runs nothing but previews, its card shows a red banner, and a daily notification reminds you
(a secret you typed at create is confirmed by typing it again instead, except a crypt password
typed with a password2, which needs the check code). The kit is a text file
with the destination's location, the secret (and, if you tick the box, the storage credentials),
the check code, the layout, and step-by-step commands to list and restore without Bunkarr.

- Keep the kit **in a password manager and on paper**, away from the server. Whoever holds the kit
  (and access to the storage) can read the whole backup; every export sends a notification.
- **Bunkarr cannot recover a lost kit** once `/config` is gone: there is no reset. Export it again
  while you still have the server.

### Restoring without Bunkarr

In an empty folder on any machine with restic and rclone installed, paste the kit's shell block
(between `BEGIN SHELL` and `END SHELL`) into a POSIX shell; it writes the password file, the
`rclone.conf` and, for SFTP, the pinned host keys. Then:

```sh
# restic
restic snapshots --tag 'bunkarr-dest:<tag from the kit>'
restic restore <snapshot>:<source path> --target <directory>      # a whole source
restic dump <snapshot> '<source path>/<file>' > <file>             # one file
# rclone crypt
rclone lsd bunkarr-crypt:
rclone copy 'bunkarr-crypt:<destination folder>' <directory>
```

Hardlinked names are recreated from `.bunkarr/links.tsv` as described in [Restoring](#restoring)
(restic restores hardlinks within one restore by itself). The Plex DB, *arr and manifest versions
are snapshots tagged `bunkarr-kind:plexdb|arr|manifest` (restic) or folders under
`.bunkarr/plex`, `.bunkarr/arr` and `.bunkarr/manifests` (rclone). After losing `/config`,
**attach** the destination to a new Bunkarr (create it with *Attach* and the kit's secret: for
restic its repository password; for rclone crypt, under *Use my own / an existing crypt
password*, its "rclone crypt password" and, when the kit lists one, its "rclone crypt password2";
leave password2 empty when the kit says none): the first sync reads your sources again and
uploads only what is missing. The old snapshots stay in the repository under
their old tag.

**Test the kit once**, on another machine: restore one file as above. That is the only proof that
the kit you keep is complete.

### Verifying

Every off-site destination gets a verify job (weekly by default; the card's **Verify** runs one
now). Nothing counts as verified from a listing alone: the job reads data back from the provider.

- **restic**: `restic check` reads a rotating share of the repository's packs (the verify sample,
  5 % by default, reads every pack once in 20 runs), then restores a sample of files into
  `<config>/staging` and compares each with its source's SHA-256.
- **rclone**: every object is listed and compared with Bunkarr's records (a missing one, or one of
  another size, is uploaded again by the next sync), then a sample is downloaded with
  `rclone check --download` and compared with the sources. One version of each Plex DB, *arr and
  manifest kind is downloaded and hashed too.
- The content sample is capped by `settings.verify.sampleMaxBytes` (4 GiB on restic, 16 GiB on
  rclone). To read everything once, `POST /api/v1/destinations/<id>/verify` with
  `{"readData": true}` (all of it is downloaded: mind the egress).
- A failed check or a mismatch fails the job's item and sends a notification. On restic, see
  [When restic reports damage](#when-restic-reports-damage); a new sync does not repair it.

### Costs

- **B2 and S3 charge for requests**, not only storage: uploads and deletes are cheap (B2 class A
  is free), listings and downloads are class B and C transactions (B2 includes 2,500 of each per
  day). restic packs data into 64 MiB files on remote destinations
  (`settings.restic.packSizeMiB`), which keeps the object count, and so the transactions, low;
  rclone stores one object per file (plus the `.bunkarr/` files).
- **Egress**: a verify job downloads its sample (a share of the files per run, capped by
  `settings.verify.sampleMaxBytes`), and `restic prune` downloads and rewrites packs that are
  partly unused (at most weekly by default, `settings.restic.pruneEveryDays`). B2's egress is free
  up to three times the data you store per month; other providers charge for it. Check your
  provider's current prices.
- **The restic cache** lives in `<config>/cache/restic/<destination id>`. It holds encrypted
  metadata only (indexes, snapshots, trees) and can reach several GiB for large repositories; it
  saves most of the listing and download transactions. It is safe to delete (restic rebuilds it).

### Bandwidth and transfer windows

Per destination (the **Bandwidth** tab): an upload and download limit (KiB/s, 0 = unlimited), up
to 16 timetable entries that change the limit by day and time (rclone switches the rate during a
transfer, and restic's traffic to remote destinations goes through rclone too), and a **transfer
window** (days, from–to, in the container's time zone) outside which the destination's syncs,
verifies and retention jobs do not run. A job that reaches the window's end stops cleanly (no
file fails and nothing partial is recorded), waits, and resumes in the next window with the same
plan ("Waiting for the transfer window until 01:00"). The Plex DB, *arr and manifest backups ignore
the window (they are small; the limits apply). At most two off-site syncs upload at a time
(`engines.uploadSlots`, `PUT /api/v1/settings/engines`, applied after a restart). They run on
these upload slots, not on the two job workers of every other job, so an off-site seed that
uploads for days does not hold back the nightly sync to the NAS or the Plex and *arr backups.

**A file larger than the window** (it cannot be uploaded within one whole window at the rate in
force, for example a 60 GB remux at 2 MiB/s in a 6-hour window) fails its item with a warning each
run, and the rest of the job completes. Raise the limit, widen the window, or turn on *Let a file
larger than the window run past its end*.

### B2, S3 and SFTP notes

- **Backblaze B2**: create an **application key restricted to the bucket**, never the master key,
  with the capabilities `listBuckets`, `listFiles`, `readFiles`, `writeFiles` and `deleteFiles`.
  Bunkarr warns when a key is not restricted to one bucket: whoever obtains `/config` could then
  erase every bucket of the account. Bunkarr's deletes (expired retention, `restic prune`) are
  **hard deletes**, so B2 keeps no hidden versions to pay for, and a deleted file cannot be
  undeleted. For a copy that survives a compromise of `/config`, turn on **Object Lock** for the
  bucket with a retention longer than your retention settings (deletes of locked files then fail
  until the lock expires). Set the bucket's **lifecycle** to *Keep only the last version of the
  file* (B2's default keeps all versions): it removes, after a day, the previous versions of the
  few files Bunkarr rewrites in place (such as `.bunkarr/links.tsv` on rclone). Never add a rule
  that hides or deletes current files after some days (`daysFromUploadingToHiding`): it would
  remove backed-up data behind Bunkarr's back, and restic would report a damaged repository.
- **S3**: an endpoint with a self-signed certificate needs its CA certificate in the destination's
  *CA certificate* field; TLS verification is never turned off. Plain `http://` endpoints are
  accepted only on a private network, with a warning. Two destinations may not share a bucket
  prefix (one inside the other).
- **SFTP**: "Fetch host keys" shows the server's key fingerprints; compare them with the server's
  own (`ssh-keygen -lf /etc/ssh/ssh_host_*_key.pub` on the server) before you confirm. A changed
  host key stops every job until you pin the new one. The folder must exist on the server. Use an
  SSH key (a password must be at least 8 characters). File names get about 1.6 times longer
  under crypt, so very long names can exceed the server's 255-byte limit (that file then fails).

### One host name per install

restic decides whether a repository lock is stale by the host name that wrote it. Give every
Bunkarr install its **own, stable host name** (`hostname: bunkarr-<server name>` in the compose
file) and **never run two containers with the same host name against the same repository**: a
second install, a test or dev container, or the old and new container during an Unraid
update/replace. Bunkarr refuses to remove a lock of its own host name that a process it does not
know holds ("another restic process with host name … is using this repository"), so such a clash
stops the retention job instead of damaging data; fix the host names and run it again. On Unraid,
set the host name in the container template (Advanced View → Extra Parameters:
`--hostname=bunkarr-tower`) and keep it when you edit the container.

### When restic reports damage

A verify job runs `restic check` (reading a rotating share of the packs; everything with
`readData`) and restores a sample of files to compare with the sources. If it reports errors, a sync
alone cannot fix them: restic deduplicates, so while the index lists the damaged data a backup
reuses it. Pause Bunkarr's jobs of that destination (Destinations → Edit → disable) and repair
from inside the container, where the sources are mounted at the paths Bunkarr backs up. Open the
shell **as Bunkarr's user**: `docker exec -it -u <PUID>:<PGID> bunkarr sh` (Unraid: `-u 99:100`).
Never use a plain `docker exec -it bunkarr sh`: it is a root shell, and restic run as root writes
index and pack files that Bunkarr cannot read, so every later job of the destination fails until
you give them back to `PUID`:`PGID` (root can also be refused reading sources on NFS). Then
`mkdir /tmp/repair && cd /tmp/repair`, paste the kit's shell block, and run:

```sh
export RESTIC_CACHE_DIR=/tmp/repair/cache  # not /config (the home of Bunkarr's user)
restic check                    # see what is damaged
restic repair index             # rebuild the index from the packs
restic repair packs <pack id>…  # salvage what can be read from damaged packs
restic repair snapshots --dry-run | tee damage.txt  # what is lost; changes nothing
sed -n 's/^ *file "\(.*\)": removed missing content$/\1/p' damage.txt | sort -u > heal.txt
restic backup --host bunkarr --tag repair --files-from-verbatim heal.txt  # re-upload those files
restic check                    # "no errors were found": repaired
```

In `damage.txt`, a `file "…": removed missing content` line is a file whose data is gone and a
`dir "…"` line is a lost folder listing. The backup reads again only the files in `heal.txt` (a
name printed with a `\` escape must be written out by hand; skip the backup if the list is empty)
and uploads their data, which makes the damaged snapshots whole under their own ids: Bunkarr keeps
every version it recorded. Never back up whole source paths here: that ignores Bunkarr's tiers and
excludes and uploads everything the repository does not hold. restic here also ignores the
destination's bandwidth limits and transfer window: on a remote destination add
`--limit-upload <KiB/s>` to the backup. Bunkarr never forgets a snapshot without its tags, so once
`check` is clean run `restic forget <id of the repair snapshot>` (the data stays: the old snapshots
use it).

If `check` still reports errors, some lost data cannot be uploaded again: a lost folder listing (a
backup never recreates it), or a file that is no longer at the sources as it was (deleted or changed
since, an older version, a Plex DB, *arr or manifest version). The backup names the files it could
not read ("at least one source file could not be read"); if one is still at its source (a permission
error), fix that and run the backup again first. Only then run `restic repair snapshots --forget`:
it replaces each damaged snapshot with a copy without the lost files, under a **new id** that
Bunkarr has not recorded. The next sync treats every replaced snapshot as removed outside Bunkarr:
the current files it held are uploaded again, but the older versions and deleted files Bunkarr kept
in it are dropped ("version lost"), **including those the repair kept intact**, and retention later
forgets the copies. So before you enable the destination, restore what you still need from the new
snapshots (the ids the command prints) as in
[Restoring without Bunkarr](#restoring-without-bunkarr).

Remove the working folder (`cd / && rm -rf /tmp/repair`: it holds the repository password), then
enable the destination and run a sync. Stale locks after a crash are removed by Bunkarr
before its next exclusive step (or with Destinations → Remove stale locks).

## Upgrading and downgrading

Update the checkout (`git pull`) and run the Quick start's `up -d --build` again, or pull a
released image (`docker compose -f deploy/docker-compose.yml pull`, then `up -d`). Running jobs
are queued to resume. On Unraid: **Docker** tab → **apply update**
([`unraid/README.md`](unraid/README.md#7-updating)).

**From Phase 1-3 (compose).** The compose example now requires `SERVER_NAME` (the container's
host name becomes `bunkarr-<SERVER_NAME>`; restic uses it to tell stale locks apart, so it must
be unique per install). Add `SERVER_NAME=<your server's name>` to `deploy/.env` before `up`, or
compose refuses with "required variable SERVER_NAME is missing a value".

**Pre-migration copy.** Before a new version changes the database schema, Bunkarr writes a copy
of the database to `/config/backups/bunkarr-v<old schema version>-<time>.db` (mode 0600; the new
copy and the newest two others are kept), and it refuses to upgrade when that copy fails. Phase
2's upgrade rebuilds tables, and an older Bunkarr refuses a newer database, so this copy is the
only way back. To downgrade:

1. Stop Bunkarr: `docker compose -f deploy/docker-compose.yml down`.
2. In `/config`, move `bunkarr.db` and its `bunkarr.db-wal` and `bunkarr.db-shm` files (if
   present) aside.
3. Copy the pre-migration copy to `/config/bunkarr.db`, owned by `PUID`:`PGID`. Keep
   `bunkarr.key`: it is the same key.
4. Start the previous version: set `image:` to its tag, or check out its git tag and build.

Whatever Bunkarr recorded after the upgrade (jobs, backup versions, settings) is not in the copy.

### Verifying a release

Every [release](https://github.com/sl0wz3r/bunkarr/releases) names its image digest: pin a version
**and its digest** (`ghcr.io/sl0wz3r/bunkarr:X.Y.Z@sha256:<digest>`) to pull exactly the image the
test suites ran against, and check where it was built with
`gh attestation verify oci://ghcr.io/sl0wz3r/bunkarr:X.Y.Z --owner sl0wz3r`. Each release also
has SBOMs (SPDX and CycloneDX) of the source and of each image platform, and `checksums.txt`:
[verifying a release](SECURITY.md#verifying-a-release). GitHub releases, with their
attestations and SBOMs, start with the release after 0.1.0-beta.1; for that one,
`docker buildx imagetools inspect ghcr.io/sl0wz3r/bunkarr:0.1.0-beta.1` prints the digest.

## The mass-change guard

An unmounted share, a bad script or ransomware looks to a backup like "delete or rewrite
everything". Two guards stop that from reaching the backup:

- **Scans refuse** to change the catalog when a source root is missing, not a directory, on another
  filesystem than before, empty while files are catalogued, or returns I/O errors. The sync
  fails; nothing at the destination changes.
- **Large changes are held.** Before copying, each sync counts per source the files it would move
  into retention (deleted at the source) or replace (changed). Above 10 % of the source's files
  *and* more than 20 files, or above 1000 files (both limits per destination: *Max changes*,
  *Max changed files*), those items are **held**: nothing is moved or replaced, new files still
  sync, the job ends *completed with warnings* and a warning notification is sent. A change that
  would leave a file empty or less than half its backed-up size is always held.
- **Tiers count too.** Released files (see [Tiers](#tiers)) count as changes, like deletions, and
  so do new copies that are full only because a tier fact is unknown (for example Radarr's cache
  is stale). Above the limits those copies are held ("tier unknown because … is not fresh").

To apply held changes that you expected, open the job (Activity) and click **Apply held changes**.
It starts a sync with `allowChanges`, which runs them; the API equivalent is
`POST /api/v1/destinations/{id}/sync` with `{"allowChanges": true}`. Deleted and replaced files
still go to retention first. Preview shows what would be held.

## Restoring

- **Files** are plain copies under `<target>/<destination folder>/…`. Deleted files and older
  versions stay under `<target>/.bunkarr/retention/<time>-job<id>/…` until they expire. Copy them
  back with any tool (a restore wizard is Phase 5).
- **Hardlinked names.** Of a hardlink group (an *arr download and its library copy), the
  destination holds one file. Where the share supports hardlinks (and the destination's
  *Hardlinks* setting is "recreate") the other names are hardlinks of it; otherwise (SMB without
  Unix extensions, or "copy") they are only recorded and **have no file at the destination**.
  After every sync Bunkarr writes `<target>/.bunkarr/links.tsv`, one line per hardlinked name:

  ```
  <name> TAB <primary> TAB <state>
  ```

  Both paths are relative to `<target>`; `state` is `linked` (already a hardlink) or
  `link_recorded` (no file: recreate it). Lines starting with `#` are comments. In paths, `\\`,
  `\t`, `\n` and `\r` stand for a backslash, tab, newline and carriage return, and a leading `\#`
  for `#`. After copying the files back, recreate the recorded names from the restored copy
  (`$RESTORE` is where you restored `<target>`'s tree to; use `cp` instead of `ln` across
  filesystems):

  ```sh
  cd "$RESTORE"
  grep -v '^#' /path/to/target/.bunkarr/links.tsv | grep -v '\\' |
    while IFS="$(printf '\t')" read -r name primary state; do
      [ -e "$name" ] || { mkdir -p "$(dirname "$name")" && ln "$primary" "$name"; }
    done
  ```

  The loop skips lines with an escaped character (`grep -v '\\'`); recreate those by hand. Apart
  from Bunkarr's own database (which Bunkarr does not back up), the manifest is the only record
  of these names.
- **Plex database** versions are under `<target>/.bunkarr/plex/<server>-<id>/<time>/` with a
  `manifest.json` (sha256 of each file, integrity result). To restore:
  1. Stop Plex.
  2. Move aside `com.plexapp.plugins.library.db` and `com.plexapp.plugins.library.blobs.db` in
     `Plug-in Support/Databases/`, and delete their `-wal` and `-shm` files (a stale WAL would be
     replayed onto the restored database).
  3. Copy in the backup's database files, owned by Plex's user, mode 0644.
  4. Restore `Preferences.xml` only to the same server: it holds the server's identity and token.
  5. Start Plex.
- **`Preferences.xml` holds your Plex token in cleartext.** Every Plex DB version includes it,
  and it contains `PlexOnlineToken`, the server's authentication token for your Plex account.
  Bunkarr writes it with mode 0600, but an SMB share mounted without POSIX extensions takes file
  modes from its mount options (Unraid's Unassigned Devices uses `file_mode=0777`), so anyone who
  can read the share may be able to read the token. Restrict who can read the backup share (or
  its `.bunkarr/plex` folder) on the NAS, or mount it over NFS or SMB with Unix extensions. An
  option to skip or encrypt this file is planned ([DEFERRED.md](DEFERRED.md)).
- **Sonarr, Radarr and Lidarr configuration**: each version's zip under
  `<target>/.bunkarr/arr/…`; restore it in the app (see [*arr config backups](#arr-config-backups)).
  The same caution applies: the zips hold the app's API key and passwords.
- **The *arr library itself** (to acquire media again): the newest version under
  `<target>/.bunkarr/manifests/` (see [Manifests](#manifests)).
- **Off-site destinations** (restic, rclone) are restored with restic or rclone and the
  destination's recovery kit; see [Restoring without Bunkarr](#restoring-without-bunkarr).

## API

REST under `/api/v1`, JSON, *arr-style authentication: the API key from **Settings → General** in
the `X-Api-Key` header or the `apikey` query parameter.

```sh
curl -H "X-Api-Key: $KEY" http://localhost:8787/api/v1/system/status
curl -H "X-Api-Key: $KEY" -H 'Content-Type: application/json' -d '{"dryRun":true}' \
  http://localhost:8787/api/v1/destinations/1/sync   # preview a sync (202 + the job)
```

- `GET /api/v1/health` — unauthenticated liveness check.
- `GET /api/v1/openapi.json` — the OpenAPI 3.1 description of every endpoint (a test keeps it in
  step with the router). The Phase 1 contract is [`docs/design/phase1.md`](docs/design/phase1.md) §7;
  Phase 2's and Phase 3's additions are in [`docs/design/phase2-3.md`](docs/design/phase2-3.md)
  §13.
- `/api/v1/tiers/…` — the tier rules (`GET`/`PUT /tiers/rules`, with the revision a save is
  based on), `GET /tiers/fields`, `GET /tiers/presets`, `POST /tiers/preview` (and
  `GET /tiers/preview/{id}/items`) and the irreplaceable flags (`/tiers/flags`). A release is a
  sync with `releaseDemoted` (see the OpenAPI description).
- `/api/v1/destinations/…` for off-site destinations (Phase 4, [`docs/design/phase4.md`](docs/design/phase4.md)
  §12): `kind`, `engine`, `remote`, write-only `credentials`, `encryption` and `bandwidth` on
  create; `POST /destinations/test`, `POST /destinations/sftp/hostkeys`,
  `POST /destinations/{id}/recovery-kit` (and `/confirm`), `/unlock` and `/retention`. The routes
  that choose where data goes need a UI session and `currentPassword` (403 with the API key).
- `POST /api/v1/webhook/{app}/{integrationId}` — the *arr webhooks. They take only that
  connection's webhook key (Basic auth password, `X-Api-Key` or `apikey`), never the API key
  above; see [Webhook](#webhook).

## Roadmap

| Phase | |
|---|---|
| 0 | Foundation: server, auth, settings, UI shell, image, CI ✅ |
| 1 | MVP: Plex integration, scanner with hardlink detection, file-copy engine, scheduled syncs, Plex DB backup, activity/history, Apprise notifications ✅ |
| 2 | *arr awareness: Sonarr/Radarr/Lidarr APIs and webhooks, *arr config backups, manifest export, Sign in with Plex ✅ |
| 3 | Tiering: rule engine (tags, quality, Plex libraries, Tautulli, Seerr, Maintainerr), presets, preview, release of demoted files, irreplaceable flags ✅ |
| 4 | Destinations & versioning: restic and rclone engines, B2/S3/SFTP, encryption and recovery kits, bandwidth limits and transfer windows ✅ |
| 5 | Restore & disaster recovery: restore wizard, manifest re-acquisition, restore tests |
| 6 | Release polish: Unraid CA listing (the template is in [`unraid/`](unraid/README.md)), metrics, notifications, docs site, hardening |

What comes after the phases, and the postponed items that matter most to users:
[`ROADMAP.md`](ROADMAP.md). Decisions are recorded in [`docs/adr`](docs/adr); postponed items with
reasons in [`DEFERRED.md`](DEFERRED.md).

## Help and security

Questions and setup help: [Discussions → Q&A](https://github.com/sl0wz3r/bunkarr/discussions/categories/q-a).
Bugs and feature requests: the [issue forms](https://github.com/sl0wz3r/bunkarr/issues/new/choose).
[SUPPORT.md](SUPPORT.md) says what to include and how to share logs safely. Report a vulnerability
privately, never in a public issue: [SECURITY.md](SECURITY.md).

## Development

Requirements: Go 1.27, Node 24, make; Docker for the image tests.

```sh
make web build        # UI into web/dist, then bin/bunkarr embedding it
make run              # serve on :8787 with ./config
make test lint        # Go (race) + web tests; gofmt, vet, typecheck, shellcheck
make test-e2e         # acceptance suite against the real binary (syncs, hardlinks, kill -9 + resume, guards)
make docker-test      # build the image and smoke-test it
make test-docker      # image smoke, container kill, Plex backup/restore and SMB/NFS share tests (Docker and Go)
make test-plex        # the Plex backup/restore test only (slow; pulls plexinc/pms-docker once)
make test-shares      # syncs and kill + resume on Samba (CIFS) and NFS shares (privileged containers)
make test-arr         # real Sonarr, Radarr and Lidarr: imports, upgrades, webhooks, backups, manifests, tiers (needs internet)
make test-engines     # real restic and rclone against MinIO and an SFTP server, in containers (no Bunkarr image)
make test-offsite     # off-site acceptance: restic/rclone destinations of the image on MinIO + SFTP, kits, windows, secrets audit
cd web && npm run dev # UI dev server on :5173, proxying /api to :8787
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[GPL-3.0-or-later](LICENSE), like the *arr ecosystem.
