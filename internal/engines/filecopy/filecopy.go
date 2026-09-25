// Package filecopy holds the filesystem primitives of Bunkarr's "filecopy" engine (design
// docs/design/phase1.md §2, §4): atomic, verified copies into a mounted destination (safety rule
// S7), hardlinks, same-filesystem moves, retention and expiry (S5), adoption checks (§4.4), full
// and head/tail hashes, page-cache dropping for verification (§4.5), temp-file cleanup, free
// space and name checks (S11).
//
// Every function works on an *os.Root, never on a raw path, so no destination operation can
// escape the destination target (S2), and source files are opened read-only after an lstat that
// refuses symlinks and other non-regular files (S1). The package keeps no state and knows nothing
// about the database or jobs: callers record what they do.
//
// Paths ("rel") are slash-separated and relative to the root, as stored in the database. They
// must be local and clean: no "..", no absolute paths, no empty or "." components.
//
// Fault-injection points (internal/faultinject), in order of a copy: "copy.beforeTemp",
// "copy.afterWrite", "copy.afterSync", "copy.afterChtimes", "copy.afterRename"; and
// "move.afterRename" (Move, RenameDir), "link.afterLink", "expire.afterRemove".
package filecopy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
)

const (
	// MetaDir is Bunkarr's own directory at the top of a destination.
	MetaDir = ".bunkarr"
	// MarkerRel is the destination marker file (safety rule S3).
	MarkerRel = MetaDir + "/destination.json"
	// ProbeDir is where capability probes run (design §3).
	ProbeDir = MetaDir + "/probe"
	// RetentionRoot holds retained files: RetentionRoot/<queuedAt>-job<id>/<destFolder>/<relPath>.
	RetentionRoot = MetaDir + "/retention"
	// TempPrefix starts the base name of every temp file Bunkarr creates.
	TempPrefix = ".bunkarr-tmp-"
	// DefaultDirPerm is the mode of directories Bunkarr creates; the process umask applies (the
	// container sets it from UMASK).
	DefaultDirPerm fs.FileMode = 0o777
	// DefaultFilePerm is the mode of files Bunkarr creates; the process umask applies.
	DefaultFilePerm fs.FileMode = 0o666
	// HeadTailBytes is the size of each end HeadTailHash reads.
	HeadTailBytes = 1 << 20
	// HashPrefix starts every full-content hash ("sha256:<hex>").
	HashPrefix = "sha256:"
	// HeadTailPrefix starts every head/tail hash, so one is never mistaken for a content hash.
	HeadTailPrefix = "headtail-sha256:"

	// copyBufSize is the read/write buffer of a copy.
	copyBufSize = 1 << 20
	// noFollow makes an open fail on a final symlink (os.Root adds it too, but then resolves
	// links that stay inside the root; openRegular's lstat/inode check refuses those).
	noFollow = syscall.O_NOFOLLOW
)

var (
	// ErrSourceChanged means the source file changed while it was copied ("changed during copy":
	// the item fails and is retried by the next run).
	ErrSourceChanged = errors.New("source changed during copy")
	// ErrNotRegular means a path is a directory, symlink, device, FIFO or socket where a regular
	// file is required.
	ErrNotRegular = errors.New("not a regular file")
	// ErrExists means the target of a no-replace operation already exists.
	ErrExists = errors.New("target already exists")
	// ErrMismatch means a file's size or content is not the expected one.
	ErrMismatch = errors.New("size or content does not match")
	// ErrInvalidPath means a path is not a clean, local, relative path or names Bunkarr's marker.
	ErrInvalidPath = errors.New("invalid path")
	// ErrNotTemp means a path that must be a Bunkarr temp file does not have the temp prefix.
	ErrNotTemp = errors.New("not a Bunkarr temp file")
	// ErrOutsideRetention means an expiry was asked for a path outside .bunkarr/retention.
	ErrOutsideRetention = errors.New("path is not inside " + RetentionRoot)
)

// Side names the tree an error happened in.
type Side string

