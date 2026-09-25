# Deferred

Work that was consciously postponed, with the reason. Items move out of here when they are built.

| Item | From | Reason / plan |
|---|---|---|
| Trusted reverse proxies (`X-Forwarded-For`) | Phase 0 | Proxy headers are ignored so nobody can spoof a local address. Behind a proxy all clients share the proxy's address: local-address bypass then applies to everyone (or no one) and the login limiter counts them together. Add a trusted-proxy CIDR setting before the Unraid CA release (Phase 6). |
| URL base (serving under `/bunkarr`) | Phase 0 | Not in the spec; common in *arr setups behind a proxy. Add with the trusted-proxy setting. |
| Built-in TLS | Phase 0 | Use a reverse proxy; the session cookie is marked `Secure` when `X-Forwarded-Proto: https`. |
| First-run setup token | Phase 0 | Like the *arrs, whoever reaches a fresh instance first can create the login. Mitigation today: bring the container up on a trusted network and complete setup immediately (the log warns while no user exists). A one-time setup token printed to the log is an option if this is judged too weak. |
| Playwright smoke test (login → add destination → sync) | Phase 0 | The flow needs destinations and syncs (Phase 1). Component tests cover login and setup now. |
| Webhook endpoints `/api/v1/webhook/{sonarr,radarr,lidarr}` | Phase 0 | Phase 2 deliverable. |
| `/metrics` (Prometheus) | Phase 0 | Phase 6 deliverable. |
| Image signing (cosign), SBOM publication, dependency scanning beyond govulncheck/npm audit | Phase 0 | Phase 6 deliverable. The release workflow already attaches BuildKit SBOM and provenance attestations. |
| Sanitizing export: gitleaks secret scan | Phase 0 | The private→public sync scans for private terms (LAN addresses, account names, home paths) but not for high-entropy secrets yet. |
