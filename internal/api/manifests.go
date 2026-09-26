package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/manifest"
)

// Manifests (docs/design/phase2-3.md §11, §13): a destination's manifest versions, the job that
// writes one, the verified download of a version and the on-the-spot export. Files are staged and
// hashed before the first byte is sent (S20): a failed build or a damaged version answers an
// error, never a partial body.

func (s *Server) manifestRoutes(r chi.Router) {
	r.Post("/destinations/{id}/manifest", s.startManifestExport)
	r.Get("/destinations/{id}/manifests", s.listManifests)
	r.Get("/manifests/{id}/download", s.downloadManifest)
	r.Get("/manifest/export", s.exportManifest)
}

// SHA256Header carries the hex sha256 of a manifest download.
const SHA256Header = "X-Bunkarr-SHA256"

// newManifestRunner builds the manifest_export runner (it also serves the export and the
// downloads). Every file's tier is full until Phase 3 plugs in the tier evaluator.
func (a *App) newManifestRunner(o AppOptions, configDir string) (*manifest.Runner, error) {
	return manifest.NewRunner(manifest.Options{
		DB:           o.DB,
		Catalog:      a.Catalog,
		Integrations: a.Integrations,
		Destinations: a.Destinations,
		Index:        a.Index,
		ConfigDir:    configDir,
		Log:          a.log.With("component", "manifest"),
		Location:     o.Location,
	})
}

// manifestError classifies the manifest package's errors (the others go to statusOf).
func manifestError(err error) error {
	switch {
	case errors.Is(err, manifest.ErrNotFound):
		return errorf(http.StatusNotFound, "%s", err.Error())
	case errors.Is(err, manifest.ErrDamaged):
		return errorf(http.StatusConflict, "%s", manifest.ErrDamaged.Error())
	case errors.Is(err, manifest.ErrBusy):
		return errorf(http.StatusTooManyRequests, "%s", err.Error())
	}
	return err
}

// manifestFormat reads the format query parameter: json (the default) or csv.
func manifestFormat(r *http.Request) (string, error) {
	f := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if f == "" {
		return manifest.FormatJSON, nil
	}
	if !manifest.ValidFileFormat(f) {
		return "", errorf(http.StatusBadRequest, "format must be json or csv")
	}
	return f, nil
}

func (s *Server) startManifestExport(w http.ResponseWriter, r *http.Request) {
	d, err := s.enabledDestination(r)
	if err != nil {
		s.fail(w, r, "start a manifest export", err)
		return
	}
	var body struct {
		DryRun bool `json:"dryRun"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "start a manifest export", err)
		return
	}
	job, err := s.app.Jobs.Enqueue(r.Context(), jobs.Spec{Type: jobs.TypeManifestExport, Trigger: jobs.TriggerManual, DryRun: body.DryRun,
		Params: jobs.Params{DestinationID: d.ID}})
	if err != nil {
		s.fail(w, r, "start a manifest export", err)
		return
	}
	writeAccepted(w, job.ID, job)
}

func (s *Server) listManifests(w http.ResponseWriter, r *http.Request) {
	d, err := s.destination(r)
	if err != nil {
		s.fail(w, r, "list manifests", err)
		return
	}
	list, err := s.app.Manifests.Store().List(r.Context(), d.ID)
	if err != nil {
		s.fail(w, r, "list manifests", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) downloadManifest(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "download a manifest", err)
		return
	}
	format, err := manifestFormat(r)
	if err != nil {
		s.fail(w, r, "download a manifest", err)
		return
	}
	f, err := s.app.Manifests.Download(r.Context(), id, format)
	if err != nil {
		s.fail(w, r, "download a manifest", manifestError(err))
		return
	}
	s.serveStaged(w, r, f)
}

func (s *Server) exportManifest(w http.ResponseWriter, r *http.Request) {
	format, err := manifestFormat(r)
	if err != nil {
		s.fail(w, r, "export a manifest", err)
		return
	}
	destID, err := int64Param(r.URL.Query(), "destinationId")
	if err != nil {
		s.fail(w, r, "export a manifest", err)
		return
	}
	f, err := s.app.Manifests.Export(r.Context(), destID, format)
	if err != nil {
		if errors.Is(err, manifest.ErrBusy) {
			w.Header().Set("Retry-After", "5")
		}
		s.fail(w, r, "export a manifest", manifestError(err))
		return
	}
	s.serveStaged(w, r, f)
}

// serveStaged sends a staged manifest file with its length and digest as an attachment, then
// removes it.
func (s *Server) serveStaged(w http.ResponseWriter, r *http.Request, f *manifest.StagedFile) {
	defer func() {
		if err := f.Close(); err != nil {
			s.log.Warn("Could not remove a staged manifest", "error", err)
		}
	}()
	rd, err := f.Open()
	if err != nil {
		s.fail(w, r, "send a manifest", err)
		return
	}
	defer rd.Close()
	h := w.Header()
	h.Set("Content-Type", f.ContentType)
	h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	h.Set(SHA256Header, f.SHA256)
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rd); err != nil {
		s.log.Warn("A manifest download was interrupted", "file", f.Name, "error", err)
	}
}
