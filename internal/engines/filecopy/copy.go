package filecopy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"time"
	"unicode/utf8"

	"github.com/sl0wz3r/bunkarr/internal/faultinject"
)

// CopyOptions configures WriteTemp.
type CopyOptions struct {
	// Hash computes the sha256 of the copied bytes (Temp.Hash). Set it when verification is on.
	Hash bool
	// OnTemp is called with the temp file's path BEFORE the temp file is created, so the caller
	// can persist it (item detail) and a resumed job can remove it (design S7). An error aborts
	// the copy before anything is written.
	OnTemp func(tempRel string) error
	// Progress is called after each chunk with the number of bytes written.
	Progress func(delta int64)
	// DirPerm is the mode of directories created for the destination path (default
	// DefaultDirPerm); FilePerm the mode of the temp file (default DefaultFilePerm). The process
	// umask applies to both.
	DirPerm, FilePerm fs.FileMode
	// WrapWriter, when set, wraps the temp file's writer (bandwidth limiting, fault simulation in
	// tests). The wrapper must write through to the writer it is given.
	WrapWriter func(w io.Writer) io.Writer
}

// Temp is a completely written, fsynced temp file ready for Commit.
type Temp struct {
	// Rel is the temp file's path relative to the destination root.
	Rel string
	// Size is the number of bytes copied; it equals the source's size before and after the copy.
	Size int64
	// MtimeNs is the source's mtime, which the temp file now carries.
	MtimeNs int64
	// Hash is "sha256:<hex>" of the copied bytes when CopyOptions.Hash was set, else "".
	Hash string
}

// WriteTemp copies the regular file srcRel of src into a new temp file next to dstRel in dst
// (design S7): it creates dstRel's parent directories, reports the temp path through OnTemp,
// creates "<dir>/.bunkarr-tmp-<base>-<random>" exclusively, copies (hashing when asked, stopping
// promptly when ctx is cancelled), fsyncs, checks that the source's size and mtime did not change
// during the copy and that the bytes copied equal its size, and sets the temp file's mtime (and
// atime) to the source's.
//
// On any error the temp file is removed and the error is returned (ctx.Err() on cancellation,
// ErrSourceChanged when the source changed, *Error otherwise). dstRel itself is never touched:
// Commit renames the temp file into place.
func WriteTemp(ctx context.Context, src *os.Root, srcRel string, dst *os.Root, dstRel string, o CopyOptions) (t Temp, err error) {
	if err := checkRel(srcRel); err != nil {
		return Temp{}, srcErr("copy", srcRel, err)
	}
	if err := checkWritable(dstRel); err != nil {
		return Temp{}, dstErr("copy", dstRel, err)
	}
	if err := ctx.Err(); err != nil {
		return Temp{}, err
	}
	dirPerm, filePerm := o.DirPerm, o.FilePerm
	if dirPerm == 0 {
		dirPerm = DefaultDirPerm
	}
	if filePerm == 0 {
		filePerm = DefaultFilePerm
	}

	in, before, err := openRegular(src, srcRel, SideSource)
	if err != nil {
		return Temp{}, err
	}
	defer in.Close()

	dir := parentDir(dstRel)
	if dir != "." {
		if err := dst.MkdirAll(dir, dirPerm); err != nil {
			return Temp{}, dstErr("mkdir", dir, err)
		}
	}
	tempRel := path.Join(dir, tempName(path.Base(dstRel)))
	if o.OnTemp != nil {
		if err := o.OnTemp(tempRel); err != nil {
			return Temp{}, fmt.Errorf("record temp path %s: %w", tempRel, err)
		}
	}
	faultinject.Point("copy.beforeTemp")
	out, err := dst.OpenFile(tempRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return Temp{}, dstErr("create", tempRel, err)
	}
	outClosed := false
	defer func() {
		if !outClosed {
			_ = out.Close()
		}
		// Remove the temp file on failure. A panic (a crash-matrix fault point) leaves err nil and
		// the temp file in place, as a real crash would.
		if err != nil {
			_ = dst.Remove(tempRel)
		}
	}()

	var h hash.Hash
	if o.Hash {
		h = sha256.New()
	}
	var w io.Writer = out
	if o.WrapWriter != nil {
		w = o.WrapWriter(out)
	}
	n, err := copyLoop(ctx, in, w, h, before.Size, o.Progress, srcRel, tempRel)
	if err != nil {
		return Temp{}, err
	}
	faultinject.Point("copy.afterWrite")

	if err = out.Sync(); err != nil {
		return Temp{}, dstErr("fsync", tempRel, err)
	}
	faultinject.Point("copy.afterSync")

	fi, err := in.Stat()
	if err != nil {
		return Temp{}, srcErr("stat", srcRel, err)
	}
	after := statOf(fi)
	if after.Size != before.Size || after.MtimeNs != before.MtimeNs {
		err = srcErr("copy", srcRel, fmt.Errorf("%w (size %d→%d, mtime %d→%d)", ErrSourceChanged, before.Size, after.Size, before.MtimeNs, after.MtimeNs))
		return Temp{}, err
	}
	if n != before.Size {
		err = srcErr("copy", srcRel, fmt.Errorf("%w: read %d bytes of %d", ErrSourceChanged, n, before.Size))
		return Temp{}, err
	}

	err = out.Close()
	outClosed = true
	if err != nil {
		return Temp{}, dstErr("close", tempRel, err)
	}
	// Times are set after close: some SMB servers update the modification time when a written
	// handle is closed.
	atime := before.atime
	if atime.IsZero() {
		atime = time.Unix(0, before.MtimeNs)
	}
	if err = dst.Chtimes(tempRel, atime, time.Unix(0, before.MtimeNs)); err != nil {
		return Temp{}, dstErr("chtimes", tempRel, err)
	}
	faultinject.Point("copy.afterChtimes")

	got, err := Lstat(dst, tempRel)
	if err != nil {
		return Temp{}, dstErr("stat", tempRel, err)
	}
	if got.Size != n {
		err = dstErr("check", tempRel, fmt.Errorf("%w: temp file has %d bytes, %d were written", ErrMismatch, got.Size, n))
		return Temp{}, err
	}
	t = Temp{Rel: tempRel, Size: n, MtimeNs: before.MtimeNs}
	if h != nil {
		t.Hash = HashPrefix + hex.EncodeToString(h.Sum(nil))
	}
	return t, nil
}

