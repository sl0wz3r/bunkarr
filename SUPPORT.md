# Getting help with Bunkarr

Bunkarr is a small project in beta. Please ask in the right place: the answers are public, so they
help the next person too.

| You want to… | Go to |
|---|---|
| Ask how to set something up, or whether a result is expected | [Discussions → Q&A](https://github.com/sl0wz3r/bunkarr/discussions/categories/q-a) |
| Suggest an idea or talk about the [roadmap](ROADMAP.md) | [Discussions → Ideas](https://github.com/sl0wz3r/bunkarr/discussions/categories/ideas) |
| Report a bug: something is wrong or broken | [New issue → Bug report](https://github.com/sl0wz3r/bunkarr/issues/new/choose) |
| Request a specific, well-defined feature | [New issue → Feature request](https://github.com/sl0wz3r/bunkarr/issues/new/choose) |
| Report a security vulnerability | **Privately**, as described in [SECURITY.md](SECURITY.md). Never in a public issue or discussion. |
| Install or run it on Unraid | The [Unraid install guide](unraid/README.md) first, then Discussions → Q&A. An Unraid forum support thread will be added once Bunkarr is listed in Community Applications. |

## If a backup is missing, incomplete or cannot be restored

1. Leave the destination as it is. Pause the jobs of the affected destination (*Destinations →
   Edit → disable*), so no sync, verify or retention job changes it while you look.
2. Open *Activity*: the job's items and log say what was copied, retained, skipped or failed.
   Deleted and replaced files stay in the destination's retention until they expire (README,
   [Restoring](README.md#restoring)); an off-site destination is restored with restic or rclone
   and its recovery kit ([Restoring without Bunkarr](README.md#restoring-without-bunkarr)).
3. Open a [bug report](https://github.com/sl0wz3r/bunkarr/issues/new/choose) and answer **Yes**
   to "Is a backup missing, incomplete or not restorable, or did Bunkarr change a source file?".
   These reports get the `safety` label and are handled first.

## Before you ask

- Look at *System → Status* and the destination's **Test**: they catch most configuration
  problems, such as an unmounted share, a missing marker or a wrong path.
- Read the [README](README.md) (setup, destinations, Sonarr, Radarr and Lidarr, tiers,
  restoring, upgrading) and, on Unraid, the [install guide](unraid/README.md). Unmounted shares
  and path mappings cause most problems.
- Search the existing [issues](https://github.com/sl0wz3r/bunkarr/issues?q=is%3Aissue) and
  [discussions](https://github.com/sl0wz3r/bunkarr/discussions), and
  [DEFERRED.md](DEFERRED.md), which lists known limits and postponed work with the reasons.

## What to include

- **Version:** *System → Status*, or `docker exec bunkarr /entrypoint.sh version`. Include the
  image tag or digest if you pin one.
- **Install method:** Unraid (template from Community Applications or installed by hand, or
  Compose Manager), Docker Compose on another host, or `docker run`; on Unraid, its version and
  the container's Extra Parameters.
- **Mounts:** each mapping as host path → container path with its access mode, `PUID` and
  `PGID`, and how the backup share is mounted (NFS or SMB, Unassigned Devices).
- **Destinations involved:** a mounted folder, or restic or rclone to B2, S3 or SFTP.
- **Your other apps:** the versions of Plex, Sonarr, Radarr and Lidarr (and Tautulli, Seerr or
  Maintainerr if tiers are involved).
- **Logs:** see below.

### Sharing logs safely

1. Set the log level to `debug` (Unraid: *Edit → Advanced View → Log level*; Docker Compose:
   `BUNKARR_LOG_LEVEL=debug` in the environment) and reproduce the problem.
2. Get the log: the container log (`docker logs bunkarr`, or the container's *Logs* on Unraid),
   `/config/logs/bunkarr.log`, or the job's log (*Activity*, open the job). Only share the part
   around the problem, and set the level back to `info` afterwards.
3. **Check it before you post it.** Bunkarr redacts the secrets it knows. It does **not** hide
   file paths, host names, IP addresses, bucket names or SFTP user names, and paths show your
   folder layout and your titles. Replace anything you do not want to make public, but keep the
   structure: `/media/movies/…` versus `/data/media/movies/…` is often the answer.

**Never post** your Plex token, the Sonarr, Radarr and Lidarr API keys, Bunkarr's API key or a
webhook key (a webhook URL can carry one as `?apikey=`), B2, S3 or SFTP credentials, encryption
passwords or a recovery kit, Apprise URLs, or passwords. Never attach `bunkarr.db`, `bunkarr.key`,
a recovery kit, a Plex `Preferences.xml` or a Sonarr, Radarr or Lidarr backup zip: they all hold
secrets. If something leaks, replace it: Bunkarr's API key under *Settings → General*, a webhook
key with **Regenerate key** on the connection's card; a Plex token, an \*arr key or a storage
credential in the app or service that issued it, then update Bunkarr.
