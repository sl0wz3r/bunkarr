# Roadmap

Where Bunkarr is heading after the beta. **Nothing on this page is a promise or a date.** Bunkarr
is a small project that holds people's backups, so every item has to meet the same bar as the code
that copies files today: it never writes to source media, it fails safe when a share is unmounted
or data is stale, it has tests for the unhappy paths and a preview of anything that deletes, and
it is documented (see [CONTRIBUTING.md](CONTRIBUTING.md)).

The phases are the ones in the [README](README.md#roadmap); postponed details, with the reasons,
are in [DEFERRED.md](DEFERRED.md). During the beta, the order follows what users run into. The
best way to move an item up is to add your setup and use case to its issue (label `roadmap`),
or to open a thread in
[Discussions → Ideas](https://github.com/sl0wz3r/bunkarr/discussions/categories/ideas). Please
discuss anything large there before you open a pull request.

**Labels used on roadmap issues**

| Label | Meaning |
|---|---|
| `roadmap` | Tracks an item on this page. |
| `good first issue` | Small and well-scoped: a good first contribution. |
| `help wanted` | A contributor would be welcome. Comment on the issue first so work is not duplicated. |
| `safety` | Could affect source media, backup integrity or restores. Needs regression tests for the unhappy paths and an extra review. |

## At a glance

| Item | Stage | How you can help |
|---|---|---|
| [Restore and disaster recovery (Phase 5)](#restore-and-disaster-recovery-phase-5) | Next | `safety`: test restores from the recovery kit and report what was hard |
| [Unraid Community Applications listing](#unraid-community-applications-listing) | Next | Installs on real Unraid servers, with a NAS share through Unassigned Devices |
| [Reverse proxies: trusted proxies, URL base, external URL](#reverse-proxies-trusted-proxies-url-base-external-url) | Planned | Your proxy setup (SWAG, Nginx Proxy Manager, Traefik, Caddy) |
| [First-run setup token](#first-run-setup-token) | Planned | `safety`, design discussion |
| [Least-privilege container](#least-privilege-container) | Planned | `help wanted`: run the Docker suites under the reduced capability set |
| [Plex token in the Plex database backup](#plex-token-in-the-plex-database-backup) | Planned | `safety` |
| [Metrics, notifications and a docs site (Phase 6)](#metrics-notifications-and-a-docs-site-phase-6) | Planned | `help wanted`: which metrics and notification targets you need |

*Next*: being designed or built now. *Planned*: the design leaves room for it, and the reason it
waits is in [DEFERRED.md](DEFERRED.md). *Exploring*: a direction that still needs a design.

---

## Restore and disaster recovery (Phase 5)

- **Today:** restores are manual and need no Bunkarr: mounted destinations hold plain files, with
  deleted and replaced versions in retention and hardlinked names in `links.tsv`; Plex database
  versions carry a `manifest.json`; the \*arr configuration zips are restored in the app; off-site
  destinations are restored with restic or rclone and the destination's recovery kit
  (README, [Restoring](README.md#restoring) and
  [Restoring without Bunkarr](README.md#restoring-without-bunkarr)). The manifests list every
  Sonarr, Radarr and Lidarr item, so a library can be acquired again, by hand.
- **Goal:** a restore wizard for every kind of backup (files and their older versions, Plex
  database versions, \*arr configurations, off-site snapshots); manifest re-acquisition, which
  adds a manifest's items back to a fresh Sonarr, Radarr or Lidarr so that it downloads them
  again; and restore tests. Also an attached destination that reads its records back from the
  newest snapshot instead of reading every source again.
- **Care:** a restore is the one step that may write where media lives. Like every other job it
  needs a preview first, and tests that a restore of every destination kind gives back exactly
  what was backed up. This is a `safety` item.

## Unraid Community Applications listing

- **Today:** the template (`unraid/bunkarr.xml`) is installed by hand
  ([`unraid/README.md`](unraid/README.md)), and support runs on GitHub.
- **Goal:** the listing in Community Applications, then a `[Support]` thread in the Unraid forum
  linked from the template.
- **Before the listing:** the checks recorded in [DEFERRED.md](DEFERRED.md) ("Unraid behaviors
  the install guide assumes", the real-share and real-B2 passes) and the decisions on the next
  three items.

## Reverse proxies: trusted proxies, URL base, external URL

- **Today:** `X-Forwarded-For` is ignored so that nobody can pose as a local address. Behind a
  proxy every client shares the proxy's address, so keep *Authentication required: Enabled*
  (`unraid/README.md`, [Reverse proxy](unraid/README.md#reverse-proxy)). Bunkarr must be served
  at the root of a host name of its own.
- **Goal:** a trusted-proxy setting (CIDRs) and an explicit local-networks setting, a URL base
  (`/bunkarr`), and an external URL that also turns on the link to the job in notifications.
- **Care:** a proxy header must never let a client anywhere look local.

## First-run setup token

- **Today:** like the \*arr apps, whoever reaches a fresh instance first creates the login. The
  setup is refused unless Bunkarr is opened by one of its own host names (which closes DNS
  rebinding), and the log warns while no user exists: create the login right after the install.
- **Goal:** a one-time setup token printed to the container log, so that a device on the LAN
  cannot race the admin (after an install, or after `reset-auth`).

## Least-privilege container

- **Today:** the compose example and the Unraid template drop no capabilities.
- **Goal:** `--security-opt=no-new-privileges:true --cap-drop=ALL` plus only what the entrypoint
  needs (`CHOWN`, `DAC_OVERRIDE`, `KILL`, `SETGID`, `SETUID`), in the compose example and the
  template's Extra Parameters, once the image, kill, Plex, share and off-site test suites pass
  under those flags. Installed containers keep their saved Extra Parameters, so existing users
  would add it by hand (announced in the template's change log).

## Plex token in the Plex database backup

- **Today:** every Plex database version includes `Preferences.xml`, which holds the server's
  Plex token in cleartext; on an SMB share without POSIX extensions, anyone who can read the share
  may read it (README, [Restoring](README.md#restoring)).
- **Goal:** a per-server option to skip `Preferences.xml`, or to seal it so that only Bunkarr
  with its `bunkarr.key` can restore it.

## Metrics, notifications and a docs site (Phase 6)

- **Metrics:** a Prometheus `/metrics` endpoint.
- **Notifications:** Apprise is the only kind today; other kinds, and coalescing many failures
  into one message.
- **Docs site:** a documentation site next to the README and the install guide.
- **Hardening:** the items above, and the smaller ones in [DEFERRED.md](DEFERRED.md).

---

## Not planned

Bunkarr will not do these things. We will decline proposals that need one of them:

- write to, move or delete anything under a source (sources are mounted read-only);
- delete a backed-up file or version except through retention you configured or a release you
  confirmed after its preview;
- let a fact it cannot know (an unreachable \*arr, Tautulli or Seerr, a stale cache) make a file
  less protected;
- store secrets unencrypted in its database, or pass them to restic or rclone on the command line.

Not planned for now, with the reasons in [DEFERRED.md](DEFERRED.md) and the ADRs: native binaries
(the image, with its pinned restic and rclone, is the only supported install), and a
registry-free install on Unraid from a `docker save` archive (pre-release tags serve for testing
instead). Tell us in Discussions → Ideas if you need one of them.
