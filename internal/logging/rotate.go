package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingWriter appends to <dir>/<name>.log and, when the file would exceed maxBytes, renames it
// to <name>.1.log (shifting older files up to <name>.<keep>.log, dropping the oldest).
type RotatingWriter struct {
	mu       sync.Mutex
	dir      string
	name     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

// NewRotatingWriter opens (creating) the log file.
func NewRotatingWriter(dir, name string, maxBytes int64, keep int) (*RotatingWriter, error) {
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	if keep <= 0 {
		keep = 5
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	w := &RotatingWriter{dir: dir, name: name, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingWriter) path(n int) string {
	if n == 0 {
		return filepath.Join(w.dir, w.name+".log")
	}
	return filepath.Join(w.dir, fmt.Sprintf("%s.%d.log", w.name, n))
}

func (w *RotatingWriter) open() error {
	f, err := os.OpenFile(w.path(0), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("open log file: %w", err)
	}
	w.f, w.size = f, fi.Size()
	return nil
}

// Write implements io.Writer. slog handlers write one record per call, so records never straddle
// two files.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingWriter) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil
	_ = os.Remove(w.path(w.keep))
	for i := w.keep - 1; i >= 0; i-- {
		if _, err := os.Stat(w.path(i)); err == nil {
			if err := os.Rename(w.path(i), w.path(i+1)); err != nil {
				return fmt.Errorf("rotate log: %w", err)
			}
		}
	}
	return w.open()
}

// Close closes the current file.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
