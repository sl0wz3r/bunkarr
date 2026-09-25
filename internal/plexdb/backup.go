package plexdb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"modernc.org/sqlite"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// maxPrefsBytes bounds how much of Preferences.xml is read (it is a few KiB).
const maxPrefsBytes = 16 << 20

// BackupOptions configures Backup.
type BackupOptions struct {
	// DataPath is Plex's "Plex Media Server" directory as Bunkarr sees it (normally mounted
	// read-only), holding Preferences.xml and "Plug-in Support/Databases". Absolute.
	DataPath string
	// StagingDir receives the copies. It is created (0700) when missing and must not already hold
	// files with the backup's names.
	StagingDir string
	// BusyTimeout is the busy_timeout of the connection to a live database (default
	// DefaultBusyTimeout).
	BusyTimeout time.Duration
	// Attempts is how many times a copy is tried per file (default DefaultAttempts).
	Attempts int
	// Log receives progress and retry messages; nil discards them. File contents are never
	// logged.
	Log func(level slog.Level, msg string, args ...any)

	// afterCopy, when set (tests), runs after a database copy was taken and before the checks
	// that follow it; attempt counts from 1.
	afterCopy func(name string, attempt int)
}

// StagedFile is one file Backup copied into the staging directory.
type StagedFile struct {
	// Name is the file's base name (LibraryDB, BlobsDB or PreferencesXML), also its name in a
	// snapshot version.
	Name string `json:"name"`
	// Path is the staged copy.
	Path string `json:"path"`
	// Source is the file that was copied.
	Source string `json:"source"`
	// Size and SHA256 (lowercase hex) describe the staged copy.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Method is MethodOnlineBackup, MethodOnlineBackupImmutable or MethodCopy.
	Method string `json:"method"`
	// Attempts is how many tries the copy took.
	Attempts int `json:"attempts"`
	// SQLiteVersion is the version of the SQLite that made a database copy ("" for
	// Preferences.xml).
	SQLiteVersion string `json:"sqliteVersion,omitempty"`
}

// BackupResult is what Backup staged.
type BackupResult struct {
	// Files are the staged files: the library database first, then the blobs database when
	// present, then Preferences.xml when it could be read.
	Files []StagedFile `json:"files"`
	// Method is the library database's method.
	Method string `json:"method"`
	// SQLiteVersion is the version of the SQLite that made the copies.
	SQLiteVersion string `json:"sqliteVersion"`
	// TakenAt is when the library database copy completed.
	TakenAt time.Time `json:"takenAt"`
	// Warnings are problems that did not stop the backup (Preferences.xml unreadable).
	Warnings []string `json:"warnings"`
}

// Size returns the total size of the staged files.
func (r BackupResult) Size() int64 {
	var n int64
	for _, f := range r.Files {
		n += f.Size
	}
	return n
}

// File returns the staged file with the given name.
func (r BackupResult) File(name string) (StagedFile, bool) {
	for _, f := range r.Files {
		if f.Name == name {
			return f, true
		}
	}
	return StagedFile{}, false
}

