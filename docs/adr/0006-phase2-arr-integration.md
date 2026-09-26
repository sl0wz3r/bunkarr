# 0006. *arr integration: webhook keys, *arr backups, webhook deletes and Plex sign-in

- Status: accepted
- Date: 2026-09-25
- Design: [`docs/design/phase2-3.md`](../design/phase2-3.md) revision 3 (D3, D4, D7, D10, D14;
  S8, S12, S13, S16, S17, S19)

## Context

Phase 2 connects Bunkarr to Sonarr, Radarr and Lidarr (webhooks, a metadata index, backups of
their configuration, manifests) and adds "Sign in with Plex". Four of its decisions trade
convenience against the safety of the backups or of the user's credentials, and each departs
from the literal spec or from the sibling project Dupearr. They are recorded here. The *arr
fixture spike (real Sonarr 4.0.20, Radarr 6.4.4 and Lidarr 3.1.0) supplied the facts they rest on.

## Decision 1: webhooks authenticate with a per-integration webhook key (D7)

The spec asks the webhooks to carry "the API key". The *arrs return a webhook's URL and headers
unmasked through their own API, and keep them in their backup zips, which Bunkarr itself copies
to the backup share. Bunkarr's master API key there would be full Bunkarr admin for anyone who
holds any *arr API key or can read the share.

- Each Sonarr, Radarr and Lidarr integration gets its own **webhook key**: 16 random bytes,
  hex-encoded, generated when the integration is created. It is sealed at rest like every other
  secret (ADR 0003, AAD `integration:<id>:webhookKey`) and held in the log redaction registry.
- The only response that contains it is `POST /api/v1/integrations/{id}/webhook/key`
  (`{"rotate": false}` reveals it, `true` replaces it). The UI shows it on request.
- It is accepted **only** by `POST /api/v1/webhook/{app}/{id}` (one URL per integration) and the
  spec's `POST /api/v1/webhook/{app}` (which picks the integration whose key matches). Those
  routes accept nothing else: not the master API key, not a session, not "disabled for local
  addresses".
- It is sent as the password of HTTP Basic auth (recommended: the *arr keeps that field private),
  the `X-Api-Key` header, or `?apikey=`. It is compared in constant time against an in-memory map,
  before anything else; the failed-authentication limiter (10 a minute per address) counts and
  blocks only requests without a valid key, so a stale key in one *arr cannot block the others
  behind the same proxy or Docker bridge.
- A payload is a hint, never truth (S12): it only selects which *arr items to refresh. The
  refresh reads each item from the *arr's API, and only folders inside sources are scanned.

**Rejected:** the master API key (above); one shared webhook key (no per-app revocation, and the
generic route could not tell the integrations apart).

**Consequences:** each *arr gets its own key to paste. After "Regenerate key" the *arr's events
are refused until the new key is pasted; the next full refresh catches up. `?apikey=` works but
ends up in proxy logs. A leaked webhook key can cause nothing but refreshes of that integration's
items and scans of folders inside sources.

## Decision 2: *arr backups come from the Backups folder, else over HTTP (D3, D4, S17)

With Forms login on, the API key cannot download a backup zip: the *arr answers with a redirect
to `/login` (spike). Getting the zips over HTTP would need either the *arr's UI password or its
"Disabled for Local Addresses" mode.

- An *arr backup is read from the **Backups folder** when one is set: the app's `Backups`
  directory mounted into Bunkarr read-only, read through an `os.Root` with `O_NOFOLLOW`, regular
  files only. Otherwise it is downloaded **over HTTP** with the API key, which works only when the
  *arr does not require a login for Bunkarr's address. The *arr's UI password is never stored.
  Bunkarr checks the access (reads the folder, or a 1-byte range request over HTTP) before it asks
  the *arr to make a backup, so a misconfigured job does not leave backups behind in the *arr.
- A **fresh scheduled backup** of the *arr (younger than `maxScheduledAgeDays`, default 7) is
  copied as it is. Only when there is none does Bunkarr send the `Backup` command, the only write
  it makes to an *arr. The *arrs never prune manual backups made through the API, so the default
  schedule is weekly and the Test result shows how many manual backups have piled up.
- The zip is verified before it is stored (entry count and expansion limits, every entry's CRC,
  `quick_check` of the database extracted in a 0700 staging directory), then stored exactly as
  received: files 0600, directories 0700, versioned like the Plex DB (14 daily + 8 weekly by
  default). It is **not sealed** (D4): a copy that only Bunkarr's key can open would be lost
  together with Bunkarr's config, exactly when it is needed.
