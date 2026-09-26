package integrations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/sl0wz3r/bunkarr/internal/integrations/arr"
	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
)

// ArrTestResult is POST /integrations/test's answer for Sonarr, Radarr and Lidarr (design §13).
// OK means the URL answered as the chosen application, the API key was accepted and the checks
// below could read what they need; the other fields describe what was found.
type ArrTestResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Version string `json:"version,omitempty"`
	AppName string `json:"appName,omitempty"`
	// Backup says how backups can be fetched: from the backup folder, and over HTTP.
	Backup *ArrBackupAccess `json:"backup,omitempty"`
	// ManualBackups counts the manual backups in the *arr: it never prunes them (§10).
	ManualBackups *ArrBackupCount `json:"manualBackups,omitempty"`
	// RootFolders are the *arr's root folders, mapped with the tested path mappings.
	// It is null when they could not be read.
	RootFolders []ArrRootFolderCheck `json:"rootFolders"`
	// RecycleBin is the *arr's recycle bin, located among the sources; null when it has none.
	RecycleBin *ArrRecycleBin `json:"recycleBin"`
	// FileDate is the *arr's "change file date" setting; anything but "none" is a warning.
	FileDate string `json:"fileDate,omitempty"`
}

// Backup access outcomes of ArrBackupAccess.
const (
	BackupFolderOK         = "ok"
	BackupFolderMissing    = "missing"
	BackupFolderUnreadable = "unreadable"
	BackupFolderNotSet     = "not-set"
	BackupHTTPOK           = "ok"
	BackupHTTPLogin        = "login-required"
	BackupHTTPUnknown      = "unknown"
)

// ArrBackupAccess is how the *arr's backups can be fetched (design D3).
type ArrBackupAccess struct {
	// Folder: "ok", "missing" (the folder, or the newest backup in it, does not exist),
	// "unreadable" or "not-set".
	Folder string `json:"folder"`
	// HTTP: "ok", "login-required" (the *arr requires a login for Bunkarr's address) or
	// "unknown" (no backup exists yet, or the check failed).
	HTTP string `json:"http"`
}

// ArrBackupCount is a number of backups and their total size.
type ArrBackupCount struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// ArrRootFolderCheck is one root folder of the *arr as Bunkarr sees it.
type ArrRootFolderCheck struct {
	// Path is the root folder as the *arr sees it; Accessible is the *arr's own report.
	Path       string `json:"path"`
	Accessible bool   `json:"accessible"`
	// LocalPath is Path through the path mappings; null when no mapping covers it.
	LocalPath *string `json:"localPath"`
	// SourceID is the source LocalPath lies in (the longest prefix); null when none.
	SourceID *int64 `json:"sourceId"`
	// Exists is whether LocalPath is a directory Bunkarr can see.
	Exists bool `json:"exists"`
	// Reason says why the folder is not usable ("" when it is).
	Reason string `json:"reason"`
}

// ArrRecycleBin is the *arr's recycle bin as Bunkarr sees it (§6.1: inside a source whose
// excludes do not cover it, every upgrade's old file is copied twice).
type ArrRecycleBin struct {
	Path     string `json:"path"`
	SourceID *int64 `json:"sourceId"`
	RelPath  string `json:"relPath"`
	Excluded bool   `json:"excluded"`
}

// SourceRef is what the *arr test needs to know of a source: its id, resolved path and own
// exclude patterns.
type SourceRef struct {
	ID      int64
	Path    string
	Exclude []string
}

// ArrTestOptions configures TestArr.
type ArrTestOptions struct {
	// Client configures the *arr client (its default transport dials through netguard).
	Client arr.Options
	// Sources are the sources to locate local folders in; DefaultExcludes are the patterns every
	// source skips (catalog.DefaultExcludes: matched case-insensitively).
	Sources         []SourceRef
	DefaultExcludes []string
}