// Backup copies Plex's databases and Preferences.xml from o.DataPath into o.StagingDir (ADR
// 0005). Each database is copied with SQLite's online backup API in a single Step(-1):
//
//   - When "<db>-wal" (or a rollback "<db>-journal") exists, Plex is running or crashed: the
//     source is opened "mode=ro" with a busy timeout, which reads the committed WAL content
//     consistently, also from a read-only mount. immutable=1 is never used then.
//   - Otherwise Plex stopped cleanly and the file is self-contained: it is opened
//     "mode=ro&immutable=1" (which creates no -wal/-shm next to it and works on a read-only
//     mount), and its size, mtime and inode are compared before and after the copy. A change or a
//     -wal appearing discards the copy and the copy is retried (up to o.Attempts times).
//
// The blobs database is copied when present. Preferences.xml is read twice and staged only when
// both reads and the file's metadata agree; when it is missing or unreadable (Bunkarr does not
// run as Plex's user) the backup continues with a warning. The copies are written to temp names,
// fsynced and renamed, and hashed (sha256).
//
// Plex's files are never written. On error the files this call staged are removed.
func Backup(ctx context.Context, o BackupOptions) (res BackupResult, err error) {
	if !filepath.IsAbs(o.DataPath) {
		return BackupResult{}, fmt.Errorf("the Plex data path %q is not an absolute path", o.DataPath)
	}
	if o.StagingDir == "" {
		return BackupResult{}, errors.New("no staging directory")
	}
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = DefaultBusyTimeout
	}
	if o.Attempts < 1 {
		o.Attempts = DefaultAttempts
	}
	if o.Log == nil {
		o.Log = func(slog.Level, string, ...any) {}
	}
	src, err := os.OpenRoot(o.DataPath)
	if err != nil {
		return BackupResult{}, fmt.Errorf("open the Plex data path: %w", err)
	}
	defer src.Close()
	if err := os.MkdirAll(o.StagingDir, 0o700); err != nil {
		return BackupResult{}, fmt.Errorf("create the staging directory: %w", err)
	}
	defer func() {
		if err != nil {
			for _, f := range res.Files {
				_ = os.Remove(f.Path)
			}
			res = BackupResult{}
		}
	}()
	res.Warnings = []string{}

	lib, err := backupDB(ctx, src, o, LibraryDB)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && ctx.Err() == nil {
			return res, fmt.Errorf("%w: %s", ErrNoDatabase, filepath.Join(o.DataPath, filepath.FromSlash(DatabasesDir), LibraryDB))
		}
		return res, err
	}
	res.Files = append(res.Files, lib)
	res.Method, res.SQLiteVersion, res.TakenAt = lib.Method, lib.SQLiteVersion, time.Now().UTC()

	_, err = src.Lstat(dbRel(BlobsDB))
	switch {
	case err == nil:
		blobs, err := backupDB(ctx, src, o, BlobsDB)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, blobs)
	case errors.Is(err, fs.ErrNotExist):
		o.Log(slog.LevelInfo, "Plex has no blobs database; skipped", "file", BlobsDB)
	default:
		return res, fmt.Errorf("stat %s: %w", dbRel(BlobsDB), err)
	}

	prefs, err := copyPreferences(ctx, src, o)
	switch {
	case err == nil:
		res.Files = append(res.Files, prefs)
	case ctx.Err() != nil:
		return res, ctx.Err()
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), errors.Is(err, filecopy.ErrNotRegular),
		errors.Is(err, ErrUnstable), errors.Is(err, syscall.ELOOP):
		w := preferencesWarning(err)
		o.Log(slog.LevelWarn, w)
		res.Warnings = append(res.Warnings, w)
	default:
		return res, err
	}
	return res, nil
}

// preferencesWarning explains why Preferences.xml was not backed up.
func preferencesWarning(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "Preferences.xml was not backed up: it does not exist in the Plex data path"
	case errors.Is(err, fs.ErrPermission):
		return "Preferences.xml was not backed up: permission denied (Plex writes it with mode 0600; run Bunkarr with Plex's PUID/PGID)"
	case errors.Is(err, ErrUnstable):
		return "Preferences.xml was not backed up: it kept changing while it was read"
	default:
		return "Preferences.xml was not backed up: it is not a regular file"
	}
}

// dbRel is a database's path relative to the data path.
func dbRel(name string) string { return DatabasesDir + "/" + name }