// Sides.
const (
	SideSource      Side = "source"
	SideDestination Side = "destination"
)

// Error is a filesystem error annotated with the side and operation. Err is the underlying
// cause (usually a syscall.Errno or one of this package's sentinel errors), so errors.Is works.
type Error struct {
	Side Side
	Op   string
	Path string
	Err  error
}

// Error implements error.
func (e *Error) Error() string {
	if e.Side == "" {
		return fmt.Sprintf("%s %s: %v", e.Op, e.Path, e.Err)
	}
	return fmt.Sprintf("%s %s %s: %v", e.Side, e.Op, e.Path, e.Err)
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// bare strips *fs.PathError and *os.LinkError wrappers (their messages repeat absolute paths the
// Error already names relative to the root).
func bare(err error) error {
	for {
		switch e := err.(type) {
		case *fs.PathError:
			err = e.Err
		case *os.LinkError:
			err = e.Err
		default:
			return err
		}
	}
}

func srcErr(op, rel string, err error) error {
	return &Error{Side: SideSource, Op: op, Path: rel, Err: bare(err)}
}

func dstErr(op, rel string, err error) error {
	return &Error{Side: SideDestination, Op: op, Path: rel, Err: bare(err)}
}

// Class is how a job treats an error (design §6.1).
type Class int

// Error classes. The zero Class is returned for a nil error.
const (
	// Item errors concern one file: the item fails with a warning and the job continues.
	Item Class = iota + 1
	// Fatal errors concern the whole destination or mount (or the job was cancelled): the job
	// stops and fails.
	Fatal
)

// String implements fmt.Stringer.
func (c Class) String() string {
	switch c {
	case Item:
		return "item"
	case Fatal:
		return "fatal"
	default:
		return "none"
	}
}

// fatalErrnos are errors that mean the storage or its connection is gone or full, on either side.
var fatalErrnos = []error{
	syscall.ENOSPC, syscall.EDQUOT, syscall.EIO, syscall.ENOTCONN, syscall.ESTALE, syscall.EROFS,
	syscall.EHOSTDOWN, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.ENETUNREACH, syscall.ETIMEDOUT,
}

// Classify says whether err stops the job (Fatal) or only fails one item (Item):
//   - Fatal: ENOSPC, EDQUOT, EIO, ENOTCONN, ESTALE, EROFS, EHOSTDOWN and the other "storage or
//     network gone" errors on either side; permission denied (EACCES/EPERM) on the destination
//     side (Bunkarr cannot write there, so every item would fail the same way); a cancelled or
//     expired context.
//   - Item: everything else, notably a vanished (ENOENT) or unreadable (EACCES) source file,
//     ErrSourceChanged, ErrNotRegular, ErrExists, ErrMismatch and names the destination rejects.
//
// Errors returned by this package carry their side (*Error); errors without one are classified
// by errno alone.
func Classify(err error) Class {
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Fatal
	}
	for _, e := range fatalErrnos {
		if errors.Is(err, e) {
			return Fatal
		}
	}
	var fe *Error
	if errors.As(err, &fe) && fe.Side == SideDestination && errors.Is(err, fs.ErrPermission) {
		return Fatal
	}
	return Item
}

// checkRel validates a root-relative path.
func checkRel(rel string) error {
	if rel == "" || rel == "." || path.IsAbs(rel) || path.Clean(rel) != rel || strings.ContainsRune(rel, 0) ||
		rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	return nil
}

// checkWritable validates a path this package may create, rename or remove: a valid path that is
// not Bunkarr's metadata directory or marker.
func checkWritable(rel string) error {
	if err := checkRel(rel); err != nil {
		return err
	}
	if rel == MetaDir || rel == MarkerRel {
		return fmt.Errorf("%w: %q is reserved", ErrInvalidPath, rel)
	}
	return nil
}

// IsTempName reports whether a base name is a Bunkarr temp file name.
func IsTempName(base string) bool { return strings.HasPrefix(base, TempPrefix) }

