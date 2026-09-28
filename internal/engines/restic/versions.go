package restic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// The config-version store of restic destinations (docs/design/phase4.md §8.2): one snapshot per
// version. Put renames the staged version directory to
// <staging>/versions/<destinationId>/<logical path>, so the snapshot's path ends with the logical
// path, backs it up with --time = the version's time and the version's tags, and reads the
// snapshot back (its tags, and every file with its staged size). Remove never touches the
// repository: it requests the forget in the caller's transaction, and the destination's retention
// job forgets the snapshot under the S24 checks (D28).

// ManifestName is the file every version holds.
const ManifestName = "manifest.json"

// ForgetRequester records a forget request in the caller's transaction (engine_forget; enginerun
// implements it).
type ForgetRequester func(ctx context.Context, tx *sql.Tx, snapshotID, kind, reason string) error

type versionStore struct {
	r             *Repo
	stagingRoot   string
	requestForget ForgetRequester
	mu            sync.Mutex
	// dirs caches each version snapshot's directory (its only path).
	dirs map[string]string
}

// NewVersionStore returns the VersionStore of a restic destination. stagingRoot is
// <config>/staging, where the runners stage versions; requestForget records a forget request.
// It removes what earlier Puts left under <staging>/versions (sweepVersions).
func NewVersionStore(r *Repo, stagingRoot string, requestForget ForgetRequester) engines.VersionStore {
	s := &versionStore{r: r, stagingRoot: filepath.Clean(stagingRoot), requestForget: requestForget, dirs: map[string]string{}}
	putting.Lock()
	sweepVersions(filepath.Join(s.stagingRoot, "versions"), r.drv.log())
	putting.Unlock()
	return s
}

// putting holds the version directories that Puts of this process (any destination) have staged
// under <staging>/versions right now.
var putting = struct {
	sync.Mutex
	dirs map[string]bool
}{dirs: map[string]bool{}}

// sweepVersions removes everything under root (<staging>/versions) that no Put of this process is
// using. Put moves the staged version there and back when it ends; a crash or kill during the
// backup leaves it there (Preferences.xml with the PlexOnlineToken, an *arr zip with API keys, in
// clear text), and the resumed job stages a new version under another logical path, so nothing
// else would remove it. The caller holds putting's lock.
func sweepVersions(root string, log *slog.Logger) {
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			switch {
			case putting.dirs[p]:
			case e.IsDir() && holdsPut(p):
				walk(p)
			default:
				if err := os.RemoveAll(p); err != nil {
					log.Warn("restic versions: could not remove a staged version an interrupted Put left", "path", p, "error", err)
				}
			}
		}
	}
	walk(root)
}