// backupDB copies database name into the staging directory (see Backup for the rules).
func backupDB(ctx context.Context, src *os.Root, o BackupOptions, name string) (StagedFile, error) {
	rel := dbRel(name)
	dbPath := filepath.Join(o.DataPath, filepath.FromSlash(rel))
	final := filepath.Join(o.StagingDir, name)
	if _, err := os.Lstat(final); err == nil {
		return StagedFile{}, fmt.Errorf("staging: %s already exists", final)
	}
	tmp := final + ".partial"
	var lastErr error
	for attempt := 1; attempt <= o.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return StagedFile{}, err
		}
		if err := removeSQLiteFiles(tmp); err != nil {
			return StagedFile{}, err
		}
		before, err := filecopy.Lstat(src, rel)
		if err != nil {
			return StagedFile{}, fmt.Errorf("stat %s: %w", rel, err)
		}
		if !before.Regular() {
			return StagedFile{}, fmt.Errorf("%s: %w", rel, filecopy.ErrNotRegular)
		}
		live, err := hasLog(src, rel)
		if err != nil {
			return StagedFile{}, err
		}
		method, query, busy := MethodOnlineBackupImmutable, "mode=ro&immutable=1", time.Duration(0)
		if live {
			method, query, busy = MethodOnlineBackup, "mode=ro", o.BusyTimeout
		}
		uri, err := fileURI(dbPath, query)
		if err != nil {
			return StagedFile{}, err
		}
		o.Log(slog.LevelInfo, "Copying the Plex database", "file", name, "method", method, "size", before.Size, "attempt", attempt)
		started := time.Now()
		version, err := onlineBackup(ctx, uri, tmp, busy)
		if o.afterCopy != nil {
			o.afterCopy(name, attempt)
		}
		if err == nil && ctx.Err() != nil {
			// Cancelled while the one Step(-1) ran (SQLite cannot interrupt it): stop now rather
			// than fsync and hash a copy nobody will use.
			err = ctx.Err()
		}
		if err != nil {
			_ = removeSQLiteFiles(tmp)
			if ctx.Err() != nil {
				return StagedFile{}, ctx.Err()
			}
			lastErr = fmt.Errorf("copy %s (%s): %w", rel, method, err)
			o.Log(slog.LevelWarn, "Copying the Plex database failed", "file", name, "attempt", attempt, "error", err.Error())
			continue
		}
		if !live {
			after, aerr := filecopy.Lstat(src, rel)
			liveNow, lerr := hasLog(src, rel)
			if aerr != nil || lerr != nil || liveNow || changed(before, after) {
				_ = removeSQLiteFiles(tmp)
				lastErr = fmt.Errorf("%w: %s changed (or Plex started) during an immutable read", ErrUnstable, rel)
				o.Log(slog.LevelWarn, "The Plex database changed while it was copied; retrying", "file", name, "attempt", attempt)
				continue
			}
		}
		sf, err := finishStaged(tmp, final)
		if err != nil {
			_ = removeSQLiteFiles(tmp)
			return StagedFile{}, err
		}
		sf.Name, sf.Source, sf.Method, sf.Attempts, sf.SQLiteVersion = name, dbPath, method, attempt, version
		o.Log(slog.LevelInfo, "Copied the Plex database", "file", name, "method", method, "size", sf.Size,
			"durationMs", time.Since(started).Milliseconds())
		return sf, nil
	}
	return StagedFile{}, fmt.Errorf("%w (%d attempts): %w", ErrUnstable, o.Attempts, lastErr)
}

// changed reports whether a file's size, mtime or identity differ between two stats.
func changed(a, b filecopy.Stat) bool {
	return a.Size != b.Size || a.MtimeNs != b.MtimeNs || a.Ino != b.Ino || a.Dev != b.Dev || !b.Regular()
}

// hasLog reports whether the database rel has a -wal or a rollback -journal next to it (then it
// is not self-contained and immutable=1 must not be used).
func hasLog(src *os.Root, rel string) (bool, error) {
	for _, suffix := range []string{"-wal", "-journal"} {
		_, err := src.Lstat(rel + suffix)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("stat %s%s: %w", rel, suffix, err)
		}
	}
	return false, nil
}

