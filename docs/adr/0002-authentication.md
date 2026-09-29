# 0002. Authentication

- Status: accepted
- Date: 2026-09-24

## Context

The spec requires auth on by default, a first-run wizard, forms login, an optional "disabled for
local addresses" mode, and an *arr-compatible API key (`X-Api-Key` header or `?apikey=`).

## Decision

- **One user**, like the *arrs, stored with a bcrypt hash. Setup is only possible while no user
  exists (checked inside the insert transaction, so concurrent setups create exactly one).
- **Server-side sessions** in SQLite: a random 256-bit token in an `HttpOnly`, `SameSite=Strict`
  cookie; only its SHA-256 is stored. 30-day lifetime. Changing the password ends every other
  session. `bunkarr reset-auth` removes the user to recover a lost password.
- **API key**: 128-bit random, stored sealed (ADR 0003), compared in constant time. A request
  that presents a wrong key is rejected even if it also carries a valid cookie, so a stale key in
  a script fails loudly.
- **Local bypass** uses the TCP peer address only. `X-Forwarded-For` is not trusted (see
  DEFERRED.md), otherwise any client could claim to be local. The container's gateways (its
  routing table's next hops, read at start-up) never count as local: Docker's userland proxy
  relays IPv6 clients to a container on an IPv4-only bridge, and the Docker host's own
  connections, from the bridge gateway, a private address, so a client anywhere would look
  local.
- **CSRF**: the standard library's `CrossOriginProtection` on every `/api/v1` route rejects
  cross-site state-changing browser requests (Sec-Fetch-Site / Origin). This matters most for the
  local bypass, where the browser sends no credentials at all. Non-browser clients (scripts, the
  *arr webhooks) send neither header and are unaffected.
- **DNS rebinding** defeats that check: a page on a name its owner points at this server's LAN
  address is same-origin to the browser and reads every answer. The host-only session cookie never
  reaches it, but the two things that need no credential would, so both also need a Host header
  no outside DNS answer can produce: an IP address, localhost, a single label (`tower`), a name
  under `.local`, `.home.arpa` or `.internal`, or one listed in `BUNKARR_ALLOWED_HOSTS`. Under any
  other name the local bypass does not apply (the login is required) and the first-run setup
  answers 403. Logins and the API key work under every name, so reverse proxies need no setting
  unless the setup or the bypass is used through them.
- **Brute force**: 5 failed logins per client in 15 minutes → HTTP 429 with Retry-After. A client
  is an IPv4 address or an IPv6 /64 (a device can take any address of its /64); when 10,000
  clients are tracked the least recently failing one is dropped. A guess counts as a failure from
  the moment its check starts (a right password then clears the count), so guesses sent together
  get 5 checks, not one per request in flight. The fresh-password checks (S29, the recovery kit)
  and a credentials change count against the same limiter.

## Consequences

- `?apikey=` puts the key in URLs; Bunkarr redacts it from its own logs, but reverse proxies may
  log it. The header is preferred and documented first.
- Behind a reverse proxy, the limiter and the local bypass see only the proxy's address until the
  trusted-proxy setting exists.
