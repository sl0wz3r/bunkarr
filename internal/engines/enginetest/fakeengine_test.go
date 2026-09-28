package enginetest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
)

func TestFakeVersionStore(t *testing.T) {
	ctx := context.Background()
	s := NewFakeVersionStore()
	dir := t.TempDir()
	for name, content := range map[string]string{"manifest.json": `{"files":1}`, "SHA256SUMS": "abc  db.zip\n", "sub/db.zip": "zipdata"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	when := time.Date(2026, 9, 27, 1, 0, 0, 0, time.UTC)
	ref, err := s.Put(ctx, engines.PutVersion{Kind: engines.VersionPlexDB, LogicalPath: ".bunkarr/plex/Plex/20260927T010000Z", Dir: dir, Time: when, JobID: 7, IntegrationID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, engines.PutVersion{LogicalPath: ".bunkarr/plex/Plex/x", Dir: dir}); err == nil {
		t.Error("Put without manifest.json succeeded")
	}
	leftover := s.AddLeftover(engines.StoredVersion{LogicalPath: ".bunkarr/plex/Plex/20260926T010000Z"}, map[string][]byte{"db.zip": []byte("partial")})
	s.AddLeftover(engines.StoredVersion{LogicalPath: ".bunkarr/arr/Radarr/20260926T010000Z", Complete: true}, nil)

	got, err := s.List(ctx, ".bunkarr/plex/Plex")
	if err != nil || len(got) != 2 || got[0].Ref != leftover || got[0].Complete || got[1].Ref != ref || !got[1].Complete ||
		got[1].Files["sub/db.zip"] != 7 || got[1].Version != "20260927T010000Z" || got[1].JobID != 7 || !got[1].Time.Equal(when) {
		t.Fatalf("List = %+v, %v", got, err)
	}
	if b, err := s.ReadFile(ctx, ref, "manifest.json", 5); err != nil || string(b) != `{"fil` {
		t.Errorf("ReadFile = %q, %v", b, err)
	}
	if _, err := s.ReadFile(ctx, ref, "nope", 10); err == nil {
		t.Error("ReadFile of a missing file")
	}
	dst := t.TempDir()
	if err := s.Fetch(ctx, ref, "sub/db.zip", dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "db.zip")); string(b) != "zipdata" {
		t.Errorf("fetched %q", b)
	}
	boom := errors.New("boom")
	s.FailNext("ReadFile", boom)
	if _, err := s.ReadFile(ctx, ref, "manifest.json", 10); !errors.Is(err, boom) {
		t.Errorf("FailNext: %v", err)
	}
	if _, err := s.ReadFile(ctx, ref, "manifest.json", 10); err != nil {
		t.Errorf("FailNext failed twice: %v", err)
	}
	s.FailAlways("List", boom)
	if _, err := s.List(ctx, ".bunkarr/plex"); !errors.Is(err, boom) {
		t.Error("FailAlways")
	}
	s.FailAlways("List", nil)
	if err := s.Remove(ctx, nil, leftover, engines.VersionPlexDB); err != nil {
		t.Fatal(err)
	}
	if rm := s.Removed(); len(rm) != 1 || rm[0].Ref != leftover || rm[0].Kind != engines.VersionPlexDB {
		t.Errorf("Removed = %+v", rm)
	}
	if got, _ := s.List(ctx, ".bunkarr/plex/Plex"); len(got) != 1 {
		t.Errorf("after Remove: %+v", got)
	}
}

func TestFakeEngineAndSession(t *testing.T) {
	ctx := context.Background()
	e := &FakeEngine{EngineKind: engines.Rclone, TestResult: engines.TestResult{OK: true, Reachable: true}}
	sess, err := e.Open(ctx, engines.Destination{ID: 1}, engines.Secrets{}, engines.Runtime{JobID: 9})
	if err != nil || sess == nil {
		t.Fatal(err)
	}
	fs := sess.(*FakeSession)
	fs.FS.HeadTails["Movies/a.mkv"] = "headtail-sha256:ab"
	if h, err := sess.PlanFS().DestHeadTail("Movies/a.mkv"); err != nil || h != "headtail-sha256:ab" {
		t.Errorf("DestHeadTail = %q, %v", h, err)
	}
	if _, err := sess.PlanFS().DestHeadTail("Movies/b.mkv"); !errors.Is(err, engines.ErrNoHeadTail) {
		t.Errorf("missing head/tail: %v", err)
	}
	if _, ok, err := sess.PlanFS().DestStat("x"); ok || err != nil {
		t.Error("DestStat found an object")
	}
	fs.Listed = []engines.Version{{Kind: engines.VersionMedia, SourceID: 1}, {Kind: engines.VersionMedia, SourceID: 2}, {Kind: engines.VersionPlexDB, IntegrationID: 3}}
	if v, _ := sess.List(ctx, engines.ListFilter{Kind: engines.VersionMedia, SourceID: 2}); len(v) != 1 || v[0].SourceID != 2 {
		t.Errorf("List = %+v", v)
	}
	_ = sess.Close()
	if fs.Closed() != 1 || sess.Versions() == nil || !sess.Caps().EnforcesModes {
		t.Error("session")
	}
	if res, err := e.Test(ctx, engines.Destination{ID: 2}, engines.Secrets{}); err != nil || !res.OK {
		t.Errorf("Test = %+v, %v", res, err)
	}
	e.OnCreate = func(context.Context, engines.Destination, engines.Secrets, bool) (engines.CreateResult, error) {
		return engines.CreateResult{MarkerID: "restic:abc", Initialized: true}, errors.New("after init")
	}
	if res, err := e.Create(ctx, engines.Destination{ID: 3}, engines.Secrets{}, true); err == nil || !res.Initialized {
		t.Errorf("Create = %+v, %v", res, err)
	}
	calls := e.Calls()
	if len(calls) != 3 || calls[0].Method != "Open" || calls[0].Runtime.JobID != 9 || calls[2].Method != "Create" || !calls[2].Attach {
		t.Errorf("calls %+v", calls)
	}
}
