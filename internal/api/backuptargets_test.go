package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/manifest"
)

// TestIntegrationBackupTargets: a Plex integration with several backup targets gets one schedule
// per target; GET shows the targets and the single form mirroring targets[0]; the Phase 1 single
// form still works and replaces targets[0] only; a schedule edited on System → Tasks shows in its
// target; a manual backup without a destination uses targets[0]; a deleted destination leaves the
// targets (phase4.md §8.5).
func TestIntegrationBackupTargets(t *testing.T) {
	e := newEnv(t, nil)
	d1 := e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, nil)
	d2 := e.createDestination(t, "NAS2", e.mkdir(t, "nas2"), nil, nil)
	d3 := e.createDestination(t, "NAS3", e.mkdir(t, "nas3"), nil, nil)
	target := func(dest int64, cron string, enabled bool) map[string]any {
		return map[string]any{"destinationId": dest, "cron": cron, "enabled": enabled}
	}
	settings := func(backup map[string]any) map[string]any {
		return map[string]any{"name": "Plex", "url": "http://127.0.0.1:9", "settings": map[string]any{"dataPath": "/plex", "backup": backup}}
	}
	body := settings(map[string]any{"targets": []any{target(d1, "0 6 * * *", true), target(d2, "0 7 * * 0", true), target(d3, "", false)}})
	body["type"] = "plex"
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", body, &it)
	path := fmt.Sprintf("/integrations/%d", it.ID)
	schedules := func() map[int64]scheduleView {
		out := map[int64]scheduleView{}
		for _, sc := range e.plexSchedules(t, it.ID) {
			out[sc.Params.DestinationID] = sc
		}
		return out
	}
	scs := schedules()
	if len(scs) != 2 || scs[d1].Cron != "0 6 * * *" || scs[d2].Cron != "0 7 * * 0" || !scs[d2].Enabled {
		t.Fatalf("schedules of the targets: %+v", scs)
	}
	ps := plexSettingsOf(t, it)
	if len(ps.Backup.Targets) != 3 || ps.Backup.DestinationID != d1 || ps.Backup.Cron != "0 6 * * *" || !ps.Backup.Enabled {
		t.Fatalf("backup settings: %+v", ps.Backup)
	}

	// System → Tasks edits a target's schedule.
	e.call(t, 200, "PUT", fmt.Sprintf("/schedules/%d", scs[d2].ID), map[string]any{"cron": "30 7 * * 0", "enabled": false}, nil)
	e.call(t, 200, "GET", path, nil, &it)
	ps = plexSettingsOf(t, it)
	if ps.Backup.Targets[1].Cron != "30 7 * * 0" || ps.Backup.Targets[1].Enabled {
		t.Fatalf("target after a schedule edit: %+v", ps.Backup.Targets)
	}

	// The single form (a client that knows only Phase 1) replaces targets[0] and keeps the others.
	e.call(t, 200, "PUT", path, settings(map[string]any{"destinationId": d1, "cron": "15 5 * * *", "enabled": true}), &it)
	scs = schedules()
	if len(scs) != 2 || scs[d1].Cron != "15 5 * * *" || scs[d2].Cron != "30 7 * * 0" || scs[d2].Enabled {
		t.Fatalf("schedules after a single-form update: %+v", scs)
	}

	// A manual backup without a destination goes to targets[0].
	var j jobs.Job
	e.call(t, 202, "POST", path+"/plex/backup", map[string]any{"dryRun": true}, &j)
	if j.Params.DestinationID != d1 {
		t.Fatalf("backup job: %+v", j.Params)
	}
	e.waitJob(t, j.ID)

	// Invalid targets: more than four, one destination twice, a missing destination.
	for _, bad := range []map[string]any{
		{"targets": []any{target(d1, "", false), target(d1, "", false)}},
		{"targets": []any{target(d1, "", false), target(d2, "", false), target(d3, "", false), target(d1+10, "", false), target(d1+11, "", false)}},
		{"targets": []any{target(999, "0 6 * * *", true)}},
		{"targets": []any{target(d1, "every day", true)}},
	} {
		if code, msg := e.status(t, "PUT", path, settings(bad)); code != 400 {
			t.Errorf("targets %v: %d %q", bad, code, msg)
		}
	}

	// A deleted destination leaves the targets and takes its schedule along.
	e.call(t, 204, "DELETE", fmt.Sprintf("/destinations/%d", d1), nil, nil)
	e.call(t, 200, "GET", path, nil, &it)
	ps = plexSettingsOf(t, it)
	if len(ps.Backup.Targets) != 2 || ps.Backup.Targets[0].DestinationID != d2 || ps.Backup.DestinationID != d2 {
		t.Fatalf("targets after the delete: %+v", ps.Backup)
	}
	if scs = schedules(); len(scs) != 1 || scs[d2].ID == 0 {
		t.Fatalf("schedules after the delete: %+v", scs)
	}
}

