package rclone

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// The config-version store of rclone destinations (docs/design/phase4.md §8.3): a version is the
// directory <root>/<logical path> (".bunkarr/<kind>/<folder>/<version>", or
// ".bunkarr/manifests/<version>"), written with manifest.json last, so a directory without it is
// incomplete, and read back by a listing of every file with its staged size.

// ManifestName is the file a version directory is complete with.
const ManifestName = "manifest.json"

// versionIDLimit bounds how much of a manifest.json List reads for its job and integration ids.
const versionIDLimit = 64 << 10

type versionStore struct {
	c            *Conn
	isVersionDir func(string) bool
}

// NewVersionStore returns the VersionStore of an rclone destination. isVersionDir decides what
// a version directory is (the caller wires snapshots.Layout.SplitVersionPath and the manifests'
// version pattern); Remove purges only a path it accepts, and List groups files by it.
func NewVersionStore(c *Conn, isVersionDir func(string) bool) engines.VersionStore {
	return &versionStore{c: c, isVersionDir: isVersionDir}
}

// stagedFiles lists the regular files under dir (slash-separated relative names) with their
// sizes; anything else (a symlink, a device) is an error.
func stagedFiles(dir string) (map[string]int64, error) {
	files := map[string]int64{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = fi.Size()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("staged version %s: %w", dir, err)
	}
	if _, ok := files[ManifestName]; !ok {
		return nil, fmt.Errorf("staged version %s has no %s", dir, ManifestName)
	}
	return files, nil
}

// Put implements engines.VersionStore (§8.3): rclone copy <dir> <root>/<logical path>
// --exclude /manifest.json, then rclone copyto <dir>/manifest.json <root>/<logical
// path>/manifest.json, then a listing of every file with its staged size. The reference is the
// logical path.
func (s *versionStore) Put(ctx context.Context, v engines.PutVersion) (engines.Ref, error) {
	if err := checkVersionDir(v.LogicalPath, s.isVersionDir); err != nil {
		return "", err
	}
	if err := checkLocal(v.Dir); err != nil {
		return "", err
	}
	files, err := stagedFiles(v.Dir)
	if err != nil {
		return "", err
	}
	names := slices.Sorted(maps.Keys(files))
	for _, n := range names {
		if err := CheckFileListName(n); err != nil {
			return "", err
		}
	}
	if len(files) > 1 {
		t, ch := s.c.transfers()
		cmd := command{words: []string{"copy"}, args: func(*proc.RunDir) []string {
			a := []string{v.Dir, s.c.remote(v.LogicalPath), "--exclude", "/" + ManifestName}
			a = append(a, transferLog...)
			return append(a, "--transfers", t, "--checkers", ch)
		}}
		out, err := s.c.run(ctx, cmd)
		if err != nil {
			return "", err
		}
		if err := check(cmd, out); err != nil {
			return "", err
		}
	}
	if err := s.c.CopyTo(ctx, filepath.Join(v.Dir, ManifestName), v.LogicalPath+"/"+ManifestName); err != nil {
		return "", err
	}
	got, err := s.c.StatMany(ctx, v.LogicalPath, names)
	if err != nil {
		return "", err
	}
	for _, n := range names {
		o, ok := got[n]
		switch {
		case !ok:
			return "", fmt.Errorf("version %s did not read back: %s is missing", v.LogicalPath, n)
		case o.Size != files[n]:
			return "", fmt.Errorf("version %s did not read back: %s has %d bytes, staged %d", v.LogicalPath, n, o.Size, files[n])
		}
	}
	return engines.Ref(v.LogicalPath), nil
}

// versionTimeRe matches the time at the start of a version name (20260924T120000Z-job12).
var versionTimeRe = regexp.MustCompile(`^\d{8}T\d{6}Z`)