// onlineBackup copies the database at srcURI into dstPath with one Step(-1) of SQLite's online
// backup API and returns the SQLite version.
func onlineBackup(ctx context.Context, srcURI, dstPath string, busy time.Duration) (version string, err error) {
	db := openDB(srcURI)
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer conn.Close()
	if busy > 0 {
		if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = "+strconv.FormatInt(busy.Milliseconds(), 10)); err != nil {
			return "", fmt.Errorf("set busy_timeout: %w", err)
		}
	}
	if err := conn.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		return "", fmt.Errorf("read the SQLite version: %w", err)
	}
	dstURI, err := fileURI(dstPath, "")
	if err != nil {
		return "", err
	}
	err = conn.Raw(func(dc any) error {
		b, ok := dc.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("the SQLite driver has no online backup API")
		}
		bk, err := b.NewBackup(dstURI)
		if err != nil {
			return fmt.Errorf("start the online backup: %w", err)
		}
		// One step: an incremental backup restarts on every commit of another process and never
		// finishes against a busy Plex (spike 0001).
		more, err := bk.Step(-1)
		if err != nil {
			_ = bk.Finish()
			return fmt.Errorf("online backup: %w", err)
		}
		if more {
			_ = bk.Finish()
			return errors.New("online backup: pages left after a complete step")
		}
		if err := bk.Finish(); err != nil {
			return fmt.Errorf("finish the online backup: %w", err)
		}
		return nil
	})
	return version, err
}

// removeSQLiteFiles removes a staged temp copy and any journal files SQLite left next to it.
func removeSQLiteFiles(p string) error {
	for _, q := range []string{p, p + "-journal", p + "-wal", p + "-shm"} {
		if err := os.Remove(q); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("staging: %w", err)
		}
	}
	return nil
}

// finishStaged makes the temp copy tmp durable under final: SQLite's empty side files are
// removed (a non-empty one means the copy is not self-contained), the copy is fsynced and renamed
// and the directory fsynced; then the copy is hashed.
func finishStaged(tmp, final string) (StagedFile, error) {
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		fi, err := os.Lstat(tmp + suffix)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return StagedFile{}, fmt.Errorf("staging: %w", err)
		}
		if suffix != "-shm" && fi.Size() > 0 {
			return StagedFile{}, fmt.Errorf("staging: the copy left a non-empty %s", filepath.Base(tmp+suffix))
		}
		if err := os.Remove(tmp + suffix); err != nil {
			return StagedFile{}, fmt.Errorf("staging: %w", err)
		}
	}
	if err := syncFile(tmp); err != nil {
		return StagedFile{}, err
	}
	if err := renameNew(tmp, final); err != nil {
		return StagedFile{}, err
	}
	size, sum, err := hashPath(final)
	if err != nil {
		return StagedFile{}, err
	}
	return StagedFile{Path: final, Size: size, SHA256: sum}, nil
}

// syncFile fsyncs the file at p.
func syncFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("staging: %w", err)
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("staging: fsync %s: %w", filepath.Base(p), err)
	}
	return nil
}

// renameNew renames tmp to final (which must not exist) and fsyncs the directory.
func renameNew(tmp, final string) error {
	if _, err := os.Lstat(final); err == nil {
		return fmt.Errorf("staging: %s already exists", final)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("staging: %w", err)
	}
	d, err := os.Open(filepath.Dir(final))
	if err != nil {
		return fmt.Errorf("staging: %w", err)
	}
	err = d.Sync()
	cerr := d.Close()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("staging: fsync directory: %w", err)
	}
	if cerr != nil {
		return fmt.Errorf("staging: %w", cerr)
	}
	return nil
}

