package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
)

// File formats of the export and the downloads.
const (
	FormatJSON = "json"
	FormatCSV  = "csv"
)

// stagingDirName is the directory under the config directory that holds staging directories
// (shared with the Plex DB and *arr backups).
const stagingDirName = "staging"

// Staging directory prefixes of on-the-spot exports and downloads.
const (
	exportPrefix   = "manifest-export-"
	downloadPrefix = "manifest-download-"
	// staleStaging is the age after which a staging directory a crash left is removed.
	staleStaging = time.Hour
)

// ValidFileFormat reports whether f is FormatJSON or FormatCSV.
func ValidFileFormat(f string) bool { return f == FormatJSON || f == FormatCSV }

// StagedFile is a manifest file staged in Bunkarr's config directory and hashed before the first
// byte is served (design §11.2, S20): a failed build or a damaged version never sends a partial
// body. Close removes it.
type StagedFile struct {
	// Name is the download's file name; ContentType its media type.
	Name        string
	ContentType string
	// Size and SHA256 (hex) are the staged file's.
	Size   int64
	SHA256 string

	path    string
	dir     string
	release func()
	once    sync.Once
}

// Open opens the staged file for reading.
func (f *StagedFile) Open() (*os.File, error) { return os.Open(f.path) }

// Close removes the staging directory and frees the export slot. It is safe to call twice.
func (f *StagedFile) Close() error {
	var err error
	f.once.Do(func() {
		err = os.RemoveAll(f.dir)
		if f.release != nil {
			f.release()
		}
	})
	return err
}

// Export builds a manifest on the spot, in one read transaction, and stages it in the requested
// format (design §11.2 "Downloads"). destinationID 0 is the export scope (every enabled source,
// tiers null); otherwise it is that destination's view. At most MaxExports run at a time
// (ErrBusy beyond); the slot is held until the StagedFile is closed. The build itself waits for
// a build slot (MaxBuilds, shared with the manifest_export jobs) while ctx lasts.
func (r *Runner) Export(ctx context.Context, destinationID int64, format string) (*StagedFile, error) {
	if !ValidFileFormat(format) {
		return nil, fmt.Errorf("unknown manifest format %q: use json or csv", format)
	}
	select {
	case r.exports <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	release := func() { <-r.exports }
	handed := false
	defer func() {
		// Also when the build panics: the slot is never lost.
		if !handed {
			release()
		}
	}()
	f, err := r.export(ctx, destinationID, format)
	if err != nil {
		return nil, err
	}
	f.release, handed = release, true
	return f, nil
}

func (r *Runner) export(ctx context.Context, destinationID int64, format string) (*StagedFile, error) {
	scope := BuildScope{}
	label := "all"
	if destinationID != 0 {
		d, err := r.destinations.Get(ctx, destinationID)
		if err != nil {
			return nil, err
		}
		scope.Destination = &d
		label = snapshots.Slug(d.Name)
		if label == "" {
			label = fmt.Sprintf("destination-%d", d.ID)
		}
	}
	release, err := r.acquireBuild(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer release()
	m, err := r.builder.Build(ctx, scope)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("bunkarr-manifest-%s-%s.%s", label, snapshots.VersionName(m.CreatedAt), format)
	// Streamed into the staging file: the encoded manifest is never held in memory.
	return r.stage(exportPrefix, name, format, func(w io.Writer) error {
		if format == FormatCSV {
			return WriteCSV(w, m)
		}
		return WriteJSON(w, m)
	})
}

// Download stages the file of recorded version id in the requested format, read through the
// destination (S3: ErrNotMounted and the other Open errors when it is not mounted) and verified
// against the recorded checksum and the version's SHA256SUMS before the first byte is served. A
// version that is missing or does not match is marked damaged and ErrDamaged is returned; one
// already marked damaged returns ErrDamaged without being read.
func (r *Runner) Download(ctx context.Context, id int64, format string) (*StagedFile, error) {
	if !ValidFileFormat(format) {
		return nil, fmt.Errorf("unknown manifest format %q: use json or csv", format)
	}
	v, err := r.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.Integrity != IntegrityOK {
		return nil, ErrDamaged
	}
	h, err := r.destinations.Open(ctx, v.DestinationID)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	f, err := r.download(h, v, format)
	if errors.Is(err, ErrDamaged) {
		if merr := r.store.MarkDamaged(context.WithoutCancel(ctx), v.ID); merr != nil {
			r.log.Error("Could not mark a damaged manifest version", "id", v.ID, "error", merr)
		}
		r.log.Warn("A manifest version is damaged at its destination", "id", v.ID, "path", v.Path, "reason", err.Error())
		return nil, ErrDamaged
	}
	return f, err
}

func (r *Runner) download(h *destinations.Handle, v Version, format string) (*StagedFile, error) {
	sums, err := readSums(h.Root, v.Path)
	if err != nil {
		return nil, err
	}
	if Checksum(sums[JSONName]) != v.Checksum {
		return nil, fmt.Errorf("%w: %s does not match the recorded checksum", ErrDamaged, SumsName)
	}
	file := JSONName
	if format == FormatCSV {
		file = CSVName
	}
	label := snapshots.Slug(h.Destination.Name)
	if label == "" {
		label = fmt.Sprintf("destination-%d", h.Destination.ID)
	}
	name := fmt.Sprintf("bunkarr-manifest-%s-%s.%s", label, path.Base(v.Path), format)
	return r.stage(downloadPrefix, name, format, func(w io.Writer) error {
		sum, _, err := hashVersionFile(h.Root, v.Path, file, w)
		if err != nil {
			return err
		}
		if sum != sums[file] {
			return fmt.Errorf("%w: %s", ErrDamaged, file)
		}
		return nil
	})
}

// stage writes a file into a new 0700 staging directory through fill, hashing it, and returns it
// staged. Staging directories of this kind that a crash left behind are removed first.
func (r *Runner) stage(prefix, name, format string, fill func(io.Writer) error) (*StagedFile, error) {
	base := filepath.Join(r.configDir, stagingDirName)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("create the staging directory: %w", err)
	}
	r.cleanStaleStaging(base)
	dir, err := os.MkdirTemp(base, prefix)
	if err != nil {
		return nil, fmt.Errorf("create a staging directory: %w", err)
	}
	p := filepath.Join(dir, "manifest."+format)
	out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("stage the manifest: %w", err)
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(out, h)}
	err = fill(cw)
	if cerr := out.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("stage the manifest: %w", cerr)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	ct := "application/json"
	if format == FormatCSV {
		ct = "text/csv; charset=utf-8"
	}
	return &StagedFile{Name: name, ContentType: ct, Size: cw.n, SHA256: hex.EncodeToString(h.Sum(nil)), path: p, dir: dir}, nil
}

// cleanStaleStaging removes export and download staging directories older than staleStaging (a
// crash while one was served). Best effort.
func (r *Runner) cleanStaleStaging(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || (!strings.HasPrefix(e.Name(), exportPrefix) && !strings.HasPrefix(e.Name(), downloadPrefix)) {
			continue
		}
		fi, err := e.Info()
		if err != nil || r.now().Sub(fi.ModTime()) < staleStaging {
			continue
		}
		p := filepath.Join(base, e.Name())
		if err := os.RemoveAll(p); err != nil {
			r.log.Warn("Could not remove a stale manifest staging directory", "path", p, "error", err)
		}
	}
}

// countWriter counts the bytes written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
