package bwlimit

import (
	"io"
	"time"
)

// NewWriter returns a writer that passes everything to w at no more than rate() bytes per second:
// a token bucket that holds at most 100 ms of data, so a pause is never followed by a burst. rate
// is read again at least once a second, so a timetable change point takes effect during a
// transfer; 0 (or less) means unlimited. filecopy wraps its destination writer with it
// (docs/design/phase4.md §9.1). The writer is not safe for concurrent use.
func NewWriter(w io.Writer, rate func() int64) io.Writer {
	return newWriter(w, rate, time.Now, time.Sleep)
}

func newWriter(w io.Writer, rate func() int64, now func() time.Time, sleep func(time.Duration)) *limitedWriter {
	return &limitedWriter{w: w, rate: rate, now: now, sleep: sleep}
}

// rateInterval is how often the writer reads its rate again.
const rateInterval = time.Second

type limitedWriter struct {
	w      io.Writer
	rate   func() int64
	now    func() time.Time
	sleep  func(time.Duration)
	cur    int64     // the rate in force (bytes/s)
	readAt time.Time // when cur was read
	tokens float64   // bytes that may be written now
	last   time.Time // last refill
}

// Write implements io.Writer.
func (l *limitedWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		now := l.now()
		if l.readAt.IsZero() || now.Sub(l.readAt) >= rateInterval {
			l.cur, l.readAt = l.rate(), now
		}
		r := l.cur
		if r <= 0 {
			l.last = time.Time{}
			n, err := l.w.Write(p)
			return written + n, err
		}
		burst := max(r/10, 1)
		if !l.last.IsZero() {
			l.tokens += now.Sub(l.last).Seconds() * float64(r)
		}
		l.last = now
		l.tokens = min(l.tokens, float64(burst))
		chunk := min(int64(len(p)), burst)
		if l.tokens < float64(chunk) {
			wait := time.Duration((float64(chunk) - l.tokens) / float64(r) * float64(time.Second))
			l.sleep(min(max(wait, time.Millisecond), rateInterval))
			continue
		}
		n, err := l.w.Write(p[:chunk])
		written += n
		l.tokens -= float64(n)
		p = p[n:]
		if err != nil {
			return written, err
		}
		if n < int(chunk) {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}
