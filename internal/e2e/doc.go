// Package e2e holds Bunkarr's acceptance suite (docs/design/phase1.md §9). It has no code of its
// own: every test file carries the build tag e2e, so a plain `go test ./...` skips it.
//
//	go test -tags e2e ./internal/e2e/...        the binary suite (make test-e2e)
//	sh docker/test-kill.sh bunkarr:dev          the container kill test (make test-docker)
//	sh docker/test-plex-restore.sh bunkarr:dev  the Plex backup/restore test (make test-plex)
//	sh docker/test-shares.sh bunkarr:dev        the SMB/NFS share test (make test-shares)
//	sh docker/test-arr.sh bunkarr:dev           real Radarr/Sonarr webhooks (make test-arr)
//
// The binary suite builds the real bunkarr binary into a temporary directory and drives it only
// through its HTTP API and the filesystem: first-run setup, a source over a generated fixture tree,
// a destination on a temporary directory (allowLocal), full and incremental syncs, hardlinks,
// kill -9 at fault points (BUNKARR_FAULTPOINT) and resume, a missing destination marker, an
// emptied source, dry runs and the mass-change guard; and Phase 2's webhook path (fake Radarr and
// Sonarr serving the recorded fixtures, the recorded webhooks, shortened windows) and "Sign in
// with Plex" (a fake plex.tv and PMS), docs/design/phase2-3.md §15 E2E; and Phase 3's tiers (the
// pinned table's dry run with fake Plex, Tautulli, Seerr and Maintainerr, acceptance 7 with its
// stale case, a demotion and its confirmed release) and an upgrade of a database the Phase 2
// release wrote (BUNKARR_E2E_PREVIOUS_BINARY, else built from the Phase 2 commit). The syncs and
// kill tests also snapshot the source tree around every job: Bunkarr must never modify it (S1).
// BUNKARR_E2E_BINARY runs another binary; BUNKARR_E2E_TARGET_ROOT puts the destinations under
// that directory (the share test).
//
// The Docker tests run only when BUNKARR_E2E_IMAGE names a Bunkarr image; the Plex test also
// needs BUNKARR_E2E_PLEX_IMAGE (the scripts pin plexinc/pms-docker by digest), the share test
// BUNKARR_E2E_SHARES, the *arr test BUNKARR_E2E_ARR (and internet). They use named volumes and reach every HTTP API through `docker exec`, so
// they also work against a remote or docker-in-docker daemon. Containers, volumes, networks and
// images they build are removed; pulled images are kept.
package e2e