// TestPlexBackupNoneRemovesTargets: "None" on the Plex page sends the targets form with no
// targets; the re-encoded body (preparePlexSettings) must keep that empty list, or the store
// takes it for a single-form client and keeps the stored targets[1:] (NAS2 would become the
// first target, its schedule still on).
func TestPlexBackupNoneRemovesTargets(t *testing.T) {
	e := newEnv(t, nil)
	d1 := e.createDestination(t, "NAS", e.mkdir(t, "nas"), nil, nil)
	d2 := e.createDestination(t, "NAS2", e.mkdir(t, "nas2"), nil, nil)
	target := func(dest int64, cron string) map[string]any {
		return map[string]any{"destinationId": dest, "cron": cron, "enabled": true}
	}
	settings := func(backup map[string]any) map[string]any {
		return map[string]any{"name": "Plex", "url": "http://127.0.0.1:9", "settings": map[string]any{"dataPath": "/plex", "backup": backup}}
	}
	body := settings(map[string]any{"targets": []any{target(d1, "0 6 * * *"), target(d2, "0 7 * * 0")}})
	body["type"] = "plex"
	var it integrations.Integration
	e.call(t, 201, "POST", "/integrations", body, &it)
	if n := len(e.plexSchedules(t, it.ID)); n != 2 {
		t.Fatalf("schedules before: %d", n)
	}
	path := fmt.Sprintf("/integrations/%d", it.ID)
	e.call(t, 200, "PUT", path, settings(map[string]any{"destinationId": 0, "cron": "0 6 * * *", "enabled": false, "targets": []any{}}), &it)
	ps := plexSettingsOf(t, it)
	if len(ps.Backup.Targets) != 0 || ps.Backup.DestinationID != 0 || ps.Backup.Enabled {
		t.Fatalf("None kept targets: %+v", ps.Backup)
	}
	if scs := e.plexSchedules(t, it.ID); len(scs) != 0 {
		t.Fatalf("None kept schedules: %+v", scs)
	}
}

// TestArrBackupTargets: an *arr integration's targets, each with its own schedule and
// acceptInsecureModes, and the legacy single form with the top-level acceptInsecureModes.
func TestArrBackupTargets(t *testing.T) {
	e := newEnv(t, nil)
	fake, folder, d1 := backupSetup(t, e)
	d2 := e.createDestination(t, "NAS2", e.mkdir(t, "nas2"), nil, nil)
	it := e.createRadarr(t, fake.URL, map[string]any{"backupFolder": folder, "backup": map[string]any{"targets": []any{
		map[string]any{"destinationId": d1, "cron": "0 3 * * 0", "enabled": true},
		map[string]any{"destinationId": d2, "cron": "0 4 * * 1", "enabled": true},
	}}})
	var dests []int64
	for _, sc := range e.arrBackupSchedules(t) {
		if sc.Params.IntegrationID == it.ID {
			dests = append(dests, sc.Params.DestinationID)
		}
	}
	slices.Sort(dests)
	if !slices.Equal(dests, []int64{d1, d2}) {
		t.Fatalf("arr backup schedules: %v", dests)
	}
	as, err := it.ArrSettings()
	if err != nil || len(as.Backup.Targets) != 2 || as.Backup.DestinationID != d1 {
		t.Fatalf("arr backup settings: %+v %v", as.Backup, err)
	}
	// Legacy form: the single destination replaces targets[0].
	var got integrations.Integration
	e.call(t, 200, "PUT", fmt.Sprintf("/integrations/%d", it.ID), map[string]any{"name": "Radarr", "url": fake.URL,
		"settings": map[string]any{"backupFolder": folder, "backup": map[string]any{"destinationId": d2, "cron": "0 5 * * *", "enabled": true}}}, &got)
	as, _ = got.ArrSettings()
	if len(as.Backup.Targets) != 1 || as.Backup.Targets[0].DestinationID != d2 || as.Backup.Targets[0].Cron != "0 5 * * *" {
		t.Fatalf("after the single form: %+v", as.Backup)
	}
}

// TestManifestDownloadFromEngine: a manifest version at a restic destination is written through
// the destination's version store and downloads with its checksum verified; a store that cannot
// be read is 409 "destination not reachable", and damaged content 409 "manifest damaged: checksum
// mismatch" (phase4.md §8.6).
func TestManifestDownloadFromEngine(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	c := ee.session(t)
	movies := ee.mkdir(t, "media/movies")
	writeFile(t, movies+"/Film (2020)/Film.mkv", "film", mustTime(t, "2026-09-01T10:00:00Z"))
	src := ee.createSource(t, "Movies", movies)
	body := s3Body("Offsite", "restic", "media", "manifest")
	body["attach"] = true
	body["sourceIds"] = []int64{src}
	body["encryption"] = map[string]any{"mode": "restic", "generate": false, "secret": testUserSecret}
	id := ee.createEngineDest(t, c, body)

	var j jobs.Job
	ee.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/manifest", id), nil, &j)
	if done := ee.waitJob(t, j.ID); done.Status != jobs.StatusCompleted {
		t.Fatalf("manifest export: %s %q", done.Status, done.Error)
	}
	var versions []manifest.Version
	ee.call(t, 200, "GET", fmt.Sprintf("/destinations/%d/manifests", id), nil, &versions)
	if len(versions) != 1 || versions[0].EngineRef == "" || len(ee.versions.Stored()) != 1 {
		t.Fatalf("manifest versions: %+v, stored %d", versions, len(ee.versions.Stored()))
	}
	dl := fmt.Sprintf("/manifests/%d/download", versions[0].ID)
	code, raw, hdr := ee.sessionRaw(t, c, "GET", dl, nil)
	sum := sha256.Sum256(raw)
	if code != 200 || hdr.Get(SHA256Header) != hex.EncodeToString(sum[:]) || !strings.Contains(string(raw), `"bunkarr-manifest"`) {
		t.Fatalf("download: %d %q %.200s", code, hdr.Get(SHA256Header), raw)
	}

	ee.versions.FailNext("ReadFile", errors.New("connection refused"))
	if code, msg := ee.status(t, "GET", dl, nil); code != 409 || msg != manifest.ErrUnreachable.Error() {
		t.Fatalf("download from an unreachable destination: %d %q", code, msg)
	}
	ee.versions.setCorrupt(true)
	if code, msg := ee.status(t, "GET", dl, nil); code != 409 || msg != manifest.ErrDamaged.Error() {
		t.Fatalf("download of a damaged version: %d %q", code, msg)
	}
}
