package jobqueue

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

func TestProgressThrottlingAndETA(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	clock := &manualClock{t: t0}
	m := New(d, nil, Options{ProgressEvery: 2 * time.Second, Now: clock.Now})
	job, _, err := m.store.CreateJob(ctx, syncDest1)
	if err != nil {
		t.Fatal(err)
	}
	job, _, _ = m.store.markRunning(ctx, job.ID, t0)
	r := newReporter(m, job)

	stored := func() (jobs.Progress, string) {
		t.Helper()
		j, err := m.store.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		var hb string
		if err := d.Reader().QueryRowContext(ctx, `SELECT heartbeat_at FROM jobs WHERE id = ?`, job.ID).Scan(&hb); err != nil {
			t.Fatal(err)
		}
		return j.Progress, hb
	}
	const total = 1000

	// The first report is persisted at once.
	r.Progress(jobs.Progress{Phase: "copying", BytesTotal: total, BytesDone: 0, BytesPerSec: 999, ETASeconds: 999})
	if p, _ := stored(); p.Phase != "copying" || p.BytesPerSec != 0 || p.ETASeconds != 0 {
		t.Fatalf("first progress = %+v (runner-set rate/ETA must be ignored)", p)
	}

	// 1 s later: first throughput sample (100 B/s), ETA 9 s; not persisted (< 2 s).
	clock.Add(time.Second)
	r.Progress(jobs.Progress{Phase: "copying", BytesTotal: total, BytesDone: 100})
	live := r.snapshot()
	if live.BytesPerSec != 100 || live.ETASeconds != 9 {
		t.Fatalf("live = %+v; want 100 B/s, ETA 9", live)
	}
	if p, _ := stored(); p.BytesDone != 0 {
		t.Fatalf("persisted within ProgressEvery: %+v", p)
	}

	// 2 s after the first write: EWMA over 10 s, persisted with the heartbeat.
	clock.Add(time.Second)
	r.Progress(jobs.Progress{Phase: "copying", BytesTotal: total, BytesDone: 300})
	wantRate := 100 + (1-math.Exp(-0.1))*(200-100)
	live = r.snapshot()
	if math.Abs(live.BytesPerSec-wantRate) > 1e-9 || live.ETASeconds != int64(math.Ceil(700/wantRate)) {
		t.Fatalf("live = %+v; want rate %.3f, ETA %d", live, wantRate, int64(math.Ceil(700/wantRate)))
	}
	p, hb := stored()
	if p.BytesDone != 300 || math.Abs(p.BytesPerSec-wantRate) > 1e-9 || hb != db.FormatTime(t0.Add(2*time.Second)) {
		t.Fatalf("stored = %+v, heartbeat %s", p, hb)
	}

	// Calls closer together than the sample interval accumulate (rate unchanged).
	clock.Add(100 * time.Millisecond)
	r.Progress(jobs.Progress{Phase: "copying", BytesTotal: total, BytesDone: 350})
	if live = r.snapshot(); live.BytesPerSec != wantRate || live.ETASeconds != int64(math.Ceil(650/wantRate)) {
		t.Fatalf("live = %+v", live)
	}
	for range 50 {
		r.Progress(jobs.Progress{Phase: "copying", BytesTotal: total, BytesDone: 360})
	}
	if p, _ := stored(); p.BytesDone != 300 {
		t.Fatalf("burst was persisted: %+v", p)
	}

	// A new phase restarting the byte counter resets the rate; ETA unknown.
	clock.Add(3 * time.Second)
	r.Progress(jobs.Progress{Phase: "verifying", BytesTotal: total, BytesDone: 0})
	if live = r.snapshot(); live.BytesPerSec != 0 || live.ETASeconds != 0 {
		t.Fatalf("after reset = %+v", live)
	}

	// A closed reporter (job finished) no longer writes.
	r.close()
	clock.Add(time.Hour)
	r.Progress(jobs.Progress{Phase: "late", BytesTotal: total, BytesDone: 1})
	if p, _ := stored(); p.Phase == "late" {
		t.Fatal("closed reporter persisted progress")
	}
}

func TestETA(t *testing.T) {
	cases := []struct {
		remaining int64
		rate      float64
		want      int64
	}{
		{0, 100, 0},
		{-5, 100, 0},
		{100, 0, 0},
		{100, 0.5, 0},
		{100, 100, 1},
		{101, 100, 2},
		{1 << 60, 1, math.MaxInt32},
	}
	for _, tc := range cases {
		if got := eta(tc.remaining, tc.rate); got != tc.want {
			t.Errorf("eta(%d, %v) = %d; want %d", tc.remaining, tc.rate, got, tc.want)
		}
	}
}

type secretStringer struct{ s string }

func (s secretStringer) String() string { return "stringer " + s.s }