// hashPath returns the size and sha256 (hex) of the file at p.
func hashPath(p string) (int64, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, "", fmt.Errorf("hash %s: %w", filepath.Base(p), err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", fmt.Errorf("hash %s: %w", filepath.Base(p), err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// fileIdentity is what two reads of a plain file compare.
type fileIdentity struct {
	size, mtimeNs int64
	dev, ino      uint64
}

func identityOf(fi fs.FileInfo) fileIdentity {
	id := fileIdentity{size: fi.Size(), mtimeNs: fi.ModTime().UnixNano()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id.dev, id.ino = uint64(st.Dev), st.Ino // Dev is int32 on darwin, uint64 on linux
	}
	return id
}

// errReadChanged means a file changed while it was read.
var errReadChanged = errors.New("changed while it was read")

// readSource reads the regular file rel of src (O_RDONLY|O_NOFOLLOW, never a symlink, FIFO or
// device) and returns its content and identity; errReadChanged when its metadata changed during
// the read.
func readSource(src *os.Root, rel string) ([]byte, fileIdentity, error) {
	fi, err := src.Lstat(rel)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fileIdentity{}, fmt.Errorf("%s: %w", rel, filecopy.ErrNotRegular)
	}
	f, err := src.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	defer f.Close()
	fi1, err := f.Stat()
	if err != nil {
		return nil, fileIdentity{}, err
	}
	if !fi1.Mode().IsRegular() {
		return nil, fileIdentity{}, fmt.Errorf("%s: %w", rel, filecopy.ErrNotRegular)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPrefsBytes+1))
	if err != nil {
		return nil, fileIdentity{}, fmt.Errorf("read %s: %w", rel, err)
	}
	if len(data) > maxPrefsBytes {
		return nil, fileIdentity{}, fmt.Errorf("%s is larger than %d bytes", rel, maxPrefsBytes)
	}
	fi2, err := f.Stat()
	if err != nil {
		return nil, fileIdentity{}, err
	}
	id := identityOf(fi1)
	if id != identityOf(fi2) || id.size != int64(len(data)) || id.ino != identityOf(fi).ino {
		return nil, fileIdentity{}, errReadChanged
	}
	return data, id, nil
}

// copyPreferences stages Preferences.xml: it is read twice and staged (0600) only when both reads
// and the file's metadata agree (Plex replaces it atomically, so a change means a new version was
// written in between). The PlexOnlineToken found in it is registered as a secret.
func copyPreferences(ctx context.Context, src *os.Root, o BackupOptions) (StagedFile, error) {
	final := filepath.Join(o.StagingDir, PreferencesXML)
	if _, err := os.Lstat(final); err == nil {
		return StagedFile{}, fmt.Errorf("staging: %s already exists", final)
	}
	for attempt := 1; attempt <= o.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return StagedFile{}, err
		}
		first, id1, err := readSource(src, PreferencesXML)
		if errors.Is(err, errReadChanged) {
			continue
		}
		if err != nil {
			return StagedFile{}, err
		}
		second, id2, err := readSource(src, PreferencesXML)
		if errors.Is(err, errReadChanged) {
			continue
		}
		if err != nil {
			return StagedFile{}, err
		}
		if id1 != id2 || !bytes.Equal(first, second) {
			o.Log(slog.LevelInfo, "Preferences.xml changed while it was read; retrying", "attempt", attempt)
			continue
		}
		registerPlexTokens(first)
		sf, err := stageBytes(final, first)
		if err != nil {
			return StagedFile{}, err
		}
		sf.Name, sf.Source, sf.Method, sf.Attempts = PreferencesXML, filepath.Join(o.DataPath, PreferencesXML), MethodCopy, attempt
		return sf, nil
	}
	return StagedFile{}, fmt.Errorf("%s: %w", PreferencesXML, ErrUnstable)
}

