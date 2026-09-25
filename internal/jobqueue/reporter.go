package jobqueue

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

const (
	// throughputWindow is the EWMA time constant of BytesPerSec.
	throughputWindow = 10 * time.Second
	// minRateSample is the shortest interval a throughput sample spans; faster Progress calls
	// accumulate into one sample.
	minRateSample = 500 * time.Millisecond
)

// reporter is the jobs.Reporter of one running job. Progress lives in memory (Manager.Get merges
// it in) and is persisted, together with the heartbeat, at most every ProgressEvery.
type reporter struct {
	m       *Manager
	jobID   int64
	jobType jobs.Type
	every   time.Duration
	// closed stops database writes once the job's final state (or re-queue) is written.
	closed atomic.Bool

	mu          sync.Mutex
	p           jobs.Progress
	lastPersist time.Time
	persisted   bool
	sampleAt    time.Time
	sampleBytes int64
	sampled     bool
	rate        float64
	hasRate     bool
}

var _ jobs.Reporter = (*reporter)(nil)

func newReporter(m *Manager, job jobs.Job) *reporter {
	return &reporter{m: m, jobID: job.ID, jobType: job.Type, every: m.opts.ProgressEvery, p: job.Progress}
}

// Progress implements jobs.Reporter. BytesPerSec and ETASeconds are computed here; values the
// runner set are ignored.
func (r *reporter) Progress(p jobs.Progress) {
	now := r.m.now()
	r.mu.Lock()
	r.sample(now, p.BytesDone)
	p.BytesPerSec = r.rate
	p.ETASeconds = eta(p.BytesTotal-p.BytesDone, r.rate)
	r.p = p
	persist := !r.persisted || now.Sub(r.lastPersist) >= r.every
	var data []byte
	if persist {
		r.persisted, r.lastPersist = true, now
		data = r.encodeLocked()
	}
	r.mu.Unlock()
	if persist && !r.closed.Load() {
		if err := r.m.store.saveProgress(r.m.ctx(), r.jobID, data, now); err != nil {
			r.m.log.Warn("Could not save job progress", "jobId", r.jobID, "error", err)
		}
	}
}

// sample feeds the throughput EWMA with the bytes done at now. Caller holds r.mu.
func (r *reporter) sample(now time.Time, bytesDone int64) {
	if !r.sampled || bytesDone < r.sampleBytes {
		// First call, or the runner restarted its byte counter (a new phase): new baseline.
		if r.sampled {
			r.rate, r.hasRate = 0, false
		}
		r.sampleAt, r.sampleBytes, r.sampled = now, bytesDone, true
		return
	}
	dt := now.Sub(r.sampleAt)
	if dt < minRateSample {
		return
	}
	inst := float64(bytesDone-r.sampleBytes) / dt.Seconds()
	if !r.hasRate {
		r.rate, r.hasRate = inst, true
	} else {
		alpha := 1 - math.Exp(-dt.Seconds()/throughputWindow.Seconds())
		r.rate += alpha * (inst - r.rate)
	}
	r.sampleAt, r.sampleBytes = now, bytesDone
}

// eta is the whole seconds remaining at rate bytes/s (0 = unknown or done).
func eta(remaining int64, rate float64) int64 {
	if remaining <= 0 || rate < 1 {
		return 0
	}
	s := math.Ceil(float64(remaining) / rate)
	if s > float64(math.MaxInt32) {
		return math.MaxInt32
	}
	return int64(s)
}

// snapshot returns the current in-memory progress.
func (r *reporter) snapshot() jobs.Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.p
	p.CurrentFile = redactText(p.CurrentFile)
	return p
}

// encode returns the current progress as stored JSON.
func (r *reporter) encode() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.encodeLocked()
}

func (r *reporter) encodeLocked() []byte {
	p := r.p
	p.CurrentFile = redactText(p.CurrentFile)
	b, err := json.Marshal(p)
	if err != nil {
		// jobs.Progress has only strings and numbers (BytesPerSec is finite: it is built from
		// finite samples), so this does not happen; keep the previous value rather than fail.
		return []byte("{}")
	}
	return b
}

// close stops database writes (progress and log lines) of this reporter.
func (r *reporter) close() { r.closed.Store(true) }

// Log implements jobs.Reporter: the line goes to the process log (with the job id) and to
// job_logs. The message and every attribute are redacted (keys by name, values against the
// registered secrets). Debug lines are persisted only when the process log has debug enabled.
func (r *reporter) Log(level slog.Level, msg string, args ...any) {
	msg = redactText(msg)
	attrs := redactArgs(args)
	ctx := context.Background()
	all := make([]slog.Attr, 0, len(attrs)+2)
	all = append(all, slog.Int64("jobId", r.jobID), slog.String("jobType", string(r.jobType)))
	all = append(all, attrs...)
	r.m.log.LogAttrs(ctx, level, msg, all...)
	if level < slog.LevelInfo && !r.m.log.Enabled(ctx, level) {
		return
	}
	if r.closed.Load() {
		return
	}
	if _, err := r.m.store.AppendLog(r.m.ctx(), r.jobID, level, msg, fieldsJSON(attrs)); err != nil {
		r.m.log.Warn("Could not write job log", "jobId", r.jobID, "error", err)
	}
}