// Stat is the part of a file's metadata the engine compares.
type Stat struct {
	Size    int64
	MtimeNs int64
	Mode    fs.FileMode
	Dev     uint64
	Ino     uint64
	Nlink   uint64
	atime   time.Time
}

// Regular reports whether the file is a regular file.
func (s Stat) Regular() bool { return s.Mode.IsRegular() }

func statOf(fi fs.FileInfo) Stat {
	s := Stat{Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano(), Mode: fi.Mode()}
	if dev, ino, nlink, atime, ok := sysStat(fi); ok {
		s.Dev, s.Ino, s.Nlink, s.atime = dev, ino, nlink, atime
	}
	return s
}

// Lstat returns rel's metadata without following a final symlink.
func Lstat(root *os.Root, rel string) (Stat, error) {
	if err := checkRel(rel); err != nil {
		return Stat{}, err
	}
	fi, err := root.Lstat(rel)
	if err != nil {
		return Stat{}, err
	}
	return statOf(fi), nil
}

// RootStat returns the metadata of the root directory itself (its Dev is the root's st_dev).
func RootStat(root *os.Root) (Stat, error) {
	fi, err := root.Stat(".")
	if err != nil {
		return Stat{}, err
	}
	return statOf(fi), nil
}

// FSStat is what statfs says about the filesystem holding a root.
type FSStat struct {
	// Type is the filesystem type: on Linux the name of the statfs f_type magic ("cifs",
	// "smb2", "nfs", "ext4", "xfs", "btrfs", "zfs", "tmpfs", "overlay", "fuse", "ramfs", ...,
	// or "0x<hex>" for an unknown magic); on macOS the kernel's name ("apfs", "smbfs", ...).
	Type string
	// Free is the space available to an unprivileged user, in bytes.
	Free uint64
	// Total is the filesystem size in bytes.
	Total uint64
}

// StatFS runs fstatfs on the root's own directory (opened through the root, so it describes the
// filesystem the root's descriptor is on, even if something was mounted over the path since).
func StatFS(root *os.Root) (FSStat, error) {
	f, err := root.Open(".")
	if err != nil {
		return FSStat{}, fmt.Errorf("statfs: %w", err)
	}
	defer f.Close()
	st, err := statfsFile(f)
	if err != nil {
		return FSStat{}, fmt.Errorf("statfs %s: %w", root.Name(), err)
	}
	return st, nil
}

// FreeSpace returns the free (available to Bunkarr) and total bytes of the root's filesystem.
func FreeSpace(root *os.Root) (free, total uint64, err error) {
	st, err := StatFS(root)
	if err != nil {
		return 0, 0, err
	}
	return st.Free, st.Total, nil
}

// FSType returns the filesystem type of the root (see FSStat.Type).
func FSType(root *os.Root) (string, error) {
	st, err := StatFS(root)
	if err != nil {
		return "", err
	}
	return st.Type, nil
}

// syncDir fsyncs a directory inside root ("." for the root itself). Filesystems that do not
// support fsync on directories (EINVAL, ENOTSUP) are accepted: the rename is then as durable as
// the filesystem makes it.
func syncDir(root *os.Root, dir string) error {
	f, err := root.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	cerr := f.Close()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
		err = nil
	}
	if err != nil {
		return err
	}
	return cerr
}

// withFD runs fn with f's descriptor, keeping f alive and in its current (blocking) mode.
func withFD(f *os.File, fn func(fd int) error) error {
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := sc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

// withFD2 runs fn with the descriptors of a and b.
func withFD2(a, b *os.File, fn func(afd, bfd int) error) error {
	return withFD(a, func(afd int) error {
		return withFD(b, func(bfd int) error { return fn(afd, bfd) })
	})
}

// ignoringEINTR retries fn while it fails with EINTR.
func ignoringEINTR(fn func() error) error {
	for {
		err := fn()
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// parentDir returns the directory of rel ("." at the top level).
func parentDir(rel string) string { return path.Dir(rel) }