// holdsPut reports whether a Put in progress uses a directory below dir.
func holdsPut(dir string) bool {
	for d := range putting.dirs {
		if strings.HasPrefix(d, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// versionsDir is <staging>/versions/<destinationId>.
func (s *versionStore) versionsDir() string {
	return filepath.Join(s.stagingRoot, "versions", strconv.FormatInt(s.r.dest.ID, 10))
}

// kindOf maps a kind folder (".bunkarr/plex", ".bunkarr/arr", ".bunkarr/manifests", or a folder
// below one) to its snapshot kind.
func kindOf(folder string) (string, bool) {
	parts := strings.Split(folder, "/")
	if len(parts) < 2 || parts[0] != filecopy.MetaDir {
		return "", false
	}
	switch parts[1] {
	case "plex":
		return engines.VersionPlexDB, true
	case "arr":
		return engines.VersionArr, true
	case "manifests":
		return engines.VersionManifest, true
	}
	return "", false
}

// checkLogical checks a version's logical path: clean, relative, under .bunkarr/ and a kind
// folder, without control characters.
func checkLogical(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("restic versions: %q is not a clean relative path", p)
	}
	if _, ok := kindOf(p); !ok || strings.Count(p, "/") < 2 {
		return fmt.Errorf("restic versions: %q is not a config version path", p)
	}
	return nil
}

// staged lists the regular files under dir with their sizes (manifest.json required).
func staged(dir string) (map[string]int64, error) {
	files := map[string]int64{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
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

// Put implements engines.VersionStore (§8.2).
func (s *versionStore) Put(ctx context.Context, v engines.PutVersion) (engines.Ref, error) {
	if err := checkLogical(v.LogicalPath); err != nil {
		return "", err
	}
	kind, _ := kindOf(v.LogicalPath)
	if v.Kind != kind {
		return "", fmt.Errorf("restic versions: a %s version under %s", v.Kind, v.LogicalPath)
	}
	dir := filepath.Clean(v.Dir)
	if rel, err := filepath.Rel(s.stagingRoot, dir); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("restic versions: %s is not inside the staging directory %s", v.Dir, s.stagingRoot)
	}
	files, err := staged(dir)
	if err != nil {
		return "", err
	}
	tags, err := Tags(TagInput{EngineTag: s.r.dest.EngineTag, Kind: v.Kind, JobID: v.JobID, IntegrationID: v.IntegrationID,
		Version: path.Base(v.LogicalPath)})
	if err != nil {
		return "", err
	}
	target := filepath.Join(s.versionsDir(), filepath.FromSlash(v.LogicalPath))
	putting.Lock()
	if putting.dirs[target] {
		putting.Unlock()
		return "", fmt.Errorf("restic versions: %s is being stored already", v.LogicalPath)
	}
	// What earlier Puts left (a crash during their backup) goes first, this one's own leftover
	// included.
	sweepVersions(filepath.Join(s.stagingRoot, "versions"), s.r.drv.log())
	putting.dirs[target] = true
	putting.Unlock()
	defer func() {
		putting.Lock()
		delete(putting.dirs, target)
		putting.Unlock()
	}()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", fmt.Errorf("restic versions: %w", err)
	}
	if err := os.Rename(dir, target); err != nil {
		return "", fmt.Errorf("restic versions: stage %s: %w", v.LogicalPath, err)
	}
	defer func() {
		if err := os.Rename(target, dir); err != nil {
			s.r.drv.log().Warn("restic versions: move the staged version back", "path", target, "error", err)
		}
	}()
	t := v.Time
	// A local repository is limited per backup (§9.1: config version jobs use the destination's
	// limits); remote kinds through RCLONE_BWLIMIT.
	up, down := s.r.dest.Bandwidth.InForce(s.r.rt.Clock(), s.r.rt.Location)
	res, err := s.r.Backup(ctx, BackupArgs{Paths: []string{target}, Tags: tags, Time: &t, RetryLock: true,
		LimitUpKiB: up, LimitDownKiB: down})
	if err != nil {
		return "", err
	}
	if res.Interrupted || res.Exit != 0 {
		return "", fmt.Errorf("restic versions: the backup of %s did not complete (exit %d): %v", v.LogicalPath, res.Exit, res.Errors)
	}
	snaps, err := s.r.Snapshots(ctx, nil, []string{res.SnapshotID})
	if err != nil {
		return "", err
	}
	if len(snaps) != 1 || !snaps[0].HasTags(tags...) || !slices.Equal(snaps[0].Paths, []string{target}) {
		return "", fmt.Errorf("restic versions: snapshot %s of %s did not read back with its tags and path", shortID(res.SnapshotID), v.LogicalPath)
	}
	got, err := s.files(ctx, res.SnapshotID, target)
	if err != nil {
		return "", err
	}
	for _, n := range slices.Sorted(maps.Keys(files)) {
		if size, ok := got[n]; !ok || size != files[n] {
			return "", fmt.Errorf("restic versions: snapshot %s of %s lacks %s with %d bytes", shortID(res.SnapshotID), v.LogicalPath, n, files[n])
		}
	}
	s.mu.Lock()
	s.dirs[res.SnapshotID] = target
	s.mu.Unlock()
	return engines.Ref(res.SnapshotID), nil
}

// files reads back the files under dir in a snapshot, by name relative to dir.
func (s *versionStore) files(ctx context.Context, id, dir string) (map[string]int64, error) {
	got := map[string]int64{}
	err := s.r.Ls(ctx, id, func(n Node) error {
		if rel, ok := strings.CutPrefix(n.Path, dir+"/"); ok && n.Type == "file" {
			got[rel] = n.Size
		}
		return nil
	})
	return got, err
}

// logicalOf returns the logical path of a version snapshot's directory
// (…/versions/<destinationId>/.bunkarr/…), also when the config directory moved since.
func (s *versionStore) logicalOf(dir string) (string, bool) {
	marker := "/versions/" + strconv.FormatInt(s.r.dest.ID, 10) + "/" + filecopy.MetaDir + "/"
	i := strings.LastIndex(dir, marker)
	if i < 0 {
		return "", false
	}
	logical := dir[i+len(marker)-len(filecopy.MetaDir)-1:]
	return logical, checkLogical(logical) == nil
}

// List implements engines.VersionStore: the snapshots of this row's kind (restic snapshots
// --json --no-lock --tag bunkarr-dest:<engine_tag>,bunkarr-kind:<kind>) under kindFolder, with
// JobID and IntegrationID from their tags and their files from a read-back. Snapshots whose tags
// do not parse for this row are not listed.
func (s *versionStore) List(ctx context.Context, kindFolder string) ([]engines.StoredVersion, error) {
	kindFolder = strings.TrimSuffix(kindFolder, "/")
	kind, ok := kindOf(kindFolder)
	if !ok {
		return nil, fmt.Errorf("restic versions: %q is not a config version folder", kindFolder)
	}
	snaps, err := s.r.Snapshots(ctx, []string{KindTag(kind)}, nil)
	if err != nil {
		return nil, err
	}
	var out []engines.StoredVersion
	for _, sn := range snaps {
		info, ok := ParseTags(sn.Tags, s.r.dest.EngineTag)
		if !ok || info.Kind != kind || len(sn.Paths) != 1 {
			continue
		}
		logical, ok := s.logicalOf(sn.Paths[0])
		if !ok || !strings.HasPrefix(logical, kindFolder+"/") {
			continue
		}
		files, err := s.files(ctx, sn.ID, sn.Paths[0])
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.dirs[sn.ID] = sn.Paths[0]
		s.mu.Unlock()
		out = append(out, engines.StoredVersion{Ref: engines.Ref(sn.ID), LogicalPath: logical, Version: info.Version, Time: sn.Time,
			Complete: true, Files: files, JobID: info.JobID, IntegrationID: info.IntegrationID})
	}
	slices.SortStableFunc(out, func(a, b engines.StoredVersion) int {
		if c := strings.Compare(a.LogicalPath, b.LogicalPath); c != 0 {
			return c
		}
		return a.Time.Compare(b.Time)
	})
	return out, nil
}

// dirOf returns the directory a version snapshot holds.
func (s *versionStore) dirOf(ctx context.Context, ref engines.Ref) (string, error) {
	id := string(ref)
	if !snapshotIDRe.MatchString(id) {
		return "", fmt.Errorf("restic versions: %q is not a snapshot id", id)
	}
	s.mu.Lock()
	dir, ok := s.dirs[id]
	s.mu.Unlock()
	if ok {
		return dir, nil
	}
	snaps, err := s.r.Snapshots(ctx, nil, []string{id})
	if err != nil {
		return "", err
	}
	if len(snaps) != 1 || len(snaps[0].Paths) != 1 {
		return "", fmt.Errorf("restic versions: snapshot %s: %w", shortID(id), fs.ErrNotExist)
	}
	if _, ok := s.logicalOf(snaps[0].Paths[0]); !ok {
		return "", fmt.Errorf("restic versions: snapshot %s is not a config version", shortID(id))
	}
	s.mu.Lock()
	s.dirs[id] = snaps[0].Paths[0]
	s.mu.Unlock()
	return snaps[0].Paths[0], nil
}

// restoreOne restores one file of a version into a scratch run directory and returns the
// restored path and the directory's cleanup.
func (s *versionStore) restoreOne(ctx context.Context, ref engines.Ref, name string) (string, func(), error) {
	if name == "" || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return "", nil, fmt.Errorf("restic versions: %q is not a file name of a version", name)
	}
	dir, err := s.dirOf(ctx, ref)
	if err != nil {
		return "", nil, err
	}
	scratch, err := s.r.dirs.New(s.r.rt.JobID)
	if err != nil {
		return "", nil, fmt.Errorf("restic versions: %w", err)
	}
	cleanup := func() { _ = scratch.Remove() }
	target := filepath.Join(scratch.DataDir(), "restore")
	if err := s.r.Restore(ctx, RestoreArgs{Snapshot: string(ref), Subpath: dir, Target: target, Include: []string{name}}); err != nil {
		cleanup()
		return "", nil, err
	}
	p := filepath.Join(target, filepath.FromSlash(name))
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		cleanup()
		return "", nil, fmt.Errorf("restic versions: %s of snapshot %s was not restored: %w", name, shortID(string(ref)), fs.ErrNotExist)
	}
	return p, cleanup, nil
}

