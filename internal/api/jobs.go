package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/jobqueue"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// Jobs and schedules (design §7).

func (s *Server) jobRoutes(r chi.Router) {
	r.Get("/jobs", s.listJobs)
	r.Get("/jobs/{id}", s.getJob)
	r.Post("/jobs/{id}/cancel", s.cancelJob)
	r.Get("/jobs/{id}/items", s.listItems)
	r.Get("/jobs/{id}/items/summary", s.itemSummary)
	r.Get("/jobs/{id}/logs", s.jobLogs)
	r.Get("/schedules", s.listSchedules)
	r.Put("/schedules/{id}", s.updateSchedule)
	r.Post("/schedules/{id}/run", s.runSchedule)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, size, err := paging(q)
	if err != nil {
		s.fail(w, r, "list jobs", err)
		return
	}
	destID, err := int64Param(q, "destinationId")
	if err != nil {
		s.fail(w, r, "list jobs", err)
		return
	}
	integID, err := int64Param(q, "integrationId")
	if err != nil {
		s.fail(w, r, "list jobs", err)
		return
	}
	res, err := s.app.Jobs.List(r.Context(), jobqueue.JobQuery{
		State:         q.Get("state"),
		Type:          jobs.Type(q.Get("type")),
		Status:        jobs.Status(q.Get("status")),
		DestinationID: destID,
		IntegrationID: integID,
		Page:          page,
		PageSize:      size,
	})
	if err != nil {
		s.fail(w, r, "list jobs", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// job loads the job named by the {id} URL parameter (with a running job's live progress).
func (s *Server) job(r *http.Request) (jobs.Job, error) {
	id, err := pathID(r)
	if err != nil {
		return jobs.Job{}, err
	}
	return s.app.Jobs.Get(r.Context(), id)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.job(r)
	if err != nil {
		s.fail(w, r, "get job", err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// cancelJob cancels a queued job at once, or asks a running job to stop (it becomes cancelled
// when its runner returns). It answers with the job as it is now; a finished job is a 409.
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "cancel job", err)
		return
	}
	j, err := s.app.Jobs.Cancel(r.Context(), id)
	if err != nil {
		s.fail(w, r, "cancel job", err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) listItems(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "list job items", err)
		return
	}
	q := r.URL.Query()
	page, size, err := paging(q)
	if err != nil {
		s.fail(w, r, "list job items", err)
		return
	}
	res, err := s.app.Jobs.Store().ListItems(r.Context(), id, jobqueue.ItemQuery{
		Action:   jobs.ItemAction(q.Get("action")),
		Status:   jobs.ItemStatus(q.Get("status")),
		Page:     page,
		PageSize: size,
	})
	if err != nil {
		s.fail(w, r, "list job items", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) itemSummary(w http.ResponseWriter, r *http.Request) {
	j, err := s.job(r)
	if err != nil {
		s.fail(w, r, "summarize job items", err)
		return
	}
	counts, err := s.app.Jobs.Store().Counts(r.Context(), j.ID)
	if err != nil {
		s.fail(w, r, "summarize job items", err)
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

func (s *Server) jobLogs(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "read job logs", err)
		return
	}
	q := r.URL.Query()
	after, err := int64Param(q, "afterId")
	if err != nil {
		s.fail(w, r, "read job logs", err)
		return
	}
	limit, err := intParam(q, "limit", jobqueue.DefaultLogLimit, 1, jobqueue.MaxLogLimit)
	if err != nil {
		s.fail(w, r, "read job logs", err)
		return
	}
	logs, err := s.app.Jobs.Store().ListLogs(r.Context(), id, after, limit)
	if err != nil {
		s.fail(w, r, "read job logs", err)
		return
	}
	writeJSON(w, http.StatusOK, logs)
}