// copyLoop copies in to w, hashing into h when not nil. It stops early when the source grows
// beyond size (a file still being written).
func copyLoop(ctx context.Context, in io.Reader, w io.Writer, h hash.Hash, size int64, progress func(int64), srcRel, tempRel string) (int64, error) {
	buf := make([]byte, copyBufSize)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		nr, rerr := in.Read(buf)
		if nr > 0 {
			nw, werr := w.Write(buf[:nr])
			if werr == nil && nw != nr {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				return n, dstErr("write", tempRel, werr)
			}
			if h != nil {
				h.Write(buf[:nr])
			}
			n += int64(nr)
			if progress != nil {
				progress(int64(nr))
			}
			if n > size {
				return n, srcErr("copy", srcRel, fmt.Errorf("%w: grew beyond %d bytes", ErrSourceChanged, size))
			}
		}
		if errors.Is(rerr, io.EOF) {
			return n, nil
		}
		if rerr != nil {
			return n, srcErr("read", srcRel, rerr)
		}
	}
}

// openRegular opens rel read-only after an lstat that requires a regular file, and checks that
// the opened file is the one the lstat saw (same dev/inode), so a symlink swapped in between is
// never followed (S1).
func openRegular(root *os.Root, rel string, side Side) (*os.File, Stat, error) {
	wrap := func(op string, err error) error {
		if side == SideSource {
			return srcErr(op, rel, err)
		}
		return dstErr(op, rel, err)
	}
	if err := checkRel(rel); err != nil {
		return nil, Stat{}, wrap("open", err)
	}
	fi, err := root.Lstat(rel)
	if err != nil {
		return nil, Stat{}, wrap("lstat", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, Stat{}, wrap("open", fmt.Errorf("%w (%s)", ErrNotRegular, fi.Mode().Type()))
	}
	pre := statOf(fi)
	f, err := root.OpenFile(rel, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return nil, Stat{}, wrap("open", err)
	}
	ffi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Stat{}, wrap("stat", err)
	}
	st := statOf(ffi)
	if !ffi.Mode().IsRegular() || st.Dev != pre.Dev || st.Ino != pre.Ino {
		_ = f.Close()
		return nil, Stat{}, wrap("open", fmt.Errorf("%w: replaced while opening", ErrSourceChanged))
	}
	return f, st, nil
}

// maxTempBase keeps temp names within the 255-byte name limit:
// TempPrefix + base + "-" + 12 hex digits.
const maxTempBase = 255 - len(TempPrefix) - 1 - 12

