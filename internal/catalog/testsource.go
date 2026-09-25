package catalog

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// TestResult is the outcome of TestSource (API: POST /sources/test).
type TestResult struct {
	OK bool `json:"ok"`
	// Path is the resolved path that would be stored.
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	IsDir   bool   `json:"isDir"`
	FSType  string `json:"fsType"`
	FUSE    bool   `json:"fuse"`
	Entries int    `json:"entries"`
	// Message is "ok" or why the path cannot be a source.
	Message  string   `json:"message"`
	Warnings []string `json:"warnings"`
}

// maxTestEntries bounds the top-level entries TestSource counts.
const maxTestEntries = 100_000

// TestSource checks whether p can be a source, reading only its top-level directory listing. It
// never fails: problems are in Message (not ok) and Warnings (ok, but worth knowing).
func TestSource(p string) TestResult {
	res := TestResult{Path: p, Warnings: []string{}}
	if strings.TrimSpace(p) == "" || !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		res.Message = "path must be absolute (the path inside the Bunkarr container)"
		return res
	}
	clean := filepath.Clean(p)
	li, err := os.Lstat(clean)
	if err != nil {
		res.Message = describePathErr(p, err)
		return res
	}
	res.Exists = true
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		res.Message = describePathErr(p, err)
		return res
	}
	res.Path = resolved
	if li.Mode()&fs.ModeSymlink != 0 || resolved != clean {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s is (or passes through) a symlink; Bunkarr stores the resolved path %s", p, resolved))
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		res.Message = describePathErr(p, err)
		return res
	}
	res.IsDir = fi.IsDir()
	if !res.IsDir {
		res.Message = fmt.Sprintf("%s is not a directory", p)
		return res
	}
	if resolved == string(filepath.Separator) {
		res.Message = "the filesystem root cannot be a source"
		return res
	}
	if name, fuse, err := fsTypeOfPath(resolved); err == nil {
		res.FSType, res.FUSE = name, fuse
	} else {
		res.Warnings = append(res.Warnings, fmt.Sprintf("cannot determine the filesystem type: %v", err))
	}
	if res.FUSE {
		msg := "the path is on a FUSE filesystem: hardlinks are only detected when inode numbers are stable"
		if resolved == "/mnt/user" || strings.HasPrefix(resolved, "/mnt/user/") || strings.HasPrefix(resolved, "/mnt/user0/") {
			msg = "the path is on Unraid's /mnt/user (FUSE): hardlinks are only detected with Settings → Global Share Settings → \"Tunable (support Hard Links)\" enabled"
		}
		res.Warnings = append(res.Warnings, msg+"; mounting a pool or disk path (for example /mnt/cache/data or /mnt/disk1/data) avoids this. Bunkarr confirms FUSE hardlinks by hashing the first and last MiB.")
	}
	n, err := countEntries(resolved)
	if err != nil {
		res.Message = describePathErr(p, err)
		return res
	}
	res.Entries = n
	if n == 0 {
		res.Warnings = append(res.Warnings, "the directory is empty (is the share mounted into the container?)")
	}
	res.OK = true
	res.Message = "ok"
	return res
}

// countEntries counts the top-level entries of dir (at most maxTestEntries).
func countEntries(dir string) (int, error) {
	f, err := os.Open(dir)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	for n < maxTestEntries {
		names, err := f.Readdirnames(1024)
		n += len(names)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func describePathErr(p string, err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("%s does not exist (is the volume mounted into the container?)", p)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf("%s is not readable by Bunkarr's user: permission denied (check PUID/PGID)", p)
	default:
		return fmt.Sprintf("cannot read %s: %v", p, err)
	}
}
