package arrbackup

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/plexdb"
)

// checkSpaceMargin is the free space kept in the staging filesystem beyond the database entry's
// size before it is extracted (design §10 step 6). A variable so tests can lower it.
var checkSpaceMargin uint64 = 1 << 30

// maxProblems bounds ZipReport.Problems.
const maxProblems = 20

// ZipEntry is one entry of a backup zip: its name, uncompressed size and CRC-32 (8 lowercase hex
// digits), as the zip declares them.
type ZipEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	CRC32 string `json:"crc32"`
}

// ZipReport is the result of VerifyZip.
type ZipReport struct {
	// Entries are the zip's entries when its structure passed the checks made before any entry
	// is read; nil otherwise.
	Entries []ZipEntry
	// Zip is IntegrityOK when the structure, the entry names and sizes and every CRC passed.
	Zip string
	// Database is IntegrityOK when <app>.db passed quick_check, IntegrityFailed when it did not,
	// IntegrityNotChecked when the zip failed before the database could be checked.
	Database string
	// Problems say what failed, at most 20 lines. They name entries and checks, never content.
	Problems []string
}

// Integrity is the report as a manifest's integrity object.
func (r ZipReport) Integrity() ManifestIntegrity {
	return ManifestIntegrity{Zip: r.Zip, Database: r.Database}
}

// OK reports whether both checks passed.
func (r ZipReport) OK() bool { return r.Integrity().Result() == IntegrityOK }

func (r *ZipReport) problem(format string, args ...any) {
	if len(r.Problems) < maxProblems {
		r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
	}
}

// failZip marks the zip (and so the database, not checked) as failed.
func (r *ZipReport) failZip(format string, args ...any) ZipReport {
	r.Zip = IntegrityFailed
	if r.Database == "" {
		r.Database = IntegrityNotChecked
	}
	r.problem(format, args...)
	return *r
}

// DatabaseName is the name of an *arr's database inside its backup zip: "<app>.db" with app the
// integration type ("radarr" → "radarr.db").
func DatabaseName(app string) string {
	return strings.ToLower(app) + ".db"
}