// tempName returns a new temp file base name for a file named base.
func tempName(base string) string {
	var b [6]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (it aborts the program instead)
	return TempPrefix + truncateUTF8(base, maxTempBase) + "-" + hex.EncodeToString(b[:])
}

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Commit renames the temp file tempRel (made by WriteTemp) to finalRel and fsyncs the directory.
// With noReplace the rename fails with ErrExists when finalRel exists: renameat2
// RENAME_NOREPLACE on Linux (renameatx_np RENAME_EXCL on macOS) where the filesystem supports
// it, else an lstat of finalRel followed by rename(2) — NFS, for one, has no no-replace rename;
// the gap between the two is only a risk if something else writes the same path at the same
// moment, which S2 rules out for Bunkarr's own tree. Without noReplace an existing file at
// finalRel is replaced atomically (an update whose old version was already hardlinked into
// retention).
func Commit(dst *os.Root, tempRel, finalRel string, noReplace bool) error {
	if err := checkRel(tempRel); err != nil {
		return dstErr("commit", tempRel, err)
	}
	if !IsTempName(path.Base(tempRel)) {
		return dstErr("commit", tempRel, ErrNotTemp)
	}
	if err := checkWritable(finalRel); err != nil {
		return dstErr("commit", finalRel, err)
	}
	fi, err := dst.Lstat(tempRel)
	if err != nil {
		return dstErr("commit", tempRel, err)
	}
	if !fi.Mode().IsRegular() {
		return dstErr("commit", tempRel, ErrNotRegular)
	}
	if err := rename(dst, tempRel, finalRel, noReplace); err != nil {
		return err
	}
	faultinject.Point("copy.afterRename")
	return syncDirs(dst, tempRel, finalRel)
}

// rename renames within dst, optionally refusing to replace newRel.
func rename(dst *os.Root, oldRel, newRel string, noReplace bool) error {
	if !noReplace {
		if err := dst.Rename(oldRel, newRel); err != nil {
			return dstErr("rename", oldRel+" → "+newRel, err)
		}
		return nil
	}
	oldDir, err := dst.Open(parentDir(oldRel))
	if err != nil {
		return dstErr("rename", oldRel, err)
	}
	defer oldDir.Close()
	newDir, err := dst.Open(parentDir(newRel))
	if err != nil {
		return dstErr("rename", newRel, err)
	}
	defer newDir.Close()
	supported, err := renameNoReplaceAt(oldDir, path.Base(oldRel), newDir, path.Base(newRel))
	if supported {
		if errors.Is(err, fs.ErrExist) {
			return dstErr("rename", newRel, ErrExists)
		}
		if err != nil {
			return dstErr("rename", oldRel+" → "+newRel, err)
		}
		return nil
	}
	return renameCheckFirst(dst, oldRel, newRel)
}

// renameCheckFirst is the no-replace rename for filesystems without renameat2 RENAME_NOREPLACE
// (NFS): lstat newRel, then rename(2).
func renameCheckFirst(dst *os.Root, oldRel, newRel string) error {
	if _, err := dst.Lstat(newRel); err == nil {
		return dstErr("rename", newRel, ErrExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return dstErr("rename", newRel, err)
	}
	if err := dst.Rename(oldRel, newRel); err != nil {
		return dstErr("rename", oldRel+" → "+newRel, err)
	}
	return nil
}

// syncDirs fsyncs the directories of a and b (once when they are the same).
func syncDirs(dst *os.Root, a, b string) error {
	da, db := parentDir(a), parentDir(b)
	if err := syncDir(dst, db); err != nil {
		return dstErr("fsync", db, err)
	}
	if da != db {
		if err := syncDir(dst, da); err != nil {
			return dstErr("fsync", da, err)
		}
	}
	return nil
}

// VerifyFile re-reads rel and checks it: a regular file of wantSize bytes whose sha256 is
// wantHash (when wantHash is not ""). The page cache is dropped before (and after) reading
// where the OS allows it, so the bytes come from the storage rather than from memory (design
// §4.5). It returns the hash it computed, also on a mismatch (a missing file returns an error
// matching fs.ErrNotExist; a short, long or different file one matching ErrMismatch). progress,
// when not nil, is called with the bytes read.
func VerifyFile(ctx context.Context, root *os.Root, rel string, wantSize int64, wantHash string, progress func(delta int64)) (string, error) {
	f, st, err := openRegular(root, rel, SideDestination)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if st.Size != wantSize {
		return "", dstErr("verify", rel, fmt.Errorf("%w: size %d, want %d", ErrMismatch, st.Size, wantSize))
	}
	dropCache(f)
	got, n, err := hashReader(ctx, f, progress, rel, SideDestination)
	dropCache(f)
	if err != nil {
		return "", err
	}
	if n != wantSize {
		return got, dstErr("verify", rel, fmt.Errorf("%w: read %d bytes, want %d", ErrMismatch, n, wantSize))
	}
	if wantHash != "" && got != wantHash {
		return got, dstErr("verify", rel, fmt.Errorf("%w: hash %s, want %s", ErrMismatch, got, wantHash))
	}
	return got, nil
}

// VerifyTemp re-reads a temp file written by WriteTemp (with CopyOptions.Hash) and checks its
// size and hash (design S7 "optional re-read verification").
func VerifyTemp(ctx context.Context, dst *os.Root, t Temp) error {
	_, err := VerifyFile(ctx, dst, t.Rel, t.Size, t.Hash, nil)
	return err
}

// HashFile returns "sha256:<hex>" of the regular file rel and the number of bytes read. It does
// not drop the page cache (use VerifyFile for that). Errors carry no side: classify them by
// errno.
func HashFile(ctx context.Context, root *os.Root, rel string, progress func(delta int64)) (string, int64, error) {
	f, _, err := openRegular(root, rel, "")
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return hashReader(ctx, f, progress, rel, "")
}

func hashReader(ctx context.Context, r io.Reader, progress func(int64), rel string, side Side) (string, int64, error) {
	h := sha256.New()
	buf := make([]byte, copyBufSize)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return "", n, err
		}
		nr, err := r.Read(buf)
		if nr > 0 {
			h.Write(buf[:nr])
			n += int64(nr)
			if progress != nil {
				progress(int64(nr))
			}
		}
		if errors.Is(err, io.EOF) {
			return HashPrefix + hex.EncodeToString(h.Sum(nil)), n, nil
		}
		if err != nil {
			return "", n, &Error{Side: side, Op: "read", Path: rel, Err: bare(err)}
		}
	}
}