// stageBytes writes data to final (0600) through a temp file, fsynced and renamed.
func stageBytes(final string, data []byte) (StagedFile, error) {
	tmp := final + ".partial"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return StagedFile{}, fmt.Errorf("staging: %w", err)
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return StagedFile{}, fmt.Errorf("staging: %w", err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = renameNew(tmp, final)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return StagedFile{}, fmt.Errorf("staging %s: %w", filepath.Base(final), err)
	}
	sum := sha256.Sum256(data)
	return StagedFile{Path: final, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, nil
}

// plexTokenRe finds the PlexOnlineToken attribute of Preferences.xml.
var plexTokenRe = regexp.MustCompile(`PlexOnlineToken\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// registerPlexTokens registers every PlexOnlineToken value in prefs with logging.RegisterSecret.
func registerPlexTokens(prefs []byte) {
	for _, m := range plexTokenRe.FindAllSubmatch(prefs, -1) {
		logging.RegisterSecret(string(m[1]) + string(m[2]))
	}
}

// SourceFile describes one of Plex's files as a dry run reports it (Inspect).
type SourceFile struct {
	// Name is LibraryDB, BlobsDB or PreferencesXML.
	Name string `json:"name"`
	// Rel is the path relative to the data path; Path the full path.
	Rel  string `json:"rel"`
	Path string `json:"path"`
	// Present reports whether the file exists; Size is its size.
	Present bool  `json:"present"`
	Size    int64 `json:"size"`
	// WALPresent and WALSize describe "<db>-wal" (databases only): present while Plex runs or
	// after it crashed; the copy then includes the committed transactions in it.
	WALPresent bool  `json:"walPresent"`
	WALSize    int64 `json:"walSize"`
	// SHMPresent and JournalPresent report "<db>-shm" and a rollback "<db>-journal".
	SHMPresent     bool `json:"shmPresent"`
	JournalPresent bool `json:"journalPresent"`
	// Method is how Backup would copy the file now ("" when it is not present).
	Method string `json:"method,omitempty"`
	// Readable reports whether Bunkarr can open the file for reading.
	Readable bool `json:"readable"`
	// Problem explains why the file would not be backed up ("" when it would).
	Problem string `json:"problem,omitempty"`
}

// Inspect reports, without reading any content or writing anything, what Backup would copy from
// dataPath now: the library database, the blobs database and Preferences.xml, each with its
// size, its WAL state and the method. It fails only when the data path cannot be opened.
func Inspect(dataPath string) ([]SourceFile, error) {
	if !filepath.IsAbs(dataPath) {
		return nil, fmt.Errorf("the Plex data path %q is not an absolute path", dataPath)
	}
	src, err := os.OpenRoot(dataPath)
	if err != nil {
		return nil, fmt.Errorf("open the Plex data path: %w", err)
	}
	defer src.Close()
	out := make([]SourceFile, 0, 3)
	for _, name := range []string{LibraryDB, BlobsDB} {
		rel := dbRel(name)
		sf := inspectFile(src, dataPath, name, rel)
		if sf.Present {
			if fi, err := src.Lstat(rel + "-wal"); err == nil {
				sf.WALPresent, sf.WALSize = true, fi.Size()
			}
			_, err := src.Lstat(rel + "-shm")
			sf.SHMPresent = err == nil
			_, err = src.Lstat(rel + "-journal")
			sf.JournalPresent = err == nil
			sf.Method = MethodOnlineBackupImmutable
			if sf.WALPresent || sf.JournalPresent {
				sf.Method = MethodOnlineBackup
			}
		}
		out = append(out, sf)
	}
	prefs := inspectFile(src, dataPath, PreferencesXML, PreferencesXML)
	if prefs.Present {
		prefs.Method = MethodCopy
	}
	return append(out, prefs), nil
}

// inspectFile stats and test-opens one file for Inspect.
func inspectFile(src *os.Root, dataPath, name, rel string) SourceFile {
	sf := SourceFile{Name: name, Rel: rel, Path: filepath.Join(dataPath, filepath.FromSlash(rel))}
	fi, err := src.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		sf.Problem = "not found"
		return sf
	}
	if err != nil {
		sf.Problem = err.Error()
		return sf
	}
	sf.Present, sf.Size = true, fi.Size()
	if !fi.Mode().IsRegular() {
		sf.Problem = "not a regular file"
		return sf
	}
	f, err := src.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			sf.Problem = "permission denied (run Bunkarr with Plex's PUID/PGID)"
		} else {
			sf.Problem = err.Error()
		}
		return sf
	}
	_ = f.Close()
	sf.Readable = true
	return sf
}
