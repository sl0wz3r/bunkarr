package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"syscall"
)

// maxSumsBytes bounds SHA256SUMS.
const maxSumsBytes = 4 << 10

// Parse reads a manifest.json: a single JSON object of FormatName and FormatVersion (anything
// else wraps ErrFormat). Unknown fields are ignored, so a later writer may add some.
func Parse(r io.Reader) (*Manifest, error) {
	dec := json.NewDecoder(r)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: data after the manifest object", ErrFormat)
	}
	switch {
	case m.Format != FormatName:
		return nil, fmt.Errorf("%w: format %q", ErrFormat, m.Format)
	case m.FormatVersion != FormatVersion:
		return nil, fmt.Errorf("%w: format version %d (this Bunkarr reads %d)", ErrFormat, m.FormatVersion, FormatVersion)
	case m.Scope.Kind != ScopeDestination && m.Scope.Kind != ScopeExport:
		return nil, fmt.Errorf("%w: scope %q", ErrFormat, m.Scope.Kind)
	}
	return &m, nil
}

// ParseDir reads a version directory (a copy of .bunkarr/manifests/<version>/, or one at the
// destination): it checks manifest.json and manifest.csv against SHA256SUMS (wrapping ErrDamaged
// when a file is missing or does not match) and returns the parsed manifest.json.
func ParseDir(dir string) (*Manifest, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open manifest directory %s: %w", dir, err)
	}
	defer root.Close()
	m, _, err := ReadVersion(root, ".", "")
	return m, err
}

// VersionCheck is what CheckVersion found in a version directory.
type VersionCheck struct {
	// JSON and CSV are the hex sha256 of manifest.json and manifest.csv.
	JSON string
	CSV  string
	// JSONSize is manifest.json's size.
	JSONSize int64
}

// CheckVersion hashes the files of the version directory rel inside root (every directory on the
// way real, the files regular and opened without following symlinks) and compares them with its
// SHA256SUMS and, when checksum is not "", with the recorded "sha256:<hex>" of manifest.json. A
// missing or mismatching file wraps ErrDamaged; other errors (the share went away) do not.
func CheckVersion(root *os.Root, rel, checksum string) (VersionCheck, error) {
	var c VersionCheck
	sums, err := readSums(root, rel)
	if err != nil {
		return c, err
	}
	if checksum != "" && Checksum(sums[JSONName]) != checksum {
		return c, fmt.Errorf("%w: %s does not match the recorded checksum", ErrDamaged, SumsName)
	}
	if c.JSON, c.JSONSize, err = hashVersionFile(root, rel, JSONName, nil); err != nil {
		return c, err
	}
	if c.JSON != sums[JSONName] {
		return c, fmt.Errorf("%w: %s", ErrDamaged, JSONName)
	}
	if c.CSV, _, err = hashVersionFile(root, rel, CSVName, nil); err != nil {
		return c, err
	}
	if c.CSV != sums[CSVName] {
		return c, fmt.Errorf("%w: %s", ErrDamaged, CSVName)
	}
	return c, nil
}

// ReadVersion is CheckVersion that also parses manifest.json. A version whose manifest.json
// parses but whose checksums do not match returns the manifest together with an error wrapping
// ErrDamaged (recovery records such a version as damaged).
func ReadVersion(root *os.Root, rel, checksum string) (*Manifest, VersionCheck, error) {
	c, cerr := CheckVersion(root, rel, checksum)
	if cerr != nil && !errors.Is(cerr, ErrDamaged) {
		return nil, c, cerr
	}
	f, err := openVersionFile(root, rel, JSONName)
	if err != nil {
		return nil, c, err
	}
	defer f.Close()
	h := sha256.New()
	m, err := Parse(io.TeeReader(f, h))
	if err != nil {
		return nil, c, err
	}
	if cerr != nil {
		c.JSON = hex.EncodeToString(h.Sum(nil))
		return m, c, cerr
	}
	return m, c, nil
}

// readSums reads and parses a version's SHA256SUMS.
func readSums(root *os.Root, rel string) (map[string]string, error) {
	f, err := openVersionFile(root, rel, SumsName)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSumsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", SumsName, err)
	}
	if len(data) > maxSumsBytes {
		return nil, fmt.Errorf("%w: %s is too large", ErrDamaged, SumsName)
	}
	return ParseSums(data)
}

// hashVersionFile returns the hex sha256 and size of a version's file, copying it to copyTo when
// that is not nil.
func hashVersionFile(root *os.Root, rel, name string, copyTo io.Writer) (string, int64, error) {
	f, err := openVersionFile(root, rel, name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	var h hash.Hash = sha256.New()
	var w io.Writer = h
	if copyTo != nil {
		w = io.MultiWriter(h, copyTo)
	}
	n, err := io.Copy(w, f)
	if err != nil {
		return "", n, fmt.Errorf("read %s: %w", name, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// openVersionFile opens a regular file of a version directory for reading, never through a
// symlink (the directories on the way must be real ones). A missing or non-regular file wraps
// ErrDamaged.
func openVersionFile(root *os.Root, rel, name string) (*os.File, error) {
	p := path.Join(rel, name)
	if rel != "." {
		if err := realDirsOrDamaged(root, rel); err != nil {
			return nil, err
		}
	}
	fi, err := root.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: %s is missing", ErrDamaged, name)
	case err != nil:
		return nil, fmt.Errorf("look up %s: %w", p, err)
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrDamaged, name)
	}
	f, err := root.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("%w: %s is missing", ErrDamaged, name)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	return f, nil
}

// realDirsOrDamaged checks that rel and its parents are real directories; one that is missing or
// is not a directory makes the version damaged.
func realDirsOrDamaged(root *os.Root, rel string) error {
	if err := realDirs(root, rel); err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNotDir) {
			return fmt.Errorf("%w: %s: %v", ErrDamaged, rel, err)
		}
		return err
	}
	return nil
}

var errNotDir = errors.New("not a directory")

// realDirs checks that rel and every directory above it inside root are real directories (not
// symlinks), so a path cannot be redirected elsewhere in the destination.
func realDirs(root *os.Root, rel string) error {
	cur := ""
	for _, part := range splitPath(rel) {
		cur = path.Join(cur, part)
		fi, err := root.Lstat(cur)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s: %w", cur, errNotDir)
		}
	}
	return nil
}

func splitPath(rel string) []string {
	var out []string
	for rel != "" && rel != "." {
		dir, base := path.Split(rel)
		out = append([]string{base}, out...)
		rel = path.Clean(dir)
		if rel == "/" {
			break
		}
	}
	return out
}
