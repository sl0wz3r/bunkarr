package mediaindex

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

func TestStoreFactQueries(t *testing.T) {
	e := newEnv(t, arr.KindSonarr)
	e.full()
	s := e.runner.Store()
	ctx := context.Background()
	// FilesUnder a source's path: every file of the source, ordered by local path.
	var under []File
	if err := s.FilesUnder(ctx, nil, e.src.Path, func(f File) error { under = append(under, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(under) != 6 {
		t.Fatalf("FilesUnder = %d files", len(under))
	}
	for i := 1; i < len(under); i++ {
		if under[i-1].LocalPath > under[i].LocalPath {
			t.Fatal("FilesUnder is not ordered by local path")
		}
	}
	// A sibling folder with a longer name is not under the source.
	var none int
	_ = s.FilesUnder(ctx, nil, e.src.Path+"/The Beverly", func(File) error { none++; return nil })
	if none != 0 {
		t.Fatalf("FilesUnder matched a partial name: %d", none)
	}
	// FilesAt one local path.
	at, err := s.FilesAt(ctx, nil, under[0].LocalPath)
	if err != nil || len(at) != 1 || at[0].ArrFileID != under[0].ArrFileID {
		t.Fatalf("FilesAt = %+v, %v", at, err)
	}
	// Inside a read transaction.
	tx, err := e.db.Reader().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := s.EachItem(ctx, tx, e.it.ID, false, func(Item) error { n++; return nil }); err != nil || n != 2 {
		t.Fatalf("EachItem(tx) = %d, %v", n, err)
	}
	n = 0
	if err := s.EachFile(ctx, tx, 0, false, func(File) error { n++; return nil }); err != nil || n != 6 {
		t.Fatalf("EachFile(tx, all) = %d, %v", n, err)
	}
	stop := errors.New("stop")
	if err := s.EachItem(ctx, nil, 0, true, func(Item) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("EachItem did not stop: %v", err)
	}
	it, err := s.Item(ctx, nil, e.it.ID, KindSeries, 2)
	if err != nil || it.Title != "One Step Beyond" {
		t.Fatalf("Item = %+v, %v", it, err)
	}
	if _, err := s.Item(ctx, nil, e.it.ID, KindSeries, 99); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("Item(99) = %v", err)
	}
	byID, err := s.ItemsByID(ctx, nil, []int64{it.ID, 12345})
	if err != nil || len(byID) != 1 || byID[it.ID].ArrID != 2 {
		t.Fatalf("ItemsByID = %v, %v", byID, err)
	}
	files, err := s.FilesOfItem(ctx, nil, it.ID)
	if err != nil || len(files) != 1 {
		t.Fatalf("FilesOfItem = %v, %v", files, err)
	}
	states, err := s.States(ctx, nil)
	if err != nil || states[e.it.ID].Status != StatusOK {
		t.Fatalf("States = %v, %v", states, err)
	}
	// Files of deleted items supply no facts.
	e.editList("series", "series.json", func(l []map[string]any) []map[string]any { return l[:1] })
	e.full()
	under = nil
	_ = s.FilesUnder(ctx, nil, e.src.Path, func(f File) error { under = append(under, f); return nil })
	if len(under) != 5 {
		t.Fatalf("FilesUnder after a deletion = %d, want 5", len(under))
	}
}

func TestIndexStatus(t *testing.T) {
	e := newEnv(t, arr.KindRadarr)
	v, err := e.runner.Store().IndexStatus(context.Background(), e.it)
	if err != nil || v.Status != StatusNever || v.Fresh || v.RefreshedAt != nil || v.StaleAfterHours != 24 || string(v.Stats) != "{}" {
		t.Fatalf("before = %+v, %v", v, err)
	}
	e.full()
	v, err = e.runner.Store().IndexStatus(context.Background(), e.it)
	if err != nil || v.Status != StatusOK || !v.Fresh || !v.InstanceMatches || v.Error != nil || !strings.Contains(string(v.Stats), `"files":3`) {
		t.Fatalf("after = %+v (%s), %v", v, v.Stats, err)
	}
	e.clock.Advance(25 * 60 * 60 * 1e9)
	v, _ = e.runner.Store().IndexStatus(context.Background(), e.it)
	if v.Fresh || !strings.Contains(v.Reason, "25 h old") {
		t.Fatalf("stale = %+v", v)
	}
	plex := integrations.Integration{ID: 99, Type: integrations.TypePlex}
	if v, err := e.runner.Store().IndexStatus(context.Background(), plex); err != nil || v.Fresh || v.Reason == "" {
		t.Fatalf("plex = %+v, %v", v, err)
	}
}

func TestQueueStartupAndNeedsRefresh(t *testing.T) {
	enq := &memEnqueuer{}
	list := []integrations.Integration{
		{ID: 1, Name: "Radarr", Type: integrations.TypeRadarr, Enabled: true},
		{ID: 2, Name: "Off", Type: integrations.TypeSonarr, Enabled: false},
		{ID: 3, Name: "Plex", Type: integrations.TypePlex, Enabled: true},
		{ID: 4, Name: "Lidarr", Type: integrations.TypeLidarr, Enabled: true},
	}
	got, err := QueueStartup(context.Background(), list, enq)
	if err != nil || len(got) != 2 || got[0].Params.IntegrationID != 1 || got[1].Params.IntegrationID != 4 ||
		got[0].Trigger != jobs.TriggerStartup || got[0].Type != jobs.TypeRefresh {
		t.Fatalf("QueueStartup = %+v, %v", got, err)
	}
	// A Phase 1 install (Plex only) queues nothing.
	enq = &memEnqueuer{}
	if got, _ := QueueStartup(context.Background(), list[2:3], enq); len(got) != 0 || len(enq.queued()) != 0 {
		t.Fatalf("Plex only queued %v", got)
	}
	enq.fail = errors.New("boom")
	if _, err := QueueStartup(context.Background(), list[:1], enq); err == nil {
		t.Fatal("enqueue error lost")
	}

	arrIt := func(url, mappings string, enabled bool) integrations.Integration {
		return integrations.Integration{Type: integrations.TypeRadarr, URL: url, Enabled: enabled,
			Settings: []byte(`{"pathMappings":` + mappings + `}`)}
	}
	a := arrIt("http://r:7878", `[{"arr":"/movies","local":"/media/movies"}]`, true)
	cases := []struct {
		name   string
		before *integrations.Integration
		after  integrations.Integration
		key    bool
		want   bool
	}{
		{"created", nil, a, false, true},
		{"unchanged", &a, a, false, false},
		{"key changed", &a, a, true, true},
		{"url changed", &a, arrIt("http://other:7878", `[{"arr":"/movies","local":"/media/movies"}]`, true), false, true},
		{"mappings changed", &a, arrIt("http://r:7878", `[{"arr":"/movies","local":"/media/films"}]`, true), false, true},
		{"re-enabled", ptr(arrIt("http://r:7878", `[]`, false)), arrIt("http://r:7878", `[]`, true), false, true},
		{"disabled", &a, arrIt("http://r:7878", `[]`, false), true, false},
		{"plex", nil, integrations.Integration{Type: integrations.TypePlex, Enabled: true}, false, false},
	}
	for _, c := range cases {
		if got := NeedsRefresh(c.before, c.after, c.key); got != c.want {
			t.Errorf("%s: NeedsRefresh = %v, want %v", c.name, got, c.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestPool(t *testing.T) {
	ctx := context.Background()
	var got atomic.Int64
	err := pool(ctx, 4, 50, func(_ context.Context, i int) (fetched, error) {
		return fetched{arrID: int64(i)}, nil
	}, func(f fetched) error { got.Add(f.arrID); return nil })
	if err != nil || got.Load() != 49*50/2 {
		t.Fatalf("pool = %d, %v", got.Load(), err)
	}
	boom := errors.New("boom")
	err = pool(ctx, 4, 50, func(_ context.Context, i int) (fetched, error) {
		if i == 7 {
			return fetched{}, boom
		}
		return fetched{}, nil
	}, func(fetched) error { return nil })
	if !errors.Is(err, boom) {
		t.Fatalf("fetch error = %v", err)
	}
	err = pool(ctx, 2, 10, func(context.Context, int) (fetched, error) { return fetched{}, nil },
		func(fetched) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("fn error = %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	err = pool(cctx, 2, 10, func(ctx context.Context, _ int) (fetched, error) { return fetched{}, ctx.Err() },
		func(fetched) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled = %v", err)
	}
	if err := pool(ctx, 3, 0, nil, nil); err != nil {
		t.Fatalf("empty pool = %v", err)
	}
}