// VerifyZip checks the backup zip at zipPath of an *arr of type app ("radarr") without trusting
// it (design §10 step 6). Before any entry is read:
//
//   - the end-of-central-directory record may announce at most MaxEntries entries (so a zip that
//     claims 10^5 entries is refused before its directory is parsed), and the parsed directory
//     may hold at most MaxEntries entries, each named with at most MaxEntryNameBytes bytes, with
//     no name twice;
//   - the declared uncompressed sizes may add up to at most MaxExpansion times the zip's size (or
//     ExpansionAllowance, when that is more) and to at most MaxUncompressedBytes;
//   - config.xml and <app>.db must be entries at the top level;
//   - the staging filesystem (workDir's) must have the database's size plus 1 GiB free.
//
// Then every entry is read as a stream and its CRC checked (archive/zip also refuses an entry
// that yields more or fewer bytes than it declares). Only <app>.db is extracted, to
// <workDir>/check.db (created O_CREATE|O_EXCL|O_NOFOLLOW, mode 0600, with exactly the declared
// number of bytes; more data fails the check), and checked with plexdb.QuickCheck. check.db is
// removed before VerifyZip returns.
//
// Problems of the zip are reported, not returned: the error is for a zip that cannot be opened
// at all, for a workDir that cannot be used, and for cancellation (ctx.Err()).
func VerifyZip(ctx context.Context, zipPath, app, workDir string) (ZipReport, error) {
	rep := ZipReport{}
	f, err := os.OpenFile(zipPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return rep, fmt.Errorf("verify the backup zip: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return rep, fmt.Errorf("verify the backup zip: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return rep, fmt.Errorf("verify the backup zip: %w", filecopy.ErrNotRegular)
	}
	size := fi.Size()
	if n, ok := announcedEntries(f, size); ok && n > MaxEntries {
		return rep.failZip("the zip has %d entries (at most %d are accepted)", n, MaxEntries), nil
	}
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return rep.failZip("not a valid zip file: %v", zipError(err)), nil
	}
	if len(zr.File) > MaxEntries {
		return rep.failZip("the zip has %d entries (at most %d are accepted)", len(zr.File), MaxEntries), nil
	}
	dbName := DatabaseName(app)
	var (
		total   uint64
		dbEntry *zip.File
		seen    = map[string]bool{}
		entries = make([]ZipEntry, 0, len(zr.File))
		limit   = min(uint64(MaxUncompressedBytes), max(uint64(size)*MaxExpansion, ExpansionAllowance))
	)
	for _, zf := range zr.File {
		switch {
		case len(zf.Name) > MaxEntryNameBytes:
			return rep.failZip("an entry name is longer than %d bytes", MaxEntryNameBytes), nil
		case seen[zf.Name]:
			return rep.failZip("the entry %q appears twice", zf.Name), nil
		case zf.UncompressedSize64 > limit || total+zf.UncompressedSize64 > limit:
			return rep.failZip("the entries expand to more than %d bytes (%d times the zip's size above %d MiB, at most %d GiB)",
				limit, MaxExpansion, ExpansionAllowance>>20, MaxUncompressedBytes>>30), nil
		}
		seen[zf.Name] = true
		total += zf.UncompressedSize64
		entries = append(entries, ZipEntry{Name: zf.Name, Size: int64(zf.UncompressedSize64), CRC32: fmt.Sprintf("%08x", zf.CRC32)})
		if zf.Name == dbName {
			dbEntry = zf
		}
	}
	rep.Entries = entries
	var missing []string
	for _, name := range []string{ConfigXML, dbName} {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return rep.failZip("the zip has no %s at its top level (not a backup of this application)", strings.Join(missing, " or ")), nil
	}
	work, err := os.OpenRoot(workDir)
	if err != nil {
		return rep, fmt.Errorf("open the staging directory: %w", err)
	}
	defer work.Close()
	if free, _, err := filecopy.FreeSpace(work); err == nil && free < dbEntry.UncompressedSize64+checkSpaceMargin {
		return rep, fmt.Errorf("not enough free space in the staging directory to check the database: it needs %d bytes plus 1 GiB, %d are free",
			dbEntry.UncompressedSize64, free)
	}

	rep.Zip = IntegrityOK
	checkPath := filepath.Join(workDir, CheckDBName)
	defer os.Remove(checkPath)
	dbExtracted := false
	for _, zf := range zr.File {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if zf == dbEntry {
			ok, err := extractEntry(ctx, zf, work, &rep)
			if err != nil {
				return rep, err
			}
			dbExtracted = ok
			continue
		}
		if err := readEntry(ctx, zf); err != nil {
			if ctx.Err() != nil {
				return rep, ctx.Err()
			}
			rep.Zip = IntegrityFailed
			rep.problem("%s: %v", zf.Name, zipError(err))
		}
	}
	if !dbExtracted {
		rep.Database = IntegrityNotChecked
		if rep.Zip == IntegrityOK {
			rep.Zip = IntegrityFailed
		}
		return rep, nil
	}
	lines, err := plexdb.QuickCheck(ctx, checkPath)
	if err != nil {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		return rep, fmt.Errorf("check the database: %w", err)
	}
	rep.Database = IntegrityOK
	if len(lines) > 0 {
		rep.Database = IntegrityFailed
		for _, l := range lines {
			rep.problem("%s: %s", dbName, l)
		}
	}
	return rep, nil
}

// readEntry reads an entry to its end; archive/zip checks its size and CRC.
func readEntry(ctx context.Context, zf *zip.File) error {
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, &ctxReader{ctx: ctx, r: rc})
	return err
}

