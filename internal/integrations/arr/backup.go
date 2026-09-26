package arr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
)

// Backup types (the directories of the *arr's Backups folder).
const (
	BackupManual    = "manual"
	BackupScheduled = "scheduled"
	BackupUpdate    = "update"
)

// backupName is what a backup file name must look like (phase2-3.md §10 step 4, S18).
var backupName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}\.zip$`)

// ValidateBackup checks that b's type is manual, scheduled or update and that its name is a
// plain zip file name (^[A-Za-z0-9._-]{1,200}\.zip$, so "." and ".." cannot occur alone). Only a
// valid entry may be located, in the backup folder or over HTTP (S18).
func ValidateBackup(b Backup) error {
	switch b.Type {
	case BackupManual, BackupScheduled, BackupUpdate:
	default:
		return fmt.Errorf("%w: type %q", ErrInvalidBackup, truncate(b.Type, 32))
	}
	if !backupName.MatchString(b.Name) {
		return fmt.Errorf("%w: name %q", ErrInvalidBackup, truncate(b.Name, 64))
	}
	return nil
}

// BackupPath is the location of a valid backup relative to the *arr's Backups folder and to its
// base URL: "<type>/<name>". It is built from the validated type and name only (S18).
func BackupPath(b Backup) (string, error) {
	if err := ValidateBackup(b); err != nil {
		return "", err
	}
	return b.Type + "/" + b.Name, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// backupRequest builds the non-API GET <base>/backup/<type>/<name>.
func (c *Client) backupRequest(b Backup) (request, error) {
	rel, err := BackupPath(b)
	if err != nil {
		return request{}, &Error{Method: http.MethodGet, Path: "/backup/…", Err: err}
	}
	p, err := url.JoinPath("/", "backup", b.Type, b.Name)
	if err != nil || p != "/backup/"+rel {
		return request{}, &Error{Method: http.MethodGet, Path: "/backup/…", Err: ErrInvalidBackup}
	}
	return request{method: http.MethodGet, path: p, timeout: c.dlTmo, limit: MaxBackupBytes,
		accept: "application/zip, application/octet-stream"}, nil
}

// backupStatus maps the status of a backup download: a redirect (to /login) or 401 means the
// *arr requires a login for Bunkarr's address (spike: Forms authentication answers 302).
func backupStatus(code int) error {
	switch {
	case code == http.StatusUnauthorized, code >= 300 && code < 400:
		return ErrLoginRequired
	case code == http.StatusNotFound:
		return ErrNotFound
	}
	return fmt.Errorf("unexpected status %d %s", code, http.StatusText(code))
}

// zipTypes are the content types a backup download may have. The *arrs send
// application/x-zip-compressed (Lidarr 3.1.0, recorded for this client).
var zipTypes = map[string]bool{
	"application/zip":              true,
	"application/x-zip-compressed": true,
	"application/x-zip":            true,
	"application/octet-stream":     true,
}

// DownloadBackup streams the backup b from GET <base>/backup/<type>/<name> into w (the non-API
// route, with the key header and no redirects) and returns the number of bytes written. It needs
// a 200 with a zip content type; a redirect or 401 returns an *Error wrapping ErrLoginRequired
// (the *arr requires a login, design D3). More than MaxBackupBytes, or more than b.Size when that
// is known, fails with ErrTooLarge. b must be valid (ValidateBackup).
func (c *Client) DownloadBackup(ctx context.Context, b Backup, w io.Writer) (int64, error) {
	r, err := c.backupRequest(b)
	if err != nil {
		return 0, err
	}
	if b.Size > 0 && b.Size < r.limit {
		r.limit = b.Size
	}
	resp, body, release, err := c.send(ctx, r)
	if err != nil {
		return 0, err
	}
	defer release()
	if resp.StatusCode != http.StatusOK {
		return 0, c.fail(r, resp.StatusCode, backupStatus(resp.StatusCode))
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || !zipTypes[mt] {
		return 0, c.fail(r, resp.StatusCode, fmt.Errorf("%w: the answer is not a zip file", ErrLoginRequired))
	}
	n, err := io.Copy(w, body)
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return n, c.fail(r, resp.StatusCode, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, r.limit))
		}
		return n, c.fail(r, resp.StatusCode, c.transportCause(ctx, err, r.timeout))
	}
	return n, nil
}

// BackupAccess is how a backup download over HTTP answers.
type BackupAccess string

// Backup download outcomes (POST /integrations/test, backup.http).
const (
	BackupAccessOK            BackupAccess = "ok"
	BackupAccessLoginRequired BackupAccess = "login-required"
)

// ProbeBackup checks whether the backup b can be downloaded over HTTP with the API key: it sends
// the same GET as DownloadBackup with "Range: bytes=0-0" and reads at most one byte (the *arrs
// ignore the range and answer 200 with the whole file, which is then not read). A 200 or 206 with
// a zip content type is BackupAccessOK; a redirect, a 401 or an HTML page is
// BackupAccessLoginRequired; anything else is an error.
func (c *Client) ProbeBackup(ctx context.Context, b Backup) (BackupAccess, error) {
	r, err := c.backupRequest(b)
	if err != nil {
		return "", err
	}
	r.rangeFirst, r.timeout = true, c.tmo
	resp, body, release, err := c.send(ctx, r)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = resp.Body.Close() // not drained: the rest of the zip is never read
		release()
	}()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		cause := backupStatus(resp.StatusCode)
		if errors.Is(cause, ErrLoginRequired) {
			return BackupAccessLoginRequired, nil
		}
		return "", c.fail(r, resp.StatusCode, cause)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || !zipTypes[mt] {
		return BackupAccessLoginRequired, nil
	}
	var one [1]byte
	_, _ = io.ReadFull(body, one[:])
	return BackupAccessOK, nil
}