// TestArr tests an *arr integration: GET system/status must answer as typ's application with the
// key accepted; then its root folders (mapped with settings' path mappings and located among the
// sources), its media management settings (recycle bin, file date) and its backups (the backup
// folder, a 1-byte HTTP download of the newest backup, the manual backups) are checked. settings
// may be nil (an unsaved integration without settings): the backup folder is then "not-set" and
// no root folder is mapped. The message is user-facing and never contains the key.
func TestArr(ctx context.Context, typ Type, baseURL, apiKey string, settings *ArrSettings, opts ArrTestOptions) ArrTestResult {
	app := typ.AppName()
	res := ArrTestResult{}
	if !typ.IsArr() {
		res.Message = fmt.Sprintf("%s is not Sonarr, Radarr or Lidarr.", app)
		return res
	}
	done := func() ArrTestResult {
		res.Message = logging.RedactValues(res.Message, apiKey)
		return res
	}
	c, err := arr.New(arr.Kind(typ), baseURL, apiKey, opts.Client)
	if err != nil {
		res.Message = fmt.Sprintf("The URL cannot be used: %v.", err)
		return done()
	}
	st, err := c.Status(ctx)
	if err != nil {
		res.Message = describeArrError(app, apiKey, err)
		if errors.Is(err, arr.ErrWrongApp) && appNamePlain(st.AppName) {
			res.AppName, res.Version = st.AppName, st.Version
			res.Message = fmt.Sprintf("The server at this URL is %s, not %s: check the URL and the type.", st.AppName, app)
		}
		return done()
	}
	res.AppName, res.Version = st.AppName, st.Version

	var failed []string
	fail := func(what string, err error) {
		failed = append(failed, fmt.Sprintf("reading its %s failed (%s)", what, describeArrError(app, apiKey, err)))
	}
	roots, err := c.RootFolders(ctx)
	if err != nil {
		fail("root folders", err)
	} else {
		res.RootFolders = checkRootFolders(roots, settings, opts)
	}
	mm, err := c.MediaManagement(ctx)
	if err != nil {
		fail("media management settings", err)
	} else {
		res.FileDate = mm.FileDate
		if mm.RecycleBin != "" {
			res.RecycleBin = locateRecycleBin(mm.RecycleBin, settings, opts)
		}
	}
	backups, err := c.Backups(ctx)
	if err != nil {
		fail("backups", err)
	} else {
		res.ManualBackups, res.Backup = checkBackups(ctx, c, backups, settings)
	}
	if len(failed) > 0 {
		res.Message = fmt.Sprintf("Connected to %s %s, but %s.", app, st.Version, strings.Join(failed, "; "))
		return done()
	}
	res.OK = true
	res.Message = fmt.Sprintf("Connected to %s %s.", app, st.Version)
	return done()
}

// appNamePlain reports whether an appName is safe to show (a short plain name).
func appNamePlain(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ' ') {
			return false
		}
	}
	return true
}

// describeArrError turns a client error into a user-facing sentence.
func describeArrError(app, apiKey string, err error) string {
	var ae *arr.Error
	var msg string
	switch {
	case errors.Is(err, arr.ErrUnauthorized) && apiKey == "":
		msg = fmt.Sprintf("%s requires an API key (401 Unauthorized). Copy it from %s → Settings → General.", app, app)
	case errors.Is(err, arr.ErrUnauthorized):
		msg = fmt.Sprintf("%s rejected the API key (401 Unauthorized). Copy it from %s → Settings → General.", app, app)
	case errors.Is(err, arr.ErrWrongApp):
		msg = fmt.Sprintf("The server at this URL is not %s: check the URL, including any URL base such as /%s, and the type.", app, strings.ToLower(app))
	case errors.Is(err, netguard.ErrBlocked):
		msg = "Bunkarr does not connect to this address (a link-local or cloud metadata address)."
	case errors.Is(err, arr.ErrNotArr):
		msg = fmt.Sprintf("The server at this URL did not answer like %s. Check the URL, including any URL base such as /%s.", app, strings.ToLower(app))
	case errors.Is(err, context.Canceled):
		msg = "The test was cancelled."
	case errors.Is(err, context.DeadlineExceeded):
		msg = fmt.Sprintf("%s did not answer in time.", app)
	case errors.As(err, &ae) && ae.StatusCode != 0:
		msg = fmt.Sprintf("%s answered with an unexpected status (%d %s).", app, ae.StatusCode, http.StatusText(ae.StatusCode))
	default:
		msg = fmt.Sprintf("Could not reach %s: %v.", app, err)
	}
	return logging.RedactValues(msg, apiKey)
}

// checkRootFolders maps and locates the *arr's root folders.
func checkRootFolders(roots []arr.RootFolder, settings *ArrSettings, opts ArrTestOptions) []ArrRootFolderCheck {
	out := make([]ArrRootFolderCheck, 0, len(roots))
	for _, rf := range roots {
		c := ArrRootFolderCheck{Path: rf.Path, Accessible: rf.Accessible}
		var reasons []string
		if !rf.Accessible {
			reasons = append(reasons, "the *arr reports this folder as not accessible")
		}
		var local string
		ok := false
		if settings != nil {
			local, ok = settings.MapPath(rf.Path)
		}
		switch {
		case !ok:
			reasons = append(reasons, "no path mapping covers this folder")
		default:
			c.LocalPath = &local
			if fi, err := os.Stat(local); err == nil && fi.IsDir() {
				c.Exists = true
			} else {
				reasons = append(reasons, "the mapped folder does not exist in Bunkarr's container")
			}
			if loc, found := LocateInSources(opts.Sources, opts.DefaultExcludes, local); found {
				id := loc.SourceID
				c.SourceID = &id
			} else {
				reasons = append(reasons, "the mapped folder is not inside a source")
			}
		}
		c.Reason = strings.Join(reasons, "; ")
		out = append(out, c)
	}
	return out
}

// locateRecycleBin maps and locates the *arr's recycle bin.
func locateRecycleBin(p string, settings *ArrSettings, opts ArrTestOptions) *ArrRecycleBin {
	rb := &ArrRecycleBin{Path: p}
	if settings == nil {
		return rb
	}
	local, ok := settings.MapPath(p)
	if !ok {
		return rb
	}
	if loc, found := LocateInSources(opts.Sources, opts.DefaultExcludes, local); found {
		id := loc.SourceID
		rb.SourceID, rb.RelPath, rb.Excluded = &id, loc.RelPath, loc.Excluded
	}
	return rb
}