// List implements engines.VersionStore: every file under kindFolder, grouped into the version
// directories isVersionDir accepts. Complete means manifest.json is there; JobID and
// IntegrationID come from its first bytes. A missing kind folder has no versions.
func (s *versionStore) List(ctx context.Context, kindFolder string) ([]engines.StoredVersion, error) {
	kindFolder = strings.TrimSuffix(kindFolder, "/")
	if err := CheckArgName(kindFolder); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(kindFolder, filecopy.MetaDir+"/") {
		return nil, fmt.Errorf("%w: %q is not a config version folder", ErrFence, kindFolder)
	}
	versions := map[string]*engines.StoredVersion{}
	newest := map[string]time.Time{}
	err := s.c.LsJSON(ctx, kindFolder, true, func(o Object) error {
		full := kindFolder + "/" + o.Path
		dir, name, ok := s.versionOf(kindFolder, full)
		if !ok {
			return nil
		}
		v := versions[dir]
		if v == nil {
			v = &engines.StoredVersion{Ref: engines.Ref(dir), LogicalPath: dir, Version: path.Base(dir), Files: map[string]int64{}}
			versions[dir] = v
		}
		v.Files[name] = o.Size
		if o.ModTime.After(newest[dir]) {
			newest[dir] = o.ModTime
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrPathNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var out []engines.StoredVersion
	for _, dir := range slices.Sorted(maps.Keys(versions)) {
		v := versions[dir]
		v.Time = newest[dir]
		if m := versionTimeRe.FindString(v.Version); m != "" {
			if t, err := time.Parse("20060102T150405Z", m); err == nil {
				v.Time = t
			}
		}
		if _, ok := v.Files[ManifestName]; ok {
			v.Complete = true
			head, err := s.c.Cat(ctx, dir+"/"+ManifestName, versionIDLimit)
			if err != nil {
				return nil, err
			}
			v.JobID, v.IntegrationID = versionIDs(head)
		}
		out = append(out, *v)
	}
	return out, nil
}

// versionOf returns the version directory holding the file full (the shallowest ancestor
// isVersionDir accepts) and the file's name inside it.
func (s *versionStore) versionOf(kindFolder, full string) (dir, name string, ok bool) {
	if s.isVersionDir == nil {
		return "", "", false
	}
	rest := strings.TrimPrefix(full, kindFolder+"/")
	parts := strings.Split(rest, "/")
	for i := 1; i < len(parts); i++ {
		d := kindFolder + "/" + strings.Join(parts[:i], "/")
		if s.isVersionDir(d) {
			return d, strings.Join(parts[i:], "/"), true
		}
	}
	return "", "", false
}

// versionIDs reads the job and integration ids from the start of a version's manifest.json:
// top-level "jobId" and "integrationId" (Plex DB and *arr versions) or "job": {"id"} (library
// manifests). A manifest cut at the read limit still yields the ids before the cut.
func versionIDs(head []byte) (jobID, integrationID int64) {
	dec := json.NewDecoder(bytes.NewReader(head))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return 0, 0
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		key, _ := tok.(string)
		switch key {
		case "jobId":
			if dec.Decode(&jobID) != nil {
				return
			}
		case "integrationId":
			if dec.Decode(&integrationID) != nil {
				return
			}
		case "job":
			var j struct {
				ID int64 `json:"id"`
			}
			if dec.Decode(&j) != nil {
				return
			}
			if jobID == 0 {
				jobID = j.ID
			}
		default:
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return
			}
		}
	}
	return
}

// checkVersionFile checks a file name inside a version directory.
func (s *versionStore) checkVersionFile(ref engines.Ref, name string) (string, error) {
	if err := checkVersionDir(string(ref), s.isVersionDir); err != nil {
		return "", err
	}
	if err := CheckArgName(name); err != nil {
		return "", err
	}
	return string(ref) + "/" + name, nil
}

// ReadFile implements engines.VersionStore: the file is downloaded byte-exact (rclone copyto)
// into a scratch run directory and at most limit bytes are returned.
func (s *versionStore) ReadFile(ctx context.Context, ref engines.Ref, name string, limit int64) ([]byte, error) {
	rel, err := s.checkVersionFile(ref, name)
	if err != nil {
		return nil, err
	}
	scratch, err := s.c.dirs.New(s.c.rt.JobID)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	defer func() { _ = scratch.Remove() }()
	local := scratch.DataPath("version-file")
	if err := s.c.Download(ctx, rel, local); err != nil {
		return nil, err
	}
	f, err := os.Open(local)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max(limit, 0)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	return b, nil
}

// Fetch implements engines.VersionStore: the file is downloaded to dstDir/<base name> (0600).
func (s *versionStore) Fetch(ctx context.Context, ref engines.Ref, name, dstDir string) error {
	rel, err := s.checkVersionFile(ref, name)
	if err != nil {
		return err
	}
	if err := checkLocal(dstDir); err != nil {
		return err
	}
	dst := filepath.Join(dstDir, path.Base(name))
	if err := s.c.Download(ctx, rel, dst); err != nil {
		return err
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return fmt.Errorf("fetch %s: %w", rel, err)
	}
	return nil
}

// Remove implements engines.VersionStore: rclone purge <root>/<logical path> --max-delete
// <files of the version + 1>, after isVersionDir accepted the path (S23). tx is not used: the
// version is gone at once, and the caller deletes its row afterwards (§8.4 step 4).
func (s *versionStore) Remove(ctx context.Context, _ *sql.Tx, ref engines.Ref, _ string) error {
	rel := string(ref)
	if err := checkVersionDir(rel, s.isVersionDir); err != nil {
		return err
	}
	n := 0
	err := s.c.LsJSON(ctx, rel, true, func(Object) error { n++; return nil })
	if errors.Is(err, ErrPathNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.c.Purge(ctx, rel, n+1, s.isVersionDir)
}