func TestReporterLogRedaction(t *testing.T) {
	const secret = "reporter-secret-value-77aa88bb"
	logging.RegisterSecret(secret)
	ctx := context.Background()
	d := openDB(t)
	procLog := &syncBuffer{}
	m := New(d, jsonLogger(procLog), Options{})
	t.Cleanup(func() { _ = m.Stop(ctx) })
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		env.Reporter.Log(slog.LevelWarn, "request to http://plex:32400/?X-Plex-Token="+secret+" failed",
			"token", "plain-token-by-key",
			"apiKey", 12345,
			"url", "http://plex:32400/library?X-Plex-Token="+secret,
			"error", errors.New("dial: "+secret),
			"who", secretStringer{secret},
			slog.Group("auth", "password", "hunter22-hunter22", "user", "bob"),
			"payload", map[string]any{"Authorization": "Bearer abc", "note": "has " + secret, "n": 3},
			"count", 3, "ok", true, "took", 1500*time.Millisecond,
		)
		env.Reporter.Log(slog.LevelDebug, "debug lines follow the process log level", "k", "v")
		return jobs.Result{}, nil
	}))
	start(t, m)
	j := enqueue(t, m, syncDest1)
	waitStatus(t, m.store, j.ID, jobs.StatusCompleted)

	logs, err := m.store.ListLogs(ctx, j.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var line *LogEntry
	for i := range logs {
		if strings.HasPrefix(logs[i].Message, "request to") {
			line = &logs[i]
		}
		if raw, _ := json.Marshal(logs[i]); strings.Contains(string(raw), secret) || strings.Contains(string(raw), "hunter22") ||
			strings.Contains(string(raw), "plain-token-by-key") || strings.Contains(string(raw), "Bearer abc") {
			t.Fatalf("secret in job log: %s", raw)
		}
	}
	if line == nil {
		t.Fatalf("log line missing: %+v", logs)
	}
	if line.Level != "warn" || !strings.Contains(line.Message, logging.Redacted) {
		t.Fatalf("line = %+v", line)
	}
	var f map[string]any
	if err := json.Unmarshal(line.Fields, &f); err != nil {
		t.Fatal(err)
	}
	if f["token"] != logging.Redacted || f["apiKey"] != logging.Redacted {
		t.Fatalf("sensitive keys not redacted: %v", f)
	}
	if f["count"] != float64(3) || f["ok"] != true || f["took"] != "1.5s" {
		t.Fatalf("plain fields changed: %v", f)
	}
	if g, _ := f["auth"].(map[string]any); g["password"] != logging.Redacted || g["user"] != "bob" {
		t.Fatalf("group = %v", f["auth"])
	}
	if p, _ := f["payload"].(map[string]any); p["Authorization"] != logging.Redacted || p["n"] != float64(3) {
		t.Fatalf("payload = %v", f["payload"])
	}
	if s, _ := f["who"].(string); !strings.HasPrefix(s, "stringer ") {
		t.Fatalf("stringer = %v", f["who"])
	}
	debugKept := false
	for _, l := range logs {
		debugKept = debugKept || strings.HasPrefix(l.Message, "debug lines")
	}
	if !debugKept {
		t.Fatal("debug line dropped although the process log has debug enabled")
	}
	out := procLog.String()
	if strings.Contains(out, secret) || strings.Contains(out, "hunter22") || strings.Contains(out, "plain-token-by-key") {
		t.Fatalf("secret in process log: %s", out)
	}
	if !strings.Contains(out, `"jobId":`+jsonInt(j.ID)) {
		t.Fatalf("process log lines must carry the job id: %s", out)
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestDebugJobLogsFollowProcessLevel(t *testing.T) {
	ctx := context.Background()
	d := openDB(t)
	info := slog.New(slog.NewJSONHandler(&syncBuffer{}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	m := New(d, info, Options{})
	t.Cleanup(func() { _ = m.Stop(ctx) })
	m.Register(jobs.TypeSync, jobs.RunnerFunc(func(ctx context.Context, job jobs.Job, env jobs.Env) (jobs.Result, error) {
		env.Reporter.Log(slog.LevelDebug, "noise")
		env.Reporter.Log(slog.LevelInfo, "signal")
		return jobs.Result{}, nil
	}))
	start(t, m)
	j := enqueue(t, m, syncDest1)
	waitStatus(t, m.store, j.ID, jobs.StatusCompleted)
	logs, _ := m.store.ListLogs(ctx, j.ID, 0, 0)
	var msgs []string
	for _, l := range logs {
		msgs = append(msgs, l.Message)
	}
	joined := strings.Join(msgs, "|")
	if strings.Contains(joined, "noise") || !strings.Contains(joined, "signal") {
		t.Fatalf("logs = %v", msgs)
	}
}

func TestListLogsAfterIDAndAppendValidation(t *testing.T) {
	ctx := context.Background()
	st := NewStore(openDB(t))
	job := newJob(t, st)
	var ids []int64
	for i, lvl := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError, slog.LevelError + 4} {
		id, err := st.AppendLog(ctx, job.ID, lvl, "line", json.RawMessage(`{"i":`+jsonInt(int64(i))+`}`))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	all, err := st.ListLogs(ctx, job.ID, 0, 0)
	if err != nil || len(all) != 5 {
		t.Fatalf("ListLogs = %d, %v", len(all), err)
	}
	levels := []string{}
	for _, l := range all {
		levels = append(levels, l.Level)
	}
	if strings.Join(levels, ",") != "debug,info,warn,error,error" {
		t.Fatalf("levels = %v", levels)
	}
	after, _ := st.ListLogs(ctx, job.ID, ids[2], 1)
	if len(after) != 1 || after[0].ID != ids[3] || string(after[0].Fields) != `{"i":3}` {
		t.Fatalf("after = %+v", after)
	}
	if _, err := st.AppendLog(ctx, job.ID, slog.LevelInfo, "x", json.RawMessage(`[1]`)); !isValidation(err) {
		t.Fatalf("non-object fields accepted: %v", err)
	}
	if _, err := st.ListLogs(ctx, 999, 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListLogs(unknown) = %v", err)
	}
}
