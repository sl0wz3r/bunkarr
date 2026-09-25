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
  DEFERRED.md), otherwise any client could claim to be local.
- **CSRF**: the standard library's `CrossOriginProtection` on every `/api/v1` route rejects
  cross-site state-changing browser requests (Sec-Fetch-Site / Origin). This matters most for the
  local bypass, where the browser sends no credentials at all. Non-browser clients (scripts, the
  *arr webhooks) send neither header and are unaffected.
- **Brute force**: 5 failed logins per client address in 15 minutes → HTTP 429 with Retry-After.

## Consequences

- `?apikey=` puts the key in URLs; Bunkarr redacts it from its own logs, but reverse proxies may
  log it. The header is preferred and documented first.
- Behind a reverse proxy, the limiter and the local bypass see only the proxy's address until the
  trusted-proxy setting exists.