// HeadTailHash returns "headtail-sha256:<hex>": the sha256 of the first HeadTailBytes of the
// regular file rel followed by its last HeadTailBytes (the two ranges never overlap, so a file
// of up to 2 MiB is hashed whole). Used to confirm a rename (design §4.2 move) and hardlink
// groups on FUSE sources (§4.3) cheaply. Errors carry no side.
func HeadTailHash(root *os.Root, rel string) (string, error) {
	f, st, err := openRegular(root, rel, "")
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	headLen := min(st.Size, HeadTailBytes)
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, headLen)); err != nil {
		return "", &Error{Op: "read", Path: rel, Err: bare(err)}
	}
	if st.Size > HeadTailBytes {
		tailOff := max(int64(HeadTailBytes), st.Size-HeadTailBytes)
		if _, err := io.Copy(h, io.NewSectionReader(f, tailOff, st.Size-tailOff)); err != nil {
			return "", &Error{Op: "read", Path: rel, Err: bare(err)}
		}
	}
	return HeadTailPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// CleanupTemp removes the temp file tempRel. Only regular files whose base name starts with
// TempPrefix are removed; a temp file that is already gone is not an error (resume, cancel).
func CleanupTemp(dst *os.Root, tempRel string) error {
	if err := checkRel(tempRel); err != nil {
		return dstErr("cleanup", tempRel, err)
	}
	if !IsTempName(path.Base(tempRel)) {
		return dstErr("cleanup", tempRel, ErrNotTemp)
	}
	fi, err := dst.Lstat(tempRel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return dstErr("cleanup", tempRel, err)
	}
	if !fi.Mode().IsRegular() {
		return dstErr("cleanup", tempRel, ErrNotRegular)
	}
	if err := dst.Remove(tempRel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return dstErr("cleanup", tempRel, err)
	}
	return nil
}

// WriteFileAtomic writes a small file (the destination marker, a Plex backup manifest) through a
// temp file: parent directories are created, the data is written and fsynced, then renamed to rel
// (refusing an existing rel when noReplace) and the directory is fsynced. Unlike Commit it may
// write Bunkarr's marker.
func WriteFileAtomic(dst *os.Root, rel string, data []byte, perm fs.FileMode, noReplace bool) (err error) {
	if err := checkRel(rel); err != nil {
		return dstErr("write", rel, err)
	}
	if rel == MetaDir {
		return dstErr("write", rel, ErrInvalidPath)
	}
	if perm == 0 {
		perm = DefaultFilePerm
	}
	dir := parentDir(rel)
	if dir != "." {
		if err := dst.MkdirAll(dir, DefaultDirPerm); err != nil {
			return dstErr("mkdir", dir, err)
		}
	}
	tempRel := path.Join(dir, tempName(path.Base(rel)))
	f, err := dst.OpenFile(tempRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return dstErr("create", tempRel, err)
	}
	defer func() {
		if err != nil {
			_ = dst.Remove(tempRel)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return dstErr("write", tempRel, err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return dstErr("fsync", tempRel, err)
	}
	if err = f.Close(); err != nil {
		return dstErr("close", tempRel, err)
	}
	if err = rename(dst, tempRel, rel, noReplace); err != nil {
		return err
	}
	return syncDirs(dst, tempRel, rel)
}