// extractEntry writes the database entry to check.db inside work: exactly its declared size,
// then one more read must end the entry (which makes archive/zip check the CRC). It reports
// whether the extracted file can be checked; a problem with the entry is recorded in rep.
func extractEntry(ctx context.Context, zf *zip.File, work *os.Root, rep *ZipReport) (bool, error) {
	fail := func(err error) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		rep.Zip = IntegrityFailed
		rep.Database = IntegrityNotChecked
		rep.problem("%s: %v", zf.Name, zipError(err))
		return false, nil
	}
	rc, err := zf.Open()
	if err != nil {
		return fail(err)
	}
	defer rc.Close()
	out, err := work.OpenFile(CheckDBName, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false, fmt.Errorf("create %s in the staging directory: %w", CheckDBName, err)
	}
	src := &ctxReader{ctx: ctx, r: rc}
	_, err = io.CopyN(out, src, int64(zf.UncompressedSize64))
	if cerr := out.Close(); err == nil && cerr != nil {
		return false, fmt.Errorf("write %s: %w", CheckDBName, cerr)
	}
	if err != nil {
		return fail(err)
	}
	var one [1]byte
	n, err := src.Read(one[:])
	switch {
	case n > 0:
		return fail(errors.New("the entry holds more data than it declares"))
	case err == nil:
		// A reader may return 0, nil; read to the end to get the CRC check.
		if extra, err := io.Copy(io.Discard, src); err != nil {
			return fail(err)
		} else if extra > 0 {
			return fail(errors.New("the entry holds more data than it declares"))
		}
	case !errors.Is(err, io.EOF):
		return fail(err)
	}
	return true, nil
}

// zipError describes an archive/zip failure.
func zipError(err error) string {
	switch {
	case errors.Is(err, zip.ErrChecksum):
		return "CRC mismatch (the entry is damaged)"
	case errors.Is(err, zip.ErrFormat):
		return "not a valid zip entry (its data does not match its header)"
	case errors.Is(err, zip.ErrAlgorithm):
		return "unsupported compression method"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "the entry ends early (the zip is truncated)"
	}
	return err.Error()
}

// ctxReader stops a read when ctx is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

// Read implements io.Reader.
func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Signatures of the end-of-central-directory records.
const (
	eocdSignature       = 0x06054b50
	eocdLen             = 22
	zip64LocatorSig     = 0x07064b50
	zip64LocatorLen     = 20
	zip64EOCDSignature  = 0x06064b50
	zip64EOCDLen        = 56
	maxZipCommentLength = 0xffff
)

// announcedEntries reads the number of entries the end-of-central-directory record announces
// (the zip64 one when the classic record says 0xffff), without parsing the central directory. ok
// is false when no record is found (zip.NewReader then reports the zip as invalid).
func announcedEntries(r io.ReaderAt, size int64) (n uint64, ok bool) {
	if size < eocdLen {
		return 0, false
	}
	tail := min(size, int64(eocdLen+maxZipCommentLength))
	buf := make([]byte, tail)
	if _, err := r.ReadAt(buf, size-tail); err != nil && !errors.Is(err, io.EOF) {
		return 0, false
	}
	sig := binary.LittleEndian.AppendUint32(nil, eocdSignature)
	i := bytes.LastIndex(buf, sig)
	if i < 0 || len(buf)-i < eocdLen {
		return 0, false
	}
	eocd := buf[i:]
	count := uint64(binary.LittleEndian.Uint16(eocd[10:12]))
	if count != 0xffff {
		return count, true
	}
	// zip64: the locator sits right before the classic record.
	locOff := size - tail + int64(i) - zip64LocatorLen
	if locOff < 0 {
		return count, true
	}
	loc := make([]byte, zip64LocatorLen)
	if _, err := r.ReadAt(loc, locOff); err != nil || binary.LittleEndian.Uint32(loc[0:4]) != zip64LocatorSig {
		return count, true
	}
	recOff := binary.LittleEndian.Uint64(loc[8:16])
	if size < zip64EOCDLen || recOff > uint64(size-zip64EOCDLen) {
		return count, true
	}
	rec := make([]byte, zip64EOCDLen)
	if _, err := r.ReadAt(rec, int64(recOff)); err != nil || binary.LittleEndian.Uint32(rec[0:4]) != zip64EOCDSignature {
		return count, true
	}
	return binary.LittleEndian.Uint64(rec[32:40]), true
}