// ReadFile implements engines.VersionStore: the file is restored byte-exact into a scratch run
// directory (restic restore --include-file) and at most limit bytes are returned.
func (s *versionStore) ReadFile(ctx context.Context, ref engines.Ref, name string, limit int64) ([]byte, error) {
	p, cleanup, err := s.restoreOne(ctx, ref, name)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max(limit, 0)))
}

// Fetch implements engines.VersionStore: the file is restored and moved to dstDir/<base name>
// (0600).
func (s *versionStore) Fetch(ctx context.Context, ref engines.Ref, name, dstDir string) error {
	if !filepath.IsAbs(dstDir) {
		return fmt.Errorf("restic versions: %q is not an absolute directory", dstDir)
	}
	p, cleanup, err := s.restoreOne(ctx, ref, name)
	if err != nil {
		return err
	}
	defer cleanup()
	dst := filepath.Join(dstDir, path.Base(name))
	if err := os.Rename(p, dst); err != nil {
		if !errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("restic versions: fetch %s: %w", name, err)
		}
		if err := copyFile(p, dst); err != nil {
			return fmt.Errorf("restic versions: fetch %s: %w", name, err)
		}
	}
	return os.Chmod(dst, 0o600)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Remove implements engines.VersionStore: a forget request for the snapshot in the caller's
// transaction (D28); the retention job forgets it after the S24 checks.
func (s *versionStore) Remove(ctx context.Context, tx *sql.Tx, ref engines.Ref, kind string) error {
	if !snapshotIDRe.MatchString(string(ref)) {
		return fmt.Errorf("restic versions: %q is not a snapshot id", ref)
	}
	if kind != engines.VersionPlexDB && kind != engines.VersionArr && kind != engines.VersionManifest {
		return fmt.Errorf("restic versions: unknown kind %q", kind)
	}
	if s.requestForget == nil {
		return errors.New("restic versions: no forget requester")
	}
	return s.requestForget(ctx, tx, string(ref), kind, "pruned by its runner")
}