- A zip holds the *arr's API key, its password hash, indexer and download-client credentials, its
  Plex token and its connections, including Bunkarr's webhook key. It is never served by the API,
  never logged, and described only by names, sizes and hashes. A destination whose probe does not
  find that it keeps mode 0600 (SMB without POSIX extensions) needs the explicit
  `acceptInsecureModes` setting. The staged copy under `/config/staging` is removed when the job
  ends, by a hook when crash recovery fails the job, and at start-up when a day old.
- A job interrupted while the *arr makes its backup records the command's id first and follows it
  when it resumes, so a restart does not make the *arr create a second manual backup; a command
  the *arr reports orphaned (it restarted too) is replaced by a new one.

**Rejected:** storing the *arr's UI credentials (a new secret; left to the user, D13); requiring
"Disabled for Local Addresses" (weakens the *arr); sealing with Bunkarr's key (above); deleting
the manual backups Bunkarr made in the *arr (a new write to another application, D13).

**Consequences:** one more read-only mount per *arr. The folder must exist before the container
starts: a fresh *arr creates it with its first backup. The backup share must be protected like
the *arrs themselves.

## Decision 3: deletes reported by webhooks are retained by the next full sync (D14, S10)

A webhook sync covers one item's folder. The mass-change guard (S10) counts per sync, so a
library wiped item by item (hundreds of delete events, each its own small sync) would never reach
its thresholds, and the deleted files would move into retention and expire unnoticed.

- A **targeted sync retains a vanished name only when the same directory gets new content in the
  same plan**: an upgrade (the copy runs before the retain, S6) or a rename.
- Every other vanished name inside a targeted scope stays live and recorded at the destination
  until the next untargeted sync, whose guard sees the whole change at once. The targeted scan
  still marks its catalog rows deleted.
- An upgrade whose delete event arrives minutes before its Download event (the *arr copies the new
  file from another volume) is held until the Download or 30 minutes, so the targeted sync sees
  both.

**Rejected:** retaining at once (the guard would never see a slow mass deletion); a cumulative
24-hour guard over webhook syncs (retains sooner, but more state and more ways to be wrong; open
question 6 of the design).

**Consequences:** a file deleted in an *arr stays in the live backup until the next scheduled full
sync, then sits in retention for `deletedDays`. Nothing is lost by the delay. The webhook panel
warns when a linked destination has no sync schedule.

## Decision 4: Sign in with Plex keeps every token on the server (D10, S19)

The user asked for Dupearr's "Sign in with Plex" (a plex.tv PIN, then a server picker). Dupearr
sends the account token and every server's token to the browser, which conflicts with S8: a
stored secret is write-only and only ever sent to its own URL.

- The server runs the plex.tv PIN flow. The browser holds only an opaque sign-in id (32 hex
  characters) and a server id. The account token and the server tokens stay in an in-memory
  registry on the server: at most 8 live sign-ins, 10 minutes while pending, 20 minutes after the
  approval, registered for log redaction, gone at a restart.
- The saved token is the chosen server's own access token. An owned server without one can use
  the account token only through an explicit checkbox; a shared server without one needs a token
  entered by hand.
- Before any token is sent to a URL, Bunkarr calls `/identity` there without it and requires the
  chosen server's `machineIdentifier`. Connections are probed through the outbound guard
  (`internal/netguard`, S16), which refuses link-local and cloud metadata addresses at dial time.
  The probe sends the token only over https (or http to a loopback address); an `http`
  connection is shown reachable with the token withheld, is never recommended ("Unencrypted"),
  and needs "Use anyway".
- Each install identifies itself to plex.tv with its own client identifier (a UUID stored as
  `plex.clientIdentifier`), so one install can be revoked on its own under plex.tv → Authorized
  Devices.
- COOP changes from `same-origin` to `same-origin-allow-popups`, so Bunkarr can close the plex.tv
  popup. The popup navigates only to `https://app.plex.tv/auth`.
- The plex.tv URLs can be overridden only in binaries built with `-tags e2e`.
- The manual URL + token path is unchanged.

**Rejected:** porting Dupearr's handlers as they are (tokens in the browser); keeping COOP
`same-origin` (the fallback if the relaxed header is objected to: the popup is then not closed
automatically).

**Consequences:** Bunkarr contacts plex.tv and clients.plex.tv, over HTTPS, only when the user
clicks "Sign in with Plex". A sign-in does not survive a restart of Bunkarr. The Plex client, the
connection probe, the *arr clients and Apprise now all dial through the outbound guard.
