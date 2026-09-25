package api

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Sources, catalog and the read-only filesystem picker (design §7).

func (s *Server) sourceRoutes(r chi.Router) {
	r.Get("/sources", s.listSources)
	r.Post("/sources", s.createSource)
	r.Post("/sources/test", s.testSource)
	r.Get("/sources/{id}", s.getSource)
	r.Put("/sources/{id}", s.updateSource)
	r.Delete("/sources/{id}", s.deleteSource)
	r.Post("/sources/{id}/scan", s.scanSource)
	r.Get("/sources/{id}/files", s.sourceFiles)
	r.Get("/catalog/stats", s.catalogStats)
	r.Get("/filesystem", s.browse)
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	list, err := s.app.Catalog.List(r.Context())
	if err != nil {
		s.fail(w, r, "list sources", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "get source", err)
		return
	}
	src, err := s.app.Catalog.Get(r.Context(), id)
	if err != nil {
		s.fail(w, r, "get source", err)
		return
	}
	writeJSON(w, http.StatusOK, src)
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	var in catalog.SourceInput
	if err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, "create source", err)
		return
	}
	src, err := s.app.Catalog.Create(r.Context(), in)
	if err != nil {
		s.fail(w, r, "create source", err)
		return
	}
	s.log.Info("Source created", "id", src.ID, "name", src.Name, "path", src.Path, "destFolder", src.DestFolder)
	writeJSON(w, http.StatusCreated, src)
}

func (s *Server) updateSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "update source", err)
		return
	}
	var in catalog.SourceInput
	if err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, "update source", err)
		return
	}
	src, err := s.app.Catalog.Update(r.Context(), id, in)
	if err != nil {
		s.fail(w, r, "update source", err)
		return
	}
	s.log.Info("Source updated", "id", src.ID, "name", src.Name, "path", src.Path)
	writeJSON(w, http.StatusOK, src)
}

// deleteSource removes a source, its catalog and its scan schedules. Its backups stay at the
// destinations.
func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "delete source", err)
		return
	}
	if err := s.app.Catalog.Delete(r.Context(), id); err != nil {
		s.fail(w, r, "delete source", err)
		return
	}
	n, err := s.app.Jobs.Store().DeleteSchedulesFor(r.Context(), jobs.Params{SourceIDs: []int64{id}})
	if err != nil {
		s.fail(w, r, "delete source", errScheduleSync("the deletion", err))
		return
	}
	if n > 0 {
		s.reloadSchedules(r.Context())
	}
	s.log.Info("Source deleted", "id", id, "schedulesRemoved", n)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, "test source", err)
		return
	}
	writeJSON(w, http.StatusOK, s.app.Catalog.TestSource(r.Context(), body.Path))
}

func (s *Server) scanSource(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "scan source", err)
		return
	}
	if _, err := s.app.Catalog.Get(r.Context(), id); err != nil {
		s.fail(w, r, "scan source", err)
		return
	}
	job, err := s.app.Jobs.Enqueue(r.Context(), jobs.Spec{Type: jobs.TypeScan, Trigger: jobs.TriggerManual, Params: jobs.Params{SourceIDs: []int64{id}}})
	if err != nil {
		s.fail(w, r, "scan source", err)
		return
	}
	writeAccepted(w, job.ID, job)
}

func (s *Server) sourceFiles(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "list catalog files", err)
		return
	}
	q := r.URL.Query()
	page, size, err := paging(q)
	if err != nil {
		s.fail(w, r, "list catalog files", err)
		return
	}
	res, err := s.app.Catalog.Files(r.Context(), id, catalog.Query{Page: page, PageSize: size,
		Search: q.Get("search"), Filter: catalog.Filter(q.Get("filter"))})
	if err != nil {
		s.fail(w, r, "list catalog files", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) catalogStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.app.Catalog.Stats(r.Context())
	if err != nil {
		s.fail(w, r, "catalog stats", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// maxBrowseEntries bounds the directories one /filesystem answer lists.
const maxBrowseEntries = 10000

// dirEntry is a directory in a /filesystem listing.
type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// dirListing is GET /filesystem's answer.
type dirListing struct {
	Path string `json:"path"`
	// Parent is "" at "/".
	Parent      string     `json:"parent"`
	Directories []dirEntry `json:"directories"`
	// Truncated is true when the directory has more than maxBrowseEntries subdirectories.
	Truncated bool `json:"truncated"`
}

// browse lists the subdirectories of an absolute path ("/" when path is empty) for the path
// picker. It only reads directory listings; files are never listed. Symlinks that point to
// directories are listed (under their own name); the picked path is resolved when saved.
func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	if p == "" {
		p = "/"
	}
	if strings.ContainsRune(p, 0) || !filepath.IsAbs(p) {
		s.fail(w, r, "browse", errorf(http.StatusBadRequest, "path must be absolute (the path inside the Bunkarr container)"))
		return
	}
	p = filepath.Clean(p)
	entries, err := os.ReadDir(p)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			err = errorf(http.StatusNotFound, "%s does not exist", p)
		case errors.Is(err, fs.ErrPermission):
			err = errorf(http.StatusForbidden, "%s is not readable by Bunkarr's user (permission denied)", p)
		default:
			var pe *fs.PathError
			if fi, serr := os.Stat(p); serr == nil && !fi.IsDir() {
				err = errorf(http.StatusBadRequest, "%s is not a directory", p)
			} else if errors.As(err, &pe) {
				err = errorf(http.StatusBadRequest, "cannot list %s: %v", p, pe.Err)
			}
		}
		s.fail(w, r, "browse", err)
		return
	}
	out := dirListing{Path: p, Directories: []dirEntry{}}
	if p != "/" {
		out.Parent = filepath.Dir(p)
	}
	for _, e := range entries {
		isDir := e.IsDir()
		if !isDir && e.Type()&fs.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(p, e.Name())); err == nil && fi.IsDir() {
				isDir = true
			}
		}
		if !isDir {
			continue
		}
		if len(out.Directories) == maxBrowseEntries {
			out.Truncated = true
			break
		}
		out.Directories = append(out.Directories, dirEntry{Name: e.Name(), Path: filepath.Join(p, e.Name())})
	}
	sort.Slice(out.Directories, func(i, j int) bool {
		return strings.ToLower(out.Directories[i].Name) < strings.ToLower(out.Directories[j].Name)
	})
	writeJSON(w, http.StatusOK, out)
}
