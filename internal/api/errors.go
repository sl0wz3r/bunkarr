package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/notify"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// httpError is an error the handler has already classified: its status and user-facing message.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func errorf(status int, format string, args ...any) error {
	return &httpError{status: status, msg: fmt.Sprintf(format, args...)}
}

// statusOf maps an error of the Phase 1 packages to its HTTP status:
//   - validation errors of every package → 400;
//   - "not found" of every package → 404;
//   - conflicts with the current state (duplicate names, a source being scanned, a destfolder
//     that is immutable, a marker already present, a job that is no longer active) → 409;
//   - safety rule S3 refusals: a local filesystem without allowLocal → 400, a destination that is
//     not mounted (marker missing or foreign, filesystem changed) → 409;
//   - Plex answering badly or not at all → 502;
//   - everything else → 500.
func statusOf(err error) int {
	var (
		he   *httpError
		ive  integrations.ValidationError
		cve  *catalog.ValidationError
		dve  destinations.ValidationError
		jve  jobqueue.ValidationError
		nve  notify.ValidationError
		perr *plex.Error
	)
	switch {
	case errors.As(err, &he):
		return he.status
	case errors.As(err, &ive), errors.As(err, &cve), errors.As(err, &dve), errors.As(err, &jve), errors.As(err, &nve),
		errors.Is(err, destinations.ErrLocalFilesystem):
		return http.StatusBadRequest
	case errors.Is(err, integrations.ErrNotFound), errors.Is(err, catalog.ErrNotFound), errors.Is(err, destinations.ErrNotFound),
		errors.Is(err, jobqueue.ErrNotFound), errors.Is(err, notify.ErrNotFound), errors.Is(err, snapshots.ErrNotFound),
		errors.Is(err, syncer.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, catalog.ErrConflict), errors.Is(err, destinations.ErrNameTaken), errors.Is(err, destinations.ErrMarkerExists),
		errors.Is(err, destinations.ErrNotMounted), errors.Is(err, destinations.ErrMarkerMismatch), errors.Is(err, destinations.ErrFSChanged),
		errors.Is(err, jobqueue.ErrNotActive):
		return http.StatusConflict
	case errors.As(err, &perr):
		return http.StatusBadGateway
	case errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// fail writes err as a JSON error with the status statusOf chooses. The message is the error's
// own text with every registered secret value redacted; server errors are also logged.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	status := statusOf(err)
	msg := logging.RedactSecrets(err.Error())
	if status >= http.StatusInternalServerError {
		s.log.Error("Request failed", "action", what, "method", r.Method, "path", r.URL.Path, "error", msg)
	}
	writeError(w, status, msg)
}