// checkBackups counts the manual backups and checks how the newest backup can be fetched.
func checkBackups(ctx context.Context, c *arr.Client, backups []arr.Backup, settings *ArrSettings) (*ArrBackupCount, *ArrBackupAccess) {
	count := &ArrBackupCount{}
	var newest *arr.Backup
	for i, b := range backups {
		if b.Type == arr.BackupManual {
			count.Count++
			count.Bytes += b.Size
		}
		if arr.ValidateBackup(b) != nil {
			continue
		}
		if newest == nil || b.Time.After(newest.Time) {
			newest = &backups[i]
		}
	}
	access := &ArrBackupAccess{Folder: BackupFolderNotSet, HTTP: BackupHTTPUnknown}
	if settings != nil && settings.BackupFolder != "" {
		access.Folder = checkBackupFolder(settings.BackupFolder, newest)
	}
	if newest != nil {
		switch got, err := c.ProbeBackup(ctx, *newest); {
		case err != nil:
		case got == arr.BackupAccessOK:
			access.HTTP = BackupHTTPOK
		case got == arr.BackupAccessLoginRequired:
			access.HTTP = BackupHTTPLogin
		}
	}
	return count, access
}

// checkBackupFolder checks the backup folder the way the backup job reads it (S18): through an
// os.Root on the folder, the newest backup at <type>/<name>, a regular file opened with
// O_NOFOLLOW.
func checkBackupFolder(folder string, newest *arr.Backup) string {
	root, err := os.OpenRoot(folder)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return BackupFolderMissing
	case err != nil:
		if fi, serr := os.Stat(folder); serr == nil && !fi.IsDir() {
			return BackupFolderMissing
		}
		return BackupFolderUnreadable
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return BackupFolderUnreadable
	}
	_, err = dir.ReadDir(1)
	_ = dir.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return BackupFolderUnreadable
	}
	if newest == nil {
		return BackupFolderOK
	}
	rel, err := arr.BackupPath(*newest)
	if err != nil {
		return BackupFolderOK
	}
	fi, err := root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return BackupFolderMissing
	case err != nil, !fi.Mode().IsRegular():
		return BackupFolderUnreadable
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return BackupFolderUnreadable
	}
	_ = f.Close()
	return BackupFolderOK
}

// PathLocation is where a local path lies in a source.
type PathLocation struct {
	SourceID int64
	// RelPath is the path relative to the source root ("" for the root itself).
	RelPath string
	// Excluded is whether the source's excludes (its own or the defaults) skip the path, as a
	// directory, or one of its ancestors.
	Excluded bool
}

// LocateInSources returns the source whose path is the longest segment-boundary prefix of local
// (a clean absolute path). defaults are matched case-insensitively, each source's own patterns
// case-sensitively, with the rules of catalog.Source.Exclude: a pattern matches an entry's base
// name or its relative path, a leading "/" matches only the relative path, a trailing "/" only
// directories.
func LocateInSources(sources []SourceRef, defaults []string, local string) (PathLocation, bool) {
	if !path.IsAbs(local) {
		return PathLocation{}, false
	}
	local = path.Clean(local)
	best := -1
	for i, s := range sources {
		p := path.Clean(s.Path)
		if !path.IsAbs(p) || !hasPathPrefix(local, p) {
			continue
		}
		if best < 0 || len(p) > len(path.Clean(sources[best].Path)) {
			best = i
		}
	}
	if best < 0 {
		return PathLocation{}, false
	}
	s := sources[best]
	root := path.Clean(s.Path)
	rel := strings.TrimPrefix(strings.TrimPrefix(local, root), "/")
	loc := PathLocation{SourceID: s.ID, RelPath: rel}
	if rel != "" {
		parts := strings.Split(rel, "/")
		for i := range parts {
			if dirExcluded(strings.Join(parts[:i+1], "/"), parts[i], defaults, s.Exclude) {
				loc.Excluded = true
				break
			}
		}
	}
	return loc, true
}

// dirExcluded applies the exclude rules to one directory.
func dirExcluded(rel, base string, defaults, own []string) bool {
	match := func(raw string, fold bool) bool {
		g := raw
		anchored := false
		g = strings.TrimRight(g, "/") // directories: every pattern applies
		if strings.HasPrefix(g, "/") {
			anchored = true
			g = strings.TrimLeft(g, "/")
		}
		r, b := rel, base
		if fold {
			g, r, b = strings.ToLower(g), strings.ToLower(r), strings.ToLower(b)
		}
		if !anchored {
			if ok, _ := path.Match(g, b); ok {
				return true
			}
		}
		ok, _ := path.Match(g, r)
		return ok
	}
	for _, d := range defaults {
		if match(d, true) {
			return true
		}
	}
	for _, o := range own {
		if match(strings.TrimSpace(o), false) {
			return true
		}
	}
	return false
}
